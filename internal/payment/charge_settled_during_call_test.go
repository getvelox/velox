package payment

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// paidMidCharge seeds a store whose row was paid offline while the charge was
// in flight — which bumped charge_attempt_seq — and returns it with the
// pre-call snapshot the charge started from (seq 0). The mock store models the
// real attempt-seq CAS; the SQL is proven in
// internal/invoice/charge_outcome_guard_integration_test.go.
func paidMidCharge() (*mockInvoiceUpdater, domain.Invoice) {
	base := newMockInvoiceUpdater()
	paidAt := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	base.invoices["inv_1"] = domain.Invoice{
		ID: "inv_1", TenantID: "t1", CustomerID: "cus_1",
		Status: domain.InvoicePaid, PaymentStatus: domain.PaymentSucceeded,
		StripePaymentIntentID: "out_of_band:2026-10-10T09:00:00Z", PaidAt: &paidAt,
		Currency: "USD", ChargeAttemptSeq: 1,
	}
	return base, pendingSnapshot()
}

// declinedMidCharge: this attempt's own decline webhook recorded failed/pi_X
// (bumping the seq) and sent its notice before the charge call returned — the
// store sets failure_notified_pi in the same write, so the fixture does too.
func declinedMidCharge() (*mockInvoiceUpdater, domain.Invoice) {
	base := newMockInvoiceUpdater()
	row := pendingSnapshot()
	row.PaymentStatus = domain.PaymentFailed
	row.StripePaymentIntentID = "pi_X"
	row.ChargeAttemptSeq = 1
	base.invoices["inv_1"] = row
	base.byPI["pi_X"] = "inv_1"
	base.failNotedPI["inv_1"] = "pi_X"
	base.failedEventEnqueues = 1
	return base, pendingSnapshot()
}

func pendingSnapshot() domain.Invoice {
	return domain.Invoice{
		ID: "inv_1", TenantID: "t1", CustomerID: "cus_1",
		Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending,
		AmountDueCents: 5000, Currency: "USD",
	}
}

// A decline whose invoice was paid during the call must not start dunning:
// StartDunning has no paid check, so gating on the pre-call snapshot dunned a
// paid invoice — retries, warning emails, possibly a cancel final action.
func TestChargeInvoice_DeclineAfterMidChargePayment_NoDunning(t *testing.T) {
	invoices, snapshot := paidMidCharge()
	client := &mockStripeClient{shouldFail: true, failErr: &PaymentError{
		Message: "card_declined", PaymentIntentID: "pi_decline", Unknown: false,
	}}
	dunning := &recordingDunningStarter{}
	stripe := NewStripe(client, invoices, newMockWebhookStore(), nil, dunning)

	if _, err := stripe.ChargeInvoice(context.Background(), "t1", snapshot, "cus_stripe", "pm_test"); err == nil {
		t.Fatal("want the decline returned: it is the truth about this attempt")
	}
	if got := invoices.invoices["inv_1"]; got.Status != domain.InvoicePaid || got.StripePaymentIntentID != "out_of_band:2026-10-10T09:00:00Z" {
		t.Fatalf("paid row overwritten: %s/%s/%q", got.Status, got.PaymentStatus, got.StripePaymentIntentID)
	}
	if len(dunning.calls) != 0 {
		t.Fatalf("StartDunning called %d times on an invoice paid during the charge; want 0", len(dunning.calls))
	}
}

// A processing PaymentIntent whose invoice was paid during the call returns
// the paid row, not a processing one, so the caller sees what is on disk.
func TestChargeInvoice_ProcessingAfterMidChargePayment_ReturnsPaidRow(t *testing.T) {
	invoices, snapshot := paidMidCharge()
	client := &mockStripeClient{piID: "pi_async", chargeStatus: "processing"}
	stripe := NewStripe(client, invoices, newMockWebhookStore(), nil, &recordingDunningStarter{})

	got, err := stripe.ChargeInvoice(context.Background(), "t1", snapshot, "cus_stripe", "pm_test")
	if err != nil {
		t.Fatalf("ChargeInvoice: %v", err)
	}
	if got.Status != domain.InvoicePaid || got.PaymentStatus != domain.PaymentSucceeded ||
		got.StripePaymentIntentID != "out_of_band:2026-10-10T09:00:00Z" {
		t.Fatalf("returned %s/%s/%s; want the paid row with the offline payment's id",
			got.Status, got.PaymentStatus, got.StripePaymentIntentID)
	}
}

