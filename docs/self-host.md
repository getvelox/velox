# Self-host Velox

This page is for the engineer who installs and runs Velox. It covers the two ways to run it, what Postgres needs, the settings, how much load it takes, and what to watch.

Velox is one Go program that talks to Postgres. In production it also needs Redis, which it uses only for rate limiting.

## Two ways to run it

| | Good for | What you run |
|---|---|---|
| **One VM with Docker Compose** | Trying Velox, and production for a small team where a few minutes of downtime during a restart is fine | Five containers on one machine |
| **Two or more copies behind a load balancer** | High availability: production where a restart or a crashed copy must not cause downtime | Two or more `velox-api` copies, a managed Postgres, a managed Redis |

Both are supported for production. Start with one VM; move to two or more copies when downtime during a restart is no longer acceptable.

A Helm chart and a Terraform module are not written yet. They will be when an early customer tells us which platform they run.

## One VM with Docker Compose

Follow the step-by-step guide: [`deploy/compose/README.md`](../deploy/compose/README.md).

In short:

1. Set four secrets.
2. Run `docker compose up -d`.
3. Make one `POST /v1/bootstrap` call. It returns your dashboard login and your API keys.

That gives you a working [tenant](glossary.md): one business account with its own data, dashboard users and API keys.

To run it for real customers, follow [Production on one VM](production-one-vm.md). It adds HTTPS, the web address settings, email, Stripe, nightly backups with a restore test, a go-live checklist and upgrade steps.

