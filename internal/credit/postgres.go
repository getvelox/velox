package credit

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
	db     *postgres.DB
	outbox OutboxEnqueuer
}

func NewPostgresStore(db *postgres.DB) *PostgresStore {
	return &PostgresStore{db: db}
}

// OutboxEnqueuer enqueues an outbound webhook event inside the caller's tx, so
// the event is persisted atomically with the balance change (ADR-040
// transactional outbox). Satisfied by *webhook.OutboxStore; declared as a
// narrow consumer-side interface so the credit store needs no webhook import.
type OutboxEnqueuer interface {
	Enqueue(ctx context.Context, tx *sql.Tx, tenantID, eventType string, payload map[string]any) (string, error)
}

// SetOutboxEnqueuer wires transactional balance-crossing events (ADR-078:
// credit.balance_low / balance_depleted / balance_recovered). Optional —
// when unset, no events are enqueued.
func (s *PostgresStore) SetOutboxEnqueuer(o OutboxEnqueuer) { s.outbox = o }

// emitBalanceCrossings enqueues the ADR-078 balance-VALUE crossing events for
// a before→after balance change, on the caller's tx. Callers MUST hold the
// per-customer advisory lock — that lock is what makes crossings well-ordered
// per customer (every balance writer acquires it: append/adjust/apply/expiry).
//
// Crossings are defined on the raw SUM(amount_cents) balance. Known, accepted
// lag: an expired-but-unswept block keeps SUM positive until the expiry sweep
// retires it — the sweep's expiry entry produces the crossing then
// (minutes-scale, matches the expiry discipline).
func (s *PostgresStore) emitBalanceCrossings(ctx context.Context, tx *sql.Tx, tenantID, customerID string, before, after int64) error {
	if s.outbox == nil || before == after {
		return nil
	}
	base := func() map[string]any {
		return map[string]any{
			"customer_id":   customerID,
			"balance_cents": after,
		}
	}
	if before > 0 && after <= 0 {
		if _, err := s.outbox.Enqueue(ctx, tx, tenantID, domain.EventCreditBalanceDepleted, base()); err != nil {
			return fmt.Errorf("enqueue credit.balance_depleted: %w", err)
		}
	}
	if before <= 0 && after > 0 {
		if _, err := s.outbox.Enqueue(ctx, tx, tenantID, domain.EventCreditBalanceRecovered, base()); err != nil {
			return fmt.Errorf("enqueue credit.balance_recovered: %w", err)
		}
	}
	// Low-threshold crossing: only when the tenant configured one. Read
	// in-tx (RLS-scoped); NULL/absent = low alerts off. Fires alongside
	// depleted when a single write crosses both lines — per-cause events,
	// consumers subscribe to what they need.
	var threshold sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT credit_balance_low_threshold_cents FROM tenant_settings WHERE tenant_id = $1`,
		tenantID,
	).Scan(&threshold); err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("read balance-low threshold: %w", err)
	}
	if threshold.Valid && threshold.Int64 > 0 && before >= threshold.Int64 && after < threshold.Int64 {
		p := base()
		p["threshold_cents"] = threshold.Int64
		if _, err := s.outbox.Enqueue(ctx, tx, tenantID, domain.EventCreditBalanceLow, p); err != nil {
			return fmt.Errorf("enqueue credit.balance_low: %w", err)
		}
	}
	return nil
}

func (s *PostgresStore) AppendEntry(ctx context.Context, tenantID string, entry domain.CreditLedgerEntry) (domain.CreditLedgerEntry, error) {
	return s.AppendEntryAudited(ctx, tenantID, entry, nil)
}

// AppendEntryAudited is AppendEntry with an in-tx audit emission hook: the
// ledger write and its audit row commit or roll back together (ADR-090
// shared fate). The service builds the emission (it owns row content); the
// store owns the transaction and exposes it to the closure — never the
// other way around. emit receives the persisted entry so the audit row can
// reference the store-assigned id. nil emit = unaudited append (engine
// paths that carry their own coverage story).
func (s *PostgresStore) AppendEntryAudited(ctx context.Context, tenantID string, entry domain.CreditLedgerEntry, emit func(tx *sql.Tx, out domain.CreditLedgerEntry) error) (domain.CreditLedgerEntry, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.CreditLedgerEntry{}, err
	}
	defer postgres.Rollback(tx)
	out, err := s.appendEntryInTx(ctx, tx, tenantID, entry)
	if err != nil {
		return domain.CreditLedgerEntry{}, err
	}
	if emit != nil {
		if err := emit(tx, out); err != nil {
			return domain.CreditLedgerEntry{}, fmt.Errorf("audit emission: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return domain.CreditLedgerEntry{}, err
	}
	return out, nil
}

// AppendEntryTx is the in-transaction variant used by the subscription
// handler's atomic AddItem+proration flow: the caller has already
// opened a tx wrapping the sub-item insert + invoice/credit insert,
// and this entry needs to share that tx so a failure here rolls back
// the item add too. ADR-030 atomic-proration follow-through.
func (s *PostgresStore) AppendEntryTx(ctx context.Context, tx *sql.Tx, tenantID string, entry domain.CreditLedgerEntry) (domain.CreditLedgerEntry, error) {
	return s.appendEntryInTx(ctx, tx, tenantID, entry)
}

// appendEntryInTx is the shared body. AppendEntry opens+commits its
// own tx; AppendEntryTx delegates to the caller.
func (s *PostgresStore) appendEntryInTx(ctx context.Context, tx *sql.Tx, tenantID string, entry domain.CreditLedgerEntry) (domain.CreditLedgerEntry, error) {
	// Serialize concurrent AppendEntry-path writers for this customer so two
	// grants can't compute the same balance_after off a stale snapshot. A
	// `SELECT ... FOR UPDATE` over customer_credit_ledger only locks EXISTING
	// rows — for a customer with an empty ledger (the very first grant) it
	// matches zero rows and acquires no lock, so the first concurrent appends
	// raced. A per-customer transaction advisory lock always serializes,
	// regardless of ledger state; it releases automatically on commit/rollback.
	//
	// Since ADR-078 this advisory lock is the UNIVERSAL per-customer
	// serializer: ApplyToInvoiceAtomic and AdjustAtomic acquire it too
	// (before their row locks), so every balance writer is mutually
	// serialized — which is what makes balance_after snapshots and the
	// emitBalanceCrossings before/after computations well-ordered per
	// customer. Global lock order: invoice-row → advisory → ledger-rows.
	//
	// tenant_id is folded into the lock key as defense-in-depth (RLS already
	// scopes the tx to this tenant).
	if _, err := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		tenantID+":"+entry.CustomerID,
	); err != nil {
		return domain.CreditLedgerEntry{}, fmt.Errorf("acquire credit ledger lock: %w", err)
	}
	var currentBalance int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(amount_cents), 0) FROM customer_credit_ledger WHERE tenant_id = $1 AND customer_id = $2`,
		tenantID, entry.CustomerID,
	).Scan(&currentBalance); err != nil {
		return domain.CreditLedgerEntry{}, err
	}

	entry.BalanceAfter = currentBalance + entry.AmountCents
	entry.ID = postgres.NewID("vlx_ccl")

	metaJSON, _ := json.Marshal(entry.Metadata)
	if entry.Metadata == nil {
		metaJSON = []byte("{}")
	}

	// Honor caller-supplied CreatedAt so ledger entries land at the
	// simulated instant the fact occurred — grant at cycle close,
	// expiry at grant.expires_at, usage at the invoice's issue moment.
	// Without this, every entry stamps clock.Now() (= advance-end
	// frozen_time during catchup) and the customer's Credits tab
	// shows a stack of entries all at one timestamp instead of the
	// per-fact chronology. Falls back to clock.Now() when zero so
	// wall-clock callers stay correct.
	createdAt := entry.CreatedAt
	if createdAt.IsZero() {
		createdAt = clock.Now(ctx)
	}
	err := tx.QueryRowContext(ctx, `
		INSERT INTO customer_credit_ledger (id, tenant_id, customer_id, entry_type,
			amount_cents, balance_after, description, invoice_id, expires_at, metadata, created_at,
			source_subscription_id, source_subscription_item_id, source_plan_changed_at, source_change_type,
			source_credit_note_id, source_invoice_reversal_id, grant_kind, source_invoice_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)
		RETURNING id, tenant_id, customer_id, entry_type, amount_cents, balance_after,
			description, COALESCE(invoice_id,''), expires_at, metadata, created_at,
			COALESCE(source_subscription_id,''), COALESCE(source_subscription_item_id,''),
			source_plan_changed_at, COALESCE(source_change_type,''),
			COALESCE(source_credit_note_id,''), COALESCE(grant_kind,''), COALESCE(source_invoice_id,'')
	`, entry.ID, tenantID, entry.CustomerID, entry.EntryType,
		entry.AmountCents, entry.BalanceAfter, entry.Description,
		postgres.NullableString(entry.InvoiceID), postgres.NullableTime(entry.ExpiresAt),
		metaJSON, createdAt,
		postgres.NullableString(entry.SourceSubscriptionID),
		postgres.NullableString(entry.SourceSubscriptionItemID),
		postgres.NullableTime(entry.SourcePlanChangedAt),
		postgres.NullableString(string(entry.SourceChangeType)),
		postgres.NullableString(entry.SourceCreditNoteID),
		postgres.NullableString(entry.SourceInvoiceReversalID),
		postgres.NullableString(string(entry.GrantKind)),
		postgres.NullableString(entry.SourceInvoiceID),
	).Scan(&entry.ID, &entry.TenantID, &entry.CustomerID, &entry.EntryType,
		&entry.AmountCents, &entry.BalanceAfter, &entry.Description,
		&entry.InvoiceID, &entry.ExpiresAt, &metaJSON, &entry.CreatedAt,
		&entry.SourceSubscriptionID, &entry.SourceSubscriptionItemID, &entry.SourcePlanChangedAt,
		(*string)(&entry.SourceChangeType),
		&entry.SourceCreditNoteID, (*string)(&entry.GrantKind), &entry.SourceInvoiceID)

	if err != nil {
		if postgres.IsUniqueViolation(err) {
			// Route by constraint name so callers can distinguish a
			// proration-dedup replay (idempotent: re-fetch and return
			// the prior entry) from a credit-note-dedup replay (same
			// shape, different semantic — caller's responsibility to
			// handle). Pre-2026-05-28 every unique violation collapsed
			// into a generic ErrAlreadyExists and the caller assumed it
			// was proration — silent misroute when the credit-note
			// dedup index fired instead. ADR-030 cross-flow audit
			// 2026-05-28 / feedback_no_silent_fallbacks memory.
			switch postgres.UniqueViolationConstraint(err) {
			case "idx_credit_ledger_proration_dedup":
				return domain.CreditLedgerEntry{}, errs.AlreadyExists("proration_source",
					"credit ledger entry already exists for this item change").WithCode("credit_proration_source_taken")
			case "idx_credit_ledger_credit_note_dedup":
				return domain.CreditLedgerEntry{}, errs.AlreadyExists("credit_note_source",
					"credit ledger entry already exists for this credit note").WithCode("credit_note_source_taken")
			case "idx_credit_ledger_reversal_dedup":
				return domain.CreditLedgerEntry{}, errs.AlreadyExists("invoice_reversal_source",
					"credit ledger reversal already exists for this invoice").WithCode("credit_reversal_source_taken")
			case "idx_credit_ledger_commit_fund_dedup":
				// ADR-078 fund-once backstop. The finalize CAS already
				// guarantees the commit granter runs at most once per
				// invoice, so hitting this index means an invariant broke —
				// and inside the finalize coordinator tx the violation has
				// poisoned the tx anyway (never catch-and-continue here).
				return domain.CreditLedgerEntry{}, errs.AlreadyExists("commit_fund_source",
					"commit grant already exists for this funding invoice").WithCode("credit_commit_fund_taken")
			}
			return domain.CreditLedgerEntry{}, errs.AlreadyExists("",
				fmt.Sprintf("unique constraint %q violated on credit ledger insert",
					postgres.UniqueViolationConstraint(err))).WithCode("credit_unique_violation")
		}
		if postgres.IsForeignKeyViolation(err) {
			return domain.CreditLedgerEntry{}, fmt.Errorf("customer %q not found", entry.CustomerID)
		}
		return domain.CreditLedgerEntry{}, err
	}
	_ = json.Unmarshal(metaJSON, &entry.Metadata)

	// ADR-078 balance-crossing events, atomic with the entry (the advisory
	// lock acquired above keeps crossings well-ordered per customer).
	if err := s.emitBalanceCrossings(ctx, tx, tenantID, entry.CustomerID, currentBalance, entry.BalanceAfter); err != nil {
		return domain.CreditLedgerEntry{}, err
	}
	return entry, nil
}

