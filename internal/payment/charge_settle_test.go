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
	s.SetChargeIntents(newMemChargeIntents())

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
	s.SetChargeIntents(newMemChargeIntents())

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

// TestChargeAttemptFacts_RecordedAtChokepoint locks the ADR-102/103
// writer split: the CHOKEPOINT records the attempt when it asks Stripe
// for money — a definite decline is a failed fact even with no dunning
// and no webhook yet, and a synchronous success is inserted `pending`
// before the settle resolves it. The OUTCOME half now lands inside the
// settle transaction (store-side, ADR-103), so it is proven by
// TestChargeAttempt_SettleTransitionsResolveOutcomeAtomically rather
// than by a post-commit write here.
func TestChargeAttemptFacts_RecordedAtChokepoint(t *testing.T) {
	// Declined charge → one failed attempt fact with the provider reason.
	client := &mockStripeClient{shouldFail: true, failErr: &PaymentError{
		Message: "Your card was declined.", DeclineCode: "card_declined", PaymentIntentID: "pi_declined",
	}}
	invoices := newMockInvoiceUpdater()
	invoices.invoices["inv_1"] = finalizedPendingInvoice()
	s := NewStripe(client, invoices, newMockWebhookStore(), nil, &recordingDunningStarter{})
	s.SetChargeIntents(newMemChargeIntents())
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

	// Synchronous success → the chokepoint's pending insert carries the
	// trigger + PI; the settle tx flips it to succeeded.
	client = &mockStripeClient{piID: "pi_ok", chargeStatus: "succeeded"}
	invoices = newMockInvoiceUpdater()
	invoices.invoices["inv_1"] = finalizedPendingInvoice()
	s = NewStripe(client, invoices, newMockWebhookStore(), nil, &recordingDunningStarter{})
	s.SetChargeIntents(newMemChargeIntents())
	if _, err := s.ChargeInvoice(context.Background(), "t1", invoices.invoices["inv_1"], "cus_stripe_abc", "pm_test"); err != nil {
		t.Fatalf("ChargeInvoice: %v", err)
	}
	if len(invoices.chargeAttempts) != 1 {
		t.Fatalf("sync success: %d chokepoint writes, want 1 (pending insert)", len(invoices.chargeAttempts))
	}
	if invoices.chargeAttempts[0].Outcome != domain.ChargeAttemptPending ||
		invoices.chargeAttempts[0].StripePaymentIntentID != "pi_ok" {
		t.Fatalf("sync success insert wrong: %+v", invoices.chargeAttempts[0])
	}
}

// TestChargeLease_ReleasedOnDefinitiveOutcomes locks the D1-walk fix:
// the chokepoint frees the per-invoice charge lease when Stripe ANSWERS
// (definite decline, or inline settle) — the round-trip is complete and
// a held lease only starves the next initiator (under test-clock
// catchup, back-to-back due dunning retries are seconds apart and a
// 5-minute lease stalled the cascade: "dunning catchup loop made no
// progress"). Ambiguous outcomes (unknown create error, async
// processing) must NOT release — the charge may be live at Stripe and
// the lease window is the anti-double-charge guard.
func TestChargeLease_ReleasedOnDefinitiveOutcomes(t *testing.T) {
	charge := func(client *mockStripeClient) *mockInvoiceUpdater {
		t.Helper()
		invoices := newMockInvoiceUpdater()
		invoices.invoices["inv_1"] = finalizedPendingInvoice()
		s := NewStripe(client, invoices, newMockWebhookStore(), nil, &recordingDunningStarter{})
		s.SetChargeIntents(newMemChargeIntents())
		_, _ = s.ChargeInvoice(context.Background(), "t1", invoices.invoices["inv_1"], "cus_stripe_abc", "pm_test")
		return invoices
	}

	// Definite decline → released.
	inv := charge(&mockStripeClient{shouldFail: true, failErr: &PaymentError{
		Message: "Your card was declined.", DeclineCode: "card_declined", PaymentIntentID: "pi_x",
	}})
	if len(inv.leaseReleases) != 1 {
		t.Fatalf("definite decline: %d lease releases, want 1", len(inv.leaseReleases))
	}

	// Inline settle success → released.
	inv = charge(&mockStripeClient{piID: "pi_ok", chargeStatus: "succeeded"})
	if len(inv.leaseReleases) != 1 {
		t.Fatalf("inline settle: %d lease releases, want 1", len(inv.leaseReleases))
	}

	// Unknown outcome → lease HELD for the reconciler.
	inv = charge(&mockStripeClient{shouldFail: true, failErr: &PaymentError{
		Message: "connection reset", Unknown: true,
	}})
	if len(inv.leaseReleases) != 0 {
		t.Fatalf("unknown outcome: %d lease releases, want 0 (lease must wait out the window)", len(inv.leaseReleases))
	}

	// Async processing → lease HELD (charge genuinely in flight).
	inv = charge(&mockStripeClient{piID: "pi_async", chargeStatus: "processing"})
	if len(inv.leaseReleases) != 0 {
		t.Fatalf("processing: %d lease releases, want 0", len(inv.leaseReleases))
	}
}
