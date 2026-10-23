package usage

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// Usage that arrives after its period was invoiced (ADR-120 amendment
// 2026-10-22). Velox stores such an event and answers 2xx, and no invoice
// bills it, now or later. The caller and the operator are told: the event
// answer carries billing_status "after_close", the batch and LiteLLM answers
// count after_close rows, and velox_usage_after_close_total counts them.
//
// The rule is one function, classify, shared by ingest and the held-usage
// release. Ingest reads the open periods BEFORE it writes the event, and
// that order is what keeps the flag true. Every writer of a period start
// (the cycle close, BillDueNow, a cross-interval swap, a test-clock advance)
// and the threshold fire's watermark invoice commit in
// the transaction that froze that window, after reading its usage. So when
// the read shows the window frozen, it was frozen before this event existed,
// and nothing counted it. A close that commits between the read and the
// write is not seen: that event goes unflagged, never wrongly flagged.
//
// A threshold invoice that is not paid can still be voided, and then the
// period's close bills usage from the period start again (#243). A write-off
// does not reopen the window (ADR-066 amendment 2026-10-23), but a
// written-off invoice can still be voided. So only a paid threshold
// invoice's watermark flags: a paid invoice can be neither voided nor
// written off. An event before the watermark of a threshold invoice that is
// open or written off is Unsettled and not flagged.

// BillingStatusAfterClose is UsageEvent.BillingStatus for an event that
// arrived after its period was invoiced.
const BillingStatusAfterClose = "after_close"

// Class is where one usage row falls against a customer's open periods.
// Higher wins when subscriptions disagree.
type Class int

const (
	// Outside: no subscription bills it and none billed its time. Before a
	// subscription started, in a trial, or after a final period.
	Outside Class = iota
	// AfterClose: a subscription that prices the meter already invoiced the
	// row's time, and no future invoice counts it.
	AfterClose
	// NotOnPlan: the time falls in a subscription's open period, but none of
	// its plans prices the meter.
	NotOnPlan
	// Unsettled: a threshold invoice that is not paid billed the row's time.
	// If that invoice is paid or written off, no other invoice counts the
	// row. If it is voided before the period closes, the period's invoice
	// counts it.
	Unsettled
	// InOpen: an open period's invoice counts it.
	InOpen
)

// OpenPeriod is what classify needs from one active or trialing
// subscription. Build it with OpenPeriodOf.
type OpenPeriod struct {
	// Start and End are the current billing period [Start, End).
	Start, End time.Time
	// Floor is where the subscription's billing of usage began: the trial
	// end when it had a trial (trial usage is free), else its start. Nil
	// means unknown, and nothing before Start is called AfterClose.
	Floor *time.Time
	// Meters are the meters the current plans price.
	Meters map[string]bool
	// NextMeters are the meters priced from End on: the plans after any
	// change pending at or before End. Nil when the subscription ends at End
	// (cancel at period end, or cancel_at at or before End).
	NextMeters map[string]bool
	// BilledThrough and PaidThrough are the subscription's threshold
	// watermarks (ThresholdWatermark).
	ThresholdWatermark
}

// ThresholdWatermark is how far the threshold fires of a subscription's
// current period billed its sum and count buckets. Read
// from the period's threshold invoices for every subscription, whether or
// not it still has thresholds: the close keys the watermark on the
// invoices, not on the setting.
type ThresholdWatermark struct {
	// BilledThrough is the close's watermark: the latest end of a threshold
	// invoice that is not voided (a written-off one counts). The close bills
	// sum and count buckets from it. Nil when none. It falls back only on a
	// void, which is final.
	BilledThrough *time.Time
	// PaidThrough is the latest end of a paid threshold invoice, at or
	// before BilledThrough. A paid invoice can be neither voided nor written
	// off, so the close never bills those buckets before it. Nil when none.
	PaidThrough *time.Time
}

// MeterBuckets is what classify needs from the meter itself.
type MeterBuckets struct {
	// Additive: the meter's aggregation is sum or count. Only then does
	// the threshold watermark leave a pre-watermark event unbilled: a max
	// or last bucket the fire deferred is billed over the whole period at
	// close (ADR-066 §4). One aggregation per meter (ADR-130 §2) makes this
	// a property of the meter, the same for every price of it.
	Additive bool
}

