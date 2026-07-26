package domain

import (
	"fmt"
	"time"
)

// AttentionSeverity is the urgency of an Attention surface. Operators
// sort/filter on this and the dashboard renders one colour per level.
//
// Priority (descending): Critical > Warning > Info.
//
// Severity is implicit elsewhere in the industry (Stripe/Recurly convey
// it via lifecycle state); shipping it explicit is a small "industry-
// grade plus" that drives chip colour and queue ordering without
// breaking precedent.
type AttentionSeverity string

const (
	AttentionSeverityInfo     AttentionSeverity = "info"
	AttentionSeverityWarning  AttentionSeverity = "warning"
	AttentionSeverityCritical AttentionSeverity = "critical"
)

// AttentionReason is the closed, typed code naming *why* an invoice
// needs operator attention. Public-API contract — once shipped, codes
// are never repurposed; deprecations keep the code reserved and add a
// successor.
//
// Closed enum so the dashboard can switch on it for icon, copy, and
// CTA layout. The open variant — Attention.Code — carries the
// underlying provider-specific reason (e.g. tax.customer_data_invalid)
// for programmatic clients.
//
// Anchored to industry precedent: Stripe's invoice event-name
// granularity (invoice.payment_failed, invoice.finalization_failed),
// Chargebee's dunning_status, Lago's payment_status. Five codes today
// matching the signal we actually have; the enum is open for
// extension as we ship dunning, disputes, and 3DS support.
//
// Reserved-but-not-yet-emitted (planned, do not reuse):
//
//	finalization_failed         — non-tax finalization error
//	payment_action_required     — 3DS / SCA pending
//	payment_method_required     — no PM on file
//	dunning_exhausted           — shipped 2026-07-22 (payment-surfacing audit)
//	dispute_lost                — Lago parity
type AttentionReason string

const (
	// AttentionReasonTaxCalculationFailed: Stripe Tax (or another
	// provider) couldn't compute tax for this invoice. Underlying cause
	// (jurisdiction unsupported, provider outage, auth, unknown) lives
	// in Attention.Code. Severity = Warning while retries are pending,
	// Critical once exhausted.
	AttentionReasonTaxCalculationFailed AttentionReason = "tax_calculation_failed"

	// AttentionReasonTaxLocationRequired: customer's billing profile is
	// missing or malformed in a way the tax provider needs (postal_code
	// for US, country, tax_id). Distinguished from the generic
	// tax_calculation_failed because the operator action is concrete:
	// edit the customer's address. Mirrors Stripe's
	// `customer_tax_location_invalid` / `requires_location_inputs`.
	AttentionReasonTaxLocationRequired AttentionReason = "tax_location_required"

	// AttentionReasonPaymentFailed: Stripe (or another payment provider)
	// declined the charge. Underlying decline_code in Attention.Code,
	// human message in Attention.Message.
	AttentionReasonPaymentFailed AttentionReason = "payment_failed"

	// AttentionReasonPaymentUnconfirmed: charge attempt returned an
	// ambiguous error (5xx, timeout) — the reconciler resolves it once
	// Stripe reports a terminal outcome (in-flight PIs are skipped).
	// Velox-specific (no direct industry parity); shipped because the
	// PaymentIntent + reconciler architecture creates this state and
	// operators benefit from a quiet "the system has it" signal rather
	// than silence.
	AttentionReasonPaymentUnconfirmed AttentionReason = "payment_unconfirmed"

	// AttentionReasonPaymentAnomaly: the settle path detected money that
	// does not reconcile — a second different PaymentIntent succeeded on an
	// already-paid invoice (double charge), the captured amount differs
	// from the booked amount, or a payment landed on a voided invoice
	// (ADR-068). Critical, and deliberately surfaced EVEN ON TERMINAL
	// invoices: with auto-refund deferred the operator is the refund
	// mechanism, and the anomaly usually fires on an invoice that is
	// already paid. Attention.Code carries the anomaly kind; Message names
	// both PaymentIntents and the captured amount.
	AttentionReasonPaymentAnomaly AttentionReason = "payment_anomaly"

	// AttentionReasonPaymentProcessing: charge attempt is in flight at
	// the provider — Stripe has accepted the PaymentIntent but hasn't
	// returned a final outcome. Self-resolves; no operator action.
	// Stripe parity: `processing` status on the payment_intent.
	AttentionReasonPaymentProcessing AttentionReason = "payment_processing"

	// AttentionReasonPaymentScheduled: invoice is finalized and unpaid
	// with auto_charge_pending=true — the scheduler will fire the auto-
	// charge on its next tick. Operators can short-circuit with
	// "Charge now". Mirrors Stripe's "Awaiting payment" with auto-retry
	// scheduled state.
	AttentionReasonPaymentScheduled AttentionReason = "payment_scheduled"

	// AttentionReasonAwaitingPayment: invoice is finalized and unpaid,
	// no charge attempt has fired yet (no PaymentIntent or
	// auto_charge_pending=false). Customer-pay collection mode, or a
	// pre-first-charge window. Operator can trigger a charge or send a
	// reminder. Stripe parity: `open` invoice with no payment activity.
	AttentionReasonAwaitingPayment AttentionReason = "awaiting_payment"

	// AttentionReasonNoPaymentMethod: invoice is finalized and unpaid,
	// and the customer has no PaymentSetup ready. The invoice IS queued
	// (auto_charge_pending) — the sweep re-resolves the payment setup
	// every tick and collects the moment a card attaches (charge-on-
	// attach), and dunning enrollment ages it out. What automation
	// CANNOT do is conjure the card: a card-less retry counts an
	// attempt and reminds, it never collects — so the operator (or the
	// customer, via the emailed setup link) is still the mover that
	// unblocks it. Surfaces distinct from awaiting_payment because the
	// action is concrete: "Add payment method", not "Wait and see".
	// Mirrors Stripe's `customer.invoice.requires_payment_method` flow.
	// (Pre-2026-07-22 this comment claimed "NO retry, NO sweep" — born
	// false: the sweep + dunning enrollment already existed.)
	AttentionReasonNoPaymentMethod AttentionReason = "no_payment_method"

	// AttentionReasonDunningExhausted: invoice is finalized and unpaid,
	// the customer STILL has no payment method, and its dunning run has
	// escalated — automatic recovery ended without collecting, and the
	// policy's final action fired (default: collection paused on the
	// subscription). Distinct from no_payment_method because the
	// reassuring "recovery is running" framing is no longer true: the
	// operator must know the engine has escalated. Charge-on-attach
	// still works (the pending-charge sweep has no pause filter), so
	// attaching a card still collects this invoice. Chargebee/Stripe
	// parity: the end-of-dunning terminal is an explicit, visible state.
	AttentionReasonDunningExhausted AttentionReason = "dunning_exhausted"
)

