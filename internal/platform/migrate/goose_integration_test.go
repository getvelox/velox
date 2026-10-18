package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

// Real-Postgres tests of the goose runner (ADR-125). Each names the
// mutation that must turn it red; the migrations CI job covers the rest
// (new install == upgraded install, privileges, the restore drill).

const widgetsFile = `-- +goose Up
CREATE TABLE widgets (id int NOT NULL);
-- +goose Down
DROP TABLE widgets;
`

// T1. Three copies take over one v0.8 database at once. Mutation: a
// provider without WithSessionLocker; then two copies run migration 180
// and goose writes row 180 twice (its table has no unique constraint, so
// only the lock keeps it to one).
func TestUpAdoptsV08Concurrently(t *testing.T) {
	dsn, db := scratchDB(t, "velox_migrate_t1")
	ctx := context.Background()
	if err := up(ctx, dsn, noFiles); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	buildV08(t, db, 180, false)
	fastLockWait(t, 60)

	errs := make([]error, 3)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = up(ctx, dsn, noFiles)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("copy %d: %v", i, err)
		}
	}
	if n := gooseRows(t, db, 180); n != 1 {
		t.Fatalf("goose_db_version holds %d rows for 180, want 1", n)
	}
	var comment sql.NullString
	if err := db.QueryRow(`SELECT obj_description('public.schema_migrations'::regclass, 'pg_class')`).Scan(&comment); err != nil {
		t.Fatal(err)
	}
	if comment.String != v08Marker {
		t.Fatalf("schema_migrations comment = %q, want the marker", comment.String)
	}
	var v int64
	var dirty bool
	if err := db.QueryRow(`SELECT version, dirty FROM public.schema_migrations`).Scan(&v, &dirty); err != nil || v != 180 || dirty {
		t.Fatalf("schema_migrations after adoption: %d %v %v, want 180 false (frozen)", v, dirty, err)
	}
}

// T3. Every state the check refuses, built by hand: Up returns the
// message, creates no goose_db_version where none existed, and changes
// nothing. Mutation: read the record without its comment; the
// "restored over" case then passes as a frozen record.
func TestUpRefusesV08States(t *testing.T) {
	cases := []struct {
		name  string
		build func(t *testing.T, db *sql.DB)
		dsn   func(t *testing.T, dsn string) string
		kind  v08Kind
		want  string
	}{
		{"v0.8 below 180", func(t *testing.T, db *sql.DB) { writeV08Record(t, db, "public", 179, false) }, nil,
			v08NotClean, "says version 179, dirty=false"},
		{"v0.8 dirty at 180", func(t *testing.T, db *sql.DB) { writeV08Record(t, db, "public", 180, true) }, nil,
			v08NotClean, "says version 180, dirty=true"},
		{"v0.8 development build at 181", func(t *testing.T, db *sql.DB) { writeV08Record(t, db, "public", 181, false) }, nil,
			v08NotClean, "A version above 180 came from a development build"},
		{"v0.8 backup restored over an adopted database", func(t *testing.T, db *sql.DB) {
			recordGoose(t, db, 180)
			writeV08Record(t, db, "public", 180, false) // no marker: --clean recreated the table
		}, nil, v08Mismatch, "without the comment this release adds"},
		{"v0.8 rollback after adoption", func(t *testing.T, db *sql.DB) {
			recordGoose(t, db, 180)
			writeV08Record(t, db, "public", 180, false)
			mustExec(t, db, `COMMENT ON TABLE public.schema_migrations IS '`+v08Marker+`'`)
			mustExec(t, db, `TRUNCATE public.schema_migrations`)
			mustExec(t, db, `INSERT INTO public.schema_migrations VALUES (179, false)`)
		}, nil, v08Mismatch, "(version 179, dirty=false) does not match this release's record in goose_db_version (version 180)"},
		{"v0.8 binary ran on a new database", func(t *testing.T, db *sql.DB) {
			recordGoose(t, db, 180)
			writeV08Record(t, db, "public", 1, true)
		}, nil, v08Mismatch, "version 1, dirty=true"},
		{"v0.8 tables outside public", func(t *testing.T, db *sql.DB) {
			mustExec(t, db, `CREATE SCHEMA other`)
			writeV08Record(t, db, "other", 180, false)
		}, func(t *testing.T, dsn string) string { return withParam(t, dsn, "search_path", "other") },
			v08OutsidePublic, `schema "other"`},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dsn, db := scratchDB(t, "velox_migrate_t3_"+string(rune('a'+i)))
			tc.build(t, db)
			if tc.dsn != nil {
				dsn = tc.dsn(t, dsn)
			}
			hadGoose := tableExists(t, db, versionTable)
			before := fingerprint(t, db)

			err := up(context.Background(), dsn, noFiles)
			var refused *V08Error
			if !errors.As(err, &refused) || refused.kind != tc.kind {
				t.Fatalf("Up: want a V08Error of kind %d, got %v", tc.kind, err)
			}
			for _, want := range []string{tc.want, "Nothing was changed."} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("message lacks %q: %v", want, err)
				}
			}
			if !hadGoose && tableExists(t, db, versionTable) {
				t.Error("Up created goose_db_version on a refused database")
			}
			if after := fingerprint(t, db); after != before {
				t.Errorf("Up changed the database:\nbefore %s\nafter  %s", before, after)
			}
			// The same refusal at boot, on a pool with the same settings.
			check, err := sql.Open("pgx", dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = check.Close() }()
			if err := CheckSchemaReady(check); !errors.As(err, &refused) || refused.kind != tc.kind {
				t.Errorf("CheckSchemaReady: want the same refusal, got %v", err)
			}
		})
	}
}

