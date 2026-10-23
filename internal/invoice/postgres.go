package invoice

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/crypto"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

type PostgresStore struct {
	db                  *postgres.DB
	enc                 *crypto.Encryptor
	outbox              OutboxEnqueuer
	commitFunder        CommitFunder
	dunningPauseRelease DunningPauseReleaser
}

func NewPostgresStore(db *postgres.DB) *PostgresStore {
	return &PostgresStore{db: db, enc: crypto.NewNoop()}
}

// OutboxEnqueuer enqueues an outbound webhook event inside the caller's tx, so
// the event is persisted atomically with the state change (ADR-040 transactional
// outbox). Satisfied by *webhook.OutboxStore; declared as a narrow consumer-side
// interface so the invoice store needs no webhook import.
type OutboxEnqueuer interface {
	Enqueue(ctx context.Context, tx *sql.Tx, tenantID, eventType string, payload map[string]any) (string, error)
}

// SetOutboxEnqueuer wires transactional webhook emission for transitions that
// fire from many call sites (notably invoice.paid, emitted from MarkPaid so it
// fires exactly once regardless of which path settled the invoice). Optional —
// when unset, no event is enqueued.
func (s *PostgresStore) SetOutboxEnqueuer(o OutboxEnqueuer) { s.outbox = o }

// CommitFunder grants a prepaid-commit credit block, in the funding invoice's
// currency (ADR-129), on the caller's tx —
// FinalizeWithDates calls it inside the finalize coordinator tx so the status
// flip and the grant are both-or-neither (ADR-078 D2). Satisfied by
// *credit.Service; declared consumer-side so the invoice store needs no
// credit import. Unlike the outbox, this dependency is NOT optional when a
// commit line exists: FinalizeWithDates fails loud on a commit invoice with
// no funder wired.
type CommitFunder interface {
	GrantCommitForInvoiceTx(ctx context.Context, tx *sql.Tx, tenantID, customerID, invoiceID, invoiceNumber, currency string, amountCents int64, expiresAt *time.Time) (domain.CreditLedgerEntry, error)
}

// SetCommitFunder wires commit-grant funding at finalize (ADR-078).
func (s *PostgresStore) SetCommitFunder(f CommitFunder) { s.commitFunder = f }

// DunningPauseReleaser lifts a dunning collection pause in the transaction
// that marks the invoice it was set for paid (ADR-036 amendment
// 2026-10-22). Every paid flip goes through markPaidLockedTx, so every
// payment path (card, offline, credit note, credits) releases the pause the
// same way. LockDunningPauseTx runs first in each transaction that can reach
// that flip, before the invoice row is locked, so the release takes the
// subscription row before the invoice like every other writer of both.
// Satisfied by *subscription.PostgresStore; declared consumer-side so the
// invoice store needs no subscription import. The producer always wires it;
// unwired, a dunning pause stays until the operator resumes it.
type DunningPauseReleaser interface {
	LockDunningPauseTx(ctx context.Context, tx *sql.Tx, invoiceID string) error
	ReleaseDunningPauseTx(ctx context.Context, tx *sql.Tx, subID, paidInvoiceID string, at time.Time) error
}

// lockDunningPauseTx takes the subscription lock a release of invoiceID's
// dunning pause needs, before the caller locks the invoice row.
func (s *PostgresStore) lockDunningPauseTx(ctx context.Context, tx *sql.Tx, invoiceID string) error {
	if s.dunningPauseRelease == nil {
		return nil
	}
	return s.dunningPauseRelease.LockDunningPauseTx(ctx, tx, invoiceID)
}

// SetDunningPauseReleaser wires the dunning-pause release on payment.
func (s *PostgresStore) SetDunningPauseReleaser(r DunningPauseReleaser) { s.dunningPauseRelease = r }

// SetEncryptor wires AES-256-GCM encryption for the hosted-invoice public_token
// at rest. When set (non-noop), the raw token is encrypted before storage and
// decrypted on read so the hosted URL can be rebuilt on re-send; lookups go
// through the SHA-256 blind index (public_token_hash) which never needs the key.
// Without it, public_token_encrypted holds plaintext (dev/migration parity) —
// the blind index still hides the raw token from a snapshot of the hash column.
func (s *PostgresStore) SetEncryptor(enc *crypto.Encryptor) {
	if enc == nil {
		enc = crypto.NewNoop()
	}
	s.enc = enc
}

// decryptScanner is a sql.Scanner that decrypts the public_token_encrypted
// column into a plain string on read, so inv.PublicToken stays populated for
// the e-mail/checkout-URL paths without those callers knowing about encryption.
type decryptScanner struct {
	enc *crypto.Encryptor
	dst *string
}

// encodeToken returns the (encrypted, hash) pair to persist for a raw hosted-
// invoice token. Empty token → empty pair. Encryption failure (real key
// misconfigured) is non-fatal: the hash is still stored so lookups work; only
// the re-send URL is degraded until the token is rotated.
func (s *PostgresStore) encodeToken(rawToken string) (encrypted, hash string) {
	if rawToken == "" {
		return "", ""
	}
	hash = HashPublicToken(rawToken)
	if ct, err := s.enc.Encrypt(rawToken); err == nil {
		encrypted = ct
	}
	return encrypted, hash
}

func (d decryptScanner) Scan(src any) error {
	var ct string
	switch v := src.(type) {
	case nil:
		*d.dst = ""
		return nil
	case []byte:
		ct = string(v)
	case string:
		ct = v
	default:
		return fmt.Errorf("decryptScanner: unexpected source type %T", src)
	}
	if ct == "" {
		*d.dst = ""
		return nil
	}
	pt, err := d.enc.Decrypt(ct)
	if err != nil {
		return fmt.Errorf("decrypt public_token: %w", err)
	}
	*d.dst = pt
	return nil
}

// Unique-index names on the invoices table. Constants instead of inline
// string literals so the constraint-name router in mapInvoiceUniqueViolation
// is type-checked against actual index names — a typo here surfaces in
// integration tests, not as a silent fall-through to the generic
// "unknown constraint" branch.
const (
	idxInvoicesBillingIdempotency  = "idx_invoices_billing_idempotency"
	idxInvoicesProrationDedup      = "idx_invoices_proration_dedup"
	idxInvoicesInvoiceNumberUnique = "invoices_tenant_id_livemode_invoice_number_key"
	idxInvoicesPublicTokenUnique   = "idx_invoices_public_token_hash"
	idxInvoicesThresholdPerCycle   = "idx_invoices_threshold_unique_per_cycle"
	idxInvoicesStripeInvoiceID     = "idx_invoices_stripe_invoice_id"
)

// mapInvoiceUniqueViolation routes a Postgres unique-violation error to
// a structured DomainError tagged with the constraint that fired. Pre-
// 2026-05-28 every invoice unique violation was squashed into a single
// "billing_period" or "invoice_number" message — callers couldn't tell
// which constraint fired and the proration-retry path mis-routed
// billing-period collisions through GetByProrationSource (which then
// returned "not found" because the existing row had a different item
// ID). See ADR-030 cross-flow audit + feedback_no_silent_fallbacks
// memory.
//
// Returns nil if err isn't a unique violation; caller passes the
// original err through unchanged in that case.
func mapInvoiceUniqueViolation(err error, inv domain.Invoice) error {
	if !postgres.IsUniqueViolation(err) {
		return nil
	}
	switch postgres.UniqueViolationConstraint(err) {
	case idxInvoicesBillingIdempotency:
		// Name the period the way every other surface shows it: inclusive
		// last day ("Nov 1, 2026 – Nov 30, 2026"), not the stored exclusive
		// end. The conflicting row has the same subscription and period, and
		// the org zone is stamped on both (ADR-077), so the zone of the row
		// being written stands in for it. Empty (legacy) falls back to UTC.
		btz := domain.LoadLocationOrUTC(inv.BillingTimezone)
		period := domain.FormatInclusivePeriod(inv.BillingPeriodStart, inv.BillingPeriodEnd, btz)
		if period == "" {
			period = inv.BillingPeriodStart.In(btz).Format("Jan 2, 2006")
		}
		return errs.AlreadyExists("billing_period",
			fmt.Sprintf("invoice already exists for subscription %q period %s",
				inv.SubscriptionID, period)).WithCode("invoice_billing_period_taken")
	case idxInvoicesProrationDedup:
		return errs.AlreadyExists("proration_source",
			"proration invoice already exists for this item change").WithCode("invoice_proration_source_taken")
	case idxInvoicesInvoiceNumberUnique:
		return errs.AlreadyExists("invoice_number",
			fmt.Sprintf("invoice number %q already exists", inv.InvoiceNumber)).WithCode("invoice_number_taken")
	case idxInvoicesPublicTokenUnique:
		// 256-bit random token collision is astronomically unlikely; if
		// it ever fires, fail loudly rather than re-use the existing
		// row (would expose another invoice via the duplicate URL).
		return errs.AlreadyExists("public_token",
			"public token collision — regenerate").WithCode("invoice_public_token_collision")
	case idxInvoicesThresholdPerCycle:
		return errs.AlreadyExists("threshold_cycle",
			"threshold invoice already exists for this subscription cycle").WithCode("invoice_threshold_cycle_taken")
	case idxInvoicesStripeInvoiceID:
		return errs.AlreadyExists("stripe_invoice_id",
			"a Velox invoice is already linked to this Stripe invoice id").WithCode("invoice_stripe_id_taken")
	}
	// Unknown constraint — surface the constraint name in the message
	// so the operator + logs identify what fired, instead of squashing
	// into a generic "already exists" that the caller misroutes.
	return errs.AlreadyExists("",
		fmt.Sprintf("unique constraint %q violated on invoice insert",
			postgres.UniqueViolationConstraint(err))).WithCode("invoice_unique_violation")
}

const invCols = `id, tenant_id, customer_id, COALESCE(subscription_id,''), invoice_number, status,
	payment_status, currency, subtotal_cents, discount_cents, tax_amount_cents, tax_rate,
	COALESCE(tax_name,''), COALESCE(tax_country,''), COALESCE(tax_id,''),
	total_amount_cents, amount_due_cents, amount_paid_cents, credits_applied_cents,
	billing_period_start, billing_period_end, issued_at, due_at, paid_at, voided_at, uncollectible_at,
	COALESCE(stripe_payment_intent_id,''), COALESCE(last_payment_error,''),
	auto_charge_pending, net_payment_term_days, COALESCE(memo,''), COALESCE(footer,''),
	metadata, created_at, updated_at, source_plan_changed_at, COALESCE(source_subscription_item_id,''),
	COALESCE(source_change_type,''),
	tax_provider, tax_calculation_id, COALESCE(tax_transaction_id,''),
	tax_reverse_charge, tax_exempt_reason,
	tax_status, tax_deferred_at, tax_retry_count, tax_pending_reason,
	COALESCE(tax_error_code,''), tax_next_retry_at,
	COALESCE(payment_card_brand,''), COALESCE(payment_card_last4,''),
	COALESCE(public_token_encrypted,''), COALESCE(billing_reason,''), COALESCE(stripe_invoice_id,''),
	is_simulated, tax_reversed_at,
	COALESCE(payment_anomaly_kind,''), COALESCE(payment_anomaly_payment_intent_id,''), COALESCE(payment_anomaly_captured_cents,0),
	COALESCE(billing_timezone,''), no_pm_notified_at, charge_attempt_seq,
	COALESCE(provider_payment_status,''), provider_synced_at,
	bill_to_snapshot, supplier_snapshot, below_minimum_at, paid_out_of_band`

// invClockCol reads the test clock an invoice's dates follow from its
// customer (domain.Invoice.TestClockID). It follows invCols on the list and
// get reads only; scan it into inv.TestClockID after scanInvDest.
const invClockCol = `COALESCE((SELECT c.test_clock_id FROM customers c WHERE c.id = invoices.customer_id), '')`

// qualifiedInvCols returns invCols with every column reference prefixed
// by the given table alias. Used by ADR-029's per-clock queries that
// JOIN invoices to subscriptions — without qualification, columns like
// `id` and `tenant_id` are ambiguous (both tables have them) and
// Postgres rejects the query with SQLSTATE 42702.
//
// Mirrors qualifiedSubCols in internal/subscription/postgres.go;
// kept package-local to keep invCols a single source of truth.
func qualifiedInvCols(alias string) string {
	var b strings.Builder
	for i, col := range splitTopLevelCommas(invCols) {
		if i > 0 {
			b.WriteString(", ")
		}
		col = strings.TrimSpace(col)
		if strings.HasPrefix(col, "COALESCE(") {
			closing := strings.IndexByte(col, ')')
			inner := col[len("COALESCE("):closing]
			parts := strings.SplitN(inner, ",", 2)
			b.WriteString("COALESCE(")
			b.WriteString(alias)
			b.WriteByte('.')
			b.WriteString(strings.TrimSpace(parts[0]))
			if len(parts) == 2 {
				b.WriteString(",")
				b.WriteString(parts[1])
			}
			b.WriteString(col[closing:])
			continue
		}
		b.WriteString(alias)
		b.WriteByte('.')
		b.WriteString(col)
	}
	return b.String()
}

// splitTopLevelCommas splits a column list on commas that are NOT
// inside parentheses (so COALESCE(a, ”) stays as one column).
func splitTopLevelCommas(s string) []string {
	var out []string
	depth := 0
	start := 0
	for i, r := range s {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	out = append(out, s[start:])
	return out
}

func (s *PostgresStore) Create(ctx context.Context, tenantID string, inv domain.Invoice) (domain.Invoice, error) {
	return s.CreateAudited(ctx, tenantID, inv, nil)
}

// CreateAudited is Create with an in-tx audit emission (ADR-090): emit runs on
// the SAME transaction as the header INSERT, after the row exists (so it can
// reference the store-assigned id / invoice number) and BEFORE commit — the
// invoice and its audit row share one fate. An emission error aborts the
// create. nil emit = unaudited write (engine/cycle paths, whose canonical
// evidence is the finalize row).
func (s *PostgresStore) CreateAudited(
	ctx context.Context, tenantID string, inv domain.Invoice,
	emit func(tx *sql.Tx, out domain.Invoice) error,
) (domain.Invoice, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.Invoice{}, err
	}
	defer postgres.Rollback(tx)

	// Canonical UPPERCASE ISO-4217. This is the single chokepoint both
	// manual (Service.Create) and cycle (engine, incl. its "usd" fallback)
	// invoices funnel through, so the stored currency can't vary by origin.
	// Must match the tenant default ("USD") because analytics/dunning
	// revenue queries filter with a case-sensitive `currency = $1`.
	inv.Currency = strings.ToUpper(inv.Currency)

	id := postgres.NewID("vlx_inv")
	// Honor caller-provided CreatedAt — engine paths pass clk.Now()
	// so test-clock-driven invoices align with their issued_at /
	// due_at on simulation time. Zero falls back to wall-clock.
	now := inv.CreatedAt
	if now.IsZero() {
		now = clock.Now(ctx)
	}
	metaJSON, _ := json.Marshal(inv.Metadata)
	if inv.Metadata == nil {
		metaJSON = []byte("{}")
	}

	taxStatus := inv.TaxStatus
	if taxStatus == "" {
		taxStatus = domain.InvoiceTaxOK
	}
	err = tx.QueryRowContext(ctx, `
		INSERT INTO invoices (id, tenant_id, customer_id, subscription_id, invoice_number,
			status, payment_status, currency, subtotal_cents, discount_cents, tax_amount_cents, tax_name,
			tax_country, tax_id,
			total_amount_cents, amount_due_cents, amount_paid_cents, credits_applied_cents,
			billing_period_start, billing_period_end, issued_at, due_at,
			net_payment_term_days, memo, footer, metadata, created_at, updated_at,
			source_plan_changed_at, source_subscription_item_id, source_change_type,
			tax_provider, tax_calculation_id, tax_reverse_charge, tax_exempt_reason,
			tax_status, tax_deferred_at, tax_retry_count, tax_pending_reason, tax_error_code, billing_reason,
			stripe_invoice_id, is_simulated, billing_timezone, auto_charge_pending)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$27,$28,$29,$30,$31,$32,$33,$34,$35,$36,$37,$38,$39,$40,$41,$42,$43,$44)
		RETURNING `+invCols,
		id, tenantID, inv.CustomerID, postgres.NullableString(inv.SubscriptionID), inv.InvoiceNumber,
		inv.Status, inv.PaymentStatus, inv.Currency,
		inv.SubtotalCents, inv.DiscountCents, inv.TaxAmountCents, inv.TaxName,
		inv.TaxCountry, inv.TaxID,
		inv.TotalAmountCents, inv.AmountDueCents, inv.AmountPaidCents, inv.CreditsAppliedCents,
		inv.BillingPeriodStart, inv.BillingPeriodEnd,
		postgres.NullableTime(inv.IssuedAt), postgres.NullableTime(inv.DueAt),
		inv.NetPaymentTermDays, postgres.NullableString(inv.Memo),
		postgres.NullableString(inv.Footer), metaJSON, now,
		postgres.NullableTime(inv.SourcePlanChangedAt),
		postgres.NullableString(inv.SourceSubscriptionItemID),
		postgres.NullableString(string(inv.SourceChangeType)),
		inv.TaxProvider, inv.TaxCalculationID, inv.TaxReverseCharge, inv.TaxExemptReason,
		string(taxStatus), postgres.NullableTime(inv.TaxDeferredAt), inv.TaxRetryCount, inv.TaxPendingReason,
		postgres.NullableString(inv.TaxErrorCode),
		postgres.NullableString(string(inv.BillingReason)),
		postgres.NullableString(inv.StripeInvoiceID),
		inv.IsSimulated,
		postgres.NullableString(inv.BillingTimezone),
		collectionQueuedAtFinalize(inv.Status, inv.PaymentStatus),
	).Scan(s.scanInvDest(&inv)...)

	if err != nil {
		if mapped := mapInvoiceUniqueViolation(err, inv); mapped != nil {
			return domain.Invoice{}, mapped
		}
		return domain.Invoice{}, err
	}
	if inv.Status != domain.InvoiceDraft {
		if err := s.stampIssuedPartiesTx(ctx, tx, &inv); err != nil {
			return domain.Invoice{}, err
		}
	}
	// Emit only after the INSERT actually landed a row (the Scan above
	// succeeded) — never on a failed/duplicate create.
	if emit != nil {
		if err := emit(tx, inv); err != nil {
			return domain.Invoice{}, fmt.Errorf("audit emission: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return domain.Invoice{}, err
	}
	return inv, nil
}

// ActionRequiredObsolete reports whether an action-required email for this
// invoice is moot. Settled (payment succeeded, voided, uncollectible) or
// ABSENT (test-clock teardown hard-deletes invoices; the email must not
// deliver a dead link asking for payment) both count as obsolete.
func (s *PostgresStore) ActionRequiredObsolete(ctx context.Context, tenantID, invoiceNumber string) (bool, error) {
	state, err := s.InvoiceTerminalState(ctx, tenantID, invoiceNumber)
	if err != nil {
		return false, err
	}
	return state != "", nil
}

// InvoiceTerminalState reports the terminal state an action-required
// email's invoice has reached — "" when it is still live, otherwise
// "paid", "voided", "uncollectible", or "gone" (row deleted). The
// dispatcher needs the SPECIFIC state, not just "settled": a
// mark_uncollectible dunning outcome makes the invoice uncollectible in
// the same breath as it enqueues the escalation email that ANNOUNCES
// that outcome, so a blanket settled-check muted the one email whose
// purpose is that state (found on the FLOW I6 walk, VLX-000016).
func (s *PostgresStore) InvoiceTerminalState(ctx context.Context, tenantID, invoiceNumber string) (string, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return "", err
	}
	defer postgres.Rollback(tx)
	var status, paymentStatus string
	err = tx.QueryRowContext(ctx,
		`SELECT status, payment_status FROM invoices WHERE invoice_number = $1`,
		invoiceNumber,
	).Scan(&status, &paymentStatus)
	if err == sql.ErrNoRows {
		return "gone", nil
	}
	if err != nil {
		return "", err
	}
	switch {
	case paymentStatus == string(domain.PaymentSucceeded) || status == string(domain.InvoicePaid):
		return string(domain.InvoicePaid), nil
	case status == string(domain.InvoiceVoided):
		return string(domain.InvoiceVoided), nil
	case status == string(domain.InvoiceUncollectible):
		return string(domain.InvoiceUncollectible), nil
	}
	return "", nil
}

func (s *PostgresStore) Get(ctx context.Context, tenantID, id string) (domain.Invoice, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.Invoice{}, err
	}
	defer postgres.Rollback(tx)

	var inv domain.Invoice
	err = tx.QueryRowContext(ctx, `SELECT `+invCols+`, `+invClockCol+` FROM invoices WHERE id = $1`, id).
		Scan(append(s.scanInvDest(&inv), &inv.TestClockID)...)
	if err == sql.ErrNoRows {
		return domain.Invoice{}, errs.ErrNotFound
	}
	return inv, err
}

// GetByProrationSource returns the invoice previously generated for a
// specific (subscription, item, change_type, change_at) event, if any.
// Callers use this after CreateWithLineItems returns ErrAlreadyExists to
// complete an idempotent retry — the proration dedup index ensures
// uniqueness. change_type disambiguates plan-vs-quantity-vs-add-vs-remove
// mutations that coincidentally share a wall-clock timestamp; the item id
// keeps cross-item changes in the same transaction distinct.
func (s *PostgresStore) GetByProrationSource(ctx context.Context, tenantID, subscriptionID, subscriptionItemID string, changeType domain.ItemChangeType, changeAt time.Time) (domain.Invoice, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.Invoice{}, err
	}
	defer postgres.Rollback(tx)

	var inv domain.Invoice
	err = tx.QueryRowContext(ctx, `SELECT `+invCols+`
		FROM invoices
		WHERE tenant_id = $1 AND subscription_id = $2 AND source_subscription_item_id = $3 AND source_change_type = $4 AND source_plan_changed_at = $5`,
		tenantID, subscriptionID, subscriptionItemID, string(changeType), changeAt,
	).Scan(s.scanInvDest(&inv)...)
	if err == sql.ErrNoRows {
		return domain.Invoice{}, errs.ErrNotFound
	}
	return inv, err
}

func (s *PostgresStore) GetByNumber(ctx context.Context, tenantID, number string) (domain.Invoice, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.Invoice{}, err
	}
	defer postgres.Rollback(tx)

	var inv domain.Invoice
	err = tx.QueryRowContext(ctx, `SELECT `+invCols+` FROM invoices WHERE invoice_number = $1`, number).
		Scan(s.scanInvDest(&inv)...)
	if err == sql.ErrNoRows {
		return domain.Invoice{}, errs.ErrNotFound
	}
	return inv, err
}

