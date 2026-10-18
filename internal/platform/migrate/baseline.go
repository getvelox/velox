package migrate

import (
	"context"
	"database/sql"
	_ "embed" // the baseline file
	"errors"

	"github.com/pressly/goose/v3"
)

// baselineVersion is migration 180: the last of the 178 golang-migrate
// files Velox v0.8.0 and v0.8.1 shipped, and the version their databases
// record. Version numbers do not change across the switch to goose, so new
// files start at 00181.
const baselineVersion int64 = 180

// baselineName labels migration 180 in logs and errors. It has no file in
// sql/: it is a Go migration.
const baselineName = "00180 v0.8.1 baseline"

// baselineSQL is the schema the 178 files build, generated from the v0.8.1
// image by scripts/gen-baseline.sh. It lives outside sql/, so goose never
// collects it as a migration of its own.
//
//go:embed baseline/00180_baseline.sql
var baselineSQL string

// errBaselineDown is migration 180's Down: the database never goes below 180.
var errBaselineDown = errors.New("the v0.8.1 baseline (migration 180) cannot be rolled back: restore the backup taken before the upgrade")

// newMigration180 builds migration 180 for one provider. goose runs it
// inside its session lock and its transaction, and writes row 180 in that
// same transaction, so a failure leaves nothing behind and two copies
// cannot both run it.
func newMigration180() *goose.Migration {
	return goose.NewGoMigration(baselineVersion,
		&goose.GoFunc{RunTx: adoptOrBaseline},
		&goose.GoFunc{RunTx: func(context.Context, *sql.Tx) error { return errBaselineDown }},
	)
}

// adoptOrBaseline re-reads the v0.8 record under the lock (decide's rows
// 4 to 6) and then:
//
//   - with no schema_migrations, runs the baseline: one Exec with no
//     arguments, which pgx sends as one simple query inside goose's
//     transaction;
//   - with a v0.8 record at (180, false), changes no table: it marks the
//     record as frozen, so a later restore or v0.8.x binary that brings
//     the record back is caught (decide's row 3);
//   - with any other record, refuses (M1).
func adoptOrBaseline(ctx context.Context, tx *sql.Tx) error {
	rec, err := readV08Record(ctx, tx)
	if err != nil {
		return err
	}
	switch {
	case !rec.exists:
		_, err = tx.ExecContext(ctx, baselineSQL)
	case rec.clean180():
		_, err = tx.ExecContext(ctx, `COMMENT ON TABLE public.schema_migrations IS '`+v08Marker+`'`)
	default:
		err = &V08Error{kind: v08NotClean, record: rec}
	}
	return err
}
