package dunning

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
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

// manualReviewPolicy: retries exhaust into escalation with no terminal
// mover, so the escalation write is the only thing exhaustion does.
func manualReviewPolicy(store *memStore) {
	p := store.policies[store.defaultID]
	p.FinalSubscriptionAction = domain.SubActionNone
	p.FinalInvoiceAction = domain.InvActionNone
	store.policies[store.defaultID] = p
}

// TestExhaust_InterruptedAfterFinalAttempt_IsRedriven is SB-1 itself, wired
// the way production is (an invoice reader serving the still-unpaid
// invoice): the final retry fails and the escalation write errors. The run
// must stay re-drivable (active, next_action_at = the lease, hidden until it
// expires) and the next tick after the lease must escalate it once, stamped
// at the final retry's instant.
//
// Mutations that turn it red: writing next_action_at = now at the final
// reschedule (the run is due inside the exhaust window); stamping the
// re-drive with the processing time instead of the final retry.
func TestExhaust_InterruptedAfterFinalAttempt_IsRedriven(t *testing.T) {
	mem := newMemStore()
	manualReviewPolicy(mem)
	store := &escalateFailsOnce{memStore: mem}
	svc := NewService(store, &failingRetrier{}, nil)
	svc.SetSubscriptionPauser(&recordingPauser{}, stubInvGet{inv: unpaidInv()})
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

	// Time passes beyond the lease; the row is untouched (its lease value is
	// the proof the stamp rule reads).
	later := clock.WithEffectiveNow(ctx, got.NextActionAt.Add(time.Second))
	if _, errs := svc.ProcessDueRuns(later, "t1", 20); len(errs) != 0 {
		t.Fatalf("re-drive: %v", errs)
	}
	got = mem.runs[run.ID]
	if got.State != domain.DunningEscalated || got.Resolution != domain.ResolutionRetriesExhausted {
		t.Fatalf("re-drive did not finish the exhaustion: state=%s resolution=%s", got.State, got.Resolution)
	}
	if got.ResolvedAt == nil || !got.ResolvedAt.Equal(finalRetry) {
		t.Fatalf("resolved_at = %v, want the final retry's instant %v (class J: never the re-drive's processing time)", got.ResolvedAt, finalRetry)
	}
	if n := countEvents(mem, domain.DunningEventEscalated); n != 1 {
		t.Fatalf("escalated events = %d, want exactly 1", n)
	}
	for _, e := range mem.events {
		if e.EventType == domain.DunningEventEscalated && !e.CreatedAt.Equal(finalRetry) {
			t.Errorf("escalated event at %v, want the final retry's instant %v", e.CreatedAt, finalRetry)
		}
	}
	if due, _ := mem.ListDueRuns(ctx, "t1", got.ResolvedAt.Add(time.Hour), 20); len(due) != 0 {
		t.Fatal("an escalated run is still due")
	}
}

func countEvents(mem *memStore, typ domain.DunningEventType) int {
	n := 0
	for _, e := range mem.events {
		if e.EventType == typ {
			n++
		}
	}
	return n
}

// writeOffMover marks the shared invoice uncollectible, as the real mover
// does, so the invoice reader sees the write-off on the next read.
type writeOffMover struct {
	inv   *domain.Invoice
	calls int
}

func (m *writeOffMover) MarkUncollectible(context.Context, string, string) error {
	m.calls++
	m.inv.Status = domain.InvoiceUncollectible
	return nil
}

type ptrInvGet struct{ inv *domain.Invoice }

func (g ptrInvGet) Get(context.Context, string, string) (domain.Invoice, error) { return *g.inv, nil }

