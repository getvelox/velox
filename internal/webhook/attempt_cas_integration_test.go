package webhook

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/leader"
	"github.com/sagarsuperuser/velox/internal/platform/leader/leadertest"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// A delivery's attempt_count is the retry budget: maxRetries is enforced
// against it, so an attempt that goes unrecorded buys the endpoint a free
// extra POST. Before the count CAS the write was a read-modify-write — two
// attempters that both read N both wrote N+1 — reachable when a first attempt
// outlives its born-lease and the retry worker claims the row underneath it.
func TestUpdateDelivery_AttemptCountCAS(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test — requires postgres")
	}
	db := testutil.SetupTestDB(t)
	admin := testutil.AdminPool(t)
	base := postgres.WithLivemode(context.Background(), false)
	ctx := leadertest.Token(t, admin, base, leader.RoleWebhookDelivery)
	tenantID := testutil.CreateTestTenant(t, db, "Attempt CAS")
	store := NewPostgresStore(db)

	ep, err := store.CreateEndpoint(ctx, tenantID, domain.WebhookEndpoint{
		URL: "https://example.test/hook", Events: []string{"invoice.paid"}, Active: true, Secret: "whsec_test",
	})
	if err != nil {
		t.Fatalf("create endpoint: %v", err)
	}
	mk := func(name string) domain.WebhookDelivery {
		t.Helper()
		_, ds, err := store.CreateEventWithDeliveries(ctx, tenantID, domain.WebhookEvent{
			EventType: "invoice.paid", Payload: map[string]any{"n": name},
		}, []string{ep.ID}, 0, "")
		if err != nil {
			t.Fatalf("create delivery %s: %v", name, err)
		}
		return ds[0]
	}

	t.Run("one of two attempters recording the same attempt wins", func(t *testing.T) {
		d := mk("race")
		const racers = 8
		var wg sync.WaitGroup
		wins := make(chan int, racers)
		for i := 0; i < racers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				mine := d
				mine.AttemptCount = d.AttemptCount + 1 // every racer read the same count
				mine.Status = domain.DeliveryPending
				next := time.Now().UTC().Add(time.Minute)
				mine.NextRetryAt = &next
				mine.ErrorMessage = "HTTP 500"
				if _, err := store.UpdateDelivery(ctx, tenantID, mine, d.AttemptCount); err == nil {
					wins <- i
				} else if !errors.Is(err, ErrStaleDeliveryMark) {
					t.Errorf("racer %d: unexpected error %v", i, err)
				}
			}(i)
		}
		wg.Wait()
		close(wins)
		n := 0
		for range wins {
			n++
		}
		if n != 1 {
			t.Fatalf("%d attempters recorded the same attempt; want exactly 1 (the rest must be refused)", n)
		}
		got, err := store.GetDelivery(ctx, tenantID, d.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.AttemptCount != d.AttemptCount+1 {
			t.Fatalf("attempt_count = %d, want %d — a lost race must not double-count or under-count", got.AttemptCount, d.AttemptCount+1)
		}
	})

	t.Run("a stale attempter cannot rewind the count", func(t *testing.T) {
		d := mk("stale")
		bumped := d
		bumped.AttemptCount = d.AttemptCount + 1
		next := time.Now().UTC().Add(time.Minute)
		bumped.NextRetryAt = &next
		if _, err := store.UpdateDelivery(ctx, tenantID, bumped, d.AttemptCount); err != nil {
			t.Fatalf("first attempt: %v", err)
		}
		// The loser of that race believes it wrote the attempt and reports its
		// own outcome from the pre-race snapshot: refused, nothing written.
		stale := d
		stale.AttemptCount = d.AttemptCount + 1
		stale.Status = domain.DeliverySucceeded
		if _, err := store.UpdateDelivery(ctx, tenantID, stale, d.AttemptCount); !errors.Is(err, ErrStaleDeliveryMark) {
			t.Fatalf("stale outcome: err=%v, want ErrStaleDeliveryMark", err)
		}
		got, err := store.GetDelivery(ctx, tenantID, d.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != domain.DeliveryPending || got.AttemptCount != d.AttemptCount+1 {
			t.Fatalf("stale write changed the row: status=%q attempts=%d", got.Status, got.AttemptCount)
		}
	})
}

// TestScheduleRetryOrFail_StaleAttempterIsDropped is the service-level twin of
// the store CAS, on the in-memory store: two attempters that planned from the
// same snapshot must leave ONE recorded attempt, not two. It exists so the
// memStore's mirrored predicate is load-bearing rather than decorative —
// remove the guard from the fake and this fails.
func TestScheduleRetryOrFail_StaleAttempterIsDropped(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, nil)
	ctx := context.Background()

	d := domain.WebhookDelivery{
		ID: "whd_stale", TenantID: "t1", WebhookEventID: "whe_1", WebhookEndpointID: "whe_ep",
		Status: domain.DeliveryPending, AttemptCount: 0,
	}
	store.deliveries = append(store.deliveries, d)

	// Both attempters hold the same pre-attempt snapshot (count 0).
	svc.scheduleRetryOrFail(ctx, d.TenantID, d, "HTTP 500 (attempter A)")
	svc.scheduleRetryOrFail(ctx, d.TenantID, d, "HTTP 500 (attempter B)")

	var got domain.WebhookDelivery
	for _, x := range store.deliveries {
		if x.ID == d.ID {
			got = x
		}
	}
	if got.AttemptCount != 1 {
		t.Fatalf("attempt_count = %d, want 1 — the second attempter planned from a stale snapshot and must be dropped", got.AttemptCount)
	}
	if got.ErrorMessage != "HTTP 500 (attempter A)" {
		t.Fatalf("error_message = %q, want attempter A's — the stale write must not overwrite the recorded outcome", got.ErrorMessage)
	}
}
