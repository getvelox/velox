package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/credit"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/payment"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/money"
	"github.com/sagarsuperuser/velox/internal/platform/telemetry"
	"github.com/sagarsuperuser/velox/internal/tax"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// roundDays converts a duration to whole-day count, rounded to nearest.
// Period boundaries snapped to 00:00 in tenant TZ produce durations
// that are either exact day-multiples or within ±1h of one (DST
// spring-forward subtracts an hour, fall-back adds one). Round absorbs
// the DST drift; truncation would silently miscount.
func roundDays(d time.Duration) int {
	return int(math.Round(d.Hours() / 24))
}

// Engine orchestrates the billing cycle: finds subscriptions due for billing,
// aggregates usage, computes charges, and generates invoices with line items.
//
// It coordinates across domain boundaries (subscription, usage, pricing, invoice)
// without those domains knowing about each other.
// SettingsReader reads tenant settings for invoice configuration.
type SettingsReader interface {
	NextInvoiceNumber(ctx context.Context, tenantID string) (string, error)
	Get(ctx context.Context, tenantID string) (domain.TenantSettings, error)
}

type Engine struct {
	subs          SubscriptionReader
	usage         UsageAggregator
	pricing       PricingReader
	invoices      InvoiceWriter
	credits       CreditApplier
	creditGranter CreditGranter
	// #22: settle an UNPAID in_advance prebill at mid-period cancel.
	invoiceVoider      InvoiceVoider
	creditNoteAdjuster CreditNoteAdjuster
	creditHeadroom     CreditHeadroomReader
	settings           SettingsReader
	paymentSetups      PaymentReadiness
	charger            InvoiceCharger
	profiles           BillingProfileReader
	customers          CustomerReader
	taxProviders       TaxProviderResolver
	taxCalcStore       TaxCalculationWriter
	clock              clock.Clock
	testClocks         TestClockReader
	events             domain.EventDispatcher
	noPMNotifier       NoPaymentMethodNotifier
	dunningStarter     DunningStarter
	dunningResolver    DunningResolver
	cancelExecutor     ScheduledCancelExecutor
	auditLogger        AuditWriter
	txRunner           TxRunner
	// deferredThresholdNotes dedups the crossed-but-deferred loudness
	// artifact to once per (sub, period) — see noteThresholdDeferred.
	deferredThresholdNotes sync.Map

	// ADR-101 segment-source seam (intervals_reader.go): the snapshot
	// reader, the boot-frozen mode (off|shadow|on), and the parity
	// counters the corpus CI gate asserts on.
	intervalSnap IntervalSnapshotter
}

// TxRunner runs fn inside one tenant-scoped transaction — the coordinator-tx
// seam for engine writes that must commit across two domain stores atomically
// (threshold fire + cycle re-anchor). Production wires *postgres.DB (which
// satisfies this directly) via SetTxRunner; paths that need atomicity FAIL
// LOUDLY when it is nil rather than degrading to sequential writes.
type TxRunner interface {
	WithTenantTx(ctx context.Context, tenantID string, fn func(tx *sql.Tx) error) error
}

// SetTxRunner wires the coordinator-transaction seam (production: the
// *postgres.DB pool, in router.go).
func (e *Engine) SetTxRunner(r TxRunner) {
	e.txRunner = r
}

// AuditWriter is the narrow audit surface the engine needs. Defined
// here (not imported from internal/audit) so billing doesn't gain a
// reverse import of the audit package — production wires
// *audit.Logger via SetAuditLogger in router.go. Optional: nil = engine
// skips the audit write but still mutates state + dispatches webhooks.
type AuditWriter interface {
	Log(ctx context.Context, tenantID, action, resourceType, resourceID, resourceLabel string, metadata map[string]any) error
	// LogInTx writes the row on the caller's transaction (ADR-090 shared fate):
	// the row and the business mutation commit or roll back together. tenant_id,
	// actor and the sim axis are derived from ctx + the tx GUC, so the Entry
	// carries only the row's own facts. Used by the engine's finalize emission so
	// the audit row rides the invoice-create tx instead of a lossy post-commit
	// write. The concrete *audit.Logger satisfies this directly.
	LogInTx(ctx context.Context, tx *sql.Tx, e audit.Entry) error
}

// SetAuditLogger wires the audit logger used by the engine to record
// background-fired lifecycle events (scheduled cancellation, engine-
// finalized invoices, trial auto-expiry). Without this, those auto-fires
// only show in outbound webhooks and slog — the operator Activity feed
// and the audit log miss them.
func (e *Engine) SetAuditLogger(a AuditWriter) {
	e.auditLogger = a
}

// auditWriter exposes the engine's wired audit seam to the billing HTTP
// handler (same package). POST /v1/billing/run emits its operator-trigger row
// through the SAME logger the run's finalize rows use, so there is exactly ONE
// wiring point for the domain — engine.SetAuditLogger, already fail-closed at
// boot via audit.MustWired(engine) — and no second Set* line for the composition
// root to forget (the silent-un-audit failure mode ADR-090 exists to kill).
func (e *Engine) auditWriter() AuditWriter { return e.auditLogger }

// dispatchEvent enqueues an outbound webhook event and LOGS on failure rather
// than silently dropping it. Once the enqueue commits the dispatcher delivers
// durably (ADR-040 outbox); a non-nil error means the enqueue INSERT failed and
// the event is lost — make that visible (no-silent-fallbacks) instead of the
// prior `_ =` swallow. In-tx atomicity with the state change (DispatchTx) is the
// deferred follow-up; this only closes the silent-error vector.
func (e *Engine) dispatchEvent(ctx context.Context, tenantID, eventType string, payload map[string]any) {
	if e.events == nil {
		return
	}
	if err := e.events.Dispatch(ctx, tenantID, eventType, payload); err != nil {
		slog.ErrorContext(ctx, "failed to enqueue webhook event",
			"event_type", eventType, "tenant_id", tenantID, "error", err)
	}
}

// finalizeAuditEntry builds the finalize audit row for an invoice the engine
// created already-finalized (cycle close, subscription create, cancel-final,
// threshold), or returns ok=false for a draft.
//
// Drafts (tax-pending / pause-collection) are skipped: they finalize later via
// the tax-retry chain's service.Finalize, which writes the row there — auditing
// here too would double-count. tenant_id, actor and the sim axis are derived
// from ctx + the tx GUC inside LogInTx, so the Entry carries only the row's own
// facts (ADR-030: created_at stays wall-clock; the simulated instant is a
// derived column, not metadata).
//
// The operator HTTP path and the tax-retry auto-finalize get their row from
// invoice.Service.Finalize; engine invoices never pass through Finalize — they
// are born finalized — so without this they leave no audit trace at all, and the
// TTFI metric (MIN(created_at) of finalize rows) reports nothing for tenants
// whose first invoice is engine-generated (the normal path).
func finalizeAuditEntry(inv domain.Invoice) (audit.Entry, bool) {
	if inv.Status != domain.InvoiceFinalized {
		return audit.Entry{}, false
	}
	return audit.Entry{
		Action:        domain.AuditActionFinalize,
		ResourceType:  "invoice",
		ResourceID:    inv.ID,
		ResourceLabel: inv.InvoiceNumber,
		Metadata: map[string]any{
			"invoice_number":     inv.InvoiceNumber,
			"customer_id":        inv.CustomerID,
			"total_amount_cents": inv.TotalAmountCents,
			"currency":           inv.Currency,
			"triggered_by":       string(inv.BillingReason),
		},
	}, true
}

// emitFinalizeAuditTx writes the finalize audit row on tx (ADR-090 shared fate):
// it commits with the invoice or not at all. Returns nil for drafts and when no
// audit logger is wired (the logger is optional — nil = engine runs unaudited,
// same contract as the old post-commit path). This is THE fix for the residual
// own-tx post-commit Log, which left a finalized invoice visible with no record
// of what created it whenever that second write failed.
func (e *Engine) emitFinalizeAuditTx(ctx context.Context, tx *sql.Tx, inv domain.Invoice) error {
	if e.auditLogger == nil {
		return nil
	}
	entry, ok := finalizeAuditEntry(inv)
	if !ok {
		return nil
	}
	return e.auditLogger.LogInTx(ctx, tx, entry)
}

// finalizeAuditEmit adapts emitFinalizeAuditTx to the CreateInvoiceWithLineItemsAudited
// callback shape, so the own-tx finalize paths audit on the create tx. The store
// hands back the persisted invoice as `out` (with its real ID); audit that, not
// the pre-insert struct.
func (e *Engine) finalizeAuditEmit(ctx context.Context) func(tx *sql.Tx, out domain.Invoice) error {
	return func(tx *sql.Tx, out domain.Invoice) error {
		return e.emitFinalizeAuditTx(ctx, tx, out)
	}
}

// dispatchInvoiceFinalized fires the invoice.finalized WEBHOOK for an engine-born
// invoice. It runs POST-COMMIT and stays here (the audit row moved in-tx, ADR-090):
// a webhook is an external effect that must not ride a DB tx, and it is idempotent
// per invoice via the same skip-drafts gate.
//
// Engine-born invoices never pass service.Finalize, so until 2026-06-13 the normal
// billing path (cycle / create / cancel-final / threshold) emitted NO
// invoice.finalized at all — integrators only ever saw operator-clicked finalizes.
func (e *Engine) dispatchInvoiceFinalized(ctx context.Context, sub domain.Subscription, inv domain.Invoice) {
	if inv.Status != domain.InvoiceFinalized {
		return
	}
	if e.events != nil {
		e.dispatchEvent(ctx, sub.TenantID, domain.EventInvoiceFinalized, map[string]any{
			"invoice_id":         inv.ID,
			"invoice_number":     inv.InvoiceNumber,
			"customer_id":        inv.CustomerID,
			"status":             string(inv.Status),
			"payment_status":     string(inv.PaymentStatus),
			"total_amount_cents": inv.TotalAmountCents,
			"amount_due_cents":   inv.AmountDueCents,
			"currency":           inv.Currency,
		})
	}
}

// SetCreditGranter wires the credit-grant issuer used by BillOnCancel
// for cancel proration on in_advance plans (ADR-031). Optional —
// without it, in_advance subs cancel without a proration credit.
func (e *Engine) SetCreditGranter(g CreditGranter) {
	e.creditGranter = g
}

// SetInvoiceVoider / SetCreditNoteAdjuster wire the two issuers BillOnCancel
// uses to settle an UNPAID in_advance invoice at mid-period cancel (#22):
// void it when nothing was consumed, or reduce its amount_due to the consumed
// portion via an adjustment credit note. Both REQUIRED — boot fails closed if
// unwired (#442); the old "unwired → leave for dunning" fallback arms are
// deleted, so a missing issuer can never silently dun a customer for a
// canceled period again.
func (e *Engine) SetInvoiceVoider(v InvoiceVoider) {
	e.invoiceVoider = v
}

func (e *Engine) SetCreditNoteAdjuster(a CreditNoteAdjuster) {
	e.creditNoteAdjuster = a
}

func (e *Engine) SetCreditHeadroomReader(r CreditHeadroomReader) {
	e.creditHeadroom = r
}

// CustomerReader is the narrow read interface the engine uses to
// resolve a customer's test_clock_id pin (ADR-027). Implemented by
// *customer.PostgresStore. Optional — when nil, SimForCustomer
// falls back to wall-clock; this is the safe default for narrow unit
// tests that don't exercise customer-level clock pins.
type CustomerReader interface {
	Get(ctx context.Context, tenantID, id string) (domain.Customer, error)
}

// SetCustomerReader wires the customer reader used by
// SimForCustomer (and transitively SimForInvoice on
// one-off invoices). Production wires *customer.PostgresStore via
// api/router.go.
func (e *Engine) SetCustomerReader(r CustomerReader) {
	e.customers = r
}

// Compile-time assertion that *Engine satisfies clock.Resolver. Lets
// every domain bind effective-now via the platform/clock package
// without importing billing — clock owns the interface, billing owns
// the implementation. Replaces the per-service ClockResolver
// interfaces we threaded through dunning / subscription / invoice in
// the post-ADR-029 patches.
var _ clock.Resolver = (*Engine)(nil)

// NoPaymentMethodNotifier dispatches a customer-facing email when an
// invoice finalizes for a customer with no PaymentSetup ready. Without
// this, the customer would never know to add a card and the invoice
// would silently sit unpaid until it goes overdue and dunning fires
// — too late for happy-path collection. Stripe sends an equivalent
// "Action required: payment method needed" email at finalize for
// failed charges; we extend the same pattern to the no-PM case so the
// customer experience is symmetric.
//
// Optional — when nil, engine skips the notification (local dev,
// integration tests). Wire in router.go via SetNoPaymentMethodNotifier.
type NoPaymentMethodNotifier interface {
	NotifyNoPaymentMethod(ctx context.Context, tenantID string, inv domain.Invoice, trigger string) (domain.NotifyOutcome, error)
}

// DunningStarter enrolls a stuck invoice into a dunning campaign.
//
// Why this exists: a card DECLINE already starts dunning inline
// (ChargeInvoice → StartDunning), so the declined-card customer is
// escalated toward a terminal (pause/cancel/uncollectible). But an
// invoice with NO resolvable payment method is only flagged
// auto_charge_pending + emailed once, then retried by RetryPendingCharges
// forever with nothing to charge — it never reaches a terminal. That
// card-less "limbo" is an absorbing sink: unbounded unpaid invoices, no
// escalation. Routing the no-card invoice into the SAME dunning machine
// gives it the same escalation-to-terminal the declined-card path gets.
//
// StartDunning is idempotent (one run per invoice, lifetime), so calling
// it every tick for the same candidate is safe — an invoice that already
// has a run (e.g. from a prior decline) is a no-op. A "dunning disabled"
// outcome is a deliberate skip (the adapter swallows it), matching the
// declined-card path's behaviour when dunning is off.
//
// REQUIRED — boot fails closed if unwired (#442); the old nil skip left
// card-less invoices out of dunning forever. Wire in router.go via
// SetDunningStarter.
type DunningStarter interface {
	StartDunning(ctx context.Context, tenantID, invoiceID, customerID string, failureAt time.Time, cause domain.DunningStartCause) error
}

// DunningResolver resolves an active dunning run when an invoice settles via a
// BACKGROUND path — a credit-cover sweep or threshold-close that MarkPaids the
// invoice WITHOUT going through the invoice handler (which is the only place that
// resolved dunning before this). Without it, a paid invoice keeps an active
// dunning run: a stale red badge, and — worse — on the run's next due tick
// dunning.processRun could fire a dunning email or a TERMINAL action
// (pause-collection / subscription-cancel) on a fully-paid invoice. The resolve
// is idempotent (no active run → no-op via GetActiveRunByInvoice) so it's safe to
// call on every settle, and it is best-effort post-commit — a failure logs and
// never fails the settle; the dunning.processRun paid-pre-check is the durable
// backstop. REQUIRED — boot fails closed if unwired (#442). Wire via
// SetDunningResolver.
type DunningResolver interface {
	ResolveByInvoice(ctx context.Context, tenantID, invoiceID string, resolution domain.DunningResolution) error
}

// ScheduledCancelExecutor executes a scheduled cancel whose cancel_at fell
// strictly INSIDE a billing period (ADR-097). The engine's cycle scans
// detect due-ness (the cancel arm of the due queries); the subscription
// domain owns the transition — one tx composing the CAS flip
// (canceled_at = at), the final partial-period invoice, and the
// prepaid-relief credit drafts, then the post-commit finalize/issue legs.
// Implemented by subscription.Service.FireDueScheduledCancel; wired in
// router.go via SetScheduledCancelExecutor.
//
// REQUIRED — like DunningStarter, boot fails closed if unwired: a due
// mid-period cancel the loop cannot execute would otherwise re-fetch on
// every scan pass forever (the 2026-05-31 spin-bug shape), and the sub
// would keep renewing past its own cancellation (the ADR-097 money bug).
type ScheduledCancelExecutor interface {
	FireDueScheduledCancel(ctx context.Context, tenantID, id string, at time.Time) error
}

// TestClockReader looks up a test clock's frozen_time. The billing engine
// calls this for every subscription that has test_clock_id set, so the clock
// decides "what time is it for this sub?" instead of wall-clock. Returns
// errs.ErrNotFound when the clock has been deleted (caller treats missing
// clock as wall-clock — the detached sub quietly rejoins the live timeline).
type TestClockReader interface {
	Get(ctx context.Context, tenantID, id string) (domain.TestClock, error)
}

// CreditApplier applies customer credits to an invoice before charging.
// ApplyToInvoiceAt is the simulated-time-aware variant — engine callers
// pass their `now` (cycle close instant) so the ledger usage entry +
// invoice updated_at land on simulated time rather than advance-end
// frozen_time during catchup.
type CreditApplier interface {
	ApplyToInvoiceAt(ctx context.Context, tenantID, customerID, invoiceID string, amountCents int64, at time.Time, invoiceNumber ...string) (int64, error)
}

// CreditGranter issues a new credit grant. Used by BillOnCancel
// (ADR-031 slice 3) to refund the unused portion of an already-
// billed in_advance period to the customer's credit balance.
// Implemented by *credit.Service.
type CreditGranter interface {
	Grant(ctx context.Context, tenantID string, input credit.GrantInput) (domain.CreditLedgerEntry, error)
}

// InvoiceVoider annuls a finalized, unpaid invoice. Used by BillOnCancel (#22)
// to void an in_advance prebill that the customer never consumed any of when
// they cancel mid-period. Implemented by *invoice.Service (refuses paid and
// already-voided invoices, reverses upstream tax). Narrow on purpose so billing
// keeps coordinating through a domain-typed interface, not an invoice import.
type InvoiceVoider interface {
	Void(ctx context.Context, tenantID, id string) (domain.Invoice, error)
}

// CreditNoteAdjuster reduces an unpaid finalized invoice's amount_due by issuing
// an adjustment credit note. Used by BillOnCancel (#22) to settle a partially-
// consumed unpaid in_advance prebill down to the consumed portion (the unused
// fraction is removed from the receivable, proportional tax reversed). The
// amount is gross (tax-inclusive). Implemented by *creditnote.Service.
type CreditNoteAdjuster interface {
	CreateAndIssueAdjustment(ctx context.Context, tenantID, invoiceID string, grossCents int64, reason, description string) (domain.CreditNote, error)
	// CreateAdjustmentDraftTx creates the adjustment credit note as an
	// issue_pending DRAFT on the caller's coordinator tx (ADR-057), to be issued
	// post-commit. Used by the atomic cancel-proration-credit path.
	CreateAdjustmentDraftTx(ctx context.Context, tx *sql.Tx, tenantID, invoiceID string, grossCents int64, reason, description string) (domain.CreditNote, error)
	// Issue issues a previously-created draft credit note (the post-commit relay
	// of CreateAdjustmentDraftTx). Both already exist on *creditnote.Service.
	Issue(ctx context.Context, tenantID, id string) (domain.CreditNote, error)
}

// CreditHeadroomReader reports how much of an invoice has already been credited
// (sum of non-voided credit-note totals). OPTIONAL — when wired, the
// multi-invoice credit fan-out caps each funding invoice's share at its
// REMAINING creditable headroom (TotalAmountCents - credited) and water-fills
// the overflow onto invoices that still have room, so a prior credit note (e.g.
// an earlier downgrade clawback) can't make a later cancel/swap overrun a single
// invoice's credit-note cap. Unwired (narrow tests) → headroom defaults to the
// full invoice total, i.e. the pre-headroom-aware behavior. Implemented by
// *creditnote.Service.CreditedCents.
type CreditHeadroomReader interface {
	CreditedCents(ctx context.Context, tenantID, invoiceID string) (int64, error)
}

// BillingProfileReader reads customer billing profiles for tax exemption checks.
type BillingProfileReader interface {
	GetBillingProfile(ctx context.Context, tenantID, customerID string) (domain.CustomerBillingProfile, error)
}

// PaymentReadiness resolves the data the engine needs to decide
// "can we auto-charge this customer's invoice right now?" Two values
// in one call — the Stripe Customer ID (required as the PI customer
// param) plus the Stripe PaymentMethod id of the customer's active
// default card (empty when there's no chargeable default). The same
// resolved PM id is threaded into the charge so the card we gate on is
// the exact card charged — Velox is the single source of truth for PM
// selection (we never let Stripe pick). Reads customers.stripe_customer_id
// for the mapping and the default payment_methods row canonically.
type PaymentReadiness interface {
	ResolveForCharge(ctx context.Context, tenantID, customerID string) (stripeCustomerID string, stripePaymentMethodID string, err error)
}

// InvoiceCharger creates a Stripe PaymentIntent for a finalized invoice.
// Narrow interface — default-mode charge only. Callers that need to
// tag the PI for special routing (the dunning retrier marks PIs with
// velox_purpose=dunning_retry so the webhook suppresses the duplicate
// payment-failed email) access *payment.Stripe directly and use the
// dedicated typed method (ChargeInvoiceForDunningRetry). Keeps engine
// package free of payment-options surface.
type InvoiceCharger interface {
	ChargeInvoice(ctx context.Context, tenantID string, inv domain.Invoice, stripeCustomerID, stripePaymentMethodID string) (domain.Invoice, error)
}

// SubscriptionReader reads subscription and plan data for billing.
type SubscriptionReader interface {
	// GetDueBilling: wall-clock cron path. Returns ONLY subs that
	// are NOT pinned to a test clock (ADR-028 disjoint flows). The
	// scheduler tick fans this out per livemode.
	GetDueBilling(ctx context.Context, before time.Time, limit int) ([]domain.Subscription, error)
	// GetDueBillingForClock: operator-driven catchup path. Returns
	// ONLY subs pinned to the given clock whose next_billing_at is
	// on-or-before that clock's frozen time. Called by RunCatchup
	// after Advance flips status to 'advancing'.
	GetDueBillingForClock(ctx context.Context, tenantID, clockID string, limit int) ([]domain.Subscription, error)
	// GetDueBillingForTenant: operator-triggered (POST /v1/billing/run)
	// path, scoped to ONE tenant under RLS. Wall-clock-due, non-clock-
	// pinned subs for the caller's tenant only — never cross-tenant.
	GetDueBillingForTenant(ctx context.Context, tenantID string, before time.Time, limit int) ([]domain.Subscription, error)
	Get(ctx context.Context, tenantID, id string) (domain.Subscription, error)
	UpdateBillingCycle(ctx context.Context, tenantID, id string, periodStart, periodEnd, nextBillingAt time.Time, anchorDay int) error
	// UpdateBillingCycleTx is the in-tx variant — used by fireThreshold's
	// reset=true arm so the threshold invoice insert and the cycle re-anchor
	// commit atomically (ADR-066): a crash between the two otherwise strands
	// the reset forever, because the fire-once probe blocks every retry.
	UpdateBillingCycleTx(ctx context.Context, tx *sql.Tx, tenantID, id string, periodStart, periodEnd, nextBillingAt time.Time, anchorDay int) error
	// ApplyDuePendingItemPlansAtomic swaps plan_id ← pending_plan_id for every
	// item on the subscription whose pending_plan_effective_at <= now, in one
	// statement. Returns the refreshed items (including any that weren't due,
	// untouched). Called at the cycle boundary so the just-closed period is
	// billed on the pre-change plan and the next period uses the new plan.
	// Returns nil + no error when no items are due (caller proceeds with the
	// existing plan).
	ApplyDuePendingItemPlansAtomic(ctx context.Context, tenantID, subscriptionID string, now time.Time) ([]domain.SubscriptionItem, error)

	// FireScheduledCancellation transitions a sub with a due cancel_at or
	// cancel_at_period_end intent to canceled in one statement. Called by
	// the cycle scan after invoice generation, instead of UpdateBillingCycle,
	// when the schedule fields say it's time. The `at` argument is the
	// engine's effectiveNow so canceled_at stays consistent with test-
	// clock-driven time travel.
	FireScheduledCancellation(ctx context.Context, tenantID, id string, at time.Time) (domain.Subscription, error)

	// ActivateAfterTrialWithBill atomically transitions a sub from
	// 'trialing' to 'active' AND runs billFn (the day-1 in_advance invoice)
	// in the SAME tx (ADR-056 — the old post-flip bill was a revenue leak on
	// failure). The UPDATE refuses when a cancel schedule is due at trial
	// end and returns domain.ErrTrialCancelDue (ADR-069) — the caller routes
	// to CancelAtTrialEnd instead of billing a customer who canceled.
	// Already-active rows return InvalidState (caller re-reads and routes).
	ActivateAfterTrialWithBill(ctx context.Context, tenantID, id string, at time.Time, billFn func(tx *sql.Tx, activated domain.Subscription) error) (domain.Subscription, error)
	// CancelAtTrialEnd is the dedicated FREE trialing→canceled transition
	// (ADR-069): no invoice, canceled_at = trial_end_at, CAS on the observed
	// trial end + a due schedule (domain.ErrTrialCancelConflict on a lost
	// race — the sub is not handled this pass).
	CancelAtTrialEnd(ctx context.Context, tenantID, id string, observedTrialEnd time.Time) (domain.Subscription, error)

	// ListWithThresholds returns active+trialing subscriptions in the given
	// livemode partition that have at least one billing threshold configured
	// (amount or per-item). Drives the threshold scan tick — each row is
	// rated against its current partial-cycle running totals to decide
	// whether to fire an early finalize. Hydrated with Items and
	// BillingThresholds so the scan doesn't issue per-sub follow-up reads.
	// afterID is the drain cursor: only subs with id > afterID are returned
	// (ORDER BY id), "" starts from the beginning. ScanThresholds pages with
	// it until a short batch so the WHOLE candidate set is scanned every tick
	// — a plain LIMIT would silently disable spend caps past the first batch.
	ListWithThresholds(ctx context.Context, livemode bool, afterID string, limit int) ([]domain.Subscription, error)
	// ListWithThresholdsForClock is the catchup-path counterpart to
	// ListWithThresholds — returns clock-pinned subs with thresholds
	// configured. ADR-029 Phase 3.
	ListWithThresholdsForClock(ctx context.Context, tenantID, clockID, afterID string, limit int) ([]domain.Subscription, error)

	// ListItemChangesInPeriod returns every subscription_item_changes row
	// for the given subscription whose changed_at falls within
	// (periodStart, periodEnd]. Drives segment-aware base-fee billing at
	// cycle close — each row marks a boundary in the period at which the
	// plan or quantity changed, and each [boundary_n, boundary_{n+1}]
	// segment is billed at its own rate × duration. Lago / Chargebee /
	// Orb shape for mid-period proration.
	ListItemChangesInPeriod(ctx context.Context, tenantID, subscriptionID string, periodStart, periodEnd time.Time) ([]domain.SubscriptionItemChange, error)
}

// UsageAggregator aggregates usage events for a billing period. Returns
// decimal.Decimal so fractional AI-usage primitives (GPU-hours, cached-token
// ratios) round-trip without precision loss; the engine converts to cents
// at the multiplication step.
//
// AggregateByPricingRules is the multi-dim-aware path (priority+claim
// LATERAL JOIN across the 5 aggregation modes). The cycle scan, the
// customer-usage endpoint, and the create_preview surface all call it —
// preview math == invoice math by construction.
type UsageAggregator interface {
	AggregateForBillingPeriodByAgg(ctx context.Context, tenantID, customerID string, meters map[string]string, from, to time.Time) (map[string]decimal.Decimal, error)
	AggregateByPricingRules(ctx context.Context, tenantID, customerID, meterID string, defaultMode domain.AggregationMode, from, to time.Time) ([]domain.RuleAggregation, error)
}

// PricingReader reads plan, rating rule, and override data.
//
// ListMeterPricingRulesByMeter is needed by the preview path to echo each
// rule's DimensionMatch (the canonical pricing identity) onto the
// per-rule preview line.
type PricingReader interface {
	GetPlan(ctx context.Context, tenantID, id string) (domain.Plan, error)
	GetMeter(ctx context.Context, tenantID, id string) (domain.Meter, error)
	GetRatingRule(ctx context.Context, tenantID, id string) (domain.RatingRuleVersion, error)
	// GetRuleByKeyAsOf is the ONE version-resolution rule every rating
	// path shares (ADR-070 pin-at-period-start): the version in force
	// when the billing period opened prices the whole period — close,
	// cancel finalize, threshold fire, and preview all agree by
	// construction, and a mid-close publish can't mix versions inside
	// one invoice.
	GetRuleByKeyAsOf(ctx context.Context, tenantID, ruleKey string, asOf time.Time) (domain.RatingRuleVersion, error)
	// GetOverrideByKeyAsOf resolves the customer's negotiated price in
	// force at the period open. errs.ErrNotFound = no override (list
	// price); ANY other error aborts the rating path — a transient DB
	// failure must never silently bill list price (ADR-070,
	// no-silent-fallbacks).
	GetOverrideByKeyAsOf(ctx context.Context, tenantID, customerID, ruleKey string, asOf time.Time) (domain.CustomerPriceOverride, error)
	ListMeterPricingRulesByMeter(ctx context.Context, tenantID, meterID string) ([]domain.MeterPricingRule, error)
}

// InvoiceWriter creates invoices and line items.
type InvoiceWriter interface {
	CreateInvoiceWithLineItems(ctx context.Context, tenantID string, inv domain.Invoice, items []domain.InvoiceLineItem) (domain.Invoice, error)
	// CreateInvoiceWithLineItemsAudited is CreateInvoiceWithLineItems with an
	// in-transaction audit emission (ADR-090): emit runs on the SAME tx as the
	// header + line INSERTs, after they land and BEFORE commit, so an emission
	// error rolls the whole invoice back — the finalize row and the invoice share
	// fate. The own-tx engine finalize paths (cycle close, subscription create,
	// cancel-final) route through here so a finalized invoice can never commit
	// without its audit trail (the residual own-tx post-commit Log left an
	// invoice visible with no record of what created it if the write failed).
	CreateInvoiceWithLineItemsAudited(ctx context.Context, tenantID string, inv domain.Invoice, items []domain.InvoiceLineItem, emit func(tx *sql.Tx, out domain.Invoice) error) (domain.Invoice, error)
	// CreateInvoiceWithLineItemsTx is the in-tx variant — used by BillOnCreateTx
	// so the cross-interval plan-swap can insert the new-period invoice on the
	// coordinator's tx, atomically with the plan write + watermark advance. The
	// finalize audit row rides this same tx too (emitted by the caller right
	// after the insert), so it commits atomically with the swap.
	CreateInvoiceWithLineItemsTx(ctx context.Context, tx *sql.Tx, tenantID string, inv domain.Invoice, items []domain.InvoiceLineItem) (domain.Invoice, error)
	GetInvoice(ctx context.Context, tenantID, id string) (domain.Invoice, error)
	// FindFundingInvoicesForPeriod returns EVERY non-voided/non-uncollectible
	// invoice that prepaid the period — the day-1 base invoice AND any
	// mid-period upgrade / quantity-increase proration invoice issued against
	// the same cycle. A period-wide unused credit (cancel / cross-interval
	// plan-swap / downgrade clawback) is sized off the post-change base, which
	// can exceed the single base invoice's total; fanning the credit across the
	// full funding set lets each piece stay within its own invoice's
	// credit-note cap. ErrNotFound when nothing funded the period (trial / pure
	// in_arrears) so callers keep their existing no-op behavior.
	FindFundingInvoicesForPeriod(ctx context.Context, tenantID, subscriptionID string, periodStart, periodEnd time.Time) ([]domain.Invoice, error)
	// GetLatestThresholdInvoiceForCycle returns the newest non-voided
	// threshold-fired invoice whose billing_period_start falls inside
	// [periodStart, periodEnd) — the same predicate as
	// LatestThresholdPeriodEnd but the full row, so the fire-once probe can
	// also HEAL a crash between invoice-create and the $0/credited MarkPaid
	// (ADR-066). errs.ErrNotFound when the cycle has no threshold invoice.
	GetLatestThresholdInvoiceForCycle(ctx context.Context, tenantID, subscriptionID string, periodStart, periodEnd time.Time) (domain.Invoice, error)
	// GetInvoiceForPeriod returns the newest non-voided invoice keyed exactly
	// like idx_invoices_billing_idempotency (subscription, period_start,
	// period_end) — the row billOnePeriod's ErrAlreadyExists re-entry path
	// heals ($0 MarkPaid crash window, ADR-066).
	GetInvoiceForPeriod(ctx context.Context, tenantID, subscriptionID string, periodStart, periodEnd time.Time) (domain.Invoice, error)
	ListLineItems(ctx context.Context, tenantID, invoiceID string) ([]domain.InvoiceLineItem, error)
	MarkPaid(ctx context.Context, tenantID, id string, stripePaymentIntentID string, paidAt time.Time) (domain.Invoice, error)
	SetAutoChargePending(ctx context.Context, tenantID, id string, pending bool) error
	// SetNoPMNotifiedAt stamps the send-once marker for the no-PM setup-link
	// email (ADR-087 follow-up): whoever delivers the email stamps it; the
	// auto-charge sweep checks it so a card-less invoice is emailed exactly
	// once, not once per tick.
	SetNoPMNotifiedAt(ctx context.Context, tenantID, invoiceID string, at time.Time) error
	// ClaimAutoCharge takes the per-invoice charge lease (HA hazard #1):
	// CAS re-asserting the full eligibility predicate; false = another
	// leader owns the invoice or its state moved on — skip, never charge.
	ClaimAutoCharge(ctx context.Context, tenantID, id string) (bool, error)
	// ReleaseAutoChargeClaim clears the lease on provably-pre-Stripe skip
	// paths so the next tick/Advance retries immediately.
	ReleaseAutoChargeClaim(ctx context.Context, tenantID, id string) error
	ListAutoChargePending(ctx context.Context, limit int) ([]domain.Invoice, error)
	// ListAutoChargePendingForClock is the catchup-path counterpart to
	// ListAutoChargePending — returns invoices whose owning subscription
	// is pinned to the given clock. ADR-029 Phase 1: simulation-time
	// charge attempts only fire on operator Advance, never on the
	// wall-clock cron tick, mirroring Stripe Test Clocks.
	ListAutoChargePendingForClock(ctx context.Context, tenantID, clockID string, limit int) ([]domain.Invoice, error)
	// ListFailedWithoutDunningRun powers the dunning_backfill reconciler:
	// finalized, still-owed invoices in payment_status='failed' with NO
	// dunning run — the SettleFailed post-commit crash / exhausted-retry
	// window. olderThan is the cool-off.
	ListFailedWithoutDunningRun(ctx context.Context, olderThan time.Time, limit int) ([]domain.Invoice, error)
	// SetTaxTransaction persists the upstream provider's tax_transaction
	// reference (Stripe: tx_xxx) after CommitTax succeeds. Required for
	// later reversal when a credit note is issued against the invoice.
	SetTaxTransaction(ctx context.Context, tenantID, id string, taxTransactionID string) error
	// UpdateTaxAtomic re-stamps an invoice's tax decision after a manual
	// retry. Used by Engine.RetryTaxForInvoice to persist the recomputed
	// per-line and invoice-level tax fields atomically.
	UpdateTaxAtomic(ctx context.Context, tenantID, invoiceID string, update domain.InvoiceTaxRetryUpdate, lineItems []domain.InvoiceLineItem) (domain.Invoice, error)
}