// T4. A new database: the baseline, then a file with unqualified names, in
// the same Up. Mutation: drop gen-baseline's R4 (search_path made
// transaction-local); the baseline's empty search_path then leaks into
// 00181, which has no schema to create its table in.
func TestUpBaselineThenFileInOneUp(t *testing.T) {
	dsn, db := scratchDB(t, "velox_migrate_t4")
	if err := up(context.Background(), dsn, files(map[string]string{"00181_widgets.sql": widgetsFile})); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if !tableExists(t, db, "public.widgets") || !tableExists(t, db, "public.tenants") {
		t.Fatal("want public.tenants (baseline) and public.widgets (00181)")
	}
	for _, v := range []int64{180, 181} {
		if n := gooseRows(t, db, v); n != 1 {
			t.Fatalf("version %d: %d rows, want 1", v, n)
		}
	}
	// 0174's seed: five rows, each token from clock_timestamp() at install.
	var leases int
	var fresh bool
	if err := db.QueryRow(`SELECT count(*), bool_and(abs(holder_token - (extract(epoch FROM now()) * 1000)::bigint) < 86400000) FROM leader_leases`).Scan(&leases, &fresh); err != nil || leases != 5 || !fresh {
		t.Fatalf("leader_leases: want 5 rows with tokens from install time, got %d rows, fresh=%v (%v)", leases, fresh, err)
	}
	// A second run finds nothing to do.
	if err := up(context.Background(), dsn, files(map[string]string{"00181_widgets.sql": widgetsFile})); err != nil {
		t.Fatalf("second Up: %v", err)
	}
	// This binary ships 180 alone, so to the boot check the scratch
	// database is ahead (it has 181): a WARN, not an error.
	if err := CheckSchemaReady(db); err != nil {
		t.Fatalf("CheckSchemaReady: %v", err)
	}
}

