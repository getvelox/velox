# Install Velox on one VM with Docker Compose

This guide takes you from a fresh VM to a working Velox with a dashboard login and API keys.

Just trying Velox on your own machine? Run `./quickstart.sh` in this directory instead. It does steps 1 to 4 for you, on port 8080, with the published images. This guide explains each of those steps, for a server you will keep.

Going to production? [Production on one VM](../../docs/production-one-vm.md) does the same install with a pinned release, then adds HTTPS, email, Stripe, nightly backups and a go-live checklist.

You get five containers: Postgres, Redis, the Velox API, the dashboard, and nginx in front of them on port 80.

## Before you start

- Docker 24 or newer, with the Compose plugin. Check with `docker compose version`.
- About 1 vCPU and 1 GB of RAM is enough to try it.
- Port 80 open, if you want to reach the API from outside the VM.

## 1. Get the code and set four secrets

```bash
git clone https://github.com/getvelox/velox.git
cd velox/deploy/compose
cp .env.example .env
```

Generate the four secrets and add them to `.env`:

```bash
cat >> .env <<EOF
POSTGRES_PASSWORD=$(openssl rand -hex 24)
VELOX_APP_DB_PASSWORD=$(openssl rand -hex 24)
VELOX_ENCRYPTION_KEY=$(openssl rand -hex 32)
VELOX_BOOTSTRAP_TOKEN=$(openssl rand -hex 32)
EOF
```

| Secret | What it is for |
|---|---|
| `POSTGRES_PASSWORD` | The database admin password |
| `VELOX_APP_DB_PASSWORD` | The password of the limited database user that API requests run as |
| `VELOX_ENCRYPTION_KEY` | Encrypts stored personal data. Must be 64 hex characters. |
| `VELOX_BOOTSTRAP_TOKEN` | Lets you create the first tenant in step 4. At least 16 characters. |

Everything else in `.env` is optional. Velox starts without email or Stripe set up.

## 2. Start it

```bash
docker compose up -d
```

The first start builds the two Velox images from source, which takes a few minutes. Then:

1. Postgres creates the `velox` database and the limited `velox_app` user.
2. The API applies the database schema, checks that `velox_app` cannot see across tenants, and starts listening.
3. nginx forwards port 80 to the API and the dashboard.

Watch it come up:

```bash
docker compose logs -f velox-api
```

It is ready when you see `"listening" addr=:8080`.

## 3. Check it is healthy

```bash
curl -fsS http://localhost/health
# {"status":"ok"}

curl -fsS http://localhost/health/ready
# {"checks":{"api":"ok","database":"ok","scheduler":"..."},"status":"ok"}
```

- `/health` says the process is running.
- `/health/ready` says it can reach the database. Point a load balancer's health check here. Its body also reports whether background jobs have run recently; that does not change the status code.

## 4. Create your first tenant

A tenant is one business account, with its own data, dashboard users and API keys.

Run this on the VM itself. The reply contains your password and a live API key, and this stack has no TLS yet, so do not send it across a network.

```bash
set -a; . ./.env; set +a
curl -X POST http://localhost/v1/bootstrap \
  -H "Authorization: Bearer ${VELOX_BOOTSTRAP_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{"tenant_name":"Acme","owner_email":"you@example.com","owner_password":"a-strong-password-12ch"}'
```

The email and password are optional. Without them the owner is `admin@velox.local`, with a generated password. A password needs at least 12 characters.

**Save the reply. It is shown once.** Velox stores only hashes, so none of it can be recovered later. It contains:

- your dashboard login (`owner_email` and `owner_password`),
- a test secret key (`vlx_secret_test_…`),
- a live secret key (`vlx_secret_live_…`). This one charges real cards. Leave it alone until you mean it.
- a test publishable key.

Calling it a second time returns `409 already_bootstrapped`.

## 5. Sign in to the dashboard

Open `http://localhost/` on the VM and sign in with the owner login.

**From your laptop, use an SSH tunnel:**

```bash
ssh -L 8080:localhost:80 you@server
# then open http://localhost:8080/
```

