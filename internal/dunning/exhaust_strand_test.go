package dunning

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// SB-1 / SB-2 (2026-10-08): an exhaustion interrupted after the final retry
// used to strand the run (active, next_action_at NULL — invisible to both
// due-run pickers forever), and an escalated run could be re-escalated or
// reopened by a second processor. These tests pin the lease that replaced
// the NULL, the exhaust-on-entry claim, and the state='active' write guard.

// escalateFailsOnce fails the first write that would escalate a run — a DB
// error landing after the final retry committed, the SB-1 cut point.
type escalateFailsOnce struct {
	*memStore
	failed bool
}

func (s *escalateFailsOnce) UpdateRunIfActive(ctx context.Context, tenantID string, run domain.InvoiceDunningRun, expected int, then func(*sql.Tx) error) (bool, error) {
	if run.State == domain.DunningEscalated && !s.failed {
		s.failed = true
		return false, errors.New("connection reset during the escalation write")
	}
	return s.memStore.UpdateRunIfActive(ctx, tenantID, run, expected, then)
}

// finalAttemptRun puts a run one retry short of exhaustion, due now.
func finalAttemptRun(t *testing.T, store *memStore, svc *Service) domain.InvoiceDunningRun {
	t.Helper()
	run, err := svc.StartDunning(context.Background(), "t1", "inv_1", "cus_1", time.Now(), domain.DunningCausePaymentFailed)
	if err != nil {
		t.Fatalf("start dunning: %v", err)
	}
	run.AttemptCount = store.policies[store.defaultID].MaxRetryAttempts - 1
	due := time.Now().UTC().Add(-time.Minute)
	run.NextActionAt = &due
	store.runs[run.ID] = run
	return run
}

// expireLease moves a run's lease into the past (time passing), keeping
// every other field — exactly what the next tick after the lease would list.
func expireLease(store *memStore, id string) {
	r := store.runs[id]
	past := time.Now().UTC().Add(-time.Second).Truncate(time.Microsecond)
	r.NextActionAt = &past
	store.runs[id] = r
}

// TestExhaust_InterruptedAfterFinalAttempt_IsRedriven is SB-1 itself: the
// final retry fails, the terminal action runs, and the escalation write
// errors. The run must stay re-drivable (active, next_action_at = the
// lease, hidden until it expires) and the next tick after the lease must
// escalate it once, stamped at the final retry's instant.
//
// Mutations that turn it red: writing next_action_at = now at the final
// reschedule (the run is due inside the exhaust window); stamping the
// re-drive with the processing time instead of LastAttemptAt.
func TestExhaust_InterruptedAfterFinalAttempt_IsRedriven(t *testing.T) {
	mem := newMemStore()
	markUncollectiblePolicy(mem)
	store := &escalateFailsOnce{memStore: mem}
	svc := NewService(store, &failingRetrier{}, nil)
	mover := &capturingUncollect{}
	svc.SetInvoiceUncollectibleMarker(mover)
	ctx := context.Background()

	run := finalAttemptRun(t, mem, svc)
	if _, errs := svc.ProcessDueRuns(ctx, "t1", 20); len(errs) != 1 {
		t.Fatalf("the escalation write's error must surface: errs=%v", errs)
	}

	got := mem.runs[run.ID]
	if got.State != domain.DunningActive || got.NextActionAt == nil {
		t.Fatalf("interrupted exhaustion stranded the run: state=%s next=%v (want active with a lease)", got.State, got.NextActionAt)
	}
	if got.LastAttemptAt == nil {
		t.Fatal("final attempt not recorded")
	}
	finalRetry := *got.LastAttemptAt
	if want := leaseFrom(finalRetry); !got.NextActionAt.Equal(want) {
		t.Fatalf("next_action_at = %v, want the exhaustion lease %v", got.NextActionAt, want)
	}
	if due, _ := mem.ListDueRuns(ctx, "t1", time.Now().UTC(), 20); len(due) != 0 {
		t.Fatal("the run is due inside its exhaust lease — a second processor could exhaust it concurrently (SB-2)")
	}

	expireLease(mem, run.ID)
	if _, errs := svc.ProcessDueRuns(ctx, "t1", 20); len(errs) != 0 {
		t.Fatalf("re-drive: %v", errs)
	}
	got = mem.runs[run.ID]
	if got.State != domain.DunningEscalated || got.Resolution != domain.ResolutionRetriesExhausted {
		t.Fatalf("re-drive did not finish the exhaustion: state=%s resolution=%s", got.State, got.Resolution)
	}
	if got.ResolvedAt == nil || !got.ResolvedAt.Equal(finalRetry) {
		t.Fatalf("resolved_at = %v, want the final retry's instant %v (class J: never the re-drive's processing time)", got.ResolvedAt, finalRetry)
	}
	escalated := 0
	for _, e := range mem.events {
		if e.EventType == domain.DunningEventEscalated {
			escalated++
			if !e.CreatedAt.Equal(finalRetry) {
				t.Errorf("escalated event at %v, want the final retry's instant %v", e.CreatedAt, finalRetry)
			}
		}
	}
	if escalated != 1 {
		t.Fatalf("escalated events = %d, want exactly 1", escalated)
	}
	if len(mover.calls) != 2 {
		t.Fatalf("terminal action calls = %d, want 2 (inline, then the re-drive)", len(mover.calls))
	}
	if due, _ := mem.ListDueRuns(ctx, "t1", time.Now().UTC().Add(time.Hour), 20); len(due) != 0 {
		t.Fatal("an escalated run is still due")
	}
}

