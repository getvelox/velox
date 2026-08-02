package dunning

import (
	"context"
	"database/sql"
	"encoding/json"
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

// scanPolicy is the shared row decoder for the policy SELECTs below.
// Returned columns are fixed: id, tenant_id, name, enabled, is_default,
// retry_schedule (jsonb), max_retry_attempts, final_action,
// grace_period_days, created_at, updated_at — in that order.
func scanPolicy(row interface {
	Scan(dest ...any) error
}) (domain.DunningPolicy, error) {
	var p domain.DunningPolicy
	var scheduleJSON []byte
	if err := row.Scan(&p.ID, &p.TenantID, &p.Name, &p.Enabled, &p.IsDefault, &scheduleJSON,
		&p.MaxRetryAttempts, &p.FinalAction, &p.GracePeriodDays, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return domain.DunningPolicy{}, err
	}
	_ = json.Unmarshal(scheduleJSON, &p.RetrySchedule)
	return p, nil
}

const policyColumns = `id, tenant_id, name, enabled, is_default, retry_schedule, max_retry_attempts, final_action, grace_period_days, created_at, updated_at`

// GetPolicyByID looks up a single policy by its id within the tenant.
func (s *PostgresStore) GetPolicyByID(ctx context.Context, tenantID, id string) (domain.DunningPolicy, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.DunningPolicy{}, err
	}
	defer postgres.Rollback(tx)

	p, err := scanPolicy(tx.QueryRowContext(ctx, `SELECT `+policyColumns+` FROM dunning_policies WHERE id = $1`, id))
	if err == sql.ErrNoRows {
		return domain.DunningPolicy{}, errs.ErrNotFound
	}
	return p, err
}

// GetDefaultPolicy returns the tenant's default policy (is_default=true).
// Exactly one such row exists per (tenant, livemode) — enforced by the
// partial UNIQUE index from migration 0086.
func (s *PostgresStore) GetDefaultPolicy(ctx context.Context, tenantID string) (domain.DunningPolicy, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.DunningPolicy{}, err
	}
	defer postgres.Rollback(tx)

	p, err := scanPolicy(tx.QueryRowContext(ctx, `SELECT `+policyColumns+` FROM dunning_policies WHERE is_default LIMIT 1`))
	if err == sql.ErrNoRows {
		return domain.DunningPolicy{}, errs.ErrNotFound
	}
	return p, err
}

// ListPolicies returns all policies for the tenant, defaults first
// then by created_at. Caller renders the campaigns admin page off this.
func (s *PostgresStore) ListPolicies(ctx context.Context, tenantID string) ([]domain.DunningPolicy, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	rows, err := tx.QueryContext(ctx, `SELECT `+policyColumns+` FROM dunning_policies ORDER BY is_default DESC, created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []domain.DunningPolicy
	for rows.Next() {
		p, err := scanPolicy(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeletePolicy removes a policy. Refuses to delete the default policy
// (operator must promote another policy first); also refuses when any
// customer has an explicit dunning_policy_id pointing at it (operator
// reassigns those customers first). The application-layer guards are
// the primary defense; the FK on customers.dunning_policy_id ON DELETE
// SET NULL is belt-and-suspenders only.
func (s *PostgresStore) DeletePolicy(ctx context.Context, tenantID, id string) error {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)

	var isDefault bool
	if err := tx.QueryRowContext(ctx, `SELECT is_default FROM dunning_policies WHERE id = $1`, id).Scan(&isDefault); err != nil {
		if err == sql.ErrNoRows {
			return errs.ErrNotFound
		}
		return err
	}
	if isDefault {
		return errs.InvalidState("cannot delete the default dunning policy — promote another policy first")
	}
	var assigned int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM customers WHERE dunning_policy_id = $1`, id).Scan(&assigned); err != nil {
		return err
	}
	if assigned > 0 {
		return errs.InvalidState(fmt.Sprintf("cannot delete policy — %d customer(s) still assigned; reassign them first", assigned))
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM dunning_policies WHERE id = $1`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// SetDefaultPolicy promotes the given policy to default and demotes the
// previous default in a single tx. The partial UNIQUE index would
// otherwise reject "set new default first" (two is_default=true rows
// simultaneously), so we flip both atomically.
func (s *PostgresStore) SetDefaultPolicy(ctx context.Context, tenantID, id string) error {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)

	// Demote first; the partial UNIQUE index permits zero defaults
	// transiently, then we promote the target.
	if _, err := tx.ExecContext(ctx, `UPDATE dunning_policies SET is_default = false WHERE is_default`); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE dunning_policies SET is_default = true, updated_at = NOW() WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errs.ErrNotFound
	}
	return tx.Commit()
}

// CountCustomersOnPolicy returns the number of customers explicitly
// assigned to the given policy. Used by the handler / UI to surface
// "N customers assigned" and to gate destructive operations.
func (s *PostgresStore) CountCustomersOnPolicy(ctx context.Context, tenantID, policyID string) (int, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return 0, err
	}
	defer postgres.Rollback(tx)

	var n int
	err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM customers WHERE dunning_policy_id = $1`, policyID).Scan(&n)
	return n, err
}

