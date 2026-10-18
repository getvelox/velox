# Run Velox in production on one VM

This page takes a fresh Linux server to a Velox that bills real customers. It is for the engineer who installs Velox for a small team.

One VM is a supported production setup. Velox is down while it restarts, for example during an upgrade. When that is no longer acceptable, run two or more copies: see [High availability](ops/high-availability.md).

Tested on a €6.49/month Hetzner CX23; see [How much traffic Velox handles](scale.md#what-one-small-server-handles).

Think of it as getting a car ready for the road. The [install guide](../deploy/compose/README.md) gives you a car that runs. This page adds the number plates (a web address with HTTPS), the lights (email), the fuel (Stripe) and the spare wheel (backups).

At the end you have:

- Velox at `https://billing.example.com`, with a certificate that renews itself.
- Emails that reach your customers, with working links.
- Your Stripe account connected, so invoices are charged to the customer's card.
- A nightly backup, and a restore you have tested.

## Before you start

- A Linux VM with Docker 24 or newer and the Compose plugin. Check with `docker compose version`. To install both on Ubuntu, follow [Docker's install guide](https://docs.docker.com/engine/install/ubuntu/). Its convenience script, `get.docker.com`, is quickest on a fresh server, but Docker does not recommend it for production.
- A domain name for Velox, such as `billing.example.com`, with a DNS `A` record pointing at the VM.
- Ports 80 and 443 open to the internet. Port 80 is needed to get the HTTPS certificate.
- An account with an email provider that gives you SMTP settings, such as Postmark, SendGrid or AWS SES.
- A Stripe account.

Every example uses `billing.example.com`. Replace it with your own address.

## 1. Install a pinned release

A release is a tested version with a number, such as `v0.8.1`. Production runs a release, not the newest code: the newest code can hold changes that are not released yet. Find the newest release on the [releases page](https://github.com/getvelox/velox/releases). This page uses `v0.8.1`.

1. Get the files for that release.

   ```bash
   VELOX_VERSION=v0.8.1
   sudo git clone --branch "$VELOX_VERSION" https://github.com/getvelox/velox.git /opt/velox
   sudo chown -R "$USER" /opt/velox
   cd /opt/velox/deploy/compose
   cp .env.example .env
   ```

   **You see:** `Note: switching to '…'` from git. That is normal: you are on a release, not on a branch.

2. Generate the four secrets, pin the release images and keep port 80 free for HTTPS. These commands fill in the lines `.env` already has, so each setting appears once. Run them in the same shell as step 1, which set `VELOX_VERSION`.

   ```bash
   sed -i \
     -e "s|^POSTGRES_PASSWORD=.*|POSTGRES_PASSWORD=$(openssl rand -hex 24)|" \
     -e "s|^VELOX_APP_DB_PASSWORD=.*|VELOX_APP_DB_PASSWORD=$(openssl rand -hex 24)|" \
     -e "s|^VELOX_ENCRYPTION_KEY=.*|VELOX_ENCRYPTION_KEY=$(openssl rand -hex 32)|" \
     -e "s|^VELOX_BOOTSTRAP_TOKEN=.*|VELOX_BOOTSTRAP_TOKEN=$(openssl rand -hex 32)|" \
     -e "s|^# VELOX_IMAGE=.*|VELOX_IMAGE=ghcr.io/getvelox/velox:${VELOX_VERSION}|" \
     -e "s|^# VELOX_DASHBOARD_IMAGE=.*|VELOX_DASHBOARD_IMAGE=ghcr.io/getvelox/velox-dashboard:${VELOX_VERSION}|" \
     -e "s|^NGINX_HTTP_PORT=.*|NGINX_HTTP_PORT=127.0.0.1:8080|" \
     .env
   chmod 600 .env
   grep -cE '^(POSTGRES_PASSWORD|VELOX_APP_DB_PASSWORD|VELOX_ENCRYPTION_KEY|VELOX_BOOTSTRAP_TOKEN|VELOX_IMAGE|VELOX_DASHBOARD_IMAGE)=.+' .env
   grep NGINX_HTTP_PORT .env
   ```

   **You see:** `6`, then `NGINX_HTTP_PORT=127.0.0.1:8080`.

   What each line is for:

   | Line | What it is for |
   |---|---|
   | `POSTGRES_PASSWORD` | The database admin password |
   | `VELOX_APP_DB_PASSWORD` | The password of the limited database user that API requests run as |
   | `VELOX_ENCRYPTION_KEY` | Encrypts stored personal data and Stripe keys. Without it, a backup cannot be read. |
   | `VELOX_BOOTSTRAP_TOKEN` | Lets you create the first account in step 2 |
   | `VELOX_IMAGE`, `VELOX_DASHBOARD_IMAGE` | Run the published release images instead of building from source |
   | `NGINX_HTTP_PORT=127.0.0.1:8080` | Velox listens only on the VM itself, on port 8080. The HTTPS server in step 3 sits in front of it. |

3. Start Velox.

   ```bash
   docker compose up -d
   docker compose exec velox-api /usr/local/bin/velox version
   ```

   **You see:** five containers start, then `velox v0.8.1`.

4. Check that it is healthy. On the first start Velox sets up its database (runs its migrations), which takes about 10 seconds. Until then the check fails with a 502, so this command retries every 2 seconds and gives up after 2 minutes.

   ```bash
   timeout 120 bash -c \
     'until curl -fsS http://localhost:8080/health/ready; do sleep 2; done'
   ```

   **You see:** perhaps a few `curl: (22) The requested URL returned error: 502` lines, then `{"checks":{"api":"ok","database":"ok","scheduler":"..."},"status":"ok"}`. If the command ends with no `"status":"ok"`, see the fixes linked below.

If a step fails, the [install guide](../deploy/compose/README.md#if-something-goes-wrong) lists each start-up error and its fix.

## 2. Create your account and first login

An account (a "tenant" in the API) is one business, with its own data, dashboard users and API keys.

Run this on the VM. The reply holds your password and a live API key, so it must not cross the network before HTTPS is set up.

```bash
set -a; . ./.env; set +a
curl -X POST http://localhost:8080/v1/bootstrap \
  -H "Authorization: Bearer ${VELOX_BOOTSTRAP_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{"tenant_name":"Acme","owner_email":"you@example.com","owner_password":"a-strong-password-12ch"}'
```

**You see:** JSON with `owner_email`, `owner_password`, `secret_key_test` (`vlx_secret_test_…`), `secret_key_live` (`vlx_secret_live_…`) and `publishable_key_test`.

Save it in your password manager now. It is shown once, and Velox keeps only hashes, so it cannot be shown again. A second call answers `409 already_bootstrapped`.

## 3. Add HTTPS with Caddy

Caddy is a web server that gets a free HTTPS certificate from Let's Encrypt and renews it by itself. Any HTTPS proxy works. This page uses Caddy because it needs no certificate setup.

1. Install Caddy on the VM: follow [caddyserver.com/docs/install](https://caddyserver.com/docs/install) for your Linux.
2. Replace `/etc/caddy/Caddyfile` with these three lines:

   ```text
   billing.example.com {
       reverse_proxy 127.0.0.1:8080
   }
   ```

3. Reload Caddy.

   ```bash
   sudo systemctl reload caddy
   curl -fsS https://billing.example.com/health
   ```

   **You see:** `{"status":"ok"}`. The first request can take a few seconds while Caddy gets the certificate.

4. Open `https://billing.example.com` in your browser and sign in with the login from step 2.

   **You see:** the dashboard, in test mode. Nothing in test mode charges real money.

`APP_ENV=production` is already set in `.env`. It marks the sign-in cookie as HTTPS-only, which is why sign-in works only at your `https://` address or on `localhost`.

Velox reads each visitor's real address from the headers Caddy and nginx forward. Caddy's requests reach Velox through Docker's private network, which the Compose stack's default `TRUST_PROXY` already trusts, so leave that setting as it is.

## 4. Set the web addresses

Emails and Stripe send people to pages in Velox: your customers to the invoice page and the page to add a card, and your team to the password-reset page. Velox needs your address to build those links. It never guesses the address from a request.

1. Add four lines to `.env`. With this setup the dashboard and the customer pages share one address.

   ```bash
   cat >> .env <<'EOF'
   HOSTED_INVOICE_BASE_URL=https://billing.example.com
   PAYMENT_UPDATE_URL=https://billing.example.com/update-payment
   DASHBOARD_BASE_URL=https://billing.example.com
   CUSTOMER_PORTAL_URL=https://billing.example.com
   EOF
   docker compose up -d
   ```

   **You see:** `velox-api` is recreated.

   | Setting | What breaks without it |
   |---|---|
   | `HOSTED_INVOICE_BASE_URL` | Invoice, receipt and reminder emails go out without the link to view and pay the invoice. |
   | `PAYMENT_UPDATE_URL` | Emails asking a customer to add a card are not sent. |
   | `DASHBOARD_BASE_URL` | Password-reset and team-invite emails are not sent. |
   | `CUSTOMER_PORTAL_URL` | After a customer saves a card on Stripe's page, Stripe sends them to `http://localhost:5173`, which does not exist on your server. The card is still saved. |

2. Check the start-up log.

   ```bash
   docker compose logs velox-api \
     | grep -E "(HOSTED_INVOICE_BASE_URL|PAYMENT_UPDATE_URL|DASHBOARD_BASE_URL|CUSTOMER_PORTAL_URL) NOT SET"
   ```

   **You see:** nothing. Each of these settings that is still missing logs a warning that starts with its name and `NOT SET`.

### Other start-up warnings

Velox logs a few more warnings when it starts. To list them all:

```bash
docker compose logs velox-api | grep '"level":"WARN"'
```

| Warning starts with | What it means | Do you need to act? |
|---|---|---|
| `SMTP NOT CONFIGURED` | No email is sent. | Yes. [Step 5](#5-connect-email) fixes it. |
| `VELOX_EMAIL_BIDX_KEY not set` | Velox cannot find a customer by email address, so it cannot stop emailing an address that bounced. | Yes, before your first customer. See below. |
| `POSTMARK_WEBHOOK_USER/POSTMARK_WEBHOOK_PASS not set` | Velox turns away Postmark's reports of late bounces. | Only if you send email through Postmark. See [Email setup](ops/email-setup.md#when-postmark-reports-a-bounce-later). |
| `METRICS_TOKEN not set` | `/metrics` answers 401 to everyone. | Only if you collect metrics with Prometheus. See [What to watch](self-host.md#what-to-watch). |
| `VELOX_API_BASE_URL NOT SET` | The link you copy to share a customer's cost dashboard (a page of their usage and charges) is a path, not a full address. | Only if you share cost dashboards. Set it in `.env` to your HTTPS address. |
| `RUN_MIGRATIONS_ON_BOOT=true in production` | Velox updates its database as it starts. | No. On one VM this is how upgrades apply. The warning is for setups with several copies of Velox. |

To set `VELOX_EMAIL_BIDX_KEY`, add a random key and restart. Set it before you add customers: Velox indexes a customer's email only when the customer is created or edited.

```bash
echo "VELOX_EMAIL_BIDX_KEY=$(openssl rand -hex 32)" >> .env
docker compose up -d
sleep 10
docker compose logs velox-api | grep "email blind index enabled"
```

**You see:** a line with `email blind index enabled`. Do not change this key later: a new key does not match the customers indexed with the old one.

## 5. Connect email

Velox sends invoices, receipts, payment reminders, password resets and team invites through your email provider's SMTP server.

1. Add your provider's SMTP settings to `.env`. Use an address on a domain you have set up with your provider for `SMTP_FROM`.

   ```bash
   cat >> .env <<'EOF'
   SMTP_HOST=smtp.example.com
   SMTP_PORT=587
   SMTP_TLS=starttls
   SMTP_USERNAME=your-smtp-user
   SMTP_PASSWORD=your-smtp-password
   SMTP_FROM=billing@example.com
   EOF
   docker compose up -d
   ```

   **You see:** `velox-api` is recreated. `docker compose logs velox-api | grep "SMTP configured"` shows your host.

2. Send yourself a password-reset email. Sign out, click **Forgot password?** on the sign-in page and enter your email.

   **You see:** the reset email arrives within a few minutes, and its link opens `https://billing.example.com/reset-password`. If it does not arrive, check spam, then `docker compose logs velox-api`.

Releases from v0.8.0 on also have **Settings → Email**. It shows whether each email setting is in place and has a **Send test email** button.

Settings for each provider, sender-domain setup (SPF and DKIM) and bounce handling are in [Email setup](ops/email-setup.md).

## 6. Connect Stripe

Velox works out each bill and makes the invoice. Stripe charges the card. You connect your own Stripe account in the dashboard, so the money goes straight to you. There is no Stripe setting in `.env`.

Start in test mode. Connect live mode the same way when you are ready to take real money.

1. In Stripe, open **Developers → API keys** and copy the secret key (`sk_test_…`) and the publishable key (`pk_test_…`).
2. In Velox, open **Settings → Payments**. Under step 1, **Connect your Stripe API keys**, paste both keys and save.

   **You see:** "Verified as" and your Stripe account name.

3. Step 2, **Deliver Stripe events to Velox**, shows a webhook URL. A webhook is a message Stripe sends to Velox when something happens, for example when a card payment succeeds. Copy the URL. It looks like `https://billing.example.com/v1/webhooks/stripe/vlx_spc_…`.
4. In Stripe, open **Developers → Webhooks** and add an endpoint. Paste the URL and pick the events that Velox lists under the URL.
5. Stripe shows the endpoint's signing secret (`whsec_…`). Paste it into step 2 in Velox and click **Finish setup**.

   **You see:** the card's badge reads **Connected**.

Without step 5, Stripe's page tells a customer their card is saved, but Velox never hears about it.

To check a whole charge in test mode, from saving a card to a refund, follow [Stripe end-to-end test](dev/stripe-end-to-end-test.md).

## 7. Back up every night

Two things must be backed up, and kept apart:

- **The database.** It holds every customer, invoice and payment.
- **The `.env` file.** It holds `VELOX_ENCRYPTION_KEY`. Without that key, the encrypted parts of a backup (customer details, Stripe keys) cannot be read. Copy `.env` to your password manager or a secrets store now, not next to the backups.

The repository ships `scripts/backup.sh`. It dumps the database, checks that the dump can be read, and deletes dumps older than 30 days. The commands below run it in a Postgres 16 container, so the VM needs no Postgres tools.

1. Create the backup command.

   ```bash
   sudo mkdir -p /var/backups/velox
   sudo chmod 700 /var/backups/velox
   sudo tee /usr/local/bin/velox-backup >/dev/null <<'EOF'
   #!/usr/bin/env bash
   set -euo pipefail
   cd /opt/velox/deploy/compose
   set -a; . ./.env; set +a
   docker run --rm \
     --network "container:$(docker compose ps -q postgres)" \
     -v /opt/velox/scripts:/scripts:ro \
     -v /var/backups/velox:/var/backups/velox \
     -e DATABASE_URL="postgres://velox:${POSTGRES_PASSWORD}@localhost:5432/velox?sslmode=disable" \
     postgres:16 bash /scripts/backup.sh
   EOF
   sudo chmod 755 /usr/local/bin/velox-backup
   ```

   `--network "container:…"` puts the backup container next to the database, which has no port open on the VM. `chmod 700` lets only root open the backup folder, because the dumps hold every customer's data.

2. Run it once.

   ```bash
   sudo velox-backup
   ```

   **You see:** log lines ending with a "Backup Summary" block that says `Verified:  yes`. The dump is in `/var/backups/velox`, and `latest.dump` points at the newest one.

3. Run it every night at 03:00.

   ```bash
   echo '0 3 * * * root /usr/local/bin/velox-backup >> /var/log/velox-backup.log 2>&1' \
     | sudo tee /etc/cron.d/velox-backup
   ```

   **You see:** the line echoed back. Tomorrow, `/var/log/velox-backup.log` has the first run.

4. Copy `/var/backups/velox` off the VM, for example with your cloud's object storage tool. A backup on the same disk is lost with the VM.

### Test a restore

A backup counts only once you have restored it. This test restores the newest dump into a second, empty database on the same server. Your live data is not touched.

1. Restore into a test database.

   ```bash
   cd /opt/velox/deploy/compose
   set -a; . ./.env; set +a
   docker compose exec -T postgres createdb -U velox velox_restore_test
   docker run --rm \
     --network "container:$(docker compose ps -q postgres)" \
     -v /opt/velox/scripts:/scripts:ro \
     -v /var/backups/velox:/var/backups/velox:ro \
     -e DATABASE_URL="postgres://velox:${POSTGRES_PASSWORD}@localhost:5432/velox_restore_test?sslmode=disable" \
     -e VELOX_RESTORE_CONFIRM=yes \
     postgres:16 bash /scripts/restore.sh /var/backups/velox/latest.dump
   ```

   **You see:** `All critical tables present.` and then `Restore complete.`

2. Compare the restored data with the live data.

   ```bash
   for db in velox velox_restore_test; do
     docker compose exec -T postgres psql -U velox -d "$db" -tAc \
       "SELECT count(*) FROM invoices"
   done
   ```

   **You see:** the same number twice, or a slightly higher first number if invoices were made since the backup.

3. Delete the test database.

   ```bash
   docker compose exec -T postgres dropdb -U velox velox_restore_test
   ```

Repeat this test every few months, and after every upgrade. More on what to back up, and on point-in-time restore: [Backups](ops/backup-considerations.md).

## You are live when

Check each line before the first real customer.

- [ ] `docker compose exec velox-api /usr/local/bin/velox version` shows a release, not `dev`.
- [ ] `https://billing.example.com/health/ready` answers `"status":"ok"`.
- [ ] You can sign in at `https://billing.example.com`.
- [ ] A password-reset email arrived, and its link opens your `https://` address.
- [ ] **Settings → Payments** reads **Connected** in test mode, and a test charge worked ([Stripe end-to-end test](dev/stripe-end-to-end-test.md)).
- [ ] The **Dunning policies** page shows the policy you want as the default. Dunning means retrying a failed card payment and emailing the customer. A new account starts with "Default retries": three retries over two weeks, then the subscription keeps billing and the invoice stays open. Edit it, or create your own, if you want collection paused or the invoice written off instead.
- [ ] `/var/log/velox-backup.log` shows a nightly backup, a copy is off the VM, and a restore test passed.
- [ ] `.env` is stored somewhere safe, apart from the backups.
- [ ] If you collect metrics: `METRICS_TOKEN` is set, and Prometheus sends it. See [What to watch](self-host.md#what-to-watch).
- [ ] For real money: switch to live mode with the Test/Live toggle in the sidebar, then connect your live Stripe keys and live webhook under **Settings → Payments** the same way. Your app uses the live API key, `vlx_secret_live_…`.

## Upgrade to a new release

Velox is down while the new version starts. Database changes (migrations) run by themselves as it starts.

1. Read the upgrade notes in the [changelog](../CHANGELOG.md) for every release between yours and the new one.
2. Take a backup.

   ```bash
   sudo velox-backup
   ```

   **You see:** `Verified:  yes`.

3. Get the new release's files and point `.env` at its images. This example moves to `v0.9.0`.

   ```bash
   VELOX_VERSION=v0.9.0
   cd /opt/velox
   git fetch --tags
   git checkout "$VELOX_VERSION"
   cd deploy/compose
   sed -i "s|^VELOX_IMAGE=.*|VELOX_IMAGE=ghcr.io/getvelox/velox:${VELOX_VERSION}|; s|^VELOX_DASHBOARD_IMAGE=.*|VELOX_DASHBOARD_IMAGE=ghcr.io/getvelox/velox-dashboard:${VELOX_VERSION}|" .env
   ```

   **You see:** `HEAD is now at …` from git.

4. Pull the new images and see what the upgrade will change. `migrate status` only reads.

   ```bash
   docker compose pull
   docker compose run --rm velox-api migrate status
   ```

   **You see:** a few `Container … Running` lines, then the plan. From v0.8.x to v0.9.0 it says `state:        ready to move: the next "velox migrate" records 180 in goose_db_version and changes no table`. For a later release it lists the migrations to apply. If it says `refused`, stop here and follow the message.

5. Start the new images.

   ```bash
   docker compose up -d
   docker compose exec velox-api /usr/local/bin/velox version
   ```

   **You see:** `velox v0.9.0`, and `curl -fsS http://localhost:8080/health/ready` answers `"status":"ok"` again.

If the upgrade stops with an error, nothing needs repair: a failed migration is undone. Follow [A database upgrade stopped](ops/runbook/migration-failed-midway.md) to find the cause, then start the API again.

If it says `waited 5s for a table lock`, something held a lock on a table the upgrade changes, often a long-running query. Start the API again, or see [An upgrade waited for a table lock](ops/runbook/migration-lock-timeout.md).

### Going back

Never roll the database back with `velox migrate rollback`: it runs only with `APP_ENV=local`, because it loses data the newer release wrote. To go back to the old release:

- **Restore the backup from step 2 into an empty database**, then start the old release. See [Restore a backup](ops/backup-considerations.md#4-restore-a-backup).
- **From v0.9.0 to v0.8.x only, when v0.9.0 applied no migration after 180,** you can also start the old images as they are: v0.8.x reads its own record, still at 180, and changes nothing. `velox migrate status` (with v0.9.0) shows `database:     180` when this applies.

Do not run v0.8.x's `migrate rollback` or `migrate force` on a database v0.9.0 took over. If one runs, v0.9.0 refuses to start until the records agree again; see [Two migration records](ops/runbook/migration-failed-midway.md#two-migration-records).

## Related

- [Install on one VM](../deploy/compose/README.md): each install step explained, and fixes for start-up errors.
- [Self-host](self-host.md): every setting, Postgres, and what to monitor.
