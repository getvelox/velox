package subscription_test

import (
	"context"
	"database/sql"
	"errors"
	"github.com/sagarsuperuser/velox/internal/errs"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/subscription"
	"github.com/sagarsuperuser/velox/internal/subscription/subscriptiontest"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestCancelAtomic_OneWinnerUnderContention is the regression test for COR-4c:
// Cancel previously read the subscription in one tx and wrote the updated
// status back in a separate tx, so N goroutines could each observe status
// "active", each pass the transition check, and each issue an UPDATE — the
// final write wins but every caller returns success. Callers that then acted
// on that "success" (e.g. credit-refund, email dispatch) would fire N times.
// The atomic implementation scopes the transition check to the UPDATE WHERE
// clause, so exactly one caller sees a row returned and the rest correctly
// fail with a stale-status error.
func TestCancelAtomic_OneWinnerUnderContention(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)

	store := subscription.NewPostgresStore(db)
	svc := subscription.NewService(store, nil)
	tenantID := testutil.CreateTestTenant(t, db, "Sub Cancel Race")
	subID := seedActiveSubscription(t, db, tenantID, "cus_cancel_race", "plan_cancel_race", "sub-cancel-race")

	const goroutines = 20
	var (
		wg        sync.WaitGroup
		successes atomic.Int64
	)

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := svc.Cancel(ctx, tenantID, subID)
			if err == nil {
				successes.Add(1)
				return
			}
			// Every non-winner must see an already-terminated/invalid-state
			// error — any other error (deadlock, tenant-scope, FK) is a bug.
			// The atomic cancel path reports "cannot cancel canceled
			// subscription (already terminated)"; the validation path reports
			// "can only cancel active or paused". Both are valid race-loser
			// outcomes.
			msg := err.Error()
			if !strings.Contains(msg, "can only cancel active or paused") &&
				!strings.Contains(msg, "cannot cancel canceled subscription") {
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := successes.Load(); got != 1 {
		t.Fatalf("expected exactly 1 successful cancel, got %d", got)
	}

	final, err := svc.Get(ctx, tenantID, subID)
	if err != nil {
		t.Fatalf("get final sub: %v", err)
	}
	if final.Status != domain.SubscriptionCanceled {
		t.Fatalf("final status = %s, want canceled", final.Status)
	}
	if final.CanceledAt == nil {
		t.Fatal("canceled_at must be set after successful cancel")
	}
}

// TestTransitionAtomic_NotFoundVsWrongState verifies the two-bucket error
// contract: unknown IDs return ErrNotFound (HTTP 404 upstream), while
// known IDs in a disallowed state return a conflict message with the
// current status so operators can debug without re-fetching.
func TestTransitionAtomic_NotFoundVsWrongState(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)

	store := subscription.NewPostgresStore(db)
	svc := subscription.NewService(store, nil)
	tenantID := testutil.CreateTestTenant(t, db, "Sub Transitions")

	// Cancel a nonexistent subscription — must be ErrNotFound, not a
	// status-mismatch message that would leak the schema to callers.
	_, _, err := svc.Cancel(ctx, tenantID, "vlx_sub_does_not_exist")
	if err == nil {
		t.Fatal("expected ErrNotFound for unknown id")
	}
	if strings.Contains(err.Error(), "can only cancel") {
		t.Errorf("expected not-found, got wrong-status error: %v", err)
	}
}

