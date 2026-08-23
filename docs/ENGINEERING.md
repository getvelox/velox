# Engineering

Velox is an open-source usage-based billing engine (Go + PostgreSQL). This page is the short version of how it is engineered, for engineers who have not seen the repo: what is guaranteed, what was measured, what
gates a change that touches money, what was decided and reversed, and what the
measurements found wrong with Velox itself. Every claim links to the artifact
behind it.

---

## 1. The guarantee is a database constraint

Exactly-once billing in Velox is **not** application logic. It is a partial
unique index — a uniqueness rule the database itself enforces on a subset of
rows (migration 0101):

```sql
CREATE UNIQUE INDEX idx_invoices_billing_idempotency
    ON invoices (tenant_id, subscription_id, billing_period_start, billing_period_end)
    WHERE status <> 'voided' AND source_plan_changed_at IS NULL;
```

A live cycle invoice is a normal billing-period invoice — the voided and
plan-change rows are what the WHERE clause above excludes. If a period already
has one, a second cannot be committed. Not "is unlikely to be" — cannot be,
and the application layer is not a second line of defence. What makes that a
measurement rather than a claim is the negative control below: the same
experiment with the safety deliberately removed, proving the measurement can
see failure. ([failure-correctness.md](benchmarks/failure-correctness.md))

## 2. What was measured

- **[Correctness under failure](benchmarks/failure-correctness.md)** — a real
  billing process killed (`SIGKILL`) mid-run at five different points — each
  chosen by watching the database state, not by a timer — with a second
  process then running the same cycle. 40 subscriptions, $1,000 of periods: **0 duplicate invoices, 0 lost invoices, 0
  cents of drift** at every kill point. The negative control — the same
  run, four leader processes racing, with the index dropped — billed **103 invoices for 40 periods,
  $2,575.00 instead of $1,000.00, and every leader reported success** (a second
  run of it: 129 invoices, $3,225.00). Nothing in the application layer noticed.
- **[Sustained throughput](benchmarks/sustained-throughput.md)** — on a
  `db.m7g.4xlarge`, **12,000 events/sec at p99 22.6 ms** and **15,000 at p99
  43.8 ms**, each holding 4 of 5 ten-minute repeats on stock RDS settings. A
  third, instrumented run caught the tail stalls live and attributed them to
  WAL-segment creation when RDS's recycled-segment pool runs dry. With the pool
  sized — `min_wal_size = max_wal_size = 16 GB`, two dynamic settings, no
  reboot — both rates run **5 of 5** (12,000 with a worst 10-second p99 of
  52 ms; 15,000 at p99 40.2–42.8 ms). Every event is reconciled against
  Postgres — a run whose sent count does not match the rows stored is discarded
  rather than reported — and `pgbench` on the same row shape supplies the
  baseline for what the hardware itself can do. Stalls the WAL mechanism does
  not explain are named as unexplained rather than left out.

Both pages carry a *What this does not show* section, and the throughput page
keeps a table of its own superseded figures. The retractions are part of the
result.

## 3. Two protocols gate money-path work

- **[Money-path robustness playbook](dev/money-path-robustness-playbook.md) —
  how to build.** It exists because of one PR
  ([#325](https://github.com/getvelox/velox/pull/325)): a dunning change reviewed four
  times, where each round caught a *different* instance of the *same* root
  problem and each fix exposed the next. The failure wasn't "review harder", it
  was reasoning *locally* about the function in the diff when the real surface
  was the whole state machine. Hence the rule — enumerate the state's complete
  site-set — every place that writes it, fires an effect from it, reads it
  behind a guard, calls into it, or can crash mid-change — before writing a
  line. Twelve gates; a "no" blocks the PR.
- **[Manual-test strategy](dev/manual-test-strategy.md) — how to prove.** "I
  clicked it and nothing broke" is not a test result here. Every flow is checked
  from five angles (behavior, money math, honest UI copy, design, and what the
  screen actually looks like), there are rules for what must happen before a
  test checkbox may be ticked, and the document's list of testing techniques
  has one admission rule: a technique is only listed if it once caught a real
  bug in this codebase — where a technique cites a PR, that PR records the bug
  it caught. The document describes what actually happens, not what we aspire to.

## 4. Three decisions, one reversal, and one thing the monitoring missed

- **PaymentIntent-only Stripe** ([ADR-001](adr/001-paymentintent-only-stripe.md)).
  Velox owns invoices, dunning and the payment lifecycle end-to-end; Stripe
  executes the card charge as a plain PaymentIntent. No Stripe Billing objects.
- **Coupons cut after shipping** ([ADR-039](adr/039-cut-coupons-pre-launch.md)).
  A full coupon surface shipped in a Stripe-parity sprint and was deleted once
  it was clear nobody had asked for it — the credit ledger is the discount
  primitive.
- **The timezone decouple, designed and deliberately not built**
  ([ADR-092](adr/092-split-billing-timezone-from-display.md)). Prototyped,
  adversarially reviewed, then recorded with a named build trigger instead of
  built; the one-zone handling ([ADR-091](adr/091-org-timezone-change-seam-absorb.md))
  already closes the only actual defect.
- **One reversal** ([ADR-074](adr/074-subscription-billing-timezone-snapshot.md) →
  [ADR-077](adr/077-org-level-billing-timezone.md)): a per-subscription
  timezone snapshot shipped, then was deleted — the organization is the unit a
  timezone attaches to, and the snapshot caused a class of about eight display
  bugs. The superseded ADR records what was wrong with it.
- **What the monitoring missed.** The deliberately sabotaged negative-control
  database from the correctness benchmark — left holding 129 invoices where 40
  should exist — was swept by `velox-doctor` before its 28th check existed:
  all **27 checks reported zero violations**
  ([failure-correctness.md](benchmarks/failure-correctness.md)). Its other
  limit: the sweep must run on an admin connection — under a
  row-level-security-scoped role it sees zero rows and reports clean.

## 5. The benchmarks found two defects, and both are ours

Filed against Velox, with the evidence, beside the numbers they affect — one
fixed and re-measured, one open.

- **[#818](https://github.com/getvelox/velox/issues/818) — a hot row, not the
  hardware.** Every request updated `last_used_at` on the API key it
  authenticated with. One high-volume client is one row, and a row lock hands
  off at roughly one round trip (~1.75 ms) — **~570 requests/s, on any
  hardware**. Found by sampling `pg_stat_activity` during a rate that would not
  hold: 50–55 of 61 backends waiting on that one `UPDATE`. Fixed and re-measured
  on the rig: at batch 10 (ten events per request) the knee — where the latency
  curve bends upward — moved from ~570 req/s to between 1,600 and 2,000, and
  12,000 ev/s now holds at p99 22.6 ms (4 of 5 stock repeats; 5 of 5 with the
  WAL pool sized as above).
- **[#819](https://github.com/getvelox/velox/issues/819) — a linear scan.** The
  per-customer usage summary is a `COUNT + SUM GROUP BY meter` over the
  customer's rows with no rollup: about **2.7 µs per event**, so above ~180k
  events per customer per month it misses a 500 ms read budget regardless of
  write rate. Filed with the number; not yet fixed. The read gate is reported
  separately from the ingest gate for exactly this reason — at 10,000 ev/s
  ingest held and the summary missed its budget, and both are true.

---

Deeper: [`docs/adr/`](adr/) (112 decision records, including the ones that were
reversed) · [architecture](../README.md#architecture) · [the invariants machines
enforce](../README.md#engineering) · [self-hosting](self-host.md) ·
[Postgres requirements](ops/postgres-requirements.md)
