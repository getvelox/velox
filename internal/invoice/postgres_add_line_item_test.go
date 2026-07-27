package invoice_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/subscription"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestAddLineItemAtomic_ConcurrentAdds is the regression test for COR-4a:
// the previous implementation issued CreateLineItem, ListLineItems, and
// UpdateTotals as separate transactions, so two goroutines adding line
// items at the same time could each read the subtotal before the other
// wrote, then both write back a stale sum — a classic lost-update race.
// The atomic version locks the invoice row FOR UPDATE, so the second
// caller blocks until the first commits and sees the updated subtotal.
func TestAddLineItemAtomic_ConcurrentAdds(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)

	store := invoice.NewPostgresStore(db)
	tenantID := testutil.CreateTestTenant(t, db, "AddLineItem Concurrency")
	invID := seedDraftInvoice(t, db, tenantID)

	const (
		goroutines = 8
		perG       = 5 // each goroutine appends 5 lines
		unitCents  = int64(100)
		qty        = int64(1)
	)

	var wg sync.WaitGroup
	errCh := make(chan error, goroutines*perG)

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for j := 0; j < perG; j++ {
				_, _, err := store.AddLineItemAtomic(ctx, tenantID, invID, domain.InvoiceLineItem{
					LineType:         domain.LineTypeAddOn,
					Description:      "concurrent add",
					Quantity:         qty,
					UnitAmountCents:  unitCents,
					AmountCents:      qty * unitCents,
					TotalAmountCents: qty * unitCents,
				})
				if err != nil {
					errCh <- err
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent add: %v", err)
	}

	inv, err := store.Get(ctx, tenantID, invID)
	if err != nil {
		t.Fatalf("get final invoice: %v", err)
	}

	expected := int64(goroutines*perG) * qty * unitCents
	if inv.SubtotalCents != expected {
		t.Fatalf("lost-update race: subtotal = %d, want %d (concurrent writes were not serialized)",
			inv.SubtotalCents, expected)
	}
	if inv.TotalAmountCents != expected {
		t.Fatalf("total_amount_cents = %d, want %d", inv.TotalAmountCents, expected)
	}
	if inv.AmountDueCents != expected {
		t.Fatalf("amount_due_cents = %d, want %d", inv.AmountDueCents, expected)
	}

	items, err := store.ListLineItems(ctx, tenantID, invID)
	if err != nil {
		t.Fatalf("list line items: %v", err)
	}
	if len(items) != goroutines*perG {
		t.Fatalf("line item count = %d, want %d", len(items), goroutines*perG)
	}
}

// TestPostgresLineItem_TaxabilityReasonRoundTrip is the regression test for
// issue #4 at the persistence boundary: the per-line `tax_reason` column
// stores Stripe's structured taxability_reason verbatim, and ListLineItems
// reads it back into the domain struct without normalization. The end-to-end
// chain is tested separately in the engine; this test pins the SQL itself.
func TestPostgresLineItem_TaxabilityReasonRoundTrip(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)

	store := invoice.NewPostgresStore(db)
	tenantID := testutil.CreateTestTenant(t, db, "TaxReason RoundTrip")
	invID := seedDraftInvoice(t, db, tenantID)

	// Three lines, each carrying a different Stripe-canonical reason to
	// prove the column doesn't collapse them onto a single value, and to
	// cover the cases that drive different PDF legends (customer_exempt
	// triggers the exemption legend; standard_rated and not_collecting
	// don't, which the dashboard relies on).
	want := []string{"customer_exempt", "standard_rated", "not_collecting"}
	for _, reason := range want {
		_, err := store.CreateLineItem(ctx, tenantID, domain.InvoiceLineItem{
			InvoiceID:        invID,
			LineType:         domain.LineTypeBaseFee,
			Description:      "line " + reason,
			Quantity:         1,
			UnitAmountCents:  100,
			AmountCents:      100,
			TotalAmountCents: 100,
			Currency:         "USD",
			TaxabilityReason: reason,
		})
		if err != nil {
			t.Fatalf("create line item with reason %q: %v", reason, err)
		}
	}

	got, err := store.ListLineItems(ctx, tenantID, invID)
	if err != nil {
		t.Fatalf("list line items: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("ListLineItems returned %d items, want %d", len(got), len(want))
	}
	gotReasons := make([]string, len(got))
	for i, it := range got {
		gotReasons[i] = it.TaxabilityReason
	}
	// CreateLineItem orders by created_at ASC, so we expect the same order.
	for i, w := range want {
		if gotReasons[i] != w {
			t.Errorf("line %d TaxabilityReason = %q, want %q (column must round-trip the Stripe-canonical reason)", i, gotReasons[i], w)
		}
	}
}