// A new database where the admin user owns a schema named after itself,
// so its default search_path ("$user", public) puts new objects there. The
// baseline names every object public.x; a later file's unqualified CREATE
// TABLE must land in public too, where velox_app looks. Mutation: drop
// migrationSession's SET search_path; widgets then lands in the user's
// own schema.
func TestUpCreatesInPublicWhateverTheSearchPath(t *testing.T) {
	dsn, db := scratchDB(t, "velox_migrate_search_path")
	mustExec(t, db, `CREATE SCHEMA AUTHORIZATION CURRENT_USER`)
	fsys := files(map[string]string{"00181_widgets.sql": widgetsFile})
	if err := up(context.Background(), dsn, fsys); err != nil {
		t.Fatalf("Up: %v", err)
	}
	var schemas sql.NullString
	if err := db.QueryRow(`SELECT string_agg(n.nspname, ',' ORDER BY n.nspname) FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace WHERE c.relname IN ('widgets', 'tenants')`).Scan(&schemas); err != nil {
		t.Fatal(err)
	}
	if schemas.String != "public,public" {
		t.Fatalf("tenants (baseline) and widgets (00181) are in %q, want both in public", schemas.String)
	}
	// A second run, then the boot check on a pool with the user's own
	// search_path: no M3 refusal, because Velox's tables are in public.
	if err := up(context.Background(), dsn, fsys); err != nil {
		t.Fatalf("second Up: %v", err)
	}
	if err := CheckSchemaReady(db); err != nil {
		t.Fatalf("CheckSchemaReady: %v", err)
	}
}

// T5. Copies start together on a new database, and two copies meet an
// index build that waits for an old snapshot. Mutation: a blocking
// pg_advisory_lock locker; the waiting copy's lock statement holds a
// snapshot the build waits for, and Postgres cancels one side (E3).
func TestUpConcurrentCopies(t *testing.T) {
	fastLockWait(t, 120)
	ctx := context.Background()

	t.Run("new database, three copies", func(t *testing.T) {
		dsn, db := scratchDB(t, "velox_migrate_t5a")
		fsys := files(map[string]string{"00181_widgets.sql": widgetsFile})
		errs := make([]error, 3)
		var wg sync.WaitGroup
		for i := range errs {
			wg.Add(1)
			go func(i int) { defer wg.Done(); errs[i] = up(ctx, dsn, fsys) }(i)
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("copy %d: %v", i, err)
			}
		}
		for _, v := range []int64{0, 180, 181} {
			if n := gooseRows(t, db, v); n != 1 {
				t.Fatalf("version %d: %d rows, want 1", v, n)
			}
		}
	})

	t.Run("index build waits for an old snapshot", func(t *testing.T) {
		dsn, db := scratchDB(t, "velox_migrate_t5b")
		fsys := files(map[string]string{"00181_widgets.sql": widgetsFile})
		if err := up(ctx, dsn, fsys); err != nil {
			t.Fatalf("Up: %v", err)
		}
		mustExec(t, db, `INSERT INTO widgets SELECT g FROM generate_series(1, 1000) g`)
		fsys["00182_widgets_id_index.sql"] = indexFile("idx_widgets_id", "CREATE UNIQUE INDEX CONCURRENTLY idx_widgets_id ON widgets (id);")

		holder, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := holder.Exec(`SELECT 1`); err != nil { // takes the snapshot
			t.Fatal(err)
		}
		errs := make([]error, 2)
		var wg sync.WaitGroup
		for i := range errs {
			wg.Add(1)
			go func(i int) { defer wg.Done(); errs[i] = up(ctx, dsn, fsys) }(i)
		}
		time.Sleep(3 * time.Second) // the build is now waiting for the holder
		_ = holder.Commit()
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("copy %d: %v", i, err)
			}
		}
		if !indexValid(t, db, "idx_widgets_id") {
			t.Fatal("idx_widgets_id is missing or INVALID")
		}
		if n := gooseRows(t, db, 182); n != 1 {
			t.Fatalf("version 182: %d rows, want 1", n)
		}
	})
}

