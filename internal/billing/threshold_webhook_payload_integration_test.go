package billing_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/subscription"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/webhook"
)

// TestThresholdFire_WebhookCarriesTheSubscription: the outbox row a crossed
// spend threshold queues carries the subscription as the API returns it
// (ADR-131), and the crossing's own facts beside it.
func TestThresholdFire_WebhookCarriesTheSubscription(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	f := newThresholdFixture(t, "Threshold Webhook Payload")
	f.engine.SetEventDispatcher(webhook.NewOutboxDispatcher(webhook.NewOutboxStore(f.db)))
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 60*time.Second)
	defer cancel()

	f.ingestUsage(t, ctx, 100, 10)
	if _, err := f.subSvc.SetBillingThresholds(ctx, f.tenantID, f.subID, subscription.BillingThresholdsInput{AmountGTE: 500}); err != nil {
		t.Fatalf("set threshold: %v", err)
	}
	if fired, errs := f.engine.ScanThresholds(ctx, 50); fired != 1 || len(errs) != 0 {
		t.Fatalf("threshold scan: fired=%d errs=%v, want 1 and none", fired, errs)
	}
	invs := f.listInvoices(t, ctx)
	if len(invs) != 1 {
		t.Fatalf("invoices %+v, want one threshold invoice", invs)
	}

	var raw []byte
	if err := testutil.AdminPool(t).QueryRow(
		`SELECT payload FROM webhook_outbox WHERE tenant_id = $1 AND event_type = $2`,
		f.tenantID, domain.EventSubscriptionThresholdCrossed).Scan(&raw); err != nil {
		t.Fatalf("read the subscription.threshold_crossed outbox row: %v", err)
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	if err := domain.CheckWebhookData(domain.EventSubscriptionThresholdCrossed, data); err != nil {
		t.Fatal(err)
	}

	// The subscription as GET /v1/subscriptions/{id} returns it, without the
	// display string it renders per read.
	sub, err := f.subSvc.Get(ctx, f.tenantID, f.subID)
	if err != nil {
		t.Fatal(err)
	}
	sub.CurrentBillingPeriodDisplay = ""
	enc, _ := json.Marshal(sub)
	var want map[string]any
	if err := json.Unmarshal(enc, &want); err != nil {
		t.Fatal(err)
	}
	if got := data[domain.WebhookResourceSubscription]; !reflect.DeepEqual(got, want) {
		t.Errorf("data.subscription differs from the API's:\n got %v\nwant %v", got, want)
	}
	if data["invoice_id"] != invs[0].ID || data["running_subtotal"] != float64(1000) || data["crossed_amount"] != true {
		t.Errorf("threshold facts = invoice_id %v running_subtotal %v crossed_amount %v, want %s 1000 true",
			data["invoice_id"], data["running_subtotal"], data["crossed_amount"], invs[0].ID)
	}

	// The threshold invoice's invoice.finalized carries the invoice as the
	// store holds it after the fire, without the fields read per request.
	var finRaw []byte
	if err := testutil.AdminPool(t).QueryRow(
		`SELECT payload FROM webhook_outbox WHERE tenant_id = $1 AND event_type = $2`,
		f.tenantID, domain.EventInvoiceFinalized).Scan(&finRaw); err != nil {
		t.Fatalf("read the invoice.finalized outbox row: %v", err)
	}
	var fin map[string]any
	if err := json.Unmarshal(finRaw, &fin); err != nil {
		t.Fatal(err)
	}
	if err := domain.CheckWebhookData(domain.EventInvoiceFinalized, fin); err != nil {
		t.Fatal(err)
	}
	inv, err := f.invStore.Get(ctx, f.tenantID, invs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	enc, _ = json.Marshal(domain.InvoiceWebhookData(inv, nil))
	var wantInv map[string]any
	if err := json.Unmarshal(enc, &wantInv); err != nil {
		t.Fatal(err)
	}
	// The no-card notice the collection sends after the event stamps
	// no_pm_notified_at: the event comes before it.
	gotInv, _ := fin[domain.WebhookResourceInvoice].(map[string]any)
	stored, _ := wantInv[domain.WebhookResourceInvoice].(map[string]any)
	if _, inEvent := gotInv["no_pm_notified_at"]; inEvent || stored["no_pm_notified_at"] == nil {
		t.Errorf("no_pm_notified_at: event %v, stored %v; want it stamped only after the event", gotInv["no_pm_notified_at"], stored["no_pm_notified_at"])
	}
	delete(stored, "no_pm_notified_at")
	if got, want := any(gotInv), any(stored); !reflect.DeepEqual(got, want) {
		t.Errorf("invoice.finalized data.invoice differs from the stored invoice:\n got %v\nwant %v", got, want)
	}
}
