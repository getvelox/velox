package billing

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// countingCharger records per-invoice charge invocations — the
// exactly-once probe for the claim tests.
type countingCharger struct {
	mu    sync.Mutex
	calls map[string]int
}

func (c *countingCharger) ChargeInvoice(_ context.Context, _ string, inv domain.Invoice, _, _ string) (domain.Invoice, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.calls == nil {
		c.calls = map[string]int{}
	}
	c.calls[inv.ID]++
	inv.PaymentStatus = domain.PaymentSucceeded
	return inv, nil
}

// TestProcessAutoCharge_DualLeaderChargesOnce reproduces the audited
// dual-leader interleaving (HA hazard #1) deterministically: BOTH
// leaders list the same candidate set (lists are claim-blind), then run
// the charge loop over it. The per-invoice CAS claim must admit exactly
// one into the charge leg — pre-fix, leader B re-charged with a
// divergent Stripe idempotency key minted from A's outcome bump.
func TestProcessAutoCharge_DualLeaderChargesOnce(t *testing.T) {
	inv := &mockInvoices{
		invoices: []domain.Invoice{{
			ID: "inv_dual", TenantID: "t1", CustomerID: "cus_1",
			Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending,
			AutoChargePending: true, AmountDueCents: 1000,
		}},
	}
	pms := &fakePaymentSetups{ready: true, stripeCustomerID: "cus_stripe_1"}
	charger := &countingCharger{}
	engine := wireBaseTax(NewEngine(&mockSubs{cycleUpdated: make(map[string]bool)}, &mockUsage{}, &mockPricing{}, inv, nil, &mockSettings{}, pms, charger, billingTestClock()))

	// Both leaders fetched the candidate list BEFORE either claimed —
	// the exact premise of the hazard.
	pending, err := inv.ListAutoChargePending(context.Background(), domain.InvoiceKeyset{}, 50)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	samePending := make([]domain.Invoice, len(pending))
	copy(samePending, pending)

	chargedA, errsA := engine.processAutoCharge(context.Background(), pending, nil, noPMTriggerSweep)
	chargedB, errsB := engine.processAutoCharge(context.Background(), samePending, nil, noPMTriggerSweep)

	if len(errsA) != 0 || len(errsB) != 0 {
		t.Fatalf("unexpected errors: A=%v B=%v", errsA, errsB)
	}
	if chargedA != 1 || chargedB != 0 {
		t.Errorf("charged: A=%d B=%d, want 1/0 — the claim must stop leader B before the charge leg", chargedA, chargedB)
	}
	if got := charger.calls["inv_dual"]; got != 1 {
		t.Fatalf("ChargeInvoice invocations: got %d, want exactly 1 — a second invocation is the double-charge", got)
	}
}

// failingCredits fails every credit application — drives the
// credit-apply-failure skip path.
type failingCredits struct{}

func (failingCredits) ApplyToInvoiceAt(context.Context, string, string, string, int64, time.Time, ...string) (int64, error) {
	return 0, errors.New("injected credit-apply failure")
}

// TestProcessAutoCharge_ReleasesClaimOnPreStripeSkips: the two skip
// paths where ChargeInvoice was provably never called (credit-apply
// failure, no payment method) must RELEASE the lease so the next
// tick — or the next test-clock Advance — retries immediately instead
// of silently waiting out five wall-clock minutes.
func TestProcessAutoCharge_ReleasesClaimOnPreStripeSkips(t *testing.T) {
	t.Run("no payment method", func(t *testing.T) {
		inv := &mockInvoices{
			invoices: []domain.Invoice{{
				ID: "inv_nopm", TenantID: "t1", CustomerID: "cus_1",
				Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending,
				AutoChargePending: true, AmountDueCents: 1000,
			}},
		}
		pms := &fakePaymentSetups{ready: false}
		engine := wireBaseTax(NewEngine(&mockSubs{cycleUpdated: make(map[string]bool)}, &mockUsage{}, &mockPricing{}, inv, nil, &mockSettings{}, pms, &countingCharger{}, billingTestClock()))

		pending, _ := inv.ListAutoChargePending(context.Background(), domain.InvoiceKeyset{}, 50)
		_, _ = engine.processAutoCharge(context.Background(), pending, nil, noPMTriggerSweep)

		if ok, _ := inv.ClaimAutoCharge(context.Background(), "t1", "inv_nopm"); !ok {
			t.Fatal("claim must be re-takeable immediately after the no-PM skip — the lease was not released")
		}
	})

	t.Run("credit apply failure", func(t *testing.T) {
		inv := &mockInvoices{
			invoices: []domain.Invoice{{
				ID: "inv_credfail", TenantID: "t1", CustomerID: "cus_1",
				Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending,
				AutoChargePending: true, AmountDueCents: 1000,
			}},
		}
		charger := &countingCharger{}
		engine := wireBaseTax(NewEngine(&mockSubs{cycleUpdated: make(map[string]bool)}, &mockUsage{}, &mockPricing{}, inv, failingCredits{}, &mockSettings{}, &fakePaymentSetups{ready: true, stripeCustomerID: "cus_s"}, charger, billingTestClock()))

		pending, _ := inv.ListAutoChargePending(context.Background(), domain.InvoiceKeyset{}, 50)
		_, _ = engine.processAutoCharge(context.Background(), pending, nil, noPMTriggerSweep)

		if charger.calls["inv_credfail"] != 0 {
			t.Fatal("charge must not fire when credit re-apply failed (overcharge guard)")
		}
		if ok, _ := inv.ClaimAutoCharge(context.Background(), "t1", "inv_credfail"); !ok {
			t.Fatal("claim must be re-takeable immediately after the credit-apply skip — the lease was not released")
		}
	})
}
