package invoice

import (
	"context"
	"database/sql"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

type Store interface {
	// ActionRequiredObsolete reports whether an action-required email
	// referencing this invoice is moot: the invoice is settled (paid /
	// voided / uncollectible) or no longer exists (test-clock teardown).
	// Consumed by the email dispatcher's staleness gate via the
	// composition root — email never imports invoice.
	ActionRequiredObsolete(ctx context.Context, tenantID, invoiceNumber string) (bool, error)
	Create(ctx context.Context, tenantID string, inv domain.Invoice) (domain.Invoice, error)
	CreateWithLineItems(ctx context.Context, tenantID string, inv domain.Invoice, items []domain.InvoiceLineItem) (domain.Invoice, error)
	// CreateAudited / CreateWithLineItemsAudited / AddLineItemAtomicAudited run
	// the caller-supplied audit emission on the SAME transaction as the write
	// (ADR-090 in-tx emission): the invoice mutation and its audit row commit
	// or roll back together. The service builds the emission closure (it owns
	// audit-row content — the manual-create / line-item-added intent); the
	// store owns the transaction and exposes it to the closure. emit receives
	// the PERSISTED row(s) so the audit row can reference the store-assigned
	// id, invoice number, and recomputed totals. nil emit = unaudited write —
	// what the engine's cycle/threshold paths pass, since their canonical
	// evidence is the finalize row.
	CreateAudited(ctx context.Context, tenantID string, inv domain.Invoice, emit func(tx *sql.Tx, out domain.Invoice) error) (domain.Invoice, error)
	CreateWithLineItemsAudited(ctx context.Context, tenantID string, inv domain.Invoice, items []domain.InvoiceLineItem, emit func(tx *sql.Tx, out domain.Invoice) error) (domain.Invoice, error)
	Get(ctx context.Context, tenantID, id string) (domain.Invoice, error)
	GetByNumber(ctx context.Context, tenantID, number string) (domain.Invoice, error)
	GetByProrationSource(ctx context.Context, tenantID, subscriptionID, subscriptionItemID string, changeType domain.ItemChangeType, changeAt time.Time) (domain.Invoice, error)
	List(ctx context.Context, filter ListFilter) ([]domain.Invoice, int, error)
	UpdateStatus(ctx context.Context, tenantID, id string, status domain.InvoiceStatus) (domain.Invoice, error)
	// UpdateStatusWithReversal flips the status AND runs reverseFn (the
	// consumed-credit reversal on void) on the SAME tx — either both commit or
	// neither, so a void never strands the customer's applied credits unrestored.
	UpdateStatusWithReversal(ctx context.Context, tenantID, id string, status domain.InvoiceStatus, reverseFn func(tx *sql.Tx) error) (domain.Invoice, error)
	// UpdateStatusWithReversalPrior additionally hands the closure the TRUE
	// prior status read under the row lock (ADR-090: audit rows must not
	// stamp status_before from a racy pre-tx snapshot).
	UpdateStatusWithReversalPrior(ctx context.Context, tenantID, id string, status domain.InvoiceStatus, reverseFn func(tx *sql.Tx, prior domain.InvoiceStatus) error) (domain.Invoice, error)
	// FinalizeWithDates flips status to finalized AND re-stamps issued_at +
	// due_at to the finalize moment. Used for operator-composed (manual)
	// invoices so the issue/due dates anchor to issuance, not draft-create
	// (Stripe finalized_at semantics). Cycle invoices keep their build-time
	// dates via UpdateStatus.
	FinalizeWithDates(ctx context.Context, tenantID, id string, issuedAt, dueAt time.Time) (domain.Invoice, error)
	UpdatePayment(ctx context.Context, tenantID, id string, paymentStatus domain.InvoicePaymentStatus, stripePaymentIntentID, lastPaymentError string, paidAt *time.Time) (domain.Invoice, error)
	// MarkPaymentFailedReportingTransition records a failure and reports
	// whether THIS call is the first to fire the failure-notification set
	// (payment.failed event + customer email + dunning) for this
	// PaymentIntent. SettleFailed gates those side-effects on the flag so a
	// concurrent at-least-once redelivery of the same
	// payment_intent.payment_failed doesn't double-notify. PI-keyed because
	// failure is non-terminal (one fresh failure per dunning retry).
	MarkPaymentFailedReportingTransition(ctx context.Context, tenantID, id, paymentIntentID, lastPaymentError string, then func(tx *sql.Tx, fresh domain.Invoice) error) (domain.Invoice, bool, error)
	// MarkPaid flips status='paid', payment_status='succeeded',
	// amount_paid=amount_due, amount_due=0 in one transaction. Used by
	// the engine's zero-amount auto-pay path, the billing threshold
	// scan, and the operator-driven Record Offline Payment flow. The
	// store is the right place to atomically mutate all four fields
	// — Service.RecordOfflinePayment calls this so an out-of-band
	// collection produces the same invoice end-state as a successful
	// engine charge.
	MarkPaid(ctx context.Context, tenantID, id string, stripePaymentIntentID string, paidAt time.Time) (domain.Invoice, error)
	ApplyCreditNote(ctx context.Context, tenantID, id string, amountCents int64) (domain.Invoice, error)

	SetAutoChargePending(ctx context.Context, tenantID, id string, pending bool) error
	// SetNoPMNotifiedAt stamps the send-once marker for the no-PM setup-link
	// email (checked by the auto-charge sweep; stamped by whichever sender
	// delivered the email).
	SetNoPMNotifiedAt(ctx context.Context, tenantID, invoiceID string, at time.Time) error
	// ClaimAutoCharge / ReleaseAutoChargeClaim: the per-invoice charge
	// lease (HA hazard #1) — exactly one sweep leader enters the charge
	// leg per invoice per 5m window.
	ClaimAutoCharge(ctx context.Context, tenantID, id string) (bool, error)
	// ClaimChargeForManualCollect: operator-collect twin on the same
	// lease — see postgres.go (2026-07-21 snapshot-race audit).
	ClaimChargeForManualCollect(ctx context.Context, tenantID, id string) (bool, error)
	ReleaseAutoChargeClaim(ctx context.Context, tenantID, id string) error
	ListAutoChargePending(ctx context.Context, limit int) ([]domain.Invoice, error)

	CreateLineItem(ctx context.Context, tenantID string, item domain.InvoiceLineItem) (domain.InvoiceLineItem, error)
	ListLineItems(ctx context.Context, tenantID, invoiceID string) ([]domain.InvoiceLineItem, error)

	// AddLineItemAtomic inserts a line item and recomputes the invoice totals
	// in one transaction. It locks the invoice row FOR UPDATE, verifies draft
	// status, inserts the line item (filling InvoiceID, TenantID, and Currency
	// from the locked invoice), recomputes the subtotal from all line items,
	// and rewrites subtotal/total/amount_due. The lock prevents lost updates
	// when two clients append lines concurrently. Returns the inserted line
	// item and the updated invoice.
	AddLineItemAtomic(ctx context.Context, tenantID, invoiceID string, item domain.InvoiceLineItem) (domain.InvoiceLineItem, domain.Invoice, error)
	// AddLineItemAtomicAudited is AddLineItemAtomic with an in-tx audit
	// emission (see CreateAudited). emit receives the persisted line item and
	// the rewritten invoice — the invoice number it labels the row with is
	// therefore read INSIDE the tx, under the same row lock as the mutation.
	AddLineItemAtomicAudited(ctx context.Context, tenantID, invoiceID string, item domain.InvoiceLineItem, emit func(tx *sql.Tx, item domain.InvoiceLineItem, inv domain.Invoice) error) (domain.InvoiceLineItem, domain.Invoice, error)

	// UpdateTaxAtomic re-stamps an invoice's tax decision after a manual
	// retry. Locks the invoice row, gates on tax_status in (pending, failed)
	// and status='draft', rewrites per-line tax stamps and invoice-level tax
	// columns, increments tax_retry_count, recomputes total + amount_due.
	// Returns errs.InvalidState when a gate fails (caller surfaces 409) and
	// errs.ErrNotFound when the invoice doesn't exist (caller surfaces 404).
	UpdateTaxAtomic(ctx context.Context, tenantID, invoiceID string, update domain.InvoiceTaxRetryUpdate, lineItems []domain.InvoiceLineItem) (domain.Invoice, error)

	// ListPendingTaxRetry returns draft invoices awaiting another
	// tax-calculation attempt: tax_status in (pending, failed),
	// tax_error_code is in the retryable set (the worker filters
	// further on this side), tax_next_retry_at is NULL or in the
	// past, and tax_retry_count is below the per-invoice cap.
	// Ordered by tax_next_retry_at ASC (NULLS FIRST) so newly-
	// stuck invoices process first. Cross-tenant; returned rows
	// carry their tenant_id so the caller can dispatch per-row
	// with the right RLS partition.
	//
	// livemode filters to a single mode partition. The reconciler
	// runs once per mode in the scheduler tick; without this filter
	// the cross-mode RLS-bypassed scan would return rows for the
	// other mode that fail per-row RLS lookup with "not found".
	ListPendingTaxRetry(ctx context.Context, batch int, retryableCodes []string, maxAttempts int, livemode bool) ([]domain.Invoice, error)
	// ListPendingTaxCommit finds finalized/paid/voided/uncollectible
	// stripe_tax invoices with a successful calculation (tax_status=ok,
	// tax_calculation_id set) but no persisted tax_transaction_id — the orphan
	// state from a commit that succeeded at Stripe but failed to persist
	// locally (incl. the case where finalize+auto-charge flipped the invoice
	// to 'paid' before the reconciler's first scan). Powers the commit
	// reconciler.
	ListPendingTaxCommit(ctx context.Context, batch int, livemode bool) ([]domain.Invoice, error)
	// ListPendingTaxReversal finds voided/uncollectible stripe_tax invoices that
	// still carry a tax_transaction_id but have tax_reversed_at NULL — a tax
	// reversal that failed (or never completed) at Stripe, leaving the tenant
	// over-remitting. Powers the reversal recovery sweep (symmetric sibling of
	// the #267 commit reconciler). Bounded to a freshness window so it doesn't
	// re-scan ancient voids forever.
	ListPendingTaxReversal(ctx context.Context, batch int, livemode bool) ([]domain.Invoice, error)
	// MarkTaxReversed stamps tax_reversed_at=now() once the upstream reversal is
	// confirmed (or there was nothing to reverse), dropping the row from the
	// reversal sweep.
	MarkTaxReversed(ctx context.Context, tenantID, id string) error

	// ListPendingTaxRetryForClock is the catchup-path counterpart to
	// ListPendingTaxRetry — returns clock-pinned draft invoices stuck
	// at tax_status=pending with a retryable code. ADR-029 Phase 2:
	// clock-pinned tax retries fire only on operator Advance, never on
	// the wall-clock tick. One retry per row per Advance (no backoff
	// gate) — operator-friendly: each click does something visible.
	ListPendingTaxRetryForClock(ctx context.Context, tenantID, clockID string, retryableCodes []string, maxAttempts, limit int) ([]domain.Invoice, error)

	// ListCustomerDataInvalidErrors returns draft invoices for ONE
	// customer stuck on `customer_data_invalid` — the only tax error a
	// billing-profile update can resolve. Backs the per-customer
	// retry-flush fired from customer.Service.UpsertBillingProfile,
	// mirroring the ADR-019 Stripe-reconnect flush pattern at
	// per-customer scope.
	ListCustomerDataInvalidErrors(ctx context.Context, tenantID, customerID string) ([]domain.Invoice, error)

	// ListProviderConfigErrors returns draft invoices stuck on Stripe-
	// configuration errors (tax_error_code IN provider_not_configured,
	// provider_auth) for the given (tenant, livemode). Used by the
	// tenantstripe.Connect path to fan out a one-shot retry the moment
	// fresh credentials land — operator-driven recovery, not
	// background polling. Tenant-scoped RLS via TxTenant; the per-mode
	// filter is in the WHERE clause so a test-mode connect doesn't
	// surface live-mode rows. ADR-019.
	ListProviderConfigErrors(ctx context.Context, tenantID string, livemode bool) ([]domain.Invoice, error)

	// GetOutstandingBalance returns the customer's accounts-receivable
	// exposure: SUM(amount_due_cents) + COUNT(*) of finalized invoices
	// in payment_status pending/failed/unknown, excluding voided +
	// uncollectible (those are either canceled or written off as bad
	// debt). Powers the "Outstanding balance" card on customer detail —
	// industry-standard surface (Stripe customer overview tile, Lago /
	// Chargebee / Recurly customer page) that operators rely on to see
	// total AR exposure for a customer at a glance.
	GetOutstandingBalance(ctx context.Context, tenantID, customerID string) (OutstandingBalance, error)

	// SetPublicToken writes (or overwrites) the hosted-invoice-URL token for
	// an invoice. Called from Service.Finalize at first generation and from
	// the rotate-public-token endpoint for explicit rotation. Invoice must
	// exist and be non-draft; drafts never carry a token. Returns
	// errs.ErrNotFound if the invoice is missing or still draft.
	SetPublicToken(ctx context.Context, tenantID, invoiceID, token string) error

	// GetByPublicToken resolves the hosted-invoice-URL token to its invoice
	// AND its livemode (the second return value). Runs under TxBypass because
	// the caller is unauthenticated: the public route receives a raw token
	// from the URL and must resolve it to a tenant + mode BEFORE any tenant
	// context can be set. The livemode return is what callers need to pin
	// `postgres.WithLivemode(ctx, …)` on the request context for every
	// downstream RLS-scoped read; without it, the public route defaults to
	// live and a test-mode invoice's line items / customer / settings reads
	// silently 404. The token itself is the credential (256 bits of entropy,
	// UNIQUE indexed) so cross-tenant probing isn't feasible. Returns
	// errs.ErrNotFound on miss.
	GetByPublicToken(ctx context.Context, token string) (domain.Invoice, bool, error)

	// GetByStripeInvoiceID resolves a Stripe invoice id (in_xxx) to its
	// imported Velox invoice row. Built as the idempotency check for the
	// Stripe importer removed in the 2026-04-29 lean-cut (rebuild deferred
	// until a design-partner cutover) — no production caller today. The
	// partial unique index from migration 0063 makes stripe_invoice_id the
	// dedup key for imported rows. Returns errs.ErrNotFound when no invoice
	// carries that Stripe id.
	GetByStripeInvoiceID(ctx context.Context, tenantID, stripeInvoiceID string) (domain.Invoice, error)

	// FindBaseInvoiceForPeriod returns the most-recent NON-VOIDED invoice
	// carrying a base-fee line item with billing_period_start = the given
	// periodStart for the given subscription. Used by proration-credit
	// paths (BillOnCancel + handleItemProration on downgrade) to verify
	// the customer actually paid for the current period before granting
	// a "refund" — industry standard (Chargebee distinguishes Refundable
	// vs Adjustment credit notes based on this; Stripe warns operators
	// to disable proration when the latest invoice is unpaid).
	//
	// For in_advance plans, the cycle-close invoice from the prior period
	// (or BillOnCreate's day-1 invoice for new subs) carries the base
	// line whose billing_period_start matches the now-being-canceled
	// period's start. For in_arrears plans no such line exists —
	// callers shouldn't be granting unpaid-period credits to in_arrears
	// subs in the first place. Returns errs.ErrNotFound when no
	// matching invoice exists.
	FindBaseInvoiceForPeriod(ctx context.Context, tenantID, subscriptionID string, periodStart time.Time) (domain.Invoice, error)

	// LatestThresholdPeriodEnd returns the latest billing_period_end of
	// the subscription's non-voided threshold invoices whose
	// billing_period_start falls inside [periodStart, periodEnd). The
	// cycle close treats it as the "billed through" watermark so a
	// reset_billing_cycle=false threshold fire doesn't get re-billed at
	// period end. errs.ErrNotFound when no threshold invoice exists.
	LatestThresholdPeriodEnd(ctx context.Context, tenantID, subscriptionID string, periodStart, periodEnd time.Time) (time.Time, error)

	// RecordChargeAttempt / ListChargeAttemptsByInvoice are the ADR-102
	// charge-attempt facts: one row per attempt, upserted by
	// PaymentIntent id as settle paths resolve the outcome. The
	// timeline renders each attempt exactly once via the precedence
	// chain dunning row → attempt row → stripe webhook row.
	RecordChargeAttempt(ctx context.Context, tenantID string, a domain.InvoiceChargeAttempt) error
	ListChargeAttemptsByInvoice(ctx context.Context, tenantID, invoiceID string) ([]domain.InvoiceChargeAttempt, error)
}

// OutstandingBalance is the customer-AR snapshot — total cents owed
// across all unpaid (pending / failed / unknown) finalized invoices,
// excluding voided + uncollectible. UnpaidCount is the cardinality
// of those invoices so the UI can show "$X across N unpaid invoices."
type OutstandingBalance struct {
	TotalCents  int64 `json:"total_cents"`
	UnpaidCount int   `json:"unpaid_count"`
}

type ListFilter struct {
	TenantID       string
	CustomerID     string
	SubscriptionID string
	Status         string
	PaymentStatus  string
	// Search filters by invoice number, case-insensitive substring
	// (ILIKE, metacharacters escaped). Empty = no filter. Backs the
	// dashboard list search box and the command palette.
	Search string
	// CreatedFrom / CreatedTo bound created_at — inclusive on both
	// ends, matching the shared api/timefilter contract (?from / ?to).
	// Zero values = unbounded.
	CreatedFrom time.Time
	CreatedTo   time.Time
	// Overdue restricts to open invoices past their due date that
	// haven't settled and aren't mid-payment: status='finalized',
	// due_at < now(), payment_status not in (succeeded, processing).
	// Backs the dashboard's "Past due" tab (Stripe-parity segment).
	// NOTE: deliberately computed from due_at at query time — the
	// false (schema 0001 default); do not filter on it.
	Overdue bool
	// IDs scopes the result to a specific set of invoice IDs. Used by
	// other list pages (CreditNotes) to fetch exactly the invoices
	// referenced by their primary rows — avoids the
	// "list-then-client-side-join" pagination bug. Empty = no filter.
	IDs    []string
	Limit  int
	Offset int
	// Cursor-based pagination (2026-05-29). Seek-method query:
	// WHERE (created_at, id) < (AfterCreatedAt, AfterID). Mutually
	// exclusive with Offset — the handler routes by query param
	// (?after= takes precedence over ?offset=). Stable across the
	// cycle-close write burst that otherwise page-skews operator
	// pagination through "all invoices for last month."
	AfterCreatedAt time.Time
	AfterID        string
	// Sort: column name from a closed set (validated by the store).
	// Empty string means default (created_at).
	Sort string
	// SortDir: "asc" or "desc". Empty string means desc.
	SortDir string
}
