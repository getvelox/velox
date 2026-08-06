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
// instant, whether the settle happens inline (charge response), via the
// wall-side webhook (which can't see the caller's ctx and recovers the
// anchor from the PI's velox_anchor_at metadata), or via the RECONCILER,
// which recovers the same key off a retrieved PI (ADR-049 Phase 2).
//
// All three must agree, because which one runs is decided by whether a
// webhook happened to be delivered — never by anything about the charge.

func TestChargeInvoice_SimBoundCtx_AnchorsPaidAtAndPIMetadata(t *testing.T) {
	contracted := time.Date(2027, 3, 7, 18, 30, 0, 0, time.UTC)
	ctx := clock.WithSim(context.Background(), clock.Sim{At: contracted, TestClockID: "clk_1"})

	client := &mockStripeClient{piID: "pi_anchor", chargeStatus: "succeeded"}
	invoices := newMockInvoiceUpdater()
	invoices.invoices["inv_1"] = finalizedPendingInvoice()
	s := NewStripe(client, invoices, newMockWebhookStore(), nil, &recordingDunningStarter{})

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

	ctx := withSettleAnchor(context.Background(), contracted)
	if err := s.SettleSucceeded(ctx, "t1", invoices.invoices["inv_1"], "pi_wh", 5000, SourceWebhook); err != nil {
		t.Fatalf("SettleSucceeded: %v", err)
	}
	stored := invoices.invoices["inv_1"]
	if stored.PaidAt == nil || !stored.PaidAt.Equal(contracted) {
		t.Errorf("paid_at: got %v, want the anchored %v (the bind-time clock must not win)", stored.PaidAt, contracted)
	}
}

// anchorCapturingSettler records the anchor the reconciler put on the ctx it
// handed to the settlement primitive. That ctx value is exactly what
// SettleSucceeded consults (settlement.go: `if a, ok := settleAnchorFrom(ctx)`),
// so capturing it here asserts the same thing the sibling tests above assert
// through paid_at, without needing the primitive's whole store graph.
type anchorCapturingSettler struct {
	anchor    time.Time
	hadAnchor bool
	calls     int
}

func (a *anchorCapturingSettler) SettleSucceeded(ctx context.Context, _ string, _ domain.Invoice, _ string, _ int64, _ SettlementSource) error {
	a.calls++
	a.anchor, a.hadAnchor = settleAnchorFrom(ctx)
	return nil
}

func (a *anchorCapturingSettler) SettleFailed(context.Context, string, domain.Invoice, string, string, bool, SettlementSource) error {
	return nil
}

// TestReconciler_RecoversChargeAnchorFromPI is the third leg of the anchor
// contract. The reconciler runs precisely when the webhook that carries the
// anchor was DROPPED, so if it settles without one it re-introduces the exact
// drift the anchor exists to prevent: SettleSucceeded's invoice-pin binding
// resolves the clock's CURRENT frozen_time, which after an advance is the
// advance target rather than the instant the charge fired.
//
// Before the fix the reconciler passed a bare ctx and this failed with
// hadAnchor=false — the same charge produced a different paid_at depending
// only on whether Stripe's webhook was delivered.
func TestReconciler_RecoversChargeAnchorFromPI(t *testing.T) {
	contracted := time.Date(2027, 3, 7, 18, 30, 0, 0, time.UTC)
	store := newMockReconcileStore(domain.Invoice{
		ID: "inv_1", TenantID: "t1", PaymentStatus: domain.PaymentUnknown,
		StripePaymentIntentID: "pi_1",
	})
	client := &mockStripeClient{piStates: map[string]PaymentIntentResult{
		"pi_1": {ID: "pi_1", Status: "succeeded", AmountReceivedCents: 5000, AnchorAt: contracted},
	}}
	settler := &anchorCapturingSettler{}
	r := NewReconciler(client, store, time.Second)
	r.SetSettler(settler)

	if _, errs := r.Run(context.Background(), 10); len(errs) != 0 {
		t.Fatalf("errs: %v", errs)
	}
	if settler.calls != 1 {
		t.Fatalf("SettleSucceeded calls: got %d, want 1", settler.calls)
	}
	if !settler.hadAnchor {
		t.Fatal("reconciler settled with NO anchor — paid_at would land on the clock's current frozen_time, not the instant the charge fired")
	}
	if !settler.anchor.Equal(contracted) {
		t.Errorf("anchor: got %v, want the contracted instant %v", settler.anchor, contracted)
	}
}

// A wall-clock charge carries no velox_anchor_at, and must not acquire one —
// the pin binding (or plain wall now) has to stay in charge there.
func TestReconciler_WallChargeSettlesWithoutAnchor(t *testing.T) {
	store := newMockReconcileStore(domain.Invoice{
		ID: "inv_1", TenantID: "t1", PaymentStatus: domain.PaymentUnknown,
		StripePaymentIntentID: "pi_1",
	})
	client := &mockStripeClient{piStates: map[string]PaymentIntentResult{
		"pi_1": {ID: "pi_1", Status: "succeeded", AmountReceivedCents: 5000},
	}}
	settler := &anchorCapturingSettler{}
	r := NewReconciler(client, store, time.Second)
	r.SetSettler(settler)

	if _, errs := r.Run(context.Background(), 10); len(errs) != 0 {
		t.Fatalf("errs: %v", errs)
	}
	if settler.hadAnchor {
		t.Errorf("wall charge acquired an anchor %v — nothing contracted it", settler.anchor)
	}
}

func TestAnchorFromPIMetadata(t *testing.T) {
	contracted := time.Date(2027, 3, 7, 18, 30, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		meta map[string]string
		want time.Time
	}{
		{"valid", map[string]string{"velox_anchor_at": contracted.Format(time.RFC3339Nano)}, contracted},
		{"absent", map[string]string{"velox_purpose": "dunning_retry"}, time.Time{}},
		{"empty string", map[string]string{"velox_anchor_at": ""}, time.Time{}},
		// Degrade to the pin binding rather than blocking a settlement: a
		// malformed anchor must never keep money in an unresolved state.
		{"malformed", map[string]string{"velox_anchor_at": "not-a-time"}, time.Time{}},
		{"nil map", nil, time.Time{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := anchorFromPIMetadata(tc.meta); !got.Equal(tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}
