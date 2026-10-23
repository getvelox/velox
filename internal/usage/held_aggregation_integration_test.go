package usage_test

// ADR-130 §2 (amended 2026-10-24), on real Postgres: only usage events fix a
// meter's aggregation. A held row has never been billed, so it does not: a
// meter made with the wrong aggregation after its event name was held can
// still be fixed. A release writes usage_events, and each insert's foreign
// key takes KEY SHARE on the meter row, the lock an ingest takes. The
// aggregation PATCH locks the meter FOR UPDATE before it reads whether the
// meter has usage, so a PATCH and a release run one after the other: a
// released event is never billed under an aggregation that changed after it
// was written.

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/usage"
)

// pausedAudit pauses a write inside its transaction, at its in-tx audit
// emission: a meter PATCH after the meter lock and the usage read, a release
// after its usage_events inserts. It is the concurrent resolver: the other
// write races exactly there.
type pausedAudit struct {
	inTx    chan struct{}
	release chan struct{}
}

func newPausedAudit() pausedAudit {
	return pausedAudit{inTx: make(chan struct{}), release: make(chan struct{})}
}

func (p pausedAudit) LogInTx(context.Context, *sql.Tx, audit.Entry) error {
	close(p.inTx)
	<-p.release
	return nil
}

// waitPaused waits until p's write reached its audit emission.
func waitPaused(t *testing.T, p pausedAudit, done <-chan error, what string) {
	t.Helper()
	select {
	case <-p.inTx:
	case err := <-done:
		t.Fatalf("%s finished before its audit write: %v", what, err)
	case <-time.After(10 * time.Second):
		t.Fatalf("%s never reached its audit write", what)
	}
}

// heldAggFixture is a held fixture with a pricing service on the same
// database, a sum meter for eventName, and two held rows for it.
type heldAggFixture struct {
	heldFixture
	psvc      *pricing.Service
	meter     domain.Meter
	eventName string
}

func newHeldAggFixture(t *testing.T, name, eventName string) heldAggFixture {
	t.Helper()
	f := heldAggFixture{heldFixture: newHeldFixture(t, name), eventName: eventName}
	f.psvc = pricing.NewService(pricing.NewPostgresStore(f.db))
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	for _, key := range []string{"agg-" + eventName + ":input", "agg-" + eventName + ":output"} {
		if _, err := f.svc.Hold(f.ctx, f.tenantID, usage.HoldInput{
			Source: domain.HeldUsageSourceLiteLLM, ExternalCustomerID: "ext_" + eventName, EventName: eventName,
			Quantity: decimal.NewFromInt(5), IdempotencyKey: key, EventAt: &at,
			Reason: domain.HeldReasonCustomerNotFound,
		}); err != nil {
			t.Fatalf("hold %s: %v", key, err)
		}
	}
	m, err := f.psvc.CreateMeter(f.ctx, f.tenantID, pricing.CreateMeterInput{
		EventName: eventName, Name: eventName, Unit: "tokens", Aggregation: "sum",
	})
	if err != nil {
		t.Fatalf("create meter: %v", err)
	}
	f.meter = m
	return f
}

func (f heldAggFixture) patch(agg string) error {
	_, err := f.psvc.UpdateMeter(f.ctx, f.tenantID, f.meter.ID, pricing.UpdateMeterInput{Aggregation: &agg})
	return err
}

func (f heldAggFixture) aggregation(t *testing.T) string {
	t.Helper()
	m, err := f.psvc.GetMeter(f.ctx, f.tenantID, f.meter.ID)
	if err != nil {
		t.Fatalf("get meter: %v", err)
	}
	return m.Aggregation
}

func (f heldAggFixture) hasUsage(t *testing.T) bool {
	t.Helper()
	has, err := f.psvc.MeterHasUsage(f.ctx, f.tenantID, f.meter)
	if err != nil {
		t.Fatalf("has usage: %v", err)
	}
	return has
}

func (f heldAggFixture) releaseAll(customerID string) error {
	_, err := f.svc.Release(f.ctx, f.tenantID, usage.ReleaseInput{
		ExternalCustomerID: "ext_" + f.eventName,
		CustomerID:         customerID,
		MeterIDs:           map[string]string{f.eventName: f.meter.ID},
	})
	return err
}

func (f heldAggFixture) events(t *testing.T) int {
	return f.countEvents(t, "agg-"+f.eventName+":input", "agg-"+f.eventName+":output")
}

func wantMeterHasUsage(t *testing.T, what string, err error) {
	t.Helper()
	if !errors.Is(err, errs.ErrInvalidState) || errs.Code(err) != errs.CodeMeterHasUsage {
		t.Fatalf("%s: want ErrInvalidState with code %s, got %v (code %q)", what, errs.CodeMeterHasUsage, err, errs.Code(err))
	}
}

