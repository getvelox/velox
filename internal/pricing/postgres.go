package pricing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

type PostgresStore struct {
	db *postgres.DB
}

func NewPostgresStore(db *postgres.DB) *PostgresStore {
	return &PostgresStore{db: db}
}

// ---------------------------------------------------------------------------
// Rating Rules
// ---------------------------------------------------------------------------

func (s *PostgresStore) CreateRatingRule(ctx context.Context, tenantID string, rule domain.RatingRuleVersion) (domain.RatingRuleVersion, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.RatingRuleVersion{}, err
	}
	defer postgres.Rollback(tx)

	created, err := s.createRatingRuleTx(ctx, tx, tenantID, rule)
	if err != nil {
		return domain.RatingRuleVersion{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.RatingRuleVersion{}, err
	}
	return created, nil
}

// CreateRatingRuleTx inserts a rating rule version inside an existing tx.
// Used by recipe.Service.Instantiate to atomically create the recipe's
// rating rules + meters + plans + pricing rules in a single transaction —
// the recipe rolls back as a unit if any step fails.
func (s *PostgresStore) CreateRatingRuleTx(ctx context.Context, tx *sql.Tx, tenantID string, rule domain.RatingRuleVersion) (domain.RatingRuleVersion, error) {
	return s.createRatingRuleTx(ctx, tx, tenantID, rule)
}

func (s *PostgresStore) createRatingRuleTx(ctx context.Context, tx *sql.Tx, tenantID string, rule domain.RatingRuleVersion) (domain.RatingRuleVersion, error) {
	id := postgres.NewID("vlx_rrv")
	tiersJSON, err := json.Marshal(rule.GraduatedTiers)
	if err != nil {
		return domain.RatingRuleVersion{}, fmt.Errorf("marshal graduated_tiers: %w", err)
	}

	// Version allocated IN SQL: MAX(version)+1 per (tenant, rule_key)
	// under the same statement as the insert. The previous Go-side
	// list-max let two concurrent publishes read the same max and the
	// loser 409 spuriously; it also let the recipe path hardcode
	// Version:1, which made reinstall-after-uninstall a guaranteed
	// unique violation (ADR-070). The caller's Version field is
	// ignored. A concurrent racer can still collide on the unique
	// (tenant, rule_key, version) — service-level CreateRatingRule
	// retries; tx composers (recipe) surface the rare conflict.
	//
	// created_at via clock.Now(ctx) (was wall-clock time.Now): as-of
	// resolution compares period starts against these stamps, so they
	// must honor any ctx-bound effective-now the same way every other
	// time-anchored write does (ADR-030).
	err = tx.QueryRowContext(ctx, `
		INSERT INTO rating_rule_versions (id, tenant_id, rule_key, name, version, lifecycle_state, mode,
			currency, flat_amount_cents, graduated_tiers, package_size, package_amount_cents,
			overage_unit_amount_cents, created_at)
		SELECT $1, $2, $3, $4,
			COALESCE(MAX(version), 0) + 1,
			$5, $6, $7, $8, $9, $10, $11, $12, $13
		FROM rating_rule_versions WHERE tenant_id = $2 AND rule_key = $3
		RETURNING id, tenant_id, rule_key, name, version, lifecycle_state, mode, currency,
			flat_amount_cents, graduated_tiers, package_size, package_amount_cents,
			overage_unit_amount_cents, created_at
	`, id, tenantID, rule.RuleKey, rule.Name, rule.LifecycleState, rule.Mode,
		rule.Currency, rule.FlatAmountCents, tiersJSON, rule.PackageSize,
		rule.PackageAmountCents, rule.OverageUnitAmountCents, clock.Now(ctx),
	).Scan(
		&rule.ID, &rule.TenantID, &rule.RuleKey, &rule.Name, &rule.Version,
		&rule.LifecycleState, &rule.Mode, &rule.Currency, &rule.FlatAmountCents,
		&tiersJSON, &rule.PackageSize, &rule.PackageAmountCents,
		&rule.OverageUnitAmountCents, &rule.CreatedAt,
	)
	if err != nil {
		if postgres.IsUniqueViolation(err) {
			return domain.RatingRuleVersion{}, errs.AlreadyExists("rule_key", fmt.Sprintf("rule_key %q: concurrent version publish, retry", rule.RuleKey))
		}
		return domain.RatingRuleVersion{}, err
	}

	if err := json.Unmarshal(tiersJSON, &rule.GraduatedTiers); err != nil {
		return domain.RatingRuleVersion{}, err
	}
	return rule, nil
}

