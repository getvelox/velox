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

	"github.com/shopspring/decimal"

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

// CustomerReader fetches a customer by id — narrow shape used by
// Create to read customer.test_clock_id for inheritance (ADR-027).
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
	// usage). No-op when canceled_at is at/after current_period_end
	// (clean cancel, cycle close handles it).
	BillFinalOnImmediateCancel(ctx context.Context, sub domain.Subscription) (domain.Invoice, error)
	// BillFinalOnImmediateCancelTx is the in-tx variant: inserts the
	// final-on-cancel invoice on the caller's coordinator tx (ADR-056); the
	// external finalize runs post-commit via FinalizeOnCreateInvoice. Empty
	// invoice (nil error) for the no-op cases.
	BillFinalOnImmediateCancelTx(ctx context.Context, tx *sql.Tx, sub domain.Subscription) (domain.Invoice, error)
	BillOnCancel(ctx context.Context, sub domain.Subscription) (int64, error)
	// BillOnCancelDraftsTx is the ATOMIC half of the cancel-proration credit
	// (ADR-057 extension): when every funding source is PAID, it creates the
	// credit-note drafts (issue_pending) on the caller's cancel tx and returns
	// their ids + the gross owed. handled=false means the in-tx path declined
	// (any unpaid source, or no funding) and the caller must fall back to the
	// post-commit BillOnCancel. A draft-create error rolls the cancel back.
	BillOnCancelDraftsTx(ctx context.Context, tx *sql.Tx, sub domain.Subscription) (ids []string, creditedCents int64, handled bool, err error)
	// IssueCancelDrafts issues the cancel-credit drafts post-commit (external
	// Stripe tax reversal + balance grant); best-effort, recovered by the
	// clawback reconciler. Mirrors FinalizeOnCreateInvoice.
	IssueCancelDrafts(ctx context.Context, sub domain.Subscription, ids []string)
	// BillOnPlanSwapImmediate issues a refund credit for the unused
	// portion of an in_advance billed period when an immediate
	// plan-swap restructures the cycle (cross-interval). No-op for
	// in_arrears subs (nothing prebilled) and when at >= period_end.
	// Caller invokes BEFORE applying the swap so plan lookups still
	// resolve the outgoing plan rate. POST-COMMIT FALLBACK ONLY on the
	// atomic path: BillOnPlanSwapDraftsTx below is the primary (deferred
	// Bug B, closed 2026-07-05) — this runs only when the in-tx half
	// declined (unpaid funding source / lookup blip).
	BillOnPlanSwapImmediate(ctx context.Context, sub domain.Subscription, at time.Time) (int64, error)
	// BillOnPlanSwapDraftsTx is the ATOMIC half of the cross-interval swap
	// refund — the BillOnCancelDraftsTx shape: when every funding source
	// for the OLD period is PAID, the refund credit-note drafts commit ON
	// the swap tx (issue_pending), so a crash between the swap commit and
	// the refund can no longer lose the customer's unused prepayment (a
	// swap retry 400s on the same-plan guard; no reconciler re-derives a
	// missed swap refund). handled=false = declined → caller falls back to
	// the post-commit BillOnPlanSwapImmediate. A draft-create error rolls
	// the whole swap back. sub is the PRE-swap snapshot.
	BillOnPlanSwapDraftsTx(ctx context.Context, tx *sql.Tx, sub domain.Subscription, at time.Time) (ids []string, creditedCents int64, handled bool, err error)
	// IssueSwapDrafts issues the swap-refund drafts post-commit; best-effort,
	// recovered by the clawback reconciler. Twin of IssueCancelDrafts.
	IssueSwapDrafts(ctx context.Context, sub domain.Subscription, ids []string)
	// BillOnCreateTx is the in-tx variant of BillOnCreate: it inserts the day-1
	// in_advance invoice on the caller's tx (no finalize). ok=false means there
	// was nothing to bill (no in_advance items / zero subtotal) or an idempotent
	// skip. Used by the atomic cross-interval swap so the new-period invoice
	// commits atomically with the plan write + watermark advance.
	BillOnCreateTx(ctx context.Context, tx *sql.Tx, sub domain.Subscription) (domain.Invoice, bool, error)
	// FinalizeOnCreateInvoice runs the post-commit external steps (audit, tax
	// commit, auto-charge) for an invoice inserted via BillOnCreateTx. Called
	// AFTER the tx commits — never inside it.
	FinalizeOnCreateInvoice(ctx context.Context, sub domain.Subscription, inv domain.Invoice)
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
	resolver  clock.Resolver
	events    domain.EventDispatcher
	audit     AuditLogger
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
// to inherit test_clock_id from the owning customer (ADR-027). When
// unwired (narrow unit tests), the new sub has no clock unless the
// caller stamps one directly. Production wires the real customer
// store via router.go.
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

// tenantLocation resolves the tenant's preferred timezone (ADR-010).
// Errors and missing/invalid TZ strings collapse to UTC — the snap is
// a UX improvement over raw timestamps and shouldn't fail the create
// call when settings are unreadable. ADR-010-aligned with the
// dashboard's @/lib/dates helpers.
func (s *Service) tenantLocation(ctx context.Context, tenantID string) *time.Location {
	if s.settings == nil {
		return time.UTC
	}
	ts, err := s.settings.Get(ctx, tenantID)
	if err != nil || ts.Timezone == "" {
		return time.UTC
	}
	loc, err := time.LoadLocation(ts.Timezone)
	if err != nil {
		return time.UTC
	}
	return loc
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
// activated without a trial — either via StartNow on Create, or via the
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
// billing-profile currency (highest-precedence invoice label), and the
// customer's other non-terminal subs (the credit ledger is a currency-blind
// per-customer pot — mixed-currency subs would drain it across
// denominations 1:1). Skipped when PlanReader isn't wired (narrow unit
// tests), mirroring every sibling guard.
func (s *Service) rejectCurrencyMismatch(ctx context.Context, tenantID, customerID, excludeSubID string, planIDs []string, outgoingPlanID string) error {
	if s.plans == nil {
		return nil
	}
	target, describe := "", ""
	if outgoingPlanID != "" {
		if op, err := s.plans.GetPlan(ctx, tenantID, outgoingPlanID); err == nil {
			target = strings.ToUpper(strings.TrimSpace(op.Currency))
			describe = fmt.Sprintf("the item's current plan %q (%s)", op.Code, target)
		}
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
		if bp, err := s.profiles.GetBillingProfile(ctx, tenantID, customerID); err == nil {
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
				"this customer already has subscription %q billing in %s — a customer's subscriptions share one currency (credits and balances don't convert); use a %s plan or a separate customer",
				o.SubCode, oc, oc))
		}
	}
	return nil
}

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

