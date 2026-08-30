package billing_test

import (
	"context"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/billing"
	"github.com/sagarsuperuser/velox/internal/credit"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/subscription"
	"github.com/sagarsuperuser/velox/internal/subscription/subscriptiontest"
	"github.com/sagarsuperuser/velox/internal/tax"
	"github.com/sagarsuperuser/velox/internal/tenant"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/usage"
)

// TestBillOnCreate_CreditBalance_E2E drives ADR-088 against Postgres: a
// customer holding credit balance subscribes to an in_advance plan — the
// day-1 invoice must consume the balance through the REAL credit ledger
// (event-sourced apply, atomic amount_due reduction). Full coverage settles
// the invoice paid with no charge attempt; partial coverage leaves exactly
// the remainder due. Pre-ADR-088 the day-1 invoice ignored the balance and
// charged (or queued) the full amount.
func TestBillOnCreate_CreditBalance_E2E(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)

	pricingStore := pricing.NewPostgresStore(db)
	subStore := subscription.NewPostgresStore(db)
	invoiceStore := invoice.NewPostgresStore(db)
	usageStore := usage.NewPostgresStore(db)
	creditStore := credit.NewPostgresStore(db)
	creditSvc := credit.NewService(creditStore)
	settingsStore := tenant.NewSettingsStore(db)
	customerStore := customer.NewPostgresStore(db)

	tenantID := testutil.CreateTestTenant(t, db, "Credit Day1 Corp")
	plan, err := pricingStore.CreatePlan(ctx, tenantID, domain.Plan{
		Code: "pro-adv-credit", Name: "Pro", Currency: "USD",
		BillingInterval: domain.BillingMonthly, Status: domain.PlanActive,
		BaseAmountCents: 4900, BaseBillTiming: domain.BillInAdvance,
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}

	periodStart := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	newEngine := func() *billing.Engine {
		e := billing.NewEngine(
			&subStoreAdapter{subStore}, &usageStoreAdapter{usageStore},
			&pricingStoreAdapter{pricingStore}, &invoiceStoreAdapter{invoiceStore},
			creditSvc, settingsStore, testPaymentSetupsNoPM{}, testChargerSentinel{},
			clock.NewFake(periodStart.Add(time.Hour)),
		)
		e.SetIntervalReader(subStore)
		e.SetTaxProviderResolver(tax.NewResolver(nil))
		e.SetNoPaymentMethodNotifier(&testNoPMNotifier{})
		e.SetDunningResolver(&testDunningResolver{})
		return e
	}

	freshSub := func(extID string, grantCents int64) domain.Subscription {
		cust, err := customerStore.Create(ctx, tenantID, domain.Customer{ExternalID: extID, DisplayName: extID})
		if err != nil {
			t.Fatalf("create customer: %v", err)
		}
		if grantCents > 0 {
			if _, err := creditSvc.Grant(ctx, tenantID, credit.GrantInput{
				CustomerID: cust.ID, AmountCents: grantCents,
				Description: "seed balance", At: periodStart,
			}); err != nil {
				t.Fatalf("grant credit: %v", err)
			}
		}
		sub, err := subStore.Create(ctx, tenantID, domain.Subscription{
			Code: "sub-" + extID, DisplayName: extID, CustomerID: cust.ID,
			Items:  []domain.SubscriptionItem{{PlanID: plan.ID, Quantity: 1}},
			Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
			StartedAt:                 &periodStart,
			CurrentBillingPeriodStart: &periodStart, CurrentBillingPeriodEnd: bivPE(periodStart),
		})
		if err != nil {
			t.Fatalf("create sub: %v", err)
		}
		subscriptiontest.SetBillingCycle(t, ctx, db, tenantID, sub.ID, periodStart, periodEnd, periodEnd, 0)
		sub.CurrentBillingPeriodStart = &periodStart
		sub.CurrentBillingPeriodEnd = &periodEnd
		return sub
	}

	t.Run("full coverage → day-1 invoice paid via ledger, no charge", func(t *testing.T) {
		sub := freshSub("cus_full_cover", 4900)
		inv, err := newEngine().BillOnCreate(ctx, sub)
		if err != nil {
			t.Fatalf("BillOnCreate: %v", err)
		}
		got, err := invoiceStore.Get(ctx, tenantID, inv.ID)
		if err != nil {
			t.Fatalf("re-read: %v", err)
		}
		if got.Status != domain.InvoicePaid || got.PaymentStatus != domain.PaymentSucceeded {
			t.Errorf("invoice = %s/%s, want paid/succeeded", got.Status, got.PaymentStatus)
		}
		if got.AmountDueCents != 0 || got.CreditsAppliedCents != 4900 {
			t.Errorf("due=%d creditsApplied=%d, want 0/4900", got.AmountDueCents, got.CreditsAppliedCents)
		}
		bal, err := creditSvc.GetBalance(ctx, tenantID, sub.CustomerID)
		if err != nil {
			t.Fatalf("balance: %v", err)
		}
		if bal.BalanceCents != 0 {
			t.Errorf("ledger balance = %d, want 0 (fully drained)", bal.BalanceCents)
		}
	})

	t.Run("partial coverage → exactly the remainder stays due", func(t *testing.T) {
		sub := freshSub("cus_part_cover", 2000)
		inv, err := newEngine().BillOnCreate(ctx, sub)
		if err != nil {
			t.Fatalf("BillOnCreate: %v", err)
		}
		got, _ := invoiceStore.Get(ctx, tenantID, inv.ID)
		if got.AmountDueCents != 2900 || got.CreditsAppliedCents != 2000 {
			t.Errorf("due=%d creditsApplied=%d, want 2900/2000", got.AmountDueCents, got.CreditsAppliedCents)
		}
		if got.Status != domain.InvoiceFinalized || got.PaymentStatus != domain.PaymentPending {
			t.Errorf("invoice = %s/%s, want finalized/pending (remainder awaits collection)", got.Status, got.PaymentStatus)
		}
		if !got.AutoChargePending {
			t.Error("no-PM fixture: the remainder must queue for the sweep")
		}
	})
}
