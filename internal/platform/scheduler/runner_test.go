package scheduler

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/platform/leader"
)

func waitFor(t *testing.T, cond func() bool, within time.Duration, msg string) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if !cond() {
		t.Fatal(msg)
	}
}

// TestRun_PanicRecoveryContinuesLoop pins the load-bearing claim of
// this helper: a panic in one tick must NOT kill the runner goroutine.
// Without recover(), the panic would unwind out of the for-select loop
// and the worker would silently die while the ticker channel kept
// buffering — which is exactly the bug the helper exists to prevent.
// With the lease gate the panic must also not leak the lease: the gate
// double's Lead runs the work synchronously, so a panic that escaped
// runOneTick would propagate out of Lead and kill this test's goroutine.
func TestRun_PanicRecoveryContinuesLoop(t *testing.T) {
	var ticks int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	work := func(_ context.Context) {
		n := atomic.AddInt32(&ticks, 1)
		if n == 1 {
			panic("boom")
		}
	}

	done := make(chan struct{})
	go func() {
		Run(ctx, leader.Role("test_worker"), 5*time.Millisecond, leader.AlwaysLead{}, work, nil)
		close(done)
	}()

	waitFor(t, func() bool { return atomic.LoadInt32(&ticks) >= 3 }, 500*time.Millisecond,
		"ticks < 3: a panic on tick 1 stopped the loop")

	cancel()
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Run did not return after ctx cancel")
	}
}

// TestRun_StopsOnContextCancel asserts the runner exits cleanly when
// the parent ctx is cancelled — no leaked goroutines or stuck loops
// when cmd/velox shuts down.
func TestRun_StopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Run(ctx, leader.Role("test_worker"), 10*time.Millisecond, leader.AlwaysLead{}, func(context.Context) {}, nil)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Run did not return after ctx cancel")
	}
}

// TestRun_FollowerNeverRunsWork is the multi-replica claim in one line: a
// replica whose gate never leads never executes the tick body, however
// many polls fire. Mutation check: drop the gate call in Run and run the
// work directly → this fails.
func TestRun_FollowerNeverRunsWork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var ran int32
	var polls int32
	go Run(ctx, leader.Role("test_worker"), 2*time.Millisecond, leader.NeverLead{}, func(context.Context) { atomic.AddInt32(&ran, 1) },
		func() { atomic.AddInt32(&polls, 1) })
	waitFor(t, func() bool { return atomic.LoadInt32(&polls) >= 5 }, 500*time.Millisecond, "onPoll never fired 5 times")
	if got := atomic.LoadInt32(&ran); got != 0 {
		t.Fatalf("follower ran the work %d times; want 0", got)
	}
}

// TestRun_OnPollFiresOnEveryPollIncludingFollowers: the liveness stamp
// (billing's onRun → /health/ready) must keep firing on a follower, or a
// healthy follower reads as a stalled scheduler. Covered by the previous
// test's polls counter; this one pins the leader side too.
func TestRun_OnPollFiresOnEveryPollIncludingFollowers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var polls, ran int32
	go Run(ctx, leader.Role("test_worker"), 2*time.Millisecond, leader.AlwaysLead{}, func(context.Context) { atomic.AddInt32(&ran, 1) },
		func() { atomic.AddInt32(&polls, 1) })
	waitFor(t, func() bool { return atomic.LoadInt32(&ran) >= 3 }, 500*time.Millisecond, "leader never ran 3 ticks")
	if p, r := atomic.LoadInt32(&polls), atomic.LoadInt32(&ran); p < r {
		t.Fatalf("polls=%d < ticks led=%d: onPoll must fire on every poll", p, r)
	}
}

// TestRun_GateErrorSkipsTickAndContinues: an infrastructure error from the
// gate (DB down) skips this poll and the loop lives to poll again — it
// must neither run the work ungated nor die.
func TestRun_GateErrorSkipsTickAndContinues(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var ran, polls int32
	gate := leader.ErrGate{Err: errors.New("db down")}
	go Run(ctx, leader.Role("test_worker"), 2*time.Millisecond, gate, func(context.Context) { atomic.AddInt32(&ran, 1) },
		func() { atomic.AddInt32(&polls, 1) })
	waitFor(t, func() bool { return atomic.LoadInt32(&polls) >= 5 }, 500*time.Millisecond, "loop died after a gate error")
	if got := atomic.LoadInt32(&ran); got != 0 {
		t.Fatalf("work ran %d times on gate error; want 0 (never run ungated)", got)
	}
}

// TestRun_NilGateRefusesToLoop: an ungated singleton loop at N>=2 is a
// double-billing machine, so Run refuses rather than defaulting to
// "always lead". Mutation check: make nil mean AlwaysLead → this fails.
func TestRun_NilGateRefusesToLoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var ran int32
	done := make(chan struct{})
	go func() {
		Run(ctx, leader.Role("test_worker"), time.Millisecond, nil, func(context.Context) { atomic.AddInt32(&ran, 1) }, nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Run with a nil gate kept looping")
	}
	if got := atomic.LoadInt32(&ran); got != 0 {
		t.Fatalf("work ran %d times with a nil gate", got)
	}
}

// TestRun_PollsNoSlowerThanMaxPoll: a 1h role must still poll every
// leader.MaxPoll so a dead leader's role migrates within
// LeaseTTL + MaxPoll, not within an hour.
func TestRun_PollsNoSlowerThanMaxPoll(t *testing.T) {
	if leader.MaxPoll > 10*time.Second {
		t.Fatalf("MaxPoll=%s: failover bound is LeaseTTL+MaxPoll; keep it small", leader.MaxPoll)
	}
	// Structural pin — the clamp lives in Run: poll = min(interval, MaxPoll).
	// Exercised with a real gate in leader's integration tests; here we
	// pin the constant so a future bump is a conscious change.
}
