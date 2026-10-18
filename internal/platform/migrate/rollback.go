package migrate

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	"github.com/pressly/goose/v3"
)

// ErrNothingToRollBack is returned by Down when no migration is applied.
var ErrNothingToRollBack = errors.New("no migration is applied: nothing to roll back")

// Down rolls back the latest migration, one step, and returns the version
// the database is at afterwards. It is the local loop for writing a new
// migration: `velox migrate rollback` refuses outside APP_ENV=local, because
// rolling back loses data the newer release wrote. Migration 180 never
// rolls back (errBaselineDown).
func Down(dsn string) (int64, error) {
	return down(context.Background(), dsn, migrationFiles(), -1)
}

// down rolls back one step when to < 0, or every migration above to.
func down(ctx context.Context, dsn string, fsys fs.FS, to int64) (int64, error) {
	db, err := openPool(dsn)
	if err != nil {
		return 0, err
	}
	defer func() { _ = db.Close() }()

	st, err := readState(ctx, db)
	if err != nil {
		return 0, fmt.Errorf("read migration state: %w", err)
	}
	d := decide(st)
	if err := d.refusal(); err != nil {
		return 0, err
	}
	if d.act != actNormal {
		return 0, ErrNothingToRollBack
	}
	p, err := newProvider(db, fsys, true)
	if err != nil {
		return 0, err
	}
	if to < 0 {
		_, err = p.Down(ctx)
	} else {
		_, err = p.DownTo(ctx, to)
	}
	if err != nil {
		var partial *goose.PartialError
		switch {
		case errors.Is(err, errBaselineDown):
			return st.gooseVersion(), errBaselineDown
		case errors.Is(err, goose.ErrNoNextVersion):
			return st.gooseVersion(), ErrNothingToRollBack
		case errors.As(err, &partial):
			src := partial.Failed.Source
			noTx := src.Type == goose.TypeSQL && isNoTxFile(fsys, src.Path)
			return st.gooseVersion(), &FileError{Version: src.Version, Name: sourceName(src), NoTx: noTx, Err: partial.Err}
		}
		return st.gooseVersion(), upError(err, fsys, st, nil)
	}
	after, err := readState(ctx, db)
	if err != nil {
		return 0, fmt.Errorf("read migration state: %w", err)
	}
	return after.gooseVersion(), nil
}
