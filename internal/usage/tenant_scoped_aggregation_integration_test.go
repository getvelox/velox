package usage_test

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/usage"
)

// TestAggregateForBillingPeriodByAgg is the parity contract for the batched
// aggregation. The method used to issue one query PER METER; it now issues one
// per DISTINCT aggregation function, so the risks worth pinning are (a) a mode
// landing on the wrong meter when several share a query, (b) a meter with no
// events in the period leaking in as a zero rather than being absent, and
// (c) the tenant_id predicate — added so Postgres can use the index — changing
// which rows are visible.
//
// (c) is the one that would be a money bug: the predicate is a performance
// fix, and it must be exactly redundant with the RLS the tx already applies.
func TestAggregateForBillingPeriodByAgg(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 30*time.Second)
	defer cancel()

	store := usage.NewPostgresStore(db)

	base := time.Now().Truncate(time.Hour)
	from := base.Add(-1 * time.Hour)
	to := base.Add(1 * time.Hour)

	t.Run("all modes in one call", func(t *testing.T) {
		tenantID := testutil.CreateTestTenant(t, db, "Agg Modes")
		customerID := insertTestCustomer(t, db, tenantID, "cus_modes")

		sumMeter := insertTestMeter(t, db, tenantID, "mtr_sum", "k_sum")
		maxMeter := insertTestMeter(t, db, tenantID, "mtr_max", "k_max")
		cntMeter := insertTestMeter(t, db, tenantID, "mtr_cnt", "k_cnt")
		lastMeter := insertTestMeter(t, db, tenantID, "mtr_last", "k_last")
		emptyMeter := insertTestMeter(t, db, tenantID, "mtr_empty", "k_empty")

		ingest(t, ctx, store, tenantID, customerID, sumMeter, decimal.NewFromInt(10), nil)
		ingest(t, ctx, store, tenantID, customerID, sumMeter, decimal.NewFromInt(20), nil)

		ingest(t, ctx, store, tenantID, customerID, maxMeter, decimal.NewFromInt(5), nil)
		ingest(t, ctx, store, tenantID, customerID, maxMeter, decimal.NewFromInt(50), nil)
		ingest(t, ctx, store, tenantID, customerID, maxMeter, decimal.NewFromInt(7), nil)

		for i := 0; i < 3; i++ {
			ingest(t, ctx, store, tenantID, customerID, cntMeter, decimal.NewFromInt(99), nil)
		}

		// 'last' is the latest event IN the period, not the largest.
		ingestAt(t, ctx, store, tenantID, customerID, lastMeter, decimal.NewFromInt(111), nil, base.Add(-30*time.Minute))
		ingestAt(t, ctx, store, tenantID, customerID, lastMeter, decimal.NewFromInt(4), nil, base.Add(-10*time.Minute))

		got, err := store.AggregateForBillingPeriodByAgg(ctx, tenantID, customerID, map[string]string{
			sumMeter:   "sum",
			maxMeter:   "max",
			cntMeter:   "count",
			lastMeter:  "last",
			emptyMeter: "sum",
		}, from, to)
		if err != nil {
			t.Fatalf("aggregate: %v", err)
		}

		want := map[string]decimal.Decimal{
			sumMeter:  decimal.NewFromInt(30),
			maxMeter:  decimal.NewFromInt(50),
			cntMeter:  decimal.NewFromInt(3),
			lastMeter: decimal.NewFromInt(4),
		}
		assertTotals(t, got, want)

		// A meter with no events is ABSENT, not zero — the billing engine
		// treats presence as "this meter has a billable line".
		if _, present := got[emptyMeter]; present {
			t.Errorf("meter with no events present in totals: %v", got[emptyMeter])
		}
	})

	t.Run("events outside the period are excluded", func(t *testing.T) {
		tenantID := testutil.CreateTestTenant(t, db, "Agg Window")
		customerID := insertTestCustomer(t, db, tenantID, "cus_window")
		meterID := insertTestMeter(t, db, tenantID, "mtr_window", "k_window")

		// The window sits wholly in the past so BOTH bounds can be probed —
		// the service rejects future-dated ingest, so an "after to" event has
		// to still be before now.
		winFrom := base.Add(-3 * time.Hour)
		winTo := base.Add(-1 * time.Hour)

		ingestAt(t, ctx, store, tenantID, customerID, meterID, decimal.NewFromInt(1000), nil, winFrom.Add(-1*time.Hour))
		ingestAt(t, ctx, store, tenantID, customerID, meterID, decimal.NewFromInt(7), nil, winFrom.Add(30*time.Minute))
		ingestAt(t, ctx, store, tenantID, customerID, meterID, decimal.NewFromInt(2000), nil, winTo.Add(30*time.Minute))

		got, err := store.AggregateForBillingPeriodByAgg(ctx, tenantID, customerID,
			map[string]string{meterID: "sum"}, winFrom, winTo)
		if err != nil {
			t.Fatalf("aggregate: %v", err)
		}
		assertTotals(t, got, map[string]decimal.Decimal{meterID: decimal.NewFromInt(7)})
	})

	// The negative control for the tenant_id predicate. Two tenants each with
	// their own customer, meter and events: neither may see the other's rows,
	// and asking for a meter that belongs to the other tenant must return
	// nothing rather than that tenant's total.
	t.Run("another tenant's events never appear", func(t *testing.T) {
		tenantA := testutil.CreateTestTenant(t, db, "Agg Tenant A")
		custA := insertTestCustomer(t, db, tenantA, "cus_a")
		meterA := insertTestMeter(t, db, tenantA, "mtr_a", "k_a")

		tenantB := testutil.CreateTestTenant(t, db, "Agg Tenant B")
		custB := insertTestCustomer(t, db, tenantB, "cus_b")
		meterB := insertTestMeter(t, db, tenantB, "mtr_b", "k_b")

		ingest(t, ctx, store, tenantA, custA, meterA, decimal.NewFromInt(11), nil)
		ingest(t, ctx, store, tenantB, custB, meterB, decimal.NewFromInt(22), nil)

		gotA, err := store.AggregateForBillingPeriodByAgg(ctx, tenantA, custA,
			map[string]string{meterA: "sum"}, from, to)
		if err != nil {
			t.Fatalf("aggregate A: %v", err)
		}
		assertTotals(t, gotA, map[string]decimal.Decimal{meterA: decimal.NewFromInt(11)})

		gotB, err := store.AggregateForBillingPeriodByAgg(ctx, tenantB, custB,
			map[string]string{meterB: "sum"}, from, to)
		if err != nil {
			t.Fatalf("aggregate B: %v", err)
		}
		assertTotals(t, gotB, map[string]decimal.Decimal{meterB: decimal.NewFromInt(22)})

		// Tenant A asking for tenant B's meter and customer: empty, never 22.
		crossed, err := store.AggregateForBillingPeriodByAgg(ctx, tenantA, custB,
			map[string]string{meterB: "sum"}, from, to)
		if err != nil {
			t.Fatalf("aggregate crossed: %v", err)
		}
		if len(crossed) != 0 {
			t.Errorf("cross-tenant aggregate returned %v, want empty", crossed)
		}
	})
}

