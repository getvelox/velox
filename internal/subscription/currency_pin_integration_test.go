package subscription

import (
	"context"
	"strings"
	"testing"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestCurrencyPin is the real-Postgres proof of the ADR-100 customer
// currency pin. The engine relabels rule-computed cents under one invoice
// currency without conversion, and the credit ledger is a currency-blind
// per-customer pot — so every write that binds a plan to a subscription
// must keep the customer's billing in ONE currency. Each clause is proven
// in both directions (refusal + coherent twin), including the two bypass
// windows the design panel found: profile-set-first ordering and the
// pre-existing divergent draft at Activate.
func TestCurrencyPin(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Currency Pin")

	custStore := customer.NewPostgresStore(db)
	cust, err := custStore.Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_currency_pin", DisplayName: "Currency Pin",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	pricingStore := pricing.NewPostgresStore(db)
	newPlan := func(code, currency, meterID string) domain.Plan {
		p, err := pricingStore.CreatePlan(ctx, tenantID, domain.Plan{
			Code: code, Name: code, Currency: currency,
			BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInArrears,
			BaseAmountCents: 1000, Status: domain.PlanActive, MeterIDs: []string{meterID},
		})
		if err != nil {
			t.Fatalf("create plan %s: %v", code, err)
		}
		return p
	}
	// Distinct meters so the meter-overlap guard stays out of the way.
	usdA := newPlan("pin-usd-a", "USD", "mtr_pin_a")
	usdB := newPlan("pin-usd-b", "USD", "mtr_pin_b")
	eurC := newPlan("pin-eur-c", "EUR", "mtr_pin_c")
	eurD := newPlan("pin-eur-d", "EUR", "mtr_pin_d")

	svc := NewService(NewPostgresStore(db), nil)
	svc.SetPlanReader(pricingStore)
	svc.SetProfileReader(custStore)

	create := func(code string, startNow bool, planIDs ...string) (domain.Subscription, error) {
		items := make([]CreateItemInput, 0, len(planIDs))
		for _, pid := range planIDs {
			items = append(items, CreateItemInput{PlanID: pid, Quantity: 1})
		}
		return svc.Create(ctx, tenantID, CreateInput{
			Code: code, DisplayName: code, CustomerID: cust.ID,
			Items: items, BillingTime: domain.BillingTimeAnniversary, StartNow: startNow,
		})
	}
	wantRefusal := func(err error, needle, scenario string) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s: want currency refusal, got nil", scenario)
		}
		if !strings.Contains(err.Error(), needle) {
			t.Fatalf("%s: refusal should mention %q, got: %v", scenario, needle, err)
		}
	}

	// Mixed-currency item SET at create refuses.
	_, err = create("pin-mixed", true, usdA.ID, eurC.ID)
	wantRefusal(err, "one currency", "mixed item set")

	// Ordering bypass closed: profile EUR set FIRST (store-level, simulating
	// any pre-guard write), then a USD-plan sub refuses.
	if _, err := custStore.UpsertBillingProfile(ctx, tenantID, domain.CustomerBillingProfile{
		CustomerID: cust.ID, Currency: "EUR", ProfileStatus: domain.BillingProfileIncomplete,
	}); err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	_, err = create("pin-usd-vs-profile", true, usdA.ID)
	wantRefusal(err, "billing profile", "profile-first ordering")

	// Coherent twin: EUR plan under the EUR profile passes.
	subEUR, err := create("pin-eur-ok", true, eurC.ID)
	if err != nil {
		t.Fatalf("EUR sub under EUR profile must pass: %v", err)
	}

	// Cross-sub pin: with an EUR sub live, clearing the profile does not
	// unpin — a USD sub still refuses, naming the existing sub.
	if _, err := custStore.UpsertBillingProfile(ctx, tenantID, domain.CustomerBillingProfile{
		CustomerID: cust.ID, Currency: "", ProfileStatus: domain.BillingProfileIncomplete,
	}); err != nil {
		t.Fatalf("clear profile: %v", err)
	}
	_, err = create("pin-usd-vs-sub", true, usdA.ID)
	wantRefusal(err, "pin-eur-ok", "cross-sub pin")

	// Same-currency second sub passes (different meter).
	if _, err := create("pin-eur-second", true, eurD.ID); err != nil {
		t.Fatalf("second EUR sub must pass: %v", err)
	}

	// Cross-currency SWAP refuses even on a single-item sub (raw-cents
	// proration): schedule a swap of the EUR sub's item to a USD plan.
	_, err = svc.UpdateItem(ctx, tenantID, subEUR.ID, subEUR.Items[0].ID, UpdateItemInput{NewPlanID: usdB.ID})
	wantRefusal(err, "current plan", "cross-currency swap")

	// Pre-existing divergent DRAFT cannot activate: build the draft while
	// coherent (no profile), then diverge the profile store-level (pre-ring
	// data), then Activate refuses.
	if _, err := custStore.UpsertBillingProfile(ctx, tenantID, domain.CustomerBillingProfile{
		CustomerID: cust.ID, Currency: "", ProfileStatus: domain.BillingProfileIncomplete,
	}); err != nil {
		t.Fatalf("clear profile: %v", err)
	}
	cust2, err := custStore.Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_pin_draft", DisplayName: "Pin Draft",
	})
	if err != nil {
		t.Fatalf("customer 2: %v", err)
	}
	draft, err := svc.Create(ctx, tenantID, CreateInput{
		Code: "pin-draft", DisplayName: "pin-draft", CustomerID: cust2.ID,
		Items:       []CreateItemInput{{PlanID: usdA.ID, Quantity: 1}},
		BillingTime: domain.BillingTimeAnniversary, StartNow: false,
	})
	if err != nil {
		t.Fatalf("draft create: %v", err)
	}
	if _, err := custStore.UpsertBillingProfile(ctx, tenantID, domain.CustomerBillingProfile{
		CustomerID: cust2.ID, Currency: "EUR", ProfileStatus: domain.BillingProfileIncomplete,
	}); err != nil {
		t.Fatalf("diverge profile: %v", err)
	}
	_, err = svc.Activate(ctx, tenantID, draft.ID)
	wantRefusal(err, "billing profile", "divergent draft activate")
}
