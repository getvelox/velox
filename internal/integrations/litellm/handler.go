package litellm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/sagarsuperuser/velox/internal/api/respond"
	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/usage"
)

// CustomerLookup is the narrow surface the adapter uses to resolve
// LiteLLM's `user` field (= external_customer_id) to a Velox internal
// customer_id. Implemented by *customer.PostgresStore.
type CustomerLookup interface {
	GetByExternalID(ctx context.Context, tenantID, externalID string) (domain.Customer, error)
}

// MeterLookup is the narrow surface the adapter uses to resolve the
// meter_key ("tokens_input" / "tokens_output") to a Velox internal
// meter_id. Implemented by *pricing.Service.
type MeterLookup interface {
	GetMeterByKey(ctx context.Context, tenantID, key string) (domain.Meter, error)
}

// Ingester is the narrow surface the adapter calls to actually
// persist resolved events. Implemented by *usage.Service.
type Ingester interface {
	Ingest(ctx context.Context, tenantID string, input usage.IngestInput) (domain.UsageEvent, error)
}

// Handler exposes POST /v1/integrations/litellm/spend. Auth is the
// standard API key (Bearer); operator generates a Velox key and
// pastes it into LiteLLM's GENERIC_LOGGER_HEADERS as
// `Authorization: Bearer <vlx_secret_…>`.
type Handler struct {
	customers CustomerLookup
	meters    MeterLookup
	ingester  Ingester
}

// New constructs the LiteLLM adapter handler. All three deps are
// required — adapter does no useful work without persistence.
func New(customers CustomerLookup, meters MeterLookup, ingester Ingester) *Handler {
	return &Handler{
		customers: customers,
		meters:    meters,
		ingester:  ingester,
	}
}

// Routes returns the chi router for /v1/integrations/litellm/*.
// Mounted by api/router.go under the standard /v1 auth-required
// stack — LiteLLM's generic_api callback sets the Bearer header on
// every POST, so the Velox auth middleware enforces the tenant.
func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Post("/spend", h.spend)
	return r
}

// SpendResponse is the wire shape returned by POST /spend. Mirrors
// the existing /v1/usage-events/batch response so partners using
// both surfaces see one shape:
//
//	{ "accepted": N, "deduplicated": D, "skipped": M, "errors": [{ "id": "...", "error": "..." }] }
//
// accepted counts rows the store NEWLY recorded. Deduplicated counts rows
// it already held — a success, since LiteLLM redelivering a batch is the
// happy path, but not new usage. They were previously summed into
// accepted, so a pure replay reported the same number as the original
// delivery and an operator reconciling spend could not tell a retry from
// fresh consumption. /v1/usage-events/batch already separated the two.
type SpendResponse struct {
	Accepted     int             `json:"accepted"`
	Deduplicated int             `json:"deduplicated"`
	Skipped      int             `json:"skipped"`
	Errors       []SpendRowError `json:"errors,omitempty"`
}

// SpendRowError carries the per-payload reason a particular row was
// rejected. The adapter is intentionally permissive — one bad row
// doesn't fail the whole batch. LiteLLM retries the whole batch on
// 5xx, so per-row 422 / "skip" semantics avoid retry storms when a
// single misconfigured call lacks `user`.
type SpendRowError struct {
	ID    string `json:"id"`
	Error string `json:"error"`
}

