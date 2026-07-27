package payment

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
)

type countingDunningStarter struct {
	calls int
	err   error
}

func (c *countingDunningStarter) StartDunning(_ context.Context, _, _, _ string, _ time.Time, _ domain.DunningStartCause) (domain.InvoiceDunningRun, error) {
	c.calls++
	return domain.InvoiceDunningRun{}, c.err
}

// TestStartDunningWithRetry_SkipsInvalidState locks the companion cleanup:
// a deliberate-skip ErrInvalidState (dunning disabled OR not-configured) returns
// success IMMEDIATELY without burning the retry budget — so a no-policy/disabled
// tenant doesn't waste 3 attempts or emit the misleading "operator must start
// manually" ERROR per declined invoice.
func TestStartDunningWithRetry_SkipsInvalidState(t *testing.T) {
	d := &countingDunningStarter{err: errs.InvalidState("dunning not configured — no policy for tenant")}
	if err := startDunningWithRetry(context.Background(), d, "t1", "inv_1", "cus_1", time.Time{}, domain.DunningCausePaymentFailed); err != nil {
		t.Fatalf("InvalidState is a deliberate skip → want nil, got %v", err)
	}
	if d.calls != 1 {
		t.Fatalf("calls = %d, want 1 (a deliberate skip must NOT retry)", d.calls)
	}
}

// TestStartDunningWithRetry_RetriesTransient is the mutation guard: the skip is
// scoped to ErrInvalidState only — a genuine transient error still burns every
// attempt and then propagates (fail loud).
func TestStartDunningWithRetry_RetriesTransient(t *testing.T) {
	d := &countingDunningStarter{err: errors.New("db down")}
	if err := startDunningWithRetry(context.Background(), d, "t1", "inv_1", "cus_1", time.Time{}, domain.DunningCausePaymentFailed); err == nil {
		t.Fatal("a transient error must propagate after retries, got nil")
	}
	if d.calls != 3 {
		t.Fatalf("calls = %d, want 3 (transient errors retry)", d.calls)
	}
}