// Held rows only: the aggregation changes and has_usage is false. After the
// release the rows are usage events, and the aggregation is fixed.
//
// Mutation-verify: count held rows in the has-usage read again → the PATCH
// with only held rows is refused, and this fails.
func TestHeldRows_DoNotFixTheAggregation(t *testing.T) {
	f := newHeldAggFixture(t, "Held agg free", "tokens_free")

	if f.hasUsage(t) {
		t.Error("has_usage with only held rows = true, want false: held rows have never been billed")
	}
	if err := f.patch("max"); err != nil {
		t.Fatalf("PATCH aggregation with only held rows: %v, want it changed", err)
	}
	if got := f.aggregation(t); got != "max" {
		t.Fatalf("aggregation = %q, want max", got)
	}

	customerID := insertTestCustomer(t, f.db, f.tenantID, "ext_"+f.eventName)
	if err := f.releaseAll(customerID); err != nil {
		t.Fatalf("release: %v", err)
	}
	if n := f.events(t); n != 2 {
		t.Fatalf("usage events after the release = %d, want 2", n)
	}
	if !f.hasUsage(t) {
		t.Error("has_usage after the release = false, want true")
	}
	wantMeterHasUsage(t, "PATCH after the release", f.patch("sum"))
	if got := f.aggregation(t); got != "max" {
		t.Errorf("aggregation = %q, want max: the released events are counted under it", got)
	}
}

