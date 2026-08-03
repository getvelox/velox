package creditnote

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

type PostgresStore struct {
	db *postgres.DB
}

func NewPostgresStore(db *postgres.DB) *PostgresStore {
	return &PostgresStore{db: db}
}

// cnReadCols is the canonical credit-note SELECT/RETURNING column list.
// Every read site uses it with cnScanDest so a schema change (e.g. adding
// is_simulated) updates one place and can't drift a positional scan.
const cnReadCols = `id, tenant_id, invoice_id, customer_id, credit_note_number,
	status, reason, subtotal_cents, tax_amount_cents, total_cents,
	refund_amount_cents, credit_amount_cents, out_of_band_amount_cents,
	currency, issued_at, voided_at, refund_status, COALESCE(stripe_refund_id,''),
	COALESCE(tax_transaction_id,''), is_simulated, issue_pending,
	tax_reversal_pending, commit_retired_cents,
	metadata, created_at, updated_at, recorded_at`

// cnScanDest returns Scan destinations in cnReadCols order. metaJSON is the
// raw metadata bytes the caller unmarshals into cn.Metadata after Scan.
func cnScanDest(cn *domain.CreditNote, metaJSON *[]byte) []any {
	return []any{
		&cn.ID, &cn.TenantID, &cn.InvoiceID, &cn.CustomerID, &cn.CreditNoteNumber,
		&cn.Status, &cn.Reason, &cn.SubtotalCents, &cn.TaxAmountCents, &cn.TotalCents,
		&cn.RefundAmountCents, &cn.CreditAmountCents, &cn.OutOfBandAmountCents,
		&cn.Currency, &cn.IssuedAt, &cn.VoidedAt, &cn.RefundStatus, &cn.StripeRefundID,
		&cn.TaxTransactionID, &cn.IsSimulated, &cn.IssuePending,
		&cn.TaxReversalPending, &cn.CommitRetiredCents,
		metaJSON, &cn.CreatedAt, &cn.UpdatedAt, &cn.RecordedAt,
	}
}

func (s *PostgresStore) Create(ctx context.Context, tenantID string, cn domain.CreditNote) (domain.CreditNote, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.CreditNote{}, err
	}
	defer postgres.Rollback(tx)

	created, err := s.insertCreditNoteTx(ctx, tx, tenantID, cn)
	if err != nil {
		return domain.CreditNote{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.CreditNote{}, err
	}
	return created, nil
}

// CreateUnderInvoiceLock serializes credit-note creation per invoice. It takes
// a transaction-scoped advisory lock keyed on (tenant, invoice), lists the
// invoice's existing credit notes inside the same transaction, hands them to
// `build` (which runs the over-credit / over-refund cap checks and returns the
// credit note to insert), and inserts the note PLUS its line items — all in
// the one transaction. A concurrent Create for the same invoice blocks on the
// lock until this transaction commits, then sees this credit note in its own
// list, so the caps can't be bypassed by a time-of-check/time-of-use race.
// The lock releases automatically on commit.
//
// Lines commit with the header or not at all. Pre-fix the header committed
// here and each line was inserted afterwards in its own transaction — a crash
// or failure between them left an orphan credit note with a non-zero total
// and zero lines, which Issue() would still act on.
func (s *PostgresStore) CreateUnderInvoiceLock(ctx context.Context, tenantID, invoiceID string, lines []domain.CreditNoteLineItem, build func(existing []domain.CreditNote) (domain.CreditNote, error)) (domain.CreditNote, error) {
	return s.CreateUnderInvoiceLockAudited(ctx, tenantID, invoiceID, lines, build, nil)
}

