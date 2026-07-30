package payment

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
)

type mockReconcileStore struct {
	unknowns []domain.Invoice
	byID     map[string]*domain.Invoice
	// getResult, when set for an id, overrides Get's return — used to simulate
	// a webhook winning the race during the GetPaymentIntent round-trip (the
	// sweep listed a stale snapshot; Get re-reads the now-settled invoice).
	getResult map[string]domain.Invoice
}

func newMockReconcileStore(unknowns ...domain.Invoice) *mockReconcileStore {
	byID := make(map[string]*domain.Invoice, len(unknowns))
	for i := range unknowns {
		byID[unknowns[i].ID] = &unknowns[i]
	}
	return &mockReconcileStore{unknowns: unknowns, byID: byID}
}

func (m *mockReconcileStore) ListUnknownPayments(_ context.Context, _ time.Time, _ int) ([]domain.Invoice, error) {
	var out []domain.Invoice
	for _, inv := range m.unknowns {
		if inv.PaymentStatus == domain.PaymentUnknown {
			out = append(out, inv)
		}
	}
	return out, nil
}

func (m *mockReconcileStore) UpdatePayment(_ context.Context, _, id string, ps domain.InvoicePaymentStatus, piID, errMsg string, _ *time.Time) (domain.Invoice, error) {
	inv, ok := m.byID[id]
	if !ok {
		return domain.Invoice{}, errs.ErrNotFound
	}
	inv.PaymentStatus = ps
	inv.StripePaymentIntentID = piID
	inv.LastPaymentError = errMsg
	return *inv, nil
}

func (m *mockReconcileStore) RecordChargeAttempt(_ context.Context, _ string, _ domain.InvoiceChargeAttempt) error {
	return nil
}

func (m *mockReconcileStore) MarkPaid(_ context.Context, _, id string, piID string, paidAt time.Time) (domain.Invoice, error) {
	inv, ok := m.byID[id]
	if !ok {
		return domain.Invoice{}, errs.ErrNotFound
	}
	inv.PaymentStatus = domain.PaymentSucceeded
	inv.Status = domain.InvoicePaid
	inv.StripePaymentIntentID = piID
	inv.PaidAt = &paidAt
	inv.AmountDueCents = 0
	return *inv, nil
}

func (m *mockReconcileStore) ListProcessingPayments(_ context.Context, _ time.Time, _ int) ([]domain.Invoice, error) {
	var out []domain.Invoice
	for _, inv := range m.unknowns {
		if inv.PaymentStatus == domain.PaymentProcessing {
			out = append(out, inv)
		}
	}
	return out, nil
}

func (m *mockReconcileStore) Get(_ context.Context, _, id string) (domain.Invoice, error) {
	if m.getResult != nil {
		if inv, ok := m.getResult[id]; ok {
			return inv, nil
		}
	}
	inv, ok := m.byID[id]
	if !ok {
		return domain.Invoice{}, errs.ErrNotFound
	}
	return *inv, nil
}

func TestReconciler_SucceededResolvesUnknown(t *testing.T) {
	store := newMockReconcileStore(domain.Invoice{
		ID: "inv_1", TenantID: "t1", PaymentStatus: domain.PaymentUnknown,
		StripePaymentIntentID: "pi_ambig_1",
	})
	client := &mockStripeClient{
		piStates: map[string]PaymentIntentResult{
			"pi_ambig_1": {ID: "pi_ambig_1", Status: "succeeded"},
		},
	}
	r := NewReconciler(client, store, time.Second)

	resolved, errs := r.Run(context.Background(), 10)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if resolved != 1 {
		t.Errorf("resolved: got %d, want 1", resolved)
	}
	got := store.byID["inv_1"]
	if got.PaymentStatus != domain.PaymentSucceeded {
		t.Errorf("payment_status: got %q, want succeeded", got.PaymentStatus)
	}
	if got.Status != domain.InvoicePaid {
		t.Errorf("status: got %q, want paid", got.Status)
	}
}

