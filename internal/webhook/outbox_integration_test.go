package webhook_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/webhook"
)

// TestOutbox_EnqueueStandalone_Persists verifies the base durability
// guarantee: after EnqueueStandalone returns without error, a pending row
// exists in webhook_outbox. No dispatcher involvement.
func TestOutbox_EnqueueStandalone_Persists(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 15*time.Second)
	defer cancel()

	tenantID := testutil.CreateTestTenant(t, db, "Outbox Enqueue")
	store := webhook.NewOutboxStore(db)

	id, err := store.EnqueueStandalone(ctx, tenantID, "invoice.finalized", map[string]any{
		"invoice_id": "inv_1",
		"amount":     1234,
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty outbox id")
	}

	status, attempts, payload := readOutbox(t, db, id)
	if status != webhook.OutboxPending {
		t.Errorf("status: got %q, want %q", status, webhook.OutboxPending)
	}
	if attempts != 0 {
		t.Errorf("attempts: got %d, want 0", attempts)
	}
	if payload["invoice_id"] != "inv_1" {
		t.Errorf("payload missing invoice_id: %+v", payload)
	}
}

// TestOutbox_Enqueue_TxAtomicity verifies the core of the transactional
// outbox pattern: a row enqueued inside a tx that rolls back must NOT
// persist. This is what lets producers enqueue in the same tx as their
// state change with zero risk of an orphan event.
func TestOutbox_Enqueue_TxAtomicity(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 15*time.Second)
	defer cancel()

	tenantID := testutil.CreateTestTenant(t, db, "Outbox Tx")
	store := webhook.NewOutboxStore(db)

	// Rollback path — row must not exist after rollback.
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	rollbackID, err := store.Enqueue(ctx, tx, tenantID, "test.rollback", map[string]any{"k": "v"})
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("enqueue in rollback tx: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if exists := outboxExists(t, db, rollbackID); exists {
		t.Error("row persisted despite rollback — outbox breaks tx atomicity")
	}

	// Commit path — row must exist after commit.
	tx2, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin tx2: %v", err)
	}
	commitID, err := store.Enqueue(ctx, tx2, tenantID, "test.commit", map[string]any{"k": "v"})
	if err != nil {
		_ = tx2.Rollback()
		t.Fatalf("enqueue in commit tx: %v", err)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if exists := outboxExists(t, db, commitID); !exists {
		t.Error("row missing after commit — outbox dropped the insert")
	}
}

// TestOutbox_ProcessBatch_Success covers the happy path: handler returns nil,
// row transitions to 'dispatched', attempts=1, dispatched_at populated.
func TestOutbox_ProcessBatch_Success(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 15*time.Second)
	defer cancel()

	tenantID := testutil.CreateTestTenant(t, db, "Outbox Success")
	store := webhook.NewOutboxStore(db)

	id, err := store.EnqueueStandalone(ctx, tenantID, "evt.ok", map[string]any{"n": 1})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	var saw webhook.OutboxRow
	n, err := store.ProcessBatch(ctx, 10, func(_ context.Context, row webhook.OutboxRow) error {
		saw = row
		return nil
	})
	if err != nil {
		t.Fatalf("process batch: %v", err)
	}
	if n != 1 {
		t.Fatalf("processed: got %d, want 1", n)
	}
	if saw.ID != id || saw.TenantID != tenantID || saw.EventType != "evt.ok" {
		t.Errorf("handler got wrong row: %+v", saw)
	}

	status, attempts, _ := readOutbox(t, db, id)
	if status != webhook.OutboxDispatched {
		t.Errorf("status: got %q, want %q", status, webhook.OutboxDispatched)
	}
	if attempts != 1 {
		t.Errorf("attempts: got %d, want 1", attempts)
	}
}