// MustValidate panics unless every collaborator field on the Engine is
// wired, naming ALL nil fields in one message. The composition root calls it
// after the last Set* — so a production boot with a missing collaborator
// fails closed at startup instead of silently changing money math at 3am
// (the 2026-07-10 design review found ~10 prod paths whose nil-collaborator
// arms diverge on money: a nil creditGranter silently skips a cancel refund,
// a nil creditNoteAdjuster skips a tax reversal, a nil creditHeadroom
// disables the #276-#278 overcharge cap).
//
// Why boot-time and not construction-time: the engine and its peer services
// are mutually referential (invoiceSvc needs the engine as clock resolver;
// the engine needs invoiceSvc as invoice voider), so a fully-populated
// construction-time deps struct is not achievable without restructuring the
// services — the setters exist BECAUSE of that cycle. Boot-validate is the
// minimal complete mechanism: same guarantee, no restructuring. Tests that
// construct partial engines deliberately do not call this.
//
// Reflection walks the struct's interface fields so a future 26th
// collaborator cannot be forgotten here (drift-guarded by
// TestMustValidate_CoversEveryCollaborator).
func (e *Engine) MustValidate() {
	v := reflect.ValueOf(e).Elem()
	t := v.Type()
	var missing []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.Type.Kind() != reflect.Interface {
			continue // sync.Map bookkeeping etc. — collaborators are interfaces
		}
		if v.Field(i).IsNil() {
			missing = append(missing, f.Name)
		}
	}
	if len(missing) > 0 {
		panic(fmt.Sprintf("billing.Engine misconfigured — nil collaborators: %s (every field must be wired before serving; see MustValidate doc)", strings.Join(missing, ", ")))
	}
}

func NewEngine(subs SubscriptionReader, usage UsageAggregator, pricing PricingReader, invoices InvoiceWriter, credits CreditApplier, settings SettingsReader, paymentSetups PaymentReadiness, charger InvoiceCharger, clk clock.Clock, profiles ...BillingProfileReader) *Engine {
	if clk == nil {
		clk = clock.Real()
	}
	e := &Engine{subs: subs, usage: usage, pricing: pricing, invoices: invoices, credits: credits, settings: settings, paymentSetups: paymentSetups, charger: charger, clock: clk}
	if len(profiles) > 0 {
		e.profiles = profiles[0]
	}
	return e
}

// TaxProviderResolver returns the Provider implementation for a given
// tenant's settings. Injected into the engine so billing doesn't need to
// know about Stripe keys or the tenant's provider choice directly.
type TaxProviderResolver interface {
	Resolve(ctx context.Context, ts domain.TenantSettings) (tax.Provider, error)
}

// TaxCalculationWriter persists a provider calculation to the
// tax_calculations audit table. Separate interface so engine tests can
// skip persistence without wiring a full postgres store.
//
// LookupCalculationCreatedAt powers the CommitTax expiry guard.
// Stripe Tax calculations expire 24h after creation — operator-finalized
// drafts held longer than that quietly fail at commit time and leave
// the invoice with tax_calculation_id but no tax_transaction_id (i.e.
// Stripe Tax reporting silently broken). Returning the calc's age lets
// CommitTax fail loud before calling Stripe so the operator knows to
// retry tax first. Returns errs.ErrNotFound when no row matches —
// caller treats as "skip the guard" (manual / none provider produces
// no row; defensive against tests that wire a fake store without the
// row pre-loaded).
type TaxCalculationWriter interface {
	Record(ctx context.Context, tenantID, invoiceID string, req tax.Request, res *tax.Result) (string, error)
	LookupCalculationCreatedAt(ctx context.Context, tenantID, invoiceID, providerRef string) (time.Time, error)
	// LinkInvoice backfills invoice_id on the audit row once the invoice
	// exists. Record runs before the invoice is persisted (invoice_id NULL);
	// CommitTax calls this with the known id so audit joins + the expiry
	// guard's invoice_id filter resolve.
	LinkInvoice(ctx context.Context, tenantID, invoiceID, providerRef string) error
}

// taxCalculationMaxAge is the Stripe Tax calculation window. Stripe's
// own docs put it at 24h; we use 23h to leave a safety buffer so a
// near-expiry calc isn't sent to Stripe just to bounce. Operator
// guidance on expiry: "Retry tax to refresh, then finalize."
const taxCalculationMaxAge = 23 * time.Hour

// SetTaxProviderResolver wires the per-tenant tax provider resolver. When
// nil, the engine skips tax calculation entirely and invoices carry zero
// tax (same behaviour as tax_provider='none').
func (e *Engine) SetTaxProviderResolver(r TaxProviderResolver) {
	e.taxProviders = r
}

// SetTaxCalculationStore wires the audit-trail writer for tax calculations.
// Optional — engine still works without it, but nothing is persisted to
// tax_calculations and post-hoc audit is limited to whatever Stripe keeps.
func (e *Engine) SetTaxCalculationStore(s TaxCalculationWriter) {
	e.taxCalcStore = s
}

// CommitTax resolves the tenant's tax provider and commits the named
// calculation to a tax_transaction. Used after invoice finalize to
// finalize Stripe Tax reporting. Returns nil when no resolver is wired
// or the tenant's settings can't be loaded — the invoice still exists,
// so the caller should log but not unwind.
//
// On success, the returned upstream transaction id (Stripe: tx_xxx) is
// persisted onto the invoice so a later credit note can issue a tax
// reversal against it. Persistence failures are logged but non-fatal —
// the tax is still committed upstream and the calculation row + Stripe
// dashboard let an operator reconcile manually.
func (e *Engine) CommitTax(ctx context.Context, tenantID, invoiceID, calculationID string) error {
	if e.taxProviders == nil || e.settings == nil {
		return nil
	}
	// Pin tenant on ctx — see ApplyTaxToLineItems for the rationale.
	ctx = auth.WithTenantID(ctx, tenantID)
	ts, err := e.settings.Get(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("load tenant settings: %w", err)
	}
	provider, err := e.taxProviders.Resolve(ctx, ts)
	if err != nil {
		return fmt.Errorf("resolve provider: %w", err)
	}
	if provider == nil {
		return nil
	}
	// Expiry guard. Stripe Tax calculations are valid for 24h after
	// creation; operator-finalized drafts held longer fail at Stripe's
	// CreateFromCalculation with "calculation expired" and previously
	// left the invoice with tax_calculation_id but no
	// tax_transaction_id. Fail loud here so the operator sees a clear
	// "retry tax to refresh" message instead of a silent reporting gap.
	// Skipped when the store isn't wired (engine tests) — the store
	// row IS the source of truth for calc creation time, and tests
	// without it shouldn't be gated by a guard they didn't opt into.
	if e.taxCalcStore != nil && calculationID != "" {
		// Backfill invoice_id on the audit row now that the invoice is
		// persisted — Record wrote it during tax application before the
		// invoice existed. Best-effort: a failure here only degrades the
		// expiry-guard lookup below to "not found → skip", never blocks
		// finalize.
		if linkErr := e.taxCalcStore.LinkInvoice(ctx, tenantID, invoiceID, calculationID); linkErr != nil {
			slog.Warn("tax: failed to link invoice to tax_calculations row",
				"error", linkErr, "tenant_id", tenantID, "invoice_id", invoiceID,
				"calculation_id", calculationID)
		}
		createdAt, lookupErr := e.taxCalcStore.LookupCalculationCreatedAt(ctx, tenantID, invoiceID, calculationID)
		if lookupErr == nil {
			// REAL wall-clock on BOTH sides — never clock.Now(ctx). The
			// calc's 24h TTL lives at Stripe, which knows nothing about a
			// Velox test clock; the row's created_at is stamped by the DB
			// (now(), real time). Using the ctx-bound simulated clock here
			// conflated the two: a customer pinned to a test clock advanced
			// >23h past real wall-clock (e.g. a mid-period proration after
			// advancing the clock 15 days) made clock.Now(ctx) - createdAt
			// exceed the window, so the guard falsely "expired" a calc that
			// was created seconds ago in real time. The commit was skipped
			// and the proration invoice kept tax_calculation_id but no
			// tax_transaction_id — and the wall-clock reconciler excludes
			// clock-pinned invoices, so nothing recovered it. Real elapsed
			// time is the only correct measure of a Stripe-side TTL.
			if age := time.Now().UTC().Sub(createdAt); age > taxCalculationMaxAge { // wall-clock: Stripe's calc TTL is a real-world 24h, never the simulated clock
				return errs.InvalidState(fmt.Sprintf(
					"tax calculation expired (age %s, max %s) — retry tax to refresh, then finalize",
					age.Truncate(time.Minute), taxCalculationMaxAge))
			}
		} else if !errors.Is(lookupErr, errs.ErrNotFound) {
			// Lookup failure on a real DB error: log and fall through.
			// Better to attempt commit and let Stripe reject than to
			// block finalize on a transient DB blip.
			slog.Warn("tax: expiry-guard lookup failed; attempting commit anyway",
				"error", lookupErr, "tenant_id", tenantID, "invoice_id", invoiceID,
				"calculation_id", calculationID)
		}
	}
	txID, err := provider.Commit(ctx, calculationID, invoiceID)
	if err != nil {
		return err
	}
	if txID != "" {
		if err := e.invoices.SetTaxTransaction(ctx, tenantID, invoiceID, txID); err != nil {
			slog.Warn("tax: commit succeeded but persisting tax_transaction_id failed",
				"error", err, "tenant_id", tenantID, "invoice_id", invoiceID,
				"tax_transaction_id", txID)
		}
	}
	return nil
}

// ReverseTax resolves the tenant's tax provider and issues a reversal
// against a previously committed tax_transaction. Called from the credit
// note issue path after the refund / credit-grant / invoice-reduction
// side-effects have succeeded. Returns an empty ReversalResult when no
// resolver is wired, no provider is configured, or the provider has no
// durable upstream state (none, manual) — the caller treats an empty
// TransactionID as "nothing to record" and proceeds.
func (e *Engine) ReverseTax(ctx context.Context, tenantID string, req tax.ReversalRequest) (*tax.ReversalResult, error) {
	if e.taxProviders == nil || e.settings == nil {
		return &tax.ReversalResult{}, nil
	}
	// Pin tenant on ctx — see ApplyTaxToLineItems for the rationale.
	ctx = auth.WithTenantID(ctx, tenantID)
	ts, err := e.settings.Get(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("load tenant settings: %w", err)
	}
	provider, err := e.taxProviders.Resolve(ctx, ts)
	if err != nil {
		return nil, fmt.Errorf("resolve provider: %w", err)
	}
	if provider == nil {
		return &tax.ReversalResult{}, nil
	}
	return provider.Reverse(ctx, req)
}

// SetTestClockReader wires the test-clock resolver. Optional: when nil, the
// engine always uses wall-clock time, even for subs with test_clock_id set
// (useful in narrow unit tests that don't exercise test-mode timing).
func (e *Engine) SetTestClockReader(r TestClockReader) {
	e.testClocks = r
}

// SetEventDispatcher wires the outbound webhook dispatcher. The engine emits
// subscription.pending_change.applied at the cycle boundary when a scheduled
// item plan change rolls into effect; without a dispatcher that event is
// dropped (acceptable for narrow billing unit tests).
func (e *Engine) SetEventDispatcher(d domain.EventDispatcher) {
	e.events = d
}

// SetNoPaymentMethodNotifier wires the customer-notification dispatcher
// fired when an invoice finalizes without a PaymentSetup ready. See
// the NoPaymentMethodNotifier doc-comment for the full rationale.
func (e *Engine) SetNoPaymentMethodNotifier(n NoPaymentMethodNotifier) {
	e.noPMNotifier = n
}

// SetDunningStarter wires the no-payment-method dunning enroller — the
// sweep that routes card-less auto_charge_pending invoices into dunning
// so they reach a terminal instead of looping in RetryPendingCharges
// forever. See the DunningStarter doc-comment for the full rationale.
func (e *Engine) SetDunningStarter(d DunningStarter) {
	e.dunningStarter = d
}

// SetDunningResolver wires the resolver that closes an active dunning run when an
// invoice settles via a background path (credit-cover sweep / threshold close).
// See the DunningResolver doc-comment.
func (e *Engine) SetDunningResolver(d DunningResolver) {
	e.dunningResolver = d
}

// SetScheduledCancelExecutor wires the ADR-097 mid-period scheduled-cancel
// executor. See the ScheduledCancelExecutor doc-comment.
func (e *Engine) SetScheduledCancelExecutor(x ScheduledCancelExecutor) {
	e.cancelExecutor = x
}

// resolveDunningRecovered best-effort resolves an active dunning run for an
// invoice that just settled via a BACKGROUND path that bypasses the invoice
// handler's dunning resolve. Idempotent state-correctness (no active run →
// no-op), NOT money-exactly-once: post-commit, a failure logs and never fails the
// settle (the dunning.processRun paid-pre-check is the durable backstop).
func (e *Engine) resolveDunningRecovered(ctx context.Context, tenantID, invoiceID string) {
	// dunningResolver is REQUIRED (boot fails closed, #442) — the old nil
	// no-op left a recovered invoice's dunning run active until the
	// processRun paid-pre-check caught it.
	if err := e.dunningResolver.ResolveByInvoice(ctx, tenantID, invoiceID, domain.ResolutionPaymentRecovered); err != nil {
		slog.WarnContext(ctx, "failed to resolve dunning after background credit settle",
			"invoice_id", invoiceID, "error", err)
	}
}

// shouldFireScheduledCancel reports whether a sub's soft-cancel intent has
// caught up with the current cycle tick. Two trigger conditions, OR'd:
//
//   - cancel_at_period_end=true and the period we just billed has ended
//     (periodEnd <= now) — by construction this is true at every invocation
//     since billSubscription is only entered when next_billing_at <= now,
//     but we keep the guard explicit so the helper doesn't depend on
//     caller invariants.
//
//   - cancel_at <= now — a specific timestamp the cycle has crossed.
//
// The check is intentionally placed after invoice generation so the just-
// ended period bills normally before the sub transitions to canceled
// (matching Stripe: the final invoice goes out, then the sub ends).
func shouldFireScheduledCancel(sub domain.Subscription, periodEnd, now time.Time) bool {
	if sub.CancelAtPeriodEnd && !periodEnd.After(now) {
		return true
	}
	if sub.CancelAt != nil && !sub.CancelAt.After(now) {
		return true
	}
	return false
}

// baseSegment is one [start, end] slice of a period during which an
// item had a fixed plan + quantity. Segment-aware base-fee billing
// emits one line per segment at the segment's own plan rate × duration
// fraction, matching the Lago / Chargebee / Orb shape for mid-period
// proration. Single-segment items (no mid-period changes) collapse
// to the pre-segment-aware single-line behavior.
type baseSegment struct {
	start, end time.Time
	planID     string
	quantity   int64
}

// usageInterval is one [start, end) range during which a meter was
// active for the subscription. Built per-segment from the same
// windowSegments derivation used for base-fee billing — Orb-shape
// segment-aware metering. Multiple items with overlapping segments on
// the same meter get merged so the meter is billed once per disjoint
// active range, not double-counted per item.
type usageInterval struct {
	start, end time.Time
}

// mergeUsageIntervals collapses overlapping or touching ranges into
// disjoint intervals. Sorted by start. Keeps the segment-aware usage
// loop from emitting two overlapping lines for the same meter when
// two items happen to share a segment range.
func mergeUsageIntervals(ivs []usageInterval) []usageInterval {
	if len(ivs) == 0 {
		return nil
	}
	sort.Slice(ivs, func(i, j int) bool { return ivs[i].start.Before(ivs[j].start) })
	out := []usageInterval{ivs[0]}
	for _, iv := range ivs[1:] {
		last := &out[len(out)-1]
		// Touching or overlapping intervals merge — covers the
		// boundary-swap case where seg1.end == seg2.start.
		if !iv.start.After(last.end) {
			if iv.end.After(last.end) {
				last.end = iv.end
			}
			continue
		}
		out = append(out, iv)
	}
	return out
}

// emitBaseSegmentLine pushes a base-fee line item for one [start, end]
// segment of an in_arrears period. Prorated against the cycle length
// (full plan-interval cycle from periodStart) so a 14-day segment of
// a 30-day cycle bills at 14/30 of the segment's plan amount. Single-
// segment items (no mid-period changes) collapse to "segment ==
// period" math, matching the pre-segment-aware single-line path.
//
// The denominator's anchor is derived from THE PERIOD BEING BILLED
// (AnchorDayFor(periodStart)), never from the subscription's live
// billing_anchor_day: a cross-interval swap rewrites the sub's anchor
// before the truncated old period closes, and the mutated anchor
// stretched the old period's denominator (found live, FLOW B21
// 2026-07-21 — a calendar-monthly period closed at "15/46 days"
// instead of 15/31, silently under-billing every anchor-changing
// arrears swap). Residual ambiguity: a period whose start was
// month-end-CLAMPED from a higher anchor reconstructs the clamped
// day (a ≤3-day denominator error in that one rare combination) —
// strictly narrower than the up-to-half-cycle error it replaces.
func emitBaseSegmentLine(seg baseSegment, plan domain.Plan, periodStart time.Time, periodDays int, currency string, loc *time.Location, billingTime domain.SubscriptionBillingTime, lineItems *[]domain.InvoiceLineItem, subtotal *int64) {
	segDays := roundDays(seg.end.Sub(seg.start))
	if segDays <= 0 {
		return
	}
	anchorDay := domain.AnchorDayFor(periodStart, billingTime, plan.BillingInterval, loc)
	fullCycleDays := roundDays(advanceBillingPeriod(periodStart, plan.BillingInterval, loc, anchorDay).Sub(periodStart))
	baseFee := plan.BaseAmountCents * seg.quantity
	description := fmt.Sprintf("%s - base fee (qty %d)", plan.Name, seg.quantity)

	// Prorate when:
	//   (a) this is a partial segment within a full cycle (mid-period
	//       change or partial-creation period), OR
	//   (b) the period itself is partial (creation mid-cycle), independent
	//       of segments — same formula reduces correctly.
	if segDays > 0 && fullCycleDays > 0 && segDays < fullCycleDays {
		baseFee = money.RoundHalfToEven(plan.BaseAmountCents*seg.quantity*int64(segDays), int64(fullCycleDays))
		description = fmt.Sprintf("%s - base fee (qty %d, prorated %d/%d days)", plan.Name, seg.quantity, segDays, fullCycleDays)
	} else if periodDays > 0 && fullCycleDays > 0 && periodDays < fullCycleDays {
		// Backstop for the periodDays<fullCycleDays case when only a
		// single segment exists and equals the whole period — preserves
		// the pre-segment-aware partial-creation behavior.
		baseFee = money.RoundHalfToEven(plan.BaseAmountCents*seg.quantity*int64(periodDays), int64(fullCycleDays))
		description = fmt.Sprintf("%s - base fee (qty %d, prorated %d/%d days)", plan.Name, seg.quantity, periodDays, fullCycleDays)
	}

	unitAmount := plan.BaseAmountCents
	if seg.quantity > 0 {
		unitAmount = money.RoundHalfToEven(baseFee, seg.quantity)
	}
	segStart := seg.start
	segEnd := seg.end
	*lineItems = append(*lineItems, domain.InvoiceLineItem{
		LineType:           domain.LineTypeBaseFee,
		Description:        description,
		Quantity:           seg.quantity,
		UnitAmountCents:    unitAmount,
		AmountCents:        baseFee,
		TotalAmountCents:   baseFee,
		Currency:           currency,
		BillingPeriodStart: &segStart,
		BillingPeriodEnd:   &segEnd,
	})
	*subtotal += baseFee
}

// advanceCycleOrCancel either fires a due scheduled cancel or advances the
// billing cycle, whichever the sub's current schedule fields require. The
// two outcomes are mutually exclusive at this point in the flow — a sub
// that's about to cancel must not also have its cycle advanced, otherwise
// the next tick would observe a canceled sub with a fresh next_billing_at
// and either log a confusing skip-not-active or risk a double-cycle bug
// later. trigger is "scheduled" or "scheduled_at" for telemetry.
func (e *Engine) advanceCycleOrCancel(ctx context.Context, sub domain.Subscription, periodEnd, nextPeriodStart, nextPeriodEnd, now time.Time) error {
	if !shouldFireScheduledCancel(sub, periodEnd, now) {
		return e.subs.UpdateBillingCycle(ctx, sub.TenantID, sub.ID, nextPeriodStart, nextPeriodEnd, nextPeriodEnd, sub.BillingAnchorDay)
	}

	canceled, err := e.subs.FireScheduledCancellation(ctx, sub.TenantID, sub.ID, now)
	if err != nil {
		// InvalidState here means a concurrent immediate-cancel already won
		// the race. Treat as a no-op success — the sub is canceled, which is
		// what we wanted, and the immediate-cancel handler already fired its
		// own webhook. A surfaced error here would mark the cycle as failed.
		if errors.Is(err, errs.ErrInvalidState) {
			slog.Info("scheduled cancel skipped, already canceled",
				"subscription_id", sub.ID, "tenant_id", sub.TenantID)
			return nil
		}
		return fmt.Errorf("fire scheduled cancel: %w", err)
	}

	slog.Info("scheduled cancel fired",
		"subscription_id", sub.ID,
		"tenant_id", sub.TenantID,
		"canceled_at", now.UTC(),
	)

	// Audit row for the engine-initiated transition. Operator-side
	// cancel + portal-side cancel both write AuditActionCancel; this
	// is the third path (auto-fire at period end) that previously
	// skipped audit, leaving the subscription Activity timeline
	// showing "Cancellation scheduled" with no terminal event. The
	// canceled_by='schedule' field matches the outbound-webhook
	// shape so a CS rep reading both surfaces sees the same vocabulary.
	if e.auditLogger != nil {
		_ = e.auditLogger.Log(ctx, sub.TenantID, domain.AuditActionCancel, "subscription", canceled.ID, canceled.Code, map[string]any{
			"canceled_by": "schedule",
			"customer_id": canceled.CustomerID,
		})
	}

	// subscription.canceled (canceled_by=schedule) is enqueued IN the
	// FireScheduledCancellation tx (store-level DispatchTx subset).
	return nil
}

// effectiveNow returns the clock time the engine should use for this sub.
// Thin wrapper over simForSub — see there for the fallback semantics.
func (e *Engine) effectiveNow(ctx context.Context, sub domain.Subscription) time.Time {
	return e.simForSub(ctx, sub).At
}

// simForSub resolves a sub's simulated-time context: the clock it is
// pinned to and that clock's frozen_time. If the sub is attached to a test
// clock, the clock's frozen_time wins; otherwise wall-clock via e.clock
// with an EMPTY TestClockID (not in a simulation).
//
// A deleted or unreadable test clock falls back silently — a dangling
// test_clock_id must not stall the billing tick for every other tenant. The
// fallback drops the clock id, so it can never stamp "clock C at wall-clock
// now," the half-truth the Sim type exists to make unrepresentable.
//
// Note what the fallback does NOT promise: that the instant is wall-clock.
// e.clock.Now(ctx) READS the ctx binding, so under an already-bound ctx (a
// catchup) it returns the SIMULATED instant, and the fallback's value is
// {simulated instant, no clock} — the mirror-image half-truth. That value is
// safe to use as a time (it is the catchup's own instant, which is what the
// work should run at), but it must never be BOUND back onto ctx, or it
// unstamps every audit row below it. clock.BindEffectiveNow refuses to bind a
// clock-less Sim over an inherited one for exactly this reason; read its guard
// before changing either side.
func (e *Engine) simForSub(ctx context.Context, sub domain.Subscription) clock.Sim {
	if sub.TestClockID == "" || e.testClocks == nil {
		return clock.Sim{At: e.clock.Now(ctx)}
	}
	tc, err := e.testClocks.Get(ctx, sub.TenantID, sub.TestClockID)
	if err != nil {
		slog.Warn("test clock lookup failed, falling back to wall clock",
			"subscription_id", sub.ID, "test_clock_id", sub.TestClockID, "error", err)
		return clock.Sim{At: e.clock.Now(ctx)}
	}
	return clock.Sim{At: tc.FrozenTime, TestClockID: sub.TestClockID}
}

// SimForInvoice resolves the simulated-time context for any per-invoice
// state-machine step that needs to stay in the simulation. Resolves
// invoice → subscription → test_clock and returns frozen_time + the clock
// id when pinned, a bare wall-clock Sim otherwise. Manual drafts (no
// subscription) fall back to the customer pin via SimForCustomer — one-off
// invoices for clock-pinned customers stamp simulated time too.
//
// Used by dunning to keep `next_action_at` in the same time domain
// the catchup query compares against (`<= frozen_time`). Without this
// resolver, dunning would stamp wall-clock into a column the
// orchestrator reads as simulated-time — and clock-pinned runs whose
// stamps land outside the catchup window get stranded. ADR-029 follow-up.
// It also carries the clock id, which is what lets every audit emission
// downstream of a pin resolution land on the sim axis (ADR-090 §5).
//
// Errors fall back to wall-clock with a warn — same safety stance as
// simForSub: a dangling subscription / clock pointer can't stall an
// operator-triggered retry. The fallback may stamp the wrong domain
// for clock-pinned runs, but failing the operator's action is worse.
func (e *Engine) SimForInvoice(ctx context.Context, tenantID, invoiceID string) (clock.Sim, error) {
	inv, err := e.invoices.GetInvoice(ctx, tenantID, invoiceID)
	if err != nil {
		return clock.Sim{At: e.clock.Now(ctx)}, fmt.Errorf("get invoice for clock resolution: %w", err)
	}
	if inv.SubscriptionID == "" {
		// One-off invoice: customer may still be clock-pinned (ADR-027).
		return e.SimForCustomer(ctx, tenantID, inv.CustomerID)
	}
	sub, err := e.subs.Get(ctx, tenantID, inv.SubscriptionID)
	if err != nil {
		slog.Warn("subscription lookup failed during clock resolution, falling back to wall clock",
			"invoice_id", invoiceID, "subscription_id", inv.SubscriptionID, "error", err)
		return clock.Sim{At: e.clock.Now(ctx)}, nil
	}
	return e.simForSub(ctx, sub), nil
}

// SimForSubscription resolves the simulated-time context for an
// operator-triggered action on an existing subscription (Activate,
// ChangeItem, etc.). Loads the sub, then delegates to simForSub.
// Wall-clock fallback on error mirrors the other resolvers.
func (e *Engine) SimForSubscription(ctx context.Context, tenantID, subscriptionID string) (clock.Sim, error) {
	sub, err := e.subs.Get(ctx, tenantID, subscriptionID)
	if err != nil {
		return clock.Sim{At: e.clock.Now(ctx)}, fmt.Errorf("get subscription for clock resolution: %w", err)
	}
	return e.simForSub(ctx, sub), nil
}

// SimForCustomer resolves the simulated-time context for an
// operator-triggered action where only the customer is known
// (subscription.Service.Create, one-off invoice composer). Reads
// customer.test_clock_id directly — subs inherit the pin from the
// owning customer at creation (ADR-027), so customer-level resolution
// is authoritative for any (yet-to-be-created or one-off) entity in
// that customer's orbit.
//
// Wall-clock fallback when the engine isn't wired with a customer
// reader (narrow unit tests) or when the customer / clock lookup
// fails. Same safety stance as the other resolvers: never block an
// operator action on a dangling pin.
func (e *Engine) SimForCustomer(ctx context.Context, tenantID, customerID string) (clock.Sim, error) {
	if e.customers == nil {
		return clock.Sim{At: e.clock.Now(ctx)}, nil
	}
	cust, err := e.customers.Get(ctx, tenantID, customerID)
	if err != nil {
		return clock.Sim{At: e.clock.Now(ctx)}, fmt.Errorf("get customer for clock resolution: %w", err)
	}
	if cust.TestClockID == "" || e.testClocks == nil {
		return clock.Sim{At: e.clock.Now(ctx)}, nil
	}
	tc, err := e.testClocks.Get(ctx, tenantID, cust.TestClockID)
	if err != nil {
		slog.Warn("test clock lookup failed during customer clock resolution, falling back to wall clock",
			"customer_id", customerID, "test_clock_id", cust.TestClockID, "error", err)
		return clock.Sim{At: e.clock.Now(ctx)}, nil
	}
	return clock.Sim{At: tc.FrozenTime, TestClockID: cust.TestClockID}, nil
}

// TaxApplication is the invoice-level tax summary returned by
// ApplyTaxToLineItems. The line items passed in are mutated in place with
// per-line TaxRateBP, TaxAmountCents, Jurisdiction, TaxCode, and
// TotalAmountCents so caller-side sums reconcile to TaxAmountCents.
//
// SubtotalCents and DiscountCents are the net (ex-tax) values the caller
// should persist to invoice.SubtotalCents / invoice.DiscountCents. In
// tax-exclusive mode they equal the caller's input. In tax-inclusive mode
// the provider carves tax out of the gross line amounts and the engine
// rewrites lineItems[i].AmountCents to the net value so the invariant
// SubtotalCents - DiscountCents + TaxAmountCents = customer payment holds.
//
// TaxProvider / TaxCalculationID / TaxReverseCharge / TaxExemptReason are
// the durable audit snapshot stamped onto the invoice header.
//
// TaxStatus signals whether the calculation succeeded (ok) or was deferred
// because the provider failed under a block-on-failure policy (pending).
// Pending invoices are persisted without tax amounts and are blocked from
// finalize until a retry worker completes the calculation.
type TaxApplication struct {
	// TaxFacts: the shared 13-field calculation bundle (see
	// domain/tax_facts.go) — writers copy it onto invoices as ONE struct
	// assignment instead of 13 hand-mirrored fields.
	domain.TaxFacts
	SubtotalCents int64
	DiscountCents int64
}

