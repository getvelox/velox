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

// CountParkedInvoices mirrors the REAL store's predicate rather than stubbing
// zero. A fake that always answers 0 would let the gauge silently report "no
// parked invoices" forever — the exact drift class that gave an earlier fix zero
// executing coverage.
func (m *mockReconcileStore) CountParkedInvoices(_ context.Context) (int, error) {
	n := 0
	for _, inv := range m.byID {
		if inv.Status == domain.InvoiceFinalized &&
			inv.PaymentStatus == domain.PaymentUnknown &&
			inv.StripePaymentIntentID == "" {
			n++
		}
	}
	return n, nil
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

// TestReconciler_NoPaymentIntentIDStaysUnknown replaces
// TestReconciler_NoPaymentIntentIDMarksFailed, which pinned the behaviour that
// WAS the bug: it asserted the reconciler settles an unresolvable invoice
// failed, under the comment "mark failed so downstream can decide".
//
// Settling failed is exactly what makes the double charge possible. It bumps
// charge_attempt_seq — rotating the Stripe idempotency key — and moves
// payment_status out of 'unknown' into a state every charge-claim predicate
// admits, so the next retry opens a SECOND PaymentIntent beside one that may be
// live and charging. Leaving it at 'unknown' closes that by construction
// (ADR-107).
func TestReconciler_NoPaymentIntentIDStaysUnknown(t *testing.T) {
	store := newMockReconcileStore(domain.Invoice{
		ID: "inv_4", TenantID: "t1", PaymentStatus: domain.PaymentUnknown,
	})
	r := NewReconciler(&mockStripeClient{}, store, time.Second)

	resolved, errs := r.Run(context.Background(), 10)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if resolved != 0 {
		t.Errorf("resolved %d — an invoice whose attempt cannot be named has not been resolved by anything", resolved)
	}
	got := store.byID["inv_4"]
	if got.PaymentStatus != domain.PaymentUnknown {
		t.Fatalf("payment_status moved to %q — leaving 'unknown' is the whole guarantee: every charge-claim predicate excludes it, so no sweep, operator click or dunning retry can open a second PaymentIntent", got.PaymentStatus)
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
