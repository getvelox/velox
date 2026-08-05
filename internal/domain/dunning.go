package domain

import (
	"strings"
	"time"
)

// Exhausting a dunning run settles TWO independent questions, and ADR-112
// gives each its own column: what happens to the SUBSCRIPTION, and what
// happens to the unpaid INVOICE.
//
// They were one enum until 2026-08-05, which made Stripe's own default
// outcome — cancel the subscription AND write off the invoice —
// inexpressible, and left the debt permanently open in the three of four
// values that were not `mark_uncollectible`. Every peer separates them
// (ADR-112 carries the verbatim quotes): Stripe derives the invoice
// write-off from the subscription setting, Recurly pairs an invoice
// auto-fail flag with an expire-subscription flag, Chargebee configures
// "the action to be performed on the subscription ... and invoice".
//
// The old `manual_review` value is not missing — it IS
// (SubActionNone, InvActionNone), matching Chargebee's "remain active"
// + "not paid".

// DunningSubscriptionAction is what exhaustion does to the subscription
// behind the unpaid invoice. No-op when the invoice has no subscription
// (a one-off), which the escalation copy must then not claim.
type DunningSubscriptionAction string

const (
	// SubActionNone leaves the subscription alone — the operator handles
	// it. Stripe "leave past_due", Chargebee "remain active".
	SubActionNone DunningSubscriptionAction = "none"
	// SubActionPause calls subscription.PauseCollection
	// (behavior=keep_as_draft): the cycle keeps drafting invoices, but
	// nothing is charged and no dunning starts until an operator resumes.
	// Matches Stripe's pause_collection — NOT the hard PauseAtomic, which
	// silently skipped invoice generation and was replaced for parity.
	SubActionPause DunningSubscriptionAction = "pause"
	// SubActionCancel cancels the subscription outright.
	SubActionCancel DunningSubscriptionAction = "cancel"
)

// DunningInvoiceAction is what exhaustion does to the unpaid invoice that
// the run was chasing.
type DunningInvoiceAction string

const (
	// InvActionNone leaves the invoice finalized and due. Honest, and the
	// default: nobody has decided this is bad debt yet. It does mean the
	// receivable stays open until a human closes or collects it, which the
	// policy form says in as many words.
	InvActionNone DunningInvoiceAction = "none"
	// InvActionMarkUncollectible writes the invoice off as bad debt. The
	// receivable closes; the invoice stays on the books for audit and stays
	// settleable out of band (ADR-110).
	//
	// Auto-VOID and Chargebee's auto-"Reverse" (mark paid + adjustment
	// credit note) are deliberately NOT offered — see ADR-112. Void asserts
	// the sale never happened and reverses tax (ADR-111); a machine making
	// that assertion about delivered usage is precisely the bad money event
	// the governing rule refuses to CREATE.
	InvActionMarkUncollectible DunningInvoiceAction = "mark_uncollectible"
)

// Valid reports whether the action is a known enum value. UpsertPolicy
// refuses anything else rather than defaulting — an unrecognized action
// silently becoming "none" would quietly stop closing receivables the
// operator asked to close.
func (a DunningSubscriptionAction) Valid() bool {
	return a == SubActionNone || a == SubActionPause || a == SubActionCancel
}

// Valid reports whether the action is a known enum value. See the note on
// DunningSubscriptionAction.Valid.
func (a DunningInvoiceAction) Valid() bool {
	return a == InvActionNone || a == InvActionMarkUncollectible
}

// DunningEscalationOutcome is what a terminal exhaustion actually did to
// ONE invoice — which is not the same as what its policy asked for. Each
// field means "the requested end state holds", so it stays true across a
// re-attempt that skipped a half already in place, and false when the
// action could not apply here at all (a one-off invoice has no
// subscription to cancel; an unwired mover cannot act).
//
// The escalation email, the escalated timeline row, and the outbound
// webhook all render THIS rather than the policy. A debtor must never
// read "your subscription has been canceled" when nothing was.
type DunningEscalationOutcome struct {
	SubscriptionPaused   bool
	SubscriptionCanceled bool
	InvoiceWrittenOff    bool
}

// Reason is the stable machine token stamped on the escalated event and
// the outbound webhook. It describes OUTCOMES rather than echoing the
// policy: "" means the run escalated with nothing changed, which is the
// honest reading of a (none, none) policy and of a one-off invoice under
// a cancel policy alike.
func (o DunningEscalationOutcome) Reason() string {
	var parts []string
	if o.SubscriptionPaused {
		parts = append(parts, "subscription_paused")
	}
	if o.SubscriptionCanceled {
		parts = append(parts, "subscription_canceled")
	}
	if o.InvoiceWrittenOff {
		parts = append(parts, "invoice_marked_uncollectible")
	}
	return strings.Join(parts, "+")
}