// CreateUnderInvoiceLockAudited is CreateUnderInvoiceLock with an in-tx audit
// emission (ADR-090): emit runs on the SAME transaction as the header+lines
// insert and receives the CREATED row (so it can stamp the generated id and
// number). An emission failure aborts the create — a credit note never commits
// without its evidence, and evidence never commits without the note. It fires
// exactly once, and only on the path that actually inserted a row (a build
// rejection returns before it).
func (s *PostgresStore) CreateUnderInvoiceLockAudited(ctx context.Context, tenantID, invoiceID string, lines []domain.CreditNoteLineItem, build func(existing []domain.CreditNote) (domain.CreditNote, error), emit func(tx *sql.Tx, created domain.CreditNote) error) (domain.CreditNote, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.CreditNote{}, err
	}
	defer postgres.Rollback(tx)

	created, err := s.createUnderInvoiceLockInTx(ctx, tx, tenantID, invoiceID, lines, build)
	if err != nil {
		return domain.CreditNote{}, err
	}
	if emit != nil {
		if err := emit(tx, created); err != nil {
			return domain.CreditNote{}, fmt.Errorf("audit emission: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return domain.CreditNote{}, err
	}
	return created, nil
}

// CreateUnderInvoiceLockTx is CreateUnderInvoiceLock on the CALLER's transaction
// (coordinator-owned *sql.Tx, ADR-056) — so the credit note + its lines commit
// ATOMICALLY with the caller's other writes (e.g. a subscription item delete in
// atomicRemoveItemWithProration). A failure on either side rolls back both: the
// item change AND the clawback obligation, closing the post-commit
// fire-and-forget gap where a removed item could leave the customer
// un-credited. The per-invoice advisory lock is taken on the caller's tx and
// releases when it commits. The caller owns Begin/Commit/Rollback.
func (s *PostgresStore) CreateUnderInvoiceLockTx(ctx context.Context, tx *sql.Tx, tenantID, invoiceID string, lines []domain.CreditNoteLineItem, build func(existing []domain.CreditNote) (domain.CreditNote, error)) (domain.CreditNote, error) {
	return s.CreateUnderInvoiceLockTxAudited(ctx, tx, tenantID, invoiceID, lines, build, nil)
}

// CreateUnderInvoiceLockTxAudited is CreateUnderInvoiceLockTx with the ADR-090
// in-tx emission: the create row rides the CALLER's coordinator tx, exactly as
// the credit note itself does — so the own-tx and caller-tx create paths carry
// the same evidence (the own-tx/caller-tx divergence ADR-090 §3 closed for
// grants), and an emission failure rolls the caller's whole change back.
func (s *PostgresStore) CreateUnderInvoiceLockTxAudited(ctx context.Context, tx *sql.Tx, tenantID, invoiceID string, lines []domain.CreditNoteLineItem, build func(existing []domain.CreditNote) (domain.CreditNote, error), emit func(tx *sql.Tx, created domain.CreditNote) error) (domain.CreditNote, error) {
	created, err := s.createUnderInvoiceLockInTx(ctx, tx, tenantID, invoiceID, lines, build)
	if err != nil {
		return domain.CreditNote{}, err
	}
	if emit != nil {
		if err := emit(tx, created); err != nil {
			return domain.CreditNote{}, fmt.Errorf("audit emission: %w", err)
		}
	}
	return created, nil
}

func (s *PostgresStore) createUnderInvoiceLockInTx(ctx context.Context, tx *sql.Tx, tenantID, invoiceID string, lines []domain.CreditNoteLineItem, build func(existing []domain.CreditNote) (domain.CreditNote, error)) (domain.CreditNote, error) {
	if _, err := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, tenantID+":"+invoiceID,
	); err != nil {
		return domain.CreditNote{}, fmt.Errorf("acquire credit-note invoice lock: %w", err)
	}

	existing, err := s.listByInvoiceTx(ctx, tx, invoiceID)
	if err != nil {
		return domain.CreditNote{}, err
	}
	cn, err := build(existing)
	if err != nil {
		return domain.CreditNote{}, err
	}
	created, err := s.insertCreditNoteTx(ctx, tx, tenantID, cn)
	if err != nil {
		return domain.CreditNote{}, err
	}
	for i, line := range lines {
		line.CreditNoteID = created.ID
		if _, err := s.insertLineItemTx(ctx, tx, tenantID, line); err != nil {
			return domain.CreditNote{}, fmt.Errorf("create line item %d: %w", i+1, err)
		}
	}
	return created, nil
}

// CreateUnderInvoiceLockDynamicTx is CreateUnderInvoiceLockTx for callers
// whose LINE AMOUNTS depend on state that must be read UNDER locks taken
// inside build (ADR-080 commit relief: the CN's cash amount is derived from
// the grant's live drawdown state, which is only frozen once build takes
// the customer advisory + grant row locks — after this method has taken
// the per-invoice CN lock, preserving the invoice-CN → customer-advisory →
// ledger-row order). build returns the header AND its lines.
func (s *PostgresStore) CreateUnderInvoiceLockDynamicTx(ctx context.Context, tx *sql.Tx, tenantID, invoiceID string, build func(existing []domain.CreditNote) (domain.CreditNote, []domain.CreditNoteLineItem, error)) (domain.CreditNote, error) {
	if _, err := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, tenantID+":"+invoiceID,
	); err != nil {
		return domain.CreditNote{}, fmt.Errorf("acquire credit-note invoice lock: %w", err)
	}
	existing, err := s.listByInvoiceTx(ctx, tx, invoiceID)
	if err != nil {
		return domain.CreditNote{}, err
	}
	cn, lines, err := build(existing)
	if err != nil {
		return domain.CreditNote{}, err
	}
	created, err := s.insertCreditNoteTx(ctx, tx, tenantID, cn)
	if err != nil {
		return domain.CreditNote{}, err
	}
	for i, line := range lines {
		line.CreditNoteID = created.ID
		if _, err := s.insertLineItemTx(ctx, tx, tenantID, line); err != nil {
			return domain.CreditNote{}, fmt.Errorf("create line item %d: %w", i+1, err)
		}
	}
	return created, nil
}

