package dunning

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// recordingRetrier counts RetryPayment calls so a test can prove whether the
// paid-pre-check short-circuited (retrier NOT called) or fell through (called).
type recordingRetrier struct{ calls int }

func (r *recordingRetrier) RetryPayment(_ context.Context, _, _, _ string) error {
	r.calls++
	return nil
}

// stubInvGet serves a fixed invoice (or error) as the dunning InvoiceGetter.
type stubInvGet struct {
	inv domain.Invoice
	err error
}

func (s stubInvGet) Get(_ context.Context, _, _ string) (domain.Invoice, error) {
	return s.inv, s.err
}

type recordingSubCanceler struct{ calls int }

func (c *recordingSubCanceler) Cancel(_ context.Context, _, _ string) error { c.calls++; return nil }

type recordingPauser struct{ calls int }

func (p *recordingPauser) PauseCollection(_ context.Context, _, _ string) error {
	p.calls++
	return nil
}

// dueRunAt seeds an active run for inv_1 that is due now (backdated
// next_action_at) with the given attempt count.
func dueRunAt(t *testing.T, store *memStore, svc *Service, attempt int) domain.InvoiceDunningRun {
	t.Helper()
	run, err := svc.StartDunning(context.Background(), "t1", "inv_1", "cus_1", time.Now(), domain.DunningCausePaymentFailed)
	if err != nil {
		t.Fatalf("start dunning: %v", err)
	}
	run.AttemptCount = attempt
	past := time.Now().UTC().Add(-time.Hour)
	run.NextActionAt = &past
	store.runs[run.ID] = run
	return run
}

func paidInv() domain.Invoice {
	return domain.Invoice{
		ID: "inv_1", TenantID: "t1", SubscriptionID: "sub_1",
		Status: domain.InvoicePaid, PaymentStatus: domain.PaymentSucceeded,
	}
}

// resolvingRetrier simulates a synchronous card-settle: the retry succeeds and settles
// the invoice INLINE, resolving the dunning run by re-fetching it from the store — as
// payment.SettleSucceeded -> ResolveByInvoice does inside RetryPayment.
type resolvingRetrier struct{ svc *Service }

func (r *resolvingRetrier) RetryPayment(ctx context.Context, tenantID, invoiceID, _ string) error {
	_ = r.svc.ResolveByInvoice(ctx, tenantID, invoiceID, domain.ResolutionPaymentRecovered)
	return nil
}

type transientRetrier struct{}

func (transientRetrier) RetryPayment(_ context.Context, _, _, _ string) error {
	return ErrTransientSkip
}

// resolvingTransientRetrier models the ambiguous PI-may-have-succeeded outcome: the
// charge's PaymentIntent actually succeeded and its webhook resolved the run during
// the charge window, but our client then saw a timeout → ErrTransientSkip.
type resolvingTransientRetrier struct{ svc *Service }

func (r *resolvingTransientRetrier) RetryPayment(ctx context.Context, tenantID, invoiceID, _ string) error {
	_ = r.svc.ResolveByInvoice(ctx, tenantID, invoiceID, domain.ResolutionPaymentRecovered)
	return ErrTransientSkip
}

// TestProcessRun_TransientSkip_DoesNotClobberConcurrentResolve locks the guard on the
// transient-skip rewind: when the ambiguous PI-may-have-succeeded outcome races a
// concurrent settle-webhook resolve, the rewind must NOT un-resolve the run back to
// active (which would let a later tick re-resolve and re-fire dunning.resolved). The
// run stays resolved and dunning.resolved fires exactly once.
func TestProcessRun_TransientSkip_DoesNotClobberConcurrentResolve(t *testing.T) {
	store := newMemStore()
	disp := &captureDispatcher{}
	svc := NewService(store, &noopRetrier{}, nil)
	svc.SetEventDispatcher(disp)
	svc.SetRetrier(&resolvingTransientRetrier{svc: svc})

	run := dueRunAt(t, store, svc, 1)
	svc.ProcessDueRuns(context.Background(), "t1", 20)

	if got := store.runs[run.ID]; got.State != domain.DunningResolved {
		t.Fatalf("the transient-skip rewind must NOT clobber the concurrent resolve back to active: got state=%q", got.State)
	}
	// A second tick must not re-pick a resolved run and re-fire; overall exactly one.
	svc.ProcessDueRuns(context.Background(), "t1", 20)
	if n := disp.countOf(domain.EventDunningResolved); n != 1 {
		t.Errorf("dunning.resolved fired %d times, want exactly 1 (a clobbered resolve would re-fire)", n)
	}
}

