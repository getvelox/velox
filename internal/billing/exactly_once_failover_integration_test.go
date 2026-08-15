package billing_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/sagarsuperuser/velox/internal/billing"
	"github.com/sagarsuperuser/velox/internal/credit"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/doctor"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/subscription"
	"github.com/sagarsuperuser/velox/internal/tax"
	"github.com/sagarsuperuser/velox/internal/tenant"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/usage"
)

// Exactly-once under leader failure.
//
// The guarantee under test is not application logic. It is a partial unique
// index (migration 0101):
//
//	CREATE UNIQUE INDEX idx_invoices_billing_idempotency
//	  ON invoices (tenant_id, subscription_id, billing_period_start, billing_period_end)
//	  WHERE status <> 'voided' AND source_plan_changed_at IS NULL;
//
// So "we bill each period once" is enforced by Postgres, and the interesting
// question is whether a leader dying mid-run leaves the system in a state its
// successor can finish — not whether the successor can create a duplicate,
// which the database forbids outright.
//
// Two shapes are covered, because they fail differently:
//
//   - Sequential takeover (a real process SIGKILLed mid-run, successor
//     completes). Catches "the dead leader left a half-written period the
//     successor now refuses/duplicates".
//   - Concurrent leaders (N racing at once). Catches the same window with both
//     transactions genuinely in flight, which sequential takeover cannot
//     produce. A crash is one interleaving; a race is all of them.

const (
	exactlyOnceHelperEnv = "VELOX_TEST_BILLING_LEADER"
	// billedCents is the flat in-arrears base fee each seeded subscription
	// bills per period. Value is arbitrary; what matters is that it is
	// non-zero, so a duplicate would be visible as money and not just as a row.
	billedCents = 2500
)

// exactlyOnceSeed is everything both the parent and a helper subprocess need
// to bill the same tenant at the same simulated instant.
type exactlyOnceSeed struct {
	tenantID    string
	subIDs      []string
	periodStart time.Time
	periodEnd   time.Time
	// runAt is the simulated clock both sides use. Passed to the subprocess
	// as RFC3339Nano so leader and successor agree on "now" exactly — a
	// difference here would change which periods are due and make any
	// duplicate/missing count meaningless.
	runAt time.Time
}