func (s *PostgresStore) UpsertPolicy(ctx context.Context, tenantID string, p domain.DunningPolicy) (domain.DunningPolicy, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.DunningPolicy{}, err
	}
	defer postgres.Rollback(tx)

	stored, err := s.upsertPolicyTx(ctx, tx, tenantID, p)
	if err != nil {
		return domain.DunningPolicy{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.DunningPolicy{}, err
	}
	return stored, nil
}

// UpsertPolicyTx upserts the dunning policy inside an existing tx. Used by
// recipe.Service.Instantiate so a recipe that opts into a custom dunning
// schedule can land it atomically with the rest of the recipe's objects.
func (s *PostgresStore) UpsertPolicyTx(ctx context.Context, tx *sql.Tx, tenantID string, p domain.DunningPolicy) (domain.DunningPolicy, error) {
	return s.upsertPolicyTx(ctx, tx, tenantID, p)
}

// upsertPolicyTx inserts a new policy when p.ID is empty, updates the
// existing row when p.ID is set. The migration to the campaigns model
// (ADR-036) made tenant↔policy a 1:N relation, so the prior ON CONFLICT
// (tenant_id, livemode) singleton-merge no longer applies — that
// constraint was dropped in migration 0086.
//
// IsDefault handling: the caller CANNOT set is_default via p (an UPDATE
// preserves the stored flag; SetDefaultPolicy is the dedicated atomic
// demote-all/promote-one path). The ONE exception is the initial default —
// the FIRST policy inserted per (tenant, livemode) is auto-promoted to
// is_default=true in-statement (ADR-036 amendment; see the INSERT branch),
// so a recipe/first-create tenant resolves a working default with no extra
// call. The partial UNIQUE index is the loud backstop against two defaults.
func (s *PostgresStore) upsertPolicyTx(ctx context.Context, tx *sql.Tx, tenantID string, p domain.DunningPolicy) (domain.DunningPolicy, error) {
	// A dunning policy is tenant-level operator config, not a per-customer
	// billing event — its created_at/updated_at must record the real instant
	// the operator edited it, never a test clock's frozen_time. (Dunning RUN
	// timestamps elsewhere DO use clock.Now(ctx) — those live on the customer's
	// simulated timeline; policy config writes escape the simulation boundary.)
	now := time.Now().UTC() // wall-clock: operator config write (ADR-030 addendum), never frozen_time
	scheduleJSON, _ := json.Marshal(p.RetrySchedule)
	if p.ID == "" {
		// INSERT new policy. The FIRST policy in a (tenant, livemode) scope is
		// born is_default=true (ADR-036 amendment): NOT EXISTS(... WHERE is_default)
		// is evaluated against the pre-insert table state, RLS-scoped to the
		// current (tenant, livemode) — so a tenant that instantiates a recipe (or
		// creates its first policy manually) gets a WORKING default with no extra
		// SetDefaultPolicy step, closing the "policies exist but GetDefaultPolicy
		// → not found" trap. Subsequent policies are is_default=false; the operator
		// re-points via SetDefaultPolicy (the atomic demote-all/promote-one path).
		// The partial unique index idx_dunning_policies_one_default_per_tenant is
		// the LOUD backstop for a concurrent-first-insert race (a losing INSERT
		// gets a clean 23505, never two defaults) — no savepoint-retry belt is
		// built (creates are serial pre-launch; add the retry when a named
		// concurrency need appears).
		newID := postgres.NewID("vlx_dpol")
		row := tx.QueryRowContext(ctx, `
			INSERT INTO dunning_policies (id, tenant_id, name, enabled, is_default,
				retry_schedule, max_retry_attempts, final_action, grace_period_days,
				created_at, updated_at)
			VALUES ($1,$2,$3,$4,NOT EXISTS(SELECT 1 FROM dunning_policies WHERE is_default),$5,$6,$7,$8,$9,$9)
			RETURNING `+policyColumns,
			newID, tenantID, p.Name, p.Enabled, scheduleJSON, p.MaxRetryAttempts,
			p.FinalAction, p.GracePeriodDays, now,
		)
		return scanPolicy(row)
	}
	// UPDATE existing policy. is_default is preserved (callers use
	// SetDefaultPolicy to flip).
	row := tx.QueryRowContext(ctx, `
		UPDATE dunning_policies
		SET name = $2, enabled = $3, retry_schedule = $4,
			max_retry_attempts = $5, final_action = $6, grace_period_days = $7,
			updated_at = $8
		WHERE id = $1
		RETURNING `+policyColumns,
		p.ID, p.Name, p.Enabled, scheduleJSON, p.MaxRetryAttempts,
		p.FinalAction, p.GracePeriodDays, now,
	)
	return scanPolicy(row)
}