type CreateInput struct {
	Code          string                         `json:"code"`
	DisplayName   string                         `json:"display_name"`
	CustomerID    string                         `json:"customer_id"`
	Items         []CreateItemInput              `json:"items"`
	BillingTime   domain.SubscriptionBillingTime `json:"billing_time"`
	TrialDays     int                            `json:"trial_days,omitempty"`
	StartNow      bool                           `json:"start_now,omitempty"`
	UsageCapUnits *int64                         `json:"usage_cap_units,omitempty"`
	OverageAction string                         `json:"overage_action,omitempty"`
}

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
		if cust, err := s.customers.Get(ctx, tenantID, input.CustomerID); err == nil {
			// Archived customer = no NEW subscriptions (ADR-067). Archive's
			// contract is "hide + stop new business"; without this guard a
			// direct API call quietly starts billing a customer the operator
			// explicitly retired.
			if cust.Status == domain.CustomerStatusArchived {
				return domain.Subscription{}, errs.InvalidState("customer is archived — unarchive the customer before creating a subscription")
			}
			inheritedClockID = cust.TestClockID
		}
		// If err != nil here, fall through — the downstream FK check
		// on customer_id will fail with a clean 400.
	}

	// Archived plans accept no new subscriptions (ADR-067; same enforcement
	// seam as the archived-customer guard). Pre-fix an archived plan
	// remained fully subscribable via the API — archive only hid it from
	// the picker. Skipped when PlanReader isn't wired (narrow unit tests).
	if s.plans != nil {
		for _, it := range items {
			plan, err := s.plans.GetPlan(ctx, tenantID, it.PlanID)
			if err != nil {
				return domain.Subscription{}, errs.Invalid("items", fmt.Sprintf("plan %q not found", it.PlanID))
			}
			if plan.Status == domain.PlanArchived {
				return domain.Subscription{}, errs.InvalidState(fmt.Sprintf("plan %q is archived — unarchive it or pick an active plan", it.PlanID))
			}
		}

		// Double-billing guard: the new sub's meters must be disjoint from
		// every other live sub of this customer (and from each other).
		planIDs := make([]string, 0, len(items))
		for _, it := range items {
			planIDs = append(planIDs, it.PlanID)
		}
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
	loc := s.tenantLocation(ctx, tenantID)

	if input.TrialDays > 0 {
		ts := now
		te := now.AddDate(0, 0, input.TrialDays)
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
	} else if input.StartNow {
		status = domain.SubscriptionActive
		startedAt = &now
		// Immediate activation IS an activation — stamp activated_at so
		// the sub is visible to MRR movement / point-in-time / churn,
		// which key on activated_at as the MRR-start event. The operator
		// Activate() and trial-end ActivateAfterTrial() paths already do
		// this; the StartNow create path used to leave it NULL, so
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

	overageAction := input.OverageAction
	if overageAction == "" {
		overageAction = "charge"
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
		UsageCapUnits:             input.UsageCapUnits,
		OverageAction:             overageAction,
		TestClockID:               inheritedClockID,
		Items:                     items,
	}, func(tx *sql.Tx, created domain.Subscription) error {
		if s.biller == nil || created.Status != domain.SubscriptionActive {
			return nil
		}
		inv, ok, billErr := s.biller.BillOnCreateTx(ctx, tx, created)
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
		return domain.Subscription{}, errs.InvalidState(fmt.Sprintf("can only activate draft subscriptions, current status: %s", sub.Status))
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

	// First-period math: same helper as Create's StartNow branch — period
	// begins at `now` (day-snapped) and ends at the next calendar boundary
	// (calendar billing) or `now + 1 month` (anniversary). (The historical
	// beginningOfMonth backdating bug and its fix are recorded on
	// firstPeriodForActivate.) A draft that already carries a period (e.g.
	// seeded by an earlier flow) keeps it unchanged.
	ps, pe := sub.CurrentBillingPeriodStart, sub.CurrentBillingPeriodEnd
	anchorDay := sub.BillingAnchorDay
	if ps == nil {
		loc := s.tenantLocation(ctx, sub.TenantID)
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
		inv, ok, billErr := s.biller.BillOnCreateTx(ctx, tx, a)
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
	// crossAxisNewInvoice carries the new-period in_advance invoice created
	// in-tx by the atomic cross-interval swap (applyCrossIntervalPlanSwapTx),
	// so the handler can run FinalizeOnCreateInvoice (tax commit + auto-charge)
	// POST-commit. nil when the swap was in_arrears (no day-1 invoice) or when
	// the swap ran via the non-atomic fallback. Internal — not serialized.
	crossAxisNewInvoice *domain.Invoice
	// swapRefundDraftIDs are the issue_pending refund drafts committed ON the
	// swap tx by BillOnPlanSwapDraftsTx; FinalizeCrossIntervalSwap issues them
	// post-commit. swapRefundHandled=true means the in-tx half owned the
	// refund (possibly a no-op) and the post-commit BillOnPlanSwapImmediate
	// fallback MUST NOT run — it would double-credit.
	swapRefundDraftIDs []string
	swapRefundHandled  bool
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
	// Clawback* carry a downgrade clawback that must be issued as a tax-
	// reversing credit note AFTER the atomic tx commits — the credit-note
	// service is not tx-aware and its tax reversal is an external Stripe call,
	// so it can't ride the DB tx (a rollback would orphan a committed CN +
	// balance grant). Set only on the downgrade-credit path when the CN issuer
	// is wired and a PAID source invoice was resolved (ADR-048); empty
	// otherwise (legacy net-grant fallback, upgrade/invoice path). Internal —
	// not API surface.
	ClawbackInvoiceID  string `json:"-"`
	ClawbackGrossCents int64  `json:"-"`
	ClawbackReason     string `json:"-"`
	ClawbackMemo       string `json:"-"`
	// ClawbackPieces fans a downgrade/qty-decrease/item-removal clawback across
	// EVERY funding invoice of the period — LIFO (newest funding invoice first)
	// for a plan downgrade, proportional for fungible qty/item changes — each a
	// tax-reversing adjustment credit note against its own invoice. When
	// non-empty it supersedes the scalar Clawback* fields (which remain the
	// single-piece carrier for the unpaid-source amount_due relief path). On the
	// ATOMIC path each piece is CREATED as a draft IN the caller's tx
	// (createClawbackDraftsTx → ClawbackDraftIDs) so it commits with the item
	// change, then ISSUED post-commit (issueClawbackDrafts); the non-atomic
	// fallback still create+issues inline via issueClawbackCreditNote.
	ClawbackPieces []ClawbackPiece `json:"-"`
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
}

// ClawbackPiece is one funding invoice's slice of a multi-invoice downgrade
// clawback. Each is issued as its own tax-reversing adjustment credit note
// against InvoiceID for GrossCents (tax-inclusive), reversing that invoice's
// own proportional output tax. The pieces sum to the clawback's total gross.
type ClawbackPiece struct {
	InvoiceID  string
	GrossCents int64
}

func (s *Service) AddItem(ctx context.Context, tenantID, subscriptionID string, input AddItemInput) (domain.SubscriptionItem, error) {
	if input.PlanID == "" {
		return domain.SubscriptionItem{}, errs.Required("plan_id")
	}
	qty := input.Quantity
	if qty == 0 {
		qty = 1
	}
	if qty < 1 {
		return domain.SubscriptionItem{}, errs.Invalid("quantity", "must be >= 1")
	}

	sub, err := s.store.Get(ctx, tenantID, subscriptionID)
	if err != nil {
		return domain.SubscriptionItem{}, err
	}
	if sub.Status == domain.SubscriptionCanceled || sub.Status == domain.SubscriptionArchived {
		return domain.SubscriptionItem{}, errs.InvalidState(fmt.Sprintf("cannot add items to %s subscriptions", sub.Status))
	}

	// Mixed-interval guard: new item's plan must match the sub's
	// existing item intervals. Same rationale as Create — the period
	// anchor is per-sub, so adding a yearly item to a monthly sub
	// would have no coherent cycle. Skipped when PlanReader isn't
	// wired (narrow unit tests).
	if s.plans != nil && len(sub.Items) > 0 {
		newPlusExisting := append([]domain.SubscriptionItem{{PlanID: input.PlanID}}, sub.Items...)
		if err := s.rejectMixedItemIntervals(ctx, tenantID, newPlusExisting); err != nil {
			return domain.SubscriptionItem{}, err
		}
	}

	// Double-billing guard: the added plan's meters must be disjoint from
	// every live sub of the customer — including this sub's other items.
	if err := s.rejectMeterOverlap(ctx, tenantID, sub.CustomerID, subscriptionID, "", []string{input.PlanID}); err != nil {
		return domain.SubscriptionItem{}, err
	}
	// Currency pin (ADR-100): the added plan + this sub's existing items
	// must share one currency (and match profile + the customer's other
	// subs — this sub is excluded from the cross-sub query, so its own
	// items ride in planIDs).
	{
		pin := []string{input.PlanID}
		for _, it := range sub.Items {
			pin = append(pin, it.PlanID)
		}
		if err := s.rejectCurrencyMismatch(ctx, tenantID, sub.CustomerID, subscriptionID, pin, ""); err != nil {
			return domain.SubscriptionItem{}, err
		}
	}

	ctx = s.bindForSub(ctx, tenantID, subscriptionID)
	return s.store.AddItem(ctx, tenantID, domain.SubscriptionItem{
		SubscriptionID: subscriptionID,
		PlanID:         input.PlanID,
		Quantity:       qty,
	})
}

// AddItemTx is the in-transaction variant used by the handler's atomic
// AddItem-with-proration flow. Runs the same validation as AddItem
// (required fields, status gate, mixed-interval guard) but writes the
// item via the store's AddItemTx so the insert shares the caller's tx.
// Without this, the item committed in its own tx and a subsequent
// proration failure left an orphan item (the bug fixed by ADR-030
// atomic-proration follow-through, 2026-05-29).
func (s *Service) AddItemTx(ctx context.Context, tx *sql.Tx, tenantID, subscriptionID string, input AddItemInput) (domain.SubscriptionItem, error) {
	if input.PlanID == "" {
		return domain.SubscriptionItem{}, errs.Required("plan_id")
	}
	qty := input.Quantity
	if qty == 0 {
		qty = 1
	}
	if qty < 1 {
		return domain.SubscriptionItem{}, errs.Invalid("quantity", "must be >= 1")
	}

	// Re-read sub for status + mixed-interval guard. The reads happen
	// in their own tx via store.Get (a snapshot of the pre-tx state is
	// fine for guard checks — concurrent mutations on the same sub
	// would race regardless, and the unique index on (sub, plan) is
	// the real correctness gate at write time).
	sub, err := s.store.Get(ctx, tenantID, subscriptionID)
	if err != nil {
		return domain.SubscriptionItem{}, err
	}
	if sub.Status == domain.SubscriptionCanceled || sub.Status == domain.SubscriptionArchived {
		return domain.SubscriptionItem{}, errs.InvalidState(fmt.Sprintf("cannot add items to %s subscriptions", sub.Status))
	}

	if s.plans != nil && len(sub.Items) > 0 {
		newPlusExisting := append([]domain.SubscriptionItem{{PlanID: input.PlanID}}, sub.Items...)
		if err := s.rejectMixedItemIntervals(ctx, tenantID, newPlusExisting); err != nil {
			return domain.SubscriptionItem{}, err
		}
	}

	// Double-billing guard — same as the non-Tx AddItem. Runs its own
	// read tx (like the sub re-read above); the unique index stays the
	// write-time gate for same-plan duplicates.
	if err := s.rejectMeterOverlap(ctx, tenantID, sub.CustomerID, subscriptionID, "", []string{input.PlanID}); err != nil {
		return domain.SubscriptionItem{}, err
	}
	// Currency pin (ADR-100) — same as the non-Tx AddItem.
	{
		pin := []string{input.PlanID}
		for _, it := range sub.Items {
			pin = append(pin, it.PlanID)
		}
		if err := s.rejectCurrencyMismatch(ctx, tenantID, sub.CustomerID, subscriptionID, pin, ""); err != nil {
			return domain.SubscriptionItem{}, err
		}
	}

	ctx = s.bindForSub(ctx, tenantID, subscriptionID)
	return s.store.AddItemTx(ctx, tx, tenantID, domain.SubscriptionItem{
		SubscriptionID: subscriptionID,
		PlanID:         input.PlanID,
		Quantity:       qty,
	})
}

// UpdateItemTx is the in-transaction variant of UpdateItem for the
// simple paths (quantity change, same-interval immediate plan change).
// Returns an error if the call would route to the cross-interval-swap
// path (which involves engine.BillOnPlanSwapImmediate and multiple
// store writes that don't yet have Tx variants) or the scheduled-
// change path (no proration emission — atomicity isn't needed).
// Caller falls back to the non-Tx UpdateItem in those cases.
//
// ADR-030 atomic-proration follow-through (2026-05-29). Cross-interval
// swap atomicity is a documented follow-up — the engine-level write
// composition is bigger than the handler-level work this PR scoped.
func (s *Service) UpdateItemTx(ctx context.Context, tx *sql.Tx, tenantID, subscriptionID, itemID string, input UpdateItemInput) (ItemChangeResult, error) {
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
		return ItemChangeResult{}, errs.ErrNotFound
	}
	sub, err := s.store.Get(ctx, tenantID, subscriptionID)
	if err != nil {
		return ItemChangeResult{}, err
	}
	if sub.Status != domain.SubscriptionActive {
		return ItemChangeResult{}, errs.InvalidState(fmt.Sprintf("can only modify items on active subscriptions, current status: %s", sub.Status))
	}
	ctx = s.bindForSub(ctx, tenantID, subscriptionID)
	now := s.clock.Now(ctx)

	if input.Quantity != nil {
		if *input.Quantity < 1 {
			return ItemChangeResult{}, errs.Invalid("quantity", "must be >= 1")
		}
		if *input.Quantity == item.Quantity {
			return ItemChangeResult{}, errs.Invalid("quantity", "new quantity is the same as current quantity")
		}
		updated, err := s.store.UpdateItemQuantityTx(ctx, tx, tenantID, itemID, *input.Quantity)
		if err != nil {
			return ItemChangeResult{}, err
		}
		return ItemChangeResult{Item: updated, EffectiveAt: now}, nil
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
			return s.applyCrossIntervalPlanSwapTx(ctx, tx, tenantID, subscriptionID, itemID, input.NewPlanID, sub, newInterval, newTiming, now)
		}
	}
	if !input.Immediate {
		return ItemChangeResult{}, errAtomicNotApplicable
	}
	updated, err := s.store.ApplyItemPlanImmediatelyTx(ctx, tx, tenantID, itemID, input.NewPlanID, now)
	if err != nil {
		return ItemChangeResult{}, err
	}
	return ItemChangeResult{Item: updated, EffectiveAt: now}, nil
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
		return ItemChangeResult{}, errs.InvalidState(fmt.Sprintf("can only modify items on active subscriptions, current status: %s", sub.Status))
	}

	// Bind via sub pin: pending_plan_effective_at (and the immediate-
	// effectiveAt return) need to be in simulated time so the engine's
	// catchup picks up the rollover at the operator's next Advance,
	// not at wall-clock-2026.
	ctx = s.bindForSub(ctx, tenantID, subscriptionID)
	now := s.clock.Now(ctx)

	if input.Quantity != nil {
		if *input.Quantity < 1 {
			return ItemChangeResult{}, errs.Invalid("quantity", "must be >= 1")
		}
		if *input.Quantity == item.Quantity {
			return ItemChangeResult{}, errs.Invalid("quantity", "new quantity is the same as current quantity")
		}
		updated, err := s.store.UpdateItemQuantity(ctx, tenantID, itemID, *input.Quantity)
		if err != nil {
			return ItemChangeResult{}, err
		}
		return ItemChangeResult{Item: updated, EffectiveAt: now}, nil
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
		//   period via BillOnPlanSwapImmediate, jump current_period
		//   to (now, NextBillingPeriodEnd for NEW interval), and
		//   synchronously bill the NEW in_advance period (with stub
		//   proration if calendar snap shortens it).
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
			return s.applyCrossIntervalPlanSwap(ctx, tenantID, subscriptionID, itemID, input.NewPlanID, sub, newInterval, newTiming, now)
		}
	}

	if !input.Immediate {
		var effectiveAt time.Time
		if sub.CurrentBillingPeriodEnd != nil {
			effectiveAt = *sub.CurrentBillingPeriodEnd
		} else {
			effectiveAt = now
		}
		updated, err := s.store.SetItemPendingPlan(ctx, tenantID, itemID, input.NewPlanID, effectiveAt)
		if err != nil {
			return ItemChangeResult{}, err
		}
		return ItemChangeResult{Item: updated, EffectiveAt: effectiveAt}, nil
	}

	updated, err := s.store.ApplyItemPlanImmediately(ctx, tenantID, itemID, input.NewPlanID, now)
	if err != nil {
		return ItemChangeResult{}, err
	}
	return ItemChangeResult{Item: updated, EffectiveAt: now}, nil
}

