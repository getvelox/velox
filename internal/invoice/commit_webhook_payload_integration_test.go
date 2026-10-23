package invoice_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/sagarsuperuser/velox/internal/creditnote"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// TestCommitRelief_WebhookCarriesTheBalance: credit.commit_retired queues
// the customer's balance in the grant's currency as GET
// /v1/credits/balance/{customer_id} lists it, after the retire, with the
// retire's own figures beside it (ADR-131). The depletion the retire causes
// carries the same balance.
func TestCommitRelief_WebhookCarriesTheBalance(t *testing.T) {
	h, ctx := newCommitHarness(t, "Relief Webhook Payload")
	inv := h.createCommitInvoice(t, ctx, 9000, 10000, nil)
	if _, err := h.invSvc.Finalize(ctx, h.tenantID, inv.ID); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	payOffline(t, h, ctx, inv.ID)
	drain(t, h, ctx, 4000, 1)

	cn, err := newReliefCNService(h).CreateAndIssueCommitRelief(ctx, h.tenantID, creditnote.CommitReliefInput{
		InvoiceID: inv.ID, RetireAll: true, Reason: "customer churned",
	})
	if err != nil {
		t.Fatalf("relief: %v", err)
	}

	balances, err := h.creditSvc.GetBalance(ctx, h.tenantID, h.custID)
	if err != nil || len(balances) != 1 {
		t.Fatalf("balance = %+v, %v; want one USD balance", balances, err)
	}
	enc, _ := json.Marshal(balances[0])
	var want map[string]any
	if err := json.Unmarshal(enc, &want); err != nil {
		t.Fatal(err)
	}

	event := func(eventType string) map[string]any {
		t.Helper()
		tx, err := h.db.BeginTx(ctx, postgres.TxBypass, "")
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer postgres.Rollback(tx)
		var raw []byte
		if err := tx.QueryRowContext(ctx, `SELECT payload FROM webhook_outbox
			WHERE tenant_id = $1 AND event_type = $2 ORDER BY created_at DESC, id DESC LIMIT 1`, h.tenantID, eventType).Scan(&raw); err != nil {
			t.Fatalf("read %s: %v", eventType, err)
		}
		var data map[string]any
		if err := json.Unmarshal(raw, &data); err != nil {
			t.Fatal(err)
		}
		if err := domain.CheckWebhookData(eventType, data); err != nil {
			t.Fatal(err)
		}
		if got := data[domain.WebhookResourceCreditBalance]; !reflect.DeepEqual(got, any(want)) {
			t.Errorf("%s: data.credit_balance differs from the API's:\n got %v\nwant %v", eventType, got, want)
		}
		return data
	}

	retired := event(domain.EventCreditCommitRetired)
	grant, ok := h.commitGrant(t, ctx, inv.ID)
	if !ok {
		t.Fatal("no commit grant")
	}
	for k, v := range map[string]any{
		"grant_id": grant.ID, "funding_invoice_id": inv.ID, "credit_note_id": cn.ID,
		"retired_cents": float64(6000), "remaining_after_cents": float64(0),
	} {
		if retired[k] != v {
			t.Errorf("credit.commit_retired %s = %v, want %v", k, retired[k], v)
		}
	}
	if _, ok := retired["refunded_gross_cents"].(float64); !ok {
		t.Errorf("credit.commit_retired refunded_gross_cents = %v, want a number", retired["refunded_gross_cents"])
	}
	event(domain.EventCreditBalanceDepleted)
}
