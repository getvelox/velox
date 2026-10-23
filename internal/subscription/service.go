package subscription

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

var slugPattern = regexp.MustCompile(`^[a-zA-Z0-9_\-]+$`)

// SettingsReader is the narrow surface this service uses to resolve
// the tenant's timezone for period-boundary snapping. Optional —
// nil-safe, falls back to UTC. Avoids importing internal/tenant
// directly so the dependency graph stays acyclic.
type SettingsReader interface {
	Get(ctx context.Context, tenantID string) (domain.TenantSettings, error)
}

// CustomerReader fetches a customer by id on a tenant- and mode-scoped
// transaction — narrow shape used by Create to confirm the customer is the
// caller's and to read customer.test_clock_id for inheritance (ADR-027).
// Implemented by *customer.PostgresStore.
type CustomerReader interface {
	Get(ctx context.Context, tenantID, customerID string) (domain.Customer, error)
}

// Biller is the narrow shape used after Create + Cancel to handle
// the in_advance bill_timing artifacts (ADR-031): day-1 invoice on
// create, cancel proration credit on mid-period cancel. Optional —
// nil-safe; without it, in_advance subs silently behave like
// in_arrears (no day-1 invoice, no cancel proration). Implemented
// by *billing.Engine.
type Biller interface {
	BillOnCreate(ctx context.Context, sub domain.Subscription) (domain.Invoice, error)
	// BillFinalOnImmediateCancel emits the final partial-period invoice
	// for a mid-period immediate cancel (in_arrears prorated base +
	// usage). It refuses (InvalidState, code unbilled_period) when
	// canceled_at is at/after current_period_end: that period ended
	// unbilled, and no billing run bills a canceled subscription.
	BillFinalOnImmediateCancel(ctx context.Context, sub domain.Subscription) (domain.Invoice, error)
	// BillFinalOnImmediateCancelTx is the in-tx variant: inserts the
	// final-on-cancel invoice on the caller's coordinator tx (ADR-056); the
	// external finalize runs post-commit via FinalizeOnCreateInvoice. Empty
	// invoice (nil error) for the no-op cases. afterCommit is never nil; the
	// caller runs it only once the tx commits (it counts usage the invoice
	// left unpriced, which a rolled-back, retried cancel must not count
	// twice).
	BillFinalOnImmediateCancelTx(ctx context.Context, tx *sql.Tx, sub domain.Subscription) (inv domain.Invoice, afterCommit func(), err error)
	// BillOnCancelDraftsTx writes the cancel-proration relief on the caller's
	// cancel tx (ADR-057, ADR-118): credit-note drafts (issue_pending) for
	// paid and partly-used unpaid funding invoices, and an in-place void of a
	// fully-unused unpaid one. Any error rolls the cancel back — never a
	// canceled subscription whose relief was lost. The caller runs
	// FinishRelief after commit.
	BillOnCancelDraftsTx(ctx context.Context, tx *sql.Tx, sub domain.Subscription) (domain.Relief, error)
	// FinishRelief runs a committed relief's post-commit legs (issue the
	// drafts; the voids' tax reversal and webhook). Best-effort: each leg is
	// durable and recovered by its own sweep.
	FinishRelief(ctx context.Context, sub domain.Subscription, r domain.Relief)
	// ThresholdBilledThroughTx reads, on the caller's tx, the instant a
	// mid-cycle threshold invoice already billed sub's CURRENT period
	// through (nil = no threshold fire this period). The atomic plan swap
	// calls it under the subscription row lock (ADR-115): a reset=false fire
	// that committed after the swap read the sub passes the period CAS but
	// may have billed usage past the swap's effective instant; truncating the
	// period below that window would re-bill it at the next close, so the
	// swap refuses instead.
	ThresholdBilledThroughTx(ctx context.Context, tx *sql.Tx, sub domain.Subscription) (*time.Time, error)
	// BillOnPlanSwapDraftsTx is the plan-swap twin of BillOnCancelDraftsTx:
	// the refund for the OLD period's unused prepayment, written on the swap
	// tx; any error rolls the swap back. sub is the PRE-swap snapshot. The
	// caller runs FinishRelief after commit.
	BillOnPlanSwapDraftsTx(ctx context.Context, tx *sql.Tx, sub domain.Subscription, at time.Time) (domain.Relief, error)
	// BillOnCreateTx is the in-tx variant of BillOnCreate: it inserts the day-1
	// in_advance invoice on the caller's tx (no finalize). ok=false means there
	// was nothing to bill (no in_advance items / zero subtotal) or an idempotent
	// skip. Used by create, activate and the trial flip. startAt is the
	// instant the billed period began (the trial end on a trial flip, the
	// request's now otherwise); the pause is read as it stood then.
	BillOnCreateTx(ctx context.Context, tx *sql.Tx, sub domain.Subscription, startAt time.Time) (domain.Invoice, bool, error)
	// BillOnPlanSwapTx inserts, on the caller's tx, the invoice of an
	// immediate in_advance cross-interval swap at `at`: after's new-period
	// base plus the usage of before's period over [its start, at), which the
	// swap closes (ADR-056, ADR-031 renewal shape). before is the pre-swap
	// snapshot the swap proved under the row lock. ok=false means nothing to
	// bill. afterCommit is never nil; the caller runs it only once the tx
	// commits (it counts usage the invoice left unpriced).
	BillOnPlanSwapTx(ctx context.Context, tx *sql.Tx, before, after domain.Subscription, at time.Time) (inv domain.Invoice, ok bool, afterCommit func(), err error)
	// FinalizeOnCreateInvoice runs the post-commit external steps (audit, tax
	// commit, auto-charge) for an invoice inserted via BillOnCreateTx or
	// BillOnPlanSwapTx. Called AFTER the tx commits — never inside it.
	FinalizeOnCreateInvoice(ctx context.Context, sub domain.Subscription, inv domain.Invoice)
}

// DueBiller bills one subscription now, through the same attempt the billing
// run makes (period CAS, failure record and all). It is the bill half of the
// operator's retry-billing action (ADR-119) and of the close every
// subscription write runs first (closeEndedPeriods). Its own one-method
// interface, not a method on Biller, so the fakes of that interface do not
// all grow. Implemented by *billing.Engine.
//
// grace is the invoice grace the attempt honours (ADR-120): a due instant
// counts only once grace has passed after it. 0 closes every ended period
// now, as the operator's writes do; dunning's final pause and cancel pass
// INVOICE_GRACE_PERIOD, so a catch-up closes the periods past their grace and
// leaves the newest to its grace.
type DueBiller interface {
	BillDue(ctx context.Context, tenantID, subscriptionID string, grace time.Duration) (invoicesGenerated int, err error)
}

// PlanReader (defined in handler.go) is also used at sub-lifecycle
// entry points (Create / Activate / EndTrial / ExtendTrial) to read
// BillingInterval for yearly-aware period anchoring (Bug #10).
// Optional: nil-safe — the period helpers default to monthly math
// when the reader isn't wired.

type Service struct {
	store     Store
	clock     clock.Clock
	settings  SettingsReader
	customers CustomerReader
	profiles  ProfileCurrencyReader
	plans     PlanReader
	biller    Biller
	dueBiller DueBiller
	// invoiceGrace is INVOICE_GRACE_PERIOD (ADR-120). Only dunning's final
	// pause and cancel wait for it; see SetInvoiceGrace.
	invoiceGrace time.Duration
	resolver     clock.Resolver
	events       domain.EventDispatcher
	audit        AuditLogger
}

// AuditLogger is the narrow audit-write interface the service uses to
// record state-changing operations that are reachable from multiple
// callers (operator-driven via Handler + system-driven via the dunning
// adapter, etc). Pre-fix the audit-write lived only in Handler — which
// meant dunning's auto-pause path bypassed it entirely, leaving the
// Activity timeline blank for any sub paused by exhausted dunning.
// Matches *audit.Logger so SetAuditLogger(auditLogger) wires it.
type AuditLogger interface {
	Log(ctx context.Context, tenantID, action, resourceType, resourceID, resourceLabel string, metadata map[string]any) error
	// LogInTx is the ADR-090 in-tx emission: the audit row rides the
	// caller's transaction (Cancel uses it inside CancelAtomicWithBill so
	// the cancel flip, the final invoice, and the audit row share fate).
	LogInTx(ctx context.Context, tx *sql.Tx, e audit.Entry) error
}

// SetAuditLogger wires the audit logger used by state-changing service
// methods (PauseCollection / ResumeCollection today). Optional — without
// it those methods skip the audit write (test-friendly).
func (s *Service) SetAuditLogger(l AuditLogger) {
	s.audit = l
}

func NewService(store Store, clk clock.Clock) *Service {
	if clk == nil {
		clk = clock.Real()
	}
	return &Service{store: store, clock: clk}
}

// SetSettingsReader wires the tenant settings reader. Kept as a setter
// rather than a constructor arg because router.go builds the settings
// store after the subscription service today; setter avoids a forced
// re-order. Calls before the setter is wired fall back to UTC for
// period snapping (tested via the nil branch of tenantLocation).
func (s *Service) SetSettingsReader(r SettingsReader) {
	s.settings = r
}

// SetCustomerReader wires the customer fetcher used at sub-create time
// to refuse a customer outside the caller's tenant and mode and to
// inherit test_clock_id from the owning customer (ADR-027). When
// unwired (narrow unit tests), neither happens: the new sub has no
// clock unless the caller stamps one directly. Production wires the
// real customer store via router.go.
func (s *Service) SetCustomerReader(r CustomerReader) {
	s.customers = r
}

// ProfileCurrencyReader is the narrow seam for the ADR-100 currency pin:
// the customer's billing-profile currency is the highest-precedence invoice
// label, so a new plan must match it. Nil-safe like every sibling guard
// seam; production wires the customer store via router.go.
type ProfileCurrencyReader interface {
	GetBillingProfile(ctx context.Context, tenantID, customerID string) (domain.CustomerBillingProfile, error)
}

func (s *Service) SetProfileReader(r ProfileCurrencyReader) {
	s.profiles = r
}

// SetPlanReader wires the plan fetcher used to read BillingInterval
// at sub-lifecycle entry points (Create / Activate / EndTrial /
// ExtendTrial) for yearly-aware period anchoring. Optional —
// nil-safe; without it, the period helpers default to monthly math
// (pre-Bug-#10 behavior). Production wires the pricing store via
// router.go.
func (s *Service) SetPlanReader(r PlanReader) {
	s.plans = r
}

// SetEventDispatcher wires the outbound-webhook dispatcher used by
// the trial-expiry scan paths (ProcessExpiredTrialsForClock +
// ProcessExpiredTrials) to fire `subscription.trial_ended` events
// when status flips at trial_end_at. Engine auto-flip path emits
// the same event from billing/engine.go; this setter brings the
// catchup-orchestrator and wall-clock-cron paths into parity so
// webhook consumers see one event per trial transition regardless
// of which path activated the sub.
func (s *Service) SetEventDispatcher(d domain.EventDispatcher) {
	s.events = d
}

// dispatchEvent enqueues an outbound webhook event and LOGS on failure rather
// than silently dropping it. Once the enqueue commits the dispatcher delivers
// durably (ADR-040 outbox); a non-nil error means the enqueue INSERT failed and
// the event is lost — make that visible (no-silent-fallbacks) instead of the
// prior `_ =` swallow. In-tx atomicity with the state change (DispatchTx) is the
// deferred follow-up; this only closes the silent-error vector.
func (s *Service) dispatchEvent(ctx context.Context, tenantID, eventType string, payload map[string]any) {
	if s.events == nil {
		return
	}
	if err := s.events.Dispatch(ctx, tenantID, eventType, payload); err != nil {
		slog.ErrorContext(ctx, "failed to enqueue webhook event",
			"event_type", eventType, "tenant_id", tenantID, "error", err)
	}
}

// SetBiller wires the billing engine for the in_advance first-invoice
// path (ADR-031). Optional — without it, in_advance plans silently
// behave like in_arrears until the next cycle close. Production wires
// *billing.Engine via router.go; unit tests can leave it unwired
// because most don't exercise the day-1 invoice path.
func (s *Service) SetBiller(b Biller) {
	s.biller = b
}

// SetDueBiller wires the engine for RetryBilling and for Cancel's bill of an
// ended period. Without it a retry still clears the failure record, and the
// next billing run bills the subscription; a cancel after the period end is
// refused until that run has billed the period.
func (s *Service) SetDueBiller(b DueBiller) {
	s.dueBiller = b
}

// SetInvoiceGrace sets INVOICE_GRACE_PERIOD (ADR-120), the same value the
// billing engine's runs wait for (billing.Engine.SetInvoiceGrace). Dunning's
// final pause and cancel (PauseCollectionForDunning, CancelForDunning) do not
// close a period that ended less than this long ago: dunning is not the
// operator acting now, so it waits for the grace like the billing run, and
// usage that arrives inside the grace is on the period's invoice. Every other
// subscription write closes the ended period at once. Unset (0) waits for
// nothing.
func (s *Service) SetInvoiceGrace(d time.Duration) {
	s.invoiceGrace = d
}

// InsideInvoiceGraceError is the answer of a dunning final action (pause or
// cancel) that would close a period still inside its invoice grace. Nothing
// was billed or changed. Until is when the grace ends (the due instant plus
// INVOICE_GRACE_PERIOD, truncated to the microsecond the database keeps): the
// action can run then.
type InsideInvoiceGraceError struct {
	Until time.Time
}

func (e *InsideInvoiceGraceError) Error() string {
	return "the subscription's ended period is inside its invoice grace until " + e.Until.UTC().Format(time.RFC3339Nano)
}

// SetResolver wires the unified clock.Resolver used to bind
// effective-now at operator entry points on clock-pinned entities.
// Once bound on ctx via clock.BindEffectiveNow, every downstream
// s.clock.Now(ctx) (including in the postgres store) returns
// frozen_time. Optional: nil leaves binding off and every callsite
// reads wall-clock — the test-friendly default.
//
// Replaces the per-service ClockResolver pattern shipped during the
// post-ADR-029 patches; now matches Stripe's model — entity pin is
// resolved once at the boundary, simulated time inherits down.
func (s *Service) SetResolver(r clock.Resolver) {
	s.resolver = r
}

// bindForCustomer binds effective-now from a customer pin. Used by
// Create, where the sub doesn't exist yet but the customer does.
// Failing on a dangling pin is worse than stamping wall-clock; on
// resolver error, returns ctx unchanged (downstream reads wall-clock).
func (s *Service) bindForCustomer(ctx context.Context, tenantID, customerID string) context.Context {
	bound, ok := clock.BindEffectiveNow(ctx, s.resolver, clock.Pin{TenantID: tenantID, CustomerID: customerID})
	if !ok && s.resolver != nil && customerID != "" {
		// Resolver wired but binding skipped — likely an error during
		// resolution. Log; don't fail the operator's action.
		slog.Warn("subscription: customer pin binding skipped, downstream uses wall-clock",
			"tenant_id", tenantID, "customer_id", customerID)
	}
	return bound
}

// bindForSub binds effective-now from a sub pin. Used by every
// per-sub mutation entry point (Activate, ChangeItem, ScheduleCancel,
// PauseCollection, EndTrial, ExtendTrial, Cancel).
func (s *Service) bindForSub(ctx context.Context, tenantID, subscriptionID string) context.Context {
	bound, ok := clock.BindEffectiveNow(ctx, s.resolver, clock.Pin{TenantID: tenantID, SubscriptionID: subscriptionID})
	if !ok && s.resolver != nil && subscriptionID != "" {
		slog.Warn("subscription: sub pin binding skipped, downstream uses wall-clock",
			"tenant_id", tenantID, "subscription_id", subscriptionID)
	}
	return bound
}

