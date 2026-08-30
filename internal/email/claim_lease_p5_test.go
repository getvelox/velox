package email_test

import (
	"context"
	"errors"
	"fmt"
	"github.com/sagarsuperuser/velox/internal/platform/leader"
	"github.com/sagarsuperuser/velox/internal/platform/leader/leadertest"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/email"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// P5 lease-model locks (panel test gaps).

// TestP5_LeaseInvariantChain pins the constants relation the whole
// design rests on: BatchSize×PerRowBudget ≤ BatchTimeout < ClaimLease.
// If someone hand-tunes one constant, this fails before production
// double-sends do.
func TestP5_LeaseInvariantChain(t *testing.T) {
	d := email.NewDispatcher(nil, nil, email.DispatcherConfig{}, leader.AlwaysLead{})
	cfg := d.Config()
	if got := time.Duration(cfg.BatchSize) * email.PerRowBudget; got > cfg.BatchTimeout {
		t.Errorf("BatchSize×PerRowBudget (%v) > BatchTimeout (%v) — rows can be cancelled mid-send", got, cfg.BatchTimeout)
	}
	if lease := email.ClaimLease(cfg.BatchSize); lease <= cfg.BatchTimeout {
		t.Errorf("ClaimLease (%v) ≤ BatchTimeout (%v) — a live batch can be re-claimed under itself: double-send", lease, cfg.BatchTimeout)
	}
}

// TestP5_MarkSurvivesBatchCtxDeath is THE mark-loss lock: the batch ctx
// dies immediately after the handler succeeds (the old shape lost the
// mark in the whole-batch tx → delivered invoice re-sent at the next
// tick). Marks run detached, so the dispatched mark must still commit.
//
// Mutation-verify: mark on the batch ctx instead of WithoutCancel — the
// dispatched assertion fails.
func TestP5_MarkSurvivesBatchCtxDeath(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := leadertest.Token(t, testutil.AdminPool(t), postgres.WithLivemode(context.Background(), false), leader.RoleEmailOutbox)
	tenantID := testutil.CreateTestTenant(t, db, "P5 Mark Detach")
	store := email.NewOutboxStore(db)

	id, err := store.EnqueueStandalone(ctx, tenantID, email.TypePaymentReceipt, map[string]any{"to": "a@b.test"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	batchCtx, cancel := context.WithCancel(ctx)
	n, err := store.ProcessBatch(batchCtx, 5, func(_ context.Context, row email.OutboxRow) error {
		// The send "succeeded"; the batch deadline fires before the mark.
		cancel()
		return nil
	})
	if err != nil {
		t.Fatalf("process batch: %v", err)
	}
	if n != 1 {
		t.Fatalf("attempted: got %d, want 1", n)
	}

	status, attempts := emailRowState(t, db, id)
	if status != "dispatched" {
		t.Fatalf("row status after ctx death: %q, want dispatched (mark lost — the delivered email will be re-sent)", status)
	}
	if attempts != 1 {
		t.Errorf("attempts: got %d, want 1 (claim is the single increment site)", attempts)
	}
}

// TestP5_PermanentErrorsDLQImmediately: suppressed recipients and
// undecodable payloads can never succeed — riding the ~72h ramp on them
// wastes 15 cycles and pounds dead inboxes.
func TestP5_PermanentErrorsDLQImmediately(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := leadertest.Token(t, testutil.AdminPool(t), postgres.WithLivemode(context.Background(), false), leader.RoleEmailOutbox)
	tenantID := testutil.CreateTestTenant(t, db, "P5 Permanent DLQ")
	store := email.NewOutboxStore(db)

	id, err := store.EnqueueStandalone(ctx, tenantID, email.TypePaymentReceipt, map[string]any{"to": "dead@b.test"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := store.ProcessBatch(ctx, 5, func(context.Context, email.OutboxRow) error {
		return fmt.Errorf("wrapped: %w", email.ErrRecipientSuppressed)
	}); err != nil {
		t.Fatalf("process batch: %v", err)
	}
	status, attempts := emailRowState(t, db, id)
	if status != "failed" || attempts != 1 {
		t.Fatalf("suppressed recipient: status=%q attempts=%d, want failed/1 (immediate DLQ)", status, attempts)
	}
}

// TestP5_PanicRowDoesNotStrandBatch: a poisoned row is recovered
// per-row — later rows in the same claimed batch still get attempted
// this tick, and the poison row records an attempt (advances to DLQ
// instead of cycling lease-long forever).
func TestP5_PanicRowDoesNotStrandBatch(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := leadertest.Token(t, testutil.AdminPool(t), postgres.WithLivemode(context.Background(), false), leader.RoleEmailOutbox)
	tenantID := testutil.CreateTestTenant(t, db, "P5 Panic Poison")
	store := email.NewOutboxStore(db)

	poisonID, err := store.EnqueueStandalone(ctx, tenantID, email.TypePaymentReceipt, map[string]any{"to": "poison@b.test"})
	if err != nil {
		t.Fatalf("enqueue poison: %v", err)
	}
	okID, err := store.EnqueueStandalone(ctx, tenantID, email.TypePaymentReceipt, map[string]any{"to": "fine@b.test"})
	if err != nil {
		t.Fatalf("enqueue ok: %v", err)
	}

	n, err := store.ProcessBatch(ctx, 5, func(_ context.Context, row email.OutboxRow) error {
		if row.ID == poisonID {
			panic("pathological payload")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("process batch: %v", err)
	}
	if n != 2 {
		t.Fatalf("attempted: got %d, want 2 (panic must not strand the batch)", n)
	}
	if status, _ := emailRowState(t, db, okID); status != "dispatched" {
		t.Errorf("row after the poison: %q, want dispatched", status)
	}
	// Handler panics classify as payload-decode (permanent) → DLQ now.
	if status, _ := emailRowState(t, db, poisonID); status != "failed" {
		t.Errorf("poison row: %q, want failed", status)
	}
}

// TestP5_ConcurrentClaimersDisjoint: two dispatchers racing the same
// due set attempt each row exactly once (claims are FOR UPDATE SKIP
// LOCKED + leased).
func TestP5_ConcurrentClaimersDisjoint(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := leadertest.Token(t, testutil.AdminPool(t), postgres.WithLivemode(context.Background(), false), leader.RoleEmailOutbox)
	tenantID := testutil.CreateTestTenant(t, db, "P5 Disjoint Claims")
	store := email.NewOutboxStore(db)

	for i := 0; i < 10; i++ {
		if _, err := store.EnqueueStandalone(ctx, tenantID, email.TypePaymentReceipt, map[string]any{"to": fmt.Sprintf("c%d@b.test", i)}); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}

	var mu sync.Mutex
	seen := map[string]int{}
	handler := func(_ context.Context, row email.OutboxRow) error {
		mu.Lock()
		seen[row.ID]++
		mu.Unlock()
		time.Sleep(50 * time.Millisecond) // widen the race window
		return nil
	}

	var wg sync.WaitGroup
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := store.ProcessBatch(ctx, 10, handler); err != nil {
				t.Errorf("process batch: %v", err)
			}
		}()
	}
	wg.Wait()

	for id, count := range seen {
		if count != 1 {
			t.Errorf("row %s attempted %d times, want exactly 1", id, count)
		}
	}
	if len(seen) != 10 {
		t.Errorf("attempted %d distinct rows, want 10", len(seen))
	}
}

// TestP5_StrictSTARTTLS: a server that doesn't advertise STARTTLS is a
// hard error in the default mode (the old smtp.SendMail path silently
// proceeded in plaintext — downgrade-strippable), and SMTP_TLS=none is
// rejected in production before any dial.
func TestP5_StrictSTARTTLS(t *testing.T) {
	// Minimal SMTP server that greets and advertises NO extensions.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				_, _ = c.Write([]byte("220 plain.test ESMTP\r\n"))
				buf := make([]byte, 512)
				for {
					n, err := c.Read(buf)
					if err != nil {
						return
					}
					cmd := strings.ToUpper(strings.TrimSpace(string(buf[:n])))
					switch {
					case strings.HasPrefix(cmd, "EHLO"):
						_, _ = c.Write([]byte("250-plain.test\r\n250 SIZE 35882577\r\n")) // no STARTTLS
					case strings.HasPrefix(cmd, "HELO"):
						_, _ = c.Write([]byte("250 plain.test\r\n"))
					case strings.HasPrefix(cmd, "QUIT"):
						_, _ = c.Write([]byte("221 bye\r\n"))
						return
					default:
						_, _ = c.Write([]byte("250 ok\r\n"))
					}
				}
			}(conn)
		}
	}()

	host, port, _ := net.SplitHostPort(ln.Addr().String())
	s := email.NewTestSender(host, port, "starttls", false)
	err = s.DeliverForTest(context.Background(), "x@y.test", []byte("Subject: t\r\n\r\nbody"))
	if err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Errorf("plaintext server in strict mode: err=%v, want STARTTLS refusal", err)
	}

	// Production forbids none-mode before any dial happens.
	sProd := email.NewTestSender("smtp.invalid", "587", "none", true)
	err = sProd.DeliverForTest(context.Background(), "x@y.test", []byte("Subject: t\r\n\r\nbody"))
	if err == nil || !strings.Contains(err.Error(), "forbidden in production") {
		t.Errorf("none-mode in prod: err=%v, want forbidden", err)
	}
}

func emailRowState(t *testing.T, db *postgres.DB, id string) (status string, attempts int) {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if err := tx.QueryRow(`SELECT status, attempts FROM email_outbox WHERE id = $1`, id).Scan(&status, &attempts); err != nil {
		t.Fatalf("read row: %v", err)
	}
	return status, attempts
}

var _ = errors.Is // keep errors import if assertions change
