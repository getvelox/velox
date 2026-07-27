package dunning

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
)

// RetryError is the adapter's decline wrapper: it carries the Stripe
// PaymentIntent id of the FAILED charge across the domain boundary
// without dunning importing payment types. The timeline uses the id to
// attribute payment_intent.payment_failed webhook rows to their exact
// retry (pre-fix the k-th failure paired with the k-th dunning row by
// index, so a customer's failed hosted-page attempt could stamp ITS
// decline message onto an engine retry's row). Error() delegates so
// event Reason strings are unchanged.
type RetryError struct {
	PaymentIntentID string
	Err             error
}

func (e *RetryError) Error() string { return e.Err.Error() }
func (e *RetryError) Unwrap() error { return e.Err }

// ErrTransientSkip signals that the PaymentRetrier could not attempt a
// charge this tick — the Stripe call never happened, so dunning must NOT
// tick attempt_count or emit a failure event. Typical causes: per-tenant
// circuit breaker open, context timeout fired before the Stripe API call.
// The adapter that bridges payment → dunning maps payment's internal
// sentinel to this dunning-visible one so peer domains don't import each
// other.
var ErrTransientSkip = errors.New("dunning retry skipped: upstream transient")

// PaymentRetrier retries a payment for an invoice.
type PaymentRetrier interface {
	RetryPayment(ctx context.Context, tenantID, invoiceID, stripeCustomerID string) error
}

// SubscriptionPauser pauses collection on a subscription when dunning
// exhausts retries with final_action='pause' (ADR-036 amendment —
// semantics changed to match Stripe's `pause_collection.behavior=
// keep_as_draft`: cycle continues, drafts pile up, no charging /
// dunning until resumed). Pre-amendment this called PauseAtomic
// (hard pause, no further cycles); that was non-Stripe and silently
// skipped invoice generation for the affected periods.
type SubscriptionPauser interface {
	PauseCollection(ctx context.Context, tenantID, id string) error
}

// SubscriptionCanceler cancels a subscription when dunning exhausts
// retries with final_action='cancel_subscription' — Stripe's default
// terminal action; supported by 3 of 4 reference platforms (Stripe,
// Lago, Recurly) per the ADR-036 amendment research.
type SubscriptionCanceler interface {
	Cancel(ctx context.Context, tenantID, id string) error
}

// InvoiceUncollectibleMarker marks an invoice uncollectible when
// dunning exhausts retries with final_action='mark_uncollectible'.
// Stripe-standard terminal: "we won't try again; close out the
// receivable." Replaces the pre-amendment write_off_later semantics
// (the implementation was identical — invoice mutation, no sub
// state-change — but the name was non-standard).
type InvoiceUncollectibleMarker interface {
	MarkUncollectible(ctx context.Context, tenantID, invoiceID string) error
}

// InvoiceGetter gets invoice details for finding the subscription.
type InvoiceGetter interface {
	Get(ctx context.Context, tenantID, id string) (domain.Invoice, error)
}

// CustomerEmailFetcher resolves customer contact info for email notifications.
type CustomerEmailFetcher interface {
	// GetCustomerEmail returns the primary recipient, display name, and
	// the additional-emails CC list (ADR-082).
	GetCustomerEmail(ctx context.Context, tenantID, customerID string) (email, displayName string, additionalEmails []string, err error)
}

// EmailNotifier sends dunning-related emails. publicToken is the
// hosted-invoice URL credential (T0-17) — pass empty when unavailable
// (drafts, pre-addendum invoices); the sender gracefully omits the CTA.
// ctx carries livemode for the underlying enqueue / brand lookup.
type EmailNotifier interface {
	SendPaymentFailed(ctx context.Context, tenantID, to string, cc []string, customerName, invoiceNumber, reason, publicToken string) error
	// SendDunningWarning emails the customer about a failed retry.
	// failureReason is the latest decline reason (from the retrier's
	// error message) — surfaced inline so customers can act
	// (insufficient_funds → top up; lost_card → swap card). Empty
	// reason renders without the diagnostic block. No final-attempt
	// tone exists: the exhausting attempt sends the escalation email
	// instead of a warning (see the warning-enqueue gate in processRun).
	SendDunningWarning(ctx context.Context, tenantID, to string, cc []string, customerName, invoiceNumber string, attemptNumber, maxAttempts int, nextRetryDate, failureReason, publicToken string) error
	SendDunningEscalation(ctx context.Context, tenantID, to string, cc []string, customerName, invoiceNumber, action, publicToken string) error
}

// CustomerPolicyReader returns a customer's assigned dunning_policy_id
// (empty string = no explicit assignment, fall back to tenant default).
// Implemented by *customer.Service so the dunning service can resolve
// the effective policy without importing the customer package.
type CustomerPolicyReader interface {
	GetDunningPolicyID(ctx context.Context, tenantID, customerID string) (string, error)
}

type Service struct {
	store            Store
	retrier          PaymentRetrier
	subPauser        SubscriptionPauser
	subCanceler      SubscriptionCanceler
	invoiceUncollect InvoiceUncollectibleMarker
	invoiceGet       InvoiceGetter
	events           domain.EventDispatcher
	emailNotifier    EmailNotifier
	customerEmail    CustomerEmailFetcher
	custPolicy       CustomerPolicyReader
	resolver         clock.Resolver
	clock            clock.Clock
}

// SetResolver wires the unified clock.Resolver. Once bound on ctx via
// clock.BindEffectiveNow at the entry point of any per-invoice
// dunning operation, every downstream s.clock.Now(ctx) reads
// frozen_time on clock-pinned invoices. Optional: nil leaves binding
// off and every callsite reads wall-clock.
func (s *Service) SetResolver(r clock.Resolver) {
	s.resolver = r
}

// bindForInvoice binds effective-now from an invoice id at every
// dunning state-machine entry point (StartDunning, processRun,
// exhaustRun, ResolveRun, ResolveByInvoice). Returns ctx unchanged on
// resolver error or no resolver — wall-clock fallback.
func (s *Service) bindForInvoice(ctx context.Context, tenantID, invoiceID string) context.Context {
	bound, _ := clock.BindEffectiveNow(ctx, s.resolver, clock.Pin{TenantID: tenantID, InvoiceID: invoiceID})
	return bound
}

func NewService(store Store, retrier PaymentRetrier, clk clock.Clock) *Service {
	if clk == nil {
		clk = clock.Real()
	}
	return &Service{store: store, retrier: retrier, clock: clk}
}

func (s *Service) SetRetrier(retrier PaymentRetrier) {
	s.retrier = retrier
}

// SetCustomerPolicyReader wires the customer→policy_id lookup used by
// GetEffectivePolicyForCustomer. Without it, every dunning operation
// falls back to the tenant default policy (per-customer assignment
// silently ignored). Production must wire this; narrow unit tests
// can leave it nil.
func (s *Service) SetCustomerPolicyReader(r CustomerPolicyReader) {
	s.custPolicy = r
}

// GetEffectivePolicyForCustomer returns the policy that governs this
// customer's next dunning run. Resolution order (ADR-036):
//  1. If the customer has an explicit dunning_policy_id, load that
//     policy by id.
//  2. Otherwise, the tenant's is_default=true policy.
//
// Lookup failures on step 1 (e.g. the assigned policy was deleted
// underneath the customer's pointer; the FK ON DELETE SET NULL safety
// net should prevent this but defensive coding) fall through to the
// default — a missing-policy customer continues to dun rather than
// erroring out at run time.
func (s *Service) GetEffectivePolicyForCustomer(ctx context.Context, tenantID, customerID string) (domain.DunningPolicy, error) {
	if s.custPolicy != nil && customerID != "" {
		if pid, err := s.custPolicy.GetDunningPolicyID(ctx, tenantID, customerID); err == nil && pid != "" {
			if p, err := s.store.GetPolicyByID(ctx, tenantID, pid); err == nil {
				return p, nil
			}
			// Assigned policy not found — fall through to default.
		}
	}
	return s.store.GetDefaultPolicy(ctx, tenantID)
}

