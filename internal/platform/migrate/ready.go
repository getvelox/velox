package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
)

// CheckSchemaReady refuses to let the server start on a database this
// binary cannot serve. It runs the same state check as `velox migrate`,
// read-only, on the caller's pool, and returns an error when:
//
//   - the state check refuses the database (V08Error, with its next step);
//   - a migration this binary ships is not applied, including migration
//     180 on an empty or a v0.8 database. The app would fail on any query
//     that touches a new column or table.
//
// Call it at start-up, after Up when RUN_MIGRATIONS_ON_BOOT is on, before
// serving. Refusing is safer than serving: the orchestrator retries, and
// by then the migration has run.
func CheckSchemaReady(db *sql.DB) error {
	ctx := context.Background()
	st, err := readState(ctx, db)
	if err != nil {
		return fmt.Errorf("read migration state: %w", err)
	}
	files, err := embeddedFiles(db, migrationFiles())
	if err != nil {
		return err
	}
	p := planFor(st, files)
	switch p.State {
	case PlanRefused:
		return p.Refusal
	case PlanEmpty, PlanBehind:
		return fmt.Errorf("schema behind code: %s to apply; run `velox migrate`", countMigrations(len(p.Pending)))
	case PlanAdopt:
		return fmt.Errorf("schema behind code: %s to apply; run `velox migrate`", countMigrations(len(p.Pending)+1))
	case PlanAhead:
		// This (older) binary runs on a newer schema: a rolling deploy
		// overlap or a rollback. It WARNs and continues: old code may not
		// understand new columns or enum values, and no expand/contract
		// rule is enforced yet. Whether to refuse instead is open; see
		// docs/records/ha-readiness-2026-07-06.md (item 3) before tightening.
		var ahead []int64
		for _, a := range p.Ahead {
			ahead = append(ahead, a.Version)
		}
		slog.Warn("schema ahead of binary — likely a rollback in progress",
			"database_version", p.Database,
			"binary_version", p.Latest,
			"versions_not_shipped", ahead,
		)
		return nil
	}
	slog.Info("schema ready", "version", p.Database, "binary_expects", p.Latest)
	return nil
}
