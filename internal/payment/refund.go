package payment

import (
	"context"
	"fmt"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/stripe/stripe-go/v82"
)

// mapStripeRefundStatus collapses Stripe's 5 refund states into Velox's 4-value
// refund_status (DB CHECK: none/pending/succeeded/failed). canceled→failed (the
// money returned to the platform balance, not the customer — operator-actionable);
// requires_action→pending (still in-flight). Shared by CreateRefund (create-time)
// and handleRefundUpdated (the async webhook truth) so both agree. Note: even
// `succeeded` means "submitted to the card network", NOT "on the cardholder
// statement" (5–10 business days, no confirming event).
// Takes the raw Stripe status STRING (stable API values) so the webhook handler
// in stripe.go — which abstracts stripe-go behind StripeClient and doesn't import
// it — can share this exact mapping.
func mapStripeRefundStatus(s string) domain.RefundStatus {
	switch s {
	case "succeeded":
		return domain.RefundSucceeded
	case "failed", "canceled":
		return domain.RefundFailed
	case "pending", "requires_action":
		return domain.RefundPending
	default:
		return domain.RefundPending // unknown → in-flight; the webhook corrects it
	}
}

// StripeRefunder processes refunds via the Stripe API.
// Mode-aware: selects live/test client per ctx livemode.
type StripeRefunder struct {
	clients *StripeClients
}

func NewStripeRefunder(clients *StripeClients) *StripeRefunder {
	if !clients.Has() {
		return nil
	}
	return &StripeRefunder{clients: clients}
}

// CreateRefund creates a Stripe refund for the given PaymentIntent.
//
// `idempotencyKey` is mandatory in production — same key + same params
// returns the existing refund without creating a duplicate. Velox passes
// `velox_cn_<credit_note_id>` so that a credit-note Issue() retry after
// a partial failure (e.g. DB hiccup on the post-refund credit grant
// step) hits Stripe's cache and gets back the original refund_id,
// not a second refund.
//
// Without this protection, the pre-fix shape was: Stripe refund
// succeeds → in-memory refund_id set → credit grant fails → function
// returns error → CN stays draft → operator retries → Stripe called
// again with no idempotency key → DUPLICATE refund → customer over-
// refunded (caught 2026-05-22).
// Returns the refund id AND Stripe's create-time status (mapped to a Velox
// refund_status). A card refund on a healthy balance returns `succeeded`
// synchronously; ACH/balance-constrained refunds legitimately return `pending`,
// whose terminal outcome (succeeded/failed) lands later via a refund webhook —
// so the caller must record what Stripe actually said, not a blanket success.
func (r *StripeRefunder) CreateRefund(ctx context.Context, paymentIntentID string, amountCents int64, idempotencyKey string) (string, domain.RefundStatus, error) {
	// ForCtx resolves credentials from the TENANT bound on ctx. A background
	// sweep that lists cross-tenant must bind each row's tenant before calling,
	// or every call lands here — and a caller that reads this as "the provider
	// refused" will record a failure for a request never made (found by
	// adversarial review, 2026-07-30). ErrStripeNotConfigured carries
	// NotConfigured=true precisely so that misreading is refusable.
	sc := r.clients.ForCtx(ctx)
	if sc == nil {
		return "", "", ErrStripeNotConfigured
	}
	params := &stripe.RefundCreateParams{
		PaymentIntent: stripe.String(paymentIntentID),
		Amount:        stripe.Int64(amountCents),
	}
	if idempotencyKey != "" {
		params.IdempotencyKey = stripe.String(idempotencyKey)
	}
	ref, err := sc.V1Refunds.Create(ctx, params)
	if err != nil {
		// WRAP, never flatten. This used to return
		// fmt.Errorf("stripe refund: %s", stripeErrorMessage(err)), which
		// destroyed the *PaymentError and with it the one bit that decides what
		// the caller may honestly record: whether the outcome is AMBIGUOUS (the
		// refund may already have happened) or a DEFINITE rejection. Every
		// caller therefore treated a timeout like a decline and wrote
		// refund_status='failed' — telling an operator the customer was not
		// refunded when the money may have left the account, and inviting a
		// manual second refund.
		return "", "", fmt.Errorf("stripe refund: %w", classifyStripeError(err))
	}

	return ref.ID, mapStripeRefundStatus(string(ref.Status)), nil
}