// TestApplyItemPlanImmediately_RaceConverges pins the store-level contract
// for concurrent immediate plan swaps: N goroutines each swap the same item
// to the same target plan, Postgres serializes the row-level UPDATEs, and
// the final row must reflect exactly that target — never a half-applied
// state, never a revert to the old plan, and never a bubbled serialization
// error. This is the foundation proration dedup rests on: without a
// deterministic swap under contention, the dedup key itself is a moving
// target.
//
// The realistic trigger is a user double-clicking "Change plan" or two
// browser tabs racing the same mutation. The assertion that every caller
// returned without error matters here — any bubbled 500 would surface as a
// phantom failure even though the change landed.
func TestApplyItemPlanImmediately_RaceConverges(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)

	store := subscription.NewPostgresStore(db)
	tenantID := testutil.CreateTestTenant(t, db, "Plan Change Race")

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_plan_race", DisplayName: "Plan Race",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	pricingStore := pricing.NewPostgresStore(db)
	planA, err := pricingStore.CreatePlan(ctx, tenantID, domain.Plan{
		Code: "plan-a-race", Name: "Plan A", Currency: "USD",
		BillingInterval: domain.BillingMonthly, Status: domain.PlanActive,
	})
	if err != nil {
		t.Fatalf("create plan A: %v", err)
	}
	planB, err := pricingStore.CreatePlan(ctx, tenantID, domain.Plan{
		Code: "plan-b-race", Name: "Plan B", Currency: "USD",
		BillingInterval: domain.BillingMonthly, Status: domain.PlanActive,
	})
	if err != nil {
		t.Fatalf("create plan B: %v", err)
	}

	now := time.Now().UTC()
	sub, err := store.Create(ctx, tenantID, domain.Subscription{
		Code: "sub-plan-race", DisplayName: "Plan Race Sub",
		CustomerID: cust.ID,
		Status:     domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
		StartedAt:                 &now,
		CurrentBillingPeriodStart: &now, CurrentBillingPeriodEnd: bivPE(now),
		Items: []domain.SubscriptionItem{{PlanID: planA.ID, Quantity: 1}},
	})
	if err != nil {
		t.Fatalf("create subscription: %v", err)
	}
	if len(sub.Items) != 1 {
		t.Fatalf("expected 1 item hydrated on create, got %d", len(sub.Items))
	}
	itemID := sub.Items[0].ID

	const goroutines = 20
	var (
		wg       sync.WaitGroup
		swapErrs = make(chan error, goroutines)
	)

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := store.ApplyItemPlanImmediately(ctx, tenantID, itemID, planB.ID, time.Now().UTC())
			if err != nil {
				swapErrs <- err
			}
		}()
	}
	wg.Wait()
	close(swapErrs)

	for err := range swapErrs {
		t.Errorf("unexpected error from ApplyItemPlanImmediately under contention: %v", err)
	}

	final, err := store.GetItem(ctx, tenantID, itemID)
	if err != nil {
		t.Fatalf("get final item: %v", err)
	}
	if final.PlanID != planB.ID {
		t.Errorf("final plan_id = %q, want %q (race did not converge to target)", final.PlanID, planB.ID)
	}
	if final.PendingPlanID != "" {
		t.Errorf("pending_plan_id not cleared after immediate swap: %q", final.PendingPlanID)
	}
	if final.PlanChangedAt == nil {
		t.Errorf("plan_changed_at not stamped after swap")
	}
}

