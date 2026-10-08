package testclock

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"runtime/debug"
	"strings"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
)

// envInjectCatchupFailure is the manual-test injection knob — see
// runCatchupLoop and MANUAL_TEST.md FLOW TC2. Setting the env var to
// any non-empty value causes the next catchup attempt to fail with
// the value as the failure reason ("injected: <reason>"). The flag
// is one-shot — cleared after firing — so the operator can chain
// failure → Retry advance → success in one session.
//
// Off-by-default. Production processes don't set it; if they
// accidentally do, every advance fails until they unset it (loud
// failure, easy to spot). Not a security risk: the env is process-
// local and only affects an operator-initiated test-clock advance.
const envInjectCatchupFailure = "VELOX_TEST_CLOCK_INJECT_FAILURE"

// injectedCatchupFailureReason reads + clears the injection env var.
// One-shot semantics: if the var was set, the helper returns the
// value AND unsets the env so a subsequent retry sees a clean state.
// Returns empty when the var isn't set (production fast path).
func injectedCatchupFailureReason() string {
	v := os.Getenv(envInjectCatchupFailure)
	if v == "" {
		return ""
	}
	// One-shot: clear so Retry advance sees a clean state. Process-
	// local env mutation; doesn't affect the parent shell.
	_ = os.Unsetenv(envInjectCatchupFailure)
	return "injected: " + v
}

// BillingRunner is the narrow hook the service uses to drive a billing
// catchup after a clock advance. In production billing.Engine satisfies
// it (via RunCycleForClock); tests can stub with a spy that records calls.
//
// Post-ADR-028 disjoint flows: the catchup uses RunCycleForClock,
// which scopes to subs pinned to ONE clock. The wall-clock cron's
// generic RunCycle is a different code path and never touches
// clock-pinned subs. This separation removes the SKIP-LOCKED race
// and the drip-bill artifact that the dual-flow design produced.
//
// The per-sub period loop in billSubscription handles multi-year
// catchup internally — one call per sub processes all due periods.
type BillingRunner interface {
	RunCycleForClock(ctx context.Context, tenantID, clockID string, batchSize int) (int, []error)

	// RetryPendingChargesForClock retries auto-charge attempts on
	// clock-pinned invoices flagged auto_charge_pending=true. Called
	// during catchup AFTER period generation, so customers who attached
	// a payment method since the last advance get charged as part of
	// THIS advance, not on a future wall-clock tick. ADR-029 Phase 1.
	RetryPendingChargesForClock(ctx context.Context, tenantID, clockID string, limit int) (int, []error)

	// ScanThresholdsForClock fires hard-cap (Stripe-parity billing-
	// thresholds) invoices for clock-pinned subs whose running cycle
	// subtotal has crossed a configured threshold. Runs during catchup
	// after period generation; without it, the wall-clock cron would
	// (incorrectly) fire threshold invoices on clock-pinned subs.
	// ADR-029 Phase 3.
	ScanThresholdsForClock(ctx context.Context, tenantID, clockID string, batchSize int) (int, []error)
}

// CustomerReader is the narrow hook the service uses to list customers
// pinned to a clock. The customer domain owns the customers table —
// including the encrypt-at-rest wrapper around display_name / email —
// so testclock reads through this interface instead of joining the
// table directly. That way every caller of the "attached customers"
// surface gets the same decrypted view that every other read path
// (customer detail page, customer list, etc.) does.
//
// Per-domain rule (CLAUDE.md): zero cross-domain imports between peer
// packages; coordination via narrow interfaces only.
type CustomerReader interface {
	ListByTestClockID(ctx context.Context, tenantID, clockID string) ([]domain.Customer, error)
}

// TrialExpirer is the narrow hook the catchup orchestrator uses to
// flip trialing subs whose `trial_end_at` has elapsed in sim time to
// active — at `trial_end_at`, not at the later cycle close.
// Implemented by *subscription.Service via ProcessExpiredTrialsForClock.
// Runs as Phase 0.5 (before cycle billing) so by the time Phase 1
// reads the sub list, the status field reflects the actual lifecycle
// state. Without this phase, status stays 'trialing' for the gap
// between trial_end_at and the first chargeable cycle close (up to
// ~30 days for calendar billing).
type TrialExpirer interface {
	ProcessExpiredTrialsForClock(ctx context.Context, tenantID, clockID string, frozenTime time.Time) (int, []error)
}

// PauseResumer is the narrow hook the catchup orchestrator uses to
// auto-resume subs whose pause_collection_resumes_at has elapsed.
// Implemented by *subscription.Service via
// ProcessExpiredPauseCollectionsForClock. Runs as Phase 0.7 (after
// trial expiry, before cycle billing) so a sub whose pause expires
// inside this Advance window unpauses BEFORE the cycle scan evaluates
// it — giving Stripe-parity "resume at resumes_at" semantics. Without
// this phase, the auto-resume only fires inside billOnePeriod, which
// is silent on subs whose next_billing_at is still in the future.
type PauseResumer interface {
	ProcessExpiredPauseCollectionsForClock(ctx context.Context, tenantID, clockID string, frozenTime time.Time) (int, []error)
}

