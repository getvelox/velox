package billing_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/billing"
	"github.com/sagarsuperuser/velox/internal/doctor"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/leader"
	"github.com/sagarsuperuser/velox/internal/platform/leader/leadertest"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/subscription"
	"github.com/sagarsuperuser/velox/internal/tax"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/usage"
)

// ADR-115 collision tests: the threshold fire and the cycle close (and the
// cancel, and the plan swap) are period writers that used to have disjoint
// idempotency seams and no shared row lock. Every schedule below is driven
// deterministically through the concurrent-resolver hooks on the
// failableSubAdapter (playbook §5.3) — the competing writer commits between
// the engine's read and its write — except the barrier test, which races for
// real. Time is bound per writer with clock.WithEffectiveNow on the ctx (the
// engine's Real clock and every store's clock.Now read it): the fire runs
// at t1 inside the period, the operator close at P1+1ns.
//
// Fixture geometry (newThresholdFixture): period [P0, P1) = [now-72h, +30d),
// 1c/call, no base fee, 100 usage events at P0+i minutes (i < 100) × qty 10
// = 1000c. t1 = P0+30min splits them: 300c before the fire, 700c after.

// periodWriter is one engine over the fixture's stores with its own
// fault/hook adapters, so the fire and the close (or a cancel, or a swap) can
// carry independent hooks.
type periodWriter struct {
	engine *billing.Engine
	subs   *failableSubAdapter
	invs   *failableInvoiceAdapter
}

func newPeriodWriter(f *thresholdFixture) *periodWriter {
	subs := &failableSubAdapter{subStoreAdapter: &subStoreAdapter{f.subStore}}
	invs := &failableInvoiceAdapter{invoiceStoreAdapter: &invoiceStoreAdapter{f.invStore}}
	engine := billing.NewEngine(
		subs,
		&usageStoreAdapter{usage.NewPostgresStore(f.db)},
		&pricingStoreAdapter{pricing.NewPostgresStore(f.db)},
		invs,
		nil, f.settings, testPaymentSetupsNoPM{}, testChargerSentinel{}, nil,
	)
	engine.SetIntervalReader(f.subStore)
	engine.SetTaxProviderResolver(tax.NewResolver(nil))
	engine.SetNoPaymentMethodNotifier(&testNoPMNotifier{})
	return &periodWriter{engine: engine, subs: subs, invs: invs}
}

// s1Rig is a fixture plus the instants and writers every ADR-115 test uses.
type s1Rig struct {
	f      *thresholdFixture
	base   context.Context // livemode-bound, 120 s timeout
	t1     time.Time       // the fire's instant: P0+30min (300c of usage before it)
	tc     time.Time       // a mid-period cancel instant: P0+40min (400c before it)
	close  time.Time       // the operator close's instant: P1+1ns
	fire   *periodWriter
	closer *periodWriter
}

func newS1Rig(t *testing.T, name string, amountGTE int64, reset bool) *s1Rig {
	t.Helper()
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	f := newThresholdFixture(t, name)
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 120*time.Second)
	t.Cleanup(cancel)
	f.ingestUsage(t, ctx, 100, 10)
	if _, err := f.subSvc.SetBillingThresholds(ctx, f.tenantID, f.subID, subscription.BillingThresholdsInput{
		AmountGTE: amountGTE, ResetBillingCycle: &reset,
	}); err != nil {
		t.Fatalf("set threshold: %v", err)
	}
	return &s1Rig{
		f: f, base: ctx,
		t1:    f.cycleStart.Add(30 * time.Minute),
		tc:    f.cycleStart.Add(40 * time.Minute),
		close: f.cycleEnd.Add(time.Nanosecond),
		fire:  newPeriodWriter(f), closer: newPeriodWriter(f),
	}
}

func (r *s1Rig) at(t time.Time) context.Context { return clock.WithEffectiveNow(r.base, t) }

// runClose is the operator path: POST /v1/billing/run's engine call, unfenced
// (ADR-114 §97), synchronous, drains the tenant.
func (r *s1Rig) runClose(t *testing.T) int {
	t.Helper()
	gen, failures := r.closer.engine.RunCycleForTenant(r.at(r.close), r.f.tenantID, 50)
	if len(failures) != 0 {
		t.Fatalf("operator close: %v", failures)
	}
	return gen
}

func (r *s1Rig) runFire(t *testing.T) int {
	t.Helper()
	fired, errs := r.fire.engine.ScanThresholds(r.at(r.t1), 50)
	if len(errs) != 0 {
		t.Fatalf("threshold scan: %v", errs)
	}
	return fired
}

// liveInvoices returns the tenant's non-voided invoices split by reason.
func (r *s1Rig) liveInvoices(t *testing.T) (cycle, threshold, other []domain.Invoice) {
	t.Helper()
	for _, inv := range r.f.listInvoices(t, r.base) {
		if inv.Status == domain.InvoiceVoided {
			continue
		}
		switch inv.BillingReason {
		case domain.BillingReasonSubscriptionCycle:
			cycle = append(cycle, inv)
		case domain.BillingReasonThreshold:
			threshold = append(threshold, inv)
		default:
			other = append(other, inv)
		}
	}
	return cycle, threshold, other
}

