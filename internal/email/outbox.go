package email

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// OutboxRow is a single queued outbound-email intent. Produced inside the
// business-op transaction (or standalone when no tx is available), drained
// by the dispatcher.
type OutboxRow struct {
	ID            string
	TenantID      string
	Livemode      bool
	EmailType     string
	Payload       map[string]any
	Status        string
	Attempts      int
	NextAttemptAt time.Time
	LastError     string
	CreatedAt     time.Time
	DispatchedAt  *time.Time
	// DeliveryState is the provider-confirmed outcome (ADR-098),
	// orthogonal to Status. Populated by the read paths that surface it
	// (ListByInvoice/ListByCustomer/GetByID); the dispatcher's claim
	// path leaves it zero — dispatch never reads it.
	DeliveryState string
	// SimEffectiveAt / TestClockID are the billing-axis anchor (ADR-104):
	// the simulated instant that CAUSED this send, stamped at enqueue from
	// the bound ctx. Nil for unbound enqueues (live-mode mail) — the two
	// calendars coincide. CreatedAt stays the real-world dispatch record.
	SimEffectiveAt *time.Time
	TestClockID    string
}

// outbox row statuses.
const (
	OutboxPending    = "pending"
	OutboxDispatched = "dispatched"
	OutboxFailed     = "failed"
)

// Delivery states (ADR-098) — what the provider learned about a send
// AFTER the SMTP handoff, reported via its webhooks. Orthogonal to the
// status lifecycle above ('dispatched' = relay accepted the handoff;
// 'delivered' = the RECIPIENT's mail server accepted the message — every
// major ESP models both, never one column). Severity-monotonic:
// unknown < delivered < bounced < complained; a webhook write only ever
// promotes, so at-least-once redelivery and Delivery-vs-Bounce
// reordering converge to the most-severe truth without a dedup table.
const (
	DeliveryUnknown    = "unknown"
	DeliveryDelivered  = "delivered"
	DeliveryBounced    = "bounced"
	DeliveryComplained = "complained"
)

// MaxOutboxAttempts is the DLQ threshold. After this many failed attempts a
// row becomes terminal ('failed') and is no longer retried automatically.
// Same shape as webhook_outbox — backoff ramp (see outboxBackoff) totals
// ~72h across 15 attempts, which covers most transient SMTP provider outages.
const MaxOutboxAttempts = 15

// Lease arithmetic (P5 panel — lease_arithmetic). The invariant chain the
// dispatcher must satisfy is
//
//	BatchSize × PerRowBudget ≤ BatchTimeout < ClaimLease(BatchSize)
//
// with marks running on a context DETACHED from the batch deadline.
// PerRowBudget is the worst-case cost of one row end-to-end: suppression
// check 5s (bounded in checkSuppression) + branding 5s + dial 10s + SMTP
// exchange 30s + bounce report 5s + mark tx ≈ 55s, rounded to 60s and
// ENFORCED by a per-row ctx — the budget is a guarantee, not an estimate.
// A row claimed but not attempted (budget exhausted) simply waits out its
// lease and is re-claimed; a row attempted but whose mark is lost is
// re-sent after the lease (at-least-once; the duplicate is one invoice
// email, bounded to one per crash).
const (
	PerRowBudget = 60 * time.Second
	// minRowStartBudget is the floor of remaining batch-ctx time below
	// which no new row is started (see the gate in ProcessBatch).
	minRowStartBudget = 10 * time.Second
	// claimLeaseMargin absorbs claim-tx latency + clock skew between the
	// claimer and a competing replica.
	claimLeaseMargin = 60 * time.Second
)

// ClaimLease is how far a claim pushes next_attempt_at into the future —
// the crash-recovery bound AND the double-send guard for a live-but-slow
// worker. Derived, never hand-tuned.
func ClaimLease(batchSize int) time.Duration {
	return time.Duration(batchSize)*PerRowBudget + claimLeaseMargin
}

// IsPermanentSendError classifies handler failures that can NEVER succeed
// on retry: riding the ~72h backoff ramp on these wastes 15 cycles before
// an operator sees 'failed' (and a suppressed recipient would be
// re-attempted against a known-dead inbox every slot). ProcessBatch DLQs
// these immediately.
func IsPermanentSendError(err error) bool {
	if errors.Is(err, ErrRecipientSuppressed) || errors.Is(err, ErrPayloadDecode) {
		return true
	}
	permanent, _ := isPermanentSMTPBounce(err)
	return permanent
}

