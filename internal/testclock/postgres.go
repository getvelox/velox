package testclock

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

const clockCols = `id, tenant_id, name, frozen_time, status,
	created_at, updated_at,
	COALESCE(last_failure_reason,''), last_advance_summary`

func (s *PostgresStore) Create(ctx context.Context, tenantID string, clk domain.TestClock) (domain.TestClock, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.TestClock{}, err
	}
	defer postgres.Rollback(tx)

	err = tx.QueryRowContext(ctx, `
		INSERT INTO test_clocks (tenant_id, name, frozen_time, status)
		VALUES ($1, $2, $3, 'ready')
		RETURNING `+clockCols,
		tenantID, clk.Name, clk.FrozenTime,
	).Scan(scanDest(&clk)...)
	if err != nil {
		// 23514 = check_violation; the livemode CHECK rejects any attempt to
		// insert a test clock from a live-mode session. Surface as 400 invalid
		// instead of leaking a raw SQL error string.
		if postgres.IsCheckViolation(err) {
			return domain.TestClock{}, errs.Invalid("livemode", "test clocks can only be created in test mode")
		}
		return domain.TestClock{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.TestClock{}, err
	}
	return clk, nil
}

// ResolveSim answers "is this clock attached-able, and what instant does it
// stand at?" in ONE read — used by the customer service at customer-create
// time, which must both validate the pin and bind the new customer's audit row
// onto the sim axis (ADR-090 §5).
//
// It returns the pair from a single Get precisely so the two halves cannot
// drift: an "exists?" bool would send the caller back for the instant, and a
// clock id paired with a separately-fetched instant is the half-truth
// clock.Sim is designed to make unrepresentable.
//
// ErrNotFound → (zero, false, nil); other errors propagate. Soft-deleted clocks
// count as not-existing.
func (s *PostgresStore) ResolveSim(ctx context.Context, tenantID, id string) (clock.Sim, bool, error) {
	clk, err := s.Get(ctx, tenantID, id)
	if err != nil {
		if errors.Is(err, errs.ErrNotFound) {
			return clock.Sim{}, false, nil
		}
		return clock.Sim{}, false, err
	}
	return clock.Sim{At: clk.FrozenTime, TestClockID: clk.ID}, true, nil
}

func (s *PostgresStore) Get(ctx context.Context, tenantID, id string) (domain.TestClock, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.TestClock{}, err
	}
	defer postgres.Rollback(tx)

	var clk domain.TestClock
	err = tx.QueryRowContext(ctx, `SELECT `+clockCols+` FROM test_clocks WHERE id = $1`, id).
		Scan(scanDest(&clk)...)
	if err == sql.ErrNoRows {
		return domain.TestClock{}, errs.ErrNotFound
	}
	return clk, err
}