// GetRuleByKeyAsOf resolves the rating-rule version in force at asOf
// (ADR-070 pin-at-period-start): the highest active version whose
// created_at <= asOf. A key born AFTER asOf (rule created mid-period —
// there is no prior price to preserve) resolves to its earliest active
// version. Archived/draft versions never resolve. errs.ErrNotFound when
// the key has no active versions.
func (s *PostgresStore) GetRuleByKeyAsOf(ctx context.Context, tenantID, ruleKey string, asOf time.Time) (domain.RatingRuleVersion, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.RatingRuleVersion{}, err
	}
	defer postgres.Rollback(tx)

	rule, err := scanRatingRule(tx.QueryRowContext(ctx, `
		SELECT id, tenant_id, rule_key, name, version, lifecycle_state, mode, currency,
			flat_amount_cents, graduated_tiers, package_size, package_amount_cents,
			overage_unit_amount_cents, created_at
		FROM rating_rule_versions
		WHERE rule_key = $1 AND lifecycle_state = 'active' AND created_at <= $2
		ORDER BY version DESC LIMIT 1
	`, ruleKey, asOf))
	if err == nil {
		return rule, nil
	}
	if !errors.Is(err, errs.ErrNotFound) {
		return domain.RatingRuleVersion{}, err
	}
	// Key born after asOf: earliest active version.
	return scanRatingRule(tx.QueryRowContext(ctx, `
		SELECT id, tenant_id, rule_key, name, version, lifecycle_state, mode, currency,
			flat_amount_cents, graduated_tiers, package_size, package_amount_cents,
			overage_unit_amount_cents, created_at
		FROM rating_rule_versions
		WHERE rule_key = $1 AND lifecycle_state = 'active'
		ORDER BY version ASC LIMIT 1
	`, ruleKey))
}

func (s *PostgresStore) GetRatingRule(ctx context.Context, tenantID, id string) (domain.RatingRuleVersion, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.RatingRuleVersion{}, err
	}
	defer postgres.Rollback(tx)

	return scanRatingRule(tx.QueryRowContext(ctx, `
		SELECT id, tenant_id, rule_key, name, version, lifecycle_state, mode, currency,
			flat_amount_cents, graduated_tiers, package_size, package_amount_cents,
			overage_unit_amount_cents, created_at
		FROM rating_rule_versions WHERE id = $1
	`, id))
}

func (s *PostgresStore) ListRatingRules(ctx context.Context, filter RatingRuleFilter) ([]domain.RatingRuleVersion, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, filter.TenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	query := `SELECT id, tenant_id, rule_key, name, version, lifecycle_state, mode, currency,
		flat_amount_cents, graduated_tiers, package_size, package_amount_cents,
		overage_unit_amount_cents, created_at
		FROM rating_rule_versions`
	args := []any{}
	clauses := []string{}
	idx := 1

	if filter.RuleKey != "" {
		clauses = append(clauses, fmt.Sprintf("rule_key = $%d", idx))
		args = append(args, filter.RuleKey)
		idx++
	}
	if filter.LifecycleState != "" {
		clauses = append(clauses, fmt.Sprintf("lifecycle_state = $%d", idx))
		args = append(args, filter.LifecycleState)
	}

	if len(clauses) > 0 {
		query += " WHERE " + joinClauses(clauses)
	}

	if filter.LatestOnly {
		query = `SELECT DISTINCT ON (rule_key) ` + query[len("SELECT "):]
		query += " ORDER BY rule_key, version DESC"
	} else {
		query += " ORDER BY rule_key, version DESC"
	}

	query += " LIMIT 500"

	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var rules []domain.RatingRuleVersion
	for rows.Next() {
		r, err := scanRatingRuleRow(rows)
		if err != nil {
			return nil, err
		}
		rules = append(rules, r)
	}
	return rules, rows.Err()
}

// ---------------------------------------------------------------------------
// Meters
// ---------------------------------------------------------------------------

func (s *PostgresStore) CreateMeter(ctx context.Context, tenantID string, m domain.Meter) (domain.Meter, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.Meter{}, err
	}
	defer postgres.Rollback(tx)

	created, err := s.createMeterTx(ctx, tx, tenantID, m)
	if err != nil {
		return domain.Meter{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.Meter{}, err
	}
	return created, nil
}

// CreateMeterTx inserts a meter inside an existing tx. See CreateRatingRuleTx
// for the cross-domain composition rationale.
func (s *PostgresStore) CreateMeterTx(ctx context.Context, tx *sql.Tx, tenantID string, m domain.Meter) (domain.Meter, error) {
	return s.createMeterTx(ctx, tx, tenantID, m)
}

