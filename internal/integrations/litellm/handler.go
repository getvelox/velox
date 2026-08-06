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
// The handler NEVER returns 5xx once past decode: per-row persist
// failures — including a fully-down DB reached past auth — surface as
// errors[] entries with a 200 envelope, so callers MUST monitor
// errors[] (a full DB outage also tends to die earlier, at API-key
// auth, as a 401 per ADR-026). Rationale for no-5xx: a 5xx would make
// a retry-configured LiteLLM re-send the whole batch (dedup catches
// it, but wasted work) — and at stock config LiteLLM retries NOTHING
// anyway (max_retries=0, queue cleared on any send error; verified
// against LiteLLM source 2026-07-06). Recovery for dropped batches is
// operator replay of LiteLLM's spend logs — idempotency keys make it
// a pure gap-fill. See docs/dev/ha-readiness-2026-07-06.md.
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
// shape and calls the existing usage Ingest path. Errors that look
// like operator misconfiguration (customer / meter not found) are
// wrapped in the partial-failure path; storage errors propagate as
// 5xx via FromError → respond.InternalError.
func (h *Handler) persist(ctx context.Context, tenantID string, ing ExternalIngest) error {
	cust, err := h.customers.GetByExternalID(ctx, tenantID, ing.ExternalCustomerID)
	if err != nil {
		return fmt.Errorf("customer %q not found (set user=<external_customer_id> on the LiteLLM call)", ing.ExternalCustomerID)
	}
	meter, err := h.meters.GetMeterByKey(ctx, tenantID, ing.MeterKey)
	if err != nil {
		return fmt.Errorf("meter %q not found (create it via the recipe or POST /v1/meters)", ing.MeterKey)
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
		return err
	}
	return nil
}

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
