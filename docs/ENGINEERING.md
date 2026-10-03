# Engineering

Velox is an open-source usage-based billing engine (Go + PostgreSQL). This page
is the short version of how it is engineered, for engineers who have not seen
the repo. It covers what is guaranteed, what was measured, what gates a change
that touches money, what was decided and reversed, and what the measurements
found wrong with Velox itself. Every claim links to the artifact behind it.

In short:

- **No double billing under crashes.** 0 duplicate and 0 lost invoices across
  five `SIGKILL` points. With the index dropped and four leader processes racing, the run billed 103
  invoices for 40 periods.
- **Throughput.** 12,000 events/sec at p99 22.6 ms on a `db.m7g.4xlarge`,
  passing 4 of 5 ten-minute repeats on stock settings (5 of 5 with the WAL
  pool sized).
- **Two defects in Velox itself**, found by the benchmarks: #818 is fixed and
  #819 is open.

---

## 1. The guarantee is a database constraint

Exactly-once billing in Velox is **not** application logic. It is a partial
unique index: a uniqueness rule the database itself enforces on a subset of
rows (migration 0101):

```sql
CREATE UNIQUE INDEX idx_invoices_billing_idempotency
    ON invoices (tenant_id, subscription_id, billing_period_start, billing_period_end)
    WHERE status <> 'voided' AND source_plan_changed_at IS NULL;
```

A live cycle invoice is a normal billing-period invoice. The WHERE clause above
excludes voided invoices and plan-change invoices. If a period already has a
live cycle invoice, the database cannot commit a second one. It is impossible,
not merely unlikely. The application layer is not a second line of defence
behind it: the index is the only guard.

The negative control below makes this a measurement rather than a claim. It is
the same experiment with the safety deliberately removed, which proves the
measurement can see failure.
([failure-correctness.md](benchmarks/failure-correctness.md))

## 2. What was measured

- **[Correctness under failure](benchmarks/failure-correctness.md).** A real
  billing process (the leader) is killed with `SIGKILL` mid-run at five
  different points. Each point is chosen by watching the database state, not
  by a timer. A second process then runs the same cycle. With 40
  subscriptions, one period each and $1,000 in total, every kill point gave
  **0 duplicate invoices, 0 lost invoices, 0 cents of drift**.
  - Negative control: the same run with four leader processes racing and the
    index dropped. It billed **103 invoices for 40 periods, $2,575.00 instead
    of $1,000.00, and every leader reported success**. A second run of it
    billed 129 invoices, $3,225.00. Nothing in the application layer noticed.
- **[Sustained throughput](benchmarks/sustained-throughput.md)** on a
  `db.m7g.4xlarge`. A rate counts as passing a repeat only when that repeat
  meets its gate on the throughput page.

| Setting | Result |
|---|---|
| Stock RDS settings | **12,000 events/sec at p99 22.6 ms** and **15,000 at p99 43.8 ms**, each passing 4 of 5 ten-minute repeats |
| Cause of the tail stalls | A third, instrumented run caught them live: WAL-segment creation when RDS's recycled-segment pool runs dry |
| WAL pool sized: `min_wal_size = max_wal_size = 16 GB` (two dynamic settings, no reboot) | Both rates run **5 of 5**: 12,000 with a worst 10-second p99 of 52 ms; 15,000 at p99 40.2–42.8 ms |

Every event is reconciled against Postgres. A run whose sent count does not
match the rows stored is discarded rather than reported. `pgbench` on the same
row shape supplies the baseline for what the hardware itself can do. Stalls the
WAL mechanism does not explain are named as unexplained rather than left out.

Both pages carry a *What this does not show* section, and the throughput page
keeps a table of its own superseded figures. The retractions are part of the
result.

## 3. Two protocols gate money-path work

