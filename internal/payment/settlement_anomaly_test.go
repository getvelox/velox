package payment

import (
	"context"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
)

type recordingAnomalies struct {
	kinds []string
	pis   []string
}

func (r *recordingAnomalies) RecordPaymentAnomaly(_ context.Context, _, _, kind, pi string, _ int64) error {
	r.kinds = append(r.kinds, kind)
	r.pis = append(r.pis, pi)
	return nil
}

func newAnomalyHarness(inv domain.Invoice) (*Stripe, *mockInvoiceUpdater, *recordingEventDispatcher, *recordingAnomalies) {
	invoices := newMockInvoiceUpdater()
	invoices.invoices[inv.ID] = inv
	if inv.StripePaymentIntentID != "" {
		invoices.byPI[inv.StripePaymentIntentID] = inv.ID
	}
	events := &recordingEventDispatcher{byType: map[string]int{}}
	anomalies := &recordingAnomalies{}
	s := NewStripe(&mockStripeClient{}, invoices, newMockWebhookStore(), nil)
	s.SetChargeIntents(newMemChargeIntents())
	s.SetEventDispatcher(events)
	s.SetAnomalyRecorder(anomalies)
	return s, invoices, events, anomalies
}

// TestSettleSucceeded_DuplicatePI_FastPath: a SECOND, different PI succeeding
// against an already-paid invoice escalates payment.duplicate_charge + stamps
// the durable marker; a same-PI redelivery stays a silent skip. Mutation
// seam: drop the fast-path comparison → zero events.
func TestSettleSucceeded_DuplicatePI_FastPath(t *testing.T) {
	paid := domain.Invoice{
		ID: "inv_1", TenantID: "t1", Status: domain.InvoicePaid,
		PaymentStatus: domain.PaymentSucceeded, StripePaymentIntentID: "pi_A",
		AmountPaidCents: 1000,
	}
	s, _, events, anomalies := newAnomalyHarness(paid)

	// Same PI: routine redelivery — silence.
	if err := s.SettleSucceeded(context.Background(), "t1", paid, "pi_A", 1000, SourceWebhook); err != nil {
		t.Fatalf("same-PI: %v", err)
	}
	if events.byType[domain.EventPaymentDuplicateCharge] != 0 {
		t.Fatalf("same-PI redelivery escalated duplicate_charge — false alarm")
	}

	// Different PI: money captured twice.
	if err := s.SettleSucceeded(context.Background(), "t1", paid, "pi_B", 1000, SourceWebhook); err != nil {
		t.Fatalf("different-PI: %v", err)
	}
	if events.byType[domain.EventPaymentDuplicateCharge] != 1 {
		t.Fatalf("duplicate_charge events = %d, want 1", events.byType[domain.EventPaymentDuplicateCharge])
	}
	if len(anomalies.kinds) != 1 || anomalies.kinds[0] != domain.EventPaymentDuplicateCharge || anomalies.pis[0] != "pi_B" {
		t.Fatalf("durable marker = %v/%v, want duplicate_charge/pi_B", anomalies.kinds, anomalies.pis)
	}
}

// TestSettleSucceeded_DuplicatePI_TransitionSkip: the transitioned=false skip
// point compares against the row RETURNED by the FOR-UPDATE transition — a
// checkout invoice records its PI only at settle, so the caller's stale
// snapshot (empty recorded PI) must NOT be the comparand. Same-PI concurrent
// loser = silence; different-PI = duplicate_charge.
func TestSettleSucceeded_DuplicatePI_TransitionSkip(t *testing.T) {
	// Caller snapshot is PRE-settle (processing, no recorded PI — checkout
	// shape); the stored row is already paid by pi_A (the concurrent winner).
	snapshot := domain.Invoice{
		ID: "inv_1", TenantID: "t1", Status: domain.InvoiceFinalized,
		PaymentStatus: domain.PaymentProcessing, AmountDueCents: 1000,
	}
	stored := snapshot
	stored.Status = domain.InvoicePaid
	stored.PaymentStatus = domain.PaymentSucceeded
	stored.StripePaymentIntentID = "pi_A"
	stored.AmountPaidCents = 1000
	stored.AmountDueCents = 0

	s, invoices, events, _ := newAnomalyHarness(stored)
	_ = invoices

	// Same PI as the winner: the loser of a routine concurrent redelivery.
	if err := s.SettleSucceeded(context.Background(), "t1", snapshot, "pi_A", 1000, SourceWebhook); err != nil {
		t.Fatalf("same-PI loser: %v", err)
	}
	if events.byType[domain.EventPaymentDuplicateCharge] != 0 {
		t.Fatalf("same-PI concurrent loser escalated duplicate_charge — the stale-snapshot false alarm the panel predicted")
	}

	// Different PI: genuine double charge racing the settle.
	if err := s.SettleSucceeded(context.Background(), "t1", snapshot, "pi_B", 1000, SourceWebhook); err != nil {
		t.Fatalf("different-PI loser: %v", err)
	}
	if events.byType[domain.EventPaymentDuplicateCharge] != 1 {
		t.Fatalf("duplicate_charge events = %d, want 1", events.byType[domain.EventPaymentDuplicateCharge])
	}
}

