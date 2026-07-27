package dunning

import (
	"context"
	"github.com/sagarsuperuser/velox/internal/domain"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/platform/clock"
)

// TestProcessDueRuns_WallClockCadenceAfterDowntime covers the medium-severity
// audit finding: on the pure wall-clock cron path, a retry anchored on a stale
// run.NextActionAt. After the scheduler was down for several intervals,
// NextActionAt is far in the past, so the next retry was scheduled at
// staleNextActionAt + interval — still in the past — and the whole backlog
// fired back-to-back in one tick, collapsing the configured cadence.
//
// The fix clamps the anchor to max(now, NextActionAt) on the non-catchup,
// non-clock-pinned path, so a recovered scheduler resumes the cadence from the
// recovery instant.
func TestProcessDueRuns_WallClockCadenceAfterDowntime(t *testing.T) {
	recoveryNow := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	store := newMemStore()
	// No resolver wired → pinned=false; pure wall-clock path.
	svc := NewService(store, &failingRetrier{}, clock.NewFake(recoveryNow))
	ctx := context.Background()

	run, _ := svc.StartDunning(ctx, "t1", "inv_1", "cus_1", recoveryNow.Add(-30*24*time.Hour), domain.DunningCausePaymentFailed)
	// Simulate multi-interval scheduler downtime: this run was due 10 days ago
	// and never fired.
	stalePast := recoveryNow.Add(-10 * 24 * time.Hour)
	run.AttemptCount = 0
	run.NextActionAt = &stalePast
	store.runs[run.ID] = run

	if _, errs := svc.ProcessDueRuns(ctx, "t1", 20); len(errs) > 0 {
		t.Fatalf("ProcessDueRuns errors: %v", errs)
	}

	updated := store.runs[run.ID]
	if updated.LastAttemptAt == nil {
		t.Fatal("last_attempt_at should be stamped")
	}
	// Anchored on the recovery instant, NOT the stale scheduled time.
	if !updated.LastAttemptAt.Equal(recoveryNow) {
		t.Errorf("last_attempt_at: got %v, want %v (clamped to recovery now, not stale past %v)",
			*updated.LastAttemptAt, recoveryNow, stalePast)
	}
	if updated.NextActionAt == nil {
		t.Fatal("next_action_at should be re-stamped")
	}
	// Next retry scheduled forward from recovery, not back-dated into the past.
	wantNext := recoveryNow.Add(72 * time.Hour) // schedule[0]
	if !updated.NextActionAt.Equal(wantNext) {
		t.Errorf("next_action_at: got %v, want %v (recovery now + 72h interval)", *updated.NextActionAt, wantNext)
	}
	if !updated.NextActionAt.After(recoveryNow) {
		t.Errorf("next_action_at %v is not in the future — cadence collapsed", *updated.NextActionAt)
	}
}
