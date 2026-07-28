package billing

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// TestBillFinalOnImmediateCancel_MultiDimMeterBilled locks the ADR-044 fix for
// the cancel-path revenue leak: a meter priced via dimension-match pricing
// rules (the AI `tokens` shape — one rule per {model, token_type}, meter
// RatingRuleVersionID empty) was silently skipped by BillFinalOnImmediateCancel's
// single-rule-only usage loop, so a mid-period immediate cancel emitted NO
// invoice for all token usage. The cycle path, preview, and the threshold
// scan all carry the AggregateByPricingRules fork; this asserts the cancel
// path now does too.
//
// Numbers: 1,000,000 input tokens @ $3.00/M + 500,000 output tokens @
// $15.00/M = $3.00 + $7.50 = $10.50 the customer would otherwise consume
// for free by canceling mid-period.
func TestBillFinalOnImmediateCancel_MultiDimMeterBilled(t *testing.T) {
	periodStart := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	cancelAt := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC) // mid-period

	sub := domain.Subscription{
		ID: "sub_1", TenantID: "t1", CustomerID: "cus_1", Code: "tokens",
		Status: domain.SubscriptionCanceled,
		Items: []domain.SubscriptionItem{{
			ID: "si_1", PlanID: "pln_tok", Quantity: 1,
		}},
		CurrentBillingPeriodStart: &periodStart,
		CurrentBillingPeriodEnd:   &periodEnd,
		CanceledAt:                &cancelAt,
	}

	pricing := &mockPricing{
		plans: map[string]domain.Plan{
			// Pure-usage plan: no base fee, one multi-dim tokens meter.
			"pln_tok": {ID: "pln_tok", Name: "Tokens", Currency: "USD",
				BillingInterval: domain.BillingMonthly, BaseAmountCents: 0,
				BaseBillTiming: domain.BillInArrears, MeterIDs: []string{"mtr_tokens"}},
		},
		meters: map[string]domain.Meter{
			// Multi-dim shape: NO meter-linked default rule. Pre-fix the
			// cancel loop's `RatingRuleVersionID == "" → continue` guard
			// skipped this meter entirely.
			"mtr_tokens": {ID: "mtr_tokens", Key: "tokens", Name: "Tokens",
				Unit: "tokens", Aggregation: "sum", RatingRuleVersionID: ""},
		},
		rules: map[string]domain.RatingRuleVersion{
			// Per-token decimal rates (ADR-045): $3.00/M and $15.00/M.
			"rrv_in": {ID: "rrv_in", RuleKey: "tok_in", Version: 1,
				Mode: domain.PricingFlat, FlatAmountCents: decimal.RequireFromString("0.0003")},
			"rrv_out": {ID: "rrv_out", RuleKey: "tok_out", Version: 1,
				Mode: domain.PricingFlat, FlatAmountCents: decimal.RequireFromString("0.0015")},
		},
		meterPricingRules: map[string][]domain.MeterPricingRule{
			"mtr_tokens": {
				{ID: "mpr_in", MeterID: "mtr_tokens", RatingRuleVersionID: "rrv_in",
					DimensionMatch: map[string]any{"token_type": "input"}},
				{ID: "mpr_out", MeterID: "mtr_tokens", RatingRuleVersionID: "rrv_out",
					DimensionMatch: map[string]any{"token_type": "output"}},
			},
		},
	}
	usage := &mockUsage{
		ruleAggs: map[string][]domain.RuleAggregation{
			"mtr_tokens": {
				{RuleID: "mpr_in", RatingRuleVersionID: "rrv_in", Quantity: decimal.NewFromInt(1_000_000)},
				{RuleID: "mpr_out", RatingRuleVersionID: "rrv_out", Quantity: decimal.NewFromInt(500_000)},
			},
		},
	}
	invoices := &mockInvoices{}

	engine := wireBaseTax(NewEngine(&mockSubs{subs: map[string]domain.Subscription{sub.ID: sub}}, usage, pricing, invoices, nil, &mockSettings{}, nil, nil, billingTestClock()))

	inv, err := engine.BillFinalOnImmediateCancel(context.Background(), sub)
	if err != nil {
		t.Fatalf("BillFinalOnImmediateCancel: %v", err)
	}

	// Pre-fix: subtotal stayed 0 → the zero-subtotal guard returned an empty
	// invoice and the token usage was never billed.
	if inv.ID == "" {
		t.Fatal("REVENUE LEAK: no final invoice emitted — multi-dim token usage went unbilled on immediate cancel")
	}
	if inv.SubtotalCents != 1050 {
		t.Errorf("subtotal: got %d, want 1050 ($3.00 input + $7.50 output)", inv.SubtotalCents)
	}
	if inv.BillingReason != domain.BillingReasonSubscriptionCancel {
		t.Errorf("billing reason: got %q, want subscription_cancel", inv.BillingReason)
	}

	if len(invoices.lineItems) != 2 {
		t.Fatalf("line items: got %d, want 2 (one per claimed pricing rule)", len(invoices.lineItems))
	}
	byRule := map[string]domain.InvoiceLineItem{}
	for _, li := range invoices.lineItems {
		byRule[li.RatingRuleVersionID] = li
	}
	in, out := byRule["rrv_in"], byRule["rrv_out"]
	if in.AmountCents != 300 {
		t.Errorf("input line amount: got %d, want 300", in.AmountCents)
	}
	if out.AmountCents != 750 {
		t.Errorf("output line amount: got %d, want 750", out.AmountCents)
	}
	if !in.QuantityDecimal.Equal(decimal.NewFromInt(1_000_000)) {
		t.Errorf("input line QuantityDecimal: got %s, want 1000000", in.QuantityDecimal)
	}
	for _, li := range invoices.lineItems {
		if !strings.Contains(li.Description, "canceled mid-period") {
			t.Errorf("line description %q: want the '- canceled mid-period' suffix (cancel-path parity)", li.Description)
		}
		if li.LineType != domain.LineTypeUsage {
			t.Errorf("line type: got %q, want usage", li.LineType)
		}
	}
}