// spend handles the LiteLLM generic_api callback. Accepts either a
// single payload (LiteLLM's default callback shape) or a batch
// (operator-side buffered shape).
//
// Flow:
//  1. Decode body; normalize single | batch to []StandardLoggingPayload
//  2. For each payload: MapPayload → 0/1/2 ExternalIngest events
//  3. Resolve external_customer_id + meter_key per event
//  4. Call usage.Service.Ingest per event (idempotent via the
//     usage_events UNIQUE (tenant_id, livemode, idempotency_key) —
//     tenant-wide, NOT per customer/meter, which is why the mapper
//     suffixes the key per token type)
//  5. Tally accepted / skipped / errors; return 200 with envelope
//
// The status code answers ONE question: did Velox reach a verdict on
// this batch? Per-row verdicts — a customer or meter the operator has
// not mapped, a payload that fails validation, a row the store already
// holds — stay 200 with errors[] / deduplicated, so one misconfigured
// call never fails the batch and never makes LiteLLM retry it. A
// NON-verdict — the usage store could not be reached or could not
// commit (managed-Postgres failover, a replica losing its pool during
// a rolling restart, a stalled connection) — aborts the batch fail-fast
// with 503 api_error/ingest_unavailable so LiteLLM's retry (configure
// max_retries > 0; docs/integrations/litellm.md) re-sends it. Before
// 2026-08-30 this path returned 200 with the failure filed as a
// customer-not-found row; LiteLLM's generic logger treats any 2xx as
// delivered and clears its queue, so every completion in the outage
// window was silently lost (ADR-033 amendment 2026-08-30). Whole-batch
// retry is safe by construction: every row is idempotency-keyed and the
// store dedups on (tenant_id, livemode, idempotency_key), so rows
// committed before the abort come back as deduplicated.
func (h *Handler) spend(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())

	body, err := decodeBody(r)
	if err != nil {
		respond.BadRequest(w, r, err.Error())
		return
	}

	resp := SpendResponse{Errors: []SpendRowError{}}

	for _, payload := range body {
		ingests, err := MapPayload(payload)
		if err != nil {
			resp.Errors = append(resp.Errors, SpendRowError{
				ID:    payload.ID,
				Error: err.Error(),
			})
			continue
		}
		if len(ingests) == 0 {
			// Non-token-bearing call (image gen, moderation, etc.)
			// or zero-token completion (error response). Counted as
			// skipped — not rejected — so the operator's spend
			// dashboard reconciles.
			resp.Skipped++
			continue
		}
		for _, ing := range ingests {
			switch err := h.persist(r.Context(), tenantID, ing); {
			case err == nil:
				resp.Accepted++
			case errors.Is(err, errDuplicateRow):
				// Already held by the store. Still a success — LiteLLM
				// retrying a delivered batch is the happy path — but
				// counting it as accepted told an operator reconciling
				// spend that a replay had recorded fresh usage.
				resp.Deduplicated++
			case errors.Is(err, errIngestUnavailable):
				// Non-verdict: abort the whole batch for client retry.
				// The raw error is logged, never sent to the caller (ADR-026).
				slog.ErrorContext(r.Context(), "litellm spend: usage store unavailable — batch aborted for client retry",
					"tenant_id", tenantID,
					"accepted", resp.Accepted,
					"deduplicated", resp.Deduplicated,
					"error", err,
				)
				respond.Error(w, r, http.StatusServiceUnavailable, "api_error", "ingest_unavailable",
					"usage store temporarily unavailable — retry the whole batch; rows already recorded replay as deduplicated")
				return
			default:
				resp.Errors = append(resp.Errors, SpendRowError{
					ID:    payload.ID,
					Error: err.Error(),
				})
			}
		}
	}

	if len(resp.Errors) > 0 {
		slog.WarnContext(r.Context(), "litellm spend: partial failure",
			"tenant_id", tenantID,
			"accepted", resp.Accepted,
			"deduplicated", resp.Deduplicated,
			"skipped", resp.Skipped,
			"errors", len(resp.Errors),
		)
	}
	respond.JSON(w, r, http.StatusOK, resp)
}

