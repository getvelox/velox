# Back up Velox

Back up two things: the Postgres database and the encryption key. This page is for the engineer who runs Velox. It shows how to back up with the scripts in this repository, how to restore, and how to prove a restore works.

Think of the encryption key as the key to a safe, and the backup as the safe. A backup without its key cannot be opened, so the two must be kept apart, and both must be kept.

On the one-VM Docker Compose install, follow [Production on one VM, step 7](../production-one-vm.md#7-back-up-every-night): it runs these scripts in a container, every night.

## 1. Keep the encryption key

`VELOX_ENCRYPTION_KEY` encrypts personal data (email, names, phone numbers, tax IDs), webhook signing secrets and each account's Stripe keys before Velox saves them. This is on top of any disk encryption your Postgres has.

- Store the key in a secrets manager (such as AWS Secrets Manager, GCP Secret Manager or HashiCorp Vault), apart from the database backups.
- Without the key, every encrypted column in a backup is unreadable. The rest of the backup cannot replace it.
- Keep the same key. Velox has no tool to re-encrypt data with a new key, so changing it makes the old data unreadable.

## 2. Pick how to back up the database

| Method | When to use it | What you can lose |
|---|---|---|
| **Point-in-time restore (PITR)**, recommended | Managed Postgres turns it on with a setting (RDS automated backups, Cloud SQL, Aurora). On your own Postgres, use pgbackrest or WAL-G. | Almost nothing: you can restore to any second within the retention period |
| **Nightly dump** with `scripts/backup.sh` | One VM, or as a second copy you can restore anywhere | Everything written since the last dump |

Avoid these:

- **Dumping table by table.** Velox adds tables often, and a list of tables goes out of date.
- **Disk snapshots without the write-ahead log (WAL).** You lose everything since the last snapshot. Hourly snapshots still lose about 30 minutes on average.

On managed Postgres, the provider's snapshots and PITR are your main backup. Still run the [restore drill](#5-run-the-restore-drill) every few months: only the drill proves that Velox's money tables come back whole, and a dump shows your billing data is not tied to one cloud.

## 3. Take a backup

`scripts/backup.sh` dumps the database with `pg_dump`, checks that the dump can be read, can upload it to S3, and deletes local dumps older than the retention period. It needs `pg_dump` and `pg_restore` on the machine that runs it.

1. Run it with the admin database URL.

   ```bash
   DATABASE_URL="postgres://velox:PASSWORD@db-host:5432/velox?sslmode=require" \
     ./scripts/backup.sh
   ```

   **You see:** timestamped lines from `Starting backup of database 'velox'...` to `Verification passed: archive contains N table entries.`, then a "Backup Summary" block with `Verified:  yes`.

2. Check the file.

   ```bash
   ls -l /var/backups/velox/
   ```

   **You see:** `velox_<date>_<time>.dump`, and `latest.dump` pointing at the newest one.

Options, all set as environment variables:

| Variable | Default | What it does |
|---|---|---|
| `BACKUP_DIR` | `/var/backups/velox` | Where dumps are written |
| `BACKUP_S3_BUCKET` | unset | Also upload each dump to this S3 bucket. Needs the `aws` command; without it, the upload is skipped with a warning. |
| `BACKUP_S3_PREFIX` | `logical-backups` | The folder inside the bucket |
| `BACKUP_RETENTION_DAYS` | `30` | Local dumps older than this are deleted |
| `PG_DUMP` | `pg_dump` | A different `pg_dump` to run, for example one in a container |

Exit codes: `0` success, `1` a setting is wrong, `2` `pg_dump` failed, `3` the dump could not be read back, `4` the S3 upload failed.

**Use tools that match the server.** A `pg_dump` newer than the Postgres server writes a dump that server cannot restore, so the script stops. Run a matching one in a container:

```bash
PG_DUMP="docker run --rm -i --network host postgres:16-alpine pg_dump" ./scripts/backup.sh
```

The dump travels through standard output, so the container needs no mounted folders.

## 4. Restore a backup

`scripts/restore.sh` replaces the objects in the backup, in the target database, in one transaction: either all of it is restored, or nothing changes. It needs `pg_restore` and `psql` on the machine that runs it.

**Restore into an empty database when you can.** The script leaves alone what the backup does not hold. Restored over a newer database, that includes its migration record and any table a newer migration created. A backup from v0.8.x must go into an empty database: over a database that v0.9.0 or newer has upgraded, Velox refuses to start (see [Two migration records](runbook/migration-failed-midway.md#two-migration-records)). A backup from before v0.8.0 goes into an empty database, then through v0.8.1's `velox migrate`, then the new release's.

1. On a new Postgres server, create the limited user first. Users belong to the server, not to the database, so a dump cannot carry them. Without it the restore fails, and the script stops before it starts.

   ```sql
   CREATE ROLE velox_app LOGIN PASSWORD '<your-password>';
   ```

2. Run the restore against the target database.

   ```bash
   DATABASE_URL="postgres://velox:PASSWORD@db-host:5432/velox?sslmode=require" \
     ./scripts/restore.sh /var/backups/velox/latest.dump
   ```

   **You see:** a warning box naming the target and the backup, then `Type 'yes' to proceed:`. Type `yes`. To skip the question in a script, set `VELOX_RESTORE_CONFIRM=yes`.

3. Wait for it to finish.

   **You see:** `All critical tables present.`, a "Restore Summary" block, and `Restore complete. Verify application behavior before resuming traffic.`

4. Run the [checks after a restore](#checks-after-a-restore), then start Velox.

Exit codes: `0` success, `1` a setting or the file is wrong, `2` the restore failed and was rolled back, `3` a critical table is missing after the restore.

The tool rule from step 3 applies here too: if `pg_restore` is newer than the target server, the script stops. Use `PG_RESTORE="docker run --rm -i --network host postgres:16-alpine pg_restore"`.

## 5. Run the restore drill

A backup counts only once you have restored it. `scripts/restore-drill.sh` does the whole loop on a throwaway Postgres container, without touching your database or your backup folder:

1. It counts the rows in five key tables of the source database.
2. It takes a backup with `backup.sh` into a temporary folder.
3. It starts a Postgres container with the same major version as the source, on port 15432.
4. It restores the backup there with `restore.sh`.
5. It counts the same rows again, compares them, and removes the container.

Run it every few months, and after every Postgres upgrade. It needs Docker, `pg_dump`, `pg_restore` and `psql` on the machine that runs it.

```bash
SOURCE_DATABASE_URL="postgres://velox:PASSWORD@db-host:5432/velox?sslmode=require" \
  ./scripts/restore-drill.sh
```

**You see:** `OK` and a count for each table (`tenants`, `customers`, `subscriptions`, `invoices`, `customer_credit_ledger`), then a "Drill Summary" block with `Result:       PASS` and the backup and restore times.

Each run adds one line to `~/.velox/drill.log`, so you can see how long backups and restores take over time. Compare the total with how long you can afford to be down.

| Variable | Default | What it does |
|---|---|---|
| `SOURCE_DATABASE_URL` | `DATABASE_URL` | The database to back up and compare |
| `DRILL_PG_IMAGE` | the source's major, else `postgres:16-alpine` | The Postgres image for the throwaway container |
| `DRILL_PG_PORT` | `15432` | The port of the throwaway container. The drill stops if it is in use. |
| `DRILL_LOG` | `~/.velox/drill.log` | Where the result lines go |
| `CRITICAL_TABLES` | the five tables above | The tables whose row counts must match |

Exit codes: `0` passed, `1` a setting is wrong, `2` the backup failed, `3` the restore failed, `4` row counts differ, `5` the container did not start.

After a drill passes, you can also start Velox against a restored copy, check that `/health/ready` answers 200, and run `go run ./cmd/velox-doctor` against it. It should report no problems.

## Checks after a restore

Run these before you let traffic back in.

### 1. The schema version matches

Run the `velox migrate status` of the release you will start, against the restored database.

**You see:** `state:        up to date, nothing to apply`. Another state lists what `velox migrate` will do, or, for `refused`, what to do first. A database restored from a v0.8.x backup says `ready to move`: `velox migrate` takes it over without changing a table.

The record itself, if you want to look:

```sql
SELECT max(version_id) FROM public.goose_db_version;
```

A backup from v0.8.x has `schema_migrations` instead: `SELECT version, dirty FROM schema_migrations;` must show `180 | f`.

### 2. Row-Level Security is on

```sql
SELECT schemaname, tablename, rowsecurity
FROM pg_tables WHERE schemaname = 'public' AND rowsecurity = true
ORDER BY tablename;
```

Row-Level Security (RLS) is the Postgres feature that keeps each account's rows apart. The list must include every account-scoped table, such as `customers` and `invoices`. A missing table means its data is not kept apart.

### 3. The money tables look right

```sql
SELECT tenant_id, count(*) FROM invoices GROUP BY tenant_id;
SELECT tenant_id, count(*) FROM credit_notes GROUP BY tenant_id;
SELECT tenant_id, sum(total_amount_cents) FROM invoices
  WHERE status = 'paid' AND created_at >= now() - interval '90 days'
  GROUP BY tenant_id;
```

Compare the numbers with what you saw before the incident, for example in your metrics.

### 4. Queued emails and webhooks

Velox queues outgoing emails and webhooks in two tables, `email_outbox` and `webhook_outbox`, and sends them later. After a restore, every `pending` row is sent again. If the backup is older than some sends, customers get those emails or webhooks twice.

The safe default: leave the rows `pending` and accept a few duplicates. Webhook receivers should already handle the same event twice.

If you are sure you want to skip them, mark them sent, and write down that you did:

```sql
UPDATE email_outbox SET status = 'dispatched', dispatched_at = now(),
  payload = payload - '{setup_url,update_url,password_reset_url,invite_url,public_token,pdf}'::text[]
  WHERE status = 'pending';
UPDATE webhook_outbox SET status = 'dispatched', dispatched_at = now() WHERE status = 'pending';
```

The `payload - '{…}'` part removes the links and the PDF from the email rows, so they are not kept for emails that were never sent.

### 5. Billing picks up where it stopped

Velox's background jobs resume by themselves after a restart, on one copy or on several:

- A subscription that was due but not billed is billed on the next billing run (hourly in production, every 5 minutes in local development).
- Invoices waiting for an automatic charge are charged.
- Payment retries (dunning) that are due run.

After the restart, these counts should fall to zero within a few minutes:

```sql
-- Subscriptions that should have billed
SELECT count(*) FROM subscriptions
  WHERE status IN ('active', 'trialing') AND next_billing_at < now();

-- Invoices waiting for an automatic charge
SELECT count(*) FROM invoices
  WHERE auto_charge_pending = true;

-- Payment retries that are due
SELECT count(*) FROM invoice_dunning_runs
  WHERE state = 'active' AND next_action_at < now();
```

If they do not fall, the background jobs are not running. See the [runbook](runbook/background-job-stuck.md).

### 6. Payments made during the gap

If the backup is hours older than the failure, Stripe events sent in between are missing. Velox's background check asks Stripe about invoices whose payment status is `unknown`, but it does not replay every event. List the invoices to check by hand:

```sql
SELECT id, payment_status, updated_at FROM invoices
  WHERE updated_at < now() - interval '4 hours'
    AND payment_status IN ('processing', 'unknown', 'pending');
```

Compare them with the Stripe dashboard, or resend the events from Stripe.

## What is in the database

Some tables matter more than others when you decide what a restore must bring back.

### Money and setup: never lose these

| Table | Why it matters |
|---|---|
| `tenants` | Every other row belongs to an account |
| `customers`, `customer_billing_profiles` | Who the customers are, with tax details and personal data |
| `subscriptions`, `subscription_items` | What each customer is billed for |
| `plans`, `meters`, `rating_rule_versions` | Prices. Invoice amounts depend on them. |
| `invoices`, `invoice_line_items` | Issued invoices: a financial record |
| `credit_notes`, `credit_note_line_items` | Refunds and adjustments: a financial record |
| `customer_credit_ledger` | Customer credit balances. Every entry is needed to work out a balance. |
| `payment_methods`, `customers.stripe_customer_id` | Saved cards, and the link to each Stripe customer |
| `dunning_policies` (and `customers.dunning_policy_id`) | Payment-retry policies, and which customer uses which |
| `invoice_dunning_runs`, `invoice_dunning_events` | Payment retries in progress. Losing them can charge a failed card again too soon. |
| `tax_calculations` | Stripe Tax records that tie an invoice to Stripe's tax transaction |
| `audit_log` | Who changed what. Audits such as SOC 2 often ask for 7 years. |
| `coupons`, `customer_discounts`, `coupon_redemptions` | Discounts. Coupons are no longer offered, and the tables remain. |
| `users`, `user_tenants`, `password_reset_tokens` | Dashboard users |
| `api_keys` | API keys |
| `tenant_settings`, `stripe_provider_credentials` | Account settings, including the encrypted Stripe keys |
| `webhook_endpoints` | Where Velox sends webhooks, with their signing secrets |
| `recipe_instances` | Installed pricing recipes |
| `test_clocks`, `dashboard_sessions` | Not money, but losing them disrupts the people using the dashboard |
| `goose_db_version` | The schema version. Velox does not start without it. |
| `schema_migrations` | On a database taken over from v0.8.x: v0.8's version record, frozen at 180. Keep it with `goose_db_version`; Velox checks that the two agree. |

`usage_events` for billing periods that are still open belong here too: the next invoice needs every one of them.

### Queues: can be dropped in an emergency

| Table | If lost | Why it can be rebuilt |
|---|---|---|
| `email_outbox` | Queued emails are not sent | Later changes send new emails. A lost email is an annoyance, not lost money. |
| `webhook_outbox` | Queued webhooks are not sent | Receivers should handle replays; send events again if needed |
| `webhook_events` (Stripe, incoming) | Some history is lost | Stripe holds the original events and its API returns them |
| `stripe_webhook_events` | Same as above | Same |

### Old usage: can be dropped

`usage_events` rows from billing periods that are already invoiced. Their totals are on the invoices; the rows are for analysis only.

### Not in the database

These do not need a database backup:

- Velox itself and its container images: download or build them again.
- Settings and secrets (the `.env` file or your secrets manager): back them up separately, together with the encryption key.
- Invoice PDFs: Velox builds them again from the invoice.

## Background

The first restore drill, on 2026-08-11, failed and found four defects in the scripts. All four were fixed the same day, and the drill then passed.

| Date | Result | Notes |
|---|---|---|
| 2026-08-11 | **FAIL → 4 defects → PASS** (7s total: backup 1s, restore 3s; 5 critical tables count-matched) | First-ever run. Found: (1) host `pg_dump` 18 vs server 16 → unrestorable archive; (2) host `pg_restore` 18 vs target 16 → client `SET transaction_timeout` rolled restore back; (3) fresh-cluster restore rolled back on missing `velox_app` role — the DR path had never worked; (4) restore.sh's validation query referenced a nonexistent column (`table_name` vs `relname`) — broken since birth. All four fixed same day; drill re-run green. |
