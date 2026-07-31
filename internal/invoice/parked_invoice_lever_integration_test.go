package invoice_test

import (
	"context"
	"strings"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestParkedInvoiceHasExactlyOneWayOut is the liveness half of ADR-107.
//
// The safety half — no charge path accepts payment_status='unknown' — is what
// makes a parked invoice safe. On its own it also made the invoice a DEAD END:
// void, mark-uncollectible and record-offline-payment all refuse an in-flight
// payment, and 'unknown' is in-flight, so the complete set of state-changing
// operator actions was EMPTY. An invoice that can reach no terminal state
// violates the every-invoice-terminates rule, and the CRITICAL log was
// instructing operators to "settle or void it by hand" — both refused.
//
// Mark-uncollectible is the one exit that is safe here: it moves no money, and
// if the charge did succeed the provider webhook still marks the invoice paid
// through the ordinary recovery path.
func TestParkedInvoiceHasExactlyOneWayOut(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Parked Lever")
	store := invoice.NewPostgresStore(db)
	svc := invoice.NewService(store, nil, nil)

	park := func(num string) domain.Invoice {
		inv := seedClaimableInvoice(t, db, ctx, tenantID, num)
		if _, err := store.UpdatePayment(ctx, tenantID, inv.ID,
			domain.PaymentUnknown, "", "ambiguous: no payment_intent_id", nil); err != nil {
			t.Fatalf("park: %v", err)
		}
		return inv
	}

	t.Run("mark uncollectible is allowed — it is the way out", func(t *testing.T) {
		inv := park("INV-PARK-UNCOLL")
		got, err := svc.MarkUncollectible(ctx, tenantID, inv.ID)
		if err != nil {
			t.Fatalf("a parked invoice must be write-off-able, else it can reach no terminal state at all: %v", err)
		}
		if got.Status != domain.InvoiceUncollectible {
			t.Fatalf("status = %s, want uncollectible", got.Status)
		}
	})

	t.Run("void stays refused, and says why honestly", func(t *testing.T) {
		inv := park("INV-PARK-VOID")
		_, err := svc.Void(ctx, tenantID, inv.ID)
		if err == nil {
			t.Fatal("voiding an invoice whose charge may have succeeded creates a paid-and-voided contradiction")
		}
		// The old copy said "wait for it to settle or cancel it" — two things
		// that cannot happen for a parked invoice.
		if strings.Contains(err.Error(), "wait for it to settle") {
			t.Errorf("the refusal still tells the operator to wait for a settlement that will never come: %v", err)
		}
		if !strings.Contains(strings.ToLower(err.Error()), "uncollectible") {
			t.Errorf("the refusal does not point at the action that IS available: %v", err)
		}
	})

	t.Run("a genuinely in-flight charge still refuses the write-off", func(t *testing.T) {
		// Negative control. If 'processing' could also be written off, the
		// carve-out would be a hole rather than an exit: that charge really is
		// resolving, and writing it off races the settle.
		inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-PARK-PROC")
		if _, err := store.UpdatePayment(ctx, tenantID, inv.ID,
			domain.PaymentProcessing, "pi_live_1", "", nil); err != nil {
			t.Fatalf("set processing: %v", err)
		}
		if _, err := svc.MarkUncollectible(ctx, tenantID, inv.ID); err == nil {
			t.Fatal("a charge that is genuinely in flight must NOT be write-off-able — it is about to settle")
		}
	})
}
