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

// PaymentFailureReport is one report that a PaymentIntent failed, from any
// of the three sources that learn it: the charge call's own response, the
// payment_intent.payment_failed webhook, or the reconciler.
//
// The invoice records only one attempt at a time — the one it is waiting on
// (its stripe_payment_intent_id and payment_status). A report changes those
// fields only when it is about that attempt. Every report still lands on its
// own attempt row, so a failure that does not move the invoice stays visible.
type PaymentFailureReport struct {
	PaymentIntentID string
	Message         string
	// ChargedAtSeq is set only by the charge call reporting its own attempt:
	// the charge_attempt_seq it charged with. It is that attempt's identity
	// before the invoice has seen its PaymentIntent, so the report is
	// recorded only if no other outcome was recorded during the call (P13).
	// Webhook and reconciler reports leave it nil.
	ChargedAtSeq *int64
	// External marks a PaymentIntent created outside Velox's charge paths
	// (hosted Checkout). Velox never waits on one, so its failure never moves
	// the invoice: it is recorded on its attempt row only.
	External bool
}

// PaymentFailureResult says what a PaymentFailureReport did to the invoice.
type PaymentFailureResult struct {
	// Recorded: the invoice records this PaymentIntent's failure as its
	// current attempt, written by this report or by an earlier one. False
	// when the report was about another attempt, or the invoice is paid.
	Recorded bool
	// FirstNotice: this report fired the failure notification for this
	// PaymentIntent (payment.failed and the customer email). At most one
	// report per PaymentIntent does.
	FirstNotice bool
}

// AttemptStanding is where a reported PaymentIntent sits in the invoice's
// own attempt history (invoice_charge_attempts), among the attempts Velox
// made itself (auto_charge, dunning_retry). Velox writes that row when its
// charge call returns, before it reports the outcome, and its charges are
// serialized by the charge lease, so the row order is the attempt order.
type AttemptStanding int

const (
	// AttemptNotVelox: no Velox-made attempt row names this PaymentIntent —
	// a hosted Checkout attempt, an attempt whose id Velox never learned (a
	// parked one), or one whose row write was lost.
	AttemptNotVelox AttemptStanding = iota
	// AttemptNewest: the newest Velox-made attempt on the invoice.
	AttemptNewest
	// AttemptSuperseded: Velox has made a newer attempt since.
	AttemptSuperseded
)

// MovesInvoice reports whether this report records its failure on an invoice
// in the given state — its status, payment status, the PaymentIntent it
// holds, its charge_attempt_seq, and where the reported PaymentIntent stands
// in its attempt history. The store applies it under the row lock; it lives
// here so every implementation applies the same rule.
//
//   - a paid invoice → never;
//   - the invoice already holds this PaymentIntent → yes, whoever created
//     it (a parked invoice can adopt a hosted attempt, ADR-108);
//   - an External attempt otherwise → never: Velox does not wait on it;
//   - the charge call reporting its own attempt → only if the seq is still
//     the one it charged with (nothing recorded during the call, P13);
//   - a webhook or reconciler report about a different PaymentIntent:
//     never while the invoice waits on an in-flight attempt (no Velox charge
//     starts from 'processing' or 'unknown'); yes if it is Velox's newest
//     attempt (its charge call's own report was lost); no if Velox has made a
//     newer one (a late decline for an older attempt); and with no Velox row
//     to judge by, only if the invoice holds no PaymentIntent yet (the first
//     attempt, or a parked attempt whose id Velox never learned).
func (r PaymentFailureReport) MovesInvoice(status InvoiceStatus, paymentStatus InvoicePaymentStatus, holdsPI string, seq int64, standing AttemptStanding) bool {
	switch {
	case status == InvoicePaid || paymentStatus == PaymentSucceeded:
		return false
	case holdsPI != "" && holdsPI == r.PaymentIntentID:
		return true
	case r.External:
		return false
	case r.ChargedAtSeq != nil:
		return seq == *r.ChargedAtSeq
	case holdsPI != "" && (paymentStatus == PaymentProcessing || paymentStatus == PaymentUnknown):
		return false
	case standing == AttemptNewest:
		return true
	case standing == AttemptSuperseded:
		return false
	default:
		return holdsPI == ""
	}
}
