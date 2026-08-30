package leader_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/platform/leader"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// Every test below runs against the real leader_leases table (migration
// 0174) as the app role, with the admin pool only to force row state.
// Rows are reset by UPDATE (never TRUNCATE) so the seeded tokens survive.

func resetRoles(t *testing.T, admin *sql.DB) {
	t.Helper()
	ctx := context.Background()
	// The test harness truncates every public table at setup (including
	// leader_leases — deliberately no special case: in production the seed
	// comes from migration 0174 and the ACQUIRE upsert recreates a missing
	// row). Re-seed here the same way the migration does, then clear state.
	for _, r := range leader.Roles {
		if _, err := admin.ExecContext(ctx, `INSERT INTO leader_leases (role, holder_token) VALUES ($1, (extract(epoch FROM clock_timestamp()) * 1000)::bigint) ON CONFLICT (role) DO NOTHING`, string(r)); err != nil {
			t.Fatalf("reseed %s: %v", r, err)
		}
	}
	if _, err := admin.ExecContext(ctx, `UPDATE leader_leases SET holder_id=NULL, acquired_at=NULL, heartbeat_at=NULL, expires_at=NULL, last_tick_ended_at=NULL, last_tick_holder=NULL, paused_at=NULL, paused_by=NULL, pause_reason=NULL`); err != nil {
		t.Fatalf("reset roles: %v", err)
	}
}

func rowToken(t *testing.T, admin *sql.DB, role leader.Role) (token int64, holder sql.NullString, expires sql.NullTime) {
	t.Helper()
	if err := admin.QueryRowContext(context.Background(), `SELECT holder_token, holder_id, expires_at FROM leader_leases WHERE role=$1`, string(role)).Scan(&token, &holder, &expires); err != nil {
		t.Fatalf("row %s: %v", role, err)
	}
	return
}