// TaxRetrier is the narrow hook the catchup orchestrator uses to drive
// the per-clock tax-retry phase. Implemented by *invoice.Service via
// RetryPendingTaxForClock. ADR-029 Phase 2.
//
// Returns (processed_count, per-row errors) — same shape as every
// other phase callback so the orchestrator can collect failures
// uniformly.
type TaxRetrier interface {
	RetryPendingTaxForClock(ctx context.Context, tenantID, clockID string, batch int) (int, []error)
}

// ClawbackRetrier is the narrow hook the catchup orchestrator uses to issue a
// clock's DEFERRED simulated clawback drafts — a downgrade clawback whose source
// invoice was in-flight (ADR-059 defer) when created and has since settled.
// Implemented by *creditnote.Service via RetryPendingClawbackIssueForClock.
// Takes frozenTime so Issue() stamps the credit note and its ledger / tax
// effects in simulated time. ADR-029 disjoint flows.
type ClawbackRetrier interface {
	RetryPendingClawbackIssueForClock(ctx context.Context, tenantID, clockID string, frozenTime time.Time, batch int) (int, []error)
}

// CreditExpirer is the narrow hook the catchup orchestrator uses to
// expire grants belonging to clock-pinned customers. ADR-029 Phase 4.
// Takes frozenTime explicitly so the per-grant `expires_at < now`
// comparison happens in simulated time, not wall-clock.
type CreditExpirer interface {
	ExpireCreditsForClock(ctx context.Context, tenantID, clockID string, frozenTime time.Time) (int, []error)
}

// DunningProcessor is the narrow hook the catchup orchestrator uses to
// advance dunning state for clock-pinned invoices. ADR-029 Phase 5.
// Takes frozenTime explicitly so the per-run next_action_at compare
// runs in simulated time.
type DunningProcessor interface {
	ProcessDueRunsForClock(ctx context.Context, tenantID, clockID string, frozenTime time.Time, limit int) (int, []error)
}

// Service provides the test-clock API surface. Depends on Store for
// persistence, optionally BillingRunner to drive the billing engine
// during catchup, and optionally CatchupQueue to dispatch catchup
// asynchronously after Advance. When the queue is wired, Advance
// returns as soon as the clock is marked advancing — a worker
// picks up the job and runs the catchup off the request path. When
// the queue is nil (narrow unit tests), Advance runs catchup
// inline so tests can assert end-state synchronously.
type Service struct {
	store         Store
	billing       BillingRunner
	queue         CatchupQueue
	customers     CustomerReader
	trialExpirer  TrialExpirer
	pauseResumer  PauseResumer
	taxRetry      TaxRetrier
	clawback      ClawbackRetrier
	creditExpirer CreditExpirer
	dunning       DunningProcessor
}

func NewService(store Store) *Service {
	return &Service{store: store}
}

// SetCustomerReader wires the customer-domain hook used to list
// attached customers. Late-bound (not a constructor arg) because the
// customer service and the testclock service are built in the same
// router-wiring step; passing a nil reader is fine for narrow unit
// tests that don't exercise the attached-customers surface, but the
// production producer always sets a real reader.
func (s *Service) SetCustomerReader(r CustomerReader) {
	s.customers = r
}

// SetBillingRunner wires the billing catchup hook. Kept as a setter rather
// than a constructor arg because the billing engine is built after the
// testclock service in router.go — the engine and service form a small
// dependency cycle (engine reads clocks, clock advance runs engine) that
// we break by deferred injection.
func (s *Service) SetBillingRunner(b BillingRunner) {
	s.billing = b
}

// SetCatchupQueue wires the async dispatch path. Production code
// always sets this; unit tests that want sync behaviour leave it
// nil so Advance runs catchup inline.
func (s *Service) SetCatchupQueue(q CatchupQueue) {
	s.queue = q
}

// SetDunning wires the per-clock dunning-advance phase of catchup.
// Optional — when nil, catchup skips Phase 5 (dunning advance) for
// clock-pinned invoices. The cron's ProcessDueRuns excludes them
// already, so nil here means clock-pinned dunning state freezes
// until an operator advances; for narrow tests without dunning
// fixtures, that's fine. ADR-029 Phase 5.
func (s *Service) SetDunning(d DunningProcessor) {
	s.dunning = d
}

