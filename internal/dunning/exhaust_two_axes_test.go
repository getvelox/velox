package dunning

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// stubSubState is the skip-if-done read. `terminated` flips to true once
// the fake canceler runs, so a re-attempt observes the same thing the real
// store would.
type stubSubState struct {
	terminated bool
	err        error
	calls      int
}

func (s *stubSubState) GetTerminalState(_ context.Context, _, _ string) (SubscriptionTerminalState, error) {
	s.calls++
	if s.err != nil {
		return SubscriptionTerminalState{}, s.err
	}
	return SubscriptionTerminalState{Terminated: s.terminated}, nil
}

// flakyCanceler fails until `failures` attempts have been made, then
// succeeds and marks the shared state terminal.
type flakyCanceler struct {
	failures int
	calls    int
	state    *stubSubState
}

func (c *flakyCanceler) Cancel(_ context.Context, _, _ string) error {
	c.calls++
	if c.calls <= c.failures {
		return errors.New("stripe blip")
	}
	if c.state != nil {
		c.state.terminated = true
	}
	return nil
}

type capturingEscalationEmail struct {
	outcomes []domain.DunningEscalationOutcome
}

func (e *capturingEscalationEmail) SendPaymentFailed(context.Context, string, string, []string, string, string, string, string) error {
	return nil
}

func (e *capturingEscalationEmail) SendDunningWarning(context.Context, string, string, []string, string, string, int, int, string, string, string) error {
	return nil
}

func (e *capturingEscalationEmail) SendDunningEscalation(_ context.Context, _, _ string, _ []string, _, _ string, outcome domain.DunningEscalationOutcome, _ string) error {
	e.outcomes = append(e.outcomes, outcome)
	return nil
}

func twoAxisPolicy(store *memStore, sub domain.DunningSubscriptionAction, inv domain.DunningInvoiceAction) {
	p := store.policies[store.defaultID]
	p.FinalSubscriptionAction = sub
	p.FinalInvoiceAction = inv
	store.policies[store.defaultID] = p
}

func makeDue(store *memStore, runID string) {
	r := store.runs[runID]
	past := time.Now().UTC().Add(-time.Hour)
	r.NextActionAt = &past
	store.runs[runID] = r
}

// TestExhaust_CancelAndWriteOff_BothApply is the whole point of ADR-112:
// Stripe's own default outcome — cancel the subscription AND write off the
// invoice — was inexpressible under the single `final_action` enum, which
// forced a choice between them. This is the discriminating fixture: any
// implementation that still collapses to one action fails it.
func TestExhaust_CancelAndWriteOff_BothApply(t *testing.T) {
	store := newMemStore()
	twoAxisPolicy(store, domain.SubActionCancel, domain.InvActionMarkUncollectible)
	svc := NewService(store, &noopRetrier{}, nil)

	state := &stubSubState{}
	canceler := &flakyCanceler{state: state}
	uncollect := &capturingUncollect{}
	svc.SetSubscriptionPauser(&recordingPauser{}, stubInvGet{inv: unpaidInv()})
	svc.SetSubscriptionCanceler(canceler)
	svc.SetSubscriptionStateReader(state)
	svc.SetInvoiceUncollectibleMarker(uncollect)

	run := exhaustedRun(t, store, svc)
	svc.ProcessDueRuns(context.Background(), "t1", 20)

	if canceler.calls != 1 {
		t.Errorf("Cancel calls: got %d, want 1", canceler.calls)
	}
	if len(uncollect.calls) != 1 {
		t.Errorf("MarkUncollectible calls: got %d, want 1 — the write-off must not be sacrificed to the cancel", len(uncollect.calls))
	}
	if got := store.runs[run.ID]; got.State != domain.DunningEscalated {
		t.Errorf("state: got %q, want escalated", got.State)
	}
}

// TestExhaust_PartialFailure_ReattemptSkipsTheDoneHalf is the convergence
// guarantee. Both subscription movers refuse an already-terminal sub
// (cancelSpec allows only draft/trialing/active), so without the
// skip-if-done read a re-attempt after a partial failure would fail forever
// on the half that already succeeded and never reach the half that did not.
//
// Sequenced so the SUBSCRIPTION half fails first: the invoice half is gated
// on it precisely so the invoice is not written off while the subscription
// action is outstanding — a written-off invoice makes exhaustRun's
// late-paid re-check resolve the run, stranding the cancel permanently.
func TestExhaust_PartialFailure_ReattemptSkipsTheDoneHalf(t *testing.T) {
	store := newMemStore()
	twoAxisPolicy(store, domain.SubActionCancel, domain.InvActionMarkUncollectible)
	svc := NewService(store, &noopRetrier{}, nil)

	state := &stubSubState{}
	canceler := &flakyCanceler{failures: 1, state: state}
	uncollect := &capturingUncollect{}
	svc.SetSubscriptionPauser(&recordingPauser{}, stubInvGet{inv: unpaidInv()})
	svc.SetSubscriptionCanceler(canceler)
	svc.SetSubscriptionStateReader(state)
	svc.SetInvoiceUncollectibleMarker(uncollect)
	ctx := context.Background()

	run := exhaustedRun(t, store, svc)

	// Tick 1: cancel fails → run stays requeryable, and the invoice must NOT
	// have been written off.
	svc.ProcessDueRuns(ctx, "t1", 20)
	if got := store.runs[run.ID]; got.State != domain.DunningActive {
		t.Fatalf("after subscription-action failure, state = %q, want active", got.State)
	}
	if len(uncollect.calls) != 0 {
		t.Fatalf("invoice was written off while the subscription action was still outstanding (%d calls) — the re-attempt would then short-circuit on the late-paid re-check and never retry the cancel", len(uncollect.calls))
	}

	// Tick 2: cancel succeeds, write-off follows.
	makeDue(store, run.ID)
	svc.ProcessDueRuns(ctx, "t1", 20)
	if got := store.runs[run.ID]; got.State != domain.DunningEscalated {
		t.Fatalf("after recovery, state = %q, want escalated", got.State)
	}
	if canceler.calls != 2 {
		t.Errorf("Cancel calls: got %d, want 2 (failed then succeeded)", canceler.calls)
	}
	if len(uncollect.calls) != 1 {
		t.Errorf("MarkUncollectible calls: got %d, want 1", len(uncollect.calls))
	}
}

