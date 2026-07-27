package subscription

import (
	"context"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestGetDueBillingForTenant_ScopesToTenant is the real-Postgres proof of the
// P3 fix (audit HIGH #8): the operator-triggered POST /v1/billing/run must bill
// ONLY the caller's own tenant. GetDueBillingForTenant runs under TxTenant, so
// the mode-aware RLS policy fences the result to one tenant + livemode — a
// manual run can never observe or bill another tenant's subscriptions (the
// pre-fix RunCycle/GetDueBilling used TxBypass and swept every tenant).
//
// Mutation check (manual): switching GetDueBillingForTenant's BeginTx from
// TxTenant back to TxBypass makes tenant B's due sub appear in tenant A's
// result — the exact cross-tenant leak this test locks out.
func TestGetDueBillingForTenant_ScopesToTenant(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	store := NewPostgresStore(db)

	tenantA := testutil.CreateTestTenant(t, db, "Billing Run A")
	tenantB := testutil.CreateTestTenant(t, db, "Billing Run B")

	past := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)

	// seedDue creates a due (active, next_billing_at in the past) subscription
	// for one tenant and returns its id.
	seedDue := func(t *testing.T, tenantID, suffix string) string {
		t.Helper()
		cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
			ExternalID: "cus_" + suffix, DisplayName: suffix,
		})
		if err != nil {
			t.Fatalf("%s customer: %v", suffix, err)
		}
		plan, err := pricing.NewPostgresStore(db).CreatePlan(ctx, tenantID, domain.Plan{
			Code: "plan-" + suffix, Name: suffix, Currency: "USD",
			BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInArrears,
			BaseAmountCents: 5000, Status: domain.PlanActive,
		})
		if err != nil {
			t.Fatalf("%s plan: %v", suffix, err)
		}
		sub, err := store.Create(ctx, tenantID, domain.Subscription{
			Code: "sub-" + suffix, DisplayName: suffix, CustomerID: cust.ID,
			Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
			StartedAt: &past, NextBillingAt: &past,
			CurrentBillingPeriodStart: &past, CurrentBillingPeriodEnd: bivPE(past),
			Items: []domain.SubscriptionItem{{PlanID: plan.ID, Quantity: 1}},
		})
		if err != nil {
			t.Fatalf("%s sub: %v", suffix, err)
		}
		return sub.ID
	}

	subA := seedDue(t, tenantA, "a")
	subB := seedDue(t, tenantB, "b")

	// Tenant A's manual run sees ONLY A's due sub.
	dueA, err := store.GetDueBillingForTenant(ctx, tenantA, now, 50)
	if err != nil {
		t.Fatalf("GetDueBillingForTenant(A): %v", err)
	}
	gotA := map[string]bool{}
	for _, s := range dueA {
		gotA[s.ID] = true
	}
	if !gotA[subA] {
		t.Errorf("tenant A's own due subscription %s must be returned", subA)
	}
	if gotA[subB] {
		t.Fatalf("CROSS-TENANT LEAK: tenant B's subscription %s appeared in tenant A's billing run", subB)
	}
	if len(dueA) != 1 {
		t.Errorf("tenant A's run returned %d subs, want exactly 1 (only its own)", len(dueA))
	}

	// Symmetric: B sees only B.
	dueB, err := store.GetDueBillingForTenant(ctx, tenantB, now, 50)
	if err != nil {
		t.Fatalf("GetDueBillingForTenant(B): %v", err)
	}
	if len(dueB) != 1 || dueB[0].ID != subB {
		t.Errorf("tenant B's run must return only its own sub %s, got %+v", subB, dueB)
	}
}

// TestGetDueBillingForTenant_ExcludesClockPinnedAndNotDue locks the disjoint-flow
// + due predicates: a clock-pinned sub (operator Advance owns it) and a
// not-yet-due sub are both excluded from the manual run, mirroring GetDueBilling.
func TestGetDueBillingForTenant_ExcludesClockPinnedAndNotDue(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	store := NewPostgresStore(db)
	tenantID := testutil.CreateTestTenant(t, db, "Billing Run Excl")

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_excl", DisplayName: "Excl",
	})
	if err != nil {
		t.Fatalf("customer: %v", err)
	}
	plan, err := pricing.NewPostgresStore(db).CreatePlan(ctx, tenantID, domain.Plan{
		Code: "plan-excl", Name: "Excl", Currency: "USD",
		BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInArrears,
		BaseAmountCents: 5000, Status: domain.PlanActive,
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	past := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	future := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)

	mk := func(code string, next time.Time) string {
		s, err := store.Create(ctx, tenantID, domain.Subscription{
			Code: code, DisplayName: code, CustomerID: cust.ID,
			Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
			StartedAt: &past, NextBillingAt: &next,
			CurrentBillingPeriodStart: &past, CurrentBillingPeriodEnd: bivPE(past),
			Items: []domain.SubscriptionItem{{PlanID: plan.ID, Quantity: 1}},
		})
		if err != nil {
			t.Fatalf("create %s: %v", code, err)
		}
		return s.ID
	}
	dueSub := mk("sub-due", past)
	notDue := mk("sub-notdue", future)
	clockPinned := mk("sub-clock", past)

	// Pin one sub to a test clock via raw SQL (real test_clocks row).
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO test_clocks (id, tenant_id, name, frozen_time, status, livemode)
		 VALUES ('tc_excl', $1, 'excl', $2, 'ready', false)`, tenantID, now); err != nil {
		t.Fatalf("seed test clock: %v", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE subscriptions SET test_clock_id = 'tc_excl' WHERE id = $1`, clockPinned); err != nil {
		t.Fatalf("pin sub to clock: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	due, err := store.GetDueBillingForTenant(ctx, tenantID, now, 50)
	if err != nil {
		t.Fatalf("GetDueBillingForTenant: %v", err)
	}
	got := map[string]bool{}
	for _, s := range due {
		got[s.ID] = true
	}
	if !got[dueSub] {
		t.Errorf("the due, non-clock-pinned sub %s must be returned", dueSub)
	}
	if got[notDue] {
		t.Errorf("a not-yet-due sub %s must be excluded", notDue)
	}
	if got[clockPinned] {
		t.Errorf("a clock-pinned sub %s must be excluded (operator Advance owns it, ADR-028)", clockPinned)
	}
}