// applyCrossIntervalPlanSwap handles the immediate same-cadence
// cross-interval plan-swap path (the only restructure-the-cycle plan
// change Velox supports today). Caller has already validated:
//   - sub is active
//   - new plan exists, differs from current
//   - bill_timing matches on both sides (cross-cadence rejected upstream)
//   - newInterval differs from currentInterval
//
// See UpdateItem's inline comment for the by-cadence semantics.
func (s *Service) applyCrossIntervalPlanSwap(
	ctx context.Context,
	tenantID, subscriptionID, itemID, newPlanID string,
	sub domain.Subscription,
	newInterval domain.BillingInterval,
	newTiming domain.BillTiming,
	now time.Time,
) (ItemChangeResult, error) {
	// Step 1: refund unused OLD in_advance portion BEFORE the swap so
	// plan lookups still resolve the outgoing rate. We don't block the
	// terminal swap on a credit failure (a DB blip shouldn't strand the
	// operator's intent), but the failure is logged at ERROR — it means the
	// customer was NOT credited their unused prepayment and a manual credit is
	// required, which must be alarmable, not a silent warning.
	// No-op for in_arrears (nothing prebilled).
	if s.biller != nil {
		if _, err := s.biller.BillOnPlanSwapImmediate(ctx, sub, now); err != nil {
			slog.ErrorContext(ctx, "plan-swap refund credit FAILED — customer not credited for unused prepayment; manual credit required",
				"subscription_id", subscriptionID,
				"tenant_id", tenantID,
				"error", err)
		}
	}

	// Step 2: apply the plan swap. After this, item.plan_id = newPlanID
	// and subscription_item_changes records the transition at `now`.
	updated, err := s.store.ApplyItemPlanImmediately(ctx, tenantID, itemID, newPlanID, now)
	if err != nil {
		return ItemChangeResult{}, err
	}

	// Step 3a (in_advance): jump period to (now, NextBillingPeriodEnd)
	// and synchronously bill the new in_advance period.
	// Step 3b (in_arrears): truncate to (oldPS, now); scheduler closes
	// the partial period under the OLD plan via segment-aware billing.
	loc := s.tenantLocation(ctx, sub.TenantID)
	// The swap re-anchors the cycle to `now`, so the billing anchor day is
	// recomputed for the new cadence (ADR-055) — it must NOT keep the old
	// interval's anchor day.
	swapAnchorDay := domain.AnchorDayFor(now, sub.BillingTime, newInterval, loc)
	if newTiming == domain.BillInAdvance {
		newPE := domain.NextBillingPeriodEnd(now, sub.BillingTime, newInterval, loc, swapAnchorDay)
		if err := s.store.UpdateBillingCycle(ctx, tenantID, subscriptionID, now, newPE, newPE, swapAnchorDay); err != nil {
			return ItemChangeResult{}, err
		}
		if s.biller != nil {
			refreshed, err := s.store.Get(ctx, tenantID, subscriptionID)
			if err != nil {
				return ItemChangeResult{}, err
			}
			if _, err := s.biller.BillOnCreate(ctx, refreshed); err != nil {
				// Non-atomic fallback (h.db unwired — tests only). The watermark
				// already advanced above, so this failure CANNOT be "retried by
				// the scheduler" — the just-opened period is past next_billing_at
				// and cycle close skips in_advance base segments, so the new
				// period's base would be silently dropped. Fail LOUD instead.
				// Production uses applyCrossIntervalPlanSwapTx, which commits the
				// new-period invoice atomically with the watermark advance.
				return ItemChangeResult{}, fmt.Errorf("plan-swap new in_advance bill failed: %w", err)
			}
		}
	} else {
		// in_arrears: truncate. Period start preserved; period end +
		// next_billing pulled in to `now`.
		if sub.CurrentBillingPeriodStart != nil {
			if err := s.store.UpdateBillingCycle(ctx, tenantID, subscriptionID, *sub.CurrentBillingPeriodStart, now, now, swapAnchorDay); err != nil {
				return ItemChangeResult{}, err
			}
		}
	}
	return ItemChangeResult{Item: updated, EffectiveAt: now, OrchestratedCrossAxis: true}, nil
}

