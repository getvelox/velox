package usage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	mw "github.com/sagarsuperuser/velox/internal/api/middleware"
	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"

	"log/slog"

	"github.com/prometheus/client_golang/prometheus"
)

type Service struct {
	store    Store
	resolver clock.Resolver
	audit    AuditEmitter
}

func NewService(store Store) *Service {
	return &Service{store: store}
}

// SetAuditLogger wires in-tx audit emission for BACKFILL. Live ingest stays
// unaudited by design (machine metering; usage_events is the record) — but an
// operator inserting BACKDATED usage is changing what a customer gets billed
// for a period that may already have closed, so that action is recorded.
func (s *Service) SetAuditLogger(a AuditEmitter) { s.audit = a }

// SetResolver wires the unified clock.Resolver (implemented by
// *billing.Engine). Ingest timestamps default to and gate against the
// CUSTOMER's effective now — a test clock's frozen_time when the
// customer is pinned — so usage ingestion works in simulated time: on
// a clock advanced into the (wall-clock) future, events without a
// timestamp land at frozen_time inside the simulated current period,
// and sim-timestamped events pass the future-skew gate that wall-clock
// comparison would wrongly reject. Optional: nil keeps wall-clock
// gating (narrow tests).
func (s *Service) SetResolver(r clock.Resolver) {
	s.resolver = r
}

// simNow returns the customer's simulated-time context: the ctx-bound one
// if an upstream entry point already bound it, else the customer's own pin
// (frozen_time + clock id when pinned to a test clock), else a bare
// wall-clock Sim.
//
// Live mode skips the resolver lookup entirely — test clocks are
// test-mode-only (DB CHECK on test_clocks.livemode), so a live
// customer can never be pinned and the high-volume production ingest
// path stays at zero extra queries.
func (s *Service) simNow(ctx context.Context, tenantID, customerID string) clock.Sim {
	if sim, ok := clock.SimOf(ctx); ok {
		return sim
	}
	if t, ok := clock.EffectiveNow(ctx); ok {
		// Bound instant whose clock is unknown — keep the instant, claim no clock.
		return clock.Sim{At: t}
	}
	if s.resolver != nil && !postgres.Livemode(ctx) && customerID != "" {
		if sim, err := s.resolver.SimForCustomer(ctx, tenantID, customerID); err == nil {
			return sim
		}
		// Resolver errors mean the customer read failed — ingest's own
		// store call will surface that; don't block on the clock here.
	}
	return clock.Sim{At: time.Now().UTC()}
}

// effectiveNow is simNow's instant — the value the ingest gates and
// timestamps use.
func (s *Service) effectiveNow(ctx context.Context, tenantID, customerID string) time.Time {
	return s.simNow(ctx, tenantID, customerID).At
}

