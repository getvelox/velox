package billing

import (
	"time"

	"context"
	"errors"
	"github.com/sagarsuperuser/velox/internal/platform/leader"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// roleGate is a leader.Gate double that leads exactly the roles in lead and
// counts Lead calls per role — so a test can prove the scheduler polls BOTH
// roles independently and runs a body only when its own role is led.
type roleGate struct {
	lead  map[leader.Role]bool
	mu    sync.Mutex
	calls map[leader.Role]int
}

func newRoleGate(lead ...leader.Role) *roleGate {
	g := &roleGate{lead: map[leader.Role]bool{}, calls: map[leader.Role]int{}}
	for _, r := range lead {
		g.lead[r] = true
	}
	return g
}

func (g *roleGate) Lead(ctx context.Context, role leader.Role, _ time.Duration, work func(context.Context)) (bool, error) {
	g.mu.Lock()
	g.calls[role]++
	led := g.lead[role]
	g.mu.Unlock()
	if !led {
		return false, nil
	}
	work(ctx)
	return true, nil
}

func (g *roleGate) callsFor(role leader.Role) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls[role]
}

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

// ---- Minimal dependency stubs -------------------------------------------
//
// The scheduler runBillingHalf / runDunningHalf paths touch engine.RunCycle,
// engine.RetryPendingCharges, dunning.ProcessDueRuns, and tenants.ListTenantIDs.
// All are counted so the test asserts whether the leader-gated body actually
// ran for a given tick.

type countingDunning struct {
	calls atomic.Int32

	// modesMu guards modes; the scheduler fans out per livemode so every
	// tick should record both true and false at least once per tenant.
	modesMu sync.Mutex
	modes   []bool
}

func (c *countingDunning) ProcessDueRuns(ctx context.Context, _ string, _ int) (int, []error) {
	c.calls.Add(1)
	c.modesMu.Lock()
	c.modes = append(c.modes, postgres.Livemode(ctx))
	c.modesMu.Unlock()
	return 0, nil
}

func (c *countingDunning) observedModes() []bool {
	c.modesMu.Lock()
	defer c.modesMu.Unlock()
	out := make([]bool, len(c.modes))
	copy(out, c.modes)
	return out
}

type fixedTenants struct {
	ids []string
}

func (t *fixedTenants) ListTenantIDs(_ context.Context) ([]string, error) {
	return t.ids, nil
}

// TestScheduler_DunningFansOutPerLivemode verifies #13's core guarantee: every
// tick invokes the dunning body once per livemode per tenant, and each call
// carries the correct livemode in ctx (not the default-to-live fallback).
func TestScheduler_DunningFansOutPerLivemode(t *testing.T) {
	t.Parallel()

	dunning := &countingDunning{}
	s := &Scheduler{
		engine:  &Engine{},
		dunning: dunning,
		tenants: &fixedTenants{ids: []string{"t_1"}},
		batch:   1,
	}

	s.runDunningHalf(context.Background())

	modes := dunning.observedModes()
	if len(modes) != 2 {
		t.Fatalf("expected 2 calls (1 tenant × 2 modes); got %d", len(modes))
	}
	var sawLive, sawTest bool
	for _, m := range modes {
		if m {
			sawLive = true
		} else {
			sawTest = true
		}
	}
	if !sawLive || !sawTest {
		t.Fatalf("fan-out must cover both live and test modes; saw live=%v test=%v", sawLive, sawTest)
	}
}

// TestScheduler_RunDunningForMode_PanicsWithoutLivemode is the regression
// guard for #14: runDunningForMode is a mode-aware entry point and must
// panic if its caller forgot to wrap ctx with WithLivemode. Without this
// assertion the scheduler would silently route test-mode work into the
// live partition via the default-to-live fallback.
func TestScheduler_RunDunningForMode_PanicsWithoutLivemode(t *testing.T) {
	t.Parallel()

	s := &Scheduler{
		engine:  &Engine{},
		dunning: &countingDunning{},
		tenants: &fixedTenants{ids: []string{"t_1"}},
		batch:   1,
	}

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("runDunningForMode should panic on ctx without WithLivemode")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "without explicit livemode") {
			t.Fatalf("panic should mention missing livemode; got %q", msg)
		}
	}()
	s.runDunningForMode(context.Background(), true, []string{"t_1"})
}

