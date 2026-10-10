package invoice_test

import (
	"context"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestPerClockQueries_SQLValid is a regression for the ambiguous-column
// bug surfaced 2026-05-08 — the per-clock invoice queries
// (ListAutoChargePendingForClock, ListPendingTaxRetryForClock) joined
// invoices to another table (subscriptions then; customers today), and
// the SELECT had bare invCols (no alias prefix), so Postgres rejected
// `id`/`tenant_id`/etc. as ambiguous (SQLSTATE 42702). Live operator
// hit this on the first real Advance after ADR-029 shipped.
//
// This test executes each query against a real Postgres against an
// empty table — it asserts the SQL is valid (no syntax / ambiguous-
// column error) regardless of fixture data. Catches the same class
// of bug in CI before any operator hits it again. Mirrors the
// principle from feedback_long_term_means_cross_flow_audit: when
// adding code that reaches the SQL layer, exercise it with a real
// connection at least once, not just with mock interfaces.
func TestPerClockQueries_SQLValid(t *testing.T) {
	db := testutil.SetupTestDB(t)
	store := invoice.NewPostgresStore(db)
	ctx := postgres.WithLivemode(context.Background(), false)

	// Empty-DB smoke: each query should return ([], nil) — proving the
	// SQL parses and executes, not that anything matches.
	t.Run("ListAutoChargePendingForClock", func(t *testing.T) {
		got, err := store.ListAutoChargePendingForClock(ctx, "vlx_ten_test", "vlx_tclk_test", domain.InvoiceKeyset{}, 50)
		if err != nil {
			t.Fatalf("expected nil error on empty DB; got: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("expected empty slice; got %d rows", len(got))
		}
	})

	t.Run("ListPendingTaxRetryForClock", func(t *testing.T) {
		got, err := store.ListPendingTaxRetryForClock(ctx, "vlx_ten_test", "vlx_tclk_test",
			[]string{"provider_outage", "unknown"}, 8, 50)
		if err != nil {
			t.Fatalf("expected nil error on empty DB; got: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("expected empty slice; got %d rows", len(got))
		}
	})

}

// TestListAutoChargePendingForClock_IncludesOneOffInvoices is the
// regression for the stranded-one-off bug found live in FLOW TC4
// (2026-07-18): the query resolved clock membership through an INNER
// JOIN on subscriptions, so a one-off invoice (subscription_id NULL)
// on a clock-pinned customer was invisible to the catchup charge
// phase. The wall sweep correctly skips it (is_simulated gate), so
// nothing would EVER charge it — customer owed money, card on file,
// stranded forever. Clock membership now resolves through customers
// (the pin's owner, ADR-027), which covers both sub-cycle and one-off
// invoices; a sub's clock always equals its customer's clock
// (inherit-only, immutable), so sub-backed behavior is unchanged.
//
// Three-row fixture, three assertions:
//   - one-off on the PINNED customer  -> returned (FAILS on the old join)
//   - sub-cycle on the PINNED customer -> still returned
//   - one-off on a WALL customer       -> not returned
func TestListAutoChargePendingForClock_IncludesOneOffInvoices(t *testing.T) {
	db := testutil.SetupTestDB(t)
	store := invoice.NewPostgresStore(db)
	ctx := postgres.WithLivemode(context.Background(), false)

	const tenantID = "vlx_ten_oneoff_clk"
	const clockID = "vlx_tclk_oneoff_clk"

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
	// The 0021 livemode-autoset trigger overwrites NEW.livemode from the
	// app.livemode GUC; TxBypass doesn't set it, so pin the tx to test
	// mode or every seeded row lands livemode=true and trips the
	// test-clock CHECKs.
	mustExec(`SELECT set_config('app.livemode', 'off', true)`)
	mustExec(`INSERT INTO tenants (id, name, status) VALUES ($1, 'OneOff Clk', 'active')`, tenantID)
	mustExec(`INSERT INTO test_clocks (id, tenant_id, name, frozen_time, status, livemode)
		VALUES ($1, $2, 'oneoff', now(), 'ready', false)`, clockID, tenantID)
	mustExec(`INSERT INTO customers (id, tenant_id, external_id, display_name, email, test_clock_id, created_at, updated_at)
		VALUES ('cus_pinned_oo', $1, 'pinned_oo', 'Pinned', '', $2, now(), now())`, tenantID, clockID)
	mustExec(`INSERT INTO customers (id, tenant_id, external_id, display_name, email, created_at, updated_at)
		VALUES ('cus_wall_oo', $1, 'wall_oo', 'Wall', '', now(), now())`, tenantID)
	mustExec(`INSERT INTO subscriptions (id, tenant_id, code, display_name, customer_id, status, livemode, test_clock_id)
		VALUES ('sub_pinned_oo', $1, 'pinned-oo', 'pinned-oo', 'cus_pinned_oo', 'active', false, $2)`, tenantID, clockID)
	seedInv := func(id, custID, subID string) {
		sub := any(nil)
		if subID != "" {
			sub = subID
		}
		mustExec(`INSERT INTO invoices (id, tenant_id, customer_id, subscription_id, invoice_number,
			status, payment_status, auto_charge_pending, is_simulated, currency,
			subtotal_cents, total_amount_cents, amount_due_cents, tax_status,
			billing_period_start, billing_period_end, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $1, 'finalized', 'pending', true, true, 'USD',
			2500, 2500, 2500, 'ok', now(), now(), now(), now())`, id, tenantID, custID, sub)
	}
	seedInv("inv_oneoff_pinned", "cus_pinned_oo", "")             // the stranded case
	seedInv("inv_cycle_pinned", "cus_pinned_oo", "sub_pinned_oo") // sub-backed, unchanged
	seedInv("inv_oneoff_wall", "cus_wall_oo", "")                 // must not leak in
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	got, err := store.ListAutoChargePendingForClock(ctx, tenantID, clockID, domain.InvoiceKeyset{}, 50)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	found := map[string]bool{}
	for _, inv := range got {
		found[inv.ID] = true
	}
	if !found["inv_oneoff_pinned"] {
		t.Errorf("one-off invoice on the pinned customer NOT returned — the subscriptions join dropped it (the stranded-one-off bug)")
	}
	if !found["inv_cycle_pinned"] {
		t.Errorf("sub-cycle invoice on the pinned customer not returned — customer-join must cover sub-backed invoices too")
	}
	if found["inv_oneoff_wall"] {
		t.Errorf("wall-clock customer's invoice returned — clock scoping leaked")
	}
	if len(got) != 2 {
		t.Errorf("expected exactly 2 rows, got %d", len(got))
	}
}
