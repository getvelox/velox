package domain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// The payload rule for outbound webhooks (ADR-131).
//
// An event's data holds the resource the event is about, under the
// resource's name, in the JSON shape the API returns for it, as it stood
// when the event was created. Every event of a family carries the same
// resource. The few facts the resource does not hold (who did it, the
// charge that raised an anomaly, the dispute) sit beside it as named keys,
// listed per event in WebhookEventExtras. Nothing else is in data.
//
// Fields the API works out at read time for the dashboard are left out:
// they describe the moment of the read, not the event (see the *WebhookData
// builders below for which).

// Resource keys: the name each family's resource has in an event's data.
const (
	WebhookResourceInvoice       = "invoice"
	WebhookResourceSubscription  = "subscription"
	WebhookResourceCustomer      = "customer"
	WebhookResourcePaymentMethod = "payment_method"
	WebhookResourceDunningRun    = "dunning_run"
	WebhookResourceCreditBalance = "credit_balance"
)

// webhookFamilyResource maps an event family (the event type before its
// first dot) to the resource every event of the family carries. payment.*
// carries the invoice the payment was for: Velox has no payment object.
var webhookFamilyResource = map[string]string{
	"invoice":        WebhookResourceInvoice,
	"payment":        WebhookResourceInvoice,
	"payment_method": WebhookResourcePaymentMethod,
	"subscription":   WebhookResourceSubscription,
	"customer":       WebhookResourceCustomer,
	"dunning":        WebhookResourceDunningRun,
	"credit":         WebhookResourceCreditBalance,
}

// WebhookEventFamily returns an event type's family: the part before the
// first dot ("subscription" for "subscription.item.added").
func WebhookEventFamily(eventType string) string {
	family, _, _ := strings.Cut(eventType, ".")
	return family
}

// WebhookEventResource returns the resource key an event type's data
// carries, or "" for a type outside every family.
func WebhookEventResource(eventType string) string {
	return webhookFamilyResource[WebhookEventFamily(eventType)]
}

// WebhookEventExtras lists, for every event type in the catalog, the keys
// its data may carry beside the resource. An empty list means the event
// carries the resource alone. docs/webhooks.md shows the same list per
// event; webhook_catalog_test.go fails when the two differ.
var WebhookEventExtras = map[string][]string{
	EventInvoiceFinalized:           {},
	EventInvoicePaid:                {},
	EventInvoiceVoided:              {},
	EventInvoiceMarkedUncollectible: {},
	// recovered_from: the status the invoice was paid from (finalized or
	// uncollectible). The invoice itself now says paid.
	EventInvoicePaymentRecorded: {"recovered_from"},

	EventPaymentSucceeded: {},
	EventPaymentFailed:    {},
	// The charge that raised the anomaly. The invoice's
	// stripe_payment_intent_id is not always another charge: on
	// amount_mismatch it is this one (docs/webhooks.md, per event).
	EventPaymentDuplicateCharge:         {"incoming_payment_intent_id", "captured_cents"},
	EventPaymentAmountMismatch:          {"incoming_payment_intent_id", "captured_cents"},
	EventPaymentCurrencyMismatch:        {"incoming_payment_intent_id", "captured_cents"},
	EventPaymentReceivedOnVoidedInvoice: {"incoming_payment_intent_id", "captured_cents"},
	EventPaymentDisputeOpened:           {"dispute"},
	EventPaymentDisputeClosed:           {"dispute"},

	EventPaymentMethodAttached: {"stripe_payment_method_id"},
	EventPaymentMethodUpdated:  {"stripe_customer_id"},

	EventSubscriptionCreated:   {},
	EventSubscriptionActivated: {},
	// reason is set only on a cancel at trial end (trial_end_cancel).
	EventSubscriptionCanceled:               {"canceled_by", "reason"},
	EventSubscriptionTrialEnded:             {"ended_at", "triggered_by"},
	EventSubscriptionTrialExtended:          {"triggered_by"},
	EventSubscriptionCancelScheduled:        {},
	EventSubscriptionCancelCleared:          {},
	EventSubscriptionCollectionPaused:       {},
	EventSubscriptionCollectionResumed:      {"triggered_by", "resumed_at"},
	EventSubscriptionItemAdded:              {"item_id"},
	EventSubscriptionItemUpdated:            {"item_id"},
	EventSubscriptionItemRemoved:            {"item_id"},
	EventSubscriptionPendingChangeScheduled: {"item_id"},
	EventSubscriptionPendingChangeApplied:   {"item_id", "old_plan_id", "applied_at"},
	EventSubscriptionPendingChangeCanceled:  {"item_id"},
	EventSubscriptionThresholdCrossed:       {"invoice_id", "running_subtotal", "crossed_amount", "crossed_item_id"},

	EventCustomerEmailBounced:    {"reason"},
	EventCustomerEmailComplained: {"reason"},

	EventDunningStarted:   {},
	EventDunningEscalated: {"final_subscription_action", "final_invoice_action", "applied"},
	EventDunningResolved:  {},

	EventCreditBalanceLow:       {"threshold_cents"},
	EventCreditBalanceDepleted:  {},
	EventCreditBalanceRecovered: {},
	EventCreditCommitRetired: {
		"grant_id", "funding_invoice_id", "credit_note_id",
		"retired_cents", "refunded_gross_cents", "remaining_after_cents",
	},
}