// TestExhaustOnEntry_StaleProcessorFiresNothing is SB-2: a second processor
// holding the same listed snapshot reaches exhaust-on-entry after the first
// already claimed and escalated the run. Its claim must lose, so it fires no
// terminal action, no second escalation, and never reopens the run.
//
// Mutations that turn it red: skipping the ClaimExhaustion call (the stale
// processor's mover fires); reverting UpdateRunIfActive's guard to
// state <> 'resolved' (with the claim also skipped, the escalated run is
// rewritten).
func TestExhaustOnEntry_StaleProcessorFiresNothing(t *testing.T) {
	mem := newMemStore()
	markUncollectiblePolicy(mem)
	moverA, moverB := &capturingUncollect{}, &capturingUncollect{}
	a := NewService(mem, &noopRetrier{}, nil)
	a.SetInvoiceUncollectibleMarker(moverA)
	b := NewService(mem, &noopRetrier{}, nil)
	b.SetInvoiceUncollectibleMarker(moverB)
	ctx := context.Background()

	run := exhaustedRun(t, mem, a) // attempts == max, due: exhaust-on-entry
	snapshot := mem.runs[run.ID]   // what both processors listed

	if err := a.processRun(ctx, "t1", snapshot, false); err != nil {
		t.Fatalf("processor A: %v", err)
	}
	if err := b.processRun(ctx, "t1", snapshot, false); err != nil {
		t.Fatalf("processor B (stale): %v", err)
	}

	if len(moverA.calls) != 1 || len(moverB.calls) != 0 {
		t.Fatalf("terminal action calls A=%d B=%d, want 1 and 0 — the stale processor must lose the claim", len(moverA.calls), len(moverB.calls))
	}
	got := mem.runs[run.ID]
	if got.State != domain.DunningEscalated {
		t.Fatalf("state = %s, want escalated", got.State)
	}
	escalated := 0
	for _, e := range mem.events {
		if e.EventType == domain.DunningEventEscalated {
			escalated++
		}
	}
	if escalated != 1 {
		t.Fatalf("escalated events = %d, want 1", escalated)
	}
}

// TestUpdateRunIfActive_EscalatedIsTerminal: a stale snapshot of an escalated
// run (the action_failed loser of SB-2) can no longer flip it back to active.
// Mutation: memStore/real guard back to `state != resolved`.
func TestUpdateRunIfActive_EscalatedIsTerminal(t *testing.T) {
	mem := newMemStore()
	ctx := context.Background()
	svc := NewService(mem, &noopRetrier{}, nil)
	run := exhaustedRun(t, mem, svc)
	escalated := mem.runs[run.ID]
	escalated.State = domain.DunningEscalated
	escalated.Resolution = domain.ResolutionRetriesExhausted
	escalated.NextActionAt = nil
	mem.runs[run.ID] = escalated

	stale := run // still active in the loser's memory
	retry := time.Now().Add(24 * time.Hour)
	stale.NextActionAt = &retry
	stale.Resolution = domain.ResolutionActionFailed
	applied, err := mem.UpdateRunIfActive(ctx, "t1", stale, stale.AttemptCount, nil)
	if err != nil || applied {
		t.Fatalf("a stale write applied to an escalated run: applied=%v err=%v", applied, err)
	}
	if mem.runs[run.ID].State != domain.DunningEscalated {
		t.Fatalf("escalated run reopened to %s", mem.runs[run.ID].State)
	}
}

// TestUpdateRunIfActive_RefusesStrandingWrite mechanizes SB-1's invariant:
// an active run is never written without next_action_at.
// Mutation: delete refuseStrandingWrite's check.
func TestUpdateRunIfActive_RefusesStrandingWrite(t *testing.T) {
	mem := newMemStore()
	svc := NewService(mem, &noopRetrier{}, nil)
	run := exhaustedRun(t, mem, svc)
	stranded := run
	stranded.NextActionAt = nil
	if _, err := mem.UpdateRunIfActive(context.Background(), "t1", stranded, run.AttemptCount, nil); err == nil {
		t.Fatal("an active run was written with next_action_at NULL")
	}
	if mem.runs[run.ID].NextActionAt == nil {
		t.Fatal("the refused write still changed the row")
	}
}

// skipOnFinal returns ErrTransientSkip (the charge never reached Stripe).
type skipOnFinal struct{}

func (skipOnFinal) RetryPayment(context.Context, string, string, string) error {
	return ErrTransientSkip
}

