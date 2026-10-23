package api

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/usage"
)

// releaseSubs answers one active subscription whose plan prices meterID.
type releaseSubs struct {
	meterID    string
	start, end time.Time
}

func (s releaseSubs) ListLiveByCustomer(context.Context, string, string) ([]domain.Subscription, map[string][]string, error) {
	started := s.start.Add(-time.Hour)
	return []domain.Subscription{{
		ID: "sub_release", Status: domain.SubscriptionActive,
		CurrentBillingPeriodStart: &s.start, CurrentBillingPeriodEnd: &s.end, StartedAt: &started,
		Items: []domain.SubscriptionItem{{PlanID: "pln_release"}},
	}}, map[string][]string{"pln_release": {s.meterID}}, nil
}

// releaseThresholds answers a paid threshold invoice that billed through at.
type releaseThresholds struct{ at time.Time }

func (r releaseThresholds) ThresholdWatermarks(context.Context, string, string, time.Time, time.Time) (*time.Time, *time.Time, error) {
	return &r.at, &r.at, nil
}

// A held-usage release reads its meters' aggregation on its own
// transaction's connection, through the production OpenPeriodReader. Read on
// a second connection while the release's transaction holds one, a pool of
// one connection (or N releases on a pool of N) waits for a connection that
// is never freed.
//
// Mutation-verify: read the aggregation through the adapter's MeterBuckets
// (pricing GetMeter, its own transaction) in releaseBuckets again → the
// release fails at its deadline with nothing released.
func TestRelease_ReadsTheAggregationOnItsOwnConnection(t *testing.T) {
	db := testutil.SetupTestDB(t)
	tenantID := testutil.CreateTestTenant(t, db, "Release one connection")
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 60*time.Second)
	defer cancel()

	usageSvc := usage.NewService(usage.NewPostgresStore(db))
	pricingStore := pricing.NewPostgresStore(db)
	const ext, eventName = "ext_one_conn", "tokens_one_conn"
	now := time.Now().UTC()
	at := now.Add(-time.Hour).Truncate(time.Second)
	for _, key := range []string{"one-conn:input", "one-conn:output"} {
		if _, err := usageSvc.Hold(ctx, tenantID, usage.HoldInput{
			Source: domain.HeldUsageSourceLiteLLM, ExternalCustomerID: ext, EventName: eventName,
			Quantity: decimal.NewFromInt(5), IdempotencyKey: key, EventAt: &at,
			Reason: domain.HeldReasonCustomerNotFound,
		}); err != nil {
			t.Fatalf("hold %s: %v", key, err)
		}
	}
	meter, err := pricing.NewService(pricingStore).CreateMeter(ctx, tenantID, pricing.CreateMeterInput{
		EventName: eventName, Name: eventName, Unit: "tokens", Aggregation: "sum",
	})
	if err != nil {
		t.Fatalf("create meter: %v", err)
	}
	cus, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{DisplayName: "One conn", ExternalID: ext})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	usageSvc.SetOpenPeriodReader(usageOpenPeriods{
		subs:       releaseSubs{meterID: meter.ID, start: now.Add(-2 * time.Hour), end: now.Add(24 * time.Hour)},
		pricing:    pricingStore,
		thresholds: releaseThresholds{at: now.Add(-30 * time.Minute)},
	})

	db.Pool.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Pool.SetMaxOpenConns(5) })
	relCtx, relCancel := context.WithTimeout(ctx, 10*time.Second)
	defer relCancel()
	res, err := usageSvc.Release(relCtx, tenantID, usage.ReleaseInput{
		ExternalCustomerID: ext,
		CustomerID:         cus.ID,
		MeterIDs:           map[string]string{eventName: meter.ID},
	})
	if err != nil {
		t.Fatalf("release on a pool of one connection: %v", err)
	}
	// sum is additive, and the rows are before the paid threshold invoice's
	// end: the close does not count them.
	want := usage.ReleaseResult{CustomerID: cus.ID, Released: 2, OutsideOpenPeriod: 2}
	if res != want {
		t.Errorf("release = %+v, want %+v", res, want)
	}
}
