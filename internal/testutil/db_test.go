package testutil

import (
	"context"
	"database/sql"
	neturl "net/url"
	"strings"
	"testing"

	"github.com/sagarsuperuser/velox/internal/platform/migrate"
)

// These pin the harness's own contract against the real test database.
// The class they guard fired on 2026-05-17: a hand-maintained TRUNCATE list
// named tables a migration had dropped, the undefined_table handler
// swallowed the statement, and cleanup silently did nothing for a run.

func countRows(t *testing.T, pool *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := pool.QueryRowContext(context.Background(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// TestHarness_CleanDBDerivesTablesFromCatalog: a table the harness has never
// heard of, created after setup, is emptied by cleanDB — proof the list is
// derived, not hand-maintained.
func TestHarness_CleanDBDerivesTablesFromCatalog(t *testing.T) {
	db := SetupTestDB(t)
	_ = db
	admin := AdminPool(t)
	ctx := context.Background()
	if _, err := admin.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS _testutil_probe (id int)"); err != nil {
		t.Fatalf("create probe: %v", err)
	}
	t.Cleanup(func() { _, _ = admin.ExecContext(ctx, "DROP TABLE IF EXISTS _testutil_probe") })
	if _, err := admin.ExecContext(ctx, "INSERT INTO _testutil_probe VALUES (1), (2)"); err != nil {
		t.Fatalf("seed probe: %v", err)
	}
	cleanDB(t, admin)
	if n := countRows(t, admin, "_testutil_probe"); n != 0 {
		t.Fatalf("cleanDB left %d rows in a table it never listed by hand — the table set is not catalog-derived", n)
	}
}

// scratchDatabase creates an empty database next to the test database and
// returns its URL and a pool on it, so a test can migrate, break and reset
// it without touching the shared one.
func scratchDatabase(t *testing.T, name string) (string, *sql.DB) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping integration test (use -short=false)")
	}
	adminURL := envOr("TEST_ADMIN_DATABASE_URL", defaultAdminDBURL)
	admin := openPool(t, adminURL)
	defer func() { _ = admin.Close() }()
	ctx := context.Background()
	if _, err := admin.ExecContext(ctx, `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(ctx, `CREATE DATABASE `+name); err != nil {
		t.Fatal(err)
	}
	u, err := neturl.Parse(adminURL)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	pool := openPool(t, u.String())
	t.Cleanup(func() {
		_ = pool.Close()
		admin := openPool(t, adminURL)
		defer func() { _ = admin.Close() }()
		_, _ = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
	})
	return u.String(), pool
}

// TestHarness_CleanDBKeepsVersionTables: both migration records survive
// cleanup — goose_db_version, and the frozen schema_migrations of a
// database taken over from v0.8. Mutation: remove either name from
// cleanDBErr's exclusion → this goes red (and the next process's
// migrate.Up would run the baseline on a full database).
func TestHarness_CleanDBKeepsVersionTables(t *testing.T) {
	url, pool := scratchDatabase(t, "velox_testutil_clean")
	if err := migrate.Up(url); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := context.Background()
	for _, q := range []string{
		`CREATE TABLE public.schema_migrations (version bigint NOT NULL PRIMARY KEY, dirty boolean NOT NULL)`,
		`INSERT INTO public.schema_migrations VALUES (180, false)`,
	} {
		if _, err := pool.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	goose := countRows(t, pool, "goose_db_version")
	if goose < 2 {
		t.Fatalf("goose_db_version holds %d rows after migrating, want row 0 and 180", goose)
	}
	cleanDB(t, pool)
	if n := countRows(t, pool, "goose_db_version"); n != goose {
		t.Fatalf("goose_db_version holds %d rows after cleanDB, want %d", n, goose)
	}
	if n := countRows(t, pool, "schema_migrations"); n != 1 {
		t.Fatalf("schema_migrations holds %d rows after cleanDB, want 1", n)
	}
}

// TestHarness_ResetAfterFailedUp walks runMigrations' fallback: migrate.Up
// refuses a database (here a v0.8 record below 180), the reset drops every
// object that is not an extension's, and migrate.Up then builds it from
// the baseline. Mutation: drop only tables; the baseline's CREATE
// FUNCTION then fails on a function the reset left behind.
func TestHarness_ResetAfterFailedUp(t *testing.T) {
	url, pool := scratchDatabase(t, "velox_testutil_reset")
	if err := migrate.Up(url); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := context.Background()
	for _, q := range []string{
		`DROP TABLE public.goose_db_version`,
		`CREATE TABLE public.schema_migrations (version bigint NOT NULL PRIMARY KEY, dirty boolean NOT NULL)`,
		`INSERT INTO public.schema_migrations VALUES (179, true)`,
	} {
		if _, err := pool.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrate.Up(url); err == nil {
		t.Fatal("migrate.Up accepted a dirty v0.8 record")
	}
	if err := dropAllTablesErr(pool); err != nil {
		t.Fatalf("reset: %v", err)
	}
	var left int
	if err := pool.QueryRowContext(ctx, `
		SELECT (SELECT count(*) FROM pg_class c WHERE c.relnamespace = 'public'::regnamespace AND c.relkind IN ('r','p','v','m','S'))
		     + (SELECT count(*) FROM pg_proc p WHERE p.pronamespace = 'public'::regnamespace
		          AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_proc'::regclass AND d.objid = p.oid AND d.deptype = 'e'))`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("the reset left %d relations or functions in public", left)
	}
	if err := migrate.Up(url); err != nil {
		t.Fatalf("migrate after the reset: %v", err)
	}
	if err := migrate.CheckSchemaReady(pool); err != nil {
		t.Fatalf("schema after the reset: %v", err)
	}
}

// TestHarness_CleanDBEmptiesEveryPublicTable: after cleanDB, no public base
// table other than the two migration records has rows.
func TestHarness_CleanDBEmptiesEveryPublicTable(t *testing.T) {
	db := SetupTestDB(t)
	_ = CreateTestTenant(t, db, "harness-residue")
	admin := AdminPool(t)
	cleanDB(t, admin)
	rows, err := admin.QueryContext(context.Background(),
		"SELECT tablename FROM pg_tables WHERE schemaname='public' AND tablename NOT IN ('goose_db_version', 'schema_migrations') ORDER BY 1")
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var dirty []string
	var total int
	for rows.Next() {
		var tbl string
		if err := rows.Scan(&tbl); err != nil {
			t.Fatal(err)
		}
		total++
		if countRows(t, admin, tbl) != 0 {
			dirty = append(dirty, tbl)
		}
	}
	if total < 40 {
		t.Fatalf("expected the migrated schema (40+ tables), found %d — wrong database?", total)
	}
	if len(dirty) > 0 {
		t.Fatalf("tables still holding rows after cleanDB: %s", strings.Join(dirty, ", "))
	}
}

// TestHarness_MigrationsRunOncePerProcess: two setups in one process run
// migrate.Up once.
func TestHarness_MigrationsRunOncePerProcess(t *testing.T) {
	_ = SetupTestDB(t)
	_ = SetupTestDB(t)
	if migrateRuns != 1 {
		t.Fatalf("migrate.Up ran %d times in this process, want exactly 1", migrateRuns)
	}
}
