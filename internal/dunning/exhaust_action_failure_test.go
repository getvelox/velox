package dunning

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// markUncollectiblePolicy mutates the mem store's default policy to a
// mark_uncollectible terminal so exhaustRun invokes a real mover.
func markUncollectiblePolicy(store *memStore) {
	p := store.policies[store.defaultID]
	p.FinalAction = domain.DunningActionMarkUncollectible
	store.policies[store.defaultID] = p
}

func exhaustedRun(t *testing.T, store *memStore, svc *Service) domain.InvoiceDunningRun {
	t.Helper()
	run, err := svc.StartDunning(context.Background(), "t1", "inv_1", "cus_1", time.Now(), domain.DunningCausePaymentFailed)
	if err != nil {
		t.Fatalf("start dunning: %v", err)
	}
	run.AttemptCount = 3 // == MaxRetryAttempts → next process hits exhaustRun
	past := time.Now().UTC().Add(-1 * time.Hour)
	run.NextActionAt = &past
	store.runs[run.ID] = run
	return run
}

// TestExhaustRun_ActionFailure_StaysRequeryable locks the fix: when the
// terminal final_action mover (here mark_uncollectible) FAILS, the run is
// left state=active with next_action_at set (NOT a clean escalated), so the
// due-run picker re-attempts it instead of recording "done" beside an
// invoice that never got closed.
func TestExhaustRun_ActionFailure_StaysRequeryable(t *testing.T) {
	store := newMemStore()
	markUncollectiblePolicy(store)
	svc := NewService(store, &noopRetrier{}, nil)
	svc.SetInvoiceUncollectibleMarker(&capturingUncollect{err: errors.New("stripe blip")})

	run := exhaustedRun(t, store, svc)
	svc.ProcessDueRuns(context.Background(), "t1", 20)

	got := store.runs[run.ID]
	if got.State != domain.DunningActive {
		t.Errorf("state: got %q, want active (terminal action failed → requeryable)", got.State)
	}
	if got.Resolution != domain.ResolutionActionFailed {
		t.Errorf("resolution: got %q, want action_failed", got.Resolution)
	}
	if got.ResolvedAt != nil {
		t.Error("resolved_at must be nil while the run is kept active for re-attempt")
	}
	if got.NextActionAt == nil {
		t.Error("next_action_at must be set so the due-run picker re-attempts the action")
	}
}

// TestExhaustRun_ActionSuccess_Escalates is the regression guard: a
// SUCCEEDING terminal action still lands the run escalated +
// retries_exhausted (the success path is unchanged).
func TestExhaustRun_ActionSuccess_Escalates(t *testing.T) {
	store := newMemStore()
	markUncollectiblePolicy(store)
	svc := NewService(store, &noopRetrier{}, nil)
	mover := &capturingUncollect{} // nil err → succeeds
	svc.SetInvoiceUncollectibleMarker(mover)

	run := exhaustedRun(t, store, svc)
	svc.ProcessDueRuns(context.Background(), "t1", 20)

	got := store.runs[run.ID]
	if got.State != domain.DunningEscalated {
		t.Errorf("state: got %q, want escalated (mover succeeded)", got.State)
	}
	if got.Resolution != domain.ResolutionRetriesExhausted {
		t.Errorf("resolution: got %q, want retries_exhausted", got.Resolution)
	}
	if got.ResolvedAt == nil {
		t.Error("resolved_at should be set on a clean escalation")
	}
	if len(mover.calls) != 1 {
		t.Errorf("MarkUncollectible calls: got %d, want 1", len(mover.calls))
	}
}

// TestExhaustRun_ActionFailure_ReattemptsAndRecovers proves the run is
// genuinely requeryable: a failed action keeps the run active, and once the
// mover recovers a later due tick re-invokes it and escalates.
func TestExhaustRun_ActionFailure_ReattemptsAndRecovers(t *testing.T) {
	store := newMemStore()
	markUncollectiblePolicy(store)
	svc := NewService(store, &noopRetrier{}, nil)
	mover := &capturingUncollect{err: errors.New("blip")}
	svc.SetInvoiceUncollectibleMarker(mover)
	ctx := context.Background()

	run := exhaustedRun(t, store, svc)
	svc.ProcessDueRuns(ctx, "t1", 20) // fails → active
	if store.runs[run.ID].State != domain.DunningActive {
		t.Fatalf("after failure, state = %q, want active", store.runs[run.ID].State)
	}

	// Mover recovers; backdate next_action_at so the run is due again.
	mover.err = nil
	r := store.runs[run.ID]
	past := time.Now().UTC().Add(-1 * time.Hour)
	r.NextActionAt = &past
	store.runs[run.ID] = r

	svc.ProcessDueRuns(ctx, "t1", 20) // succeeds → escalated
	if store.runs[run.ID].State != domain.DunningEscalated {
		t.Errorf("after recovery, state = %q, want escalated", store.runs[run.ID].State)
	}
	if len(mover.calls) != 2 {
		t.Errorf("MarkUncollectible calls: got %d, want 2 (failed then succeeded)", len(mover.calls))
	}
}
