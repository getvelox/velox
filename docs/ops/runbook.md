# Operations Runbook

What pages the on-call engineer, what doesn't, and what to do when
Velox is in trouble. Written for whoever operates a Velox deployment.
Velox-specific failure modes only — generic Postgres / Kubernetes (K8s) /
Stripe troubleshooting is out of scope.

## Health endpoints

| Endpoint | Purpose | Use for |
|---|---|---|
| `GET /health` | Liveness — process running | Kubernetes liveness probe |
| `GET /health/ready` | Readiness — DB reachable (ping), scheduler ran recently | Kubernetes readiness probe + LB health check |
| `GET /metrics` | Prometheus scrape (Bearer `METRICS_TOKEN` when set) | Metrics-collection job |

`/health/ready` returns 503 if the scheduler (the background loop that
drives billing) hasn't ticked in 2× the configured interval — a stalled
scheduler is caught by the readiness probe, without waiting for a
liveness restart. See "Scheduler stalled" below.

## Key metrics to alert on

All metrics are exported under `/metrics`. The set below is the
alerting tier — what should page someone vs. what's informational.

### Page (critical)

| Metric | Threshold | What it means |
|---|---|---|
| `velox_http_request_duration_seconds` p99 | > 5s for 5m | Server is unhealthy |
| `velox_billing_cycle_errors_total` | rate > 0.1/s for 5m | Billing cycles failing systematically |
| `up{job="velox"}` | == 0 | Process down |
| Postgres connection errors (count) | > 10/min | DB connectivity broken |
| `time() - velox_scheduler_last_run_timestamp_seconds` | > 2× tick interval | Scheduler stalled (also flips `/health/ready` to 503) |
| `velox_leader_last_tick_age_seconds{role}` | > 3× the role's interval | The role has not finished a tick on ANY replica — the cluster-wide stall signal a per-replica liveness gauge cannot give. `SELECT * FROM leader_status;` shows the holder and whether an operator paused the role ([runbook-leader-leases.md](runbook-leader-leases.md)) |
| `increase(velox_leader_lease_lost_total[1h])` | > 0 | A leader could not keep its lease mid-tick (frozen process, DB stall, pooler hiccup). Correctness held — fence + row CAS — but find out why |

### Warn (slack/email, not page)

