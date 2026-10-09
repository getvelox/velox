package billing

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
)

var errSettingsDown = errors.New("settings read: connection reset")

// flakySettings returns ts on every Get except the failOn-th call (1-based),
// which fails. failOn = 0 never fails. calls counts every Get, so a clean run
// tells a test how many settings reads one invoice write makes.
type flakySettings struct {
	mockSettings
	ts     domain.TenantSettings
	failOn int
	calls  int
}

func (f *flakySettings) Get(_ context.Context, _ string) (domain.TenantSettings, error) {
	f.calls++
	if f.calls == f.failOn {
		return domain.TenantSettings{}, errSettingsDown
	}
	return f.ts, nil
}

func defaultTermsSettings() domain.TenantSettings {
	return domain.TenantSettings{NetPaymentTerms: 30, Timezone: "UTC", TaxProvider: "none"}
}

// failEachSettingsRead runs write once cleanly to count its settings reads,
// then once per read with that read failing, and asserts every failing run
// returns an error and writes no invoice. It is order-independent: a read
// added later is covered without editing the test (P24 — no silent defaults
// in billing).
func failEachSettingsRead(t *testing.T, write func(s *flakySettings) (written int, err error)) {
	t.Helper()
	clean := &flakySettings{ts: defaultTermsSettings()}
	n, err := write(clean)
	if err != nil {
		t.Fatalf("clean run: %v", err)
	}
	if n == 0 {
		t.Fatal("clean run wrote no invoice; the fixture does not exercise the writer")
	}
	if clean.calls == 0 {
		t.Fatal("clean run made no settings reads")
	}
	for k := 1; k <= clean.calls; k++ {
		s := &flakySettings{ts: defaultTermsSettings(), failOn: k}
		written, err := write(s)
		if err == nil {
			t.Errorf("settings read %d of %d failed but the write succeeded (a silent default was used)", k, clean.calls)
		}
		if written != 0 {
			t.Errorf("settings read %d of %d failed but %d invoice(s) were written", k, clean.calls, written)
		}
	}
}

func TestRunCycle_EverySettingsReadFailsClosed(t *testing.T) {
	failEachSettingsRead(t, func(s *flakySettings) (int, error) {
		engine, subs, _, _, invoices := setupEngine()
		engine.settings = s
		_, runErrs := engine.RunCycle(context.Background(), 50)
		if len(runErrs) > 0 {
			if subs.cycleUpdated["sub_1"] {
				t.Errorf("read %d failed but the billing period advanced", s.failOn)
			}
			return len(invoices.invoices), runErrs[0]
		}
		return len(invoices.invoices), nil
	})
}

func TestRunCycle_HonorsDueOnReceipt(t *testing.T) {
	engine, _, _, _, invoices := setupEngine()
	ts := defaultTermsSettings()
	ts.NetPaymentTerms = 0
	engine.settings = &flakySettings{ts: ts}

	if _, runErrs := engine.RunCycle(context.Background(), 50); len(runErrs) > 0 {
		t.Fatalf("RunCycle: %v", runErrs)
	}
	if len(invoices.invoices) != 1 {
		t.Fatalf("invoices: got %d, want 1", len(invoices.invoices))
	}
	inv := invoices.invoices[0]
	if inv.NetPaymentTermDays != 0 {
		t.Errorf("NetPaymentTermDays: got %d, want 0 (Due on receipt; was coerced to 30)", inv.NetPaymentTermDays)
	}
	if inv.DueAt == nil || inv.IssuedAt == nil || !inv.DueAt.Equal(*inv.IssuedAt) {
		t.Errorf("DueAt = %v, want IssuedAt %v", inv.DueAt, inv.IssuedAt)
	}
}

