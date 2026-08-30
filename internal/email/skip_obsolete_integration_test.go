package email_test

import (
	"context"
	"github.com/sagarsuperuser/velox/internal/platform/leader"
	"github.com/sagarsuperuser/velox/internal/platform/leader/leadertest"
	"testing"

	"github.com/sagarsuperuser/velox/internal/email"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestProcessBatch_ObsoleteRowMarksSkipped locks the 0155 'skipped'
// terminal state: an ErrEmailObsolete from the handler (the dispatcher's
// staleness gate — invoice settled while the row sat queued) marks the row
// 'skipped', never 'failed' (nothing broke) and never re-claimed (the
// pending-only claim predicate excludes it).
func TestProcessBatch_ObsoleteRowMarksSkipped(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := leadertest.Token(t, testutil.AdminPool(t), postgres.WithLivemode(context.Background(), false), leader.RoleEmailOutbox)
	tenantID := testutil.CreateTestTenant(t, db, "Skip Obsolete")
	store := email.NewOutboxStore(db)

	id, err := store.EnqueueStandalone(ctx, tenantID, email.TypePaymentSetupRequest,
		map[string]any{"to": "a@b.test", "invoice_number": "NIM-1"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	n, err := store.ProcessBatch(ctx, 5, func(_ context.Context, _ email.OutboxRow) error {
		return email.ErrEmailObsolete
	})
	if err != nil {
		t.Fatalf("process batch: %v", err)
	}
	if n != 1 {
		t.Fatalf("attempted: got %d, want 1", n)
	}

	var status, lastErr string
	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if err := tx.QueryRow(
		`SELECT status, COALESCE(last_error,'') FROM email_outbox WHERE id = $1`, id,
	).Scan(&status, &lastErr); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if status != "skipped" {
		t.Fatalf("status: got %q, want skipped (obsolete mail must be neither failed nor delivered)", status)
	}
	if lastErr == "" {
		t.Error("skipped row must record WHY in last_error")
	}

	// A skipped row is terminal: another batch claims nothing.
	n2, err := store.ProcessBatch(ctx, 5, func(_ context.Context, _ email.OutboxRow) error {
		t.Fatal("skipped row was re-claimed")
		return nil
	})
	if err != nil {
		t.Fatalf("second batch: %v", err)
	}
	if n2 != 0 {
		t.Fatalf("second batch attempted %d rows, want 0", n2)
	}
}
