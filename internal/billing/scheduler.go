package billing

import (
	"context"
	"log/slog"
	"time"

	mw "github.com/sagarsuperuser/velox/internal/api/middleware"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/platform/scheduler"
)

// DunningProcessor processes due dunning runs.
type DunningProcessor interface {
	ProcessDueRuns(ctx context.Context, tenantID string, limit int) (int, []error)
}

// TenantLister lists all tenant IDs for background processing.
type TenantLister interface {
	ListTenantIDs(ctx context.Context) ([]string, error)
}

// CreditExpirer expires credit grants past their expiry date.
type CreditExpirer interface {
	ExpireCredits(ctx context.Context) (int, []error)
}

// TaxRetrier is the narrow hook the scheduler uses to drive the
// background tax-retry reconciler (ADR-017). Implemented by
// *invoice.Service. Returns (processed, per-row errors); the
// errors slice is non-fatal — one bad row doesn't abort the
// batch.
type TaxRetrier interface {
	RetryPendingTax(ctx context.Context, batch int) (int, []error)
	// RetryPendingTaxCommit re-commits finalized stripe_tax invoices whose
	// Stripe tax_transaction was created but whose id never persisted locally
	// (so a later void couldn't reverse it). Idempotent — Stripe returns the
	// original transaction, never a duplicate.
	RetryPendingTaxCommit(ctx context.Context, batch int) (int, []error)
	// RetryPendingTaxReversal re-reverses voided/uncollectible stripe_tax
	// invoices whose upstream reversal failed at status-change time (tenant
	// over-remits until recovered). Symmetric sibling of RetryPendingTaxCommit;
	// idempotent at Stripe via the invoice-stable reversal reference.
	RetryPendingTaxReversal(ctx context.Context, batch int) (int, []error)
}

// ClawbackRetrier re-issues subscription downgrade/removal/qty-decrease clawback
// credit notes whose post-commit Issue() failed (created as drafts IN the
// atomic item-change tx, marked issue_pending). Implemented by
// *creditnote.Service. Idempotent — the Stripe Tax reversal + the ledger credit
// are deduped, so a re-issue converges rather than double-credits. Without this
// hook a transient Issue() failure leaves the customer un-credited for a
// removed/downgraded item until manual reconciliation (ADR-056 follow-up).
type ClawbackRetrier interface {
	RetryPendingClawbackIssue(ctx context.Context, batch int) (int, []error)
	// RetryPendingCreditNoteTaxReversal re-drives credit-note upstream tax
	// reversals whose POST-COMMIT inline attempt failed (tax_reversal_pending).
	// The CN-path counterpart to RetryPendingTaxReversal, which only scans
	// voided/uncollectible invoices and so can't see a reversal stamped on a
	// finalized/paid invoice by an issued credit note. Implemented by
	// *creditnote.Service.
	RetryPendingCreditNoteTaxReversal(ctx context.Context, batch int) (int, []error)
}

// TrialExpirer is the narrow hook the scheduler uses to flip
// non-clock-pinned trialing subs to active at trial_end_at on the
// wall-clock cron tick. Implemented by *subscription.Service via
// ProcessExpiredTrials. Bug #8 — without this, status stays
// 'trialing' until the engine wakes at next_billing_at (~30 days
// past trial_end_at for calendar billing).
//
// ADR-028 disjoint flows: implementation EXCLUDES clock-pinned
// rows — those flow through the catchup orchestrator's Phase 0.5.
type TrialExpirer interface {
	ProcessExpiredTrials(ctx context.Context, batch int) (int, []error)
}

// PauseResumer is the narrow hook the scheduler uses to auto-resume
// non-clock-pinned subs whose pause_collection_resumes_at has
// elapsed in wall-clock time. Implemented by *subscription.Service
// via ProcessExpiredPauseCollections. Stripe-parity: resume happens
// AT resumes_at, not at the next cycle close. Without this, a
// production sub whose resumes_at is past but whose next_billing_at
// is in the future stays paused indefinitely — the engine's in-cycle
// gate would only fire when a cycle was actually due.
type PauseResumer interface {
	ProcessExpiredPauseCollections(ctx context.Context, batch int) (int, []error)
}

