package webhook

import (
	"context"
	"errors"
	"github.com/sagarsuperuser/velox/internal/platform/leader"
	"github.com/sagarsuperuser/velox/internal/platform/leader/leadertest"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// P5 webhook semantics locks (panel test gaps), real Postgres.

func seedEndpoint(t *testing.T, db *postgres.DB, ctx context.Context, tenantID string) domain.WebhookEndpoint {
	t.Helper()
	store := NewPostgresStore(db)
	ep, err := store.CreateEndpoint(ctx, tenantID, domain.WebhookEndpoint{
		URL: "https://example.test/hook", Events: []string{"*"}, Active: true, Secret: "whsec_test",
	})
	if err != nil {
		t.Fatalf("create endpoint: %v", err)
	}
	return ep
}

// TestP5_WebhookOutboxLeaseInvariant pins the webhook outbox's own
// constants relation (the verifier caught the old 25×10s > 30s default
// violating it): BatchSize×outboxPerRowBudget ≤ BatchTimeout <
// outboxClaimLease(BatchSize).
func TestP5_WebhookOutboxLeaseInvariant(t *testing.T) {
	d := NewDispatcher(nil, nil, DispatcherConfig{}, leader.AlwaysLead{})
	cfg := d.Config()
	if got := time.Duration(cfg.BatchSize) * outboxPerRowBudget; got > cfg.BatchTimeout {
		t.Errorf("BatchSize×outboxPerRowBudget (%v) > BatchTimeout (%v)", got, cfg.BatchTimeout)
	}
	if lease := outboxClaimLease(cfg.BatchSize); lease <= cfg.BatchTimeout {
		t.Errorf("outboxClaimLease (%v) ≤ BatchTimeout (%v) — live batch re-claimable under itself", lease, cfg.BatchTimeout)
	}
	// Retry-side relation: the claim lease must cover the claim batch.
	if lease := retryClaimLease(10); lease <= 10*perRetryRowBudget {
		t.Errorf("retryClaimLease(10) (%v) has no margin over 10×perRetryRowBudget (%v)", lease, 10*perRetryRowBudget)
	}
}

// TestP5_EventAndDeliveriesOneTx_BornLeased: Dispatch's fan-out rows
// commit WITH the event and are born leased — no window where an event
// exists that no delivery row references, no NULL next_retry_at, and
// the retry worker does not steal the row from the in-process
// goroutine before the birth lease expires.
func TestP5_EventAndDeliveriesOneTx_BornLeased(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := leadertest.Token(t, testutil.AdminPool(t), postgres.WithLivemode(context.Background(), false), leader.RoleWebhookOutbox, leader.RoleWebhookDelivery)
	tenantID := testutil.CreateTestTenant(t, db, "P5 Born Leased")
	store := NewPostgresStore(db)
	ep := seedEndpoint(t, db, ctx, tenantID)

	before := time.Now().UTC()
	event, deliveries, err := store.CreateEventWithDeliveries(ctx, tenantID, domain.WebhookEvent{
		EventType: "invoice.paid", Payload: map[string]any{"invoice_id": "inv_1"},
	}, []string{ep.ID}, birthLeaseWindow, "")
	if err != nil {
		t.Fatalf("create event with deliveries: %v", err)
	}
	if len(deliveries) != 1 {
		t.Fatalf("deliveries: got %d, want 1", len(deliveries))
	}
	d := deliveries[0]
	if d.NextRetryAt == nil {
		t.Fatal("delivery born with NULL next_retry_at — the double-POST race the design closes")
	}
	if lease := d.NextRetryAt.Sub(before); lease < birthLeaseWindow-5*time.Second || lease > birthLeaseWindow+5*time.Second {
		t.Errorf("birth lease: %v, want ≈%v", lease, birthLeaseWindow)
	}
	if event.ID != d.WebhookEventID {
		t.Errorf("delivery references %s, want %s", d.WebhookEventID, event.ID)
	}

	// The retry worker must NOT claim a born-leased row now (the
	// in-process goroutine owns the first attempt).
	claimed, err := store.ListPendingDeliveries(ctx, 10)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	for _, c := range claimed {
		if c.ID == d.ID {
			t.Error("retry worker claimed a born-leased row inside its birth lease")
		}
	}
}

// TestP5_UpdateDeliveryCAS_StaleMarkDropped: worker A's lease expires,
// worker B re-claims and SUCCEEDS; A's late failure-mark must not flip
// the row back to pending (that third-POSTs a delivered webhook).
//
// Mutation-verify: drop the AND status='pending' guard — this fails.
func TestP5_UpdateDeliveryCAS_StaleMarkDropped(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "P5 CAS Marks")
	store := NewPostgresStore(db)
	ep := seedEndpoint(t, db, ctx, tenantID)

	_, deliveries, err := store.CreateEventWithDeliveries(ctx, tenantID, domain.WebhookEvent{
		EventType: "invoice.paid", Payload: map[string]any{},
	}, []string{ep.ID}, 0, "") // lease 0: immediately claimable
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	d := deliveries[0]

	// Worker B resolves the row.
	now := time.Now().UTC()
	success := d
	success.AttemptCount = 1
	success.Status = domain.DeliverySucceeded
	success.CompletedAt = &now
	success.NextRetryAt = nil
	if _, err := store.UpdateDelivery(ctx, tenantID, success, 0); err != nil {
		t.Fatalf("success mark: %v", err)
	}

	// Worker A's stale failure-mark (old pending snapshot) arrives late.
	later := now.Add(time.Minute)
	stale := d
	stale.AttemptCount = 1
	stale.Status = domain.DeliveryPending
	stale.NextRetryAt = &later
	stale.ErrorMessage = "HTTP 500 (stale)"
	if _, err := store.UpdateDelivery(ctx, tenantID, stale, 0); !errors.Is(err, ErrStaleDeliveryMark) {
		t.Fatalf("stale mark: err=%v, want ErrStaleDeliveryMark", err)
	}

	all, err := store.ListDeliveries(ctx, tenantID, d.WebhookEventID)
	if err != nil {
		t.Fatalf("list deliveries: %v", err)
	}
	if len(all) != 1 || all[0].Status != domain.DeliverySucceeded {
		t.Fatalf("delivery after stale mark: %+v, want one succeeded row (stale writer flipped a resolved row)", all)
	}
}