// TestLease_RolesMatchCheckConstraint pins the Go role list to migration
// 0174's CHECK constraint in both directions (enum + CHECK sync rule): every
// Go role is insertable, a role the CHECK does not know is rejected, and the
// CHECK lists exactly the Go roles.
func TestLease_RolesMatchCheckConstraint(t *testing.T) {
	db := testutil.SetupTestDB(t)
	_ = db
	admin := testutil.AdminPool(t)
	resetRoles(t, admin) // inserts every Go role — a role outside the CHECK would fail here
	ctx := context.Background()
	if _, err := admin.ExecContext(ctx, `INSERT INTO leader_leases (role, holder_token) VALUES ('not_a_role', 1)`); err == nil {
		t.Fatal("a role outside the CHECK constraint was accepted")
	}
	var check string
	if err := admin.QueryRowContext(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid='leader_leases'::regclass AND contype='c' AND pg_get_constraintdef(oid) LIKE '%role%'`).Scan(&check); err != nil {
		t.Fatalf("role CHECK: %v", err)
	}
	inCheck := map[string]bool{}
	for _, q := range strings.Split(check, "'") {
		if q != "" && !strings.ContainsAny(q, "() ,") && !strings.Contains(q, "CHECK") {
			inCheck[q] = true
		}
	}
	for _, r := range leader.Roles {
		if !inCheck[string(r)] {
			t.Errorf("Go role %q is not in the CHECK constraint: %s", r, check)
		}
		delete(inCheck, string(r))
	}
	for extra := range inCheck {
		if strings.Contains(check, "'"+extra+"'") && extra != "text" && extra != "ARRAY" {
			t.Errorf("CHECK constraint lists %q, which has no Go constant", extra)
		}
	}
}

// TestLease_TwoManagersContend: two managers, many goroutines, one role that
// is always due (interval 0): exactly one leads per round and the token
// strictly increases.
func TestLease_TwoManagersContend(t *testing.T) {
	db := testutil.SetupTestDB(t)
	admin := testutil.AdminPool(t)
	resetRoles(t, admin)
	a, b := leader.New(db.Pool, nil), leader.New(db.Pool, nil)

	var lastToken int64
	for round := 0; round < 10; round++ {
		var led atomic.Int64
		var wg sync.WaitGroup
		for i := 0; i < 20; i++ {
			wg.Add(1)
			m := a
			if i%2 == 1 {
				m = b
			}
			go func() {
				defer wg.Done()
				ok, err := m.Lead(context.Background(), leader.RoleWebhookOutbox, 0, func(ctx context.Context) {
					_, tok, ferr := leader.Fence(ctx)
					if ferr != nil {
						t.Errorf("led work without a token: %v", ferr)
					}
					if tok <= atomic.LoadInt64(&lastToken) {
						t.Errorf("token did not increase: %d <= %d", tok, lastToken)
					}
					atomic.StoreInt64(&lastToken, tok)
					time.Sleep(30 * time.Millisecond) // hold the lease while siblings try
				})
				if err != nil {
					t.Errorf("Lead: %v", err)
				}
				if ok {
					led.Add(1)
				}
			}()
		}
		wg.Wait()
		if led.Load() != 1 {
			t.Fatalf("round %d: %d leaders, want exactly 1", round, led.Load())
		}
	}
}

// TestLease_TakeoverAfterExpiry: a held lease whose holder stopped
// heartbeating (simulated by shortening expires_at on the DB clock) is
// acquirable once it expires — and not before — with token+1.
func TestLease_TakeoverAfterExpiry(t *testing.T) {
	db := testutil.SetupTestDB(t)
	admin := testutil.AdminPool(t)
	resetRoles(t, admin)
	ctx := context.Background()
	// A dead leader: held row, no heartbeat coming; 1.5 s of lease left.
	var deadToken int64
	if err := admin.QueryRowContext(ctx, `UPDATE leader_leases SET holder_id='dead:1:deadbeef', acquired_at=now(), heartbeat_at=now(), expires_at=now()+interval '1500 milliseconds', holder_token=holder_token+1 WHERE role='billing' RETURNING holder_token`).Scan(&deadToken); err != nil {
		t.Fatalf("seed dead leader: %v", err)
	}
	m := leader.New(db.Pool, nil)
	if led, _ := m.Lead(ctx, leader.RoleBilling, 0, func(context.Context) {}); led {
		t.Fatal("led while the dead leader's lease was still live (control)")
	}
	deadline := time.Now().Add(leader.LeaseTTL)
	var ledAt time.Time
	for time.Now().Before(deadline) {
		led, err := m.Lead(ctx, leader.RoleBilling, 0, func(ctx context.Context) {
			_, tok, _ := leader.Fence(ctx)
			if tok != deadToken+1 {
				t.Errorf("takeover token %d, want %d", tok, deadToken+1)
			}
		})
		if err != nil {
			t.Fatalf("Lead: %v", err)
		}
		if led {
			ledAt = time.Now()
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if ledAt.IsZero() {
		t.Fatal("never took over an expired lease")
	}
}

// TestLease_RenewFailureCancelsWork: a takeover (admin bumps the token)
// cancels the holder's work within one heartbeat + statement timeout, with
// cause ErrLeaseLost and reason "takeover"; the interrupted tick leaves the
// role due (no completed stamp).
func TestLease_RenewFailureCancelsWork(t *testing.T) {
	db := testutil.SetupTestDB(t)
	admin := testutil.AdminPool(t)
	resetRoles(t, admin)
	var reason atomic.Value
	m := leader.New(db.Pool, func(_ leader.Role, r string) { reason.Store(r) })

	var cause atomic.Value
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = m.Lead(context.Background(), leader.RoleDunning, time.Hour, func(ctx context.Context) {
			<-ctx.Done()
			cause.Store(context.Cause(ctx))
		})
	}()
	// Wait until the row is held, then steal it.
	for i := 0; i < 50; i++ {
		_, holder, _ := rowToken(t, admin, leader.RoleDunning)
		if holder.Valid {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := admin.ExecContext(context.Background(), `UPDATE leader_leases SET holder_token=holder_token+1, holder_id='thief:1:00000000' WHERE role='dunning'`); err != nil {
		t.Fatalf("steal: %v", err)
	}
	select {
	case <-done:
	case <-time.After(leader.HeartbeatEvery + leader.StatementTimeout + 2*time.Second):
		t.Fatal("work was not cancelled after the lease was taken over")
	}
	if c, _ := cause.Load().(error); !errors.Is(c, leader.ErrLeaseLost) {
		t.Fatalf("cancel cause = %v, want ErrLeaseLost", c)
	}
	if r, _ := reason.Load().(string); r != "takeover" {
		t.Fatalf("lost reason = %q, want takeover", r)
	}
	var ended sql.NullTime
	_ = admin.QueryRowContext(context.Background(), `SELECT last_tick_ended_at FROM leader_leases WHERE role='dunning'`).Scan(&ended)
	if ended.Valid && !ended.Time.Before(time.Now().Add(-time.Minute)) {
		t.Fatalf("an interrupted tick must not stamp a completed last_tick_ended_at (got %v)", ended.Time)
	}
}

// failingRenews is an Executor that delegates everything except RENEW, which
// fails after the first — simulates a database that stops answering.
type failingRenews struct {
	*sql.DB
	renews atomic.Int64
}

func (f *failingRenews) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	if strings.Contains(q, "SET heartbeat_at = now()") && f.renews.Add(1) > 0 {
		cctx, cancel := context.WithCancel(ctx)
		cancel() // an already-cancelled ctx: the driver errors immediately
		return f.DB.QueryRowContext(cctx, q, args...)
	}
	return f.DB.QueryRowContext(ctx, q, args...)
}

// TestLease_HeartbeatTimeoutAbandons: when RENEW cannot be acknowledged, the
// holder cancels its own work after AbandonAfter with reason
// heartbeat_timeout — before the lease could have expired for a successor.
func TestLease_HeartbeatTimeoutAbandons(t *testing.T) {
	db := testutil.SetupTestDB(t)
	admin := testutil.AdminPool(t)
	resetRoles(t, admin)
	var reason atomic.Value
	m := leader.New(&failingRenews{DB: db.Pool}, func(_ leader.Role, r string) { reason.Store(r) })

	start := time.Now()
	var cause atomic.Value
	led, err := m.Lead(context.Background(), leader.RoleEmailOutbox, time.Hour, func(ctx context.Context) {
		<-ctx.Done()
		cause.Store(context.Cause(ctx))
	})
	took := time.Since(start)
	if err != nil || !led {
		t.Fatalf("Lead: led=%v err=%v", led, err)
	}
	if c, _ := cause.Load().(error); !errors.Is(c, leader.ErrLeaseLost) {
		t.Fatalf("cause = %v, want ErrLeaseLost", c)
	}
	if r, _ := reason.Load().(string); r != "heartbeat_timeout" {
		t.Fatalf("reason = %q, want heartbeat_timeout", r)
	}
	if took < leader.AbandonAfter || took > leader.LeaseTTL {
		t.Fatalf("abandoned after %v, want within [%v, %v)", took, leader.AbandonAfter, leader.LeaseTTL)
	}
}

// TestLease_PauseUnpause: leader_pause evicts the holder and blocks acquire
// cluster-wide; a mid-tick holder is cancelled within a heartbeat; unpause
// makes the role due at once.
func TestLease_PauseUnpause(t *testing.T) {
	db := testutil.SetupTestDB(t)
	admin := testutil.AdminPool(t)
	resetRoles(t, admin)
	ctx := context.Background()
	var reason atomic.Value
	m := leader.New(db.Pool, func(_ leader.Role, r string) { reason.Store(r) })

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = m.Lead(ctx, leader.RoleWebhookDelivery, time.Hour, func(ctx context.Context) { <-ctx.Done() })
	}()
	for i := 0; i < 50; i++ {
		if _, holder, _ := rowToken(t, admin, leader.RoleWebhookDelivery); holder.Valid {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	var paused sql.NullBool
	if err := admin.QueryRowContext(ctx, `SELECT leader_pause('webhook_delivery', 'test', 'FLOW E7')`).Scan(&paused); err != nil || !paused.Valid || !paused.Bool {
		t.Fatalf("pause: %v %v", paused, err)
	}
	select {
	case <-done:
	case <-time.After(leader.HeartbeatEvery + leader.StatementTimeout + 2*time.Second):
		t.Fatal("holder was not cancelled by the pause")
	}
	if r, _ := reason.Load().(string); r != "paused" {
		t.Fatalf("reason = %q, want paused", r)
	}
	if led, _ := m.Lead(ctx, leader.RoleWebhookDelivery, 0, func(context.Context) {}); led {
		t.Fatal("led a paused role")
	}
	if err := admin.QueryRowContext(ctx, `SELECT leader_pause('webhook_delivery', 'test', 'again')`).Scan(&paused); err != nil || paused.Valid {
		t.Fatalf("pausing an already-paused role must return NULL, got %v %v", paused, err)
	}
	if err := admin.QueryRowContext(ctx, `SELECT leader_unpause('webhook_delivery')`).Scan(&paused); err != nil || !paused.Valid {
		t.Fatalf("unpause: %v %v", paused, err)
	}
	if led, err := m.Lead(ctx, leader.RoleWebhookDelivery, time.Hour, func(context.Context) {}); !led || err != nil {
		t.Fatalf("after unpause the role must be due at once: led=%v err=%v", led, err)
	}
}

// TestLease_CleanShutdownReleasesImmediately: cancelling the parent ctx
// during work releases at once (no TTL wait) and leaves the role due for
// another manager.
func TestLease_CleanShutdownReleasesImmediately(t *testing.T) {
	db := testutil.SetupTestDB(t)
	admin := testutil.AdminPool(t)
	resetRoles(t, admin)
	parent, stop := context.WithCancel(context.Background())
	a := leader.New(db.Pool, nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = a.Lead(parent, leader.RoleBilling, time.Hour, func(ctx context.Context) { <-ctx.Done() })
	}()
	for i := 0; i < 50; i++ {
		if _, holder, _ := rowToken(t, admin, leader.RoleBilling); holder.Valid {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	stop() // SIGTERM
	<-done
	if _, holder, _ := rowToken(t, admin, leader.RoleBilling); holder.Valid {
		t.Fatal("lease still held after a clean shutdown")
	}
	b := leader.New(db.Pool, nil)
	if led, err := b.Lead(context.Background(), leader.RoleBilling, time.Hour, func(context.Context) {}); !led || err != nil {
		t.Fatalf("another manager must lead immediately after a clean release: led=%v err=%v", led, err)
	}
}

// TestLease_CadenceIsClusterWide: after a COMPLETED tick nobody leads the
// role until interval has elapsed on the DB clock; backdating the stamp makes
// it due again.
func TestLease_CadenceIsClusterWide(t *testing.T) {
	db := testutil.SetupTestDB(t)
	admin := testutil.AdminPool(t)
	resetRoles(t, admin)
	ctx := context.Background()
	a, b := leader.New(db.Pool, nil), leader.New(db.Pool, nil)
	if led, err := a.Lead(ctx, leader.RoleDunning, time.Hour, func(context.Context) {}); !led || err != nil {
		t.Fatalf("first tick: led=%v err=%v", led, err)
	}
	if led, _ := b.Lead(ctx, leader.RoleDunning, time.Hour, func(context.Context) {}); led {
		t.Fatal("a second manager led within the interval after a completed tick")
	}
	if _, err := admin.ExecContext(ctx, `UPDATE leader_leases SET last_tick_ended_at = now() - interval '2 hours' WHERE role='dunning'`); err != nil {
		t.Fatal(err)
	}
	if led, err := b.Lead(ctx, leader.RoleDunning, time.Hour, func(context.Context) {}); !led || err != nil {
		t.Fatalf("after the interval the role must be due: led=%v err=%v", led, err)
	}
}

// TestLease_MissingRowRecreated: a deleted role row is recreated by the next
// ACQUIRE with a token above the deleted one (epoch-ms seed).
func TestLease_MissingRowRecreated(t *testing.T) {
	db := testutil.SetupTestDB(t)
	admin := testutil.AdminPool(t)
	resetRoles(t, admin)
	ctx := context.Background()
	old, _, _ := rowToken(t, admin, leader.RoleEmailOutbox)
	if _, err := admin.ExecContext(ctx, `DELETE FROM leader_leases WHERE role='email_outbox'`); err != nil {
		t.Fatal(err)
	}
	m := leader.New(db.Pool, nil)
	var got int64
	if led, err := m.Lead(ctx, leader.RoleEmailOutbox, 0, func(ctx context.Context) { _, got, _ = leader.Fence(ctx) }); !led || err != nil {
		t.Fatalf("Lead after delete: led=%v err=%v", led, err)
	}
	if got <= old {
		t.Fatalf("recreated token %d must exceed the deleted token %d", got, old)
	}
}
