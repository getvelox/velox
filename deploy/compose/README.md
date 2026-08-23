# Velox self-host on a single VM (Docker Compose)

A 5-minute path from a fresh VM to a working Velox tenant (one
isolated billing organization — Velox is multi-tenant). Five
containers behind one nginx: postgres, redis, velox-api, velox-dashboard, nginx.
Migrations run on first boot.

## Prerequisites

- Docker 24+ with the Compose plugin (`docker compose version` works)
- Open inbound TCP/80 if you want the API reachable from outside the VM
- ~1 vCPU and 1 GB RAM is enough for evaluation; production sizing in
  [`docs/self-host.md`](../../docs/self-host.md)

## 1. Clone and configure

```bash
git clone https://github.com/getvelox/velox.git
cd velox/deploy/compose
cp .env.example .env
```

Edit `.env` and set the four required secrets:

```bash
# Postgres admin/migration password
POSTGRES_PASSWORD=$(openssl rand -hex 24)

# Password for the least-privilege velox_app runtime role (RLS enforced).
# hex output keeps it URL-safe — it's embedded into a connection URL.
VELOX_APP_DB_PASSWORD=$(openssl rand -hex 24)

# 64 hex chars (32 bytes) for PII encryption-at-rest
VELOX_ENCRYPTION_KEY=$(openssl rand -hex 32)

# Authorises POST /v1/bootstrap to create the first tenant (min 16 chars)
VELOX_BOOTSTRAP_TOKEN=$(openssl rand -hex 32)
```

(Or generate them in-shell and paste in.) Everything else in `.env` is
optional — the binary boots without SMTP or Stripe configuration. See
`.env.example` for the full list.

## 2. Bring the stack up

```bash
docker compose up -d
```

First boot does three things you'll see in the logs:

1. `postgres` initialises the `velox` database and creates the
   `velox_app` runtime role with your `VELOX_APP_DB_PASSWORD` (see
   `postgres-init.sh`).
2. `velox-api` starts with `RUN_MIGRATIONS_ON_BOOT=true`, applies all
   pending migrations from `internal/platform/migrate/sql/`, verifies
   the runtime role cannot bypass RLS (row-level security, the
   Postgres feature Velox uses for tenant isolation), then begins
   serving on `:8080` and starts the in-process scheduler.
3. `nginx` proxies host `:80` to `velox-api:8080`.

Tail the logs while it converges:

```bash
docker compose logs -f velox-api
```

You're done when you see `"listening" addr=:8080`.

## 3. Health check

```bash
curl -fsS http://localhost/health
# {"status":"ok"}

curl -fsS http://localhost/health/ready
# {"checks":{"api":"ok","database":"ok","scheduler":"..."},"status":"ok"}
```

`/health` is liveness (process up). `/health/ready` is readiness
(database reachable + scheduler not stalled). Wire your load balancer's
health check to `/health/ready`.

## 4. Create your first tenant

The bootstrap endpoint is gated by the token you set in `.env`.

**Run this ON the VM** (`http://localhost/...`) or through an SSH
tunnel. The response carries your owner password and live API key,
and this stack terminates no TLS (no service in it accepts HTTPS, so traffic to the VM travels in plaintext) —
do not send it over plain HTTP across a network. The token travels in
the `Authorization` header (never a query string) so it stays out of
proxy access logs.

```bash
curl -X POST http://localhost/v1/bootstrap \
  -H "Authorization: Bearer ${VELOX_BOOTSTRAP_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{"tenant_name":"Acme","owner_email":"you@example.com","owner_password":"a-strong-password-12ch"}'
```

(`owner_email`/`owner_password` are optional — omitted, the owner is
`admin@velox.local` with a generated password returned once. Passwords
must be at least 12 characters.)

The response carries, exactly once (Velox stores only hashes — none of
it is recoverable later):

- your dashboard owner credentials (`owner_email` / `owner_password`)
  — log in at `http://localhost/` (the `velox-dashboard` container serves
  the operator UI behind the same proxy as the API),
- a TEST secret key (`vlx_secret_test_…`),
- a LIVE secret key (`vlx_secret_live_…` — charges real cards; ignore
  it until you mean it),
- a test publishable key.