// TestApplyItemPlanImmediately_SupersedesPendingUnderRace covers the
// immediate-vs-scheduled interleave: one goroutine schedules a future plan
// change (SetItemPendingPlan), another applies an immediate change. Since
// the immediate path clears pending_plan_id as part of its UPDATE, the
// outcome must be that the item is on the immediate's target with no
// pending remnant — regardless of which commit lands first. If this ever
// regressed, the billing engine's next-cycle ApplyDuePendingItemPlans run
// would re-swap the plan back, effectively undoing the user's immediate
// change.
func TestApplyItemPlanImmediately_SupersedesPendingUnderRace(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)

	store := subscription.NewPostgresStore(db)
	tenantID := testutil.CreateTestTenant(t, db, "Plan Change Supersede")

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_supersede", DisplayName: "Supersede",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	pricingStore := pricing.NewPostgresStore(db)
	planA, err := pricingStore.CreatePlan(ctx, tenantID, domain.Plan{
		Code: "plan-sup-a", Name: "A", Currency: "USD",
		BillingInterval: domain.BillingMonthly, Status: domain.PlanActive,
	})
	if err != nil {
		t.Fatalf("create plan A: %v", err)
	}
	planB, err := pricingStore.CreatePlan(ctx, tenantID, domain.Plan{
		Code: "plan-sup-b", Name: "B", Currency: "USD",
		BillingInterval: domain.BillingMonthly, Status: domain.PlanActive,
	})
	if err != nil {
		t.Fatalf("create plan B: %v", err)
	}
	planC, err := pricingStore.CreatePlan(ctx, tenantID, domain.Plan{
		Code: "plan-sup-c", Name: "C", Currency: "USD",
		BillingInterval: domain.BillingMonthly, Status: domain.PlanActive,
	})
	if err != nil {
		t.Fatalf("create plan C: %v", err)
	}

	now := time.Now().UTC()
	sub, err := store.Create(ctx, tenantID, domain.Subscription{
		Code: "sub-supersede", DisplayName: "Supersede Sub",
		CustomerID: cust.ID,
		Status:     domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
		StartedAt:                 &now,
		CurrentBillingPeriodStart: &now, CurrentBillingPeriodEnd: bivPE(now),
		Items: []domain.SubscriptionItem{{PlanID: planA.ID, Quantity: 1}},
	})
	if err != nil {
		t.Fatalf("create subscription: %v", err)
	}
	itemID := sub.Items[0].ID
	future := now.Add(30 * 24 * time.Hour)

	// Run scheduled + immediate concurrently. Each ordering is valid; only the
	// end state is constrained.
	const rounds = 10
	for round := 0; round < rounds; round++ {
		// Reset item to pristine state so each round exercises the race from
		// a known baseline — otherwise a prior round could leave plan=C and
		// the next round's "expect plan_id == C" assertion would pass
		// vacuously.
		if _, err := store.ApplyItemPlanImmediately(ctx, tenantID, itemID, planA.ID, now); err != nil {
			t.Fatalf("round %d: reset item: %v", round, err)
		}

		var wg sync.WaitGroup
		wg.Add(2)
		var scheduleErr, immediateErr atomic.Value
		go func() {
			defer wg.Done()
			if _, err := store.SetItemPendingPlan(ctx, tenantID, itemID, planB.ID, future); err != nil {
				scheduleErr.Store(err)
			}
		}()
		go func() {
			defer wg.Done()
			if _, err := store.ApplyItemPlanImmediately(ctx, tenantID, itemID, planC.ID, time.Now().UTC()); err != nil {
				immediateErr.Store(err)
			}
		}()
		wg.Wait()

		if v := scheduleErr.Load(); v != nil {
			t.Errorf("round %d: schedule error: %v", round, v)
		}
		if v := immediateErr.Load(); v != nil {
			t.Errorf("round %d: immediate error: %v", round, v)
		}

		got, err := store.GetItem(ctx, tenantID, itemID)
		if err != nil {
			t.Fatalf("round %d: get item: %v", round, err)
		}
		// Two valid end states depending on commit order:
		//   - schedule committed first, immediate committed second → plan=C,
		//     pending cleared (immediate supersedes).
		//   - immediate committed first, schedule committed second → plan=C,
		//     pending=B (the schedule simply layered on after the swap).
		// Invalid state: plan=A (the swap got lost) — this is the regression
		// we're guarding against.
		if got.PlanID == planA.ID {
			t.Errorf("round %d: immediate swap was lost; plan_id reverted to A", round)
		}
		if got.PlanID != planC.ID {
			t.Errorf("round %d: final plan_id = %q, want %q", round, got.PlanID, planC.ID)
		}
	}
}

