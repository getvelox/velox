# Backup Considerations

Velox-specific backup semantics. The mechanics of taking and restoring
a Postgres backup are your DBA team's call (pgbackrest, WAL-G, RDS
PITR, postgres-operator backups — pick what fits your infra). This
doc names what you need to know **about Velox** to take a useful
backup and validate a successful restore.

## What's financial vs transient

Not every table in Velox carries equal weight. A backup that loses
some tables is recoverable; losing others is a financial liability.
Categorise by tier:

### Tier 1 — Financial (never lose these)

Loss of any row in these tables is unrecoverable financial harm.
Backup MUST include them; restore MUST verify they match.

| Table | Why critical |
|---|---|
| `tenants` | Top-level isolation key; every other row references this |
| `customers`, `customer_billing_profiles` | Customer identity + tax/PII |
| `subscriptions`, `subscription_items` | Active billing relationships |
| `plans`, `meters`, `rating_rule_versions` | Pricing config — invoice math depends on this |
| `invoices`, `invoice_line_items` | Issued invoices — financial record |
| `credit_notes`, `credit_note_line_items` | Refunds and adjustments — financial record |
| `customer_credit_ledger` | Customer credit balance state (event-sourced; ALL entries are needed) |
| `payment_methods`, `customers.stripe_customer_id` | Saved cards + the Stripe Customer mapping |
| `dunning_policies` (+ `customers.dunning_policy_id`) | Operator-set dunning policy + per-customer assignment |
| `invoice_dunning_runs`, `invoice_dunning_events` | Dunning state machine; loss can re-stack failed charges |
| `tax_calculations` | Stripe Tax provider records — ties invoice to upstream tax_transaction |
| `audit_log` | SOC 2 evidence — usually 7y retention required |
| `coupons`, `customer_discounts`, `coupon_redemptions` | Discount config + assignments (coupons are cut pre-launch per ADR-039; tables remain in the schema) |
| `users`, `user_tenants`, `password_reset_tokens` | Operator identities |
| `api_keys` | Authentication credentials |
| `tenant_settings`, `stripe_provider_credentials` | Tenant configuration including encrypted Stripe keys |
| `webhook_endpoints` | Outbound webhook config + signing secrets |
| `recipe_instances` | Installed pricing-recipe records |
| `test_clocks`, `dashboard_sessions` | Active state; not financial but loss = operator UX disruption |
| `schema_migrations` | Migration version state — required for app to boot |

### Tier 2 — Reconstructable (can drop on restore in extremis)

These tables are useful for replay/audit but the application can
function correctly without them. If restore time is critical and a
sub-second-old backup is unavailable, **dropping these is acceptable
and reduces restore complexity**.

| Table | Loss impact | Why it's reconstructable |
|---|---|---|
| `email_outbox` | Pending emails won't deliver | Producers re-fire on next state change; lost emails are a UX hit, not a financial one |
| `webhook_outbox` | Pending webhooks won't deliver | Same — consumers should be idempotent; replay if needed |
| `webhook_events` (Stripe inbound) | Reconciler loses some history | Stripe is the source of truth; can re-fetch events via API |
| `stripe_webhook_events` | Same as above | Same |

### Tier 3 — Cache / log (drop freely)

| Table | Loss impact |
|---|---|
| `usage_events` (rows older than current open billing periods) | Already aggregated into invoices; aged data is for analytics only |

**Important caveat**: usage_events for the *current open billing
period* are Tier-1. The cycle that hasn't been billed yet needs
every event. Snapshot the cutover carefully.

## Backup strategy

### Recommended: full PITR (point-in-time recovery)

Most managed Postgres providers ship this by default (RDS automated
backups + WAL archiving, Cloud SQL PITR, Aurora continuous backup).
Self-managed: pgbackrest or WAL-G.

This is the safest pattern for Velox because every table is captured
and you can restore to any second within retention.

### Acceptable alternative: nightly logical dump

`pg_dump --format=custom` nightly + WAL archive for replay between
dumps. Velox's schema is small enough (a few dozen tables); a full
dump completes quickly even at scale.

