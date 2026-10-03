# Postgres Requirements

This doc lists what Velox, a usage-based billing engine, needs from the
Postgres database it runs against. Operating Postgres itself
(replication, backups, failover, sizing) is out of scope. Assume your
DBA (database administrator) team or your managed-Postgres provider
handles that. This doc names what they need to know.

## Versions

- **Minimum: Postgres 14.** Velox relies on `gen_random_bytes` from
  the `pgcrypto` extension and standard SQL features through PG14.
- **Tested: Postgres 16.** The `docker-compose.yml` shipped with
  Velox uses `postgres:16-alpine` for local dev.
- **Recommended for production: Postgres 16+.** No PG17-specific
  features in use; safe to upgrade ahead.

## Required extensions

Both are standard `contrib` extensions — part of Postgres's own
extension collection, present in every managed Postgres (RDS, Cloud
SQL, Aurora, CloudNativePG, postgres-operator, self-managed
Debian/Ubuntu Postgres). Velox creates them via its database
migrations (versioned schema scripts); the DB role running migrations
needs `CREATE EXTENSION` permission on first install.

| Extension | Used for | Migration |
|---|---|---|
| `pgcrypto` | `gen_random_bytes()` for ID generation, `digest()` for hashing | `0001_schema.up.sql` |
| `citext` | Case-insensitive `email` column on `users` | `0069_user_password_auth.up.sql` |

If your environment forbids `CREATE EXTENSION` at runtime (some
managed Postgres setups), pre-create both extensions before running
migrations:

```sql
CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE EXTENSION IF NOT EXISTS citext;
```

## Required session settings (RLS)

Velox isolates tenants with Postgres Row-Level Security (RLS): the
database itself filters every tenant-scoped query down to that
tenant's rows. To drive the policies, the application sets two
per-transaction session variables (GUCs, Postgres's runtime
configuration parameters):

- `app.tenant_id` — set when running tenant-scoped work; RLS policies
  filter on `tenant_id = current_setting('app.tenant_id', true)`.
- `app.bypass_rls` — set to `'on'` for scheduler / reconciler paths
  (background jobs that legitimately span tenants); RLS policies fall
  through, i.e. do not filter.

Both GUCs must be allowed; on managed Postgres this is typically the
default, since namespaced custom GUCs (`app.*`) require no
configuration on stock Postgres. Some hardened deployments also set
`force_row_level_security`. That's compatible — Velox declares its
RLS policies with `FORCE ROW LEVEL SECURITY` on every tenant-scoped
table.

If your environment strips custom GUCs, ensure `app.tenant_id` and
`app.bypass_rls` are allowed.

## Connection pooling

Velox's connection-pool defaults (env-overridable):

| Setting | Default | When to tune |
|---|---|---|
| `DB_MAX_OPEN_CONNS` | 20 | Raise if you see connection-pool waits in metrics + Postgres has headroom (`max_connections` typically 100-200) |
| `DB_MAX_IDLE_CONNS` | 5 | Raise to match `MaxOpenConns` if you have spiky workload |
| `DB_CONN_MAX_LIFETIME_MIN` | 30 | Lower to 5-10 if running behind a pooler with a short `server_lifetime` |
| `DB_CONN_MAX_IDLE_TIME_SEC` | 120 | Idle-eviction; lower for tight-pool environments |

**Concurrent-connection profile**: at peak, Velox holds connections for:

- HTTP request handlers (one per in-flight request).
- Billing scheduler tick (one connection while running per-tenant
  cycles, briefly).
- Webhook outbox dispatcher — the worker draining the queue table of
  pending outbound webhooks (one connection per dispatch worker;
  default 1).
- Email outbox dispatcher (one connection; default 1).
- Dunning (failed-payment follow-up) policy ticks (per-tenant, brief).

For a single-tenant deployment running ~50 RPS (requests per second),
20 open connections is comfortable. For multi-tenant deployments,
scale linearly with tenant count up to your Postgres
`max_connections` ceiling. If you exceed ~100 application-side pool
size, use PgBouncer (a common Postgres connection pooler) or RDS Proxy
in front of Postgres. Session or transaction mode both work for the
server (see below). Migrations want a direct or session-mode
connection.

