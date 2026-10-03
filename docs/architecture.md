# Architecture

One Go binary, one package per domain. Each domain owns its store, service,
and handler. The decisions behind this layout live in the [ADRs](adr/).

## Design rules

- **Per-domain packages.** Each domain owns its store, service, and handler. No peer domain touches another's internals. The imports that legitimately cross domains are listed edge by edge in an allowlist, enforced by an [architecture test](../internal/arch/boundaries_test.go).
- **Row-Level Security (RLS).** Every tenant-scoped query runs inside a transaction where Postgres enforces tenant isolation. Integration tests prove it.
- **PaymentIntent-only Stripe.** No Stripe Billing or Stripe Invoices. Velox owns invoices end to end; Stripe executes the card charge.
- **Billing engine as coordinator.** The engine orchestrates the domains through narrow interfaces. It is not one object that does everything.
- **Append-only records for money.** Credits, the audit log, and the outbound webhook outbox are written as append-only events.

## Package layout

```
cmd/velox/                  — single Go binary
cmd/velox-doctor/           — read-only money-invariant sweep (see ENGINEERING.md)
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