func indexFile(name, create string) *fstest.MapFile {
	return &fstest.MapFile{Data: []byte("-- +goose NO TRANSACTION\n-- +goose Up\nSET lock_timeout = 0;\n" +
		"DROP INDEX CONCURRENTLY IF EXISTS " + name + ";\n" + create + "\nRESET lock_timeout;\n" +
		"-- +goose Down\nSET lock_timeout = 0;\nDROP INDEX CONCURRENTLY IF EXISTS " + name + ";\nRESET lock_timeout;\n")}
}

func indexValid(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var valid bool
	err := db.QueryRow(`SELECT i.indisvalid FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid WHERE c.relname = $1`, name).Scan(&valid)
	if errors.Is(err, sql.ErrNoRows) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	return valid
}

// T6. A failed normal file leaves no table and no row, and passes once
// fixed (E5 as a regression test).
func TestUpFailedFileLeavesNothing(t *testing.T) {
	dsn, db := scratchDB(t, "velox_migrate_t6")
	ctx := context.Background()
	bad := files(map[string]string{"00181_broken.sql": "-- +goose Up\nCREATE TABLE half_done (id int);\nSELECT 1/0;\n-- +goose Down\nDROP TABLE half_done;\n"})
	err := up(ctx, dsn, bad)
	var fe *FileError
	if !errors.As(err, &fe) || fe.Version != 181 || fe.NoTx {
		t.Fatalf("want a FileError for 181, got %v", err)
	}
	for _, want := range []string{"00181_broken.sql", "division by zero", "22012", "none of it was applied", RunbookPath} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message lacks %q: %v", want, err)
		}
	}
	if tableExists(t, db, "public.half_done") || gooseRows(t, db, 181) != 0 {
		t.Fatal("the failed file left its table or its row")
	}
	if gooseRows(t, db, 180) != 1 {
		t.Fatal("the baseline before it should have committed")
	}
	fixed := files(map[string]string{"00181_broken.sql": "-- +goose Up\nCREATE TABLE half_done (id int);\n-- +goose Down\nDROP TABLE half_done;\n"})
	if err := up(ctx, dsn, fixed); err != nil {
		t.Fatalf("Up after the fix: %v", err)
	}
	if !tableExists(t, db, "public.half_done") || gooseRows(t, db, 181) != 1 {
		t.Fatal("the fixed file did not apply")
	}
}

// blockTable holds ACCESS SHARE on a table, the way an open SELECT does.
func blockTable(t *testing.T, db *sql.DB, table string) (release func()) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`LOCK TABLE ` + table + ` IN ACCESS SHARE MODE`); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release = func() { once.Do(func() { _ = tx.Rollback() }) }
	t.Cleanup(release)
	return release
}

// upWithin runs up and fails the test if it has not returned in time, so a
// mutation that makes it wait forever fails instead of hanging.
func upWithin(t *testing.T, d time.Duration, dsn string, fsys fstest.MapFS, release func()) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- up(context.Background(), dsn, fsys) }()
	select {
	case err := <-done:
		return err
	case <-time.After(d):
		release()
		t.Fatalf("Up still waiting after %s: lock_timeout is not in force", d)
		return nil
	}
}