### Not recommended

- **Per-table dumps** — easy to miss a table on add. Velox adds
  tables every few weeks; per-table dumps drift.
- **Snapshot-only without WAL** — guarantees data loss between
  snapshot intervals. The shorter the interval, the more acceptable;
  hourly snapshots without WAL are still ~30min average loss.

## Restore validation

After a restore, validate before resuming traffic. This is Velox-
specific work — what to check that the DB is functionally healthy.

### 1. Schema migration version matches application

```sql
SELECT version, dirty FROM schema_migrations ORDER BY version DESC LIMIT 1;
```

Should match the version the binary expects. `dirty=false` is
required — a `dirty=true` row means a migration failed mid-way and
the schema is in an unknown state. Resolve before booting.

### 2. RLS policies intact

```sql
SELECT schemaname, tablename, rowsecurity
FROM pg_tables WHERE schemaname = 'public' AND rowsecurity = true
ORDER BY tablename;
```

Should list every tenant-scoped table (customers, invoices, etc).
Missing entries = data isolation broken.

### 3. Tier-1 financial table sanity

```sql
-- Per tenant, recent invoice + credit note counts should match
-- pre-restore expectations.
SELECT tenant_id, count(*) FROM invoices GROUP BY tenant_id;
SELECT tenant_id, count(*) FROM credit_notes GROUP BY tenant_id;
SELECT tenant_id, sum(total_amount_cents) FROM invoices
  WHERE status = 'paid' AND created_at >= now() - interval '90 days'
  GROUP BY tenant_id;
```

Compare against pre-incident metrics (Grafana retention should hold
this).

### 4. Idempotency keys + outbox not duplicating work

After restore, the email + webhook outboxes will re-fire any
`pending` rows. If you restored to a point before some emails were
dispatched but webhooks were already delivered, you'll get duplicate
deliveries. Mitigation:

```sql
-- Optionally mark all pending outbox rows as dispatched if you're
-- confident downstream consumers are idempotent and you want to
-- skip the re-fire. Document this in the restore log.
UPDATE email_outbox SET status = 'dispatched', dispatched_at = now() WHERE status = 'pending';
UPDATE webhook_outbox SET status = 'dispatched', dispatched_at = now() WHERE status = 'pending';
```

This is a judgment call. Default safer behaviour: leave them
`pending`, accept some duplicate sends; consumers should be
idempotent.

### 5. Scheduler resumes cleanly

The billing scheduler is leader-elected via Postgres advisory locks —
one leader runs each tick, other replicas stand by, so a single instance
or a multi-replica set both resume cleanly. On restart it picks up where
it left off:

- `subscriptions.next_billing_at` is the cycle anchor — if a sub was
  due at the moment of failure but didn't bill, the next scheduler
  tick (1h in production, 5m in local) picks it up.
- `invoices.auto_charge_pending = true` rows get retried by the
  scheduler.
- `invoice_dunning_runs` with `next_action_at <= now()` get
  processed.

After restart, verify:

```sql
-- Subs that should have billed but didn't (next_billing_at in past)
SELECT count(*) FROM subscriptions
  WHERE status IN ('active', 'trialing') AND next_billing_at < now();

-- Pending auto-charges
SELECT count(*) FROM invoices
  WHERE auto_charge_pending = true;

-- Dunning runs awaiting action
SELECT count(*) FROM invoice_dunning_runs
  WHERE state = 'active' AND next_action_at < now();
```

These counts should drain within a few minutes of normal scheduler
operation. If they don't, scheduler isn't running.

### 6. Stripe webhook reconciler catches up

Restored from a point hours behind? Stripe webhook events fired
during the gap won't be in `webhook_events`. The reconciler queries
Stripe to resolve `payment_status='unknown'` invoices, but it won't
auto-replay every webhook. If the gap is long:

```sql
-- Surface invoices that may have stale payment status
SELECT id, payment_status, updated_at FROM invoices
  WHERE updated_at < now() - interval '4 hours'
    AND payment_status IN ('processing', 'unknown', 'pending');
```