// applyCrossIntervalPlanSwapTx is the atomic, in-tx variant of
// applyCrossIntervalPlanSwap used by the handler's tx-owning path. It runs the
// cycle-restructuring DB writes — the plan swap, the watermark advance, and (for
// in_advance) the new-period invoice insert via BillOnCreateTx — on the caller's
// single tx, so any failure rolls ALL of them back. This is the fix for the
// silent revenue drop: the watermark can no longer advance past a new in_advance
// period whose day-1 invoice failed to commit. The OLD-period refund and the new
// invoice's external steps (tax commit, auto-charge) are deliberately NOT done
// here — the handler runs them POST-commit, because Stripe calls must never ride
// a DB tx. A created new invoice is carried back on the result for that
// post-commit FinalizeOnCreateInvoice. Caller has validated the swap exactly as
// applyCrossIntervalPlanSwap documents (active sub, plan exists + differs,
// same-cadence, different interval).
func (s *Service) applyCrossIntervalPlanSwapTx(
	ctx context.Context,
	tx *sql.Tx,
	tenantID, subscriptionID, itemID, newPlanID string,
	sub domain.Subscription,
	newInterval domain.BillingInterval,
	newTiming domain.BillTiming,
	now time.Time,
) (ItemChangeResult, error) {
	// OLD-period refund drafts FIRST, on this same tx (deferred Bug B,
	// closed 2026-07-05): computed from the pre-swap `sub` snapshot so the
	// math resolves the OUTGOING plan rate and the old period's funding
	// invoices. A draft-create error fails the whole swap (never a swapped
	// sub with a silently-lost refund); handled=false defers the refund to
	// the post-commit fallback in FinalizeCrossIntervalSwap (unpaid funding
	// source / lookup blip — the cancel path's exact decline contract).
	var swapDraftIDs []string
	swapRefundHandled := false
	if s.biller != nil {
		ids, _, handled, derr := s.biller.BillOnPlanSwapDraftsTx(ctx, tx, sub, now)
		if derr != nil {
			return ItemChangeResult{}, fmt.Errorf("plan-swap refund drafts: %w", derr)
		}
		swapDraftIDs, swapRefundHandled = ids, handled
	}

	// Apply the plan swap on the tx (stamps item.plan_id + plan_changed_at=now).
	updated, err := s.store.ApplyItemPlanImmediatelyTx(ctx, tx, tenantID, itemID, newPlanID, now)
	if err != nil {
		return ItemChangeResult{}, err
	}

	loc := s.tenantLocation(ctx, sub.TenantID)
	// Re-anchor the cycle to `now` for the new cadence (ADR-055) — must NOT keep
	// the old interval's anchor day.
	swapAnchorDay := domain.AnchorDayFor(now, sub.BillingTime, newInterval, loc)
	res := ItemChangeResult{
		Item: updated, EffectiveAt: now, OrchestratedCrossAxis: true,
		swapRefundDraftIDs: swapDraftIDs, swapRefundHandled: swapRefundHandled,
	}

	if newTiming == domain.BillInAdvance {
		newPE := domain.NextBillingPeriodEnd(now, sub.BillingTime, newInterval, loc, swapAnchorDay)
		// Advance the watermark on the SAME tx as the new-period invoice below,
		// so the watermark can never move without the day-1 invoice existing.
		if err := s.store.UpdateBillingCycleTx(ctx, tx, tenantID, subscriptionID, now, newPE, newPE, swapAnchorDay); err != nil {
			return ItemChangeResult{}, err
		}
		if s.biller != nil {
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

			inv, ok, err := s.biller.BillOnCreateTx(ctx, tx, prospective)
			if err != nil {
				return ItemChangeResult{}, fmt.Errorf("plan-swap new in_advance bill: %w", err)
			}
			if ok {
				invCopy := inv
				res.crossAxisNewInvoice = &invCopy
			}
		}
	} else {
		// in_arrears: truncate the current period to (oldPS, now); the scheduler
		// closes the partial period under the OLD plan via segment-aware billing.
		if sub.CurrentBillingPeriodStart != nil {
			if err := s.store.UpdateBillingCycleTx(ctx, tx, tenantID, subscriptionID, *sub.CurrentBillingPeriodStart, now, now, swapAnchorDay); err != nil {
				return ItemChangeResult{}, err
			}
		}
	}
	return res, nil
}