// TestFinalAttempt_TransientSkip_RestoresNextActionAt: a final attempt whose
// charge never happened is rewound completely — count, last attempt, AND the
// lease the pre-charge persist wrote — so it is retried on the next tick,
// not after the lease. Mutation: drop the next_action_at restore.
func TestFinalAttempt_TransientSkip_RestoresNextActionAt(t *testing.T) {
	mem := newMemStore()
	svc := NewService(mem, skipOnFinal{}, nil)
	run := finalAttemptRun(t, mem, svc)
	before := mem.runs[run.ID]

	if _, errs := svc.ProcessDueRuns(context.Background(), "t1", 20); len(errs) != 0 {
		t.Fatalf("process: %v", errs)
	}
	got := mem.runs[run.ID]
	if got.AttemptCount != before.AttemptCount {
		t.Fatalf("attempt_count = %d, want the rewound %d", got.AttemptCount, before.AttemptCount)
	}
	if got.NextActionAt == nil || !got.NextActionAt.Equal(*before.NextActionAt) {
		t.Fatalf("next_action_at = %v, want restored %v (not the exhaust lease)", got.NextActionAt, before.NextActionAt)
	}
}

// nestedProcessor re-enters the dunning tick from inside the final charge,
// standing in for a second processor that lists due runs at that moment.
type nestedProcessor struct {
	svc    *Service
	listed int
}

func (n *nestedProcessor) RetryPayment(ctx context.Context, tenantID, _, _ string) error {
	due, _ := n.svc.store.ListDueRuns(ctx, tenantID, time.Now().UTC().Add(time.Minute), 20)
	n.listed = len(due)
	return errors.New("card_declined")
}

// TestFinalAttempt_LeaseHidesRunDuringCharge: the final attempt records its
// exhaustion lease BEFORE the charge, so a processor listing due runs while
// the charge is in flight sees nothing to exhaust. Mutation: drop the
// pre-charge lease (the run's old due next_action_at is still listed).
func TestFinalAttempt_LeaseHidesRunDuringCharge(t *testing.T) {
	mem := newMemStore()
	retrier := &nestedProcessor{}
	svc := NewService(mem, retrier, nil)
	retrier.svc = svc
	finalAttemptRun(t, mem, svc)

	svc.ProcessDueRuns(context.Background(), "t1", 20)
	if retrier.listed != 0 {
		t.Fatalf("%d runs were due while the final charge was in flight — a second processor could exhaust them under the charge", retrier.listed)
	}
}

// escalateAlwaysFails models an escalation write that can never land.
type escalateAlwaysFails struct{ *memStore }

func (s escalateAlwaysFails) UpdateRunIfActive(ctx context.Context, tenantID string, run domain.InvoiceDunningRun, expected int, then func(*sql.Tx) error) (bool, error) {
	if run.State == domain.DunningEscalated {
		return false, errors.New("escalation write keeps failing")
	}
	return s.memStore.UpdateRunIfActive(ctx, tenantID, run, expected, then)
}

// TestProcessDueRunsForClock_PersistentExhaustFailure_Terminates: when the
// escalation can never land, each re-drive only moves the lease 15 simulated
// minutes, so within one advance the run is due again and again. The catchup
// loop must still stop: the first re-drive counts as progress (count moved
// Max-1 -> Max between listing and processing), the next sees no progress
// and exits. The run is left re-drivable, never NULL.
// Mutation: the no-progress guard `>` -> `>=` (the loop runs to its cap).
func TestProcessDueRunsForClock_PersistentExhaustFailure_Terminates(t *testing.T) {
	mem := newMemStore()
	markUncollectiblePolicy(mem)
	svc := NewService(escalateAlwaysFails{mem}, &failingRetrier{}, nil)
	mover := &capturingUncollect{}
	svc.SetInvoiceUncollectibleMarker(mover)
	run := finalAttemptRun(t, mem, svc)

	frozen := time.Now().UTC().Add(48 * time.Hour)
	svc.ProcessDueRunsForClock(context.Background(), "t1", "clk", frozen, 20)

	got := mem.runs[run.ID]
	if got.State != domain.DunningActive || got.NextActionAt == nil {
		t.Fatalf("persistent failure left state=%s next=%v, want active and re-drivable", got.State, got.NextActionAt)
	}
	if n := len(mover.calls); n != 2 {
		t.Fatalf("exhaustion attempted %d times in one advance, want 2 (inline, then one re-drive) — the no-progress guard did not stop the loop", n)
	}
}

func TestAnchorInstant(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	past, future := now.Add(-48*time.Hour), now.Add(time.Hour)
	for _, tc := range []struct {
		name      string
		scheduled *time.Time
		simulated bool
		want      time.Time
	}{
		{"simulated uses the scheduled instant even in the past", &past, true, past},
		{"simulated with nothing scheduled falls back to now", nil, true, now},
		{"wall clock clamps a stale past schedule to now", &past, false, now},
		{"wall clock keeps a future schedule", &future, false, future},
	} {
		if got := anchorInstant(now, tc.scheduled, tc.simulated); !got.Equal(tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}