## PgBouncer / RDS Proxy compatibility

A pooler in *session mode* gives each client one dedicated server
connection for as long as the client stays connected. *Transaction
mode* hands each transaction to whichever server connection is free.

**The Velox server is safe behind PgBouncer in either mode, and behind a
direct connection.** Since ADR-114 every statement the server issues is
self-contained:

- Tenant isolation is set per transaction with `set_config(.., true)`,
  which reverts when the transaction ends.
- The singleton roles take their leadership from a row in
  `leader_leases`, not from a database session. A singleton role must
  run on exactly one replica at a time. The roles are billing, dunning,
  the two outbox dispatchers, and webhook delivery retry.

Before ADR-114, leadership was a session-scoped advisory lock, which a
transaction pooler silently strands. The boot-time topology probe that
guarded against that is retired with it.

The `test-pgbouncer` CI job runs the whole integration suite on every
push, with the app pool behind PgBouncer
1.24 in transaction mode. `scripts/pgbouncer-test.sh` runs the same
thing locally. So a statement that assumes a session cannot land
unnoticed.

### Prepared statements: the pgx floor

The Go driver (pgx) prepares and caches named statements on each
connection by default. A transaction-mode pooler must therefore track
protocol-level prepared statements. That needs **PgBouncer ≥
1.21 with `max_prepared_statements` > 0**. CI runs 200. The harness's
negative control at 0 fails two thirds of statements with `prepared
statement "stmtcache_…" does not exist`.

Behind an older PgBouncer, or a pooler without that feature, add
`default_query_exec_mode=cache_describe` to
`DATABASE_URL`/`APP_DATABASE_URL`. Then no named statements survive a
transaction, at a little more round-trip cost.

### RDS Proxy: safe, multiplexing unverified

Every tenant transaction issues `set_config`. AWS lists
`SET`/`set_config` as session-pinning triggers for PostgreSQL, with no
transaction-local carve-out. So expect the proxy to pin each client
connection to a backend for its lifetime.

That is correct, because pinned means session semantics. But it gives
little pooling benefit. Size the proxy for N × `DB_MAX_OPEN_CONNS`
backends, and watch `DatabaseConnectionsCurrentlySessionPinned`.
Nothing in this repository has run against a live RDS Proxy; treat it
as unproven until it has.

### Migrations need a session

The one exception is **migrations**. `RUN_MIGRATIONS_ON_BOOT=true` and
`velox-migrate` hold `pg_advisory_lock(76540007)` on a dedicated
single-connection pool for the whole run. Some migrations also run
outside a transaction (`-- no-tx`). Run migrations on a direct
connection or a session-mode pooler, as a deploy step.

### Session variables

Session variables need nothing special. Session GUCs
(`app.tenant_id`, `app.bypass_rls`) are set with `set_config(.., true)`.
The `true` makes the value transaction-scoped: it reverts when the
transaction ends. Prepared statements exist per connection, and the
pooler must know about them; see the pgx floor above.

### Failover bound

A role's tick is a lease of **10 s** (`leader.LeaseTTL`). The holder
renews it every 3 s while its work runs. Every replica polls each role
at least every 5 s (`leader.MaxPoll`).

A leader can crash, be partitioned, be paused by the OS (SIGSTOP), or
lose its VM. In each case it stops renewing. The lease then expires on
the **database clock**, and another replica takes the role on its next
poll. The worst case is **LeaseTTL + MaxPoll = 15 s**; the typical
case is under 10. There is no TCP-keepalive dependency, and nothing to
tune on a load balancer or service mesh. Nothing can strand, because
the lease is a row with an expiry, not a session.

The other half of the problem is the old holder that is still running.
Velox handles it in-process and in SQL:

- A holder that cannot renew for 6 s (`leader.AbandonAfter`) cancels
  its own tick. That is well inside the 10 s TTL, so it stops before
  anyone else can start.
