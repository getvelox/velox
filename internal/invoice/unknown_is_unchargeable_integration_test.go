package invoice_test

import (
	"context"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestUnknownPaymentIsUnchargeableByEveryClaimPath is the mechanised form of
// ADR-107's entire guarantee, and it is the reason that ADR can be one deletion
// instead of a subsystem.
//
// An ambiguous charge can leave a PaymentIntent live at Stripe that Velox never
// learned the name of. Rather than settle such an invoice failed — which bumps
// charge_attempt_seq, rotating the idempotency key, and returns the invoice to
// a claimable state so the next retry opens a SECOND PaymentIntent — it is left
// at payment_status='unknown' forever.
//
// That is only safe while NO charge path will claim an 'unknown' invoice. Today
// all three predicates exclude it, but they are three separate SQL statements in
// three separate functions: a fourth charge path, or one predicate widened to
// `payment_status <> 'succeeded'`, silently reopens a double-charge hole with
// nothing failing. This test is what fails instead.
//
// Mutation check: add 'unknown' to any one of the three predicates in
// internal/invoice/postgres.go and this test fails naming that path.
func TestUnknownPaymentIsUnchargeableByEveryClaimPath(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Unknown Unchargeable")
	store := invoice.NewPostgresStore(db)

	claims := []struct {
		name string
		// what this path is, in operator terms, so a failure explains itself
		what  string
		claim func(inv domain.Invoice) (bool, error)
	}{
		{
			name: "ClaimAutoCharge",
			what: "the background auto-charge sweep",
			claim: func(inv domain.Invoice) (bool, error) {
				return store.ClaimAutoCharge(ctx, tenantID, inv.ID)
			},
		},
		{
			name: "ClaimChargeForManualCollect",
			what: "an operator pressing Collect payment",
			claim: func(inv domain.Invoice) (bool, error) {
				return store.ClaimChargeForManualCollect(ctx, tenantID, inv.ID)
			},
		},
		{
			name: "ClaimChargeForDunningRetry",
			what: "a dunning retry",
			claim: func(inv domain.Invoice) (bool, error) {
				return store.ClaimChargeForDunningRetry(ctx, tenantID, inv.ID)
			},
		},
	}

	for _, c := range claims {
		t.Run(c.name, func(t *testing.T) {
			inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-UNK-"+c.name)

			// Control: while the invoice is claimable the path DOES claim it.
			// Without this the test could pass against a predicate that refuses
			// everything, which would prove nothing.
			ok, err := c.claim(inv)
			if err != nil {
				t.Fatalf("control claim: %v", err)
			}
			if !ok {
				t.Fatalf("%s refused a claimable invoice — the negative result below would be meaningless", c.name)
			}
			releaseClaim(t, db, inv.ID)

			// Treatment: an ambiguous outcome with no PaymentIntent id.
			if _, err := store.UpdatePayment(ctx, tenantID, inv.ID,
				domain.PaymentUnknown, "", "ambiguous: no payment_intent_id", nil); err != nil {
				t.Fatalf("set unknown: %v", err)
			}

			ok, err = c.claim(inv)
			if err != nil {
				t.Fatalf("treatment claim: %v", err)
			}
			if ok {
				t.Fatalf("%s (%s) CLAIMED an invoice at payment_status='unknown'. "+
					"A PaymentIntent may be live at Stripe for it, so this charge would be the customer's SECOND. "+
					"ADR-107's guarantee is that no claim path admits 'unknown' — if this path must change, the "+
					"guarantee needs replacing, not the assertion.", c.name, c.what)
			}
		})
	}
}

// releaseClaim clears the shared charge lease so the next claim in the table is
// testing the payment_status predicate rather than the lease.
func releaseClaim(t *testing.T, db *postgres.DB, invoiceID string) {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.Exec(
		`UPDATE invoices SET auto_charge_claimed_until = NULL WHERE id = $1`, invoiceID); err != nil {
		t.Fatalf("release lease: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}
