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
	GetMeterByEventName(ctx context.Context, tenantID, eventName string) (domain.Meter, error)
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
	//
	// guard, when non-nil, runs first on the same transaction, before the
	// UPDATE; an error from it aborts the patch (the D05 meter-in-use guard).
	UpdateMeterAudited(ctx context.Context, tenantID string, m domain.Meter, guard func(tx *sql.Tx) error, emit func(tx *sql.Tx, out domain.Meter) error) (domain.Meter, error)

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
	// See docs/dev/design/multi-dim-meters.md.
	// guard, when non-nil, runs first on the upsert's transaction; an error
	// from it aborts the write.
	UpsertMeterPricingRule(ctx context.Context, tenantID string, rule domain.MeterPricingRule, guard func(tx *sql.Tx) error) (domain.MeterPricingRule, error)
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
	// nil emit = unaudited delete. guard, when non-nil, runs first on the
	// same transaction; an error from it aborts the delete.
	DeleteMeterPricingRuleAudited(ctx context.Context, tenantID, id string, guard func(tx *sql.Tx) error, emit func(tx *sql.Tx, deleted domain.MeterPricingRule) error) error

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

	// The meter-in-use guard's reads (D05). Each runs on the caller's
	// transaction, so the guard and the edit it gates commit together.
	//
	// LockMeterTx locks the meter row FOR UPDATE and returns it as committed
	// now. Every guarded edit on a meter takes it first, so two edits on one
	// meter run one after the other and each sees the other's result.
	LockMeterTx(ctx context.Context, tx *sql.Tx, meterID string) (domain.Meter, error)
	// MeterHasUsageTx reports whether the meter has usage events (rows in
	// usage_events). Once it has, its aggregation is fixed (ADR-130 §2);
	// held rows do not count. Read after LockMeterTx, it sees every event
	// whose insert the lock waited for.
	MeterHasUsageTx(ctx context.Context, tx *sql.Tx, m domain.Meter) (bool, error)
	// MeterHasUsage is MeterHasUsageTx on its own transaction, for reads.
	MeterHasUsage(ctx context.Context, tenantID string, m domain.Meter) (bool, error)
	// CountSubsUsingMeterTx locks every plan whose meter_ids holds the meter
	// (FOR UPDATE), then counts the draft, trialing and active subscriptions
	// that bill it: an item on such a plan, or an item change to or from
	// one since the subscription's current period started. The plan lock is
	// what orders the count against a subscription being created or moved
	// onto such a plan: the subscription writer takes FOR KEY SHARE on each
	// plan it references, which waits for this lock. A writer that
	// references more than one plan takes them in ascending id, as this
	// does, right after its subscription row (D08 §4.21).
	CountSubsUsingMeterTx(ctx context.Context, tx *sql.Tx, meterID string) (int, error)
	// GetMeterPricingRuleByPairTx returns the meter's rule bound to the rating
	// rule version, or errs.ErrNotFound.
	GetMeterPricingRuleByPairTx(ctx context.Context, tx *sql.Tx, meterID, ratingRuleVersionID string) (domain.MeterPricingRule, error)
	// CountRulesOutrankedTx counts the meter's rules with priority at or below
	// priority that one event could match together with match: no key the
	// two share has different values. A new rule could take such a rule's
	// events.
	CountRulesOutrankedTx(ctx context.Context, tx *sql.Tx, meterID string, match map[string]any, priority int) (int, error)
}

type RatingRuleFilter struct {
	TenantID       string
	RuleKey        string
	LifecycleState string
	LatestOnly     bool
}
