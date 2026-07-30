package invoice

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/crypto"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

type PostgresStore struct {
	db           *postgres.DB
	enc          *crypto.Encryptor
	outbox       OutboxEnqueuer
	commitFunder CommitFunder
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

// CommitFunder grants a prepaid-commit credit block on the caller's tx —
// FinalizeWithDates calls it inside the finalize coordinator tx so the status
// flip and the grant are both-or-neither (ADR-078 D2). Satisfied by
// *credit.Service; declared consumer-side so the invoice store needs no
// credit import. Unlike the outbox, this dependency is NOT optional when a
// commit line exists: FinalizeWithDates fails loud on a commit invoice with
// no funder wired.
type CommitFunder interface {
	GrantCommitForInvoiceTx(ctx context.Context, tx *sql.Tx, tenantID, customerID, invoiceID, invoiceNumber string, amountCents int64, expiresAt *time.Time) (domain.CreditLedgerEntry, error)
}

// SetCommitFunder wires commit-grant funding at finalize (ADR-078).
func (s *PostgresStore) SetCommitFunder(f CommitFunder) { s.commitFunder = f }

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
		btz := domain.LoadLocationOrUTC(inv.BillingTimezone)
		return errs.AlreadyExists("billing_period",
			fmt.Sprintf("invoice already exists for subscription %q period %s..%s",
				inv.SubscriptionID,
				inv.BillingPeriodStart.In(btz).Format("2006-01-02"),
				inv.BillingPeriodEnd.In(btz).Format("2006-01-02"))).WithCode("invoice_billing_period_taken")
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
	COALESCE(billing_timezone,''), no_pm_notified_at, charge_attempt_seq`

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
			stripe_invoice_id, is_simulated, billing_timezone)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$27,$28,$29,$30,$31,$32,$33,$34,$35,$36,$37,$38,$39,$40,$41,$42,$43)
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
	).Scan(s.scanInvDest(&inv)...)

	if err != nil {
		if mapped := mapInvoiceUniqueViolation(err, inv); mapped != nil {
			return domain.Invoice{}, mapped
		}
		return domain.Invoice{}, err
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
	err = tx.QueryRowContext(ctx, `SELECT `+invCols+` FROM invoices WHERE id = $1`, id).
		Scan(s.scanInvDest(&inv)...)
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
	query := `SELECT ` + invCols + ` FROM invoices` + where +
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
		if err := rows.Scan(s.scanInvDest(&inv)...); err != nil {
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
	var fn func(tx *sql.Tx, prior domain.InvoiceStatus) error
	if reverseFn != nil {
		fn = func(tx *sql.Tx, _ domain.InvoiceStatus) error { return reverseFn(tx) }
	}
	return s.UpdateStatusWithReversalPrior(ctx, tenantID, id, status, fn)
}

// UpdateStatusWithReversalPrior is UpdateStatusWithReversal handing the
// closure the invoice's TRUE prior status, read FOR UPDATE on this same tx
// before the flip — the service's earlier snapshot races concurrent
// transitions, and an audit row stamping status_before from it could
// permanently record the wrong provenance (ADR-090 PR4 review).
func (s *PostgresStore) UpdateStatusWithReversalPrior(ctx context.Context, tenantID, id string, status domain.InvoiceStatus, reverseFn func(tx *sql.Tx, prior domain.InvoiceStatus) error) (domain.Invoice, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.Invoice{}, err
	}
	defer postgres.Rollback(tx)

	// Row lock + true prior status; same row the CAS below updates, so no
	// new lock ordering edge.
	var prior string
	if err := tx.QueryRowContext(ctx,
		`SELECT status FROM invoices WHERE id = $1 AND tenant_id = $2 FOR UPDATE`,
		id, tenantID).Scan(&prior); err != nil {
		return domain.Invoice{}, fmt.Errorf("lock invoice for status flip: %w", err)
	}

	inv, err := s.updateStatusInTx(ctx, tx, id, status)
	if err != nil {
		return domain.Invoice{}, err
	}
	if reverseFn != nil {
		if err := reverseFn(tx, domain.InvoiceStatus(prior)); err != nil {
			return domain.Invoice{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return domain.Invoice{}, err
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
// an annulled invoice. Allowed sources per target (matching the service-layer
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
	// Terminal statuses also retire the auto-charge work flag: every
	// charge-path predicate already excludes non-finalized invoices, so
	// this is a truth fix, not a behavior change — a voided or
	// uncollectible invoice must not read auto_charge_pending=true
	// (there is nothing pending). Finalize (draft→finalized) leaves the
	// flag alone. Settle clears it at the markPaidReportingTransition
	// choke point the same way.
	err := tx.QueryRowContext(ctx, `
		UPDATE invoices SET status = $1, voided_at = $2,
			uncollectible_at = COALESCE($3, uncollectible_at), updated_at = $4,
			auto_charge_pending = CASE WHEN $1 IN ('voided', 'uncollectible')
				THEN FALSE ELSE auto_charge_pending END
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
		UPDATE invoices SET status = $1, issued_at = $2, due_at = $3, updated_at = $4
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
			inv.CustomerID, inv.ID, inv.InvoiceNumber, commitCents.Int64, expiresAt); ferr != nil {
			return domain.Invoice{}, fmt.Errorf("fund commit grant at finalize: %w", ferr)
		}
	}

	if err := tx.Commit(); err != nil {
		return domain.Invoice{}, err
	}
	return inv, nil
}

