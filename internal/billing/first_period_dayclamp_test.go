package billing

// Day-grade 'add' clamp tests (ADR-012 amendment 2026-07-26).
//
// The DB trigger (migrations 0029/0129) stamps every subscription_items
// INSERT with an 'add' change row at the RAW creation instant, while
// ADR-012 snaps a sub's first period start BACKWARD to the signup day's
// midnight in tenant TZ. Pre-fix, the segment walk read that row as a
// mid-period add and re-prorated a full first period ("prorated 30/31
// days" on a full month — the exact defect ADR-012 removed; found live
// on FLOW I1, invoice NIM-000247, 2026-07-26). Every existing test
// masked the collision by seeding no 'add' rows or backdating periods
// past the creation instant; these tests stage the trigger row INSIDE
// the window.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
)

// tzSettings is mockSettings with a tenant timezone, so tenantLocation
// resolves a real zone instead of the UTC fallback.
type tzSettings struct {
	mockSettings
	tz string
}

func (s *tzSettings) Get(_ context.Context, _ string) (domain.TenantSettings, error) {
	return domain.TenantSettings{Timezone: s.tz}, nil
}

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("load location %s: %v", name, err)
	}
	return loc
}

// The live I1 fixture: tenant TZ Asia/Kolkata, anniversary-monthly sub
// created 2026-07-26 21:05 IST. ADR-012 snapped the first period to
// Jul 26 00:00 → Aug 26 00:00 IST (a full 31-day month).
var (
	istPeriodStart = time.Date(2026, 7, 25, 18, 30, 0, 0, time.UTC) // Jul 26 00:00 IST
	istPeriodEnd   = time.Date(2026, 8, 25, 18, 30, 0, 0, time.UTC) // Aug 26 00:00 IST
	istCreatedAt   = time.Date(2026, 7, 26, 15, 35, 43, 0, time.UTC)
)

func creationAddRow(changedAt time.Time, planID string, qty int64) domain.SubscriptionItemChange {
	return domain.SubscriptionItemChange{
		ID: "vlx_sic_add", TenantID: "t1", SubscriptionID: "sub_1",
		SubscriptionItemID: "it_1", ChangeType: "add",
		ToPlanID: planID, ToQuantity: qty, ChangedAt: changedAt,
	}
}

func TestRunCycle_FirstPeriod_CreationAddRow_BillsFullBase(t *testing.T) {
	periodStart, periodEnd := istPeriodStart, istPeriodEnd

	subs := &mockSubs{
		subs: map[string]domain.Subscription{
			"sub_1": {
				ID: "sub_1", TenantID: "t1", CustomerID: "cus_1",
				Items: []domain.SubscriptionItem{{
					ID: "it_1", PlanID: "pln_i1", Quantity: 1,
				}},
				Status:      domain.SubscriptionActive,
				BillingTime: domain.BillingTimeAnniversary, BillingAnchorDay: 26,
				CurrentBillingPeriodStart: &periodStart, CurrentBillingPeriodEnd: &periodEnd,
				NextBillingAt: &periodEnd,
			},
		},
		cycleUpdated: make(map[string]bool),
		// The creation 'add' row exactly as trigger 0029/0129 stamps it:
		// changed_at = the raw creation instant, 21:05 IST — strictly
		// inside the (periodStart, periodEnd] window.
		itemChanges: []domain.SubscriptionItemChange{
			creationAddRow(istCreatedAt, "pln_i1", 1),
		},
	}

	pricing := &mockPricing{
		plans: map[string]domain.Plan{
			"pln_i1": {ID: "pln_i1", Name: "I1 Multi-Meter", Currency: "USD",
				BillingInterval: domain.BillingMonthly, BaseAmountCents: 2900,
				BaseBillTiming: domain.BillInArrears, MeterIDs: []string{"mtr_api"}},
		},
		meters: map[string]domain.Meter{
			"mtr_api": {ID: "mtr_api", Key: "i1_api", Name: "I1 API Calls",
				Unit: "calls", Aggregation: "sum", RatingRuleVersionID: "rrv_api"},
		},
		rules: map[string]domain.RatingRuleVersion{
			"rrv_api": {ID: "rrv_api", RuleKey: "i1_api_rate", Version: 1,
				Mode: domain.PricingFlat, FlatAmountCents: decimal.NewFromInt(1)},
		},
	}
	usage := &mockUsage{totals: map[string]int64{"mtr_api": 2000}}
	invoices := &mockInvoices{}
	engine := wireBaseTax(NewEngine(subs, usage, pricing, invoices, nil,
		&tzSettings{tz: "Asia/Kolkata"}, nil, nil,
		clock.NewFake(periodEnd.Add(time.Second))))

	count, errs := engine.RunCycle(context.Background(), 50)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if count != 1 {
		t.Fatalf("invoices generated: got %d, want 1", count)
	}

	inv := invoices.invoices[0]
	// Full base 2900 + usage 2000×$0.01 = 2000 → 4900. Pre-fix: base
	// prorated 30/31 = 2806 → subtotal 4806.
	if inv.SubtotalCents != 4900 {
		t.Errorf("subtotal: got %d, want 4900 (FULL $29 base + $20 usage; 4806 = the ADR-012 regression)", inv.SubtotalCents)
	}

	var baseLines, usageLines int
	for _, li := range invoices.lineItems {
		if li.InvoiceID != inv.ID {
			continue
		}
		switch li.LineType {
		case domain.LineTypeBaseFee:
			baseLines++
			if li.AmountCents != 2900 {
				t.Errorf("base amount: got %d, want 2900 (full month)", li.AmountCents)
			}
			if strings.Contains(li.Description, "prorated") {
				t.Errorf("base description claims proration on a full first period: %q", li.Description)
			}
			if li.BillingPeriodStart == nil || !li.BillingPeriodStart.Equal(periodStart) {
				t.Errorf("base line period start: got %v, want %v (the invoice period, not the creation instant)", li.BillingPeriodStart, periodStart)
			}
		case domain.LineTypeUsage:
			usageLines++
			if li.BillingPeriodStart == nil || !li.BillingPeriodStart.Equal(periodStart) {
				t.Errorf("usage line period start: got %v, want %v (the invoice period, not the creation instant)", li.BillingPeriodStart, periodStart)
			}
		}
	}
	if baseLines != 1 {
		t.Errorf("base-fee lines: got %d, want 1", baseLines)
	}
	if usageLines != 1 {
		t.Errorf("usage lines: got %d, want 1", usageLines)
	}
}

