package usage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/api/middleware"
	"github.com/sagarsuperuser/velox/internal/api/respond"
	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
)

// CustomerResolver looks up a customer by external ID.
type CustomerResolver interface {
	GetByExternalID(ctx context.Context, tenantID, externalID string) (domain.Customer, error)
}

// MeterResolver looks up a meter by key.
type MeterResolver interface {
	GetMeterByKey(ctx context.Context, tenantID, key string) (domain.Meter, error)
}

type Handler struct {
	svc       *Service
	customers CustomerResolver
	meters    MeterResolver
}

func NewHandler(svc *Service, customers CustomerResolver, meters MeterResolver) *Handler {
	return &Handler{svc: svc, customers: customers, meters: meters}
}

func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Post("/", h.ingest)
	r.Post("/batch", h.batchIngest)
	r.Get("/", h.list)
	// Server-side aggregate that powers the /usage page's stat cards +
	// "Usage by Meter" breakdown. Mounted as a sibling of GET / so chi
	// picks the more-specific pattern; reuses parseListFilter so filter
	// semantics match the list query exactly. See internal/usage/store.go
	// (Aggregate type) for the response shape.
	r.Get("/aggregate", h.aggregate)
	return r
}

// Backfill is exported so the router can mount it behind PermUsageWrite —
// the /usage-events subtree uses PermUsageRead for historical reasons, but
// backfill is a sensitive ledger operation and should not be reachable by
// read-only keys.
func (h *Handler) Backfill(w http.ResponseWriter, r *http.Request) {
	h.backfill(w, r)
}

// apiEvent is the public API input — developers send external identifiers.
// Quantity accepts both number (5) and string ("5.5") forms via shopspring's
// UnmarshalJSON; the response side serializes as a string for precision.
//
// Dimensions: per docs/design-multi-dim-meters.md, AI-native ingest uses
// `dimensions` as the field name for the JSONB filter that pricing rules
// dispatch on. Properties is the original name and is kept as an alias
// for backward compatibility — if both are present, dimensions wins (it
// is the documented v1 field; properties was an internal name leaked
// into the wire). The two collapse onto the same usage_events.properties
// column at the storage layer.
type apiEvent struct {
	ExternalCustomerID string           `json:"external_customer_id"`
	EventName          string           `json:"event_name"`
	Quantity           decimal.Decimal  `json:"quantity,omitempty"`
	Properties         map[string]any   `json:"properties,omitempty"`
	Dimensions         map[string]any   `json:"dimensions,omitempty"`
	IdempotencyKey     string           `json:"idempotency_key,omitempty"`
	Timestamp          *json.RawMessage `json:"timestamp,omitempty"`
}

// resolveCache memoizes the customer and meter lookups for the lifetime of
// ONE request. Each lookup opens its own RLS transaction (BEGIN + three
// set_config calls + the SELECT), so a 1000-event batch for one customer and
// one meter paid 2000 of them — the dominant cost of batch ingest, and pure
// waste, since a batch is overwhelmingly one customer's traffic.
//
// The scope is deliberately per-request and NOT a process-lifetime cache: a
// longer-lived one would keep serving a customer deleted mid-flight or a meter
// re-keyed by another operator, with no invalidation path. Within a single
// request that staleness window does not exist.
type resolveCache struct {
	customers map[string]string // external customer id -> internal id
	meters    map[string]string // meter key -> meter id
}

func newResolveCache() *resolveCache {
	return &resolveCache{
		customers: make(map[string]string),
		meters:    make(map[string]string),
	}
}

// The four accessors are nil-safe so the single-event doors can pass nil and
// take the uncached path without a branch at every call site.
func (c *resolveCache) customer(key string) (string, bool) {
	if c == nil {
		return "", false
	}
	id, ok := c.customers[key]
	return id, ok
}

func (c *resolveCache) putCustomer(key, id string) {
	if c != nil {
		c.customers[key] = id
	}
}