// persist resolves an ExternalIngest (external IDs) to the internal
// shape and calls the existing usage Ingest path. It classifies every
// error into a VERDICT (returned as a per-row error: operator
// misconfiguration such as an unmapped customer or meter, a validation
// failure, or a duplicate) or a NON-VERDICT (wrapped in
// errIngestUnavailable: the store could not be reached or could not
// commit). Both lookup stores return errs.ErrNotFound only on
// sql.ErrNoRows and the raw driver error otherwise, which is what makes
// this split honest — a BeginTx failure used to be reported as
// "customer not found".
func (h *Handler) persist(ctx context.Context, tenantID string, ing ExternalIngest) error {
	cust, err := h.customers.GetByExternalID(ctx, tenantID, ing.ExternalCustomerID)
	if err != nil {
		if errors.Is(err, errs.ErrNotFound) {
			return fmt.Errorf("customer %q not found (set user=<external_customer_id> on the LiteLLM call)", ing.ExternalCustomerID)
		}
		return fmt.Errorf("%w: customer lookup: %w", errIngestUnavailable, err)
	}
	meter, err := h.meters.GetMeterByKey(ctx, tenantID, ing.MeterKey)
	if err != nil {
		if errors.Is(err, errs.ErrNotFound) {
			return fmt.Errorf("meter %q not found (create it via the recipe or POST /v1/meters)", ing.MeterKey)
		}
		return fmt.Errorf("%w: meter lookup: %w", errIngestUnavailable, err)
	}

	input := usage.IngestInput{
		CustomerID:     cust.ID,
		MeterID:        meter.ID,
		Quantity:       ing.Quantity,
		Dimensions:     ing.Dimensions,
		IdempotencyKey: ing.IdempotencyKey,
		Timestamp:      ing.Timestamp,
		// ADR-079 D4: the provider's own per-half cost, already filtered by
		// observedCostMicros to the halves D4 allows. nil leaves rate-table
		// inference in charge, which is every pre-existing ingest path.
		ObservedCostMicros: ing.ObservedCostMicros,
	}

	if _, err := h.ingester.Ingest(ctx, tenantID, input); err != nil {
		// Idempotency replay is silent success: the usage store returns
		// ErrDuplicateKey on the (tenant_id, livemode, idempotency_key) UNIQUE — a
		// LiteLLM network-retry redelivering an already-ingested batch is
		// the happy path, not a failure. Pre-fix this matched only
		// ErrAlreadyExists (which the store never returns here), so every
		// replay filled errors[] + WARN logs while the DB dedup was in
		// fact working (front-door audit 2026-07-05). Any other error is
		// a real persistence problem and bubbles to the partial-failure
		// accounting.
		if errors.Is(err, errs.ErrDuplicateKey) || errors.Is(err, errs.ErrAlreadyExists) {
			return errDuplicateRow
		}
		// Verdicts on the row itself stay per-row: a bad payload must
		// never 503, or LiteLLM retries it until max_retries is spent.
		// Classified on Kind sentinels only — a DomainError's Code is not
		// a verdict signal (a future infra error carrying a code would be
		// misfiled as per-row).
		if errors.Is(err, errs.ErrValidation) || errors.Is(err, errs.ErrNotFound) {
			return err
		}
		return fmt.Errorf("%w: ingest: %w", errIngestUnavailable, err)
	}
	return nil
}

// errIngestUnavailable marks a NON-verdict: the usage store could not be
// reached or could not commit. spend aborts the batch with 503 so the
// client retries it; never returned to the caller as text.
var errIngestUnavailable = errors.New("litellm: usage store unavailable")

// errDuplicateRow marks a row the store already held. It is a successful
// outcome, not a failure — it just must not be counted as newly recorded,
// so a replayed batch reports what it actually changed. Never returned to
// the caller.
var errDuplicateRow = errors.New("litellm: row already ingested")

// decodeBody normalizes the request body into a slice of payloads.
// Accepts:
//   - LiteLLM's default callback shape: single payload as the top-level object
//   - Batched shape: { "events": [...] } (used by some operator-side buffers)
//   - Bare array: [payload, payload, ...] (defensive — some HTTP clients send this)
func decodeBody(r *http.Request) ([]StandardLoggingPayload, error) {
	defer func() { _ = r.Body.Close() }()

	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty body")
	}

	// Bare array first — try [..] before the object shape.
	if raw[0] == '[' {
		var batch []StandardLoggingPayload
		if err := json.Unmarshal(raw, &batch); err == nil {
			return batch, nil
		}
	}

	// { events: [...] } shape.
	var withEvents struct {
		Events []StandardLoggingPayload `json:"events"`
	}
	if err := json.Unmarshal(raw, &withEvents); err == nil && len(withEvents.Events) > 0 {
		return withEvents.Events, nil
	}

	// Single payload.
	var one StandardLoggingPayload
	if err := json.Unmarshal(raw, &one); err != nil {
		return nil, fmt.Errorf("invalid JSON body")
	}
	return []StandardLoggingPayload{one}, nil
}
