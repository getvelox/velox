package subscription_test

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
	"github.com/sagarsuperuser/velox/internal/subscription"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// swapPlanReader is a subscription.PlanReader over a fixed plan map — lets the
// service route the swap without a real pricing adapter. The plan IDs are the
// real ones created in the pricing store (so the item's FK to plans holds).
type swapPlanReader struct{ plans map[string]domain.Plan }

func (r *swapPlanReader) GetPlan(_ context.Context, _, id string) (domain.Plan, error) {
	p, ok := r.plans[id]
	if !ok {
		return domain.Plan{}, errors.New("plan not found")
	}
	return p, nil
}

// billOnCreateTxErrBiller implements subscription.Biller and fails the in-tx
// new-period bill — the exact in-tx failure the atomic swap must roll back from.
type billOnCreateTxErrBiller struct{ err error }

func (b *billOnCreateTxErrBiller) BillOnCreate(context.Context, domain.Subscription) (domain.Invoice, error) {
	return domain.Invoice{}, nil
}
func (b *billOnCreateTxErrBiller) BillFinalOnImmediateCancel(context.Context, domain.Subscription) (domain.Invoice, error) {
	return domain.Invoice{}, nil
}
func (b *billOnCreateTxErrBiller) BillFinalOnImmediateCancelTx(context.Context, *sql.Tx, domain.Subscription) (domain.Invoice, error) {
	return domain.Invoice{}, nil
}
func (b *billOnCreateTxErrBiller) BillOnCancel(context.Context, domain.Subscription) (int64, error) {
	return 0, nil
}
func (b *billOnCreateTxErrBiller) BillOnCancelDraftsTx(context.Context, *sql.Tx, domain.Subscription) ([]string, int64, bool, error) {
	return nil, 0, false, nil
}
func (b *billOnCreateTxErrBiller) IssueCancelDrafts(context.Context, domain.Subscription, []string) {
}
func (b *billOnCreateTxErrBiller) BillOnPlanSwapImmediate(context.Context, domain.Subscription, time.Time) (int64, error) {
	return 0, nil
}
func (b *billOnCreateTxErrBiller) BillOnPlanSwapDraftsTx(context.Context, *sql.Tx, domain.Subscription, time.Time) ([]string, int64, bool, error) {
	return nil, 0, false, nil
}
func (b *billOnCreateTxErrBiller) IssueSwapDrafts(context.Context, domain.Subscription, []string) {
}
func (b *billOnCreateTxErrBiller) BillOnCreateTx(context.Context, *sql.Tx, domain.Subscription) (domain.Invoice, bool, error) {
	return domain.Invoice{}, false, b.err
}
func (b *billOnCreateTxErrBiller) FinalizeOnCreateInvoice(context.Context, domain.Subscription, domain.Invoice) {
}
func (b *billOnCreateTxErrBiller) ThresholdBilledThroughTx(context.Context, *sql.Tx, domain.Subscription) (*time.Time, error) {
	return nil, nil
}

// TestUpdateItemTx_CrossIntervalSwap_RealTxRollsBackOnBillFailure is the
// real-Postgres proof of the ADR-056 atomicity guarantee: when the in-tx
// new-period bill fails, the plan write AND the watermark advance — both REAL
// writes on the caller's tx — must roll back, so the subscription is left
// exactly as it was (no silent revenue drop, no half-applied swap). The
// in-memory unit test (TestUpdateItemTx_CrossIntervalAtomic) can only prove the
// error propagates; this proves the rollback against real tx semantics.
func TestUpdateItemTx_CrossIntervalSwap_RealTxRollsBackOnBillFailure(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Cross-interval Swap Rollback")

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_swap_rb", DisplayName: "Swap RB",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	ps := pricing.NewPostgresStore(db)
	oldPlan, err := ps.CreatePlan(ctx, tenantID, domain.Plan{
		Code: "old-yearly-adv", Name: "Old Yearly", Currency: "USD",
		BillingInterval: domain.BillingYearly, BaseBillTiming: domain.BillInAdvance,
		BaseAmountCents: 120000, Status: domain.PlanActive,
	})
	if err != nil {
		t.Fatalf("create old plan: %v", err)
	}
	newPlan, err := ps.CreatePlan(ctx, tenantID, domain.Plan{
		Code: "new-monthly-adv", Name: "New Monthly", Currency: "USD",
		BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInAdvance,
		BaseAmountCents: 12000, Status: domain.PlanActive,
	})
	if err != nil {
		t.Fatalf("create new plan: %v", err)
	}

	store := subscription.NewPostgresStore(db)
	start := time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC) // microsecond-clean for round-trip equality
	end := start.AddDate(1, 0, 0)
	created, err := store.Create(ctx, tenantID, domain.Subscription{
		Code: "sub-swap-rb", DisplayName: "Swap RB", CustomerID: cust.ID,
		Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeAnniversary,
		StartedAt:                 &start,
		CurrentBillingPeriodStart: &start,
		CurrentBillingPeriodEnd:   &end,
		NextBillingAt:             &end,
		Items:                     []domain.SubscriptionItem{{PlanID: oldPlan.ID, Quantity: 1}},
	})
	if err != nil {
		t.Fatalf("create sub: %v", err)
	}
	// Re-read so item IDs are reliably populated.
	sub, err := store.Get(ctx, tenantID, created.ID)
	if err != nil {
		t.Fatalf("get created sub: %v", err)
	}
	if len(sub.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(sub.Items))
	}
	itemID := sub.Items[0].ID

	svc := subscription.NewService(store, nil)
	svc.SetPlanReader(&swapPlanReader{plans: map[string]domain.Plan{oldPlan.ID: oldPlan, newPlan.ID: newPlan}})
	svc.SetBiller(&billOnCreateTxErrBiller{err: errors.New("simulated new-period bill failure")}) // BillOnCreateTx fails in-tx

	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	_, uerr := svc.UpdateItemTx(ctx, tx, tenantID, sub.ID, itemID, subscription.UpdateItemInput{
		NewPlanID: newPlan.ID, Immediate: true,
	})
	if uerr == nil {
		_ = tx.Rollback()
		t.Fatal("expected UpdateItemTx to fail when the in-tx new-period bill errors")
	}
	// Mirror the handler's deferred Rollback on a returned error.
	if rbErr := tx.Rollback(); rbErr != nil {
		t.Fatalf("rollback: %v", rbErr)
	}

	// Fresh read from a new connection: the plan write + watermark advance that
	// applyCrossIntervalPlanSwapTx performed on the tx MUST be gone.
	after, err := store.Get(ctx, tenantID, sub.ID)
	if err != nil {
		t.Fatalf("get after rollback: %v", err)
	}
	if len(after.Items) != 1 || after.Items[0].PlanID != oldPlan.ID {
		t.Errorf("plan write must roll back: item plan got %q want %q", after.Items[0].PlanID, oldPlan.ID)
	}
	if after.CurrentBillingPeriodStart == nil || !after.CurrentBillingPeriodStart.Equal(start) {
		t.Errorf("period_start must be unchanged: got %v want %v", after.CurrentBillingPeriodStart, start)
	}
	if after.CurrentBillingPeriodEnd == nil || !after.CurrentBillingPeriodEnd.Equal(end) {
		t.Errorf("watermark period_end must roll back: got %v want %v", after.CurrentBillingPeriodEnd, end)
	}
	if after.NextBillingAt == nil || !after.NextBillingAt.Equal(end) {
		t.Errorf("next_billing_at must roll back: got %v want %v", after.NextBillingAt, end)
	}
}