// TestBillFinalOnImmediateCancel_SingleRuleMeterStillBilled proves the legacy
// single-rule path is untouched by the multi-dim fork: a meter with a direct
// RatingRuleVersionID binding (and no pricing rules) bills exactly as before.
func TestBillFinalOnImmediateCancel_SingleRuleMeterStillBilled(t *testing.T) {
	periodStart := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	cancelAt := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)

	sub := domain.Subscription{
		ID: "sub_1", TenantID: "t1", CustomerID: "cus_1", Code: "api",
		Status: domain.SubscriptionCanceled,
		Items: []domain.SubscriptionItem{{
			ID: "si_1", PlanID: "pln_api", Quantity: 1,
		}},
		CurrentBillingPeriodStart: &periodStart,
		CurrentBillingPeriodEnd:   &periodEnd,
		CanceledAt:                &cancelAt,
	}

	pricing := &mockPricing{
		plans: map[string]domain.Plan{
			"pln_api": {ID: "pln_api", Name: "API", Currency: "USD",
				BillingInterval: domain.BillingMonthly, BaseAmountCents: 0,
				BaseBillTiming: domain.BillInArrears, MeterIDs: []string{"mtr_api"}},
		},
		meters: map[string]domain.Meter{
			"mtr_api": {ID: "mtr_api", Key: "api", Name: "API Calls",
				Unit: "calls", Aggregation: "sum", RatingRuleVersionID: "rrv_api"},
		},
		rules: map[string]domain.RatingRuleVersion{
			"rrv_api": {ID: "rrv_api", RuleKey: "api_calls", Version: 1,
				Mode: domain.PricingFlat, FlatAmountCents: decimal.NewFromInt(2)},
		},
		// No meterPricingRules → single-rule path.
	}
	usage := &mockUsage{totals: map[string]int64{"mtr_api": 500}}
	invoices := &mockInvoices{}

	engine := wireBaseTax(NewEngine(&mockSubs{subs: map[string]domain.Subscription{sub.ID: sub}}, usage, pricing, invoices, nil, &mockSettings{}, nil, nil, billingTestClock()))

	inv, err := engine.BillFinalOnImmediateCancel(context.Background(), sub)
	if err != nil {
		t.Fatalf("BillFinalOnImmediateCancel: %v", err)
	}
	if inv.ID == "" {
		t.Fatal("expected a final invoice for single-rule meter usage")
	}
	if inv.SubtotalCents != 1000 {
		t.Errorf("subtotal: got %d, want 1000 (500 calls × 2¢)", inv.SubtotalCents)
	}
}