// truncateReason clips a provider error string to a sensible length before
// stuffing it into invoices.tax_pending_reason. Full provider payloads are
// persisted to tax_calculations for audit — this column is a human-readable
// hint for the dashboard banner.
func truncateReason(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

// displayTaxRate picks the rate to persist as invoice-level invoices.tax_rate.
// When every taxed line shares one rate (the common single-jurisdiction case)
// that statutory rate is authoritative — it is the real rate the customer was
// charged and what an auditor expects to read on the invoice. Only when lines
// carry genuinely different rates (a multi-jurisdiction invoice, where no
// single statutory rate exists) do we fall back to the blended effective rate
// (tax ÷ subtotal). Untaxed/exempt lines (TaxAmountCents == 0) are skipped so
// a mix of taxed + exempt lines still counts as single-rate.
//
// This unifies stripe_tax with the manual provider, which already reports its
// configured statutory rate as the effective rate. Stripe computes effective
// as tax÷subtotal, so a $100 NYC invoice would otherwise store 8.88 (888÷10000)
// instead of the 8.8750 its line items carry — a display/audit defect, never a
// mischarge (the amount is always Stripe's verbatim figure). See ADR-047.
func displayTaxRate(lines []tax.ResultLine, effectiveRate float64) float64 {
	rate := -1.0
	for _, l := range lines {
		if l.TaxAmountCents == 0 {
			continue
		}
		switch {
		case rate < 0:
			rate = l.TaxRate
		case math.Abs(l.TaxRate-rate) > 1e-9:
			return effectiveRate // genuinely mixed rates → blended effective
		}
	}
	if rate <= 0 {
		return effectiveRate // no taxed line carried a positive rate
	}
	return rate
}

// domesticReverseCharge reports whether a reverse_charge customer sits in the
// seller's own registration country — a likely-wrong override, since reverse
// charge applies only to CROSS-BORDER B2B (the buyer self-accounts in their own
// country; a buyer in the seller's country is taxed normally). Returns false
// when the status isn't reverse_charge or when either country is unknown (can't
// judge). Case- and whitespace-insensitive. CompanyCountry is a single-
// registration proxy (ADR-052), so this is a best-effort home-country signal,
// not full multi-registration enforcement.
func domesticReverseCharge(status domain.CustomerTaxStatus, customerCountry, registrationCountry string) bool {
	if status != domain.TaxStatusReverseCharge {
		return false
	}
	c := strings.TrimSpace(customerCountry)
	r := strings.TrimSpace(registrationCountry)
	if c == "" || r == "" {
		return false
	}
	return strings.EqualFold(c, r)
}

// ApplyTaxToLineItems resolves the tenant's configured tax provider, calls
// Calculate, and stamps the per-line + invoice-level results back onto the
// supplied domain types. Shared by the main billing path and proration so
// every invoice shape carries the same audit snapshot.
//
// Flow:
//  1. Load tenant settings + customer billing profile.
//  2. Resolve the Provider (NoneProvider / ManualProvider / StripeTaxProvider).
//  3. Build a tax.Request from the line items, billing profile, and plan-level
//     tax codes collected by the caller via InvoiceLineItem.TaxCode.
//  4. Call Provider.Calculate; on a provider error, DEFER the invoice — persist
//     it with tax_status=pending (placeholder zero-tax lines) + a classified
//     pending reason, and leave finalize blocked until a retry worker re-runs
//     calculation. Never silently charges zero tax (ADR-041).
//  5. Mutate line items in place with the per-line results. In inclusive
//     mode the provider returns carved net amounts; the engine rewrites
//     AmountCents to that net value so Subtotal - Discount + Tax == gross.
//  6. Persist the calculation to tax_calculations for durable audit.
//
// Safe to call with subtotal-discount <= 0 — returns zero tax and leaves
// line items untouched.
func (e *Engine) ApplyTaxToLineItems(ctx context.Context, tenantID, customerID, currency string, subtotal, discount int64, lineItems []domain.InvoiceLineItem) (TaxApplication, error) {
	// Wiring is required, not optional. Tests that don't need tax
	// must wire NoneProvider explicitly (e.NewResolver(nil) +
	// e.SetTaxProviderResolver) — the previous silent zero-tax
	// fallback masked misconfiguration in production. Fail loudly
	// here so the caller sees the misconfiguration in the log
	// instead of getting a $0-tax invoice.
	if e.taxProviders == nil {
		return TaxApplication{}, fmt.Errorf("tax provider resolver not wired (call SetTaxProviderResolver)")
	}
	if e.settings == nil {
		return TaxApplication{}, fmt.Errorf("settings store not wired")
	}

	app := TaxApplication{
		SubtotalCents: subtotal,
		DiscountCents: discount,
		TaxFacts:      domain.TaxFacts{TaxStatus: domain.InvoiceTaxOK},
	}

	// Pin tenant_id on ctx before resolving the provider. The
	// StripeTaxProvider's clientForCtx reads auth.TenantID(ctx) to
	// look up per-tenant Stripe credentials; background workers
	// (scheduler tick, test-clock catchup) build ctx with
	// WithLivemode but never set the tenant. Without this pin,
	// every tax call from a worker resolved a nil Stripe client
	// and surfaced as "no client configured for livemode=…" even
	// when credentials existed in the DB. HTTP-handler-driven
	// calls were unaffected because session/auth middleware pins
	// tenant_id on the request ctx.
	ctx = auth.WithTenantID(ctx, tenantID)

	// SettingsStore.Get synthesizes Velox defaults on miss
	// (tenant.DefaultSettings) — this Get returns a real error
	// only on a true DB failure, never on missing-row. Bootstrap
	// always creates the settings row, so missing-row is itself
	// rare.
	ts, err := e.settings.Get(ctx, tenantID)
	if err != nil {
		return TaxApplication{}, fmt.Errorf("load tenant settings: %w", err)
	}
	app.TaxName = ts.TaxName

	// The resolver fails loud (ADR-041): tax_provider=stripe_tax with no
	// wired client, or an unrecognized provider string, returns an error
	// rather than silently substituting manual/none. A Resolve error aborts
	// invoice creation here — surfacing the misconfiguration instead of
	// emitting a wrong-tax invoice.
	provider, err := e.taxProviders.Resolve(ctx, ts)
	if err != nil {
		return TaxApplication{}, fmt.Errorf("resolve tax provider: %w", err)
	}
	app.TaxProvider = provider.Name()

	var profile domain.CustomerBillingProfile
	if e.profiles != nil && customerID != "" {
		if bp, err := e.profiles.GetBillingProfile(ctx, tenantID, customerID); err == nil {
			profile = bp
			app.TaxCountry = bp.Country
			app.TaxID = bp.TaxID
		}
	}

	// Domestic reverse charge is a contradiction: reverse charge applies only to
	// CROSS-BORDER B2B. A buyer in the seller's own registration country is taxed
	// normally. We do NOT block — the operator's explicit override is honored
	// (same trust model as exempt, and the only mechanism for manual/none
	// providers) — but we log it, because the override silently zero-rates a sale
	// that should be taxed. For a standard customer the cross-border decision is
	// deferred to the tax engine (Stripe auto-applies reverse charge from the
	// tax ID + jurisdictions); ADR-052. Non-blocking; no-op when either country
	// is unknown, and CompanyCountry is a single-registration proxy.
	if domesticReverseCharge(profile.TaxStatus, profile.Country, ts.CompanyCountry) {
		slog.WarnContext(ctx, "tax: domestic reverse charge — buyer is in the seller's registration country, so reverse charge does not apply; invoice zero-rated on the operator's explicit override. Verify this customer's tax status (reverse charge is cross-border B2B only).",
			"tenant_id", tenantID, "customer_id", customerID,
			"customer_country", profile.Country, "registration_country", ts.CompanyCountry)
	}

	onFailure := ts.TaxOnFailure
	if onFailure == "" {
		onFailure = tax.OnFailureBlock
	}

	// Providers must never see a negative line (Stripe Tax 400s it; manual
	// clamps it and corrupts the per-line split). The retry family reaches
	// here with STORED lines, which for an ADR-048 split proration invoice
	// include the negative credit line — collapse restores the same net-line
	// shape the create path sent (#556). No-op when no line is negative.
	reqLines, lineGroups, err := collapseTaxRequestLines(lineItems, ts.DefaultProductTaxCode)
	if err != nil {
		return TaxApplication{}, err
	}

	req := tax.Request{
		Currency: currency,
		CustomerAddress: tax.Address{
			Line1:      profile.AddressLine1,
			Line2:      profile.AddressLine2,
			City:       profile.City,
			State:      profile.State,
			PostalCode: profile.PostalCode,
			Country:    profile.Country,
		},
		CustomerTaxID:        profile.TaxID,
		CustomerTaxIDType:    profile.TaxIDType,
		CustomerStatus:       profile.TaxStatus,
		CustomerExemptReason: profile.TaxExemptReason,
		TaxInclusive:         ts.TaxInclusive,
		DiscountCents:        discount,
		DefaultTaxCode:       ts.DefaultProductTaxCode,
		OnFailure:            onFailure,
		LineItems:            reqLines,
	}

	res, err := provider.Calculate(ctx, req)
	if err != nil {
		// Under OnFailureBlock policy the provider surfaces the error so we
		// can defer the invoice rather than silently charging the wrong tax.
		// The invoice is persisted with tax_status=pending + zero tax lines;
		// a background retry worker will re-run calculation and lift the
		// block when Stripe returns. Finalize is guarded downstream.
		slog.Warn("tax: provider calculation failed, deferring invoice",
			"error", err, "tenant_id", tenantID, "provider", provider.Name())
		for i := range lineItems {
			lineItems[i].TaxRate = 0
			lineItems[i].TaxAmountCents = 0
			lineItems[i].TotalAmountCents = lineItems[i].AmountCents
		}
		app.TaxStatus = domain.InvoiceTaxPending
		// Classify the provider error into a typed taxonomy so the
		// dashboard banner and webhook consumers can branch on cause
		// (customer-data fix vs provider-outage retry). The pending
		// reason is the cleaned upstream message (Stripe Tax JSON
		// envelope unwrapped) — preserved verbatim in tax_calculations
		// for diagnostic depth.
		app.TaxErrorCode = string(tax.Classify(err))
		app.TaxPendingReason = truncateReason(tax.CleanMessage(err.Error()), 500)
		// tax_deferred_at lands in simulated time on clock-pinned
		// invoices via ctx-bound effective-now. The clock-nil branch
		// covers narrow unit tests that construct an Engine without
		// wiring a Clock; production always wires Real().
		var deferredAt time.Time
		if e.clock != nil {
			deferredAt = e.clock.Now(ctx)
		} else {
			deferredAt = clock.Now(ctx)
		}
		app.TaxDeferredAt = &deferredAt
		// Persist the failed attempt so operators can see why. Uses a nil
		// result — the store's RecordFromResult handles that by marking the
		// provider "none" with the marshaled request as audit material.
		if e.taxCalcStore != nil {
			if _, perr := e.taxCalcStore.Record(ctx, tenantID, "", req, nil); perr != nil {
				slog.Warn("tax: failed to persist deferred tax_calculations row",
					"error", perr, "tenant_id", tenantID)
			}
		}
		return app, nil
	}

	app.TaxAmountCents = res.TotalTaxCents
	app.TaxRate = displayTaxRate(res.Lines, res.EffectiveRate)
	if res.TaxName != "" {
		app.TaxName = res.TaxName
	}
	if res.TaxCountry != "" {
		app.TaxCountry = res.TaxCountry
	}
	app.TaxCalculationID = res.CalculationID
	app.TaxReverseCharge = res.ReverseCharge
	app.TaxExemptReason = res.ExemptReason

	// Apply per-line results. expandTaxResultLines returns one entry per
	// original line item (collapsed groups re-partitioned by the ADR-048
	// split rule); with no negative lines it is the provider's positional
	// result unchanged. An empty Ref marks a line the provider never
	// produced — leave that line item untouched, as the positional loop
	// always has.
	expanded := expandTaxResultLines(res.Lines, lineGroups, lineItems)
	var netSubtotalSum int64
	for i := range lineItems {
		if i >= len(expanded) {
			break
		}
		rl := expanded[i]
		if rl.Ref == "" {
			continue
		}
		net := rl.NetAmountCents
		if net == 0 {
			net = lineItems[i].AmountCents
		}
		lineItems[i].AmountCents = net
		lineItems[i].TaxRate = rl.TaxRate
		lineItems[i].TaxAmountCents = rl.TaxAmountCents
		lineItems[i].TaxJurisdiction = rl.Jurisdiction
		lineItems[i].TaxCode = rl.TaxCode
		lineItems[i].TaxabilityReason = rl.TaxabilityReason
		lineItems[i].TotalAmountCents = net + rl.TaxAmountCents
		netSubtotalSum += net
	}

	// Inclusive mode: the provider returned net line amounts; rewrite
	// subtotal/discount to net units so the invoice invariant holds.
	if ts.TaxInclusive && netSubtotalSum != subtotal {
		app.SubtotalCents = netSubtotalSum
		// Gross paid == subtotal - discount, which after tax carve-out is
		// netSubtotalSum - netDiscount + tax. Solve for netDiscount.
		gross := subtotal - discount
		app.DiscountCents = max(netSubtotalSum+res.TotalTaxCents-gross, 0)
	}

	if e.taxCalcStore != nil {
		if _, err := e.taxCalcStore.Record(ctx, tenantID, "", req, res); err != nil {
			// Persistence failure doesn't block the invoice — log it. The
			// billing engine's correctness does not depend on the audit row.
			slog.Warn("tax: failed to persist tax_calculations row",
				"error", err, "tenant_id", tenantID, "provider", provider.Name())
		}
	}

	return app, nil
}

// RunCycle finds all subscriptions due for billing and generates invoices.
// Returns the number of invoices generated and any errors encountered.
// RunCycleForClock processes due subs attached to ONE test clock. The
// per-sub period loop in billSubscription catches each sub up to the
// clock's frozen time in a single pass. Called by the test-clock
// catchup worker after MarkAdvancing — operator-driven (Advance
// click), never the wall-clock cron.
//
// Returns the count of invoices generated and any per-sub errors
// (non-fatal — failures on one sub don't stall the others). The
// outer loop ensures every due sub on the clock is processed even
// if more than batchSize are attached.
//
// ADR-028 disjoint-flow architecture: this is the ONLY path for
// clock-pinned billing. The wall-clock RunCycle explicitly excludes
// clock-pinned subs.
func (e *Engine) RunCycleForClock(ctx context.Context, tenantID, clockID string, batchSize int) (int, []error) {
	if batchSize <= 0 {
		batchSize = 50
	}
	ctx, span := telemetry.Tracer("billing").Start(ctx, "billing.RunCycleForClock",
		trace.WithAttributes(
			attribute.String("clock_id", clockID),
			attribute.String("tenant_id", tenantID),
			attribute.Int("batch_size", batchSize),
		),
	)
	defer span.End()

	generated := 0
	var errs []error

	// Outer loop only matters if a clock has more than batchSize
	// attached subs (rare). Each pass fetches a fresh batch; subs
	// whose catchup completes in the inner per-sub period loop advance
	// past the clock and fall off the next pass.
	//
	// Termination guard (best-practice hardening, 2026-05-31):
	// GetDueBillingForClock returns subs whose next_billing_at <=
	// frozen_time. A sub that FAILS to bill never advances
	// next_billing_at, so it stays "due" and is re-fetched on every
	// pass — an infinite outer loop. We attempt each sub at most once
	// per call: once it errors it's excluded from further passes via
	// `failed`. When every due sub has already failed this call, no
	// progress is possible — break and let the worker mark the clock
	// internal_failure (the operator's "Retry advance" resumes once
	// the cause is fixed). Pre-fix, a single persistently-failing sub
	// (e.g. a malformed INSERT, a constraint violation) spun this loop
	// at ~30/sec, pegging CPU and flooding tax_calculations with
	// orphan rows — and because RunCycleForClock never returned, the
	// worker never reached MarkFailed, so the clock stayed 'advancing'
	// and was re-enqueued on every boot.
	failed := make(map[string]bool)
	for {
		if err := ctx.Err(); err != nil {
			errs = append(errs, fmt.Errorf("clock catchup ctx done: %w", err))
			break
		}

		due, err := e.subs.GetDueBillingForClock(ctx, tenantID, clockID, batchSize)
		if err != nil {
			errs = append(errs, fmt.Errorf("fetch due-on-clock: %w", err))
			break
		}
		if len(due) == 0 {
			break
		}

		// No-progress break: if every due sub has already been
		// attempted-and-failed this call, re-fetching returns the same
		// stuck set forever. Stop.
		progressable := false
		for _, sub := range due {
			if !failed[sub.ID] {
				progressable = true
				break
			}
		}
		if !progressable {
			break
		}

		for _, sub := range due {
			if failed[sub.ID] {
				continue // already attempted + failed this call
			}
			n, err := e.billSubscription(ctx, sub)
			if err != nil {
				slog.Error("bill subscription failed (clock-catchup)",
					"subscription_id", sub.ID,
					"clock_id", clockID,
					"invoices_before_error", n,
					"error", err,
				)
				errs = append(errs, fmt.Errorf("subscription %s: %w", sub.ID, err))
				failed[sub.ID] = true
			}
			generated += n
		}
	}

	span.SetAttributes(attribute.Int("generated", generated))
	slog.Info("clock catchup cycle complete",
		"clock_id", clockID,
		"generated", generated,
		"errors", len(errs),
	)
	return generated, errs
}

// SubBillError pairs a subscription that failed to bill with its cause. The
// cause is logged server-side in full; only the (caller-owned) subscription id
// plus a generic class are surfaced to the API caller — raw pq constraint names
// and Stripe error text must never enter the response body.
type SubBillError struct {
	SubscriptionID string
	Err            error
}

func (e SubBillError) Error() string {
	if e.SubscriptionID == "" {
		return e.Err.Error()
	}
	return fmt.Sprintf("subscription %s: %v", e.SubscriptionID, e.Err)
}
func (e SubBillError) Unwrap() error { return e.Err }

// RunCycleForTenant is the operator-triggered (POST /v1/billing/run) billing
// pass, scoped to ONE tenant. Unlike the unscoped RunCycle (scheduler-only,
// scheduler.go), it bills only the caller's own due subscriptions via
// GetDueBillingForTenant (RLS-fenced) — the manual trigger can never sweep or
// observe another tenant. It drains the tenant's due set across batches (a
// single batchSize pass would silently under-bill a tenant with >batchSize due
// subs), attempting each sub at most once per call: a sub that fails to bill
// never advances next_billing_at and stays "due", so the no-progress guard
// breaks instead of re-billing it forever — the same shape as RunCycleForClock.
func (e *Engine) RunCycleForTenant(ctx context.Context, tenantID string, batchSize int) (int, []SubBillError) {
	if batchSize <= 0 {
		batchSize = 50
	}
	ctx, span := telemetry.Tracer("billing").Start(ctx, "billing.RunCycleForTenant",
		trace.WithAttributes(
			attribute.String("tenant_id", tenantID),
			attribute.Int("batch_size", batchSize),
		),
	)
	defer span.End()

	// Anchor the whole run at one instant so a sub billed early can't re-qualify
	// under a later "now" within the same call.
	now := e.clock.Now(ctx)

	generated := 0
	var failures []SubBillError
	failed := make(map[string]bool)
	for {
		if err := ctx.Err(); err != nil {
			failures = append(failures, SubBillError{Err: fmt.Errorf("billing run ctx done: %w", err)})
			break
		}

		due, err := e.subs.GetDueBillingForTenant(ctx, tenantID, now, batchSize)
		if err != nil {
			span.RecordError(err)
			failures = append(failures, SubBillError{Err: fmt.Errorf("fetch due subscriptions: %w", err)})
			break
		}
		if len(due) == 0 {
			break
		}

		// No-progress break: if every due sub has already been attempted-and-
		// failed this call, re-fetching returns the same stuck set forever.
		progressable := false
		for _, sub := range due {
			if !failed[sub.ID] {
				progressable = true
				break
			}
		}
		if !progressable {
			break
		}

		for _, sub := range due {
			if failed[sub.ID] {
				continue
			}
			n, err := e.billSubscription(ctx, sub)
			if err != nil {
				slog.Error("bill subscription failed (manual run)",
					"tenant_id", tenantID,
					"subscription_id", sub.ID,
					"invoices_before_error", n,
					"error", err,
				)
				failed[sub.ID] = true
				failures = append(failures, SubBillError{SubscriptionID: sub.ID, Err: err})
			}
			generated += n
		}
	}

	span.SetAttributes(attribute.Int("generated", generated))
	return generated, failures
}

func (e *Engine) RunCycle(ctx context.Context, batchSize int) (int, []error) {
	if batchSize <= 0 {
		batchSize = 50
	}

	ctx, span := telemetry.Tracer("billing").Start(ctx, "billing.RunCycle",
		trace.WithAttributes(attribute.Int("batch_size", batchSize)),
	)
	defer span.End()

	due, err := e.subs.GetDueBilling(ctx, e.clock.Now(ctx), batchSize)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "fetch due subscriptions")
		return 0, []error{fmt.Errorf("fetch due subscriptions: %w", err)}
	}
	span.SetAttributes(attribute.Int("due_count", len(due)))

	if len(due) == 0 {
		return 0, nil
	}

	slog.Info("billing cycle started", "due_count", len(due))

	generated := 0
	var errs []error

	for _, sub := range due {
		n, err := e.billSubscription(ctx, sub)
		if err != nil {
			slog.Error("bill subscription failed",
				"subscription_id", sub.ID,
				"tenant_id", sub.TenantID,
				"invoices_before_error", n,
				"error", err,
			)
			errs = append(errs, fmt.Errorf("subscription %s: %w", sub.ID, err))
		}
		generated += n
	}

	slog.Info("billing cycle complete", "generated", generated, "errors", len(errs))
	return generated, errs
}

// maxPeriodsPerSubPerCall caps how many periods billSubscription
// will generate for a single sub in one call. The cap is a safety
// guard against a billOnePeriod bug that fails to advance
// next_billing_at — without it the period loop would spin forever.
//
// 10000 covers any realistic catch-up: 833 years of monthly billing,
// 27 years of daily. Test-clock advances beyond that need a chained
// retry, which is fine — the wall-clock 10-min CatchupTimeout is the
// outer ceiling.
const maxPeriodsPerSubPerCall = 10000

// billSubscription catches a subscription up to its effectiveNow by
// looping billOnePeriod until next_billing_at exceeds the clock the
// sub runs on. Production wall-clock subs typically need exactly one
// iteration (the cycle accumulates one period of debt before each
// scheduler tick). Test-clock catch-up after a multi-year advance
// runs N iterations in a single call — the operator clicks Advance
// once and the sub catches up fully, no chained retries.
//
// Returns the number of invoices generated (skipped/trial periods
// don't count) and any fatal error encountered. Partial progress
// is preserved on error: invoices generated before the error stay
// committed (each iteration is its own DB tx via billOnePeriod).
//
// The pacing env knob (VELOX_TEST_CLOCK_CATCHUP_DELAY_MS) is honoured
// between iterations so the manual restart-resilience smoke test
// keeps working — kill -9 mid-loop is still observable from the
// outside.
//
// ADR-028.
func (e *Engine) billSubscription(ctx context.Context, sub domain.Subscription) (int, error) {
	count := 0
	for i := 0; i < maxPeriodsPerSubPerCall; i++ {
		// Honour ctx deadline (CatchupTimeout from the worker, request
		// timeout from the scheduler). Returns the partial count + err.
		if err := ctx.Err(); err != nil {
			return count, fmt.Errorf("billing loop ctx done: %w", err)
		}

		// Refresh sub state. Required because billOnePeriod commits
		// per-period UpdateBillingCycle in its own tx, and we want
		// the next iteration to observe the post-advance fields.
		fresh, err := e.subs.Get(ctx, sub.TenantID, sub.ID)
		if err != nil {
			return count, fmt.Errorf("refresh subscription %s: %w", sub.ID, err)
		}
		sub = fresh

		// Caught-up check. The clock the sub lives on (test or wall)
		// determines "now"; sub.NextBillingAt strictly after that
		// means there's nothing more to bill in this call.
		now := e.effectiveNow(ctx, sub)

		// ADR-097: a cancel_at strictly INSIDE the current period fires
		// here, BEFORE the caught-up early-return — the motivating bug's
		// sub has next_billing_at months in the future, so any code below
		// that return never sees it. Strict Before on next_billing_at:
		// cancel_at == next_billing_at is the boundary path's tie
		// (shouldFireScheduledCancel fires at equality after billing the
		// full period); the two paths must never both claim an instant.
		// The due scans admit these subs via their cancel arm (gated
		// status='active' in SQL — the same predicate as this branch, so
		// every admitted row makes progress and the fetch loop can't
		// livelock on a clean decline).
		if sub.Status == domain.SubscriptionActive &&
			sub.CancelAt != nil && !sub.CancelAt.After(now) &&
			(sub.NextBillingAt == nil || sub.CancelAt.Before(*sub.NextBillingAt)) {
			if e.cancelExecutor == nil {
				// Fail loud: an unexecutable due cancel would re-fetch on
				// every scan pass forever and keep renewing past its own
				// cancellation. The producer always wires the executor.
				return count, fmt.Errorf("due mid-period cancel_at on %s but no ScheduledCancelExecutor wired", sub.ID)
			}
			if err := e.cancelExecutor.FireDueScheduledCancel(ctx, sub.TenantID, sub.ID, *sub.CancelAt); err != nil {
				return count, fmt.Errorf("fire due mid-period cancel %s: %w", sub.ID, err)
			}
			// Terminal: the sub is canceled (or a benign race already
			// resolved it) — either way it drops out of the due scans.
			return count, nil
		}

		if sub.NextBillingAt == nil || sub.NextBillingAt.After(now) {
			return count, nil
		}

		// Snapshot for the no-progress detector below.
		prevNextBilling := sub.NextBillingAt

		invoiced, err := e.billOnePeriod(ctx, sub)
		if err != nil {
			return count, err
		}
		if invoiced {
			count++
		}

		// No-progress guard: if billOnePeriod returned cleanly but
		// did not advance next_billing_at, the next iteration would
		// fire identically forever. Bail with a typed error so the
		// caller can mark the clock internal_failure with a useful
		// reason instead of looping until the per-sub cap.
		afterCheck, err := e.subs.Get(ctx, sub.TenantID, sub.ID)
		if err != nil {
			return count, fmt.Errorf("post-bill refresh %s: %w", sub.ID, err)
		}
		if afterCheck.NextBillingAt == nil || (prevNextBilling != nil && !afterCheck.NextBillingAt.After(*prevNextBilling)) {
			// Skipped sub (status non-active, no items, etc.) —
			// billOnePeriod returned cleanly without advancing.
			// That's the natural exit, not an error.
			return count, nil
		}

		// Optional pacing for the manual kill-mid-flight restart-
		// resilience smoke test (MANUAL_TEST FLOW TC2). Honours ctx
		// cancellation so kill -TERM mid-sleep wakes promptly.
		if delay := catchupDelayFromEnvBilling(); delay > 0 {
			select {
			case <-ctx.Done():
				return count, fmt.Errorf("billing loop ctx done during pacing: %w", ctx.Err())
			case <-time.After(delay):
			}
		}
	}
	return count, fmt.Errorf("subscription %s: per-sub safety cap %d hit — billOnePeriod did not converge", sub.ID, maxPeriodsPerSubPerCall)
}

// catchupDelayFromEnvBilling reads VELOX_TEST_CLOCK_CATCHUP_DELAY_MS
// for in-loop pacing. Mirrors testclock.catchupDelayFromEnv but
// duplicated here to avoid a billing→testclock import (would create
// a cycle). The semantics match — this is the same env, same
// behaviour, just read at the inner loop instead of the outer one.
func catchupDelayFromEnvBilling() time.Duration {
	v := os.Getenv("VELOX_TEST_CLOCK_CATCHUP_DELAY_MS")
	if v == "" {
		return 0
	}
	ms, err := strconv.Atoi(v)
	if err != nil || ms <= 0 {
		return 0
	}
	return time.Duration(ms) * time.Millisecond
}

// billOnePeriod generates an invoice for ONE billing period of a
// subscription, advancing next_billing_at by one cycle. Pre-Phase-2
// (ADR-028) this was the only billing primitive — RunCycle called it
// once per due sub per pass, and runCatchupLoop relied on an outer
// loop to compress N periods of catch-up into N passes. Phase 2
// keeps this function unchanged but wraps it in billSubscription
// which loops until the sub catches up to its effectiveNow.
//
// Returns (true, nil) if an invoice was created, (false, nil) if
// skipped (e.g. trial-active, status not active). The bool here
// means "invoice generated", NOT "next_billing_at advanced" — some
// skip paths (trial-active) advance the cycle without generating
// an invoice. The wrapper distinguishes via fresh sub-state reads.
// usageLineDescription labels a multi-dim usage line by its dimension match
// so an invoice with N rule buckets on one meter (e.g. tokens → input /
// cache_read / output) reads as distinct lines rather than N identical
// "Tokens (tokens)" rows. Falls back to the meter unit when the matched rule
// carries no dimension_match (or the bucket was the unclaimed fallback).
func usageLineDescription(meter domain.Meter, rule domain.MeterPricingRule) string {
	if len(rule.DimensionMatch) == 0 {
		return fmt.Sprintf("%s (%s)", meter.Name, meter.Unit)
	}
	// Prefer a compact "model · token_type" form for the AI shape; otherwise
	// join whatever dimensions the rule matches on, in a stable order.
	keys := make([]string, 0, len(rule.DimensionMatch))
	for k := range rule.DimensionMatch {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%v", rule.DimensionMatch[k]))
	}
	return fmt.Sprintf("%s (%s)", meter.Name, strings.Join(parts, " · "))
}

// applyDueScheduledPlanChanges applies any item plan changes scheduled
// to take effect at or before this cycle's close, BEFORE the caller
// reads plans — so the new cycle bills on the new plans. Extracted from
// billOnePeriod (behavior-preserving); returns the subscription with its
// item set refreshed when a swap applied, else unchanged.
func (e *Engine) applyDueScheduledPlanChanges(ctx context.Context, sub domain.Subscription, now time.Time) (domain.Subscription, error) {
	// If any item has a scheduled plan change whose effective_at falls
	// within (or at) the cycle being processed, apply them all BEFORE
	// reading plans so the new cycle bills on the new plans.
	//
	// Gate is CurrentBillingPeriodEnd, NOT wall-clock now. Stripe-Billing
	// parity: a change scheduled for "next cycle" must not apply
	// retroactively to a late-billed earlier cycle just because the
	// engine ran late. Pre-fix the comparison was `effective_at <= now`,
	// which fired for any cycle as long as the engine was running after
	// the effective date — visible bug on test-clock catchup spanning
	// multiple periods (engine processes each period sequentially with
	// frozen_time as `now`, so a mid-year scheduled change retroactively
	// applied to ALL prior periods).
	//
	// Falls back to `now` when CurrentBillingPeriodEnd is unset (rare;
	// shouldn't happen for a finalized cycle, but the fallback preserves
	// pre-fix behavior in that path).
	gate := now
	if sub.CurrentBillingPeriodEnd != nil {
		gate = *sub.CurrentBillingPeriodEnd
	}
	anyDue := false
	for _, it := range sub.Items {
		if it.PendingPlanID != "" && it.PendingPlanEffectiveAt != nil && !it.PendingPlanEffectiveAt.After(gate) {
			anyDue = true
			break
		}
	}
	if anyDue {
		// Snapshot the item IDs whose pending change is due BEFORE the
		// atomic swap — ApplyDuePendingItemPlansAtomic returns the full
		// refreshed item set (due + not-due) with pending_plan_id
		// cleared, so we can't tell post-hoc which rows actually
		// changed. This pre-swap list is what we fire
		// subscription.pending_change.applied events for. The outgoing
		// plan itself is captured in subscription_item_changes by the
		// DB trigger (migration 0029) — the segment-aware base-fee
		// loop further down reads that history rather than carrying a
		// snapshot map through this function.
		dueItems := make([]domain.SubscriptionItem, 0)
		for _, it := range sub.Items {
			if it.PendingPlanID != "" && it.PendingPlanEffectiveAt != nil && !it.PendingPlanEffectiveAt.After(gate) {
				dueItems = append(dueItems, it)
			}
		}

		// SQL filter inside ApplyDuePendingItemPlansAtomic uses the
		// gate value to select rows: WHERE pending_plan_effective_at
		// <= $gate. Same semantics as the Go-side check above so
		// in-memory and DB views agree.
		applied, err := e.subs.ApplyDuePendingItemPlansAtomic(ctx, sub.TenantID, sub.ID, gate)
		if err != nil && !errors.Is(err, errs.ErrNotFound) {
			return sub, fmt.Errorf("apply pending item plans: %w", err)
		}
		if applied != nil {
			sub.Items = applied
			slog.Info("applied scheduled item plan changes",
				"subscription_id", sub.ID,
				"items_changed", len(applied),
			)

			// Post-swap the plan swap is durable. Two INDEPENDENT consequences
			// follow, and they are deliberately no longer nested one inside the
			// other: the outbound webhook, and the audit row.
			//
			// The audit emission used to live inside `if e.events != nil`, so a
			// deployment with no event dispatcher wired wrote NO AUDIT ROW for a
			// plan swap that really happened — audit coverage silently riding on
			// webhook wiring, which is a completely different concern. Evidence
			// that a mutation occurred must not depend on whether anyone is
			// listening for it.
			newPlanByItem := make(map[string]string, len(applied))
			for _, it := range applied {
				newPlanByItem[it.ID] = it.PlanID
			}

			// EVIDENCE: the swap happened; record it. Independent of the webhook.
			if e.auditLogger != nil {
				for _, was := range dueItems {
					_ = e.auditLogger.Log(ctx, sub.TenantID, "subscription.pending_change_applied", "subscription", sub.ID, sub.Code, map[string]any{
						"item_id":     was.ID,
						"old_plan_id": was.PlanID,
						"new_plan_id": newPlanByItem[was.ID],
					})
				}
			}

			// NOTIFICATION: one event per transitioned item, only on a successful
			// swap so a half-applied state doesn't lie to webhook consumers.
			if e.events != nil {
				for _, was := range dueItems {
					payload := map[string]any{
						"subscription_id": sub.ID,
						"customer_id":     sub.CustomerID,
						"item_id":         was.ID,
						"old_plan_id":     was.PlanID,
						"new_plan_id":     newPlanByItem[was.ID],
						"applied_at":      now.UTC(),
					}
					if err := e.events.Dispatch(ctx, sub.TenantID, domain.EventSubscriptionPendingChangeApplied, payload); err != nil {
						slog.Error("dispatch subscription.pending_change.applied",
							"subscription_id", sub.ID,
							"item_id", was.ID,
							"tenant_id", sub.TenantID,
							"error", err,
						)
					}
				}
			}
		}
	}
	return sub, nil
}

// resolveBillingInputs gathers everything needed to price this period:
// each item's plan, the invoice currency, and the per-meter usage totals
// (usage-cap-scaled when a subscription cap fires). Extracted from
// billOnePeriod (behavior-preserving).
func (e *Engine) resolveBillingInputs(ctx context.Context, sub domain.Subscription, periodStart, periodEnd time.Time) (map[string]domain.Plan, string, map[string]string, map[string]decimal.Decimal, error) {
	// Resolve every item's plan up-front so we can read currency / meters / base
	// fee from the set. Plans come back keyed by item plan_id — items sharing a
	// plan (which UNIQUE (sub_id, plan_id) prevents, but defend anyway) resolve
	// to the same plan struct.
	plans := make(map[string]domain.Plan, len(sub.Items))
	for _, it := range sub.Items {
		if _, ok := plans[it.PlanID]; ok {
			continue
		}
		pl, err := e.pricing.GetPlan(ctx, sub.TenantID, it.PlanID)
		if err != nil {
			return nil, "", nil, nil, fmt.Errorf("get plan %s: %w", it.PlanID, err)
		}
		plans[it.PlanID] = pl
	}

	// Invoice currency — ACTUAL precedence (the old comment here claimed
	// settings outranked plans; born false): customer billing profile >
	// first item's plan > tenant settings (dead branch in practice — plans
	// always carry a currency) > "USD". The first-item tie-breaker in
	// multi-item mode is unreachable-by-construction since ADR-100: the
	// subscription-side currency pin refuses mixed-currency item sets, a
	// divergent profile, and cross-currency swaps at every write, so by the
	// time this code runs all sources agree. Kept as a fallback order, not
	// a reconciliation policy.
	firstPlanCurrency := plans[sub.Items[0].PlanID].Currency
	invoiceCurrency := firstPlanCurrency
	if e.profiles != nil {
		if bp, err := e.profiles.GetBillingProfile(ctx, sub.TenantID, sub.CustomerID); err == nil && bp.Currency != "" {
			invoiceCurrency = bp.Currency
		}
	}
	if invoiceCurrency == "" && e.settings != nil {
		if ts, err := e.settings.Get(ctx, sub.TenantID); err == nil && ts.DefaultCurrency != "" {
			invoiceCurrency = ts.DefaultCurrency
		}
	}
	if invoiceCurrency == "" {
		invoiceCurrency = "USD"
	}

	// Collect the union of meter_ids across every item's plan. Usage is
	// customer+meter-scoped (not item-scoped) — a meter shared between two
	// items' plans aggregates once, not twice. The aggregation type is picked
	// from whichever meter lookup resolves first; in practice a meter has one
	// canonical aggregation so this is a no-op, but the map shape tolerates
	// duplicates defensively.
	meterAggs := make(map[string]string)
	for _, it := range sub.Items {
		for _, meterID := range plans[it.PlanID].MeterIDs {
			if _, ok := meterAggs[meterID]; ok {
				continue
			}
			m, err := e.pricing.GetMeter(ctx, sub.TenantID, meterID)
			if err == nil {
				meterAggs[meterID] = m.Aggregation
			} else {
				meterAggs[meterID] = "sum"
			}
		}
	}

	// Aggregate usage for each meter using its configured aggregation type.
	usageTotals, err := e.usage.AggregateForBillingPeriodByAgg(ctx, sub.TenantID, sub.CustomerID, meterAggs, periodStart, periodEnd)
	if err != nil {
		return nil, "", nil, nil, fmt.Errorf("aggregate usage: %w", err)
	}

	// Enforce usage cap if configured. Cap is a subscription-level total
	// across all meters — a container-level guardrail, not a per-plan
	// constraint. Cap stays integer (UsageCapUnits int64) because operators
	// author it as a whole-unit ceiling; per-meter quantities are decimal
	// and prorated proportionally if the cap fires.
	if sub.UsageCapUnits != nil && *sub.UsageCapUnits > 0 && sub.OverageAction == "block" {
		totalUsage := decimal.Zero
		for _, qty := range usageTotals {
			totalUsage = totalUsage.Add(qty)
		}
		capDec := decimal.NewFromInt(*sub.UsageCapUnits)
		if totalUsage.GreaterThan(capDec) {
			for mid, qty := range usageTotals {
				usageTotals[mid] = qty.Mul(capDec).Div(totalUsage)
			}
		}
	}
	return plans, invoiceCurrency, meterAggs, usageTotals, nil
}