// TestOutbox_ProcessBatch_RetryBackoff covers the retry path: a transient
// handler error increments attempts and pushes next_attempt_at into the
// future per outboxBackoff — so a subsequent immediate ProcessBatch call
// MUST NOT re-claim the row.
func TestOutbox_ProcessBatch_RetryBackoff(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 15*time.Second)
	defer cancel()

	tenantID := testutil.CreateTestTenant(t, db, "Outbox Retry")
	store := webhook.NewOutboxStore(db)

	id, err := store.EnqueueStandalone(ctx, tenantID, "evt.flaky", map[string]any{})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// First pass — handler fails.
	n, err := store.ProcessBatch(ctx, 10, func(_ context.Context, _ webhook.OutboxRow) error {
		return errors.New("boom")
	})
	if err != nil {
		t.Fatalf("process batch 1: %v", err)
	}
	if n != 1 {
		t.Fatalf("processed: got %d, want 1", n)
	}

	status, attempts, _ := readOutbox(t, db, id)
	if status != webhook.OutboxPending {
		t.Errorf("status: got %q, want %q (not yet DLQ)", status, webhook.OutboxPending)
	}
	if attempts != 1 {
		t.Errorf("attempts: got %d, want 1", attempts)
	}
	lastErr, nextAt := readOutboxRetry(t, db, id)
	if lastErr != "boom" {
		t.Errorf("last_error: got %q, want %q", lastErr, "boom")
	}
	if !nextAt.After(time.Now().UTC()) {
		t.Errorf("next_attempt_at should be in the future, got %v", nextAt)
	}

	// Second immediate pass — row is not yet due, so nothing processed.
	n2, err := store.ProcessBatch(ctx, 10, func(_ context.Context, _ webhook.OutboxRow) error {
		return errors.New("should not run")
	})
	if err != nil {
		t.Fatalf("process batch 2: %v", err)
	}
	if n2 != 0 {
		t.Errorf("processed on second pass: got %d, want 0 (next_attempt_at not due)", n2)
	}
}

// TestOutbox_ProcessBatch_DLQ verifies that after MaxOutboxAttempts failures
// the row becomes 'failed' and is no longer claimed by subsequent batches —
// exactly what the dead-letter-queue contract promises.
func TestOutbox_ProcessBatch_DLQ(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 30*time.Second)
	defer cancel()

	tenantID := testutil.CreateTestTenant(t, db, "Outbox DLQ")
	store := webhook.NewOutboxStore(db)

	id, err := store.EnqueueStandalone(ctx, tenantID, "evt.broken", map[string]any{})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// Drive attempts to the DLQ threshold by repeatedly forcing next_attempt_at
	// back to now and running a failing handler. This is the same transition
	// path the real dispatcher would take over ~72h, compressed into one test.
	for i := 1; i <= webhook.MaxOutboxAttempts; i++ {
		if err := resetDue(db, id); err != nil {
			t.Fatalf("attempt %d: reset due: %v", i, err)
		}
		n, err := store.ProcessBatch(ctx, 10, func(_ context.Context, _ webhook.OutboxRow) error {
			return fmt.Errorf("attempt %d failed", i)
		})
		if err != nil {
			t.Fatalf("attempt %d: process: %v", i, err)
		}
		if n != 1 {
			t.Fatalf("attempt %d: processed %d, want 1", i, n)
		}
	}

	status, attempts, _ := readOutbox(t, db, id)
	if status != webhook.OutboxFailed {
		t.Errorf("status: got %q, want %q after %d attempts", status, webhook.OutboxFailed, webhook.MaxOutboxAttempts)
	}
	if attempts != webhook.MaxOutboxAttempts {
		t.Errorf("attempts: got %d, want %d", attempts, webhook.MaxOutboxAttempts)
	}

	// DLQ rows are terminal — they should NOT be re-claimed even if made "due".
	if err := resetDue(db, id); err != nil {
		t.Fatalf("reset due on DLQ row: %v", err)
	}
	n, err := store.ProcessBatch(ctx, 10, func(_ context.Context, _ webhook.OutboxRow) error {
		t.Error("DLQ row was re-claimed — terminal status not respected")
		return nil
	})
	if err != nil {
		t.Fatalf("post-DLQ batch: %v", err)
	}
	if n != 0 {
		t.Errorf("post-DLQ: processed %d, want 0", n)
	}
}

// TestOutbox_Counts verifies PendingCount and FailedCount match reality —
// these feed operator dashboards, so accuracy is non-negotiable.
func TestOutbox_Counts(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 15*time.Second)
	defer cancel()

	tenantID := testutil.CreateTestTenant(t, db, "Outbox Counts")
	store := webhook.NewOutboxStore(db)

	for range 3 {
		if _, err := store.EnqueueStandalone(ctx, tenantID, "evt.x", map[string]any{}); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}

	pending, err := store.PendingCount(ctx)
	if err != nil {
		t.Fatalf("pending count: %v", err)
	}
	if pending != 3 {
		t.Errorf("pending: got %d, want 3", pending)
	}

	failed, err := store.FailedCount(ctx)
	if err != nil {
		t.Fatalf("failed count: %v", err)
	}
	if failed != 0 {
		t.Errorf("failed: got %d, want 0", failed)
	}
}

