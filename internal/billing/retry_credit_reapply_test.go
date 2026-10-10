package billing

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// fakeCreditApplier drives the credit-before-charge contract in the retry
// sweep. applyCents is drained against the mock invoice's amount_due (capped),
// mirroring ApplyToInvoiceAtomic's min(due, balance) semantics; err simulates
// a failed application (DB blip).
type fakeCreditApplier struct {
	inv        *mockInvoices
	applyCents int64
	err        error
	calls      int
}

func (f *fakeCreditApplier) ApplyToInvoiceAt(_ context.Context, _, _, invoiceID string, amountCents int64, _ time.Time, _ ...string) (int64, error) {
	f.calls++
	if f.err != nil {
		return 0, f.err
	}
	deduct := min(f.applyCents, amountCents)
	for i, inv := range f.inv.invoices {
		if inv.ID == invoiceID {
			f.inv.invoices[i].AmountDueCents -= deduct
		}
	}
	return deduct, nil
}

// recordingCharger records each invoice it is asked to charge. With store set
// it also records the success on the row (payment_status='succeeded'), as the
// real charger's settle does.
type recordingCharger struct {
	got   []domain.Invoice
	store *mockInvoices
}

func (c *recordingCharger) ChargeInvoice(_ context.Context, _ string, inv domain.Invoice, _, _ string) (domain.Invoice, error) {
	c.got = append(c.got, inv)
	c.store.stampPaymentStatus(inv.ID, domain.PaymentSucceeded)
	return inv, nil
}

// fakeDunResolver records ResolveByInvoice calls so a test can prove a background
// credit-cover settle resolves the invoice's dunning run (the prompt half of the
// stranded-active-run fix).
type fakeDunResolver struct {
	got []struct {
		invoiceID  string
		resolution domain.DunningResolution
	}
}

func (f *fakeDunResolver) ResolveByInvoice(_ context.Context, _, invoiceID string, res domain.DunningResolution) error {
	f.got = append(f.got, struct {
		invoiceID  string
		resolution domain.DunningResolution
	}{invoiceID, res})
	return nil
}

func pendingInvoice() domain.Invoice {
	return domain.Invoice{
		ID: "inv_1", TenantID: "t1", CustomerID: "cus_1",
		Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending,
		TaxFacts: domain.TaxFacts{TaxStatus: domain.InvoiceTaxOK}, InvoiceNumber: "VLX-1",
		AutoChargePending: true, AmountDueCents: 1000,
	}
}

func retryHarness(t *testing.T, applier *fakeCreditApplier) (*mockInvoices, *recordingCharger, *Engine) {
	t.Helper()
	inv := &mockInvoices{invoices: []domain.Invoice{pendingInvoice()}}
	if applier != nil {
		applier.inv = inv
	}
	charger := &recordingCharger{}
	pms := &fakePaymentSetups{ready: true, stripeCustomerID: "cus_stripe_1"}
	var credits CreditApplier
	if applier != nil {
		credits = applier
	}
	engine := wireBaseTax(NewEngine(&mockSubs{cycleUpdated: make(map[string]bool)}, &mockUsage{}, &mockPricing{}, inv, credits, &mockSettings{}, pms, charger, billingTestClock()))
	return inv, charger, engine
}

// TestRetrySweep_ReappliesCreditsBeforeCharge locks the H1 fix: an invoice in
// the auto-charge sweep gets customer credits applied BEFORE the card is
// charged, and the charge fires for the reduced remainder — never the raw
// pre-credit amount_due. Pre-fix the sweep charged 1000 with 400 of credits
// sitting unconsumed.
func TestRetrySweep_ReappliesCreditsBeforeCharge(t *testing.T) {
	applier := &fakeCreditApplier{applyCents: 400}
	_, charger, engine := retryHarness(t, applier)

	charged, errs := engine.RetryPendingCharges(context.Background(), 50, nil)
	if len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	if charged != 1 {
		t.Fatalf("charged: got %d, want 1", charged)
	}
	if applier.calls != 1 {
		t.Fatalf("credit applier calls: got %d, want 1", applier.calls)
	}
	if len(charger.got) != 1 {
		t.Fatalf("charge calls: got %d, want 1", len(charger.got))
	}
	if got := charger.got[0].AmountDueCents; got != 600 {
		t.Errorf("charged amount_due: got %d, want 600 (1000 - 400 credits)", got)
	}
}

