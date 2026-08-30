package billing_test

import (
	"context"
	"fmt"
	"github.com/sagarsuperuser/velox/internal/platform/leader"
	"github.com/sagarsuperuser/velox/internal/platform/leader/leadertest"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/billing"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/dunning"
	"github.com/sagarsuperuser/velox/internal/integrations/litellm"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/recipe"
	"github.com/sagarsuperuser/velox/internal/subscription"
	"github.com/sagarsuperuser/velox/internal/subscription/subscriptiontest"
	"github.com/sagarsuperuser/velox/internal/tax"
	"github.com/sagarsuperuser/velox/internal/tenant"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/usage"
	"github.com/sagarsuperuser/velox/internal/webhook"
)

// TestLiteLLM_WedgeE2E is the AI-native wedge as a CI test: a REALISTIC LiteLLM
// spend payload (dated API model string + a prompt-cache hit) → the real
// /spend handler → usage events → cycle billing → invoice priced by the
// anthropic_style recipe. It exercises the two fixes that make the wedge
// actually rate end-to-end (ADR-044):
//
//   - MODEL NORMALIZATION: the payload carries "claude-3-5-sonnet-20241022"
//     (what LiteLLM forwards); the mapper canonicalizes it to the recipe family
//     "claude-3.5-sonnet" so the {model, token_type} rules match. Without this
//     the usage rates at $0.
//   - CACHE_READ additive-disjoint split: prompt_tokens includes the cached
//     tokens; the mapper emits input = prompt − cached and cache_read = cached,
//     each priced at its own recipe rate.
//
// This is the test that would have caught the S2/X15 wedge breakage at CI time.
func TestLiteLLM_WedgeE2E(t *testing.T) {
	db := testutil.SetupTestDB(t)
	// livemode=false test partition; tenant bound below for the handler path.
	baseCtx := leadertest.Token(t, testutil.AdminPool(t), postgres.WithLivemode(context.Background(), false), leader.RoleBilling, leader.RoleDunning)

	customerStore := customer.NewPostgresStore(db)
	pricingStore := pricing.NewPostgresStore(db)
	pricingSvc := pricing.NewService(pricingStore)
	subStore := subscription.NewPostgresStore(db)
	invoiceStore := invoice.NewPostgresStore(db)
	usageStore := usage.NewPostgresStore(db)
	usageSvc := usage.NewService(usageStore)
	settingsStore := tenant.NewSettingsStore(db)

	tenantID := testutil.CreateTestTenant(t, db, "LiteLLM Wedge Corp")
	// Stores/engine take tenantID as a param (mirroring the other billing
	// integration tests); only the litellm HTTP handler reads tenant from ctx.
	ctx := baseCtx
	authCtx := auth.WithTenantID(baseCtx, tenantID)

	// 1. Instantiate the anthropic_style recipe → one `tokens` meter, the
	//    per-{model, token_type} rules, and the ai_api_pro plan.
	registry, err := recipe.Load()
	if err != nil {
		t.Fatalf("load recipe registry: %v", err)
	}
	recipeSvc := recipe.NewService(db, recipe.NewPostgresStore(db), registry,
		pricingSvc, dunning.NewService(dunning.NewPostgresStore(db), nil, nil),
		webhook.NewService(webhook.NewPostgresStore(db), nil))
	if _, err := recipeSvc.Instantiate(ctx, tenantID, "anthropic_style", nil, recipe.InstantiateOptions{}); err != nil {
		t.Fatalf("instantiate anthropic_style: %v", err)
	}

	// Find the recipe's plan (default code ai_api_pro).
	plans, err := pricingStore.ListPlans(ctx, tenantID)
	if err != nil {
		t.Fatalf("list plans: %v", err)
	}
	var plan domain.Plan
	for _, p := range plans {
		if p.Code == "ai_api_pro" {
			plan = p
		}
	}
	if plan.ID == "" {
		t.Fatal("recipe did not create the ai_api_pro plan")
	}

	// 2. Customer + subscription on the recipe plan, over an ELAPSED period in
	//    the past (so RunCycle bills it and event timestamps aren't future).
	cust, err := customerStore.Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_wedge", DisplayName: "Wedge Customer",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	periodStart := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	sub, err := subStore.Create(ctx, tenantID, domain.Subscription{
		Code: "sub-wedge", DisplayName: "Wedge sub", CustomerID: cust.ID,
		Items:  []domain.SubscriptionItem{{PlanID: plan.ID, Quantity: 1}},
		Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
		StartedAt:                 &periodStart,
		CurrentBillingPeriodStart: &periodStart, CurrentBillingPeriodEnd: bivPE(periodStart),
	})
	if err != nil {
		t.Fatalf("create sub: %v", err)
	}
	subscriptiontest.SetBillingCycle(t, ctx, db, tenantID, sub.ID, periodStart, periodEnd, periodEnd, 0)

	// 3. POST a realistic LiteLLM spend payload through the REAL /spend handler.
	//    Quantities are scaled to whole-cent amounts at the recipe's
	//    claude-3.5-sonnet rates (input $3.00/M, cache_read $0.30/M, output
	//    $15.00/M): input 1M → 300¢, cache_read 1M → 30¢, output 1M → 1500¢.
	eventTS := periodStart.Add(15 * 24 * time.Hour).Unix() // mid-period (Apr 15)
	body := fmt.Sprintf(`{
		"id":"wedge_call_1","call_type":"completion",
		"model":"claude-3-5-sonnet-20241022","custom_llm_provider":"anthropic",
		"user":"cus_wedge",
		"usage":{"prompt_tokens":2000000,"completion_tokens":1000000,"total_tokens":3000000,
		         "prompt_tokens_details":{"cached_tokens":1000000},"cache_read_input_tokens":1000000},
		"endTime":%d
	}`, eventTS)

	handler := litellm.New(customerStore, pricingSvc, usageSvc)
	req := httptest.NewRequest(http.MethodPost, "/spend", strings.NewReader(body)).WithContext(authCtx)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/spend: status %d, body %s", rec.Code, rec.Body.String())
	}
	// input + cache_read + output = 3 accepted events.
	if got := rec.Body.String(); !strings.Contains(got, `"accepted":3`) {
		t.Fatalf("/spend body: got %s, want accepted=3 (input+cache_read+output)", got)
	}

	// 3b. Verify the events landed with the canonical family + token roles.
	events, err := usageStore.AggregateByPricingRules(ctx, tenantID, cust.ID, plan.MeterIDs[0], domain.AggSum, periodStart, periodEnd)
	if err != nil {
		t.Fatalf("aggregate by rules: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("no rule aggregations — events didn't match recipe rules (model normalization failed?)")
	}

	// 4. Run the cycle → invoice for the elapsed period.
	engine := billing.NewEngine(
		&subStoreAdapter{subStore}, &usageStoreAdapter{usageStore},
		&pricingStoreAdapter{pricingStore}, &invoiceStoreAdapter{invoiceStore},
		nil, settingsStore, testPaymentSetupsNoPM{}, testChargerSentinel{}, clock.NewFake(periodEnd.Add(time.Nanosecond)),
	)
	engine.SetIntervalReader(subStore)
	engine.SetTaxProviderResolver(tax.NewResolver(nil))
	engine.SetNoPaymentMethodNotifier(&testNoPMNotifier{})
	if _, errs := engine.RunCycle(ctx, 50); len(errs) > 0 {
		t.Fatalf("RunCycle: %v", errs)
	}

	// 5. Assert the invoice rates the usage at the recipe's per-{model,token_type}
	//    rates — proving the dated model string normalized and all three roles
	//    (input/cache_read/output) priced correctly.
	invs, _, err := invoiceStore.List(ctx, invoice.ListFilter{TenantID: tenantID})
	if err != nil {
		t.Fatalf("list invoices: %v", err)
	}
	var inv domain.Invoice
	for _, i := range invs {
		if i.SubscriptionID == sub.ID {
			inv = i
		}
	}
	if inv.ID == "" {
		t.Fatal("no cycle invoice for the wedge sub")
	}
	// input 300¢ + cache_read 30¢ + output 1500¢ = 1830¢ (base $0, no tax).
	const wantCents = 1830
	if inv.SubtotalCents != wantCents {
		items, _ := invoiceStore.ListLineItems(ctx, tenantID, inv.ID)
		t.Errorf("invoice subtotal: got %d¢, want %d¢ (input 300 + cache_read 30 + output 1500). Lines: %+v", inv.SubtotalCents, wantCents, items)
	}
	t.Logf("wedge e2e: claude-3-5-sonnet-20241022 → claude-3.5-sonnet, 3 token roles rated, invoice $%.2f", float64(inv.SubtotalCents)/100)
}