- **[Money-path robustness playbook](dev/money-path-robustness-playbook.md):
  how to build.** The rule: before writing a line, list the state's complete
  site-set. That is every place that writes the state, fires an effect from
  it, reads it behind a guard, calls into it, or can crash mid-change. The
  playbook has twelve gates (yes/no checklist questions), and a "no" blocks
  the PR.
  - Why it exists: one PR ([#325](https://github.com/getvelox/velox/pull/325)),
    a dunning change (dunning is the automatic retrying and reminding for an
    unpaid invoice). It was reviewed four times. Each round caught a
    *different* instance of the *same* root problem, and each fix exposed the
    next. Reviewing harder was not the answer. The cause was reasoning
    *locally* about the function in the diff, when the real surface was the
    whole state machine.
- **[Manual-test strategy](dev/manual-test-strategy.md): how to prove.** "I
  clicked it and nothing broke" is not a test result here.
  - Every flow is checked from five angles: behavior, money math, honest UI
    copy, design, and what the screen actually looks like.
  - Rules say what must happen before a test checkbox may be ticked.
  - The list of testing techniques has one admission rule: a technique is
    listed only if it once caught a real bug in this codebase. Where a
    technique cites a PR, that PR records the bug it caught.

  The strategy documents current practice, not aspirations.

## 4. Three decisions, one reversal, and one thing the monitoring missed

- **PaymentIntent-only Stripe** ([ADR-001](adr/001-paymentintent-only-stripe.md)).
  Velox owns invoices, dunning and the payment lifecycle end-to-end. Stripe
  executes the card charge as a plain PaymentIntent; Velox creates no Stripe
  Billing objects. Why: Stripe Billing adds a 0.5% fee, and using it would mean
  two systems tracking the same invoices.
- **Coupons cut after shipping** ([ADR-039](adr/039-cut-coupons-pre-launch.md)).
  A full coupon surface shipped in a Stripe-parity sprint. It was deleted once
  it was clear nobody had asked for it. The credit ledger is the discount
  primitive.
- **Splitting the billing timezone from the display timezone: designed and
  deliberately not built**
  ([ADR-092](adr/092-split-billing-timezone-from-display.md)). The split was
  prototyped and adversarially reviewed. It was then recorded, not built,
  together with a named condition that would trigger building it. The
  existing fix that absorbs the billing-period seam when the single org
  timezone changes
  ([ADR-091](adr/091-org-timezone-change-seam-absorb.md)) already closes the
  only actual defect.
- **One reversal** ([ADR-074](adr/074-subscription-billing-timezone-snapshot.md) →
  [ADR-077](adr/077-org-level-billing-timezone.md)). A per-subscription
  timezone snapshot shipped and was then deleted. The organization is the unit
  a timezone attaches to, and the snapshot caused a class of about eight
  display bugs. The superseded ADR records what was wrong with it.
- **What the monitoring missed.** `velox-doctor` is a read-only command that
  scans a Velox database for states no correct code could produce. It swept
  the deliberately sabotaged negative-control database from the correctness
  benchmark, which held 129 invoices where 40 should exist. At that time the
  28th check did not yet exist, and all **27 checks reported zero violations**
  ([failure-correctness.md](benchmarks/failure-correctness.md)). Its other
  limit: the sweep must run on an admin connection. Under a
  row-level-security-scoped role it sees zero rows and reports clean.

## 5. The benchmarks found two defects, and both are ours

Both are filed against Velox, with the evidence, beside the numbers they
affect. One is fixed and re-measured; one is open.

- **[#818](https://github.com/getvelox/velox/issues/818): a hot row, not the
  hardware.** Every request updated `last_used_at` on the API key it
  authenticated with. One high-volume client is one row. A row lock hands off
  at roughly one round trip (~1.75 ms), so the limit is **~570 requests/s, on
  any hardware**. It was found by sampling `pg_stat_activity` during a rate
  that would not hold: 50–55 of 61 backends were waiting on that one `UPDATE`.
  Fixed and re-measured on the rig at batch 10 (ten events per request):
  - The knee, where the latency curve bends upward, moved from ~570 req/s to
    between 1,600 and 2,000.
  - 12,000 ev/s now holds at p99 22.6 ms (4 of 5 stock repeats; 5 of 5 with
    the WAL pool sized as above).
- **[#819](https://github.com/getvelox/velox/issues/819): a linear scan.** The
  per-customer usage summary is a `COUNT + SUM GROUP BY meter` over the
  customer's rows with no rollup. It costs about **2.7 µs per event**, so above
  ~180k events per customer per month it misses a 500 ms read budget, whatever
  the write rate. Filed with the number; not yet fixed. This is why the read
  gate is reported separately from the ingest gate. At 10,000 ev/s, ingest held
  and the summary missed its budget, and both are true.

---

Deeper: [`docs/adr/`](adr/) (112 decision records, including the ones that were
reversed) · [architecture](../README.md#architecture) · [the invariants machines
enforce](../README.md#engineering) · [self-hosting](self-host.md) ·
[Postgres requirements](ops/postgres-requirements.md)
