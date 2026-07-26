package subscription

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
)

// TestAddItem_DayOneOfPeriod_BillsFullPeriod pins the day-grade clamp
// on the in_advance AddItem proration (ADR-012 amendment 2026-07-26):
// an item added ON the period-start calendar day owes the whole period
// — the add day counts whole. Pre-clamp, a post-noon day-1 add paid
// remainingDays = roundDays(periodEnd − 21:00) = 29 of 30 ($19.33)
// while its in_arrears twin bills the full period at cycle close via
// the itemBaseSegments clamp — a 1-day cadence asymmetry on the same
// day-grade principle.
func TestAddItem_DayOneOfPeriod_BillsFullPeriod(t *testing.T) {
	// 21:00 on the period's FIRST day (post-noon, so pre-clamp rounding
	// visibly drops a day).
	dayOneEvening := time.Date(2026, 6, 1, 21, 0, 0, 0, time.UTC)
	ctx := clock.WithEffectiveNow(context.Background(), dayOneEvening)
	tenantID := "t1"

	store := newMemStore()
	subID, _ := seedSubWithItemAt(t, store, tenantID, "cus_1", "plan_existing", proPeriodStart, proPeriodEnd)
	svc := NewService(store, nil)

	plans := &plansMock{plans: map[string]domain.Plan{
		"plan_existing": {ID: "plan_existing", Name: "Basic", BaseAmountCents: 1000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
		"plan_new":      {ID: "plan_new", Name: "Add-on", BaseAmountCents: 2000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
	}}
	invoices := &invoicesMock{
		sourceInvoice: domain.Invoice{ID: "src_inv", PaymentStatus: domain.PaymentSucceeded},
	}
	credits := &creditsMock{}

	h := NewHandler(svc)
	h.SetProrationDeps(plans, invoices, credits)

	body, _ := json.Marshal(AddItemInput{PlanID: "plan_new", Quantity: 1})
	req := addItemURL(context.WithValue(ctx, auth.TestTenantIDKey(), tenantID), subID, body)

	rr := httptest.NewRecorder()
	h.addItem(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status: got %d, want 201. body=%s", rr.Code, rr.Body.String())
	}
	if len(invoices.createdInvoices) != 1 {
		t.Fatalf("createdInvoices: got %d, want 1", len(invoices.createdInvoices))
	}
	// Full $20 — not 29/30 = $19.33. Mutation: removing the day-grade
	// clamp in the addItem handler fails this assertion.
	if inv := invoices.createdInvoices[0]; inv.SubtotalCents != 2000 {
		t.Errorf("invoice subtotal: got %d, want 2000 (full period — the add day counts whole; 1933 = the pre-clamp 29/30)", inv.SubtotalCents)
	}
}
