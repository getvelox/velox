package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"

	"github.com/pressly/goose/v3/database"
)

// The two version records (ADR-125):
//
//   - public.goose_db_version, which goose writes: one row per applied
//     migration, plus row 0.
//   - public.schema_migrations, golang-migrate's single (version, dirty)
//     row from v0.8.x. On a database this release took over it stays,
//     frozen at (180, false) and marked with v08Marker, so a v0.8.x binary
//     still sees a full database and does not replay 0001. A new database
//     never has it.
//
// readState reads both, read-only: no lock, and it creates nothing (goose's
// own Status, HasPending and GetVersions all create goose_db_version).
// decide turns what it read into one of the rows below. Up, Down,
// CheckSchemaReady and `velox migrate status` all use the same pair, so a
// state that is refused is refused everywhere.

// v08Marker is the table comment migration 180 puts on schema_migrations
// when it takes a v0.8 database over. golang-migrate's writes (TRUNCATE and
// INSERT) keep it. A v0.8.x backup restored with --clean, or a v0.8.x
// binary creating the table afresh, brings the table back without it, and
// that is how decide tells a frozen record from one that came back.
const v08Marker = "velox: frozen at 180; the migration record moved to public.goose_db_version"

// v08Record is golang-migrate's record in public.schema_migrations.
type v08Record struct {
	exists  bool
	rows    int // golang-migrate keeps one row; none means "no version"
	version int64
	dirty   bool
	marked  bool // carries v08Marker
}

// clean180 is the only v0.8 record this release takes over: v0.8.0 and
// v0.8.1 both end at 180.
func (r v08Record) clean180() bool {
	return r.exists && r.rows == 1 && r.version == baselineVersion && !r.dirty
}

func (r v08Record) describe() string {
	var s string
	switch r.rows {
	case 0:
		s = "no version"
	case 1:
		s = fmt.Sprintf("version %d, dirty=%v", r.version, r.dirty)
	default:
		s = fmt.Sprintf("%d rows, highest version %d", r.rows, r.version)
	}
	if r.rows == 1 && r.version == baselineVersion && !r.dirty && !r.marked {
		s += ", without the comment this release adds when it takes a database over"
	}
	return s
}

// state is what readState found.
type state struct {
	// outside is the schema that an unqualified schema_migrations or
	// tenants resolves to through search_path, when that is not public.
	outside string

	gooseTable bool
	// applied lists the versions in goose_db_version, ascending, without
	// row 0.
	applied []int64
	v08     v08Record
}

func (s state) has(version int64) bool {
	for _, v := range s.applied {
		if v == version {
			return true
		}
	}
	return false
}

// gooseVersion is the highest version goose recorded, 0 when none.
func (s state) gooseVersion() int64 {
	if len(s.applied) == 0 {
		return 0
	}
	return s.applied[len(s.applied)-1]
}

// queryer is what readState needs from *sql.DB or *sql.Tx.
type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// versionStore reads public.goose_db_version the way goose does.
func versionStore() (database.StoreExtender, error) {
	s, err := database.NewStore(database.DialectPostgres, versionTable)
	if err != nil {
		return nil, err
	}
	ext, ok := s.(database.StoreExtender)
	if !ok {
		return nil, errors.New("goose's postgres store cannot check whether its table exists")
	}
	return ext, nil
}

func readState(ctx context.Context, db *sql.DB) (state, error) {
	var st state

	// Location: v0.8.1 read its record unqualified, so an install that kept
	// its tables outside public found them through search_path.
	err := db.QueryRowContext(ctx, `
		SELECT n.nspname
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.oid IN (to_regclass('schema_migrations'), to_regclass('tenants'))
		  AND n.nspname <> 'public'
		ORDER BY 1 LIMIT 1`).Scan(&st.outside)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return st, fmt.Errorf("read where the tables are: %w", err)
	}

	store, err := versionStore()
	if err != nil {
		return st, err
	}
	st.gooseTable, err = store.TableExists(ctx, db)
	if err != nil {
		return st, err
	}
	if st.gooseTable {
		rows, err := store.ListMigrations(ctx, db)
		if err != nil {
			return st, err
		}
		for _, r := range rows {
			if r.Version != 0 {
				st.applied = append(st.applied, r.Version)
			}
		}
		sort.Slice(st.applied, func(i, j int) bool { return st.applied[i] < st.applied[j] })
	}

	st.v08, err = readV08Record(ctx, db)
	return st, err
}

// readV08Record reads public.schema_migrations. Migration 180 calls it
// again inside goose's lock and transaction.
func readV08Record(ctx context.Context, q queryer) (v08Record, error) {
	var r v08Record
	if err := q.QueryRowContext(ctx, `SELECT to_regclass('public.schema_migrations') IS NOT NULL`).Scan(&r.exists); err != nil {
		return r, fmt.Errorf("look for schema_migrations: %w", err)
	}
	if !r.exists {
		return r, nil
	}
	var version sql.NullInt64
	var dirty sql.NullBool
	err := q.QueryRowContext(ctx, `
		SELECT count(*), max(version), bool_or(dirty),
		       coalesce(obj_description('public.schema_migrations'::regclass, 'pg_class') = $1, false)
		FROM public.schema_migrations`, v08Marker).Scan(&r.rows, &version, &dirty, &r.marked)
	if err != nil {
		return r, fmt.Errorf("read schema_migrations: %w", err)
	}
	r.version, r.dirty = version.Int64, dirty.Bool
	return r, nil
}

// action is what a command may do with a state.
type action int

const (
	actRefuse   action = iota
	actNormal          // goose has 180: run goose as usual
	actBaseline        // empty: migration 180 runs the baseline
	actAdopt           // v0.8 at (180, false): migration 180 marks the record
)

type decision struct {
	row int // the row of the table in ADR-125, for tests and status
	act action
	err *V08Error // set when act is actRefuse
}

// decide is pure. Its rows are checked in order:
//
//	row  goose has 180  public.schema_migrations                    decision
//	0    -              tables found outside public                 refuse (M3)
//	1    yes            absent                                      normal
//	2    yes            (180, false) and marked                     normal
//	3    yes            anything else                               refuse (M2)
//	4    no             absent                                      baseline
//	5    no             (180, false), marked or not                 adopt
//	6    no             anything else                               refuse (M1)
//
// "goose has 180: no" covers both no goose table and one holding only
// row 0, which a rolled-back migration 180 leaves behind. A database with
// Velox tables but no record lands in row 4; the baseline then fails on
// its first CREATE, inside its transaction.
func decide(s state) decision {
	if s.outside != "" {
		return decision{row: 0, err: &V08Error{kind: v08OutsidePublic, schema: s.outside}}
	}
	if s.has(baselineVersion) {
		switch {
		case !s.v08.exists:
			return decision{row: 1, act: actNormal}
		case s.v08.clean180() && s.v08.marked:
			return decision{row: 2, act: actNormal}
		default:
			return decision{row: 3, err: &V08Error{kind: v08Mismatch, record: s.v08, goose: s.gooseVersion()}}
		}
	}
	switch {
	case !s.v08.exists:
		return decision{row: 4, act: actBaseline}
	case s.v08.clean180():
		return decision{row: 5, act: actAdopt}
	default:
		return decision{row: 6, err: &V08Error{kind: v08NotClean, record: s.v08}}
	}
}

// refusal returns the decision's error, or nil. It returns a plain error
// interface so a nil *V08Error never becomes a non-nil error.
func (d decision) refusal() error {
	if d.err == nil {
		return nil
	}
	return d.err
}