func (s *PostgresStore) List(ctx context.Context, filter ListFilter) ([]domain.Invoice, int, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, filter.TenantID)
	if err != nil {
		return nil, 0, err
	}
	defer postgres.Rollback(tx)

	// Limit: default to 50 when unset/invalid; clamp to 100 when caller
	// asks for more. Pre-2026-05-28 the over-cap branch silently fell
	// back to 50 — surprising for any caller asking for >100 (e.g. the
	// CSV export's exportPageSize=1000 only ever got 50 back, then the
	// pagination loop broke early on len(rows)<requested). Explicit
	// clamp preserves the runaway-query protection without lying about
	// what was returned. See ADR-030 / feedback_no_silent_fallbacks.
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	} else if limit > 100 {
		limit = 100
	}

	where, args := buildInvWhere(filter)
	useCursor := !filter.AfterCreatedAt.IsZero() && filter.AfterID != ""

	if useCursor {
		// Cursor predicate honors the primary sort direction. Only
		// the default `created_at DESC` cursor is supported today —
		// callers asking for a custom Sort+After combination would
		// hit an inconsistent seek-vs-order pairing; the handler
		// rejects those upstream. The (created_at, id) tuple matches
		// invoiceOrderBy's tie-break, so seek + ORDER BY are aligned.
		args = append(args, filter.AfterCreatedAt, filter.AfterID)
		clause := fmt.Sprintf(`(created_at, id) < ($%d, $%d)`, len(args)-1, len(args))
		if where == "" {
			where = " WHERE " + clause
		} else {
			where += " AND " + clause
		}
	}

	// Cursor path skips COUNT — handler derives hasMore from the
	// limit+1 over-fetch. Offset path still needs total for "Page
	// X of N" UX in the legacy dashboard.
	var total int
	if !useCursor {
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM invoices`+where, args...).Scan(&total); err != nil {
			return nil, 0, err
		}
	}

	// Sort with deterministic tie-break on id. Without the tie-break,
	// catchup-generated invoices share microsecond-level created_at
	// values and Postgres returns ties in arbitrary order — looks
	// "random" to operators. Tie-break direction matches the primary
	// sort direction so consecutive ties read as a single ordered
	// group rather than zig-zagging.
	orderBy := invoiceOrderBy(filter.Sort, filter.SortDir)
	queryLimit := limit
	if useCursor {
		queryLimit = limit + 1
	}
	args = append(args, queryLimit)
	query := `SELECT ` + invCols + `, ` + invClockCol + ` FROM invoices` + where +
		` ORDER BY ` + orderBy +
		` LIMIT $` + fmt.Sprintf("%d", len(args))
	if !useCursor {
		args = append(args, filter.Offset)
		query += ` OFFSET $` + fmt.Sprintf("%d", len(args))
	}

	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()

	var invoices []domain.Invoice
	for rows.Next() {
		var inv domain.Invoice
		if err := rows.Scan(append(s.scanInvDest(&inv), &inv.TestClockID)...); err != nil {
			return nil, 0, err
		}
		invoices = append(invoices, inv)
	}
	return invoices, total, rows.Err()
}

func (s *PostgresStore) UpdateStatus(ctx context.Context, tenantID, id string, status domain.InvoiceStatus) (domain.Invoice, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.Invoice{}, err
	}
	defer postgres.Rollback(tx)

	inv, err := s.updateStatusInTx(ctx, tx, id, status)
	if err != nil {
		return domain.Invoice{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.Invoice{}, err
	}
	return inv, nil
}

// UpdateStatusWithReversal flips the invoice status AND runs a caller-supplied
// in-tx side effect (the consumed-credit reversal on void) in ONE transaction.
// Either both commit or neither does: a reversal failure rolls the status flip
// back, so the invoice never lands voided-but-credits-still-consumed (which
// would silently strip the customer of credits they paid the invoice with).
// reverseFn receives the same *sql.Tx and must do all its writes on it.
// Mirrors subscription.CreateWithBill — the store owns the tx, the cross-domain
// effect is a callback so no peer-package import leaks in.
func (s *PostgresStore) UpdateStatusWithReversal(ctx context.Context, tenantID, id string, status domain.InvoiceStatus, reverseFn func(tx *sql.Tx) error) (domain.Invoice, error) {
	var fn func(tx *sql.Tx, locked domain.Invoice) error
	if reverseFn != nil {
		fn = func(tx *sql.Tx, _ domain.Invoice) error { return reverseFn(tx) }
	}
	return s.UpdateStatusWithReversalPrior(ctx, tenantID, id, status, fn)
}

// UpdateStatusWithReversalPrior is UpdateStatusWithReversal handing the
// closure the invoice as it is UNDER THE ROW LOCK, read FOR UPDATE on this
// same tx before the flip. The service's earlier snapshot races concurrent
// transitions: an audit row stamping status_before from it could permanently
// record the wrong provenance (ADR-090 PR4 review), and a refusal decided on
// it misses a charge that started after the read (#975). A closure error
// rolls the flip back.
func (s *PostgresStore) UpdateStatusWithReversalPrior(ctx context.Context, tenantID, id string, status domain.InvoiceStatus, fn func(tx *sql.Tx, locked domain.Invoice) error) (domain.Invoice, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.Invoice{}, err
	}
	defer postgres.Rollback(tx)

	inv, err := s.UpdateStatusWithReversalPriorTx(ctx, tx, tenantID, id, status, fn)
	if err != nil {
		return domain.Invoice{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.Invoice{}, err
	}
	return inv, nil
}

// UpdateStatusWithReversalPriorTx is the flip on the CALLER's tx: it locks the
// row, hands the closure the invoice as it is UNDER THE LOCK (so a caller can
// re-check what its earlier snapshot assumed), then flips the status and runs
// the closure's writes on the same tx. The billing engine voids a fully-unused
// unpaid prebill this way inside the cancel / plan-swap transaction, so the
// relief commits — or rolls back — with the change that caused it.
func (s *PostgresStore) UpdateStatusWithReversalPriorTx(ctx context.Context, tx *sql.Tx, tenantID, id string, status domain.InvoiceStatus, fn func(tx *sql.Tx, prior domain.Invoice) error) (domain.Invoice, error) {
	// Row lock + the true prior state; same row the CAS below updates, so no
	// new lock ordering edge.
	var prior domain.Invoice
	if err := tx.QueryRowContext(ctx,
		`SELECT `+invCols+` FROM invoices WHERE id = $1 AND tenant_id = $2 FOR UPDATE`,
		id, tenantID).Scan(s.scanInvDest(&prior)...); err != nil {
		if err == sql.ErrNoRows {
			return domain.Invoice{}, errs.ErrNotFound
		}
		return domain.Invoice{}, fmt.Errorf("lock invoice for status flip: %w", err)
	}
	inv, err := s.updateStatusInTx(ctx, tx, id, status)
	if err != nil {
		return domain.Invoice{}, err
	}
	// The closure's error aborts the caller's tx, flip included.
	if fn != nil {
		if err := fn(tx, prior); err != nil {
			return domain.Invoice{}, err
		}
	}
	return inv, nil
}

// updateStatusInTx is the shared body: the status flip without owning the tx.
// UpdateStatus opens+commits its own; UpdateStatusWithReversal threads the
// credit reversal through the same one.
//
// The UPDATE carries an in-SQL allowed-source CAS (ADR-078 D5). The service
// guards (Void's paid/in-flight checks, Finalize's draft check,
// MarkUncollectible's switch) read an EARLIER snapshot on a different tx —
// without the predicate, a pay-vs-void race flips paid→voided and retires
// credits the customer just paid for, and a void-vs-finalize race resurrects
// an annulled invoice. The CAS covers status only. Payment state and the
// charge hold are checked by Void and MarkUncollectible on the row their
// locking variant reads (#975), not here: this body also serves
// draft→finalized, and a parked invoice must stay writable-off. Allowed sources per target (matching the service-layer
// state machine): finalized←draft; voided←draft/finalized/uncollectible
// (never paid — "issue a credit note instead"); uncollectible←finalized.
// 0 rows → re-read → typed conflict with the actual current status.
func (s *PostgresStore) updateStatusInTx(ctx context.Context, tx *sql.Tx, id string, status domain.InvoiceStatus) (domain.Invoice, error) {
	now := clock.Now(ctx)
	var voidedAt, uncollectibleAt *time.Time
	if status == domain.InvoiceVoided {
		voidedAt = &now
	}
	if status == domain.InvoiceUncollectible {
		uncollectibleAt = &now
	}

	var allowedSources []string
	switch status {
	case domain.InvoiceFinalized:
		allowedSources = []string{string(domain.InvoiceDraft)}
	case domain.InvoiceVoided:
		allowedSources = []string{string(domain.InvoiceDraft), string(domain.InvoiceFinalized), string(domain.InvoiceUncollectible)}
	case domain.InvoiceUncollectible:
		allowedSources = []string{string(domain.InvoiceFinalized)}
	default:
		// No current caller flips to any other status via this path (paid
		// is owned by markPaidReportingTransition). Fail loud rather than
		// allow an unguarded transition to slip in later.
		return domain.Invoice{}, fmt.Errorf("updateStatusInTx: unsupported target status %q", status)
	}

	var inv domain.Invoice
	// The auto-charge flag moves with the status in this same write.
	// Finalize (draft→finalized) of an unpaid invoice queues it for
	// collection (collectionQueuedAtFinalize). Void and uncollectible
	// retire it: there is nothing left to collect. Settle clears it at the
	// markPaidReportingTransition choke point the same way.
	err := tx.QueryRowContext(ctx, `
		UPDATE invoices SET status = $1, voided_at = $2,
			uncollectible_at = COALESCE($3, uncollectible_at), updated_at = $4,
			auto_charge_pending = CASE
				WHEN $1 IN ('voided', 'uncollectible') THEN FALSE
				WHEN $1 = 'finalized' AND payment_status = 'pending' THEN TRUE
				ELSE auto_charge_pending END
		WHERE id = $5 AND status = ANY($6)
		RETURNING `+invCols,
		status, postgres.NullableTime(voidedAt), postgres.NullableTime(uncollectibleAt), now, id,
		postgres.StringArray(allowedSources),
	).Scan(s.scanInvDest(&inv)...)

	if err == sql.ErrNoRows {
		var cur string
		rerr := tx.QueryRowContext(ctx, `SELECT status FROM invoices WHERE id = $1`, id).Scan(&cur)
		if rerr == sql.ErrNoRows {
			return domain.Invoice{}, errs.ErrNotFound
		}
		if rerr != nil {
			return domain.Invoice{}, rerr
		}
		return domain.Invoice{}, errs.InvalidState(fmt.Sprintf(
			"cannot transition invoice to %s from %s", status, cur))
	}
	if err != nil {
		return domain.Invoice{}, err
	}
	if status == domain.InvoiceFinalized {
		if err := s.stampIssuedPartiesTx(ctx, tx, &inv); err != nil {
			return domain.Invoice{}, err
		}
	}
	// Void / uncollectible exit the payable state: close open checkout claims
	// in the SAME tx (ADR-068 choke-point rule — see markPaidReportingTransition).
	if status == domain.InvoiceVoided || status == domain.InvoiceUncollectible {
		if _, err := tx.ExecContext(ctx, `
			UPDATE checkout_sessions SET status = 'invoice_settled', updated_at = now()
			WHERE invoice_id = $1 AND status = 'open'
		`, id); err != nil {
			return domain.Invoice{}, fmt.Errorf("close checkout claims on %s: %w", status, err)
		}
	}
	return inv, nil
}

// FinalizeWithDates flips status to finalized and re-stamps issued_at + due_at
// to the finalize moment in one UPDATE — for operator-composed invoices whose
// draft may have been created on an earlier (test-clock) instant. Cycle
// invoices use UpdateStatus and keep their build-time dates.
//
// The UPDATE carries an `AND status='draft'` CAS (ADR-078 D5): the service's
// draft guard reads an earlier snapshot, so without the predicate a concurrent
// void/finalize could flip voided→finalized — resurrecting an annulled invoice
// and, below, minting its commit grant. 0 rows → typed conflict, no grant.
//
// Commit funding (ADR-078 D2): when the invoice carries a commit line, the
// injected commitFunder grants the credit block IN THIS TX — the status flip
// and the grant commit or roll back together. The commit line is read in-tx,
// AFTER the flip (the row UPDATE serializes against AddLineItemAtomic's FOR
// UPDATE, so a line added concurrently with finalize is either seen here or
// rejected by its own draft re-check). A funder error fails finalize by
// design: operator-synchronous, loud, retryable — never weaken to
// post-commit.
func (s *PostgresStore) FinalizeWithDates(ctx context.Context, tenantID, id string, issuedAt, dueAt time.Time) (domain.Invoice, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.Invoice{}, err
	}
	defer postgres.Rollback(tx)

	now := clock.Now(ctx)
	var inv domain.Invoice
	err = tx.QueryRowContext(ctx, `
		UPDATE invoices SET status = $1, issued_at = $2, due_at = $3, updated_at = $4,
			auto_charge_pending = (payment_status = 'pending')
		WHERE id = $5 AND status = 'draft'
		RETURNING `+invCols,
		domain.InvoiceFinalized, issuedAt, dueAt, now, id,
	).Scan(s.scanInvDest(&inv)...)

	if err == sql.ErrNoRows {
		// Missing row or CAS miss — re-read to tell the two apart and to
		// surface the actual state in the conflict error.
		var cur string
		rerr := tx.QueryRowContext(ctx, `SELECT status FROM invoices WHERE id = $1`, id).Scan(&cur)
		if rerr == sql.ErrNoRows {
			return domain.Invoice{}, errs.ErrNotFound
		}
		if rerr != nil {
			return domain.Invoice{}, rerr
		}
		return domain.Invoice{}, errs.InvalidState(fmt.Sprintf(
			"can only finalize draft invoices, current status: %s", cur))
	}
	if err != nil {
		return domain.Invoice{}, err
	}
	if err := s.stampIssuedPartiesTx(ctx, tx, &inv); err != nil {
		return domain.Invoice{}, err
	}

	// Fund the commit line, if any (at most one — enforced at line create).
	var (
		commitCents   sql.NullInt64
		commitExpires sql.NullTime
	)
	err = tx.QueryRowContext(ctx, `
		SELECT commit_granted_cents, commit_expires_at
		FROM invoice_line_items
		WHERE invoice_id = $1 AND tenant_id = $2 AND commit_granted_cents IS NOT NULL
	`, id, tenantID).Scan(&commitCents, &commitExpires)
	if err != nil && err != sql.ErrNoRows {
		return domain.Invoice{}, fmt.Errorf("read commit line at finalize: %w", err)
	}
	if err == nil && commitCents.Valid {
		if s.commitFunder == nil {
			// A commit line with no funder wired would finalize a purchase
			// that never grants — fail loud, never silently skip.
			return domain.Invoice{}, fmt.Errorf("invoice %s carries a commit line but no commit funder is wired", id)
		}
		var expiresAt *time.Time
		if commitExpires.Valid {
			expiresAt = &commitExpires.Time
		}
		if _, ferr := s.commitFunder.GrantCommitForInvoiceTx(ctx, tx, tenantID,
			inv.CustomerID, inv.ID, inv.InvoiceNumber, inv.Currency, commitCents.Int64, expiresAt); ferr != nil {
			return domain.Invoice{}, fmt.Errorf("fund commit grant at finalize: %w", ferr)
		}
	}

	if err := tx.Commit(); err != nil {
		return domain.Invoice{}, err
	}
	return inv, nil
}

// StampChargeOutcome records a charge attempt's outcome (processing, unknown,
// or a failure with no PaymentIntent) on the invoice. A failure that names a
// PaymentIntent goes through MarkPaymentFailedReportingTransition, which
// applies the same seq CAS for the charge call (ChargedAtSeq). The write lands AFTER a Stripe round-trip, and
// another outcome can be recorded during it: the PaymentIntent's own webhook
// (paid or declined), an offline payment, a hosted Checkout payment, a credit
// cover. A late write over any of those undid it (P13) — a paid invoice lost
// paid_at and its PaymentIntent, so a second capture read as "already
// settled" instead of payment.duplicate_charge; a recorded decline turned
// back into "unknown", and with no PaymentIntent id that parked the invoice
// for good.
//
// It clears below_minimum_at: this answer from Stripe replaces an earlier
// below-minimum one (StampBelowMinimum).
//
// attemptSeq is the charge_attempt_seq the attempt read — the value its
// Stripe idempotency key was built from. Every writer that records an outcome
// bumps the column in the same statement (TestChargeAttemptSeqBumpedByEveryPIStamp),
// and status='paid' is only ever set by one of them, so "seq unchanged" means
// exactly "nothing was recorded since this attempt began". That one CAS covers
// a named and an unnamed attempt alike, which no comparison of state or
// PaymentIntent id can: an unknown with no id cannot tell its own decline
// from someone else's.
//
// Writes that do not record an outcome leave the seq alone and so do not
// block the stamp: a void or write-off that lands mid-charge still gets it,
// and on those rows it is the reconciler's only handle on a PaymentIntent that
// may have captured. Since #975 both refuse while the charge hold is live, so
// they land mid-charge only once the hold has lapsed under a stalled charger.
//
// The CAS is in the UPDATE's own WHERE, so under READ COMMITTED it is
// re-checked against a concurrent settle's committed row after waiting on its
// lock. A refused stamp re-reads in the same transaction and returns the row
// as it now stands with applied=false and a nil error.
func (s *PostgresStore) StampChargeOutcome(ctx context.Context, tenantID, id string, attemptSeq int64, paymentStatus domain.InvoicePaymentStatus, stripePaymentIntentID, lastPaymentError string) (domain.Invoice, bool, error) {
	switch paymentStatus {
	case domain.PaymentProcessing, domain.PaymentUnknown:
	case domain.PaymentFailed:
		// A failure that names a PaymentIntent is a report the webhook and
		// reconciler may repeat; it goes through
		// MarkPaymentFailedReportingTransition so the invoice is moved once
		// and notified once between them. Only a failure with nothing to
		// match (the create itself failed) is stamped here.
		if stripePaymentIntentID != "" {
			return domain.Invoice{}, false, fmt.Errorf("stamp charge outcome: a failure naming a PaymentIntent goes through MarkPaymentFailedReportingTransition")
		}
	default:
		// succeeded goes through MarkPaid*, which also flips status and
		// amounts and emits the paid events.
		return domain.Invoice{}, false, fmt.Errorf("stamp charge outcome: %q is not a charge outcome", paymentStatus)
	}
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.Invoice{}, false, err
	}
	defer postgres.Rollback(tx)

	now := clock.Now(ctx)
	var inv domain.Invoice
	applied := true
	err = tx.QueryRowContext(ctx, `
		UPDATE invoices SET payment_status = $1, stripe_payment_intent_id = $2,
			last_payment_error = $3, updated_at = $4, below_minimum_at = NULL,
			charge_attempt_seq = charge_attempt_seq + 1
		WHERE id = $5 AND charge_attempt_seq = $6
		RETURNING `+invCols,
		paymentStatus, postgres.NullableString(stripePaymentIntentID),
		postgres.NullableString(lastPaymentError), now, id, attemptSeq,
	).Scan(s.scanInvDest(&inv)...)

	if err == sql.ErrNoRows {
		// Refused, or no such invoice: the re-read tells which.
		applied = false
		if err := tx.QueryRowContext(ctx, `SELECT `+invCols+` FROM invoices WHERE id = $1`, id).
			Scan(s.scanInvDest(&inv)...); err != nil {
			if err == sql.ErrNoRows {
				return domain.Invoice{}, false, errs.ErrNotFound
			}
			return domain.Invoice{}, false, fmt.Errorf("re-read invoice after refused charge-outcome stamp: %w", err)
		}
	} else if err != nil {
		return domain.Invoice{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return domain.Invoice{}, false, err
	}
	return inv, applied, nil
}

// StampBelowMinimum records a charge Stripe refused because the invoice's
// amount is below Stripe's minimum charge (domain.ErrBelowMinimumCharge). It
// is not a failed payment, so payment_status goes to 'pending' (from
// 'pending', or from 'failed' when an earlier decline left it there), and in
// the same write:
//   - auto_charge_pending is cleared, so the auto-charge sweep does not ask
//     Stripe again every tick;
//   - below_minimum_at is stamped, which the attention notice, the hosted
//     page and dunning read (domain.Invoice.BelowStripeMinimum); the next
//     charge outcome Stripe gives clears it (StampChargeOutcome, and
//     MarkPaymentFailedReportingTransition when it moves the invoice);
//   - last_payment_error keeps Stripe's sentence, which names the minimum;
//   - stripe_payment_intent_id is cleared and charge_attempt_seq bumped, as
//     for any attempt outcome with no PaymentIntent (StampChargeOutcome): the
//     next charge gets a fresh idempotency key.
//
// attemptSeq is the seq CAS of StampChargeOutcome (P13): the write is refused,
// applied=false with the row as it stands, when another outcome was recorded
// during the call. Nothing moves an invoice that is no longer finalized.
func (s *PostgresStore) StampBelowMinimum(ctx context.Context, tenantID, id string, attemptSeq int64, lastPaymentError string) (domain.Invoice, bool, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.Invoice{}, false, err
	}
	defer postgres.Rollback(tx)

	now := clock.Now(ctx)
	var inv domain.Invoice
	applied := true
	err = tx.QueryRowContext(ctx, `
		UPDATE invoices SET payment_status = 'pending', stripe_payment_intent_id = NULL,
			last_payment_error = $1, auto_charge_pending = FALSE, below_minimum_at = $2, updated_at = $2,
			charge_attempt_seq = charge_attempt_seq + 1
		WHERE id = $3 AND charge_attempt_seq = $4 AND status = 'finalized'
		RETURNING `+invCols,
		postgres.NullableString(lastPaymentError), now, id, attemptSeq,
	).Scan(s.scanInvDest(&inv)...)
	if err == sql.ErrNoRows {
		applied = false
		if err := tx.QueryRowContext(ctx, `SELECT `+invCols+` FROM invoices WHERE id = $1`, id).
			Scan(s.scanInvDest(&inv)...); err != nil {
			if err == sql.ErrNoRows {
				return domain.Invoice{}, false, errs.ErrNotFound
			}
			return domain.Invoice{}, false, fmt.Errorf("re-read invoice after refused below-minimum stamp: %w", err)
		}
	} else if err != nil {
		return domain.Invoice{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return domain.Invoice{}, false, err
	}
	return inv, applied, nil
}

// MarkPaymentFailedReportingTransition records one report that a
// PaymentIntent failed, and says whether it moved the invoice and whether it
// was the first notice for that PaymentIntent. It is the one writer of a
// failure that carries a PaymentIntent: the charge call's own decline, the
// payment_intent.payment_failed webhook and the reconciler all report here.
// (A decline with no PaymentIntent is stamped by StampChargeOutcome — there
// is nothing to notify about and nothing for a webhook to match.)
//
// The invoice records one attempt at a time: the one it is waiting on. A
// report moves the invoice's payment fields only when it is about that
// attempt — domain.PaymentFailureReport.MovesInvoice, evaluated here under
// the row lock. Before, a webhook for any PaymentIntent of the invoice moved
// it: a late decline for an older attempt relinked the stale PaymentIntent,
// hid an in-flight one from the reconciler, and bumped the seq out from
// under a charge in flight.
//
// Every report lands on its own attempt row (ADR-102), in this transaction,
// so a failure that does not move the invoice still shows on the timeline.
//
// Notification is keyed on the PaymentIntent, not the status: failure is
// not terminal (each dunning retry is a new PaymentIntent and a real event),
// and redeliveries plus the charge response report the same PaymentIntent
// more than once. failure_notified_pi holds the last one notified;
// FirstNotice is true for the first report that records a PaymentIntent the
// marker does not hold. payment.failed is enqueued in this transaction on
// FirstNotice; then (nil = none) runs on FirstNotice too, under a SAVEPOINT
// (ADR-040 amendment — see the paid twin).
//
// The seq moves only when a new outcome is recorded. Re-recording the
// PaymentIntent the invoice already holds as failed touches only the marker:
// a charge already in flight for the next attempt reads the seq as its CAS,
// and a bump here would refuse that charge's outcome.
//
// A paid invoice is never touched: a failure arriving after a success would
// otherwise null paid_at, relink a stale PaymentIntent and dun a paid invoice.
func (s *PostgresStore) MarkPaymentFailedReportingTransition(ctx context.Context, tenantID, id string, report domain.PaymentFailureReport, then func(tx *sql.Tx, fresh domain.Invoice) error) (domain.Invoice, domain.PaymentFailureResult, error) {
	if report.PaymentIntentID == "" {
		return domain.Invoice{}, domain.PaymentFailureResult{}, fmt.Errorf("mark payment failed: a failure report needs a PaymentIntent id")
	}
	pi := report.PaymentIntentID

	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.Invoice{}, domain.PaymentFailureResult{}, err
	}
	defer postgres.Rollback(tx)

	var status, paymentStatus string
	var notifiedPI, rowPI sql.NullString
	var seq, amountDue int64
	if err := tx.QueryRowContext(ctx,
		`SELECT status, payment_status, failure_notified_pi, stripe_payment_intent_id, charge_attempt_seq, amount_due_cents
		 FROM invoices WHERE id = $1 FOR UPDATE`, id,
	).Scan(&status, &paymentStatus, &notifiedPI, &rowPI, &seq, &amountDue); err != nil {
		if err == sql.ErrNoRows {
			return domain.Invoice{}, domain.PaymentFailureResult{}, errs.ErrNotFound
		}
		return domain.Invoice{}, domain.PaymentFailureResult{}, fmt.Errorf("load invoice for mark-failed: %w", err)
	}

	holdsThisPI := rowPI.Valid && rowPI.String == pi
	standing := domain.AttemptNotVelox
	if !holdsThisPI && report.ChargedAtSeq == nil && !report.External {
		if standing, err = veloxAttemptStandingTx(ctx, tx, id, pi); err != nil {
			return domain.Invoice{}, domain.PaymentFailureResult{}, err
		}
	}
	if !report.MovesInvoice(domain.InvoiceStatus(status), domain.InvoicePaymentStatus(paymentStatus), rowPI.String, seq, standing) {
		// Not the attempt the invoice waits on: its own attempt row is the
		// only write. A paid invoice's attempt row still records the failure —
		// the attempt did fail.
		if err := upsertChargeAttemptTx(ctx, tx, tenantID, id, pi,
			domain.ChargeAttemptFailed, report.Message, amountDue, report.DeclineCode, report.PaymentMethodID, simAtOf(ctx)); err != nil {
			return domain.Invoice{}, domain.PaymentFailureResult{}, err
		}
		var inv domain.Invoice
		if err := tx.QueryRowContext(ctx,
			`SELECT `+invCols+` FROM invoices WHERE id = $1`, id,
		).Scan(s.scanInvDest(&inv)...); err != nil {
			return domain.Invoice{}, domain.PaymentFailureResult{}, fmt.Errorf("reload invoice after unrecorded failure: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return domain.Invoice{}, domain.PaymentFailureResult{}, err
		}
		return inv, domain.PaymentFailureResult{}, nil
	}

	firstNotice := !notifiedPI.Valid || notifiedPI.String != pi
	alreadyFailed := holdsThisPI && domain.InvoicePaymentStatus(paymentStatus) == domain.PaymentFailed

	now := clock.Now(ctx)
	var inv domain.Invoice
	if alreadyFailed {
		err = tx.QueryRowContext(ctx, `
			UPDATE invoices SET
				last_payment_error = $1,
				failure_notified_pi = $2,
				updated_at = $3
			WHERE id = $4
			RETURNING `+invCols,
			postgres.NullableString(report.Message), pi, now, id,
		).Scan(s.scanInvDest(&inv)...)
	} else {
		err = tx.QueryRowContext(ctx, `
			UPDATE invoices SET
				payment_status = 'failed',
				stripe_payment_intent_id = $1,
				last_payment_error = $2,
				failure_notified_pi = $1,
				below_minimum_at = NULL,
				charge_attempt_seq = charge_attempt_seq + 1,
				updated_at = $3
			WHERE id = $4
			RETURNING `+invCols,
			pi, postgres.NullableString(report.Message), now, id,
		).Scan(s.scanInvDest(&inv)...)
	}
	if err != nil {
		if err == sql.ErrNoRows {
			return domain.Invoice{}, domain.PaymentFailureResult{}, errs.ErrNotFound
		}
		return domain.Invoice{}, domain.PaymentFailureResult{}, err
	}
	// payment.failed — enqueued in the SAME tx as the failed-stamp, so the event
	// is crash-safe with the transition instead of fire-and-forget post-commit
	// (the mirror of payment.succeeded in markPaidReportingTransition; ADR-040
	// transactional outbox). Gated on firstNotice: a second report of the same
	// PaymentIntent must not double-notify, while a NEW retry PaymentIntent is
	// a genuinely distinct failure and fires again.
	if firstNotice && s.outbox != nil {
		// The invoice row this transaction wrote holds the charge
		// (stripe_payment_intent_id) and its failure (last_payment_error).
		if _, err := s.outbox.Enqueue(ctx, tx, tenantID, domain.EventPaymentFailed, domain.InvoiceWebhookData(inv, nil)); err != nil {
			return domain.Invoice{}, domain.PaymentFailureResult{}, fmt.Errorf("enqueue payment.failed: %w", err)
		}
	}
	// ADR-103: the attempt's outcome lands in THIS tx, so the timeline's
	// single payment owner can never disagree with the invoice's state.
	if err := upsertChargeAttemptTx(ctx, tx, tenantID, inv.ID, pi,
		domain.ChargeAttemptFailed, report.Message, inv.AmountDueCents, report.DeclineCode, report.PaymentMethodID, simAtOf(ctx)); err != nil {
		return domain.Invoice{}, domain.PaymentFailureResult{}, err
	}
	// The money email rides THIS transaction (ADR-040 amendment): enqueued
	// beside the events above, gated on the same transition, inside a
	// SAVEPOINT so an email-side failure is logged and skipped while the
	// settlement still commits. Never the reverse — a receipt must not be
	// able to un-settle a payment.
	if then != nil && firstNotice {
		if _, err := tx.ExecContext(ctx, "SAVEPOINT settle_email"); err != nil {
			return domain.Invoice{}, domain.PaymentFailureResult{}, err
		}
		if herr := then(tx, inv); herr != nil {
			if _, rerr := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT settle_email"); rerr != nil {
				return domain.Invoice{}, domain.PaymentFailureResult{}, rerr
			}
			slog.ErrorContext(ctx, "settlement email enqueue failed inside the settle transaction — settlement committed, email skipped",
				"invoice_id", id, "tenant_id", tenantID, "error", herr)
		} else if _, err := tx.ExecContext(ctx, "RELEASE SAVEPOINT settle_email"); err != nil {
			return domain.Invoice{}, domain.PaymentFailureResult{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return domain.Invoice{}, domain.PaymentFailureResult{}, err
	}
	return inv, domain.PaymentFailureResult{Recorded: true, FirstNotice: firstNotice}, nil
}

// veloxAttemptStandingTx says where PaymentIntent pi stands among the
// attempts Velox itself made on the invoice (domain.AttemptStanding). Velox's
// charge call writes its attempt row when Stripe answers, before it reports
// the outcome, and its charges are serialized by the charge lease, so the
// newest Velox-made row is the attempt the invoice is waiting on. Rows for
// attempts Velox did not make (hosted Checkout, inserted 'external' by a
// report) do not count. Empty-PI rows (a create that failed, a parked
// attempt) do: an older attempt's late decline is superseded by them.
func veloxAttemptStandingTx(ctx context.Context, tx *sql.Tx, invoiceID, pi string) (domain.AttemptStanding, error) {
	var mine bool
	var newest sql.NullString
	err := tx.QueryRowContext(ctx, `
		SELECT
			EXISTS (SELECT 1 FROM invoice_charge_attempts
				WHERE invoice_id = $1 AND stripe_payment_intent_id = $2
				  AND trigger_source IN ('auto_charge', 'charge_now', 'dunning_retry')),
			(SELECT stripe_payment_intent_id FROM invoice_charge_attempts
				WHERE invoice_id = $1 AND trigger_source IN ('auto_charge', 'charge_now', 'dunning_retry')
				ORDER BY created_at DESC, id DESC LIMIT 1)`,
		invoiceID, pi,
	).Scan(&mine, &newest)
	if err != nil {
		return domain.AttemptNotVelox, fmt.Errorf("read attempt standing: %w", err)
	}
	switch {
	case !mine:
		return domain.AttemptNotVelox, nil
	case newest.String == pi:
		return domain.AttemptNewest, nil
	default:
		return domain.AttemptSuperseded, nil
	}
}

// MarkPaid settles an invoice (status→paid, amount_due→0) and is the
// single transactional outbox point for invoice.paid. Thin wrapper over
// MarkPaidReportingTransition for the many callers that don't care
// whether THIS call did the transition.
func (s *PostgresStore) MarkPaid(ctx context.Context, tenantID, id string, stripePaymentIntentID string, paidAt time.Time) (domain.Invoice, error) {
	inv, _, err := s.MarkPaidReportingTransition(ctx, tenantID, id, stripePaymentIntentID, paidAt)
	return inv, err
}

// MarkPaidReportingTransition is MarkPaid plus a `transitioned` flag:
// true when THIS call moved the invoice finalized/uncollectible → paid,
// false when it was already paid (the idempotent no-op branch). The
// row lock serializes concurrent callers, so for two
// at-least-once webhook deliveries of the SAME charge exactly one gets
// transitioned=true. SettleSucceeded gates its post-paid side-effects
// (payment.succeeded event, receipt email, card stamp) on the flag so a
// concurrent redelivery — which the stale-read fast-path guard can't
// catch — fires them once, not twice. invoice.paid itself is already
// once-only (enqueued inside this tx, after the no-op branch returns).
func (s *PostgresStore) MarkPaidReportingTransition(ctx context.Context, tenantID, id string, stripePaymentIntentID string, paidAt time.Time) (domain.Invoice, bool, error) {
	return s.markPaidReportingTransition(ctx, tenantID, id, stripePaymentIntentID, paidAt, settleOther, nil, nil)
}

// MarkPaidOffline is MarkPaid for an operator-recorded payment (cheque,
// wire): the paid flip sets paid_out_of_band and leaves the invoice with no
// PaymentIntent, since no charge was sent to Stripe (it takes none, so this
// path cannot write one). check runs on the invoice as it is under this
// transition's row lock, before anything is written, and its error refuses
// the payment. It exists so the offline path can refuse while a card charge
// is in progress (#975) — the service's own read comes before the lock and
// can miss a charge that started after it. Only the offline path gets a
// check: the card settlement and the credit-covered settles run inside the
// charge's own hold and must always be able to record it.
func (s *PostgresStore) MarkPaidOffline(ctx context.Context, tenantID, id string, paidAt time.Time, check func(tx *sql.Tx, locked domain.Invoice) error) (domain.Invoice, error) {
	inv, _, err := s.markPaidReportingTransition(ctx, tenantID, id, "", paidAt, settleOffline, check, nil)
	return inv, err
}

// MarkPaidCardSettlementTransition is MarkPaidReportingTransition for the CARD
// settlement path (SettleSucceeded): in addition to invoice.paid it enqueues
// payment.succeeded in the SAME tx, so that event — the only one carrying the
// Stripe payment_intent_id — is crash-safe with the paid-flip instead of the
// old fire-and-forget post-commit dispatch. Non-card settlement paths
// (credits-cover, offline record-payment, dunning-recovery bare-MarkPaid) keep
// calling MarkPaidReportingTransition and emit only invoice.paid (they never
// fired payment.succeeded). ADR-040 transactional outbox; see the durability-
// tiering note in payment/settlement.go.
// then (nil = none) runs INSIDE this transition's transaction, after the
// paid-flip and its payment.succeeded enqueue, only when THIS call won the
// transition — and under a SAVEPOINT, so an email-side failure is logged and
// skipped while the money write still commits (ADR-040's amendment: the
// receipt used to be enqueued after this tx committed, and a process death or
// failover in that gap lost the receipt for good — the transition gate then
// suppresses every later attempt). It receives the freshly-written row
// because the receipt's amount is the amount actually booked here.
func (s *PostgresStore) MarkPaidCardSettlementTransition(ctx context.Context, tenantID, id string, stripePaymentIntentID string, paidAt time.Time, then func(tx *sql.Tx, fresh domain.Invoice) error) (domain.Invoice, bool, error) {
	return s.markPaidReportingTransition(ctx, tenantID, id, stripePaymentIntentID, paidAt, settleCard, nil, then)
}

// settleKind says which paid flip markPaidLockedTx writes: the effects that
// ride it differ by how the money arrived.
type settleKind int

const (
	// settleOther: credits cover it, nothing is due, or a caller that names
	// the PaymentIntent it settles without being the card settle path (the
	// reconciler's fallback). invoice.paid only.
	settleOther settleKind = iota
	// settleCard: SettleSucceeded. Adds payment.succeeded in the same tx.
	settleCard
	// settleOffline: RecordOfflinePayment. Sets paid_out_of_band; no
	// PaymentIntent, no charge attempt.
	settleOffline
)

// markPaidReportingTransition is the shared core. settleCard also enqueues
// payment.succeeded on the SAME tx as the paid-flip (gated on the
// transition, so exactly-once); settleOffline marks the payment out of band. Both event enqueues live inside this one tx, so
// they are atomic with finalized/uncollectible→paid: tx rolls back → no events;
// commits → the dispatcher delivers.
// check (nil = none) is MarkPaidOffline's refusal, run under the row lock.
func (s *PostgresStore) markPaidReportingTransition(ctx context.Context, tenantID, id string, stripePaymentIntentID string, paidAt time.Time, kind settleKind, check func(tx *sql.Tx, locked domain.Invoice) error, then func(tx *sql.Tx, fresh domain.Invoice) error) (domain.Invoice, bool, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.Invoice{}, false, err
	}
	defer postgres.Rollback(tx)

	// Hard invariant: paying an invoice implies authoritative amounts.
	// Authoritative amounts require status IN (finalized, uncollectible)
	// — draft means tax may still be pending or the operator's still
	// editing line items; voided means the invoice was annulled and
	// must not transition to paid.
	//
	// Also reject when tax_status is still pending: even a
	// finalized invoice with unresolved tax (rare — finalize itself
	// gates on tax_status=ok, but a manual finalize bypassing that
	// gate could create one) would lock the customer at the wrong
	// total. Stripe Tax-equivalent shape.
	//
	// Pre-fix bug (caught 2026-05-22): billOnePeriod's "credits cover
	// 100%, mark paid immediately" branch called MarkPaid on a draft
	// + tax_status=pending invoice, transitioning draft→paid directly
	// and bypassing the finalize gate. Customer charged subtotal-only
	// (tax=0) and tax retry blocked forever (retry requires
	// status='draft', but status is now 'paid').
	//
	// Re-paying an already-paid invoice is allowed (idempotent — the
	// PaymentSucceeded webhook can fire twice on legitimate retries).
	if err := s.lockDunningPauseTx(ctx, tx, id); err != nil {
		return domain.Invoice{}, false, err
	}
	// FOR NO KEY UPDATE serializes with every other writer of the row and
	// leaves a period closer's FOR KEY SHARE watermark read free
	// (GetLatestThresholdInvoiceForCycleTx): this transaction can need the
	// subscription lock that closer holds (the dunning-pause release), so
	// the closer must not wait for it.
	var status, taxStatus string
	if err := tx.QueryRowContext(ctx,
		`SELECT status, tax_status FROM invoices WHERE id = $1 FOR NO KEY UPDATE`, id,
	).Scan(&status, &taxStatus); err != nil {
		if err == sql.ErrNoRows {
			return domain.Invoice{}, false, errs.ErrNotFound
		}
		return domain.Invoice{}, false, fmt.Errorf("load invoice for mark-paid: %w", err)
	}
	if check != nil {
		// The row is locked above, so this read is the state the write acts on.
		var locked domain.Invoice
		if err := tx.QueryRowContext(ctx, `SELECT `+invCols+` FROM invoices WHERE id = $1`, id).
			Scan(s.scanInvDest(&locked)...); err != nil {
			return domain.Invoice{}, false, fmt.Errorf("read locked invoice for mark-paid check: %w", err)
		}
		if err := check(tx, locked); err != nil {
			return domain.Invoice{}, false, err
		}
	}
	switch domain.InvoiceStatus(status) {
	case domain.InvoiceFinalized, domain.InvoiceUncollectible:
		// ok — transition to paid below.
	case domain.InvoicePaid:
		// Already paid: re-marking is a legitimate idempotent event. The
		// payment_intent.succeeded webhook and the ambiguous-charge reconciler
		// can resolve the SAME charge under different Stripe event ids (the
		// reconciler never goes through event-id dedup), so MarkPaid fires
		// more than once on the unknown-charge recovery path. Return the
		// existing row UNCHANGED rather than re-running the money UPDATE.
		//
		// The money UPDATE is NOT idempotent: `amount_paid_cents =
		// amount_due_cents` reads the row's CURRENT amount_due_cents, which the
		// first MarkPaid already zeroed — so a second call set
		// amount_paid_cents = 0, corrupting the recorded paid amount and
		// blocking card refunds (refunds size against amount_paid_cents).
		// (audit: velox-ops MarkPaid finding)
		var inv domain.Invoice
		if err := tx.QueryRowContext(ctx,
			`SELECT `+invCols+` FROM invoices WHERE id = $1`, id,
		).Scan(s.scanInvDest(&inv)...); err != nil {
			return domain.Invoice{}, false, fmt.Errorf("reload already-paid invoice: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return domain.Invoice{}, false, err
		}
		return inv, false, nil
	default:
		return domain.Invoice{}, false, errs.InvalidState(fmt.Sprintf(
			"cannot mark invoice paid from status %q — finalize the invoice first (tax-pending invoices stay draft until tax resolves; voided invoices are terminal)",
			status,
		))
	}
	if domain.InvoiceTaxStatus(taxStatus) != domain.InvoiceTaxOK {
		// Pending = tax provider hasn't returned a final answer; the
		// invoice total is subtotal-only and would lock if marked paid.
		// Failed = retries exhausted; the operator should void or
		// manually finalize after resolving the tax decision out-of-
		// band, not silently pay at $0 tax.
		// (Tax-exempt customers / regions resolve to tax_status='ok'
		// with tax_amount_cents=0 + tax_exempt_reason — they're not a
		// separate status.)
		return domain.Invoice{}, false, errs.InvalidState(fmt.Sprintf(
			"cannot mark invoice paid with tax_status=%q — wait for tax retry to resolve, then re-attempt",
			taxStatus,
		))
	}

	inv, err := s.markPaidLockedTx(ctx, tx, tenantID, id, stripePaymentIntentID, paidAt, kind)
	if err != nil {
		return domain.Invoice{}, false, err
	}
	// The money email rides THIS transaction (ADR-040 amendment): enqueued
	// beside the events above, gated on the same transition, inside a
	// SAVEPOINT so an email-side failure is logged and skipped while the
	// settlement still commits. Never the reverse — a receipt must not be
	// able to un-settle a payment.
	if then != nil {
		if _, err := tx.ExecContext(ctx, "SAVEPOINT settle_email"); err != nil {
			return domain.Invoice{}, false, err
		}
		if herr := then(tx, inv); herr != nil {
			if _, rerr := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT settle_email"); rerr != nil {
				return domain.Invoice{}, false, rerr
			}
			slog.ErrorContext(ctx, "settlement email enqueue failed inside the settle transaction — settlement committed, email skipped",
				"invoice_id", id, "tenant_id", tenantID, "error", herr)
		} else if _, err := tx.ExecContext(ctx, "RELEASE SAVEPOINT settle_email"); err != nil {
			return domain.Invoice{}, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return domain.Invoice{}, false, err
	}
	return inv, true, nil
}

