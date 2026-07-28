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

// ADR-101 Phase 4 corpus gate: billing_intervals is the ONLY segment
// source, so every walked mutation shape asserts its invoice lines
// against GOLDEN fingerprints — captured from the two-mode parity gate
// that ran on every PR between cutover (#635) and Phase 4, where the
// interval reader billed line-for-line identically to the legacy
// interpretation across this exact corpus. A writer or reader
// regression changes a line and fails the golden.
//
// Usage lines derive from the same per-item segments the base fee
// bills from, so base-line coverage here covers the usage windows
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

func (f *parityFixture) engine(t *testing.T, now time.Time) *billing.Engine {
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
	e.SetIntervalReader(f.subs)
	return e
}

// lineFingerprint is the golden unit: everything about a base-fee line
// except IDs and invoice linkage. Plan codes are per-scenario constants
// so the fingerprints are stable across runs.
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

// intervalScenario builds one mutation shape and pins its expected
// invoice lines. cancelDay != 0 routes through BillOnCancel instead of
// the cycle close.
type intervalScenario struct {
	name      string
	cancelDay int
	build     func(t *testing.T, f *parityFixture) domain.Subscription
	want      []string
}

func intervalCorpus() []intervalScenario {
	day := func(n int) time.Time { return parityPS.AddDate(0, 0, n) }
	return []intervalScenario{
		{name: "full-period", want: []string{
			"base_fee|fp - base fee (qty 2)|2|6200|2026-05-01T00:00:00Z|2026-06-01T00:00:00Z",
		}, build: func(t *testing.T, f *parityFixture) domain.Subscription {
			p := f.plan(t, "fp", 3100)
			return f.activeSub(t, "sub-fp", parityPS, domain.SubscriptionItem{PlanID: p.ID, Quantity: 2})
		}},
		{name: "creation-day-clamp", want: []string{
			"base_fee|cd - base fee (qty 1)|1|2900|2026-05-01T00:00:00Z|2026-06-01T00:00:00Z",
		}, build: func(t *testing.T, f *parityFixture) domain.Subscription {
			// Created 14:00 on the period-start day: the stored period
			// start is the day-snapped midnight; the interval writer
			// opened the lifetime at the stored start (write-time
			// day-grade), so the first period bills the FULL base.
			p := f.plan(t, "cd", 2900)
			return f.activeSub(t, "sub-cd", parityPS.Add(14*time.Hour), domain.SubscriptionItem{PlanID: p.ID, Quantity: 1})
		}},
		{name: "mid-period-add", want: []string{
			"base_fee|ma-a - base fee (qty 1)|1|3100|2026-05-01T00:00:00Z|2026-06-01T00:00:00Z",
			"base_fee|ma-b - base fee (qty 1, prorated 21/31 days)|1|4200|2026-05-11T00:00:00Z|2026-06-01T00:00:00Z",
		}, build: func(t *testing.T, f *parityFixture) domain.Subscription {
			pA := f.plan(t, "ma-a", 3100)
			pB := f.plan(t, "ma-b", 6200)
			sub := f.activeSub(t, "sub-ma", parityPS, domain.SubscriptionItem{PlanID: pA.ID, Quantity: 1})
			if _, err := f.subs.AddItem(clock.WithEffectiveNow(f.ctx, day(10)), f.tenantID, domain.SubscriptionItem{SubscriptionID: sub.ID, PlanID: pB.ID, Quantity: 1}); err != nil {
				t.Fatalf("add item: %v", err)
			}
			return sub
		}},
		{name: "quantity-change", want: []string{
			"base_fee|qc - base fee (qty 1, prorated 8/31 days)|1|800|2026-05-01T00:00:00Z|2026-05-09T00:00:00Z",
			"base_fee|qc - base fee (qty 5, prorated 23/31 days)|5|11500|2026-05-09T00:00:00Z|2026-06-01T00:00:00Z",
		}, build: func(t *testing.T, f *parityFixture) domain.Subscription {
			p := f.plan(t, "qc", 3100)
			sub := f.activeSub(t, "sub-qc", parityPS, domain.SubscriptionItem{PlanID: p.ID, Quantity: 1})
			if _, err := f.subs.UpdateItemQuantity(clock.WithEffectiveNow(f.ctx, day(8)), f.tenantID, sub.Items[0].ID, 5); err != nil {
				t.Fatalf("qty: %v", err)
			}
			return sub
		}},
		{name: "plan-swap-immediate", want: []string{
			"base_fee|ps-a - base fee (qty 1, prorated 12/31 days)|1|1200|2026-05-01T00:00:00Z|2026-05-13T00:00:00Z",
			"base_fee|ps-b - base fee (qty 1, prorated 19/31 days)|1|5700|2026-05-13T00:00:00Z|2026-06-01T00:00:00Z",
		}, build: func(t *testing.T, f *parityFixture) domain.Subscription {
			pA := f.plan(t, "ps-a", 3100)
			pB := f.plan(t, "ps-b", 9300)
			sub := f.activeSub(t, "sub-ps", parityPS, domain.SubscriptionItem{PlanID: pA.ID, Quantity: 1})
			if _, err := f.subs.ApplyItemPlanImmediately(f.ctx, f.tenantID, sub.Items[0].ID, pB.ID, day(12)); err != nil {
				t.Fatalf("swap: %v", err)
			}
			return sub
		}},
		{name: "scheduled-swap-at-boundary", want: []string{
			"base_fee|ss-a - base fee (qty 1)|1|3100|2026-05-01T00:00:00Z|2026-06-01T00:00:00Z",
		}, build: func(t *testing.T, f *parityFixture) domain.Subscription {
			pA := f.plan(t, "ss-a", 3100)
			pB := f.plan(t, "ss-b", 9300)
			sub := f.activeSub(t, "sub-ss", parityPS, domain.SubscriptionItem{PlanID: pA.ID, Quantity: 1})
			if _, err := f.subs.SetItemPendingPlan(f.ctx, f.tenantID, sub.Items[0].ID, pB.ID, parityPE); err != nil {
				t.Fatalf("schedule swap: %v", err)
			}
			return sub
		}},
		{name: "remove-mid-period", want: []string{
			"base_fee|rm-a - base fee (qty 1)|1|3100|2026-05-01T00:00:00Z|2026-06-01T00:00:00Z",
			"base_fee|rm-b - base fee (qty 1, prorated 15/31 days)|1|3000|2026-05-01T00:00:00Z|2026-05-16T00:00:00Z",
		}, build: func(t *testing.T, f *parityFixture) domain.Subscription {
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
		{name: "same-instant-add-and-swap", want: []string{
			"base_fee|si-a - base fee (qty 1)|1|3100|2026-05-01T00:00:00Z|2026-06-01T00:00:00Z",
			"base_fee|si-c - base fee (qty 1, prorated 25/31 days)|1|7500|2026-05-07T00:00:00Z|2026-06-01T00:00:00Z",
		}, build: func(t *testing.T, f *parityFixture) domain.Subscription {
			// A frozen sim clock lands an add and a plan swap on ONE
			// instant — the shape that exposed the id-order tie bug on
			// real data. The reader must bill the POST-swap plan.
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
		{name: "cancel-mid-period", want: []string{
			"base_fee|cx-a - base fee (qty 2, prorated 9/31 days)|2|1800|2026-05-01T00:00:00Z|2026-05-10T00:00:00Z",
			"base_fee|cx-a - base fee (qty 3, prorated 11/31 days)|3|3300|2026-05-10T00:00:00Z|2026-05-21T00:00:00Z",
		}, cancelDay: 20, build: func(t *testing.T, f *parityFixture) domain.Subscription {
			pA := f.plan(t, "cx-a", 3100)
			sub := f.activeSub(t, "sub-cx", parityPS, domain.SubscriptionItem{PlanID: pA.ID, Quantity: 2})
			if _, err := f.subs.UpdateItemQuantity(clock.WithEffectiveNow(f.ctx, day(9)), f.tenantID, sub.Items[0].ID, 3); err != nil {
				t.Fatalf("qty: %v", err)
			}
			return sub
		}},
	}
}

// runIntervalScenario executes one shape and returns the sub's sorted
// line fingerprints.
func runIntervalScenario(t *testing.T, db *postgres.DB, sc intervalScenario) []string {
	t.Helper()
	f := newParityFixture(t, db, sc.name)
	sub := sc.build(t, f)

	if sc.cancelDay > 0 {
		at := parityPS.AddDate(0, 0, sc.cancelDay)
		canceled, err := f.subs.CancelAtomic(clock.WithEffectiveNow(f.ctx, at), f.tenantID, sub.ID)
		if err != nil {
			t.Fatalf("cancel: %v", err)
		}
		eng := f.engine(t, at.Add(time.Minute))
		// The in_arrears final-on-cancel invoice (the segment-walk path).
		if _, err := eng.BillFinalOnImmediateCancel(f.ctx, canceled); err != nil {
			t.Fatalf("bill final on cancel: %v", err)
		}
		return f.invoiceLines(t, sub.ID)
	}

	eng := f.engine(t, parityPE.Add(time.Nanosecond))
	count, errs := eng.RunCycleForTenant(f.ctx, f.tenantID, 50)
	if len(errs) > 0 {
		t.Fatalf("cycle close: %v", errs)
	}
	if count == 0 {
		t.Fatal("cycle close: no invoice generated")
	}
	return f.invoiceLines(t, sub.ID)
}

// TestIntervalReader_Corpus is the ADR-101 CI hard gate post-Phase-4:
// each corpus shape's invoice lines must match the pinned goldens.
func TestIntervalReader_Corpus(t *testing.T) {
	db := testutil.SetupTestDB(t)
	for _, sc := range intervalCorpus() {
		t.Run(sc.name, func(t *testing.T) {
			got := runIntervalScenario(t, db, sc)
			if len(got) == 0 {
				t.Fatal("scenario produced no lines — corpus shape is not exercising billing")
			}
			if len(sc.want) == 0 {
				t.Fatalf("golden missing for %s — captured lines:\n%s", sc.name, strings.Join(got, "\n"))
			}
			if strings.Join(got, "\n") != strings.Join(sc.want, "\n") {
				t.Fatalf("lines diverge from golden:\ngot:\n%s\nwant:\n%s",
					strings.Join(got, "\n"), strings.Join(sc.want, "\n"))
			}
		})
	}
}

// TestIntervalReader_CatchupLifetimeOutsideWindow: an item added AFTER
// the closing window (engine-down catch-up interleave) must NOT be
// billed for that window — its interval lifetime starts later. This is
// the shape the legacy fact-log walk got wrong (billed the phantom
// full window); the interval reader fixes it structurally.
func TestIntervalReader_CatchupLifetimeOutsideWindow(t *testing.T) {
	db := testutil.SetupTestDB(t)
	f := newParityFixture(t, db, "cu")
	pA := f.plan(t, "cu-a", 3100)
	pB := f.plan(t, "cu-b", 770000) // loud in any line it appears on
	sub := f.activeSub(t, "sub-cu", parityPS, domain.SubscriptionItem{PlanID: pA.ID, Quantity: 1})
	if _, err := f.subs.AddItem(clock.WithEffectiveNow(f.ctx, parityPE.AddDate(0, 0, 2)), f.tenantID,
		domain.SubscriptionItem{SubscriptionID: sub.ID, PlanID: pB.ID, Quantity: 1}); err != nil {
		t.Fatalf("late add: %v", err)
	}

	eng := f.engine(t, parityPE.Add(time.Nanosecond))
	if _, errs := eng.RunCycleForTenant(f.ctx, f.tenantID, 50); len(errs) > 0 {
		t.Fatalf("close: %v", errs)
	}
	lines := f.invoiceLines(t, sub.ID)
	if strings.Contains(strings.Join(lines, "\n"), pB.Name) {
		t.Fatalf("reader must not bill an item added after the window: %v", lines)
	}
	if !strings.Contains(strings.Join(lines, "\n"), pA.Name) {
		t.Fatalf("the in-window item must still bill: %v", lines)
	}
}

// TestIntervalReader_MissingIntervalRowsLoud: the reader refuses to
// bill an active item that has NO interval rows at all — that is a
// writer bug, and silence would bill the item zero forever.
func TestIntervalReader_MissingIntervalRowsLoud(t *testing.T) {
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

	eng := f.engine(t, parityPE.Add(time.Nanosecond))
	_, errs := eng.RunCycleForTenant(f.ctx, f.tenantID, 50)
	if len(errs) == 0 {
		t.Fatal("the reader must fail loud on an active item with no interval rows")
	}
	if !strings.Contains(fmt.Sprint(errs), "no interval rows") {
		t.Fatalf("error must name the missing-interval invariant: %v", errs)
	}
}

// TestIntervalReader_TZChangeDoesNotReinterpret: the day-grade decision
// was made ONCE at write time in the tenant TZ that existed then; a
// later TZ change must not re-render it (the org-TZ clamp-miss class
// ADR-101 §Context names — the legacy read-time walk re-evaluated the
// calendar day in the NEW zone and disagreed with itself).
func TestIntervalReader_TZChangeDoesNotReinterpret(t *testing.T) {
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
	// period-start calendar day there, so the write did NOT clamp: the
	// interval opens at the raw instant. Flipping the tenant to UTC
	// (where it IS the same day) must not change what bills.
	addAt := parityPS.Add(20 * time.Hour)
	if _, err := f.subs.AddItem(clock.WithEffectiveNow(f.ctx, addAt), f.tenantID,
		domain.SubscriptionItem{SubscriptionID: sub.ID, PlanID: pB.ID, Quantity: 1}); err != nil {
		t.Fatalf("add: %v", err)
	}
	setTZ("UTC")

	eng := f.engine(t, parityPE.Add(time.Nanosecond))
	if _, errs := eng.RunCycleForTenant(f.ctx, f.tenantID, 50); len(errs) > 0 {
		t.Fatalf("close: %v", errs)
	}
	joined := strings.Join(f.invoiceLines(t, sub.ID), "\n")
	// The pB line's period starts at the write-time instant, not a
	// re-clamped parityPS.
	if !strings.Contains(joined, addAt.UTC().Format(time.RFC3339)) {
		t.Fatalf("pB must bill from the write-time open instant %s (no TZ reinterpretation):\n%s", addAt.UTC().Format(time.RFC3339), joined)
	}
	if strings.Contains(joined, "tz-b|1|6200|"+parityPS.UTC().Format(time.RFC3339)) {
		t.Fatalf("pB billed a full window — the later TZ change re-clamped the open:\n%s", joined)
	}
}
