# Engineering: the evidence

This page shows the proof behind the rules in [How Velox is
engineered](ENGINEERING.md). It is for an engineer who wants to check those
claims before trusting Velox with money. Each claim links to the test, run or
record behind it.

Velox is a billing system you run yourself, written in Go on PostgreSQL. The
results in short:

- **No double billing under crashes.** A billing process was killed with
  `SIGKILL` at five different points. Each one ended with 0 duplicate
  invoices and 0 lost invoices. A separate control run removed the database
  guard on purpose and billed 103 invoices for 40 periods. That shows the
  test can see a failure when there is one.
- **Throughput.** 12,000 usage events per second at batch 10 (ten events
  per request), p99 22.6 ms, on a `db.m7g.4xlarge` database. It passed 4 of
  5 ten-minute repeats on stock database settings, and 5 of 5 with the WAL pool
  sized.
- **Two defects in Velox itself**, found by the benchmarks: #818 is fixed
  and #819 is open.

---

## 1. The guarantee: a row lock, then a database constraint

Two things stop a billing period being billed twice. First, every cycle close
takes a row lock on the subscription and checks that its period has not moved
([ADR-115](adr/115-one-closer-for-the-billing-period.md)); a writer that loses
the race writes nothing. Second, migration 0101 adds a partial unique index: a
uniqueness rule the database itself enforces on a subset of rows. With it, the
database cannot commit a second live cycle invoice for the same period, even if
the first check is bypassed:

```sql
CREATE UNIQUE INDEX idx_invoices_billing_idempotency
    ON invoices (tenant_id, subscription_id, billing_period_start, billing_period_end)
    WHERE status <> 'voided' AND source_plan_changed_at IS NULL;
```

A live cycle invoice is a normal billing-period invoice. The WHERE clause above
excludes voided invoices and plan-change invoices. If a period already has a
live cycle invoice, the database cannot commit a second one.

The negative control below removes the index and shows the race billing twice,
which proves the measurement can see failure. It was run on 2026-08-15, before
ADR-115 added the period-close lock, so it shows what the index alone was
guarding at that time. It has not been re-run on the current code.
([failure-correctness.md](benchmarks/failure-correctness.md))

## 2. What was measured

- **[Correctness under failure](benchmarks/failure-correctness.md).** A real
  billing process (the leader) is killed with `SIGKILL` mid-run at five
  different points. Each point is chosen by watching the database state, not
  by a timer. A second process then runs the same cycle. With 40
  subscriptions, one period each and $1,000 in total, every kill point gave
  **0 duplicate invoices, 0 lost invoices, 0 cents of drift**.
  - Negative control (2026-08-15, before ADR-115): four billing engines
    racing as goroutines in one test process, with the index dropped. It
    billed **103 invoices for 40 periods, $2,575.00 instead of $1,000.00,
    and every leader reported success**. A second run of it
    billed 129 invoices, $3,225.00. Nothing in the application layer noticed.
- **[Sustained throughput](benchmarks/sustained-throughput.md)** on a
  `db.m7g.4xlarge`. A rate counts as passing a repeat only when that repeat
  meets its gate on the throughput page.