| Metric | Threshold | What it means |
|---|---|---|
| `velox_payment_charges_total{result="failed"}` | rate spikes 5× baseline | Stripe issue or systematic decline |
| `velox_dunning_runs_processed_total{outcome="failed"}` | rate > 0.5/s | Dunning machinery struggling |
| `velox_webhook_deliveries_total{status="failed"}` | sustained failure | Customer's webhook endpoint down or signature wrong |
| `velox_stripe_breaker_state` | == 2 (open; 0=closed, 1=half_open) | Stripe API circuit-breaker tripped |
| `velox_email_outbox_pending` | > 1000 | Email dispatcher stuck or SMTP provider issue (−1 = the metric query itself failed) |
| `velox_webhook_outbox_pending` | > 1000 | Webhook dispatcher stuck (−1 = metric query failed) |
| `velox_creditnote_pending_tax_reversals` | any row older than 24h (alert on age, not presence) | An issued credit note whose upstream tax reversal is still owed; the sweep re-drives it every tick and keeps it forever (promoted on first failure, 2026-08-30). Presence for a tick or two is normal; a day means the provider keeps rejecting it — tier-2 below |
| `velox_invoice_pending_tax_reversals` | any row older than 24h | A voided `stripe_tax` invoice whose committed tax transaction has no confirmed reversal; the #310 sweep re-drives it — same tier-2 |
| `velox_creditnote_pending_issue_drafts` | sustained growth over days | Clawback drafts not issuing. NOTE: drafts deferred behind an in-flight source payment (ADR-059) sit here legitimately and do NOT appear in error logs — the reconciler's eligibility scan skips them by design until the source settles. Alert on growth/age, not presence. If this gauge and `velox_parked_invoices` move together, the drafts are waiting on parked invoices: resolve those (write them off) and these drain on the next reconciler pass. |
| `velox_parked_invoices{mode}` | > 0, sustained | An invoice whose charge attempt could not be identified with the provider (ADR-107). It is deliberately unchargeable — no sweep, no dunning retry and no operator "Collect payment" will touch it, which is what makes a double charge unreachable. **It will not resolve on its own and needs a human.** Find the attempt in Stripe by customer + amount + approximate time: if it succeeded, its webhook settles the invoice (no action); if nothing was charged, mark the invoice uncollectible — the only action the invoice page offers, and the one that releases its deferred clawback draft and closes its dunning run. Alert on presence, not growth: at zero customers the expected value is zero, and each one is a real invoice a real customer cannot pay. |
| `velox_http_requests_total{method="POST",path="/v1/integrations/litellm/spend",status="503"}` | any increase | LiteLLM spend batches were refused for retry (usage store unreachable). If LiteLLM's proxy log shows `Generic API Logger Error sending batch`, its retries were exhausted — run §8 |
| `velox_auto_charge_retries_total{result="failed"}` | growing rapidly | Many invoices stuck in retry |
| `velox_audit_write_errors_total{outcome="row_lost"}` | rate > 0/s | **Evidence PERMANENTLY LOST.** The mutation committed and its audit row did not, and nothing retries it. Irrecoverable — the row cannot be reconstructed. Treat as a compliance incident: capture the tenant from the ERROR LOG (the metric deliberately carries no tenant label — see below), and identify the affected mutations by their absence. |
| `velox_audit_write_errors_total{outcome="mutation_refused"}` | rate > 0/s | **Nothing is missing.** The audit write failed INSIDE the business transaction, so ADR-090's shared fate rolled the mutation back with it. This is an availability problem, not an evidence problem — the customer got an error, and the log is intact. Investigate the DB, not the audit trail. |

> **Why the audit metric carries no `tenant_id` label.** Prometheus client counters
> never age out, so a raw-tenant label mints a permanent time series for every tenant
> that ever has a single failure — unbounded cardinality that grows with the customer
> base and outlives the incident. One shared-DB blip during a busy hour could mint
> thousands. The tenant is in the **error log**, which is queryable and expires; the
> metric answers only *"is evidence at risk, and in which direction"*.
| `velox_audit_uncovered_mutation_total{route}` | any increase | A route mutated state and wrote NO audit row. Should be **flat zero**: every mutating route is declared in `internal/api/audit_routes.go` as `explicit` (it emits) or `exempt` (it doesn't need to). A non-zero counter means one of three things, in order of likelihood: (1) a route declared `explicit` has an emission path that can be skipped — find it via the `route` label and the `UNCOVERED MUTATION` error log; (2) a genuinely non-mutating 2xx path (a cache/idempotency replay, a no-op save) needs an `audit.MarkSkip` declaration; (3) a new route shipped without a declaration — impossible via CI (the route-walk test fails the build), but possible if the registry was edited to silence it. Do not "fix" this by adding an exemption without recording what is being given up. |

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
aren't being invoiced; `velox_billing_cycles_total` rate drops to 0.

**Why it happens**:
- Long-running transaction holding row locks (e.g., a tenant with
  millions of usage events on a single subscription).
- DB primary failover; connections lost mid-tick.
- Scheduler goroutine — its background worker thread — panic'd (rare;
  `slog.Error` logs it).

**Diagnose**:
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
   `cmd/velox/main.go`) and is not env-configurable today. A
   hot-spotting tenant (one tenant dominating the batch) is drained
   on demand with `POST /v1/billing/run` (per tenant, loops until empty)
   rather than by shrinking the batch.

### 2. Email outbox backed up

**Symptom**: `email_outbox` table growing past 1000 rows in
`status='pending'`; customers report missing invoice emails.

**Why**:
- SMTP provider rate-limiting or down.
- Provider rejected mail (auth, sender domain, etc).
- Dispatcher stopped (rare).

**Diagnose**:
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

