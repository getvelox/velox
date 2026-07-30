// Package scheduler hosts the standard tick-loop runner used by every
// ticker-driven background worker in Velox. There are exactly four, and
// `grep -rn "scheduler.Run("` is the authoritative list: billing (1h prod /
// 5m local), webhook_outbox (2s), email_outbox (5s), webhook_retry (30s).
// The test-clock catchup worker is queue-driven, not ticker-driven, so it
// does not use this runner.
//
// Before this helper, every worker hand-rolled the same select-on-
// ticker loop. Five copies started drifting (some logged a heartbeat,
// some didn't; none had panic recovery, so a panic mid-tick would
// silently kill the goroutine and the ticker would buffer forever).
// This package consolidates the shape so each worker is ~3 lines and
// shares one place to wire panic recovery, heartbeat logging, and
// future hooks (status registry, prometheus counters).
package scheduler

import (
	"context"
	"log/slog"
	"runtime/debug"
	"time"
)

// heartbeatThreshold marks the boundary between "slow" workers (that
// log a heartbeat at INFO every tick) and "fast" workers (where INFO
// per tick would be log-volume noise — they emit at DEBUG instead).
//
// 1 minute keeps billing / billing-alerts / webhook-retry visible in
// `tail -f` for an operator running locally; sub-minute workers
// (email outbox 5s, webhook outbox 2s) stay quiet and rely on the
// per-dispatch logs they already emit when work happens.
const heartbeatThreshold = 1 * time.Minute

// WorkFunc is the per-tick body. It must be safe to call repeatedly
// and return cleanly on ctx cancellation. Errors are the WorkFunc's
// to log — the runner does not attempt to interpret return values
// because each worker has its own outcome shape (count + []error,
// just error, just nothing).
type WorkFunc func(ctx context.Context)

// Run drives a standard tick loop until ctx is cancelled. Every tick:
//
//  1. Logs a heartbeat at INFO (interval >= 1 minute) or DEBUG
//     (sub-minute), so a `tail -f` shows visible pulse for slow
//     workers without flooding for fast ones.
//
//  2. Calls workFn inside a panic-recovering wrapper. A panic during
//     a single tick logs at ERROR with the recovered value plus a
//     full stack and continues to the next tick — without this, a
//     panic would kill the goroutine silently while the ticker
//     channel kept buffering.
//
// On ctx.Done the runner logs "stopped" and returns. Boot/teardown
// log lines are emitted by the caller before/after Run so each
// worker can choose its phrasing ("dispatcher started", "evaluator
// started", etc.) and include any worker-specific metadata
// (batch size, target queue) the runner doesn't know about.
func Run(ctx context.Context, name string, interval time.Duration, workFn WorkFunc) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	heartbeat := slog.LevelInfo
	if interval < heartbeatThreshold {
		heartbeat = slog.LevelDebug
	}

	for {
		select {
		case <-ctx.Done():
			slog.Info("scheduler stopped", "worker", name)
			return
		case <-ticker.C:
			slog.Log(ctx, heartbeat, "scheduler tick", "worker", name)
			runOneTick(ctx, name, workFn)
		}
	}
}

// runOneTick wraps a single workFn invocation in a recover() so a
// panic doesn't kill the runner goroutine. Logs the recovered value
// + a stack at ERROR; the caller's next tick fires normally.
func runOneTick(ctx context.Context, name string, workFn WorkFunc) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("scheduler panic recovered",
				"worker", name,
				"panic", r,
				"stack", string(debug.Stack()),
			)
		}
	}()
	workFn(ctx)
}
