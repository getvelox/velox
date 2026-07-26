package billing_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/billing"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/subscription"
	"github.com/sagarsuperuser/velox/internal/tax"
	"github.com/sagarsuperuser/velox/internal/tenant"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/usage"
)

// TestFirstPeriod_TriggerAddRow_BillsFullBase_E2E stages the REAL
// collision every other integration test masks: the 0029/0129 DB
// trigger stamps the creation 'add' change row at the raw creation
// instant, while ADR-012 snaps the first period start BACKWARD to the
// creation day's midnight — so the row lands strictly INSIDE the
// (periodStart, periodEnd] billing window. Pre-clamp, cycle close read
// it as a mid-period add and billed the base "prorated 30/31 days" on
// a full first period (found live, FLOW I1 invoice NIM-000247).
//
// Unlike TestFullBillingCycle_E2E (which backdates the period to a
// fixed past date, pushing the wall-clock trigger row out of the
// window), this test derives the period from the actual creation
// day's snap — never hardcode dates here (see the time-bomb note in
// cancel_credit_atomic_integration_test.go).
func TestFirstPeriod_TriggerAddRow_BillsFullBase_E2E(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)

	customerStore := customer.NewPostgresStore(db)
	pricingStore := pricing.NewPostgresStore(db)
	subStore := subscription.NewPostgresStore(db)
	usageStore := usage.NewPostgresStore(db)
	invoiceStore := invoice.NewPostgresStore(db)

	tenantID := testutil.CreateTestTenant(t, db, "First Period Corp")

	cust, err := customerStore.Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_first_period", DisplayName: "First Period Customer",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	plan, err := pricingStore.CreatePlan(ctx, tenantID, domain.Plan{
		Code: "first-period", Name: "First Period Plan", Currency: "USD",
		BillingInterval: domain.BillingMonthly, Status: domain.PlanActive,
		BaseAmountCents: 2900,
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}

	// The ADR-012 first period for a sub created RIGHT NOW (default org
	// TZ = UTC): day-snapped start, one anniversary month forward —
	// exactly what firstPeriodForActivate stamps on a StartNow create.
	periodStart := domain.BeginningOfDayIn(time.Now().UTC(), time.UTC)
	anchorDay := domain.AnchorDayFor(periodStart, domain.BillingTimeAnniversary, domain.BillingMonthly, time.UTC)
	periodEnd := domain.NextBillingPeriodEnd(periodStart, domain.BillingTimeAnniversary, domain.BillingMonthly, time.UTC, anchorDay)

	sub, err := subStore.Create(ctx, tenantID, domain.Subscription{
		Code: "sub-first-period", DisplayName: "First Period",
		CustomerID: cust.ID,
		Items:      []domain.SubscriptionItem{{PlanID: plan.ID, Quantity: 1}},
		Status:     domain.SubscriptionActive, BillingTime: domain.BillingTimeAnniversary,
	})
	if err != nil {
		t.Fatalf("create subscription: %v", err)
	}
	if err := subStore.UpdateBillingCycle(ctx, tenantID, sub.ID, periodStart, periodEnd, periodEnd, anchorDay); err != nil {
		t.Fatalf("set billing cycle: %v", err)
	}

	// GUARD: the trigger's creation 'add' row must be INSIDE the window
	// — otherwise this test degenerates into the masked shape and stops
	// exercising the collision. (Only a create in the same second as
	// UTC midnight could legitimately land ON periodStart and be
	// excluded by the exclusive-left window.)
	rows, err := subStore.ListItemChangesInPeriod(ctx, tenantID, sub.ID, periodStart, periodEnd)
	if err != nil {
		t.Fatalf("list item changes: %v", err)
	}
	if len(rows) != 1 || rows[0].ChangeType != "add" {
		t.Fatalf("expected the trigger's creation 'add' row inside the billing window, got %+v", rows)
	}

	settingsStore := tenant.NewSettingsStore(db)
	fakeClk := clock.NewFake(periodEnd.Add(time.Nanosecond))
	engine := billing.NewEngine(
		&subStoreAdapter{subStore},
		&usageStoreAdapter{usageStore},
		&pricingStoreAdapter{pricingStore},
		&invoiceStoreAdapter{invoiceStore},
		nil, settingsStore, testPaymentSetupsNoPM{}, testChargerSentinel{}, fakeClk,
	)
	engine.SetTaxProviderResolver(tax.NewResolver(nil))
	engine.SetNoPaymentMethodNotifier(&testNoPMNotifier{})

	count, errs := engine.RunCycle(ctx, 50)
	if len(errs) > 0 {
		t.Fatalf("billing cycle errors: %v", errs)
	}
	if count != 1 {
		t.Fatalf("expected 1 invoice, got %d", count)
	}

	invoices, _, err := invoiceStore.List(ctx, invoice.ListFilter{TenantID: tenantID})
	if err != nil {
		t.Fatalf("list invoices: %v", err)
	}
	if len(invoices) != 1 {
		t.Fatalf("expected 1 invoice, got %d", len(invoices))
	}

	items, err := invoiceStore.ListLineItems(ctx, tenantID, invoices[0].ID)
	if err != nil {
		t.Fatalf("list line items: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 base line item, got %d", len(items))
	}
	li := items[0]
	// Full $29 — mutation-verifying: reverting the itemBaseSegments
	// day-grade clamp bills 2806 ("prorated 30/31 days") here.
	if li.AmountCents != 2900 {
		t.Errorf("base fee: got %d, want 2900 (full first period per ADR-012)", li.AmountCents)
	}
	if strings.Contains(li.Description, "prorated") {
		t.Errorf("base line claims proration on a full first period: %q", li.Description)
	}
	if li.BillingPeriodStart == nil || !li.BillingPeriodStart.Equal(periodStart) {
		t.Errorf("base line period start: got %v, want %v (the invoice period, not the creation instant)", li.BillingPeriodStart, periodStart)
	}
}