// AttentionAction names the operator's recommended next step. Closed
// enum because every code maps to a concrete server endpoint or a
// concrete frontend route, and audit logs key off the code. Mirrors
// the Stripe / Recurly / Chargebee verb cluster (Retry / Edit / Mark
// Paid / Void / Stop Dunning).
type AttentionAction string

const (
	AttentionActionEditBillingProfile AttentionAction = "edit_billing_profile"
	AttentionActionRetryTax           AttentionAction = "retry_tax"
	AttentionActionRetryPayment       AttentionAction = "retry_payment"
	AttentionActionWaitProvider       AttentionAction = "wait_provider"
	AttentionActionRotateAPIKey       AttentionAction = "rotate_api_key"
	AttentionActionReconcilePayment   AttentionAction = "reconcile_payment"
	AttentionActionReviewRegistration AttentionAction = "review_registration"
	// AttentionActionChargeNow triggers an immediate auto-charge (calls
	// the existing collect-payment endpoint). Operators use this to
	// short-circuit the scheduler when they want to bill now.
	AttentionActionChargeNow AttentionAction = "charge_now"
	// AttentionActionSendReminder dispatches an invoice email to the
	// customer (calls the existing send-invoice-email endpoint). Used
	// in customer-pay collection mode when the invoice is sitting
	// unpaid and a nudge is appropriate.
	AttentionActionSendReminder AttentionAction = "send_reminder"
	// AttentionActionAddPaymentMethod deep-links the dashboard to the
	// customer's billing surface to attach a PM. Surfaced when an
	// invoice is awaiting payment AND the customer has no PaymentSetup
	// ready — the only state where adding a PM unblocks collection.
	AttentionActionAddPaymentMethod AttentionAction = "add_payment_method"
	// AttentionActionUpdatePaymentMethod opens a Stripe-hosted setup
	// flow (Checkout in setup mode) so the customer can replace the
	// card that just declined. Distinct from AddPaymentMethod (which
	// deep-links the dashboard for the no-PM case): there's already a
	// PM on file but it's broken — the verb is "replace this card",
	// not "set up your first card". Surfaced on payment.declined.
	// Operator clicking it opens the send-setup-link dialog
	// (email the customer / copy the link) backed by the operator
	// setup-session endpoint — the customer completes the Stripe-
	// hosted setup themselves; nothing opens in the operator's tab.
	AttentionActionUpdatePaymentMethod AttentionAction = "update_payment_method"
	// AttentionActionConnectTaxProvider deep-links the dashboard to
	// Settings → Payments so the operator can connect the tax
	// provider's credentials. Distinct from RotateAPIKey: nothing
	// to rotate, the connection has never been made.
	AttentionActionConnectTaxProvider AttentionAction = "connect_tax_provider"
)

// AttentionActionItem is one entry in Attention.Actions. Frontend
// renders one button per item; first item is the primary CTA, the
// rest are secondary. Label is a default rendering — UIs are free to
// substitute localized copy.
type AttentionActionItem struct {
	Code  AttentionAction `json:"code"`
	Label string          `json:"label,omitempty"`
}

