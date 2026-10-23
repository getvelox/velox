package api

import (
	"bytes"
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

// TestE2E_InvoiceSettlement_OfflineAndStripeWebhook takes three finalized
// invoices of one customer to a payment outcome over the real router and
// database:
//
//   - A: "Record offline payment" (POST /v1/invoices/{id}/record-payment).
//   - B: Stripe's payment_intent.succeeded, posted to
//     /v1/webhooks/stripe/{endpoint_id} with a real signature.
//   - C: Stripe's payment_intent.payment_failed, the same way.
//
// Each outcome is read back through the API (the invoice, its payment
// timeline, its dunning run, the customer's sent emails), and each happens
// once: a replayed request changes nothing. Before this test the
// record-payment success path had only a unit test, and no test sent a
// Stripe webhook over HTTP into a real database.
//
// The PaymentIntents are not created through Velox (that needs a card saved
// at Stripe). The events find their invoice by its velox_invoice_id
// metadata, the path a webhook takes when it arrives before the charge has
// recorded its PaymentIntent on the invoice.
func TestE2E_InvoiceSettlement_OfflineAndStripeWebhook(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ts := httptest.NewServer(NewServer(db, clock.Real(), nil))
	defer ts.Close()

	tenantID := testutil.CreateTestTenant(t, db, "Settlement Flow Co")
	key := createAPIKeyWithMode(t, db, tenantID, false)
	call := func(method, path string, req flowReq) flowResp {
		t.Helper()
		req.bearer = key
		return flowDo(t, ts, method, path, req)
	}

	// With a dunning policy, finalizing an invoice that has no card on file
	// opens a dunning run. The first policy becomes the default.
	call("POST", "/v1/dunning/policies", flowReq{body: map[string]any{
		"name": "Settlement flow policy", "enabled": true, "retry_days": []int{3, 3},
	}}).want(t, 201, "create dunning policy")

	const customerEmail = "ap@settlement-flow.test"
	customerID := flowStr(call("POST", "/v1/customers", flowReq{body: map[string]any{
		"external_id": "settlement-flow", "display_name": "Settlement Flow Customer", "email": customerEmail,
	}}).want(t, 201, "create customer").body, "id")

	newInvoice := func(desc string, cents int64) map[string]any {
		t.Helper()
		id := flowStr(call("POST", "/v1/invoices", flowReq{body: map[string]any{
			"customer_id": customerID,
			"line_items":  []map[string]any{{"description": desc, "quantity": 1, "unit_amount_cents": cents}},
		}}).want(t, 201, "create invoice: "+desc).body, "id")
		inv := call("POST", "/v1/invoices/"+id+"/finalize", flowReq{}).want(t, 200, "finalize: "+desc).body
		if flowStr(inv, "status") != "finalized" || flowInt(inv, "amount_due_cents") != cents {
			t.Fatalf("finalized %q: %v, want finalized with %d due", desc, inv, cents)
		}
		return inv
	}
	getInvoice := func(id string) map[string]any {
		t.Helper()
		return flowObj(call("GET", "/v1/invoices/"+id, flowReq{}).want(t, 200, "get invoice").body, "invoice")
	}
	dunningRun := func(invoiceID string) map[string]any {
		t.Helper()
		runs := flowList(call("GET", "/v1/dunning/runs?invoice_id="+invoiceID, flowReq{}).want(t, 200, "list dunning runs").body, "data")
		if len(runs) != 1 {
			t.Fatalf("invoice %s has %d dunning runs, want 1", invoiceID, len(runs))
		}
		return runs[0]
	}
	// settled is what a payment outcome writes on the invoice. A replay
	// must leave all of it as it was.
	settled := func(inv map[string]any) string {
		return fmt.Sprintf("status=%v payment_status=%v due=%v paid=%v paid_at=%v pi=%v error=%v updated_at=%v",
			inv["status"], inv["payment_status"], inv["amount_due_cents"], inv["amount_paid_cents"],
			inv["paid_at"], inv["stripe_payment_intent_id"], inv["last_payment_error"], inv["updated_at"])
	}

	invA := newInvoice("Offline payment", 12000)
	invB := newInvoice("Card payment", 4500)
	invC := newInvoice("Declined card", 7300)
	idA, idB, idC := flowStr(invA, "id"), flowStr(invB, "id"), flowStr(invC, "id")
	for _, id := range []string{idA, idB, idC} {
		if state := flowStr(dunningRun(id), "state"); state != "active" {
			t.Fatalf("invoice %s dunning run is %q after finalizing with no card, want active", id, state)
		}
	}

	// A: record an offline payment, with an Idempotency-Key as a retrying
	// client sends it.
	recordA := flowReq{header: map[string]string{"Idempotency-Key": "settlement-flow-record-a"}, body: map[string]any{"note": "Wire ref SF-1"}}
	recorded := call("POST", "/v1/invoices/"+idA+"/record-payment", recordA).want(t, 200, "record offline payment")
	paidA := getInvoice(idA)
	if flowStr(paidA, "status") != "paid" || flowStr(paidA, "payment_status") != "succeeded" ||
		flowInt(paidA, "amount_due_cents") != 0 || flowInt(paidA, "amount_paid_cents") != 12000 ||
		flowStr(paidA, "paid_at") == "" || flowStr(paidA, "stripe_payment_intent_id") != "" || paidA["paid_out_of_band"] != true {
		t.Errorf("invoice A after record-payment: %s paid_out_of_band=%v, want paid, 0 due, 12000 paid, no PaymentIntent, paid_out_of_band", settled(paidA), paidA["paid_out_of_band"])
	}
	if settled(recorded.body) != settled(paidA) {
		t.Errorf("record-payment answered %s, but the invoice reads %s", settled(recorded.body), settled(paidA))
	}
	timeline := call("GET", "/v1/invoices/"+idA+"/payment-timeline", flowReq{}).want(t, 200, "payment timeline")
	var paidRows []map[string]any
	for _, e := range flowList(timeline.body, "events") {
		if flowStr(e, "event_type") == "invoice.paid" {
			paidRows = append(paidRows, e)
		}
	}
	if len(paidRows) != 1 || flowStr(paidRows[0], "description") != "Offline payment received" || flowInt(paidRows[0], "amount_cents") != 12000 {
		t.Errorf("timeline paid rows = %v, want one \"Offline payment received\" row for 12000", paidRows)
	}
	if d, _ := timeline.body["degraded"].([]any); len(d) != 0 {
		t.Errorf("timeline degraded lanes: %v", d)
	}
	if run := dunningRun(idA); flowStr(run, "state") != "resolved" || flowStr(run, "resolution") != "payment_recovered" {
		t.Errorf("invoice A dunning run = %s/%s, want resolved/payment_recovered", flowStr(run, "state"), flowStr(run, "resolution"))
	}
	// The same request again: the first answer comes back and nothing is
	// written. Without the key, the invoice is already paid.
	if replay := call("POST", "/v1/invoices/"+idA+"/record-payment", recordA).want(t, 200, "replay with the same key"); !bytes.Equal(replay.raw, recorded.raw) {
		t.Errorf("replay answered %s, want the first answer %s", replay.raw, recorded.raw)
	}
	again := call("POST", "/v1/invoices/"+idA+"/record-payment", flowReq{body: map[string]any{"note": "Wire ref SF-1"}}).want(t, 409, "record a second payment")
	if !strings.Contains(again.errMessage(), "already paid") {
		t.Errorf("second record-payment message %q, want it to say the invoice is already paid", again.errMessage())
	}
	if settled(getInvoice(idA)) != settled(paidA) {
		t.Errorf("invoice A changed after the replays: %s, was %s", settled(getInvoice(idA)), settled(paidA))
	}
	recordedRows := 0
	for _, e := range flowList(call("GET", "/v1/audit-log?resource_type=invoice&resource_id="+idA, flowReq{}).want(t, 200, "audit log").body, "data") {
		if flowStr(e, "metadata", "action") == "payment_recorded" {
			recordedRows++
		}
	}
	if recordedRows != 1 {
		t.Errorf("payment_recorded audit rows for invoice A: %d, want 1", recordedRows)
	}

	// Connect Stripe in test mode, then register the webhook signing secret
	// as the settings page does. (The connect call itself checks the keys
	// with Stripe, so the keys go in through the store.)
	if _, err := tenantstripe.NewStore(db).Upsert(context.Background(), tenantstripe.UpsertInput{
		TenantID: tenantID, Livemode: false, SecretKey: "sk_test_settlement_flow", PublishableKey: "pk_test_settlement_flow",
	}); err != nil {
		t.Fatalf("connect stripe: %v", err)
	}
	const signingSecret = "whsec_settlement_flow"
	creds := call("PATCH", "/v1/settings/stripe/test/webhook", flowReq{body: map[string]any{"webhook_secret": signingSecret}}).want(t, 200, "set webhook secret").body
	if creds["has_webhook_secret"] != true || flowStr(creds, "id") == "" {
		t.Fatalf("webhook secret response: %v", creds)
	}
	endpoint := "/v1/webhooks/stripe/" + flowStr(creds, "id")
	stripe := installStripeStub(t)

	deliver := func(path string, payload []byte, signature string) flowResp {
		t.Helper()
		return flowDo(t, ts, "POST", path, flowReq{header: map[string]string{"Stripe-Signature": signature}, body: payload})
	}
	piEvent := func(eventID, eventType, piID string, inv map[string]any, fields map[string]any) map[string]any {
		obj := map[string]any{
			"id": piID, "object": "payment_intent", "amount": flowInt(inv, "amount_due_cents"),
			"currency": strings.ToLower(flowStr(inv, "currency")), "customer": "cus_settlement_flow",
			"metadata": map[string]any{"velox_tenant_id": tenantID, "velox_invoice_id": flowStr(inv, "id"), "velox_customer_id": customerID},
		}
		for k, v := range fields {
			obj[k] = v
		}
		return map[string]any{
			"id": eventID, "object": "event", "type": eventType, "created": time.Now().Unix(), "livemode": false,
			"data": map[string]any{"object": obj},
		}
	}
	declined := map[string]any{"status": "requires_payment_method", "last_payment_error": map[string]any{"message": "Your card was declined."}}

	// B: refused first, without touching the invoice: an unknown endpoint,
	// another account's secret, a changed body.
	eventB := piEvent("evt_settlement_flow_b", "payment_intent.succeeded", "pi_settlement_flow_b", invB, map[string]any{"status": "succeeded"})
	succeeded, signature := signedStripeEvent(t, signingSecret, eventB)
	_, foreignSignature := signedStripeEvent(t, "whsec_another_account", eventB)
	tampered := bytes.Replace(succeeded, []byte(`"amount":4500`), []byte(`"amount":1`), 1)
	if bytes.Equal(tampered, succeeded) {
		t.Fatal("test setup: the tampered body equals the signed one")
	}
	deliver("/v1/webhooks/stripe/vlx_spc_unknown", succeeded, signature).want(t, 404, "event for an unknown endpoint")
	if res := deliver(endpoint, succeeded, foreignSignature).want(t, 400, "event signed with another secret"); !strings.Contains(res.errMessage(), "invalid signature") {
		t.Errorf("wrong-secret refusal message %q, want invalid signature", res.errMessage())
	}
	deliver(endpoint, tampered, signature).want(t, 400, "event with a changed body")
	if got := getInvoice(idB); flowStr(got, "status") != "finalized" || flowStr(got, "payment_status") == "succeeded" {
		t.Fatalf("invoice B changed after refused events: %s", settled(got))
	}
	if n := flowCount(t, db, `SELECT count(*) FROM stripe_webhook_events WHERE stripe_event_id = $1`, "evt_settlement_flow_b"); n != 0 {
		t.Fatalf("refused events left %d stripe_webhook_events rows, want 0", n)
	}

	// The signed event settles the invoice, closes its dunning run, and the
	// card comes from Stripe.
	if res := deliver(endpoint, succeeded, signature).want(t, 200, "payment_intent.succeeded"); flowStr(res.body, "status") != "processed" {
		t.Errorf("webhook answered %s, want status processed", res.raw)
	}
	paidB := getInvoice(idB)
	if flowStr(paidB, "status") != "paid" || flowStr(paidB, "payment_status") != "succeeded" ||
		flowInt(paidB, "amount_due_cents") != 0 || flowInt(paidB, "amount_paid_cents") != 4500 ||
		flowStr(paidB, "stripe_payment_intent_id") != "pi_settlement_flow_b" || flowStr(paidB, "paid_at") == "" || paidB["paid_out_of_band"] != false {
		t.Errorf("invoice B after payment_intent.succeeded: %s paid_out_of_band=%v, want paid, 0 due, 4500 paid, pi_settlement_flow_b, not out of band", settled(paidB), paidB["paid_out_of_band"])
	}
	if flowStr(paidB, "payment_card_brand") != "visa" || flowStr(paidB, "payment_card_last4") != "4242" {
		t.Errorf("invoice B card = %q %q, want visa 4242 from the PaymentIntent", flowStr(paidB, "payment_card_brand"), flowStr(paidB, "payment_card_last4"))
	}
	if run := dunningRun(idB); flowStr(run, "state") != "resolved" || flowStr(run, "resolution") != "payment_recovered" {
		t.Errorf("invoice B dunning run = %s/%s, want resolved/payment_recovered", flowStr(run, "state"), flowStr(run, "resolution"))
	}

	// Stripe redelivers: acknowledged, and nothing changes.
	deliver(endpoint, succeeded, signature).want(t, 200, "redelivered payment_intent.succeeded")
	if got := getInvoice(idB); settled(got) != settled(paidB) {
		t.Errorf("invoice B changed on redelivery: %s, was %s", settled(got), settled(paidB))
	}
	if n := flowCount(t, db, `SELECT count(*) FROM stripe_webhook_events WHERE stripe_event_id = $1`, "evt_settlement_flow_b"); n != 1 {
		t.Errorf("stripe_webhook_events rows for the event: %d, want 1", n)
	}
	if n := flowCount(t, db, `SELECT count(*) FROM webhook_outbox WHERE event_type = 'invoice.paid' AND payload->'invoice'->>'id' = $1`, idB); n != 1 {
		t.Errorf("invoice.paid events for invoice B: %d, want 1", n)
	}
	// A late decline for the paid invoice does not undo the payment.
	lateFail, lateSignature := signedStripeEvent(t, signingSecret,
		piEvent("evt_settlement_flow_b_late", "payment_intent.payment_failed", "pi_settlement_flow_b_old", invB, declined))
	deliver(endpoint, lateFail, lateSignature).want(t, 200, "late payment_failed for a paid invoice")
	if got := getInvoice(idB); settled(got) != settled(paidB) {
		t.Errorf("invoice B changed on a late decline: %s, was %s", settled(got), settled(paidB))
	}

	// C: a decline marks the payment failed and keeps the invoice open.
	failed, failSignature := signedStripeEvent(t, signingSecret,
		piEvent("evt_settlement_flow_c", "payment_intent.payment_failed", "pi_settlement_flow_c", invC, declined))
	deliver(endpoint, failed, failSignature).want(t, 200, "payment_intent.payment_failed")
	failedC := getInvoice(idC)
	if flowStr(failedC, "status") != "finalized" || flowStr(failedC, "payment_status") != "failed" ||
		flowInt(failedC, "amount_due_cents") != 7300 || flowInt(failedC, "amount_paid_cents") != 0 ||
		flowStr(failedC, "last_payment_error") != "Your card was declined." || flowStr(failedC, "stripe_payment_intent_id") != "pi_settlement_flow_c" {
		t.Errorf("invoice C after payment_failed: %s, want finalized, failed, 7300 due, the decline message", settled(failedC))
	}
	if state := flowStr(dunningRun(idC), "state"); state != "active" {
		t.Errorf("invoice C dunning run is %q after a decline, want active", state)
	}
	deliver(endpoint, failed, failSignature).want(t, 200, "redelivered payment_intent.payment_failed")
	if got := getInvoice(idC); settled(got) != settled(failedC) {
		t.Errorf("invoice C changed on redelivery: %s, was %s", settled(got), settled(failedC))
	}
	if n := flowCount(t, db, `SELECT count(*) FROM webhook_outbox WHERE event_type = 'payment.failed' AND payload->'invoice'->>'id' = $1`, idC); n != 1 {
		t.Errorf("payment.failed events for invoice C: %d, want 1", n)
	}

	// The customer's sent emails: one receipt for B, one decline notice
	// for C, both to the customer.
	sent := call("GET", "/v1/customers/"+customerID+"/sent-emails", flowReq{}).want(t, 200, "sent emails")
	emails := map[string]int{}
	for _, e := range flowList(sent.body, "sent_emails") {
		if r := flowStr(e, "recipient"); r != customerEmail {
			t.Errorf("sent email %s went to %q, want %q", flowStr(e, "email_type"), r, customerEmail)
		}
		emails[flowStr(e, "email_type")+" "+flowStr(e, "invoice_number")]++
	}
	for k, want := range map[string]int{
		"payment_receipt " + flowStr(invB, "invoice_number"): 1,
		"payment_failed " + flowStr(invC, "invoice_number"):  1,
		"payment_receipt " + flowStr(invC, "invoice_number"): 0,
		"payment_failed " + flowStr(invB, "invoice_number"):  0,
	} {
		if emails[k] != want {
			t.Errorf("sent emails %q: %d, want %d (all: %v)", k, emails[k], want, emails)
		}
	}

	// Stripe was asked for B's card once, and for nothing else.
	if n := stripe.count("GET /v1/payment_intents/pi_settlement_flow_b"); n != 1 {
		t.Errorf("PaymentIntent reads for B: %d, want 1", n)
	}
	if u := stripe.unexpected(); len(u) != 0 {
		t.Errorf("unexpected Stripe calls: %v", u)
	}
}