// SetCreditExpirer wires the per-clock credit-expiry phase of catchup.
// Optional — when nil, catchup skips Phase 4 (credit expiry). The
// cron's ListExpiredGrants would have processed those rows pre-
// ADR-029, so nil here means clock-pinned grants don't expire until
// an operator advances the clock; for narrow unit tests that don't
// exercise credits, nil is fine. Production wires the real credit
// service. ADR-029 Phase 4.
func (s *Service) SetCreditExpirer(c CreditExpirer) {
	s.creditExpirer = c
}

// SetTaxRetrier wires the per-clock tax-retry phase of catchup.
// Optional — when nil, catchup skips Phase 2 (tax retry); the cron's
// ListPendingTaxRetry would have processed those rows pre-ADR-029, so
// nil here means clock-pinned tax retries simply don't fire (callers
// without the wiring are typically narrow unit tests). Production
// always wires this with the real invoice service. ADR-029 Phase 2.
func (s *Service) SetTaxRetrier(t TaxRetrier) {
	s.taxRetry = t
}

// SetClawbackRetrier wires the per-clock clawback-issue phase (ADR-029). When
// nil, catchup skips it and a deferred simulated clawback waits for a later
// advance that has the wiring; production wires the creditnote Service. Runs
// after the charge phase so a source invoice that just settled this advance is
// terminal by the time the deferred clawback re-drives.
func (s *Service) SetClawbackRetrier(c ClawbackRetrier) {
	s.clawback = c
}

// SetTrialExpirer wires the per-clock trial-expiry phase (Phase 0.5).
// Optional — narrow unit tests skip it; production wires the
// subscription Service. Without this, a clock advance past
// trial_end_at leaves the sub's status='trialing' until the next
// cycle close (Bug #8 regression).
func (s *Service) SetTrialExpirer(t TrialExpirer) {
	s.trialExpirer = t
}

// SetPauseResumer wires the per-clock pause-resume phase (Phase 0.7).
// Optional — without it, clock-pinned subs whose
// pause_collection_resumes_at has elapsed stay paused until a cycle
// happens to be due (the engine's defensive in-cycle gate). Production
// wires the subscription Service so resume-at-resumes_at parity with
// Stripe holds across all subs, not just those with imminent cycles.
func (s *Service) SetPauseResumer(r PauseResumer) {
	s.pauseResumer = r
}

type CreateInput struct {
	Name       string    `json:"name"`
	FrozenTime time.Time `json:"frozen_time"`
}

func (s *Service) Create(ctx context.Context, tenantID string, input CreateInput) (domain.TestClock, error) {
	name := strings.TrimSpace(input.Name)
	if len(name) > 200 {
		return domain.TestClock{}, errs.Invalid("name", "must be at most 200 characters")
	}
	if input.FrozenTime.IsZero() {
		return domain.TestClock{}, errs.Required("frozen_time")
	}

	return s.store.Create(ctx, tenantID, domain.TestClock{
		Name:       name,
		FrozenTime: input.FrozenTime.UTC(),
	})
}

func (s *Service) Get(ctx context.Context, tenantID, id string) (domain.TestClock, error) {
	return s.store.Get(ctx, tenantID, id)
}

func (s *Service) List(ctx context.Context, tenantID string) ([]domain.TestClock, error) {
	return s.store.List(ctx, tenantID)
}

func (s *Service) Delete(ctx context.Context, tenantID, id string) error {
	return s.store.Delete(ctx, tenantID, id)
}

// ListSubscriptions returns the subscriptions pinned to the given clock.
// Verifies the clock exists first so a missing-clock id surfaces as 404
// rather than an empty list (which would look like an empty clock).
func (s *Service) ListSubscriptions(ctx context.Context, tenantID, clockID string) ([]domain.Subscription, error) {
	if _, err := s.store.Get(ctx, tenantID, clockID); err != nil {
		return nil, err
	}
	return s.store.ListSubscriptionsOnClock(ctx, tenantID, clockID)
}

// ListAttachedCustomers returns customers pinned to this clock —
// the Stripe-parity "attached customers" surface (ADR-027 Tier 3).
// 404s if the clock doesn't exist. Reads through the customer
// domain's CustomerReader so display_name / email arrive decrypted;
// see CustomerReader doc.
func (s *Service) ListAttachedCustomers(ctx context.Context, tenantID, clockID string) ([]domain.Customer, error) {
	if _, err := s.store.Get(ctx, tenantID, clockID); err != nil {
		return nil, err
	}
	if s.customers == nil {
		return nil, fmt.Errorf("testclock: customer reader not wired")
	}
	return s.customers.ListByTestClockID(ctx, tenantID, clockID)
}

type AdvanceInput struct {
	FrozenTime time.Time `json:"frozen_time"`
}