func (s *PostgresStore) createMeterTx(ctx context.Context, tx *sql.Tx, tenantID string, m domain.Meter) (domain.Meter, error) {
	id := postgres.NewID("vlx_mtr")
	now := time.Now().UTC()

	err := tx.QueryRowContext(ctx, `
		INSERT INTO meters (id, tenant_id, key, name, unit, aggregation, rating_rule_version_id, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$8)
		RETURNING id, tenant_id, key, name, unit, aggregation, COALESCE(rating_rule_version_id,''), created_at, updated_at
	`, id, tenantID, m.Key, m.Name, m.Unit, m.Aggregation,
		postgres.NullableString(m.RatingRuleVersionID), now,
	).Scan(&m.ID, &m.TenantID, &m.Key, &m.Name, &m.Unit, &m.Aggregation,
		&m.RatingRuleVersionID, &m.CreatedAt, &m.UpdatedAt)

	if err != nil {
		if postgres.IsUniqueViolation(err) {
			return domain.Meter{}, errs.AlreadyExists("key", fmt.Sprintf("meter key %q already exists", m.Key))
		}
		return domain.Meter{}, err
	}
	return m, nil
}

func (s *PostgresStore) GetMeter(ctx context.Context, tenantID, id string) (domain.Meter, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.Meter{}, err
	}
	defer postgres.Rollback(tx)

	var m domain.Meter
	err = tx.QueryRowContext(ctx, `
		SELECT id, tenant_id, key, name, unit, aggregation, COALESCE(rating_rule_version_id,''), created_at, updated_at
		FROM meters WHERE id = $1
	`, id).Scan(&m.ID, &m.TenantID, &m.Key, &m.Name, &m.Unit, &m.Aggregation,
		&m.RatingRuleVersionID, &m.CreatedAt, &m.UpdatedAt)
	if err == sql.ErrNoRows {
		return domain.Meter{}, errs.ErrNotFound
	}
	return m, err
}

func (s *PostgresStore) GetMeterByKey(ctx context.Context, tenantID, key string) (domain.Meter, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.Meter{}, err
	}
	defer postgres.Rollback(tx)

	var m domain.Meter
	err = tx.QueryRowContext(ctx, `
		SELECT id, tenant_id, key, name, unit, aggregation, COALESCE(rating_rule_version_id,''), created_at, updated_at
		FROM meters WHERE key = $1
	`, key).Scan(&m.ID, &m.TenantID, &m.Key, &m.Name, &m.Unit, &m.Aggregation,
		&m.RatingRuleVersionID, &m.CreatedAt, &m.UpdatedAt)
	if err == sql.ErrNoRows {
		return domain.Meter{}, errs.ErrNotFound
	}
	return m, err
}

