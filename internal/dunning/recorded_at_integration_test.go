package dunning_test

import (
	"context"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/dunning"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestDunningEvent_RecordedAt_DualStamp is the invoice_dunning_events half
// of migration 0164 (ADR-104 Invariant A, corrected boundary): an event
// written under a bound simulated ctx carries created_at on the entity's
// calendar and recorded_at on ours. Same class as the credit-note twin —
// both are INSERT-backed narrative rows that previously had no wall stamp.
func TestDunningEvent_RecordedAt_DualStamp(t *testing.T) {
	db := testutil.SetupTestDB(t)
	base := postgres.WithLivemode(context.Background(), false)

	tenantID := testutil.CreateTestTenant(t, db, "Dunning recorded_at")
	custStore := customer.NewPostgresStore(db)
	invStore := invoice.NewPostgresStore(db)
	dunStore := dunning.NewPostgresStore(db)

	cust, err := custStore.Create(base, tenantID, domain.Customer{ExternalID: "dun-rec", DisplayName: "Dun Rec"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	now := time.Now().UTC()
	inv, err := invStore.Create(base, tenantID, domain.Invoice{
		CustomerID: cust.ID, InvoiceNumber: "VLX-DUNREC-1",
		Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentFailed,
		Currency: "USD", SubtotalCents: 5000, TotalAmountCents: 5000, AmountDueCents: 5000,
		BillingPeriodStart: now.Add(-30 * 24 * time.Hour), BillingPeriodEnd: now,
		IssuedAt: &now,
	})
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}

	policy, err := dunStore.UpsertPolicy(base, tenantID, domain.DunningPolicy{
		Name: "default", Enabled: true, RetrySchedule: []string{"72h"}, MaxRetryAttempts: 3,
		FinalSubscriptionAction: domain.SubActionNone, FinalInvoiceAction: domain.InvActionMarkUncollectible, GracePeriodDays: 3,
	})
	if err != nil {
		t.Fatalf("upsert policy: %v", err)
	}

	frozen := time.Date(2027, 3, 7, 9, 0, 0, 0, time.UTC)
	simCtx := clock.WithSim(base, clock.Sim{At: frozen, TestClockID: "vlx_tclk_dunrec"})

	run, err := dunStore.CreateRun(simCtx, tenantID, domain.InvoiceDunningRun{
		InvoiceID: inv.ID, CustomerID: cust.ID, PolicyID: policy.ID,
		State: domain.DunningActive, AttemptCount: 0,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	// CreateEvent's contract: zero CreatedAt falls back to clock.Now(ctx)
	// — the simulated instant on a bound ctx.
	evt, err := dunStore.CreateEvent(simCtx, tenantID, domain.InvoiceDunningEvent{
		RunID: run.ID, InvoiceID: inv.ID,
		EventType: domain.DunningEventStarted, State: domain.DunningActive,
	})
	if err != nil {
		t.Fatalf("create event: %v", err)
	}
	if !evt.CreatedAt.Equal(frozen) {
		t.Errorf("created_at = %s, want the frozen instant %s (ADR-030)", evt.CreatedAt, frozen)
	}

	events, err := dunStore.ListEvents(base, tenantID, run.ID)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("no events listed")
	}
	got := events[len(events)-1]
	if got.RecordedAt == nil {
		t.Fatal("recorded_at is nil — the row carries no wall stamp (Invariant A)")
	}
	if got.RecordedAt.Equal(frozen) {
		t.Error("recorded_at = the frozen instant — stamped from the simulated clock instead of the real INSERT moment")
	}
	if d := time.Since(*got.RecordedAt); d < 0 || d > 2*time.Minute {
		t.Errorf("recorded_at = %s, not within the test's real-time window (drift %s)", got.RecordedAt, d)
	}
}