// cancelFixture is a canceled usage-only sub with 500 billable calls, so the
// final cancel invoice has something to bill.
func cancelFixture(settings SettingsReader) (*Engine, domain.Subscription, *mockInvoices) {
	periodStart := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	cancelAt := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)
	sub := domain.Subscription{
		ID: "sub_1", TenantID: "t1", CustomerID: "cus_1", Code: "api",
		Status:                    domain.SubscriptionCanceled,
		Items:                     []domain.SubscriptionItem{{ID: "si_1", PlanID: "pln_api", Quantity: 1}},
		CurrentBillingPeriodStart: &periodStart,
		CurrentBillingPeriodEnd:   &periodEnd,
		CanceledAt:                &cancelAt,
	}
	pricing := &mockPricing{
		plans: map[string]domain.Plan{
			"pln_api": {ID: "pln_api", Name: "API", Currency: "USD",
				BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInArrears,
				MeterIDs: []string{"mtr_api"}},
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
	invoices := &mockInvoices{}
	engine := wireBaseTax(NewEngine(&mockSubs{subs: map[string]domain.Subscription{sub.ID: sub}},
		&mockUsage{totals: map[string]int64{"mtr_api": 500}}, pricing, invoices, nil, settings, nil, nil, billingTestClock()))
	return engine, sub, invoices
}

func TestBillFinalOnImmediateCancel_EverySettingsReadFailsClosed(t *testing.T) {
	failEachSettingsRead(t, func(s *flakySettings) (int, error) {
		engine, sub, invoices := cancelFixture(s)
		_, err := engine.BillFinalOnImmediateCancel(context.Background(), sub)
		return len(invoices.invoices), err
	})
}

// onCreateFixture is a fresh in_advance sub whose create invoice bills the
// full monthly base.
func onCreateFixture(settings SettingsReader) (*Engine, domain.Subscription, *mockInvoices) {
	now := time.Date(2026, 6, 19, 0, 0, 0, 0, time.UTC)
	periodEnd := now.AddDate(0, 1, 0)
	pricing := &mockPricing{plans: map[string]domain.Plan{
		"p_m_adv": {ID: "p_m_adv", Name: "Monthly Adv", Currency: "USD", BillingInterval: domain.BillingMonthly,
			BaseBillTiming: domain.BillInAdvance, BaseAmountCents: 12000},
	}}
	invoices := &mockInvoices{}
	engine := wireBaseTax(NewEngine(&mockSubs{subs: map[string]domain.Subscription{}}, &mockUsage{}, pricing, invoices, nil, settings, nil, nil, clock.NewFake(now)))
	sub := domain.Subscription{
		ID: "sub_x", TenantID: "t1", CustomerID: "c1",
		Status:                    domain.SubscriptionActive,
		BillingTime:               domain.BillingTimeAnniversary,
		CurrentBillingPeriodStart: &now,
		CurrentBillingPeriodEnd:   &periodEnd,
		Items:                     []domain.SubscriptionItem{{ID: "si_1", PlanID: "p_m_adv", Quantity: 1}},
	}
	return engine, sub, invoices
}

func TestBillOnCreateTx_EverySettingsReadFailsClosed(t *testing.T) {
	failEachSettingsRead(t, func(s *flakySettings) (int, error) {
		engine, sub, invoices := onCreateFixture(s)
		_, _, err := engine.BillOnCreateTx(context.Background(), nil, sub)
		return len(invoices.invoices), err
	})
}

func TestBillOnCreateTx_HonorsDueOnReceipt(t *testing.T) {
	ts := defaultTermsSettings()
	ts.NetPaymentTerms = 0
	engine, sub, _ := onCreateFixture(&flakySettings{ts: ts})

	inv, ok, err := engine.BillOnCreateTx(context.Background(), nil, sub)
	if err != nil || !ok {
		t.Fatalf("BillOnCreateTx: ok=%v err=%v", ok, err)
	}
	if inv.NetPaymentTermDays != 0 {
		t.Errorf("NetPaymentTermDays: got %d, want 0 (Due on receipt; was coerced to 30)", inv.NetPaymentTermDays)
	}
	if inv.DueAt == nil || inv.IssuedAt == nil || !inv.DueAt.Equal(*inv.IssuedAt) {
		t.Errorf("DueAt = %v, want IssuedAt %v", inv.DueAt, inv.IssuedAt)
	}
}

// A timezone the settings row names but this host cannot load fails the close
// rather than billing the period in UTC.
func TestRunCycle_UnloadableTimezoneFailsClosed(t *testing.T) {
	engine, subs, _, _, invoices := setupEngine()
	ts := defaultTermsSettings()
	ts.Timezone = "Mars/Olympus_Mons"
	engine.settings = &flakySettings{ts: ts}

	if _, runErrs := engine.RunCycle(context.Background(), 50); len(runErrs) == 0 {
		t.Fatal("RunCycle succeeded with an unloadable timezone; it must fail, not fall back to UTC")
	}
	if len(invoices.invoices) != 0 || subs.cycleUpdated["sub_1"] {
		t.Errorf("unloadable timezone: invoices=%d advanced=%v, want 0/false", len(invoices.invoices), subs.cycleUpdated["sub_1"])
	}
}