func (s *PostgresStore) UpdatePayment(ctx context.Context, tenantID, id string, paymentStatus domain.InvoicePaymentStatus, stripePaymentIntentID, lastPaymentError string, paidAt *time.Time) (domain.Invoice, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.Invoice{}, err
	}
	defer postgres.Rollback(tx)

	now := clock.Now(ctx)
	var inv domain.Invoice
	err = tx.QueryRowContext(ctx, `
		UPDATE invoices SET payment_status = $1, stripe_payment_intent_id = $2,
			last_payment_error = $3, paid_at = $4, updated_at = $5,
			charge_attempt_seq = charge_attempt_seq + 1,
			auto_charge_pending = CASE WHEN $1 = 'succeeded'
				THEN FALSE ELSE auto_charge_pending END
		WHERE id = $6
		RETURNING `+invCols,
		paymentStatus, postgres.NullableString(stripePaymentIntentID),
		postgres.NullableString(lastPaymentError), postgres.NullableTime(paidAt), now, id,
	).Scan(s.scanInvDest(&inv)...)

	if err == sql.ErrNoRows {
		return domain.Invoice{}, errs.ErrNotFound
	}
	if err != nil {
		return domain.Invoice{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.Invoice{}, err
	}
	return inv, nil
}

// MarkPaymentFailedReportingTransition records a payment failure and reports
// whether THIS call is the first to fire the failure-NOTIFICATION set for this
// PaymentIntent — the payment.failed outbound event, the customer "payment
// failed" email, and auto-started dunning. It is the failed-path analogue of
// MarkPaidReportingTransition, but the dedup key is the PaymentIntent id, not
// the status, because failure is non-terminal: an invoice legitimately re-fails
// once per dunning retry, each with a distinct PI.
//
// SELECT … FOR UPDATE serializes concurrent callers. The inbound webhook dedup
// is a non-atomic read pre-check (payment.HandleWebhook), so two at-least-once
// deliveries of the SAME payment_intent.payment_failed — or a reconciler
// recovery racing the original webhook — can both reach here. The first sets
// failure_notified_pi to this PI and returns firstForThisPI=true; the duplicate
// sees the marker already equals this PI and returns false, so SettleFailed
// fires the notification set once, not twice.
//
// The synchronous charge path stamps payment_status='failed' (same PI) WITHOUT
// firing notifications, deferring them to the webhook — so the marker, which
// that path never writes, is the only reliable discriminator (a status-keyed
// gate would suppress the webhook's notifications entirely).
//
// An already-settled invoice (an out-of-order failure for a charge that already
// succeeded) is left untouched and returns false — the authoritative form of
// SettleFailed's stale-snapshot guard, so a stale failure can never flip a paid
// invoice back to failed.
func (s *PostgresStore) MarkPaymentFailedReportingTransition(ctx context.Context, tenantID, id, paymentIntentID, lastPaymentError string) (domain.Invoice, bool, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.Invoice{}, false, err
	}
	defer postgres.Rollback(tx)

	var status, paymentStatus string
	var notifiedPI sql.NullString
	if err := tx.QueryRowContext(ctx,
		`SELECT status, payment_status, failure_notified_pi FROM invoices WHERE id = $1 FOR UPDATE`, id,
	).Scan(&status, &paymentStatus, &notifiedPI); err != nil {
		if err == sql.ErrNoRows {
			return domain.Invoice{}, false, errs.ErrNotFound
		}
		return domain.Invoice{}, false, fmt.Errorf("load invoice for mark-failed: %w", err)
	}

	// Out-of-order failure for an already-settled invoice: never flip paid back
	// to failed (would null paid_at, relink a stale PI, and dun a paid invoice).
	// Return the row unchanged; not a fresh notification.
	if domain.InvoiceStatus(status) == domain.InvoicePaid || domain.InvoicePaymentStatus(paymentStatus) == domain.PaymentSucceeded {
		var inv domain.Invoice
		if err := tx.QueryRowContext(ctx,
			`SELECT `+invCols+` FROM invoices WHERE id = $1`, id,
		).Scan(s.scanInvDest(&inv)...); err != nil {
			return domain.Invoice{}, false, fmt.Errorf("reload settled invoice: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return domain.Invoice{}, false, err
		}
		return inv, false, nil
	}

	firstForThisPI := !notifiedPI.Valid || notifiedPI.String != paymentIntentID

	now := clock.Now(ctx)
	var inv domain.Invoice
	if err := tx.QueryRowContext(ctx, `
		UPDATE invoices SET
			payment_status = 'failed',
			stripe_payment_intent_id = $1,
			last_payment_error = $2,
			failure_notified_pi = $1,
			charge_attempt_seq = charge_attempt_seq + 1,
			updated_at = $3
		WHERE id = $4
		RETURNING `+invCols,
		postgres.NullableString(paymentIntentID), postgres.NullableString(lastPaymentError), now, id,
	).Scan(s.scanInvDest(&inv)...); err != nil {
		if err == sql.ErrNoRows {
			return domain.Invoice{}, false, errs.ErrNotFound
		}
		return domain.Invoice{}, false, err
	}
	// payment.failed — enqueued in the SAME tx as the failed-stamp, so the event
	// is crash-safe with the transition instead of fire-and-forget post-commit
	// (the mirror of payment.succeeded in markPaidReportingTransition; ADR-040
	// transactional outbox). Gated on firstForThisPI: a same-PI redelivery must
	// not double-notify, while a NEW retry PI is a genuinely distinct failure and
	// fires again. The out-of-order paid guard above returns before reaching
	// here, so a stale failure on a settled invoice never emits.
	if firstForThisPI && s.outbox != nil {
		if _, err := s.outbox.Enqueue(ctx, tx, tenantID, domain.EventPaymentFailed, map[string]any{
			"invoice_id":        inv.ID,
			"customer_id":       inv.CustomerID,
			"payment_intent_id": paymentIntentID,
			"failure_message":   lastPaymentError,
			"amount_cents":      inv.TotalAmountCents,
			"currency":          inv.Currency,
		}); err != nil {
			return domain.Invoice{}, false, fmt.Errorf("enqueue payment.failed: %w", err)
		}
	}
	// ADR-103: the attempt's outcome lands in THIS tx, so the timeline's
	// single payment owner can never disagree with the invoice's state.
	if err := upsertChargeAttemptTx(ctx, tx, tenantID, inv.ID, paymentIntentID,
		domain.ChargeAttemptFailed, lastPaymentError, inv.AmountDueCents); err != nil {
		return domain.Invoice{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return domain.Invoice{}, false, err
	}
	return inv, firstForThisPI, nil
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
// SELECT … FOR UPDATE serializes concurrent callers, so for two
// at-least-once webhook deliveries of the SAME charge exactly one gets
// transitioned=true. SettleSucceeded gates its post-paid side-effects
// (payment.succeeded event, receipt email, card stamp) on the flag so a
// concurrent redelivery — which the stale-read fast-path guard can't
// catch — fires them once, not twice. invoice.paid itself is already
// once-only (enqueued inside this tx, after the no-op branch returns).
func (s *PostgresStore) MarkPaidReportingTransition(ctx context.Context, tenantID, id string, stripePaymentIntentID string, paidAt time.Time) (domain.Invoice, bool, error) {
	return s.markPaidReportingTransition(ctx, tenantID, id, stripePaymentIntentID, paidAt, false)
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
func (s *PostgresStore) MarkPaidCardSettlementTransition(ctx context.Context, tenantID, id string, stripePaymentIntentID string, paidAt time.Time) (domain.Invoice, bool, error) {
	return s.markPaidReportingTransition(ctx, tenantID, id, stripePaymentIntentID, paidAt, true)
}

// markPaidReportingTransition is the shared core. cardSettlement=true also
// enqueues payment.succeeded on the SAME tx as the paid-flip (gated on the
// transition, so exactly-once). Both event enqueues live inside this one tx, so
// they are atomic with finalized/uncollectible→paid: tx rolls back → no events;
// commits → the dispatcher delivers.
func (s *PostgresStore) markPaidReportingTransition(ctx context.Context, tenantID, id string, stripePaymentIntentID string, paidAt time.Time, cardSettlement bool) (domain.Invoice, bool, error) {
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
	// Also reject when tax_status is still pending/failed: even a
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
	var status, taxStatus string
	if err := tx.QueryRowContext(ctx,
		`SELECT status, tax_status FROM invoices WHERE id = $1 FOR UPDATE`, id,
	).Scan(&status, &taxStatus); err != nil {
		if err == sql.ErrNoRows {
			return domain.Invoice{}, false, errs.ErrNotFound
		}
		return domain.Invoice{}, false, fmt.Errorf("load invoice for mark-paid: %w", err)
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
	err = tx.QueryRowContext(ctx, `
		UPDATE invoices SET
			status = 'paid',
			payment_status = 'succeeded',
			stripe_payment_intent_id = $1,
			charge_attempt_seq = charge_attempt_seq + 1,
			paid_at = $2,
			amount_paid_cents = amount_due_cents,
			amount_due_cents = 0,
			auto_charge_pending = FALSE,
			updated_at = $3
		WHERE id = $4
		RETURNING `+invCols,
		postgres.NullableString(stripePaymentIntentID), paidAt, now, id,
	).Scan(s.scanInvDest(&inv)...)

	if err == sql.ErrNoRows {
		return domain.Invoice{}, false, errs.ErrNotFound
	}
	if err != nil {
		return domain.Invoice{}, false, err
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
		return domain.Invoice{}, false, fmt.Errorf("close checkout claims on settle: %w", err)
	}
	// invoice.paid — enqueued in the SAME tx as the finalized/uncollectible →
	// paid transition. The already-paid branch above returns before reaching
	// here, so this fires EXACTLY ONCE, and it covers every settlement path
	// (card via SettleSucceeded, credits-cover, offline record-payment, dunning
	// recovery, and the reconciler's bare-MarkPaid fallback) without each
	// needing its own dispatch. ADR-040 transactional outbox: if the tx rolls
	// back, no event; if it commits, the dispatcher delivers.
	if s.outbox != nil {
		if _, err := s.outbox.Enqueue(ctx, tx, tenantID, domain.EventInvoicePaid, map[string]any{
			"invoice_id":        inv.ID,
			"invoice_number":    inv.InvoiceNumber,
			"customer_id":       inv.CustomerID,
			"subscription_id":   inv.SubscriptionID,
			"amount_paid_cents": inv.AmountPaidCents,
			"currency":          inv.Currency,
			"paid_at":           paidAt.UTC(),
		}); err != nil {
			return domain.Invoice{}, false, fmt.Errorf("enqueue invoice.paid: %w", err)
		}
	}
	// payment.succeeded — CARD settlement path only. Enqueued in the SAME tx as
	// the paid-flip so the only event carrying the Stripe payment_intent_id is
	// crash-safe rather than fire-and-forget post-commit (a post-commit window
	// is as wide as whatever runs before the enqueue — see the durability-
	// tiering block in payment/settlement.go). Reaches here only on the
	// finalized/uncollectible→paid transition (the already-paid branch returned
	// above), so it fires exactly once, same as invoice.paid.
	if cardSettlement && s.outbox != nil {
		if _, err := s.outbox.Enqueue(ctx, tx, tenantID, domain.EventPaymentSucceeded, map[string]any{
			"invoice_id":        inv.ID,
			"customer_id":       inv.CustomerID,
			"payment_intent_id": stripePaymentIntentID,
			"amount_cents":      inv.TotalAmountCents,
			"currency":          inv.Currency,
		}); err != nil {
			return domain.Invoice{}, false, fmt.Errorf("enqueue payment.succeeded: %w", err)
		}
	}
	// ADR-103: same-tx outcome resolution (see upsertChargeAttemptTx).
	// amount_paid_cents is what this PaymentIntent actually settled.
	if err := upsertChargeAttemptTx(ctx, tx, tenantID, inv.ID, stripePaymentIntentID,
		domain.ChargeAttemptSucceeded, "", inv.AmountPaidCents); err != nil {
		return domain.Invoice{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return domain.Invoice{}, false, err
	}
	return inv, true, nil
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

	inv, err := s.ApplyCreditNoteTx(ctx, tx, tenantID, id, amountCents)
	if err != nil {
		return domain.Invoice{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.Invoice{}, err
	}
	return inv, nil
}

// ApplyCreditNoteTx reduces amount_due on the caller's coordinator tx, so the
// reduction commits atomically with the credit note's draft→issued CAS (ADR-061,
// creditnote.Issue). The single-CAS-gated-caller invariant — exactly one Issue()
// reaches this per credit note — makes the reduction idempotent by construction,
// so no source-dedup row is needed. A reviewer adding a second caller of this
// method MUST reintroduce (tenant, source) dedup; see ADR-061.
func (s *PostgresStore) ApplyCreditNoteTx(ctx context.Context, tx *sql.Tx, tenantID, id string, amountCents int64) (domain.Invoice, error) {
	now := clock.Now(ctx)
	var inv domain.Invoice
	err := tx.QueryRowContext(ctx, `
		UPDATE invoices SET
			amount_due_cents = GREATEST(amount_due_cents - $1, 0),
			updated_at = $2
		WHERE id = $3
		RETURNING `+invCols,
		amountCents, now, id,
	).Scan(s.scanInvDest(&inv)...)

	if err == sql.ErrNoRows {
		return domain.Invoice{}, errs.ErrNotFound
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
			tax_jurisdiction, tax_code, tax_reason, quantity_decimal, nominal_unit_amount_decimal)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24)
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
		nominalRateArg(item.NominalUnitAmountDecimal),
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
			commit_granted_cents, commit_expires_at, nominal_unit_amount_decimal
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
			&item.CommitGrantedCents, &item.CommitExpiresAt, &nominal); err != nil {
			return nil, err
		}
		item.NominalUnitAmountDecimal = scanNominalRate(nominal)
		_ = json.Unmarshal(metaJSON, &item.Metadata)
		items = append(items, item)
	}
	return items, rows.Err()
}

// GetOutstandingBalance computes the customer's AR exposure: sum of
// amount_due_cents + count across all finalized invoices in
// payment_status pending/failed/unknown, excluding voided +
// uncollectible. Single tenant-scoped aggregate query; powers the
// "Outstanding balance" card on customer detail.
func (s *PostgresStore) GetOutstandingBalance(ctx context.Context, tenantID, customerID string) (OutstandingBalance, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return OutstandingBalance{}, err
	}
	defer postgres.Rollback(tx)

	var out OutstandingBalance
	err = tx.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(amount_due_cents), 0), COUNT(*)
		FROM invoices
		WHERE customer_id = $1
		  AND payment_status IN ('pending', 'failed', 'unknown')
		  AND status NOT IN ('voided', 'uncollectible', 'draft')
	`, customerID).Scan(&out.TotalCents, &out.UnpaidCount)
	if err != nil {
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
			commit_granted_cents, commit_expires_at, nominal_unit_amount_decimal)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26)
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
		nominalRateArg(item.NominalUnitAmountDecimal),
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
			billing_timezone)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$28,$29,$30,$31,$32,$33,$34,$35,$36,$37,$38,$39,$40,$41,$42,$43,$44,$45,$46,$47,$48)
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
	).Scan(s.scanInvDest(&inv)...)

	if err != nil {
		if mapped := mapInvoiceUniqueViolation(err, inv); mapped != nil {
			return domain.Invoice{}, mapped
		}
		return domain.Invoice{}, err
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
				commit_granted_cents, commit_expires_at, nominal_unit_amount_decimal)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26)
		`, itemID, inv.ID, tenantID, items[i].LineType, postgres.NullableString(items[i].MeterID),
			items[i].Description, items[i].Quantity, items[i].UnitAmountCents, items[i].AmountCents,
			items[i].TaxRate, items[i].TaxAmountCents, items[i].TotalAmountCents,
			items[i].Currency, postgres.NullableString(items[i].PricingMode),
			postgres.NullableString(items[i].RatingRuleVersionID),
			postgres.NullableTime(items[i].BillingPeriodStart), postgres.NullableTime(items[i].BillingPeriodEnd),
			itemMetaJSON, now, items[i].TaxJurisdiction, items[i].TaxCode, items[i].TaxabilityReason, items[i].QuantityDecimal,
			items[i].CommitGrantedCents, postgres.NullableTime(items[i].CommitExpiresAt),
			nominalRateArg(items[i].NominalUnitAmountDecimal),
		)
		if err != nil {
			return domain.Invoice{}, fmt.Errorf("create line item %d: %w", i, err)
		}
	}

	return inv, nil
}

