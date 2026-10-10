package billing

import (
	"context"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/shopspring/decimal"
)

// These tests lock the C2 fix from the periodic-jobs audit: when a
// threshold invoice fired mid-cycle and the cycle was NOT reset
// (reset_billing_cycle=false, or reset=true whose cycle advance failed),
// the cycle close must bill only the residual — usage from the threshold
// invoice's billing_period_end, and NO in_arrears base fee (fireThreshold
// already billed the full unprorated base). Pre-fix the cycle close
// re-billed the whole period on top of the threshold invoice: base 2x,
// pre-fire usage 2x.

func residualHarness(thresholds *domain.BillingThresholds) (*mockSubs, *mockUsage, *mockPricing, *mockInvoices, *Engine) {
	periodStart := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	nextBilling := periodEnd

	subs := &mockSubs{
		subs: map[string]domain.Subscription{
			"sub_1": {
				ID: "sub_1", TenantID: "t1", CustomerID: "cus_1",
				Items:                     []domain.SubscriptionItem{{ID: "subitem_1", PlanID: "pln_1", Quantity: 1}},
				Status:                    domain.SubscriptionActive,
				BillingTime:               domain.BillingTimeCalendar,
				CurrentBillingPeriodStart: &periodStart,
				CurrentBillingPeriodEnd:   &periodEnd,
				NextBillingAt:             &nextBilling,
				BillingThresholds:         thresholds,
			},
		},
		cycleUpdated: make(map[string]bool),
	}
	usage := &mockUsage{
		totals: map[string]int64{"mtr_api": 1000}, // full period [Apr 1, May 1)
		perInterval: map[string]int64{
			// residual window [Apr 10, May 1) — what the clamped cycle
			// close must aggregate instead of the full period
			mockIntervalKey("mtr_api", time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC), periodEnd): 300,
		},
	}
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
	clk := clock.NewFake(time.Date(2026, 5, 1, 0, 0, 1, 0, time.UTC))
	engine := wireBaseTax(NewEngine(subs, usage, pricing, invoices, nil, &mockSettings{}, nil, nil, clk))
	return subs, usage, pricing, invoices, engine
}

func seedThresholdInvoice(inv *mockInvoices, status domain.InvoiceStatus) {
	inv.invoices = append(inv.invoices, domain.Invoice{
		ID: "inv_thr", TenantID: "t1", CustomerID: "cus_1", SubscriptionID: "sub_1",
		InvoiceNumber: "VLX-THR-1", BillingReason: domain.BillingReasonThreshold,
		Status: status, PaymentStatus: domain.PaymentSucceeded,
		BillingPeriodStart: time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
		BillingPeriodEnd:   time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC),
		SubtotalCents:      74900, TotalAmountCents: 74900,
	})
}

func cycleInvoiceLines(t *testing.T, inv *mockInvoices, invoiceID string) (base, usage []domain.InvoiceLineItem) {
	t.Helper()
	for _, li := range inv.lineItems {
		if li.InvoiceID != invoiceID {
			continue
		}
		switch li.LineType {
		case domain.LineTypeBaseFee:
			base = append(base, li)
		case domain.LineTypeUsage:
			usage = append(usage, li)
		}
	}
	return base, usage
}

func TestCycleClose_AfterThresholdFire_BillsOnlyResidual(t *testing.T) {
	_, _, _, invoices, engine := residualHarness(&domain.BillingThresholds{AmountGTE: 100000, ResetBillingCycle: false})
	seedThresholdInvoice(invoices, domain.InvoiceFinalized)

	if _, errs := engine.RunCycle(context.Background(), 50, nil); len(errs) > 0 {
		t.Fatalf("cycle errors: %v", errs)
	}
	if len(invoices.invoices) != 2 {
		t.Fatalf("invoices: got %d, want 2 (seeded threshold + cycle close)", len(invoices.invoices))
	}
	cycle := invoices.invoices[1]
	base, usage := cycleInvoiceLines(t, invoices, cycle.ID)

	// Base was billed IN FULL by the threshold invoice — re-emitting it
	// here is the 2x-base half of the double-bill.
	if len(base) != 0 {
		t.Errorf("cycle close must not re-bill the in_arrears base after a threshold fire; got %d base lines", len(base))
	}
	// Usage clamps to the residual window: 300 units @ 100 = 30000, NOT
	// the full-period 1000 units (the pre-fire 700 were on the threshold
	// invoice).
	if len(usage) != 1 {
		t.Fatalf("usage lines: got %d, want 1", len(usage))
	}
	if !usage[0].QuantityDecimal.Equal(decimal.NewFromInt(300)) {
		t.Errorf("usage quantity: got %s, want 300 (residual window only)", usage[0].QuantityDecimal)
	}
	if usage[0].AmountCents != 30000 {
		t.Errorf("usage amount: got %d, want 30000", usage[0].AmountCents)
	}
	wantStart := time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC)
	if usage[0].BillingPeriodStart == nil || !usage[0].BillingPeriodStart.Equal(wantStart) {
		t.Errorf("usage line period start: got %v, want %v (threshold watermark)", usage[0].BillingPeriodStart, wantStart)
	}
	if cycle.SubtotalCents != 30000 {
		t.Errorf("cycle subtotal: got %d, want 30000 (residual usage only)", cycle.SubtotalCents)
	}
}

