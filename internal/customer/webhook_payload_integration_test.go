package customer_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/webhook"
)

// TestEmailSuppression_WebhookCarriesTheCustomer: customer.email_bounced
// and customer.email_complained queue the customer as GET
// /v1/customers/{id} returns it (ADR-131), with the provider's reason
// beside it. A complaint on a customer that already complained changes
// nothing on the row, so its reason is only in the event.
func TestEmailSuppression_WebhookCarriesTheCustomer(t *testing.T) {
	db := testutil.SetupTestDB(t)
	tenantID := testutil.CreateTestTenant(t, db, "Customer Webhook Payload")
	ctx := auth.WithTenantID(postgres.WithLivemode(context.Background(), false), tenantID)

	store := customer.NewPostgresStore(db)
	svc := customer.NewService(store)
	svc.SetEventDispatcher(webhook.NewOutboxDispatcher(webhook.NewOutboxStore(db)))
	r := chi.NewRouter()
	r.Mount("/v1/customers", customer.NewHandler(svc).Routes())

	c, err := svc.Create(ctx, tenantID, customer.CreateInput{
		ExternalID: "wp-bounce", DisplayName: "Bounce Co", Email: "ap@bounce.test",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	apiCustomer := func() map[string]any {
		t.Helper()
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/customers/"+c.ID, nil).WithContext(ctx))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET customer: %d %s", rec.Code, rec.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	lastEvent := func(eventType string) map[string]any {
		t.Helper()
		var raw []byte
		if err := testutil.AdminPool(t).QueryRow(
			`SELECT payload FROM webhook_outbox WHERE tenant_id = $1 AND event_type = $2 ORDER BY created_at DESC, id DESC LIMIT 1`,
			tenantID, eventType).Scan(&raw); err != nil {
			t.Fatalf("read %s outbox row: %v", eventType, err)
		}
		var data map[string]any
		if err := json.Unmarshal(raw, &data); err != nil {
			t.Fatal(err)
		}
		if err := domain.CheckWebhookData(eventType, data); err != nil {
			t.Fatal(err)
		}
		return data
	}

	steps := []struct {
		eventType, reason string
		mark              func(ctx context.Context, tenantID, customerID, reason string) error
	}{
		{domain.EventCustomerEmailBounced, "mailbox does not exist", svc.MarkEmailBounced},
		{domain.EventCustomerEmailComplained, "marked as spam", svc.MarkEmailComplained},
		{domain.EventCustomerEmailComplained, "marked as spam again", svc.MarkEmailComplained},
	}
	for _, s := range steps {
		if err := s.mark(ctx, tenantID, c.ID, s.reason); err != nil {
			t.Fatalf("%s: %v", s.eventType, err)
		}
		data := lastEvent(s.eventType)
		if got, want := data[domain.WebhookResourceCustomer], apiCustomer(); !reflect.DeepEqual(got, any(want)) {
			t.Errorf("%s (%s): data.customer differs from the API's:\n got %v\nwant %v", s.eventType, s.reason, got, want)
		}
		if data["reason"] != s.reason {
			t.Errorf("%s: reason = %v, want %q", s.eventType, data["reason"], s.reason)
		}
	}
}