func (s *PostgresStore) CreateRun(ctx context.Context, tenantID string, run domain.InvoiceDunningRun) (domain.InvoiceDunningRun, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.InvoiceDunningRun{}, err
	}
	defer postgres.Rollback(tx)

	id := postgres.NewID("vlx_drun")
	// Honor caller-provided CreatedAt — dunning Service passes
	// s.clock.Now() so test-clock-driven runs (started during a
	// clock-advance billing cycle) have created_at on simulation
	// time, matching the related invoice's issued_at.
	now := run.CreatedAt
	if now.IsZero() {
		now = clock.Now(ctx)
	}
	err = tx.QueryRowContext(ctx, `
		INSERT INTO invoice_dunning_runs (id, tenant_id, invoice_id, customer_id, policy_id,
			state, reason, attempt_count, next_action_at, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$10)
		RETURNING id, tenant_id, invoice_id, COALESCE(customer_id,''), policy_id, state,
			COALESCE(reason,''), attempt_count, last_attempt_at, next_action_at,
			paused, resolved_at, COALESCE(resolution,''), created_at, updated_at
	`, id, tenantID, run.InvoiceID, postgres.NullableString(run.CustomerID),
		run.PolicyID, run.State, postgres.NullableString(run.Reason),
		run.AttemptCount, postgres.NullableTime(run.NextActionAt), now,
	).Scan(&run.ID, &run.TenantID, &run.InvoiceID, &run.CustomerID, &run.PolicyID,
		&run.State, &run.Reason, &run.AttemptCount, &run.LastAttemptAt, &run.NextActionAt,
		&run.Paused, &run.ResolvedAt, &run.Resolution, &run.CreatedAt, &run.UpdatedAt)
	if err != nil {
		return domain.InvoiceDunningRun{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.InvoiceDunningRun{}, err
	}
	return run, nil
}

func (s *PostgresStore) GetRun(ctx context.Context, tenantID, id string) (domain.InvoiceDunningRun, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.InvoiceDunningRun{}, err
	}
	defer postgres.Rollback(tx)

	var run domain.InvoiceDunningRun
	err = tx.QueryRowContext(ctx, `
		SELECT id, tenant_id, invoice_id, COALESCE(customer_id,''), policy_id, state,
			COALESCE(reason,''), attempt_count, last_attempt_at, next_action_at,
			paused, resolved_at, COALESCE(resolution,''), created_at, updated_at
		FROM invoice_dunning_runs WHERE id = $1
	`, id).Scan(&run.ID, &run.TenantID, &run.InvoiceID, &run.CustomerID, &run.PolicyID,
		&run.State, &run.Reason, &run.AttemptCount, &run.LastAttemptAt, &run.NextActionAt,
		&run.Paused, &run.ResolvedAt, &run.Resolution, &run.CreatedAt, &run.UpdatedAt)
	if err == sql.ErrNoRows {
		return domain.InvoiceDunningRun{}, errs.ErrNotFound
	}
	return run, err
}