// usageLines returns an invoice's usage lines.
func (r *s1Rig) usageLines(t *testing.T, invID string) []domain.InvoiceLineItem {
	t.Helper()
	lines, err := r.f.invStore.ListLineItems(r.base, r.f.tenantID, invID)
	if err != nil {
		t.Fatalf("list lines %s: %v", invID, err)
	}
	var out []domain.InvoiceLineItem
	for _, li := range lines {
		if li.LineType == domain.LineTypeUsage {
			out = append(out, li)
		}
	}
	return out
}

func (r *s1Rig) sub(t *testing.T) domain.Subscription {
	t.Helper()
	sub, err := r.f.subStore.Get(r.base, r.f.tenantID, r.f.subID)
	if err != nil {
		t.Fatalf("get sub: %v", err)
	}
	return sub
}

// assertPeriod checks the sub's period start and that next_billing_at ==
// current_billing_period_end (the live-sub invariant the doctor also holds).
func (r *s1Rig) assertPeriod(t *testing.T, wantStart time.Time, wantNext *time.Time) domain.Subscription {
	t.Helper()
	sub := r.sub(t)
	if sub.CurrentBillingPeriodStart == nil || !sub.CurrentBillingPeriodStart.Equal(wantStart) {
		t.Fatalf("period start = %v, want %v", sub.CurrentBillingPeriodStart, wantStart)
	}
	if sub.NextBillingAt == nil || sub.CurrentBillingPeriodEnd == nil || !sub.NextBillingAt.Equal(*sub.CurrentBillingPeriodEnd) {
		t.Fatalf("next_billing_at %v != period end %v", sub.NextBillingAt, sub.CurrentBillingPeriodEnd)
	}
	if wantNext != nil && !sub.NextBillingAt.Equal(*wantNext) {
		t.Fatalf("next_billing_at = %v, want %v", sub.NextBillingAt, *wantNext)
	}
	return sub
}

// assertDoctorClean runs the read-side twin of the CAS: no usage instant of
// one (subscription, meter, bucket) on two live invoices.
func assertUsageBilledOnce(t *testing.T, ctx context.Context) {
	t.Helper()
	res := doctor.Run(ctx, testutil.AdminPool(t), doctor.Checks)
	for _, e := range res.Errors {
		t.Errorf("doctor check failed to run: %v", e)
	}
	for _, v := range res.Violations {
		if v.Check.Name == "usage_billed_once_per_subscription_meter_window" {
			t.Errorf("doctor: usage billed twice: %s %s", v.RowID, v.Detail)
		}
	}
}

// ---------------------------------------------------------------------------
// T2 — S1-A / S2: operator close commits while a reset=true fire is in flight.
// ---------------------------------------------------------------------------

// TestThresholdFire_vs_OperatorClose_CloseFirst_ResetTrue: the leader's
// threshold-reset fire has read its page, evaluated and taxed; before its tx
// opens, an operator POST /v1/billing/run closes the period. The fire's CAS
// (expected next = P1) finds P2 → 0 rows → nothing written, no rewind. Both
// writers hold the SAME live billing lease: the lease is not what closes this
// (ADR-114 §97 keeps the operator path unfenced); the row CAS is.
//
// Mutation-verify: make ClosePeriodTx's WHERE id-only (each predicate `OR
// TRUE`) → the fire re-anchors after the close and T lands beside I1.
func TestThresholdFire_vs_OperatorClose_CloseFirst_ResetTrue(t *testing.T) {
	r := newS1Rig(t, "S1 close-first reset", 200, true)
	r.base = leadertest.Token(t, testutil.AdminPool(t), r.base, leader.RoleBilling)

	var closeGen int
	r.fire.subs.beforeTx = func() {
		r.fire.subs.beforeTx = nil
		closeGen = r.runClose(t)
	}
	if fired := r.runFire(t); fired != 0 {
		t.Fatalf("fire reported %d after losing the CAS, want 0", fired)
	}
	if closeGen != 1 {
		t.Fatalf("operator close generated %d, want 1", closeGen)
	}

	cycle, threshold, other := r.liveInvoices(t)
	if len(cycle) != 1 || len(threshold) != 0 || len(other) != 0 {
		t.Fatalf("invoices: cycle=%d threshold=%d other=%d, want exactly one cycle invoice", len(cycle), len(threshold), len(other))
	}
	if !cycle[0].BillingPeriodStart.Equal(r.f.cycleStart) || !cycle[0].BillingPeriodEnd.Equal(r.f.cycleEnd) {
		t.Fatalf("cycle invoice period [%v, %v), want [%v, %v)", cycle[0].BillingPeriodStart, cycle[0].BillingPeriodEnd, r.f.cycleStart, r.f.cycleEnd)
	}
	if cycle[0].SubtotalCents != 1000 {
		t.Fatalf("cycle invoice subtotal = %d, want 1000 (every usage instant once)", cycle[0].SubtotalCents)
	}
	r.assertPeriod(t, r.f.cycleEnd, nil) // [P1, P2): the close's advance stands
	assertUsageBilledOnce(t, r.base)
}

// ---------------------------------------------------------------------------
// T2b — barrier form: both writers finish pre-tx work, then race for real.
// ---------------------------------------------------------------------------