// seedDueSubscriptions creates one tenant, one in-arrears plan, one customer,
// and n subscriptions whose period has already closed — so a billing run has
// exactly n invoices to generate and the expected end state is unambiguous.
func seedDueSubscriptions(t *testing.T, db *postgres.DB, n int) exactlyOnceSeed {
	t.Helper()
	ctx := postgres.WithLivemode(context.Background(), false)

	pricingStore := pricing.NewPostgresStore(db)
	subStore := subscription.NewPostgresStore(db)
	customerStore := customer.NewPostgresStore(db)

	tenantID := testutil.CreateTestTenant(t, db, "Failover Corp")

	periodStart := time.Date(2027, 6, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2027, 7, 1, 0, 0, 0, 0, time.UTC)

	plan, err := pricingStore.CreatePlan(ctx, tenantID, domain.Plan{
		Code: "failover-flat", Name: "Failover Flat", Currency: "USD",
		BillingInterval: domain.BillingMonthly, Status: domain.PlanActive,
		BaseAmountCents: billedCents, BaseBillTiming: domain.BillInArrears,
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}

	cust, err := customerStore.Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_failover", DisplayName: "Failover",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	subIDs := make([]string, 0, n)
	for i := 0; i < n; i++ {
		sub, err := subStore.Create(ctx, tenantID, domain.Subscription{
			Code:        fmt.Sprintf("sub-failover-%03d", i),
			DisplayName: fmt.Sprintf("Failover %03d", i),
			CustomerID:  cust.ID,
			Items:       []domain.SubscriptionItem{{PlanID: plan.ID, Quantity: 1}},
			Status:      domain.SubscriptionActive,
			BillingTime: domain.BillingTimeCalendar,
			StartedAt:   &periodStart,
			// activated_at is required for an ACTIVE sub by the doctor check
			// live_sub_cycle_stamps_complete_and_watermark_aligned. Creating
			// through the store directly skips the activation path that would
			// normally stamp it, so the fixture must — otherwise the post-crash
			// doctor sweep reports 40 violations that the crash did not cause.
			ActivatedAt:               &periodStart,
			CurrentBillingPeriodStart: &periodStart,
			CurrentBillingPeriodEnd:   &periodEnd,
		})
		if err != nil {
			t.Fatalf("create sub %d: %v", i, err)
		}
		// next_billing_at = periodEnd makes the sub due the moment the clock
		// passes the period close.
		if err := subStore.UpdateBillingCycle(ctx, tenantID, sub.ID, periodStart, periodEnd, periodEnd, 1); err != nil {
			t.Fatalf("billing cycle %d: %v", i, err)
		}
		subIDs = append(subIDs, sub.ID)
	}

	return exactlyOnceSeed{
		tenantID:    tenantID,
		subIDs:      subIDs,
		periodStart: periodStart,
		periodEnd:   periodEnd,
		// One hour past close: every seeded sub is due for exactly one period.
		// Deliberately NOT far past — a larger gap would make subs due for
		// several periods and turn "one invoice per sub" into a moving target.
		runAt: periodEnd.Add(time.Hour),
	}
}

// newFailoverEngine builds an engine wired to real Postgres stores at a fixed
// simulated instant. Used identically by the parent and by the helper
// subprocess, so both leaders are the same code driving the same path.
func newFailoverEngine(db *postgres.DB, now time.Time) *billing.Engine {
	pricingStore := pricing.NewPostgresStore(db)
	subStore := subscription.NewPostgresStore(db)
	invoiceStore := invoice.NewPostgresStore(db)
	usageStore := usage.NewPostgresStore(db)
	creditStore := credit.NewPostgresStore(db)
	settingsStore := tenant.NewSettingsStore(db)

	e := billing.NewEngine(
		&subStoreAdapter{subStore}, &usageStoreAdapter{usageStore},
		&pricingStoreAdapter{pricingStore}, &invoiceStoreAdapter{invoiceStore},
		credit.NewService(creditStore), settingsStore,
		testPaymentSetupsNoPM{}, testChargerSentinel{},
		clock.NewFake(now),
	)
	e.SetIntervalReader(subStore)
	e.SetTaxProviderResolver(tax.NewResolver(nil))
	e.SetNoPaymentMethodNotifier(&testNoPMNotifier{})
	e.SetDunningResolver(&testDunningResolver{})
	return e
}

// cycleInvoiceCounts reports, for the seeded tenant, how many non-voided cycle
// invoices exist in total and how many distinct (subscription, period) tuples
// they cover. Exactly-once means these two numbers are equal AND equal to the
// number of subscriptions.
func cycleInvoiceCounts(t *testing.T, db *postgres.DB, tenantID string) (total, distinct int, totalCents int64) {
	t.Helper()
	ctx := postgres.WithLivemode(context.Background(), false)
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin read tx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COUNT(DISTINCT (subscription_id, billing_period_start, billing_period_end)),
		       COALESCE(SUM(total_amount_cents), 0)
		FROM invoices
		WHERE tenant_id = $1
		  AND status <> 'voided'
		  AND source_plan_changed_at IS NULL
		  AND subscription_id IS NOT NULL`, tenantID,
	).Scan(&total, &distinct, &totalCents); err != nil {
		t.Fatalf("count invoices: %v", err)
	}
	return total, distinct, totalCents
}

// TestExactlyOnce_LeaderKilledMidRun is the literal Track A scenario applied to
// money: a real billing leader is SIGKILLed partway through generating
// invoices, a successor finishes the run, and every subscription must end with
// exactly one invoice for the closed period — no duplicate, none lost.
//
// The kill point is chosen by watching the database rather than by sleeping:
// the parent polls until the leader has committed killAfter invoices, then
// kills it. That puts the kill inside the run every time instead of racing a
// timer, and makes the interesting boundary (killed immediately after a commit,
// before the loop advances) reachable on purpose.
func TestExactlyOnce_LeaderKilledMidRun(t *testing.T) {
	// Sweep the kill point rather than picking one. A single kill position can
	// pass by luck — the boundaries are where recovery breaks, so 1 (killed
	// immediately after the very first commit) and subCount-1 (killed with one
	// subscription left) are included deliberately, not just a comfortable
	// middle.
	for _, killAfter := range []int{1, 5, 12, 25, 39} {
		t.Run(fmt.Sprintf("killed_after_%d_invoices", killAfter), func(t *testing.T) {
			runKilledLeaderTrial(t, killAfter)
		})
	}
}

func runKilledLeaderTrial(t *testing.T, killAfter int) {
	db := testutil.SetupTestDB(t)
	const subCount = 40

	seed := seedDueSubscriptions(t, db, subCount)

	leader := exec.Command(os.Args[0], "-test.run=TestBillingLeaderHelperProcess")
	leader.Env = append(os.Environ(),
		exactlyOnceHelperEnv+"=1",
		"VELOX_TEST_TENANT_ID="+seed.tenantID,
		"VELOX_TEST_RUN_AT="+seed.runAt.Format(time.RFC3339Nano),
	)
	leader.Stderr = os.Stderr
	if err := leader.Start(); err != nil {
		t.Fatalf("start billing leader: %v", err)
	}
	defer func() { _ = leader.Process.Kill(); _, _ = leader.Process.Wait() }()

	// Wait until the leader has actually committed partial work.
	deadline := time.Now().Add(60 * time.Second)
	var committedBeforeKill int
	for {
		total, _, _ := cycleInvoiceCounts(t, db, seed.tenantID)
		if total >= killAfter {
			committedBeforeKill = total
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("leader only committed %d invoices in 60s; expected to reach %d", total, killAfter)
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := leader.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL billing leader: %v", err)
	}
	state, err := leader.Process.Wait()
	if err != nil {
		t.Fatalf("wait for killed leader: %v", err)
	}
	ws, ok := state.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("leader did not die of SIGKILL (state=%v) — the test would be measuring "+
			"a clean shutdown, which proves nothing about crash recovery", state)
	}

	midTotal, midDistinct, _ := cycleInvoiceCounts(t, db, seed.tenantID)
	if midTotal != midDistinct {
		t.Fatalf("the dead leader alone left %d invoices over %d distinct periods — it duplicated "+
			"before anyone took over", midTotal, midDistinct)
	}
	if midTotal >= subCount {
		t.Skipf("leader finished all %d subscriptions before the kill landed; nothing was "+
			"interrupted, so this run proves nothing (raise subCount or lower killAfter)", subCount)
	}
	t.Logf("leader SIGKILLed after committing %d/%d invoices (observed %d at kill time)",
		midTotal, subCount, committedBeforeKill)

	// Successor picks up.
	ctx := postgres.WithLivemode(context.Background(), false)
	generated, failures := newFailoverEngine(db, seed.runAt).RunCycleForTenant(ctx, seed.tenantID, 50)
	for _, f := range failures {
		t.Errorf("successor run failure sub=%s: %v", f.SubscriptionID, f.Err)
	}

	total, distinct, totalCents := cycleInvoiceCounts(t, db, seed.tenantID)
	t.Logf("successor generated %d; final: %d invoices over %d distinct (sub,period) tuples, %d cents",
		generated, total, distinct, totalCents)

	if total != distinct {
		t.Fatalf("DUPLICATE BILLING: %d invoices covering only %d distinct (sub,period) tuples",
			total, distinct)
	}
	if total != subCount {
		t.Fatalf("expected exactly %d invoices (one per subscription), got %d — %d subscriptions "+
			"were never billed after the leader died", subCount, total, subCount-total)
	}
	if want := int64(subCount) * billedCents; totalCents != want {
		t.Fatalf("billed %d cents, expected %d", totalCents, want)
	}

	assertDoctorClean(t)
}

// assertDoctorClean runs all 27 money-invariant checks after a failover.
//
// The invoice counts above prove nothing was double-billed or lost. They say
// nothing about whether the crash left some OTHER invariant broken — a line
// item orphaned from its invoice, a ledger entry without its counterpart, a
// subscription whose period bounds no longer agree with its invoices. That is
// what the doctor is for, and a crash is exactly the event most likely to
// produce one.
//
// Deliberately uses the ADMIN pool. The checks are raw SELECTs with no tenant
// predicate, so under an RLS-scoped app role they return zero rows and the
// sweep passes without having looked at anything — a green result that means
// the opposite of what it appears to.
func assertDoctorClean(t *testing.T) {
	t.Helper()
	res := doctor.Run(context.Background(), testutil.AdminPool(t), doctor.Checks)
	for _, err := range res.Errors {
		t.Errorf("doctor check failed to RUN (a check that cannot run is itself a finding): %v", err)
	}
	for _, v := range res.Violations {
		t.Errorf("doctor violation after failover [%s/%s] row=%s %s — %s",
			v.Check.Domain, v.Check.Name, v.RowID, v.Detail, v.Check.Why)
	}
	t.Logf("doctor: %d checks, %d violations, %d errors, %v",
		res.Checks, len(res.Violations), len(res.Errors), res.Duration.Round(time.Millisecond))
}

// TestExactlyOnce_ConcurrentLeaders is the stronger sibling: instead of one
// leader dying and another taking over, N leaders bill the same due set at the
// same time.
//
// This exists because sequential takeover only ever exercises one interleaving
// — whatever the dead leader happened to finish. Racing leaders put two
// transactions inside the same (sub, period) window simultaneously, which is
// the state a crash can produce but a sequential test cannot construct on
// purpose. If the unique index is the real guarantee, this passes; if
// exactly-once actually rested on read-then-write application logic, this is
// where it breaks.
func TestExactlyOnce_ConcurrentLeaders(t *testing.T) {
	db := testutil.SetupTestDB(t)
	const (
		subCount    = 40
		leaderCount = 4
	)

	seed := seedDueSubscriptions(t, db, subCount)
	ctx := postgres.WithLivemode(context.Background(), false)

	var wg sync.WaitGroup
	generated := make([]int, leaderCount)
	failed := make([]int, leaderCount)
	firstErr := make([]error, leaderCount)
	start := make(chan struct{})
	for i := 0; i < leaderCount; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			eng := newFailoverEngine(db, seed.runAt)
			<-start // release all leaders together
			// batchSize 1 is what actually creates the race. With a large
			// batch the first leader to arrive claims and bills every due
			// subscription before the others finish starting, so they find
			// nothing due and the test passes without ever colliding. One
			// subscription per fetch makes all leaders contend on the same
			// row, which is the state a crashed-and-restarted leader produces.
			n, errs := eng.RunCycleForTenant(ctx, seed.tenantID, 1)
			generated[idx], failed[idx] = n, len(errs)
			if len(errs) > 0 {
				firstErr[idx] = errs[0].Err
			}
		}(i)
	}
	close(start)
	wg.Wait()

	total, distinct, totalCents := cycleInvoiceCounts(t, db, seed.tenantID)
	t.Logf("%d concurrent leaders: generated=%v failures=%v; final: %d invoices over %d "+
		"distinct (sub,period) tuples, %d cents", leaderCount, generated, failed, total, distinct, totalCents)

	// Prove the race actually happened. If only one leader did any work, the
	// others simply arrived late and this run tested nothing about collision —
	// a green result would be meaningless.
	workers := 0
	for _, n := range generated {
		if n > 0 {
			workers++
		}
	}
	if workers < 2 {
		t.Fatalf("only %d leader(s) generated anything (%v) — no collision occurred, so this "+
			"run does not exercise concurrent billing at all", workers, generated)
	}
	for i, e := range firstErr {
		if e != nil {
			t.Errorf("leader %d reported a run failure during contention: %v", i, e)
		}
	}

	if total != distinct {
		t.Fatalf("DUPLICATE BILLING under %d concurrent leaders: %d invoices covering only %d "+
			"distinct (sub,period) tuples", leaderCount, total, distinct)
	}
	if total != subCount {
		t.Fatalf("expected exactly %d invoices, got %d", subCount, total)
	}
	if want := int64(subCount) * billedCents; totalCents != want {
		t.Fatalf("billed %d cents, expected %d", totalCents, want)
	}

	assertDoctorClean(t)
}

// TestBillingLeaderHelperProcess is not a test. It is the billing leader that
// TestExactlyOnce_LeaderKilledMidRun kills mid-run: with the guard env var set
// it opens its own pool (never testutil.SetupTestDB, which truncates), builds
// the same engine the parent builds, and bills the seeded tenant until it is
// killed. Without the env var it returns immediately, so a normal run skips it.
func TestBillingLeaderHelperProcess(t *testing.T) {
	if os.Getenv(exactlyOnceHelperEnv) != "1" {
		return
	}
	tenantID := os.Getenv("VELOX_TEST_TENANT_ID")
	runAt, err := time.Parse(time.RFC3339Nano, os.Getenv("VELOX_TEST_RUN_AT"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "helper: parse run-at: %v\n", err)
		return
	}

	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://velox_test_app:velox_test_app@localhost:5432/velox_test?sslmode=disable"
	}
	pool, err := sql.Open("pgx", dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "helper: open pool: %v\n", err)
		return
	}
	pool.SetMaxOpenConns(4)
	db := postgres.NewDB(pool, 30*time.Second)

	ctx := postgres.WithLivemode(context.Background(), false)
	// Batch of 1 so the leader commits one invoice at a time and the parent's
	// database poll can land the kill mid-run rather than after a bulk commit.
	n, failures := newFailoverEngine(db, runAt).RunCycleForTenant(ctx, tenantID, 1)
	fmt.Fprintf(os.Stderr, "helper: completed without being killed (generated=%d failures=%d)\n",
		n, len(failures))
}
