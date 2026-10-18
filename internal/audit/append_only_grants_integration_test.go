package audit_test

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// runtimeRoleURL is the PRODUCTION runtime role (velox_app) — the role
// cmd/velox connects as via APP_DATABASE_URL, and the role migration 0150
// revokes the audit_log write grants from. Since v0.9.0, 0150 is part of
// the v0.8.1 baseline: the baseline's GRANT lines carry the result, and
// scripts/gen-baseline.sh appends 0150's REVOKE (rule R7) so installs whose
// velox_app has default privileges end the same way (ADR-125).
//
// Note this is deliberately NOT testutil's TEST_DATABASE_URL (velox_test_app).
// velox_test_app is test-only scaffolding (docker/init.sql + ci.yml) that picks
// up ALL on every table through ALTER DEFAULT PRIVILEGES, so it is NOT affected
// by 0150 — which is exactly what makes it useful below as the "still holds the
// grants" role that proves the triggers are an independent second layer.
const defaultRuntimeRoleURL = "postgres://velox_app:velox_app@localhost:5432/velox_test?sslmode=disable"

// runtimeRoleURL resolves the velox_app (runtime-role) connection string.
//
// It DERIVES from the suite's own TEST_DATABASE_URL rather than hardcoding a
// database name: the suite can be pointed at a throwaway database (parallel
// agents, CI matrices), and a hardcoded "velox_test" would then assert against
// the WRONG database — passing while the database under test kept its grants.
// Only the role is swapped; host, database and params are inherited.
func runtimeRoleURL() string {
	if explicit := os.Getenv("TEST_RUNTIME_ROLE_DATABASE_URL"); explicit != "" {
		return explicit
	}
	suite := os.Getenv("TEST_DATABASE_URL")
	if suite == "" {
		return defaultRuntimeRoleURL
	}
	u, err := url.Parse(suite)
	if err != nil {
		return defaultRuntimeRoleURL
	}
	u.User = url.UserPassword("velox_app", "velox_app")
	return u.String()
}

