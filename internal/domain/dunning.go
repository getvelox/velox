package domain

import "time"

// DunningFinalAction is the terminal-state action when a dunning run
// exhausts its retries. Aligned with the cross-platform set verified
// 2026-05-16 (ADR-036 amendment): Stripe / Lago / Orb / Recurly all
// converge on some subset of {manual_review, pause, mark_uncollectible,
// cancel_subscription}. Velox supports all four.
//
// Semantics:
//   - ManualReview       — run lands at state=escalated, no sub/invoice
//     mutation; operator handles. Maps to Stripe
//     "Keep active" and Lago default.
//   - Pause              — calls subscription.SetPauseCollection
//     (behavior=keep_as_draft). Cycle keeps drafting
//     invoices; no charging / dunning until resumed.
//     Matches Stripe's pause_collection (NOT the
//     hard PauseAtomic — that was the pre-amendment
//     implementation, replaced for Stripe-parity).
//   - MarkUncollectible  — marks the unpaid invoice as uncollectible
//     (industry-standard term for "we won't try
//     again; close out the receivable"). Replaces
//     the pre-amendment "write_off_later" naming.
//   - CancelSubscription — cancels the subscription. Stripe-default
//     terminal action; supported by 3 of 4
//     reference platforms.
type DunningFinalAction string

const (
	DunningActionManualReview       DunningFinalAction = "manual_review"
	DunningActionPause              DunningFinalAction = "pause"
	DunningActionMarkUncollectible  DunningFinalAction = "mark_uncollectible"
	DunningActionCancelSubscription DunningFinalAction = "cancel_subscription"
)

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
	ID               string             `json:"id"`
	TenantID         string             `json:"tenant_id,omitempty"`
	Name             string             `json:"name"`
	Enabled          bool               `json:"enabled"`
	IsDefault        bool               `json:"is_default"`
	RetrySchedule    []string           `json:"retry_schedule"`
	MaxRetryAttempts int                `json:"max_retry_attempts"`
	FinalAction      DunningFinalAction `json:"final_action"`
	GracePeriodDays  int                `json:"grace_period_days"`
	CreatedAt        time.Time          `json:"created_at"`
	UpdatedAt        time.Time          `json:"updated_at"`
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
}
