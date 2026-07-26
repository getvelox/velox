package postgres_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestRLSIsolation_ClosedBypassTables asserts that the tenant_isolation policy
// added in migration 0006 actually blocks cross-tenant reads on the three
// tables that were previously unprotected: tenant_settings, idempotency_keys,
// payment_update_tokens.
//
// Row-level checks cover tenant_settings and idempotency_keys (no heavy
// FK chain). payment_update_tokens shares the identical policy definition and
// is covered by the metadata assertion plus its own integration path.
func TestRLSIsolation_ClosedBypassTables(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), true), 10*time.Second)
	defer cancel()

	tenantA := testutil.CreateTestTenant(t, db, "RLS Test A")
	tenantB := testutil.CreateTestTenant(t, db, "RLS Test B")

	assertRLSEnabled(t, db, ctx, "tenant_settings")
	assertRLSEnabled(t, db, ctx, "idempotency_keys")
	assertRLSEnabled(t, db, ctx, "payment_update_tokens")

	seedSettingsAndIdempotency(t, db, ctx, tenantA, "key-A")
	seedSettingsAndIdempotency(t, db, ctx, tenantB, "key-B")

	// Both tables: TxTenant(A) with no WHERE filter must see exactly one row
	// (A's), never B's.
	assertOnlyTenantVisible(t, db, ctx, tenantA,
		`SELECT tenant_id FROM tenant_settings`, 1)
	assertOnlyTenantVisible(t, db, ctx, tenantA,
		`SELECT tenant_id FROM idempotency_keys`, 1)

	// Write isolation: acting as A, inserting a row tagged with B's id must fail.
	assertInsertDenied(t, db, ctx, tenantA, tenantB)
}

func assertRLSEnabled(t *testing.T, db *postgres.DB, ctx context.Context, tableName string) {
	t.Helper()

	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin bypass: %v", err)
	}
	defer postgres.Rollback(tx)

	var relrowsecurity, relforcerowsecurity bool
	err = tx.QueryRowContext(ctx,
		`SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = $1`,
		tableName,
	).Scan(&relrowsecurity, &relforcerowsecurity)
	if err != nil {
		t.Fatalf("read pg_class for %s: %v", tableName, err)
	}
	if !relrowsecurity {
		t.Fatalf("%s: ROW LEVEL SECURITY is not ENABLED", tableName)
	}
	if !relforcerowsecurity {
		t.Fatalf("%s: ROW LEVEL SECURITY is not FORCED", tableName)
	}

	var policyCount int
	err = tx.QueryRowContext(ctx,
		`SELECT count(*) FROM pg_policies WHERE tablename = $1 AND policyname = 'tenant_isolation'`,
		tableName,
	).Scan(&policyCount)
	if err != nil {
		t.Fatalf("read pg_policies for %s: %v", tableName, err)
	}
	if policyCount != 1 {
		t.Fatalf("%s: expected exactly one tenant_isolation policy, got %d", tableName, policyCount)
	}
}

func seedSettingsAndIdempotency(t *testing.T, db *postgres.DB, ctx context.Context,
	tenantID, idemKey string) {
	t.Helper()

	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("seed begin: %v", err)
	}
	defer postgres.Rollback(tx)

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO tenant_settings (tenant_id) VALUES ($1)
		ON CONFLICT (tenant_id) DO NOTHING`, tenantID); err != nil {
		t.Fatalf("seed tenant_settings for %s: %v", tenantID, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO idempotency_keys (tenant_id, key, http_method, http_path, status_code, response_body)
		VALUES ($1, $2, 'POST', '/v1/test', 200, '{}')`,
		tenantID, idemKey); err != nil {
		t.Fatalf("seed idempotency_keys for %s: %v", tenantID, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("seed commit: %v", err)
	}
}

func assertOnlyTenantVisible(t *testing.T, db *postgres.DB, ctx context.Context,
	tenantID, query string, expected int) {
	t.Helper()

	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin tenant tx: %v", err)
	}
	defer postgres.Rollback(tx)

	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	defer func() { _ = rows.Close() }()

	count := 0
	for rows.Next() {
		var seen string
		if err := rows.Scan(&seen); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if seen != tenantID {
			t.Fatalf("RLS LEAK: query %q returned row for tenant %q while context is %q",
				query, seen, tenantID)
		}
		count++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows err: %v", err)
	}
	if count != expected {
		t.Fatalf("query %q: expected %d row(s) visible to tenant %s, got %d",
			query, expected, tenantID, count)
	}
}

func assertInsertDenied(t *testing.T, db *postgres.DB, ctx context.Context,
	actingTenant, targetTenant string) {
	t.Helper()

	tx, err := db.BeginTx(ctx, postgres.TxTenant, actingTenant)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)

	_, err = tx.ExecContext(ctx,
		`INSERT INTO idempotency_keys (tenant_id, key, http_method, http_path, status_code, response_body)
		VALUES ($1, 'hostile-key', 'POST', '/v1/test', 200, '{}')`,
		targetTenant)
	if err == nil {
		t.Fatalf("RLS LEAK: tenant %s successfully inserted a row tagged with tenant %s",
			actingTenant, targetTenant)
	}
}

