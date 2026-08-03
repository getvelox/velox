package creditnote

import (
	"context"
	"errors"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
)

// TestIssue_RecordsTruePendingRefundStatus locks the core fix: when Stripe
// returns the create-time refund as `pending` (async settlement), Issue() must
// record `pending`, NOT a blanket `succeeded`. A blanket succeeded is the
// false-success bug — a pending refund can still flip to failed later.
func TestIssue_RecordsTruePendingRefundStatus(t *testing.T) {
	t.Parallel()
	svc, store, _, refunder := setupRefundSvc(t)
	refunder.returnStatus = domain.RefundPending // Stripe: async, still settling

	cn, err := svc.CreateRefund(context.Background(), "t1", RefundInput{InvoiceID: "inv_paid", Reason: "requested_by_customer"})
	if err != nil {
		t.Fatalf("CreateRefund: %v", err)
	}
	if cn.RefundStatus != domain.RefundPending {
		t.Errorf("refund_status: got %q, want pending (must record Stripe's true status, not blanket succeeded)", cn.RefundStatus)
	}
	if store.notes[cn.ID].RefundStatus != domain.RefundPending {
		t.Errorf("persisted refund_status: got %q, want pending", store.notes[cn.ID].RefundStatus)
	}
}

// TestApplyRefundWebhook_FlipsThenMonotonic: the webhook is the source of truth
// for the async outcome. A pending refund that the bank rejects flips to failed;
// a stale, out-of-order `pending` webhook arriving afterward must NOT clobber the
// terminal failed.
func TestApplyRefundWebhook_FlipsThenMonotonic(t *testing.T) {
	t.Parallel()
	svc, store, _, refunder := setupRefundSvc(t)
	refunder.returnStatus = domain.RefundPending

	cn, err := svc.CreateRefund(context.Background(), "t1", RefundInput{InvoiceID: "inv_paid", Reason: "requested_by_customer"})
	if err != nil {
		t.Fatalf("CreateRefund: %v", err)
	}
	rid := cn.StripeRefundID
	if rid == "" {
		t.Fatal("expected a stripe_refund_id on the issued refund CN")
	}

	// webhook: pending → failed (bank reject / insufficient platform balance)
	if err := svc.ApplyRefundWebhook(context.Background(), "t1", rid, domain.RefundFailed, "lost_or_stolen_card"); err != nil {
		t.Fatalf("ApplyRefundWebhook(failed): %v", err)
	}
	if got := store.notes[cn.ID].RefundStatus; got != domain.RefundFailed {
		t.Fatalf("after webhook: got %q, want failed", got)
	}

	// stale out-of-order pending must NOT clobber the terminal failed
	if err := svc.ApplyRefundWebhook(context.Background(), "t1", rid, domain.RefundPending, ""); err != nil {
		t.Fatalf("ApplyRefundWebhook(stale pending): %v", err)
	}
	if got := store.notes[cn.ID].RefundStatus; got != domain.RefundFailed {
		t.Errorf("monotonic violated: stale pending clobbered terminal — got %q, want failed", got)
	}
}

// TestApplyRefundWebhook_UnknownRefundIsNotFound: a webhook for a refund Velox
// didn't create (Stripe dashboard / direct API) returns ErrNotFound so the
// caller acks it permanently — never fabricates a credit note.
func TestApplyRefundWebhook_UnknownRefundIsNotFound(t *testing.T) {
	t.Parallel()
	svc, _, _, _ := setupRefundSvc(t)
	err := svc.ApplyRefundWebhook(context.Background(), "t1", "re_foreign_dashboard", domain.RefundSucceeded, "")
	if !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("foreign refund: got %v, want ErrNotFound", err)
	}
}

// TestApplyRefundWebhook_FailedAbsorbing: Stripe can move a refund succeeded→failed
// (bank rejects an initially-accepted refund), so a later `failed` MUST win over a
// recorded `succeeded`; and once `failed`, a stale out-of-order `succeeded`
// redelivery must NOT un-fail it (failed is absorbing). This is the money-safety
// invariant — a refund the customer never received must not read as succeeded.
func TestApplyRefundWebhook_FailedAbsorbing(t *testing.T) {
	t.Parallel()
	svc, store, _, refunder := setupRefundSvc(t)
	refunder.returnStatus = domain.RefundSucceeded // create-time: Stripe said succeeded

	cn, err := svc.CreateRefund(context.Background(), "t1", RefundInput{InvoiceID: "inv_paid", Reason: "requested_by_customer"})
	if err != nil {
		t.Fatalf("CreateRefund: %v", err)
	}
	rid := cn.StripeRefundID
	if store.notes[cn.ID].RefundStatus != domain.RefundSucceeded {
		t.Fatalf("precondition: want succeeded, got %q", store.notes[cn.ID].RefundStatus)
	}

	// succeeded → failed (real Stripe transition) MUST win
	if err := svc.ApplyRefundWebhook(context.Background(), "t1", rid, domain.RefundFailed, "lost_or_stolen_card"); err != nil {
		t.Fatalf("ApplyRefundWebhook(failed): %v", err)
	}
	if got := store.notes[cn.ID].RefundStatus; got != domain.RefundFailed {
		t.Fatalf("succeeded→failed must win: got %q, want failed", got)
	}

	// stale 'succeeded' redelivery must NOT un-fail it
	if err := svc.ApplyRefundWebhook(context.Background(), "t1", rid, domain.RefundSucceeded, ""); err != nil {
		t.Fatalf("ApplyRefundWebhook(stale succeeded): %v", err)
	}
	if got := store.notes[cn.ID].RefundStatus; got != domain.RefundFailed {
		t.Errorf("failed must be absorbing: stale succeeded clobbered it — got %q, want failed", got)
	}
}
