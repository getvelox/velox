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

// TestResolveRun_CAS_ExactlyOnce is the real-Postgres proof of the resolve CAS: the
// first ResolveRun on an active run WINS (RowsAffected=1) and transitions it to
// resolved; a second ResolveRun on the now-resolved run LOSES (RowsAffected=0) via
// the `WHERE state <> 'resolved'` guard. This DB-level exactly-once transition is
// what lets the service fire dunning.resolved exactly once when two resolvers race
// the same run (the card-settle + processRun double-resolve regression).
func TestResolveRun_CAS_ExactlyOnce(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Resolve CAS")

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{ExternalID: "cus_cas", DisplayName: "CAS"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	store := dunning.NewPostgresStore(db)
	policy, err := store.UpsertPolicy(ctx, tenantID, domain.DunningPolicy{
		Name: "default", Enabled: true, RetrySchedule: []string{"72h"}, MaxRetryAttempts: 3,
		FinalSubscriptionAction: domain.SubActionNone, FinalInvoiceAction: domain.InvActionMarkUncollectible, GracePeriodDays: 3,
	})
	if err != nil {
		t.Fatalf("upsert policy: %v", err)
	}
	now := time.Now().UTC()
	inv, err := invoice.NewPostgresStore(db).Create(ctx, tenantID, domain.Invoice{
		CustomerID: cust.ID, InvoiceNumber: "INV-CAS", Status: domain.InvoiceFinalized,
		PaymentStatus: domain.PaymentFailed, Currency: "USD", SubtotalCents: 5000,
		TotalAmountCents: 5000, AmountDueCents: 5000, BillingPeriodStart: now.Add(-time.Hour),
		BillingPeriodEnd: now, IssuedAt: &now,
	})
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}
	run, err := store.CreateRun(ctx, tenantID, domain.InvoiceDunningRun{
		InvoiceID: inv.ID, CustomerID: cust.ID, PolicyID: policy.ID,
		State: domain.DunningActive, Reason: "payment_failed",
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	// The resolved fields the service stamps before the CAS.
	run.State = domain.DunningResolved
	run.Resolution = domain.ResolutionPaymentRecovered
	run.ResolvedAt = &now

	won1, err := store.ResolveRun(ctx, tenantID, run)
	if err != nil {
		t.Fatalf("ResolveRun #1: %v", err)
	}
	if !won1 {
		t.Fatal("first ResolveRun on an active run must WIN the CAS (RowsAffected=1)")
	}

	won2, err := store.ResolveRun(ctx, tenantID, run)
	if err != nil {
		t.Fatalf("ResolveRun #2: %v", err)
	}
	if won2 {
		t.Fatal("second ResolveRun on an already-resolved run must LOSE the CAS (RowsAffected=0) — the exactly-once guard")
	}

	got, err := store.GetRun(ctx, tenantID, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.State != domain.DunningResolved {
		t.Errorf("run state: got %q, want resolved", got.State)
	}
}