// TokenCleaner cleans up expired payment update tokens.
type TokenCleaner interface {
	Cleanup(ctx context.Context) (int, error)
}

// IdempotencyCleaner cleans up expired idempotency keys.
type IdempotencyCleaner interface {
	Cleanup(ctx context.Context) (int, error)
}

// PaymentReconciler resolves invoices in the PaymentUnknown state by querying
// Stripe for the authoritative PaymentIntent outcome. See payment.Reconciler.
type PaymentReconciler interface {
	Run(ctx context.Context, limit int) (int, []error)
	// RecoverChargeIntents names PaymentIntents whose outcome was lost — the
	// crash between the Stripe call and the outcome write, and the ambiguous
	// error carrying no PI id. It replays the attempt's stored idempotency key,
	// which returns the ORIGINAL PaymentIntent rather than creating a second
	// one (ADR-106). Must run before Run: Run is the sweep that would otherwise
	// settle those invoices failed and free a retry to double-charge.
	RecoverChargeIntents(ctx context.Context, limit int) (int, []error)
}

// Lock represents a held cluster-wide lock that the holder must release.
type Lock interface {
	Release()
}

// Locker acquires cluster-wide singleton locks by key — typically backed by
// Postgres advisory locks. Returned (nil, false, nil) means another leader
// holds the lock; caller should skip the tick. Nil Locker disables leader
// gating (single-replica mode).
type Locker interface {
	TryAdvisoryLock(ctx context.Context, key int64) (Lock, bool, error)
}

// Scheduler runs the billing cycle engine and dunning processor on a periodic interval.
type Scheduler struct {
	engine            *Engine
	dunning           DunningProcessor
	tenants           TenantLister
	credits           CreditExpirer
	tokenCleaner      TokenCleaner
	idempotencyClean  IdempotencyCleaner
	taxRetrier        TaxRetrier
	clawbackRetrier   ClawbackRetrier
	trialExpirer      TrialExpirer
	pauseResumer      PauseResumer
	paymentReconciler PaymentReconciler
	locker            Locker
	billingLockKey    int64
	dunningLockKey    int64
	interval          time.Duration
	batch             int
	onRun             func() // called after each complete scheduler tick (for health tracking)
	clock             clock.Clock
}

// Interval returns the configured scheduler interval.
func (s *Scheduler) Interval() time.Duration { return s.interval }

func NewScheduler(engine *Engine, interval time.Duration, batch int, dunning DunningProcessor, tenants TenantLister, clk clock.Clock, credits ...CreditExpirer) *Scheduler {
	if interval <= 0 {
		interval = 1 * time.Hour
	}
	if batch <= 0 {
		batch = 50
	}
	if clk == nil {
		clk = clock.Real()
	}
	s := &Scheduler{engine: engine, dunning: dunning, tenants: tenants, interval: interval, batch: batch, clock: clk}
	if len(credits) > 0 {
		s.credits = credits[0]
	}
	return s
}

// SetTokenCleaner sets the token cleanup dependency for expired payment update tokens.
func (s *Scheduler) SetTokenCleaner(cleaner TokenCleaner) {
	s.tokenCleaner = cleaner
}

// SetIdempotencyCleaner wires the idempotency-key cleanup task. Without a
// periodic purge the idempotency_keys table grows unbounded (one row per
// mutating API call per tenant), and every cache lookup walks a larger
// B-tree — a slow leak that only shows up in p99 latency weeks later.
func (s *Scheduler) SetIdempotencyCleaner(cleaner IdempotencyCleaner) {
	s.idempotencyClean = cleaner
}

// SetTaxRetrier wires the background tax-retry reconciler. Each
// tick scans invoices stuck at tax_status pending|failed with a
// retryable code (provider_outage, unknown) past their backoff
// timestamp, recomputes tax, and (on success + non-manual
// invoices) auto-finalizes. Without this hook, tax-pending
// invoices wait on an operator click forever. ADR-017.
func (s *Scheduler) SetTaxRetrier(r TaxRetrier) {
	s.taxRetrier = r
}