// OpenPeriodOf builds sub's OpenPeriod. plans holds the plans of sub's
// items and of their pending changes; a plan missing from it prices
// nothing. ok is false when sub is not active or trialing or has no current
// period.
func OpenPeriodOf(sub domain.Subscription, plans map[string]domain.Plan, wm ThresholdWatermark) (OpenPeriod, bool) {
	if sub.Status != domain.SubscriptionActive && sub.Status != domain.SubscriptionTrialing {
		return OpenPeriod{}, false
	}
	if sub.CurrentBillingPeriodStart == nil || sub.CurrentBillingPeriodEnd == nil {
		return OpenPeriod{}, false
	}
	p := OpenPeriod{
		Start:              *sub.CurrentBillingPeriodStart,
		End:                *sub.CurrentBillingPeriodEnd,
		Meters:             map[string]bool{},
		ThresholdWatermark: wm,
	}
	switch {
	case sub.TrialEndAt != nil:
		p.Floor = sub.TrialEndAt
	case sub.StartedAt != nil:
		p.Floor = sub.StartedAt
	}
	continues := !sub.CancelAtPeriodEnd && (sub.CancelAt == nil || sub.CancelAt.After(p.End))
	if continues {
		p.NextMeters = map[string]bool{}
	}
	for _, it := range sub.Items {
		for _, id := range plans[it.PlanID].MeterIDs {
			p.Meters[id] = true
		}
		if !continues {
			continue
		}
		next := it.PlanID
		if it.PendingPlanID != "" && it.PendingPlanEffectiveAt != nil && !it.PendingPlanEffectiveAt.After(p.End) {
			next = it.PendingPlanID
		}
		for _, id := range plans[next].MeterIDs {
			p.NextMeters[id] = true
		}
	}
	return p, true
}

// BucketsOf builds a meter's MeterBuckets. An empty aggregation reads as
// sum, as billing reads it.
func BucketsOf(meter domain.Meter) MeterBuckets {
	return MeterBuckets{Additive: meter.Aggregation == "" || domain.AdditiveAggregation(meter.Aggregation)}
}

// classify places one row of meter meterID at time at against periods. The
// meter is read only when the answer depends on it, through meter.
//
// For each period:
//   - at >= End (the grace before End's invoice, or a close running late):
//     the next period counts it if the subscription continues and prices the
//     meter from End on. Never AfterClose: the next close bills it.
//   - Start <= at < End: InOpen if a current plan prices the meter, else
//     NotOnPlan. When the meter is additive and at is past the floor:
//     before PaidThrough it is AfterClose, and before BilledThrough (a
//     threshold invoice not paid yet) it is Unsettled.
//   - at < Start: AfterClose when a current plan prices the meter and at is
//     at or past the floor. Else Outside.
//
// The highest class over all periods wins, so another subscription whose
// open period holds at keeps the row out of AfterClose.
func classify(at time.Time, meterID string, periods []OpenPeriod, meter func() (MeterBuckets, error)) (Class, error) {
	best := Outside
	var mb *MeterBuckets
	buckets := func() (MeterBuckets, error) {
		if mb == nil {
			b, err := meter()
			if err != nil {
				return MeterBuckets{}, err
			}
			mb = &b
		}
		return *mb, nil
	}
	for _, p := range periods {
		c := Outside
		pastFloor := p.Floor != nil && !at.Before(*p.Floor)
		switch {
		case !at.Before(p.End):
			if p.NextMeters != nil {
				c = NotOnPlan
				if p.NextMeters[meterID] {
					c = InOpen
				}
			}
		case !at.Before(p.Start):
			c = NotOnPlan
			if p.Meters[meterID] {
				c = InOpen
				if p.BilledThrough != nil && at.Before(*p.BilledThrough) && pastFloor {
					b, err := buckets()
					if err != nil {
						return Outside, err
					}
					if b.Additive {
						c = Unsettled
						if p.PaidThrough != nil && at.Before(*p.PaidThrough) {
							c = AfterClose
						}
					}
				}
			}
		case p.Meters[meterID] && pastFloor:
			c = AfterClose
		}
		if c > best {
			best = c
		}
	}
	return best, nil
}