// T7. A normal file stops at MIGRATION_LOCK_TIMEOUT, undone in full. The
// second case runs the baseline and the file in one Up. Mutation: drop
// gen-baseline's R3 (SET LOCAL); the baseline's lock_timeout = 0 then
// stays on the session, and the file waits forever.
func TestUpStopsOnLockTimeout(t *testing.T) {
	t.Setenv(LockTimeoutEnv, "1s")

	t.Run("after earlier files", func(t *testing.T) {
		dsn, db := scratchDB(t, "velox_migrate_t7a")
		fsys := files(map[string]string{"00181_widgets.sql": widgetsFile})
		if err := up(context.Background(), dsn, fsys); err != nil {
			t.Fatalf("Up: %v", err)
		}
		release := blockTable(t, db, "widgets")
		fsys["00182_widgets_note.sql"] = &fstest.MapFile{Data: []byte("-- +goose Up\nALTER TABLE widgets ADD COLUMN note text;\n-- +goose Down\nALTER TABLE widgets DROP COLUMN note;\n")}
		start := time.Now()
		err := upWithin(t, 20*time.Second, dsn, fsys, release)
		var lte *LockTimeoutError
		if !errors.As(err, &lte) || lte.Version != 182 || lte.Previous != 181 || lte.Timeout != time.Second {
			t.Fatalf("want LockTimeoutError{182, previous 181, 1s}, got %v", err)
		}
		if time.Since(start) > 10*time.Second {
			t.Fatalf("took %s to give up with a 1s lock timeout", time.Since(start))
		}
		for _, want := range []string{"waited 1s for a table lock", "is still at version 181", "nothing needs repair", LockTimeoutEnv, LockTimeoutRunbookPath} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("message lacks %q: %v", want, err)
			}
		}
		if gooseRows(t, db, 182) != 0 {
			t.Fatal("the stopped file has a version row")
		}
		release()
		if err := up(context.Background(), dsn, fsys); err != nil {
			t.Fatalf("Up after the lock is gone: %v", err)
		}
		if gooseRows(t, db, 182) != 1 {
			t.Fatal("182 did not apply on the rerun")
		}
	})

	t.Run("baseline and file in one Up", func(t *testing.T) {
		dsn, db := scratchDB(t, "velox_migrate_t7b")
		mustExec(t, db, `CREATE SCHEMA side`)
		mustExec(t, db, `CREATE TABLE side.t (id int)`)
		release := blockTable(t, db, "side.t")
		fsys := files(map[string]string{"00181_side.sql": "-- +goose Up\nALTER TABLE side.t ADD COLUMN c int;\n-- +goose Down\nALTER TABLE side.t DROP COLUMN c;\n"})
		err := upWithin(t, 20*time.Second, dsn, fsys, release)
		var lte *LockTimeoutError
		if !errors.As(err, &lte) || lte.Version != 181 || lte.Previous != 180 {
			t.Fatalf("want LockTimeoutError{181, previous 180}, got %v", err)
		}
		if gooseRows(t, db, 180) != 1 || gooseRows(t, db, 181) != 0 {
			t.Fatal("want the baseline applied and 181 not")
		}
	})
}

// T8. An index build fails on duplicate rows and leaves an INVALID index;
// once the rows are fixed, a rerun builds it valid. Mutations: a template
// without DROP ... IF EXISTS (the rerun fails with 42P07), or with IF NOT
// EXISTS (the rerun keeps the INVALID index).
//
// T9. Between the two, a new Up with a normal file still stops at
// MIGRATION_LOCK_TIMEOUT. Mutation: reuse the pool across Up calls; the
// failed build's session keeps lock_timeout = 0.
func TestUpIndexBuildFailure(t *testing.T) {
	t.Setenv(LockTimeoutEnv, "1s")
	dsn, db := scratchDB(t, "velox_migrate_t8")
	ctx := context.Background()
	base := map[string]string{"00181_widgets.sql": widgetsFile}
	if err := up(ctx, dsn, files(base)); err != nil {
		t.Fatalf("Up: %v", err)
	}
	mustExec(t, db, `INSERT INTO widgets VALUES (1), (2), (2)`)

	build := files(base)
	build["00182_widgets_id_unique.sql"] = indexFile("idx_widgets_id_unique", "CREATE UNIQUE INDEX CONCURRENTLY idx_widgets_id_unique ON widgets (id);")
	err := up(ctx, dsn, build)
	var fe *FileError
	if !errors.As(err, &fe) || fe.Version != 182 || !fe.NoTx {
		t.Fatalf("want a no-transaction FileError for 182, got %v", err)
	}
	for _, want := range []string{"23505", "detail: Key (id)=(2) is duplicated.", "constraint: idx_widgets_id_unique", "INVALID", "Running the upgrade again is safe"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message lacks %q: %v", want, err)
		}
	}
	if indexValid(t, db, "idx_widgets_id_unique") || gooseRows(t, db, 182) != 0 {
		t.Fatal("want an INVALID (or no) index and no row for 182")
	}

	// T9: the next command starts on new sessions.
	release := blockTable(t, db, "widgets")
	other := files(base)
	other["00183_widgets_note.sql"] = &fstest.MapFile{Data: []byte("-- +goose Up\nALTER TABLE widgets ADD COLUMN note text;\n-- +goose Down\nALTER TABLE widgets DROP COLUMN note;\n")}
	var lte *LockTimeoutError
	if err := upWithin(t, 20*time.Second, dsn, other, release); !errors.As(err, &lte) {
		t.Fatalf("T9: want LockTimeoutError, got %v", err)
	}
	release()

	mustExec(t, db, `DELETE FROM widgets WHERE id = 2`)
	mustExec(t, db, `INSERT INTO widgets VALUES (2)`)
	if err := up(ctx, dsn, build); err != nil {
		t.Fatalf("rerun after fixing the rows: %v", err)
	}
	if !indexValid(t, db, "idx_widgets_id_unique") {
		t.Fatal("the rerun left the index INVALID")
	}
	if n := gooseRows(t, db, 182); n != 1 {
		t.Fatalf("182: %d rows, want 1", n)
	}
}