// GetRunByInvoice returns the dunning run for the given invoice
// regardless of state. With the migration-0085 UNIQUE index there
// is at most one run per (tenant_id, invoice_id) — this method
// returns it (or ErrNotFound). Used by StartDunning's lifetime
// idempotency check.
func (s *PostgresStore) GetRunByInvoice(ctx context.Context, tenantID, invoiceID string) (domain.InvoiceDunningRun, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.InvoiceDunningRun{}, err
	}
	defer postgres.Rollback(tx)

	var run domain.InvoiceDunningRun
	err = tx.QueryRowContext(ctx, `
		SELECT id, tenant_id, invoice_id, COALESCE(customer_id,''), policy_id, state,
			COALESCE(reason,''), attempt_count, last_attempt_at, next_action_at,
			paused, resolved_at, COALESCE(resolution,''), created_at, updated_at
		FROM invoice_dunning_runs
		WHERE invoice_id = $1
		LIMIT 1
	`, invoiceID).Scan(&run.ID, &run.TenantID, &run.InvoiceID, &run.CustomerID, &run.PolicyID,
		&run.State, &run.Reason, &run.AttemptCount, &run.LastAttemptAt, &run.NextActionAt,
		&run.Paused, &run.ResolvedAt, &run.Resolution, &run.CreatedAt, &run.UpdatedAt)
	if err == sql.ErrNoRows {
		return domain.InvoiceDunningRun{}, errs.ErrNotFound
	}
	return run, err
}

func (s *PostgresStore) GetActiveRunByInvoice(ctx context.Context, tenantID, invoiceID string) (domain.InvoiceDunningRun, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.InvoiceDunningRun{}, err
	}
	defer postgres.Rollback(tx)

	var run domain.InvoiceDunningRun
	err = tx.QueryRowContext(ctx, `
		SELECT id, tenant_id, invoice_id, COALESCE(customer_id,''), policy_id, state,
			COALESCE(reason,''), attempt_count, last_attempt_at, next_action_at,
			paused, resolved_at, COALESCE(resolution,''), created_at, updated_at
		FROM invoice_dunning_runs
		-- Exclude only already-resolved runs. Escalated (retries-exhausted) runs
		-- MUST still be returned so ResolveByInvoice can resolve them when the
		-- customer pays out-of-band after escalation; otherwise the run is stuck
		-- in 'escalated' forever and never emits dunning.resolved.
		WHERE invoice_id = $1 AND state != 'resolved'
		LIMIT 1
	`, invoiceID).Scan(&run.ID, &run.TenantID, &run.InvoiceID, &run.CustomerID, &run.PolicyID,
		&run.State, &run.Reason, &run.AttemptCount, &run.LastAttemptAt, &run.NextActionAt,
		&run.Paused, &run.ResolvedAt, &run.Resolution, &run.CreatedAt, &run.UpdatedAt)
	if err == sql.ErrNoRows {
		return domain.InvoiceDunningRun{}, errs.ErrNotFound
	}
	return run, err
}

func (s *PostgresStore) ListRuns(ctx context.Context, filter RunListFilter) ([]domain.InvoiceDunningRun, int, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, filter.TenantID)
	if err != nil {
		return nil, 0, err
	}
	defer postgres.Rollback(tx)

	args := []any{}
	clauses := []string{}
	idx := 1

	if filter.InvoiceID != "" {
		clauses = append(clauses, fmt.Sprintf("r.invoice_id = $%d", idx))
		args = append(args, filter.InvoiceID)
		idx++
	}
	if filter.State != "" {
		clauses = append(clauses, fmt.Sprintf("r.state = $%d", idx))
		args = append(args, filter.State)
		idx++
	}

	whereClause := ""
	if len(clauses) > 0 {
		whereClause = " WHERE "
		for i, c := range clauses {
			if i > 0 {
				whereClause += " AND "
			}
			whereClause += c
		}
	}

	var total int
	countQuery := `SELECT COUNT(*) FROM invoice_dunning_runs r` + whereClause
	if err := tx.QueryRowContext(ctx, countQuery, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	// LEFT JOIN denormalizes invoice fields + the owning CUSTOMER's
	// test-clock frozen_time onto each row (through customers, not
	// subscriptions — a one-off invoice has no subscription row, and
	// the sub join left its runs rendering wall-anchored relative
	// times with no clock badge; a sub's clock always equals its
	// customer's clock, ADR-027). Eliminates N round-trips for
	// the /dunning page (was: 1 list + N invoice fetches + N sub
	// fetches + N clock fetches; now: 1 query). LEFT JOINs because
	// invoice / sub / clock references can be NULL or unresolvable
	// (RLS gap, deleted) without dropping the dunning row itself.
	query := `SELECT r.id, r.tenant_id, r.invoice_id, COALESCE(r.customer_id,''), r.policy_id, r.state,
		COALESCE(r.reason,''), r.attempt_count, r.last_attempt_at, r.next_action_at,
		r.paused, r.resolved_at, COALESCE(r.resolution,''), r.created_at, r.updated_at,
		COALESCE(i.invoice_number, ''), COALESCE(i.amount_due_cents, 0), COALESCE(i.currency, ''),
		tc.frozen_time, COALESCE(tc.id, '')
		FROM invoice_dunning_runs r
		LEFT JOIN invoices i ON i.id = r.invoice_id
		LEFT JOIN customers cu ON cu.id = i.customer_id
		LEFT JOIN test_clocks tc ON tc.id = cu.test_clock_id` + whereClause + ` ORDER BY r.created_at DESC`
	// Default 50, clamp to 100 — no-silent-fallbacks principle.
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	} else if limit > 100 {
		limit = 100
	}
	query += fmt.Sprintf(" LIMIT $%d OFFSET $%d", idx, idx+1)
	args = append(args, limit, filter.Offset)

	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()

	var runs []domain.InvoiceDunningRun
	for rows.Next() {
		var r domain.InvoiceDunningRun
		if err := rows.Scan(&r.ID, &r.TenantID, &r.InvoiceID, &r.CustomerID, &r.PolicyID,
			&r.State, &r.Reason, &r.AttemptCount, &r.LastAttemptAt, &r.NextActionAt,
			&r.Paused, &r.ResolvedAt, &r.Resolution, &r.CreatedAt, &r.UpdatedAt,
			&r.InvoiceNumber, &r.InvoiceAmountDue, &r.InvoiceCurrency, &r.EffectiveNow, &r.TestClockID); err != nil {
			return nil, 0, err
		}
		runs = append(runs, r)
	}
	return runs, total, rows.Err()
}

