# Architecture — package layout

One Go binary, one package per domain. Each domain owns its store, service,
and handler; the design rules this layout enforces are in the README's
[Architecture](../README.md#architecture) section, and the load-bearing
decisions live in the [ADRs](adr/).

```
cmd/velox/                  — single Go binary
cmd/velox-doctor/           — read-only money-invariant sweep (see Engineering)
internal/                     (abridged — one package per domain)
  domain/                   — pure domain models, zero deps
  auth/                     — API key auth (3 key types, 17 permissions)
  customer/                 — customer CRUD + billing profiles
  pricing/                  — meters, rating rules, plans, price overrides
  subscription/             — lifecycle (draft → trialing → active → paused → canceled)
  usage/                    — event ingestion + multi-dim aggregation
  invoice/                  — state machine (draft → finalized → paid) + PDF
  billing/                  — billing engine + scheduler + preview
  payment/                  — Stripe PaymentIntent + webhook receiver
  dunning/                  — payment retry state machine
  credit/                   — event-sourced prepaid balance ledger
  creditnote/               — credit notes + refunds
  webhook/                  — outbound webhooks (HMAC-signed delivery)
  audit/                    — immutable append-only audit log
  platform/postgres/        — RLS-aware database layer
  platform/migrate/         — embedded SQL migrations

web-v2/                     — operator dashboard (React 19 + TypeScript + Tailwind)
```