// TestCloseVsThresholdFire_BothOrders_BillsUsageOnce races the reset=true fire
// and the operator close from a rendezvous inside both WithTenantTx wrappers,
// 20 iterations on fresh fixtures, with a per-iteration bias so BOTH lock
// orders are observed. Whoever takes the row lock second loses its CAS: one
// invoice, no usage instant billed twice, next_billing_at == period end.
//
// Mutation-verify: WHERE id-only CAS → in the close-first order the fire
// re-anchors after the close and bills [P0, t1) again (two invoices, doctor
// overlap).
func TestCloseVsThresholdFire_BothOrders_BillsUsageOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	const iterations = 20
	var fireWon, closeWon int
	for i := range iterations {
		r := newS1Rig(t, fmt.Sprintf("S1 barrier %d", i), 200, true)

		var arrive sync.WaitGroup
		arrive.Add(2)
		rendezvous := func(delay bool) func() {
			return func() {
				arrive.Done()
				arrive.Wait() // both writers are past their reads, tax and line build
				if delay {
					time.Sleep(3 * time.Millisecond) // bias the lock order this iteration
				}
			}
		}
		r.fire.subs.beforeTx = rendezvous(i%2 == 0)
		r.closer.subs.beforeTx = rendezvous(i%2 == 1)

		var fired, closeGen int
		var wg sync.WaitGroup
		wg.Go(func() { fired = r.runFire(t) })
		wg.Go(func() { closeGen = r.runClose(t) })
		wg.Wait()

		cycle, threshold, other := r.liveInvoices(t)
		total := len(cycle) + len(threshold) + len(other)
		switch {
		case fired == 1 && closeGen == 0 && len(threshold) == 1 && total == 1:
			fireWon++
			// The reset re-anchored to [t1, t1+1mo): the close re-read and was caught up.
			r.assertPeriod(t, r.t1, nil)
		case fired == 0 && closeGen == 1 && len(cycle) == 1 && total == 1:
			closeWon++
			r.assertPeriod(t, r.f.cycleEnd, nil)
		default:
			t.Fatalf("iteration %d: fired=%d closeGen=%d cycle=%d threshold=%d other=%d — want exactly one writer, one invoice", i, fired, closeGen, len(cycle), len(threshold), len(other))
		}
		var usageCents int64
		for _, inv := range append(cycle, threshold...) {
			for _, li := range r.usageLines(t, inv.ID) {
				usageCents += li.AmountCents
			}
		}
		if usageCents != 1000 && usageCents != 300 {
			t.Fatalf("iteration %d: usage billed = %dc, want 1000 (close won) or 300 (fire won: [P0,t1) only)", i, usageCents)
		}
		assertUsageBilledOnce(t, r.base)
	}
	t.Logf("lock orders observed: fire-first=%d close-first=%d", fireWon, closeWon)
	if fireWon == 0 || closeWon == 0 {
		t.Fatalf("both lock orders must be observed: fire-first=%d close-first=%d", fireWon, closeWon)
	}
}

// ---------------------------------------------------------------------------
// T3 — S1-B: reset=true fire commits while the close is in flight.
// ---------------------------------------------------------------------------

// TestThresholdFire_vs_Close_FireFirst_ResetTrue: the close has read the sub
// and built [P0, P1) from the full usage; before its tx opens, the reset=true
// fire commits T [P0, t1) and re-anchors to [t1, t1+1mo). The close's CAS
// (expected next = P1) finds t1+1mo → nothing written; the retry re-reads,
// is caught up, and RunCycleForTenant reports 0 with no error. The pre-fix
// shape committed I1 for [P0, P1) beside T and then rewound the watermark.
//
// Mutation-verify: this schedule is caught by BOTH the CAS and the in-tx
// watermark re-read, so a single guard removed is not enough — WHERE id-only
// CAS + `sameInstant(...) || true` together → I1 lands beside T.
func TestThresholdFire_vs_Close_FireFirst_ResetTrue(t *testing.T) {
	r := newS1Rig(t, "S1 fire-first reset", 200, true)

	var fired int
	r.closer.subs.beforeTx = func() {
		r.closer.subs.beforeTx = nil
		fired = r.runFire(t)
	}
	if gen := r.runClose(t); gen != 0 {
		t.Fatalf("close generated %d after the fire re-anchored, want 0", gen)
	}
	if fired != 1 {
		t.Fatalf("fire reported %d, want 1", fired)
	}

	cycle, threshold, other := r.liveInvoices(t)
	if len(threshold) != 1 || len(cycle) != 0 || len(other) != 0 {
		t.Fatalf("invoices: cycle=%d threshold=%d other=%d, want only the threshold invoice", len(cycle), len(threshold), len(other))
	}
	if threshold[0].SubtotalCents != 300 || !threshold[0].BillingPeriodEnd.Equal(r.t1) {
		t.Fatalf("threshold invoice = %dc through %v, want 300c through %v", threshold[0].SubtotalCents, threshold[0].BillingPeriodEnd, r.t1)
	}
	r.assertPeriod(t, r.t1, nil) // [t1, t1+1mo): the fire's re-anchor stands
	assertUsageBilledOnce(t, r.base)
}

// ---------------------------------------------------------------------------
// T4a — S1-C: reset=false fire commits while the close is in flight.
// ---------------------------------------------------------------------------

