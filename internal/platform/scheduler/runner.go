// Package scheduler hosts the standard tick-loop runner used by every
// ticker-driven singleton worker in Velox. There are exactly five, and
// `grep -rn "scheduler.Run("` is the authoritative list: billing (1h prod /
// 5m local), dunning (same interval), webhook_outbox (2s), email_outbox (5s),
// webhook_delivery (30s). The test-clock catchup worker is queue-driven, not
// ticker-driven, so it does not use this runner.
//
// Every loop is leader-gated (ADR-114): a tick runs only when this replica
// leads the role for that tick, on a ctx carrying the lease token the
// role's claim funnel proves. The gate is a required parameter — a loop
// cannot forget it — and the runner polls at min(interval, leader.MaxPoll)
// so a dead leader's role runs elsewhere within LeaseTTL + MaxPoll.
//
// Before this helper, every worker hand-rolled the same select-on-ticker
// loop; copies drifted (heartbeat logging, no panic recovery). This package
// consolidates the shape so each worker is ~3 lines and shares one place
// for panic recovery, heartbeat logging and the leader gate.
package scheduler

import (
	"context"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/sagarsuperuser/velox/internal/platform/leader"
)

// heartbeatThreshold marks the boundary between "slow" roles (that log at
// INFO when they lead a tick) and "fast" roles (DEBUG — INFO per tick would
// be log-volume noise).
const heartbeatThreshold = 1 * time.Minute

// WorkFunc is the per-tick body. It must be safe to call repeatedly and
// return cleanly on ctx cancellation — the ctx is cancelled with cause
// leader.ErrLeaseLost if the lease cannot be kept. Errors are the
// WorkFunc's to log.
type WorkFunc func(ctx context.Context)

// Run drives a leader-gated tick loop for role until ctx is cancelled.
//
// Every poll (min(interval, leader.MaxPoll)): onPoll fires if non-nil
// (used by the billing loop to stamp this replica's liveness for
// /health/ready — followers stamp too, which is today's contract); then
// gate.Lead tries to take one tick of the role. Not due / held elsewhere /
// paused → nothing happens. Led → workFn runs inside a panic-recovering
// wrapper on the led ctx; a panic logs at ERROR with the stack and the
// lease is still released (the gate defers it). A gate error logs at
// ERROR once per poll — the existing per-tick posture, no rate limiter.
func Run(ctx context.Context, role leader.Role, interval time.Duration, gate leader.Gate, workFn WorkFunc, onPoll func()) {
	if gate == nil {
		slog.Error("scheduler: no leader gate wired — refusing to run an ungated singleton loop", "role", role)
		return
	}
	poll := interval
	if poll > leader.MaxPoll {
		poll = leader.MaxPoll
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()

	level := slog.LevelInfo
	if interval < heartbeatThreshold {
		level = slog.LevelDebug
	}

	for {
		select {
		case <-ctx.Done():
			slog.Info("scheduler stopped", "worker", role)
			return
		case <-ticker.C:
			if onPoll != nil {
				onPoll()
			}
			start := time.Now()
			led, err := gate.Lead(ctx, role, interval, func(c context.Context) { runOneTick(c, string(role), workFn) })
			if err != nil {
				slog.Error("scheduler: leader gate error", "worker", role, "error", err)
				continue
			}
			if led {
				slog.Log(ctx, level, "scheduler tick led", "worker", role, "took", time.Since(start).Round(time.Millisecond).String())
			}
		}
	}
}

// runOneTick wraps a single workFn invocation in a recover() so a panic
// doesn't kill the runner goroutine. Logs the recovered value + a stack at
// ERROR; the caller's next tick fires normally.
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
