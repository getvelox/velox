package billing

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/tax"
	"github.com/shopspring/decimal"
)

// taxSettings returns a fixed TenantSettings so engine tests don't need a
// real store. NextInvoiceNumber is unused in the tax-apply path and errors
// loudly if something calls it by accident.
type taxSettings struct {
	provider  string
	rateBP    int64
	name      string
	inclusive bool
	taxCode   string
}

func (s *taxSettings) Get(_ context.Context, _ string) (domain.TenantSettings, error) {
	return domain.TenantSettings{
		TaxProvider:           s.provider,
		TaxRate:               float64(s.rateBP) / 100, // legacy fixture: rateBP is still int64 — convert to percent for the new field
		TaxName:               s.name,
		TaxInclusive:          s.inclusive,
		DefaultProductTaxCode: s.taxCode,
	}, nil
}

func (s *taxSettings) NextInvoiceNumber(_ context.Context, _ string) (string, error) {
	return "", errors.New("NextInvoiceNumber must not be called in tax-apply tests")
}

// taxProfiles returns a billing profile for configured customers; missing
// customer → error (matches real store behaviour).
type taxProfiles struct {
	profiles map[string]domain.CustomerBillingProfile
}

func (p *taxProfiles) GetBillingProfile(_ context.Context, _, customerID string) (domain.CustomerBillingProfile, error) {
	bp, ok := p.profiles[customerID]
	if !ok {
		return domain.CustomerBillingProfile{}, fmt.Errorf("not found")
	}
	return bp, nil
}

// resolverFunc adapts a function into a TaxProviderResolver.
type resolverFunc func(ctx context.Context, ts domain.TenantSettings) (tax.Provider, error)

func (f resolverFunc) Resolve(ctx context.Context, ts domain.TenantSettings) (tax.Provider, error) {
	return f(ctx, ts)
}

// manualResolver wires a ManualProvider with the settings' rate/name.
func manualResolver() TaxProviderResolver {
	return resolverFunc(func(_ context.Context, ts domain.TenantSettings) (tax.Provider, error) {
		return tax.NewManualProvider(ts.TaxRate, ts.TaxName), nil
	})
}

// stubProvider returns a prebuilt Result from Calculate. Used to assert the
// engine correctly maps provider output onto line items without coupling
// tests to ManualProvider's arithmetic.
type stubProvider struct {
	result *tax.Result
	err    error
}

func (*stubProvider) Name() string { return "stub" }
func (s *stubProvider) Calculate(_ context.Context, _ tax.Request) (*tax.Result, error) {
	return s.result, s.err
}
func (*stubProvider) Commit(_ context.Context, _, _ string) (string, error) { return "", nil }
func (*stubProvider) Reverse(_ context.Context, _ tax.ReversalRequest) (*tax.ReversalResult, error) {
	return &tax.ReversalResult{}, nil
}

func stubResolver(p tax.Provider) TaxProviderResolver {
	return resolverFunc(func(_ context.Context, _ domain.TenantSettings) (tax.Provider, error) {
		return p, nil
	})
}

func newManualEngine(rateBP int64, name string, profiles map[string]domain.CustomerBillingProfile) *Engine {
	e := &Engine{
		settings:     &taxSettings{provider: "manual", rateBP: rateBP, name: name},
		taxProviders: manualResolver(),
	}
	if profiles != nil {
		e.profiles = &taxProfiles{profiles: profiles}
	}
	return e
}

func newInclusiveManualEngine(rateBP int64, name string) *Engine {
	return &Engine{
		settings:     &taxSettings{provider: "manual", rateBP: rateBP, name: name, inclusive: true},
		taxProviders: manualResolver(),
	}
}

