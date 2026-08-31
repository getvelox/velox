package invoice_test

import (
	"context"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestTerminalTransitions_ClearAutoChargePending locks the 2026-07-20
// FLOW B4 residual fix: auto_charge_pending is a WORK flag, and every
// terminal transition retires it. Before this, only the auto-charge
// success path cleared it — an offline-paid, operator-collected, voided,
// or uncollectible invoice kept answering auto_charge_pending=true in
// the API (a lie: nothing is pending). Operationally inert either way
// (every charge predicate gates on finalized+pending), so this is a
// truth fix — asserted at the store choke points so every settle
// variant inherits it.
func TestTerminalTransitions_ClearAutoChargePending(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "ACP Terminal")
	store := invoice.NewPostgresStore(db)

	acp := func(id string) bool {
		t.Helper()
		inv, err := store.Get(ctx, tenantID, id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		return inv.AutoChargePending
	}

	t.Run("card settle clears it (every MarkPaid variant converges here)", func(t *testing.T) {
		inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-ACP-SETTLE")
		if _, _, err := store.MarkPaidCardSettlementTransition(ctx, tenantID, inv.ID, "pi_acp_settle", time.Now().UTC(), nil); err != nil {
			t.Fatalf("settle: %v", err)
		}
		if acp(inv.ID) {
			t.Error("paid invoice must not answer auto_charge_pending=true")
		}
	})

	t.Run("void clears it", func(t *testing.T) {
		inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-ACP-VOID")
		if _, err := store.UpdateStatus(ctx, tenantID, inv.ID, domain.InvoiceVoided); err != nil {
			t.Fatalf("void: %v", err)
		}
		if acp(inv.ID) {
			t.Error("voided invoice must not answer auto_charge_pending=true")
		}
	})

	t.Run("uncollectible clears it", func(t *testing.T) {
		inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-ACP-UNCOLL")
		if _, err := store.UpdateStatus(ctx, tenantID, inv.ID, domain.InvoiceUncollectible); err != nil {
			t.Fatalf("uncollectible: %v", err)
		}
		if acp(inv.ID) {
			t.Error("uncollectible invoice must not answer auto_charge_pending=true")
		}
	})

	t.Run("UpdatePayment succeeded clears it; failed leaves it", func(t *testing.T) {
		inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-ACP-UPDPAY")
		if _, err := store.UpdatePayment(ctx, tenantID, inv.ID, domain.PaymentFailed, "pi_acp_f", "card_declined", nil); err != nil {
			t.Fatalf("update failed: %v", err)
		}
		if !acp(inv.ID) {
			t.Error("a FAILED payment must keep the flag — the invoice is still on the charge track (dunning/collect)")
		}
		paidAt := time.Now().UTC()
		if _, err := store.UpdatePayment(ctx, tenantID, inv.ID, domain.PaymentSucceeded, "pi_acp_s", "", &paidAt); err != nil {
			t.Fatalf("update succeeded: %v", err)
		}
		if acp(inv.ID) {
			t.Error("a succeeded payment must retire the flag")
		}
	})
}