// ErrPayloadDecode marks a row whose payload cannot be decoded — it will
// fail identically on every attempt.
var ErrPayloadDecode = errors.New("email outbox: payload decode failed")

// ErrEmailObsolete marks an action-required row whose invoice settled (or
// was torn down) while the row sat queued. ProcessBatch marks it 'skipped'
// — deliberately not sent; nothing failed, nothing was delivered.
var ErrEmailObsolete = errors.New("email outbox: obsolete — invoice settled before delivery")

// outboxBackoff returns the delay before the next attempt given the current
// attempt count (1 = after the first failure). Ramp: 1s, 5s, 30s, 2m, 5m,
// 15m, 30m, 1h, 2h, 4h, 8h, 12h, 12h, 12h, 12h — ~72h total over 15 tries.
func outboxBackoff(attempt int) time.Duration {
	ramp := []time.Duration{
		1 * time.Second,
		5 * time.Second,
		30 * time.Second,
		2 * time.Minute,
		5 * time.Minute,
		15 * time.Minute,
		30 * time.Minute,
		1 * time.Hour,
		2 * time.Hour,
		4 * time.Hour,
		8 * time.Hour,
		12 * time.Hour,
		12 * time.Hour,
		12 * time.Hour,
		12 * time.Hour,
	}
	idx := max(attempt-1, 0)
	if idx >= len(ramp) {
		idx = len(ramp) - 1
	}
	return ramp[idx]
}

// OutboxStore persists email-emission intents.
type OutboxStore struct {
	db *postgres.DB
}

func NewOutboxStore(db *postgres.DB) *OutboxStore {
	return &OutboxStore{db: db}
}