// ExpireGrantAtomic retires one expired grant: it flips the grant's
// consumed_cents to amount_cents AND appends the -remaining expiry entry in a
// SINGLE transaction. Returns the retired (expired) amount in cents; 0 when
// the grant was already fully consumed or retired by the time the lock was
// held (clean no-op — nothing written).
//
// Why one tx, and why the locks (P14, plan §4.4):
//
//   - The caller's candidate snapshot (ListExpiredGrants*) is stale by
//     construction — it was read on a different, already-closed tx. A
//     backdated ApplyToInvoiceAtomic (at < expires_at passes the eligibility
//     filter on a not-yet-retired grant) can drain the grant between the
//     list read and this call. `remaining` is therefore recomputed HERE,
//     under the same FOR UPDATE row lock the apply/adjust paths take —
//     that row lock is the ONLY mutual exclusion between expiry and
//     apply/adjust (the customer advisory lock below serializes only
//     AppendEntry-path writers; apply/adjust never acquire it).
//   - The consumed_cents flip and the expiry entry must commit together.
//     Entry-then-flip split: a crash in between leaves phantom headroom on
//     a grant the candidate queries no longer re-list — the backdated-apply
//     hole persists forever for that grant. Flip-then-entry split: the
//     remainder is excluded from draining but never deducted from the
//     balance — permanently inflated, unspendable, undrainable.
//   - The flip's `consumed_cents < amount_cents` predicate is the
//     exactly-once gate for the expiry entry: duplicate/overlapping sweeps
//     serialize on the row lock, the loser re-reads remaining == 0 and
//     no-ops. No description-matching dedup needed (the old LIKE filter was
//     operator-visible display copy doubling as an idempotency key).
//
// Lock order is advisory → ledger rows, matching every other writer;
// pg_advisory_xact_lock is reentrant within the tx, so appendEntryInTx
// re-acquiring it below is a no-op.
func (s *PostgresStore) ExpireGrantAtomic(ctx context.Context, tenantID, customerID, grantID string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return 0, err
	}
	defer postgres.Rollback(tx)

	if _, err := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		tenantID+":"+customerID,
	); err != nil {
		return 0, fmt.Errorf("acquire credit ledger lock: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`SELECT 1 FROM customer_credit_ledger WHERE tenant_id = $1 AND customer_id = $2 FOR UPDATE`,
		tenantID, customerID,
	); err != nil {
		return 0, fmt.Errorf("lock credit ledger: %w", err)
	}

	var amount, consumed int64
	var expiresAt sql.NullTime
	err = tx.QueryRowContext(ctx, `
		SELECT amount_cents, consumed_cents, expires_at
		FROM customer_credit_ledger
		WHERE tenant_id = $1 AND customer_id = $2 AND id = $3 AND entry_type = 'grant'
	`, tenantID, customerID, grantID).Scan(&amount, &consumed, &expiresAt)
	if err == sql.ErrNoRows {
		return 0, fmt.Errorf("expire grant %s: %w", grantID, errs.ErrNotFound)
	}
	if err != nil {
		return 0, fmt.Errorf("re-read grant %s under lock: %w", grantID, err)
	}
	remaining := amount - consumed
	if remaining <= 0 {
		// Drained or retired between the candidate snapshot and our lock —
		// nothing left to expire. No entry, no error.
		return 0, nil
	}
	if !expiresAt.Valid {
		// Candidate queries require expires_at IS NOT NULL and nothing
		// un-sets it; reaching here means the caller passed a non-expiring
		// grant. Fail loud rather than fabricate an expiry timestamp.
		return 0, fmt.Errorf("expire grant %s: grant has no expires_at", grantID)
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE customer_credit_ledger
		SET consumed_cents = amount_cents
		WHERE tenant_id = $1 AND id = $2 AND consumed_cents < amount_cents
	`, tenantID, grantID)
	if err != nil {
		return 0, fmt.Errorf("retire grant %s: %w", grantID, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return 0, fmt.Errorf("retire grant %s: rows affected: %w", grantID, err)
	} else if n == 0 {
		// Unreachable after the locked re-read above; kept as the structural
		// exactly-once gate so the entry below can never double-append.
		return 0, nil
	}

	// Stamp the expiry entry at the grant's own expires_at — the simulated
	// (or wall-clock) instant it actually expired — so the customer's
	// Credits timeline shows per-fact chronology, not the sweep tick time.
	if _, err := s.appendEntryInTx(ctx, tx, tenantID, domain.CreditLedgerEntry{
		CustomerID:  customerID,
		EntryType:   domain.CreditExpiry,
		AmountCents: -remaining,
		Description: fmt.Sprintf("Expired grant %s", grantID),
		CreatedAt:   expiresAt.Time,
	}); err != nil {
		return 0, fmt.Errorf("append expiry entry for grant %s: %w", grantID, err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit expiry of grant %s: %w", grantID, err)
	}
	return remaining, nil
}

// AdjustAtomic inserts a manual adjustment entry while holding a row lock on
// the customer's ledger, so the balance check and the insert observe the same
// snapshot. Without the lock, two concurrent deductions can each read the
// full balance, each pass "balance + amount >= 0", and both commit — driving
// the ledger negative.
//
// Per-block consumed_cents attribution (Orb credit-block model). A negative
// adjustment (clawback) FIFO-drains across active positive blocks (grants +
// prior positive adjustments + invoice reversals), bumping each block's
// consumed_cents by the drained amount. Without this attribution, balance
// drops below sum(grant.remaining) and subsequent ApplyToInvoiceAtomic or
// processExpiry calls drive the ledger negative — the customer's $30 balance
// + $80 grant remaining drift documented 2026-05-24. Positive adjustments
// don't drain; they're themselves drainable by future operations because
// ApplyToInvoiceAtomic now selects any positive entry, not only grants.
func (s *PostgresStore) AdjustAtomic(
	ctx context.Context, tenantID, customerID, description string, amountCents int64,
) (domain.CreditLedgerEntry, error) {
	return s.AdjustAtomicAudited(ctx, tenantID, customerID, description, amountCents, nil)
}

// AdjustAtomicAudited is AdjustAtomic with an in-tx audit emission hook —
// same contract as AppendEntryAudited (ADR-090 shared fate).
func (s *PostgresStore) AdjustAtomicAudited(
	ctx context.Context, tenantID, customerID, description string, amountCents int64,
	emit func(tx *sql.Tx, out domain.CreditLedgerEntry) error,
) (domain.CreditLedgerEntry, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.CreditLedgerEntry{}, err
	}
	defer postgres.Rollback(tx)

	// Per-customer advisory lock BEFORE the row locks (ADR-078 global lock
	// order: invoice-row → advisory → ledger-rows; AdjustAtomic takes no
	// invoice locks, so its order starts at the advisory). This closes the
	// grant-vs-adjust unserialized window — grants take the advisory lock
	// but no row locks, so before ADR-078 the two writers shared no lock at
	// all and balance_after / crossing computations could interleave.
	if _, err := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		tenantID+":"+customerID,
	); err != nil {
		return domain.CreditLedgerEntry{}, fmt.Errorf("acquire credit ledger lock: %w", err)
	}

	// Lock customer's ledger rows (defense-in-depth: tenant_id predicate in
	// addition to RLS — see AppendEntry).
	if _, err := tx.ExecContext(ctx,
		`SELECT 1 FROM customer_credit_ledger WHERE tenant_id = $1 AND customer_id = $2 FOR UPDATE`,
		tenantID, customerID,
	); err != nil {
		return domain.CreditLedgerEntry{}, fmt.Errorf("lock credit ledger: %w", err)
	}

	var currentBalance int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(amount_cents), 0) FROM customer_credit_ledger WHERE tenant_id = $1 AND customer_id = $2`,
		tenantID, customerID,
	).Scan(&currentBalance); err != nil {
		return domain.CreditLedgerEntry{}, fmt.Errorf("read credit balance: %w", err)
	}

	if amountCents < 0 && currentBalance+amountCents < 0 {
		// errs.Invalid so the API surfaces a 400 — this is operator input
		// exceeding the available balance, not a server fault (was a plain
		// fmt.Errorf that respond.FromError mapped to 500 internal_error).
		return domain.CreditLedgerEntry{}, errs.Invalid("amount_cents",
			fmt.Sprintf("insufficient balance: available %.2f, deduction %.2f",
				float64(currentBalance)/100, float64(-amountCents)/100))
	}

	now := clock.Now(ctx)

	// Negative adjustment: FIFO-drain positive blocks so each block's
	// consumed_cents stays consistent with the ledger sum. The raw-SUM
	// balance check above is NOT sufficient on its own: it counts
	// expired-but-unswept grant headroom that drainPositiveBlocks
	// rightly refuses to drain (expires_at <= now). Booking the full
	// deduction against blocks that only partially absorbed it would
	// drive SUM(amount_cents) below the blocks' remaining capacity —
	// the exact negative-ledger drift the attribution model exists to
	// prevent. Fail loud on the shortfall instead (P14, plan §4.4).
	var drainedFrom []drainedBlock
	if amountCents < 0 {
		drained, _, blocks, err := s.drainPositiveBlocks(ctx, tx, tenantID, customerID, -amountCents, now)
		if err != nil {
			return domain.CreditLedgerEntry{}, fmt.Errorf("attribute clawback: %w", err)
		}
		if drained != -amountCents {
			// errs.Invalid → 400 for the same reason as the balance check
			// above: operator asked for more than the drainable balance.
			return domain.CreditLedgerEntry{}, errs.Invalid("amount_cents", fmt.Sprintf(
				"insufficient drainable balance: active credit blocks cover %.2f of the %.2f deduction — the rest of the balance is expired credit pending the expiry sweep",
				float64(drained)/100, float64(-amountCents)/100))
		}
		drainedFrom = blocks
	}

	entry := domain.CreditLedgerEntry{
		ID:           postgres.NewID("vlx_ccl"),
		TenantID:     tenantID,
		CustomerID:   customerID,
		EntryType:    domain.CreditAdjustment,
		AmountCents:  amountCents,
		BalanceAfter: currentBalance + amountCents,
		Description:  description,
		CreatedAt:    now,
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO customer_credit_ledger (id, tenant_id, customer_id, entry_type,
			amount_cents, balance_after, description, metadata, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
	`, entry.ID, entry.TenantID, entry.CustomerID, entry.EntryType,
		entry.AmountCents, entry.BalanceAfter, entry.Description, drainMetadataJSON(drainedFrom), entry.CreatedAt,
	); err != nil {
		if postgres.IsForeignKeyViolation(err) {
			return domain.CreditLedgerEntry{}, fmt.Errorf("customer %q not found", customerID)
		}
		return domain.CreditLedgerEntry{}, fmt.Errorf("insert adjustment: %w", err)
	}

	// ADR-078 balance-crossing events (advisory lock held above).
	if err := s.emitBalanceCrossings(ctx, tx, tenantID, customerID, currentBalance, entry.BalanceAfter); err != nil {
		return domain.CreditLedgerEntry{}, err
	}

	if emit != nil {
		if err := emit(tx, entry); err != nil {
			return domain.CreditLedgerEntry{}, fmt.Errorf("audit emission: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return domain.CreditLedgerEntry{}, err
	}
	return entry, nil
}

// drainPositiveBlocks FIFO-drains up to `wantDrain` cents across active
// positive blocks (grants + positive adjustments + invoice reversals),
// bumping each block's consumed_cents. Returns (drained, available):
// drained = min(wantDrain, available); available = the total remaining
// capacity across the selected blocks (sum of amount_cents-consumed_cents).
//
// Order (ADR-078): promotional (zero-cost-basis) blocks first — free
// marketing credits burn before paid-class blocks (commits + legacy
// NULL-kind money-derived credits), the peer-verified revenue-preserving
// rule. WITHIN a cost-basis class: soonest-expiring first (NULL last) so
// usage minimizes wasted expiring credits, earliest-created as the
// tie-breaker. The promotional predicate MUST be NULL-safe (IS NOT
// DISTINCT FROM): a bare `grant_kind = 'promotional'` evaluates NULL for
// every legacy block, and `DESC` sorts NULL FIRST — draining money-derived
// legacy credits before free promo, the exact inversion this order exists
// to prevent (panel-verified against live Postgres). Applies to BOTH
// callers — drawdown (ApplyToInvoiceAtomic) AND clawback attribution
// (AdjustAtomic) — promotional-first is intended for both. Skips blocks
// past their expires_at — those will be retired by the expiry path;
// draining them here would mask the retirement.
//
// Caller MUST hold a FOR UPDATE lock on the customer's ledger rows.
// In a clean ledger `available` equals the SUM(amount_cents) balance
// (every negative ledger entry attributes via this path). ApplyToInvoiceAtomic
// compares the two to detect drift and caps its drain at the balance.
func (s *PostgresStore) drainPositiveBlocks(
	ctx context.Context, tx *sql.Tx, tenantID, customerID string, wantDrain int64, now time.Time,
) (int64, int64, []drainedBlock, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, amount_cents, consumed_cents
		FROM customer_credit_ledger
		WHERE tenant_id = $1
		  AND customer_id = $2
		  AND amount_cents > 0
		  AND consumed_cents < amount_cents
		  AND (expires_at IS NULL OR expires_at > $3)
		ORDER BY (grant_kind IS NOT DISTINCT FROM 'promotional') DESC,
		         expires_at NULLS LAST, created_at, id
	`, tenantID, customerID, now)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("scan positive blocks: %w", err)
	}
	type block struct {
		id        string
		remaining int64
	}
	var blocks []block
	var available int64
	for rows.Next() {
		var id string
		var amount, consumed int64
		if err := rows.Scan(&id, &amount, &consumed); err != nil {
			_ = rows.Close()
			return 0, 0, nil, fmt.Errorf("scan block: %w", err)
		}
		rem := amount - consumed
		blocks = append(blocks, block{id: id, remaining: rem})
		available += rem
	}
	if err := rows.Close(); err != nil {
		return 0, 0, nil, fmt.Errorf("close blocks cursor: %w", err)
	}
	if err := rows.Err(); err != nil {
		return 0, 0, nil, fmt.Errorf("iterate blocks: %w", err)
	}

	remaining := wantDrain
	var drainedFrom []drainedBlock
	for _, b := range blocks {
		if remaining <= 0 {
			break
		}
		take := min(b.remaining, remaining)
		if take <= 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE customer_credit_ledger
			SET consumed_cents = consumed_cents + $1
			WHERE id = $2 AND tenant_id = $3
		`, take, b.id, tenantID); err != nil {
			return 0, 0, nil, fmt.Errorf("update block %s consumed_cents: %w", b.id, err)
		}
		drainedFrom = append(drainedFrom, drainedBlock{BlockID: b.id, TakeCents: take})
		remaining -= take
	}
	return wantDrain - remaining, available, drainedFrom, nil
}

// drainedBlock is one (block, amount) leg of a drain — the attribution
// record stamped into the consuming ledger entry's metadata as
// {"drained_blocks": [...]}. Persisting it is deliberate (2026-07-05
// reassessment): drainPositiveBlocks always computed this list and threw
// it away while the entry's metadata was written as '{}', so every drain
// permanently destroyed the block-level attribution any future
// per-block reversal or commit-burndown reporting needs. The eventual
// reversal-semantics redesign stays deferred; the DATA is not deferrable
// — a drain that happened unrecorded is unrecoverable.
type drainedBlock struct {
	BlockID   string `json:"block_id"`
	TakeCents int64  `json:"take_cents"`
}

// drainMetadataJSON renders the drained-block attribution as the ledger
// entry's metadata document. Empty drain → '{}' (metadata column is NOT NULL).
func drainMetadataJSON(drainedFrom []drainedBlock) []byte {
	if len(drainedFrom) == 0 {
		return []byte("{}")
	}
	doc, err := json.Marshal(map[string]any{"drained_blocks": drainedFrom})
	if err != nil {
		// A map of two scalar-field structs cannot fail to marshal; keep
		// the entry writable regardless — the drain itself must not abort
		// over its audit stamp.
		return []byte("{}")
	}
	return doc
}

// ApplyToInvoiceAtomic debits the customer's credit balance and reduces the
// invoice's amount_due_cents in a SINGLE transaction. Either both writes
// succeed or both are rolled back. This closes the dual-write hole where a
// credit ledger entry could be created but the invoice's denormalized
// amount_due fail to update, leaving the customer double-billed.
//
// Returns the amount deducted (0 if balance is zero or invoice is free).
// Writes across two tables (customer_credit_ledger + invoices) inside one
// tenant-scoped tx — intentional: the invoice's credit fields are a cache of
// the ledger's source of truth, and they must stay in lockstep.
func (s *PostgresStore) ApplyToInvoiceAtomic(ctx context.Context, tenantID, customerID, invoiceID, invoiceDesc string, invoiceAmountCents int64, at time.Time) (int64, error) {
	if invoiceAmountCents <= 0 {
		return 0, nil
	}

	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return 0, err
	}
	defer postgres.Rollback(tx)

	// ADR-078 global lock order: invoice-row → customer advisory → ledger
	// rows. The invoice row is locked FIRST and its state re-read IN-TX —
	// the caller's invoiceAmountCents is a stale pre-read (dunning retry /
	// auto-charge sweep read it on an earlier connection), and before
	// ADR-078 a settle racing this apply silently burned credits into an
	// already-paid invoice (usage entry + credits_applied bump committed
	// while GREATEST capped the due at 0). The re-read also enforces that
	// commit funding invoices are CASH instruments: a customer's balance —
	// including the commit's own just-granted credits — must never pay the
	// invoice that funds a grant ("credits buy credits": revenue booked on
	// zero cash).
	var (
		invStatus       string
		invAmountDue    int64
		isCommitFunding bool
	)
	err = tx.QueryRowContext(ctx, `
		SELECT i.status, i.amount_due_cents,
			EXISTS (
				SELECT 1 FROM invoice_line_items li
				WHERE li.invoice_id = i.id AND li.tenant_id = i.tenant_id
				  AND li.commit_granted_cents IS NOT NULL
			)
		FROM invoices i
		WHERE i.id = $1 AND i.tenant_id = $2
		FOR UPDATE OF i
	`, invoiceID, tenantID).Scan(&invStatus, &invAmountDue, &isCommitFunding)
	if err == sql.ErrNoRows {
		return 0, fmt.Errorf("apply credits: invoice %s: %w", invoiceID, errs.ErrNotFound)
	}
	if err != nil {
		return 0, fmt.Errorf("apply credits: lock invoice %s: %w", invoiceID, err)
	}
	if isCommitFunding {
		// Designed no-op, not a failure (ADR-078 D4). Stored-PM auto-charge
		// still collects commit invoices with real cash.
		slog.DebugContext(ctx, "credit apply skipped: commit funding invoices are cash instruments",
			"invoice_id", invoiceID, "customer_id", customerID)
		return 0, nil
	}
	switch domain.InvoiceStatus(invStatus) {
	case domain.InvoiceDraft, domain.InvoiceFinalized:
		// Payable — draft stays eligible: billOnePeriod applies credits to
		// tax-pending drafts at build time.
	default:
		// Paid / voided: nothing to cover. The caller's
		// pre-read went stale (e.g. the customer settled via checkout while
		// a dunning tick was in flight).
		slog.DebugContext(ctx, "credit apply skipped: invoice no longer payable",
			"invoice_id", invoiceID, "status", invStatus)
		return 0, nil
	}
	if invAmountDue <= 0 {
		return 0, nil
	}

	// Per-customer advisory lock (ADR-078): serializes this drain against
	// grant-path writers (which take the advisory lock but no row locks),
	// making balance_after snapshots and alert crossings well-ordered.
	if _, err := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		tenantID+":"+customerID,
	); err != nil {
		return 0, fmt.Errorf("acquire credit ledger lock: %w", err)
	}

	// Lock the customer's ledger rows to serialize concurrent applications —
	// without this, two simultaneous billing runs on the same customer could
	// each see the full balance and over-deduct.
	//
	// tenant_id included in predicate as defense-in-depth (see AppendEntry).
	if _, err := tx.ExecContext(ctx,
		`SELECT 1 FROM customer_credit_ledger WHERE tenant_id = $1 AND customer_id = $2 FOR UPDATE`,
		tenantID, customerID,
	); err != nil {
		return 0, fmt.Errorf("lock credit ledger: %w", err)
	}

	var currentBalance int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(amount_cents), 0) FROM customer_credit_ledger WHERE tenant_id = $1 AND customer_id = $2`,
		tenantID, customerID,
	).Scan(&currentBalance); err != nil {
		return 0, fmt.Errorf("read credit balance: %w", err)
	}

	if currentBalance <= 0 {
		return 0, nil // No credits to apply — no need to write anything.
	}

	now := at
	if now.IsZero() {
		now = clock.Now(ctx)
	}

	// FIFO-drain active positive blocks (Orb's credit-block model).
	// drainPositiveBlocks returns the actual drained amount (≤
	// requested) — partial-application (invoice > balance) naturally
	// drains only what's available; the leftover stays on the
	// invoice for the PaymentIntent to cover. Negative balance was
	// short-circuited above.
	//
	// Cap the drain at the authoritative balance, NOT the sum of blocks'
	// remaining-to-drain. In a clean ledger these are equal: every negative
	// entry (clawback via AdjustAtomic, expiry) attributes to a block by
	// bumping consumed_cents, so block-remaining stays in lockstep with the
	// SUM(amount_cents) balance, and this cap is a no-op. The cap is a loud
	// defensive guard against a *future* path that adds a negative entry
	// WITHOUT attributing it: that drift would leave blocks with more
	// remaining than the balance, and an uncapped drain would write a negative
	// balance_after — silent money corruption. We cap (balance can't go
	// negative) AND warn (the drift is surfaced, never absorbed).
	// Drain against the IN-TX re-read amount_due, never the caller's stale
	// invoiceAmountCents (ADR-078: the pre-read may predate a concurrent
	// settle or credit note).
	drainTarget := min(invAmountDue, currentBalance)
	deduct, drainable, drainedFrom, err := s.drainPositiveBlocks(ctx, tx, tenantID, customerID, drainTarget, now)
	if err != nil {
		return 0, fmt.Errorf("drain credits for invoice: %w", err)
	}
	if drainable != currentBalance {
		// Drift: the positive blocks' total remaining capacity disagrees with
		// the authoritative SUM(amount_cents) balance — a negative entry was
		// not attributed to a block, or a block's expiry is out of sync. The
		// drain above stayed capped at the balance so the ledger can't go
		// negative; surface the drift so it's reconciled, never absorbed.
		slog.WarnContext(ctx, "credit ledger drift: drainable block capacity != balance",
			"customer_id", customerID,
			"balance_cents", currentBalance,
			"drainable_cents", drainable,
		)
	}
	if deduct <= 0 {
		// All drainable "balance" was in expired/exhausted grants.
		return 0, nil
	}

	entryID := postgres.NewID("vlx_ccl")
	balanceAfter := currentBalance - deduct

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO customer_credit_ledger (id, tenant_id, customer_id, entry_type,
			amount_cents, balance_after, description, invoice_id, metadata, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
	`, entryID, tenantID, customerID, domain.CreditUsage,
		-deduct, balanceAfter, invoiceDesc, invoiceID, drainMetadataJSON(drainedFrom), now,
	); err != nil {
		if postgres.IsForeignKeyViolation(err) {
			return 0, fmt.Errorf("customer %q not found", customerID)
		}
		return 0, fmt.Errorf("insert credit usage: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE invoices
		SET amount_due_cents = GREATEST(amount_due_cents - $1, 0),
		    credits_applied_cents = credits_applied_cents + $1,
		    updated_at = $2
		WHERE id = $3 AND tenant_id = $4
	`, deduct, now, invoiceID, tenantID); err != nil {
		return 0, fmt.Errorf("update invoice amount_due: %w", err)
	}

	// Any amount_due change invalidates an open checkout claim — its Stripe
	// session was minted for the OLD amount, and a customer paying it after
	// this credit overpays (ADR-068). The CN-issue writer
	// (invoice.ApplyCreditNoteTx) carries this same supersede; THIS writer
	// lost it when the live credit path moved off the since-deleted
	// invoice.ApplyCredits (found by the 2026-07-21 derived-data audit —
	// the supersede + its test were stranded on the dead method). The
	// settle-time sweep expires any superseded row whose Stripe session is
	// still live.
	if _, err := tx.ExecContext(ctx, `
		UPDATE checkout_sessions SET status = 'superseded', updated_at = now()
		WHERE invoice_id = $1 AND tenant_id = $2 AND status = 'open'
	`, invoiceID, tenantID); err != nil {
		return 0, fmt.Errorf("supersede checkout claims on credit apply: %w", err)
	}

	// ADR-078 balance-crossing events (advisory lock held above).
	if err := s.emitBalanceCrossings(ctx, tx, tenantID, customerID, currentBalance, balanceAfter); err != nil {
		return 0, err
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit credit application: %w", err)
	}
	return deduct, nil
}