// TestExhaust_InterruptedAfterWriteOff_ResolvesNotCollectible pins the
// deferred residue honestly (ADR-112 amendment 2026-10-08, deferral 1): when
// the write-off committed and only the escalation write failed, the re-drive
// sees an uncollectible invoice and closes the run as invoice_not_collectible
// — no escalation, no second write-off. The run is finished, not stranded;
// what is lost is the final notice.
func TestExhaust_InterruptedAfterWriteOff_ResolvesNotCollectible(t *testing.T) {
	mem := newMemStore()
	markUncollectiblePolicy(mem)
	store := &escalateFailsOnce{memStore: mem}
	svc := NewService(store, &failingRetrier{}, nil)
	inv := unpaidInv()
	svc.SetSubscriptionPauser(&recordingPauser{}, ptrInvGet{inv: &inv})
	mover := &writeOffMover{inv: &inv}
	svc.SetInvoiceUncollectibleMarker(mover)
	ctx := context.Background()

	run := finalAttemptRun(t, mem, svc)
	svc.ProcessDueRuns(ctx, "t1", 20)
	lease := *mem.runs[run.ID].NextActionAt
	if _, errs := svc.ProcessDueRuns(clock.WithEffectiveNow(ctx, lease.Add(time.Second)), "t1", 20); len(errs) != 0 {
		t.Fatalf("re-drive: %v", errs)
	}
	got := mem.runs[run.ID]
	if got.State != domain.DunningResolved || got.Resolution != domain.ResolutionInvoiceNotCollectible {
		t.Fatalf("state=%s resolution=%s, want resolved/invoice_not_collectible", got.State, got.Resolution)
	}
	if mover.calls != 1 || countEvents(mem, domain.DunningEventEscalated) != 0 {
		t.Fatalf("write-offs=%d escalations=%d, want 1 and 0", mover.calls, countEvents(mem, domain.DunningEventEscalated))
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
	if n := countEvents(mem, domain.DunningEventEscalated); n != 1 {
		t.Fatalf("escalated events = %d, want 1", n)
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

// reentrantMover re-enters a second processor's processRun while the first
// is inside its terminal action — the in-flight SB-2 race.
type reentrantMover struct {
	other    *Service
	snapshot domain.InvoiceDunningRun
	calls    int
	errOther error
}

func (m *reentrantMover) MarkUncollectible(ctx context.Context, tenantID, _ string) error {
	m.calls++
	if m.calls == 1 {
		m.errOther = m.other.processRun(ctx, tenantID, m.snapshot, false)
	}
	return nil
}

// TestExhaustOnEntry_ConcurrentClaimDuringTerminalAction: B arrives with
// the same listing while A holds the claim and is still inside its terminal
// action (state still active, count unchanged). Only next_action_at tells
// them apart, so B's claim must lose on it and fire nothing.
// Mutation: drop the next_action_at component of the claim (B wins too).
func TestExhaustOnEntry_ConcurrentClaimDuringTerminalAction(t *testing.T) {
	mem := newMemStore()
	markUncollectiblePolicy(mem)
	a := NewService(mem, &noopRetrier{}, nil)
	b := NewService(mem, &noopRetrier{}, nil)
	moverB := &capturingUncollect{}
	b.SetInvoiceUncollectibleMarker(moverB)
	run := exhaustedRun(t, mem, a)
	snapshot := mem.runs[run.ID]
	moverA := &reentrantMover{other: b, snapshot: snapshot}
	a.SetInvoiceUncollectibleMarker(moverA)

	if err := a.processRun(context.Background(), "t1", snapshot, false); err != nil {
		t.Fatalf("A: %v", err)
	}
	if moverA.errOther != nil {
		t.Fatalf("B: %v", moverA.errOther)
	}
	if len(moverB.calls) != 0 {
		t.Fatalf("B fired its terminal action %d times while A held the claim", len(moverB.calls))
	}
	if n := countEvents(mem, domain.DunningEventEscalated); n != 1 {
		t.Fatalf("escalated events = %d, want 1", n)
	}
}

// TestExhaustOnEntry_LoweredPolicy_StampsWhenItRan: a run at attempt 2 of 3
// whose policy is lowered to 2 exhausts on its next tick. Nothing was
// contracted at its last retry, so the escalation records the tick it ran,
// not the old retry. Mutation: exhaustStampAt always returning LastAttemptAt.
func TestExhaustOnEntry_LoweredPolicy_StampsWhenItRan(t *testing.T) {
	mem := newMemStore()
	manualReviewPolicy(mem)
	svc := NewService(mem, &noopRetrier{}, nil)
	run := finalAttemptRun(t, mem, svc) // attempt 2 of 3, normally scheduled
	r := mem.runs[run.ID]
	oldRetry := time.Now().UTC().Add(-72 * time.Hour)
	r.LastAttemptAt = &oldRetry
	mem.runs[run.ID] = r
	p := mem.policies[mem.defaultID]
	p.MaxRetryAttempts = r.AttemptCount // the operator lowers it
	mem.policies[mem.defaultID] = p

	tick := time.Now().UTC().Truncate(time.Microsecond)
	if _, errs := svc.ProcessDueRuns(clock.WithEffectiveNow(context.Background(), tick), "t1", 20); len(errs) != 0 {
		t.Fatalf("process: %v", errs)
	}
	got := mem.runs[run.ID]
	if got.State != domain.DunningEscalated || got.ResolvedAt == nil {
		t.Fatalf("state=%s, want escalated", got.State)
	}
	if !got.ResolvedAt.Equal(tick) {
		t.Fatalf("resolved_at = %v, want the tick that ran it %v (not the old retry %v)", got.ResolvedAt, tick, oldRetry)
	}
}

// TestExhaustOnEntry_ActionFailedAfterOutage_BacksOffFromNow: an interrupted
// exhaustion re-driven after a long outage whose terminal action then fails
// must schedule its re-attempt 24h from NOW, not from the old final retry
// (which would already be in the past and re-fire every tick).
// Mutation: base the +24h on stampAt instead of attemptAt.
func TestExhaustOnEntry_ActionFailedAfterOutage_BacksOffFromNow(t *testing.T) {
	mem := newMemStore()
	markUncollectiblePolicy(mem)
	svc := NewService(mem, &noopRetrier{}, nil)
	svc.SetInvoiceUncollectibleMarker(&capturingUncollect{err: errors.New("stripe down")})
	run := exhaustedRun(t, mem, svc)
	r := mem.runs[run.ID]
	finalRetry := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond)
	lease := leaseFrom(finalRetry)
	r.LastAttemptAt, r.NextActionAt = &finalRetry, &lease
	mem.runs[run.ID] = r

	tick := time.Now().UTC().Truncate(time.Microsecond)
	svc.ProcessDueRuns(clock.WithEffectiveNow(context.Background(), tick), "t1", 20)
	got := mem.runs[run.ID]
	if got.Resolution != domain.ResolutionActionFailed || got.NextActionAt == nil {
		t.Fatalf("resolution=%s next=%v, want action_failed with a re-attempt", got.Resolution, got.NextActionAt)
	}
	if want := tick.Add(24 * time.Hour); !got.NextActionAt.Equal(want) {
		t.Fatalf("re-attempt at %v, want %v (24h from the re-drive, not from the old retry)", got.NextActionAt, want)
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

// TestExhaust_StaleResolveCannotUnescalate is F1 from the money-path review:
// a stale processor listed the run while active; another escalated it and
// wrote the invoice off; the stale one resumes and its tick-start pre-check
// sees an uncollectible invoice. Its automated resolve must not flip the
// escalated run to resolved (a second terminal webhook, a contradictory
// timeline). Mutation: let the automated resolve admit escalated rows.
func TestExhaust_StaleResolveCannotUnescalate(t *testing.T) {
	mem := newMemStore()
	svc := NewService(mem, &noopRetrier{}, nil)
	run := exhaustedRun(t, mem, svc)
	stale := mem.runs[run.ID] // listed while active

	esc := stale
	esc.State = domain.DunningEscalated
	esc.Resolution = domain.ResolutionRetriesExhausted
	now := time.Now().UTC()
	esc.ResolvedAt, esc.NextActionAt = &now, nil
	mem.runs[run.ID] = esc

	inv := unpaidInv()
	inv.Status = domain.InvoiceUncollectible
	svc.SetSubscriptionPauser(&recordingPauser{}, stubInvGet{inv: inv})
	if err := svc.processRun(context.Background(), "t1", stale, false); err != nil {
		t.Fatalf("stale processor: %v", err)
	}
	if got := mem.runs[run.ID]; got.State != domain.DunningEscalated {
		t.Fatalf("a stale automated resolve rewrote an escalated run to %s/%s", got.State, got.Resolution)
	}
	if n := countEvents(mem, domain.DunningEventResolved); n != 0 {
		t.Fatalf("resolved events = %d, want 0", n)
	}
}
