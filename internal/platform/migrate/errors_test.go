package migrate

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pressly/goose/v3"
)

func TestIsLockTimeout(t *testing.T) {
	lockErr := &pgconn.PgError{Code: "55P03", Message: "canceling statement due to lock timeout"}
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"nil":            {nil, false},
		"pg error":       {lockErr, true},
		"wrapped":        {fmt.Errorf("partial migration error: %w", lockErr), true},
		"other pg error": {&pgconn.PgError{Code: "42P01"}, false},
		"plain error":    {errors.New("boom"), false},
	} {
		if got := isLockTimeout(tc.err); got != tc.want {
			t.Errorf("%s: got %v, want %v", name, got, tc.want)
		}
	}
}

func TestFileErrorMessages(t *testing.T) {
	dup := &pgconn.PgError{Severity: "ERROR", Code: "23505", Message: `could not create unique index "idx_x"`,
		Detail: "Key (id)=(2) is duplicated.", ConstraintName: "idx_x"}
	got := (&FileError{Version: 182, Name: "00182_x.sql", NoTx: true, Err: dup}).Error()
	for _, want := range []string{"migration 00182_x.sql failed", "(SQLSTATE 23505)", "detail: Key (id)=(2) is duplicated.",
		"constraint: idx_x", "INVALID", "drops the index and builds it again", RunbookPath} {
		if !strings.Contains(got, want) {
			t.Errorf("index-build message lacks %q: %s", want, got)
		}
	}

	got = (&FileError{Version: 181, Name: "00181_y.sql", Err: &pgconn.PgError{Code: "22012", Message: "division by zero", Position: 12}}).Error()
	for _, want := range []string{"none of it was applied", "at character 12", "Fix the cause and run the upgrade again"} {
		if !strings.Contains(got, want) {
			t.Errorf("in-transaction message lacks %q: %s", want, got)
		}
	}

	exists := &pgconn.PgError{Code: "42723", Message: `function "leader_fence" already exists with same argument types`}
	got = (&FileError{Version: 180, Name: baselineName, Err: exists}).Error()
	for _, want := range []string{"00180 v0.8.1 baseline", "already has Velox tables but no migration record", "use an empty database"} {
		if !strings.Contains(got, want) {
			t.Errorf("baseline message lacks %q: %s", want, got)
		}
	}
}

func TestLockTimeoutErrorMessages(t *testing.T) {
	got := (&LockTimeoutError{Version: 182, Name: "00182_x.sql", Previous: 181, Timeout: 5 * time.Second}).Error()
	for _, want := range []string{"migration 00182_x.sql waited 5s for a table lock", "is still at version 181",
		"nothing needs repair", LockTimeoutEnv, LockTimeoutRunbookPath} {
		if !strings.Contains(got, want) {
			t.Errorf("message lacks %q: %s", want, got)
		}
	}
	if got := (&LockTimeoutError{Name: baselineName, Timeout: time.Second}).Error(); !strings.Contains(got, "has no migration applied") {
		t.Errorf("no previous version: %s", got)
	}
}

