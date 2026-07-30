package payment

import (
	"context"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
)

// The contracted-instant anchor (ADR-030 rule, timeline-audit finding 1
// deep half): a charge fired at a simulated instant — a dunning retry's
// next_action_at, a cycle's close — must settle with paid_at at THAT
// instant, whether the settle happens inline (charge response) or via the
// wall-side webhook (which can't see the caller's ctx and recovers the
// anchor from the PI's velox_anchor_at metadata).

func TestChargeInvoice_SimBoundCtx_AnchorsPaidAtAndPIMetadata(t *testing.T) {
	contracted := time.Date(2027, 3, 7, 18, 30, 0, 0, time.UTC)
	ctx := clock.WithSim(context.Background(), clock.Sim{At: contracted, TestClockID: "clk_1"})

	client := &mockStripeClient{piID: "pi_anchor", chargeStatus: "succeeded"}
	invoices := newMockInvoiceUpdater()
	invoices.invoices["inv_1"] = finalizedPendingInvoice()
	s := NewStripe(client, invoices, newMockWebhookStore(), nil, &recordingDunningStarter{})
	s.SetChargeIntents(newMemChargeIntents())

	if _, err := s.ChargeInvoice(ctx, "t1", invoices.invoices["inv_1"], "cus_stripe_abc", "pm_test"); err != nil {
		t.Fatalf("ChargeInvoice: %v", err)
	}

	// The PI carries the anchor for the async webhook settle.
	if got := client.lastParams.Metadata["velox_anchor_at"]; got != contracted.Format(time.RFC3339Nano) {
		t.Errorf("PI velox_anchor_at: got %q, want %q", got, contracted.Format(time.RFC3339Nano))
	}
	// The inline settle stamped the contracted instant, not wall now and
	// not whatever the invoice pin would re-resolve.
	stored := invoices.invoices["inv_1"]
	if stored.PaidAt == nil || !stored.PaidAt.Equal(contracted) {
		t.Errorf("paid_at: got %v, want the contracted instant %v", stored.PaidAt, contracted)
	}
}

func TestChargeInvoice_WallCtx_NoAnchor(t *testing.T) {
	client := &mockStripeClient{piID: "pi_wall", chargeStatus: "succeeded"}
	invoices := newMockInvoiceUpdater()
	invoices.invoices["inv_1"] = finalizedPendingInvoice()
	s := NewStripe(client, invoices, newMockWebhookStore(), nil, &recordingDunningStarter{})
	s.SetChargeIntents(newMemChargeIntents())

	before := time.Now().UTC()
	if _, err := s.ChargeInvoice(context.Background(), "t1", invoices.invoices["inv_1"], "cus_stripe_abc", "pm_test"); err != nil {
		t.Fatalf("ChargeInvoice: %v", err)
	}
	if _, present := client.lastParams.Metadata["velox_anchor_at"]; present {
		t.Error("wall-clock charges must not stamp velox_anchor_at")
	}
	stored := invoices.invoices["inv_1"]
	if stored.PaidAt == nil || stored.PaidAt.Before(before.Add(-time.Minute)) {
		t.Errorf("wall paid_at should be ~now, got %v", stored.PaidAt)
	}
}

func TestAnchorFromEventPayload(t *testing.T) {
	contracted := time.Date(2027, 3, 7, 18, 30, 0, 0, time.UTC)
	mk := func(meta map[string]any) domain.StripeWebhookEvent {
		return domain.StripeWebhookEvent{Payload: map[string]any{
			"data": map[string]any{"object": map[string]any{"metadata": meta}},
		}}
	}
	if got, ok := anchorFromEventPayload(mk(map[string]any{"velox_anchor_at": contracted.Format(time.RFC3339Nano)})); !ok || !got.Equal(contracted) {
		t.Errorf("present anchor: got %v ok=%v", got, ok)
	}
	if _, ok := anchorFromEventPayload(mk(map[string]any{})); ok {
		t.Error("absent anchor must report false")
	}
	if _, ok := anchorFromEventPayload(mk(map[string]any{"velox_anchor_at": "not-a-time"})); ok {
		t.Error("malformed anchor must report false (fallback, never block a settlement)")
	}
	if _, ok := anchorFromEventPayload(domain.StripeWebhookEvent{Payload: map[string]any{}}); ok {
		t.Error("payload without data.object must report false")
	}
}

func TestSettleSucceeded_CtxAnchorBeatsBindTime(t *testing.T) {
	contracted := time.Date(2027, 3, 7, 18, 30, 0, 0, time.UTC)
	invoices := newMockInvoiceUpdater()
	invoices.invoices["inv_1"] = finalizedPendingInvoice()
	s := NewStripe(&mockStripeClient{}, invoices, newMockWebhookStore(), nil, &recordingDunningStarter{})
	s.SetChargeIntents(newMemChargeIntents())

	ctx := withSettleAnchor(context.Background(), contracted)
	if err := s.SettleSucceeded(ctx, "t1", invoices.invoices["inv_1"], "pi_wh", 5000, SourceWebhook); err != nil {
		t.Fatalf("SettleSucceeded: %v", err)
	}
	stored := invoices.invoices["inv_1"]
	if stored.PaidAt == nil || !stored.PaidAt.Equal(contracted) {
		t.Errorf("paid_at: got %v, want the anchored %v (the bind-time clock must not win)", stored.PaidAt, contracted)
	}
}