Operator may need to manually reconcile from the Stripe Dashboard
or fire a webhook backfill via Stripe's API.

## What you don't need to back up

- **Application binaries** — rebuild from source.
- **Container images** — rebuild or pull from registry.
- **Static config** (env vars, secrets) — managed via your secrets
  store; back up separately from Velox.
- **Customer-facing PDFs** — regenerated on demand from invoice
  data.

## Encryption at rest

If `VELOX_ENCRYPTION_KEY` is set, Velox encrypts customer PII (email,
names, phone, tax IDs), webhook signing secrets, and per-tenant
Stripe credentials at the application layer (AES-256-GCM) before
persistence. This is **in addition to** any disk-level encryption
your Postgres provides (RDS encryption, GCP CMEK, LUKS, etc).

**Critical**: you must back up the `VELOX_ENCRYPTION_KEY` separately
from the database. **A backup without the key is unrecoverable** —
every encrypted column reads as garbage. Most teams store the key in
a KMS or secret manager (AWS Secrets Manager, GCP Secret Manager,
HashiCorp Vault); the secret-store's own backup story applies.

Document the key rotation procedure: re-encrypt-on-read isn't
implemented, so a key change requires a full re-encrypt migration
across affected tables. Defer until you have a real rotation
trigger.

## Test the restore

The only validated backup is a restore that booted Velox and passed
the validation checks above. Schedule quarterly restore drills:

1. Pull the most recent backup.
2. Restore to a non-production database.
3. Run the validation queries.
4. Boot Velox against the restored DB; hit `/health/ready`.
5. Run the smoke tests in `MANUAL_TEST.md` (FLOW S1).

A backup you've never restored is hope, not a backup.

### Managed Postgres (RDS, Aurora, Cloud SQL)

On managed Postgres the division of labor changes: the provider's native
snapshots + point-in-time recovery are your PRIMARY mechanism (turn them on,
set retention — they beat anything a dump script offers), and the WAL-archive
recipe below does not apply (managed services don't expose that access). What
this repo's tooling still gives you there, and why you still run the drill
quarterly:

1. **App-level verification** — the provider proves snapshots exist; only the
   drill proves Velox boots against a restore and the money tables
   count-match.
2. **Portability** — provider snapshots restore only into the same service; a
   logical dump is your proof the billing data isn't cloud-locked.
3. **The guards fire where managed setups actually break** — managed servers
   run pinned majors while laptop/CI tools run newer (the exact mismatch the
   2026-08-11 drill caught), and a fresh managed instance has no `velox_app`
   role either.

### Fresh-cluster restores: provision roles first

Roles are cluster-level; a database dump cannot carry them. On a fresh
cluster — the disaster-recovery case — create the runtime role BEFORE
restoring, or the archive's GRANTs roll the single-transaction restore
back (restore.sh now refuses up front with this remedy):

```sql
CREATE ROLE velox_app LOGIN PASSWORD '<your-password>';
```

### Tool-version rule

`pg_dump`/`pg_restore` majors newer than the server produce archives the
server's major can't restore (dump side) or client `SET` commands the
target rejects (restore side). Both scripts fail loudly on mismatch; use
matching tools, e.g. `PG_DUMP="docker run --rm -i --network host
postgres:16-alpine pg_dump"` (archives travel via stdio, so wrapped
binaries need no mounts).

### Drill history

| Date | Result | Notes |
|---|---|---|
| 2026-08-11 | **FAIL → 4 defects → PASS** (7s total: backup 1s, restore 3s; 5 critical tables count-matched) | First-ever run. Found: (1) host `pg_dump` 18 vs server 16 → unrestorable archive; (2) host `pg_restore` 18 vs target 16 → client `SET transaction_timeout` rolled restore back; (3) fresh-cluster restore rolled back on missing `velox_app` role — the DR path had never worked; (4) restore.sh's validation query referenced a nonexistent column (`table_name` vs `relname`) — broken since birth. All four fixed same day; drill re-run green. |

