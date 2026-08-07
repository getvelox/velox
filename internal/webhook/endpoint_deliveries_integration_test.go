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

// TestListDeliveriesByEndpoint_ScopedAndHydrated pins the two contracts
// behind the endpoint drill-down against the REAL store: (1) the list is
// scoped to one receiver — another endpoint's deliveries to the same
// events never bleed in; (2) each row is hydrated with its event's type
// and replay pivot, because the surface lists deliveries ACROSS events
// and a row must say which business fact it carried without a second
// fetch.
func TestListDeliveriesByEndpoint_ScopedAndHydrated(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test — requires postgres")
	}
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 15*time.Second)
	defer cancel()

	tenantID := testutil.CreateTestTenant(t, db, "Endpoint Drilldown")
	store := webhook.NewPostgresStore(db)

	epA, err := store.CreateEndpoint(ctx, tenantID, domain.WebhookEndpoint{
		URL: "https://acme.test/hooks/a", Events: []string{"*"}, Active: true, Secret: "whsec_a",
	})
	if err != nil {
		t.Fatalf("create endpoint A: %v", err)
	}
	epB, err := store.CreateEndpoint(ctx, tenantID, domain.WebhookEndpoint{
		URL: "https://acme.test/hooks/b", Events: []string{"*"}, Active: true, Secret: "whsec_b",
	})
	if err != nil {
		t.Fatalf("create endpoint B: %v", err)
	}

	// Two events, each delivered to BOTH endpoints (the fan-out shape).
	evt1, err := store.CreateEvent(ctx, tenantID, domain.WebhookEvent{
		EventType: "invoice.finalized", Payload: map[string]any{"n": 1},
	})
	if err != nil {
		t.Fatalf("create event 1: %v", err)
	}
	evt2, err := store.CreateEvent(ctx, tenantID, domain.WebhookEvent{
		EventType: "payment.failed", Payload: map[string]any{"n": 2},
	})
	if err != nil {
		t.Fatalf("create event 2: %v", err)
	}
	for _, pair := range []struct {
		ep  domain.WebhookEndpoint
		ev  domain.WebhookEvent
		sts domain.DeliveryStatus
	}{
		{epA, evt1, domain.DeliverySucceeded},
		{epB, evt1, domain.DeliveryFailed},
		{epA, evt2, domain.DeliverySucceeded},
		{epB, evt2, domain.DeliverySucceeded},
	} {
		if _, err := store.CreateDelivery(ctx, tenantID, domain.WebhookDelivery{
			WebhookEndpointID: pair.ep.ID, WebhookEventID: pair.ev.ID, Status: pair.sts,
		}); err != nil {
			t.Fatalf("create delivery (%s→%s): %v", pair.ev.EventType, pair.ep.URL, err)
		}
	}

	// A replay clone of event 1, delivered to B only (the scoped-replay shape).
	clone, err := store.CreateReplayEvent(ctx, tenantID, evt1.ID)
	if err != nil {
		t.Fatalf("create replay event: %v", err)
	}
	if _, err := store.CreateDelivery(ctx, tenantID, domain.WebhookDelivery{
		WebhookEndpointID: epB.ID, WebhookEventID: clone.ID, Status: domain.DeliverySucceeded,
	}); err != nil {
		t.Fatalf("create replay delivery: %v", err)
	}

	got, err := store.ListDeliveriesByEndpoint(ctx, tenantID, epB.ID, 50)
	if err != nil {
		t.Fatalf("ListDeliveriesByEndpoint: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("deliveries for B = %d, want 3 (2 originals + 1 replay; A has 2 that must not bleed in)", len(got))
	}
	for _, d := range got {
		if d.WebhookEndpointID != epB.ID {
			t.Errorf("row %s belongs to endpoint %s — scope leak", d.ID, d.WebhookEndpointID)
		}
		if d.EventType == "" {
			t.Errorf("row %s has empty EventType — hydration JOIN broken", d.ID)
		}
	}
	// Newest-first: the replay delivery was created last.
	if got[0].WebhookEventID != clone.ID {
		t.Errorf("first row event = %s, want the replay clone %s (created_at DESC)", got[0].WebhookEventID, clone.ID)
	}
	if got[0].EventReplayOfID != evt1.ID {
		t.Errorf("replay row EventReplayOfID = %q, want %q — the drill-down can't badge replays", got[0].EventReplayOfID, evt1.ID)
	}

	// GetDelivery roundtrip + cross-tenant invisibility.
	d0, err := store.GetDelivery(ctx, tenantID, got[0].ID)
	if err != nil {
		t.Fatalf("GetDelivery: %v", err)
	}
	if d0.WebhookEndpointID != epB.ID || d0.WebhookEventID != clone.ID {
		t.Errorf("GetDelivery returned %+v, want endpoint %s / event %s", d0, epB.ID, clone.ID)
	}
	otherTenant := testutil.CreateTestTenant(t, db, "Drilldown Other Tenant")
	if _, err := store.GetDelivery(ctx, otherTenant, got[0].ID); err == nil {
		t.Error("GetDelivery under another tenant returned a row — RLS scope leak")
	}
}
