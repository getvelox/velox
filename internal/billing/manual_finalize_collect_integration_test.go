package billing_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/billing"
	"github.com/sagarsuperuser/velox/internal/credit"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/subscription"
	"github.com/sagarsuperuser/velox/internal/tax"
	"github.com/sagarsuperuser/velox/internal/tenant"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/usage"
)

// amountCharger records the amount each charge was for.
type amountCharger struct {
	mu  sync.Mutex
	got []int64
}

func (c *amountCharger) ChargeInvoice(_ context.Context, _ string, inv domain.Invoice, _, _ string) (domain.Invoice, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.got = append(c.got, inv.AmountDueCents)
	return inv, nil
}

// TestManualFinalize_CollectsThroughTheEngine_E2E drives the operator's
// POST /invoices/{id}/finalize end to end against Postgres, with the real
// billing engine wired as the handler's collector the way the router wires it.
// It proves the request collects through the one collector: a $0 invoice comes
// back paid, and a credit-holding customer's card is charged only the
// remainder after the real ledger drains.
func TestManualFinalize_CollectsThroughTheEngine_E2E(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Manual Finalize Collect")

	invoiceStore := invoice.NewPostgresStore(db)
	settings := tenant.NewSettingsStore(db)
	creditSvc := credit.NewService(credit.NewPostgresStore(db))
	customers := customer.NewPostgresStore(db)
	svc := invoice.NewService(invoiceStore, clock.Real(), settings)
	svc.SetCreditApplier(creditSvc)

	charger := &amountCharger{}
	engine := billing.NewEngine(
		&subStoreAdapter{subscription.NewPostgresStore(db)}, &usageStoreAdapter{usage.NewPostgresStore(db)},
		&pricingStoreAdapter{pricing.NewPostgresStore(db)}, &invoiceStoreAdapter{invoiceStore},
		creditSvc, settings, testPaymentSetupsReady{}, charger, clock.Real(),
	)
	engine.SetTaxProviderResolver(tax.NewResolver(nil))
	engine.SetNoPaymentMethodNotifier(&testNoPMNotifier{})
	engine.SetDunningResolver(&testDunningResolver{})

	h := invoice.NewHandler(svc, customers, settings)
	h.SetCollector(engine)
	routes := h.Routes()

	finalize := func(t *testing.T, id string) domain.Invoice {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/"+id+"/finalize", nil)
		reqCtx := context.WithValue(postgres.WithLivemode(req.Context(), false), auth.TestTenantIDKey(), tenantID)
		rr := httptest.NewRecorder()
		routes.ServeHTTP(rr, req.WithContext(reqCtx))
		if rr.Code != http.StatusOK {
			t.Fatalf("finalize status = %d, body=%s", rr.Code, rr.Body.String())
		}
		var out domain.Invoice
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out
	}
	newCustomer := func(t *testing.T, ext string) string {
		t.Helper()
		c, err := customers.Create(ctx, tenantID, domain.Customer{ExternalID: ext, DisplayName: ext})
		if err != nil {
			t.Fatalf("create customer: %v", err)
		}
		return c.ID
	}

	t.Run("$0 manual invoice → paid in the response", func(t *testing.T) {
		draft, err := svc.Create(ctx, tenantID, invoice.CreateInput{CustomerID: newCustomer(t, "cus_mf_zero")})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		out := finalize(t, draft.ID)
		if out.Status != domain.InvoicePaid {
			t.Errorf("response status = %s, want paid (zero-due settles through the collector)", out.Status)
		}
		stored, _ := invoiceStore.Get(ctx, tenantID, draft.ID)
		if stored.Status != domain.InvoicePaid || stored.AutoChargePending {
			t.Errorf("stored = %s queued=%v, want paid and dequeued", stored.Status, stored.AutoChargePending)
		}
	})

	t.Run("credit-holding customer → ledger drains, card charged the remainder", func(t *testing.T) {
		cusID := newCustomer(t, "cus_mf_credit")
		if _, err := creditSvc.Grant(ctx, tenantID, credit.GrantInput{
			CustomerID: cusID, AmountCents: 2000, Description: "seed", At: time.Now().UTC().Add(-time.Hour),
		}); err != nil {
			t.Fatalf("grant: %v", err)
		}
		draft, err := svc.Create(ctx, tenantID, invoice.CreateInput{CustomerID: cusID})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := svc.AddLineItem(ctx, tenantID, draft.ID, invoice.AddLineItemInput{
			Description: "Onboarding", Quantity: 1, UnitAmountCents: 5000,
		}); err != nil {
			t.Fatalf("add line: %v", err)
		}
		before := len(charger.got)
		finalize(t, draft.ID)
		if len(charger.got) != before+1 || charger.got[before] != 3000 {
			t.Fatalf("charges = %v, want one charge of 3000 (5000 - 2000 credits)", charger.got[before:])
		}
		bal, err := creditSvc.GetBalance(ctx, tenantID, cusID)
		if err != nil {
			t.Fatalf("balance: %v", err)
		}
		if bal.BalanceCents != 0 {
			t.Errorf("ledger balance = %d, want 0 (drained into the invoice)", bal.BalanceCents)
		}
	})
}
