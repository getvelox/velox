package testclock

import (
	"context"
	"testing"
	"time"
)

// TestCatchupWorker_Stop_HonorsDeadlineWhileJobRuns pins stage 2 of the
// shutdown budget (2026-08-30): Stop returns false within its deadline
// while an advance is still running, instead of waiting CatchupTimeout
// (10 min) and blowing every orchestrator grace period. The abandoned
// advance resumes from durable state on the next boot.
// Mutation-verify: make Stop ignore the deadline → this test times out.
func TestCatchupWorker_Stop_HonorsDeadlineWhileJobRuns(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	q := NewCatchupQueue(1)
	w := NewCatchupWorker(q, func(_ context.Context, _ CatchupJob) error {
		close(entered)
		<-release
		close(finished)
		return nil
	})
	w.Start()
	if err := q.Enqueue(CatchupJob{TenantID: "t1", ClockID: "clk_1"}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("worker never picked up the job")
	}

	start := time.Now()
	if w.Stop(100 * time.Millisecond) {
		t.Fatal("Stop reported a clean stop while the job was still running")
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("Stop took %v, want ~100ms — the deadline was not honoured", took)
	}

	close(release)
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("abandoned job never finished after release")
	}
}
