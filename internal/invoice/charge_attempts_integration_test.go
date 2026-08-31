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

// TestChargeAttempt_SettleTransitionsResolveOutcomeAtomically is the
// ADR-103 proof: the settle transitions resolve an attempt's outcome
// INSIDE their own transaction, so the timeline's single payment owner
// can never disagree with the invoice's state. It also covers the
// hosted-checkout shape — a PaymentIntent the chokepoint never recorded
// gets its row created by the settle itself.
func TestChargeAttempt_SettleTransitionsResolveOutcomeAtomically(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	store := invoice.NewPostgresStore(db)
	tenantID := testutil.CreateTestTenant(t, db, "Attempt Settle Atomicity")

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_settle_atomic", DisplayName: "Settle Atomic",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	ps := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	due := ps.AddDate(0, 0, 30)
	mk := func(number string) domain.Invoice {
		t.Helper()
		inv, err := store.Create(ctx, tenantID, domain.Invoice{
			CustomerID: cust.ID, InvoiceNumber: number,
			Status: domain.InvoiceDraft, PaymentStatus: domain.PaymentPending, Currency: "USD",
			BillingPeriodStart: ps, BillingPeriodEnd: ps.AddDate(0, 1, 0),
			IssuedAt: &ps, DueAt: &due, NetPaymentTermDays: 30,
		})
		if err != nil {
			t.Fatalf("create %s: %v", number, err)
		}
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer postgres.Rollback(tx)
		if _, err := tx.ExecContext(ctx, `
			UPDATE invoices SET status='finalized', amount_due_cents=2500, total_amount_cents=2500
			WHERE id=$1`, inv.ID); err != nil {
			t.Fatalf("finalize %s: %v", number, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit %s: %v", number, err)
		}
		return inv
	}

	// FAILED settle on a PI no chokepoint ever recorded (hosted checkout):
	// the transition itself must create the row.
	failed := mk("VLX-ATOMIC-FAIL")
	if _, _, err := store.MarkPaymentFailedReportingTransition(ctx, tenantID, failed.ID, "pi_atomic_fail", "Your card was declined.", nil); err != nil {
		t.Fatalf("mark failed: %v", err)
	}
	got, err := store.ListChargeAttemptsByInvoice(ctx, tenantID, failed.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].Outcome != domain.ChargeAttemptFailed ||
		got[0].StripePaymentIntentID != "pi_atomic_fail" || got[0].ProviderReason != "Your card was declined." {
		t.Fatalf("failed settle must record the attempt in-tx: %+v", got)
	}
	if got[0].Trigger != domain.ChargeTriggerExternal {
		t.Fatalf("a settle-created row is an external attempt, got %s", got[0].Trigger)
	}

	// SUCCEEDED settle resolves an existing chokepoint row in place —
	// insert-time identity (trigger, sim anchor) must survive.
	paid := mk("VLX-ATOMIC-PAID")
	simAt := ps.AddDate(0, 0, 3)
	if err := store.RecordChargeAttempt(ctx, tenantID, domain.InvoiceChargeAttempt{
		InvoiceID: paid.ID, StripePaymentIntentID: "pi_atomic_ok",
		Trigger: domain.ChargeTriggerDunningRetry, Outcome: domain.ChargeAttemptPending,
		AmountCents: 2500, OccurredAt: time.Now().UTC(), SimEffectiveAt: &simAt,
	}); err != nil {
		t.Fatalf("chokepoint insert: %v", err)
	}
	if _, _, err := store.MarkPaidCardSettlementTransition(ctx, tenantID, paid.ID, "pi_atomic_ok", time.Now().UTC(), nil); err != nil {
		t.Fatalf("mark paid: %v", err)
	}
	got, err = store.ListChargeAttemptsByInvoice(ctx, tenantID, paid.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("settle must resolve the SAME row, got %d", len(got))
	}
	if got[0].Outcome != domain.ChargeAttemptSucceeded {
		t.Fatalf("outcome not resolved in-tx: %+v", got[0])
	}
	if got[0].Trigger != domain.ChargeTriggerDunningRetry {
		t.Fatalf("settle overwrote the chokepoint's trigger: %s", got[0].Trigger)
	}
	if got[0].SimEffectiveAt == nil || !got[0].SimEffectiveAt.Equal(simAt) {
		t.Fatalf("settle stripped the billing-axis anchor: %v", got[0].SimEffectiveAt)
	}
}