// handleTrialState runs the trial lifecycle for a trialing subscription:
// advance-without-billing while the trial runs, cancel-free when a cancel
// is due at trial end, or atomically flip to active (billing day-1
// in-advance coverage) when the trial has elapsed. Returns the
// (possibly-updated) subscription and done=true when the caller should
// stop billing this sub this tick (trial still active, canceled, or
// resolved to a non-active state by a concurrent writer); done=false
// means continue to normal billing with the returned subscription.
// Non-trialing subscriptions return (sub, false, nil) immediately.
// Extracted from billOnePeriod (behavior-preserving).
func (e *Engine) handleTrialState(ctx context.Context, sub domain.Subscription, now, periodStart, periodEnd time.Time) (domain.Subscription, bool, error) {
	if sub.Status == domain.SubscriptionTrialing {
		trialOver := sub.TrialEndAt == nil || !now.Before(*sub.TrialEndAt)
		if !trialOver {
			// Trial-active cycle advance: honors billing_time so calendar
			// subs with an extended-past-period trial stay calendar-aligned.
			// Interval is hardcoded monthly here (plans not yet fetched);
			// trial-extended-past-yearly-cycle is an edge case that the
			// pre-existing hardcoded `monthly` already approximated.
			nextBilling := domain.NextBillingPeriodEnd(periodEnd, sub.BillingTime, domain.BillingMonthly, e.tenantLocation(ctx, sub.TenantID), sub.BillingAnchorDay)
			slog.Info("skipping billing (trial active)", "subscription_id", sub.ID)
			return sub, true, e.subs.UpdateBillingCycle(ctx, sub.TenantID, sub.ID, periodEnd, nextBilling, nextBilling, sub.BillingAnchorDay)
		}
		// ADR-069: a due cancel schedule means CANCEL FREE — never
		// activate-and-bill. Pre-route on the snapshot; the activation
		// UPDATE's SQL guard is the atomic backstop for a schedule landing
		// after it.
		if sub.CancelAtPeriodEnd || (sub.CancelAt != nil && sub.TrialEndAt != nil && !sub.CancelAt.After(*sub.TrialEndAt)) {
			return sub, true, e.cancelTrialAtEndFromEngine(ctx, sub)
		}
		var firstInv domain.Invoice
		var haveFirstInv bool
		updated, err := e.subs.ActivateAfterTrialWithBill(ctx, sub.TenantID, sub.ID, now, func(tx *sql.Tx, activated domain.Subscription) error {
			// Day-1 in_advance coverage rides the activation tx (ADR-056
			// alignment — the old post-flip BillOnCreate lost the fee on
			// failure while WARNing it was "deferred"). The invoice is
			// CAPTURED for the post-commit finalize below — discarding it
			// left day-1 invoices durable but never tax-committed, audited,
			// or auto-charged (spec-verifier catch, ADR-069 item 8).
			inv, ok, billErr := e.BillOnCreateTx(ctx, tx, activated)
			if billErr != nil {
				return billErr
			}
			firstInv, haveFirstInv = inv, ok
			return nil
		})
		if err != nil {
			if errors.Is(err, domain.ErrTrialCancelDue) {
				// Schedule committed after the snapshot — route to the free
				// cancel. NO invoice, NO cycle advance either way.
				fresh, gErr := e.subs.Get(ctx, sub.TenantID, sub.ID)
				if gErr != nil {
					return sub, true, fmt.Errorf("re-read after trial-cancel guard: %w", gErr)
				}
				return sub, true, e.cancelTrialAtEndFromEngine(ctx, fresh)
			}
			if errors.Is(err, errs.ErrInvalidState) {
				// The row left 'trialing' concurrently. Pre-ADR-069 this
				// fell through and BILLED the stale trialing snapshot —
				// which, once a concurrent scan can CANCEL the sub, means
				// invoicing a canceled customer. Re-read and route on the
				// truth: active → bill fresh below; anything else → done,
				// no invoice, no advance.
				fresh, gErr := e.subs.Get(ctx, sub.TenantID, sub.ID)
				if gErr != nil {
					return sub, true, fmt.Errorf("re-read after activation conflict: %w", gErr)
				}
				if fresh.Status != domain.SubscriptionActive {
					slog.Info("trial resolved to a non-active state by a concurrent writer; skipping billing",
						"subscription_id", sub.ID, "status", fresh.Status)
					return sub, true, nil
				}
				sub = fresh
			} else {
				// Loud: an activation failure must not silently bill the
				// stale snapshot (the pre-fix fall-through).
				return sub, true, fmt.Errorf("auto-activate after trial: %w", err)
			}
		} else {
			sub = updated
			// Post-commit external finalize for the day-1 invoice (audit row,
			// tax commit, auto-charge / retry enrollment) — Stripe calls never
			// ride a DB tx; same contract as the three service-side
			// activation writers.
			if haveFirstInv {
				e.FinalizeOnCreateInvoice(ctx, updated, firstInv)
			}
			slog.Info("trial ended, transitioned to active",
				"subscription_id", sub.ID,
				"tenant_id", sub.TenantID,
			)
			// Audit the auto-flip — third trial-end path (cycle-close);
			// matches the row the operator EndTrial and the scheduler /
			// catchup expiry scans write.
			if e.auditLogger != nil {
				meta := map[string]any{
					"action":       "trial_ended",
					"customer_id":  sub.CustomerID,
					"triggered_by": "schedule",
				}
				_ = e.auditLogger.Log(ctx, sub.TenantID, domain.AuditActionUpdate, "subscription", sub.ID, sub.Code, meta)
			}
			// ADR-031 trial-end in_advance coverage now rides the
			// activation tx above (BillOnCreateTx via WithBill) — atomic
			// with the flip, ADR-056/069.
			// subscription.trial_ended (triggered_by=schedule) is enqueued
			// IN the ActivateAfterTrial tx (store-level DispatchTx subset).
		}
	}
	return sub, false, nil
}

// nominalRate returns the actually-billed CONFIGURED per-unit rate (decimal
// cents) for a FLAT-mode usage line, so the invoice shows the clean rate-card
// figure ($0.000003) instead of the effective amount÷quantity rate, which the
// line's cent-rounding turns into a repeating, slightly-inflated decimal
// ($0.00000333333333) (ADR-054 re-examination). rule is the OVERRIDE-APPLIED
// rule (resolveRatedRule applies any CustomerPriceOverride on top), so this
// captures the negotiated rate, not list price — reading rating_rule_versions
// back would yield list price because the override preserves the row id.
// Returns nil for graduated/package modes: no single nominal rate exists, so
// those correctly fall back to EffectiveUnitAmountDecimal (the honest blend).
func nominalRate(rule domain.RatingRuleVersion) *decimal.Decimal {
	// Single source: the usage Activity panel and the public cost dashboard
	// stamp the same nominal via domain.DisplayUnitAmountDecimalFor, so the
	// unit price can't drift between the invoice and those surfaces.
	return domain.NominalUnitAmountDecimal(rule)
}

// buildLineItems computes the invoice line items and subtotal for one
// billing period: per-item base-fee segments (change-log aware) plus
// per-meter usage charges (multi-dimensional and single-rule paths),
// honoring usage caps, thresholds already billed this cycle, and
// scheduled-cancel terminal-cycle skips. Extracted whole from
// billOnePeriod (behavior-preserving) to separate the pricing
// computation from the billing-cycle orchestration; it remains large
// and is the next candidate for further decomposition.
func (e *Engine) buildLineItems(ctx context.Context, sub domain.Subscription, now, periodStart, periodEnd time.Time, plans map[string]domain.Plan, invoiceCurrency string, meterAggs map[string]string, usageTotals map[string]decimal.Decimal) ([]domain.InvoiceLineItem, int64, error) {
	var lineItems []domain.InvoiceLineItem
	subtotal := int64(0)

	// Detect partial period once — same across all items since they share the
	// billing period. Use math.Round, not int truncation: a sub created
	// at 14:00 was previously billed for 30/31 days because hour-precise
	// duration / 24 truncated to 30. With period boundaries snapped to
	// 00:00 in tenant TZ (subscription.Service), elapsed hours land on
	// (or within ±1h of) a whole-day multiple — DST transitions
	// account for the ±1h tolerance, which Round absorbs.
	periodDays := roundDays(periodEnd.Sub(periodStart))

	// At cycle close, check whether the scheduled cancel will fire at
	// this boundary. If yes, in_advance base lines for the UPCOMING
	// period MUST be skipped — the sub is about to terminate, the
	// customer won't consume the upcoming period, and emitting the
	// line would overcharge by one full prepayment. The cycle-close
	// invoice still bills usage from the just-elapsed period
	// (in-arrears for usage is always correct) and any in_arrears
	// base for the just-elapsed period. After this invoice fires,
	// advanceCycleOrCancel below fires the scheduled cancel.
	// (audit finding flagged during the 2026-05-18 cancel-flow walk
	// through; same bug class would re-appear if hard-pause is ever
	// re-added — paused subs at pause-activation should likewise
	// skip the next-period in_advance base line.)
	terminalCycleClose := shouldFireScheduledCancel(sub, periodEnd, now)

	// Threshold-fired invoice for THIS cycle: when fireThreshold ran
	// mid-cycle and the cycle was NOT reset (reset_billing_cycle=false —
	// or reset=true whose UpdateBillingCycle failed after the fire), that
	// invoice already billed (and typically charged) usage through its
	// billing_period_end PLUS the full in_arrears base fee
	// (evaluateThresholds → previewWithWindow emits the unprorated base).
	// Without this watermark the cycle close re-billed both: usage from
	// periodStart and the base again — every reset=false threshold cycle
	// double-charged. Keyed on the invoice (ground truth), not the
	// mutable BillingThresholds config, so an operator removing
	// thresholds mid-cycle doesn't resurrect the double-bill.
	// errs.ErrNotFound = no threshold fire = zero behavior change; any
	// other error aborts this sub's cycle (billing blind here risks a
	// double charge — feedback_no_silent_fallbacks).
	wm, err := e.loadThresholdWatermark(ctx, sub.TenantID, sub.ID, periodStart, periodEnd)
	if err != nil {
		return nil, 0, err
	}
	thresholdBilledThrough := wm.billedThrough

	// ADR-101 segment source: billing_intervals — policy-applied item
	// lifetimes recorded at write time; the read is a dumb
	// interval×window intersection (Phase 4 removed the fact-log
	// interpretation). Drives segment-aware base-fee billing (Lago /
	// Chargebee / Orb shape): a mid-period plan or quantity change
	// bills one line per segment at the segment's own plan + qty
	// rate × duration fraction; no mid-period changes → one
	// full-period line.
	segsByItem, err := e.windowSegments(ctx, sub, periodStart, periodEnd)
	if err != nil {
		return nil, 0, fmt.Errorf("resolve segments: %w", err)
	}
	// Hydrate any plans referenced only by an interval segment (items
	// removed mid-period, pre-swap plans not on current items). Plans
	// already loaded for current items are skipped.
	//
	// Failure propagates to the per-sub error handler. Pre-fix the
	// segment under a failed plan lookup would be silently dropped from
	// the invoice, undercharging the customer; per
	// feedback_no_silent_fallbacks the engine fails the sub's cycle
	// rather than guess.
	for _, segs := range segsByItem {
		for _, seg := range segs {
			if seg.planID == "" {
				continue
			}
			if _, ok := plans[seg.planID]; ok {
				continue
			}
			pl, err := e.pricing.GetPlan(ctx, sub.TenantID, seg.planID)
			if err != nil {
				return nil, 0, fmt.Errorf("get segment plan %s: %w", seg.planID, err)
			}
			plans[seg.planID] = pl
		}
	}
	// Augment meterAggs with meters from hydrated change-log plans —
	// segment-aware usage needs every meter that was active at ANY
	// point during the period, not just meters on current items'
	// plans. Without this, a meter that existed on plan_A (now
	// swapped out) wouldn't be aggregated in the [seg.start, seg.end)
	// window where plan_A was still active.
	for _, pl := range plans {
		for _, meterID := range pl.MeterIDs {
			if _, ok := meterAggs[meterID]; ok {
				continue
			}
			m, err := e.pricing.GetMeter(ctx, sub.TenantID, meterID)
			if err == nil {
				meterAggs[meterID] = m.Aggregation
			} else {
				meterAggs[meterID] = "sum"
			}
		}
	}

	// Base fee line item — segment-aware for in_arrears, single-line
	// for in_advance (which bills the upcoming period at the post-swap
	// plan; segments don't apply because the previous period was
	// already pre-paid).
	//
	// ADR-031 + segment-aware (Lago/Chargebee/Orb):
	//   - in_arrears: one line per segment within [periodStart, periodEnd].
	//     Multi-segment when the customer changed plan or quantity
	//     mid-period; single-segment otherwise. Each segment bills at
	//     its own rate × (segment_days / cycle_days).
	//   - in_advance: one line for the UPCOMING period at the current
	//     (post-swap) plan. The just-elapsed period's base was paid
	//     upfront at the previous boundary.
	itemsByID := map[string]domain.SubscriptionItem{}
	for _, it := range sub.Items {
		itemsByID[it.ID] = it
	}

	// Pass 1: items currently on the sub.
	for _, it := range sub.Items {
		plan := plans[it.PlanID]
		isAdvance := plan.BaseBillTiming == domain.BillInAdvance

		if isAdvance {
			if terminalCycleClose {
				// Sub cancels at this boundary — don't pre-pay a period
				// that won't be used. Usage on this invoice still
				// captures the just-elapsed consumption.
				continue
			}
			if plan.BaseAmountCents <= 0 {
				continue
			}
			baseStart := periodEnd
			// In_advance base-fee line item label MUST match what the
			// engine will use as the next billing period — uses the
			// billing_time-aware helper so calendar+monthly subs show
			// the calendar-aligned next period on the line item, not
			// the day-of-month-preserved drifted period.
			billingLoc := e.tenantLocation(ctx, sub.TenantID)
			baseEnd := domain.NextBillingPeriodEnd(periodEnd, sub.BillingTime, plan.BillingInterval, billingLoc, sub.BillingAnchorDay)
			baseFee := plan.BaseAmountCents * it.Quantity
			description := fmt.Sprintf("%s - base fee (qty %d)", plan.Name, it.Quantity)
			// Prorate when the upcoming period is shorter than a full
			// plan cycle — e.g. the calendar-snap stub produced after
			// a yearly→monthly plan-change cycle close (new period =
			// `(yearly_end, first-of-next-month)`, 1-30 days). Pre-fix
			// this billed the full monthly base for a 7-day stub.
			// Same shape as BillOnCreate's proration and
			// emitBaseSegmentLine's segDays/fullCycleDays gate.
			advanceDays := roundDays(baseEnd.Sub(baseStart))
			// Absorb a sub-day upcoming period (ADR-091): an org-timezone
			// change (ADR-077, resolved live each roll) can re-time the next
			// boundary onto a sub-day "seam" a few hours past periodEnd. An
			// in_advance base fee is length-insensitive, so billing one for a
			// <1-day window overcharges a full cycle — and roundDays makes the
			// window 0 days, so the proration below silently no-ops and bills
			// full. Skip the line so a cosmetically-motivated org-timezone change
			// never bills a phantom period; the sub re-aligns to clean boundaries
			// on the next full cycle. Mirrors emitBaseSegmentLine's `segDays <= 0`
			// in_arrears absorb (that path was already seam-safe).
			if advanceDays <= 0 {
				continue
			}
			fullCycleDays := roundDays(advanceBillingPeriod(baseStart, plan.BillingInterval, billingLoc, sub.BillingAnchorDay).Sub(baseStart))
			// An org-timezone change (ADR-077, resolved live each roll) re-resolves
			// the stored boundary in the new zone, leaving baseStart OFF the anchor
			// grid.
			// For anniversary/yearly, advanceBillingPeriod re-anchors to the next
			// anchor DAY, so fullCycleDays collapses to the seam width (==
			// advanceDays) and the gate below dies — a ~24h seam would then bill a
			// FULL base fee (the anniversary/yearly analogue of the calendar seam,
			// ADR-091). When baseStart is off-anchor, measure the full cycle
			// NOMINALLY (one calendar interval from baseStart, anchorDay 0) so the
			// short seam prorates instead of overbilling. Calendar billing
			// (anchorDay 0) is reported on-anchor and untouched — its denominator
			// already never collapses (it snaps to the fixed 1st-of-month grid).
			if !domain.IsPeriodStartOnAnchor(baseStart, sub.BillingAnchorDay, billingLoc) {
				fullCycleDays = roundDays(advanceBillingPeriod(baseStart, plan.BillingInterval, billingLoc, 0).Sub(baseStart))
			}
			if advanceDays > 0 && fullCycleDays > 0 && advanceDays < fullCycleDays {
				baseFee = money.RoundHalfToEven(plan.BaseAmountCents*it.Quantity*int64(advanceDays), int64(fullCycleDays))
				description = fmt.Sprintf("%s - base fee (qty %d, prorated %d/%d days)", plan.Name, it.Quantity, advanceDays, fullCycleDays)
			}
			unitAmount := plan.BaseAmountCents
			if it.Quantity > 0 {
				unitAmount = money.RoundHalfToEven(baseFee, it.Quantity)
			}
			baseStartCopy := baseStart
			baseEndCopy := baseEnd
			lineItems = append(lineItems, domain.InvoiceLineItem{
				LineType:           domain.LineTypeBaseFee,
				Description:        description,
				Quantity:           it.Quantity,
				UnitAmountCents:    unitAmount,
				AmountCents:        baseFee,
				TotalAmountCents:   baseFee,
				Currency:           invoiceCurrency,
				BillingPeriodStart: &baseStartCopy,
				BillingPeriodEnd:   &baseEndCopy,
			})
			subtotal += baseFee
			continue
		}

		// in_arrears: emit per-segment lines — UNLESS a threshold invoice
		// already billed this cycle's base. fireThreshold bills the FULL
		// (unprorated) in_arrears base for every item on the sub at fire
		// time, so re-emitting any segment here double-bills it. Known
		// imprecision: an item added AFTER the fire wasn't on the
		// threshold invoice and is under-billed by its partial-period
		// base — accepted; the alternative (segment-level reconciliation
		// against the threshold invoice's lines) over-bills in the
		// mirrored cases, and under-billing is the safer failure for a
		// rare same-cycle combo.
		if thresholdBilledThrough != nil {
			continue
		}
		segments := segsByItem[it.ID]
		for _, seg := range segments {
			segPlan, ok := plans[seg.planID]
			if !ok || segPlan.BaseAmountCents <= 0 {
				continue
			}
			// Skip segments whose plan was in_advance — that portion of
			// the period was already prepaid (BillOnCreate / prior cycle
			// close) and emitting a segment line here would double-bill
			// it. Matches the Pass 2 guard for removed in_advance items
			// below. Lets cross-cadence plan-swaps (in_advance OLD →
			// in_arrears NEW) bill correctly: the OLD prepaid segment
			// is skipped, the NEW in_arrears segment bills its consumed
			// portion at the NEW rate.
			if segPlan.BaseBillTiming == domain.BillInAdvance {
				continue
			}
			emitBaseSegmentLine(seg, segPlan, periodStart, periodDays, invoiceCurrency, e.tenantLocation(ctx, sub.TenantID), sub.BillingTime, &lineItems, &subtotal)
		}
	}

	// Pass 2: items removed mid-period (in the segment map but not on
	// sub.Items now). Bill the pre-remove segments at their own rates.
	// in_arrears only — in_advance items removed mid-period already
	// paid upfront for the period; refund flows through the cancel-
	// proration / removed-item credit path (not this loop).
	// Skipped when a threshold invoice billed this cycle — same
	// double-bill guard as Pass 1 (an item removed after the fire had
	// its full base on the threshold invoice).
	for itemID, segments := range segsByItem {
		if thresholdBilledThrough != nil {
			break
		}
		if _, stillPresent := itemsByID[itemID]; stillPresent {
			continue
		}
		for _, seg := range segments {
			segPlan, ok := plans[seg.planID]
			if !ok || segPlan.BaseAmountCents <= 0 {
				continue
			}
			// Removed-item segments are always in_arrears-style billing
			// (period consumed before remove). in_advance items would
			// have been refunded via the cancel-proration credit path,
			// not billed here.
			if segPlan.BaseBillTiming == domain.BillInAdvance {
				continue
			}
			emitBaseSegmentLine(seg, segPlan, periodStart, periodDays, invoiceCurrency, e.tenantLocation(ctx, sub.TenantID), sub.BillingTime, &lineItems, &subtotal)
		}
	}

	// Segment-aware usage billing (Orb shape): each meter is billed
	// once per disjoint [start, end) range during which it was
	// active on the sub. For subs with no mid-period changes, every
	// meter has exactly one full-period interval — output collapses
	// to the pre-segment-aware single-line behavior. For subs where
	// a mid-period plan change adds/removes meters, each meter
	// bills only for the time it was actually on the sub.
	//
	// Cap math is preserved: the period's totalUsage (pre-cap) and
	// the capScale derived from it apply uniformly across all
	// (meter, interval) pairs.
	capScale := decimal.New(1, 0)
	if sub.UsageCapUnits != nil && *sub.UsageCapUnits > 0 && sub.OverageAction == "block" {
		// usageTotals here is already post-cap (the existing block
		// above scaled it in place). Recover the scale by comparing
		// to a re-aggregated full-period total — but simpler: the
		// cap-scale block above sets a stable factor we can re-derive
		// from sub.UsageCapUnits + sum of post-cap totals.
		postCapTotal := decimal.Zero
		for _, qty := range usageTotals {
			postCapTotal = postCapTotal.Add(qty)
		}
		capDec := decimal.NewFromInt(*sub.UsageCapUnits)
		if postCapTotal.Equal(capDec) && !postCapTotal.IsZero() {
			// Cap actually fired (post-cap total == cap). Compute the
			// pre-cap total by re-aggregating, then derive scale.
			preCapTotals, err := e.usage.AggregateForBillingPeriodByAgg(ctx, sub.TenantID, sub.CustomerID, meterAggs, periodStart, periodEnd)
			if err == nil {
				preCapTotal := decimal.Zero
				for _, qty := range preCapTotals {
					preCapTotal = preCapTotal.Add(qty)
				}
				if preCapTotal.GreaterThan(capDec) && !preCapTotal.IsZero() {
					capScale = capDec.Div(preCapTotal)
				}
			}
		}
	}

	// Build (meter, intervals) maps from item segments + removed-item
	// segments. Each item × segment contributes [seg.start, seg.end]
	// to every meter in its segment's plan.
	//
	// TWO maps (ADR-066 §4): meterIntervals carries the watermark-CLAMPED
	// windows that additive (sum/count) buckets bill from — usage in
	// [periodStart, thresholdBilledThrough) already landed on the mid-cycle
	// threshold invoice. meterIntervalsFull carries the UNCLAMPED windows:
	// non-additive (max/last) buckets that the fire deliberately deferred
	// (no line on the watermark invoice) must aggregate over the full
	// window — clamping a max to the residual window drops a pre-fire peak
	// (partial-window mis-bill), and splitting it double-bills. With no
	// watermark the two maps are identical and only the clamped one is used.
	meterIntervals := map[string][]usageInterval{}
	meterIntervalsFull := map[string][]usageInterval{}
	addMeterInterval := func(mid string, start, end time.Time) {
		if thresholdBilledThrough != nil && end.After(start) {
			meterIntervalsFull[mid] = append(meterIntervalsFull[mid], usageInterval{start, end})
		}
		// Clamp to the threshold watermark. Intervals fully inside the
		// billed window drop out entirely. Nil watermark = no threshold
		// fire = full period, unchanged.
		if thresholdBilledThrough != nil && start.Before(*thresholdBilledThrough) {
			start = *thresholdBilledThrough
		}
		if !end.After(start) {
			return
		}
		meterIntervals[mid] = append(meterIntervals[mid], usageInterval{start, end})
	}
	for _, it := range sub.Items {
		segments := segsByItem[it.ID]
		if len(segments) == 0 {
			// Item present at period_end but no segments (e.g. zero-duration
			// boundary swap collapsed by the helper). Treat the post-change
			// plan's meters as active for the full period — defensive
			// fallback that preserves pre-segment-aware behavior.
			for _, mid := range plans[it.PlanID].MeterIDs {
				addMeterInterval(mid, periodStart, periodEnd)
			}
			continue
		}
		for _, seg := range segments {
			segPlan, ok := plans[seg.planID]
			if !ok {
				continue
			}
			for _, mid := range segPlan.MeterIDs {
				addMeterInterval(mid, seg.start, seg.end)
			}
		}
	}
	for itemID, segments := range segsByItem {
		if _, stillPresent := itemsByID[itemID]; stillPresent {
			continue
		}
		for _, seg := range segments {
			segPlan, ok := plans[seg.planID]
			if !ok {
				continue
			}
			for _, mid := range segPlan.MeterIDs {
				addMeterInterval(mid, seg.start, seg.end)
			}
		}
	}

	// Cache per-interval aggregation so a meter active across N
	// non-overlapping intervals incurs at most N queries (and 1 query
	// when N == 1 and the interval equals the full period — we use
	// the precomputed usageTotals in that case).
	intervalAggCache := map[string]map[string]decimal.Decimal{}
	intervalKey := func(iv usageInterval) string {
		return iv.start.UTC().Format(time.RFC3339Nano) + "|" + iv.end.UTC().Format(time.RFC3339Nano)
	}
	// Separate cache for the multi-dim (pricing-rules) path — keyed by
	// meter + interval since AggregateByPricingRules is per-meter.
	ruleAggCache := map[string][]domain.RuleAggregation{}
	one := decimal.New(1, 0)

	// Union of meters with billable windows. When a watermark exists, a
	// meter whose clamped intervals vanished entirely (all usage inside the
	// fire's window) can still owe a full-window bill for a deferred
	// non-additive bucket — so iterate the full map's keys too.
	meterIDSet := make(map[string]struct{}, len(meterIntervals))
	for mid := range meterIntervals {
		meterIDSet[mid] = struct{}{}
	}
	for mid := range meterIntervalsFull {
		meterIDSet[mid] = struct{}{}
	}
	// A meterWindow is one aggregation pass: the clamped pass
	// (nonAdditiveOnly=false) bills every bucket EXCEPT deferred
	// non-additive ones; the full pass (nonAdditiveOnly=true, only when a
	// watermark exists) bills ONLY deferred non-additive buckets over the
	// unclamped window.
	type meterWindow struct {
		iv              usageInterval
		nonAdditiveOnly bool
	}
	for meterID := range meterIDSet {
		meter, err := e.pricing.GetMeter(ctx, sub.TenantID, meterID)
		if err != nil {
			return nil, 0, fmt.Errorf("get meter %s: %w", meterID, err)
		}

		// Does this meter price via dimension-match pricing rules (the
		// multi-dim shape: e.g. the AI `tokens` meter carries one rule per
		// {model, token_type})? If so we MUST use the same
		// AggregateByPricingRules + per-bucket pricing path that preview and
		// the threshold scan use — a single meter-linked rating rule can't
		// express N rates, so the legacy single-rule branch below would skip
		// the meter and emit a $0 invoice (the wedge breakage, ADR-044).
		// previewMeter is the mirror of this loop: same path → preview ==
		// invoice. Meters with no pricing rules keep the single-rule path.
		pricingRules, err := e.pricing.ListMeterPricingRulesByMeter(ctx, sub.TenantID, meterID)
		if err != nil {
			return nil, 0, fmt.Errorf("list pricing rules for meter %s: %w", meterID, err)
		}
		rulesByID := make(map[string]domain.MeterPricingRule, len(pricingRules))
		for _, r := range pricingRules {
			rulesByID[r.ID] = r
		}

		windows := make([]meterWindow, 0, 4)
		for _, iv := range mergeUsageIntervals(meterIntervals[meterID]) {
			windows = append(windows, meterWindow{iv: iv})
		}
		if thresholdBilledThrough != nil {
			for _, iv := range mergeUsageIntervals(meterIntervalsFull[meterID]) {
				windows = append(windows, meterWindow{iv: iv, nonAdditiveOnly: true})
			}
		}
		for _, w := range windows {
			iv := w.iv
			ivStart := iv.start
			ivEnd := iv.end
			fullPeriod := iv.start.Equal(periodStart) && iv.end.Equal(periodEnd)

			// ---- Multi-dim path: one billable bucket per claimed rule. ----
			if len(pricingRules) > 0 {
				defaultMode := mapMeterAggregation(meter.Aggregation)
				cacheK := meterID + "|" + intervalKey(iv)
				aggs, cached := ruleAggCache[cacheK]
				if !cached {
					a, err := e.usage.AggregateByPricingRules(ctx, sub.TenantID, sub.CustomerID, meterID, defaultMode, iv.start, iv.end)
					if err != nil {
						return nil, 0, fmt.Errorf("aggregate by pricing rules for meter %s [%v, %v): %w", meterID, iv.start, iv.end, err)
					}
					aggs = a
					ruleAggCache[cacheK] = a
				}
				for _, agg := range aggs {
					qty := agg.Quantity
					// AggregateByPricingRules always returns raw (pre-cap)
					// quantities, so apply the cap scale here in BOTH the
					// full-period and sub-period cases (unlike usageTotals,
					// which is pre-scaled in place above).
					if !capScale.Equal(one) {
						qty = qty.Mul(capScale)
					}
					if qty.IsZero() {
						continue
					}
					ratingRuleID := agg.RatingRuleVersionID
					if ratingRuleID == "" {
						ratingRuleID = meter.RatingRuleVersionID
					}
					if ratingRuleID == "" {
						// Usage that matched no rule and the meter has no
						// default binding — bill nothing, but loudly (a
						// silent skip would hide unbilled revenue).
						slog.Warn("cycle: usage matched no rating rule and meter has no default binding — not billed",
							"meter", meter.Key, "quantity", qty.String(),
							"subscription_id", sub.ID)
						continue
					}
					// ADR-066 §4 pass routing, per RULE BUCKET: a deferred
					// non-additive bucket (max/last with NO line on the
					// watermark fire) bills ONLY on the full-window pass —
					// billing it from the clamped window drops pre-fire peaks,
					// and billing it on both passes double-bills. Every other
					// bucket (additive, or non-additive that already rode the
					// fire — the accepted partial-window degradation) bills
					// ONLY on the clamped pass. No watermark ⇒ deferred is
					// always false and no full pass exists: unchanged.
					if wm.deferredBucket(agg.AggregationMode, meterID, ratingRuleID) != w.nonAdditiveOnly {
						continue
					}
					rule, err := e.resolveRatedRule(ctx, sub.TenantID, sub.CustomerID, ratingRuleID, periodStart)
					if err != nil {
						return nil, 0, fmt.Errorf("resolve pricing for meter %s rule %s: %w", meterID, ratingRuleID, err)
					}
					amount, err := domain.ComputeAmountCents(rule, qty)
					if err != nil {
						return nil, 0, fmt.Errorf("compute amount for meter %s rule %s: %w", meterID, ratingRuleID, err)
					}
					unitAmount := decimal.NewFromInt(amount).Div(qty).RoundBank(0).IntPart()
					lineItems = append(lineItems, domain.InvoiceLineItem{
						LineType:                 domain.LineTypeUsage,
						MeterID:                  meterID,
						Description:              usageLineDescription(meter, rulesByID[agg.RuleID]),
						Quantity:                 qty.IntPart(),
						QuantityDecimal:          qty,
						UnitAmountCents:          unitAmount,
						AmountCents:              amount,
						TotalAmountCents:         amount,
						Currency:                 invoiceCurrency,
						PricingMode:              string(rule.Mode),
						RatingRuleVersionID:      rule.ID,
						NominalUnitAmountDecimal: nominalRate(rule),
						BillingPeriodStart:       &ivStart,
						BillingPeriodEnd:         &ivEnd,
					})
					subtotal += amount
				}
				continue
			}

			// ---- Single-rule path (pre-multi-dim meters). ----
			// Same ADR-066 §4 pass routing as the multi-dim branch, at meter
			// granularity (a single-rule meter has exactly one bucket, whose
			// mode is the meter's own aggregation).
			if wm.deferredBucket(mapMeterAggregation(meter.Aggregation), meterID, meter.RatingRuleVersionID) != w.nonAdditiveOnly {
				continue
			}
			var quantity decimal.Decimal
			if fullPeriod {
				quantity = usageTotals[meterID]
			} else {
				key := intervalKey(iv)
				totals, cached := intervalAggCache[key]
				if !cached {
					t, err := e.usage.AggregateForBillingPeriodByAgg(ctx, sub.TenantID, sub.CustomerID, meterAggs, iv.start, iv.end)
					if err != nil {
						return nil, 0, fmt.Errorf("aggregate usage for segment [%v, %v): %w", iv.start, iv.end, err)
					}
					totals = t
					intervalAggCache[key] = t
				}
				quantity = totals[meterID]
				// Apply cap scale to sub-period quantities so the
				// per-interval sum matches the cap-scaled full-period
				// total. Cap doesn't apply if capScale == 1.
				if !capScale.Equal(one) {
					quantity = quantity.Mul(capScale)
				}
			}
			if quantity.IsZero() {
				continue
			}

			if meter.RatingRuleVersionID == "" {
				// Unpriced meter → its usage can't be rated. Never silent
				// (ADR-096): the plan-attach guard blocks this at authoring,
				// so reaching it means a rule was unbound out from under a
				// live sub. Loud log, no revenue billed — matches the
				// multi-dim sibling above.
				slog.Warn("cycle: usage on a meter with no rating rule — not billed (unpriced meter on plan; ADR-096)",
					"meter", meter.Key, "quantity", quantity.String(), "subscription_id", sub.ID)
				continue
			}
			rule, err := e.resolveRatedRule(ctx, sub.TenantID, sub.CustomerID, meter.RatingRuleVersionID, periodStart)
			if err != nil {
				return nil, 0, fmt.Errorf("resolve pricing for meter %s: %w", meterID, err)
			}
			amount, err := domain.ComputeAmountCents(rule, quantity)
			if err != nil {
				return nil, 0, fmt.Errorf("compute amount for meter %s: %w", meterID, err)
			}
			unitAmount := decimal.NewFromInt(amount).Div(quantity).RoundBank(0).IntPart()

			lineItems = append(lineItems, domain.InvoiceLineItem{
				LineType:                 domain.LineTypeUsage,
				MeterID:                  meterID,
				Description:              fmt.Sprintf("%s (%s)", meter.Name, meter.Unit),
				Quantity:                 quantity.IntPart(),
				QuantityDecimal:          quantity,
				UnitAmountCents:          unitAmount,
				AmountCents:              amount,
				TotalAmountCents:         amount,
				Currency:                 invoiceCurrency,
				PricingMode:              string(rule.Mode),
				RatingRuleVersionID:      rule.ID,
				NominalUnitAmountDecimal: nominalRate(rule),
				BillingPeriodStart:       &ivStart,
				BillingPeriodEnd:         &ivEnd,
			})
			subtotal += amount
		}
	}

	return lineItems, subtotal, nil
}