// SetSubscriptionPauser configures the pause-collection terminal action
// (ADR-036 amendment — semantics now Stripe-aligned: keep_as_draft,
// not hard pause).
func (s *Service) SetSubscriptionPauser(pauser SubscriptionPauser, invoices InvoiceGetter) {
	s.subPauser = pauser
	s.invoiceGet = invoices
}

// SetSubscriptionCanceler configures the cancel-subscription terminal
// action (ADR-036 amendment).
func (s *Service) SetSubscriptionCanceler(c SubscriptionCanceler) {
	s.subCanceler = c
}

// SetInvoiceUncollectibleMarker configures the mark-uncollectible
// terminal action (ADR-036 amendment).
func (s *Service) SetInvoiceUncollectibleMarker(m InvoiceUncollectibleMarker) {
	s.invoiceUncollect = m
}

// SetEmailNotifier configures email notifications for dunning events.
func (s *Service) SetEmailNotifier(notifier EmailNotifier) {
	s.emailNotifier = notifier
}

// SetCustomerEmailFetcher configures customer email resolution for dunning notifications.
func (s *Service) SetCustomerEmailFetcher(fetcher CustomerEmailFetcher) {
	s.customerEmail = fetcher
}

// SetEventDispatcher configures outbound webhook event firing.
func (s *Service) SetEventDispatcher(events domain.EventDispatcher) {
	s.events = events
}

// fireEvent dispatches a webhook event. Synchronous: with the outbox (RES-1)
// Dispatch is a short DB insert that must persist-before-return so a
// process crash can't silently drop the event.
func (s *Service) fireEvent(ctx context.Context, tenantID, eventType string, payload map[string]any) {
	if s.events == nil {
		return
	}
	if err := s.events.Dispatch(ctx, tenantID, eventType, payload); err != nil {
		slog.Error("dispatch dunning event",
			"event_type", eventType,
			"tenant_id", tenantID,
			"error", err,
		)
	}
}

// StartDunning initiates a dunning run for a failed invoice payment.
//
// One run per invoice, lifetime — Stripe-parity. The previous behaviour
// (idempotent only on ACTIVE runs, allowing a new run after an escalated
// or resolved one) produced duplicates on /dunning?tab=runs whenever
// a re-triggered payment failure landed on an already-terminal invoice.
// Escalated runs are terminal in our state machine; subsequent payment
// failures on the same invoice should NOT start a fresh campaign —
// the operator interprets the existing escalated run as the canonical
// record and resolves it manually if the customer pays.
//
// Returns the existing run regardless of state. The DB UNIQUE index
// (migration 0085) is the belt to this code's suspenders: even if a
// race somehow gets past this check, the INSERT in CreateRun fails
// with a constraint violation.
//
// failureAt is the simulated moment the charge failed — typically the
// invoice's cycle-close instant, NOT wall-clock-or-frozen "now." For
// clock-pinned invoices under catchup, frozen_time is advance-end
// (e.g. May 20) while the charge "actually" failed at the May 1 cycle
// close; anchoring next_action_at on frozen_time would push the first
// retry past advance-end and leave it stranded. The caller is
// responsible for resolving failureAt from invoice period boundaries.
// Pass time.Now() (or s.clock.Now(ctx)) when no period anchor is
// available — that's the wall-clock / manual-invoice case.
//
// cause is WHY collection needs a run — declared by the caller at write
// time (decline paths pass payment_failed; the card-less enrollment
// sweeps pass no_payment_method) and stamped on the run + started
// event. Pre-fix this was hardcoded "payment_failed" for every start,
// so card-less invoices carried a payment-failure they never had; the
// timeline hid the lie by rendering a generic row. An unknown cause is
// refused loudly.
func (s *Service) StartDunning(ctx context.Context, tenantID string, invoiceID, customerID string, failureAt time.Time, cause domain.DunningStartCause) (domain.InvoiceDunningRun, error) {
	if !cause.Valid() {
		return domain.InvoiceDunningRun{}, errs.Invalid("cause", fmt.Sprintf("unknown dunning start cause %q", cause))
	}
	existing, err := s.store.GetRunByInvoice(ctx, tenantID, invoiceID)
	if err == nil && existing.ID != "" {
		return existing, nil // Idempotent — return existing run regardless of state.
	}

	policy, err := s.GetEffectivePolicyForCustomer(ctx, tenantID, customerID)
	if err != nil {
		// No effective policy (tenant has no default configured) is a DELIBERATE
		// SKIP, the same class as a disabled policy — an unconfigured optional
		// feature must never poison the money-path enrollment sweep/catchup (the
		// SMTP-unset precedent). Map ONLY ErrNotFound to InvalidState so all three
		// callers (no-payment sweep, inline decline, post-commit settle) treat it
		// as a no-op via their shared ErrInvalidState swallow. A real infra error
		// (DB down, etc.) still propagates and fails loud — no silent fallback.
		if errors.Is(err, errs.ErrNotFound) {
			slog.WarnContext(ctx, "dunning not configured — no effective policy; skipping enrollment",
				"tenant_id", tenantID, "invoice_id", invoiceID, "customer_id", customerID)
			return domain.InvoiceDunningRun{}, errs.InvalidState("dunning not configured — no policy for tenant")
		}
		return domain.InvoiceDunningRun{}, fmt.Errorf("get effective dunning policy: %w", err)
	}
	if !policy.Enabled {
		return domain.InvoiceDunningRun{}, errs.InvalidState("dunning is disabled (assigned policy or tenant default is not enabled)")
	}

	// Grace period determines when the first retry happens.
	// retry_schedule determines the intervals between subsequent retries.
	firstRetryDelay := time.Duration(policy.GracePeriodDays) * 24 * time.Hour
	if firstRetryDelay <= 0 {
		firstRetryDelay = 24 * time.Hour // minimum 1 day before first retry
	}

	ctx = s.bindForInvoice(ctx, tenantID, invoiceID)
	if failureAt.IsZero() {
		// Defensive fallback — callers should always supply, but a missing
		// timestamp should not blow up dunning. Use the clock's "now",
		// which under catchup is advance-end frozen_time (the same
		// degenerate case we're trying to avoid, but at least the run
		// gets created so the operator can see it).
		failureAt = s.clock.Now(ctx)
	}
	t := failureAt.Add(firstRetryDelay)
	nextActionAt := &t

	run, err := s.store.CreateRun(ctx, tenantID, domain.InvoiceDunningRun{
		InvoiceID:    invoiceID,
		CustomerID:   customerID,
		PolicyID:     policy.ID,
		State:        domain.DunningActive,
		Reason:       string(cause),
		AttemptCount: 0,
		NextActionAt: nextActionAt,
		// CreatedAt = failureAt so the dunning run lives on simulated
		// cycle-close time, not orchestrator frozen_time. Aligns the
		// 'Automatic retry scheduled' row in the invoice timeline with
		// the cycle's period_end.
		CreatedAt: failureAt,
	})
	if err != nil {
		return domain.InvoiceDunningRun{}, fmt.Errorf("create dunning run: %w", err)
	}

	// Record start event at the simulated cycle-close instant so
	// the invoice timeline's 'Automatic retry scheduled' row aligns
	// with the cycle's period_end, not the orchestrator's frozen_time.
	_, _ = s.store.CreateEvent(ctx, tenantID, domain.InvoiceDunningEvent{
		RunID:     run.ID,
		InvoiceID: invoiceID,
		EventType: domain.DunningEventStarted,
		State:     domain.DunningActive,
		Reason:    string(cause),
		CreatedAt: failureAt,
	})

	slog.Info("dunning started", "run_id", run.ID, "invoice_id", invoiceID)

	s.fireEvent(ctx, tenantID, domain.EventDunningStarted, map[string]any{
		"run_id":      run.ID,
		"invoice_id":  invoiceID,
		"customer_id": customerID,
	})

	return run, nil
}

