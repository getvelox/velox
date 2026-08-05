package invoice_test

import (
	"context"
	"testing"

	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestListPendingTaxReversal_FindsOrphanAndSelfClears is the real-Postgres proof
// of the tax-reversal recovery sweep (#30): a voided stripe_tax invoice that
// still carries a committed tax_transaction_id but has tax_reversed_at NULL (a
// reversal that failed upstream) is discovered by ListPendingTaxReversal, and
// once MarkTaxReversed stamps it the row falls out of the scan. The self-
// clearing marker is what keeps the sweep from re-reversing an already-reversed
// invoice every tick.
func TestListPendingTaxReversal_FindsOrphanAndSelfClears(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	store := invoice.NewPostgresStore(db)
	tenantID := testutil.CreateTestTenant(t, db, "Tax Reversal Sweep")
	invID := seedDraftInvoice(t, db, tenantID)

	// Make it a voided stripe_tax invoice with a committed transaction and no
	// confirmed reversal — exactly the orphan the sweep must pick up.
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin seed tx: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE invoices
		   SET status = 'voided', voided_at = now(),
		       tax_provider = 'stripe_tax', tax_status = 'ok',
		       tax_transaction_id = 'tx_seed',
		       total_amount_cents = 1000, amount_due_cents = 1000,
		       updated_at = now()
		 WHERE id = $1`, invID); err != nil {
		t.Fatalf("seed voided stripe_tax invoice: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit seed tx: %v", err)
	}

	contains := func(t *testing.T) bool {
		t.Helper()
		pending, err := store.ListPendingTaxReversal(ctx, 50, false)
		if err != nil {
			t.Fatalf("ListPendingTaxReversal: %v", err)
		}
		for _, inv := range pending {
			if inv.ID == invID {
				return true
			}
		}
		return false
	}

	if !contains(t) {
		t.Fatal("the unreversed voided stripe_tax invoice must appear in the reversal sweep")
	}

	if err := store.MarkTaxReversed(ctx, tenantID, invID); err != nil {
		t.Fatalf("MarkTaxReversed: %v", err)
	}

	if contains(t) {
		t.Fatal("after MarkTaxReversed the invoice must fall out of the reversal sweep (self-clearing)")
	}

	// MarkTaxReversed is idempotent — a second stamp is a no-op, not an error.
	if err := store.MarkTaxReversed(ctx, tenantID, invID); err != nil {
		t.Fatalf("second MarkTaxReversed must be a no-op: %v", err)
	}

	// MarkTaxReversed on a non-existent / cross-tenant id is a silent no-op
	// (RLS-scoped UPDATE matches zero rows), NOT an error — pins the contract
	// the memStore mock mirrors.
	if err := store.MarkTaxReversed(ctx, tenantID, "inv_does_not_exist"); err != nil {
		t.Fatalf("MarkTaxReversed on a missing id must be a no-op: %v", err)
	}
}

// TestListPendingTaxReversal_ExcludesSimulatedAndIncludesOneOff covers the
// predicate branches the happy-path test doesn't: a simulated (test-clock)
// invoice is EXCLUDED via is_simulated (the durable flag that catches both
// subscription- and customer-pinned one-offs a subscriptions-join would miss),
// and a one-off invoice (NULL subscription_id) is INCLUDED.
func TestListPendingTaxReversal_ExcludesSimulatedAndIncludesOneOff(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	store := invoice.NewPostgresStore(db)

	// Two tenants: seedDraftInvoice uses a fixed customer external_id, so each
	// call must land in its own tenant. The sweep scans cross-tenant (TxBypass),
	// so one scan sees both rows.
	tenantA := testutil.CreateTestTenant(t, db, "Tax Reversal Predicate A")
	tenantB := testutil.CreateTestTenant(t, db, "Tax Reversal Predicate B")
	simID := seedDraftInvoice(t, db, tenantA)
	oneOffID := seedDraftInvoice(t, db, tenantB)

	mutate := func(tenantID, id, sql string) {
		t.Helper()
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin seed tx: %v", err)
		}
		if _, err := tx.ExecContext(ctx, sql, id); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit seed tx: %v", err)
		}
	}
	// (1) A simulated voided stripe_tax invoice — must be EXCLUDED.
	mutate(tenantA, simID, `
		UPDATE invoices SET status='voided', voided_at=now(), tax_provider='stripe_tax',
		       tax_status='ok', tax_transaction_id='tx_sim', total_amount_cents=1000,
		       amount_due_cents=1000, is_simulated=true, updated_at=now()
		 WHERE id=$1`)
	// (2) A one-off (subscription_id NULL) voided stripe_tax invoice — must be INCLUDED.
	mutate(tenantB, oneOffID, `
		UPDATE invoices SET subscription_id=NULL, status='voided', voided_at=now(),
		       tax_provider='stripe_tax', tax_status='ok', tax_transaction_id='tx_oneoff',
		       total_amount_cents=1000, amount_due_cents=1000, is_simulated=false, updated_at=now()
		 WHERE id=$1`)

	pending, err := store.ListPendingTaxReversal(ctx, 200, false)
	if err != nil {
		t.Fatalf("ListPendingTaxReversal: %v", err)
	}
	got := map[string]bool{}
	for _, inv := range pending {
		got[inv.ID] = true
	}
	if got[simID] {
		t.Error("a simulated (test-clock) invoice must be excluded from the reversal sweep")
	}
	if !got[oneOffID] {
		t.Error("a one-off invoice (NULL subscription_id) must be included in the reversal sweep")
	}
}

// TestListPendingTaxReversal_AgedOrphanIsNotStranded is the ADR-111 follow-up:
// the sweep used to carry `updated_at > now() - interval '24 hours'`, copied
// from ListPendingTaxCommit where it matches a real ceiling (the Stripe Tax
// calculation TTL). A reversal has no such ceiling, so the window only ever
// discarded work — measured on the live database at 4 voided invoices holding
// $46.69 of committed tax, over-remitted permanently and named by nothing.
//
// The negative control is load-bearing. Removing a WHERE clause is exactly the
// kind of fix that "works" by sweeping everything, so the second half asserts
// that an equally OLD invoice which needs no reversal — same age, same
// provider, tax_reversed_at already stamped — still stays out. Without it a
// sweep that reversed already-reversed invoices every tick would pass.
func TestListPendingTaxReversal_AgedOrphanIsNotStranded(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	store := invoice.NewPostgresStore(db)
	// Two tenants because seedDraftInvoice pins one external_id per tenant.
	// The sweep scans cross-tenant (TxBypass), so both rows are in range.
	strandedTenant := testutil.CreateTestTenant(t, db, "Tax Reversal Aged Stranded")
	settledTenant := testutil.CreateTestTenant(t, db, "Tax Reversal Aged Settled")

	strandedID := seedDraftInvoice(t, db, strandedTenant)
	settledID := seedDraftInvoice(t, db, settledTenant)

	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin seed tx: %v", err)
	}
	// Both voided, both stripe_tax, both committed, both far outside the old
	// 24h window. They differ ONLY in whether the reversal was confirmed.
	if _, err := tx.ExecContext(ctx, `
		UPDATE invoices
		   SET status = 'voided', voided_at = now() - interval '40 days',
		       tax_provider = 'stripe_tax', tax_status = 'ok',
		       tax_transaction_id = 'tx_aged',
		       total_amount_cents = 4669, amount_due_cents = 4669,
		       tax_reversed_at = NULL,
		       updated_at = now() - interval '40 days'
		 WHERE id = $1`, strandedID); err != nil {
		t.Fatalf("seed stranded invoice: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE invoices
		   SET status = 'voided', voided_at = now() - interval '40 days',
		       tax_provider = 'stripe_tax', tax_status = 'ok',
		       tax_transaction_id = 'tx_aged_done',
		       total_amount_cents = 4669, amount_due_cents = 4669,
		       tax_reversed_at = now() - interval '40 days',
		       updated_at = now() - interval '40 days'
		 WHERE id = $1`, settledID); err != nil {
		t.Fatalf("seed settled invoice: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit seed: %v", err)
	}

	pending, err := store.ListPendingTaxReversal(ctx, 200, false)
	if err != nil {
		t.Fatalf("ListPendingTaxReversal: %v", err)
	}
	var sawStranded, sawSettled bool
	for _, inv := range pending {
		switch inv.ID {
		case strandedID:
			sawStranded = true
		case settledID:
			sawSettled = true
		}
	}
	if !sawStranded {
		t.Error("a 40-day-old unreversed void was not swept — it is over-remitted tax with no recovery path, and nothing else in the product names it")
	}
	if sawSettled {
		t.Error("a 40-day-old ALREADY-REVERSED void was swept — the fix must widen the age bound, not drop the tax_reversed_at guard, or every tick re-reverses settled invoices")
	}
}
