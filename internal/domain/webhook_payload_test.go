package domain

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// go test ./internal/domain -run TestWebhookPayloadGolden -update rewrites
// the goldens. Review the diff: every change is a change to a public
// contract.
var updateGolden = flag.Bool("update", false, "rewrite the webhook payload goldens")

var goldenAt = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func at(d time.Duration) *time.Time {
	t := goldenAt.Add(d)
	return &t
}

// The fixed resources every golden is rendered from. Read-time fields are
// set on purpose (attention, billing_period_display, test_clock_id, the
// run's joined figures) so the goldens show they are left out.
func goldenInvoice() Invoice {
	return Invoice{
		ID:             "vlx_inv_golden",
		TenantID:       "vlx_ten_golden",
		CustomerID:     "vlx_cus_golden",
		SubscriptionID: "vlx_sub_golden",
		InvoiceNumber:  "VLX-000042",
		Status:         InvoicePaid,
		PaymentStatus:  PaymentSucceeded,
		Currency:       "USD",
		SubtotalCents:  10000,
		TaxFacts: TaxFacts{
			TaxAmountCents: 825, TaxRate: 8.25, TaxName: "Sales tax", TaxCountry: "US",
			TaxProvider: "manual", TaxStatus: InvoiceTaxOK,
		},
		TotalAmountCents:      10825,
		AmountDueCents:        0,
		AmountPaidCents:       10825,
		CreditsAppliedCents:   0,
		BillingPeriodStart:    goldenAt.Add(-30 * 24 * time.Hour),
		BillingPeriodEnd:      goldenAt,
		BillingTimezone:       "UTC",
		BillingPeriodDisplay:  "Sep 1, 2026 – Sep 30, 2026",
		IssuedAt:              at(0),
		DueAt:                 at(0),
		PaidAt:                at(time.Minute),
		StripePaymentIntentID: "pi_golden",
		PaymentCardBrand:      "visa",
		PaymentCardLast4:      "4242",
		NetPaymentTermDays:    0,
		Metadata:              map[string]any{"order": "A-1"},
		BillingReason:         BillingReasonSubscriptionCycle,
		Attention:             &Attention{Severity: "warning", Reason: "overdue", Message: "read-time only"},
		RecoveryWarning:       &PaymentBlock{Code: "read_time_only"},
		TestClockID:           "vlx_tclk_read_time_only",
		CreatedAt:             goldenAt,
		UpdatedAt:             goldenAt.Add(time.Minute),
	}
}

func goldenSubscription() Subscription {
	return Subscription{
		ID:                          "vlx_sub_golden",
		TenantID:                    "vlx_ten_golden",
		Code:                        "acme-pro",
		DisplayName:                 "Acme Pro",
		CustomerID:                  "vlx_cus_golden",
		Status:                      SubscriptionActive,
		BillingTime:                 BillingTimeCalendar,
		StartedAt:                   at(-30 * 24 * time.Hour),
		ActivatedAt:                 at(-30 * 24 * time.Hour),
		CurrentBillingPeriodStart:   at(0),
		CurrentBillingPeriodEnd:     at(31 * 24 * time.Hour),
		CurrentBillingPeriodDisplay: "read-time only",
		NextBillingAt:               at(31 * 24 * time.Hour),
		BillingThresholds: &BillingThresholds{
			AmountGTE:      50000,
			ItemThresholds: []SubscriptionItemThreshold{},
		},
		CreatedAt: goldenAt.Add(-30 * 24 * time.Hour),
		UpdatedAt: goldenAt,
		Items: []SubscriptionItem{{
			ID:                     "vlx_si_golden",
			TenantID:               "vlx_ten_golden",
			SubscriptionID:         "vlx_sub_golden",
			PlanID:                 "vlx_pln_pro",
			Quantity:               3,
			PendingPlanID:          "vlx_pln_team",
			PendingPlanEffectiveAt: at(31 * 24 * time.Hour),
			CreatedAt:              goldenAt.Add(-30 * 24 * time.Hour),
			UpdatedAt:              goldenAt,
		}},
	}
}