// TestPostgresLineItem_QuantityDecimalRoundTrip is the regression test for the
// fractional-usage quantity fix: a usage line's exact decimal quantity (e.g.
// 1.5 GPU-hours) must survive the store round-trip, while the integer Quantity
// column keeps the truncated value for back-compat (Stripe quantity_decimal
// parity). The line amount stays whole cents.
func TestPostgresLineItem_QuantityDecimalRoundTrip(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)

	store := invoice.NewPostgresStore(db)
	tenantID := testutil.CreateTestTenant(t, db, "QtyDecimal RoundTrip")
	invID := seedDraftInvoice(t, db, tenantID)

	qty := decimal.RequireFromString("1.5")
	if _, err := store.CreateLineItem(ctx, tenantID, domain.InvoiceLineItem{
		InvoiceID:        invID,
		LineType:         domain.LineTypeUsage,
		Description:      "GPU-hours",
		Quantity:         qty.IntPart(), // 1 (truncated, back-compat)
		QuantityDecimal:  qty,           // 1.5 (exact)
		UnitAmountCents:  1000,
		AmountCents:      1500,
		TotalAmountCents: 1500,
		Currency:         "USD",
	}); err != nil {
		t.Fatalf("create usage line item: %v", err)
	}

	got, err := store.ListLineItems(ctx, tenantID, invID)
	if err != nil {
		t.Fatalf("list line items: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListLineItems returned %d items, want 1", len(got))
	}
	if !got[0].QuantityDecimal.Equal(qty) {
		t.Errorf("QuantityDecimal = %s, want 1.5 (decimal quantity must round-trip, not truncate to %d)", got[0].QuantityDecimal, got[0].Quantity)
	}
	if got[0].Quantity != 1 {
		t.Errorf("Quantity = %d, want 1 (truncated int kept for back-compat)", got[0].Quantity)
	}
}

// TestAddLineItemAtomic_RejectsNonDraft ensures the status check happens
// inside the locking tx: callers cannot add a line item to a finalized
// or voided invoice, even if they race a concurrent Finalize call.
func TestAddLineItemAtomic_RejectsNonDraft(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)

	store := invoice.NewPostgresStore(db)
	tenantID := testutil.CreateTestTenant(t, db, "AddLineItem NonDraft")
	invID := seedDraftInvoice(t, db, tenantID)

	if _, err := store.UpdateStatus(ctx, tenantID, invID, domain.InvoiceFinalized); err != nil {
		t.Fatalf("finalize invoice: %v", err)
	}

	_, _, err := store.AddLineItemAtomic(ctx, tenantID, invID, domain.InvoiceLineItem{
		LineType:         domain.LineTypeAddOn,
		Description:      "rejected",
		Quantity:         1,
		UnitAmountCents:  500,
		AmountCents:      500,
		TotalAmountCents: 500,
	})
	if err == nil {
		t.Fatal("expected error adding line item to finalized invoice, got nil")
	}
}

// seedDraftInvoice creates the minimum fixture chain (customer → plan →
// subscription → invoice) and returns the draft invoice ID.
func seedDraftInvoice(t *testing.T, db *postgres.DB, tenantID string) string {
	t.Helper()
	ctx := postgres.WithLivemode(context.Background(), false)

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_add_line_item_test", DisplayName: "Line Item Tester",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	plan, err := pricing.NewPostgresStore(db).CreatePlan(ctx, tenantID, domain.Plan{
		Code: "line-item-plan", Name: "Line Item Plan", Currency: "USD",
		BillingInterval: domain.BillingMonthly, Status: domain.PlanActive,
		BaseAmountCents: 0,
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}

	periodStart := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	sub, err := subscription.NewPostgresStore(db).Create(ctx, tenantID, domain.Subscription{
		Code: "sub-line-item", DisplayName: "Line Item Sub",
		CustomerID: cust.ID,
		Status:     domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
		StartedAt:                 &periodStart,
		CurrentBillingPeriodStart: &periodStart, CurrentBillingPeriodEnd: bivPE(periodStart),
		Items: []domain.SubscriptionItem{{PlanID: plan.ID, Quantity: 1}},
	})
	if err != nil {
		t.Fatalf("create subscription: %v", err)
	}

	issuedAt := periodStart
	dueAt := periodStart.AddDate(0, 0, 30)
	inv, err := invoice.NewPostgresStore(db).Create(ctx, tenantID, domain.Invoice{
		CustomerID: cust.ID, SubscriptionID: sub.ID,
		InvoiceNumber:      "VLX-LINEITEM-001",
		Status:             domain.InvoiceDraft,
		PaymentStatus:      domain.PaymentPending,
		Currency:           "USD",
		BillingPeriodStart: periodStart,
		BillingPeriodEnd:   periodStart.AddDate(0, 1, 0),
		IssuedAt:           &issuedAt,
		DueAt:              &dueAt,
		NetPaymentTermDays: 30,
	})
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}
	return inv.ID
}