**Signing in before you add TLS.** The stack serves plain HTTP on port 80. Until you put TLS in front, sign in to the dashboard at `http://localhost/` on the server, or through an SSH tunnel. Sign-in does not work at `http://<server-ip>/`. The [compose guide](../deploy/compose/README.md#tls) covers both.

**What to expect from one VM:**

- There is one copy of the API. While it restarts, Velox is down.
- Postgres runs on the same machine, unless you point `DATABASE_URL` at one elsewhere.
- There is no load balancer.

## Two or more copies behind a load balancer

This is the high-availability setup:

- Two or more `velox-api` copies behind a load balancer.
- A managed Postgres with automatic failover and point-in-time restore.
- A managed Redis.

Things to know:

- **Background jobs run once, not once per copy.** Billing runs, payment retries and webhook delivery are background jobs. The copies agree, through a row in Postgres, on which one runs each job. The others stand by, and one takes over within about 15 seconds if the first dies.
- **Connection poolers.** PgBouncer in transaction mode works for the server, and CI runs the test suite through it. RDS Proxy is expected to behave the same way but has not been tested ([details](ops/postgres-requirements.md)). Run database upgrades (migrations) over a direct connection.
- **Give each copy 120 seconds to shut down.** On a stop signal, a copy first tells the load balancer it is leaving, then finishes its work. The worst case is 90 seconds.
- **A few features still assume one copy.** The live event stream in the dashboard is one. None of them touch money. The full list is in the [readiness review](records/ha-readiness-2026-07-06.md).

Load balancer setup, health checks, shutdown and rolling upgrades: [High availability](ops/high-availability.md).

## Working on Velox locally

This runs the API from source on your machine. It is for development, not for hosting.

```bash
git clone https://github.com/getvelox/velox.git
cd velox

cp .env.example .env   # make dev reads it; the defaults work as they are
docker compose up -d postgres redis mailpit
VELOX_BOOTSTRAP_EMAIL=you@example.com VELOX_BOOTSTRAP_PASSWORD=change-me-please1 \
  make bootstrap
make dev
```

The email and password are optional. Without them, the owner is `admin@velox.local` and bootstrap prints a generated password. A password needs at least 12 characters.

You now have:

| Service | Where | Notes |
|---|---|---|
| Postgres | `:5432` | Password `velox`. Data is kept in a Docker volume. |
| Redis | `:6379` | Used for rate limiting. Optional outside production. |
| Mailpit | `:1025` (SMTP), `:8025` (web) | Catches every email Velox sends, so nothing leaves your machine. |
| The API | `:8080` | Started by `make dev`. Not a container. |

The dashboard runs separately:

```bash
cd web-v2 && npm install && npm run dev
# → http://localhost:5173
```

### Add a second account

One Velox server can hold many accounts (tenants), each with its own data,
dashboard users and API keys. To add another one, run the bootstrap tool
again with a different owner email:

```bash
make bootstrap VELOX_BOOTSTRAP_EMAIL=tenant-b@local \
  VELOX_BOOTSTRAP_PASSWORD='choose-a-password' \
  VELOX_BOOTSTRAP_TENANT='Tenant B'
```

The tool runs from source, so it needs Go and this repository. It is not in
the Docker image. There is no button for this in the dashboard, and a
secret key cannot do it: only a platform key can manage accounts, and a
standard Velox server issues none.

## Postgres

The Compose stack ships Postgres 16 with default settings, which is enough for trying Velox. For your own Postgres:

- **Version:** 16 is the version CI tests and the Compose stack runs. Postgres 14 is the oldest that should work: CI applies the migrations on it, but the tests run only on 16. See [Postgres requirements](ops/postgres-requirements.md#versions).
- **Extensions:** `pgcrypto`, `citext` and `btree_gist`. Velox creates them during database upgrades, so the role that runs upgrades needs permission to create extensions. If your managed Postgres forbids that, create them first: [Postgres requirements](ops/postgres-requirements.md).
- **Backups:** point-in-time restore (most managed Postgres offer it) is best. A nightly `pg_dump`, which `scripts/backup.sh` takes, also works, but you lose what was written since the last dump. Back up `VELOX_ENCRYPTION_KEY` too, separately: without it, the encrypted data in a backup (customer details, Stripe keys, webhook secrets) cannot be read. See [backups](ops/backup-considerations.md).

### The two database users

Velox uses two database users, and the second one is what keeps tenants apart.

| Setting | User | Used for |
|---|---|---|
| `DATABASE_URL` | The admin user | Database upgrades |
| `APP_DATABASE_URL` | A limited user, usually `velox_app` | Every API request |

Postgres hides each tenant's rows from the others (a feature called Row-Level Security). An admin user can bypass that, so requests must never run as the admin.

The Compose stack creates `velox_app` for you from `VELOX_APP_DB_PASSWORD`. On your own Postgres, create it yourself:

```sql
-- use psql -v pw='...' and :'pw' quoting, or substitute a literal
CREATE ROLE velox_app WITH LOGIN PASSWORD :'pw';
GRANT ALL PRIVILEGES ON DATABASE velox TO velox_app;
GRANT ALL ON SCHEMA public TO velox_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON TABLES TO velox_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON SEQUENCES TO velox_app;
```

With `APP_ENV` set to `staging` or `production`, Velox refuses to start when `APP_DATABASE_URL`:

- is missing,
- uses the default password `velox_app`, or an empty one,
- cannot connect, or
- points at a user that can bypass Row-Level Security. This catches a copied `DATABASE_URL`.

With `APP_ENV=local` it builds `velox_app:velox_app` from `DATABASE_URL` and logs a warning instead.

## Database upgrades (migrations)

A migration is a versioned script that changes the database schema. Velox applies them with [goose](https://github.com/pressly/goose). A new database gets one baseline, the schema of Velox v0.8.1 (migration 180); later migrations are in [`internal/platform/migrate/sql/`](../internal/platform/migrate/sql/).

- **See the plan first.** `velox migrate status` shows the database's version, the migrations this release would apply, and whether a migration is running now. It changes nothing.
- Set `RUN_MIGRATIONS_ON_BOOT=true` and Velox applies any new ones when it starts. The Compose stack and the local `.env.example` both set it. The default is `false`: then run `velox migrate` before you start a new release, because Velox refuses to start while a migration is missing.
- If several copies start at once, they take turns: one applies the migrations and the others wait, then find nothing left to do.
- **A failed migration leaves nothing to repair.** Each one runs in one transaction, so a failure undoes all of it. Fix the cause the error names and run the upgrade again. An index build runs without a transaction and may leave an INVALID index; running the upgrade again drops it and builds it again. See [A database upgrade stopped](ops/runbook/migration-failed-midway.md).
- **Never roll back in production.** `velox migrate rollback` runs only with `APP_ENV=local`. To go back after an upgrade, restore the backup you took before it; see [Going back](production-one-vm.md#going-back).
- **Each migration waits at most 5 seconds for a table lock.** While a migration waits for a lock, other queries on that table wait behind it, so Velox stops the upgrade instead of letting the table stall. The migration leaves nothing behind; run the upgrade again when the table is quieter. `MIGRATION_LOCK_TIMEOUT` changes the limit. See [An upgrade waited for a table lock](ops/runbook/migration-lock-timeout.md).
- **From v0.8.x,** v0.9.0 takes over a database at version 180 (v0.8.0 or v0.8.1) without changing a table. Older databases upgrade to v0.8.1 first. The [changelog](../CHANGELOG.md) has the steps.

## Upgrading

1. Read the upgrade notes in the [changelog](../CHANGELOG.md) for every version between yours and the new one.
2. Take a backup. See [backups](ops/backup-considerations.md).
3. See the plan: run the new version's `velox migrate status`.

   **You see:** `state:` with the migrations it will apply, and `running now:  no`. A `refused` state names its next step; follow it before you go on.

4. Start the new version. With `RUN_MIGRATIONS_ON_BOOT=true`, it applies the database upgrades as it starts. Otherwise run `velox migrate` first.

   **You see:** `/health/ready` answers 200 again.

On one VM there is one copy, so Velox is down while the new version starts. For the Compose commands, see [Day-to-day commands](../deploy/compose/README.md#day-to-day-commands). With two or more copies, see [Upgrades](ops/high-availability.md#upgrades).

## Settings

Velox reads its settings from environment variables. The tables below list the main ones. More settings, each with a comment, are in the repository's [`.env.example`](../.env.example). The Compose stack reads its own [`deploy/compose/.env.example`](../deploy/compose/.env.example).

**Required**

| Variable | What it is |
|---|---|
| `DATABASE_URL` | Postgres connection URL for the admin user |
| `APP_DATABASE_URL` | Postgres connection URL for the limited user. Required in `staging` and `production`. |
| `VELOX_ENCRYPTION_KEY` | Encrypts stored personal data. 64 hex characters. Required in `production`. |
| `REDIS_URL` | Redis, for rate limiting. Required in `production`. |
| `TRUST_PROXY` | The addresses of your proxy or load balancer, comma-separated. Velox then trusts the client address they forward. Use `none` if there is no proxy. Required in `production`. Velox refuses to start if an entry is not an IP address or CIDR. |

Why `TRUST_PROXY` is required: without it, every request behind a load balancer appears to come from the load balancer, so all users would share one rate limit. The audit log and dashboard sign-in records would also show the load balancer's address.

**Read once, when creating the first tenant** (by `make bootstrap`, not by the server)

| Variable | What it is |
|---|---|
| `VELOX_BOOTSTRAP_EMAIL` | The dashboard owner's email. Default `admin@velox.local`. |
| `VELOX_BOOTSTRAP_PASSWORD` | The owner's password. If unset, one is generated and printed once. |
| `VELOX_BOOTSTRAP_TENANT` | The tenant's name. Default `Demo Tenant`. |

**Optional**

| Variable | Default | What it does |
|---|---|---|
| `APP_ENV` | `local` | `local`, `staging` or `production`. Outside `local`, cookies are marked secure and the start-up checks on this page apply. A value that starts with `prod` but is not exactly `production`, such as `prod`, stops start-up. Any other unrecognised value starts with a warning and is treated as not production. |
| `RUN_MIGRATIONS_ON_BOOT` | `false` | Apply database upgrades at start-up. With `false`, run `velox migrate` before starting a new release. |
| `MIGRATION_LOCK_TIMEOUT` | `5s` | How long a database upgrade waits for a table lock before it stops. A duration such as `30s`, or `0` for no limit. Any other value stops the upgrade. |
| `DASHBOARD_BASE_URL` | unset | The dashboard's address, used in password-reset links. If unset, no password-reset emails are sent. For local development use `http://localhost:5173`. |
| `CUSTOMER_PORTAL_URL` | unset | The dashboard's address, with no path. Stripe returns a customer here after they save a card. If unset, Stripe sends them to `http://localhost:5173`, which exists only in local development; the card is still saved. |
| `SMTP_HOST`, `SMTP_PORT` | unset | Your email server. If unset, Velox sends no email. The local `.env.example` points these at Mailpit; the Compose stack leaves them unset. See [email setup](ops/email-setup.md). |
| `VELOX_BOOTSTRAP_TOKEN` | unset | Allows the `POST /v1/bootstrap` call that creates the first tenant. At least 16 characters outside `local`. |
| `METRICS_TOKEN` | unset | The bearer token a scraper must send to read `/metrics`. If unset, `/metrics` answers 401 in `production` and is open elsewhere. |
| `INVOICE_GRACE_PERIOD` | `1h` | How long Velox waits after a billing period ends before it builds the invoice, so usage that arrives late but belongs to the period is billed. See below. A duration from `0` to `72h`, such as `30m` or `24h`. Any other value stops start-up. `0` builds the invoice as soon as the period ends. |

Stripe is not set here. Each tenant connects its own Stripe account in the dashboard, under Settings → Payments. The Stripe API keys and the webhook signing secret (`whsec_…`) are stored per tenant, in the database.

**`INVOICE_GRACE_PERIOD` delays every period-end invoice**, not only invoices with usage. A subscription's period-end invoice, and the final invoice of a cancel scheduled for a date, is built on the first billing run after the period end plus the grace. The billing run is hourly, so with the default the invoice is built 1 to 2 hours after the period ends. Everything that follows the invoice moves with it: the `invoice.finalized` webhook, the tax record, credits applied, the charge, and the first payment-failure retry. The invoice's dates and amounts do not move: it is issued and dated at the period end. A cancel or a plan change scheduled for a date also takes effect at that close, up to the grace after its date: until then the subscription is still `active` on its old plan, and `subscription.canceled` is sent then. Its dates do not move: `canceled_at` is the scheduled date. A scheduled resume of paused collection that falls after a period end waits for that period's close too. A credit grant is retired up to the grace after its `expires_at`, and the balance shows it until then.

Raise it if your usage pipeline can deliver an event more than an hour late. Usage stamped inside a period that arrives after its invoice is built is stored but not billed (see [late events](api-surface.md)). Other billing products use 1 hour (Stripe), 12 hours (Orb) and 24 hours (Metronome).

These close a period at once, with no grace: an immediate cancel, `POST /v1/subscriptions/{id}/retry-billing`, any other change to a subscription whose period has ended, and advancing a test clock. `POST /v1/billing/run` waits for the grace, like the hourly run.

## How much load it takes

On AWS, one database took **1,000 usage events per second** on a
`db.m7g.2xlarge` (32 GB memory) and **12,000 per second** on a
`db.m7g.4xlarge` (64 GB memory). The database ran out first, never the Velox
process. One small VM takes far less.

The measured runs, and what to do when you need more, are in
[How much traffic Velox handles](scale.md).

## What to watch

Velox serves Prometheus metrics at `/metrics` and writes structured logs to standard output. Connect both to the monitoring you already run.

Velox ships one Grafana dashboard, [`ops/grafana-dashboard.json`](ops/grafana-dashboard.json). Import it into Grafana and point it at the Prometheus that scrapes `/metrics`. It shows HTTP traffic, billing runs, payments, payment retries, the email and webhook queues, and tax. Velox does not ship alert rules.

In `production`, set `METRICS_TOKEN` and have Prometheus send it as a bearer token. Without it, `/metrics` answers 401. At most three scrapes run at once; a fourth gets a 503.

The metrics worth an alert:

| Metric | What it tells you |
|---|---|
| `velox_billing_cycle_duration_seconds` | How long a billing run takes |
| `velox_tax_outcome_total` | Tax calculations that failed, and why |
| `velox_audit_write_errors_total` | Audit log entries that failed to save |
| `velox_audit_uncovered_mutation_total` | Changes made with no audit log entry. Should stay at zero. |
| `velox_stripe_breaker_state` | Whether Velox has paused calls to Stripe after repeated failures. 0 is normal, 2 is paused. |

What to do when one fires: the [operations runbook](ops/runbook/index.md).

## Related

- [Tax calculation](ops/tax-calculation.md): tax providers and what happens when one fails.
- [Stripe end-to-end test](dev/stripe-end-to-end-test.md): check a real Stripe account before going live.
- [Decision records](adr/README.md): why Velox is built this way. The ones behind this page are the in-process scheduler (ADR-006), tenant isolation (ADR-003), the limited database user (ADR-073) and how copies share background jobs (ADR-114).