A `last_error` beginning `email outbox: unknown email_type` on a *pending*
row means a replica older than the row's producer claimed it (rolling
deploy); it retries on the backoff ramp and a replica that knows the type
delivers it. The same text on a *failed* row means no replica in the fleet
knew the type for the whole ramp — the producer shipped without its
dispatcher case.

**Fix**:
1. Diagnose SMTP provider via `last_error`.
2. Once provider is healthy, the dispatcher drains automatically;
   pending rows fire on their `next_attempt_at` schedule.
3. To speed recovery, mass-reset `next_attempt_at` to now:
   ```sql
   UPDATE email_outbox SET next_attempt_at = now()
   WHERE status = 'pending' AND attempts < 15;
   ```
4. Failed rows (dead-lettered — "DLQ'd" — set aside after delivery
   gave up): investigate root cause, then either fix-and-
   retry (`UPDATE ... SET status='pending', attempts=0`) or accept
   the loss and alert affected customers.

### 3. Webhook outbox backed up

**Symptom**: Same as email outbox but for `webhook_outbox`.

**Why**:
- Customer's webhook endpoint is down or rejecting.
- HMAC signature mismatch — the shared-secret signature on each
  delivery no longer verifies (customer rotated secret without telling
  Velox).

**Diagnose**:
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
2. If signature mismatch: rotate signing secret in dashboard
   (`Webhooks → Endpoint → Rotate secret`), customer updates their
   side, replay failed events.

### 4. Dunning circuit breaker open

**Symptom**: `velox_stripe_breaker_state == 2` (open; 1 = half-open
probing); dunning retries — the automated follow-up attempts on failed
payments — are silently skipping (correct behaviour);
customers report they expected retries but no email arrived.

**Why**:
- Stripe API has been failing repeatedly; the circuit breaker (which
  stops calling a dependency that keeps failing, until it recovers)
  tripped to protect the retry budget.
- Tenant's Stripe credentials are invalid (per-tenant breaker).

**Diagnose**:
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
  against Stripe) isn't running (single-instance assumption broken?).
- The Stripe PaymentIntent (PI) is parked at `requires_action` — an
  off-session SCA challenge (Strong Customer Authentication, a bank
  verification step) that nobody completes. The reconciler resolves
  only TERMINAL Stripe outcomes — it deliberately skips in-flight PIs
  every sweep, so these never
  self-heal: cancel the PI in Stripe (the reconciler then settles it
  failed) or get the customer to complete authentication.

**Diagnose**:
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
   sweeps automatically every tick; per invoice, use the dashboard's
   invoice attention actions (charge now / retry) — the reconciler's
   next pass also self-heals any invoice whose PI reached a terminal
   state at Stripe.

### 6. Test-clock advance hung

**Symptom**: `test_clocks.status='advancing'` for >5min; operator
sees Advancing badge stuck.

**Why**:
- Catchup loop processing many cycles — a test clock simulates time so
  billing can be exercised without waiting, and a large jump on a
  monthly sub can require dozens of billing-engine sweeps.
- Billing-engine error mid-catchup; sub flipped to
  `internal_failure`.
- A deploy abandoned an in-flight advance (shutdown waits 30s for it,
  then exits); the clock stays `advancing` until **any replica
  (re)starts** — recovery runs once at boot, never on a schedule. In a
  rolling deploy the new replica has usually already booted, so nothing
  will pick it up: restart one replica. (Program ha-9 makes recovery a
  scheduled leader tick; until then this is the operator action.)

**Diagnose**:
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
1. If progress is happening (invoices being generated), wait — large
   jumps take time.
2. If `internal_failure`, retry the advance: the dashboard's
   `Retry advance` button (or `POST /v1/test-clocks/<clock_id>/retry-advance`)
   flips the clock back to `advancing` and resumes catchup from where it
   stopped (recorded in ADR-018 — an architecture decision record).
   Delete only as a last resort — since ADR-086, clock
   deletion is a complete teardown of the clock's simulated data.

### 7. RLS leakage suspected

**Symptom**: A tenant reports seeing another tenant's data, OR a
support ticket includes data from a different tenant than the
operator's session.