// ClaimAutoCharge takes the per-invoice charge lease (HA hazard #1,
// migration 0141): a 5-minute CAS claim that admits exactly one sweep
// leader into the charge leg per invoice per window. The full
// eligibility predicate is re-asserted so the CAS doubles as a
// freshness gate — under READ COMMITTED the UPDATE re-evaluates against
// the latest committed row, so an invoice settled by a webhook (or
// marked failed/unknown by a rival leader) between list and claim fails
// the claim rather than being charged stale.
//
// Two load-bearing choices:
//   - updated_at is NOT touched: the Stripe idempotency key derives
//     from it (payment/stripe.go), and key stability across claim
//     windows is what makes a re-claimed retry after a stalled leader
//     converge on the SAME PaymentIntent instead of minting a second.
//   - DB-side now() on both sides of the lease comparison: test clocks
//     freeze simulated time, and a simulated-time lease would never
//     expire on a frozen clock (the catchup path shares this claim).
//
// The 5m lease covers ONE invoice's charge iteration (30s Stripe ctx +
// credit apply + refresh ≈ <60s degraded) with margin, and sits well
// under the 1h prod tick so a crashed leader's claim self-heals before
// the next sweep. Deliberately per-invoice, not stamped at list time —
// a batch-wide lease cannot cover a serial 50-invoice loop, and a
// claiming LIST would starve dunning enrollment, which shares the list
// methods (adversarial review, 2026-07-06).
func (s *PostgresStore) ClaimAutoCharge(ctx context.Context, tenantID, id string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return false, err
	}
	defer postgres.Rollback(tx)

	res, err := tx.ExecContext(ctx, `
		UPDATE invoices
		SET auto_charge_claimed_until = now() + interval '5 minutes'
		WHERE id = $1
		  AND auto_charge_pending = TRUE
		  AND payment_status = 'pending'
		  AND status = 'finalized'
		  AND amount_due_cents > 0
		  AND (auto_charge_claimed_until IS NULL OR auto_charge_claimed_until < now())
	`, id)
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