// SetClawbackRetrier wires the background reconciler that re-issues clawback
// credit-note drafts whose post-commit Issue() failed. Without this hook a
// transient Issue() failure leaves the draft un-issued (customer un-credited)
// until an operator reconciles manually (ADR-056 follow-up).
func (s *Scheduler) SetClawbackRetrier(r ClawbackRetrier) {
	s.clawbackRetrier = r
}

// SetTrialExpirer wires the wall-clock trial-expiry phase (Bug #8).
// Each tick scans non-clock-pinned trialing subs past their
// trial_end_at and flips status to active at the trial-end instant.
// Without this, the engine flips status only at cycle close — for
// calendar billing that's the next month boundary, ~30 days late.
func (s *Scheduler) SetTrialExpirer(t TrialExpirer) {
	s.trialExpirer = t
}

// SetPauseResumer wires the wall-clock pause-resume phase. Without it,
// non-clock-pinned subs whose pause_collection_resumes_at has elapsed
// stay paused until a cycle is independently due — diverging from
// Stripe's "resume AT resumes_at" semantics.
func (s *Scheduler) SetPauseResumer(p PauseResumer) {
	s.pauseResumer = p
}

// SetPaymentReconciler wires a resolver for PaymentUnknown invoices. Runs
// each tick after auto-charge retries, before the billing cycle generates
// new invoices, so a charge stuck in the unknown state can clear before
// we risk issuing a duplicate.
func (s *Scheduler) SetPaymentReconciler(r PaymentReconciler) {
	s.paymentReconciler = r
}

// SetLocker enables leader gating. When set, the scheduler only runs the
// billing and dunning halves of its tick if it wins the relevant advisory
// lock — preventing two app replicas from both generating invoices or both
// advancing the same dunning run. Pass nil (default) for single-replica or
// test-mode operation where gating is unwanted.
func (s *Scheduler) SetLocker(locker Locker, billingKey, dunningKey int64) {
	s.locker = locker
	s.billingLockKey = billingKey
	s.dunningLockKey = dunningKey
}

// SetOnRun registers a callback invoked after each complete scheduler tick.
// Used by the API health check to track scheduler liveness.
func (s *Scheduler) SetOnRun(fn func()) {
	s.onRun = fn
}

// Start runs the scheduler in a background goroutine.
// It blocks until the context is canceled (graceful shutdown).
func (s *Scheduler) Start(ctx context.Context) {
	slog.Info("billing scheduler started",
		"interval", s.interval.String(),
		"batch_size", s.batch,
	)
	scheduler.Run(ctx, "billing", s.interval, s.runOnce)
}

func (s *Scheduler) runOnce(ctx context.Context) {
	s.runBillingHalf(ctx)
	s.runDunningHalf(ctx)

	// Notify health check that a scheduler tick completed. Fires even when
	// both halves were skipped (lock contention) so the health probe still
	// sees the scheduler as alive on follower replicas.
	if s.onRun != nil {
		s.onRun()
	}
}

