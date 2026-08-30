package subscription_test

import (
	"context"
	"github.com/sagarsuperuser/velox/internal/platform/leader"
	"github.com/sagarsuperuser/velox/internal/platform/leader/leadertest"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/subscription"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestDueScans_CancelArm_Scoping is the ADR-097 mutation-verified lock on
// the three due-subscription queries' new cancel arm. The arm must admit
// EXACTLY the rows the engine's mid-period branch can make progress on —
// an admitted row the branch declines re-fetches on every pass forever
// (the 2026-05-31 spin-bug shape). Scoping under attack:
//
//   - status: active only (trialing cancels belong to the trial scan);
//   - clock fencing: the wall queries must NOT pick up clock-pinned subs
//     via the OR arm (drip-bill race the ADR-028 comment documents), and
//     the clock query compares against tc.frozen_time, never wall now;
//   - livemode partition on GetDueBilling.
//
// Mutation seam: remove the parentheses around the OR arm (binding it
// outside the AND chain) and the clock-pinned + cross-livemode assertions
// fail; drop the status='active' gate and the trialing assertion fails.
func TestDueScans_CancelArm_Scoping(t *testing.T) {
	db := testutil.SetupTestDB(t)
	store := subscription.NewPostgresStore(db)
	ctx := leadertest.Token(t, testutil.AdminPool(t), postgres.WithLivemode(context.Background(), false), leader.RoleBilling)

	const tenantID = "vlx_ten_cancelarm"
	const clockID = "vlx_tclk_cancelarm"

	wallNow := time.Now().UTC().Truncate(time.Microsecond)
	frozen := wallNow.Add(-90 * 24 * time.Hour) // sim time lags wall by 90d
	pastCancel := wallNow.Add(-1 * time.Hour)   // due vs wall, future vs frozen
	simDueCancel := frozen.Add(-1 * time.Hour)  // due vs frozen too
	futureBilling := wallNow.Add(300 * 24 * time.Hour)

	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	// 0021 livemode-autoset trigger reads the app.livemode GUC; pin the tx
	// to test mode or every row lands livemode=true.
	mustExec(`SELECT set_config('app.livemode', 'off', true)`)
	mustExec(`INSERT INTO tenants (id, name, status) VALUES ($1, 'CancelArm', 'active')`, tenantID)
	mustExec(`INSERT INTO test_clocks (id, tenant_id, name, frozen_time, status, livemode)
		VALUES ($1, $2, 'cancelarm', $3, 'ready', false)`, clockID, tenantID, frozen)
	mustExec(`INSERT INTO customers (id, tenant_id, external_id, display_name, email, created_at, updated_at)
		VALUES ('cus_ca_wall', $1, 'ca_wall', 'Wall', '', $2, $2),
		       ('cus_ca_pin',  $1, 'ca_pin',  'Pin',  '', $2, $2)`, tenantID, wallNow)

	seedSub := func(id, custID, status string, clock any, cancelAt time.Time, livemode bool) {
		mustExec(`INSERT INTO subscriptions (
			id, tenant_id, code, display_name, customer_id, status, billing_time,
			current_billing_period_start, current_billing_period_end, next_billing_at,
			cancel_at, livemode, test_clock_id, created_at, updated_at
		) VALUES ($1, $2, $1, $1, $3, $4, 'calendar', $5, $6, $6, $7, $8, $9, $5, $5)`,
			id, tenantID, custID, status,
			wallNow.Add(-30*24*time.Hour), futureBilling, cancelAt, livemode, clock)
	}
	// The motivating row: active, wall, cancel due, boundary far future.
	seedSub("sub_ca_hit", "cus_ca_wall", "active", nil, pastCancel, false)
	// Trialing with a due cancel: must NOT be admitted by the arm.
	seedSub("sub_ca_trial", "cus_ca_wall", "trialing", nil, pastCancel, false)
	// Clock-pinned, cancel due vs WALL but future vs FROZEN: invisible to
	// both the wall query (clock fence) and the clock query (frozen compare).
	seedSub("sub_ca_pin_future", "cus_ca_pin", "active", clockID, pastCancel, false)
	// Clock-pinned, cancel due vs frozen: the clock query's hit.
	seedSub("sub_ca_pin_due", "cus_ca_pin", "active", clockID, simDueCancel, false)
	// Cross-livemode: due cancel in LIVE mode must not leak into a test scan.
	// The 0021 autoset trigger overwrites livemode from the GUC — flip it
	// around this one insert or the row lands test-mode and the assertion
	// tests nothing.
	mustExec(`SELECT set_config('app.livemode', 'on', true)`)
	seedSub("sub_ca_live", "cus_ca_wall", "active", nil, pastCancel, true)
	mustExec(`SELECT set_config('app.livemode', 'off', true)`)
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	t.Run("GetDueBilling wall", func(t *testing.T) {
		got, err := store.GetDueBilling(ctx, wallNow, 50)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		found := map[string]bool{}
		for _, s := range got {
			found[s.ID] = true
		}
		if !found["sub_ca_hit"] {
			t.Error("active wall sub with due cancel_at NOT returned — the ADR-097 arm is missing (the stranded-cancel bug)")
		}
		if found["sub_ca_trial"] {
			t.Error("trialing sub admitted by the cancel arm — livelock decliner (must be active-only)")
		}
		if found["sub_ca_pin_future"] || found["sub_ca_pin_due"] {
			t.Error("clock-pinned sub leaked into the WALL scan via the cancel arm — OR binds outside the clock fence")
		}
		if found["sub_ca_live"] {
			t.Error("live-mode sub leaked into a test-mode scan via the cancel arm — OR binds outside the livemode fence")
		}
	})

	t.Run("GetDueBillingForClock frozen compare", func(t *testing.T) {
		got, err := store.GetDueBillingForClock(ctx, tenantID, clockID, 50)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		found := map[string]bool{}
		for _, s := range got {
			found[s.ID] = true
		}
		if !found["sub_ca_pin_due"] {
			t.Error("pinned sub with cancel_at due vs frozen_time NOT returned by the clock scan")
		}
		if found["sub_ca_pin_future"] {
			t.Error("clock scan admitted a cancel_at only due vs WALL time — must compare tc.frozen_time")
		}
	})

	t.Run("GetDueBillingForTenant carries the same cancel arm", func(t *testing.T) {
		// The tenant manual-run scan is the THIRD due-subscription query
		// (ADR-097 names all three); it carried the arm untested — the
		// header's "three queries" claim was only two-thirds true until
		// this leg existed (2026-07-19 truth audit).
		got, err := store.GetDueBillingForTenant(ctx, tenantID, wallNow, 50)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		found := map[string]bool{}
		for _, s := range got {
			found[s.ID] = true
		}
		if !found["sub_ca_hit"] {
			t.Error("active sub with due cancel_at NOT returned by the tenant scan — its ADR-097 arm is missing")
		}
		if found["sub_ca_trial"] {
			t.Error("trialing sub admitted by the tenant scan's cancel arm — livelock decliner (must be active-only)")
		}
		if found["sub_ca_pin_future"] || found["sub_ca_pin_due"] {
			t.Error("clock-pinned sub leaked into the tenant WALL scan via the cancel arm")
		}
		if found["sub_ca_live"] {
			t.Error("live-mode sub leaked into a test-mode tenant scan via the cancel arm")
		}
	})
}