// FinalizeCrossIntervalSwap runs the POST-commit external steps for an atomic
// cross-interval swap, called by the handler AFTER the swap tx is durable
// (Stripe calls must never ride a DB tx):
//
//   - OLD-period refund. The refund credit-note DRAFTS were committed ON the
//     swap tx by BillOnPlanSwapDraftsTx (deferred Bug B, closed 2026-07-05) —
//     here they only get their post-commit Issue relay (Stripe tax reversal +
//     balance grant), and a never-issued draft self-heals via
//     RetryPendingClawbackIssue. Only when the in-tx half DECLINED (unpaid
//     funding source / lookup blip) does the legacy post-commit
//     BillOnPlanSwapImmediate run — same decline contract as the cancel path;
//     its failure still needs the loud ERROR because the unpaid-leg relief has
//     no reconciler behind it.
//   - FinalizeOnCreateInvoice (tax commit + auto-charge) for the new in_advance
//     day-1 invoice committed inside the swap tx, if one was created.
func (s *Service) FinalizeCrossIntervalSwap(ctx context.Context, tenantID string, subBefore domain.Subscription, result ItemChangeResult) {
	if s.biller == nil {
		return
	}
	if result.swapRefundHandled {
		s.biller.IssueSwapDrafts(ctx, subBefore, result.swapRefundDraftIDs)
	} else if _, err := s.biller.BillOnPlanSwapImmediate(ctx, subBefore, result.EffectiveAt); err != nil {
		slog.ErrorContext(ctx, "plan-swap refund credit FAILED — customer not credited for unused prepayment; manual credit required",
			"subscription_id", subBefore.ID,
			"tenant_id", tenantID,
			"error", err)
	}
	if result.crossAxisNewInvoice != nil {
		s.biller.FinalizeOnCreateInvoice(ctx, subBefore, *result.crossAxisNewInvoice)
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
	return s.store.ClearItemPendingPlan(ctx, tenantID, itemID)
}

// RemoveItem deletes an item. Removing the only remaining item on an active
// subscription is rejected — a subscription with zero priced lines has no
// valid billing semantics. Callers wanting to end billing altogether should
// Cancel the subscription.
func (s *Service) RemoveItem(ctx context.Context, tenantID, subscriptionID, itemID string) error {
	item, err := s.store.GetItem(ctx, tenantID, itemID)
	if err != nil {
		return err
	}
	if item.SubscriptionID != subscriptionID {
		return errs.ErrNotFound
	}
	sub, err := s.store.Get(ctx, tenantID, subscriptionID)
	if err != nil {
		return err
	}
	if sub.Status == domain.SubscriptionActive && len(sub.Items) <= 1 {
		return errs.InvalidState("cannot remove the last item from an active subscription; cancel the subscription instead")
	}
	ctx = s.bindForSub(ctx, tenantID, subscriptionID)
	return s.store.RemoveItem(ctx, tenantID, itemID)
}

// RemoveItemTx is the in-transaction variant — same validation as
// RemoveItem but the actual DELETE goes through store.RemoveItemTx
// so it shares the caller's tx. Used by the atomic
// RemoveItem-with-proration flow so a failed proration-credit grant
// rolls back the item delete too. ADR-030 atomic-proration follow-
// through (2026-05-29).
func (s *Service) RemoveItemTx(ctx context.Context, tx *sql.Tx, tenantID, subscriptionID, itemID string) error {
	item, err := s.store.GetItem(ctx, tenantID, itemID)
	if err != nil {
		return err
	}
	if item.SubscriptionID != subscriptionID {
		return errs.ErrNotFound
	}
	sub, err := s.store.Get(ctx, tenantID, subscriptionID)
	if err != nil {
		return err
	}
	if sub.Status == domain.SubscriptionActive && len(sub.Items) <= 1 {
		return errs.InvalidState("cannot remove the last item from an active subscription; cancel the subscription instead")
	}
	ctx = s.bindForSub(ctx, tenantID, subscriptionID)
	return s.store.RemoveItemTx(ctx, tx, tenantID, itemID)
}

// Cancel returns the canceled subscription, the cents-amount of any
// cancel-proration credit granted (0 when none — in_arrears sub,
// clean cancel, unpaid source invoice), and an error. The handler
// surfaces the credit amount in the audit-log entry so the activity
// timeline shows "Subscription canceled · Prorated credit $X.XX"
// (industry standard — Stripe / Lago / Chargebee / Orb all surface
// the credit on the subscription timeline, not just the customer's
// credit balance).
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
	var cancelDraftIDs []string
	var cancelDraftCredit int64
	var cancelDraftsHandled bool
	canceled, err := s.store.FireScheduledCancellationWithBill(ctx, tenantID, id, at, func(tx *sql.Tx, c domain.Subscription) error {
		emitAudit := func(credited int64, handled bool, currency string) error {
			if s.audit == nil {
				return nil
			}
			meta := map[string]any{
				"customer_id": c.CustomerID,
				"plan_ids":    planIDsFromItems(c.Items),
				"canceled_by": "schedule",
			}
			if handled && credited > 0 {
				meta["prorated_credit_cents"] = credited
				if currency != "" {
					meta["currency"] = currency
				}
			}
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
			return emitAudit(0, false, "")
		}
		inv, billErr := s.biller.BillFinalOnImmediateCancelTx(ctx, tx, c)
		if billErr != nil {
			return fmt.Errorf("final-on-scheduled-cancel invoice: %w", billErr)
		}
		finalInv = inv
		ids, credited, handled, draftErr := s.biller.BillOnCancelDraftsTx(ctx, tx, c)
		if draftErr != nil {
			return fmt.Errorf("scheduled-cancel proration drafts: %w", draftErr)
		}
		cancelDraftIDs, cancelDraftCredit, cancelDraftsHandled = ids, credited, handled
		return emitAudit(credited, handled, inv.Currency)
	})
	if err != nil {
		if errors.Is(err, errs.ErrInvalidState) {
			slog.InfoContext(ctx, "due scheduled cancel: benign race, skipping",
				"subscription_id", id, "tenant_id", tenantID, "reason", err.Error())
			return nil
		}
		return err
	}

	// Post-commit external legs — identical to Cancel: finalize (tax commit +
	// auto-charge) for the final invoice, then issue the durable relief
	// drafts (clawback reconciler recovers a failed issue) or the loud
	// unpaid-source fallback.
	if s.biller != nil && finalInv.ID != "" {
		s.biller.FinalizeOnCreateInvoice(ctx, canceled, finalInv)
	}
	if s.biller != nil {
		if cancelDraftsHandled {
			s.biller.IssueCancelDrafts(ctx, canceled, cancelDraftIDs)
			_ = cancelDraftCredit
		} else {
			if _, err := s.biller.BillOnCancel(ctx, canceled); err != nil {
				slog.ErrorContext(ctx, "scheduled-cancel proration credit FAILED — customer not credited for unused prepayment; manual credit required",
					"subscription_id", canceled.ID,
					"tenant_id", tenantID,
					"error", err)
			}
		}
	}
	return nil
}