func TestApplyTaxToLineItems_ManualFlatRate(t *testing.T) {
	e := newManualEngine(1850, "VAT", nil)
	lineItems := []domain.InvoiceLineItem{{AmountCents: 10000, Description: "base", Quantity: 1}}

	r, err := e.ApplyTaxToLineItems(context.Background(), "t1", "cus_1", "USD", 10000, 0, lineItems)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.TaxAmountCents != 1850 {
		t.Errorf("got tax %d, want 18.50", r.TaxAmountCents)
	}
	if r.TaxRate != 18.50 || r.TaxName != "VAT" {
		t.Errorf("got rate=%g name=%q, want 18.50 VAT", r.TaxRate, r.TaxName)
	}
	if r.TaxProvider != "manual" {
		t.Errorf("got provider %q, want manual", r.TaxProvider)
	}
	if lineItems[0].TaxAmountCents != 1850 || lineItems[0].TotalAmountCents != 11850 {
		t.Errorf("line: tax=%d total=%d, want 18.50 and 11850",
			lineItems[0].TaxAmountCents, lineItems[0].TotalAmountCents)
	}
}

func TestApplyTaxToLineItems_ExemptStatusZeroesTax(t *testing.T) {
	profiles := map[string]domain.CustomerBillingProfile{
		"cus_1": {CustomerID: "cus_1", TaxStatus: tax.StatusExempt, TaxExemptReason: "501(c)(3) nonprofit"},
	}
	e := newManualEngine(1850, "VAT", profiles)
	lineItems := []domain.InvoiceLineItem{{AmountCents: 10000, Quantity: 1}}

	r, err := e.ApplyTaxToLineItems(context.Background(), "t1", "cus_1", "USD", 10000, 0, lineItems)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.TaxAmountCents != 0 {
		t.Errorf("got tax %d, want 0 (exempt)", r.TaxAmountCents)
	}
	if r.TaxExemptReason != "501(c)(3) nonprofit" {
		t.Errorf("got exempt reason %q, want the billing profile's value", r.TaxExemptReason)
	}
	if r.TaxReverseCharge {
		t.Error("exempt must not set reverse-charge flag")
	}
	if lineItems[0].TaxAmountCents != 0 {
		t.Errorf("line tax = %d, want 0", lineItems[0].TaxAmountCents)
	}
}

func TestApplyTaxToLineItems_ReverseChargeStatus(t *testing.T) {
	profiles := map[string]domain.CustomerBillingProfile{
		"cus_1": {CustomerID: "cus_1", Country: "DE", TaxStatus: tax.StatusReverseCharge, TaxID: "DE123456789"},
	}
	e := newManualEngine(1850, "VAT", profiles)
	lineItems := []domain.InvoiceLineItem{{AmountCents: 10000, Quantity: 1}}

	r, err := e.ApplyTaxToLineItems(context.Background(), "t1", "cus_1", "USD", 10000, 0, lineItems)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.TaxAmountCents != 0 {
		t.Errorf("got tax %d, want 0 (reverse charge)", r.TaxAmountCents)
	}
	if !r.TaxReverseCharge {
		t.Error("reverse-charge flag must be set so the PDF legend renders")
	}
	if r.TaxExemptReason != "" {
		t.Errorf("reverse-charge must not carry exempt reason, got %q", r.TaxExemptReason)
	}
}

func TestApplyTaxToLineItems_ZeroSubtotal(t *testing.T) {
	e := newManualEngine(1850, "VAT", nil)
	lineItems := []domain.InvoiceLineItem{{AmountCents: 0}}

	r, err := e.ApplyTaxToLineItems(context.Background(), "t1", "cus_1", "USD", 0, 0, lineItems)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.TaxAmountCents != 0 {
		t.Errorf("got tax %d, want 0", r.TaxAmountCents)
	}
}

func TestApplyTaxToLineItems_DiscountReducesTax(t *testing.T) {
	// 18.5% of ($100 - $50) = $9.25 → 925 cents.
	e := newManualEngine(1850, "VAT", nil)
	lineItems := []domain.InvoiceLineItem{{AmountCents: 10000, Quantity: 1}}

	r, err := e.ApplyTaxToLineItems(context.Background(), "t1", "cus_1", "USD", 10000, 5000, lineItems)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.TaxAmountCents != 925 {
		t.Errorf("got tax %d, want 925", r.TaxAmountCents)
	}
}