// TestAuditLog_AppendOnlyIsTwoIndependentLayers pins the defense-in-depth
// posture migration 0150 establishes for the append-only audit log.
//
// Before 0150 the BEFORE UPDATE/DELETE (0011) and BEFORE TRUNCATE (0115)
// triggers were the ONLY barrier: velox_app still held UPDATE/DELETE/TRUNCATE
// from 0001's blanket `GRANT ALL`, so disabling or dropping a trigger would
// have left the tamper-evidence log writable by the role that serves customer
// traffic. Now there are two INDEPENDENT layers, and this test asserts the
// error each one raises so a regression in either is distinguishable:
//
//	Layer 1 — privileges: velox_app is refused at the PERMISSION check
//	          (SQLSTATE 42501), before any trigger runs.
//	Layer 2 — triggers:   a role that STILL holds the write grants is refused
//	          by the trigger (SQLSTATE P0001, "append-only").
//
// If a future migration ever re-grants the privileges, layer 1's assertions
// fail with the trigger's P0001 instead of 42501 — the test tells you WHICH
// layer stopped the write, not merely that something did.
func TestAuditLog_AppendOnlyIsTwoIndependentLayers(t *testing.T) {
	db := testutil.SetupTestDB(t)
	tenantID := testutil.CreateTestTenant(t, db, "Append Only Grants")

	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 20*time.Second)
	defer cancel()

	// Seed a real row. The row-level trigger only fires on a MATCHED row, so a
	// layer-2 assertion against a WHERE that matches nothing would pass
	// vacuously (no rows → no trigger → no error).
	logger := audit.NewLogger(db)
	if err := logger.Log(ctx, tenantID, "invoice.finalize", "invoice", "vlx_inv_grants", "", nil); err != nil {
		t.Fatalf("seed audit row: %v", err)
	}

	var auditID string
	readTx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin read tx: %v", err)
	}
	if err := readTx.QueryRowContext(ctx,
		`SELECT id FROM audit_log WHERE tenant_id = $1 ORDER BY created_at DESC LIMIT 1`,
		tenantID,
	).Scan(&auditID); err != nil {
		_ = readTx.Rollback()
		t.Fatalf("read audit id: %v", err)
	}
	if err := readTx.Commit(); err != nil {
		t.Fatalf("commit read tx: %v", err)
	}

	writes := []struct {
		name string
		sql  string
		args []any
	}{
		{"UPDATE", `UPDATE audit_log SET action = 'tampered' WHERE id = $1`, []any{auditID}},
		{"DELETE", `DELETE FROM audit_log WHERE id = $1`, []any{auditID}},
		{"TRUNCATE", `TRUNCATE audit_log`, nil},
	}

	// ---- Layer 1: the runtime role has no write privilege at all. ----------
	//
	// Connect as velox_app itself. A permission check precedes both RLS and the
	// triggers, so this fails even though velox_app cannot see the row under
	// RLS (no app.tenant_id GUC set) — the statement never gets that far.
	t.Run("layer 1: velox_app is refused at the permission check", func(t *testing.T) {
		runtimeURL := runtimeRoleURL()
		pool, err := sql.Open("pgx", runtimeURL)
		if err != nil {
			t.Fatalf("open velox_app pool: %v", err)
		}
		defer func() { _ = pool.Close() }()
		if err := pool.PingContext(ctx); err != nil {
			// Never skip: a silent skip would retire the security assertion the
			// moment the local role drifts, which is how the original GRANT ALL
			// survived unnoticed for 114 migrations.
			t.Fatalf("velox_app must exist and be able to connect (see docker/init.sql / ci.yml): %v", err)
		}

		for _, w := range writes {
			t.Run(w.name, func(t *testing.T) {
				// Each statement in its own tx: the first failure aborts the tx,
				// and an aborted tx would fail every later statement with 25P02
				// ("current transaction is aborted") instead of its own error.
				tx, err := pool.BeginTx(ctx, nil)
				if err != nil {
					t.Fatalf("begin: %v", err)
				}
				defer func() { _ = tx.Rollback() }()

				_, err = tx.ExecContext(ctx, w.sql, w.args...)
				if err == nil {
					t.Fatalf("velox_app was allowed to %s audit_log — migration 0150's REVOKE is not in effect", w.name)
				}
				var pgErr *pgconn.PgError
				if !errors.As(err, &pgErr) {
					t.Fatalf("%s: expected a *pgconn.PgError, got %T: %v", w.name, err, err)
				}
				// 42501 = insufficient_privilege. This is the whole point: the
				// write is stopped by the GRANT system, NOT by the trigger.
				if pgErr.Code != "42501" {
					t.Errorf("%s: got SQLSTATE %s (%q), want 42501 insufficient_privilege — the write was stopped by the wrong layer",
						w.name, pgErr.Code, pgErr.Message)
				}
				if strings.Contains(strings.ToLower(pgErr.Message), "append-only") {
					t.Errorf("%s: the trigger (layer 2) answered, not the permission check (layer 1): %q", w.name, pgErr.Message)
				}
			})
		}
	})

	// ---- Layer 2: the triggers still hold for a role that HAS the grants. --
	//
	// velox_test_app retains ALL on audit_log (ALTER DEFAULT PRIVILEGES, not
	// touched by 0150) and TxBypass turns off RLS, so this role can see and
	// match the row. It is refused anyway — by the trigger. That is what makes
	// the trigger an INDEPENDENT layer rather than a consequence of the REVOKE:
	// the same protection would hold for the table owner or a superuser.
	t.Run("layer 2: a role holding the grants is refused by the trigger", func(t *testing.T) {
		for _, w := range writes {
			t.Run(w.name, func(t *testing.T) {
				tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
				if err != nil {
					t.Fatalf("begin: %v", err)
				}
				defer postgres.Rollback(tx)

				_, err = tx.ExecContext(ctx, w.sql, w.args...)
				if err == nil {
					t.Fatalf("%s: the append-only trigger did not fire for a grant-holding role", w.name)
				}
				var pgErr *pgconn.PgError
				if !errors.As(err, &pgErr) {
					t.Fatalf("%s: expected a *pgconn.PgError, got %T: %v", w.name, err, err)
				}
				// P0001 = raise_exception, i.e. audit_log_immutable()'s RAISE.
				if pgErr.Code != "P0001" {
					t.Errorf("%s: got SQLSTATE %s (%q), want P0001 — expected the trigger to answer",
						w.name, pgErr.Code, pgErr.Message)
				}
				if !strings.Contains(pgErr.Message, "append-only") {
					t.Errorf("%s: trigger message did not mention append-only: %q", w.name, pgErr.Message)
				}
			})
		}
	})

	// The row survived every attempt above.
	verifyTx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin verify tx: %v", err)
	}
	defer postgres.Rollback(verifyTx)
	var action string
	if err := verifyTx.QueryRowContext(ctx, `SELECT action FROM audit_log WHERE id = $1`, auditID).Scan(&action); err != nil {
		t.Fatalf("re-read audit row: %v", err)
	}
	if action != "invoice.finalize" {
		t.Fatalf("audit row was mutated: action = %q, want %q", action, "invoice.finalize")
	}
}
