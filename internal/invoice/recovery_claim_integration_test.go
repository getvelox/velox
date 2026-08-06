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

// ADR-113: NOTHING initiates a card charge against a written-off invoice.
// Charge-in-place recovery (shipped briefly 2026-08-05, ADR-110 amendment) was
// removed on industry evidence — 1 of 6 verified platforms (Stripe alone)
// charges the written-off object; recovery runs on NORMAL rails via a fresh
// recovery invoice. These tests pin the refusal in REAL Postgres, because the
// refusal is a SQL predicate in the claim CAS and a fake would only prove the
// fake.

// seedWrittenOff creates a written-off invoice in the recoverable shape, then
// applies `mutate` (raw SQL) so each test can set exactly the one column its
// gate keys on.
func seedWrittenOff(t *testing.T, db *postgres.DB, ctx context.Context, tenantID, num, mutate string) domain.Invoice {
	t.Helper()
	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_" + num, DisplayName: "Recovery " + num,
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	store := invoice.NewPostgresStore(db)
	inv, err := store.Create(ctx, tenantID, domain.Invoice{
		CustomerID: cust.ID, InvoiceNumber: num,
		Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentFailed,
		Currency: "USD", SubtotalCents: 5000, TotalAmountCents: 5000, AmountDueCents: 5000,
		BillingPeriodStart: time.Now().UTC().Add(-30 * 24 * time.Hour),
		BillingPeriodEnd:   time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}
	tx, err := db.BeginTx(context.Background(), postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(context.Background(),
		`UPDATE invoices SET status='uncollectible', uncollectible_at=now() `+mutate+` WHERE id=$1`, inv.ID); err != nil {
		t.Fatalf("write off %s: %v", num, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return inv
}

// TestRecoveryClaim_OperatorMayMachineMayNot is THE safety boundary of this
// feature, and the reason ClaimChargeForManualCollect stopped sharing
// claimChargeLease.
//
// An operator may charge a written-off invoice — the customer came back. No
// MACHINE may: dunning re-charging a debt the business formally gave up on is
// the failure this whole design exists to avoid. Both directions in one test,
// so collapsing the two claims back onto one shared lease fails here.
func TestManualCollectClaim_RefusesWrittenOff(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 20*time.Second)
	defer cancel()
	tenantID := testutil.CreateTestTenant(t, db, "ADR113 Refusal")
	store := invoice.NewPostgresStore(db)

	// The DISCRIMINATING fixture: this invoice is in the exact shape the
	// removed feature admitted (uncollectible, payment failed, amount due,
	// no gate condition set). If the claim ever re-admits uncollectible,
	// this is the row that slips through.
	writtenOff := seedWrittenOff(t, db, ctx, tenantID, "INV-113-A", "")
	if got, err := store.ClaimChargeForManualCollect(ctx, tenantID, writtenOff.ID); err != nil {
		t.Fatalf("claim: %v", err)
	} else if got {
		t.Fatal("ClaimChargeForManualCollect admitted a WRITTEN-OFF invoice — ADR-113 removed charge-in-place recovery; a written-off invoice is settled by recording writers only")
	}

	// Positive control: the same claim still serves its actual job — an
	// operator retrying a FINALIZED invoice whose charge declined. Without
	// this arm, a claim that refuses everything would pass. (Seeded written
	// off, then flipped back by raw SQL — seedWrittenOff always sets
	// status, so the flip must be a second statement, not a mutate.)
	finalized := seedWrittenOff(t, db, ctx, tenantID, "INV-113-B", "")
	{
		tx, err := db.BeginTx(context.Background(), postgres.TxBypass, "")
		if err != nil {
			t.Fatalf("begin flip tx: %v", err)
		}
		defer postgres.Rollback(tx)
		if _, err := tx.ExecContext(context.Background(),
			`UPDATE invoices SET status='finalized', uncollectible_at=NULL WHERE id=$1`, finalized.ID); err != nil {
			t.Fatalf("flip control to finalized: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit flip: %v", err)
		}
	}
	if got, err := store.ClaimChargeForManualCollect(ctx, tenantID, finalized.ID); err != nil {
		t.Fatalf("claim finalized: %v", err)
	} else if !got {
		t.Fatal("claim refused a finalized/failed invoice — the operator retry path must survive the ADR-113 narrowing")
	}
}

// TestManualCollectClaim_RefusalIsStatusNotGates: the three recovery gates
// (tax reversed / threshold / relief) were DELETED, not folded into the
// refusal. A written-off invoice with NONE of those conditions must still be
// refused — the refusal keys on status alone. Guards against a future
// "re-admit uncollectible but keep the gates" half-revert.
func TestManualCollectClaim_RefusalIsStatusNotGates(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 20*time.Second)
	defer cancel()
	tenantID := testutil.CreateTestTenant(t, db, "ADR113 Status Only")
	store := invoice.NewPostgresStore(db)

	for _, tc := range []struct{ num, mutate string }{
		{"INV-113-C", ""},                             // pristine recoverable shape
		{"INV-113-D", ", tax_reversed_at=now()"},      // old gate 1 condition
		{"INV-113-E", ", billing_reason='threshold'"}, // old gate 2 condition
		{"INV-113-F", ", payment_status='unknown'"},   // parked
	} {
		inv := seedWrittenOff(t, db, ctx, tenantID, tc.num, tc.mutate)
		if got, err := store.ClaimChargeForManualCollect(ctx, tenantID, inv.ID); err != nil {
			t.Fatalf("%s: %v", tc.num, err)
		} else if got {
			t.Errorf("%s (mutate %q): claim admitted a written-off invoice", tc.num, tc.mutate)
		}
	}
}
