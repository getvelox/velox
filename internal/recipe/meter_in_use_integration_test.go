package recipe

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// D05 in a recipe apply: adopting a meter that a live subscription uses is
// refused when the recipe's prices would take usage from a price that bills
// it now (here, the meter's default price), with the same meter_in_use code
// as the pricing endpoints. A recipe whose prices only price unpriced usage
// on that meter still installs, which is what lets a second AI recipe adopt
// the shared tokens meter (ADR-085).

// liveTokensMeter makes a tokens meter (sum, as the recipes want) on a plan
// with one active subscription. defaultCents > 0 also gives it a default
// price; existingModel != "" adds a rule {model: existingModel}.
func liveTokensMeter(t *testing.T, f *recipeIntegrationFixture, ctx context.Context, tenantID string, defaultCents int64, existingModel string) domain.Meter {
	t.Helper()
	return liveMeter(t, f, ctx, tenantID, "sum", defaultCents, existingModel)
}

// liveMeter is liveTokensMeter with the meter's aggregation.
func liveMeter(t *testing.T, f *recipeIntegrationFixture, ctx context.Context, tenantID, aggregation string, defaultCents int64, existingModel string) domain.Meter {
	t.Helper()
	defaultID := ""
	if defaultCents > 0 {
		r, err := f.pricingSvc.CreateRatingRule(ctx, tenantID, pricing.CreateRatingRuleInput{
			RuleKey: "tokens_default", Name: "Tokens default", Mode: domain.PricingFlat,
			Currency: "USD", FlatAmountCents: decimal.NewFromInt(defaultCents),
		})
		if err != nil {
			t.Fatalf("default rule: %v", err)
		}
		defaultID = r.ID
	}
	m, err := f.pricingSvc.CreateMeter(ctx, tenantID, pricing.CreateMeterInput{
		EventName: "tokens", Name: "Tokens", Unit: "tokens", Aggregation: aggregation, RatingRuleVersionID: defaultID,
	})
	if err != nil {
		t.Fatalf("meter: %v", err)
	}
	if existingModel != "" {
		r, err := f.pricingSvc.CreateRatingRule(ctx, tenantID, pricing.CreateRatingRuleInput{
			RuleKey: "my_model", Name: "My model", Mode: domain.PricingFlat,
			Currency: "USD", FlatAmountCents: decimal.NewFromInt(1),
		})
		if err != nil {
			t.Fatalf("model rule: %v", err)
		}
		if _, err := f.pricingSvc.UpsertMeterPricingRule(ctx, tenantID, pricing.UpsertMeterPricingRuleInput{
			MeterID: m.ID, RatingRuleVersionID: r.ID, DimensionMatch: map[string]any{"model": existingModel}, Priority: 100,
		}); err != nil {
			t.Fatalf("bind model rule: %v", err)
		}
	}
	p, err := f.pricingSvc.CreatePlan(ctx, tenantID, pricing.CreatePlanInput{
		Code: "operator_plan", Name: "Operator plan", Currency: "USD",
		BillingInterval: domain.BillingMonthly, MeterIDs: []string{m.ID},
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	tx, err := f.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	now := time.Now().UTC()
	custID := postgres.NewID("vlx_cus")
	subID := postgres.NewID("vlx_sub")
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO customers (id, tenant_id, external_id, display_name, created_at, updated_at) VALUES ($1, $2, $3, $3, $4, $4)`,
			[]any{custID, tenantID, "ext_" + custID, now}},
		{`INSERT INTO subscriptions (id, tenant_id, code, display_name, customer_id, status, billing_time,
			current_billing_period_start, current_billing_period_end, next_billing_at, created_at, updated_at)
			VALUES ($1, $2, $3, $3, $4, 'active', 'anniversary', $5, $6, $6, $5, $5)`,
			[]any{subID, tenantID, "code_" + subID, custID, now, now.AddDate(0, 1, 0)}},
		{`INSERT INTO subscription_items (id, tenant_id, subscription_id, plan_id, quantity, metadata, created_at, updated_at)
			VALUES ($1, $2, $3, $4, 1, '{}'::jsonb, $5, $5)`,
			[]any{postgres.NewID("vlx_si"), tenantID, subID, p.ID, now}},
	} {
		if _, err := tx.ExecContext(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("seed subscription: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return m
}

func TestService_Instantiate_RefusesPricesThatRepriceAMeterInUse(t *testing.T) {
	f := newRecipeFixture(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, f.db, "recipe meter in use")
	liveTokensMeter(t, f, ctx, tenantID, 1, "")

	_, err := f.svc.Instantiate(ctx, tenantID, "anthropic_style", nil, InstantiateOptions{})
	if !errors.Is(err, errs.ErrInvalidState) || errs.Code(err) != "meter_in_use" {
		t.Fatalf("want ErrInvalidState with code meter_in_use, got %v (code %q)", err, errs.Code(err))
	}
	if !strings.HasPrefix(err.Error(), "1 subscription uses meter") {
		t.Errorf("the refusal must be the pricing message itself, got %q", err.Error())
	}
	// The whole apply rolled back: no pricing rule, no plan, no badge.
	if n := countRows(t, f.db, tenantID, "meter_pricing_rules"); n != 0 {
		t.Errorf("meter_pricing_rules after refusal: got %d, want 0", n)
	}
	if n := countRows(t, f.db, tenantID, "plans"); n != 1 {
		t.Errorf("plans after refusal: got %d, want 1 (the operator's)", n)
	}
	if n := countRows(t, f.db, tenantID, "recipe_instances"); n != 0 {
		t.Errorf("recipe_instances after refusal: got %d, want 0", n)
	}
}

func TestService_Instantiate_AddsUnpricedModelsToAMeterInUse(t *testing.T) {
	f := newRecipeFixture(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, f.db, "recipe meter in use disjoint")
	m := liveTokensMeter(t, f, ctx, tenantID, 0, "my-model")

	if _, err := f.svc.Instantiate(ctx, tenantID, "anthropic_style", nil, InstantiateOptions{}); err != nil {
		t.Fatalf("prices for models no rule prices must install on a meter in use, got %v", err)
	}
	rules, err := f.pricingSvc.ListMeterPricingRulesByMeter(ctx, tenantID, m.ID)
	if err != nil {
		t.Fatalf("list rules: %v", err)
	}
	if len(rules) < 2 {
		t.Errorf("recipe rules must be added beside the operator's, have %d", len(rules))
	}
}

// TestService_Instantiate_AggregationConflictOnAMeterInUse: the recipe's
// refusal for a meter that adds up usage another way says when its
// aggregation can still change: until the meter has usage, whether or not
// subscriptions use it (ADR-130 §2).
func TestService_Instantiate_AggregationConflictOnAMeterInUse(t *testing.T) {
	f := newRecipeFixture(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, f.db, "recipe meter aggregation in use")
	m := liveMeter(t, f, ctx, tenantID, "max", 1, "")

	_, err := f.svc.Instantiate(ctx, tenantID, "anthropic_style", nil, InstantiateOptions{})
	if !errors.Is(err, errs.ErrAlreadyExists) {
		t.Fatalf("want ErrAlreadyExists (meter conflict), got %v", err)
	}
	if !strings.Contains(err.Error(), "Its aggregation can change only until the meter has a usage event") {
		t.Errorf("the refusal must say the aggregation can change only until the meter has a usage event, got %q", err.Error())
	}
	sum := "sum"
	if _, err := f.pricingSvc.UpdateMeter(ctx, tenantID, m.ID, pricing.UpdateMeterInput{Aggregation: &sum}); err != nil {
		t.Fatalf("changing the aggregation of a meter with no usage, in use or not: %v", err)
	}
}
