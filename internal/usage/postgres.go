package usage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

type PostgresStore struct {
	db *postgres.DB
}

func NewPostgresStore(db *postgres.DB) *PostgresStore {
	return &PostgresStore{db: db}
}

func (s *PostgresStore) Ingest(ctx context.Context, tenantID string, event domain.UsageEvent) (domain.UsageEvent, error) {
	return s.IngestAudited(ctx, tenantID, event, nil)
}

// IngestAudited is Ingest with an in-tx audit emission hook (ADR-090).
//
// Live ingest (POST /v1/usage-events, /batch, the LiteLLM spend callback)
// passes nil: it is machine metering at up to ~1000/s, usage_events IS the
// record, and a row per event would double the write volume of the hottest
// path for no forensic gain. BACKFILL is the exception and passes an emitter:
// an operator inserting BACKDATED usage is changing what a customer will be
// billed for a period that may already have closed — an operator action on the
// money path, not machine telemetry. Sharing the ingest tx means a backfilled
// event that cannot be recorded is not ingested at all.
func (s *PostgresStore) IngestAudited(ctx context.Context, tenantID string, event domain.UsageEvent, emit func(tx *sql.Tx, out domain.UsageEvent) error) (domain.UsageEvent, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.UsageEvent{}, err
	}
	defer postgres.Rollback(tx)

	event, err = ingestOneTx(ctx, tx, tenantID, event)
	if err != nil {
		return domain.UsageEvent{}, err
	}
	if emit != nil {
		if err := emit(tx, event); err != nil {
			return domain.UsageEvent{}, fmt.Errorf("audit emission: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return domain.UsageEvent{}, err
	}
	return event, nil
}

// IngestBatch writes every event in ONE transaction — all-or-nothing.
// Pre-fix, BatchIngest looped one tx per event: a mid-batch abort (client
// timeout, connection drop, crash) left a COMMITTED PREFIX, and the
// standard client response — retry the whole batch — re-ingested that
// prefix. Events without idempotency keys have no dedup line of defense,
// so the retry double-billed every prefix event. Atomic batches make the
// keyless retry safe: either the response arrived (don't retry) or
// nothing committed (retry is a clean first write).
//
// Keyed duplicates (replay of a fully-committed batch, or the same key
// twice within one batch) are counted in deduped, not errors — matching
// the LiteLLM door's replay contract.
func (s *PostgresStore) IngestBatch(ctx context.Context, tenantID string, events []domain.UsageEvent) (inserted, deduped int, err error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return 0, 0, err
	}
	defer postgres.Rollback(tx)

	for i, event := range events {
		_, err := ingestOneTx(ctx, tx, tenantID, event)
		switch {
		case errors.Is(err, errs.ErrDuplicateKey):
			deduped++
		case err != nil:
			return 0, 0, fmt.Errorf("event[%d]: %w", i, err)
		default:
			inserted++
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return inserted, deduped, nil
}

// GetByIdempotencyKey fetches the event a replayed key originally wrote.
// Backs replay-as-success on the public ingest door: instead of a bare
// 409, the handler returns the original row (Stripe idempotency shape).
// Livemode scoping rides the RLS session like every other reader.
func (s *PostgresStore) GetByIdempotencyKey(ctx context.Context, tenantID, key string) (domain.UsageEvent, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.UsageEvent{}, err
	}
	defer postgres.Rollback(tx)

	var event domain.UsageEvent
	err = tx.QueryRowContext(ctx, `
		SELECT id, tenant_id, customer_id, meter_id,
			quantity, properties, COALESCE(idempotency_key,''), timestamp, origin,
			provider_cost_micros, COALESCE(provider_cost_source,'')
		FROM usage_events WHERE idempotency_key = $1`, key,
	).Scan(&event.ID, &event.TenantID, &event.CustomerID, &event.MeterID,
		&event.Quantity, propertiesScanner{&event.Dimensions},
		&event.IdempotencyKey, &event.Timestamp, &event.Origin,
		&event.ProviderCostMicros, &event.ProviderCostSource)
	if err == sql.ErrNoRows {
		return domain.UsageEvent{}, errs.ErrNotFound
	}
	if err != nil {
		return domain.UsageEvent{}, err
	}
	return event, nil
}

// ingestOneTx is the single-source INSERT every ingest path funnels
// through (live POST, batch, backfill, LiteLLM). Runs on the caller's tx
// so Ingest keeps its own-commit shape while IngestBatch composes N
// inserts atomically. Returns errs.ErrDuplicateKey (no row written) when
// the event's idempotency key already exists — via ON CONFLICT DO NOTHING
// rather than a unique-violation error, because a violation would poison
// the surrounding batch transaction.
func ingestOneTx(ctx context.Context, tx *sql.Tx, tenantID string, event domain.UsageEvent) (domain.UsageEvent, error) {
	id := postgres.NewID("vlx_evt")

	origin := string(event.Origin)
	if origin == "" {
		origin = string(domain.UsageOriginAPI)
	}

	props, err := propertiesJSON(event.Dimensions)
	if err != nil {
		return domain.UsageEvent{}, err
	}

	// Provider-cost stamp (ADR-079): COGS attaches at INGEST — the
	// universal verified snapshot semantic; later rate edits are
	// non-retroactive by construction. Resolution runs as a scalar
	// subquery INSIDE this INSERT (every ingest path — live POST, batch,
	// backfill, LiteLLM — funnels through here, so the site-set is closed
	// by construction, and it adds zero extra round trips on the hottest
	// write path; an empty rate table costs ~nothing via the unique
	// index). Key order per ADR-079 D3: model_raw exact first (raw ids
	// price per snapshot), then the canonical model family token. Micros
	// = cost_per_token × quantity × 1e6, ROUND HALF UP. Source semantics:
	// 'table' when a rate matched; 'not_applicable' when the event has no
	// costable dims (non-token meters — keeps the honesty counter
	// meaningful); NULL when costable dims exist but no rate matched
	// ('unresolved' — the actionable signal).
	err = tx.QueryRowContext(ctx, `
		WITH rate AS (
			SELECT r.cost_per_token
			FROM provider_cost_rates r
			WHERE r.tenant_id = $2
			  AND r.provider = $6::jsonb->>'provider'
			  AND r.token_type = $6::jsonb->>'token_type'
			  AND r.model IN ($6::jsonb->>'model_raw', $6::jsonb->>'model')
			ORDER BY (r.model IS NOT DISTINCT FROM $6::jsonb->>'model_raw') DESC
			LIMIT 1
		)
		INSERT INTO usage_events (id, tenant_id, customer_id, meter_id,
			quantity, properties, idempotency_key, timestamp, origin,
			provider_cost_micros, provider_cost_source)
		SELECT $1, $2, $3, $4, $5, $6, $7, $8, $9,
			CASE
				WHEN $10::BIGINT IS NOT NULL THEN $10::BIGINT
				WHEN (SELECT COUNT(*) FROM rate) = 1
					THEN ROUND((SELECT cost_per_token FROM rate) * $5 * 1000000)::BIGINT
			END,
			CASE
				WHEN $10::BIGINT IS NOT NULL THEN 'observed'
				WHEN $6::jsonb->>'provider' IS NULL
				  OR $6::jsonb->>'token_type' IS NULL
				  OR ($6::jsonb->>'model' IS NULL AND $6::jsonb->>'model_raw' IS NULL)
					THEN 'not_applicable'
				WHEN (SELECT COUNT(*) FROM rate) = 1 THEN 'table'
			END
		ON CONFLICT (tenant_id, livemode, idempotency_key) DO NOTHING
		RETURNING id, tenant_id, customer_id, meter_id,
			quantity, properties, COALESCE(idempotency_key,''), timestamp, origin,
			provider_cost_micros, COALESCE(provider_cost_source,'')
	`, id, tenantID, event.CustomerID, event.MeterID,
		event.Quantity,
		props, postgres.NullableString(event.IdempotencyKey),
		event.Timestamp, origin,
		// $10 — the provider's own per-half cost when the caller supplied
		// one (ADR-079 D4). NULL keeps the rate-table path exactly as it
		// was; non-NULL short-circuits both CASEs above.
		event.ProviderCostMicros,
	).Scan(&event.ID, &event.TenantID, &event.CustomerID, &event.MeterID,
		&event.Quantity, propertiesScanner{&event.Dimensions},
		&event.IdempotencyKey, &event.Timestamp, &event.Origin,
		&event.ProviderCostMicros, &event.ProviderCostSource)

	// ON CONFLICT DO NOTHING returns no row on an idempotency replay —
	// surfaced as ErrDuplicateKey to keep this store's contract identical
	// to the pre-batch unique-violation shape (callers branch on it).
	if err == sql.ErrNoRows {
		return domain.UsageEvent{}, fmt.Errorf("%w: idempotency_key %q", errs.ErrDuplicateKey, event.IdempotencyKey)
	}
	if err != nil {
		return domain.UsageEvent{}, err
	}
	return event, nil
}

// List paginates usage events ordered by (timestamp DESC, id DESC).
// Returns events + total (for the legacy offset path; 0 when cursor
// path used). Supports two mutually-exclusive paging shapes:
//
//   - Cursor (preferred, 2026-05-29): filter.AfterTimestamp +
//     filter.AfterID set → seek-method query
//     `WHERE (timestamp, id) < (after_ts, after_id)`. Skips the
//     COUNT query, fetches limit+1 to detect hasMore via the next
//     handler call. Stable across concurrent inserts at the table's
//     head — usage_events is the highest-write table in Velox, so
//     offset-based pagination page-skewed reliably whenever a cycle
//     close fired between operator pages.
//   - Offset (legacy): COUNT + LIMIT/OFFSET. Kept for the dashboard
//     paths that still use offset+total for "Page 1 of N" UX.
//
// id as the tiebreaker keeps ordering deterministic when many events
// share a microsecond — common for batched ingestion.
func (s *PostgresStore) List(ctx context.Context, filter ListFilter) ([]domain.UsageEvent, int, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, filter.TenantID)
	if err != nil {
		return nil, 0, err
	}
	defer postgres.Rollback(tx)

	// Default 100, clamp to 1000 — usage list intentionally allows a
	// higher cap than other stores (usage volume is naturally higher;
	// clients page through it). No-silent-fallbacks principle.
	limit := filter.Limit
	if limit <= 0 {
		limit = 100
	} else if limit > 1000 {
		limit = 1000
	}

	where, args := buildUsageWhere(filter)
	useCursor := !filter.AfterTimestamp.IsZero() && filter.AfterID != ""

	if useCursor {
		args = append(args, filter.AfterTimestamp, filter.AfterID)
		clause := fmt.Sprintf(`(timestamp, id) < ($%d, $%d)`, len(args)-1, len(args))
		if where == "" {
			where = " WHERE " + clause
		} else {
			where += " AND " + clause
		}
	}

	// Cursor path: fetch limit+1, skip the COUNT — the handler
	// derives hasMore from the over-fetch. Offset path: COUNT first
	// so callers get a "Page X of N" total.
	var total int
	if !useCursor {
		countQuery := `SELECT COUNT(*) FROM usage_events` + where
		if err := tx.QueryRowContext(ctx, countQuery, args...).Scan(&total); err != nil {
			return nil, 0, err
		}
	}

	queryLimit := limit
	if useCursor {
		queryLimit = limit + 1
	}
	args = append(args, queryLimit)
	query := `SELECT id, tenant_id, customer_id, meter_id,
		quantity, properties, COALESCE(idempotency_key,''), timestamp,
		provider_cost_micros, COALESCE(provider_cost_source,'')
		FROM usage_events` + where + ` ORDER BY timestamp DESC, id DESC LIMIT $` + fmt.Sprintf("%d", len(args))
	if !useCursor {
		args = append(args, filter.Offset)
		query += ` OFFSET $` + fmt.Sprintf("%d", len(args))
	}

	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()

	var events []domain.UsageEvent
	for rows.Next() {
		var e domain.UsageEvent
		if err := rows.Scan(&e.ID, &e.TenantID, &e.CustomerID, &e.MeterID,
			&e.Quantity, propertiesScanner{&e.Dimensions},
			&e.IdempotencyKey, &e.Timestamp,
			&e.ProviderCostMicros, &e.ProviderCostSource); err != nil {
			return nil, 0, err
		}
		events = append(events, e)
	}
	return events, total, rows.Err()
}