// ListPendingClawbackDrafts returns auto-issue clawback drafts (issue_pending,
// still status='draft') that are READY to issue, for the RetryPendingClawbackIssue
// reconciler. Cross-tenant (TxBypass) + scoped by livemode, mirroring
// ListPendingTaxCommit; each row carries its TenantID so the service re-issues
// under the right tenant.
//
// "Ready" = the source invoice's payment is NOT in flight (processing/unknown).
// This is the defer-until-settle gate (ADR-059): an automated clawback against a
// source whose charge is still settling is created as a draft but NOT issued —
// issuing it now would reduce amount_due before MarkPaid records the captured
// amount (under-record) or relieve a charge that may yet succeed. The draft waits
// here until the source reaches a terminal payment state, at which point Issue()'s
// payment_status branch picks the right channel (paid→credit, failed→reduce).
//
// There is deliberately NO time window: an off-session SCA source can sit
// 'processing' for days, so a fixed horizon (the prior 24h bound) would age a
// legitimately-deferred draft out of the scan and silently drop the customer's
// relief. The NOT-EXISTS source-terminal gate is the only eligibility predicate;
// once a draft's source settles it becomes eligible regardless of draft age.
//
// WHY THE STATUS TERM (2026-07-31). "Waits until the source settles" assumed
// every source eventually settles. ADR-107 broke that assumption: a PARKED
// invoice ('unknown' with no PaymentIntent id) never settles, so its draft was
// excluded here permanently — no issue, no void, no error log, and the gauge
// counting it as "pending" forever. Writing the invoice off did not release it
// either, because this predicate tested payment_status ALONE and a write-off
// does not change payment_status: the payment question stays open even though
// the invoice is closed.
//
// The status term says a TERMINAL invoice no longer defers anything. It does
// not decide the outcome — Issue()'s orphan guard already voids a draft whose
// source was voided or written off ("source annulled before issue", audited,
// no money movement). That code was correct and simply unreachable, because
// this query never handed the draft to it. The bug was eligibility, not
// behaviour, which is why the fix is a predicate rather than a new rule.
//
// OBSERVABILITY (2026-07-06 truth pass): a draft deferred behind an
// in-flight source is EXCLUDED by the NOT-EXISTS above, so it never
// reaches RetryPendingClawbackIssue's per-row logging — deferred drafts
// produce NO error logs by design (the prior claim here that
// repeated ERROR logs surface them was false). The
// velox_creditnote_pending_issue_drafts gauge is the surface: alert on
// sustained growth/age (see docs/ops/runbook.md), not presence.
func (s *PostgresStore) ListPendingClawbackDrafts(ctx context.Context, batch int, livemode bool) ([]domain.CreditNote, error) {
	if batch <= 0 {
		batch = 50
	}
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	rows, err := tx.QueryContext(ctx, `SELECT `+cnReadCols+`
		FROM credit_notes
		WHERE livemode = $1
		  AND is_simulated = false
		  AND issue_pending
		  AND status = 'draft'
		  AND NOT EXISTS (
		    SELECT 1 FROM invoices i
		    WHERE i.id = credit_notes.invoice_id
		      AND i.tenant_id = credit_notes.tenant_id
		      AND i.payment_status IN ('processing', 'unknown')
		      AND i.status NOT IN ('voided', 'uncollectible')
		  )
		ORDER BY updated_at ASC
		LIMIT $2`, livemode, batch)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var notes []domain.CreditNote
	for rows.Next() {
		var cn domain.CreditNote
		var metaJSON []byte
		if err := rows.Scan(cnScanDest(&cn, &metaJSON)...); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(metaJSON, &cn.Metadata)
		notes = append(notes, cn)
	}
	return notes, rows.Err()
}

// ListPendingClawbackDraftsForClock is the catchup counterpart to
// ListPendingClawbackDrafts (ADR-029 disjoint flows): it returns the pending
// auto-issue clawback drafts owned by ONE test clock's simulated customers, so
// the advance path can re-issue a deferred simulated clawback in simulated time
// — instead of leaving it to the wall-clock reconciler, which no longer touches
// simulated rows (is_simulated=false above). Scoped to the clock's customer set
// (test_clock_id = $2), with the same in-flight-source defer (ADR-059) as the
// wall-clock scan.
func (s *PostgresStore) ListPendingClawbackDraftsForClock(ctx context.Context, tenantID, clockID string, batch int) ([]domain.CreditNote, error) {
	if batch <= 0 {
		batch = 50
	}
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	rows, err := tx.QueryContext(ctx, `SELECT `+cnReadCols+`
		FROM credit_notes
		WHERE tenant_id = $1
		  AND customer_id IN (SELECT id FROM customers WHERE test_clock_id = $2)
		  AND issue_pending
		  AND status = 'draft'
		  AND NOT EXISTS (
		    SELECT 1 FROM invoices i
		    WHERE i.id = credit_notes.invoice_id
		      AND i.tenant_id = credit_notes.tenant_id
		      AND i.payment_status IN ('processing', 'unknown')
		      AND i.status NOT IN ('voided', 'uncollectible')
		  )
		ORDER BY updated_at ASC
		LIMIT $3`, tenantID, clockID, batch)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var notes []domain.CreditNote
	for rows.Next() {
		var cn domain.CreditNote
		var metaJSON []byte
		if err := rows.Scan(cnScanDest(&cn, &metaJSON)...); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(metaJSON, &cn.Metadata)
		notes = append(notes, cn)
	}
	return notes, rows.Err()
}

