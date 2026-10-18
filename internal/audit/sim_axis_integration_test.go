package audit_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// emit writes one audit row on its own tx, under the ctx binding the caller
// supplies — the same path every production emitter takes.
// analyze refreshes the planner's stats — without it the planner has no idea
// the clock's rows are a minority and cannot make the choice this test is about.
// analyze gathers statistics on audit_log, as its owner. The app role
// cannot: Postgres 16 skips ANALYZE on a table the role does not own, with
// only a WARNING. Without statistics every candidate index costs the same
// on these small fixtures, and the planner breaks the tie by index creation
// order — which the v0.8.1 baseline (ADR-125) creates by name, not in
// migration order. With statistics the plan follows the costs.
func analyze(t *testing.T, _ *postgres.DB, ctx context.Context, _ string) {
	t.Helper()
	if _, err := testutil.AdminPool(t).ExecContext(ctx, `ANALYZE audit_log`); err != nil {
		t.Fatalf("analyze: %v", err)
	}
}

func emit(t *testing.T, db *postgres.DB, ctx context.Context, tenantID string, logger *audit.Logger, e audit.Entry) {
	t.Helper()
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if err := logger.LogInTx(ctx, tx, e); err != nil {
		t.Fatalf("LogInTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// TestSimAxis_FilterAndWindow covers the read path the sim axis exists for:
// scope to one clock, and window in SIMULATED time.
//
// There is deliberately no "order by simulated time" — see QueryFilter. Within a
// clock it would produce the same order as created_at (advances are monotonic,
// and every row of ONE advance shares that advance's instant), and across clocks
// it would interleave unrelated simulations into a timeline that never happened.
//
// The collapse is the whole problem. A clock advance replays months of billing
// in a few hundred milliseconds, so created_at (wall-clock, ADR-030, and
// correctly so — it answers "when did the operator click Advance") orders those
// rows by nothing meaningful and windows them into a single day. Only
// sim_effective_at can say which simulated month a row belongs to.
func TestSimAxis_FilterAndWindow(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Sim Axis Read")
	logger := audit.NewLogger(db)

	clockA := "vlx_tclk_aaa"
	clockB := "vlx_tclk_bbb"
	jan := time.Date(2027, 1, 15, 0, 0, 0, 0, time.UTC)
	feb := time.Date(2027, 2, 15, 0, 0, 0, 0, time.UTC)
	mar := time.Date(2027, 3, 15, 0, 0, 0, 0, time.UTC)

	// THREE ADVANCES of one clock (jan, feb, mar) — the only way one clock's rows
	// can carry different simulated instants. A SINGLE advance stamps everything
	// it settles with ONE instant (it stands at the new time and closes what came
	// due; it does not replay), so a fixture with three instants from one advance
	// would model a state production cannot produce.
	//
	// All of them are written back-to-back, so their wall-clock created_at values
	// are milliseconds apart while their simulated instants are months apart —
	// which is exactly why created_at cannot answer a sim-time question.
	emit(t, db, ctx, tenantID, logger, audit.Entry{Action: "finalize", ResourceType: "invoice", ResourceID: "inv_mar"})
	emit(t, db, ctx, tenantID, logger, audit.Entry{Action: "finalize", ResourceType: "invoice", ResourceID: "inv_jan"})
	emit(t, db, ctx, tenantID, logger, audit.Entry{Action: "finalize", ResourceType: "invoice", ResourceID: "inv_feb"})
	// Re-emit them WITH sim context (the three above are the wall-clock control
	// group: same tenant, no clock — they must never appear in a sim query).
	simA := func(at time.Time) context.Context {
		return clock.WithSim(ctx, clock.Sim{At: at, TestClockID: clockA})
	}
	emit(t, db, simA(mar), tenantID, logger, audit.Entry{Action: "finalize", ResourceType: "invoice", ResourceID: "sim_mar"})
	emit(t, db, simA(jan), tenantID, logger, audit.Entry{Action: "finalize", ResourceType: "invoice", ResourceID: "sim_jan"})
	emit(t, db, simA(feb), tenantID, logger, audit.Entry{Action: "cancel", ResourceType: "subscription", ResourceID: "sim_feb"})
	// A second clock's rows — the filter must not bleed across simulations.
	emit(t, db, clock.WithSim(ctx, clock.Sim{At: feb, TestClockID: clockB}), tenantID, logger,
		audit.Entry{Action: "finalize", ResourceType: "invoice", ResourceID: "other_clock"})

	t.Run("test_clock_id returns exactly that clock's rows", func(t *testing.T) {
		got, _, err := logger.Query(ctx, tenantID, audit.QueryFilter{TestClockID: clockA})
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		ids := map[string]bool{}
		for _, e := range got {
			ids[e.ResourceID] = true
			if e.TestClockID != clockA {
				t.Errorf("row %s leaked from clock %q", e.ResourceID, e.TestClockID)
			}
		}
		for _, want := range []string{"sim_jan", "sim_feb", "sim_mar"} {
			if !ids[want] {
				t.Errorf("missing %s from clock A's slice", want)
			}
		}
		for _, unwanted := range []string{"other_clock", "inv_jan", "inv_feb", "inv_mar"} {
			if ids[unwanted] {
				t.Errorf("%s must not appear in clock A's slice", unwanted)
			}
		}
		if len(got) != 3 {
			t.Errorf("clock A slice size: got %d, want 3", len(got))
		}
	})

	// The axis records WHEN THE CLOCK STOOD, and separates ADVANCES. It does not
	// (and cannot) separate the periods inside one advance — see the fixture note.
	t.Run("each advance's rows carry that advance's instant", func(t *testing.T) {
		got, _, err := logger.Query(ctx, tenantID, audit.QueryFilter{TestClockID: clockA})
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		want := map[string]time.Time{"sim_jan": jan, "sim_feb": feb, "sim_mar": mar}
		for _, e := range got {
			at, ok := want[e.ResourceID]
			if !ok {
				continue
			}
			if e.SimEffectiveAt == nil || !e.SimEffectiveAt.Equal(at) {
				t.Errorf("%s sim_effective_at = %v, want %v", e.ResourceID, e.SimEffectiveAt, at)
			}
			// The property that makes the axis necessary: the wall clock says all
			// three happened in the same second.
			if e.CreatedAt.Sub(got[0].CreatedAt).Abs() > time.Minute {
				t.Errorf("%s: fixture no longer models one wall-clock moment", e.ResourceID)
			}
		}
	})

	t.Run("sim_from / sim_to window in SIMULATED time", func(t *testing.T) {
		got, _, err := logger.Query(ctx, tenantID, audit.QueryFilter{
			TestClockID: clockA,
			SimFrom:     time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC),
			SimTo:       time.Date(2027, 2, 28, 0, 0, 0, 0, time.UTC),
		})
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		if len(got) != 1 || got[0].ResourceID != "sim_feb" {
			var ids []string
			for _, e := range got {
				ids = append(ids, e.ResourceID)
			}
			t.Errorf("February window: got %v, want [sim_feb]", ids)
		}
		// The same window on the WALL-CLOCK axis returns nothing — every row was
		// written today. That divergence is the feature.
		wall, _, err := logger.Query(ctx, tenantID, audit.QueryFilter{
			TestClockID: clockA,
			DateFrom:    time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC),
			DateTo:      time.Date(2027, 2, 28, 0, 0, 0, 0, time.UTC),
		})
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		if len(wall) != 0 {
			t.Errorf("wall-clock window over simulated dates returned %d rows; the axes are not interchangeable", len(wall))
		}

		// A sim window is defined only over the simulated slice: a wall-clock row
		// has a NULL sim_effective_at, and NULL never satisfies a range predicate,
		// so those rows drop out by construction rather than by a special case.
		simOnly, _, err := logger.Query(ctx, tenantID, audit.QueryFilter{
			SimFrom: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
			SimTo:   time.Date(2027, 12, 31, 0, 0, 0, 0, time.UTC),
		})
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		if len(simOnly) != 4 { // 3 on clock A + 1 on clock B
			t.Errorf("simulated slice size: got %d, want 4", len(simOnly))
		}
		for _, e := range simOnly {
			if e.TestClockID == "" || e.SimEffectiveAt == nil {
				t.Errorf("wall-clock row %s leaked into a sim window", e.ResourceID)
			}
		}
	})

	t.Run("a clock-scoped slice pages without skipping or repeating", func(t *testing.T) {
		// Page size 2 over clock A's 3 rows. The seek is on (created_at, id) — the
		// SAME tuple the sort uses (there is one axis, deliberately), so the pages
		// partition the slice exactly.
		first, _, err := logger.Query(ctx, tenantID, audit.QueryFilter{
			TestClockID: clockA, Limit: 2,
		})
		if err != nil {
			t.Fatalf("page 1: %v", err)
		}
		if len(first) != 2 {
			t.Fatalf("page 1 size: got %d, want 2", len(first))
		}
		last := first[len(first)-1]
		second, _, err := logger.Query(ctx, tenantID, audit.QueryFilter{
			TestClockID: clockA, Limit: 2,
			AfterCreatedAt: last.CreatedAt, AfterID: last.ID,
		})
		if err != nil {
			t.Fatalf("page 2: %v", err)
		}
		if len(second) != 1 {
			t.Fatalf("page 2 size: got %d, want 1 (3 rows, page size 2)", len(second))
		}
		seen := map[string]bool{}
		for _, e := range append(append([]domain.AuditEntry{}, first...), second...) {
			if seen[e.ID] {
				t.Errorf("row %s (%s) returned on both pages", e.ID, e.ResourceID)
			}
			seen[e.ID] = true
		}
		if len(seen) != 3 {
			t.Errorf("pages covered %d distinct rows, want all 3 of clock A's", len(seen))
		}
	})
}

// TestSimAxis_UsesPartialClockIndex pins the plan, under the APP role.
//
// The RLS tenant-isolation policy carries a column-free bypass-GUC OR-arm, so
// the planner can never derive index quals from RLS alone: without the explicit
// tenant_id + livemode predicates Query already carries, a clock-scoped read
// degrades to a seq scan over every tenant's audit rows (the audit e2e's F3).
// The clock index is partial (WHERE test_clock_id IS NOT NULL), so it is only
// eligible when the query itself proves the column is non-NULL — which the
// equality predicate does.
func TestSimAxis_UsesPartialClockIndex(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Sim Axis Explain")
	logger := audit.NewLogger(db)

	// Model the real ratio: a clock's rows are a small MINORITY of a tenant's log
	// (one sandbox against everything the business does). With a fixture where
	// every row is on the clock, the general read index (tenant, livemode,
	// created_at) serves the query just as cheaply and the planner picks it —
	// which tells us nothing, because at real sizes it would have to scan past
	// every wall-clock row to collect the clock's newest 50. The clock index is
	// only worth its write cost if it wins THIS shape.
	frozen := time.Date(2027, 5, 1, 0, 0, 0, 0, time.UTC)
	clockID := "vlx_tclk_explain"
	for i := 0; i < 40; i++ {
		emit(t, db, clock.WithSim(ctx, clock.Sim{At: frozen.Add(time.Duration(i) * time.Hour), TestClockID: clockID}),
			tenantID, logger, audit.Entry{Action: "finalize", ResourceType: "invoice", ResourceID: fmt.Sprintf("inv_%d", i)})
	}
	for i := 0; i < 2000; i++ {
		emit(t, db, ctx, tenantID, logger, audit.Entry{
			Action: "update", ResourceType: "customer", ResourceID: fmt.Sprintf("cus_%d", i),
		})
	}
	analyze(t, db, ctx, tenantID)

	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)

	// Tiny table + default settings would make a seq scan legitimately cheaper,
	// which would tell us nothing about the plan at real sizes. Disable seq scan
	// for this statement so the question becomes "CAN the planner use the clock
	// index" — i.e. is the query shape index-compatible — which is the property
	// that rots (a dropped predicate, a wrapped column, a NULL-permitting
	// filter), not the cost model.
	if _, err := tx.ExecContext(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
		t.Fatalf("set enable_seqscan: %v", err)
	}
	// Bitmap scans too. A bitmap heap scan can use this index and still need a
	// Sort, because bitmaps do not preserve index order — so leaving it enabled
	// makes the Sort assertion below a test of the PLANNER'S SCAN-TYPE CHOICE,
	// which varies with statistics and machine, rather than of the index KEY,
	// which is the thing that actually rots. (It cost a red CI run to learn: this
	// passed locally on an index scan and failed on CI, which chose a bitmap.)
	if _, err := tx.ExecContext(ctx, `SET LOCAL enable_bitmapscan = off`); err != nil {
		t.Fatalf("set enable_bitmapscan: %v", err)
	}

	// EXPLAIN the shape the CODE emits, not a hand-written lookalike: order by
	// (created_at, id) DESC — see auditListOrder — because that is the list the
	// dashboard's clock filter actually runs, and the index exists to serve it.
	// A pin against a query no caller makes cannot catch the regression it is
	// there to catch.
	plan := explain(t, tx, ctx, `
		SELECT al.id FROM audit_log al
		WHERE al.tenant_id = $1 AND al.livemode = $2 AND al.test_clock_id = $3
		ORDER BY al.created_at DESC, al.id DESC
		LIMIT 50`, tenantID, false, clockID)

	if !strings.Contains(plan, "idx_audit_log_clock") {
		t.Errorf("clock-scoped read does not use the partial clock index (0148).\nPlan:\n%s", plan)
	}
	// The index carries the sort columns, so the clock slice must come back
	// pre-ordered. A Sort node here means the index no longer matches the query
	// (a reordered key, a dropped column) and deep pagination silently degrades.
	if strings.Contains(plan, "Sort") {
		t.Errorf("clock-scoped read needs a Sort — the index key no longer matches the query's ORDER BY.\nPlan:\n%s", plan)
	}
}

func explain(t *testing.T, tx *sql.Tx, ctx context.Context, q string, args ...any) string {
	t.Helper()
	rows, err := tx.QueryContext(ctx, "EXPLAIN "+q, args...)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var b strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}