func (s *PostgresStore) List(ctx context.Context, tenantID string) ([]domain.TestClock, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	rows, err := tx.QueryContext(ctx, `
		SELECT `+clockCols+` FROM test_clocks
		ORDER BY created_at DESC LIMIT 500
	`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var clocks []domain.TestClock
	for rows.Next() {
		var clk domain.TestClock
		if err := rows.Scan(scanDest(&clk)...); err != nil {
			return nil, err
		}
		clocks = append(clocks, clk)
	}
	return clocks, rows.Err()
}

// clockTeardownStatements are the ordered DELETEs that tear down a test
// clock's ENTIRE simulated customer graph (ADR-086, Design B). Order is
// bottom-up so the customer_id / invoice_id RESTRICT FKs (migration 0015)
// never trip — every child is gone before its parent. Every statement is
// transitively keyed on the clock's customer set `test_clock_id = $1`, so
// only livemode=false rows are ever reachable (a live customer has
// test_clock_id NULL; a test clock is always livemode=false by CHECK).
//
// Tables with ON DELETE CASCADE from a parent (subscription_items /
// _changes / _thresholds via subscriptions; payment_methods / portal via
// customers) would be removed automatically, but the payment_methods and
// portal rows are deleted explicitly so this list is the complete,
// self-documenting record the completeness arch-test asserts against.
//
// NOT torn down (survivors): the audit log and all tenant config — meters,
// plans, coupons, dunning_policies, api_keys, stripe_webhook_events. Keep
// this list in sync with TestTeardownCoversEverySimulatedTable.
var clockTeardownStatements = []string{
	// Invoice sub-graph — children first (leaves have no customer_id; keyed via parent).
	`DELETE FROM invoice_dunning_events WHERE invoice_id IN (SELECT id FROM invoices WHERE customer_id IN (SELECT id FROM customers WHERE test_clock_id = $1))`,
	`DELETE FROM invoice_dunning_runs   WHERE invoice_id IN (SELECT id FROM invoices WHERE customer_id IN (SELECT id FROM customers WHERE test_clock_id = $1))`,
	`DELETE FROM tax_calculations       WHERE invoice_id IN (SELECT id FROM invoices WHERE customer_id IN (SELECT id FROM customers WHERE test_clock_id = $1))`,
	`DELETE FROM credit_note_line_items WHERE credit_note_id IN (SELECT id FROM credit_notes WHERE customer_id IN (SELECT id FROM customers WHERE test_clock_id = $1))`,
	`DELETE FROM credit_notes           WHERE customer_id IN (SELECT id FROM customers WHERE test_clock_id = $1)`,
	`DELETE FROM invoice_line_items     WHERE invoice_id IN (SELECT id FROM invoices WHERE customer_id IN (SELECT id FROM customers WHERE test_clock_id = $1))`,
	`DELETE FROM payment_update_tokens  WHERE customer_id IN (SELECT id FROM customers WHERE test_clock_id = $1)`,
	`DELETE FROM coupon_redemptions     WHERE customer_id IN (SELECT id FROM customers WHERE test_clock_id = $1)`,
	`DELETE FROM invoices               WHERE customer_id IN (SELECT id FROM customers WHERE test_clock_id = $1)`,
	// Usage + credit — the ledger MUST precede subscriptions: its
	// source_subscription_item_id RESTRICT-references subscription_items,
	// which cascades away when subscriptions is deleted.
	`DELETE FROM billed_entries         WHERE customer_id IN (SELECT id FROM customers WHERE test_clock_id = $1)`,
	`DELETE FROM usage_events           WHERE customer_id IN (SELECT id FROM customers WHERE test_clock_id = $1)`,
	`DELETE FROM customer_credit_ledger WHERE customer_id IN (SELECT id FROM customers WHERE test_clock_id = $1)`,
	// Subscriptions — CASCADE removes subscription_items / _changes /
	// _thresholds / billing_intervals (ADR-101; the intervals FK is the
	// ADR-086 teardown carve-out).
	`DELETE FROM subscriptions          WHERE customer_id IN (SELECT id FROM customers WHERE test_clock_id = $1)`,
	// Per-customer config + portal (customer_discounts / payment_methods /
	// portal also ON DELETE CASCADE from the customer; deleted explicitly so
	// the set is the complete self-documenting record the arch-test asserts).
	`DELETE FROM customer_price_overrides    WHERE customer_id IN (SELECT id FROM customers WHERE test_clock_id = $1)`,
	`DELETE FROM customer_billing_profiles   WHERE customer_id IN (SELECT id FROM customers WHERE test_clock_id = $1)`,
	`DELETE FROM customer_discounts          WHERE customer_id IN (SELECT id FROM customers WHERE test_clock_id = $1)`,
	`DELETE FROM payment_methods             WHERE customer_id IN (SELECT id FROM customers WHERE test_clock_id = $1)`,
	`DELETE FROM customer_portal_sessions    WHERE customer_id IN (SELECT id FROM customers WHERE test_clock_id = $1)`,
	`DELETE FROM customer_portal_magic_links WHERE customer_id IN (SELECT id FROM customers WHERE test_clock_id = $1)`,
	// The customers themselves LAST (before the clock): customers.test_clock_id
	// is ON DELETE SET NULL, so deleting the clock first would silently detach
	// every customer and leak the whole set.
	`DELETE FROM customers WHERE test_clock_id = $1`,
}

// Delete tears down a test clock and its ENTIRE simulated customer graph in
// one transaction (ADR-086, Design B — complete teardown, supersedes ADR-016's
// soft-delete + detach). After it runs no simulated row survives, so no
// wall-clock plane (auto-charge, dunning, usage-aggregation, credit-expiry,
// analytics) can ever act on one — the class the ADR dissolves.
//
// Safety: see clockTeardownStatements — every DELETE is keyed transitively on
// `test_clock_id = $clock`, and a test clock is always livemode=false, so no
// live-mode row is reachable; the clock row's own DELETE adds `livemode=false`
// as belt. Runs under TxTenant (RLS confines it to the tenant).
//
// Idempotent: re-deleting a gone clock returns errs.ErrNotFound.
func (s *PostgresStore) Delete(ctx context.Context, tenantID, id string) error {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)

	// Existence first: tearing down a gone clock hits an empty customer set
	// (harmless), but the contract is ErrNotFound.
	var exists bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM test_clocks WHERE id = $1)`, id).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return errs.ErrNotFound
	}

	for i, stmt := range clockTeardownStatements {
		if _, err := tx.ExecContext(ctx, stmt, id); err != nil {
			return fmt.Errorf("clock teardown step %d: %w", i, err)
		}
	}

	// The clock row itself, last — its customers are already gone, so the
	// ON DELETE SET NULL fires on nothing. Guarded livemode=false.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM test_clocks WHERE id = $1 AND livemode = false`, id); err != nil {
		return fmt.Errorf("delete test clock: %w", err)
	}

	return tx.Commit()
}