func TestApplyTaxToLineItems_ProportionalDiscountDistribution(t *testing.T) {
	// $50 + $150 = $200 gross; $20 discount; 10% → $18 tax total.
	// Line sums must match.
	e := newManualEngine(1000, "VAT", nil)
	lineItems := []domain.InvoiceLineItem{
		{AmountCents: 5000, Description: "small", Quantity: 1},
		{AmountCents: 15000, Description: "large", Quantity: 1},
	}

	r, err := e.ApplyTaxToLineItems(context.Background(), "t1", "cus_1", "USD", 20000, 2000, lineItems)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.TaxAmountCents != 1800 {
		t.Errorf("got tax %d, want 1800", r.TaxAmountCents)
	}
	if lineItems[0].TaxAmountCents+lineItems[1].TaxAmountCents != r.TaxAmountCents {
		t.Errorf("line tax sum != invoice tax (%d + %d vs %d)",
			lineItems[0].TaxAmountCents, lineItems[1].TaxAmountCents, r.TaxAmountCents)
	}
}

func TestApplyTaxToLineItems_ProviderResultMapped(t *testing.T) {
	// A provider that returns jurisdictional breakdown data — verify the
	// engine stamps per-line Jurisdiction/TaxCode/TaxRate back onto line
	// items without extra arithmetic.
	provider := &stubProvider{result: &tax.Result{
		Provider:      "stripe_tax",
		CalculationID: "taxcalc_test_123",
		TotalTaxCents: 2000,
		EffectiveRate: 20.00,
		TaxName:       "GST",
		TaxCountry:    "AU",
		Lines: []tax.ResultLine{
			{Ref: "line_0", NetAmountCents: 10000, TaxAmountCents: 2000, TaxRate: 20.00, TaxName: "GST", Jurisdiction: "AU", TaxCode: "txcd_10103001"},
		},
	}}
	e := &Engine{
		settings:     &taxSettings{provider: "stripe_tax", rateBP: 0},
		taxProviders: stubResolver(provider),
	}
	lineItems := []domain.InvoiceLineItem{{AmountCents: 10000, Quantity: 1}}

	r, err := e.ApplyTaxToLineItems(context.Background(), "t1", "cus_1", "USD", 10000, 0, lineItems)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.TaxAmountCents != 2000 || r.TaxName != "GST" || r.TaxCountry != "AU" {
		t.Errorf("got %+v, want 2000 GST AU", r)
	}
	if r.TaxProvider != "stub" || r.TaxCalculationID != "taxcalc_test_123" {
		t.Errorf("got provider=%q calcID=%q, want stub / taxcalc_test_123",
			r.TaxProvider, r.TaxCalculationID)
	}
	if lineItems[0].TaxJurisdiction != "AU" || lineItems[0].TaxCode != "txcd_10103001" {
		t.Errorf("line: juris=%q code=%q, want AU / txcd_10103001",
			lineItems[0].TaxJurisdiction, lineItems[0].TaxCode)
	}
}

func TestApplyTaxToLineItems_ProviderErrorDefersInvoice(t *testing.T) {
	// Provider errors surface under OnFailureBlock — the engine must defer
	// the invoice (tax_status=pending) rather than silently charge the
	// wrong tax. Zero-tax-fallback is the legacy fallback_manual policy
	// and belongs to a different provider code path.
	provider := &stubProvider{err: errors.New("stripe down")}
	e := &Engine{
		settings:     &taxSettings{provider: "stripe_tax"},
		taxProviders: stubResolver(provider),
	}
	lineItems := []domain.InvoiceLineItem{{AmountCents: 10000, Quantity: 1}}

	r, err := e.ApplyTaxToLineItems(context.Background(), "t1", "cus_1", "USD", 10000, 0, lineItems)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.TaxStatus != domain.InvoiceTaxPending {
		t.Errorf("tax_status = %q, want pending", r.TaxStatus)
	}
	if r.TaxPendingReason == "" {
		t.Error("tax_pending_reason should capture the provider error")
	}
	if r.TaxDeferredAt == nil {
		t.Error("tax_deferred_at should be stamped when deferred")
	}
	if r.TaxAmountCents != 0 {
		t.Errorf("got tax %d on deferred, want 0", r.TaxAmountCents)
	}
	if lineItems[0].TotalAmountCents != 10000 {
		t.Errorf("line total = %d, want 10000 when deferred", lineItems[0].TotalAmountCents)
	}
}

