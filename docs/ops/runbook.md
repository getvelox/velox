# Operations Runbook

What pages the on-call engineer, what does not, and what to do when
Velox is in trouble. Written for whoever operates a Velox deployment.
Velox-specific failure modes only — generic Postgres / Kubernetes (K8s) /
Stripe troubleshooting is out of scope.

Each failure section follows the same order: **Symptom**, **Why**,
**Check** (commands), **Fix**, and **Verify** where the text says how to
confirm the fix.

## Symptom index

| If you see | Go to |
|---|---|
| `/health/ready` returns 503, or no invoices are being generated | [1. Scheduler stalled](#1-scheduler-stalled) |
| `email_outbox` growing, or customers report missing invoice emails | [2. Email outbox backed up](#2-email-outbox-backed-up) |
| `webhook_outbox` growing, or one customer's webhook endpoint failing | [3. Webhook outbox backed up or one endpoint failing](#3-webhook-outbox-backed-up-or-one-endpoint-failing) |
| `velox_stripe_breaker_state == 2`, or dunning retries skipped | [4. Dunning circuit breaker open](#4-dunning-circuit-breaker-open) |
| Invoices stuck with an unconfirmed payment for hours | [5. Stale `payment_status='unknown'` invoices](#5-stale-payment_statusunknown-invoices) |
| A test clock stuck on the Advancing badge | [6. Test-clock advance hung](#6-test-clock-advance-hung) |
| A tenant sees another tenant's data | [7. RLS leakage suspected](#7-rls-leakage-suspected) |
| LiteLLM spend 503s, or Velox spend short of LiteLLM's | [8. LiteLLM spend gap after an outage](#8-litellm-spend-gap-after-an-outage) |
| A replica will not boot and names Redis | [9. A replica will not start](#9-a-replica-will-not-start-redis_url-is-required--redis-is-invalid-or-unreachable) |
| `velox_parked_invoices` above zero | [10. Parked invoices](#10-parked-invoices) |
| `velox_creditnote_pending_issue_drafts` growing over days | [11. Clawback drafts not issuing](#11-clawback-drafts-not-issuing) |
| `velox_audit_write_errors_total` rising | [12. Audit write errors](#12-audit-write-errors) |
| `velox_audit_uncovered_mutation_total` rising | [13. Mutation without an audit row](#13-mutation-without-an-audit-row) |
| Write I/O high for minutes after a bulk load | [After a large backfill](#after-a-large-backfill-expect-a-few-minutes-of-elevated-write-io) |
| p99 jumps, p50 flat, after a quiet spell (RDS) | [Size the WAL segment pool](#on-rds-size-the-wal-segment-pool--the-defaults-let-commits-stall-after-a-quiet-spell) |
| p50 lifts for everyone for seconds on a large table | [Autovacuum stall](#autovacuum-on-a-large-insert-only-table-can-stall-every-commit-for-seconds) |

## Health endpoints

| Endpoint | Purpose | Use for |
|---|---|---|
| `GET /health` | Liveness — process running | Kubernetes liveness probe |
| `GET /health/ready` | Readiness — DB reachable (ping), scheduler ran recently | Kubernetes readiness probe + LB health check |
| `GET /metrics` | Prometheus scrape (Bearer `METRICS_TOKEN` when set) | Metrics-collection job |

The scheduler is the background loop that drives billing.
`/health/ready` returns 503 if the scheduler has not ticked in 2× the
configured interval. So the readiness probe catches a stalled scheduler,
without waiting for a liveness restart. See
[Scheduler stalled](#1-scheduler-stalled).

## Key metrics to alert on

All metrics are exported under `/metrics`. The set below is the
alerting tier: what should page someone, and what is only informational.

### Page (critical)

| Metric | Threshold | What it means |
|---|---|---|
| `velox_http_request_duration_seconds` p99 | > 5s for 5m | Server is unhealthy |
| `velox_billing_cycle_errors_total` | rate > 0.1/s for 5m | Billing cycles failing systematically |
| `up{job="velox"}` | == 0 | Process down |
| Postgres connection errors (count) | > 10/min | DB connectivity broken |
| `time() - velox_scheduler_last_run_timestamp_seconds` | > 2× tick interval | Scheduler stalled (also flips `/health/ready` to 503) |
| `velox_leader_last_tick_age_seconds{role}` | > 3× the role's interval | The role has not finished a tick on any replica. This is the cluster-wide stall signal that a per-replica liveness gauge cannot give. `SELECT * FROM leader_status;` shows the holder and whether an operator paused the role ([runbook-leader-leases.md](runbook-leader-leases.md)) |
| `increase(velox_leader_lease_lost_total[1h])` | > 0 | A leader could not keep its lease mid-tick (frozen process, DB stall, pooler hiccup). Correctness held (fence + row CAS), but find out why |

### Warn (slack/email, not page)

| Metric | Threshold | What it means |
|---|---|---|
| `velox_payment_charges_total{result="failed"}` | rate spikes 5× baseline | Stripe issue or systematic decline |
| `velox_dunning_runs_processed_total{outcome="failed"}` | rate > 0.5/s | Dunning machinery struggling |
| `velox_webhook_deliveries_total{status="failed"}` | sustained failure | Customer's webhook endpoint down or signature wrong |
| `velox_stripe_breaker_state` | == 2 (open; 0=closed, 1=half_open) | Stripe API circuit-breaker tripped |
| `velox_email_outbox_pending` | > 1000 | Email dispatcher stuck or SMTP provider issue (−1 = the metric query itself failed) |
| `velox_webhook_outbox_pending` | > 1000 | Webhook dispatcher stuck (−1 = metric query failed) |
| `velox_creditnote_pending_tax_reversals` | any row older than 24h (alert on age, not presence) | An issued credit note whose upstream tax reversal is still owed. The sweep re-drives it every tick and keeps it forever (promoted on first failure, 2026-08-30). Presence for a tick or two is normal. A day means the provider keeps rejecting it: handle as SEV-2 (see [Escalation](#escalation)) |
| `velox_invoice_pending_tax_reversals` | any row older than 24h | A voided `stripe_tax` invoice whose committed tax transaction has no confirmed reversal. The #310 sweep re-drives it. Same as the row above: SEV-2 (see [Escalation](#escalation)) |
| `velox_creditnote_pending_issue_drafts` | sustained growth over days | Clawback drafts not issuing. Alert on growth/age, not presence. See [§11](#11-clawback-drafts-not-issuing) |
| `velox_parked_invoices{mode}` | > 0, sustained | An invoice whose charge attempt could not be identified with the provider (ADR-107). **It will not resolve on its own and needs a human.** See [§10](#10-parked-invoices) |
| `velox_http_requests_total{method="POST",path="/v1/integrations/litellm/spend",status="503"}` | any increase | LiteLLM spend batches were refused for retry (usage store unreachable). If LiteLLM's proxy log shows `Generic API Logger Error sending batch`, its retries were exhausted. Run [§8](#8-litellm-spend-gap-after-an-outage) |
| `velox_auto_charge_retries_total{result="failed"}` | growing rapidly | Many invoices stuck in retry |
| `velox_audit_write_errors_total{outcome="row_lost"}` | rate > 0/s | **Audit evidence permanently lost.** Treat as a compliance incident. See [§12](#12-audit-write-errors) |
| `velox_audit_write_errors_total{outcome="mutation_refused"}` | rate > 0/s | **Nothing is missing.** An availability problem, not an evidence problem. See [§12](#12-audit-write-errors) |
| `velox_audit_uncovered_mutation_total{route}` | any increase | A route mutated state and wrote no audit row. Should be **flat zero**. See [§13](#13-mutation-without-an-audit-row) |

### Info (dashboards, no alert)

- `velox_billing_cycles_total` — cycle throughput
- `velox_invoices_generated_total` — invoice volume
- `velox_usage_events_ingested_total` — usage ingest rate
- `velox_billing_cycle_duration_seconds` — cycle latency
- `velox_credit_operations_total` — credit ledger activity
- `velox_tax_outcome_total{outcome, reason}` — non-happy tax outcomes (deferrals) by reason
- `velox_scheduled_cleanup_rows_total` — periodic cleanup activity

## Failure modes — diagnosis + fix

### 1. Scheduler stalled

**Symptom**: `/health/ready` returns 503; subscriptions due for billing
are not being invoiced; `velox_billing_cycles_total` rate drops to 0.

Before you treat a gap in invoices as an outage, check the tick interval
in [Scheduler interval tuning](#scheduler-interval-tuning).
`/health/ready` returns 503 only after 2× that interval.

**Why it happens**:
- Long-running transaction holding row locks (e.g., a tenant with
  millions of usage events on a single subscription).
- DB primary failover; connections lost mid-tick.
- The scheduler goroutine (its background worker thread) panicked. This
  is rare, and `slog.Error` logs it.

**Check**:
1. Cluster view: `SELECT * FROM leader_status;` shows the holder and
   whether an operator paused the role. See
   [runbook-leader-leases.md](runbook-leader-leases.md).
2. Run the queries below. The block mixes SQL and one shell command: run
   the `curl` line in a shell, not in psql.

```sql
-- Long-running queries
SELECT pid, now() - query_start AS duration, state, query
FROM pg_stat_activity
WHERE state = 'active' AND query_start < now() - interval '30 seconds'
ORDER BY duration DESC;

-- Scheduler last-run timestamp (from /health/ready response body)
curl -s http://localhost:8080/health/ready
```

**Fix**:
1. Check application logs for panics; restart pod if found.
2. Cancel long queries with `SELECT pg_cancel_backend(<pid>)` if
   appropriate.
3. Batch size is hard-coded as a fixed literal (50 subs per tick,
   `cmd/velox/main.go`) and is not env-configurable today, so the batch cannot be
   shrunk. Drain a hot-spotting tenant (one tenant dominating the batch) on
   demand with `POST /v1/billing/run` (per tenant, loops until empty).
   - Safe to run while the leader's tick is in flight (ADR-115).
   - A drain may report 0 for a subscription the leader is committing at
     that instant. The leader's commit bills it, and the two can never
     both bill a period.

### 2. Email outbox backed up

**Symptom**: `email_outbox` table growing past 1000 rows in
`status='pending'`; customers report missing invoice emails.

**Why**:
- SMTP provider rate-limiting or down.
- Provider rejected mail (auth, sender domain, etc).
- Dispatcher stopped (rare).

**Check**:
```sql
SELECT email_type, status, count(*), max(attempts) AS max_attempts
FROM email_outbox
WHERE status IN ('pending', 'failed')
GROUP BY email_type, status
ORDER BY count(*) DESC;

-- Most-recent failure messages (truncated to most-recent N)
SELECT email_type, last_error, count(*)
FROM email_outbox
WHERE status = 'failed' AND last_error IS NOT NULL
GROUP BY email_type, last_error
ORDER BY count(*) DESC
LIMIT 10;
```

A `last_error` beginning `email outbox: unknown email_type` means
different things by row status:
- On a *pending* row: a replica older than the row's producer claimed it
  (rolling deploy). It retries on the backoff ramp, and a replica that
  knows the type delivers it.
- On a *failed* row: no replica in the fleet knew the type for the whole
  ramp. The producer shipped without its dispatcher case.

**Fix**:
1. Diagnose SMTP provider via `last_error`.
2. Once provider is healthy, the dispatcher drains automatically;
   pending rows fire on their `next_attempt_at` schedule.
3. To speed recovery, mass-reset `next_attempt_at` to now:
   ```sql
   UPDATE email_outbox SET next_attempt_at = now()
   WHERE status = 'pending' AND attempts < 15;
   ```
4. Failed rows are dead-lettered ("DLQ'd"): set aside after delivery
   gave up. Investigate the root cause. Then either fix and retry
   (`UPDATE ... SET status='pending', attempts=0`) or accept the loss
   and alert affected customers.

### 3. Webhook outbox backed up or one endpoint failing

**Symptom**: `webhook_outbox` growing past 1000 rows in
`status='pending'` (as for email in §2), or one customer's endpoint
failing. Per-endpoint attempts live in `webhook_deliveries`, not in the
outbox.

**Why**:
- Customer's webhook endpoint is down or rejecting.
- HMAC signature mismatch: the shared-secret signature on each delivery
  no longer verifies. The customer rotated the secret without telling
  Velox.

**Check**:
```sql
-- The outbox is the queue (one row per event, no endpoint column);
-- per-endpoint attempts live in webhook_deliveries.
SELECT event_type, status, count(*), max(attempts) AS max_attempts
FROM webhook_outbox
WHERE status IN ('pending', 'failed')
GROUP BY event_type, status;

-- Per-endpoint failure rate (delivery log, not the outbox)
SELECT webhook_endpoint_id, error_message, count(*)
FROM webhook_deliveries
WHERE status = 'failed' AND error_message IS NOT NULL
GROUP BY webhook_endpoint_id, error_message
ORDER BY count(*) DESC
LIMIT 20;
```

**Fix**:
1. Contact customer; confirm endpoint is up.
2. If signature mismatch:
   1. Rotate the signing secret in the dashboard
      (`Webhooks → Endpoint → Rotate secret`).
   2. The customer updates their side.
   3. Replay failed events.
3. Only in a SEV-1, pause deliveries by deactivating the endpoint (see step 1 of
   [Escalation](#escalation)). Events emitted while it is inactive are not delivered to it.

### 4. Dunning circuit breaker open

**Symptom**:
- `velox_stripe_breaker_state == 2` (open; 1 = half-open probing).
- Dunning retries (the automated follow-up attempts on failed payments)
  are silently skipping. This is correct behaviour.
- Customers report they expected retries but no email arrived.

**Why**:
- Stripe API has been failing repeatedly. The circuit breaker stops
  calling a dependency that keeps failing, until it recovers. It tripped
  to protect the retry budget.
- Tenant's Stripe credentials are invalid (per-tenant breaker).

**Check**:
```sql
-- Check recent payment retry outcomes
SELECT outcome, count(*) FROM (
  SELECT
    CASE WHEN reason LIKE '%breaker%' OR reason LIKE '%transient%'
         THEN 'transient_skip'
         ELSE 'real_failure' END AS outcome
  FROM invoice_dunning_events
  WHERE event_type = 'retry_attempted' AND created_at > now() - interval '1 hour'
) t GROUP BY outcome;
```

**Fix**:
1. Check Stripe status (`status.stripe.com`).
2. If tenant-specific: verify Stripe credentials in
   `Settings → Stripe`; rotate if needed.
3. Breaker auto-resets after cool-off; no manual intervention
   normally required.

### 5. Stale `payment_status='unknown'` invoices

**Symptom**: Invoices stuck at `payment_unconfirmed` for hours.

**Why**:
- Stripe webhook delivery delayed or lost.
- The reconciler (the periodic sweep that re-checks payment status
  against Stripe) is not running (single-instance assumption broken?).
- The Stripe PaymentIntent (PI) is parked at `requires_action`. That is
  an off-session SCA challenge (Strong Customer Authentication, a bank
  verification step) that nobody completes. The reconciler resolves only
  terminal Stripe outcomes and deliberately skips in-flight PIs every
  sweep. So these never self-heal; see Fix step 3.

**Check**:
```sql
SELECT id, payment_status, stripe_payment_intent_id, updated_at
FROM invoices
WHERE payment_status = 'unknown' AND updated_at < now() - interval '1 hour'
LIMIT 20;
```

**Fix**:
1. Check application logs for "reconciler" entries; reconcilers run
   once per scheduler tick (1h in production, 5m in local), not on a
   60s loop.
2. There is no manual bulk-reconcile endpoint. The payment reconciler
   sweeps automatically every tick. Per invoice, use the dashboard's
   invoice attention actions (charge now / retry). The reconciler's next
   pass also self-heals any invoice whose PI reached a terminal state at
   Stripe.
3. For a PI parked at `requires_action`: cancel the PI in Stripe (the
   reconciler then settles it failed), or get the customer to complete
   authentication.

### 6. Test-clock advance hung

**Symptom**: `test_clocks.status='advancing'` for >5min; operator
sees Advancing badge stuck.

**Why**:
- Catchup loop processing many cycles. A test clock simulates time so
  billing can be exercised without waiting. A large jump on a monthly
  sub can require dozens of billing-engine sweeps.
- Billing-engine error mid-catchup; sub flipped to
  `internal_failure`.
- A deploy abandoned an in-flight advance (shutdown waits 30s for it,
  then exits). The clock stays `advancing` until **any replica
  (re)starts**, because recovery runs once at boot, never on a schedule.
  In a rolling deploy the new replica has usually already booted, so
  nothing will pick it up. See Fix step 3.

**Check**:
```sql
SELECT id, name, status, frozen_time, updated_at
FROM test_clocks WHERE status = 'advancing';

-- Check catchup progress: subscriptions on this clock
SELECT s.id, s.next_billing_at, count(i.id) AS invoices_generated
FROM subscriptions s
JOIN test_clocks tc ON tc.id = s.test_clock_id
LEFT JOIN invoices i ON i.subscription_id = s.id AND i.created_at > tc.updated_at
WHERE tc.id = '<clock_id>'
GROUP BY s.id, s.next_billing_at, tc.updated_at;
```

**Fix**:
1. If progress is happening (invoices being generated), wait. Large
   jumps take time.
2. If `internal_failure`, retry the advance with the dashboard's
   `Retry advance` button (or `POST /v1/test-clocks/<clock_id>/retry-advance`).
   It flips the clock back to `advancing` and resumes catchup from where
   it stopped (recorded in ADR-018, an architecture decision record).
   Delete only as a last resort: since ADR-086, clock deletion is a
   complete teardown of the clock's simulated data.
3. If a deploy abandoned the advance: restart one replica. (Program ha-9
   makes recovery a scheduled leader tick; until then this is the
   operator action.)

### 7. RLS leakage suspected

**Symptom**: A tenant reports seeing another tenant's data, OR a
support ticket includes data from a different tenant than the
operator's session.

**This is a SEV-1** (highest-severity incident). A leak across RLS
(Row-Level Security, the Postgres mechanism that hides each tenant's
rows from every other tenant) is the worst-case bug.

**Check**:
1. Lock down. Velox has no read-only mode. Contain by revoking API
   keys (dashboard → API Keys) and/or stopping the API container;
   Postgres stays up for forensics.
2. Verify RLS is enabled on every tenant-scoped table:
   ```sql
   SELECT schemaname, tablename, rowsecurity
   FROM pg_tables WHERE schemaname = 'public' AND rowsecurity = false
   ORDER BY tablename;
   ```
   Anything unexpected here = isolation broken.
3. Check the leaked query: was it run with `app.tenant_id` correctly
   set? Was `app.bypass_rls` involved?

**Fix**:
- Patch the path that bypassed RLS.
- Audit `audit_log` for affected tenant pair.
- Notify both customers + breach review.

### 8. LiteLLM spend gap after an outage

**Symptom**: the `…/litellm/spend` 503 counter rose during a Postgres
failover or a rolling restart, and LiteLLM's proxy log shows
`Generic API Logger Error sending batch` (its `max_retries` were spent).
Or LiteLLM was never configured with retries (`docs/integrations/litellm.md`).

**What was lost**: every LLM call LiteLLM flushed in that window. Velox's
spend view will be short against LiteLLM's spend page for the period.

**Fix**: replay LiteLLM's spend logs for the window to
`POST /v1/integrations/litellm/spend`.
- Every row is idempotency-keyed
  (`<litellm_call_id>:input|output|cache_read`). So a replay of a window
  that was partly recorded is a pure gap-fill.
- Hard deadline: the period must not be finalized yet. A replay landing
  in a finalized period increments `velox_usage_late_event_total` and
  needs a manual credit/debit.

**Verify**: rows already held come back as `deduplicated`, never
double-counted.

### 9. A replica will not start: "REDIS_URL is required" / "Redis is invalid or unreachable"

**Symptom**: a production replica refuses to boot and logs one of the
two messages above.

**Why**: production refuses to boot without a reachable Redis
(2026-08-30). The general and hosted-invoice rate limiters fail closed
without it. So a Redis-less replica would boot green and answer 429 to
its share of traffic, including hosted-invoice pay pages, while
`/health/ready` stays ok. The refusal is the fix.

**Fix**: check `REDIS_URL`, the security group / network path to 6379,
and that the managed Redis is up.

**Verify**: the replica starts on the next attempt.

**Redis going down after boot** is a different situation:
- General and hosted-invoice requests 429 (fail closed).
- Ingest and `/v1/auth` keep working (fail open).
- Restore Redis; no restart needed.

### 10. Parked invoices

**Symptom**: `velox_parked_invoices{mode}` > 0, sustained.

**Why**: a parked invoice is one whose charge result is unclear, so
Velox cannot tell if the customer paid (see the
[glossary](../README.md#glossary)). Here, its charge attempt could not
be identified with the provider (ADR-107).
- It is deliberately unchargeable. No sweep, no dunning retry and no
  operator "Collect payment" will touch it. That is what makes a double
  charge unreachable.
- **It will not resolve on its own and needs a human.**
- Alert on presence, not growth. At zero customers the expected value is
  zero, and each one is a real invoice a real customer cannot pay.

**Check**: find the attempt in Stripe by customer + amount + approximate
time.

**Fix**:
1. If it succeeded, its webhook settles the invoice (no action).
2. If nothing was charged, mark the invoice uncollectible. This is the
   only action the invoice page offers. It releases its deferred
   clawback draft and closes its dunning run.

### 11. Clawback drafts not issuing

**Symptom**: `velox_creditnote_pending_issue_drafts` shows sustained
growth over days.

**Why**: a clawback is a credit note Velox issues when a paid, in-advance
period is cut short (see the [glossary](../README.md#glossary)).
- Drafts deferred behind an in-flight source payment (ADR-059) sit here
  legitimately and do not appear in error logs.
- The reconciler's eligibility scan skips them by design until the
  source settles.
- So alert on growth/age, not presence.

**Check**: does this gauge move together with `velox_parked_invoices`?
If so, the drafts are waiting on parked invoices.

**Fix**: only if the two gauges move together, resolve those parked invoices
(write them off; see [§10](#10-parked-invoices)). Otherwise the drafts are waiting on
in-flight source payments and need no action.

**Verify**: these drafts drain on the next reconciler pass.

### 12. Audit write errors

**Symptom**: `velox_audit_write_errors_total` rate > 0/s. The `outcome`
label says which of two very different cases you have.

**`outcome="row_lost"`: evidence permanently lost.**
- The mutation committed and its audit row did not, and nothing retries
  it.
- Irrecoverable: the row cannot be reconstructed.
- Fix: treat it as a compliance incident.
  1. Capture the tenant from the error log. The metric deliberately
     carries no tenant label; see the note below.
  2. Identify the affected mutations by their absence.

**`outcome="mutation_refused"`: nothing is missing.**
- The audit write failed inside the business transaction. So ADR-090's
  shared fate rolled the mutation back with it.
- This is an availability problem, not an evidence problem. The customer
  got an error, and the log is intact.
- Fix: investigate the DB, not the audit trail.

> **Why the audit metric carries no `tenant_id` label.** Prometheus client counters
> never age out. So a raw-tenant label mints a permanent time series for every tenant
> that ever has a single failure. That is unbounded cardinality: it grows with the
> customer base and outlives the incident. One shared-DB blip during a busy hour could
> mint thousands. The tenant is in the **error log**, which is queryable and expires; the
> metric answers only *"is evidence at risk, and in which direction"*.

### 13. Mutation without an audit row

**Symptom**: `velox_audit_uncovered_mutation_total{route}` shows any
increase. A route mutated state and wrote no audit row.

**Why**: the counter should be **flat zero**. Every mutating route is
declared in `internal/api/audit_routes.go` as `explicit` (it emits) or
`exempt` (it does not need to). A non-zero counter means one of three
things, in order of likelihood:
1. A route declared `explicit` has an emission path that can be skipped.
2. A genuinely non-mutating 2xx path (a cache/idempotency replay, a
   no-op save) needs an `audit.MarkSkip` declaration.
3. A new route shipped without a declaration. CI makes this impossible
   (the route-walk test fails the build). It is still possible if
   someone edited the registry to silence it.

**Check**: find the route via the `route` label and the
`UNCOVERED MUTATION` error log.

**Fix**: do not "fix" this by adding an exemption without recording what
is being given up.

## After a large backfill, expect a few minutes of elevated write I/O

**Symptom**: after a bulk load, write I/O stays high for a few minutes
and a commit can stall.

Measured on the benchmark rig (2026-08-16):
- A 20M-row bulk load into `usage_events` was followed by ~5 minutes of
  RDS write IOPS at ~2× baseline. Autovacuum and the checkpointer were
  working through the load.
- One commit stall of ~5 s came about 20 s after ingest resumed at
  200 ev/s.
- Steady traffic in the following 90 minutes across three rates showed
  nothing similar.

**Why**: this is Postgres maintenance, not a Velox defect.

**Fix**: suppose you backfill tens of millions of events
(`POST /v1/usage-events/backfill`, or a direct load) and then need
sub-10 ms p99 immediately. Then wait for `WriteIOPS` to return to
baseline first, or schedule large backfills off-peak.

**Check**: watch `ReadIOPS` too. A tail that rises with CPU flat is the
index working set falling out of cache. See
`docs/benchmarks/sustained-throughput.md` for the numbers on `db.m7g.2xlarge`.

## On RDS, size the WAL segment pool — the defaults let commits stall after a quiet spell

**Symptom**: after a quiet spell, every commit freezes for
~0.2–0.3 s every ~5 s until the next checkpoint refills the pool.
- p99 jumps 5–10×; p50 does not move.
- CPU, memory and CloudWatch's 60-second I/O averages look normal (the
  stall minute read 1,039 IOPS, 42 MB/s, queue depth 1.3).

**Why**: measured on the benchmark rig (2026-08-17, `db.m7g.4xlarge`,
100 GB gp3, PG 16.14, 12,000 events/s, 1-second instrumentation). The
tail events the third run caught are **WAL segment creation under
`WALWriteLock`.**
- RDS uses 64 MB WAL segments and keeps a pool of pre-made ones.
- Recycling at each checkpoint completion refills the pool to about one
  checkpoint's worth. By the recycling arithmetic in `xlog.c`, that
  leaves less than one segment of margin at this write rate.
- When the pool runs dry, the committing backend that needs the next
  segment writes 64 MB of zeros and fsyncs it. Every other commit waits.

Two things drain the pool, both after a quiet spell:
1. Small checkpoints decay the checkpointer's distance estimate, so
   fewer old segments are recycled. `min_wal_size` (RDS default 192 MB)
   is the floor that would stop it.
2. RDS's retained segments (`wal_keep_size` 2 GB) sit inside the
   `max_wal_size` window after an idle. So the pool on resume is
   shallower than in steady state, and the first timer-driven checkpoint
   refills too late. `max_wal_size` bounds the depth.

Both were reproduced on demand; either knob alone was not enough
(provocation arms A–C in `docs/benchmarks/sustained-throughput.md` § third run).

**Check** (how to see it if you suspect it):
- `pg_stat_activity` (or the rig sampler) shows a client backend in
  `IO:WALInitWrite`/`WALInitSync` with a pile-up on `LWLock:WALWrite`.
- `TransactionLogsDiskUsage` grows under load right after it fell at a
  checkpoint.
- Enhanced Monitoring (1 s) shows device writes near the volume's
  throughput ceiling, with the average request size falling toward
  ~13 KB while `pg_stat_io` writers are steady.
- Performance Insights at 1 s usually misses the ~0.2 s stalls, and a
  `WALWrite` pile-up alone is not specific.

Numbers and the full attribution: `docs/benchmarks/sustained-throughput.md`
§ third run.

**Fix**: in the instance's parameter group set **`min_wal_size = max_wal_size ≥
wal_keep_size + (1 + checkpoint_completion_target) × peak WAL rate ×
checkpoint_timeout, with margin`**.

| Peak rate | WAL rate | Set |
|---|---|---|
| 12–15k events/s | 14–20 MB/s | 2 + 8–11 GB, so **16 GB** |
| 25,000 events/s | 29.6 MB/s | **24–32 GB**, or shorten `checkpoint_timeout`. The pool at 16 GB still hit zero twice in 35 minutes |

Both are dynamic (no reboot). Cost is disk: `pg_wal` sits near that size
permanently. Two cautions:
1. Raising the setting does not manufacture segments. The pool grows
   only as segments are created, one ~0.25 s commit pause per 64 MB
   file, until ~16 GB of WAL has been written.
   - That is about a minute of pauses in total, paid once in the
     instance's life.
   - It is invisible if traffic ramps up or an initial data load runs
     first (the load creates the files while nobody waits on commits).
   - It is a rough ~15 minutes if you start at peak on an empty pool.
   - The alternative is the stock behaviour: the same pauses after
     *every* quiet spell, indefinitely.
   - `pg_switch_wal()` is not granted to `rds_superuser`, so there is no
     cheap pre-grow on RDS. A bulk load or ~15 minutes of peak-rate
     traffic grows it. (Measured: applying 16 GB to a shallow pool made
     the next 5 minutes *worse*, provocation arm D.)
2. At higher write rates than measured, re-derive the number.

**Verify**: check the log at peak. Checkpoints should read
`checkpoint starting: time`, not `wal`. A WAL-driven checkpoint puts you
back on the one-segment margin.

Verified on the rig, one series each, on `db.m7g.4xlarge` + 100 GB gp3:
- With the pool at depth, the idle-then-resume recipe did not stall. That
  same recipe stalled stock, floor-only, depth-only and freshly-raised
  settings (worst 10-s p99 24.7 ms vs 162–254 ms).
- A 5 × 10 min series at 12,000 ev/s ran with all checkpoints
  time-driven, no tail window, and the pool never below 45 segments.

## Autovacuum on a large insert-only table can stall every commit for seconds

**Symptom**: for 1–6 s, **p50 lifts for everyone** (36 → 64 ms, 96–99 %
of requests affected). This differs from the WAL-pool stall above, which
leaves p50 alone. Telltale in Enhanced Monitoring: IOs/s ×10–20 while the
average IO size collapses from ~100 KB to ~6 KB.

**Why**: measured on the benchmark rig (2026-08-19, `db.m7g.4xlarge`,
100 GB gp3, 25,000 events/s, stock RDS parameters, vacuum log on).
- PG13+ runs an insert-triggered autovacuum pass over `usage_events`
  after ~20 % growth.
- The end of that pass rewrites nearly every page added since the
  previous pass. This is hint bits and opportunistic freezing, which
  `data_checksums=on` pushes through full-page writes.
- One pass dirtied 540k pages (4.4 GB) and froze 6.6M tuples at
  107 MB/s.
- RDS's `autovacuum_vacuum_cost_limit` (1,200 on this class, 2 ms delay)
  lets it write at the volume's full throughput.
- So the disk queue runs to hundreds, with tens of thousands of ~6 KB
  writes per second, and every commit's WAL write waits behind it.

**Fix**: set **`autovacuum_vacuum_insert_scale_factor = 0.02`** (dynamic;
or per-table reloptions on the events table). It bounds each storm to the
~2 % growth slice instead of 20 % of an ever-growing table. The burst no
longer scales with table size.

**Verify**: tested control-vs-treatment on the same rig at 25,000
events/s.

| Setting | Result |
|---|---|
| Stock | The pass at 80M rows froze every commit for 11 s (worst request 3.3 s, worst 1-s p99 ≈2.9 s, dropped requests, failed repeat) |
| 0.02 | Passes ~10× smaller, worst freeze 0.63 s, five repeats of five passed with zero drops, median untouched (55.7 vs 55.9 ms) |

Measured at 25k batch 100; at lower rates the residual storms are smaller
still. The pacing lever (`autovacuum_vacuum_cost_delay`/`cost_limit`) was
not needed and stays untested. Details and evidence:
`docs/benchmarks/sustained-throughput.md` § third run, "What this leaves".

## Scheduler interval tuning

The tick interval and batch size are compiled-in, not env-configurable:
- The scheduler ticks every **1 hour** in staging/production and **5
  minutes** only when `APP_ENV=local`.
- It processes a fixed **50 subs per tick** (`cmd/velox/main.go`).

A tenant with a backlog is drained on demand via `POST /v1/billing/run`
(loops until that tenant is empty), not by tuning these knobs. A drain
running under the leader's tick may report 0 for a subscription the
leader is committing at that instant (its due fetch skips locked rows).
The leader's commit bills it (ADR-115).

Watch `velox_billing_cycle_duration_seconds` to ensure each tick fits
inside the interval. A leader lease means one replica holds the
`billing` role per tick and renews it while it works (ADR-114). So if a
tick runs long, the next tick starts one interval after the long one
ends, on whichever replica polls first. There is no collision and no
lock wait, only a lengthening backlog.
`velox_leader_last_tick_age_seconds{role="billing"}` is the cluster-wide
lag; see [runbook-leader-leases.md](runbook-leader-leases.md).

## Manual operator interventions

Documented operator-side actions for incidents:

### Force-resolve a stuck dunning run

Pick the resolution that names what actually happened to the invoice.
The column is how finance later answers "why did we stop collecting
this?", and these writes bypass the endpoint that would otherwise
enforce it:

| Invoice ended up | Use |
|---|---|
| paid outside Velox | `payment_recovered` |
| annulled / billed in error | `invoice_voided` |
| written off as bad debt | `invoice_not_collectible` |

```sql
UPDATE invoice_dunning_runs
SET state = 'resolved', resolution = 'invoice_voided',  -- see table above
    resolved_at = now(), next_action_at = NULL
WHERE id = '<run_id>';

INSERT INTO invoice_dunning_events (tenant_id, run_id, invoice_id, event_type, state, reason)
VALUES ('<tenant_id>', '<run_id>', '<invoice_id>', 'resolved', 'resolved', 'invoice_voided');
```

Do not write `manually_resolved`. It is the legacy value from before
migration 0170 that meant "voided OR written off". The CHECK constraint
keeps it legal only so rows predating the split stay readable.

**This SQL resolves the run only. It does not touch the invoice.** The
endpoint propagates the matching invoice change (void / mark-uncollectible /
record-payment); this SQL does not. Flip the invoice too. Otherwise you
leave the pair disagreeing, exactly the shape of the one row the 0170
backfill could not map.

Use only when the dashboard "Resolve" action is unavailable. Record
this action in the audit log.

### Force-mark an invoice paid (offline payment received)

Use the dashboard's `Mark as paid` action. Direct SQL alternative:

```sql
-- payment_status has a CHECK (pending/processing/succeeded/failed/unknown) —
-- 'paid' is an INVOICE status, not a payment_status. Mirror what MarkPaid does:
UPDATE invoices
SET status = 'paid', payment_status = 'succeeded',
    amount_paid_cents = amount_due_cents, amount_due_cents = 0,
    paid_at = now(), auto_charge_pending = false, updated_at = now()
WHERE id = '<invoice_id>';
```

Audit log this action manually if running SQL directly.

### A customer has no usable payment method on file

There is no `setup_status` flag to flip: the `customer_payment_setups`
table was dropped (migration 0097). Saved cards live in the
`payment_methods` table, written by the Stripe `setup_intent.succeeded` /
`payment_method.attached` webhooks. If a webhook was missed, do not flip
a SQL flag. Instead:
- re-drive it (re-send from the Stripe dashboard), or
- have the customer re-add a card via the hosted payment-setup page.

## Logs to grep when paged

Velox uses structured logging via `slog` (Go's standard structured
logger). Useful greps:

```bash
# Billing cycle errors
grep "billing cycle complete" log | jq 'select(.errors > 0)'

# Auto-charge failures
grep "auto-charge failed" log

# Webhook delivery failures
grep "webhook delivery failed" log

# Tax provider failures
grep "tax outcome" log | jq 'select(.outcome == "failed")'

# Scheduler last run
grep "billing cycle started" log | tail -5
```

Trace IDs (`Velox-Request-Id` header) propagate across logs and
appear in error responses. Paste a request ID into your log
aggregator to see the full request chain.

## Escalation

For SEV-1 (data leakage, billing-correctness bug, all customer
charges failing):

1. Contain the damage: revoke API keys / stop the API container (no
   read-only mode exists). Pause webhook delivery by deactivating
   endpoints (PATCH active=false — keeps the signing secret).
2. Snapshot DB state for forensic review.
3. Assemble responders: backend lead + DBA + (if customer-facing)
   support lead.
4. Communicate: status page, affected customer notifications.
5. Postmortem within 5 business days.

For SEV-2 (subset of customers affected; financial impact bounded):

1. Identify affected scope via DB query.
2. Page on-call engineer (do not wait for next business day on
   billing issues).
3. Patch + retroactive correction (credit notes, manual reconcile).
4. Postmortem.

For SEV-3 (small operator UX issue, edge case):

- File an issue, schedule for next sprint.