func goldenCustomer() Customer {
	return Customer{
		ID:                 "vlx_cus_golden",
		TenantID:           "vlx_ten_golden",
		ExternalID:         "acme-1",
		DisplayName:        "Acme Inc",
		Email:              "billing@acme.example",
		Status:             CustomerStatusActive,
		EmailStatus:        EmailStatusBounced,
		EmailLastBouncedAt: at(0),
		EmailBounceReason:  "mailbox does not exist",
		StripeCustomerID:   "cus_golden",
		CreatedAt:          goldenAt.Add(-60 * 24 * time.Hour),
		UpdatedAt:          goldenAt,
	}
}

func goldenPaymentMethod() PaymentMethodView {
	return PaymentMethodView{
		ID: "vlx_pm_golden", CustomerID: "vlx_cus_golden", Type: "card",
		CardBrand: "visa", CardLast4: "4242", CardExpMo: 12, CardExpYr: 2030,
		IsDefault: true, CreatedAt: goldenAt,
	}
}

func goldenDunningRun() InvoiceDunningRun {
	return InvoiceDunningRun{
		ID:               "vlx_dr_golden",
		TenantID:         "vlx_ten_golden",
		InvoiceID:        "vlx_inv_golden",
		CustomerID:       "vlx_cus_golden",
		PolicyID:         "vlx_dpol_golden",
		State:            DunningEscalated,
		Reason:           "payment_failed",
		AttemptCount:     4,
		LastAttemptAt:    at(-time.Hour),
		ResolvedAt:       at(0),
		Resolution:       ResolutionRetriesExhausted,
		CreatedAt:        goldenAt.Add(-14 * 24 * time.Hour),
		UpdatedAt:        goldenAt,
		InvoiceNumber:    "read-time only",
		InvoiceAmountDue: 999,
		InvoiceCurrency:  "XXX",
		EffectiveNow:     at(0),
		TestClockID:      "vlx_tclk_read_time_only",
		MaxRetryAttempts: 4,
	}
}

func goldenCreditBalance() CreditBalance {
	return CreditBalance{
		CustomerID: "vlx_cus_golden", Currency: "USD", BalanceCents: 900,
		TotalGranted: 10000, TotalUsed: 8000, TotalExpired: 100, TotalRetired: 500, TotalDeducted: 500,
	}
}

func goldenDispute() InvoiceDispute {
	return InvoiceDispute{
		ID: "vlx_dsp_golden", InvoiceID: "vlx_inv_golden", StripeDisputeID: "du_golden",
		StripeChargeID: "ch_golden", StripePaymentIntentID: "pi_golden", Status: DisputeStatus("needs_response"),
		Reason: "fraudulent", AmountCents: 10825, Currency: "USD", EvidenceDueBy: at(7 * 24 * time.Hour),
		OpenedAt: goldenAt, Livemode: false, CreatedAt: goldenAt, UpdatedAt: goldenAt,
	}
}