// tenantLocation resolves the tenant's billing timezone (ADR-077). The zone
// sets the stored period boundaries and anchor day every later close bills
// from, so a settings read failure or an unloadable zone is an error, never
// UTC: a guessed zone would store a boundary hours off the tenant's civil day
// (P24, same rule as billing.Engine.tenantLocation). UTC only when the setting
// is unset.
func (s *Service) tenantLocation(ctx context.Context, tenantID string) (*time.Location, error) {
	if s.settings == nil {
		return time.UTC, nil // test seam; production always wires settings
	}
	ts, err := s.settings.Get(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("read tenant timezone: %w", err)
	}
	if ts.Timezone == "" {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(ts.Timezone)
	if err != nil {
		return nil, fmt.Errorf("load tenant timezone %q: %w", ts.Timezone, err)
	}
	return loc, nil
}

// beginningOfDayIn is a package-local alias for domain.BeginningOfDayIn
// so existing callers in this file don't need updates. Calendar-month
// snapping flows through domain.NextBillingPeriodEnd (called via
// firstPeriodForActivate / firstPeriodAfterTrial), so no local
// beginningOfMonthIn helper is needed.
func beginningOfDayIn(t time.Time, loc *time.Location) time.Time {
	return domain.BeginningOfDayIn(t, loc)
}

// firstPeriodAfterTrial computes (current_period_start, current_period_end)
// for the first post-trial billing period anchored on a trial_end_at instant.
//
// Yearly billing: always anniversary semantics (period = trial_end →
// trial_end + 1 year). billing_time is effectively ignored for yearly
// plans — Stripe ships no "calendar yearly" either, and there's no
// industry pattern for "stub to next Jan 1 then full year" billing.
//
// Monthly + calendar: start = trial_end (day-snapped), end = first day
// of the next calendar month. Produces a stub period (trial_end →
// next-month-start) that bills the partial month at the trial-end
// cycle close — Stripe / Lago parity. If trial_end IS already a
// calendar month boundary the stub collapses and the first period is
// a clean full cycle from that boundary.
//
// Monthly + anniversary: start = trial_end, end = trial_end + 1 month.
// No stub — cycle anchors on the trial-end instant.
//
// Subsequent cycles roll forward via billOnePeriod's per-cycle
// advanceBillingPeriod call (which is already interval-aware).
func firstPeriodAfterTrial(trialEnd time.Time, billingTime domain.SubscriptionBillingTime, interval domain.BillingInterval, loc *time.Location) (time.Time, time.Time, int) {
	ps := beginningOfDayIn(trialEnd, loc)
	anchorDay := domain.AnchorDayFor(ps, billingTime, interval, loc)
	pe := domain.NextBillingPeriodEnd(ps, billingTime, interval, loc, anchorDay)
	// Edge: trial_end fell exactly on a calendar boundary — the stub
	// computation could collapse (ps == pe). Promote to a clean full
	// cycle from that boundary so the engine doesn't see a zero-length
	// period.
	if !ps.Before(pe) {
		pe = domain.NextBillingPeriodEnd(pe, billingTime, interval, loc, anchorDay)
	}
	return ps, pe, anchorDay
}

// firstPeriodForActivate computes the first billing period when a sub is
// activated without a trial — either by a start-now Create, or by the
// operator's Activate on a draft sub.
//
// Yearly billing: always anniversary semantics — period begins at `at`
// (day-snapped) and ends at `at + 1 year`. Stripe doesn't ship calendar
// yearly either; a "stub to next Jan 1" model has no industry analog.
//
// Monthly + calendar: stub from `at` to the next calendar-month
// boundary (proration handled by billOnePeriod).
//
// Monthly + anniversary: full month from `at`.
func firstPeriodForActivate(at time.Time, billingTime domain.SubscriptionBillingTime, interval domain.BillingInterval, loc *time.Location) (time.Time, time.Time, int) {
	ps := beginningOfDayIn(at, loc)
	anchorDay := domain.AnchorDayFor(ps, billingTime, interval, loc)
	pe := domain.NextBillingPeriodEnd(ps, billingTime, interval, loc, anchorDay)
	return ps, pe, anchorDay
}

// rejectMixedItemIntervals validates that every item's plan shares
// the same BillingInterval AND the same BaseBillTiming. Returns
// errs.Invalid("items", ...) on mismatch.
//
//   - Interval mix: Stripe / Lago / Chargebee all reject mixed
//     intervals on a single sub because the period anchor is per-sub
//     and a monthly + yearly mix has no coherent cycle.
//   - Bill-timing mix: in_arrears and in_advance carry different
//     invoice-shape semantics (close elapsed vs open upcoming) and
//     mixing them on the same sub would emit inconsistent lines on the
//     same invoice. Velox's hybrid invoice shape assumes a uniform
//     bill_timing across items.
//
// Empty BillingInterval defaults to monthly; empty BaseBillTiming
// defaults to in_arrears — matching the engine's lenient defaults so
// pre-ADR-031 plans validate cleanly.
//
// Plan-fetch errors (RLS gap / deleted plan) surface as
// errs.Invalid so the operator gets a clean 400 instead of a 500.
func (s *Service) rejectMixedItemIntervals(ctx context.Context, tenantID string, items []domain.SubscriptionItem) error {
	if len(items) < 2 {
		return nil
	}
	first, err := s.plans.GetPlan(ctx, tenantID, items[0].PlanID)
	if err != nil {
		return errs.Invalid("items", fmt.Sprintf("plan %q not found", items[0].PlanID))
	}
	firstInterval := first.BillingInterval
	if firstInterval == "" {
		firstInterval = domain.BillingMonthly
	}
	firstTiming := first.BaseBillTiming
	if firstTiming == "" {
		firstTiming = domain.BillInArrears
	}
	for _, it := range items[1:] {
		plan, err := s.plans.GetPlan(ctx, tenantID, it.PlanID)
		if err != nil {
			return errs.Invalid("items", fmt.Sprintf("plan %q not found", it.PlanID))
		}
		otherInterval := plan.BillingInterval
		if otherInterval == "" {
			otherInterval = domain.BillingMonthly
		}
		if otherInterval != firstInterval {
			return errs.Invalid("items", fmt.Sprintf(
				"all items must share the same billing interval (plan %q is %s, plan %q is %s)",
				items[0].PlanID, firstInterval, it.PlanID, otherInterval))
		}
		otherTiming := plan.BaseBillTiming
		if otherTiming == "" {
			otherTiming = domain.BillInArrears
		}
		if otherTiming != firstTiming {
			return errs.Invalid("items", fmt.Sprintf(
				"all items must share the same bill_timing (plan %q is %s, plan %q is %s)",
				items[0].PlanID, firstTiming, it.PlanID, otherTiming))
		}
	}
	return nil
}

// rejectArchivedPlans refuses plans that are archived, for the writes that
// put new business on a plan: Create and both add-item paths (ADR-067). An
// archived plan keeps billing the items already on it (archive is not
// cancel); it only takes no new ones. The two plan-swap paths keep their own
// check on the plan they have already read. Callers check s.plans != nil.
func (s *Service) rejectArchivedPlans(ctx context.Context, tenantID string, planIDs []string) error {
	for _, pid := range planIDs {
		plan, err := s.plans.GetPlan(ctx, tenantID, pid)
		if err != nil {
			return errs.Invalid("items", fmt.Sprintf("plan %q not found", pid))
		}
		if plan.Status == domain.PlanArchived {
			return errs.InvalidState(fmt.Sprintf("plan %q is archived — unarchive it or pick an active plan", pid))
		}
	}
	return nil
}

// guardSwapCurrency assembles the currency-pin inputs for a plan swap: the
// incoming plan, the sub's OTHER items' plans, and the outgoing plan of the
// swapped item (raw-cents proration makes a cross-currency swap wrong even
// on a single-item sub).
func (s *Service) guardSwapCurrency(ctx context.Context, tenantID string, sub domain.Subscription, subscriptionID, itemID, newPlanID string) error {
	pin := []string{newPlanID}
	outgoing := ""
	for _, it := range sub.Items {
		if it.ID == itemID {
			outgoing = it.PlanID
			continue
		}
		pin = append(pin, it.PlanID)
	}
	return s.rejectCurrencyMismatch(ctx, tenantID, sub.CustomerID, subscriptionID, pin, outgoing)
}

// rejectCurrencyMismatch is the ADR-100 customer currency pin, enforced at
// every write that binds a plan to a subscription (create, add-item, swap,
// activate). The invoice engine attaches rule-computed cents to ONE currency
// label without conversion, so every currency source in a customer's billing
// must agree: the candidate plans among themselves, the outgoing plan on a
// swap (proration subtracts old cents from new cents raw), the customer's
// billing-profile currency (highest-precedence invoice label, so a mismatch
// relabels plan prices), and the customer's other non-terminal subs (a
// customer is billed in one currency; manual invoices take it too, and a
// credit pays only invoices in its own currency, ADR-129). Skipped when
// PlanReader isn't wired (narrow unit tests), mirroring every sibling guard.
func (s *Service) rejectCurrencyMismatch(ctx context.Context, tenantID, customerID, excludeSubID string, planIDs []string, outgoingPlanID string) error {
	if s.plans == nil {
		return nil
	}
	target, describe := "", ""
	if outgoingPlanID != "" {
		// A read error must not drop the outgoing currency from the pin:
		// proration subtracts old cents from new cents raw.
		op, err := s.plans.GetPlan(ctx, tenantID, outgoingPlanID)
		if err != nil {
			return fmt.Errorf("currency pin: outgoing plan %q: %w", outgoingPlanID, err)
		}
		target = strings.ToUpper(strings.TrimSpace(op.Currency))
		describe = fmt.Sprintf("the item's current plan %q (%s)", op.Code, target)
	}
	for _, pid := range planIDs {
		plan, err := s.plans.GetPlan(ctx, tenantID, pid)
		if err != nil {
			return errs.Invalid("items", fmt.Sprintf("plan %q not found", pid))
		}
		cur := strings.ToUpper(strings.TrimSpace(plan.Currency))
		if cur == "" {
			continue
		}
		if target == "" {
			target, describe = cur, fmt.Sprintf("plan %q (%s)", plan.Code, cur)
			continue
		}
		if cur != target {
			return errs.InvalidState(fmt.Sprintf(
				"plan %q is in %s but %s is in %s — a subscription bills in one currency; pick same-currency plans or use a separate customer",
				plan.Code, cur, describe, target))
		}
	}
	if target == "" {
		return nil
	}
	if s.profiles != nil {
		bp, err := s.profiles.GetBillingProfile(ctx, tenantID, customerID)
		switch {
		case errors.Is(err, errs.ErrNotFound):
			// No billing profile: no profile currency to agree with.
		case err != nil:
			return fmt.Errorf("currency pin: billing profile: %w", err)
		default:
			pc := strings.ToUpper(strings.TrimSpace(bp.Currency))
			if pc != "" && pc != target {
				return errs.InvalidState(fmt.Sprintf(
					"the customer's billing profile is set to %s but this plan bills in %s — invoices would relabel amounts without conversion; change the profile currency or pick a %s plan",
					pc, target, pc))
			}
		}
	}
	others, err := s.store.CustomerSubPlanCurrencies(ctx, tenantID, customerID, excludeSubID)
	if err != nil {
		return fmt.Errorf("currency pin: customer subs: %w", err)
	}
	for _, o := range others {
		oc := strings.ToUpper(strings.TrimSpace(o.Currency))
		if oc != "" && oc != target {
			return errs.InvalidState(fmt.Sprintf(
				"this customer already has subscription %q billing in %s — a customer's subscriptions share one currency; use a %s plan or a separate customer",
				o.SubCode, oc, oc))
		}
	}
	return nil
}

// rejectMeterOverlap is the double-billing guard: no two live (draft /
// trialing / active) subscriptions of a customer may bill the same meter,
// and no two items on one subscription may either. Usage aggregation is
// customer+meter scoped with NO subscription filter (engine.go
// AggregateForBillingPeriodByAgg), so overlapping meter sets would invoice
// the same usage once per subscription — silent double-billing. Migration
// 0026 dropped the one-live-sub-per-customer index deferring to
// "application policy"; this is that policy, scoped to the actual money
// invariant (disjoint-meter subs stay allowed).
//
// planIDs is the incoming plan set (all items on Create; the added /
// swapped-to plan on item mutations). currentSubID is the sub being
// mutated ("" on Create — it doesn't exist yet) and only steers the error
// message: a same-sub conflict means "this sub already has an item on that
// meter". excludeItemID skips the item being plan-swapped so its outgoing
// plan's meters don't self-conflict ("" for creates and adds). Skipped
// when PlanReader isn't wired (narrow unit tests) — mirrors every sibling
// guard.
func (s *Service) rejectMeterOverlap(ctx context.Context, tenantID, customerID, currentSubID, excludeItemID string, planIDs []string) error {
	if s.plans == nil {
		return nil
	}
	meterPlan := make(map[string]string) // meter id → first candidate plan billing it
	meters := make([]string, 0, len(planIDs))
	for _, pid := range planIDs {
		plan, err := s.plans.GetPlan(ctx, tenantID, pid)
		if err != nil {
			return errs.Invalid("items", fmt.Sprintf("plan %q not found", pid))
		}
		for _, m := range plan.MeterIDs {
			if prev, dup := meterPlan[m]; dup {
				if prev == pid {
					continue // same plan listing a meter twice — harmless
				}
				return errs.Invalid("items", fmt.Sprintf(
					"plans %q and %q both bill meter %s — usage would be invoiced twice; keep one of them", prev, pid, m))
			}
			meterPlan[m] = pid
			meters = append(meters, m)
		}
	}
	if len(meters) == 0 {
		return nil
	}
	conflicts, err := s.store.FindMeterConflicts(ctx, tenantID, customerID, excludeItemID, meters)
	if err != nil {
		return err
	}
	if len(conflicts) == 0 {
		return nil
	}
	c := conflicts[0]
	if c.SubscriptionID == currentSubID {
		return errs.InvalidState(fmt.Sprintf(
			"this subscription already has an item billing meter %s — usage would be invoiced twice; remove or swap the existing item instead",
			c.MeterID))
	}
	return errs.InvalidState(fmt.Sprintf(
		"subscription %q already bills meter %s for this customer — usage would be invoiced twice; cancel that subscription first, or price both plans as items on one subscription",
		c.SubscriptionCode, c.MeterID))
}

// firstPlanInterval returns the BillingInterval to use for period
// anchoring on a sub. Reads the first item's plan via PlanReader; on
// any failure (reader not wired, plan deleted, RLS gap) defaults to
// BillingMonthly — the pre-Bug-#10 behavior, so unwired test paths
// don't break.
//
// Stripe / Lago reject mixed intervals on the same sub. Velox should
// too eventually; for now the first-item-wins approach is consistent
// with how plans / currencies are resolved elsewhere (engine.go's
// firstPlanCurrency pattern).
func (s *Service) firstPlanInterval(ctx context.Context, tenantID string, items []domain.SubscriptionItem) domain.BillingInterval {
	if s.plans == nil || len(items) == 0 {
		return domain.BillingMonthly
	}
	plan, err := s.plans.GetPlan(ctx, tenantID, items[0].PlanID)
	if err != nil || plan.BillingInterval == "" {
		return domain.BillingMonthly
	}
	return plan.BillingInterval
}

// CreateItemInput is a single priced line the caller wants on a new
// subscription. At least one item is required; duplicate plan_ids are rejected
// so the underlying UNIQUE (subscription_id, plan_id) never surfaces as a
// mid-transaction conflict.
type CreateItemInput struct {
	PlanID   string `json:"plan_id"`
	Quantity int64  `json:"quantity,omitempty"`
}

// maxTrialDays is the longest trial create and extend-trial accept: two
// years, Stripe's limit for trial_end.
const maxTrialDays = 730

type CreateInput struct {
	Code        string                         `json:"code"`
	DisplayName string                         `json:"display_name"`
	CustomerID  string                         `json:"customer_id"`
	Items       []CreateItemInput              `json:"items"`
	BillingTime domain.SubscriptionBillingTime `json:"billing_time"`
	TrialDays   int                            `json:"trial_days,omitempty"`
	// Status is empty (the subscription starts now, or starts its trial now)
	// or "draft" (it bills nothing until Activate). A start is the default,
	// as in Stripe and Lago, because a forgotten flag used to make a draft
	// that silently billed nothing (simplification audit D2, 2026-10-23).
	Status domain.SubscriptionStatus `json:"status,omitempty"`
}

// createStatusMessage is the 422 for a create's status other than "draft",
// including one sent empty or null (the handler refuses those).
const createStatusMessage = `must be "draft" or left out; a subscription starts now unless it is created as a draft`

func (s *Service) Create(ctx context.Context, tenantID string, input CreateInput) (domain.Subscription, error) {
	code := strings.TrimSpace(input.Code)
	displayName := strings.TrimSpace(input.DisplayName)

	if code == "" {
		return domain.Subscription{}, errs.Required("code")
	}
	if !slugPattern.MatchString(code) {
		return domain.Subscription{}, errs.Invalid("code", "must contain only alphanumeric characters, hyphens, and underscores")
	}
	if displayName == "" {
		return domain.Subscription{}, errs.Required("display_name")
	}
	if input.CustomerID == "" {
		return domain.Subscription{}, errs.Required("customer_id")
	}
	if len(input.Items) == 0 {
		return domain.Subscription{}, errs.Required("items")
	}

	seen := make(map[string]struct{}, len(input.Items))
	items := make([]domain.SubscriptionItem, 0, len(input.Items))
	for i, in := range input.Items {
		if in.PlanID == "" {
			return domain.Subscription{}, errs.Required(fmt.Sprintf("items[%d].plan_id", i))
		}
		if _, dup := seen[in.PlanID]; dup {
			return domain.Subscription{}, errs.Invalid("items", fmt.Sprintf("duplicate plan_id %q", in.PlanID))
		}
		seen[in.PlanID] = struct{}{}
		qty := in.Quantity
		if qty == 0 {
			qty = 1
		}
		if qty < 1 {
			return domain.Subscription{}, errs.Invalid(fmt.Sprintf("items[%d].quantity", i), "must be >= 1")
		}
		items = append(items, domain.SubscriptionItem{
			PlanID:   in.PlanID,
			Quantity: qty,
		})
	}

	billingTime := input.BillingTime
	if billingTime == "" {
		billingTime = domain.BillingTimeCalendar
	}
	if billingTime != domain.BillingTimeCalendar && billingTime != domain.BillingTimeAnniversary {
		return domain.Subscription{}, errs.Invalid("billing_time", "must be calendar or anniversary")
	}
	// A negative trial_days used to read as "no trial", so the customer was
	// charged on day 1. The upper bound is Stripe's: a trial "can be at most
	// two years". A longer pause in billing is not a trial.
	if input.TrialDays < 0 || input.TrialDays > maxTrialDays {
		return domain.Subscription{}, errs.Invalid("trial_days", fmt.Sprintf("must be between 0 and %d", maxTrialDays))
	}
	draft := false
	switch input.Status {
	case "":
	case domain.SubscriptionDraft:
		draft = true
	default:
		return domain.Subscription{}, errs.Invalid("status", createStatusMessage)
	}
	// A draft does not start, so it has no trial to start; Activate starts
	// billing at once. Refused, not ignored.
	if draft && input.TrialDays > 0 {
		return domain.Subscription{}, errs.Invalid("trial_days", "a draft cannot have a trial; leave out status to start the trial now")
	}

	// Reject mixed billing intervals across items (Stripe / Lago parity).
	// All items on a sub must share an interval — the period anchor is
	// per-sub, so a monthly + yearly mix has no coherent cycle. Skipped
	// when PlanReader isn't wired (narrow unit tests) or when there's
	// only one item.
	if s.plans != nil && len(items) > 1 {
		if err := s.rejectMixedItemIntervals(ctx, tenantID, items); err != nil {
			return domain.Subscription{}, err
		}
	}

	// Customer-level test-clock attach (ADR-027, Stripe parity): a
	// sub's clock is unconditionally inherited from its owning customer.
	// The API does not accept a per-sub test_clock_id — Stripe doesn't
	// either, and accepting one only created a redundant validation
	// path against the canonical customer-level value.
	//
	// Skipped when the customer reader isn't wired (narrow unit-test
	// path); the sub then has no clock unless the test sets one
	// directly on the domain.Subscription it constructs.
	var inheritedClockID string
	if s.customers != nil {
		cust, err := s.customers.Get(ctx, tenantID, input.CustomerID)
		if errors.Is(err, errs.ErrNotFound) {
			// This read runs in the caller's tenant and mode and is the only
			// check that does. The foreign key on customer_id is on the id
			// alone and foreign-key checks do not apply row-level security,
			// so the insert accepts another tenant's or the other mode's
			// customer.
			return domain.Subscription{}, errs.NotFound("customer")
		}
		if err != nil {
			// A read error must not skip the archived guard or the clock
			// inheritance (a clock customer's sub would bill on wall time).
			return domain.Subscription{}, fmt.Errorf("get customer: %w", err)
		}
		// Archived customer = no NEW subscriptions (ADR-067). Archive's
		// contract is "hide + stop new business"; without this guard a
		// direct API call quietly starts billing a customer the operator
		// explicitly retired.
		if cust.Status == domain.CustomerStatusArchived {
			return domain.Subscription{}, errs.InvalidState("customer is archived — unarchive the customer before creating a subscription")
		}
		inheritedClockID = cust.TestClockID
	}

	// Archived plans accept no new subscriptions (ADR-067; same enforcement
	// seam as the archived-customer guard). Pre-fix an archived plan
	// remained fully subscribable via the API — archive only hid it from
	// the picker. Skipped when PlanReader isn't wired (narrow unit tests).
	if s.plans != nil {
		planIDs := make([]string, 0, len(items))
		for _, it := range items {
			planIDs = append(planIDs, it.PlanID)
		}
		if err := s.rejectArchivedPlans(ctx, tenantID, planIDs); err != nil {
			return domain.Subscription{}, err
		}

		// Double-billing guard: the new sub's meters must be disjoint from
		// every other live sub of this customer (and from each other).
		if err := s.rejectMeterOverlap(ctx, tenantID, input.CustomerID, "", "", planIDs); err != nil {
			return domain.Subscription{}, err
		}
		// Currency pin (ADR-100): the new sub's plans must agree with each
		// other, the profile currency, and the customer's other live subs.
		if err := s.rejectCurrencyMismatch(ctx, tenantID, input.CustomerID, "", planIDs, ""); err != nil {
			return domain.Subscription{}, err
		}
	}

	status := domain.SubscriptionDraft
	// Bind effective-now from the customer's test_clock pin (if any).
	// Every downstream s.clock.Now(ctx) — including in the postgres
	// store's created_at / updated_at writes — inherits the simulated
	// time. Mirrors Stripe's "no semantic change" guarantee at
	// resource-create.
	ctx = s.bindForCustomer(ctx, tenantID, input.CustomerID)
	now := s.clock.Now(ctx)

	var trialStart, trialEnd *time.Time
	var startedAt, activatedAt *time.Time

	var periodStart, periodEnd, nextBilling *time.Time
	billingAnchorDay := 0

	// Resolve tenant TZ once for all period-boundary snaps below. Day-
	// grade billing (Chargebee / Lago default) snaps both endpoints to
	// 00:00 in tenant TZ so the "Period: <start> - <end>" UI exactly
	// matches the proration math. Without this, a sub created at 14:00
	// on the 1st gets billed for 30/31 days even though the UI shows
	// May 1 → Jun 1 — see service_test.go TestPeriod_DayGradeSnap.
	// The org's billing timezone (ADR-077) — every sub in a tenant bills in
	// the one org zone (default UTC). This resolves it once for the first
	// period's civil-day math; it is NOT snapshotted onto the sub (a per-sub
	// timezone was ADR-074, superseded by ADR-077 — no peer bills each subscription in
	// its own zone, and it caused a divergence class that cost ~8 render bugs).
	loc, err := s.tenantLocation(ctx, tenantID)
	if err != nil {
		return domain.Subscription{}, err
	}

	if input.TrialDays > 0 {
		ts := now
		// Trial days are calendar days in the tenant zone: a 24h-per-day
		// add drifts an hour across DST and can push trial_end (and so the
		// first paid period's start) past civil midnight onto the next day.
		te := domain.AddDaysIn(now, input.TrialDays, loc)
		trialStart = &ts
		trialEnd = &te
		status = domain.SubscriptionTrialing
		startedAt = &now
		// First post-trial period anchors on trial_end. Yearly plans
		// get a full year from trial-end; monthly + calendar produces
		// the trial_end → next-month-start stub (Stripe parity);
		// monthly + anniversary produces a full month from trial_end.
		// See helper doc for edge cases.
		interval := s.firstPlanInterval(ctx, tenantID, items)
		ps, pe, anchorDay := firstPeriodAfterTrial(te, billingTime, interval, loc)
		periodStart = &ps
		periodEnd = &pe
		nextBilling = &pe
		billingAnchorDay = anchorDay
	} else if !draft {
		status = domain.SubscriptionActive
		startedAt = &now
		// Immediate activation IS an activation — stamp activated_at so
		// the sub is visible to MRR movement / point-in-time / churn,
		// which key on activated_at as the MRR-start event. The operator
		// Activate() and trial-end ActivateAfterTrial() paths already do
		// this; the start-now create path used to leave it NULL, so
		// every no-trial sub was counted in headline MRR (status='active')
		// but invisible to New/Net movement — the dashboard never
		// reconciled. Trialing subs are NOT activated here (their
		// activated_at is stamped at trial-end).
		activatedAt = &now
		interval := s.firstPlanInterval(ctx, tenantID, items)
		ps, pe, anchorDay := firstPeriodForActivate(now, billingTime, interval, loc)
		periodStart = &ps
		periodEnd = &pe
		nextBilling = &pe
		billingAnchorDay = anchorDay
	}

	// ADR-031: in_advance plans get a day-1 invoice covering the upcoming
	// period's base fee. Build it IN THE SAME tx as the subscription insert
	// (ADR-056 coordinator) so a billing failure rolls the whole create back
	// rather than leaving an active sub with no first-period invoice. The old
	// "deferred to next cycle close" backstop was false: the cycle scheduler
	// bills the UPCOMING period and SKIPS the just-elapsed in_advance segment,
	// so a dropped day-1 invoice is a permanent revenue leak. Trialing subs
	// skip this (their first invoice fires at trial-end). External steps (tax
	// commit + auto-charge) run post-commit via FinalizeOnCreateInvoice.
	var createdInv domain.Invoice
	var haveInv bool
	sub, err := s.store.CreateWithBill(ctx, tenantID, domain.Subscription{
		Code:                      code,
		DisplayName:               displayName,
		CustomerID:                input.CustomerID,
		Status:                    status,
		BillingTime:               billingTime,
		TrialStartAt:              trialStart,
		TrialEndAt:                trialEnd,
		StartedAt:                 startedAt,
		ActivatedAt:               activatedAt,
		CurrentBillingPeriodStart: periodStart,
		CurrentBillingPeriodEnd:   periodEnd,
		NextBillingAt:             nextBilling,
		BillingAnchorDay:          billingAnchorDay,
		TestClockID:               inheritedClockID,
		Items:                     items,
	}, func(tx *sql.Tx, created domain.Subscription) error {
		if s.biller == nil || created.Status != domain.SubscriptionActive {
			return nil
		}
		inv, ok, billErr := s.biller.BillOnCreateTx(ctx, tx, created, now)
		if billErr != nil {
			return fmt.Errorf("first-invoice-on-create: %w", billErr)
		}
		createdInv, haveInv = inv, ok
		return nil
	})
	if err != nil {
		return domain.Subscription{}, err
	}

	// Post-commit external steps (tax commit + auto-charge) — Stripe calls must
	// never ride a DB tx, and the invoice is now durable + atomic with the sub.
	// A failure here is recovered by the existing tax-commit reconciler /
	// auto-charge retry sweep, unlike the lost-invoice case the tx closes.
	if haveInv {
		s.biller.FinalizeOnCreateInvoice(ctx, sub, createdInv)
	}

	return sub, nil
}

func (s *Service) Get(ctx context.Context, tenantID, id string) (domain.Subscription, error) {
	return s.store.Get(ctx, tenantID, id)
}

func (s *Service) List(ctx context.Context, filter ListFilter) ([]domain.Subscription, int, error) {
	return s.store.List(ctx, filter)
}

func (s *Service) Activate(ctx context.Context, tenantID, id string) (domain.Subscription, error) {
	sub, err := s.store.Get(ctx, tenantID, id)
	if err != nil {
		return domain.Subscription{}, err
	}
	if sub.Status != domain.SubscriptionDraft {
		return domain.Subscription{}, errs.InvalidState(fmt.Sprintf("Only a draft subscription can be activated. This one is %s.", sub.Status))
	}

	// Currency pin (ADR-100): drafts predating the pin (or created while a
	// divergent profile existed) must not start billing in a currency that
	// conflicts with the profile or the customer's other live subs.
	if len(sub.Items) > 0 {
		pin := make([]string, 0, len(sub.Items))
		for _, it := range sub.Items {
			pin = append(pin, it.PlanID)
		}
		if err := s.rejectCurrencyMismatch(ctx, tenantID, sub.CustomerID, id, pin, ""); err != nil {
			return domain.Subscription{}, err
		}
	}

	// Bind effective-now via the sub's TestClockID; downstream stamps
	// inherit. Activate's writes (activated_at, started_at, period
	// bounds, next_billing_at, updated_at on the sub row) all land in
	// simulated time on clock-pinned subs.
	ctx = s.bindForSub(ctx, tenantID, id)
	now := s.clock.Now(ctx)

	// First-period math: same helper as Create's start-now branch — period
	// begins at `now` (day-snapped) and ends at the next calendar boundary
	// (calendar billing) or `now + 1 month` (anniversary). (The historical
	// beginningOfMonth backdating bug and its fix are recorded on
	// firstPeriodForActivate.) A draft that already carries a period (e.g.
	// seeded by an earlier flow) keeps it unchanged.
	ps, pe := sub.CurrentBillingPeriodStart, sub.CurrentBillingPeriodEnd
	anchorDay := sub.BillingAnchorDay
	if ps == nil {
		loc, err := s.tenantLocation(ctx, sub.TenantID)
		if err != nil {
			return domain.Subscription{}, err
		}
		interval := s.firstPlanInterval(ctx, tenantID, sub.Items)
		p1, p2, a := firstPeriodForActivate(now, sub.BillingTime, interval, loc)
		ps, pe, anchorDay = &p1, &p2, a
	}

	// Atomic flip + day-1 bill (ADR-056 pattern, draft sibling): the
	// in_advance first invoice rides the SAME tx as the activation. Pre-fix
	// Activate flipped status without ever billing — the first base fee of a
	// draft-then-activated in_advance sub was silently never invoiced by any
	// path (BillOnCreate only runs on create; the first cycle close bills the
	// NEXT period in advance). A bill failure rolls the activation back.
	var firstInv domain.Invoice
	var haveInv bool
	activated, err := s.store.ActivateDraftWithBill(ctx, tenantID, id, now, *ps, *pe, anchorDay, func(tx *sql.Tx, a domain.Subscription) error {
		if s.biller == nil {
			return nil
		}
		inv, ok, billErr := s.biller.BillOnCreateTx(ctx, tx, a, now)
		if billErr != nil {
			return fmt.Errorf("first-invoice-on-activate: %w", billErr)
		}
		firstInv, haveInv = inv, ok
		return nil
	})
	if err != nil {
		return domain.Subscription{}, err
	}

	// Post-commit external steps (tax commit + auto-charge) — mirrors Create:
	// Stripe calls never ride a DB tx; the invoice is durable + atomic with
	// the flip, and failures here are recovered by the tax-commit reconciler /
	// auto-charge retry sweep.
	if haveInv {
		s.biller.FinalizeOnCreateInvoice(ctx, activated, firstInv)
	}

	return activated, nil
}

// ---- Items ----

// AddItemInput adds a new priced line to an existing subscription.
type AddItemInput struct {
	PlanID   string `json:"plan_id"`
	Quantity int64  `json:"quantity,omitempty"`
}

// UpdateItemInput mutates a single item. Exactly one of {Quantity, NewPlanID}
// may be supplied per call — separating the two keeps the proration branches
// distinct and avoids having to reason about "changed plan and quantity in
// one shot" edge cases. Quantity changes settle within the current billing
// period via the quantity-proration code path (separate from plan-change
// proration). Plan changes follow Immediate/scheduled semantics mirroring the
// prior ChangePlan behaviour, now per-item.
type UpdateItemInput struct {
	Quantity  *int64 `json:"quantity,omitempty"`
	NewPlanID string `json:"new_plan_id,omitempty"`
	Immediate bool   `json:"immediate,omitempty"`
}

// ItemChangeResult mirrors ChangePlanResult but scoped to a single item. The
// Proration payload is stamped by the billing layer when the caller requests
// an immediate plan change; AddItem/RemoveItem/quantity-only edits return
// nil Proration (their proration goes through separate invoice/credit lines
// stitched in at next-cycle close).
type ItemChangeResult struct {
	Item        domain.SubscriptionItem `json:"item"`
	EffectiveAt time.Time               `json:"effective_at"`
	Proration   *ProrationDetail        `json:"proration,omitempty"`
	// OrchestratedCrossAxis is true when UpdateItem went through the
	// cross-axis (cross-interval or cross-cadence) orchestrator path.
	// The handler MUST NOT run its legacy handleItemProration when
	// this flag is set — the orchestrator already issued the refund
	// credit and (for NEW in_advance) the new in_advance invoice;
	// firing handleItemProration on top would emit a second credit
	// against the same OLD period and over-credit the customer.
	OrchestratedCrossAxis bool `json:"-"`
	// crossAxisNewInvoice carries the swap invoice (the new in_advance
	// period's base plus the old period's usage up to the swap) created
	// in-tx by the atomic cross-interval swap (applyCrossIntervalPlanSwapTx),
	// so the handler can run FinalizeOnCreateInvoice (tax commit + auto-charge)
	// POST-commit. nil when the swap was in_arrears (no day-1 invoice).
	// Internal — not serialized.
	crossAxisNewInvoice *domain.Invoice
	// swapRelief is the OLD period's refund written ON the swap tx by
	// BillOnPlanSwapDraftsTx; FinalizeCrossIntervalSwap runs its post-commit
	// legs.
	swapRelief domain.Relief
	// swapAfterCommit is BillOnPlanSwapTx's afterCommit, which
	// FinalizeCrossIntervalSwap runs once the swap tx committed. nil when the
	// swap billed nothing in-tx (in_arrears).
	swapAfterCommit func()
	// pricedFrom is the state the change replaced: the subscription and the
	// item as UpdateItemTx or RemoveItemTx found them under the subscription
	// row lock (#1270); for AddItemTx, the subscription only. The handler
	// prices the proration from it, never from the read it made before the
	// transaction. Internal — not serialized.
	pricedFrom itemChangeBase
}

// itemChangeBase is a subscription and one of its items, read together under
// the subscription row lock.
type itemChangeBase struct {
	sub  domain.Subscription
	item domain.SubscriptionItem
}

type ProrationDetail struct {
	OldPlanID       string  `json:"old_plan_id"`
	NewPlanID       string  `json:"new_plan_id"`
	ProrationFactor float64 `json:"proration_factor"`
	// AmountCents is the money amount of the proration OUTCOME artifact,
	// gross in every type: the invoice's total including tax ("invoice"),
	// the gross credited to the balance ("credit"), or the amount_due
	// relieved on the unpaid source ("adjustment"). The pre-tax net delta
	// is derivable from the invoice itself (subtotal_cents).
	AmountCents int64  `json:"amount_cents"`
	Type        string `json:"type"` // "invoice", "credit", or "adjustment" (unpaid-source amount_due reduction, ADR-050)
	InvoiceID   string `json:"invoice_id,omitempty"`
	// TaxProvider / TaxCalculationID are internal (not API surface) — they
	// route the post-commit Stripe Tax transaction creation up to the atomic
	// caller, which can only commit after its tx is durable. Empty for
	// credit-path (downgrade) prorations and manual/none providers.
	TaxProvider      string `json:"-"`
	TaxCalculationID string `json:"-"`
	// Clawback* carry the credit for unused time (a downgrade, quantity
	// decrease or item removal), issued as tax-reversing adjustment credit
	// notes AFTER the atomic tx commits: the credit-note service's tax reversal
	// is an external Stripe call, so it can't ride the DB tx. ClawbackPieces
	// are the shares domain.AllocateUnusedCredit placed, one per invoice that
	// charged the item, newest first (ADR-118). On the ATOMIC path each is
	// CREATED as a draft IN the caller's tx (createClawbackDraftsTx →
	// ClawbackDraftIDs) so it commits with the item change, then ISSUED
	// post-commit (issueClawbackDrafts); the non-atomic path create+issues
	// inline via issueClawbackCreditNote. Internal — not API surface.
	ClawbackReason string               `json:"-"`
	ClawbackMemo   string               `json:"-"`
	ClawbackPieces []domain.CreditShare `json:"-"`
	// UncreditedCents (net) is the unused time the invoices had no room left
	// for. It is not owed; the audit row records it (uncredited_cents).
	UncreditedCents int64 `json:"-"`
	// ClawbackDraftIDs are the draft credit notes createClawbackDraftsTx wrote
	// IN the atomic tx (atomic with the item change). issueClawbackDrafts issues
	// them post-commit; RetryPendingClawbackIssue re-issues any whose Issue()
	// failed. Empty on the non-atomic path (it issues inline). Internal.
	ClawbackDraftIDs []string `json:"-"`
	// AutoChargeInvoiceID is the freshly-created proration CHARGE invoice to
	// enroll into the auto-charge sweep AFTER the tx commits (the sweep then
	// collects it — wall-clock cron for live subs, test-clock catchup on
	// advance for clock-pinned). Set only on fresh creation of a positive
	// proration invoice, never on the dedup-replay path (that invoice may
	// already be paid). Empty for credit/adjustment paths. Internal.
	AutoChargeInvoiceID string `json:"-"`
	// finalizedInvoice is the proration invoice this change created already
	// finalized, for the invoice.finalized webhook sent after commit
	// (dispatchProrationFinalized). nil for a draft, a credit and the dedup
	// replay.
	finalizedInvoice *domain.Invoice
}

// addItemQuantity checks the request alone and returns the quantity to
// write.
func addItemQuantity(input AddItemInput) (int64, error) {
	if input.PlanID == "" {
		return 0, errs.Required("plan_id")
	}
	qty := input.Quantity
	if qty == 0 {
		qty = 1
	}
	if qty < 1 {
		return 0, errs.Invalid("quantity", "must be >= 1")
	}
	return qty, nil
}

// validateAddItem checks an add against sub, the subscription AddItemTx
// read under the subscription row lock (#1270).
//
// Its other reads (plans, meter conflicts, currencies) run in their own
// transactions. That is enough for this subscription's items: every write
// to them takes the same lock first, so the reads see every change to them
// that committed before the lock, and none can commit until the caller's
// transaction ends. Two adds of plans that bill one meter are therefore
// checked one after the other. The unique index on (subscription, plan) is
// the write-time gate for duplicates. Archiving a plan writes only the plan
// row, so an archive that commits between this check and the insert orders
// as "added, then archived": an existing item on an archived plan, which
// keeps billing (ADR-067).
func (s *Service) validateAddItem(ctx context.Context, tenantID string, sub domain.Subscription, planID string) error {
	if sub.Status == domain.SubscriptionCanceled {
		return errs.InvalidState(fmt.Sprintf("Items can't be added to a %s subscription.", sub.Status))
	}

	// Skipped when PlanReader isn't wired (narrow unit tests).
	if s.plans != nil {
		// An archived plan takes no new business (ADR-067), and an added
		// item is new business on its plan. Same refusal as Create.
		if err := s.rejectArchivedPlans(ctx, tenantID, []string{planID}); err != nil {
			return err
		}
		// Mixed-interval guard: new item's plan must match the sub's
		// existing item intervals. Same rationale as Create — the period
		// anchor is per-sub, so adding a yearly item to a monthly sub
		// would have no coherent cycle.
		if len(sub.Items) > 0 {
			newPlusExisting := append([]domain.SubscriptionItem{{PlanID: planID}}, sub.Items...)
			if err := s.rejectMixedItemIntervals(ctx, tenantID, newPlusExisting); err != nil {
				return err
			}
		}
	}

	// Double-billing guard: the added plan's meters must be disjoint from
	// every live sub of the customer — including this sub's other items.
	if err := s.rejectMeterOverlap(ctx, tenantID, sub.CustomerID, sub.ID, "", []string{planID}); err != nil {
		return err
	}
	// Currency pin (ADR-100): the added plan + this sub's existing items
	// must share one currency (and match profile + the customer's other
	// subs — this sub is excluded from the cross-sub query, so its own
	// items ride in planIDs).
	pin := []string{planID}
	for _, it := range sub.Items {
		pin = append(pin, it.PlanID)
	}
	return s.rejectCurrencyMismatch(ctx, tenantID, sub.CustomerID, sub.ID, pin, "")
}

// AddItem adds an item in a transaction of its own: AddItemTx's lock,
// checks and write, for an add the handler does not prorate (a trialing
// subscription, the last hours of a period).
func (s *Service) AddItem(ctx context.Context, tenantID, subscriptionID string, input AddItemInput) (domain.SubscriptionItem, error) {
	var res ItemChangeResult
	err := s.store.WithTenantTx(ctx, tenantID, func(tx *sql.Tx) error {
		var err error
		res, err = s.AddItemTx(ctx, tx, tenantID, subscriptionID, input)
		return err
	})
	return res.Item, err
}

// AddItemTx adds an item on the caller's tx: the handler's atomic
// add-with-proration tx, so a failed proration rolls the insert back too
// (ADR-030 atomic-proration follow-through), or AddItem's own.
//
// Its first statement locks the subscription row; the checks and the period
// check run on the state read under that lock (#1270). Two adds of plans
// that bill one meter are checked one after the other, and an add that
// waited on a plan swap or a period close sees what it wrote. The result
// carries that state: the handler prices the add from it, never from its own
// read before the tx.
func (s *Service) AddItemTx(ctx context.Context, tx *sql.Tx, tenantID, subscriptionID string, input AddItemInput) (ItemChangeResult, error) {
	qty, err := addItemQuantity(input)
	if err != nil {
		return ItemChangeResult{}, err
	}
	ctx = s.bindForSub(ctx, tenantID, subscriptionID)
	sub, err := s.store.LockForItemChangeTx(ctx, tx, tenantID, subscriptionID)
	if err != nil {
		return ItemChangeResult{}, err
	}
	if err := s.validateAddItem(ctx, tenantID, sub, input.PlanID); err != nil {
		return ItemChangeResult{}, err
	}
	now := s.clock.Now(ctx)
	// As in changeItemUnderLock: the boundary can pass after the handler's
	// CloseEndedPeriodFirst. An item added to a due period is missing from
	// the invoice the billing run builds for it. The same condition as
	// CloseEndedPeriodFirst: a subscription with no items cannot be billed,
	// so refusing its add would leave its period due for good.
	if sub.Status == domain.SubscriptionActive && len(sub.Items) > 0 && domain.BillingDueAt(sub, now) {
		return ItemChangeResult{}, errUnbilledPeriod(sub)
	}
	item, err := s.store.AddItemTx(ctx, tx, tenantID, domain.SubscriptionItem{
		SubscriptionID: subscriptionID,
		PlanID:         input.PlanID,
		Quantity:       qty,
	})
	if err != nil {
		return ItemChangeResult{}, err
	}
	return ItemChangeResult{Item: item, EffectiveAt: now, pricedFrom: itemChangeBase{sub: sub}}, nil
}

// UpdateItemTx is the in-transaction variant of UpdateItem for the
// simple paths (quantity change, same-interval immediate plan change).
// The immediate cross-interval swap runs here too, atomically
// (applyCrossIntervalPlanSwapTx); the non-Tx UpdateItem refuses it. The
// scheduled-change path needs no tx (no proration emission).
//
// The checks below read the item and the subscription before the tx. The
// write then runs from the state read under the subscription row lock
// (changeItemUnderLock, or the swap's period CAS plus its item check), and
// the result carries that state for the handler to price from (#1270).
func (s *Service) UpdateItemTx(ctx context.Context, tx *sql.Tx, tenantID, subscriptionID, itemID string, input UpdateItemInput) (ItemChangeResult, error) {
	if input.Quantity == nil && input.NewPlanID == "" {
		return ItemChangeResult{}, errs.Invalid("body", "one of quantity or new_plan_id is required")
	}
	if input.Quantity != nil && input.NewPlanID != "" {
		return ItemChangeResult{}, errs.Invalid("body", "quantity and new_plan_id cannot be set together; issue two requests")
	}
	sub, err := s.store.Get(ctx, tenantID, subscriptionID)
	if err != nil {
		return ItemChangeResult{}, err
	}
	// The item comes from the same read as sub.Items, which the swap's item
	// check compares under the lock: a change that commits between two reads
	// cannot slip past it.
	item, ok := findItem(sub.Items, itemID)
	if !ok {
		return ItemChangeResult{}, errs.ErrNotFound
	}
	if sub.Status != domain.SubscriptionActive {
		return ItemChangeResult{}, errs.InvalidState(fmt.Sprintf("Items can only be changed on an active subscription. This one is %s.", sub.Status))
	}
	ctx = s.bindForSub(ctx, tenantID, subscriptionID)
	now := s.clock.Now(ctx)

	if input.Quantity != nil {
		if *input.Quantity < 1 {
			return ItemChangeResult{}, errs.Invalid("quantity", "must be >= 1")
		}
		// The "same as current" check runs under the lock, against the
		// quantity the change is priced from.
		return s.changeItemUnderLock(ctx, tx, tenantID, subscriptionID, item, input)
	}

	// Plan-change branch. Same-interval + same-cadence immediate is the simple
	// atomic plan write below. Cross-interval (same-cadence) immediate now
	// restructures the cycle atomically on THIS tx via applyCrossIntervalPlanSwapTx
	// (plan write + watermark + new in_advance invoice committed together).
	// Scheduled changes still return the not-applicable sentinel (handler falls
	// back to the legacy non-atomic flow); cross-cadence swaps are rejected.
	if input.NewPlanID == item.PlanID {
		return ItemChangeResult{}, errs.Invalid("new_plan_id", "new plan is the same as current plan")
	}
	if s.plans != nil {
		currentPlan, perr := s.plans.GetPlan(ctx, tenantID, item.PlanID)
		if perr != nil {
			return ItemChangeResult{}, errs.Invalid("items", fmt.Sprintf("plan %q not found", item.PlanID))
		}
		newPlan, perr := s.plans.GetPlan(ctx, tenantID, input.NewPlanID)
		if perr != nil {
			return ItemChangeResult{}, errs.Invalid("new_plan_id", fmt.Sprintf("plan %q not found", input.NewPlanID))
		}
		// Swapping ONTO an archived plan is new business on a retired price
		// (ADR-067) — same guard as subscription.Create. Existing items
		// already on an archived plan keep billing (archive ≠ cancel).
		if newPlan.Status == domain.PlanArchived {
			return ItemChangeResult{}, errs.InvalidState(fmt.Sprintf("plan %q is archived — pick an active plan", input.NewPlanID))
		}
		// Double-billing guard: the swapped-to plan's meters must be
		// disjoint from every live sub of the customer and this sub's
		// OTHER items. The item being swapped is excluded — its outgoing
		// plan stops billing when the new one starts.
		if err := s.rejectMeterOverlap(ctx, tenantID, sub.CustomerID, subscriptionID, itemID, []string{input.NewPlanID}); err != nil {
			return ItemChangeResult{}, err
		}
		// Currency pin (ADR-100): a swap may not cross currencies — the
		// proration path subtracts old-plan cents from new-plan cents as
		// raw integers, and the sub's other items + profile + other subs
		// must stay coherent. Outgoing plan resolved via the swapped item.
		if err := s.guardSwapCurrency(ctx, tenantID, sub, subscriptionID, itemID, input.NewPlanID); err != nil {
			return ItemChangeResult{}, err
		}
		if !input.Immediate {
			return ItemChangeResult{}, errAtomicNotApplicable
		}
		// Mixed-interval guard — mirrors the non-atomic UpdateItem (the engine's
		// hybrid-invoice + per-sub period anchor assume a UNIFORM interval across
		// items). Without this, a multi-item swap would re-anchor the whole sub
		// to the swapped item's new interval and bill the UNCHANGED items over
		// the wrong period (e.g. a monthly item billed a full base over a yearly
		// period — an overcharge). Reject the swap if the post-change set mixes
		// intervals, exactly as the non-atomic path does.
		hypothetical := make([]domain.SubscriptionItem, 0, len(sub.Items))
		for _, existing := range sub.Items {
			if existing.ID == itemID {
				hypothetical = append(hypothetical, domain.SubscriptionItem{PlanID: input.NewPlanID})
			} else {
				hypothetical = append(hypothetical, existing)
			}
		}
		if err := s.rejectMixedItemIntervals(ctx, tenantID, hypothetical); err != nil {
			return ItemChangeResult{}, err
		}
		currentTiming := currentPlan.BaseBillTiming
		if currentTiming == "" {
			currentTiming = domain.BillInArrears
		}
		newTiming := newPlan.BaseBillTiming
		if newTiming == "" {
			newTiming = domain.BillInArrears
		}
		// Cross-cadence (in_advance ↔ in_arrears) plan-swap is unsupported —
		// no industry peer documents it as an in-place change (2026-05-21
		// verification). Same rejection + message as the non-atomic UpdateItem.
		if currentTiming != newTiming {
			return ItemChangeResult{}, errs.Invalid("new_plan_id", fmt.Sprintf(
				"bill_timing change is not supported on plan-swap (current %s, new %s); cancel the subscription and start a new one with the target plan",
				currentTiming, newTiming))
		}
		currentInterval := currentPlan.BillingInterval
		if currentInterval == "" {
			currentInterval = domain.BillingMonthly
		}
		newInterval := newPlan.BillingInterval
		if newInterval == "" {
			newInterval = domain.BillingMonthly
		}
		if currentInterval != newInterval {
			// Cross-interval, immediate, same-cadence → atomic restructure on
			// THIS tx. The OLD-period refund + the new invoice's tax-commit and
			// auto-charge run POST-commit in the handler (external Stripe steps
			// must never ride the DB tx).
			res, err := s.applyCrossIntervalPlanSwapTx(ctx, tx, tenantID, subscriptionID, itemID, input.NewPlanID, sub, newInterval, newTiming, now)
			if err != nil {
				return ItemChangeResult{}, err
			}
			// The swap proved this snapshot under the row lock: the period by
			// its CAS, the items by its item check.
			res.pricedFrom = itemChangeBase{sub: sub, item: item}
			return res, nil
		}
	}
	if !input.Immediate {
		return ItemChangeResult{}, errAtomicNotApplicable
	}
	return s.changeItemUnderLock(ctx, tx, tenantID, subscriptionID, item, input)
}

// changeItemUnderLock writes a quantity change, a same-interval immediate
// plan change or a scheduled plan change from the state read under the
// subscription row lock (#1270). It is the first statement of the change on
// the caller's tx: UpdateItemTx's, or UpdateItem's own for a change that
// prorates nothing. checked is the item the caller ran its checks on, before
// the tx.
//
// A change that committed in between is seen here, not overwritten: a
// quantity change is priced from the quantity and plan the item holds now,
// and a request that now changes nothing is refused as a no-op. A plan
// change whose checks ran on another plan (bill timing, interval, meters,
// currency) is refused with item_changed rather than written unchecked, and
// its meter check runs again against the other items as they are now.
func (s *Service) changeItemUnderLock(ctx context.Context, tx *sql.Tx, tenantID, subscriptionID string, checked domain.SubscriptionItem, input UpdateItemInput) (ItemChangeResult, error) {
	sub, item, err := s.lockItemForChange(ctx, tx, tenantID, subscriptionID, checked.ID)
	if err != nil {
		return ItemChangeResult{}, err
	}
	if sub.Status != domain.SubscriptionActive {
		return ItemChangeResult{}, errs.InvalidState(fmt.Sprintf("Items can only be changed on an active subscription. This one is %s.", sub.Status))
	}
	// Read after the lock, so the change is stamped after any change that
	// committed before it.
	now := s.clock.Now(ctx)
	// The handler closed an ended period before its read
	// (CloseEndedPeriodFirst); the boundary can pass after that.
	if domain.BillingDueAt(sub, now) {
		return ItemChangeResult{}, errUnbilledPeriod(sub)
	}
	pricedFrom := itemChangeBase{sub: sub, item: item}

	if input.Quantity != nil {
		if *input.Quantity == item.Quantity {
			return ItemChangeResult{}, errs.Invalid("quantity", "new quantity is the same as current quantity")
		}
		updated, err := s.store.UpdateItemQuantityTx(ctx, tx, tenantID, item.ID, *input.Quantity)
		if err != nil {
			return ItemChangeResult{}, err
		}
		return ItemChangeResult{Item: updated, EffectiveAt: now, pricedFrom: pricedFrom}, nil
	}

	if input.NewPlanID == item.PlanID {
		return ItemChangeResult{}, errs.Invalid("new_plan_id", "new plan is the same as current plan")
	}
	if item.PlanID != checked.PlanID {
		return ItemChangeResult{}, errItemChanged()
	}
	// The meter check before the lock read the subscription's other items
	// as they were then. An add, or a plan change on another item, that
	// committed since may bill a meter the new plan bills; run it again now
	// that no change to these items can commit. It refuses only a change
	// that conflicts.
	if err := s.rejectMeterOverlap(ctx, tenantID, sub.CustomerID, subscriptionID, item.ID, []string{input.NewPlanID}); err != nil {
		return ItemChangeResult{}, err
	}
	if !input.Immediate {
		// Scheduled: the change takes effect when the period found under the
		// lock ends. It prorates nothing now.
		effectiveAt := now
		if sub.CurrentBillingPeriodEnd != nil {
			effectiveAt = *sub.CurrentBillingPeriodEnd
		}
		updated, err := s.store.SetItemPendingPlanTx(ctx, tx, tenantID, item.ID, input.NewPlanID, effectiveAt)
		if err != nil {
			return ItemChangeResult{}, err
		}
		return ItemChangeResult{Item: updated, EffectiveAt: effectiveAt, pricedFrom: pricedFrom}, nil
	}
	updated, err := s.store.ApplyItemPlanImmediatelyTx(ctx, tx, tenantID, item.ID, input.NewPlanID, now)
	if err != nil {
		return ItemChangeResult{}, err
	}
	return ItemChangeResult{Item: updated, EffectiveAt: now, pricedFrom: pricedFrom}, nil
}

// lockItemForChange locks the subscription row on tx and returns the
// subscription and the item as they stand under that lock (#1270).
// ErrNotFound when the item was removed, or is not on this subscription.
func (s *Service) lockItemForChange(ctx context.Context, tx *sql.Tx, tenantID, subscriptionID, itemID string) (domain.Subscription, domain.SubscriptionItem, error) {
	sub, err := s.store.LockForItemChangeTx(ctx, tx, tenantID, subscriptionID)
	if err != nil {
		return domain.Subscription{}, domain.SubscriptionItem{}, err
	}
	item, ok := findItem(sub.Items, itemID)
	if !ok {
		return domain.Subscription{}, domain.SubscriptionItem{}, errs.ErrNotFound
	}
	return sub, item, nil
}

// findItem returns the live item itemID among items.
func findItem(items []domain.SubscriptionItem, itemID string) (domain.SubscriptionItem, bool) {
	for _, it := range items {
		if it.ID == itemID {
			return it, true
		}
	}
	return domain.SubscriptionItem{}, false
}

// errItemChanged refuses a change whose checks or prices were computed from
// items that another change replaced before this one took the subscription
// row lock (#1270).
func errItemChanged() error {
	return errs.InvalidState("The subscription's items changed while this request was running. Refresh the page and try again.").WithCode("item_changed")
}

// sameItems reports whether two reads of a subscription's live items hold
// the same items on the same plans at the same quantities.
func sameItems(a, b []domain.SubscriptionItem) bool {
	if len(a) != len(b) {
		return false
	}
	byID := make(map[string]domain.SubscriptionItem, len(a))
	for _, it := range a {
		byID[it.ID] = it
	}
	for _, it := range b {
		was, ok := byID[it.ID]
		if !ok || was.PlanID != it.PlanID || was.Quantity != it.Quantity {
			return false
		}
	}
	return true
}

// errAtomicNotApplicable signals that the requested UpdateItem variant
// (scheduled change, cross-interval swap, cross-cadence swap) doesn't
// support the atomic-tx flow yet — handler should fall back to the
// legacy non-atomic UpdateItem. Sentinel rather than a typed error so
// the handler can errors.Is-match it.
var errAtomicNotApplicable = errs.InvalidState("atomic-tx UpdateItem applies only to quantity changes + same-interval immediate plan changes")

// UpdateItem applies a quantity-only patch OR a plan change (immediate or
// scheduled) to a single item. Exactly one of Quantity/NewPlanID must be set.
// Plan change semantics match the prior subscription-level ChangePlan: an
// immediate change supersedes any existing pending change on the same item,
// while a scheduled change records pending_plan_id + effective_at for the
// billing engine to apply at the next cycle boundary.
func (s *Service) UpdateItem(ctx context.Context, tenantID, subscriptionID, itemID string, input UpdateItemInput) (ItemChangeResult, error) {
	if input.Quantity == nil && input.NewPlanID == "" {
		return ItemChangeResult{}, errs.Invalid("body", "one of quantity or new_plan_id is required")
	}
	if input.Quantity != nil && input.NewPlanID != "" {
		return ItemChangeResult{}, errs.Invalid("body", "quantity and new_plan_id cannot be set together; issue two requests")
	}

	item, err := s.store.GetItem(ctx, tenantID, itemID)
	if err != nil {
		return ItemChangeResult{}, err
	}
	if item.SubscriptionID != subscriptionID {
		// Scoping the item to its parent keeps a tenant from mutating an item
		// on a subscription they didn't supply in the URL — the tenant_id RLS
		// check already blocks cross-tenant, but intra-tenant cross-sub has to
		// be enforced here.
		return ItemChangeResult{}, errs.ErrNotFound
	}
	sub, err := s.store.Get(ctx, tenantID, subscriptionID)
	if err != nil {
		return ItemChangeResult{}, err
	}
	if sub.Status != domain.SubscriptionActive {
		return ItemChangeResult{}, errs.InvalidState(fmt.Sprintf("Items can only be changed on an active subscription. This one is %s.", sub.Status))
	}

	// Bind via sub pin: pending_plan_effective_at (and the immediate-
	// effectiveAt return) need to be in simulated time so the engine's
	// catchup picks up the rollover at the operator's next Advance,
	// not at wall-clock-2026.
	ctx = s.bindForSub(ctx, tenantID, subscriptionID)

	if input.Quantity != nil {
		if *input.Quantity < 1 {
			return ItemChangeResult{}, errs.Invalid("quantity", "must be >= 1")
		}
		return s.changeItemInOwnTx(ctx, tenantID, subscriptionID, item, input)
	}

	if input.NewPlanID == item.PlanID {
		return ItemChangeResult{}, errs.Invalid("new_plan_id", "new plan is the same as current plan")
	}

	// Mixed-interval / mixed-bill-timing guard on plan-change. Composes
	// the post-change item set (other items unchanged + the new plan on
	// this item) and asserts every interval AND every bill_timing
	// matches. Same rationale as Create / AddItem — the period anchor
	// and invoice shape are per-sub and would drift if a single item
	// swapped to a different cadence. Skipped when PlanReader is
	// unwired (narrow unit tests).
	//
	// The multi-item check catches mixes; the inline single-item guard
	// below catches the case where swapping the only item changes the
	// sub's bill_timing (e.g. in_arrears $29 → in_advance $49). The
	// engine's hybrid-invoice shape assumes a uniform bill_timing per
	// sub; cross-bill-timing swaps are not exercised end-to-end, so
	// rejecting at request time is the pre-launch safe stance.
	if s.plans != nil {
		hypothetical := make([]domain.SubscriptionItem, 0, len(sub.Items))
		for _, existing := range sub.Items {
			if existing.ID == itemID {
				hypothetical = append(hypothetical, domain.SubscriptionItem{PlanID: input.NewPlanID})
			} else {
				hypothetical = append(hypothetical, existing)
			}
		}
		if err := s.rejectMixedItemIntervals(ctx, tenantID, hypothetical); err != nil {
			return ItemChangeResult{}, err
		}

		// Single-item bill_timing-change guard. The multi-item check
		// above can't fire when len(items)==1, so explicitly compare
		// the current item's plan timing against the new plan's
		// timing. Both legs default to in_arrears for pre-ADR-031
		// plans to match the engine's lenient defaults.
		currentPlan, err := s.plans.GetPlan(ctx, tenantID, item.PlanID)
		if err != nil {
			return ItemChangeResult{}, errs.Invalid("items", fmt.Sprintf("plan %q not found", item.PlanID))
		}
		newPlan, err := s.plans.GetPlan(ctx, tenantID, input.NewPlanID)
		if err != nil {
			return ItemChangeResult{}, errs.Invalid("new_plan_id", fmt.Sprintf("plan %q not found", input.NewPlanID))
		}
		// Same archived-plan guard as the atomic path (UpdateItemTx) and
		// subscription.Create — swapping onto a retired price is new business
		// (ADR-067).
		if newPlan.Status == domain.PlanArchived {
			return ItemChangeResult{}, errs.InvalidState(fmt.Sprintf("plan %q is archived — pick an active plan", input.NewPlanID))
		}
		// Double-billing guard — same as UpdateItemTx. Also gates SCHEDULED
		// changes: a pending swap onto an already-billed meter would start
		// double-billing at the next cycle boundary.
		if err := s.rejectMeterOverlap(ctx, tenantID, sub.CustomerID, subscriptionID, itemID, []string{input.NewPlanID}); err != nil {
			return ItemChangeResult{}, err
		}
		// Currency pin (ADR-100) — same as UpdateItemTx; also gates
		// scheduled swaps, which would re-denominate at the next boundary.
		if err := s.guardSwapCurrency(ctx, tenantID, sub, subscriptionID, itemID, input.NewPlanID); err != nil {
			return ItemChangeResult{}, err
		}
		currentTiming := currentPlan.BaseBillTiming
		if currentTiming == "" {
			currentTiming = domain.BillInArrears
		}
		newTiming := newPlan.BaseBillTiming
		if newTiming == "" {
			newTiming = domain.BillInArrears
		}
		// Cross-cadence plan-swap (in_advance ↔ in_arrears) is
		// rejected — both immediate and scheduled. 2026-05-21
		// industry verification across Stripe / Lago / Orb / Chargebee
		// / Recurly / Metronome found no documented support for
		// swapping a customer between a prepaid plan and a postpaid
		// plan as an in-place plan-change operation. Lago — the
		// closest model to Velox (per-plan pay_in_advance flag) —
		// explicitly documents same-cadence transitions only. The
		// industry pattern is: change collection_method at sub level
		// (Stripe) or end + recreate (Metronome contract transitions).
		// Operator path: cancel + recreate with the target plan.
		if currentTiming != newTiming {
			return ItemChangeResult{}, errs.Invalid("new_plan_id", fmt.Sprintf(
				"bill_timing change is not supported on plan-swap (current %s, new %s); cancel the subscription and start a new one with the target plan",
				currentTiming, newTiming))
		}

		currentInterval := currentPlan.BillingInterval
		if currentInterval == "" {
			currentInterval = domain.BillingMonthly
		}
		newInterval := newPlan.BillingInterval
		if newInterval == "" {
			newInterval = domain.BillingMonthly
		}
		crossInterval := currentInterval != newInterval

		// Immediate same-cadence cross-interval swap restructures the
		// cycle (e.g. yearly→monthly downgrade mid-year). Path differs
		// by cadence:
		//
		//   in_advance: refund the unused portion of the OLD prepaid
		//   period (BillOnPlanSwapDraftsTx), jump current_period
		//   to (now, NextBillingPeriodEnd for NEW interval), and
		//   synchronously bill one invoice (BillOnPlanSwapTx): the NEW
		//   in_advance period's base (with stub proration if calendar
		//   snap shortens it) plus the OLD plan's usage over
		//   (oldPS, now), which the jump closes.
		//
		//   in_arrears: truncate current_period to (oldPS, now). The
		//   scheduler / catchup picks up next_billing_at=now, closes
		//   the partial period under the OLD plan rate via segment-
		//   aware billing (subscription_item_changes captures the
		//   transition at `now`), then opens a new period under the
		//   NEW plan's interval. No synchronous bill — same UX as any
		//   other in_arrears cycle close.
		//
		// Industry shape (verified 2026-05-21): Stripe / Lago / Orb /
		// Chargebee / Recurly all ship immediate cross-interval with
		// similar restructure-and-bill semantics. Cross-cadence is
		// rejected upstream — no industry peer documents it as an
		// in-place plan-swap operation.
		if input.Immediate && crossInterval {
			// Only the atomic path (applyCrossIntervalPlanSwapTx, through the
			// handler's tx) may restructure the cycle: its refund, plan write,
			// period CAS and new-period invoice commit together. The old
			// non-atomic variant refunded first and swapped after, and lost the
			// refund on any failure in between (ADR-118).
			return ItemChangeResult{}, errs.InvalidState("an immediate cross-interval plan swap must run on the atomic path").WithCode("swap_requires_atomic_path")
		}
	}

	// A scheduled change, or an immediate one the handler does not prorate.
	return s.changeItemInOwnTx(ctx, tenantID, subscriptionID, item, input)
}

// changeItemInOwnTx runs changeItemUnderLock in a transaction of its own,
// for a change the handler does not prorate (a scheduled plan change, an
// immediate change in the last hours of a period): its checks run on the
// state under the subscription row lock as on the prorating path (#1270).
func (s *Service) changeItemInOwnTx(ctx context.Context, tenantID, subscriptionID string, checked domain.SubscriptionItem, input UpdateItemInput) (ItemChangeResult, error) {
	var res ItemChangeResult
	err := s.store.WithTenantTx(ctx, tenantID, func(tx *sql.Tx) error {
		var err error
		res, err = s.changeItemUnderLock(ctx, tx, tenantID, subscriptionID, checked, input)
		return err
	})
	return res, err
}

// mapPeriodMoved turns the store's stale-snapshot sentinel into the 409 an
// item change returns when the subscription's billing period changed between
// the request's read and its write (ADR-115). Everything else passes through.
func mapPeriodMoved(err error) error {
	if errors.Is(err, ErrWatermarkMoved) {
		return errs.InvalidState("This subscription just moved to a new billing period. Refresh the page and try again.").WithCode("subscription_period_moved")
	}
	return err
}

// applyCrossIntervalPlanSwapTx is the immediate same-cadence cross-interval
// plan swap — the only restructure-the-cycle plan change Velox supports —
// run on the handler's tx. It runs the
// cycle-restructuring DB writes — the period CAS (first statement, ADR-115),
// the refund drafts, the plan swap, and (for in_advance) the swap invoice
// insert via BillOnPlanSwapTx (the new period's base plus the old period's
// usage up to now) — on the caller's single tx, so any failure rolls
// ALL of them back. This is the fix for the
// silent revenue drop: the watermark can no longer advance past a new in_advance
// period whose day-1 invoice failed to commit. The OLD-period refund and the new
// invoice's external steps (tax commit, auto-charge) are deliberately NOT done
// here — the handler runs them POST-commit, because Stripe calls must never ride
// a DB tx. A created new invoice is carried back on the result for that
// post-commit FinalizeOnCreateInvoice. Caller has validated the swap (active
// sub, plan exists + differs, same-cadence, different interval).
func (s *Service) applyCrossIntervalPlanSwapTx(
	ctx context.Context,
	tx *sql.Tx,
	tenantID, subscriptionID, itemID, newPlanID string,
	sub domain.Subscription,
	newInterval domain.BillingInterval,
	newTiming domain.BillTiming,
	now time.Time,
) (ItemChangeResult, error) {
	// The swap re-anchors the period at `now`. Run on a period that has
	// already ended unbilled, it would skip that period's close entirely
	// (in_advance) or stretch it past its end (in_arrears). The handler
	// closes such a period first (CloseEndedPeriodFirst); this refuses a
	// boundary that passed after that. The period CAS below proves the row
	// still equals sub, so the check holds at the write.
	if domain.BillingDueAt(sub, now) {
		return ItemChangeResult{}, errUnbilledPeriod(sub)
	}
	loc, err := s.tenantLocation(ctx, sub.TenantID)
	if err != nil {
		return ItemChangeResult{}, err
	}
	// Re-anchor the cycle to `now` for the new cadence (ADR-055) — must NOT keep
	// the old interval's anchor day.
	swapAnchorDay := domain.AnchorDayFor(now, sub.BillingTime, newInterval, loc)

	// The period write is the FIRST statement of the swap tx (ADR-115): it
	// row-locks the subscription and proves the snapshot this request was
	// planned from — status, period start, watermark — before the refund
	// drafts, the plan write or the new-period invoice touch anything. A cycle
	// close, a threshold fire or a sibling swap that committed after the Get
	// in UpdateItemTx makes it 0 rows → ErrWatermarkMoved → 409 here, and the
	// handler's deferred rollback has nothing to undo. Holding the row
	// exclusively from the first statement also removes the share→exclusive
	// upgrade the old order had (items → intervalTransition FOR SHARE → the
	// period UPDATE last), which two concurrent swaps on one sub could
	// deadlock on.
	expected := SnapshotOf(sub)
	var newPE time.Time
	if newTiming == domain.BillInAdvance {
		newPE = domain.NextBillingPeriodEnd(now, sub.BillingTime, newInterval, loc, swapAnchorDay)
		// The watermark moves on the SAME tx as the new-period invoice below,
		// so it can never move without the day-1 invoice existing.
		if err := s.store.ClosePeriodTx(ctx, tx, tenantID, subscriptionID, expected, now, newPE, newPE, swapAnchorDay); err != nil {
			return ItemChangeResult{}, mapPeriodMoved(err)
		}
	} else if sub.CurrentBillingPeriodStart != nil {
		// in_arrears: truncate the current period to (oldPS, now); the scheduler
		// closes the partial period under the OLD plan via segment-aware billing.
		if err := s.store.ClosePeriodTx(ctx, tx, tenantID, subscriptionID, expected, *sub.CurrentBillingPeriodStart, now, now, swapAnchorDay); err != nil {
			return ItemChangeResult{}, mapPeriodMoved(err)
		}
	}

	// The refund and the new-period invoice below are priced from sub.Items
	// (#1270). An item change that committed after UpdateItemTx's read did
	// not move the period, so the CAS above passed. Every item writer takes
	// this row first, so with the row held, read the items again and refuse
	// if they are not the ones this swap priced.
	locked, err := s.store.LockForItemChangeTx(ctx, tx, tenantID, subscriptionID)
	if err != nil {
		return ItemChangeResult{}, err
	}
	if !sameItems(locked.Items, sub.Items) {
		return ItemChangeResult{}, errItemChanged()
	}
	// The swap references the item's current plan and the new one. Take
	// both now, before the refund drafts lock invoices and number a credit
	// note: a swap that waited for a plan while holding the tenant's number
	// row could deadlock with a meter edit and an activation (lockPlansTx).
	var currentPlanID string
	if it, ok := findItem(sub.Items, itemID); ok {
		currentPlanID = it.PlanID
	}
	if err := s.store.LockPlansTx(ctx, tx, []string{currentPlanID, newPlanID}); err != nil {
		return ItemChangeResult{}, err
	}

	// Fire-vs-swap residual (ADR-115): a threshold fire that
	// committed after UpdateItemTx read the sub passes the CAS above — it does
	// not move the period — but may have billed usage through an instant AFTER
	// `now`. Re-anchoring the period below that window would re-bill
	// [now, fire end) at the next close, on either cadence. Read the watermark
	// under the row lock we now hold and refuse; never silently move the
	// operator's instant.
	if s.biller != nil && sub.CurrentBillingPeriodStart != nil && sub.CurrentBillingPeriodEnd != nil {
		billedThrough, err := s.biller.ThresholdBilledThroughTx(ctx, tx, sub)
		if err != nil {
			return ItemChangeResult{}, fmt.Errorf("plan-swap threshold watermark: %w", err)
		}
		if billedThrough != nil && billedThrough.After(now) {
			return ItemChangeResult{}, errs.InvalidState("A spend-threshold invoice already billed this subscription past the current time. Schedule the plan change for the next period, or try again later.").WithCode("subscription_period_moved")
		}
	}

	// OLD-period refund, on this same tx (deferred Bug B; ADR-118): computed
	// from the pre-swap `sub` snapshot so the math resolves the OUTGOING plan
	// rate and the old period's funding invoices, paid or unpaid. Any error
	// fails the whole swap — never a swapped sub with a silently-lost refund.
	var swapRelief domain.Relief
	if s.biller != nil {
		r, rerr := s.biller.BillOnPlanSwapDraftsTx(ctx, tx, sub, now)
		if rerr != nil {
			return ItemChangeResult{}, fmt.Errorf("plan-swap refund: %w", rerr)
		}
		swapRelief = r
	}

	// Apply the plan swap on the tx (stamps item.plan_id + plan_changed_at=now).
	updated, err := s.store.ApplyItemPlanImmediatelyTx(ctx, tx, tenantID, itemID, newPlanID, now)
	if err != nil {
		return ItemChangeResult{}, err
	}

	res := ItemChangeResult{
		Item: updated, EffectiveAt: now, OrchestratedCrossAxis: true,
		swapRelief: swapRelief,
	}

	if newTiming == domain.BillInAdvance && s.biller != nil {
		// Build a prospective sub reflecting the swapped plan + new period:
		// s.store.Get would NOT observe the uncommitted tx writes, so derive
		// it in-memory from the pre-swap sub + the swap result.
		prospective := sub
		prospective.CurrentBillingPeriodStart = &now
		prospective.CurrentBillingPeriodEnd = &newPE
		prospective.NextBillingAt = &newPE
		prospective.BillingAnchorDay = swapAnchorDay
		items := make([]domain.SubscriptionItem, len(sub.Items))
		for i, it := range sub.Items {
			if it.ID == itemID {
				items[i] = updated
			} else {
				items[i] = it
			}
		}
		prospective.Items = items

		// The jump to [now, newPE) closes [oldPS, now): the swap invoice bills
		// that window's usage beside the new base. sub is the pre-swap
		// snapshot the CAS and the item check above proved.
		inv, ok, afterCommit, err := s.biller.BillOnPlanSwapTx(ctx, tx, sub, prospective, now)
		if err != nil {
			return ItemChangeResult{}, fmt.Errorf("plan-swap new in_advance bill: %w", err)
		}
		res.swapAfterCommit = afterCommit
		if ok {
			invCopy := inv
			res.crossAxisNewInvoice = &invCopy
		}
	}
	return res, nil
}

// FinalizeCrossIntervalSwap runs the POST-commit external steps for an atomic
// cross-interval swap, called by the handler AFTER the swap tx is durable
// (Stripe calls must never ride a DB tx):
//
//   - OLD-period refund. It was written ON the swap tx by
//     BillOnPlanSwapDraftsTx (ADR-118); here only its post-commit legs run
//     (draft Issue; a void's tax reversal and webhook), each recovered by its
//     own sweep if this fails.
//   - FinalizeOnCreateInvoice (tax commit + auto-charge) for the swap invoice
//     committed inside the swap tx, if one was created.
//   - The swap invoice's afterCommit (counts usage it left unpriced).
func (s *Service) FinalizeCrossIntervalSwap(ctx context.Context, tenantID string, subBefore domain.Subscription, result ItemChangeResult) {
	if result.swapAfterCommit != nil {
		result.swapAfterCommit()
	}
	var inv domain.Invoice
	if result.crossAxisNewInvoice != nil {
		inv = *result.crossAxisNewInvoice
	}
	s.finishReliefThenCollect(ctx, subBefore, result.swapRelief, inv)
}

// finishReliefThenCollect runs the post-commit legs of a committed cancel or
// cross-interval swap, in this order (ADR-118, amended 2026-10-19):
//
//  1. FinishRelief issues the relief's credit notes. A paid share's note puts
//     its credit on the customer's balance.
//  2. FinalizeOnCreateInvoice commits the tax of the invoice the change wrote,
//     if it wrote one, and collects it. Collection applies the balance before
//     it charges the card (ADR-088), so the relief reduces this invoice.
//
// In the other order a card on file was charged the whole final invoice of a
// cancel, and the relief was left on the balance of a customer with no
// subscription. With no card, the no-card email and no-payment dunning went
// out for the whole amount.
//
// If a step fails here, the credit notes, the tax commit and the collection
// are each picked up by their own sweep (RetryPendingClawbackIssue,
// RetryPendingTaxCommit, the auto-charge sweep). The invoice.finalized
// webhook is not: no sweep sends it again.
func (s *Service) finishReliefThenCollect(ctx context.Context, sub domain.Subscription, r domain.Relief, inv domain.Invoice) {
	// The change has committed, so both legs run to the end even if the
	// caller's context ends now: an API request's timeout or a client
	// disconnect, a billing run stopping on shutdown or a lost lease, a
	// dunning tick stopping. Collection already ignores the caller's
	// cancellation (CollectInvoice). If the relief did not, Issue failed on
	// its first database call, and the card was charged the whole final
	// invoice with the credit left on the balance (#1286 review). Each Stripe
	// call is bounded by the client's own 30s timeout, and shutdown's
	// published budget bounds how long the process waits for this.
	ctx = context.WithoutCancel(ctx)
	if s.biller == nil {
		return
	}
	s.biller.FinishRelief(ctx, sub, r)
	if inv.ID != "" {
		s.biller.FinalizeOnCreateInvoice(ctx, sub, inv)
	}
}

// CancelPendingItemChange clears a scheduled plan change on a single item.
// Idempotent — a no-op if nothing was scheduled.
func (s *Service) CancelPendingItemChange(ctx context.Context, tenantID, subscriptionID, itemID string) (domain.SubscriptionItem, error) {
	item, err := s.store.GetItem(ctx, tenantID, itemID)
	if err != nil {
		return domain.SubscriptionItem{}, err
	}
	if item.SubscriptionID != subscriptionID {
		return domain.SubscriptionItem{}, errs.ErrNotFound
	}
	if item.PendingPlanID == "" {
		return item, nil
	}
	ctx = s.bindForSub(ctx, tenantID, subscriptionID)
	// A pending change due at a boundary that has passed has taken effect,
	// even if the billing run has not closed the period yet: the close runs
	// first and applies it, and the cancel is then refused rather than
	// reported as a cancel.
	if _, err := s.CloseEndedPeriodFirst(ctx, tenantID, subscriptionID); err != nil {
		return domain.SubscriptionItem{}, err
	}
	after, err := s.store.GetItem(ctx, tenantID, itemID)
	if err != nil {
		return domain.SubscriptionItem{}, err
	}
	if after.PendingPlanID == "" {
		if after.PlanID == item.PendingPlanID {
			return domain.SubscriptionItem{}, errs.InvalidState("the pending plan change already took effect when the billing period ended")
		}
		return after, nil
	}
	return s.store.ClearItemPendingPlan(ctx, tenantID, itemID)
}

// RemoveItem deletes an item. Removing the only remaining item on an active
// subscription is rejected — a subscription with zero priced lines has no
// valid billing semantics. Callers wanting to end billing altogether should
// Cancel the subscription.
//
// It runs RemoveItemTx in a transaction of its own, for a removal the
// handler does not prorate (the last hours of a period, a subscription that
// is not active), so its checks run under the subscription row lock too:
// two removals of the two items of a subscription cannot both pass the
// last-item check (#1270).
func (s *Service) RemoveItem(ctx context.Context, tenantID, subscriptionID, itemID string) error {
	return s.store.WithTenantTx(ctx, tenantID, func(tx *sql.Tx) error {
		_, err := s.RemoveItemTx(ctx, tx, tenantID, subscriptionID, itemID)
		return err
	})
}

// RemoveItemTx removes an item on the caller's tx: the atomic
// RemoveItem-with-proration flow's, so a failed proration-credit grant
// rolls back the item delete too (ADR-030 atomic-proration follow-through,
// 2026-05-29), or RemoveItem's own.
//
// Its first statement locks the subscription row, and the checks run on the
// state read under that lock (#1270). The result carries that state: Item is
// the item as it was removed, EffectiveAt the removal instant, and the
// handler prices the removal credit from them.
func (s *Service) RemoveItemTx(ctx context.Context, tx *sql.Tx, tenantID, subscriptionID, itemID string) (ItemChangeResult, error) {
	ctx = s.bindForSub(ctx, tenantID, subscriptionID)
	sub, item, err := s.lockItemForChange(ctx, tx, tenantID, subscriptionID, itemID)
	if err != nil {
		return ItemChangeResult{}, err
	}
	if sub.Status == domain.SubscriptionActive && len(sub.Items) <= 1 {
		return ItemChangeResult{}, errs.InvalidState("A subscription needs at least one item. To stop billing, cancel the subscription instead.")
	}
	now := s.clock.Now(ctx)
	// As in changeItemUnderLock: the boundary can pass after the handler's
	// CloseEndedPeriodFirst.
	if sub.Status == domain.SubscriptionActive && domain.BillingDueAt(sub, now) {
		return ItemChangeResult{}, errUnbilledPeriod(sub)
	}
	if err := s.store.RemoveItemTx(ctx, tx, tenantID, itemID); err != nil {
		return ItemChangeResult{}, err
	}
	return ItemChangeResult{Item: item, EffectiveAt: now, pricedFrom: itemChangeBase{sub: sub, item: item}}, nil
}

// cancelActorLabel derives the timeline's "canceled by …" vocabulary from
// the request identity (ADR-090 D16): a dashboard session or API key reads
// as "operator", a stamped customer actor as "customer". Background callers
// name themselves via WithCancelOrigin (the dunning adapter stamps
// "dunning"); an unstamped background cancel is honestly "system".
//
// The SAME function feeds the outbound subscription.canceled webhook
// (postgres.go, enqueued in the cancel tx), so the webhook and the audit
// row can never disagree about who canceled.
func cancelActorLabel(ctx context.Context) string {
	switch actorType, _ := audit.ResolveActor(ctx); actorType {
	case "user", "api_key":
		return "operator"
	case "customer":
		return "customer"
	}
	// Background caller. The machinery that drove the cancel STAMPS its own
	// identity (WithCancelOrigin) rather than us inferring it from the
	// absence of a request actor — an unlabeled background canceller is
	// honestly "system", never a guess at whichever background path happens
	// to exist today.
	if origin := CancelOrigin(ctx); origin != "" {
		return origin
	}
	return "system"
}

type cancelOriginKey struct{}

// WithCancelOrigin stamps the background machinery driving a cancel (e.g.
// "dunning") so both the ADR-090 audit row and the outbound
// subscription.canceled webhook — written in the SAME transaction — name it
// identically. Request-driven cancels need no stamp: ResolveActor already
// answers who they were.
func WithCancelOrigin(ctx context.Context, origin string) context.Context {
	return context.WithValue(ctx, cancelOriginKey{}, origin)
}

// CancelOrigin reads the stamp set by WithCancelOrigin ("" when unset).
func CancelOrigin(ctx context.Context) string {
	origin, _ := ctx.Value(cancelOriginKey{}).(string)
	return origin
}

// FireDueScheduledCancel executes a scheduled cancel whose cancel_at fell
// strictly inside a billing period (ADR-097) — the billing engine detects
// due-ness through the cycle scans' cancel arm and delegates the transition
// here via its ScheduledCancelExecutor seam, keeping the domain boundary:
// the engine knows WHEN, this domain owns HOW.
//
// The composition is Cancel's, with three deltas: the flip rides
// FireScheduledCancellationWithBill (CAS on status AND cancel_at = at, so
// a concurrent unschedule/re-schedule defeats the fire), canceled_at is
// the contracted instant `at` rather than now, and provenance is
// "schedule". Both benign race outcomes (already terminated; schedule
// changed) surface as errs.InvalidState and return nil — whichever writer
// won has left the sub in a state the scans resolve on their own.
func (s *Service) FireDueScheduledCancel(ctx context.Context, tenantID, id string, at time.Time) error {
	ctx = s.bindForSub(ctx, tenantID, id)

	var finalInv domain.Invoice
	afterBillCommit := func() {}
	var relief domain.Relief
	canceled, err := s.store.FireScheduledCancellationWithBill(ctx, tenantID, id, at, func(tx *sql.Tx, c domain.Subscription) error {
		emitAudit := func(r domain.Relief, currency string) error {
			if s.audit == nil {
				return nil
			}
			meta := map[string]any{
				"customer_id": c.CustomerID,
				"plan_ids":    planIDsFromItems(c.Items),
				"canceled_by": "schedule",
			}
			addReliefMeta(meta, r, currency)
			if auditErr := s.audit.LogInTx(ctx, tx, audit.Entry{
				Action:        domain.AuditActionCancel,
				ResourceType:  "subscription",
				ResourceID:    c.ID,
				ResourceLabel: c.Code,
				Metadata:      meta,
			}); auditErr != nil {
				return fmt.Errorf("audit emission: %w", auditErr)
			}
			return nil
		}
		if s.biller == nil {
			return emitAudit(domain.Relief{}, "")
		}
		inv, afterCommit, billErr := s.biller.BillFinalOnImmediateCancelTx(ctx, tx, c)
		if billErr != nil {
			return fmt.Errorf("final-on-scheduled-cancel invoice: %w", billErr)
		}
		finalInv = inv
		afterBillCommit = afterCommit
		r, reliefErr := s.biller.BillOnCancelDraftsTx(ctx, tx, c)
		if reliefErr != nil {
			return fmt.Errorf("scheduled-cancel proration relief: %w", reliefErr)
		}
		relief = r
		return emitAudit(r, inv.Currency)
	})
	if err != nil {
		// Only the fire's own race outcomes are benign. A relief or billing
		// refusal is also an InvalidState, and skipping it silently left the
		// subscription active past its cancel_at; it is returned, so the
		// billing run reports it and saves it as a billing failure (ADR-119).
		if errs.Code(err) == ScheduledCancelSuperseded {
			slog.InfoContext(ctx, "due scheduled cancel: benign race, skipping",
				"subscription_id", id, "tenant_id", tenantID, "reason", err.Error())
			return nil
		}
		return err
	}

	afterBillCommit()

	// Post-commit legs, as in Cancel: the relief first, so its credit reduces
	// the final invoice, then the final invoice's tax commit and collection.
	s.finishReliefThenCollect(ctx, canceled, relief, finalInv)
	return nil
}

// closeEndedPeriods closes the periods of an active subscription that have
// ended and that the billing run has not closed yet. The billing run closes a
// period on its first tick after the boundary (up to an hour today, longer
// after downtime or a failed attempt), and until then the row still describes
// the ended period. A write that lands in that gap would act on the ended
// period, and the close would then read the result: a cancel scheduled "at
// period end" would cancel retroactively at the boundary, a pause or resume
// would decide the ended period's invoice, a plan or quantity change would
// skip its proration. So every subscription write runs this first, and then
// acts on the period that is running, as if the billing run had gone first.
//
// The close is the billing run's own attempt (DueBiller.BillDue): the period
// CAS, catch-up over several periods and the failure record (ADR-119). Its
// error is logged here, not returned: the engine has already logged and saved
// it, and the caller learns the outcome from the returned row (still due or
// not).
//
// grace is the invoice grace the close waits for (ADR-120). The operator's
// writes pass 0: the operator is acting now, so the ended period closes now.
// Dunning's final pause and cancel pass INVOICE_GRACE_PERIOD (amendment
// 2026-10-22): periods past their grace are closed, and a period still
// inside it is left open and answered with *InsideInvoiceGraceError, so the
// action runs when the grace ends and usage arriving inside the grace is on
// the invoice. The close itself honours the grace, and the check after it
// reads the row at the same now, so no period inside its grace is closed
// however the boundary and this call interleave. A test clock has no grace
// (ADR-120): its advance decides when usage arrives.
//
// Trialing subscriptions are left to the trial scans, as Cancel always has
// (ADR-069: a trial cancel is free however late it lands). closed reports
// whether an attempt ran; sub is the row read after it.
func (s *Service) closeEndedPeriods(ctx context.Context, tenantID, id string, grace time.Duration) (sub domain.Subscription, closed bool, err error) {
	sub, err = s.store.Get(ctx, tenantID, id)
	if err != nil {
		return domain.Subscription{}, false, err
	}
	now := s.clock.Now(ctx)
	if s.dueBiller == nil || sub.Status != domain.SubscriptionActive || !domain.BillingDueAt(sub, now) {
		return sub, false, nil
	}
	if sub.TestClockID != "" {
		grace = 0
	}
	// The close honours the grace (BillDue closes no period still inside it),
	// so the one guard is the check after it: whatever is still due at now but
	// not at now less the grace waits. A period inside its grace on the first
	// read is left as it was and answered the same way.
	if _, billErr := s.dueBiller.BillDue(ctx, tenantID, id, grace); billErr != nil {
		slog.WarnContext(ctx, "billing the ended period before a subscription write failed",
			"subscription_id", id, "tenant_id", tenantID, "error", billErr)
	}
	sub, err = s.store.Get(ctx, tenantID, id)
	if err != nil {
		return domain.Subscription{}, true, err
	}
	if insideInvoiceGrace(sub, now, grace) {
		// The periods past their grace are closed (none, or several on a
		// catch-up); the newest waits.
		return sub, true, graceWait(sub, grace)
	}
	return sub, true, nil
}

// insideInvoiceGrace reports whether sub has a due instant that has come at
// now but whose grace has not passed.
func insideInvoiceGrace(sub domain.Subscription, now time.Time, grace time.Duration) bool {
	return grace > 0 && domain.BillingDueAt(sub, now) && !domain.BillingDueAt(sub, now.Add(-grace))
}

func graceWait(sub domain.Subscription, grace time.Duration) *InsideInvoiceGraceError {
	return &InsideInvoiceGraceError{Until: domain.DueInstant(sub).Add(grace).Truncate(time.Microsecond)}
}

// errUnbilledPeriod is the refusal of a write that would act on a period that
// ended and could not be closed. Code unbilled_period, the same answer an
// immediate cancel gives (billing.Engine.billFinalOnImmediateCancelImpl).
func errUnbilledPeriod(sub domain.Subscription) error {
	next := "retry billing for this subscription, then try again"
	if sub.TestClockID != "" {
		next = "finish or retry the clock advance, then try again"
	}
	return errs.InvalidState("this subscription has a billing period that ended and is not invoiced yet; " + next).
		WithCode("unbilled_period")
}

// CloseEndedPeriodFirst is the pre-step every subscription write other than
// Cancel runs before it reads the subscription it plans from (see
// closeEndedPeriods). It returns the row after the close. When the close could
// not bill (billing failing, waiting or parked), the write is refused with
// unbilled_period rather than acting on the ended period; retry-billing is the
// operator's way out. The item handlers call it before their proration
// snapshot, so the snapshot is the new period's.
//
// A subscription with no items is not refused: its close fails for that very
// reason (refusalNoItems: "Add an item, then retry billing"), so refusing the
// add would leave it unbillable for good, and with nothing to bill there is
// nothing on the ended period a write could change.
func (s *Service) CloseEndedPeriodFirst(ctx context.Context, tenantID, id string) (domain.Subscription, error) {
	return s.closeEndedPeriodFirstWithin(ctx, tenantID, id, 0)
}

// closeEndedPeriodFirstWithin is CloseEndedPeriodFirst waiting for grace (see
// closeEndedPeriods). A period inside its grace answers
// *InsideInvoiceGraceError.
func (s *Service) closeEndedPeriodFirstWithin(ctx context.Context, tenantID, id string, grace time.Duration) (domain.Subscription, error) {
	ctx = s.bindForSub(ctx, tenantID, id)
	sub, _, err := s.closeEndedPeriods(ctx, tenantID, id, grace)
	if err != nil {
		return domain.Subscription{}, err
	}
	if sub.Status == domain.SubscriptionActive && len(sub.Items) > 0 && domain.BillingDueAt(sub, s.clock.Now(ctx)) {
		return domain.Subscription{}, errUnbilledPeriod(sub)
	}
	return sub, nil
}

// Cancel returns the canceled subscription, the cents-amount of any
// cancel-proration credit granted (0 when none — in_arrears sub,
// clean cancel, unpaid source invoice), and an error. The cancel's own
// audit row, written in the cancel transaction (ADR-090), carries the
// credit amount so the activity timeline shows "Subscription canceled ·
// Prorated credit $X.XX" (industry standard — Stripe / Lago / Chargebee /
// Orb all surface the credit on the subscription timeline, not just the
// customer's credit balance).
//
// It is the operator's cancel: an ended period is closed now, inside its
// invoice grace or not. Dunning cancels through CancelForDunning.
func (s *Service) Cancel(ctx context.Context, tenantID, id string) (domain.Subscription, int64, error) {
	return s.cancel(ctx, tenantID, id, 0)
}

// CancelForDunning is dunning's cancel final action. It is Cancel, except
// that it does not close a period still inside its invoice grace: it answers
// *InsideInvoiceGraceError and changes nothing, and dunning runs it again
// when the grace ends (ADR-120 amendment 2026-10-22).
func (s *Service) CancelForDunning(ctx context.Context, tenantID, id string) (domain.Subscription, int64, error) {
	return s.cancel(ctx, tenantID, id, s.invoiceGrace)
}

func (s *Service) cancel(ctx context.Context, tenantID, id string, grace time.Duration) (domain.Subscription, int64, error) {
	ctx = s.bindForSub(ctx, tenantID, id)

	// A cancel can arrive after the period ended and before the billing run
	// closed it. No run bills a canceled subscription, so the ended periods
	// are closed first, through the run's own attempt (period CAS, catch-up
	// over several periods, failure record). The cancel below then lands
	// inside the current period, as if the run had gone first.
	//
	// The attempt's error is not returned: the engine has logged and saved
	// it, and the final-invoice step below refuses the cancel (code
	// unbilled_period) for as long as a period is left unbilled.
	sub, closed, err := s.closeEndedPeriods(ctx, tenantID, id, grace)
	if err != nil {
		return domain.Subscription{}, 0, err
	}
	if closed && sub.Status == domain.SubscriptionCanceled {
		// A scheduled cancel that was due fired inside the attempt, at its
		// contracted instant, with its own invoice, audit row and event. The
		// subscription is canceled, which is what was asked for.
		return sub, 0, nil
	}

	// PR-10 / ADR-056: emit the final partial-period invoice (in_arrears
	// prorated base + usage from current_period_start → canceled_at) IN THE SAME
	// tx as the cancel flip, so a billing failure rolls the cancel back rather
	// than leaving a canceled sub with an uninvoiced partial period — a permanent
	// revenue leak (there is no final-on-cancel reconciler; the prior "G3
	// deferred reconciler" referenced here never existed). That includes a
	// period that ended unbilled (canceled_at at/after current_period_end):
	// the biller refuses it. The relief is written after it on the same tx.
	// Neither write applies credits; after commit the relief is issued first
	// and the final invoice collected second, so the relief's balance credit
	// reduces this invoice (finishReliefThenCollect).
	var finalInv domain.Invoice
	afterBillCommit := func() {}
	var relief domain.Relief
	canceled, err := s.store.CancelAtomicWithBill(ctx, tenantID, id, func(tx *sql.Tx, c domain.Subscription) error {
		// ADR-090 in-tx emission runs at the END of this closure (emitCancelAudit)
		// so the atomic-path credit amount can ride the row — but it fires
		// UNCONDITIONALLY: audit coverage must not be conditioned on the
		// unrelated biller dependency (a biller-less construction still
		// flips the sub to canceled).
		emitCancelAudit := func(r domain.Relief, currency string) error {
			if s.audit == nil {
				return nil
			}
			meta := map[string]any{
				"customer_id": c.CustomerID,
				"plan_ids":    planIDsFromItems(c.Items),
				"canceled_by": cancelActorLabel(ctx),
			}
			addReliefMeta(meta, r, currency)
			// No sim field: Cancel's entry points bind the sub's pin onto ctx
			// (bindForSub), and the Logger derives the sim axis from that
			// binding. The hand-rolled pairing this replaces read the clock id
			// off the ENTITY and the instant off clock.Now(ctx) — two sources,
			// so an unbound ctx (dunning's terminal cancel reaches Cancel from
			// its own path) produced "clock C at wall-clock now".
			if auditErr := s.audit.LogInTx(ctx, tx, audit.Entry{
				Action:        domain.AuditActionCancel,
				ResourceType:  "subscription",
				ResourceID:    c.ID,
				ResourceLabel: c.Code,
				Metadata:      meta,
			}); auditErr != nil {
				return fmt.Errorf("audit emission: %w", auditErr)
			}
			return nil
		}
		if s.biller == nil {
			return emitCancelAudit(domain.Relief{}, "")
		}
		inv, afterCommit, billErr := s.biller.BillFinalOnImmediateCancelTx(ctx, tx, c)
		if billErr != nil {
			return fmt.Errorf("final-on-cancel invoice: %w", billErr)
		}
		finalInv = inv
		afterBillCommit = afterCommit
		// Cancel-proration relief (ADR-057, ADR-118): written IN this tx after
		// the final invoice, for every funding invoice, paid or unpaid. Any
		// failure rolls the whole cancel back.
		r, reliefErr := s.biller.BillOnCancelDraftsTx(ctx, tx, c)
		if reliefErr != nil {
			return fmt.Errorf("cancel proration relief: %w", reliefErr)
		}
		relief = r

		return emitCancelAudit(r, inv.Currency)
	})
	if err != nil {
		return domain.Subscription{}, 0, err
	}
	afterBillCommit()

	// Post-commit legs; Stripe calls must never ride a DB tx. The relief and
	// the final invoice are durable and atomic with the cancel. The relief's
	// credit notes are issued first, then the final invoice's tax is
	// committed and it is collected, so the relief's balance credit reduces
	// it (ADR-088; ADR-118 amendment of 2026-10-19).
	s.finishReliefThenCollect(ctx, canceled, relief, finalInv)

	// The credited amount stamps the timeline's "Prorated credit $X.XX" line.
	var prorationCreditCents int64
	if s.biller != nil {
		prorationCreditCents = relief.CreditedCents
	}
	return canceled, prorationCreditCents, nil
}

// RetryBillingResult is what one retry-billing call did.
type RetryBillingResult struct {
	// Subscription is the row re-read after the attempt. Its BillingFailure
	// is set when the attempt failed and the failure was saved.
	Subscription domain.Subscription
	// InvoicesGenerated counts the period-close invoices the attempt created.
	// A scheduled cancel's final invoice and a trial-end first invoice are
	// not counted, so 0 does not mean failure.
	InvoicesGenerated int
	// StillDue is true when, after the attempt, the subscription has no saved
	// failure and is still due: the attempt did not complete and nothing was
	// saved (a lost connection, a deadlock abort, a failed failure-write).
	// The subscription is unbilled; the next billing run attempts it.
	StillDue bool
}

// RetryBilling is the operator's exit from a failing or parked subscription
// (ADR-119): POST /v1/subscriptions/{id}/retry-billing.
//
// It clears the saved failure record and writes the audit row in one
// transaction, then bills the subscription inline with the same attempt the
// billing run makes. Clear first, then attempt, for three reasons: the audit
// row shares a transaction with a real state change; repeated clicks can
// never park a subscription, because each one starts the count again; and the
// attempt needs no special path past the gate.
//
// A subscription with no live record answers InvalidState (409, code
// billing_not_failing) and writes nothing. Once the clear has committed, the
// attempt's outcome is never an error. It is one of three results:
//
//   - Billed, or nothing was left to bill: no BillingFailure, StillDue false.
//   - Failed again, and the failure was saved: BillingFailure is set (attempt
//     1, next attempt in 15 minutes) and the billing run resumes on its own.
//     This includes an attempt that returned cleanly but left the
//     subscription due.
//   - The attempt did not complete and nothing was saved (a lost connection,
//     a deadlock abort, a failure-write that failed): no BillingFailure,
//     StillDue true. The next billing run attempts the subscription.
//
// The third is deliberately not an error. The clear and its audit row are
// committed, so a caller that retried after a 5xx would get 409
// billing_not_failing for a subscription that is still unbilled.
//
// If the process dies after the clear commits and before the bill, the
// subscription is simply due and healthy, and the next billing run bills it.
func (s *Service) RetryBilling(ctx context.Context, tenantID, id string) (RetryBillingResult, error) {
	sub, err := s.store.Get(ctx, tenantID, id)
	if err != nil {
		return RetryBillingResult{}, err
	}
	if sub.TestClockID != "" {
		return RetryBillingResult{}, errs.InvalidState("this subscription is on a test clock; use Retry advance on the clock")
	}

	if _, err := s.store.ClearBillingFailure(ctx, tenantID, id, func(tx *sql.Tx, cleared domain.BillingFailure) error {
		if s.audit == nil {
			return nil
		}
		// ADR-090: the row rides the clear's transaction, so there is never
		// an un-park without its evidence, nor evidence of one that rolled
		// back.
		if auditErr := s.audit.LogInTx(ctx, tx, audit.Entry{
			Action:        domain.AuditActionUpdate,
			ResourceType:  "subscription",
			ResourceID:    sub.ID,
			ResourceLabel: sub.Code,
			Metadata: map[string]any{
				"action":            domain.SubAuditBillingRetried,
				"customer_id":       sub.CustomerID,
				"attempts_before":   cleared.Attempts,
				"was_parked":        cleared.Parked,
				"last_error_before": cleared.LastError,
				"failed_at_before":  cleared.FailedAt.UTC(),
			},
		}); auditErr != nil {
			return fmt.Errorf("audit emission: %w", auditErr)
		}
		return nil
	}); err != nil {
		return RetryBillingResult{}, err
	}

	// After commit, on the request ctx. The engine saves a counted failure and
	// logs every failure, including its own reads. The error is not returned
	// to the operator (see above); the outcome is read off the re-read row.
	var res RetryBillingResult
	var billErr error
	if s.dueBiller != nil {
		res.InvoicesGenerated, billErr = s.dueBiller.BillDue(ctx, tenantID, id, 0)
	}

	res.Subscription, err = s.store.Get(ctx, tenantID, id)
	if err != nil {
		return RetryBillingResult{}, err
	}
	res.StillDue = res.Subscription.BillingFailure == nil && domain.BillingDueAt(res.Subscription, s.clock.Now(ctx))
	if billErr != nil && res.Subscription.BillingFailure == nil {
		// The one outcome with no saved record to show for it.
		slog.Warn("retry-billing: the attempt failed and no failure was saved; the next billing run attempts the subscription",
			"subscription_id", id, "tenant_id", tenantID, "still_due", res.StillDue, "error", billErr)
	}
	return res, nil
}

// ScheduleCancelInput carries the soft-cancel intent. Exactly one of
// AtPeriodEnd or CancelAt must be set on a single call. AtPeriodEnd is
// STATUS-POLYMORPHIC (ADR-069, Stripe semantics): on an ACTIVE sub it
// defers the cancel to current_billing_period_end; on a TRIALING sub it
// means cancel FREE at trial_end_at — the trial scans and the engine route
// it through the dedicated no-invoice transition, and ExtendTrial moves it
// with the trial (the reason it stays a flag rather than eagerly converting
// to a pinned cancel_at). CancelAt is an explicit timestamp the cycle scan
// compares against effectiveNow. The mutually-exclusive split forces
// unambiguous caller intent — Stripe's update endpoint accepts both fields
// together but the resulting precedence is surprising; rejecting the
// combination here keeps the API obvious. The derived read-only
// cancel_effective_at on the subscription answers "when does it actually
// cancel" for consumers.
type ScheduleCancelInput struct {
	AtPeriodEnd bool       `json:"at_period_end,omitempty"`
	CancelAt    *time.Time `json:"cancel_at,omitempty"`
}

// ScheduleCancel persists the soft-cancel intent. For ACTIVE subs,
// CancelAt must be >= current_billing_period_end so the period bills
// normally and the cancel lands on a clean cycle boundary (the
// shorten-current-period + proration variant needs the proration generator
// wired into the engine cancel path). For TRIALING subs (ADR-069), exactly
// two CancelAt values are honorable: == trial_end_at (free cancel via the
// trial-end guard) or >= current_billing_period_end (activate, bill period
// 1, cancel at its close); the open interval between them is rejected —
// no machinery honors it.
//
// Re-scheduling is idempotent: a second call with the same intent leaves
// the row unchanged but for updated_at. Toggling between modes (e.g.
// AtPeriodEnd → CancelAt) is allowed because each call is a full
// replacement of the row's schedule fields.
func (s *Service) ScheduleCancel(ctx context.Context, tenantID, id string, input ScheduleCancelInput) (domain.Subscription, error) {
	if !input.AtPeriodEnd && input.CancelAt == nil {
		return domain.Subscription{}, errs.Invalid("body", "one of at_period_end or cancel_at must be set")
	}
	if input.AtPeriodEnd && input.CancelAt != nil {
		return domain.Subscription{}, errs.Invalid("body", "at_period_end and cancel_at cannot be set together; pick one")
	}

	// "Period end" is the end of the period that is running. In the gap
	// after a boundary the row still holds the ended period, and a flag set
	// there would cancel retroactively at that boundary.
	sub, err := s.CloseEndedPeriodFirst(ctx, tenantID, id)
	if err != nil {
		return domain.Subscription{}, err
	}

	// Future-check the operator-supplied cancel_at against simulated
	// time on clock-pinned subs. Without this, an operator who set
	// cancel_at = "1 month from frozen_now" would be rejected as past
	// because wall-clock has drifted ahead of frozen_time.
	ctx = s.bindForSub(ctx, tenantID, id)
	now := s.clock.Now(ctx)
	var cancelAt *time.Time
	if input.CancelAt != nil {
		ts := input.CancelAt.UTC()
		if !ts.After(now) {
			return domain.Subscription{}, errs.Invalid("cancel_at", "must be in the future")
		}
		// Boundary constraint (ADR-069). For a TRIALING sub exactly two
		// values are honorable: trial_end_at (free cancel — the trial-end
		// guard fires) or on/after current_billing_period_end (activate,
		// bill period 1, cancel at its close). The open interval between
		// them is accepted by no machinery — the guard wouldn't fire and
		// the cycle only cancels at boundaries — so a customer would prepay
		// a FULL period against an echoed-in-GET date nothing honors.
		// Active subs keep the existing on-or-after-period-end rule.
		if sub.Status == domain.SubscriptionTrialing && sub.TrialEndAt != nil && ts.Equal(*sub.TrialEndAt) {
			// Free cancel at trial end — allowed.
		} else if sub.CurrentBillingPeriodEnd != nil && ts.Before(*sub.CurrentBillingPeriodEnd) {
			if sub.Status == domain.SubscriptionTrialing && sub.TrialEndAt != nil {
				return domain.Subscription{}, errs.Invalid("cancel_at", fmt.Sprintf(
					"for a trialing subscription cancel_at must be the trial end (%s) or on/after current_billing_period_end (%s) — dates in between are honored by neither boundary",
					sub.TrialEndAt.UTC().Format(time.RFC3339), sub.CurrentBillingPeriodEnd.UTC().Format(time.RFC3339)))
			}
			// The message must name the ACTUAL constraint: only the period
			// already in progress is closed. A cancel_at landing mid-period in
			// a LATER period is accepted and prorates correctly — ADR-097
			// shipped that firing path, and FLOW TC8 walks it (cancel_at on the
			// 16th of a future month produces a final invoice for
			// [period_start, cancel_at] at 15/30 of the base). The previous
			// wording, "mid-period cancel with proration is not yet supported",
			// described a capability gap that no longer exists and sent
			// operators looking for a workaround they don't need.
			return domain.Subscription{}, errs.Invalid("cancel_at", fmt.Sprintf(
				"must be on or after current_billing_period_end (%s) — the period already in progress can't be shortened; a date inside a later period is fine and bills that period prorated up to it",
				sub.CurrentBillingPeriodEnd.UTC().Format(time.RFC3339)))
		}
		cancelAt = &ts
	}

	return s.store.ScheduleCancellation(ctx, tenantID, id, cancelAt, input.AtPeriodEnd, sub.Status)
}

// ClearScheduledCancel undoes any prior schedule. Idempotent — a row
// without a schedule returns unchanged.
//
// A schedule whose boundary has passed has already taken effect, even if the
// billing run has not closed the period yet: the close runs first and fires
// it, and the clear is then refused rather than reported as a clear.
func (s *Service) ClearScheduledCancel(ctx context.Context, tenantID, id string) (domain.Subscription, error) {
	ctx = s.bindForSub(ctx, tenantID, id)
	before, err := s.store.Get(ctx, tenantID, id)
	if err != nil {
		return domain.Subscription{}, err
	}
	after, err := s.CloseEndedPeriodFirst(ctx, tenantID, id)
	if err != nil {
		return domain.Subscription{}, err
	}
	if before.Status == domain.SubscriptionActive && after.Status == domain.SubscriptionCanceled {
		return domain.Subscription{}, errs.InvalidState("the scheduled cancel already took effect when the billing period ended; the subscription is canceled")
	}
	return s.store.ClearScheduledCancellation(ctx, tenantID, id)
}

// PauseCollectionInput carries the collection-pause intent. Behavior is
// required and must be one of the supported modes (v1: keep_as_draft).
// ResumesAt is optional; when set, the cycle scan auto-clears the pause
// at the start of the period containing or after that timestamp so that
// period bills normally. When nil, only an explicit DELETE clears it.
type PauseCollectionInput struct {
	Behavior  domain.PauseCollectionBehavior `json:"behavior"`
	ResumesAt *time.Time                     `json:"resumes_at,omitempty"`
}

// PauseCollection sets the operator's Stripe-parity collection-pause state.
// Distinct from Pause (hard freeze on status). The cycle keeps advancing;
// while pause_collection is non-null the engine generates each new invoice
// as a draft, so it is not finalized, not charged and starts no dunning run.
// The pause is the operator's (set_by=operator), also when it replaces a
// dunning pause, so a payment no longer lifts it. Dunning pauses through
// PauseCollectionForDunning.
//
// It does not stop collection on invoices finalized before the pause. A
// dunning run already open on one keeps its retry schedule and charges the
// customer's default card, as Stripe keeps retrying invoices created before a
// pause unless they are voided. Only the charge sweeps (the queued auto-charge
// and the charge when a card is added) skip a paused subscription. To stop an
// open run, void the invoice or mark it uncollectible (ADR-036, amended
// 2026-10-22).
//
// Idempotent: a second call with the same input replaces the row's
// pause_collection_* columns with the same values. Switching from one
// resumes_at to another is supported because each call is a full
// replacement.
func (s *Service) PauseCollection(ctx context.Context, tenantID, id string, input PauseCollectionInput) (domain.Subscription, error) {
	if input.Behavior == "" {
		return domain.Subscription{}, errs.Invalid("behavior", "behavior is required")
	}
	if input.Behavior != domain.PauseCollectionKeepAsDraft {
		// The old message claimed uncollectible "does not yet exist" —
		// false since ADR-064 shipped mark-uncollectible as a live endpoint
		// AND a dunning final action; the API was lying about the product
		// to exactly the API-first engineers evaluating it (2026-07-06
		// truth pass). The honest reason the other behaviors are deferred:
		// pause-time auto-void / auto-mark-uncollectible of each cycle's
		// draft touches five invoice-writer sites and no design partner has
		// asked for Stripe's void/mark_uncollectible pause flavors yet.
		return domain.Subscription{}, errs.Invalid("behavior",
			"only 'keep_as_draft' is supported today — paused cycles keep drafting invoices without collecting; the 'void' and 'mark_uncollectible' pause behaviors (auto-resolving each cycle's draft) are not implemented yet")
	}

	// Bind to the sub pin upfront so the store's sub.UpdatedAt stamp +
	// any downstream clock.Now reads honor simulated time on
	// clock-pinned subs. Pre-fix the bind was only inside the
	// `if input.ResumesAt != nil` branch, so the common no-resumes_at
	// path stamped wall-clock UpdatedAt — which then propagated through
	// auditCtxForSub to wall-clock audit rows.
	ctx = s.bindForSub(ctx, tenantID, id)

	// The ended period's invoice follows the pause state at its end, so a
	// pause landing after the boundary must not turn it into a draft.
	if _, err := s.CloseEndedPeriodFirst(ctx, tenantID, id); err != nil {
		return domain.Subscription{}, err
	}

	pc := domain.PauseCollection{Behavior: input.Behavior}
	if input.ResumesAt != nil {
		ts := input.ResumesAt.UTC()
		if !ts.After(s.clock.Now(ctx)) {
			return domain.Subscription{}, errs.Invalid("resumes_at", "must be in the future")
		}
		pc.ResumesAt = &ts
	}

	sub, err := s.store.SetPauseCollection(ctx, tenantID, id, pc)
	if err != nil {
		return domain.Subscription{}, err
	}

	if s.audit != nil {
		meta := map[string]any{
			"action":       domain.SubAuditCollectionPaused,
			"customer_id":  sub.CustomerID,
			"behavior":     string(input.Behavior),
			"triggered_by": "operator",
		}
		if sub.PauseCollection != nil && sub.PauseCollection.ResumesAt != nil {
			meta["resumes_at"] = sub.PauseCollection.ResumesAt.UTC()
		}
		_ = s.audit.Log(ctx, tenantID, domain.AuditActionUpdate, "subscription", sub.ID, sub.Code, meta)
	}

	return sub, nil
}

// PauseCollectionForDunning is dunning's pause final action for the unpaid
// invoice invoiceID. applied reports whether collection is paused when it
// returns: true when this call paused it, and when a pause was already in
// force (the operator's, which it leaves as it is, this run's earlier
// attempt, or another invoice's dunning pause). It is false when the invoice was paid before the pause could
// land; nothing is paused then. The "Collection paused" row is written only
// when this call paused collection, so a re-attempted exhaustion writes no
// second row.
//
// The pause lifts when invoiceID is paid (ADR-036 amendment 2026-10-22,
// PostgresStore.ReleaseDunningPauseTx).
func (s *Service) PauseCollectionForDunning(ctx context.Context, tenantID, id, invoiceID string) (bool, error) {
	ctx = s.bindForSub(ctx, tenantID, id)
	// The ended period's invoice follows the pause state at its end, as
	// for an operator's pause. Unlike the operator's, this close waits for
	// the invoice grace: inside it nothing is paused, the answer is
	// *InsideInvoiceGraceError, and dunning pauses when the grace ends
	// (ADR-120 amendment 2026-10-22).
	if _, err := s.closeEndedPeriodFirstWithin(ctx, tenantID, id, s.invoiceGrace); err != nil {
		return false, err
	}
	sub, outcome, err := s.store.SetDunningPause(ctx, tenantID, id, invoiceID)
	if err != nil {
		return false, err
	}
	switch outcome {
	case DunningPauseInvoiceSettled:
		return false, nil
	case DunningPauseAlreadyHeld:
		return true, nil
	}
	if s.audit != nil {
		_ = s.audit.Log(ctx, tenantID, domain.AuditActionUpdate, "subscription", sub.ID, sub.Code, map[string]any{
			"action":       domain.SubAuditCollectionPaused,
			"customer_id":  sub.CustomerID,
			"behavior":     string(domain.PauseCollectionKeepAsDraft),
			"triggered_by": "dunning",
			"invoice_id":   invoiceID,
		})
	}
	return true, nil
}

// ResumeCollection clears any active collection-pause. Idempotent — a row
// without an active pause returns unchanged. Distinct from Resume (which
// flips status from paused back to active).
//
// Its one caller is the operator (handler). The scheduled resumers write
// their own row (triggered_by "schedule", or "payment" for a dunning pause
// a payment released) and do not call this method.
func (s *Service) ResumeCollection(ctx context.Context, tenantID, id string) (domain.Subscription, error) {
	ctx = s.bindForSub(ctx, tenantID, id)
	// A period that ended while paused keeps a draft invoice; the resume
	// applies from the period that is running.
	if _, err := s.CloseEndedPeriodFirst(ctx, tenantID, id); err != nil {
		return domain.Subscription{}, err
	}
	sub, err := s.store.ClearPauseCollection(ctx, tenantID, id)
	if errors.Is(err, ErrNotPaused) {
		// Idempotent for the operator: nothing changed, nothing announced.
		return s.store.Get(ctx, tenantID, id)
	}
	if err != nil {
		return domain.Subscription{}, err
	}
	if s.audit != nil {
		_ = s.audit.Log(ctx, tenantID, domain.AuditActionUpdate, "subscription", sub.ID, sub.Code, map[string]any{
			"action":       domain.SubAuditCollectionResumed,
			"customer_id":  sub.CustomerID,
			"triggered_by": "operator",
		})
	}
	return sub, nil
}

// EndTrial transitions a 'trialing' subscription to 'active' immediately,
// regardless of trial_end_at. Operator-driven counterpart to the cycle-
// scan auto-flip — used when the customer wants to start paying ahead of
// the trial schedule, or the operator is upgrading them off a free trial
// after a sales call. Idempotent at the SQL level (the store atomic
// returns errs.InvalidState if the row is already active or terminal).
func (s *Service) EndTrial(ctx context.Context, tenantID, id string) (domain.Subscription, error) {
	ctx = s.bindForSub(ctx, tenantID, id)
	now := s.clock.Now(ctx)

	// Read sub for billing_time + status validation. Early-end resets
	// the period anchor so billing starts immediately (Stripe parity);
	// the engine-auto-flip path (billOnePeriod calling ActivateAfterTrial
	// at cycle close) is the other transition path and leaves periods
	// alone because they were already advanced to the cycle boundary.
	sub, err := s.store.Get(ctx, tenantID, id)
	if err != nil {
		return domain.Subscription{}, err
	}
	if sub.Status != domain.SubscriptionTrialing {
		return domain.Subscription{}, errs.InvalidState(fmt.Sprintf("Only a trialing subscription's trial can be ended. This one is %s.", sub.Status))
	}
	// A pending cancel schedule blocks the early end (ADR-069, no silent
	// precedence): ending the trial would activate + bill period 1 while the
	// schedule silently mutates into a paid-period cancel — the customer
	// pays a full period despite having canceled first. This service check
	// is the fast operator feedback; the store UPDATE's schedule-empty
	// predicate is the atomic backstop for a schedule racing this read.
	if sub.CancelAtPeriodEnd || sub.CancelAt != nil {
		return domain.Subscription{}, errs.InvalidState("This subscription is scheduled to cancel. Undo the scheduled cancellation first, then end the trial.")
	}

	loc, err := s.tenantLocation(ctx, sub.TenantID)
	if err != nil {
		return domain.Subscription{}, err
	}
	interval := s.firstPlanInterval(ctx, tenantID, sub.Items)
	ps, pe, anchorDay := firstPeriodForActivate(now, sub.BillingTime, interval, loc)

	// ADR-031 / ADR-056: in_advance items get a day-1 invoice covering the first
	// paid period (now → period_end). Build it IN THE SAME tx as the
	// trialing→active flip so a billing failure rolls the early-end back rather
	// than leaving an active sub with no first-period invoice (the cycle
	// scheduler skips the just-elapsed in_advance segment — it is NOT "deferred
	// to next cycle close", that was a revenue leak). External steps (tax commit
	// + auto-charge) run post-commit via FinalizeOnCreateInvoice.
	var firstInv domain.Invoice
	var haveInv bool
	activated, err := s.store.EndTrialEarlyWithBill(ctx, tenantID, id, now, ps, pe, pe, anchorDay, func(tx *sql.Tx, a domain.Subscription) error {
		if s.biller == nil {
			return nil
		}
		inv, ok, billErr := s.biller.BillOnCreateTx(ctx, tx, a, now)
		if billErr != nil {
			return fmt.Errorf("first-invoice-on-trial-end: %w", billErr)
		}
		firstInv, haveInv = inv, ok
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrTrialCancelDue) {
			// The SQL backstop caught a schedule that raced past the check
			// above — same operator answer.
			return domain.Subscription{}, errs.InvalidState("This subscription is scheduled to cancel. Undo the scheduled cancellation first, then end the trial.")
		}
		return domain.Subscription{}, err
	}
	if haveInv {
		s.biller.FinalizeOnCreateInvoice(ctx, activated, firstInv)
	}

	return activated, nil
}

// trialCancelDue reports whether the sub's schedule means "cancel at trial
// end" (ADR-069): the flag on a trialing sub, or an explicit cancel_at at or
// before trial_end.
func trialCancelDue(sub domain.Subscription) bool {
	if sub.TrialEndAt == nil {
		return false
	}
	return sub.CancelAtPeriodEnd || (sub.CancelAt != nil && !sub.CancelAt.After(*sub.TrialEndAt))
}

// cancelTrialAtEnd routes one expired trialing sub with a due schedule
// through the dedicated free cancel transition and fires the terminal
// emissions EXACTLY ONCE — from the CAS winner (ADR-069). All service-side
// activation writers share THIS helper so the emissions cannot drift
// (single-writer convention); the engine's trial branch is the one other
// writer and reuses its scheduled-cancel vocabulary.
//
// Deliberately NO subscription.trial_ended (consumers read that as "billing
// begins" — provisioning a canceled sub), NO invoice (trials are free), and
// canceled_at = trial_end_at (stamped in the store transition, never the
// observing site's possibly-late now). A CAS conflict (rescinded schedule /
// extended trial / already canceled) is a silent no-op: the sub is NOT
// handled this pass and the next scan re-routes whatever state it finds.
// Returns handled=true only when THIS call performed the cancel.
func (s *Service) cancelTrialAtEnd(ctx context.Context, tenantID string, sub domain.Subscription) (bool, error) {
	if sub.TrialEndAt == nil {
		return false, nil
	}
	canceled, err := s.store.CancelAtTrialEnd(ctx, tenantID, sub.ID, *sub.TrialEndAt)
	if errors.Is(err, ErrTrialCancelConflict) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	meta := map[string]any{
		"action":       domain.SubAuditTrialEndCancel,
		"customer_id":  canceled.CustomerID,
		"triggered_by": "schedule",
		"canceled_at":  canceled.CanceledAt.UTC().Format(time.RFC3339),
	}
	// The trial-end instant is its OWN fact, under its own key. The row's
	// sim_effective_at is a separate column the audit writer owns from the
	// ctx binding (simColumns); the two used to collide on one key, with the
	// writer silently winning. The catchup binds ctx to trial_end_at before
	// calling this (B12), so on a test clock both now carry the same instant.
	if sub.TrialEndAt != nil {
		meta["trial_end_at"] = sub.TrialEndAt.UTC().Format(time.RFC3339)
	}
	if s.audit != nil {
		_ = s.audit.Log(ctx, tenantID, domain.AuditActionCancel, "subscription", canceled.ID, canceled.Code, meta)
	}
	// subscription.canceled (schedule + trial_end_cancel) is enqueued IN the
	// CancelAtTrialEnd tx (store-level DispatchTx subset) — not re-fired here.
	slog.Info("trial ended with a due cancel schedule — canceled free of charge",
		"subscription_id", canceled.ID, "tenant_id", tenantID)
	return true, nil
}

// ProcessExpiredTrialsForClock is the catchup Phase 0.5 entry point —
// scans subs pinned to this clock whose trial has elapsed in sim time
// and flips each from 'trialing' to 'active' at trial_end_at (not at
// the later cycle close). Bug #8: without this, status stays
// 'trialing' for the gap between trial_end_at and the first
// chargeable cycle close (up to ~30 days for calendar billing).
//
// Per sub: ActivateAfterTrialWithBill(at=trial_end_at) does the status
// flip + activated_at stamp and bills in_advance items' first paid
// period at trial_end_at in the same tx (ADR-056; Bug #6 coverage
// carries through). A sub whose cancel schedule is due at trial end is
// canceled free instead (ADR-069, cancelTrialAtEnd). Failures collected
// but don't abort the batch — one bad sub doesn't stall the others.
// Returns (processed_count, errors).
//
// Subscriptions matched by the scan but already-EndTrial'd by an
// operator race will land in ActivateAfterTrialWithBill's InvalidState
// branch — treated as a no-op (already correct state).
func (s *Service) ProcessExpiredTrialsForClock(ctx context.Context, tenantID, clockID string, frozen time.Time) (int, []error) {
	expired, err := s.store.ListExpiredTrialsForClock(ctx, tenantID, clockID, frozen, 100)
	if err != nil {
		return 0, []error{fmt.Errorf("list expired trials: %w", err)}
	}

	var (
		processed int
		batchErrs []error
	)
	for _, sub := range expired {
		if sub.TrialEndAt == nil {
			continue
		}
		trialEndAt := *sub.TrialEndAt
		// The trial ends at trial_end_at, not wherever the advance landed
		// (B12): override the INSTANT while keeping the CLOCK, as the pause
		// resume below does. Every branch runs on this ctx: the flip's audit
		// row, the day-1 invoice (issued at trial_end_at, see
		// buildOnCreateInvoice), its finalize row and collection, and the
		// free cancel's row. Nested binds of the same clock inherit it
		// (clock.BindEffectiveNow).
		bound := clock.WithSim(s.bindForSub(ctx, tenantID, sub.ID), clock.Sim{At: trialEndAt, TestClockID: clockID})
		// ADR-069 routing: a due cancel schedule means CANCEL FREE, never
		// activate-and-bill. The SQL guard on the activation UPDATE is the
		// atomic backstop for a schedule landing after this snapshot.
		if trialCancelDue(sub) {
			handled, cErr := s.cancelTrialAtEnd(bound, tenantID, sub)
			if cErr != nil {
				batchErrs = append(batchErrs, fmt.Errorf("trial-end cancel sub %s: %w", sub.ID, cErr))
			} else if handled {
				processed++
			}
			continue
		}
		// ADR-056 alignment (panel finding): the day-1 in_advance invoice now
		// rides the SAME tx as the flip on this path too — the old post-flip
		// BillOnCreate's "will be deferred" WARN was false (the cycle
		// scheduler skips the just-elapsed segment; the fee was simply lost).
		var firstInv domain.Invoice
		var haveInv bool
		activated, err := s.store.ActivateAfterTrialWithBill(bound, tenantID, sub.ID, trialEndAt, func(tx *sql.Tx, a domain.Subscription) error {
			if s.biller == nil {
				return nil
			}
			inv, ok, billErr := s.biller.BillOnCreateTx(bound, tx, a, trialEndAt)
			if billErr != nil {
				return fmt.Errorf("first-invoice-on-trial-expiry: %w", billErr)
			}
			firstInv, haveInv = inv, ok
			return nil
		})
		if err != nil {
			if errors.Is(err, ErrTrialCancelDue) {
				// A cancel schedule committed between the snapshot and the
				// flip — the SQL guard blocked the activation. Route it.
				fresh, gErr := s.store.Get(bound, tenantID, sub.ID)
				if gErr == nil {
					if handled, cErr := s.cancelTrialAtEnd(bound, tenantID, fresh); cErr != nil {
						batchErrs = append(batchErrs, fmt.Errorf("trial-end cancel sub %s: %w", sub.ID, cErr))
					} else if handled {
						processed++
					}
				}
				continue
			}
			// Operator-EndTrial race: the row already left 'trialing'
			// between the scan SELECT and the UPDATE. Not an error —
			// just a no-op for this pass.
			if errors.Is(err, errs.ErrInvalidState) {
				continue
			}
			batchErrs = append(batchErrs, fmt.Errorf("activate sub %s: %w", sub.ID, err))
			continue
		}

		// Audit the auto-flip — the operator EndTrial path writes the same
		// trial_ended row; without this, scheduler-driven expiry showed only
		// in webhooks and the Activity feed missed it (same asymmetry the
		// scheduled-cancel + pause-auto-resume audits already fixed).
		if s.audit != nil {
			// trial_end_at is its own fact, on every trial-end writer; the
			// row's sim axis (test_clock_id + sim_effective_at) comes from the
			// ctx binding on `bound`, which the audit writer owns and which
			// is trial_end_at here.
			_ = s.audit.Log(bound, tenantID, domain.AuditActionUpdate, "subscription", activated.ID, activated.Code, map[string]any{
				"action":       domain.SubAuditTrialEnded,
				"customer_id":  activated.CustomerID,
				"triggered_by": "schedule",
				"trial_end_at": trialEndAt.UTC().Format(time.RFC3339),
			})
		}

		// Post-commit external finalize for the day-1 invoice (tax commit +
		// auto-charge) — Stripe calls never ride a DB tx; the invoice is
		// already durable + atomic with the activation.
		if haveInv {
			s.biller.FinalizeOnCreateInvoice(bound, activated, firstInv)
		}
		// subscription.trial_ended (triggered_by=schedule) is enqueued IN
		// the ActivateAfterTrial tx (store-level DispatchTx subset).
		processed++
	}
	return processed, batchErrs
}

// ProcessExpiredTrials is the wall-clock cron counterpart to
// ProcessExpiredTrialsForClock — scans non-clock-pinned trialing
// subs whose `trial_end_at` has elapsed in wall-clock time and
// flips each to active at trial_end_at. Same shape as the catchup
// Phase 0.5: ActivateAfterTrial (atomic flip + activated_at) +
// BillOnCreate (in_advance first-paid-period coverage).
//
// Livemode partition comes from ctx (the scheduler fans out per
// mode). Per-row errors are collected but don't abort the batch
// — same shape as every other scheduler-tick processor.
//
// ADR-028 disjoint flows: the store query EXPLICITLY EXCLUDES
// clock-pinned rows (`test_clock_id IS NULL` filter). Those
// flow through the catchup orchestrator's Phase 0.5 instead.
func (s *Service) ProcessExpiredTrials(ctx context.Context, batch int) (int, []error) {
	if batch <= 0 {
		batch = 100
	}
	livemode := postgres.Livemode(ctx)
	now := s.clock.Now(ctx)
	expired, err := s.store.ListExpiredTrials(ctx, now, livemode, batch)
	if err != nil {
		return 0, []error{fmt.Errorf("list expired trials: %w", err)}
	}

	var (
		processed int
		batchErrs []error
	)
	for _, sub := range expired {
		if sub.TrialEndAt == nil {
			continue
		}
		trialEndAt := *sub.TrialEndAt
		// ADR-069 routing: a due cancel schedule cancels FREE — see the
		// clock-path twin for the rationale; the activation UPDATE's SQL
		// guard is the atomic backstop.
		if trialCancelDue(sub) {
			handled, cErr := s.cancelTrialAtEnd(ctx, sub.TenantID, sub)
			if cErr != nil {
				batchErrs = append(batchErrs, fmt.Errorf("trial-end cancel sub %s: %w", sub.ID, cErr))
			} else if handled {
				processed++
			}
			continue
		}
		// ADR-056: build the day-1 in_advance invoice IN THE SAME tx as the
		// trialing→active flip, so a billing failure rolls the activation back
		// (the sub stays trialing; this scan retries it next tick unless the
		// billing run has saved a failure for it, ADR-119) rather than leaving
		// an active sub with no first-period invoice — a revenue leak (the cycle
		// scheduler skips the just-elapsed in_advance segment).
		var firstInv domain.Invoice
		var haveInv bool
		activated, err := s.store.ActivateAfterTrialWithBill(ctx, sub.TenantID, sub.ID, trialEndAt, func(tx *sql.Tx, a domain.Subscription) error {
			if s.biller == nil {
				return nil
			}
			inv, ok, billErr := s.biller.BillOnCreateTx(ctx, tx, a, trialEndAt)
			if billErr != nil {
				return fmt.Errorf("first-invoice-on-trial-expiry: %w", billErr)
			}
			firstInv, haveInv = inv, ok
			return nil
		})
		if err != nil {
			if errors.Is(err, ErrTrialCancelDue) {
				// Schedule committed after the snapshot — the SQL guard
				// blocked the activation. Route to the free cancel.
				fresh, gErr := s.store.Get(ctx, sub.TenantID, sub.ID)
				if gErr == nil {
					if handled, cErr := s.cancelTrialAtEnd(ctx, sub.TenantID, fresh); cErr != nil {
						batchErrs = append(batchErrs, fmt.Errorf("trial-end cancel sub %s: %w", sub.ID, cErr))
					} else if handled {
						processed++
					}
				}
				continue
			}
			if errors.Is(err, errs.ErrInvalidState) {
				// Operator-EndTrial race (or already-active by some
				// other path) — desired state reached, skip.
				continue
			}
			batchErrs = append(batchErrs, fmt.Errorf("activate sub %s: %w", sub.ID, err))
			continue
		}

		// Audit the auto-flip (wall-clock counterpart of the catchup path's
		// trial_ended row — no sim metadata, this scan excludes clock-pinned subs).
		if s.audit != nil {
			_ = s.audit.Log(ctx, activated.TenantID, domain.AuditActionUpdate, "subscription", activated.ID, activated.Code, map[string]any{
				"action":       domain.SubAuditTrialEnded,
				"customer_id":  activated.CustomerID,
				"triggered_by": "schedule",
				"trial_end_at": trialEndAt.UTC().Format(time.RFC3339),
			})
		}

		// Post-commit external finalize for the day-1 invoice (tax commit +
		// auto-charge) — Stripe calls must never ride a DB tx; the invoice is now
		// durable + atomic with the activation.
		if haveInv {
			s.biller.FinalizeOnCreateInvoice(ctx, activated, firstInv)
		}
		// subscription.trial_ended (triggered_by=schedule) is enqueued IN
		// the ActivateAfterTrial tx (store-level DispatchTx subset).
		processed++
	}
	return processed, batchErrs
}

// ProcessExpiredPauseCollectionsForClock is the catchup-orchestrator
// entry point that scans clock-pinned subs whose
// pause_collection_resumes_at has passed in simulated time, clears the
// pause, writes audit (triggered_by=schedule), and dispatches
// `subscription.collection_resumed`. Runs as a catchup phase AFTER
// cycle billing, and the store query skips a sub that still owes a close
// before resumes_at: that close bills the pause as it stood at the
// period's end, so the pause must still be on the row when it runs.
//
// Stripe-parity rationale: Stripe resumes collection AT resumes_at,
// not at the next cycle close. The pre-fix gate inside billOnePeriod
// only fired when a cycle was due, so a sub whose resumes_at had
// passed but whose next_billing_at was still in the future stayed
// paused indefinitely — first surfaced by an operator running a test
// clock past resumes_at and observing the pause hadn't lifted.
func (s *Service) ProcessExpiredPauseCollectionsForClock(ctx context.Context, tenantID, clockID string, frozen time.Time) (int, []error) {
	expired, err := s.store.ListExpiredPauseCollectionsForClock(ctx, tenantID, clockID, frozen, 100)
	if err != nil {
		return 0, []error{fmt.Errorf("list expired pause collections: %w", err)}
	}
	var (
		processed int
		batchErrs []error
	)
	for _, sub := range expired {
		// The resume's instant is the CONTRACTED resumes_at, not wherever
		// the advance happened to land (same split as trial flips, boundary
		// cancels, and ADR-097: contractual facts stamp their promised
		// instant). Pre-fix a Sep 4 auto-resume swept by an advance to
		// Oct 2 stamped resumed_at/sim_effective_at = Oct 2 — an event
		// consumer asking "when did collection resume" got a date up to
		// the whole advance-span late. Fallback to frozen is defensive
		// (the scan only returns rows with a due resumes_at).
		resumeAt := frozen
		if sub.PauseCollection != nil && sub.PauseCollection.ResumesAt != nil {
			resumeAt = *sub.PauseCollection.ResumesAt
		}
		// Override the INSTANT while keeping the CLOCK: WithEffectiveNow
		// deliberately clears the clock id (it means "time from no clock I
		// can name"), which stripped sim_effective_at/test_clock_id off the
		// resume's audit row and cost it the test-clock chip. This phase
		// runs inside a named clock's catchup — WithSim keeps the pair.
		bound := clock.WithSim(s.bindForSub(ctx, tenantID, sub.ID), clock.Sim{At: resumeAt, TestClockID: clockID})
		cleared, err := s.store.ClearPauseCollection(bound, tenantID, sub.ID)
		if errors.Is(err, ErrNotPaused) {
			// Another clearer (a sibling leader, an operator) got there
			// first: it announced the resume; we announce nothing.
			slog.InfoContext(bound, "pause collection already cleared — skipping resume announcement", "subscription_id", sub.ID)
			continue
		}
		if err != nil {
			batchErrs = append(batchErrs, fmt.Errorf("clear pause %s: %w", sub.ID, err))
			continue
		}
		triggeredBy := scheduledResumeTrigger(sub)
		if s.audit != nil {
			_ = s.audit.Log(bound, tenantID, domain.AuditActionUpdate, "subscription", cleared.ID, cleared.Code, map[string]any{
				"action":       domain.SubAuditCollectionResumed,
				"customer_id":  cleared.CustomerID,
				"resumed_at":   resumeAt.UTC(),
				"triggered_by": triggeredBy,
			})
		}
		if s.events != nil {
			s.dispatchEvent(bound, tenantID, domain.EventSubscriptionCollectionResumed, domain.SubscriptionWebhookData(cleared, map[string]any{
				"resumed_at":   resumeAt.UTC(),
				"triggered_by": triggeredBy,
			}))
		}
		processed++
	}
	return processed, batchErrs
}

// scheduledResumeTrigger names what lifted a pause the scheduled resumer
// clears: "payment" for a dunning pause, whose resumes_at only the payment
// of the invoice it was set for sets (ReleaseDunningPauseTx);
// "schedule" for an operator's timed pause.
func scheduledResumeTrigger(listed domain.Subscription) string {
	if listed.PauseCollection != nil && listed.PauseCollection.SetBy == domain.PauseSetByDunning {
		return "payment"
	}
	return "schedule"
}

// ProcessExpiredPauseCollections is the wall-clock cron counterpart —
// scans non-clock-pinned subs whose pause_collection_resumes_at has
// elapsed in wall-clock time. Same shape as the catchup entry above.
// Per ADR-028 disjoint flows, the store query explicitly excludes
// clock-pinned rows.
func (s *Service) ProcessExpiredPauseCollections(ctx context.Context, batch int) (int, []error) {
	if batch <= 0 {
		batch = 100
	}
	now := s.clock.Now(ctx)
	expired, err := s.store.ListExpiredPauseCollections(ctx, now, batch)
	if err != nil {
		return 0, []error{fmt.Errorf("list expired pause collections: %w", err)}
	}
	var (
		processed int
		batchErrs []error
	)
	for _, sub := range expired {
		// Contracted instant, same rationale as the clock path above. On
		// the cron this is at most one tick behind wall now, but the
		// principle holds: resumed_at is when the resume was promised.
		resumeAt := now
		if sub.PauseCollection != nil && sub.PauseCollection.ResumesAt != nil {
			resumeAt = *sub.PauseCollection.ResumesAt
		}
		cleared, err := s.store.ClearPauseCollection(ctx, sub.TenantID, sub.ID)
		if errors.Is(err, ErrNotPaused) {
			// Another clearer (a sibling leader in a failover window, an
			// operator) got there first and announced the resume.
			slog.InfoContext(ctx, "pause collection already cleared — skipping resume announcement", "subscription_id", sub.ID)
			continue
		}
		if err != nil {
			batchErrs = append(batchErrs, fmt.Errorf("clear pause %s: %w", sub.ID, err))
			continue
		}
		triggeredBy := scheduledResumeTrigger(sub)
		if s.audit != nil {
			_ = s.audit.Log(ctx, sub.TenantID, domain.AuditActionUpdate, "subscription", cleared.ID, cleared.Code, map[string]any{
				"action":       domain.SubAuditCollectionResumed,
				"customer_id":  cleared.CustomerID,
				"resumed_at":   resumeAt.UTC(),
				"triggered_by": triggeredBy,
			})
		}
		if s.events != nil {
			s.dispatchEvent(ctx, sub.TenantID, domain.EventSubscriptionCollectionResumed, domain.SubscriptionWebhookData(cleared, map[string]any{
				"resumed_at":   resumeAt.UTC(),
				"triggered_by": triggeredBy,
			}))
		}
		processed++
	}
	return processed, batchErrs
}

// ExtendTrial pushes a trialing subscription's trial_end_at later. Used
// when sales/ops grant an existing free-trial customer more time before
// the auto-flip-and-bill fires. newTrialEnd must be strictly in the
// future (compared to the service clock) and strictly after the current
// trial_end_at — shrinking a trial bypasses the operator-intent that
// EndTrial captures, and setting a past timestamp would make the next
// cycle scan flip the sub immediately, which is what EndTrial is for.
// The store atomic enforces status='trialing' so this can't race the
// cycle-scan auto-flip.
func (s *Service) ExtendTrial(ctx context.Context, tenantID, id string, newTrialEnd time.Time) (domain.Subscription, error) {
	ctx = s.bindForSub(ctx, tenantID, id)
	now := s.clock.Now(ctx)
	if !newTrialEnd.After(now) {
		return domain.Subscription{}, errs.Invalid("trial_end", "must be in the future")
	}
	current, err := s.store.Get(ctx, tenantID, id)
	if err != nil {
		return domain.Subscription{}, err
	}
	if current.Status != domain.SubscriptionTrialing {
		return domain.Subscription{}, errs.InvalidState(fmt.Sprintf("Only a trialing subscription's trial can be extended. This one is %s.", current.Status))
	}
	if current.TrialEndAt != nil && !newTrialEnd.After(*current.TrialEndAt) {
		return domain.Subscription{}, errs.Invalid("trial_end", "Pick a date after the current trial end. To end the trial sooner, use End trial now.")
	}
	// A pending EXPLICIT cancel_at blocks the extension (ADR-069): extending
	// past the pinned timestamp strands it — nothing fires inside a running
	// trial, so the sub silently outlives the operator's echoed cancel date.
	// Flag-only schedules pass through: cancel-at-trial-end MOVES with the
	// trial by design (the very reason the flag is kept as a flag). Fast
	// feedback here; the store UPDATE's cancel_at IS NULL predicate is the
	// atomic backstop.
	if current.CancelAt != nil {
		return domain.Subscription{}, errs.InvalidState("This subscription is scheduled to cancel on a set date. Undo the scheduled cancellation first, then extend the trial.")
	}

	// Re-anchor the first chargeable cycle on the new trial_end. Without
	// this, extending past the old current_period_end silently drops a
	// stub (same bug class as the pre-fix calendar+trial Create branch).
	loc, err := s.tenantLocation(ctx, current.TenantID)
	if err != nil {
		return domain.Subscription{}, err
	}
	newEnd := newTrialEnd.UTC()
	// The create bound holds here too: a trial ends at most maxTrialDays
	// after it started, counted the way create counts trial_days.
	if current.TrialStartAt != nil && newEnd.After(domain.AddDaysIn(*current.TrialStartAt, maxTrialDays, loc)) {
		return domain.Subscription{}, errs.Invalid("trial_end", fmt.Sprintf("A trial can last at most %d days from its start. Pick an earlier date.", maxTrialDays))
	}
	interval := s.firstPlanInterval(ctx, tenantID, current.Items)
	ps, pe, anchorDay := firstPeriodAfterTrial(newEnd, current.BillingTime, interval, loc)
	return s.store.ExtendTrial(ctx, tenantID, id, newEnd, ps, pe, pe, anchorDay)
}

// ItemThresholdInput is one configured per-item usage cap on a subscription.
// UsageGTE arrives as a JSON string from the wire so meter quantities that
// can be fractional (cached-token ratios, GPU-hours) round-trip without
// float drift. The service parses it into shopspring/decimal before handing
// off to the store.
type ItemThresholdInput struct {
	SubscriptionItemID string `json:"subscription_item_id"`
	UsageGTE           string `json:"usage_gte"`
}

// BillingThresholdsInput is the PATCH body shape for /v1/subscriptions/{id}
// when setting thresholds. AmountGTE is integer cents; ItemThresholds is the
// always-array of per-item caps. Either AmountGTE or ItemThresholds (or both)
// must be set — a body with neither is rejected as no-op.
//
// A threshold fire always keeps the billing period (Stripe's default,
// reset_billing_cycle_anchor=false). reset_billing_cycle was removed on
// 2026-10-23: the handler decodes strictly, so a body that still sends it,
// true or false, is refused with a 422 naming the field.
type BillingThresholdsInput struct {
	AmountGTE      int64                `json:"amount_gte,omitempty"`
	ItemThresholds []ItemThresholdInput `json:"item_thresholds,omitempty"`
}

// SetBillingThresholds writes (amount_gte, item_thresholds) onto
// a subscription. Validates: at least one of amount_gte or item_thresholds is
// supplied (a body with neither is a no-op masquerade); amount_gte > 0 if
// supplied; usage_gte is a decimal string above 0 inside
// domain.QuantityLimits; every
// subscription_item_id in item_thresholds belongs to this subscription;
// duplicate item ids are rejected so the underlying PK doesn't surface as a
// mid-tx integrity error. Multi-currency rejection happens upstream at the
// handler (the only layer with a PlanReader).
//
// Replaces the full set on every call: per-item rows for any item not in the
// new slice are deleted by the store. Idempotent — calling with the same
// input replaces the row's columns and aux rows with the same values.
func (s *Service) SetBillingThresholds(ctx context.Context, tenantID, id string, input BillingThresholdsInput) (domain.Subscription, error) {
	if input.AmountGTE == 0 && len(input.ItemThresholds) == 0 {
		return domain.Subscription{}, errs.Invalid("billing_thresholds",
			"at least one of amount_gte or item_thresholds must be set; to clear use DELETE")
	}
	if input.AmountGTE < 0 {
		return domain.Subscription{}, errs.Invalid("amount_gte", "must be > 0")
	}

	ctx = s.bindForSub(ctx, tenantID, id)
	sub, err := s.store.Get(ctx, tenantID, id)
	if err != nil {
		return domain.Subscription{}, err
	}
	if sub.Status == domain.SubscriptionCanceled {
		return domain.Subscription{}, errs.InvalidState(
			fmt.Sprintf("Spend thresholds can't be set on a %s subscription.", sub.Status))
	}

	itemIDs := make(map[string]struct{}, len(sub.Items))
	for _, it := range sub.Items {
		itemIDs[it.ID] = struct{}{}
	}

	parsed := make([]domain.SubscriptionItemThreshold, 0, len(input.ItemThresholds))
	seen := make(map[string]struct{}, len(input.ItemThresholds))
	for i, t := range input.ItemThresholds {
		if t.SubscriptionItemID == "" {
			return domain.Subscription{}, errs.Required(fmt.Sprintf("item_thresholds[%d].subscription_item_id", i))
		}
		if _, dup := seen[t.SubscriptionItemID]; dup {
			return domain.Subscription{}, errs.Invalid("item_thresholds",
				fmt.Sprintf("duplicate subscription_item_id %q", t.SubscriptionItemID))
		}
		seen[t.SubscriptionItemID] = struct{}{}
		if _, ok := itemIDs[t.SubscriptionItemID]; !ok {
			return domain.Subscription{}, errs.Invalid(
				fmt.Sprintf("item_thresholds[%d].subscription_item_id", i),
				fmt.Sprintf("item %q does not belong to this subscription", t.SubscriptionItemID))
		}
		// The column is NUMERIC(38,12) with CHECK usage_gte > 0: a 13th
		// decimal place was rounded away, and 0 or 10^26 failed the write.
		field := fmt.Sprintf("item_thresholds[%d].usage_gte", i)
		usage, err := domain.ParseDecimalString(field, t.UsageGTE)
		if err != nil {
			return domain.Subscription{}, err
		}
		if usage, err = domain.CheckDecimal(field, usage, domain.QuantityLimits); err != nil {
			return domain.Subscription{}, err
		}
		if !usage.IsPositive() {
			return domain.Subscription{}, errs.Invalid(field, field+" must be greater than 0")
		}
		parsed = append(parsed, domain.SubscriptionItemThreshold{
			SubscriptionItemID: t.SubscriptionItemID,
			UsageGTE:           usage,
		})
	}

	return s.store.SetBillingThresholds(ctx, tenantID, id, domain.BillingThresholds{
		AmountGTE:      input.AmountGTE,
		ItemThresholds: parsed,
	})
}

// ClearBillingThresholds removes any threshold configuration on a
// subscription. Idempotent — clearing on a sub that has no threshold returns
// the unchanged subscription.
func (s *Service) ClearBillingThresholds(ctx context.Context, tenantID, id string) (domain.Subscription, error) {
	ctx = s.bindForSub(ctx, tenantID, id)
	return s.store.ClearBillingThresholds(ctx, tenantID, id)
}

// addReliefMeta stamps a cancel's relief on its audit row: the balance credit
// ("Prorated credit $X") and the unused time the funding invoices had no room
// left for (uncredited_cents, ADR-118 amendment 2026-10-22). The currency is
// the final invoice's, built in the same tx; a cancel with no final invoice
// omits it and the timeline falls back to its default rendering.
func addReliefMeta(meta map[string]any, r domain.Relief, currency string) {
	if r.CreditedCents > 0 {
		meta["prorated_credit_cents"] = r.CreditedCents
	}
	if r.UncreditedCents > 0 {
		meta["uncredited_cents"] = r.UncreditedCents
	}
	if (r.CreditedCents > 0 || r.UncreditedCents > 0) && currency != "" {
		meta["currency"] = currency
	}
}
