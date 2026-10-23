package invoice_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/tax"
	"github.com/sagarsuperuser/velox/internal/tenant"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// stampingTaxReverser stands in for Stripe Tax: every reversal succeeds.
type stampingTaxReverser struct{ calls int }

func (r *stampingTaxReverser) ReverseTax(_ context.Context, _ string, req tax.ReversalRequest) (*tax.ReversalResult, error) {
	r.calls++
	return &tax.ReversalResult{TransactionID: "taxrev_" + req.Reference}, nil
}

// rowAtDispatch records each event with the invoice row as the database
// held it at the moment the event was created.
type rowAtDispatch struct {
	store *invoice.PostgresStore
	got   map[string]map[string]any
	row   map[string]domain.Invoice
}

func (d *rowAtDispatch) Dispatch(ctx context.Context, tenantID, eventType string, payload map[string]any) error {
	inv, _ := payload[domain.WebhookResourceInvoice].(map[string]any)
	id, _ := inv["id"].(string)
	row, err := d.store.Get(ctx, tenantID, id)
	if err != nil {
		return err
	}
	d.got[eventType], d.row[eventType] = payload, row
	return nil
}

// TestVoid_WebhookShowsTheRowAtTheEvent: invoice.voided carries the invoice
// as the database held it when the event was created (ADR-131). Under a tax
// provider the void also reverses the tax and stamps tax_reversed_at; the
// event must not leave out a stamp the row already had.
func TestVoid_WebhookShowsTheRowAtTheEvent(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Void Webhook Tax")
	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{ExternalID: "cus_void_tax", DisplayName: "Void Tax"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	store := invoice.NewPostgresStore(db)
	svc := invoice.NewService(store, nil, tenant.NewSettingsStore(db))
	rev := &stampingTaxReverser{}
	svc.SetTaxReverser(rev)
	events := &rowAtDispatch{store: store, got: map[string]map[string]any{}, row: map[string]domain.Invoice{}}
	svc.SetEventDispatcher(events)

	inv, err := store.Create(ctx, tenantID, domain.Invoice{
		CustomerID: cust.ID, InvoiceNumber: "VLX-VOID-TAX-1",
		Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending,
		Currency: "USD", SubtotalCents: 10800, TotalAmountCents: 10800, AmountDueCents: 10800,
		BillingPeriodStart: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		BillingPeriodEnd:   time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("seed invoice: %v", err)
	}
	if err := store.SetTaxTransaction(ctx, tenantID, inv.ID, "tx_void_tax"); err != nil {
		t.Fatalf("set tax transaction: %v", err)
	}

	if _, err := svc.Void(ctx, tenantID, inv.ID); err != nil {
		t.Fatalf("void: %v", err)
	}
	final, err := store.Get(ctx, tenantID, inv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rev.calls != 1 || final.TaxReversedAt == nil {
		t.Fatalf("tax reversal: %d calls, tax_reversed_at %v; want 1 call and a stamp", rev.calls, final.TaxReversedAt)
	}
	payload, ok := events.got[domain.EventInvoiceVoided]
	if !ok {
		t.Fatal("no invoice.voided event")
	}
	asJSON := func(v any) map[string]any {
		enc, _ := json.Marshal(v)
		var m map[string]any
		_ = json.Unmarshal(enc, &m)
		return m
	}
	got := asJSON(payload)[domain.WebhookResourceInvoice]
	want := asJSON(domain.InvoiceWebhookData(events.row[domain.EventInvoiceVoided], nil))[domain.WebhookResourceInvoice]
	if !reflect.DeepEqual(got, want) {
		t.Errorf("invoice.voided data.invoice differs from the row when the event was created:\n got %v\nwant %v", got, want)
	}
}