func TestApplyTaxToLineItems_NoneProvider(t *testing.T) {
	// tax_provider='none' short-circuits the whole pipeline to zero tax.
	e := &Engine{
		settings: &taxSettings{provider: "none"},
		taxProviders: resolverFunc(func(_ context.Context, _ domain.TenantSettings) (tax.Provider, error) {
			return tax.NewNoneProvider(), nil
		}),
	}
	lineItems := []domain.InvoiceLineItem{{AmountCents: 10000, Quantity: 1}}

	r, err := e.ApplyTaxToLineItems(context.Background(), "t1", "cus_1", "USD", 10000, 0, lineItems)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.TaxAmountCents != 0 {
		t.Errorf("got tax %d, want 0", r.TaxAmountCents)
	}
	if r.TaxProvider != "none" {
		t.Errorf("got provider %q, want none", r.TaxProvider)
	}
}

func TestApplyTaxToLineItems_Inclusive_Simple(t *testing.T) {
	// $118 gross at 18% inclusive → $100 net + $18 tax.
	e := newInclusiveManualEngine(1800, "GST")
	lineItems := []domain.InvoiceLineItem{{AmountCents: 11800, Quantity: 1}}

	r, err := e.ApplyTaxToLineItems(context.Background(), "t1", "cus_1", "USD", 11800, 0, lineItems)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.TaxAmountCents != 1800 {
		t.Errorf("got tax %d, want 1800", r.TaxAmountCents)
	}
	if r.SubtotalCents != 10000 {
		t.Errorf("got subtotal %d, want 10000 (net)", r.SubtotalCents)
	}
	if got := r.SubtotalCents - r.DiscountCents + r.TaxAmountCents; got != 11800 {
		t.Errorf("invariant: got %d, want 11800 (customer paid)", got)
	}
	if lineItems[0].AmountCents != 10000 {
		t.Errorf("line amount = %d, want 10000 (net)", lineItems[0].AmountCents)
	}
	if lineItems[0].TaxAmountCents != 1800 {
		t.Errorf("line tax = %d, want 1800", lineItems[0].TaxAmountCents)
	}
}

func TestApplyTaxToLineItems_Inclusive_ZeroRate(t *testing.T) {
	e := newInclusiveManualEngine(0, "")
	lineItems := []domain.InvoiceLineItem{{AmountCents: 10000, Quantity: 1}}

	r, err := e.ApplyTaxToLineItems(context.Background(), "t1", "cus_1", "USD", 10000, 500, lineItems)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.TaxAmountCents != 0 {
		t.Errorf("got tax %d, want 0", r.TaxAmountCents)
	}
	if r.SubtotalCents != 10000 || r.DiscountCents != 500 {
		t.Errorf("inputs should pass through: subtotal=%d discount=%d", r.SubtotalCents, r.DiscountCents)
	}
}

func TestApplyTaxToLineItems_RoundingReconciliation(t *testing.T) {
	// Three lines at 7.25% produce per-line rounding drift; the residual is
	// apportioned by largest remainder (ADR-046) so line-tax sums match the
	// invoice-level total. This test asserts only the sum invariant; the
	// distribution/no-inversion property is covered in internal/tax.
	e := newManualEngine(725, "VAT", nil)
	lineItems := []domain.InvoiceLineItem{
		{AmountCents: 333, Quantity: 1},
		{AmountCents: 333, Quantity: 1},
		{AmountCents: 334, Quantity: 1},
	}

	r, err := e.ApplyTaxToLineItems(context.Background(), "t1", "cus_1", "USD", 1000, 0, lineItems)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var sum int64
	for _, li := range lineItems {
		sum += li.TaxAmountCents
	}
	if sum != r.TaxAmountCents {
		t.Errorf("line tax sum %d != invoice tax %d", sum, r.TaxAmountCents)
	}
}