// zeroTermsSettings returns a settings row whose NetPaymentTerms is 0 — the
// legacy-row shape that predates validation clamping (a hard store ERROR
// can't reach the netDays code: ApplyTaxToLineItems fails loudly first).
// Exercises the `> 0` guard falling through to the fallback.
type zeroTermsSettings struct{ mockSettings }

func (z *zeroTermsSettings) Get(_ context.Context, _ string) (domain.TenantSettings, error) {
	return domain.TenantSettings{NetPaymentTerms: 0, TaxProvider: "none"}, nil
}

// TestBillFinalOnImmediateCancel_UsageCapApplied locks the cap-scaling fix:
// a "block"-capped subscription's final cancel invoice must bill at most the
// per-cycle cap, scaled proportionally across buckets — exactly what cycle
// close would have charged. Pre-fix the cancel path billed raw above-cap
// usage (both single-rule and multi-dim branches).
//
// Cap 750,000 units against 1.5M raw (1M input + 500k output) → scale 0.5:
// input 500k × $3.00/M = $1.50, output 250k × $15.00/M = $3.75 → $5.25,
// half the uncapped $10.50.
func TestBillFinalOnImmediateCancel_UsageCapApplied(t *testing.T) {
	periodStart := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	cancelAt := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)
	cap := int64(750_000)

	sub := domain.Subscription{
		ID: "sub_1", TenantID: "t1", CustomerID: "cus_1", Code: "tokens",
		Status: domain.SubscriptionCanceled,
		Items: []domain.SubscriptionItem{{
			ID: "si_1", PlanID: "pln_tok", Quantity: 1,
		}},
		CurrentBillingPeriodStart: &periodStart,
		CurrentBillingPeriodEnd:   &periodEnd,
		CanceledAt:                &cancelAt,
		UsageCapUnits:             &cap,
		OverageAction:             "block",
	}

	pricing := &mockPricing{
		plans: map[string]domain.Plan{
			"pln_tok": {ID: "pln_tok", Name: "Tokens", Currency: "USD",
				BillingInterval: domain.BillingMonthly, BaseAmountCents: 0,
				BaseBillTiming: domain.BillInArrears, MeterIDs: []string{"mtr_tokens"}},
		},
		meters: map[string]domain.Meter{
			"mtr_tokens": {ID: "mtr_tokens", Key: "tokens", Name: "Tokens",
				Unit: "tokens", Aggregation: "sum", RatingRuleVersionID: ""},
		},
		rules: map[string]domain.RatingRuleVersion{
			"rrv_in": {ID: "rrv_in", RuleKey: "tok_in", Version: 1,
				Mode: domain.PricingFlat, FlatAmountCents: decimal.RequireFromString("0.0003")},
			"rrv_out": {ID: "rrv_out", RuleKey: "tok_out", Version: 1,
				Mode: domain.PricingFlat, FlatAmountCents: decimal.RequireFromString("0.0015")},
		},
		meterPricingRules: map[string][]domain.MeterPricingRule{
			"mtr_tokens": {
				{ID: "mpr_in", MeterID: "mtr_tokens", RatingRuleVersionID: "rrv_in",
					DimensionMatch: map[string]any{"token_type": "input"}},
				{ID: "mpr_out", MeterID: "mtr_tokens", RatingRuleVersionID: "rrv_out",
					DimensionMatch: map[string]any{"token_type": "output"}},
			},
		},
	}
	usage := &mockUsage{
		// totals feeds the cap computation (window aggregate)…
		totals: map[string]int64{"mtr_tokens": 1_500_000},
		// …ruleAggs feeds the multi-dim per-bucket pricing (raw, pre-cap).
		ruleAggs: map[string][]domain.RuleAggregation{
			"mtr_tokens": {
				{RuleID: "mpr_in", RatingRuleVersionID: "rrv_in", Quantity: decimal.NewFromInt(1_000_000)},
				{RuleID: "mpr_out", RatingRuleVersionID: "rrv_out", Quantity: decimal.NewFromInt(500_000)},
			},
		},
	}
	invoices := &mockInvoices{}

	engine := wireBaseTax(NewEngine(&mockSubs{subs: map[string]domain.Subscription{sub.ID: sub}}, usage, pricing, invoices, nil, &mockSettings{}, nil, nil, billingTestClock()))

	inv, err := engine.BillFinalOnImmediateCancel(context.Background(), sub)
	if err != nil {
		t.Fatalf("BillFinalOnImmediateCancel: %v", err)
	}
	if inv.ID == "" {
		t.Fatal("expected a final invoice")
	}
	if inv.SubtotalCents != 525 {
		t.Errorf("subtotal: got %d, want 525 (cap-scaled half of the raw $10.50)", inv.SubtotalCents)
	}
	byRule := map[string]domain.InvoiceLineItem{}
	for _, li := range invoices.lineItems {
		byRule[li.RatingRuleVersionID] = li
	}
	if got := byRule["rrv_in"].AmountCents; got != 150 {
		t.Errorf("input line: got %d, want 150 (500k capped tokens)", got)
	}
	if got := byRule["rrv_out"].AmountCents; got != 375 {
		t.Errorf("output line: got %d, want 375 (250k capped tokens)", got)
	}
	if !byRule["rrv_in"].QuantityDecimal.Equal(decimal.NewFromInt(500_000)) {
		t.Errorf("input qty: got %s, want 500000 (cap-scaled)", byRule["rrv_in"].QuantityDecimal)
	}
}