// upError picks the error type from what failed. Mutation: map a lock
// timeout in an index-build file to LockTimeoutError, whose "nothing needs
// repair" would be false there.
func TestUpErrorMapping(t *testing.T) {
	fsys := fstest.MapFS{
		"00181_a.sql": {Data: []byte("-- +goose Up\nALTER TABLE t ADD COLUMN c int;\n")},
		"00182_b.sql": {Data: []byte("-- +goose NO TRANSACTION\n-- +goose Up\nSET lock_timeout = 0;\n")},
	}
	lockErr := &pgconn.PgError{Code: "55P03", Message: "canceling statement due to lock timeout"}
	partial := func(v int64, typ goose.MigrationType, path string, err error) error {
		return fmt.Errorf("wrapped: %w", &goose.PartialError{
			Applied: []*goose.MigrationResult{{Source: &goose.Source{Version: 180, Type: goose.TypeGo}}},
			Failed:  &goose.MigrationResult{Source: &goose.Source{Version: v, Type: typ, Path: path}},
			Err:     err,
		})
	}
	st := state{}

	var lte *LockTimeoutError
	if err := upError(partial(181, goose.TypeSQL, "00181_a.sql", lockErr), fsys, st, []*goose.MigrationResult{{Source: &goose.Source{Version: 180}}}); !errors.As(err, &lte) || lte.Previous != 180 {
		t.Fatalf("in-tx lock timeout: got %v", err)
	}
	var fe *FileError
	if err := upError(partial(182, goose.TypeSQL, "00182_b.sql", lockErr), fsys, st, nil); !errors.As(err, &fe) || !fe.NoTx {
		t.Fatalf("no-tx lock timeout: want a no-tx FileError, got %v", err)
	}
	refused := &V08Error{kind: v08NotClean, record: v08Record{exists: true, rows: 1, version: 179}}
	var v8 *V08Error
	if err := upError(partial(180, goose.TypeGo, "", refused), fsys, st, nil); !errors.As(err, &v8) || v8 != refused {
		t.Fatalf("migration 180's refusal: got %v", err)
	}
	if err := upError(partial(180, goose.TypeGo, "", sql.ErrConnDone), fsys, st, nil); !errors.As(err, &fe) || fe.Name != baselineName {
		t.Fatalf("migration 180 failure: got %v", err)
	}
	// A lock timeout while migration 180 takes a v0.8 database over: the
	// database is at v0.8's 180, not "no migration applied".
	v08 := state{v08: v08Record{exists: true, rows: 1, version: 180}}
	if err := upError(partial(180, goose.TypeGo, "", lockErr), fsys, v08, nil); !errors.As(err, &lte) || lte.Previous != 180 ||
		!strings.Contains(err.Error(), "is still at version 180") {
		t.Fatalf("lock timeout during the v0.8 takeover: want Previous 180, got %v", err)
	}
	lw := &LockWaitError{Waited: time.Minute}
	if err := upError(fmt.Errorf("failed to initialize: %w", lw), fsys, st, nil); err != lw {
		t.Fatalf("lock wait: got %v", err)
	}
}

func TestIsNoTxFile(t *testing.T) {
	fsys := fstest.MapFS{
		"a.sql": {Data: []byte("-- +goose NO TRANSACTION\n-- +goose Up\nSELECT 1;\n")},
		"b.sql": {Data: []byte("-- +goose Up\nSELECT 1; -- +goose NO TRANSACTION is only text here\n")},
		"c.sql": {Data: []byte("-- +goose no transaction\r\n-- +goose Up\nSELECT 1;\n")},
		// goose v3.27.0 (internal/sqlparser) reads each of these as NO
		// TRANSACTION too: it drops every "--" and the first "+goose" from
		// the line and compares the rest without case.
		"d.sql": {Data: []byte("--+goose NO TRANSACTION\n-- +goose Up\nSELECT 1;\n")},
		"e.sql": {Data: []byte("-- +goose\tNO TRANSACTION\n-- +goose Up\nSELECT 1;\n")},
		"f.sql": {Data: []byte("-- +goose Up\nSELECT 1;\n-- +goose Down\n----  +goose   No Transaction\nSELECT 1;\n")},
		// goose refuses a file whose annotation line is indented, so it never
		// runs it without a transaction.
		"g.sql": {Data: []byte(" -- +goose NO TRANSACTION\n-- +goose Up\nSELECT 1;\n")},
	}
	for name, want := range map[string]bool{"a.sql": true, "b.sql": false, "c.sql": true, "d.sql": true, "e.sql": true,
		"f.sql": true, "g.sql": false, "missing.sql": false} {
		if got := isNoTxFile(fsys, name); got != want {
			t.Errorf("%s: got %v, want %v", name, got, want)
		}
	}
}