// TestRLSIsolation_Livemode asserts that mode-aware rows are partitioned by
// app.livemode — a row written under test mode is invisible to live-mode
// reads for the same tenant, and vice versa. This is the last line of defence
// against test-mode data leaking into production responses (or vice versa)
// when a caller forgets to propagate WithLivemode through the request ctx.
//
// The BEFORE INSERT trigger from migration 0021 is also covered indirectly:
// we never set NEW.livemode explicitly — the trigger reads app.livemode off
// the session and stamps the row. If the trigger regressed, both seeded rows
// would land on the same mode and one of the assertions below would fire.
func TestRLSIsolation_Livemode(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), true), 10*time.Second)
	defer cancel()

	tenantID := testutil.CreateTestTenant(t, db, "Livemode RLS Test")

	// Seed distinct keys under each mode for the same tenant. We use
	// idempotency_keys because its primary key is (tenant_id, livemode, key)
	// post-migration 0020 — the same literal key can coexist across modes.
	seedIdempotencyKeyForMode(t, db, ctx, tenantID, true, "live-key")
	seedIdempotencyKeyForMode(t, db, ctx, tenantID, false, "test-key")

	// Live-mode session: sees live-key only.
	assertLivemodeVisibleKeys(t, db, ctx, tenantID, true,
		[]string{"live-key"}, []string{"test-key"})

	// Test-mode session: sees test-key only.
	assertLivemodeVisibleKeys(t, db, ctx, tenantID, false,
		[]string{"test-key"}, []string{"live-key"})

	// Trigger-level enforcement: an INSERT under test mode cannot smuggle in a
	// livemode=true row by passing it explicitly. The trigger overwrites
	// NEW.livemode from the session, so the row lands as test-mode regardless.
	assertLivemodeTriggerOverridesCaller(t, db, ctx, tenantID)
}