// Advance moves the clock forward to FrozenTime and dispatches a
// billing catchup for every subscription attached to it. Catchup
// runs the billing engine in a loop because a large jump (e.g.
// 3 months forward on a monthly sub) closes multiple cycles —
// each engine sweep processes the cycles that are now due,
// advances next_billing_at, and the next sweep picks up the
// following cycle.
//
// Async dispatch: when SetCatchupQueue has been wired (the
// production path), Advance returns as soon as the clock is
// marked advancing and a CatchupJob has been enqueued. A worker
// picks up the job, runs the catchup, and flips the clock to
// ready / internal_failure when done. The dashboard polls
// /v1/test-clocks/{id} every 1.5s while status === 'advancing'
// to surface the transition. This matches Stripe's Test Clocks
// shape — the HTTP advance call returns in milliseconds, the
// catchup runs in the background.
//
// Sync fallback: when the queue is nil (narrow unit tests),
// Advance runs the catchup inline. RunCatchup contains the same
// logic the worker calls.
//
// State machine:
//
//	ready ──Advance── advancing ──catchup ok── ready
//	                       │
//	                       └──catchup errored── internal_failure
//
// While in advancing, other callers get 409 from the MarkAdvancing CAS; while
// in internal_failure, all further advances are blocked until the tenant
// inspects and deletes the clock.
func (s *Service) Advance(ctx context.Context, tenantID, id string, input AdvanceInput) (domain.TestClock, error) {
	if input.FrozenTime.IsZero() {
		return domain.TestClock{}, errs.Required("frozen_time")
	}
	newTime := input.FrozenTime.UTC()

	current, err := s.store.Get(ctx, tenantID, id)
	if err != nil {
		return domain.TestClock{}, err
	}
	if current.Status != domain.TestClockStatusReady {
		return domain.TestClock{}, errs.InvalidState(fmt.Sprintf("clock is %s, must be ready to advance", current.Status))
	}
	if !newTime.After(current.FrozenTime) {
		return domain.TestClock{}, errs.Invalid("frozen_time", "must be strictly after current frozen_time")
	}
	// Stripe-parity advance window cap (ADR-028 amendment): a single
	// Advance call shifts frozen_time by ≤ 1 year. Larger ranges are
	// chunked into successive advances by the operator. Reasons:
	//   - Predictable per-click resource use (operator can't trigger
	//     a multi-decade catchup that exceeds the 10-min worker
	//     timeout, then has to manually retry from internal_failure).
	//   - Iteration discipline: each chunk's invoices/dunning state
	//     is reviewable before the next advance.
	//   - Failure isolation: a bug in year 14 doesn't poison earlier
	//     years' simulated billing.
	// AddDate(1,0,0) handles leap years correctly (Mar 1 → next Feb 29
	// or Mar 1 depending on year). Constant year-arithmetic, not a
	// fixed nanosecond duration.
	maxAllowed := current.FrozenTime.AddDate(1, 0, 0)
	if newTime.After(maxAllowed) {
		return domain.TestClock{}, errs.Invalid("frozen_time",
			"advance cannot exceed 1 year per call — chunk longer ranges into successive advances (Stripe parity)")
	}

	advancing, err := s.store.MarkAdvancing(ctx, tenantID, id, newTime)
	if err != nil {
		return domain.TestClock{}, err
	}

	if s.queue != nil {
		// Async path. The worker drains the queue and calls
		// RunCatchup. If enqueue fails (buffer full), revert to
		// internal_failure so the operator gets visible feedback
		// rather than a clock stuck in 'advancing' forever.
		if err := s.queue.Enqueue(CatchupJob{TenantID: tenantID, ClockID: id, PrevFrozenTime: current.FrozenTime}); err != nil {
			if _, ferr := s.store.MarkFailed(ctx, tenantID, id, "catchup queue full: "+err.Error()); ferr != nil {
				slog.Error("test clock: failed to mark clock as failed after enqueue error",
					"clock_id", id, "enqueue_err", err, "mark_err", ferr)
			}
			return domain.TestClock{}, fmt.Errorf("dispatch catchup: %w", err)
		}
		return advancing, nil
	}

	// Sync fallback (tests / narrow setups without a queue).
	if err := s.RunCatchup(ctx, CatchupJob{TenantID: tenantID, ClockID: id, PrevFrozenTime: current.FrozenTime}); err != nil {
		return domain.TestClock{}, err
	}
	return s.store.Get(ctx, tenantID, id)
}

