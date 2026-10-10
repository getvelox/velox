package payment

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// flakyUpdater embeds the full mock InvoiceUpdater and fails the first N
// StampChargeOutcome calls, so the load-bearing-write retry can be exercised.
type flakyUpdater struct {
	*mockInvoiceUpdater
	failFirst int
	calls     int
}

func (f *flakyUpdater) StampChargeOutcome(ctx context.Context, tenantID, id string, attemptSeq int64, ps domain.InvoicePaymentStatus, piID, errMsg string) (domain.Invoice, bool, error) {
	f.calls++
	if f.calls <= f.failFirst {
		return domain.Invoice{}, false, errors.New("transient db blip")
	}
	return f.mockInvoiceUpdater.StampChargeOutcome(ctx, tenantID, id, attemptSeq, ps, piID, errMsg)
}

// TestPersistChargeOutcomeWithRetry locks the fix for the swallowed
// PaymentUnknown enrollment write (`_, _ = UpdatePayment`). That write is the
// SOLE thing enrolling a possibly-succeeded PI into the reconciler's
// unknown-payment sweep, so a transient failure must be retried, and a
// permanent failure must surface (so the caller logs loud) rather than be
// silently dropped.
func TestPersistChargeOutcomeWithRetry(t *testing.T) {
	inv := domain.Invoice{ID: "inv_1", TenantID: "t1"}

	t.Run("recovers after transient failures", func(t *testing.T) {
		base := newMockInvoiceUpdater()
		base.invoices["inv_1"] = inv
		f := &flakyUpdater{mockInvoiceUpdater: base, failFirst: 2}

		_, applied, err := persistChargeOutcomeWithRetry(context.Background(), f, "t1", inv, domain.PaymentUnknown, "pi_1", "timeout")
		if err != nil || !applied {
			t.Fatalf("expected recovery after retries, got applied=%v err=%v", applied, err)
		}
		if f.calls != 3 {
			t.Fatalf("calls = %d, want 3 (2 fail + 1 success)", f.calls)
		}
		if base.invoices["inv_1"].PaymentStatus != domain.PaymentUnknown {
			t.Fatalf("payment_status not persisted on recovery: %q", base.invoices["inv_1"].PaymentStatus)
		}
		if base.invoices["inv_1"].StripePaymentIntentID != "pi_1" {
			t.Fatalf("PaymentIntent id not persisted on recovery")
		}
	})

	t.Run("fails loud after exhausting retries", func(t *testing.T) {
		base := newMockInvoiceUpdater()
		base.invoices["inv_1"] = inv
		f := &flakyUpdater{mockInvoiceUpdater: base, failFirst: 99}

		_, _, err := persistChargeOutcomeWithRetry(context.Background(), f, "t1", inv, domain.PaymentUnknown, "pi_1", "timeout")
		if err == nil {
			t.Fatal("expected an error after exhausting retries so the caller fails loud, got nil")
		}
		if f.calls != 3 {
			t.Fatalf("calls = %d, want 3 attempts", f.calls)
		}
	})

	t.Run("a refused stamp is a result, not a failure to retry", func(t *testing.T) {
		base := newMockInvoiceUpdater()
		moved := inv
		moved.ChargeAttemptSeq = 1 // an outcome was recorded during the call
		moved.PaymentStatus = domain.PaymentFailed
		moved.StripePaymentIntentID = "pi_1"
		base.invoices["inv_1"] = moved
		f := &flakyUpdater{mockInvoiceUpdater: base}

		got, applied, err := persistChargeOutcomeWithRetry(context.Background(), f, "t1", inv, domain.PaymentUnknown, "", "timeout")
		if err != nil || applied {
			t.Fatalf("applied=%v err=%v, want a refused stamp with no error", applied, err)
		}
		if f.calls != 1 {
			t.Fatalf("calls = %d, want 1 — a refusal must not be retried", f.calls)
		}
		if got.PaymentStatus != domain.PaymentFailed || got.StripePaymentIntentID != "pi_1" {
			t.Fatalf("returned %s/%q, want the recorded failed/pi_1", got.PaymentStatus, got.StripePaymentIntentID)
		}
	})
}