// TestRetrySweep_FullyCreditCovered_MarksPaidWithoutCharge: when credits cover
// the entire remainder, the invoice settles from the balance — MarkPaid, flag
// cleared, no card charge at all.
func TestRetrySweep_FullyCreditCovered_MarksPaidWithoutCharge(t *testing.T) {
	applier := &fakeCreditApplier{applyCents: 1000}
	inv, charger, engine := retryHarness(t, applier)
	resolver := &fakeDunResolver{}
	engine.SetDunningResolver(resolver)

	charged, errs := engine.RetryPendingCharges(context.Background(), 50, nil)
	if len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	if charged != 1 {
		t.Fatalf("charged (settled) count: got %d, want 1", charged)
	}
	if len(charger.got) != 0 {
		t.Fatalf("card must not be charged on a fully-credited invoice; got %d charges", len(charger.got))
	}
	row := inv.invoices[0]
	if row.Status != domain.InvoicePaid || row.PaymentStatus != domain.PaymentSucceeded {
		t.Errorf("invoice state: got %s/%s, want paid/succeeded", row.Status, row.PaymentStatus)
	}
	if row.AutoChargePending {
		t.Error("AutoChargePending must be cleared after credit settlement")
	}
	// The fix: a background credit settle resolves the invoice's dunning run so
	// it isn't left active (stale red badge / risk of dunning a paid invoice).
	if len(resolver.got) != 1 || resolver.got[0].invoiceID != "inv_1" || resolver.got[0].resolution != domain.ResolutionPaymentRecovered {
		t.Errorf("credit settle must resolve dunning as payment_recovered, got %+v", resolver.got)
	}
}

// TestRetrySweep_CreditApplyFailure_SkipsCharge: a failed credit application
// must SKIP the charge (and leave the flag set for the next tick) — charging
// the raw amount_due would consummate the overcharge the cycle path's
// flag-and-retry exists to prevent.
func TestRetrySweep_CreditApplyFailure_SkipsCharge(t *testing.T) {
	applier := &fakeCreditApplier{err: errors.New("db blip")}
	inv, charger, engine := retryHarness(t, applier)

	charged, errs := engine.RetryPendingCharges(context.Background(), 50, nil)
	if len(errs) != 0 {
		t.Fatalf("a credit-apply failure is a skip, not an escalated error: %v", errs)
	}
	if charged != 0 {
		t.Fatalf("charged: got %d, want 0", charged)
	}
	if len(charger.got) != 0 {
		t.Fatalf("card must not be charged when credit apply failed; got %d charges", len(charger.got))
	}
	if !inv.invoices[0].AutoChargePending {
		t.Error("AutoChargePending must remain set so the next tick retries")
	}
}

// TestRetrySweep_NoCreditsWired_ChargesAsBefore: nil credit applier (narrow
// test setups) preserves the legacy direct-charge behavior.
func TestRetrySweep_NoCreditsWired_ChargesAsBefore(t *testing.T) {
	_, charger, engine := retryHarness(t, nil)

	charged, errs := engine.RetryPendingCharges(context.Background(), 50, nil)
	if len(errs) != 0 || charged != 1 {
		t.Fatalf("charged/errs: got %d/%v, want 1/none", charged, errs)
	}
	if len(charger.got) != 1 || charger.got[0].AmountDueCents != 1000 {
		t.Fatalf("expected one full-amount charge, got %+v", charger.got)
	}
}