// Attention is the unified "this invoice needs operator attention"
// surface. Computed on read by ClassifyInvoiceAttention; never
// persisted. Durable fields (tax_status, tax_error_code,
// payment_status, last_payment_error) are the source
// of truth.
//
// Wire shape mirrors Stripe's last_finalization_error /
// last_payment_error envelope (`code`, `message`, `doc_url`, `param`)
// for parity with what design partners already integrate against, plus
// three Velox additions:
//
//   - reason   — closed enum the dashboard switches on
//   - severity — closed three-value enum for sort/filter
//   - actions  — prescribed verbs so third-party UIs and audit logs
//     have a machine-readable affordance for "what to do next"
//
// On the wire, the field is omitted entirely when the invoice is
// healthy (matches Stripe's `last_finalization_error: null` ergonomic
// — clients render `if (invoice.attention)` without nested checks).
type Attention struct {
	Severity AttentionSeverity     `json:"severity"`
	Reason   AttentionReason       `json:"reason"`
	Message  string                `json:"message"`
	Actions  []AttentionActionItem `json:"actions,omitempty"`

	// Code is the open, dotted, provider-specific code for programmatic
	// clients. New codes ship without a major-version bump. Examples:
	// tax.customer_data_invalid, tax.provider_outage, payment.declined.
	// Stripe parity (their `code` field is also open).
	Code string `json:"code,omitempty"`

	// DocURL deep-links to the operator-facing documentation page for
	// this reason. Stripe parity. Empty when no doc page exists yet.
	DocURL string `json:"doc_url,omitempty"`

	// Param points at the entity field the operator must edit to
	// resolve the issue, in dotted-path form (e.g.
	// "customer.address.postal_code"). Stripe parity — lets the
	// dashboard's Edit CTA deep-link to the broken field. Empty when
	// the resolution isn't field-scoped.
	Param string `json:"param,omitempty"`

	// Detail is Velox's own classification context — always operator-
	// safe and always our framing. Populated when there's something
	// useful beyond the headline (e.g., "engine retried 3 times before
	// giving up"). NOT a place for upstream provider payloads — those
	// go in ProviderResponse so the UI can label them honestly.
	// May be empty when the headline + typed code + actions cover
	// everything an operator needs.
	Detail string `json:"detail,omitempty"`

	// ProviderResponse is the raw upstream payload from a third-party
	// service (Stripe Tax JSON envelope, Stripe PaymentIntent
	// last_payment_error body, etc.). Populated ONLY when we actually
	// made an HTTP call and received a response. Pre-flight
	// classification errors (e.g. tax.provider_not_configured —
	// where we never called) leave this empty.
	//
	// The UI labels this as "Provider response" in a collapsible
	// section, so the operator knows the bytes literally came from
	// the upstream service, not from Velox. Stripe's own API
	// distinguishes error.message (their classification) from
	// error.payment_intent.last_payment_error (the upstream
	// provider's response); this field mirrors that contract.
	ProviderResponse string `json:"provider_response,omitempty"`

	// Since is when the attention condition started — operators triage
	// by age. tax_deferred_at for tax reasons; updated_at as a proxy
	// for payment_failed/unconfirmed (we don't track a precise
	// failed_at timestamp).
	Since *time.Time `json:"since,omitempty"`

	// NextAttemptAt is when the engine will retry an automatic action.
	// Populated ONLY when a real scheduled instant exists — today that
	// is the tax reconciler's tax_next_retry_at while automatic tax
	// retries remain. payment_scheduled deliberately does NOT populate
	// it (the sweep is tick-based; "on its next tick" is honest, a
	// fabricated precise instant is not), and dunning surfaces its own
	// next_action_at on the dunning run. Crucially, this is NOT due_at:
	// due_at is a deadline, not when the engine is scheduled to act.
	// Empty when the system has no automatic next action.
	NextAttemptAt *time.Time `json:"next_attempt_at,omitempty"`

	// DueBy is the invoice's payment deadline (mirrors invoice.due_at).
	// Surfaced separately from NextAttemptAt so the operator-facing
	// banner can render "Due by Jun 16" without claiming the engine
	// will act on that date. Empty when the invoice has no due_at set.
	DueBy *time.Time `json:"due_by,omitempty"`
}

// AttentionContext carries ancillary signals needed by the classifier
// that aren't durable on the Invoice row itself. Today this is just
// the customer's payment-method state, but the struct is the seam for
// future signals (dunning policy retry schedule, collection mode, etc).
// Zero-value is safe — classifier falls back to existing behaviour
// when callers don't have the signal available.
type AttentionContext struct {
	// HasPaymentMethod is true when the customer has a PaymentSetup
	// row in `ready` state with a stripe_customer_id. Distinguishes
	// the operator-actionable no_payment_method state from the generic
	// awaiting_payment race window.
	HasPaymentMethod bool

	// CustomerHasEmail is true ONLY when the customer confirmably has an
	// email address on file. Consumed only by the no_payment_method
	// classifier: the engine's finalize-time setup-link email
	// (NotifyNoPaymentMethod) skips when there's no address (a typed,
	// reported skip since 2026-07-10 — but per-invoice send state still
	// isn't observable here), so
	// the banner must not claim "we emailed a link" — and must not offer a
	// resend that cannot send — in that case. Zero-value (false) is the
	// conservative default and also covers "couldn't determine" (read
	// error / no reader): the false-branch copy states engine behavior,
	// not this customer's email state, so it stays honest either way. The
	// invoice service populates it from the customer record.
	CustomerHasEmail bool

	// StripeConnected reports whether the invoice's tenant has Stripe
	// credentials connected for this invoice's mode (livemode). Used
	// by the tax-classification path to distinguish two states that
	// previously rendered identically:
	//   - tax_error_code='provider_not_configured' AND Stripe not
	//     connected → operator action required ("Connect Stripe").
	//   - tax_error_code='provider_not_configured' AND Stripe IS now
	//     connected → calculation will retry on the next scheduler
	//     tick (5 min dev, 1 hr prod). The previous "Stripe isn't
	//     connected" copy was misleading during this gap window.
	// Resolution: the operator just connected Stripe; the invoice's
	// stale tax_error_code hasn't been replaced yet because the
	// scheduler hasn't ticked. Banner now reads "calculation queued"
	// + "Retry now" so the operator gets agency without waiting.
	StripeConnected bool

	// DunningEscalated is true when the invoice's dunning run has reached
	// state='escalated' — automatic recovery exhausted its retries and the
	// policy's final action fired. Consumed only by the no_payment_method
	// classifier branch (the invoice service fetches the run lazily under
	// the same finalized-unpaid-no-PM guard as CustomerHasEmail, so list
	// reads don't pay a dunning lookup per row). Zero-value (false) keeps
	// the pre-escalation banner — safe for callers without the signal.
	DunningEscalated bool

	// Now is wall-clock now, used only to age the in-flight payment banner
	// (processing → Info under the expected-settle window, Warning past it).
	// Staleness is a REAL-WORLD duration (the provider settles in wall-clock),
	// so this is wall-clock, not resolver-bound sim-time. Zero value disables
	// the age escalation (the banner stays Info) — safe for callers that don't
	// set it.
	Now time.Time
}

