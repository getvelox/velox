package payment

import (
	"context"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// recordingSettler captures what the reconciler routes through the settlement
// primitive (ADR-049 Phase 2). The primitive's side-effects (dunning, event,
// email) are proven in settlement_test.go; these tests assert the reconciler
// DELEGATES — terminal status → SettleSucceeded/SettleFailed with the right
// args — which is the convergence that closes the silent-under-collection gap.
type recordingSettler struct {
	succeeded []settledCall
	failed    []settledCall
}

type settledCall struct {
	invoiceID     string
	piID          string
	failMsg       string
	suppressEmail bool
}

func (r *recordingSettler) SettleSucceeded(_ context.Context, _ string, inv domain.Invoice, piID string, _ int64, _ SettlementSource) error {
	r.succeeded = append(r.succeeded, settledCall{invoiceID: inv.ID, piID: piID})
	return nil
}

func (r *recordingSettler) SettleFailed(_ context.Context, _ string, inv domain.Invoice, piID, failMsg string, suppress bool, _ SettlementSource) error {
	r.failed = append(r.failed, settledCall{invoiceID: inv.ID, piID: piID, failMsg: failMsg, suppressEmail: suppress})
	return nil
}

// TestReconciler_RecoveredFailureRoutesThroughSettler is the Phase 2 keystone:
// a dropped payment_failed webhook recovered by the reconciler must route
// through the primitive (so dunning + event + email fire), NOT a bare write.
func TestReconciler_RecoveredFailureRoutesThroughSettler(t *testing.T) {
	store := newMockReconcileStore(domain.Invoice{
		ID: "inv_1", TenantID: "t1", PaymentStatus: domain.PaymentUnknown,
		StripePaymentIntentID: "pi_1",
	})
	client := &mockStripeClient{piStates: map[string]PaymentIntentResult{
		"pi_1": {ID: "pi_1", Status: "canceled"},
	}}
	rec := &recordingSettler{}
	r := NewReconciler(client, store, time.Second)
	r.SetSettler(rec)

	resolved, errs := r.Run(context.Background(), 10)
	if len(errs) != 0 {
		t.Fatalf("errs: %v", errs)
	}
	if resolved != 1 {
		t.Errorf("resolved: got %d, want 1", resolved)
	}
	if len(rec.failed) != 1 || rec.failed[0].invoiceID != "inv_1" || rec.failed[0].piID != "pi_1" {
		t.Fatalf("SettleFailed: got %+v, want one call for inv_1/pi_1 (not a bare write)", rec.failed)
	}
	if rec.failed[0].suppressEmail {
		t.Error("a plain auto-charge failure must NOT suppress the customer email")
	}
	if len(rec.succeeded) != 0 {
		t.Errorf("unexpected SettleSucceeded calls: %+v", rec.succeeded)
	}
}

// TestReconciler_DunningRetryFailureSuppressesEmail proves the reconciler
// replicates the webhook's email suppression from the PI purpose — a recovered
// dunning-retry failure must NOT double-notify (dunning sent its own email).
func TestReconciler_DunningRetryFailureSuppressesEmail(t *testing.T) {
	store := newMockReconcileStore(domain.Invoice{
		ID: "inv_1", TenantID: "t1", PaymentStatus: domain.PaymentUnknown,
		StripePaymentIntentID: "pi_dr",
	})
	client := &mockStripeClient{piStates: map[string]PaymentIntentResult{
		"pi_dr": {ID: "pi_dr", Status: "requires_payment_method", Purpose: "dunning_retry"},
	}}
	rec := &recordingSettler{}
	r := NewReconciler(client, store, time.Second)
	r.SetSettler(rec)

	if _, errs := r.Run(context.Background(), 10); len(errs) != 0 {
		t.Fatalf("errs: %v", errs)
	}
	if len(rec.failed) != 1 || !rec.failed[0].suppressEmail {
		t.Fatalf("SettleFailed: got %+v, want suppressEmail=true for a dunning_retry PI", rec.failed)
	}
}

// TestReconciler_StaleProcessingSweptToSucceeded proves the new stale-
// 'processing' sweep (the dropped-webhook backstop) resolves a processing
// invoice whose PI actually succeeded at Stripe.
func TestReconciler_StaleProcessingSweptToSucceeded(t *testing.T) {
	store := newMockReconcileStore(domain.Invoice{
		ID: "inv_p", TenantID: "t1", PaymentStatus: domain.PaymentProcessing,
		StripePaymentIntentID: "pi_p",
	})
	client := &mockStripeClient{piStates: map[string]PaymentIntentResult{
		"pi_p": {ID: "pi_p", Status: "succeeded"},
	}}
	rec := &recordingSettler{}
	r := NewReconciler(client, store, time.Second)
	r.SetSettler(rec)

	resolved, errs := r.Run(context.Background(), 10)
	if len(errs) != 0 {
		t.Fatalf("errs: %v", errs)
	}
	if resolved != 1 || len(rec.succeeded) != 1 || rec.succeeded[0].invoiceID != "inv_p" {
		t.Fatalf("stale-processing sweep: resolved=%d succeeded=%+v, want one SettleSucceeded for inv_p", resolved, rec.succeeded)
	}
}

// TestReconciler_StillInFlightProcessingSkipped: a processing invoice whose PI
// is genuinely still in flight at Stripe is left alone (no premature settle).
func TestReconciler_StillInFlightProcessingSkipped(t *testing.T) {
	store := newMockReconcileStore(domain.Invoice{
		ID: "inv_p", TenantID: "t1", PaymentStatus: domain.PaymentProcessing,
		StripePaymentIntentID: "pi_p",
	})
	client := &mockStripeClient{piStates: map[string]PaymentIntentResult{
		"pi_p": {ID: "pi_p", Status: "processing"},
	}}
	rec := &recordingSettler{}
	r := NewReconciler(client, store, time.Second)
	r.SetSettler(rec)

	resolved, errs := r.Run(context.Background(), 10)
	if len(errs) != 0 {
		t.Fatalf("errs: %v", errs)
	}
	if resolved != 0 || len(rec.succeeded) != 0 || len(rec.failed) != 0 {
		t.Fatalf("still-in-flight processing must be skipped: resolved=%d succ=%+v fail=%+v", resolved, rec.succeeded, rec.failed)
	}
}

// TestReconciler_RaceGuardSkipsWebhookWinner: the sweep listed a stale snapshot,
// but the webhook settled the invoice during the GetPaymentIntent round-trip.
// The fresh re-read sees it already paid → the reconciler must NOT re-settle
// (no duplicate receipt / dunning-advance).
func TestReconciler_RaceGuardSkipsWebhookWinner(t *testing.T) {
	paidAt := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	store := newMockReconcileStore(domain.Invoice{
		ID: "inv_1", TenantID: "t1", PaymentStatus: domain.PaymentUnknown,
		StripePaymentIntentID: "pi_1",
	})
	// Webhook won during the round-trip: the fresh re-read returns a paid invoice.
	store.getResult = map[string]domain.Invoice{
		"inv_1": {ID: "inv_1", TenantID: "t1", Status: domain.InvoicePaid,
			PaymentStatus: domain.PaymentSucceeded, PaidAt: &paidAt, StripePaymentIntentID: "pi_1"},
	}
	client := &mockStripeClient{piStates: map[string]PaymentIntentResult{
		"pi_1": {ID: "pi_1", Status: "canceled"}, // Stripe says failed, but the webhook already settled it paid
	}}
	rec := &recordingSettler{}
	r := NewReconciler(client, store, time.Second)
	r.SetSettler(rec)

	resolved, errs := r.Run(context.Background(), 10)
	if len(errs) != 0 {
		t.Fatalf("errs: %v", errs)
	}
	if resolved != 0 || len(rec.failed) != 0 || len(rec.succeeded) != 0 {
		t.Fatalf("race guard failed: resolved=%d failed=%+v succeeded=%+v — must skip an already-settled invoice", resolved, rec.failed, rec.succeeded)
	}
	// The skip must still RECORD the observation (verbatim provider status).
	// Pre-fix this branch wrote nothing, so a row that keeps re-entering the
	// sweep's list (invoice status terminal, payment_status still in-flight on
	// disk — a shape only seed/ops artifacts produce) was polled every tick
	// with provider_synced_at NULL and permanently headed the LIMITed queue:
	// found live on two rows monopolising the walk DB's queue head since May.
	got := store.byID["inv_1"]
	if got.ProviderPaymentStatus != "canceled" {
		t.Errorf("provider observation = %q, want %q — the already-settled skip must stamp the observation or the row heads the rotation queue forever", got.ProviderPaymentStatus, "canceled")
	}
}
