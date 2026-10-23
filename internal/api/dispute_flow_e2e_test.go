package api

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/tenantstripe"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestE2E_CardDispute_RecordedNeverActedOn takes an invoice paid by card
// through Stripe's dispute events over the real router and database (#979):
//
//   - the events carry no velox_* metadata (a Dispute has its own, empty
//     metadata), and are still recorded against the invoice that the
//     disputed PaymentIntent paid;
//   - they arrive out of order (the close first) and redelivered, and the
//     record ends at the newest event with one payment.dispute_opened and
//     one payment.dispute_closed;
//   - the invoice stays paid with its amounts, and shows the dispute_lost
//     notice and the dispute rows on its timeline;
//   - a dispute on a payment no Velox invoice holds is acknowledged and
//     recorded nowhere; a status Velox does not know fails the delivery so
//     Stripe sends it again.
func TestE2E_CardDispute_RecordedNeverActedOn(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ts := httptest.NewServer(NewServer(db, clock.Real(), nil))
	defer ts.Close()

	tenantID := testutil.CreateTestTenant(t, db, "Dispute Flow Co")
	key := createAPIKeyWithMode(t, db, tenantID, false)
	call := func(method, path string, req flowReq) flowResp {
		t.Helper()
		req.bearer = key
		return flowDo(t, ts, method, path, req)
	}
	customerID := flowStr(call("POST", "/v1/customers", flowReq{body: map[string]any{
		"external_id": "dispute-flow", "display_name": "Dispute Flow Customer",
	}}).want(t, 201, "create customer").body, "id")
	invID := flowStr(call("POST", "/v1/invoices", flowReq{body: map[string]any{
		"customer_id": customerID,
		"line_items":  []map[string]any{{"description": "Disputed service", "quantity": 1, "unit_amount_cents": 4500}},
	}}).want(t, 201, "create invoice").body, "id")
	inv := call("POST", "/v1/invoices/"+invID+"/finalize", flowReq{}).want(t, 200, "finalize").body
	currency := strings.ToLower(flowStr(inv, "currency"))
	getInvoice := func() map[string]any {
		t.Helper()
		return flowObj(call("GET", "/v1/invoices/"+invID, flowReq{}).want(t, 200, "get invoice").body, "invoice")
	}
	settled := func(inv map[string]any) string {
		return fmt.Sprintf("status=%v payment_status=%v due=%v paid=%v paid_at=%v pi=%v",
			inv["status"], inv["payment_status"], inv["amount_due_cents"], inv["amount_paid_cents"],
			inv["paid_at"], inv["stripe_payment_intent_id"])
	}

	if _, err := tenantstripe.NewStore(db).Upsert(context.Background(), tenantstripe.UpsertInput{
		TenantID: tenantID, Livemode: false, SecretKey: "sk_test_dispute_flow", PublishableKey: "pk_test_dispute_flow",
	}); err != nil {
		t.Fatalf("connect stripe: %v", err)
	}
	const signingSecret = "whsec_dispute_flow"
	creds := call("PATCH", "/v1/settings/stripe/test/webhook", flowReq{body: map[string]any{"webhook_secret": signingSecret}}).want(t, 200, "set webhook secret").body
	endpoint := "/v1/webhooks/stripe/" + flowStr(creds, "id")
	stripe := installStripeStub(t)
	deliver := func(event map[string]any, status int, what string) flowResp {
		t.Helper()
		payload, sig := signedStripeEvent(t, signingSecret, event)
		return flowDo(t, ts, "POST", endpoint, flowReq{header: map[string]string{"Stripe-Signature": sig}, body: payload}).want(t, status, what)
	}

	// The card pays the invoice.
	const pi = "pi_dispute_flow"
	deliver(map[string]any{
		"id": "evt_dispute_flow_paid", "object": "event", "type": "payment_intent.succeeded",
		"created": time.Now().Unix(), "livemode": false,
		"data": map[string]any{"object": map[string]any{
			"id": pi, "object": "payment_intent", "status": "succeeded", "amount": 4500, "currency": currency,
			"metadata": map[string]any{"velox_tenant_id": tenantID, "velox_invoice_id": invID, "velox_customer_id": customerID},
		}},
	}, 200, "payment_intent.succeeded")
	paid := getInvoice()
	if flowStr(paid, "status") != "paid" || flowInt(paid, "amount_paid_cents") != 4500 {
		t.Fatalf("invoice after the card payment: %s, want paid 4500", settled(paid))
	}

	opened := time.Now().Add(-72 * time.Hour).Truncate(time.Second).UTC()
	due := opened.Add(14 * 24 * time.Hour)
	dispute := func(eventID, eventType, disputeID, piID, status string, at time.Time) map[string]any {
		return map[string]any{
			"id": eventID, "object": "event", "type": eventType, "created": at.Unix(), "livemode": false,
			"data": map[string]any{"object": map[string]any{
				"id": disputeID, "object": "dispute", "amount": 4500, "currency": currency,
				"charge": "ch_dispute_flow", "payment_intent": piID, "reason": "fraudulent",
				"status": status, "created": opened.Unix(), "livemode": false, "metadata": map[string]any{},
				"is_charge_refundable": false, "balance_transactions": []any{},
				"evidence_details": map[string]any{"due_by": due.Unix(), "has_evidence": false, "past_due": false, "submission_count": 0},
			}},
		}
	}
	lostAt := opened.Add(48 * time.Hour)
	closedEv := dispute("evt_dispute_flow_closed", "charge.dispute.closed", "du_dispute_flow", pi, "lost", lostAt)

	// The close arrives first, then the older events, then the close again.
	if res := deliver(closedEv, 200, "charge.dispute.closed"); flowStr(res.body, "status") != "processed" {
		t.Fatalf("dispute close answered %s, want processed (not skipped for missing metadata)", res.raw)
	}
	deliver(dispute("evt_dispute_flow_created", "charge.dispute.created", "du_dispute_flow", pi, "needs_response", opened), 200, "charge.dispute.created")
	deliver(dispute("evt_dispute_flow_updated", "charge.dispute.updated", "du_dispute_flow", pi, "under_review", opened.Add(time.Hour)), 200, "charge.dispute.updated")
	deliver(closedEv, 200, "redelivered charge.dispute.closed")

	disputes := flowList(call("GET", "/v1/invoices/"+invID+"/disputes", flowReq{}).want(t, 200, "list disputes").body, "data")
	if len(disputes) != 1 {
		t.Fatalf("disputes: %v, want 1", disputes)
	}
	d := disputes[0]
	if flowStr(d, "status") != "lost" || flowStr(d, "stripe_dispute_id") != "du_dispute_flow" ||
		flowStr(d, "stripe_payment_intent_id") != pi || flowStr(d, "stripe_charge_id") != "ch_dispute_flow" ||
		flowInt(d, "amount_cents") != 4500 || flowStr(d, "currency") != strings.ToUpper(currency) ||
		flowStr(d, "reason") != "fraudulent" || flowStr(d, "invoice_id") != invID {
		t.Errorf("dispute = %v, want lost du_dispute_flow on %s, 4500 %s, fraudulent", d, pi, strings.ToUpper(currency))
	}
	if got, _ := time.Parse(time.RFC3339, flowStr(d, "closed_at")); !got.Equal(lostAt) {
		t.Errorf("closed_at = %q, want the close event's time %s", flowStr(d, "closed_at"), lostAt)
	}
	if got, _ := time.Parse(time.RFC3339, flowStr(d, "opened_at")); !got.Equal(opened) {
		t.Errorf("opened_at = %q, want Stripe's dispute creation %s", flowStr(d, "opened_at"), opened)
	}

	// The invoice is untouched, and says what happened.
	after := getInvoice()
	if settled(after) != settled(paid) {
		t.Errorf("the dispute changed the invoice: %s, was %s", settled(after), settled(paid))
	}
	if r := flowStr(after, "attention", "reason"); r != "dispute_lost" || flowStr(after, "attention", "severity") != "critical" {
		t.Errorf("attention = %v, want critical dispute_lost", after["attention"])
	}
	timeline := call("GET", "/v1/invoices/"+invID+"/payment-timeline", flowReq{}).want(t, 200, "payment timeline")
	rows := map[string]string{}
	for _, e := range flowList(timeline.body, "events") {
		if flowStr(e, "source") == "dispute" {
			rows[flowStr(e, "event_type")] = flowStr(e, "description") + " / " + flowStr(e, "status")
		}
	}
	if rows["payment.dispute_opened"] != "Payment disputed / warning" ||
		rows["payment.dispute_closed"] != "Dispute closed / failed" || len(rows) != 2 {
		t.Errorf("timeline dispute rows = %v, want the opened row and the lost row", rows)
	}
	if deg, _ := timeline.body["degraded"].([]any); len(deg) != 0 {
		t.Errorf("timeline degraded lanes: %v", deg)
	}

	// One event each way, and one audit row per Stripe event, naming the payment.
	for eventType, want := range map[string]int{"payment.dispute_opened": 1, "payment.dispute_closed": 1} {
		if n := flowCount(t, db, `SELECT count(*) FROM webhook_outbox WHERE event_type = $1 AND payload->'invoice'->>'id' = $2`, eventType, invID); n != want {
			t.Errorf("%s events: %d, want %d", eventType, n, want)
		}
	}
	if n := flowCount(t, db, `SELECT count(*) FROM stripe_webhook_events WHERE stripe_event_id LIKE 'evt_dispute_flow_%' AND event_type LIKE 'charge.dispute.%' AND payment_intent_id = $1`, pi); n != 3 {
		t.Errorf("dispute audit rows naming %s: %d, want 3 (one per event, redelivery included once)", pi, n)
	}

	// A dispute on a payment no Velox invoice holds: acknowledged, recorded nowhere.
	deliver(dispute("evt_dispute_flow_foreign", "charge.dispute.created", "du_foreign", "pi_not_velox", "needs_response", opened), 200, "dispute on a foreign payment")
	if n := flowCount(t, db, `SELECT count(*) FROM invoice_disputes WHERE stripe_dispute_id = 'du_foreign'`); n != 0 {
		t.Errorf("foreign dispute rows: %d, want 0", n)
	}
	// A status Velox does not know: the delivery fails, so Stripe redelivers,
	// and nothing is recorded or marked processed.
	deliver(dispute("evt_dispute_flow_unknown", "charge.dispute.updated", "du_dispute_flow", pi, "under_arbitration", lostAt.Add(time.Hour)), 500, "dispute with an unknown status")
	if n := flowCount(t, db, `SELECT count(*) FROM stripe_webhook_events WHERE stripe_event_id = 'evt_dispute_flow_unknown'`); n != 0 {
		t.Errorf("unknown-status event was marked processed (%d rows); Stripe would never redeliver it", n)
	}
	if got := flowList(call("GET", "/v1/invoices/"+invID+"/disputes", flowReq{}).want(t, 200, "list disputes again").body, "data"); len(got) != 1 || flowStr(got[0], "status") != "lost" {
		t.Errorf("disputes after the unknown status: %v, want still one, lost", got)
	}

	call("GET", "/v1/invoices/vlx_inv_missing/disputes", flowReq{}).want(t, 404, "disputes of a missing invoice")
	if u := stripe.unexpected(); len(u) != 0 {
		t.Errorf("unexpected Stripe calls: %v (a dispute needs none)", u)
	}
}