func (s *PostgresStore) ListMeters(ctx context.Context, tenantID string) ([]domain.Meter, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	rows, err := tx.QueryContext(ctx, `
		SELECT id, tenant_id, key, name, unit, aggregation, COALESCE(rating_rule_version_id,''), created_at, updated_at
		FROM meters ORDER BY created_at DESC LIMIT 500
	`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var meters []domain.Meter
	for rows.Next() {
		var m domain.Meter
		if err := rows.Scan(&m.ID, &m.TenantID, &m.Key, &m.Name, &m.Unit, &m.Aggregation,
			&m.RatingRuleVersionID, &m.CreatedAt, &m.UpdatedAt); err != nil {
			return nil, err
		}
		meters = append(meters, m)
	}
	return meters, rows.Err()
}

func (s *PostgresStore) UpdateMeter(ctx context.Context, tenantID string, m domain.Meter) (domain.Meter, error) {
	return s.UpdateMeterAudited(ctx, tenantID, m, nil)
}

// UpdateMeterAudited patches the meter and runs the caller-supplied audit
// emission on the SAME transaction (ADR-090 shared fate). The emission sees
// the RETURNING row — the state that actually landed, read under this tx —
// and only ever runs when the UPDATE matched a row: a PATCH against a meter
// that doesn't exist (or lives on the other livemode plane / another tenant
// under RLS) returns ErrNotFound with no audit row, so the log can never
// assert a change to a meter that was never touched. An emit error aborts
// the patch.
func (s *PostgresStore) UpdateMeterAudited(ctx context.Context, tenantID string, m domain.Meter, emit func(tx *sql.Tx, out domain.Meter) error) (domain.Meter, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.Meter{}, err
	}
	defer postgres.Rollback(tx)

	now := time.Now().UTC()
	err = tx.QueryRowContext(ctx, `
		UPDATE meters SET name = $1, unit = $2, aggregation = $3, rating_rule_version_id = $4, updated_at = $5
		WHERE id = $6
		RETURNING id, tenant_id, key, name, unit, aggregation, COALESCE(rating_rule_version_id,''), created_at, updated_at
	`, m.Name, m.Unit, m.Aggregation, postgres.NullableString(m.RatingRuleVersionID), now, m.ID,
	).Scan(&m.ID, &m.TenantID, &m.Key, &m.Name, &m.Unit, &m.Aggregation,
		&m.RatingRuleVersionID, &m.CreatedAt, &m.UpdatedAt)

	if err == sql.ErrNoRows {
		return domain.Meter{}, errs.ErrNotFound
	}
	if err != nil {
		return domain.Meter{}, err
	}
	if emit != nil {
		if err := emit(tx, m); err != nil {
			return domain.Meter{}, fmt.Errorf("audit emission: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return domain.Meter{}, err
	}
	return m, nil
}

// ---------------------------------------------------------------------------
// Plans
// ---------------------------------------------------------------------------

func (s *PostgresStore) CreatePlan(ctx context.Context, tenantID string, p domain.Plan) (domain.Plan, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.Plan{}, err
	}
	defer postgres.Rollback(tx)

	created, err := s.createPlanTx(ctx, tx, tenantID, p)
	if err != nil {
		return domain.Plan{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.Plan{}, err
	}
	return created, nil
}

// CreatePlanTx inserts a plan inside an existing tx. See CreateRatingRuleTx
// for the cross-domain composition rationale.
func (s *PostgresStore) CreatePlanTx(ctx context.Context, tx *sql.Tx, tenantID string, p domain.Plan) (domain.Plan, error) {
	return s.createPlanTx(ctx, tx, tenantID, p)
}

func (s *PostgresStore) createPlanTx(ctx context.Context, tx *sql.Tx, tenantID string, p domain.Plan) (domain.Plan, error) {
	id := postgres.NewID("vlx_pln")
	now := time.Now().UTC()
	meterIDsJSON, _ := json.Marshal(p.MeterIDs)

	baseBillTiming := p.BaseBillTiming
	if baseBillTiming == "" {
		baseBillTiming = domain.BillInArrears
	}
	err := tx.QueryRowContext(ctx, `
		INSERT INTO plans (id, tenant_id, code, name, description, currency, billing_interval,
			status, base_amount_cents, meter_ids, created_at, updated_at, tax_code, base_bill_timing)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$11,$12,$13)
		RETURNING id, tenant_id, code, name, COALESCE(description,''), currency, billing_interval,
			status, base_amount_cents, meter_ids, created_at, updated_at, tax_code, base_bill_timing
	`, id, tenantID, p.Code, p.Name, postgres.NullableString(p.Description),
		p.Currency, p.BillingInterval, p.Status, p.BaseAmountCents, meterIDsJSON, now, p.TaxCode, baseBillTiming,
	).Scan(&p.ID, &p.TenantID, &p.Code, &p.Name, &p.Description, &p.Currency,
		&p.BillingInterval, &p.Status, &p.BaseAmountCents, &meterIDsJSON,
		&p.CreatedAt, &p.UpdatedAt, &p.TaxCode, &p.BaseBillTiming)

	if err != nil {
		if postgres.IsUniqueViolation(err) {
			return domain.Plan{}, errs.AlreadyExists("code", fmt.Sprintf("plan code %q already exists", p.Code))
		}
		return domain.Plan{}, err
	}
	_ = json.Unmarshal(meterIDsJSON, &p.MeterIDs)
	return p, nil
}

func (s *PostgresStore) GetPlan(ctx context.Context, tenantID, id string) (domain.Plan, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.Plan{}, err
	}
	defer postgres.Rollback(tx)

	return scanPlan(tx.QueryRowContext(ctx, `
		SELECT id, tenant_id, code, name, COALESCE(description,''), currency, billing_interval,
			status, base_amount_cents, meter_ids, created_at, updated_at, tax_code, base_bill_timing
		FROM plans WHERE id = $1
	`, id))
}

func (s *PostgresStore) ListPlans(ctx context.Context, tenantID string) ([]domain.Plan, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	rows, err := tx.QueryContext(ctx, `
		SELECT id, tenant_id, code, name, COALESCE(description,''), currency, billing_interval,
			status, base_amount_cents, meter_ids, created_at, updated_at, tax_code, base_bill_timing
		FROM plans ORDER BY created_at DESC LIMIT 500
	`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var plans []domain.Plan
	for rows.Next() {
		p, err := scanPlanRow(rows)
		if err != nil {
			return nil, err
		}
		plans = append(plans, p)
	}
	return plans, rows.Err()
}

func (s *PostgresStore) UpdatePlan(ctx context.Context, tenantID string, p domain.Plan) (domain.Plan, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.Plan{}, err
	}
	defer postgres.Rollback(tx)

	now := time.Now().UTC()
	meterIDsJSON, _ := json.Marshal(p.MeterIDs)

	baseBillTiming := p.BaseBillTiming
	if baseBillTiming == "" {
		baseBillTiming = domain.BillInArrears
	}
	err = tx.QueryRowContext(ctx, `
		UPDATE plans SET name = $1, description = $2, status = $3, base_amount_cents = $4,
			meter_ids = $5, updated_at = $6, tax_code = $7, base_bill_timing = $8
		WHERE id = $9
		RETURNING id, tenant_id, code, name, COALESCE(description,''), currency, billing_interval,
			status, base_amount_cents, meter_ids, created_at, updated_at, tax_code, base_bill_timing
	`, p.Name, postgres.NullableString(p.Description), p.Status, p.BaseAmountCents,
		meterIDsJSON, now, p.TaxCode, baseBillTiming, p.ID,
	).Scan(&p.ID, &p.TenantID, &p.Code, &p.Name, &p.Description, &p.Currency,
		&p.BillingInterval, &p.Status, &p.BaseAmountCents, &meterIDsJSON,
		&p.CreatedAt, &p.UpdatedAt, &p.TaxCode, &p.BaseBillTiming)

	if err == sql.ErrNoRows {
		return domain.Plan{}, errs.ErrNotFound
	}
	if err != nil {
		return domain.Plan{}, err
	}
	_ = json.Unmarshal(meterIDsJSON, &p.MeterIDs)
	if err := tx.Commit(); err != nil {
		return domain.Plan{}, err
	}
	return p, nil
}

// ---------------------------------------------------------------------------
// Scan helpers
// ---------------------------------------------------------------------------

type rowScanner interface {
	Scan(dest ...any) error
}

func scanRatingRule(row rowScanner) (domain.RatingRuleVersion, error) {
	var r domain.RatingRuleVersion
	var tiersJSON []byte
	err := row.Scan(&r.ID, &r.TenantID, &r.RuleKey, &r.Name, &r.Version,
		&r.LifecycleState, &r.Mode, &r.Currency, &r.FlatAmountCents,
		&tiersJSON, &r.PackageSize, &r.PackageAmountCents,
		&r.OverageUnitAmountCents, &r.CreatedAt)
	if err == sql.ErrNoRows {
		return domain.RatingRuleVersion{}, errs.ErrNotFound
	}
	if err != nil {
		return domain.RatingRuleVersion{}, err
	}
	// Fail loud on malformed tier JSON rather than silently leaving
	// GraduatedTiers empty — an empty graduated rule would surface as
	// ErrInvalidPricingConfig only later at billing time, far from the cause.
	if len(tiersJSON) > 0 {
		if err := json.Unmarshal(tiersJSON, &r.GraduatedTiers); err != nil {
			return domain.RatingRuleVersion{}, fmt.Errorf("unmarshal graduated_tiers for rule %s: %w", r.ID, err)
		}
	}
	return r, nil
}

func scanRatingRuleRow(rows *sql.Rows) (domain.RatingRuleVersion, error) {
	return scanRatingRule(rows)
}

func scanPlan(row rowScanner) (domain.Plan, error) {
	var p domain.Plan
	var meterIDsJSON []byte
	err := row.Scan(&p.ID, &p.TenantID, &p.Code, &p.Name, &p.Description, &p.Currency,
		&p.BillingInterval, &p.Status, &p.BaseAmountCents, &meterIDsJSON,
		&p.CreatedAt, &p.UpdatedAt, &p.TaxCode, &p.BaseBillTiming)
	if err == sql.ErrNoRows {
		return domain.Plan{}, errs.ErrNotFound
	}
	if err != nil {
		return domain.Plan{}, err
	}
	_ = json.Unmarshal(meterIDsJSON, &p.MeterIDs)
	return p, nil
}

func scanPlanRow(rows *sql.Rows) (domain.Plan, error) {
	return scanPlan(rows)
}

func joinClauses(parts []string) string {
	result := ""
	for i, p := range parts {
		if i > 0 {
			result += " AND "
		}
		result += p
	}
	return result
}

// ---------------------------------------------------------------------------
// Meter Pricing Rules
// ---------------------------------------------------------------------------

// UpsertMeterPricingRule inserts or updates a pricing rule for a meter.
// The unique key is (tenant_id, meter_id, rating_rule_version_id) — a
// rule is identified by which rating rule it points at, so re-issuing
// the same point-pair with new dimension_match / mode / priority
// updates the existing row. ON CONFLICT keeps the original id and
// created_at, and bumps updated_at.
func (s *PostgresStore) UpsertMeterPricingRule(ctx context.Context, tenantID string, rule domain.MeterPricingRule) (domain.MeterPricingRule, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.MeterPricingRule{}, err
	}
	defer postgres.Rollback(tx)

	stored, err := s.upsertMeterPricingRuleTx(ctx, tx, tenantID, rule)
	if err != nil {
		return domain.MeterPricingRule{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.MeterPricingRule{}, err
	}
	return stored, nil
}

// UpsertMeterPricingRuleTx upserts a rule inside an existing tx. Recipe
// instantiation pairs this with CreateMeterTx + CreateRatingRuleTx so the
// rule's foreign keys to meter_id and rating_rule_version_id are visible
// inside the same uncommitted snapshot.
func (s *PostgresStore) UpsertMeterPricingRuleTx(ctx context.Context, tx *sql.Tx, tenantID string, rule domain.MeterPricingRule) (domain.MeterPricingRule, error) {
	return s.upsertMeterPricingRuleTx(ctx, tx, tenantID, rule)
}

func (s *PostgresStore) upsertMeterPricingRuleTx(ctx context.Context, tx *sql.Tx, tenantID string, rule domain.MeterPricingRule) (domain.MeterPricingRule, error) {
	id := rule.ID
	if id == "" {
		id = postgres.NewID("vlx_mpr")
	}

	dimMatch := rule.DimensionMatch
	if dimMatch == nil {
		dimMatch = map[string]any{}
	}
	matchJSON, err := json.Marshal(dimMatch)
	if err != nil {
		return domain.MeterPricingRule{}, fmt.Errorf("marshal dimension_match: %w", err)
	}

	now := time.Now().UTC()
	var stored domain.MeterPricingRule
	var storedMatch []byte
	err = tx.QueryRowContext(ctx, `
		INSERT INTO meter_pricing_rules
			(id, tenant_id, meter_id, rating_rule_version_id, dimension_match,
			 aggregation_mode, priority, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$8)
		ON CONFLICT (tenant_id, meter_id, rating_rule_version_id) DO UPDATE
		   SET dimension_match  = EXCLUDED.dimension_match,
		       aggregation_mode = EXCLUDED.aggregation_mode,
		       priority         = EXCLUDED.priority,
		       updated_at       = EXCLUDED.updated_at
		RETURNING id, tenant_id, meter_id, rating_rule_version_id,
		          dimension_match, aggregation_mode, priority,
		          created_at, updated_at
	`, id, tenantID, rule.MeterID, rule.RatingRuleVersionID, matchJSON,
		string(rule.AggregationMode), rule.Priority, now,
	).Scan(
		&stored.ID, &stored.TenantID, &stored.MeterID, &stored.RatingRuleVersionID,
		&storedMatch, &stored.AggregationMode, &stored.Priority,
		&stored.CreatedAt, &stored.UpdatedAt,
	)
	if err != nil {
		return domain.MeterPricingRule{}, err
	}

	if len(storedMatch) > 0 {
		if err := json.Unmarshal(storedMatch, &stored.DimensionMatch); err != nil {
			return domain.MeterPricingRule{}, fmt.Errorf("unmarshal dimension_match: %w", err)
		}
	}
	if stored.DimensionMatch == nil {
		stored.DimensionMatch = map[string]any{}
	}
	return stored, nil
}

func (s *PostgresStore) GetMeterPricingRule(ctx context.Context, tenantID, id string) (domain.MeterPricingRule, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.MeterPricingRule{}, err
	}
	defer postgres.Rollback(tx)

	rule, err := scanMeterPricingRule(tx.QueryRowContext(ctx, `
		SELECT id, tenant_id, meter_id, rating_rule_version_id,
		       dimension_match, aggregation_mode, priority,
		       created_at, updated_at
		  FROM meter_pricing_rules
		 WHERE id = $1
	`, id))
	if err != nil {
		return domain.MeterPricingRule{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.MeterPricingRule{}, err
	}
	return rule, nil
}

// ListMeterPricingRulesByMeter returns all pricing rules for a meter
// ordered by (priority DESC, created_at ASC, id ASC). This matches the
// runtime resolution order in design-multi-dim-meters.md so callers can
// walk the slice top-down without re-sorting. The id tiebreaker pins
// determinism for the corner case where two rules share priority AND
// created_at (same-txn bulk import, clock-resolution collisions).
func (s *PostgresStore) ListMeterPricingRulesByMeter(ctx context.Context, tenantID, meterID string) ([]domain.MeterPricingRule, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	rows, err := tx.QueryContext(ctx, `
		SELECT id, tenant_id, meter_id, rating_rule_version_id,
		       dimension_match, aggregation_mode, priority,
		       created_at, updated_at
		  FROM meter_pricing_rules
		 WHERE meter_id = $1
		 ORDER BY priority DESC, created_at ASC, id ASC
	`, meterID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []domain.MeterPricingRule
	for rows.Next() {
		rule, err := scanMeterPricingRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rule)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *PostgresStore) DeleteMeterPricingRule(ctx context.Context, tenantID, id string) error {
	return s.DeleteMeterPricingRuleAudited(ctx, tenantID, id, nil)
}

// DeleteMeterPricingRuleAudited deletes the rule and runs the caller-supplied
// audit emission on the SAME transaction (ADR-090 shared fate).
//
// This route is ADR-090's poster child: the deleted HTTP catch-all recorded this
// DELETE as "deleted meter {meter_id}" (its path classifier took the second
// segment as the resource id) — a false permanent row in an append-only
// compliance log claiming the operator destroyed a meter and all its pricing,
// when they removed one dimension rule. The emission here replaces that lie.
//
// DELETE … RETURNING makes the deleted row itself the evidence source: the
// audit row's meter_id is the one the rule actually carried, read inside the
// tx — not the {meter_id} URL segment, which the router never checks against
// the rule (a caller can pass any meter id and still delete the rule; see the
// note in the final report). A DELETE that removes no row yields ErrNoRows →
// ErrNotFound (the handler's 404) and emits NOTHING.
func (s *PostgresStore) DeleteMeterPricingRuleAudited(ctx context.Context, tenantID, id string, emit func(tx *sql.Tx, deleted domain.MeterPricingRule) error) error {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)

	deleted, err := scanMeterPricingRule(tx.QueryRowContext(ctx, `
		DELETE FROM meter_pricing_rules
		 WHERE id = $1
		RETURNING id, tenant_id, meter_id, rating_rule_version_id,
		          dimension_match, aggregation_mode, priority,
		          created_at, updated_at
	`, id))
	if err != nil {
		// scanMeterPricingRule already folds sql.ErrNoRows → errs.ErrNotFound:
		// the zero-row delete never reaches emit.
		return err
	}
	if emit != nil {
		if err := emit(tx, deleted); err != nil {
			return fmt.Errorf("audit emission: %w", err)
		}
	}
	return tx.Commit()
}

func scanMeterPricingRule(row rowScanner) (domain.MeterPricingRule, error) {
	var r domain.MeterPricingRule
	var matchJSON []byte
	err := row.Scan(&r.ID, &r.TenantID, &r.MeterID, &r.RatingRuleVersionID,
		&matchJSON, &r.AggregationMode, &r.Priority,
		&r.CreatedAt, &r.UpdatedAt)
	if err == sql.ErrNoRows {
		return domain.MeterPricingRule{}, errs.ErrNotFound
	}
	if err != nil {
		return domain.MeterPricingRule{}, err
	}
	if len(matchJSON) > 0 {
		if err := json.Unmarshal(matchJSON, &r.DimensionMatch); err != nil {
			return domain.MeterPricingRule{}, fmt.Errorf("unmarshal dimension_match: %w", err)
		}
	}
	if r.DimensionMatch == nil {
		r.DimensionMatch = map[string]any{}
	}
	return r, nil
}

// ---- Currency-coherence reads (ADR-100) ----------------------------------

// inScopePlansCond selects plans that still BILL: everything non-archived,
// plus archived plans referenced by a non-terminal subscription item (those
// keep billing at cycle close; excluding sub-less archived plans is what
// makes the wrong-currency repair flow — archive, then republish — possible).
const inScopePlansCond = `
	(p.status != 'archived' OR EXISTS (
		SELECT 1 FROM subscription_items si
		JOIN subscriptions sub ON sub.id = si.subscription_id
		WHERE si.tenant_id = p.tenant_id
		  AND (si.plan_id = p.id OR si.pending_plan_id = p.id)
		  AND sub.status IN ('draft', 'trialing', 'active')
	))`

// resolvedKeyCurrenciesCTE resolves each rule_key in bound_keys to its
// engine-visible currency: the latest ACTIVE version's, with a mixed flag
// when active versions span more than one currency.
const resolvedKeyCurrenciesCTE = `
	resolved AS (
		SELECT rule_key,
		       (ARRAY_AGG(currency ORDER BY version DESC))[1] AS currency,
		       COUNT(DISTINCT currency) > 1 AS mixed
		FROM rating_rule_versions
		WHERE tenant_id = $1
		  AND lifecycle_state = 'active'
		  AND rule_key IN (SELECT rule_key FROM bound_keys)
		GROUP BY rule_key
	)`

func (s *PostgresStore) PlanCurrenciesBoundToRuleKey(ctx context.Context, tenantID, ruleKey string) ([]planCurrency, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)
	rows, err := tx.QueryContext(ctx, `
		WITH key_versions AS (
			SELECT id FROM rating_rule_versions WHERE tenant_id = $1 AND rule_key = $2
		),
		bound_meters AS (
			SELECT m.id FROM meters m
			WHERE m.tenant_id = $1 AND m.rating_rule_version_id IN (SELECT id FROM key_versions)
			UNION
			SELECT mpr.meter_id FROM meter_pricing_rules mpr
			WHERE mpr.tenant_id = $1 AND mpr.rating_rule_version_id IN (SELECT id FROM key_versions)
		)
		SELECT DISTINCT p.code, p.currency
		FROM plans p
		JOIN bound_meters bm ON p.meter_ids @> to_jsonb(ARRAY[bm.id])
		WHERE p.tenant_id = $1 AND `+inScopePlansCond+`
	`, tenantID, ruleKey)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []planCurrency
	for rows.Next() {
		var pc planCurrency
		if err := rows.Scan(&pc.Code, &pc.Currency); err != nil {
			return nil, err
		}
		out = append(out, pc)
	}
	return out, rows.Err()
}