// TestExhaust_AlreadyTerminalSubscription_DoesNotFail is the same guarantee
// from the other side, and it is what stops an infinite 24h retry loop: a
// subscription canceled by an operator (or by an earlier tick) must make
// the cancel action a satisfied no-op, not a permanent failure.
func TestExhaust_AlreadyTerminalSubscription_DoesNotFail(t *testing.T) {
	store := newMemStore()
	twoAxisPolicy(store, domain.SubActionCancel, domain.InvActionNone)
	svc := NewService(store, &noopRetrier{}, nil)

	state := &stubSubState{terminated: true}
	canceler := &flakyCanceler{state: state}
	svc.SetSubscriptionPauser(&recordingPauser{}, stubInvGet{inv: unpaidInv()})
	svc.SetSubscriptionCanceler(canceler)
	svc.SetSubscriptionStateReader(state)

	run := exhaustedRun(t, store, svc)
	svc.ProcessDueRuns(context.Background(), "t1", 20)

	if canceler.calls != 0 {
		t.Errorf("Cancel calls: got %d, want 0 — an already-canceled subscription needs no mover call", canceler.calls)
	}
	if got := store.runs[run.ID]; got.State != domain.DunningEscalated {
		t.Errorf("state: got %q, want escalated — an already-satisfied action is not a failure", got.State)
	}
}

// TestExhaust_OneOffInvoice_ClaimsNothing: an invoice with no subscription
// cannot have one canceled, and the escalation email must not say it did.
// The debtor-facing lie is the failure this guards.
func TestExhaust_OneOffInvoice_ClaimsNothing(t *testing.T) {
	store := newMemStore()
	twoAxisPolicy(store, domain.SubActionCancel, domain.InvActionNone)
	svc := NewService(store, &noopRetrier{}, nil)

	oneOff := unpaidInv()
	oneOff.SubscriptionID = "" // one-off — no subscription behind it
	state := &stubSubState{}
	canceler := &flakyCanceler{state: state}
	emails := &capturingEscalationEmail{}
	svc.SetSubscriptionPauser(&recordingPauser{}, stubInvGet{inv: oneOff})
	svc.SetSubscriptionCanceler(canceler)
	svc.SetSubscriptionStateReader(state)
	svc.SetEmailNotifier(emails)
	svc.SetCustomerEmailFetcher(stubCustomerEmail{})

	exhaustedRun(t, store, svc)
	svc.ProcessDueRuns(context.Background(), "t1", 20)

	if canceler.calls != 0 {
		t.Errorf("Cancel calls: got %d, want 0 (nothing to cancel)", canceler.calls)
	}
	if len(emails.outcomes) != 1 {
		t.Fatalf("escalation emails: got %d, want 1", len(emails.outcomes))
	}
	if emails.outcomes[0].Any() {
		t.Errorf("escalation claimed %q on a one-off invoice — the debtor must never read that a subscription was canceled when none existed", emails.outcomes[0].Reason())
	}
}

// TestExhaust_EmailRendersOutcomeNotPolicy pins the rule that the email
// reads the OUTCOME. Here the policy asks for a write-off and gets one, so
// the outcome carries it — and carries ONLY it, since the subscription half
// is `none`.
func TestExhaust_EmailRendersOutcomeNotPolicy(t *testing.T) {
	store := newMemStore()
	twoAxisPolicy(store, domain.SubActionNone, domain.InvActionMarkUncollectible)
	svc := NewService(store, &noopRetrier{}, nil)

	emails := &capturingEscalationEmail{}
	svc.SetSubscriptionPauser(&recordingPauser{}, stubInvGet{inv: unpaidInv()})
	svc.SetInvoiceUncollectibleMarker(&capturingUncollect{})
	svc.SetEmailNotifier(emails)
	svc.SetCustomerEmailFetcher(stubCustomerEmail{})

	exhaustedRun(t, store, svc)
	svc.ProcessDueRuns(context.Background(), "t1", 20)

	if len(emails.outcomes) != 1 {
		t.Fatalf("escalation emails: got %d, want 1", len(emails.outcomes))
	}
	got := emails.outcomes[0]
	if !got.InvoiceWrittenOff {
		t.Error("outcome should record the write-off")
	}
	if got.SubscriptionCanceled || got.SubscriptionPaused {
		t.Errorf("outcome claims a subscription change under a none policy: %q", got.Reason())
	}
	if got.Reason() != "invoice_marked_uncollectible" {
		t.Errorf("Reason(): got %q, want invoice_marked_uncollectible", got.Reason())
	}
}