func TestReconciler_CanceledMarksFailed(t *testing.T) {
	store := newMockReconcileStore(domain.Invoice{
		ID: "inv_2", TenantID: "t1", PaymentStatus: domain.PaymentUnknown,
		StripePaymentIntentID: "pi_ambig_2",
	})
	client := &mockStripeClient{
		piStates: map[string]PaymentIntentResult{
			"pi_ambig_2": {ID: "pi_ambig_2", Status: "canceled"},
		},
	}
	r := NewReconciler(client, store, time.Second)

	resolved, errs := r.Run(context.Background(), 10)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if resolved != 1 {
		t.Errorf("resolved: got %d, want 1", resolved)
	}
	if got := store.byID["inv_2"].PaymentStatus; got != domain.PaymentFailed {
		t.Errorf("payment_status: got %q, want failed", got)
	}
}

func TestReconciler_StillInFlightSkipped(t *testing.T) {
	store := newMockReconcileStore(domain.Invoice{
		ID: "inv_3", TenantID: "t1", PaymentStatus: domain.PaymentUnknown,
		StripePaymentIntentID: "pi_ambig_3",
	})
	client := &mockStripeClient{
		piStates: map[string]PaymentIntentResult{
			"pi_ambig_3": {ID: "pi_ambig_3", Status: "processing"},
		},
	}
	r := NewReconciler(client, store, time.Second)

	resolved, errs := r.Run(context.Background(), 10)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if resolved != 0 {
		t.Errorf("resolved: got %d, want 0 (still in flight)", resolved)
	}
	if got := store.byID["inv_3"].PaymentStatus; got != domain.PaymentUnknown {
		t.Errorf("payment_status: got %q, want unknown (unchanged)", got)
	}
}

func TestReconciler_NoPaymentIntentIDMarksFailed(t *testing.T) {
	// The ambiguous error came back without a PI ID — we cannot query
	// Stripe. Mark failed so downstream (dunning / operator) can decide.
	store := newMockReconcileStore(domain.Invoice{
		ID: "inv_4", TenantID: "t1", PaymentStatus: domain.PaymentUnknown,
	})
	r := NewReconciler(&mockStripeClient{}, store, time.Second)

	resolved, errs := r.Run(context.Background(), 10)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if resolved != 1 {
		t.Errorf("resolved: got %d, want 1", resolved)
	}
	got := store.byID["inv_4"]
	if got.PaymentStatus != domain.PaymentFailed {
		t.Errorf("payment_status: got %q, want failed", got.PaymentStatus)
	}
	if got.LastPaymentError == "" {
		t.Error("last_payment_error should explain why we gave up")
	}
}

type errStripeClient struct{ mockStripeClient }

func (e *errStripeClient) GetPaymentIntent(_ context.Context, _ string) (PaymentIntentResult, error) {
	return PaymentIntentResult{}, fmt.Errorf("stripe 502 on reconcile")
}

func TestReconciler_StripeErrorLeavesUnknown(t *testing.T) {
	// Stripe 5xx on the reconcile query itself — we must not flip state.
	store := newMockReconcileStore(domain.Invoice{
		ID: "inv_5", TenantID: "t1", PaymentStatus: domain.PaymentUnknown,
		StripePaymentIntentID: "pi_ambig_5",
	})
	r := NewReconciler(&errStripeClient{}, store, time.Second)

	resolved, errs := r.Run(context.Background(), 10)
	if resolved != 0 {
		t.Errorf("resolved: got %d, want 0", resolved)
	}
	if len(errs) != 1 {
		t.Fatalf("expected 1 per-invoice error, got %d", len(errs))
	}
	if got := store.byID["inv_5"].PaymentStatus; got != domain.PaymentUnknown {
		t.Errorf("payment_status: got %q, want unknown (unchanged on Stripe error)", got)
	}
}
