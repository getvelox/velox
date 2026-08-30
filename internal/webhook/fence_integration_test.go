package webhook_test

import (
	"context"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/leader"
	"github.com/sagarsuperuser/velox/internal/platform/leader/leadertest"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/webhook"
)

// TestListPendingDeliveries_SupersededTokenClaimsNothing is the funnel-level
// half of ADR-114: a tick whose lease was taken over (the row's token moved
// on) must claim NOTHING from a claim funnel, silently — not error, not the
// due row. The lease primitive's own tests prove the token moves; this
// proves the funnel honours it. Mutation check: drop `AND leader_fence(...)`
// from ListPendingDeliveries → the stale tick claims the due delivery →
// this fails. The same predicate guards the other four funnels
// (internal/arch/leader_fence_test.go pins that each still carries it).
func TestListPendingDeliveries_SupersededTokenClaimsNothing(t *testing.T) {
	db := testutil.SetupTestDB(t)
	admin := testutil.AdminPool(t)
	base := postgres.WithLivemode(context.Background(), false)
	ctx, cancel := context.WithTimeout(leadertest.Token(t, admin, base, leader.RoleWebhookDelivery), 15*time.Second)
	defer cancel()

	tenantID := testutil.CreateTestTenant(t, db, "Webhook Fence")
	store := webhook.NewPostgresStore(db)
	ep, err := store.CreateEndpoint(ctx, tenantID, domain.WebhookEndpoint{
		URL: "https://example.test/hook", Events: []string{"invoice.finalized"}, Active: true, Secret: "whsec_test",
	})
	if err != nil {
		t.Fatalf("create endpoint: %v", err)
	}
	evt, err := store.CreateEvent(ctx, tenantID, domain.WebhookEvent{EventType: "invoice.finalized", Payload: map[string]any{"id": "inv_1"}})
	if err != nil {
		t.Fatalf("create event: %v", err)
	}
	del, err := store.CreateDelivery(ctx, tenantID, domain.WebhookDelivery{WebhookEndpointID: ep.ID, WebhookEventID: evt.ID, Status: domain.DeliveryPending})
	if err != nil {
		t.Fatalf("create delivery: %v", err)
	}
	setDuePast(t, db, del.ID)

	// Takeover: another replica acquired the role — the row's token moved on.
	if _, err := admin.ExecContext(ctx, `UPDATE leader_leases SET holder_token = holder_token + 1 WHERE role = $1`, string(leader.RoleWebhookDelivery)); err != nil {
		t.Fatalf("simulate takeover: %v", err)
	}

	stale, err := store.ListPendingDeliveries(ctx, 100)
	if err != nil {
		t.Fatalf("superseded tick must not error, got %v", err)
	}
	if len(stale) != 0 {
		t.Fatalf("superseded tick claimed %d row(s); the fence must make it claim nothing", len(stale))
	}

	// The successor's tick (fresh token) claims the row the stale one could not.
	fresh := leadertest.Token(t, admin, base, leader.RoleWebhookDelivery)
	got, err := store.ListPendingDeliveries(fresh, 100)
	if err != nil {
		t.Fatalf("successor claim: %v", err)
	}
	if !containsDelivery(got, del.ID) {
		t.Fatalf("successor should claim the due delivery; got %d rows", len(got))
	}
}
