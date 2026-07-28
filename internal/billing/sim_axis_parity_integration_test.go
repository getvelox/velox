package billing_test

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/billing"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/subscription"
	"github.com/sagarsuperuser/velox/internal/tax"
	"github.com/sagarsuperuser/velox/internal/tenant"
	"github.com/sagarsuperuser/velox/internal/testclock"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/usage"
)

// TestSimAxis_ClockDrivenLifecycle_EveryAuditRowIsStamped is the PARITY proof
// for the audit sim axis (ADR-090 §5).
//
// It advances a clock through a real billing lifecycle — three monthly periods
// generated in one catchup, each finalized — against real Postgres and the real
// audit.Logger, then asserts a SET property: EVERY audit row the advance
// produced carries sim_effective_at + test_clock_id. Not "the rows we thought
// to name" — every row in the window.
//
// That shape is deliberate. A named-row assertion passes forever while a new
// emitter quietly lands unstamped rows, and an unstamped row is invisible to
// ?test_clock_id= — so the clock filter would lie BY OMISSION, which is worse
// than having no filter: an auditor reading a complete-looking timeline of a
// simulation would be reading an incomplete one. The set assertion fails the
// moment any emitter on the clock plane forgets.
//
// The rows also have to survive the thing that makes them matter: ADR-086
// teardown hard-deletes every simulated business row, so after the operator
// deletes this clock, these audit rows are the ONLY record that the invoices
// ever existed.
func TestSimAxis_ClockDrivenLifecycle_EveryAuditRowIsStamped(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Sim Axis Parity")

	logger := audit.NewLogger(db)
	customerSvc := customer.NewService(customer.NewPostgresStore(db))
	pricingStore := pricing.NewPostgresStore(db)
	pricingSvc := pricing.NewService(pricingStore)
	subStore := subscription.NewPostgresStore(db)
	usageStore := usage.NewPostgresStore(db)
	invoiceStore := invoice.NewPostgresStore(db)
	settingsStore := tenant.NewSettingsStore(db)

	engine := billing.NewEngine(
		&subStoreAdapter{subStore},
		&usageStoreAdapter{usageStore},
		&pricingStoreAdapter{pricingStore},
		&invoiceStoreAdapter{invoiceStore},
		nil, settingsStore, testPaymentSetupsNoPM{}, testChargerSentinel{}, nil,
	)
	engine.SetIntervalReader(subStore)
	engine.SetTaxProviderResolver(tax.NewResolver(nil))
	engine.SetNoPaymentMethodNotifier(&testNoPMNotifier{})
	engine.SetTxRunner(db)
	engine.SetAuditLogger(logger) // the finalize row now rides the invoice-create tx (LogInTx, ADR-090)

	// A clock frozen well away from wall-clock: sim time and real time must be
	// distinguishable, or a test that stamps wall-clock passes by accident.
	frozen := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	clockID := postgres.NewID("vlx_tclk")
	execTx(t, db, ctx, tenantID,
		`INSERT INTO test_clocks (id, tenant_id, livemode, name, frozen_time, status)
		 VALUES ($1, $2, false, 'Parity Clock', $3, 'ready')`, clockID, tenantID, frozen)

	// The customer is created INTO the clock through the real service — not
	// created bare and UPDATEd into it afterwards. This is the origin row of the
	// whole simulation, it is emitted by a NON-engine writer (so the set below is
	// not just "every row the engine wrote"), and it is the row ADR-086 teardown
	// makes irreplaceable: teardown HARD-DELETES the customer, so this row is the
	// only surviving evidence the customer was ever in this clock's world.
	customerSvc.SetAuditLogger(logger)
	customerSvc.SetTestClockChecker(testclock.NewPostgresStore(db))

	cust, err := customerSvc.Create(ctx, tenantID, customer.CreateInput{
		ExternalID: "cus_sim_parity", DisplayName: "Sim Parity", Email: "sim@example.test",
		TestClockID: clockID,
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	if cust.TestClockID != clockID {
		t.Fatalf("customer test_clock_id = %q, want %q", cust.TestClockID, clockID)
	}

	rrv, err := pricingSvc.CreateRatingRule(ctx, tenantID, pricing.CreateRatingRuleInput{
		RuleKey: "sim_calls", Name: "Sim Calls",
		Mode: domain.PricingFlat, Currency: "USD", FlatAmountCents: decimal.NewFromInt(1),
	})
	if err != nil {
		t.Fatalf("create rating rule: %v", err)
	}
	meter, err := pricingSvc.CreateMeter(ctx, tenantID, pricing.CreateMeterInput{
		Key: "sim_calls", Name: "Sim Calls", Unit: "calls",
		Aggregation: "sum", RatingRuleVersionID: rrv.ID,
	})
	if err != nil {
		t.Fatalf("create meter: %v", err)
	}
	plan, err := pricingSvc.CreatePlan(ctx, tenantID, pricing.CreatePlanInput{
		Code: "pln_sim", Name: "Sim Plan", Currency: "USD",
		BillingInterval: domain.BillingMonthly, BaseAmountCents: 5000,
		MeterIDs: []string{meter.ID},
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}

	// The sub is three monthly periods BEHIND the clock: one advance catches up
	// across all of them. Note what that means for the axis, and what it does
	// NOT: the three finalizes are all PERFORMED at the advance's instant (the
	// engine does not replay time — it stands at the new frozen_time and settles
	// what came due), so all three rows carry the SAME sim_effective_at. The
	// axis records where the clock stood, not which period the row was about;
	// the assertion below pins exactly that.
	cycleStart := frozen.AddDate(0, -3, 0)
	cycleEnd := cycleStart.AddDate(0, 1, 0)
	subID := postgres.NewID("vlx_sub")
	itemID := postgres.NewID("vlx_si")
	execTx(t, db, ctx, tenantID, `
		INSERT INTO subscriptions (
			id, tenant_id, code, display_name, customer_id, status, billing_time,
			test_clock_id, current_billing_period_start, current_billing_period_end,
			next_billing_at, created_at, updated_at
		) VALUES ($1,$2,'code-sim','Sim Sub',$3,'active','anniversary',$4,$5,$6,$6,$7,$7)`,
		subID, tenantID, cust.ID, clockID, cycleStart, cycleEnd, cycleStart)
	execTx(t, db, ctx, tenantID, `
		INSERT INTO subscription_items (id, tenant_id, subscription_id, plan_id, quantity, metadata, created_at, updated_at)
		VALUES ($1,$2,$3,$4,1,'{}'::jsonb,$5,$5)`,
		itemID, tenantID, subID, plan.ID, cycleStart)
	// Raw-SQL fixture bypasses the ADR-101 interval writer — mirror it.
	execTx(t, db, ctx, tenantID, `
		INSERT INTO billing_intervals (tenant_id, subscription_id, subscription_item_id, plan_id, quantity, starts_at, source)
		VALUES ($1,$2,$3,$4,1,$5,'create')`,
		tenantID, subID, itemID, plan.ID, cycleStart)

	// Everything below this line runs on the ctx CATCHUP binds (asserted
	// separately by testclock.TestRunCatchup_BindsClockOntoCtx): the clock, not
	// just its frozen instant. Nothing else in the lifecycle knows about it.
	simCtx := clock.WithSim(ctx, clock.Sim{At: frozen, TestClockID: clockID})

	count, errsOut := engine.RunCycleForClock(simCtx, tenantID, clockID, 100)
	if len(errsOut) > 0 {
		t.Fatalf("RunCycleForClock: %v", errsOut)
	}
	if count == 0 {
		t.Fatal("no periods billed — the parity assertion would be vacuous")
	}

	rows := readAuditRows(t, db, ctx, tenantID)
	if len(rows) == 0 {
		t.Fatal("no audit rows produced by the advance — parity assertion would be vacuous")
	}

	for _, r := range rows {
		if r.testClockID == nil {
			t.Errorf("audit row %s (%s %s) carries NO test_clock_id — it is invisible to the clock filter, and after teardown it is unattributable to any simulation",
				r.id, r.action, r.resourceType)
			continue
		}
		if *r.testClockID != clockID {
			t.Errorf("audit row %s: test_clock_id = %q, want %q", r.id, *r.testClockID, clockID)
		}
		if r.simAt == nil {
			t.Errorf("audit row %s (%s %s) has a clock but NO sim_effective_at — a half-stamped row sits in the partial index and answers sim-time queries with nothing",
				r.id, r.action, r.resourceType)
			continue
		}
		// Post contracted-instant anchoring (the #513-#523 arc), a catchup
		// row stamps ITS OWN cycle's close instant, not advance-end — a
		// Jan-cycle row says Jan 1 even when the advance lands Mar 1. The
		// parity assertion is therefore a WINDOW: every sim stamp lies
		// inside the advance's simulated span, never past the frozen
		// target and never before the simulation's first billable instant.
		if r.simAt.After(frozen) {
			t.Errorf("audit row %s: sim_effective_at = %v is PAST the clock's frozen_time %v", r.id, r.simAt, frozen)
		}
		if r.simAt.Before(cycleStart) {
			t.Errorf("audit row %s: sim_effective_at = %v predates the simulation's first cycle %v", r.id, r.simAt, cycleStart)
		}
		// created_at stays WALL-clock (ADR-030) — the sim axis is the second
		// axis, never a replacement. A row whose created_at drifted into 2027
		// would mean the operator-action timeline had been overwritten with
		// simulated time, which is the regression ADR-030 was amended to stop.
		if r.createdAt.After(time.Now().UTC().Add(time.Minute)) {
			t.Errorf("audit row %s: created_at = %v is in the future — wall-clock forensics was overwritten with sim time", r.id, r.createdAt)
		}
	}

	// The rows must survive the thing that makes them matter. ADR-086 teardown
	// HARD-DELETES the clock and its entire simulated graph — customers,
	// subscriptions, invoices, all of it. If audit_log.test_clock_id were a
	// foreign key, that delete would cascade or fail; it is deliberately a bare
	// TEXT column, so the evidence outlives its subject. After teardown these
	// rows are the ONLY record the invoices ever existed, and they must still be
	// attributable to the clock that produced them.
	before := len(rows)
	execTx(t, db, ctx, tenantID, `DELETE FROM invoice_line_items WHERE invoice_id IN (SELECT id FROM invoices WHERE customer_id = $1)`, cust.ID)
	execTx(t, db, ctx, tenantID, `DELETE FROM invoices WHERE customer_id = $1`, cust.ID)
	execTx(t, db, ctx, tenantID, `DELETE FROM subscriptions WHERE customer_id = $1`, cust.ID)
	execTx(t, db, ctx, tenantID, `DELETE FROM customers WHERE id = $1`, cust.ID)
	execTx(t, db, ctx, tenantID, `DELETE FROM test_clocks WHERE id = $1`, clockID)

	after := readAuditRows(t, db, ctx, tenantID)
	if len(after) != before {
		t.Fatalf("audit rows after teardown: got %d, want all %d — the simulation's only surviving record was deleted with it", len(after), before)
	}
	for _, r := range after {
		if r.testClockID == nil || *r.testClockID != clockID {
			t.Errorf("audit row %s lost its clock attribution in teardown (%v) — the rows survive but can no longer be filtered to the simulation that produced them", r.id, r.testClockID)
		}
	}
}

type auditRow struct {
	id           string
	action       string
	resourceType string
	createdAt    time.Time
	simAt        *time.Time
	testClockID  *string
}

func readAuditRows(t *testing.T, db *postgres.DB, ctx context.Context, tenantID string) []auditRow {
	t.Helper()
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	rs, err := tx.QueryContext(ctx,
		`SELECT id, action, resource_type, created_at, sim_effective_at, test_clock_id
		 FROM audit_log WHERE tenant_id = $1 ORDER BY created_at`, tenantID)
	if err != nil {
		t.Fatalf("read audit rows: %v", err)
	}
	defer func() { _ = rs.Close() }()
	var out []auditRow
	for rs.Next() {
		var r auditRow
		if err := rs.Scan(&r.id, &r.action, &r.resourceType, &r.createdAt, &r.simAt, &r.testClockID); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, r)
	}
	return out
}

func execTx(t *testing.T, db *postgres.DB, ctx context.Context, tenantID, q string, args ...any) {
	t.Helper()
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(ctx, q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}