func TestApplyTaxToLineItems_NoProvidersFailsLoudly(t *testing.T) {
	// Engine must fail with a clear error when the tax provider
	// resolver isn't wired — pre-fix this silently zero-taxed,
	// which masked production misconfigurations. Tests that need
	// a no-tax shape must wire NoneProvider explicitly.
	e := &Engine{settings: &taxSettings{provider: "manual", rateBP: 1800}}
	lineItems := []domain.InvoiceLineItem{{AmountCents: 10000, Quantity: 1}}

	_, err := e.ApplyTaxToLineItems(context.Background(), "t1", "cus_1", "USD", 10000, 0, lineItems)
	if err == nil {
		t.Fatal("expected error when tax provider resolver is unwired, got nil")
	}
	if !strings.Contains(err.Error(), "tax provider resolver not wired") {
		t.Errorf("error should reference unwired resolver, got: %v", err)
	}
}

// TestRunCycle_TaxErrorAbortsBeforeInvoiceAndCycleAdvance is the regression
// test for the discarded ApplyTaxToLineItems error in billOnePeriod. A tax
// failure (here: an unwired resolver, the same surface as a transient DB blip
// resolving provider credentials) MUST abort the cycle close before any
// invoice is created/finalized and before the billing cycle advances — so the
// next tick retries against an untouched sub.
//
// Pre-fix billOnePeriod read `taxApp, _ :=` and dropped the error, then went
// on to finalize a $0-tax invoice AND advance the cycle, silently swallowing
// the failure. With the error captured and propagated, RunCycle surfaces it,
// no invoice is stored, and the cycle is left untouched.
func TestRunCycle_TaxErrorAbortsBeforeInvoiceAndCycleAdvance(t *testing.T) {
	periodStart := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	nextBilling := periodEnd

	subs := &mockSubs{
		subs: map[string]domain.Subscription{
			"sub_1": {
				ID: "sub_1", TenantID: "t1", CustomerID: "cus_1",
				Items:  []domain.SubscriptionItem{{PlanID: "pln_1", Quantity: 1}},
				Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
				CurrentBillingPeriodStart: &periodStart, CurrentBillingPeriodEnd: &periodEnd,
				NextBillingAt: &nextBilling,
			},
		},
		cycleUpdated: make(map[string]bool),
	}

	usage := &mockUsage{totals: map[string]int64{"mtr_api": 1000}}

	pricing := &mockPricing{
		plans: map[string]domain.Plan{
			"pln_1": {
				ID: "pln_1", Name: "Pro Plan", Currency: "USD",
				BillingInterval: domain.BillingMonthly,
				BaseAmountCents: 4900,
				MeterIDs:        []string{"mtr_api"},
			},
		},
		meters: map[string]domain.Meter{
			"mtr_api": {ID: "mtr_api", Name: "API Calls", Unit: "calls", RatingRuleVersionID: "rrv_api"},
		},
		rules: map[string]domain.RatingRuleVersion{
			"rrv_api": {
				ID: "rrv_api", RuleKey: "api_calls", Version: 1, Mode: domain.PricingFlat,
				FlatAmountCents: decimal.NewFromInt(100),
			},
		},
	}

	invoices := &mockInvoices{}

	fakeClk := clock.NewFake(periodEnd.Add(time.Nanosecond))
	// Deliberately do NOT call wireBaseTax — the tax provider resolver is
	// left unwired so ApplyTaxToLineItems returns an error.
	engine := NewEngine(subs, usage, pricing, invoices, nil, &mockSettings{}, nil, nil, fakeClk)
	engine.SetIntervalReader(subs)

	count, errs := engine.RunCycle(context.Background(), 50)

	if len(errs) == 0 {
		t.Fatal("expected a billing error when tax application fails, got none")
	}
	var sawTaxErr bool
	for _, e := range errs {
		if strings.Contains(e.Error(), "apply tax") {
			sawTaxErr = true
		}
	}
	if !sawTaxErr {
		t.Errorf("expected an 'apply tax' error to surface, got: %v", errs)
	}
	if count != 0 {
		t.Errorf("no invoice should be generated when tax fails, got count=%d", count)
	}
	if len(invoices.invoices) != 0 {
		t.Errorf("no invoice should be stored when tax fails, got %d", len(invoices.invoices))
	}
	if len(invoices.lineItems) != 0 {
		t.Errorf("no line items should be stored when tax fails, got %d", len(invoices.lineItems))
	}
	if subs.cycleUpdated["sub_1"] {
		t.Error("billing cycle must NOT advance when tax fails — the sub must be retried untouched next tick")
	}
}