// runBillingHalf runs the leader-gated half that generates money — recovery
// reconcilers, auto-charge retries, invoice issuance, and the state flips the
// cycle scan depends on (trial expiry, pause auto-resume). Splitting from
// dunning lets two replicas divvy up roles instead of one replica monopolising
// every periodic job.
//
// Mode fan-out: every mode-scoped step runs once per livemode (live, then
// test) under its own ctx so downstream TxTenant transactions route to the
// correct RLS partition. Without this the scheduler inherits the default
// live-mode fallback from an unset ctx and silently hides test-mode rows
// (reads) or misroutes writes into the live partition. Cross-mode cleanup
// steps (token + idempotency purge) run once, outside the fan-out.
func (s *Scheduler) runBillingHalf(ctx context.Context) {
	start := time.Now()

	if s.locker != nil {
		lock, acquired, err := s.locker.TryAdvisoryLock(ctx, s.billingLockKey)
		if err != nil {
			slog.Error("billing scheduler: lock acquire failed", "error", err)
			return
		}
		if !acquired {
			slog.Debug("billing scheduler: another leader holds the lock; skipping tick")
			return
		}
		defer lock.Release()
	}

	// Mode-scoped work: live first (the bulk of traffic), then test. One
	// mode's errors do not gate the other — they share nothing at the
	// DB level once the RLS predicate splits them.
	for _, live := range []bool{true, false} {
		modeCtx := postgres.WithLivemode(ctx, live)
		s.runBillingCycleForMode(modeCtx, live)
	}

	duration := time.Since(start)
	mw.RecordBillingCycleDuration(duration.Seconds())

	// Cross-mode cleanup: TxBypass DELETEs that are mode-agnostic by design
	// (tokens / idempotency keys expire regardless of mode). Running these
	// once saves two DB round-trips per tick.
	s.runCrossModeCleanup(ctx)
}