// markPaidLockedTx writes the finalized/uncollectible → paid transition on tx
// and the effects that ride it: the open checkout claims close, invoice.paid
// is enqueued, and so is payment.succeeded for a card settlement. An offline
// settlement sets paid_out_of_band and names no PaymentIntent. The caller
// holds the invoice row lock on tx and has decided the invoice may be paid.
// Every paid flip goes through here (ADR-068 choke point), so the effects
// cannot differ between settle paths.
func (s *PostgresStore) markPaidLockedTx(ctx context.Context, tx *sql.Tx, tenantID, id string, stripePaymentIntentID string, paidAt time.Time, kind settleKind) (domain.Invoice, error) {
	now := clock.Now(ctx)
	var inv domain.Invoice
	// amount_paid_cents records the CURRENT amount_due (what the PaymentIntent was
	// created for), NOT Stripe's actual captured amount. Holds because the PI is
	// created for exactly amount_due and capture is synchronous + full on the
	// card-on-file path. KNOWN EDGE class: on an ASYNC / SCA charge
	// (payment_status='processing'), a credit note that reduces amount_due AFTER
	// PI-create but BEFORE this settle would record amount_paid BELOW the captured
	// amount, under-reporting the refund cap (creditnote refund is capped at
	// amount_paid).
	// PART A + PART B are now CLOSED (ADR-059, 2026-06-22), so amount_due-at-settle
	// == the captured amount for both the operator and the automated path:
	//   PART A — creditnote.Service.Create rejects an amount_due-reducing CN while
	//     the payment is in flight (operator 409).
	//   PART B — the AUTOMATED clawback (CreateAndIssueAdjustment, ADR-050
	//     unpaid-source) no longer reduces amount_due on an in-flight source: it
	//     creates the clawback as a draft and Issue() DEFERS until the source
	//     settles, so amount_due stays full through this settle.
	// PART C remains, but is NOT reachable today: recording amount_paid from the
	// PI's amount_received (vs re-reading amount_due) is only load-bearing under
	// PARTIAL capture, and Velox is PaymentIntent-only full-capture (no
	// partial/manual-capture flow). Revisit if partial capture is added.
	// (2026-06-15 proration audit; Parts A/B shipped #295 + ADR-059.)
	err := tx.QueryRowContext(ctx, `
		UPDATE invoices SET
			status = 'paid',
			payment_status = 'succeeded',
			stripe_payment_intent_id = $1,
			paid_out_of_band = $5,
			charge_attempt_seq = charge_attempt_seq + 1,
			paid_at = $2,
			amount_paid_cents = amount_due_cents,
			amount_due_cents = 0,
			auto_charge_pending = FALSE,
			updated_at = $3
		WHERE id = $4
		RETURNING `+invCols,
		postgres.NullableString(stripePaymentIntentID), paidAt, now, id, kind == settleOffline,
	).Scan(s.scanInvDest(&inv)...)

	if err == sql.ErrNoRows {
		return domain.Invoice{}, errs.ErrNotFound
	}
	if err != nil {
		return domain.Invoice{}, err
	}
	// Close open checkout claims IN THIS TX (ADR-068): every MarkPaid variant
	// converges here, so the DB-side close is a single choke point instead of
	// 9+ hand-stitched post-commit hooks — and it atomically kills the "POST
	// /checkout returns a stored open session for a just-settled invoice"
	// race. The Stripe-side expire is the caller's post-commit best-effort;
	// the session's own ExpiresAt is the backstop.
	if _, err := tx.ExecContext(ctx, `
		UPDATE checkout_sessions SET status = 'invoice_settled', updated_at = now()
		WHERE invoice_id = $1 AND status = 'open'
	`, id); err != nil {
		return domain.Invoice{}, fmt.Errorf("close checkout claims on settle: %w", err)
	}
	// invoice.paid — enqueued in the SAME tx as the finalized/uncollectible →
	// paid transition. Callers reach here only for an invoice that is not paid
	// yet (markPaidReportingTransition's already-paid branch returns first;
	// SettleZeroDueTx admits only an open invoice), so this fires EXACTLY ONCE,
	// and it covers every settlement path (card via SettleSucceeded,
	// credits-cover, offline record-payment, dunning recovery, the reconciler's
	// bare-MarkPaid fallback, and a credit note that leaves nothing due)
	// without each needing its own dispatch. ADR-040 transactional outbox: if the tx rolls
	// back, no event; if it commits, the dispatcher delivers.
	if s.outbox != nil {
		if _, err := s.outbox.Enqueue(ctx, tx, tenantID, domain.EventInvoicePaid, domain.InvoiceWebhookData(inv, nil)); err != nil {
			return domain.Invoice{}, fmt.Errorf("enqueue invoice.paid: %w", err)
		}
	}
	// payment.succeeded — CARD settlement path only. Enqueued in the SAME tx as
	// the paid-flip so the only event carrying the Stripe payment_intent_id is
	// crash-safe rather than fire-and-forget post-commit (a post-commit window
	// is as wide as whatever runs before the enqueue — see the durability-
	// tiering block in payment/settlement.go). Reaches here only on the
	// finalized/uncollectible→paid transition, so it fires exactly once, same
	// as invoice.paid.
	if kind == settleCard && s.outbox != nil {
		if _, err := s.outbox.Enqueue(ctx, tx, tenantID, domain.EventPaymentSucceeded, domain.InvoiceWebhookData(inv, nil)); err != nil {
			return domain.Invoice{}, fmt.Errorf("enqueue payment.succeeded: %w", err)
		}
	}
	// ADR-103: same-tx outcome resolution (see upsertChargeAttemptTx).
	// amount_paid_cents is what this PaymentIntent actually settled. On a
	// test clock the attempt is dated where the invoice says it was paid.
	var attemptSimAt *time.Time
	if _, ok := clock.SimOf(ctx); ok {
		attemptSimAt = &paidAt
	}
	if err := upsertChargeAttemptTx(ctx, tx, tenantID, inv.ID, stripePaymentIntentID,
		domain.ChargeAttemptSucceeded, "", inv.AmountPaidCents, "", "", attemptSimAt); err != nil {
		return domain.Invoice{}, err
	}
	// A dunning pause set for this invoice lifts in this transaction, so
	// the release cannot be lost to a crash after commit. The caller took
	// the subscription lock it needs first (lockDunningPauseTx).
	if inv.SubscriptionID != "" && s.dunningPauseRelease != nil {
		if err := s.dunningPauseRelease.ReleaseDunningPauseTx(ctx, tx, inv.SubscriptionID, inv.ID, now); err != nil {
			return domain.Invoice{}, err
		}
	}
	return inv, nil
}

// SetPaymentCard stamps the card brand + last4 used to settle an
// invoice. Called from the payment_intent.succeeded webhook handler
// AFTER MarkPaid lands; kept as a separate update so MarkPaid stays
// backward-compatible (many call sites — dunning retrier, public
// payment-page handler, payment reconciler — none of which know
// about the card). Best-effort: card resolution failure leaves the
// columns empty, which renders no sub-line in the timeline.
// Migration 0077 / ADR-020.
func (s *PostgresStore) SetPaymentCard(ctx context.Context, tenantID, id, brand, last4 string) error {
	if brand == "" && last4 == "" {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)

	res, err := tx.ExecContext(ctx, `
		UPDATE invoices SET
			payment_card_brand = $1,
			payment_card_last4 = $2,
			updated_at = now()
		WHERE id = $3
	`, postgres.NullableString(brand), postgres.NullableString(last4), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errs.ErrNotFound
	}
	return tx.Commit()
}

func (s *PostgresStore) ApplyCreditNote(ctx context.Context, tenantID, id string, amountCents int64) (domain.Invoice, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.Invoice{}, err
	}
	defer postgres.Rollback(tx)

	inv, err := s.ApplyCreditNoteTx(ctx, tx, tenantID, id, amountCents, 0)
	if err != nil {
		return domain.Invoice{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.Invoice{}, err
	}
	return inv, nil
}

// LockForCreditNoteTx locks the invoice row on the caller's coordinator tx and
// returns it as it is under the lock. Credit-note Issue() takes it before
// anything else and decides from this row, because the amount still due and the
// payment state can both move between its earlier read and its transaction (a
// credit application, a settle, a void). FOR NO KEY UPDATE serializes with
// those writers without blocking the foreign-key checks of rows that reference
// the invoice. A note that leaves nothing due marks the invoice paid on this
// tx (SettleZeroDueTx), so the subscription lock a dunning-pause release
// needs is taken first.
func (s *PostgresStore) LockForCreditNoteTx(ctx context.Context, tx *sql.Tx, tenantID, id string) (domain.Invoice, error) {
	if err := s.lockDunningPauseTx(ctx, tx, id); err != nil {
		return domain.Invoice{}, err
	}
	var inv domain.Invoice
	err := tx.QueryRowContext(ctx,
		`SELECT `+invCols+` FROM invoices WHERE id = $1 AND tenant_id = $2 FOR NO KEY UPDATE`,
		id, tenantID).Scan(s.scanInvDest(&inv)...)
	if err == sql.ErrNoRows {
		return domain.Invoice{}, errs.ErrNotFound
	}
	if err != nil {
		return domain.Invoice{}, fmt.Errorf("lock invoice for credit note: %w", err)
	}
	return inv, nil
}

// ApplyCreditNoteTx applies an issued credit note to an open receivable on the
// caller's coordinator tx, so it commits atomically with the note's
// draft→issued CAS (ADR-061, creditnote.Issue): amount_due goes down by
// reduceCents, and creditsReturnedCents of customer credits are taken back off
// the invoice (the caller grants them to the balance on the same tx). The two
// add up to the note's total.
//
// An open receivable is a finalized or written-off (uncollectible) invoice
// not yet paid: a write-off ends collection, not what is owed, so a relief
// note lowers what a written-off invoice asks for as it does an unpaid one's
// (ADR-050 amendment 2026-10-23). A written-off invoice left with nothing due
// stays written off: SettleZeroDueTx settles only a finalized one.
//
// Both subtractions are exact. A note that asks for more than is due, or to
// take back more credits than are applied, is refused with
// credit_note_exceeds_invoice: a silent clamp here relieved an invoice by more
// than its total (ADR-061 amendment, 2026-10-12).
//
// The single-CAS-gated-caller invariant — exactly one Issue() reaches this per
// credit note — makes the reduction idempotent by construction, so no
// source-dedup row is needed. A reviewer adding a second caller of this method
// MUST reintroduce (tenant, source) dedup; see ADR-061.
func (s *PostgresStore) ApplyCreditNoteTx(ctx context.Context, tx *sql.Tx, tenantID, id string, reduceCents, creditsReturnedCents int64) (domain.Invoice, error) {
	if reduceCents < 0 || creditsReturnedCents < 0 {
		return domain.Invoice{}, fmt.Errorf("apply credit note: negative amount (reduce %d, credits returned %d)", reduceCents, creditsReturnedCents)
	}
	now := clock.Now(ctx)
	var inv domain.Invoice
	err := tx.QueryRowContext(ctx, `
		UPDATE invoices SET
			amount_due_cents = amount_due_cents - $1,
			credits_applied_cents = credits_applied_cents - $2,
			updated_at = $3
		WHERE id = $4 AND status IN ('finalized', 'uncollectible') AND payment_status <> 'succeeded'
		  AND amount_due_cents >= $1 AND credits_applied_cents >= $2
		RETURNING `+invCols,
		reduceCents, creditsReturnedCents, now, id,
	).Scan(s.scanInvDest(&inv)...)

	if err == sql.ErrNoRows {
		// Either no such invoice, or it is no longer an open receivable: the
		// caller (credit-note Issue) chose the "reduce amount due" branch from
		// a read taken before its tx, and a payment (or a finalize/void) landed
		// since. Refuse, so its tx rolls back and the reconciler re-drives the
		// note against the invoice's current state (a paid one is credited to
		// the balance instead of losing the relief).
		var status, payment string
		var due, applied int64
		rerr := tx.QueryRowContext(ctx, `SELECT status, payment_status, amount_due_cents, credits_applied_cents FROM invoices WHERE id = $1`, id).Scan(&status, &payment, &due, &applied)
		if rerr == sql.ErrNoRows {
			return domain.Invoice{}, errs.ErrNotFound
		}
		if rerr != nil {
			return domain.Invoice{}, rerr
		}
		if (status == string(domain.InvoiceFinalized) || status == string(domain.InvoiceUncollectible)) && payment != string(domain.PaymentSucceeded) {
			return domain.Invoice{}, errs.InvalidState(fmt.Sprintf(
				"credit note asks to reduce invoice %s by %d with %d due and to return %d credits with %d applied",
				id, reduceCents, due, creditsReturnedCents, applied)).
				WithCode("credit_note_exceeds_invoice")
		}
		return domain.Invoice{}, errs.InvalidState(fmt.Sprintf(
			"invoice %s is %s/%s, not an open receivable; the credit note will be re-applied against its current state", id, status, payment)).
			WithCode("invoice_not_open_receivable")
	}
	if err != nil {
		return domain.Invoice{}, err
	}
	// Any amount_due change invalidates an open checkout claim (its session
	// was minted for the OLD amount): close it in the SAME tx so the next
	// POST mints at the new amount and the post-commit helper can expire the
	// stale-amount Stripe session (ADR-068; covers both partial credit and
	// credit-to-zero — the latter also exits the payable state entirely).
	if _, err := tx.ExecContext(ctx, `
		UPDATE checkout_sessions SET status = 'superseded', updated_at = now()
		WHERE invoice_id = $1 AND status = 'open'
	`, id); err != nil {
		return domain.Invoice{}, fmt.Errorf("supersede checkout claims on credit apply: %w", err)
	}
	return inv, nil
}

// SettleZeroDueTx marks an open invoice with nothing left due as paid, on the
// caller's transaction: the ADR-066 zero-due terminal, with invoice.paid in
// the same transaction. Stripe does the same for a credit note: one that
// "reduces the balance of an open invoice to 0" makes the invoice paid. The
// caller holds the invoice row lock on tx.
//
// It settles only a finalized invoice whose payment is pending or failed, with
// nothing due, tax ok, and no charge hold. Any hold refuses, its own owner's
// included: dunning's final action issues the cancel's relief note under its
// hold, and an invoice paid in the middle of that would turn the write-off
// into a failed final action and the cancel into "payment recovered". A stale
// hold refuses too (ADR-127): its charge may have taken the money.
//
// Not settling is an answer, not an error: it returns the row and false, so
// the caller's transaction (a credit note's issue) still commits. paid
// reports whether this call settled the invoice.
func (s *PostgresStore) SettleZeroDueTx(ctx context.Context, tx *sql.Tx, tenantID, id string, paidAt time.Time) (domain.Invoice, bool, error) {
	var inv domain.Invoice
	if err := tx.QueryRowContext(ctx, `SELECT `+invCols+` FROM invoices WHERE id = $1`, id).
		Scan(s.scanInvDest(&inv)...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Invoice{}, false, errs.ErrNotFound
		}
		return domain.Invoice{}, false, fmt.Errorf("read invoice for the zero-due settle: %w", err)
	}
	if inv.Status != domain.InvoiceFinalized || inv.AmountDueCents != 0 || inv.TaxStatus != domain.InvoiceTaxOK ||
		(inv.PaymentStatus != domain.PaymentPending && inv.PaymentStatus != domain.PaymentFailed) {
		return inv, false, nil
	}
	// The row lock the caller holds is the one every claim takes before it
	// writes the hold, so this answer holds until tx commits. Read before
	// the UPDATE below, which takes no stronger lock than the caller's.
	hold, _, err := s.ChargeHeldTx(ctx, tx, id)
	if err != nil {
		return domain.Invoice{}, false, err
	}
	if hold != domain.ChargeHoldNone {
		return inv, false, nil
	}
	paid, err := s.markPaidLockedTx(ctx, tx, tenantID, id, "", paidAt, settleOther)
	if err != nil {
		return domain.Invoice{}, false, err
	}
	return paid, true, nil
}