| Setting | Result |
|---|---|
| Stock RDS settings | **12,000 events/sec at batch 10 (p99 22.6 ms)** and **15,000 at batch 100 (p99 43.8 ms)**, each passing 4 of 5 ten-minute repeats |
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
  - Why it exists: one dunning change needed four review rounds, and each
    round found a new case of the same mistake. The mistake was checking only
    the function in the diff, when the change affected the whole state
    machine. (Dunning is retrying a failed card payment and emailing the
    customer.) The story: [#325](https://github.com/getvelox/velox/pull/325).
- **[How we test](testing.md): how to prove.** "I
  clicked it and nothing broke" is not a test result here.
  - Every change ships with an automated test at the right layer: Go tests
    against real Postgres for money and state, Playwright for the dashboard.
  - A fix is proven by watching its test fail without the fix.
  - What only a real provider can show (Stripe, email, the PDF) is checked by
    hand before each release.

## 4. Three decisions, one reversal, and one thing the monitoring missed

- **PaymentIntent-only Stripe** ([ADR-001](adr/001-paymentintent-only-stripe.md)).
  Velox owns invoices, dunning and the payment lifecycle end-to-end. Stripe
  executes the card charge as a plain PaymentIntent; Velox creates no Stripe
  Billing objects. Why: Stripe Billing adds a percentage fee, and using it
  would mean two systems tracking the same invoices.
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
  scans a Velox database for states no correct code could produce. It now
  has 30 checks. On 2026-08-15 it swept the negative-control database from
  the correctness benchmark, which held 129 invoices where 40 should exist.
  It **reported zero violations**, because no check yet looked for a second
  invoice in one period. That check,
  `cycle_invoice_unique_per_subscription_period`, was added after the run
  ([failure-correctness.md](benchmarks/failure-correctness.md)). Its other
  limit: the sweep must run on an admin connection. Under a
  row-level-security-scoped role it sees zero rows and reports clean.

## 5. The benchmarks found two defects, and both are ours

Both are filed against Velox, with the evidence, beside the numbers they
affect. One is fixed and re-measured; one is open.

- **[#818](https://github.com/getvelox/velox/issues/818): a hot row, not the
  hardware.** Every request updated `last_used_at` on the API key it
  authenticated with. One high-volume client is one row. A row lock hands off
  at about one round trip (~1.75 ms on this rig), so one API key was capped at
  **~570 requests/s, whatever the instance size**. It was found by sampling
  `pg_stat_activity` during a rate that would not hold: 50–55 of 61 backends were waiting on that one `UPDATE`.
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

## 6. Checks that machines run

If a mistake can recur, a machine should catch the next one. These run in
CI on every pull request and every push to main:

- **Tenant isolation.** Postgres Row-Level Security (RLS) keeps each
  tenant's rows apart. Tests fail if a query reaches another tenant's rows
  ([`rls_isolation_test.go`](../internal/platform/postgres/rls_isolation_test.go)).
- **One auto-charge per invoice.** Charging claims the invoice with a
  compare-and-swap update: when two claimers race, exactly one wins
  ([`auto_charge_claim_integration_test.go`](../internal/invoice/auto_charge_claim_integration_test.go)).
- **Package boundaries.** A new import between domain packages fails the
  architecture test until it is added to an allowlist
  ([`boundaries_test.go`](../internal/arch/boundaries_test.go)).
- **Test-clock time.** In packages whose time can come from a test clock, a
  bare `time.Now()` fails golangci-lint unless the line says why it needs
  real time (a `forbidigo` rule in [`.golangci.yml`](../.golangci.yml)).
- **velox-doctor, 30 checks.** CI runs it over the data the golden-path demo
  writes through the real API. It also runs inside a 13-month billing soak
  ([`clock_soak_e2e_test.go`](../internal/api/clock_soak_e2e_test.go)): the
  test closes a subscription month 13 times through the real server and
  needs a clean sweep after every close.
- **Generated cases for the money math.** Where there are more cases than
  anyone can list by hand, property tests generate them: billing dates
  ([`billing_dates_property_test.go`](../internal/domain/billing_dates_property_test.go)),
  pricing ([`pricing_property_test.go`](../internal/domain/pricing_property_test.go)),
  proration ([`proration_property_test.go`](../internal/subscription/proration_property_test.go)),
  tax apportionment ([`apportionment_property_test.go`](../internal/tax/apportionment_property_test.go))
  and the credit waterfall ([`waterfall_property_integration_test.go`](../internal/credit/waterfall_property_integration_test.go)).
- **Real Postgres behind the unit tests.** Some unit tests use in-memory
  fakes for speed. The stores, migrations and RLS policies are also tested
  against real Postgres. That includes collision tests, where two claimers
  race on purpose, and mutation checks: break the logic on purpose, and the
  test must fail. The repo has about 138k lines of Go test code and about
  104k lines of hand-written production Go.

Two drills cover failures that only happen in production. They are run by
hand:

- [`scripts/partition-drill.sh`](../scripts/partition-drill.sh) cuts the
  network link between the leader and Postgres, and measures how long until
  another process takes over the leader role: 9 s
  ([failure-correctness.md](benchmarks/failure-correctness.md)).
- [`scripts/restore-drill.sh`](../scripts/restore-drill.sh) runs backup,
  restore and a row-count check against a throwaway Postgres.

---

Related:

- [Decision records](adr/): 120 ADRs, including the reversed ones.
- [All published benchmark runs](scale.md#published-runs).
- [Architecture](architecture.md), [self-hosting](self-host.md) and
  [Postgres requirements](ops/postgres-requirements.md).
- Records: the [full-product audit of 2026-07](records/audit-2026-07-02-full-product.md)
  (117 findings; every high and medium one fixed or deferred by name), and
  the [HA-readiness audit](records/ha-readiness-2026-07-06.md).