// ProcessDueRuns finds runs due for action and executes retries —
// CRON path. ADR-029 Phase 5: the cron's ListDueRuns excludes
// dunning runs whose owning invoice's sub is clock-pinned; those go
// through ProcessDueRunsForClock during catchup.
func (s *Service) ProcessDueRuns(ctx context.Context, tenantID string, limit int) (int, []error) {
	if limit <= 0 {
		limit = 20
	}
	dueRuns, err := s.store.ListDueRuns(ctx, tenantID, s.clock.Now(ctx), limit)
	if err != nil {
		return 0, []error{fmt.Errorf("list due runs: %w", err)}
	}
	return s.processRunsBatch(ctx, tenantID, dueRuns, false /* wall-clock cron */)
}

// ProcessDueRunsForClock is the catchup-path counterpart. ADR-029
// Phase 5: clock-pinned dunning runs advance only on operator Advance,
// against the clock's frozen_time. The dunning state machine itself
// (processRun) is identical between paths — only the candidate-fetch
// scope differs.
//
// Loops until ListDueRunsForClock returns zero rows or the safety cap
// hits — required because one run can advance through multiple retries
// in a single Advance click. Pre-fix, a single batch only fired the
// retry that was due at query time; the new next_action_at written
// by processRun was never re-queried, so the operator saw at most
// one retry per click even when several were due in the simulated
// window. Stripe Test Clocks parity: one Advance walks every
// time-driven action to completion.
//
// The re-query is bounded by:
//   - maxDunningCatchupIters: prevents pathological infinite loops if
//     processRun leaves a run in a non-progressing state (e.g.
//     persistent transient skip that rewinds attempt_count without
//     advancing next_action_at — would otherwise yield the same row
//     every iteration).
//   - Per-iteration progress check: if a run reappears with the same
//     attempt_count it had on the previous iteration, it didn't
//     advance — bail to avoid spinning.
const maxDunningCatchupIters = 50

func (s *Service) ProcessDueRunsForClock(ctx context.Context, tenantID, clockID string, frozenTime time.Time, limit int) (int, []error) {
	if limit <= 0 {
		limit = 20
	}
	total := 0
	var allErrs []error
	seen := make(map[string]int) // run_id → last-iteration attempt_count
	for iter := range maxDunningCatchupIters {
		if err := ctx.Err(); err != nil {
			return total, append(allErrs, fmt.Errorf("dunning catchup ctx done: %w", err))
		}
		dueRuns, err := s.store.ListDueRunsForClock(ctx, tenantID, clockID, frozenTime, limit)
		if err != nil {
			return total, append(allErrs, fmt.Errorf("list due runs for clock %s: %w", clockID, err))
		}
		if len(dueRuns) == 0 {
			return total, allErrs
		}
		if iter > 0 {
			anyProgress := false
			for _, r := range dueRuns {
				if r.AttemptCount > seen[r.ID] {
					anyProgress = true
					break
				}
			}
			if !anyProgress {
				slog.Warn("dunning catchup loop made no progress — exiting",
					"clock_id", clockID, "iter", iter, "remaining_due", len(dueRuns))
				return total, allErrs
			}
		}
		for _, r := range dueRuns {
			seen[r.ID] = r.AttemptCount
		}
		n, errs := s.processRunsBatch(ctx, tenantID, dueRuns, true /* test-clock catchup */)
		total += n
		allErrs = append(allErrs, errs...)
	}
	slog.Warn("dunning catchup loop hit safety cap — remaining runs deferred to next Advance",
		"clock_id", clockID, "cap", maxDunningCatchupIters, "processed", total)
	return total, allErrs
}

// processRunsBatch is the shared per-run body of ProcessDueRuns and
// ProcessDueRunsForClock. The candidate list shape differs by trigger;
// the per-run state-machine step is identical.
func (s *Service) processRunsBatch(ctx context.Context, tenantID string, dueRuns []domain.InvoiceDunningRun, isCatchup bool) (int, []error) {
	processed := 0
	var runErrs []error
	for _, run := range dueRuns {
		if err := s.processRun(ctx, tenantID, run, isCatchup); err != nil {
			runErrs = append(runErrs, fmt.Errorf("run %s: %w", run.ID, err))
			continue
		}
		processed++
	}
	return processed, runErrs
}

