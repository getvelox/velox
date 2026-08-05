package invoice_test

import (
	"context"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// mkParked seeds a finalized invoice in the ADR-107 parked shape and returns
// its id. age backdates updated_at (the park timestamp — parked rows are never
// written again, so updated_at doubles as park age); synced, when non-zero,
// backdates provider_synced_at (the search cool-off cursor).
func mkParked(t *testing.T, ctx context.Context, db *postgres.DB, store *invoice.PostgresStore, tenantID, custID, num string, age time.Duration, synced time.Duration) string {
	t.Helper()
	now := time.Now().UTC()
	inv, err := store.Create(ctx, tenantID, domain.Invoice{
		CustomerID: custID, InvoiceNumber: num, Status: domain.InvoiceFinalized,
		PaymentStatus: domain.PaymentUnknown, Currency: "USD",
		SubtotalCents: 1000, TotalAmountCents: 1000, AmountDueCents: 1000,
		BillingPeriodStart: now.Add(-24 * time.Hour), BillingPeriodEnd: now, IssuedAt: &now,
	})
	if err != nil {
		t.Fatalf("create %s: %v", num, err)
	}
	tx, err := db.BeginTx(context.Background(), postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(context.Background(),
		`UPDATE invoices SET updated_at = now() - make_interval(secs => $1) WHERE id = $2`,
		age.Seconds(), inv.ID); err != nil {
		t.Fatalf("age %s: %v", num, err)
	}
	if synced > 0 {
		if _, err := tx.ExecContext(context.Background(),
			`UPDATE invoices SET provider_synced_at = now() - make_interval(secs => $1), provider_payment_status = 'search_not_found' WHERE id = $2`,
			synced.Seconds(), inv.ID); err != nil {
			t.Fatalf("stamp sync %s: %v", num, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return inv.ID
}

// TestListParkedSearchable_Predicate pins all four predicate clauses of the
// ADR-108 sweep input, each an attack-round finding. One test, one row per
// clause, so a predicate edit that drops a clause fails here naming it.
func TestListParkedSearchable_Predicate(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Parked Search Pred")
	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{ExternalID: "cus_ps", DisplayName: "PS"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	store := invoice.NewPostgresStore(db)

	eligible := mkParked(t, ctx, db, store, tenantID, cust.ID, "INV-ELIGIBLE", 2*time.Hour, 0)
	tooYoung := mkParked(t, ctx, db, store, tenantID, cust.ID, "INV-YOUNG", 5*time.Minute, 0)
	recentlySearched := mkParked(t, ctx, db, store, tenantID, cust.ID, "INV-RECENT-SEARCH", 2*time.Hour, 10*time.Minute)
	searchedLongAgo := mkParked(t, ctx, db, store, tenantID, cust.ID, "INV-OLD-SEARCH", 2*time.Hour, 2*time.Hour)

	// Wrong mode: flip the eligible shape's livemode directly.
	wrongMode := mkParked(t, ctx, db, store, tenantID, cust.ID, "INV-WRONG-MODE", 2*time.Hour, 0)
	tx, _ := db.BeginTx(context.Background(), postgres.TxBypass, "")
	if _, err := tx.ExecContext(context.Background(), `UPDATE invoices SET livemode = true WHERE id = $1`, wrongMode); err != nil {
		t.Fatalf("flip mode: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	got, err := store.ListParkedSearchable(ctx, time.Now().UTC().Add(-time.Hour), 50)
	if err != nil {
		t.Fatalf("ListParkedSearchable: %v", err)
	}
	byID := map[string]bool{}
	for _, inv := range got {
		byID[inv.ID] = true
	}

	if !byID[eligible] {
		t.Error("the eligible parked row (old, never searched, right mode) was not listed")
	}
	if !byID[searchedLongAgo] {
		t.Error("a row searched 2h ago was not re-listed — the cool-off must expire, or every parked row is searched exactly once and then never again")
	}
	if byID[tooYoung] {
		t.Error("a row parked 5 minutes ago was listed — the 1h age gate exists so the webhook wins the common race and no search is spent on a PI that could not be indexed yet")
	}
	if byID[recentlySearched] {
		t.Error("a row searched 10 minutes ago was re-listed — the cool-off IS the rate pacing; without it the sweep hammers the same row every tick")
	}
	if byID[wrongMode] {
		t.Error("a live-mode row was listed under a test-mode sweep — the #13 bug class; a mode-mismatched search is empty with probability 1 and burns the 20/s budget for nothing")
	}

	// Rotation: never-searched rows come before previously-searched ones.
	if len(got) >= 2 && got[0].ID != eligible {
		t.Errorf("ordering: first listed = %s, want the never-searched row (provider_synced_at NULLS FIRST)", got[0].InvoiceNumber)
	}
}

// TestAdoptPaymentIntentIfParked_CAS pins the adopt write both ways: it stamps
// exactly the parked shape (moving it to processing WITH an id, bumping the seq
// once), and it refuses — zero rows, no error — once ANY leg of the shape has
// moved on, so a webhook-settled invoice can never be regressed to in-flight
// by a slow sweep (attack-verified: the webhook racing the sweep is the COMMON
// case, both fire when the provider recovers).
func TestAdoptPaymentIntentIfParked_CAS(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Parked Adopt CAS")
	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{ExternalID: "cus_cas", DisplayName: "CAS"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	store := invoice.NewPostgresStore(db)

	t.Run("adopts the parked shape and bumps the seq once", func(t *testing.T) {
		id := mkParked(t, ctx, db, store, tenantID, cust.ID, "INV-CAS-1", 2*time.Hour, 0)
		before, _ := store.Get(ctx, tenantID, id)

		won, err := store.AdoptPaymentIntentIfParked(ctx, tenantID, id, "pi_found")
		if err != nil {
			t.Fatalf("adopt: %v", err)
		}
		if !won {
			t.Fatal("CAS lost on a genuinely parked row")
		}
		after, _ := store.Get(ctx, tenantID, id)
		if after.PaymentStatus != domain.PaymentProcessing || after.StripePaymentIntentID != "pi_found" {
			t.Errorf("row = %s/%q, want processing/pi_found", after.PaymentStatus, after.StripePaymentIntentID)
		}
		if after.ChargeAttemptSeq != before.ChargeAttemptSeq+1 {
			t.Errorf("seq %d -> %d, want +1 exactly — adopting a named PI records an attempt (ADR-105)", before.ChargeAttemptSeq, after.ChargeAttemptSeq)
		}
	})

	t.Run("refuses once the webhook settled the row", func(t *testing.T) {
		id := mkParked(t, ctx, db, store, tenantID, cust.ID, "INV-CAS-2", 2*time.Hour, 0)
		// Webhook wins: settle the invoice paid under a different PI.
		if _, err := store.UpdatePayment(ctx, tenantID, id, domain.PaymentSucceeded, "pi_webhook", "", nil); err != nil {
			t.Fatalf("simulate webhook settle: %v", err)
		}
		won, err := store.AdoptPaymentIntentIfParked(ctx, tenantID, id, "pi_search")
		if err != nil {
			t.Fatalf("adopt after settle must not error (losing is the designed outcome): %v", err)
		}
		if won {
			t.Fatal("CAS won against a settled row — a slow sweep just regressed a paid invoice to in-flight")
		}
		after, _ := store.Get(ctx, tenantID, id)
		if after.StripePaymentIntentID != "pi_webhook" {
			t.Errorf("winner's PI overwritten: %q", after.StripePaymentIntentID)
		}
	})

	t.Run("refuses an empty PI id outright", func(t *testing.T) {
		id := mkParked(t, ctx, db, store, tenantID, cust.ID, "INV-CAS-3", 2*time.Hour, 0)
		if _, err := store.AdoptPaymentIntentIfParked(ctx, tenantID, id, ""); err == nil {
			t.Fatal("adopting an EMPTY id must error — it would re-park the row while bumping the seq, silently rotating the key")
		}
	})
}

// TestParkedSweep_WrittenOffRowsStayVisible pins the scan-exclusion sink closed
// (2026-08-05).
//
// Writing a parked invoice off is the exit the product ADVISES — the attention
// banner tells the operator to "mark this invoice uncollectible to close it
// out". Taking that advice used to drop the row out of BOTH the ADR-108 search
// (`status = 'finalized'`) and the gauge, so the one sweep that could ever name
// its PaymentIntent stopped looking, silently, at the exact moment a human
// acted. Money that may or may not have moved became permanently unaccounted.
//
// Both directions in one test on purpose: the written-off row must be visible,
// AND the finalized row must still be, so a predicate that simply swapped one
// status for the other fails here.
func TestParkedSweep_WrittenOffRowsStayVisible(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Parked WriteOff Sink")
	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{ExternalID: "cus_wo", DisplayName: "WO"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	store := invoice.NewPostgresStore(db)

	stillOpen := mkParked(t, ctx, db, store, tenantID, cust.ID, "INV-WO-OPEN", 2*time.Hour, 0)
	writtenOff := mkParked(t, ctx, db, store, tenantID, cust.ID, "INV-WO-OFF", 2*time.Hour, 0)

	// Write it off the way the product does — status only. payment_status stays
	// 'unknown' deliberately (ADR-107: a write-off closes the INVOICE, it does
	// not answer whether the card was charged).
	tx, _ := db.BeginTx(context.Background(), postgres.TxBypass, "")
	if _, err := tx.ExecContext(context.Background(),
		`UPDATE invoices SET status = 'uncollectible' WHERE id = $1`, writtenOff); err != nil {
		t.Fatalf("write off: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// 1. The search sweep still sees BOTH.
	rows, err := store.ListParkedSearchable(ctx, time.Now().UTC().Add(-time.Hour), 50)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	seen := map[string]bool{}
	for _, r := range rows {
		seen[r.ID] = true
	}
	if !seen[writtenOff] {
		t.Error("a WRITTEN-OFF parked invoice is not searchable — the sink is still open: writing it off stops the only sweep that could name its PaymentIntent")
	}
	if !seen[stillOpen] {
		t.Error("regression: the finalized parked invoice is no longer searchable")
	}

	// 2. The gauge splits them rather than summing — an already-decided row must
	//    not read as an unmade decision.
	open, off, err := store.CountParkedInvoices(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if open != 1 {
		t.Errorf("open parked count = %d, want 1", open)
	}
	if off != 1 {
		t.Errorf("written-off parked count = %d, want 1 — the write-off disposition is invisible to the gauge", off)
	}

	// 3. Adoption works on the written-off row, which is the whole point: the
	//    search found the PaymentIntent, so the invoice can rejoin the ordinary
	//    reconciler and settle uncollectible -> paid.
	adopted, err := store.AdoptPaymentIntentIfParked(ctx, tenantID, writtenOff, "pi_found_after_writeoff")
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if !adopted {
		t.Fatal("a written-off parked invoice could not adopt a found PaymentIntent — it can never resolve")
	}
	got, err := store.Get(ctx, tenantID, writtenOff)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != domain.InvoiceUncollectible {
		t.Errorf("adoption changed status to %q — it must leave the write-off standing; only payment_status moves", got.Status)
	}
	if got.PaymentStatus != domain.PaymentProcessing {
		t.Errorf("payment_status = %q, want processing", got.PaymentStatus)
	}
}
