package billing_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/billing"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/dunning"
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

// testPaymentSetupsReady reports a chargeable card for everyone.
type testPaymentSetupsReady struct{}

func (testPaymentSetupsReady) ResolveForCharge(_ context.Context, _, _ string) (string, string, error) {
	return "cus_stripe", "pm_card", nil
}

// countingCharger counts charges per invoice. delay holds each charge open
// so concurrent collectors overlap inside the charge leg.
type countingCharger struct {
	mu    sync.Mutex
	n     map[string]int
	delay time.Duration
}

func (c *countingCharger) ChargeInvoice(_ context.Context, _ string, inv domain.Invoice, _, _ string) (domain.Invoice, error) {
	time.Sleep(c.delay)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n == nil {
		c.n = map[string]int{}
	}
	c.n[inv.ID]++
	return inv, nil
}

func (c *countingCharger) count(id string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n[id]
}

type collectHarness struct {
	ctx      context.Context
	tenantID string
	invoices *invoice.PostgresStore
	newEng   func(ps billing.PaymentReadiness, ch billing.InvoiceCharger, ds billing.DunningStarter) *billing.Engine
	seed     func(t *testing.T, num string) domain.Invoice
	db       *postgres.DB
}

func newCollectHarness(t *testing.T) collectHarness {
	t.Helper()
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Collect Recovery")
	invoiceStore := invoice.NewPostgresStore(db)
	subStore := subscription.NewPostgresStore(db)
	customerStore := customer.NewPostgresStore(db)

	newEng := func(ps billing.PaymentReadiness, ch billing.InvoiceCharger, ds billing.DunningStarter) *billing.Engine {
		e := billing.NewEngine(
			&subStoreAdapter{subStore}, &usageStoreAdapter{usage.NewPostgresStore(db)},
			&pricingStoreAdapter{pricing.NewPostgresStore(db)}, &invoiceStoreAdapter{invoiceStore},
			nil, tenant.NewSettingsStore(db), ps, ch, clock.NewFake(time.Now().UTC()),
		)
		e.SetTaxProviderResolver(tax.NewResolver(nil))
		e.SetNoPaymentMethodNotifier(&testNoPMNotifier{})
		e.SetDunningStarter(testDunningStarter{})
		e.SetDunningResolver(&testDunningResolver{})
		if ds != nil {
			e.SetDunningStarter(ds)
		}
		return e
	}
	// seed writes a finalized, unpaid invoice through the real store: the
	// same write a finalize commits. Nothing after it runs, which is exactly
	// the state a crash right after the finalize commit leaves behind.
	seed := func(t *testing.T, num string) domain.Invoice {
		t.Helper()
		cust, err := customerStore.Create(ctx, tenantID, domain.Customer{ExternalID: "cus_" + num, DisplayName: num})
		if err != nil {
			t.Fatalf("create customer: %v", err)
		}
		inv, err := invoiceStore.Create(ctx, tenantID, domain.Invoice{
			CustomerID: cust.ID, InvoiceNumber: num,
			Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending,
			Currency: "USD", SubtotalCents: 5000, TotalAmountCents: 5000, AmountDueCents: 5000,
			BillingPeriodStart: time.Now().UTC().Add(-30 * 24 * time.Hour),
			BillingPeriodEnd:   time.Now().UTC(),
		})
		if err != nil {
			t.Fatalf("create invoice: %v", err)
		}
		return inv
	}
	return collectHarness{ctx: ctx, tenantID: tenantID, invoices: invoiceStore, newEng: newEng, seed: seed, db: db}
}