func (s *Service) Cancel(ctx context.Context, tenantID, id string) (domain.Subscription, int64, error) {
	ctx = s.bindForSub(ctx, tenantID, id)

	// PR-10 / ADR-056: emit the final partial-period invoice (in_arrears
	// prorated base + usage from current_period_start → canceled_at) IN THE SAME
	// tx as the cancel flip, so a billing failure rolls the cancel back rather
	// than leaving a canceled sub with an uninvoiced partial period — a permanent
	// revenue leak (there is no final-on-cancel reconciler; the prior "G3
	// deferred reconciler" referenced here never existed). No-op for a clean
	// cancel (canceled_at at/after current_period_end). The build runs BEFORE
	// BillOnCancel so the credit grant doesn't pre-apply against this invoice.
	var finalInv domain.Invoice
	var cancelDraftIDs []string
	var cancelDraftCredit int64
	var cancelDraftsHandled bool
	canceled, err := s.store.CancelAtomicWithBill(ctx, tenantID, id, func(tx *sql.Tx, c domain.Subscription) error {
		// ADR-090 in-tx emission runs at the END of this closure (emitCancelAudit)
		// so the atomic-path credit amount can ride the row — but it fires
		// UNCONDITIONALLY: audit coverage must not be conditioned on the
		// unrelated biller dependency (a biller-less construction still
		// flips the sub to canceled).
		emitCancelAudit := func(credited int64, handled bool, currency string) error {
			if s.audit == nil {
				return nil
			}
			meta := map[string]any{
				"customer_id": c.CustomerID,
				"plan_ids":    planIDsFromItems(c.Items),
				"canceled_by": cancelActorLabel(ctx),
			}
			if handled && credited > 0 {
				meta["prorated_credit_cents"] = credited
				// Currency from the final invoice built in this same tx;
				// credit-only cancels (no final invoice) omit it and the
				// timeline falls back to its default rendering — the
				// fallback-path credit carries its own audited grant row.
				if currency != "" {
					meta["currency"] = currency
				}
			}
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
			return emitCancelAudit(0, false, "")
		}
		inv, billErr := s.biller.BillFinalOnImmediateCancelTx(ctx, tx, c)
		if billErr != nil {
			return fmt.Errorf("final-on-cancel invoice: %w", billErr)
		}
		finalInv = inv
		// Cancel-proration credit drafts (ADR-057 extension): created IN this tx
		// AFTER the final invoice (so the credit isn't sized against it — the
		// documented ordering at service.go's original BillOnCancel-runs-after
		// comment, now structurally guaranteed). All-paid funding only; any unpaid
		// source declines (handled=false) and is relieved post-commit by
		// BillOnCancel. A draft-create failure rolls the whole cancel back.
		ids, credited, handled, draftErr := s.biller.BillOnCancelDraftsTx(ctx, tx, c)
		if draftErr != nil {
			return fmt.Errorf("cancel proration drafts: %w", draftErr)
		}
		cancelDraftIDs, cancelDraftCredit, cancelDraftsHandled = ids, credited, handled

		return emitCancelAudit(credited, handled, inv.Currency)
	})
	if err != nil {
		return domain.Subscription{}, 0, err
	}

	// Post-commit external finalize for the final invoice (audit + tax commit +
	// auto-charge) — Stripe calls must never ride a DB tx; the invoice is now
	// durable + atomic with the cancel. Recoverable by the existing reconciler /
	// retry sweeps if a step fails.
	if s.biller != nil && finalInv.ID != "" {
		s.biller.FinalizeOnCreateInvoice(ctx, canceled, finalInv)
	}

	// ADR-031 / ADR-057 ext: the in_advance cancel-proration credit for the
	// unused portion of an already-billed period. No-op for in_arrears.
	// Returns the cents granted so the handler can stamp the cancel audit row
	// (the timeline "Prorated credit $X.XX" line).
	//
	// Two paths: (1) the ATOMIC path — when every funding source was paid, the
	// credit drafts were created in the cancel tx above; here we just ISSUE them
	// post-commit (external Stripe tax reversal + balance grant). The drafts are
	// already durable, so a crash/transient failure here is recovered by the
	// clawback reconciler — the customer can no longer be silently un-credited.
	// (2) the FALLBACK path — any unpaid funding source (or the in-tx path
	// declined): today's post-commit BillOnCancel, loud-on-failure (manual credit
	// required; a silent warning here is exactly how the 2026-06-15 upgrade→cancel
	// overcharge went unnoticed). PR1 scope: unpaid relief stays on this path.
	var prorationCreditCents int64
	if s.biller != nil {
		if cancelDraftsHandled {
			s.biller.IssueCancelDrafts(ctx, canceled, cancelDraftIDs)
			prorationCreditCents = cancelDraftCredit
		} else {
			amt, err := s.biller.BillOnCancel(ctx, canceled)
			if err != nil {
				slog.ErrorContext(ctx, "cancel proration credit FAILED — customer not credited for unused prepayment; manual credit required",
					"subscription_id", canceled.ID,
					"tenant_id", tenantID,
					"error", err)
			} else {
				prorationCreditCents = amt
			}
		}
	}

	return canceled, prorationCreditCents, nil
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

	sub, err := s.store.Get(ctx, tenantID, id)
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
			return domain.Subscription{}, errs.Invalid("cancel_at",
				"must be on or after current_billing_period_end (mid-period cancel with proration is not yet supported)")
		}
		cancelAt = &ts
	}

	return s.store.ScheduleCancellation(ctx, tenantID, id, cancelAt, input.AtPeriodEnd, sub.Status)
}