// TestScheduler_Start_RefusesWithoutGate: an ungated scheduler at N>=2 is a
// double-billing machine, so Start returns at once instead of defaulting to
// "always lead". Mutation check: make nil gate mean AlwaysLead → fails.
func TestScheduler_Start_RefusesWithoutGate(t *testing.T) {
	t.Parallel()
	dunning := &countingDunning{}
	s := &Scheduler{engine: &Engine{}, dunning: dunning, tenants: &fixedTenants{ids: []string{"t_1"}}, batch: 1, interval: time.Millisecond}
	done := make(chan struct{})
	go func() { s.Start(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Start without a gate kept running")
	}
	if got := dunning.calls.Load(); got != 0 {
		t.Fatalf("dunning ran %d times ungated", got)
	}
}

// TestScheduler_Start_FollowerRunsNothingButStaysLive: a replica that never
// leads runs neither half — and still stamps onRun every poll, because
// /health/ready means "this replica's loop is alive", not "this replica
// is the leader" (a healthy follower must not read as a stalled scheduler).
func TestScheduler_Start_FollowerRunsNothingButStaysLive(t *testing.T) {
	t.Parallel()
	dunning := &countingDunning{}
	var polls atomic.Int32
	s := &Scheduler{engine: &Engine{}, dunning: dunning, tenants: &fixedTenants{ids: []string{"t_1"}}, batch: 1, interval: 2 * time.Millisecond}
	s.SetGate(leader.NeverLead{})
	s.SetOnRun(func() { polls.Add(1) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Start(ctx)
	waitFor(t, func() bool { return polls.Load() >= 5 }, time.Second, "follower stopped stamping liveness")
	if got := dunning.calls.Load(); got != 0 {
		t.Fatalf("follower ran dunning %d times; want 0", got)
	}
}

// TestScheduler_Start_RolesAreIndependent: billing and dunning are two
// leases, polled by two loops. A gate that leads dunning only must see
// BOTH roles asked for (independent polling) and run only the dunning
// body — the billing body never runs on a replica that does not lead it.
// Mutation check: run both halves under one Lead call → billing runs
// (and, on the zero Engine here, panics into the recover) → fails.
func TestScheduler_Start_RolesAreIndependent(t *testing.T) {
	t.Parallel()
	dunning := &countingDunning{}
	gate := newRoleGate(leader.RoleDunning)
	s := &Scheduler{engine: &Engine{}, dunning: dunning, tenants: &fixedTenants{ids: []string{"t_1"}}, batch: 1, interval: 2 * time.Millisecond}
	s.SetGate(gate)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Start(ctx)
	waitFor(t, func() bool { return dunning.calls.Load() >= 4 }, time.Second, "dunning never led two ticks")
	waitFor(t, func() bool { return gate.callsFor(leader.RoleBilling) >= 2 }, time.Second, "billing role was never polled independently")
	// 1 tenant × 2 modes per led tick: dunning fan-out still holds under the gate.
	modes := dunning.observedModes()
	var live, test bool
	for _, m := range modes {
		live, test = live || m, test || !m
	}
	if !live || !test {
		t.Fatalf("led dunning ticks must fan out to both modes; live=%v test=%v", live, test)
	}
}

// TestScheduler_Start_GateErrorRunsNothing: DB down at the gate skips the
// poll and keeps the loop alive; the body never runs ungated.
func TestScheduler_Start_GateErrorRunsNothing(t *testing.T) {
	t.Parallel()
	dunning := &countingDunning{}
	var polls atomic.Int32
	s := &Scheduler{engine: &Engine{}, dunning: dunning, tenants: &fixedTenants{ids: []string{"t_1"}}, batch: 1, interval: 2 * time.Millisecond}
	s.SetGate(leader.ErrGate{Err: errors.New("db down")})
	s.SetOnRun(func() { polls.Add(1) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Start(ctx)
	waitFor(t, func() bool { return polls.Load() >= 5 }, time.Second, "loop died on gate error")
	if got := dunning.calls.Load(); got != 0 {
		t.Fatalf("dunning ran %d times on gate error", got)
	}
}