func (s *PostgresStore) MarkAdvancing(ctx context.Context, tenantID, id string, newFrozenTime time.Time) (domain.TestClock, error) {
	return s.transition(ctx, tenantID, id, "advancing", []string{"ready"},
		&newFrozenTime, "can only advance a clock in status=ready")
}

// CompleteAdvance flips advancing → ready and clears any prior
// last_failure_reason. The clear matters when the operator
// retried a failed advance and it succeeded — the dashboard
// shouldn't keep showing yesterday's error.
func (s *PostgresStore) CompleteAdvance(ctx context.Context, tenantID, id string) (domain.TestClock, error) {
	return s.transitionWithReason(ctx, tenantID, id, "ready",
		[]string{"advancing"}, "", true,
		"can only complete an advance from status=advancing")
}

// MarkFailed flips advancing → internal_failure and persists the
// reason. Caller truncates to ~500 chars; full payload stays in
// structured slog.
func (s *PostgresStore) MarkFailed(ctx context.Context, tenantID, id, reason string) (domain.TestClock, error) {
	return s.transitionWithReason(ctx, tenantID, id, "internal_failure",
		[]string{"advancing"}, reason, false,
		"can only mark failed from status=advancing")
}

// RetryFromFailed flips internal_failure → advancing, clearing
// the failure reason. Frozen_time stays at its current value —
// the catchup loop is idempotent on subs whose next_billing_at
// has already passed it. ADR-018.
func (s *PostgresStore) RetryFromFailed(ctx context.Context, tenantID, id string) (domain.TestClock, error) {
	return s.transitionWithReason(ctx, tenantID, id, "advancing",
		[]string{"internal_failure"}, "", true,
		"can only retry from status=internal_failure")
}

