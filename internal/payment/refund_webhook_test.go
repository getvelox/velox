package payment

import (
	"context"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
)

func TestMapStripeRefundStatus(t *testing.T) {
	t.Parallel()
	cases := map[string]domain.RefundStatus{
		"succeeded":          domain.RefundSucceeded,
		"failed":             domain.RefundFailed,
		"canceled":           domain.RefundFailed, // money returned to platform → operator-actionable
		"pending":            domain.RefundPending,
		"requires_action":    domain.RefundPending,
		"weird_future_value": domain.RefundPending, // unknown → in-flight; the webhook corrects later
	}
	for in, want := range cases {
		if got := mapStripeRefundStatus(in); got != want {
			t.Errorf("mapStripeRefundStatus(%q) = %q, want %q", in, got, want)
		}
	}
}

type fakeRefundUpdaterCall struct {
	refundID string
	status   domain.RefundStatus
}

type fakeRefundUpdater struct {
	calls []fakeRefundUpdaterCall
	err   error
}

func (f *fakeRefundUpdater) ApplyRefundWebhook(_ context.Context, _, refundID string, status domain.RefundStatus, _ string) error {
	f.calls = append(f.calls, fakeRefundUpdaterCall{refundID: refundID, status: status})
	return f.err
}

// TestHandleWebhook_RefundUpdated_AppliesStatus: a refund webhook re-parses the
// Refund object and applies the mapped status to the matching credit note.
func TestHandleWebhook_RefundUpdated_AppliesStatus(t *testing.T) {
	stripe := NewStripe(&mockStripeClient{}, newMockInvoiceUpdater(), newMockWebhookStore(), nil)
	updater := &fakeRefundUpdater{}
	stripe.SetRefundStatusUpdater(updater)

	raw := `{"data":{"object":{"id":"re_123","status":"failed","created":1000,"payment_intent":{"id":"pi_x"}}}}`
	err := stripe.HandleWebhook(context.Background(), "t1", domain.StripeWebhookEvent{
		StripeEventID: "evt_re_1",
		EventType:     "refund.failed",
		Payload:       map[string]any{"raw": raw},
	})
	if err != nil {
		t.Fatalf("HandleWebhook: %v", err)
	}
	if len(updater.calls) != 1 {
		t.Fatalf("ApplyRefundWebhook calls: got %d, want 1", len(updater.calls))
	}
	if updater.calls[0].refundID != "re_123" || updater.calls[0].status != domain.RefundFailed {
		t.Errorf("got %+v, want {re_123 failed}", updater.calls[0])
	}
}

// TestHandleWebhook_RefundUpdated_ForeignRefundAcked: a refund Velox didn't
// create (ErrNotFound) that is OLD (well outside the race window) is ack'd
// permanently — no error, no credit-note fabrication.
func TestHandleWebhook_RefundUpdated_ForeignRefundAcked(t *testing.T) {
	stripe := NewStripe(&mockStripeClient{}, newMockInvoiceUpdater(), newMockWebhookStore(), nil)
	updater := &fakeRefundUpdater{err: errs.ErrNotFound}
	stripe.SetRefundStatusUpdater(updater)

	// created=1000 (1970) → far older than the race window → ack permanently.
	raw := `{"data":{"object":{"id":"re_foreign","status":"succeeded","created":1000,"payment_intent":{"id":"pi_x"}}}}`
	err := stripe.HandleWebhook(context.Background(), "t1", domain.StripeWebhookEvent{
		StripeEventID: "evt_re_2",
		EventType:     "refund.updated",
		Payload:       map[string]any{"raw": raw},
	})
	if err != nil {
		t.Errorf("foreign refund should be ack'd (nil error), got %v", err)
	}
}