// goldenWebhookData renders one event type's data from the fixed resources,
// with the extras its emitter sets filled with fixed values.
func goldenWebhookData(t *testing.T, eventType string) map[string]any {
	t.Helper()
	anomaly := map[string]any{"incoming_payment_intent_id": "pi_second", "captured_cents": int64(10825)}
	switch eventType {
	case EventInvoiceFinalized, EventInvoicePaid, EventInvoiceVoided, EventInvoiceMarkedUncollectible,
		EventPaymentSucceeded, EventPaymentFailed:
		return InvoiceWebhookData(goldenInvoice(), nil)
	case EventInvoicePaymentRecorded:
		return InvoiceWebhookData(goldenInvoice(), map[string]any{"recovered_from": string(InvoiceUncollectible)})
	case EventPaymentDuplicateCharge, EventPaymentAmountMismatch, EventPaymentCurrencyMismatch, EventPaymentReceivedOnVoidedInvoice:
		return InvoiceWebhookData(goldenInvoice(), anomaly)
	case EventPaymentDisputeOpened, EventPaymentDisputeClosed:
		return InvoiceWebhookData(goldenInvoice(), map[string]any{"dispute": goldenDispute()})
	case EventPaymentMethodAttached:
		return PaymentMethodWebhookData(goldenPaymentMethod(), map[string]any{"stripe_payment_method_id": "pm_golden"})
	case EventPaymentMethodUpdated:
		return PaymentMethodWebhookData(goldenPaymentMethod(), map[string]any{"stripe_customer_id": "cus_golden"})
	case EventSubscriptionCreated, EventSubscriptionActivated, EventSubscriptionCancelScheduled,
		EventSubscriptionCancelCleared, EventSubscriptionCollectionPaused:
		return SubscriptionWebhookData(goldenSubscription(), nil)
	case EventSubscriptionCanceled:
		return SubscriptionWebhookData(goldenSubscription(), map[string]any{"canceled_by": "schedule", "reason": "trial_end_cancel"})
	case EventSubscriptionTrialEnded:
		return SubscriptionWebhookData(goldenSubscription(), map[string]any{"ended_at": goldenAt, "triggered_by": "schedule"})
	case EventSubscriptionTrialExtended:
		return SubscriptionWebhookData(goldenSubscription(), map[string]any{"triggered_by": "operator"})
	case EventSubscriptionCollectionResumed:
		return SubscriptionWebhookData(goldenSubscription(), map[string]any{"triggered_by": "schedule", "resumed_at": goldenAt})
	case EventSubscriptionItemAdded, EventSubscriptionItemUpdated, EventSubscriptionItemRemoved,
		EventSubscriptionPendingChangeScheduled, EventSubscriptionPendingChangeCanceled:
		return SubscriptionWebhookData(goldenSubscription(), map[string]any{"item_id": "vlx_si_golden"})
	case EventSubscriptionPendingChangeApplied:
		return SubscriptionWebhookData(goldenSubscription(), map[string]any{
			"item_id": "vlx_si_golden", "old_plan_id": "vlx_pln_basic", "applied_at": goldenAt,
		})
	case EventSubscriptionThresholdCrossed:
		return SubscriptionWebhookData(goldenSubscription(), map[string]any{
			"invoice_id": "vlx_inv_golden", "running_subtotal": int64(50100), "crossed_amount": true, "crossed_item_id": "",
		})
	case EventCustomerEmailBounced, EventCustomerEmailComplained:
		return CustomerWebhookData(goldenCustomer(), map[string]any{"reason": "mailbox does not exist"})
	case EventDunningStarted, EventDunningResolved:
		return DunningRunWebhookData(goldenDunningRun(), nil)
	case EventDunningEscalated:
		return DunningRunWebhookData(goldenDunningRun(), map[string]any{
			"final_subscription_action": "cancel", "final_invoice_action": "mark_uncollectible",
			"applied": "canceled_and_written_off",
		})
	case EventCreditBalanceDepleted, EventCreditBalanceRecovered:
		return CreditBalanceWebhookData(goldenCreditBalance(), nil)
	case EventCreditBalanceLow:
		return CreditBalanceWebhookData(goldenCreditBalance(), map[string]any{"threshold_cents": int64(1000)})
	case EventCreditCommitRetired:
		return CreditBalanceWebhookData(goldenCreditBalance(), map[string]any{
			"grant_id": "vlx_ccl_golden", "funding_invoice_id": "vlx_inv_commit", "credit_note_id": "vlx_cn_golden",
			"retired_cents": int64(500), "refunded_gross_cents": int64(500), "remaining_after_cents": int64(0),
		})
	}
	t.Fatalf("no golden fixture for %s: add one", eventType)
	return nil
}

