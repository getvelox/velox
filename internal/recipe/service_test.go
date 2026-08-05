package recipe

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/shopspring/decimal"
)

// memStore is a non-tx in-memory fake of recipe.Store used by unit tests
// that exercise the registry-only methods (Preview, GetRecipe,
// ListRecipes). The tx-bearing path (Instantiate) is covered by
// service_integration_test.go because it needs a real *sql.Tx.
type memStore struct {
	byKey map[string]domain.RecipeInstance
}

func newMemStore() *memStore {
	return &memStore{byKey: make(map[string]domain.RecipeInstance)}
}

func (m *memStore) GetByKey(_ context.Context, _ string, recipeKey string) (domain.RecipeInstance, error) {
	inst, ok := m.byKey[recipeKey]
	if !ok {
		return domain.RecipeInstance{}, errs.ErrNotFound
	}
	return inst, nil
}

func (m *memStore) List(_ context.Context, _ string) ([]domain.RecipeInstance, error) {
	out := make([]domain.RecipeInstance, 0, len(m.byKey))
	for _, v := range m.byKey {
		out = append(out, v)
	}
	return out, nil
}

func (m *memStore) GetByKeyTx(_ context.Context, _ *sql.Tx, _ string, recipeKey string) (domain.RecipeInstance, error) {
	inst, ok := m.byKey[recipeKey]
	if !ok {
		return domain.RecipeInstance{}, errs.ErrNotFound
	}
	return inst, nil
}

func (m *memStore) CreateTx(_ context.Context, _ *sql.Tx, inst domain.RecipeInstance) (domain.RecipeInstance, error) {
	inst.ID = "vlx_rci_test_" + inst.RecipeKey
	m.byKey[inst.RecipeKey] = inst
	return inst, nil
}

// loadRegistry loads the embedded recipes for tests that need a real
// rendered recipe graph. Failing here means the bundled YAML is broken —
// surface it as a t.Fatal rather than masking with a nil registry.
func loadRegistry(t *testing.T) *Registry {
	t.Helper()
	reg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	return reg
}