// Aggregate returns the totals + per-meter breakdown that powers the
// /usage page's stat cards + "Usage by Meter" card. It honours the same
// filter as List (customer / meter / from / to) but ignores limit/offset —
// the whole point is to surface filtered totals, not page slices.
//
// SQL strategy: one row-of-totals query for COUNT/SUM/distinct-counts,
// plus a GROUP BY meter_id query for the per-meter breakdown. Both run
// inside the same tenant tx so RLS scopes them identically. SUM is
// decimal-precision (NUMERIC(38,12)) so 0.5 + 0.5 + 0.0001 round-trips
// to "1.0001" without floating-point drift.
func (s *PostgresStore) Aggregate(ctx context.Context, filter ListFilter) (Aggregate, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, filter.TenantID)
	if err != nil {
		return Aggregate{}, err
	}
	defer postgres.Rollback(tx)

	where, args := buildUsageWhere(filter)

	var agg Aggregate
	err = tx.QueryRowContext(ctx, `
		SELECT
			COUNT(*),
			COALESCE(SUM(quantity), 0),
			COUNT(DISTINCT meter_id),
			COUNT(DISTINCT customer_id)
		FROM usage_events`+where, args...,
	).Scan(&agg.TotalEvents, &agg.TotalUnits, &agg.ActiveMeters, &agg.ActiveCustomers)
	if err != nil {
		return Aggregate{}, fmt.Errorf("aggregate totals: %w", err)
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT meter_id, COALESCE(SUM(quantity), 0)
		FROM usage_events`+where+`
		GROUP BY meter_id
		ORDER BY 2 DESC`, args...)
	if err != nil {
		return Aggregate{}, fmt.Errorf("aggregate by_meter: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var m MeterTotal
		if err := rows.Scan(&m.MeterID, &m.Total); err != nil {
			return Aggregate{}, err
		}
		agg.ByMeter = append(agg.ByMeter, m)
	}
	if err := rows.Err(); err != nil {
		return Aggregate{}, err
	}
	if agg.ByMeter == nil {
		agg.ByMeter = []MeterTotal{}
	}
	return agg, nil
}

func (s *PostgresStore) AggregateForBillingPeriodByAgg(ctx context.Context, tenantID, customerID string, meters map[string]string, from, to time.Time) (map[string]decimal.Decimal, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	result := make(map[string]decimal.Decimal)
	if len(meters) == 0 {
		return result, nil
	}

	// One query per DISTINCT aggregation function (at most four) rather
	// than one per meter. A subscription spanning M meters cost M round
	// trips per billing period — and billOnePeriod calls this up to four
	// times (base, pre-cap re-derive, per-interval), so the fan-out was
	// multiplicative in M.
	byAgg := make(map[string][]string, 4)
	for meterID, agg := range meters {
		byAgg[agg] = append(byAgg[agg], meterID)
	}
	aggs := make([]string, 0, len(byAgg))
	for agg := range byAgg {
		aggs = append(aggs, agg)
	}
	sort.Strings(aggs)

	for _, agg := range aggs {
		meterIDs := byAgg[agg]
		sort.Strings(meterIDs)

		args := []any{tenantID, customerID, from, to}
		placeholders := make([]string, len(meterIDs))
		for i, id := range meterIDs {
			placeholders[i] = fmt.Sprintf("$%d", i+5)
			args = append(args, id)
		}
		in := strings.Join(placeholders, ",")

		// tenant_id is in the WHERE clause even though the tx is already
		// RLS-scoped. The policy is an OR (bypass_rls OR tenant match),
		// which Postgres can only apply as a post-scan Filter — never an
		// Index Cond. Every usage_events btree leads with tenant_id, so
		// without this predicate the leading column is unconstrained and
		// the aggregate seq-scans EVERY tenant's events. Same defect class
		// migration 0147 fixed for audit_log.
		var query string
		if agg == "last" {
			// Latest in-period event per meter. id DESC breaks
			// same-timestamp ties so the billed total is reproducible
			// across replicas (same rationale as ranked_rules below).
			query = `SELECT DISTINCT ON (meter_id) meter_id, COALESCE(quantity, 0)
				FROM usage_events
				WHERE tenant_id = $1 AND customer_id = $2
				  AND timestamp >= $3 AND timestamp < $4
				  AND meter_id IN (` + in + `)
				ORDER BY meter_id, timestamp DESC, id DESC`
		} else {
			aggFunc := "SUM"
			switch agg {
			case "max":
				aggFunc = "MAX"
			case "count":
				aggFunc = "COUNT"
			}
			query = fmt.Sprintf(`SELECT meter_id, COALESCE(%s(quantity), 0)
				FROM usage_events
				WHERE tenant_id = $1 AND customer_id = $2
				  AND timestamp >= $3 AND timestamp < $4
				  AND meter_id IN (%s)
				GROUP BY meter_id`, aggFunc, in)
		}

		// Propagate query errors — swallowing them silently drops the
		// meter from the billing total (under-billing at finalize).
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, fmt.Errorf("aggregate meters (%s): %w", agg, err)
		}
		scanErr := func() error {
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var meterID string
				var val decimal.Decimal
				if err := rows.Scan(&meterID, &val); err != nil {
					return err
				}
				// Only a positive aggregate is a billable line. A meter
				// with no events in the period produces no row at all,
				// which lands in the same place: absent from the map.
				if val.IsPositive() {
					result[meterID] = val
				}
			}
			return rows.Err()
		}()
		if scanErr != nil {
			return nil, fmt.Errorf("aggregate meters (%s): %w", agg, scanErr)
		}
	}

	return result, nil
}

// AggregateDailyBuckets — see Store interface for contract. UTC-day
// granularity matches every reference platform (Datadog, OpenAI, AWS
// Cost Explorer); finer grain (hour) lives in a future bucket-grain
// param when an operator needs it. NULL meter_ids → empty result with
// no DB roundtrip. The result is unsorted; the service fills gaps and
// sorts by (bucket_start, meter_id) before serving.
func (s *PostgresStore) AggregateDailyBuckets(ctx context.Context, tenantID, customerID string, meterIDs []string, from, to time.Time) ([]DailyBucketRow, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	if len(meterIDs) == 0 {
		return nil, nil
	}

	placeholders := make([]string, len(meterIDs))
	args := []any{tenantID, customerID, from, to}
	for i, id := range meterIDs {
		placeholders[i] = fmt.Sprintf("$%d", i+5)
		args = append(args, id)
	}

	// tenant_id predicate: see AggregateForBillingPeriodByAgg — RLS alone
	// cannot drive an index scan here.
	rows, err := tx.QueryContext(ctx, `
		SELECT date_trunc('day', timestamp AT TIME ZONE 'UTC') AS bucket_start,
		       meter_id,
		       COALESCE(SUM(quantity), 0) AS qty
		FROM usage_events
		WHERE tenant_id = $1
		  AND customer_id = $2
		  AND timestamp >= $3
		  AND timestamp < $4
		  AND meter_id IN (`+strings.Join(placeholders, ",")+`)
		GROUP BY 1, 2
	`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []DailyBucketRow
	for rows.Next() {
		var row DailyBucketRow
		if err := rows.Scan(&row.BucketStart, &row.MeterID, &row.Quantity); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func (s *PostgresStore) AggregateForBillingPeriod(ctx context.Context, tenantID, customerID string, meterIDs []string, from, to time.Time) (map[string]decimal.Decimal, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	result := make(map[string]decimal.Decimal)
	if len(meterIDs) == 0 {
		return result, nil
	}

	placeholders := make([]string, len(meterIDs))
	args := []any{tenantID, customerID, from, to}
	for i, id := range meterIDs {
		placeholders[i] = fmt.Sprintf("$%d", i+5)
		args = append(args, id)
	}

	// tenant_id predicate: see AggregateForBillingPeriodByAgg.
	rows, err := tx.QueryContext(ctx, `
		SELECT meter_id, COALESCE(SUM(quantity), 0)
		FROM usage_events
		WHERE tenant_id = $1 AND customer_id = $2
			AND timestamp >= $3 AND timestamp < $4
			AND meter_id IN (`+strings.Join(placeholders, ",")+`)
		GROUP BY meter_id
	`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var meterID string
		var total decimal.Decimal
		if err := rows.Scan(&meterID, &total); err != nil {
			return nil, err
		}
		result[meterID] = total
	}
	return result, rows.Err()
}

// AggregateByPricingRules implements the priority+claim resolution path
// described in docs/design-multi-dim-meters.md. Strategy:
//
//  1. Rank meter_pricing_rules by (priority DESC, created_at ASC, id ASC)
//     so the ROW_NUMBER is fully deterministic. The id tiebreaker covers
//     the corner case where two rules share the same priority AND were
//     inserted within the same created_at tick (bulk import, same-txn
//     bootstrap, clock-resolution collisions) — without it, Postgres is
//     free to order them differently across servers, which would make
//     billing non-reproducible across replicas.
//  2. LEFT JOIN LATERAL each in-period event against the ranked rules,
//     keeping only the top-priority rule whose dimension_match is a
//     subset of the event's properties. NULL rule means unclaimed.
//  3. Aggregate per rule. The CASE inside the SELECT is safe because
//     every event in a given (rule_id) group shares the rule's mode —
//     we GROUP BY (rule_id, mode, rrv) so the CASE evaluates a constant
//     within each group.
//
// last_ever needs a separate query because it ignores the period bounds.
// We run it only if any of the meter's rules is last_ever, then merge.
func (s *PostgresStore) AggregateByPricingRules(
	ctx context.Context,
	tenantID, customerID, meterID string,
	defaultMode domain.AggregationMode,
	from, to time.Time,
) ([]domain.RuleAggregation, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	// Period-bounded modes: sum, count, last_during_period, max, plus
	// the unclaimed bucket (defaultMode is one of these — last_ever on
	// the meter default is rejected at the service layer).
	//
	// last_during_period uses (array_agg ORDER BY ts DESC)[1] to pick
	// the latest event's quantity per group — a standard Postgres trick
	// that compiles down to a single sort over the group.
	rows, err := tx.QueryContext(ctx, `
		WITH ranked_rules AS (
			SELECT id, dimension_match, aggregation_mode, rating_rule_version_id,
			       ROW_NUMBER() OVER (ORDER BY priority DESC, created_at ASC, id ASC) AS rule_rank
			FROM meter_pricing_rules
			WHERE tenant_id = $1 AND meter_id = $2
		),
		claims AS (
			SELECT
				e.id,
				e.quantity,
				e.timestamp,
				rr.id                     AS rule_id,
				rr.aggregation_mode       AS aggregation_mode,
				rr.rating_rule_version_id AS rating_rule_version_id
			FROM usage_events e
			LEFT JOIN LATERAL (
				-- ALL rules (last_ever included) compete to claim the event,
				-- so rule_rank picks the true top-priority match. last_ever
				-- must NOT be filtered out of the candidate set here: if it
				-- were, an event whose real winner is a last_ever rule would
				-- fall through to a lower-priority period rule (or the
				-- unclaimed bucket) AND get claimed again by the last_ever
				-- pass below — double-counting it. We exclude such events
				-- from the period buckets after the claim instead.
				SELECT id, aggregation_mode, rating_rule_version_id, rule_rank
				FROM ranked_rules
				WHERE e.properties @> ranked_rules.dimension_match
				ORDER BY rule_rank ASC
				LIMIT 1
			) rr ON TRUE
			WHERE e.tenant_id  = $1
			  AND e.meter_id   = $2
			  AND e.customer_id = $3
			  AND e.timestamp >= $4
			  AND e.timestamp <  $5
			  -- Events whose winning rule is last_ever belong solely to the
			  -- last_ever pass; drop them from the period-bounded set so they
			  -- neither double-count nor leak into the unclaimed default.
			  AND COALESCE(rr.aggregation_mode, '') <> 'last_ever'
		)
		SELECT
			COALESCE(rule_id, '')                AS rule_id,
			COALESCE(rating_rule_version_id, '') AS rating_rule_version_id,
			COALESCE(aggregation_mode, $6)       AS aggregation_mode,
			CASE COALESCE(aggregation_mode, $6)
				WHEN 'sum'                THEN COALESCE(SUM(quantity), 0)
				WHEN 'count'              THEN COUNT(*)::numeric
				WHEN 'max'                THEN COALESCE(MAX(quantity), 0)
				WHEN 'last_during_period' THEN (array_agg(quantity ORDER BY timestamp DESC))[1]
				ELSE 0
			END AS quantity
		FROM claims
		GROUP BY rule_id, rating_rule_version_id, aggregation_mode
	`, tenantID, meterID, customerID, from, to, string(defaultMode))
	if err != nil {
		return nil, fmt.Errorf("aggregate by pricing rules (period): %w", err)
	}

	var out []domain.RuleAggregation
	for rows.Next() {
		var r domain.RuleAggregation
		var mode string
		if err := rows.Scan(&r.RuleID, &r.RatingRuleVersionID, &mode, &r.Quantity); err != nil {
			_ = rows.Close()
			return nil, err
		}
		r.AggregationMode = domain.AggregationMode(mode)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	// last_ever pass — each last_ever rule's quantity is the most recent
	// event (across all time) CLAIMED by it. Claim semantics must be truly
	// identical to the period-bounded path: ALL of the meter's rules compete
	// for each event and the single top-priority match wins; only events
	// whose WINNER is a last_ever rule belong here.
	//
	// The pre-fix query filtered the candidate set to last_ever rules BEFORE
	// the claim (JOIN … WHERE aggregation_mode='last_ever'), so an event
	// already claimed by a higher-priority sum/max/count rule in the period
	// pass was claimed AGAIN by a lower-priority last_ever rule — the same
	// event billed twice (period bucket + last_ever bucket). Mirroring the
	// period pass's LATERAL top-1 claim and filtering on the winner's mode
	// AFTER the claim closes the double-count.
	//
	// The probe below is what makes the doc comment above true. This pass is
	// deliberately unbounded in time — that is what last_ever MEANS — so
	// running it unconditionally walked the customer's entire lifetime of
	// events for the meter on every invoice, every preview, and every hourly
	// threshold scan, including for the overwhelming majority of meters that
	// have no last_ever rule at all. The probe is an index hit on
	// (tenant_id, meter_id); the scan it skips is not.
	var hasLastEver bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM meter_pricing_rules
			WHERE tenant_id = $1 AND meter_id = $2
			  AND aggregation_mode = 'last_ever'
		)
	`, tenantID, meterID).Scan(&hasLastEver); err != nil {
		return nil, fmt.Errorf("aggregate by pricing rules (last_ever probe): %w", err)
	}
	if !hasLastEver {
		return out, nil
	}

	leverRows, err := tx.QueryContext(ctx, `
		WITH ranked_rules AS (
			SELECT id, dimension_match, aggregation_mode, rating_rule_version_id,
			       ROW_NUMBER() OVER (ORDER BY priority DESC, created_at ASC, id ASC) AS rule_rank
			FROM meter_pricing_rules
			WHERE tenant_id = $1 AND meter_id = $2
		),
		claims AS (
			SELECT
				e.id,
				e.quantity,
				e.timestamp,
				rr.id                     AS rule_id,
				rr.rating_rule_version_id AS rating_rule_version_id
			FROM usage_events e
			LEFT JOIN LATERAL (
				SELECT id, aggregation_mode, rating_rule_version_id, rule_rank
				FROM ranked_rules
				WHERE e.properties @> ranked_rules.dimension_match
				ORDER BY rule_rank ASC
				LIMIT 1
			) rr ON TRUE
			WHERE e.tenant_id   = $1
			  AND e.meter_id    = $2
			  AND e.customer_id = $3
			  AND rr.aggregation_mode = 'last_ever'
		)
		SELECT DISTINCT ON (rule_id)
			rule_id, rating_rule_version_id, quantity
		FROM claims
		ORDER BY rule_id, timestamp DESC
	`, tenantID, meterID, customerID)
	if err != nil {
		return nil, fmt.Errorf("aggregate by pricing rules (last_ever): %w", err)
	}
	defer func() { _ = leverRows.Close() }()

	for leverRows.Next() {
		var r domain.RuleAggregation
		r.AggregationMode = domain.AggLastEver
		if err := leverRows.Scan(&r.RuleID, &r.RatingRuleVersionID, &r.Quantity); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := leverRows.Err(); err != nil {
		return nil, err
	}

	return out, nil
}