// OpenPeriodReader reads what ingest and the held-usage release classify
// against. Implemented in internal/api from the subscription store, the
// pricing service and the invoice store (the threshold watermarks).
type OpenPeriodReader interface {
	// OpenPeriods returns the OpenPeriod of every active and trialing
	// subscription of the customer.
	OpenPeriods(ctx context.Context, tenantID, customerID string) ([]OpenPeriod, error)
	// MeterBuckets returns the meter's MeterBuckets.
	MeterBuckets(ctx context.Context, tenantID, meterID string) (MeterBuckets, error)
	// MeterBucketsTx returns the meter's MeterBuckets, read on tx (which
	// carries the tenant). A write that reads inside its open transaction
	// uses it, so it never waits for a second connection.
	MeterBucketsTx(ctx context.Context, tx *sql.Tx, meterID string) (MeterBuckets, error)
}

// periodCache holds one request's reads, so a batch reads each customer's
// periods and each meter's buckets once. Every read still happens before
// the writes it classifies. Not for concurrent use.
type periodCache struct {
	periods map[string][]OpenPeriod
	meters  map[string]MeterBuckets
}

type periodCacheKey struct{}

// WithPeriodCache returns ctx carrying a cache for the open-period reads of
// the ingest calls made with it. A door that ingests many rows one call at a
// time (the LiteLLM callback) uses it for its request.
func WithPeriodCache(ctx context.Context) context.Context {
	return context.WithValue(ctx, periodCacheKey{}, &periodCache{periods: map[string][]OpenPeriod{}, meters: map[string]MeterBuckets{}})
}

func periodCacheFrom(ctx context.Context) *periodCache {
	if c, ok := ctx.Value(periodCacheKey{}).(*periodCache); ok {
		return c
	}
	return &periodCache{periods: map[string][]OpenPeriod{}, meters: map[string]MeterBuckets{}}
}

// classifyEvent classifies one prepared row against the customer's open
// periods. With no reader wired it is Outside. A failed read is returned
// wrapped in errIngestUnavailable: the doors answer 503 and nothing is
// written, the same contract as a failed customer or meter lookup.
func (s *Service) classifyEvent(ctx context.Context, tenantID string, ev domain.UsageEvent, cache *periodCache) (Class, error) {
	if s.periods == nil {
		return Outside, nil
	}
	periods, ok := cache.periods[ev.CustomerID]
	if !ok {
		var err error
		periods, err = s.periods.OpenPeriods(ctx, tenantID, ev.CustomerID)
		if err != nil {
			return Outside, fmt.Errorf("%w: open periods: %w", errIngestUnavailable, err)
		}
		cache.periods[ev.CustomerID] = periods
	}
	return classify(ev.Timestamp, ev.MeterID, periods, func() (MeterBuckets, error) {
		if b, ok := cache.meters[ev.MeterID]; ok {
			return b, nil
		}
		b, err := s.periods.MeterBuckets(ctx, tenantID, ev.MeterID)
		if err != nil {
			return MeterBuckets{}, fmt.Errorf("%w: meter buckets: %w", errIngestUnavailable, err)
		}
		cache.meters[ev.MeterID] = b
		return b, nil
	})
}

var afterCloseEvents *prometheus.CounterVec

func init() {
	c := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "velox_usage_after_close_total",
		Help: "Usage events written after their period was invoiced: stored, billed on no invoice (ADR-120). Counted once per written row; a replay is not counted.",
	}, []string{"origin"})
	if err := prometheus.DefaultRegisterer.Register(c); err != nil {
		if are, ok := err.(prometheus.AlreadyRegisteredError); ok {
			afterCloseEvents = are.ExistingCollector.(*prometheus.CounterVec)
		} else {
			panic(err)
		}
	} else {
		afterCloseEvents = c
	}
}

// recordAfterClose counts and logs one written after-close row. Called only
// after the write committed, for rows the write inserted.
func recordAfterClose(ctx context.Context, ev domain.UsageEvent) {
	afterCloseEvents.WithLabelValues(string(ev.Origin)).Inc()
	slog.WarnContext(ctx, "usage event arrived after its period was invoiced — stored, billed on no invoice",
		"customer_id", ev.CustomerID,
		"meter_id", ev.MeterID,
		"timestamp", ev.Timestamp,
		"origin", string(ev.Origin),
	)
}