func (e *Engine) billOnePeriod(ctx context.Context, sub domain.Subscription) (bool, error) {
	ctx, span := telemetry.Tracer("billing").Start(ctx, "billing.BillSubscription",
		trace.WithAttributes(
			attribute.String("subscription_id", sub.ID),
			attribute.String("tenant_id", sub.TenantID),
			attribute.String("customer_id", sub.CustomerID),
		),
	)
	defer span.End()

	// Guard: only bill active or trialing subscriptions. Trialing subs flow
	// through to the trial state machine below, which either advances the
	// cycle without billing (trial active) or atomically flips to active and
	// then bills (trial elapsed).
	if sub.Status != domain.SubscriptionActive && sub.Status != domain.SubscriptionTrialing {
		slog.Info("skipping billing (not active)", "subscription_id", sub.ID, "status", sub.Status)
		return false, nil
	}

	if sub.CurrentBillingPeriodStart == nil || sub.CurrentBillingPeriodEnd == nil {
		return false, fmt.Errorf("subscription has no billing period set")
	}

	// Resolve "now" as this cycle's own close instant — sub.NextBillingAt
	// at entry, falling back to the clock's frozen_time / wall-clock only
	// when NextBillingAt is unset (unreachable from billSubscription's
	// caught-up check, but kept defensive).
	//
	// Anchoring on the cycle boundary keeps every per-cycle decision in
	// the time domain the cycle belongs to:
	//   - IssuedAt / CreatedAt / DueAt stamped at the cycle close,
	//     not at advance-end frozen_time. Multi-period catchup now
	//     produces one invoice per cycle with its own period-correct
	//     timestamp instead of all invoices sharing advance-end.
	//   - Pause auto-resume gate evaluates at cycle close: a pause
	//     scheduled to resume mid-cycle is honored — the May 1 cycle
	//     does NOT bill if pause resumes May 5, even when the advance
	//     lands at May 20. Industry parity (Stripe, Lago, Orb).
	//   - Trial-end activation stamps `activated_at` at the cycle's
	//     own boundary instead of advance-end.
	//   - Cancel-at-period-end / scheduled-change applications stamp
	//     their `canceled_at` / `applied_at` at the cycle boundary.
	//   - Tax calculation date matches the cycle, so the tax rate
	//     applicable at the cycle close (not at advance-end) is used.
	//   - MarkPaid for zero-amount auto-paid invoices stamps PaidAt at
	//     the cycle close, aligning with IssuedAt.
	//
	// In the cron (wall-clock) path NextBillingAt ≈ time.Now() within
	// one scheduler tick, so the change is neutral — invoice timestamps
	// land exactly on the period boundary instead of "few minutes after,"
	// which is the more defensible cosmetic anyway.
	now := e.effectiveNow(ctx, sub)
	if sub.NextBillingAt != nil {
		now = *sub.NextBillingAt
	}
	// Rebind ctx's instant to this cycle's own close (keeping the clock)
	// so downstream effects that read ctx time — the auto-charge, whose
	// PI metadata anchor drives the settle's paid_at — land on the
	// boundary like every other cycle stamp, not on advance-end
	// frozen_time (live find: a Jul-1-boundary invoice recorded
	// paid_at at the advance target an hour later).
	if sim, ok := clock.SimOf(ctx); ok {
		ctx = clock.WithSim(ctx, clock.Sim{At: now, TestClockID: sim.TestClockID})
	}

	// Pause auto-resume is no longer evaluated here — it now runs as a
	// dedicated phase BEFORE this loop (Scheduler.pauseResumer for
	// wall-clock, testclock orchestrator Phase 0.7 for clock-pinned).
	// That phase clears pause_collection at resumes_at directly,
	// matching Stripe-parity "resume AT resumes_at" semantics; by the
	// time we reach this code the pause has either already been
	// cleared or it's still genuinely in force. The old in-cycle gate
	// silently leaked any sub whose next_billing_at was further out
	// than resumes_at — first surfaced by a test-clock advance past
	// resumes_at on a sub whose cycle wasn't due. See ADR-038.

	// Apply any scheduled plan changes due at this cycle boundary before
	// reading plans (see applyDueScheduledPlanChanges).
	sub, err := e.applyDueScheduledPlanChanges(ctx, sub, now)
	if err != nil {
		return false, err
	}

	if len(sub.Items) == 0 {
		return false, fmt.Errorf("subscription has no items to bill")
	}

	periodStart := *sub.CurrentBillingPeriodStart
	periodEnd := *sub.CurrentBillingPeriodEnd

	// Trial lifecycle: advance-without-bill, cancel-free, or flip-to-active.
	sub, trialDone, err := e.handleTrialState(ctx, sub, now, periodStart, periodEnd)
	if trialDone {
		return false, err
	}

	// Gather pricing inputs for this period (plans, currency, usage).
	plans, invoiceCurrency, meterAggs, usageTotals, err := e.resolveBillingInputs(ctx, sub, periodStart, periodEnd)
	if err != nil {
		return false, err
	}

	// Build the invoice line items (base fees + usage) for this period.
	lineItems, subtotal, err := e.buildLineItems(ctx, sub, now, periodStart, periodEnd, plans, invoiceCurrency, meterAggs, usageTotals)
	if err != nil {
		return false, err
	}

	// Skip empty cycle-close invoices — matches BillOnCreate's and
	// BillFinalOnImmediateCancel's existing zero-subtotal guards and
	// the Stripe / Lago / Chargebee / Orb convention of NOT emitting
	// a $0 invoice when there's literally nothing to bill. Common
	// triggers in practice:
	//
	//   - in_advance plan + scheduled cancel-at-period-end (PR-9):
	//     upcoming-period base line is skipped because the customer
	//     won't use the period; if there's no usage to bill for the
	//     just-elapsed period either, the result is zero line items.
	//   - in_arrears item removed mid-period: pre-remove segment
	//     emits, but post-remove there's no current item, so the
	//     next cycle's invoice may have zero base lines.
	//   - Pure-trial period closure: nothing to bill yet.
	//
	// Still advance the cycle so the period anchor moves forward —
	// the absence of an invoice doesn't mean the period didn't pass.
	// No invoice number is consumed (NextInvoiceNumber is monotonic;
	// burning one on a phantom invoice creates audit gaps).
	if subtotal == 0 && len(lineItems) == 0 {
		nextPeriodStart := periodEnd
		nextPeriodEnd := domain.NextBillingPeriodEnd(periodEnd, sub.BillingTime, plans[sub.Items[0].PlanID].BillingInterval, e.tenantLocation(ctx, sub.TenantID), sub.BillingAnchorDay)
		if err := e.advanceCycleOrCancel(ctx, sub, periodEnd, nextPeriodStart, nextPeriodEnd, now); err != nil {
			return false, fmt.Errorf("advance billing cycle (no-op invoice): %w", err)
		}
		slog.Info("cycle close skipped — no billable lines",
			"subscription_id", sub.ID,
			"period_start", periodStart,
			"period_end", periodEnd,
		)
		return false, nil
	}

	// Create invoice — pull settings for payment terms + tax, then allocate the
	// invoice number as a strictly monotonic per-tenant sequence. No fallback:
	// a collision-prone number is worse than a failed billing tick since the
	// tick will retry, while a duplicate invoice number corrupts accounting.
	// `now` was resolved at the top of billSubscription via effectiveNow —
	// reuse it so invoice timestamps sit on the same timeline as the rest of
	// this call (matters for test-clock subs where wall-clock ≠ frozen_time).
	netDays := 30

	if e.settings == nil {
		return false, fmt.Errorf("billing engine: settings reader is required for invoice numbering")
	}
	if ts, err := e.settings.Get(ctx, sub.TenantID); err == nil && ts.NetPaymentTerms > 0 {
		netDays = ts.NetPaymentTerms
	}
	invoiceNumber, err := e.settings.NextInvoiceNumber(ctx, sub.TenantID)
	if err != nil {
		return false, fmt.Errorf("allocate invoice number: %w", err)
	}

	// Coupons removed 2026-05-29 (Phase A1). Discount stays at zero;
	// AI-native discount intent flows through the credit ledger.
	var discountCents int64
	taxApp, err := e.ApplyTaxToLineItems(ctx, sub.TenantID, sub.CustomerID, invoiceCurrency, subtotal, discountCents, lineItems)
	if err != nil {
		// A tax failure must abort before invoice create/finalize and
		// before the cycle advances — otherwise a transient DB blip would
		// finalize a $0 invoice and burn the period. Returning the error
		// leaves the sub untouched so the next tick retries.
		return false, fmt.Errorf("apply tax: %w", err)
	}
	// In tax-inclusive mode the engine back-calculates net subtotal/discount
	// from the gross inputs; in exclusive mode these pass through unchanged,
	// so the caller always reads the authoritative values off the result.
	totalWithTax := taxApp.SubtotalCents - taxApp.DiscountCents + taxApp.TaxAmountCents
	dueAt := now.AddDate(0, 0, netDays)

	// Single source of truth for the draft/finalized decision —
	// shared across all four invoice-emitting paths (cycle close,
	// BillOnCreate, BillFinalOnImmediateCancel, handleItemProration).
	// Encodes: tax pending → draft; pause_collection set → draft;
	// otherwise finalized. See domain.InvoiceFinalizationStatus.
	invStatus := domain.InvoiceFinalizationStatus(taxApp.TaxStatus, sub.PauseCollection)
	collectionPaused := sub.PauseCollection != nil

	// ADR-031: when ANY plan on the sub is in_advance, the cycle
	// invoice's header period shifts to the UPCOMING period. The base
	// for the upcoming period dominates the invoice's intent — usage
	// from the just-elapsed period rides along on dedicated line
	// items (those keep their own elapsed-period stamps set above).
	// This shift is what lets the day-1 (subscription_create) invoice
	// and the cycle-close (subscription_cycle) invoice coexist under
	// the (sub_id, period_start, period_end) UNIQUE constraint — they
	// land on different periods.
	invoicePeriodStart, invoicePeriodEnd := periodStart, periodEnd
	for _, it := range sub.Items {
		if plans[it.PlanID].BaseBillTiming == domain.BillInAdvance {
			invoicePeriodStart = periodEnd
			// Invoice header for an in_advance sub covers the upcoming
			// period — must match what the sub's next current_period_*
			// will be set to (computed via NextBillingPeriodEnd below
			// at cycle close). Diverging here would leave the invoice
			// header period and the sub's tracked period out of sync.
			invoicePeriodEnd = domain.NextBillingPeriodEnd(periodEnd, sub.BillingTime, plans[it.PlanID].BillingInterval, e.tenantLocation(ctx, sub.TenantID), sub.BillingAnchorDay)
			break
		}
	}

	// ATOMIC: Create invoice + all line items in a single transaction.
	// This prevents orphaned invoices with missing line items on partial failure.
	// The unique index on (tenant_id, subscription_id, billing_period_start, billing_period_end)
	// provides idempotency — duplicate calls return an error instead of double-billing.
	inv, err := e.invoices.CreateInvoiceWithLineItemsAudited(ctx, sub.TenantID, domain.Invoice{
		CustomerID:      sub.CustomerID,
		SubscriptionID:  sub.ID,
		BillingTimezone: e.tenantLocation(ctx, sub.TenantID).String(),
		InvoiceNumber:   invoiceNumber,
		Status:          invStatus,
		PaymentStatus:   domain.PaymentPending,
		Currency:        invoiceCurrency,
		SubtotalCents:   taxApp.SubtotalCents,
		DiscountCents:   taxApp.DiscountCents,
		// ONE assignment carries every tax fact (see domain/tax_facts.go).
		TaxFacts:           taxApp.TaxFacts,
		TotalAmountCents:   totalWithTax,
		AmountDueCents:     totalWithTax,
		BillingPeriodStart: invoicePeriodStart,
		BillingPeriodEnd:   invoicePeriodEnd,
		IssuedAt:           &now,
		DueAt:              &dueAt,
		// CreatedAt = clock.Now() so test-clock-driven invoices land
		// created_at on simulation time (matching issued_at). Pre-fix
		// the store fell back to time.Now() (wall-clock) and the
		// activity timeline showed split-brain timestamps — created
		// on real time, issued on test-clock time.
		CreatedAt:          now,
		NetPaymentTermDays: netDays,
		BillingReason:      domain.BillingReasonSubscriptionCycle,
		// Persist whether this invoice's domain timestamps were stamped on a
		// frozen test clock (the subscription is pinned), so the activity
		// timeline + header render the simulated badge without re-deriving
		// from the mutable test_clock_id at read time. The manual-invoice path
		// captures the same signal from the customer's pin (invoice.Service).
		IsSimulated: sub.TestClockID != "",
	}, lineItems, e.finalizeAuditEmit(ctx))
	if err != nil {
		// Idempotency: if this invoice already exists (UNIQUE violation on the
		// per-subscription+period constraint), the store returns errs.ErrAlreadyExists.
		// Match on the sentinel, not err.Error() substrings — translated messages,
		// wrapped errors, or DB driver changes would silently break substring matches
		// and cause duplicate charges in multi-worker retries.
		if errors.Is(err, errs.ErrAlreadyExists) {
			slog.Info("invoice already exists for billing period (idempotent skip)",
				"subscription_id", sub.ID,
				"period_start", periodStart,
				"period_end", periodEnd,
			)
			// The invoice already exists, so two pieces of state can still be
			// missing (a prior tick crashed mid-flow — create-then-mark and
			// create-then-advance are separate store writes):
			//
			// 1. A zero-due MarkPaid (ADR-066): an invoice born $0 or fully
			//    credited whose MarkPaid never ran. Best-effort — a MarkPaid
			//    failure WARNs and the invoice stays visible in the attention
			//    queue (operator backstop) rather than blocking the advance
			//    below: stalling all future billing over a $0 paid-mark would
			//    be the worse trade. Runs before the advance so the one
			//    re-entry this sub gets still attempts it. ErrNotFound
			//    (blocking row voided since) skips the heal; other fetch
			//    errors stay loud.
			if existing, gErr := e.invoices.GetInvoiceForPeriod(ctx, sub.TenantID, sub.ID, periodStart, periodEnd); gErr == nil {
				e.healStrandedZeroDue(ctx, sub.TenantID, existing, now)
			} else if !errors.Is(gErr, errs.ErrNotFound) {
				return false, fmt.Errorf("fetch existing invoice (heal path): %w", gErr)
			}
			// 2. The cycle advance. Heal it, and surface a failure
			// LOUDLY, matching the happy-path advance below. A swallowed error
			// here strands the watermark: the sub stays due, every subsequent
			// tick re-hits this skip, and nothing ever alerts. The benign
			// concurrent-cancel race (ErrInvalidState) is already absorbed to
			// nil inside advanceCycleOrCancel, so this won't false-positive.
			nextPeriodStart := periodEnd
			nextPeriodEnd := domain.NextBillingPeriodEnd(periodEnd, sub.BillingTime, plans[sub.Items[0].PlanID].BillingInterval, e.tenantLocation(ctx, sub.TenantID), sub.BillingAnchorDay)
			if err := e.advanceCycleOrCancel(ctx, sub, periodEnd, nextPeriodStart, nextPeriodEnd, now); err != nil {
				return false, fmt.Errorf("advance billing cycle (heal path): %w", err)
			}
			return false, nil
		}
		return false, fmt.Errorf("create invoice: %w", err)
	}

	// The finalize AUDIT row already committed inside the create tx above
	// (finalizeAuditEmit → LogInTx, ADR-090 shared fate): a finalized invoice
	// cannot exist without it. Here we fire only the post-commit WEBHOOK — an
	// external effect that must not ride the DB tx.
	e.dispatchInvoiceFinalized(ctx, sub, inv)

	// Stripe Tax: once the invoice is durably persisted, commit the
	// tax_calculation into a tax_transaction so Stripe can report the
	// liability. Failures here don't unwind the invoice — the calculation
	// row survives as an audit trail and we surface the failure via logs
	// + metrics for the tenant to reconcile. Manual/none providers have
	// Commit as a no-op so this path is safe to call unconditionally.
	if inv.TaxProvider != "" && inv.TaxCalculationID != "" {
		if err := e.CommitTax(ctx, sub.TenantID, inv.ID, inv.TaxCalculationID); err != nil {
			slog.Warn("tax: commit failed after invoice creation",
				"error", err,
				"tenant_id", sub.TenantID,
				"invoice_id", inv.ID,
				"provider", inv.TaxProvider,
				"tax_calculation_id", inv.TaxCalculationID)
		}
	}

	// Apply customer credits before charging. ApplyToInvoice is atomic:
	// it both debits the credit ledger AND reduces the invoice's amount_due_cents
	// in a single transaction. A failure leaves both unchanged — no dual-write
	// hole where credits are consumed but the invoice still shows the pre-credit
	// amount due (which would double-bill the customer via Stripe).
	//
	// Skip during pause_collection — credits should not be consumed against a
	// draft invoice that may never be finalized; the credit will apply when
	// collection resumes and the invoice transitions out of draft.
	//
	// Pre-fix bug (caught 2026-05-30 design-debt audit): a DB blip in
	// ApplyToInvoiceAt would log + continue, then the auto-charge block
	// below would charge Stripe the FULL pre-credit total — silently
	// overcharging the customer by the credit balance amount. Fix:
	// flag the invoice for scheduler retry and skip the downstream
	// MarkPaid + auto-charge blocks so the next RetryPendingCharges
	// tick can re-apply credits atomically with the charge.
	creditApplyOK := true
	if e.credits != nil && totalWithTax > 0 && !collectionPaused {
		credited, err := e.credits.ApplyToInvoiceAt(ctx, sub.TenantID, sub.CustomerID, inv.ID, totalWithTax, now, inv.InvoiceNumber)
		if err != nil {
			slog.Warn("failed to apply credits — flagging for retry; auto-charge skipped to avoid overcharge",
				"invoice_id", inv.ID, "error", err)
			creditApplyOK = false
			if err := e.invoices.SetAutoChargePending(ctx, sub.TenantID, inv.ID, true); err != nil {
				// A failed set(true) is a liveness sink: the invoice stays
				// invisible to RetryPendingCharges forever (playbook class G).
				slog.Warn("failed to queue invoice for charge retry", "invoice_id", inv.ID, "error", err)
			}
		} else if credited > 0 {
			slog.Info("credits applied to invoice",
				"invoice_id", inv.ID,
				"credited_cents", credited,
			)
		}
	}

	// If credits covered 100%, mark as paid immediately (no Stripe
	// charge needed) — BUT only when the invoice was finalized at
	// create time (i.e., tax_status=ok and pause_collection unset, per
	// InvoiceFinalizationStatus). For draft invoices (tax pending or
	// pause-collection set), skip the MarkPaid call: leave the
	// invoice draft with credits already applied. A tax-pending draft
	// auto-finalizes later via the tax-retry chain when tax resolves; a
	// pause-collection draft stays draft until the operator finalizes it
	// (resume clears the pause and the next cycle generates finalized
	// invoices, but the accrued drafts are NOT auto-finalized — there is
	// no pause-resume auto-finalize chain).
	//
	// Pre-fix bug (caught 2026-05-22): this block called MarkPaid
	// regardless of status, transitioning a tax-pending draft directly
	// to paid. The customer was charged subtotal-only (tax_amount=0)
	// and tax retry blocked forever (retry requires status='draft',
	// but status was 'paid'). Customer DEMO-000906 demonstrated.
	//
	// ADR-066: no `totalWithTax > 0` conjunct. A cycle invoice born $0
	// (zero-priced usage lines are emitted per the zero-amount line
	// convention) was stranded payment_pending forever — never charged
	// (amount_due=0 skips the charge arm), never paid, permanently
	// "awaiting payment" in the attention queue. Stripe parity:
	// zero-amount invoices auto-mark paid with no payment attempt.
	if creditApplyOK && inv.Status == domain.InvoiceFinalized {
		updatedInv, err := e.invoices.GetInvoice(ctx, sub.TenantID, inv.ID)
		if err == nil && updatedInv.AmountDueCents <= 0 {
			// Reuse the sub-scoped `now` so fully-credit-paid invoices on a
			// test clock get paid_at from the frozen timeline, not wall-clock.
			if _, err := e.invoices.MarkPaid(ctx, sub.TenantID, inv.ID, "", now); err != nil {
				slog.Warn("failed to mark fully-credited invoice as paid", "invoice_id", inv.ID, "error", err)
			} else {
				slog.Info("invoice fully covered by credits, marked as paid", "invoice_id", inv.ID)
				// Background credit settle bypasses the invoice handler's dunning
				// resolve — close any active run so it isn't left stale (ADR-040
				// framework: post-commit best-effort idempotent state-correctness).
				e.resolveDunningRecovered(ctx, sub.TenantID, inv.ID)
				// Still advance the billing cycle (billing_time-aware
				// so calendar subs auto-realign on credit-paid cycles too).
				nextPeriodStart := periodEnd
				nextPeriodEnd := domain.NextBillingPeriodEnd(periodEnd, sub.BillingTime, plans[sub.Items[0].PlanID].BillingInterval, e.tenantLocation(ctx, sub.TenantID), sub.BillingAnchorDay)
				if err := e.advanceCycleOrCancel(ctx, sub, periodEnd, nextPeriodStart, nextPeriodEnd, now); err != nil {
					return true, fmt.Errorf("advance billing cycle: %w", err)
				}
				return true, nil
			}
		}
	}

	// Auto-charge: synchronous with timeout. If it fails, mark for scheduler retry
	// instead of fire-and-forget goroutine that loses failures.
	//
	// Skip entirely when pause_collection is set — the invoice is draft so
	// charging it would be a state-violation; dunning is also off the table
	// because finalize hasn't happened. This is the Stripe-parity behavior:
	// pause_collection neuters the financial side without touching the cycle.
	if creditApplyOK && inv.AmountDueCents > 0 && !collectionPaused {
		e.collectAfterFinalize(ctx, sub, inv, "cycle close")
	}

	// Advance billing cycle (or fire scheduled cancel if due). Uses
	// domain.NextBillingPeriodEnd (NOT the legacy interval-only
	// advanceBillingPeriod) so calendar-billing subs whose anchor day
	// drifted from a prior plan-interval change auto-re-align to the
	// next calendar boundary instead of carrying the drifted day
	// forward forever.
	nextPeriodStart := periodEnd
	nextPeriodEnd := domain.NextBillingPeriodEnd(periodEnd, sub.BillingTime, plans[sub.Items[0].PlanID].BillingInterval, e.tenantLocation(ctx, sub.TenantID), sub.BillingAnchorDay)

	if err := e.advanceCycleOrCancel(ctx, sub, periodEnd, nextPeriodStart, nextPeriodEnd, now); err != nil {
		return false, fmt.Errorf("advance billing cycle: %w", err)
	}

	slog.Info("invoice generated",
		"invoice_id", inv.ID,
		"subscription_id", sub.ID,
		"total_cents", totalWithTax,
		"tax_rate", taxApp.TaxRate,
		"line_items", len(lineItems),
	)

	return true, nil
}

// BillOnCreate emits the day-1 invoice for an in_advance subscription
// (ADR-031). Called by subscription.Service.Create immediately after
// the sub is persisted. Returns a zero Invoice + nil when the sub
// has no in_advance items — the cycle path will pick the sub up at
// period close as usual.
//
// Scope of the invoice:
//   - Period: [CurrentBillingPeriodStart, CurrentBillingPeriodEnd].
//     Mid-period creation prorates the base by remaining days
//     (matches the cycle path).
//   - Line items: base fee for each in_advance item. Arrears items
//     contribute nothing — their base waits for cycle close.
//   - Usage: zero. No events have happened yet at t=0.
//   - billing_reason = subscription_create (distinguishes from the
//     subscription_cycle invoice that lands at period close with
//     usage from this same elapsed period).
//
// Idempotent via the standard (subscription_id, period_start,
// period_end) UNIQUE constraint: if BillOnCreate runs twice (e.g. a
// retry after a transient failure), the second call gets
// ErrAlreadyExists and returns the existing invoice's nil state — no
// duplicate. Auto-charge is best-effort: PM ready → synchronous
// charge with 30s timeout; no PM → queue auto_charge_pending and
// fire the no-PM notifier email (same path as the cycle invoice).
func (e *Engine) BillOnCreate(ctx context.Context, sub domain.Subscription) (domain.Invoice, error) {
	ctx, span := telemetry.Tracer("billing").Start(ctx, "billing.BillOnCreate",
		trace.WithAttributes(
			attribute.String("subscription_id", sub.ID),
			attribute.String("tenant_id", sub.TenantID),
			attribute.String("customer_id", sub.CustomerID),
		),
	)
	defer span.End()

	inv, lineItems, ok, err := e.buildOnCreateInvoice(ctx, sub)
	if err != nil {
		return domain.Invoice{}, err
	}
	if !ok {
		return domain.Invoice{}, nil
	}

	invoiceNumber, err := e.settings.NextInvoiceNumber(ctx, sub.TenantID)
	if err != nil {
		return domain.Invoice{}, fmt.Errorf("mint invoice number: %w", err)
	}
	inv.InvoiceNumber = invoiceNumber

	created, err := e.invoices.CreateInvoiceWithLineItemsAudited(ctx, sub.TenantID, inv, lineItems, e.finalizeAuditEmit(ctx))
	if err != nil {
		if errors.Is(err, errs.ErrAlreadyExists) {
			slog.Info("subscription_create invoice already exists (idempotent skip)",
				"subscription_id", sub.ID,
				"period_start", inv.BillingPeriodStart,
				"period_end", inv.BillingPeriodEnd,
			)
			return domain.Invoice{}, nil
		}
		return domain.Invoice{}, fmt.Errorf("create invoice: %w", err)
	}

	// Post-persist external/best-effort steps (webhook, tax commit, auto-charge).
	// The finalize AUDIT row already committed inside the create tx above.
	e.FinalizeOnCreateInvoice(ctx, sub, created)

	slog.Info("subscription_create invoice generated",
		"invoice_id", created.ID,
		"subscription_id", sub.ID,
		"total_cents", created.TotalAmountCents,
		"line_items", len(lineItems),
	)
	return created, nil
}

// BillOnCreateTx is the in-transaction variant of BillOnCreate: it builds and
// inserts the day-1 in_advance invoice on the caller's tx but does NOT finalize
// (audit / tax-commit / auto-charge are external and must run after the tx
// commits). The caller commits, then calls FinalizeOnCreateInvoice post-commit.
// Used by the atomic cross-interval plan-swap so the new-period invoice commits
// atomically with the plan write + watermark advance — a failed bill rolls the
// whole swap back rather than silently dropping the new in_advance period.
// Returns ok=false (nil error) for the no-op cases (no in_advance items / zero
// subtotal) and for an idempotent-skip on the period-uniqueness constraint.
func (e *Engine) BillOnCreateTx(ctx context.Context, tx *sql.Tx, sub domain.Subscription) (domain.Invoice, bool, error) {
	ctx, span := telemetry.Tracer("billing").Start(ctx, "billing.BillOnCreateTx",
		trace.WithAttributes(
			attribute.String("subscription_id", sub.ID),
			attribute.String("tenant_id", sub.TenantID),
		),
	)
	defer span.End()

	inv, lineItems, ok, err := e.buildOnCreateInvoice(ctx, sub)
	if err != nil {
		return domain.Invoice{}, false, err
	}
	if !ok {
		return domain.Invoice{}, false, nil
	}

	// NextInvoiceNumber is monotonic; minting outside the tx means a rolled-back
	// swap leaves a harmless invoice-number gap (acceptable, like the cycle
	// path's idempotent-skip note) — never a wrong or duplicate number.
	invoiceNumber, err := e.settings.NextInvoiceNumber(ctx, sub.TenantID)
	if err != nil {
		return domain.Invoice{}, false, fmt.Errorf("mint invoice number: %w", err)
	}
	inv.InvoiceNumber = invoiceNumber

	created, err := e.invoices.CreateInvoiceWithLineItemsTx(ctx, tx, sub.TenantID, inv, lineItems)
	if err != nil {
		// Unlike the non-tx BillOnCreate, there is NO clean "idempotent skip"
		// here: a unique-violation (duplicate period) fires on a statement inside
		// the CALLER's tx, which poisons it — every later statement and the final
		// commit then fail. So surface the error (ErrAlreadyExists included) and
		// let the swap roll back with a clear message rather than an opaque
		// commit failure. The swap re-anchors to a fresh per-request `now`, so a
		// natural period collision is effectively unreachable; this is the
		// duplicate-retry case, which SHOULD fail loud.
		return domain.Invoice{}, false, fmt.Errorf("create new-period invoice (tx): %w", err)
	}
	// The finalize audit row rides the coordinator tx (ADR-090 shared fate): it
	// commits atomically with the plan write + watermark advance, or the whole
	// swap rolls back. The post-commit webhook/tax/auto-charge fire later via
	// FinalizeOnCreateInvoice, which the swap caller invokes after commit.
	if err := e.emitFinalizeAuditTx(ctx, tx, created); err != nil {
		return domain.Invoice{}, false, fmt.Errorf("finalize audit (swap tx): %w", err)
	}
	return created, true, nil
}

// buildOnCreateInvoice computes the day-1 in_advance invoice for sub: resolves
// plans, filters to in_advance items, builds prorated base-fee lines, and
// applies tax. It performs NO DB writes and mints NO invoice number — the
// caller persists (BillOnCreate via the store's own tx, BillOnCreateTx on a
// coordinator tx) and finalizes. Returns ok=false (nil error) for the no-op
// cases: inactive sub, no in_advance items, or zero subtotal. The returned
// invoice has no InvoiceNumber and carries CreatedAt == effective-now.
func (e *Engine) buildOnCreateInvoice(ctx context.Context, sub domain.Subscription) (domain.Invoice, []domain.InvoiceLineItem, bool, error) {
	if sub.Status != domain.SubscriptionActive {
		return domain.Invoice{}, nil, false, nil
	}
	if sub.CurrentBillingPeriodStart == nil || sub.CurrentBillingPeriodEnd == nil {
		return domain.Invoice{}, nil, false, fmt.Errorf("subscription has no billing period set")
	}

	now := e.effectiveNow(ctx, sub)
	periodStart := *sub.CurrentBillingPeriodStart
	periodEnd := *sub.CurrentBillingPeriodEnd

	// Resolve plans for every item — needed to filter to in_advance
	// items and to read base fee + currency.
	plans := make(map[string]domain.Plan, len(sub.Items))
	for _, it := range sub.Items {
		if _, ok := plans[it.PlanID]; ok {
			continue
		}
		pl, err := e.pricing.GetPlan(ctx, sub.TenantID, it.PlanID)
		if err != nil {
			return domain.Invoice{}, nil, false, fmt.Errorf("get plan %s: %w", it.PlanID, err)
		}
		plans[it.PlanID] = pl
	}

	// Filter to in_advance items only. If none, no day-1 invoice;
	// the cycle path will handle this sub naturally at period close.
	advanceItems := make([]domain.SubscriptionItem, 0, len(sub.Items))
	for _, it := range sub.Items {
		if plans[it.PlanID].BaseBillTiming == domain.BillInAdvance {
			advanceItems = append(advanceItems, it)
		}
	}
	if len(advanceItems) == 0 {
		return domain.Invoice{}, nil, false, nil
	}

	// Invoice currency — actual precedence (see billOnePeriod's note;
	// ADR-100): profile > first in_advance item's plan > settings (dead
	// branch) > "USD". Same order
	// as the cycle path.
	invoiceCurrency := plans[advanceItems[0].PlanID].Currency
	if e.profiles != nil {
		if bp, err := e.profiles.GetBillingProfile(ctx, sub.TenantID, sub.CustomerID); err == nil && bp.Currency != "" {
			invoiceCurrency = bp.Currency
		}
	}
	if invoiceCurrency == "" && e.settings != nil {
		if ts, err := e.settings.Get(ctx, sub.TenantID); err == nil && ts.DefaultCurrency != "" {
			invoiceCurrency = ts.DefaultCurrency
		}
	}
	if invoiceCurrency == "" {
		invoiceCurrency = "USD"
	}

	// Build base-fee line items for in_advance items, with mid-period
	// proration (identical math to billOnePeriod's base loop).
	lineItems := make([]domain.InvoiceLineItem, 0, len(advanceItems))
	subtotal := int64(0)
	periodDays := roundDays(periodEnd.Sub(periodStart))
	for _, it := range advanceItems {
		plan := plans[it.PlanID]
		if plan.BaseAmountCents <= 0 {
			continue
		}
		baseFee := plan.BaseAmountCents * it.Quantity
		description := fmt.Sprintf("%s - base fee (qty %d)", plan.Name, it.Quantity)

		fullCycleDays := roundDays(advanceBillingPeriod(periodStart, plan.BillingInterval, e.tenantLocation(ctx, sub.TenantID), sub.BillingAnchorDay).Sub(periodStart))
		if periodDays > 0 && fullCycleDays > 0 && periodDays < fullCycleDays {
			baseFee = money.RoundHalfToEven(plan.BaseAmountCents*it.Quantity*int64(periodDays), int64(fullCycleDays))
			description = fmt.Sprintf("%s - base fee (qty %d, prorated %d/%d days)", plan.Name, it.Quantity, periodDays, fullCycleDays)
		}

		unitAmount := plan.BaseAmountCents
		if it.Quantity > 0 {
			unitAmount = money.RoundHalfToEven(baseFee, it.Quantity)
		}

		baseStartCopy := periodStart
		baseEndCopy := periodEnd
		lineItems = append(lineItems, domain.InvoiceLineItem{
			LineType:           domain.LineTypeBaseFee,
			Description:        description,
			Quantity:           it.Quantity,
			UnitAmountCents:    unitAmount,
			AmountCents:        baseFee,
			TotalAmountCents:   baseFee,
			Currency:           invoiceCurrency,
			TaxCode:            plan.TaxCode,
			BillingPeriodStart: &baseStartCopy,
			BillingPeriodEnd:   &baseEndCopy,
		})
		subtotal += baseFee
	}

	if subtotal <= 0 {
		// All in_advance items had zero base fees. Nothing to bill;
		// don't emit a $0 invoice (matches cycle-path behavior for
		// zero-amount cycles).
		return domain.Invoice{}, nil, false, nil
	}

	// Apply tax.
	taxApp, err := e.ApplyTaxToLineItems(ctx, sub.TenantID, sub.CustomerID, invoiceCurrency, subtotal, 0, lineItems)
	if err != nil {
		return domain.Invoice{}, nil, false, fmt.Errorf("apply tax: %w", err)
	}

	// Fallback 30 — the schema default — matching billOnePeriod, the
	// threshold writer, and the subscription handler's proration path.
	// Pre-fix this was 0: when settings were unreadable this invoice
	// stamped DueAt == IssuedAt (immediately overdue, dunning fires on
	// day 0) while every sibling cycle invoice stayed Net-30.
	netDays := 30
	if e.settings != nil {
		if ts, err := e.settings.Get(ctx, sub.TenantID); err == nil && ts.NetPaymentTerms > 0 {
			netDays = ts.NetPaymentTerms
		}
	}
	dueAt := now.AddDate(0, 0, netDays)

	totalWithTax := taxApp.SubtotalCents - taxApp.DiscountCents + taxApp.TaxAmountCents

	inv := domain.Invoice{
		TenantID:        sub.TenantID,
		CustomerID:      sub.CustomerID,
		SubscriptionID:  sub.ID,
		BillingTimezone: e.tenantLocation(ctx, sub.TenantID).String(),
		// Tax-deferred + pause-collection gate (matches billOnePeriod).
		// Pre-fix this path hardcoded Finalized regardless of tax;
		// invoices with tax_status=pending finalized with
		// TaxAmountCents=0, lying about authoritative amounts.
		Status:        domain.InvoiceFinalizationStatus(taxApp.TaxStatus, sub.PauseCollection),
		PaymentStatus: domain.PaymentPending,
		Currency:      invoiceCurrency,
		SubtotalCents: taxApp.SubtotalCents,
		DiscountCents: taxApp.DiscountCents,
		// ONE assignment carries every tax fact (see domain/tax_facts.go).
		TaxFacts:           taxApp.TaxFacts,
		TotalAmountCents:   totalWithTax,
		AmountDueCents:     totalWithTax,
		BillingPeriodStart: periodStart,
		BillingPeriodEnd:   periodEnd,
		IssuedAt:           &now,
		DueAt:              &dueAt,
		CreatedAt:          now,
		NetPaymentTermDays: netDays,
		BillingReason:      domain.BillingReasonSubscriptionCreate,
		IsSimulated:        sub.TestClockID != "",
	}
	return inv, lineItems, true, nil
}