func (s *Service) processRun(ctx context.Context, tenantID string, run domain.InvoiceDunningRun, isCatchup bool) error {
	// Resolve the policy bound to this run at StartDunning time.
	// Runs stay on their original policy for their lifetime — if the
	// customer's dunning_policy_id assignment changed mid-flight (or
	// the assigned policy was edited), in-flight runs continue with
	// the original config and only the NEXT run for this customer
	// picks up the new policy. Stripe-Lago shape (verified during
	// ADR-036 research — no platform switches a mid-flight retry
	// schedule under the operator's feet).
	policy, err := s.store.GetPolicyByID(ctx, tenantID, run.PolicyID)
	if err != nil {
		return fmt.Errorf("get bound policy %s for run %s: %w", run.PolicyID, run.ID, err)
	}

	if run.Paused {
		return nil // Skip paused runs
	}

	// Paid-pre-check — the durable backstop for an invoice settled OUT-OF-BAND
	// (a credit-cover sweep MarkPaids the invoice without resolving its run, or
	// any settle path the prompt-resolve doesn't instrument). Resolve the run in
	// place instead of retrying. CRITICALLY placed BEFORE the max-retries →
	// exhaustRun branch below: exhaustRun's terminal action can pause-collection
	// or CANCEL THE SUBSCRIPTION with no paid-check, so a max-retries run on a
	// now-paid invoice would otherwise cancel a paying customer's subscription.
	// Gate on terminal STATUS (not amount_due<=0, which a mid-tax-retry draft can
	// momentarily show). On a fetch error, fall through to the normal path (which
	// itself short-circuits at $0) — don't burn an attempt on a DB blip.
	if s.invoiceGet != nil {
		if inv, err := s.invoiceGet.Get(ctx, tenantID, run.InvoiceID); err == nil {
			switch {
			case inv.Status == domain.InvoicePaid || inv.PaymentStatus == domain.PaymentSucceeded:
				_, rerr := s.resolveRunNow(ctx, tenantID, run, domain.ResolutionPaymentRecovered, "payment_recovered")
				return rerr
			case inv.Status == domain.InvoiceVoided || inv.Status == domain.InvoiceUncollectible:
				_, rerr := s.resolveRunNow(ctx, tenantID, run, domain.ResolutionManuallyResolved, "invoice_"+string(inv.Status))
				return rerr
			}
		}
	}

	// Check if max retries exhausted
	if run.AttemptCount >= policy.MaxRetryAttempts {
		return s.exhaustRun(ctx, tenantID, run, policy, s.clock.Now(ctx))
	}

	// Attempt retry
	run.AttemptCount++
	prevLastAttemptAt := run.LastAttemptAt // for the transient-skip rewind below
	var pinned bool
	ctx, pinned = clock.BindEffectiveNow(ctx, s.resolver, clock.Pin{TenantID: tenantID, InvoiceID: run.InvoiceID})
	// Anchor this attempt's instant.
	//
	// Simulated time (test-clock catchup, or a clock-pinned customer): anchor
	// on the simulated moment the retry was scheduled for (run.NextActionAt)
	// rather than the orchestrator's frozen_time. Without this, every retry
	// under catchup gets last_attempt_at = advance-end frozen_time, and the
	// next retry is scheduled at frozen_time + interval (always past
	// advance-end, so it never fires in the same Advance click). Anchoring on
	// NextActionAt walks the state machine through simulated time.
	//
	// Pure wall-clock cron (not catchup, not pinned): anchor on
	// max(now, NextActionAt). In steady state NextActionAt == now. But after
	// the scheduler is down for several intervals, NextActionAt is
	// stale-in-the-past; anchoring on it would schedule the next retry at
	// staleNextActionAt + interval — still in the past — so the whole backlog
	// fires back-to-back in one tick, collapsing the configured cadence.
	// Clamping to now resumes the cadence from the recovery instant.
	now := s.clock.Now(ctx)
	if run.NextActionAt != nil {
		if isCatchup || pinned || run.NextActionAt.After(now) {
			now = *run.NextActionAt
		}
	}
	run.LastAttemptAt = &now

	// Rebind ctx's instant to the anchored retry time while keeping the
	// clock (WithSim, never WithEffectiveNow — that clears the clock id).
	// Everything downstream of this retry — the charge (which stamps the
	// anchor into PI metadata for the webhook settle), credit applies,
	// the inline settle's paid_at — inherits the CONTRACTED instant
	// instead of the catchup's advance-end frozen_time. Wall-clock runs
	// carry no Sim binding and are untouched.
	if sim, ok := clock.SimOf(ctx); ok {
		ctx = clock.WithSim(ctx, clock.Sim{At: now, TestClockID: sim.TestClockID})
	}

	// Record the attempt BEFORE the charge (record-before-effect): a resolver that
	// fires synchronously inside RetryPayment — a card success that settles this
	// invoice — re-reads the run from the store, so it must see the FULL
	// attempt_count; and a crash mid-charge then cannot lose the attempt (we won't
	// under-retry). The transient-skip branch below rewinds this if the charge never
	// actually happened. Guarded on state <> 'resolved': if a concurrent settle
	// resolved this run between the paid-pre-check above and here, don't clobber it
	// back to active — and don't charge an already-resolved run — just stop.
	if applied, err := s.store.UpdateRunIfActive(ctx, tenantID, run); err != nil {
		return fmt.Errorf("persist dunning attempt before retry: %w", err)
	} else if !applied {
		slog.Info("dunning retry skipped — run resolved concurrently before the charge",
			"run_id", run.ID, "invoice_id", run.InvoiceID)
		return nil
	}

	// Actually retry the payment
	retryErr := fmt.Errorf("payment retrier not configured")
	if s.retrier != nil {
		retryErr = s.retrier.RetryPayment(ctx, tenantID, run.InvoiceID, run.CustomerID)
	}

	// Transient skip: the Stripe call never happened (circuit breaker open or
	// timeout before the call). This is NOT a dunning attempt — rewind the attempt we
	// recorded above (count + last_attempt_at) so the run looks as it did before this
	// tick, don't log a failure event or reschedule, and let the next scheduler tick
	// retry. A five-minute Stripe outage should not burn a tenant's entire retry
	// budget.
	if errors.Is(retryErr, ErrTransientSkip) {
		run.AttemptCount--
		run.LastAttemptAt = prevLastAttemptAt
		// GUARDED rewind. ErrTransientSkip also covers the ambiguous
		// PI-may-have-succeeded outcome (client saw a 5xx/timeout but the charge
		// actually went through): its webhook may have resolved this run during the
		// charge window. UpdateRunIfActive no-ops on a resolved run, so we never
		// clobber that resolve back to active (which would re-fire dunning.resolved).
		// A failed rewind on a still-active run leaves the count one high — the SAFE
		// direction: the exhaustion gate uses `>=`, so an over-count can only end a
		// retry cycle early, never over-retry.
		if _, err := s.store.UpdateRunIfActive(ctx, tenantID, run); err != nil {
			slog.Warn("dunning transient-skip: failed to rewind the pre-charge attempt persist",
				"run_id", run.ID, "invoice_id", run.InvoiceID, "error", err)
		}
		slog.Info("dunning retry skipped — upstream transient (breaker/timeout)",
			"run_id", run.ID, "invoice_id", run.InvoiceID)
		return nil
	}

	if retryErr != nil {
		run.State = domain.DunningActive // Will retry again later
		slog.Warn("dunning retry failed",
			"run_id", run.ID,
			"invoice_id", run.InvoiceID,
			"attempt", run.AttemptCount,
			"error", retryErr,
		)

		// Record failed retry event at this retry's simulated instant
		// (= run.NextActionAt at fire time, captured into `now` above)
		// so each retry row on the invoice timeline carries its own
		// scheduled timestamp instead of all sharing frozen_time.
		// Attribution key for the invoice timeline: the failed charge's
		// PI id, when the adapter surfaced it (RetryError). Legacy rows
		// without it fall back to index pairing in the timeline merge.
		var evtMeta map[string]any
		var rerr *RetryError
		if errors.As(retryErr, &rerr) && rerr.PaymentIntentID != "" {
			evtMeta = map[string]any{"payment_intent_id": rerr.PaymentIntentID}
		}
		_, _ = s.store.CreateEvent(ctx, tenantID, domain.InvoiceDunningEvent{
			RunID:        run.ID,
			InvoiceID:    run.InvoiceID,
			EventType:    domain.DunningEventRetryAttempted,
			State:        domain.DunningActive,
			AttemptCount: run.AttemptCount,
			Reason:       retryErr.Error(),
			Metadata:     evtMeta,
			CreatedAt:    now,
		})

		// The dunning warning email is enqueued further down, AFTER the
		// next-retry reschedule — see the comment there. Enqueuing it
		// here rendered "We'll try again on <date>" from a NextActionAt
		// still holding the JUST-FAILED attempt's scheduled instant.
	} else {
		slog.Info("dunning resolved — payment succeeded",
			"run_id", run.ID,
			"invoice_id", run.InvoiceID,
			"attempt", run.AttemptCount,
		)
		// The SUCCESSFUL retry gets its own timeline row too — pre-fix
		// only failures wrote retry_attempted, so a run resolved "after
		// 3 retry attempts" showed 2 retry rows and the recovering
		// attempt's instant appeared nowhere on the invoice timeline.
		_, _ = s.store.CreateEvent(ctx, tenantID, domain.InvoiceDunningEvent{
			RunID:        run.ID,
			InvoiceID:    run.InvoiceID,
			EventType:    domain.DunningEventRetryAttempted,
			State:        domain.DunningResolved,
			AttemptCount: run.AttemptCount,
			Reason:       "succeeded",
			CreatedAt:    now,
		})
		// Resolve AT the retry's contracted instant (`now` = the anchored
		// run.NextActionAt) — resolveRunNow's own clock read is the
		// advance-end frozen_time under catchup, which stamped a Mar 7
		// recovery "resolved Apr 1" when the operator advanced a month in
		// one click (the contracted-instant class, 4th sighting).
		_, rerr := s.resolveRunAt(ctx, tenantID, run, domain.ResolutionPaymentRecovered, "payment_recovered", &now)
		return rerr
	}

	// Schedule next retry.
	// retry_schedule contains the intervals between retries:
	//   retry_schedule[0] = gap between retry 1 and retry 2
	//   retry_schedule[1] = gap between retry 2 and retry 3
	// Grace period (used in StartDunning) determines when retry 1 happens.
	//
	// Save-time validation (Service.UpsertPolicy) guarantees the schedule
	// has at least (MaxRetryAttempts - 1) entries AND that every entry
	// parses as a positive Go duration (an unparseable entry here would
	// error after the attempt persisted but before the reschedule landed,
	// wedging the run — the FLOW TC5 "3d" livelock). The pre-ADR-036
	// "reuse last interval when idx out of bounds" runtime fallback was
	// removed — that was a silent fallback (feedback_no_silent_fallbacks);
	// out-of-bounds here now indicates a schema invariant violation, so
	// fail loudly rather than substitute a default.
	if run.AttemptCount < policy.MaxRetryAttempts {
		idx := run.AttemptCount - 1 // 1-based; schedule[0] = after retry 1
		if idx < 0 || idx >= len(policy.RetrySchedule) {
			return fmt.Errorf("retry_schedule index %d out of bounds for policy %s (schedule has %d entries, max_retry_attempts %d) — save-time validation must enforce schedule length",
				idx, policy.ID, len(policy.RetrySchedule), policy.MaxRetryAttempts)
		}
		d, err := time.ParseDuration(policy.RetrySchedule[idx])
		if err != nil {
			return fmt.Errorf("parse retry_schedule[%d]=%q for policy %s: %w", idx, policy.RetrySchedule[idx], policy.ID, err)
		}
		t := now.Add(d)
		run.NextActionAt = &t
	} else {
		run.NextActionAt = nil
	}

	if applied, err := s.store.UpdateRunIfActive(ctx, tenantID, run); err != nil {
		return err
	} else if !applied {
		// Concurrently resolved during the failed-charge window (a non-charge settle
		// such as an operator resolve or credit-cover sweep) — don't reschedule or
		// exhaust a resolved run, and don't clobber the resolve back to active.
		// No warning email either — "we'll try again" to a customer who just paid.
		return nil
	}

	// Dunning warning email for a non-final failed retry. Positioned AFTER
	// the reschedule above so run.NextActionAt carries the ACTUAL next-retry
	// instant — pre-fix the email was enqueued before the reschedule, so
	// "We'll try again on <date>" rendered the JUST-FAILED attempt's
	// scheduled time (after scheduler downtime, a date in the past).
	//
	// Skipped when this attempt exhausted the budget — exhaustRun below
	// fires the terminal escalation email instead. A "we'll retry on [no
	// more retries scheduled]" warning is confusing and stacks two back-to-
	// back emails on the customer. Customer experience: N-1 warnings during
	// retries 1..N-1, then ONE escalation on retry N. Catchup-mode
	// experience is the same, just compressed in real time.
	//
	// Synchronous enqueue (DB insert via the email outbox). Pre-fix this
	// ran in a goroutine bound to the parent ctx, which under test-clock
	// catchup gets canceled the instant RunCatchup returns
	// (testclock/catchup.go's `defer cancel()`) — goroutines spawned at
	// the tail of the catchup pass lost the race and never enqueued, even
	// though the dunning state was correctly transitioned. The SMTP send
	// remains async via the email outbox dispatcher worker.
	if run.AttemptCount < policy.MaxRetryAttempts && s.emailNotifier != nil && s.customerEmail != nil {
		// The failure reason renders INSIDE the customer email. A no-PM
		// retry failure is internal phrasing ("no payment method for
		// customer") — map it to empty so the warning renders without the
		// diagnostic callout instead of leaking system-perspective copy
		// into a customer inbox (2026-07-22 payment-surfacing audit,
		// P2-3). Real decline reasons (insufficient_funds, lost_card)
		// pass through — the customer can act on those.
		reason := retryErr.Error()
		if errors.Is(retryErr, domain.ErrNoPaymentMethodOnRetry) {
			reason = ""
		}
		s.enqueueDunningWarning(ctx, tenantID, run, policy, reason)
	}

	// Check if exhausted after this attempt. Pass the simulated instant
	// of this final retry (= run.NextActionAt at fire time, captured
	// into `now` above) so the escalated event row aligns with the
	// retry that actually triggered the exhaustion, not orchestrator
	// frozen_time.
	if run.AttemptCount >= policy.MaxRetryAttempts {
		return s.exhaustRun(ctx, tenantID, run, policy, now)
	}

	return nil
}