// SettleZeroDue is SettleZeroDueTx in a transaction of its own, which locks
// the invoice row first. A dunning retry that finds nothing due calls it.
func (s *PostgresStore) SettleZeroDue(ctx context.Context, tenantID, id string, paidAt time.Time) (domain.Invoice, bool, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.Invoice{}, false, err
	}
	defer postgres.Rollback(tx)

	if err := s.lockDunningPauseTx(ctx, tx, id); err != nil {
		return domain.Invoice{}, false, err
	}
	var one int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM invoices WHERE id = $1 FOR NO KEY UPDATE`, id).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Invoice{}, false, errs.ErrNotFound
		}
		return domain.Invoice{}, false, fmt.Errorf("lock invoice for the zero-due settle: %w", err)
	}
	inv, paid, err := s.SettleZeroDueTx(ctx, tx, tenantID, id, paidAt)
	if err != nil {
		return domain.Invoice{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return domain.Invoice{}, false, err
	}
	return inv, paid, nil
}

// ApplyCredits and UpdateTotals were DELETED (2026-07-21 derived-data
// audit): both were caller-free interface remnants that blind-set derived
// money columns (totals not recomputed from line items; credits bumped
// without a ledger entry), bypassing every in-tx recompute guard the live
// paths use. Re-adding an unguarded totals/credits writer re-opens the
// drift class — write through AddLineItemAtomic / credit.ApplyToInvoiceAtomic.

// nominalRateArg returns the driver value for the nullable
// nominal_unit_amount_decimal column: the decimal (via its Valuer) when set,
// else nil → SQL NULL. See domain.InvoiceLineItem.NominalUnitAmountDecimal.
func nominalRateArg(p *decimal.Decimal) any {
	if p == nil {
		return nil
	}
	return *p
}

// scanNominalRate converts a scanned nullable NUMERIC into the optional
// nominal per-unit rate pointer (nil when the column is NULL).
func scanNominalRate(n decimal.NullDecimal) *decimal.Decimal {
	if !n.Valid {
		return nil
	}
	d := n.Decimal
	return &d
}

func (s *PostgresStore) CreateLineItem(ctx context.Context, tenantID string, item domain.InvoiceLineItem) (domain.InvoiceLineItem, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.InvoiceLineItem{}, err
	}
	defer postgres.Rollback(tx)

	id := postgres.NewID("vlx_ili")
	now := clock.Now(ctx)
	metaJSON, _ := json.Marshal(item.Metadata)
	if item.Metadata == nil {
		metaJSON = []byte("{}")
	}

	err = tx.QueryRowContext(ctx, `
		INSERT INTO invoice_line_items (id, invoice_id, tenant_id, line_type, meter_id,
			description, quantity, unit_amount_cents, amount_cents, tax_rate, tax_amount_cents,
			total_amount_cents, currency, pricing_mode, rating_rule_version_id,
			billing_period_start, billing_period_end, metadata, created_at,
			tax_jurisdiction, tax_code, tax_reason, quantity_decimal, nominal_unit_amount_decimal, meter_unit)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25)
		RETURNING id, invoice_id, tenant_id, line_type, COALESCE(meter_id,''), description,
			quantity, unit_amount_cents, amount_cents, tax_rate, tax_amount_cents,
			total_amount_cents, currency, COALESCE(pricing_mode,''),
			COALESCE(rating_rule_version_id,''), billing_period_start, billing_period_end,
			metadata, created_at, tax_jurisdiction, tax_code, tax_reason, quantity_decimal
	`, id, item.InvoiceID, tenantID, item.LineType, postgres.NullableString(item.MeterID),
		item.Description, item.Quantity, item.UnitAmountCents, item.AmountCents,
		item.TaxRate, item.TaxAmountCents, item.TotalAmountCents, item.Currency,
		postgres.NullableString(item.PricingMode), postgres.NullableString(item.RatingRuleVersionID),
		postgres.NullableTime(item.BillingPeriodStart), postgres.NullableTime(item.BillingPeriodEnd),
		metaJSON, now, item.TaxJurisdiction, item.TaxCode, item.TaxabilityReason, item.QuantityDecimal,
		nominalRateArg(item.NominalUnitAmountDecimal), postgres.NullableString(item.MeterUnit),
	).Scan(&item.ID, &item.InvoiceID, &item.TenantID, &item.LineType, &item.MeterID,
		&item.Description, &item.Quantity, &item.UnitAmountCents, &item.AmountCents,
		&item.TaxRate, &item.TaxAmountCents, &item.TotalAmountCents, &item.Currency,
		&item.PricingMode, &item.RatingRuleVersionID,
		&item.BillingPeriodStart, &item.BillingPeriodEnd, &metaJSON, &item.CreatedAt,
		&item.TaxJurisdiction, &item.TaxCode, &item.TaxabilityReason, &item.QuantityDecimal)

	if err != nil {
		return domain.InvoiceLineItem{}, err
	}
	_ = json.Unmarshal(metaJSON, &item.Metadata)
	if err := tx.Commit(); err != nil {
		return domain.InvoiceLineItem{}, err
	}
	return item, nil
}

func (s *PostgresStore) ListLineItems(ctx context.Context, tenantID, invoiceID string) ([]domain.InvoiceLineItem, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	rows, err := tx.QueryContext(ctx, `
		SELECT id, invoice_id, tenant_id, line_type, COALESCE(meter_id,''), description,
			quantity, unit_amount_cents, amount_cents, tax_rate, tax_amount_cents,
			total_amount_cents, currency, COALESCE(pricing_mode,''),
			COALESCE(rating_rule_version_id,''), billing_period_start, billing_period_end,
			metadata, created_at, tax_jurisdiction, tax_code, tax_reason, quantity_decimal,
			commit_granted_cents, commit_expires_at, nominal_unit_amount_decimal, COALESCE(meter_unit,'')
		FROM invoice_line_items WHERE invoice_id = $1
		ORDER BY created_at ASC
	`, invoiceID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var items []domain.InvoiceLineItem
	for rows.Next() {
		var item domain.InvoiceLineItem
		var metaJSON []byte
		var nominal decimal.NullDecimal
		if err := rows.Scan(&item.ID, &item.InvoiceID, &item.TenantID, &item.LineType,
			&item.MeterID, &item.Description, &item.Quantity, &item.UnitAmountCents,
			&item.AmountCents, &item.TaxRate, &item.TaxAmountCents, &item.TotalAmountCents,
			&item.Currency, &item.PricingMode, &item.RatingRuleVersionID,
			&item.BillingPeriodStart, &item.BillingPeriodEnd, &metaJSON, &item.CreatedAt,
			&item.TaxJurisdiction, &item.TaxCode, &item.TaxabilityReason, &item.QuantityDecimal,
			&item.CommitGrantedCents, &item.CommitExpiresAt, &nominal, &item.MeterUnit); err != nil {
			return nil, err
		}
		item.NominalUnitAmountDecimal = scanNominalRate(nominal)
		_ = json.Unmarshal(metaJSON, &item.Metadata)
		items = append(items, item)
	}
	return items, rows.Err()
}

// GetOutstandingBalance computes the customer's AR exposure: amount_due_cents
// + count across all finalized invoices in payment_status
// pending/failed/unknown, excluding voided + uncollectible, grouped by
// currency (cents in different currencies cannot be added into one figure).
// Single tenant-scoped aggregate query; powers the "Outstanding" cell on
// customer detail.
func (s *PostgresStore) GetOutstandingBalance(ctx context.Context, tenantID, customerID string) (OutstandingBalance, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return OutstandingBalance{}, err
	}
	defer postgres.Rollback(tx)

	rows, err := tx.QueryContext(ctx, `
		SELECT currency, SUM(amount_due_cents), COUNT(*)
		FROM invoices
		WHERE customer_id = $1
		  AND payment_status IN ('pending', 'failed', 'unknown')
		  AND status NOT IN ('voided', 'uncollectible', 'draft')
		GROUP BY currency
		ORDER BY SUM(amount_due_cents) DESC, currency
	`, customerID)
	if err != nil {
		return OutstandingBalance{}, err
	}
	defer func() { _ = rows.Close() }()

	out := OutstandingBalance{ByCurrency: []CurrencyAmount{}}
	for rows.Next() {
		var a CurrencyAmount
		var n int
		if err := rows.Scan(&a.Currency, &a.AmountCents, &n); err != nil {
			return OutstandingBalance{}, err
		}
		out.TotalCents += a.AmountCents
		out.UnpaidCount += n
		if a.AmountCents > 0 {
			out.ByCurrency = append(out.ByCurrency, a)
		}
	}
	if err := rows.Err(); err != nil {
		return OutstandingBalance{}, err
	}
	return out, nil
}

// AddLineItemAtomic inserts a line item and recomputes invoice totals in a
// single transaction, locking the invoice row FOR UPDATE so concurrent
// AddLineItem calls serialize on that row and subtotal reflects every
// committed line item. Fails if the invoice isn't in draft status.
func (s *PostgresStore) AddLineItemAtomic(
	ctx context.Context, tenantID, invoiceID string, item domain.InvoiceLineItem,
) (domain.InvoiceLineItem, domain.Invoice, error) {
	return s.AddLineItemAtomicAudited(ctx, tenantID, invoiceID, item, nil)
}

// AddLineItemAtomicAudited is AddLineItemAtomic with an in-tx audit emission
// (ADR-090): emit runs on the SAME transaction, after the line INSERT and the
// totals recompute have both landed and BEFORE commit. It receives the
// persisted line AND the rewritten invoice (for the invoice number the audit
// row is labelled with — read inside the tx, not from a pre-tx snapshot). An
// emission error rolls the line item and the totals back together. nil emit =
// unaudited write.
func (s *PostgresStore) AddLineItemAtomicAudited(
	ctx context.Context, tenantID, invoiceID string, item domain.InvoiceLineItem,
	emit func(tx *sql.Tx, item domain.InvoiceLineItem, inv domain.Invoice) error,
) (domain.InvoiceLineItem, domain.Invoice, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.InvoiceLineItem{}, domain.Invoice{}, err
	}
	defer postgres.Rollback(tx)

	var (
		status        domain.InvoiceStatus
		currency      string
		billingReason sql.NullString
	)
	err = tx.QueryRowContext(ctx,
		`SELECT status, currency, billing_reason FROM invoices WHERE id = $1 FOR UPDATE`,
		invoiceID,
	).Scan(&status, &currency, &billingReason)
	if err == sql.ErrNoRows {
		return domain.InvoiceLineItem{}, domain.Invoice{}, errs.ErrNotFound
	}
	if err != nil {
		return domain.InvoiceLineItem{}, domain.Invoice{}, err
	}
	if status != domain.InvoiceDraft {
		return domain.InvoiceLineItem{}, domain.Invoice{},
			fmt.Errorf("can only add line items to draft invoices, current status: %s", status)
	}

	// Invoice-level commit rules (ADR-078), enforced under the row lock so
	// they can't race a concurrent add: commit lines live only on manual
	// invoices, at most one per invoice (the per-invoice fund-once index
	// depends on it).
	if item.IsCommitLine() {
		if domain.InvoiceBillingReason(billingReason.String) != domain.BillingReasonManual {
			return domain.InvoiceLineItem{}, domain.Invoice{},
				errs.Invalid("commit_granted_cents", "commit purchase lines are only supported on manual invoices")
		}
		var hasCommit bool
		if err := tx.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM invoice_line_items
				WHERE invoice_id = $1 AND tenant_id = $2 AND commit_granted_cents IS NOT NULL
			)`, invoiceID, tenantID,
		).Scan(&hasCommit); err != nil {
			return domain.InvoiceLineItem{}, domain.Invoice{}, fmt.Errorf("check existing commit line: %w", err)
		}
		if hasCommit {
			return domain.InvoiceLineItem{}, domain.Invoice{},
				errs.Invalid("commit_granted_cents", "invoice already has a commit line — one commit per invoice; use a separate invoice")
		}
	}

	item.InvoiceID = invoiceID
	item.TenantID = tenantID
	item.Currency = currency

	itemID := postgres.NewID("vlx_ili")
	now := clock.Now(ctx)
	metaJSON, _ := json.Marshal(item.Metadata)
	if item.Metadata == nil {
		metaJSON = []byte("{}")
	}

	err = tx.QueryRowContext(ctx, `
		INSERT INTO invoice_line_items (id, invoice_id, tenant_id, line_type, meter_id,
			description, quantity, unit_amount_cents, amount_cents, tax_rate, tax_amount_cents,
			total_amount_cents, currency, pricing_mode, rating_rule_version_id,
			billing_period_start, billing_period_end, metadata, created_at,
			tax_jurisdiction, tax_code, tax_reason, quantity_decimal,
			commit_granted_cents, commit_expires_at, nominal_unit_amount_decimal, meter_unit)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27)
		RETURNING id, invoice_id, tenant_id, line_type, COALESCE(meter_id,''), description,
			quantity, unit_amount_cents, amount_cents, tax_rate, tax_amount_cents,
			total_amount_cents, currency, COALESCE(pricing_mode,''),
			COALESCE(rating_rule_version_id,''), billing_period_start, billing_period_end,
			metadata, created_at, tax_jurisdiction, tax_code, tax_reason, quantity_decimal,
			commit_granted_cents, commit_expires_at
	`, itemID, invoiceID, tenantID, item.LineType, postgres.NullableString(item.MeterID),
		item.Description, item.Quantity, item.UnitAmountCents, item.AmountCents,
		item.TaxRate, item.TaxAmountCents, item.TotalAmountCents, currency,
		postgres.NullableString(item.PricingMode), postgres.NullableString(item.RatingRuleVersionID),
		postgres.NullableTime(item.BillingPeriodStart), postgres.NullableTime(item.BillingPeriodEnd),
		metaJSON, now, item.TaxJurisdiction, item.TaxCode, item.TaxabilityReason, item.QuantityDecimal,
		item.CommitGrantedCents, postgres.NullableTime(item.CommitExpiresAt),
		nominalRateArg(item.NominalUnitAmountDecimal), postgres.NullableString(item.MeterUnit),
	).Scan(&item.ID, &item.InvoiceID, &item.TenantID, &item.LineType, &item.MeterID,
		&item.Description, &item.Quantity, &item.UnitAmountCents, &item.AmountCents,
		&item.TaxRate, &item.TaxAmountCents, &item.TotalAmountCents, &item.Currency,
		&item.PricingMode, &item.RatingRuleVersionID,
		&item.BillingPeriodStart, &item.BillingPeriodEnd, &metaJSON, &item.CreatedAt,
		&item.TaxJurisdiction, &item.TaxCode, &item.TaxabilityReason, &item.QuantityDecimal,
		&item.CommitGrantedCents, &item.CommitExpiresAt)
	if err != nil {
		return domain.InvoiceLineItem{}, domain.Invoice{}, err
	}
	_ = json.Unmarshal(metaJSON, &item.Metadata)

	// Recompute subtotal from ALL line items now in the tx (including the one
	// just inserted), then rewrite derived totals. Using a correlated subquery
	// in one UPDATE so the read and write stay in the same snapshot.
	var inv domain.Invoice
	err = tx.QueryRowContext(ctx, `
		UPDATE invoices i SET
			subtotal_cents = sub.subtotal,
			total_amount_cents = sub.subtotal + i.tax_amount_cents - i.discount_cents,
			amount_due_cents = GREATEST(
				sub.subtotal + i.tax_amount_cents - i.discount_cents
					- i.amount_paid_cents - i.credits_applied_cents, 0),
			updated_at = $2
		FROM (
			SELECT COALESCE(SUM(amount_cents), 0)::BIGINT AS subtotal
			FROM invoice_line_items WHERE invoice_id = $1
		) sub
		WHERE i.id = $1
		RETURNING `+invCols,
		invoiceID, now,
	).Scan(s.scanInvDest(&inv)...)
	if err != nil {
		return domain.InvoiceLineItem{}, domain.Invoice{}, err
	}

	// Both writes landed (the RETURNING scans above would have errored
	// otherwise) — this is a genuinely mutated path, so the row is evidence
	// of a mutation that actually happened.
	if emit != nil {
		if err := emit(tx, item, inv); err != nil {
			return domain.InvoiceLineItem{}, domain.Invoice{}, fmt.Errorf("audit emission: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return domain.InvoiceLineItem{}, domain.Invoice{}, err
	}
	return item, inv, nil
}

func (s *PostgresStore) UpdateTaxAtomic(
	ctx context.Context, tenantID, invoiceID string,
	update domain.InvoiceTaxRetryUpdate, lineItems []domain.InvoiceLineItem,
) (domain.Invoice, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.Invoice{}, err
	}
	defer postgres.Rollback(tx)

	var (
		status    domain.InvoiceStatus
		taxStatus string
	)
	err = tx.QueryRowContext(ctx,
		`SELECT status, tax_status FROM invoices WHERE id = $1 FOR UPDATE`,
		invoiceID,
	).Scan(&status, &taxStatus)
	if err == sql.ErrNoRows {
		return domain.Invoice{}, errs.ErrNotFound
	}
	if err != nil {
		return domain.Invoice{}, err
	}
	// Draft-only is the genuine data invariant: tax is mutable while an
	// invoice is a draft and frozen once finalized. The FOR UPDATE lock above
	// re-asserts it under concurrency. The narrower "tax_status in (pending,
	// failed)" restriction is retry-specific POLICY, not an invariant —
	// RetryTaxForInvoice enforces it at the engine layer before reaching here.
	// ComputeTaxForInvoice (finalize-time tax for manual invoices) legitimately
	// runs against a fresh draft whose tax_status is still 'ok', so the store
	// guards only what it must: draft.
	if status != domain.InvoiceDraft {
		return domain.Invoice{}, errs.InvalidState(fmt.Sprintf(
			"invoice tax is only mutable on draft invoices (current: %s)", status))
	}
	_ = taxStatus // selected for the FOR UPDATE lock; no longer gated here

	now := clock.Now(ctx)

	for _, li := range lineItems {
		if li.ID == "" {
			continue
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE invoice_line_items
			SET tax_rate = $3,
			    tax_amount_cents = $4,
			    total_amount_cents = $5,
			    tax_jurisdiction = $6,
			    tax_code = $7,
			    tax_reason = $8
			WHERE invoice_id = $1 AND id = $2
		`, invoiceID, li.ID, li.TaxRate, li.TaxAmountCents, li.TotalAmountCents,
			li.TaxJurisdiction, li.TaxCode, li.TaxabilityReason)
		if err != nil {
			return domain.Invoice{}, fmt.Errorf("update line item tax stamp: %w", err)
		}
	}

	var inv domain.Invoice
	err = tx.QueryRowContext(ctx, `
		UPDATE invoices SET
			tax_amount_cents = $2,
			tax_rate = $3,
			tax_name = $4,
			tax_country = $5,
			tax_id = $6,
			tax_provider = $7,
			tax_calculation_id = $8,
			tax_reverse_charge = $9,
			tax_exempt_reason = $10,
			tax_status = $11,
			tax_deferred_at = $12,
			tax_pending_reason = $13,
			tax_error_code = $14,
			tax_retry_count = tax_retry_count + CASE WHEN $20 THEN 0 ELSE 1 END,
			tax_next_retry_at = $15,
			total_amount_cents = $16,
			subtotal_cents = $18,
			discount_cents = $19,
			amount_due_cents = GREATEST($16 - amount_paid_cents - credits_applied_cents, 0),
			updated_at = $17
		WHERE id = $1
		RETURNING `+invCols,
		invoiceID,
		update.TaxAmountCents, update.TaxRate,
		update.TaxName, update.TaxCountry, update.TaxID,
		// tax_provider / tax_calculation_id are NOT NULL DEFAULT '' (the
		// INSERT path binds them raw). NullableString would map the manual
		// provider's empty calculation id to SQL NULL and trip the NOT NULL
		// constraint, so bind raw here too.
		update.TaxProvider,
		update.TaxCalculationID,
		update.TaxReverseCharge, update.TaxExemptReason,
		string(update.TaxStatus), postgres.NullableTime(update.TaxDeferredAt),
		update.TaxPendingReason, postgres.NullableString(update.TaxErrorCode),
		postgres.NullableTime(update.TaxNextRetryAt),
		update.TotalAmountCents, now,
		// Net subtotal/discount from the tax application — carved out of the
		// gross in tax-inclusive mode, pass-through (== stored header) in
		// exclusive mode. Keeps subtotal − discount + tax == gross.
		update.SubtotalCents, update.DiscountCents,
		update.InitialAttempt,
	).Scan(s.scanInvDest(&inv)...)
	if err != nil {
		return domain.Invoice{}, err
	}

	if err := tx.Commit(); err != nil {
		return domain.Invoice{}, err
	}
	return inv, nil
}

// CreateWithLineItems creates an invoice and all its line items in a single
// atomic transaction. This prevents orphaned invoices with missing line items.
// The unique index on (tenant_id, subscription_id, billing_period_start, billing_period_end)
// provides idempotency — duplicate calls return an error.
func (s *PostgresStore) CreateWithLineItems(ctx context.Context, tenantID string, inv domain.Invoice, items []domain.InvoiceLineItem) (domain.Invoice, error) {
	return s.CreateWithLineItemsAudited(ctx, tenantID, inv, items, nil)
}

// CreateWithLineItemsAudited is CreateWithLineItems with an in-tx audit
// emission (ADR-090): emit runs on the SAME transaction as the header + line
// INSERTs, after they all land and BEFORE commit, so an emission error rolls
// the whole composed invoice back. The two manual-create shapes (bare header
// vs create-with-lines) therefore audit identically — a draft created either
// way leaves exactly one 'create' row. nil emit = unaudited write (engine
// cycle/threshold invoices).
func (s *PostgresStore) CreateWithLineItemsAudited(
	ctx context.Context, tenantID string, inv domain.Invoice, items []domain.InvoiceLineItem,
	emit func(tx *sql.Tx, out domain.Invoice) error,
) (domain.Invoice, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.Invoice{}, err
	}
	defer postgres.Rollback(tx)
	out, err := s.createWithLineItemsInTx(ctx, tx, tenantID, inv, items)
	if err != nil {
		return domain.Invoice{}, err
	}
	if emit != nil {
		if err := emit(tx, out); err != nil {
			return domain.Invoice{}, fmt.Errorf("audit emission: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return domain.Invoice{}, err
	}
	return out, nil
}

// CreateWithLineItemsTx is the in-transaction variant used by the
// subscription handler's atomic AddItem-with-proration flow: the caller
// has already opened a tx wrapping the sub-item insert, and the
// proration invoice insert needs to share that tx so a failure here
// rolls back the item add too. ADR-030 atomic-proration follow-through.
func (s *PostgresStore) CreateWithLineItemsTx(ctx context.Context, tx *sql.Tx, tenantID string, inv domain.Invoice, items []domain.InvoiceLineItem) (domain.Invoice, error) {
	return s.createWithLineItemsInTx(ctx, tx, tenantID, inv, items)
}

// createWithLineItemsInTx is the shared body. The exported wrappers
// differ only in tx ownership.
func (s *PostgresStore) createWithLineItemsInTx(ctx context.Context, tx *sql.Tx, tenantID string, inv domain.Invoice, items []domain.InvoiceLineItem) (domain.Invoice, error) {
	id := postgres.NewID("vlx_inv")
	// Canonical UPPERCASE ISO-4217 (see Create) — normalize the header and
	// every line so origin (manual vs cycle) can't change the casing and
	// the stored value matches the tenant default the analytics/dunning
	// revenue queries filter on case-sensitively.
	inv.Currency = strings.ToUpper(inv.Currency)
	for i := range items {
		items[i].Currency = strings.ToUpper(items[i].Currency)
	}
	// Honor caller-provided CreatedAt — engine paths pass clk.Now()
	// so test-clock-driven invoices align with their issued_at /
	// due_at on simulation time. Zero falls back to wall-clock.
	now := inv.CreatedAt
	if now.IsZero() {
		now = clock.Now(ctx)
	}
	metaJSON, _ := json.Marshal(inv.Metadata)
	if inv.Metadata == nil {
		metaJSON = []byte("{}")
	}

	taxStatus := inv.TaxStatus
	if taxStatus == "" {
		taxStatus = domain.InvoiceTaxOK
	}
	// Public token is a property of the finalized state. Service.Finalize
	// mints one for the operator-driven path; the billing engine
	// (engine.go + threshold_scan.go) inserts directly with status=
	// finalized and previously skipped the mint, leaving every
	// engine-generated invoice without a hosted_invoice_url. That breaks
	// every customer-facing email CTA. Mint here so the invariant
	// "finalized ⇒ has public_token" holds at the data boundary
	// regardless of which caller produced the invoice. A generation
	// failure is non-fatal — the row still inserts; operators can
	// repair via the rotate endpoint.
	publicToken := inv.PublicToken
	if publicToken == "" && inv.Status == domain.InvoiceFinalized {
		if t, tokenErr := GeneratePublicToken(); tokenErr == nil {
			publicToken = t
		}
	}
	// Store the token as an encrypted blob (reversible, for re-send URLs) + a
	// SHA-256 blind index (for lookup). Never store the raw token.
	encToken, tokenHash := s.encodeToken(publicToken)
	err := tx.QueryRowContext(ctx, `
		INSERT INTO invoices (id, tenant_id, customer_id, subscription_id, invoice_number,
			status, payment_status, currency, subtotal_cents, discount_cents, tax_amount_cents,
			tax_rate, tax_name, tax_country, tax_id,
			total_amount_cents, amount_due_cents, amount_paid_cents, credits_applied_cents,
			billing_period_start, billing_period_end, issued_at, due_at,
			net_payment_term_days, memo, footer, metadata, created_at, updated_at,
			source_plan_changed_at, source_subscription_item_id, source_change_type,
			tax_provider, tax_calculation_id, tax_reverse_charge, tax_exempt_reason,
			tax_status, tax_deferred_at, tax_retry_count, tax_pending_reason, tax_error_code, billing_reason,
			stripe_invoice_id, public_token_encrypted, public_token_hash, paid_at, voided_at, is_simulated,
			billing_timezone, auto_charge_pending)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$28,$29,$30,$31,$32,$33,$34,$35,$36,$37,$38,$39,$40,$41,$42,$43,$44,$45,$46,$47,$48,$49)
		RETURNING `+invCols,
		id, tenantID, inv.CustomerID, postgres.NullableString(inv.SubscriptionID), inv.InvoiceNumber,
		inv.Status, inv.PaymentStatus, inv.Currency,
		inv.SubtotalCents, inv.DiscountCents, inv.TaxAmountCents, inv.TaxRate,
		inv.TaxName,
		inv.TaxCountry, inv.TaxID,
		inv.TotalAmountCents, inv.AmountDueCents, inv.AmountPaidCents, inv.CreditsAppliedCents,
		inv.BillingPeriodStart, inv.BillingPeriodEnd,
		postgres.NullableTime(inv.IssuedAt), postgres.NullableTime(inv.DueAt),
		inv.NetPaymentTermDays, postgres.NullableString(inv.Memo),
		postgres.NullableString(inv.Footer), metaJSON, now,
		postgres.NullableTime(inv.SourcePlanChangedAt),
		postgres.NullableString(inv.SourceSubscriptionItemID),
		postgres.NullableString(string(inv.SourceChangeType)),
		inv.TaxProvider, inv.TaxCalculationID, inv.TaxReverseCharge, inv.TaxExemptReason,
		string(taxStatus), postgres.NullableTime(inv.TaxDeferredAt), inv.TaxRetryCount, inv.TaxPendingReason,
		postgres.NullableString(inv.TaxErrorCode),
		postgres.NullableString(string(inv.BillingReason)),
		postgres.NullableString(inv.StripeInvoiceID),
		postgres.NullableString(encToken), postgres.NullableString(tokenHash),
		postgres.NullableTime(inv.PaidAt), postgres.NullableTime(inv.VoidedAt),
		inv.IsSimulated,
		postgres.NullableString(inv.BillingTimezone),
		collectionQueuedAtFinalize(inv.Status, inv.PaymentStatus),
	).Scan(s.scanInvDest(&inv)...)

	if err != nil {
		if mapped := mapInvoiceUniqueViolation(err, inv); mapped != nil {
			return domain.Invoice{}, mapped
		}
		return domain.Invoice{}, err
	}
	// Born issued (cycle close, threshold, cancel final, day-1, proration):
	// copy the parties in this same transaction (ADR-109).
	if inv.Status != domain.InvoiceDraft {
		if err := s.stampIssuedPartiesTx(ctx, tx, &inv); err != nil {
			return domain.Invoice{}, err
		}
	}

	// Invoice-level commit rules (ADR-078): commit lines live only on
	// manual invoices, at most one per invoice. Enforced here so the
	// composer's atomic create-with-lines path carries the same rules as
	// AddLineItemAtomic (buildLineItem validates only line-local rules).
	commitLines := 0
	for i := range items {
		if items[i].IsCommitLine() {
			commitLines++
		}
	}
	if commitLines > 0 && inv.BillingReason != domain.BillingReasonManual {
		return domain.Invoice{}, errs.Invalid("commit_granted_cents", "commit purchase lines are only supported on manual invoices")
	}
	if commitLines > 1 {
		return domain.Invoice{}, errs.Invalid("commit_granted_cents", "invoice already has a commit line — one commit per invoice; use a separate invoice")
	}

	// Create all line items within the same transaction
	for i := range items {
		items[i].InvoiceID = inv.ID
		itemID := postgres.NewID("vlx_ili")
		itemMetaJSON, _ := json.Marshal(items[i].Metadata)
		if items[i].Metadata == nil {
			itemMetaJSON = []byte("{}")
		}

		_, err = tx.ExecContext(ctx, `
			INSERT INTO invoice_line_items (id, invoice_id, tenant_id, line_type, meter_id,
				description, quantity, unit_amount_cents, amount_cents, tax_rate,
				tax_amount_cents, total_amount_cents, currency, pricing_mode,
				rating_rule_version_id, billing_period_start, billing_period_end, metadata, created_at,
				tax_jurisdiction, tax_code, tax_reason, quantity_decimal,
				commit_granted_cents, commit_expires_at, nominal_unit_amount_decimal, meter_unit)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27)
		`, itemID, inv.ID, tenantID, items[i].LineType, postgres.NullableString(items[i].MeterID),
			items[i].Description, items[i].Quantity, items[i].UnitAmountCents, items[i].AmountCents,
			items[i].TaxRate, items[i].TaxAmountCents, items[i].TotalAmountCents,
			items[i].Currency, postgres.NullableString(items[i].PricingMode),
			postgres.NullableString(items[i].RatingRuleVersionID),
			postgres.NullableTime(items[i].BillingPeriodStart), postgres.NullableTime(items[i].BillingPeriodEnd),
			itemMetaJSON, now, items[i].TaxJurisdiction, items[i].TaxCode, items[i].TaxabilityReason, items[i].QuantityDecimal,
			items[i].CommitGrantedCents, postgres.NullableTime(items[i].CommitExpiresAt),
			nominalRateArg(items[i].NominalUnitAmountDecimal), postgres.NullableString(items[i].MeterUnit),
		)
		if err != nil {
			return domain.Invoice{}, fmt.Errorf("create line item %d: %w", i, err)
		}
	}

	return inv, nil
}

// collectionQueuedAtFinalize is the auto_charge_pending value an invoice gets
// in the same write that makes it finalized and owed. Recording the intent
// there, not after the commit, is what makes collection crash-safe: an invoice
// can't be finalized and unpaid without the auto-charge sweep seeing it. Before
// this, every finalize path committed with the flag off and set it only when
// its inline charge failed, so a crash between the commit and the charge left
// an owed invoice that no sweep, email, or dunning run ever saw.
//
// The amount is deliberately not part of the rule. A $0 invoice is queued too,
// and the collector settles it as paid with no charge.
func collectionQueuedAtFinalize(status domain.InvoiceStatus, paymentStatus domain.InvoicePaymentStatus) bool {
	return status == domain.InvoiceFinalized && paymentStatus == domain.PaymentPending
}

// noLiveHostedPageSQL keeps every Velox charge off an invoice the customer
// may be paying on the hosted page: one payer at a time
// (domain.HostedCheckoutLiveSQL says when a page counts as live).
var noLiveHostedPageSQL = "NOT " + domain.HostedCheckoutLiveSQL("invoices.id")

// lockInvoiceForChargeClaim takes the invoice row lock before a charge
// claim's UPDATE. Without it the UPDATE could miss a hosted session created
// concurrently: the hosted claim (payment.CheckoutSessionStore.ClaimOpen)
// holds FOR SHARE on this row while it inserts the session, but never changes
// the row, so an UPDATE whose snapshot predates that commit waits for the
// lock and then proceeds on its old snapshot, blind to the new session.
// Taking the lock in its own statement means the UPDATE that follows reads a
// snapshot taken after any such commit. false = the row is not visible.
func lockInvoiceForChargeClaim(ctx context.Context, tx *sql.Tx, id string) (bool, error) {
	var one int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM invoices WHERE id = $1 FOR UPDATE`, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// chargeHoldFree is the claim predicate on the hold (ADR-127): only a NULL
// hold is free. An expired hold is NOT free — it means a charge may have been
// sent and its result was not saved, and only the stale-hold sweep, after
// asking Stripe, may clear it. Before ADR-127 the claims took an expired hold
// as free (`IS NULL OR < now()`), so an interrupted charge left the invoice
// looking exactly like one never charged and the next initiator charged it
// again (#976). Shared by every claim so they cannot drift apart.
const chargeHoldFree = `auto_charge_claimed_until IS NULL`

// chargeHoldReleaseTimeout bounds a release detached from its caller's
// context (ReleaseChargeHold): one UPDATE, so a stalled database cannot hold
// a request or a shutdown for longer than this.
const chargeHoldReleaseTimeout = 10 * time.Second

// chargeHoldTTLSecs is domain.ChargeHoldTTL in the form the claims' SQL takes,
// so the TTL has one source (TestChargeHoldTiming pins the relations).
var chargeHoldTTLSecs = domain.ChargeHoldTTL.Seconds()

// claimChargeHold runs one claim: lock the row, then the UPDATE whose
// predicate is the claim's eligibility. It returns the hold value written —
// the claim's token — or the zero ChargeHold when the claim was refused.
func (s *PostgresStore) claimChargeHold(ctx context.Context, tenantID, id, eligibility string) (domain.ChargeHold, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.ChargeHold{}, err
	}
	defer postgres.Rollback(tx)

	if ok, err := lockInvoiceForChargeClaim(ctx, tx, id); err != nil || !ok {
		return domain.ChargeHold{}, err
	}
	var until time.Time
	err = tx.QueryRowContext(ctx, `
		UPDATE invoices
		SET auto_charge_claimed_until = now() + make_interval(secs => $2)
		WHERE id = $1
		  AND `+eligibility+`
		  AND `+chargeHoldFree+`
		  AND `+noLiveHostedPageSQL+`
		RETURNING auto_charge_claimed_until
	`, id, chargeHoldTTLSecs).Scan(&until)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ChargeHold{}, nil
	}
	if err != nil {
		return domain.ChargeHold{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.ChargeHold{}, err
	}
	return domain.ChargeHold{Until: until}, nil
}

