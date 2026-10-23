package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/tenantstripe"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// The payload rule (ADR-131), checked over the real router and database:
// every outbox row Velox queues carries, under its family's resource key,
// the resource as the API returns it. Each check compares the event's
// resource with a GET of the same resource right after the action, field
// by field. A field that moved after the event was queued (a post-commit
// write such as the card stamp) must be named in moved, so the test says
// exactly which fields an event shows from before them.

// outboxEvent is one webhook_outbox row.
type outboxEvent struct {
	Type string
	Data map[string]any
}

// outboxEvents reads the tenant's queued webhook events, oldest first.
func outboxEvents(t *testing.T, db *postgres.DB, tenantID string) []outboxEvent {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer postgres.Rollback(tx)
	rows, err := tx.Query(`SELECT event_type, payload FROM webhook_outbox WHERE tenant_id = $1 ORDER BY created_at, id`, tenantID)
	if err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []outboxEvent
	for rows.Next() {
		var ev outboxEvent
		var raw []byte
		if err := rows.Scan(&ev.Type, &raw); err != nil {
			t.Fatalf("scan outbox row: %v", err)
		}
		if err := json.Unmarshal(raw, &ev.Data); err != nil {
			t.Fatalf("decode %s payload: %v", ev.Type, err)
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	return out
}

// webhookCursor hands out the events queued since the last call.
type webhookCursor struct {
	t        *testing.T
	db       *postgres.DB
	tenantID string
	seen     int
}

// next returns the events queued since the last call, by type. It fails
// when an event type it is asked for (want) did not arrive exactly once.
func (c *webhookCursor) next(what string, want ...string) map[string]outboxEvent {
	c.t.Helper()
	all := outboxEvents(c.t, c.db, c.tenantID)
	fresh := all[c.seen:]
	c.seen = len(all)
	byType := map[string]outboxEvent{}
	count := map[string]int{}
	for _, ev := range fresh {
		byType[ev.Type] = ev
		count[ev.Type]++
	}
	for _, w := range want {
		if count[w] != 1 {
			c.t.Fatalf("%s: %d %s events queued, want 1 (queued: %v)", what, count[w], w, count)
		}
	}
	return byType
}

// readTimeFields are the keys the API works out per read, which events
// leave out (domain.*WebhookData).
var readTimeFields = map[string][]string{
	domain.WebhookResourceInvoice:      {"attention", "recovery_warning", "billing_period_display", "test_clock_id"},
	domain.WebhookResourceSubscription: {"current_billing_period_display"},
}

// checkEventResource checks ev against the payload rule and compares its
// resource with api (the resource as a GET returned it). moved names the
// fields allowed to differ because a write after the event changed them;
// each must actually differ, so a stale entry fails too.
func checkEventResource(t *testing.T, what string, ev outboxEvent, api map[string]any, moved ...string) {
	t.Helper()
	if err := domain.CheckWebhookData(ev.Type, ev.Data); err != nil {
		t.Errorf("%s: %v", what, err)
		return
	}
	key := domain.WebhookEventResource(ev.Type)
	got, _ := ev.Data[key].(map[string]any)
	want := map[string]any{}
	for k, v := range api {
		want[k] = v
	}
	for _, k := range readTimeFields[key] {
		delete(want, k)
	}
	allowed := map[string]bool{}
	for _, k := range moved {
		allowed[k] = true
	}
	keys := map[string]bool{}
	for k := range got {
		keys[k] = true
	}
	for k := range want {
		keys[k] = true
	}
	var diffs []string
	differs := map[string]bool{}
	for k := range keys {
		if reflect.DeepEqual(got[k], want[k]) {
			continue
		}
		differs[k] = true
		if !allowed[k] {
			diffs = append(diffs, fmt.Sprintf("%s: event %v, API %v", k, got[k], want[k]))
		}
	}
	for _, k := range moved {
		if !differs[k] {
			diffs = append(diffs, fmt.Sprintf("%s: listed as moved after the event but equal (%v)", k, got[k]))
		}
	}
	if len(diffs) > 0 {
		sort.Strings(diffs)
		t.Errorf("%s: data.%s differs from the API's:\n  %s", what, key, strings.Join(diffs, "\n  "))
	}
}

// TestE2E_WebhookPayloads_InvoicePaymentDunningPaymentMethod drives the
// invoice.*, payment.*, dunning.* and payment_method.* emitters through the
// API and Stripe's webhooks.
func TestE2E_WebhookPayloads_InvoicePaymentDunningPaymentMethod(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ts := httptest.NewServer(NewServer(db, clock.Real(), nil))
	defer ts.Close()

	tenantID := testutil.CreateTestTenant(t, db, "Webhook Payload Co")
	key := createAPIKeyWithMode(t, db, tenantID, false)
	call := func(method, path string, req flowReq) flowResp {
		t.Helper()
		req.bearer = key
		return flowDo(t, ts, method, path, req)
	}
	events := &webhookCursor{t: t, db: db, tenantID: tenantID}

	call("POST", "/v1/dunning/policies", flowReq{body: map[string]any{
		"name": "Payload policy", "enabled": true, "retry_days": []int{3, 3},
	}}).want(t, 201, "create dunning policy")
	customerID := flowStr(call("POST", "/v1/customers", flowReq{body: map[string]any{
		"external_id": "webhook-payload", "display_name": "Webhook Payload Customer", "email": "ap@payload.test",
	}}).want(t, 201, "create customer").body, "id")
	events.next("setup")

	getInvoice := func(id string) map[string]any {
		t.Helper()
		return flowObj(call("GET", "/v1/invoices/"+id, flowReq{}).want(t, 200, "get invoice").body, "invoice")
	}
	getRun := func(invoiceID string) map[string]any {
		t.Helper()
		runs := flowList(call("GET", "/v1/dunning/runs?invoice_id="+invoiceID, flowReq{}).want(t, 200, "list dunning runs").body, "data")
		if len(runs) != 1 {
			t.Fatalf("invoice %s has %d dunning runs, want 1", invoiceID, len(runs))
		}
		return flowObj(call("GET", "/v1/dunning/runs/"+flowStr(runs[0], "id"), flowReq{}).want(t, 200, "get dunning run").body, "run")
	}
	newInvoice := func(desc string, cents int64) string {
		t.Helper()
		id := flowStr(call("POST", "/v1/invoices", flowReq{body: map[string]any{
			"customer_id": customerID,
			"line_items":  []map[string]any{{"description": desc, "quantity": 1, "unit_amount_cents": cents}},
		}}).want(t, 201, "create invoice: "+desc).body, "id")
		call("POST", "/v1/invoices/"+id+"/finalize", flowReq{}).want(t, 200, "finalize: "+desc)
		got := events.next("finalize "+desc, domain.EventInvoiceFinalized, domain.EventDunningStarted)
		// The collection that runs after the event touches the row.
		checkEventResource(t, "invoice.finalized "+desc, got[domain.EventInvoiceFinalized], getInvoice(id),
			"updated_at")
		checkEventResource(t, "dunning.started "+desc, got[domain.EventDunningStarted], getRun(id))
		return id
	}

	// An offline payment.
	idA := newInvoice("Offline payment", 12000)
	call("POST", "/v1/invoices/"+idA+"/record-payment", flowReq{body: map[string]any{"note": "Wire ref P-1"}}).want(t, 200, "record payment")
	got := events.next("record payment", domain.EventInvoicePaid, domain.EventInvoicePaymentRecorded, domain.EventDunningResolved)
	checkEventResource(t, "invoice.paid (offline)", got[domain.EventInvoicePaid], getInvoice(idA))
	checkEventResource(t, "invoice.payment_recorded", got[domain.EventInvoicePaymentRecorded], getInvoice(idA))
	if r := got[domain.EventInvoicePaymentRecorded].Data["recovered_from"]; r != "finalized" {
		t.Errorf("invoice.payment_recorded recovered_from = %v, want finalized", r)
	}
	checkEventResource(t, "dunning.resolved (paid)", got[domain.EventDunningResolved], getRun(idA))

	// A void.
	idD := newInvoice("Voided", 3000)
	call("POST", "/v1/invoices/"+idD+"/void", flowReq{}).want(t, 200, "void")
	got = events.next("void", domain.EventInvoiceVoided, domain.EventDunningResolved)
	checkEventResource(t, "invoice.voided", got[domain.EventInvoiceVoided], getInvoice(idD))
	checkEventResource(t, "dunning.resolved (voided)", got[domain.EventDunningResolved], getRun(idD))

	// A write-off, then the money arrives offline.
	idE := newInvoice("Written off", 2500)
	call("POST", "/v1/invoices/"+idE+"/mark-uncollectible", flowReq{}).want(t, 200, "mark uncollectible")
	got = events.next("mark uncollectible", domain.EventInvoiceMarkedUncollectible)
	checkEventResource(t, "invoice.marked_uncollectible", got[domain.EventInvoiceMarkedUncollectible], getInvoice(idE))
	call("POST", "/v1/invoices/"+idE+"/record-payment", flowReq{body: map[string]any{"note": "Late wire"}}).want(t, 200, "record payment on a write-off")
	got = events.next("record payment on a write-off", domain.EventInvoicePaid, domain.EventInvoicePaymentRecorded)
	checkEventResource(t, "invoice.payment_recorded (write-off)", got[domain.EventInvoicePaymentRecorded], getInvoice(idE))
	if r := got[domain.EventInvoicePaymentRecorded].Data["recovered_from"]; r != "uncollectible" {
		t.Errorf("invoice.payment_recorded recovered_from = %v, want uncollectible", r)
	}

	// Stripe: a card payment, a second charge, a decline, a dispute, a card.
	if _, err := tenantstripe.NewStore(db).Upsert(context.Background(), tenantstripe.UpsertInput{
		TenantID: tenantID, Livemode: false, SecretKey: "sk_test_webhook_payload", PublishableKey: "pk_test_webhook_payload",
	}); err != nil {
		t.Fatalf("connect stripe: %v", err)
	}
	const signingSecret = "whsec_webhook_payload"
	creds := call("PATCH", "/v1/settings/stripe/test/webhook", flowReq{body: map[string]any{"webhook_secret": signingSecret}}).want(t, 200, "set webhook secret").body
	endpoint := "/v1/webhooks/stripe/" + flowStr(creds, "id")
	installStripeStub(t)
	deliver := func(event map[string]any, what string) {
		t.Helper()
		payload, sig := signedStripeEvent(t, signingSecret, event)
		flowDo(t, ts, "POST", endpoint, flowReq{header: map[string]string{"Stripe-Signature": sig}, body: payload}).want(t, 200, what)
	}
	stripeEvent := func(eventID, eventType string, object map[string]any) map[string]any {
		return map[string]any{
			"id": eventID, "object": "event", "type": eventType, "created": time.Now().Unix(), "livemode": false,
			"data": map[string]any{"object": object},
		}
	}
	piObject := func(piID, invoiceID string, cents int64, fields map[string]any) map[string]any {
		obj := map[string]any{
			"id": piID, "object": "payment_intent", "amount": cents, "amount_received": cents, "currency": "usd",
			"customer": "cus_webhook_payload",
			"metadata": map[string]any{"velox_tenant_id": tenantID, "velox_invoice_id": invoiceID, "velox_customer_id": customerID},
		}
		for k, v := range fields {
			obj[k] = v
		}
		return obj
	}

	idB := newInvoice("Card payment", 4500)
	deliver(stripeEvent("evt_wp_b", "payment_intent.succeeded", piObject("pi_wp_b", idB, 4500, map[string]any{"status": "succeeded"})), "payment_intent.succeeded")
	got = events.next("card payment", domain.EventInvoicePaid, domain.EventPaymentSucceeded, domain.EventDunningResolved)
	// The card used is stamped on the invoice after the payment commits.
	checkEventResource(t, "invoice.paid (card)", got[domain.EventInvoicePaid], getInvoice(idB),
		"payment_card_brand", "payment_card_last4", "updated_at")
	checkEventResource(t, "payment.succeeded", got[domain.EventPaymentSucceeded], getInvoice(idB),
		"payment_card_brand", "payment_card_last4", "updated_at")
	checkEventResource(t, "dunning.resolved (card)", got[domain.EventDunningResolved], getRun(idB))

	deliver(stripeEvent("evt_wp_b2", "payment_intent.succeeded", piObject("pi_wp_b2", idB, 4500, map[string]any{"status": "succeeded"})), "second payment_intent.succeeded")
	got = events.next("second charge", domain.EventPaymentDuplicateCharge)
	dup := got[domain.EventPaymentDuplicateCharge]
	// The anomaly marker is written on the invoice after the event.
	checkEventResource(t, "payment.duplicate_charge", dup, getInvoice(idB),
		"payment_anomaly_kind", "payment_anomaly_payment_intent_id", "payment_anomaly_captured_cents", "updated_at")
	if dup.Data["incoming_payment_intent_id"] != "pi_wp_b2" || dup.Data["captured_cents"] != float64(4500) {
		t.Errorf("payment.duplicate_charge extras = %v / %v, want pi_wp_b2 / 4500", dup.Data["incoming_payment_intent_id"], dup.Data["captured_cents"])
	}
	// The invoice names the other charge, the one it recorded (docs/webhooks.md).
	if pi := flowStr(dup.Data["invoice"].(map[string]any), "stripe_payment_intent_id"); pi != "pi_wp_b" {
		t.Errorf("payment.duplicate_charge invoice.stripe_payment_intent_id = %q, want pi_wp_b", pi)
	}

	// A charge for less than the invoice owed: the invoice is paid by it
	// and names it.
	idF := newInvoice("Short charge", 5000)
	deliver(stripeEvent("evt_wp_f", "payment_intent.succeeded", piObject("pi_wp_f", idF, 4000, map[string]any{"status": "succeeded"})), "short payment_intent.succeeded")
	got = events.next("short charge", domain.EventInvoicePaid, domain.EventPaymentSucceeded, domain.EventPaymentAmountMismatch, domain.EventDunningResolved)
	mis := got[domain.EventPaymentAmountMismatch]
	checkEventResource(t, "payment.amount_mismatch", mis, getInvoice(idF),
		"payment_anomaly_kind", "payment_anomaly_payment_intent_id", "payment_anomaly_captured_cents",
		"payment_card_brand", "payment_card_last4", "updated_at")
	if mis.Data["incoming_payment_intent_id"] != "pi_wp_f" || mis.Data["captured_cents"] != float64(4000) {
		t.Errorf("payment.amount_mismatch extras = %v / %v, want pi_wp_f / 4000", mis.Data["incoming_payment_intent_id"], mis.Data["captured_cents"])
	}
	if inv := mis.Data["invoice"].(map[string]any); flowStr(inv, "stripe_payment_intent_id") != "pi_wp_f" || inv["amount_paid_cents"] != float64(5000) {
		t.Errorf("payment.amount_mismatch invoice = PI %v, paid %v; want pi_wp_f and 5000 booked", inv["stripe_payment_intent_id"], inv["amount_paid_cents"])
	}

	// A charge in another currency: the event shows the invoice as it was
	// before Velox recorded the charge, then the payment settles as usual.
	idG := newInvoice("Other currency", 6000)
	before := getInvoice(idG)
	deliver(stripeEvent("evt_wp_g", "payment_intent.succeeded", piObject("pi_wp_g", idG, 6000, map[string]any{"status": "succeeded", "currency": "eur"})), "payment_intent.succeeded in EUR")
	got = events.next("other currency", domain.EventPaymentCurrencyMismatch, domain.EventInvoicePaid, domain.EventPaymentSucceeded, domain.EventDunningResolved)
	cur := got[domain.EventPaymentCurrencyMismatch]
	checkEventResource(t, "payment.currency_mismatch", cur, before)
	if cur.Data["incoming_payment_intent_id"] != "pi_wp_g" || cur.Data["captured_cents"] != float64(6000) {
		t.Errorf("payment.currency_mismatch extras = %v / %v, want pi_wp_g / 6000", cur.Data["incoming_payment_intent_id"], cur.Data["captured_cents"])
	}

	idC := newInvoice("Declined card", 7300)
	deliver(stripeEvent("evt_wp_c", "payment_intent.payment_failed", piObject("pi_wp_c", idC, 7300, map[string]any{
		"status": "requires_payment_method", "last_payment_error": map[string]any{"message": "Your card was declined."},
	})), "payment_intent.payment_failed")
	got = events.next("decline", domain.EventPaymentFailed)
	checkEventResource(t, "payment.failed", got[domain.EventPaymentFailed], getInvoice(idC))

	// A dispute on B's payment.
	opened := time.Now().Add(-time.Hour).Truncate(time.Second).UTC()
	deliver(stripeEvent("evt_wp_dispute", "charge.dispute.created", map[string]any{
		"id": "du_wp", "object": "dispute", "amount": 4500, "currency": "usd", "charge": "ch_wp",
		"payment_intent": "pi_wp_b", "reason": "fraudulent", "status": "needs_response", "created": opened.Unix(),
		"livemode": false, "metadata": map[string]any{}, "is_charge_refundable": false, "balance_transactions": []any{},
		"evidence_details": map[string]any{"due_by": opened.Add(7 * 24 * time.Hour).Unix(), "has_evidence": false, "past_due": false, "submission_count": 0},
	}), "charge.dispute.created")
	got = events.next("dispute", domain.EventPaymentDisputeOpened)
	opn := got[domain.EventPaymentDisputeOpened]
	checkEventResource(t, "payment.dispute_opened", opn, getInvoice(idB))
	disputes := flowList(call("GET", "/v1/invoices/"+idB+"/disputes", flowReq{}).want(t, 200, "list disputes").body, "data")
	if len(disputes) != 1 || !reflect.DeepEqual(opn.Data["dispute"], map[string]any(disputes[0])) {
		t.Errorf("payment.dispute_opened dispute = %v, want the API's %v", opn.Data["dispute"], disputes)
	}
	deliver(stripeEvent("evt_wp_dispute_closed", "charge.dispute.closed", map[string]any{
		"id": "du_wp", "object": "dispute", "amount": 4500, "currency": "usd", "charge": "ch_wp",
		"payment_intent": "pi_wp_b", "reason": "fraudulent", "status": "lost", "created": opened.Unix(),
		"livemode": false, "metadata": map[string]any{}, "is_charge_refundable": false, "balance_transactions": []any{},
		"evidence_details": map[string]any{"due_by": opened.Add(7 * 24 * time.Hour).Unix(), "has_evidence": false, "past_due": false, "submission_count": 0},
	}), "charge.dispute.closed")
	got = events.next("dispute closed", domain.EventPaymentDisputeClosed)
	cls := got[domain.EventPaymentDisputeClosed]
	checkEventResource(t, "payment.dispute_closed", cls, getInvoice(idB))
	disputes = flowList(call("GET", "/v1/invoices/"+idB+"/disputes", flowReq{}).want(t, 200, "list disputes").body, "data")
	if len(disputes) != 1 || !reflect.DeepEqual(cls.Data["dispute"], map[string]any(disputes[0])) || disputes[0]["status"] != "lost" {
		t.Errorf("payment.dispute_closed dispute = %v, want the API's lost dispute %v", cls.Data["dispute"], disputes)
	}

	// A Checkout that saved a card, then the same card saved through a
	// Velox setup session.
	deliver(stripeEvent("evt_wp_cs", "checkout.session.completed", map[string]any{
		"id": "cs_wp", "object": "checkout.session", "mode": "setup", "status": "complete",
		"customer": "cus_webhook_payload", "metadata": map[string]any{"velox_tenant_id": tenantID, "velox_customer_id": customerID},
	}), "checkout.session.completed")
	got = events.next("checkout card", domain.EventPaymentMethodUpdated)
	upd := got[domain.EventPaymentMethodUpdated]
	pms := flowList(call("GET", "/v1/customers/"+customerID+"/payment-methods", flowReq{}).want(t, 200, "list payment methods").body, "data")
	if len(pms) != 1 {
		t.Fatalf("payment methods after the checkout: %v, want 1", pms)
	}
	checkEventResource(t, "payment_method.updated", upd, pms[0])
	if upd.Data["stripe_customer_id"] != "cus_webhook_payload" {
		t.Errorf("payment_method.updated stripe_customer_id = %v, want cus_webhook_payload", upd.Data["stripe_customer_id"])
	}

	deliver(stripeEvent("evt_wp_seti", "setup_intent.succeeded", map[string]any{
		"id": "seti_wp", "object": "setup_intent", "status": "succeeded", "payment_method": "pm_flow",
		"customer": "cus_webhook_payload", "metadata": map[string]any{"velox_customer_id": customerID},
	}), "setup_intent.succeeded")
	got = events.next("setup intent", domain.EventPaymentMethodAttached)
	att := got[domain.EventPaymentMethodAttached]
	pms = flowList(call("GET", "/v1/customers/"+customerID+"/payment-methods", flowReq{}).want(t, 200, "list payment methods").body, "data")
	if len(pms) != 1 {
		t.Fatalf("payment methods: %v, want 1", pms)
	}
	checkEventResource(t, "payment_method.attached", att, pms[0])
	if att.Data["stripe_payment_method_id"] != "pm_flow" {
		t.Errorf("payment_method.attached stripe_payment_method_id = %v, want pm_flow", att.Data["stripe_payment_method_id"])
	}

	// The operator removes the card; then Stripe's events for it arrive late.
	// The card stays removed and is not in the API's list, so no
	// payment_method.* event names it.
	call("DELETE", "/v1/customers/"+customerID+"/payment-methods/"+flowStr(pms[0], "id"), flowReq{}).want(t, 200, "remove card")
	events.next("remove card")
	deliver(stripeEvent("evt_wp_seti_late", "setup_intent.succeeded", map[string]any{
		"id": "seti_wp_late", "object": "setup_intent", "status": "succeeded", "payment_method": "pm_flow",
		"customer": "cus_webhook_payload", "metadata": map[string]any{"velox_customer_id": customerID},
	}), "late setup_intent.succeeded")
	deliver(stripeEvent("evt_wp_cs_late", "checkout.session.completed", map[string]any{
		"id": "cs_wp_late", "object": "checkout.session", "mode": "setup", "status": "complete",
		"customer": "cus_webhook_payload", "metadata": map[string]any{"velox_tenant_id": tenantID, "velox_customer_id": customerID},
	}), "late checkout.session.completed")
	if late := events.next("late card events"); len(late) != 0 {
		t.Errorf("late events for a removed card queued %v, want none", late)
	}
	if pms = flowList(call("GET", "/v1/customers/"+customerID+"/payment-methods", flowReq{}).want(t, 200, "list payment methods").body, "data"); len(pms) != 0 {
		t.Errorf("payment methods after the late events: %v, want none", pms)
	}
	// Money for the voided invoice D.
	deliver(stripeEvent("evt_wp_d", "payment_intent.succeeded", piObject("pi_wp_d", idD, 3000, map[string]any{"status": "succeeded"})), "payment on a voided invoice")
	got = events.next("payment on a voided invoice", domain.EventPaymentReceivedOnVoidedInvoice)
	rov := got[domain.EventPaymentReceivedOnVoidedInvoice]
	checkEventResource(t, "payment.received_on_voided_invoice", rov, getInvoice(idD),
		"payment_anomaly_kind", "payment_anomaly_payment_intent_id", "payment_anomaly_captured_cents", "updated_at")
	if rov.Data["incoming_payment_intent_id"] != "pi_wp_d" || rov.Data["captured_cents"] != float64(3000) {
		t.Errorf("payment.received_on_voided_invoice extras = %v / %v, want pi_wp_d / 3000", rov.Data["incoming_payment_intent_id"], rov.Data["captured_cents"])
	}
	// D was never charged: no PaymentIntent, and not paid outside Velox
	// either. docs/webhooks.md says so.
	if inv := rov.Data["invoice"].(map[string]any); inv["stripe_payment_intent_id"] != nil || inv["paid_out_of_band"] != false {
		t.Errorf("payment.received_on_voided_invoice invoice = PI %v, paid_out_of_band %v; want none and false", inv["stripe_payment_intent_id"], inv["paid_out_of_band"])
	}
}

// TestE2E_WebhookPayloads_Subscription drives every operator-side
// subscription.* emitter through the API.
func TestE2E_WebhookPayloads_Subscription(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ts := httptest.NewServer(NewServer(db, clock.Real(), nil))
	defer ts.Close()

	tenantID := testutil.CreateTestTenant(t, db, "Webhook Payload Subs Co")
	key := createAPIKeyWithMode(t, db, tenantID, false)
	call := func(method, path string, req flowReq) flowResp {
		t.Helper()
		req.bearer = key
		return flowDo(t, ts, method, path, req)
	}
	events := &webhookCursor{t: t, db: db, tenantID: tenantID}

	plan := func(code string, cents int) string {
		return flowStr(call("POST", "/v1/plans", flowReq{body: map[string]any{
			"code": code, "name": code, "currency": "USD", "billing_interval": "monthly",
			"base_amount_cents": cents, "base_bill_timing": "in_arrears",
		}}).want(t, 201, "create plan "+code).body, "id")
	}
	starter, pro, extra := plan("wp-starter", 1000), plan("wp-pro", 2000), plan("wp-extra", 300)
	customerID := flowStr(call("POST", "/v1/customers", flowReq{body: map[string]any{
		"external_id": "wp-subs", "display_name": "Webhook Payload Subs",
	}}).want(t, 201, "create customer").body, "id")
	events.next("setup")

	getSub := func(id string) map[string]any {
		t.Helper()
		return call("GET", "/v1/subscriptions/"+id, flowReq{}).want(t, 200, "get subscription").body
	}
	// step makes one call and returns the one eventType event it queued.
	step := func(what, eventType string, method, path string, body any, status int) outboxEvent {
		t.Helper()
		call(method, path, flowReq{body: body}).want(t, status, what)
		return events.next(what, eventType)[eventType]
	}

	sub := call("POST", "/v1/subscriptions", flowReq{body: map[string]any{
		"code": "wp-sub", "display_name": "WP Sub", "customer_id": customerID,
		"items": []map[string]any{{"plan_id": starter, "quantity": 1}}, "billing_time": "anniversary",
	}}).want(t, 201, "create subscription").body
	subID := flowStr(sub, "id")
	base := "/v1/subscriptions/" + subID
	checkEventResource(t, "subscription.created", events.next("create", domain.EventSubscriptionCreated)[domain.EventSubscriptionCreated], getSub(subID))

	ev := step("schedule cancel", domain.EventSubscriptionCancelScheduled, "POST", base+"/schedule-cancel", map[string]any{"at_period_end": true}, 200)
	checkEventResource(t, "subscription.cancel_scheduled", ev, getSub(subID))
	ev = step("clear scheduled cancel", domain.EventSubscriptionCancelCleared, "DELETE", base+"/scheduled-cancel", nil, 200)
	checkEventResource(t, "subscription.cancel_cleared", ev, getSub(subID))
	ev = step("pause collection", domain.EventSubscriptionCollectionPaused, "PUT", base+"/pause-collection", map[string]any{"behavior": "keep_as_draft"}, 200)
	checkEventResource(t, "subscription.collection_paused", ev, getSub(subID))
	ev = step("resume collection", domain.EventSubscriptionCollectionResumed, "DELETE", base+"/pause-collection", nil, 200)
	checkEventResource(t, "subscription.collection_resumed", ev, getSub(subID))
	if ev.Data["triggered_by"] != "operator" {
		t.Errorf("subscription.collection_resumed triggered_by = %v, want operator", ev.Data["triggered_by"])
	}

	added := call("POST", base+"/items", flowReq{body: map[string]any{"plan_id": extra, "quantity": 2}}).want(t, 201, "add item").body
	addedID := flowStr(added, "id")
	ev = events.next("add item", domain.EventSubscriptionItemAdded)[domain.EventSubscriptionItemAdded]
	checkEventResource(t, "subscription.item.added", ev, getSub(subID))
	if ev.Data["item_id"] != addedID {
		t.Errorf("subscription.item.added item_id = %v, want %s", ev.Data["item_id"], addedID)
	}
	ev = step("change quantity", domain.EventSubscriptionItemUpdated, "PATCH", base+"/items/"+addedID, map[string]any{"quantity": 3}, 200)
	checkEventResource(t, "subscription.item.updated", ev, getSub(subID))
	if ev.Data["item_id"] != addedID {
		t.Errorf("subscription.item.updated item_id = %v, want %s", ev.Data["item_id"], addedID)
	}

	items, _ := sub["items"].([]any)
	itemID := flowStr(items[0].(map[string]any), "id")
	ev = step("schedule plan change", domain.EventSubscriptionPendingChangeScheduled, "PATCH", base+"/items/"+itemID, map[string]any{"new_plan_id": pro, "immediate": false}, 200)
	checkEventResource(t, "subscription.pending_change.scheduled", ev, getSub(subID))
	if ev.Data["item_id"] != itemID {
		t.Errorf("subscription.pending_change.scheduled item_id = %v, want %s", ev.Data["item_id"], itemID)
	}
	ev = step("cancel plan change", domain.EventSubscriptionPendingChangeCanceled, "DELETE", base+"/items/"+itemID+"/pending-change", nil, 200)
	checkEventResource(t, "subscription.pending_change.canceled", ev, getSub(subID))

	ev = step("remove item", domain.EventSubscriptionItemRemoved, "DELETE", base+"/items/"+addedID, nil, 204)
	checkEventResource(t, "subscription.item.removed", ev, getSub(subID))
	if ev.Data["item_id"] != addedID {
		t.Errorf("subscription.item.removed item_id = %v, want %s", ev.Data["item_id"], addedID)
	}

	// A plan change still scheduled at the cancel: the cancel clears it, and
	// the event shows the subscription without it, as the API does.
	step("schedule plan change before cancel", domain.EventSubscriptionPendingChangeScheduled, "PATCH", base+"/items/"+itemID, map[string]any{"new_plan_id": pro, "immediate": false}, 200)
	call("POST", base+"/cancel", flowReq{body: map[string]any{}}).want(t, 200, "cancel")
	got := events.next("cancel", domain.EventSubscriptionCanceled)
	ev = got[domain.EventSubscriptionCanceled]
	checkEventResource(t, "subscription.canceled", ev, getSub(subID))
	if ev.Data["canceled_by"] != "operator" {
		t.Errorf("subscription.canceled canceled_by = %v, want operator", ev.Data["canceled_by"])
	}

	// A trial: extended, then ended by the operator.
	trial := call("POST", "/v1/subscriptions", flowReq{body: map[string]any{
		"code": "wp-trial", "display_name": "WP Trial", "customer_id": customerID,
		"items": []map[string]any{{"plan_id": starter, "quantity": 1}}, "billing_time": "anniversary", "trial_days": 14,
	}}).want(t, 201, "create trial").body
	trialID := flowStr(trial, "id")
	events.next("create trial", domain.EventSubscriptionCreated)
	trialEnd, err := time.Parse(time.RFC3339, flowStr(trial, "trial_end_at"))
	if err != nil {
		t.Fatalf("trial_end_at %q: %v", flowStr(trial, "trial_end_at"), err)
	}
	ev = step("extend trial", domain.EventSubscriptionTrialExtended, "POST", "/v1/subscriptions/"+trialID+"/extend-trial",
		map[string]any{"trial_end": trialEnd.Add(7 * 24 * time.Hour).UTC().Format(time.RFC3339)}, 200)
	checkEventResource(t, "subscription.trial_extended", ev, getSub(trialID))
	if ev.Data["triggered_by"] != "operator" {
		t.Errorf("subscription.trial_extended triggered_by = %v, want operator", ev.Data["triggered_by"])
	}
	ev = step("end trial", domain.EventSubscriptionTrialEnded, "POST", "/v1/subscriptions/"+trialID+"/end-trial", nil, 200)
	checkEventResource(t, "subscription.trial_ended", ev, getSub(trialID))
	if ev.Data["triggered_by"] != "operator" || flowStr(ev.Data, "ended_at") == "" {
		t.Errorf("subscription.trial_ended extras = %v / %v, want operator and the end time", ev.Data["triggered_by"], ev.Data["ended_at"])
	}

	// A draft, activated.
	draft := call("POST", "/v1/subscriptions", flowReq{body: map[string]any{
		"code": "wp-draft", "display_name": "WP Draft", "customer_id": customerID, "status": "draft",
		"items": []map[string]any{{"plan_id": starter, "quantity": 1}}, "billing_time": "anniversary",
	}}).want(t, 201, "create draft").body
	draftID := flowStr(draft, "id")
	events.next("create draft", domain.EventSubscriptionCreated)
	ev = step("activate", domain.EventSubscriptionActivated, "POST", "/v1/subscriptions/"+draftID+"/activate", nil, 200)
	checkEventResource(t, "subscription.activated", ev, getSub(draftID))
}

// TestE2E_WebhookPayloads_CreditBalance drives the credit.balance_*
// emitters through grants and adjustments.
func TestE2E_WebhookPayloads_CreditBalance(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ts := httptest.NewServer(NewServer(db, clock.Real(), nil))
	defer ts.Close()

	tenantID := testutil.CreateTestTenant(t, db, "Webhook Payload Credit Co")
	key := createAPIKeyWithMode(t, db, tenantID, false)
	call := func(method, path string, body any) flowResp {
		t.Helper()
		return flowDo(t, ts, method, path, flowReq{bearer: key, body: body})
	}
	events := &webhookCursor{t: t, db: db, tenantID: tenantID}

	customerID := flowStr(call("POST", "/v1/customers", map[string]any{
		"external_id": "wp-credit", "display_name": "Webhook Payload Credit",
	}).want(t, 201, "create customer").body, "id")
	call("PUT", "/v1/settings", map[string]any{"credit_balance_low_threshold_cents": 1000}).want(t, 200, "set low threshold")
	events.next("setup")
	balance := func(currency string) map[string]any {
		t.Helper()
		for _, b := range flowList(call("GET", "/v1/credits/balance/"+customerID, nil).want(t, 200, "get balance").body, "data") {
			if flowStr(b, "currency") == currency {
				return b
			}
		}
		t.Fatalf("no %s balance", currency)
		return nil
	}

	call("POST", "/v1/credits/grant", map[string]any{
		"customer_id": customerID, "currency": "USD", "amount_cents": 5000, "description": "starter credit",
	}).want(t, 201, "grant")
	got := events.next("grant", domain.EventCreditBalanceRecovered)
	checkEventResource(t, "credit.balance_recovered", got[domain.EventCreditBalanceRecovered], balance("USD"))

	call("POST", "/v1/credits/adjust", map[string]any{
		"customer_id": customerID, "currency": "USD", "amount_cents": -4500, "description": "take most back",
	}).want(t, 201, "adjust down")
	got = events.next("adjust below the threshold", domain.EventCreditBalanceLow)
	low := got[domain.EventCreditBalanceLow]
	checkEventResource(t, "credit.balance_low", low, balance("USD"))
	if low.Data["threshold_cents"] != float64(1000) {
		t.Errorf("credit.balance_low threshold_cents = %v, want 1000", low.Data["threshold_cents"])
	}

	call("POST", "/v1/credits/adjust", map[string]any{
		"customer_id": customerID, "currency": "USD", "amount_cents": -500, "description": "the rest",
	}).want(t, 201, "adjust to zero")
	got = events.next("adjust to zero", domain.EventCreditBalanceDepleted)
	checkEventResource(t, "credit.balance_depleted", got[domain.EventCreditBalanceDepleted], balance("USD"))
}

// TestE2E_WebhookPayloads_SubscriptionSchedule drives the schedule-side
// subscription.* emitters (the billing engine on a test clock): a trial
// that ends, a plan change and a collection pause that come due, and a
// cancel at period end.
func TestE2E_WebhookPayloads_SubscriptionSchedule(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ts := httptest.NewServer(NewServer(db, clock.Real(), nil))
	defer ts.Close()

	tenantID := testutil.CreateTestTenant(t, db, "Webhook Payload Schedule Co")
	key := createAPIKeyWithMode(t, db, tenantID, false)
	call := func(method, path string, body any) flowResp {
		t.Helper()
		return flowDo(t, ts, method, path, flowReq{bearer: key, body: body})
	}
	events := &webhookCursor{t: t, db: db, tenantID: tenantID}

	plan := func(code string, cents int) string {
		return flowStr(call("POST", "/v1/plans", map[string]any{
			"code": code, "name": code, "currency": "USD", "billing_interval": "monthly",
			"base_amount_cents": cents, "base_bill_timing": "in_arrears",
		}).want(t, 201, "create plan "+code).body, "id")
	}
	starter, pro := plan("wps-starter", 1000), plan("wps-pro", 2000)
	start := time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC)
	clockID := flowStr(call("POST", "/v1/test-clocks", map[string]any{
		"name": "webhook payload schedule", "frozen_time": start.Format(time.RFC3339),
	}).want(t, 201, "create test clock").body, "id")
	customerID := flowStr(call("POST", "/v1/customers", map[string]any{
		"external_id": "wps-cust", "display_name": "Webhook Payload Schedule", "test_clock_id": clockID,
	}).want(t, 201, "create customer").body, "id")
	newSub := func(code string, extra map[string]any) map[string]any {
		body := map[string]any{
			"code": code, "display_name": code, "customer_id": customerID,
			"items": []map[string]any{{"plan_id": starter, "quantity": 1}}, "billing_time": "anniversary",
		}
		for k, v := range extra {
			body[k] = v
		}
		return call("POST", "/v1/subscriptions", body).want(t, 201, "create "+code).body
	}
	getSub := func(id string) map[string]any {
		t.Helper()
		return call("GET", "/v1/subscriptions/"+id, nil).want(t, 200, "get subscription").body
	}
	advanceTo := func(target time.Time) {
		t.Helper()
		call("POST", "/v1/test-clocks/"+clockID+"/advance", map[string]any{"frozen_time": target.Format(time.RFC3339)}).want(t, 200, "advance")
		deadline := time.Now().Add(60 * time.Second)
		for flowStr(call("GET", "/v1/test-clocks/"+clockID, nil).want(t, 200, "get clock").body, "status") != "ready" {
			if time.Now().After(deadline) {
				t.Fatal("clock advance did not settle")
			}
			time.Sleep(200 * time.Millisecond)
		}
	}

	trial := newSub("wps-trial", map[string]any{"trial_days": 7})
	trialCancel := newSub("wps-trial-cancel", map[string]any{"trial_days": 7})
	call("POST", "/v1/subscriptions/"+flowStr(trialCancel, "id")+"/schedule-cancel", map[string]any{"at_period_end": true}).want(t, 200, "schedule cancel in trial")
	changing := newSub("wps-change", nil)
	changeItem := flowStr(changing["items"].([]any)[0].(map[string]any), "id")
	call("PATCH", "/v1/subscriptions/"+flowStr(changing, "id")+"/items/"+changeItem,
		map[string]any{"new_plan_id": pro, "immediate": false}).want(t, 200, "schedule plan change")
	paused := newSub("wps-paused", nil)
	call("PUT", "/v1/subscriptions/"+flowStr(paused, "id")+"/pause-collection",
		map[string]any{"behavior": "keep_as_draft", "resumes_at": start.Add(20 * 24 * time.Hour).Format(time.RFC3339)}).want(t, 200, "pause collection")
	ending := newSub("wps-ending", nil)
	call("POST", "/v1/subscriptions/"+flowStr(ending, "id")+"/schedule-cancel", map[string]any{"at_period_end": true}).want(t, 200, "schedule cancel")
	events.next("setup")

	// Past the trial end.
	advanceTo(start.Add(8 * 24 * time.Hour))
	got := events.next("trial end", domain.EventSubscriptionTrialEnded, domain.EventSubscriptionCanceled)
	ev := got[domain.EventSubscriptionTrialEnded]
	checkEventResource(t, "subscription.trial_ended (schedule)", ev, getSub(flowStr(trial, "id")))
	if ev.Data["triggered_by"] != "schedule" {
		t.Errorf("subscription.trial_ended triggered_by = %v, want schedule", ev.Data["triggered_by"])
	}
	// The trial with a cancel scheduled ends canceled, with no trial_ended.
	ev = got[domain.EventSubscriptionCanceled]
	checkEventResource(t, "subscription.canceled (trial end)", ev, getSub(flowStr(trialCancel, "id")))
	if ev.Data["canceled_by"] != "schedule" || ev.Data["reason"] != "trial_end_cancel" {
		t.Errorf("subscription.canceled at trial end extras = %v / %v, want schedule / trial_end_cancel", ev.Data["canceled_by"], ev.Data["reason"])
	}

	// Past the first period end: the plan change applies, the pause's
	// resume date passed, the scheduled cancel fires.
	advanceTo(start.AddDate(0, 1, 1))
	got = events.next("period end", domain.EventSubscriptionPendingChangeApplied,
		domain.EventSubscriptionCollectionResumed, domain.EventSubscriptionCanceled)
	ev = got[domain.EventSubscriptionPendingChangeApplied]
	// The change applies at the period end, before the close that starts
	// the next period: the event shows the period that just ended.
	checkEventResource(t, "subscription.pending_change.applied", ev, getSub(flowStr(changing, "id")),
		"current_billing_period_start", "current_billing_period_end", "next_billing_at", "updated_at")
	if ev.Data["item_id"] != changeItem || ev.Data["old_plan_id"] != starter || flowStr(ev.Data, "applied_at") == "" {
		t.Errorf("subscription.pending_change.applied extras = %v / %v / %v, want %s / %s / a time",
			ev.Data["item_id"], ev.Data["old_plan_id"], ev.Data["applied_at"], changeItem, starter)
	}
	ev = got[domain.EventSubscriptionCollectionResumed]
	checkEventResource(t, "subscription.collection_resumed (schedule)", ev, getSub(flowStr(paused, "id")))
	if ev.Data["triggered_by"] != "schedule" || flowStr(ev.Data, "resumed_at") == "" {
		t.Errorf("subscription.collection_resumed extras = %v / %v, want schedule and the resume time", ev.Data["triggered_by"], ev.Data["resumed_at"])
	}
	ev = got[domain.EventSubscriptionCanceled]
	checkEventResource(t, "subscription.canceled (schedule)", ev, getSub(flowStr(ending, "id")))
	if ev.Data["canceled_by"] != "schedule" {
		t.Errorf("subscription.canceled canceled_by = %v, want schedule", ev.Data["canceled_by"])
	}
}

// TestE2E_WebhookPayloads_EngineAndDunning drives the engine's and dunning's
// emitters on a test clock: the day-1 invoice of a subscription billed in
// advance, its dunning run exhausting into a cancel and a write-off, and a
// cycle close.
func TestE2E_WebhookPayloads_EngineAndDunning(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ts := httptest.NewServer(NewServer(db, clock.Real(), nil))
	defer ts.Close()

	tenantID := testutil.CreateTestTenant(t, db, "Webhook Payload Engine Co")
	key := createAPIKeyWithMode(t, db, tenantID, false)
	call := func(method, path string, body any) flowResp {
		t.Helper()
		return flowDo(t, ts, method, path, flowReq{bearer: key, body: body})
	}
	events := &webhookCursor{t: t, db: db, tenantID: tenantID}

	call("POST", "/v1/dunning/policies", map[string]any{
		"name": "One retry", "enabled": true, "retry_days": []int{1},
		"final_subscription_action": "cancel", "final_invoice_action": "mark_uncollectible",
	}).want(t, 201, "create dunning policy")
	plan := func(code, timing string) string {
		return flowStr(call("POST", "/v1/plans", map[string]any{
			"code": code, "name": code, "currency": "USD", "billing_interval": "monthly",
			"base_amount_cents": 1000, "base_bill_timing": timing,
		}).want(t, 201, "create plan "+code).body, "id")
	}
	advancePlan, arrearsPlan := plan("wpe-advance", "in_advance"), plan("wpe-arrears", "in_arrears")
	start := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	clockID := flowStr(call("POST", "/v1/test-clocks", map[string]any{
		"name": "webhook payload engine", "frozen_time": start.Format(time.RFC3339),
	}).want(t, 201, "create test clock").body, "id")
	newSub := func(code, planID string) string {
		t.Helper()
		customerID := flowStr(call("POST", "/v1/customers", map[string]any{
			"external_id": code, "display_name": code, "email": code + "@payload.test", "test_clock_id": clockID,
		}).want(t, 201, "create customer "+code).body, "id")
		return flowStr(call("POST", "/v1/subscriptions", map[string]any{
			"code": code, "display_name": code, "customer_id": customerID,
			"items": []map[string]any{{"plan_id": planID, "quantity": 1}}, "billing_time": "anniversary",
		}).want(t, 201, "create "+code).body, "id")
	}
	getSub := func(id string) map[string]any {
		t.Helper()
		return call("GET", "/v1/subscriptions/"+id, nil).want(t, 200, "get subscription").body
	}
	getInvoice := func(id string) map[string]any {
		t.Helper()
		return flowObj(call("GET", "/v1/invoices/"+id, nil).want(t, 200, "get invoice").body, "invoice")
	}
	getRun := func(invoiceID string) map[string]any {
		t.Helper()
		runs := flowList(call("GET", "/v1/dunning/runs?invoice_id="+invoiceID, nil).want(t, 200, "list dunning runs").body, "data")
		if len(runs) != 1 {
			t.Fatalf("invoice %s has %d dunning runs, want 1", invoiceID, len(runs))
		}
		return flowObj(call("GET", "/v1/dunning/runs/"+flowStr(runs[0], "id"), nil).want(t, 200, "get dunning run").body, "run")
	}
	advanceTo := func(target time.Time) {
		t.Helper()
		call("POST", "/v1/test-clocks/"+clockID+"/advance", map[string]any{"frozen_time": target.Format(time.RFC3339)}).want(t, 200, "advance")
		deadline := time.Now().Add(60 * time.Second)
		for flowStr(call("GET", "/v1/test-clocks/"+clockID, nil).want(t, 200, "get clock").body, "status") != "ready" {
			if time.Now().After(deadline) {
				t.Fatal("clock advance did not settle")
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	events.next("setup")

	// The day-1 invoice of a subscription billed in advance.
	advanceSub := newSub("wpe-advance", advancePlan)
	got := events.next("day-1 invoice", domain.EventSubscriptionCreated, domain.EventInvoiceFinalized, domain.EventDunningStarted)
	dayOne := got[domain.EventInvoiceFinalized]
	dayOneID := flowStr(dayOne.Data["invoice"].(map[string]any), "id")
	checkEventResource(t, "invoice.finalized (day 1)", dayOne, getInvoice(dayOneID))
	checkEventResource(t, "dunning.started (day 1)", got[domain.EventDunningStarted], getRun(dayOneID))

	// Its one retry fails: dunning cancels the subscription and writes the
	// invoice off.
	advanceTo(start.Add(2 * 24 * time.Hour))
	got = events.next("dunning exhausted", domain.EventDunningEscalated, domain.EventSubscriptionCanceled, domain.EventInvoiceMarkedUncollectible)
	esc := got[domain.EventDunningEscalated]
	checkEventResource(t, "dunning.escalated", esc, getRun(dayOneID))
	if esc.Data["final_subscription_action"] != "cancel" || esc.Data["final_invoice_action"] != "mark_uncollectible" {
		t.Errorf("dunning.escalated actions = %v / %v, want cancel / mark_uncollectible", esc.Data["final_subscription_action"], esc.Data["final_invoice_action"])
	}
	checkEventResource(t, "subscription.canceled (dunning)", got[domain.EventSubscriptionCanceled], getSub(advanceSub))
	checkEventResource(t, "invoice.marked_uncollectible (dunning)", got[domain.EventInvoiceMarkedUncollectible], getInvoice(dayOneID))

	// A cycle close bills a subscription in arrears.
	newSub("wpe-arrears", arrearsPlan)
	events.next("create arrears subscription", domain.EventSubscriptionCreated)
	advanceTo(start.AddDate(0, 1, 0).Add(2 * 24 * time.Hour))
	got = events.next("cycle close", domain.EventInvoiceFinalized)
	cycle := got[domain.EventInvoiceFinalized]
	checkEventResource(t, "invoice.finalized (cycle)", cycle, getInvoice(flowStr(cycle.Data["invoice"].(map[string]any), "id")))
}
