package billing

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/subscription"
)

// P27: the scheduler billed one page of subs per tick (50 per hour in prod),
// so 20,000 subs due on the 1st took about 17 days to invoice. These tests
// pin the drain that replaced it and the exclusion that keeps failing subs
// from blocking healthy ones.

// failingDueSub is due a month before dueSubFixture's subs, so it sorts first,
// and has no billing period, so billOnePeriod refuses it on every attempt and
// it stays due.
func failingDueSub(id string) domain.Subscription {
	sub := dueSubFixture(id)
	nba := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	sub.NextBillingAt = &nba
	sub.CurrentBillingPeriodStart = nil
	sub.CurrentBillingPeriodEnd = nil
	return sub
}

func subsMock(subs ...domain.Subscription) *mockSubs {
	m := &mockSubs{subs: map[string]domain.Subscription{}, cycleUpdated: map[string]bool{}}
	for _, s := range subs {
		m.subs[s.ID] = s
	}
	return m
}

// More due subs than one page: a single RunCycle call bills all of them, and
// stamps liveness once per page.
func TestRunCycle_DrainsEveryDueSubInOneCall(t *testing.T) {
	var due []domain.Subscription
	for _, id := range []string{"sub_1", "sub_2", "sub_3", "sub_4", "sub_5", "sub_6", "sub_7"} {
		due = append(due, dueSubFixture(id))
	}
	subs := subsMock(due...)
	engine := tenantRunEngine(subs)

	pages := 0
	generated, errs := engine.RunCycle(context.Background(), 3, func() { pages++ })
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if generated != 7 {
		t.Fatalf("generated = %d, want 7: one call must bill every due sub, not one page of 3", generated)
	}
	for id := range subs.subs {
		if !subs.cycleUpdated[id] {
			t.Errorf("%s was not billed", id)
		}
	}
	// Pages of 3, 3, 1; the empty fetch that ends the run is not a page.
	if pages != 3 {
		t.Errorf("onPage ran %d times, want 3 (once per non-empty page)", pages)
	}
}

// Failing subs are the oldest due, so they lead every page. They must be
// attempted once and then skipped, so the healthy subs behind them still bill.
// Before the fix the scheduler's one page was all failures, and the manual and
// Advance runs stopped at a page where every sub had already failed.
func TestDrain_FailingSubsAtTheHeadDoNotBlockHealthyOnes(t *testing.T) {
	runs := map[string]func(e *Engine) (int, []error){
		"scheduled": func(e *Engine) (int, []error) {
			return e.RunCycle(context.Background(), 2, nil)
		},
		"manual": func(e *Engine) (int, []error) {
			n, f := e.RunCycleForTenant(context.Background(), "t1", 2)
			return n, subBillErrors(f)
		},
	}
	for name, run := range runs {
		t.Run(name, func(t *testing.T) {
			subs := subsMock(
				failingDueSub("sub_bad_1"), failingDueSub("sub_bad_2"),
				dueSubFixture("sub_ok_1"), dueSubFixture("sub_ok_2"), dueSubFixture("sub_ok_3"),
			)
			generated, errs := run(tenantRunEngine(subs))

			if generated != 3 {
				t.Errorf("generated = %d, want 3: the healthy subs behind the failing ones must bill", generated)
			}
			for _, id := range []string{"sub_ok_1", "sub_ok_2", "sub_ok_3"} {
				if !subs.cycleUpdated[id] {
					t.Errorf("%s was not billed", id)
				}
			}
			// Exactly one error per failing sub: each is attempted once, then
			// left out of later pages. A second error (errStillDue) means a
			// failing sub came back on a later page.
			if len(errs) != 2 {
				t.Fatalf("got %d errors, want 2 (one per failing sub): %v", len(errs), errs)
			}
			for _, err := range errs {
				if errors.Is(err, errStillDue) {
					t.Errorf("failing sub reappeared after it failed: %v", err)
				}
			}
		})
	}
}