// RetryAdvance resumes a clock parked in status='internal_failure'
// from a prior catchup error. Stripe-parity recovery — the catchup
// loop is idempotent (only processes subs with next_billing_at <=
// frozen_time), so resuming from where the previous attempt
// stopped is safe. Frozen_time stays at its current value; the
// operator's earlier Advance input is preserved by virtue of
// MarkAdvancing already having stamped frozen_time before the
// failure. ADR-018.
//
// Async dispatch: same as Advance — when SetCatchupQueue is
// wired, returns as soon as the clock is back in 'advancing' and
// a CatchupJob is enqueued. Worker drains; dashboard polls.
//
// Refuses to retry from any state other than internal_failure
// with a 409. A clock currently in 'advancing' has a worker
// already running on it; a clock in 'ready' has no failure to
// retry.
func (s *Service) RetryAdvance(ctx context.Context, tenantID, id string) (domain.TestClock, error) {
	current, err := s.store.Get(ctx, tenantID, id)
	if err != nil {
		return domain.TestClock{}, err
	}
	if current.Status != domain.TestClockStatusInternalFailed {
		return domain.TestClock{}, errs.InvalidState(fmt.Sprintf(
			"retry only valid on clocks in internal_failure (current: %s)", current.Status))
	}

	advancing, err := s.store.RetryFromFailed(ctx, tenantID, id)
	if err != nil {
		return domain.TestClock{}, err
	}

	if s.queue != nil {
		if err := s.queue.Enqueue(CatchupJob{TenantID: tenantID, ClockID: id, PrevFrozenTime: current.FrozenTime}); err != nil {
			if _, ferr := s.store.MarkFailed(ctx, tenantID, id, "catchup queue full on retry: "+err.Error()); ferr != nil {
				slog.Error("test clock: failed to mark clock as failed after retry-enqueue error",
					"clock_id", id, "enqueue_err", err, "mark_err", ferr)
			}
			return domain.TestClock{}, fmt.Errorf("dispatch retry catchup: %w", err)
		}
		return advancing, nil
	}

	// Sync fallback (tests).
	if err := s.RunCatchup(ctx, CatchupJob{TenantID: tenantID, ClockID: id, PrevFrozenTime: current.FrozenTime}); err != nil {
		return domain.TestClock{}, err
	}
	return s.store.Get(ctx, tenantID, id)
}

