package billing_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/billing"
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

// recordingDunningStarter records which invoices were enrolled, and why.
type recordingDunningStarter struct {
	mu    sync.Mutex
	cause map[string]domain.DunningStartCause
}

func (d *recordingDunningStarter) StartDunning(_ context.Context, _, invoiceID, _ string, _ time.Time, cause domain.DunningStartCause) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cause == nil {
		d.cause = map[string]domain.DunningStartCause{}
	}
	d.cause[invoiceID] = cause
	return nil
}

type collectHarness struct {
	ctx      context.Context
	tenantID string
	invoices *invoice.PostgresStore
	newEng   func(ps billing.PaymentReadiness, ch billing.InvoiceCharger, ds billing.DunningStarter) *billing.Engine
	seed     func(t *testing.T, num string) domain.Invoice
	age      func(t *testing.T, invoiceID string, by time.Duration)
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
	// age backdates updated_at, standing in for "finalized a while ago".
	age := func(t *testing.T, invoiceID string, by time.Duration) {
		t.Helper()
		tx, err := db.BeginTx(context.Background(), postgres.TxBypass, "")
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := tx.Exec(`UPDATE invoices SET updated_at = now() - $2::interval WHERE id = $1`,
			invoiceID, fmt.Sprintf("%d seconds", int(by.Seconds()))); err != nil {
			_ = tx.Rollback()
			t.Fatalf("age invoice: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}
	return collectHarness{ctx: ctx, tenantID: tenantID, invoices: invoiceStore, newEng: newEng, seed: seed, age: age}
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

// Every unpaid invoice is queued from birth, so the queue alone no longer
// means "no card". No-payment dunning must enroll only invoices whose customer
// really has no chargeable payment method — a cardholder's queued invoice (the
// sweep didn't reach it, or another collector holds its lease) must not be
// dunned as no_payment_method.
func TestEnrollStalledForDunning_OnlyCardless(t *testing.T) {
	h := newCollectHarness(t)
	inv := h.seed(t, "INV-ENROLL-1")

	// Just finalized: its finalize-time collection may still be applying
	// credits that cover it, so even a card-less invoice waits out the
	// settle window rather than being dunned (and firing dunning.started).
	fresh := &recordingDunningStarter{}
	if _, errs := h.newEng(testPaymentSetupsNoPM{}, testChargerSentinel{}, fresh).EnrollStalledForDunning(h.ctx, 50); len(errs) != 0 {
		t.Fatalf("enroll errs: %v", errs)
	}
	if _, ok := fresh.cause[inv.ID]; ok {
		t.Fatal("a just-finalized invoice must not be enrolled before its collection settles")
	}
	h.age(t, inv.ID, time.Hour)

	starter := &recordingDunningStarter{}
	withCard := h.newEng(testPaymentSetupsReady{}, &countingCharger{}, starter)
	if _, errs := withCard.EnrollStalledForDunning(h.ctx, 50); len(errs) != 0 {
		t.Fatalf("enroll errs: %v", errs)
	}
	if _, ok := starter.cause[inv.ID]; ok {
		t.Fatal("a customer with a card on file must not be enrolled as no_payment_method")
	}

	cardless := h.newEng(testPaymentSetupsNoPM{}, testChargerSentinel{}, starter)
	if _, errs := cardless.EnrollStalledForDunning(h.ctx, 50); len(errs) != 0 {
		t.Fatalf("enroll errs: %v", errs)
	}
	if got := starter.cause[inv.ID]; got != domain.DunningCauseNoPaymentMethod {
		t.Fatalf("cardless invoice cause = %q, want %q", got, domain.DunningCauseNoPaymentMethod)
	}
}
