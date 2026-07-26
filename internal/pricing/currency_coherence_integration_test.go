package pricing_test

// ADR-100 currency-coherence ring — real-Postgres tests for the pricing-side
// guards (G1 republish, G2 plan create/update, G2b binding writes), each
// exercised in BOTH directions: the divergence refuses loud, the coherent
// twin succeeds, and the archived-plan repair flow stays open.

import (
	"context"
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

type coherenceFixture struct {
	svc      *pricing.Service
	tenantID string
	ctx      context.Context
}

func newCoherenceFixture(t *testing.T) coherenceFixture {
	t.Helper()
	db := testutil.SetupTestDB(t)
	return coherenceFixture{
		svc:      pricing.NewService(pricing.NewPostgresStore(db)),
		tenantID: testutil.CreateTestTenant(t, db, "currency coherence"),
		ctx:      postgres.WithLivemode(context.Background(), false),
	}
}

// usdGraph builds rule(key)->meter(default binding)->plan(USD) and returns ids.
func (f coherenceFixture) usdGraph(t *testing.T, ruleKey, meterKey, planCode string) (ruleID, meterID, planID string) {
	t.Helper()
	rule, err := f.svc.CreateRatingRule(f.ctx, f.tenantID, pricing.CreateRatingRuleInput{
		RuleKey: ruleKey, Name: ruleKey, Mode: domain.PricingFlat,
		Currency: "USD", FlatAmountCents: decimal.RequireFromString("0.5"),
	})
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}
	meter, err := f.svc.CreateMeter(f.ctx, f.tenantID, pricing.CreateMeterInput{
		Key: meterKey, Name: meterKey, Unit: "units", Aggregation: "sum",
		RatingRuleVersionID: rule.ID,
	})
	if err != nil {
		t.Fatalf("create meter: %v", err)
	}
	plan, err := f.svc.CreatePlan(f.ctx, f.tenantID, pricing.CreatePlanInput{
		Code: planCode, Name: planCode, Currency: "USD",
		BillingInterval: domain.BillingMonthly, MeterIDs: []string{meter.ID},
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	return rule.ID, meter.ID, plan.ID
}

func TestCurrencyRing_RepublishBlockedWhilePlanBound(t *testing.T) {
	f := newCoherenceFixture(t)
	f.usdGraph(t, "api_calls", "calls", "pro")

	// EUR republish of a plan-bound key → loud refusal naming the plan.
	_, err := f.svc.CreateRatingRule(f.ctx, f.tenantID, pricing.CreateRatingRuleInput{
		RuleKey: "api_calls", Name: "api calls v2", Mode: domain.PricingFlat,
		Currency: "EUR", FlatAmountCents: decimal.RequireFromString("0.4"),
	})
	if err == nil || !strings.Contains(err.Error(), "pro") {
		t.Fatalf("want plan-naming refusal, got %v", err)
	}

	// Same-currency republish (a plain rate change) stays legal.
	if _, err := f.svc.CreateRatingRule(f.ctx, f.tenantID, pricing.CreateRatingRuleInput{
		RuleKey: "api_calls", Name: "api calls v2", Mode: domain.PricingFlat,
		Currency: "USD", FlatAmountCents: decimal.RequireFromString("0.4"),
	}); err != nil {
		t.Fatalf("same-currency republish must pass: %v", err)
	}
}

func TestCurrencyRing_ArchivedPlanRepairFlow(t *testing.T) {
	f := newCoherenceFixture(t)
	_, _, planID := f.usdGraph(t, "gpu_secs", "gpu", "gpu_plan")

	// Archive the (sub-less) plan → the key unbinds → EUR republish = the
	// documented wrong-currency repair path, must succeed.
	st := "archived"
	if _, err := f.svc.UpdatePlan(f.ctx, f.tenantID, planID, pricing.CreatePlanInput{Status: st}); err != nil {
		t.Fatalf("archive plan: %v", err)
	}
	if _, err := f.svc.CreateRatingRule(f.ctx, f.tenantID, pricing.CreateRatingRuleInput{
		RuleKey: "gpu_secs", Name: "gpu v2", Mode: domain.PricingFlat,
		Currency: "EUR", FlatAmountCents: decimal.RequireFromString("0.3"),
	}); err != nil {
		t.Fatalf("republish after archive (repair flow) must pass: %v", err)
	}
}

func TestCurrencyRing_PlanCreateAndUpdateBlocked(t *testing.T) {
	f := newCoherenceFixture(t)
	_, meterID, _ := f.usdGraph(t, "tokens_usd", "tokens", "base_usd")

	// EUR plan over a USD-ruled meter → refusal naming the key.
	_, err := f.svc.CreatePlan(f.ctx, f.tenantID, pricing.CreatePlanInput{
		Code: "eur_plan", Name: "eur", Currency: "EUR",
		BillingInterval: domain.BillingMonthly, MeterIDs: []string{meterID},
	})
	if err == nil || !strings.Contains(err.Error(), "tokens_usd") {
		t.Fatalf("want key-naming refusal, got %v", err)
	}

	// Meterless EUR plan passes vacuously — then ATTACHING the USD meter via
	// update must refuse (the create-only bypass the panel found).
	eur, err := f.svc.CreatePlan(f.ctx, f.tenantID, pricing.CreatePlanInput{
		Code: "eur_empty", Name: "eur", Currency: "EUR",
		BillingInterval: domain.BillingMonthly,
	})
	if err != nil {
		t.Fatalf("meterless EUR plan: %v", err)
	}
	if _, err := f.svc.UpdatePlan(f.ctx, f.tenantID, eur.ID, pricing.CreatePlanInput{
		MeterIDs: []string{meterID},
	}); err == nil {
		t.Fatal("meter attach on update must refuse a currency mismatch")
	}
}

func TestCurrencyRing_BindingWritesBlocked(t *testing.T) {
	f := newCoherenceFixture(t)
	_, meterID, _ := f.usdGraph(t, "req_usd", "requests", "req_plan")

	eurRule, err := f.svc.CreateRatingRule(f.ctx, f.tenantID, pricing.CreateRatingRuleInput{
		RuleKey: "req_eur", Name: "eur rule", Mode: domain.PricingFlat,
		Currency: "EUR", FlatAmountCents: decimal.RequireFromString("0.2"),
	})
	if err != nil {
		t.Fatalf("eur rule: %v", err)
	}

	// Pricing-rule binding of an EUR key onto the USD meter → refuse.
	if _, err := f.svc.UpsertMeterPricingRule(f.ctx, f.tenantID, pricing.UpsertMeterPricingRuleInput{
		MeterID: meterID, RatingRuleVersionID: eurRule.ID,
	}); err == nil {
		t.Fatal("EUR binding on USD meter must refuse")
	}

	// Default-binding rebind to the EUR rule → refuse.
	rid := eurRule.ID
	if _, err := f.svc.UpdateMeter(f.ctx, f.tenantID, meterID, pricing.UpdateMeterInput{
		RatingRuleVersionID: &rid,
	}); err == nil {
		t.Fatal("EUR default rebind on USD meter must refuse")
	}
}

func TestCurrencyRing_ZeroDecimalRefused(t *testing.T) {
	f := newCoherenceFixture(t)
	_, err := f.svc.CreatePlan(f.ctx, f.tenantID, pricing.CreatePlanInput{
		Code: "jpy_plan", Name: "jpy", Currency: "JPY",
		BillingInterval: domain.BillingMonthly,
	})
	if err == nil || !strings.Contains(err.Error(), "minor unit") {
		t.Fatalf("JPY must refuse with the zero-decimal message, got %v", err)
	}
}
