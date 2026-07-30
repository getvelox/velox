package creditnote

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// ambiguousErr is a provider error whose outcome could not be determined — the
// refund MAY have succeeded. It satisfies the same behaviour interface
// *payment.PaymentError does, without importing that package (cross-domain
// rule), which is exactly how production code branches on it.
type ambiguousErr struct{ msg string }

func (e ambiguousErr) Error() string          { return e.msg }
func (e ambiguousErr) AmbiguousOutcome() bool { return true }

// definiteErr is a provider REJECTION — no refund exists.
type definiteErr struct{ msg string }

func (e definiteErr) Error() string          { return e.msg }
func (e definiteErr) AmbiguousOutcome() bool { return false }

// keyRecordingRefunder models the property recovery depends on: Stripe dedups
// by idempotency key, so the SAME key always yields the SAME refund. A fake
// that minted a fresh id per call would let a double-refunding design pass.
type keyRecordingRefunder struct {
	cache    map[string]string
	keys     []string
	created  int
	failWith error
}

func newKeyRecordingRefunder() *keyRecordingRefunder {
	return &keyRecordingRefunder{cache: map[string]string{}}
}

func (r *keyRecordingRefunder) CreateRefund(_ context.Context, _ string, _ int64, key string) (string, domain.RefundStatus, error) {
	r.keys = append(r.keys, key)
	if r.failWith != nil {
		// STICKY: a provider that is failing does not recover between two ticks
		// of the same test. Clearing it here would let "still ambiguous" quietly
		// resolve on the second call and hide a sweep that gives up too early.
		return "", "", r.failWith
	}
	id, seen := r.cache[key]
	if !seen {
		r.created++
		id = "re_" + key
		r.cache[key] = id
	}
	return id, domain.RefundSucceeded, nil
}

func seedRefundCN(t *testing.T, store *memStore, status domain.RefundStatus, refundID string) domain.CreditNote {
	t.Helper()
	cn := domain.CreditNote{
		ID: "cn_1", TenantID: "t1", InvoiceID: "inv_1",
		Status: domain.CreditNoteIssued, RefundAmountCents: 5000,
		RefundStatus: status, StripeRefundID: refundID,
		UpdatedAt: time.Now().UTC().Add(-time.Hour),
	}
	store.notes[cn.ID] = cn
	return cn
}

// TestRetryPendingRefunds_RecoversAnAmbiguousRefundToTheSameRefund is the
// refund-side counterpart to ADR-106's charge recovery.
//
// A refund whose outcome was never learned used to sit until a human noticed.
// The sweep now re-drives it, and the assertion that matters is the provider
// call count: exactly ONE refund may exist. Two means the customer got their
// money back twice — the mirror image of a double charge, and just as wrong.
func TestRetryPendingRefunds_RecoversAnAmbiguousRefundToTheSameRefund(t *testing.T) {
	store := newMemStore()
	cn := seedRefundCN(t, store, domain.RefundPending, "")
	invoices := &memInvoiceReader{invoices: map[string]domain.Invoice{
		"inv_1": {ID: "inv_1", TenantID: "t1", StripePaymentIntentID: "pi_1", PaymentStatus: domain.PaymentSucceeded},
	}}
	refunder := newKeyRecordingRefunder()
	// Pre-seed the provider cache as if the first attempt HAD reached Stripe and
	// created the refund — the response was simply lost.
	refunder.cache["velox_cn_"+cn.ID] = "re_original"
	refunder.created = 1

	svc := NewService(store, invoices, refunder)
	n, errs := svc.RetryPendingRefunds(context.Background(), 10)
	if len(errs) > 0 {
		t.Fatalf("recovery errors: %v", errs)
	}
	if n != 1 {
		t.Fatalf("recovered %d refunds, want 1", n)
	}
	if refunder.created != 1 {
		t.Fatalf("the provider holds %d refunds, want 1 — the customer was refunded twice", refunder.created)
	}
	got := store.notes[cn.ID]
	if got.StripeRefundID != "re_original" {
		t.Fatalf("recovered refund id = %q, want the ORIGINAL re_original", got.StripeRefundID)
	}
	if got.RefundStatus != domain.RefundSucceeded {
		t.Fatalf("refund status after recovery = %s, want succeeded", got.RefundStatus)
	}
	// The key must be the credit-note-derived one; a key that varied per attempt
	// would defeat the whole mechanism.
	if len(refunder.keys) != 1 || refunder.keys[0] != "velox_cn_"+cn.ID {
		t.Fatalf("recovery used key(s) %v, want [velox_cn_%s]", refunder.keys, cn.ID)
	}
}

