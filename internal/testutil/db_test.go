package testutil

import (
	"context"
	"database/sql"
	"strings"
	"testing"
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

// TestHarness_CleanDBKeepsSchemaMigrations: golang-migrate's version row
// survives cleanup. Mutation: remove the schema_migrations exclusion in
// cleanDBErr → this goes red (and the next process's migrate.Up would
// nuke-and-rebuild every table).
func TestHarness_CleanDBKeepsSchemaMigrations(t *testing.T) {
	_ = SetupTestDB(t)
	admin := AdminPool(t)
	cleanDB(t, admin)
	if n := countRows(t, admin, "schema_migrations"); n != 1 {
		t.Fatalf("schema_migrations should hold exactly its version row after cleanDB, got %d rows", n)
	}
}

// TestHarness_CleanDBEmptiesEveryPublicTable: after cleanDB, no public base
// table other than schema_migrations has rows.
func TestHarness_CleanDBEmptiesEveryPublicTable(t *testing.T) {
	db := SetupTestDB(t)
	_ = CreateTestTenant(t, db, "harness-residue")
	admin := AdminPool(t)
	cleanDB(t, admin)
	rows, err := admin.QueryContext(context.Background(),
		"SELECT tablename FROM pg_tables WHERE schemaname='public' AND tablename <> 'schema_migrations' ORDER BY 1")
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