func (s *PostgresStore) PlanCurrenciesWiringMeters(ctx context.Context, tenantID string, meterIDs []string) ([]planCurrency, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)
	rows, err := tx.QueryContext(ctx, `
		SELECT DISTINCT p.code, p.currency
		FROM plans p
		JOIN unnest($2::text[]) AS mid(id) ON p.meter_ids @> to_jsonb(ARRAY[mid.id])
		WHERE p.tenant_id = $1 AND `+inScopePlansCond+`
	`, tenantID, postgres.StringArray(meterIDs))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []planCurrency
	for rows.Next() {
		var pc planCurrency
		if err := rows.Scan(&pc.Code, &pc.Currency); err != nil {
			return nil, err
		}
		out = append(out, pc)
	}
	return out, rows.Err()
}

func (s *PostgresStore) MeterBoundKeyCurrencies(ctx context.Context, tenantID string, meterIDs []string) ([]keyCurrency, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)
	rows, err := tx.QueryContext(ctx, `
		WITH bound_keys AS (
			SELECT DISTINCT rrv.rule_key
			FROM meters m JOIN rating_rule_versions rrv ON rrv.id = m.rating_rule_version_id
			WHERE m.tenant_id = $1 AND m.id = ANY($2::text[])
			UNION
			SELECT DISTINCT rrv.rule_key
			FROM meter_pricing_rules mpr JOIN rating_rule_versions rrv ON rrv.id = mpr.rating_rule_version_id
			WHERE mpr.tenant_id = $1 AND mpr.meter_id = ANY($2::text[])
		),
		`+resolvedKeyCurrenciesCTE+`
		SELECT rule_key, currency, mixed FROM resolved
	`, tenantID, postgres.StringArray(meterIDs))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []keyCurrency
	for rows.Next() {
		var kc keyCurrency
		if err := rows.Scan(&kc.RuleKey, &kc.Currency, &kc.Mixed); err != nil {
			return nil, err
		}
		out = append(out, kc)
	}
	return out, rows.Err()
}

func (s *PostgresStore) RuleKeyResolvedCurrency(ctx context.Context, tenantID, ruleVersionID string) (keyCurrency, error) {
	var kc keyCurrency
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return keyCurrency{}, err
	}
	defer postgres.Rollback(tx)
	err = tx.QueryRowContext(ctx, `
		WITH bound_keys AS (
			SELECT rule_key FROM rating_rule_versions WHERE tenant_id = $1 AND id = $2
		),
		`+resolvedKeyCurrenciesCTE+`
		SELECT rule_key, currency, mixed FROM resolved
	`, tenantID, ruleVersionID).Scan(&kc.RuleKey, &kc.Currency, &kc.Mixed)
	return kc, err
}