// processingStaleAfter is how long a payment may sit `processing` before the
// in-flight banner escalates from Info to Warning. Cards confirm in seconds
// (inline, ADR-049 Phase 3) or within the reconciler's window (Phase 2), so a
// card past this is genuinely anomalous — point the operator at Stripe. Tunable;
// will need to go method-specific (cards: hours; ACH/SEPA: days) when an async
// method is enabled (ADR-049 deferred tail).
const processingStaleAfter = 6 * time.Hour

// docBaseURL is the doc site root for operator-facing error pages. Kept
// as a const so the SDK and the dashboard agree without env coupling.
const docBaseURL = "https://docs.velox.dev/errors/"

// ClassifyInvoiceAttention returns the most-pressing reason this
// invoice needs operator attention, or nil if everything is healthy.
//
// Priority order (first match wins, descending urgency):
//  1. Tax failed          — blocks finalize, Critical
//  2. Payment failed      — money flow broken, Critical
//  3. Tax pending         — engine retrying, Warning
//  4. Payment unconfirmed — reconciler resolves, Info
//  5. Payment processing  — charge in flight at provider, Info
//  6. Payment scheduled   — auto-charge will fire on next tick, Info
//  7. Awaiting payment    — finalized, no charge attempted yet, Info
//
// Terminal-state invoices (paid, voided) never raise attention.
// Drafts also skip — the page itself communicates draft state and
// adding a banner would be redundant noise.
//
// Pure function — no I/O, deterministic on the input Invoice +
// AttentionContext. Tested in invoice_attention_test.go across the
// cartesian product of (status, tax_status, tax_error_code,
// payment_status, auto_charge_pending, has_payment_method).
//
// The atc.HasPaymentMethod signal distinguishes two awaiting sub-
// states: a no-PM invoice (operator-actionable, the engine has nothing
// scheduled) vs a has-PM race window (transient, engine will fire on
// next sweep). Without the context, callers fall through to the
// generic awaiting_payment classification — backwards-compat for
// internal call sites that don't have a PaymentMethodReader handy.
func ClassifyInvoiceAttention(inv Invoice, atc AttentionContext) *Attention {
	// Terminal-ish statuses produce no attention surface. Uncollectible
	// is technically forward-transitionable (operator can still record
	// an offline payment or void it) but Stripe-parity treats it as a
	// settled state for dunning/diagnostic purposes — the operator has
	// already acknowledged "we're not collecting this." Surfacing a
	// payment_failed attention banner on top of that contradicts the
	// operator's decision.
	// Payment anomalies pierce the terminal early-return below (ADR-068):
	// the double-charge / amount-mismatch / paid-a-voided-invoice marker
	// fires almost exclusively on invoices that are ALREADY paid or voided —
	// exactly the rows the early-return hides. The operator must see it to
	// refund it.
	if inv.PaymentAnomalyKind != "" {
		return classifyPaymentAnomaly(inv)
	}
	if inv.Status == InvoicePaid || inv.Status == InvoiceVoided || inv.Status == InvoiceUncollectible {
		return nil
	}
	switch {
	case inv.TaxStatus == InvoiceTaxFailed:
		return classifyTaxAttention(inv, atc, AttentionSeverityCritical)
	case inv.PaymentStatus == PaymentFailed:
		return classifyPaymentFailure(inv)
	case inv.TaxStatus == InvoiceTaxPending:
		return classifyTaxAttention(inv, atc, AttentionSeverityWarning)
	case inv.PaymentStatus == PaymentUnknown:
		return classifyPaymentUnconfirmed(inv)
	case inv.PaymentStatus == PaymentProcessing:
		return classifyPaymentProcessing(inv, atc)
	// no_payment_method beats payment_scheduled: when both flags are
	// set (engine queued for retry but PM is still missing), the
	// scheduler will keep skipping until a PM is attached. The
	// operator-facing reality is "no PM" — that's the actionable
	// reason. Surfacing payment_scheduled instead would falsely tell
	// the operator "engine will retry on its next tick" when the
	// retry will skip again until a PM is attached.
	case inv.Status == InvoiceFinalized && inv.PaymentStatus == PaymentPending && !atc.HasPaymentMethod:
		return classifyNoPaymentMethod(inv, atc)
	case inv.Status == InvoiceFinalized && inv.PaymentStatus == PaymentPending && inv.AutoChargePending:
		return classifyPaymentScheduled(inv)
	case inv.Status == InvoiceFinalized && inv.PaymentStatus == PaymentPending:
		return classifyAwaitingPayment(inv)
	}
	return nil
}

// classifyTaxAttention branches on the typed tax_error_code persisted
// by internal/tax.Classify. Sub-cases differ in three dimensions:
// Reason (location-vs-generic), action verbs, and `param` pointer. The
// open `code` carries the full granularity (one per error_code value).
//
// Provenance routing per typed code (ADR-025):
//   - provider_not_configured: pre-flight failure, we never called the
//     provider. tax_pending_reason holds Velox's own string ("no client
//     configured for livemode=…"). Both Detail and ProviderResponse
//     stay empty — the headline + code + Connect Stripe action are the
//     whole UI; surfacing the internal string under "Provider response"
//     would mislead operators into thinking we got a 4xx from Stripe.
//   - provider_auth / provider_outage / customer_data_invalid /
//     jurisdiction_unsupported: we made the call and got a response.
//     tax_pending_reason holds the upstream payload (Stripe Tax JSON
//     envelope, prefixed with "stripe_tax: "). Goes into
//     ProviderResponse.
//   - unknown: routes to ProviderResponse — the safer slot for
//     unclassified content. If it turns out to be Velox-internal, we'd
//     just be mis-labelling it as a provider response, which is the
//     same as today's behaviour for known cases.
//
// MaxTaxRetryAttempts is the reconciler's hard cap on automatic tax
// retries (the SQL predicate lives in billing/tax_retry.go, which
// sources this const). The attention banner asserts this policy to
// operators, so the classifier and the reconciler must share one
// number — a banner promising retries the reconciler stopped making
// was the 2026-07-19 truth-audit finding this closes.
const MaxTaxRetryAttempts = 8