// ClaimAutoCharge takes the per-invoice charge hold (HA hazard #1,
// migration 0141) for the auto-charge sweep: a 5-minute claim that admits
// exactly one collector into the charge leg per invoice. The full
// eligibility predicate is re-asserted so the claim doubles as a freshness
// gate — under READ COMMITTED the UPDATE re-evaluates against the latest
// committed row, so an invoice settled by a webhook (or marked failed/unknown
// by a rival leader) between list and claim fails the claim rather than being
// charged stale. Since ADR-127 it also refuses while ANY hold is set,
// including an expired one (chargeHoldFree).
//
// Two load-bearing choices:
//   - updated_at is NOT touched. It feeds the attention banner's age
//     ("since"), the reconciler sweeps' staleness cool-offs, and the past-due
//     view's proxies — a claim write that touched it would make every
//     claimed invoice look freshly-active once per window.
//   - DB-side now() on both sides of the hold comparison: test clocks freeze
//     simulated time, and a simulated-time hold would never expire on a
//     frozen clock (the catchup path shares this claim).
//
// Deliberately per-invoice, not stamped at list time — a batch-wide hold
// cannot cover a serial 50-invoice loop, and a claiming LIST would hide rows
// from dunning enrollment, which shares the list methods (adversarial review,
// 2026-07-06). The list stays claim-blind: a held row is listed and skipped
// here when its claim fails.
func (s *PostgresStore) ClaimAutoCharge(ctx context.Context, tenantID, id string) (domain.ChargeHold, error) {
	return s.claimChargeHold(ctx, tenantID, id, `auto_charge_pending = TRUE
		  AND payment_status = 'pending'
		  AND status = 'finalized'`)
}

// ReleaseChargeHold clears the hold its owner took, and only that hold: the
// UPDATE compares the value the owner's claim wrote (ADR-127). An owner that
// stalled past its hold, while the stale-hold sweep cleared it and another
// owner claimed, would otherwise erase that owner's hold — and with it the
// only trace that its charge may have been sent.
//
// Owners call it on paths that provably never reached Stripe (credit-apply
// failure, no payment method, a settle that needed no charge, the charge's
// re-check refusing, the breaker open, unreadable credentials), after a
// DEFINITE outcome (a decline, a settled success), and dunning's final action
// after its subscription action. An unclear outcome (timeout, unknown, 429)
// never releases: the call may have reached Stripe, and the stale-hold sweep
// asks Stripe before anything can charge again. No updated_at write (see
// ClaimAutoCharge).
//
// It runs to completion even when the caller's context has ended: Collect
// releases on the request's context (an operator closing the tab) and the
// auto-charge sweep on the tick's (a shutdown). A release lost that way left
// an invoice whose charge was never sent refused by every charge, void and
// offline payment until the stale-hold sweep asked Stripe, an hour or more.
func (s *PostgresStore) ReleaseChargeHold(ctx context.Context, tenantID, id string, hold domain.ChargeHold) error {
	if !hold.Held() {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), chargeHoldReleaseTimeout)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)

	if _, err := tx.ExecContext(ctx,
		`UPDATE invoices SET auto_charge_claimed_until = NULL WHERE id = $1 AND auto_charge_claimed_until = $2`,
		id, hold.Until); err != nil {
		return err
	}
	return tx.Commit()
}

// ChargeHeldTx reports the invoice's hold: none, live, or stale (ADR-127),
// and the hold value. The caller must hold the invoice's row lock on tx —
// every claim takes that lock before it writes the hold, so the answer cannot
// change before tx commits. Read on the database's clock (the claims write the
// hold with now(), so a Go-side compare would mix clocks). Void, write-off and
// offline payment refuse on it (#975, PaymentBlocksActionHeld).
func (s *PostgresStore) ChargeHeldTx(ctx context.Context, tx *sql.Tx, id string) (domain.ChargeHoldState, domain.ChargeHold, error) {
	var until sql.NullTime
	var live bool
	if err := tx.QueryRowContext(ctx,
		`SELECT auto_charge_claimed_until, COALESCE(auto_charge_claimed_until > now(), false) FROM invoices WHERE id = $1`, id,
	).Scan(&until, &live); err != nil {
		if err == sql.ErrNoRows {
			return domain.ChargeHoldNone, domain.ChargeHold{}, errs.ErrNotFound
		}
		return domain.ChargeHoldNone, domain.ChargeHold{}, fmt.Errorf("read charge hold: %w", err)
	}
	switch {
	case !until.Valid:
		return domain.ChargeHoldNone, domain.ChargeHold{}, nil
	case live:
		return domain.ChargeHoldLive, domain.ChargeHold{Until: until.Time}, nil
	default:
		return domain.ChargeHoldStale, domain.ChargeHold{Until: until.Time}, nil
	}
}

// ChargeHoldStateOf reads the invoice's hold outside any lock. Only for
// choosing what to tell an operator whose claim was refused; never a gate.
func (s *PostgresStore) ChargeHoldStateOf(ctx context.Context, tenantID, id string) (domain.ChargeHoldState, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.ChargeHoldNone, err
	}
	defer postgres.Rollback(tx)
	state, _, err := s.ChargeHeldTx(ctx, tx, id)
	return state, err
}

// ConfirmChargeHold is the charge's re-check right before it sends
// (ADR-127): true only while the owner's hold is still the one it wrote and
// has at least domain.ChargeSendMargin left, the charge counter is still the
// one its idempotency key was built from, and the invoice is still finalized
// and unpaid. The charge sends nothing on false.
//
//   - Hold: a charger that stalled past its hold (the sweep cleared it and
//     another owner may be charging) must not send. With the send budget
//     (domain.ChargeSendBudget <= ChargeSendMargin) no request for an attempt
//     can start after its hold ends, which is what lets the sweep read
//     "Stripe has nothing" as final.
//   - Seq and status: a webhook for an older PaymentIntent can settle the
//     invoice while the charger stalls; without these the charge went out on
//     its stale snapshot.
func (s *PostgresStore) ConfirmChargeHold(ctx context.Context, tenantID, id string, hold domain.ChargeHold, attemptSeq int64) (bool, error) {
	if !hold.Held() {
		return false, nil
	}
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return false, err
	}
	defer postgres.Rollback(tx)

	var ok bool
	err = tx.QueryRowContext(ctx, `
		SELECT COALESCE(
			auto_charge_claimed_until = $2
			AND auto_charge_claimed_until > now() + make_interval(secs => $3)
			AND charge_attempt_seq = $4
			AND status = 'finalized' AND payment_status IN ('pending', 'failed'),
			false)
		FROM invoices WHERE id = $1
	`, id, hold.Until, domain.ChargeSendMargin.Seconds(), attemptSeq).Scan(&ok)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("confirm charge hold: %w", err)
	}
	return ok, nil
}

// ClaimChargeForDunningRetry is the dunning-side twin of ClaimAutoCharge
// (HA hazard #11): both charge paths share ONE hold, so the auto-charge sweep
// and a due dunning retry can never hold the same invoice concurrently —
// their Stripe idempotency keys differ BY CONSTRUCTION (the purpose suffix),
// so Stripe cannot dedupe them and mutual exclusion here is the only guard.
//
// Predicate differences from the sweep claim, both deliberate:
//   - no auto_charge_pending requirement (dunning retries fire off the
//     run schedule, not the flag);
//   - payment_status IN ('pending','failed') — 'pending' covers the
//     card-less-enrolled invoice whose card just attached, 'failed' is
//     the normal retry state. 'unknown' is EXCLUDED on purpose: an
//     ambiguous outcome may be a real payment and must wait for the
//     reconciler, never be blind re-charged.
//
// Same hold/no-updated_at rules as ClaimAutoCharge (see there).
func (s *PostgresStore) ClaimChargeForDunningRetry(ctx context.Context, tenantID, id string) (domain.ChargeHold, error) {
	return s.claimChargeHold(ctx, tenantID, id, `payment_status IN ('pending', 'failed')
		  AND status = 'finalized'
		  AND amount_due_cents > 0`)
}

// ClaimChargeForManualCollect is the operator-collect twin on the SAME hold
// (2026-07-21 snapshot-race audit): before this claim existed,
// POST /invoices/{id}/collect charged its handler snapshot with no mutual
// exclusion against the auto-charge sweep or a due dunning retry. Identical
// predicate to the dunning claim: 'unknown' stays excluded (a
// possibly-succeeded payment is the reconciler's, never blind re-charged).
//
// finalized ONLY. This claim briefly (2026-08-05, ADR-110 amendment) also
// admitted `uncollectible`; ADR-113 removed it on industry evidence:
// charge-the-written-off-object is a Stripe-only pattern. A written-off
// invoice is settleable by RECORDING writers only (offline payment, the
// stale-hold sweep's settle) — nothing initiates money against
// it. Kept a separate function from the dunning claim so a future divergence
// is a deliberate edit, not a shared-hold accident.
func (s *PostgresStore) ClaimChargeForManualCollect(ctx context.Context, tenantID, id string) (domain.ChargeHold, error) {
	return s.claimChargeHold(ctx, tenantID, id, `payment_status IN ('pending', 'failed')
		  AND status = 'finalized'
		  AND amount_due_cents > 0`)
}

// ClaimChargeHoldForFinalAction takes the hold for dunning's final action
// (ADR-112 amendment, ADR-127): while the subscription is canceled or paused
// nothing can start a charge on the invoice, and the final action does not
// run at all while a hold is set (a charge running, or one whose result was
// lost). It closes two races: canceling a customer whose lost charge
// succeeded, and the auto-charge sweep charging a no-card invoice whose card
// was just attached while dunning cancels its subscription. No amount
// condition: an open $0 invoice must still be held off any charge. A credit
// note that leaves nothing due marks the invoice paid unless a hold is set
// (SettleZeroDueTx), so this claim meets an open $0 invoice only when a note
// was issued under a hold, or when credits covered a retry whose mark-paid
// failed.
// The cancel runs under domain.WithChargeHoldOwner so it can still void this
// invoice when it is a wholly unused prebill. Its relief credit note, issued
// under this hold, leaves the invoice open even when nothing is left due:
// the write-off that follows needs it open.
func (s *PostgresStore) ClaimChargeHoldForFinalAction(ctx context.Context, tenantID, id string) (domain.ChargeHold, error) {
	return s.claimChargeHold(ctx, tenantID, id, `payment_status IN ('pending', 'failed')
		  AND status = 'finalized'`)
}

// staleHoldRecheckAfter is how long a checked hold waits before the sweep
// asks Stripe about it again. A kept hold's check stamps provider_synced_at
// (MarkStaleHoldChecked), so a hold whose check failed rotates to the back of
// the queue instead of heading it forever (the 0167 rotation). Shorter than
// the hourly production tick on purpose: a stamp taken during one tick must
// not hide the row from the next.
const staleHoldRecheckAfter = 30 * time.Minute

