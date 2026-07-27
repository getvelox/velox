package invoice_test

import (
	"context"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/subscription"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestProrationInvoiceIndexes is FLOW B3 item #2: two proration invoices from item
// changes in the SAME billing period must BOTH persist — the billing-idempotency
// index (idx_invoices_billing_idempotency) EXEMPTS proration via its
// `source_plan_changed_at IS NULL` predicate, so two prorations sharing a period
// don't falsely collide. They're distinguished by source_subscription_item_id.
// A duplicate of the SAME (item, change-type, change-instant) tuple IS rejected
// by idx_invoices_proration_dedup (mapped to code invoice_proration_source_taken).
func TestProrationInvoiceIndexes(t *testing.T) {
	db := testutil.SetupTestDB(t) // skips on -short
	ctx := postgres.WithLivemode(context.Background(), false)

	custStore := customer.NewPostgresStore(db)
	planStore := pricing.NewPostgresStore(db)
	subStore := subscription.NewPostgresStore(db)
	invStore := invoice.NewPostgresStore(db)

	tenantID := testutil.CreateTestTenant(t, db, "Proration Index Corp")
	cust, err := custStore.Create(ctx, tenantID, domain.Customer{ExternalID: "cus_prorate", DisplayName: "Prorate Cust"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	// Two distinct plans → a sub with two real items (the FK targets).
	planA, err := planStore.CreatePlan(ctx, tenantID, domain.Plan{Code: "a", Name: "A", Currency: "USD", BillingInterval: domain.BillingMonthly, Status: domain.PlanActive, BaseAmountCents: 1000})
	if err != nil {
		t.Fatalf("create plan A: %v", err)
	}
	planB, err := planStore.CreatePlan(ctx, tenantID, domain.Plan{Code: "b", Name: "B", Currency: "USD", BillingInterval: domain.BillingMonthly, Status: domain.PlanActive, BaseAmountCents: 2000})
	if err != nil {
		t.Fatalf("create plan B: %v", err)
	}

	periodStart := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := periodStart.AddDate(0, 1, 0)
	sub, err := subStore.Create(ctx, tenantID, domain.Subscription{
		Code: "sub-prorate", DisplayName: "Prorate Sub", CustomerID: cust.ID,
		Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar, StartedAt: &periodStart,
		CurrentBillingPeriodStart: &periodStart, CurrentBillingPeriodEnd: bivPE(periodStart),
		Items: []domain.SubscriptionItem{{PlanID: planA.ID, Quantity: 1}, {PlanID: planB.ID, Quantity: 1}},
	})
	if err != nil {
		t.Fatalf("create subscription: %v", err)
	}
	got, err := subStore.Get(ctx, tenantID, sub.ID)
	if err != nil {
		t.Fatalf("get sub: %v", err)
	}
	if len(got.Items) != 2 {
		t.Fatalf("expected 2 subscription items, got %d", len(got.Items))
	}
	itemA, itemB := got.Items[0].ID, got.Items[1].ID

	changedAt := periodStart.Add(10 * 24 * time.Hour) // a mid-period change instant
	prorate := func(num, itemID string) domain.Invoice {
		at := changedAt
		return domain.Invoice{
			CustomerID: cust.ID, SubscriptionID: sub.ID,
			InvoiceNumber: num, Status: domain.InvoiceDraft, PaymentStatus: domain.PaymentPending, Currency: "USD",
			BillingPeriodStart: periodStart, BillingPeriodEnd: periodEnd,
			BillingReason:       domain.BillingReasonSubscriptionUpdate,
			SourcePlanChangedAt: &at, SourceSubscriptionItemID: itemID, SourceChangeType: domain.ItemChangeTypeAdd,
			IssuedAt: &at, DueAt: &at, NetPaymentTermDays: 0,
		}
	}

	// 1. Two prorations, same period, DIFFERENT items → BOTH persist. If the
	//    billing-idempotency index did NOT exempt proration, the second would
	//    collide on (tenant, sub, period).
	if _, err := invStore.Create(ctx, tenantID, prorate("VLX-PRO-A", itemA)); err != nil {
		t.Fatalf("proration A: %v", err)
	}
	if _, err := invStore.Create(ctx, tenantID, prorate("VLX-PRO-B", itemB)); err != nil {
		t.Fatalf("proration B (different item, SAME period) must NOT collide — billing-idempotency exempts proration: %v", err)
	}

	// 2. The SAME (item, change-type, change-instant) tuple again → deduped by
	//    idx_invoices_proration_dedup (distinct invoice number, so it is the
	//    proration-source index that fires, not the invoice-number one).
	_, err = invStore.Create(ctx, tenantID, prorate("VLX-PRO-A-DUP", itemA))
	if err == nil {
		t.Fatal("duplicate proration for the same item+change should be rejected by idx_invoices_proration_dedup")
	}
	if code := errs.Code(err); code != "invoice_proration_source_taken" {
		t.Fatalf("dedup error code: got %q, want invoice_proration_source_taken", code)
	}

	// Exactly the two distinct prorations persisted.
	_, total, err := invStore.List(ctx, invoice.ListFilter{TenantID: tenantID})
	if err != nil {
		t.Fatalf("list invoices: %v", err)
	}
	if total != 2 {
		t.Fatalf("expected exactly 2 proration invoices, got %d", total)
	}
}