// TestOutbox_ProcessBatch_ConcurrentClaimersDisjoint is the webhook-outbox
// port of email's TestP5_ConcurrentClaimersDisjoint. The 2026-07-06 HA
// posture named all three drainers as pinned by concurrent-claimer tests,
// but only the email outbox and the webhook RETRY claim had one — this
// drainer's claim was held by convention. Two dispatchers racing the same
// due set must attempt each row exactly once: a dual-leader window (a
// failover releases the session advisory lock while the old leader's tick
// is still running) would otherwise mint the same webhook event twice
// under two different ids — a semantic duplicate consumers cannot dedupe.
//
// What this pins is the LEASE: the 50ms handler sleep keeps the winner's
// rows claimed-but-unmarked while the sibling claims, and the sibling must
// see nothing. In this harness the two claims serialize (the second
// BeginTx opens a fresh pool connection, which outlasts the ~1ms claim
// tx), so the lock clause is not exercised here — it is pinned
// deterministically by TestOutbox_ProcessBatch_SkipsRowsLockedBySibling.
//
// Mutation-verify: keep next_attempt_at unchanged in the claim UPDATE
// (drop the lease stamp) — the sibling re-claims every row, red 10/10.
// Deleting `FOR UPDATE SKIP LOCKED` stays green here (0/10 red): that is
// the sibling test's mutation, not this one's.
func TestOutbox_ProcessBatch_ConcurrentClaimersDisjoint(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 20*time.Second)
	defer cancel()

	tenantID := testutil.CreateTestTenant(t, db, "Outbox Disjoint Claims")
	store := webhook.NewOutboxStore(db)

	const n = 10
	for range n {
		if _, err := store.EnqueueStandalone(ctx, tenantID, "invoice.finalized", map[string]any{"id": "inv_disjoint"}); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}

	var mu sync.Mutex
	seen := map[string]int{}
	handler := func(_ context.Context, row webhook.OutboxRow) error {
		mu.Lock()
		seen[row.ID]++
		mu.Unlock()
		time.Sleep(50 * time.Millisecond) // keep the claimed rows leased-but-unmarked while the sibling claims
		return nil
	}

	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := store.ProcessBatch(ctx, n, handler); err != nil {
				t.Errorf("process batch: %v", err)
			}
		}()
	}
	wg.Wait()

	for id, count := range seen {
		if count != 1 {
			t.Errorf("row %s attempted %d times, want exactly 1 (double-dispatch)", id, count)
		}
	}
	if len(seen) != n {
		t.Errorf("attempted %d distinct rows, want %d (nothing stranded)", len(seen), n)
	}
}

