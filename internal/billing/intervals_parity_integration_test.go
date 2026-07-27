package billing_test

import (
	"context"
	"fmt"
	"sort"
	"strings"
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

// ADR-101 Phase 2/3 corpus gate: every walked mutation shape must bill
// IDENTICALLY under the legacy fact-log interpretation (mode shadow)
// and the billing_intervals reader (mode on), with the shadow
// comparator reporting zero unexplained divergence in both. The two
// known-divergence classes (catch-up lifetime, org-TZ clamp-miss) get
// their own tests asserting the classifier catches them AND the
// interval side is the more-correct one.
//
// Usage lines derive from the same per-item segments the base fee
// bills from, so base-line parity here covers the usage windows
// structurally; the corpus keeps meters out to stay deterministic.

var parityPS = time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
var parityPE = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

type parityFixture struct {
	db       *postgres.DB
	ctx      context.Context
	tenantID string
	custID   string
	subs     *subscription.PostgresStore
	pricing  *pricing.PostgresStore
	invoices *invoice.PostgresStore
	plans    map[string]domain.Plan
}

func newParityFixture(t *testing.T, db *postgres.DB, label string) *parityFixture {
	t.Helper()
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Parity "+label)
	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_parity_" + label, DisplayName: label,
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	return &parityFixture{
		db: db, ctx: ctx, tenantID: tenantID, custID: cust.ID,
		subs: subscription.NewPostgresStore(db), pricing: pricing.NewPostgresStore(db),
		invoices: invoice.NewPostgresStore(db), plans: map[string]domain.Plan{},
	}
}

func (f *parityFixture) plan(t *testing.T, code string, cents int64) domain.Plan {
	t.Helper()
	p, err := f.pricing.CreatePlan(f.ctx, f.tenantID, domain.Plan{
		Code: code, Name: code, Currency: "USD",
		BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInArrears,
		BaseAmountCents: cents, Status: domain.PlanActive,
	})
	if err != nil {
		t.Fatalf("create plan %s: %v", code, err)
	}
	f.plans[code] = p
	return p
}

func (f *parityFixture) activeSub(t *testing.T, code string, at time.Time, items ...domain.SubscriptionItem) domain.Subscription {
	t.Helper()
	sub, err := f.subs.Create(clock.WithEffectiveNow(f.ctx, at), f.tenantID, domain.Subscription{
		Code: code, DisplayName: code, CustomerID: f.custID,
		Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
		StartedAt:                 &parityPS,
		CurrentBillingPeriodStart: &parityPS,
		CurrentBillingPeriodEnd:   &parityPE,
		NextBillingAt:             &parityPE,
		Items:                     items,
	})
	if err != nil {
		t.Fatalf("create sub %s: %v", code, err)
	}
	return sub
}

func (f *parityFixture) engine(t *testing.T, mode string, now time.Time) *billing.Engine {
	t.Helper()
	e := billing.NewEngine(
		&subStoreAdapter{f.subs},
		&usageStoreAdapter{usage.NewPostgresStore(f.db)},
		&pricingStoreAdapter{f.pricing},
		&invoiceStoreAdapter{f.invoices},
		nil, tenant.NewSettingsStore(f.db), testPaymentSetupsNoPM{}, testChargerSentinel{}, clock.NewFake(now),
	)
	e.SetTaxProviderResolver(tax.NewResolver(nil))
	e.SetNoPaymentMethodNotifier(&testNoPMNotifier{})
	e.SetIntervalReader(f.subs, mode)
	return e
}

// lineFingerprint is the cross-mode comparison unit: everything about a
// base-fee line except IDs and invoice linkage.
func lineFingerprint(li domain.InvoiceLineItem) string {
	ps, pe := "-", "-"
	if li.BillingPeriodStart != nil {
		ps = li.BillingPeriodStart.UTC().Format(time.RFC3339)
	}
	if li.BillingPeriodEnd != nil {
		pe = li.BillingPeriodEnd.UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf("%s|%s|%d|%d|%s|%s", li.LineType, li.Description, li.Quantity, li.AmountCents, ps, pe)
}

func (f *parityFixture) invoiceLines(t *testing.T, subID string) []string {
	t.Helper()
	invs, _, err := f.invoices.List(f.ctx, invoice.ListFilter{TenantID: f.tenantID})
	if err != nil {
		t.Fatalf("list invoices: %v", err)
	}
	var out []string
	for _, iv := range invs {
		if iv.SubscriptionID != subID {
			continue
		}
		lines, err := f.invoices.ListLineItems(f.ctx, f.tenantID, iv.ID)
		if err != nil {
			t.Fatalf("list invoice lines: %v", err)
		}
		for _, li := range lines {
			out = append(out, lineFingerprint(li))
		}
	}
	sort.Strings(out)
	return out
}

// parityScenario builds one mutation shape and returns the sub to bill.
// cancelDay != 0 routes through BillOnCancel instead of the cycle close.
type parityScenario struct {
	name      string
	cancelDay int
	build     func(t *testing.T, f *parityFixture) domain.Subscription
}

func parityCorpus() []parityScenario {
	day := func(n int) time.Time { return parityPS.AddDate(0, 0, n) }
	return []parityScenario{
		{name: "full-period", build: func(t *testing.T, f *parityFixture) domain.Subscription {
			p := f.plan(t, "fp", 3100)
			return f.activeSub(t, "sub-fp", parityPS, domain.SubscriptionItem{PlanID: p.ID, Quantity: 2})
		}},
		{name: "creation-day-clamp", build: func(t *testing.T, f *parityFixture) domain.Subscription {
			// Created 14:00 on the period-start day: the stored period
			// start is the day-snapped midnight, the 'add' fact row is
			// 14:00 — legacy clamps at read, intervals opened at the
			// stored start at write.
			p := f.plan(t, "cd", 2900)
			return f.activeSub(t, "sub-cd", parityPS.Add(14*time.Hour), domain.SubscriptionItem{PlanID: p.ID, Quantity: 1})
		}},
		{name: "mid-period-add", build: func(t *testing.T, f *parityFixture) domain.Subscription {
			pA := f.plan(t, "ma-a", 3100)
			pB := f.plan(t, "ma-b", 6200)
			sub := f.activeSub(t, "sub-ma", parityPS, domain.SubscriptionItem{PlanID: pA.ID, Quantity: 1})
			if _, err := f.subs.AddItem(clock.WithEffectiveNow(f.ctx, day(10)), f.tenantID, domain.SubscriptionItem{SubscriptionID: sub.ID, PlanID: pB.ID, Quantity: 1}); err != nil {
				t.Fatalf("add item: %v", err)
			}
			return sub
		}},
		{name: "quantity-change", build: func(t *testing.T, f *parityFixture) domain.Subscription {
			p := f.plan(t, "qc", 3100)
			sub := f.activeSub(t, "sub-qc", parityPS, domain.SubscriptionItem{PlanID: p.ID, Quantity: 1})
			if _, err := f.subs.UpdateItemQuantity(clock.WithEffectiveNow(f.ctx, day(8)), f.tenantID, sub.Items[0].ID, 5); err != nil {
				t.Fatalf("qty: %v", err)
			}
			return sub
		}},
		{name: "plan-swap-immediate", build: func(t *testing.T, f *parityFixture) domain.Subscription {
			pA := f.plan(t, "ps-a", 3100)
			pB := f.plan(t, "ps-b", 9300)
			sub := f.activeSub(t, "sub-ps", parityPS, domain.SubscriptionItem{PlanID: pA.ID, Quantity: 1})
			if _, err := f.subs.ApplyItemPlanImmediately(f.ctx, f.tenantID, sub.Items[0].ID, pB.ID, day(12)); err != nil {
				t.Fatalf("swap: %v", err)
			}
			return sub
		}},
		{name: "scheduled-swap-at-boundary", build: func(t *testing.T, f *parityFixture) domain.Subscription {
			pA := f.plan(t, "ss-a", 3100)
			pB := f.plan(t, "ss-b", 9300)
			sub := f.activeSub(t, "sub-ss", parityPS, domain.SubscriptionItem{PlanID: pA.ID, Quantity: 1})
			if _, err := f.subs.SetItemPendingPlan(f.ctx, f.tenantID, sub.Items[0].ID, pB.ID, parityPE); err != nil {
				t.Fatalf("schedule swap: %v", err)
			}
			return sub
		}},
		{name: "remove-mid-period", build: func(t *testing.T, f *parityFixture) domain.Subscription {
			pA := f.plan(t, "rm-a", 3100)
			pB := f.plan(t, "rm-b", 6200)
			sub := f.activeSub(t, "sub-rm", parityPS,
				domain.SubscriptionItem{PlanID: pA.ID, Quantity: 1},
				domain.SubscriptionItem{PlanID: pB.ID, Quantity: 1})
			if err := f.subs.RemoveItem(clock.WithEffectiveNow(f.ctx, day(15)), f.tenantID, sub.Items[1].ID); err != nil {
				t.Fatalf("remove: %v", err)
			}
			return sub
		}},
		{name: "same-instant-add-and-swap", build: func(t *testing.T, f *parityFixture) domain.Subscription {
			// A frozen sim clock lands an add and a plan swap on ONE
			// instant — the shape that exposed the id-order tie bug on
			// real data. Both readers must bill the POST-swap plan.
			pA := f.plan(t, "si-a", 3100)
			pB := f.plan(t, "si-b", 6200)
			pC := f.plan(t, "si-c", 9300)
			sub := f.activeSub(t, "sub-si", parityPS, domain.SubscriptionItem{PlanID: pA.ID, Quantity: 1})
			frozen := clock.WithEffectiveNow(f.ctx, day(6))
			it, err := f.subs.AddItem(frozen, f.tenantID, domain.SubscriptionItem{SubscriptionID: sub.ID, PlanID: pB.ID, Quantity: 1})
			if err != nil {
				t.Fatalf("add: %v", err)
			}
			if _, err := f.subs.ApplyItemPlanImmediately(frozen, f.tenantID, it.ID, pC.ID, day(6)); err != nil {
				t.Fatalf("same-instant swap: %v", err)
			}
			return sub
		}},
		{name: "cancel-mid-period", cancelDay: 20, build: func(t *testing.T, f *parityFixture) domain.Subscription {
			pA := f.plan(t, "cx-a", 3100)
			sub := f.activeSub(t, "sub-cx", parityPS, domain.SubscriptionItem{PlanID: pA.ID, Quantity: 2})
			if _, err := f.subs.UpdateItemQuantity(clock.WithEffectiveNow(f.ctx, day(9)), f.tenantID, sub.Items[0].ID, 3); err != nil {
				t.Fatalf("qty: %v", err)
			}
			return sub
		}},
	}
}

// runParityScenario executes one shape under one mode and returns the
// sub's sorted line fingerprints.
func runParityScenario(t *testing.T, db *postgres.DB, sc parityScenario, mode string) (lines []string, allowlisted, unexplained uint64) {
	t.Helper()
	f := newParityFixture(t, db, sc.name+"-"+mode)
	sub := sc.build(t, f)

	if sc.cancelDay > 0 {
		at := parityPS.AddDate(0, 0, sc.cancelDay)
		canceled, err := f.subs.CancelAtomic(clock.WithEffectiveNow(f.ctx, at), f.tenantID, sub.ID)
		if err != nil {
			t.Fatalf("cancel: %v", err)
		}
		eng := f.engine(t, mode, at.Add(time.Minute))
		// The in_arrears final-on-cancel invoice (the segment-walk path).
		if _, err := eng.BillFinalOnImmediateCancel(f.ctx, canceled); err != nil {
			t.Fatalf("bill final on cancel (%s): %v", mode, err)
		}
		_, al, un := eng.ShadowParityStats()
		return f.invoiceLines(t, sub.ID), al, un
	}

	eng := f.engine(t, mode, parityPE.Add(time.Nanosecond))
	count, errs := eng.RunCycleForTenant(f.ctx, f.tenantID, 50)
	if len(errs) > 0 {
		t.Fatalf("cycle close (%s): %v", mode, errs)
	}
	if count == 0 {
		t.Fatalf("cycle close (%s): no invoice generated", mode)
	}
	_, al, un := eng.ShadowParityStats()
	return f.invoiceLines(t, sub.ID), al, un
}

// TestIntervalParity_Corpus is the ADR-101 CI hard gate: for every
// corpus shape, (a) the comparator reports zero unexplained divergence
// in both modes, and (b) the invoices billed by mode=shadow (legacy
// math) and mode=on (interval reader) are line-for-line identical.
func TestIntervalParity_Corpus(t *testing.T) {
	db := testutil.SetupTestDB(t)
	for _, sc := range parityCorpus() {
		t.Run(sc.name, func(t *testing.T) {
			legacyLines, alS, unS := runParityScenario(t, db, sc, billing.IntervalReaderShadow)
			intervalLines, alO, unO := runParityScenario(t, db, sc, billing.IntervalReaderOn)
			if unS != 0 || unO != 0 {
				t.Fatalf("unexplained divergence: shadow=%d on=%d", unS, unO)
			}
			if alS != 0 || alO != 0 {
				t.Fatalf("no corpus shape should hit an allowlist class: shadow=%d on=%d", alS, alO)
			}
			if strings.Join(legacyLines, "\n") != strings.Join(intervalLines, "\n") {
				t.Fatalf("mode=shadow and mode=on billed differently:\nlegacy:\n%s\nintervals:\n%s",
					strings.Join(legacyLines, "\n"), strings.Join(intervalLines, "\n"))
			}
			if len(legacyLines) == 0 {
				t.Fatal("scenario produced no lines — corpus shape is not exercising billing")
			}
		})
	}
}

// TestIntervalParity_CatchupLifetimeAllowlisted: an item added AFTER
// the closing window (engine-down catch-up interleave) is billed for
// the full window by the legacy walk — a registered bug — while its
// interval lifetime correctly starts later. The comparator must
// classify (not scream), and the interval reader must NOT bill the
// phantom line.
func TestIntervalParity_CatchupLifetimeAllowlisted(t *testing.T) {
	db := testutil.SetupTestDB(t)

	build := func(f *parityFixture) (domain.Subscription, domain.Plan) {
		pA := f.plan(t, "cu-a", 3100)
		pB := f.plan(t, "cu-b", 770000) // loud in any line it appears on
		sub := f.activeSub(t, "sub-cu", parityPS, domain.SubscriptionItem{PlanID: pA.ID, Quantity: 1})
		if _, err := f.subs.AddItem(clock.WithEffectiveNow(f.ctx, parityPE.AddDate(0, 0, 2)), f.tenantID,
			domain.SubscriptionItem{SubscriptionID: sub.ID, PlanID: pB.ID, Quantity: 1}); err != nil {
			t.Fatalf("late add: %v", err)
		}
		return sub, pB
	}

	// Shadow: legacy bills the phantom full-window line (current
	// behavior preserved), comparator classifies it as the known class.
	fS := newParityFixture(t, db, "cu-shadow")
	subS, pBS := build(fS)
	engS := fS.engine(t, billing.IntervalReaderShadow, parityPE.Add(time.Nanosecond))
	if _, errs := engS.RunCycleForTenant(fS.ctx, fS.tenantID, 50); len(errs) > 0 {
		t.Fatalf("shadow close: %v", errs)
	}
	_, alS, unS := engS.ShadowParityStats()
	if unS != 0 || alS == 0 {
		t.Fatalf("catch-up shape must be allowlisted, not unexplained: allowlisted=%d unexplained=%d", alS, unS)
	}
	if lines := fS.invoiceLines(t, subS.ID); !strings.Contains(strings.Join(lines, "\n"), pBS.Name) {
		t.Fatalf("shadow mode must preserve legacy behavior (phantom line present): %v", lines)
	}

	// On: the interval reader bills only what existed in the window.
	fO := newParityFixture(t, db, "cu-on")
	subO, pBO := build(fO)
	engO := fO.engine(t, billing.IntervalReaderOn, parityPE.Add(time.Nanosecond))
	if _, errs := engO.RunCycleForTenant(fO.ctx, fO.tenantID, 50); len(errs) > 0 {
		t.Fatalf("on close: %v", errs)
	}
	if lines := fO.invoiceLines(t, subO.ID); strings.Contains(strings.Join(lines, "\n"), pBO.Name) {
		t.Fatalf("interval reader must not bill an item added after the window: %v", lines)
	}
}

// TestIntervalParity_MissingIntervalRowsLoud: reader mode "on" refuses
// to bill an active item that has NO interval rows at all (writer bug
// = silent zero forever); shadow mode records it as unexplained but
// keeps billing legacy.
func TestIntervalParity_MissingIntervalRowsLoud(t *testing.T) {
	db := testutil.SetupTestDB(t)
	f := newParityFixture(t, db, "missing")
	p := f.plan(t, "mi", 3100)
	sub := f.activeSub(t, "sub-mi", parityPS, domain.SubscriptionItem{PlanID: p.ID, Quantity: 1})

	tx, err := db.BeginTx(f.ctx, postgres.TxTenant, f.tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(f.ctx, `DELETE FROM billing_intervals WHERE subscription_id = $1`, sub.ID); err != nil {
		t.Fatalf("wipe intervals: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	engS := f.engine(t, billing.IntervalReaderShadow, parityPE.Add(time.Nanosecond))
	if _, errs := engS.RunCycleForTenant(f.ctx, f.tenantID, 50); len(errs) > 0 {
		t.Fatalf("shadow mode must keep billing on missing rows: %v", errs)
	}
	_, _, un := engS.ShadowParityStats()
	if un == 0 {
		t.Fatal("missing interval rows must count as unexplained divergence in shadow mode")
	}

	// Fresh sub, same corruption, reader on: the close must fail loud.
	f2 := newParityFixture(t, db, "missing-on")
	p2 := f2.plan(t, "mi2", 3100)
	sub2 := f2.activeSub(t, "sub-mi2", parityPS, domain.SubscriptionItem{PlanID: p2.ID, Quantity: 1})
	tx2, err := db.BeginTx(f2.ctx, postgres.TxTenant, f2.tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx2)
	if _, err := tx2.ExecContext(f2.ctx, `DELETE FROM billing_intervals WHERE subscription_id = $1`, sub2.ID); err != nil {
		t.Fatalf("wipe: %v", err)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	engO := f2.engine(t, billing.IntervalReaderOn, parityPE.Add(time.Nanosecond))
	_, errs := engO.RunCycleForTenant(f2.ctx, f2.tenantID, 50)
	if len(errs) == 0 {
		t.Fatal("reader mode on must fail loud on an active item with no interval rows")
	}
	if !strings.Contains(fmt.Sprint(errs), "no interval rows") {
		t.Fatalf("error must name the missing-interval invariant: %v", errs)
	}
}

// TestIntervalParity_TZClampMissAllowlisted: the tenant timezone
// changes AFTER an add was clamped at write time — the legacy reader
// re-evaluates the calendar day in the NEW zone and disagrees. The
// write-time decision is the correct one (ADR-101's structural fix);
// the comparator classifies instead of screaming.
func TestIntervalParity_TZClampMissAllowlisted(t *testing.T) {
	db := testutil.SetupTestDB(t)
	f := newParityFixture(t, db, "tz")
	settings := tenant.NewSettingsStore(db)
	setTZ := func(tz string) {
		t.Helper()
		ts, err := settings.Get(f.ctx, f.tenantID) // defaults for the rest
		if err != nil {
			t.Fatalf("get settings: %v", err)
		}
		ts.TenantID = f.tenantID
		ts.Timezone = tz
		if _, err := settings.Upsert(f.ctx, ts); err != nil {
			t.Fatalf("set tz %s: %v", tz, err)
		}
	}
	setTZ("Asia/Kolkata")

	pA := f.plan(t, "tz-a", 3100)
	pB := f.plan(t, "tz-b", 6200)
	sub := f.activeSub(t, "sub-tz", parityPS, domain.SubscriptionItem{PlanID: pA.ID, Quantity: 1})
	// 2026-05-01 20:00 UTC is 2026-05-02 01:30 in Kolkata — NOT the
	// period-start calendar day there, so the write does not clamp; in
	// UTC (the later zone) it IS the same day, so the legacy read
	// clamps. Write-time truth: open at the raw instant.
	addAt := parityPS.Add(20 * time.Hour)
	if _, err := f.subs.AddItem(clock.WithEffectiveNow(f.ctx, addAt), f.tenantID,
		domain.SubscriptionItem{SubscriptionID: sub.ID, PlanID: pB.ID, Quantity: 1}); err != nil {
		t.Fatalf("add: %v", err)
	}
	setTZ("UTC")

	eng := f.engine(t, billing.IntervalReaderShadow, parityPE.Add(time.Nanosecond))
	if _, errs := eng.RunCycleForTenant(f.ctx, f.tenantID, 50); len(errs) > 0 {
		t.Fatalf("close: %v", errs)
	}
	_, al, un := eng.ShadowParityStats()
	if un != 0 || al == 0 {
		t.Fatalf("tz clamp-miss must be allowlisted, not unexplained: allowlisted=%d unexplained=%d", al, un)
	}
}
