package subscription

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestUpdateItem_ProrationSettingsUnreadable_RollsBack (P24): on the production
// path (handler with a DB, one tx around the item write and the proration), a
// failed tenant-settings read during proration — Net terms or timezone — fails
// the request and rolls the quantity change back. Never an upgraded item
// prorated in a guessed zone or on guessed terms, and no proration invoice.
func TestUpdateItem_ProrationSettingsUnreadable_RollsBack(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)

	cases := []struct {
		name string
		wire func(h *Handler)
	}{
		{"net_terms_unreadable", func(h *Handler) {
			h.SetNetTermsReader(netTermsStub{err: errors.New("settings read: connection reset")})
		}},
		{"timezone_unreadable", func(h *Handler) { h.SetTenantLocator(errTenantLocator{}) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tenantID := testutil.CreateTestTenant(t, db, "Proration Settings "+tc.name)
			cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
				ExternalID: "cus_" + tc.name, DisplayName: "P24",
			})
			if err != nil {
				t.Fatalf("create customer: %v", err)
			}
			plan, err := pricing.NewPostgresStore(db).CreatePlan(ctx, tenantID, domain.Plan{
				Code: "p24-adv-" + tc.name, Name: "Base", Currency: "USD",
				BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInAdvance,
				BaseAmountCents: 6000, Status: domain.PlanActive,
			})
			if err != nil {
				t.Fatalf("create plan: %v", err)
			}
			store := NewPostgresStore(db)
			created, err := store.Create(ctx, tenantID, domain.Subscription{
				Code: "sub-p24-" + tc.name, DisplayName: "P24", CustomerID: cust.ID,
				Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
				StartedAt:                 &proPeriodStart,
				CurrentBillingPeriodStart: &proPeriodStart,
				CurrentBillingPeriodEnd:   &proPeriodEnd,
				NextBillingAt:             &proPeriodEnd,
				Items:                     []domain.SubscriptionItem{{PlanID: plan.ID, Quantity: 1}},
			})
			if err != nil {
				t.Fatalf("create sub: %v", err)
			}
			sub, err := store.Get(ctx, tenantID, created.ID)
			if err != nil {
				t.Fatalf("get sub: %v", err)
			}

			invoices := &invoicesMock{sourceInvoice: domain.Invoice{
				ID: "src_inv", PaymentStatus: domain.PaymentSucceeded,
				SubtotalCents: 6000, TotalAmountCents: 6000, CreatedAt: proPeriodStart,
			}}
			h := NewHandler(svcWithStore(store))
			h.SetDB(db)
			h.SetProrationDeps(&plansMock{plans: map[string]domain.Plan{plan.ID: plan}}, invoices, &creditsMock{})
			tc.wire(h)

			reqCtx := clock.WithEffectiveNow(context.WithValue(ctx, auth.TestTenantIDKey(), tenantID), proNow)
			newQty := int64(2)
			body, _ := json.Marshal(UpdateItemInput{Quantity: &newQty, Immediate: true})
			rr := httptest.NewRecorder()
			h.updateItem(rr, updateItemURL(reqCtx, sub.ID, sub.Items[0].ID, body))

			if rr.Code >= 200 && rr.Code < 300 {
				t.Fatalf("updateItem succeeded (%d) with unreadable settings; want a failure", rr.Code)
			}
			after, err := store.Get(ctx, tenantID, sub.ID)
			if err != nil {
				t.Fatalf("get after: %v", err)
			}
			if after.Items[0].Quantity != 1 {
				t.Errorf("quantity must roll back to 1, got %d", after.Items[0].Quantity)
			}
			if n := len(invoices.createdInvoices); n != 0 {
				t.Errorf("proration invoices created: got %d, want 0", n)
			}
		})
	}
}