// TaxRetryableErrorCodes is the single source for WHICH tax error
// codes the reconciler retries. provider_not_configured is retryable
// on purpose: it fails PRE-FLIGHT (no provider API call, no quota),
// and retrying self-heals the invoice that raced a Stripe connect —
// the banner already promised this behavior; now it is true. Codes
// outside this list are operator-action-required (auth, bad customer
// data, unregistered jurisdiction): retrying those burns provider
// quota with no chance of success.
func TaxRetryableErrorCodes() []string {
	return []string{"provider_outage", "unknown", "provider_not_configured"}
}

// TaxErrorCodeRetryable reports whether the reconciler retries code.
func TaxErrorCodeRetryable(code string) bool {
	for _, c := range TaxRetryableErrorCodes() {
		if c == code {
			return true
		}
	}
	return false
}

func classifyTaxAttention(inv Invoice, atc AttentionContext, severity AttentionSeverity) *Attention {
	errorCode := inv.TaxErrorCode
	if errorCode == "" {
		errorCode = "unknown"
	}

	att := &Attention{
		Severity: severity,
		Since:    inv.TaxDeferredAt,
		Code:     "tax." + errorCode,
	}
	// Exhaustion escalates. After MaxTaxRetryAttempts the reconciler
	// permanently excludes the invoice (tax_retry.go predicate), so a
	// "retrying automatically" banner would soothe forever over an
	// invoice nothing will touch — the operator is the only mover, and
	// the severity must say so.
	retryable := TaxErrorCodeRetryable(errorCode)
	exhausted := retryable && inv.TaxRetryCount >= MaxTaxRetryAttempts
	if exhausted {
		att.Severity = AttentionSeverityCritical
	}
	// A real scheduled next attempt exists only while retries remain;
	// the reconciler stamps tax_next_retry_at (plumbed fact, not a
	// derived guess).
	if retryable && !exhausted && inv.TaxNextRetryAt != nil {
		att.NextAttemptAt = inv.TaxNextRetryAt
	}
	// provider_not_configured is the only tax code that's pre-flight
	// (no API call). Every other code reflects something the provider
	// actually returned. Route accordingly.
	if errorCode != "provider_not_configured" && inv.TaxPendingReason != "" {
		att.ProviderResponse = inv.TaxPendingReason
	}

	switch errorCode {
	case "customer_data_invalid":
		att.Reason = AttentionReasonTaxLocationRequired
		att.Message = "The customer's billing profile has missing or invalid data the tax provider requires (postal code, country, or tax ID)."
		att.Param = "customer.address.postal_code"
		att.DocURL = docBaseURL + "tax-location-required"
		att.Actions = []AttentionActionItem{
			{Code: AttentionActionEditBillingProfile, Label: "Edit billing profile"},
			{Code: AttentionActionRetryTax, Label: "Retry tax"},
		}
	case "jurisdiction_unsupported":
		att.Reason = AttentionReasonTaxCalculationFailed
		att.Message = "The provider can't compute tax for this customer's region. Register with the provider in that jurisdiction or override tax manually."
		att.DocURL = docBaseURL + "tax-jurisdiction-unsupported"
		att.Actions = []AttentionActionItem{
			{Code: AttentionActionReviewRegistration, Label: "Review tax registration"},
			{Code: AttentionActionRetryTax, Label: "Retry tax"},
		}
	case "provider_outage":
		att.Reason = AttentionReasonTaxCalculationFailed
		if exhausted {
			att.Message = "The tax provider stayed unreachable through all automatic retries. Retry manually once the provider recovers."
		} else {
			att.Message = fmt.Sprintf("The tax provider is temporarily unreachable. The engine retries automatically (%d of %d attempts used).", inv.TaxRetryCount, MaxTaxRetryAttempts)
		}
		att.DocURL = docBaseURL + "tax-provider-outage"
		att.Actions = []AttentionActionItem{
			{Code: AttentionActionWaitProvider, Label: "Check provider status"},
			{Code: AttentionActionRetryTax, Label: "Retry now"},
		}
	case "provider_auth":
		att.Reason = AttentionReasonTaxCalculationFailed
		att.Message = "The configured tax-provider API key is invalid or revoked. Rotate the key in Settings."
		att.DocURL = docBaseURL + "tax-provider-auth"
		att.Actions = []AttentionActionItem{
			{Code: AttentionActionRotateAPIKey, Label: "Rotate API key"},
			{Code: AttentionActionRetryTax, Label: "Retry tax"},
		}
	case "provider_not_configured":
		att.Reason = AttentionReasonTaxCalculationFailed
		att.DocURL = docBaseURL + "tax-provider-not-configured"
		// Banner copy + actions split on whether the tenant has Stripe
		// connected NOW. The invoice's tax_error_code was stamped at
		// the time of the failed calculation; if the operator has
		// since connected Stripe, the only thing the invoice is
		// waiting for is the next scheduler tick — not operator
		// action. Telling them "Stripe isn't connected" in that
		// window is misleading and erodes trust in the diagnostic.
		// ADR-019 already wires a tenant-wide retry on Stripe connect,
		// so the gap is purely about UX during the 5-min/1-hr window
		// before the tick fires.
		if atc.StripeConnected && !exhausted {
			att.Severity = AttentionSeverityInfo
			att.Message = "Tax calculation is queued. The engine retries automatically now that Stripe is connected. Click Retry now to recompute immediately."
			att.Actions = []AttentionActionItem{
				{Code: AttentionActionRetryTax, Label: "Retry now"},
			}
		} else if atc.StripeConnected {
			att.Message = "Automatic tax retries are exhausted for this invoice. Click Retry now to recompute it."
			att.Actions = []AttentionActionItem{
				{Code: AttentionActionRetryTax, Label: "Retry now"},
			}
		} else {
			att.Message = "Stripe Tax is selected in Settings → Tax but Stripe isn't connected for this mode. Connect your Stripe keys for the active mode (test or live) in Settings → Payments."
			att.Actions = []AttentionActionItem{
				{Code: AttentionActionConnectTaxProvider, Label: "Connect Stripe"},
				{Code: AttentionActionRetryTax, Label: "Retry tax"},
			}
		}
	default:
		att.Reason = AttentionReasonTaxCalculationFailed
		if exhausted {
			att.Message = "Tax calculation kept failing for an unrecognised reason and automatic retries are exhausted. See the raw provider response below."
		} else {
			att.Message = "Tax calculation failed for an unrecognised reason. See the raw provider response below."
		}
		att.DocURL = docBaseURL + "tax-calculation-failed"
		att.Actions = []AttentionActionItem{
			{Code: AttentionActionRetryTax, Label: "Retry tax"},
		}
	}
	return att
}

