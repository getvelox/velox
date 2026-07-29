package domain

import (
	"time"

	"github.com/shopspring/decimal"
)

type CreditNoteStatus string

const (
	CreditNoteDraft  CreditNoteStatus = "draft"
	CreditNoteIssued CreditNoteStatus = "issued"
	CreditNoteVoided CreditNoteStatus = "voided"
)

type RefundStatus string

const (
	RefundNone      RefundStatus = "none"
	RefundPending   RefundStatus = "pending"
	RefundSucceeded RefundStatus = "succeeded"
	RefundFailed    RefundStatus = "failed"
)

type CreditNote struct {
	ID                   string           `json:"id"`
	TenantID             string           `json:"tenant_id,omitempty"`
	InvoiceID            string           `json:"invoice_id"`
	CustomerID           string           `json:"customer_id"`
	CreditNoteNumber     string           `json:"credit_note_number"`
	Status               CreditNoteStatus `json:"status"`
	Reason               string           `json:"reason"`
	SubtotalCents        int64            `json:"subtotal_cents"`
	TaxAmountCents       int64            `json:"tax_amount_cents"`
	TotalCents           int64            `json:"total_cents"`
	RefundAmountCents    int64            `json:"refund_amount_cents"`
	CreditAmountCents    int64            `json:"credit_amount_cents"`
	OutOfBandAmountCents int64            `json:"out_of_band_amount_cents"`
	// CommitRetiredCents: how many commit credits THIS credit note retired
	// (ADR-080 relief leg; 0 for every non-relief CN). Real column, not
	// metadata — auditors reconcile grant.cn_retired_cents ==
	// SUM(cn.commit_retired_cents).
	CommitRetiredCents int64        `json:"commit_retired_cents,omitempty"`
	Currency           string       `json:"currency"`
	IssuedAt           *time.Time   `json:"issued_at,omitempty"`
	VoidedAt           *time.Time   `json:"voided_at,omitempty"`
	RefundStatus       RefundStatus `json:"refund_status"`
	StripeRefundID     string       `json:"stripe_refund_id,omitempty"`
	// TaxTransactionID is the upstream reversal transaction id returned
	// by the tax provider (Stripe: tx_xxx for the negative tax_transaction)
	// when Issue succeeds. Empty while the credit note is draft, or when
	// the invoice has no upstream tax state to reverse (manual/none
	// provider, or legacy invoice pre-dating invoice.tax_transaction_id).
	TaxTransactionID string `json:"tax_transaction_id,omitempty"`
	// IsSimulated marks a credit note whose issued_at is in simulated
	// (test-clock) time — true for engine clawbacks on a clock-pinned sub,
	// false for operator HTTP issuance (always wall-clock). Drives the
	// invoice activity-timeline lane so a simulated CN isn't shown with a
	// simulated timestamp in the wall-clock "Real-time activity" lane.
	IsSimulated bool `json:"is_simulated"`
	// IssuePending marks an AUTO-ISSUE clawback draft: created IN-TRANSACTION
	// with a subscription downgrade / item-removal / qty-decrease (so the item
	// change and the clawback obligation commit atomically), then issued
	// post-commit. Set true ONLY at create; NEVER cleared. RetryPendingClawback
	// Issue recovers a draft whose post-commit Issue() never ran (e.g. a crash
	// before issuance) — it scans status='draft' AND issue_pending, so an
	// issued CN drops out via its status, not via this flag. (As of ADR-061 the
	// post-CAS window is closed: Issue() commits the status flip and the internal
	// money effect on ONE tx, so there is no 'issued' CN with an un-applied
	// internal effect; the external legs self-heal via their own sweeps —
	// RetryRefund and RetryPendingCreditNoteTaxReversal.) Always false for
	// operator-created drafts (migration 0121).
	IssuePending bool `json:"issue_pending"`
	// TaxReversalPending is the FAST-PATH marker for an issued credit note whose
	// POST-COMMIT upstream tax reversal was attempted and FAILED (transient Stripe
	// error). RetryPendingCreditNoteTaxReversal re-drives marked rows with the
	// per-CN velox_tax_rev_<cn.ID> key and clears the flag on success. It is an
	// optimisation (partial-index-backed), NOT the sole recovery key: the sweep
	// ALSO derives eligibility structurally (an issued CN with no reversal stamped
	// against a tax-bearing source), so a failed marker write does not lose
	// recovery (ADR-061). Distinct from a NULL tax_transaction_id (ambiguous
	// across not-tried / no-provider): this boolean is the explicit "tried, failed."
	TaxReversalPending bool           `json:"tax_reversal_pending"`
	Metadata           map[string]any `json:"metadata,omitempty"`
	CreatedAt          time.Time      `json:"created_at"`
	// RecordedAt is the real-world instant this row was INSERTed (ADR-104
	// Invariant A): CreatedAt follows the entity's calendar (simulated when
	// clock-pinned, ADR-030), so without this the row had no wall stamp at
	// all and the timeline couldn't show both calendars. Nil on rows
	// created before migration 0164 — never backfilled.
	RecordedAt *time.Time `json:"recorded_at,omitempty"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

type CreditNoteLineItem struct {
	ID                string    `json:"id"`
	CreditNoteID      string    `json:"credit_note_id"`
	TenantID          string    `json:"tenant_id,omitempty"`
	InvoiceLineItemID string    `json:"invoice_line_item_id,omitempty"`
	Description       string    `json:"description"`
	Quantity          int64     `json:"quantity"`
	UnitAmountCents   int64     `json:"unit_amount_cents"`
	AmountCents       int64     `json:"amount_cents"`
	CreatedAt         time.Time `json:"created_at"`
}

type CustomerPriceOverride struct {
	ID       string `json:"id"`
	TenantID string `json:"tenant_id,omitempty"`
	// RuleKey is the override's identity (ADR-070): a negotiated price
	// follows the RULE across version publishes. RatingRuleVersionID
	// records which version the operator was looking at when the
	// override was created — provenance, never a lookup key.
	RuleKey                string          `json:"rule_key"`
	CustomerID             string          `json:"customer_id"`
	RatingRuleVersionID    string          `json:"rating_rule_version_id"`
	Mode                   PricingMode     `json:"mode"`
	FlatAmountCents        decimal.Decimal `json:"flat_amount_cents"`
	GraduatedTiers         []RatingTier    `json:"graduated_tiers"`
	PackageSize            int64           `json:"package_size"`
	PackageAmountCents     int64           `json:"package_amount_cents"`
	OverageUnitAmountCents decimal.Decimal `json:"overage_unit_amount_cents"`
	Reason                 string          `json:"reason,omitempty"`
	Active                 bool            `json:"active"`
	CreatedAt              time.Time       `json:"created_at"`
	UpdatedAt              time.Time       `json:"updated_at"`
}

// ApplyTo patches the override's PRICE onto the resolved rating-rule
// version: only Mode and the pricing fields are replaced; the base
// rule's ID, RuleKey, Name, and — critically — Currency survive
// (ADR-070: an override freezes price, not rule semantics). The old
// replace-wholesale shape fabricated a rule with Currency == "" —
// preview totals silently dropped every overridden line and threshold
// fires hard-failed "no invoice currency resolved" on usage-only
// all-override subscriptions.
func (o CustomerPriceOverride) ApplyTo(base RatingRuleVersion) RatingRuleVersion {
	patched := base
	patched.Mode = o.Mode
	patched.FlatAmountCents = o.FlatAmountCents
	patched.GraduatedTiers = o.GraduatedTiers
	patched.PackageSize = o.PackageSize
	patched.PackageAmountCents = o.PackageAmountCents
	patched.OverageUnitAmountCents = o.OverageUnitAmountCents
	return patched
}