// ClearScheduledCancel undoes any prior schedule. Idempotent — a row
// without a schedule returns unchanged.
func (s *Service) ClearScheduledCancel(ctx context.Context, tenantID, id string) (domain.Subscription, error) {
	ctx = s.bindForSub(ctx, tenantID, id)
	return s.store.ClearScheduledCancellation(ctx, tenantID, id)
}

// PauseCollectionInput carries the collection-pause intent. Behavior is
// required and must be one of the supported modes (v1: keep_as_draft).
// ResumesAt is optional; when set, the cycle scan auto-clears the pause
// at the start of the period containing or after that timestamp so that
// period bills normally. When nil, only an explicit DELETE clears it.
//
// TriggeredBy is set by the caller (handler / dunning adapter / future
// callers) so the audit trail can distinguish operator-driven pauses
// from system-driven ones. Not exposed in JSON so the field can't be
// spoofed by an API client. Defaults to "operator" inside the service
// if empty.
type PauseCollectionInput struct {
	Behavior    domain.PauseCollectionBehavior `json:"behavior"`
	ResumesAt   *time.Time                     `json:"resumes_at,omitempty"`
	TriggeredBy string                         `json:"-"`
}

// PauseCollection sets the Stripe-parity collection-pause state. Distinct
// from Pause (hard freeze on status). The cycle keeps advancing; the engine
// generates the invoice as draft and skips finalize/charge/dunning while
// pause_collection is non-null.
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
		triggeredBy := input.TriggeredBy
		if triggeredBy == "" {
			triggeredBy = "operator"
		}
		meta := map[string]any{
			"action":       "collection_paused",
			"customer_id":  sub.CustomerID,
			"behavior":     string(input.Behavior),
			"triggered_by": triggeredBy,
		}
		if sub.PauseCollection != nil && sub.PauseCollection.ResumesAt != nil {
			meta["resumes_at"] = sub.PauseCollection.ResumesAt.UTC()
		}
		_ = s.audit.Log(ctx, tenantID, domain.AuditActionUpdate, "subscription", sub.ID, sub.Code, meta)
	}

	return sub, nil
}

// ResumeCollection clears any active collection-pause. Idempotent — a row
// without an active pause returns unchanged. Distinct from Resume (which
// flips status from paused back to active).
//
// All current callers of this method are operator-driven (handler);
// the engine's cycle-scan auto-resume writes its own audit row with
// triggered_by="schedule" via Engine.SetAuditLogger and does not call
// this service method. If a future system path resumes via the service
// (e.g. webhook callback from PSP), thread a TriggeredBy param the same
// way PauseCollection does today.
func (s *Service) ResumeCollection(ctx context.Context, tenantID, id string) (domain.Subscription, error) {
	ctx = s.bindForSub(ctx, tenantID, id)
	sub, err := s.store.ClearPauseCollection(ctx, tenantID, id)
	if err != nil {
		return domain.Subscription{}, err
	}
	if s.audit != nil {
		_ = s.audit.Log(ctx, tenantID, domain.AuditActionUpdate, "subscription", sub.ID, sub.Code, map[string]any{
			"action":       "collection_resumed",
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
		return domain.Subscription{}, errs.InvalidState(fmt.Sprintf("cannot end trial on %s subscription", sub.Status))
	}
	// A pending cancel schedule blocks the early end (ADR-069, no silent
	// precedence): ending the trial would activate + bill period 1 while the
	// schedule silently mutates into a paid-period cancel — the customer
	// pays a full period despite having canceled first. This service check
	// is the fast operator feedback; the store UPDATE's schedule-empty
	// predicate is the atomic backstop for a schedule racing this read.
	if sub.CancelAtPeriodEnd || sub.CancelAt != nil {
		return domain.Subscription{}, errs.InvalidState("subscription has a scheduled cancellation — clear the scheduled cancel first, then end the trial")
	}

	loc := s.tenantLocation(ctx, sub.TenantID)
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
		inv, ok, billErr := s.biller.BillOnCreateTx(ctx, tx, a)
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
			return domain.Subscription{}, errs.InvalidState("subscription has a scheduled cancellation — clear the scheduled cancel first, then end the trial")
		}
		return domain.Subscription{}, err
	}
	if haveInv {
		s.biller.FinalizeOnCreateInvoice(ctx, activated, firstInv)
	}

	return activated, nil
}