func TestCycleClose_NoThresholdInvoice_FullPeriodUnchanged(t *testing.T) {
	_, _, _, invoices, engine := residualHarness(&domain.BillingThresholds{AmountGTE: 100000, ResetBillingCycle: false})

	if _, errs := engine.RunCycle(context.Background(), 50, nil); len(errs) > 0 {
		t.Fatalf("cycle errors: %v", errs)
	}
	if len(invoices.invoices) != 1 {
		t.Fatalf("invoices: got %d, want 1", len(invoices.invoices))
	}
	cycle := invoices.invoices[0]
	base, usage := cycleInvoiceLines(t, invoices, cycle.ID)
	if len(base) != 1 || base[0].AmountCents != 4900 {
		t.Fatalf("expected the full in_arrears base line (4900); got %+v", base)
	}
	if len(usage) != 1 || !usage[0].QuantityDecimal.Equal(decimal.NewFromInt(1000)) {
		t.Fatalf("expected full-period usage of 1000 units; got %+v", usage)
	}
	if cycle.SubtotalCents != 104900 {
		t.Errorf("cycle subtotal: got %d, want 104900", cycle.SubtotalCents)
	}
}

// The watermark keys on the INVOICE (ground truth), not the mutable
// BillingThresholds config — an operator removing thresholds after a
// mid-cycle fire must not resurrect the double-bill at period end.
func TestCycleClose_ThresholdsRemovedAfterFire_StillBillsResidual(t *testing.T) {
	_, _, _, invoices, engine := residualHarness(nil)
	seedThresholdInvoice(invoices, domain.InvoiceFinalized)

	if _, errs := engine.RunCycle(context.Background(), 50, nil); len(errs) > 0 {
		t.Fatalf("cycle errors: %v", errs)
	}
	cycle := invoices.invoices[1]
	base, usage := cycleInvoiceLines(t, invoices, cycle.ID)
	if len(base) != 0 {
		t.Errorf("base must stay skipped even after thresholds were removed; got %d base lines", len(base))
	}
	if len(usage) != 1 || !usage[0].QuantityDecimal.Equal(decimal.NewFromInt(300)) {
		t.Fatalf("expected residual usage of 300 units; got %+v", usage)
	}
}

// A VOIDED threshold invoice returned the money — its window is billable
// again, so the cycle close must fall back to full-period billing.
func TestCycleClose_VoidedThresholdInvoice_BillsFullPeriod(t *testing.T) {
	_, _, _, invoices, engine := residualHarness(&domain.BillingThresholds{AmountGTE: 100000, ResetBillingCycle: false})
	seedThresholdInvoice(invoices, domain.InvoiceVoided)

	if _, errs := engine.RunCycle(context.Background(), 50, nil); len(errs) > 0 {
		t.Fatalf("cycle errors: %v", errs)
	}
	cycle := invoices.invoices[1]
	base, usage := cycleInvoiceLines(t, invoices, cycle.ID)
	if len(base) != 1 {
		t.Errorf("voided threshold invoice must not suppress the base line; got %d base lines", len(base))
	}
	if len(usage) != 1 || !usage[0].QuantityDecimal.Equal(decimal.NewFromInt(1000)) {
		t.Fatalf("expected full-period usage of 1000 units; got %+v", usage)
	}
}
