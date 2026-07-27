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

// TestCancelAtomicWithBill_BillFailure_RealTxRollsBackCancel is the real-Postgres
// proof of the ADR-056 cancel atomicity: CancelAtomicWithBill flips the sub to
// canceled AND runs billFn (the final-on-cancel partial-period invoice insert)
// in ONE tx, so a billFn failure must roll the CANCEL back — no canceled sub
// with an uninvoiced partial period (a revenue leak; there is no final-on-cancel
// reconciler). Only a real tx proves the status flip is undone.
func TestCancelAtomicWithBill_BillFailure_RealTxRollsBackCancel(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Cancel Atomic Rollback")

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_cancel_rb", DisplayName: "Cancel RB",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	plan, err := pricing.NewPostgresStore(db).CreatePlan(ctx, tenantID, domain.Plan{
		Code: "cancel-rb-monthly-adv", Name: "Cancel RB", Currency: "USD",
		BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInAdvance,
		BaseAmountCents: 5000, Status: domain.PlanActive,
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}

	store := NewPostgresStore(db)
	created, err := store.Create(ctx, tenantID, domain.Subscription{
		Code: "sub-cancel-rb", DisplayName: "Cancel RB", CustomerID: cust.ID,
		Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
		CurrentBillingPeriodStart: bivPS(time.Now().UTC()), CurrentBillingPeriodEnd: bivPE(time.Now().UTC()),
		Items: []domain.SubscriptionItem{{PlanID: plan.ID, Quantity: 1}},
	})
	if err != nil {
		t.Fatalf("create sub: %v", err)
	}

	billErr := errors.New("simulated in-tx final-on-cancel bill failure")
	billFnCalled := false
	_, err = store.CancelAtomicWithBill(ctx, tenantID, created.ID, func(tx *sql.Tx, canceled domain.Subscription) error {
		billFnCalled = true
		if canceled.Status != domain.SubscriptionCanceled {
			t.Errorf("billFn should see the in-tx-canceled sub; status=%q", canceled.Status)
		}
		return billErr
	})
	if !errors.Is(err, billErr) {
		t.Fatalf("CancelAtomicWithBill must surface the billFn error, got %v", err)
	}
	if !billFnCalled {
		t.Fatal("billFn was never invoked — the test is vacuous")
	}

	// The real assertion: the cancel flip MUST have rolled back — a fresh read
	// still sees the subscription ACTIVE, not canceled.
	after, err := store.Get(ctx, tenantID, created.ID)
	if err != nil {
		t.Fatalf("get after rollback: %v", err)
	}
	if after.Status != domain.SubscriptionActive {
		t.Fatalf("cancel must roll back on a final-on-cancel bill failure; status=%q, want active (canceled sub with no final invoice = revenue leak)", after.Status)
	}
	if after.CanceledAt != nil {
		t.Errorf("canceled_at must be unset after rollback; got %v", after.CanceledAt)
	}
}