// TestAggregateByPricingRules_LastEverProbe pins the guard that made the
// method's own doc comment true. The last_ever pass is deliberately unbounded
// in time, and it used to run on every call — including for the overwhelming
// majority of meters that have no last_ever rule. Skipping it must not change
// a single returned row.
func TestAggregateByPricingRules_LastEverProbe(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 30*time.Second)
	defer cancel()

	store := usage.NewPostgresStore(db)
	from := time.Now().Add(-1 * time.Hour)
	to := time.Now().Add(1 * time.Hour)

	t.Run("meter without last_ever rules is unaffected", func(t *testing.T) {
		tenantID := testutil.CreateTestTenant(t, db, "Probe No LastEver")
		customerID := insertTestCustomer(t, db, tenantID, "cus_probe")
		meterID := insertTestMeter(t, db, tenantID, "mtr_probe", "k_probe")
		rrvID := insertTestRatingRule(t, db, tenantID, "rate_probe")
		insertTestPricingRule(t, db, tenantID, meterID, rrvID,
			map[string]any{"model": "gpt-4"}, domain.AggSum, 100)

		ingest(t, ctx, store, tenantID, customerID, meterID, decimal.NewFromInt(10), map[string]any{"model": "gpt-4"})
		ingest(t, ctx, store, tenantID, customerID, meterID, decimal.NewFromInt(5), map[string]any{"model": "claude"})

		got, err := store.AggregateByPricingRules(ctx, tenantID, customerID, meterID, domain.AggSum, from, to)
		if err != nil {
			t.Fatalf("aggregate: %v", err)
		}
		assertRuleAggregations(t, got, map[string]decimal.Decimal{
			rrvID: decimal.NewFromInt(10),
			"":    decimal.NewFromInt(5),
		})
		for _, r := range got {
			if r.AggregationMode == domain.AggLastEver {
				t.Errorf("last_ever row returned for a meter with no last_ever rule: %+v", r)
			}
		}
	})

	// Positive control: with a last_ever rule present the probe must let the
	// pass run, and the value must come from OUTSIDE the period — that is the
	// whole point of the mode, and a probe that skipped wrongly would silently
	// drop the line.
	t.Run("meter with a last_ever rule still gets it", func(t *testing.T) {
		tenantID := testutil.CreateTestTenant(t, db, "Probe LastEver")
		customerID := insertTestCustomer(t, db, tenantID, "cus_probe2")
		meterID := insertTestMeter(t, db, tenantID, "mtr_probe2", "k_probe2")
		rrvID := insertTestRatingRule(t, db, tenantID, "rate_probe2")
		insertTestPricingRule(t, db, tenantID, meterID, rrvID,
			map[string]any{"plan": "seat"}, domain.AggLastEver, 100)

		// Both events are BEFORE the period window.
		ingestAt(t, ctx, store, tenantID, customerID, meterID, decimal.NewFromInt(3), map[string]any{"plan": "seat"}, from.Add(-48*time.Hour))
		ingestAt(t, ctx, store, tenantID, customerID, meterID, decimal.NewFromInt(9), map[string]any{"plan": "seat"}, from.Add(-24*time.Hour))

		got, err := store.AggregateByPricingRules(ctx, tenantID, customerID, meterID, domain.AggSum, from, to)
		if err != nil {
			t.Fatalf("aggregate: %v", err)
		}
		assertRuleAggregations(t, got, map[string]decimal.Decimal{
			rrvID: decimal.NewFromInt(9), // most recent across all time
		})
	})
}

func assertTotals(t *testing.T, got, want map[string]decimal.Decimal) {
	t.Helper()
	for meterID, wantVal := range want {
		gotVal, ok := got[meterID]
		if !ok {
			t.Errorf("meter %s missing from totals (got %v)", meterID, got)
			continue
		}
		if !gotVal.Equal(wantVal) {
			t.Errorf("meter %s = %s, want %s", meterID, gotVal, wantVal)
		}
	}
}