// transitionWithReason is the CAS helper for transitions that
// either write or clear last_failure_reason. clearReason=true
// means SET last_failure_reason = NULL; false means SET
// last_failure_reason = $reason. Used by CompleteAdvance,
// MarkFailed, RetryFromFailed.
func (s *PostgresStore) transitionWithReason(
	ctx context.Context, tenantID, id, to string, allowedFrom []string,
	reason string, clearReason bool, wrongStateMsg string,
) (domain.TestClock, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.TestClock{}, err
	}
	defer postgres.Rollback(tx)

	var clk domain.TestClock
	var query string
	var args []any
	if clearReason {
		query = `UPDATE test_clocks SET status = $1, last_failure_reason = NULL,
			updated_at = now()
			WHERE id = $2 AND status = ANY($3)			RETURNING ` + clockCols
		args = []any{to, id, postgres.StringArray(allowedFrom)}
	} else {
		query = `UPDATE test_clocks SET status = $1, last_failure_reason = $2,
			updated_at = now()
			WHERE id = $3 AND status = ANY($4)			RETURNING ` + clockCols
		args = []any{to, truncateReason(reason), id, postgres.StringArray(allowedFrom)}
	}
	err = tx.QueryRowContext(ctx, query, args...).Scan(scanDest(&clk)...)
	if err == sql.ErrNoRows {
		var current string
		err2 := tx.QueryRowContext(ctx, `SELECT status FROM test_clocks WHERE id = $1`, id).Scan(&current)
		if err2 == sql.ErrNoRows {
			return domain.TestClock{}, errs.ErrNotFound
		}
		if err2 != nil {
			return domain.TestClock{}, err2
		}
		return domain.TestClock{}, errs.InvalidState(fmt.Sprintf("%s, current status: %s", wrongStateMsg, current))
	}
	if err != nil {
		return domain.TestClock{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.TestClock{}, err
	}
	return clk, nil
}

// truncateReason caps the failure reason at a length the
// dashboard can render in a single inline panel without scroll.
// Full payload stays in slog for ops grep.
func truncateReason(s string) string {
	const max = 500
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}

// transition is the atomic CAS helper for status changes: UPDATE … WHERE
// status = ANY(allowedFrom) returning the row. Returning zero rows is
// ambiguous (missing row vs wrong state), so a second lookup distinguishes.
func (s *PostgresStore) transition(ctx context.Context, tenantID, id, to string, allowedFrom []string, frozenTime *time.Time, wrongStateMsg string) (domain.TestClock, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.TestClock{}, err
	}
	defer postgres.Rollback(tx)

	var clk domain.TestClock
	var query string
	var args []any
	if frozenTime != nil {
		query = `UPDATE test_clocks SET status = $1, frozen_time = $2, updated_at = now()
			WHERE id = $3 AND status = ANY($4) RETURNING ` + clockCols
		args = []any{to, *frozenTime, id, postgres.StringArray(allowedFrom)}
	} else {
		query = `UPDATE test_clocks SET status = $1, updated_at = now()
			WHERE id = $2 AND status = ANY($3) RETURNING ` + clockCols
		args = []any{to, id, postgres.StringArray(allowedFrom)}
	}
	err = tx.QueryRowContext(ctx, query, args...).Scan(scanDest(&clk)...)
	if err == sql.ErrNoRows {
		var current string
		err2 := tx.QueryRowContext(ctx, `SELECT status FROM test_clocks WHERE id = $1`, id).Scan(&current)
		if err2 == sql.ErrNoRows {
			return domain.TestClock{}, errs.ErrNotFound
		}
		if err2 != nil {
			return domain.TestClock{}, err2
		}
		return domain.TestClock{}, errs.InvalidState(fmt.Sprintf("%s, current status: %s", wrongStateMsg, current))
	}
	if err != nil {
		return domain.TestClock{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.TestClock{}, err
	}
	return clk, nil
}

