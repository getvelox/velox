# API surface (selected)

This page is a short list of the core API routes. The authoritative reference
is [`api/openapi.yaml`](../api/openapi.yaml). It covers the core resource
routes. Some operational routes (exports, analytics, audit log, settings)
aren't in the spec yet.

Error responses (statuses, the error body, and the exceptions) are in
[Errors](#errors) at the end of this page.

If you consume webhooks, read [`webhooks.md`](webhooks.md). It covers
signature verification, the envelope, the retry ladder, the event catalog and
the delivery contract.

## Usage ingest routes

```
POST   /v1/usage-events                 — ingest with dimensions + decimal value
POST   /v1/usage-events/batch           — batch ingest, up to 1000 per call
```

### Ingest retry contract

A non-2xx response means *nothing was recorded*. Retry the same request.

- **Replays are dedup-safe.** A batch commits all-or-nothing. Every event's
  `idempotency_key` lands in the database as `ON CONFLICT DO NOTHING`.
  Together, these mean that sending twice can never double-count.
- **Per-event key.** For at-least-once pipelines, an `idempotency_key` per
  event is strongly recommended. See *idempotency key* in the
  [glossary](README.md#pricing-and-usage).
- **`Idempotency-Key` HTTP header.** On ingest routes, a 5xx never pins the
  header's key. That is, Velox does not keep the 5xx to replay for later
  requests with that key. Retry with the same key and the write re-executes.
  On other routes, a 5xx response is replayed on purpose.
- **Late events.** Events that arrive more than 24 h late may land in an
  already-finalized period. They are stored, never silently billed, and
  counted on `velox_usage_late_event_total`. Runbook: manual credit/debit.

## Other core routes

```
POST   /v1/meters/{id}/pricing-rules    — add a dimension-matched pricing rule
GET    /v1/customers/{id}/usage         — period aggregation, grouped by dimension

POST   /v1/customers                    — create customer
POST   /v1/subscriptions                — create subscription
PATCH  /v1/subscriptions/{id}/items/{itemID}     — plan/quantity change (proration on immediate)
PUT    /v1/subscriptions/{id}/pause-collection   — keep cycle, invoice as draft
POST   /v1/subscriptions/{id}/extend-trial       — push trial_end_at later

POST   /v1/billing/run                  — finalize all due cycles
GET    /v1/billing/preview/{sub_id}     — invoice preview (dry run)

POST   /v1/credit-notes                 — issue credit note (credit or refund)
POST   /v1/credits/grant                — grant prepaid credits to a customer
GET    /v1/credits/balance/{customer_id} — current balance + ledger

GET    /v1/dunning/runs                 — list dunning runs
POST   /v1/webhook-endpoints/endpoints  — register an outbound webhook endpoint
GET    /v1/audit-log                    — query the append-only audit log
```

## Errors

Most API errors return one JSON envelope with the same fields. Every response that reaches the Velox router also carries the `Velox-Request-Id` header. Send that id to support when you report a problem.

```json
{
  "error": {
    "type": "invalid_request_error",
    "code": "validation_error",
    "message": "external_id is required",
    "request_id": "req_<id>",
    "param": "external_id"
  }
}
```

`type`, `code`, `message` and `request_id` are always present in the envelope. When `param` is present, it names the field that failed. Read `code` in your code; `message` is meant for people. Some endpoints put a more specific code in `code` (for example `clock_advancing`, `not_payable`, `cooldown_active`) in place of the default code shown below.

| Status | type | When |
|---|---|---|
| 400 | `invalid_request_error` | The request is malformed: the body is not valid JSON, or a required path or query parameter is missing or invalid (`invalid_request`). |
| 401 | `authentication_error` | The API key, session or public token is missing, invalid or expired. |
| 403 | `authentication_error` | You are signed in but not allowed to do this, or the request was blocked as cross-site. |
| 404 | `invalid_request_error` | The resource does not exist (`not_found`, or a specific code such as `invoice_not_found`). |
| 405 | (empty body) | The path exists but does not accept this method. The `Allow` header lists the methods it does accept. |
| 409 | `invalid_request_error` | The value already exists (`already_exists`), or the resource is in the wrong state for this action (`invalid_state` or a more specific code). |
| 413 | `invalid_request_error` | The usage batch (`batch_too_large`, only when the request has no `Idempotency-Key`) or the Stripe webhook body (`payload_too_large`) is too large. |
| 422 | `invalid_request_error` | A field or business rule failed (`validation_error`, or a specific code). |
| 429 | `rate_limit_error` | Too many requests (`rate_limited`, or `cooldown_active` for a repeated setup-link email). Wait for the number of seconds in `Retry-After`. |
| 500 | `api_error` | Unexpected server error (`internal_error`). The details are logged on the server, not returned. A few operations return a specific code with instructions instead, for example `proration_failed`. |
| 502 | `api_error` | An upstream call failed: the payment provider rejected the request (`provider_error`, `stripe_error`), or an email could not be queued (`email_enqueue_failed`). |
| 503 | `api_error` | A dependency is down for a short time (`authentication_unavailable`, `ingest_unavailable`), so you can retry. `stripe_unavailable` and `email_unavailable` mean the feature is not configured on this deployment, and retrying will not help. |

Request bodies are limited to 1 MB; Stripe webhook bodies are limited to 64 KB. Only the two routes listed in the 413 row return 413. Other routes do not promise a specific status for a body that is too large. If an oversized usage batch is sent with an `Idempotency-Key` header, it gets 400 with `type` `bad_request` in the smaller idempotency body (see Exceptions), not 413.

### Exceptions

- **Idempotency-Key errors** return a smaller body, `{"error": {"type", "message"}}`. It has no `code` and no `request_id` in the body; the header still has the id. Here `type` is the reason:
  - `idempotency_error` (422): the key was used before with a different method, path or body. This is checked only after the first request has finished. While the first request is still running, a mismatched request gets `conflict_idempotency`.
  - `conflict_idempotency` (409): another request with this key is still running, or did not finish. Retry later.
  - `conflict_idempotency_unresolved` (409): an earlier request with this key started more than 5 minutes ago and never recorded an outcome. Check whether the resource exists before you retry with a new key.
  - `bad_request` (400): the body could not be read. This includes any body over 1 MB, so an oversized usage batch sent with an Idempotency-Key gets this 400 instead of the 413 `batch_too_large`.
- **A replayed key** (POST, PUT or PATCH under `/v1` with the same key, method, path and body) returns the first response's status and body, plus `Idempotent-Replayed: true`. The body's `request_id` is the first request's; the `Velox-Request-Id` header is the new request's. The response's other headers are not sent again. A first response of 409 or 422 is not stored, and neither is a 5xx on the usage-ingest routes (`/v1/usage-events`, `/v1/integrations/litellm`). In those cases the key is freed, and a retry with the same key runs the request again.
- **Plain-text or empty bodies (not JSON)** come from these cases:
  - an unknown path (`404 page not found`);
  - a method the path does not accept (405, empty body);
  - a server panic (500, empty body);
  - a request that hits its time limit before the server has written any response (504, empty body);
  - `GET /v1/webhook_events/stream` when it returns 500;
  - `/metrics` when it returns 401;
  - a request too malformed for the server to parse, such as bad syntax or headers that are too large (400 or 431 from the HTTP server itself, with no `Velox-Request-Id` header).
- **Other JSON shapes:**
  - `GET /health/ready` returns 503 with `{"status", "checks"}`.
  - `GET /v1/usage-summary/{customer_id}` returns 500 with `{"error": "internal_error"}`.
  - A rejected usage batch returns 422 with code `batch_rejected`. Its `error` object has no `request_id`, and the body also has `errors` (one entry per failing event), `ingested: 0` and `total`.
  - `GET /v1/auth/password-reset/check` returns 422 with `{"valid": false, "reason": "invalid_or_expired"}`.
  - The Postmark webhook returns `{"error": "<reason>"}` with 401 or 500. A payload that is too large or is not valid JSON is accepted with 200 and `{"status": "skipped", "reason": "<reason>"}`.
- **Known inconsistencies:** some invoice actions return 409 with `type` `invalid_state`, and the setup-link email returns 422 with `type` `invalid_state`. On `POST /v1/subscriptions/{id}/extend-trial` and on `PUT /v1/subscriptions/{id}/billing-thresholds` for a multi-currency subscription, the fields are shifted. `type` is `invalid_body` (400) or `validation_error` (422). `code` holds the message text. `message` holds the field name (`trial_end`, `billing_thresholds`) or is empty.
