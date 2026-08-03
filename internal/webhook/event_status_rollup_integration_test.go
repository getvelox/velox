package webhook_test

import (
	"context"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/webhook"
)

// TestEventDeliveryStatuses_RollsUpFromRealDeliveries pins the SSE snapshot's
// status source against the REAL store. It replaced an age heuristic
// ("<24h old ⇒ pending, else delivered") found live on the FLOW W walk
// (2026-08-03): every settled event younger than a day wore a "pending" chip
// all day, and a permanently-failed delivery would have read "delivered" once
// it aged past the ladder. The roll-up precedence is the contract:
// any pending → pending, else any failed → failed, else delivered; an event
// with no deliveries is ABSENT (the caller renders "no_endpoints").
func TestEventDeliveryStatuses_RollsUpFromRealDeliveries(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 15*time.Second)
	defer cancel()

	tenantID := testutil.CreateTestTenant(t, db, "Webhook Status Rollup")
	store := webhook.NewPostgresStore(db)

	ep, err := store.CreateEndpoint(ctx, tenantID, domain.WebhookEndpoint{
		URL: "https://example.test/hook", Events: []string{"*"}, Active: true, Secret: "whsec_test",
	})
	if err != nil {
		t.Fatalf("create endpoint: %v", err)
	}

	mkEvent := func(name string) string {
		t.Helper()
		evt, err := store.CreateEvent(ctx, tenantID, domain.WebhookEvent{
			EventType: "invoice.finalized", Payload: map[string]any{"fixture": name},
		})
		if err != nil {
			t.Fatalf("create event %s: %v", name, err)
		}
		return evt.ID
	}
	mkDelivery := func(eventID string, status domain.DeliveryStatus) {
		t.Helper()
		if _, err := store.CreateDelivery(ctx, tenantID, domain.WebhookDelivery{
			WebhookEndpointID: ep.ID, WebhookEventID: eventID, Status: status,
		}); err != nil {
			t.Fatalf("create delivery: %v", err)
		}
	}

	allOK := mkEvent("all-succeeded")
	mkDelivery(allOK, domain.DeliverySucceeded)
	mkDelivery(allOK, domain.DeliverySucceeded)

	inFlight := mkEvent("one-still-pending")
	mkDelivery(inFlight, domain.DeliverySucceeded)
	mkDelivery(inFlight, domain.DeliveryPending)

	partialFail := mkEvent("failed-beside-success")
	mkDelivery(partialFail, domain.DeliverySucceeded)
	mkDelivery(partialFail, domain.DeliveryFailed)

	// Pending outranks failed: work is still in flight, so the event is not
	// yet a terminal failure.
	pendingBeatsFailed := mkEvent("pending-beside-failed")
	mkDelivery(pendingBeatsFailed, domain.DeliveryFailed)
	mkDelivery(pendingBeatsFailed, domain.DeliveryPending)

	noDeliveries := mkEvent("no-endpoint-matched")

	got, err := store.EventDeliveryStatuses(ctx, tenantID,
		[]string{allOK, inFlight, partialFail, pendingBeatsFailed, noDeliveries})
	if err != nil {
		t.Fatalf("EventDeliveryStatuses: %v", err)
	}

	want := map[string]string{
		allOK:              "delivered",
		inFlight:           "pending",
		partialFail:        "failed",
		pendingBeatsFailed: "pending",
	}
	for id, exp := range want {
		if got[id] != exp {
			t.Errorf("event %s: status = %q, want %q", id, got[id], exp)
		}
	}
	if _, present := got[noDeliveries]; present {
		t.Errorf("event with no deliveries must be ABSENT from the map (caller renders no_endpoints); got %q", got[noDeliveries])
	}
}