// RunCatchup is the worker's entry point. The "real work" — looping
// over due periods per sub — lives in the billing engine
// (Engine.billSubscription, ADR-028). RunCatchup is a thin
// orchestrator: it calls RunCycle once, then flips the clock state
// from advancing → ready (success) or → internal_failure (error)
// with the failure reason captured for the dashboard banner.
//
// CatchupTimeout (10 min, set on the worker's ctx) bounds the total
// operation. The engine's per-sub safety counter (maxPeriodsPerSubPerCall)
// is the inner ceiling.
func (s *Service) RunCatchup(ctx context.Context, job CatchupJob) (err error) {
	// A panic anywhere in the catchup (a nil-deref in one of the billing
	// phases, etc.) must not strand the clock at status='advancing' —
	// that state has no operator exit: Advance requires 'ready' and
	// Retry advance requires 'internal_failure', so a wedged clock is
	// stuck until someone hand-edits the row. The worker's recover (and
	// chi's, on the inline path) only saves the process; neither flips
	// the clock. Convert the panic into the same internal_failure flip
	// the error path below takes — on a detached ctx, since the panic
	// may itself stem from ctx expiry — and return it as an error so
	// callers log it once. Reason text stays generic: panic strings
	// carry Go internals the dashboard banner must not leak (ADR-026).
	defer func() {
		if r := recover(); r != nil {
			slog.Error("test-clock catchup panicked",
				"clock_id", job.ClockID,
				"tenant_id", job.TenantID,
				"panic", r,
				"stack", string(debug.Stack()),
			)
			failCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			reason := errs.SanitizeForOperator(fmt.Errorf("internal error during catchup"), job.ClockID)
			if _, ferr := s.store.MarkFailed(failCtx, job.TenantID, job.ClockID, reason); ferr != nil {
				slog.Error("test clock: failed to mark clock as failed after catchup panic",
					"clock_id", job.ClockID, "panic", r, "mark_err", ferr)
			}
			err = fmt.Errorf("catchup panicked: %v", r)
		}
	}()

	if s.billing == nil {
		// No billing wired — just complete the state transition.
		// Used by narrow unit tests that exercise the state machine
		// without standing up the full engine.
		_, err := s.store.CompleteAdvance(ctx, job.TenantID, job.ClockID)
		return err
	}

	// Manual-test injection point (MANUAL_TEST FLOW TC2). Fires
	// before any billing work so the failure-UI smoke test lands
	// a fast, deterministic failure regardless of sub count.
	var (
		runErr         error
		operatorReason string                 // sanitized via errs.SummarizeForOperator (ADR-026)
		summary        *domain.AdvanceSummary // per-phase counts, persisted before the transition
	)
	if reason := injectedCatchupFailureReason(); reason != "" {
		runErr = fmt.Errorf("%s", reason)
		// Injection text is operator-safe by construction (test-only
		// knob, the value comes from a manual test fixture).
		operatorReason = reason
	} else {
		// ADR-028 disjoint flows: RunCycleForClock scopes to
		// THIS clock's pinned subs. Wall-clock cron uses
		// generic RunCycle and never sees these.
		//
		// ADR-029 catchup orchestration sequence — every time-aware
		// engine concern that touches clock-pinned entities runs HERE,
		// in lockstep with simulated time, instead of on the wall-clock
		// tick. Phases run in dependency order; a failure in any phase
		// is collected but doesn't stop subsequent phases (failure
		// isolation; retry-advance picks up wherever this left off).
		var runErrs []error

		// Accumulate the per-phase counts into the advance summary as we go.
		// AdvancedFrom is the pre-advance frozen_time the operator's request
		// captured (zero on the recover-in-flight path); AdvancedTo is filled
		// once we read the (now-advanced) frozen_time below.
		sum := domain.AdvanceSummary{AdvancedFrom: job.PrevFrozenTime}

		// Read frozen_time ONCE and bind it to ctx via
		// clock.WithEffectiveNow. Every downstream `clock.Now(ctx)` —
		// including store-level fallbacks in AppendEntry, invoice
		// create timestamps, etc. — inherits the simulated time.
		// Without this binding, helpers that fall back to
		// clock.Now(ctx) land at wall-clock and stamp rows with
		// today instead of the simulated instant the fact occurred
		// (caught 2026-05-22 against credit-expiry — expiry rows
		// landed at wall-clock today despite catchup running at
		// sim time).
		//
		// Concurrent advance can't race because status='advancing'
		// is the gate the operator's request hit before enqueueing.
		var frozen time.Time
		clk, clkErr := s.store.Get(ctx, job.TenantID, job.ClockID)
		if clkErr != nil {
			runErrs = append(runErrs, fmt.Errorf("read clock for catchup: %w", clkErr))
			// Without frozen_time we can't bind ctx; downstream
			// phases that need it skip via their own guards.
		} else {
			frozen = clk.FrozenTime
			// WithSim, not WithEffectiveNow: the catchup ctx carries the CLOCK
			// as well as the instant, so every audit row every phase below emits
			// (cycle finalize, threshold, tax retry, charge, dunning, clawback
			// issue) lands on the sim axis without any of them knowing test clocks
			// exist. Credit expiry runs under this same binding but emits NO audit
			// row at all — the event-sourced credit ledger IS its record — so it is
			// not in that list. (It was, in three separate places; see
			// internal/audit/audit.go's SIM-AXIS COVERAGE note, which is the one
			// that governs.) This is the driver
			// that makes the clock filter honest: after ADR-086 teardown these
			// rows are the ONLY surviving record of the advance.
			ctx = clock.WithSim(ctx, clock.Sim{At: frozen, TestClockID: job.ClockID})
			sum.AdvancedTo = frozen
		}
		trialExpiryFrozen := frozen

		// Phase 0.5 (Bug #8): flip trialing subs to active at
		// trial_end_at when sim time has elapsed past it, BEFORE
		// Phase 1 reads the sub list for cycle billing. Without
		// this, a sub whose trial ended weeks ago in sim time but
		// whose next_billing_at (cycle close) is still future keeps
		// reading as 'trialing' on the dashboard — confusing for
		// operators, semantically wrong vs Stripe / Lago where
		// status flips at trial_end_at.
		//
		// Also fires BillOnCreate per activated sub so in_advance
		// items' first paid period is covered at the activation
		// instant (Bug #6 carry-through).
		if s.trialExpirer != nil && !trialExpiryFrozen.IsZero() {
			trialCount, trialErrs := s.trialExpirer.ProcessExpiredTrialsForClock(ctx, job.TenantID, job.ClockID, trialExpiryFrozen)
			sum.TrialsActivated = trialCount
			runErrs = append(runErrs, trialErrs...)
		}

		// Phase 0.7: auto-resume any sub whose
		// pause_collection_resumes_at has elapsed in simulated time —
		// BEFORE Phase 1 reads the due list, so the cycle scan sees
		// the sub as un-paused and bills its now-finalized invoice
		// instead of leaving a draft. Stripe-parity (resume AT
		// resumes_at, not at next cycle close). frozen_time was
		// already read above for Phase 0.5; reuse it. Skips silently
		// if the wiring is absent.
		if s.pauseResumer != nil && !trialExpiryFrozen.IsZero() {
			pauseCount, pauseErrs := s.pauseResumer.ProcessExpiredPauseCollectionsForClock(ctx, job.TenantID, job.ClockID, trialExpiryFrozen)
			sum.PausesResumed = pauseCount
			runErrs = append(runErrs, pauseErrs...)
		}

		// Phase 1 (ADR-028): generate any newly-due periods. Multi-period
		// catchup happens inside billSubscription's per-sub loop.
		periodCount, periodErrs := s.billing.RunCycleForClock(ctx, job.TenantID, job.ClockID, 100)
		sum.InvoicesGenerated = periodCount
		runErrs = append(runErrs, periodErrs...)

		// Phase 1.5 (ADR-029 Phase 3): threshold scan for hard-cap
		// invoices. Runs after period generation (so the latest
		// running cycle is the one being evaluated) but before tax
		// retry / charge so any threshold-fired invoices get the
		// same downstream treatment as period-generated ones.
		thresholdCount, thresholdErrs := s.billing.ScanThresholdsForClock(ctx, job.TenantID, job.ClockID, 100)
		sum.ThresholdsFired = thresholdCount
		runErrs = append(runErrs, thresholdErrs...)

		// Phase 2 (ADR-029): tax retry on clock-pinned invoices left
		// at tax_status=pending with a retryable code. Runs BEFORE
		// Phase 3 (charge) so a successful tax retry unblocks finalize
		// in the same Advance — without this ordering, an invoice that
		// gets unstuck would have to wait for the next advance to fire
		// its charge attempt.
		if s.taxRetry != nil {
			taxCount, taxErrs := s.taxRetry.RetryPendingTaxForClock(ctx, job.TenantID, job.ClockID, 100)
			sum.TaxRetried = taxCount
			runErrs = append(runErrs, taxErrs...)
		}

		// Phase 3 (ADR-029): collect auto_charge_pending invoices for this
		// clock's subs. A card-less invoice gets its setup-link email and
		// no-payment dunning here too (the collector starts it), so a
		// simulated subscription with no payment method reaches a terminal
		// on a later Advance once simulated time passes the grace window. Customer may have attached a
		// PM via the customer portal between advances — without this
		// hook, neither cron nor catchup would charge until an operator
		// clicked Advance. This call closes that loop.
		chargeCount, chargeErrs := s.billing.RetryPendingChargesForClock(ctx, job.TenantID, job.ClockID, 100)
		sum.ChargesRetried = chargeCount
		runErrs = append(runErrs, chargeErrs...)

		// Phase 3.7 (ADR-029): issue this clock's DEFERRED simulated clawback
		// drafts. Runs AFTER the charge phase so a source invoice that just
		// settled this advance is terminal by now and its deferred clawback
		// (ADR-059) can re-drive; the wall-clock reconciler skips simulated
		// rows, so this is their only issuer. frozen_time is bound so Issue()
		// stamps the note + its ledger / tax effects in simulated time.
		if s.clawback != nil {
			clawbackCount, clawbackErrs := s.clawback.RetryPendingClawbackIssueForClock(ctx, job.TenantID, job.ClockID, frozen, 100)
			sum.ClawbacksIssued = clawbackCount
			runErrs = append(runErrs, clawbackErrs...)
		}

		// Phases 4 and 5 both need frozen_time — already read once at
		// the top of the catchup block and bound to ctx via
		// clock.WithSim above (NOT WithEffectiveNow — that binds the instant
		// only, and the catchup deliberately binds the CLOCK too so every audit
		// row below lands on the sim axis); `frozen` is the same value
		// for the whole job, so we reuse it here rather than re-reading.
		needFrozen := (s.creditExpirer != nil || s.dunning != nil) && !frozen.IsZero()

		// Phase 4 (ADR-029): credit expiry for grants belonging to
		// customers pinned to this clock. expires_at compared against
		// simulated time, not wall-clock.
		if s.creditExpirer != nil && needFrozen {
			creditCount, creditErrs := s.creditExpirer.ExpireCreditsForClock(ctx, job.TenantID, job.ClockID, frozen)
			sum.CreditsExpired = creditCount
			runErrs = append(runErrs, creditErrs...)
		}

		// Phase 5 (ADR-029): dunning advance for clock-pinned invoices.
		// Runs after the charge/credit-settle phases. A Phase 1/1.5/3
		// credit-cover settle now resolves the invoice's dunning run in place —
		// the engine calls resolveDunningRecovered on a credit-cover settle, and
		// processRun carries a paid-pre-check backstop — so a just-settled
		// invoice's run is resolved, never re-advanced (a credit-cover settle
		// previously left it active, the bug this fixes). next_action_at vs
		// simulated time.
		if s.dunning != nil && needFrozen {
			dunningCount, dunningErrs := s.dunning.ProcessDueRunsForClock(ctx, job.TenantID, job.ClockID, frozen, 50)
			sum.DunningAdvanced = dunningCount
			runErrs = append(runErrs, dunningErrs...)
		}

		// Billing ran — capture the summary (counts are exact, from the phase
		// returns) regardless of whether a phase errored; HadErrors records the
		// partial-failure case so the dashboard can caveat it.
		sum.HadErrors = len(runErrs) > 0
		summary = &sum

		if len(runErrs) > 0 {
			runErr = fmt.Errorf("catchup errors: %v", runErrs)
			// Operator-safe summary lands on the clock row's
			// last_failure_reason, which the dashboard renders in
			// the failure banner. Internal-only errors (SQLSTATE,
			// panic markers, file:line) are rolled up into a
			// generic "N internal error(s). Reference: <clock>"
			// per ADR-026; SafeMessageError implementers pass
			// through. Full Go-level diagnostic stays in slog
			// below — operator gets a clean banner, support has
			// the unredacted chain in logs.
			operatorReason = errs.SummarizeForOperator(runErrs, job.ClockID)
		}
	}

	// persistSummary writes the accumulated per-phase counts onto the clock row
	// just before the transition, so CompleteAdvance / MarkFailed read it back.
	// Best-effort: the summary is operator-facing display data, not on the
	// critical path — a save failure is logged but never blocks the transition
	// (the clock just shows no summary). The marshal is in-memory; failCtx vs
	// ctx is passed by the caller because the error path runs on a detached ctx.
	persistSummary := func(c context.Context) {
		if summary == nil {
			return
		}
		b, mErr := json.Marshal(summary)
		if mErr != nil {
			slog.Error("test clock: marshal advance summary", "clock_id", job.ClockID, "error", mErr)
			return
		}
		if sErr := s.store.SaveAdvanceSummary(c, job.TenantID, job.ClockID, b); sErr != nil {
			slog.Error("test clock: persist advance summary", "clock_id", job.ClockID, "error", sErr)
		}
	}

	if runErr != nil {
		// Flip to internal_failure with the operator-safe reason —
		// dashboard can show "Catchup failed: <reason>" without
		// leaking SQLSTATE / Go internals. Partial catchup (any
		// periods committed before the error) stays applied; the
		// operator can Retry advance (idempotent) or delete the
		// clock to start fresh. ADR-018, ADR-026.
		reason := operatorReason
		if reason == "" {
			// Fallback: injection path (already operator-safe by
			// construction) or unreachable. Sanitize anyway as
			// belt-and-suspenders against future paths.
			reason = errs.SanitizeForOperator(runErr, job.ClockID)
		}
		// The common cause of runErr is the catchup ctx hitting CatchupTimeout.
		// Reusing that already-expired ctx for MarkFailed would fail too,
		// leaving the clock stuck in 'advancing' forever. Detach from the
		// catchup deadline (retaining ctx values — the livemode pin) and give
		// the failure-flip its own short timeout so the clock always lands in
		// internal_failure where the operator can retry or delete it.
		failCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		persistSummary(failCtx)
		if _, ferr := s.store.MarkFailed(failCtx, job.TenantID, job.ClockID, reason); ferr != nil {
			slog.Error("test clock: failed to mark clock as failed after catchup error",
				"clock_id", job.ClockID, "catchup_err", runErr, "mark_err", ferr)
		}
		return fmt.Errorf("billing catchup failed: %w", runErr)
	}

	persistSummary(ctx)
	if _, err := s.store.CompleteAdvance(ctx, job.TenantID, job.ClockID); err != nil {
		return fmt.Errorf("complete advance: %w", err)
	}
	return nil
}

