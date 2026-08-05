package dunning_test

import (
	"context"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/dunning"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestGetStats_AtRiskScopedToDefaultCurrency covers the low audit finding: the
// dunning at-risk stat summed amount_due_cents across mixed currencies into one
// integer. It's now scoped to the tenant's default currency so a EUR invoice
// can't corrupt a USD-denominated at-risk figure.
func TestGetStats_AtRiskScopedToDefaultCurrency(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)

	tenantID := testutil.CreateTestTenant(t, db, "Dunning Stats Currency")
	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_dun", DisplayName: "Dun",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	store := dunning.NewPostgresStore(db)
	policy, err := store.UpsertPolicy(ctx, tenantID, domain.DunningPolicy{
		Name: "default", Enabled: true,
		RetrySchedule: []string{"72h", "120h"}, MaxRetryAttempts: 3,
		FinalSubscriptionAction: domain.SubActionNone, FinalInvoiceAction: domain.InvActionMarkUncollectible, GracePeriodDays: 3,
	})
	if err != nil {
		t.Fatalf("upsert policy: %v", err)
	}

	invStore := invoice.NewPostgresStore(db)
	// USD invoice $100 due, EUR invoice $999 due — both with an active run.
	mkInvoice := func(num, currency string, due int64) string {
		now := time.Now().UTC()
		issued := now
		inv, err := invStore.Create(ctx, tenantID, domain.Invoice{
			CustomerID: cust.ID, InvoiceNumber: num,
			Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentFailed,
			Currency: currency, SubtotalCents: due, TotalAmountCents: due, AmountDueCents: due,
			BillingPeriodStart: now.Add(-time.Hour), BillingPeriodEnd: now, IssuedAt: &issued,
		})
		if err != nil {
			t.Fatalf("create %s invoice: %v", currency, err)
		}
		if _, err := store.CreateRun(ctx, tenantID, domain.InvoiceDunningRun{
			InvoiceID: inv.ID, CustomerID: cust.ID, PolicyID: policy.ID,
			State: domain.DunningActive, Reason: "payment_failed",
		}); err != nil {
			t.Fatalf("create run for %s: %v", currency, err)
		}
		return inv.ID
	}
	mkInvoice("INV-USD", "USD", 100_00)
	mkInvoice("INV-EUR", "EUR", 999_00)

	stats, err := store.GetStats(ctx, tenantID)
	if err != nil {
		t.Fatalf("get stats: %v", err)
	}
	if stats.Currency != "USD" {
		t.Errorf("currency: got %q, want USD", stats.Currency)
	}
	if stats.AtRiskCents != 100_00 {
		t.Errorf("at_risk_cents: got %d, want 10000 (USD only — EUR excluded, not summed in)", stats.AtRiskCents)
	}
	if stats.ActiveCount != 2 {
		t.Errorf("active_count: got %d, want 2 (count is currency-agnostic)", stats.ActiveCount)
	}
}