func (s *PostgresStore) insertCreditNoteTx(ctx context.Context, tx *sql.Tx, tenantID string, cn domain.CreditNote) (domain.CreditNote, error) {
	id := postgres.NewID("vlx_cn")
	// Honors ctx-bound effective-now (ADR-030) — credit notes against
	// invoices on clock-pinned subs inherit sim-time. Falls back to
	// wall-clock for unbound ctx (production credit notes).
	now := clock.Now(ctx)
	metaJSON, _ := json.Marshal(cn.Metadata)
	if cn.Metadata == nil {
		metaJSON = []byte("{}")
	}

	err := tx.QueryRowContext(ctx, `
		INSERT INTO credit_notes (id, tenant_id, invoice_id, customer_id, credit_note_number,
			status, reason, subtotal_cents, tax_amount_cents, total_cents,
			refund_amount_cents, credit_amount_cents, out_of_band_amount_cents,
			currency, refund_status, is_simulated, issue_pending, commit_retired_cents, metadata, created_at, updated_at, recorded_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$20,now())
		RETURNING `+cnReadCols,
		id, tenantID, cn.InvoiceID, cn.CustomerID, cn.CreditNoteNumber,
		cn.Status, cn.Reason, cn.SubtotalCents, cn.TaxAmountCents, cn.TotalCents,
		cn.RefundAmountCents, cn.CreditAmountCents, cn.OutOfBandAmountCents,
		cn.Currency, cn.RefundStatus, cn.IsSimulated, cn.IssuePending, cn.CommitRetiredCents, metaJSON, now,
	).Scan(cnScanDest(&cn, &metaJSON)...)
	if err != nil {
		return domain.CreditNote{}, err
	}
	_ = json.Unmarshal(metaJSON, &cn.Metadata)
	return cn, nil
}

// listByInvoiceTx lists every credit note for an invoice inside the caller's
// transaction (used under the advisory lock in CreateUnderInvoiceLock). Mirrors
// List's column projection.
func (s *PostgresStore) listByInvoiceTx(ctx context.Context, tx *sql.Tx, invoiceID string) ([]domain.CreditNote, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+cnReadCols+`
		FROM credit_notes WHERE invoice_id = $1`, invoiceID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var notes []domain.CreditNote
	for rows.Next() {
		var cn domain.CreditNote
		var metaJSON []byte
		if err := rows.Scan(cnScanDest(&cn, &metaJSON)...); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(metaJSON, &cn.Metadata)
		notes = append(notes, cn)
	}
	return notes, rows.Err()
}

func (s *PostgresStore) Get(ctx context.Context, tenantID, id string) (domain.CreditNote, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.CreditNote{}, err
	}
	defer postgres.Rollback(tx)

	var cn domain.CreditNote
	var metaJSON []byte
	err = tx.QueryRowContext(ctx, `SELECT `+cnReadCols+`
		FROM credit_notes WHERE id = $1
	`, id).Scan(cnScanDest(&cn, &metaJSON)...)
	if err == sql.ErrNoRows {
		return domain.CreditNote{}, errs.ErrNotFound
	}
	if err != nil {
		return domain.CreditNote{}, err
	}
	_ = json.Unmarshal(metaJSON, &cn.Metadata)
	return cn, nil
}

func (s *PostgresStore) List(ctx context.Context, filter ListFilter) ([]domain.CreditNote, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, filter.TenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	// Default 50, clamp to 100 — no-silent-fallbacks principle.
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	} else if limit > 100 {
		limit = 100
	}

	query := `SELECT ` + cnReadCols + ` FROM credit_notes`
	args := []any{}
	idx := 1
	clauses := []string{}

	if filter.InvoiceID != "" {
		clauses = append(clauses, fmt.Sprintf("invoice_id = $%d", idx))
		args = append(args, filter.InvoiceID)
		idx++
	}
	if filter.CustomerID != "" {
		clauses = append(clauses, fmt.Sprintf("customer_id = $%d", idx))
		args = append(args, filter.CustomerID)
		idx++
	}
	if filter.Status != "" {
		clauses = append(clauses, fmt.Sprintf("status = $%d", idx))
		args = append(args, filter.Status)
		idx++
	}
	if filter.RefundStatus == "needs_attention" {
		// failed (terminal — money returned to the platform, customer un-refunded)
		// OR a 'pending' that's genuinely STUCK (>72h ≈ 3 business days). Fresh
		// pending is normal async settlement and must NOT alert (it would flood
		// the dashboard once true-pending is recorded). is_simulated excluded —
		// test-clock CNs aren't an operator obligation. Literal, no arg → no
		// injection surface.
		clauses = append(clauses, "status = 'issued' AND (refund_status = 'failed' OR (refund_status = 'pending' AND updated_at < now() - interval '72 hours')) AND is_simulated = false")
	} else if filter.RefundStatus != "" {
		clauses = append(clauses, fmt.Sprintf("refund_status = $%d", idx))
		args = append(args, filter.RefundStatus)
		idx++
	}
	if len(clauses) > 0 {
		query += " WHERE "
		for i, c := range clauses {
			if i > 0 {
				query += " AND "
			}
			query += c
		}
	}
	query += fmt.Sprintf(" ORDER BY %s LIMIT $%d OFFSET $%d", creditNoteOrderBy(filter.Sort, filter.SortDir), idx, idx+1)
	args = append(args, limit, filter.Offset)

	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var notes []domain.CreditNote
	for rows.Next() {
		var cn domain.CreditNote
		var metaJSON []byte
		if err := rows.Scan(cnScanDest(&cn, &metaJSON)...); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(metaJSON, &cn.Metadata)
		notes = append(notes, cn)
	}
	return notes, rows.Err()
}