**This is a SEV-1** (highest-severity incident). A leak across RLS
(Row-Level Security — the Postgres mechanism that hides each tenant's
rows from every other tenant) is the worst-case bug.

**Diagnose**:
1. Lock down. Velox has NO read-only mode — contain by revoking API
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

### 9. A replica will not start: "REDIS_URL is required" / "Redis is invalid or unreachable"

Production refuses to boot without a reachable Redis (2026-08-30): the
general and hosted-invoice rate limiters fail CLOSED without it, so a
Redis-less replica would boot green and answer 429 to its share of
traffic — including hosted-invoice pay pages — while `/health/ready` stays
ok. The refusal is the fix. Check `REDIS_URL`, the security group / network
path to 6379, and that the managed Redis is up; the replica starts on the
next attempt. Redis going down AFTER boot is a different situation: general
and hosted-invoice requests 429 (fail closed), ingest and `/v1/auth` keep
working (fail open) — restore Redis, no restart needed.
### 8. LiteLLM spend gap after an outage

**Symptom**: the `…/litellm/spend` 503 counter rose during a Postgres
failover or a rolling restart, and LiteLLM's proxy log shows
`Generic API Logger Error sending batch` (its `max_retries` were spent) —
or LiteLLM was never configured with retries (`docs/integrations/litellm.md`).

**What was lost**: every LLM call LiteLLM flushed in that window. Velox's
spend view will be short against LiteLLM's spend page for the period.

**Fix**: replay LiteLLM's spend logs for the window to
`POST /v1/integrations/litellm/spend`. Every row is idempotency-keyed
(`<litellm_call_id>:input|output|cache_read`), so a replay of a window that
was partly recorded is a pure gap-fill — rows already held come back as
`deduplicated`, never double-counted. Hard deadline: the period must not be
finalized yet; a replay landing in a finalized period increments
`velox_usage_late_event_total` and needs a manual credit/debit.

## After a large backfill, expect a few minutes of elevated write I/O

Measured on the benchmark rig (2026-08-16): a 20M-row bulk load into
`usage_events` was followed by ~5 minutes of RDS write IOPS at ~2× baseline —
autovacuum and the checkpointer working through the load — and one commit
stall of ~5 s about 20 s after ingest resumed at 200 ev/s. Steady traffic in
the following 90 minutes across three rates showed nothing similar. This is
Postgres maintenance, not a Velox defect: if you backfill tens of millions of
events (`POST /v1/usage-events/backfill`, or a direct load) and then need
sub-10 ms p99 immediately, wait for `WriteIOPS` to return to baseline first, or
schedule large backfills off-peak. Watch `ReadIOPS` too — a tail that rises
with CPU flat is the index working set falling out of cache; see
`docs/benchmarks/sustained-throughput.md` for the numbers on `db.m7g.2xlarge`.

## On RDS, size the WAL segment pool — the defaults let commits stall after a quiet spell

Measured on the benchmark rig (2026-08-17, `db.m7g.4xlarge`, 100 GB gp3, PG
16.14, 12,000 events/s, 1-second instrumentation): the tail events the third
run caught are **WAL segment creation under `WALWriteLock`.** RDS uses 64 MB
WAL segments and keeps a pool of pre-made ones, refilled by recycling at each
checkpoint completion to about one checkpoint's worth — with less than one
segment of margin at this write rate, by the recycling arithmetic in
`xlog.c`. When the pool runs dry, the committing backend that needs the next
segment writes 64 MB of zeros and fsyncs it while every other commit waits:
~0.2–0.3 s of frozen commits every ~5 s until the next checkpoint refills the
pool. p99 jumps 5–10×; p50 does not move; CPU, memory and CloudWatch's
60-second I/O averages look normal (the stall minute read 1,039 IOPS, 42 MB/s,
queue depth 1.3). Two things drain the pool, both after a quiet spell: small
checkpoints decay the checkpointer's distance estimate so fewer old segments
are recycled (`min_wal_size`, RDS default 192 MB, is the floor that would stop
it), and RDS's retained segments (`wal_keep_size` 2 GB) sit inside the
`max_wal_size` window after an idle, so the pool on resume is shallower than
in steady state and the first timer-driven checkpoint refills too late
(`max_wal_size` bounds the depth). Both were reproduced on demand; either
knob alone was not enough (provocation arms A–C in
`docs/benchmarks/sustained-throughput.md` § third run).