func (s *PostgresStore) UpdateRun(ctx context.Context, tenantID string, run domain.InvoiceDunningRun) (domain.InvoiceDunningRun, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.InvoiceDunningRun{}, err
	}
	defer postgres.Rollback(tx)

	now := clock.Now(ctx)
	_, err = tx.ExecContext(ctx, `
		UPDATE invoice_dunning_runs SET state=$1, reason=$2, attempt_count=$3,
			last_attempt_at=$4, next_action_at=$5, paused=$6, resolved_at=$7, resolution=$8, updated_at=$9
		WHERE id=$10`,
		run.State, postgres.NullableString(run.Reason), run.AttemptCount,
		postgres.NullableTime(run.LastAttemptAt), postgres.NullableTime(run.NextActionAt),
		run.Paused, postgres.NullableTime(run.ResolvedAt), postgres.NullableString(string(run.Resolution)),
		now, run.ID)
	if err != nil {
		return domain.InvoiceDunningRun{}, err
	}
	run.UpdatedAt = now
	if err := tx.Commit(); err != nil {
		return domain.InvoiceDunningRun{}, err
	}
	return run, nil
}

// ResolveRun applies the run's resolved fields as a CAS — only when the row is not
// already 'resolved' — and returns whether THIS call won the transition. Same field
// set as UpdateRun plus the `state <> 'resolved'` guard; the RowsAffected==1 result
// is the exactly-once gate the service uses to fire the resolve side-effects once.
func (s *PostgresStore) ResolveRun(ctx context.Context, tenantID string, run domain.InvoiceDunningRun) (bool, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return false, err
	}
	defer postgres.Rollback(tx)

	now := clock.Now(ctx)
	res, err := tx.ExecContext(ctx, `
		UPDATE invoice_dunning_runs SET state=$1, reason=$2, attempt_count=$3,
			last_attempt_at=$4, next_action_at=$5, paused=$6, resolved_at=$7, resolution=$8, updated_at=$9
		WHERE id=$10 AND state <> 'resolved'`,
		run.State, postgres.NullableString(run.Reason), run.AttemptCount,
		postgres.NullableTime(run.LastAttemptAt), postgres.NullableTime(run.NextActionAt),
		run.Paused, postgres.NullableTime(run.ResolvedAt), postgres.NullableString(string(run.Resolution)),
		now, run.ID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return n == 1, nil
}