// InvoiceWebhookData is the data of an invoice.* or payment.* event: the
// invoice as GET /v1/invoices/{id} returns it under "invoice", plus the
// event's extras. Left out: attention, recovery_warning,
// billing_period_display and test_clock_id, which the API works out per read.
func InvoiceWebhookData(inv Invoice, extra map[string]any) map[string]any {
	inv.Attention = nil
	inv.RecoveryWarning = nil
	inv.BillingPeriodDisplay = ""
	inv.TestClockID = ""
	return webhookData(WebhookResourceInvoice, inv, extra)
}

// SubscriptionWebhookData is the data of a subscription.* event: the
// subscription with its items, as GET /v1/subscriptions/{id} returns it,
// plus the event's extras. Left out: current_billing_period_display, which
// the API renders per read in the tenant's timezone.
func SubscriptionWebhookData(sub Subscription, extra map[string]any) map[string]any {
	sub.CurrentBillingPeriodDisplay = ""
	return webhookData(WebhookResourceSubscription, sub, extra)
}

// CustomerWebhookData is the data of a customer.* event: the customer as
// GET /v1/customers/{id} returns it, plus the event's extras.
func CustomerWebhookData(c Customer, extra map[string]any) map[string]any {
	return webhookData(WebhookResourceCustomer, c, extra)
}

// PaymentMethodWebhookData is the data of a payment_method.* event: the
// payment method as GET /v1/customers/{id}/payment-methods lists it, plus
// the event's extras.
func PaymentMethodWebhookData(pm PaymentMethodView, extra map[string]any) map[string]any {
	return webhookData(WebhookResourcePaymentMethod, pm, extra)
}

// DunningRunWebhookData is the data of a dunning.* event: the run as
// GET /v1/dunning/runs/{id} returns it under "run", plus the event's extras.
// Left out: the invoice and policy figures the run lists join in.
func DunningRunWebhookData(run InvoiceDunningRun, extra map[string]any) map[string]any {
	run.InvoiceNumber = ""
	run.InvoiceAmountDue = 0
	run.InvoiceCurrency = ""
	run.EffectiveNow = nil
	run.TestClockID = ""
	run.MaxRetryAttempts = 0
	return webhookData(WebhookResourceDunningRun, run, extra)
}

// CreditBalanceWebhookData is the data of a credit.* event: the customer's
// balance in the event's currency, as GET /v1/credits/balance/{customer_id}
// lists it, plus the event's extras.
func CreditBalanceWebhookData(b CreditBalance, extra map[string]any) map[string]any {
	return webhookData(WebhookResourceCreditBalance, b, extra)
}

// PaymentMethodView is a saved payment method as the API returns it (the
// rows of GET /v1/customers/{id}/payment-methods) and as payment_method.*
// events carry it.
type PaymentMethodView struct {
	ID         string    `json:"id"`
	CustomerID string    `json:"customer_id"`
	Type       string    `json:"type"`
	CardBrand  string    `json:"card_brand,omitempty"`
	CardLast4  string    `json:"card_last4,omitempty"`
	CardExpMo  int       `json:"card_exp_month,omitempty"`
	CardExpYr  int       `json:"card_exp_year,omitempty"`
	IsDefault  bool      `json:"is_default"`
	CreatedAt  time.Time `json:"created_at"`
}

// webhookData builds {resourceKey: resource, extras...} as plain JSON
// values, the form the outbox stores and the dispatcher sends, so an event
// read back from the database equals the one built here. Numbers stay
// json.Number, so no amount passes through a float. A value that cannot be
// encoded is kept as it is, and the outbox enqueue then fails with the
// encode error, so nothing is dropped quietly.
func webhookData(resourceKey string, resource any, extra map[string]any) map[string]any {
	data := make(map[string]any, len(extra)+1)
	for k, v := range extra {
		data[k] = v
	}
	data[resourceKey] = resource
	raw, err := json.Marshal(data)
	if err != nil {
		return data
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var plain map[string]any
	if err := dec.Decode(&plain); err != nil {
		return data
	}
	return plain
}

// WebhookEventCustomerID returns the customer an event is about: the
// customer_id of its resource (the id, for a customer.* event). Events
// stored before ADR-131 carried customer_id at the top of data; they are
// read too.
func WebhookEventCustomerID(eventType string, data map[string]any) string {
	if v, ok := data["customer_id"].(string); ok {
		return v
	}
	key := WebhookEventResource(eventType)
	res, ok := data[key].(map[string]any)
	if !ok {
		return ""
	}
	field := "customer_id"
	if key == WebhookResourceCustomer {
		field = "id"
	}
	v, _ := res[field].(string)
	return v
}

// CheckWebhookData reports how data breaks the payload rule for eventType:
// a missing or non-object resource, or a key the event does not list.
// Tests call it on what each emitter queues.
func CheckWebhookData(eventType string, data map[string]any) error {
	key := WebhookEventResource(eventType)
	if key == "" {
		return fmt.Errorf("%s: no webhook family", eventType)
	}
	extras, ok := WebhookEventExtras[eventType]
	if !ok {
		return fmt.Errorf("%s: not in WebhookEventExtras", eventType)
	}
	if _, ok := data[key].(map[string]any); !ok {
		return fmt.Errorf("%s: data.%s is missing or not an object (keys %v)", eventType, key, sortedDataKeys(data))
	}
	allowed := map[string]bool{key: true}
	for _, e := range extras {
		allowed[e] = true
	}
	var unknown []string
	for k := range data {
		if !allowed[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return fmt.Errorf("%s: data has keys the event does not list: %v", eventType, unknown)
	}
	return nil
}

func sortedDataKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