// TestUpdate_PersistsActivationCycleAndAnchor is the regression for the
// ADR-055 e2e-audit finding: PostgresStore.Update (Service.Activate's only
// writer) historically omitted the period bounds, next_billing_at, started_at,
// and billing_anchor_day, so a draft→active anniversary sub anchored on a
// month-end day silently dropped its anchor (and period) on the real Postgres
// path — masked by the in-memory test fake that replaces the whole struct.
// All of those columns must now round-trip through Update.
func TestUpdate_PersistsActivationCycleAndAnchor(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	store := subscription.NewPostgresStore(db)
	tenantID := testutil.CreateTestTenant(t, db, "Activate Persist")

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_activate", DisplayName: "Activate Co",
	})
	if err != nil {
		t.Fatalf("customer: %v", err)
	}
	plan, err := pricing.NewPostgresStore(db).CreatePlan(ctx, tenantID, domain.Plan{
		Code: "plan_activate", Name: "Activate Plan", Currency: "USD",
		BillingInterval: domain.BillingMonthly, Status: domain.PlanActive,
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	// Draft sub: no period, anchor 0 (as Create stores it for a plain draft).
	draft, err := store.Create(ctx, tenantID, domain.Subscription{
		Code: "sub-activate", DisplayName: "Activate Sub", CustomerID: cust.ID,
		Status: domain.SubscriptionDraft, BillingTime: domain.BillingTimeAnniversary,
		Items: []domain.SubscriptionItem{{PlanID: plan.ID, Quantity: 1}},
	})
	if err != nil {
		t.Fatalf("create draft: %v", err)
	}
	if draft.BillingAnchorDay != 0 {
		t.Fatalf("draft anchor: got %d, want 0", draft.BillingAnchorDay)
	}

	// Mirror Service.Activate's writes: active + first period (Jan 31 → Feb 28,
	// the anniversary month-end clamp) + anchor 31.
	ps := time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC)
	pe := time.Date(2026, 2, 28, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 1, 31, 12, 0, 0, 0, time.UTC)
	if _, err := store.ActivateDraftWithBill(ctx, tenantID, draft.ID, now, ps, pe, 31, nil); err != nil {
		t.Fatalf("activate: %v", err)
	}

	// Re-fetch from Postgres — every activation column must have persisted.
	got, err := store.Get(ctx, tenantID, draft.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != domain.SubscriptionActive {
		t.Errorf("status: got %s, want active", got.Status)
	}
	if got.BillingAnchorDay != 31 {
		t.Errorf("billing_anchor_day: got %d, want 31 (dropped → anniversary month-end ratchet)", got.BillingAnchorDay)
	}
	if got.CurrentBillingPeriodStart == nil || !got.CurrentBillingPeriodStart.Equal(ps) {
		t.Errorf("current_billing_period_start: got %v, want %v", got.CurrentBillingPeriodStart, ps)
	}
	if got.CurrentBillingPeriodEnd == nil || !got.CurrentBillingPeriodEnd.Equal(pe) {
		t.Errorf("current_billing_period_end: got %v, want %v", got.CurrentBillingPeriodEnd, pe)
	}
	if got.NextBillingAt == nil || !got.NextBillingAt.Equal(pe) {
		t.Errorf("next_billing_at: got %v, want %v", got.NextBillingAt, pe)
	}
	if got.StartedAt == nil {
		t.Error("started_at not persisted")
	}
}

// seedActiveSubscription creates the FK chain (customer → plan → subscription)
// and returns an active subscription's ID ready for state-transition testing.
func seedActiveSubscription(t *testing.T, db *postgres.DB, tenantID, custExt, planCode, subCode string) string {
	t.Helper()
	ctx := postgres.WithLivemode(context.Background(), false)

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: custExt, DisplayName: "Transition Tester",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	plan, err := pricing.NewPostgresStore(db).CreatePlan(ctx, tenantID, domain.Plan{
		Code: planCode, Name: "Transition Plan", Currency: "USD",
		BillingInterval: domain.BillingMonthly, Status: domain.PlanActive,
		BaseAmountCents: 0,
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}

	now := time.Now().UTC()
	sub, err := subscription.NewPostgresStore(db).Create(ctx, tenantID, domain.Subscription{
		Code: subCode, DisplayName: "Transition Sub",
		CustomerID: cust.ID,
		Status:     domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
		StartedAt:                 &now,
		CurrentBillingPeriodStart: &now, CurrentBillingPeriodEnd: bivPE(now),
		Items: []domain.SubscriptionItem{{PlanID: plan.ID, Quantity: 1}},
	})
	if err != nil {
		t.Fatalf("create subscription: %v", err)
	}
	return sub.ID
}

// TestScheduleAndFireCancellation_Roundtrip exercises the full schedule →
// clear → re-schedule → fire pipeline against postgres. The contract tested:
// (1) ScheduleCancellation persists CancelAt and CancelAtPeriodEnd and a
// SELECT round-trips the same values, (2) ClearScheduledCancellation wipes
// both fields, (3) FireScheduledCancellation flips status to canceled,
// stamps canceled_at to the supplied `at` (test-clock parity), nulls out
// next_billing_at, and clears the schedule fields, and (4) firing on an
// already-canceled sub returns ErrInvalidState — the engine relies on this
// to detect concurrent immediate-cancel races and treat them as no-ops.
func TestScheduleAndFireCancellation_Roundtrip(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)

	store := subscription.NewPostgresStore(db)
	tenantID := testutil.CreateTestTenant(t, db, "Sub Schedule Cancel")
	subID := seedActiveSubscription(t, db, tenantID, "cus_sched", "plan_sched", "sub-sched")

	cancelAt := time.Now().UTC().Add(30 * 24 * time.Hour).Truncate(time.Microsecond)

	// 1. Schedule with timestamp + flag both set so we exercise both columns.
	scheduled, err := store.ScheduleCancellation(ctx, tenantID, subID, &cancelAt, true, domain.SubscriptionActive)
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	if !scheduled.CancelAtPeriodEnd {
		t.Error("CancelAtPeriodEnd should round-trip true")
	}
	if scheduled.CancelAt == nil || !scheduled.CancelAt.Equal(cancelAt) {
		t.Errorf("CancelAt round-trip: got %v, want %v", scheduled.CancelAt, cancelAt)
	}

	// SELECT path: another Get hits the read columns directly.
	read, err := store.Get(ctx, tenantID, subID)
	if err != nil {
		t.Fatalf("get after schedule: %v", err)
	}
	if !read.CancelAtPeriodEnd || read.CancelAt == nil {
		t.Errorf("schedule fields not visible to Get: %+v", read)
	}

	// 2. Clear wipes both columns.
	cleared, err := store.ClearScheduledCancellation(ctx, tenantID, subID)
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if cleared.CancelAtPeriodEnd || cleared.CancelAt != nil {
		t.Errorf("clear left schedule fields set: %+v", cleared)
	}

	// 3. Re-schedule and fire.
	if _, err := store.ScheduleCancellation(ctx, tenantID, subID, nil, true, domain.SubscriptionActive); err != nil {
		t.Fatalf("re-schedule: %v", err)
	}
	fireAt := time.Now().UTC().Truncate(time.Microsecond)
	fired, err := fireScheduledCancellation(ctx, store, tenantID, subID, fireAt)
	if err != nil {
		t.Fatalf("fire: %v", err)
	}
	if fired.Status != domain.SubscriptionCanceled {
		t.Errorf("status: got %q, want canceled", fired.Status)
	}
	if fired.CanceledAt == nil || !fired.CanceledAt.Equal(fireAt) {
		t.Errorf("canceled_at: got %v, want %v (test-clock parity)", fired.CanceledAt, fireAt)
	}
	if fired.NextBillingAt != nil {
		t.Error("next_billing_at must be nil on canceled sub")
	}
	if fired.CancelAtPeriodEnd || fired.CancelAt != nil {
		t.Errorf("schedule fields not cleared on fire: %+v", fired)
	}

	// 4. Firing again must return ErrWatermarkMoved — the closer's snapshot
	// is stale (ADR-115); the engine's bounded retry re-reads and finds the
	// sub terminated.
	_, err = fireScheduledCancellation(ctx, store, tenantID, subID, fireAt)
	if !errors.Is(err, subscription.ErrWatermarkMoved) {
		t.Fatalf("firing on an already-canceled sub: got %v, want ErrWatermarkMoved", err)
	}
}

// fireScheduledCancellation drives the ADR-115 terminal closer the way the
// engine does: a fresh snapshot, then FireScheduledCancellationTx as the
// first statement of one tenant tx.
func fireScheduledCancellation(ctx context.Context, store *subscription.PostgresStore, tenantID, subID string, at time.Time) (domain.Subscription, error) {
	fresh, err := store.Get(ctx, tenantID, subID)
	if err != nil {
		return domain.Subscription{}, err
	}
	var fired domain.Subscription
	err = store.WithTenantTx(ctx, tenantID, func(tx *sql.Tx) error {
		var ferr error
		fired, ferr = store.FireScheduledCancellationTx(ctx, tx, tenantID, subID, subscription.SnapshotOf(fresh), at)
		return ferr
	})
	return fired, err
}

// TestPauseCollection_Roundtrip exercises the full set → re-set → clear
// pipeline against postgres. The contract tested: (1) SetPauseCollection
// persists behavior + resumes_at and a SELECT round-trips both, (2) a second
// Set replaces both columns (no merge), (3) a Set with nil ResumesAt clears
// the timestamp column, (4) ClearPauseCollection wipes both columns and is
// idempotent, and (5) Set on a canceled sub is rejected.
func TestPauseCollection_Roundtrip(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)

	store := subscription.NewPostgresStore(db)
	tenantID := testutil.CreateTestTenant(t, db, "Sub Pause Collection")
	subID := seedActiveSubscription(t, db, tenantID, "cus_pc", "plan_pc", "sub-pc")

	resumesAt := time.Now().UTC().Add(7 * 24 * time.Hour).Truncate(time.Microsecond)

	// 1. Set behavior + resumes_at, both columns round-trip.
	paused, err := store.SetPauseCollection(ctx, tenantID, subID, domain.PauseCollection{
		Behavior:  domain.PauseCollectionKeepAsDraft,
		ResumesAt: &resumesAt,
	})
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if paused.PauseCollection == nil {
		t.Fatal("PauseCollection must be non-nil after set")
	}
	if paused.PauseCollection.Behavior != domain.PauseCollectionKeepAsDraft {
		t.Errorf("behavior: got %q, want %q", paused.PauseCollection.Behavior, domain.PauseCollectionKeepAsDraft)
	}
	if paused.PauseCollection.ResumesAt == nil || !paused.PauseCollection.ResumesAt.Equal(resumesAt) {
		t.Errorf("resumes_at round-trip: got %v, want %v", paused.PauseCollection.ResumesAt, resumesAt)
	}

	// SELECT path: another Get hits the read columns directly.
	read, err := store.Get(ctx, tenantID, subID)
	if err != nil {
		t.Fatalf("get after set: %v", err)
	}
	if read.PauseCollection == nil || read.PauseCollection.ResumesAt == nil {
		t.Errorf("pause_collection not visible to Get: %+v", read.PauseCollection)
	}

	// 2. Re-set with nil ResumesAt clears the timestamp column.
	paused2, err := store.SetPauseCollection(ctx, tenantID, subID, domain.PauseCollection{
		Behavior: domain.PauseCollectionKeepAsDraft,
	})
	if err != nil {
		t.Fatalf("re-set: %v", err)
	}
	if paused2.PauseCollection == nil {
		t.Fatal("PauseCollection should still be non-nil after re-set")
	}
	if paused2.PauseCollection.ResumesAt != nil {
		t.Errorf("resumes_at should be nil after re-set without it, got %v", paused2.PauseCollection.ResumesAt)
	}

	// 3. Clear wipes both columns.
	cleared, err := store.ClearPauseCollection(ctx, tenantID, subID)
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if cleared.PauseCollection != nil {
		t.Errorf("clear left pause_collection set: %+v", cleared.PauseCollection)
	}

	// 4. Clear again is a CAS miss at the store (2026-08-30, HA hazard 5):
	// nothing changed, so it reports ErrNotPaused instead of pretending it
	// cleared — the schedule callers use that to announce nothing; the
	// operator path (Service.ResumeCollection) stays idempotent by returning
	// the row unchanged.
	if _, err := store.ClearPauseCollection(ctx, tenantID, subID); !errors.Is(err, subscription.ErrNotPaused) {
		t.Fatalf("second clear: got %v, want ErrNotPaused", err)
	}
	svc := subscription.NewService(store, nil)
	if again, err := svc.ResumeCollection(ctx, tenantID, subID); err != nil || again.ID != subID || again.PauseCollection != nil {
		t.Fatalf("operator resume on an unpaused sub must be an idempotent no-op: %+v %v", again.PauseCollection, err)
	}

	// 5. Set on a canceled sub is rejected.
	if _, err := store.CancelAtomic(ctx, tenantID, subID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	_, err = store.SetPauseCollection(ctx, tenantID, subID, domain.PauseCollection{
		Behavior: domain.PauseCollectionKeepAsDraft,
	})
	if err == nil {
		t.Fatal("expected error setting pause_collection on canceled sub, got nil")
	}
}

// --- HA-readiness hazard 5 root fix (2026-08-30, ADR-114 PR-B) ---

// TestClearPauseCollection_CASOneWinner: two leaders clearing the same pause
// (a failover window, a resumed frozen process) must produce exactly ONE
// success — the winner announces `subscription.collection_resumed`, every
// other clearer gets ErrNotPaused and announces nothing. Mutation-verify:
// drop `AND pause_collection_behavior IS NOT NULL` from the UPDATE → every
// goroutine succeeds.
func TestClearPauseCollection_CASOneWinner(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	store := subscription.NewPostgresStore(db)
	tenantID := testutil.CreateTestTenant(t, db, "Pause Clear Race")
	subID := seedActiveSubscription(t, db, tenantID, "cus_pause_race", "plan_pause_race", "sub-pause-race")

	resumes := time.Now().Add(-time.Minute)
	if _, err := store.SetPauseCollection(ctx, tenantID, subID, domain.PauseCollection{Behavior: domain.PauseCollectionBehavior("keep_as_draft"), ResumesAt: &resumes}); err != nil {
		t.Fatalf("set pause: %v", err)
	}

	const goroutines = 20
	var (
		wg        sync.WaitGroup
		successes atomic.Int64
		notPaused atomic.Int64
	)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := store.ClearPauseCollection(ctx, tenantID, subID)
			switch {
			case err == nil:
				successes.Add(1)
			case errors.Is(err, subscription.ErrNotPaused):
				notPaused.Add(1)
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()
	if successes.Load() != 1 || notPaused.Load() != goroutines-1 {
		t.Fatalf("successes=%d notPaused=%d, want exactly 1 winner and %d ErrNotPaused", successes.Load(), notPaused.Load(), goroutines-1)
	}
	// And a clear on an unpaused row is ErrNotPaused, never a silent no-op.
	if _, err := store.ClearPauseCollection(ctx, tenantID, subID); !errors.Is(err, subscription.ErrNotPaused) {
		t.Fatalf("second clear: got %v, want ErrNotPaused", err)
	}
	if _, err := store.ClearPauseCollection(ctx, tenantID, "sub_does_not_exist"); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("missing row: got %v, want ErrNotFound", err)
	}
}

// TestClosePeriodTx_CASOneWinner locks ADR-115's one period guard on real
// Postgres: ClosePeriodTx proves the caller's PeriodSnapshot (status, period
// start, watermark) in the WHERE of the UPDATE that takes the row lock, so of
// N closers planning from the same snapshot exactly one commits and the rest
// write nothing. Two racing shapes: the TRUNCATION shape (an in_arrears swap
// keeps the period start and pulls the watermark in — only next_billing_at
// distinguishes the winner) and the CLOSE shape (start and watermark both
// move). A backward re-stamp from a FRESH snapshot succeeds (a swap or reset
// truncates a period legally — stale is what is refused), and a concurrent
// immediate cancel defeats every closer through the status predicate.
//
// Mutation-verify: drop `next_billing_at IS NOT DISTINCT FROM $9` → the
// truncation race has 20 winners; drop `status = $7` → the cancel subcase
// re-anchors a canceled sub.
func TestClosePeriodTx_CASOneWinner(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	store := subscription.NewPostgresStore(db)
	tenantID := testutil.CreateTestTenant(t, db, "Period CAS")

	p0 := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	p1 := p0.AddDate(0, 1, 0)

	// race runs 20 closers from ONE snapshot; racer i's target period is
	// [start(i), p1 + (i+1) months) so the row afterwards names its winner.
	// Returns the winner's index.
	race := func(t *testing.T, subID string, start func(i int) time.Time) int {
		t.Helper()
		sub, err := store.Get(ctx, tenantID, subID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		expected := subscription.SnapshotOf(sub) // {active, p0, p1}: what every racer read

		const racers = 20
		type outcome struct {
			i   int
			err error
		}
		results := make(chan outcome, racers)
		begin := make(chan struct{})
		var wg sync.WaitGroup
		for i := range racers {
			wg.Go(func() {
				<-begin
				target := p1.AddDate(0, i+1, 0)
				err := store.WithTenantTx(ctx, tenantID, func(tx *sql.Tx) error {
					return store.ClosePeriodTx(ctx, tx, tenantID, subID, expected, start(i), target, target, 1)
				})
				results <- outcome{i: i, err: err}
			})
		}
		close(begin)
		wg.Wait()
		close(results)

		winner, moved := -1, 0
		for r := range results {
			switch {
			case r.err == nil:
				if winner != -1 {
					t.Fatalf("two winners: racer %d and racer %d both committed", winner, r.i)
				}
				winner = r.i
			case errors.Is(r.err, subscription.ErrWatermarkMoved):
				moved++
			default:
				t.Fatalf("racer %d: unexpected error %v", r.i, r.err)
			}
		}
		if winner == -1 || moved != racers-1 {
			t.Fatalf("winner=%d moved=%d, want exactly one winner and %d ErrWatermarkMoved", winner, moved, racers-1)
		}
		after, err := store.Get(ctx, tenantID, subID)
		if err != nil {
			t.Fatalf("get after race: %v", err)
		}
		wantNext := p1.AddDate(0, winner+1, 0)
		if after.NextBillingAt == nil || !after.NextBillingAt.Equal(wantNext) || !after.CurrentBillingPeriodStart.Equal(start(winner)) {
			t.Fatalf("row after race = [%v, next %v), want the winner's [%v, next %v)", after.CurrentBillingPeriodStart, after.NextBillingAt, start(winner), wantNext)
		}
		return winner
	}

	t.Run("truncation shape: start kept, watermark decides", func(t *testing.T) {
		subID := seedActiveSubscription(t, db, tenantID, "cus_cas_tr", "plan_cas_tr", "sub-cas-tr")
		subscriptiontest.SetBillingCycle(t, ctx, db, tenantID, subID, p0, p1, p1, 1)
		race(t, subID, func(int) time.Time { return p0 })
	})

	t.Run("close shape: start and watermark move", func(t *testing.T) {
		subID := seedActiveSubscription(t, db, tenantID, "cus_cas_cl", "plan_cas_cl", "sub-cas-cl")
		subscriptiontest.SetBillingCycle(t, ctx, db, tenantID, subID, p0, p1, p1, 1)
		race(t, subID, func(int) time.Time { return p1 })

		after, err := store.Get(ctx, tenantID, subID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		stale := subscription.PeriodSnapshot{Start: &p0, Next: &p1, Status: domain.SubscriptionActive}

		// Backward is legal from a FRESH snapshot: a swap truncating the period.
		fresh := subscription.SnapshotOf(after)
		truncated := p1.Add(24 * time.Hour)
		if err := store.WithTenantTx(ctx, tenantID, func(tx *sql.Tx) error {
			return store.ClosePeriodTx(ctx, tx, tenantID, subID, fresh, p1, truncated, truncated, 2)
		}); err != nil {
			t.Fatalf("backward re-stamp from a fresh snapshot must succeed: %v", err)
		}
		// ...and the snapshot the racers held is still refused.
		err = store.WithTenantTx(ctx, tenantID, func(tx *sql.Tx) error {
			return store.ClosePeriodTx(ctx, tx, tenantID, subID, stale, p1, truncated, truncated, 2)
		})
		if !errors.Is(err, subscription.ErrWatermarkMoved) {
			t.Fatalf("stale snapshot after the race: got %v, want ErrWatermarkMoved", err)
		}
	})

	t.Run("immediate cancel first defeats every closer", func(t *testing.T) {
		cancelSubID := seedActiveSubscription(t, db, tenantID, "cus_cas_cxl", "plan_cas_cxl", "sub-cas-cxl")
		subscriptiontest.SetBillingCycle(t, ctx, db, tenantID, cancelSubID, p0, p1, p1, 1)
		read, err := store.Get(ctx, tenantID, cancelSubID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		snap := subscription.SnapshotOf(read)
		if _, err := store.CancelAtomicWithBill(ctx, tenantID, cancelSubID, nil); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		for i := range 3 {
			err := store.WithTenantTx(ctx, tenantID, func(tx *sql.Tx) error {
				return store.ClosePeriodTx(ctx, tx, tenantID, cancelSubID, snap, p1, p1.AddDate(0, 1, 0), p1.AddDate(0, 1, 0), 1)
			})
			if !errors.Is(err, subscription.ErrWatermarkMoved) {
				t.Fatalf("closer %d on a canceled sub: got %v, want ErrWatermarkMoved", i, err)
			}
		}
		canceled, err := store.Get(ctx, tenantID, cancelSubID)
		if err != nil {
			t.Fatalf("get canceled: %v", err)
		}
		if canceled.Status != domain.SubscriptionCanceled || !canceled.CurrentBillingPeriodStart.Equal(p0) || canceled.NextBillingAt == nil || !canceled.NextBillingAt.Equal(p1) {
			t.Fatalf("canceled sub's period must be untouched: status=%s period=[%v, next %v)", canceled.Status, canceled.CurrentBillingPeriodStart, canceled.NextBillingAt)
		}
	})
}