// lockedBuf is a goroutine-safe log sink.
type lockedBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func captureLogs(t *testing.T) *lockedBuf {
	t.Helper()
	out := &lockedBuf{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(out, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return out
}

// The "invoice is parked" CRITICAL tells an operator to go find a charge in
// Stripe. It is announced only once the park is on disk: a timed-out, unnamed
// charge whose invoice was paid during the call parks nothing.
func TestChargeInvoice_UnnamedTimeout_ParkAnnouncedOnlyWhenParked(t *testing.T) {
	const parked = "this invoice is parked"
	timeout := &PaymentError{Message: "context deadline exceeded", Unknown: true}

	t.Run("paid during the call: not announced", func(t *testing.T) {
		logs := captureLogs(t)
		invoices, snapshot := paidMidCharge()
		stripe := NewStripe(&mockStripeClient{shouldFail: true, failErr: timeout}, invoices, newMockWebhookStore(), nil, nil)
		_, _ = stripe.ChargeInvoice(context.Background(), "t1", snapshot, "cus_stripe", "pm_test")
		if strings.Contains(logs.String(), parked) {
			t.Fatalf("park announced for an invoice that was paid, not parked:\n%s", logs)
		}
	})

	t.Run("own decline recorded during the call: not parked, not announced", func(t *testing.T) {
		logs := captureLogs(t)
		invoices, snapshot := declinedMidCharge()
		stripe := NewStripe(&mockStripeClient{shouldFail: true, failErr: timeout}, invoices, newMockWebhookStore(), nil, nil)
		_, _ = stripe.ChargeInvoice(context.Background(), "t1", snapshot, "cus_stripe", "pm_test")
		if got := invoices.invoices["inv_1"]; got.PaymentStatus != domain.PaymentFailed || got.StripePaymentIntentID != "pi_X" {
			t.Fatalf("recorded decline overwritten: %s/%q, want failed/pi_X (still retryable by dunning)", got.PaymentStatus, got.StripePaymentIntentID)
		}
		if strings.Contains(logs.String(), parked) {
			t.Fatalf("park announced for an invoice whose decline was recorded:\n%s", logs)
		}
	})

	t.Run("control, nothing recorded during the call: parked and announced", func(t *testing.T) {
		logs := captureLogs(t)
		invoices := newMockInvoiceUpdater()
		snapshot := pendingSnapshot()
		invoices.invoices["inv_1"] = snapshot
		stripe := NewStripe(&mockStripeClient{shouldFail: true, failErr: timeout}, invoices, newMockWebhookStore(), nil, nil)
		_, _ = stripe.ChargeInvoice(context.Background(), "t1", snapshot, "cus_stripe", "pm_test")
		if !strings.Contains(logs.String(), parked) {
			t.Fatalf("park not announced for an invoice that did park:\n%s", logs)
		}
	})
}

// The stamp must carry the seq the attempt's idempotency key was built from,
// not a constant: a dunning retry starts from an invoice that already records
// earlier attempts. With nothing recorded during the call, its decline lands.
func TestChargeInvoice_RetryAfterEarlierAttempts_StampLands(t *testing.T) {
	invoices := newMockInvoiceUpdater()
	snapshot := pendingSnapshot()
	snapshot.PaymentStatus = domain.PaymentFailed
	snapshot.StripePaymentIntentID = "pi_W"
	snapshot.ChargeAttemptSeq = 3
	invoices.invoices["inv_1"] = snapshot
	client := &mockStripeClient{shouldFail: true, failErr: &PaymentError{
		Message: "card_declined", PaymentIntentID: "pi_X", Unknown: false,
	}}
	stripe := NewStripe(client, invoices, newMockWebhookStore(), nil, &recordingDunningStarter{})

	_, _ = stripe.ChargeInvoice(context.Background(), "t1", snapshot, "cus_stripe", "pm_test")
	if got := invoices.invoices["inv_1"]; got.StripePaymentIntentID != "pi_X" || got.ChargeAttemptSeq != 4 {
		t.Fatalf("row %s/%q/seq %d, want failed/pi_X/seq 4 — the retry's decline was not recorded", got.PaymentStatus, got.StripePaymentIntentID, got.ChargeAttemptSeq)
	}
}
