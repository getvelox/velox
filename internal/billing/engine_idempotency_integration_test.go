package billing_test

import (
	"context"
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

// TestBilling_SamePeriodTwice_IdempotentSkip is FLOW B3 item #1, sequential: billing
// the SAME subscription+period a second time produces NO duplicate invoice — the
// second run's INSERT collides on idx_invoices_billing_idempotency, which
// billOnePeriod catches as a graceful idempotent skip ("invoice already exists for
// billing period"), returning invoiced=false, err=nil. Models a duplicate scheduler
// trigger / a re-run that didn't advance the cycle. (The concurrent twin is
// TestConcurrentBilling_ExactlyOneInvoice.)
func TestBilling_SamePeriodTwice_IdempotentSkip(t *testing.T) {
	db := testutil.SetupTestDB(t) // skips on -short
	ctx := postgres.WithLivemode(context.Background(), false)

	customerStore := customer.NewPostgresStore(db)
	pricingStore := pricing.NewPostgresStore(db)
	subStore := subscription.NewPostgresStore(db)
	usageStore := usage.NewPostgresStore(db)
	invoiceStore := invoice.NewPostgresStore(db)

	tenantID := testutil.CreateTestTenant(t, db, "Idempotency Test Corp")
	cust, err := customerStore.Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_idempotency", DisplayName: "Idempotency Customer",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	plan, err := pricingStore.CreatePlan(ctx, tenantID, domain.Plan{
		Code: "flat", Name: "Flat Plan", Currency: "USD",
		BillingInterval: domain.BillingMonthly, Status: domain.PlanActive,
		BaseAmountCents: 4900,
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}

	periodStart := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	sub, err := subStore.Create(ctx, tenantID, domain.Subscription{
		Code: "sub-idempotency", DisplayName: "Idempotency Sub",
		CustomerID: cust.ID,
		Items:      []domain.SubscriptionItem{{PlanID: plan.ID, Quantity: 1}},
		Status:     domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
		StartedAt:                 &periodStart,
		CurrentBillingPeriodStart: &periodStart, CurrentBillingPeriodEnd: &periodEnd,
	})
	if err != nil {
		t.Fatalf("create subscription: %v", err)
	}
	if err := subStore.UpdateBillingCycle(ctx, tenantID, sub.ID, periodStart, periodEnd, periodEnd, 0); err != nil {
		t.Fatalf("set billing cycle: %v", err)
	}

	fakeClk := clock.NewFake(periodEnd.Add(time.Nanosecond))
	engine := billing.NewEngine(
		&subStoreAdapter{subStore},
		&usageStoreAdapter{usageStore},
		&pricingStoreAdapter{pricingStore},
		&invoiceStoreAdapter{invoiceStore},
		nil, tenant.NewSettingsStore(db), testPaymentSetupsNoPM{}, testChargerSentinel{}, fakeClk,
	)
	engine.SetTaxProviderResolver(tax.NewResolver(nil))
	engine.SetNoPaymentMethodNotifier(&testNoPMNotifier{})

	// Run 1: bills the period.
	count1, errs1 := engine.RunCycle(ctx, 50)
	if len(errs1) > 0 {
		t.Fatalf("run 1 errors: %v", errs1)
	}
	if count1 != 1 {
		t.Fatalf("run 1: expected 1 invoice generated, got %d", count1)
	}

	// Re-arm the SAME period (simulate a duplicate trigger / a re-run that did not
	// advance the cycle) so run 2 re-attempts billing the already-billed period.
	if err := subStore.UpdateBillingCycle(ctx, tenantID, sub.ID, periodStart, periodEnd, periodEnd, 0); err != nil {
		t.Fatalf("re-arm billing cycle: %v", err)
	}

	// Run 2: must idempotently skip — no second invoice, no surfaced error.
	count2, errs2 := engine.RunCycle(ctx, 50)
	if len(errs2) > 0 {
		t.Fatalf("run 2 surfaced an error — the duplicate-period insert was not handled gracefully: %v", errs2)
	}
	if count2 != 0 {
		t.Fatalf("run 2: expected 0 invoices generated (idempotent skip), got %d", count2)
	}

	// The DB holds exactly one invoice for the subscription.
	_, total, err := invoiceStore.List(ctx, invoice.ListFilter{TenantID: tenantID})
	if err != nil {
		t.Fatalf("list invoices: %v", err)
	}
	if total != 1 {
		t.Fatalf("expected exactly 1 invoice after billing the same period twice, got %d", total)
	}
}