func TestService_GetRecipe(t *testing.T) {
	svc := NewService(nil, newMemStore(), loadRegistry(t), nil, nil, nil)

	r, err := svc.GetRecipe("anthropic_style")
	if err != nil {
		t.Fatalf("GetRecipe: %v", err)
	}
	if r.Key != "anthropic_style" {
		t.Errorf("Key: got %q, want anthropic_style", r.Key)
	}
	if len(r.Overridable) == 0 {
		t.Error("expected overridable keys on anthropic_style")
	}

	if _, err := svc.GetRecipe("does_not_exist"); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestService_Preview_RendersTemplates(t *testing.T) {
	svc := NewService(nil, newMemStore(), loadRegistry(t), nil, nil, nil)

	rendered, err := svc.Preview(context.Background(), "anthropic_style", map[string]any{
		"currency":  "EUR",
		"plan_code": "ai_eur",
		"plan_name": "AI EUR",
	})
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if rendered.Key != "anthropic_style" {
		t.Errorf("Key: got %q, want anthropic_style", rendered.Key)
	}
	if len(rendered.Objects.Plans) != 1 {
		t.Fatalf("expected 1 plan, got %d", len(rendered.Objects.Plans))
	}
	if got := rendered.Objects.Plans[0].Code; got != "ai_eur" {
		t.Errorf("plan.code: got %q, want ai_eur", got)
	}
	if got := rendered.Objects.Plans[0].Currency; got != "EUR" {
		t.Errorf("plan.currency: got %q, want EUR", got)
	}
	if got := rendered.Objects.RatingRules[0].Currency; got != "EUR" {
		t.Errorf("rating_rule.currency: got %q, want EUR", got)
	}
	if rendered.Warnings == nil {
		t.Error("Warnings must be non-nil so JSON serializes as [] not null")
	}
}

func TestService_Preview_UnknownRecipe(t *testing.T) {
	svc := NewService(nil, newMemStore(), loadRegistry(t), nil, nil, nil)
	if _, err := svc.Preview(context.Background(), "no_such_recipe", nil); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestService_Preview_RejectsBadOverride(t *testing.T) {
	svc := NewService(nil, newMemStore(), loadRegistry(t), nil, nil, nil)

	// "currency" enum is [USD, EUR, GBP]; "JPY" must fail.
	_, err := svc.Preview(context.Background(), "anthropic_style", map[string]any{
		"currency": "JPY",
	})
	if err == nil {
		t.Fatal("expected enum-violation error")
	}
	if !errors.Is(err, errs.ErrValidation) {
		t.Errorf("expected validation error, got %T (%v)", err, err)
	}
}

func TestService_ListRecipes_TagsInstalled(t *testing.T) {
	store := newMemStore()
	store.byKey["anthropic_style"] = domain.RecipeInstance{
		ID: "vlx_rci_existing", TenantID: "t1",
		RecipeKey: "anthropic_style", RecipeVersion: "1.0.0",
	}
	svc := NewService(nil, store, loadRegistry(t), nil, nil, nil)

	items, err := svc.ListRecipes(context.Background(), "t1")
	if err != nil {
		t.Fatalf("ListRecipes: %v", err)
	}
	if len(items) == 0 {
		t.Fatal("expected at least one recipe")
	}
	var found bool
	for _, item := range items {
		if item.Key == "anthropic_style" {
			found = true
			if item.Instantiated == nil {
				t.Error("expected Instantiated to be populated for installed recipe")
			} else if item.Instantiated.ID != "vlx_rci_existing" {
				t.Errorf("Instantiated.ID: got %q, want vlx_rci_existing", item.Instantiated.ID)
			}
		} else if item.Instantiated != nil {
			t.Errorf("recipe %q should be uninstalled but Instantiated is set", item.Key)
		}
	}
	if !found {
		t.Error("anthropic_style not in list")
	}
}

func TestService_Instantiate_UnknownRecipe(t *testing.T) {
	svc := NewService(nil, newMemStore(), loadRegistry(t), nil, nil, nil)

	_, err := svc.Instantiate(context.Background(), "t1", "no_such_recipe", nil, InstantiateOptions{})
	if !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// TestWireShape_SnakeCase pins the JSON contract Track B's picker UI
// depends on. Drift here (e.g. dropping a json:"…" tag and falling back
// to PascalCase) breaks the dashboard at runtime, so we assert the
// snake_case field names + creates summary + preview wrapper directly.
func TestWireShape_SnakeCase(t *testing.T) {
	svc := NewService(nil, newMemStore(), loadRegistry(t), nil, nil, nil)

	t.Run("ListRecipes", func(t *testing.T) {
		items, err := svc.ListRecipes(context.Background(), "t1")
		if err != nil {
			t.Fatalf("ListRecipes: %v", err)
		}
		blob, err := json.Marshal(items[0])
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var raw map[string]any
		if err := json.Unmarshal(blob, &raw); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		for _, k := range []string{"key", "version", "name", "summary", "overridable", "meters", "rating_rules", "pricing_rules", "plans", "creates"} {
			if _, ok := raw[k]; !ok {
				t.Errorf("list response missing %q key (raw=%v)", k, raw)
			}
		}
		for _, k := range []string{"Key", "Version", "Name", "Summary", "RatingRules", "PricingRules", "DunningPolicy"} {
			if _, ok := raw[k]; ok {
				t.Errorf("list response should not have PascalCase key %q", k)
			}
		}
		creates, ok := raw["creates"].(map[string]any)
		if !ok {
			t.Fatalf("creates not an object: %T", raw["creates"])
		}
		for _, k := range []string{"meters", "rating_rules", "pricing_rules", "plans", "dunning_policies", "webhook_endpoints"} {
			if _, ok := creates[k]; !ok {
				t.Errorf("creates missing %q", k)
			}
		}
	})

	t.Run("GetRecipe", func(t *testing.T) {
		detail, err := svc.GetRecipe("anthropic_style")
		if err != nil {
			t.Fatalf("GetRecipe: %v", err)
		}
		blob, err := json.Marshal(detail)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var raw map[string]any
		if err := json.Unmarshal(blob, &raw); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		for _, k := range []string{"key", "version", "name", "summary", "description", "creates"} {
			if _, ok := raw[k]; !ok {
				t.Errorf("detail response missing %q (raw keys=%v)", k, mapKeys(raw))
			}
		}
	})

	t.Run("Preview", func(t *testing.T) {
		out, err := svc.Preview(context.Background(), "anthropic_style", nil)
		if err != nil {
			t.Fatalf("Preview: %v", err)
		}
		blob, err := json.Marshal(out)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var raw map[string]any
		if err := json.Unmarshal(blob, &raw); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		for _, k := range []string{"key", "version", "objects", "warnings"} {
			if _, ok := raw[k]; !ok {
				t.Errorf("preview response missing %q (raw keys=%v)", k, mapKeys(raw))
			}
		}
		objects, ok := raw["objects"].(map[string]any)
		if !ok {
			t.Fatalf("objects not an object: %T", raw["objects"])
		}
		for _, k := range []string{"meters", "rating_rules", "pricing_rules", "plans", "dunning_policies", "webhook_endpoints"} {
			if _, ok := objects[k]; !ok {
				t.Errorf("preview.objects missing %q", k)
			}
		}
		if _, isSlice := raw["warnings"].([]any); !isSlice {
			t.Errorf("warnings must marshal as JSON array, got %T", raw["warnings"])
		}
	})
}

func mapKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestRatingRuleFromRecipe(t *testing.T) {
	got := ratingRuleFromRecipe(domain.RecipeRatingRule{
		Key: "tier1", Mode: domain.PricingFlat, Currency: "USD", FlatAmountCents: decimal.NewFromInt(100),
	})
	if got.RuleKey != "tier1" {
		t.Errorf("RuleKey: got %q", got.RuleKey)
	}
	if got.Name != "tier1" {
		t.Errorf("Name should default to Key: got %q", got.Name)
	}
	if got.Version != 0 {
		t.Errorf("Version: got %d, want 0 (allocated in SQL by the store — MAX+1 per key, so reinstall lands the next version instead of 409ing; ADR-070)", got.Version)
	}
	if got.LifecycleState != domain.RatingRuleActive {
		t.Errorf("LifecycleState: got %q, want active", got.LifecycleState)
	}
}

func TestDunningFromRecipe(t *testing.T) {
	got := dunningFromRecipe(domain.RecipeDunningPolicy{
		Name: "Test", MaxRetries: 4, IntervalsHours: []int{24, 72},
		FinalSubscriptionAction: "cancel", FinalInvoiceAction: "mark_uncollectible",
	})
	if !got.Enabled {
		t.Error("Enabled should be true")
	}
	if got.MaxRetryAttempts != 4 {
		t.Errorf("MaxRetryAttempts: got %d, want 4", got.MaxRetryAttempts)
	}
	// Discriminating fixture: cancel+write-off is the combination the old
	// single enum could NOT express (ADR-112), so a translation that dropped
	// either half would fail here. A pause/none pair would not have.
	if got.FinalSubscriptionAction != domain.SubActionCancel {
		t.Errorf("FinalSubscriptionAction: got %q, want cancel", got.FinalSubscriptionAction)
	}
	if got.FinalInvoiceAction != domain.InvActionMarkUncollectible {
		t.Errorf("FinalInvoiceAction: got %q, want mark_uncollectible", got.FinalInvoiceAction)
	}
	if len(got.RetrySchedule) != 2 || got.RetrySchedule[0] != "24h" || got.RetrySchedule[1] != "72h" {
		t.Errorf("RetrySchedule: got %v, want [24h 72h]", got.RetrySchedule)
	}
	if got.GracePeriodDays != 3 {
		t.Errorf("GracePeriodDays default: got %d, want 3", got.GracePeriodDays)
	}
}

// TestDunningFromRecipe_NoSilentDefault replaces a test that asserted the
// opposite. dunningFromRecipe used to default an absent final action to
// manual_review; ADR-112 moved that decision to parse-time validation, which
// REFUSES a recipe missing either half.
//
// Defaulting here would be the silent fallback the recipe parser now exists
// to prevent: a recipe author who omits the key gets an error naming both
// fields, not a terminal action nobody chose.
func TestDunningFromRecipe_NoSilentDefault(t *testing.T) {
	got := dunningFromRecipe(domain.RecipeDunningPolicy{Name: "T", MaxRetries: 1})
	if got.FinalSubscriptionAction != "" || got.FinalInvoiceAction != "" {
		t.Errorf("got (%q, %q) — dunningFromRecipe must not invent a terminal action; parse rejects the recipe instead",
			got.FinalSubscriptionAction, got.FinalInvoiceAction)
	}
}