- Every claim funnel (a query that claims due work) re-checks its fence
  token in the claim statement itself (`leader_fence(role, token)`).
  The fence token is a per-role number (`holder_token` in
  `leader_leases`) that goes up every time a replica acquires that role's lease. So a superseded tick that races through anyway
  claims nothing.
- Row-level compare-and-swap at the completion writes (ADR-114 PR-B)
  covers work that was already in flight.

To observe it, use `SELECT * FROM leader_status;`,
`velox_leader_last_tick_age_seconds{role}` and
`velox_leader_lease_lost_total{role,reason}`. To pause a role, run
`SELECT leader_pause('billing', 'oncall', 'why');`. Full procedures:
[runbook-leader-leases.md](runbook-leader-leases.md).

### Query timeouts

**Queries are not bounded by default.** Velox never sets a
server-side `statement_timeout`. `BeginTx` (the transaction-open
helper) inherits whatever deadline the caller's Go context already
carries; it adds none.

`postgres.NewDB` carries `DB_QUERY_TIMEOUT_MS` (default 5s) on the
`*postgres.DB` handle. Only the tenant store and the billing TTFI
(time-to-first-invoice) reader apply it as a per-query deadline. The
other fifteen `internal/*/postgres.go` stores set no deadline of their
own; invoice, usage, credit, subscription and webhook are among them.
Those queries run for as long as the caller's context allows. If you
need a hard ceiling, set it on the role:

```sql
ALTER ROLE velox_app SET statement_timeout = '30s';
```

### Idle-in-transaction timeout

**One transaction-idle bound IS required (ADR-115).** Every write that
closes or moves a subscription's billing period takes the subscription
row lock as the first statement of its transaction. Examples include the
cycle close, a threshold fire, a plan swap, and a scheduled cancel. The
write holds the lock until the invoice commits.

No such transaction spans network I/O: tax runs before it, and the card
charge after. So a healthy holder releases the lock in milliseconds.

A holder can freeze mid-transaction, for example on a paused VM or in a
wedged process. It would then stall every other write that closes or
moves that subscription's period. It would also hide the subscription
from the scheduler's `SKIP LOCKED` fetches for as long as the session
lives.

The idle-in-transaction timeout is the liveness bound. With it, the
server drops a session that sits inside an open transaction without
issuing a statement. Nothing in Velox sets it, so set it on the role:

```sql
ALTER ROLE velox_app SET idle_in_transaction_session_timeout = '30s';
```

## Schema sizing

Tables ordered by expected row growth (top = grows fastest in
production):

| Table | Growth driver | Retention guidance |
|---|---|---|
| `usage_events` | Per-customer event ingestion (the metering substrate). Can hit 10M+ rows/month at scale. | Partition by month or archive >12mo to cold storage. Velox doesn't auto-prune. |
| `audit_log` | Operator + system actions. ~100s/day per tenant. | Retain ≥7y for SOC 2; archive older to compliance storage. |
| `email_outbox` | One row per outbound email (invoice, receipt, dunning, setup link). Growth is per email, not per usage event. | **Do not prune.** Dispatched rows are the delivery record: the invoice timeline (`ListByInvoice`, no age bound), the customer Sent-emails panel, and the Postmark delivery/bounce webhook all read them. |
| `webhook_outbox` | One row per outbound webhook *intent* — transport only; `webhook_events` + `webhook_deliveries` are the replayable record. | `status='dispatched'` rows are safe to delete at any age. Velox does not do it automatically (register: outbox-pruner). Never delete `failed` rows — the runbook re-drives them. |
| `webhook_events`, `webhook_deliveries` | Velox's OUTBOUND event log and per-attempt delivery history (replay, timelines, endpoint stats). | Retain; financial-adjacent evidence. No pruner. |
| `stripe_webhook_events` (Stripe inbound) | Per Stripe webhook event observed (dedup key `tenant_id, livemode, stripe_event_id`). | Retain ≥90d for reconciler + audit; longer if needed for replay. |
| `invoice_dunning_events` | Per dunning lifecycle event. | Retain with the invoice (financial). |
| `invoices`, `invoice_line_items` | Per cycle + per addon line. | **Never prune** — financial. |
| `credit_notes`, `credit_note_line_items` | Per refund/adjustment. | **Never prune** — financial. |
| `subscriptions`, `subscription_items` | Per subscription. Slow growth. | Never prune. |
| `customers`, `customer_billing_profiles` | Per customer. Slow. | Honour GDPR-delete only via tenant-scoped flow (not automated yet). |