func classifyPaymentFailure(inv Invoice) *Attention {
	headline := truncate(inv.LastPaymentError, 200)
	if headline == "" {
		headline = "Payment attempt failed."
	}
	since := inv.UpdatedAt
	// Update Payment Method is the primary action: the card on file
	// is broken, retrying with the same card will decline again.
	// Retry Payment is offered as secondary for transient declines
	// (network blip, temporary insufficient funds) where the
	// operator wants to re-attempt without changing the card.
	//
	// LastPaymentError is the Stripe last_payment_error message —
	// upstream payload from Stripe, not Velox's classification —
	// so it goes in ProviderResponse (ADR-025). The headline already
	// renders the truncated form; ProviderResponse holds the full
	// string for the disclosure.
	return &Attention{
		Severity: AttentionSeverityCritical,
		Reason:   AttentionReasonPaymentFailed,
		Code:     "payment.declined",
		Message:  headline,
		DocURL:   docBaseURL + "payment-failed",
		Actions: []AttentionActionItem{
			{Code: AttentionActionUpdatePaymentMethod, Label: "Update payment method"},
			{Code: AttentionActionRetryPayment, Label: "Retry payment"},
		},
		ProviderResponse: inv.LastPaymentError,
		Since:            &since,
	}
}

func classifyPaymentUnconfirmed(inv Invoice) *Attention {
	since := inv.UpdatedAt
	// No operator action: the reconciler re-queries the provider on the next
	// tick and now settles the outcome COMPLETELY — mark + dunning + email +
	// event, identical to the webhook (ADR-049 Phase 2). The prior disabled
	// "Check provider" button was non-functional (no endpoint); an on-demand
	// re-check is deferred until real stuck-payment pressure (ADR-049), so the
	// dead button is removed rather than shipped greyed-out.
	return &Attention{
		Severity: AttentionSeverityInfo,
		Reason:   AttentionReasonPaymentUnconfirmed,
		Code:     "payment.unconfirmed",
		// Honest scope (2026-07-06 truth pass): the reconciler resolves
		// TERMINAL Stripe outcomes (succeeded / canceled /
		// requires_payment_method). A PI parked at requires_action —
		// off-session SCA nobody completes — is deliberately SKIPPED every
		// sweep, so those need the operator (cancel or nudge the customer),
		// and this copy must not promise otherwise.
		Message: "Payment outcome unconfirmed by the provider — Velox re-checks automatically and resolves once Stripe reports a final outcome. If it sits here for hours the charge may be waiting on customer action (e.g. 3-D Secure): check the payment in Stripe, then cancel or retry it.",
		DocURL:  docBaseURL + "payment-unconfirmed",
		Since:   &since,
	}
}

// classifyPaymentProcessing surfaces the in-flight charge state. Velox now
// confirms it automatically — inline from the synchronous charge response
// (ADR-049 Phase 3) or via the reconciler backstop if the provider callback is
// delayed/dropped (Phase 2) — so the healthy state needs no operator action.
//
// Age-aware: a REAL (non-simulated) invoice still `processing` past
// processingStaleAfter is anomalous for a card (it should have settled inline
// or via the reconciler), so escalate Info → Warning and point the operator at
// the provider — without falsely promising auto-resolution for the stuck case.
// Simulated invoices never escalate: their "age" is sim-time, not a real-world
// duration, so a wall-clock age check would false-positive (atc.Now is
// wall-clock). Zero atc.Now also keeps it Info (callers that don't set it).
func classifyPaymentProcessing(inv Invoice, atc AttentionContext) *Attention {
	since := inv.UpdatedAt

	stale := !inv.IsSimulated && !atc.Now.IsZero() && atc.Now.Sub(inv.UpdatedAt) > processingStaleAfter
	if stale {
		return &Attention{
			Severity: AttentionSeverityWarning,
			Reason:   AttentionReasonPaymentProcessing,
			Code:     "payment.processing",
			Message:  "Payment has been awaiting confirmation at the provider for an unusually long time — check this payment in Stripe.",
			DocURL:   docBaseURL + "payment-processing",
			Since:    &since,
		}
	}

	return &Attention{
		Severity: AttentionSeverityInfo,
		Reason:   AttentionReasonPaymentProcessing,
		Code:     "payment.processing",
		Message:  "Payment is in flight at the provider — Velox confirms it automatically when the provider responds.",
		DocURL:   docBaseURL + "payment-processing",
		Since:    &since,
	}
}

