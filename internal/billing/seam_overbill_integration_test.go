package billing_test

import (
	"context"
	"testing"
	"time"

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

const (
	baseCents     = 3000                // $30/mo (or /yr) in_advance base fee used across the seam tests.
	oneMonthFloor = 28 * 24 * time.Hour // any real monthly period exceeds this; a seam is far shorter.
)

// seamFixture is a $30 in_advance subscription created under org timezone `zone`,
// with helpers to change the ONE org timezone (ADR-077) and drive cycle rolls.
// Backs the ADR-091 seam tests: an org-timezone change re-resolves the stored
// period boundary in the new zone, which can produce a degenerate short "seam"
// period. This must never bill a full base fee for that seam — calendar seams are
// absorbed (sub-day), anniversary/yearly seams prorate (off-anchor) — for every
// billing interval, not just calendar (the coverage gap that let the anniversary
// overbill ship).
type seamFixture struct {
	t              *testing.T
	ctx            context.Context
	tenantID       string
	subID          string
	custID         string
	subStore       *subscription.PostgresStore
	invStore       *invoice.PostgresStore
	settings       *tenant.SettingsStore
	newEngine      func() *billing.Engine
	fakeClock      *clock.Fake
	p0start        time.Time
	p0end          time.Time
	createInv      domain.Invoice
	fullCycleFloor time.Duration // a base line spanning less than this is "short" (seam/stub).
}

func newSeamFixture(t *testing.T, zone string, billingTime domain.SubscriptionBillingTime, interval domain.BillingInterval, anchorDay int) *seamFixture {
	t.Helper()
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)

	pricingStore := pricing.NewPostgresStore(db)
	subStore := subscription.NewPostgresStore(db)
	invoiceStore := invoice.NewPostgresStore(db)
	usageStore := usage.NewPostgresStore(db)
	creditStore := credit.NewPostgresStore(db)
	creditSvc := credit.NewService(creditStore)
	settingsStore := tenant.NewSettingsStore(db)
	customerStore := customer.NewPostgresStore(db)

	tenantID := testutil.CreateTestTenant(t, db, "Seam Corp")

	loc, err := time.LoadLocation(zone)
	if err != nil {
		t.Fatalf("load %s: %v", zone, err)
	}

	f := &seamFixture{t: t, ctx: ctx, tenantID: tenantID, subStore: subStore, invStore: invoiceStore, settings: settingsStore}
	f.setOrgTimezone(zone)

	// A clean full period in `zone`, sized to the interval.
	f.p0start = time.Date(2027, 6, 1, 0, 0, 0, 0, loc).UTC()
	if interval == domain.BillingYearly {
		f.p0end = time.Date(2028, 6, 1, 0, 0, 0, 0, loc).UTC()
		f.fullCycleFloor = 360 * 24 * time.Hour
	} else {
		f.p0end = time.Date(2027, 7, 1, 0, 0, 0, 0, loc).UTC()
		f.fullCycleFloor = oneMonthFloor
	}

	plan, err := pricingStore.CreatePlan(ctx, tenantID, domain.Plan{
		Code: "pro-adv-seam", Name: "Pro", Currency: "USD",
		BillingInterval: interval, Status: domain.PlanActive,
		BaseAmountCents: baseCents, BaseBillTiming: domain.BillInAdvance,
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	cust, err := customerStore.Create(ctx, tenantID, domain.Customer{ExternalID: "cus_seam", DisplayName: "Seam"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	f.custID = cust.ID
	sub, err := subStore.Create(ctx, tenantID, domain.Subscription{
		Code: "sub-seam", DisplayName: "Seam", CustomerID: cust.ID,
		Items:  []domain.SubscriptionItem{{PlanID: plan.ID, Quantity: 1}},
		Status: domain.SubscriptionActive, BillingTime: billingTime,
		StartedAt:                 &f.p0start,
		CurrentBillingPeriodStart: &f.p0start, CurrentBillingPeriodEnd: &f.p0end,
	})
	if err != nil {
		t.Fatalf("create sub: %v", err)
	}
	f.subID = sub.ID
	if err := subStore.UpdateBillingCycle(ctx, tenantID, sub.ID, f.p0start, f.p0end, f.p0end, anchorDay); err != nil {
		t.Fatalf("billing cycle: %v", err)
	}
	sub.CurrentBillingPeriodStart = &f.p0start
	sub.CurrentBillingPeriodEnd = &f.p0end
	sub.BillingAnchorDay = anchorDay

	f.fakeClock = clock.NewFake(f.p0start.Add(time.Hour))
	f.newEngine = func() *billing.Engine {
		e := billing.NewEngine(
			&subStoreAdapter{subStore}, &usageStoreAdapter{usageStore},
			&pricingStoreAdapter{pricingStore}, &invoiceStoreAdapter{invoiceStore},
			creditSvc, settingsStore, testPaymentSetupsNoPM{}, testChargerSentinel{},
			f.fakeClock,
		)
		e.SetTaxProviderResolver(tax.NewResolver(nil))
		e.SetNoPaymentMethodNotifier(&testNoPMNotifier{})
		e.SetDunningResolver(&testDunningResolver{})
		return e
	}

	createInv, err := f.newEngine().BillOnCreate(ctx, sub)
	if err != nil {
		t.Fatalf("BillOnCreate: %v", err)
	}
	f.createInv = createInv
	return f
}

// setOrgTimezone changes the single org timezone (ADR-077) — the display AND
// billing zone in the one-zone model.
func (f *seamFixture) setOrgTimezone(tz string) {
	f.t.Helper()
	ts, err := f.settings.Get(f.ctx, f.tenantID)
	if err != nil {
		f.t.Fatalf("get settings: %v", err)
	}
	ts.Timezone = tz
	if _, err := f.settings.Upsert(f.ctx, ts); err != nil {
		f.t.Fatalf("set org timezone %s: %v", tz, err)
	}
}

func (f *seamFixture) rollOnce() (int, domain.Subscription) {
	f.t.Helper()
	s, err := f.subStore.Get(f.ctx, f.tenantID, f.subID)
	if err != nil {
		f.t.Fatalf("re-read sub: %v", err)
	}
	f.fakeClock.Set(s.CurrentBillingPeriodEnd.Add(time.Hour))
	gen, fails := f.newEngine().RunCycleForTenant(f.ctx, f.tenantID, 50)
	if len(fails) > 0 {
		f.t.Fatalf("cycle failures: %v", fails)
	}
	after, err := f.subStore.Get(f.ctx, f.tenantID, f.subID)
	if err != nil {
		f.t.Fatalf("re-read sub after roll: %v", err)
	}
	return gen, after
}

func (f *seamFixture) invoices() []domain.Invoice {
	f.t.Helper()
	invs, _, err := f.invStore.List(f.ctx, invoice.ListFilter{TenantID: f.tenantID, CustomerID: f.custID})
	if err != nil {
		f.t.Fatalf("list invoices: %v", err)
	}
	return invs
}

// assertNoFullFeeForShortPeriod is the core money assertion: NO base-fee line may
// bill the FULL base fee for a period shorter than a full cycle. A calendar seam
// is absorbed (no line); an anniversary/yearly seam prorates (< full). Only a
// broken build bills the full base for a sub-cycle window (the seam overbill).
func (f *seamFixture) assertNoFullFeeForShortPeriod() {
	f.t.Helper()
	for _, inv := range f.invoices() {
		lines, err := f.invStore.ListLineItems(f.ctx, f.tenantID, inv.ID)
		if err != nil {
			f.t.Fatalf("list lines: %v", err)
		}
		for _, l := range lines {
			if l.LineType != domain.LineTypeBaseFee || l.BillingPeriodStart == nil || l.BillingPeriodEnd == nil {
				continue
			}
			span := l.BillingPeriodEnd.Sub(*l.BillingPeriodStart)
			if span < f.fullCycleFloor && l.AmountCents >= baseCents {
				f.t.Errorf("invoice %s: base line bills %d cents (full fee) for a %s period (< a full cycle) — seam overbill",
					inv.ID, l.AmountCents, span)
			}
		}
	}
}

// TestOrgTZChange_CalendarSeam_Absorbed_E2E — the calendar sub-day seam: a
// west-ward org-timezone change lands the next boundary ~9.5h past periodEnd; the
// base line is absorbed (no full fee), and the sub re-aligns next cycle.
func TestOrgTZChange_CalendarSeam_Absorbed_E2E(t *testing.T) {
	f := newSeamFixture(t, "Asia/Kolkata", domain.BillingTimeCalendar, domain.BillingMonthly, 0)
	f.setOrgTimezone("America/New_York")

	gen1, _ := f.rollOnce() // sub-day seam absorbed → no invoice
	if gen1 != 0 {
		t.Fatalf("calendar seam roll generated = %d, want 0 (sub-day base absorbed)", gen1)
	}
	gen2, _ := f.rollOnce() // first full New_York cycle
	if gen2 != 1 {
		t.Fatalf("first NY cycle generated = %d, want 1", gen2)
	}
	f.assertNoFullFeeForShortPeriod()
	if got := len(f.invoices()); got != 2 {
		t.Errorf("invoices = %d, want 2 (create + one full cycle; seam absorbed)", got)
	}
}

// TestOrgTZChange_AnniversaryMonthlySeam_Prorated_E2E is the regression for the
// adversarial-review HIGH: an anniversary-monthly sub's ~24h re-anchor seam is
// NOT sub-day (advanceDays rounds to 1, not 0) and — before the off-anchor fix —
// could not prorate (fullCycleDays == advanceDays), so it billed a FULL month for
// a 1-day window (~30× overbill). The off-anchor detector now measures the cycle
// nominally so the seam prorates to ~1 day's worth instead.
//
// Mutation-verify: delete the `!IsPeriodStartOnAnchor` override in engine.go and
// the seam bills the full base fee for a ~1-day line → assertNoFullFeeForShortPeriod fails.
func TestOrgTZChange_AnniversaryMonthlySeam_Prorated_E2E(t *testing.T) {
	f := newSeamFixture(t, "Asia/Kolkata", domain.BillingTimeAnniversary, domain.BillingMonthly, 1)
	f.setOrgTimezone("America/New_York")

	gen1, _ := f.rollOnce() // the ~24h seam: not absorbed, prorated to a small line
	if gen1 != 1 {
		t.Fatalf("anniversary seam roll generated = %d, want 1 (prorated seam line)", gen1)
	}
	gen2, _ := f.rollOnce() // first full New_York cycle
	if gen2 != 1 {
		t.Fatalf("first NY cycle generated = %d, want 1", gen2)
	}
	f.assertNoFullFeeForShortPeriod()

	// The seam invoice's base fee must be a small proration, not a full month.
	var sawProratedSeam bool
	for _, inv := range f.invoices() {
		if inv.ID == f.createInv.ID {
			continue
		}
		lines, _ := f.invStore.ListLineItems(f.ctx, f.tenantID, inv.ID)
		for _, l := range lines {
			if l.LineType == domain.LineTypeBaseFee && l.BillingPeriodStart != nil && l.BillingPeriodEnd != nil {
				span := l.BillingPeriodEnd.Sub(*l.BillingPeriodStart)
				if span < f.fullCycleFloor {
					sawProratedSeam = true
					if l.AmountCents >= baseCents {
						t.Errorf("anniversary seam base line = %d cents for a %s period, want a small proration", l.AmountCents, span)
					}
				}
			}
		}
	}
	if !sawProratedSeam {
		t.Error("expected a prorated sub-cycle seam line after the anniversary org-TZ change")
	}
}

// TestOrgTZChange_YearlySeam_Prorated_E2E extends the off-anchor fix to yearly
// anniversary subs — a zone change leaves the yearly boundary off-anchor and the
// short residual prorates instead of billing a full year.
func TestOrgTZChange_YearlySeam_Prorated_E2E(t *testing.T) {
	f := newSeamFixture(t, "Asia/Kolkata", domain.BillingTimeAnniversary, domain.BillingYearly, 1)
	f.setOrgTimezone("America/New_York")

	f.rollOnce() // drive the seam roll — the assertion below is on the emitted invoices
	f.assertNoFullFeeForShortPeriod()
}
