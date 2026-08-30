package webhook

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/sagarsuperuser/velox/internal/platform/leader"
	"log/slog"
	"time"

	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// OutboxRow is a single queued outbound-event intent. Produced inside the
// business-op transaction, drained by the dispatcher.
type OutboxRow struct {
	ID            string
	TenantID      string
	Livemode      bool // Carries the producer tx's mode; dispatcher propagates this into ctx so delivery hits only same-mode endpoints.
	EventType     string
	Payload       map[string]any
	Status        string
	Attempts      int
	NextAttemptAt time.Time
	LastError     string
	CreatedAt     time.Time
	DispatchedAt  *time.Time
}

// outbox row statuses.
const (
	OutboxPending    = "pending"
	OutboxDispatched = "dispatched"
	OutboxFailed     = "failed"
)

// MaxOutboxAttempts is the DLQ threshold. After this many failed attempts a
// row becomes terminal ('failed') and is no longer retried automatically.
// Backoff ramp (see outboxBackoff) totals ~72h across 15 attempts.
const MaxOutboxAttempts = 15

// Claim-lease sizing (P5, symmetric with the email outbox — see
// internal/email/outbox.go for the full model). The webhook outbox
// handler is DB-bound (event + delivery rows in one tx + goroutine
// spawns; no network wait), so its per-row budget is small.
const (
	outboxPerRowBudget         = 10 * time.Second
	outboxMinRowStartBudget    = 3 * time.Second
	outboxClaimLeaseMarginSecs = 60
)

func outboxClaimLease(batchSize int) time.Duration {
	return time.Duration(batchSize)*outboxPerRowBudget + outboxClaimLeaseMarginSecs*time.Second
}

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

// OutboxStore persists webhook-event emission intents.
type OutboxStore struct {
	db *postgres.DB
}

func NewOutboxStore(db *postgres.DB) *OutboxStore {
	return &OutboxStore{db: db}
}

// Enqueue inserts a pending outbox row inside the caller's tx. Use this from
// a business-op store method so the event is persisted atomically with the
// state change — if the tx rolls back, no event is emitted; if it commits,
// the dispatcher will eventually deliver.
func (s *OutboxStore) Enqueue(ctx context.Context, tx *sql.Tx, tenantID, eventType string, payload map[string]any) (string, error) {
	if tenantID == "" {
		return "", fmt.Errorf("outbox: tenant_id required")
	}
	if eventType == "" {
		return "", fmt.Errorf("outbox: event_type required")
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("outbox: marshal payload: %w", err)
	}

	id := postgres.NewID("vlx_whob")
	_, err = tx.ExecContext(ctx, `
		INSERT INTO webhook_outbox (id, tenant_id, event_type, payload, status, attempts, next_attempt_at)
		VALUES ($1, $2, $3, $4, 'pending', 0, now())
	`, id, tenantID, eventType, payloadJSON)
	if err != nil {
		return "", fmt.Errorf("outbox: insert: %w", err)
	}
	return id, nil
}

// EnqueueStandalone opens its own tenant-scoped tx to insert the outbox row.
// Use when the caller has no tx already in scope — still durable because the
// insert commits before return, but not atomic with the preceding business op.
// Prefer Enqueue whenever a tx is available.
func (s *OutboxStore) EnqueueStandalone(ctx context.Context, tenantID, eventType string, payload map[string]any) (string, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return "", fmt.Errorf("outbox: begin tx: %w", err)
	}
	defer postgres.Rollback(tx)

	id, err := s.Enqueue(ctx, tx, tenantID, eventType, payload)
	if err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("outbox: commit: %w", err)
	}
	return id, nil
}

// OutboxHandler is called once per claimed row by ProcessBatch. Returning nil
// means the row is dispatched successfully; returning an error schedules a
// retry (or DLQ once MaxOutboxAttempts is reached).
type OutboxHandler func(ctx context.Context, row OutboxRow) error