// FinalizeOnCreateInvoice runs the post-persist steps for a day-1 in_advance
// invoice: the engine audit row, the Stripe Tax commit, and auto-charge (or
// enroll-for-retry + no-PM notify). These are external / best-effort and MUST
// run AFTER the invoice is durably committed — never inside a DB tx. BillOnCreate
// calls it inline; the atomic cross-interval swap calls it post-commit.
func (e *Engine) FinalizeOnCreateInvoice(ctx context.Context, sub domain.Subscription, inv domain.Invoice) {
	// The finalize AUDIT row rode the invoice-create tx (ADR-090 shared fate):
	// the own-tx callers (BillOnCreate) emit it via finalizeAuditEmit, and the
	// coordinator-tx caller (the cross-interval swap through BillOnCreateTx) emits
	// it on the coordinator tx. So this post-commit path fires only the external,
	// best-effort effects — WEBHOOK, tax commit, auto-charge — none of which may
	// ride a DB tx.
	e.dispatchInvoiceFinalized(ctx, sub, inv)

	// Commit tax if a provider produced a calculation (same pattern
	// as the cycle path; ManualProvider / NoneProvider no-op).
	if inv.TaxProvider != "" && inv.TaxCalculationID != "" {
		if err := e.CommitTax(ctx, sub.TenantID, inv.ID, inv.TaxCalculationID); err != nil {
			slog.Warn("tax: commit failed after subscription_create invoice",
				"error", err,
				"tenant_id", sub.TenantID,
				"invoice_id", inv.ID,
			)
		}
	}

	// Apply the customer's credit balance, then auto-charge the remainder
	// (or queue + notify) via the shared pipeline (ADR-088 — industry parity
	// is unanimous that day-1 invoices consume the balance; pre-ADR-088 a
	// credit-holding customer's card was charged full price here). No pause
	// gate: a day-1 invoice is created at subscribe time, before any pause
	// can exist.
	e.applyCreditsAndCollect(ctx, sub, inv, "subscription_create")
}

// BillFinalOnImmediateCancel emits the final partial-period invoice
// for a sub that was canceled mid-period via the operator's immediate
// Cancel action. Pre-PR-10, mid-period immediate cancels generated NO
// final invoice — partial-period usage was never billed (revenue leak:
// customer could rack up usage and cancel for free). For in_arrears
// plans, the partial-period base was also never billed.
//
// Scope of the invoice:
//   - Period: [current_period_start, canceled_at] — the elapsed
//     portion of the just-canceled cycle.
//   - Lines:
//   - in_arrears base items: prorated by `elapsed / full_cycle`.
//   - in_advance base items: skipped (already paid up-front; the
//     refund of the unused portion is BillOnCancel's job — credit
//     grant to balance).
//   - Usage: aggregated for [periodStart, canceled_at] across every
//     meter referenced by every item. Same shape as a normal cycle
//     invoice's usage section, just with a truncated period_end.
//   - billing_reason = subscription_cancel (distinguishes from the
//     normal subscription_cycle invoice and from subscription_create).
//
// No-op when:
//   - sub.Status != canceled (defensive — caller should have flipped it)
//   - no current period set / canceled_at missing
//   - canceled_at AT or AFTER current_period_end (clean cancel at
//     boundary; the cycle close already fired or will fire normally)
//   - canceled_at AT or BEFORE current_period_start (defensive)
//   - computed subtotal rounds to zero (no in_arrears base AND no
//     usage events; matches BillOnCreate's $0-invoice-skip pattern)
//
// Idempotent via the standard (subscription_id, period_start,
// period_end) UNIQUE constraint — the period_end is canceled_at which
// is durably stamped, so a retry against the same canceled sub
// returns the existing-invoice idempotent skip.
//
// Auto-charge: attempted synchronously when a PM is ready, mirrors
// BillOnCreate's post-finalize path. Without a PM, the invoice is
// marked auto_charge_pending and the no-PM notifier fires. Dunning
// takes over from there on a failed charge.
func (e *Engine) BillFinalOnImmediateCancel(ctx context.Context, sub domain.Subscription) (domain.Invoice, error) {
	return e.billFinalOnImmediateCancelImpl(ctx, nil, sub)
}

// BillFinalOnImmediateCancelTx is the in-tx variant: it inserts the
// final-on-cancel invoice on the caller's coordinator tx (ADR-056) and leaves
// the external finalize (audit + tax commit + auto-charge) to the caller's
// post-commit FinalizeOnCreateInvoice. Used by CancelAtomicWithBill so a billing
// failure rolls the cancel back rather than orphaning a canceled sub with an
// uninvoiced partial period. Returns an empty invoice (nil error) for the no-op
// cases (nothing to bill on cancel).
func (e *Engine) BillFinalOnImmediateCancelTx(ctx context.Context, tx *sql.Tx, sub domain.Subscription) (domain.Invoice, error) {
	return e.billFinalOnImmediateCancelImpl(ctx, tx, sub)
}

// resolveRatedRule is the single pricing-resolution path every rater
// shares (ADR-070): from a pinned rule-version binding (meter default or
// pricing-rule row), resolve the version of that rule_key in force at
// the period open, then patch the customer's override in force at the
// same instant on top. One period, one price — cycle close, cancel
// finalize, threshold fire, and preview all call this (preview via its
// mirror in previewMeter), so they agree by construction and a
// mid-close publish or override edit can't mix prices inside one
// invoice.
//
// Error contract (no-silent-fallbacks): a missing override
// (errs.ErrNotFound) means list price; ANY other failure — including
// version resolution — aborts the rating path and retries next tick.
// The pre-ADR-070 code treated every override error as absence and fell
// back to the pinned version on resolution errors: a transient DB blip
// silently billed a negotiated customer at list price.
func (e *Engine) resolveRatedRule(ctx context.Context, tenantID, customerID, pinnedRuleVersionID string, periodStart time.Time) (domain.RatingRuleVersion, error) {
	linked, err := e.pricing.GetRatingRule(ctx, tenantID, pinnedRuleVersionID)
	if err != nil {
		return domain.RatingRuleVersion{}, fmt.Errorf("get rating rule %s: %w", pinnedRuleVersionID, err)
	}
	rule, err := e.pricing.GetRuleByKeyAsOf(ctx, tenantID, linked.RuleKey, periodStart)
	if err != nil {
		if !errors.Is(err, errs.ErrNotFound) {
			return domain.RatingRuleVersion{}, fmt.Errorf("resolve rule %q as of %s: %w", linked.RuleKey, periodStart.Format(time.RFC3339), err)
		}
		// Key has no ACTIVE versions (all archived) but a binding still
		// points at one — rate at the pinned version rather than
		// refusing to bill usage the meter admitted.
		rule = linked
	}
	override, err := e.pricing.GetOverrideByKeyAsOf(ctx, tenantID, customerID, rule.RuleKey, periodStart)
	if err == nil {
		return override.ApplyTo(rule), nil
	}
	if errors.Is(err, errs.ErrNotFound) {
		return rule, nil
	}
	return domain.RatingRuleVersion{}, fmt.Errorf("resolve override for rule %q: %w", rule.RuleKey, err)
}

// buildCancelLineItems computes the final invoice's line items and
// subtotal for an immediate mid-period cancel: segment-aware in_arrears
// base fees over the partial period [periodStart, canceledAt] (skipped
// entirely when a threshold fire already billed the unprorated base)
// plus segment-aware usage, honoring the usage cap and the threshold
// watermark's clamped/full dual-window protocol. Extracted whole from
// billFinalOnImmediateCancelImpl (behavior-preserving) to separate the
// pricing computation from the cancel-invoice orchestration; it is the
// cancel-path analog of buildLineItems and shares its segment model.
func (e *Engine) buildCancelLineItems(ctx context.Context, sub domain.Subscription, wm thresholdWatermark, plans map[string]domain.Plan, invoiceCurrency string, periodStart, canceledAt time.Time) ([]domain.InvoiceLineItem, int64, error) {
	// Build base lines: segment-aware in_arrears billing over the
	// partial period [periodStart, canceledAt]. in_advance items are
	// explicitly skipped — their base for the just-canceled period
	// was already paid at period start (BillOnCreate or prior cycle),
	// and the unused portion will be refunded by BillOnCancel as a
	// credit grant.
	//
	// Segment-aware: if the customer changed plan or quantity (or
	// added/removed an item) between periodStart and canceledAt, each
	// segment is billed at its own rate × (segment_days / cycle_days).
	// No mid-period changes → single segment from periodStart to
	// canceledAt, matching the pre-segment-aware single-line behavior.
	//
	// Fail-loud parity with buildLineItems (2026-07-10 design review):
	// pre-fix the error was swallowed (`_ =`), so a transient DB failure
	// here silently billed a changed sub's FINAL invoice as a single
	// full-window line at the current plan's rate — mis-billing with no
	// operator signal. Per feedback_no_silent_fallbacks the cancel bill
	// fails instead; the operator's cancel retries.
	// ADR-101 segment source for the cancel window — same seam as the
	// cycle path, window-ended at the cancel instant. The interval read
	// sees pre-cancel state (the close-all seal rides the cancel tx,
	// invisible to this read), which clips identically to
	// [periodStart, canceledAt] — open intervals end at the window edge
	// either way.
	segsByItem, err := e.windowSegments(ctx, sub, periodStart, canceledAt)
	if err != nil {
		return nil, 0, fmt.Errorf("resolve segments on cancel: %w", err)
	}
	for _, segs := range segsByItem {
		for _, seg := range segs {
			if seg.planID == "" {
				continue
			}
			if _, ok := plans[seg.planID]; ok {
				continue
			}
			pl, err := e.pricing.GetPlan(ctx, sub.TenantID, seg.planID)
			if err != nil {
				return nil, 0, fmt.Errorf("get segment plan %s on cancel: %w", seg.planID, err)
			}
			plans[seg.planID] = pl
		}
	}
	itemsByID := map[string]domain.SubscriptionItem{}
	for _, it := range sub.Items {
		itemsByID[it.ID] = it
	}

	lineItems := make([]domain.InvoiceLineItem, 0, len(sub.Items))
	subtotal := int64(0)
	periodDays := roundDays(canceledAt.Sub(periodStart))

	// In_arrears base is skipped ENTIRELY when a threshold fire exists in
	// this window (ADR-066 §5): a reset=false fire bills the FULL unprorated
	// base, so re-emitting any prorated segment here double-bills — same
	// watermark base skip billOnePeriod applies. (reset=true fires are
	// invisible to this window's watermark by construction: the atomic
	// re-anchor moves period_start past the fire's own billing_period_start.)
	if !wm.exists() {
		// Pass 1: items currently on the sub at cancel time.
		for _, it := range sub.Items {
			plan := plans[it.PlanID]
			if plan.BaseBillTiming == domain.BillInAdvance {
				continue
			}
			segments := segsByItem[it.ID]
			for _, seg := range segments {
				segPlan, ok := plans[seg.planID]
				if !ok || segPlan.BaseAmountCents <= 0 || segPlan.BaseBillTiming == domain.BillInAdvance {
					continue
				}
				emitBaseSegmentLine(seg, segPlan, periodStart, periodDays, invoiceCurrency, e.tenantLocation(ctx, sub.TenantID), sub.BillingTime, &lineItems, &subtotal)
			}
		}

		// Pass 2: items removed between periodStart and canceledAt (in the
		// segment map but not on sub.Items now). Bill their pre-remove
		// segments at the respective plans' rates.
		for itemID, segments := range segsByItem {
			if _, stillPresent := itemsByID[itemID]; stillPresent {
				continue
			}
			for _, seg := range segments {
				segPlan, ok := plans[seg.planID]
				if !ok || segPlan.BaseAmountCents <= 0 || segPlan.BaseBillTiming == domain.BillInAdvance {
					continue
				}
				emitBaseSegmentLine(seg, segPlan, periodStart, periodDays, invoiceCurrency, e.tenantLocation(ctx, sub.TenantID), sub.BillingTime, &lineItems, &subtotal)
			}
		}
	}

	// Build usage lines: aggregate every meter referenced by every
	// plan that was active at any point in [periodStart, canceledAt].
	// Same aggregation surface the cycle path uses; only the period
	// boundaries differ. Iterates the full `plans` map (current items'
	// plans + hydrated change-log plans) so segment-aware billing
	// picks up meters that existed on now-removed plans.
	meterAggs := make(map[string]string)
	for _, pl := range plans {
		for _, meterID := range pl.MeterIDs {
			if _, ok := meterAggs[meterID]; ok {
				continue
			}
			m, err := e.pricing.GetMeter(ctx, sub.TenantID, meterID)
			if err == nil {
				meterAggs[meterID] = m.Aggregation
			} else {
				meterAggs[meterID] = "sum"
			}
		}
	}
	usageTotals := make(map[string]decimal.Decimal)
	if len(meterAggs) > 0 {
		totals, err := e.usage.AggregateForBillingPeriodByAgg(ctx, sub.TenantID, sub.CustomerID, meterAggs, periodStart, canceledAt)
		if err != nil {
			return nil, 0, fmt.Errorf("aggregate usage on cancel: %w", err)
		}
		usageTotals = totals
	}

	// Enforce the subscription-level usage cap, same semantics as
	// billOnePeriod: the partial window [periodStart, canceledAt] is the
	// entire billable extent of the final cycle, so the per-cycle cap
	// applies to it whole. Totals are scaled in place (full-period reads)
	// and capScale carries the same factor to the sub-period and
	// multi-dim branches below, whose aggregations return raw quantities.
	// Pre-fix the cancel path billed raw above-cap usage that cycle close
	// would have capped — overbilling "block"-capped subs on cancel.
	capScale := decimal.New(1, 0)
	capOne := decimal.New(1, 0)
	if sub.UsageCapUnits != nil && *sub.UsageCapUnits > 0 && sub.OverageAction == "block" {
		totalUsage := decimal.Zero
		for _, qty := range usageTotals {
			totalUsage = totalUsage.Add(qty)
		}
		capDec := decimal.NewFromInt(*sub.UsageCapUnits)
		if totalUsage.GreaterThan(capDec) {
			capScale = capDec.Div(totalUsage)
			for mid, qty := range usageTotals {
				usageTotals[mid] = qty.Mul(capScale)
			}
		}
	}

	// Segment-aware usage billing over the partial period
	// [periodStart, canceledAt]. Mirrors the billOnePeriod shape:
	// each meter bills only for the time it was active on the sub —
	// including the dual clamped/full interval maps for the threshold
	// watermark protocol (ADR-066 §4; see billOnePeriod for the full
	// rationale).
	meterIntervals := map[string][]usageInterval{}
	meterIntervalsFull := map[string][]usageInterval{}
	addMeterInterval := func(mid string, start, end time.Time) {
		if wm.exists() && end.After(start) {
			meterIntervalsFull[mid] = append(meterIntervalsFull[mid], usageInterval{start, end})
		}
		if wm.billedThrough != nil && start.Before(*wm.billedThrough) {
			start = *wm.billedThrough
		}
		if !end.After(start) {
			return
		}
		meterIntervals[mid] = append(meterIntervals[mid], usageInterval{start, end})
	}
	for _, it := range sub.Items {
		segments := segsByItem[it.ID]
		if len(segments) == 0 {
			for _, mid := range plans[it.PlanID].MeterIDs {
				addMeterInterval(mid, periodStart, canceledAt)
			}
			continue
		}
		for _, seg := range segments {
			segPlan, ok := plans[seg.planID]
			if !ok {
				continue
			}
			for _, mid := range segPlan.MeterIDs {
				addMeterInterval(mid, seg.start, seg.end)
			}
		}
	}
	for itemID, segments := range segsByItem {
		if _, stillPresent := itemsByID[itemID]; stillPresent {
			continue
		}
		for _, seg := range segments {
			segPlan, ok := plans[seg.planID]
			if !ok {
				continue
			}
			for _, mid := range segPlan.MeterIDs {
				addMeterInterval(mid, seg.start, seg.end)
			}
		}
	}

	intervalAggCache := map[string]map[string]decimal.Decimal{}
	intervalKey := func(iv usageInterval) string {
		return iv.start.UTC().Format(time.RFC3339Nano) + "|" + iv.end.UTC().Format(time.RFC3339Nano)
	}

	// Union of meters with billable windows (a meter fully clamped away can
	// still owe a full-window bill for a deferred non-additive bucket) +
	// the clamped/full pass work-list — same shape as billOnePeriod.
	cancelMeterIDSet := make(map[string]struct{}, len(meterIntervals))
	for mid := range meterIntervals {
		cancelMeterIDSet[mid] = struct{}{}
	}
	for mid := range meterIntervalsFull {
		cancelMeterIDSet[mid] = struct{}{}
	}
	type meterWindow struct {
		iv              usageInterval
		nonAdditiveOnly bool
	}
	for meterID := range cancelMeterIDSet {
		// Does this meter price via dimension-match pricing rules (the
		// multi-dim shape: e.g. the AI `tokens` meter, one rule per
		// {model, token_type})? Same fork billOnePeriod / preview / the
		// threshold scan take — a single meter-linked rating rule can't
		// express N rates, so for these meters RatingRuleVersionID is
		// empty and the single-rule branch below would silently skip
		// them. Pre-fix this fork was MISSING here: a mid-period
		// immediate cancel emitted $0 for all token usage (the ADR-044
		// wedge breakage, on the cancel path).
		pricingRules, err := e.pricing.ListMeterPricingRulesByMeter(ctx, sub.TenantID, meterID)
		if err != nil {
			return nil, 0, fmt.Errorf("list pricing rules for meter %s on cancel: %w", meterID, err)
		}

		windows := make([]meterWindow, 0, 4)
		for _, iv := range mergeUsageIntervals(meterIntervals[meterID]) {
			windows = append(windows, meterWindow{iv: iv})
		}
		if wm.exists() {
			for _, iv := range mergeUsageIntervals(meterIntervalsFull[meterID]) {
				windows = append(windows, meterWindow{iv: iv, nonAdditiveOnly: true})
			}
		}

		// ---- Multi-dim path: one billable bucket per claimed rule. ----
		if len(pricingRules) > 0 {
			meter, err := e.pricing.GetMeter(ctx, sub.TenantID, meterID)
			if err != nil {
				return nil, 0, fmt.Errorf("get meter %s on cancel: %w", meterID, err)
			}
			rulesByID := make(map[string]domain.MeterPricingRule, len(pricingRules))
			for _, r := range pricingRules {
				rulesByID[r.ID] = r
			}
			defaultMode := mapMeterAggregation(meter.Aggregation)
			for _, mw := range windows {
				iv := mw.iv
				ivStart := iv.start
				ivEnd := iv.end
				aggs, err := e.usage.AggregateByPricingRules(ctx, sub.TenantID, sub.CustomerID, meterID, defaultMode, iv.start, iv.end)
				if err != nil {
					return nil, 0, fmt.Errorf("aggregate by pricing rules for meter %s on cancel [%v, %v): %w", meterID, iv.start, iv.end, err)
				}
				for _, agg := range aggs {
					qty := agg.Quantity
					// AggregateByPricingRules returns raw (pre-cap)
					// quantities — apply the cap scale here, same as
					// billOnePeriod's multi-dim branch.
					if !capScale.Equal(capOne) {
						qty = qty.Mul(capScale)
					}
					if qty.IsZero() {
						continue
					}
					ratingRuleID := agg.RatingRuleVersionID
					if ratingRuleID == "" {
						ratingRuleID = meter.RatingRuleVersionID
					}
					if ratingRuleID == "" {
						// Usage that matched no rule and the meter has no
						// default binding — bill nothing, but loudly (a
						// silent skip would hide unbilled revenue).
						slog.Warn("cancel: usage matched no rating rule and meter has no default binding — not billed",
							"meter", meter.Key, "quantity", qty.String(),
							"subscription_id", sub.ID)
						continue
					}
					// ADR-066 §4 pass routing — see billOnePeriod.
					if wm.deferredBucket(agg.AggregationMode, meterID, ratingRuleID) != mw.nonAdditiveOnly {
						continue
					}
					rule, err := e.resolveRatedRule(ctx, sub.TenantID, sub.CustomerID, ratingRuleID, periodStart)
					if err != nil {
						return nil, 0, fmt.Errorf("resolve pricing for meter %s rule %s on cancel: %w", meterID, ratingRuleID, err)
					}
					amount, err := domain.ComputeAmountCents(rule, qty)
					if err != nil {
						return nil, 0, fmt.Errorf("compute amount for meter %s rule %s on cancel: %w", meterID, ratingRuleID, err)
					}
					unitAmount := decimal.NewFromInt(amount).Div(qty).RoundBank(0).IntPart()
					lineItems = append(lineItems, domain.InvoiceLineItem{
						LineType:                 domain.LineTypeUsage,
						MeterID:                  meterID,
						Description:              usageLineDescription(meter, rulesByID[agg.RuleID]) + " - canceled mid-period",
						Quantity:                 qty.IntPart(),
						QuantityDecimal:          qty,
						UnitAmountCents:          unitAmount,
						AmountCents:              amount,
						TotalAmountCents:         amount,
						Currency:                 invoiceCurrency,
						PricingMode:              string(rule.Mode),
						RatingRuleVersionID:      rule.ID,
						NominalUnitAmountDecimal: nominalRate(rule),
						BillingPeriodStart:       &ivStart,
						BillingPeriodEnd:         &ivEnd,
					})
					subtotal += amount
				}
			}
			continue
		}

		// ---- Single-rule path (pre-multi-dim meters). ----
		for _, mw := range windows {
			iv := mw.iv
			meter, err := e.pricing.GetMeter(ctx, sub.TenantID, meterID)
			if err != nil {
				return nil, 0, fmt.Errorf("get meter %s on cancel: %w", meterID, err)
			}
			// ADR-066 §4 pass routing at meter granularity — see billOnePeriod.
			if wm.deferredBucket(mapMeterAggregation(meter.Aggregation), meterID, meter.RatingRuleVersionID) != mw.nonAdditiveOnly {
				continue
			}
			var quantity decimal.Decimal
			fullPeriod := iv.start.Equal(periodStart) && iv.end.Equal(canceledAt)
			if fullPeriod {
				quantity = usageTotals[meterID]
			} else {
				key := intervalKey(iv)
				totals, cached := intervalAggCache[key]
				if !cached {
					t, err := e.usage.AggregateForBillingPeriodByAgg(ctx, sub.TenantID, sub.CustomerID, meterAggs, iv.start, iv.end)
					if err != nil {
						return nil, 0, fmt.Errorf("aggregate usage on cancel for segment [%v, %v): %w", iv.start, iv.end, err)
					}
					totals = t
					intervalAggCache[key] = t
				}
				quantity = totals[meterID]
				// Sub-period aggregates are raw — apply the cap scale so
				// the per-interval sum matches the capped window total
				// (usageTotals above is already scaled in place).
				if !capScale.Equal(capOne) {
					quantity = quantity.Mul(capScale)
				}
			}
			if quantity.IsZero() {
				continue
			}
			if meter.RatingRuleVersionID == "" {
				// Unpriced meter → its usage can't be rated. Never silent
				// (ADR-096); see the cycle-path sibling. Loud log, no revenue
				// billed.
				slog.Warn("cancel: usage on a meter with no rating rule — not billed (unpriced meter on plan; ADR-096)",
					"meter", meter.Key, "quantity", quantity.String(), "subscription_id", sub.ID)
				continue
			}
			rule, err := e.resolveRatedRule(ctx, sub.TenantID, sub.CustomerID, meter.RatingRuleVersionID, periodStart)
			if err != nil {
				return nil, 0, fmt.Errorf("resolve pricing for meter %s on cancel: %w", meterID, err)
			}
			amount, err := domain.ComputeAmountCents(rule, quantity)
			if err != nil {
				return nil, 0, fmt.Errorf("compute amount for meter %s on cancel: %w", meterID, err)
			}
			unitAmount := decimal.NewFromInt(amount).Div(quantity).RoundBank(0).IntPart()
			ivStart := iv.start
			ivEnd := iv.end
			lineItems = append(lineItems, domain.InvoiceLineItem{
				LineType:                 domain.LineTypeUsage,
				MeterID:                  meterID,
				Description:              fmt.Sprintf("%s (%s) - canceled mid-period", meter.Name, meter.Unit),
				Quantity:                 quantity.IntPart(),
				QuantityDecimal:          quantity,
				UnitAmountCents:          unitAmount,
				AmountCents:              amount,
				TotalAmountCents:         amount,
				Currency:                 invoiceCurrency,
				PricingMode:              string(rule.Mode),
				RatingRuleVersionID:      rule.ID,
				NominalUnitAmountDecimal: nominalRate(rule),
				BillingPeriodStart:       &ivStart,
				BillingPeriodEnd:         &ivEnd,
			})
			subtotal += amount
		}
	}
	return lineItems, subtotal, nil
}

func (e *Engine) billFinalOnImmediateCancelImpl(ctx context.Context, tx *sql.Tx, sub domain.Subscription) (domain.Invoice, error) {
	ctx, span := telemetry.Tracer("billing").Start(ctx, "billing.BillFinalOnImmediateCancel",
		trace.WithAttributes(
			attribute.String("subscription_id", sub.ID),
			attribute.String("tenant_id", sub.TenantID),
		),
	)
	defer span.End()

	if sub.Status != domain.SubscriptionCanceled {
		return domain.Invoice{}, nil
	}
	if sub.CurrentBillingPeriodStart == nil || sub.CurrentBillingPeriodEnd == nil {
		return domain.Invoice{}, nil
	}
	if sub.CanceledAt == nil {
		return domain.Invoice{}, nil
	}
	periodStart := *sub.CurrentBillingPeriodStart
	periodEnd := *sub.CurrentBillingPeriodEnd
	canceledAt := *sub.CanceledAt

	// Only mid-period cancels need a final invoice. canceled_at at or
	// after period_end means the cycle close handles the period
	// normally; canceled_at at or before period_start is defensive.
	if !canceledAt.After(periodStart) || !canceledAt.Before(periodEnd) {
		return domain.Invoice{}, nil
	}

	// Never-activated trial write-off (ADR-069): a sub canceled straight out
	// of 'trialing' (activated_at never stamped) emits NO final invoice —
	// trials are free. The reachable window is the post-trial LAG: the trial
	// elapsed but the activation scan hadn't flipped the row yet, so
	// canceled_at lands inside [trial_end = period_start, period_end) and
	// this function would otherwise bill prorated base + usage for a
	// never-activated sub. Decided: write off — matches the trial-end
	// scheduled-cancel semantics ("you won't be charged") rather than
	// billing on scan lag the customer can't see. Subs created active carry
	// activated_at (or no trial fields) and are unaffected.
	if sub.TrialEndAt != nil && sub.ActivatedAt == nil {
		slog.Info("immediate cancel of a never-activated trial — no final invoice (trials are free)",
			"subscription_id", sub.ID, "tenant_id", sub.TenantID)
		return domain.Invoice{}, nil
	}

	// Threshold watermark (ADR-066 §4/§5): the immediate cancel is the
	// period's SECOND closer, and pre-fix it ignored the fire entirely — a
	// reset=false threshold fire followed by an immediate cancel re-billed
	// the pre-fire sum usage AND the full in_arrears base (both already on
	// the fire invoice, typically already charged). Shared protocol with
	// billOnePeriod: base skipped when a fire exists, additive buckets
	// clamped to the residual window, deferred non-additive buckets billed
	// full-window exactly once.
	wm, err := e.loadThresholdWatermark(ctx, sub.TenantID, sub.ID, periodStart, periodEnd)
	if err != nil {
		return domain.Invoice{}, err
	}

	// Resolve plans for every item — needed for in_arrears proration
	// math, currency resolution, and usage-meter discovery.
	plans := make(map[string]domain.Plan, len(sub.Items))
	for _, it := range sub.Items {
		if _, ok := plans[it.PlanID]; ok {
			continue
		}
		pl, err := e.pricing.GetPlan(ctx, sub.TenantID, it.PlanID)
		if err != nil {
			return domain.Invoice{}, fmt.Errorf("get plan %s: %w", it.PlanID, err)
		}
		plans[it.PlanID] = pl
	}
	if len(sub.Items) == 0 {
		return domain.Invoice{}, nil
	}

	// Invoice currency — actual precedence (see billOnePeriod's note;
	// ADR-100): profile > first item's plan > settings (dead branch) >
	// "USD". Same order as billOnePeriod and BillOnCreate.
	invoiceCurrency := plans[sub.Items[0].PlanID].Currency
	if e.profiles != nil {
		if bp, err := e.profiles.GetBillingProfile(ctx, sub.TenantID, sub.CustomerID); err == nil && bp.Currency != "" {
			invoiceCurrency = bp.Currency
		}
	}
	if invoiceCurrency == "" && e.settings != nil {
		if ts, err := e.settings.Get(ctx, sub.TenantID); err == nil && ts.DefaultCurrency != "" {
			invoiceCurrency = ts.DefaultCurrency
		}
	}
	if invoiceCurrency == "" {
		invoiceCurrency = "USD"
	}

	// Compute the final invoice's line items over the partial cancel
	// period [periodStart, canceledAt]: segment-aware in_arrears base
	// (skipped when a threshold fire already billed it) plus usage.
	lineItems, subtotal, err := e.buildCancelLineItems(ctx, sub, wm, plans, invoiceCurrency, periodStart, canceledAt)
	if err != nil {
		return domain.Invoice{}, err
	}

	if subtotal <= 0 {
		// Nothing to bill — no in_arrears base AND no usage. The
		// customer canceled before they consumed anything billable.
		// Skip the $0 invoice (matches BillOnCreate's behavior).
		return domain.Invoice{}, nil
	}

	// Apply tax.
	taxApp, err := e.ApplyTaxToLineItems(ctx, sub.TenantID, sub.CustomerID, invoiceCurrency, subtotal, 0, lineItems)
	if err != nil {
		return domain.Invoice{}, fmt.Errorf("apply tax on cancel: %w", err)
	}

	// Fallback 30 — the schema default — matching billOnePeriod, the
	// threshold writer, and the subscription handler's proration path.
	// Pre-fix this was 0: when settings were unreadable this invoice
	// stamped DueAt == IssuedAt (immediately overdue, dunning fires on
	// day 0) while every sibling cycle invoice stayed Net-30.
	netDays := 30
	if e.settings != nil {
		if ts, err := e.settings.Get(ctx, sub.TenantID); err == nil && ts.NetPaymentTerms > 0 {
			netDays = ts.NetPaymentTerms
		}
	}
	now := e.effectiveNow(ctx, sub)
	dueAt := now.AddDate(0, 0, netDays)
	totalWithTax := taxApp.SubtotalCents - taxApp.DiscountCents + taxApp.TaxAmountCents

	invoiceNumber, err := e.settings.NextInvoiceNumber(ctx, sub.TenantID)
	if err != nil {
		return domain.Invoice{}, fmt.Errorf("mint invoice number on cancel: %w", err)
	}

	invToCreate := domain.Invoice{
		TenantID:        sub.TenantID,
		CustomerID:      sub.CustomerID,
		SubscriptionID:  sub.ID,
		BillingTimezone: e.tenantLocation(ctx, sub.TenantID).String(),
		InvoiceNumber:   invoiceNumber,
		// Tax-deferred + pause-collection gate (matches billOnePeriod).
		// Pre-fix this path hardcoded Finalized regardless of tax;
		// invoices with tax_status=pending finalized with
		// TaxAmountCents=0, lying about authoritative amounts.
		Status:        domain.InvoiceFinalizationStatus(taxApp.TaxStatus, sub.PauseCollection),
		PaymentStatus: domain.PaymentPending,
		Currency:      invoiceCurrency,
		SubtotalCents: taxApp.SubtotalCents,
		DiscountCents: taxApp.DiscountCents,
		// ONE assignment carries every tax fact (see domain/tax_facts.go).
		TaxFacts:           taxApp.TaxFacts,
		TotalAmountCents:   totalWithTax,
		AmountDueCents:     totalWithTax,
		BillingPeriodStart: periodStart,
		BillingPeriodEnd:   canceledAt,
		IssuedAt:           &now,
		DueAt:              &dueAt,
		CreatedAt:          now,
		NetPaymentTermDays: netDays,
		BillingReason:      domain.BillingReasonSubscriptionCancel,
		IsSimulated:        sub.TestClockID != "",
	}
	if tx != nil {
		// Atomic path (CancelAtomicWithBill): insert on the caller's coordinator
		// tx. No idempotent-skip — a unique violation poisons the caller's tx, so
		// surface it and let the cancel roll back (same contract as the swap
		// path's BillOnCreateTx). The external finalize (webhook + tax commit +
		// auto-charge) runs post-commit via FinalizeOnCreateInvoice; the AUDIT row
		// rides THIS coordinator tx so it commits atomically with the cancel.
		created, err := e.invoices.CreateInvoiceWithLineItemsTx(ctx, tx, sub.TenantID, invToCreate, lineItems)
		if err != nil {
			return domain.Invoice{}, err
		}
		if err := e.emitFinalizeAuditTx(ctx, tx, created); err != nil {
			return domain.Invoice{}, fmt.Errorf("finalize audit (cancel tx): %w", err)
		}
		return created, nil
	}

	inv, err := e.invoices.CreateInvoiceWithLineItemsAudited(ctx, sub.TenantID, invToCreate, lineItems, e.finalizeAuditEmit(ctx))
	if err != nil {
		if errors.Is(err, errs.ErrAlreadyExists) {
			slog.Info("subscription_cancel final invoice already exists (idempotent skip)",
				"subscription_id", sub.ID,
				"period_start", periodStart,
				"canceled_at", canceledAt,
			)
			return domain.Invoice{}, nil
		}
		return domain.Invoice{}, fmt.Errorf("create final-on-cancel invoice: %w", err)
	}

	// The finalize AUDIT row already committed inside the create tx above; here
	// only the post-commit WEBHOOK fires.
	e.dispatchInvoiceFinalized(ctx, sub, inv)

	if inv.TaxProvider != "" && inv.TaxCalculationID != "" {
		if err := e.CommitTax(ctx, sub.TenantID, inv.ID, inv.TaxCalculationID); err != nil {
			slog.Warn("tax: commit failed after final-on-cancel invoice",
				"error", err, "tenant_id", sub.TenantID, "invoice_id", inv.ID)
		}
	}

	// Apply the customer's credit balance, then auto-charge the remainder
	// (or queue + notify) via the shared pipeline (ADR-088); dunning takes
	// over on a real decline (inline in the charger).
	e.applyCreditsAndCollect(ctx, sub, inv, "final on cancel")

	slog.Info("subscription_cancel final invoice generated",
		"invoice_id", inv.ID,
		"subscription_id", sub.ID,
		"period_start", periodStart,
		"canceled_at", canceledAt,
		"total_cents", totalWithTax,
		"line_items", len(lineItems),
	)
	return inv, nil
}

