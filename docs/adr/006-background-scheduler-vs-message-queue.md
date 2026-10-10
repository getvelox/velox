# ADR-006: Background Scheduler vs. Message Queue

**Date:** 2026-04-15
**Status:** Accepted

## Status
Accepted

## Date
2026-04-14

## Context
Velox needs to run periodic background work: billing cycle execution (find due subscriptions, generate invoices, charge payments), dunning retry processing (retry failed payments on schedule), and outbound webhook delivery (retry failed deliveries). These workloads are periodic, not event-driven — they poll for work on a timer.

The standard enterprise answer is a message queue (RabbitMQ, SQS) or a workflow engine (Temporal). These provide durability, retry semantics, dead-letter queues, and horizontal scaling. They also add operational dependencies, deployment complexity, and infrastructure cost that may not be justified for a v1 product.

## Decision
Velox v1 uses a simple `billing.Scheduler` — a goroutine with a `time.Ticker` that runs in the same process as the API server. Every tick, it calls `Engine.RunCycle()` for billing and `DunningService.ProcessDueRuns()` for each tenant.

Work distribution and concurrency safety come from PostgreSQL, not the scheduler:

- `GetDueBilling()` uses `SELECT ... WHERE next_billing_at <= $1 ORDER BY next_billing_at LIMIT $2 FOR UPDATE SKIP LOCKED` — multiple scheduler instances (in a horizontal scaling scenario) will not process the same subscription twice
- `ListDueRuns()` uses the same `FOR UPDATE SKIP LOCKED` pattern for dunning runs
- Credit ledger writes use `SELECT ... FOR UPDATE` to serialize concurrent balance mutations

The scheduler is stateless. If the process crashes, the next tick picks up where it left off — `FOR UPDATE SKIP LOCKED` ensures no double-processing, and idempotent PaymentIntent creation (keyed on invoice ID) prevents duplicate charges.

## Consequences

### Positive
- Zero additional infrastructure: no Redis, no RabbitMQ, no Temporal cluster to operate
- Single binary deployment: `go build` produces one artifact that handles HTTP, billing, dunning, and webhooks
- PostgreSQL is already required — using it as a job queue adds no new failure modes
- `FOR UPDATE SKIP LOCKED` provides safe horizontal scaling without a distributed lock service

### Negative
- Billing cycle throughput is limited to what one goroutine can process per tick (mitigated by batch processing and the SKIP LOCKED pattern allowing multiple replicas)
- No built-in dead-letter queue or retry backoff for the scheduler itself (individual operations like payment and webhook delivery have their own retry logic)
- Observability requires structured logging rather than queue-native dashboards
- No fan-out: a single slow subscription blocks the rest of the batch within that tick

### Trade-offs
- We trade scalability ceiling for operational simplicity. A single Velox instance processing 50 subscriptions per tick at 5-minute intervals handles ~600 invoices/hour — sufficient for thousands of customers. When this becomes a bottleneck, adding a second replica with SKIP LOCKED doubles throughput without architecture changes.

## Alternatives Considered
- **Temporal**: Provides durable workflows, automatic retries, and visibility. But it requires a Temporal server cluster (3+ pods), adds ~500ms latency per workflow step, and introduces a significant operational dependency. Rejected for v1 — the complexity is not justified when PostgreSQL SKIP LOCKED provides the core guarantee we need.
- **Redis + worker pool (e.g., Asynq, Faktory)**: Adds Redis as a dependency. Provides faster polling than PostgreSQL but introduces a new failure mode (Redis unavailability). Since our job source-of-truth is already PostgreSQL (subscriptions, dunning runs), adding Redis as an intermediary creates data consistency concerns. Rejected.
- **SQS/PubSub**: Cloud-native but creates vendor lock-in and requires event-driven refactoring. Billing cycles are naturally periodic (run every hour), not event-driven (react to each subscription change). Rejected for poor fit.

## Amendment 2026-10-10 (P27): a tick bills everything due

Two claims above were false.

- **"50 subscriptions per tick at 5-minute intervals handles ~600 invoices/hour."** Production ticks hourly (5 minutes is local dev only), so the cap was 50 invoices per hour per mode. 20,000 subscriptions due on the 1st took about 17 days to invoice.
- **"Adding a second replica with SKIP LOCKED doubles throughput."** Every singleton loop has run on a leader lease since ADR-114. One replica leads billing at a time, and a second replica adds no billing throughput.

**Change.** `RunCycle` now drains: it bills page after page of `batch` due subscriptions until a page comes back empty. `batch` is now the page size, not a per-tick cap. The manual `POST /v1/billing/run` and test-clock Advance use the same loop (`drainDue`), as ADR-065 already did for the threshold scan. Each subscription is attempted at most once per run. A subscription still due after its attempt is excluded from later pages in SQL (`NOT (id = ANY(exclude))`). Without that, a persistently failing subscription is the oldest due row and leads every page, ahead of the healthy ones.

**What a long tick costs.**

- The rest of the billing tick waits for the drain: reconcilers, charge retries and the test-mode pass. The dunning loop is a separate role and keeps running.
- The lease does not expire, because ADR-114 heartbeats every 3 s for the whole tick.
- The replica's liveness stamp (`/health/ready`, `velox_scheduler_last_run_timestamp_seconds`) normally fires only between ticks. The drain now stamps it after every page. Without that, a drain longer than 2× the interval turns `/health/ready` into a 503, which on a single replica takes the API out of the load balancer. A tick wedged inside one subscription completes no page, so it still goes stale.
- `velox_leader_last_tick_age_seconds{role="billing"}` grows during a long drain, the same as for a wedged tick. `velox_billing_due_subscriptions{mode}` tells the two apart: it falls during a drain and stays flat when the tick is wedged.

**No circuit breaker.** A drain does not stop after N consecutive failures. A breaker would bring back the head-of-line block this change removes: a run of failing subscriptions at the front would end the tick before the healthy ones. The broad failures that could make a whole-set drain expensive are already handled. A tax provider outage defers the invoice's tax (`tax_status=pending`) instead of failing the subscription. A database outage fails the page fetch, which ends the drain.

**Deferred: billing subscriptions in parallel.** The drain is serial, and each subscription's close includes its inline charge (a Stripe round trip). Drain time therefore grows with the number of subscriptions due at once. Trigger to revisit: `velox_billing_cycle_duration_seconds` p95 approaching the billing interval.
