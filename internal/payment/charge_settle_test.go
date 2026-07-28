package payment

import (
	"context"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

func finalizedPendingInvoice() domain.Invoice {
	issued := time.Date(2024, 5, 1, 0, 0, 0, 0, time.UTC)
	return domain.Invoice{
		ID: "inv_1", TenantID: "t1", CustomerID: "cus_1",
		Status:             domain.InvoiceFinalized,
		PaymentStatus:      domain.PaymentPending,
		AmountDueCents:     5000,
		TotalAmountCents:   5000,
		Currency:           "USD",
		IssuedAt:           &issued,
		BillingPeriodStart: issued.Add(-30 * 24 * time.Hour),
		BillingPeriodEnd:   issued,
	}
}

// TestChargeInvoice_SyncSuccessSettlesInline locks ADR-049 Phase 3: when Stripe
// returns a `succeeded` PaymentIntent synchronously in the create response (the
// common off-session card case), the charge path settles the invoice INLINE —
// no webhook required. This is the fix for the test-clock stuck-`processing`
// symptom: the invoice resolves in-request rather than waiting on a wall-clock
// webhook.
func TestChargeInvoice_SyncSuccessSettlesInline(t *testing.T) {
	client := &mockStripeClient{piID: "pi_ok", chargeStatus: "succeeded"}
	invoices := newMockInvoiceUpdater()
	invoices.invoices["inv_1"] = finalizedPendingInvoice()
	dunning := &recordingDunningStarter{}
	s := NewStripe(client, invoices, newMockWebhookStore(), nil, dunning)

	got, err := s.ChargeInvoice(context.Background(), "t1", invoices.invoices["inv_1"], "cus_stripe_abc", "pm_test")
	if err != nil {
		t.Fatalf("ChargeInvoice: %v", err)
	}
	// Returned invoice reflects the settled state (callers act on it).
	if got.PaymentStatus != domain.PaymentSucceeded || got.Status != domain.InvoicePaid {
		t.Errorf("returned invoice: got payment=%q status=%q, want succeeded/paid", got.PaymentStatus, got.Status)
	}
	// Persisted state is settled WITHOUT any webhook.
	stored := invoices.invoices["inv_1"]
	if stored.PaymentStatus != domain.PaymentSucceeded || stored.Status != domain.InvoicePaid {
		t.Errorf("stored invoice: got payment=%q status=%q, want succeeded/paid (settled inline, no webhook)", stored.PaymentStatus, stored.Status)
	}
	if stored.PaidAt == nil {
		t.Error("paid_at must be set by the inline settle")
	}
	if stored.StripePaymentIntentID != "pi_ok" {
		t.Errorf("stripe_payment_intent_id: got %q, want pi_ok", stored.StripePaymentIntentID)
	}
	// A successful charge does not start dunning.
	if len(dunning.calls) != 0 {
		t.Errorf("dunning started on a successful charge: %+v", dunning.calls)
	}
}

// TestChargeInvoice_ProcessingStaysProcessing confirms a genuinely in-flight
// status (async methods / off-session SCA) is NOT settled inline — it stays
// `processing` and awaits the webhook (+ the reconciler backstop from Phase 2).
func TestChargeInvoice_ProcessingStaysProcessing(t *testing.T) {
	client := &mockStripeClient{piID: "pi_async", chargeStatus: "processing"}
	invoices := newMockInvoiceUpdater()
	invoices.invoices["inv_1"] = finalizedPendingInvoice()
	s := NewStripe(client, invoices, newMockWebhookStore(), nil)

	got, err := s.ChargeInvoice(context.Background(), "t1", invoices.invoices["inv_1"], "cus_stripe_abc", "pm_test")
	if err != nil {
		t.Fatalf("ChargeInvoice: %v", err)
	}
	if got.PaymentStatus != domain.PaymentProcessing {
		t.Errorf("returned invoice: got %q, want processing", got.PaymentStatus)
	}
	stored := invoices.invoices["inv_1"]
	if stored.PaymentStatus != domain.PaymentProcessing {
		t.Errorf("stored invoice: got %q, want processing (in-flight awaits webhook)", stored.PaymentStatus)
	}
	if stored.Status == domain.InvoicePaid {
		t.Error("an in-flight charge must NOT mark the invoice paid")
	}
	if stored.StripePaymentIntentID != "pi_async" {
		t.Errorf("stripe_payment_intent_id: got %q, want pi_async", stored.StripePaymentIntentID)
	}
}

// TestChargeAttemptFacts_RecordedAtChokepointAndSettle locks the ADR-102
// writer set: a declined charge records a FAILED attempt fact (with the
// decline reason) even when no dunning follows and no webhook has landed —
// the row that closes the dunning-off timeline gap; a synchronous success
// records the attempt at PI-create time (pending) and the inline settle
// resolves the SAME PI to succeeded; SettleFailed resolves a failure the
// same way for PIs it didn't mint (hosted checkout).
func TestChargeAttemptFacts_RecordedAtChokepointAndSettle(t *testing.T) {
	// Declined charge → one failed attempt fact with the provider reason.
	client := &mockStripeClient{shouldFail: true, failErr: &PaymentError{
		Message: "Your card was declined.", DeclineCode: "card_declined", PaymentIntentID: "pi_declined",
	}}
	invoices := newMockInvoiceUpdater()
	invoices.invoices["inv_1"] = finalizedPendingInvoice()
	s := NewStripe(client, invoices, newMockWebhookStore(), nil, &recordingDunningStarter{})
	if _, err := s.ChargeInvoice(context.Background(), "t1", invoices.invoices["inv_1"], "cus_stripe_abc", "pm_test"); err == nil {
		t.Fatal("declined charge must return an error")
	}
	if len(invoices.chargeAttempts) != 1 {
		t.Fatalf("declined charge: %d attempt facts, want 1", len(invoices.chargeAttempts))
	}
	a := invoices.chargeAttempts[0]
	if a.Outcome != domain.ChargeAttemptFailed || a.Trigger != domain.ChargeTriggerAutoCharge ||
		a.StripePaymentIntentID != "pi_declined" || a.ProviderReason != "Your card was declined." {
		t.Fatalf("declined attempt fact wrong: %+v", a)
	}

	// Synchronous success → pending at PI create, succeeded at inline settle,
	// both on the SAME PI (the store upserts them into one row).
	client = &mockStripeClient{piID: "pi_ok", chargeStatus: "succeeded"}
	invoices = newMockInvoiceUpdater()
	invoices.invoices["inv_1"] = finalizedPendingInvoice()
	s = NewStripe(client, invoices, newMockWebhookStore(), nil, &recordingDunningStarter{})
	if _, err := s.ChargeInvoice(context.Background(), "t1", invoices.invoices["inv_1"], "cus_stripe_abc", "pm_test"); err != nil {
		t.Fatalf("ChargeInvoice: %v", err)
	}
	if len(invoices.chargeAttempts) != 2 {
		t.Fatalf("sync success: %d attempt writes, want 2 (pending insert + succeeded resolve)", len(invoices.chargeAttempts))
	}
	if invoices.chargeAttempts[0].Outcome != domain.ChargeAttemptPending ||
		invoices.chargeAttempts[1].Outcome != domain.ChargeAttemptSucceeded ||
		invoices.chargeAttempts[0].StripePaymentIntentID != "pi_ok" ||
		invoices.chargeAttempts[1].StripePaymentIntentID != "pi_ok" {
		t.Fatalf("sync success attempt writes wrong: %+v", invoices.chargeAttempts)
	}

	// SettleFailed on a PI the chokepoint didn't mint (hosted checkout) →
	// the settle-path insert IS the record, trigger external.
	invoices = newMockInvoiceUpdater()
	inv := finalizedPendingInvoice()
	invoices.invoices["inv_1"] = inv
	s = NewStripe(&mockStripeClient{}, invoices, newMockWebhookStore(), nil, &recordingDunningStarter{})
	if err := s.SettleFailed(context.Background(), "t1", inv, "pi_checkout", "Your card was declined.", true, SourceWebhook); err != nil {
		t.Fatalf("SettleFailed: %v", err)
	}
	if len(invoices.chargeAttempts) != 1 {
		t.Fatalf("SettleFailed: %d attempt facts, want 1", len(invoices.chargeAttempts))
	}
	a = invoices.chargeAttempts[0]
	if a.Outcome != domain.ChargeAttemptFailed || a.Trigger != domain.ChargeTriggerExternal ||
		a.StripePaymentIntentID != "pi_checkout" || a.SimEffectiveAt != nil {
		t.Fatalf("checkout attempt fact wrong: %+v", a)
	}
}