**Storage estimation**: at ~10k subscriptions doing ~100 events/day
each, expect ~30M `usage_events` rows/month. Measured on the
benchmark rig, a `usage_events` row costs **~516 bytes** all-in:
62M rows occupied 14 GB of heap (the main table storage) plus 18 GB
across its five indexes
(see [sustained-throughput.md](../benchmarks/sustained-throughput.md)).
So plan for **~15 GB/month, ~185 GB/year**, and partition by month or
set a retention window before year two. TOAST is Postgres's mechanism
for moving oversized column values out of the main table. It does not
help here: it engages above ~2 kB, and these rows are ~230 bytes of
heap.

## Indexes

Velox migrations create indexes for every hot query path. Monitor
`pg_stat_user_indexes` quarterly to spot unused indexes; the only
ones likely to drift are partial indexes (indexes over a filtered
subset of rows) added in later migrations. Don't drop indexes without
verifying via query plans — billing correctness sometimes depends on
subtle index-only scans (plans answered from the index alone, without
touching the table).

## Required user permissions

The DB role running Velox needs:

- `CREATE`, `INSERT`, `UPDATE`, `DELETE`, `SELECT` on its database.
- `USAGE` on the public schema.
- `CREATE EXTENSION` (first install only — see above for the
  pre-created alternative).
- BYPASS RLS is **not** required — Velox sets the GUCs described
  above explicitly.

For managed Postgres (RDS, Cloud SQL): the typical "owner" role of
the database is sufficient.

## Backup and replication: out of scope

Velox doesn't ship Postgres HA (high availability), replication, or
backup tooling. Use whatever your infrastructure already runs:

- Managed Postgres: trust the provider's PITR (point-in-time
  recovery) + replicas.
- Self-managed on K8s: use CloudNativePG, postgres-operator, or
  similar.
- Self-managed on VMs: use pgbackrest, WAL-G, or your DBA team's
  preferred pattern.

What Velox owns is **what to back up and how to validate post-
restore** — see `backup-considerations.md`.

## Health-check query

To verify Velox can talk to Postgres:

```sql
SELECT 1;
```

Velox's `/health/ready` endpoint pings the configured DSN (database
connection string). Use it for readiness probes.

## Compatibility matrix

**Tested** = a test run against it exists. **Exercised** = it has
carried real load, outside CI. **Expected** = no run, but a reason
from the code to think it works. **Untested** = no run and no such
reason; the note says what to expect.

| Postgres flavour | Status | Notes |
|---|---|---|
| Vanilla Postgres 16 | ✅ Tested | CI, every PR (`postgres:16-alpine`) |
| AWS RDS for Postgres 16 | ✅ Exercised | The AWS benchmark rig. Set `rds.force_ssl=1`; use `sslmode=require` in DSN |
| Vanilla Postgres 14, 15, 17 | ⚠️ Expected | No PG15+ syntax in any migration; not exercised in CI |
| Google Cloud SQL Postgres | ⚠️ Expected | Same engine as RDS; not tested |
| Aurora Postgres | ⚠️ Expected | Aurora's slightly different I/O model may affect long scheduler ticks; not tested |
| TimescaleDB on Postgres | ⚠️ Untested | Postgres-compatible; hypertable conversion of `usage_events` is untested advice |
| CockroachDB | ⚠️ Untested | Expect friction on RLS policy semantics, `gen_random_bytes`, and some constraint patterns. Unverified — nobody has run the suite against it |
| YugabyteDB | ⚠️ Untested | Same — the RLS model differs |