// ReleaseAutoChargeClaim clears the lease on skip paths where the
// charge call was PROVABLY never made (credit-apply failure, no payment
// method on file) so the next tick — or the next test-clock Advance —
// retries immediately instead of waiting out the lease. Never called
// after ChargeInvoice was attempted: terminal outcomes exit the list
// predicate on their own, and an ambiguous transient (breaker/timeout)
// must wait out the lease precisely because the call may have reached
// Stripe. No updated_at write (see ClaimAutoCharge).
func (s *PostgresStore) ReleaseAutoChargeClaim(ctx context.Context, tenantID, id string) error {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)

	if _, err := tx.ExecContext(ctx,
		`UPDATE invoices SET auto_charge_claimed_until = NULL WHERE id = $1`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// ClaimChargeForDunningRetry is the dunning-side twin of
// ClaimAutoCharge (HA hazard #11): both charge paths share ONE lease
// column, so the auto-charge sweep and a due dunning retry can never
// hold the same invoice concurrently — their Stripe idempotency keys
// differ BY CONSTRUCTION (the purpose suffix), so Stripe cannot dedupe
// them and mutual exclusion here is the only guard.
//
// Predicate differences from the sweep claim, both deliberate:
//   - no auto_charge_pending requirement (dunning retries fire off the
//     run schedule, not the flag);
//   - payment_status IN ('pending','failed') — 'pending' covers the
//     card-less-enrolled invoice whose card just attached, 'failed' is
//     the normal retry state. 'unknown' is EXCLUDED on purpose: an
//     ambiguous outcome may be a real payment and must wait for the
//     reconciler, never be blind re-charged — this also closes the
//     same-tick N=1 window where the billing half's unknown outcome
//     was followed seconds later by the dunning half's retry.
//
// Same lease/no-updated_at rules as ClaimAutoCharge (see there).
func (s *PostgresStore) ClaimChargeForDunningRetry(ctx context.Context, tenantID, id string) (bool, error) {
	return s.claimChargeLease(ctx, tenantID, id)
}

// ClaimChargeForManualCollect is the operator-collect twin on the SAME
// lease (2026-07-21 snapshot-race audit): before this claim existed,
// POST /invoices/{id}/collect charged its handler snapshot with no
// mutual exclusion against the auto-charge sweep or a due dunning retry
// — the one charge initiator outside the ring. Identical predicate to
// the dunning claim: 'unknown' stays excluded (a possibly-succeeded
// payment is the reconciler's, never blind re-charged).
func (s *PostgresStore) ClaimChargeForManualCollect(ctx context.Context, tenantID, id string) (bool, error) {
	return s.claimChargeLease(ctx, tenantID, id)
}

// claimChargeLease is the shared CAS both non-sweep charge claims ride:
// finalized, pending/failed, owing, lease free. No updated_at write —
// Stripe idempotency keys derive from it (see ClaimAutoCharge).
func (s *PostgresStore) claimChargeLease(ctx context.Context, tenantID, id string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return false, err
	}
	defer postgres.Rollback(tx)

	res, err := tx.ExecContext(ctx, `
		UPDATE invoices
		SET auto_charge_claimed_until = now() + interval '5 minutes'
		WHERE id = $1
		  AND payment_status IN ('pending', 'failed')
		  AND status = 'finalized'
		  AND amount_due_cents > 0
		  AND (auto_charge_claimed_until IS NULL OR auto_charge_claimed_until < now())
	`, id)
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
func notPausedForCollection(alias string) string {
	return `NOT EXISTS (
			SELECT 1 FROM subscriptions ps
			 WHERE ps.id = ` + alias + `.subscription_id
			   AND ps.pause_collection_behavior IS NOT NULL
		)`
}

// ListAutoChargePending returns invoices that need auto-charge retry —
// CRON path. Excludes clock-pinned subscriptions per ADR-029: simulation
// time progresses only on operator Advance, so the wall-clock scheduler
// must never charge a clock-pinned invoice. The catchup worker uses
// ListAutoChargePendingForClock as the disjoint per-clock entry point.
func (s *PostgresStore) ListAutoChargePending(ctx context.Context, limit int) ([]domain.Invoice, error) {
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
		  AND i.amount_due_cents > 0
		  AND i.livemode = $1
		  AND i.is_simulated = false
		  AND `+notPausedForCollection("i")+`
		ORDER BY i.created_at ASC
		LIMIT $2
	`, postgres.Livemode(ctx), limit)
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
// SettleFailed starts dunning POST-COMMIT (best-effort, behind the firstForThisPI
// gate), so a crash — or an exhausted StartDunning retry — in that window leaves
// the invoice failed-but-undunned, and a same-PI webhook redelivery skips the
// restart (firstForThisPI=false). This sweep hands those invoices back to the
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
func (s *PostgresStore) ListAutoChargePendingForClock(ctx context.Context, tenantID, clockID string, limit int) ([]domain.Invoice, error) {
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
		  AND i.amount_due_cents > 0
		  AND i.tenant_id = $1
		  AND c.test_clock_id = $2
		  AND `+notPausedForCollection("i")+`
		ORDER BY i.created_at ASC
		LIMIT $3
	`, tenantID, clockID, limit)
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
		ORDER BY updated_at ASC
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
//   - status='draft' AND tax_status IN (pending, failed)
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
		  AND i.tax_status IN ('pending', 'failed')
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
// Bounded to invoices touched in the last 24h. Unlike ListPendingTaxCommit —
// whose 24h matches a HARD limit (the Stripe Tax calc TTL, after which a
// re-commit is impossible) — a reversal has no such ceiling; here the window is
// purely (a) anti-churn and (b) a grandfather for pre-0122 voids (whose
// tax_reversed_at is NULL by default but were almost certainly reversed inline
// already), avoiding a one-time re-reversal burst on first deploy WITHOUT a data
// backfill. A transient failure resolves in seconds-to-minutes, well inside the
// window. A SUSTAINED (>24h) failure ages out to manual reconcile — the loud
// ERROR at the inline failure (raised from WARN) is the operator signal, same
// recovery model as ListPendingTaxCommit's aged-out orphans. Re-reversal is
// idempotent at Stripe (the invoice-stable `inv_taxrev_<id>` reference dedups),
// so a row that WAS reversed but failed to stamp tax_reversed_at re-confirms
// harmlessly and stamps out next tick.
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
		  AND i.status IN ('voided', 'uncollectible')
		  AND i.tax_provider = 'stripe_tax'
		  AND COALESCE(i.tax_transaction_id, '') <> ''
		  AND i.tax_reversed_at IS NULL
		  AND i.is_simulated = false
		  AND i.updated_at > now() - interval '24 hours'
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
		  AND i.tax_status IN ('pending', 'failed')
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
		  AND tax_status IN ('pending', 'failed')
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
		  AND tax_status IN ('pending', 'failed')
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

// FindBaseInvoiceForPeriod is the proration-credit gate: returns the
// invoice carrying an in_advance base line for the given subscription
// period. The line's `billing_period_start` matches the period the
// base fee covers (industry-standard semantic — Stripe / Lago / Orb /
// Chargebee align). Caller reads invoice.PaymentStatus to decide
// whether a "refundable" credit (Chargebee Refundable) or "skip /
// adjust unpaid invoice" (Chargebee Adjustment / Stripe none) shape
// applies.
//
// Voided / uncollectible invoices are filtered out — they don't
// represent revenue the customer paid for, even if their line items
// match the period. ORDER BY created_at DESC + LIMIT 1 surfaces the
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
		  AND i.status NOT IN ('voided', 'uncollectible')
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

// FindFundingInvoicesForPeriod returns EVERY non-voided/non-uncollectible
// invoice that prepaid the cycle [periodStart, periodEnd) for a subscription —
// the day-1 base invoice AND any mid-period proration invoice that a plan
// upgrade / quantity increase issued against the same cycle. The single-result
// FindBaseInvoiceForPeriod misses the latter, because a mid-period proration
// invoice stamps its base-fee LINE's billing_period_start at the change instant
// (changeAt), not the cycle's periodStart — so a period-wide credit sized off
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
//     line's billing_period_start is changeAt, so the line predicate can't see
//     it — the header is the reliable key.)
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
		  AND i.status NOT IN ('voided', 'uncollectible')
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

// LatestThresholdPeriodEnd returns the latest billing_period_end across
// the subscription's threshold-fired invoices for the cycle starting at
// periodStart. The billing engine's cycle close uses it as the
// "already billed through" watermark: usage before that instant (and
// the in_arrears base fee) landed on the threshold invoice, so the
// cycle-close invoice must bill only the residual window — otherwise a
// reset_billing_cycle=false threshold double-bills the customer.
//
// Window-scoped by billing_period_start ∈ [periodStart, periodEnd) so
// the watermark generalizes to multiple threshold fires per cycle
// (each successive fire's period starts at the previous one's end).
// Voided / uncollectible invoices don't count — their amounts were
// returned or written off, so the usage they covered is billable again.
// Returns errs.ErrNotFound when no threshold invoice fired this cycle.
func (s *PostgresStore) LatestThresholdPeriodEnd(ctx context.Context, tenantID, subscriptionID string, periodStart, periodEnd time.Time) (time.Time, error) {
	if subscriptionID == "" {
		return time.Time{}, errs.ErrNotFound
	}
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return time.Time{}, err
	}
	defer postgres.Rollback(tx)

	var end sql.NullTime
	err = tx.QueryRowContext(ctx, `
		SELECT MAX(billing_period_end) FROM invoices
		WHERE subscription_id = $1
		  AND billing_reason = 'threshold'
		  AND status NOT IN ('voided', 'uncollectible')
		  AND billing_period_start >= $2
		  AND billing_period_start < $3
	`, subscriptionID, periodStart, periodEnd).Scan(&end)
	if err != nil {
		return time.Time{}, err
	}
	if !end.Valid {
		return time.Time{}, errs.ErrNotFound
	}
	return end.Time, nil
}

