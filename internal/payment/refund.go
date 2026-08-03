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
// creditNoteID stamps metadata velox_cn_id on the refund. That metadata is
// the ADOPTION key: Stripe v1 idempotency keys expire after ~24h, so the key
// alone cannot make a late retry converge on an earlier attempt — but a
// metadata-stamped refund can always be found again by FindRefundForCreditNote
// and adopted instead of duplicated.
func (r *StripeRefunder) CreateRefund(ctx context.Context, paymentIntentID string, amountCents int64, idempotencyKey, creditNoteID string) (string, domain.RefundStatus, error) {
	sc := r.clients.ForCtx(ctx)
	if sc == nil {
		return "", "", ErrStripeNotConfigured
	}
	params := &stripe.RefundCreateParams{
		PaymentIntent: stripe.String(paymentIntentID),
		Amount:        stripe.Int64(amountCents),
	}
	if creditNoteID != "" {
		params.Metadata = map[string]string{"velox_cn_id": creditNoteID}
	}
	if idempotencyKey != "" {
		params.IdempotencyKey = stripe.String(idempotencyKey)
	}
	ref, err := sc.V1Refunds.Create(ctx, params)
	if err != nil {
		return "", "", fmt.Errorf("stripe refund: %s", stripeErrorMessage(err))
	}

	return ref.ID, mapStripeRefundStatus(string(ref.Status)), nil
}

// GetRefund reads a refund's CURRENT provider state. This is a plain read —
// unlike an idempotency-key replay of the create call, it can never return a
// stale saved response, which is what makes it safe to consult before a
// retry. The second return is Stripe's failure_reason (empty unless failed).
func (r *StripeRefunder) GetRefund(ctx context.Context, refundID string) (domain.RefundStatus, string, error) {
	sc := r.clients.ForCtx(ctx)
	if sc == nil {
		return "", "", ErrStripeNotConfigured
	}
	ref, err := sc.V1Refunds.Retrieve(ctx, refundID, nil)
	if err != nil {
		return "", "", fmt.Errorf("stripe get refund: %s", stripeErrorMessage(err))
	}
	return mapStripeRefundStatus(string(ref.Status)), string(ref.FailureReason), nil
}

// FindRefundForCreditNote searches the PaymentIntent's refunds for one minted
// for this credit note (metadata velox_cn_id). Returns ("", "", nil) when none
// exists. This is the search half of search-and-adopt (ADR-108's pattern,
// applied to refunds): it recovers the lost-response shapes — a create that
// errored on the wire after Stripe minted the refund, in either the original
// Issue leg (Velox holds no id) or a later re-drive (Velox still holds the
// dead predecessor's id, passed as excludeRefundID).
//
// Preference among matches: a LIVE refund (succeeded/pending) wins over a
// failed one — adopting a live twin is what prevents a duplicate; adopting a
// failed twin merely records the truer id, so it is the fallback.
func (r *StripeRefunder) FindRefundForCreditNote(ctx context.Context, paymentIntentID, creditNoteID, excludeRefundID string) (string, domain.RefundStatus, error) {
	sc := r.clients.ForCtx(ctx)
	if sc == nil {
		return "", "", ErrStripeNotConfigured
	}
	params := &stripe.RefundListParams{PaymentIntent: stripe.String(paymentIntentID)}
	var failedID string
	var failedStatus domain.RefundStatus
	for ref, err := range sc.V1Refunds.List(ctx, params) {
		if err != nil {
			return "", "", fmt.Errorf("stripe list refunds: %s", stripeErrorMessage(err))
		}
		if ref.ID == excludeRefundID || ref.Metadata["velox_cn_id"] != creditNoteID {
			continue
		}
		status := mapStripeRefundStatus(string(ref.Status))
		if status != domain.RefundFailed {
			return ref.ID, status, nil
		}
		if failedID == "" {
			failedID, failedStatus = ref.ID, status
		}
	}
	return failedID, failedStatus, nil
}