// ListSubscriptionsOnClock returns subscriptions attached to the given clock.
// This crosses into the subscription table on purpose — the test clock owns
// the catchup orchestration and needs the sub rows to drive it. We keep the
// query narrow (id + fields the catchup consults) to avoid duplicating the
// subscription package's full scan.
func (s *PostgresStore) ListSubscriptionsOnClock(ctx context.Context, tenantID, clockID string) ([]domain.Subscription, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	// Items aren't hydrated here — this list is used by the test clock
	// advance path which only reads scheduling fields (trial, billing period,
	// next_billing_at). If a caller needs item data they should go through
	// the subscription package's Get/List which hydrate Items explicitly.
	rows, err := tx.QueryContext(ctx, `
		SELECT id, tenant_id, code, customer_id, status,
			trial_end_at, current_billing_period_start, current_billing_period_end,
			next_billing_at,
			COALESCE(test_clock_id,''), created_at, updated_at
		FROM subscriptions
		WHERE test_clock_id = $1
		ORDER BY created_at ASC LIMIT 1000
	`, clockID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var subs []domain.Subscription
	for rows.Next() {
		var sub domain.Subscription
		if err := rows.Scan(&sub.ID, &sub.TenantID, &sub.Code, &sub.CustomerID,
			&sub.Status, &sub.TrialEndAt, &sub.CurrentBillingPeriodStart,
			&sub.CurrentBillingPeriodEnd, &sub.NextBillingAt,
			&sub.TestClockID, &sub.CreatedAt, &sub.UpdatedAt); err != nil {
			return nil, err
		}
		subs = append(subs, sub)
	}
	return subs, rows.Err()
}

// ListAllAdvancing scans test_clocks for rows in status='advancing'
// across every tenant. Used at boot to recover catchup jobs that
// were in-flight when the previous process exited. RLS-bypassed
// (TxBypass) because the recovery path runs before any tenant ctx
// is established and needs to see all tenants. Limited to 1000 to
// bound the recovery enqueue burst — at expected pre-launch
// volumes there will rarely be more than 0-1 stuck clocks; the
// limit is a sanity cap, not a paging target.
func (s *PostgresStore) ListAllAdvancing(ctx context.Context) ([]domain.TestClock, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	rows, err := tx.QueryContext(ctx, `
		SELECT `+clockCols+` FROM test_clocks
		WHERE status = 'advancing'		ORDER BY updated_at ASC
		LIMIT 1000
	`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var clocks []domain.TestClock
	for rows.Next() {
		var clk domain.TestClock
		if err := rows.Scan(scanDest(&clk)...); err != nil {
			return nil, err
		}
		clocks = append(clocks, clk)
	}
	return clocks, rows.Err()
}

func scanDest(c *domain.TestClock) []any {
	return []any{
		&c.ID, &c.TenantID, &c.Name, &c.FrozenTime, &c.Status,
		&c.CreatedAt, &c.UpdatedAt,
		&c.LastFailureReason, advanceSummaryScan{&c.LastAdvanceSummary},
	}
}

// advanceSummaryScan adapts the nullable last_advance_summary JSONB column to
// the *domain.AdvanceSummary field: NULL (or empty) → nil, otherwise unmarshal.
type advanceSummaryScan struct{ dst **domain.AdvanceSummary }

func (a advanceSummaryScan) Scan(src any) error {
	if src == nil {
		*a.dst = nil
		return nil
	}
	var b []byte
	switch v := src.(type) {
	case []byte:
		b = v
	case string:
		b = []byte(v)
	default:
		return fmt.Errorf("last_advance_summary: unexpected scan type %T", src)
	}
	if len(b) == 0 {
		*a.dst = nil
		return nil
	}
	var s domain.AdvanceSummary
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("decode last_advance_summary: %w", err)
	}
	*a.dst = &s
	return nil
}

// SaveAdvanceSummary persists the per-phase counts the catchup produced onto
// the clock row. Written by RunCatchup just before the advancing → ready /
// internal_failure transition, so the transition's RETURNING clause reads it
// back. A plain column write (not gated on status): the catchup is the only
// writer and the clock is 'advancing' at this point. Best-effort relative to
// the transition — if the process dies between the two writes the clock simply
// shows no summary, which is strictly better than blocking the transition.
func (s *PostgresStore) SaveAdvanceSummary(ctx context.Context, tenantID, id string, summaryJSON []byte) error {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)

	if _, err := tx.ExecContext(ctx,
		`UPDATE test_clocks SET last_advance_summary = $1::jsonb, updated_at = now()
		 WHERE id = $2`,
		summaryJSON, id,
	); err != nil {
		return err
	}
	return tx.Commit()
}
