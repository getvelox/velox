package invoice_test

import (
	"context"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
)

// failureReport is a webhook- or reconciler-shaped report: no ChargedAtSeq.
func failureReport(pi, msg string) domain.PaymentFailureReport {
	return domain.PaymentFailureReport{PaymentIntentID: pi, Message: msg}
}

// ownDecline is the charge call reporting its own attempt's decline, charged
// at the invoice's current seq — how a new PaymentIntent reaches an invoice
// that holds the previous one.
func ownDecline(t *testing.T, store *invoice.PostgresStore, ctx context.Context, tenantID, id, pi, msg string) domain.PaymentFailureReport {
	t.Helper()
	cur, err := store.Get(ctx, tenantID, id)
	if err != nil {
		t.Fatalf("read seq for own decline: %v", err)
	}
	seq := cur.ChargeAttemptSeq
	return domain.PaymentFailureReport{PaymentIntentID: pi, Message: msg, ChargedAtSeq: &seq}
}