// TestThresholdFire_vs_Close_ResetFalse_FireFirst: the reset=false fire does
// NOT move the period, so the close's CAS passes — the in-tx watermark
// re-read is what catches it: the close built its lines against "no fire",
// the tx now sees T through t1, so it rolls back; the retry rebuilds with the
// watermark and bills only the residual [t1, P1).
//
// Mutation-verify: `sameInstant(current, builtAgainst) || true` in
// commitPeriodClose → the close commits its full-period lines: the usage
// line starts at P0 and 1300c is billed for 1000c of usage.
func TestThresholdFire_vs_Close_ResetFalse_FireFirst(t *testing.T) {
	r := newS1Rig(t, "S1 fire-first keep-anchor", 200, false)

	r.closer.subs.beforeTx = func() {
		r.closer.subs.beforeTx = nil
		if fired := r.runFire(t); fired != 1 {
			t.Fatalf("fire reported %d, want 1", fired)
		}
	}
	if gen := r.runClose(t); gen != 1 {
		t.Fatalf("close generated %d, want 1 (the residual)", gen)
	}

	cycle, threshold, other := r.liveInvoices(t)
	if len(cycle) != 1 || len(threshold) != 1 || len(other) != 0 {
		t.Fatalf("invoices: cycle=%d threshold=%d other=%d, want one of each", len(cycle), len(threshold), len(other))
	}
	if !cycle[0].BillingPeriodStart.Equal(r.f.cycleStart) || !cycle[0].BillingPeriodEnd.Equal(r.f.cycleEnd) {
		t.Fatalf("cycle invoice header [%v, %v), want [P0, P1)", cycle[0].BillingPeriodStart, cycle[0].BillingPeriodEnd)
	}
	lines := r.usageLines(t, cycle[0].ID)
	if len(lines) != 1 || lines[0].BillingPeriodStart == nil || !lines[0].BillingPeriodStart.Equal(r.t1) || lines[0].AmountCents != 700 {
		t.Fatalf("cycle usage lines = %+v, want one residual line [t1, P1) = 700c", lines)
	}
	if threshold[0].SubtotalCents != 300 {
		t.Fatalf("threshold invoice = %dc, want 300", threshold[0].SubtotalCents)
	}
	r.assertPeriod(t, r.f.cycleEnd, nil)
	assertUsageBilledOnce(t, r.base)
}

// ---------------------------------------------------------------------------
// T4b — S1-C, close first: the verify-only CAS of a reset=false fire.
// ---------------------------------------------------------------------------

// TestThresholdFire_vs_Close_ResetFalse_CloseFirst: the reset=false fire
// writes the row verify-only (same values) — that UPDATE is what takes the
// lock and proves the snapshot. The operator close commits first; the fire's
// verify-only CAS finds next = P2 → nothing written. Pre-fix the reset=false
// arm never touched the row and T [P0, t1) landed after I1 [P0, P1).
//
// Mutation-verify: ignore the verify-only CAS result in fireThreshold
// (`err != nil && false`) → T lands after I1.
func TestThresholdFire_vs_Close_ResetFalse_CloseFirst(t *testing.T) {
	r := newS1Rig(t, "S1 close-first keep-anchor", 200, false)

	r.fire.subs.beforeTx = func() {
		r.fire.subs.beforeTx = nil
		if gen := r.runClose(t); gen != 1 {
			t.Fatalf("operator close generated %d, want 1", gen)
		}
	}
	if fired := r.runFire(t); fired != 0 {
		t.Fatalf("fire reported %d after the close, want 0", fired)
	}

	cycle, threshold, other := r.liveInvoices(t)
	if len(cycle) != 1 || len(threshold) != 0 || len(other) != 0 {
		t.Fatalf("invoices: cycle=%d threshold=%d other=%d, want only the full cycle invoice", len(cycle), len(threshold), len(other))
	}
	if cycle[0].SubtotalCents != 1000 {
		t.Fatalf("cycle invoice = %dc, want 1000", cycle[0].SubtotalCents)
	}
	r.assertPeriod(t, r.f.cycleEnd, nil)
	assertUsageBilledOnce(t, r.base)
}

// ---------------------------------------------------------------------------
// T5 — S3: immediate cancel holds the row while the fire arrives.
// ---------------------------------------------------------------------------