**Sign-in does not work at `http://<server-ip>/`.** Browsers keep the sign-in cookie only on HTTPS or on localhost, and the sign-in page tells you so. Once you add [TLS](#tls), sign in at your HTTPS address.

## 6. Make a first API call

```bash
SECRET=vlx_secret_test_...
curl -fsS http://localhost/v1/customers \
  -H "Authorization: Bearer ${SECRET}"
# {"data":[],"total":0}
```

Velox is running. To see a full flow, from usage to an invoice, run the demo from the repository root:

```bash
VELOX_BASE=http://localhost VELOX_DASHBOARD_URL=http://localhost ./scripts/demo.sh vlx_secret_test_...
```

`VELOX_DASHBOARD_URL` makes the dashboard links the demo prints point at this stack. Without it they point at `http://localhost:5173`, the development server.

## Day-to-day commands

| To | Run |
|---|---|
| See logs | `docker compose logs -f` |
| Restart the API | `docker compose restart velox-api` |
| Upgrade, built from source | `git pull`, then `docker compose up -d --build` |
| Upgrade, published release image | See [Upgrade to a new release](../../docs/production-one-vm.md#upgrade-to-a-new-release) |
| Stop, keeping data | `docker compose stop` |
| Stop and delete all data | `docker compose down -v` |
| See the database's version and the upgrades still to apply | `docker compose run --rm velox-api migrate status` |
| Open a database shell | `docker compose exec postgres psql -U velox -d velox` |

Read the upgrade notes in the [changelog](../../CHANGELOG.md) before upgrading.

## TLS

This stack serves plain HTTP. Before real use, put TLS in front of it. Two common ways:

- **A managed load balancer**, such as AWS ALB or Cloudflare. It handles TLS and forwards to port 80 on this VM over a private network.
- **nginx and certbot on the VM**, outside this stack. It handles TLS and forwards to `localhost:80`.

[Production on one VM](../../docs/production-one-vm.md#3-add-https-with-caddy) has a copy-paste example with Caddy, a web server that gets and renews the certificate by itself.

Keep `APP_ENV=production`, the default. It marks cookies as secure and turns on HSTS.

## Backups

Once the data matters, set up backups: [`docs/ops/backup-considerations.md`](../../docs/ops/backup-considerations.md). It includes backup and restore scripts and a restore drill.

## If something goes wrong

**The API exits with `VELOX_ENCRYPTION_KEY is required in production`.**
Set the key in `.env` (`openssl rand -hex 32`) and run `docker compose up -d` again.

**The API exits with `APP_DATABASE_URL is required in production` or `default/guessable password`.**
Set `VELOX_APP_DB_PASSWORD` in `.env` and start the stack again. Compose builds `APP_DATABASE_URL` from it.

**The API exits with `could not open the app database connection`.**
The `velox_app` user is missing, or its password differs from `VELOX_APP_DB_PASSWORD`. Postgres creates that user only on its very first start, on an empty data volume. On an existing volume, set the password yourself:

```bash
docker compose exec -e VELOX_APP_DB_PASSWORD postgres \
  psql -U velox -d velox -v pw="$VELOX_APP_DB_PASSWORD" \
  -c "ALTER ROLE velox_app PASSWORD :'pw'"
```

If the user does not exist at all, create it with the SQL in [Self-host, "The two database users"](../../docs/self-host.md#the-two-database-users).

**The API exits with `role can BYPASS row-level security`.**
`APP_DATABASE_URL` points at the admin user, usually because `DATABASE_URL` was copied into it. Point it at `velox_app`.

**The API exits with `migration … failed`, or names `schema_migrations` or `goose_db_version`.**
A database upgrade stopped. A failed migration leaves nothing to repair: fix the cause the error names, then start the API again. `docker compose run --rm velox-api migrate status` shows the plan. The cases are in [A database upgrade stopped](../../docs/ops/runbook/migration-failed-midway.md).

**`/health/ready` shows `scheduler: degraded`.**
The API is up and still answers 200 here, but its background jobs have stopped running. Check the API logs. To see which copy last ran each job, run `SELECT * FROM leader_status;` in the database shell.

**`/metrics` returns 401.**
`METRICS_TOKEN` is not set, or the scraper did not send it. Set it in `.env` (`openssl rand -hex 32`), start the stack again, and have Prometheus send it as a bearer token.

**Port 80 is already in use.**
Set `NGINX_HTTP_PORT=8080`, or any free port, in `.env` and start the stack again.

**Sign-in says it does not work at this address.**
You opened the dashboard over plain HTTP at an address other than localhost. See [step 5](#5-sign-in-to-the-dashboard).

## Next

- [Production on one VM](../../docs/production-one-vm.md): HTTPS, email, Stripe, backups and a go-live checklist.
- [Self-host](../../docs/self-host.md): settings, Postgres, production setup, and what to monitor.
- [Backups](../../docs/ops/backup-considerations.md).

There is no Helm chart or Terraform module yet.