// A crash between the finalize commit and the post-commit collection used to
// strand the invoice: it was committed with auto_charge_pending=false and only
// the inline step (which never ran) would have set it. Now the finalize write
// queues it, so the next sweep charges it — exactly once across ticks.
func TestCollect_CrashAfterFinalize_SweepChargesExactlyOnce(t *testing.T) {
	h := newCollectHarness(t)
	inv := h.seed(t, "INV-CRASH-1")

	stored, err := h.invoices.Get(h.ctx, h.tenantID, inv.ID)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if !stored.AutoChargePending {
		t.Fatal("a finalized, unpaid invoice must be queued by its finalize write")
	}

	charger := &countingCharger{}
	e := h.newEng(testPaymentSetupsReady{}, charger, nil)
	for tick := 0; tick < 3; tick++ {
		e.RetryPendingCharges(h.ctx, 50)
	}
	if got := charger.count(inv.ID); got != 1 {
		t.Fatalf("charges = %d across 3 ticks, want exactly 1", got)
	}
}

// The finalize-time nudge and the hourly sweep can reach the same invoice at
// once (a cycle close racing a rival leader's sweep, or a request finishing as
// the tick starts). They share the per-invoice lease, so the card is charged
// once.
func TestCollect_NudgeAndSweepRace_ChargesOnce(t *testing.T) {
	h := newCollectHarness(t)
	inv := h.seed(t, "INV-RACE-1")
	charger := &countingCharger{delay: 50 * time.Millisecond}
	e := h.newEng(testPaymentSetupsReady{}, charger, nil)

	var wg sync.WaitGroup
	var started atomic.Int32
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			started.Add(1)
			e.CollectInvoice(h.ctx, h.tenantID, inv.ID, nil)
		}()
		go func() {
			defer wg.Done()
			started.Add(1)
			e.RetryPendingCharges(h.ctx, 50)
		}()
	}
	wg.Wait()
	if got := charger.count(inv.ID); got != 1 {
		t.Fatalf("charges = %d with %d racing collectors, want exactly 1", got, started.Load())
	}
}

// dunningServiceStarter adapts the real dunning.Service to the engine's
// DunningStarter, the way api.dunningStarterAdapter does.
type dunningServiceStarter struct{ svc *dunning.Service }

func (d dunningServiceStarter) StartDunning(ctx context.Context, tenantID, invoiceID, customerID string, failureAt time.Time, cause domain.DunningStartCause) error {
	_, err := d.svc.StartDunning(ctx, tenantID, invoiceID, customerID, failureAt, cause)
	return err
}

// The collector starts no-payment dunning for a card-less invoice, against
// the real dunning store: one run (cause no_payment_method) however many
// ticks revisit it, and none at all for a customer with a card.
func TestCollector_StartsNoPaymentDunning_RealStore(t *testing.T) {
	h := newCollectHarness(t)
	dstore := dunning.NewPostgresStore(h.db)
	if _, err := dstore.UpsertPolicy(h.ctx, h.tenantID, domain.DunningPolicy{
		Name: "default", Enabled: true, RetrySchedule: []string{"72h"}, MaxRetryAttempts: 3,
		FinalSubscriptionAction: domain.SubActionNone, FinalInvoiceAction: domain.InvActionNone, GracePeriodDays: 3,
	}); err != nil {
		t.Fatalf("upsert policy: %v", err)
	}
	starter := dunningServiceStarter{svc: dunning.NewService(dstore, nil, nil)}

	cardless := h.seed(t, "INV-NOCARD-1")
	e := h.newEng(testPaymentSetupsNoPM{}, testChargerSentinel{}, starter)
	for tick := 0; tick < 3; tick++ {
		e.RetryPendingCharges(h.ctx, 50)
	}
	run, err := dstore.GetRunByInvoice(h.ctx, h.tenantID, cardless.ID)
	if err != nil {
		t.Fatalf("card-less invoice must have a dunning run: %v", err)
	}
	if run.Reason != string(domain.DunningCauseNoPaymentMethod) {
		t.Errorf("run reason = %q, want %q", run.Reason, domain.DunningCauseNoPaymentMethod)
	}

	withCard := h.seed(t, "INV-CARD-1")
	h.newEng(testPaymentSetupsReady{}, &countingCharger{}, starter).RetryPendingCharges(h.ctx, 50)
	if _, err := dstore.GetRunByInvoice(h.ctx, h.tenantID, withCard.ID); err == nil {
		t.Error("a customer with a card must not be dunned as no_payment_method")
	}
}
