package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/payment"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/money"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/platform/scheduler"
	"github.com/sagarsuperuser/velox/internal/platform/telemetry"
	"github.com/sagarsuperuser/velox/internal/subscription"
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

// SettingsReader reads tenant settings for invoice configuration.
type SettingsReader interface {
	NextInvoiceNumber(ctx context.Context, tenantID string) (string, error)
	// NextInvoiceNumberTx allocates on the caller's tx. The period closers
	// (ADR-115) take the number AFTER the row CAS on the same tx, so a lost
	// race burns no number and a rolled-back invoice leaves no sequence gap.
	NextInvoiceNumberTx(ctx context.Context, tx *sql.Tx, tenantID string) (string, error)
	Get(ctx context.Context, tenantID string) (domain.TenantSettings, error)
}

// Engine orchestrates the billing cycle: finds subscriptions due for billing,
// aggregates usage, computes charges, and generates invoices with line items.
//
// It coordinates across domain boundaries (subscription, usage, pricing, invoice)
// without those domains knowing about each other.
type Engine struct {
	subs     SubscriptionReader
	usage    UsageAggregator
	pricing  PricingReader
	invoices InvoiceWriter
	credits  CreditApplier
	// #22: settle an UNPAID in_advance prebill at mid-period cancel.
	invoiceVoider      InvoiceVoider
	creditNoteAdjuster CreditNoteAdjuster
	creditHeadroom     CreditHeadroomReader
	settings           SettingsReader
	paymentSetups      PaymentReadiness
	charger            InvoiceCharger
	profiles           BillingProfileReader
	customers          CustomerReader
	// Issue #1019: the two reads LiveCreditBalance composes (live_balance.go).
	creditBalances  CreditBalanceReader
	customerSubs    CustomerSubscriptionLister
	taxProviders    TaxProviderResolver
	taxCalcStore    TaxCalculationWriter
	clock           clock.Clock
	testClocks      TestClockReader
	events          domain.EventDispatcher
	noPMNotifier    NoPaymentMethodNotifier
	dunningStarter  DunningStarter
	dunningResolver DunningResolver
	cancelExecutor  ScheduledCancelExecutor
	auditLogger     AuditWriter
	// invoiceGrace is INVOICE_GRACE_PERIOD (ADR-120): how long after a period
	// ends the billing runs wait before they close it. See SetInvoiceGrace.
	invoiceGrace time.Duration
	// deferredThresholdNotes dedups the crossed-but-deferred loudness
	// artifact to once per (sub, period) — see noteThresholdDeferred.
	deferredThresholdNotes sync.Map

	// ADR-101 segment-source seam (intervals_reader.go): the snapshot
	// reader, the boot-frozen mode (off|shadow|on), and the parity
	// counters the corpus CI gate asserts on.
	intervalSnap IntervalSnapshotter
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
// are born finalized — so without this they leave no audit trace at all.
//
// born_finalized marks the row as one of those: the invoice was finalized in
// the statement that created it, never a draft someone finalized. The
// activity line reads it to say why the invoice exists (P-1, ADR-128); the
// rows invoice.Service.Finalize writes never carry it.
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
			"born_finalized":     true,
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

// EmitFinalizeAuditTx is emitFinalizeAuditTx for an invoice another domain
// inserts already finalized on its own transaction: the subscription handler's
// proration invoice (upgrade, seat increase, added item). It writes the same
// row the engine writes for its own invoices, so the two cannot drift (#1279).
func (e *Engine) EmitFinalizeAuditTx(ctx context.Context, tx *sql.Tx, inv domain.Invoice) error {
	return e.emitFinalizeAuditTx(ctx, tx, inv)
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
	e.DispatchInvoiceFinalized(ctx, sub.TenantID, inv)
}

// DispatchInvoiceFinalized sends invoice.finalized for an invoice born
// finalized, after the transaction that created it committed. Exported for
// the subscription handler's proration invoice, so it sends the payload the
// engine sends for its own invoices (#1279). Drafts send nothing: they send
// it from invoice.Service.Finalize when they finalize.
//
// It carries the invoice as the create returned it (ADR-131). It is sent
// before the collector applies the customer's credit (ADR-116), so the
// invoice's amount_due_cents is the amount before that credit.
func (e *Engine) DispatchInvoiceFinalized(ctx context.Context, tenantID string, inv domain.Invoice) {
	if inv.Status != domain.InvoiceFinalized {
		return
	}
	if e.events != nil {
		e.dispatchEvent(ctx, tenantID, domain.EventInvoiceFinalized, domain.InvoiceWebhookData(inv, nil))
	}
}

// SetInvoiceVoider / SetCreditNoteAdjuster wire the two issuers the cancel and
// plan-swap relief writes through (#22, ADR-118): a void when an unpaid
// invoice's period was not used at all, an adjustment credit note otherwise. Both REQUIRED — boot fails closed if
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
// auto_charge_pending + emailed once, then visited by RetryPendingCharges
// every tick with nothing to charge — with no escalation it would sit unpaid
// for good. Routing the no-card invoice into the SAME dunning machine gives it
// the same escalation-to-terminal the declined-card path gets. The invoice
// stays queued while the run escalates, so a card added mid-campaign is
// charged on the next visit.
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
	StartDunning(ctx context.Context, tenantID, invoiceID, customerID string, simFailedAt time.Time, cause domain.DunningStartCause) error
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

// InvoiceVoider annuls a finalized, unpaid invoice inside a cancel or plan-swap
// transaction: the relief for an in_advance prebill the customer never used any
// of (#22). VoidUnchangedTx voids on the caller's tx and refuses if the invoice
// changed since the engine read it; AfterVoid runs every void's post-commit
// legs (stop collection at Stripe, tax reversal, webhook, close the dunning
// run). Implemented by *invoice.Service. Narrow on purpose so billing
// keeps coordinating through a domain-typed interface, not an invoice import.
type InvoiceVoider interface {
	VoidUnchangedTx(ctx context.Context, tx *sql.Tx, tenantID string, snapshot domain.Invoice) (domain.Invoice, error)
	AfterVoid(ctx context.Context, tenantID string, voided domain.Invoice)
}

// CreditNoteAdjuster writes the cancel / plan-swap relief as adjustment credit
// notes: on a PAID source the note credits the customer's balance, on an UNPAID
// one it reduces amount_due to the used portion (proportional tax reversed in
// both). The amount is gross (tax-inclusive). Implemented by
// *creditnote.Service.
type CreditNoteAdjuster interface {
	// CreateAdjustmentDraftTx creates the adjustment credit note as an
	// issue_pending DRAFT on the caller's coordinator tx (ADR-057), to be issued
	// post-commit. Used by the atomic cancel-proration-credit path.
	CreateAdjustmentDraftTx(ctx context.Context, tx *sql.Tx, tenantID, invoiceID string, grossCents int64, reason, description string) (domain.CreditNote, error)
	// LockInvoicesForDraftsTx takes every listed invoice's credit-note lock
	// before the first draft, so drafts on several invoices in one tx cannot
	// deadlock with a concurrent credit note on one of them.
	LockInvoicesForDraftsTx(ctx context.Context, tx *sql.Tx, tenantID string, invoiceIDs []string) error
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
//
// hold is the charge hold the caller's claim returned (ADR-127): the charge
// re-checks it right before sending and sends nothing without it.
type InvoiceCharger interface {
	ChargeInvoice(ctx context.Context, tenantID string, inv domain.Invoice, hold domain.ChargeHold, stripeCustomerID, stripePaymentMethodID string) (domain.Invoice, error)
}

// SubscriptionReader reads subscription and plan data for billing.
type SubscriptionReader interface {
	// GetDueBilling: wall-clock cron path. Returns ONLY subs that
	// are NOT pinned to a test clock (ADR-028 disjoint flows). The
	// scheduler tick fans this out per livemode. Every due scan takes
	// exclude: ids the caller already attempted this run (drainDue).
	// now gates billing failures (ADR-119); dueBy = now less the invoice
	// grace admits due instants (ADR-120).
	GetDueBilling(ctx context.Context, now, dueBy time.Time, exclude []string, limit int) ([]domain.Subscription, error)
	// GetDueBillingForClock: operator-driven catchup path. Returns
	// ONLY subs pinned to the given clock whose next_billing_at is
	// on-or-before that clock's frozen time. Called by RunCatchup
	// after Advance flips status to 'advancing'.
	GetDueBillingForClock(ctx context.Context, tenantID, clockID string, exclude []string, limit int) ([]domain.Subscription, error)
	// GetDueBillingForTenant: operator-triggered (POST /v1/billing/run)
	// path, scoped to ONE tenant under RLS. Wall-clock-due, non-clock-
	// pinned subs for the caller's tenant only — never cross-tenant.
	// now and dueBy as for GetDueBilling.
	GetDueBillingForTenant(ctx context.Context, tenantID string, now, dueBy time.Time, exclude []string, limit int) ([]domain.Subscription, error)
	Get(ctx context.Context, tenantID, id string) (domain.Subscription, error)
	// RecordBillingFailure saves one counted billing failure on a wall-clock
	// subscription (ADR-119) and reports whether a row was written. Zero rows
	// is normal: the due instant moved, or another run counted this attempt.
	// It writes only the five billing_* columns.
	RecordBillingFailure(ctx context.Context, tenantID, id string, f subscription.BillingFailureWrite) (bool, error)
	// WithTenantTx opens a period closer's transaction (ADR-115). The store
	// that owns the period state machine owns the coordinator tx: the engine
	// runs ClosePeriodTx / FireScheduledCancellationTx as the FIRST statement,
	// then the invoice-number allocation and the invoice insert on the same
	// tx, so a CAS miss rolls back before any invoice exists.
	WithTenantTx(ctx context.Context, tenantID string, fn func(tx *sql.Tx) error) error
	// ClosePeriodTx is THE period writer: one UPDATE that row-locks the
	// subscription and proves `expected` (status, period start, watermark),
	// then re-stamps the period. subscription.ErrWatermarkMoved when the
	// snapshot is stale — the caller wrote nothing. The threshold fire calls
	// it verify-only (same values) to take the lock and prove the
	// snapshot before inserting.
	ClosePeriodTx(ctx context.Context, tx *sql.Tx, tenantID, id string, expected subscription.PeriodSnapshot, periodStart, periodEnd, nextBillingAt time.Time, anchorDay int) error
	// ClosePeriod is the own-tx wrapper for the trial-active advance (no
	// invoice, nothing else to commit alongside).
	ClosePeriod(ctx context.Context, tenantID, id string, expected subscription.PeriodSnapshot, periodStart, periodEnd, nextBillingAt time.Time, anchorDay int) error
	// ApplyDuePendingItemPlansAtomic swaps plan_id ← pending_plan_id for every
	// item on the subscription whose pending_plan_effective_at <= now, in one
	// statement. Returns the refreshed items (including any that weren't due,
	// untouched). Called at the cycle boundary so the just-closed period is
	// billed on the pre-change plan and the next period uses the new plan.
	// Returns nil + no error when no items are due (caller proceeds with the
	// existing plan).
	ApplyDuePendingItemPlansAtomic(ctx context.Context, tenantID, subscriptionID string, now time.Time) ([]domain.SubscriptionItem, error)

	// FireScheduledCancellationTx is the terminal cycle close (ADR-115): on
	// the closer's tx it flips a sub with a due cancel_at / cancel_at_period_end
	// intent to canceled, CAS-ing on `expected` as well as status='active',
	// seals its intervals and enqueues subscription.canceled; the final
	// invoice is inserted on the same tx. The `at` argument is the engine's
	// effectiveNow so canceled_at stays consistent with test-clock time
	// travel. subscription.ErrWatermarkMoved covers both "period moved" and
	// "already terminated" — the closer's snapshot is stale either way.
	FireScheduledCancellationTx(ctx context.Context, tx *sql.Tx, tenantID, id string, expected subscription.PeriodSnapshot, at time.Time) (domain.Subscription, error)

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
// AggregateClaims is the multi-dim-aware path: each event goes to the first
// claim (pricing rule) it matches, and each bucket is aggregated under the
// meter's one aggregation (ADR-130 §2). The cycle scan, the customer-usage
// endpoint, and the create_preview surface all call it — preview math ==
// invoice math by construction. AggregateForBillingPeriodByAgg is the
// closers' read for meters with no pricing rules: one query for every
// meter of an invoice's window, with the same aggregates, the `last`
// tie-break included.
type UsageAggregator interface {
	AggregateForBillingPeriodByAgg(ctx context.Context, tenantID, customerID string, meters map[string]string, from, to time.Time) (map[string]decimal.Decimal, error)
	AggregateClaims(ctx context.Context, tenantID, customerID, meterID, aggregation string, claims []domain.UsageClaim, from, to time.Time) ([]domain.RuleAggregation, error)
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
	// and BillOnPlanSwapTx; the latter inserts the cross-interval swap's invoice
	// on the coordinator's tx, atomically with the plan write + watermark
	// advance. The finalize audit row rides this same tx too (emitted by the
	// caller right after the insert), so it commits atomically with the swap.
	CreateInvoiceWithLineItemsTx(ctx context.Context, tx *sql.Tx, tenantID string, inv domain.Invoice, items []domain.InvoiceLineItem) (domain.Invoice, error)
	GetInvoice(ctx context.Context, tenantID, id string) (domain.Invoice, error)
	// FindFundingInvoicesForPeriod returns EVERY non-voided invoice that
	// prepaid the period (a written-off one counts: billed but unpaid) — the
	// day-1 base invoice AND any mid-period upgrade / quantity-increase
	// proration invoice issued against the same cycle. A period-wide unused credit (cancel / cross-interval
	// plan-swap / downgrade clawback) is sized off the post-change base, which
	// can exceed the single base invoice's total; fanning the credit across the
	// full funding set lets each piece stay within its own invoice's
	// credit-note cap. ErrNotFound when nothing funded the period (trial / pure
	// in_arrears) so callers keep their existing no-op behavior.
	FindFundingInvoicesForPeriod(ctx context.Context, tenantID, subscriptionID string, periodStart, periodEnd time.Time) ([]domain.Invoice, error)
	// GetLatestThresholdInvoiceForCycle returns the threshold watermark: the
	// newest threshold-fired invoice whose billing_period_start falls inside
	// [periodStart, periodEnd) and that still bills its window — any status
	// but voided; a written-off one keeps its window billed (ADR-066
	// amendment 2026-10-23). errs.ErrNotFound when there is none.
	GetLatestThresholdInvoiceForCycle(ctx context.Context, tenantID, subscriptionID string, periodStart, periodEnd time.Time) (domain.Invoice, error)
	// GetThresholdInvoiceForCycleAnyStatus is the fire-once probe's read: the
	// same window in ANY status, since the cycle's one threshold slot
	// (idx_invoices_threshold_unique_per_cycle) is held whatever the status.
	// The full row lets the probe also HEAL a crash between invoice-create
	// and the $0/credited MarkPaid (ADR-066). Never a watermark.
	GetThresholdInvoiceForCycleAnyStatus(ctx context.Context, tenantID, subscriptionID string, periodStart, periodEnd time.Time) (domain.Invoice, error)
	// GetLatestThresholdInvoiceForCycleTx is the same read on the closer's tx
	// (ADR-115): issued after the row-lock CAS it sees every fire that
	// committed before the lock, so the closer proves the watermark it built
	// its lines against is still the watermark — or rolls back. It holds the
	// threshold invoice FOR KEY SHARE until the tx ends, so a void after the
	// read lands after the closer's commit; a payment does not wait for it.
	GetLatestThresholdInvoiceForCycleTx(ctx context.Context, tx *sql.Tx, tenantID, subscriptionID string, periodStart, periodEnd time.Time) (domain.Invoice, error)
	// GetInvoiceForPeriod returns the newest non-voided invoice keyed exactly
	// like idx_invoices_billing_idempotency (subscription, period_start,
	// period_end) — the row billOnePeriod's ErrAlreadyExists re-entry path
	// heals ($0 MarkPaid crash window, ADR-066).
	GetInvoiceForPeriod(ctx context.Context, tenantID, subscriptionID string, periodStart, periodEnd time.Time) (domain.Invoice, error)
	ListLineItems(ctx context.Context, tenantID, invoiceID string) ([]domain.InvoiceLineItem, error)
	MarkPaid(ctx context.Context, tenantID, id string, stripePaymentIntentID string, paidAt time.Time) (domain.Invoice, error)
	// SetNoPMNotifiedAt stamps the send-once marker for the no-PM setup-link
	// email (ADR-087 follow-up): whoever delivers the email stamps it; the
	// auto-charge sweep checks it so a card-less invoice is emailed exactly
	// once, not once per tick.
	SetNoPMNotifiedAt(ctx context.Context, tenantID, invoiceID string, at time.Time) error
	// ClaimAutoCharge takes the per-invoice charge hold (HA hazard #1,
	// ADR-127): a claim re-asserting the full eligibility predicate and
	// refusing any set hold. The zero ChargeHold = another owner holds the
	// invoice, a lost charge waits for the stale-hold sweep, or its state
	// moved on — skip, never charge.
	ClaimAutoCharge(ctx context.Context, tenantID, id string) (domain.ChargeHold, error)
	// ReleaseChargeHold clears this owner's hold (compared by token) on
	// paths that sent no charge, so the next tick/Advance retries
	// immediately instead of waiting for the stale-hold sweep.
	ReleaseChargeHold(ctx context.Context, tenantID, id string, hold domain.ChargeHold) error
	ListAutoChargePending(ctx context.Context, after domain.InvoiceKeyset, limit int) ([]domain.Invoice, error)
	// ListAutoChargePendingForClock is the catchup-path counterpart to
	// ListAutoChargePending — returns invoices whose owning subscription
	// is pinned to the given clock. ADR-029 Phase 1: simulation-time
	// charge attempts only fire on operator Advance, never on the
	// wall-clock cron tick, mirroring Stripe Test Clocks.
	ListAutoChargePendingForClock(ctx context.Context, tenantID, clockID string, after domain.InvoiceKeyset, limit int) ([]domain.Invoice, error)
	// ListFailedWithoutDunningRun powers the dunning_backfill reconciler:
	// finalized, still-owed invoices in payment_status='failed' with NO
	// dunning run — the SettleFailed post-commit crash / exhausted-retry
	// window. olderThan is the cool-off.
	ListFailedWithoutDunningRun(ctx context.Context, olderThan time.Time, limit int) ([]domain.Invoice, error)
	SetAutoChargePending(ctx context.Context, tenantID, id string, pending bool) error
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
// arms diverge on money: a nil creditNoteAdjuster skips a tax reversal, a
// nil creditHeadroom disables the #276-#278 overcharge cap).
//
// Why boot-time and not construction-time: the engine and its peer services
// are mutually referential (invoiceSvc needs the engine as clock resolver;
// the engine needs invoiceSvc as invoice voider), so a fully-populated
// construction-time deps struct is not achievable without restructuring the
// services — the setters exist BECAUSE of that cycle. Boot-validate is the
// minimal complete mechanism: same guarantee, no restructuring. Tests that
// construct partial engines deliberately do not call this.
//
// Reflection walks the struct's interface fields so a future 27th
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
	// Expiry guard — shared with the finalize pre-check so the rule that
	// blocks a commit and the rule that blocks a finalize can never drift.
	if err := e.CheckTaxCalculationUsable(ctx, tenantID, invoiceID, calculationID); err != nil {
		return err
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

// CheckTaxCalculationUsable is the Stripe-Tax calc expiry guard, callable
// on its own. Stripe Tax calculations are valid for 24h after creation;
// a draft held longer fails at Stripe's CreateFromCalculation with
// "calculation expired". Non-nil means: this calculation can NEVER be
// committed — only a fresh calculation (retry tax) fixes it.
//
// Two callers, deliberately: CommitTax (so an expired calc is refused at
// the commit) and invoice.Service.Finalize's PRE-transition check. The
// finalize check is the load-bearing one — the post-finalize commit
// failure is swallowed BY DECISION (transient provider errors defer to
// the orphan reconciler, which re-commits idempotently within its 24h
// window), and that decision is only sound when deferral can succeed.
// An expired calc is the one failure the reconciler can never heal: the
// same age rule blocks its re-commit, and after the scan's 24h bound the
// orphan exits silently — an invoice collected with tax printed on the
// PDF but never reported to Stripe. So expiry must refuse the finalize
// UP FRONT, while the invoice is still a draft and "retry tax" is still
// available. That is also what this guard's message has promised the
// operator all along.
//
// Returns nil when there is nothing to guard: store unwired, no
// calculation, a tenant with no committing provider (none/manual), or no
// audit row for the calculation. A read ERROR fails closed (2026-10-11): it
// used to return nil — "never block an invoice on a DB blip" — but a
// finalize let through on an unreadable guard could finalize an expired
// calculation, which is the one failure nothing heals. A refused finalize
// is retried; callers that only need "proven expired" check the
// tax_calculation_expired code.
func (e *Engine) CheckTaxCalculationUsable(ctx context.Context, tenantID, invoiceID, calculationID string) error {
	if e.taxCalcStore == nil || calculationID == "" || e.taxProviders == nil || e.settings == nil {
		return nil
	}
	ctx = auth.WithTenantID(ctx, tenantID)
	// Mirror CommitTax's provider short-circuit: a tenant whose provider
	// resolves to nil (none/manual, or since disconnected) skips the commit
	// entirely, so an aged calc must not block THEIR finalize either.
	ts, err := e.settings.Get(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("tax expiry guard: read settings: %w", err)
	}
	provider, perr := e.taxProviders.Resolve(ctx, ts)
	if perr != nil {
		return fmt.Errorf("tax expiry guard: resolve provider: %w", perr)
	}
	if provider == nil {
		return nil
	}
	// Backfill invoice_id on the audit row now that the invoice is
	// persisted — Record wrote it during tax application before the
	// invoice existed. The lookup below filters on it, so a failed link
	// would read as "no row" and skip the guard: fail instead.
	if linkErr := e.taxCalcStore.LinkInvoice(ctx, tenantID, invoiceID, calculationID); linkErr != nil {
		return fmt.Errorf("tax expiry guard: link calculation %s to invoice: %w", calculationID, linkErr)
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
		if age := time.Now().UTC().Sub(createdAt); age > taxCalculationMaxAge { //nolint:forbidigo // wall-clock: Stripe's calc TTL is a real-world 24h, never the simulated clock
			de := errs.InvalidState(fmt.Sprintf(
				"tax calculation expired (age %s, max %s) — retry tax to refresh, then finalize",
				age.Truncate(time.Minute), taxCalculationMaxAge))
			// Stable machine code: integrators and the dashboard branch on
			// this, never on the message text.
			return de.WithCode("tax_calculation_expired")
		}
	} else if !errors.Is(lookupErr, errs.ErrNotFound) {
		return fmt.Errorf("tax expiry guard: look up calculation age: %w", lookupErr)
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
// so they escalate to a terminal instead of only being re-visited by
// RetryPendingCharges. See the DunningStarter doc-comment for the full rationale.
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

// SetInvoiceGrace sets INVOICE_GRACE_PERIOD (ADR-120). Usage reaches Velox
// some time after it happens, so the two wall-clock billing runs (the
// scheduler's RunCycle and the operator's POST /v1/billing/run) close a
// period only once the grace has passed after its end, or after a
// mid-period cancel_at. Usage stamped inside the period that arrives within
// the grace is then on the invoice.
//
// The other closers do not wait. An operator acting on one subscription
// (immediate cancel, retry-billing, the pre-close of every other
// subscription write) closes the ended period now, and a test clock closes at
// the instant it is advanced to: there the operator decides when usage
// arrives. Dunning's final pause and cancel do wait: they close through
// BillDue with the same grace (subscription.Service.SetInvoiceGrace, ADR-120
// amendment 2026-10-22). Unset (0) closes as soon as the period is due.
func (e *Engine) SetInvoiceGrace(d time.Duration) {
	e.invoiceGrace = d
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

// usageFloors are the instants before which a subscription's usage window for
// a meter may not start, beyond the window's own bounds (ADR-012 amendment
// 2026-10-22). Periods start at midnight of the day they started (ADR-012),
// and usage is aggregated per customer and meter with no subscription key, so
// a window that opens at the period start can reach usage that is not this
// subscription's:
//
//   - trialEnd: usage before the trial end is trial usage, which is free. The
//     first paid period starts at midnight of the trial-end day.
//   - coverageEnds: per meter, where another subscription of the customer
//     stopped covering it (domain.MeterCoverageEnd). The guard against two
//     live subscriptions on one meter (rejectMeterOverlap) means an end
//     inside this window came before this subscription took the meter, and
//     the usage up to it was that subscription's to bill.
type usageFloors struct {
	trialEnd     *time.Time
	coverageEnds map[string][]time.Time
}

// start is where a usage window [s, e) for meter mid opens: at s, moved to
// the trial end when that is later, then to the latest coverage end of
// another subscription strictly inside the window. Strictly before e is what
// keeps an earlier subscription's window whole: a later subscription's
// coverage always ends after the earlier one's window does, so it never
// moves it. The result can be at or past e, an empty window.
func (f usageFloors) start(mid string, s, e time.Time) time.Time {
	if f.trialEnd != nil && f.trialEnd.After(s) {
		s = *f.trialEnd
	}
	for _, t := range f.coverageEnds[mid] {
		if t.After(s) && t.Before(e) {
			s = t
		}
	}
	return s
}

// loadUsageFloors reads sub's usage floors for windows inside [from, to].
// A read failure is returned: a window built without its floor bills usage
// another subscription already billed, or trial usage.
func (e *Engine) loadUsageFloors(ctx context.Context, sub domain.Subscription, from, to time.Time) (usageFloors, error) {
	var f usageFloors
	if sub.TrialEndAt != nil {
		te := *sub.TrialEndAt
		f.trialEnd = &te
	}
	ends, err := e.intervalSnap.ListCustomerMeterCoverageEnds(ctx, sub.TenantID, sub.CustomerID, sub.ID, from, to)
	if err != nil {
		return usageFloors{}, fmt.Errorf("usage window: other subscriptions' meter coverage: %w", err)
	}
	for _, end := range ends {
		if f.coverageEnds == nil {
			f.coverageEnds = make(map[string][]time.Time)
		}
		f.coverageEnds[end.MeterID] = append(f.coverageEnds[end.MeterID], end.EndsAt)
	}
	return f, nil
}

// meterUsageIntervals builds, per meter, the windows a closer bills usage
// over. Each item x segment contributes [seg.start, seg.end] to every meter
// in its segment's plan, so a meter is billed only for the time a plan
// carrying it was on the subscription: a mid-period plan swap, item add or
// item removal splits the period. segsByItem also holds items that left the
// subscription during the window. Every closer that bills a period's usage
// by segment calls this: the period close (buildLineItems), the immediate
// cancel and the cross-interval swap (buildCancelLineItems), the operator
// preview (previewAsClose) and the live credit read (unbilledUsageCents).
//
// floors moves each window's start first (usageFloors.start), in both maps:
// trial usage and another subscription's usage are nobody's here, whatever
// the bucket.
//
// TWO maps (ADR-066 §4): clamped carries the watermark-CLAMPED windows that
// additive (sum/count) buckets bill from; usage in
// [periodStart, billedThrough) already landed on the mid-cycle threshold
// invoice. full carries the UNCLAMPED windows: non-additive (max/last)
// buckets that the fire deliberately deferred (no line on the watermark
// invoice) must aggregate over the full window, because clamping a max to
// the residual window drops a pre-fire peak (partial-window mis-bill), and
// splitting it double-bills. With no watermark full is empty and only
// clamped is used. Intervals are not merged; callers run mergeUsageIntervals.
func meterUsageIntervals(sub domain.Subscription, segsByItem map[string][]baseSegment, plans map[string]domain.Plan, periodStart, periodEnd time.Time, billedThrough *time.Time, floors usageFloors) (clamped, full map[string][]usageInterval) {
	clamped = map[string][]usageInterval{}
	full = map[string][]usageInterval{}
	addMeterInterval := func(mid string, start, end time.Time) {
		start = floors.start(mid, start, end)
		if billedThrough != nil && end.After(start) {
			full[mid] = append(full[mid], usageInterval{start, end})
		}
		// Clamp to the threshold watermark. Intervals fully inside the
		// billed window drop out entirely. Nil watermark = no threshold
		// fire = full period, unchanged.
		if billedThrough != nil && start.Before(*billedThrough) {
			start = *billedThrough
		}
		if !end.After(start) {
			return
		}
		clamped[mid] = append(clamped[mid], usageInterval{start, end})
	}
	present := make(map[string]struct{}, len(sub.Items))
	for _, it := range sub.Items {
		present[it.ID] = struct{}{}
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
		if _, stillPresent := present[itemID]; stillPresent {
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
	return clamped, full
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

// errPeriodMoved: billOnePeriod wrote NOTHING because its snapshot went stale —
// another closer (a threshold fire, a plan swap, a cancel, a sibling close)
// committed between this close's read and its write (ADR-115).
// billSubscription re-reads once and re-plans; a second miss leaves the sub
// to the next fetch.
var errPeriodMoved = errors.New("billing: period moved under this close — nothing written")

// periodCloseOutcome is what commitPeriodClose committed.
type periodCloseOutcome struct {
	inv      domain.Invoice       // zero when no invoice was inserted (empty close)
	canceled *domain.Subscription // set when the scheduled-cancel arm fired
}

// commitPeriodClose is the ONE place a cycle close commits (ADR-115). Order
// inside the tx:
//
//  1. row lock + snapshot proof — FireScheduledCancellationTx when the
//     schedule says this boundary terminates the sub, ClosePeriodTx
//     otherwise. The CAS is the FIRST statement, so a closer planning from a
//     stale snapshot fails here and never reaches an insert; a closer that
//     blocks on another closer's lock re-evaluates the CAS against the row
//     that closer committed.
//  2. threshold-watermark re-read on this tx: the fire the lines were built
//     against (builtAgainst) must still be the newest fire for the period —
//     nil vs nil, or the same instant. A reset=false fire that committed
//     between buildLineItems and this tx does not move the period (the CAS
//     passes) but has already billed part of the window; this compare is
//     what catches it. So does a void that committed in between. The read
//     holds the threshold invoice FOR KEY SHARE, so a later void waits for
//     this commit (a payment does not).
//  3. NextInvoiceNumberTx — after the CAS, so a lost race burns no number.
//  4. CreateInvoiceWithLineItemsTx + emitFinalizeAuditTx (ADR-090 shared fate).
//  5. the subscription.net_negative_usage row for belowZero, when any usage
//     was below zero (net_negative.go), with or without an invoice.
//
// Steps 3-4 run only when lines != nil (an empty close advances without an
// invoice). No network I/O inside: tax ran before, webhook / CommitTax /
// credits / charge run after. Error contract: subscription.ErrWatermarkMoved
// → errPeriodMoved (nothing written); errs.ErrAlreadyExists (the cycle
// index — reachable only for a pre-cutover row whose invoice exists while
// the sub is still due, since the CAS now runs before the insert) propagates
// for the heal path; anything else is an error.
func (e *Engine) commitPeriodClose(ctx context.Context, sub domain.Subscription, expected subscription.PeriodSnapshot,
	periodStart, periodEnd, nextStart, nextEnd, now time.Time, builtAgainst *time.Time,
	inv domain.Invoice, lines []domain.InvoiceLineItem, belowZero netNegativeUsage) (periodCloseOutcome, error) {
	var out periodCloseOutcome
	err := e.subs.WithTenantTx(ctx, sub.TenantID, func(tx *sql.Tx) error {
		if shouldFireScheduledCancel(sub, periodEnd, now) {
			canceled, err := e.subs.FireScheduledCancellationTx(ctx, tx, sub.TenantID, sub.ID, expected, now)
			if err != nil {
				return err
			}
			out.canceled = &canceled
		} else if err := e.subs.ClosePeriodTx(ctx, tx, sub.TenantID, sub.ID, expected, nextStart, nextEnd, nextEnd, sub.BillingAnchorDay); err != nil {
			return err
		}

		// FOR KEY SHARE on the threshold invoice until this tx ends: a void that
		// arrives after this read waits for the commit and lands after the
		// period's invoice (the window is forgiven); one that holds the row
		// now is waited for and seen.
		var current *time.Time
		wm, err := e.invoices.GetLatestThresholdInvoiceForCycleTx(ctx, tx, sub.TenantID, sub.ID, periodStart, periodEnd)
		switch {
		case err == nil:
			end := wm.BillingPeriodEnd
			current = &end
		case !errors.Is(err, errs.ErrNotFound):
			return fmt.Errorf("re-read threshold watermark: %w", err)
		}
		if !sameInstant(current, builtAgainst) {
			return subscription.ErrWatermarkMoved
		}

		if lines == nil {
			return e.noteNetNegativeTx(ctx, tx, sub, belowZero, domain.BillingReasonSubscriptionCycle, periodStart, periodEnd, domain.Invoice{})
		}
		num, err := e.settings.NextInvoiceNumberTx(ctx, tx, sub.TenantID)
		if err != nil {
			return fmt.Errorf("allocate invoice number: %w", err)
		}
		inv.InvoiceNumber = num
		created, err := e.invoices.CreateInvoiceWithLineItemsTx(ctx, tx, sub.TenantID, inv, lines)
		if err != nil {
			return err
		}
		// The finalize audit row rides the closer tx (ADR-090 shared fate): a
		// finalized invoice cannot exist without it.
		if err := e.emitFinalizeAuditTx(ctx, tx, created); err != nil {
			return err
		}
		if err := e.noteNetNegativeTx(ctx, tx, sub, belowZero, domain.BillingReasonSubscriptionCycle, periodStart, periodEnd, created); err != nil {
			return err
		}
		out.inv = created
		return nil
	})
	if errors.Is(err, subscription.ErrWatermarkMoved) {
		slog.InfoContext(ctx, "billing period moved under this close — nothing written; re-reading",
			"subscription_id", sub.ID, "tenant_id", sub.TenantID,
			"period_start", periodStart, "period_end", periodEnd)
		return periodCloseOutcome{}, errPeriodMoved
	}
	return out, err
}

// sameInstant compares two optional instants (nil == nil; otherwise Equal).
func sameInstant(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

// afterScheduledCancel runs the post-commit legs of the terminal close: the
// operator log line and the AuditActionCancel row. Operator-side cancel +
// portal-side cancel both write AuditActionCancel; this is the third path
// (auto-fire at period end) that previously skipped audit, leaving the
// subscription Activity timeline showing "Cancellation scheduled" with no
// terminal event. The canceled_by='schedule' field matches the outbound-
// webhook shape so a CS rep reading both surfaces sees the same vocabulary.
// subscription.canceled (canceled_by=schedule) itself was enqueued IN the
// closer tx by FireScheduledCancellationTx.
func (e *Engine) afterScheduledCancel(ctx context.Context, sub, canceled domain.Subscription, now time.Time) {
	slog.Info("scheduled cancel fired",
		"subscription_id", sub.ID,
		"tenant_id", sub.TenantID,
		"canceled_at", now.UTC(),
	)
	if e.auditLogger != nil {
		_ = e.auditLogger.Log(ctx, sub.TenantID, domain.AuditActionCancel, "subscription", canceled.ID, canceled.Code, map[string]any{
			"canceled_by": "schedule",
			"customer_id": canceled.CustomerID,
		})
	}
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

	// Every invoice with priced lines passes here, so this is where its
	// amounts are held to int64 cents: callers sum their lines unchecked,
	// and when the lines' sizes sum inside int64, no sum of them wrapped.
	if err := checkLineAmountsFit(lineItems); err != nil {
		return TaxApplication{}, err
	}
	subtotal = commitInvoiceSubtotal(lineItems, subtotal)
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
		return TaxApplication{}, refuse(refusalTaxProvider, fmt.Errorf("resolve tax provider: %w", err))
	}
	app.TaxProvider = provider.Name()
	commitReason := commitLineTaxReason(provider.Name())

	var profile domain.CustomerBillingProfile
	if e.profiles != nil && customerID != "" {
		bp, err := e.profiles.GetBillingProfile(ctx, tenantID, customerID)
		switch {
		case err == nil:
			profile = bp
			app.TaxCountry = bp.Country
			app.TaxID = bp.TaxID
		case errors.Is(err, errs.ErrNotFound):
			// No billing profile: nothing to apply (not set).
		default:
			// A read error used to leave an empty profile, and tax was then
			// computed without the customer's exemption, reverse-charge
			// status, address and tax ID — and committed that way.
			return TaxApplication{}, fmt.Errorf("get billing profile for tax: %w", err)
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

	// An invoice that only sells prepaid credit has nothing to tax (the
	// purchase is an advance payment; ADR-078 amendment 2026-10-19). Every
	// other line reaches the request, so an empty request over a non-empty
	// invoice means every line is a commit line. The provider is not asked:
	// there is nothing to calculate, so a provider outage cannot hold the
	// invoice as pending, and a customer's exemption or reverse-charge status
	// is not printed on a sale that is untaxed for another reason.
	if len(reqLines) == 0 && len(lineItems) > 0 {
		for i := range lineItems {
			if lineItems[i].IsCommitLine() {
				stampUntaxedCommitLine(&lineItems[i], commitReason)
			}
		}
		return app, nil
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
			if lineItems[i].IsCommitLine() {
				stampUntaxedCommitLine(&lineItems[i], commitReason)
				continue
			}
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
	// always has. A commit line was never sent: it is stamped untaxed, and
	// its price counts toward the net subtotal as it stands.
	expanded := expandTaxResultLines(res.Lines, lineGroups, lineItems)
	var netSubtotalSum int64
	for i := range lineItems {
		if lineItems[i].IsCommitLine() {
			stampUntaxedCommitLine(&lineItems[i], commitReason)
			netSubtotalSum += lineItems[i].AmountCents
			continue
		}
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
		total, ok := addCentsSigned(net, rl.TaxAmountCents)
		if !ok {
			return TaxApplication{}, fmt.Errorf("line %d total with tax: %w", i, domain.ErrAmountOverflow)
		}
		lineItems[i].TotalAmountCents = total
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
	// The invoice total (subtotal − discount + tax) must fit too; callers
	// work it out unchecked from these three.
	if net, ok := addCentsSigned(app.SubtotalCents, -app.DiscountCents); !ok {
		return TaxApplication{}, fmt.Errorf("invoice total: %w", domain.ErrAmountOverflow)
	} else if _, ok := addCentsSigned(net, app.TaxAmountCents); !ok {
		return TaxApplication{}, fmt.Errorf("invoice total with tax: %w", domain.ErrAmountOverflow)
	}

	if e.taxCalcStore != nil {
		if _, err := e.taxCalcStore.Record(ctx, tenantID, "", req, res); err != nil {
			// Required when there is a provider calculation to commit: the
			// finalize expiry guard reads this row's age, and without it the
			// guard finds "no row" forever and an aged calculation finalizes
			// uncommittable. Manual/none results carry no calculation id and
			// nothing reads their row, so for them it stays audit-only.
			if res.CalculationID != "" {
				return TaxApplication{}, fmt.Errorf("persist tax calculation %s: %w", res.CalculationID, err)
			}
			slog.Warn("tax: failed to persist tax_calculations row",
				"error", err, "tenant_id", tenantID, "provider", provider.Name())
		}
	}

	return app, nil
}

// errStillDue marks a subscription that was billed without error in this run
// and came back due on a later page. billSubscription returns cleanly while the
// sub is still due only when a concurrent change raced it: another closer won
// its period twice in a row (errPeriodMoved), or a cancel or trial change was
// rescheduled while it fired. The drain does not bill a sub twice in one run,
// so it reports the sub.
//
// It is saved as a counted failure (ADR-119), so the retry comes after the
// first wait, not on the next run. A real race costs that one 15 minute wait,
// which the next clean bill voids. A sub that makes no progress on every
// attempt parks, where it used to loop with only this log line.
var errStillDue = refuse(refusalNoProgress, errors.New("still due after a clean billing attempt: the subscription changed concurrently or made no progress"))

// dueFetch returns up to limit due subscriptions, oldest first, leaving out
// the ids in exclude.
type dueFetch func(ctx context.Context, exclude []string, limit int) ([]domain.Subscription, error)

// drainDue bills page after page of due subscriptions until a page comes back
// empty. All three billing runs use it: the scheduler tick, the operator's
// POST /v1/billing/run, and test-clock Advance.
//
// Each sub is attempted at most once per call. A sub that is still due after
// its attempt (it failed, or errStillDue) goes into exclude, and the next page
// skips it. Without that, a persistently failing sub stays first in the
// oldest-first order and comes back on every page. A page made only of such
// subs used to end the run while healthy subs waited behind them.
//
// A failed attempt on a wall-clock sub is also saved on the subscription
// (attemptSub, ADR-119), and the wall-clock scans then skip it until its next
// attempt is due, or for good once it is parked. The exclude list stays: it
// covers the failures that are not saved (an interrupted run, a transient
// database error, a failed save) and every clock-pinned sub.
//
// Termination: every sub on a non-empty page is either attempted for the first
// time or added to exclude, so each page shrinks what the next page can return.
//
// onPage, if set, runs after every page. The scheduler stamps its liveness
// there, because one tick can now run for hours.
//
// grace is the invoice grace this run honours (ADR-120): the wall-clock runs
// pass the engine's, the test-clock run passes 0. It goes to
// billSubscription, whose checks must use the same instant as fetch's scan.
//
// A sub whose attempt panics is handled like one that failed: attemptSub has
// saved the failure, and the sub goes into exclude and failures. The drain
// bills the rest, and the first panic is raised again when it returns, so the
// caller still sees it (the scheduler's tick ends as panicked; the operator's
// run answers 500; the test-clock catch-up marks the clock failed). Without
// this, one panicking sub stopped every sub after it in the run.
func (e *Engine) drainDue(ctx context.Context, batchSize int, fetch dueFetch, onPage func(), grace time.Duration, logArgs ...any) (int, []SubBillError) {
	if batchSize <= 0 {
		batchSize = 50
	}
	var panics scheduler.Panics
	generated := 0
	var failures []SubBillError
	attempted := make(map[string]bool)
	var exclude []string
	for {
		if err := ctx.Err(); err != nil {
			failures = append(failures, SubBillError{Err: fmt.Errorf("billing run ctx done: %w", err)})
			panics.Repanic()
			return generated, failures
		}
		due, err := fetch(ctx, exclude, batchSize)
		if err != nil {
			failures = append(failures, SubBillError{Err: fmt.Errorf("fetch due subscriptions: %w", err)})
			panics.Repanic()
			return generated, failures
		}
		if len(due) == 0 {
			panics.Repanic()
			return generated, failures
		}
		for _, sub := range due {
			// Shutdown or a lost lease mid-page: stop here. Billing the rest
			// would only fail each sub with the same ctx error.
			if err := ctx.Err(); err != nil {
				failures = append(failures, SubBillError{Err: fmt.Errorf("billing run ctx done: %w", err)})
				panics.Repanic()
				return generated, failures
			}
			if attempted[sub.ID] {
				exclude = append(exclude, sub.ID)
				// The page's row is the freshest read of a sub that was
				// attempted cleanly and is still due.
				e.reportBillFailure(ctx, sub, 0, errStillDue, logArgs...)
				failures = append(failures, SubBillError{SubscriptionID: sub.ID, Err: errStillDue})
				continue
			}
			attempted[sub.ID] = true
			// errBillPanic stays when attemptSub panics: the assignment
			// below never happens.
			n, err := 0, errBillPanic
			func() {
				defer panics.Guard(append([]any{"subscription_id", sub.ID, "tenant_id", sub.TenantID}, logArgs...)...)
				n, err = e.attemptSub(ctx, sub, grace, logArgs...)
			}()
			generated += n
			if errors.Is(err, errBillingGated) {
				// Not attempted, so not a failure and never "still due" in
				// this run. If its record is cleared while this run is still
				// draining, the next run attempts it.
				exclude = append(exclude, sub.ID)
				continue
			}
			if err != nil {
				exclude = append(exclude, sub.ID)
				failures = append(failures, SubBillError{SubscriptionID: sub.ID, Err: err})
			}
		}
		if onPage != nil {
			onPage()
		}
	}
}

// subBillErrors flattens SubBillErrors for the []error callers.
func subBillErrors(failures []SubBillError) []error {
	if len(failures) == 0 {
		return nil
	}
	out := make([]error, len(failures))
	for i, f := range failures {
		out[i] = f
	}
	return out
}

// RunCycleForClock bills the due subs attached to ONE test clock, catching
// each up to the clock's frozen time. Called by the test-clock catchup worker
// after MarkAdvancing — operator-driven (Advance click), never the wall-clock
// cron. ADR-028 disjoint flows: this is the ONLY path for clock-pinned
// billing; the wall-clock RunCycle excludes clock-pinned subs.
//
// Per-sub errors are returned, not fatal: one failing sub doesn't stall the
// others. When any sub fails, the worker marks the clock internal_failure and
// the operator's "Retry advance" resumes once the cause is fixed.
func (e *Engine) RunCycleForClock(ctx context.Context, tenantID, clockID string, batchSize int) (int, []error) {
	ctx, span := telemetry.Tracer("billing").Start(ctx, "billing.RunCycleForClock",
		trace.WithAttributes(
			attribute.String("clock_id", clockID),
			attribute.String("tenant_id", tenantID),
			attribute.Int("batch_size", batchSize),
		),
	)
	defer span.End()

	fetch := func(ctx context.Context, exclude []string, limit int) ([]domain.Subscription, error) {
		return e.subs.GetDueBillingForClock(ctx, tenantID, clockID, exclude, limit)
	}
	// No invoice grace on a test clock (ADR-120): advancing to a period end
	// produces its invoice, and the operator decides when usage arrives.
	generated, failures := e.drainDue(ctx, batchSize, fetch, nil, 0, "run", "clock_catchup", "clock_id", clockID)

	span.SetAttributes(attribute.Int("generated", generated))
	slog.Info("clock catchup cycle complete",
		"clock_id", clockID,
		"generated", generated,
		"errors", len(failures),
	)
	return generated, subBillErrors(failures)
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
// observe another tenant.
//
// It follows the same rule as the scheduler's run for a failing sub (ADR-119):
// a sub that is waiting for its next attempt or is parked is skipped, and a
// failed attempt is saved and counted. So a scripted run cannot burn a sub's
// attempts, and it is not the way to retry one sub: that is
// POST /v1/subscriptions/{id}/retry-billing.
//
// It also waits for the invoice grace like the scheduler's run (ADR-120). It
// is a due scan; if it did not wait, one call would close every period
// inside its grace and the usage still arriving would miss its invoice.
func (e *Engine) RunCycleForTenant(ctx context.Context, tenantID string, batchSize int) (int, []SubBillError) {
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
	dueBy := now.Add(-e.invoiceGrace)
	fetch := func(ctx context.Context, exclude []string, limit int) ([]domain.Subscription, error) {
		return e.subs.GetDueBillingForTenant(ctx, tenantID, now, dueBy, exclude, limit)
	}
	generated, failures := e.drainDue(ctx, batchSize, fetch, nil, e.invoiceGrace, "run", "manual")

	span.SetAttributes(attribute.Int("generated", generated))
	return generated, failures
}

// RunCycle is the scheduler's billing run: it bills every wall-clock sub that
// is due, across all tenants in ctx's livemode, before returning. Before P27 it
// billed one page of batchSize subs per tick, which at 50 subs per hourly tick
// meant 20,000 subs due on the 1st took about 17 days to invoice.
//
// "Due" leaves out a sub whose billing is failing and is either waiting for
// its next attempt or parked (ADR-119). Those are counted by
// velox_billing_failing_subscriptions, not attempted here.
//
// "Due" also waits for the invoice grace (ADR-120): a period is closed once
// INVOICE_GRACE_PERIOD has passed after its end (or after a mid-period
// cancel_at), so usage that arrives in that time is on its invoice. The
// billing-failure wait above keeps the real now.
//
// onPage runs after each page; the scheduler passes its liveness stamp.
// Returns the invoices generated and the per-sub errors.
func (e *Engine) RunCycle(ctx context.Context, batchSize int, onPage func()) (int, []error) {
	ctx, span := telemetry.Tracer("billing").Start(ctx, "billing.RunCycle",
		trace.WithAttributes(attribute.Int("batch_size", batchSize)),
	)
	defer span.End()

	// Same one-instant anchor as RunCycleForTenant: a sub due a few minutes
	// into a long drain waits for the next tick instead of extending this one.
	now := e.clock.Now(ctx)
	dueBy := now.Add(-e.invoiceGrace)
	fetch := func(ctx context.Context, exclude []string, limit int) ([]domain.Subscription, error) {
		return e.subs.GetDueBilling(ctx, now, dueBy, exclude, limit)
	}
	generated, failures := e.drainDue(ctx, batchSize, fetch, onPage, e.invoiceGrace, "run", "scheduled")

	span.SetAttributes(attribute.Int("generated", generated))
	if len(failures) > 0 {
		span.SetStatus(codes.Error, "subscriptions failed to bill")
	}
	if generated > 0 || len(failures) > 0 {
		slog.Info("billing cycle complete", "generated", generated, "errors", len(failures))
	}
	return generated, subBillErrors(failures)
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
// progress, when non-nil, is kept current as the loop runs: the row of each
// loop-top read and the invoices generated so far. The caller (attemptSub)
// then knows which row a failing step planned from, and how much was already
// billed, even when this returns an error or panics.
//
// grace is the invoice grace of the run (ADR-120; 0 for the operator's
// one-subscription close and for test clocks). A due instant (next_billing_at,
// or a mid-period cancel_at) counts only once grace has passed after it, so a
// catch-up after downtime bills the older periods at once and the newest
// after its grace. The due scans admit with the same rule, both arms.
//
// ADR-028.
func (e *Engine) billSubscription(ctx context.Context, sub domain.Subscription, grace time.Duration, progress *billProgress) (int, error) {
	count := 0
	movedRetry := false
	for i := 0; i < maxPeriodsPerSubPerCall; i++ {
		// Honour ctx deadline (CatchupTimeout from the worker, request
		// timeout from the scheduler). Returns the partial count + err.
		if err := ctx.Err(); err != nil {
			return count, fmt.Errorf("billing loop ctx done: %w", err)
		}

		// Refresh sub state. Required because billOnePeriod commits each
		// period close in its own tx, and we want the next iteration to
		// observe the post-advance fields — and, after errPeriodMoved, the
		// period another closer committed.
		fresh, err := e.subs.Get(ctx, sub.TenantID, sub.ID)
		if err != nil {
			return count, fmt.Errorf("refresh subscription %s: %w", sub.ID, err)
		}
		sub = fresh
		if progress != nil {
			progress.seen = fresh
		}

		// Billing-failure gate, re-checked at attempt time (ADR-119). The due
		// scans already skip a waiting or parked sub, but this row may come
		// from a page fetched before another run recorded a failure. Without
		// the re-check that stale page would attempt past the wait. The
		// sentinel tells the caller no attempt was made; count is kept because
		// an earlier iteration of a catch-up may already have invoiced.
		// Never set on a clock-pinned sub.
		if bf := sub.BillingFailure; bf != nil && (bf.NextAttemptAt == nil || bf.NextAttemptAt.After(e.clock.Now(ctx))) {
			return count, errBillingGated
		}

		// Caught-up check. The clock the sub lives on (test or wall)
		// determines "now"; sub.NextBillingAt strictly after that
		// means there's nothing more to bill in this call. dueBy is that
		// now less the run's invoice grace (ADR-120). It decides only
		// whether a due instant has come; the close itself still runs at
		// the due instant (billOnePeriod anchors on next_billing_at, the
		// cancel on cancel_at), so no invoice date moves.
		dueBy := e.effectiveNow(ctx, sub).Add(-grace)

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
			sub.CancelAt != nil && !sub.CancelAt.After(dueBy) &&
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

		if sub.NextBillingAt == nil || sub.NextBillingAt.After(dueBy) {
			return count, nil
		}

		// Snapshot for the no-progress detector below.
		prevNextBilling := sub.NextBillingAt

		invoiced, err := e.billOnePeriod(ctx, sub)
		if errors.Is(err, errPeriodMoved) {
			// ADR-115: another closer committed between this close's read and
			// its write; nothing was written. Re-read once and re-plan — the
			// loop top's Get observes the new period (typically caught up).
			// A second miss leaves the sub to the next fetch: a sub two writers
			// keep fighting over is not this call's to win by spinning.
			if movedRetry {
				return count, nil
			}
			movedRetry = true
			continue
		}
		if err != nil {
			return count, err
		}
		if invoiced {
			count++
			if progress != nil {
				progress.invoiced = count
			}
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
	}
	return count, fmt.Errorf("subscription %s: per-sub safety cap %d hit — billOnePeriod did not converge", sub.ID, maxPeriodsPerSubPerCall)
}

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
		if pendingPlanDue(it, gate) {
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
			if pendingPlanDue(it, gate) {
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
					_ = e.auditLogger.Log(ctx, sub.TenantID, domain.AuditActionSubscriptionPendingChangeApplied, "subscription", sub.ID, sub.Code, map[string]any{
						"item_id":     was.ID,
						"old_plan_id": was.PlanID,
						"new_plan_id": newPlanByItem[was.ID],
					})
				}
			}

			// NOTIFICATION: one event per transitioned item, only on a successful
			// swap so a half-applied state doesn't lie to webhook consumers.
			// Each carries the subscription read right after the swap
			// (ADR-131): the new plan is the item's plan_id; the plan it left
			// is gone from it, so it rides beside it.
			if e.events != nil {
				swapped, err := e.subs.Get(ctx, sub.TenantID, sub.ID)
				if err != nil {
					slog.Error("subscription.pending_change.applied not sent: the subscription could not be read after the swap",
						"subscription_id", sub.ID, "tenant_id", sub.TenantID, "error", err)
				} else {
					for _, was := range dueItems {
						payload := domain.SubscriptionWebhookData(swapped, map[string]any{
							"item_id":     was.ID,
							"old_plan_id": was.PlanID,
							"applied_at":  now.UTC(),
						})
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
	}
	return sub, nil
}

// pendingPlanDue reports whether the item's scheduled plan change applies at
// the close of the period ending at gate. ApplyDuePendingItemPlansAtomic
// selects the same rows in SQL (pending_plan_effective_at <= gate).
func pendingPlanDue(it domain.SubscriptionItem, gate time.Time) bool {
	return it.PendingPlanID != "" && it.PendingPlanEffectiveAt != nil && !it.PendingPlanEffectiveAt.After(gate)
}

// withDuePlanChanges is sub with the plan changes due at gate applied in
// memory: the items the close at gate bills, without the write.
func withDuePlanChanges(sub domain.Subscription, gate time.Time) domain.Subscription {
	items := make([]domain.SubscriptionItem, len(sub.Items))
	for i, it := range sub.Items {
		if pendingPlanDue(it, gate) {
			it.PlanID = it.PendingPlanID
			it.PendingPlanID = ""
			it.PendingPlanEffectiveAt = nil
		}
		items[i] = it
	}
	sub.Items = items
	return sub
}

// invoiceCurrency resolves an invoice's currency in the ADR-100 order:
// the customer's billing profile, else planCurrency, else the tenant default
// (a dead branch since ADR-100), else "USD". A source that is NOT SET (no
// profile, an empty field) falls through to the next; a READ ERROR fails —
// guessing the next source could label the invoice in the wrong currency.
func (e *Engine) invoiceCurrency(ctx context.Context, sub domain.Subscription, planCurrency string) (string, error) {
	if e.profiles != nil {
		bp, err := e.profiles.GetBillingProfile(ctx, sub.TenantID, sub.CustomerID)
		switch {
		case err == nil && bp.Currency != "":
			return bp.Currency, nil
		case err != nil && !errors.Is(err, errs.ErrNotFound):
			return "", fmt.Errorf("read billing profile for invoice currency: %w", err)
		}
	}
	if planCurrency != "" {
		return planCurrency, nil
	}
	if e.settings != nil {
		ts, err := e.settings.Get(ctx, sub.TenantID)
		if err != nil {
			return "", fmt.Errorf("read tenant settings for invoice currency: %w", err)
		}
		if ts.DefaultCurrency != "" {
			return ts.DefaultCurrency, nil
		}
	}
	return "USD", nil
}

// resolveBillingInputs resolves each item's plan and the invoice currency
// for this period. Usage is read in buildLineItems, once the plans that
// only a mid-period segment carries are known (#1192).
func (e *Engine) resolveBillingInputs(ctx context.Context, sub domain.Subscription) (map[string]domain.Plan, string, error) {
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
			return nil, "", fmt.Errorf("get plan %s: %w", it.PlanID, err)
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
	invoiceCurrency, err := e.invoiceCurrency(ctx, sub, invoiceCurrency)
	if err != nil {
		return nil, "", err
	}
	return plans, invoiceCurrency, nil
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
			loc, err := e.tenantLocation(ctx, sub.TenantID)
			if err != nil {
				return sub, true, err
			}
			nextBilling := domain.NextBillingPeriodEnd(periodEnd, sub.BillingTime, domain.BillingMonthly, loc, sub.BillingAnchorDay)
			slog.Info("skipping billing (trial active)", "subscription_id", sub.ID)
			// ADR-115: the advance proves the trialing snapshot — EndTrial /
			// ExtendTrial change status or period, so a stale advance misses.
			err = e.subs.ClosePeriod(ctx, sub.TenantID, sub.ID, subscription.SnapshotOf(sub), periodEnd, nextBilling, nextBilling, sub.BillingAnchorDay)
			if errors.Is(err, subscription.ErrWatermarkMoved) {
				return sub, true, errPeriodMoved
			}
			return sub, true, err
		}
		// ADR-069: a due cancel schedule means CANCEL FREE — never
		// activate-and-bill. Pre-route on the snapshot; the activation
		// UPDATE's SQL guard is the atomic backstop for a schedule landing
		// after it.
		if trialCancelsFree(sub) {
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
			inv, ok, billErr := e.BillOnCreateTx(ctx, tx, activated, now)
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
					"action":       domain.SubAuditTrialEnded,
					"customer_id":  sub.CustomerID,
					"triggered_by": "schedule",
				}
				if sub.TrialEndAt != nil {
					meta["trial_end_at"] = sub.TrialEndAt.UTC().Format(time.RFC3339)
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
// rule (PriceSource.Price applies any CustomerPriceOverride on top), so this
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
// per-meter usage charges (priced by the one rater, rate.go),
// honoring thresholds already billed this cycle and scheduled-cancel
// terminal-cycle skips. Extracted whole from
// billOnePeriod (behavior-preserving) to separate the pricing
// computation from the billing-cycle orchestration; it remains large
// and is the next candidate for further decomposition. Usage left
// unpriced is added to unpriced, which the caller records once the close
// commits. Usage below zero bills 0 and is added to belowZero, which the
// caller writes on the closer tx (net_negative.go).
func (e *Engine) buildLineItems(ctx context.Context, sub domain.Subscription, now, periodStart, periodEnd time.Time, plans map[string]domain.Plan, invoiceCurrency string, billingLoc *time.Location, unpriced unpricedUsage, belowZero netNegativeUsage) ([]domain.InvoiceLineItem, int64, *time.Time, error) {
	if tapped(ctx) {
		return rateTap.buildLineItems(ctx, e, sub, now, periodStart, periodEnd, plans, invoiceCurrency, billingLoc, unpriced, belowZero)
	}
	var lineItems []domain.InvoiceLineItem
	subtotal := int64(0)

	// At cycle close, check whether the scheduled cancel will fire at
	// this boundary. If yes, in_advance base lines for the UPCOMING
	// period MUST be skipped — the sub is about to terminate, the
	// customer won't consume the upcoming period, and emitting the
	// line would overcharge by one full prepayment. The cycle-close
	// invoice still bills usage from the just-elapsed period
	// (in-arrears for usage is always correct) and any in_arrears
	// base for the just-elapsed period. commitPeriodClose then fires the
	// scheduled cancel in the same tx as this invoice (ADR-115).
	// (audit finding flagged during the 2026-05-18 cancel-flow walk
	// through; same bug class would re-appear if hard-pause is ever
	// re-added — paused subs at pause-activation should likewise
	// skip the next-period in_advance base line.)
	terminalCycleClose := shouldFireScheduledCancel(sub, periodEnd, now)

	// Threshold-fired invoice for THIS cycle: when fireThreshold ran
	// mid-cycle (the period keeps its anchor), that invoice already billed
	// (and typically charged) usage through its billing_period_end. It bills
	// no base fee: the close bills the period's base as if no fire happened
	// (a fire made before that change carried the full in_arrears base, and
	// wm.billedBase skips it here). Without this watermark the cycle close
	// re-billed the usage from periodStart — every threshold cycle
	// double-charged. Keyed on the invoice (ground truth), not the
	// mutable BillingThresholds config, so an operator removing
	// thresholds mid-cycle doesn't resurrect the double-bill.
	// errs.ErrNotFound = no threshold fire = zero behavior change; any
	// other error aborts this sub's cycle (billing blind here risks a
	// double charge — feedback_no_silent_fallbacks). The instant is
	// returned to the caller: commitPeriodClose re-reads it on the closer
	// tx and refuses to commit lines built against a different watermark
	// (ADR-115 — a fire landing between here and the commit).
	wm, err := e.loadThresholdWatermark(ctx, sub.TenantID, sub.ID, periodStart, periodEnd)
	if err != nil {
		return nil, 0, nil, err
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
		return nil, 0, nil, fmt.Errorf("resolve segments: %w", err)
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
				return nil, 0, nil, fmt.Errorf("get segment plan %s: %w", seg.planID, err)
			}
			plans[seg.planID] = pl
		}
	}
	// Every meter the invoice bills: the meters of the current items' plans
	// and of the plans hydrated above. Segment-aware usage needs every meter
	// that was active at ANY point during the period: a meter that existed
	// on plan_A (now swapped out or removed) bills its [seg.start, seg.end)
	// window.
	meters, err := e.planMeters(ctx, sub.TenantID, plans)
	if err != nil {
		return nil, 0, nil, err
	}

	// Base-fee lines: segment-aware for in_arrears, one upcoming-period
	// line for in_advance. closeBaseFeeLines is shared with the operator
	// preview (previewCycle) so the estimate and the invoice cannot drift.
	for _, bl := range closeBaseFeeLines(sub, segsByItem, plans, periodStart, periodEnd, invoiceCurrency, billingLoc, terminalCycleClose, wm.billedBase()) {
		lineItems = append(lineItems, bl.line)
		subtotal += bl.line.AmountCents
	}

	// Segment-aware usage billing (Orb shape): each meter is billed
	// once per disjoint [start, end) range during which it was
	// active on the sub. For subs with no mid-period changes, every
	// meter has exactly one full-period interval — output collapses
	// to the pre-segment-aware single-line behavior. For subs where
	// a mid-period plan change adds/removes meters, each meter
	// bills only for the time it was actually on the sub.

	// The windows each meter is billed over, from item segments and
	// removed-item segments (meterUsageIntervals; shared with every other
	// closer and the live credit read so they cannot drift).
	floors, err := e.loadUsageFloors(ctx, sub, periodStart, periodEnd)
	if err != nil {
		return nil, 0, nil, err
	}
	meterIntervals, meterIntervalsFull := meterUsageIntervals(sub, segsByItem, plans, periodStart, periodEnd, thresholdBilledThrough, floors)

	// Usage is read once per window (one read covers every meter for a
	// window; a pricing-rule meter reads its buckets per window) and priced
	// by the one rater (rate.go): the clamped pass, then after a threshold
	// invoice the full-window pass for the buckets it deferred, with one tier
	// ladder per bucket continuing from that invoice (P21, ADR-066 amendment).
	reads := newWindowUsage(e, sub, meters)
	usageLines, usageSubtotal, err := e.rateCloseUsage(ctx, sub, reads, rateSite{}, meterIntervals, meterIntervalsFull, wm, periodStart, invoiceCurrency, "", "cycle", unpriced, belowZero)
	if err != nil {
		return nil, 0, nil, err
	}
	lineItems = append(lineItems, usageLines...)
	subtotal += usageSubtotal
	return lineItems, subtotal, thresholdBilledThrough, nil
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
		return false, refuse(refusalNoPeriod, fmt.Errorf("subscription has no billing period set"))
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
	//   - The pause is read at the cycle close: the period ending May 1
	//     stays a draft when the pause resumes May 5, even when the
	//     close runs May 20.
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

	// The pause is cleared by the scheduled resumers, not here
	// (Scheduler.pauseResumer for wall-clock, the test-clock catchup's
	// resume phase for clock-pinned subs). They run after the billing
	// run and never clear a pause while a period ending before
	// resumes_at is still unclosed, so the row still holds the pause
	// this close must read; the finalization gate below reads it as of
	// `now`, the period's end.

	// Apply any scheduled plan changes due at this cycle boundary before
	// reading plans (see applyDueScheduledPlanChanges).
	sub, err := e.applyDueScheduledPlanChanges(ctx, sub, now)
	if err != nil {
		return false, err
	}

	if len(sub.Items) == 0 {
		return false, refuse(refusalNoItems, fmt.Errorf("subscription has no items to bill"))
	}

	periodStart := *sub.CurrentBillingPeriodStart
	periodEnd := *sub.CurrentBillingPeriodEnd

	// Trial lifecycle: advance-without-bill, cancel-free, or flip-to-active.
	sub, trialDone, err := e.handleTrialState(ctx, sub, now, periodStart, periodEnd)
	if trialDone {
		return false, err
	}

	// ADR-115: the snapshot every write below proves. `sub` here is the
	// RETURNING row of any trial flip — its period is the one this close
	// bills — so a threshold fire, plan swap, cancel or sibling close that
	// commits from here on makes this close's CAS miss and nothing written.
	expected := subscription.SnapshotOf(sub)

	// Gather pricing inputs for this period (plans, currency); buildLineItems
	// reads the usage.
	plans, invoiceCurrency, err := e.resolveBillingInputs(ctx, sub)
	if err != nil {
		return false, err
	}

	// The tenant zone is read once per close and used for every boundary,
	// proration denominator and the invoice's BillingTimezone, so one invoice
	// can never mix two zones (P24).
	billingLoc, err := e.tenantLocation(ctx, sub.TenantID)
	if err != nil {
		return false, err
	}

	// Build the invoice line items (base fees + usage) for this period.
	// thresholdBilledThrough is the threshold watermark the lines were built
	// against; commitPeriodClose re-reads it on the closer tx.
	unpriced := unpricedUsage{}
	belowZero := netNegativeUsage{}
	lineItems, subtotal, thresholdBilledThrough, err := e.buildLineItems(ctx, sub, now, periodStart, periodEnd, plans, invoiceCurrency, billingLoc, unpriced, belowZero)
	if err != nil {
		return false, err
	}

	// The period this close opens. Uses domain.NextBillingPeriodEnd (NOT the
	// legacy interval-only advanceBillingPeriod) so calendar-billing subs
	// whose anchor day drifted from a prior plan-interval change auto-re-align
	// to the next calendar boundary instead of carrying the drifted day
	// forward forever.
	nextPeriodStart := periodEnd
	nextPeriodEnd := domain.NextBillingPeriodEnd(periodEnd, sub.BillingTime, plans[sub.Items[0].PlanID].BillingInterval, billingLoc, sub.BillingAnchorDay)

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
	// Still advance the cycle (or fire the scheduled cancel) so the period
	// anchor moves forward — the absence of an invoice doesn't mean the
	// period didn't pass. No invoice number is consumed.
	if subtotal == 0 && len(lineItems) == 0 {
		out, err := e.commitPeriodClose(ctx, sub, expected, periodStart, periodEnd, nextPeriodStart, nextPeriodEnd, now, thresholdBilledThrough, domain.Invoice{}, nil, belowZero)
		if errors.Is(err, errPeriodMoved) {
			return false, err
		}
		if err != nil {
			return false, fmt.Errorf("advance billing cycle (no-op invoice): %w", err)
		}
		// The period closed with no invoice; any unpriced usage in it is
		// now billed nowhere, so it counts here, after the commit.
		unpriced.record(ctx, sub)
		if out.canceled != nil {
			e.afterScheduledCancel(ctx, sub, *out.canceled, now)
		}
		slog.Info("cycle close skipped — no billable lines",
			"subscription_id", sub.ID,
			"period_start", periodStart,
			"period_end", periodEnd,
		)
		return false, nil
	}

	// Create invoice — pull settings for payment terms + tax. The invoice
	// number is allocated INSIDE the closer tx, after the row CAS, as a
	// strictly monotonic per-tenant sequence (NextInvoiceNumberTx): a lost
	// race burns no number, and a rolled-back invoice leaves no gap. No
	// fallback: a collision-prone number is worse than a failed billing
	// attempt, since the attempt is retried, while a duplicate invoice number corrupts
	// accounting. `now` was resolved at the top of billSubscription via
	// effectiveNow — reuse it so invoice timestamps sit on the same timeline
	// as the rest of this call (matters for test-clock subs where wall-clock
	// ≠ frozen_time).
	if e.settings == nil {
		return false, fmt.Errorf("billing engine: settings reader is required for invoice numbering")
	}
	netDays, err := e.netPaymentTermDays(ctx, sub.TenantID)
	if err != nil {
		return false, err
	}

	// Coupons removed 2026-05-29 (Phase A1). Discount stays at zero;
	// AI-native discount intent flows through the credit ledger.
	var discountCents int64
	taxApp, err := e.ApplyTaxToLineItems(tax.WithCalculationScope(ctx, "cycle:"+sub.ID+":"+periodStart.Format(time.RFC3339)+":"+periodEnd.Format(time.RFC3339)), sub.TenantID, sub.CustomerID, invoiceCurrency, subtotal, discountCents, lineItems)
	if err != nil {
		// A tax failure must abort before invoice create/finalize and
		// before the cycle advances — otherwise a transient DB blip would
		// finalize a $0 invoice and burn the period. Returning the error
		// writes no invoice and does not advance the period; the attempt is
		// retried (ADR-119: after the saved wait when the failure is counted).
		return false, fmt.Errorf("apply tax: %w", err)
	}
	// In tax-inclusive mode the engine back-calculates net subtotal/discount
	// from the gross inputs; in exclusive mode these pass through unchanged,
	// so the caller always reads the authoritative values off the result.
	totalWithTax := taxApp.SubtotalCents - taxApp.DiscountCents + taxApp.TaxAmountCents
	dueAt := domain.AddDaysIn(now, netDays, billingLoc)

	// Single source of truth for the draft/finalized decision —
	// shared across all four invoice-emitting paths (cycle close,
	// BillOnCreate, BillFinalOnImmediateCancel, handleItemProration).
	// Encodes: tax pending → draft; collection paused at the period's
	// end → draft; otherwise finalized. See
	// domain.InvoiceFinalizationStatus.
	invStatus := domain.InvoiceFinalizationStatus(taxApp.TaxStatus, sub.PauseCollection, now)

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
			// will be set to (nextPeriodEnd, computed via
			// NextBillingPeriodEnd above). Diverging here would leave the
			// invoice header period and the sub's tracked period out of sync.
			invoicePeriodEnd = domain.NextBillingPeriodEnd(periodEnd, sub.BillingTime, plans[it.PlanID].BillingInterval, billingLoc, sub.BillingAnchorDay)
			break
		}
	}

	// ATOMIC (ADR-115): the period CAS, the invoice number, the invoice + all
	// line items and the finalize audit row commit in ONE transaction, or
	// none of them do. The unique index on (tenant_id, subscription_id,
	// billing_period_start, billing_period_end) stays as the schema-level
	// idempotency seam behind the CAS.
	invoiceRow := domain.Invoice{
		CustomerID:      sub.CustomerID,
		SubscriptionID:  sub.ID,
		BillingTimezone: billingLoc.String(),
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
	}
	out, err := e.commitPeriodClose(ctx, sub, expected, periodStart, periodEnd, nextPeriodStart, nextPeriodEnd, now, thresholdBilledThrough, invoiceRow, lineItems, belowZero)
	if errors.Is(err, errPeriodMoved) {
		return false, err
	}
	if err != nil {
		// Idempotency: if this invoice already exists (UNIQUE violation on the
		// per-subscription+period constraint), the store returns errs.ErrAlreadyExists.
		// Match on the sentinel, not err.Error() substrings — translated messages,
		// wrapped errors, or DB driver changes would silently break substring matches
		// and cause duplicate charges in multi-worker retries.
		//
		// Reachable only for a PRE-ADR-115 row: an invoice a crashed tick
		// committed while the sub stayed due (the old create-then-advance
		// shape). A racing closer never lands here now — its CAS misses
		// before the insert.
		if errors.Is(err, errs.ErrAlreadyExists) {
			slog.Info("invoice already exists for billing period (idempotent skip)",
				"subscription_id", sub.ID,
				"period_start", periodStart,
				"period_end", periodEnd,
			)
			// Two pieces of state can still be missing:
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
			// 2. The cycle advance. Re-drive it through the same closer (no
			//    invoice this time) and surface a failure LOUDLY, matching the
			//    happy path. A swallowed error here strands the watermark: the
			//    sub stays due, every subsequent tick re-hits this skip, and
			//    nothing ever alerts. No usage note: the lines built
			//    here are discarded, the existing invoice is the bill.
			healed, err := e.commitPeriodClose(ctx, sub, expected, periodStart, periodEnd, nextPeriodStart, nextPeriodEnd, now, thresholdBilledThrough, domain.Invoice{}, nil, nil)
			if errors.Is(err, errPeriodMoved) {
				return false, err
			}
			if err != nil {
				return false, fmt.Errorf("advance billing cycle (heal path): %w", err)
			}
			if healed.canceled != nil {
				e.afterScheduledCancel(ctx, sub, *healed.canceled, now)
			}
			return false, nil
		}
		return false, fmt.Errorf("create invoice: %w", err)
	}
	inv := out.inv
	// Counted after the commit: a close that lost its CAS or rolled back
	// counted nothing, and the retry that wins counts once.
	unpriced.record(ctx, sub)
	// The period is closed (or the sub terminated) — every leg from here is
	// post-commit and at-least-once.
	if out.canceled != nil {
		e.afterScheduledCancel(ctx, sub, *out.canceled, now)
	}

	// The finalize AUDIT row already committed inside the closer tx above
	// (emitFinalizeAuditTx → LogInTx, ADR-090 shared fate): a finalized invoice
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

	// Collect now: apply credits, settle if nothing is owed, else charge the
	// card (or email for one). The finalize write already queued the invoice,
	// so this is a nudge to the one collector; a crash here leaves it for the
	// next sweep. Anchored at the period instant so the credits live at the
	// boundary are the ones that apply. A draft (tax pending, collection
	// paused) is skipped by CollectInvoice and collected when it finalizes.
	e.CollectInvoice(ctx, sub.TenantID, inv.ID, &now)

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

	inv, lineItems, ok, err := e.buildOnCreateInvoice(ctx, sub, e.effectiveNow(ctx, sub), nil)
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

// nextInvoiceNumber allocates an invoice number on the caller's transaction
// when there is one. Inside a transaction the number must never come from the
// settings store's own connection: the tenant's sequence row may already be
// locked by this same transaction (a credit-note number taken earlier holds it
// to commit), and a second connection then waits on it until the request times
// out. A rollback also returns the number, so no gap is left.
func (e *Engine) nextInvoiceNumber(ctx context.Context, tx *sql.Tx, tenantID string) (string, error) {
	if tx != nil {
		return e.settings.NextInvoiceNumberTx(ctx, tx, tenantID)
	}
	return e.settings.NextInvoiceNumber(ctx, tenantID)
}

// BillOnCreateTx is the in-transaction variant of BillOnCreate: it builds and
// inserts the day-1 in_advance invoice on the caller's tx but does NOT finalize
// (audit / tax-commit / auto-charge are external and must run after the tx
// commits). The caller commits, then calls FinalizeOnCreateInvoice post-commit.
// Used by create, activate and the trial flip, so the day-1 invoice commits
// atomically with the subscription write. (The cross-interval plan swap uses
// BillOnPlanSwapTx, which also bills the old period's usage.)
// Returns ok=false (nil error) for the no-op cases (no in_advance items / zero
// subtotal) and for an idempotent-skip on the period-uniqueness constraint.
//
// startAt is the instant the billed period began for this subscription: the
// trial end on a trial flip, the request's now on create, activate and early
// trial end (and the swap instant for BillOnPlanSwapTx). The pause is read as it stood then, because a trial
// flip can run after resumes_at for a period that began while still paused.
func (e *Engine) BillOnCreateTx(ctx context.Context, tx *sql.Tx, sub domain.Subscription, startAt time.Time) (domain.Invoice, bool, error) {
	ctx, span := telemetry.Tracer("billing").Start(ctx, "billing.BillOnCreateTx",
		trace.WithAttributes(
			attribute.String("subscription_id", sub.ID),
			attribute.String("tenant_id", sub.TenantID),
		),
	)
	defer span.End()

	inv, lineItems, ok, err := e.buildOnCreateInvoice(ctx, sub, startAt, nil)
	if err != nil {
		return domain.Invoice{}, false, err
	}
	if !ok {
		return domain.Invoice{}, false, nil
	}

	invoiceNumber, err := e.nextInvoiceNumber(ctx, tx, sub.TenantID)
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

// BillOnPlanSwapTx writes, on the swap's tx, the invoice of an immediate
// in_advance cross-interval plan swap (ADR-056). The swap moves the period
// from [old start, old end) to [at, new end), so it is the closer of the old
// window [old start, at): nothing else bills that window's usage later. The
// invoice has the renewal shape (ADR-031): the new period's base, plus the old
// window's usage priced as an immediate cancel prices its partial window
// (buildCancelLineItems: from the threshold watermark, segment-aware, in_advance
// base skipped). One invoice, one charge.
//
// before is the pre-swap snapshot the caller proved under the subscription
// row lock (the period CAS and the item check); after is the prospective
// subscription on the new plan and period. The threshold watermark is read on
// tx, after that lock, so a reset=false fire that committed before the lock is
// seen and its window is not billed again; a later fire fails its own CAS.
//
// ok=false (nil error) when there is nothing to bill. afterCommit is never
// nil; the caller runs it once tx commits (it counts the usage left unpriced,
// which a rolled-back, retried swap must not count twice).
func (e *Engine) BillOnPlanSwapTx(ctx context.Context, tx *sql.Tx, before, after domain.Subscription, at time.Time) (domain.Invoice, bool, func(), error) {
	ctx, span := telemetry.Tracer("billing").Start(ctx, "billing.BillOnPlanSwapTx",
		trace.WithAttributes(
			attribute.String("subscription_id", before.ID),
			attribute.String("tenant_id", before.TenantID),
		),
	)
	defer span.End()

	unpriced := unpricedUsage{}
	afterCommit := func() { unpriced.record(ctx, before) }
	if before.CurrentBillingPeriodStart == nil || before.CurrentBillingPeriodEnd == nil {
		return domain.Invoice{}, false, afterCommit, fmt.Errorf("subscription has no billing period set")
	}
	ps, pe := *before.CurrentBillingPeriodStart, *before.CurrentBillingPeriodEnd

	belowZero := netNegativeUsage{}
	var carry []domain.InvoiceLineItem
	if at.After(ps) {
		wm, err := e.loadThresholdWatermarkTx(ctx, tx, before.TenantID, before.ID, ps, pe)
		if err != nil {
			return domain.Invoice{}, false, afterCommit, err
		}
		plans := make(map[string]domain.Plan, len(before.Items))
		for _, it := range before.Items {
			if _, ok := plans[it.PlanID]; ok {
				continue
			}
			pl, err := e.pricing.GetPlan(ctx, before.TenantID, it.PlanID)
			if err != nil {
				return domain.Invoice{}, false, afterCommit, fmt.Errorf("get plan %s: %w", it.PlanID, err)
			}
			plans[it.PlanID] = pl
		}
		if len(before.Items) > 0 {
			// Same currency order as the cancel; the swap refuses a currency
			// change (ADR-100), so it is the new invoice's currency too.
			cur, err := e.invoiceCurrency(ctx, before, plans[before.Items[0].PlanID].Currency)
			if err != nil {
				return domain.Invoice{}, false, afterCommit, err
			}
			loc, err := e.tenantLocation(ctx, before.TenantID)
			if err != nil {
				return domain.Invoice{}, false, afterCommit, err
			}
			carry, _, err = e.buildCancelLineItems(ctx, before, wm, plans, cur, ps, at, loc, unpriced, belowZero, swapUsageSuffix)
			if err != nil {
				return domain.Invoice{}, false, afterCommit, fmt.Errorf("plan swap: old period usage: %w", err)
			}
		}
	}
	noteBelowZero := func(inv domain.Invoice) error {
		return e.noteNetNegativeTx(ctx, tx, before, belowZero, domain.BillingReasonSubscriptionCreate, ps, at, inv)
	}

	inv, lineItems, ok, err := e.buildOnCreateInvoice(ctx, after, at, carry)
	if err != nil {
		return domain.Invoice{}, false, afterCommit, err
	}
	if !ok {
		if err := noteBelowZero(domain.Invoice{}); err != nil {
			return domain.Invoice{}, false, afterCommit, fmt.Errorf("usage below zero row (swap tx): %w", err)
		}
		return domain.Invoice{}, false, afterCommit, nil
	}
	invoiceNumber, err := e.nextInvoiceNumber(ctx, tx, after.TenantID)
	if err != nil {
		return domain.Invoice{}, false, afterCommit, fmt.Errorf("mint invoice number: %w", err)
	}
	inv.InvoiceNumber = invoiceNumber
	// No idempotent skip: a unique violation poisons the caller's tx (see
	// BillOnCreateTx), so it fails the swap loud.
	created, err := e.invoices.CreateInvoiceWithLineItemsTx(ctx, tx, after.TenantID, inv, lineItems)
	if err != nil {
		return domain.Invoice{}, false, afterCommit, fmt.Errorf("create plan-swap invoice (tx): %w", err)
	}
	if err := e.emitFinalizeAuditTx(ctx, tx, created); err != nil {
		return domain.Invoice{}, false, afterCommit, fmt.Errorf("finalize audit (swap tx): %w", err)
	}
	if err := noteBelowZero(created); err != nil {
		return domain.Invoice{}, false, afterCommit, fmt.Errorf("usage below zero row (swap tx): %w", err)
	}
	return created, true, afterCommit, nil
}

// buildOnCreateInvoice computes the day-1 in_advance invoice for sub: resolves
// plans, filters to in_advance items, builds prorated base-fee lines, and
// applies tax. It performs NO DB writes and mints NO invoice number — the
// caller persists (BillOnCreate via the store's own tx, BillOnCreateTx on a
// coordinator tx) and finalizes. Returns ok=false (nil error) for the no-op
// cases: inactive sub, no in_advance items, or zero subtotal. The returned
// invoice has no InvoiceNumber and carries IssuedAt == CreatedAt == startAt,
// the instant the billed period began (see BillOnCreateTx); the pause is read
// as it stood then, and DueAt counts the net terms from it.
//
// carry is lines another window owes that this invoice bills beside the base:
// the old period's usage on a plan swap (BillOnPlanSwapTx). They count toward
// the zero-subtotal check and are taxed with the base. nil for a plain day-1
// invoice.
func (e *Engine) buildOnCreateInvoice(ctx context.Context, sub domain.Subscription, startAt time.Time, carry []domain.InvoiceLineItem) (domain.Invoice, []domain.InvoiceLineItem, bool, error) {
	if sub.Status != domain.SubscriptionActive {
		return domain.Invoice{}, nil, false, nil
	}
	if sub.CurrentBillingPeriodStart == nil || sub.CurrentBillingPeriodEnd == nil {
		return domain.Invoice{}, nil, false, fmt.Errorf("subscription has no billing period set")
	}

	// The invoice is issued when its period began. Every caller passes that
	// instant: effective-now for create, activate, early trial end and the
	// plan swap; trial_end_at for the trial-expiry scans; the cycle close for
	// the engine's own trial flip. Reading the clock here instead dated a
	// day-1 invoice swept by a long advance at the advance's target, after
	// the cycle invoices it preceded (B12, P-3).
	now := startAt
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
	invoiceCurrency, err := e.invoiceCurrency(ctx, sub, invoiceCurrency)
	if err != nil {
		return domain.Invoice{}, nil, false, err
	}

	// Build base-fee line items for in_advance items, with mid-period
	// proration (identical math to billOnePeriod's base loop).
	// Settings resolve before any line or tax work: a read failure fails the
	// create with nothing written (no orphan tax_calculations row).
	billingLoc, err := e.tenantLocation(ctx, sub.TenantID)
	if err != nil {
		return domain.Invoice{}, nil, false, err
	}
	netDays, err := e.netPaymentTermDays(ctx, sub.TenantID)
	if err != nil {
		return domain.Invoice{}, nil, false, err
	}
	baseLines := onCreateBaseLines(sub, plans, advanceItems, periodStart, periodEnd, invoiceCurrency, billingLoc)
	lineItems := make([]domain.InvoiceLineItem, 0, len(baseLines)+len(carry))
	subtotal := int64(0)
	for _, bl := range baseLines {
		lineItems = append(lineItems, bl.line)
		subtotal += bl.line.AmountCents
	}
	for _, li := range carry {
		lineItems = append(lineItems, li)
		subtotal += li.AmountCents
	}

	if subtotal <= 0 {
		// All in_advance items had zero base fees. Nothing to bill;
		// don't emit a $0 invoice (matches cycle-path behavior for
		// zero-amount cycles).
		return domain.Invoice{}, nil, false, nil
	}

	// Apply tax.
	taxApp, err := e.ApplyTaxToLineItems(tax.WithCalculationScope(ctx, "create:"+sub.ID+":"+periodStart.Format(time.RFC3339)+":"+periodEnd.Format(time.RFC3339)), sub.TenantID, sub.CustomerID, invoiceCurrency, subtotal, 0, lineItems)
	if err != nil {
		return domain.Invoice{}, nil, false, fmt.Errorf("apply tax: %w", err)
	}

	dueAt := domain.AddDaysIn(now, netDays, billingLoc)

	totalWithTax := taxApp.SubtotalCents - taxApp.DiscountCents + taxApp.TaxAmountCents

	inv := domain.Invoice{
		TenantID:        sub.TenantID,
		CustomerID:      sub.CustomerID,
		SubscriptionID:  sub.ID,
		BillingTimezone: billingLoc.String(),
		// Tax-deferred + pause-collection gate (matches billOnePeriod).
		// Pre-fix this path hardcoded Finalized regardless of tax;
		// invoices with tax_status=pending finalized with
		// TaxAmountCents=0, lying about authoritative amounts.
		Status:        domain.InvoiceFinalizationStatus(taxApp.TaxStatus, sub.PauseCollection, startAt),
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

// onCreateBaseLines is the day-1 in_advance invoice's base-fee lines for
// [periodStart, periodEnd]: one line per in_advance item, prorated when the
// period is shorter than a full plan cycle (a calendar stub). Moved out of
// buildOnCreateInvoice unchanged so the operator preview of a trialing
// subscription (previewTrialEnd) shows the invoice the trial end issues with
// this one copy of the math.
func onCreateBaseLines(sub domain.Subscription, plans map[string]domain.Plan, advanceItems []domain.SubscriptionItem, periodStart, periodEnd time.Time, invoiceCurrency string, billingLoc *time.Location) []closeBaseLine {
	out := make([]closeBaseLine, 0, len(advanceItems))
	periodDays := roundDays(periodEnd.Sub(periodStart))
	for _, it := range advanceItems {
		plan := plans[it.PlanID]
		if plan.BaseAmountCents <= 0 {
			continue
		}
		baseFee := plan.BaseAmountCents * it.Quantity
		description := fmt.Sprintf("%s - base fee (qty %d)", plan.Name, it.Quantity)

		fullCycleDays := roundDays(advanceBillingPeriod(periodStart, plan.BillingInterval, billingLoc, sub.BillingAnchorDay).Sub(periodStart))
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
		out = append(out, closeBaseLine{plan: plan, line: domain.InvoiceLineItem{
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
		}})
	}
	return out
}

// FinalizeOnCreateInvoice runs the post-persist steps for a day-1 in_advance
// invoice: the engine audit row, the Stripe Tax commit, and auto-charge (or
// enroll-for-retry + no-PM notify). These are external / best-effort and MUST
// run AFTER the invoice is durably committed — never inside a DB tx. BillOnCreate
// calls it inline; the atomic cross-interval swap calls it post-commit.
func (e *Engine) FinalizeOnCreateInvoice(ctx context.Context, sub domain.Subscription, inv domain.Invoice) {
	// The finalize AUDIT row rode the invoice-create tx (ADR-090 shared fate):
	// the own-tx callers (BillOnCreate) emit it via finalizeAuditEmit, and the
	// coordinator-tx callers (BillOnCreateTx, the cross-interval swap through
	// BillOnPlanSwapTx, the cancel through BillFinalOnImmediateCancelTx) emit it
	// on the coordinator tx. So this post-commit path fires only the external,
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

	// Collect now (credits first, ADR-088). The invoice was queued in its
	// create tx, so a crash before this leaves it for the next sweep.
	e.CollectInvoice(ctx, sub.TenantID, inv.ID, nil)
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
//     relief for the unused portion is BillOnCancelDraftsTx's job).
//   - Usage: aggregated for [periodStart, canceled_at] across every
//     meter referenced by every item. Same shape as a normal cycle
//     invoice's usage section, just with a truncated period_end.
//   - billing_reason = subscription_cancel (distinguishes from the
//     normal subscription_cycle invoice and from subscription_create).
//
// No-op when:
//   - sub.Status != canceled (defensive — caller should have flipped it)
//   - no current period set / canceled_at missing
//   - canceled_at AT or BEFORE current_period_start (defensive)
//   - a never-activated trial (trials are free, ADR-069)
//   - computed subtotal rounds to zero (no in_arrears base AND no
//     usage events; matches BillOnCreate's $0-invoice-skip pattern)
//
// Refuses (InvalidState, code unbilled_period) when canceled_at is AT or
// AFTER current_period_end: the period ended and its close has not run.
// The cycle close skips a canceled subscription, so letting the cancel
// through would leave that period uninvoiced for good.
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
	return e.billFinalOnImmediateCancelImpl(ctx, nil, sub, unpricedUsage{})
}

// BillFinalOnImmediateCancelTx is the in-tx variant: it inserts the
// final-on-cancel invoice on the caller's coordinator tx (ADR-056) and leaves
// the external finalize (audit + tax commit + auto-charge) to the caller's
// post-commit FinalizeOnCreateInvoice. Used by CancelAtomicWithBill so a billing
// failure rolls the cancel back rather than orphaning a canceled sub with an
// uninvoiced partial period. Returns an empty invoice (nil error) for the no-op
// cases (nothing to bill on cancel).
//
// afterCommit counts the usage the cancel left unpriced (#981). It is never
// nil, and the caller runs it once its transaction commits. Counting here
// instead would count again on every retry of a cancel whose transaction
// rolled back after this returned: a refused relief is saved as a billing
// failure and retried on backoff (ADR-119).
func (e *Engine) BillFinalOnImmediateCancelTx(ctx context.Context, tx *sql.Tx, sub domain.Subscription) (inv domain.Invoice, afterCommit func(), err error) {
	unpriced := unpricedUsage{}
	inv, err = e.billFinalOnImmediateCancelImpl(ctx, tx, sub, unpriced)
	return inv, func() { unpriced.record(ctx, sub) }, err
}

// The usage-line suffixes of a partial window: an immediate cancel ended it,
// or a cross-interval plan swap did (BillOnPlanSwapTx).
const (
	cancelUsageSuffix = " - canceled mid-period"
	swapUsageSuffix   = " - before plan change"
)

// buildCancelLineItems computes the final invoice's line items and
// subtotal for an immediate mid-period cancel: segment-aware in_arrears
// base fees over the partial period [periodStart, canceledAt] (skipped
// entirely when a threshold fire already billed the unprorated base)
// plus segment-aware usage, honoring the threshold watermark's
// clamped/full dual-window protocol. Extracted whole from
// billFinalOnImmediateCancelImpl (behavior-preserving) to separate the
// pricing computation from the cancel-invoice orchestration; it is the
// cancel-path analog of buildLineItems and shares its segment model.
// Usage left unpriced is added to unpriced, and usage below zero to
// belowZero, as in buildLineItems. usageSuffix ends each usage line's
// description and says why the window ended early: cancelUsageSuffix or
// swapUsageSuffix.
func (e *Engine) buildCancelLineItems(ctx context.Context, sub domain.Subscription, wm thresholdWatermark, plans map[string]domain.Plan, invoiceCurrency string, periodStart, canceledAt time.Time, billingLoc *time.Location, unpriced unpricedUsage, belowZero netNegativeUsage, usageSuffix string) ([]domain.InvoiceLineItem, int64, error) {
	if tapped(ctx) {
		return rateTap.buildCancelLineItems(ctx, e, sub, wm, plans, invoiceCurrency, periodStart, canceledAt, billingLoc, unpriced, belowZero, usageSuffix)
	}
	// Build base lines: segment-aware in_arrears billing over the
	// partial period [periodStart, canceledAt]. in_advance items are
	// explicitly skipped — their base for the just-canceled period
	// was already paid at period start (BillOnCreate or prior cycle),
	// and the unused portion is relieved by BillOnCancelDraftsTx.
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

	// The in_arrears base is billed here, prorated to the cancel, whether or
	// not a threshold fired: a fire bills usage only. A fire made before that
	// change carried the full base (wm.billedBase), so it is skipped then —
	// the same skip billOnePeriod applies.
	if !wm.billedBase() {
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
				emitBaseSegmentLine(seg, segPlan, periodStart, periodDays, invoiceCurrency, billingLoc, sub.BillingTime, &lineItems, &subtotal)
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
				emitBaseSegmentLine(seg, segPlan, periodStart, periodDays, invoiceCurrency, billingLoc, sub.BillingTime, &lineItems, &subtotal)
			}
		}
	}

	// Build usage lines: aggregate every meter referenced by every
	// plan that was active at any point in [periodStart, canceledAt].
	// Same aggregation surface the cycle path uses; only the period
	// boundaries differ. Iterates the full `plans` map (current items'
	// plans + hydrated change-log plans) so segment-aware billing
	// picks up meters that existed on now-removed plans.
	meters, err := e.planMeters(ctx, sub.TenantID, plans)
	if err != nil {
		return nil, 0, fmt.Errorf("cancel: %w", err)
	}

	// Segment-aware usage billing over the partial period
	// [periodStart, canceledAt]: the period close's own window builder
	// (meterUsageIntervals), ended at the cancel instant, with the dual
	// clamped/full maps of the threshold watermark protocol (ADR-066 §4) and
	// the usage floors.
	floors, err := e.loadUsageFloors(ctx, sub, periodStart, canceledAt)
	if err != nil {
		return nil, 0, err
	}
	meterIntervals, meterIntervalsFull := meterUsageIntervals(sub, segsByItem, plans, periodStart, canceledAt, wm.billedThrough, floors)

	// The lines read usage through reads, once per window, and are priced by
	// the one rater, with the same passes and ladder as the period close.
	reads := newWindowUsage(e, sub, meters)
	usageLines, usageSubtotal, err := e.rateCloseUsage(ctx, sub, reads, rateSite{cancel: true}, meterIntervals, meterIntervalsFull, wm, periodStart, invoiceCurrency, usageSuffix, "cancel", unpriced, belowZero)
	if err != nil {
		return nil, 0, err
	}
	lineItems = append(lineItems, usageLines...)
	subtotal += usageSubtotal
	return lineItems, subtotal, nil
}

func (e *Engine) billFinalOnImmediateCancelImpl(ctx context.Context, tx *sql.Tx, sub domain.Subscription, unpriced unpricedUsage) (domain.Invoice, error) {
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

	// canceled_at at or before period_start is defensive: nothing of this
	// period ran, so there is nothing to bill.
	if !canceledAt.After(periodStart) {
		return domain.Invoice{}, nil
	}

	// Never-activated trial write-off (ADR-069): a sub canceled straight out
	// of 'trialing' (activated_at never stamped) emits NO final invoice —
	// trials are free. The reachable window is the post-trial LAG: the trial
	// elapsed but the activation scan hadn't flipped the row yet, so
	// canceled_at lands at or after trial_end, which is after period_start
	// (the first paid period starts at midnight of the trial-end day,
	// firstPeriodAfterTrial), and this function would otherwise bill
	// prorated base + usage for a
	// never-activated sub. Decided: write off — matches the trial-end
	// scheduled-cancel semantics ("you won't be charged") rather than
	// billing on scan lag the customer can't see. Subs created active carry
	// activated_at (or no trial fields) and are unaffected. It sits above the
	// period-end refusal so a trial stays a free cancel however far behind
	// its activation is.
	if sub.TrialEndAt != nil && sub.ActivatedAt == nil {
		slog.Info("immediate cancel of a never-activated trial — no final invoice (trials are free)",
			"subscription_id", sub.ID, "tenant_id", sub.TenantID)
		return domain.Invoice{}, nil
	}

	// canceled_at at or after period_end: the period ended and nobody closed
	// it. This function bills only [period_start, canceled_at) inside the
	// period, and no billing run bills a canceled subscription, so the cancel
	// is refused and rolls back. subscription.Service.Cancel closes ended
	// periods before it gets here; this stops the cancel when that close
	// could not run (billing failing, a deadline mid catch-up, a boundary
	// crossed in between). sub is the row the cancel's own UPDATE returned,
	// so nothing can move the period between this check and the commit.
	if !canceledAt.Before(periodEnd) {
		next := "retry billing for this subscription, then cancel again"
		if sub.TestClockID != "" {
			next = "finish or retry the clock advance, then cancel again"
		}
		return domain.Invoice{}, errs.InvalidState(
			"this subscription has a billing period that ended and is not invoiced yet; " + next).
			WithCode("unbilled_period")
	}

	// Threshold watermark (ADR-066 §4/§5): the immediate cancel is the
	// period's SECOND closer, and pre-fix it ignored the fire entirely — a
	// threshold fire followed by an immediate cancel re-billed
	// the pre-fire sum usage (already on the fire invoice, typically
	// already charged). Shared protocol with billOnePeriod: additive buckets
	// clamped to the residual window, deferred non-additive buckets billed
	// full-window exactly once. The in_arrears base is prorated to the cancel
	// as with no fire (a fire bills no base), except behind a fire made
	// before that change, which billed it in full.
	//
	// Read on the cancel's tx when there is one: after the cancel's UPDATE
	// locked the subscription, and holding the threshold invoice FOR KEY
	// SHARE until the cancel commits. A void of that invoice after this read then
	// lands after the final invoice and the window is forgiven; a void that
	// commits first is seen here, and the final invoice bills the window
	// (ADR-066 amendment 2026-10-23).
	wm, err := e.loadThresholdWatermarkTx(ctx, tx, sub.TenantID, sub.ID, periodStart, periodEnd)
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
	invoiceCurrency, err = e.invoiceCurrency(ctx, sub, invoiceCurrency)
	if err != nil {
		return domain.Invoice{}, err
	}

	// Compute the final invoice's line items over the partial cancel
	// period [periodStart, canceledAt]: segment-aware in_arrears base
	// (skipped when a threshold fire already billed it) plus usage.
	// Settings are read once, before lines and tax: one zone for the whole
	// invoice, and a read failure leaves no orphan tax_calculations row.
	netDays, err := e.netPaymentTermDays(ctx, sub.TenantID)
	if err != nil {
		return domain.Invoice{}, err
	}
	billingLoc, err := e.tenantLocation(ctx, sub.TenantID)
	if err != nil {
		return domain.Invoice{}, err
	}
	// The cancel's final invoice is the last one for this period, so usage
	// it leaves unpriced is billed nowhere. Without tx it is counted here
	// once the decision is durable: no invoice, or the invoice written. With
	// tx, the caller's transaction can still roll back, so it is left in
	// unpriced for the caller to count after its commit.
	belowZero := netNegativeUsage{}
	lineItems, subtotal, err := e.buildCancelLineItems(ctx, sub, wm, plans, invoiceCurrency, periodStart, canceledAt, billingLoc, unpriced, belowZero, cancelUsageSuffix)
	if err != nil {
		return domain.Invoice{}, err
	}
	// Usage below zero billed 0 (#1193). Its row rides the transaction that
	// makes this decision final: the caller's, the invoice's, or (no tx and
	// no invoice) one of its own.
	noteBelowZero := func(tx *sql.Tx, inv domain.Invoice) error {
		return e.noteNetNegativeTx(ctx, tx, sub, belowZero, domain.BillingReasonSubscriptionCancel, periodStart, canceledAt, inv)
	}

	if subtotal <= 0 {
		// Nothing to bill — no in_arrears base AND no usage. The
		// customer canceled before they consumed anything billable.
		// Skip the $0 invoice (matches BillOnCreate's behavior).
		if tx != nil {
			if err := noteBelowZero(tx, domain.Invoice{}); err != nil {
				return domain.Invoice{}, fmt.Errorf("usage below zero row (cancel tx): %w", err)
			}
			return domain.Invoice{}, nil
		}
		if len(belowZero) > 0 {
			if err := e.subs.WithTenantTx(ctx, sub.TenantID, func(tx *sql.Tx) error {
				return noteBelowZero(tx, domain.Invoice{})
			}); err != nil {
				return domain.Invoice{}, fmt.Errorf("usage below zero row (cancel): %w", err)
			}
		}
		unpriced.record(ctx, sub)
		return domain.Invoice{}, nil
	}

	// Apply tax.
	taxApp, err := e.ApplyTaxToLineItems(tax.WithCalculationScope(ctx, "cancel:"+sub.ID+":"+canceledAt.Format(time.RFC3339Nano)), sub.TenantID, sub.CustomerID, invoiceCurrency, subtotal, 0, lineItems)
	if err != nil {
		return domain.Invoice{}, fmt.Errorf("apply tax on cancel: %w", err)
	}
	now := e.effectiveNow(ctx, sub)
	dueAt := domain.AddDaysIn(now, netDays, billingLoc)
	totalWithTax := taxApp.SubtotalCents - taxApp.DiscountCents + taxApp.TaxAmountCents

	invoiceNumber, err := e.nextInvoiceNumber(ctx, tx, sub.TenantID)
	if err != nil {
		return domain.Invoice{}, fmt.Errorf("mint invoice number on cancel: %w", err)
	}

	invToCreate := domain.Invoice{
		TenantID:        sub.TenantID,
		CustomerID:      sub.CustomerID,
		SubscriptionID:  sub.ID,
		BillingTimezone: billingLoc.String(),
		InvoiceNumber:   invoiceNumber,
		// Tax-deferred + pause-collection gate (matches billOnePeriod).
		// Pre-fix this path hardcoded Finalized regardless of tax;
		// invoices with tax_status=pending finalized with
		// TaxAmountCents=0, lying about authoritative amounts.
		Status:        domain.InvoiceFinalizationStatus(taxApp.TaxStatus, sub.PauseCollection, canceledAt),
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
		// path's BillOnPlanSwapTx). The external finalize (webhook + tax commit +
		// auto-charge) runs post-commit via FinalizeOnCreateInvoice; the AUDIT row
		// rides THIS coordinator tx so it commits atomically with the cancel.
		created, err := e.invoices.CreateInvoiceWithLineItemsTx(ctx, tx, sub.TenantID, invToCreate, lineItems)
		if err != nil {
			return domain.Invoice{}, err
		}
		if err := e.emitFinalizeAuditTx(ctx, tx, created); err != nil {
			return domain.Invoice{}, fmt.Errorf("finalize audit (cancel tx): %w", err)
		}
		if err := noteBelowZero(tx, created); err != nil {
			return domain.Invoice{}, fmt.Errorf("usage below zero row (cancel tx): %w", err)
		}
		return created, nil
	}

	inv, err := e.invoices.CreateInvoiceWithLineItemsAudited(ctx, sub.TenantID, invToCreate, lineItems, func(tx *sql.Tx, out domain.Invoice) error {
		if err := e.emitFinalizeAuditTx(ctx, tx, out); err != nil {
			return err
		}
		return noteBelowZero(tx, out)
	})
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
	unpriced.record(ctx, sub)

	// The finalize AUDIT row already committed inside the create tx above; here
	// only the post-commit WEBHOOK fires.
	e.dispatchInvoiceFinalized(ctx, sub, inv)

	if inv.TaxProvider != "" && inv.TaxCalculationID != "" {
		if err := e.CommitTax(ctx, sub.TenantID, inv.ID, inv.TaxCalculationID); err != nil {
			slog.Warn("tax: commit failed after final-on-cancel invoice",
				"error", err, "tenant_id", sub.TenantID, "invoice_id", inv.ID)
		}
	}

	// Collect now (credits first, ADR-088); dunning takes over on a real
	// decline. The invoice was queued in its create tx.
	e.CollectInvoice(ctx, sub.TenantID, inv.ID, nil)

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

// cancelCreditInputs is the shared setup for the in_advance cancel-proration
// credit: the funding-period bounds, the net unused base, and the human
// description. Computed by prepareCancelCredit (prepareSwapCredit for a plan
// swap) and consumed by the in-tx relief trunk.
type cancelCreditInputs struct {
	PeriodStart, PeriodEnd, CancelAt time.Time
	TotalUnused                      int64
	Desc                             string
}

// unusedBaseForPeriod is the single source of the mid-period unused-base
// arithmetic shared by the cancel and plan-swap relief. Day-based math to
// avoid int64 overflow — nanosecond math overflows for ~$36+ base fees on
// a full-month proration. Denominator is the FULL plan-interval cycle (not
// the current period's length), since the customer paid
// baseFee × periodDays/fullCycleDays for a stub period; using periodDays
// as the denominator over-refunds whenever periodDays < fullCycleDays.
// Same pattern as emitBaseSegmentLine.
func (e *Engine) unusedBaseForPeriod(ctx context.Context, sub domain.Subscription, periodStart, periodEnd, at time.Time, billingLoc *time.Location) (int64, error) {
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
		fullCycleDays := roundDays(advanceBillingPeriod(periodStart, plan.BillingInterval, billingLoc, sub.BillingAnchorDay).Sub(periodStart))
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

// prepareCancelCredit runs the guards + day-based unused-base math for a
// mid-period cancel. ok=false means there is nothing to credit (not
// canceled, a cancel at/after period end, no in_advance items, or zero
// unused) — both cancel-credit paths no-op. Error only on a malformed sub
// (canceled with no canceled_at), a tenant-timezone lookup failure, or a
// plan lookup failure.
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

	// At or after period end there is no unused portion to relieve (and the
	// final-invoice step has already refused the cancel unless nothing was
	// owed). cancel_at == period_start is a REAL zero-
	// consumption case (ADR-097: a schedule landing between the loop's
	// refresh and the boundary's cycle advance cancels a fully-prepaid,
	// zero-used period) — relieve the whole period rather than keep it;
	// only strictly-before-period-start stays defensive-rejected.
	if !cancelAt.Before(periodEnd) || cancelAt.Before(periodStart) {
		return cancelCreditInputs{}, false, nil
	}

	// One zone for the refund amount and its description.
	loc, err := e.tenantLocation(ctx, sub.TenantID)
	if err != nil {
		return cancelCreditInputs{}, false, err
	}
	totalUnused, err := e.unusedBaseForPeriod(ctx, sub, periodStart, periodEnd, cancelAt, loc)
	if err != nil {
		return cancelCreditInputs{}, false, err
	}
	if totalUnused <= 0 {
		return cancelCreditInputs{}, false, nil
	}

	// Civil-day dates on the customer-facing credit-note / ledger description
	// render in the tenant billing timezone (ADR-077), not UTC — a raw UTC
	// render prints the prior calendar day for a positive-offset billing zone.
	desc := prorationRefundDesc("Cancel proration", sub.Code, "canceled", periodStart, periodEnd, cancelAt, loc)

	return cancelCreditInputs{
		PeriodStart: periodStart,
		PeriodEnd:   periodEnd,
		CancelAt:    cancelAt,
		TotalUnused: totalUnused,
		Desc:        desc,
	}, true, nil
}

// BillOnCancelDraftsTx writes the cancel-proration relief on the caller's
// cancel *sql.Tx (ADR-057, completed by ADR-118): every funding share, paid or
// unpaid, so the relief commits — or rolls back — with the cancel. Any error
// rolls the cancel back: never a canceled subscription whose relief was lost.
// The caller runs FinishRelief after commit.
//
// Engine-writes-on-the-subscription-store's-tx is the BillFinalOnImmediateCancelTx
// precedent (the final invoice already inserts on this same tx).
func (e *Engine) BillOnCancelDraftsTx(ctx context.Context, tx *sql.Tx, sub domain.Subscription) (domain.Relief, error) {
	in, ok, err := e.prepareCancelCredit(ctx, sub)
	if err != nil {
		return domain.Relief{}, err // malformed sub or plan lookup failure → roll the cancel back
	}
	if !ok {
		return domain.Relief{}, nil // nothing to credit
	}
	return e.reliefTx(ctx, tx, sub, in, "subscription_cancellation", "cancel proration")
}

// BillOnPlanSwapDraftsTx is the ATOMIC half of the cross-interval plan-swap
// refund — the same folded-write shape as BillOnCancelDraftsTx (deferred
// Bug B, closed 2026-07-05). Pre-fix the ENTIRE swap refund ran post-commit
// on the request ctx: a crash or disconnect
// between the swap commit and the refund permanently lost the customer's
// unused prepayment — the retry 400s on the same-plan guard, and no
// reconciler re-derives a missed swap refund. Now, when every funding
// invoice for the OLD period is PAID (the dominant case), the credit-note
// drafts commit ON the swap tx (issue_pending), post-commit Issue relays the
// external legs, and a never-issued draft is recovered by
// RetryPendingClawbackIssue — reason-agnostic, so swap drafts ride the same
// sweep as cancel drafts.
//
// Since ADR-118 every funding share — unpaid ones included — is written on
// the swap tx, and any error rolls the whole swap back (never a swapped sub
// with a silently-lost refund). sub is the PRE-swap snapshot; at is the swap
// instant.
func (e *Engine) BillOnPlanSwapDraftsTx(ctx context.Context, tx *sql.Tx, sub domain.Subscription, at time.Time) (domain.Relief, error) {
	in, ok, err := e.prepareSwapCredit(ctx, sub, at)
	if err != nil {
		return domain.Relief{}, err // plan lookup failure → roll the swap back
	}
	if !ok {
		return domain.Relief{}, nil // nothing to credit (in_arrears, boundary swap)
	}
	return e.reliefTx(ctx, tx, sub, in, "subscription_plan_change", "plan-swap refund")
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

	// One zone for the refund amount and its description.
	loc, err := e.tenantLocation(ctx, sub.TenantID)
	if err != nil {
		return cancelCreditInputs{}, false, err
	}
	totalUnused, err := e.unusedBaseForPeriod(ctx, sub, periodStart, periodEnd, at, loc)
	if err != nil {
		return cancelCreditInputs{}, false, err
	}
	if totalUnused <= 0 {
		return cancelCreditInputs{}, false, nil
	}

	desc := prorationRefundDesc("Plan-swap refund", sub.Code, "swapped", periodStart, periodEnd, at, loc)

	return cancelCreditInputs{
		PeriodStart: periodStart,
		PeriodEnd:   periodEnd,
		CancelAt:    at,
		TotalUnused: totalUnused,
		Desc:        desc,
	}, true, nil
}

// reliefTx is the shared in-tx trunk of BillOnCancelDraftsTx and
// BillOnPlanSwapDraftsTx: funding lookup → allocation → one write per share on
// the caller's tx. what labels log lines ("cancel proration" / "plan-swap
// refund").
//
// Every share is written here, paid or unpaid (ADR-118). Until then an UNPAID
// funding source sent the whole relief to a post-commit path whose failure was
// only logged — the cancel or swap stood and the customer's relief was lost,
// with no retry. Any error now rolls the change back; callers retry (dunning
// and the scheduled-cancel scan do it on their own).
func (e *Engine) reliefTx(ctx context.Context, tx *sql.Tx, sub domain.Subscription, in cancelCreditInputs, reason, what string) (domain.Relief, error) {
	sources, err := e.invoices.FindFundingInvoicesForPeriod(ctx, sub.TenantID, sub.ID, in.PeriodStart, in.PeriodEnd)
	if errors.Is(err, errs.ErrNotFound) {
		// No in_advance invoice funded this period (e.g. trial cancel) — nothing to relieve.
		slog.InfoContext(ctx, what+": no in_advance invoice for period; no relief to apply",
			"subscription_id", sub.ID, "customer_id", sub.CustomerID, "period_start", in.PeriodStart)
		return domain.Relief{}, nil
	}
	if err != nil {
		return domain.Relief{}, fmt.Errorf("%s: look up funding invoices: %w", what, err)
	}

	var r domain.Relief
	// An unpaid source whose whole window is unused (zero used days at the
	// relief math's day granularity — the change lands on its period start's
	// day), with nothing paid and no charge in flight, is
	// voided outright: the void relieves its full total — its amount due AND
	// the customer credits applied to it, which it hands back. It leaves the
	// allocation, so its share cannot also spill onto a paid source as balance
	// credit (that was a double relief whenever credits had been applied).
	//
	// Only an invoice that is nothing but that prepaid base is voided. An
	// in_advance renewal invoice also carries the period that just ended
	// (its usage, any in_arrears base); voiding it would unbill that period,
	// and nothing bills it again once the subscription is canceled. Such an
	// invoice takes the partly-used path below, which relieves the base share
	// and leaves the rest due.
	var allocate []domain.Invoice
	totalUnused := in.TotalUnused
	for _, src := range sources {
		voidable := src.PaymentStatus != domain.PaymentSucceeded && src.AmountPaidCents == 0 &&
			roundDays(in.CancelAt.Sub(src.BillingPeriodStart)) <= 0 &&
			!domain.PaymentBlocksAction(src, domain.ActionVoid).Blocked
		if voidable {
			voidable, err = e.onlyPrepaidBase(ctx, sub.TenantID, src, in.PeriodStart)
			if err != nil {
				return domain.Relief{}, fmt.Errorf("%s: read lines of %s: %w", what, src.ID, err)
			}
		}
		if voidable {
			voided, verr := e.invoiceVoider.VoidUnchangedTx(ctx, tx, sub.TenantID, src)
			if verr != nil {
				return domain.Relief{}, fmt.Errorf("%s: void fully-unused %s: %w", what, src.ID, verr)
			}
			r.Voided = append(r.Voided, voided)
			totalUnused = max(0, totalUnused-src.SubtotalCents)
			continue
		}
		allocate = append(allocate, src)
	}
	if len(allocate) == 0 || totalUnused <= 0 {
		return r, nil
	}

	// One crediting rule (ADR-118, amended 2026-10-22): the newest charging
	// invoice first, each capped at its room. The headroom read MUST stay off
	// this tx: it counts drafts, so an on-tx read would let this change's own
	// drafts shrink their room. CreateAdjustmentDraftTx re-checks the cap
	// under the invoice lock.
	credited := make(map[string]int64, len(allocate))
	for _, src := range allocate {
		c, cerr := e.creditHeadroom.CreditedCents(ctx, sub.TenantID, src.ID)
		if cerr != nil {
			return domain.Relief{}, fmt.Errorf("%s: read creditable headroom for %s: %w", what, src.ID, cerr)
		}
		credited[src.ID] = c
	}
	shares, unplaced := domain.AllocateUnusedCredit(allocate, credited, "", totalUnused)
	// What did not fit is not owed (every invoice is credited up to its total,
	// or relieved of all it asks for); the change records it.
	r.UncreditedCents = unplaced
	// A draft whose tax is not settled has no final total to size its note
	// against: refuse, so the change is retried once the tax retry resolves it
	// (normally minutes) rather than relieving a wrong amount.
	if src, pending := domain.TaxPendingShare(allocate, shares); pending {
		return domain.Relief{}, errs.InvalidState(fmt.Sprintf(
			"%s: funding invoice %s is a draft whose tax is still %s; retry once it is calculated", what, src.ID, src.TaxStatus)).
			WithCode("funding_invoice_tax_pending")
	}

	shareIDs := make([]string, 0, len(shares))
	for _, sh := range shares {
		shareIDs = append(shareIDs, sh.InvoiceID)
	}
	if err := e.creditNoteAdjuster.LockInvoicesForDraftsTx(ctx, tx, sub.TenantID, shareIDs); err != nil {
		return domain.Relief{}, fmt.Errorf("%s: lock funding invoices: %w", what, err)
	}
	for _, sh := range shares {
		// A credit-note cap rejection here (a concurrent operator credit note
		// shrank the source's headroom after the off-tx CreditedCents read)
		// rolls the change back too; the retry re-reads the smaller headroom.
		// A draft or in-flight source gets its note too: Issue holds it until
		// the invoice is finalized or the charge settles (and voids it if the
		// source is voided instead).
		cn, cerr := e.creditNoteAdjuster.CreateAdjustmentDraftTx(ctx, tx, sub.TenantID, sh.InvoiceID, sh.GrossCents, reason, in.Desc)
		if cerr != nil {
			return domain.Relief{}, fmt.Errorf("%s: credit-note draft on %s: %w", what, sh.InvoiceID, cerr)
		}
		r.DraftIDs = append(r.DraftIDs, cn.ID)
		if sh.ToBalance {
			r.CreditedCents += sh.GrossCents
		}
	}
	return r, nil
}

// onlyPrepaidBase reports whether every line of the invoice is base fee for
// the period starting at periodStart, or for a later slice of it. An invoice
// with no lines is not one.
//
// A mid-period proration invoice (plan change, quantity change, item add)
// writes its base-fee lines with no period of their own; the header says which
// slice they cover (billing_reason subscription_update, source_plan_changed_at).
// So a line with no period counts only on such an invoice whose change falls at
// or after periodStart.
func (e *Engine) onlyPrepaidBase(ctx context.Context, tenantID string, src domain.Invoice, periodStart time.Time) (bool, error) {
	lines, err := e.invoices.ListLineItems(ctx, tenantID, src.ID)
	if err != nil {
		return false, err
	}
	proration := src.BillingReason == domain.BillingReasonSubscriptionUpdate &&
		src.SourcePlanChangedAt != nil && !src.SourcePlanChangedAt.Before(periodStart)
	for _, li := range lines {
		if li.LineType != domain.LineTypeBaseFee {
			return false, nil
		}
		if li.BillingPeriodStart == nil {
			if !proration {
				return false, nil
			}
			continue
		}
		if li.BillingPeriodStart.Before(periodStart) {
			return false, nil
		}
	}
	return len(lines) > 0, nil
}

// FinishRelief runs a committed Relief's post-commit legs: issue the
// credit-note drafts (Stripe tax reversal + customer-balance grant, or the
// amount_due reduction) and each void's AfterVoid, the same legs an operator
// void runs (stop collection at Stripe, tax reversal, webhook, close the
// dunning run). Best-effort:
// every leg is already durable and recovered by its own sweep, so a failure
// here is logged, never returned.
func (e *Engine) FinishRelief(ctx context.Context, sub domain.Subscription, r domain.Relief) {
	for _, id := range r.DraftIDs {
		if _, err := e.creditNoteAdjuster.Issue(ctx, sub.TenantID, id); err != nil {
			// Issue() is atomic + recoverable (ADR-061): either the draft→issued
			// CAS never committed (still draft + issue_pending →
			// RetryPendingClawbackIssue re-drives) or a post-commit external leg
			// failed (refund → RetryRefund; tax reversal →
			// RetryPendingCreditNoteTaxReversal).
			slog.ErrorContext(ctx, "relief credit-note issue failed post-commit; self-heals via reconciler",
				"subscription_id", sub.ID, "credit_note_id", id, "error", err)
		}
	}
	for _, inv := range r.Voided {
		e.invoiceVoider.AfterVoid(ctx, sub.TenantID, inv)
	}
}

// invoicePage returns one page of a background invoice queue after the
// cursor, oldest first by (created_at, id).
type invoicePage func(ctx context.Context, after domain.InvoiceKeyset, limit int) ([]domain.Invoice, error)

// drainInvoiceQueue visits every row of a background invoice queue once:
// page after page by (created_at, id) until a short page. It replaced
// single-page sweeps. Those read the oldest pageSize rows and stopped, so rows
// that stay queued by design (a card-less invoice waits for a card; a
// no-policy invoice is skipped by dunning) filled the page for good, and
// nothing newer was ever reached.
//
// The cursor comes from the LISTED row, and the queue's order columns never
// change, so the drain ends: each full page moves the cursor forward. A row
// that joins behind the cursor mid-drain is visited on the next call.
//
// visit returns how many rows it completed; onPage, if set, runs after each
// page (the scheduler's liveness stamp: a drain can outlast the interval).
func drainInvoiceQueue(ctx context.Context, pageSize int, fetch invoicePage, visit func(context.Context, []domain.Invoice) (int, []error), onPage func()) (listed, done int, errs []error) {
	if pageSize <= 0 {
		pageSize = 50
	}
	var after domain.InvoiceKeyset
	for {
		if err := ctx.Err(); err != nil {
			return listed, done, append(errs, fmt.Errorf("queue drain stopped: %w", err))
		}
		page, err := fetch(ctx, after, pageSize)
		if err != nil {
			return listed, done, append(errs, err)
		}
		if len(page) == 0 {
			return listed, done, errs
		}
		n, verrs := visit(ctx, page)
		listed += len(page)
		done += n
		errs = append(errs, verrs...)
		after = domain.KeysetOf(page[len(page)-1])
		if onPage != nil {
			onPage()
		}
		if len(page) < pageSize {
			return listed, done, errs
		}
	}
}

// RetryPendingCharges visits every invoice queued for auto-charge, once per
// call, pageSize at a time, and tries to collect it. CRON path — the
// wall-clock scheduler calls this every tick, passing its liveness stamp as
// onPage. ADR-029 Phase 1: clock-pinned invoices are excluded from this query
// and are processed instead by RetryPendingChargesForClock during catchup.
func (e *Engine) RetryPendingCharges(ctx context.Context, pageSize int, onPage func()) (int, []error) {
	if e.charger == nil || e.paymentSetups == nil {
		return 0, nil
	}
	start := time.Now() //nolint:forbidigo // wall-clock: measures how long the sweep ran, for its log line; not a billing instant
	fetch := func(ctx context.Context, after domain.InvoiceKeyset, limit int) ([]domain.Invoice, error) {
		pending, err := e.invoices.ListAutoChargePending(ctx, after, limit)
		if err != nil {
			return nil, fmt.Errorf("list pending charges: %w", err)
		}
		return pending, nil
	}
	visit := func(ctx context.Context, page []domain.Invoice) (int, []error) {
		return e.processAutoCharge(ctx, page, nil, noPMTriggerSweep)
	}
	listed, charged, errs := drainInvoiceQueue(ctx, pageSize, fetch, visit, onPage)
	if listed > 0 {
		slog.Info("auto-charge sweep", "livemode", postgres.Livemode(ctx), "listed", listed,
			"charged", charged, "errors", len(errs), "duration_ms", time.Since(start).Milliseconds())
	}
	return charged, errs
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
	fetch := func(ctx context.Context, after domain.InvoiceKeyset, limit int) ([]domain.Invoice, error) {
		pending, err := e.invoices.ListAutoChargePendingForClock(ctx, tenantID, clockID, after, limit)
		if err != nil {
			return nil, fmt.Errorf("list pending charges for clock %s: %w", clockID, err)
		}
		return pending, nil
	}
	visit := func(ctx context.Context, page []domain.Invoice) (int, []error) {
		return e.processAutoCharge(ctx, page, nil, noPMTriggerSweep)
	}
	_, charged, errs := drainInvoiceQueue(ctx, limit, fetch, visit, nil)
	return charged, errs
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
// adapter's "dunning disabled" swallow.
func (e *Engine) EnrollFailedWithoutDunning(ctx context.Context, limit int) (int, []error) {
	// dunningStarter is REQUIRED (boot fails closed, #442) — the old nil
	// early-returns in the three enroll sweeps silently left card-less
	// invoices out of dunning forever.
	// Cool-off cutoff. The sweep is livemode-only (clock-pinned rows are excluded
	// by the store query), so a real-time cutoff against updated_at is correct and
	// never touches simulated time.
	olderThan := time.Now().UTC().Add(-failedDunningBackfillCoolOff) //nolint:forbidigo // wall-clock: reconciler is livemode-only; clock-pinned rows are excluded by the query
	pending, err := e.invoices.ListFailedWithoutDunningRun(ctx, olderThan, limit)
	if err != nil {
		return 0, []error{fmt.Errorf("list failed-without-dunning for backfill: %w", err)}
	}
	return e.enrollStalledForDunning(ctx, pending, domain.DunningCausePaymentFailed)
}

// enrollStalledForDunning is the shared body: enroll each candidate via
// the idempotent StartDunning (which anchors the first retry: now for a
// wall-clock invoice, the issue instant for a clock-pinned one). A "dunning
// disabled" outcome is swallowed by the adapter as a deliberate skip; other
// errors are collected per-invoice so one bad row doesn't abort the sweep.
func (e *Engine) enrollStalledForDunning(ctx context.Context, pending []domain.Invoice, cause domain.DunningStartCause) (int, []error) {
	swept := 0
	var errs []error
	for _, inv := range pending {
		if err := e.dunningStarter.StartDunning(ctx, inv.TenantID, inv.ID, inv.CustomerID, inv.IssuedAtOrZero(), cause); err != nil {
			errs = append(errs, fmt.Errorf("enroll invoice %s for dunning: %w", inv.ID, err))
			continue
		}
		swept++
	}
	return swept, errs
}

// processAutoCharge is the one collector for finalized invoices. The two
// sweeps (RetryPendingCharges, RetryPendingChargesForClock) and the post-commit
// CollectInvoice nudge all run it, so collection has a single implementation.
//
// at anchors the credit application and a credit-covered settle; nil means
// "the invoice's own clock now" (SimForInvoice), which is what the sweeps want.
// trigger labels the no-payment-method email (finalize_no_pm when a finalize
// site nudges, auto_charge_retry_no_pm from the sweeps).
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
func (e *Engine) processAutoCharge(ctx context.Context, pending []domain.Invoice, at *time.Time, trigger string) (int, []error) {
	charged := 0
	var errs []error
	for _, inv := range pending {
		// Shutdown or a lost lease: stop before claiming the next row. The
		// finalize-time nudge passes a detached ctx, so this only ends sweeps.
		// No error here: the drain reports the stop once, so one shutdown is
		// not counted as a failed retry per remaining row.
		if ctx.Err() != nil {
			return charged, errs
		}
		// Per-invoice charge lease (HA hazard #1): exactly one collector
		// enters the charge leg per invoice per 5m window. The CAS
		// re-asserts the full eligibility predicate, so a rival collector's
		// outcome (or a webhook settle) landing between list and claim
		// fails the claim here — BEFORE the credit-apply/refresh path
		// that could otherwise mint a divergent Stripe idempotency key.
		//
		// Refused also while a charge whose result was lost waits for the
		// stale-hold sweep to ask Stripe (ADR-127): charging it now could take
		// a second payment. The sweep runs before this one in the same tick.
		hold, cerr := e.invoices.ClaimAutoCharge(ctx, inv.TenantID, inv.ID)
		if cerr != nil {
			errs = append(errs, fmt.Errorf("claim auto-charge %s: %w", inv.ID, cerr))
			continue
		}
		if !hold.Held() {
			continue
		}
		// Every path below that sends no charge releases the hold (by its
		// token): left set, it would wait for the stale-hold sweep. The store
		// runs the release even after a shutdown ends ctx.
		release := func() {
			if err := e.invoices.ReleaseChargeHold(ctx, inv.TenantID, inv.ID, hold); err != nil {
				slog.Warn("auto-charge: releasing the charge hold failed — the invoice waits for the stale-hold sweep",
					"invoice_id", inv.ID, "error", err)
			}
		}
		// Work from the row as it is now, not the list snapshot: a rival
		// collector may have applied credits or sent the no-payment-method
		// email (and stamped no_pm_notified_at) between the list and the claim.
		fresh, err := e.invoices.GetInvoice(ctx, inv.TenantID, inv.ID)
		if err != nil {
			errs = append(errs, fmt.Errorf("reload invoice %s after claim: %w", inv.ID, err))
			release()
			continue
		}
		inv = fresh

		settleAt := e.collectionAt(ctx, inv, at)
		// Apply customer credits BEFORE charging (trap R1: never charge the
		// pre-credit amount). ApplyToInvoiceAt is safe to re-run: it drains
		// min(amount_due, current balance), so an already-applied invoice or
		// an empty balance applies nothing. Failure → skip this invoice (flag
		// stays set; the next sweep retries) rather than charge pre-credit.
		if e.credits != nil && inv.AmountDueCents > 0 {
			if _, err := e.credits.ApplyToInvoiceAt(ctx, inv.TenantID, inv.CustomerID, inv.ID, inv.AmountDueCents, settleAt, inv.InvoiceNumber); err != nil {
				slog.Warn("auto-charge: credit apply failed — skipping charge to avoid overcharging; will retry next tick",
					"invoice_id", inv.ID, "error", err)
				// Provably pre-Stripe: release the hold so the next
				// tick/Advance retries immediately.
				release()
				continue
			}
			// Refresh: credits may have reduced (or fully covered) amount_due.
			refreshed, err := e.invoices.GetInvoice(ctx, inv.TenantID, inv.ID)
			if err != nil {
				errs = append(errs, fmt.Errorf("refresh invoice %s after credit apply: %w", inv.ID, err))
				release() // provably pre-Stripe
				continue
			}
			inv = refreshed
		}
		if inv.AmountDueCents <= 0 {
			// Nothing owed: born $0, fully covered by credits, or reduced to
			// $0 by a credit note. Settle with no card charge (Stripe parity:
			// zero-amount invoices are marked paid with no payment attempt).
			// The claim admitted only a finalized, pending row, and MarkPaid
			// re-checks status and tax_status under FOR UPDATE, so a draft
			// can't be settled here. MarkPaid also clears the flag.
			// No charge is sent either way, so the hold is released either way.
			_, err := e.invoices.MarkPaid(ctx, inv.TenantID, inv.ID, "", settleAt)
			release()
			if err != nil {
				errs = append(errs, fmt.Errorf("settle zero-due invoice %s: %w", inv.ID, err))
				continue
			}
			// A background settle bypasses the invoice handler's dunning
			// resolve; close any active run here (best-effort; the
			// processRun pre-check backstops).
			e.resolveDunningRecovered(ctx, inv.TenantID, inv.ID)
			charged++
			slog.Info("auto-charge: nothing owed, marked paid", "invoice_id", inv.ID)
			continue
		}

		stripeCusID, stripePMID, err := e.paymentSetups.ResolveForCharge(ctx, inv.TenantID, inv.CustomerID)
		if err != nil {
			// A failed read (a database error, or a missing
			// VELOX_ENCRYPTION_KEY that fails every customer decrypt) is not
			// "no card": an error, so the sweep logs and counts it. No card
			// resolves to ("", "", nil). Nothing was sent; the release below
			// frees the hold and the invoice stays queued.
			errs = append(errs, fmt.Errorf("resolve payment method for invoice %s: %w", inv.ID, err))
		}
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
				outcome, nerr := e.noPMNotifier.NotifyNoPaymentMethod(ctx, inv.TenantID, inv, trigger)
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
			// Owed, credits already applied, and genuinely no card: start
			// no-payment dunning so the invoice reaches a terminal
			// (pause/cancel/write-off) instead of waiting forever. This is
			// the one place it starts, the no-card twin of the charger
			// starting dunning on a decline. Being here is what makes it
			// correct: the credit step above succeeded and still left money
			// owed, under this invoice's lease. (A separate enrollment job
			// that only read the queue could dun an invoice whose credits
			// were about to cover it, or whose credit apply had just failed.)
			// A resolve ERROR is not "no card", so it starts nothing.
			// StartDunning is idempotent: one run per invoice, ever.
			if err == nil {
				// Anchored on settleAt, the instant this collection runs in the
				// invoice's time domain: a cycle invoice's close when collected
				// at finalize, the clock's now for a draft finalized on a later
				// Advance (a paused subscription's). Its issue instant would date
				// that draft's first retry in the past.
				if derr := e.dunningStarter.StartDunning(ctx, inv.TenantID, inv.ID, inv.CustomerID, settleAt, domain.DunningCauseNoPaymentMethod); derr != nil {
					// Left queued: the next sweep retries the start.
					errs = append(errs, fmt.Errorf("start no-payment dunning for %s: %w", inv.ID, derr))
				}
			}
			// Provably pre-Stripe (no chargeable PM resolved): release so a
			// card attach retries without lease lag. The invoice stays queued
			// on purpose, and the sweep visits it every tick (the drain
			// reaches every queued row). That visit is what charges it once a
			// card is attached, settles it if credits arrive, retries an email
			// that was skipped, and starts dunning once the tenant has a
			// policy.
			release()
			continue
		}

		chargeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		if _, err := e.charger.ChargeInvoice(chargeCtx, inv.TenantID, inv, hold, stripeCusID, stripePMID); err != nil {
			cancel()
			// Below Stripe's minimum charge: not a decline and not an error of
			// this sweep. The charger recorded it (the invoice stays pending,
			// out of this queue, with the operator's notice) and ended any
			// payment recovery on it; nothing else is owed here.
			if errors.Is(err, domain.ErrBelowMinimumCharge) {
				slog.Info("auto-charge: Stripe refused the amount as below its minimum charge; left for the operator",
					"invoice_id", inv.ID)
				continue
			}
			// A DEFINITE failure (declined, or refused outright by Stripe):
			// ChargeInvoice recorded payment_status='failed' AND started
			// dunning inline (a decline through SettleFailed, which its
			// payment_intent.payment_failed webhook re-drives; a failure with
			// no PaymentIntent through stampUnreportableOutcome; a lost start is
			// backstopped by the EnrollFailedWithoutDunning reconciler). Clearing the flag is the
			// OWNERSHIP HANDOFF: dunning's retry schedule drives every later
			// attempt, so the sweep is never a second retry owner (ADR-116).
			//
			// Cleared even if the charger could not record 'failed' (it logs
			// CRITICAL then). Keeping the invoice queued would re-charge it
			// under the same idempotency key with rebuilt parameters (amount
			// after credits, a swapped card, the test-clock anchor), which
			// Stripe rejects as an idempotency conflict, and that parks the
			// invoice as unknown for good. The inline dunning run, which
			// retries under its own key, and the payment_failed webhook own it:
			// the charge call wrote its attempt row before reporting, so the
			// webhook records the decline as the invoice's newest attempt.
			var pe *payment.PaymentError
			if errors.As(err, &pe) && !pe.Unknown {
				if err := e.invoices.SetAutoChargePending(ctx, inv.TenantID, inv.ID, false); err != nil {
					// Benign relative to set(true): the next sweep re-lists the row
					// and its predicate re-check skips ineligible invoices.
					slog.Warn("failed to clear auto_charge_pending", "invoice_id", inv.ID, "error", err)
				}
				if pe.DeclineCode != "" {
					// A card decline is a normal business outcome; don't push
					// the catchup to internal_failure for it.
					slog.Info("auto-charge declined; dunning will retry on schedule",
						"invoice_id", inv.ID, "decline_code", pe.DeclineCode)
					continue
				}
				// No decline code (e.g. an invalid_request): still dunning's,
				// but surfaced so the operator sees an unusual refusal.
				errs = append(errs, fmt.Errorf("charge invoice %s refused: %w", inv.ID, err))
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
// that's currently parked at tax_status=pending and
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
//   - tax_status must be pending (retry-specific policy enforced
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
	// pending is the classic retry shape. The second admitted shape
	// is a draft whose calculation SUCCEEDED at build time but has since
	// aged past the provider's 24h TTL — a paused-collection sub's cycle
	// invoice is the common way a draft sits that long. Finalize refuses
	// those with "retry tax to refresh, then finalize" (the expiry
	// pre-check), so the refresh it names must be performable; recomputing
	// a DRAFT's tax is always safe, its totals are not yet issued.
	//
	// The refresh is deliberately NARROW — all three conditions found by
	// walking this against live Stripe, not by taste:
	//  - TaxTransactionID empty: once a transaction is committed,
	//    refreshing mints a second calculation beside it, and the
	//    invoice-scoped commit idempotency key (at most ONE tax
	//    transaction per invoice, ever) then rejects every future commit.
	//  - PROVEN expired (the same CheckTaxCalculationUsable rule finalize
	//    uses): a fresh OK calc has nothing to refresh, and keeping it
	//    refreshable would let an operator race the orphan reconciler's
	//    heal of a persist-failed commit — replacing the exact calc whose
	//    idempotent re-commit would have recovered the transaction id.
	okWithExpiredCalc := false
	if inv.TaxStatus == domain.InvoiceTaxOK && inv.TaxCalculationID != "" && inv.TaxTransactionID == "" {
		// PROVEN expired only — a guard that could not read its inputs is not
		// proof, and treating it as such would mint a second calculation.
		if err := e.CheckTaxCalculationUsable(ctx, tenantID, invoiceID, inv.TaxCalculationID); err != nil {
			if errs.Code(err) != "tax_calculation_expired" {
				return domain.Invoice{}, fmt.Errorf("check the draft's tax calculation: %w", err)
			}
			okWithExpiredCalc = true
		}
	}
	if inv.TaxStatus != domain.InvoiceTaxPending && !okWithExpiredCalc {
		return domain.Invoice{}, errs.InvalidState(fmt.Sprintf(
			"tax retry only valid when tax_status is pending, or to refresh a draft's EXPIRED uncommitted calculation (current: %s)", inv.TaxStatus))
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
// only difference is the gate. RetryTaxForInvoice requires tax_status
// pending because it backs the operator "Retry" affordance on a
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
		// pending so it always counts, and a re-finalize of a STUCK
		// draft (prior compute failed → pending) counts too — that
		// second compute genuinely is a retry. Cycle invoices never pass
		// through here at build time, so the counter now uniformly means
		// "attempts after the initial computation" for both invoice types.
		InitialAttempt: inv.TaxStatus != domain.InvoiceTaxPending,
	}

	return e.invoices.UpdateTaxAtomic(ctx, tenantID, invoiceID, update, items)
}

// nextTaxRetry decides whether the reconciler should pick this row
// up again, and when. Three outcomes encoded as a return:
//
//   - ok / exempt: nil → "ready now" (the row leaves the retryable
//     filter anyway because tax_status flips out of pending,
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
func (e *Engine) TenantLocation(ctx context.Context, tenantID string) (*time.Location, error) {
	return e.tenantLocation(ctx, tenantID)
}

// NetPaymentTermDays exposes the tenant's configured Net payment terms so the
// subscription handler's proration invoice stamps the same terms + due date
// the engine's own invoices do. Wired via subH.SetNetTermsReader(engine).
func (e *Engine) NetPaymentTermDays(ctx context.Context, tenantID string) (int, error) {
	return e.netPaymentTermDays(ctx, tenantID)
}

// netPaymentTermDays resolves the tenant's Net terms for an invoice the engine
// writes. 0 is a real setting ("Due on receipt": due_at == issued_at), not
// "unset" — the column is NOT NULL DEFAULT 30, so unset never reads as 0. A
// settings read failure is an error, not a Net 30 guess: a wrong due date
// moves reminders and dunning by up to a year, so the caller fails its close
// instead (P24; a cycle close is then retried per ADR-119).
func (e *Engine) netPaymentTermDays(ctx context.Context, tenantID string) (int, error) {
	if e.settings == nil {
		return 30, nil // test seam; production always wires settings
	}
	ts, err := e.settings.Get(ctx, tenantID)
	if err != nil {
		return 0, fmt.Errorf("read tenant net payment terms: %w", err)
	}
	if ts.NetPaymentTerms < 0 {
		return 0, refuse(refusalNetTerms, fmt.Errorf("tenant net payment terms is negative (%d)", ts.NetPaymentTerms))
	}
	return ts.NetPaymentTerms, nil
}

// tenantLocation resolves the tenant's billing timezone (the one org-level
// timezone, ADR-077). It anchors every month/year calendar advance (period
// boundaries AND proration denominators, ADR-058) so billing date-math is
// independent of the host time.Local and the value's ambient Location.
// A settings read failure is an error, never UTC: a boundary computed in the
// wrong zone moves hours of usage onto the neighbouring month's invoice, and
// one failed read among several in a close would mix two zones on one invoice
// (P24). UTC only when the setting itself is UTC or unset.
func (e *Engine) tenantLocation(ctx context.Context, tenantID string) (*time.Location, error) {
	if e.settings == nil {
		return time.UTC, nil // test seam; production always wires settings
	}
	ts, err := e.settings.Get(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("read tenant timezone: %w", err)
	}
	if ts.Timezone == "" {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(ts.Timezone)
	if err != nil {
		return nil, refuse(refusalTimezone, fmt.Errorf("load tenant timezone %q: %w", ts.Timezone, err))
	}
	return loc, nil
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
			"action":       domain.SubAuditTrialEndCancel,
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
