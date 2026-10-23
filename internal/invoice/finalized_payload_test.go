package invoice

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// Finalizing a draft sends invoice.finalized carrying the finalized invoice
// (ADR-131), the same data the billing engine sends for an invoice born
// finalized. Credit is normally applied when the invoice is collected, after
// this event. A draft built before credit moved to collection (ADR-116) can
// already carry credit, and the invoice in the event then says so, so its
// amount_due_cents and total_amount_cents add up (#1281).
func TestFinalize_InvoiceFinalizedPayloadCarriesCreditsApplied(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, nil, newMemNumberer())
	events := &captureEvents{}
	svc.SetEventDispatcher(events)
	ctx := context.Background()

	inv, err := svc.Create(ctx, "t1", CreateInput{
		CustomerID: "cus_1",
		LineItems:  []AddLineItemInput{{Description: "Consulting", Quantity: 1, UnitAmountCents: 10000}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	draft := store.invoices[inv.ID]
	draft.CreditsAppliedCents = 2500
	draft.AmountDueCents = draft.TotalAmountCents - 2500
	store.invoices[inv.ID] = draft

	final, err := svc.Finalize(ctx, "t1", inv.ID)
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}

	var data map[string]any
	for _, ev := range events.events {
		if ev.eventType == domain.EventInvoiceFinalized {
			data = ev.payload
		}
	}
	if data == nil {
		t.Fatalf("no invoice.finalized event; got %+v", events.events)
	}
	if err := domain.CheckWebhookData(domain.EventInvoiceFinalized, data); err != nil {
		t.Fatal(err)
	}
	if want := domain.InvoiceWebhookData(final, nil); !reflect.DeepEqual(data, want) {
		t.Errorf("data is not the finalized invoice:\n got %v\nwant %v", data, want)
	}
	got := data[domain.WebhookResourceInvoice].(map[string]any)
	if got["credits_applied_cents"] != json.Number("2500") {
		t.Errorf("credits_applied_cents: got %#v, want 2500", got["credits_applied_cents"])
	}
	if got["amount_due_cents"] != json.Number("7500") || got["total_amount_cents"] != json.Number("10000") {
		t.Errorf("amounts: due %v total %v, want 7500 and 10000", got["amount_due_cents"], got["total_amount_cents"])
	}
}
