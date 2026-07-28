package domain

import "time"

// ChargeAttemptOutcome is the lifecycle of one charge attempt. Only
// 'succeeded' is terminal — a 3DS PaymentIntent can go failed →
// succeeded on the customer's second try within one checkout session,
// so every other transition stays open until provider truth settles it.
type ChargeAttemptOutcome string

const (
	ChargeAttemptPending   ChargeAttemptOutcome = "pending"
	ChargeAttemptUnknown   ChargeAttemptOutcome = "unknown"
	ChargeAttemptFailed    ChargeAttemptOutcome = "failed"
	ChargeAttemptSucceeded ChargeAttemptOutcome = "succeeded"
)

// ChargeAttemptTrigger names the writer that made the attempt.
type ChargeAttemptTrigger string

const (
	// ChargeTriggerAutoCharge: the saved-PM chokepoint — cycle close,
	// auto-charge sweep, operator collect.
	ChargeTriggerAutoCharge ChargeAttemptTrigger = "auto_charge"
	// ChargeTriggerDunningRetry: same chokepoint, dunning-retry purpose.
	ChargeTriggerDunningRetry ChargeAttemptTrigger = "dunning_retry"
	// ChargeTriggerExternal: a settle path recorded a PI Velox didn't
	// mint inline (hosted-checkout attempts).
	ChargeTriggerExternal ChargeAttemptTrigger = "external"
)

// InvoiceChargeAttempt is one charge attempt on an invoice — a
// first-class billing fact (ADR-102). Written at PaymentIntent-create
// time and upserted by PI as settle paths learn the outcome, so the
// billing-axis timeline can render every attempt exactly once without
// borrowing the dunning subsystem's events or the webhook stream's
// wall-clock rows.
type InvoiceChargeAttempt struct {
	ID                    string
	TenantID              string
	InvoiceID             string
	StripePaymentIntentID string // empty when the PI create itself failed
	Trigger               ChargeAttemptTrigger
	Outcome               ChargeAttemptOutcome
	ProviderReason        string
	AmountCents           int64
	OccurredAt            time.Time  // wall-clock
	SimEffectiveAt        *time.Time // billing-axis instant under a test clock; nil = wall-clock fact
	Livemode              bool
	CreatedAt             time.Time
	UpdatedAt             time.Time
}
