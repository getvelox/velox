package invoice_test

import (
	"context"
	"testing"

	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/payment"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// chargeKey derives the Stripe idempotency key the way production does — from
// payment.ChargeIdempotencyKey, never by re-typing the format here. A test that
// hardcodes the shape keeps passing after the real key changes.
func chargeKey(t *testing.T, store *invoice.PostgresStore, ctx context.Context, tenantID, id string) string {
	t.Helper()
	inv, err := store.Get(ctx, tenantID, id)
	if err != nil {
		t.Fatalf("get invoice: %v", err)
	}
	return payment.ChargeIdempotencyKey(inv.ID, inv.UpdatedAt, "")
}

// TestAutoChargeLease_NeverTouchesUpdatedAt pins the load-bearing claim in
// ClaimAutoCharge's own comment: "updated_at is NOT touched: the Stripe
// idempotency key derives from it, and key stability across claim windows is
// what makes a re-claimed retry after a stalled leader converge on the SAME
// PaymentIntent instead of minting a second."
//
// The property was declared and relied upon, but nothing tested it, so a lease
// write that added `updated_at = now()` — the natural thing to type — would
// have shipped green while quietly converting every crashed-leader retry into
// a second charge on the customer's card.
//
// Mutation check: add `, updated_at = now()` to ClaimAutoCharge's UPDATE and
// this test fails on the first assertion.
func TestAutoChargeLease_NeverTouchesUpdatedAt(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Key Stability")
	inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-KEY-STABLE")
	store := invoice.NewPostgresStore(db)

	want := chargeKey(t, store, ctx, tenantID, inv.ID)

	assertKeyUnchanged := func(step string) {
		t.Helper()
		if got := chargeKey(t, store, ctx, tenantID, inv.ID); got != want {
			t.Fatalf("%s re-seeded the idempotency key: got %q, want %q — a crashed "+
				"leader's retry would now mint a SECOND PaymentIntent", step, got, want)
		}
	}

	if ok, _ := store.ClaimAutoCharge(ctx, tenantID, inv.ID); !ok {
		t.Fatal("first claim must succeed")
	}
	assertKeyUnchanged("taking the lease")

	if ok, _ := store.ClaimAutoCharge(ctx, tenantID, inv.ID); ok {
		t.Fatal("second claim inside the lease must fail")
	}
	assertKeyUnchanged("a refused claim")

	// The crashed-leader path: the lease ages out and the next sweep re-claims.
	// This is the exact sequence the stable key exists to serve — the first
	// attempt may already have reached Stripe.
	expireLease(t, db, inv.ID)
	if ok, _ := store.ClaimAutoCharge(ctx, tenantID, inv.ID); !ok {
		t.Fatal("claim after lease expiry must succeed")
	}
	assertKeyUnchanged("re-claiming after a stalled leader")

	if err := store.ReleaseAutoChargeClaim(ctx, tenantID, inv.ID); err != nil {
		t.Fatalf("release: %v", err)
	}
	assertKeyUnchanged("releasing the lease")
}

// TestAutoChargeKey_ReSeededByAnUnrelatedInvoiceWrite is a CHARACTERISATION of
// the residual exposure, not a blessing of it. The lease is drift-proof by
// construction (test above), but the key seed is the invoice's updated_at, so
// ANY other writer touching the row inside the crash window re-seeds it.
//
// SetTaxTransaction is not a hypothetical writer: it is the tax_commit
// reconciler's own stamp, and tax_commit runs immediately BEFORE the
// auto-charge sweep in the same scheduler tick (billing/scheduler.go). So the
// sequence is ordinary, not exotic:
//
//	tick N    auto-charge calls Stripe, PI_A created, process dies pre-stamp
//	          (invoice still payment_status='pending', updated_at unchanged)
//	tick N+1  tax_commit finally lands its transaction id  → updated_at bumps
//	          auto-charge retries with a DIFFERENT key      → PI_B created
//
// Both PIs can settle. The webhook for PI_A normally closes the window first
// (it moves payment_status out of 'pending' and the CAS then refuses the
// claim), so this needs a lost or >5-minute-delayed webhook to bite — narrow,
// but the guard is conditional, and the code comments claimed it as absolute.
//
// If this test ever FAILS because the key stopped depending on updated_at, the
// exposure is closed — delete the test rather than repairing it.
func TestAutoChargeKey_ReSeededByAnUnrelatedInvoiceWrite(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Key Drift")
	inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-KEY-DRIFT")
	store := invoice.NewPostgresStore(db)

	before := chargeKey(t, store, ctx, tenantID, inv.ID)

	if err := store.SetTaxTransaction(ctx, tenantID, inv.ID, "tax_txn_drift"); err != nil {
		t.Fatalf("set tax transaction: %v", err)
	}

	after := chargeKey(t, store, ctx, tenantID, inv.ID)
	if after == before {
		t.Skip("key no longer derives from updated_at — the drift exposure is closed; delete this test")
	}
	t.Logf("documented exposure: an unrelated invoice write re-seeded the charge key (%s → %s)", before, after)
}