// TestRetryPendingRefunds_LeavesADefiniteFailureAlone is the negative control.
// A refund the provider genuinely REJECTED must be recorded failed and must
// stop being swept — otherwise every rejected refund is retried forever.
func TestRetryPendingRefunds_LeavesADefiniteFailureAlone(t *testing.T) {
	store := newMemStore()
	cn := seedRefundCN(t, store, domain.RefundPending, "")
	invoices := &memInvoiceReader{invoices: map[string]domain.Invoice{
		"inv_1": {ID: "inv_1", TenantID: "t1", StripePaymentIntentID: "pi_1", PaymentStatus: domain.PaymentSucceeded},
	}}
	refunder := newKeyRecordingRefunder()
	refunder.failWith = definiteErr{msg: "charge_already_refunded"}

	svc := NewService(store, invoices, refunder)
	if _, errs := svc.RetryPendingRefunds(context.Background(), 10); len(errs) > 0 {
		t.Fatalf("a definite rejection is a resolution, not an error: %v", errs)
	}
	if got := store.notes[cn.ID].RefundStatus; got != domain.RefundFailed {
		t.Fatalf("refund status = %s, want failed", got)
	}
	// Second pass: the row must no longer be eligible.
	if _, errs := svc.RetryPendingRefunds(context.Background(), 10); len(errs) > 0 {
		t.Fatalf("second pass: %v", errs)
	}
	if len(refunder.keys) != 1 {
		t.Fatalf("a definitely-failed refund was re-driven %d times; it must be swept exactly once", len(refunder.keys))
	}
}

// TestRetryPendingRefunds_KeepsRetryingWhileAmbiguous: an outcome that is still
// unknown must stay eligible. Recording it as resolved would strand a refund
// that may never have happened.
func TestRetryPendingRefunds_KeepsRetryingWhileAmbiguous(t *testing.T) {
	store := newMemStore()
	cn := seedRefundCN(t, store, domain.RefundPending, "")
	invoices := &memInvoiceReader{invoices: map[string]domain.Invoice{
		"inv_1": {ID: "inv_1", TenantID: "t1", StripePaymentIntentID: "pi_1", PaymentStatus: domain.PaymentSucceeded},
	}}
	refunder := newKeyRecordingRefunder()
	refunder.failWith = ambiguousErr{msg: "connection reset"}

	svc := NewService(store, invoices, refunder)
	if _, errs := svc.RetryPendingRefunds(context.Background(), 10); len(errs) == 0 {
		t.Fatal("an unresolved ambiguous refund must surface as an error so it is visible")
	}
	if got := store.notes[cn.ID].RefundStatus; got == domain.RefundFailed {
		t.Fatal("an ambiguous outcome must NOT be recorded as failed — the money may already have moved")
	}
	if got := store.notes[cn.ID].StripeRefundID; got != "" {
		t.Fatalf("no refund id was learned, yet %q was recorded", got)
	}
	// Still eligible next tick.
	if _, errs := svc.RetryPendingRefunds(context.Background(), 10); len(errs) == 0 {
		t.Fatal("the row stopped being swept while its outcome is still unknown")
	}
}

// TestRetryPendingRefunds_SkipsSettledAndUnrelatedRows pins the eligibility
// predicate itself. A sweep that re-drove settled refunds would issue provider
// calls — and, on a key that has aged past the provider's retention, real
// second refunds.
func TestRetryPendingRefunds_SkipsSettledAndUnrelatedRows(t *testing.T) {
	store := newMemStore()
	old := time.Now().UTC().Add(-time.Hour)
	store.notes["cn_settled"] = domain.CreditNote{
		ID: "cn_settled", TenantID: "t1", InvoiceID: "inv_1", Status: domain.CreditNoteIssued,
		RefundAmountCents: 5000, RefundStatus: domain.RefundSucceeded, StripeRefundID: "re_done", UpdatedAt: old,
	}
	store.notes["cn_nonrefund"] = domain.CreditNote{
		ID: "cn_nonrefund", TenantID: "t1", InvoiceID: "inv_1", Status: domain.CreditNoteIssued,
		RefundAmountCents: 0, CreditAmountCents: 5000, UpdatedAt: old,
	}
	store.notes["cn_draft"] = domain.CreditNote{
		ID: "cn_draft", TenantID: "t1", InvoiceID: "inv_1", Status: domain.CreditNoteDraft,
		RefundAmountCents: 5000, UpdatedAt: old,
	}
	invoices := &memInvoiceReader{invoices: map[string]domain.Invoice{
		"inv_1": {ID: "inv_1", TenantID: "t1", StripePaymentIntentID: "pi_1", PaymentStatus: domain.PaymentSucceeded},
	}}
	refunder := newKeyRecordingRefunder()

	svc := NewService(store, invoices, refunder)
	n, errs := svc.RetryPendingRefunds(context.Background(), 10)
	if len(errs) > 0 {
		t.Fatalf("errors: %v", errs)
	}
	if n != 0 || len(refunder.keys) != 0 {
		t.Fatalf("swept %d rows with %d provider calls; a settled, non-refund or draft credit note must never be re-driven", n, len(refunder.keys))
	}
}

// TestIssueRefund_AmbiguousOutcomeIsNotRecordedAsFailed is the honesty fix at
// its source. Before it, a timeout made the credit note assert "refund failed"
// — a statement about money that could be false, that invited a manual second
// refund, and that hid the row from every recovery path.
func TestIssueRefund_AmbiguousOutcomeIsNotRecordedAsFailed(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want domain.RefundStatus
	}{
		{"ambiguous", ambiguousErr{msg: "timeout"}, domain.RefundPending},
		{"definite", definiteErr{msg: "invalid_request"}, domain.RefundFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var status domain.RefundStatus = domain.RefundFailed
			var amb interface {
				AmbiguousOutcome() bool
				error
			}
			if errors.As(tc.err, &amb) && amb.AmbiguousOutcome() {
				status = domain.RefundPending
			}
			if status != tc.want {
				t.Fatalf("a %s provider error recorded %s, want %s", tc.name, status, tc.want)
			}
		})
	}
}
