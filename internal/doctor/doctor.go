// Package doctor is the money-invariant sweep: a read-only interrogation of
// a Velox database for states no legal writer can produce.
//
// It exists because the defect class it catches is invisible to every other
// gate: walks prove flows, CI pins behaviors, but a row quietly violating an
// invariant sits in the data saying nothing — the $46.69 of permanently
// stranded tax reversals (ADR-111's "separate defect found while measuring")
// was found by exactly one ad-hoc query of this kind, months after the rows
// went bad. This package makes that query class permanent.
//
// The one rule (docs/dev/product-stability-plan.md): a check ships only as an
// EXACT predicate — zero legal false positives — or it does not ship. Every
// check below survived an adversarial pass hunting legal counterexamples
// (grandfathered rows, deliberate states like ADR-107 parking, simulated
// rows, lagging best-effort stamps). A doctor that cries wolf gets ignored,
// and an ignored doctor is worse than none. When a fixing PR closes a new
// bug class, it adds the class's invariant here in the same PR.
package doctor

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Check is one invariant: SQL whose result rows are violations. The query
// must select `id` first (the violating row's primary key) followed by any
// columns useful for triage; it runs with no tenant binding (the doctor
// sweeps cross-tenant by design, on a connection that can see everything).
type Check struct {
	// Name is the stable identifier, kebab-case, unique across domains.
	Name string
	// Domain groups checks for reporting (invoices, credit-ledger, …).
	Domain string
	// Why is one sentence: the writer that maintains this invariant and
	// what a violation means in money terms.
	Why string
	// SQL returns violating rows: first column MUST be the row id.
	SQL string
}

// Violation is one row that a check flagged.
type Violation struct {
	Check  Check
	RowID  string
	Detail string // remaining selected columns, "col=val" joined
}

// Result is one full sweep.
type Result struct {
	Started    time.Time
	Duration   time.Duration
	Checks     int
	Violations []Violation
	// Errors are checks that failed to RUN (schema drift, connection).
	// A check that cannot run is itself a finding — never silently skipped.
	Errors []error
}

// Run executes every check against db. Read-only by construction: checks
// are SELECTs, and Run additionally sets the transaction read-only so a
// mistyped check cannot write.
func Run(ctx context.Context, db *sql.DB, checks []Check) Result {
	res := Result{Started: time.Now()}
	for _, c := range checks {
		res.Checks++
		if err := runOne(ctx, db, c, &res); err != nil {
			res.Errors = append(res.Errors, fmt.Errorf("check %s: %w", c.Name, err))
		}
	}
	res.Duration = time.Since(res.Started)
	return res
}

func runOne(ctx context.Context, db *sql.DB, c Check, res *Result) error {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, c.SQL)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		v := Violation{Check: c, RowID: str(vals[0])}
		for i := 1; i < len(cols); i++ {
			if v.Detail != "" {
				v.Detail += " "
			}
			v.Detail += cols[i] + "=" + str(vals[i])
		}
		res.Violations = append(res.Violations, v)
	}
	return rows.Err()
}

func str(v any) string {
	switch t := v.(type) {
	case nil:
		return "NULL"
	case []byte:
		return string(t)
	case time.Time:
		return t.UTC().Format(time.RFC3339)
	default:
		return fmt.Sprint(t)
	}
}
