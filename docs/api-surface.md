# API surface (selected)

A quick orientation to the core routes. The authoritative reference is
[`api/openapi.yaml`](../api/openapi.yaml) — it covers the core resource
routes; some operational routes (exports, analytics, audit log, settings)
aren't in the spec yet. Webhook consumers: [`webhooks.md`](webhooks.md) —
signature verification, envelope, retry ladder, event catalog, delivery
contract.

```
POST   /v1/usage-events                 — ingest with dimensions + decimal value
POST   /v1/usage-events/batch           — batch ingest, up to 1000 per call
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
