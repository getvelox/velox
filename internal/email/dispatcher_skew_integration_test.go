package email

import (
	"context"
	"github.com/sagarsuperuser/velox/internal/platform/leader"
	"github.com/sagarsuperuser/velox/internal/platform/leader/leadertest"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

func skewRowState(t *testing.T, db *postgres.DB, id string) (status string, attempts int, lastErr string, dueInFuture bool) {
	t.Helper()
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	var due time.Time
	if err := tx.QueryRowContext(ctx,
		`SELECT status, attempts, COALESCE(last_error,''), next_attempt_at FROM email_outbox WHERE id = $1`, id,
	).Scan(&status, &attempts, &lastErr, &due); err != nil {
		t.Fatalf("row state: %v", err)
	}
	return status, attempts, lastErr, due.After(time.Now())
}

func skewMakeDue(t *testing.T, db *postgres.DB, id string) {
	t.Helper()
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(ctx, `UPDATE email_outbox SET next_attempt_at = now() - interval '1 second' WHERE id = $1`, id); err != nil {
		t.Fatalf("make due: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// TestEmailOutbox_UnknownType_StaysPendingThenNewerHandlerDelivers runs the
// rolling-deploy skew end to end on the real store: an OLD binary (this
// dispatcher, which has no case for the type) claims the row and must leave
// it PENDING with a future retry — not DLQ'd — and a NEWER binary
// (simulated by a handler that knows the type) then delivers it.
//
// Mutation-verify: add ErrUnknownEmailType to IsPermanentSendError's
// permanent list — the row goes 'failed' on the first claim and this fails.
func TestEmailOutbox_UnknownType_StaysPendingThenNewerHandlerDelivers(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := leadertest.Token(t, testutil.AdminPool(t), postgres.WithLivemode(context.Background(), false), leader.RoleEmailOutbox)
	tenantID := testutil.CreateTestTenant(t, db, "Skew Pending")
	store := NewOutboxStore(db)

	id, err := store.EnqueueStandalone(ctx, tenantID, "future_type_from_a_newer_release", map[string]any{"to": "c@example.com"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	old := NewDispatcher(store, &recordingDeliverer{}, DispatcherConfig{}, leader.AlwaysLead{})
	n, err := store.ProcessBatch(ctx, 5, old.handle)
	if err != nil {
		t.Fatalf("old binary batch: %v", err)
	}
	if n != 1 {
		t.Fatalf("old binary attempted %d rows, want 1", n)
	}
	status, attempts, lastErr, future := skewRowState(t, db, id)
	if status != "pending" {
		t.Fatalf("after the old binary's claim: status %q, want pending (a DLQ here is the hazard-2 bug)", status)
	}
	if attempts != 1 || !future || !strings.Contains(lastErr, "unknown email_type") {
		t.Fatalf("after the old binary's claim: attempts=%d future=%v last_error=%q", attempts, future, lastErr)
	}

	// The newer replica claims it once it is due again.
	skewMakeDue(t, db, id)
	n, err = store.ProcessBatch(ctx, 5, func(_ context.Context, row OutboxRow) error { return nil })
	if err != nil || n != 1 {
		t.Fatalf("newer binary batch: n=%d err=%v", n, err)
	}
	if status, _, _, _ = skewRowState(t, db, id); status != "dispatched" {
		t.Fatalf("after the newer binary's claim: status %q, want dispatched", status)
	}
}

// TestEmailOutbox_UnknownType_DLQAfterCap: a type NO binary knows still
// ends in the dead-letter queue — at the attempt cap, not on first sight.
func TestEmailOutbox_UnknownType_DLQAfterCap(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := leadertest.Token(t, testutil.AdminPool(t), postgres.WithLivemode(context.Background(), false), leader.RoleEmailOutbox)
	tenantID := testutil.CreateTestTenant(t, db, "Skew DLQ")
	store := NewOutboxStore(db)

	id, err := store.EnqueueStandalone(ctx, tenantID, "type_nobody_knows", map[string]any{"to": "c@example.com"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	old := NewDispatcher(store, &recordingDeliverer{}, DispatcherConfig{}, leader.AlwaysLead{})
	for i := 1; i <= MaxOutboxAttempts; i++ {
		skewMakeDue(t, db, id)
		if _, err := store.ProcessBatch(ctx, 5, old.handle); err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
		status, attempts, _, _ := skewRowState(t, db, id)
		if i < MaxOutboxAttempts && status != "pending" {
			t.Fatalf("attempt %d: status %q, want pending until the cap", i, status)
		}
		if attempts != i {
			t.Fatalf("attempt %d: attempts=%d", i, attempts)
		}
	}
	if status, attempts, _, _ := skewRowState(t, db, id); status != "failed" || attempts != MaxOutboxAttempts {
		t.Fatalf("after the cap: status %q attempts %d, want failed/%d", status, attempts, MaxOutboxAttempts)
	}
	skewMakeDue(t, db, id)
	if n, _ := store.ProcessBatch(ctx, 5, old.handle); n != 0 {
		t.Fatalf("a failed row must not be claimed again, attempted %d", n)
	}
}