// BillOnCancel emits the cancel proration credit for an in_advance
// subscription that was canceled before its current period closed
// (ADR-031). The base for the current period was already billed —
// either on day 1 via BillOnCreate or at the prior cycle close — so
// the unused portion (canceled_at → period_end) needs to flow back
// to the customer.
//
// Refund mode: credit grant to the customer's balance. Operator can
// separately issue a credit note against the original invoice for a
// PM refund if desired (matches Stripe's out_of_band vs payment
// refund choice). Granting to balance is the safer default — no
// Stripe round-trip, applies automatically to future invoices.
//
// No-op when:
//   - sub.Status != canceled (defensive — caller should have flipped it)
//   - no current period set
//   - cancel_at at/after period_end (clean cancel, no proration owed)
//   - no item's plan is in_advance (every base was arrears, nothing prebilled)
//   - computed unused amount rounds to zero
//
// Idempotency: not provided. Cancel is called once; if BillOnCancel
// fails after the cancel succeeds, the operator can manually issue
// a credit grant from the dashboard. Re-calling Cancel against an
// already-canceled sub returns the existing canceled state before
// reaching the biller (so BillOnCancel doesn't run twice in normal
// retry).
// Returns (credit_cents, error). credit_cents is the cents-amount of
// the cancel-proration credit granted (0 when no credit applied —
// in_arrears sub, clean cancel, unpaid source invoice, etc.). The
// caller surfaces this to the activity timeline so operators see the
// "Cancel proration credit: $X.XX" line linked to the cancel event.
func (e *Engine) BillOnCancel(ctx context.Context, sub domain.Subscription) (int64, error) {
	ctx, span := telemetry.Tracer("billing").Start(ctx, "billing.BillOnCancel",
		trace.WithAttributes(
			attribute.String("subscription_id", sub.ID),
			attribute.String("tenant_id", sub.TenantID),
		),
	)
	defer span.End()

	in, ok, err := e.prepareCancelCredit(ctx, sub)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, nil
	}

	// Resolve EVERY invoice that funded this period — the day-1 base invoice
	// AND any mid-period upgrade/qty proration invoice. The credit is sized off
	// the post-change base, which can exceed the single base invoice;
	// settleUnusedAcrossFunding fans it across the full set so each piece stays
	// within its own invoice's credit-note cap, paid sources credited to
	// balance / unpaid sources relieved on amount_due. A per-invoice cap failure
	// is surfaced loudly here, never swallowed to a silent $0 (ADR-031/ADR-048).
	sources, lookupErr := e.invoices.FindFundingInvoicesForPeriod(ctx, sub.TenantID, sub.ID, in.PeriodStart, in.PeriodEnd)
	if errors.Is(lookupErr, errs.ErrNotFound) {
		slog.InfoContext(ctx, "cancel proration: no in_advance invoice for period; no relief to apply",
			"subscription_id", sub.ID,
			"customer_id", sub.CustomerID,
			"period_start", in.PeriodStart,
		)
		return 0, nil
	}
	if lookupErr != nil {
		// Real lookup error (DB blip, etc.) — fail loud rather than
		// silently shortchange the customer (2026-05-30 design-debt
		// audit Tier 1 #6).
		return 0, fmt.Errorf("cancel proration: lookup funding invoices: %w", lookupErr)
	}
	credited, err := e.settleUnusedAcrossFunding(ctx, sub, sources, in.PeriodEnd, in.CancelAt, in.TotalUnused, "subscription_cancellation", in.Desc)
	if err != nil {
		return 0, fmt.Errorf("cancel proration credit: %w", err)
	}
	slog.Info("cancel proration credit issued",
		"subscription_id", sub.ID,
		"customer_id", sub.CustomerID,
		"amount_cents", credited,
		"funding_invoices", len(sources),
		"period_start", in.PeriodStart,
		"period_end", in.PeriodEnd,
		"canceled_at", in.CancelAt,
	)
	return credited, nil
}

// cancelCreditInputs is the shared setup for the in_advance cancel-proration
// credit: the funding-period bounds, the net unused base, and the human
// description. Computed once by prepareCancelCredit and consumed by BOTH the
// post-commit BillOnCancel and the in-tx BillOnCancelDraftsTx (so the unused-base
// math has a single source of truth).
type cancelCreditInputs struct {
	PeriodStart, PeriodEnd, CancelAt time.Time
	TotalUnused                      int64
	Desc                             string
}

// unusedBaseForPeriod is the single source of the mid-period unused-base
// arithmetic shared by the cancel and plan-swap refund paths (both the
// in-tx draft halves and the post-commit fallbacks). Day-based math to
// avoid int64 overflow — nanosecond math overflows for ~$36+ base fees on
// a full-month proration. Denominator is the FULL plan-interval cycle (not
// the current period's length), since the customer paid
// baseFee × periodDays/fullCycleDays for a stub period; using periodDays
// as the denominator over-refunds whenever periodDays < fullCycleDays.
// Same pattern as emitBaseSegmentLine.
func (e *Engine) unusedBaseForPeriod(ctx context.Context, sub domain.Subscription, periodStart, periodEnd, at time.Time) (int64, error) {
	unusedDays := roundDays(periodEnd.Sub(at))
	if unusedDays <= 0 {
		return 0, nil
	}
	totalUnused := int64(0)
	for _, it := range sub.Items {
		plan, err := e.pricing.GetPlan(ctx, sub.TenantID, it.PlanID)
		if err != nil {
			return 0, fmt.Errorf("get plan %s: %w", it.PlanID, err)
		}
		if plan.BaseBillTiming != domain.BillInAdvance || plan.BaseAmountCents <= 0 {
			continue
		}
		fullCycleDays := roundDays(advanceBillingPeriod(periodStart, plan.BillingInterval, e.tenantLocation(ctx, sub.TenantID), sub.BillingAnchorDay).Sub(periodStart))
		if fullCycleDays <= 0 {
			continue
		}
		baseFee := plan.BaseAmountCents * it.Quantity
		if unused := money.RoundHalfToEven(baseFee*int64(unusedDays), int64(fullCycleDays)); unused > 0 {
			totalUnused += unused
		}
	}
	return totalUnused, nil
}

// prepareCancelCredit runs the guards + day-based unused-base math for a
// mid-period cancel. ok=false means there is nothing to credit (not
// canceled, clean cancel at/after period end, no in_advance items, or zero
// unused) — both cancel-credit paths no-op. Error only on a malformed sub
// (canceled with no canceled_at) or a plan lookup failure.
//
// creditGranter is REQUIRED (boot fails closed if unwired, #442) — the old
// nil arm silently returned "nothing to credit", turning a missing wiring
// into an unrefunded customer. Deleted 2026-07-10 (design-review redesign
// #3 stage 2): a nil granter here is now a test-fixture bug, not a silent
// $0 refund.
// prorationRefundDesc formats the customer-facing credit-note / ledger
// description for a cancel or plan-swap base-fee refund. Civil-day dates render
// in the tenant billing timezone (loc), NOT UTC (ADR-077) — a raw UTC render
// prints the prior calendar day for a positive-offset billing zone. Shared by
// the cancel and both plan-swap refund paths so the format can't drift.
func prorationRefundDesc(leadIn, subCode, verb string, periodStart, periodEnd, at time.Time, loc *time.Location) string {
	return fmt.Sprintf("%s — unused portion of %s base fee (period %s to %s, %s %s)",
		leadIn, subCode,
		periodStart.In(loc).Format("2006-01-02"),
		periodEnd.In(loc).Format("2006-01-02"),
		verb, at.In(loc).Format("2006-01-02"))
}

func (e *Engine) prepareCancelCredit(ctx context.Context, sub domain.Subscription) (cancelCreditInputs, bool, error) {
	if sub.Status != domain.SubscriptionCanceled {
		return cancelCreditInputs{}, false, nil
	}
	if sub.CurrentBillingPeriodStart == nil || sub.CurrentBillingPeriodEnd == nil {
		return cancelCreditInputs{}, false, nil
	}
	if sub.CanceledAt == nil {
		return cancelCreditInputs{}, false, fmt.Errorf("canceled sub has no canceled_at")
	}

	periodStart := *sub.CurrentBillingPeriodStart
	periodEnd := *sub.CurrentBillingPeriodEnd
	cancelAt := *sub.CanceledAt

	// Clean cancel (at or after period end): period was billed normally,
	// no unused portion. cancel_at == period_start is a REAL zero-
	// consumption case (ADR-097: a schedule landing between the loop's
	// refresh and the boundary's cycle advance cancels a fully-prepaid,
	// zero-used period) — relieve the whole period rather than keep it;
	// only strictly-before-period-start stays defensive-rejected.
	if !cancelAt.Before(periodEnd) || cancelAt.Before(periodStart) {
		return cancelCreditInputs{}, false, nil
	}

	totalUnused, err := e.unusedBaseForPeriod(ctx, sub, periodStart, periodEnd, cancelAt)
	if err != nil {
		return cancelCreditInputs{}, false, err
	}
	if totalUnused <= 0 {
		return cancelCreditInputs{}, false, nil
	}

	// Civil-day dates on the customer-facing credit-note / ledger description
	// render in the tenant billing timezone (ADR-077), not UTC — a raw UTC
	// render prints the prior calendar day for a positive-offset billing zone.
	loc := e.tenantLocation(ctx, sub.TenantID)
	desc := prorationRefundDesc("Cancel proration", sub.Code, "canceled", periodStart, periodEnd, cancelAt, loc)

	return cancelCreditInputs{
		PeriodStart: periodStart,
		PeriodEnd:   periodEnd,
		CancelAt:    cancelAt,
		TotalUnused: totalUnused,
		Desc:        desc,
	}, true, nil
}

// BillOnCancelDraftsTx is the ATOMIC half of the cancel-proration credit
// (ADR-057 extension). When EVERY funding invoice for the period is PAID (the
// dominant in_advance cancel), it creates the credit notes as issue_pending
// DRAFTS on the caller's cancel *sql.Tx — so the customer's owed credit is
// durable atomically with the cancel flip. The external Issue (Stripe tax
// reversal + customer-balance grant) runs POST-COMMIT via IssueCancelDrafts, and
// a never-issued draft is recovered by RetryPendingClawbackIssue.
//
// handled=false (no error, no rollback) means the in-tx path DECLINED — any
// UNPAID funding source (PR1 leaves the whole unpaid leg on the post-commit
// BillOnCancel path), a transient funding-lookup blip (BillOnCancel re-handles it
// loudly), or an allocation fault (kept non-blocking, as today). A DRAFT-CREATE
// failure returns an error → the caller rolls the cancel back (never a canceled
// sub with a silently-lost credit). creditedCents is the gross owed (durable once
// the drafts commit, even if the post-commit Issue is later retried).
//
// Engine-writes-on-the-subscription-store's-tx is the BillFinalOnImmediateCancelTx
// precedent (the final invoice already inserts on this same tx).
func (e *Engine) BillOnCancelDraftsTx(ctx context.Context, tx *sql.Tx, sub domain.Subscription) (ids []string, creditedCents int64, handled bool, err error) {
	in, ok, perr := e.prepareCancelCredit(ctx, sub)
	if perr != nil {
		return nil, 0, false, perr // malformed sub → roll the cancel back
	}
	if !ok {
		return nil, 0, true, nil // nothing to credit — handled no-op
	}
	return e.createUnusedDraftsTx(ctx, tx, sub, in, "subscription_cancellation", "cancel proration")
}

// BillOnPlanSwapDraftsTx is the ATOMIC half of the cross-interval plan-swap
// refund — the same folded-write shape as BillOnCancelDraftsTx (deferred
// Bug B, closed 2026-07-05). Pre-fix the ENTIRE swap refund ran post-commit
// via BillOnPlanSwapImmediate on the request ctx: a crash or disconnect
// between the swap commit and the refund permanently lost the customer's
// unused prepayment — the retry 400s on the same-plan guard, and no
// reconciler re-derives a missed swap refund. Now, when every funding
// invoice for the OLD period is PAID (the dominant case), the credit-note
// drafts commit ON the swap tx (issue_pending), post-commit Issue relays the
// external legs, and a never-issued draft is recovered by
// RetryPendingClawbackIssue — reason-agnostic, so swap drafts ride the same
// sweep as cancel drafts.
//
// handled=false declines to the post-commit BillOnPlanSwapImmediate exactly
// as the cancel path declines to BillOnCancel: any UNPAID funding source
// (relief routes through relieveUnpaidPrebill there), a transient lookup
// blip, or an allocation fault. A draft-create failure returns an error →
// the caller rolls the whole swap back (never a swapped sub with a
// silently-lost refund). sub is the PRE-swap snapshot; at is the swap
// instant.
func (e *Engine) BillOnPlanSwapDraftsTx(ctx context.Context, tx *sql.Tx, sub domain.Subscription, at time.Time) (ids []string, creditedCents int64, handled bool, err error) {
	in, ok, perr := e.prepareSwapCredit(ctx, sub, at)
	if perr != nil {
		return nil, 0, false, perr // plan lookup failure → roll the swap back
	}
	if !ok {
		return nil, 0, true, nil // nothing to credit (in_arrears, boundary swap) — handled no-op
	}
	return e.createUnusedDraftsTx(ctx, tx, sub, in, "subscription_plan_change", "plan-swap refund")
}

// prepareSwapCredit mirrors prepareCancelCredit for the mid-period plan
// swap: same guards, same day-based unused-base math over in_advance items
// (the shared arithmetic lives in unusedBaseForPeriod), swap-flavored
// description. ok=false = nothing to credit.
func (e *Engine) prepareSwapCredit(ctx context.Context, sub domain.Subscription, at time.Time) (cancelCreditInputs, bool, error) {
	if sub.CurrentBillingPeriodStart == nil || sub.CurrentBillingPeriodEnd == nil {
		return cancelCreditInputs{}, false, nil
	}
	periodStart := *sub.CurrentBillingPeriodStart
	periodEnd := *sub.CurrentBillingPeriodEnd
	if !at.Before(periodEnd) || !at.After(periodStart) {
		return cancelCreditInputs{}, false, nil
	}

	totalUnused, err := e.unusedBaseForPeriod(ctx, sub, periodStart, periodEnd, at)
	if err != nil {
		return cancelCreditInputs{}, false, err
	}
	if totalUnused <= 0 {
		return cancelCreditInputs{}, false, nil
	}

	loc := e.tenantLocation(ctx, sub.TenantID)
	desc := prorationRefundDesc("Plan-swap refund", sub.Code, "swapped", periodStart, periodEnd, at, loc)

	return cancelCreditInputs{
		PeriodStart: periodStart,
		PeriodEnd:   periodEnd,
		CancelAt:    at,
		TotalUnused: totalUnused,
		Desc:        desc,
	}, true, nil
}

// createUnusedDraftsTx is the shared in-tx trunk of BillOnCancelDraftsTx and
// BillOnPlanSwapDraftsTx: funding lookup → all-paid gate → allocation →
// issue_pending drafts on the caller's tx. what labels log lines ("cancel
// proration" / "plan-swap refund").
func (e *Engine) createUnusedDraftsTx(ctx context.Context, tx *sql.Tx, sub domain.Subscription, in cancelCreditInputs, reason, what string) (ids []string, creditedCents int64, handled bool, err error) {
	// invoices + creditNoteAdjuster are REQUIRED (boot fails closed, #442) —
	// the old "unwired → decline to post-commit fallback" arm is deleted;
	// handled=false remains ONLY for its legitimate runtime reason (an
	// UNPAID funding source routes to the post-commit path below).
	sources, lookupErr := e.invoices.FindFundingInvoicesForPeriod(ctx, sub.TenantID, sub.ID, in.PeriodStart, in.PeriodEnd)
	if errors.Is(lookupErr, errs.ErrNotFound) {
		// No in_advance invoice funded this period (e.g. trial cancel) — nothing
		// to credit. Mirror the post-commit paths' trace so the case stays observable.
		slog.InfoContext(ctx, what+": no in_advance invoice for period; no relief to apply",
			"subscription_id", sub.ID, "customer_id", sub.CustomerID, "period_start", in.PeriodStart)
		return nil, 0, true, nil
	}
	if lookupErr != nil {
		return nil, 0, false, nil // transient lookup blip → the post-commit path re-handles loudly
	}

	// All-paid gate. CreditedCents counts non-voided notes INCLUDING drafts, so
	// the coupled allocator can't be safely re-run post-commit; the atomic path
	// therefore runs only when the whole funding set is paid (the dominant
	// case) and declines the rest to the post-commit fallback.
	for _, src := range sources {
		if src.PaymentStatus != domain.PaymentSucceeded {
			return nil, 0, false, nil
		}
	}

	shares, aerr := e.allocateUnusedAcrossFunding(ctx, sub, sources, in.PeriodEnd, in.CancelAt, in.TotalUnused)
	if aerr != nil {
		// Allocation fault (e.g. all-paid headroom can't absorb): decline to the
		// post-commit path, which surfaces the same loud ERROR without blocking
		// the terminal write — preserving the non-fatal behavior for this fault class.
		return nil, 0, false, nil
	}

	for _, sh := range shares {
		if sh.GrossCents <= 0 {
			continue
		}
		cn, cerr := e.creditNoteAdjuster.CreateAdjustmentDraftTx(ctx, tx, sub.TenantID, sh.Source.ID, sh.GrossCents, reason, in.Desc)
		if cerr != nil {
			// Draft create failed → roll the WHOLE terminal write back (the atomic
			// guarantee: never a canceled/swapped sub with a silently-lost credit).
			// Deliberately atomic-refuse for ANY draft-create failure, including a
			// credit-note cap rejection from a concurrent operator CN shrinking the
			// source's headroom AFTER the off-tx CreditedCents read — the operator's
			// retry self-heals (allocate re-reads the smaller headroom and sizes a
			// fitting share). This is stricter than the pre-fix post-commit path
			// (which logged + let the write proceed with the credit stranded);
			// refusing-and-retrying is the more correct money behavior. Distinct
			// from an UNPAID source (handled=false above → post-commit fallback).
			return nil, 0, false, fmt.Errorf("%s draft on %s: %w", what, sh.Source.ID, cerr)
		}
		ids = append(ids, cn.ID)
		creditedCents += sh.GrossCents
	}
	return ids, creditedCents, true, nil
}

// IssueCancelDrafts issues the cancel-proration credit-note drafts created in-tx
// by BillOnCancelDraftsTx — the POST-COMMIT external relay (Stripe tax reversal +
// customer-balance grant). Best-effort: a failure is logged and the drafts stay
// status='draft' + issue_pending for RetryPendingClawbackIssue to re-issue (the
// credit is already durable). Mirrors FinalizeOnCreateInvoice.
func (e *Engine) IssueCancelDrafts(ctx context.Context, sub domain.Subscription, ids []string) {
	e.issueDrafts(ctx, sub, ids, "cancel proration")
}

// IssueSwapDrafts is the plan-swap twin — issues the drafts created in-tx by
// BillOnPlanSwapDraftsTx after the swap tx commits.
func (e *Engine) IssueSwapDrafts(ctx context.Context, sub domain.Subscription, ids []string) {
	e.issueDrafts(ctx, sub, ids, "plan-swap refund")
}

func (e *Engine) issueDrafts(ctx context.Context, sub domain.Subscription, ids []string, what string) {
	for _, id := range ids {
		if _, err := e.creditNoteAdjuster.Issue(ctx, sub.TenantID, id); err != nil {
			// Issue() is atomic + recoverable (ADR-061): the draft→issued CAS and
			// the internal money effect (amount_due reduction / credit grant) commit
			// together, and the external legs recover on their own. So an error here
			// is self-healing — either the CAS never committed (still 'draft' +
			// issue_pending → RetryPendingClawbackIssue re-drives) or a post-commit
			// external leg failed (refund → RetryRefund; tax reversal →
			// RetryPendingCreditNoteTaxReversal). The credit is durable; no manual
			// reconcile, and no post-flip orphan window (closed by ADR-061).
			slog.ErrorContext(ctx, what+" draft issue failed post-commit; self-heals via reconciler (no manual reconcile needed)",
				"subscription_id", sub.ID, "credit_note_id", id, "error", err)
		}
	}
}

// relieveUnpaidPrebill settles an UNPAID in_advance prebill when the customer
// abandons the period it covers — a mid-period cancel (#22) or a same-cadence
// cross-interval plan swap (ADR-050). totalUnused is the net (tax-exclusive)
// unused base already computed by the caller; src is the unpaid finalized
// invoice for the period; reason/desc are the credit-note categorical reason
// and human description for the issuing path. No customer-balance credit is
// granted (no cash was funded), so it always returns 0 cents; the error is
// non-nil only on a hard failure.
//
// Rule:
//   - Gross up totalUnused to the invoice's gross via its own Total/Subtotal
//     ratio so the relief also covers the unused portion's proportional tax.
//     Velox invoices carry a single uniform tax rate and (post-coupon-cut) no
//     discount, so Total/Subtotal == 1 + rate; a zero-tax invoice grosses up by
//     1× (no change).
//   - Clamp to the invoice's current amount_due.
//   - Whole remaining receivable unused AND nothing paid → Void (clean terminal
//     status, full tax reversal).
//   - Otherwise → adjustment credit note reducing amount_due to the consumed
//     portion (reverses the unused fraction's tax).
//
// When the relevant issuer isn't wired (narrow unit tests), it logs and leaves
// the invoice untouched — the legacy "ride the dunning path" behavior — so
// existing engine tests that don't wire the issuers keep passing.
//
// Idempotency: each caller runs once per terminal event (BillOnCancel once per
// cancel; the swap path once per immediate swap), so this is not re-entered for
// the same invoice in normal operation; the amount_due cap is the backstop.
func (e *Engine) relieveUnpaidPrebill(ctx context.Context, sub domain.Subscription, src domain.Invoice, totalUnused int64, reason, desc string) (int64, error) {
	grossUnused := totalUnused
	if src.SubtotalCents > 0 {
		grossUnused = money.RoundHalfToEven(totalUnused*src.TotalAmountCents, src.SubtotalCents)
	}
	reduceBy := min(grossUnused, src.AmountDueCents)
	if reduceBy <= 0 {
		return 0, nil
	}

	// Whole remaining receivable is unused and nothing has been paid → void it.
	// A partial payment (amount_paid > 0) would make a void annul collected
	// money, so fall through to the reduce path in that case.
	//
	// EXCEPT when the charge is in flight (processing/unknown): voiding an
	// invoice whose payment may still succeed strands captured money on a voided
	// invoice (and invoice.Service.Void now refuses it, ADR-059). Fall through to
	// the reduce path, which creates a FULL-amount clawback draft that Issue()
	// defers until the source settles — paid → full credit to balance, failed →
	// amount_due reduced to 0 (the same end-state the void would have produced),
	// chosen by the source's settled payment_status at issue time.
	if reduceBy >= src.AmountDueCents && src.AmountPaidCents == 0 && !src.PaymentStatus.IsInFlight() {
		// invoiceVoider is required (boot fails closed, #442) — the old
		// "unwired → leave for dunning" arm silently dunned a customer for
		// a fully-unused prebill instead of voiding it.
		if _, err := e.invoiceVoider.Void(ctx, sub.TenantID, src.ID); err != nil {
			return 0, fmt.Errorf("relieve unpaid prebill: void fully-unused %s: %w", src.ID, err)
		}
		slog.InfoContext(ctx, "unpaid prebill relief: voided fully-unused invoice",
			"subscription_id", sub.ID, "customer_id", sub.CustomerID,
			"source_invoice_id", src.ID, "amount_cents", reduceBy, "reason", reason)
		return 0, nil
	}

	// Partially consumed → reduce amount_due down to the consumed portion.
	// (creditNoteAdjuster is required — boot fails closed, #442; the old
	// "unwired → leave for dunning" arm silently under-relieved.)
	if _, err := e.creditNoteAdjuster.CreateAndIssueAdjustment(
		ctx, sub.TenantID, src.ID, reduceBy, reason, desc); err != nil {
		return 0, fmt.Errorf("relieve unpaid prebill: reduce %s to consumed portion: %w", src.ID, err)
	}
	// For an in-flight source the CreateAndIssueAdjustment above created the
	// clawback as a DRAFT and Issue() DEFERRED it (ADR-059) — amount_due is NOT
	// reduced yet; the reconciler issues it once the charge settles. Log the
	// honest state so this doesn't read as a completed reduction.
	if src.PaymentStatus.IsInFlight() {
		slog.InfoContext(ctx, "unpaid prebill relief: source charge in flight — clawback drafted, deferred until settle",
			"subscription_id", sub.ID, "customer_id", sub.CustomerID,
			"source_invoice_id", src.ID, "drafted_cents", reduceBy,
			"payment_status", string(src.PaymentStatus), "reason", reason)
		return 0, nil
	}
	slog.InfoContext(ctx, "unpaid prebill relief: reduced amount_due to consumed portion",
		"subscription_id", sub.ID, "customer_id", sub.CustomerID,
		"source_invoice_id", src.ID, "reduced_by_cents", reduceBy, "reason", reason)
	return 0, nil
}

// settleUnusedAcrossFunding relieves the unused in_advance prepayment of a
// canceled / swapped period across EVERY invoice that funded it. `totalUnused`
// (net, sized off the post-change base across all items) is the authoritative
// figure; it is PARTITIONED across the funding invoices in proportion to each
// invoice's own unused contribution (its subtotal × the unused fraction of the
// window it covers), then settled per invoice:
//   - PAID source   → gross up by its own Total/Subtotal ratio and credit the
//     customer balance via a per-invoice credit note (reversing that invoice's
//     own proportional tax against its own tax_transaction_id). Adds its gross
//     to the returned credited total.
//   - UNPAID source → relieve its amount_due (void / reduce) for its share.
//
// Because the partition sums to totalUnused exactly, the customer is neither
// over- nor under-credited even after a prior reversing change (downgrade /
// qty-decrease) inflated the raw per-invoice shares. A per-invoice credit-note
// cap failure (e.g. a prior credit note already consumed a source's headroom)
// is returned as a HARD error — the caller must surface it loudly, never
// swallow it to a silent $0 credit.
// fundingShare is one funding invoice's allocated slice of the unused-prebill
// credit, from allocateUnusedAcrossFunding. GrossCents is the clamped credit-note
// gross for a PAID source (the customer-balance credit); an UNPAID source carries
// only NetCents (relieveUnpaidPrebill computes its own gross).
type fundingShare struct {
	Source     domain.Invoice
	NetCents   int64
	GrossCents int64
}

// allocateUnusedAcrossFunding is the PURE allocation half: weight each funding
// invoice by its own unused net contribution, cap each at its remaining headroom
// (PAID: creditable = total − already-credited; UNPAID: amount_due), water-fill
// via AllocateByWeightCapped, and surface an unplaceable remainder LOUDLY (all-paid
// fault) or as a WARN (mixed set). READS + MATH only — no Create/Issue — so it is
// safe inside a coordinator tx. CRITICAL: the per-PAID-source net→gross clamp AND
// the remainder loud-fail live HERE, not in the consumer, so the #276/#277/#278
// over-credit invariant (cumulative credit ≤ funding headroom) holds for BOTH the
// post-commit issue/relieve consumer AND the in-tx draft path. The CreditedCents
// headroom read MUST stay off any coordinator tx — it counts non-voided notes
// INCLUDING drafts, so an on-tx read would let an in-flight draft shrink its own
// headroom.
func (e *Engine) allocateUnusedAcrossFunding(ctx context.Context, sub domain.Subscription, sources []domain.Invoice, periodEnd, effectiveAt time.Time, totalUnused int64) ([]fundingShare, error) {
	unusedDays := roundDays(periodEnd.Sub(effectiveAt))
	if unusedDays <= 0 {
		return nil, nil
	}
	weights := make([]int64, len(sources))
	grossCaps := make([]int64, len(sources))
	netCaps := make([]int64, len(sources))
	hasUnpaid := false
	for i, src := range sources {
		windowDays := roundDays(periodEnd.Sub(src.BillingPeriodStart))
		if windowDays > 0 && src.SubtotalCents > 0 {
			// This invoice's own unused net contribution = what it prepaid times
			// the unused fraction of the window it covers. Clamp the unused span
			// to the window so a source whose window starts after the effective
			// instant isn't over-weighted.
			ud := unusedDays
			if ud > windowDays {
				ud = windowDays
			}
			weights[i] = money.RoundHalfToEven(src.SubtotalCents*int64(ud), int64(windowDays))
		}
		// Per-source headroom cap, so the allocator water-fills any overflow onto
		// sources that still have room rather than over-assigning one:
		//   - PAID + real total → remaining creditable (total minus prior
		//     non-voided credit notes via the optional reader; full total
		//     unwired). A prior credit note that shrank it pushes the share
		//     elsewhere.
		//   - UNPAID + real total → amount_due. relieveUnpaidPrebill can only
		//     reduce a receivable down to zero, so capping here makes the
		//     allocator route the un-relievable overflow to PAID sources (which
		//     credit the customer) instead of silently dropping it inside the
		//     relieve clamp (2026-06-15 mixed paid/unpaid fix).
		//   - zero-total source (drift state; a $0 invoice funds nothing) →
		//     no meaningful cap; gross clamps to 0 downstream, so the share
		//     is skipped rather than credited beyond real money in.
		switch {
		case src.PaymentStatus == domain.PaymentSucceeded && src.TotalAmountCents > 0:
			// creditHeadroom is REQUIRED (boot fails closed, #442) — the old
			// nil arm used the FULL invoice total as the cap, silently
			// disabling the #276-#278 overcharge guard (a source already
			// partially credited by a prior CN could be over-credited).
			credited, err := e.creditHeadroom.CreditedCents(ctx, sub.TenantID, src.ID)
			if err != nil {
				return nil, fmt.Errorf("read creditable headroom for %s: %w", src.ID, err)
			}
			grossCap := src.TotalAmountCents - credited
			if grossCap < 0 {
				grossCap = 0
			}
			grossCaps[i] = grossCap
			netCap := grossCap
			if src.SubtotalCents > 0 {
				netCap = money.RoundHalfToEven(grossCap*src.SubtotalCents, src.TotalAmountCents)
			}
			netCaps[i] = netCap
		case src.TotalAmountCents > 0:
			hasUnpaid = true
			grossCap := src.AmountDueCents
			if grossCap < 0 {
				grossCap = 0
			}
			grossCaps[i] = grossCap
			netCap := grossCap
			if src.SubtotalCents > 0 {
				netCap = money.RoundHalfToEven(grossCap*src.SubtotalCents, src.TotalAmountCents)
			}
			netCaps[i] = netCap
		default:
			grossCaps[i] = src.TotalAmountCents
			netCaps[i] = totalUnused // non-binding
		}
	}

	nets, remainder := money.AllocateByWeightCapped(totalUnused, weights, netCaps)
	if remainder > 0 {
		if !hasUnpaid {
			// All-paid funding set can't absorb the owed credit — a genuine fault
			// (period already fully credited, or drift). Fail LOUD; never silently
			// drop the customer's money.
			return nil, fmt.Errorf("cannot place unused-prebill credit: %d of %d exceeds funding headroom across %d invoice(s)", remainder, totalUnused, len(sources))
		}
		// Mixed/unpaid set: paid sources already absorbed all they could (the
		// allocator water-fills overflow onto them first); the residual is value
		// an UNPAID invoice can't relieve beyond its amount_due — nothing was
		// paid for it, so there's no cash to return. Not a fault, but log it.
		slog.WarnContext(ctx, "unused-prebill credit: residual unplaceable on unpaid funding invoice(s)",
			"subscription_id", sub.ID,
			"residual_cents", remainder,
			"total_unused", totalUnused,
			"funding_invoices", len(sources))
	}

	var shares []fundingShare
	for i, src := range sources {
		net := nets[i]
		if net <= 0 {
			continue
		}
		sh := fundingShare{Source: src, NetCents: net}
		if src.PaymentStatus == domain.PaymentSucceeded {
			gross := net
			if src.SubtotalCents > 0 {
				gross = money.RoundHalfToEven(net*src.TotalAmountCents, src.SubtotalCents)
			}
			if gross > grossCaps[i] {
				gross = grossCaps[i] // clamp net→gross rounding overshoot to headroom
			}
			sh.GrossCents = gross
		}
		shares = append(shares, sh)
	}
	return shares, nil
}

// settleUnusedAcrossFunding is the POST-COMMIT consumer of the allocation: PAID
// sources → a credit note that credits the customer's balance
// (CreateAndIssueAdjustment); UNPAID sources → relieveUnpaidPrebill. Behavior is
// unchanged from the pre-split single-pass version; the allocation math now lives
// in allocateUnusedAcrossFunding so the in-tx draft path (BillOnCancelDraftsTx)
// shares the exact same #277-guarded caps/clamp/remainder logic.
func (e *Engine) settleUnusedAcrossFunding(ctx context.Context, sub domain.Subscription, sources []domain.Invoice, periodEnd, effectiveAt time.Time, totalUnused int64, reason, desc string) (int64, error) {
	shares, err := e.allocateUnusedAcrossFunding(ctx, sub, sources, periodEnd, effectiveAt, totalUnused)
	if err != nil {
		return 0, err
	}
	var credited int64
	for _, sh := range shares {
		src := sh.Source
		if src.PaymentStatus == domain.PaymentSucceeded {
			if sh.GrossCents <= 0 {
				continue
			}
			if _, err := e.creditNoteAdjuster.CreateAndIssueAdjustment(ctx, sub.TenantID, src.ID, sh.GrossCents, reason, desc); err != nil {
				// Loud-fail: a source's credit-note cap rejecting its share means
				// money would be stranded. Surface it; do NOT drop to $0.
				return credited, fmt.Errorf("credit unused prebill on %s (share %d of total %d): %w", src.ID, sh.GrossCents, totalUnused, err)
			}
			credited += sh.GrossCents
			continue
		}
		// Unpaid funding invoice → relieve its receivable for its share.
		if _, err := e.relieveUnpaidPrebill(ctx, sub, src, sh.NetCents, reason, desc); err != nil {
			return credited, err
		}
	}
	return credited, nil
}