// TransitionStatus atomically flips a credit note from `from` to `to` only if
// it is currently in `from`, returning whether this call won the transition.
// The conditional UPDATE ... WHERE status=$from is the compare-and-swap that
// serializes concurrent/retried Issue() calls: exactly one caller gets won=true
// and proceeds to the (non-idempotent) amount_due reduction; the losers get
// won=false and must not re-apply.
// BeginTx opens an RLS-scoped (TxTenant) coordinator transaction the
// creditnote Service owns for Issue(): the draft→issued CAS and the internal
// money effect (amount_due reduction or credit grant) run on this one tx and
// commit/roll back together (ADR-056 / ADR-061). The caller owns
// Commit/Rollback. Cross-domain stores (invoice, credit) run their *Tx methods
// on this same handle — one Postgres, one transaction, one RLS context.
func (s *PostgresStore) BeginTx(ctx context.Context, tenantID string) (*sql.Tx, error) {
	return s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
}

func (s *PostgresStore) TransitionStatus(ctx context.Context, tenantID, id string, from, to domain.CreditNoteStatus) (bool, error) {
	return s.TransitionStatusAudited(ctx, tenantID, id, from, to, nil)
}

// TransitionStatusAudited runs the caller-supplied audit emission on the
// same tx as the CAS flip (ADR-090); emit fires only when the CAS won —
// a lost transition mutates nothing and records nothing.
func (s *PostgresStore) TransitionStatusAudited(ctx context.Context, tenantID, id string, from, to domain.CreditNoteStatus, emit func(tx *sql.Tx) error) (bool, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return false, err
	}
	defer postgres.Rollback(tx)

	won, err := s.TransitionStatusTx(ctx, tx, tenantID, id, from, to)
	if err != nil {
		return false, err
	}
	if won && emit != nil {
		if err := emit(tx); err != nil {
			return false, fmt.Errorf("audit emission: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return won, nil
}

// TransitionStatusTx is TransitionStatus on the caller's coordinator tx: the
// compare-and-swap runs but is NOT committed here, so Issue() can fold the
// status flip and the internal money effect into one atomic commit. The CAS
// (UPDATE ... WHERE status=$from) still serializes concurrent/retried Issue()
// calls — exactly one caller gets won=true.
func (s *PostgresStore) TransitionStatusTx(ctx context.Context, tx *sql.Tx, tenantID, id string, from, to domain.CreditNoteStatus) (bool, error) {
	now := clock.Now(ctx)
	var issuedAt, voidedAt *time.Time
	if to == domain.CreditNoteIssued {
		issuedAt = &now
	}
	if to == domain.CreditNoteVoided {
		voidedAt = &now
	}

	// issue_pending's job ("draft awaiting auto-issue") ends with any exit
	// from draft, but nothing cleared it — every reader gates on
	// status='draft' so behavior was unaffected, yet the API served
	// issue_pending=true on issued CNs forever (FLOW B17 walk, 2026-07-21).
	res, err := tx.ExecContext(ctx, `
		UPDATE credit_notes SET status=$1, issued_at=COALESCE($2, issued_at),
			voided_at=COALESCE($3, voided_at), issue_pending=false, updated_at=$4
		WHERE id=$5 AND status=$6`,
		to, postgres.NullableTime(issuedAt), postgres.NullableTime(voidedAt), now, id, from)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// UpdateStatus (an UNGUARDED status write: `UPDATE … WHERE id=$1`, no
// from-status predicate) was DELETED with the ADR-090 void emission. It was
// the second status writer alongside TransitionStatus's CAS, and its only
// caller (Service.Void) now goes through TransitionStatusAudited: an audit row
// may only evidence a flip that provably happened, which a blind write cannot
// prove. Re-adding an unguarded writer here re-opens both the fabricated-row
// class and the read-then-blind-write race Void carried (see Service.Void).

// SetTaxTransaction persists the upstream reversal transaction id (Stripe:
// tx_xxx) returned by the tax provider at Issue time. Idempotency guard:
// the credit note service checks for a non-empty value before calling
// Provider.Reverse so a retried Issue does not create duplicate reversals.
func (s *PostgresStore) SetTaxTransaction(ctx context.Context, tenantID, id string, taxTransactionID string) error {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)

	_, err = tx.ExecContext(ctx, `
		UPDATE credit_notes SET tax_transaction_id = $1, updated_at = $2 WHERE id = $3`,
		taxTransactionID, clock.Now(ctx), id)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *PostgresStore) UpdateRefundStatus(ctx context.Context, tenantID, id string, status domain.RefundStatus, stripeRefundID string) error {
	return s.UpdateRefundStatusAudited(ctx, tenantID, id, status, stripeRefundID, nil)
}

// UpdateRefundStatusAudited is UpdateRefundStatus with an in-tx audit emission
// (ADR-090), used by the operator refund RETRY (POST /credit-notes/{id}/retry-refund).
//
// The prior refund state is read UNDER THE ROW LOCK inside this transaction —
// not from a pre-tx snapshot — so `prior` is the state the write actually
// replaced.
//
// emit fires UNCONDITIONALLY, and is handed `changed` (did the persisted state
// actually move?). The store REPORTS; it does not DECIDE — the CALLER decides
// whether a no-move write is audit-worthy, because the answer differs by caller:
//
//   - RetryRefund (creditnote.Service): emits even when the status did not move.
//     The operator's retry HIT STRIPE; the action is the fact. The row carries
//     status_changed=false.
//   - runPostIssueExternalLegs: the post-issue refund leg, same store method.
//
// The Stripe WEBHOOK is NOT a caller of this method — it goes through
// ApplyRefundWebhookStatus, which is where the "a redelivery that moves nothing
// writes no row" rule lives. An earlier version of this comment named the webhook
// as one of the two callers here, which sent the reader to the wrong function for
// the rule it was looking for.
//
// The row is still touched (updated_at) on a no-op so the "stuck pending >72h"
// attention window keeps its existing semantics.
//
// An emission failure aborts the write (shared fate) — the refund state and its
// evidence commit together or not at all.
func (s *PostgresStore) UpdateRefundStatusAudited(ctx context.Context, tenantID, id string, status domain.RefundStatus, stripeRefundID string, emit func(tx *sql.Tx, updated domain.CreditNote, prior domain.RefundStatus, changed bool) error) error {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)

	var cur domain.CreditNote
	err = tx.QueryRowContext(ctx, `
		SELECT id, credit_note_number, customer_id, invoice_id, refund_status, COALESCE(stripe_refund_id,'')
		FROM credit_notes WHERE id=$1 FOR UPDATE`, id).
		Scan(&cur.ID, &cur.CreditNoteNumber, &cur.CustomerID, &cur.InvoiceID, &cur.RefundStatus, &cur.StripeRefundID)
	if errors.Is(err, sql.ErrNoRows) {
		return errs.ErrNotFound
	}
	if err != nil {
		return err
	}

	// Same-identity regressions are refused HERE, not only in the webhook
	// writer (ADR-063 amendment, 2026-08-04). The window this closes:
	// RetryRefund reads provider truth (GetRefund) and then persists it —
	// a refund.failed webhook landing between the read and this write
	// would be overwritten by the now-stale non-terminal answer, and no
	// webhook would ever correct it back (event-id dedup swallows the
	// redelivery). Before the reconcile-first flow the same overwrite
	// happened through an idempotency-key replay's SAVED create response.
	// Three deliberate asymmetries with the webhook guard:
	//   - a NEW provider identity (different refund id) may write ANY
	//     status: a fresh attempt legitimately restarts the lifecycle
	//     (failed → pending is exactly what a successful re-drive looks
	//     like);
	//   - a SAME-VALUE persist still runs the UPDATE so updated_at moves —
	//     the "stuck pending >72h" attention window resets on a fresh
	//     provider confirmation, per the contract above;
	//   - the refusal still EMITS (changed=false): the operator action or
	//     reconcile that hit the provider is the fact, even when its stale
	//     answer was discarded.
	sameIdentity := stripeRefundID == "" || stripeRefundID == cur.StripeRefundID
	if sameIdentity && refundStatusRegression(cur.RefundStatus, status) {
		slog.WarnContext(ctx, "refused same-refund status regression",
			"credit_note_id", id, "stripe_refund_id", cur.StripeRefundID,
			"current", string(cur.RefundStatus), "attempted", string(status))
		if emit != nil {
			if err := emit(tx, cur, cur.RefundStatus, false); err != nil {
				return fmt.Errorf("audit emission: %w", err)
			}
		}
		return tx.Commit()
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE credit_notes SET refund_status=$1, stripe_refund_id=COALESCE(NULLIF($2,''), stripe_refund_id),
			updated_at=$3
		WHERE id=$4`,
		status, stripeRefundID, clock.Now(ctx), id); err != nil {
		return err
	}

	// The persisted result of the COALESCE above: an empty stripeRefundID
	// leaves the stored id untouched.
	updated := cur
	updated.RefundStatus = status
	if stripeRefundID != "" {
		updated.StripeRefundID = stripeRefundID
	}
	// The store REPORTS whether the persisted state moved; it does not decide
	// whether that is audit-worthy. The two callers want opposite semantics:
	// a webhook no-op is a non-event (no row), while an operator's retry hit
	// Stripe and IS the event whether or not the status moved. Handing
	// `changed` to the closure keeps that judgement where the intent lives.
	changed := updated.RefundStatus != cur.RefundStatus || updated.StripeRefundID != cur.StripeRefundID
	if emit != nil {
		if err := emit(tx, updated, cur.RefundStatus, changed); err != nil {
			return fmt.Errorf("audit emission: %w", err)
		}
	}
	return tx.Commit()
}

// refundStatusRegression reports whether writing next over prior — for the
// SAME provider refund — would rewind provider truth. Mirrors the webhook
// guard's precedence (failed absorbing > succeeded > pending) minus the
// same-value case, which is a benign re-confirmation, not a regression.
func refundStatusRegression(prior, next domain.RefundStatus) bool {
	if prior == next {
		return false
	}
	if prior == domain.RefundFailed {
		return true // failed is absorbing for one refund id
	}
	if prior == domain.RefundSucceeded && next != domain.RefundFailed {
		return true // succeeded yields only to failed (bank reject)
	}
	return false
}

func (s *PostgresStore) ApplyRefundWebhookStatus(ctx context.Context, tenantID, stripeRefundID string, status domain.RefundStatus) error {
	return s.ApplyRefundWebhookStatusAudited(ctx, tenantID, stripeRefundID, status, nil)
}

// ApplyRefundWebhookStatusAudited is ApplyRefundWebhookStatus with an in-tx
// audit emission hook (ADR-090). emit fires ONLY when the monotonic guard
// actually flipped a row — a stale/no-op redelivery records nothing, so the
// log never claims a transition that didn't happen. The flipped CN's
// identifiers ride RETURNING so the emission can reference them.
func (s *PostgresStore) ApplyRefundWebhookStatusAudited(ctx context.Context, tenantID, stripeRefundID string, status domain.RefundStatus, emit func(tx *sql.Tx, cn domain.CreditNote) error) error {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)

	// Monotonic precedence (webhooks have no ordering guarantee):
	//   failed  > succeeded > pending
	//   - 'failed' is ABSORBING — once recorded it never changes (the customer
	//     was not refunded; a stale 'succeeded' redelivery must not un-fail it).
	//   - 'succeeded' yields ONLY to 'failed' — Stripe can legitimately move a
	//     refund succeeded→failed (bank rejects an initially-accepted refund), so
	//     a later 'failed' must win; a stale 'pending' must not clobber succeeded.
	//   - 'pending' yields to any terminal.
	// Same-value re-delivery of ANY status (incl. pending→pending, which
	// the two monotonic clauses alone would let through) is a zero-row
	// no-op — the guard below distinguishes it from an unknown refund id,
	// and no audit row is emitted for a transition that didn't happen.
	var cn domain.CreditNote
	flipped := true
	err = tx.QueryRowContext(ctx, `
		UPDATE credit_notes SET refund_status=$1, updated_at=$2
		WHERE stripe_refund_id=$3
		  AND refund_status IS DISTINCT FROM $1
		  AND refund_status <> 'failed'
		  AND ($1 = 'failed' OR refund_status <> 'succeeded')
		RETURNING id, credit_note_number, customer_id, invoice_id`,
		status, clock.Now(ctx), stripeRefundID).Scan(&cn.ID, &cn.CreditNoteNumber, &cn.CustomerID, &cn.InvoiceID)
	if errors.Is(err, sql.ErrNoRows) {
		flipped = false
		// Zero rows = either no CN carries this refund id (foreign/dashboard
		// refund, or not-yet-committed), OR the monotonic guard correctly
		// skipped a stale pending over a terminal. Distinguish: a genuinely
		// missing refund id is ErrNotFound (caller acks/retries); a skipped
		// stale write is a success.
		var exists bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM credit_notes WHERE stripe_refund_id=$1)`, stripeRefundID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return errs.ErrNotFound
		}
	} else if err != nil {
		return err
	}
	if flipped && emit != nil {
		if err := emit(tx, cn); err != nil {
			return fmt.Errorf("audit emission: %w", err)
		}
	}
	return tx.Commit()
}