// exhaustRun finalizes a dunning run after its last retry failed (or
// it was found already at-or-beyond max attempts on entry). firedAt
// is the simulated instant of the triggering retry — used as the
// run's resolved_at and the escalated event's CreatedAt so the
// invoice timeline shows the escalation aligned with the retry that
// actually caused it, not at orchestrator frozen_time.
func (s *Service) exhaustRun(ctx context.Context, tenantID string, run domain.InvoiceDunningRun, policy domain.DunningPolicy, firedAt time.Time) error {
	ctx = s.bindForInvoice(ctx, tenantID, run.InvoiceID)
	now := firedAt
	if now.IsZero() {
		now = s.clock.Now(ctx)
	}

	// Late paid re-check — the terminal final_action (cancel_subscription in
	// particular) is IRREVERSIBLE, and the tick-start paid-pre-check goes stale
	// across the retry's Stripe round-trip: an invoice that settled out-of-band
	// mid-tick would otherwise cancel a paying customer's subscription here. Re-
	// read invoice status as late as possible and, if it is now terminal, resolve
	// the run (CAS-safe) instead of firing the terminal action. Same STATUS gate
	// as the tick-start check; on a fetch error fall through (a DB blip must not
	// strand an exhausted run — the guarded writes below still protect a resolve
	// that lands during the action itself).
	if s.invoiceGet != nil {
		if inv, err := s.invoiceGet.Get(ctx, tenantID, run.InvoiceID); err == nil {
			switch {
			case inv.Status == domain.InvoicePaid || inv.PaymentStatus == domain.PaymentSucceeded:
				_, rerr := s.resolveRunNow(ctx, tenantID, run, domain.ResolutionPaymentRecovered, "payment_recovered")
				return rerr
			case inv.Status == domain.InvoiceVoided || inv.Status == domain.InvoiceUncollectible:
				_, rerr := s.resolveRunNow(ctx, tenantID, run, domain.ResolutionManuallyResolved, "invoice_"+string(inv.Status))
				return rerr
			}
		}
	}

	// actionFailed records whether the terminal final_action mover errored.
	// The run's terminal state is assigned AFTER the switch so a swallowed
	// mover failure leaves the run requeryable (state=active) instead of a
	// clean "escalated" beside an invoice/sub that never got closed —
	// escalated runs are permanently excluded from both due-run pickers, so
	// they'd never be re-attempted.
	actionFailed := false

	// emailAction is what the escalation email renders. It starts as the
	// policy's final action and is cleared to "" (neutral copy) when the
	// action did not apply to THIS invoice — a one-off invoice has no
	// subscription to cancel or pause, and an unwired mover can't act.
	// The debtor must never read "your subscription has been canceled"
	// when nothing was canceled.
	emailAction := string(policy.FinalAction)

	switch policy.FinalAction {
	case domain.DunningActionManualReview:
		// State stays at escalated; resolution stays retries_exhausted.
		// Operator handles via the dashboard. Stripe "Keep active"
		// equivalent.

	case domain.DunningActionPause:
		// Pause COLLECTION (keep_as_draft) — cycle keeps drafting,
		// no charging / dunning until the operator resumes. Matches
		// Stripe's pause_collection.behavior=keep_as_draft. The pre-
		// ADR-036-amendment implementation called hard PauseAtomic,
		// which silently skipped invoice generation for the affected
		// periods — non-Stripe and destructive.
		if s.subPauser != nil && s.invoiceGet != nil {
			inv, err := s.invoiceGet.Get(ctx, tenantID, run.InvoiceID)
			switch {
			case err != nil:
				// Can't tell whether a subscription exists — a failed
				// action, so the run stays requeryable and re-attempts
				// instead of escalating (and emailing "paused") on a
				// DB blip.
				actionFailed = true
				slog.Warn("failed to load invoice for dunning pause action",
					"invoice_id", run.InvoiceID, "error", err)
			case inv.SubscriptionID == "":
				// One-off invoice — nothing to pause; neutral email copy.
				emailAction = ""
			default:
				if err := s.subPauser.PauseCollection(ctx, tenantID, inv.SubscriptionID); err != nil {
					actionFailed = true
					slog.Warn("failed to pause collection after dunning exhausted",
						"invoice_id", run.InvoiceID, "subscription_id", inv.SubscriptionID, "error", err)
				} else {
					slog.Info("collection paused by dunning",
						"invoice_id", run.InvoiceID, "subscription_id", inv.SubscriptionID)
				}
			}
		} else {
			emailAction = "" // no pauser wired — nothing was paused
		}

	case domain.DunningActionCancelSubscription:
		// Cancel the subscription. Stripe-default terminal action;
		// supported by 3 of 4 reference platforms (Stripe, Lago,
		// Recurly) per the ADR-036 amendment research.
		if s.subCanceler != nil && s.invoiceGet != nil {
			inv, err := s.invoiceGet.Get(ctx, tenantID, run.InvoiceID)
			switch {
			case err != nil:
				// Can't tell whether a subscription exists — a failed
				// action, so the run stays requeryable and re-attempts
				// instead of escalating (and emailing "canceled") on a
				// DB blip.
				actionFailed = true
				slog.Warn("failed to load invoice for dunning cancel action",
					"invoice_id", run.InvoiceID, "error", err)
			case inv.SubscriptionID == "":
				// One-off invoice — nothing to cancel; neutral email copy.
				emailAction = ""
			default:
				if err := s.subCanceler.Cancel(ctx, tenantID, inv.SubscriptionID); err != nil {
					actionFailed = true
					slog.Warn("failed to cancel subscription after dunning exhausted",
						"invoice_id", run.InvoiceID, "subscription_id", inv.SubscriptionID, "error", err)
				} else {
					slog.Info("subscription canceled by dunning",
						"invoice_id", run.InvoiceID, "subscription_id", inv.SubscriptionID)
				}
			}
		} else {
			emailAction = "" // no canceler wired — nothing was canceled
		}

	case domain.DunningActionMarkUncollectible:
		// Mark the unpaid invoice as uncollectible. Stripe-standard
		// for "we won't try again; close out the receivable." The
		// subscription itself stays active — operator may
		// independently cancel via the dashboard.
		if s.invoiceUncollect != nil {
			if err := s.invoiceUncollect.MarkUncollectible(ctx, tenantID, run.InvoiceID); err != nil {
				actionFailed = true
				slog.Warn("failed to mark invoice uncollectible after dunning exhausted",
					"invoice_id", run.InvoiceID, "error", err)
			} else {
				slog.Info("invoice marked uncollectible by dunning",
					"invoice_id", run.InvoiceID)
			}
		} else {
			emailAction = "" // no mover wired — the invoice was NOT closed
		}

	default:
		slog.Warn("unknown dunning final_action — leaving run escalated without state-change action",
			"final_action", policy.FinalAction, "run_id", run.ID)
	}

	if actionFailed {
		// The terminal action errored — keep the run requeryable so the
		// due-run picker re-attempts it (escalated runs are excluded from
		// both pickers and would never retry). AttemptCount is unchanged, so
		// the clock-catchup no-progress guard still exits — no infinite loop
		// within an advance; the re-attempt lands on a later due tick.
		run.State = domain.DunningActive
		run.Resolution = domain.ResolutionActionFailed
		run.ResolvedAt = nil
		retryAt := now.Add(24 * time.Hour)
		run.NextActionAt = &retryAt
		// Guarded: a settle may have resolved this run during the failed terminal
		// action. Don't clobber that resolve back to active — the invoice is paid,
		// so resolved is the correct terminal state and no re-attempt is needed.
		if applied, err := s.store.UpdateRunIfActive(ctx, tenantID, run); err != nil {
			return err
		} else if !applied {
			slog.Info("dunning final_action failed but run resolved concurrently — leaving it resolved",
				"run_id", run.ID, "invoice_id", run.InvoiceID)
			return nil
		}
		slog.Warn("dunning final_action failed; run kept active for re-attempt",
			"run_id", run.ID, "invoice_id", run.InvoiceID, "final_action", policy.FinalAction)
		return nil
	}

	run.State = domain.DunningEscalated
	run.Resolution = domain.ResolutionRetriesExhausted
	run.ResolvedAt = &now
	run.NextActionAt = nil

	// Guarded escalation write. If a settle resolved this run during the
	// (successful) terminal action, don't clobber resolved→escalated or emit a
	// contradictory dunning.escalated on top of the settle's dunning.resolved.
	// The timeline row, escalation email, and webhook below fire only when THIS
	// call actually escalated the run.
	if applied, err := s.store.UpdateRunIfActive(ctx, tenantID, run); err != nil {
		return err
	} else if !applied {
		slog.Info("dunning exhaust: run resolved concurrently during the terminal action — not escalating",
			"run_id", run.ID, "invoice_id", run.InvoiceID)
		return nil
	}

	_, _ = s.store.CreateEvent(ctx, tenantID, domain.InvoiceDunningEvent{
		RunID:        run.ID,
		InvoiceID:    run.InvoiceID,
		EventType:    domain.DunningEventEscalated,
		State:        run.State,
		AttemptCount: run.AttemptCount,
		Reason:       string(policy.FinalAction),
		CreatedAt:    now,
	})

	slog.Info("dunning exhausted",
		"run_id", run.ID,
		"invoice_id", run.InvoiceID,
		"final_action", policy.FinalAction,
	)

	// Synchronous enqueue — see comment on enqueueDunningWarning. Renders
	// emailAction (the action that actually applied to this invoice), not
	// policy.FinalAction — see the note above the switch.
	if s.emailNotifier != nil && s.customerEmail != nil {
		s.enqueueDunningEscalation(ctx, tenantID, run, emailAction)
	}

	s.fireEvent(ctx, tenantID, domain.EventDunningEscalated, map[string]any{
		"run_id":       run.ID,
		"invoice_id":   run.InvoiceID,
		"customer_id":  run.CustomerID,
		"final_action": string(policy.FinalAction),
		"resolution":   string(run.Resolution),
		"attempts":     run.AttemptCount,
	})

	return nil
}

