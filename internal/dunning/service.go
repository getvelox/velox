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
	"github.com/sagarsuperuser/velox/internal/platform/activity"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/scheduler"
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
	// Ask marks a decline after which the card must not be charged again
	// for this invoice (domain.DeclineAskOf the decline code): the bank
	// refused it for good (#966), or wants the customer to confirm the
	// payment. The attempt counts as usual; what changes is the customer's
	// warning email, which asks for a different card, or for the
	// confirmation, instead of promising a retry.
	Ask domain.DeclineAsk
}

func (e *RetryError) Error() string { return e.Err.Error() }
func (e *RetryError) Unwrap() error { return e.Err }

// ErrTransientSkip signals that this tick's retry got no definite answer, so
// dunning must NOT tick attempt_count or emit a failure event; the next tick
// retries. The PaymentRetrier returns it for everything except a definite
// decline, no card, a card refused for good, or a card whose payment the
// bank wants the customer to confirm: the circuit breaker open,
// Stripe rate limiting or unreachable, an unconfirmed outcome, a database
// error before or after the charge. The adapter maps payment's errors to
// this dunning-visible sentinel so peer domains don't import each other.
var ErrTransientSkip = errors.New("dunning retry skipped: upstream transient")

// PaymentRetrier retries a payment for an invoice.
type PaymentRetrier interface {
	RetryPayment(ctx context.Context, tenantID, invoiceID, stripeCustomerID string) error
}

// SubscriptionPauser pauses collection on a subscription when dunning
// exhausts retries with final_subscription_action='pause' (ADR-112 —
// semantics changed to match Stripe's `pause_collection.behavior=
// keep_as_draft`: cycle continues, drafts pile up, no charging /
// dunning until resumed). Pre-amendment this called PauseAtomic
// (hard pause, no further cycles); that was non-Stripe and silently
// skipped invoice generation for the affected periods.
//
// invoiceID is the run's unpaid invoice. The pause is dunning's and records
// it: paying that invoice lifts the pause (ADR-036 amendment 2026-10-22). applied reports whether collection is paused on return: an
// operator's pause already in force counts and is left as it is; an
// invoice paid before the pause landed does not, and nothing is paused.
type SubscriptionPauser interface {
	PauseCollection(ctx context.Context, tenantID, subID, invoiceID string) (applied bool, err error)
}

// SubscriptionCanceler cancels a subscription when dunning exhausts
// retries with final_subscription_action='cancel'.
type SubscriptionCanceler interface {
	Cancel(ctx context.Context, tenantID, id string) error
}

// SubscriptionActionNotYet is what a SubscriptionPauser or SubscriptionCanceler
// returns when the action would close a billing period still inside its
// invoice grace (ADR-120 amendment 2026-10-22). Nothing was changed. The
// final actions run again at Until, when the grace has passed; it is a wait,
// not a failure.
type SubscriptionActionNotYet struct {
	Until time.Time
}

func (e *SubscriptionActionNotYet) Error() string {
	return "the subscription action waits for the invoice grace until " + e.Until.UTC().Format(time.RFC3339Nano)
}

// InvoiceUncollectibleMarker writes the unpaid invoice off when dunning
// exhausts retries with final_invoice_action='mark_uncollectible'.
type InvoiceUncollectibleMarker interface {
	MarkUncollectible(ctx context.Context, tenantID, invoiceID string) error
}

// InvoiceGetter gets invoice details for finding the subscription.
type InvoiceGetter interface {
	Get(ctx context.Context, tenantID, id string) (domain.Invoice, error)
}

// ChargeHoldClaimer takes and releases the invoice's charge hold around
// dunning's final action (ADR-112 amendment, ADR-127). While dunning holds it
// no charge can start on the invoice, and the final action does not run at
// all while any hold is set: a charge running, or one whose result was lost
// and may have taken the money. Implemented by *invoice.PostgresStore.
type ChargeHoldClaimer interface {
	ClaimChargeHoldForFinalAction(ctx context.Context, tenantID, invoiceID string) (domain.ChargeHold, error)
	ReleaseChargeHold(ctx context.Context, tenantID, invoiceID string, hold domain.ChargeHold) error
}

// SubscriptionTerminalState reports whether a subscription can still be
// acted on. Terminated: `canceled` is the state in which BOTH movers
// refuse.
type SubscriptionTerminalState struct {
	Terminated bool
	// PeriodStart is the start of the subscription's current billing
	// period, zero when it has none. A final action is never dated before
	// it (applySubscriptionAction).
	PeriodStart time.Time
}

