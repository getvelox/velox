package testutil

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/sagarsuperuser/velox/internal/platform/migrate"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

const (
	// Superuser URL — used for migrations and cleanup (bypasses RLS).
	// Points to velox_test database so tests never touch dev data.
	defaultAdminDBURL = "postgres://velox:velox@localhost:5432/velox_test?sslmode=disable"
	// App user URL — used for queries (RLS enforced).
	defaultAppDBURL = "postgres://velox_test_app:velox_test_app@localhost:5432/velox_test?sslmode=disable"
)

// SetupTestDB runs migrations as superuser, cleans data, and returns an
// app-user connection where RLS is enforced.
// utcOnce pins the test process to UTC exactly like cmd/velox/main (ADR-075),
// so integration tests observe the same canonical-UTC timestamp behavior as
// production regardless of the host timezone (a dev box on IST would otherwise
// scan timestamptz back in +05:30 and diverge from CI/prod). sync.Once keeps the
// global assignment race-free across parallel integration tests.
var utcOnce sync.Once

func SetupTestDB(t *testing.T) *postgres.DB {
	t.Helper()

	if testing.Short() {
		t.Skip("skipping integration test (use -short=false)")
	}
	utcOnce.Do(func() { time.Local = time.UTC })

	adminURL := envOr("TEST_ADMIN_DATABASE_URL", defaultAdminDBURL)
	appURL := envOr("TEST_DATABASE_URL", defaultAppDBURL)

	runMigrations(t, adminURL)

	adminPool := openPool(t, adminURL)
	// Truncate ONCE, at setup. Setup-time truncation is what guarantees a
	// clean start regardless of how the previous test ended (a test that
	// Fatalf'd mid-way leaves residue; the next setup wipes it). A second
	// truncate in t.Cleanup bought nothing and doubled the ACCESS EXCLUSIVE
	// lock churn (700 truncates per integration run where 350 will do).
	// Consequence to know: the LAST test in a process leaves its rows
	// behind — the CI velox-doctor sweep after the integration job examines
	// whatever the alphabetically-last DB package left.
	cleanDB(t, adminPool)

	// App connection: actual queries (RLS enforced)
	appPool := openPool(t, appURL)
	db := postgres.NewDB(appPool, 5*time.Second)

	t.Cleanup(func() {
		_ = appPool.Close()
		_ = adminPool.Close()
	})

	return db
}

func openPool(t *testing.T, url string) *sql.DB {
	t.Helper()

	// Use "pgx" driver (pgx stdlib adapter) — same as production.
	// golang-migrate's postgres driver works with any database/sql driver.
	pool, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatalf("open db (%s): %v", url, err)
	}
	pool.SetMaxOpenConns(5)
	pool.SetMaxIdleConns(2)
	pool.SetConnMaxLifetime(5 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := pool.PingContext(ctx); err != nil {
		t.Fatalf("ping db (%s): %v", url, err)
	}
	return pool
}

func cleanDB(t *testing.T, pool *sql.DB) {
	t.Helper()
	if err := cleanDBErr(pool); err != nil {
		t.Fatalf("clean db: %v", err)
	}
}

// cleanDBErr truncates every public base table except golang-migrate's
// schema_migrations, in ONE statement, with the table list derived from
// the catalog at call time.
//
// Why derived, and why no EXCEPTION handler: the previous hand-maintained
// list was wrapped in `EXCEPTION WHEN undefined_table THEN NULL`. When
// migration 0083 dropped two tables the list still named, the handler
// swallowed the whole TRUNCATE and cleanup silently became a no-op for
// an entire run (commit 9f08e52b, 2026-05-17). A derived list cannot go
// stale, and an error here is a real error and must surface.
//
// Why schema_migrations is excluded: truncating it leaves the schema in
// place with version=nil; the next process's migrate.Up then fails on
// 0001's CREATE TABLE and runMigrations' dirty-state path nukes and
// rebuilds every table — a slower route to the same silent-no-op class.
//
// Why no CASCADE: every table is in the list, so FK ordering is moot.
//
// audit_log carries a statement-level BEFORE TRUNCATE trigger (migration
// 0115) that blocks TRUNCATE to keep the log tamper-evident; replica
// mode disables it for this transaction only.
func cleanDBErr(pool *sql.DB) error {
	_, err := pool.ExecContext(context.Background(), `
		DO $$
		DECLARE tbls text;
		BEGIN
			PERFORM set_config('session_replication_role', 'replica', true);
			SELECT string_agg(quote_ident(tablename), ', ')
			  INTO tbls
			  FROM pg_tables
			 WHERE schemaname = 'public'
			   AND tablename <> 'schema_migrations';
			IF tbls IS NULL THEN
				RAISE EXCEPTION 'testutil.cleanDB: no public tables found — migrations not applied?';
			END IF;
			EXECUTE 'TRUNCATE ' || tbls;
		END $$;
	`)
	return err
}