// runBillingCycleForMode runs a single mode's half-cycle, in the step order
// below: reconcile → auto-charge retry → card-less dunning enrollment →
// threshold scan → trial expiry → pause auto-resume → bill → expire credits.
// Called twice per tick — once with ctx livemode=true, once with
// livemode=false. Logs are tagged with the mode so operators can distinguish
// partitions.
//
// There is NO scheduled "approaching-due"/reminder step here, and never has
// been: payment reminders are dunning-driven (runDunningHalf) or operator-
// triggered (AttentionActionSendReminder). The claim was stale prose from #13.
func (s *Scheduler) runBillingCycleForMode(ctx context.Context, live bool) {
	// Sanity: the only legitimate caller (runBillingHalf) always wraps ctx
	// with WithLivemode before calling us. Assert here so a future refactor
	// that skips the wrap crashes the test that exercises this path, not
	// silently routes test-mode work into the live partition.
	ctx = postgres.WithRequiredLivemode(ctx)

	mode := "live"
	if !live {
		mode = "test"
	}

	// Recovery reconcilers — each scans for stuck/failed post-commit effects
	// (eligibility derived from durable state) and re-drives them idempotently,
	// every tick, in a DELIBERATE order (see s.reconcilers): payment_unknown and
	// the tax reconcilers run BEFORE auto-charge below, so a stuck-unknown charge
	// that actually succeeded is marked paid, and a freshly-finalized invoice's
	// finalize-time auto-charge doesn't slip a tick. One uniform driver owns the
	// logging + the per-reconciler sweep metric (previously only auto-charge was
	// metered). The per-effect re-drive bodies and their eligibility SQL are
	// unchanged in their own services. The future obligation-queue drainer
	// (ADR-062) slots in here as one more Reconciler.
	runReconcilers(ctx, mode, s.reconcilers(), s.batch)

	// 0. Retry pending auto-charges from previous cycles
	if chargeRetried, chargeErrs := s.engine.RetryPendingCharges(ctx, s.batch); chargeRetried > 0 || len(chargeErrs) > 0 {
		slog.Info("auto-charge retries", "mode", mode, "succeeded", chargeRetried, "errors", len(chargeErrs))
		for i := 0; i < chargeRetried; i++ {
			mw.RecordAutoChargeRetry("succeeded")
		}
		for _, e := range chargeErrs {
			slog.Error("auto-charge retry error", "mode", mode, "error", e)
			mw.RecordAutoChargeRetry("failed")
		}
	}

	// 0.1 Enroll card-less stalled invoices into dunning. Runs AFTER the
	// auto-charge retry so a declined card (which already started dunning
	// inline + cleared auto_charge_pending) and a successful charge are
	// both gone from the candidate set — what remains is the no-card case
	// that RetryPendingCharges can never resolve. Without this, a finalized
	// auto_charge_pending invoice with no payment method is retried forever
	// and never reaches a terminal (the card-less "limbo" sink). StartDunning
	// is idempotent, so this is safe to run every tick.
	if swept, dErrs := s.engine.EnrollStalledForDunning(ctx, s.batch); swept > 0 || len(dErrs) > 0 {
		slog.Info("no-payment dunning enrollment", "mode", mode, "swept", swept, "errors", len(dErrs))
		for _, e := range dErrs {
			slog.Error("no-payment dunning enrollment error", "mode", mode, "error", e)
		}
	}

	// 0.5 Billing thresholds — Stripe-parity hard-cap scan. Runs before the
	// natural cycle so a threshold-fired invoice and (when reset_billing_cycle
	// is true) the cycle advance both land on this tick. The partial unique
	// index on invoices(tenant, sub, billing_period_start) WHERE
	// billing_reason='threshold' makes this idempotent under retry.
	if fired, tErrs := s.engine.ScanThresholds(ctx, s.batch); fired > 0 || len(tErrs) > 0 {
		slog.Info("threshold scan", "mode", mode, "fired", fired, "errors", len(tErrs))
		for _, err := range tErrs {
			slog.Error("threshold scan error", "mode", mode, "error", err)
		}
	}

	// 0.9 Trial expiry — flip non-clock-pinned trialing subs to
	// active at trial_end_at BEFORE the cycle scan reads the sub
	// list. Without this, calendar-billed trial subs sit at
	// status='trialing' for up to ~30 days past actual trial-end
	// (the gap from trial_end_at to next calendar-month boundary).
	// Clock-pinned subs are EXCLUDED here — they flow through the
	// catchup orchestrator's Phase 0.5 instead (ADR-028 disjoint
	// flows).
	if s.trialExpirer != nil {
		flipped, tErrs := s.trialExpirer.ProcessExpiredTrials(ctx, s.batch)
		if flipped > 0 || len(tErrs) > 0 {
			slog.Info("trial expiry", "mode", mode, "flipped", flipped, "errors", len(tErrs))
		}
		for _, err := range tErrs {
			slog.Error("trial expiry error", "mode", mode, "error", err)
		}
	}

	// 0.95 Pause auto-resume — clear pause_collection on any sub
	// whose resumes_at has elapsed, BEFORE the cycle scan reads the
	// due list. Stripe-parity (resume AT resumes_at, not next cycle
	// close). Wall-clock counterpart to the catchup orchestrator's
	// Phase 0.7. Clock-pinned subs are excluded per ADR-028.
	if s.pauseResumer != nil {
		resumed, pErrs := s.pauseResumer.ProcessExpiredPauseCollections(ctx, s.batch)
		if resumed > 0 || len(pErrs) > 0 {
			slog.Info("pause auto-resume", "mode", mode, "resumed", resumed, "errors", len(pErrs))
		}
		for _, err := range pErrs {
			slog.Error("pause auto-resume error", "mode", mode, "error", err)
		}
	}

	// 1. Billing cycle — generate invoices
	generated, errs := s.engine.RunCycle(ctx, s.batch)
	// Record the cycle execution + its invoice count. Per-mode (live, then
	// test), so billing_cycles_total counts mode-passes and invoices_generated
	// accumulates the per-mode totals. Recorded unconditionally — a run with
	// errors still ran and may have generated some invoices; the error counter
	// (below) is the separate failure signal.
	mw.RecordBillingCycle(generated)
	if len(errs) > 0 {
		slog.Error("billing cycle completed with errors",
			"mode", mode,
			"generated", generated,
			"errors", len(errs),
		)
		for _, err := range errs {
			slog.Error("billing cycle error", "mode", mode, "error", err)
			mw.RecordBillingCycleError()
		}
	} else if generated > 0 {
		slog.Info("billing cycle completed", "mode", mode, "generated", generated)
	}

	// 3. Credit expiry sweep
	if s.credits != nil {
		expired, cErrs := s.credits.ExpireCredits(ctx)
		if len(cErrs) > 0 {
			for _, e := range cErrs {
				slog.Error("credit expiry error", "mode", mode, "error", e)
			}
		}
		for i := 0; i < expired; i++ {
			mw.RecordCreditOperation("expiry")
		}
		if expired > 0 {
			slog.Info("credits expired", "mode", mode, "count", expired)
		}
	}
}