// TestBillFinalOnImmediateCancel_FirstPeriod_SameDayCancel bills one
// day-grade day, not $0. Pre-fix the segment was [creation 09:00,
// cancel 15:00] → roundDays(6h)=0 → the base line was silently
// dropped and (with no usage) no final invoice existed at all. Day-
// grade (ADR-012): the signup day counts whole → 1/31 of $29 = $0.94.
func TestBillFinalOnImmediateCancel_FirstPeriod_SameDayCancel(t *testing.T) {
	periodStart, periodEnd := istPeriodStart, istPeriodEnd
	cancelAt := time.Date(2026, 7, 26, 9, 30, 0, 0, time.UTC) // 15:00 IST (created 09:00 IST the same day)

	sub := domain.Subscription{
		ID: "sub_1", TenantID: "t1", CustomerID: "cus_1", Code: "same-day",
		Status: domain.SubscriptionCanceled,
		Items: []domain.SubscriptionItem{{
			ID: "it_1", PlanID: "pln_i1", Quantity: 1,
		}},
		BillingTime: domain.BillingTimeAnniversary, BillingAnchorDay: 26,
		CurrentBillingPeriodStart: &periodStart,
		CurrentBillingPeriodEnd:   &periodEnd,
		CanceledAt:                &cancelAt,
	}

	// Post-ADR-101-Phase-4 the day-grade decision is the WRITER's: the
	// creation-day add opened the interval at periodStart, so the cancel
	// window [periodStart, cancelAt] bills the one consumed signup day.
	// mockSubs synthesizes exactly that open interval from the item.
	subs := &mockSubs{
		subs:         map[string]domain.Subscription{"sub_1": sub},
		cycleUpdated: make(map[string]bool),
	}
	pricing := &mockPricing{
		plans: map[string]domain.Plan{
			"pln_i1": {ID: "pln_i1", Name: "I1 Multi-Meter", Currency: "USD",
				BillingInterval: domain.BillingMonthly, BaseAmountCents: 2900,
				BaseBillTiming: domain.BillInArrears},
		},
	}
	invoices := &mockInvoices{}
	engine := wireBaseTax(NewEngine(subs, &mockUsage{totals: map[string]int64{}},
		pricing, invoices, nil, &tzSettings{tz: "Asia/Kolkata"}, nil, nil,
		clock.NewFake(cancelAt.Add(time.Minute))))

	inv, err := engine.BillFinalOnImmediateCancel(context.Background(), sub)
	if err != nil {
		t.Fatalf("BillFinalOnImmediateCancel: %v", err)
	}
	if inv.ID == "" {
		t.Fatal("no final invoice — the consumed signup day went unbilled (pre-clamp roundDays(6h)=0 shape)")
	}
	// RoundHalfToEven(2900×1, 31) = 94.
	if inv.SubtotalCents != 94 {
		t.Errorf("subtotal: got %d, want 94 (1/31 of $29 — the signup day counts whole)", inv.SubtotalCents)
	}
}