// Any reports whether exhaustion changed anything at all.
func (o DunningEscalationOutcome) Any() bool {
	return o.SubscriptionPaused || o.SubscriptionCanceled || o.InvoiceWrittenOff
}

type DunningRunState string

const (
	DunningActive    DunningRunState = "active"
	DunningResolved  DunningRunState = "resolved"
	DunningEscalated DunningRunState = "escalated"
	DunningPaused    DunningRunState = "paused"
)

// DunningStartCause is WHY collection needed a dunning run — recorded
// at write time by the caller that starts the run (each one knows the
// cause precisely; deriving it later from invoice state re-creates the
// interpretation-drift class ADR-101 killed for billing). Stamped on
// the run and its dunning_started event, and rendered on the invoice
// timeline so the failure story survives after the attention banner
// clears.
type DunningStartCause string

const (
	// A real charge was attempted and failed (interactive decline,
	// auto-charge decline, or the failed-invoice backfill sweep).
	DunningCausePaymentFailed DunningStartCause = "payment_failed"
	// No usable payment method existed, so nothing was ever charged —
	// the card-less "reminder cycle" enrollment. Rendering a "payment
	// failed" row for these would be a lie: no payment happened.
	DunningCauseNoPaymentMethod DunningStartCause = "no_payment_method"
)

// Valid reports whether the cause is a known enum value — StartDunning
// refuses anything else (no-silent-fallbacks: an unlabeled run would
// re-introduce the hardcoded-reason lie this type exists to end).
func (c DunningStartCause) Valid() bool {
	return c == DunningCausePaymentFailed || c == DunningCauseNoPaymentMethod
}

type DunningEventType string

const (
	DunningEventStarted        DunningEventType = "dunning_started"
	DunningEventRetryScheduled DunningEventType = "retry_scheduled"
	DunningEventRetryAttempted DunningEventType = "retry_attempted"
	DunningEventRetrySucceeded DunningEventType = "retry_succeeded"
	DunningEventRetryFailed    DunningEventType = "retry_failed"
	DunningEventPaused         DunningEventType = "paused"
	DunningEventResumed        DunningEventType = "resumed"
	DunningEventEscalated      DunningEventType = "escalated"
	DunningEventResolved       DunningEventType = "resolved"
)

type DunningResolution string

const (
	ResolutionPaymentRecovered DunningResolution = "payment_recovered"
	// ResolutionInvoiceVoided: the run closed because its invoice was
	// VOIDED — annulled, applied credits reversed, collection stopped at
	// Stripe. Written by the operator's "Void invoice" resolution, by the
	// invoice-void handler, and by the engine's terminal floor when it finds
	// an already-voided invoice.
	//
	// Split out of ResolutionManuallyResolved (migration 0170) because that
	// one value was written for voided AND uncollectible invoices alike, so
	// the column could not answer which of the two had happened — while
	// ResolutionInvoiceNotCollectible, which names the write-off exactly,
	// sat right beside it being avoided.
	ResolutionInvoiceVoided DunningResolution = "invoice_voided"
	// ResolutionManuallyResolved is LEGACY — no writer emits it. It stays
	// legal so the rows written before the 0170 split remain readable, and
	// so the operator endpoint can name it in a rejection rather than
	// pretending it never existed. One row survives the backfill by design:
	// a run resolved with this value whose invoice never transitioned
	// (best-effort propagation failed), where neither successor is true and
	// guessing would fabricate an outcome.
	ResolutionManuallyResolved DunningResolution = "manually_resolved"
	ResolutionRetriesExhausted DunningResolution = "retries_exhausted"
	// ResolutionInvoiceNotCollectible is the operator-driven equivalent
	// of the automated MarkUncollectible final-action. Picking this in
	// the resolve-dunning dialog records the run as resolved AND flips
	// the underlying invoice to status=uncollectible (bad debt). Same
	// downstream side-effects as the automated path — Stripe-parity
	// "Mark uncollectible" surfaced on the dunning resolve flow.
	ResolutionInvoiceNotCollectible DunningResolution = "invoice_not_collectible"
	// ResolutionActionFailed marks a run whose terminal final_action
	// (pause / cancel / mark-uncollectible) FAILED at exhaustion — a
	// Stripe blip, a conflicting state, a DB error. The run is left
	// state=active (NOT escalated) with next_action_at set so the due-run
	// pickers re-attempt the action, rather than recording a clean
	// "escalated" beside an invoice/sub that never actually got closed.
	ResolutionActionFailed DunningResolution = "action_failed"
)