// ResolveRun marks a dunning run as resolved (e.g., after manual payment).
//
// When resolution is invoice_not_collectible, the underlying invoice is
// ALSO flipped to status=uncollectible — same downstream contract as
// the automated mark_uncollectible final-action. Pre-fix the two flows
// diverged: an operator picking "Write off invoice" in the resolve
// dialog only updated the dunning_runs row, leaving the invoice in
// status=finalized + payment_status=failed and every UI gate keyed
// off invoice.status still treating it as live. Cross-flow audit per
// feedback_audit_overlapping_flows: same operator intent ("we're not
// collecting"), same end state required.
//
// The invoice-side write is best-effort and logged on failure rather
// than rolled back — the run's resolved state is itself useful for the
// dunning history view, and a wrapper transaction across two domains
// is heavier than the rarity warrants. If MarkUncollectible 4xxs
// (e.g. invoice was already paid via webhook between dialog open and
// submit), we surface the error to the caller and the operator can
// reconcile.
func (s *Service) ResolveRun(ctx context.Context, tenantID, runID string, resolution domain.DunningResolution) (domain.InvoiceDunningRun, error) {
	run, err := s.store.GetRun(ctx, tenantID, runID)
	if err != nil {
		return domain.InvoiceDunningRun{}, err
	}

	// Resolve via the shared CAS path so an operator resolve racing an automated
	// resolver (a card settle or the scheduler) fires dunning.resolved exactly once —
	// the CAS winner owns the timeline row + the outbound webhook.
	updated, err := s.resolveRunNow(ctx, tenantID, run, resolution, string(resolution))
	if err != nil {
		return domain.InvoiceDunningRun{}, err
	}

	if resolution == domain.ResolutionInvoiceNotCollectible && s.invoiceUncollect != nil {
		if err := s.invoiceUncollect.MarkUncollectible(ctx, tenantID, run.InvoiceID); err != nil {
			// Already-uncollectible is benign (race with automated
			// final_action). Surface other errors so the operator
			// knows the dunning run was resolved but the invoice
			// didn't transition.
			if !errors.Is(err, errs.ErrInvalidState) {
				return updated, fmt.Errorf("mark invoice uncollectible: %w", err)
			}
		}
	}

	return updated, nil
}

