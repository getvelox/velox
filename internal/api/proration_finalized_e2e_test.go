package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// TestE2E_ProrationInvoice_FinalizedWebhookAndAuditRow (#1279): a mid-period
// upgrade, seat increase or added item writes a proration invoice that is
// born finalized. It must send invoice.finalized exactly once, with the
// payload the engine sends for its own invoices, and write one finalize
// audit row, as a cycle or first invoice does. An integrator's revenue sync
// keyed on invoice.finalized otherwise never sees these invoices.
//
// The subscription's first invoice (engine-born) is the control: the same
// queries find its event and its row, so a zero for the proration invoice is
// the missing effect, not a wrong query.
//
// Mutation seam: drop the finalize audit emission or the post-commit
// dispatch from the proration writer → this test fails on that count.
func TestE2E_ProrationInvoice_FinalizedWebhookAndAuditRow(t *testing.T) {
	skipShort(t)
	changeAt := lagP0.AddDate(0, 0, 10)
	cases := []struct {
		name  string
		write func(r *lagRig) (string, string, any)
	}{
		{"upgrade", func(r *lagRig) (string, string, any) {
			return http.MethodPatch, "/v1/subscriptions/" + r.subID + "/items/" + r.itemA, map[string]any{"new_plan_id": r.planB, "immediate": true}
		}},
		{"seat increase", func(r *lagRig) (string, string, any) {
			return http.MethodPatch, "/v1/subscriptions/" + r.subID + "/items/" + r.itemA, map[string]any{"quantity": 3}
		}},
		{"add item", func(r *lagRig) (string, string, any) {
			return http.MethodPost, "/v1/subscriptions/" + r.subID + "/items", map[string]any{"plan_id": r.planC, "quantity": 1}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newLagRig(t, "in_advance", false)
			r.payFirstInvoice(lagP0.Add(time.Hour))

			method, path, body := c.write(r)
			if code, raw := r.do(method, path, changeAt, body); code >= 300 {
				t.Fatalf("%s %s: %d %s", method, path, code, raw)
			}
			pro := r.prorationInvoice()
			if pro.status != string(domain.InvoiceFinalized) {
				t.Fatalf("proration invoice status = %s, want finalized (no tax deferral, collection running)", pro.status)
			}
			r.wantFinalizedOnce(pro.id, domain.BillingReasonSubscriptionUpdate)
		})
	}

	// A proration invoice born a draft (collection paused) sends nothing and
	// writes no finalize row until it is finalized; then it gets exactly one
	// of each, from the finalize itself.
	t.Run("draft until finalized", func(t *testing.T) {
		r := newLagRig(t, "in_advance", false)
		r.payFirstInvoice(lagP0.Add(time.Hour))
		r.must(http.MethodPut, "/v1/subscriptions/"+r.subID+"/pause-collection", lagP0.AddDate(0, 0, 2),
			map[string]any{"behavior": "keep_as_draft"}, http.StatusOK)
		if code, raw := r.do(http.MethodPatch, "/v1/subscriptions/"+r.subID+"/items/"+r.itemA, changeAt,
			map[string]any{"new_plan_id": r.planB, "immediate": true}); code >= 300 {
			t.Fatalf("upgrade while collection is paused: %d %s", code, raw)
		}
		pro := r.prorationInvoice()
		if pro.status != string(domain.InvoiceDraft) {
			t.Fatalf("proration invoice status = %s, want draft (collection paused)", pro.status)
		}
		if n := r.finalizedEvents(pro.id); n != 0 {
			t.Errorf("draft proration invoice: %d invoice.finalized events, want 0", n)
		}
		if n := r.finalizeRows(pro.id); n != 0 {
			t.Errorf("draft proration invoice: %d finalize audit rows, want 0", n)
		}

		r.must(http.MethodPost, "/v1/invoices/"+pro.id+"/finalize", changeAt.Add(time.Hour), nil, http.StatusOK)
		if n := r.finalizedEvents(pro.id); n != 1 {
			t.Errorf("after finalize: %d invoice.finalized events, want 1", n)
		}
		if n := r.finalizeRows(pro.id); n != 1 {
			t.Errorf("after finalize: %d finalize audit rows, want 1", n)
		}
	})
}

