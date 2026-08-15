# Postgres Requirements

What Velox needs from your Postgres. Operating Postgres itself
(replication, backups, failover, sizing) is out of scope — assume
your DBA team or your managed-Postgres provider handles that. This
doc names what they need to know.

## Versions

- **Minimum: Postgres 14.** Velox relies on `gen_random_bytes` from
  `pgcrypto` and standard SQL features through PG14.
- **Tested: Postgres 16.** The `docker-compose.yml` shipped with
  Velox uses `postgres:16-alpine` for local dev.
- **Recommended for production: Postgres 16+.** No PG17-specific
  features in use; safe to upgrade ahead.

## Required extensions

Both are standard `contrib` extensions — present in every managed
Postgres (RDS, Cloud SQL, Aurora, CloudNativePG, postgres-operator,
self-managed Debian/Ubuntu Postgres). Velox creates them via
migrations; the DB role running migrations needs `CREATE EXTENSION`
permission on first install.

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

Velox uses Postgres Row-Level Security for tenant isolation. The
application sets two session GUCs per transaction:

- `app.tenant_id` — set when running tenant-scoped work; RLS policies
  filter on `tenant_id = current_setting('app.tenant_id', true)`.
- `app.bypass_rls` — set to `'on'` for cross-tenant scheduler / reconciler
  paths; RLS policies fall through.

Both GUCs must be allowed; on managed Postgres this is typically the
default. Custom GUCs starting with a namespace (`app.*`) require no
configuration on stock Postgres but some hardened deployments set
`force_row_level_security`. That's compatible — Velox declares its
RLS policies with `FORCE ROW LEVEL SECURITY` on every tenant-scoped
table.

If your environment custom-strips GUCs, ensure `app.tenant_id` and
`app.bypass_rls` are allowed.

## Connection pooling

Velox's defaults (env-overridable):

| Setting | Default | When to tune |
|---|---|---|
| `DB_MAX_OPEN_CONNS` | 20 | Raise if you see connection-pool waits in metrics + Postgres has headroom (`max_connections` typically 100-200) |
| `DB_MAX_IDLE_CONNS` | 5 | Raise to match `MaxOpenConns` if you have spiky workload |
| `DB_CONN_MAX_LIFETIME_MIN` | 30 | Lower to 5-10 if running behind PgBouncer in transaction-pooling mode |
| `DB_CONN_MAX_IDLE_TIME_SEC` | 120 | Idle-eviction; lower for tight-pool environments |

**Concurrent-connection profile**: at peak, Velox holds connections for:

- HTTP request handlers (one per in-flight request).
- Billing scheduler tick (one connection while running per-tenant
  cycles, briefly).
- Webhook outbox dispatcher (one connection per dispatch worker;
  default 1).
- Email outbox dispatcher (one connection; default 1).
- Dunning policy ticks (per-tenant, brief).

For a single-tenant deployment running ~50 RPS, 20 open connections
is comfortable. For multi-tenant deployments, scale linearly with
tenant count up to your Postgres `max_connections` ceiling. Use
PgBouncer **in session mode** in front of Postgres if you exceed ~100
application-side pool size (transaction mode is unsupported — see
below).

## PgBouncer compatibility

Velox is **session-pooler safe** (PgBouncer session mode, or direct
connections). Velox is **NOT transaction-pooler safe** — PgBouncer
transaction/statement mode and RDS Proxy (which multiplexes at
transaction granularity) are unsupported, and the server **refuses to
boot** behind them:

