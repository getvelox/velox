package creditnote

import (
	"context"
	"database/sql"

	"github.com/sagarsuperuser/velox/internal/domain"
	"time"
)

type Store interface {
	Create(ctx context.Context, tenantID string, cn domain.CreditNote) (domain.CreditNote, error)
	// CreateUnderInvoiceLock serializes credit-note creation for one invoice:
	// it takes a per-invoice advisory lock, lists the invoice's existing credit
	// notes in the same transaction, lets `build` run the cap checks against
	// that locked snapshot and return the note to insert, and inserts the note
	// AND its line items in that one transaction — closing the over-credit
	// TOCTOU on concurrent Create, and guaranteeing a header can never commit
	// without its lines (no orphan credit notes on partial failure).
	CreateUnderInvoiceLock(ctx context.Context, tenantID, invoiceID string, lines []domain.CreditNoteLineItem, build func(existing []domain.CreditNote) (domain.CreditNote, error)) (domain.CreditNote, error)
	// CreateUnderInvoiceLockAudited is CreateUnderInvoiceLock with the ADR-090
	// in-tx emission: emit runs on the insert's own tx with the CREATED row, so
	// the credit note and its `create` evidence commit or roll back together.
	CreateUnderInvoiceLockAudited(ctx context.Context, tenantID, invoiceID string, lines []domain.CreditNoteLineItem, build func(existing []domain.CreditNote) (domain.CreditNote, error), emit func(tx *sql.Tx, created domain.CreditNote) error) (domain.CreditNote, error)
	// CreateUnderInvoiceLockTx is CreateUnderInvoiceLock on the CALLER's tx
	// (coordinator-owned, ADR-056) so the credit note commits atomically with
	// the caller's other writes (e.g. a subscription item delete) — the caller
	// owns Begin/Commit/Rollback.
	CreateUnderInvoiceLockTx(ctx context.Context, tx *sql.Tx, tenantID, invoiceID string, lines []domain.CreditNoteLineItem, build func(existing []domain.CreditNote) (domain.CreditNote, error)) (domain.CreditNote, error)
	// CreateUnderInvoiceLockTxAudited is the caller-tx create with the same
	// in-tx emission — the create row rides the caller's coordinator tx, so
	// both create paths carry identical evidence (ADR-090 §3).
	CreateUnderInvoiceLockTxAudited(ctx context.Context, tx *sql.Tx, tenantID, invoiceID string, lines []domain.CreditNoteLineItem, build func(existing []domain.CreditNote) (domain.CreditNote, error), emit func(tx *sql.Tx, created domain.CreditNote) error) (domain.CreditNote, error)
	// CreateUnderInvoiceLockDynamicTx: build returns the header AND lines —
	// for callers whose line amounts derive from state read under locks
	// taken inside build (ADR-080 commit relief).
	CreateUnderInvoiceLockDynamicTx(ctx context.Context, tx *sql.Tx, tenantID, invoiceID string, build func(existing []domain.CreditNote) (domain.CreditNote, []domain.CreditNoteLineItem, error)) (domain.CreditNote, error)
	// ListPendingClawbackDrafts returns auto-issue clawback drafts whose
	// post-commit Issue() hasn't succeeded yet (issue_pending, status='draft'),
	// cross-tenant + scoped by livemode, for RetryPendingClawbackIssue.
	ListPendingClawbackDrafts(ctx context.Context, batch int, livemode bool) ([]domain.CreditNote, error)
	// ListPendingClawbackDraftsForClock is the catchup counterpart, scoped to one
	// test clock's simulated customers (ADR-029 disjoint flows).
	ListPendingClawbackDraftsForClock(ctx context.Context, tenantID, clockID string, batch int) ([]domain.CreditNote, error)
	// ListPendingCreditNoteTaxReversal returns issued credit notes whose
	// post-commit upstream tax reversal failed (tax_reversal_pending),
	// cross-tenant + scoped by livemode, for RetryPendingCreditNoteTaxReversal.
	ListPendingCreditNoteTaxReversal(ctx context.Context, batch int, livemode bool) ([]domain.CreditNote, error)
	// ListRefundsAwaitingProviderID returns issued credit notes that owe a card
	// refund but carry no Stripe refund id — the outcome was never learned.
	// Purely structural eligibility, so it catches both the ambiguous provider
	// error and the crash before any status write.
	ListRefundsAwaitingProviderID(ctx context.Context, olderThan time.Time, batch int, livemode bool) ([]domain.CreditNote, error)
	// BeginTx opens the RLS-scoped coordinator tx Issue() owns (ADR-056/061):
	// the draft→issued CAS and the internal money effect commit together.
	BeginTx(ctx context.Context, tenantID string) (*sql.Tx, error)
	Get(ctx context.Context, tenantID, id string) (domain.CreditNote, error)
	List(ctx context.Context, filter ListFilter) ([]domain.CreditNote, error)
	// NOTE: there is deliberately NO unguarded UpdateStatus on this interface.
	// Every status write is a CAS (TransitionStatus*) so an audit row can only
	// evidence a flip that provably happened (ADR-090).
	//
	// TransitionStatus is a compare-and-swap status flip: it succeeds (won=true)
	// only if the credit note is currently in `from`. Used to serialize the
	// draft→issued transition against concurrent/retried Issue() calls.
	TransitionStatus(ctx context.Context, tenantID, id string, from, to domain.CreditNoteStatus) (bool, error)
	// TransitionStatusAudited emits on the CAS tx, only when the CAS won
	// (ADR-090) — the orphan-void guard uses it so the draft→voided flip
	// carries its evidence.
	TransitionStatusAudited(ctx context.Context, tenantID, id string, from, to domain.CreditNoteStatus, emit func(tx *sql.Tx) error) (bool, error)
	// TransitionStatusTx is TransitionStatus on the caller's coordinator tx, so
	// the CAS commits atomically with Issue()'s internal money effect.
	TransitionStatusTx(ctx context.Context, tx *sql.Tx, tenantID, id string, from, to domain.CreditNoteStatus) (bool, error)
	UpdateRefundStatus(ctx context.Context, tenantID, id string, status domain.RefundStatus, stripeRefundID string) error
	// UpdateRefundStatusAudited is UpdateRefundStatus with the ADR-090 in-tx
	// emission for the operator refund RETRY: `prior` is read under the row
	// lock in the write's own tx, and emit fires ONLY when the persisted refund
	// state actually moves — an idempotent retry that gets the same 'pending'
	// back from Stripe records nothing.
	UpdateRefundStatusAudited(ctx context.Context, tenantID, id string, status domain.RefundStatus, stripeRefundID string, emit func(tx *sql.Tx, updated domain.CreditNote, prior domain.RefundStatus, changed bool) error) error
	// ApplyRefundWebhookStatus monotonically applies an async refund-webhook
	// status to the credit note carrying stripeRefundID: terminal
	// (succeeded/failed) always wins; a stale out-of-order 'pending' never
	// clobbers a terminal state. Returns ErrNotFound when no credit note carries
	// that refund id (foreign/dashboard refund, or the row hasn't committed yet).
	ApplyRefundWebhookStatus(ctx context.Context, tenantID, stripeRefundID string, status domain.RefundStatus) error
	// ApplyRefundWebhookStatusAudited runs the caller-supplied audit
	// emission on the same tx, ONLY when the monotonic guard actually
	// flipped a row (ADR-090) — stale redeliveries record nothing.
	ApplyRefundWebhookStatusAudited(ctx context.Context, tenantID, stripeRefundID string, status domain.RefundStatus, emit func(tx *sql.Tx, cn domain.CreditNote) error) error
	// UpdateAllocation persists the three-channel allocation
	// (refund / credit / out-of-band). Used by Issue() to re-derive the
	// allocation from the current invoice state when a CN created against
	// a then-unpaid invoice is issued after that invoice became paid.
	UpdateAllocation(ctx context.Context, tenantID, id string, refundCents, creditCents, outOfBandCents int64) error
	// UpdateAllocationTx is UpdateAllocation on the caller's coordinator tx.
	UpdateAllocationTx(ctx context.Context, tx *sql.Tx, tenantID, id string, refundCents, creditCents, outOfBandCents int64) error
	// SetTaxTransaction persists the reversal transaction id returned by
	// the tax provider at Issue time.
	SetTaxTransaction(ctx context.Context, tenantID, id string, taxTransactionID string) error
	// SetTaxReversalPending flips the post-commit tax-reversal recovery marker
	// (true on attempted-and-failed, false on success).
	SetTaxReversalPending(ctx context.Context, tenantID, id string, pending bool) error
	ListLineItems(ctx context.Context, tenantID, creditNoteID string) ([]domain.CreditNoteLineItem, error)
}

type ListFilter struct {
	TenantID   string
	InvoiceID  string
	CustomerID string
	Status     string
	// RefundStatus filters by the Stripe refund leg. Exact match, except the
	// sentinel "needs_attention" → refund_status IN ('failed','pending') (the
	// dashboard "refunds need attention" alert links here).
	RefundStatus string
	Limit        int
	Offset       int
	Sort         string // closed allow-list (validated in store); empty defaults to created_at
	SortDir      string // "asc" or "desc"; empty defaults to desc
}
