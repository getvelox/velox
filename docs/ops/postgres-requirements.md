# Postgres requirements

Velox keeps all its data in one Postgres database. This page is a checklist for the engineer or DBA (database administrator) who prepares that database, often a managed one such as AWS RDS. Running Postgres itself (replication, failover, sizing the server) stays with you or your provider.

The Compose stack in [`deploy/compose`](../../deploy/compose/README.md) already covers the version, the extensions and the two users. Use this page when you bring your own Postgres, and for the timeouts on any install.

## Checklist

- [ ] [Postgres 16](#versions).
- [ ] [Three extensions](#extensions) exist, or the admin user may create them.
- [ ] [Two database users](#the-two-database-users): an admin user for upgrades and a limited user for requests.
- [ ] [The idle-transaction timeout](#timeouts-on-the-limited-user) set on the limited user, and a query timeout if you want one.
- [ ] [A direct connection for database upgrades](#connection-poolers), if you use a pooler.
- [ ] [Room to grow](#how-the-data-grows) for the usage table.
- [ ] Backups: see [Backups](backup-considerations.md).

## Versions

- **Tested: Postgres 16.** CI runs every test against `postgres:16-alpine`, and both Compose files (local development and the one-VM install) run it too.
- **Oldest that should work: Postgres 14.** Velox uses `gen_random_bytes` from the `pgcrypto` extension and standard SQL up to Postgres 14. CI applies the migrations on Postgres 14 for every PR, but the tests run only on 16, and nothing runs on 15, so prefer 16.
- **Newer than 16:** Velox uses nothing specific to Postgres 17, so upgrading ahead is safe.

## Extensions

Velox needs three standard extensions. They ship with Postgres and with every common managed Postgres (RDS, Cloud SQL, Aurora, CloudNativePG).

| Extension | What Velox uses it for |
|---|---|
| `pgcrypto` | Random bytes for IDs, and hashing |
| `citext` | The email column on users, which ignores upper and lower case |
| `btree_gist` | A rule that stops two billing periods of one subscription from overlapping |

Velox creates them during its database upgrades (migrations), so the admin user needs permission to create extensions. If your provider forbids that, create them first, as a user that may:

```sql
CREATE EXTENSION IF NOT EXISTS pgcrypto WITH SCHEMA public;
CREATE EXTENSION IF NOT EXISTS citext WITH SCHEMA public;
CREATE EXTENSION IF NOT EXISTS btree_gist WITH SCHEMA public;
```

They must be in schema `public`, where Velox keeps its tables and looks for them. If one already exists in another schema (some providers put extensions in a schema named `extensions`), the first upgrade stops with an error such as `function public.gen_random_bytes(integer) does not exist`, and changes nothing. Move the extension with `ALTER EXTENSION pgcrypto SET SCHEMA public`, if nothing else in the database depends on where it is, or give Velox a database of its own.

## The two database users

Velox connects as two users. The second one is what keeps each account's data apart.

| Setting | User | Used for | Needs |
|---|---|---|---|
| `DATABASE_URL` | The admin user, usually the database owner | Database upgrades (migrations) | Create tables and extensions in the database |
| `APP_DATABASE_URL` | A limited user, usually `velox_app` | Every API request and background job | Read and write the tables. It must not be a superuser and must not have `BYPASSRLS`. |

Postgres hides each account's rows from the others with a feature called Row-Level Security (RLS). A superuser, or a user with `BYPASSRLS`, skips that check. So with `APP_ENV` set to `staging` or `production`, Velox refuses to start when `APP_DATABASE_URL` points at such a user.

Create the limited user as the admin user, before the first start:

```sql
-- in psql: \set pw 'a-long-random-password'
CREATE ROLE velox_app WITH LOGIN PASSWORD :'pw';
GRANT ALL PRIVILEGES ON DATABASE velox TO velox_app;
GRANT ALL ON SCHEMA public TO velox_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON TABLES TO velox_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON SEQUENCES TO velox_app;
```

`ALTER DEFAULT PRIVILEGES` covers the tables the admin user creates later, so run the upgrades as that same admin user.

**You see:** `CREATE ROLE`, `GRANT` and `ALTER DEFAULT PRIVILEGES` lines from psql. When Velox starts, it checks the user and starts listening.

More on what Velox checks at start-up: [Self-host, "The two database users"](../self-host.md#the-two-database-users).

### Settings Velox sets on each transaction

Velox tells Postgres which account a transaction belongs to with two custom settings, `app.tenant_id` and `app.bypass_rls`. They need no setup on standard Postgres or on common managed Postgres. If your environment blocks custom settings, allow these two. Every account-scoped table uses `FORCE ROW LEVEL SECURITY`, so a hardened setup that requires it also works.

## Timeouts on the limited user

Set these two on `velox_app`, as the admin user:

```sql
ALTER ROLE velox_app SET idle_in_transaction_session_timeout = '30s';
ALTER ROLE velox_app SET statement_timeout = '30s';
```

**You see:** `ALTER ROLE` twice. They apply to new connections, so restart Velox afterwards.

- **`idle_in_transaction_session_timeout` is required.** Closing a billing period locks the subscription's row until the invoice is saved. If a Velox process freezes in the middle (a paused VM, for example), that lock blocks every other change to the subscription. This timeout ends such a frozen session. Nothing in Velox sets it.
- **`statement_timeout` is optional.** Velox sets no time limit on most queries. `DB_QUERY_TIMEOUT_MS` (default 5 seconds) covers only a few. Set this if you want a hard limit on every query.

## Connection poolers

A connection pooler, such as PgBouncer, shares a few database connections among many clients. A pooler in *session mode* gives each client its own connection while it stays connected. *Transaction mode* hands each transaction to whichever connection is free.

| Setup | Works for requests and background jobs | Works for database upgrades |
|---|---|---|
| Direct connection | Yes | Yes |
| PgBouncer, session mode | Yes | Yes |
| PgBouncer 1.21 or newer, transaction mode, `max_prepared_statements` above 0 | Yes. CI runs the whole test suite this way, with PgBouncer 1.24 and 200. | No |
| Older PgBouncer in transaction mode | Yes, if you add `default_query_exec_mode=cache_describe` to both database URLs | No |
| RDS Proxy | Expected to work, not tested. It pins each client to one connection, so it pools little. | Use a direct connection |

**Database upgrades need a direct or session-mode connection.** They hold a lock for the whole run, and some steps run outside a transaction. Run them over that connection, with `RUN_MIGRATIONS_ON_BOOT=true` or the `velox migrate` command, as a deploy step.

Why `max_prepared_statements` matters: the Go database driver reuses prepared statements on each connection. In transaction mode the pooler must track them, or two thirds of queries fail with `prepared statement "stmtcache_…" does not exist`. `scripts/pgbouncer-test.sh` runs the same pooler check locally.

With RDS Proxy, size it for the number of copies × `DB_MAX_OPEN_CONNS` connections, and watch `DatabaseConnectionsCurrentlySessionPinned`.

### Connection pool settings

| Setting | Default | When to change it |
|---|---|---|
| `DB_MAX_OPEN_CONNS` | 20 | Raise it if requests wait for a connection and Postgres has room (`max_connections` is often 100 to 200) |
| `DB_MAX_IDLE_CONNS` | 5 | Raise it to match `DB_MAX_OPEN_CONNS` for spiky traffic |
| `DB_CONN_MAX_LIFETIME_MIN` | 30 | Lower it to 5 to 10 behind a pooler with a short `server_lifetime` |
| `DB_CONN_MAX_IDLE_TIME_SEC` | 120 | Lower it where connections are scarce |

For one account at about 50 requests per second, 20 connections is enough. With many accounts, raise it with the number of accounts, up to your `max_connections`. Above about 100 connections per copy, put PgBouncer or RDS Proxy in front.

## How the data grows

| Table | Grows with | Keep |
|---|---|---|
| `usage_events` | Every usage event. Can pass 10 million rows a month. | Split by month, or move rows older than 12 months to cold storage. Velox deletes nothing. |
| `audit_log` | Every change by a person or by Velox. Hundreds a day per account. | Often 7 years for audits such as SOC 2 |
| `email_outbox` | One row per email sent | All. The invoice timeline, the customer's Sent emails and the Postmark webhook read them. |
| `webhook_outbox` | One row per webhook queued | Rows with `status='dispatched'` can be deleted at any age; Velox does not delete them. Keep `failed` rows: the runbook sends them again. |
| `webhook_events`, `webhook_deliveries` | Every webhook Velox sends, and every try | All. Replay and timelines read them. |
| `stripe_webhook_events` | Every event Stripe sends | At least 90 days |
| `invoice_dunning_events` | Every payment-retry step | As long as the invoice |
| `invoices`, `invoice_line_items`, `credit_notes`, `credit_note_line_items` | Each invoice, line and credit note | All. These are financial records. |
| `subscriptions`, `subscription_items`, `customers`, `customer_billing_profiles` | Slowly | All. There is no automatic deletion of a customer's personal data yet. |

**How much space:** 10,000 subscriptions sending 100 events a day each make about 30 million `usage_events` rows a month. On the benchmark rig one row took about 516 bytes including its indexes: 62 million rows used 14 GB of table plus 18 GB of indexes ([sustained throughput](../benchmarks/sustained-throughput.md)). Plan for about 15 GB a month, or 185 GB a year, and split the table by month or set a retention window before the second year.

## Indexes

Velox's migrations create the indexes its queries need. Check `pg_stat_user_indexes` every few months for unused ones. Do not drop an index without reading the query plans first: some billing queries depend on reading from the index alone.

## Health check

`/health/ready` pings the database. Use it for readiness checks.

## Which Postgres works

**Tested** means a test run against it exists. **Exercised** means it has carried real load, outside CI. **Expected** means no run, but a reason from the code to think it works. **Untested** means no run and no such reason.

| Postgres | Status | Notes |
|---|---|---|
| Postgres 16 | ✅ Tested | CI, every PR (`postgres:16-alpine`) |
| AWS RDS for Postgres 16 | ✅ Exercised | The AWS benchmark rig. Set `rds.force_ssl=1` and use `sslmode=require` in the database URLs. |
| Postgres 14 | ⚠️ Expected | CI applies the migrations on `postgres:14-alpine` for every PR; the tests run only on 16 |
| Postgres 15, 17 | ⚠️ Expected | No migration uses syntax newer than 14; not run in CI |
| Google Cloud SQL for Postgres | ⚠️ Expected | Standard Postgres; not tested |
| Aurora Postgres | ⚠️ Expected | Its storage works differently, which may slow long billing runs; not tested |
| TimescaleDB | ⚠️ Untested | Compatible with Postgres; turning `usage_events` into a hypertable is untested |
| CockroachDB | ⚠️ Untested | Expect problems with Row-Level Security, `gen_random_bytes` and some constraints. Nobody has run the tests against it. |
| YugabyteDB | ⚠️ Untested | Its Row-Level Security differs |

## Related

- [Background jobs on two or more copies](runbook/background-job-stuck.md): how copies share billing and other background work, and what to do when a job looks stuck.
- [Backups](backup-considerations.md).
- Why it works this way: [ADR-114](../adr/114-leader-leases-tick-scoped-fencing.md) (background jobs and poolers) and [ADR-115](../adr/115-one-closer-for-the-billing-period.md) (the subscription row lock).