// SubscriptionStateReader answers "is there anything left to do to this
// subscription" before exhaustRun fires a terminal action.
//
// It exists because ADR-112 made exhaustion fire TWO actions that can fail
// independently, and a failed exhaustion is re-attempted 24h later. Both
// subscription movers refuse when the sub is already terminal —
// cancelSpec() allows only draft/trialing/active, and SetPauseCollection
// rejects canceled outright — so without a skip-if-done read, a
// re-attempt after a partial failure would fail forever on the half that
// already succeeded and never reach the half that did not. Same reason the
// invoice half checks status==uncollectible first: MarkUncollectible
// returns "invoice is already uncollectible" rather than a no-op.
//
// Deliberately a state READ rather than error-string matching on the
// movers (no heuristic proxies) and rather than a new applied-actions
// column on dunning_runs (one source of truth — if an operator canceled
// the sub by hand mid-run, skipping is the correct behaviour, and only the
// entity knows that).
type SubscriptionStateReader interface {
	GetTerminalState(ctx context.Context, tenantID, id string) (SubscriptionTerminalState, error)
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
	SendPaymentFailed(ctx context.Context, tenantID, to string, cc []string, customerName, invoiceNumber, reason, publicToken string, ask domain.DeclineAsk) error
	// SendDunningWarning emails the customer about a failed retry.
	// failureReason is the latest decline reason (from the retrier's
	// error message) — surfaced inline so customers can act
	// (insufficient_funds → top up; lost_card → swap card). Empty
	// reason renders without the diagnostic block. No final-attempt
	// tone exists: the exhausting attempt sends the escalation email
	// instead of a warning (see the warning-enqueue gate in processRun).
	//
	// ask selects the copy. DeclineAskNewCard is "use a different card": the
	// bank refused the card for good (#966). DeclineAskConfirm is "confirm
	// your payment": the bank wants the customer to confirm it. Either way
	// dunning retries skip that card, so the email must not promise a retry
	// on a date, and failureReason is empty.
	SendDunningWarning(ctx context.Context, tenantID, to string, cc []string, customerName, invoiceNumber string, attemptNumber, maxAttempts int, nextRetryDate, failureReason, publicToken string, ask domain.DeclineAsk) error
	// outcome is what exhaustion actually did to THIS invoice, not what
	// the policy asked for — the copy is composed from it (ADR-112).
	SendDunningEscalation(ctx context.Context, tenantID, to string, cc []string, customerName, invoiceNumber string, outcome domain.DunningEscalationOutcome, publicToken string) error
	// The Tx variants ride the caller's state transaction (ADR-040): the
	// dunning reschedule/escalation and its customer email commit together,
	// so a failover between the two can no longer lose the email with the
	// state standing. The store wraps the call in a SAVEPOINT — returning an
	// error skips the email, never the money write.
	SendDunningWarningTx(ctx context.Context, tx *sql.Tx, tenantID, to string, cc []string, customerName, invoiceNumber string, attemptNumber, maxAttempts int, nextRetryDate, failureReason, publicToken string, ask domain.DeclineAsk) error
	SendDunningEscalationTx(ctx context.Context, tx *sql.Tx, tenantID, to string, cc []string, customerName, invoiceNumber string, outcome domain.DunningEscalationOutcome, publicToken string) error
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
	subState         SubscriptionStateReader
	invoiceUncollect InvoiceUncollectibleMarker
	invoiceGet       InvoiceGetter
	chargeHolds      ChargeHoldClaimer
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
// Only a policy that is genuinely not set falls through to the default: no
// dunning_policy_id, or an assigned policy that no longer exists (the FK's
// ON DELETE SET NULL should prevent that). A read ERROR fails instead — the
// run stores its policy for life, so defaulting on a blip would bind the
// customer's run to the wrong retry schedule, attempt cap and terminal
// actions for good.
func (s *Service) GetEffectivePolicyForCustomer(ctx context.Context, tenantID, customerID string) (domain.DunningPolicy, error) {
	if s.custPolicy != nil && customerID != "" {
		pid, err := s.custPolicy.GetDunningPolicyID(ctx, tenantID, customerID)
		if err != nil && !errors.Is(err, errs.ErrNotFound) {
			return domain.DunningPolicy{}, fmt.Errorf("read the customer's dunning policy: %w", err)
		}
		if pid != "" {
			p, err := s.store.GetPolicyByID(ctx, tenantID, pid)
			if err == nil {
				return p, nil
			}
			if !errors.Is(err, errs.ErrNotFound) {
				return domain.DunningPolicy{}, fmt.Errorf("load dunning policy %s: %w", pid, err)
			}
			// Assigned policy no longer exists — fall through to default.
		}
	}
	return s.store.GetDefaultPolicy(ctx, tenantID)
}

// SetSubscriptionPauser configures the pause-collection terminal action
// (Stripe-aligned keep_as_draft, not hard pause).
func (s *Service) SetSubscriptionPauser(pauser SubscriptionPauser, invoices InvoiceGetter) {
	s.subPauser = pauser
	s.invoiceGet = invoices
}

// SetSubscriptionCanceler configures the cancel terminal action.
func (s *Service) SetSubscriptionCanceler(c SubscriptionCanceler) {
	s.subCanceler = c
}

// SetSubscriptionStateReader wires the skip-if-done read that makes a
// re-attempted exhaustion converge — see SubscriptionStateReader. Leaving
// it nil is safe but degraded: the subscription action is then attempted
// unconditionally, so a re-attempt whose subscription half already
// succeeded logs a failure and re-queues instead of moving on to the
// invoice half.
func (s *Service) SetSubscriptionStateReader(r SubscriptionStateReader) {
	s.subState = r
}

// SetChargeHoldClaimer wires the final action's charge hold (ADR-127). The
// producer always wires it; unwired (narrow tests) the final action runs
// without the hold, as before ADR-127.
func (s *Service) SetChargeHoldClaimer(c ChargeHoldClaimer) {
	s.chargeHolds = c
}

// SetInvoiceUncollectibleMarker configures the write-off terminal action.
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

// fireRunEvent sends a dunning.* event carrying the run as
// GET /v1/dunning/runs/{id} returns it, read after the change committed
// (ADR-131). When the read fails there is no true resource to send, so the
// event is left out, loudly.
func (s *Service) fireRunEvent(ctx context.Context, tenantID, eventType, runID string, extra map[string]any) {
	if s.events == nil {
		return
	}
	run, err := s.store.GetRun(ctx, tenantID, runID)
	if err != nil {
		slog.ErrorContext(ctx, "dunning webhook event not sent: the run could not be read after the change",
			"event_type", eventType, "tenant_id", tenantID, "run_id", runID, "error", err)
		return
	}
	s.fireEvent(ctx, tenantID, eventType, domain.DunningRunWebhookData(run, extra))
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
// The first retry lands the policy's first retry_days entry after the moment
// collection failed, in the invoice's own time domain:
//
//   - A wall-clock invoice: now, the moment the failure is recorded. Not the
//     invoice's issue date or billing period — an invoice charged (or
//     re-charged, or backfilled) days after it was issued would otherwise get
//     a first retry already in the past and be retried with no grace at all
//     (found walking FLOW I10, 2026-10-11: an Oct-11 decline anchored on its
//     Oct-1 period start was retried at once instead of on Oct 14).
//   - A clock-pinned invoice: simFailedAt, when that is earlier than the
//     clock's now. A test-clock Advance does its work at advance-end
//     frozen_time, but a charge belongs at the simulated instant it fired —
//     a cycle invoice's charge at its cycle close, a charge after a card
//     attached on a later Advance at that Advance — so the retries fall
//     inside the window the operator advanced through, as Stripe test clocks
//     do.
//
// simFailedAt is that simulated instant, chosen by the caller: the failed
// charge's contracted anchor (payment.dunningSimFailedAt), or the issue
// instant when no charge carried one (the card-less path). Zero when unknown;
// read only for a clock-pinned invoice.
//
// cause is WHY collection needs a run — declared by the caller at write
// time (decline paths pass payment_failed; the card-less enrollment
// sweeps pass no_payment_method) and stamped on the run + started
// event. Pre-fix this was hardcoded "payment_failed" for every start,
// so card-less invoices carried a payment-failure they never had; the
// timeline hid the lie by rendering a generic row. An unknown cause is
// refused loudly.
func (s *Service) StartDunning(ctx context.Context, tenantID string, invoiceID, customerID string, simFailedAt time.Time, cause domain.DunningStartCause) (domain.InvoiceDunningRun, error) {
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
		// Logged at the same volume as the no-policy skip above: both end
		// with "this invoice will never dun", and an operator debugging that
		// needs the same evidence either way. Silence here was a real trap
		// (found walking FLOW TC5, 2026-08-04): POST /v1/dunning/policies
		// omitting `enabled` creates a DISABLED policy — and the first policy
		// a tenant creates is auto-promoted to default — so an API caller
		// ends up with a policy that is simultaneously the tenant default and
		// inert, with no error at create, no run, and (before this) no log.
		// The dashboard is unaffected: its form sends enabled:true, and the
		// recipe installer sets Enabled explicitly.
		slog.WarnContext(ctx, "dunning policy is disabled — skipping enrollment; the invoice will not dun",
			"tenant_id", tenantID, "invoice_id", invoiceID, "customer_id", customerID,
			"policy_id", policy.ID, "policy_name", policy.Name, "is_default", policy.IsDefault)
		return domain.InvoiceDunningRun{}, errs.InvalidState("dunning is disabled (assigned policy or tenant default is not enabled)")
	}

	// The first entry of retry_days is the wait before the first retry.
	firstRetryDelay, ok := policy.WaitBeforeRetry(1)
	if !ok {
		// Unreachable while every writer validates and the column's CHECK
		// holds (at least one entry). Loud: a run with no retry to schedule
		// would never be due.
		return domain.InvoiceDunningRun{}, fmt.Errorf("dunning policy %s has no retries", policy.ID)
	}

	ctx = s.bindForInvoice(ctx, tenantID, invoiceID)
	failureAt := s.clock.Now(ctx)
	if _, simulated := clock.SimOf(ctx); simulated && !simFailedAt.IsZero() && simFailedAt.Before(failureAt) {
		failureAt = simFailedAt
	}
	t := failureAt.Add(firstRetryDelay)
	nextActionAt := &t

	// One transaction: the run, its dunning_started timeline event (at the
	// failure instant, so the invoice timeline's 'Automatic retry scheduled'
	// row aligns with the cycle's period_end, not the orchestrator's
	// frozen_time), and the dunning.started webhook. A failure writes none of
	// them, and the caller's retry starts cleanly.
	run, err := s.store.StartRun(ctx, tenantID, domain.InvoiceDunningRun{
		InvoiceID:    invoiceID,
		CustomerID:   customerID,
		PolicyID:     policy.ID,
		State:        domain.DunningActive,
		Reason:       string(cause),
		AttemptCount: 0,
		NextActionAt: nextActionAt,
		// CreatedAt = failureAt so a simulated run lives on its invoice's
		// issue instant, not orchestrator frozen_time.
		CreatedAt: failureAt,
	})
	if err != nil {
		return domain.InvoiceDunningRun{}, fmt.Errorf("start dunning run: %w", err)
	}

	slog.Info("dunning started", "run_id", run.ID, "invoice_id", invoiceID)

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

// exhaustLease is how long a run whose retries are exhausted stays hidden from
// both due-run pickers while its terminal actions run (SB-1/SB-2, 2026-10-08).
// It must exceed the worst inline charge + terminal-action duration (Stripe
// calls are bounded at 30s); if the executor dies, the run is due again when
// the lease expires and exhaust-on-entry finishes it. Truncated to the
// column's microsecond precision because ClaimExhaustion compares it exactly.
const exhaustLease = 15 * time.Minute

func leaseFrom(t time.Time) time.Time { return t.Add(exhaustLease).Truncate(time.Microsecond) }

// anchorInstant is the one rule for "the instant this action was contracted
// for" (playbook class J).
//
// Simulated time (test-clock catchup, or a clock-pinned customer): the
// simulated moment the action was scheduled for (scheduled), not the
// orchestrator's frozen_time. Otherwise every action under catchup is stamped
// at advance-end frozen_time and the next one is scheduled past advance-end,
// so it never fires in the same Advance click.
//
// Pure wall-clock cron: max(now, scheduled). In steady state they are equal.
// After the scheduler was down for several intervals, scheduled is stale-in-
// the-past; anchoring on it would schedule the next action in the past too,
// so the whole backlog would fire back-to-back in one tick and collapse the
// configured cadence. Clamping to now resumes the cadence from recovery.
func anchorInstant(now time.Time, scheduled *time.Time, simulated bool) time.Time {
	if scheduled != nil && (simulated || scheduled.After(now)) {
		return *scheduled
	}
	return now
}

// exhaustStampAt is the instant a re-driven escalation records (class J).
// listedNext is the next_action_at the run was listed with, read BEFORE the
// claim overwrites it.
//
// When listedNext is exactly the lease the final retry wrote
// (leaseFrom(LastAttemptAt)), this is the late execution of the exhaustion
// contracted at that retry, so it records the retry's instant — the record
// the uninterrupted inline path writes — never the lease or frozen_time.
// Every other entry records when it ran (attemptAt): an action_failed
// re-attempt (it carries attempt+24h, not the lease) is its own scheduled
// action, and a run exhausted because an operator shortened
// retry_days was never contracted at its last retry (backdating it
// would show the escalation before the edit that caused it).
// One residue: a re-drive interrupted AFTER its claim re-leases from its own
// attempt instant, so a second re-drive records that instant instead.
func exhaustStampAt(run domain.InvoiceDunningRun, listedNext *time.Time, attemptAt time.Time) time.Time {
	if run.LastAttemptAt == nil || listedNext == nil || !listedNext.Equal(leaseFrom(*run.LastAttemptAt)) {
		return attemptAt
	}
	return *run.LastAttemptAt
}

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
//
// A run whose processing panics stops, the other runs are still processed,
// and the first panic is raised again at the end, so the tick still ends as
// panicked; the other runs' errors are logged first. A panic in the retry
// itself has already been rewound (processRun).
func (s *Service) processRunsBatch(ctx context.Context, tenantID string, dueRuns []domain.InvoiceDunningRun, isCatchup bool) (int, []error) {
	var panics scheduler.Panics
	processed := 0
	var runErrs []error
	for _, run := range dueRuns {
		func() {
			defer panics.Guard("step", "dunning_run", "run_id", run.ID, "invoice_id", run.InvoiceID, "tenant_id", tenantID)
			if err := s.processRun(ctx, tenantID, run, isCatchup); err != nil {
				runErrs = append(runErrs, fmt.Errorf("run %s: %w", run.ID, err))
				return
			}
			processed++
		}()
	}
	if panics.Panicked() {
		// The panic drops runErrs, which only the caller logs and counts. Log
		// them here, so another run's failure is not hidden while one run
		// keeps panicking.
		for _, err := range runErrs {
			slog.ErrorContext(ctx, "dunning error", "tenant_id", tenantID, "error", err)
		}
		panics.Repanic()
	}
	return processed, runErrs
}

func (s *Service) processRun(ctx context.Context, tenantID string, run domain.InvoiceDunningRun, isCatchup bool) error {
	// Resolve the policy bound to this run at StartDunning time.
	// Runs stay bound to the policy ID they started on: reassigning the
	// customer to a different policy mid-flight does not move in-flight
	// runs, and only the NEXT run picks up the new assignment (Stripe-Lago
	// shape, ADR-036 research). EDITS to the bound policy do reach
	// in-flight runs — the row is updated in place and read here every
	// tick — so shortening retry_days below the attempts made exhausts a
	// run on its next tick through the exhaust-on-entry branch below.
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
				_, rerr := s.resolveListedRun(ctx, tenantID, run, domain.ResolutionPaymentRecovered, "payment_recovered", nil)
				return rerr
			// The resolution names WHICH terminal state closed the run. The
			// event reason below already derived it from inv.Status; before
			// the 0170 split the resolution column threw that away and wrote
			// one value for both outcomes.
			case inv.Status == domain.InvoiceVoided:
				_, rerr := s.resolveListedRun(ctx, tenantID, run, domain.ResolutionInvoiceVoided, "invoice_"+string(inv.Status), nil)
				return rerr
			case inv.Status == domain.InvoiceUncollectible:
				_, rerr := s.resolveListedRun(ctx, tenantID, run, domain.ResolutionInvoiceNotCollectible, "invoice_"+string(inv.Status), nil)
				return rerr
			// Stripe refused the amount as below its minimum charge. The
			// charge path ends the run itself, but in a separate call after
			// the stamp commits, which can be lost (a DB blip, the hosted
			// request's context ending, a restart; on the final retry, a
			// crash after the exhaustion lease). A card-less or refused-card
			// retry never asks Stripe again, so without this the run went on
			// to its reminders and final action.
			case inv.BelowStripeMinimum():
				_, rerr := s.resolveListedRun(ctx, tenantID, run, domain.ResolutionAmountBelowMinimum, string(domain.ResolutionAmountBelowMinimum), nil)
				return rerr
			}
		}
	}

	// The terminal-resolution check above deliberately runs BEFORE the policy
	// fetch: resolving a run whose invoice is already terminal needs no policy,
	// so a run whose policy cannot be loaded must not error out of processing
	// when a one-line resolve is all that remains. Honest provenance: the
	// original ordering was surfaced by a 2026-08-02 walk fixture that was
	// itself malformed (a cross-mode run RLS makes unconstructible through real
	// flows), and no REAL path to an unloadable policy is known today — the FK
	// is RESTRICT and runs bind same-mode policies. The reorder stays because
	// it is correct on principle (robustness against a class, not an instance)
	// and costs nothing; TestProcessDueRuns_TerminalInvoiceResolvesWithoutPolicy
	// pins it via a bypass-constructed fixture.
	policy, err := s.store.GetPolicyByID(ctx, tenantID, run.PolicyID)
	if err != nil {
		return fmt.Errorf("get bound policy %s for run %s: %w", run.PolicyID, run.ID, err)
	}

	if run.Paused {
		return nil // Skip paused runs
	}

	ctx, _ = clock.BindEffectiveNow(ctx, s.resolver, clock.Pin{TenantID: tenantID, InvoiceID: run.InvoiceID})
	// Simulated means a test clock is bound, not merely that the resolver
	// bound an instant: BindEffectiveNow reports true for a wall-clock invoice
	// too (the engine resolves it to a clock-less Sim at now). Keying on that
	// bool made every production run look simulated, so anchorInstant's
	// downtime clamp never ran and a late retry left every later retry in the
	// past, fired one per tick (found walking FLOW I10, 2026-10-11: VLX-000081
	// retried "Oct 4" then "Oct 7" on consecutive ticks on Oct 11).
	_, simulated := clock.SimOf(ctx)

	// Retries exhausted on entry: an exhaustion that was interrupted after the
	// final retry (its lease expired), an action_failed re-attempt, or a policy
	// whose retry_days was shortened. Claim it before any terminal action
	// fires: exactly one processor moves next_action_at from the listed value
	// to a fresh lease, and only that one exhausts (SB-2). A loser fires nothing.
	retries := policy.Retries()
	if run.AttemptCount >= retries {
		if run.NextActionAt == nil { // unreachable: both pickers select on next_action_at
			return fmt.Errorf("exhaust-on-entry: run %s has no next_action_at", run.ID)
		}
		attemptAt := anchorInstant(s.clock.Now(ctx), run.NextActionAt, isCatchup || simulated)
		lease := leaseFrom(attemptAt)
		won, err := s.store.ClaimExhaustion(ctx, tenantID, run.ID, run.AttemptCount, *run.NextActionAt, lease)
		if err != nil {
			return fmt.Errorf("claim exhaustion for run %s: %w", run.ID, err)
		}
		if !won {
			slog.Info("dunning exhaust skipped — run claimed, resolved or escalated by another processor",
				"run_id", run.ID, "invoice_id", run.InvoiceID)
			return nil
		}
		stampAt := exhaustStampAt(run, run.NextActionAt, attemptAt)
		run.NextActionAt = &lease
		return s.exhaustRun(ctx, tenantID, run, policy, stampAt, attemptAt)
	}

	// Attempt retry
	run.AttemptCount++
	prevLastAttemptAt := run.LastAttemptAt // for the transient-skip rewind below
	prevNextActionAt := run.NextActionAt   // ditto: the final attempt replaces it with the lease
	// Anchor this attempt's instant on the contracted one (see anchorInstant).
	now := anchorInstant(s.clock.Now(ctx), run.NextActionAt, isCatchup || simulated)
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
	// expected = the count this tick READ (the increment above is ours to
	// prove). applied=false now covers two causes with one skip: resolved
	// concurrently, or another processor already recorded an attempt this
	// tick's read predates — charging on a stale read would burn budget
	// dishonestly (ha-8).
	//
	// The FINAL attempt also records its exhaustion lease here, before the
	// charge: on the wall clock the run stays hidden from ListDueRuns for the
	// whole charge and inline-exhaust window (so no second processor exhausts
	// it under the charge), and if this process dies anywhere in that window
	// the run is due again when the lease expires and exhaust-on-entry
	// finishes it. Under test-clock catchup the lease is in simulated time and
	// an advance can pass it; there the clock's single-flight advance is the
	// exclusion, and ClaimExhaustion guards any re-entry.
	if run.AttemptCount >= retries {
		l := leaseFrom(now)
		run.NextActionAt = &l
	}
	if applied, err := s.store.UpdateRunIfActive(ctx, tenantID, run, run.AttemptCount-1, nil); err != nil {
		return fmt.Errorf("persist dunning attempt before retry: %w", err)
	} else if !applied {
		slog.Info("dunning retry skipped — run resolved or attempt recorded by another processor",
			"run_id", run.ID, "invoice_id", run.InvoiceID)
		return nil
	}

	// Actually retry the payment
	retryErr := fmt.Errorf("payment retrier not configured")
	// A panic in the retry (a nil dereference on one unusual row) is not a
	// decline. It is handled as a transient skip, so the rewind below undoes
	// the attempt recorded above, and is then raised again. Without this the
	// run stayed due with the attempt spent: every rerun spent another (and,
	// after a decline had released the charge hold, charged the card again),
	// and exhaust-on-entry fired the final action within a few reruns.
	var retryPanic scheduler.Panics
	if s.retrier != nil {
		retryErr = ErrTransientSkip // kept when RetryPayment panics
		func() {
			defer retryPanic.Guard("step", "dunning_retry", "run_id", run.ID, "invoice_id", run.InvoiceID, "tenant_id", tenantID)
			retryErr = s.retrier.RetryPayment(ctx, tenantID, run.InvoiceID, run.CustomerID)
		}()
	}

	// Transient skip: no definite answer (see ErrTransientSkip). This is NOT a
	// dunning attempt — rewind the attempt we recorded above (count +
	// last_attempt_at) so the run looks as it did before this tick, don't log a
	// failure event or reschedule, and let the next scheduler tick retry. A
	// five-minute Stripe outage or a dropped database connection should not
	// burn a tenant's retry budget.
	if errors.Is(retryErr, ErrTransientSkip) {
		run.AttemptCount--
		run.LastAttemptAt = prevLastAttemptAt
		run.NextActionAt = prevNextActionAt // undo the final attempt's lease too
		// GUARDED rewind. ErrTransientSkip also covers the ambiguous
		// PI-may-have-succeeded outcome (client saw a 5xx/timeout but the charge
		// actually went through): its webhook may have resolved this run during the
		// charge window. UpdateRunIfActive no-ops on a resolved run, so we never
		// clobber that resolve back to active (which would re-fire dunning.resolved).
		// A failed rewind on a still-active run leaves the count one high — the SAFE
		// direction: the exhaustion gate uses `>=`, so an over-count can only end a
		// retry cycle early, never over-retry.
		if _, err := s.store.UpdateRunIfActive(ctx, tenantID, run, run.AttemptCount+1, nil); err != nil {
			slog.Warn("dunning transient-skip: failed to rewind the pre-charge attempt persist",
				"run_id", run.ID, "invoice_id", run.InvoiceID, "error", err)
		}
		retryPanic.Repanic()
		slog.Info("dunning retry skipped — no definite answer",
			"run_id", run.ID, "invoice_id", run.InvoiceID)
		return nil
	}

	// Below Stripe's minimum charge: Stripe refused the amount, not the card,
	// so no retry, card or final action can collect it. The charge path left
	// the invoice open for the operator; recovery ends here, at this retry's
	// instant, with no warning email and no final action (which by default
	// pauses the subscription). The attempt stays counted: Stripe was asked.
	if errors.Is(retryErr, domain.ErrBelowMinimumCharge) {
		slog.Info("dunning ended — Stripe refused the amount as below its minimum charge",
			"run_id", run.ID, "invoice_id", run.InvoiceID, "attempt", run.AttemptCount)
		if _, eerr := s.store.CreateEvent(ctx, tenantID, domain.InvoiceDunningEvent{
			RunID:        run.ID,
			InvoiceID:    run.InvoiceID,
			EventType:    domain.DunningEventRetryAttempted,
			State:        domain.DunningResolved,
			AttemptCount: run.AttemptCount,
			Reason:       domain.DunningRetryReasonBelowMinimum,
			CreatedAt:    now,
		}); eerr != nil {
			slog.Warn("dunning: could not record the below-minimum retry on the timeline; the run still ends",
				"run_id", run.ID, "invoice_id", run.InvoiceID, "error", eerr)
		}
		_, rerr := s.resolveListedRun(ctx, tenantID, run, domain.ResolutionAmountBelowMinimum, string(domain.ResolutionAmountBelowMinimum), &now)
		return rerr
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
		// A card-less "attempt" records the same no_payment_method cause
		// vocabulary the start event uses — the timeline renders it as a
		// reminder tick, not a charge failure. A retry that skipped a card
		// the bank refused for good charged nothing either, and says so
		// (#966), as does one that skipped a card whose payment the bank
		// wants the customer to confirm. Real declines keep their provider
		// reason text.
		evtReason := retryErr.Error()
		switch {
		case errors.Is(retryErr, domain.ErrNoPaymentMethodOnRetry):
			evtReason = string(domain.DunningCauseNoPaymentMethod)
		case errors.Is(retryErr, domain.ErrCardNeedsReplacing):
			evtReason = domain.DunningRetryReasonCardRefused
		case errors.Is(retryErr, domain.ErrCardNeedsConfirmation):
			evtReason = domain.DunningRetryReasonNeedsConfirmation
		}
		_, _ = s.store.CreateEvent(ctx, tenantID, domain.InvoiceDunningEvent{
			RunID:        run.ID,
			InvoiceID:    run.InvoiceID,
			EventType:    domain.DunningEventRetryAttempted,
			State:        domain.DunningActive,
			AttemptCount: run.AttemptCount,
			Reason:       evtReason,
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
		_, rerr := s.resolveListedRun(ctx, tenantID, run, domain.ResolutionPaymentRecovered, "payment_recovered", &now)
		return rerr
	}

	// Schedule the next retry: retry_days[k] is the wait after retry k
	// (retry_days[0], the wait before retry 1, was StartDunning's).
	// The list's length is the retry count, so a retry still to come always
	// has its entry; there is no count that can disagree with the list.
	if run.AttemptCount < retries {
		wait, ok := policy.WaitBeforeRetry(run.AttemptCount + 1)
		if !ok { // unreachable: AttemptCount+1 <= retries
			return fmt.Errorf("dunning policy %s has no retry %d", policy.ID, run.AttemptCount+1)
		}
		t := now.Add(wait)
		run.NextActionAt = &t
	} else {
		// Never NULL: NULL removes an active run from both due-run pickers for
		// good, so an exhaustion interrupted after this commit (deploy SIGTERM,
		// lease loss, a DB error on the escalation write) stranded the run
		// forever (SB-1). Not now either: that would make the run due while
		// exhaustRun is still running inline (SB-2). The lease (same value the
		// pre-charge persist wrote) re-arms it if exhaustion never finishes.
		l := leaseFrom(now)
		run.NextActionAt = &l
	}

	// Warning email rides the reschedule transaction (ADR-040): built here —
	// resolution and invoice context are plain reads — and enqueued by the
	// store inside the same tx, under a SAVEPOINT (an email failure logs
	// loud and skips the email, never the reschedule). N-1 warnings during
	// retries 1..N-1; the exhausting attempt sends the escalation instead.
	// The failure reason renders INSIDE the customer email. A no-PM retry
	// failure is internal phrasing — mapped to empty so the warning renders
	// without the diagnostic callout (2026-07-22 payment-surfacing audit,
	// P2-3); real decline reasons pass through as the provider's words
	// alone: the charge path's "payment failed: " wrapper is stripped, as the
	// timeline strips it (min-charge check 2026-10-22, finding 4.1).
	//
	// A card the bank refused for good (#966) gets the "use a different
	// card" copy, and a payment the bank wants the customer to confirm the
	// "confirm your payment" copy, on the retry that drew the decline and
	// on every retry that then skips the card: "we'll try again on <date>"
	// would promise a charge that cannot succeed.
	var warnTx func(tx *sql.Tx) error
	if run.AttemptCount < retries && s.emailNotifier != nil && s.customerEmail != nil {
		ask := retryAsk(retryErr)
		reason := activity.StripWrapper(retryErr.Error())
		if errors.Is(retryErr, domain.ErrNoPaymentMethodOnRetry) || ask != domain.DeclineAskNone {
			reason = ""
		}
		warnTx = s.buildDunningWarningTx(ctx, tenantID, run, policy, reason, ask)
	}
	if applied, err := s.store.UpdateRunIfActive(ctx, tenantID, run, run.AttemptCount, warnTx); err != nil {
		return err
	} else if !applied {
		// Concurrently resolved during the failed-charge window (a non-charge settle
		// such as an operator resolve or credit-cover sweep) — don't reschedule or
		// exhaust a resolved run, and don't clobber the resolve back to active.
		// No warning email either — "we'll try again" to a customer who just paid
		// (the hook runs only on an applied write).
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
	// Check if exhausted after this attempt. Pass the simulated instant
	// of this final retry (= run.NextActionAt at fire time, captured
	// into `now` above) so the escalated event row aligns with the
	// retry that actually triggered the exhaustion, not orchestrator
	// frozen_time.
	if run.AttemptCount >= retries {
		return s.exhaustRun(ctx, tenantID, run, policy, now, now)
	}

	return nil
}

// exhaustRun finalizes a dunning run after its last retry failed (or
// it was found already at-or-beyond max attempts on entry). stampAt is
// the contracted instant the escalation records — the run's resolved_at
// and the escalated event's CreatedAt — so the invoice timeline shows
// the escalation aligned with the retry that caused it, never the
// orchestrator's frozen_time or the lease (see exhaustStampAt).
// attemptAt is when this exhaustion is actually being attempted; an
// action_failed re-attempt is scheduled from it, so the cadence never
// collapses after an outage. Inline after the final retry, both are
// that retry's instant.
func (s *Service) exhaustRun(ctx context.Context, tenantID string, run domain.InvoiceDunningRun, policy domain.DunningPolicy, stampAt, attemptAt time.Time) error {
	ctx = s.bindForInvoice(ctx, tenantID, run.InvoiceID)
	now := stampAt
	if now.IsZero() {
		now = s.clock.Now(ctx)
	}
	// The final actions happen at the escalation's instant, not wherever the
	// advance landed: rebind ctx's instant to it, keeping the clock, so the
	// subscription's pause or cancel and the invoice's write-off below (each
	// re-binds and inherits it) are dated with the escalated event. The
	// subscription's action never goes before its current period, though
	// (applySubscriptionAction). The inline path already arrives bound to its
	// retry slot; exhaust-on-entry arrives at frozen_time. Wall-clock runs
	// carry no Sim and are untouched.
	if sim, ok := clock.SimOf(ctx); ok {
		ctx = clock.WithSim(ctx, clock.Sim{At: now, TestClockID: sim.TestClockID})
	}

	// Late paid re-check — the terminal action (cancel in
	// particular) is IRREVERSIBLE, and the tick-start paid-pre-check goes stale
	// across the retry's Stripe round-trip: an invoice that settled out-of-band
	// mid-tick would otherwise cancel a paying customer's subscription here. Re-
	// read invoice status as late as possible and, if it is now terminal, resolve
	// the run (CAS-safe) instead of firing the terminal action. Same STATUS gate
	// as the tick-start check, plus one the tick-start check does not need: a
	// payment that may still take money holds both actions (ADR-112 amendment
	// 2026-10-19). This one read also feeds both terminal actions
	// below, so the state they act on is the state this check just cleared.
	// A fetch ERROR means the state is unknown: neither action fires, and the
	// run is re-queued through the action-failed path (the old behaviour fell
	// through, and a second read that succeeded acted without this check).
	var inv domain.Invoice
	invRead, invReadFailed := false, false
	if s.invoiceGet != nil {
		loaded, err := s.invoiceGet.Get(ctx, tenantID, run.InvoiceID)
		if err != nil {
			invReadFailed = true
			slog.Warn("failed to load invoice for dunning terminal action",
				"invoice_id", run.InvoiceID, "error", err)
		} else {
			switch {
			case loaded.Status == domain.InvoicePaid || loaded.PaymentStatus == domain.PaymentSucceeded:
				_, rerr := s.resolveListedRun(ctx, tenantID, run, domain.ResolutionPaymentRecovered, "payment_recovered", nil)
				return rerr
			// The resolution names WHICH terminal state closed the run. The
			// event reason below already derived it from inv.Status; before
			// the 0170 split the resolution column threw that away and wrote
			// one value for both outcomes.
			case loaded.Status == domain.InvoiceVoided:
				_, rerr := s.resolveListedRun(ctx, tenantID, run, domain.ResolutionInvoiceVoided, "invoice_"+string(loaded.Status), nil)
				return rerr
			case loaded.Status == domain.InvoiceUncollectible:
				_, rerr := s.resolveListedRun(ctx, tenantID, run, domain.ResolutionInvoiceNotCollectible, "invoice_"+string(loaded.Status), nil)
				return rerr
			// Stamped below Stripe's minimum during the final retry (a
			// hosted Pay click Stripe refused) and its resolve lost: no
			// final action over an amount Stripe won't charge.
			case loaded.BelowStripeMinimum():
				_, rerr := s.resolveListedRun(ctx, tenantID, run, domain.ResolutionAmountBelowMinimum, string(domain.ResolutionAmountBelowMinimum), nil)
				return rerr
			case loaded.PaymentInFlight():
				// A payment that may still take money: neither final action
				// fires and nothing is written. A cancel cannot be undone, and
				// canceling a customer whose payment then succeeds is what this
				// prevents. The run stays active with the lease it already
				// carries as next_action_at (the final retry and the
				// exhaust-on-entry claim both wrote it), so a dunning tick after
				// the lease looks again: a payment that succeeded resolves the
				// run as recovered, and one that failed reaches the final
				// actions through exhaust-on-entry. While the payment is
				// processing or unknown the due-run pickers skip the run, so the
				// wait costs no ticks. A parked invoice waits for the
				// stale-hold sweep: a payment Stripe shows settles it, and with
				// none a day after the attempt it goes back to failed or
				// pending and this run goes on. An operator's write-off closes
				// the run as not collectible without the subscription action.
				slog.Info("dunning final action held: a payment on this invoice may still go through; a later dunning tick checks again",
					"run_id", run.ID, "invoice_id", run.InvoiceID,
					"payment_status", loaded.PaymentStatus,
					"final_subscription_action", policy.FinalSubscriptionAction,
					"final_invoice_action", policy.FinalInvoiceAction)
				return nil
			}
			inv, invRead = loaded, true
		}
	}

	// Take the invoice's charge hold for the final action (ADR-127). Refused
	// means a hold is set: a charge is running (an auto-charge whose card
	// was just attached, a Collect), or one whose result was lost waits for
	// the stale-hold sweep to ask Stripe — which may find the customer paid.
	// Neither action fires and nothing is written, exactly like the
	// payment-in-flight branch above: the run keeps its lease as
	// next_action_at, the due-run pickers skip it while the hold is set, and
	// the first tick after the hold clears looks again. Held, no charge can
	// start until the subscription action has run.
	var hold domain.ChargeHold
	if invRead && s.chargeHolds != nil {
		h, err := s.chargeHolds.ClaimChargeHoldForFinalAction(ctx, tenantID, run.InvoiceID)
		if err != nil {
			return fmt.Errorf("dunning exhaust: take the charge hold on invoice %s: %w", run.InvoiceID, err)
		}
		if !h.Held() {
			slog.Info("dunning final action held: a charge holds this invoice (running, or its result is being confirmed with Stripe); a later dunning tick checks again",
				"run_id", run.ID, "invoice_id", run.InvoiceID,
				"final_subscription_action", policy.FinalSubscriptionAction,
				"final_invoice_action", policy.FinalInvoiceAction)
			return nil
		}
		hold = h
	}

	// Exhaustion fires TWO independent actions (ADR-112): one on the
	// subscription, one on the unpaid invoice. Either can fail on its own,
	// and a failed exhaustion is re-attempted 24h later — so each is skipped
	// when its target state already holds. Without that, a re-attempt would
	// die forever on the half that already succeeded (both subscription
	// movers refuse a terminal sub; MarkUncollectible returns "invoice is
	// already uncollectible") and never reach the half that did not.
	//
	// The run's terminal state is assigned AFTER both, so a swallowed mover
	// failure leaves the run requeryable (state=active) instead of a clean
	// "escalated" beside an invoice/sub that never got closed — escalated
	// runs are permanently excluded from both due-run pickers, so they'd
	// never be re-attempted.
	// Unknown invoice state: neither half fires. A policy with no terminal
	// actions has nothing to protect, but the read error still means the run
	// may already be paid, so the tick fails and the run is re-checked on the
	// next one — rather than escalating, or being labelled action_failed for
	// an action that does not exist.
	if invReadFailed && policy.FinalSubscriptionAction == domain.SubActionNone && policy.FinalInvoiceAction == domain.InvActionNone {
		return fmt.Errorf("dunning exhaust: read invoice %s for the paid re-check", run.InvoiceID)
	}
	actionFailed := invReadFailed

	// applied is what the escalation email renders. It records whether each
	// REQUESTED END STATE HOLDS — not whether this call performed the
	// mutation. The email describes the subscription's and invoice's state,
	// and "your subscription has been canceled" is true whoever canceled it;
	// keying on authorship instead would drop the sentence on exactly the
	// re-attempt paths this split introduced. It stays false when the action
	// could not apply to THIS invoice at all — a one-off has no subscription
	// to cancel, an unwired mover cannot act — so the debtor never reads
	// "your subscription has been canceled" when nothing was.
	var outcome appliedActions

	// The two halves need the invoice differently, so a missing reader
	// degrades them differently:
	//
	//   - the SUBSCRIPTION action needs inv.SubscriptionID and cannot run
	//     without it at all;
	//   - the INVOICE action needs only the id already on the run. inv.Status
	//     is a convergence aid (skip an already-written-off invoice rather
	//     than re-erroring on it), not a prerequisite.
	//
	// A read that FAILS is different from a reader that is absent: absent is
	// a known-degraded construction, failure means the state is unknown, and
	// acting blind there could email a lie. So a failure blocks both halves
	// and re-queues; absence blocks only the subscription half.

	// SUBSCRIPTION first: it is the half that stops future money movement,
	// and it matches Stripe's causality (there the subscription setting is
	// what marks the invoice uncollectible).
	//
	// The action runs as the hold's owner: a cancel can void this very
	// invoice (a wholly unused prebill, ADR-118), and that void refuses while
	// a hold is set unless the caller owns it.
	subCtx := domain.WithChargeHoldOwner(ctx, run.InvoiceID, hold)
	// invoiceSettled: the pause found the invoice no longer owed, so a
	// payment (or a void) committed after the late re-check above.
	invoiceSettled := false
	// waitUntil: the subscription action would close a period still inside
	// its invoice grace, so the whole exhaustion waits until then.
	var waitUntil time.Time
	if !actionFailed && policy.FinalSubscriptionAction != domain.SubActionNone {
		if !invRead {
			slog.Warn("dunning subscription action skipped — no invoice reader wired, so no subscription to act on",
				"run_id", run.ID, "invoice_id", run.InvoiceID)
		} else {
			subApplied, subFailed, subSettled, subWait := s.applySubscriptionAction(
				subCtx, tenantID, policy.FinalSubscriptionAction, inv.SubscriptionID, run.InvoiceID)
			actionFailed = actionFailed || subFailed
			invoiceSettled = subSettled
			waitUntil = subWait
			switch {
			case !subApplied:
			case policy.FinalSubscriptionAction == domain.SubActionPause:
				outcome.SubscriptionPaused = true
			case policy.FinalSubscriptionAction == domain.SubActionCancel:
				outcome.SubscriptionCanceled = true
			}
		}
	}

	// Release the hold before the invoice action: the write-off refuses
	// while a live hold is set, and re-checks the hold under its own row
	// lock. A failed release leaves the hold to expire; the write-off is then
	// refused and re-attempted with the rest of a failed exhaustion.
	if hold.Held() {
		if err := s.chargeHolds.ReleaseChargeHold(ctx, tenantID, run.InvoiceID, hold); err != nil {
			slog.Warn("dunning exhaust: releasing the charge hold failed — the stale-hold sweep clears it",
				"run_id", run.ID, "invoice_id", run.InvoiceID, "error", err)
		}
	}

	// The subscription action waits for the invoice grace (ADR-120
	// amendment 2026-10-22): nothing was changed, so neither the write-off
	// nor the escalation runs now. The hold is released above, so a payment
	// can still settle the invoice in the wait.
	if !waitUntil.IsZero() {
		return s.waitForInvoiceGrace(ctx, tenantID, run, policy, waitUntil)
	}

	// The pause found the invoice settled after the late re-check: close the
	// run from the invoice as it is now, as that re-check would have, rather
	// than escalate over a paid invoice or leave it action_failed for a day.
	if invoiceSettled {
		fresh, err := s.invoiceGet.Get(ctx, tenantID, run.InvoiceID)
		switch {
		case err != nil:
			slog.Warn("failed to re-read the invoice the dunning pause found settled", "invoice_id", run.InvoiceID, "error", err)
			actionFailed = true
		case fresh.Status == domain.InvoicePaid || fresh.PaymentStatus == domain.PaymentSucceeded:
			_, rerr := s.resolveListedRun(ctx, tenantID, run, domain.ResolutionPaymentRecovered, "payment_recovered", nil)
			return rerr
		case fresh.Status == domain.InvoiceVoided:
			_, rerr := s.resolveListedRun(ctx, tenantID, run, domain.ResolutionInvoiceVoided, "invoice_voided", nil)
			return rerr
		default:
			actionFailed = true
		}
	}

	// INVOICE second, and ONLY if the subscription half is settled. Writing
	// the invoice off while the subscription action is still outstanding
	// would strand it: the late-paid re-check at the top of this function
	// resolves any run whose invoice is already uncollectible, so the
	// re-attempt would return there and never retry the subscription.
	//
	// `inv` is the zero value when unread, whose Status is not
	// `uncollectible`, so the skip-if-done check simply does not fire and the
	// write-off is attempted — the pre-ADR-112 behaviour.
	// The cancel can change the invoice: its relief is written in the cancel's
	// transaction (ADR-118), and an invoice whose period was wholly unused is
	// voided there. That void resolves this run itself (invoice AfterVoid),
	// so with no invoice action the escalation below finds the run resolved
	// and writes nothing. With one, re-read the invoice before the
	// write-off: a voided invoice resolves the run here (a no-op CAS when
	// the void already did) instead of failing MarkUncollectible.
	if !actionFailed && outcome.SubscriptionCanceled && invRead && policy.FinalInvoiceAction != domain.InvActionNone {
		fresh, err := s.invoiceGet.Get(ctx, tenantID, run.InvoiceID)
		if err != nil {
			slog.Warn("failed to re-read invoice after the dunning cancel", "invoice_id", run.InvoiceID, "error", err)
			actionFailed = true
		} else if fresh.Status == domain.InvoiceVoided {
			_, rerr := s.resolveListedRun(ctx, tenantID, run, domain.ResolutionInvoiceVoided, "invoice_voided", nil)
			return rerr
		} else {
			inv = fresh
		}
	}
	if !actionFailed {
		invApplied, invFailed := s.applyInvoiceAction(
			ctx, tenantID, run.InvoiceID, policy.FinalInvoiceAction, inv)
		actionFailed = actionFailed || invFailed
		outcome.InvoiceWrittenOff = invApplied
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
		base := attemptAt
		if base.IsZero() {
			base = now
		}
		retryAt := base.Add(24 * time.Hour)
		run.NextActionAt = &retryAt
		// Guarded: a settle may have resolved this run during the failed terminal
		// action. Don't clobber that resolve back to active — the invoice is paid,
		// so resolved is the correct terminal state and no re-attempt is needed.
		if applied, err := s.store.UpdateRunIfActive(ctx, tenantID, run, run.AttemptCount, nil); err != nil {
			return err
		} else if !applied {
			slog.Info("dunning terminal action failed but run was resolved or escalated by another processor — leaving it",
				"run_id", run.ID, "invoice_id", run.InvoiceID)
			return nil
		}
		slog.Warn("dunning terminal action failed; run kept active for re-attempt",
			"run_id", run.ID, "invoice_id", run.InvoiceID,
			"final_subscription_action", policy.FinalSubscriptionAction,
			"final_invoice_action", policy.FinalInvoiceAction)
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
	// The escalation email rides this write's transaction (ADR-040); it
	// renders what actually applied to this invoice, not what the policy
	// asked for. Built before the write — plain reads — and enqueued by the
	// store inside the same tx under a SAVEPOINT.
	var escalateTx func(tx *sql.Tx) error
	if s.emailNotifier != nil && s.customerEmail != nil {
		escalateTx = s.buildDunningEscalationTx(ctx, tenantID, run, outcome)
	}
	if applied, err := s.store.UpdateRunIfActive(ctx, tenantID, run, run.AttemptCount, escalateTx); err != nil {
		return err
	} else if !applied {
		slog.Info("dunning exhaust: run resolved or escalated by another processor during the terminal action — not escalating",
			"run_id", run.ID, "invoice_id", run.InvoiceID)
		return nil
	}

	_, _ = s.store.CreateEvent(ctx, tenantID, domain.InvoiceDunningEvent{
		RunID:        run.ID,
		InvoiceID:    run.InvoiceID,
		EventType:    domain.DunningEventEscalated,
		State:        run.State,
		AttemptCount: run.AttemptCount,
		// What actually happened to this invoice, not what the policy asked
		// for — the timeline renders this row, and a one-off invoice under a
		// cancel policy would otherwise read "cancel" beside a subscription
		// that never existed.
		Reason:    outcome.Reason(),
		CreatedAt: now,
	})

	slog.Info("dunning exhausted",
		"run_id", run.ID,
		"invoice_id", run.InvoiceID,
		"final_subscription_action", policy.FinalSubscriptionAction,
		"final_invoice_action", policy.FinalInvoiceAction,
		"applied", outcome.Reason(),
	)

	s.fireRunEvent(ctx, tenantID, domain.EventDunningEscalated, run.ID, map[string]any{
		// The policy's two configured actions, and separately what actually
		// landed. Subscribers reconciling their own records need the second:
		// a one-off invoice under a cancel policy escalates with nothing
		// canceled, and an aggregate "final_action" could not say so.
		"final_subscription_action": string(policy.FinalSubscriptionAction),
		"final_invoice_action":      string(policy.FinalInvoiceAction),
		"applied":                   outcome.Reason(),
	})

	return nil
}

// appliedActions is what a terminal exhaustion actually did to ONE
// invoice. It lives in domain because the email package renders it —
// the escalation copy is a function of the OUTCOME, not of the policy.
type appliedActions = domain.DunningEscalationOutcome

// applySubscriptionAction moves the subscription to the policy's terminal
// state, reporting whether that state now holds, whether the attempt
// errored, whether the pause found the invoice no longer owed (paid after
// the late re-check), and, when the action waits for the invoice grace, the
// instant the grace ends (waitUntil, zero otherwise; see
// SubscriptionActionNotYet). See SubscriptionStateReader for why the
// skip-if-done read exists rather than tolerating the movers'
// already-terminal errors.
func (s *Service) applySubscriptionAction(ctx context.Context, tenantID string, action domain.DunningSubscriptionAction, subID, invoiceID string) (applied, failed, invoiceSettled bool, waitUntil time.Time) {
	if action == domain.SubActionNone {
		return false, false, false, time.Time{}
	}
	if subID == "" {
		// One-off invoice — no subscription exists to act on. Not a
		// failure; there is simply nothing to do, and the escalation email
		// must not claim otherwise.
		return false, false, false, time.Time{}
	}

	if s.subState != nil {
		st, err := s.subState.GetTerminalState(ctx, tenantID, subID)
		if err != nil {
			slog.Warn("failed to read subscription state for dunning terminal action",
				"subscription_id", subID, "error", err)
			return false, true, false, time.Time{}
		}
		if st.Terminated {
			// Already canceled — by this run's earlier attempt,
			// by another flow, or by an operator. A `cancel` request is
			// satisfied by that (whoever did it, the end state holds). A
			// `pause` request is NOT: a terminated subscription has no
			// collection left to pause, so nothing applied and the email
			// stays silent about it.
			return action == domain.SubActionCancel, false, false, time.Time{}
		}
		// On a test clock the action is dated at the escalation (exhaustRun),
		// and that can be before the current period began: one advance bills
		// the next renewal (the catchup's cycle phase) before it runs the
		// dunning work due before it, and an exhaustion re-driven in a later
		// advance keeps the earlier instant. A cancel dated before
		// current_period_start bills no final invoice and gives the period's
		// prebill no relief, since both read it as outside the period; a
		// pause would read as earlier than the renewal it followed. So the
		// action lands at the period's start: none of the period used, its
		// prebill relieved in full. The write-off and the escalation keep
		// their instant; only this ctx moves.
		if sim, ok := clock.SimOf(ctx); ok && sim.At.Before(st.PeriodStart) {
			ctx = clock.WithSim(ctx, clock.Sim{At: st.PeriodStart, TestClockID: sim.TestClockID})
		}
	}

	switch action {
	case domain.SubActionPause:
		if s.subPauser == nil {
			// Wiring gap (the producer always wires it). Logged rather than
			// failed: failing would re-queue every run in such a deployment
			// forever, and the email correctly claims nothing.
			slog.Warn("dunning pause action skipped — no subscription pauser wired",
				"subscription_id", subID)
			return false, false, false, time.Time{}
		}
		paused, err := s.subPauser.PauseCollection(ctx, tenantID, subID, invoiceID)
		if until, ok := graceWaitUntil(err); ok {
			return false, false, false, until
		}
		if err != nil {
			slog.Warn("failed to pause collection after dunning exhausted",
				"subscription_id", subID, "error", err)
			return false, true, false, time.Time{}
		}
		if !paused {
			// The invoice was paid after the late paid re-check and before
			// the pause could land: nothing is paused, and exhaustRun closes
			// the run from the invoice instead of escalating it.
			slog.Info("dunning pause skipped: the invoice was paid first",
				"subscription_id", subID, "invoice_id", invoiceID)
			return false, false, true, time.Time{}
		}
		slog.Info("collection paused by dunning", "subscription_id", subID)
		return true, false, false, time.Time{}

	case domain.SubActionCancel:
		if s.subCanceler == nil {
			slog.Warn("dunning cancel action skipped — no subscription canceler wired",
				"subscription_id", subID)
			return false, false, false, time.Time{}
		}
		err := s.subCanceler.Cancel(ctx, tenantID, subID)
		if until, ok := graceWaitUntil(err); ok {
			return false, false, false, until
		}
		if err != nil {
			slog.Warn("failed to cancel subscription after dunning exhausted",
				"subscription_id", subID, "error", err)
			return false, true, false, time.Time{}
		}
		slog.Info("subscription canceled by dunning", "subscription_id", subID)
		return true, false, false, time.Time{}

	default:
		// Unreachable while the CHECK constraint holds. Loud rather than
		// silently no-op: a value the code does not understand must not
		// read as "the operator asked for nothing".
		slog.Error("unknown dunning final_subscription_action — no action taken",
			"action", action, "subscription_id", subID)
		return false, false, false, time.Time{}
	}
}

// graceWaitUntil reports whether err is a SubscriptionActionNotYet, and the
// instant its grace ends.
func graceWaitUntil(err error) (time.Time, bool) {
	var notYet *SubscriptionActionNotYet
	if errors.As(err, &notYet) {
		return notYet.Until, true
	}
	return time.Time{}, false
}

// waitForInvoiceGrace defers an exhaustion whose subscription action waits
// for the invoice grace (SubscriptionActionNotYet): the run's next_action_at
// moves from this exhaustion's lease to until, the end of the grace, and
// nothing else is written. The run stays active and keeps its attempt count.
// A first exhaustion is not labelled action_failed; a re-attempt of an
// earlier failed action keeps that resolution, which stays true (the last
// attempt of the action failed, and this wait did not attempt it) until the
// action runs and replaces it. No write-off, escalation row, email or
// webhook fires. When until comes, the due-run pickers list the run again and
// exhaust-on-entry runs both final actions, now closing the period with the
// usage that arrived inside the grace (ADR-120 amendment 2026-10-22).
//
// The write is ClaimExhaustion's compare-and-swap with this exhaustion's
// lease as the expected value, so it lands only while this processor still
// holds the run: if the lease ran out and another processor claimed the run
// again, or a payment resolved it, nothing is written. It touches only
// next_action_at, never an operator's paused flag. A failed write leaves the
// lease, which expires into a re-drive.
func (s *Service) waitForInvoiceGrace(ctx context.Context, tenantID string, run domain.InvoiceDunningRun, policy domain.DunningPolicy, until time.Time) error {
	if run.NextActionAt == nil { // unreachable: both exhaustion paths set the lease first
		return fmt.Errorf("dunning exhaust: run %s has no lease to defer from", run.ID)
	}
	until = until.Truncate(time.Microsecond)
	won, err := s.store.ClaimExhaustion(ctx, tenantID, run.ID, run.AttemptCount, *run.NextActionAt, until)
	if err != nil {
		return fmt.Errorf("dunning exhaust: defer run %s to the end of the invoice grace: %w", run.ID, err)
	}
	if !won {
		slog.Info("dunning final action waits for the invoice grace, but the run was claimed, resolved or escalated by another processor — leaving it",
			"run_id", run.ID, "invoice_id", run.InvoiceID)
		return nil
	}
	slog.Info("dunning final action waits for the invoice grace; the run is due again when it ends",
		"run_id", run.ID, "invoice_id", run.InvoiceID,
		"final_subscription_action", policy.FinalSubscriptionAction,
		"final_invoice_action", policy.FinalInvoiceAction,
		"next_action_at", until)
	return nil
}

// applyInvoiceAction writes the unpaid invoice off when the policy asks,
// reporting whether it is now written off and whether the attempt errored.
func (s *Service) applyInvoiceAction(ctx context.Context, tenantID, invoiceID string, action domain.DunningInvoiceAction, inv domain.Invoice) (applied, failed bool) {
	switch action {
	case domain.InvActionNone:
		return false, false

	case domain.InvActionMarkUncollectible:
		if inv.Status == domain.InvoiceUncollectible {
			// Already written off. MarkUncollectible returns "invoice is
			// already uncollectible" rather than a no-op, and treating that
			// as a failure would re-queue the run forever. (exhaustRun's
			// late-paid re-check normally resolves such a run before
			// reaching here; this covers an operator write-off landing
			// between the two reads.)
			return true, false
		}
		if s.invoiceUncollect == nil {
			slog.Warn("dunning write-off skipped — no invoice uncollectible marker wired",
				"invoice_id", invoiceID)
			return false, false
		}
		if err := s.invoiceUncollect.MarkUncollectible(ctx, tenantID, invoiceID); err != nil {
			slog.Warn("failed to mark invoice uncollectible after dunning exhausted",
				"invoice_id", invoiceID, "error", err)
			return false, true
		}
		slog.Info("invoice marked uncollectible by dunning", "invoice_id", invoiceID)
		return true, false

	default:
		slog.Error("unknown dunning final_invoice_action — no action taken",
			"action", action, "invoice_id", invoiceID)
		return false, false
	}
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
// The invoice write-off runs first; if it fails (e.g. the invoice was paid
// via webhook between dialog open and submit) the error is returned and the
// run stays open. If the run update then fails, a retry finds the invoice
// already written off and only resolves the run.
func (s *Service) ResolveRun(ctx context.Context, tenantID, runID string, resolution domain.DunningResolution) (domain.InvoiceDunningRun, error) {
	run, err := s.store.GetRun(ctx, tenantID, runID)
	if err != nil {
		return domain.InvoiceDunningRun{}, err
	}

	// The write-off runs BEFORE the run is resolved, like the operator's
	// recovered/voided actions: resolving first and then failing the write-off
	// left a resolved run beside an invoice still open, which nothing revisits.
	// An invoice already written off (a race with the automated terminal
	// action) is the requested end state, so it proceeds to resolve.
	if resolution == domain.ResolutionInvoiceNotCollectible && s.invoiceUncollect != nil && run.InvoiceID != "" {
		done, err := s.invoiceIn(ctx, tenantID, run.InvoiceID, domain.InvoiceUncollectible)
		if err != nil {
			return domain.InvoiceDunningRun{}, err
		}
		if !done {
			if err := s.invoiceUncollect.MarkUncollectible(ctx, tenantID, run.InvoiceID); err != nil {
				return domain.InvoiceDunningRun{}, fmt.Errorf("mark invoice uncollectible: %w", err)
			}
		}
	}

	// Resolve via the shared CAS path so an operator resolve racing an automated
	// resolver (a card settle or the scheduler) fires dunning.resolved exactly once —
	// the CAS winner owns the timeline row + the outbound webhook.
	return s.resolveRunNow(ctx, tenantID, run, resolution, string(resolution))
}

// invoiceIn reports whether the invoice is already in status want — an
// operator resolution whose invoice side already happened (a retry after the
// run update failed, or a race with a webhook or the automated action) is
// done, not an error. Without an invoice reader wired it reports false and
// the writer's own state check decides.
func (s *Service) invoiceIn(ctx context.Context, tenantID, invoiceID string, want domain.InvoiceStatus) (bool, error) {
	if s.invoiceGet == nil {
		return false, nil
	}
	inv, err := s.invoiceGet.Get(ctx, tenantID, invoiceID)
	if err != nil {
		return false, fmt.Errorf("read invoice %s: %w", invoiceID, err)
	}
	return inv.Status == want, nil
}

// resolveRunNow transitions an already-loaded run to resolved with the given
// resolution: stamps state/resolution/resolved_at, clears next_action_at, writes
// the DunningEventResolved row, persists, and fires EventDunningResolved. Binds
// the clock to the invoice so resolved_at lands in simulated time on clock-pinned
// invoices. Shared by ResolveByInvoice, the processRun success branch, and the
// processRun paid-pre-check so the transition is identical across all of them.
func (s *Service) resolveRunNow(ctx context.Context, tenantID string, run domain.InvoiceDunningRun, resolution domain.DunningResolution, eventReason string) (domain.InvoiceDunningRun, error) {
	return s.resolveRunAt(ctx, tenantID, run, resolution, eventReason, nil, false)
}

// resolveListedRun is the automated processRun/exhaustRun resolve: it acts on
// a run it listed as active, so the CAS also requires state = 'active' — a
// stale processor never rewrites a run another processor escalated meanwhile.
func (s *Service) resolveListedRun(ctx context.Context, tenantID string, run domain.InvoiceDunningRun, resolution domain.DunningResolution, eventReason string, at *time.Time) (domain.InvoiceDunningRun, error) {
	return s.resolveRunAt(ctx, tenantID, run, resolution, eventReason, at, true)
}

// resolveRunAt is resolveRunNow with an explicit resolve instant. at nil
// derives the clock (settle/operator paths — their contract IS "now");
// the retry-success path passes its anchored contracted instant
// (run.NextActionAt at fire time) so resolved_at and the resolved
// timeline row land on the retry that recovered the invoice, not on
// wherever the clock advance happened to end (ADR-030 contracted-instant
// rule; same split as trial flips, scheduled cancels, pause resumes).
func (s *Service) resolveRunAt(ctx context.Context, tenantID string, run domain.InvoiceDunningRun, resolution domain.DunningResolution, eventReason string, at *time.Time, fromActiveOnly bool) (domain.InvoiceDunningRun, error) {
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
	won, err := s.store.ResolveRun(ctx, tenantID, run, fromActiveOnly)
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

	s.fireRunEvent(ctx, tenantID, domain.EventDunningResolved, run.ID, nil)
	return run, nil
}

// ResolveByInvoice resolves any active dunning run for the given invoice.
// Called when an invoice is voided or paid outside of dunning.
func (s *Service) ResolveByInvoice(ctx context.Context, tenantID, invoiceID string, resolution domain.DunningResolution) error {
	run, err := s.store.GetActiveRunByInvoice(ctx, tenantID, invoiceID)
	if errors.Is(err, errs.ErrNotFound) {
		return nil // No active run — nothing to resolve
	}
	if err != nil {
		// A read error is not "no run": swallowing it left the run active
		// beside a paid or voided invoice while the caller saw success.
		return fmt.Errorf("look up the invoice's active dunning run: %w", err)
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
// an existing one (when input.ID is set), through the save-time validation
// every writer shares (normalizeAndValidatePolicy).
func (s *Service) UpsertPolicy(ctx context.Context, tenantID string, policy domain.DunningPolicy) (domain.DunningPolicy, error) {
	policy, err := normalizeAndValidatePolicy(policy)
	if err != nil {
		return domain.DunningPolicy{}, err
	}
	return s.store.UpsertPolicy(ctx, tenantID, policy)
}

// UpsertPolicyTx is the tx-aware upsert used by the recipe-instantiate path.
// It runs the SAME normalize+validate as UpsertPolicy: the recipe schema layer
// does not check the schedule's bounds (recipe/parse.go), and before #333 this
// path skipped validation, so a recipe whose retry count outran its waits
// stalled its campaign at retry time instead of failing at instantiate time.
// One validation chokepoint for every writer; a policy the API path would
// reject must never enter via a recipe.
func (s *Service) UpsertPolicyTx(ctx context.Context, tx *sql.Tx, tenantID string, policy domain.DunningPolicy) (domain.DunningPolicy, error) {
	policy, err := normalizeAndValidatePolicy(policy)
	if err != nil {
		return domain.DunningPolicy{}, err
	}
	return s.store.UpsertPolicyTx(ctx, tx, tenantID, policy)
}

// DefaultSeedPolicy is the policy every new account starts with, once per
// mode (ADR-123, #1152). Three retries on day 3, 7 and 14 after the failed
// payment (or the collection that found no card): Stripe's recommended two-week window, and its limit of three
// retries on a custom schedule. When retries run out it changes nothing:
// the subscription keeps billing and the invoice stays open and due
// (Stripe's "leave the subscription overdue"). A machine should not pause,
// cancel or write off on an account's behalf before the operator has chosen
// to. Both final actions are written explicitly, because an empty value
// normalizes to pause.
func DefaultSeedPolicy() domain.DunningPolicy {
	return domain.DunningPolicy{
		Name:                    "Default retries",
		Enabled:                 true,
		RetryDays:               []int{3, 4, 7},
		FinalSubscriptionAction: domain.SubActionNone,
		FinalInvoiceAction:      domain.InvActionNone,
	}
}

// SeedDefaultPoliciesTx creates DefaultSeedPolicy in test and live mode on
// the caller's account-creation tx, as each mode's default (ADR-123). It is
// the one writer of seeded policies; both account-creation paths
// (tenant.RunBootstrap and tenant.Service.Create) call it, so an account
// that exists has retries on from its first failed payment. The policy goes
// through the same normalize+validate as every other writer. onSeeded runs
// after each insert in that insert's mode (the callers write an audit row
// there); an error from it, or from either insert, fails account creation.
func (s *Service) SeedDefaultPoliciesTx(ctx context.Context, tx *sql.Tx, tenantID string, onSeeded func(p domain.DunningPolicy, livemode bool) error) error {
	policy, err := normalizeAndValidatePolicy(DefaultSeedPolicy())
	if err != nil {
		return fmt.Errorf("default dunning policy: %w", err)
	}
	return s.store.SeedDefaultPoliciesTx(ctx, tx, tenantID, policy, onSeeded)
}

// normalizeAndValidatePolicy applies the save-time defaults and enforces the
// policy invariants. The single chokepoint shared by UpsertPolicy and
// UpsertPolicyTx so no writer can persist a policy another path would reject.
func normalizeAndValidatePolicy(policy domain.DunningPolicy) (domain.DunningPolicy, error) {
	if err := validateRetryDays(policy.RetryDays); err != nil {
		return domain.DunningPolicy{}, err
	}
	// Default = pause (Stripe-aligned pause_collection.behavior=
	// keep_as_draft). Without it the engine keeps generating finalized
	// invoices for a delinquent customer every cycle, stacking failures and
	// dunning emails.
	if policy.FinalSubscriptionAction == "" {
		policy.FinalSubscriptionAction = domain.SubActionPause
	}
	// Default = leave the invoice open. Deliberately NOT a write-off, even
	// though Recurly defaults its equivalent on: writing off asserts bad
	// debt, and a machine should not make that assertion on a fresh
	// tenant's behalf unasked (ADR-112, same reasoning as ADR-111).
	if policy.FinalInvoiceAction == "" {
		policy.FinalInvoiceAction = domain.InvActionNone
	}
	if !policy.FinalSubscriptionAction.Valid() {
		return domain.DunningPolicy{}, errs.Invalid("final_subscription_action",
			"must be one of: none, pause, cancel")
	}
	if !policy.FinalInvoiceAction.Valid() {
		return domain.DunningPolicy{}, errs.Invalid("final_invoice_action",
			"must be one of: none, mark_uncollectible")
	}
	if err := domain.MaxLen("name", policy.Name, 255); err != nil {
		return domain.DunningPolicy{}, err
	}
	return policy, nil
}

// validateRetryDays checks the schedule: 1 to MaxDunningRetries waits, each a
// whole number of days from 1 to MaxDunningRetryWait. There is no default:
// a policy without a schedule is refused rather than given one the caller
// did not choose. Whole days are the unit, so a wait of part of a day
// cannot be written (the JSON decode refuses 1.5 or "36h" before this).
func validateRetryDays(days []int) error {
	if len(days) == 0 {
		return errs.Invalid("retry_days", "retry_days is required: the days to wait before each retry, for example [3, 4, 7]")
	}
	if len(days) > domain.MaxDunningRetries {
		return errs.Invalid("retry_days", fmt.Sprintf("at most %d retries; got %d", domain.MaxDunningRetries, len(days)))
	}
	for i, d := range days {
		if d < 1 || d > domain.MaxDunningRetryWait {
			return errs.Invalid("retry_days", fmt.Sprintf("entry %d is %d; each wait is a whole number of days from 1 to %d", i, d, domain.MaxDunningRetryWait))
		}
	}
	return nil
}

// ListRuns returns dunning runs matching the filter.
func (s *Service) ListRuns(ctx context.Context, filter RunListFilter) ([]domain.InvoiceDunningRun, int, error) {
	return s.store.ListRuns(ctx, filter)
}

func (s *Service) GetStats(ctx context.Context, tenantID string) (Stats, error) {
	return s.store.GetStats(ctx, tenantID)
}

// buildDunningWarningTx resolves the customer's email + invoice context
// (plain reads, done BEFORE the state transaction opens) and returns the
// hook that enqueues the dunning-warning email on the reschedule's own tx
// (ADR-040). nil when the recipient cannot be resolved — logged, best-effort,
// exactly the old posture.
func (s *Service) buildDunningWarningTx(ctx context.Context, tenantID string, run domain.InvoiceDunningRun, policy domain.DunningPolicy, retryErrMsg string, ask domain.DeclineAsk) func(tx *sql.Tx) error {
	email, name, cc, err := s.customerEmail.GetCustomerEmail(ctx, tenantID, run.CustomerID)
	if err != nil || email == "" {
		slog.Warn("skip dunning warning email — cannot resolve customer email",
			"run_id", run.ID, "customer_id", run.CustomerID, "error", err)
		return nil
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
	return func(tx *sql.Tx) error {
		return s.emailNotifier.SendDunningWarningTx(ctx, tx, tenantID, email, cc, name, invoiceNumber, run.AttemptCount, policy.Retries(), nextRetry, retryErrMsg, publicToken, ask)
	}
}

// retryAsk is what a failed retry asks of the customer: a different card
// (#966) when the retry skipped a card the bank had refused for good
// (domain.ErrCardNeedsReplacing), the confirmation the bank wants when it
// skipped a card for that (domain.ErrCardNeedsConfirmation), or what its
// charge drew (RetryError.Ask).
func retryAsk(retryErr error) domain.DeclineAsk {
	switch {
	case errors.Is(retryErr, domain.ErrCardNeedsReplacing):
		return domain.DeclineAskNewCard
	case errors.Is(retryErr, domain.ErrCardNeedsConfirmation):
		return domain.DeclineAskConfirm
	}
	var re *RetryError
	if errors.As(retryErr, &re) {
		return re.Ask
	}
	return domain.DeclineAskNone
}

// buildDunningEscalationTx is the escalation counterpart of
// buildDunningWarningTx: action is what ACTUALLY applied to this invoice —
// the email asserts an outcome, so it renders outcomes, not intentions.
func (s *Service) buildDunningEscalationTx(ctx context.Context, tenantID string, run domain.InvoiceDunningRun, outcome domain.DunningEscalationOutcome) func(tx *sql.Tx) error {
	email, name, cc, err := s.customerEmail.GetCustomerEmail(ctx, tenantID, run.CustomerID)
	if err != nil || email == "" {
		slog.Warn("skip dunning escalation email — cannot resolve customer email",
			"run_id", run.ID, "customer_id", run.CustomerID, "error", err)
		return nil
	}
	invoiceNumber := run.InvoiceID
	var publicToken string
	if s.invoiceGet != nil {
		if inv, err := s.invoiceGet.Get(ctx, tenantID, run.InvoiceID); err == nil {
			invoiceNumber = inv.InvoiceNumber
			publicToken = inv.PublicToken
		}
	}
	return func(tx *sql.Tx) error {
		return s.emailNotifier.SendDunningEscalationTx(ctx, tx, tenantID, email, cc, name, invoiceNumber, outcome, publicToken)
	}
}