// TestBillFinalOnImmediateCancel_NetTermsFallback30 pins the netDays
// fallback: when settings carry no positive NetPaymentTerms (legacy zero
// row), the cancel invoice must fall back to Net-30 like every other
// writer — not Net-0 (due_at == issued_at → dunning fires on day 0).
func TestBillFinalOnImmediateCancel_NetTermsFallback30(t *testing.T) {
	periodStart := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	cancelAt := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)

	sub := domain.Subscription{
		ID: "sub_1", TenantID: "t1", CustomerID: "cus_1", Code: "api",
		Status: domain.SubscriptionCanceled,
		Items: []domain.SubscriptionItem{{
			ID: "si_1", PlanID: "pln_api", Quantity: 1,
		}},
		CurrentBillingPeriodStart: &periodStart,
		CurrentBillingPeriodEnd:   &periodEnd,
		CanceledAt:                &cancelAt,
	}
	pricing := &mockPricing{
		plans: map[string]domain.Plan{
			"pln_api": {ID: "pln_api", Name: "API", Currency: "USD",
				BillingInterval: domain.BillingMonthly, BaseAmountCents: 0,
				BaseBillTiming: domain.BillInArrears, MeterIDs: []string{"mtr_api"}},
		},
		meters: map[string]domain.Meter{
			"mtr_api": {ID: "mtr_api", Key: "api", Name: "API Calls",
				Unit: "calls", Aggregation: "sum", RatingRuleVersionID: "rrv_api"},
		},
		rules: map[string]domain.RatingRuleVersion{
			"rrv_api": {ID: "rrv_api", RuleKey: "api_calls", Version: 1,
				Mode: domain.PricingFlat, FlatAmountCents: decimal.NewFromInt(2)},
		},
	}
	usage := &mockUsage{totals: map[string]int64{"mtr_api": 500}}
	invoices := &mockInvoices{}

	engine := wireBaseTax(NewEngine(&mockSubs{subs: map[string]domain.Subscription{sub.ID: sub}}, usage, pricing, invoices, nil, &zeroTermsSettings{}, nil, nil, billingTestClock()))

	inv, err := engine.BillFinalOnImmediateCancel(context.Background(), sub)
	if err != nil {
		t.Fatalf("BillFinalOnImmediateCancel: %v", err)
	}
	if inv.NetPaymentTermDays != 30 {
		t.Errorf("NetPaymentTermDays: got %d, want fallback 30 when settings are unreadable (was 0 → instant dunning)", inv.NetPaymentTermDays)
	}
	if inv.DueAt == nil || inv.IssuedAt == nil || !inv.DueAt.Equal(inv.IssuedAt.AddDate(0, 0, 30)) {
		t.Errorf("DueAt = %v, want IssuedAt (%v) + 30d", inv.DueAt, inv.IssuedAt)
	}
}