**Do:** in the instance's parameter group set **`min_wal_size = max_wal_size ≥
wal_keep_size + (1 + checkpoint_completion_target) × peak WAL rate ×
checkpoint_timeout, with margin`** — at 12–15k events/s (14–20 MB/s of WAL) that
is 2 + 8–11 GB, so **16 GB**; at 25,000 events/s WAL ran at 29.6 MB/s and the pool at 16 GB still hit zero twice in 35 minutes — size **24–32 GB** there, or shorten `checkpoint_timeout`. Both are dynamic (no reboot). Check the log at
peak: checkpoints should read `checkpoint starting: time`, not `wal` — a
WAL-driven checkpoint puts you back on the one-segment margin. Cost is disk:
`pg_wal` sits near that size permanently. Two cautions: (1) raising the
setting does not manufacture segments — the pool grows only as segments are
created, one ~0.25 s commit pause per 64 MB file, until ~16 GB of WAL has been
written: about a minute of pauses in total, paid once in the instance's life.
Invisible if traffic ramps up or an initial data load runs first (the load
creates the files while nobody waits on commits); a rough ~15 minutes if you
start at peak on an empty pool. The alternative is the stock behaviour: the
same pauses after *every* quiet spell, indefinitely; `pg_switch_wal()` is not granted to `rds_superuser`, so there is no cheap
pre-grow on RDS — a bulk load or ~15 minutes of peak-rate traffic grows it
(measured: applying 16 GB to a shallow pool made the next 5 minutes *worse*,
provocation arm D); (2) at higher
write rates than measured, re-derive the number. Verified: with the pool at depth, the idle-then-resume recipe that stalled stock, floor-only, depth-only and freshly-raised settings did not stall (worst 10-s p99 24.7 ms vs 162–254 ms), and a 5 × 10 min series at 12,000 ev/s ran with all checkpoints time-driven, no tail window, and the pool never below 45 segments — one series each, on `db.m7g.4xlarge` + 100 GB gp3.

**How to see it if you suspect it:** `pg_stat_activity` (or the rig sampler)
showing a client backend in `IO:WALInitWrite`/`WALInitSync` with a pile-up on
`LWLock:WALWrite`; `TransactionLogsDiskUsage` growing under load right after it
fell at a checkpoint; Enhanced Monitoring (1 s) showing device writes near the
volume's throughput ceiling with the average request size falling toward
~13 KB while `pg_stat_io` writers are steady. Performance Insights at 1 s
usually misses the ~0.2 s stalls, and a `WALWrite` pile-up alone is not
specific. Numbers and the full attribution: `docs/benchmarks/sustained-throughput.md`
§ third run.

## Autovacuum on a large insert-only table can stall every commit for seconds

Measured on the benchmark rig (2026-08-19, `db.m7g.4xlarge`, 100 GB gp3,
25,000 events/s, stock RDS parameters, vacuum log on): the end of an
insert-triggered autovacuum pass over `usage_events` (PG13+ runs one after
~20 % growth) rewrites nearly every page added since the previous pass —
hint bits and opportunistic freezing, which `data_checksums=on` pushes through
full-page writes: one pass dirtied 540k pages (4.4 GB) and froze 6.6M tuples
at 107 MB/s. RDS's `autovacuum_vacuum_cost_limit` (1,200 on this class, 2 ms
delay) lets it write at the volume's full throughput, so for 1–6 s the disk
queue runs to hundreds with tens of thousands of ~6 KB writes per second and
every commit's WAL write waits behind it: **p50 lifts for everyone** (36 →
64 ms, 96–99 % of requests affected), unlike the WAL-pool stall above, which
leaves p50 alone. Telltale in Enhanced Monitoring: IOs/s ×10–20 while the
average IO size collapses from ~100 KB to ~6 KB. **Do:** set **`autovacuum_vacuum_insert_scale_factor = 0.02`** (dynamic; or
per-table reloptions on the events table). Tested control-vs-treatment on the
same rig at 25,000 events/s: stock's pass at 80M rows froze every commit for
11 s (worst request 3.3 s, worst 1-s p99 ≈2.9 s, dropped requests, failed repeat); at 0.02 the passes
are ~10× smaller, worst freeze 0.63 s, five repeats of five passed with zero
drops, and the median is untouched (55.7 vs 55.9 ms). It bounds each storm to
the ~2 % growth slice instead of 20 % of an ever-growing table — the burst no
longer scales with table size. Measured at 25k batch 100; at lower rates the
residual storms are smaller still. The pacing lever
(`autovacuum_vacuum_cost_delay`/`cost_limit`) was not needed and stays
untested. Details and evidence:
`docs/benchmarks/sustained-throughput.md` § third run, "What this leaves".

## Scheduler interval tuning

The tick interval and batch size are compiled-in, not env-configurable:
the scheduler ticks every **1 hour** in staging/production and **5
minutes** only when `APP_ENV=local`, and processes a fixed **50 subs per
tick** (`cmd/velox/main.go`). A tenant with a backlog is drained on
demand via `POST /v1/billing/run` (loops until that tenant is empty)
rather than by tuning these knobs.

Watch `velox_billing_cycle_duration_seconds` to ensure each tick fits
inside the interval. If a tick runs long, the leader lease (ADR-114 — one
replica holds the `billing` role per tick and renews it while it works)
means the next tick starts one interval after the long one ENDS, on
whichever replica polls first — no collision, no lock wait, just a
lengthening backlog. `velox_leader_last_tick_age_seconds{role="billing"}`
is the cluster-wide lag; see [runbook-leader-leases.md](runbook-leader-leases.md).

## Manual operator interventions

Documented operator-side actions for incidents:

### Force-resolve a stuck dunning run

Pick the resolution that names what actually happened to the invoice — the
column is how finance later answers "why did we stop collecting this?", and
these writes bypass the endpoint that would otherwise enforce it:

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

Do NOT write `manually_resolved` — it is the legacy value from before
migration 0170 that meant "voided OR written off", and the CHECK
constraint keeps it legal only so rows predating the split stay readable.

**This SQL resolves the RUN only — it does not touch the invoice.** The
endpoint propagates the matching invoice change (void / mark-uncollectible /
record-payment); this SQL does not. Flip the invoice too, or you leave the
pair disagreeing — exactly the shape of the one row the 0170 backfill could
not map.

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

There is no `setup_status` flag to flip — the `customer_payment_setups`
table was dropped (migration 0097). Saved cards live in the
`payment_methods` table, written by the Stripe `setup_intent.succeeded` /
`payment_method.attached` webhooks. If a webhook was missed, the fix is
to re-drive it (re-send from the Stripe dashboard) or have the customer
re-add a card via the hosted payment-setup page — not a SQL flag flip.

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
appear in error responses — paste a request ID into your log
aggregator to see the full request chain.

## Escalation

For SEV-1 (data leakage, billing-correctness bug, all customer
charges failing):

1. Stop the bleeding: revoke API keys / stop the API container (no
   read-only mode exists); pause webhook delivery by deactivating
   endpoints (PATCH active=false — keeps the signing secret).
2. Snapshot DB state for forensic review.
3. Assemble responders: backend lead + DBA + (if customer-facing)
   support lead.
4. Communicate: status page, affected customer notifications.
5. Postmortem within 5 business days.

For SEV-2 (subset of customers affected; financial impact bounded):

1. Identify affected scope via DB query.
2. Page on-call engineer (don't wait for next business day on
   billing issues).
3. Patch + retroactive correction (credit notes, manual reconcile).
4. Postmortem.

For SEV-3 (small operator UX issue, edge case):

- File an issue, schedule for next sprint.
