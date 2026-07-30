package payment

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/stripe/stripe-go/v82"

	"github.com/sagarsuperuser/velox/internal/errs"
)

// stripeErrorMessage extracts a clean human-readable message from Stripe SDK
// errors. All output is run through errs.Scrub so card last4, raw PANs, and
// emails can't leak into the invoice.last_payment_error column or slog
// fields downstream. Scrubbing at this single ingress point means every
// caller (persistence, logging, HTTP responses) is automatically safe.
func stripeErrorMessage(err error) string {
	var stripeErr *stripe.Error
	if errors.As(err, &stripeErr) {
		if stripeErr.Msg != "" {
			return errs.Scrub(stripeErr.Msg)
		}
		return string(stripeErr.Code)
	}
	return errs.Scrub(err.Error())
}

// PaymentError categorises a failed PaymentIntent attempt. Unknown=true means
// the request's outcome could not be determined from the response (5xx, network
// drop, timeout) — callers must NOT treat this as a decline, because Stripe
// may have processed the charge server-side. A reconciler resolves these by
// querying Stripe after a cool-off window.
type PaymentError struct {
	Message         string
	DeclineCode     string // non-empty for card declines (card_declined, insufficient_funds, etc.)
	PaymentIntentID string // set when Stripe returned a PI object alongside the error
	Unknown         bool
}

func (e *PaymentError) Error() string { return e.Message }

// OperatorSafeMessage implements respond.SafeMessageError so the
// boundary sanitizer (respond.FromError) surfaces a curated message
// instead of falling through to "Internal error". For card declines
// we look up the typed decline_code in a curated map sourced from
// Stripe's docs (decline_codes.go) — gives operators readable
// English ("The card has been reported lost…") rather than the
// awkward title-cased SDK code ("Lost card."). Unknown codes fall
// back to title-case so new Stripe codes don't break the path.
// For non-decline errors we return a category-only message so raw
// Stripe SDK strings (idempotency conflicts, validation errors)
// never reach the operator UI verbatim. ADR-026.
func (e *PaymentError) OperatorSafeMessage() string {
	if e.DeclineCode != "" {
		return "Card was declined: " + declineCodeToOperatorMessage(string(e.DeclineCode))
	}
	if e.Unknown {
		return "Payment outcome could not be determined. Velox will reconcile shortly; refresh in a few seconds."
	}
	return "Payment provider rejected the request. Please retry; if the problem persists, contact support."
}

// humanizeDeclineCode converts Stripe's snake_case decline codes
// ("card_declined", "insufficient_funds") into title-case English.
// Used as a fallback by declineCodeToOperatorMessage when a code
// isn't in the curated map.
func humanizeDeclineCode(code string) string {
	if code == "" {
		return ""
	}
	parts := strings.Split(code, "_")
	if len(parts) == 0 {
		return code
	}
	parts[0] = strings.ToUpper(parts[0][:1]) + parts[0][1:]
	return strings.Join(parts, " ")
}

// ErrPaymentTransient signals that the payment attempt was short-circuited
// upstream of Stripe (circuit breaker open, timeout fired before the call)
// — the Stripe API was not invoked, so the invoice state is unchanged. The
// scheduler/dunning path should treat this as "skip this tick" and not
// tick the dunning attempt count, unlike a real payment failure.
var ErrPaymentTransient = errors.New("payment attempt skipped; upstream breaker open or timeout before Stripe")

// isIdempotencyConflict reports whether err is Stripe refusing a key that was
// already used with different parameters. Distinguished from the other
// ambiguous outcomes because it carries a stronger fact: an attempt under this
// exact key definitely reached Stripe. Callers use it to name the key in an
// operator-facing ERROR, since the dashboard is searchable by it.
func isIdempotencyConflict(err error) bool {
	var stripeErr *stripe.Error
	return errors.As(err, &stripeErr) && stripeErr.Type == stripe.ErrorTypeIdempotency
}

