package invoice_test

import (
	"context"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
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
	return payment.ChargeIdempotencyKey(inv.ID, inv.ChargeAttemptSeq, "")
}

// TestChargeKey_StableUntilAnOutcomeIsRecorded is requirement R1: a charge that
// reached Stripe but whose outcome was never written must retry under the SAME
// key, so Stripe returns the in-flight PaymentIntent instead of opening a
// second one and charging the customer twice.
//
// The walk interleaves the crashed-leader lease sequence with the writers that
// used to re-seed the key. SetTaxTransaction is the load-bearing case — it is
// tax_commit's own stamp, and tax_commit runs immediately BEFORE the auto-charge
// sweep in the same scheduler tick, so it is the writer most likely to land
// between a crashed attempt and its retry. Under the old updated_at seed these
// assertions FAILED; that failure was the bug (#677 characterised it, #678
// fixed it).
//
// Mutation check: re-seed the key from inv.UpdatedAt and the tax-stamp
// assertion fails; add `updated_at = now()` to ClaimAutoCharge and nothing
// fails, which is the point — the seq seed is immune to it by construction.
func TestChargeKey_StableUntilAnOutcomeIsRecorded(t *testing.T) {
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
				"attempt's retry would now mint a SECOND PaymentIntent", step, got, want)
		}
	}

	// The crashed-leader sequence: a leader claims, dies mid-charge, the lease
	// ages out, the next sweep re-claims. (updated_at stability across the lease
	// is TestClaimAutoCharge_UpdatedAtStable's job — asserted here only because
	// it is the sequence the unrelated writes below interleave with.)
	if ok, _ := store.ClaimAutoCharge(ctx, tenantID, inv.ID); !ok {
		t.Fatal("first claim must succeed")
	}
	expireLease(t, db, inv.ID)
	if ok, _ := store.ClaimAutoCharge(ctx, tenantID, inv.ID); !ok {
		t.Fatal("claim after lease expiry must succeed")
	}
	assertKeyUnchanged("re-claiming after a stalled leader")

	// Unrelated writers on the same row — the drift that used to mint a second
	// PaymentIntent. tax_commit's stamp runs immediately before the auto-charge
	// sweep in the same tick.
	if err := store.SetTaxTransaction(ctx, tenantID, inv.ID, "tax_txn_drift"); err != nil {
		t.Fatalf("set tax transaction: %v", err)
	}
	assertKeyUnchanged("tax_commit stamping tax_transaction_id")

	if err := store.SetNoPMNotifiedAt(ctx, tenantID, inv.ID, time.Now().UTC()); err != nil {
		t.Fatalf("set no-PM notified: %v", err)
	}
	assertKeyUnchanged("the sweep stamping its no-PM email marker")
}

// TestChargeKey_MovesOnEveryRecordedOutcome is requirement R2, the opposite
// failure: once a decline is RECORDED, the retry must present a fresh key. Miss
// this and Stripe replays the declined PaymentIntent for every future attempt —
// the customer fixes their card and still cannot pay, with no error raised
// anywhere (playbook class G, liveness sink).
//
// Both recorded-decline writers are exercised, because they are separate SQL
// statements and a fix applied to only one is the class-C2 shape this test
// exists to catch.
func TestChargeKey_MovesOnEveryRecordedOutcome(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Key Advance")
	store := invoice.NewPostgresStore(db)

	t.Run("UpdatePayment", func(t *testing.T) {
		inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-KEY-ADV-1")
		before := chargeKey(t, store, ctx, tenantID, inv.ID)

		if _, err := store.UpdatePayment(ctx, tenantID, inv.ID,
			domain.PaymentFailed, "pi_declined_1", "card_declined", nil); err != nil {
			t.Fatalf("update payment: %v", err)
		}

		if after := chargeKey(t, store, ctx, tenantID, inv.ID); after == before {
			t.Fatalf("a recorded decline left the key at %q — the retry would be handed "+
				"Stripe's DECLINED PaymentIntent and the invoice could never be paid", before)
		}
	})

	t.Run("MarkPaymentFailedReportingTransition", func(t *testing.T) {
		inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-KEY-ADV-2")
		before := chargeKey(t, store, ctx, tenantID, inv.ID)

		if _, _, err := store.MarkPaymentFailedReportingTransition(ctx, tenantID, inv.ID,
			"pi_declined_2", "card_declined", nil); err != nil {
			t.Fatalf("mark payment failed: %v", err)
		}

		if after := chargeKey(t, store, ctx, tenantID, inv.ID); after == before {
			t.Fatalf("a recorded decline left the key at %q — the retry would be handed "+
				"Stripe's DECLINED PaymentIntent and the invoice could never be paid", before)
		}
	})
}
