package domain

import (
	"time"

	"github.com/shopspring/decimal"
)

// RecipeInstance is the DB-backed record of a recipe being instantiated for
// a tenant. The canonical entities a recipe creates (meters, rating rules,
// pricing rules, plans, dunning policies, webhook endpoints) live in their
// own per-domain tables; RecipeInstance only tracks instantiation metadata
// so we can answer "is this recipe already installed?" idempotently and
// record what it created (CreatedObjects) for the dashboard's install badge.
// (ADR-085: apply is idempotent additive — no force, no uninstall.)
//
// See docs/design-recipes.md.
type RecipeInstance struct {
	ID             string         `json:"id"`
	TenantID       string         `json:"tenant_id,omitempty"`
	RecipeKey      string         `json:"recipe_key"`
	RecipeVersion  string         `json:"recipe_version"`
	Overrides      map[string]any `json:"overrides"`
	CreatedObjects CreatedObjects `json:"created_objects"`
	CreatedAt      time.Time      `json:"created_at"`
	CreatedBy      string         `json:"created_by,omitempty"`
}

// CreatedObjects is the per-role map of entity IDs a recipe apply produced.
// Persisted as JSONB on recipe_instances.created_object_ids and returned in
// the apply response so the dashboard can deep-link directly to the new plan /
// meter / webhook detail pages. It is the ledger of what a recipe made (a
// recipe never creates customers or subscriptions). A recipe is free to leave
// any of these slices empty if its YAML doesn't declare that role.
type CreatedObjects struct {
	MeterIDs          []string `json:"meter_ids,omitempty"`
	RatingRuleIDs     []string `json:"rating_rule_ids,omitempty"`
	PricingRuleIDs    []string `json:"pricing_rule_ids,omitempty"`
	PlanIDs           []string `json:"plan_ids,omitempty"`
	DunningPolicyID   string   `json:"dunning_policy_id,omitempty"`
	WebhookEndpointID string   `json:"webhook_endpoint_id,omitempty"`
}

// Recipe is the parsed, validated form of a recipe YAML file. Loaded once
// at process boot from the embedded FS and held in the registry; never
// mutated after load. Render() applies overrides and produces a
// RenderedRecipe ready for instantiation.
//
// JSON tags use snake_case to match the rest of /v1/* and the wire
// contract in docs/design-recipes.md.
type Recipe struct {
	Key         string           `json:"key"`
	Version     string           `json:"version"`
	Name        string           `json:"name"`
	Summary     string           `json:"summary"`
	Description string           `json:"description,omitempty"`
	Overridable []RecipeOverride `json:"overridable"`

	Meters        []RecipeMeter        `json:"meters"`
	RatingRules   []RecipeRatingRule   `json:"rating_rules"`
	PricingRules  []RecipePricingRule  `json:"pricing_rules"`
	Plans         []RecipePlan         `json:"plans"`
	DunningPolicy *RecipeDunningPolicy `json:"dunning_policy,omitempty"`
	Webhook       *RecipeWebhook       `json:"webhook,omitempty"`
}

// RecipeOverride is one override key declared in a recipe's `overridable`
// list. Drives validation at instantiate time and the override form in
// the dashboard's preview dialog.
type RecipeOverride struct {
	Key       string   `json:"key"`
	Type      string   `json:"type"` // "string" or "int"
	Default   any      `json:"default"`
	Enum      []string `json:"enum,omitempty"`
	MaxLength int      `json:"max_length,omitempty"`
	Pattern   string   `json:"pattern,omitempty"`
}

type RecipeMeter struct {
	Key         string `json:"key"`
	Name        string `json:"name"`
	Unit        string `json:"unit"`
	Aggregation string `json:"aggregation"`
}

type RecipeRatingRule struct {
	Key                    string          `json:"key"`
	Name                   string          `json:"name,omitempty"`
	Mode                   PricingMode     `json:"mode"`
	Currency               string          `json:"currency"`
	FlatAmountCents        decimal.Decimal `json:"flat_amount_cents,omitempty"`
	GraduatedTiers         []RatingTier    `json:"graduated_tiers,omitempty"`
	PackageSize            int64           `json:"package_size,omitempty"`
	PackageAmountCents     int64           `json:"package_amount_cents,omitempty"`
	OverageUnitAmountCents decimal.Decimal `json:"overage_unit_amount_cents,omitempty"`
}

type RecipePricingRule struct {
	MeterKey        string          `json:"meter_key"`
	RatingRuleKey   string          `json:"rating_rule_key"`
	DimensionMatch  map[string]any  `json:"dimension_match"`
	AggregationMode AggregationMode `json:"aggregation_mode"`
	Priority        int             `json:"priority"`
}

type RecipePlan struct {
	Code            string          `json:"code"`
	Name            string          `json:"name"`
	Currency        string          `json:"currency"`
	BillingInterval BillingInterval `json:"billing_interval"`
	BaseAmountCents int64           `json:"base_amount_cents"`
	// BaseBillTiming is the operator's packaging choice (in_advance /
	// in_arrears). Empty → CreatePlanTx defaults to in_arrears. Honored
	// verbatim on the generated plan; the recipe never second-guesses it.
	BaseBillTiming BillTiming `json:"base_bill_timing,omitempty"`
	MeterKeys      []string   `json:"meter_keys"`
}

type RecipeDunningPolicy struct {
	Name                    string `json:"name"`
	MaxRetries              int    `json:"max_retries"`
	IntervalsHours          []int  `json:"intervals_hours"`
	FinalSubscriptionAction string `json:"final_subscription_action"`
	FinalInvoiceAction      string `json:"final_invoice_action"`
}

type RecipeWebhook struct {
	Events         []string `json:"events"`
	URLPlaceholder string   `json:"url_placeholder"`
}