// TestP5_HandlerOwnsOutboxMark: DispatchFromOutbox marks the producing
// webhook_outbox row dispatched INSIDE the event tx — a replayed
// handler cannot mint a duplicate event (the old separate mark's crash
// window did exactly that, with a fresh undedupable event id).
func TestP5_HandlerOwnsOutboxMark(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := leadertest.Token(t, testutil.AdminPool(t), postgres.WithLivemode(context.Background(), false), leader.RoleWebhookOutbox, leader.RoleWebhookDelivery)
	tenantID := testutil.CreateTestTenant(t, db, "P5 Handler Owns Mark")
	store := NewPostgresStore(db)
	outbox := NewOutboxStore(db)
	svc := NewTestService(store, nil) // sync deliveries; nil client = no real POSTs
	_ = seedEndpoint(t, db, ctx, tenantID)

	rowID, err := outbox.EnqueueStandalone(ctx, tenantID, "invoice.paid", map[string]any{"invoice_id": "inv_1"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	if err := svc.DispatchFromOutbox(ctx, rowID, tenantID, "invoice.paid", map[string]any{"invoice_id": "inv_1"}); err != nil {
		t.Fatalf("dispatch from outbox: %v", err)
	}

	// The outbox row is dispatched — atomically with the event.
	var status string
	btx, _ := db.BeginTx(ctx, postgres.TxBypass, "")
	defer postgres.Rollback(btx)
	if err := btx.QueryRow(`SELECT status FROM webhook_outbox WHERE id = $1`, rowID).Scan(&status); err != nil {
		t.Fatalf("read outbox row: %v", err)
	}
	if status != "dispatched" {
		t.Fatalf("outbox row status: %q, want dispatched (marked in the event tx)", status)
	}

	// A replayed ProcessBatch tick finds nothing to re-dispatch — the
	// crash window that minted duplicate events is structurally gone.
	events, err := store.ListEvents(ctx, tenantID, 50)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events after dispatch: got %d, want exactly 1", len(events))
	}
	n, err := outbox.ProcessBatch(ctx, 10, func(_ context.Context, row OutboxRow) error {
		t.Errorf("replayed tick re-claimed dispatched row %s", row.ID)
		return nil
	})
	if err != nil {
		t.Fatalf("replay tick: %v", err)
	}
	if n != 0 {
		t.Fatalf("replay tick attempted %d rows, want 0", n)
	}
}