// resolveRunNow transitions an already-loaded run to resolved with the given
// resolution: stamps state/resolution/resolved_at, clears next_action_at, writes
// the DunningEventResolved row, persists, and fires EventDunningResolved. Binds
// the clock to the invoice so resolved_at lands in simulated time on clock-pinned
// invoices. Shared by ResolveByInvoice, the processRun success branch, and the
// processRun paid-pre-check so the transition is identical across all of them.
func (s *Service) resolveRunNow(ctx context.Context, tenantID string, run domain.InvoiceDunningRun, resolution domain.DunningResolution, eventReason string) (domain.InvoiceDunningRun, error) {
	return s.resolveRunAt(ctx, tenantID, run, resolution, eventReason, nil)
}

// resolveRunAt is resolveRunNow with an explicit resolve instant. at nil
// derives the clock (settle/operator paths — their contract IS "now");
// the retry-success path passes its anchored contracted instant
// (run.NextActionAt at fire time) so resolved_at and the resolved
// timeline row land on the retry that recovered the invoice, not on
// wherever the clock advance happened to end (ADR-030 contracted-instant
// rule; same split as trial flips, scheduled cancels, pause resumes).
func (s *Service) resolveRunAt(ctx context.Context, tenantID string, run domain.InvoiceDunningRun, resolution domain.DunningResolution, eventReason string, at *time.Time) (domain.InvoiceDunningRun, error) {
	ctx = s.bindForInvoice(ctx, tenantID, run.InvoiceID)
	now := s.clock.Now(ctx)
	if at != nil {
		now = *at
	}
	run.State = domain.DunningResolved
	run.Resolution = resolution
	run.ResolvedAt = &now
	run.NextActionAt = nil

	// CAS the resolve transition FIRST so the side-effects below fire exactly once.
	// If another path already resolved this run — e.g. the card-settle resolve firing
	// just before processRun's own resolve on a synchronous retry-success — this call
	// loses the CAS and no-ops, so integrators get exactly one dunning.resolved and
	// the timeline shows one resolved row per recovery.
	won, err := s.store.ResolveRun(ctx, tenantID, run)
	if err != nil {
		return domain.InvoiceDunningRun{}, err
	}
	if !won {
		// Already resolved by another path — return the ACTUAL persisted state so a
		// caller (Service.ResolveRun) doesn't report a resolution it didn't apply.
		if current, gerr := s.store.GetRun(ctx, tenantID, run.ID); gerr == nil {
			return current, nil
		}
		return run, nil
	}

	// Only the CAS winner writes the (non-idempotent) resolved timeline row + fires
	// the outbound dunning.resolved webhook.
	_, _ = s.store.CreateEvent(ctx, tenantID, domain.InvoiceDunningEvent{
		RunID:        run.ID,
		InvoiceID:    run.InvoiceID,
		EventType:    domain.DunningEventResolved,
		State:        domain.DunningResolved,
		AttemptCount: run.AttemptCount,
		Reason:       eventReason,
		CreatedAt:    now,
	})

	s.fireEvent(ctx, tenantID, domain.EventDunningResolved, map[string]any{
		"run_id":      run.ID,
		"invoice_id":  run.InvoiceID,
		"customer_id": run.CustomerID,
		"resolution":  string(run.Resolution),
	})
	return run, nil
}

// ResolveByInvoice resolves any active dunning run for the given invoice.
// Called when an invoice is voided or paid outside of dunning.
func (s *Service) ResolveByInvoice(ctx context.Context, tenantID, invoiceID string, resolution domain.DunningResolution) error {
	run, err := s.store.GetActiveRunByInvoice(ctx, tenantID, invoiceID)
	if err != nil {
		return nil // No active run — nothing to resolve
	}
	_, rerr := s.resolveRunNow(ctx, tenantID, run, resolution, fmt.Sprintf("invoice %s", string(resolution)))
	return rerr
}

// GetDefaultPolicy returns the tenant's default dunning policy.
// Operators / handlers needing "the current policy" use this; runtime
// dunning state machine instead resolves per-customer via
// GetEffectivePolicyForCustomer or per-run via GetPolicyByID(run.PolicyID).
func (s *Service) GetDefaultPolicy(ctx context.Context, tenantID string) (domain.DunningPolicy, error) {
	return s.store.GetDefaultPolicy(ctx, tenantID)
}

// GetPolicyByID looks up a policy by id.
func (s *Service) GetPolicyByID(ctx context.Context, tenantID, id string) (domain.DunningPolicy, error) {
	return s.store.GetPolicyByID(ctx, tenantID, id)
}

// ListPolicies returns all policies for the tenant (default first, then
// created_at). Drives the campaigns admin page.
func (s *Service) ListPolicies(ctx context.Context, tenantID string) ([]domain.DunningPolicy, error) {
	return s.store.ListPolicies(ctx, tenantID)
}

// DeletePolicy removes a policy by id. Refuses to delete the default
// policy or any policy with customers explicitly assigned to it (the
// store-level checks are authoritative; service-level pre-check just
// produces a clearer error before hitting the DB).
func (s *Service) DeletePolicy(ctx context.Context, tenantID, id string) error {
	return s.store.DeletePolicy(ctx, tenantID, id)
}

// SetDefaultPolicy promotes a policy to is_default and demotes the
// previous default atomically.
func (s *Service) SetDefaultPolicy(ctx context.Context, tenantID, id string) error {
	return s.store.SetDefaultPolicy(ctx, tenantID, id)
}

// CountCustomersOnPolicy returns the explicit-assignment count for a
// policy, used by the admin UI ("N customers assigned" badge) and by
// the delete guard.
func (s *Service) CountCustomersOnPolicy(ctx context.Context, tenantID, policyID string) (int, error) {
	return s.store.CountCustomersOnPolicy(ctx, tenantID, policyID)
}

// UpsertPolicy creates a new policy (when input.ID is empty) or updates
// an existing one (when input.ID is set). Save-time validation enforces
// the per-platform invariant that retry_schedule has enough entries to
// cover the max-attempts count — without this guard a misconfig (max=5
// with 2-entry schedule) would silently reuse the last interval at
// runtime, producing back-to-back retries the operator never asked for
// (a feedback_no_silent_fallbacks violation; the pre-ADR-036 runtime
// fallback `idx >= len(retryIntervals)` was removed alongside this).
func (s *Service) UpsertPolicy(ctx context.Context, tenantID string, policy domain.DunningPolicy) (domain.DunningPolicy, error) {
	policy, err := normalizeAndValidatePolicy(policy)
	if err != nil {
		return domain.DunningPolicy{}, err
	}
	return s.store.UpsertPolicy(ctx, tenantID, policy)
}

// UpsertPolicyTx is the tx-aware upsert used by the recipe-instantiate path.
// It runs the SAME normalize+validate as UpsertPolicy — the recipe schema layer
// validates only final_action (recipe/parse.go), NOT the schedule-length
// invariant, so skipping validation here let a mismatched recipe (max_retries
// exceeding its intervals_hours count) persist and stall that campaign at
// retry-time in a background tick ("retry_schedule index out of bounds")
// instead of failing loudly at instantiate-time. One validation chokepoint for
// every writer; a policy the API path would reject must never enter via a
// recipe. (The pre-fix comment claiming the recipe layer "already validated"
// was wrong for this invariant.)
func (s *Service) UpsertPolicyTx(ctx context.Context, tx *sql.Tx, tenantID string, policy domain.DunningPolicy) (domain.DunningPolicy, error) {
	policy, err := normalizeAndValidatePolicy(policy)
	if err != nil {
		return domain.DunningPolicy{}, err
	}
	return s.store.UpsertPolicyTx(ctx, tx, tenantID, policy)
}