- Velox's singleton workers (billing scheduler, dunning, webhook +
  email outbox dispatchers, webhook retry) elect a leader via
  **session-scoped advisory locks** (`pg_try_advisory_lock` held on a
  pinned connection for the tick's duration). Under transaction
  pooling, consecutive statements on one client connection can run on
  DIFFERENT server sessions: the unlock lands on a session that never
  took the lock, the original server session holds it forever, and
  every future tick on every replica skips as "another leader is
  running". Billing halts silently — no error is ever raised.
- Boot runs `VerifyAdvisoryLockTopology` (two-connection lock/contend/
  release probe + backend-PID stability). Definite transaction-pooling
  evidence → the process exits with an actionable error instead of
  starting a server that would stop invoicing.

### Dead-leader detection window

A leader that *crashes* is free: the kernel closes its sockets, Postgres
reads EOF, and the advisory lock releases in milliseconds. A leader whose
**host disappears without closing the socket** — network partition, power
loss, VM terminate or pause, a security-group change — is not. No FIN ever
arrives, so Postgres cannot tell that session from a healthy idle client
and the lock survives. Every replica then skips its tick, and because that
skip logs at `Debug`, a production log at `Info` shows nothing at all.

The only bound on that outage is TCP keepalive. Postgres defaults give
`tcp_keepalives_idle` 7200s + `interval` 75s × `count` 9 = **7875s
(2h11m)** of silently halted billing. Velox therefore sets these on the
lock-holding connection itself (they are `context=user`, so no superuser
is needed) to **60 + 10×3 = 90s**.

Operators: if you front Postgres with anything that terminates or rewrites
TCP — a load balancer, a service mesh, an NLB with its own idle timeout —
confirm it does not strip keepalives, and keep any idle timeout it enforces
*above* 60s so it doesn't sever healthy lock connections mid-tick.
`TestLeaderFailover_LockConnBoundsDetectionWindow` is the regression gate.

Within a supported topology the rest of the stack is unremarkable:
session GUCs (`app.tenant_id`, `app.bypass_rls`) are set with
`set_config(.., true)` (transaction-scoped), no session-lifetime
prepared statements, and transactions are bounded by the default 5s
query timeout via `postgres.NewDB`.

## Schema sizing

Tables ordered by expected row growth (top = grows fastest in
production):

| Table | Growth driver | Retention guidance |
|---|---|---|
| `usage_events` | Per-customer event ingestion (the metering substrate). Can hit 10M+ rows/month at scale. | Partition by month or archive >12mo to cold storage. Velox doesn't auto-prune. |
| `audit_log` | Operator + system actions. ~100s/day per tenant. | Retain ≥7y for SOC 2; archive older to compliance storage. |
| `email_outbox` | Per outbound email (invoices, dunning, receipts). Archive `dispatched` rows. | Prune `dispatched` >90d via cron. |
| `webhook_outbox` | Per outbound webhook. Same shape as email_outbox. | Prune `dispatched` >90d. |
| `webhook_events` (Stripe inbound) | Per Stripe webhook event observed. | Retain ≥90d for reconciler + audit; longer if needed for replay. |
| `invoice_dunning_events` | Per dunning lifecycle event. | Retain with the invoice (financial). |
| `invoices`, `invoice_line_items` | Per cycle + per addon line. | **Never prune** — financial. |
| `credit_notes`, `credit_note_line_items` | Per refund/adjustment. | **Never prune** — financial. |
| `subscriptions`, `subscription_items` | Per subscription. Slow growth. | Never prune. |
| `customers`, `customer_billing_profiles` | Per customer. Slow. | Honour GDPR-delete only via tenant-scoped flow (not automated yet). |

**Storage estimation**: at ~10k subscriptions doing ~100 events/day
each, expect ~30M `usage_events` rows/month, ~10GB/year on disk after
toast compression. Plan accordingly.

## Indexes

Velox migrations create indexes for every hot query path. Monitor
`pg_stat_user_indexes` quarterly to spot unused indexes; the only
ones likely to drift are partial indexes added in later migrations.
Don't drop indexes without verifying via query plans — billing
correctness sometimes depends on subtle index-only scans.

## Required user permissions

The DB role running Velox needs:

- `CREATE`, `INSERT`, `UPDATE`, `DELETE`, `SELECT` on its database.
- `USAGE` on the public schema.
- `CREATE EXTENSION` (first install only — see above for pre-created
  alternative).
- BYPASS RLS is **not** required — Velox sets GUCs explicitly.

For managed Postgres (RDS, Cloud SQL): the typical "owner" role of
the database is sufficient.

## Backup and replication: out of scope

Velox doesn't ship Postgres HA, replication, or backup tooling. Use
whatever your infrastructure already runs:

- Managed Postgres: trust the provider's PITR + replicas.
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

Velox's `/health/ready` endpoint pings the configured DSN.
Use it for readiness probes.

## Compatibility matrix

| Postgres flavour | Tested | Notes |
|---|---|---|
| Vanilla Postgres 14-17 | ✅ | Reference target |
| AWS RDS for Postgres | ✅ | Set `rds.force_ssl=1`; use `sslmode=require` in DSN |
| Google Cloud SQL Postgres | ✅ | Same as RDS |
| Aurora Postgres | ⚠️ | Should work; not specifically tested. Aurora's slightly different I/O model may affect long scheduler ticks. |
| CockroachDB | ❌ | Not compatible — RLS, `gen_random_bytes`, and some constraint patterns differ. |
| YugabyteDB | ❌ | Same — not tested, RLS model differs. |
| TimescaleDB on Postgres | ✅ | Postgres-compatible; `usage_events` benefits from hypertable conversion if you operate at scale (operator-side decision). |
