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

// TestRecordChargeAttempt_UpsertContract is the real-Postgres proof of the
// ADR-102 attempt-fact contract:
//   - one PI = one row: the chokepoint's insert and every settle-path
//     resolution land on the same row (partial-unique upsert);
//   - 'succeeded' is terminal (a stale failed webhook can't downgrade it)
//     while failed → succeeded IS allowed (3DS second try on one PI);
//   - the insert-time identity (trigger_source, sim_effective_at) survives
//     wall-clock settle upserts — the settle path must not strip the
//     billing-axis anchor;
//   - empty-PI attempts (the PI create itself failed) insert-only: two
//     such attempts are two rows, and each renders (no twin exists).
func TestRecordChargeAttempt_UpsertContract(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	store := invoice.NewPostgresStore(db)
	tenantID := testutil.CreateTestTenant(t, db, "Charge Attempts")

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_attempts", DisplayName: "Attempts",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	plan, err := pricing.NewPostgresStore(db).CreatePlan(ctx, tenantID, domain.Plan{
		Code: "attempts-plan", Name: "Attempts", Currency: "USD",
		BillingInterval: domain.BillingMonthly, Status: domain.PlanActive, BaseAmountCents: 3100,
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	ps := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	pe := ps.AddDate(0, 1, 0)
	sub, err := subscription.NewPostgresStore(db).Create(ctx, tenantID, domain.Subscription{
		Code: "sub-attempts", DisplayName: "Attempts Sub", CustomerID: cust.ID,
		Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
		StartedAt: &ps, Items: []domain.SubscriptionItem{{PlanID: plan.ID, Quantity: 1}},
		CurrentBillingPeriodStart: &ps, CurrentBillingPeriodEnd: &pe,
	})
	if err != nil {
		t.Fatalf("create subscription: %v", err)
	}
	due := ps.AddDate(0, 0, 30)
	inv, err := store.Create(ctx, tenantID, domain.Invoice{
		CustomerID: cust.ID, SubscriptionID: sub.ID,
		InvoiceNumber: "VLX-ATTEMPT-001",
		Status:        domain.InvoiceDraft, PaymentStatus: domain.PaymentPending, Currency: "USD",
		BillingPeriodStart: ps, BillingPeriodEnd: pe,
		IssuedAt: &ps, DueAt: &due, NetPaymentTermDays: 30,
	})
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}

	simAt := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	wall := time.Now().UTC()

	// Chokepoint insert: pending, sim-anchored, auto_charge.
	if err := store.RecordChargeAttempt(ctx, tenantID, domain.InvoiceChargeAttempt{
		InvoiceID: inv.ID, StripePaymentIntentID: "pi_attempt_1",
		Trigger: domain.ChargeTriggerAutoCharge, Outcome: domain.ChargeAttemptPending,
		AmountCents: 3100, OccurredAt: wall, SimEffectiveAt: &simAt,
	}); err != nil {
		t.Fatalf("insert attempt: %v", err)
	}
	// Settle-path failure resolution: wall-clock, external trigger — the
	// upsert must flip the outcome and reason but keep trigger + sim anchor.
	if err := store.RecordChargeAttempt(ctx, tenantID, domain.InvoiceChargeAttempt{
		InvoiceID: inv.ID, StripePaymentIntentID: "pi_attempt_1",
		Trigger: domain.ChargeTriggerExternal, Outcome: domain.ChargeAttemptFailed,
		ProviderReason: "Your card was declined.", OccurredAt: wall.Add(time.Second),
	}); err != nil {
		t.Fatalf("upsert failed outcome: %v", err)
	}
	got, err := store.ListChargeAttemptsByInvoice(ctx, tenantID, inv.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("after upsert: %d rows, want 1 (one PI = one row)", len(got))
	}
	a := got[0]
	if a.Outcome != domain.ChargeAttemptFailed || a.ProviderReason != "Your card was declined." {
		t.Fatalf("outcome not resolved: %+v", a)
	}
	if a.Trigger != domain.ChargeTriggerAutoCharge {
		t.Fatalf("settle upsert overwrote trigger: %s", a.Trigger)
	}
	if a.SimEffectiveAt == nil || !a.SimEffectiveAt.Equal(simAt) {
		t.Fatalf("settle upsert stripped the sim anchor: %v", a.SimEffectiveAt)
	}
	if a.AmountCents != 3100 {
		t.Fatalf("zero-amount upsert overwrote amount: %d", a.AmountCents)
	}

	// failed → succeeded is allowed (3DS second try)…
	if err := store.RecordChargeAttempt(ctx, tenantID, domain.InvoiceChargeAttempt{
		InvoiceID: inv.ID, StripePaymentIntentID: "pi_attempt_1",
		Trigger: domain.ChargeTriggerExternal, Outcome: domain.ChargeAttemptSucceeded,
		AmountCents: 3100, OccurredAt: wall.Add(2 * time.Second),
	}); err != nil {
		t.Fatalf("upsert succeeded: %v", err)
	}
	// …but succeeded is terminal: a stale failed redelivery can't downgrade.
	if err := store.RecordChargeAttempt(ctx, tenantID, domain.InvoiceChargeAttempt{
		InvoiceID: inv.ID, StripePaymentIntentID: "pi_attempt_1",
		Trigger: domain.ChargeTriggerExternal, Outcome: domain.ChargeAttemptFailed,
		ProviderReason: "stale redelivery", OccurredAt: wall.Add(3 * time.Second),
	}); err != nil {
		t.Fatalf("stale failed upsert: %v", err)
	}
	got, err = store.ListChargeAttemptsByInvoice(ctx, tenantID, inv.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].Outcome != domain.ChargeAttemptSucceeded {
		t.Fatalf("succeeded must be terminal, got %d rows outcome=%s", len(got), got[0].Outcome)
	}

	// Empty-PI attempts insert-only: two PI-create failures = two rows.
	for i := range 2 {
		if err := store.RecordChargeAttempt(ctx, tenantID, domain.InvoiceChargeAttempt{
			InvoiceID: inv.ID, Trigger: domain.ChargeTriggerAutoCharge,
			Outcome: domain.ChargeAttemptUnknown, ProviderReason: "network timeout creating PaymentIntent",
			AmountCents: 3100, OccurredAt: wall.Add(time.Duration(i+10) * time.Second),
		}); err != nil {
			t.Fatalf("empty-PI insert %d: %v", i, err)
		}
	}
	got, err = store.ListChargeAttemptsByInvoice(ctx, tenantID, inv.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("after empty-PI inserts: %d rows, want 3", len(got))
	}
}