func buildUsageWhere(f ListFilter) (string, []any) {
	var clauses []string
	var args []any
	idx := 1

	// Leading predicate, and the reason this is not redundant with the
	// RLS-scoped tx: the policy is an OR (bypass_rls OR tenant match), so
	// Postgres applies it as a post-scan Filter and can never use it as an
	// Index Cond. Every usage_events btree leads with tenant_id — without
	// this clause the list, the COUNT, and the /usage stat-card aggregate
	// all seq-scan every tenant's events.
	if f.TenantID != "" {
		clauses = append(clauses, fmt.Sprintf("tenant_id = $%d", idx))
		args = append(args, f.TenantID)
		idx++
	}
	if f.CustomerID != "" {
		clauses = append(clauses, fmt.Sprintf("customer_id = $%d", idx))
		args = append(args, f.CustomerID)
		idx++
	}
	if f.MeterID != "" {
		clauses = append(clauses, fmt.Sprintf("meter_id = $%d", idx))
		args = append(args, f.MeterID)
		idx++
	}
	if f.From != nil {
		clauses = append(clauses, fmt.Sprintf("timestamp >= $%d", idx))
		args = append(args, *f.From)
		idx++
	}
	if f.To != nil {
		clauses = append(clauses, fmt.Sprintf("timestamp < $%d", idx))
		args = append(args, *f.To)
		idx++
	}
	if len(f.Dimensions) > 0 {
		// One containment doc carrying ALL pairs — JSONB @> on a
		// multi-key object is AND semantics, and the GIN index on
		// properties (migration 0062) serves it. Marshal cannot fail
		// for a map built from query-string scalars.
		doc, _ := json.Marshal(f.Dimensions)
		clauses = append(clauses, fmt.Sprintf("properties @> $%d::jsonb", idx))
		args = append(args, string(doc))
	}

	if len(clauses) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

// propertiesJSON serializes the event's free-form properties for the JSONB
// column. This map is also the carrier for multi-dim meter dimensions
// (model, operation, region, etc.) that pricing_rule.dimension_match runs
// subset-match against — losing it here would silently drop dimension
// information at ingest, so a marshal failure is treated as a hard error.
func propertiesJSON(props map[string]any) (string, error) {
	if len(props) == 0 {
		return "{}", nil
	}
	b, err := json.Marshal(props)
	if err != nil {
		return "", fmt.Errorf("marshal usage_event.properties: %w", err)
	}
	return string(b), nil
}

// propertiesScanner adapts a *map[string]any to sql.Scanner so SELECT
// statements can read the JSONB column straight into the struct field.
// The pgx driver hands JSONB to Scan as []byte (or string in some paths).
type propertiesScanner struct{ dst *map[string]any }

func (s propertiesScanner) Scan(src any) error {
	if src == nil {
		*s.dst = nil
		return nil
	}
	var raw []byte
	switch v := src.(type) {
	case []byte:
		raw = v
	case string:
		raw = []byte(v)
	default:
		return fmt.Errorf("unsupported scan type for properties: %T", src)
	}
	if len(raw) == 0 {
		*s.dst = nil
		return nil
	}
	m := map[string]any{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("unmarshal usage_event.properties: %w", err)
	}
	*s.dst = m
	return nil
}

// StreamForExport walks EVERY usage event in the [from,to] window in ONE
// snapshot transaction. See customer.PostgresStore.StreamForExport for the
// general reason (issue #475); usage is the case that made it urgent. This
// table is the hottest write path in the product, and the export is given a
// five-minute timeout PRECISELY because it streams a lot of rows — which is
// exactly the window in which concurrent ingest is guaranteed. Under the old
// LIMIT/OFFSET-per-transaction paging, every event ingested mid-export shifted
// the newest-first window and duplicated the row on a page boundary. A
// duplicated usage row in a finance CSV reads as usage that did not happen.
//
// The SELECT carries `origin`, which List's does not — so the `origin` column
// in usage-events.csv was always empty, and an operator could not tell an
// operator BACKFILL from a metered API event in the artifact they reconcile
// with. The export's columns are its own contract.
func (s *PostgresStore) StreamForExport(ctx context.Context, tenantID string, from, to time.Time, fn func(domain.UsageEvent) error) error {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)

	// Tenant + livemode isolation is RLS's (TxTenant), as in List. The window is
	// REQUIRED by the caller (the table is unbounded), so both bounds always bind.
	rows, err := tx.QueryContext(ctx, `
		SELECT id, tenant_id, customer_id, meter_id,
			quantity, properties, COALESCE(idempotency_key,''), timestamp, origin,
			provider_cost_micros, COALESCE(provider_cost_source,'')
		FROM usage_events
		WHERE timestamp >= $1 AND timestamp <= $2
		ORDER BY timestamp DESC, id DESC`, from, to)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var e domain.UsageEvent
		if err := rows.Scan(&e.ID, &e.TenantID, &e.CustomerID, &e.MeterID,
			&e.Quantity, propertiesScanner{&e.Dimensions},
			&e.IdempotencyKey, &e.Timestamp, &e.Origin,
			&e.ProviderCostMicros, &e.ProviderCostSource); err != nil {
			return err
		}
		if err := fn(e); err != nil {
			return err
		}
	}
	return rows.Err()
}
