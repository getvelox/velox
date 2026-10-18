package billing_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/credit"
	"github.com/sagarsuperuser/velox/internal/creditnote"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/subscription"
	"github.com/sagarsuperuser/velox/internal/subscription/subscriptiontest"
	"github.com/sagarsuperuser/velox/internal/tenant"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

type swapLockPlanReader struct{ plans map[string]domain.Plan }

func (r *swapLockPlanReader) GetPlan(_ context.Context, _, id string) (domain.Plan, error) {
	p, ok := r.plans[id]
	if !ok {
		return domain.Plan{}, errors.New("plan not found")
	}
	return p, nil
}

// An immediate monthly→yearly swap of a paid in-advance plan writes a relief
// credit note AND the new period's invoice on one transaction. Both take a
// number from the tenant's sequence row, so both must take it on that
// transaction: a number asked for on a second connection waits for the row the
// swap itself holds, until the request times out.
func TestCrossIntervalSwap_ReliefAndNewInvoiceNumberOnOneTx(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)

	customerStore := customer.NewPostgresStore(db)
	pricingStore := pricing.NewPostgresStore(db)
	subStore := subscription.NewPostgresStore(db)
	invoiceStore := invoice.NewPostgresStore(db)
	creditSvc := credit.NewService(credit.NewPostgresStore(db))
	settingsStore := tenant.NewSettingsStore(db)

	tenantID := testutil.CreateTestTenant(t, db, "Swap Number Lock")
	monthly, err := pricingStore.CreatePlan(ctx, tenantID, domain.Plan{
		Code: "pro-monthly-lock", Name: "Pro monthly", Currency: "USD",
		BillingInterval: domain.BillingMonthly, Status: domain.PlanActive,
		BaseAmountCents: 4900, BaseBillTiming: domain.BillInAdvance,
	})
	if err != nil {
		t.Fatalf("create monthly plan: %v", err)
	}
	yearly, err := pricingStore.CreatePlan(ctx, tenantID, domain.Plan{
		Code: "pro-yearly-lock", Name: "Pro yearly", Currency: "USD",
		BillingInterval: domain.BillingYearly, Status: domain.PlanActive,
		BaseAmountCents: 49000, BaseBillTiming: domain.BillInAdvance,
	})
	if err != nil {
		t.Fatalf("create yearly plan: %v", err)
	}
	periodStart := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	swapAt := time.Date(2026, 6, 16, 0, 0, 0, 0, time.UTC)

	cnSvc := creditnote.NewService(creditnote.NewPostgresStore(db), invoiceStore, nil)
	cnSvc.SetNumberGenerator(settingsStore)

	e := newRealEngine(db, clock.NewFake(periodStart.Add(time.Hour)))
	e.SetCreditGranter(creditSvc)
	e.SetCreditNoteAdjuster(cnSvc)
	e.SetCreditHeadroomReader(cnSvc)

	cust, err := customerStore.Create(ctx, tenantID, domain.Customer{ExternalID: "cus_swap_lock", DisplayName: "Swap Lock"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	sub, err := subStore.Create(ctx, tenantID, domain.Subscription{
		Code: "sub-swap-lock", DisplayName: "Swap Lock", CustomerID: cust.ID,
		Items:  []domain.SubscriptionItem{{PlanID: monthly.ID, Quantity: 1}},
		Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
		StartedAt:                 &periodStart,
		CurrentBillingPeriodStart: &periodStart, CurrentBillingPeriodEnd: bivPE(periodStart),
	})
	if err != nil {
		t.Fatalf("create sub: %v", err)
	}
	subscriptiontest.SetBillingCycle(t, ctx, db, tenantID, sub.ID, periodStart, periodEnd, periodEnd, 0)
	sub, err = subStore.Get(ctx, tenantID, sub.ID)
	if err != nil {
		t.Fatalf("get sub: %v", err)
	}
	funding, err := e.BillOnCreate(ctx, sub)
	if err != nil {
		t.Fatalf("BillOnCreate: %v", err)
	}
	if _, err := invoiceStore.MarkPaid(ctx, tenantID, funding.ID, "pi_swap_lock", periodStart); err != nil {
		t.Fatalf("mark paid: %v", err)
	}

	subSvc := subscription.NewService(subStore, clock.NewFake(swapAt))
	subSvc.SetPlanReader(&swapLockPlanReader{plans: map[string]domain.Plan{monthly.ID: monthly, yearly.ID: yearly}})
	subSvc.SetBiller(e)

	// A request-sized deadline: the swap itself takes milliseconds.
	swapCtx, cancel := context.WithTimeout(clock.WithEffectiveNow(ctx, swapAt), 5*time.Second)
	defer cancel()
	tx, err := db.BeginTx(swapCtx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin swap tx: %v", err)
	}
	defer postgres.Rollback(tx)
	res, err := subSvc.UpdateItemTx(swapCtx, tx, tenantID, sub.ID, sub.Items[0].ID, subscription.UpdateItemInput{
		NewPlanID: yearly.ID, Immediate: true,
	})
	if err != nil {
		t.Fatalf("swap: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit swap: %v", err)
	}
	if res.Item.PlanID != yearly.ID {
		t.Fatalf("item plan = %s, want the yearly plan", res.Item.PlanID)
	}

	notes, err := cnSvc.List(ctx, creditnote.ListFilter{TenantID: tenantID, InvoiceID: funding.ID})
	if err != nil {
		t.Fatalf("list credit notes: %v", err)
	}
	if len(notes) != 1 {
		t.Fatalf("relief credit notes on the old period's invoice = %d, want 1", len(notes))
	}
}
