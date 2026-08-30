package billing

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// TestThresholdScan_CreditApplyFailure_FlagsAndSkipsCharge locks the C1 fix:
// when credit application fails on a threshold-fired invoice, the scan must
// flag the invoice for the retry sweep and SKIP the inline auto-charge — the
// pre-fix behavior charged the customer's card the FULL pre-credit total while
// their balance sat unconsumed (the same bug class billOnePeriod's
// creditApplyOK gate fixed on 2026-05-30, never ported to the threshold path).
func TestThresholdScan_CreditApplyFailure_FlagsAndSkipsCharge(t *testing.T) {
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
	usage := &mockUsage{totals: map[string]int64{"mtr_api": 1000}} // crosses the cap
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
	applier := &fakeCreditApplier{inv: invoices, err: errors.New("db blip during credit apply")}
	charger := &recordingCharger{}
	pms := &fakePaymentSetups{ready: true, stripeCustomerID: "cus_stripe_1"}

	engine := wireBaseTax(NewEngine(subs, usage, pricing, invoices, applier, &mockSettings{}, pms, charger, billingTestClock()))

	fired, errs := engine.ScanThresholds(context.Background(), 50)
	if len(errs) != 0 {
		t.Fatalf("scan errors: %v", errs)
	}
	if fired != 1 {
		t.Fatalf("fired: got %d, want 1", fired)
	}
	if applier.calls != 1 {
		t.Fatalf("credit applier calls: got %d, want 1", applier.calls)
	}
	// The gate: no inline charge, invoice flagged for the retry sweep
	// (which re-applies credits before charging — see processAutoCharge).
	if len(charger.got) != 0 {
		t.Fatalf("card must NOT be charged when credit apply failed; got %d charges", len(charger.got))
	}
	if len(invoices.invoices) != 1 {
		t.Fatalf("threshold invoices created: got %d, want 1", len(invoices.invoices))
	}
	row := invoices.invoices[0]
	if !row.AutoChargePending {
		t.Error("threshold invoice must be flagged auto_charge_pending for the retry sweep")
	}
	if row.Status == domain.InvoicePaid {
		t.Error("invoice must not be marked paid when credit apply failed")
	}
}

// TestThresholdScan_CreditsApplied_ChargesRemainder pins the happy path around
// the new gate: successful credit application reduces the inline charge to the
// remainder (no flag, no skip).
func TestThresholdScan_CreditsApplied_ChargesRemainder(t *testing.T) {
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
	applier := &fakeCreditApplier{inv: invoices, applyCents: 4900}
	charger := &recordingCharger{}
	pms := &fakePaymentSetups{ready: true, stripeCustomerID: "cus_stripe_1"}

	engine := wireBaseTax(NewEngine(subs, usage, pricing, invoices, applier, &mockSettings{}, pms, charger, billingTestClock()))

	fired, errs := engine.ScanThresholds(context.Background(), 50)
	if len(errs) != 0 || fired != 1 {
		t.Fatalf("fired/errs: %d/%v", fired, errs)
	}
	if len(charger.got) != 1 {
		t.Fatalf("charge calls: got %d, want 1", len(charger.got))
	}
	if got, want := charger.got[0].AmountDueCents, invoices.invoices[0].TotalAmountCents-4900; got != want {
		t.Errorf("charged remainder: got %d, want %d (total - 4900 credits)", got, want)
	}
	if invoices.invoices[0].AutoChargePending {
		t.Error("flag must not be set on the happy path")
	}
}