// ProcessBatch locks up to `limit` due pending rows across all tenants,
// hands them to `handler`, and marks each row based on the handler's result
// — all within a single tx. Row locks held for the tx's duration prevent
// concurrent dispatchers from double-delivering (`FOR UPDATE SKIP LOCKED`).
//
// Returns the number of rows processed (attempted, regardless of outcome).
// Callers should set a sensible query timeout on ctx so a stuck handler
// can't hold locks indefinitely.
func (s *OutboxStore) ProcessBatch(ctx context.Context, limit int, handler OutboxHandler) (int, error) {
	// Leader-only funnel (ADR-114): see subscription.GetDueBilling.
	token, err := leader.Fence(ctx, leader.RoleWebhookOutbox)
	if err != nil {
		return 0, err
	}
	if limit <= 0 {
		limit = 10
	}
	if handler == nil {
		return 0, fmt.Errorf("outbox: handler required")
	}

	// CLAIM (one short tx, P5 — same shape as the email outbox): bump
	// attempts and lease via next_attempt_at, locks released at commit.
	// The old whole-batch tx held row locks across every handler call
	// and lost ALL marks if a late row pushed past the tick deadline.
	// The attempts bump at claim is the single increment site — a
	// claim-then-crash row still advances toward DLQ.
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return 0, fmt.Errorf("outbox: begin claim tx: %w", err)
	}
	defer postgres.Rollback(tx)

	rows, err := tx.QueryContext(ctx, `
		UPDATE webhook_outbox
		SET attempts = attempts + 1,
		    next_attempt_at = now() + make_interval(secs => $1)
		WHERE id IN (
			SELECT id FROM webhook_outbox
			WHERE status = 'pending' AND next_attempt_at <= now()
			  AND leader_fence($3, $4)
			ORDER BY next_attempt_at
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id, tenant_id, livemode, event_type, payload, status, attempts,
		          next_attempt_at, COALESCE(last_error,''), created_at, dispatched_at
	`, int(outboxClaimLease(limit).Seconds()), limit, string(leader.RoleWebhookOutbox), token)
	if err != nil {
		return 0, fmt.Errorf("outbox: claim: %w", err)
	}

	var batch []OutboxRow
	for rows.Next() {
		var r OutboxRow
		var payloadJSON []byte
		if err := rows.Scan(&r.ID, &r.TenantID, &r.Livemode, &r.EventType, &payloadJSON,
			&r.Status, &r.Attempts, &r.NextAttemptAt, &r.LastError,
			&r.CreatedAt, &r.DispatchedAt); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("outbox: scan: %w", err)
		}
		if len(payloadJSON) > 0 {
			_ = json.Unmarshal(payloadJSON, &r.Payload)
		}
		batch = append(batch, r)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("outbox: rows err: %w", err)
	}
	_ = rows.Close()
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("outbox: commit claim: %w", err)
	}

	if len(batch) == 0 {
		return 0, nil
	}

	attempted := 0
	for _, row := range batch {
		// Floor, not the full budget: marks are detached so outcomes
		// are never lost; see the email outbox's gate rationale.
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < outboxMinRowStartBudget {
			slog.Warn("webhook outbox: batch budget exhausted — leaving remaining rows leased",
				"remaining", len(batch)-attempted)
			break
		}
		attempted++
		s.attemptRow(ctx, row, handler)
	}
	return attempted, nil
}

// attemptRow runs the handler under the per-row budget (panic-recovered)
// then CAS-marks on a ctx detached from the batch deadline. With
// handler-owns-mark (DispatchFromOutbox marks the row inside the event
// tx), the success mark here is a no-op backstop — the CAS's zero-row
// case, expected and silent for success.
func (s *OutboxStore) attemptRow(ctx context.Context, row OutboxRow, handler OutboxHandler) {
	rowCtx, cancel := context.WithTimeout(ctx, outboxPerRowBudget)
	hErr := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("outbox handler panic: %v", r)
			}
		}()
		return handler(rowCtx, row)
	}()
	cancel()

	markCtx, markCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer markCancel()

	mtx, err := s.db.BeginTx(markCtx, postgres.TxBypass, "")
	if err != nil {
		slog.Error("webhook outbox: begin mark tx", "outbox_id", row.ID, "error", err)
		return
	}
	defer postgres.Rollback(mtx)

	if hErr == nil {
		// Backstop only — DispatchFromOutbox already marked it in-tx.
		_, _ = mtx.ExecContext(markCtx, `
			UPDATE webhook_outbox
			SET status = 'dispatched', dispatched_at = now(), last_error = NULL
			WHERE id = $1 AND status = 'pending'
		`, row.ID)
		if err := mtx.Commit(); err != nil {
			slog.Error("webhook outbox: commit backstop mark", "outbox_id", row.ID, "error", err)
		}
		return
	}

	errMsg := truncateError(hErr.Error())
	if row.Attempts >= MaxOutboxAttempts {
		if _, err := mtx.ExecContext(markCtx, `
			UPDATE webhook_outbox
			SET status = 'failed', last_error = $2
			WHERE id = $1 AND status = 'pending'
		`, row.ID, errMsg); err != nil {
			slog.Error("webhook outbox: DLQ mark failed", "outbox_id", row.ID, "error", err)
			return
		}
		if err := mtx.Commit(); err != nil {
			slog.Error("webhook outbox: commit DLQ mark", "outbox_id", row.ID, "error", err)
			return
		}
		slog.Error("webhook outbox: row moved to DLQ after max attempts",
			"outbox_id", row.ID,
			"tenant_id", row.TenantID,
			"event_type", row.EventType,
			"attempts", row.Attempts,
			"error", errMsg,
		)
		return
	}

	nextAt := time.Now().UTC().Add(outboxBackoff(row.Attempts))
	if _, err := mtx.ExecContext(markCtx, `
		UPDATE webhook_outbox
		SET next_attempt_at = $2, last_error = $3
		WHERE id = $1 AND status = 'pending'
	`, row.ID, nextAt, errMsg); err != nil {
		slog.Error("webhook outbox: retry mark failed", "outbox_id", row.ID, "error", err)
		return
	}
	if err := mtx.Commit(); err != nil {
		slog.Error("webhook outbox: commit retry mark", "outbox_id", row.ID, "error", err)
	}
}

// PendingCount returns the current number of rows awaiting dispatch. Intended
// for metrics (operator gauge) — not on the hot path.
func (s *OutboxStore) PendingCount(ctx context.Context) (int64, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return 0, err
	}
	defer postgres.Rollback(tx)

	var n int64
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM webhook_outbox WHERE status = 'pending'`,
	).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// FailedCount returns rows currently in the DLQ — used for alerting. If this
// grows, an endpoint is persistently broken or a producer is emitting
// malformed events.
func (s *OutboxStore) FailedCount(ctx context.Context) (int64, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return 0, err
	}
	defer postgres.Rollback(tx)

	var n int64
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM webhook_outbox WHERE status = 'failed'`,
	).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

func truncateError(s string) string {
	const max = 500
	if len(s) <= max {
		return s
	}
	return s[:max]
}
