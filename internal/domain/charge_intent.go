package domain

import "time"

// ChargeIntentState is the lifecycle of a recorded charge attempt.
type ChargeIntentState string

const (
	// ChargeIntentOpen means a request may be in flight at Stripe. The invoice
	// is quarantined from further charges until this resolves — that quarantine
	// is the double-charge guard, not an optimisation.
	ChargeIntentOpen ChargeIntentState = "open"
	// ChargeIntentResolved means the PaymentIntent is named, or provably was
	// never created (the circuit breaker short-circuited before the call).
	ChargeIntentResolved ChargeIntentState = "resolved"
	// ChargeIntentNeedsReview means automatic recovery gave up. The row keeps
	// blocking on purpose: "we do not know whether a PaymentIntent is live" is
	// the last state in which opening another charge would be safe.
	ChargeIntentNeedsReview ChargeIntentState = "needs_review"
)

// ChargeIntent is the durable name of a charge attempt, written BEFORE Stripe
// is called (ADR-106). It exists so recovery never depends on the response:
// every field needed to re-issue the identical request is stored, so an attempt
// whose outcome was lost can be replayed under its original idempotency key —
// which returns the original PaymentIntent instead of creating a second one.
//
// This is not a billing fact and does not render. invoice_charge_attempts
// (ADR-102) owns the fact; this owns the operational lifecycle.
type ChargeIntent struct {
	ID        string `json:"id"`
	TenantID  string `json:"tenant_id"`
	Livemode  bool   `json:"livemode"`
	InvoiceID string `json:"invoice_id"`

	// IdempotencyKey is the EXACT header sent to Stripe, not a seed of it.
	// A key we cannot reproduce is a PaymentIntent we cannot find.
	IdempotencyKey        string            `json:"idempotency_key"`
	AmountCents           int64             `json:"amount_cents"`
	Currency              string            `json:"currency"`
	StripeCustomerID      string            `json:"stripe_customer_id"`
	StripePaymentMethodID string            `json:"stripe_payment_method_id"`
	Description           string            `json:"description"`
	Metadata              map[string]string `json:"metadata,omitempty"`
	Purpose               string            `json:"purpose,omitempty"`

	State                 ChargeIntentState `json:"state"`
	StripePaymentIntentID string            `json:"stripe_payment_intent_id,omitempty"`
	RecoveryAttempts      int               `json:"recovery_attempts"`
	LastError             string            `json:"last_error,omitempty"`

	OccurredAt time.Time `json:"occurred_at"`
	// SimEffectiveAt is the billing-axis instant of an attempt made under a
	// test-clock Advance, so a wall-plane recovery still stamps the contracted
	// instant (ADR-030). Nil for wall-clock attempts.
	SimEffectiveAt *time.Time `json:"sim_effective_at,omitempty"`
	ResolvedAt     *time.Time `json:"resolved_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// Unresolved reports whether this intent still blocks new charges on its
// invoice. Both open and needs_review block: the guard's predicate is
// "not resolved", never "open", because needs_review is exactly the case where
// the outcome is unknown.
func (c ChargeIntent) Unresolved() bool { return c.State != ChargeIntentResolved }