// ctxCheckingUpdater fails the stamp the way the real store does when the ctx
// it is handed is already done: BeginTx refuses before reaching the database.
type ctxCheckingUpdater struct {
	*mockInvoiceUpdater
}

func (f *ctxCheckingUpdater) StampChargeOutcome(ctx context.Context, tenantID, id string, attemptSeq int64, ps domain.InvoicePaymentStatus, piID, errMsg string) (domain.Invoice, bool, error) {
	if err := ctx.Err(); err != nil {
		return domain.Invoice{}, false, err
	}
	return f.mockInvoiceUpdater.StampChargeOutcome(ctx, tenantID, id, attemptSeq, ps, piID, errMsg)
}

// TestPersistChargeOutcome_CallerCancelledLeavesInvoiceReplayable: when the
// caller's own deadline or cancellation is what made the outcome Unknown, the
// stamp must NOT land. The invoice stays claimable at the same
// charge_attempt_seq, so the next claim rebuilds the same idempotency key and
// Stripe replays the request instead of charging again. Landing it (a
// detached ctx) parks the invoice for good on a charge that may never have
// been sent.
func TestPersistChargeOutcome_CallerCancelledLeavesInvoiceReplayable(t *testing.T) {
	base := newMockInvoiceUpdater()
	inv := domain.Invoice{ID: "inv_1", TenantID: "t1", PaymentStatus: domain.PaymentPending}
	base.invoices["inv_1"] = inv
	f := &ctxCheckingUpdater{mockInvoiceUpdater: base}

	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	<-ctx.Done()

	if _, _, err := persistChargeOutcomeWithRetry(ctx, f, "t1", inv, domain.PaymentUnknown, "", "context deadline exceeded"); err == nil {
		t.Fatal("stamp landed on a cancelled caller ctx; want it dropped so the invoice stays replayable")
	}
	if got := base.invoices["inv_1"]; got.PaymentStatus != domain.PaymentPending || got.ChargeAttemptSeq != 0 {
		t.Fatalf("row moved to %s/seq %d; want pending/seq 0 so the same idempotency key is rebuilt", got.PaymentStatus, got.ChargeAttemptSeq)
	}
}

// lostCommitReplyUpdater applies the first stamp and then reports an error,
// as when the server commits but the connection drops before the reply.
type lostCommitReplyUpdater struct {
	*mockInvoiceUpdater
	calls int
}

func (f *lostCommitReplyUpdater) StampChargeOutcome(ctx context.Context, tenantID, id string, attemptSeq int64, ps domain.InvoicePaymentStatus, piID, errMsg string) (domain.Invoice, bool, error) {
	f.calls++
	got, applied, err := f.mockInvoiceUpdater.StampChargeOutcome(ctx, tenantID, id, attemptSeq, ps, piID, errMsg)
	if f.calls == 1 {
		return domain.Invoice{}, false, errors.New("driver: bad connection")
	}
	return got, applied, err
}

// The retry after a lost commit reply is refused by the first attempt's own
// seq bump. That is this attempt's write, not another payment's: it must
// count as applied, or a real park goes unannounced.
func TestPersistChargeOutcome_LostCommitReplyCountsAsApplied(t *testing.T) {
	base := newMockInvoiceUpdater()
	inv := domain.Invoice{ID: "inv_1", TenantID: "t1", PaymentStatus: domain.PaymentPending, ChargeAttemptSeq: 2}
	base.invoices["inv_1"] = inv
	f := &lostCommitReplyUpdater{mockInvoiceUpdater: base}

	got, applied, err := persistChargeOutcomeWithRetry(context.Background(), f, "t1", inv, domain.PaymentUnknown, "", "connection reset")
	if err != nil || !applied {
		t.Fatalf("applied=%v err=%v, want applied — the row holds this attempt's own outcome", applied, err)
	}
	if f.calls != 2 || got.PaymentStatus != domain.PaymentUnknown || got.ChargeAttemptSeq != 3 {
		t.Fatalf("calls=%d row=%s/seq %d, want 2 calls and unknown/seq 3", f.calls, got.PaymentStatus, got.ChargeAttemptSeq)
	}
}
