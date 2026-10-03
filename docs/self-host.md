# Velox — Self-host

**What this page is:** the operator's reference for running Velox yourself.
It covers the deployment shape, Postgres requirements, environment
variables, scaling and observability. **Who it's for:** an engineer who
sets up or operates an install, whether new to this repo or checking one
flag mid-deploy.

Velox runs as a single Go binary against Postgres. It has two deployment
shapes:

- **Docker Compose on a single VM.** This is the reference and evaluation
  install, described first below.
- **The production posture: N ≥ 2 replicas behind a load balancer, on
  managed Postgres and Redis.** This was decided 2026-08-30; see
  [Operational posture](#operational-posture) below.

Only the packaging for the second shape (Helm chart, Terraform) is
deferred. It waits until a [design partner](README.md#glossary) (an early
customer who works with the team) names the platform flavour they run.

## Deploying (single-VM compose stack)

**The canonical walkthrough is
[`deploy/compose/README.md`](../deploy/compose/README.md).** It describes a
containerized five-service stack (postgres, redis, velox-api, velox-dashboard, nginx)
with its own `.env.example`. It takes five minutes from a fresh VM to a
working [tenant](README.md#glossary). A tenant is Velox's unit of
isolation: one business account with its own data, dashboard users and API
keys. One install can serve several tenants.

The steps:

1. Set four secrets.
2. Run `docker compose up -d`.
3. Make one `POST /v1/bootstrap` call. It returns your dashboard owner login
   and API keys (test + live).

Everything below on this page is reference material that applies to both
the compose stack and a hand-rolled install. It covers Postgres
requirements, env vars, scaling and observability.

## Local development (host Go toolchain, not a deployment)

```bash
git clone https://github.com/getvelox/velox.git
cd velox

cp .env.example .env   # make dev reads it; local defaults work as-is
docker compose up -d postgres redis mailpit
VELOX_BOOTSTRAP_EMAIL=you@example.com VELOX_BOOTSTRAP_PASSWORD=change-me-please1 \
  make bootstrap
make dev
```

`VELOX_BOOTSTRAP_EMAIL` and `VELOX_BOOTSTRAP_PASSWORD` are optional. Without
them, bootstrap defaults the owner to `admin@velox.local` and prints a
generated password. Passwords must be at least 12 characters.

That gives you:

- `postgres` on `:5432` (volume-backed, password `velox`)
- `redis` on `:6379` (used by the rate limiter).
  - **Required in production.** The general and hosted-invoice limiters fail
    closed without it. So `APP_ENV=production` refuses to boot unless
    `REDIS_URL` points at a reachable Redis (2026-08-30).
  - Outside production it is optional, and the limiters fail open.
- `mailpit` on `:1025` SMTP / `:8025` web UI (catches outbound transactional mail)
- `velox-api` on `:8080` (from `make dev`, not a container)

The dashboard:

```bash
cd web-v2 && npm install && npm run dev
# → http://localhost:5173
```

## Operational posture

This deployment shape is a **single-VM, single-instance** install:

- API: 1 `velox-api` process. Restart and you have downtime until it
  comes back up.
- DB: 1 Postgres instance on the same host (or a managed Postgres if you
  point `DATABASE_URL` elsewhere).
- Scheduler: an in-process goroutine inside `velox-api` (ADR-006). ADRs,
  the numbered architecture decision records, live in `docs/adr/`.
  - The scheduler and the outbox dispatchers are leader-elected per tick
    through a Postgres lease row (ADR-114). On a second replica, they stand
    by instead of running the same work twice. So an accidental N=2
    is correctness-safe on the money paths.
  - Outbox dispatchers are the workers that drain the
    [outbox](README.md#glossary) tables. Side effects such as outbound
    webhook deliveries are enqueued there in the same transaction as the
    state change that caused them.
  - A few non-money surfaces still assume one process (SSE live tail,
    per-process throttles). The full list, with evidence, is in
    [docs/dev/ha-readiness-2026-07-06.md](dev/ha-readiness-2026-07-06.md).
- LB: none.
- Shutdown: SIGTERM → readiness 503 `draining` → three bounded stages, 90s
  worst case. Give the process 120s of grace. See
  [deploy/README.md](../deploy/README.md) "Rolling deploys and shutdown".

This shape is appropriate for development, evaluation, and single-tenant
self-hosting where ~minutes of downtime per deploy/restart is acceptable.
It is **not** a production-with-availability shape.

The supported production posture (decided 2026-08-30) is a load-balanced
multi-replica deployment. The engine is being designed for it:

- N ≥ 2 `velox-api` behind a load balancer.
- Managed Postgres with failover + PITR.
- Managed Redis.

Much of the groundwork already exists: leader-elected scheduling,
SKIP-LOCKED outbox claims, and DB-backed sessions/idempotency. The remaining
work list and its status live in the HA-readiness doc above.

Leadership is a per-tick lease row (ADR-114, shipped 2026-08-30). So
PgBouncer transaction mode and RDS Proxy work for the server. Migrations
still want a direct or session-mode connection.

Packaging (Helm chart, Terraform module) stays deferred until a design
partner names the platform flavour. The architecture no longer waits for
that.

## Postgres

Compose ships Postgres 16 with default settings — sufficient for eval.
For your own VM:

- Version: 16.x
- Extensions: `pgcrypto` (provides `gen_random_bytes`) and `citext`.
  Migrations run `CREATE EXTENSION IF NOT EXISTS`, so the migrating role
  needs the `CREATE EXTENSION` permission. On locked-down managed
  Postgres, pre-create them as
  [`docs/ops/postgres-requirements.md`](ops/postgres-requirements.md)
  describes.
- **A least-privilege runtime role (required for tenant isolation).**
  Velox enforces multi-tenant isolation with Row-Level Security (RLS).
  Request traffic runs on the connection in `APP_DATABASE_URL`. That is a
  role like `velox_app` with its own password, not the admin role. The
  compose stack creates it from `VELOX_APP_DB_PASSWORD`
  ([`deploy/compose/postgres-init.sh`](../deploy/compose/postgres-init.sh));
  on your own Postgres:

  ```sql
  -- use psql -v pw='...' and :'pw' quoting, or substitute a literal
  CREATE ROLE velox_app WITH LOGIN PASSWORD :'pw';
  GRANT ALL PRIVILEGES ON DATABASE velox TO velox_app;
  GRANT ALL ON SCHEMA public TO velox_app;
  ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON TABLES TO velox_app;
  ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON SEQUENCES TO velox_app;
  ```

  **With `APP_ENV=staging` or `production`, Velox refuses to start**
  (ADR-073) when `APP_DATABASE_URL`:

  - is missing,
  - carries the default password `velox_app` (or an empty one),
  - can't be opened, or
  - points at a role that can bypass RLS (superuser/`BYPASSRLS`). This
    boot check catches a copied `DATABASE_URL`.

  In `local` it derives `velox_app:velox_app` from `DATABASE_URL` and warns
  instead, since a single-tenant dev box often uses one superuser URL.
- Backups: take a `pg_dump` snapshot on whatever cadence your data loss
  tolerance allows. Velox's outbound webhook outbox and audit log are the
  two surfaces where lost rows are most expensive. A consistent snapshot
  covers both.

## Migrations

`RUN_MIGRATIONS_ON_BOOT=true` (set in `.env.example`, which `make dev` loads as `.env`; the compose stack also sets it) runs forward
migrations on startup. Migrations are versioned and idempotent
([`internal/platform/migrate/sql/`](../internal/platform/migrate/sql/)).
Down-migrations exist for development reversal, but production rollbacks
are forward-only. To recover, write a new forward migration; do not run
the downs.

## Environment

Required:

| Var | Purpose |
|---|---|
| `DATABASE_URL` | Postgres DSN — admin/migration role |
| `APP_DATABASE_URL` | Postgres DSN — least-privilege `velox_app` runtime role (RLS enforced). Required in `staging`/`production`; local dev derives `velox_app:velox_app` from `DATABASE_URL` when unset |

Bootstrap-time (read by `make bootstrap` / `cmd/velox-bootstrap`, not the server):

| Var | Purpose |
|---|---|
| `VELOX_BOOTSTRAP_EMAIL` | Dashboard owner email (default `admin@velox.local`) |
| `VELOX_BOOTSTRAP_PASSWORD` | Owner password (unset → generated and printed once) |
| `VELOX_BOOTSTRAP_TENANT` | Tenant name (default `Demo Tenant`) |

Optional:

| Var | Default | Purpose |
|---|---|---|
| `RUN_MIGRATIONS_ON_BOOT` | `false` | Run migrations on startup (racing replicas serialize on an advisory lock and skip already-applied work) |
| `APP_ENV` | `local` | `local`/`staging`/`production`. Gates the cookie `Secure` flag and the fail-closed boot checks — `staging`/`production` refuse to start without a valid `APP_DATABASE_URL` (see Postgres above) and refuse a `VELOX_BOOTSTRAP_TOKEN` under 16 chars |
| `TRUST_PROXY` | _(unset)_ — **required in production** | Comma-separated proxy IPs/CIDRs whose `X-Forwarded-For`/`X-Real-IP` are trusted for client-IP resolution (rate limiting, audit logs). Unset = headers ignored, direct TCP peer used. `none` declares there is no proxy. Production refuses to boot with it unset (every per-IP limit would be one bucket behind a balancer). |
| `DASHBOARD_BASE_URL` | _(unset)_ | Canonical dashboard origin for password-reset links. **Unset disables password-reset emails** — the origin is never derived from request headers (host-header poisoning). Set to e.g. `http://localhost:5173` in dev |
| `SMTP_HOST` / `SMTP_PORT` | _(unset)_ | Outbound email relay. Unset → emails are not sent (`ErrSMTPNotConfigured`). The compose path points these at mailpit (`localhost:1025`) |

Stripe is configured per-tenant via the dashboard (`POST /v1/settings/stripe`), not env vars — each tenant connects their own Stripe account.

## Scaling considerations

Measured on AWS, in usage events ingested per second (ev/s), with every
event reconciled against Postgres:

- **1,000 ev/s sustained** on a `db.m7g.2xlarge` (batch 10, five
  10-minute repeats, 5 of 5 passed).
- **15,000 ev/s** on a `db.m7g.4xlarge` (batch 100, 4 of 5 repeats).

The limit was database RAM first and write IOPS after that, never the app
process. On the 4xlarge, RDS CPU stayed at or below 67 % and the app node
was 75 %+ idle. Full method and every caveat:
[docs/benchmarks/sustained-throughput.md](benchmarks/sustained-throughput.md).

On one small VM expect far less. The levers, in order:

1. The batch endpoint. One commit amortises the write cost across up to
   1,000 events.
2. Database RAM.
3. Provisioned IOPS.
4. Month-partitioning `usage_events`. Migration 0130 already ships the
   `(tenant_id, timestamp)` index.

## Observability

Velox exposes Prometheus metrics on `/metrics` and structured logs to
stdout. Connect these to whatever stack you already run. The v1 install
does not ship a Grafana / Prometheus / Alertmanager bundle; that is
deferred. Local dev observability is `tail -f` on the API logs.

Key metrics to watch:

- `velox_billing_cycle_duration_seconds` — cycle scan latency
- `velox_tax_outcome_total{outcome,reason}` — tax-provider failure modes
- `velox_audit_write_errors_total` — audit log write failures
- `velox_audit_uncovered_mutation_total{route}` — a request mutated state and left NO audit row (should be flat zero; see the runbook, `docs/ops/runbook.md`)
- `velox_stripe_breaker_state` — Stripe API circuit breaker (0 = closed, 1 = half-open, 2 = open)

## Related

- [docs/ops/tax-calculation.md](./ops/tax-calculation.md) — tax
  providers and their failure handling
- [docs/ops/stripe-end-to-end-test.md](./ops/stripe-end-to-end-test.md) —
  manual end-to-end Stripe smoke test
- [docs/adr/](./adr/) — architecture decisions worth knowing about
  (PaymentIntent-only, RLS multi-tenancy, in-process scheduler)