// TestRLSIsolation_AllModeAwareTablesHaveLivemodePredicate derives the
// mode-aware table set from the schema itself — every table carrying a
// tenant_isolation policy — and asserts each one is mode-partitioned or
// explicitly justified as mode-neutral. An earlier version of this test
// checked a hand-maintained table list, and recipe_instances shipped
// livemode-BLIND for two months precisely because nothing forced a new
// tenant table onto that list (found 2026-07-26: a test-mode recipe install
// silently no-op'd the live-mode install of the same recipe). Self-deriving
// closes that hole: a new tenant-isolated table fails here until it either
// gets the 0020 treatment or earns an allowlist entry with a justification.
func TestRLSIsolation_AllModeAwareTablesHaveLivemodePredicate(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), true), 10*time.Second)
	defer cancel()

	// Tables that carry tenant_isolation but deliberately NO livemode column.
	// The safety rule for membership: a mode-blind tenant table is safe only
	// if every lookup against it rides an FK to a mode-aware parent (the row
	// inherits mode through the referenced PK — 0020's FK-inheritance
	// argument), or the table is genuinely account-level. A NATURAL-key
	// lookup (recipe_instances.recipe_key, pre-0157) is the shape that
	// breaks: the key resolves across modes, so gates reading it act on the
	// other mode's state.
	modeNeutralByDesign := map[string]string{
		"tenant_settings":              "account-level config, shared across modes by design (0020)",
		"user_tenants":                 "account-level membership; no mode axis",
		"member_invitations":           "account-level invites; unique key is a random token hash",
		"customer_discounts":           "FK-scoped to customers (mode-aware parent)",
		"meter_pricing_rules":          "unique key is (meter_id, rating_rule_version_id) — both FK mode-aware parents",
		"subscription_item_thresholds": "FK-scoped to subscription items (mode-aware parent)",
		"tax_calculations":             "FK-scoped to invoices (mode-aware parent)",
	}

	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin bypass: %v", err)
	}
	defer postgres.Rollback(tx)

	rows, err := tx.QueryContext(ctx, `
		SELECT p.tablename, p.qual,
		       EXISTS (
		           SELECT 1 FROM information_schema.columns c
		           WHERE c.table_schema = p.schemaname
		             AND c.table_name  = p.tablename
		             AND c.column_name = 'livemode'
		       ) AS has_livemode
		FROM pg_policies p
		WHERE p.policyname = 'tenant_isolation'
		ORDER BY p.tablename
	`)
	if err != nil {
		t.Fatalf("scan pg_policies: %v", err)
	}
	defer func() { _ = rows.Close() }()

	seen := map[string]bool{}
	for rows.Next() {
		var table, qual string
		var hasLivemode bool
		if err := rows.Scan(&table, &qual, &hasLivemode); err != nil {
			t.Fatalf("scan policy row: %v", err)
		}
		seen[table] = true

		if !hasLivemode {
			if _, ok := modeNeutralByDesign[table]; !ok {
				t.Errorf("%s: tenant-isolated table has NO livemode column and no mode-neutral justification — give it the 0020 treatment (column + widened UNIQUEs + policy predicate + set_livemode trigger) or add it to modeNeutralByDesign with the reason it is safe",
					table)
			}
			continue
		}
		if _, ok := modeNeutralByDesign[table]; ok {
			t.Errorf("%s: listed as mode-neutral but now HAS a livemode column — prune the stale allowlist entry", table)
		}
		// The policy qual is the expanded form of the USING clause. Postgres
		// normalises it, so the exact string shape can shift across versions —
		// we just check for the app.livemode reference that the 0020 block
		// installs on every mode-aware table.
		if !containsLivemodePredicate(qual) {
			t.Errorf("%s: tenant_isolation policy qual does NOT reference app.livemode — mode partitioning is missing. qual was: %s",
				table, qual)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	for table := range modeNeutralByDesign {
		if !seen[table] {
			t.Errorf("%s: on the mode-neutral allowlist but has no tenant_isolation policy (dropped or renamed?) — prune the stale entry", table)
		}
	}
}

func seedIdempotencyKeyForMode(t *testing.T, db *postgres.DB, ctx context.Context,
	tenantID string, live bool, key string) {
	t.Helper()

	modeCtx := postgres.WithLivemode(ctx, live)
	tx, err := db.BeginTx(modeCtx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("seed begin (live=%v): %v", live, err)
	}
	defer postgres.Rollback(tx)

	if _, err := tx.ExecContext(modeCtx,
		`INSERT INTO idempotency_keys (tenant_id, key, http_method, http_path, status_code, response_body)
		VALUES ($1, $2, 'POST', '/v1/test', 200, '{}')`,
		tenantID, key); err != nil {
		t.Fatalf("seed idempotency_keys (tenant=%s live=%v key=%s): %v",
			tenantID, live, key, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("seed commit (live=%v): %v", live, err)
	}
}

func assertLivemodeVisibleKeys(t *testing.T, db *postgres.DB, ctx context.Context,
	tenantID string, live bool, expectVisible, expectHidden []string) {
	t.Helper()

	modeCtx := postgres.WithLivemode(ctx, live)
	tx, err := db.BeginTx(modeCtx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin tenant tx (live=%v): %v", live, err)
	}
	defer postgres.Rollback(tx)

	rows, err := tx.QueryContext(modeCtx,
		`SELECT key FROM idempotency_keys WHERE tenant_id = $1`, tenantID)
	if err != nil {
		t.Fatalf("query idempotency_keys (live=%v): %v", live, err)
	}
	defer func() { _ = rows.Close() }()

	seen := map[string]bool{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatalf("scan (live=%v): %v", live, err)
		}
		seen[k] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows err (live=%v): %v", live, err)
	}

	for _, k := range expectVisible {
		if !seen[k] {
			t.Errorf("live=%v: expected key %q visible, got hidden", live, k)
		}
	}
	for _, k := range expectHidden {
		if seen[k] {
			t.Errorf("RLS LEAK: live=%v saw key %q that belongs to the other mode", live, k)
		}
	}
}

func assertLivemodeTriggerOverridesCaller(t *testing.T, db *postgres.DB,
	ctx context.Context, tenantID string) {
	t.Helper()

	// Open a test-mode tx (app.livemode='off') and INSERT an idempotency key
	// while explicitly passing livemode=true. The BEFORE INSERT trigger from
	// migration 0021 must clobber the caller-supplied value with the session
	// value (false). After commit, a test-mode SELECT should see the row and
	// a live-mode SELECT should not.
	testCtx := postgres.WithLivemode(ctx, false)
	tx, err := db.BeginTx(testCtx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin test-mode tx: %v", err)
	}
	defer postgres.Rollback(tx)

	// NB: explicit livemode=true in the column list — the trigger should
	// reject this implicitly by overwriting, not by raising.
	_, err = tx.ExecContext(testCtx,
		`INSERT INTO idempotency_keys (tenant_id, key, livemode, http_method, http_path, status_code, response_body)
		VALUES ($1, 'trigger-override-key', true, 'POST', '/v1/test', 200, '{}')`,
		tenantID)
	if err != nil {
		t.Fatalf("insert with explicit livemode=true under test-mode session failed unexpectedly: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit test-mode insert: %v", err)
	}

	// A bypass SELECT (policy off) confirms the stored value — ground truth
	// rather than relying on another RLS-filtered query to prove the point.
	bypassTx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin bypass for verify: %v", err)
	}
	defer postgres.Rollback(bypassTx)

	var stored bool
	err = bypassTx.QueryRowContext(ctx,
		`SELECT livemode FROM idempotency_keys WHERE tenant_id = $1 AND key = $2`,
		tenantID, "trigger-override-key",
	).Scan(&stored)
	if err != nil {
		t.Fatalf("bypass read stored livemode: %v", err)
	}
	if stored {
		t.Fatalf("trigger REGRESSION: caller-supplied livemode=true was persisted under a test-mode session; livemode partitioning is compromised")
	}
}

func containsLivemodePredicate(qual string) bool {
	// pg_policies.qual renders session-var reads as current_setting(...). A
	// missing livemode clause produces a qual that mentions app.tenant_id only.
	return strings.Contains(qual, "app.livemode")
}