// TestProcessRun_RecordsAttemptBeforeCharge_ResolverSeesFullCount locks the
// record-attempt-before-charge fix: when a retry succeeds SYNCHRONOUSLY (a saved-card
// off-session charge that settles inline and resolves the run via ResolveByInvoice
// INSIDE RetryPayment), the resolver re-fetches the run and must see the FULL
// attempt_count for THIS attempt — because processRun now persists the increment
// before the charge. Pre-fix the increment was in-memory only, so the resolved run
// recorded one fewer attempt.
func TestProcessRun_RecordsAttemptBeforeCharge_ResolverSeesFullCount(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, &noopRetrier{}, nil)
	svc.SetRetrier(&resolvingRetrier{svc: svc})

	run := dueRunAt(t, store, svc, 1) // was attempt 1 → this tick is attempt 2
	svc.ProcessDueRuns(context.Background(), "t1", 20)

	got := store.runs[run.ID]
	if got.State != domain.DunningResolved {
		t.Fatalf("state: got %q, want resolved", got.State)
	}
	if got.AttemptCount != 2 {
		t.Errorf("resolved run must record the FULL attempt count: got %d, want 2 (the attempt is persisted before the synchronous settle resolves it)", got.AttemptCount)
	}
}

// TestProcessRun_TransientSkip_FullyRewindsPersistedState locks that a transient skip
// (breaker/timeout — the Stripe call never happened) fully rewinds the attempt the fix
// persists before the charge: attempt_count + last_attempt_at end exactly as they were
// before the tick, and the run stays active — so an infrastructure blip never burns a
// retry from the budget.
func TestProcessRun_TransientSkip_FullyRewindsPersistedState(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, &transientRetrier{}, nil)

	run := dueRunAt(t, store, svc, 1)
	before := store.runs[run.ID]
	svc.ProcessDueRuns(context.Background(), "t1", 20)

	got := store.runs[run.ID]
	if got.AttemptCount != before.AttemptCount {
		t.Errorf("transient skip must rewind attempt_count: got %d, want %d (unchanged — a blip must not burn a retry)", got.AttemptCount, before.AttemptCount)
	}
	if got.State != domain.DunningActive {
		t.Errorf("transient skip must leave the run active: got %q, want active", got.State)
	}
	if (got.LastAttemptAt == nil) != (before.LastAttemptAt == nil) {
		t.Errorf("transient skip must restore last_attempt_at (nil-ness changed): got %v, before %v", got.LastAttemptAt, before.LastAttemptAt)
	}
}

// TestProcessRun_PaidInvoice_ResolvesWithoutRetrying: the paid-pre-check resolves
// a run whose invoice settled out-of-band as payment_recovered, WITHOUT calling
// the retrier or bumping the attempt. This is the backstop for the confirmed bug
// (a credit-cover sweep that MarkPaids without resolving).
func TestProcessRun_PaidInvoice_ResolvesWithoutRetrying(t *testing.T) {
	store := newMemStore()
	retrier := &recordingRetrier{}
	svc := NewService(store, retrier, nil)
	svc.SetSubscriptionPauser(&recordingPauser{}, stubInvGet{inv: paidInv()})

	run := dueRunAt(t, store, svc, 1)
	svc.ProcessDueRuns(context.Background(), "t1", 20)

	got := store.runs[run.ID]
	if got.State != domain.DunningResolved {
		t.Errorf("state: got %q, want resolved", got.State)
	}
	if got.Resolution != domain.ResolutionPaymentRecovered {
		t.Errorf("resolution: got %q, want payment_recovered", got.Resolution)
	}
	if got.AttemptCount != 1 {
		t.Errorf("attempt must not be bumped by the paid pre-check: got %d, want 1", got.AttemptCount)
	}
	if retrier.calls != 0 {
		t.Errorf("retrier must NOT run on a paid invoice: calls=%d", retrier.calls)
	}
}