// TestOutbox_ProcessBatch_SkipsRowsLockedBySibling pins the claim's
// FOR UPDATE SKIP LOCKED deterministically: a sibling replica holding row
// locks on half the due set (its own claim in flight) must neither block
// this claim nor hand it those rows — and once the sibling lets go, the
// next tick claims exactly what it held.
//
// Mutation-verify: delete `SKIP LOCKED` (the subselect blocks behind the
// holder until the 5s ctx expires → claim error) or the whole
// `FOR UPDATE SKIP LOCKED` line (the UPDATE itself blocks on the held
// rows → same) — red either way.
func TestOutbox_ProcessBatch_SkipsRowsLockedBySibling(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)

	tenantID := testutil.CreateTestTenant(t, db, "Outbox Skip Locked")
	store := webhook.NewOutboxStore(db)

	const half = 5
	held, free := map[string]bool{}, map[string]bool{}
	for range half {
		id, err := store.EnqueueStandalone(ctx, tenantID, "sibling.holds", map[string]any{})
		if err != nil {
			t.Fatalf("enqueue held: %v", err)
		}
		held[id] = true
		id, err = store.EnqueueStandalone(ctx, tenantID, "sibling.free", map[string]any{})
		if err != nil {
			t.Fatalf("enqueue free: %v", err)
		}
		free[id] = true
	}

	// The sibling's claim: row locks on half the due set, held open across
	// our claim (a separate pool connection, so a real lock conflict).
	holder, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	defer postgres.Rollback(holder)
	if _, err := holder.ExecContext(ctx,
		`SELECT id FROM webhook_outbox WHERE event_type = 'sibling.holds' FOR UPDATE`); err != nil {
		t.Fatalf("hold locks: %v", err)
	}

	// 5s, not shorter: ProcessBatch refuses to start a row with <3s left.
	claimCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	seen := map[string]int{}
	record := func(_ context.Context, row webhook.OutboxRow) error {
		seen[row.ID]++
		return nil
	}
	n, err := store.ProcessBatch(claimCtx, 2*half, record)
	if err != nil {
		t.Fatalf("claim beside a lock-holding sibling: %v (blocked instead of skipping)", err)
	}
	if n != half {
		t.Errorf("attempted %d rows beside the sibling, want %d (the unlocked half)", n, half)
	}
	for id := range seen {
		if held[id] {
			t.Errorf("row %s was claimed while the sibling held its lock (double-dispatch)", id)
		}
	}
	if len(seen) != half {
		t.Errorf("attempted %d distinct rows, want %d", len(seen), half)
	}

	// Sibling releases (its tx ends) → the next tick claims exactly its half.
	// Fresh 5s budget: ProcessBatch refuses to start a row with <3s left, and
	// the first claim + handler pass may have consumed part of claimCtx on a
	// slow CI database — a reused ctx would read as a SKIP LOCKED regression.
	postgres.Rollback(holder)
	releaseCtx, cancelRelease := context.WithTimeout(ctx, 5*time.Second)
	defer cancelRelease()
	seen = map[string]int{}
	n, err = store.ProcessBatch(releaseCtx, 2*half, record)
	if err != nil {
		t.Fatalf("claim after the sibling released: %v", err)
	}
	if n != half || len(seen) != half {
		t.Errorf("after release: attempted %d rows (%d distinct), want %d", n, len(seen), half)
	}
	for id := range seen {
		if !held[id] {
			t.Errorf("after release: re-attempted %s, which the first tick already dispatched", id)
		}
	}
}

// --- helpers ---

func readOutbox(t *testing.T, db *postgres.DB, id string) (status string, attempts int, payload map[string]any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 5*time.Second)
	defer cancel()
	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("read tx: %v", err)
	}
	defer postgres.Rollback(tx)

	var payloadJSON []byte
	err = tx.QueryRowContext(ctx,
		`SELECT status, attempts, payload FROM webhook_outbox WHERE id = $1`,
		id,
	).Scan(&status, &attempts, &payloadJSON)
	if err != nil {
		t.Fatalf("scan outbox row %s: %v", id, err)
	}
	if len(payloadJSON) > 0 {
		_ = json.Unmarshal(payloadJSON, &payload)
	}
	return status, attempts, payload
}

func readOutboxRetry(t *testing.T, db *postgres.DB, id string) (lastErr string, nextAt time.Time) {
	t.Helper()
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 5*time.Second)
	defer cancel()
	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("read retry tx: %v", err)
	}
	defer postgres.Rollback(tx)

	var nullErr sql.NullString
	err = tx.QueryRowContext(ctx,
		`SELECT COALESCE(last_error,''), next_attempt_at FROM webhook_outbox WHERE id = $1`,
		id,
	).Scan(&nullErr, &nextAt)
	if err != nil {
		t.Fatalf("scan retry row %s: %v", id, err)
	}
	return nullErr.String, nextAt
}

func outboxExists(t *testing.T, db *postgres.DB, id string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 5*time.Second)
	defer cancel()
	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("exists tx: %v", err)
	}
	defer postgres.Rollback(tx)

	var n int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM webhook_outbox WHERE id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("exists scan: %v", err)
	}
	return n > 0
}

// resetDue forces next_attempt_at back to now() so the next ProcessBatch
// tick claims the row immediately. Used by DLQ/retry tests that must
// exercise many attempts within a single test run without waiting out
// real backoff durations.
func resetDue(db *postgres.DB, id string) error {
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 5*time.Second)
	defer cancel()
	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(ctx,
		`UPDATE webhook_outbox SET next_attempt_at = now() WHERE id = $1`, id); err != nil {
		return err
	}
	return tx.Commit()
}