// waitForLockWaiter polls pg_stat_activity until one backend is waiting on a
// row lock in a statement matching `like` — the fire's CAS blocked behind the
// cancel's row lock.
func waitForLockWaiter(t *testing.T, ctx context.Context, admin *sql.DB, like string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := admin.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND query ILIKE $1`, "%"+like+"%").Scan(&n); err != nil {
			t.Fatalf("pg_stat_activity: %v", err)
		}
		if n >= 1 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no backend blocked on a row lock in %q within 15s — the fire never reached its CAS", like)
}

// TestImmediateCancel_vs_ThresholdFire_Concurrent: the cancel's transitionInTx
// X-locks the row from its first statement; the fire's CAS blocks behind it,
// then re-evaluates against the committed row (status=canceled) → 0 rows. The
// final-on-cancel invoice F bills [P0, tc); no T; the canceled sub keeps its
// ORIGINAL period (a reset=true fire used to re-anchor a canceled sub, a
// reset=false fire used to land T beside F).
//
// Mutation-verify: `status = $7 OR TRUE` → reset=true re-anchors the
// canceled sub; reset=false lands T on F.
func TestImmediateCancel_vs_ThresholdFire_Concurrent(t *testing.T) {
	for _, reset := range []bool{true, false} {
		t.Run(fmt.Sprintf("reset=%v", reset), func(t *testing.T) {
			r := newS1Rig(t, fmt.Sprintf("S1 cancel-vs-fire reset=%v", reset), 200, reset)
			admin := testutil.AdminPool(t)

			type fireOut struct {
				fired int
				errs  []error
			}
			done := make(chan fireOut, 1)
			ctxCancel := r.at(r.tc)
			if _, err := r.f.subStore.CancelAtomicWithBill(ctxCancel, r.f.tenantID, r.f.subID, func(tx *sql.Tx, canceled domain.Subscription) error {
				go func() {
					fired, errs := r.fire.engine.ScanThresholds(r.at(r.t1), 50)
					done <- fireOut{fired, errs}
				}()
				waitForLockWaiter(t, r.base, admin, "UPDATE subscriptions")
				_, err := r.closer.engine.BillFinalOnImmediateCancelTx(ctxCancel, tx, canceled)
				return err
			}); err != nil {
				t.Fatalf("cancel: %v", err)
			}
			out := <-done
			if out.fired != 0 || len(out.errs) != 0 {
				t.Fatalf("fire after the cancel committed: fired=%d errs=%v, want 0 and no error", out.fired, out.errs)
			}

			cycle, threshold, other := r.liveInvoices(t)
			if len(other) != 1 || other[0].BillingReason != domain.BillingReasonSubscriptionCancel || len(threshold) != 0 || len(cycle) != 0 {
				t.Fatalf("invoices: cycle=%d threshold=%d other=%d (%v), want only the final-on-cancel invoice", len(cycle), len(threshold), len(other), other)
			}
			if other[0].SubtotalCents != 400 {
				t.Fatalf("final invoice = %dc, want 400 (usage before the cancel instant)", other[0].SubtotalCents)
			}
			sub := r.sub(t)
			if sub.Status != domain.SubscriptionCanceled || !sub.CurrentBillingPeriodStart.Equal(r.f.cycleStart) || sub.NextBillingAt == nil || !sub.NextBillingAt.Equal(r.f.cycleEnd) {
				t.Fatalf("canceled sub period must be untouched: status=%s [%v, next %v)", sub.Status, sub.CurrentBillingPeriodStart, sub.NextBillingAt)
			}
			assertUsageBilledOnce(t, r.base)
		})
	}
}

// ---------------------------------------------------------------------------
// T6 — S4 / S5 / S9a: plan swap vs close and vs fire; the fire-vs-swap residual.
// ---------------------------------------------------------------------------

// hookedPlanReader serves the swap's two plans and runs a hook on the SECOND
// GetPlan — the instant after UpdateItemTx's Get of the sub and before the
// swap's first tx statement, where a competing writer can commit.
type hookedPlanReader struct {
	plans    map[string]domain.Plan
	calls    int
	onSecond func()
}

func (r *hookedPlanReader) GetPlan(_ context.Context, _, id string) (domain.Plan, error) {
	r.calls++
	if r.calls == 2 && r.onSecond != nil {
		h := r.onSecond
		r.onSecond = nil
		h()
	}
	p, ok := r.plans[id]
	if !ok {
		return domain.Plan{}, errors.New("plan not found")
	}
	return p, nil
}

// swapRig wires a subscription.Service for cross-interval swaps on the
// fixture sub: the fixture's monthly plan → a yearly plan of the same cadence.
type swapRig struct {
	svc     *subscription.Service
	plans   *hookedPlanReader
	newPlan domain.Plan
	itemID  string
}

func newSwapRig(t *testing.T, r *s1Rig) *swapRig {
	t.Helper()
	ps := pricing.NewPostgresStore(r.f.db)
	oldPlan, err := ps.GetPlan(r.base, r.f.tenantID, r.f.planID)
	if err != nil {
		t.Fatalf("get plan: %v", err)
	}
	newPlan, err := r.f.pricingSvc.CreatePlan(r.base, r.f.tenantID, pricing.CreatePlanInput{
		Code: "pln_thresh_yearly", Name: "Threshold Plan Yearly",
		Currency: "USD", BillingInterval: domain.BillingYearly,
		BaseAmountCents: 0, MeterIDs: []string{r.f.meterID},
	})
	if err != nil {
		t.Fatalf("create yearly plan: %v", err)
	}
	plans := &hookedPlanReader{plans: map[string]domain.Plan{oldPlan.ID: oldPlan, newPlan.ID: newPlan}}
	svc := subscription.NewService(r.f.subStore, nil)
	svc.SetPlanReader(plans)
	svc.SetBiller(r.closer.engine)
	return &swapRig{svc: svc, plans: plans, newPlan: newPlan, itemID: r.f.itemID}
}

// swap runs UpdateItemTx (the handler's atomic path) on its own tx at `at`;
// commits on success, rolls back on error, returns the error.
func (s *swapRig) swap(t *testing.T, r *s1Rig, at time.Time) error {
	t.Helper()
	ctx := r.at(at)
	tx, err := r.f.db.BeginTx(ctx, postgres.TxTenant, r.f.tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	_, uerr := s.svc.UpdateItemTx(ctx, tx, r.f.tenantID, r.f.subID, s.itemID, subscription.UpdateItemInput{NewPlanID: s.newPlan.ID, Immediate: true})
	if uerr != nil {
		_ = tx.Rollback()
		return uerr
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit swap: %v", err)
	}
	return nil
}

func assertPeriodMoved409(t *testing.T, err error, wantMsg string) {
	t.Helper()
	var de *errs.DomainError
	if !errors.Is(err, errs.ErrInvalidState) || !errors.As(err, &de) || de.Code != "subscription_period_moved" {
		t.Fatalf("swap error = %v, want InvalidState (409) with code subscription_period_moved", err)
	}
	if wantMsg != "" && !strings.Contains(de.Message, wantMsg) {
		t.Fatalf("swap 409 message = %q, want it to contain %q", de.Message, wantMsg)
	}
}

func (r *s1Rig) itemPlan(t *testing.T) string {
	t.Helper()
	for _, it := range r.sub(t).Items {
		if it.ID == r.f.itemID {
			return it.PlanID
		}
	}
	t.Fatalf("item %s not on sub", r.f.itemID)
	return ""
}

// TestPlanSwap_StaleSnapshot_RejectedAfterClose: the swap read the sub, then
// a competing period writer committed; the swap's period CAS — now the FIRST
// statement of its tx — misses and the request is refused with 409
// subscription_period_moved: item plan unchanged, no refund drafts, no day-1
// invoice, the winner's period stands. A fresh retry succeeds.
//
// Mutation-verify: WHERE id-only CAS → the stale swap commits and rewinds
// the period below the close.
func TestPlanSwap_StaleSnapshot_RejectedAfterClose(t *testing.T) {
	t.Run("in_arrears: operator close wins, stale swap 409, retry succeeds", func(t *testing.T) {
		r := newS1Rig(t, "S1 swap-after-close", 200, false)
		sw := newSwapRig(t, r)
		sw.plans.onSecond = func() {
			if gen := r.runClose(t); gen != 1 {
				t.Fatalf("operator close generated %d, want 1", gen)
			}
		}
		swapAt := r.f.cycleStart.Add(20 * time.Minute)
		assertPeriodMoved409(t, sw.swap(t, r, swapAt), "re-read the subscription and retry")

		if got := r.itemPlan(t); got != r.f.planID {
			t.Fatalf("item plan = %s after a refused swap, want unchanged %s", got, r.f.planID)
		}
		cycle, threshold, other := r.liveInvoices(t)
		if len(cycle) != 1 || len(threshold) != 0 || len(other) != 0 {
			t.Fatalf("invoices: cycle=%d threshold=%d other=%d, want only the close's invoice (no swap artifacts)", len(cycle), len(threshold), len(other))
		}
		r.assertPeriod(t, r.f.cycleEnd, nil) // [P1, P2): the close stands

		// Fresh read → fresh snapshot → the swap truncates the NEW period.
		retryAt := r.f.cycleEnd.Add(time.Hour)
		if err := sw.swap(t, r, retryAt); err != nil {
			t.Fatalf("fresh retry: %v", err)
		}
		if got := r.itemPlan(t); got != sw.newPlan.ID {
			t.Fatalf("item plan = %s after the retry, want %s", got, sw.newPlan.ID)
		}
		r.assertPeriod(t, r.f.cycleEnd, &retryAt) // [P1, retryAt): truncated to the swap instant
	})

	t.Run("in_advance: operator close wins, stale swap 409", func(t *testing.T) {
		r := newS1Rig(t, "S1 swap-after-close in_advance", 200, false)
		// Re-plan the fixture sub onto an in_advance monthly plan (swap target:
		// in_advance yearly) — same cadence both sides, cross interval.
		advMonthly, err := r.f.pricingSvc.CreatePlan(r.base, r.f.tenantID, pricing.CreatePlanInput{
			Code: "pln_adv_m", Name: "Adv Monthly", Currency: "USD", BillingInterval: domain.BillingMonthly,
			BaseAmountCents: 1000, BaseBillTiming: domain.BillInAdvance, MeterIDs: []string{r.f.meterID},
		})
		if err != nil {
			t.Fatalf("create in_advance monthly: %v", err)
		}
		advYearly, err := r.f.pricingSvc.CreatePlan(r.base, r.f.tenantID, pricing.CreatePlanInput{
			Code: "pln_adv_y", Name: "Adv Yearly", Currency: "USD", BillingInterval: domain.BillingYearly,
			BaseAmountCents: 10000, BaseBillTiming: domain.BillInAdvance, MeterIDs: []string{r.f.meterID},
		})
		if err != nil {
			t.Fatalf("create in_advance yearly: %v", err)
		}
		tx, err := r.f.db.BeginTx(r.base, postgres.TxTenant, r.f.tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := tx.ExecContext(r.base, `UPDATE subscription_items SET plan_id = $1 WHERE id = $2`, advMonthly.ID, r.f.itemID); err != nil {
			t.Fatalf("re-plan item: %v", err)
		}
		if _, err := tx.ExecContext(r.base, `UPDATE billing_intervals SET plan_id = $1 WHERE subscription_item_id = $2`, advMonthly.ID, r.f.itemID); err != nil {
			t.Fatalf("re-plan interval: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit re-plan: %v", err)
		}
		plans := &hookedPlanReader{plans: map[string]domain.Plan{advMonthly.ID: advMonthly, advYearly.ID: advYearly}}
		svc := subscription.NewService(r.f.subStore, nil)
		svc.SetPlanReader(plans)
		svc.SetBiller(r.closer.engine)
		sw := &swapRig{svc: svc, plans: plans, newPlan: advYearly, itemID: r.f.itemID}

		sw.plans.onSecond = func() {
			if gen := r.runClose(t); gen != 1 {
				t.Fatalf("operator close generated %d, want 1", gen)
			}
		}
		assertPeriodMoved409(t, sw.swap(t, r, r.f.cycleStart.Add(20*time.Minute)), "")
		if got := r.itemPlan(t); got != advMonthly.ID {
			t.Fatalf("item plan = %s after a refused swap, want unchanged %s", got, advMonthly.ID)
		}
		cycle, _, other := r.liveInvoices(t)
		if len(cycle) != 1 || len(other) != 0 {
			t.Fatalf("invoices: cycle=%d other=%d, want only the close's invoice (no day-1 swap invoice)", len(cycle), len(other))
		}
		r.assertPeriod(t, r.f.cycleEnd, nil)
	})

	t.Run("reset=true fire vs swap: the second committer is refused, both orders", func(t *testing.T) {
		// Fire commits between the swap's read and its CAS → swap 409, the
		// fire's re-anchor stands.
		r := newS1Rig(t, "S1 fire-then-swap", 200, true)
		sw := newSwapRig(t, r)
		sw.plans.onSecond = func() {
			if fired := r.runFire(t); fired != 1 {
				t.Fatalf("fire reported %d, want 1", fired)
			}
		}
		assertPeriodMoved409(t, sw.swap(t, r, r.f.cycleStart.Add(20*time.Minute)), "")
		if got := r.itemPlan(t); got != r.f.planID {
			t.Fatalf("item plan = %s after a refused swap, want unchanged", got)
		}
		_, threshold, _ := r.liveInvoices(t)
		if len(threshold) != 1 {
			t.Fatalf("threshold invoices = %d, want 1", len(threshold))
		}
		r.assertPeriod(t, r.t1, nil) // [t1, t1+1mo): the fire's re-anchor stands

		// Swap commits between the fire's page read and its CAS → fire skipped,
		// the swap's truncation stands.
		r2 := newS1Rig(t, "S1 swap-then-fire", 200, true)
		sw2 := newSwapRig(t, r2)
		swapAt := r2.f.cycleStart.Add(20 * time.Minute)
		r2.fire.subs.beforeTx = func() {
			r2.fire.subs.beforeTx = nil
			if err := sw2.swap(t, r2, swapAt); err != nil {
				t.Fatalf("swap: %v", err)
			}
		}
		if fired := r2.runFire(t); fired != 0 {
			t.Fatalf("fire reported %d after the swap re-anchored, want 0", fired)
		}
		if _, threshold, _ := r2.liveInvoices(t); len(threshold) != 0 {
			t.Fatalf("threshold invoices = %d after a lost CAS, want 0", len(threshold))
		}
		r2.assertPeriod(t, r2.f.cycleStart, &swapAt) // [P0, swapAt): the truncation stands
	})

	t.Run("reset=false fire billed past the swap instant: swap refused, not silently moved", func(t *testing.T) {
		// The fire (t1 = P0+30min) commits T [P0, t1) between the swap's read
		// and its CAS; the swap's instant is P0+20min < t1. The period did not
		// move, so the CAS passes — the watermark read under the row lock is
		// what refuses: truncating to [P0, P0+20min) would re-bill
		// [P0+20min, t1) at the next close.
		//
		// Mutation-verify: `billedThrough.After(now) && false` in
		// applyCrossIntervalPlanSwapTx → the swap commits and rewinds the period
		// below the fire's window.
		r := newS1Rig(t, "S1 fire-vs-swap residual", 200, false)
		sw := newSwapRig(t, r)
		sw.plans.onSecond = func() {
			if fired := r.runFire(t); fired != 1 {
				t.Fatalf("fire reported %d, want 1", fired)
			}
		}
		swapAt := r.f.cycleStart.Add(20 * time.Minute)
		assertPeriodMoved409(t, sw.swap(t, r, swapAt), "already billed this subscription past the requested effective instant")
		if got := r.itemPlan(t); got != r.f.planID {
			t.Fatalf("item plan = %s after a refused swap, want unchanged", got)
		}
		_, threshold, other := r.liveInvoices(t)
		if len(threshold) != 1 || len(other) != 0 {
			t.Fatalf("invoices: threshold=%d other=%d, want only T", len(threshold), len(other))
		}
		r.assertPeriod(t, r.f.cycleStart, &r.f.cycleEnd) // [P0, P1): untouched
	})
}

// ---------------------------------------------------------------------------
// T7 — S9b: immediate cancel commits while the boundary close is in flight.
// ---------------------------------------------------------------------------

// TestImmediateCancel_vs_BoundaryClose_NoDoubleBill: the close read an active
// sub and built [P0, P1); before its tx opens an immediate cancel commits with
// canceled_at = tc < P1 (F bills [P0, tc)). The close's CAS carries `status =
// $7`: the canceled row misses it, nothing is written, the retry sees a
// canceled sub and skips. (Reachable on main only with divergent replica
// clocks — the status predicate closes it regardless.)
//
// Mutation-verify: `status = $7 OR TRUE` → the period is unchanged by the
// cancel, so the CAS passes and I1 [P0, P1) lands beside F.
func TestImmediateCancel_vs_BoundaryClose_NoDoubleBill(t *testing.T) {
	r := newS1Rig(t, "S1 cancel-vs-close", 200, false)

	r.closer.subs.beforeTx = func() {
		r.closer.subs.beforeTx = nil
		ctxCancel := r.at(r.tc)
		if _, err := r.f.subStore.CancelAtomicWithBill(ctxCancel, r.f.tenantID, r.f.subID, func(tx *sql.Tx, canceled domain.Subscription) error {
			_, err := r.fire.engine.BillFinalOnImmediateCancelTx(ctxCancel, tx, canceled)
			return err
		}); err != nil {
			t.Fatalf("cancel: %v", err)
		}
	}
	if gen := r.runClose(t); gen != 0 {
		t.Fatalf("close generated %d on a sub canceled under it, want 0", gen)
	}

	cycle, threshold, other := r.liveInvoices(t)
	if len(other) != 1 || other[0].BillingReason != domain.BillingReasonSubscriptionCancel || len(cycle) != 0 || len(threshold) != 0 {
		t.Fatalf("invoices: cycle=%d threshold=%d other=%d, want only the final-on-cancel invoice", len(cycle), len(threshold), len(other))
	}
	if other[0].SubtotalCents != 400 {
		t.Fatalf("final invoice = %dc, want 400", other[0].SubtotalCents)
	}
	sub := r.sub(t)
	if sub.Status != domain.SubscriptionCanceled || !sub.CurrentBillingPeriodStart.Equal(r.f.cycleStart) {
		t.Fatalf("sub = %s [%v, ...), want canceled with its original period", sub.Status, sub.CurrentBillingPeriodStart)
	}
	assertUsageBilledOnce(t, r.base)
}

// ---------------------------------------------------------------------------
// T8 — crash window: the invoice and the advance share fate.
// ---------------------------------------------------------------------------

// nextInvoiceSeq reads the tenant's test-mode invoice sequence (1 when the
// settings row does not exist yet).
func nextInvoiceSeq(t *testing.T, r *s1Rig) int {
	t.Helper()
	tx, err := r.f.db.BeginTx(r.base, postgres.TxTenant, r.f.tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	var seq int
	if err := tx.QueryRowContext(r.base, `SELECT COALESCE((SELECT invoice_next_seq_test FROM tenant_settings WHERE tenant_id = $1), 1)`, r.f.tenantID).Scan(&seq); err != nil {
		t.Fatalf("read invoice seq: %v", err)
	}
	return seq
}

// TestCycleClose_InvoiceAndAdvance_ShareFate: the closer's tx fails AFTER the
// period CAS, on the invoice insert. Nothing may survive: no invoice, the
// period unchanged, no invoice number consumed; the next run bills once. The
// pre-fix shape committed the invoice first and advanced in a second tx (or,
// in this direction, could advance without the invoice).
//
// Mutation-verify: run the CAS in its own tx (`e.subs.ClosePeriod` in place of
// `e.subs.ClosePeriodTx(ctx, tx, …)` inside commitPeriodClose) → the period
// advances although the invoice failed.
func TestCycleClose_InvoiceAndAdvance_ShareFate(t *testing.T) {
	r := newS1Rig(t, "S1 close share-fate", 200, false)
	seqBefore := nextInvoiceSeq(t, r)

	r.closer.invs.createTxErr = errors.New("injected: invoice insert failed inside the closer tx")
	gen, failures := r.closer.engine.RunCycleForTenant(r.at(r.close), r.f.tenantID, 50)
	if gen != 0 || len(failures) != 1 || !strings.Contains(failures[0].Err.Error(), "injected") {
		t.Fatalf("failed close reported gen=%d failures=%v, want 0 and the injected error", gen, failures)
	}
	if cycle, threshold, other := r.liveInvoices(t); len(cycle)+len(threshold)+len(other) != 0 {
		t.Fatalf("an invoice survived the rolled-back close: cycle=%d threshold=%d other=%d", len(cycle), len(threshold), len(other))
	}
	r.assertPeriod(t, r.f.cycleStart, &r.f.cycleEnd) // [P0, P1): the CAS rolled back with the insert
	if got := nextInvoiceSeq(t, r); got != seqBefore {
		t.Fatalf("invoice sequence moved %d → %d on a rolled-back close (a number was burned)", seqBefore, got)
	}

	r.closer.invs.createTxErr = nil
	if gen := r.runClose(t); gen != 1 {
		t.Fatalf("retry generated %d, want 1", gen)
	}
	cycle, _, _ := r.liveInvoices(t)
	if len(cycle) != 1 || cycle[0].SubtotalCents != 1000 {
		t.Fatalf("retry: cycle invoices = %d (subtotal %d), want one full invoice", len(cycle), cycle[0].SubtotalCents)
	}
	r.assertPeriod(t, r.f.cycleEnd, nil)
	assertUsageBilledOnce(t, r.base)
}