// TestSettleSucceeded_PaymentOnVoided: the transition's InvalidState (voided
// target) escalates payment.received_on_voided_invoice and ABSORBS the error
// — the webhook must not retry a terminal invoice forever.
func TestSettleSucceeded_PaymentOnVoided(t *testing.T) {
	voided := domain.Invoice{
		ID: "inv_v", TenantID: "t1", Status: domain.InvoiceVoided,
		PaymentStatus: domain.PaymentPending,
	}
	s, invoices, events, anomalies := newAnomalyHarness(voided)
	invoices.markPaidErr = errs.InvalidState("cannot mark invoice paid from status \"voided\"")

	if err := s.SettleSucceeded(context.Background(), "t1", voided, "pi_Z", 700, SourceWebhook); err != nil {
		t.Fatalf("voided settle must absorb, got: %v", err)
	}
	if events.byType[domain.EventPaymentReceivedOnVoidedInvoice] != 1 {
		t.Fatalf("received_on_voided events = %d, want 1", events.byType[domain.EventPaymentReceivedOnVoidedInvoice])
	}
	if len(anomalies.kinds) != 1 || anomalies.kinds[0] != domain.EventPaymentReceivedOnVoidedInvoice {
		t.Fatalf("marker = %v, want received_on_voided", anomalies.kinds)
	}
}

// TestSettleSucceeded_AmountMismatch: captured != booked escalates
// payment.amount_mismatch (detection-only; the settle still stands). The
// exact-match settle stays silent. Mutation seam: drop the comparison.
func TestSettleSucceeded_AmountMismatch(t *testing.T) {
	inv := domain.Invoice{
		ID: "inv_m", TenantID: "t1", Status: domain.InvoiceFinalized,
		PaymentStatus: domain.PaymentProcessing, AmountDueCents: 1000,
	}
	s, _, events, anomalies := newAnomalyHarness(inv)

	// Captured 1000 == due 1000 → silence.
	if err := s.SettleSucceeded(context.Background(), "t1", inv, "pi_ok", 1000, SourceWebhook); err != nil {
		t.Fatalf("exact settle: %v", err)
	}
	if events.byType[domain.EventPaymentAmountMismatch] != 0 {
		t.Fatal("exact-amount settle escalated amount_mismatch")
	}

	inv2 := domain.Invoice{
		ID: "inv_m2", TenantID: "t1", Status: domain.InvoiceFinalized,
		PaymentStatus: domain.PaymentProcessing, AmountDueCents: 600,
	}
	s2, _, events2, anomalies2 := newAnomalyHarness(inv2)
	// The drifted-session shape: Stripe captured 1000, the invoice books 600.
	if err := s2.SettleSucceeded(context.Background(), "t1", inv2, "pi_drift", 1000, SourceWebhook); err != nil {
		t.Fatalf("drift settle: %v", err)
	}
	if events2.byType[domain.EventPaymentAmountMismatch] != 1 {
		t.Fatalf("amount_mismatch events = %d, want 1", events2.byType[domain.EventPaymentAmountMismatch])
	}
	if len(anomalies2.kinds) != 1 || anomalies2.kinds[0] != domain.EventPaymentAmountMismatch {
		t.Fatalf("marker = %v, want amount_mismatch", anomalies2.kinds)
	}
	_ = anomalies
}
