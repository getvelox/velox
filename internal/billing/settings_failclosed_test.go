package billing

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/tax"
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

// recordingTaxCalcs counts tax_calculations rows. A write that fails after a
// row is recorded leaves an orphan row behind.
type recordingTaxCalcs struct{ records int }

func (r *recordingTaxCalcs) Record(context.Context, string, string, tax.Request, *tax.Result) (string, error) {
	r.records++
	return "taxcalc_test", nil
}
func (r *recordingTaxCalcs) LookupCalculationCreatedAt(context.Context, string, string, string) (time.Time, error) {
	return time.Time{}, nil
}
func (r *recordingTaxCalcs) LinkInvoice(context.Context, string, string, string) error { return nil }

func defaultTermsSettings() domain.TenantSettings {
	return domain.TenantSettings{NetPaymentTerms: 30, Timezone: "UTC", TaxProvider: "none"}
}

// failEachSettingsRead runs write once cleanly to count its settings reads,
// then once per read with that read failing, and asserts every failing run
// returns an error, writes no invoice and records no tax calculation (every
// settings read happens before tax is computed). It is order-independent: a
// read added later is covered without editing the test (P24 — no silent
// defaults in billing).
func failEachSettingsRead(t *testing.T, write func(s *flakySettings, taxCalcs *recordingTaxCalcs) (written int, err error)) {
	t.Helper()
	clean := &flakySettings{ts: defaultTermsSettings()}
	cleanTax := &recordingTaxCalcs{}
	n, err := write(clean, cleanTax)
	if err != nil {
		t.Fatalf("clean run: %v", err)
	}
	if n == 0 {
		t.Fatal("clean run wrote no invoice; the fixture does not exercise the writer")
	}
	if clean.calls == 0 {
		t.Fatal("clean run made no settings reads")
	}
	if cleanTax.records == 0 {
		t.Fatal("clean run recorded no tax calculation; the fixture does not reach tax")
	}
	for k := 1; k <= clean.calls; k++ {
		s := &flakySettings{ts: defaultTermsSettings(), failOn: k}
		taxCalcs := &recordingTaxCalcs{}
		written, err := write(s, taxCalcs)
		if err == nil {
			t.Errorf("settings read %d of %d failed but the write succeeded (a silent default was used)", k, clean.calls)
		}
		if written != 0 {
			t.Errorf("settings read %d of %d failed but %d invoice(s) were written", k, clean.calls, written)
		}
		if taxCalcs.records != 0 {
			t.Errorf("settings read %d of %d failed after tax was recorded (%d orphan tax_calculations rows)", k, clean.calls, taxCalcs.records)
		}
	}
}

func TestRunCycle_EverySettingsReadFailsClosed(t *testing.T) {
	failEachSettingsRead(t, func(s *flakySettings, taxCalcs *recordingTaxCalcs) (int, error) {
		engine, subs, _, _, invoices := setupEngine()
		engine.settings = s
		engine.SetTaxCalculationStore(taxCalcs)
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
	failEachSettingsRead(t, func(s *flakySettings, taxCalcs *recordingTaxCalcs) (int, error) {
		engine, sub, invoices := cancelFixture(s)
		engine.SetTaxCalculationStore(taxCalcs)
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
	failEachSettingsRead(t, func(s *flakySettings, taxCalcs *recordingTaxCalcs) (int, error) {
		engine, sub, invoices := onCreateFixture(s)
		engine.SetTaxCalculationStore(taxCalcs)
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

// thresholdFixture crosses a $500 spend threshold mid-period: 1000 calls at
// $1 plus the $49 base.
func thresholdFixture(t *testing.T, reset bool, settings SettingsReader, taxCalcs *recordingTaxCalcs) (*Engine, *mockInvoices) {
	t.Helper()
	engine, _, invoices := setupThresholdEngine(&domain.BillingThresholds{AmountGTE: 50000, ResetBillingCycle: reset}, 1000)
	engine.clock = clock.NewFake(time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC))
	engine.settings = settings
	if taxCalcs != nil {
		engine.SetTaxCalculationStore(taxCalcs)
	}
	return engine, invoices
}

func TestScanThresholds_EverySettingsReadFailsClosed(t *testing.T) {
	for _, reset := range []bool{false, true} {
		t.Run(map[bool]string{false: "continue_cycle", true: "reset_cycle"}[reset], func(t *testing.T) {
			failEachSettingsRead(t, func(s *flakySettings, taxCalcs *recordingTaxCalcs) (int, error) {
				engine, invoices := thresholdFixture(t, reset, s, taxCalcs)
				_, errs := engine.ScanThresholds(context.Background(), 50)
				if len(errs) > 0 {
					return len(invoices.invoices), errs[0]
				}
				return len(invoices.invoices), nil
			})
		})
	}
}

func TestScanThresholds_HonorsDueOnReceipt(t *testing.T) {
	ts := defaultTermsSettings()
	ts.NetPaymentTerms = 0
	engine, invoices := thresholdFixture(t, false, &flakySettings{ts: ts}, nil)

	if fired, errs := engine.ScanThresholds(context.Background(), 50); fired != 1 || len(errs) > 0 {
		t.Fatalf("ScanThresholds: fired=%d errs=%v, want one fire", fired, errs)
	}
	inv := invoices.invoices[0]
	if inv.NetPaymentTermDays != 0 {
		t.Errorf("NetPaymentTermDays: got %d, want 0 (Due on receipt; was coerced to 30)", inv.NetPaymentTermDays)
	}
	if inv.DueAt == nil || inv.IssuedAt == nil || !inv.DueAt.Equal(*inv.IssuedAt) {
		t.Errorf("DueAt = %v, want IssuedAt %v", inv.DueAt, inv.IssuedAt)
	}
}

// A trial that is still running advances its period without billing; that
// advance is computed in the tenant zone, so a failed read must not advance
// the period in UTC.
func TestRunCycle_TrialAdvanceTimezoneUnreadableFailsClosed(t *testing.T) {
	engine, subs, _, _, invoices := setupEngine()
	sub := subs.subs["sub_1"]
	trialEnd := sub.CurrentBillingPeriodEnd.AddDate(0, 3, 0)
	sub.Status = domain.SubscriptionTrialing
	sub.TrialEndAt = &trialEnd
	subs.subs["sub_1"] = sub
	engine.settings = &flakySettings{ts: defaultTermsSettings(), failOn: 1}

	if _, runErrs := engine.RunCycle(context.Background(), 50); len(runErrs) == 0 {
		t.Fatal("trial advance succeeded with an unreadable timezone; want an error")
	}
	if subs.cycleUpdated["sub_1"] || len(invoices.invoices) != 0 {
		t.Errorf("trial period advanced (%v) or invoices written (%d) despite the failure", subs.cycleUpdated["sub_1"], len(invoices.invoices))
	}
}