// classifyStripeError maps a stripe-go SDK error to a PaymentError. Non-Stripe
// errors (context cancel, DNS failure wrapped by our code, etc.) are treated
// as unknown, because we cannot prove the request never reached Stripe.
func classifyStripeError(err error) *PaymentError {
	if err == nil {
		return nil
	}
	var stripeErr *stripe.Error
	if !errors.As(err, &stripeErr) {
		return &PaymentError{Message: errs.Scrub(err.Error()), Unknown: true}
	}

	pe := &PaymentError{
		Message:     stripeErrorMessage(err),
		DeclineCode: string(stripeErr.DeclineCode),
	}
	if stripeErr.PaymentIntent != nil {
		pe.PaymentIntentID = stripeErr.PaymentIntent.ID
	}

	switch stripeErr.Type {
	case stripe.ErrorTypeCard,
		stripe.ErrorTypeInvalidRequest:
		// Stripe explicitly rejected the request; no charge occurred.
		pe.Unknown = false
	case stripe.ErrorTypeIdempotency:
		// AMBIGUOUS, not a rejection — and it used to be grouped with the two
		// above under "no charge occurred", which is backwards. Stripe raises
		// this when the key was ALREADY USED with different parameters, so the
		// one thing it proves is that an attempt with this key reached Stripe;
		// a PaymentIntent may well be live and charging right now.
		//
		// Classifying it as a definite failure stamped payment_status='failed',
		// started dunning inline, told the customer their card had failed, and
		// freed the next retry to mint a SECOND PaymentIntent alongside the
		// first. Unknown routes it to the reconciler instead, which is the
		// component whose entire job is "an attempt may exist at Stripe — go
		// find out". Found 2026-07-30 auditing the idempotency-key seed (#678).
		//
		// The key is logged at the call site precisely for this case: Stripe's
		// dashboard is searchable by idempotency key, so an operator can find
		// the live PI even when the error carries no PI id.
		pe.Unknown = true
	case stripe.ErrorTypeAPI:
		// 5xx from Stripe — request may or may not have been processed.
		pe.Unknown = true
	default:
		// Unknown error type — fail safe, treat as ambiguous.
		pe.Unknown = true
	}
	return pe
}

// ErrStripeNotConfigured is returned when a request routes to a Stripe mode
// (live or test) for which the tenant has not connected credentials. Surfaces
// as an explicit PaymentError rather than a nil deref so operators get an
// actionable signal ("connect Stripe under Settings → Payments").
var ErrStripeNotConfigured = &PaymentError{Message: "stripe not configured for this mode"}

// LiveStripeClient wraps the Stripe SDK for PaymentIntent operations. Despite
// the "Live" in the name, it handles both live and test modes — it selects
// the underlying per-mode client from clients.ForCtx(ctx) on each call.
// Implements the StripeClient interface used by the payment adapter.
type LiveStripeClient struct {
	clients *StripeClients
}

// NewLiveStripeClient creates a client from the mode-aware StripeClients
// bundle. Returns nil if clients is nil or has no configured modes.
func NewLiveStripeClient(clients *StripeClients) *LiveStripeClient {
	if !clients.Has() {
		return nil
	}
	return &LiveStripeClient{clients: clients}
}

func (c *LiveStripeClient) CreatePaymentIntent(ctx context.Context, params PaymentIntentParams) (PaymentIntentResult, error) {
	sc := c.clients.ForCtx(ctx)
	if sc == nil {
		return PaymentIntentResult{}, ErrStripeNotConfigured
	}

	metadata := make(map[string]string)
	for k, v := range params.Metadata {
		metadata[k] = v
	}

	// Charge the exact PaymentMethod the caller resolved from Velox's
	// payment_methods table (the single source of truth for the customer's
	// default card). We deliberately do NOT consult Stripe's
	// invoice_settings.default_payment_method or fall back to "most recent
	// card" — those are independent selectors that can disagree with
	// Velox's default and previously let an off-session charge hit a
	// different card than the one Velox gated on. No PM id → loud failure,
	// never a silent guess.
	if params.PaymentMethodID == "" {
		return PaymentIntentResult{}, &PaymentError{Message: "customer has no payment method on file"}
	}

	pi, err := sc.V1PaymentIntents.Create(ctx, &stripe.PaymentIntentCreateParams{
		Amount:        stripe.Int64(params.AmountCents),
		Currency:      stripe.String(params.Currency),
		Customer:      stripe.String(params.CustomerID),
		PaymentMethod: stripe.String(params.PaymentMethodID),
		Confirm:       stripe.Bool(true),
		OffSession:    stripe.Bool(true),
		Params: stripe.Params{
			// The key arrives fully composed — see ChargeIdempotencyKey. It used
			// to gain a "_<PaymentMethodID>" suffix HERE, which made the
			// exported key function's return value differ from the bytes on the
			// wire; recovery cannot replay a request whose key it cannot
			// reproduce (ADR-106).
			IdempotencyKey: stripe.String(params.IdempotencyKey),
			Metadata:       metadata,
		},
		Description: stripe.String(params.Description),
	})
	if err != nil {
		return PaymentIntentResult{}, classifyStripeError(err)
	}

	return PaymentIntentResult{
		ID:           pi.ID,
		Status:       string(pi.Status),
		ClientSecret: pi.ClientSecret,
	}, nil
}