// T10. Another session holds goose's lock for the whole wait. Mutation:
// drop the migrationSession wrapper; goose's bare "failed to acquire lock" comes
// back instead.
func TestUpLockWait(t *testing.T) {
	dsn, db := scratchDB(t, "velox_migrate_t10")
	fastLockWait(t, 2)
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(context.Background(), `SELECT pg_advisory_lock(4097083626)`); err != nil {
		t.Fatal(err)
	}
	err = up(context.Background(), dsn, noFiles)
	var lw *LockWaitError
	if !errors.As(err, &lw) || lw.Waited != 2*time.Second {
		t.Fatalf("want LockWaitError after 2s, got %v", err)
	}
	if !strings.Contains(err.Error(), "running now") || !strings.Contains(err.Error(), "4097083626") {
		t.Errorf("message: %v", err)
	}
	if gooseRows(t, db, 180) != 0 || tableExists(t, db, "public.tenants") {
		t.Fatal("a command that never got the lock changed the schema")
	}
}

// T11. The boot check. Mutation: treat row 5 (a v0.8 database at 180) as
// ready; the server would then start before migration 180 records it.
func TestCheckSchemaReady(t *testing.T) {
	ctx := context.Background()

	t.Run("empty", func(t *testing.T) {
		_, db := scratchDB(t, "velox_migrate_t11a")
		if err := CheckSchemaReady(db); err == nil || !strings.Contains(err.Error(), "1 migration to apply; run `velox migrate`") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("v0.8 at 180", func(t *testing.T) {
		dsn, db := scratchDB(t, "velox_migrate_t11b")
		if err := up(ctx, dsn, noFiles); err != nil {
			t.Fatal(err)
		}
		buildV08(t, db, 180, false)
		if err := CheckSchemaReady(db); err == nil || !strings.Contains(err.Error(), "1 migration to apply; run `velox migrate`") {
			t.Fatalf("got %v", err)
		}
		if err := up(ctx, dsn, noFiles); err != nil {
			t.Fatalf("adopt: %v", err)
		}
		if err := CheckSchemaReady(db); err != nil {
			t.Fatalf("after adoption: %v", err)
		}
		// Restored over: a v0.8.x archive brings back the unmarked record.
		mustExec(t, db, `COMMENT ON TABLE public.schema_migrations IS NULL`)
		var refused *V08Error
		if err := CheckSchemaReady(db); !errors.As(err, &refused) || refused.kind != v08Mismatch {
			t.Fatalf("restored over: want M2, got %v", err)
		}
	})
	t.Run("up to date, then ahead", func(t *testing.T) {
		dsn, db := scratchDB(t, "velox_migrate_t11c")
		if err := up(ctx, dsn, noFiles); err != nil {
			t.Fatal(err)
		}
		if err := CheckSchemaReady(db); err != nil {
			t.Fatalf("up to date: %v", err)
		}
		recordGoose(t, db, 181)
		if err := CheckSchemaReady(db); err != nil {
			t.Fatalf("ahead: want a WARN and no error, got %v", err)
		}
	})
}

// T12. Status is read-only: it creates no goose_db_version and waits for
// no lock, so it answers while a migration runs. Mutation: build it on
// goose's Provider.Status, which creates the table and takes the lock.
func TestReadPlanIsReadOnly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, tc := range []struct {
		name  string
		build func(t *testing.T, dsn string, db *sql.DB)
		state PlanState
	}{
		{"empty", func(*testing.T, string, *sql.DB) {}, PlanEmpty},
		{"v0.8 at 180", func(t *testing.T, dsn string, db *sql.DB) {
			if err := up(ctx, dsn, noFiles); err != nil {
				t.Fatal(err)
			}
			buildV08(t, db, 180, false)
		}, PlanAdopt},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dsn, db := scratchDB(t, "velox_migrate_t12_"+strings.ReplaceAll(strings.ReplaceAll(tc.name, " ", "_"), ".", ""))
			tc.build(t, dsn, db)
			holder, err := db.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = holder.Close() }()
			if _, err := holder.ExecContext(ctx, `SELECT pg_advisory_lock(4097083626)`); err != nil {
				t.Fatal(err)
			}
			var pid int
			if err := holder.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				t.Fatal(err)
			}
			pool, err := openPool(dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = pool.Close() }()
			p, err := readPlan(ctx, pool, noFiles)
			if err != nil {
				t.Fatalf("readPlan: %v", err)
			}
			if p.State != tc.state {
				t.Fatalf("state %q, want %q", p.State, tc.state)
			}
			if !strings.Contains(p.Running, fmt.Sprintf("pid %d", pid)) {
				t.Fatalf("running now = %q, want the holder (pid %d)", p.Running, pid)
			}
			if tableExists(t, db, versionTable) {
				t.Fatal("status created goose_db_version")
			}
		})
	}
}

