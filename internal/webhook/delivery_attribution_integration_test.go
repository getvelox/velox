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

// TestListDeliveries_NamesTheEndpoint_EvenAfterDeletion pins the operator
// contract behind the Delivery Timeline: every delivery row names its
// receiver in human terms (URL + the operator's own description), not just
// an opaque vlx_whe_ id — and keeps naming it AFTER the endpoint is deleted,
// because deliveries are history and the endpoints page never displays ids
// to cross-reference against.
//
// The second half is the load-bearing one: a frontend-side lookup against
// the live endpoints list would pass the first assertion and silently lose
// attribution exactly when the operator deletes a receiver — which is the
// moment they are most likely to be reading old deliveries to work out what
// that endpoint was.
func TestListDeliveries_NamesTheEndpoint_EvenAfterDeletion(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test — requires postgres")
	}
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 15*time.Second)
	defer cancel()

	tenantID := testutil.CreateTestTenant(t, db, "Delivery Attribution")
	store := webhook.NewPostgresStore(db)

	ep, err := store.CreateEndpoint(ctx, tenantID, domain.WebhookEndpoint{
		URL: "https://acme.test/hooks/velox", Description: "Acme prod receiver",
		Events: []string{"*"}, Active: true, Secret: "whsec_test",
	})
	if err != nil {
		t.Fatalf("create endpoint: %v", err)
	}

	evt, err := store.CreateEvent(ctx, tenantID, domain.WebhookEvent{
		EventType: "invoice.finalized", Payload: map[string]any{"fixture": "attribution"},
	})
	if err != nil {
		t.Fatalf("create event: %v", err)
	}
	if _, err := store.CreateDelivery(ctx, tenantID, domain.WebhookDelivery{
		WebhookEndpointID: ep.ID, WebhookEventID: evt.ID, Status: domain.DeliverySucceeded,
	}); err != nil {
		t.Fatalf("create delivery: %v", err)
	}

	assertNamed := func(when string) {
		t.Helper()
		got, err := store.ListDeliveries(ctx, tenantID, evt.ID)
		if err != nil {
			t.Fatalf("%s: ListDeliveries: %v", when, err)
		}
		if len(got) != 1 {
			t.Fatalf("%s: deliveries = %d, want 1", when, len(got))
		}
		if got[0].EndpointURL != "https://acme.test/hooks/velox" {
			t.Errorf("%s: EndpointURL = %q, want the receiver's URL — the timeline is back to showing operators an opaque id", when, got[0].EndpointURL)
		}
		if got[0].EndpointDescription != "Acme prod receiver" {
			t.Errorf("%s: EndpointDescription = %q, want the operator's own label", when, got[0].EndpointDescription)
		}
	}

	assertNamed("live endpoint")

	// Delete the endpoint (0168: soft delete — the row survives with
	// deleted_at). The delivery's attribution must NOT go blank: history
	// keeps naming the receiver it was sent to.
	if err := store.DeleteEndpoint(ctx, tenantID, ep.ID); err != nil {
		t.Fatalf("delete endpoint: %v", err)
	}
	assertNamed("after endpoint deletion")
}