// TestProcessRun_MaxRetriesPaid_DoesNotCancelSubscription is the load-bearing
// case: a run AT max retries whose invoice was settled out-of-band must NOT reach
// exhaustRun — which (final_action=cancel_subscription) would CANCEL a paying
// customer's subscription on a fully-paid invoice. The paid-pre-check must run
// BEFORE the max-retries→exhaustRun branch.
func TestProcessRun_MaxRetriesPaid_DoesNotCancelSubscription(t *testing.T) {
	store := newMemStore()
	p := store.policies[store.defaultID]
	p.FinalAction = domain.DunningActionCancelSubscription // would cancel if exhaustRun reached
	store.policies[store.defaultID] = p

	canceler := &recordingSubCanceler{}
	svc := NewService(store, &recordingRetrier{}, nil)
	svc.SetSubscriptionPauser(&recordingPauser{}, stubInvGet{inv: paidInv()})
	svc.SetSubscriptionCanceler(canceler)

	run := dueRunAt(t, store, svc, 3) // == default MaxRetryAttempts → would hit exhaustRun
	svc.ProcessDueRuns(context.Background(), "t1", 20)

	if canceler.calls != 0 {
		t.Fatalf("CANCELLED the subscription on a PAID invoice (Cancel calls=%d) — the paid-pre-check must precede the exhaustRun branch", canceler.calls)
	}
	got := store.runs[run.ID]
	if got.State != domain.DunningResolved || got.Resolution != domain.ResolutionPaymentRecovered {
		t.Errorf("max-retries paid run must resolve recovered: got state=%q resolution=%q", got.State, got.Resolution)
	}
}

// TestProcessRun_GatesOnStatusNotAmountDue: a finalized invoice that is NOT paid
// (PaymentPending) must NOT trip the pre-check even at $0 amount due (a
// mid-tax-retry draft can momentarily show $0) — it falls through to the normal
// retry path, proven by the retrier being called.
func TestProcessRun_GatesOnStatusNotAmountDue(t *testing.T) {
	store := newMemStore()
	retrier := &recordingRetrier{}
	svc := NewService(store, retrier, nil)
	inv := domain.Invoice{
		ID: "inv_1", TenantID: "t1",
		Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending, AmountDueCents: 0,
	}
	svc.SetSubscriptionPauser(&recordingPauser{}, stubInvGet{inv: inv})

	dueRunAt(t, store, svc, 1)
	svc.ProcessDueRuns(context.Background(), "t1", 20)

	if retrier.calls != 1 {
		t.Errorf("a finalized+pending invoice must fall through to retry (gate on STATUS, not amount_due): retrier calls=%d, want 1", retrier.calls)
	}
}

// TestProcessRun_VoidedInvoice_ResolvesManually: a voided invoice resolves the
// run as manually_resolved without retrying.
func TestProcessRun_VoidedInvoice_ResolvesManually(t *testing.T) {
	store := newMemStore()
	retrier := &recordingRetrier{}
	svc := NewService(store, retrier, nil)
	inv := domain.Invoice{ID: "inv_1", TenantID: "t1", Status: domain.InvoiceVoided}
	svc.SetSubscriptionPauser(&recordingPauser{}, stubInvGet{inv: inv})

	run := dueRunAt(t, store, svc, 1)
	svc.ProcessDueRuns(context.Background(), "t1", 20)

	got := store.runs[run.ID]
	if got.State != domain.DunningResolved || got.Resolution != domain.ResolutionManuallyResolved {
		t.Errorf("voided invoice: got state=%q resolution=%q, want resolved/manually_resolved", got.State, got.Resolution)
	}
	if retrier.calls != 0 {
		t.Errorf("retrier must not run on a voided invoice: calls=%d", retrier.calls)
	}
}

// TestProcessRun_InvoiceGetError_FallsThrough: a transient invoiceGet error must
// NOT resolve or skip — fall through to the normal retry path so a DB blip never
// burns or loses an attempt.
func TestProcessRun_InvoiceGetError_FallsThrough(t *testing.T) {
	store := newMemStore()
	retrier := &recordingRetrier{}
	svc := NewService(store, retrier, nil)
	svc.SetSubscriptionPauser(&recordingPauser{}, stubInvGet{err: errors.New("db blip")})

	dueRunAt(t, store, svc, 1)
	svc.ProcessDueRuns(context.Background(), "t1", 20)

	if retrier.calls != 1 {
		t.Errorf("invoiceGet error must fall through to retry: retrier calls=%d, want 1", retrier.calls)
	}
}

// unpaidInv is a finalized+pending invoice — falls through the tick-start
// paid-pre-check (gate is on terminal STATUS, not amount_due).
func unpaidInv() domain.Invoice {
	return domain.Invoice{
		ID: "inv_1", TenantID: "t1", SubscriptionID: "sub_1",
		Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending,
	}
}

// flagInvGet serves unpaid until *paid flips true (an out-of-band settle), then
// paid — so a test can settle the invoice DURING the retry window.
type flagInvGet struct{ paid *bool }

func (g flagInvGet) Get(_ context.Context, _, _ string) (domain.Invoice, error) {
	if *g.paid {
		return paidInv(), nil
	}
	return unpaidInv(), nil
}

