# API surface (selected)

This page is a short list of the core API routes. The authoritative reference
is [`api/openapi.yaml`](../api/openapi.yaml). It covers the core resource
routes. Some operational routes (exports, analytics, audit log, settings)
aren't in the spec yet.

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
