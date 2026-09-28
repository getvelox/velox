package usage

import (
	"context"
	"testing"
	"time"

	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
)

// TestIngest_LateEventCounted pins the late-event visibility contract
// (2026-07-10 design review): a live event stamped >24h in the past is
// ACCEPTED (late events are industry-standard; a hard reject breaks retries)
// but counted on velox_usage_late_event_total + WARNed, because it may fall
// inside an already-finalized period where it is stored-but-unbilled. A
// fresh event is not counted; backfill-origin events are excluded (the
// documented-safe intentional path).
func TestIngest_LateEventCounted(t *testing.T) {
	svc := NewService(newMemStore())
	ctx := context.Background()

	apiCount := func() float64 {
		return promtestutil.ToFloat64(lateUsageEvents.WithLabelValues("api"))
	}
	backfillCount := func() float64 {
		return promtestutil.ToFloat64(lateUsageEvents.WithLabelValues("backfill"))
	}

	baseAPI, baseBackfill := apiCount(), backfillCount()

	// Fresh event: no count.
	if _, err := svc.Ingest(ctx, "t1", IngestInput{
		CustomerID: "cus_1", MeterID: "mtr_1", Quantity: dec(1),
	}); err != nil {
		t.Fatalf("fresh ingest: %v", err)
	}
	if got := apiCount() - baseAPI; got != 0 {
		t.Errorf("fresh event must not count as late, got +%v", got)
	}

	// >24h-late live event: accepted + counted.
	late := time.Now().UTC().Add(-25 * time.Hour)
	if _, err := svc.Ingest(ctx, "t1", IngestInput{
		CustomerID: "cus_1", MeterID: "mtr_1", Quantity: dec(1), Timestamp: &late,
	}); err != nil {
		t.Fatalf("late ingest must still be accepted: %v", err)
	}
	if got := apiCount() - baseAPI; got != 1 {
		t.Errorf("late live event must increment the counter once, got +%v", got)
	}

	// Backfill at the same lateness: excluded.
	if _, err := svc.Backfill(ctx, "t1", IngestInput{
		CustomerID: "cus_1", MeterID: "mtr_1", Quantity: dec(1), Timestamp: &late,
	}); err != nil {
		t.Fatalf("backfill ingest: %v", err)
	}
	if got := backfillCount() - baseBackfill; got != 0 {
		t.Errorf("backfill events are deliberate operator posts and must not count, got +%v", got)
	}
}