func (c *LiveStripeClient) FetchCardDetails(ctx context.Context, stripeCustomerID string) (CardDetails, error) {
	sc := c.clients.ForCtx(ctx)
	if sc == nil {
		return CardDetails{}, ErrStripeNotConfigured
	}

	// Get the most recently created card
	var latest *stripe.PaymentMethod
	for pm, err := range sc.V1PaymentMethods.List(ctx, &stripe.PaymentMethodListParams{
		Customer: stripe.String(stripeCustomerID),
		Type:     stripe.String("card"),
	}) {
		if err != nil {
			break
		}
		if latest == nil || pm.Created > latest.Created {
			latest = pm
		}
	}
	if latest == nil || latest.Card == nil {
		return CardDetails{}, fmt.Errorf("no card found for customer %s", stripeCustomerID)
	}

	// Set this card as the customer's default payment method
	_, _ = sc.V1Customers.Update(ctx, stripeCustomerID, &stripe.CustomerUpdateParams{
		InvoiceSettings: &stripe.CustomerUpdateInvoiceSettingsParams{
			DefaultPaymentMethod: stripe.String(latest.ID),
		},
	})

	return CardDetails{
		PaymentMethodID: latest.ID,
		Brand:           string(latest.Card.Brand),
		Last4:           latest.Card.Last4,
		ExpMonth:        int(latest.Card.ExpMonth),
		ExpYear:         int(latest.Card.ExpYear),
	}, nil
}

// FetchCardForPaymentIntent retrieves the card actually charged on
// a specific PaymentIntent. Unlike FetchCardDetails (which returns
// the customer's default PM), this works for one-off Checkout
// payments where the customer didn't save the PM — Stripe still
// creates an ephemeral PaymentMethod object for the charge that
// stays accessible through the PI's `payment_method` field. ADR-020.
func (c *LiveStripeClient) FetchCardForPaymentIntent(ctx context.Context, paymentIntentID string) (CardDetails, error) {
	sc := c.clients.ForCtx(ctx)
	if sc == nil {
		return CardDetails{}, ErrStripeNotConfigured
	}
	pi, err := sc.V1PaymentIntents.Retrieve(ctx, paymentIntentID, &stripe.PaymentIntentRetrieveParams{
		Expand: []*string{stripe.String("payment_method")},
	})
	if err != nil {
		return CardDetails{}, classifyStripeError(err)
	}
	if pi.PaymentMethod == nil || pi.PaymentMethod.Card == nil {
		// Non-card payment method (bank, wallet) or expansion
		// returned the bare ID — we don't render anything.
		return CardDetails{}, nil
	}
	// Prefer DisplayBrand: it accounts for dual-branded cards where
	// the customer chose which network to use (e.g. Cartes
	// Bancaires vs Visa on a French co-branded card). Fall back to
	// Brand when DisplayBrand isn't populated by older Stripe API
	// versions. Both fields are Stripe's lowercase enum form;
	// invoice.brandDisplayName handles the title-cased presentation.
	brand := pi.PaymentMethod.Card.DisplayBrand
	if brand == "" {
		brand = string(pi.PaymentMethod.Card.Brand)
	}
	return CardDetails{
		PaymentMethodID: pi.PaymentMethod.ID,
		Brand:           brand,
		Last4:           pi.PaymentMethod.Card.Last4,
		ExpMonth:        int(pi.PaymentMethod.Card.ExpMonth),
		ExpYear:         int(pi.PaymentMethod.Card.ExpYear),
	}, nil
}

func (c *LiveStripeClient) CancelPaymentIntent(ctx context.Context, paymentIntentID string) error {
	sc := c.clients.ForCtx(ctx)
	if sc == nil {
		return ErrStripeNotConfigured
	}
	_, err := sc.V1PaymentIntents.Cancel(ctx, paymentIntentID, nil)
	if err != nil {
		return fmt.Errorf("stripe cancel: %s", stripeErrorMessage(err))
	}
	return nil
}

// GetPaymentIntent fetches the current state of a PaymentIntent. Used by the
// reconciler to resolve PaymentUnknown invoices — Stripe is the source of
// truth for whether a charge actually succeeded.
func (c *LiveStripeClient) GetPaymentIntent(ctx context.Context, paymentIntentID string) (PaymentIntentResult, error) {
	sc := c.clients.ForCtx(ctx)
	if sc == nil {
		return PaymentIntentResult{}, ErrStripeNotConfigured
	}
	pi, err := sc.V1PaymentIntents.Retrieve(ctx, paymentIntentID, nil)
	if err != nil {
		return PaymentIntentResult{}, classifyStripeError(err)
	}
	return PaymentIntentResult{
		ID:                  pi.ID,
		Status:              string(pi.Status),
		ClientSecret:        pi.ClientSecret,
		Purpose:             pi.Metadata["velox_purpose"],
		AmountReceivedCents: pi.AmountReceived,
	}, nil
}
