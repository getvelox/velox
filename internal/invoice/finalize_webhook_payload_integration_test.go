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
	"github.com/sagarsuperuser/velox/internal/tenant"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// committingTaxCommitter stands in for billing.Engine.CommitTax under Stripe
// Tax: the commit records the tax transaction on the invoice row.
type committingTaxCommitter struct {
	store *invoice.PostgresStore
	calls int
}

func (c *committingTaxCommitter) CommitTax(ctx context.Context, tenantID, invoiceID, calculationID string) error {
	c.calls++
	return c.store.SetTaxTransaction(ctx, tenantID, invoiceID, "tx_"+calculationID)
}

func (c *committingTaxCommitter) CheckTaxCalculationUsable(context.Context, string, string, string) error {
	return nil
}

// TestFinalize_WebhookShowsTheRowAtTheEvent: invoice.finalized sent by
// Service.Finalize carries the invoice as the database held it when the
// event was created (ADR-131). Finalize writes the row twice after the
// finalize itself: the hosted-page token (which moves updated_at) and, under
// Stripe Tax, the tax transaction. The event must show both writes, never the
// token with the updated_at from before it.
func TestFinalize_WebhookShowsTheRowAtTheEvent(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Finalize Webhook Tax")
	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{ExternalID: "cus_fin_tax", DisplayName: "Finalize Tax"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	store := invoice.NewPostgresStore(db)
	svc := invoice.NewService(store, nil, tenant.NewSettingsStore(db))
	committer := &committingTaxCommitter{store: store}
	svc.SetTaxCommitter(committer)
	events := &rowAtDispatch{store: store, got: map[string]map[string]any{}, row: map[string]domain.Invoice{}}
	svc.SetEventDispatcher(events)

	inv, err := store.Create(ctx, tenantID, domain.Invoice{
		CustomerID: cust.ID, InvoiceNumber: "VLX-FIN-TAX-1",
		Status: domain.InvoiceDraft, PaymentStatus: domain.PaymentPending,
		Currency: "USD", SubtotalCents: 10000, TotalAmountCents: 10800, AmountDueCents: 10800,
		TaxFacts:           domain.TaxFacts{TaxAmountCents: 800, TaxProvider: "stripe_tax", TaxCalculationID: "taxcalc_fin_1"},
		BillingPeriodStart: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		BillingPeriodEnd:   time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("seed draft: %v", err)
	}

	returned, err := svc.Finalize(ctx, tenantID, inv.ID)
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	final, err := store.Get(ctx, tenantID, inv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if committer.calls != 1 || final.TaxTransactionID != "tx_taxcalc_fin_1" || final.PublicToken == "" {
		t.Fatalf("finalize side writes: %d commits, tax_transaction_id %q, token set %v; want 1 commit, tx_taxcalc_fin_1, a token",
			committer.calls, final.TaxTransactionID, final.PublicToken != "")
	}
	payload, ok := events.got[domain.EventInvoiceFinalized]
	if !ok {
		t.Fatal("no invoice.finalized event")
	}
	asJSON := func(v any) map[string]any {
		enc, _ := json.Marshal(v)
		var m map[string]any
		_ = json.Unmarshal(enc, &m)
		return m
	}
	got := asJSON(payload)[domain.WebhookResourceInvoice]
	want := asJSON(domain.InvoiceWebhookData(events.row[domain.EventInvoiceFinalized], nil))[domain.WebhookResourceInvoice]
	if !reflect.DeepEqual(got, want) {
		t.Errorf("invoice.finalized data.invoice differs from the row when the event was created:\n got %v\nwant %v", got, want)
	}
	// Finalize's caller (the API answer) gets the same row.
	if returned.TaxTransactionID != final.TaxTransactionID || !returned.UpdatedAt.Equal(final.UpdatedAt) {
		t.Errorf("Finalize returned tax_transaction_id %q updated_at %v; the row has %q %v",
			returned.TaxTransactionID, returned.UpdatedAt, final.TaxTransactionID, final.UpdatedAt)
	}
}