// settlingDeclineRetrier models an out-of-band settle landing DURING the charge:
// it flips the invoice to paid and returns a hard decline, so the run falls
// through to exhaustRun on an invoice that is now paid.
type settlingDeclineRetrier struct{ paid *bool }

func (r *settlingDeclineRetrier) RetryPayment(_ context.Context, _, _, _ string) error {
	*r.paid = true
	return errors.New("card declined")
}

// TestExhaustRun_LatePaidRecheck_DoesNotCancel locks the late re-check: an invoice
// that settles out-of-band DURING the final retry's Stripe round-trip (unpaid at
// tick start, paid by the time exhaustRun runs) must resolve at exhaust, NOT cancel
// a now-paying customer's subscription. The tick-start paid-pre-check can't catch
// this — the settle lands after it.
func TestExhaustRun_LatePaidRecheck_DoesNotCancel(t *testing.T) {
	store := newMemStore()
	p := store.policies[store.defaultID]
	p.FinalAction = domain.DunningActionCancelSubscription
	store.policies[store.defaultID] = p

	paid := false
	canceler := &recordingSubCanceler{}
	svc := NewService(store, &noopRetrier{}, nil)
	svc.SetRetrier(&settlingDeclineRetrier{paid: &paid})
	svc.SetSubscriptionPauser(&recordingPauser{}, flagInvGet{paid: &paid})
	svc.SetSubscriptionCanceler(canceler)

	// attempt 2 → this tick is the final (3rd) attempt → a failed charge reaches exhaustRun.
	run := dueRunAt(t, store, svc, 2)
	svc.ProcessDueRuns(context.Background(), "t1", 20)

	if canceler.calls != 0 {
		t.Fatalf("late re-check failed: CANCELLED a paying customer's subscription (Cancel calls=%d) — an invoice that settled mid-tick must resolve at exhaust, not cancel", canceler.calls)
	}
	got := store.runs[run.ID]
	if got.State != domain.DunningResolved || got.Resolution != domain.ResolutionPaymentRecovered {
		t.Errorf("mid-tick settle must resolve recovered at exhaust: got state=%q resolution=%q", got.State, got.Resolution)
	}
}

// resolvingCanceler models a settle landing DURING the terminal Cancel call: the
// cancel succeeds, but a concurrent settle resolves the run mid-call (as
// SettleSucceeded -> ResolveByInvoice would).
type resolvingCanceler struct {
	svc   *Service
	calls int
}

func (c *resolvingCanceler) Cancel(ctx context.Context, tenantID, _ string) error {
	c.calls++
	_ = c.svc.ResolveByInvoice(ctx, tenantID, "inv_1", domain.ResolutionPaymentRecovered)
	return nil
}

// TestExhaustRun_ResolvedDuringTerminalAction_NoEscalatedClobber locks the guarded
// escalation write: if a settle resolves the run DURING the (successful) terminal
// action, the escalation write must no-op — the run stays resolved and NO
// contradictory dunning.escalated fires on top of the settle's dunning.resolved.
func TestExhaustRun_ResolvedDuringTerminalAction_NoEscalatedClobber(t *testing.T) {
	store := newMemStore()
	disp := &captureDispatcher{}
	p := store.policies[store.defaultID]
	p.FinalAction = domain.DunningActionCancelSubscription
	store.policies[store.defaultID] = p

	svc := NewService(store, &noopRetrier{}, nil)
	svc.SetEventDispatcher(disp)
	svc.SetSubscriptionPauser(&recordingPauser{}, stubInvGet{inv: unpaidInv()})
	canceler := &resolvingCanceler{svc: svc}
	svc.SetSubscriptionCanceler(canceler)

	run := dueRunAt(t, store, svc, 3) // == max → top-of-processRun exhaust branch
	svc.ProcessDueRuns(context.Background(), "t1", 20)

	if canceler.calls != 1 {
		t.Fatalf("terminal action should have fired exactly once: Cancel calls=%d", canceler.calls)
	}
	got := store.runs[run.ID]
	if got.State != domain.DunningResolved {
		t.Errorf("a settle during the terminal action must leave the run RESOLVED, not clobber it to escalated: got %q", got.State)
	}
	if n := disp.countOf(domain.EventDunningEscalated); n != 0 {
		t.Errorf("dunning.escalated must NOT fire when the run resolved during the terminal action: got %d", n)
	}
	if n := disp.countOf(domain.EventDunningResolved); n != 1 {
		t.Errorf("dunning.resolved should fire exactly once (from the settle): got %d", n)
	}
}