// ListStaleChargeHolds is the stale-hold sweep's input (ADR-127): invoices
// whose hold ended at least domain.StaleChargeHoldWait ago, in ANY status —
// a lost charge's payment can still be found after the invoice was settled,
// written off or voided some other way, and the sweep must flag it rather
// than leave the hold forever. Each row carries the hold value and the
// customer's Stripe customer id.
//
// Parked invoices are among them: a park keeps its claim's hold, so this is
// how the sweep finds every park (REC-5). PastParkedWait says whether the hold
// ended more than domain.ParkedNothingWait ago, on the clock the hold was
// written with.
//
// TxBypass across tenants with an explicit livemode, like the payment sweeps'
// lists. Not gated on is_simulated: the hold is always written on the
// database's wall clock, and a lost simulated charge is as real at Stripe as
// any other (ADR-030's payment-sync exception).
//
// Open invoices (finalized, pending or failed) first: only their holds refuse
// anyone (charges, the hosted page, void, offline payment, dunning), so when
// the sweep's time budget cuts a run, a settled invoice's leftover hold never
// takes an open one's place. Then oldest check first (never-checked first),
// then oldest hold. skip is the ids the sweep has already checked in this
// run: it reads page after page and checks each hold once, even one it could
// not stamp.
func (s *PostgresStore) ListStaleChargeHolds(ctx context.Context, limit int, skip []string) ([]domain.StaleChargeHold, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	if limit <= 0 {
		limit = 50
	}
	// COALESCE: a nil skip list can reach Postgres as NULL, and
	// NOT (id = ANY(NULL)) would filter out every row (see postgres.StringArray).
	rows, err := tx.QueryContext(ctx, `
		SELECT `+qualifiedInvCols("i")+`, i.auto_charge_claimed_until, COALESCE(c.stripe_customer_id, ''),
		       i.auto_charge_claimed_until < now() - make_interval(secs => $6)
		FROM invoices i
		LEFT JOIN customers c ON c.id = i.customer_id AND c.tenant_id = i.tenant_id
		WHERE i.auto_charge_claimed_until < now() - make_interval(secs => $1)
		  AND i.livemode = $2
		  AND (i.provider_synced_at IS NULL OR i.provider_synced_at < now() - make_interval(secs => $3))
		  AND NOT (i.id = ANY(COALESCE($5::text[], '{}')))
		ORDER BY (i.status = 'finalized' AND i.payment_status IN ('pending', 'failed')) DESC,
		         i.provider_synced_at ASC NULLS FIRST, i.auto_charge_claimed_until ASC, i.id ASC
		LIMIT $4
	`, domain.StaleChargeHoldWait.Seconds(), postgres.Livemode(ctx), staleHoldRecheckAfter.Seconds(), limit, postgres.StringArray(skip),
		domain.ParkedNothingWait.Seconds())
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []domain.StaleChargeHold
	for rows.Next() {
		var h domain.StaleChargeHold
		dest := append(s.scanInvDest(&h.Invoice), &h.Hold, &h.StripeCustomerID, &h.PastParkedWait)
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ClearStaleChargeHold is the stale-hold sweep's one write to the hold
// (ADR-127): a compare-and-set on the hold value and the charge counter it
// read, so a concurrent outcome (a webhook stamp, the sweep running twice)
// makes it a no-op instead of clearing a hold it no longer speaks for.
//
// moveSeq is set when Stripe was shown to hold nothing for the lost charge
// and the invoice is still open: "no payment was created" is the attempt's
// outcome, so the counter moves and the next charge sends a fresh idempotency
// key (ADR-105 amendment). Reusing the old key could be handed a 500 Stripe
// saved under it, or a 409 when credits changed the amount.
func (s *PostgresStore) ClearStaleChargeHold(ctx context.Context, tenantID, id string, hold time.Time, attemptSeq int64, moveSeq bool) (bool, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return false, err
	}
	defer postgres.Rollback(tx)

	var res sql.Result
	if moveSeq {
		res, err = tx.ExecContext(ctx, `
			UPDATE invoices
			SET auto_charge_claimed_until = NULL, charge_attempt_seq = charge_attempt_seq + 1
			WHERE id = $1 AND auto_charge_claimed_until = $2 AND charge_attempt_seq = $3
			  AND status = 'finalized' AND payment_status IN ('pending', 'failed')
		`, id, hold, attemptSeq)
	} else {
		res, err = tx.ExecContext(ctx, `
			UPDATE invoices
			SET auto_charge_claimed_until = NULL
			WHERE id = $1 AND auto_charge_claimed_until = $2 AND charge_attempt_seq = $3
		`, id, hold, attemptSeq)
	}
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return n == 1, nil
}

// MarkStaleHoldChecked stamps provider_synced_at for a stale hold the sweep
// asked about and kept, so the row rotates (staleHoldRecheckAfter). Only the
// time: provider_payment_status stays the verbatim PaymentIntent status the
// payment sweeps last read, and no money column or updated_at is touched
// (see RecordProviderSync).
//
// A parked invoice is stamped too: this sweep is the only one that checks it
// (REC-5), and unstamped it would come back on every tick ahead of the holds
// already checked. Not an 'unknown' invoice that names its PaymentIntent:
// there the column is the unknown sweep's rotation key, and that sweep stamps
// it on its own checks.
func (s *PostgresStore) MarkStaleHoldChecked(ctx context.Context, tenantID, id string) error {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)

	if _, err := tx.ExecContext(ctx, `
		UPDATE invoices SET provider_synced_at = now()
		WHERE id = $1 AND NOT (payment_status = 'unknown' AND COALESCE(stripe_payment_intent_id, '') <> '')
	`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// parkedNoPaymentReason is a parked attempt's reason once its park ends with
// no payment at Stripe (UnparkAfterNoPayment).
const parkedNoPaymentReason = "Stripe showed no payment for this attempt 24 hours later."

// UnparkAfterNoPayment is the stale-hold sweep's write when Stripe shows no
// payment for a parked invoice's charge domain.ParkedNothingWait after the
// attempt (REC-5, ADR-127 amendment 2026-10-20). It puts the invoice back
// where a charge picks it up and returns the status it set:
//
//   - 'failed' when the parked attempt was a dunning retry, so the run retries
//     it at once (the parked attempt was never counted);
//   - 'pending' otherwise: an auto-charge, Collect, or no attempt row (its
//     write is best-effort). Collect on a 'failed' invoice comes back
//     'pending': the status shown differs, who may charge does not.
//
// The park overwrote the status it came from, and no column keeps it; the
// parked attempt's row says who made it. last_payment_error is cleared: after
// a park it holds Stripe's 500 message, which the failure banner would print
// as the bank's words. The counter moves, so the next charge sends a new
// idempotency key and never gets back a 500 Stripe saved under the parked
// one (ADR-105 amendment). The hold is cleared in the same statement, so no
// claim ever sees one change without the other.
//
// A compare-and-set on the hold, the counter and the parked shape, so a
// webhook or another sweep that changed the invoice since the sweep read it
// makes this a no-op (false). Written-off parks are included: nothing charges
// an uncollectible invoice (ADR-113), so for them this only ends the park.
//
// In the same transaction the parked attempt's row is recorded as failed,
// with parkedNoPaymentReason as its reason: Velox now knows the attempt took
// no money, and the timeline would otherwise say "result unknown" forever.
// The saved answer is not kept as the reason. For a timed-out call it is Go's
// error (`Post "https://api.stripe.com/v1/payment_intents": context deadline
// exceeded`), and for a 500 or a 409 it is Stripe's words about the request,
// not about the payment; the Activity card prints the reason as the problem.
func (s *PostgresStore) UnparkAfterNoPayment(ctx context.Context, tenantID, id string, hold time.Time, attemptSeq int64) (domain.InvoicePaymentStatus, bool, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return "", false, err
	}
	defer postgres.Rollback(tx)

	var status domain.InvoicePaymentStatus
	err = tx.QueryRowContext(ctx, `
		UPDATE invoices SET
			auto_charge_claimed_until = NULL,
			charge_attempt_seq = charge_attempt_seq + 1,
			last_payment_error = NULL,
			updated_at = now(),
			-- Last: TestChargeAttemptSeqBumpedByEveryPIStamp reads the SET
			-- list up to the first WHERE, and the subquery has one.
			payment_status = CASE WHEN (
				SELECT a.trigger_source FROM invoice_charge_attempts a
				WHERE a.invoice_id = invoices.id AND a.outcome = 'unknown' AND a.stripe_payment_intent_id = ''
				ORDER BY a.created_at DESC, a.id DESC LIMIT 1) = 'dunning_retry'
			THEN 'failed' ELSE 'pending' END
		WHERE id = $1 AND auto_charge_claimed_until = $2 AND charge_attempt_seq = $3
		  AND payment_status = 'unknown' AND COALESCE(stripe_payment_intent_id, '') = ''
		  AND status IN ('finalized', 'uncollectible')
		RETURNING payment_status
	`, id, hold, attemptSeq).Scan(&status)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE invoice_charge_attempts SET outcome = 'failed', provider_reason = $2, updated_at = now()
		WHERE invoice_id = $1 AND outcome = 'unknown' AND stripe_payment_intent_id = ''
	`, id, parkedNoPaymentReason); err != nil {
		return "", false, err
	}
	if err := tx.Commit(); err != nil {
		return "", false, err
	}
	return status, true, nil
}

// SetAutoChargePending marks an invoice for scheduler-based auto-charge retry.
func (s *PostgresStore) SetAutoChargePending(ctx context.Context, tenantID, id string, pending bool) error {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)

	_, err = tx.ExecContext(ctx, `UPDATE invoices SET auto_charge_pending = $1, updated_at = $2 WHERE id = $3`,
		pending, clock.Now(ctx), id)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// SetAutoChargePendingTx is the tx-aware variant: it runs the enrollment UPDATE on
// the caller's (RLS-scoped) tx so it commits atomically with the surrounding change
// — used by the atomic proration item-change path so enrollment can't fail as a
// separate post-commit write and strand a committed charge invoice unenrolled.
func (s *PostgresStore) SetAutoChargePendingTx(ctx context.Context, tx *sql.Tx, tenantID, id string, pending bool) error {
	_, err := tx.ExecContext(ctx, `UPDATE invoices SET auto_charge_pending = $1, updated_at = $2 WHERE id = $3`,
		pending, clock.Now(ctx), id)
	return err
}

// SetTaxTransaction persists the upstream tax_transaction reference
// (Stripe: tx_xxx) after a successful CommitTax. Required by the credit
// note reversal path, which keys the reversal on the original
// transaction id.
func (s *PostgresStore) SetTaxTransaction(ctx context.Context, tenantID, id string, taxTransactionID string) error {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)

	_, err = tx.ExecContext(ctx, `UPDATE invoices SET tax_transaction_id = $1, updated_at = $2 WHERE id = $3`,
		taxTransactionID, clock.Now(ctx), id)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// notPausedForCollection is the SQL predicate both auto-charge sweeps use to
// honour `pause_collection`. A paused subscription means the operator (or
// dunning's `pause` final action) stopped automatic collection on it, so the
// sweeps must not charge its invoices — "pause_collection neuters the
// financial side without touching the cycle".
//
// The engine's own finalize sites already skip collection for a paused sub,
// which used to make this unreachable by accident: nothing set
// auto_charge_pending on a paused sub's invoice, so the sweeps never saw one.
// That stopped being true once the tax-retry chain queued the invoices it
// auto-finalizes — a paused sub's tax-deferred draft finalizes on retry and
// would otherwise have been charged, contradicting the pause. Enforcing it in
// the predicate keeps the rule with the collection owner rather than with
// whichever writer happened to set the flag.
//
// One-off invoices (no subscription) are unaffected — NOT EXISTS holds when
// there is nothing to join to.
//
// The pause is read as in force at `at` (an SQL expression for the sweep's
// now): a timed pause whose resumes_at has passed no longer holds collection,
// even while the row still carries it. The scheduled resumer leaves it there
// until the sub's periods ending before resumes_at are closed (a parked
// billing failure can hold it indefinitely), and those closes read it; the
// sub's other invoices resume at resumes_at. Mirrors
// domain.PauseCollection.InForceAt.
func notPausedForCollection(alias, at string) string {
	return `NOT EXISTS (
			SELECT 1 FROM subscriptions ps
			 WHERE ps.id = ` + alias + `.subscription_id
			   AND ps.pause_collection_behavior IS NOT NULL
			   AND (ps.pause_collection_resumes_at IS NULL OR ps.pause_collection_resumes_at > ` + at + `)
		)`
}

// ListAutoChargePending returns one page of invoices queued for the
// auto-charge sweep, after the cursor `after`, oldest first — CRON path. The
// engine pages through the whole queue every tick. A single oldest-first page
// was the bug this replaced: card-less invoices stay queued (so a card added
// later is charged on the next visit), and 50 of them filled the page for
// good, so nothing newer was ever retried.
//
// Excludes clock-pinned subscriptions per ADR-029: simulation
// time progresses only on operator Advance, so the wall-clock scheduler
// must never charge a clock-pinned invoice. The catchup worker uses
// ListAutoChargePendingForClock as the disjoint per-clock entry point.
func (s *PostgresStore) ListAutoChargePending(ctx context.Context, after domain.InvoiceKeyset, limit int) ([]domain.Invoice, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	if limit <= 0 {
		limit = 50
	}

	// TxBypass crosses tenants for the scheduler sweep; livemode must still
	// be honoured explicitly from ctx (see scheduler fan-out in #13).
	//
	// Simulated (test-clock) invoices are excluded by their OWN durable
	// is_simulated flag, not by a subscriptions join. The join missed a
	// customer-pinned one-off (empty subscription_id → nothing to join
	// through), which then leaked into this wall-clock sweep and got
	// auto-charged/dunned against sim time (ADR-029 violation). Gating on
	// is_simulated catches every simulated invoice — sub-owned or one-off
	// — while the catchup path (ListAutoChargePendingForClock) charges the
	// simulated ones in sim time. Mirrors ListPendingTaxReversal's gate.
	rows, err := tx.QueryContext(ctx, `
		SELECT `+invCols+` FROM invoices i
		WHERE i.auto_charge_pending = TRUE
		  AND i.payment_status = 'pending'
		  AND i.status = 'finalized'
		  AND i.livemode = $1
		  AND i.is_simulated = false
		  AND `+notPausedForCollection("i", "now()")+`
		  AND (i.created_at, i.id) > ($3, $4)
		ORDER BY i.created_at ASC, i.id ASC
		LIMIT $2
	`, postgres.Livemode(ctx), limit, after.CreatedAt, after.ID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var invoices []domain.Invoice
	for rows.Next() {
		var inv domain.Invoice
		if err := rows.Scan(s.scanInvDest(&inv)...); err != nil {
			return nil, err
		}
		invoices = append(invoices, inv)
	}
	return invoices, rows.Err()
}

// ListFailedWithoutDunningRun powers the dunning_backfill reconciler: finalized,
// still-owed invoices in payment_status='failed' that have NO dunning run at all.
// SettleFailed starts dunning POST-COMMIT (best-effort), so a crash — or an
// exhausted StartDunning retry — in that window leaves the invoice
// failed-but-undunned. A later report of the same decline re-drives the start,
// but nothing guarantees one arrives (the charge call's response is the only
// report when the webhook is lost). This sweep hands those invoices back to the
// idempotent StartDunning so they still reach a terminal.
//
// The NOT EXISTS on invoice_dunning_runs is STATE-AGNOSTIC (no state filter):
// StartDunning is exactly-once per invoice (GetRunByInvoice returns the existing
// run regardless of state; 0085 UNIQUE is the DB backstop), so an invoice with ANY
// run — active, escalated, or resolved — must be excluded, else the sweep would
// re-dun a resolved invoice forever. Clock-pinned invoices are excluded (the
// wall-clock scheduler must never dun a simulated sub; ADR-029 — their dunning is
// driven inline during Advance). Cross-tenant sweep (TxBypass); livemode honoured
// from ctx. olderThan is the cool-off so the inline SettleFailed path wins the
// common case and this stays a pure backstop.
func (s *PostgresStore) ListFailedWithoutDunningRun(ctx context.Context, olderThan time.Time, limit int) ([]domain.Invoice, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	if limit <= 0 {
		limit = 50
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT `+invCols+` FROM invoices i
		WHERE i.payment_status = 'failed'
		  AND i.status = 'finalized'
		  AND i.amount_due_cents > 0
		  AND i.livemode = $1
		  AND i.updated_at < $2
		  AND NOT EXISTS (
		    SELECT 1 FROM invoice_dunning_runs r WHERE r.invoice_id = i.id
		  )
		  AND i.is_simulated = false
		ORDER BY i.updated_at ASC
		LIMIT $3
	`, postgres.Livemode(ctx), olderThan, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var invoices []domain.Invoice
	for rows.Next() {
		var inv domain.Invoice
		if err := rows.Scan(s.scanInvDest(&inv)...); err != nil {
			return nil, err
		}
		invoices = append(invoices, inv)
	}
	return invoices, rows.Err()
}

// ListAutoChargePendingForClock is the catchup-path counterpart to
// ListAutoChargePending. Returns invoices whose owning CUSTOMER is
// pinned to the given clock and need auto-charge retry. ADR-029 Phase 1.
//
// Clock membership resolves through customers, NOT a subscriptions
// join: the pin is customer-level (ADR-027; a sub only ever inherits
// its customer's clock), and an inner subscriptions join silently
// dropped one-off invoices (subscription_id NULL) — the wall sweep
// correctly skips them via is_simulated, so a sub-joined catchup left
// them stranded UNCHARGEABLE forever (found live in FLOW TC4: a
// clock-pinned one-off with a card on file survived two wall ticks
// AND an Advance uncharged). Same fix applied to
// ListPendingTaxRetryForClock below; every other ForClock candidate
// query already resolves via customers.
//
// Time predicate is implicit: the catchup worker calls this AFTER it
// has finalized any newly-due periods for the same clock, so any
// auto_charge_pending row that exists is by definition due for a
// retry attempt. No separate "due_at" filter — cycle math is owned by
// the period-generation phase.
//
// Scoped by tenantID + clockID; livemode is implied (test clocks are
// test-mode-only, enforced by the test_clocks CHECK constraint).
func (s *PostgresStore) ListAutoChargePendingForClock(ctx context.Context, tenantID, clockID string, after domain.InvoiceKeyset, limit int) ([]domain.Invoice, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	if limit <= 0 {
		limit = 50
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT `+qualifiedInvCols("i")+` FROM invoices i
		JOIN customers c ON c.id = i.customer_id
		WHERE i.auto_charge_pending = TRUE
		  AND i.payment_status = 'pending'
		  AND i.status = 'finalized'
		  AND i.tenant_id = $1
		  AND c.test_clock_id = $2
		  AND `+notPausedForCollection("i", "(SELECT tc.frozen_time FROM test_clocks tc WHERE tc.id = $2)")+`
		  AND (i.created_at, i.id) > ($4, $5)
		ORDER BY i.created_at ASC, i.id ASC
		LIMIT $3
	`, tenantID, clockID, limit, after.CreatedAt, after.ID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var invoices []domain.Invoice
	for rows.Next() {
		var inv domain.Invoice
		if err := rows.Scan(s.scanInvDest(&inv)...); err != nil {
			return nil, err
		}
		invoices = append(invoices, inv)
	}
	return invoices, rows.Err()
}

// MarkPaidBatch atomically marks an invoice as paid (used by MarkPaid — kept for interface compat).
func (s *PostgresStore) MarkPaidBatch(ctx context.Context, tenantID, id string, stripePaymentIntentID string, paidAt time.Time) (domain.Invoice, error) {
	return s.MarkPaid(ctx, tenantID, id, stripePaymentIntentID, paidAt)
}

// ListUnknownPayments returns invoices whose payment_status is 'unknown' and
// whose last update is older than `olderThan` — the reconciler's cooling
// window before querying Stripe for the authoritative outcome.
func (s *PostgresStore) ListUnknownPayments(ctx context.Context, olderThan time.Time, limit int) ([]domain.Invoice, error) {
	return s.listInflightPayments(ctx, domain.PaymentUnknown, olderThan, limit)
}

// RecordProviderSync stamps what the reconciler just observed at the provider:
// the verbatim PaymentIntent status and the moment we looked (migration 0167).
//
// It is deliberately the NARROWEST possible write. It does not touch
// payment_status, stripe_payment_intent_id, charge_attempt_seq or updated_at —
// which matters more than it looks:
//
//   - charge_attempt_seq seeds the Stripe idempotency key (ADR-105). Bumping it
//     from an OBSERVATION would rotate the key without an attempt having
//     happened, so the next retry could open a second PaymentIntent beside a
//     live one. That is the exact double charge ADR-107 exists to prevent.
//   - updated_at feeds the attention banner's "since" and this sweep's own
//     staleness gate. Touching it every 5 minutes would reset the operator's
//     age signal forever and make a stuck invoice look permanently fresh.
//
// So the poll records only what it learned, and the row's money state and age
// remain the settle path's to write.
func (s *PostgresStore) RecordProviderSync(ctx context.Context, tenantID, id, providerStatus string) error {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)

	if _, err := tx.ExecContext(ctx, `
		UPDATE invoices
		SET provider_payment_status = $1, provider_synced_at = now()
		WHERE id = $2
	`, providerStatus, id); err != nil {
		return err
	}
	return tx.Commit()
}

// ListProcessingPayments returns invoices stuck in payment_status 'processing'
// older than the cool-off — the dropped-webhook backstop (ADR-049 Phase 2).
// Same tenant-crossing + livemode-scoped shape as ListUnknownPayments.
func (s *PostgresStore) ListProcessingPayments(ctx context.Context, olderThan time.Time, limit int) ([]domain.Invoice, error) {
	return s.listInflightPayments(ctx, domain.PaymentProcessing, olderThan, limit)
}

// listInflightPayments lists invoices in one non-terminal payment_status older
// than the cool-off, for the reconciler sweep. TxBypass crosses tenants for the
// sweep; the livemode filter prevents test-mode rows from being reconciled
// under a live ctx (see #13). Status is a trusted internal enum (not user
// input), interpolated into the predicate.
//
// DELIBERATELY NOT gated on is_simulated, unlike every sibling invoice sweep:
// payment state-sync is the wall-clock plane by design (ADR-030's Stripe
// exception — PaymentIntent truth lives at Stripe on real time, even for
// clock-pinned invoices), and the catchup worker has no payment-sync phase,
// so excluding simulated rows here would strand an ambiguous simulated charge
// in unknown/processing forever. The settle path re-binds simulated time via
// BindEffectiveNow + the velox_anchor_at PI anchor, so recovered outcomes
// still stamp the simulated instant. See the ADR-030 wall-plane note.
func (s *PostgresStore) listInflightPayments(ctx context.Context, status domain.InvoicePaymentStatus, olderThan time.Time, limit int) ([]domain.Invoice, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	if limit <= 0 {
		limit = 50
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT `+invCols+` FROM invoices
		WHERE payment_status = $1
		  AND updated_at < $2
		  AND livemode = $3
		  -- A row with no PaymentIntent id is NOT reconcilable here: this
		  -- sweep resolves by querying Stripe for the id, and there is none.
		  -- Such a row is parked (ADR-107): the stale-hold sweep finishes it
		  -- from Stripe's list of the customer's payments (REC-5). Listed
		  -- here, under this oldest-first LIMIT, parked rows would head the
		  -- queue and keep a NEW ambiguous charge that DOES carry an id from
		  -- being reached (found by the honesty sweep, 2026-07-31).
		  AND COALESCE(stripe_payment_intent_id, '') <> ''
		-- Least-recently-observed first, never-observed ahead of everything
		-- (migration 0167). This replaced ORDER BY updated_at ASC, which was a
		-- liveness sink: the reconciler writes NOTHING when it finds a
		-- non-terminal PaymentIntent, so such a row's updated_at froze and it
		-- headed this LIMITed queue forever — once batch-size of them
		-- accumulated the sweep returned only stuck rows and never reached a
		-- NEW ambiguous charge that Stripe resolves in one call. Now every poll
		-- stamps provider_synced_at, so a row that cannot be resolved rotates to
		-- the back and the queue round-robins.
		--
		-- Ordering, NOT exclusion, is the fix on purpose: these rows must keep
		-- being polled (the sweep is what notices when the customer finally
		-- authenticates), and every exclusion added during this arc risked
		-- hiding a row from the very code that would end its wait.
		ORDER BY provider_synced_at ASC NULLS FIRST, updated_at ASC
		LIMIT $4
	`, string(status), olderThan, postgres.Livemode(ctx), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var invoices []domain.Invoice
	for rows.Next() {
		var inv domain.Invoice
		if err := rows.Scan(s.scanInvDest(&inv)...); err != nil {
			return nil, err
		}
		invoices = append(invoices, inv)
	}
	return invoices, rows.Err()
}

// ListPendingTaxRetry powers the background tax-retry reconciler
// (ADR-017). RLS-bypassed because the sweeper crosses tenants;
// returned rows carry their tenant_id so the caller dispatches
// per-row under the correct RLS partition.
//
// Filter:
//   - status='draft' AND tax_status = pending
//   - tax_error_code IN retryableCodes (e.g. provider_outage,
//     unknown). Empty list short-circuits to "none".
//   - tax_retry_count < maxAttempts (per-invoice cap)
//   - tax_next_retry_at IS NULL OR tax_next_retry_at <= now()
//
// Postgres uses idx_invoices_tax_retry_due (migration 0074) to
// narrow the scan; the predicate matches the index where clause
// exactly so the planner picks it.
// ListPendingTaxRetry — CRON path. ADR-029 Phase 2: clock-pinned
// invoices are excluded; the test-clock catchup orchestrator drives
// tax retry for clock-pinned subs via ListPendingTaxRetryForClock.
// Without this filter the wall-clock scheduler would retry tax on a
// clock-pinned invoice every tick — same drip-bill smell ADR-028
// closed for period generation.
func (s *PostgresStore) ListPendingTaxRetry(ctx context.Context, batch int, retryableCodes []string, maxAttempts int, livemode bool) ([]domain.Invoice, error) {
	if batch <= 0 {
		batch = 50
	}
	if len(retryableCodes) == 0 {
		return nil, nil
	}
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	rows, err := tx.QueryContext(ctx, `
		SELECT `+invCols+` FROM invoices i
		WHERE i.livemode = $1
		  AND i.status = 'draft'
		  AND i.tax_status = 'pending'
		  AND COALESCE(i.tax_error_code, '') = ANY($2)
		  AND i.tax_retry_count < $3
		  AND (i.tax_next_retry_at IS NULL OR i.tax_next_retry_at <= now())
		  AND i.is_simulated = false
		ORDER BY i.tax_next_retry_at ASC NULLS FIRST
		LIMIT $4
	`, livemode, postgres.StringArray(retryableCodes), maxAttempts, batch)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []domain.Invoice
	for rows.Next() {
		var inv domain.Invoice
		if err := rows.Scan(s.scanInvDest(&inv)...); err != nil {
			return nil, err
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

// ListPendingTaxCommit finds finalized/paid/voided/uncollectible stripe_tax
// invoices whose Stripe Tax calculation succeeded (tax_status=ok,
// tax_calculation_id set) but whose tax_transaction_id never got persisted.
// Includes non-finalized terminals because the orphan can outlive 'finalized'
// when finalize + auto-charge run synchronously (billOnePeriod) and flip the
// invoice to 'paid' before the reconciler's first scan — the orphan state
// left when
// CommitTax's Stripe commit succeeded upstream but the local SetTaxTransaction
// write failed (or the process crashed in between). Powers the commit
// reconciler, which re-commits each to recover the transaction id (idempotency
// makes Stripe return the original transaction, never a duplicate).
//
// Bounded to invoices touched within the last 24h: beyond that the Stripe Tax
// calculation has expired, so the engine's expiry guard blocks the re-commit
// anyway — chasing them every tick would only log churn. An aged-out orphan
// needs a manual tax re-calc.
//
// Clock-pinned invoices are excluded — but, unlike ListPendingTaxRetry (which
// has a real catchup counterpart, ListPendingTaxRetryForClock), there is NO
// commit-reconciler ForClock half: nothing auto-recovers a clock-pinned commit
// orphan. This is a deliberate scope cut, not a TODO, because clock-pinned
// invoices are test-mode by construction (test clocks are livemode=false by DB
// CHECK constraint, so CommitTax runs against the tenant's sk_test_ Stripe
// account with no real tax authority). A stranded tax_transaction_id here only
// breaks *simulated* reversal fidelity on a *simulated* void — never real
// reporting — and the operator recovers it for free by replaying the clock.
// Build RetryPendingTaxCommitForClock (mirroring the #267 wall-clock reconciler
// + ADR-029's ForClock pattern) only when a design partner's test-clock
// void/credit-note simulation reports a silently no-op'd tax reversal.
func (s *PostgresStore) ListPendingTaxCommit(ctx context.Context, batch int, livemode bool) ([]domain.Invoice, error) {
	if batch <= 0 {
		batch = 50
	}
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	rows, err := tx.QueryContext(ctx, `
		SELECT `+invCols+` FROM invoices i
		WHERE i.livemode = $1
		  -- NOT only 'finalized': on the synchronous finalize+auto-charge path
		  -- the invoice flips to 'paid' in the same flow before any scheduler
		  -- tick, so a finalized-only filter misses that orphan and a later
		  -- credit-note/void can't reverse its tax (the reversal guards key on
		  -- a non-empty tax_transaction_id) → the tenant over-remits. CommitTax
		  -- is status-agnostic + idempotent, so re-committing a paid/voided row
		  -- is safe; the 24h window below bounds it to Stripe's calc TTL.
		  AND i.status IN ('finalized', 'paid', 'voided', 'uncollectible')
		  AND i.tax_status = 'ok'
		  AND i.tax_provider = 'stripe_tax'
		  AND COALESCE(i.tax_calculation_id, '') <> ''
		  AND COALESCE(i.tax_transaction_id, '') = ''
		  AND i.updated_at > now() - interval '24 hours'
		  AND i.is_simulated = false
		ORDER BY i.updated_at ASC
		LIMIT $2
	`, livemode, batch)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []domain.Invoice
	for rows.Next() {
		var inv domain.Invoice
		if err := rows.Scan(s.scanInvDest(&inv)...); err != nil {
			return nil, err
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

// ListPendingTaxReversal finds voided/uncollectible stripe_tax invoices that
// still carry a committed tax_transaction_id but have tax_reversed_at NULL — a
// reversal that failed (transient Stripe error) or never completed, leaving the
// upstream transaction reported as collected → the tenant over-remits. Powers
// RetryPendingTaxReversal, the symmetric sibling of the #267 commit reconciler.
//
// NOT time-bounded, and that is the difference from ListPendingTaxCommit.
//
// The commit sweep's `updated_at > now() - 24h` matches a HARD ceiling — the
// Stripe Tax calculation TTL, after which a re-commit is impossible, so rows
// outside the window are genuinely unrecoverable and skipping them costs
// nothing. A REVERSAL has no such ceiling: it stays possible forever.
//
// This sweep carried the same 24h anyway, for anti-churn plus a one-time
// grandfather of pre-0122 voids. The cost of that was measured on the live
// database while writing ADR-111: 4 voided invoices holding $46.69 of
// committed tax, each with tax_reversed_at NULL and each past the window —
// permanently stranded, silently, with the tenant over-remitting real money
// and no surface anywhere naming them. The "loud ERROR at the inline
// failure" the window relied on as the operator signal had scrolled out of
// the logs months earlier; aging out is not a recovery model when nothing
// durable records what aged out.
//
// Removing the window is safe because the set is self-draining and the drive
// is idempotent: success stamps tax_reversed_at and the row leaves for good,
// and the invoice-stable `inv_taxrev_<id>` reference dedups at Stripe, so a
// row that WAS reversed but failed to stamp re-confirms harmlessly and stamps
// out on the next tick. That also retires the grandfather rationale — a
// pre-0122 void reversed inline simply re-confirms once and drops out. The
// remaining predicates (voided + committed transaction + NULL stamp) already
// bound the set to what genuinely needs work, and ORDER BY + LIMIT bound the
// batch.
//
// The one case that now retries forever instead of aging out is the
// mode-drift limitation below, which Stripe rejects on every re-drive. That
// is a permanent per-tick ERROR rather than silent money loss — the honest
// trade, and the same direction as every other no-silent-fallback call on
// this path.
//
// LIMITATION (mode-drift): reverseInvoiceTax recomputes the reversal amount
// (total − credited) on each re-drive. If the invoice's credit-noted total
// CHANGES after the status flip while a reversal is pending-failed (e.g. an
// operator issues a credit note on an uncollectible invoice), the re-drive's
// body differs under the SAME idempotency key and Stripe rejects it — surfaced
// as the loud per-tick ERROR (→ manual reconcile), never a silent corruption.
// We deliberately do NOT vary the key by mode/amount: the G1 fix relies on the
// stable `inv_taxrev_<id>` reference to dedup an uncollectible-then-void pair to
// ONE reversal, so a mode-keyed scheme would reintroduce that double-reversal
// under-remit. Snapshotting the request body at status-change time is the clean
// fix if a real tax-filing DP hits this; deferred (pre-existing to the
// same-reference design, and zero live tax tenants today).
//
// Simulated (test-clock) invoices are excluded via is_simulated — the durable
// write-time flag (ADR-030), which covers BOTH subscription-pinned and
// customer-pinned one-off invoices (a subscriptions-only NOT EXISTS would miss
// the latter). Like ListPendingTaxCommit there is no ForClock half: a stranded
// reversal on a test-mode (livemode=false) void only breaks SIMULATED fidelity,
// never real tax reporting, and replaying the clock recovers it. Build a
// ForClock counterpart only when a DP's test-clock void simulation reports a
// silently no-op'd reversal.
func (s *PostgresStore) ListPendingTaxReversal(ctx context.Context, batch int, livemode bool) ([]domain.Invoice, error) {
	if batch <= 0 {
		batch = 50
	}
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	rows, err := tx.QueryContext(ctx, `
		SELECT `+invCols+` FROM invoices i
		WHERE i.livemode = $1
		  -- VOIDED ONLY (ADR-111). A write-off has no tax leg, so an
		  -- uncollectible invoice has nothing pending. No hole:
		  -- uncollectible -> voided is a permitted transition and the void
		  -- bumps updated_at, so a failed void-path reversal is still swept.
		  AND i.status = 'voided'
		  AND i.tax_provider = 'stripe_tax'
		  AND COALESCE(i.tax_transaction_id, '') <> ''
		  AND i.tax_reversed_at IS NULL
		  AND i.is_simulated = false
		ORDER BY i.updated_at ASC
		LIMIT $2
	`, livemode, batch)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []domain.Invoice
	for rows.Next() {
		var inv domain.Invoice
		if err := rows.Scan(s.scanInvDest(&inv)...); err != nil {
			return nil, err
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

// MarkTaxReversed stamps tax_reversed_at once the upstream reversal is confirmed
// (or there was nothing to reverse), dropping the invoice from the reversal
// sweep. Idempotent: the `IS NULL` guard makes a re-confirm a no-op.
func (s *PostgresStore) MarkTaxReversed(ctx context.Context, tenantID, id string) error {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)

	if _, err := tx.ExecContext(ctx,
		`UPDATE invoices SET tax_reversed_at = $1 WHERE id = $2 AND tax_reversed_at IS NULL`,
		clock.Now(ctx), id,
	); err != nil {
		return err
	}
	return tx.Commit()
}

// ListPendingTaxRetryForClock is the catchup-path counterpart to
// ListPendingTaxRetry. ADR-029 Phase 2.
//
// Differences from the cron path:
//   - Scoped to (tenant, clock) — clock-pinned subs only.
//   - No `tax_next_retry_at <= now()` predicate. The catchup
//     orchestrator drives this exactly once per Advance, so backoff
//     scheduling against simulated instants would silently no-op
//     small advances and confuse operators (they click Advance
//     expecting "one shot per pending row"). The retry-count cap
//     (maxAttempts) still applies as a defense against runaway
//     retries chewing through the 10-min catchup ceiling.
//
// Design choice (operator-friendly over production-fidelity): each
// Advance gives every pending row exactly one retry attempt. An
// operator running through a tax-error rehearsal scenario clicks
// Advance, sees count go up by 1 per row, and can predict the
// behaviour without doing backoff arithmetic. Faithful per-window
// retry-sequence simulation (Stripe-parity event-walking) is
// deferred to a follow-up ADR — it's a niche use case operators
// don't typically run, while operator-confusion from "I clicked
// Advance and nothing happened" is a daily failure mode.
func (s *PostgresStore) ListPendingTaxRetryForClock(ctx context.Context, tenantID, clockID string, retryableCodes []string, maxAttempts, limit int) ([]domain.Invoice, error) {
	if limit <= 0 {
		limit = 50
	}
	if len(retryableCodes) == 0 {
		return nil, nil
	}
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	rows, err := tx.QueryContext(ctx, `
		SELECT `+qualifiedInvCols("i")+` FROM invoices i
		JOIN customers c ON c.id = i.customer_id
		WHERE i.tenant_id = $1
		  AND c.test_clock_id = $2
		  AND i.status = 'draft'
		  AND i.tax_status = 'pending'
		  AND COALESCE(i.tax_error_code, '') = ANY($3)
		  AND i.tax_retry_count < $4
		ORDER BY i.created_at ASC
		LIMIT $5
	`, tenantID, clockID, postgres.StringArray(retryableCodes), maxAttempts, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []domain.Invoice
	for rows.Next() {
		var inv domain.Invoice
		if err := rows.Scan(s.scanInvDest(&inv)...); err != nil {
			return nil, err
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

// ListProviderConfigErrors returns draft invoices stuck on Stripe-
// configuration tax errors (provider_not_configured / provider_auth)
// for one (tenant, livemode). Backs the
// tenantstripe.Connect → invoice.Service.RetryProviderConfigErrors
// fan-out per ADR-019. Tenant-scoped via TxTenant + WithLivemode on
// the request ctx; the per-mode filter is also explicit in the WHERE
// so a misconfigured ctx can't accidentally surface the wrong mode's
// rows.
func (s *PostgresStore) ListProviderConfigErrors(ctx context.Context, tenantID string, livemode bool) ([]domain.Invoice, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	rows, err := tx.QueryContext(ctx, `
		SELECT `+invCols+` FROM invoices
		WHERE livemode = $1
		  AND status = 'draft'
		  AND tax_status = 'pending'
		  AND COALESCE(tax_error_code, '') IN ('provider_not_configured', 'provider_auth')
		ORDER BY created_at ASC
		LIMIT 1000
	`, livemode)
	return s.scanInvoiceRows(rows, err)
}

// ListCustomerDataInvalidErrors returns draft invoices for ONE customer
// stuck on `customer_data_invalid` — the only tax error a billing-
// profile update can resolve. Mirrors ListProviderConfigErrors but
// scoped to a customer instead of a (tenant, livemode) — fired after
// the operator updates a customer's address/postal/state/tax_id so
// any of that customer's stuck invoices auto-retry without
// per-invoice clicking. Same surgical-filter principle as ADR-019.
func (s *PostgresStore) ListCustomerDataInvalidErrors(ctx context.Context, tenantID, customerID string) ([]domain.Invoice, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	rows, err := tx.QueryContext(ctx, `
		SELECT `+invCols+` FROM invoices
		WHERE customer_id = $1
		  AND status = 'draft'
		  AND tax_status = 'pending'
		  AND COALESCE(tax_error_code, '') = 'customer_data_invalid'
		ORDER BY created_at ASC
		LIMIT 1000
	`, customerID)
	return s.scanInvoiceRows(rows, err)
}

// scanInvoiceRows is the shared per-row scanning body of the two
// retry-fanout list queries above. Centralized so the close-on-error
// + scan loop don't drift between the two callers.
func (s *PostgresStore) scanInvoiceRows(rows *sql.Rows, queryErr error) ([]domain.Invoice, error) {
	if queryErr != nil {
		return nil, queryErr
	}
	defer func() { _ = rows.Close() }()

	var out []domain.Invoice
	for rows.Next() {
		var inv domain.Invoice
		if err := rows.Scan(s.scanInvDest(&inv)...); err != nil {
			return nil, err
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

// stampIssuedPartiesTx copies what the invoice prints about its buyer and
// its seller onto the row, on the transaction that takes it out of draft
// (ADR-109). Every SQL statement that issues an invoice calls it before
// commit: createWithLineItemsInTx and CreateAudited for invoices born issued,
// updateStatusInTx and FinalizeWithDates for drafts being finalized. So the
// copy and the status change commit together or not at all.
//
// The buyer tax ID is the invoice's own tax_id when set (the ID tax was
// calculated with), otherwise the profile's. That keeps one buyer tax ID on
// an issued document, from one place.
//
// The buyer's display name, email, legal name and tax ID are encrypted in the
// copy, as they are on the customer and billing-profile rows; the scan
// decrypts them (openBillTo).
//
// The same UPDATE copies the tenant's invoice footer into invoices.footer.
// The footer carries payment details and legal text, so an issued invoice
// keeps the one it was issued with, and the PDF and the hosted page print
// that copy.
//
// The UPDATE returns the row, so inv (and every caller that renders from the
// returned value instead of re-reading) carries the copy. A customer the
// transaction cannot see fails the issue instead of writing an empty copy.
func (s *PostgresStore) stampIssuedPartiesTx(ctx context.Context, tx *sql.Tx, inv *domain.Invoice) error {
	var billTo domain.DocumentBillTo
	var supplier domain.DocumentSupplier
	err := tx.QueryRowContext(ctx, domain.DocumentPartiesSQL, inv.CustomerID).
		Scan(domain.DocumentPartiesScanDest(&billTo, &supplier)...)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("issue invoice %s: customer %s not found for the issued-document copy", inv.ID, inv.CustomerID)
	}
	if err != nil {
		return fmt.Errorf("issue invoice %s: read parties for the issued-document copy: %w", inv.ID, err)
	}
	if billTo, err = billTo.Open(s.enc); err != nil {
		return fmt.Errorf("issue invoice %s: decrypt parties for the issued-document copy: %w", inv.ID, err)
	}
	if id := strings.TrimSpace(inv.TaxID); id != "" {
		billTo.TaxID = id
	}
	if billTo, err = billTo.Seal(s.enc); err != nil {
		return fmt.Errorf("issue invoice %s: encrypt bill-to copy: %w", inv.ID, err)
	}
	billJSON, err := json.Marshal(billTo)
	if err != nil {
		return fmt.Errorf("issue invoice %s: encode bill-to copy: %w", inv.ID, err)
	}
	supJSON, err := json.Marshal(supplier)
	if err != nil {
		return fmt.Errorf("issue invoice %s: encode supplier copy: %w", inv.ID, err)
	}
	if err := tx.QueryRowContext(ctx, `
		UPDATE invoices SET bill_to_snapshot = $1::jsonb, supplier_snapshot = $2::jsonb,
			footer = (SELECT NULLIF(ts.invoice_footer, '') FROM tenant_settings ts WHERE ts.tenant_id = invoices.tenant_id)
		WHERE id = $3
		RETURNING `+invCols,
		billJSON, supJSON, inv.ID,
	).Scan(s.scanInvDest(inv)...); err != nil {
		return fmt.Errorf("issue invoice %s: write issued-document copy: %w", inv.ID, err)
	}
	return nil
}

// openBillTo decrypts the PII fields of a scanned bill-to copy, so readers
// and the API get plaintext (stampIssuedPartiesTx seals them).
func (s *PostgresStore) openBillTo(b domain.DocumentBillTo) (domain.DocumentBillTo, error) {
	return b.Open(s.enc)
}

func (s *PostgresStore) scanInvDest(inv *domain.Invoice) []any {
	var metaJSON []byte
	return []any{
		&inv.ID, &inv.TenantID, &inv.CustomerID, &inv.SubscriptionID, &inv.InvoiceNumber,
		&inv.Status, &inv.PaymentStatus, &inv.Currency,
		&inv.SubtotalCents, &inv.DiscountCents, &inv.TaxAmountCents, &inv.TaxRate,
		&inv.TaxName, &inv.TaxCountry, &inv.TaxID,
		&inv.TotalAmountCents, &inv.AmountDueCents, &inv.AmountPaidCents, &inv.CreditsAppliedCents,
		&inv.BillingPeriodStart, &inv.BillingPeriodEnd,
		&inv.IssuedAt, &inv.DueAt, &inv.PaidAt, &inv.VoidedAt, &inv.UncollectibleAt,
		&inv.StripePaymentIntentID, &inv.LastPaymentError,
		&inv.AutoChargePending, &inv.NetPaymentTermDays, &inv.Memo, &inv.Footer,
		&metaJSON, &inv.CreatedAt, &inv.UpdatedAt, &inv.SourcePlanChangedAt,
		&inv.SourceSubscriptionItemID, (*string)(&inv.SourceChangeType),
		&inv.TaxProvider, &inv.TaxCalculationID, &inv.TaxTransactionID,
		&inv.TaxReverseCharge, &inv.TaxExemptReason,
		(*string)(&inv.TaxStatus), &inv.TaxDeferredAt, &inv.TaxRetryCount, &inv.TaxPendingReason,
		&inv.TaxErrorCode, &inv.TaxNextRetryAt,
		&inv.PaymentCardBrand, &inv.PaymentCardLast4,
		decryptScanner{enc: s.enc, dst: &inv.PublicToken}, (*string)(&inv.BillingReason), &inv.StripeInvoiceID,
		&inv.IsSimulated, &inv.TaxReversedAt,
		&inv.PaymentAnomalyKind, &inv.PaymentAnomalyPaymentIntentID, &inv.PaymentAnomalyCapturedCents,
		&inv.BillingTimezone, &inv.NoPMNotifiedAt, &inv.ChargeAttemptSeq,
		&inv.ProviderPaymentStatus, &inv.ProviderSyncedAt,
		domain.SnapshotScanner[domain.DocumentBillTo]{Dst: &inv.BillToSnapshot, Open: s.openBillTo},
		domain.SnapshotScanner[domain.DocumentSupplier]{Dst: &inv.SupplierSnapshot},
		&inv.BelowMinimumAt, &inv.PaidOutOfBand,
	}
}

// SetNoPMNotifiedAt stamps the send-once marker for the "payment method
// needed" setup-link email (ADR-087 follow-up). Written by whichever sender
// delivered the email — a finalize-time collect step or the auto-charge retry
// sweep — and checked by the sweep so a no-PM invoice is emailed exactly once,
// not once per tick and not twice across finalize + sweep.
func (s *PostgresStore) SetNoPMNotifiedAt(ctx context.Context, tenantID, invoiceID string, at time.Time) error {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)
	res, err := tx.ExecContext(ctx, `
		UPDATE invoices SET no_pm_notified_at = $1 WHERE id = $2
	`, at, invoiceID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errs.ErrNotFound
	}
	return tx.Commit()
}

// SetPublicToken writes (or overwrites) the hosted-invoice-URL token for a
// non-draft invoice. The token is the URL — non-guessable by design — so a
// rotation just swaps the column value. Drafts never carry a token, hence
// the status guard; callers that try to set one on a draft get ErrNotFound.
func (s *PostgresStore) SetPublicToken(ctx context.Context, tenantID, invoiceID, token string) error {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)

	encToken, tokenHash := s.encodeToken(token)
	// Public-token rotation is an operational metadata write (re-issue a
	// hosted-invoice URL), not a billing-domain transition — stamp updated_at
	// wall-clock via SQL now(), matching SetPaymentCard. Domain transitions
	// (status, tax, paid_at) are the ones that ride the test clock.
	res, err := tx.ExecContext(ctx, `
		UPDATE invoices SET public_token_encrypted = $1, public_token_hash = $2, updated_at = now()
		WHERE id = $3 AND status <> 'draft'
	`, postgres.NullableString(encToken), postgres.NullableString(tokenHash), invoiceID)
	if err != nil {
		if postgres.IsUniqueViolation(err) {
			// 256 bits of entropy means collisions are astronomically unlikely,
			// but if one ever surfaces we want loud failure, not silent reuse
			// of another invoice's URL.
			return fmt.Errorf("set public token: collision: %w", err)
		}
		return fmt.Errorf("set public token: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return errs.ErrNotFound
	}
	return tx.Commit()
}

// GetByPublicToken resolves a hosted-invoice-URL token to its invoice row.
// The caller is unauthenticated (a public /invoice/{token} route), so we
// have to look up the tenant FROM the token before any tenant context can
// exist. Runs under TxBypass for exactly that reason; the token's 256 bits
// of entropy plus the UNIQUE index make cross-tenant probing infeasible.
// Empty token returns ErrNotFound rather than querying a null match.
func (s *PostgresStore) GetByPublicToken(ctx context.Context, token string) (domain.Invoice, bool, error) {
	if token == "" {
		return domain.Invoice{}, false, errs.ErrNotFound
	}
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return domain.Invoice{}, false, err
	}
	defer postgres.Rollback(tx)

	var (
		inv      domain.Invoice
		livemode bool
	)
	dest := append(s.scanInvDest(&inv), &livemode)
	err = tx.QueryRowContext(ctx, `SELECT `+invCols+`, livemode FROM invoices WHERE public_token_hash = $1`, HashPublicToken(token)).
		Scan(dest...)
	if err == sql.ErrNoRows {
		return domain.Invoice{}, false, errs.ErrNotFound
	}
	if err != nil {
		return domain.Invoice{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return domain.Invoice{}, false, err
	}
	return inv, livemode, nil
}

// GetByStripeInvoiceID resolves a Stripe invoice id (in_xxx) to its imported
// Velox invoice row. Built as the idempotency lookup for the Stripe importer
// removed in the 2026-04-29 lean-cut (rebuild deferred until a design-partner
// cutover), so it has no production caller today. When an importer returns,
// re-running an import after a finalized invoice has already landed must
// detect the existing row and emit skip-equivalent (or skip-divergent)
// rather than ErrAlreadyExists from a unique-violation collision.
//
// Empty stripeInvoiceID returns ErrNotFound rather than matching the
// partial unique index's NULL gap (no Velox-native invoice should match).
// Runs under TxTenant — an importer always has a tenant in context, and
// scoping by tenant is the standard RLS posture for this store.
func (s *PostgresStore) GetByStripeInvoiceID(ctx context.Context, tenantID, stripeInvoiceID string) (domain.Invoice, error) {
	if stripeInvoiceID == "" {
		return domain.Invoice{}, errs.ErrNotFound
	}
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.Invoice{}, err
	}
	defer postgres.Rollback(tx)

	var inv domain.Invoice
	err = tx.QueryRowContext(ctx, `SELECT `+invCols+` FROM invoices WHERE stripe_invoice_id = $1`, stripeInvoiceID).
		Scan(s.scanInvDest(&inv)...)
	if err == sql.ErrNoRows {
		return domain.Invoice{}, errs.ErrNotFound
	}
	return inv, err
}

// baseFundsPeriod is the one status predicate for "this invoice charged the
// period's in_advance base", shared by FindBaseInvoiceForPeriod and
// FindFundingInvoicesForPeriod: every status but voided. A void annuls the
// charge. A write-off does not: it ends collection, not the sale (ADR-111), so
// a written-off invoice is billed but unpaid, and a mid-period change treats
// it exactly as it treats an unpaid one (ADR-050 amendment 2026-10-23). The
// threshold watermark keeps a written-off invoice for the same reason
// (thresholdBillsWindow).
const baseFundsPeriod = `i.status <> 'voided'`

// FindBaseInvoiceForPeriod is the proration-credit gate: returns the
// invoice carrying an in_advance base line for the given subscription
// period. The line's `billing_period_start` matches the period the
// base fee covers (industry-standard semantic — Stripe / Lago / Orb /
// Chargebee align). Caller reads invoice.PaymentStatus to decide
// whether a "refundable" credit (Chargebee Refundable) or "skip /
// adjust unpaid invoice" (Chargebee Adjustment / Stripe none) shape
// applies.
//
// A voided invoice is filtered out: it charged nothing. A written-off
// (uncollectible) one is kept (baseFundsPeriod): it is billed but unpaid, so
// an upgrade over it is refused as over an unpaid one (ADR-050 amendment
// 2026-10-23). ORDER BY created_at DESC + LIMIT 1 surfaces the
// most-recent matching invoice (relevant for plan-swap chains within
// the same period that re-issued the base line).
func (s *PostgresStore) FindBaseInvoiceForPeriod(ctx context.Context, tenantID, subscriptionID string, periodStart time.Time) (domain.Invoice, error) {
	if subscriptionID == "" {
		return domain.Invoice{}, errs.ErrNotFound
	}
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.Invoice{}, err
	}
	defer postgres.Rollback(tx)

	var inv domain.Invoice
	err = tx.QueryRowContext(ctx, `
		SELECT `+invCols+` FROM invoices i
		WHERE i.subscription_id = $1
		  AND `+baseFundsPeriod+`
		  AND EXISTS (
		    SELECT 1 FROM invoice_line_items li
		    WHERE li.invoice_id = i.id
		      AND li.line_type = 'base_fee'
		      AND li.billing_period_start = $2
		  )
		ORDER BY i.created_at DESC
		LIMIT 1
	`, subscriptionID, periodStart).Scan(s.scanInvDest(&inv)...)
	if err == sql.ErrNoRows {
		return domain.Invoice{}, errs.ErrNotFound
	}
	return inv, err
}

// FindFundingInvoicesForPeriod returns EVERY non-voided invoice that
// prepaid the cycle [periodStart, periodEnd) for a subscription —
// the day-1 base invoice AND any mid-period proration invoice that a plan
// upgrade / quantity increase issued against the same cycle. The single-result
// FindBaseInvoiceForPeriod misses the latter, because a mid-period proration
// invoice's base-fee LINES carry no billing_period_start (only its header is
// stamped, from changeAt) — so a period-wide credit sized off
// the post-change base (cancel / cross-interval plan-swap / downgrade clawback)
// overran that one invoice's credit-note cap and was silently dropped. The
// owed credit must instead be fanned across the full funding set, each piece
// capped at its own invoice. See ADR-031/ADR-048.
//
// Two predicates, UNION'd (deduped per invoice row):
//   - base invoice: a base_fee line whose billing_period_start = periodStart
//     (the existing FindBaseInvoiceForPeriod predicate).
//   - mid-period upgrade/qty proration invoice: identified by its fully-stamped
//     HEADER — billing_reason='subscription_update', source_plan_changed_at in
//     [periodStart, periodEnd), billing_period_end = periodEnd. (Its base-fee
//     lines' billing_period_start is NULL, so the line predicate can't see
//     it — the header is the reliable key.)
//
// A written-off (uncollectible) invoice is kept, as in FindBaseInvoiceForPeriod
// (baseFundsPeriod): it is billed but unpaid, so a cancel, swap or decrease
// credits it as it credits an unpaid one, lowering what it asks for, and a
// later payment pays only the time used (ADR-050 amendment 2026-10-23).
//
// Ordered base-first (subscription_update sorts last) then created_at ASC, so a
// caller allocating a fixed total across the set has a deterministic order for
// residual-cent assignment. Returns ErrNotFound when nothing funded the period
// (trial / pure in_arrears) so callers keep their existing no-op behavior.
func (s *PostgresStore) FindFundingInvoicesForPeriod(ctx context.Context, tenantID, subscriptionID string, periodStart, periodEnd time.Time) ([]domain.Invoice, error) {
	if subscriptionID == "" {
		return nil, errs.ErrNotFound
	}
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	rows, err := tx.QueryContext(ctx, `
		SELECT `+invCols+` FROM invoices i
		WHERE i.subscription_id = $1
		  AND `+baseFundsPeriod+`
		  AND (
		    EXISTS (
		      SELECT 1 FROM invoice_line_items li
		      WHERE li.invoice_id = i.id
		        AND li.line_type = 'base_fee'
		        AND li.billing_period_start = $2
		    )
		    OR (
		      i.billing_reason = 'subscription_update'
		      AND i.source_plan_changed_at >= $2
		      AND i.source_plan_changed_at < $3
		      AND i.billing_period_end = $3
		    )
		  )
		ORDER BY (i.billing_reason = 'subscription_update'), i.created_at ASC
	`, subscriptionID, periodStart, periodEnd)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []domain.Invoice
	for rows.Next() {
		var inv domain.Invoice
		if err := rows.Scan(s.scanInvDest(&inv)...); err != nil {
			return nil, err
		}
		out = append(out, inv)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, errs.ErrNotFound
	}
	return out, nil
}

// thresholdBillsWindow is the one predicate for "this threshold invoice
// billed its window": every status but voided (ADR-066 amendment
// 2026-10-23). A write-off ends collection, not the sale (ADR-111, ADR-113),
// and a written-off invoice can still be paid (an offline payment, a card
// charge that settles late), so its window stays billed: no later invoice
// bills it again. Only a void, which can never be paid, reopens it. Every
// watermark read below uses this constant, so the closers (cycle close and
// its in-tx re-read, immediate cancel, cross-interval swap), the swap's
// re-anchor guard, the operator preview, the live balance and the
// after_close classifier cannot disagree. Unqualified on purpose: every
// query that uses it reads one table.
const thresholdBillsWindow = `status <> 'voided'`

// thresholdAnyStatus is the fire-once probe's status predicate: none.
const thresholdAnyStatus = `TRUE`

// ThresholdWatermarks returns two instants for the subscription's cycle
// [periodStart, periodEnd), over the period's threshold invoices. Usage
// ingest reads them before it writes an event (usage.ThresholdWatermark):
//
//   - billedThrough is the closers' watermark: the latest
//     billing_period_end of a threshold invoice that still bills its window
//     (thresholdBillsWindow: not voided; a written-off one counts). Nil when
//     none.
//   - paidThrough is the latest billing_period_end of a paid one. Nil when
//     none. It never passes billedThrough, and it never falls back: a paid
//     invoice can be neither voided nor written off (updateStatusInTx's
//     allowed sources), so the close's watermark stays at or past it.
//
// billedThrough falls back only on a void, and a void is final: the cycle
// has one threshold invoice (idx_invoices_threshold_unique_per_cycle), so
// nothing raises it again in that cycle.
func (s *PostgresStore) ThresholdWatermarks(ctx context.Context, tenantID, subscriptionID string, periodStart, periodEnd time.Time) (billedThrough, paidThrough *time.Time, err error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, nil, err
	}
	defer postgres.Rollback(tx)

	var billed, paid sql.NullTime
	err = tx.QueryRowContext(ctx, `
		SELECT MAX(billing_period_end) FILTER (WHERE `+thresholdBillsWindow+`),
		       MAX(billing_period_end) FILTER (WHERE status = 'paid')
		FROM invoices
		WHERE subscription_id = $1
		  AND billing_reason = 'threshold'
		  AND billing_period_start >= $2
		  AND billing_period_start < $3
	`, subscriptionID, periodStart, periodEnd).Scan(&billed, &paid)
	if err != nil {
		return nil, nil, err
	}
	if billed.Valid {
		billedThrough = &billed.Time
	}
	if paid.Valid {
		paidThrough = &paid.Time
	}
	return billedThrough, paidThrough, nil
}

// GetLatestThresholdInvoiceForCycle returns the newest threshold-fired
// invoice for the cycle window [periodStart, periodEnd) that still bills its
// window (thresholdBillsWindow: not voided; a written-off one counts). It is
// the "billed through" watermark of every closer: usage before its
// billing_period_end landed on it, so the closer bills only the residual
// window; without it a threshold fire's window would be billed twice. A fire
// bills no base fee, so the invoice existing says nothing about the base:
// only one made before that change carries a base line, and the closers skip
// the base on that line alone (billing.thresholdWatermark.billedBase).
// Window-scoped by
// billing_period_start so the watermark generalizes to several fires per
// cycle. errs.ErrNotFound when the cycle has no such invoice.
func (s *PostgresStore) GetLatestThresholdInvoiceForCycle(ctx context.Context, tenantID, subscriptionID string, periodStart, periodEnd time.Time) (domain.Invoice, error) {
	if subscriptionID == "" {
		return domain.Invoice{}, errs.ErrNotFound
	}
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.Invoice{}, err
	}
	defer postgres.Rollback(tx)
	return s.thresholdInvoiceForCycleTx(ctx, tx, subscriptionID, periodStart, periodEnd, thresholdBillsWindow, "")
}

// GetLatestThresholdInvoiceForCycleTx is GetLatestThresholdInvoiceForCycle
// on the caller's tx, holding the invoice row FOR KEY SHARE until that tx
// ends. Its callers are the period's closers (the cycle close's in-tx
// re-read, ADR-115; the immediate cancel; the cross-interval swap and its
// re-anchor guard), and each calls it after its tx locked the subscription
// row.
//
// Under READ COMMITTED a statement issued after the closer's row-lock wait
// sees every threshold fire that committed before it took the lock, so the
// closer can prove the watermark it built its lines against is still the
// watermark, and roll back if a fire landed in between. The lock does the
// same for a void (ADR-066 amendment 2026-10-23). A void locks the invoice
// FOR UPDATE (UpdateStatusWithReversalPriorTx), which conflicts with this
// lock. A void that holds the row when the read runs is waited for: the row
// is re-checked after it commits, a voided row no longer matches, and the
// closer bills the reopened window. A void that arrives after the read waits
// for the closer's commit, so it lands after the period's invoice and the
// window is forgiven. Without the lock that void could commit first and
// still find its window billed nowhere.
//
// FOR KEY SHARE, not FOR SHARE: the paid flips (markPaidReportingTransition,
// the credit note's LockForCreditNoteTx, SettleZeroDue) lock the invoice FOR
// NO KEY UPDATE, which this lock does not wait for and does not block. Only
// a void changes the watermark, and a paid flip can need the subscription
// row while it holds the invoice: when dunning's SetDunningPause for this
// invoice commits after the payment's LockDunningPauseTx read, the
// payment's pause release waits for the subscription lock this closer
// holds. Had this read waited for the payment, the two would deadlock.
// Every writer that locks the invoice FOR UPDATE (void, write-off, charge
// claims, payment failures, draft edits, disputes) takes no subscription
// lock in that transaction, except the cancel's and swap's own prebill
// void, which already holds it. Test-clock teardown (testclock clockTeardownStatements)
// is the exception: it deletes the clock's invoices before its
// subscriptions, so a teardown that overlaps a closer on one of that
// clock's subscriptions fails one of the two with a deadlock error (40P01);
// a retry succeeds. Before this lock the same overlap could fail the
// teardown on a foreign key. Simulation data only.
//
// tenantID is accepted for symmetry with the store's other *Tx methods; RLS
// scoping rides the tx's tenant binding.
func (s *PostgresStore) GetLatestThresholdInvoiceForCycleTx(ctx context.Context, tx *sql.Tx, tenantID, subscriptionID string, periodStart, periodEnd time.Time) (domain.Invoice, error) {
	if subscriptionID == "" {
		return domain.Invoice{}, errs.ErrNotFound
	}
	return s.thresholdInvoiceForCycleTx(ctx, tx, subscriptionID, periodStart, periodEnd, thresholdBillsWindow, "FOR KEY SHARE")
}

// GetThresholdInvoiceForCycleAnyStatus is the fire-once probe's read: the
// cycle window's newest threshold invoice in ANY status. The partial unique
// index idx_invoices_threshold_unique_per_cycle keeps one threshold invoice
// per (subscription, billing_period_start) whatever its status, so a voided
// or written-off fire still holds the cycle's one slot and no later fire can
// insert. Probing with the watermark predicate instead re-evaluated the
// crossing every tick after a void (and, before 2026-10-23, after a
// write-off), running a paid tax calculation before the index refused the
// insert. Never a watermark: a voided row billed nothing.
func (s *PostgresStore) GetThresholdInvoiceForCycleAnyStatus(ctx context.Context, tenantID, subscriptionID string, periodStart, periodEnd time.Time) (domain.Invoice, error) {
	if subscriptionID == "" {
		return domain.Invoice{}, errs.ErrNotFound
	}
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.Invoice{}, err
	}
	defer postgres.Rollback(tx)
	return s.thresholdInvoiceForCycleTx(ctx, tx, subscriptionID, periodStart, periodEnd, thresholdAnyStatus, "")
}

// thresholdInvoiceForCycleTx reads the newest threshold invoice whose
// billing_period_start falls in [periodStart, periodEnd) and that matches
// statusPredicate, with lockClause ("" or "FOR KEY SHARE") on the row it
// returns. Both are this file's constants, never caller input.
func (s *PostgresStore) thresholdInvoiceForCycleTx(ctx context.Context, tx *sql.Tx, subscriptionID string, periodStart, periodEnd time.Time, statusPredicate, lockClause string) (domain.Invoice, error) {
	var inv domain.Invoice
	err := tx.QueryRowContext(ctx, `
		SELECT `+invCols+` FROM invoices i
		WHERE i.subscription_id = $1
		  AND i.billing_reason = 'threshold'
		  AND `+statusPredicate+`
		  AND i.billing_period_start >= $2
		  AND i.billing_period_start < $3
		ORDER BY i.billing_period_end DESC
		LIMIT 1
		`+lockClause+`
	`, subscriptionID, periodStart, periodEnd).Scan(s.scanInvDest(&inv)...)
	if err == sql.ErrNoRows {
		return domain.Invoice{}, errs.ErrNotFound
	}
	return inv, err
}

// GetInvoiceForPeriod returns the newest non-voided invoice keyed exactly
// like idx_invoices_billing_idempotency (subscription_id, period_start,
// period_end). billOnePeriod's ErrAlreadyExists re-entry path fetches the
// blocking row through this to heal the $0-MarkPaid crash window (ADR-066).
func (s *PostgresStore) GetInvoiceForPeriod(ctx context.Context, tenantID, subscriptionID string, periodStart, periodEnd time.Time) (domain.Invoice, error) {
	if subscriptionID == "" {
		return domain.Invoice{}, errs.ErrNotFound
	}
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.Invoice{}, err
	}
	defer postgres.Rollback(tx)

	var inv domain.Invoice
	err = tx.QueryRowContext(ctx, `
		SELECT `+invCols+` FROM invoices i
		WHERE i.subscription_id = $1
		  AND i.billing_period_start = $2
		  AND i.billing_period_end = $3
		  AND i.status != 'voided'
		ORDER BY i.created_at DESC
		LIMIT 1
	`, subscriptionID, periodStart, periodEnd).Scan(s.scanInvDest(&inv)...)
	if err == sql.ErrNoRows {
		return domain.Invoice{}, errs.ErrNotFound
	}
	return inv, err
}

// invoiceOrderBy returns the ORDER BY clause for the invoice list,
// validating sort + dir against a closed set. Anything outside the
// allow-list silently falls back to the default — never interpolate
// raw user input into SQL.
//
// Tie-break on id matches the primary sort direction so a sequence
// of ties reads as a single ordered group rather than zig-zagging.
// The id column is monotonic (ulid-prefixed); using it as the
// secondary sort gives a stable, deterministic order.
func invoiceOrderBy(sort, dir string) string {
	col := invoiceSortColumn(sort)
	d := "DESC"
	if dir == "asc" {
		d = "ASC"
	}
	return col + " " + d + ", id " + d
}

// invoiceSortColumn maps the SPA's sort key to a SQL column name.
// Closed allow-list to prevent injection. Unknown keys default to
// created_at (the most common sort, matches Stripe Dashboard).
//
// amount_due sorts by the invoice's amount as the Invoices list's "Amount"
// column shows it (invoiceAmountCents in web-v2/src/lib/invoiceTerminal.ts):
// a paid invoice by what the payment and customer credit paid together, since
// the settle zeroed its amount_due; every other invoice by amount_due, which a
// void and a write-off keep. Sorting paid rows by the zeroed figure put a $29
// paid invoice below a $20 open one (sign-off of #1328).
func invoiceSortColumn(key string) string {
	switch key {
	case "invoice_number":
		return "invoice_number"
	case "amount_due_cents", "amount_due":
		return "(CASE WHEN status = 'paid' THEN amount_paid_cents + credits_applied_cents ELSE amount_due_cents END)"
	case "billing_period_start", "period":
		return "billing_period_start"
	case "due_at":
		return "due_at"
	case "issued_at":
		return "issued_at"
	case "status":
		return "status"
	case "payment_status":
		return "payment_status"
	default:
		return "created_at"
	}
}

func buildInvWhere(f ListFilter) (string, []any) {
	var clauses []string
	var args []any
	idx := 1

	if f.CustomerID != "" {
		clauses = append(clauses, fmt.Sprintf("customer_id = $%d", idx))
		args = append(args, f.CustomerID)
		idx++
	}
	if f.SubscriptionID != "" {
		clauses = append(clauses, fmt.Sprintf("subscription_id = $%d", idx))
		args = append(args, f.SubscriptionID)
		idx++
	}
	if f.Status != "" {
		clauses = append(clauses, fmt.Sprintf("status = $%d", idx))
		args = append(args, f.Status)
		idx++
	}
	if f.PaymentStatus != "" {
		clauses = append(clauses, fmt.Sprintf("payment_status = $%d", idx))
		args = append(args, f.PaymentStatus)
		idx++
	}
	if f.Search != "" {
		clauses = append(clauses, fmt.Sprintf("invoice_number ILIKE $%d", idx))
		args = append(args, "%"+postgres.EscapeLike(f.Search)+"%")
		idx++
	}
	if !f.CreatedFrom.IsZero() {
		clauses = append(clauses, fmt.Sprintf("created_at >= $%d", idx))
		args = append(args, f.CreatedFrom)
		idx++
	}
	if !f.CreatedTo.IsZero() {
		clauses = append(clauses, fmt.Sprintf("created_at <= $%d", idx))
		args = append(args, f.CreatedTo)
		idx++
	}
	if f.Overdue {
		// "Past due" = open, past its due date, not settled, not
		// mid-payment. Mirrors domain.ClassifyInvoiceAttention's
		// overdue semantics, but computed in SQL so the filter
		// paginates server-side. due_at compares against DB
		// wall-clock now(); test-clock invoices stamp due_at in
		// simulated time, so a frozen-future clock keeps its
		// invoices out of this view until wall-clock catches up —
		// the same trade Stripe's dashboard makes, and the per-row
		// attention dot still reflects the authoritative state.
		// 'unknown' is excluded alongside 'processing' because the comment above
		// already states the intent — "not mid-payment" — and IsInFlight has
		// always meant processing OR unknown. Leaving it in listed a PARKED
		// invoice (ADR-107) as past-due and chaseable, when it is the one
		// invoice no charge path will touch and whose money may already have
		// been taken. The filter said what it meant; the SQL did not match it.
		clauses = append(clauses, fmt.Sprintf(
			"(status = $%d AND due_at IS NOT NULL AND due_at < now() AND payment_status NOT IN ($%d, $%d, $%d))",
			idx, idx+1, idx+2, idx+3))
		args = append(args,
			string(domain.InvoiceFinalized),
			string(domain.PaymentSucceeded),
			string(domain.PaymentProcessing),
			string(domain.PaymentUnknown))
		idx += 4
	}
	if len(f.IDs) > 0 {
		// ids=... filter — same shape as customer.ListFilter.IDs.
		// Lets CreditNotes-and-similar pages fetch the exact invoices
		// their primary rows reference without depending on the
		// default-pagination of an unrelated invoice list.
		placeholders := make([]string, len(f.IDs))
		for i, id := range f.IDs {
			placeholders[i] = fmt.Sprintf("$%d", idx)
			args = append(args, id)
			idx++
		}
		clauses = append(clauses, "id IN ("+strings.Join(placeholders, ",")+")")
	}

	if len(clauses) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

// RecordPaymentAnomaly stamps the durable payment-anomaly marker (ADR-068)
// the dashboard attention banner reads. Single-slot, last-wins — the audit
// log carries the history; the banner needs "money is wrong on THIS
// invoice + which PI".
func (s *PostgresStore) RecordPaymentAnomaly(ctx context.Context, tenantID, invoiceID, kind, paymentIntentID string, capturedCents int64) error {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(ctx, `
		UPDATE invoices SET
			payment_anomaly_kind = $1,
			payment_anomaly_payment_intent_id = $2,
			payment_anomaly_captured_cents = $3,
			updated_at = now()
		WHERE id = $4
	`, kind, paymentIntentID, capturedCents, invoiceID); err != nil {
		return err
	}
	return tx.Commit()
}

// StreamForExport walks EVERY invoice in the date range in ONE snapshot
// transaction. See customer.PostgresStore.StreamForExport for why the export
// gets its own single-snapshot query instead of paging a list method: under
// LIMIT/OFFSET with a page-per-transaction, a row inserted mid-export shifts
// the newest-first window and the CSV duplicates the row on each page boundary
// (issue #475).
//
// invCols + scanInvDest are shared with Get/List on purpose — an invoice row in
// the CSV must be the same invoice the API returns, field for field.
func (s *PostgresStore) StreamForExport(ctx context.Context, tenantID string, from, to time.Time, fn func(domain.Invoice) error) error {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)

	// Tenant + livemode isolation is RLS's (TxTenant), as in List.
	where := ""
	var args []any
	if !from.IsZero() {
		args = append(args, from)
		where += fmt.Sprintf(" AND created_at >= $%d", len(args))
	}
	if !to.IsZero() {
		args = append(args, to)
		where += fmt.Sprintf(" AND created_at <= $%d", len(args))
	}

	rows, err := tx.QueryContext(ctx, `SELECT `+invCols+`
		FROM invoices
		WHERE TRUE`+where+`
		ORDER BY created_at DESC, id DESC`, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var inv domain.Invoice
		if err := rows.Scan(s.scanInvDest(&inv)...); err != nil {
			return err
		}
		if err := fn(inv); err != nil {
			return err
		}
	}
	return rows.Err()
}

// upsertChargeAttemptTx resolves an attempt's outcome INSIDE the caller's
// settle transaction (ADR-103). Atomic with the invoice's own state flip:
// the attempt fact and the invoice's truth about that same payment can
// never disagree, which is what lets the timeline render payments from
// ONE owner and drop the Stripe-webhook fallback tier.
//
// Insert-time identity (trigger_source, sim_effective_at, the actor) is
// preserved by the ON CONFLICT — the chokepoint knows those; a settle does
// not. A settle for a PaymentIntent Velox never minted (hosted checkout)
// INSERTS here, and that row is the only record of the attempt. It records
// no actor: the report does not know who paid, and its ctx (a webhook, the
// reconciler) would name the system, which did not.
//
// simAt dates that inserted row on the invoice's test clock (ADR-128
// decision 7, reversing ADR-102's wall-clock rule for hosted-page
// attempts): a customer paying is an act on the invoice, like any other.
// Callers pass the settle's own instant when ctx carries a test clock, nil
// on the wall clock. If RecordChargeAttempt's insert comes second, the
// trigger_source rule there decides whose date the row keeps.
//
// declineCode and paymentMethodID are what the report knows about a decline
// (#966); an empty value keeps what an earlier writer recorded, so a source
// that did not say (a reconciler read, a success) never erases a hard
// decline another source put on record.
func upsertChargeAttemptTx(ctx context.Context, tx *sql.Tx, tenantID, invoiceID, piID string, outcome domain.ChargeAttemptOutcome, reason string, amountCents int64, declineCode, paymentMethodID string, simAt *time.Time) error {
	if piID == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO invoice_charge_attempts
			(tenant_id, invoice_id, stripe_payment_intent_id, trigger_source,
			 outcome, provider_reason, amount_cents, occurred_at,
			 decline_code, stripe_payment_method_id, sim_effective_at)
		VALUES ($1, $2, $3, 'external', $4, $5, $6, now(), $7, $8, $9)
		ON CONFLICT (tenant_id, stripe_payment_intent_id)
			WHERE stripe_payment_intent_id <> ''
		DO UPDATE SET
			outcome = CASE WHEN invoice_charge_attempts.outcome = 'succeeded'
				THEN invoice_charge_attempts.outcome ELSE EXCLUDED.outcome END,
			provider_reason = CASE WHEN EXCLUDED.provider_reason <> ''
				THEN EXCLUDED.provider_reason ELSE invoice_charge_attempts.provider_reason END,
			amount_cents = CASE WHEN EXCLUDED.amount_cents > 0
				THEN EXCLUDED.amount_cents ELSE invoice_charge_attempts.amount_cents END,
			decline_code = CASE WHEN EXCLUDED.decline_code <> ''
				THEN EXCLUDED.decline_code ELSE invoice_charge_attempts.decline_code END,
			stripe_payment_method_id = CASE WHEN EXCLUDED.stripe_payment_method_id <> ''
				THEN EXCLUDED.stripe_payment_method_id ELSE invoice_charge_attempts.stripe_payment_method_id END,
			updated_at = now()`,
		tenantID, invoiceID, piID, outcome, reason, amountCents, declineCode, paymentMethodID, postgres.NullableTime(simAt))
	if err != nil {
		return fmt.Errorf("upsert charge attempt in settle tx: %w", err)
	}
	return nil
}

// simAtOf is ctx's test-clock instant for an attempt row, or nil on the wall
// clock. A settle's ctx is bound to the charge's anchor or to the invoice's
// clock (payment.settlement), so this is when the attempt happened there.
func simAtOf(ctx context.Context) *time.Time {
	sim, ok := clock.SimOf(ctx)
	if !ok {
		return nil
	}
	return &sim.At
}

// RecordChargeAttempt writes one charge attempt (ADR-102), upserting by
// PaymentIntent id: the create-time writer inserts the row and the
// settle paths (webhook, inline, reconciler) resolve its outcome in
// place — one attempt, one row. Monotonicity: 'succeeded' is terminal
// (a settle can never un-succeed an attempt); every other outcome may
// advance, including failed → succeeded (3DS second try on one PI).
// sim_effective_at is set at insert and never overwritten — the settle
// path's wall-clock context must not strip an attempt's billing-axis
// anchor. trigger_source is likewise kept, except that this writer replaces
// an 'external' left by a report that arrived first: the charge call made
// the attempt, so its trigger is the truth. The writer that made the
// attempt owns its date the same way: over an 'external' row, this writer's
// sim_effective_at (a dunning retry's slot) wins over the date the early
// report stamped (the invoice's clock at settle, ADR-128 decision 7). Empty-PI attempts (the PI create
// itself failed) insert-only; there is no twin to dedup against.
//
// The actor (ADR-128 §4) is whoever's request led to the charge, resolved
// from ctx as the audit log resolves it. That is the operator or API key
// behind Charge now, and also behind POST /v1/invoices/{id}/finalize, whose
// charge is an auto_charge made on their request. Only charges no request
// started (cycle close, the auto-charge sweep, dunning) record the system,
// so (auto_charge, user) and (auto_charge, api_key) are valid pairs. The
// actor follows trigger_source: kept once written, and filled in on a row a
// report inserted first, which has none.
func (s *PostgresStore) RecordChargeAttempt(ctx context.Context, tenantID string, a domain.InvoiceChargeAttempt) error {
	actorType, actorID := audit.ResolveActor(ctx)
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)

	// Lock order: the invoice row first, then the attempt row. A settle for
	// the same PaymentIntent (webhook, reconciler) locks the invoice FOR
	// UPDATE and then upserts this attempt row. Inserting first instead takes
	// the attempt's unique slot and then waits on the invoice for the foreign
	// key check, which deadlocks against that settle (#989).
	if _, err := tx.ExecContext(ctx, `SELECT 1 FROM invoices WHERE id = $1 FOR UPDATE`, a.InvoiceID); err != nil {
		return err
	}

	if a.StripePaymentIntentID == "" {
		_, err = tx.ExecContext(ctx, `
			INSERT INTO invoice_charge_attempts
				(tenant_id, invoice_id, stripe_payment_intent_id, trigger_source,
				 outcome, provider_reason, amount_cents, occurred_at, sim_effective_at,
				 decline_code, stripe_payment_method_id, actor_type, actor_id)
			VALUES ($1, $2, '', $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
			tenantID, a.InvoiceID, a.Trigger, a.Outcome, a.ProviderReason,
			a.AmountCents, a.OccurredAt, postgres.NullableTime(a.SimEffectiveAt),
			a.DeclineCode, a.StripePaymentMethodID, actorType, actorID,
		)
	} else {
		_, err = tx.ExecContext(ctx, `
			INSERT INTO invoice_charge_attempts
				(tenant_id, invoice_id, stripe_payment_intent_id, trigger_source,
				 outcome, provider_reason, amount_cents, occurred_at, sim_effective_at,
				 decline_code, stripe_payment_method_id, actor_type, actor_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
			ON CONFLICT (tenant_id, stripe_payment_intent_id)
				WHERE stripe_payment_intent_id <> ''
			DO UPDATE SET
				outcome = CASE WHEN invoice_charge_attempts.outcome = 'succeeded'
					THEN invoice_charge_attempts.outcome ELSE EXCLUDED.outcome END,
				provider_reason = CASE WHEN EXCLUDED.provider_reason <> ''
					THEN EXCLUDED.provider_reason ELSE invoice_charge_attempts.provider_reason END,
				amount_cents = CASE WHEN EXCLUDED.amount_cents > 0
					THEN EXCLUDED.amount_cents ELSE invoice_charge_attempts.amount_cents END,
				-- Kept when this write does not know them (#966): see
				-- upsertChargeAttemptTx.
				decline_code = CASE WHEN EXCLUDED.decline_code <> ''
					THEN EXCLUDED.decline_code ELSE invoice_charge_attempts.decline_code END,
				stripe_payment_method_id = CASE WHEN EXCLUDED.stripe_payment_method_id <> ''
					THEN EXCLUDED.stripe_payment_method_id ELSE invoice_charge_attempts.stripe_payment_method_id END,
				-- The writer that made the attempt owns its date, by the same
				-- trigger_source rule as below: an early report's settle stamps
				-- the invoice's clock, this writer stamps the charge's own
				-- instant (a dunning retry's slot).
				sim_effective_at = CASE WHEN invoice_charge_attempts.trigger_source = 'external'
					THEN COALESCE(EXCLUDED.sim_effective_at, invoice_charge_attempts.sim_effective_at)
					ELSE COALESCE(invoice_charge_attempts.sim_effective_at, EXCLUDED.sim_effective_at) END,
				-- A report that beat this writer (its webhook arrived first)
				-- inserted the row as 'external'. This writer made the attempt,
				-- so its trigger is the truth: the timeline label and the
				-- attempt order (veloxAttemptStandingTx) both read it.
				trigger_source = CASE WHEN invoice_charge_attempts.trigger_source = 'external'
					THEN EXCLUDED.trigger_source ELSE invoice_charge_attempts.trigger_source END,
				-- The actor goes with the trigger: a report's row has none.
				-- Every SET reads the row as it was, so the two CASEs agree.
				actor_type = CASE WHEN invoice_charge_attempts.trigger_source = 'external'
					THEN EXCLUDED.actor_type ELSE invoice_charge_attempts.actor_type END,
				actor_id = CASE WHEN invoice_charge_attempts.trigger_source = 'external'
					THEN EXCLUDED.actor_id ELSE invoice_charge_attempts.actor_id END,
				updated_at = now()`,
			tenantID, a.InvoiceID, a.StripePaymentIntentID, a.Trigger, a.Outcome,
			a.ProviderReason, a.AmountCents, a.OccurredAt, postgres.NullableTime(a.SimEffectiveAt),
			a.DeclineCode, a.StripePaymentMethodID, actorType, actorID,
		)
	}
	if err != nil {
		return fmt.Errorf("record charge attempt: %w", err)
	}
	return tx.Commit()
}

// PaymentPagesLive reports which of the invoices have a hosted payment page
// that may still take the customer's money, or a hosted payment not yet
// recorded: the same domain.HostedCheckoutLiveSQL rule the charge claims
// refuse on, so the banner and the refusal cannot disagree. One read for a
// whole list page (issue #1142); invoices without a live page are absent.
func (s *PostgresStore) PaymentPagesLive(ctx context.Context, tenantID string, invoiceIDs []string) (map[string]bool, error) {
	out := make(map[string]bool, len(invoiceIDs))
	if len(invoiceIDs) == 0 {
		return out, nil
	}
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)
	rows, err := tx.QueryContext(ctx, `SELECT q.id FROM unnest($1::text[]) AS q(id) WHERE `+
		domain.HostedCheckoutLiveSQL("q.id"), postgres.StringArray(invoiceIDs))
	if err != nil {
		return nil, fmt.Errorf("payment pages live: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// RetryBlockingDecline returns the Stripe decline code that stops a dunning
// retry from charging this card for this invoice, or "" when the card may be
// charged. Two codes do (domain.RetryBlock):
//   - a hard decline (domain.IsHardDecline, #966) on any attempt with the
//     card: the bank refused it for good, so it decides whatever came after;
//   - otherwise authentication_required (domain.IsAuthenticationRequired) on
//     the newest attempt with the card that names a decline code: the bank
//     wants the customer to confirm the payment, which an off-session retry
//     cannot do. A later ordinary decline (a Collect payment the bank
//     answered differently) is the newer answer and ends it.
//
// A dunning retry asks it about the card it is about to charge. The question
// is per invoice and per card, as Stripe Billing's is: another invoice, or
// another card, is charged as usual.
//
// Hard declines sort first, then the newest row; LIMIT 1 takes the deciding
// row, not because the set is singular.
func (s *PostgresStore) RetryBlockingDecline(ctx context.Context, tenantID, invoiceID, stripePaymentMethodID string) (string, error) {
	if stripePaymentMethodID == "" {
		return "", nil
	}
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return "", err
	}
	defer postgres.Rollback(tx)

	var code string
	err = tx.QueryRowContext(ctx, `
		SELECT decline_code FROM invoice_charge_attempts
		WHERE invoice_id = $1 AND stripe_payment_method_id = $2
		  AND decline_code <> ''
		ORDER BY decline_code = ANY($3::text[]) DESC, created_at DESC, id DESC
		LIMIT 1`,
		invoiceID, stripePaymentMethodID, postgres.StringArray(domain.HardDeclineCodes()),
	).Scan(&code)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("read retry-blocking decline on card: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	if domain.RetryBlock(code) == nil {
		return "", nil
	}
	return code, nil
}

// RetryBlockingDeclinesOnDefaultCards is RetryBlockingDecline for a page of
// invoices, each asked about its customer's current default card: invoice id
// → Stripe's decline code, for the invoices whose next dunning retry would
// skip that card. The attention notice reads it, so it says "needs a
// different card" or "the customer must confirm" exactly while the retry
// would skip the card. Other invoices are absent. One read for the page.
func (s *PostgresStore) RetryBlockingDeclinesOnDefaultCards(ctx context.Context, tenantID string, invoiceIDs []string) (map[string]string, error) {
	out := make(map[string]string, len(invoiceIDs))
	if len(invoiceIDs) == 0 {
		return out, nil
	}
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	rows, err := tx.QueryContext(ctx, `
		SELECT DISTINCT ON (a.invoice_id) a.invoice_id, a.decline_code
		FROM invoice_charge_attempts a
		JOIN invoices i ON i.id = a.invoice_id
		JOIN payment_methods pm ON pm.customer_id = i.customer_id
			AND pm.is_default AND pm.detached_at IS NULL
			AND pm.stripe_payment_method_id = a.stripe_payment_method_id
		WHERE a.invoice_id = ANY($1::text[])
		  AND a.stripe_payment_method_id <> ''
		  AND a.decline_code <> ''
		ORDER BY a.invoice_id, a.decline_code = ANY($2::text[]) DESC, a.created_at DESC, a.id DESC`,
		postgres.StringArray(invoiceIDs), postgres.StringArray(domain.HardDeclineCodes()))
	if err != nil {
		return nil, fmt.Errorf("read retry-blocking declines on default cards: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, code string
		if err := rows.Scan(&id, &code); err != nil {
			return nil, err
		}
		if domain.RetryBlock(code) != nil {
			out[id] = code
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

// ListChargeAttemptsByInvoice returns the invoice's charge attempts,
// oldest first — the timeline's billing-axis payment facts (ADR-102).
func (s *PostgresStore) ListChargeAttemptsByInvoice(ctx context.Context, tenantID, invoiceID string) ([]domain.InvoiceChargeAttempt, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	rows, err := tx.QueryContext(ctx, `
		SELECT id, tenant_id, invoice_id, stripe_payment_intent_id, trigger_source,
		       outcome, provider_reason, amount_cents, occurred_at, sim_effective_at,
		       livemode, created_at, updated_at, decline_code, stripe_payment_method_id
		FROM invoice_charge_attempts
		WHERE invoice_id = $1
		ORDER BY occurred_at, created_at`,
		invoiceID,
	)
	if err != nil {
		return nil, fmt.Errorf("list charge attempts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []domain.InvoiceChargeAttempt
	for rows.Next() {
		var a domain.InvoiceChargeAttempt
		var simAt sql.NullTime
		if err := rows.Scan(&a.ID, &a.TenantID, &a.InvoiceID, &a.StripePaymentIntentID,
			&a.Trigger, &a.Outcome, &a.ProviderReason, &a.AmountCents, &a.OccurredAt,
			&simAt, &a.Livemode, &a.CreatedAt, &a.UpdatedAt, &a.DeclineCode, &a.StripePaymentMethodID); err != nil {
			return nil, err
		}
		if simAt.Valid {
			t := simAt.Time
			a.SimEffectiveAt = &t
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
