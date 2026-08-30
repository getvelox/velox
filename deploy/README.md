# Velox Deployment

One install shape ships in this directory. The canonical landing page is
[`docs/self-host.md`](../docs/self-host.md).

| Path | Use when |
|---|---|
| [`compose/`](compose/) | Single-VM eval, dev/staging, low-volume production. Reference deploy. |

Kubernetes (Helm) and Terraform paths are deliberately not shipped in
v1 — they land when a design partner (an early production adopter)
names the flavour they actually run (see the "Operational posture"
section of `docs/self-host.md`). Pre-emptively shipping three
deployment paths produced surface area nobody was running.

## Local development

```bash
docker compose up -d postgres
DATABASE_URL="postgres://velox:velox@localhost:5432/velox?sslmode=disable" go run ./cmd/velox-bootstrap
DATABASE_URL="postgres://velox:velox@localhost:5432/velox?sslmode=disable" RUN_MIGRATIONS_ON_BOOT=true go run ./cmd/velox
```

Or run the whole stack in Docker:

```bash
docker build -t velox:local .        # from the repo root
cd deploy/compose
cp .env.example .env
$EDITOR .env   # set POSTGRES_PASSWORD, VELOX_APP_DB_PASSWORD, VELOX_ENCRYPTION_KEY, VELOX_BOOTSTRAP_TOKEN
VELOX_IMAGE=velox:local docker compose up -d
```

## Building the Docker image

```bash
docker build -t velox:latest .
docker run --rm -e DATABASE_URL="..." -p 8080:8080 velox:latest
```

## Required env vars

The compose-level schema (authoritative for the compose stack) is
[`compose/.env.example`](compose/.env.example). The full binary schema
is the repo-root [`.env.example`](../.env.example); it mirrors the
binary's actual reads from `internal/config/config.go` plus the
per-package `os.Getenv` callsites. Mandatory in a production
install:

| Variable | Why |
|---|---|
| `POSTGRES_PASSWORD` | Admin/migration role credentials (compose builds `DATABASE_URL` from it). |
| `VELOX_APP_DB_PASSWORD` | Password for the least-privilege `velox_app` runtime role (compose builds `APP_DATABASE_URL` from it). Production refuses to boot with the default `velox_app` password. |
| `VELOX_ENCRYPTION_KEY` | 64 hex chars (32 bytes). Production refuses to start without it (`config.validateFatal`). |
| `VELOX_BOOTSTRAP_TOKEN` | Authorises the one-shot `POST /v1/bootstrap` that creates the first tenant. |

Per-tenant Stripe API keys live in the database (see migration 0032),
not in env vars. The optional `STRIPE_WEBHOOK_SECRET` env var only
gates inbound Stripe webhook signature verification.

## Health checks

| Endpoint | Purpose |
|---|---|
| `GET /health` | Liveness — returns 200 if the process is running. |
| `GET /health/ready` | Readiness — returns 200 if DB is reachable AND the scheduler is healthy. |

Wire your load balancer / ingress / probes to `/health/ready`. Both
endpoints are exempt from rate limiting and audit logging.

## Scaling

- **Horizontal:** Multi-replica is safe on the money paths. Schedulers
  and outbox dispatchers (the workers that drain the queued
  side-effect tables — outbound webhooks and emails) are
  leader-elected: only one replica runs each job at a time, guarded
  by Postgres advisory locks (`internal/billing/lock_adapter.go`)
  and SKIP-LOCKED row claims (rows another worker already holds are
  skipped, not waited on), so replicas coexist without
  double-billing or double-sending. "Safe"
  is not "fully supported": a few surfaces still assume one process
  (the dashboard's live webhook-event tail only shows events dispatched
  by the replica serving the stream; the password-reset send cap
  becomes per-replica), and Postgres-failover edge cases are unhandled.
  The complete verified list — what breaks at N=2, what's already safe,
  and the scoped build plan — is
  [docs/dev/ha-readiness-2026-07-06.md](../docs/dev/ha-readiness-2026-07-06.md).
  As of 2026-08-30 N ≥ 2 behind a load balancer is the supported
  production posture and the remaining items are being built (see the
  HA-readiness doc's dated header). The reference compose stack stays
  single-replica for evaluation.
- **Rolling deploys and shutdown:** on SIGTERM a replica first flips
  `/health/ready` to `503 {"status":"draining"}` (liveness `/health` stays
  200) and keeps listening for `SHUTDOWN_DRAIN_DELAY` (default 5s outside
  local) so a health-check-driven balancer stops routing to it; then it
  drains in three bounded stages — in-flight HTTP (≤ 30s; the dashboard's
  live webhook tail is closed first so it cannot pin this stage), an
  in-flight test-clock advance (≤ 30s, then abandoned and resumed at the
  next boot), background workers (≤ 30s). Worst case 90s, typically under
  2s. **Set the orchestrator's grace period to 120s** (Kubernetes
  `terminationGracePeriodSeconds: 120`; compose `stop_grace_period: 120s`,
  already set). Shorter grace SIGKILLs mid-drain, which is designed-for
  (claimed outbox rows resume via their leases; a killed catch-up resumes at
  boot) but costs bounded at-least-once duplicates.
- **Database:** Velox uses connection pooling (`DB_MAX_OPEN_CONNS`,
  default 20). When scaling replicas, ensure total connections across
  all instances don't exceed your PostgreSQL `max_connections`.
- **Migrations:** Only one instance should run migrations per rollout.
  `RUN_MIGRATIONS_ON_BOOT=true` is safe under races (appliers serialize
  on an advisory lock and re-check applied state under it), but a
  dedicated migration step before rollout is still the cleaner shape.