// commitGrantState is the funded grant's live drawdown state, read under
// the customer advisory lock + grant row FOR UPDATE — one lock freezes
// every input of the relief cap formula (ADR-080).
type commitGrantState struct {
	GrantID        string
	CustomerID     string
	AmountCents    int64 // G
	ConsumedCents  int64 // C
	CNRetiredCents int64 // K — cumulative credits retired by relief CNs
}

func (g commitGrantState) Remaining() int64 { return g.AmountCents - g.ConsumedCents }

// lockCommitGrantTx locates the commit grant funded by invoiceID and
// returns its state under the ADR-078 lock discipline (customer advisory
// lock, then grant row FOR UPDATE re-read). found=false when the invoice
// funded no grant — callers decide whether that is a clean no-op (void
// leg) or a loud corruption error (relief leg: cash is about to move).
func (s *PostgresStore) LockCommitGrantTx(ctx context.Context, tx *sql.Tx, tenantID, invoiceID string) (commitGrantState, bool, error) {
	var g commitGrantState
	// Find the funded grant. No FOR UPDATE yet — the advisory lock comes
	// first (grant-path writers serialize on it, not on row locks).
	err := tx.QueryRowContext(ctx, `
		SELECT id, customer_id, amount_cents, consumed_cents, cn_retired_cents
		FROM customer_credit_ledger
		WHERE tenant_id = $1 AND source_invoice_id = $2 AND grant_kind = 'commit'
	`, tenantID, invoiceID).Scan(&g.GrantID, &g.CustomerID, &g.AmountCents, &g.ConsumedCents, &g.CNRetiredCents)
	if err == sql.ErrNoRows {
		return commitGrantState{}, false, nil
	}
	if err != nil {
		return commitGrantState{}, false, fmt.Errorf("find commit grant for invoice %s: %w", invoiceID, err)
	}

	if _, err := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		tenantID+":"+g.CustomerID,
	); err != nil {
		return commitGrantState{}, false, fmt.Errorf("acquire credit ledger lock: %w", err)
	}

	// Re-read under the row lock — the pre-lock snapshot is stale by
	// construction (a concurrent drain can bump consumed_cents between
	// the lookup and the lock).
	err = tx.QueryRowContext(ctx, `
		SELECT amount_cents, consumed_cents, cn_retired_cents
		FROM customer_credit_ledger
		WHERE tenant_id = $1 AND id = $2
		FOR UPDATE
	`, tenantID, g.GrantID).Scan(&g.AmountCents, &g.ConsumedCents, &g.CNRetiredCents)
	if err != nil {
		return commitGrantState{}, false, fmt.Errorf("re-read commit grant %s under lock: %w", g.GrantID, err)
	}
	return g, true, nil
}

