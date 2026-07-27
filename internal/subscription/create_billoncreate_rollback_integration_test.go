package subscription

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestCreateWithBill_BillFailure_RealTxRollsBackSubscription is the real-Postgres
// proof of the ADR-056 create atomicity: CreateWithBill inserts the subscription
// (+ items) and runs billFn (the day-1 in_advance invoice insert) in ONE tx, so
// a billFn failure must roll the WHOLE create back — no orphaned active
// subscription with a missing first-period invoice (a permanent revenue leak,
// since the cycle scheduler skips the just-elapsed in_advance segment). The
// in-memory store can't model tx rollback, so only a real tx proves the INSERT
// is undone.
func TestCreateWithBill_BillFailure_RealTxRollsBackSubscription(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Create Atomic Rollback")

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_create_rb", DisplayName: "Create RB",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	// subscription_items has an FK to plans, so the item insert needs a real plan.
	plan, err := pricing.NewPostgresStore(db).CreatePlan(ctx, tenantID, domain.Plan{
		Code: "create-rb-monthly-adv", Name: "Create RB", Currency: "USD",
		BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInAdvance,
		BaseAmountCents: 5000, Status: domain.PlanActive,
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}

	store := NewPostgresStore(db)
	billErr := errors.New("simulated in-tx day-1 bill failure")
	billFnCalled := false
	_, err = store.CreateWithBill(ctx, tenantID, domain.Subscription{
		Code: "sub-create-rb", DisplayName: "Create RB", CustomerID: cust.ID,
		Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
		CurrentBillingPeriodStart: bivPS(time.Now().UTC()), CurrentBillingPeriodEnd: bivPE(time.Now().UTC()),
		Items: []domain.SubscriptionItem{{PlanID: plan.ID, Quantity: 1}},
	}, func(tx *sql.Tx, created domain.Subscription) error {
		// The sub row + items were inserted on THIS tx; returning an error must
		// roll them back. created carries the assigned id so the closure could
		// reference it — but the property under test is the rollback.
		billFnCalled = true
		if created.ID == "" {
			t.Errorf("billFn should see the in-tx-created subscription with an id")
		}
		return billErr
	})
	if !errors.Is(err, billErr) {
		t.Fatalf("CreateWithBill must surface the billFn error, got %v", err)
	}
	if !billFnCalled {
		t.Fatal("billFn was never invoked — the test is vacuous")
	}

	// The real assertion: the subscription insert (+ its item) MUST have rolled
	// back. A fresh read sees no subscription for this tenant.
	subs, total, err := store.List(ctx, ListFilter{TenantID: tenantID})
	if err != nil {
		t.Fatalf("list after rollback: %v", err)
	}
	if total != 0 || len(subs) != 0 {
		t.Fatalf("subscription must roll back on a day-1 bill failure; got total=%d len=%d (orphaned sub with no invoice = revenue leak)", total, len(subs))
	}
}