// IngestInput is the internal service input — uses resolved internal IDs only.
// The handler is responsible for resolving external identifiers before calling this.
type IngestInput struct {
	CustomerID     string          `json:"customer_id"`
	MeterID        string          `json:"meter_id"`
	Quantity       decimal.Decimal `json:"quantity,omitempty"`
	Dimensions     map[string]any  `json:"dimensions,omitempty"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
	Timestamp      *time.Time      `json:"timestamp,omitempty"`
	// ObservedCostMicros is the provider's OWN reported cost for this one
	// event, in micro-dollars — the ADR-079 D4 fast-follow. When set it wins
	// over rate-table inference and stamps provider_cost_source='observed';
	// when nil the table path is unchanged.
	//
	// The caller must have already applied D4's rule, because this layer
	// cannot re-derive it: only a PER-HALF figure may arrive here
	// (input_cost onto the input event, output_cost onto the output event).
	// A whole-call figure must never reach this field — it would be stamped
	// onto each half and multi-count COGS, which is exactly why phase 1
	// stamped nothing at all.
	ObservedCostMicros *int64 `json:"observed_cost_micros,omitempty"`
}

func (s *Service) Ingest(ctx context.Context, tenantID string, input IngestInput) (domain.UsageEvent, error) {
	return s.ingest(ctx, tenantID, input, domain.UsageOriginAPI)
}

// Backfill ingests a historical usage event. Requires a non-nil timestamp
// strictly in the past; rejects missing / future / equal-to-now values so
// operators can't accidentally double-post a live event through the audit
// path. The row is tagged origin='backfill'.
//
// Billing semantics: backfilled events participate in aggregation for any
// period whose [start, end) contains the event's timestamp. Finalized
// invoices are immutable (they reference billed_entries, not live
// aggregations), so backfill into closed periods is safe — it changes the
// audit ledger without rewriting history.
func (s *Service) Backfill(ctx context.Context, tenantID string, input IngestInput) (domain.UsageEvent, error) {
	if input.Timestamp == nil {
		return domain.UsageEvent{}, errs.Required("timestamp")
	}
	// Reject future timestamps — relative to the CUSTOMER's effective now
	// (frozen_time when pinned to a test clock), so backfilling simulated
	// history onto an advanced clock works: with the clock frozen in July,
	// June events are valid backfill even when wall-clock says May.
	// Equality-with-now is allowed because the test-vs-prod clock race
	// makes strict past-only brittle to verify, and the 'backfill' origin
	// tag already distinguishes these rows from live POST traffic for
	// audit purposes.
	sim := s.simNow(ctx, tenantID, input.CustomerID)
	if input.Timestamp.After(sim.At) {
		return domain.UsageEvent{}, errs.Invalid("timestamp", "must not be in the future for backfill — use POST /usage-events for real-time ingest")
	}
	// Carry the resolved sim context downstream so ingest doesn't re-resolve —
	// and so the audit emission inherits the CLOCK, not just the instant. The
	// emission closure must be built from the BOUND ctx (it was previously
	// built from the outer one, which is why an operator backdating usage into
	// a simulation left an audit row with no sim axis).
	bound := clock.WithSim(ctx, sim)
	return s.ingestAudited(bound, tenantID, input, domain.UsageOriginBackfill, s.backfillEmission(bound))
}

// backfillEmission records the operator's backdated insert on the ingest tx.
// action=create + resource_type="usage_event" (frozen vocabulary); the
// metadata carries what makes it billing-relevant: WHICH customer/meter, the
// quantity, and the BACKDATED timestamp (the whole point — it may land in a
// period that has already been invoiced).
func (s *Service) backfillEmission(ctx context.Context) func(tx *sql.Tx, out domain.UsageEvent) error {
	if s.audit == nil {
		return nil
	}
	return func(tx *sql.Tx, out domain.UsageEvent) error {
		return s.audit.LogInTx(ctx, tx, audit.Entry{
			Action:       domain.AuditActionCreate,
			ResourceType: "usage_event",
			ResourceID:   out.ID,
			Metadata: map[string]any{
				"action":      "usage_backfilled",
				"customer_id": out.CustomerID,
				"meter_id":    out.MeterID,
				"quantity":    out.Quantity.String(),
				"event_at":    out.Timestamp.UTC().Format(time.RFC3339),
				"origin":      string(out.Origin),
			},
		})
	}
}

// MaxDimensionKeys caps the size of the JSONB dimensions map on each
// usage event. Dimensions feed pricing-rule dispatch via @> subset
// matches at finalize time; bounding the per-event JSONB size protects
// the GIN index from pathological tenants and matches the equivalent
// cap on meter_pricing_rules.dimension_match (16 keys).
const MaxDimensionKeys = 16

// usageFutureSkew is how far ahead of wall-clock a live usage event timestamp
// may be before it's rejected — absorbs minor client clock drift without
// letting events be parked in a future billing period.
const usageFutureSkew = 5 * time.Minute

// maxQuantityMagnitude is the exclusive upper bound on |quantity|. The column
// is NUMERIC(38,12) → 26 integer digits, so values ≥ 10^26 overflow it.
var maxQuantityMagnitude = decimal.New(1, 26) // 10^26

// validateDimensions enforces the v1 dimension contract: at most
// MaxDimensionKeys keys, scalar values only (string, number, bool, nil).
// Object/array values are rejected — Postgres `@>` would still match
// them but the priority+claim semantics aren't well-defined for nested
// containers in v1 (revisit if a design partner needs it).
func validateDimensions(dims map[string]any) error {
	if len(dims) > MaxDimensionKeys {
		return errs.Invalid("dimensions", fmt.Sprintf("at most %d keys (got %d)", MaxDimensionKeys, len(dims)))
	}
	for k, v := range dims {
		switch v.(type) {
		case nil, string, bool, float64, float32, int, int32, int64:
			// Scalar — fine.
		default:
			return errs.Invalid("dimensions", fmt.Sprintf("key %q value must be a scalar (string/number/bool), got %T", k, v))
		}
	}
	return nil
}

func (s *Service) ingest(ctx context.Context, tenantID string, input IngestInput, origin domain.UsageEventOrigin) (domain.UsageEvent, error) {
	return s.ingestAudited(ctx, tenantID, input, origin, nil)
}

// ingestAudited is ingest with an optional in-tx audit emission. Only backfill
// supplies one; every live path passes nil (see PostgresStore.IngestAudited).
func (s *Service) ingestAudited(ctx context.Context, tenantID string, input IngestInput, origin domain.UsageEventOrigin, emit func(tx *sql.Tx, out domain.UsageEvent) error) (domain.UsageEvent, error) {
	prepared, err := s.prepare(ctx, tenantID, input, origin)
	if err != nil {
		return domain.UsageEvent{}, err
	}
	event, err := s.store.IngestAudited(ctx, tenantID, prepared, emit)
	if err == nil {
		// Count only rows that actually landed. An idempotency-replay errors
		// out of store.Ingest (no new row), so it isn't counted — the metric
		// is true ingest throughput, not request volume. Every path funnels
		// here (live POST, batch, backfill, LiteLLM).
		mw.RecordUsageIngested(1)
	}
	return event, err
}

// prepare runs the full per-event validation + timestamp resolution and
// composes the domain row, WITHOUT writing it. Split from ingest so
// BatchIngest can validate every event up front and then hand the whole
// slice to one store transaction (all-or-nothing).
// lateUsageEvents surfaces the "silent half" of late ingestion (2026-07-10
// design review): a live event stamped >24h in the past MAY fall inside an
// already-finalized period, where it is stored but never billed (see the
// KNOWN BEHAVIOR note below). Classifying precisely needs a per-event
// subscription lookup this hot path deliberately avoids, and the true-up
// policy itself is a deferred DP decision — but the OPERATOR VISIBILITY is
// not deferred: this counter + a WARN make the late stream observable.
// Backfill-origin events are excluded (documented-safe intentional path).
var lateUsageEvents *prometheus.CounterVec

func init() {
	c := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "velox_usage_late_event_total",
		Help: "Live-origin usage events ingested with a timestamp >24h in the past — may fall in an already-finalized (unbillable) period.",
	}, []string{"origin"})
	if err := prometheus.DefaultRegisterer.Register(c); err != nil {
		if are, ok := err.(prometheus.AlreadyRegisteredError); ok {
			lateUsageEvents = are.ExistingCollector.(*prometheus.CounterVec)
		} else {
			panic(err)
		}
	} else {
		lateUsageEvents = c
	}
}

func (s *Service) prepare(ctx context.Context, tenantID string, input IngestInput, origin domain.UsageEventOrigin) (domain.UsageEvent, error) {
	if strings.TrimSpace(input.CustomerID) == "" {
		return domain.UsageEvent{}, errs.Required("customer_id")
	}
	if strings.TrimSpace(input.MeterID) == "" {
		return domain.UsageEvent{}, errs.Required("meter_id")
	}
	if err := validateDimensions(input.Dimensions); err != nil {
		return domain.UsageEvent{}, err
	}
	// quantity == 0 (absent or explicit) stays 0 — PRESENCE semantics.
	// The old code silently coerced it to 1: an integrator emitting
	// zero-usage heartbeats ("request happened, zero tokens billed") was
	// billed one unit per event on sum-aggregated meters — a money bug
	// invisible until the invoice. Count meters count the event row
	// regardless of quantity, so they are unaffected; sum/max meters now
	// honestly contribute nothing. (Stripe parity: meter events carry an
	// explicit value; there is no silent default.) Negative values remain
	// allowed as usage corrections.
	// The quantity column is NUMERIC(38,12): at most 26 integer digits. A value
	// with |q| ≥ 10^26 overflows the column and Postgres rejects the INSERT,
	// surfacing as an HTTP 500. Reject it here as a clean 422 instead. (Excess
	// fractional precision beyond 12 places is rounded by the column, not an
	// error, so only the integer magnitude is gated.)
	if input.Quantity.Abs().Cmp(maxQuantityMagnitude) >= 0 {
		return domain.UsageEvent{}, errs.Invalid("quantity", "magnitude too large — must be less than 10^26")
	}

	// "Now" is the CUSTOMER's effective now — a test clock's frozen_time
	// when the customer is pinned (Stripe Test Clocks shape: meter events
	// on a pinned customer live on the simulated timeline). Two effects:
	// events without a timestamp land at frozen_time (inside the simulated
	// current billing period, where the next Advance bills them), and the
	// future gate below compares against simulated now — pre-fix it
	// compared against wall-clock, so on a clock advanced into the future
	// every sim-timestamped event was rejected as "in the future" and the
	// flagship advance-and-bill-usage demo broke at ingest.
	now := s.effectiveNow(ctx, tenantID, input.CustomerID)
	ts := now
	if input.Timestamp != nil {
		// Reject future-dated live events (a small clock-skew tolerance keeps
		// legitimate near-now client clocks working). Without this, only
		// Backfill rejected future timestamps and a live POST could park usage
		// in a future billing period. Backfill applies its own stricter
		// past-only gate before reaching here.
		if input.Timestamp.After(now.Add(usageFutureSkew)) {
			return domain.UsageEvent{}, errs.Invalid("timestamp", "must not be in the future")
		}
		ts = input.Timestamp.UTC()
	}

	// KNOWN BEHAVIOR (no lower bound on live timestamps): an event whose
	// timestamp falls inside an ALREADY-FINALIZED billing period is
	// accepted and stored, but the cycle that closed that period won't be
	// re-billed (finalized invoices reference billed entries, not live
	// aggregations) — so it is effectively unbilled. This is deliberate:
	// late-arriving events are industry-standard (Stripe Meter Events,
	// Lago, Orb all accept out-of-order events), so a hard reject would
	// break legitimate retries / stream pipelines. Intentional historical
	// posting into closed periods goes through Backfill (origin='backfill',
	// documented safe). Deferred decision (no design partner yet): whether
	// to true-up closed periods or reject past a window — both need a
	// billing-policy call, not a silent per-event subscription lookup on
	// this hot path. The late-event COUNTER half shipped 2026-07-10 (see
	// lateUsageEvents above): >24h-late live events are counted + WARNed
	// so the stream is observable while the policy stays deferred.
	if origin != domain.UsageOriginBackfill && ts.Before(now.Add(-24*time.Hour)) {
		lateUsageEvents.WithLabelValues(string(origin)).Inc()
		slog.WarnContext(ctx, "late usage event (>24h past) — may fall in an already-finalized period and go unbilled",
			"customer_id", input.CustomerID,
			"meter_id", input.MeterID,
			"timestamp", ts,
			"origin", string(origin),
		)
	}
	return domain.UsageEvent{
		CustomerID:     input.CustomerID,
		MeterID:        input.MeterID,
		Quantity:       input.Quantity,
		Dimensions:     input.Dimensions,
		IdempotencyKey: input.IdempotencyKey,
		Timestamp:      ts,
		Origin:         origin,
		// Carries the observed cost INTO the INSERT, where it either wins
		// over the rate table or is replaced by the table's figure on
		// RETURNING. Nil leaves the phase-1 behaviour untouched.
		ProviderCostMicros: input.ObservedCostMicros,
	}, nil
}

// BatchIngest validates every event, then writes them ALL in one store
// transaction — all-or-nothing. Pre-fix each event committed in its own
// tx: a mid-batch abort (client timeout, dropped connection) left a
// committed prefix, and the standard retry-the-batch response re-ingested
// that prefix — double-billed usage for every event without an
// idempotency key. Now either the whole batch lands or none of it does,
// so a keyless retry is always a clean first write.
//
// Returns (inserted, deduped, errs). Validation failures are collected
// for EVERY failing index (a bare "quantity too large" across a 500-event
// batch was undebuggable) and abort before any write. Idempotency replays
// count as deduped — success, not failure — matching the LiteLLM door.
func (s *Service) BatchIngest(ctx context.Context, tenantID string, events []IngestInput) (int, int, []error) {
	prepared := make([]domain.UsageEvent, 0, len(events))
	var batchErrs []error
	for i, input := range events {
		ev, err := s.prepare(ctx, tenantID, input, domain.UsageOriginAPI)
		if err != nil {
			batchErrs = append(batchErrs, fmt.Errorf("event[%d]: %w", i, err))
			continue
		}
		prepared = append(prepared, ev)
	}
	if len(batchErrs) > 0 {
		return 0, 0, batchErrs
	}

	inserted, deduped, err := s.store.IngestBatch(ctx, tenantID, prepared)
	if err != nil {
		return 0, 0, []error{err}
	}
	if inserted > 0 {
		mw.RecordUsageIngested(inserted)
	}
	return inserted, deduped, nil
}

// GetByIdempotencyKey fetches the event a replayed key originally wrote —
// backs the public door's replay-as-success response (200 + original row
// + Idempotent-Replayed header instead of a bare 409).
func (s *Service) GetByIdempotencyKey(ctx context.Context, tenantID, key string) (domain.UsageEvent, error) {
	return s.store.GetByIdempotencyKey(ctx, tenantID, key)
}

func (s *Service) List(ctx context.Context, filter ListFilter) ([]domain.UsageEvent, int, error) {
	return s.store.List(ctx, filter)
}

// Aggregate returns server-side totals + per-meter breakdown matching the
// filter. Used by GET /v1/usage-events/aggregate so the /usage dashboard's
// stat cards reflect filtered totals rather than a reduce over the current
// page of paginated events. Limit/Offset on the filter are intentionally
// ignored — the whole point is the unbounded total.
func (s *Service) Aggregate(ctx context.Context, filter ListFilter) (Aggregate, error) {
	return s.store.Aggregate(ctx, filter)
}

func (s *Service) AggregateForBillingPeriod(ctx context.Context, tenantID, customerID string, meterIDs []string, from, to time.Time) (map[string]decimal.Decimal, error) {
	return s.store.AggregateForBillingPeriod(ctx, tenantID, customerID, meterIDs, from, to)
}

// AggregateDailyBuckets delegates to the store. Exposed on the Service
// so CustomerUsageService can fetch chart data without holding the
// Store directly. See Store interface for contract.
func (s *Service) AggregateDailyBuckets(ctx context.Context, tenantID, customerID string, meterIDs []string, from, to time.Time) ([]DailyBucketRow, error) {
	return s.store.AggregateDailyBuckets(ctx, tenantID, customerID, meterIDs, from, to)
}

// AggregateByPricingRules resolves a single (customer, meter, period) into
// per-rule aggregations using the priority+claim algorithm. The defaultMode
// applies to events that match no rule; it must be one of the four
// period-bounded modes (sum, count, max, last_during_period) — last_ever
// as a meter-default is rejected because it would silently break the
// "current state" semantics for unclaimed events.
//
// See docs/design-multi-dim-meters.md for the resolution semantics; this
// method is the runtime entry point that billing-finalize will call.
func (s *Service) AggregateByPricingRules(
	ctx context.Context,
	tenantID, customerID, meterID string,
	defaultMode domain.AggregationMode,
	from, to time.Time,
) ([]domain.RuleAggregation, error) {
	if defaultMode == "" {
		defaultMode = domain.AggSum
	}
	switch defaultMode {
	case domain.AggSum, domain.AggCount, domain.AggMax, domain.AggLastDuringPeriod:
		// ok
	default:
		return nil, errs.Invalid("default_mode", fmt.Sprintf("must be one of sum/count/max/last_during_period, got %q", defaultMode))
	}
	return s.store.AggregateByPricingRules(ctx, tenantID, customerID, meterID, defaultMode, from, to)
}
