package dunning_test

import (
	"context"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/dunning"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestListDueRunsForClock_IncludesOneOffInvoiceRuns is the dunning twin of
// the #510 stranded-one-off regression (invoice.TestListAutoChargePending-
// ForClock_IncludesOneOffInvoices): the catchup query resolved clock
// membership through an INNER JOIN on subscriptions, so a dunning run on a
// ONE-OFF invoice (subscription_id NULL) for a clock-pinned customer
// matched no rows — and the wall sweep excludes it via is_simulated, so
// the run could NEVER advance: no retries, no escalation, no final action,
// forever "active, next retry in Nd". Clock membership now resolves
// through customers (the pin's owner, ADR-027); a sub's clock always
// equals its customer's clock (inherit-only), so sub-backed runs are
// unchanged.
//
// Also locks two sibling fixes from the same audit:
//   - ListRuns surfaces effective_now + test_clock_id for one-off-invoice
//     runs (the sub join rendered them wall-anchored and badge-less);
//   - ListEvents tie-breaks equal created_at by id (creation-ordered xid),
//     so same-sim-instant cascades render deterministically causal.
func TestListDueRunsForClock_IncludesOneOffInvoiceRuns(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)

	const tenantID = "vlx_ten_dun_oneoff"
	const clockID = "vlx_tclk_dun_oneoff"
	frozen := time.Date(2027, 6, 1, 0, 0, 0, 0, time.UTC)

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
	// 0021 livemode-autoset trigger reads the GUC; pin the tx to test mode.
	mustExec(`SELECT set_config('app.livemode', 'off', true)`)
	mustExec(`INSERT INTO tenants (id, name, status) VALUES ($1, 'Dun OneOff', 'active')`, tenantID)
	mustExec(`INSERT INTO test_clocks (id, tenant_id, name, frozen_time, status, livemode)
		VALUES ($1, $2, 'dunoneoff', $3, 'ready', false)`, clockID, tenantID, frozen)
	mustExec(`INSERT INTO customers (id, tenant_id, external_id, display_name, email, test_clock_id, created_at, updated_at)
		VALUES ('cus_dun_pin', $1, 'dun_pin', 'Pin', '', $2, $3, $3)`, tenantID, clockID, frozen)
	mustExec(`INSERT INTO customers (id, tenant_id, external_id, display_name, email, created_at, updated_at)
		VALUES ('cus_dun_wall', $1, 'dun_wall', 'Wall', '', $2, $2)`, tenantID, frozen)
	mustExec(`INSERT INTO subscriptions (id, tenant_id, code, display_name, customer_id, status, livemode, test_clock_id)
		VALUES ('sub_dun_pin', $1, 'dun-pin', 'dun-pin', 'cus_dun_pin', 'active', false, $2)`, tenantID, clockID)
	mustExec(`INSERT INTO dunning_policies (id, tenant_id, name, enabled, is_default, retry_schedule, max_retry_attempts, final_subscription_action, final_invoice_action, grace_period_days, livemode)
		VALUES ('dpol_oneoff', $1, 'P', true, true, '["72h"]', 3, 'pause', 'none', 3, false)`, tenantID)
	seedInvRun := func(invID, custID, subID, runID string, due time.Time) {
		sub := any(nil)
		if subID != "" {
			sub = subID
		}
		mustExec(`INSERT INTO invoices (id, tenant_id, customer_id, subscription_id, invoice_number,
			status, payment_status, is_simulated, currency, subtotal_cents, total_amount_cents,
			amount_due_cents, tax_status, billing_period_start, billing_period_end, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $1, 'finalized', 'failed', true, 'USD', 2500, 2500, 2500, 'ok', $5, $5, $5, $5)`,
			invID, tenantID, custID, sub, frozen.Add(-24*time.Hour))
		mustExec(`INSERT INTO invoice_dunning_runs (id, tenant_id, invoice_id, customer_id, policy_id, state, reason, attempt_count, next_action_at, paused, created_at, updated_at, livemode)
		VALUES ($1, $2, $3, $4, 'dpol_oneoff', 'active', 'payment_failed', 0, $5, false, $6, $6, false)`,
			runID, tenantID, invID, custID, due, frozen.Add(-24*time.Hour))
	}
	seedInvRun("inv_dun_oneoff", "cus_dun_pin", "", "run_oneoff_pin", frozen.Add(-time.Hour))          // the stranded case
	seedInvRun("inv_dun_cycle", "cus_dun_pin", "sub_dun_pin", "run_cycle_pin", frozen.Add(-time.Hour)) // sub-backed, unchanged
	seedInvRun("inv_dun_wall", "cus_dun_wall", "", "run_oneoff_wall", frozen.Add(-time.Hour))          // must not leak in
	// A future-due run on the pinned customer: frozen compare must exclude it.
	seedInvRun("inv_dun_future", "cus_dun_pin", "", "run_future_pin", frozen.Add(48*time.Hour))
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	dstore := dunning.NewPostgresStore(db)

	t.Run("ForClock includes one-off runs via the customer pin", func(t *testing.T) {
		due, err := dstore.ListDueRunsForClock(ctx, tenantID, clockID, frozen, 50)
		if err != nil {
			t.Fatalf("ListDueRunsForClock: %v", err)
		}
		got := map[string]bool{}
		for _, r := range due {
			got[r.ID] = true
		}
		if !got["run_oneoff_pin"] {
			t.Error("one-off invoice's run NOT returned — the subscriptions join stranded it (the #510 class, dunning twin)")
		}
		if !got["run_cycle_pin"] {
			t.Error("sub-backed run not returned — the customers join must cover sub-backed invoices too")
		}
		if got["run_oneoff_wall"] {
			t.Error("wall customer's run leaked into the clock sweep")
		}
		if got["run_future_pin"] {
			t.Error("future-due run returned — must compare against frozen_time")
		}
	})

	t.Run("ListRuns anchors one-off runs on the customer clock", func(t *testing.T) {
		runs, _, err := dstore.ListRuns(ctx, dunning.RunListFilter{TenantID: tenantID})
		if err != nil {
			t.Fatalf("ListRuns: %v", err)
		}
		var oneoff *domain.InvoiceDunningRun
		for i := range runs {
			if runs[i].ID == "run_oneoff_pin" {
				oneoff = &runs[i]
			}
		}
		if oneoff == nil {
			t.Fatal("run_oneoff_pin missing from ListRuns")
		}
		if oneoff.EffectiveNow == nil || !oneoff.EffectiveNow.Equal(frozen) {
			t.Errorf("effective_now: got %v, want the customer clock's frozen_time %v (was NULL under the sub join — wall-anchored relative times)", oneoff.EffectiveNow, frozen)
		}
		if oneoff.TestClockID != clockID {
			t.Errorf("test_clock_id: got %q, want %q (drives the badge server-authoritatively)", oneoff.TestClockID, clockID)
		}
	})

	t.Run("ListEvents tie-breaks equal created_at by id", func(t *testing.T) {
		sameInstant := frozen.Add(-time.Hour)
		tx2, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer postgres.Rollback(tx2)
		// ids chosen so lexical id order == causal order (xid ids are
		// creation-ordered in production; here we pin it explicitly).
		for _, ev := range []struct{ id, typ string }{
			{"devt_a_started", "dunning_started"},
			{"devt_b_retry", "retry_scheduled"},
		} {
			if _, err := tx2.ExecContext(ctx, `INSERT INTO invoice_dunning_events (id, run_id, tenant_id, invoice_id, event_type, state, attempt_count, created_at, livemode)
				VALUES ($1, 'run_oneoff_pin', $2, 'inv_dun_oneoff', $3, 'active', 0, $4, false)`,
				ev.id, tenantID, ev.typ, sameInstant); err != nil {
				t.Fatalf("seed event: %v", err)
			}
		}
		if err := tx2.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
		evts, err := dstore.ListEvents(ctx, tenantID, "run_oneoff_pin")
		if err != nil {
			t.Fatalf("ListEvents: %v", err)
		}
		if len(evts) != 2 {
			t.Fatalf("events: got %d, want 2", len(evts))
		}
		if evts[0].EventType != "dunning_started" || evts[1].EventType != "retry_scheduled" {
			t.Errorf("equal created_at must order by id (causal): got %s then %s", evts[0].EventType, evts[1].EventType)
		}
	})
}