// runCrossModeCleanup runs the mode-agnostic cleanup steps exactly once per
// tick. These operate via TxBypass DELETEs that intentionally ignore livemode
// (an expired idempotency key or payment-update token is expired regardless
// of which partition it belonged to), so fanning out would just double the
// DB round-trips for no additional work.
func (s *Scheduler) runCrossModeCleanup(ctx context.Context) {
	// 5. Payment update token cleanup (expired > 7 days)
	if s.tokenCleaner != nil {
		cleaned, err := s.tokenCleaner.Cleanup(ctx)
		if err != nil {
			slog.Error("token cleanup error", "error", err)
		} else if cleaned > 0 {
			slog.Info("expired payment tokens cleaned up", "count", cleaned)
			mw.RecordScheduledCleanup("payment_tokens", cleaned)
		}
	}

	// 6. Idempotency key cleanup (expires_at < now, default 24h retention).
	// Prevents unbounded growth of idempotency_keys, which would slow every
	// cache lookup as the B-tree deepens.
	if s.idempotencyClean != nil {
		cleaned, err := s.idempotencyClean.Cleanup(ctx)
		if err != nil {
			slog.Error("idempotency cleanup error", "error", err)
		} else if cleaned > 0 {
			slog.Info("expired idempotency keys cleaned up", "count", cleaned)
			mw.RecordScheduledCleanup("idempotency_keys", cleaned)
		}
	}

}

// runDunningHalf runs the leader-gated half that advances dunning state.
// Held behind a separate lock key so a replica can win dunning even if
// another replica is currently running the (longer-lived) billing half.
//
// Fans out per livemode (live then test) so ProcessDueRuns — which opens
// TxTenant and relies on ctx livemode for RLS — sees both partitions.
// Tenant discovery runs once (ListTenantIDs is not mode-partitioned);
// per-tenant dunning work is what needs the fan-out.
func (s *Scheduler) runDunningHalf(ctx context.Context) {
	if s.dunning == nil || s.tenants == nil {
		return
	}

	if s.locker != nil {
		lock, acquired, err := s.locker.TryAdvisoryLock(ctx, s.dunningLockKey)
		if err != nil {
			slog.Error("dunning scheduler: lock acquire failed", "error", err)
			return
		}
		if !acquired {
			slog.Debug("dunning scheduler: another leader holds the lock; skipping tick")
			return
		}
		defer lock.Release()
	}

	tenantIDs, err := s.tenants.ListTenantIDs(ctx)
	if err != nil {
		slog.Error("dunning: failed to list tenants", "error", err)
		return
	}

	for _, live := range []bool{true, false} {
		modeCtx := postgres.WithLivemode(ctx, live)
		s.runDunningForMode(modeCtx, live, tenantIDs)
	}
}

// runDunningForMode processes due dunning runs for every tenant under the
// given livemode. Called twice per tick.
func (s *Scheduler) runDunningForMode(ctx context.Context, live bool, tenantIDs []string) {
	// Assert the fan-out site did its job; see note on runBillingCycleForMode.
	ctx = postgres.WithRequiredLivemode(ctx)

	mode := "live"
	if !live {
		mode = "test"
	}
	for _, tid := range tenantIDs {
		processed, dErrs := s.dunning.ProcessDueRuns(ctx, tid, 20)
		if len(dErrs) > 0 {
			// One "failed" increment per error (a per-run failure or the
			// list query itself) — the series the runbook's
			// {outcome="failed"} dunning-machinery alert watches.
			for _, e := range dErrs {
				slog.Error("dunning error", "mode", mode, "tenant_id", tid, "error", e)
				mw.RecordDunningRun("failed")
			}
		}
		for i := 0; i < processed; i++ {
			mw.RecordDunningRun("succeeded")
		}
		if processed > 0 {
			slog.Info("dunning runs processed", "mode", mode, "tenant_id", tid, "processed", processed)
		}
	}
}