func (c *resolveCache) meter(key string) (string, bool) {
	if c == nil {
		return "", false
	}
	id, ok := c.meters[key]
	return id, ok
}

func (c *resolveCache) putMeter(key, id string) {
	if c != nil {
		c.meters[key] = id
	}
}

// resolve converts public API identifiers to internal IDs. cache may be nil
// (single-event doors), in which case every lookup goes to the store.
func (h *Handler) resolve(ctx context.Context, tenantID string, evt apiEvent, cache *resolveCache) (IngestInput, error) {
	extCust := strings.TrimSpace(evt.ExternalCustomerID)
	eventName := strings.TrimSpace(evt.EventName)

	if extCust == "" {
		return IngestInput{}, errs.Required("external_customer_id")
	}
	if eventName == "" {
		return IngestInput{}, errs.Required("event_name")
	}

	// A miss is cached only on success: caching the "not found" verdict would
	// turn one bad index in a batch into a repeated failure for every later
	// event naming the same customer, which is the same answer but a slower
	// and less obvious one to debug.
	custID, ok := cache.customer(extCust)
	if !ok {
		cust, err := h.customers.GetByExternalID(ctx, tenantID, extCust)
		if err != nil {
			return IngestInput{}, errs.Invalid("external_customer_id", fmt.Sprintf("customer %q not found", extCust))
		}
		custID = cust.ID
		cache.putCustomer(extCust, custID)
	}

	meterID, ok := cache.meter(eventName)
	if !ok {
		meter, err := h.meters.GetMeterByKey(ctx, tenantID, eventName)
		if err != nil {
			return IngestInput{}, errs.Invalid("event_name", fmt.Sprintf("meter %q not found", eventName))
		}
		meterID = meter.ID
		cache.putMeter(eventName, meterID)
	}

	// dimensions takes precedence over properties — see apiEvent doc.
	dims := evt.Dimensions
	if dims == nil {
		dims = evt.Properties
	}

	input := IngestInput{
		CustomerID:     custID,
		MeterID:        meterID,
		Quantity:       evt.Quantity,
		Dimensions:     dims,
		IdempotencyKey: evt.IdempotencyKey,
	}

	if evt.Timestamp != nil {
		// A caller that sends a timestamp means it. Silently discarding a
		// malformed/non-string value and falling back to wall-clock now
		// back-dates or future-dates usage into the wrong billing period —
		// reject it instead, matching the Backfill and getSummary paths.
		var ts any
		if err := json.Unmarshal(*evt.Timestamp, &ts); err != nil {
			return IngestInput{}, errs.Invalid("timestamp", "must be an RFC3339 string, e.g. 2026-05-31T12:00:00Z")
		}
		tsStr, ok := ts.(string)
		if !ok {
			return IngestInput{}, errs.Invalid("timestamp", "must be an RFC3339 string, e.g. 2026-05-31T12:00:00Z")
		}
		parsed, err := parseTimestamp(tsStr)
		if err != nil {
			return IngestInput{}, errs.Invalid("timestamp", "must be an RFC3339 string, e.g. 2026-05-31T12:00:00Z")
		}
		input.Timestamp = &parsed
	}

	return input, nil
}

func (h *Handler) ingest(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())

	var evt apiEvent
	if err := json.NewDecoder(r.Body).Decode(&evt); err != nil {
		respond.BadRequest(w, r, "invalid JSON body")
		return
	}

	input, err := h.resolve(r.Context(), tenantID, evt, nil)
	if err != nil {
		respond.FromError(w, r, err, "usage_event")
		return
	}

	event, err := h.svc.Ingest(r.Context(), tenantID, input)
	if err != nil {
		h.respondIngestError(w, r, tenantID, input.IdempotencyKey, err)
		return
	}

	respond.JSON(w, r, http.StatusCreated, event)
}