// Enqueue inserts a pending outbox row inside the caller's tx. Use this from
// a business-op store method so the email is persisted atomically with the
// state change — if the tx rolls back, no email is sent; if it commits, the
// dispatcher will eventually deliver.
func (s *OutboxStore) Enqueue(ctx context.Context, tx *sql.Tx, tenantID, emailType string, payload map[string]any) (string, error) {
	if tenantID == "" {
		return "", fmt.Errorf("email outbox: tenant_id required")
	}
	if emailType == "" {
		return "", fmt.Errorf("email outbox: email_type required")
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("email outbox: marshal payload: %w", err)
	}

	id := postgres.NewID("vlx_emob")
	// Billing-axis anchor (ADR-104): record WHICH simulated instant caused
	// this send, read from the bound ctx at the only moment the fact exists.
	// Every invoice-caused enqueue site already binds (settle → receipt,
	// finalize/sweeps → setup-link, catchup → reminders, operator actions →
	// bindForInvoice), so this single chokepoint anchors them all; unbound
	// ctx (live-mode mail, password resets, invites) records NULL — the two
	// calendars coincide and there is nothing to say. created_at stays SQL
	// now() on purpose: it is the real-world dispatch record (the
	// "Recorded …" subline), not the story position.
	var simAt any
	var clockID any
	if sim, ok := clock.SimOf(ctx); ok {
		simAt = sim.At.UTC()
		clockID = sim.TestClockID
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO email_outbox (id, tenant_id, email_type, payload, status, attempts, next_attempt_at, sim_effective_at, test_clock_id)
		VALUES ($1, $2, $3, $4, 'pending', 0, now(), $5, $6)
	`, id, tenantID, emailType, payloadJSON, simAt, clockID)
	if err != nil {
		return "", fmt.Errorf("email outbox: insert: %w", err)
	}
	return id, nil
}

// EnqueueStandalone opens its own tenant-scoped tx to insert the outbox row.
// Used by the OutboxSender adapter since the existing email interfaces don't
// carry a *sql.Tx. Durable (commits before return) but not atomic with the
// preceding business op — if the business tx committed and this insert
// fails, the caller just sees the error and logs it, same as the pre-outbox
// behaviour. Prefer Enqueue in new code that has a tx in scope.
func (s *OutboxStore) EnqueueStandalone(ctx context.Context, tenantID, emailType string, payload map[string]any) (string, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return "", fmt.Errorf("email outbox: begin tx: %w", err)
	}
	defer postgres.Rollback(tx)

	id, err := s.Enqueue(ctx, tx, tenantID, emailType, payload)
	if err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("email outbox: commit: %w", err)
	}
	return id, nil
}

// OutboxHandler is called once per claimed row by ProcessBatch. Returning nil
// means the email was dispatched successfully; returning an error schedules a
// retry (or DLQ once MaxOutboxAttempts is reached; permanent errors per
// IsPermanentSendError DLQ immediately).
type OutboxHandler func(ctx context.Context, row OutboxRow) error

// ProcessBatch drains up to `limit` due rows in the CLAIM-LEASE shape (P5):
//
//  1. CLAIM (one short tx): atomically bump attempts and push
//     next_attempt_at to now()+ClaimLease — the claim IS the attempt
//     counter (single increment site; a claim-then-crash row still
//     advances toward DLQ instead of cycling forever) and the lease is
//     both the crash-recovery bound and the double-send guard against a
//     live-but-slow sibling. FOR UPDATE SKIP LOCKED keeps concurrent
//     claimers disjoint; locks release at commit, NOT for the batch
//     duration — the old whole-batch tx held locks across every SMTP
//     round-trip, and its single commit LOST every mark when a later row
//     stalled past the tick deadline: delivered invoices re-sent.
//  2. SEND (per row): each row runs under its own PerRowBudget ctx; the
//     loop stops starting new rows when the batch ctx has less than one
//     budget remaining (unattempted rows just wait out their lease). A
//     panicking row is recovered per-row so it cannot strand the rest of
//     the batch.
//  3. MARK (per row, short tx on a ctx DETACHED from the batch deadline):
//     work already performed must always be recorded — a mark that dies
//     with the tick deadline is exactly the delivered-email-re-sent bug
//     this rewrite closes. Marks are CAS (WHERE status='pending') so a
//     stale writer can never regress a terminal row.
//
// Returns the number of rows attempted.
func (s *OutboxStore) ProcessBatch(ctx context.Context, limit int, handler OutboxHandler) (int, error) {
	if limit <= 0 {
		limit = 5
	}
	if handler == nil {
		return 0, fmt.Errorf("email outbox: handler required")
	}

	batch, err := s.claimBatch(ctx, limit)
	if err != nil {
		return 0, err
	}
	if len(batch) == 0 {
		return 0, nil
	}

	attempted := 0
	for _, row := range batch {
		// Budget gate: don't start a row into a nearly-dead batch ctx —
		// a send cancelled mid-DATA can be a delivered-but-errored
		// duplicate. Marks are DETACHED, so no outcome is ever lost;
		// this floor only shrinks the duplicate window. The floor is
		// deliberately below PerRowBudget: the dispatcher's sizing
		// (BatchSize×PerRowBudget ≤ BatchTimeout) means it never trips
		// this except after pathologically slow rows, while callers
		// with shorter ctxs (tests, ad-hoc drains) still make progress.
		// Unattempted remainder stays leased (attempts already bumped
		// at claim; one skipped cycle costs one backoff slot).
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < minRowStartBudget {
			slog.Warn("email outbox: batch budget exhausted — leaving remaining rows leased",
				"remaining", len(batch)-attempted)
			break
		}
		attempted++
		s.attemptRow(ctx, row, handler)
	}
	return attempted, nil
}

// claimBatch is phase 1: one short tx that selects due rows with
// FOR UPDATE SKIP LOCKED and, in the same statement, bumps attempts and
// leases them via next_attempt_at.
func (s *OutboxStore) claimBatch(ctx context.Context, limit int) ([]OutboxRow, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return nil, fmt.Errorf("email outbox: begin claim tx: %w", err)
	}
	defer postgres.Rollback(tx)

	rows, err := tx.QueryContext(ctx, `
		UPDATE email_outbox
		SET attempts = attempts + 1,
		    next_attempt_at = now() + make_interval(secs => $1)
		WHERE id IN (
			SELECT id FROM email_outbox
			WHERE status = 'pending' AND next_attempt_at <= now()
			ORDER BY next_attempt_at
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id, tenant_id, livemode, email_type, payload, status, attempts,
		          next_attempt_at, COALESCE(last_error,''), created_at, dispatched_at
	`, int(ClaimLease(limit).Seconds()), limit)
	if err != nil {
		return nil, fmt.Errorf("email outbox: claim: %w", err)
	}

	var batch []OutboxRow
	for rows.Next() {
		var r OutboxRow
		var payloadJSON []byte
		if err := rows.Scan(&r.ID, &r.TenantID, &r.Livemode, &r.EmailType, &payloadJSON,
			&r.Status, &r.Attempts, &r.NextAttemptAt, &r.LastError,
			&r.CreatedAt, &r.DispatchedAt); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("email outbox: scan: %w", err)
		}
		if len(payloadJSON) > 0 {
			_ = json.Unmarshal(payloadJSON, &r.Payload)
		}
		batch = append(batch, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("email outbox: rows err: %w", err)
	}
	_ = rows.Close()

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("email outbox: commit claim: %w", err)
	}
	return batch, nil
}

// attemptRow is phases 2+3 for one row: budget-bounded handler call
// (panic-recovered) followed by a detached-ctx CAS mark.
func (s *OutboxStore) attemptRow(ctx context.Context, row OutboxRow, handler OutboxHandler) {
	rowCtx, cancel := context.WithTimeout(ctx, PerRowBudget)
	hErr := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				// A poisoned row (pathological payload) must not strand
				// the rest of the batch or cycle lease-long forever with
				// no attempt record.
				err = fmt.Errorf("%w: handler panic: %v", ErrPayloadDecode, r)
			}
		}()
		return handler(rowCtx, row)
	}()
	cancel()

	// Marks run detached from the batch deadline: the work already
	// happened; failing to record it re-sends a delivered email.
	markCtx, markCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer markCancel()

	if hErr == nil {
		s.markCAS(markCtx, row.ID, `
			UPDATE email_outbox
			SET status = 'dispatched', dispatched_at = now(), last_error = NULL
			WHERE id = $1 AND status = 'pending'
		`)
		return
	}

	// Obsolete beats every other classification: the email must not be
	// sent, retried, or DLQ'd — the world moved on while it waited.
	if errors.Is(hErr, ErrEmailObsolete) {
		s.markCAS(markCtx, row.ID, `
			UPDATE email_outbox
			SET status = 'skipped', last_error = `+"$2"+`
			WHERE id = $1 AND status = 'pending'
		`, truncateError(hErr.Error()))
		slog.Info("email outbox: row skipped as obsolete",
			"outbox_id", row.ID,
			"tenant_id", row.TenantID,
			"email_type", row.EmailType,
		)
		return
	}

	errMsg := truncateError(hErr.Error())
	// row.Attempts is the CLAIM-persisted count (the single increment
	// site) — DLQ once the budget is spent, or immediately for errors
	// that can never succeed.
	if row.Attempts >= MaxOutboxAttempts || IsPermanentSendError(hErr) {
		s.markCAS(markCtx, row.ID, `
			UPDATE email_outbox
			SET status = 'failed', last_error = `+"$2"+`
			WHERE id = $1 AND status = 'pending'
		`, errMsg)
		slog.Error("email outbox: row moved to DLQ",
			"outbox_id", row.ID,
			"tenant_id", row.TenantID,
			"email_type", row.EmailType,
			"attempts", row.Attempts,
			"permanent", IsPermanentSendError(hErr),
			"error", errMsg,
		)
		return
	}

	nextAt := time.Now().UTC().Add(outboxBackoff(row.Attempts))
	s.markCAS(markCtx, row.ID, `
		UPDATE email_outbox
		SET next_attempt_at = $2, last_error = $3
		WHERE id = $1 AND status = 'pending'
	`, nextAt, errMsg)
}

// markCAS executes a per-row mark in its own short tx. The WHERE
// status='pending' guard means a stale mark (lease expired, another
// worker already resolved the row) matches zero rows and is dropped —
// logged, never silently absorbed into a terminal-state regression.
func (s *OutboxStore) markCAS(ctx context.Context, rowID, query string, args ...any) {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		slog.Error("email outbox: begin mark tx", "outbox_id", rowID, "error", err)
		return
	}
	defer postgres.Rollback(tx)
	res, err := tx.ExecContext(ctx, query, append([]any{rowID}, args...)...)
	if err != nil {
		slog.Error("email outbox: mark failed", "outbox_id", rowID, "error", err)
		return
	}
	n, raErr := res.RowsAffected()
	if raErr != nil {
		slog.Error("email outbox: mark rows-affected", "outbox_id", rowID, "error", raErr)
		return
	}
	if n == 0 {
		slog.Warn("email outbox: stale mark dropped (row no longer pending)", "outbox_id", rowID)
		return
	}
	if err := tx.Commit(); err != nil {
		slog.Error("email outbox: commit mark", "outbox_id", rowID, "error", err)
	}
}

// GetByID fetches one outbox row by id under a BYPASS-RLS read: the
// caller is the provider-webhook handler, which arrives with NO tenant
// context — the row IS the tenant resolution (same pattern as the
// blind-index lookup). The id is unguessable (96 random bits), so
// presenting a live one is itself the capability that binds the event to
// exactly that row's tenant. Returns sql.ErrNoRows when absent.
func (s *OutboxStore) GetByID(ctx context.Context, outboxID string) (OutboxRow, error) {
	var r OutboxRow
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return r, err
	}
	defer postgres.Rollback(tx)

	var payload []byte
	var dispatchedAt sql.NullTime
	if err := tx.QueryRowContext(ctx, `
		SELECT id, tenant_id, livemode, email_type, payload, status, attempts,
		       next_attempt_at, COALESCE(last_error,''), created_at, dispatched_at,
		       delivery_state
		FROM email_outbox
		WHERE id = $1
	`, outboxID).Scan(&r.ID, &r.TenantID, &r.Livemode, &r.EmailType, &payload, &r.Status,
		&r.Attempts, &r.NextAttemptAt, &r.LastError, &r.CreatedAt, &dispatchedAt,
		&r.DeliveryState); err != nil {
		return OutboxRow{}, err
	}
	if len(payload) > 0 {
		_ = json.Unmarshal(payload, &r.Payload)
	}
	if dispatchedAt.Valid {
		t := dispatchedAt.Time
		r.DispatchedAt = &t
	}
	return r, nil
}

// MarkDeliveryState records a provider-confirmed outcome on the row,
// SEVERITY-MONOTONIC: the write lands only when the new state outranks
// the current one (unknown < delivered < bounced < complained), so a
// redelivered webhook is a harmless no-op and a late Delivery can never
// downgrade a Bounce. Returns whether a row was updated — false is
// benign (already at-or-above). A state outside the known set ranks -1
// and never writes: an unrecognized value fails closed, not loud-crash,
// because the caller already 200-acks deterministically-unprocessable
// events. Runs under the row's own tenant tx (RLS) — the caller resolved
// tenantID from the row via GetByID.
func (s *OutboxStore) MarkDeliveryState(ctx context.Context, tenantID, outboxID, state string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return false, err
	}
	defer postgres.Rollback(tx)

	res, err := tx.ExecContext(ctx, `
		UPDATE email_outbox
		SET delivery_state = $2
		WHERE id = $1
		  AND (CASE $2 WHEN 'delivered' THEN 1 WHEN 'bounced' THEN 2
		               WHEN 'complained' THEN 3 ELSE -1 END)
		    > (CASE delivery_state WHEN 'unknown' THEN 0 WHEN 'delivered' THEN 1
		            WHEN 'bounced' THEN 2 WHEN 'complained' THEN 3 ELSE 99 END)
	`, outboxID, state)
	if err != nil {
		return false, fmt.Errorf("email outbox: mark delivery state: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return n > 0, nil
}

// TryDispatcherLock tries to acquire the cluster-wide advisory lock that
// gates the email dispatcher tick. Returns (lock, true, nil) on success;
// caller defers lock.Release. Returns (nil, false, nil) if another replica
// holds it. Satisfies email.DispatchLocker.
func (s *OutboxStore) TryDispatcherLock(ctx context.Context) (DispatchLock, bool, error) {
	lock, ok, err := s.db.TryAdvisoryLock(ctx, postgres.LockKeyEmailDispatcher)
	if err != nil || !ok {
		return nil, ok, err
	}
	return lock, true, nil
}

// PendingCount returns the current number of rows awaiting dispatch. Intended
// for metrics — not on the hot path.
func (s *OutboxStore) PendingCount(ctx context.Context) (int64, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return 0, err
	}
	defer postgres.Rollback(tx)

	var n int64
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM email_outbox WHERE status = 'pending'`,
	).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// ListByInvoice returns all email_outbox rows whose payload references the
// given invoice_number. Used by the invoice timeline to surface
// customer-notification events ("Customer notified — payment method
// required", dunning reminders, receipts) alongside Stripe webhook
// events. Filters to invoice-relevant email types; portal magic links
// and password resets are excluded by their email_type since they
// aren't invoice-scoped.
func (s *OutboxStore) ListByInvoice(ctx context.Context, tenantID, invoiceNumber string) ([]OutboxRow, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	rows, err := tx.QueryContext(ctx, `
		SELECT id, tenant_id, livemode, email_type, payload, status, attempts,
		       next_attempt_at, COALESCE(last_error,''), created_at, dispatched_at,
		       delivery_state, sim_effective_at, COALESCE(test_clock_id,'')
		FROM email_outbox
		WHERE email_type IN ('invoice', 'payment_receipt', 'payment_failed',
		                     'payment_setup_request', 'dunning_warning',
		                     'dunning_escalation', 'credit_note')
		  AND payload->>'invoice_number' = $1
		ORDER BY created_at ASC
	`, invoiceNumber)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []OutboxRow
	for rows.Next() {
		var r OutboxRow
		var payload []byte
		var dispatchedAt, simAt sql.NullTime
		if err := rows.Scan(&r.ID, &r.TenantID, &r.Livemode, &r.EmailType, &payload, &r.Status,
			&r.Attempts, &r.NextAttemptAt, &r.LastError, &r.CreatedAt, &dispatchedAt,
			&r.DeliveryState, &simAt, &r.TestClockID); err != nil {
			return nil, err
		}
		if simAt.Valid {
			t := simAt.Time
			r.SimEffectiveAt = &t
		}
		if err := json.Unmarshal(payload, &r.Payload); err != nil {
			return nil, fmt.Errorf("decode payload for %s: %w", r.ID, err)
		}
		if dispatchedAt.Valid {
			t := dispatchedAt.Time
			r.DispatchedAt = &t
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListByCustomer returns email_outbox rows associated with the given
// customer, last 30 days, newest first. Rows are matched by joining
// payload->>'invoice_number' to invoices.invoice_number and filtering
// on invoices.customer_id — same email_type allowlist as ListByInvoice
// (excludes platform-internal mail like password_reset that isn't
// customer-scoped). Powers the "Sent emails" section on the customer
// detail page (Stripe shape — docs.stripe.com/invoicing/send-email).
//
// 30-day window is a deliberate operator-UX choice matching Stripe's
// retention. Older audit history stays in the email_outbox table for
// log inspection but doesn't bloat the customer page.
func (s *OutboxStore) ListByCustomer(ctx context.Context, tenantID, customerID string) ([]OutboxRow, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	rows, err := tx.QueryContext(ctx, `
		SELECT eo.id, eo.tenant_id, eo.livemode, eo.email_type, eo.payload, eo.status,
		       eo.attempts, eo.next_attempt_at, COALESCE(eo.last_error,''),
		       eo.created_at, eo.dispatched_at, eo.delivery_state
		FROM email_outbox eo
		JOIN invoices i
		  ON i.tenant_id = eo.tenant_id
		 AND i.invoice_number = eo.payload->>'invoice_number'
		WHERE eo.email_type IN ('invoice', 'payment_receipt', 'payment_failed',
		                        'payment_setup_request', 'dunning_warning',
		                        'dunning_escalation', 'credit_note')
		  AND i.customer_id = $1
		  AND eo.created_at >= now() - interval '30 days'
		ORDER BY COALESCE(eo.dispatched_at, eo.created_at) DESC
	`, customerID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []OutboxRow
	for rows.Next() {
		var r OutboxRow
		var payload []byte
		var dispatchedAt sql.NullTime
		if err := rows.Scan(&r.ID, &r.TenantID, &r.Livemode, &r.EmailType, &payload, &r.Status,
			&r.Attempts, &r.NextAttemptAt, &r.LastError, &r.CreatedAt, &dispatchedAt,
			&r.DeliveryState); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(payload, &r.Payload); err != nil {
			return nil, fmt.Errorf("decode payload for %s: %w", r.ID, err)
		}
		if dispatchedAt.Valid {
			t := dispatchedAt.Time
			r.DispatchedAt = &t
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// FailedCount returns rows currently in the DLQ — used for alerting. Growth
// means SMTP persistently broken, a producer emitting malformed payloads, or
// (post-P5) suppressed recipients being written to — permanent failures land
// here immediately now.
func (s *OutboxStore) FailedCount(ctx context.Context) (int64, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return 0, err
	}
	defer postgres.Rollback(tx)

	var n int64
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM email_outbox WHERE status = 'failed'`,
	).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

func truncateError(s string) string {
	const maxLen = 500
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen]
}