// CommitGrantExposure reports the prepaid-commit grant funded by invoiceID:
// how much of it the customer can still spend, and how much they already have.
// Both zero when the invoice funded no commit. Satisfies
// invoice.CommitGrantReader — the attention banner for credit that went out
// against cash that never arrived (ADR-078 D3's named fast-follow).
//
// Deliberately takes NO LOCK — neither the customer advisory lock that
// LockCommitGrantTx takes nor a FOR UPDATE. That is a real divergence from its
// neighbour and the next reader will be tempted to copy the locked shape, so:
// this runs once per unpaid manual invoice on every dashboard list render, and
// taking the advisory lock would serialize those renders behind every
// concurrent drawdown on the same customer. What it returns is a sentence in a
// banner, not a figure money is about to move against; a drain racing this read
// makes it a moment stale, which is the right trade for a display. Every path
// that actually MOVES commit credit still goes through the locked reader.
//
// Drawability mirrors drainPositiveBlocks' liveness predicate exactly —
// positive, not exhausted, not past expires_at at `now` — because the one thing
// this must never do is promise the operator spendable credit that the drawdown
// would refuse to spend. `now` is the caller's resolver-bound instant for the
// same reason the drain takes one: on a clock-pinned invoice, wall-clock is the
// wrong axis to judge expiry on.
//
// Consumed is reported even when the grant is exhausted or expired, because it
// is the half that no recovery can claw back and the half that decides how
// urgent the banner is.
func (s *PostgresStore) CommitGrantExposure(
	ctx context.Context, tenantID, invoiceID string, now time.Time,
) (int64, int64, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return 0, 0, err
	}
	defer postgres.Rollback(tx)

	var drawable, consumed int64
	// At most one row: idx_credit_ledger_commit_fund_dedup is UNIQUE on
	// (tenant_id, source_invoice_id) WHERE grant_kind='commit' (migration 0136),
	// which is also what makes this an index probe rather than a scan.
	err = tx.QueryRowContext(ctx, `
		SELECT
			CASE
				WHEN amount_cents > 0
				 AND consumed_cents < amount_cents
				 AND (expires_at IS NULL OR expires_at > $3)
				THEN amount_cents - consumed_cents
				ELSE 0
			END,
			GREATEST(consumed_cents, 0)
		FROM customer_credit_ledger
		WHERE tenant_id = $1 AND source_invoice_id = $2 AND grant_kind = 'commit'
	`, tenantID, invoiceID, now).Scan(&drawable, &consumed)
	if err == sql.ErrNoRows {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("read commit grant exposure for invoice %s: %w", invoiceID, err)
	}
	return drawable, consumed, nil
}

