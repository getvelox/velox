package invoice_test

import (
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/credit"
	"github.com/sagarsuperuser/velox/internal/creditnote"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// T5 (commit relief): the relief retire row and its credit.commit_retired
// event carry the commit grant's currency, which is the funding invoice's
// (ADR-129).
func TestCommitRelief_RetireCarriesTheGrantCurrency(t *testing.T) {
	h, ctx := newCommitHarness(t, "Relief Currency")
	grantCents := int64(10000)
	inv, err := h.invSvc.Create(ctx, h.tenantID, invoice.CreateInput{
		CustomerID: h.custID, Currency: "EUR",
		LineItems: []invoice.AddLineItemInput{{
			Description: "Prepaid commit — EUR", LineType: "add_on", Quantity: 1,
			UnitAmountCents: 9000, CommitGrantedCents: &grantCents,
		}},
	})
	if err != nil {
		t.Fatalf("create EUR commit invoice: %v", err)
	}
	if _, err := h.invSvc.Finalize(ctx, h.tenantID, inv.ID); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	payOffline(t, h, ctx, inv.ID)
	target, err := invoice.NewPostgresStore(h.db).Create(ctx, h.tenantID, domain.Invoice{
		CustomerID: h.custID, InvoiceNumber: "VLX-RELIEF-EUR-DRAIN",
		Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending,
		Currency: "EUR", SubtotalCents: 4000, TotalAmountCents: 4000, AmountDueCents: 4000,
		BillingPeriodStart: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		BillingPeriodEnd:   time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("seed EUR drain target: %v", err)
	}
	if applied, err := credit.NewPostgresStore(h.db).ApplyToInvoiceAtomic(ctx, h.tenantID, h.custID, target.ID, "drain", 4000, time.Now().UTC()); err != nil || applied != 4000 {
		t.Fatalf("drain = %d, %v; want 4000", applied, err)
	}

	if _, err := newReliefCNService(h).CreateAndIssueCommitRelief(ctx, h.tenantID, creditnote.CommitReliefInput{
		InvoiceID: inv.ID, RetireAll: true, Reason: "customer churned",
	}); err != nil {
		t.Fatalf("relief: %v", err)
	}

	tx, err := h.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	var rowCcy, eventCcy string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(currency, '') FROM customer_credit_ledger
		WHERE metadata->>'reason' = 'commit_refund_retire' AND metadata->>'funding_invoice_id' = $1`, inv.ID).Scan(&rowCcy); err != nil {
		t.Fatalf("read relief retire row: %v", err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(payload->'credit_balance'->>'currency', '') FROM webhook_outbox
		WHERE tenant_id = $1 AND event_type = $2`, h.tenantID, domain.EventCreditCommitRetired).Scan(&eventCcy); err != nil {
		t.Fatalf("read commit_retired event: %v", err)
	}
	if rowCcy != "EUR" {
		t.Errorf("relief retire row currency %q, want EUR", rowCcy)
	}
	if eventCcy != "EUR" {
		t.Errorf("credit.commit_retired payload currency %q, want EUR", eventCcy)
	}
}