// GetLatestThresholdInvoiceForCycle returns the newest non-voided
// threshold-fired invoice for the cycle window — the same predicate as
// LatestThresholdPeriodEnd but the full row. The billing engine's fire-once
// probe uses it so a probe hit can also HEAL a crash between invoice-create
// and the $0/credited MarkPaid (ADR-066): the probe needs the invoice's
// status, payment_status and amount_due, not just the watermark instant.
func (s *PostgresStore) GetLatestThresholdInvoiceForCycle(ctx context.Context, tenantID, subscriptionID string, periodStart, periodEnd time.Time) (domain.Invoice, error) {
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
		  AND i.billing_reason = 'threshold'
		  AND i.status NOT IN ('voided', 'uncollectible')
		  AND i.billing_period_start >= $2
		  AND i.billing_period_start < $3
		ORDER BY i.billing_period_end DESC
		LIMIT 1
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
func invoiceSortColumn(key string) string {
	switch key {
	case "invoice_number":
		return "invoice_number"
	case "amount_due_cents", "amount_due":
		return "amount_due_cents"
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
		clauses = append(clauses, fmt.Sprintf(
			"(status = $%d AND due_at IS NOT NULL AND due_at < now() AND payment_status NOT IN ($%d, $%d))",
			idx, idx+1, idx+2))
		args = append(args,
			string(domain.InvoiceFinalized),
			string(domain.PaymentSucceeded),
			string(domain.PaymentProcessing))
		idx += 3
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
// Insert-time identity (trigger_source, sim_effective_at) is preserved by
// the ON CONFLICT — the chokepoint knows those; a settle does not. A
// settle for a PaymentIntent Velox never minted (hosted checkout) INSERTS
// here, and that row is the only record of the attempt.
func upsertChargeAttemptTx(ctx context.Context, tx *sql.Tx, tenantID, invoiceID, piID string, outcome domain.ChargeAttemptOutcome, reason string, amountCents int64) error {
	if piID == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO invoice_charge_attempts
			(tenant_id, invoice_id, stripe_payment_intent_id, trigger_source,
			 outcome, provider_reason, amount_cents, occurred_at)
		VALUES ($1, $2, $3, 'external', $4, $5, $6, now())
		ON CONFLICT (tenant_id, stripe_payment_intent_id)
			WHERE stripe_payment_intent_id <> ''
		DO UPDATE SET
			outcome = CASE WHEN invoice_charge_attempts.outcome = 'succeeded'
				THEN invoice_charge_attempts.outcome ELSE EXCLUDED.outcome END,
			provider_reason = CASE WHEN EXCLUDED.provider_reason <> ''
				THEN EXCLUDED.provider_reason ELSE invoice_charge_attempts.provider_reason END,
			amount_cents = CASE WHEN EXCLUDED.amount_cents > 0
				THEN EXCLUDED.amount_cents ELSE invoice_charge_attempts.amount_cents END,
			updated_at = now()`,
		tenantID, invoiceID, piID, outcome, reason, amountCents)
	if err != nil {
		return fmt.Errorf("upsert charge attempt in settle tx: %w", err)
	}
	return nil
}

// RecordChargeAttempt writes one charge attempt (ADR-102), upserting by
// PaymentIntent id: the create-time writer inserts the row and the
// settle paths (webhook, inline, reconciler) resolve its outcome in
// place — one attempt, one row. Monotonicity: 'succeeded' is terminal
// (a settle can never un-succeed an attempt); every other outcome may
// advance, including failed → succeeded (3DS second try on one PI).
// sim_effective_at and trigger_source are set at insert and never
// overwritten — the settle path's wall-clock context must not strip an
// attempt's billing-axis anchor. Empty-PI attempts (the PI create
// itself failed) insert-only; there is no twin to dedup against.
func (s *PostgresStore) RecordChargeAttempt(ctx context.Context, tenantID string, a domain.InvoiceChargeAttempt) error {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)

	if a.StripePaymentIntentID == "" {
		_, err = tx.ExecContext(ctx, `
			INSERT INTO invoice_charge_attempts
				(tenant_id, invoice_id, stripe_payment_intent_id, trigger_source,
				 outcome, provider_reason, amount_cents, occurred_at, sim_effective_at)
			VALUES ($1, $2, '', $3, $4, $5, $6, $7, $8)`,
			tenantID, a.InvoiceID, a.Trigger, a.Outcome, a.ProviderReason,
			a.AmountCents, a.OccurredAt, postgres.NullableTime(a.SimEffectiveAt),
		)
	} else {
		_, err = tx.ExecContext(ctx, `
			INSERT INTO invoice_charge_attempts
				(tenant_id, invoice_id, stripe_payment_intent_id, trigger_source,
				 outcome, provider_reason, amount_cents, occurred_at, sim_effective_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT (tenant_id, stripe_payment_intent_id)
				WHERE stripe_payment_intent_id <> ''
			DO UPDATE SET
				outcome = CASE WHEN invoice_charge_attempts.outcome = 'succeeded'
					THEN invoice_charge_attempts.outcome ELSE EXCLUDED.outcome END,
				provider_reason = CASE WHEN EXCLUDED.provider_reason <> ''
					THEN EXCLUDED.provider_reason ELSE invoice_charge_attempts.provider_reason END,
				amount_cents = CASE WHEN EXCLUDED.amount_cents > 0
					THEN EXCLUDED.amount_cents ELSE invoice_charge_attempts.amount_cents END,
				sim_effective_at = COALESCE(invoice_charge_attempts.sim_effective_at, EXCLUDED.sim_effective_at),
				updated_at = now()`,
			tenantID, a.InvoiceID, a.StripePaymentIntentID, a.Trigger, a.Outcome,
			a.ProviderReason, a.AmountCents, a.OccurredAt, postgres.NullableTime(a.SimEffectiveAt),
		)
	}
	if err != nil {
		return fmt.Errorf("record charge attempt: %w", err)
	}
	return tx.Commit()
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
		       livemode, created_at, updated_at
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
			&simAt, &a.Livemode, &a.CreatedAt, &a.UpdatedAt); err != nil {
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
