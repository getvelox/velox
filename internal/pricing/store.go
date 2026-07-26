package pricing

import (
	"context"
	"database/sql"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

type Store interface {
	// Rating rules. CreateRatingRule/Tx allocate the version number in
	// SQL (MAX(version)+1 per (tenant, rule_key)) — the caller's Version
	// field is ignored, so concurrent publishes can't race a Go-side
	// read-modify-write into a spurious 409.
	CreateRatingRule(ctx context.Context, tenantID string, rule domain.RatingRuleVersion) (domain.RatingRuleVersion, error)
	CreateRatingRuleTx(ctx context.Context, tx *sql.Tx, tenantID string, rule domain.RatingRuleVersion) (domain.RatingRuleVersion, error)
	GetRatingRule(ctx context.Context, tenantID, id string) (domain.RatingRuleVersion, error)
	// GetRuleByKeyAsOf resolves the version in force at asOf: the
	// highest active version created at or before asOf, or the earliest
	// active version when the key was born after asOf (a rule created
	// mid-period has no prior price to preserve — its first version is
	// the period's price; ADR-070). errs.ErrNotFound when the key has
	// no active versions at all.
	GetRuleByKeyAsOf(ctx context.Context, tenantID, ruleKey string, asOf time.Time) (domain.RatingRuleVersion, error)
	ListRatingRules(ctx context.Context, filter RatingRuleFilter) ([]domain.RatingRuleVersion, error)

	// Meters
	CreateMeter(ctx context.Context, tenantID string, m domain.Meter) (domain.Meter, error)
	CreateMeterTx(ctx context.Context, tx *sql.Tx, tenantID string, m domain.Meter) (domain.Meter, error)
	GetMeter(ctx context.Context, tenantID, id string) (domain.Meter, error)
	GetMeterByKey(ctx context.Context, tenantID, key string) (domain.Meter, error)
	ListMeters(ctx context.Context, tenantID string) ([]domain.Meter, error)
	UpdateMeter(ctx context.Context, tenantID string, m domain.Meter) (domain.Meter, error)
	// UpdateMeterAudited runs the caller-supplied audit emission on the SAME
	// transaction as the meter UPDATE (ADR-090 in-tx emission): the patched
	// row and its audit row commit or roll back together. The service builds
	// the emission (it owns audit-row content); the store owns the tx and
	// exposes it. emit receives the UPDATE's RETURNING row — the values that
	// actually landed, read inside the tx, never a pre-tx snapshot. It runs
	// only when a row was updated (a missing meter returns ErrNotFound and
	// emits nothing). nil emit = unaudited write.
	UpdateMeterAudited(ctx context.Context, tenantID string, m domain.Meter, emit func(tx *sql.Tx, out domain.Meter) error) (domain.Meter, error)

	// Plans
	CreatePlan(ctx context.Context, tenantID string, p domain.Plan) (domain.Plan, error)
	CreatePlanTx(ctx context.Context, tx *sql.Tx, tenantID string, p domain.Plan) (domain.Plan, error)
	GetPlan(ctx context.Context, tenantID, id string) (domain.Plan, error)
	ListPlans(ctx context.Context, tenantID string) ([]domain.Plan, error)
	UpdatePlan(ctx context.Context, tenantID string, p domain.Plan) (domain.Plan, error)

	// Per-customer price overrides — keyed by rule_key, resolved as-of
	// the billing period's open (ADR-070).
	CreateOverride(ctx context.Context, tenantID string, o domain.CustomerPriceOverride) (domain.CustomerPriceOverride, error)
	GetOverrideByKeyAsOf(ctx context.Context, tenantID, customerID, ruleKey string, asOf time.Time) (domain.CustomerPriceOverride, error)
	DeactivateOverride(ctx context.Context, tenantID, id string) error
	CountActiveOverridesByRuleKey(ctx context.Context, tenantID, ruleKey string) (int, error)
	ListOverrides(ctx context.Context, tenantID, customerID string) ([]domain.CustomerPriceOverride, error)

	// Meter pricing rules — N-rules-per-meter dispatch via dimension_match.
	// See docs/design-multi-dim-meters.md.
	UpsertMeterPricingRule(ctx context.Context, tenantID string, rule domain.MeterPricingRule) (domain.MeterPricingRule, error)
	UpsertMeterPricingRuleTx(ctx context.Context, tx *sql.Tx, tenantID string, rule domain.MeterPricingRule) (domain.MeterPricingRule, error)
	GetMeterPricingRule(ctx context.Context, tenantID, id string) (domain.MeterPricingRule, error)
	ListMeterPricingRulesByMeter(ctx context.Context, tenantID, meterID string) ([]domain.MeterPricingRule, error)
	DeleteMeterPricingRule(ctx context.Context, tenantID, id string) error
	// DeleteMeterPricingRuleAudited runs the caller-supplied audit emission
	// on the SAME transaction as the DELETE (ADR-090). emit receives the
	// DELETED row (RETURNING) — so the audit row carries the rule's true
	// meter_id, read inside the tx, rather than a URL segment the caller
	// could have mismatched. It runs ONLY when a row was actually removed;
	// deleting a nonexistent rule returns ErrNotFound and emits nothing.
	// nil emit = unaudited delete.
	DeleteMeterPricingRuleAudited(ctx context.Context, tenantID, id string, emit func(tx *sql.Tx, deleted domain.MeterPricingRule) error) error

	// ListMeterPricingRules lists every binding for the tenant (dashboard
	// rule→meter resolution; read-only).
	ListMeterPricingRules(ctx context.Context, tenantID string) ([]domain.MeterPricingRule, error)

	// Currency-coherence reads (ADR-100). "In-scope plans" = non-archived
	// plans plus archived plans still carrying non-terminal subscriptions;
	// key currency = latest ACTIVE version (engine resolution), Mixed when
	// a key's active versions span currencies.
	PlanCurrenciesBoundToRuleKey(ctx context.Context, tenantID, ruleKey string) ([]planCurrency, error)
	PlanCurrenciesWiringMeters(ctx context.Context, tenantID string, meterIDs []string) ([]planCurrency, error)
	MeterBoundKeyCurrencies(ctx context.Context, tenantID string, meterIDs []string) ([]keyCurrency, error)
	RuleKeyResolvedCurrency(ctx context.Context, tenantID, ruleVersionID string) (keyCurrency, error)
}

type RatingRuleFilter struct {
	TenantID       string
	RuleKey        string
	LifecycleState string
	LatestOnly     bool
}
