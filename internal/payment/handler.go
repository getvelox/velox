package payment

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stripe/stripe-go/v82/webhook"

	"github.com/sagarsuperuser/velox/internal/api/respond"
	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/tenantstripe"
)

const maxWebhookBodySize = 65536 // 64KB

// EndpointResolver is the narrow surface the webhook handler needs from
// tenantstripe.Service. Exists so the handler can be tested without a DB.
type EndpointResolver interface {
	LookupEndpoint(ctx context.Context, endpointID string) (tenantstripe.EndpointLookup, error)
}

type Handler struct {
	stripe    *Stripe
	endpoints EndpointResolver
}

// NewHandler wires the Stripe webhook receiver. Credentials (signing secret,
// tenant id, livemode) are resolved per request via the endpoint id embedded
// in the URL path — there is no operator-level secret: each tenant registers
// their own webhook endpoint in their own Stripe dashboard pointing at
// /v1/webhooks/stripe/{endpoint_id}, where endpoint_id is the
// stripe_provider_credentials.id (vlx_spc_XXX) Velox handed them when they
// connected. Moving secrets off env vars is the whole point of the per-
// tenant model (see migration 0032).
func NewHandler(stripe *Stripe, endpoints EndpointResolver) *Handler {
	return &Handler{
		stripe:    stripe,
		endpoints: endpoints,
	}
}

func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Post("/stripe/{endpoint_id}", h.handleStripeWebhook)
	return r
}