// payFirstInvoice checks the control (the subscription's first invoice, born
// finalized by the engine, has its one event and one row), then records an
// offline payment on it. The check runs first because the payment lowers the
// amount due the event reported. A charge on top of an unpaid invoice for the
// running period is refused (ADR-050), which is not what these cases test.
func (r *lagRig) payFirstInvoice(at time.Time) {
	r.t.Helper()
	var id string
	for _, i := range r.invoices() {
		if i.reason == string(domain.BillingReasonSubscriptionCreate) {
			if id != "" {
				r.t.Fatalf("two subscription_create invoices")
			}
			id = i.id
		}
	}
	if id == "" {
		r.logInvoices(r.invoices())
		r.t.Fatalf("no subscription_create invoice")
	}
	r.wantFinalizedOnce(id, domain.BillingReasonSubscriptionCreate)
	r.must(http.MethodPost, "/v1/invoices/"+id+"/record-payment", at, map[string]any{"note": "fixture"}, http.StatusOK)
}

// prorationInvoice returns the subscription's one subscription_update invoice.
func (r *lagRig) prorationInvoice() lagInvoice {
	r.t.Helper()
	invs := r.invoices()
	var found []lagInvoice
	for _, i := range invs {
		if i.reason == string(domain.BillingReasonSubscriptionUpdate) {
			found = append(found, i)
		}
	}
	if len(found) != 1 {
		r.logInvoices(invs)
		r.t.Fatalf("%d proration invoices, want 1", len(found))
	}
	return found[0]
}

func (r *lagRig) finalizedEvents(invoiceID string) int {
	r.t.Helper()
	return flowCount(r.t, r.db, `SELECT count(*) FROM webhook_outbox WHERE event_type = 'invoice.finalized' AND payload->'invoice'->>'id' = $1`, invoiceID)
}

func (r *lagRig) finalizeRows(invoiceID string) int {
	r.t.Helper()
	return flowCount(r.t, r.db, `SELECT count(*) FROM audit_log WHERE action = 'finalize' AND resource_type = 'invoice' AND resource_id = $1`, invoiceID)
}

// wantFinalizedOnce checks that invoiceID has exactly one invoice.finalized
// event carrying the invoice's own facts, and exactly one finalize audit row
// naming the billing reason that created it.
func (r *lagRig) wantFinalizedOnce(invoiceID string, reason domain.InvoiceBillingReason) {
	r.t.Helper()
	if n := r.finalizedEvents(invoiceID); n != 1 {
		r.t.Errorf("%s invoice %s: %d invoice.finalized events, want 1", reason, invoiceID, n)
	}
	if n := r.finalizeRows(invoiceID); n != 1 {
		r.t.Errorf("%s invoice %s: %d finalize audit rows, want 1", reason, invoiceID, n)
	}

	ctx := context.Background()
	tx, err := r.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		r.t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)

	var number, customerID, currency string
	var total, due, credited int64
	if err := tx.QueryRowContext(ctx, `SELECT invoice_number, customer_id, currency, total_amount_cents, amount_due_cents, credits_applied_cents
		FROM invoices WHERE id = $1`, invoiceID).Scan(&number, &customerID, &currency, &total, &due, &credited); err != nil {
		r.t.Fatalf("read invoice %s: %v", invoiceID, err)
	}

	var raw []byte
	err = tx.QueryRowContext(ctx, `SELECT payload FROM webhook_outbox
		WHERE event_type = 'invoice.finalized' AND payload->'invoice'->>'id' = $1 ORDER BY created_at LIMIT 1`, invoiceID).Scan(&raw)
	if err == nil {
		var p map[string]any
		if err := json.Unmarshal(raw, &p); err != nil {
			r.t.Fatalf("decode payload: %v", err)
		}
		// The event carries the invoice and nothing beside it (ADR-131).
		if err := domain.CheckWebhookData(domain.EventInvoiceFinalized, p); err != nil {
			r.t.Errorf("%s invoice.finalized payload: %v (payload %s)", reason, err, raw)
		}
		got, _ := p[domain.WebhookResourceInvoice].(map[string]any)
		want := map[string]any{
			"id": invoiceID, "invoice_number": number, "customer_id": customerID,
			"status": "finalized", "payment_status": "pending",
			"total_amount_cents": float64(total), "amount_due_cents": float64(due),
			"credits_applied_cents": float64(credited), "currency": currency,
		}
		for k, v := range want {
			if got[k] != v {
				r.t.Errorf("%s invoice.finalized invoice %s = %v, want %v (payload %s)", reason, k, got[k], v, raw)
			}
		}
	}

	var triggeredBy string
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(metadata->>'triggered_by', '') FROM audit_log
		WHERE action = 'finalize' AND resource_type = 'invoice' AND resource_id = $1 ORDER BY created_at LIMIT 1`, invoiceID).Scan(&triggeredBy)
	if err == nil && triggeredBy != string(reason) {
		r.t.Errorf("finalize audit row for %s: triggered_by = %q, want %q", invoiceID, triggeredBy, reason)
	}
}