// UpdateRunIfActive is UpdateRun guarded on `state <> 'resolved'` — it applies the
// run's fields only when the row has not been concurrently resolved, returning
// whether it applied. See the Store interface doc-comment.
func (s *PostgresStore) UpdateRunIfActive(ctx context.Context, tenantID string, run domain.InvoiceDunningRun) (bool, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return false, err
	}
	defer postgres.Rollback(tx)

	now := clock.Now(ctx)
	res, err := tx.ExecContext(ctx, `
		UPDATE invoice_dunning_runs SET state=$1, reason=$2, attempt_count=$3,
			last_attempt_at=$4, next_action_at=$5, paused=$6, resolved_at=$7, resolution=$8, updated_at=$9
		WHERE id=$10 AND state <> 'resolved'`,
		run.State, postgres.NullableString(run.Reason), run.AttemptCount,
		postgres.NullableTime(run.LastAttemptAt), postgres.NullableTime(run.NextActionAt),
		run.Paused, postgres.NullableTime(run.ResolvedAt), postgres.NullableString(string(run.Resolution)),
		now, run.ID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return n == 1, nil
}

// ListDueRuns — CRON path. ADR-029 Phase 5: dunning runs whose owning
// invoice's subscription is clock-pinned are excluded; the catchup
// orchestrator drives those through ListDueRunsForClock against the
// clock's frozen_time.
func (s *PostgresStore) ListDueRuns(ctx context.Context, tenantID string, dueBefore time.Time, limit int) ([]domain.InvoiceDunningRun, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	if limit <= 0 {
		limit = 20
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT r.id, r.tenant_id, r.invoice_id, COALESCE(r.customer_id,''), r.policy_id, r.state,
			COALESCE(r.reason,''), r.attempt_count, r.last_attempt_at, r.next_action_at,
			r.paused, r.resolved_at, COALESCE(r.resolution,''), r.created_at, r.updated_at
		FROM invoice_dunning_runs r
		WHERE r.next_action_at <= $1 AND r.paused = false
			AND r.state NOT IN ('resolved', 'escalated')
			-- Exclude runs whose invoice is simulated by the invoice's OWN
			-- durable is_simulated flag (not a subscriptions join, which
			-- missed customer-pinned one-offs). The catchup counterpart
			-- ListDueRunsForClock drives simulated dunning in sim time.
			AND NOT EXISTS (
			  SELECT 1 FROM invoices i
			  WHERE i.id = r.invoice_id AND i.is_simulated = true
			)
			-- Payment in flight. ClaimChargeForDunningRetry requires
			-- payment_status IN ('pending','failed'), so while the payment is
			-- 'processing' or 'unknown' the claim CANNOT succeed: the adapter
			-- returns ErrTransientSkip and the transient handler deliberately
			-- does NOT reschedule (a Stripe blip must not burn a tenant's retry
			-- budget). Selecting the run is therefore provably wasted work — it
			-- writes an attempt and immediately rewinds it, every tick, for as
			-- long as the payment stays in flight.
			--
			-- Worse than the waste: next_action_at is frozen while that happens
			-- and this scan is next_action_at ASC with LIMIT, so these runs
			-- migrate to the HEAD of the queue and stay there — at LIMIT of them
			-- real dunning stops for everyone. Same starvation shape ADR-107 hit
			-- in ListUnknownPayments, same cause (oldest-first over rows that do
			-- not move).
			--
			-- WHY THE PREDICATE WIDENED (2026-07-31). It first shipped keyed on
			-- the ADR-107 PARKED shape ('unknown' with no PaymentIntent id) and
			-- carried a comment claiming a 'processing' charge "does resolve on
			-- its own, so skipping it is correct". Both halves were wrong. The
			-- reconciler settles only TERMINAL provider outcomes — reconciler.go
			-- skips processing/requires_action/requires_confirmation/
			-- requires_capture every sweep — so an 'unknown' invoice WITH an id
			-- whose PI sits at requires_action (a PI stalled non-terminal that
			-- nothing advances; NOT "off-session SCA", which raises the
			-- authentication_required DECLINE instead — phrase retracted
			-- 2026-07-31, see ADR-107) never resolves either, and spun exactly
			-- like a parked one. Drawing
			-- the line at "do we hold an id" answered the wrong question; the
			-- right one is "can this claim succeed", and the claim's own
			-- predicate answers it for every in-flight shape at once. This is
			-- domain.IsInFlight expressed in SQL, which is also why it needs no
			-- special case: when the payment leaves flight the row is selectable
			-- again on the very next tick.
			--
			-- SCOPED TO OPEN INVOICES ON PURPOSE. A voided/uncollectible source
			-- must STILL be selected: the processing path's own terminal branch
			-- (service.go, "invoice_"+status) is what resolves the run, and
			-- markUncollectible's ResolveByInvoice is explicitly best-effort,
			-- documented as relying on "dunning runs scan the invoice status on
			-- next tick anyway". Excluding terminal rows here would delete that
			-- backstop and strand the run active forever — the exact bug this
			-- predicate's twin in creditnote/postgres.go was fixed for.
			AND NOT EXISTS (
			  SELECT 1 FROM invoices i
			  WHERE i.id = r.invoice_id
			    AND i.payment_status IN ('processing', 'unknown')
			    AND i.status NOT IN ('voided', 'uncollectible')
			)
		ORDER BY r.next_action_at ASC LIMIT $2
		FOR UPDATE SKIP LOCKED
	`, dueBefore, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var runs []domain.InvoiceDunningRun
	for rows.Next() {
		var r domain.InvoiceDunningRun
		if err := rows.Scan(&r.ID, &r.TenantID, &r.InvoiceID, &r.CustomerID, &r.PolicyID,
			&r.State, &r.Reason, &r.AttemptCount, &r.LastAttemptAt, &r.NextActionAt,
			&r.Paused, &r.ResolvedAt, &r.Resolution, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

// ListDueRunsForClock is the catchup-path counterpart to ListDueRuns.
// ADR-029 Phase 5 — returns dunning runs whose owning invoice's
// subscription is pinned to the given clock and whose next_action_at
// has elapsed against the clock's frozen_time (passed explicitly so
// the comparison runs in simulated time, not wall-clock).
func (s *PostgresStore) ListDueRunsForClock(ctx context.Context, tenantID, clockID string, frozenTime time.Time, limit int) ([]domain.InvoiceDunningRun, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	if limit <= 0 {
		limit = 20
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT r.id, r.tenant_id, r.invoice_id, COALESCE(r.customer_id,''), r.policy_id, r.state,
			COALESCE(r.reason,''), r.attempt_count, r.last_attempt_at, r.next_action_at,
			r.paused, r.resolved_at, COALESCE(r.resolution,''), r.created_at, r.updated_at
		FROM invoice_dunning_runs r
		JOIN invoices i ON i.id = r.invoice_id
		-- Clock membership resolves through CUSTOMERS (the pin's owner,
		-- ADR-027), NOT subscriptions: a one-off invoice has
		-- subscription_id NULL, so the subscriptions join stranded its
		-- dunning run unadvanceable — invisible here AND excluded from
		-- the wall sweep by is_simulated (the #510 class, dunning twin).
		-- A sub's clock always equals its customer's clock (inherit-only,
		-- immutable), so sub-backed runs behave identically.
		JOIN customers c ON c.id = i.customer_id
		WHERE r.tenant_id = $1
			AND c.test_clock_id = $2
			AND r.next_action_at <= $3
			AND r.paused = false
			AND r.state NOT IN ('resolved', 'escalated')
			-- Payment in flight — the sim-time twin of the wall-clock scan's
			-- exclusion above; see there for why the claim can never succeed in
			-- these states, and for why it is scoped to OPEN invoices rather than
			-- all of them. Expressed against the joined row (1:1) instead of a
			-- second NOT EXISTS. A clock advance drives many ticks in a burst, so
			-- the spin this prevents is faster here, not slower: without it one
			-- in-flight sim invoice can consume the whole catchup budget and stall
			-- every other run on the clock.
			AND NOT (
			  i.payment_status IN ('processing', 'unknown')
			  AND i.status NOT IN ('voided', 'uncollectible')
			)
		ORDER BY r.next_action_at ASC LIMIT $4
		FOR UPDATE SKIP LOCKED
	`, tenantID, clockID, frozenTime, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var runs []domain.InvoiceDunningRun
	for rows.Next() {
		var r domain.InvoiceDunningRun
		if err := rows.Scan(&r.ID, &r.TenantID, &r.InvoiceID, &r.CustomerID, &r.PolicyID,
			&r.State, &r.Reason, &r.AttemptCount, &r.LastAttemptAt, &r.NextActionAt,
			&r.Paused, &r.ResolvedAt, &r.Resolution, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

func (s *PostgresStore) CreateEvent(ctx context.Context, tenantID string, event domain.InvoiceDunningEvent) (domain.InvoiceDunningEvent, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.InvoiceDunningEvent{}, err
	}
	defer postgres.Rollback(tx)

	id := postgres.NewID("vlx_devt")
	// Honor caller-supplied CreatedAt so each event row carries the
	// simulated instant the fact actually occurred — started at cycle
	// close, retry #N at its scheduled fire time, escalated at the
	// final retry's instant. Without this, every event in a single
	// catchup pass shares clock.Now(ctx) (= frozen_time) and the
	// invoice timeline shows four facts at one timestamp. Falls back
	// to clock.Now() when zero so wall-clock callers stay correct.
	createdAt := event.CreatedAt
	if createdAt.IsZero() {
		createdAt = clock.Now(ctx)
	}
	metaJSON, _ := json.Marshal(event.Metadata)
	if event.Metadata == nil {
		metaJSON = []byte("{}")
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO invoice_dunning_events (id, run_id, tenant_id, invoice_id,
			event_type, state, reason, attempt_count, metadata, created_at, recorded_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,now())
	`, id, event.RunID, tenantID, event.InvoiceID, event.EventType, event.State,
		postgres.NullableString(event.Reason), event.AttemptCount, metaJSON, createdAt)
	if err != nil {
		return domain.InvoiceDunningEvent{}, err
	}
	event.ID = id
	event.TenantID = tenantID
	event.CreatedAt = createdAt
	if err := tx.Commit(); err != nil {
		return domain.InvoiceDunningEvent{}, err
	}
	return event, nil
}

func (s *PostgresStore) ListEvents(ctx context.Context, tenantID, runID string) ([]domain.InvoiceDunningEvent, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	rows, err := tx.QueryContext(ctx, `
		SELECT id, run_id, tenant_id, invoice_id, event_type, state,
			COALESCE(reason,''), attempt_count, metadata, created_at, recorded_at
		FROM invoice_dunning_events WHERE run_id = $1
		ORDER BY created_at ASC, id ASC
	`, runID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var events []domain.InvoiceDunningEvent
	for rows.Next() {
		var e domain.InvoiceDunningEvent
		var metaJSON []byte
		if err := rows.Scan(&e.ID, &e.RunID, &e.TenantID, &e.InvoiceID,
			&e.EventType, &e.State, &e.Reason, &e.AttemptCount, &metaJSON, &e.CreatedAt, &e.RecordedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(metaJSON, &e.Metadata)
		events = append(events, e)
	}
	return events, rows.Err()
}

// GetStats computes the dashboard-card payload in one round trip:
// COUNT(*) GROUP BY state for the three states we surface (active,
// escalated, resolved) + SUM(amount_due_cents) on the invoices behind
// active+escalated runs (the at-risk total).
//
// Single tenant-scoped query (RLS handles tenant_id), no pagination,
// no client-side derivation. Cards stay accurate regardless of how
// many runs exist.
func (s *PostgresStore) GetStats(ctx context.Context, tenantID string) (Stats, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return Stats{}, err
	}
	defer postgres.Rollback(tx)

	// Resolve the tenant's default currency once. The at-risk total is scoped
	// to it so we never sum amount_due across currencies into a corrupt figure
	// (a EUR invoice's cents are not a USD invoice's cents). Defaults to USD
	// when settings are unset.
	defaultCurrency := "USD"
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(NULLIF(default_currency, ''), 'USD') FROM tenant_settings LIMIT 1`,
	).Scan(&defaultCurrency); err != nil && err != sql.ErrNoRows {
		return Stats{}, err
	}

	var stats Stats
	stats.Currency = defaultCurrency
	err = tx.QueryRowContext(ctx, `
		SELECT
		    COUNT(*) FILTER (WHERE r.state = 'active')                                AS active_count,
		    COUNT(*) FILTER (WHERE r.state = 'escalated')                             AS escalated_count,
		    COUNT(*) FILTER (WHERE r.state = 'resolved')                              AS resolved_count,
		    COALESCE(SUM(i.amount_due_cents) FILTER (WHERE r.state IN ('active','escalated') AND i.currency = $1), 0) AS at_risk_cents
		FROM invoice_dunning_runs r
		LEFT JOIN invoices i ON i.id = r.invoice_id
	`, defaultCurrency).Scan(&stats.ActiveCount, &stats.EscalatedCount, &stats.ResolvedCount, &stats.AtRiskCents)
	if err != nil {
		return Stats{}, err
	}
	return stats, nil
}

// Customer dunning overrides (GetCustomerOverride / UpsertCustomerOverride
// / DeleteCustomerOverride) were removed in ADR-036. Per-customer
// differentiation now flows through customers.dunning_policy_id
// referencing a DunningPolicy row, matching the Stripe / Lago / Orb /
// Recurly campaigns-model shape.