// UpdateAllocation persists the three-channel allocation. Issue() calls
// this when a CN created against a then-unpaid invoice is issued after
// the invoice became paid — the allocation frozen at Create was all
// zeros, so Issue re-derives it (default: full credit to balance) and
// persists the result here so the dashboard and any retry observe the
// same split Issue acted on.
func (s *PostgresStore) UpdateAllocation(ctx context.Context, tenantID, id string, refundCents, creditCents, outOfBandCents int64) error {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)

	if err := s.UpdateAllocationTx(ctx, tx, tenantID, id, refundCents, creditCents, outOfBandCents); err != nil {
		return err
	}
	return tx.Commit()
}

// UpdateAllocationTx is UpdateAllocation on the caller's coordinator tx, so the
// re-derived allocation commits atomically with the draft→issued CAS and the
// credit grant during Issue().
func (s *PostgresStore) UpdateAllocationTx(ctx context.Context, tx *sql.Tx, tenantID, id string, refundCents, creditCents, outOfBandCents int64) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE credit_notes SET refund_amount_cents=$1, credit_amount_cents=$2,
			out_of_band_amount_cents=$3, updated_at=$4
		WHERE id=$5`,
		refundCents, creditCents, outOfBandCents, clock.Now(ctx), id)
	return err
}

// SetTaxReversalPending flips the recovery marker for a credit note's
// POST-COMMIT upstream tax reversal: set true when the reversal is attempted and
// fails (→ RetryPendingCreditNoteTaxReversal re-drives it with the per-CN key),
// cleared on success. Runs in its own tx — the reversal is a post-commit
// external effect, not part of the issue coordinator tx.
func (s *PostgresStore) SetTaxReversalPending(ctx context.Context, tenantID, id string, pending bool) error {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)

	_, err = tx.ExecContext(ctx, `
		UPDATE credit_notes SET tax_reversal_pending=$1, updated_at=$2 WHERE id=$3`,
		pending, clock.Now(ctx), id)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// ListPendingCreditNoteTaxReversal returns issued credit notes whose POST-COMMIT
// upstream tax reversal still needs to run, cross-tenant + scoped by livemode,
// for RetryPendingCreditNoteTaxReversal. The CN-path counterpart to invoice #310
// RetryPendingTaxReversal, which scans only voided/uncollectible invoices and so
// is structurally blind to a CN reversal stamped on a finalized/paid invoice.
//
// Eligibility is DERIVABLE FROM DURABLE STATE, not dependent on a marker write
// landing (the #310 design): a row qualifies if EITHER
//
//	(a) tax_reversal_pending — the fast path: the inline attempt failed and set
//	    the marker (cheap, partial-index-backed); recovered until success, or
//	(b) the structural over-remit state — an issued CN with NO reversal stamped
//	    (tax_transaction_id='') against a tax-bearing stripe_tax source invoice.
//
// (b) is the backstop for the compound failure where BOTH ReverseTax AND the
// marker write fail in the same Issue(): the orphan (issued, no tx id, marker
// false) would otherwise be invisible to every sweep — a permanent silent
// over-remit. (b) is bounded to a 24h freshness window (anti-churn + no
// first-deploy re-reversal burst over pre-feature CNs, matching #310); a
// transient failure resolves in seconds-to-minutes, well inside it. The marker
// branch is unbounded (a known owed reversal is never aged out). Re-reversal is
// Stripe-idempotent via the per-CN velox_tax_rev_<cn.ID> reference, so any
// overlap between (a) and (b), or with the inline attempt, dedups to one.
func (s *PostgresStore) ListPendingCreditNoteTaxReversal(ctx context.Context, batch int, livemode bool) ([]domain.CreditNote, error) {
	if batch <= 0 {
		batch = 50
	}
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	rows, err := tx.QueryContext(ctx, `SELECT `+cnReadCols+`
		FROM credit_notes
		WHERE livemode = $1
		  AND status = 'issued'
		  AND is_simulated = false
		  AND (
		        tax_reversal_pending
		     OR (
		            COALESCE(tax_transaction_id, '') = ''
		        AND total_cents > 0
		        AND updated_at > now() - interval '24 hours'
		        AND EXISTS (
		              SELECT 1 FROM invoices i
		              WHERE i.id = credit_notes.invoice_id
		                AND i.tenant_id = credit_notes.tenant_id
		                AND i.tax_provider = 'stripe_tax'
		                AND COALESCE(i.tax_transaction_id, '') <> ''
		            )
		        )
		  )
		ORDER BY updated_at ASC
		LIMIT $2`, livemode, batch)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var notes []domain.CreditNote
	for rows.Next() {
		var cn domain.CreditNote
		var metaJSON []byte
		if err := rows.Scan(cnScanDest(&cn, &metaJSON)...); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(metaJSON, &cn.Metadata)
		notes = append(notes, cn)
	}
	return notes, rows.Err()
}

// insertLineItemTx writes one line item inside the caller's transaction —
// only ever called from CreateUnderInvoiceLock so a line can't exist without
// its committed header (and vice versa).
func (s *PostgresStore) insertLineItemTx(ctx context.Context, tx *sql.Tx, tenantID string, item domain.CreditNoteLineItem) (domain.CreditNoteLineItem, error) {
	id := postgres.NewID("vlx_cnli")
	now := clock.Now(ctx)
	_, err := tx.ExecContext(ctx, `
		INSERT INTO credit_note_line_items (id, credit_note_id, tenant_id,
			invoice_line_item_id, description, quantity, unit_amount_cents, amount_cents, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
	`, id, item.CreditNoteID, tenantID, postgres.NullableString(item.InvoiceLineItemID),
		item.Description, item.Quantity, item.UnitAmountCents, item.AmountCents, now)
	if err != nil {
		return domain.CreditNoteLineItem{}, err
	}
	item.ID = id
	item.TenantID = tenantID
	item.CreatedAt = now
	return item, nil
}

func (s *PostgresStore) ListLineItems(ctx context.Context, tenantID, creditNoteID string) ([]domain.CreditNoteLineItem, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	rows, err := tx.QueryContext(ctx, `
		SELECT id, credit_note_id, tenant_id, COALESCE(invoice_line_item_id,''),
			description, quantity, unit_amount_cents, amount_cents, created_at
		FROM credit_note_line_items WHERE credit_note_id = $1
		ORDER BY created_at ASC
	`, creditNoteID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var items []domain.CreditNoteLineItem
	for rows.Next() {
		var item domain.CreditNoteLineItem
		if err := rows.Scan(&item.ID, &item.CreditNoteID, &item.TenantID, &item.InvoiceLineItemID,
			&item.Description, &item.Quantity, &item.UnitAmountCents, &item.AmountCents, &item.CreatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// creditNoteOrderBy validates sort + dir against a closed allow-list
// and adds a deterministic id tie-break matching the primary
// direction. See invoiceOrderBy for the design rationale.
func creditNoteOrderBy(sort, dir string) string {
	col := creditNoteSortColumn(sort)
	d := "DESC"
	if dir == "asc" {
		d = "ASC"
	}
	return col + " " + d + ", id " + d
}

func creditNoteSortColumn(key string) string {
	switch key {
	case "credit_note_number":
		return "credit_note_number"
	case "total_cents", "amount":
		return "total_cents"
	case "status":
		return "status"
	case "issued_at":
		return "issued_at"
	default:
		return "created_at"
	}
}