// classifyPaymentScheduled surfaces the scheduler-will-retry state.
// Auto-charge is queued; the engine will fire on its next sweep. The
// operator can short-circuit with "Charge now" if they don't want to
// wait the scheduler interval.
func classifyPaymentScheduled(inv Invoice) *Attention {
	since := inv.UpdatedAt
	// payment_scheduled fires when auto_charge_pending=true: a charge
	// has been attempted, failed retryably, and the scheduler will
	// pick it up on its next sweep. The sweep cadence is short
	// (seconds-to-minutes) so we don't surface a precise
	// next_attempt_at — "on its next tick" is honest.
	//
	// A clock-pinned (simulated) invoice is the exception: the wall-clock
	// sweep EXCLUDES it (ADR-028/029 disjoint flows — ListAutoChargePending
	// filters out subs with a test_clock_id), so the retry fires only when
	// the operator advances the test clock. Saying "next tick" there is a
	// lie — in real time the invoice sits indefinitely, so adding a payment
	// method and waiting looks broken. Surface the clock-advance reality.
	message := "Auto-charge is scheduled — the engine will attempt the charge on its next tick."
	if inv.IsSimulated {
		message = "Auto-charge runs on the next test-clock advance — advance the clock to trigger it, or use Charge now to collect immediately."
	}
	return &Attention{
		Severity: AttentionSeverityInfo,
		Reason:   AttentionReasonPaymentScheduled,
		Code:     "payment.scheduled",
		Message:  message,
		DocURL:   docBaseURL + "payment-scheduled",
		Actions: []AttentionActionItem{
			{Code: AttentionActionChargeNow, Label: "Charge now"},
		},
		Since: &since,
		DueBy: inv.DueAt,
	}
}

// classifyAwaitingPayment surfaces the steady-state finalized-but-
// unpaid invoice. No charge attempt has fired yet — either customer-
// pay collection mode (operator emails the link, customer self-pays)
// or a pre-first-charge window. Operators get two paths: trigger the
// charge themselves, or send the customer a reminder email.
func classifyAwaitingPayment(inv Invoice) *Attention {
	since := inv.UpdatedAt
	// Has-PM race window: PaymentSetup is ready but the engine hasn't
	// run yet (sub-second to engine-tick window) OR the engine ran but
	// charge_immediately_at_finalize was disabled. Operator-actionable
	// only if they don't want to wait — Charge now is the override.
	return &Attention{
		Severity: AttentionSeverityInfo,
		Reason:   AttentionReasonAwaitingPayment,
		Code:     "payment.awaiting",
		Message:  "Invoice is finalized and awaiting first charge attempt.",
		DocURL:   docBaseURL + "awaiting-payment",
		Actions: []AttentionActionItem{
			{Code: AttentionActionChargeNow, Label: "Charge now"},
			{Code: AttentionActionSendReminder, Label: "Email payment link"},
		},
		Since: &since,
		DueBy: inv.DueAt,
	}
}