// respondIngestError translates ingest failures, treating an idempotency
// replay as SUCCESS: 200 + the ORIGINAL event + Idempotent-Replayed
// header (Stripe idempotency shape, and the same header the API-wide
// Idempotency-Key middleware sets). Pre-fix a replay got a bare 409 with
// no fetch-original — retry middleware doing at-least-once delivery read
// healthy dedup as failure, inconsistent with the LiteLLM door's
// silent-success contract. Falls back to the plain error mapping if the
// original row can't be read.
func (h *Handler) respondIngestError(w http.ResponseWriter, r *http.Request, tenantID, idempotencyKey string, err error) {
	if errors.Is(err, errs.ErrDuplicateKey) && idempotencyKey != "" {
		if original, gerr := h.svc.GetByIdempotencyKey(r.Context(), tenantID, idempotencyKey); gerr == nil {
			// A replay MUTATES NOTHING — it hands back the row the original
			// request already wrote (and, for backfill, already audited).
			// Declare that to the audit observer: without it every client
			// retry of a backfill would report as an uncovered mutation, and
			// a second audit row would record an insert that never happened.
			audit.MarkSkip(r.Context())
			w.Header().Set("Idempotent-Replayed", "true")
			respond.JSON(w, r, http.StatusOK, original)
			return
		}
	}
	respond.FromError(w, r, err, "usage_event")
}

// backfill ingests a historical usage event — same payload shape as POST /
// but the service enforces that timestamp is non-nil and strictly in the
// past, and the row is tagged origin='backfill' in the audit trail.
//
// Use cases: CSV imports from a legacy system, reconciliation after a
// dropped-event incident, migrating from another billing vendor. For
// real-time ingest, use POST /usage-events instead.
func (h *Handler) backfill(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())

	var evt apiEvent
	if err := json.NewDecoder(r.Body).Decode(&evt); err != nil {
		respond.BadRequest(w, r, "invalid JSON body")
		return
	}

	input, err := h.resolve(r.Context(), tenantID, evt, nil)
	if err != nil {
		respond.FromError(w, r, err, "usage_event")
		return
	}

	event, err := h.svc.Backfill(r.Context(), tenantID, input)
	if err != nil {
		// Same replay-as-success contract as live ingest — backfill
		// shares the idempotency-key space.
		h.respondIngestError(w, r, tenantID, input.IdempotencyKey, err)
		return
	}

	respond.JSON(w, r, http.StatusCreated, event)
}