// TestCtx returns a context with livemode pinned to false (test mode)
// — matching the partition every integration test fixture inserts
// against. Tests that go through the auth middleware in production
// would inherit livemode from the API key; tests that hit stores or
// services directly need to pin it explicitly, otherwise BeginTx
// trips the strict-mode panic ("TxTenant opened without ctx
// livemode") and silently routes to live in non-strict builds.
//
// Default: livemode=false (test). For live-mode integration tests,
// wrap context.Background() with postgres.WithLivemode(ctx, true)
// directly — those are rare enough not to warrant a separate helper.
func TestCtx() context.Context {
	return postgres.WithLivemode(context.Background(), false)
}

// CreateTestTenant inserts a tenant via the app connection (bypass RLS).
// Since tenants table has no RLS, this works directly.
func CreateTestTenant(t *testing.T, db *postgres.DB, name string) string {
	t.Helper()

	id := postgres.NewID("vlx_ten")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := db.Pool.ExecContext(ctx,
		`INSERT INTO tenants (id, name, status) VALUES ($1, $2, 'active')`, id, name)
	if err != nil {
		t.Fatalf("create test tenant: %v", err)
	}
	return id
}

func envOr(key, fallback string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	return v
}

// runMigrations applies pending migrations ONCE per test process (one
// package's test binary): every SetupTestDB after the first reuses the
// result instead of opening a migration pool, taking golang-migrate's
// advisory lock, and re-checking the version — ~13 redundant round-trips
// per package before this. On failure, drops everything and retries from
// scratch (safe because this is a test-only database). migrate.Up manages
// its own pool internally so the DSN is all we need here.
//
// migrateRuns is exported for the harness's own test, which asserts the
// once-ness.
var (
	migrateOnce sync.Once
	migrateErr  error
	migrateRuns int
)

func runMigrations(t *testing.T, adminURL string) {
	t.Helper()
	migrateOnce.Do(func() {
		migrateRuns++
		if err := migrate.Up(adminURL); err == nil {
			return
		}
		// Dirty or incompatible state (e.g., schema_migrations records a
		// version that no longer exists in the embedded FS — common after
		// switching branches). Drop everything and retry.
		nukePool, err := sql.Open("pgx", adminURL)
		if err != nil {
			migrateErr = err
			return
		}
		defer func() { _ = nukePool.Close() }()
		if err := dropAllTablesErr(nukePool); err != nil {
			migrateErr = err
			return
		}
		migrateErr = migrate.Up(adminURL)
	})
	if migrateErr != nil {
		t.Fatalf("run migrations: %v", migrateErr)
	}
}

func dropAllTablesErr(pool *sql.DB) error {
	if _, err := pool.ExecContext(context.Background(),
		"DROP TABLE IF EXISTS schema_migrations CASCADE"); err != nil {
		return err
	}
	_, err := pool.ExecContext(context.Background(), `
		DO $$ DECLARE r RECORD;
		BEGIN
			FOR r IN (SELECT tablename FROM pg_tables WHERE schemaname = 'public') LOOP
				EXECUTE 'DROP TABLE IF EXISTS public.' || quote_ident(r.tablename) || ' CASCADE';
			END LOOP;
		END $$;
	`)
	return err
}

// AdminPool opens the owner-role connection — the same role that runs
// migrations. For tests that must perform DDL, e.g. replaying a shipped
// migration's data transform against a seeded pre-migration shape
// (the app role can't drop/recreate indexes it doesn't own).
func AdminPool(t *testing.T) *sql.DB {
	t.Helper()
	pool := openPool(t, envOr("TEST_ADMIN_DATABASE_URL", defaultAdminDBURL))
	t.Cleanup(func() { _ = pool.Close() })
	return pool
}