// retireCommitSliceTx is the single retirement core both commit-unwind
// writers share (invoice void + CN relief — the complete writer set).
// Retires exactly `slice` credits from the locked grant: CAS-guarded
// consumed bump (+ cn_retired bump for relief slices) and ONE negative
// adjustment ledger entry. Caller holds the locks via lockCommitGrantTx.
func (s *PostgresStore) retireCommitSliceTx(ctx context.Context, tx *sql.Tx, tenantID string, g commitGrantState, slice int64, bumpCNRetired bool, description string, meta map[string]any) error {
	if slice <= 0 || slice > g.Remaining() {
		return fmt.Errorf("retire slice %d out of range (remaining %d) on grant %s", slice, g.Remaining(), g.GrantID)
	}
	cnBump := int64(0)
	if bumpCNRetired {
		cnBump = slice
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE customer_credit_ledger
		SET consumed_cents = consumed_cents + $1,
		    cn_retired_cents = cn_retired_cents + $2
		WHERE tenant_id = $3 AND id = $4 AND consumed_cents + $1 <= amount_cents
	`, slice, cnBump, tenantID, g.GrantID)
	if err != nil {
		return fmt.Errorf("retire commit grant %s: %w", g.GrantID, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("retire commit grant %s: rows affected: %w", g.GrantID, err)
	} else if n == 0 {
		// Unreachable after the locked re-read; kept as the structural
		// exactly-once gate so the entry below can never double-append.
		return fmt.Errorf("retire commit grant %s: concurrent drain won the CAS", g.GrantID)
	}

	if _, err := s.appendEntryInTx(ctx, tx, tenantID, domain.CreditLedgerEntry{
		CustomerID:  g.CustomerID,
		EntryType:   domain.CreditAdjustment,
		AmountCents: -slice,
		Description: description,
		Metadata:    meta,
		CreatedAt:   clock.Now(ctx),
	}); err != nil {
		return fmt.Errorf("append commit retirement entry for grant %s: %w", g.GrantID, err)
	}
	return nil
}

// RetireCommitGrantForInvoiceTx retires the REMAINING balance of the commit
// grant funded by `invoiceID`, on the caller's tx — the VOID leg of ADR-078
// (invoice.Service.Void runs it inside the UpdateStatusWithReversal
// coordinator tx, after the status flip). Clean no-op (0, nil) when the
// invoice funded no grant (gate never granted / not a commit invoice) or the
// grant is already fully consumed or retired. Void-retire never bumps
// cn_retired_cents — no cash left, so the relief anchor K is untouched.
//
// The `consumed_cents < amount_cents` CAS flip is the structural exactly-once
// gate: a second retire (e.g. the legal uncollectible→void sequence, or any
// retry) re-reads remaining == 0 and no-ops. Consumed stays consumed, always.
//
// Lock order (ADR-078 global): the caller already holds the invoice row lock;
// this takes the customer advisory lock, then the grant row FOR UPDATE.
func (s *PostgresStore) RetireCommitGrantForInvoiceTx(ctx context.Context, tx *sql.Tx, tenantID, invoiceID string) (int64, error) {
	g, found, err := s.LockCommitGrantTx(ctx, tx, tenantID, invoiceID)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, nil
	}
	remaining := g.Remaining()
	if remaining <= 0 {
		return 0, nil // fully drawn or already retired — nothing to claw back
	}
	if err := s.retireCommitSliceTx(ctx, tx, tenantID, g, remaining, false,
		fmt.Sprintf("Commit retired — funding invoice voided (grant %s)", g.GrantID),
		map[string]any{"reason": "commit_void_retire", "grant_id": g.GrantID, "funding_invoice_id": invoiceID},
	); err != nil {
		return 0, err
	}
	return remaining, nil
}

// RetireCommitSliceForReliefTx retires exactly `slice` credits from the
// commit grant funded by invoiceID — the RELIEF leg (ADR-080). Unlike the
// void wrapper: a missing grant is a LOUD error (cash is about to move
// against a grant that must exist — finalize funds it in the same tx, so
// absence is corruption), and cn_retired_cents IS bumped (the telescoping
// anchor K advances with every relief). Returns the post-retire state so
// the caller can surface remaining/K without a re-read.
func (s *PostgresStore) RetireCommitSliceForReliefTx(ctx context.Context, tx *sql.Tx, tenantID, invoiceID, creditNoteID string, slice, refundedGrossCents, grossPaidCents int64) (commitGrantState, error) {
	g, found, err := s.LockCommitGrantTx(ctx, tx, tenantID, invoiceID)
	if err != nil {
		return commitGrantState{}, err
	}
	if !found {
		return commitGrantState{}, fmt.Errorf("commit relief: invoice %s has a commit line but no funded grant — finalize funds the grant in the same tx, so this is corrupted state; refusing to move cash", invoiceID)
	}
	if slice > g.Remaining() {
		return commitGrantState{}, fmt.Errorf("%w: %d requested, %d remaining", ErrCommitInsufficientRemaining, slice, g.Remaining())
	}
	if err := s.retireCommitSliceTx(ctx, tx, tenantID, g, slice, true,
		fmt.Sprintf("Commit relief — %d credits retired for credit note (grant %s)", slice, g.GrantID),
		map[string]any{
			"reason":               "commit_refund_retire",
			"grant_id":             g.GrantID,
			"funding_invoice_id":   invoiceID,
			"credit_note_id":       creditNoteID,
			"refunded_gross_cents": refundedGrossCents,
			"gross_paid_cents":     grossPaidCents,
			"granted_cents":        g.AmountCents,
		},
	); err != nil {
		return commitGrantState{}, err
	}
	g.ConsumedCents += slice
	g.CNRetiredCents += slice

	// credit.commit_retired rides the SAME tx (ADR-040 outbox) — the
	// entitlement consumer that tracks commit burndown must see every
	// relief exactly-once with the state change.
	if s.outbox != nil {
		if _, err := s.outbox.Enqueue(ctx, tx, tenantID, domain.EventCreditCommitRetired, map[string]any{
			"customer_id":           g.CustomerID,
			"grant_id":              g.GrantID,
			"funding_invoice_id":    invoiceID,
			"credit_note_id":        creditNoteID,
			"retired_cents":         slice,
			"refunded_gross_cents":  refundedGrossCents,
			"remaining_after_cents": g.Remaining(),
		}); err != nil {
			return commitGrantState{}, fmt.Errorf("enqueue credit.commit_retired: %w", err)
		}
	}
	return g, nil
}

// ErrCommitInsufficientRemaining is returned when a relief requests more
// credits than the grant has remaining (a drawdown may have raced the
// operator's read). The caller surfaces the LIVE remaining so the operator
// re-decides — never a silent clamp of an approved money document.
var ErrCommitInsufficientRemaining = errors.New("commit grant has insufficient remaining credits")

// GetByProrationSource returns the credit ledger entry previously written
// for a specific (subscription, item, change_type, change_at) event, if any.
// Callers use this after AppendEntry returns ErrAlreadyExists to complete an
// idempotent retry — the proration dedup partial index ensures uniqueness.
// change_type disambiguates plan-vs-qty-vs-remove events on the same item at
// the same wall-clock instant; the item id keeps cross-item changes distinct.
func (s *PostgresStore) GetByProrationSource(ctx context.Context, tenantID, subscriptionID, subscriptionItemID string, changeType domain.ItemChangeType, changeAt time.Time) (domain.CreditLedgerEntry, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.CreditLedgerEntry{}, err
	}
	defer postgres.Rollback(tx)

	var e domain.CreditLedgerEntry
	var metaJSON []byte
	err = tx.QueryRowContext(ctx, `
		SELECT id, tenant_id, customer_id, entry_type, amount_cents, balance_after,
			description, COALESCE(invoice_id,''), expires_at, metadata, created_at,
			COALESCE(source_subscription_id,''), COALESCE(source_subscription_item_id,''),
			source_plan_changed_at, COALESCE(source_change_type,'')
		FROM customer_credit_ledger
		WHERE tenant_id = $1 AND source_subscription_id = $2 AND source_subscription_item_id = $3 AND source_change_type = $4 AND source_plan_changed_at = $5
	`, tenantID, subscriptionID, subscriptionItemID, string(changeType), changeAt,
	).Scan(&e.ID, &e.TenantID, &e.CustomerID, &e.EntryType,
		&e.AmountCents, &e.BalanceAfter, &e.Description, &e.InvoiceID,
		&e.ExpiresAt, &metaJSON, &e.CreatedAt,
		&e.SourceSubscriptionID, &e.SourceSubscriptionItemID, &e.SourcePlanChangedAt,
		(*string)(&e.SourceChangeType))
	if err == sql.ErrNoRows {
		return domain.CreditLedgerEntry{}, errs.ErrNotFound
	}
	if err != nil {
		return domain.CreditLedgerEntry{}, err
	}
	_ = json.Unmarshal(metaJSON, &e.Metadata)
	return e, nil
}

// GetByCreditNoteSource fetches the grant row tied to a specific
// credit-note Issue(). Used by credit.Service.GrantForCreditNote to
// recover from ErrAlreadyExists on retry — the partial unique index
// idx_credit_ledger_credit_note_dedup (migration 0093) enforces one
// grant per (tenant, CN), so a retry that hits this lookup confirms
// the grant already exists and returns it for the caller to use.
func (s *PostgresStore) GetByCreditNoteSource(ctx context.Context, tenantID, creditNoteID string) (domain.CreditLedgerEntry, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.CreditLedgerEntry{}, err
	}
	defer postgres.Rollback(tx)

	var e domain.CreditLedgerEntry
	var metaJSON []byte
	err = tx.QueryRowContext(ctx, `
		SELECT id, tenant_id, customer_id, entry_type, amount_cents, balance_after,
			description, COALESCE(invoice_id,''), expires_at, metadata, created_at,
			COALESCE(source_subscription_id,''), COALESCE(source_subscription_item_id,''),
			source_plan_changed_at, COALESCE(source_change_type,''),
			COALESCE(source_credit_note_id,'')
		FROM customer_credit_ledger
		WHERE tenant_id = $1 AND source_credit_note_id = $2
	`, tenantID, creditNoteID,
	).Scan(&e.ID, &e.TenantID, &e.CustomerID, &e.EntryType,
		&e.AmountCents, &e.BalanceAfter, &e.Description, &e.InvoiceID,
		&e.ExpiresAt, &metaJSON, &e.CreatedAt,
		&e.SourceSubscriptionID, &e.SourceSubscriptionItemID, &e.SourcePlanChangedAt,
		(*string)(&e.SourceChangeType),
		&e.SourceCreditNoteID)
	if err == sql.ErrNoRows {
		return domain.CreditLedgerEntry{}, errs.ErrNotFound
	}
	if err != nil {
		return domain.CreditLedgerEntry{}, err
	}
	_ = json.Unmarshal(metaJSON, &e.Metadata)
	return e, nil
}

func (s *PostgresStore) GetBalance(ctx context.Context, tenantID, customerID string) (domain.CreditBalance, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.CreditBalance{}, err
	}
	defer postgres.Rollback(tx)

	var b domain.CreditBalance
	b.CustomerID = customerID

	err = tx.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(amount_cents), 0),
			COALESCE(SUM(CASE WHEN amount_cents > 0 THEN amount_cents ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN entry_type = 'usage' THEN ABS(amount_cents) ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN entry_type = 'expiry' THEN ABS(amount_cents) ELSE 0 END), 0)
		FROM customer_credit_ledger WHERE tenant_id = $1 AND customer_id = $2
	`, tenantID, customerID).Scan(&b.BalanceCents, &b.TotalGranted, &b.TotalUsed, &b.TotalExpired)

	return b, err
}

