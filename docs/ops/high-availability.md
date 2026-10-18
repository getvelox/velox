# Run two or more copies of Velox (high availability)

This page is for the engineer who runs Velox where a restart must not cause downtime. You run two or more copies of the Velox API behind a load balancer. While one copy restarts or crashes, the others keep serving.

Think of a shop with two tills: when one closes, customers queue at the other.

One VM with Docker Compose is also a supported production setup, for a small team that can accept a few minutes of downtime during a restart. If that is you, start with [Install on one VM](../../deploy/compose/README.md) instead.

## What you need

- Two or more `velox-api` copies behind a load balancer.
- A managed Postgres with automatic failover and point-in-time restore.
- A managed Redis. Velox uses it only for rate limiting.
- A connection pooler, if you want one. PgBouncer in transaction mode works, and CI runs the test suite through it. RDS Proxy is expected to behave the same way but has not been tested ([details](postgres-requirements.md)). Run database upgrades (migrations) over a direct connection.

The settings for each copy are in [Self-host](../self-host.md#settings). Every copy uses the same settings and the same database.

## How the copies share the work

Each copy answers API requests. Background jobs are different: billing runs, payment retries (dunning), and sending webhooks and emails must run once, not once per copy. Two things make sure of that, so copies do not bill or send twice:

- **One copy runs each job at a time.** Before each run of a job, a copy takes a short lease: a row in Postgres that says "this copy runs this job now" (see [leader lease](../glossary.md)). The other copies stand by. If the copy holding a lease dies, another one takes over within about 15 seconds.
- **Queued rows are claimed once.** Webhooks and emails wait in queue tables (outboxes) until a background worker sends them. A worker skips any row another worker already holds, instead of waiting for it (Postgres `SKIP LOCKED`).

One feature still assumes a single copy. The dashboard's live webhook event stream shows only the events sent by the copy that serves the stream. It does not touch money.

## Set up the load balancer

1. Point the load balancer's health check at `GET /health/ready`.

   ```bash
   curl -fsS https://your-velox-host/health/ready
   ```

   **You see:** `{"checks":{"api":"ok","database":"ok","scheduler":"..."},"status":"ok"}`, with status 200.

2. Set `TRUST_PROXY` on every copy to the address range (CIDR) of the load balancer. Separate several ranges with commas.

   Without it, every request seems to come from the load balancer. The per-address limits on the public payment pages (60 a minute) and on sign-in, `/v1/auth` (100 a minute), then become one shared limit for every customer and every operator of the install. Set `TRUST_PROXY=none` only when the copies take client connections directly, with no proxy in front.

   **You see:** the copy starts. In `production`, a copy with `TRUST_PROXY` unset refuses to start with `TRUST_PROXY is required in production`.

3. Set `SHUTDOWN_DRAIN_DELAY` to at least the load balancer's health-check interval times its unhealthy threshold. For an AWS ALB with its defaults, that is 30s × 2, so set `SHUTDOWN_DRAIN_DELAY=60s`. Use `0` when the orchestrator removes the copy from the load balancer itself, as Kubernetes does. The default is 5s outside `local`.

   **You see:** when you stop a copy, `/health/ready` on that copy answers 503 `{"status":"draining"}` for that long, while it still serves requests.

4. Give each copy 120 seconds to shut down: Kubernetes `terminationGracePeriodSeconds: 120`. The Compose stack already sets `stop_grace_period: 120s`.

5. Check the database connections. Each copy opens up to `DB_MAX_OPEN_CONNS` connections (default 20). Copies × `DB_MAX_OPEN_CONNS` must stay below your Postgres `max_connections`.

## Health checks

| Endpoint | What it tells you |
|---|---|
| `GET /health` | Liveness: 200 while the process runs. |
| `GET /health/ready` | Readiness: 200 when the database is reachable. 503 when it is not, or when the copy is shutting down. The body also reports the billing scheduler, which does not change the status code. |

Neither endpoint is rate limited or written to the audit log.

## What happens when a copy stops

On a stop signal (SIGTERM), a copy first tells the load balancer it is leaving: `/health/ready` answers 503 while the copy still accepts connections. Then it finishes its work in three stages, each with a time limit.

| Step | What happens | Time limit |
|---|---|---|
| Readiness drain | `/health/ready` answers `503 {"status":"draining"}`; `/health` stays 200. The copy keeps listening, so the load balancer has time to stop sending it requests. | `SHUTDOWN_DRAIN_DELAY` (default 5s outside `local`) |
| 1. Requests in progress | The dashboard's live webhook stream is closed first, so it cannot hold this stage open. | Up to 30s |
| 2. A test-clock advance in progress | Stopped at the limit. The clock stays `advancing` until a copy restarts. See [Test-clock advance hung](runbook/test-clock-advance-hung.md). | Up to 30s |
| 3. Background jobs | | Up to 30s |

The worst case is 90 seconds. Usually it takes under 2 seconds.

A grace period shorter than 120 seconds can kill a copy (SIGKILL) halfway through. Velox is built to survive that: queued webhooks and emails a copy had claimed are picked up again when its lease runs out, and the next copy to start recovers a killed test-clock advance. The cost is that a few webhooks or emails can be sent twice.

## Upgrades

Read the upgrade notes in the [changelog](../../CHANGELOG.md) first. Whether you can upgrade one copy at a time (a rolling upgrade) depends on the release's migrations, below.

**Database upgrades (migrations):** `RUN_MIGRATIONS_ON_BOOT=true` is safe when several copies start at once: they take turns through a Postgres lock, and each one applies only what is still missing. A waiting copy gives up after 30 minutes and is restarted. What is not covered is old code running on a new schema, so the release notes decide the order:

- **A release whose notes say it has migrations:** stop every old copy, run `velox migrate` once (or start one new copy with `RUN_MIGRATIONS_ON_BOOT=true`), then start the other new copies.
- **A release with no migrations:** upgrade one copy at a time.
- **From v0.8.x to v0.9.0:** run v0.9.0's `velox migrate` once first. It adds goose's record (`goose_db_version`, at version 180) and changes none of Velox's tables, so the v0.8.x copies keep serving while it runs. Then upgrade the copies one at a time. With `RUN_MIGRATIONS_ON_BOOT=false`, a v0.9.0 copy started before that step refuses to start, and its log says `1 migration to apply`.

Run `velox migrate status` with the new release first to see the plan. Never roll a migration back in production; see [Self-host](../self-host.md#database-upgrades-migrations).

### Upgrading from v0.3.2 or older

The upgrade from v0.3.2 or older to v0.4.0 or newer must stop every copy before it starts the new ones. Do this one upgrade this way, not rolling:

1. Apply the migrations first, as for any upgrade.
2. Stop every old copy. In Kubernetes, use the `Recreate` strategy, or `maxSurge: 0` with `maxUnavailable: 100%`.
3. Start the new copies.

**You see:** `/health/ready` answers 200 on every new copy.

Why: v0.4.0 changed how copies agree on who runs each job. An old copy and a new copy do not see each other's claim, so while both run, every background job runs twice. Money stays correct, because each write checks the state it changes, but webhook events can be sent twice.

## Related

- [Self-host](../self-host.md): settings, Postgres and monitoring.
- [A background job is stuck](runbook/background-job-stuck.md): what to do when a background job seems stuck.
- [HA readiness review](../records/ha-readiness-2026-07-06.md): the dated record of what was checked for two or more copies, and why.