// ProcessExpiredTrialsForClock is the catchup Phase 0.5 entry point —
// scans subs pinned to this clock whose trial has elapsed in sim time
// and flips each from 'trialing' to 'active' at trial_end_at (not at
// the later cycle close). Bug #8: without this, status stays
// 'trialing' for the gap between trial_end_at and the first
// chargeable cycle close (up to ~30 days for calendar billing).
//
// Per sub: ActivateAfterTrial(at=trial_end_at) for the atomic status
// flip + activated_at stamp, then BillOnCreate to cover in_advance
// items' first paid period at trial_end_at (Bug #6 coverage carries
// through). Failures collected but don't abort the batch — one bad
// sub doesn't stall the others. Returns (processed_count, errors).
//
// Subscriptions matched by the scan but already-EndTrial'd by an
// operator race will land in ActivateAfterTrial's InvalidState
// branch — treated as a no-op (already correct state).
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
		"action":       "trial_end_cancel",
		"customer_id":  canceled.CustomerID,
		"triggered_by": "schedule",
		"canceled_at":  canceled.CanceledAt.UTC().Format(time.RFC3339),
	}
	// The trial-end instant is its OWN fact, under its own key. It is NOT the
	// row's sim_effective_at: that column means "the simulated instant the
	// clock stood at when this mutation was performed", and the audit writer
	// owns it from the ctx binding (simColumns). A catchup performs this cancel
	// at the advance's instant, which may be weeks of simulated time after the
	// trial actually ended — two different facts that used to collide on one
	// key, with the writer silently winning.
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
		bound := s.bindForSub(ctx, tenantID, sub.ID)
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
			inv, ok, billErr := s.biller.BillOnCreateTx(bound, tx, a)
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
			// trial_end_at is its own fact; the row's sim axis (test_clock_id +
			// sim_effective_at) comes from the ctx binding on `bound`, which the
			// audit writer owns. See cancelTrialAtEnd.
			_ = s.audit.Log(bound, tenantID, domain.AuditActionUpdate, "subscription", activated.ID, activated.Code, map[string]any{
				"action":       "trial_ended",
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
		// (the sub stays trialing and is retried next tick) rather than leaving
		// an active sub with no first-period invoice — a revenue leak (the cycle
		// scheduler skips the just-elapsed in_advance segment).
		var firstInv domain.Invoice
		var haveInv bool
		activated, err := s.store.ActivateAfterTrialWithBill(ctx, sub.TenantID, sub.ID, trialEndAt, func(tx *sql.Tx, a domain.Subscription) error {
			if s.biller == nil {
				return nil
			}
			inv, ok, billErr := s.biller.BillOnCreateTx(ctx, tx, a)
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
				"action":       "trial_ended",
				"customer_id":  activated.CustomerID,
				"triggered_by": "schedule",
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
// `subscription.collection_resumed`. Runs as a dedicated catchup phase
// BEFORE cycle billing — so when the cycle scan evaluates the sub, the
// pause has already been cleared and the invoice generates as
// finalized rather than draft.
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
		if err != nil {
			batchErrs = append(batchErrs, fmt.Errorf("clear pause %s: %w", sub.ID, err))
			continue
		}
		if s.audit != nil {
			_ = s.audit.Log(bound, tenantID, domain.AuditActionUpdate, "subscription", cleared.ID, cleared.Code, map[string]any{
				"action":       "collection_resumed",
				"customer_id":  cleared.CustomerID,
				"resumed_at":   resumeAt.UTC(),
				"triggered_by": "schedule",
			})
		}
		if s.events != nil {
			s.dispatchEvent(bound, tenantID, domain.EventSubscriptionCollectionResumed, map[string]any{
				"subscription_id": cleared.ID,
				"customer_id":     cleared.CustomerID,
				"resumed_at":      resumeAt.UTC(),
				"triggered_by":    "schedule",
			})
		}
		processed++
	}
	return processed, batchErrs
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
		if err != nil {
			batchErrs = append(batchErrs, fmt.Errorf("clear pause %s: %w", sub.ID, err))
			continue
		}
		if s.audit != nil {
			_ = s.audit.Log(ctx, sub.TenantID, domain.AuditActionUpdate, "subscription", cleared.ID, cleared.Code, map[string]any{
				"action":       "collection_resumed",
				"customer_id":  cleared.CustomerID,
				"resumed_at":   resumeAt.UTC(),
				"triggered_by": "schedule",
			})
		}
		if s.events != nil {
			s.dispatchEvent(ctx, sub.TenantID, domain.EventSubscriptionCollectionResumed, map[string]any{
				"subscription_id": cleared.ID,
				"customer_id":     cleared.CustomerID,
				"resumed_at":      resumeAt.UTC(),
				"triggered_by":    "schedule",
			})
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
		return domain.Subscription{}, errs.InvalidState(fmt.Sprintf("cannot extend trial on %s subscription", current.Status))
	}
	if current.TrialEndAt != nil && !newTrialEnd.After(*current.TrialEndAt) {
		return domain.Subscription{}, errs.Invalid("trial_end", "must be after the current trial_end_at — use end-trial to shorten")
	}
	// A pending EXPLICIT cancel_at blocks the extension (ADR-069): extending
	// past the pinned timestamp strands it — nothing fires inside a running
	// trial, so the sub silently outlives the operator's echoed cancel date.
	// Flag-only schedules pass through: cancel-at-trial-end MOVES with the
	// trial by design (the very reason the flag is kept as a flag). Fast
	// feedback here; the store UPDATE's cancel_at IS NULL predicate is the
	// atomic backstop.
	if current.CancelAt != nil {
		return domain.Subscription{}, errs.InvalidState("subscription has a scheduled cancellation with an explicit date — clear the scheduled cancel first, then extend the trial")
	}

	// Re-anchor the first chargeable cycle on the new trial_end. Without
	// this, extending past the old current_period_end silently drops a
	// stub (same bug class as the pre-fix calendar+trial Create branch).
	loc := s.tenantLocation(ctx, current.TenantID)
	newEnd := newTrialEnd.UTC()
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
// ResetBillingCycle defaults to FALSE at PATCH time when omitted — the
// threshold fire bills mid-cycle and the period anchor stays put, which is
// Stripe's documented default (reset_billing_cycle_anchor=false; the earlier
// comment claimed the opposite and was verified wrong, 2026-07-10 design
// review). Keep-anchor is also the safer silent default: it avoids the
// ADR-066 §5 {in_advance × reset=true} interaction entirely for operators
// who never chose a value. The pointer-on-field shape lets the handler
// distinguish "not supplied" (apply default) from an explicit choice.
// (The 0056 column default remains TRUE — historical and inert: every
// threshold write flows through this PATCH, which always stores an
// explicit value.)
type BillingThresholdsInput struct {
	AmountGTE         int64                `json:"amount_gte,omitempty"`
	ResetBillingCycle *bool                `json:"reset_billing_cycle,omitempty"`
	ItemThresholds    []ItemThresholdInput `json:"item_thresholds,omitempty"`
}

// SetBillingThresholds writes (amount_gte, reset_cycle, item_thresholds) onto
// a subscription. Validates: at least one of amount_gte or item_thresholds is
// supplied (a body with neither is a no-op masquerade); amount_gte > 0 if
// supplied; usage_gte parses as a non-negative decimal; every
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
	if sub.Status == domain.SubscriptionCanceled || sub.Status == domain.SubscriptionArchived {
		return domain.Subscription{}, errs.InvalidState(
			fmt.Sprintf("cannot configure billing thresholds on %s subscriptions", sub.Status))
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
		usage, derr := decimal.NewFromString(strings.TrimSpace(t.UsageGTE))
		if derr != nil {
			return domain.Subscription{}, errs.Invalid(
				fmt.Sprintf("item_thresholds[%d].usage_gte", i),
				"must be a numeric string (e.g. \"1000\" or \"3.14\")")
		}
		if usage.IsNegative() {
			return domain.Subscription{}, errs.Invalid(
				fmt.Sprintf("item_thresholds[%d].usage_gte", i),
				"must be >= 0")
		}
		parsed = append(parsed, domain.SubscriptionItemThreshold{
			SubscriptionItemID: t.SubscriptionItemID,
			UsageGTE:           usage,
		})
	}

	resetCycle := false
	if input.ResetBillingCycle != nil {
		resetCycle = *input.ResetBillingCycle
	}

	return s.store.SetBillingThresholds(ctx, tenantID, id, domain.BillingThresholds{
		AmountGTE:         input.AmountGTE,
		ResetBillingCycle: resetCycle,
		ItemThresholds:    parsed,
	})
}

// ClearBillingThresholds removes any threshold configuration on a
// subscription. Idempotent — clearing on a sub that has no threshold returns
// the unchanged subscription.
func (s *Service) ClearBillingThresholds(ctx context.Context, tenantID, id string) (domain.Subscription, error) {
	ctx = s.bindForSub(ctx, tenantID, id)
	return s.store.ClearBillingThresholds(ctx, tenantID, id)
}
