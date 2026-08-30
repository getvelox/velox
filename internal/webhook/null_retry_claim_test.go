package webhook_test

import (
	"context"
	"github.com/sagarsuperuser/velox/internal/platform/leader"
	"github.com/sagarsuperuser/velox/internal/platform/leader/leadertest"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/webhook"
)

// TestListPendingDeliveries_ClaimsNullRetryOrphan covers the liveness fix: a
// freshly-created 'pending' delivery has next_retry_at NULL (it's set only
// after the first attempt's HTTP outcome). If the first-attempt goroutine
// dies before that write (deploy, crash), the row must still be re-claimable.
// Pre-fix the claim predicate was `next_retry_at <= NOW()`, and NULL <= NOW()
// is never true, so the orphan stranded forever.
func TestListPendingDeliveries_ClaimsNullRetryOrphan(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(leadertest.Token(t, testutil.AdminPool(t), postgres.WithLivemode(context.Background(), false), leader.RoleWebhookDelivery), 15*time.Second)
	defer cancel()

	tenantID := testutil.CreateTestTenant(t, db, "Webhook NullRetry Orphan")
	store := webhook.NewPostgresStore(db)

	ep, err := store.CreateEndpoint(ctx, tenantID, domain.WebhookEndpoint{
		URL: "https://example.test/hook", Events: []string{"invoice.finalized"}, Active: true, Secret: "whsec_test",
	})
	if err != nil {
		t.Fatalf("create endpoint: %v", err)
	}
	evt, err := store.CreateEvent(ctx, tenantID, domain.WebhookEvent{
		EventType: "invoice.finalized", Payload: map[string]any{"id": "inv_1"},
	})
	if err != nil {
		t.Fatalf("create event: %v", err)
	}
	del, err := store.CreateDelivery(ctx, tenantID, domain.WebhookDelivery{
		WebhookEndpointID: ep.ID, WebhookEventID: evt.ID, Status: domain.DeliveryPending,
	})
	if err != nil {
		t.Fatalf("create delivery: %v", err)
	}

	// Deliberately do NOT call setDuePast — leave next_retry_at NULL, the
	// exact strand condition the fix addresses.
	got, err := store.ListPendingDeliveries(ctx, 100)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !containsDelivery(got, del.ID) {
		t.Fatalf("NULL-retry pending delivery not claimed — stranded forever; got %d rows", len(got))
	}
}