// DunningPolicy is a named template that drives the retry state
// machine for one or more customers (ADR-036, campaigns model). One
// is_default=true policy per (tenant, livemode); customers without an
// explicit `dunning_policy_id` assignment inherit the default. Mirrors
// the Lago / Recurly named-campaigns shape verified during the 2026-
// 05-16 industry research.
type DunningPolicy struct {
	ID               string   `json:"id"`
	TenantID         string   `json:"tenant_id,omitempty"`
	Name             string   `json:"name"`
	Enabled          bool     `json:"enabled"`
	IsDefault        bool     `json:"is_default"`
	RetrySchedule    []string `json:"retry_schedule"`
	MaxRetryAttempts int      `json:"max_retry_attempts"`
	// The two terminal decisions (ADR-112). Both are always present; the
	// (none, none) pair is the old `manual_review`.
	FinalSubscriptionAction DunningSubscriptionAction `json:"final_subscription_action"`
	FinalInvoiceAction      DunningInvoiceAction      `json:"final_invoice_action"`
	GracePeriodDays         int                       `json:"grace_period_days"`
	CreatedAt               time.Time                 `json:"created_at"`
	UpdatedAt               time.Time                 `json:"updated_at"`
}

type InvoiceDunningRun struct {
	ID            string            `json:"id"`
	TenantID      string            `json:"tenant_id,omitempty"`
	InvoiceID     string            `json:"invoice_id"`
	CustomerID    string            `json:"customer_id,omitempty"`
	PolicyID      string            `json:"policy_id"`
	State         DunningRunState   `json:"state"`
	Reason        string            `json:"reason,omitempty"`
	AttemptCount  int               `json:"attempt_count"`
	LastAttemptAt *time.Time        `json:"last_attempt_at,omitempty"`
	NextActionAt  *time.Time        `json:"next_action_at,omitempty"`
	Paused        bool              `json:"paused"`
	ResolvedAt    *time.Time        `json:"resolved_at,omitempty"`
	Resolution    DunningResolution `json:"resolution,omitempty"`
	CreatedAt     time.Time         `json:"created_at"`
	UpdatedAt     time.Time         `json:"updated_at"`

	// Denormalized for list rendering (saves N round-trips for the
	// /dunning page rendering invoice number / amount due / currency
	// per row). Populated by the LEFT JOIN in ListRuns; empty/zero
	// when the joined invoice can't be resolved (deleted, RLS gap).
	InvoiceNumber    string `json:"invoice_number,omitempty"`
	InvoiceAmountDue int64  `json:"invoice_amount_due_cents,omitempty"`
	InvoiceCurrency  string `json:"invoice_currency,omitempty"`

	// EffectiveNow is the owning customer's test-clock frozen_time when
	// the run is on a clock-pinned sub. Frontend uses this as the
	// "now" baseline for relative-time rendering — wall-clock would
	// surface "overdue" on every row whose next_action_at sits in
	// frozen-time domain (e.g. 2024-02 while wall-clock is 2026).
	// Nil for wall-clock runs (the relative-time renderer falls back
	// to Date.now()). Authoritative; replaces the prior client-side
	// 24h-divergence heuristic.
	EffectiveNow *time.Time `json:"effective_now,omitempty"`
	// TestClockID is the owning customer's clock when pinned — drives
	// the amber test-clock badge server-authoritatively (the prior
	// client-side subscription-map heuristic lost the badge on
	// one-off-invoice runs and past page 1).
	TestClockID string `json:"test_clock_id,omitempty"`
}

// CustomerDunningOverride was removed in ADR-036. The partial-field
// override (override max + grace + final_action but inherit
// retry_schedule) had no industry precedent (Stripe / Lago / Orb /
// Recurly all use named templates with full assignment, verified
// 2026-05-16). Per-customer differentiation now flows through
// `customers.dunning_policy_id` referencing a DunningPolicy row.

type InvoiceDunningEvent struct {
	ID           string           `json:"id"`
	RunID        string           `json:"run_id"`
	TenantID     string           `json:"tenant_id,omitempty"`
	InvoiceID    string           `json:"invoice_id"`
	EventType    DunningEventType `json:"event_type"`
	State        DunningRunState  `json:"state"`
	Reason       string           `json:"reason,omitempty"`
	AttemptCount int              `json:"attempt_count"`
	Metadata     map[string]any   `json:"metadata,omitempty"`
	CreatedAt    time.Time        `json:"created_at"`
	// RecordedAt is the real-world INSERT instant (ADR-104 Invariant A):
	// CreatedAt follows the entity's calendar (simulated during catchup,
	// ADR-030), so this is the row's only wall stamp. Nil pre-0164.
	RecordedAt *time.Time `json:"recorded_at,omitempty"`
}
