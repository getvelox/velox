package billing_test

import (
	"context"
	"github.com/sagarsuperuser/velox/internal/platform/leader"
	"github.com/sagarsuperuser/velox/internal/platform/leader/leadertest"
	"sync"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/billing"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/subscription"
	"github.com/sagarsuperuser/velox/internal/subscription/subscriptiontest"
	"github.com/sagarsuperuser/velox/internal/tax"
	"github.com/sagarsuperuser/velox/internal/tenant"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/usage"
)

// TestConcurrentBilling_ExactlyOneInvoice is the CONCURRENCY twin of FLOW B3's
// sequential "run billing twice → idempotent skip". It proves exactly-once invoice
// generation under a real race: N billing-cycle runs hit the SAME due
// subscription+period simultaneously (two scheduler instances / an overlapping
// retry), yet exactly ONE cycle invoice is created.
//
// There is no application-level pre-SELECT — the only guard is the Postgres unique
// index idx_invoices_billing_idempotency. The losing goroutines collide on it
// (SQLSTATE 23505), which the invoice store maps to errs.ErrAlreadyExists and
// billOnePeriod catches as a graceful idempotent skip (returns invoiced=false,
// err=nil). The two assertions that matter:
//   - NO goroutine surfaces an error (the loser's 23505 is handled, not bubbled).
//   - Exactly ONE invoice exists (no double-billing).
//
// Run with -race to also catch any data race in the engine under concurrency.
func TestConcurrentBilling_ExactlyOneInvoice(t *testing.T) {
	db := testutil.SetupTestDB(t) // skips on -short
	ctx := leadertest.Token(t, testutil.AdminPool(t), postgres.WithLivemode(context.Background(), false), leader.RoleBilling, leader.RoleDunning)

	customerStore := customer.NewPostgresStore(db)
	pricingStore := pricing.NewPostgresStore(db)
	subStore := subscription.NewPostgresStore(db)
	usageStore := usage.NewPostgresStore(db)
	invoiceStore := invoice.NewPostgresStore(db)

	tenantID := testutil.CreateTestTenant(t, db, "Concurrency Test Corp")
	cust, err := customerStore.Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_concurrency", DisplayName: "Concurrency Customer",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	// Base-only plan — no meters/usage needed; the cycle invoice is the base fee.
	plan, err := pricingStore.CreatePlan(ctx, tenantID, domain.Plan{
		Code: "flat", Name: "Flat Plan", Currency: "USD",
		BillingInterval: domain.BillingMonthly, Status: domain.PlanActive,
		BaseAmountCents: 4900,
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}

	periodStart := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	sub, err := subStore.Create(ctx, tenantID, domain.Subscription{
		Code: "sub-concurrency", DisplayName: "Concurrency Sub",
		CustomerID: cust.ID,
		Items:      []domain.SubscriptionItem{{PlanID: plan.ID, Quantity: 1}},
		Status:     domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
		StartedAt:                 &periodStart,
		CurrentBillingPeriodStart: &periodStart, CurrentBillingPeriodEnd: bivPE(periodStart),
	})
	if err != nil {
		t.Fatalf("create subscription: %v", err)
	}
	// Make exactly one period due: next_billing_at = periodEnd.
	subscriptiontest.SetBillingCycle(t, ctx, db, tenantID, sub.ID, periodStart, periodEnd, periodEnd, 0)

	// Pin the clock just past period_end so only one period is due.
	fakeClk := clock.NewFake(periodEnd.Add(time.Nanosecond))
	engine := billing.NewEngine(
		&subStoreAdapter{subStore},
		&usageStoreAdapter{usageStore},
		&pricingStoreAdapter{pricingStore},
		&invoiceStoreAdapter{invoiceStore},
		nil, tenant.NewSettingsStore(db), testPaymentSetupsNoPM{}, testChargerSentinel{}, fakeClk,
	)
	engine.SetIntervalReader(subStore)
	engine.SetTaxProviderResolver(tax.NewResolver(nil))
	engine.SetNoPaymentMethodNotifier(&testNoPMNotifier{})

	// Fire N billing runs at the same instant against the one due sub.
	const N = 4
	var (
		wg       sync.WaitGroup
		start    = make(chan struct{})
		mu       sync.Mutex
		totalGen int
		allErrs  []error
	)
	for range N {
		wg.Go(func() {
			<-start // barrier: all goroutines race from the same instant
			gen, errs := engine.RunCycle(ctx, 50)
			mu.Lock()
			totalGen += gen
			allErrs = append(allErrs, errs...)
			mu.Unlock()
		})
	}
	close(start)
	wg.Wait()

	// 1. The losing goroutines must NOT surface an error — the 23505 unique-index
	//    collision is caught as errs.ErrAlreadyExists and handled as an idempotent skip.
	if len(allErrs) > 0 {
		t.Fatalf("concurrent billing surfaced %d error(s) — a loser's unique-violation was not handled gracefully: %v", len(allErrs), allErrs)
	}
	// 2. Exactly one run reports generating the invoice.
	if totalGen != 1 {
		t.Fatalf("expected exactly 1 invoice generated across %d concurrent runs, got %d", N, totalGen)
	}
	// 3. The DB holds exactly one invoice for the subscription (no double-billing).
	invoices, total, err := invoiceStore.List(ctx, invoice.ListFilter{TenantID: tenantID})
	if err != nil {
		t.Fatalf("list invoices: %v", err)
	}
	if total != 1 {
		t.Fatalf("expected exactly 1 invoice in DB after the race, got %d (double-billing under concurrency)", total)
	}
	if invoices[0].SubscriptionID != sub.ID {
		t.Errorf("invoice subscription_id: got %q, want %q", invoices[0].SubscriptionID, sub.ID)
	}
}

// TestManualRunVsSchedulerRace_ExactlyOneInvoice (P3 DoD): the operator-triggered
// RunCycleForTenant and the wall-clock scheduler's RunCycle racing the SAME due
// sub+period must still produce exactly one invoice. Since ADR-115 the guard is
// the period CAS at the head of the closer tx, not the invoice index: the loser
// observes subscription.ErrWatermarkMoved and writes nothing — it never reaches
// the index. The scheduler legs carry a live billing lease (their fetch is
// fenced, ADR-114); the manual legs are unfenced on purpose.
//
// Repaired 2026-08-30: the ctx carried no lease token, so both RunCycle legs
// failed their fetch with leader.ErrNoToken and the error was discarded — the
// test raced two manual runs only. The scheduler errors are asserted empty now,
// and a hooked subtest pins the loser's exit deterministically.
//
// Mutation-verify: WHERE id-only CAS → the loser passes the CAS and hits the
// cycle index instead: alreadyExists > 0 and moved == 0.
func TestManualRunVsSchedulerRace_ExactlyOneInvoice(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := leadertest.Token(t, testutil.AdminPool(t), postgres.WithLivemode(context.Background(), false), leader.RoleBilling)

	subStore := subscription.NewPostgresStore(db)
	invoiceStore := invoice.NewPostgresStore(db)
	tenantID := testutil.CreateTestTenant(t, db, "Manual-vs-Scheduler Race")
	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{ExternalID: "cus_race2", DisplayName: "Race2"})
	if err != nil {
		t.Fatalf("customer: %v", err)
	}
	plan, err := pricing.NewPostgresStore(db).CreatePlan(ctx, tenantID, domain.Plan{
		Code: "flat2", Name: "Flat2", Currency: "USD",
		BillingInterval: domain.BillingMonthly, Status: domain.PlanActive, BaseAmountCents: 4900,
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	periodStart := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	sub, err := subStore.Create(ctx, tenantID, domain.Subscription{
		Code: "sub-race2", DisplayName: "Race2", CustomerID: cust.ID,
		Items:  []domain.SubscriptionItem{{PlanID: plan.ID, Quantity: 1}},
		Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar, StartedAt: &periodStart,
		CurrentBillingPeriodStart: &periodStart, CurrentBillingPeriodEnd: bivPE(periodStart),
	})
	if err != nil {
		t.Fatalf("sub: %v", err)
	}
	subscriptiontest.SetBillingCycle(t, ctx, db, tenantID, sub.ID, periodStart, periodEnd, periodEnd, 0)

	subs := &failableSubAdapter{subStoreAdapter: &subStoreAdapter{subStore}}
	invs := &failableInvoiceAdapter{invoiceStoreAdapter: &invoiceStoreAdapter{invoiceStore}}
	engine := billing.NewEngine(
		subs, &usageStoreAdapter{usage.NewPostgresStore(db)},
		&pricingStoreAdapter{pricing.NewPostgresStore(db)}, invs,
		nil, tenant.NewSettingsStore(db), testPaymentSetupsNoPM{}, testChargerSentinel{}, clock.NewFake(periodEnd.Add(time.Nanosecond)),
	)
	engine.SetIntervalReader(subStore)
	engine.SetTaxProviderResolver(tax.NewResolver(nil))
	engine.SetNoPaymentMethodNotifier(&testNoPMNotifier{})

	var (
		wg       sync.WaitGroup
		start    = make(chan struct{})
		mu       sync.Mutex
		totalGen int
		schedErr []error
	)
	// 2 scheduler runs + 2 manual runs, all on the same due sub.
	for range 2 {
		wg.Go(func() {
			<-start
			g, errs := engine.RunCycle(ctx, 50)
			mu.Lock()
			totalGen += g
			schedErr = append(schedErr, errs...)
			mu.Unlock()
		})
		wg.Go(func() {
			<-start
			g, failures := engine.RunCycleForTenant(ctx, tenantID, 50)
			mu.Lock()
			totalGen += g
			for _, f := range failures {
				schedErr = append(schedErr, f.Err)
			}
			mu.Unlock()
		})
	}
	close(start)
	wg.Wait()

	if len(schedErr) != 0 {
		t.Fatalf("racing runs surfaced errors (a lost race is not an error): %v", schedErr)
	}
	if totalGen != 1 {
		t.Fatalf("manual-vs-scheduler race generated %d invoices, want exactly 1", totalGen)
	}
	_, total, err := invoiceStore.List(ctx, invoice.ListFilter{TenantID: tenantID})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 1 {
		t.Fatalf("DB holds %d invoices after the manual-vs-scheduler race, want 1", total)
	}
	if n := invs.alreadyExists.Load(); n != 0 {
		t.Fatalf("%d closer(s) reached the invoice index — the CAS must refuse a loser before it inserts", n)
	}

	// Deterministic loser: the manual run's closer tx opens only after the
	// scheduler's run has committed the same period — its CAS must miss.
	t.Run("hooked: scheduler commits under the manual run", func(t *testing.T) {
		sub2, err := subStore.Create(ctx, tenantID, domain.Subscription{
			Code: "sub-race3", DisplayName: "Race3", CustomerID: cust.ID,
			Items:  []domain.SubscriptionItem{{PlanID: plan.ID, Quantity: 1}},
			Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar, StartedAt: &periodStart,
			CurrentBillingPeriodStart: &periodStart, CurrentBillingPeriodEnd: bivPE(periodStart),
		})
		if err != nil {
			t.Fatalf("sub2: %v", err)
		}
		subscriptiontest.SetBillingCycle(t, ctx, db, tenantID, sub2.ID, periodStart, periodEnd, periodEnd, 0)
		movedBefore, existsBefore := subs.moved.Load(), invs.alreadyExists.Load()

		subs.beforeTx = func() {
			subs.beforeTx = nil
			if g, errs := engine.RunCycle(ctx, 50); g != 1 || len(errs) != 0 {
				t.Fatalf("scheduler run under the manual run: gen=%d errs=%v, want 1 and none", g, errs)
			}
		}
		g, failures := engine.RunCycleForTenant(ctx, tenantID, 50)
		if g != 0 || len(failures) != 0 {
			t.Fatalf("manual run after losing the CAS: gen=%d failures=%v, want 0 and none", g, failures)
		}
		if moved := subs.moved.Load() - movedBefore; moved != 1 {
			t.Fatalf("loser observed ErrWatermarkMoved %d time(s), want exactly 1", moved)
		}
		if n := invs.alreadyExists.Load() - existsBefore; n != 0 {
			t.Fatalf("loser reached the invoice index %d time(s), want 0", n)
		}
		_, total, err := invoiceStore.List(ctx, invoice.ListFilter{TenantID: tenantID})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if total != 2 {
			t.Fatalf("DB holds %d invoices, want 2 (one per sub)", total)
		}
	})
}

// TestRunCycleForTenant_BillsOnlyCallerLivemode (P3 DoD): a test-mode run must
// bill only the tenant's test-mode subs, never its live-mode subs — the RLS
// policy (0020 mode-aware) scopes livemode from the TxTenant ctx.
func TestRunCycleForTenant_BillsOnlyCallerLivemode(t *testing.T) {
	db := testutil.SetupTestDB(t)
	subStore := subscription.NewPostgresStore(db)
	invoiceStore := invoice.NewPostgresStore(db)
	tenantID := testutil.CreateTestTenant(t, db, "Livemode Scope")

	periodStart := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)

	// One due sub per mode, same tenant.
	mkSub := func(live bool, suffix string) {
		modeCtx := postgres.WithLivemode(context.Background(), live)
		cust, err := customer.NewPostgresStore(db).Create(modeCtx, tenantID, domain.Customer{ExternalID: "cus_" + suffix, DisplayName: suffix})
		if err != nil {
			t.Fatalf("%s customer: %v", suffix, err)
		}
		plan, err := pricing.NewPostgresStore(db).CreatePlan(modeCtx, tenantID, domain.Plan{
			Code: "flat-" + suffix, Name: suffix, Currency: "USD",
			BillingInterval: domain.BillingMonthly, Status: domain.PlanActive, BaseAmountCents: 4900,
		})
		if err != nil {
			t.Fatalf("%s plan: %v", suffix, err)
		}
		sub, err := subStore.Create(modeCtx, tenantID, domain.Subscription{
			Code: "sub-" + suffix, DisplayName: suffix, CustomerID: cust.ID,
			Items:  []domain.SubscriptionItem{{PlanID: plan.ID, Quantity: 1}},
			Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar, StartedAt: &periodStart,
			CurrentBillingPeriodStart: &periodStart, CurrentBillingPeriodEnd: bivPE(periodStart),
		})
		if err != nil {
			t.Fatalf("%s sub: %v", suffix, err)
		}
		subscriptiontest.SetBillingCycle(t, modeCtx, db, tenantID, sub.ID, periodStart, periodEnd, periodEnd, 0)
	}
	mkSub(false, "test")
	mkSub(true, "live")

	engine := billing.NewEngine(
		&subStoreAdapter{subStore}, &usageStoreAdapter{usage.NewPostgresStore(db)},
		&pricingStoreAdapter{pricing.NewPostgresStore(db)}, &invoiceStoreAdapter{invoiceStore},
		nil, tenant.NewSettingsStore(db), testPaymentSetupsNoPM{}, testChargerSentinel{}, clock.NewFake(periodEnd.Add(time.Nanosecond)),
	)
	engine.SetIntervalReader(subStore)
	engine.SetTaxProviderResolver(tax.NewResolver(nil))
	engine.SetNoPaymentMethodNotifier(&testNoPMNotifier{})

	// Run in TEST mode — must bill only the test-mode sub.
	testCtx := postgres.WithLivemode(context.Background(), false)
	gen, failures := engine.RunCycleForTenant(testCtx, tenantID, 50)
	if len(failures) != 0 {
		t.Fatalf("unexpected failures: %v", failures)
	}
	if gen != 1 {
		t.Fatalf("test-mode run generated %d invoices, want exactly 1 (only the test-mode sub)", gen)
	}
	testInv, _, err := invoiceStore.List(testCtx, invoice.ListFilter{TenantID: tenantID})
	if err != nil {
		t.Fatalf("list test invoices: %v", err)
	}
	if len(testInv) != 1 {
		t.Fatalf("test mode holds %d invoices, want 1", len(testInv))
	}
	// The live-mode sub must be untouched (no live invoice generated).
	liveInv, _, err := invoiceStore.List(postgres.WithLivemode(context.Background(), true), invoice.ListFilter{TenantID: tenantID})
	if err != nil {
		t.Fatalf("list live invoices: %v", err)
	}
	if len(liveInv) != 0 {
		t.Fatalf("a test-mode billing run generated %d LIVE invoices — livemode scope leaked", len(liveInv))
	}
}