func (h *Handler) handleStripeWebhook(w http.ResponseWriter, r *http.Request) {
	endpointID := chi.URLParam(r, "endpoint_id")
	if endpointID == "" {
		respond.NotFound(w, r, "webhook_endpoint")
		return
	}

	// Look up the tenant-owned endpoint BEFORE reading the body — a bogus URL
	// shouldn't even buffer the payload.
	lookup, err := h.endpoints.LookupEndpoint(r.Context(), endpointID)
	if err != nil {
		if errors.Is(err, errs.ErrNotFound) {
			// Tenant disconnected / never registered the webhook, or someone
			// is probing random endpoint ids. 404 either way — don't leak
			// the distinction.
			respond.NotFound(w, r, "webhook_endpoint")
			return
		}
		slog.ErrorContext(r.Context(), "webhook endpoint lookup failed",
			"endpoint_id", endpointID, "error", err)
		respond.InternalError(w, r)
		return
	}

	// Read ONE byte beyond the cap so an oversized body is DETECTED, not
	// silently truncated. Pre-fix a >64KB event was truncated at the limit,
	// the HMAC then failed over the truncated bytes, and the request died as a
	// misleading "invalid signature" 400 — after ~3 days of Stripe retries the
	// event was permanently lost with nothing in the logs pointing at size.
	// A 413 + ERROR names the real problem (an event shape larger than the
	// cap) so the operator raises maxWebhookBodySize instead of chasing a
	// phantom signing-secret issue.
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBodySize+1))
	if err != nil {
		respond.BadRequest(w, r, "failed to read body")
		return
	}
	if len(body) > maxWebhookBodySize {
		slog.ErrorContext(r.Context(), "stripe webhook body exceeds the size cap — Stripe will retry ~3 days then drop the event permanently; raise maxWebhookBodySize if this event type is legitimate",
			"endpoint_id", endpointID,
			"tenant_id", lookup.TenantID,
			"cap_bytes", maxWebhookBodySize,
		)
		respond.Error(w, r, http.StatusRequestEntityTooLarge, "invalid_request_error", "payload_too_large",
			"webhook body exceeds the size cap")
		return
	}

	// Signature check is stripe-go's own (webhook.ValidatePayload, 5-minute
	// DefaultTolerance). Its tolerance is ONE-SIDED: only signatures OLDER than
	// the window are rejected. The hand-rolled check this replaced was
	// two-sided, so a host clock running >5 min behind Stripe rejected every
	// delivery — a silent total inbound blackout (Stripe retries ≤3 days, then
	// drops). Stripe's reference semantics are the contract.
	sigHeader := r.Header.Get("Stripe-Signature")
	if err := webhook.ValidatePayload(body, sigHeader, lookup.WebhookSecret); err != nil {
		slog.WarnContext(r.Context(), "stripe webhook signature verification failed",
			"endpoint_id", endpointID,
			"tenant_id", lookup.TenantID,
			"reason", err.Error(),
		)
		respond.BadRequest(w, r, "invalid signature")
		return
	}

	// Parse the event
	var raw struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Created  int64  `json:"created"`
		Livemode bool   `json:"livemode"`
		Data     struct {
			Object json.RawMessage `json:"object"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		respond.BadRequest(w, r, "invalid JSON")
		return
	}

	// The credentials row's livemode is authoritative — it's what the secret
	// was registered against. A tenant can't cross the streams by forging a
	// payload livemode because the HMAC above already rejected anything not
	// signed by the mode-specific secret.
	if raw.Livemode != lookup.Livemode {
		slog.WarnContext(r.Context(), "stripe webhook livemode mismatch",
			"endpoint_id", endpointID,
			"tenant_id", lookup.TenantID,
			"endpoint_mode", lookup.Livemode,
			"payload_mode", raw.Livemode,
			"stripe_event_id", raw.ID,
		)
		respond.BadRequest(w, r, "livemode mismatch")
		return
	}

	// Extract payment intent details from the event object
	var obj struct {
		ID        string `json:"id"`
		Object    string `json:"object"`
		Status    string `json:"status"`
		Amount    int64  `json:"amount"`
		Currency  string `json:"currency"`
		Customer  string `json:"customer"`
		LastError *struct {
			Message string `json:"message"`
		} `json:"last_payment_error"`
		Metadata map[string]string `json:"metadata"`
	}
	_ = json.Unmarshal(raw.Data.Object, &obj)

	// Defense in depth: velox_tenant_id in metadata should match the
	// endpoint's tenant. If they disagree, someone's Stripe account is
	// configured with the wrong endpoint — reject so we don't write one
	// tenant's webhook into another's invoice rows.
	metaTenant := obj.Metadata["velox_tenant_id"]
	if metaTenant != "" && metaTenant != lookup.TenantID {
		slog.WarnContext(r.Context(), "stripe webhook tenant mismatch",
			"endpoint_id", endpointID,
			"endpoint_tenant", lookup.TenantID,
			"metadata_tenant", metaTenant,
			"stripe_event_id", raw.ID,
		)
		respond.BadRequest(w, r, "tenant mismatch")
		return
	}

	if metaTenant == "" && !endpointAuthoritativeEvent(raw.Type) {
		// Not a Velox-originated payment intent (tenant fires their own
		// Stripe objects at the same endpoint). Acknowledge and skip.
		//
		// Exception: a few event types are recognized as Velox-originated from
		// the object itself, not a metadata marker — Stripe Checkout doesn't
		// copy a session's metadata onto the SetupIntent it creates, so a
		// Velox setup_intent.succeeded legitimately arrives with empty
		// metadata. Dropping it here silently loses the saved card; the
		// handler instead maps the SetupIntent's `customer` back to the Velox
		// customer. The endpoint's tenant is already authoritative (per-tenant
		// secret + HMAC), and the cross-tenant mismatch guard above still runs.
		respond.JSON(w, r, http.StatusOK, map[string]string{"status": "skipped", "reason": "no velox metadata"})
		return
	}

	// Scrub Stripe's free-text failure message before it enters Velox data.
	// Persisted to stripe_webhook_events.failure_message and echoed into
	// invoices.last_payment_error via handlePaymentFailed — scrubbing at
	// ingress keeps PII (card last4, emails) out of both.
	failureMsg := ""
	if obj.LastError != nil {
		failureMsg = errs.Scrub(obj.LastError.Message)
	}

	amount := obj.Amount
	event := domain.StripeWebhookEvent{
		StripeEventID:      raw.ID,
		EventType:          raw.Type,
		ObjectType:         obj.Object,
		PaymentIntentID:    obj.ID,
		PaymentStatus:      obj.Status,
		AmountCents:        &amount,
		Currency:           obj.Currency,
		CustomerExternalID: obj.Customer,
		FailureMessage:     failureMsg,
		InvoiceID:          obj.Metadata["velox_invoice_id"],
		Payload:            map[string]any{"raw": string(body)},
		OccurredAt:         time.Unix(raw.Created, 0).UTC(),
		Livemode:           lookup.Livemode,
	}

	// Stage ctx with the endpoint's tenant + livemode before handing off —
	// adapter DB writes read ctx tenant/livemode to satisfy RLS policies.
	ctx := auth.WithTenantID(r.Context(), lookup.TenantID)
	ctx = postgres.WithLivemode(ctx, lookup.Livemode)

	if err := h.stripe.HandleWebhook(ctx, lookup.TenantID, event); err != nil {
		slog.ErrorContext(ctx, "webhook processing failed",
			"stripe_event_id", raw.ID,
			"event_type", raw.Type,
			"tenant_id", lookup.TenantID,
			"error", err,
		)
		// A returned error is transient by construction: HandleWebhook
		// already ack's (returns nil) any event it can't ever process —
		// e.g. one referencing an invoice not in our DB — after recording
		// it for audit. So the only errors that reach here are retryable
		// (DB write failure, contention, downstream timeout), and crucially
		// NO dedup row was persisted for them. Respond 5xx so Stripe
		// redelivers and the handler re-runs; returning 200 here would let
		// a transient blip silently drop the event forever. Don't echo the
		// raw error to Stripe (may leak internal details).
		respond.InternalError(w, r)
		return
	}

	respond.JSON(w, r, http.StatusOK, map[string]string{"status": "processed"})
}

// endpointAuthoritativeEvent reports whether an event type may be processed
// using only the endpoint's authoritative tenant + the object's own
// references, without a velox_tenant_id metadata marker. Stripe Checkout
// doesn't copy a session's metadata onto the SetupIntent it creates, so a
// Velox-originated setup_intent.* arrives with empty metadata; these are
// recognized downstream by mapping the SetupIntent's `customer` back to the
// Velox customer (setup_failed is a no-op log). Kept deliberately narrow —
// payment_intent.* still requires the metadata marker so a tenant's own
// unrelated charges at the same endpoint aren't ingested.
func endpointAuthoritativeEvent(eventType string) bool {
	switch eventType {
	case "setup_intent.succeeded", "setup_intent.setup_failed":
		return true
	case "charge.refund.updated", "refund.updated", "refund.failed":
		// Refund objects carry no velox_* metadata (we create them with only an
		// idempotency key), so without admitting them here they'd be silently
		// skipped and the async-refund-status reconciliation would never run. The
		// endpoint tenant is authoritative (per-tenant secret + HMAC); the handler
		// still matches the refund to a Velox credit note by stripe_refund_id and
		// acks anything foreign.
		return true
	default:
		return false
	}
}