// classifyNoPaymentMethod surfaces the finalized-unpaid invoice whose
// customer has no PaymentSetup ready. The invoice IS queued
// (auto_charge_pending) and dunning ages it out — but automation cannot
// conjure the card, so the operator/customer is the mover. Splits three
// ways: dunning already escalated (recovery over — say so), customer has
// an email (link was sent — resend/share), no email (share directly).
//
// Severity = warning (operator action required, not financially
// broken). Promoting to critical would make a perfectly normal "send-
// invoice" collection mode look alarming.
func classifyNoPaymentMethod(inv Invoice, atc AttentionContext) *Attention {
	since := inv.UpdatedAt
	hasEmail := atc.CustomerHasEmail
	// Auto-collect framing: post-ADR-013 b18d2d3 the engine queues
	// no-PM invoices via auto_charge_pending and the scheduler picks
	// them up the moment a PaymentSetup flips to ready (Chargebee's
	// "Collect Invoice on Card Update"). So attaching a PM is enough
	// — Collect Payment is the operator's *manual override* for
	// immediate charge.
	//
	// The auto-charge reassurance is CLOCK-AWARE (2026-07-22 audit,
	// P1-1): the wall-clock sweep excludes clock-pinned subs (ADR-028/029
	// disjoint flows), so on a simulated invoice a card-attach collects
	// only on the next test-clock advance — the flat "auto-charges once
	// attached" promise was a lie on exactly the demo artifacts.
	// classifyPaymentScheduled got this branch first; this one now
	// matches. The sentence lives HERE, server-owned, so the client
	// can't drift a duplicate copy of it (it used to hardcode one).
	autoCharge := "Attaching a card auto-charges this invoice on the next billing tick — no operator action needed."
	if inv.IsSimulated {
		autoCharge = "Attaching a card auto-charges this invoice on the next test-clock advance."
	}

	// Dunning has escalated: automatic recovery ended without collecting
	// and the policy's final action fired (default pauses the
	// subscription's collection). The reassuring pre-escalation framing
	// is no longer the whole story — say so (2026-07-22 audit, P1-4).
	// Charge-on-attach still works (the pending-charge sweep has no
	// pause filter), so the attach path stays the primary fix.
	if atc.DunningEscalated {
		msg := "Payment recovery has ended without collecting — the customer still has no payment method on file, and the recovery policy's final action has fired (check the subscription: collection may be paused or it may have been canceled). " + autoCharge
		actions := []AttentionActionItem{
			{Code: AttentionActionAddPaymentMethod, Label: "Open customer page"},
		}
		if hasEmail {
			actions = append([]AttentionActionItem{
				{Code: AttentionActionSendReminder, Label: "Resend setup link"},
			}, actions...)
		}
		return &Attention{
			Severity: AttentionSeverityWarning,
			Reason:   AttentionReasonDunningExhausted,
			Code:     "payment.dunning_exhausted",
			Message:  msg,
			DocURL:   docBaseURL + "dunning-exhausted",
			Actions:  actions,
			Since:    &since,
			DueBy:    inv.DueAt,
		}
	}

	// The message splits on whether the customer has an email on file,
	// because the finalize-time setup-link email (NotifyNoPaymentMethod)
	// skips when the customer has no address — the adapter treats a
	// missing email as "a delivery gap, not a billing failure" and
	// returns without sending. A single hardcoded "the customer has been
	// emailed a setup link" then lied to the operator on exactly the
	// invoices where it mattered most: no address means no send AND no
	// resend (the resend path can't email either), so telling the
	// operator to wait or resend strands the invoice. hasEmail comes
	// from AttentionContext.CustomerHasEmail, populated by the invoice
	// service from the customer record.
	//
	// Card details are always a *customer* action (PCI: entered by the
	// cardholder via Stripe's hosted flow). The operator's levers are
	// (a) resend the emailed link and (b) open the customer page, whose
	// "Add payment method" dialog can copy a secure setup link to hand
	// to the customer directly (call/chat) or add an email to the record.
	if !hasEmail {
		// hasEmail is false in two cases: the customer confirmably has no
		// address, OR the service couldn't determine it (read error / no
		// reader). The copy is phrased to be honest in BOTH — it states
		// behavior ("we email a link only when there's an address on
		// file") rather than asserting this customer's email state or
		// claiming a link was/wasn't sent, so a rare transient customer-
		// read error can't make the banner assert a falsifiable fact.
		// Either way the operator's path is the same and always works:
		// open the customer page (see the real email, copy a secure link,
		// or add an address). The "Resend setup link" action is dropped —
		// with no confirmed recipient it can't reliably send, and the
		// customer-page dialog offers both send-email and copy-link once
		// the operator is there.
		return &Attention{
			Severity: AttentionSeverityWarning,
			Reason:   AttentionReasonNoPaymentMethod,
			Code:     "payment.no_payment_method",
			Message:  "No payment method on file. We email a setup link only when the customer has an email address on file — open the customer page to copy a secure setup link and share it with the customer directly, or add an email so it can be sent. " + autoCharge,
			DocURL:   docBaseURL + "no-payment-method",
			Actions: []AttentionActionItem{
				{Code: AttentionActionAddPaymentMethod, Label: "Open customer page"},
			},
			Since: &since,
			DueBy: inv.DueAt,
		}
	}
	return &Attention{
		Severity: AttentionSeverityWarning,
		Reason:   AttentionReasonNoPaymentMethod,
		Code:     "payment.no_payment_method",
		Message:  "No payment method on file. Velox emails the customer a setup link when an invoice finalizes — the invoice timeline and Sent emails show whether it was delivered. Resend the link if they haven't acted, or open the customer page to copy it and share it directly. " + autoCharge,
		DocURL:   docBaseURL + "no-payment-method",
		Actions: []AttentionActionItem{
			{Code: AttentionActionSendReminder, Label: "Resend setup link"},
			{Code: AttentionActionAddPaymentMethod, Label: "Open customer page"},
		},
		Since: &since,
		DueBy: inv.DueAt,
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// classifyPaymentAnomaly renders the ADR-068 durable marker: Critical, both
// PaymentIntents in the message, deep-linkable PI id in Meta. The action is
// manual reconciliation (refund the duplicate / adjust the books), so the
// message says exactly what was detected.
func classifyPaymentAnomaly(inv Invoice) *Attention {
	msg := ""
	switch inv.PaymentAnomalyKind {
	case EventPaymentDuplicateCharge:
		msg = fmt.Sprintf("A second payment (%s) succeeded on this invoice after it was already paid%s. The extra charge exists only in Stripe — review and refund it.",
			inv.PaymentAnomalyPaymentIntentID, recordedPISuffix(inv))
	case EventPaymentAmountMismatch:
		msg = fmt.Sprintf("Stripe captured %d (minor units) for payment %s but this invoice booked %d. Reconcile the difference — the recorded paid amount drives refund limits.",
			inv.PaymentAnomalyCapturedCents, inv.PaymentAnomalyPaymentIntentID, inv.AmountPaidCents)
	case EventPaymentReceivedOnVoidedInvoice:
		msg = fmt.Sprintf("Payment %s succeeded against this voided invoice. The customer paid money that is not owed — refund it in Stripe.",
			inv.PaymentAnomalyPaymentIntentID)
	case EventPaymentCurrencyMismatch:
		msg = fmt.Sprintf("Payment %s was charged in a different currency than this invoice shows. The amounts were not converted — review the charge in Stripe and correct the invoice or refund.",
			inv.PaymentAnomalyPaymentIntentID)
	default:
		msg = fmt.Sprintf("A payment anomaly (%s) was detected on this invoice — review payment %s in Stripe.",
			inv.PaymentAnomalyKind, inv.PaymentAnomalyPaymentIntentID)
	}
	return &Attention{
		Reason:   AttentionReasonPaymentAnomaly,
		Severity: AttentionSeverityCritical,
		Code:     inv.PaymentAnomalyKind,
		Message:  msg,
	}
}

func recordedPISuffix(inv Invoice) string {
	if inv.StripePaymentIntentID == "" {
		return ""
	}
	return fmt.Sprintf(" (original payment %s)", inv.StripePaymentIntentID)
}
