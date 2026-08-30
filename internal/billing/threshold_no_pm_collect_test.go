package billing

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// fakeNoPMNotifier records NotifyNoPaymentMethod calls so tests can assert
// the threshold fire's no-PM arm actually notifies (and that drafts don't).
type fakeNoPMNotifier struct {
	got []domain.Invoice
}

func (f *fakeNoPMNotifier) NotifyNoPaymentMethod(_ context.Context, _ string, inv domain.Invoice, trigger string) (domain.NotifyOutcome, error) {
	f.got = append(f.got, inv)
	return domain.NotifySent, nil
}

// thresholdNoPMFixture clones TestThresholdScan_CreditsApplied_ChargesRemainder's
// fixture with NO ready payment method. Returns the wired engine plus the mocks
// the assertions need.
func thresholdNoPMFixture() (*Engine, *mockInvoices, *recordingCharger, *fakeNoPMNotifier) {
	periodStart := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	nextBilling := periodEnd

	sub := domain.Subscription{
		ID: "sub_1", TenantID: "t1", CustomerID: "cus_1",
		Items:                     []domain.SubscriptionItem{{ID: "subitem_1", PlanID: "pln_1", Quantity: 1}},
		Status:                    domain.SubscriptionActive,
		BillingTime:               domain.BillingTimeCalendar,
		CurrentBillingPeriodStart: &periodStart,
		CurrentBillingPeriodEnd:   &periodEnd,
		NextBillingAt:             &nextBilling,
		BillingThresholds:         &domain.BillingThresholds{AmountGTE: 100000, ResetBillingCycle: true},
	}
	base := &mockSubs{subs: map[string]domain.Subscription{"sub_1": sub}, cycleUpdated: make(map[string]bool)}
	subs := &thresholdMockSubs{mockSubs: base, candidates: []domain.Subscription{sub}}
	usage := &mockUsage{totals: map[string]int64{"mtr_api": 1000}}
	pricing := &mockPricing{
		plans: map[string]domain.Plan{"pln_1": {
			ID: "pln_1", Name: "Pro Plan", Currency: "USD",
			BillingInterval: domain.BillingMonthly, BaseAmountCents: 4900,
			MeterIDs: []string{"mtr_api"},
		}},
		meters: map[string]domain.Meter{"mtr_api": {ID: "mtr_api", Name: "API Calls", Unit: "calls", RatingRuleVersionID: "rrv_api"}},
		rules: map[string]domain.RatingRuleVersion{"rrv_api": {
			ID: "rrv_api", RuleKey: "api_calls", Version: 1, Mode: domain.PricingFlat,
			FlatAmountCents: decimal.NewFromInt(100),
		}},
	}
	invoices := &mockInvoices{}
	applier := &fakeCreditApplier{inv: invoices, applyCents: 0}
	charger := &recordingCharger{}
	pms := &fakePaymentSetups{ready: false, stripeCustomerID: "cus_stripe_1"} // NO payment method

	engine := wireBaseTax(NewEngine(subs, usage, pricing, invoices, applier, &mockSettings{}, pms, charger, billingTestClock()))
	notifier := &fakeNoPMNotifier{}
	engine.SetNoPaymentMethodNotifier(notifier)
	return engine, invoices, charger, notifier
}

// TestThresholdScan_NoPM_QueuesAndNotifies pins the threshold fire's no-PM
// collect arm. Pre-fix the arm was MISSING: a no-PM customer crossing a spend
// threshold got a finalized invoice with auto_charge_pending=false (invisible
// to RetryPendingCharges — attaching a card later charged nothing) and no
// notification; it sat payment_pending until it aged into overdue. The fix
// mirrors billOnePeriod's block: queue for charge-on-attach + notify.
func TestThresholdScan_NoPM_QueuesAndNotifies(t *testing.T) {
	engine, invoices, charger, notifier := thresholdNoPMFixture()

	fired, errs := engine.ScanThresholds(context.Background(), 50)
	if len(errs) != 0 || fired != 1 {
		t.Fatalf("fired/errs: %d/%v", fired, errs)
	}
	if len(charger.got) != 0 {
		t.Fatalf("no charge must be attempted without a PM, got %d calls", len(charger.got))
	}
	if len(invoices.invoices) != 1 {
		t.Fatalf("invoices created: got %d, want 1", len(invoices.invoices))
	}
	inv := invoices.invoices[0]
	if inv.Status != domain.InvoiceFinalized {
		t.Fatalf("fixture expectation: invoice finalized, got %q", inv.Status)
	}
	if !inv.AutoChargePending {
		t.Error("no-PM threshold invoice must be queued for charge-on-attach (auto_charge_pending=true)")
	}
	if len(notifier.got) != 1 {
		t.Fatalf("customer must be notified exactly once, got %d", len(notifier.got))
	}
	if notifier.got[0].ID != inv.ID {
		t.Errorf("notified for invoice %q, want %q", notifier.got[0].ID, inv.ID)
	}
}

// TestThresholdScan_NoPM_DraftInvoiceStaysQuiet pins the draft arm: a
// tax-pending threshold invoice is a DRAFT and this site does nothing with it
// — not charged (the charger refuses non-finalized invoices), not emailed
// (totals aren't final), and not queued.
//
// It used to be queued here. The flag was inert while draft
// (ListAutoChargePending filters status='finalized') and existed only because
// the tax-retry chain finalized the invoice without collecting it, so
// pre-flagging was the one thing that made the sweep notice. That gap is
// closed at its source — invoice.Service.RetryTax queues on auto-finalize,
// covering cycle closes as well as threshold fires — and
// TestRetryTax_AutoFinalize_QueuesForCollection owns that half now. Queueing
// here again would make two sites own one handoff, inconsistently.
func TestThresholdScan_NoPM_DraftInvoiceStaysQuiet(t *testing.T) {
	engine, invoices, charger, notifier := thresholdNoPMFixture()
	// Force a tax defer -> the threshold invoice lands tax_status=pending, draft.
	engine.SetTaxProviderResolver(stubResolver(&stubProvider{err: fmt.Errorf("stripe tax down")}))

	fired, errs := engine.ScanThresholds(context.Background(), 50)
	if len(errs) != 0 || fired != 1 {
		t.Fatalf("fired/errs: %d/%v", fired, errs)
	}
	if len(invoices.invoices) != 1 {
		t.Fatalf("invoices created: got %d, want 1", len(invoices.invoices))
	}
	inv := invoices.invoices[0]
	if inv.Status == domain.InvoiceFinalized {
		t.Fatalf("fixture expectation: tax-deferred invoice must be a draft, got %q", inv.Status)
	}
	if len(charger.got) != 0 {
		t.Errorf("a draft must never be charged, got %d calls", len(charger.got))
	}
	if inv.AutoChargePending {
		t.Error("a tax-deferred draft must not be queued here; RetryTax queues it when it auto-finalizes")
	}
	if len(notifier.got) != 0 {
		t.Errorf("a draft must not trigger the no-PM notification, got %d", len(notifier.got))
	}
}