// billSubscription returns cleanly while the sub stays due when another closer
// wins its period twice in a row. The drain must not bill it again in the same
// run; it reports errStillDue once and ends.
func TestDrain_SubStillDueAfterCleanAttemptIsReportedOnce(t *testing.T) {
	subs := subsMock(dueSubFixture("sub_raced"))
	subs.closePeriodErr = subscription.ErrWatermarkMoved // every close loses the CAS
	engine := tenantRunEngine(subs)

	done := make(chan struct{})
	var generated int
	var errs []error
	go func() {
		generated, errs = engine.RunCycle(context.Background(), 50, nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunCycle did not terminate on a sub that stays due after a clean attempt")
	}
	if generated != 0 {
		t.Errorf("generated = %d, want 0", generated)
	}
	if len(errs) != 1 || !errors.Is(errs[0], errStillDue) {
		t.Fatalf("want exactly one errStillDue, got %v", errs)
	}
	if subs.closePeriodCalls != 2 {
		t.Errorf("close attempted %d times, want 2 (one attempt, one re-plan; no second attempt in the run)", subs.closePeriodCalls)
	}
}

// The scheduler must hand its liveness stamp to the drain. The stamp normally
// fires between ticks; /health/ready goes 503 when it is 2x the interval old,
// and on a single replica that pulls the only API server out of the load
// balancer. A drain of thousands of subs (each charged inline) can run that
// long, so it stamps after every page.
func TestScheduler_BillingDrainStampsLivenessPerPage(t *testing.T) {
	subs := subsMock(dueSubFixture("sub_1"), dueSubFixture("sub_2"), dueSubFixture("sub_3"))
	stamps := 0
	s := &Scheduler{engine: tenantRunEngine(subs), batch: 1, onRun: func() { stamps++ }}

	s.runBillingCycleForMode(postgres.WithLivemode(context.Background(), true), true)

	if stamps != 3 {
		t.Fatalf("liveness stamped %d times during a 3-page drain, want 3", stamps)
	}
}

// Shutdown or a lost lease in the middle of a page: the drain stops at the
// next sub instead of failing every remaining sub with the same ctx error.
func TestDrain_StopsAtTheNextSubWhenCtxEnds(t *testing.T) {
	subs := subsMock(dueSubFixture("sub_1"), dueSubFixture("sub_2"), dueSubFixture("sub_3"), dueSubFixture("sub_4"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	subs.beforeCloserTx = func(*mockSubs) { cancel() } // ctx ends while sub_1 closes
	engine := tenantRunEngine(subs)

	_, errs := engine.RunCycle(ctx, 4, nil) // one page holds all four

	// sub_1 was mid-billing when ctx ended, so it fails with ctx; the run
	// adds one error of its own. Subs 2-4 must not be attempted at all.
	for _, err := range errs {
		if !errors.Is(err, context.Canceled) {
			t.Errorf("unexpected non-ctx error: %v", err)
		}
		for _, id := range []string{"sub_2", "sub_3", "sub_4"} {
			if strings.Contains(err.Error(), id) {
				t.Errorf("%s was attempted after the ctx ended: %v", id, err)
			}
		}
	}
	if len(errs) != 2 {
		t.Errorf("got %d errors, want 2 (sub_1 in flight + the stopped run): %v", len(errs), errs)
	}
}

// The auto-charge sweep pages through the whole queue, so the scheduler hands
// it the same liveness stamp as the billing drain.
func TestScheduler_ChargeSweepStampsLivenessPerPage(t *testing.T) {
	_, invoices, _, inv := collectFixture() // one queued, card-less invoice
	for _, id := range []string{"inv_c2", "inv_c3"} {
		more := inv
		more.ID = id
		invoices.invoices = append(invoices.invoices, more)
	}
	e := wireBaseTax(NewEngine(&mockSubs{}, &mockUsage{}, &mockPricing{}, invoices, nil, &mockSettings{}, nil, nil, billingTestClock()))
	stamps := 0
	s := &Scheduler{engine: e, batch: 1, onRun: func() { stamps++ }}

	s.runBillingCycleForMode(postgres.WithLivemode(context.Background(), true), true)

	if stamps != 3 {
		t.Fatalf("liveness stamped %d times while sweeping 3 queued invoices one per page, want 3", stamps)
	}
}

// cancelingCharger ends the sweep's ctx while charging, as a shutdown or a
// lost lease would mid-sweep.
type cancelingCharger struct {
	recordingCharger
	cancel context.CancelFunc
}

func (c *cancelingCharger) ChargeInvoice(ctx context.Context, tenantID string, inv domain.Invoice, cus, pm string) (domain.Invoice, error) {
	defer c.cancel()
	return c.recordingCharger.ChargeInvoice(ctx, tenantID, inv, cus, pm)
}

// A sweep whose ctx ends mid-page stops before claiming the next invoice:
// the next tick charges it. Before, every remaining row was claimed and
// charged on a dead ctx, failing one by one.
func TestProcessAutoCharge_StopsAtTheNextInvoiceWhenCtxEnds(t *testing.T) {
	e, invoices, _, inv := collectFixture()
	second := inv
	second.ID = "inv_c2"
	invoices.invoices = append(invoices.invoices, second)
	e.paymentSetups = &fakePaymentSetups{ready: true, stripeCustomerID: "cus_stripe"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	charger := &cancelingCharger{recordingCharger: recordingCharger{store: invoices}, cancel: cancel}
	e.charger = charger

	_, errs := e.processAutoCharge(ctx, invoices.invoices, nil, noPMTriggerSweep)

	if len(charger.got) != 1 {
		t.Fatalf("charged %d invoices, want 1: the second must wait for the next tick", len(charger.got))
	}
	// The stop is not a failure per row; the drain reports it once.
	if len(errs) != 0 {
		t.Fatalf("want no per-row errors for a stopped sweep, got %v", errs)
	}
	if !autoChargePending(t, invoices, "inv_c2") {
		t.Error("the uncharged invoice must stay queued")
	}
}

// Test-clock Advance pages through the clock's queue the same way. The three
// invoices are card-less, so they stay queued after their visit: only the
// cursor moves the drain past them. Without it, the first page would come
// back forever and Advance would hang until its timeout.
func TestRetryPendingChargesForClock_PagesThroughTheQueue(t *testing.T) {
	e, invoices, notifier, inv := collectFixture() // no card on file
	invoices.clockQueue = map[string]bool{inv.ID: true}
	for _, id := range []string{"inv_c2", "inv_c3"} {
		more := inv
		more.ID = id
		invoices.invoices = append(invoices.invoices, more)
		invoices.clockQueue[id] = true
	}

	done := make(chan []error, 1)
	go func() {
		_, errs := e.RetryPendingChargesForClock(context.Background(), "t1", "clk_1", 1)
		done <- errs
	}()
	select {
	case errs := <-done:
		if len(errs) != 0 {
			t.Fatalf("errors: %v", errs)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the clock sweep did not finish: it re-reads the first page instead of paging past it")
	}
	if len(notifier.got) != 3 {
		t.Fatalf("setup emails = %d, want 3: every queued invoice must be visited once", len(notifier.got))
	}
	for _, id := range []string{inv.ID, "inv_c2", "inv_c3"} {
		if !autoChargePending(t, invoices, id) {
			t.Errorf("%s left the queue; card-less invoices stay queued", id)
		}
	}
}
