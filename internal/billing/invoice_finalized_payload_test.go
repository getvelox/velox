package billing

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// invoice.finalized for an invoice born finalized carries that invoice
// (ADR-131), the same data invoice.Service.Finalize sends for a draft. Credit
// is applied when the invoice is collected, after this event, so its
// amount_due_cents is the amount before that credit; credits_applied_cents is
// what the invoice already carries, so the two add up (#1281).
func TestDispatchInvoiceFinalized_PayloadCarriesCreditsApplied(t *testing.T) {
	d := &capturingEventDispatcher{}
	e := NewEngine(nil, nil, nil, nil, nil, nil, nil, nil, billingTestClock())
	e.SetEventDispatcher(d)

	inv := domain.Invoice{
		ID:                  "vlx_inv_1",
		InvoiceNumber:       "INV-0001",
		CustomerID:          "cus_1",
		Status:              domain.InvoiceFinalized,
		PaymentStatus:       domain.PaymentPending,
		TotalAmountCents:    1000,
		AmountDueCents:      700,
		CreditsAppliedCents: 300,
		Currency:            "USD",
	}
	e.DispatchInvoiceFinalized(context.Background(), "t1", inv)

	if len(d.events) != 1 || d.events[0].eventType != domain.EventInvoiceFinalized {
		t.Fatalf("want one invoice.finalized event, got %+v", d.events)
	}
	data := d.events[0].payload
	if err := domain.CheckWebhookData(domain.EventInvoiceFinalized, data); err != nil {
		t.Fatal(err)
	}
	if want := domain.InvoiceWebhookData(inv, nil); !reflect.DeepEqual(data, want) {
		t.Errorf("data is not the invoice:\n got %v\nwant %v", data, want)
	}
	got := data[domain.WebhookResourceInvoice].(map[string]any)
	for k, want := range map[string]json.Number{"credits_applied_cents": "300", "amount_due_cents": "700", "total_amount_cents": "1000"} {
		if got[k] != want {
			t.Errorf("%s: got %#v, want %s", k, got[k], want)
		}
	}
}

// credits_applied_cents is sent when it is zero too, which is the usual
// value: an engine invoice is sent before its first collection applies any
// credit. A receiver can then rely on the key being there.
func TestDispatchInvoiceFinalized_ZeroCreditsAppliedIsSent(t *testing.T) {
	d := &capturingEventDispatcher{}
	e := NewEngine(nil, nil, nil, nil, nil, nil, nil, nil, billingTestClock())
	e.SetEventDispatcher(d)

	e.DispatchInvoiceFinalized(context.Background(), "t1", domain.Invoice{
		ID: "vlx_inv_2", Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending,
		TotalAmountCents: 500, AmountDueCents: 500, Currency: "USD",
	})

	if len(d.events) != 1 {
		t.Fatalf("want one event, got %d", len(d.events))
	}
	got := d.events[0].payload[domain.WebhookResourceInvoice].(map[string]any)
	v, ok := got["credits_applied_cents"]
	if !ok {
		t.Fatal("credits_applied_cents missing from the invoice when it is zero")
	}
	if v != json.Number("0") {
		t.Errorf("credits_applied_cents: got %#v, want 0", v)
	}
}