// parseListFilter pulls the shared list/aggregate query params off the
// request URL. Both handlers honour customer_id / meter_id / from / to;
// the list handler additionally reads limit + offset, which Aggregate
// ignores (the whole point of the aggregate endpoint is the unbounded
// total). Empty / unparseable values are treated as absent so the
// frontend can build URLs unconditionally without sending sentinel
// strings.
func parseListFilter(r *http.Request) ListFilter {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))

	filter := ListFilter{
		TenantID:   auth.TenantID(r.Context()),
		CustomerID: q.Get("customer_id"),
		MeterID:    q.Get("meter_id"),
		Limit:      limit,
		Offset:     offset,
	}
	// Cursor takes precedence over offset when both are present —
	// industry standard (Stripe ignores ?starting_after if ?ending_before
	// is set rather than mixing them). Malformed cursors silently fall
	// back to offset (no 400) because cursor is an opt-in upgrade path.
	if c := q.Get("after"); c != "" {
		if cur, err := middleware.DecodeCursor(c); err == nil {
			filter.AfterTimestamp = cur.CreatedAt
			filter.AfterID = cur.ID
		}
	}
	if v := q.Get("from"); v != "" {
		if t, err := parseTimestamp(v); err == nil {
			filter.From = &t
		}
	}
	if v := q.Get("to"); v != "" {
		if t, err := parseTimestamp(v); err == nil {
			filter.To = &t
		}
	}
	return filter
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	filter := parseListFilter(r)
	dims, err := parseDimensionsParam(r.URL.Query().Get("dimensions"))
	if err != nil {
		// Unlike the other list params, a malformed dimension filter is a
		// hard 422, not treated-as-absent — silently dropping it would
		// render UNfiltered data as filtered (the exact bug this param's
		// server side shipped with).
		respond.Validation(w, r, err.Error())
		return
	}
	filter.Dimensions = dims
	events, total, err := h.svc.List(r.Context(), filter)
	if err != nil {
		respond.InternalError(w, r)
		slog.ErrorContext(r.Context(), "list usage events", "error", err)
		return
	}
	if events == nil {
		events = []domain.UsageEvent{}
	}

	// Cursor path: store fetched limit+1, signaled by len(events) > limit.
	// Trim the over-fetch + mint the next cursor from the last row.
	if !filter.AfterTimestamp.IsZero() && filter.AfterID != "" {
		limit := filter.Limit
		if limit <= 0 {
			limit = 100
		} else if limit > 1000 {
			limit = 1000
		}
		hasMore := len(events) > limit
		if hasMore {
			events = events[:limit]
		}
		resp := middleware.PageResponse{Data: events, HasMore: hasMore}
		if hasMore && len(events) > 0 {
			last := events[len(events)-1]
			resp.NextCursor = middleware.EncodeCursor(last.ID, last.Timestamp)
		}
		respond.JSON(w, r, http.StatusOK, resp)
		return
	}

	respond.List(w, r, events, total)
}

// aggregate returns server-side totals + per-meter breakdown for the
// current filter. Mirrors the list handler's filter parsing exactly so
// the dashboard's stat cards reflect the same scope as the events table.
// Unlike list, limit/offset are ignored — Aggregate.TotalEvents is the
// authoritative count, and ByMeter is the full filtered breakdown.
func (h *Handler) aggregate(w http.ResponseWriter, r *http.Request) {
	filter := parseListFilter(r)
	dims, err := parseDimensionsParam(r.URL.Query().Get("dimensions"))
	if err != nil {
		// Hard 422 — same rationale as list: a dropped filter shows
		// unfiltered totals as filtered.
		respond.Validation(w, r, err.Error())
		return
	}
	filter.Dimensions = dims
	// Limit/offset only matter for paginated reads. Zero them out so
	// it's obvious from a debug log that the aggregate scanned the full
	// filtered set, not a 100-row page.
	filter.Limit = 0
	filter.Offset = 0

	agg, err := h.svc.Aggregate(r.Context(), filter)
	if err != nil {
		respond.InternalError(w, r)
		slog.ErrorContext(r.Context(), "aggregate usage events", "error", err)
		return
	}
	respond.JSON(w, r, http.StatusOK, agg)
}

