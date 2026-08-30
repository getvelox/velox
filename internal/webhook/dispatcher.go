package webhook

import (
	"context"
	"log/slog"
	"time"

	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/platform/scheduler"
)

// DispatcherConfig controls the outbox dispatcher loop.
type DispatcherConfig struct {
	// Interval is the poll cadence between ProcessBatch calls. Default 2s if zero.
	Interval time.Duration
	// BatchSize bounds how many rows are claimed per tick. Default 25 if zero.
	BatchSize int
	// BatchTimeout bounds how long a single batch may run. Defaults to
	// BatchSize × the per-row budget if zero. No row locks are held across
	// the batch — the claim transaction commits immediately and the claim
	// LEASE owns exclusion (ADR-072); the timeout bounds the tick so leased
	// rows become re-claimable on schedule.
	BatchTimeout time.Duration
}

// DispatchLock is a held cluster-wide lock the dispatcher must release.
type DispatchLock interface {
	Release()
}

// DispatchLocker gates the dispatcher tick on a cluster-wide advisory lock.
// Row-level FOR UPDATE SKIP LOCKED already prevents double-delivery when two
// dispatchers race, but the lock avoids both replicas issuing the same claim
// query every 2s when only one drain worker is actually needed — less churn
// on the connection pool and on webhook_outbox's index scan. Nil Locker
// disables gating (single-replica / test mode).
type DispatchLocker interface {
	TryDispatcherLock(ctx context.Context) (DispatchLock, bool, error)
}

// Dispatcher drains the webhook_outbox by invoking Service.Dispatch for each
// pending row. It is the bridge between the durable outbox (what producers
// enqueue) and the existing per-endpoint delivery pipeline (webhook_events +
// webhook_deliveries). Handler semantics: a row is marked 'dispatched' once
// Service.Dispatch returns nil, which means the event has been persisted and
// queued to all matching endpoints — per-endpoint HTTP retry is then owned
// by Service.StartRetryWorker, independent of the outbox.
type Dispatcher struct {
	outbox *OutboxStore
	svc    *Service
	cfg    DispatcherConfig
	locker DispatchLocker
}

func NewDispatcher(outbox *OutboxStore, svc *Service, cfg DispatcherConfig) *Dispatcher {
	if cfg.Interval <= 0 {
		cfg.Interval = 2 * time.Second
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 25
	}
	if cfg.BatchTimeout <= 0 {
		// Invariant chain (ADR-072): BatchSize×outboxPerRowBudget ≤
		// BatchTimeout < outboxClaimLease(BatchSize). The old 30s
		// default violated it (25×10s=250s > 30s): under a degraded DB
		// the budget gate tripped and the unattempted remainder burned
		// a DLQ slot with zero attempts while staying leased.
		cfg.BatchTimeout = time.Duration(cfg.BatchSize) * outboxPerRowBudget
	}
	return &Dispatcher{outbox: outbox, svc: svc, cfg: cfg}
}

// SetLocker enables leader gating on the dispatcher tick.
func (d *Dispatcher) SetLocker(locker DispatchLocker) {
	d.locker = locker
}

// Config exposes the resolved configuration for the invariant test.
func (d *Dispatcher) Config() DispatcherConfig { return d.cfg }

// Start runs the dispatcher loop until ctx is cancelled. Intended to be
// launched as a goroutine from cmd/velox during boot, alongside the existing
// webhook retry worker.
func (d *Dispatcher) Start(ctx context.Context) {
	slog.Info("webhook outbox dispatcher started",
		"interval", d.cfg.Interval.String(),
		"batch_size", d.cfg.BatchSize,
	)
	scheduler.Run(ctx, "webhook_outbox", d.cfg.Interval, d.tick)
}

// tick drains one batch. Errors are logged and swallowed — the next tick will
// retry. The per-tick timeout bounds a tick so leased rows are re-claimable
// on schedule (no row locks are held across a batch post-ADR-072).
func (d *Dispatcher) tick(ctx context.Context) {
	batchCtx, cancel := context.WithTimeout(ctx, d.cfg.BatchTimeout)
	defer cancel()

	if d.locker != nil {
		lock, acquired, err := d.locker.TryDispatcherLock(batchCtx)
		if err != nil {
			slog.Error("webhook outbox dispatcher: lock acquire failed", "error", err)
			return
		}
		if !acquired {
			return
		}
		defer lock.Release()
	}

	n, err := d.outbox.ProcessBatch(batchCtx, d.cfg.BatchSize, d.handle)
	if err != nil {
		slog.Error("webhook outbox dispatcher: batch error", "error", err)
		return
	}
	if n > 0 {
		slog.Debug("webhook outbox dispatcher: batch processed", "count", n)
	}
}

// handle is the per-row handler. Returning nil marks the row 'dispatched';
// returning an error schedules a retry (or DLQ after MaxOutboxAttempts).
//
// Propagates the outbox row's livemode into ctx so the downstream Dispatch
// lands the webhook_event/deliveries in the same partition and matches only
// same-mode endpoints. Without this, a test-mode producer would cross over
// to live endpoints (or vice versa) because the background dispatcher has
// no intrinsic mode.
func (d *Dispatcher) handle(ctx context.Context, row OutboxRow) error {
	ctx = postgres.WithLivemode(ctx, row.Livemode)
	// handler-owns-mark (P5): the outbox row's dispatched-mark commits
	// inside the same tx as the event + delivery rows, so a crash
	// between the two can no longer mint a duplicate event with a fresh
	// id. ProcessBatch's own success mark becomes a CAS no-op backstop.
	return d.svc.DispatchFromOutbox(ctx, row.ID, row.TenantID, row.EventType, row.Payload)
}