func (s *PostgresStore) ListBalances(ctx context.Context, tenantID string) ([]domain.CreditBalance, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	rows, err := tx.QueryContext(ctx, `
		SELECT
			customer_id,
			COALESCE(SUM(amount_cents), 0),
			COALESCE(SUM(CASE WHEN amount_cents > 0 THEN amount_cents ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN entry_type = 'usage' THEN ABS(amount_cents) ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN entry_type = 'expiry' THEN ABS(amount_cents) ELSE 0 END), 0)
		FROM customer_credit_ledger
		WHERE tenant_id = $1
		GROUP BY customer_id
		ORDER BY SUM(amount_cents) DESC
	`, tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var balances []domain.CreditBalance
	for rows.Next() {
		var b domain.CreditBalance
		if err := rows.Scan(&b.CustomerID, &b.BalanceCents, &b.TotalGranted, &b.TotalUsed, &b.TotalExpired); err != nil {
			return nil, err
		}
		balances = append(balances, b)
	}
	return balances, rows.Err()
}

// ListExpiredGrants — CRON path. ADR-029 Phase 4: clock-pinned
// customers' grants are excluded; the catchup orchestrator drives
// per-clock expiry via ListExpiredGrantsForClock against the clock's
// frozen_time. Without this filter the wall-clock cron would expire
// a clock-pinned customer's grants based on wall-clock time, not the
// simulated time the customer's other consequences run on.
func (s *PostgresStore) ListExpiredGrants(ctx context.Context) ([]domain.CreditLedgerEntry, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	// This is candidate DISCOVERY only — ExpireGrantAtomic re-reads and
	// gates under the row lock, so a stale row here is harmless (it
	// no-ops). `consumed_cents < amount_cents` is the single exclusion:
	// retirement flips consumed_cents to amount_cents, which both
	// removes the grant from this list AND from drainPositiveBlocks
	// (migration 0127 retro-retired grants expired before the flip
	// existed). The pre-0127 description-LIKE NOT EXISTS dedup is gone —
	// it was operator-visible display copy doubling as an idempotency
	// key, and it was check-then-act across transactions anyway.
	rows, err := tx.QueryContext(ctx, `
		SELECT g.id, g.tenant_id, g.customer_id, g.amount_cents, g.consumed_cents, g.expires_at
		FROM customer_credit_ledger g
		WHERE g.entry_type = 'grant'
		  AND g.expires_at IS NOT NULL
		  AND g.expires_at < NOW()
		  AND g.livemode = $1
		  AND g.consumed_cents < g.amount_cents
		  AND NOT EXISTS (
		    SELECT 1 FROM customers c
		    WHERE c.id = g.customer_id
		      AND c.test_clock_id IS NOT NULL
		  )
	`, postgres.Livemode(ctx))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var entries []domain.CreditLedgerEntry
	for rows.Next() {
		var e domain.CreditLedgerEntry
		if err := rows.Scan(&e.ID, &e.TenantID, &e.CustomerID, &e.AmountCents, &e.ConsumedCents, &e.ExpiresAt); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// ListExpiredGrantsForClock is the catchup-path counterpart. ADR-029
// Phase 4 — returns grants belonging to customers pinned to the given
// clock whose expires_at is past the clock's frozen_time (passed
// explicitly so the comparison happens in simulated time, not wall-
// clock now).
func (s *PostgresStore) ListExpiredGrantsForClock(ctx context.Context, tenantID, clockID string, frozenTime time.Time) ([]domain.CreditLedgerEntry, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	// Candidate discovery only (see ListExpiredGrants rationale):
	// ExpireGrantAtomic re-reads under the row lock; `consumed_cents <
	// amount_cents` is the single retirement exclusion.
	rows, err := tx.QueryContext(ctx, `
		SELECT g.id, g.tenant_id, g.customer_id, g.amount_cents, g.consumed_cents, g.expires_at
		FROM customer_credit_ledger g
		JOIN customers c ON c.id = g.customer_id
		WHERE g.entry_type = 'grant'
		  AND g.expires_at IS NOT NULL
		  AND g.expires_at < $3
		  AND g.tenant_id = $1
		  AND c.test_clock_id = $2
		  AND g.consumed_cents < g.amount_cents
	`, tenantID, clockID, frozenTime)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var entries []domain.CreditLedgerEntry
	for rows.Next() {
		var e domain.CreditLedgerEntry
		if err := rows.Scan(&e.ID, &e.TenantID, &e.CustomerID, &e.AmountCents, &e.ConsumedCents, &e.ExpiresAt); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

func (s *PostgresStore) ListEntries(ctx context.Context, filter ListFilter) ([]domain.CreditLedgerEntry, error) {
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

	// Chronological running balance via SUM(amount_cents) OVER window.
	// The stored balance_after column reflects the SUM at INSERTION
	// time — which diverges from "balance as of this row's date" when
	// rows are inserted out of chronological order (catchup-stamped
	// expiry rows land with a past created_at AFTER newer rows
	// already exist). Without this override, the UI's Balance column
	// reads as gibberish for any customer who's had a catchup-driven
	// expiry processed. Industry parity (Stripe / Lago / Orb): credit
	// timelines show the chronological running balance, not insertion-
	// order snapshots.
	//
	// Window is computed over the FULL customer ledger (no entry_type
	// or invoice_id filter inside the window) so the running balance
	// stays correct even when the operator filters the view to
	// "usage only" or one invoice — they still see "balance after
	// this row's chronological moment" rather than "balance after
	// this row's row-order moment."
	//
	// CTE pattern: compute running balance over all rows, THEN apply
	// filter + sort + pagination on the outer query.
	query := `WITH ledger AS (
		SELECT id, tenant_id, customer_id, entry_type, amount_cents,
			SUM(amount_cents) OVER (
				PARTITION BY tenant_id, customer_id
				ORDER BY created_at, id
				ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW
			) AS running_balance,
			description, invoice_id, expires_at, metadata, created_at,
			consumed_cents, grant_kind, source_invoice_id
		FROM customer_credit_ledger
		WHERE tenant_id = $1 AND customer_id = $2
	)
	SELECT id, tenant_id, customer_id, entry_type, amount_cents,
		running_balance AS balance_after,
		description, COALESCE(invoice_id,''), expires_at, metadata, created_at,
		consumed_cents, COALESCE(grant_kind,''), COALESCE(source_invoice_id,'')
	FROM ledger WHERE 1=1`
	args := []any{filter.TenantID, filter.CustomerID}
	idx := 3

	if filter.InvoiceID != "" {
		query += fmt.Sprintf(" AND invoice_id = $%d", idx)
		args = append(args, filter.InvoiceID)
		idx++
	}
	if filter.EntryType != "" {
		query += fmt.Sprintf(" AND entry_type = $%d", idx)
		args = append(args, filter.EntryType)
		idx++
	}

	query += fmt.Sprintf(" ORDER BY %s LIMIT $%d OFFSET $%d", creditEntryOrderBy(filter.Sort, filter.SortDir), idx, idx+1)
	args = append(args, limit, filter.Offset)

	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var entries []domain.CreditLedgerEntry
	for rows.Next() {
		var e domain.CreditLedgerEntry
		var metaJSON []byte
		if err := rows.Scan(&e.ID, &e.TenantID, &e.CustomerID, &e.EntryType,
			&e.AmountCents, &e.BalanceAfter, &e.Description, &e.InvoiceID,
			&e.ExpiresAt, &metaJSON, &e.CreatedAt,
			&e.ConsumedCents, (*string)(&e.GrantKind), &e.SourceInvoiceID); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(metaJSON, &e.Metadata)
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// CountEntries is ListEntries' total-row twin: same filter arms, no
// limit/offset/sort. Powers honest pagination + the complete-CSV export
// on the credits page (pre-fix the UI paginated, filtered, and exported
// a silently-capped 50-row slice as if it were the whole ledger).
func (s *PostgresStore) CountEntries(ctx context.Context, filter ListFilter) (int, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, filter.TenantID)
	if err != nil {
		return 0, err
	}
	defer postgres.Rollback(tx)

	query := `SELECT COUNT(*) FROM customer_credit_ledger WHERE tenant_id = $1 AND customer_id = $2`
	args := []any{filter.TenantID, filter.CustomerID}
	idx := 3
	if filter.InvoiceID != "" {
		query += fmt.Sprintf(" AND invoice_id = $%d", idx)
		args = append(args, filter.InvoiceID)
		idx++
	}
	if filter.EntryType != "" {
		query += fmt.Sprintf(" AND entry_type = $%d", idx)
		args = append(args, filter.EntryType)
	}
	var n int
	err = tx.QueryRowContext(ctx, query, args...).Scan(&n)
	return n, err
}

// creditEntryOrderBy validates sort + dir against a closed allow-list
// and adds a deterministic id tie-break matching the primary
// direction. See invoiceOrderBy for the design rationale.
func creditEntryOrderBy(sort, dir string) string {
	col := creditEntrySortColumn(sort)
	d := "DESC"
	if dir == "asc" {
		d = "ASC"
	}
	return col + " " + d + ", id " + d
}

func creditEntrySortColumn(key string) string {
	switch key {
	case "amount_cents", "amount":
		return "amount_cents"
	case "entry_type":
		return "entry_type"
	case "expires_at":
		return "expires_at"
	default:
		return "created_at"
	}
}

// GrantSummary is one positive credit block with its live drawdown state —
// the per-grant burndown row finance asks for ("how much of the $50k
// commit is drawn, when does it expire?"). remaining = amount - consumed;
// consumed is bumped by every drain (drawdown, clawback, expiry, commit
// retire), so this is the same arithmetic the drain order runs on.
type GrantSummary struct {
	ID              string     `json:"id"`
	EntryType       string     `json:"entry_type"`
	GrantKind       string     `json:"grant_kind,omitempty"`
	Description     string     `json:"description"`
	AmountCents     int64      `json:"amount_cents"`
	ConsumedCents   int64      `json:"consumed_cents"`
	RemainingCents  int64      `json:"remaining_cents"`
	ExpiresAt       *time.Time `json:"expires_at,omitempty"`
	SourceInvoiceID string     `json:"source_invoice_id,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
}

// ListGrantSummaries returns the customer's positive blocks (grants +
// positive adjustments + reversals) with per-block remaining, newest
// first. Exhausted blocks are included when includeExhausted (history
// view); live-only otherwise. Expired-but-unswept blocks report their
// remaining honestly — the expiry sweep will retire them.
func (s *PostgresStore) ListGrantSummaries(ctx context.Context, tenantID, customerID string, includeExhausted bool) ([]GrantSummary, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	exhausted := ""
	if !includeExhausted {
		exhausted = " AND consumed_cents < amount_cents"
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT id, entry_type, COALESCE(grant_kind,''), description,
			amount_cents, consumed_cents, expires_at, COALESCE(source_invoice_id,''), created_at
		FROM customer_credit_ledger
		WHERE customer_id = $1 AND amount_cents > 0`+exhausted+`
		ORDER BY created_at DESC, id DESC`, customerID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []GrantSummary
	for rows.Next() {
		var g GrantSummary
		if err := rows.Scan(&g.ID, &g.EntryType, &g.GrantKind, &g.Description,
			&g.AmountCents, &g.ConsumedCents, &g.ExpiresAt, &g.SourceInvoiceID, &g.CreatedAt); err != nil {
			return nil, err
		}
		g.RemainingCents = g.AmountCents - g.ConsumedCents
		out = append(out, g)
	}
	return out, rows.Err()
}
