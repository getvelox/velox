package testclock

import (
	"context"
	"errors"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// CatchupJob is the unit of work the async worker processes — one
// clock's worth of post-advance billing catchup. Carries tenant +
// clock identity; the worker reloads frozen_time and the sub list
// inside its own ctx (RLS-scoped per-tenant).
type CatchupJob struct {
	TenantID string
	ClockID  string
	// PrevFrozenTime is the clock's frozen_time BEFORE this advance (the
	// simulated "from" instant), captured by Advance before MarkAdvancing
	// overwrites it. RunCatchup records it as the advance summary's
	// AdvancedFrom so the dashboard can show the span the advance covered.
	// Zero on the recover-in-flight path (the pre-advance value is no longer
	// available after a restart); the summary then shows only the destination.
	PrevFrozenTime time.Time
}

// CatchupQueue is the narrow contract the service uses to dispatch
// catchup work. Behind a small interface so the in-process channel
// implementation here can be swapped for a Redis/durable queue
// later without touching the service.
//
// Enqueue is non-blocking but bounded — if the buffer is full, the
// caller gets an error rather than blocking the request handler.
// At expected operator volumes (one operator clicking Advance, not
// a load test) the buffer never fills; on full it surfaces as a
// 503 to the caller, which is the right shape compared to a hung
// request handler.
type CatchupQueue interface {
	Enqueue(job CatchupJob) error
}

// CatchupRunner is the function the worker calls per job. The
// service satisfies it via Service.RunCatchup. Kept as a function
// type to avoid a method-on-struct dependency cycle and to make
// the worker easy to stub in tests.
type CatchupRunner func(ctx context.Context, job CatchupJob) error

// CatchupTimeout caps the wall-clock duration of a single advance
// catchup. Defends against pathological data shapes (e.g. an
// operator advancing 10 years on a daily-billed sub) that would
// otherwise tie up a worker indefinitely. On timeout the worker
// flips the clock to internal_failure so the operator can
// inspect-and-delete to recover.
const CatchupTimeout = 10 * time.Minute

// ErrQueueFull is returned by Enqueue when the buffered channel is
// at capacity. Callers translate to 503 Service Unavailable.
var ErrQueueFull = errors.New("test-clock catchup queue is full")

// chanCatchupQueue is a bounded, in-process channel-backed queue.
// Single-process — fine for Velox's single-binary deployment shape.
// When/if multi-replica self-hosters arrive, swap for a Redis or
// Postgres LISTEN/NOTIFY queue without touching service code.
type chanCatchupQueue struct {
	ch chan CatchupJob
}

// NewCatchupQueue returns a queue with the given buffer size.
// Buffer should be larger than max-expected concurrent in-flight
// advances; 100 is generous.
func NewCatchupQueue(buffer int) *chanCatchupQueue {
	if buffer <= 0 {
		buffer = 100
	}
	return &chanCatchupQueue{ch: make(chan CatchupJob, buffer)}
}

func (q *chanCatchupQueue) Enqueue(job CatchupJob) error {
	select {
	case q.ch <- job:
		return nil
	default:
		return ErrQueueFull
	}
}

// CatchupWorker drains the queue and runs catchup for each job.
// Single-goroutine on purpose — concurrent catchups on the same
// tenant fight for the same RLS partition and the same DB rows,
// so serialising is simpler and there's no per-clock isolation
// that parallelism would buy us at expected operator volumes.
type CatchupWorker struct {
	queue  *chanCatchupQueue
	runner CatchupRunner
	stopCh chan struct{}
	doneCh chan struct{}
	once   sync.Once
}

// NewCatchupWorker wires a worker around a queue + runner. Caller
// must call Start to begin draining; Stop to shut down cleanly.
func NewCatchupWorker(queue *chanCatchupQueue, runner CatchupRunner) *CatchupWorker {
	return &CatchupWorker{
		queue:  queue,
		runner: runner,
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}
}

// Start spawns the drain goroutine. Idempotent — repeat calls are
// no-ops (sync.Once-guarded) so a misconfigured boot path can't
// produce two competing workers.
func (w *CatchupWorker) Start() {
	w.once.Do(func() {
		go w.run()
	})
}

func (w *CatchupWorker) run() {
	defer close(w.doneCh)
	slog.Info("test-clock catchup worker started")
	for {
		select {
		case <-w.stopCh:
			slog.Info("test-clock catchup worker stopped")
			return
		case job, ok := <-w.queue.ch:
			if !ok {
				return
			}
			w.process(job)
		}
	}
}

// process runs one catchup with a wall-clock timeout. Test clocks
// are always test-mode (CHECK constraint at the table level), so
// the ctx is pinned to livemode=false — required by the engine
// because subs on a clock have next_billing_at compared against
// the clock's frozen_time, which only makes sense inside the
// test-mode RLS partition.
func (w *CatchupWorker) process(job CatchupJob) {
	ctx, cancel := context.WithTimeout(context.Background(), CatchupTimeout)
	defer cancel()
	ctx = postgres.WithLivemode(ctx, false)

	// A panic in the runner (nil-deref in some billing path, etc.) would
	// otherwise unwind this goroutine and crash the whole process, taking
	// down the API server for every tenant. Recover, log with the stack, and
	// let the drain loop continue — one bad clock can't kill the server.
	// Mirrors scheduler.runOneTick's recovering wrapper.
	defer func() {
		if r := recover(); r != nil {
			slog.Error("test-clock catchup panicked",
				"clock_id", job.ClockID,
				"tenant_id", job.TenantID,
				"panic", r,
				"stack", string(debug.Stack()),
			)
		}
	}()

	start := time.Now()
	if err := w.runner(ctx, job); err != nil {
		slog.Error("test-clock catchup failed",
			"clock_id", job.ClockID,
			"tenant_id", job.TenantID,
			"duration_ms", time.Since(start).Milliseconds(),
			"error", err,
		)
		return
	}
	slog.Info("test-clock catchup complete",
		"clock_id", job.ClockID,
		"tenant_id", job.TenantID,
		"duration_ms", time.Since(start).Milliseconds(),
	)
}

// Stop signals the worker to exit and waits up to deadline for any
// in-flight job to finish. Returns true if the worker stopped cleanly,
// false on timeout — an abandoned advance leaves the clock 'advancing'
// and resumes from durable state on the next boot's RecoverInFlight.
func (w *CatchupWorker) Stop(deadline time.Duration) bool {
	close(w.stopCh)
	select {
	case <-w.doneCh:
		return true
	case <-time.After(deadline):
		return false
	}
}
