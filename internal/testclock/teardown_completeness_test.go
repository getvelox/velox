package testclock

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// teardownKeepAllowlist names tables that carry a customer_id (or
// test_clock_id — ADR-104 widened the discovery) column but are
// DELIBERATELY not torn down when a test clock is deleted, each with the written
// reason. A clock-scoped row left in one of these must be provably inert — no
// wall-clock plane may ever read it by customer — or it re-opens the leak class
// ADR-086 (Design B) closes. A stale entry (table gone, or no longer carrying
// customer_id) fails the test so the list can't rot.
var teardownKeepAllowlist = map[string]string{
	// audit_log is the teardown design's NAMED survivor (ADR-086: "NOT torn
	// down (survivors): the audit log") — forensics must outlive the
	// simulation they describe, and its created_at is wall-clock always
	// (ADR-030). Its test_clock_id column is enrichment on rows that
	// already happened; no wall-clock billing plane reads audit rows to
	// act, so a surviving row is inert by construction. Discovered by the
	// test_clock_id widening (ADR-104), not newly exempted behavior.
	"audit_log": "documented ADR-086 survivor; forensic, wall-clock, never actioned by billing planes",
	// coupons is a tenant-level catalog object, not a customer-owned row. Its
	// customer_id is a nullable, FK-less "restrict to this customer" target on a
	// feature that is CUT pre-launch (ADR-039 — the credit ledger is the discount
	// primitive; coupons stays dormant until the first promo-code ask). No
	// wall-clock plane reads coupons by customer, so a dangling customer_id is
	// inert. Revisit teardown if coupons is ever rebuilt.
	"coupons": "dormant cut feature (ADR-039); tenant catalog, customer_id nullable + FK-less; inert",
}

var deleteFromRe = regexp.MustCompile(`(?i)\bDELETE\s+FROM\s+([a-z_][a-z0-9_]*)`)

// teardownTables is the set of tables whose rows a clock delete removes
// EXPLICITLY — parsed from clockTeardownStatements (the source of truth) plus
// test_clocks (the clock row itself, deleted in Delete after the statements).
func teardownTables() map[string]bool {
	set := map[string]bool{"test_clocks": true}
	for _, stmt := range clockTeardownStatements {
		if m := deleteFromRe.FindStringSubmatch(stmt); m != nil {
			set[m[1]] = true
		}
	}
	return set
}

// TestTeardownCoversEverySimulatedTable is the completeness guard the teardown
// design (ADR-086, Design B) stands on: a test clock's simulated data must be
// torn down COMPLETELY, so every table that can hold a clock-scoped row must be
// either deleted by the teardown or an explicit, reasoned survivor. It
// discovers clock-scoped tables from information_schema — any table carrying a
// customer_id OR a test_clock_id column (ADR-104 added the latter class:
// email_outbox anchors rows to a clock directly, with no customer_id) — so a
// NEW clock-owned table added later fails CI until it is classified.
// This is precisely the guard that would have caught the two real gaps found
// while building the teardown: customer_discounts missing from the delete set,
// and customer_payment_setups (a table that had been dropped).
func TestTeardownCoversEverySimulatedTable(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin bypass: %v", err)
	}
	defer postgres.Rollback(tx)

	rows, err := tx.QueryContext(ctx, `
		SELECT c.table_name
		FROM information_schema.columns c
		JOIN information_schema.tables tb
		  ON tb.table_schema = c.table_schema AND tb.table_name = c.table_name
		WHERE c.table_schema = 'public'
		  AND c.column_name IN ('customer_id', 'test_clock_id')
		  AND tb.table_type  = 'BASE TABLE'
		  AND c.table_name  <> 'test_clocks'
		ORDER BY c.table_name`)
	if err != nil {
		t.Fatalf("discover customer_id tables: %v", err)
	}
	defer func() { _ = rows.Close() }()

	torn := teardownTables()
	seen := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		seen[name] = true
		if torn[name] {
			continue
		}
		if _, ok := teardownKeepAllowlist[name]; ok {
			continue
		}
		t.Errorf("table %q has a customer_id or test_clock_id column but is neither torn down by a clock "+
			"delete nor in teardownKeepAllowlist — a clock-scoped row would survive "+
			"teardown and leak into a wall-clock plane. Add a child-first DELETE to "+
			"clockTeardownStatements, or allowlist it with a reason.", name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	for name := range teardownKeepAllowlist {
		if !seen[name] {
			t.Errorf("teardownKeepAllowlist has %q, but no such table carries a customer_id "+
				"or test_clock_id column anymore — remove the stale exemption.", name)
		}
	}
}

// TestTeardownLeavesNoDanglingReference proves the teardown set is FK-closed:
// for every foreign key whose PARENT rows a clock delete removes, the CHILD rows
// must be removed too. Otherwise the teardown either aborts (a RESTRICT /
// NO ACTION child blocks the parent delete) or silently nulls the child's
// reference and leaks it (SET NULL — the exact original bug shape:
// customers.test_clock_id was ON DELETE SET NULL, which detached the whole set).
// CASCADE children are removed automatically, so the deleted set is first
// expanded to its cascade closure; any non-cascade FK into that closure whose
// child sits outside it is a gap and fails here — no seed data required, so it
// catches child tables the integration test never populates.
func TestTeardownLeavesNoDanglingReference(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin bypass: %v", err)
	}
	defer postgres.Rollback(tx)

	type fk struct{ child, parent, onDelete string }
	rows, err := tx.QueryContext(ctx, `
		SELECT con.conrelid::regclass::text, con.confrelid::regclass::text, con.confdeltype
		FROM pg_constraint con
		WHERE con.contype = 'f' AND con.connamespace = 'public'::regnamespace`)
	if err != nil {
		t.Fatalf("discover foreign keys: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var fks []fk
	for rows.Next() {
		var f fk
		if err := rows.Scan(&f.child, &f.parent, &f.onDelete); err != nil {
			t.Fatalf("scan fk: %v", err)
		}
		f.child = strings.TrimPrefix(f.child, "public.")
		f.parent = strings.TrimPrefix(f.parent, "public.")
		fks = append(fks, f)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	// Cascade closure: start from the explicitly-deleted tables, then repeatedly
	// fold in any CASCADE child of a table already deleted, to a fixpoint.
	deleted := teardownTables()
	for changed := true; changed; {
		changed = false
		for _, f := range fks {
			if f.onDelete == "c" && deleted[f.parent] && !deleted[f.child] {
				deleted[f.child] = true
				changed = true
			}
		}
	}

	actionName := map[string]string{"r": "RESTRICT", "a": "NO ACTION", "n": "SET NULL", "d": "SET DEFAULT"}
	consequence := map[string]string{
		"r": "abort", "a": "abort",
		"n": "leave a nulled, leaked reference", "d": "leave a defaulted, leaked reference",
	}
	for _, f := range fks {
		if !deleted[f.parent] || f.onDelete == "c" || deleted[f.child] {
			continue
		}
		t.Errorf("FK %s → %s (ON DELETE %s): a clock teardown deletes %q but not %q, so it "+
			"would %s. Add a child-first DELETE for %q to clockTeardownStatements (or, if the "+
			"child is a reasoned survivor, prove it inert and allowlist it).",
			f.child, f.parent, actionName[f.onDelete], f.parent, f.child, consequence[f.onDelete], f.child)
	}
}