A repeat call answers `409 already_bootstrapped` — one-shot by design.

## 5. Verify

```bash
SECRET=vlx_secret_test_...
curl -fsS http://localhost/v1/customers \
  -H "Authorization: Bearer ${SECRET}"
# {"data":[],"total":0}
```

You're live.

## Operating the stack

| Action | Command |
|---|---|
| View logs | `docker compose logs -f` |
| Restart the API | `docker compose restart velox-api` |
| Update to a new image tag | edit `VELOX_IMAGE` in `.env`, then `docker compose pull && docker compose up -d` |
| Stop everything (preserve data) | `docker compose stop` |
| Stop and wipe data | `docker compose down -v` |
| Run a one-off migration command | `docker compose run --rm velox-api migrate status` |
| Open a psql shell | `docker compose exec postgres psql -U velox -d velox` |

## TLS

This stack listens on plain HTTP. For production add TLS one of two
ways:

- **Managed load balancer in front** — AWS ALB, Cloudflare, GCP HTTPS LB.
  Terminate TLS there, target this VM's port 80 over private network.
- **certbot on the host** — install nginx on the host (not inside the
  compose stack), terminate TLS there, proxy to `localhost:80`. Or run
  certbot in a sidecar container and bind-mount certificates into the
  nginx container.

Either way, set `APP_ENV=production` (default) so secure-cookie and
HSTS protections are on.

## Backups

Once data matters, follow
[`docs/ops/backup-considerations.md`](../../docs/ops/backup-considerations.md)
for a `pg_basebackup` + WAL-archive recipe and a tested restore drill.

## Troubleshooting

**`velox-api` exits with `VELOX_ENCRYPTION_KEY is required in production`** —
set the key in `.env` (64 hex chars, generate with `openssl rand -hex 32`)
and `docker compose up -d` again. Production refuses to start without it
to avoid silently storing PII in plaintext.

**`velox-api` exits with `APP_DATABASE_URL is required in production`
or `default/guessable password`** — set `VELOX_APP_DB_PASSWORD` in
`.env` (compose builds `APP_DATABASE_URL` from it) and bring the stack
back up. Production refuses to run the request path on the admin role
or on the publicly documented default password.

**`velox-api` exits with `could not open the app database connection`** —
the `velox_app` role is missing or its password doesn't match
`VELOX_APP_DB_PASSWORD`. `postgres-init.sh` only runs on a FRESH
`pgdata` volume. On an existing volume, create or rotate the role
manually (psql's `:'pw'` quoting keeps any password intact):

```bash
docker compose exec -e VELOX_APP_DB_PASSWORD postgres \
  psql -U velox -d velox -v pw="$VELOX_APP_DB_PASSWORD" \
  -c "ALTER ROLE velox_app PASSWORD :'pw'"
# role doesn't exist yet? CREATE it instead:
#   -c "CREATE ROLE velox_app WITH LOGIN PASSWORD :'pw'" \
#   -c "GRANT ALL PRIVILEGES ON DATABASE velox TO velox_app" \
#   -c "GRANT ALL ON SCHEMA public TO velox_app" \
#   -c "ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON TABLES TO velox_app" \
#   -c "ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON SEQUENCES TO velox_app"
```

**`velox-api` exits with `role can BYPASS row-level security`** —
`APP_DATABASE_URL` points at the admin/superuser role (usually a
copied `DATABASE_URL`). Point it at `velox_app`.

**`/health/ready` returns 503 with `scheduler: degraded`** — the
scheduler tick window has elapsed without a recorded run. Usually this
means the API process is alive but the leader-locked background work
(jobs guarded by a Postgres advisory lock) isn't progressing. Check
the API logs and the `pg_locks` table.

**Port 80 is already in use** — set `NGINX_HTTP_PORT=8080` (or any free
port) in `.env` and bring the stack back up.

## What's next

- [`docs/self-host.md`](../../docs/self-host.md) — top-level self-host landing
- [`docs/ops/backup-considerations.md`](../../docs/ops/backup-considerations.md) — backup + restore drill

Kubernetes (Helm) and Terraform install paths are deliberately not
shipped in v1 — they land when a design partner names the flavour they
actually run.
