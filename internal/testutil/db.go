package testutil

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

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

// OpenTracedDB opens a second app-user (RLS-enforced) connection to the test
// database whose every statement passes through tracer. Call it after
// SetupTestDB (which migrates and cleans). For tests that assert how many
// queries or transactions a read path costs — an N+1 regression shows up as
// a count that grows with the row count (issue #1142).
func OpenTracedDB(t *testing.T, tracer pgx.QueryTracer) *postgres.DB {
	t.Helper()
	cfg, err := pgx.ParseConfig(envOr("TEST_DATABASE_URL", defaultAppDBURL))
	if err != nil {
		t.Fatalf("parse test db url: %v", err)
	}
	cfg.Tracer = tracer
	pool := stdlib.OpenDB(*cfg)
	pool.SetMaxOpenConns(5)
	t.Cleanup(func() { _ = pool.Close() })
	return postgres.NewDB(pool, 5*time.Second)
}

func openPool(t *testing.T, url string) *sql.DB {
	t.Helper()

	// Use "pgx" driver (pgx stdlib adapter) — same as production.
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

// cleanDBErr truncates every public base table except the two migration
// records (goose_db_version, and schema_migrations on a database taken
// over from v0.8), in ONE statement, with the table list derived from the
// catalog at call time.
//
// Why derived, and why no EXCEPTION handler: the previous hand-maintained
// list was wrapped in `EXCEPTION WHEN undefined_table THEN NULL`. When
// migration 0083 dropped two tables the list still named, the handler
// swallowed the whole TRUNCATE and cleanup silently became a no-op for
// an entire run (commit 9f08e52b, 2026-05-17). A derived list cannot go
// stale, and an error here is a real error and must surface.
//
// Why the records are excluded: truncating them leaves the schema in place
// with no record; the next process's migrate.Up then runs the baseline on
// a full database, fails on its first CREATE, and runMigrations drops and
// rebuilds everything — a slower route to the same silent-no-op class.
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
			   AND tablename NOT IN ('goose_db_version', 'schema_migrations');
			IF tbls IS NULL THEN
				RAISE EXCEPTION 'testutil.cleanDB: no public tables found — migrations not applied?';
			END IF;
			EXECUTE 'TRUNCATE ' || tbls;
		END $$;
	`)
	return err
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
// result instead of opening a migration pool, taking goose's advisory
// lock, and re-checking the version — ~13 redundant round-trips per
// package before this. On failure, drops everything and retries from
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
		// A state migrate.Up refuses or cannot continue from — a v0.8
		// record that is not a clean 180, or a version from another
		// branch, common after switching branches. Drop everything and
		// retry.
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

// dropAllTablesErr drops every object in public that no extension owns:
// tables (both migration records with them), views, sequences, functions,
// procedures and types. The baseline creates its functions and types with
// plain CREATE, so anything left behind would fail it.
func dropAllTablesErr(pool *sql.DB) error {
	_, err := pool.ExecContext(context.Background(), `
		DO $$
		DECLARE stmts text[]; stmt text;
		BEGIN
			-- Every statement is built before the first DROP: a name read
			-- after a CASCADE dropped its object would print as a bare OID.
			SELECT array_agg(s) INTO stmts FROM (
				SELECT format('DROP %s IF EXISTS %s CASCADE',
				              CASE c.relkind WHEN 'v' THEN 'VIEW' WHEN 'm' THEN 'MATERIALIZED VIEW'
				                             WHEN 'S' THEN 'SEQUENCE' WHEN 'f' THEN 'FOREIGN TABLE' ELSE 'TABLE' END,
				              c.oid::regclass)
				FROM pg_class c
				WHERE c.relnamespace = 'public'::regnamespace
				  AND c.relkind IN ('r', 'p', 'v', 'm', 'S', 'f')
				  AND NOT EXISTS (SELECT 1 FROM pg_depend d
				                  WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid AND d.deptype IN ('e', 'i', 'a'))
				UNION ALL
				SELECT format('DROP %s IF EXISTS %s CASCADE',
				              CASE p.prokind WHEN 'a' THEN 'AGGREGATE' WHEN 'p' THEN 'PROCEDURE' ELSE 'FUNCTION' END,
				              p.oid::regprocedure)
				FROM pg_proc p
				WHERE p.pronamespace = 'public'::regnamespace
				  AND NOT EXISTS (SELECT 1 FROM pg_depend d
				                  WHERE d.classid = 'pg_proc'::regclass AND d.objid = p.oid AND d.deptype = 'e')
				UNION ALL
				SELECT format('DROP TYPE IF EXISTS %s CASCADE', t.oid::regtype)
				FROM pg_type t
				WHERE t.typnamespace = 'public'::regnamespace
				  AND t.typtype IN ('e', 'd', 'c', 'r', 'm')
				  AND (t.typrelid = 0 OR (SELECT relkind FROM pg_class WHERE oid = t.typrelid) = 'c')
				  AND NOT EXISTS (SELECT 1 FROM pg_depend d
				                  WHERE d.classid = 'pg_type'::regclass AND d.objid = t.oid AND d.deptype = 'e')
			) q(s);
			FOREACH stmt IN ARRAY coalesce(stmts, '{}') LOOP
				EXECUTE stmt;
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