// RecoverInFlight scans for clocks left in status='advancing' from
// a prior process — typically because the server restarted while a
// catchup was running — and re-enqueues them. Idempotent:
// runCatchupLoop only processes subs with next_billing_at <=
// frozen_time, so resuming partial work just continues from where
// it stopped.
//
// Called once on boot AFTER the worker is wired. If the queue is
// nil (test path), this is a no-op.
func (s *Service) RecoverInFlight(ctx context.Context) error {
	if s.queue == nil {
		return nil
	}
	clocks, err := s.store.ListAllAdvancing(ctx)
	if err != nil {
		return fmt.Errorf("list advancing clocks: %w", err)
	}
	for _, c := range clocks {
		if err := s.queue.Enqueue(CatchupJob{TenantID: c.TenantID, ClockID: c.ID}); err != nil {
			slog.Error("test clock: failed to re-enqueue in-flight catchup on boot",
				"clock_id", c.ID, "tenant_id", c.TenantID, "error", err)
			continue
		}
		slog.Info("test clock: re-enqueued in-flight catchup on boot",
			"clock_id", c.ID, "tenant_id", c.TenantID)
	}
	return nil
}

// runCatchupLoop is the inner loop. Extracted from the previous
// runCatchup so RunCatchup can wrap it with state-flip handling.
//
// Post-ADR-028 the catchup is a single RunCycle call. The
// engine's billSubscription internally loops billOnePeriod until
// each sub catches up to its effectiveNow — N periods compress
// into one operator action. The previous outer loop, the
// MaxAdvanceCatchupLoops cap, and the n==0 early-exit guard all
// went away with that change.
//
// The 10-minute CatchupTimeout still bounds the total operation
// (set by the worker's ctx). Per-sub safety counter
// (maxPeriodsPerSubPerCall in the engine) is the inner ceiling
// for runaway-loop detection.