// T14. Round trip after 180, and 180 itself never rolls back. Mutation:
// an empty Down for 180.
func TestDownRoundTrip(t *testing.T) {
	dsn, db := scratchDB(t, "velox_migrate_t14")
	ctx := context.Background()
	fsys := files(map[string]string{"00181_widgets.sql": widgetsFile})
	fsys["00182_widgets_id.sql"] = indexFile("idx_widgets_id", "CREATE INDEX CONCURRENTLY idx_widgets_id ON widgets (id);")
	if err := up(ctx, dsn, fsys); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if v, err := down(ctx, dsn, fsys, baselineVersion); err != nil || v != baselineVersion {
		t.Fatalf("DownTo(180): version %d, %v", v, err)
	}
	if tableExists(t, db, "public.widgets") {
		t.Fatal("181's Down left widgets")
	}
	if err := up(ctx, dsn, fsys); err != nil {
		t.Fatalf("Up again: %v", err)
	}
	if !indexValid(t, db, "idx_widgets_id") {
		t.Fatal("index missing after the second Up")
	}
	for v := int64(182); v > baselineVersion; v-- {
		if got, err := down(ctx, dsn, fsys, -1); err != nil || got != v-1 {
			t.Fatalf("Down from %d: version %d, %v", v, got, err)
		}
	}
	got, err := down(ctx, dsn, fsys, -1)
	if !errors.Is(err, errBaselineDown) || got != baselineVersion {
		t.Fatalf("Down from 180: version %d, %v; want the baseline refusal", got, err)
	}
	if gooseRows(t, db, 180) != 1 || !tableExists(t, db, "public.tenants") {
		t.Fatal("the refused Down changed the database")
	}
}