// batchIngest is ALL-OR-NOTHING: every event validates, then the whole
// batch commits in one transaction. Pre-fix each event committed in its
// own tx and a mid-batch abort (client timeout, dropped connection) left
// a committed prefix — the standard retry-the-batch response re-ingested
// it, double-billing every event without an idempotency key. Replays of
// already-ingested keys are SUCCESS (counted in "deduplicated"), not
// error rows — an at-least-once delivery pipeline retrying a committed
// batch must not trip its alerting on HTTP 206 + 1000 'duplicate key'
// strings (and the LiteLLM door already treats dupes as success; two
// front doors, one contract).
func (h *Handler) batchIngest(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())

	var events []apiEvent
	if err := json.NewDecoder(r.Body).Decode(&events); err != nil {
		// The router caps request bodies via MaxBytesReader; a too-big
		// batch must read as "split your batch" (413), not as malformed
		// JSON (400) — the byte cap fires mid-array on valid JSON.
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			respond.Error(w, r, http.StatusRequestEntityTooLarge, "invalid_request_error", "batch_too_large",
				fmt.Sprintf("request body exceeds %d bytes — send smaller batches", maxErr.Limit))
			return
		}
		respond.BadRequest(w, r, "expected JSON array of events")
		return
	}

	if len(events) == 0 {
		respond.BadRequest(w, r, "at least one event is required")
		return
	}
	if len(events) > 1000 {
		respond.BadRequest(w, r, "maximum 1000 events per batch")
		return
	}

	// Resolve-stage failures (missing fields, unknown customer/meter,
	// malformed timestamp — the common 422 class) are collected for EVERY
	// failing index, same as the ingest stage below: a bare first-error
	// made 500-event batches undebuggable, and the spec promises one
	// errors[] naming every failing index for the batch 422.
	var inputs []IngestInput
	var resolveErrs []string
	// One cache for the whole batch — see resolveCache. A batch naming one
	// customer and one meter now costs two lookups instead of 2N.
	cache := newResolveCache()
	for i, evt := range events {
		input, err := h.resolve(r.Context(), tenantID, evt, cache)
		if err != nil {
			resolveErrs = append(resolveErrs, fmt.Sprintf("event[%d]: %s", i, err.Error()))
			continue
		}
		inputs = append(inputs, input)
	}
	if len(resolveErrs) > 0 {
		respondBatchRejected(w, r, resolveErrs, len(events))
		return
	}

	inserted, deduped, ingestErrs := h.svc.BatchIngest(r.Context(), tenantID, inputs)

	if len(ingestErrs) > 0 {
		errStrings := make([]string, len(ingestErrs))
		for i, e := range ingestErrs {
			errStrings[i] = e.Error()
		}
		respondBatchRejected(w, r, errStrings, len(events))
		return
	}

	respond.JSON(w, r, http.StatusCreated, map[string]any{
		"ingested":     inserted,
		"deduplicated": deduped,
		"total":        len(events),
	})
}

// respondBatchRejected writes the batch 422 the spec documents: every
// failing index in one errors[] array ("event[i]: reason") plus an
// explicit ingested:0 marker that the all-or-nothing batch wrote nothing.
// Shared by the resolve and ingest validation stages so both produce the
// same shape — pre-fix the resolve stage returned a bare first-error 422
// with no errors[], the exact undebuggable-batch failure the ingest
// stage's accumulation exists to prevent.
func respondBatchRejected(w http.ResponseWriter, r *http.Request, errStrings []string, total int) {
	respond.JSON(w, r, http.StatusUnprocessableEntity, map[string]any{
		"error": map[string]any{
			"type":    "invalid_request_error",
			"code":    "batch_rejected",
			"message": "batch rejected — nothing was ingested; fix the listed events and retry the whole batch",
		},
		"errors":   errStrings,
		"ingested": 0,
		"total":    total,
	})
}

// parseDimensionsParam parses the `dimensions` query param: comma-
// separated key=value pairs ("model=gpt-4o,token_type=input"). Values
// that parse as JSON booleans or numbers are typed accordingly
// ("cached=true" matches the JSONB boolean true, "attempt=2" the number
// 2) — matching how ingest stored them; everything else matches as a
// string. Malformed pairs are an ERROR, never treated-as-absent: the
// callers 422 so a broken filter can't render unfiltered data as
// filtered.
func parseDimensionsParam(raw string) (map[string]any, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	dims := make(map[string]any)
	for _, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		k, v, ok := strings.Cut(pair, "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !ok || k == "" || v == "" {
			return nil, fmt.Errorf("dimension filter %q must be key=value (e.g. model=gpt-4o)", pair)
		}
		var typed any = v
		var lit any
		if err := json.Unmarshal([]byte(v), &lit); err == nil {
			switch lit.(type) {
			case bool, float64:
				typed = lit
			}
		}
		dims[k] = typed
	}
	if len(dims) == 0 {
		return nil, nil
	}
	return dims, nil
}

func parseTimestamp(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("invalid timestamp format: %s", s)
}