// C5 for a release: an aggregation PATCH against the release of the meter's
// first events. Whichever takes the meter row first, the other waits, so no
// released event is billed under an aggregation that changed after it was
// written.
//
// Mutation-verify: take away the release's meter lock (drop the
// usage_events → meters foreign key, whose KEY SHARE it is) → in the first
// subtest the PATCH changes the aggregation under the paused release's
// events; in the second the release commits events while the PATCH that read
// "no usage" is still open. Both fail.
func TestRelease_AgainstAnAggregationPatch(t *testing.T) {
	t.Run("the release writes first: the PATCH waits, then refuses", func(t *testing.T) {
		f := newHeldAggFixture(t, "Held agg release first", "tokens_release_first")
		customerID := insertTestCustomer(t, f.db, f.tenantID, "ext_"+f.eventName)

		paused := newPausedAudit()
		f.svc.SetAuditLogger(paused)
		released := make(chan error, 1)
		go func() { released <- f.releaseAll(customerID) }()
		waitPaused(t, paused, released, "the release")

		patched := make(chan error, 1)
		go func() { patched <- f.patch("max") }()
		select {
		case err := <-patched:
			close(paused.release)
			<-released
			t.Fatalf("the PATCH returned (%v) while a release that wrote the meter's events was still open; it must wait for the release's meter lock", err)
		case <-time.After(500 * time.Millisecond):
		}

		close(paused.release)
		if err := <-released; err != nil {
			t.Fatalf("release: %v", err)
		}
		select {
		case err := <-patched:
			wantMeterHasUsage(t, "PATCH after the release commits", err)
		case <-time.After(10 * time.Second):
			t.Fatal("the PATCH never finished")
		}
		if got := f.aggregation(t); got != "sum" {
			t.Errorf("aggregation = %q, want sum: the released events were written under it", got)
		}
		if n := f.events(t); n != 2 {
			t.Errorf("usage events = %d, want 2", n)
		}
	})

	t.Run("the PATCH reads first: the release waits, then its events are counted under the new aggregation", func(t *testing.T) {
		f := newHeldAggFixture(t, "Held agg patch first", "tokens_patch_first")
		customerID := insertTestCustomer(t, f.db, f.tenantID, "ext_"+f.eventName)

		paused := newPausedAudit()
		f.psvc.SetAuditLogger(paused)
		patched := make(chan error, 1)
		go func() { patched <- f.patch("max") }()
		waitPaused(t, paused, patched, "the PATCH")

		released := make(chan error, 1)
		go func() { released <- f.releaseAll(customerID) }()
		select {
		case err := <-released:
			close(paused.release)
			<-patched
			t.Fatalf("the release returned (%v) while an aggregation PATCH that read no usage was still open; it must wait for the PATCH's meter lock", err)
		case <-time.After(500 * time.Millisecond):
		}
		// While the PATCH is open, the meter's old aggregation is the
		// committed one; no event may be visible under it.
		if n := f.events(t); n != 0 {
			close(paused.release)
			<-patched
			<-released
			t.Fatalf("usage events visible while the PATCH is open = %d, want 0", n)
		}

		close(paused.release)
		if err := <-patched; err != nil {
			t.Fatalf("PATCH: %v", err)
		}
		select {
		case err := <-released:
			if err != nil {
				t.Fatalf("release after the PATCH: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the release never finished")
		}
		if got := f.aggregation(t); got != "max" {
			t.Errorf("aggregation = %q, want max", got)
		}
		if n := f.events(t); n != 2 {
			t.Errorf("usage events = %d, want 2", n)
		}
		f.psvc.SetAuditLogger(nil)
		wantMeterHasUsage(t, "PATCH after the release", f.patch("sum"))
	})
}

// racingPeriods is the concurrent resolver for a release's counts. It
// answers one open period whose paid threshold invoice billed through after
// the held rows. Its meter read, the first time it runs, starts an
// aggregation PATCH and gives it up to 500 ms to commit before it answers
// with what it read.
type racingPeriods struct {
	f         heldAggFixture
	period    usage.OpenPeriod
	to        string
	once      sync.Once
	patchErr  error
	patchDone chan struct{}
}

func (r *racingPeriods) OpenPeriods(context.Context, string, string) ([]usage.OpenPeriod, error) {
	return []usage.OpenPeriod{r.period}, nil
}

func (r *racingPeriods) MeterBuckets(ctx context.Context, tenantID, meterID string) (usage.MeterBuckets, error) {
	m, err := r.f.psvc.GetMeter(ctx, tenantID, meterID)
	if err != nil {
		return usage.MeterBuckets{}, err
	}
	return r.race(m), nil
}

func (r *racingPeriods) MeterBucketsTx(ctx context.Context, tx *sql.Tx, meterID string) (usage.MeterBuckets, error) {
	m, err := pricing.NewPostgresStore(r.f.db).GetMeterTx(ctx, tx, meterID)
	if err != nil {
		return usage.MeterBuckets{}, err
	}
	return r.race(m), nil
}

// race starts the PATCH the first time a meter is read and answers m's
// buckets.
func (r *racingPeriods) race(m domain.Meter) usage.MeterBuckets {
	r.once.Do(func() {
		go func() {
			r.patchErr = r.f.patch(r.to)
			close(r.patchDone)
		}()
		select {
		case <-r.patchDone:
		case <-time.After(500 * time.Millisecond):
		}
	})
	return usage.BucketsOf(m)
}

// A release's counts follow the aggregation its events are counted under,
// even when an aggregation PATCH races the release of the meter's first
// events. The rows are before a paid threshold invoice's end: under sum the
// close does not count them (outside_open_period), under max it does
// (in_open_period).
//
// Mutation-verify: read the meter's buckets before the release's write
// again → the PATCH commits between that read and the write, and the counts
// follow the old aggregation. Drop the usage_events → meters foreign key →
// the PATCH no longer waits for the release, and the same.
func TestRelease_CountsFollowTheAggregationItsEventsAreCountedUnder(t *testing.T) {
	for _, tc := range []struct{ from, to string }{{"sum", "max"}, {"max", "sum"}} {
		t.Run(tc.from+" to "+tc.to, func(t *testing.T) {
			f := newHeldAggFixture(t, "Held agg counts "+tc.from, "tokens_counts_"+tc.from)
			if tc.from != "sum" {
				if err := f.patch(tc.from); err != nil {
					t.Fatalf("PATCH to %s before any event: %v", tc.from, err)
				}
			}
			customerID := insertTestCustomer(t, f.db, f.tenantID, "ext_"+f.eventName)

			now := time.Now().UTC()
			at := func(d time.Duration) *time.Time { v := now.Add(d); return &v }
			meters := map[string]bool{f.meter.ID: true}
			r := &racingPeriods{f: f, to: tc.to, patchDone: make(chan struct{}), period: usage.OpenPeriod{
				Start: now.Add(-2 * time.Hour), End: now.Add(24 * time.Hour), Floor: at(-3 * time.Hour),
				Meters: meters, NextMeters: meters,
				ThresholdWatermark: usage.ThresholdWatermark{BilledThrough: at(-30 * time.Minute), PaidThrough: at(-30 * time.Minute)},
			}}
			f.svc.SetOpenPeriodReader(r)

			res, err := f.svc.Release(f.ctx, f.tenantID, usage.ReleaseInput{
				ExternalCustomerID: "ext_" + f.eventName,
				CustomerID:         customerID,
				MeterIDs:           map[string]string{f.eventName: f.meter.ID},
			})
			if err != nil {
				t.Fatalf("release: %v", err)
			}
			select {
			case <-r.patchDone:
			case <-time.After(10 * time.Second):
				t.Fatal("the PATCH never finished")
			}

			agg := f.aggregation(t)
			want := usage.ReleaseResult{CustomerID: customerID, Released: 2, OutsideOpenPeriod: 2}
			if !usage.BucketsOf(domain.Meter{Aggregation: agg}).Additive {
				want = usage.ReleaseResult{CustomerID: customerID, Released: 2, InOpenPeriod: 2}
			}
			if res != want {
				t.Errorf("release = %+v, want %+v: the events are counted under %s", res, want, agg)
			}
			wantMeterHasUsage(t, "the PATCH that ran into the release", r.patchErr)
			if agg != tc.from {
				t.Errorf("aggregation = %q, want %s: the released events were written under it", agg, tc.from)
			}
		})
	}
}
