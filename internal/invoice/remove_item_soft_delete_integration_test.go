package invoice_test

import (
	"context"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/subscription"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestRemoveItem_SoftDeleteIsFKSafeAndReAddable is FLOW B3 item #10: RemoveItem
// SOFT-deletes the subscription_item (sets deleted_at) instead of hard-deleting
// it. An invoice whose source_subscription_item_id FK-references the item is
// declared ON DELETE RESTRICT (migration 0095/0103), so a HARD delete would be
// blocked (FK violation → 500) and orphan the customer's proration history. The
// soft-delete keeps the row alive, so:
//   - RemoveItem returns cleanly (not blocked by the RESTRICT FK),
//   - the referencing invoice survives (no orphan, no cascade),
//   - the item drops out of the live item set (deleted_at IS NULL filter), and
//   - the partial unique index (subscription_id, plan_id) WHERE deleted_at IS NULL
//     allows re-adding the same plan afterwards.
//
// The mem store can't model the FK or the partial index, so this must run on real
// Postgres.
func TestRemoveItem_SoftDeleteIsFKSafeAndReAddable(t *testing.T) {
	db := testutil.SetupTestDB(t) // skips on -short
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "RemoveItem Soft Delete")

	custStore := customer.NewPostgresStore(db)
	planStore := pricing.NewPostgresStore(db)
	subStore := subscription.NewPostgresStore(db)
	invStore := invoice.NewPostgresStore(db)

	cust, err := custStore.Create(ctx, tenantID, domain.Customer{ExternalID: "cus_softdel", DisplayName: "Soft Del"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	// Two distinct plans → a sub with two items (an active sub refuses to drop its
	// last item, so we remove one and keep the other).
	planA, err := planStore.CreatePlan(ctx, tenantID, domain.Plan{Code: "sd-a", Name: "A", Currency: "USD", BillingInterval: domain.BillingMonthly, Status: domain.PlanActive, BaseAmountCents: 1000})
	if err != nil {
		t.Fatalf("create plan A: %v", err)
	}
	planB, err := planStore.CreatePlan(ctx, tenantID, domain.Plan{Code: "sd-b", Name: "B", Currency: "USD", BillingInterval: domain.BillingMonthly, Status: domain.PlanActive, BaseAmountCents: 2000})
	if err != nil {
		t.Fatalf("create plan B: %v", err)
	}

	periodStart := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	sub, err := subStore.Create(ctx, tenantID, domain.Subscription{
		Code: "sub-softdel", DisplayName: "Soft Del", CustomerID: cust.ID,
		Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar, StartedAt: &periodStart,
		CurrentBillingPeriodStart: &periodStart, CurrentBillingPeriodEnd: bivPE(periodStart),
		Items: []domain.SubscriptionItem{{PlanID: planA.ID, Quantity: 1}, {PlanID: planB.ID, Quantity: 1}},
	})
	if err != nil {
		t.Fatalf("create sub: %v", err)
	}
	got, err := subStore.Get(ctx, tenantID, sub.ID)
	if err != nil {
		t.Fatalf("get sub: %v", err)
	}
	var itemA string
	for _, it := range got.Items {
		if it.PlanID == planA.ID {
			itemA = it.ID
		}
	}
	if itemA == "" {
		t.Fatal("item A not found in seeded sub")
	}

	// A proration invoice FK-referencing item A (ON DELETE RESTRICT). A hard delete
	// of item A would be blocked by THIS row.
	at := periodStart.Add(10 * 24 * time.Hour)
	refInv, err := invStore.Create(ctx, tenantID, domain.Invoice{
		CustomerID: cust.ID, SubscriptionID: sub.ID,
		InvoiceNumber: "SD-PRO-A", Status: domain.InvoiceDraft, PaymentStatus: domain.PaymentPending, Currency: "USD",
		BillingPeriodStart: periodStart, BillingPeriodEnd: periodStart.AddDate(0, 1, 0),
		BillingReason:       domain.BillingReasonSubscriptionUpdate,
		SourcePlanChangedAt: &at, SourceSubscriptionItemID: itemA, SourceChangeType: domain.ItemChangeTypeAdd,
		IssuedAt: &at, DueAt: &at, NetPaymentTermDays: 0,
	})
	if err != nil {
		t.Fatalf("create referencing invoice: %v", err)
	}

	// (1) RemoveItem must SUCCEED via soft-delete — a hard DELETE would be blocked
	// by the ON DELETE RESTRICT FK from refInv above.
	if err := subStore.RemoveItem(ctx, tenantID, itemA); err != nil {
		t.Fatalf("RemoveItem must succeed via soft-delete (a hard delete is blocked by the RESTRICT FK): %v", err)
	}

	// (2) The referencing invoice survives — the item row was soft-deleted, not
	// hard-deleted, so its FK back-pointer stays valid (no orphan, no cascade).
	if _, err := invStore.Get(ctx, tenantID, refInv.ID); err != nil {
		t.Errorf("referencing invoice must survive the soft-delete (FK intact): %v", err)
	}

	// (3) The soft-deleted item drops out of the live item set.
	afterRemove, err := subStore.Get(ctx, tenantID, sub.ID)
	if err != nil {
		t.Fatalf("get after remove: %v", err)
	}
	for _, it := range afterRemove.Items {
		if it.ID == itemA {
			t.Errorf("soft-deleted item A must be hidden from the live item set, but it is still present")
		}
	}

	// (4) Re-adding the same plan succeeds — the partial unique index
	// (subscription_id, plan_id) WHERE deleted_at IS NULL ignores the dead row.
	if _, err := subStore.AddItem(ctx, tenantID, domain.SubscriptionItem{SubscriptionID: sub.ID, PlanID: planA.ID, Quantity: 1}); err != nil {
		t.Errorf("re-adding plan A after soft-delete must succeed (partial unique index WHERE deleted_at IS NULL): %v", err)
	}
}