// BillOnPlanSwapImmediate issues the refund credit for the unused
// portion of an in_advance billed period when a sub's plan is swapped
// to a different cadence/interval mid-period. Returns the cents amount
// granted (0 when none).
//
// Mirrors BillOnCancel's refund math but takes `at` as a parameter and
// does not gate on canceled status — the caller (Service.UpdateItem)
// invokes this BEFORE applying the plan swap, while the OLD plan is
// still on the items so plan lookups resolve the outgoing rate.
//
// No-op when:
//   - no current period set
//   - at at/after period_end OR at/before period_start (clean swap)
//   - no item's plan is in_advance (nothing prebilled to refund)
//   - source in_advance invoice not paid (mirrors BillOnCancel paid-check)
//   - computed unused amount rounds to zero
//
// Caller is responsible for applying the plan swap and updating the
// billing period AFTER this call — this method only handles the
// refund for the OLD in_advance portion.
func (e *Engine) BillOnPlanSwapImmediate(ctx context.Context, sub domain.Subscription, at time.Time) (int64, error) {
	ctx, span := telemetry.Tracer("billing").Start(ctx, "billing.BillOnPlanSwapImmediate",
		trace.WithAttributes(
			attribute.String("subscription_id", sub.ID),
			attribute.String("tenant_id", sub.TenantID),
		),
	)
	defer span.End()

	if sub.CurrentBillingPeriodStart == nil || sub.CurrentBillingPeriodEnd == nil {
		return 0, nil
	}

	periodStart := *sub.CurrentBillingPeriodStart
	periodEnd := *sub.CurrentBillingPeriodEnd

	if !at.Before(periodEnd) || !at.After(periodStart) {
		return 0, nil
	}

	totalUnused, err := e.unusedBaseForPeriod(ctx, sub, periodStart, periodEnd, at)
	if err != nil {
		return 0, err
	}
	if totalUnused <= 0 {
		return 0, nil
	}

	// Civil-day dates on the customer-facing credit-note / ledger description
	// render in the tenant billing timezone (ADR-077), not UTC.
	loc := e.tenantLocation(ctx, sub.TenantID)
	desc := prorationRefundDesc("Plan-swap refund", sub.Code, "swapped", periodStart, periodEnd, at, loc)

	// Fan the refund across EVERY invoice that funded the period (base +
	// mid-period upgrade) — same multi-source settlement as cancel — so a swap
	// after a prior upgrade doesn't overrun the single base invoice's
	// credit-note cap and silently strand the customer's unused prepayment.
	// Paid sources credit the balance (own tax reversal), unpaid sources relieve
	// amount_due (ADR-050); a per-invoice cap failure surfaces loudly here.
	sources, lookupErr := e.invoices.FindFundingInvoicesForPeriod(ctx, sub.TenantID, sub.ID, periodStart, periodEnd)
	if errors.Is(lookupErr, errs.ErrNotFound) {
		slog.InfoContext(ctx, "plan-swap refund: no in_advance invoice for period; no credit to grant",
			"subscription_id", sub.ID,
			"customer_id", sub.CustomerID,
			"period_start", periodStart,
		)
		return 0, nil
	}
	if lookupErr != nil {
		// Real lookup error (DB blip, etc.) — fail loud rather than
		// silently shortchange the customer (2026-05-30 design-debt
		// audit Tier 1 #6).
		return 0, fmt.Errorf("plan-swap refund: lookup funding invoices: %w", lookupErr)
	}
	credited, err := e.settleUnusedAcrossFunding(ctx, sub, sources, periodEnd, at, totalUnused, "subscription_plan_change", desc)
	if err != nil {
		return 0, fmt.Errorf("plan-swap refund credit: %w", err)
	}
	slog.Info("plan-swap refund credit issued",
		"subscription_id", sub.ID,
		"customer_id", sub.CustomerID,
		"amount_cents", credited,
		"funding_invoices", len(sources),
		"period_start", periodStart,
		"period_end", periodEnd,
		"swapped_at", at,
	)
	return credited, nil
}

// RetryPendingCharges picks up invoices flagged for auto-charge retry
// and attempts to charge them. CRON path — the wall-clock scheduler
// calls this every tick. ADR-029 Phase 1: clock-pinned invoices are
// excluded from this query and are processed instead by
// RetryPendingChargesForClock during catchup.
func (e *Engine) RetryPendingCharges(ctx context.Context, limit int) (int, []error) {
	if e.charger == nil || e.paymentSetups == nil {
		return 0, nil
	}

	pending, err := e.invoices.ListAutoChargePending(ctx, limit)
	if err != nil {
		return 0, []error{fmt.Errorf("list pending charges: %w", err)}
	}
	return e.processAutoCharge(ctx, pending)
}

// RetryPendingChargesForClock is the catchup-path counterpart to
// RetryPendingCharges. Called by the test-clock catchup orchestrator
// after period generation, so any invoice whose customer attached a
// PM since the last advance fires its charge as part of THIS Advance,
// not on a future wall-clock tick. ADR-029 Phase 1.
//
// Return shape matches RetryPendingCharges (count + per-invoice
// errors) so the orchestrator can log the same telemetry shape per
// catchup phase.
func (e *Engine) RetryPendingChargesForClock(ctx context.Context, tenantID, clockID string, limit int) (int, []error) {
	if e.charger == nil || e.paymentSetups == nil {
		return 0, nil
	}
	pending, err := e.invoices.ListAutoChargePendingForClock(ctx, tenantID, clockID, limit)
	if err != nil {
		return 0, []error{fmt.Errorf("list pending charges for clock %s: %w", clockID, err)}
	}
	return e.processAutoCharge(ctx, pending)
}

// EnrollStalledForDunning routes finalized, still-pending invoices that
// are auto_charge_pending with NO resolvable payment method into a
// dunning campaign so they reach a terminal (pause/cancel/uncollectible)
// instead of being retried forever by RetryPendingCharges — closing the
// card-less auto_charge_pending "limbo" sink. CRON path; clock-pinned
// subs are excluded (ListAutoChargePending's NOT EXISTS) and handled by
// EnrollStalledForDunningForClock during catchup.
//
// Runs AFTER RetryPendingCharges in the cycle: a card decline already
// sets auto_charge_pending=false + starts dunning inline, and a
// successful charge clears the flag, so the candidates that remain are
// the no-card ones. StartDunning is idempotent, so any invoice that
// still carries a run is a no-op.
func (e *Engine) EnrollStalledForDunning(ctx context.Context, limit int) (int, []error) {
	pending, err := e.invoices.ListAutoChargePending(ctx, limit)
	if err != nil {
		return 0, []error{fmt.Errorf("list stalled auto-charge for dunning: %w", err)}
	}
	return e.enrollStalledForDunning(ctx, pending, domain.DunningCauseNoPaymentMethod)
}

// EnrollStalledForDunningForClock is the catchup-path counterpart to
// EnrollStalledForDunning — enrolls clock-pinned no-card invoices into
// dunning as part of an Advance, so a card-less SIMULATED subscription
// reaches a terminal under test clocks too, not only on the wall clock.
// ADR-029 disjoint flows.
func (e *Engine) EnrollStalledForDunningForClock(ctx context.Context, tenantID, clockID string, limit int) (int, []error) {
	pending, err := e.invoices.ListAutoChargePendingForClock(ctx, tenantID, clockID, limit)
	if err != nil {
		return 0, []error{fmt.Errorf("list stalled auto-charge for dunning (clock %s): %w", clockID, err)}
	}
	return e.enrollStalledForDunning(ctx, pending, domain.DunningCauseNoPaymentMethod)
}

// failedDunningBackfillCoolOff lets the inline SettleFailed StartDunning win the
// common case before the backfill sweep considers an invoice — so the sweep stays
// a pure backstop for the crash / exhausted-retry window, mirroring the payment
// reconciler's unknown/processing cool-offs.
const failedDunningBackfillCoolOff = 10 * time.Minute

// EnrollFailedWithoutDunning is the dunning_backfill reconciler: it recovers
// finalized invoices left in payment_status='failed' with NO dunning run.
// SettleFailed starts dunning POST-COMMIT (best-effort, behind the firstForThisPI
// gate), so a crash or an exhausted StartDunning retry in that window leaves the
// invoice failed-but-undunned, and a same-PI webhook redelivery skips the restart
// (firstForThisPI=false). This sweep re-drives the idempotent StartDunning so the
// invoice still reaches a terminal instead of sitting failed forever. CRON /
// livemode path; clock-pinned invoices are excluded by the store query and dunned
// inline during Advance. Shares enrollStalledForDunning's body, so it inherits the
// adapter's "dunning disabled" swallow and dunningFailureAt simulated-time anchoring.
func (e *Engine) EnrollFailedWithoutDunning(ctx context.Context, limit int) (int, []error) {
	// dunningStarter is REQUIRED (boot fails closed, #442) — the old nil
	// early-returns in the three enroll sweeps silently left card-less
	// invoices out of dunning forever.
	// Cool-off cutoff. The sweep is livemode-only (clock-pinned rows are excluded
	// by the store query), so a real-time cutoff against updated_at is correct and
	// never touches simulated time.
	olderThan := time.Now().UTC().Add(-failedDunningBackfillCoolOff) // wall-clock: reconciler is livemode-only; clock-pinned rows are excluded by the query
	pending, err := e.invoices.ListFailedWithoutDunningRun(ctx, olderThan, limit)
	if err != nil {
		return 0, []error{fmt.Errorf("list failed-without-dunning for backfill: %w", err)}
	}
	return e.enrollStalledForDunning(ctx, pending, domain.DunningCausePaymentFailed)
}

// enrollStalledForDunning is the shared body: enroll each candidate via
// the idempotent StartDunning. failureAt anchors on the invoice's issue/
// period instant (simulation-relative for clock-pinned invoices), NOT
// wall-clock now, so the first retry lands grace-days after the real
// (simulated) collection failure — matching the inline decline path's
// timing. A "dunning disabled" outcome is swallowed by the adapter as a
// deliberate skip; other errors are collected per-invoice so one bad row
// doesn't abort the sweep.
func (e *Engine) enrollStalledForDunning(ctx context.Context, pending []domain.Invoice, cause domain.DunningStartCause) (int, []error) {
	swept := 0
	var errs []error
	for _, inv := range pending {
		if err := e.dunningStarter.StartDunning(ctx, inv.TenantID, inv.ID, inv.CustomerID, dunningFailureAt(inv), cause); err != nil {
			errs = append(errs, fmt.Errorf("enroll invoice %s for dunning: %w", inv.ID, err))
			continue
		}
		swept++
	}
	return swept, errs
}

// dunningFailureAt resolves the simulated instant a no-card invoice's
// collection "failed" — its issue time, else its period end. Returns the
// zero Time when neither is set; StartDunning then falls back to its own
// clock. Anchoring on the invoice (not wall-clock now) keeps clock-pinned
// dunning on simulated time.
func dunningFailureAt(inv domain.Invoice) time.Time {
	if inv.IssuedAt != nil && !inv.IssuedAt.IsZero() {
		return *inv.IssuedAt
	}
	if !inv.BillingPeriodEnd.IsZero() {
		return inv.BillingPeriodEnd
	}
	return time.Time{}
}

// processAutoCharge is the shared body of RetryPendingCharges and
// RetryPendingChargesForClock — once the candidate list is fetched,
// the per-invoice charge loop is identical for cron and catchup.
//
// Error classification: a card decline (Stripe 402 with a typed
// decline_code) is an EXPECTED business outcome, not a catchup
// failure. ChargeInvoice already inline-fires StartDunning for the
// declined invoice and stamps invoice.payment_status='failed' →
// dunning takes over from there. Returning the decline as an error
// would push the test-clock catchup into 'internal_failure' for what
// is supposed to be a normal payment-failure flow.
//
// Only Unknown (5xx, network drop, timeout — outcome unresolved) and
// non-Stripe errors (config, infrastructure) escalate to the caller.
// Unknown errors return because the reconciler needs to follow up on
// the ambiguous PaymentIntent; the catchup orchestrator surfaces them
// to the operator. Industry parity: Stripe Test Clocks don't fail
// when a tester uses a decline-card; they record the decline and
// move on.
func (e *Engine) processAutoCharge(ctx context.Context, pending []domain.Invoice) (int, []error) {
	charged := 0
	var errs []error
	for _, inv := range pending {
		// Per-invoice charge lease (HA hazard #1): exactly one sweep
		// leader enters the charge leg per invoice per 5m window. The CAS
		// re-asserts the full eligibility predicate, so a rival leader's
		// outcome (or a webhook settle) landing between list and claim
		// fails the claim here — BEFORE the credit-apply/refresh path
		// that could otherwise mint a divergent Stripe idempotency key.
		claimed, cerr := e.invoices.ClaimAutoCharge(ctx, inv.TenantID, inv.ID)
		if cerr != nil {
			errs = append(errs, fmt.Errorf("claim auto-charge %s: %w", inv.ID, cerr))
			continue
		}
		if !claimed {
			continue
		}
		// Re-apply customer credits BEFORE charging. An invoice lands in this
		// sweep precisely when its finalize-time flow didn't complete — and the
		// most important such case is a FAILED credit application at cycle
		// close (billOnePeriod flags auto_charge_pending and deliberately skips
		// the charge to avoid overcharging). Charging the raw amount_due here
		// without re-applying credits would consummate exactly that overcharge
		// on the retry. ApplyToInvoiceAtomic is safe to re-run: it drains
		// min(amount_due, current balance) — an already-applied invoice or an
		// empty balance applies nothing. Failure → skip this invoice this tick
		// (flag stays set; next sweep retries) rather than charge pre-credit.
		if e.credits != nil && inv.AmountDueCents > 0 {
			sim, nowErr := e.SimForInvoice(ctx, inv.TenantID, inv.ID)
			at := sim.At
			if nowErr != nil {
				at = e.clock.Now(ctx) // ADR-030: injected clock, never bare wall-clock
			}
			if _, err := e.credits.ApplyToInvoiceAt(ctx, inv.TenantID, inv.CustomerID, inv.ID, inv.AmountDueCents, at, inv.InvoiceNumber); err != nil {
				slog.Warn("auto-charge retry: credit re-apply failed — skipping charge to avoid overcharging; will retry next tick",
					"invoice_id", inv.ID, "error", err)
				// Provably pre-Stripe: release the lease so the next
				// tick/Advance retries immediately instead of waiting it out.
				_ = e.invoices.ReleaseAutoChargeClaim(ctx, inv.TenantID, inv.ID)
				continue
			}
			// Refresh: credits may have reduced (or fully covered) amount_due.
			refreshed, err := e.invoices.GetInvoice(ctx, inv.TenantID, inv.ID)
			if err != nil {
				errs = append(errs, fmt.Errorf("refresh invoice %s after credit apply: %w", inv.ID, err))
				continue
			}
			inv = refreshed
			if inv.AmountDueCents <= 0 {
				// Fully credit-covered — settle without a card charge. Sweep
				// rows are status='finalized' by query predicate, so the
				// draft gate that protects billOnePeriod's equivalent block
				// holds here by construction.
				if _, err := e.invoices.MarkPaid(ctx, inv.TenantID, inv.ID, "", at); err != nil {
					errs = append(errs, fmt.Errorf("mark credit-covered invoice %s paid: %w", inv.ID, err))
					continue
				}
				if err := e.invoices.SetAutoChargePending(ctx, inv.TenantID, inv.ID, false); err != nil {
					// Benign relative to set(true): the next sweep re-lists the row
					// and its predicate re-check skips ineligible invoices.
					slog.Warn("failed to clear auto_charge_pending", "invoice_id", inv.ID, "error", err)
				}
				// This is the path that strands dunning (the confirmed bug): the
				// sweep settles via credits without the handler's resolve. Close
				// the run here (best-effort; processRun pre-check backstops).
				e.resolveDunningRecovered(ctx, inv.TenantID, inv.ID)
				charged++
				slog.Info("auto-charge retry: fully covered by credits, marked paid", "invoice_id", inv.ID)
				continue
			}
		}

		stripeCusID, stripePMID, err := e.paymentSetups.ResolveForCharge(ctx, inv.TenantID, inv.CustomerID)
		if err != nil || stripePMID == "" || stripeCusID == "" {
			// Genuinely no PM (not a resolve error) and never emailed: send
			// the setup-link email ONCE (ADR-087 follow-up). This closes the
			// sweep-mediated silence: a proration invoice is enrolled here
			// without any finalize-time email, and a finalize-time resolver
			// error deliberately queues without emailing — pre-fix both aged
			// into overdue with zero customer contact. The no_pm_notified_at
			// stamp is the send-once marker (finalize-time senders stamp it
			// too), so the per-tick revisit never duplicates; a
			// skipped-no-email outcome is left unstamped so it self-heals if
			// the customer gains an address. Runs inside the charge lease, so
			// rival HA sweep leaders can't double-send within a tick.
			if err == nil && inv.NoPMNotifiedAt == nil {
				outcome, nerr := e.noPMNotifier.NotifyNoPaymentMethod(ctx, inv.TenantID, inv, "auto_charge_retry_no_pm")
				switch {
				case nerr != nil:
					slog.Warn("auto-charge retry: no-PM notification failed", "invoice_id", inv.ID, "error", nerr)
				case outcome == domain.NotifySkippedNoEmail:
					slog.Info("auto-charge retry: setup-link email skipped: customer has no email on file", "invoice_id", inv.ID)
				default:
					if serr := e.invoices.SetNoPMNotifiedAt(ctx, inv.TenantID, inv.ID, e.clock.Now(ctx)); serr != nil {
						slog.Warn("auto-charge retry: failed to stamp no-PM notified marker", "invoice_id", inv.ID, "error", serr)
					}
					slog.Info("auto-charge retry: setup-link email queued", "invoice_id", inv.ID)
				}
			}
			// Provably pre-Stripe (no chargeable PM resolved): release so
			// dunning enrollment / a card attach retries without lease lag.
			_ = e.invoices.ReleaseAutoChargeClaim(ctx, inv.TenantID, inv.ID)
			continue
		}

		chargeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		if _, err := e.charger.ChargeInvoice(chargeCtx, inv.TenantID, inv, stripeCusID, stripePMID); err != nil {
			cancel()
			// Card decline: expected outcome. ChargeInvoice stamped
			// invoice.payment_status='failed' AND started dunning inline for
			// definite failures (stripe.go startDunningWithRetry; the
			// payment_intent.payment_failed webhook is the idempotent second
			// path, and a lost/late webhook is backstopped by the
			// EnrollFailedWithoutDunning reconciler). Don't push the catchup
			// to internal_failure for a normal payment failure.
			// SetAutoChargePending(false) is the OWNERSHIP HANDOFF: dunning's
			// retry schedule drives subsequent attempts, and clearing the
			// flag keeps the sweep from ever being a second retry owner
			// (ADR-087 §3 records why the finalize sites' flag-on-decline is
			// still safe: a persisted decline is status='failed', which this
			// sweep's pending-only predicate never lists).
			var pe *payment.PaymentError
			if errors.As(err, &pe) && pe.DeclineCode != "" {
				if err := e.invoices.SetAutoChargePending(ctx, inv.TenantID, inv.ID, false); err != nil {
					// Benign relative to set(true): the next sweep re-lists the row
					// and its predicate re-check skips ineligible invoices.
					slog.Warn("failed to clear auto_charge_pending", "invoice_id", inv.ID, "error", err)
				}
				slog.Info("auto-charge declined; dunning will retry on schedule",
					"invoice_id", inv.ID, "decline_code", pe.DeclineCode)
				continue
			}
			// Transient (breaker open / pre-Stripe timeout): skip
			// this tick without bumping dunning. The next catchup
			// will retry.
			if errors.Is(err, payment.ErrPaymentTransient) {
				slog.Info("auto-charge skipped; transient breaker/timeout",
					"invoice_id", inv.ID)
				continue
			}
			// Everything else (Unknown PaymentError or non-Stripe
			// error) escalates — the catchup operator gets visible
			// feedback to investigate.
			errs = append(errs, fmt.Errorf("charge invoice %s: %w", inv.ID, err))
			continue
		}
		cancel()

		if err := e.invoices.SetAutoChargePending(ctx, inv.TenantID, inv.ID, false); err != nil {
			// Benign relative to set(true): the next sweep re-lists the row
			// and its predicate re-check skips ineligible invoices.
			slog.Warn("failed to clear auto_charge_pending", "invoice_id", inv.ID, "error", err)
		}
		charged++
		slog.Info("auto-charge retry succeeded", "invoice_id", inv.ID)
	}
	return charged, errs
}

// RetryTaxForInvoice re-runs ApplyTaxToLineItems against an invoice
// that's currently parked at tax_status in (pending, failed) and
// persists the new decision atomically. Backs the operator-triggered
// "Retry tax" action surfaced by the unified Attention shape.
//
// Idempotent under retry: each call increments tax_retry_count and
// rewrites the per-line and invoice-level tax fields. A retry that
// succeeds clears TaxPendingReason / TaxErrorCode and unblocks
// finalize. A retry that fails again writes the new typed code so
// the dashboard banner refreshes — operators get an immediate signal
// of whether their fix worked, without waiting on the background
// retry worker.
//
// Gates:
//   - invoice must be draft (postgres re-asserts this under FOR UPDATE in
//     UpdateTaxAtomic — it's the genuine data invariant)
//   - tax_status must be pending or failed (retry-specific policy enforced
//     HERE; the store no longer re-asserts it, since ComputeTaxForInvoice
//     legitimately computes tax on a draft whose tax_status is still 'ok')
//   - subscription is loaded if present (so jurisdiction-by-plan-tax-
//     code logic in ApplyTaxToLineItems sees the same inputs as the
//     original cycle build)
func (e *Engine) RetryTaxForInvoice(ctx context.Context, tenantID, invoiceID string) (domain.Invoice, error) {
	inv, err := e.invoices.GetInvoice(ctx, tenantID, invoiceID)
	if err != nil {
		return domain.Invoice{}, err
	}
	if inv.Status != domain.InvoiceDraft {
		return domain.Invoice{}, errs.InvalidState(fmt.Sprintf(
			"tax retry only valid on draft invoices (current: %s)", inv.Status))
	}
	if inv.TaxStatus != domain.InvoiceTaxPending && inv.TaxStatus != domain.InvoiceTaxFailed {
		return domain.Invoice{}, errs.InvalidState(fmt.Sprintf(
			"tax retry only valid when tax_status in (pending, failed) (current: %s)", inv.TaxStatus))
	}
	return e.computeAndPersistInvoiceTax(ctx, tenantID, invoiceID, inv)
}

// ComputeTaxForInvoice computes (or recomputes) tax for a DRAFT invoice
// and persists it atomically — regardless of the current tax_status.
//
// This is the finalize-time entry point for manual / operator-composed
// invoices (BillingReason in {"", manual}). Unlike cycle invoices, which
// carry engine-computed tax from the moment they're built, manual invoices
// accrue line items incrementally in the composer and have no tax until the
// operator finalizes. Computing here mirrors Stripe, which calculates tax
// when an invoice is finalized rather than at draft-create time.
//
// It shares the same compute-and-persist core as RetryTaxForInvoice; the
// only difference is the gate. RetryTaxForInvoice requires tax_status in
// (pending, failed) because it backs the operator "Retry" affordance on a
// stuck invoice. This path runs on a fresh draft whose tax_status is still
// the create-time default ('ok'), so it gates on draft-only.
func (e *Engine) ComputeTaxForInvoice(ctx context.Context, tenantID, invoiceID string) (domain.Invoice, error) {
	inv, err := e.invoices.GetInvoice(ctx, tenantID, invoiceID)
	if err != nil {
		return domain.Invoice{}, err
	}
	if inv.Status != domain.InvoiceDraft {
		return domain.Invoice{}, errs.InvalidState(fmt.Sprintf(
			"tax compute only valid on draft invoices (current: %s)", inv.Status))
	}
	return e.computeAndPersistInvoiceTax(ctx, tenantID, invoiceID, inv)
}

// computeAndPersistInvoiceTax is the shared core behind RetryTaxForInvoice
// and ComputeTaxForInvoice. Callers own the gating (draft, tax_status); this
// re-runs ApplyTaxToLineItems against the invoice's current line items and
// persists the decision atomically via UpdateTaxAtomic. inv must already be
// loaded by the caller (the gate reads it) — passed in to avoid a second
// GetInvoice round-trip.
func (e *Engine) computeAndPersistInvoiceTax(ctx context.Context, tenantID, invoiceID string, inv domain.Invoice) (domain.Invoice, error) {
	items, err := e.invoices.ListLineItems(ctx, tenantID, invoiceID)
	if err != nil {
		return domain.Invoice{}, fmt.Errorf("list line items: %w", err)
	}

	taxApp, err := e.ApplyTaxToLineItems(ctx, tenantID, inv.CustomerID, inv.Currency,
		inv.SubtotalCents, inv.DiscountCents, items)
	if err != nil {
		return domain.Invoice{}, fmt.Errorf("recompute tax: %w", err)
	}

	// Read the net subtotal/discount OFF the taxApp result, mirroring the
	// cycle build path (billOnePeriod). In tax-INCLUSIVE mode
	// ApplyTaxToLineItems carves tax OUT of the gross the operator entered,
	// so taxApp.SubtotalCents is the net (< inv.SubtotalCents) and
	// taxApp.TaxAmountCents is the portion already embedded in the gross —
	// adding it onto inv.SubtotalCents (the gross) would double-count tax
	// and overstate the total by ~one tax amount. In tax-EXCLUSIVE mode
	// (the common case) taxApp.SubtotalCents/DiscountCents pass through
	// unchanged (== inv.*), so this is a no-op there. Persisting the net
	// subtotal/discount keeps subtotal − discount + tax == gross, the same
	// invariant the cycle path maintains, and keeps the header consistent
	// with the per-line net amounts ApplyTaxToLineItems already wrote.
	totalWithTax := taxApp.SubtotalCents - taxApp.DiscountCents + taxApp.TaxAmountCents
	if totalWithTax < 0 {
		totalWithTax = 0
	}

	update := domain.InvoiceTaxRetryUpdate{
		SubtotalCents: taxApp.SubtotalCents,
		DiscountCents: taxApp.DiscountCents,
		// ONE assignment carries every tax fact (see domain/tax_facts.go).
		TaxFacts:         taxApp.TaxFacts,
		TotalAmountCents: totalWithTax,
		TaxNextRetryAt:   nextTaxRetry(ctx, taxApp.TaxStatus, taxApp.TaxErrorCode, inv.TaxRetryCount),
		// A computation over a fresh draft (tax_status still the create-time
		// 'ok') is the INITIAL attempt — the finalize-time compute of an
		// operator-composed invoice — not a retry, so it must not bump
		// tax_retry_count ("Tax retries: 1" on every healthy one-off invoice,
		// found live on FLOW I12/NIM-000258). Derived from the invoice's
		// PRIOR state rather than the caller: RetryTaxForInvoice gates on
		// pending/failed so it always counts, and a re-finalize of a STUCK
		// draft (prior compute failed → pending/failed) counts too — that
		// second compute genuinely is a retry. Cycle invoices never pass
		// through here at build time, so the counter now uniformly means
		// "attempts after the initial computation" for both invoice types.
		InitialAttempt: inv.TaxStatus != domain.InvoiceTaxPending && inv.TaxStatus != domain.InvoiceTaxFailed,
	}

	return e.invoices.UpdateTaxAtomic(ctx, tenantID, invoiceID, update, items)
}

// nextTaxRetry decides whether the reconciler should pick this row
// up again, and when. Three outcomes encoded as a return:
//
//   - ok / exempt: nil → "ready now" (the row leaves the retryable
//     filter anyway because tax_status flips out of pending/failed,
//     so the value doesn't matter; nil keeps the column tidy).
//   - retryable failure (provider_outage / unknown) under cap:
//     time.Now + taxRetryBackoff(attempts). The next reconciler
//     tick that crosses this timestamp picks it up.
//   - non-retryable failure (auth / customer_data / jurisdiction /
//     provider_not_configured): nil. The reconciler skips because
//     the code is outside taxRetryableCodes; nil keeps the row out
//     of any future retry timing logic.
//   - retryable but at-or-over cap: nil. The cap-check in
//     ListPendingTaxRetry stops fetching this row; nil is correct
//     because there's no next retry to schedule.
//
// `attempts` is the number of retries the row has already had
// (i.e. inv.TaxRetryCount BEFORE this retry runs). UpdateTaxAtomic
// increments it server-side.
//
// ctx carries effective-now via clock.WithEffectiveNow on
// clock-pinned invoices; without binding, falls back to wall-clock.
// Catchup's ListPendingTaxRetryForClock intentionally ignores
// `tax_next_retry_at` (see its doc comment in invoice/postgres.go), so the
// stamp's domain doesn't load-bear for clock-pinned scheduling — but
// keeping it in the simulation domain matches the rest of the row's
// timestamps for dashboard consistency (ADR-030).
func nextTaxRetry(ctx context.Context, status domain.InvoiceTaxStatus, errCode string, attempts int) *time.Time {
	if status == domain.InvoiceTaxOK {
		return nil
	}
	retryable := false
	for _, c := range taxRetryableCodes() {
		if c == errCode {
			retryable = true
			break
		}
	}
	if !retryable {
		return nil
	}
	// attempts here is BEFORE the increment that UpdateTaxAtomic
	// applies, so the next attempt index is `attempts` (0-based
	// schedule).
	if attempts >= maxTaxRetryAttempts-1 {
		// This was the final attempt; no next retry to schedule.
		return nil
	}
	t := clock.Now(ctx).Add(taxRetryBackoff(attempts))
	return &t
}

// advanceBillingPeriod is the legacy interval-only roll-forward —
// preserves the input timestamp's day-of-month for monthly intervals.
// Kept for non-cycle-close contexts (segment-length computation,
// in_advance base coverage projection, cancel-proration math) where
// we want the natural interval roll regardless of billing_time.
//
// **Cycle-close MUST use domain.NextBillingPeriodEnd instead** — that
// helper honors billing_time so calendar-billing subs auto-re-anchor
// after plan-interval changes drift the anchor day-of-month.
// advanceBillingPeriod is the anniversary interval advance used for stub
// proration denominators (fullCycleDays). Delegates to domain.AddBillingInterval
// so the engine and the subscription handler share one definition and can't
// drift. The advance is computed in the tenant timezone `loc` (ADR-058) so the
// full-cycle day count is host-TZ-independent. NOT the calendar cycle-close
// advance — that's domain.NextBillingPeriodEnd.
func advanceBillingPeriod(from time.Time, interval domain.BillingInterval, loc *time.Location, anchorDay int) time.Time {
	return domain.AddBillingInterval(from, interval, loc, anchorDay)
}

// TenantLocation exposes the tenant's billing timezone so the subscription
// handler's proration day-math can anchor its denominator in the same zone the
// engine's period boundaries are computed in (ADR-058). Wired via
// subH.SetTenantLocator(engine).
func (e *Engine) TenantLocation(ctx context.Context, tenantID string) *time.Location {
	return e.tenantLocation(ctx, tenantID)
}

// NetPaymentTermDays exposes the tenant's configured Net payment terms so the
// subscription handler's proration invoice stamps the same terms + due date
// the engine's own cycle/create invoices do (which read ts.NetPaymentTerms
// inline). Falls back to 30 — the schema default — when settings are
// unreadable. Wired via subH.SetNetTermsReader(engine).
func (e *Engine) NetPaymentTermDays(ctx context.Context, tenantID string) int {
	if e.settings != nil {
		if ts, err := e.settings.Get(ctx, tenantID); err == nil && ts.NetPaymentTerms > 0 {
			return ts.NetPaymentTerms
		}
	}
	return 30
}

// tenantLocation resolves the tenant's billing timezone (the one org-level
// timezone, ADR-077). It anchors every month/year calendar advance (period
// boundaries AND proration denominators, ADR-058) so billing date-math is
// independent of the host time.Local and the value's ambient Location. Failures
// collapse to UTC.
func (e *Engine) tenantLocation(ctx context.Context, tenantID string) *time.Location {
	if e.settings == nil {
		return time.UTC
	}
	ts, err := e.settings.Get(ctx, tenantID)
	if err != nil || ts.Timezone == "" {
		return time.UTC
	}
	loc, err := time.LoadLocation(ts.Timezone)
	if err != nil {
		return time.UTC
	}
	return loc
}

// cancelTrialAtEndFromEngine routes the engine's trial branch (the fourth
// activation writer, ADR-069) through the dedicated free cancel and fires
// the same terminal vocabulary as the subscription service's winner-once
// helper: subscription.canceled (canceled_by=schedule,
// reason=trial_end_cancel) + an AuditActionCancel row. NO trial_ended, NO
// invoice, NO cycle advance. A CAS conflict is a silent no-op — another
// site handled the sub, or the schedule was rescinded and the next tick
// routes the fresh state.
func (e *Engine) cancelTrialAtEndFromEngine(ctx context.Context, sub domain.Subscription) error {
	if sub.TrialEndAt == nil {
		return nil
	}
	canceled, err := e.subs.CancelAtTrialEnd(ctx, sub.TenantID, sub.ID, *sub.TrialEndAt)
	if errors.Is(err, domain.ErrTrialCancelConflict) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("cancel at trial end: %w", err)
	}
	if e.auditLogger != nil {
		meta := map[string]any{
			"action":       "trial_end_cancel",
			"customer_id":  canceled.CustomerID,
			"triggered_by": "schedule",
		}
		if canceled.CanceledAt != nil {
			meta["canceled_at"] = canceled.CanceledAt.UTC().Format(time.RFC3339)
		}
		// trial_end_at is its own fact, under its own key. The row's sim axis
		// (test_clock_id + sim_effective_at) is the audit writer's, resolved
		// from the ctx clock binding — see subscription.cancelTrialAtEnd, the
		// other writer of this vocabulary.
		if sub.TrialEndAt != nil {
			meta["trial_end_at"] = sub.TrialEndAt.UTC().Format(time.RFC3339)
		}
		_ = e.auditLogger.Log(ctx, sub.TenantID, domain.AuditActionCancel, "subscription", canceled.ID, canceled.Code, meta)
	}
	// subscription.canceled (schedule + trial_end_cancel) is enqueued IN the
	// CancelAtTrialEnd tx (store-level DispatchTx subset).
	slog.Info("trial ended with a due cancel schedule — canceled free of charge (engine path)",
		"subscription_id", canceled.ID, "tenant_id", sub.TenantID)
	return nil
}