// normalizeAndValidatePolicy applies the save-time defaults and enforces the
// policy invariants. The single chokepoint shared by UpsertPolicy and
// UpsertPolicyTx so no writer can persist a policy another path would reject.
func normalizeAndValidatePolicy(policy domain.DunningPolicy) (domain.DunningPolicy, error) {
	if policy.MaxRetryAttempts <= 0 {
		policy.MaxRetryAttempts = 3
	}
	if policy.MaxRetryAttempts > 15 {
		return domain.DunningPolicy{}, errs.Invalid("max_retry_attempts", "cannot exceed 15")
	}
	if policy.GracePeriodDays <= 0 {
		policy.GracePeriodDays = 3
	}
	if policy.GracePeriodDays > 30 {
		return domain.DunningPolicy{}, errs.Invalid("grace_period_days", "cannot exceed 30")
	}
	switch policy.FinalAction {
	case domain.DunningActionManualReview, domain.DunningActionPause,
		domain.DunningActionMarkUncollectible, domain.DunningActionCancelSubscription:
		// valid
	case "":
		// Default = pause (Stripe-aligned pause_collection.behavior=
		// keep_as_draft semantics after ADR-036 amendment). Without
		// this, the engine would keep generating finalized invoices
		// for delinquent customers every cycle — stacking failures +
		// dunning emails. pause + keep_as_draft is the operator-
		// protective choice. Operators who want a different terminal
		// action set it explicitly.
		policy.FinalAction = domain.DunningActionPause
	default:
		return domain.DunningPolicy{}, errs.Invalid("final_action",
			"must be one of: manual_review, pause, mark_uncollectible, cancel_subscription")
	}
	if err := domain.MaxLen("name", policy.Name, 255); err != nil {
		return domain.DunningPolicy{}, err
	}
	// retry_schedule length must support max_retry_attempts. Retry 1
	// uses grace_period_days; retries 2..N use retry_schedule[0..N-2].
	// So schedule must have at least MaxRetryAttempts - 1 entries.
	needed := policy.MaxRetryAttempts - 1
	if needed > 0 && len(policy.RetrySchedule) < needed {
		return domain.DunningPolicy{}, errs.Invalid("retry_schedule",
			fmt.Sprintf("max_retry_attempts (%d) requires at least %d retry_schedule entries — got %d",
				policy.MaxRetryAttempts, needed, len(policy.RetrySchedule)))
	}
	// Every entry must parse as a positive Go duration NOW: the retry path
	// calls time.ParseDuration at fire time, and an unparseable entry there
	// errors AFTER the attempt is persisted but BEFORE the reschedule lands
	// — wedging the run in a re-attempt livelock at that retry forever
	// (found live in FLOW TC5 with "3d": Go durations have no day unit).
	// Rejecting here keeps the retry path's "save-time validation
	// guarantees the schedule" contract actually true.
	for i, entry := range policy.RetrySchedule {
		d, err := time.ParseDuration(entry)
		if err != nil {
			return domain.DunningPolicy{}, errs.Invalid("retry_schedule",
				fmt.Sprintf("entry %d (%q) is not a valid Go duration — use h/m/s units, e.g. \"72h\" for 3 days", i, entry))
		}
		if d <= 0 {
			return domain.DunningPolicy{}, errs.Invalid("retry_schedule",
				fmt.Sprintf("entry %d (%q) must be a positive duration", i, entry))
		}
	}
	return policy, nil
}

// ListRuns returns dunning runs matching the filter.
func (s *Service) ListRuns(ctx context.Context, filter RunListFilter) ([]domain.InvoiceDunningRun, int, error) {
	return s.store.ListRuns(ctx, filter)
}

func (s *Service) GetStats(ctx context.Context, tenantID string) (Stats, error) {
	return s.store.GetStats(ctx, tenantID)
}

// enqueueDunningWarning resolves the customer's email + invoice context
// and synchronously enqueues a dunning-warning email via the outbox.
// Synchronous on purpose — `SendDunningWarning` is a fast DB INSERT;
// running it in a goroutine bound to the catchup ctx would race against
// `defer cancel()` in testclock/catchup.go and silently drop the email.
// The actual SMTP send happens later via the outbox dispatcher worker
// on its own long-lived ctx. Errors are logged (best-effort); they do
// NOT roll back the dunning state transition.
func (s *Service) enqueueDunningWarning(ctx context.Context, tenantID string, run domain.InvoiceDunningRun, policy domain.DunningPolicy, retryErrMsg string) {
	email, name, cc, err := s.customerEmail.GetCustomerEmail(ctx, tenantID, run.CustomerID)
	if err != nil || email == "" {
		slog.Warn("skip dunning warning email — cannot resolve customer email",
			"run_id", run.ID, "customer_id", run.CustomerID, "error", err)
		return
	}
	invoiceNumber := run.InvoiceID
	var publicToken string
	var billTZ string
	if s.invoiceGet != nil {
		if inv, err := s.invoiceGet.Get(ctx, tenantID, run.InvoiceID); err == nil {
			invoiceNumber = inv.InvoiceNumber
			publicToken = inv.PublicToken
			billTZ = inv.BillingTimezone
		}
	}
	nextRetry := "TBD"
	if run.NextActionAt != nil {
		// Render the retry date in the invoice's billing timezone (ADR-077),
		// UTC when unknown — not the raw process zone, so the customer-facing
		// "we'll try again on <date>" line doesn't shift a day on a non-UTC
		// deployment (ADR-075 audit).
		nextRetry = run.NextActionAt.In(domain.LoadLocationOrUTC(billTZ)).Format("January 2, 2006")
	}
	if err := s.emailNotifier.SendDunningWarning(ctx, tenantID, email, cc, name, invoiceNumber, run.AttemptCount, policy.MaxRetryAttempts, nextRetry, retryErrMsg, publicToken); err != nil {
		slog.Error("failed to enqueue dunning warning email",
			"run_id", run.ID, "email", email, "error", err)
	}
}

// enqueueDunningEscalation is the escalation-email counterpart to
// enqueueDunningWarning. Same synchronous-enqueue rationale.
//
// action is the final action that ACTUALLY applied to this invoice — ""
// (rendered as neutral copy) when the policy's action was skipped, e.g. a
// cancel_subscription policy exhausting on a one-off invoice with no
// subscription. Callers must not pass policy.FinalAction blindly: the
// email asserts an outcome, so it renders outcomes, not intentions.
func (s *Service) enqueueDunningEscalation(ctx context.Context, tenantID string, run domain.InvoiceDunningRun, action string) {
	email, name, cc, err := s.customerEmail.GetCustomerEmail(ctx, tenantID, run.CustomerID)
	if err != nil || email == "" {
		slog.Warn("skip dunning escalation email — cannot resolve customer email",
			"run_id", run.ID, "customer_id", run.CustomerID, "error", err)
		return
	}
	invoiceNumber := run.InvoiceID
	var publicToken string
	if s.invoiceGet != nil {
		if inv, err := s.invoiceGet.Get(ctx, tenantID, run.InvoiceID); err == nil {
			invoiceNumber = inv.InvoiceNumber
			publicToken = inv.PublicToken
		}
	}
	if err := s.emailNotifier.SendDunningEscalation(ctx, tenantID, email, cc, name, invoiceNumber, action, publicToken); err != nil {
		slog.Error("failed to enqueue dunning escalation email",
			"run_id", run.ID, "email", email, "error", err)
	}
}