// TestWebhookPayloadGolden renders every catalog event's data from fixed
// resources and compares it with the checked-in golden, so any change to a
// payload is a visible diff. Each golden also passes CheckWebhookData.
func TestWebhookPayloadGolden(t *testing.T) {
	dir := filepath.Join("testdata", "webhook_payloads")
	if *updateGolden {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, eventType := range sortedKeys(KnownWebhookEventTypes) {
		t.Run(eventType, func(t *testing.T) {
			data := goldenWebhookData(t, eventType)
			if err := CheckWebhookData(eventType, data); err != nil {
				t.Fatalf("golden data breaks the payload rule: %v", err)
			}
			got, err := json.MarshalIndent(data, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, '\n')
			path := filepath.Join(dir, eventType+".json")
			if *updateGolden {
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden (run with -update to create it): %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("%s data differs from %s (run with -update and review the diff)\n--- got\n%s", eventType, path, got)
			}
		})
	}
}

// TestWebhookPayloadGolden_NoStrayGoldens: a golden for an event the
// catalog no longer has would document a payload nobody sends.
func TestWebhookPayloadGolden_NoStrayGoldens(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join("testdata", "webhook_payloads"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		eventType := name[:len(name)-len(filepath.Ext(name))]
		if !KnownWebhookEventTypes[eventType] {
			t.Errorf("golden %s is for %q, which is not in the catalog", name, eventType)
		}
	}
}

// TestWebhookData_LeavesOutReadTimeFields pins what the builders drop:
// fields the API works out per read describe the read, not the event.
func TestWebhookData_LeavesOutReadTimeFields(t *testing.T) {
	inv := InvoiceWebhookData(goldenInvoice(), nil)["invoice"].(map[string]any)
	for _, k := range []string{"attention", "recovery_warning", "billing_period_display", "test_clock_id"} {
		if _, ok := inv[k]; ok {
			t.Errorf("invoice resource carries read-time field %q", k)
		}
	}
	sub := SubscriptionWebhookData(goldenSubscription(), nil)["subscription"].(map[string]any)
	if _, ok := sub["current_billing_period_display"]; ok {
		t.Error("subscription resource carries current_billing_period_display")
	}
	run := DunningRunWebhookData(goldenDunningRun(), nil)["dunning_run"].(map[string]any)
	for _, k := range []string{"invoice_number", "invoice_amount_due_cents", "invoice_currency", "effective_now", "test_clock_id", "max_retry_attempts"} {
		if _, ok := run[k]; ok {
			t.Errorf("dunning run resource carries list-read field %q", k)
		}
	}
}

// TestWebhookData_KeepsLargeAmountsExact: amounts are json.Number in the
// built data, so a large cents figure does not pass through a float64.
func TestWebhookData_KeepsLargeAmountsExact(t *testing.T) {
	inv := goldenInvoice()
	inv.TotalAmountCents = 9_007_199_254_740_993 // 2^53 + 1: not exact as a float64
	data := InvoiceWebhookData(inv, nil)
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"total_amount_cents":9007199254740993`)) {
		t.Errorf("total_amount_cents lost precision: %s", raw)
	}
}

func TestWebhookEventCustomerID(t *testing.T) {
	cases := []struct {
		eventType string
		data      map[string]any
		want      string
	}{
		{EventInvoicePaid, InvoiceWebhookData(goldenInvoice(), nil), "vlx_cus_golden"},
		{EventSubscriptionCreated, SubscriptionWebhookData(goldenSubscription(), nil), "vlx_cus_golden"},
		{EventCustomerEmailBounced, CustomerWebhookData(goldenCustomer(), nil), "vlx_cus_golden"},
		{EventPaymentMethodAttached, PaymentMethodWebhookData(goldenPaymentMethod(), nil), "vlx_cus_golden"},
		{EventDunningStarted, DunningRunWebhookData(goldenDunningRun(), nil), "vlx_cus_golden"},
		{EventCreditBalanceLow, CreditBalanceWebhookData(goldenCreditBalance(), nil), "vlx_cus_golden"},
		// An event stored before ADR-131 carried customer_id at the top.
		{EventInvoicePaid, map[string]any{"invoice_id": "vlx_inv_old", "customer_id": "vlx_cus_old"}, "vlx_cus_old"},
		{EventInvoicePaid, map[string]any{}, ""},
	}
	for _, c := range cases {
		if got := WebhookEventCustomerID(c.eventType, c.data); got != c.want {
			t.Errorf("%s: customer id = %q, want %q", c.eventType, got, c.want)
		}
	}
}

func TestCheckWebhookData(t *testing.T) {
	if err := CheckWebhookData(EventInvoicePaid, InvoiceWebhookData(goldenInvoice(), nil)); err != nil {
		t.Errorf("valid data refused: %v", err)
	}
	old := map[string]any{"invoice_id": "vlx_inv_1", "customer_id": "vlx_cus_1", "amount_paid_cents": 100}
	if err := CheckWebhookData(EventInvoicePaid, old); err == nil {
		t.Error("the pre-ADR-131 flat payload passed the check")
	}
	stray := InvoiceWebhookData(goldenInvoice(), map[string]any{"currency": "USD"})
	if err := CheckWebhookData(EventInvoicePaid, stray); err == nil {
		t.Error("an unlisted extra key passed the check")
	}
	wrong := SubscriptionWebhookData(goldenSubscription(), nil)
	if err := CheckWebhookData(EventInvoicePaid, wrong); err == nil {
		t.Error("a subscription resource on an invoice event passed the check")
	}
}
