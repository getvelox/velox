# Correctness under failure

What happens to your invoices when the billing process dies mid-run.

This is the one benchmark we think is worth publishing first, because it is the
question a billing system can least afford to answer with a shrug — and the one
most likely to be answered by a slide rather than a test. Everything below is
reproducible from a clean checkout with `docker compose up -d postgres` and one
`go test` command, printed at the bottom.

**Date:** 2026-08-15 · **Commit:** see `git log` for `test/track-a-exactly-once`
· **Hardware:** developer laptop, Postgres 16 in Docker, single node.

---

## The guarantee

Exactly-once billing in Velox is **not** application logic. It is a partial
unique index (migration 0101):

```sql
CREATE UNIQUE INDEX idx_invoices_billing_idempotency
  ON invoices (tenant_id, subscription_id, billing_period_start, billing_period_end)
  WHERE status <> 'voided' AND source_plan_changed_at IS NULL;
```

A second live cycle invoice for a period that already has one cannot be
committed. Not "is unlikely to be" — cannot be. That distinction is the whole
result, and the negative control below is what makes it a measurement rather
than a claim.

---

## 1. Leader killed mid-run

A real billing process bills a seeded tenant of **40 due subscriptions** at
$25.00 each. Partway through, it is `SIGKILL`ed — no graceful shutdown, no
deferred unlock, no chance to finish the row it was on. A second process then
runs the same cycle.

The kill point is chosen by watching the database, not by sleeping, so the kill
lands inside the run every time. It is swept across five positions including
both boundaries — killed after the very first commit, and killed with one
subscription left.

| Killed after | Successor generated | Final invoices | Distinct (sub, period) | Total billed |
|---:|---:|---:|---:|---:|
| 1 | 39 | 40 | 40 | $1,000.00 |
| 5 | 35 | 40 | 40 | $1,000.00 |
| 12 | 28 | 40 | 40 | $1,000.00 |
| 25 | 15 | 40 | 40 | $1,000.00 |
| 39 | 1 | 40 | 40 | $1,000.00 |

**0 duplicate invoices. 0 lost invoices. 0 cents of drift.** The leader's work
and the successor's work are exactly complementary at every kill point.

The test asserts the process died *of* `SIGKILL` (`WaitStatus.Signal()`) rather
than trusting that it did. An earlier version of this harness had the helper
process panic on its own; that also closes the socket, so the test passed while
measuring Go's runtime instead of failover.

## 2. Four leaders racing at once

A crash produces one interleaving. Running four leaders concurrently against the
same due set produces many, including both transactions inside the same
`(subscription, period)` window simultaneously — a state sequential takeover
cannot construct on purpose.

```
4 concurrent leaders: generated=[11 8 12 9]  failures=[0 0 0 0]
final: 40 invoices over 40 distinct (sub,period) tuples, 100000 cents
```

All four leaders did real work (11+8+12+9 = 40), none reported an error, and the
result is still exactly one invoice per subscription.

The test asserts that **at least two leaders generated something**. Without that
assertion this scenario passes vacuously: with a normal batch size the first
leader claims every due subscription before the others finish starting, they
find nothing to do, and a green result means only that three goroutines arrived
late. Forcing one subscription per fetch is what creates the contention.

## 3. The negative control — what happens without the constraint

The same four-leader run with `idx_invoices_billing_idempotency` dropped:

```
4 concurrent leaders: generated=[26 26 26 25]  failures=[0 0 0 0]
final: 103 invoices over 40 distinct (sub,period) tuples, 257500 cents
```

**103 invoices for 40 periods. $2,575.00 billed instead of $1,000.00 — a 2.6×
overbill — and every leader reported success.** A second run of the same
experiment produced 129 invoices and $3,225.00.

Nothing in the application layer noticed. No error, no retry, no log line. That
is the point: the guarantee is the database constraint, and the application code
is not a second line of defence. If someone tells you their billing engine is
idempotent, this is the experiment to ask for.

## 4. Money invariants after recovery

Counting invoices proves nothing was double-billed. It says nothing about
whether the crash left some *other* invariant broken — an orphaned line item, a
ledger entry without its counterpart, a subscription whose period bounds no
longer agree with its invoices.

`velox-doctor` sweeps **28 money invariants** as read-only SQL. After every
scenario above:

```
doctor: 28 checks, 0 violations, 0 errors, ~23ms
```

Two things worth stating about that number rather than leaving implied:

- The sweep runs under an **admin** connection. These checks carry no tenant
  predicate, so under a row-level-security-scoped role they return zero rows and
  report a clean bill of health without having looked at anything.
- One of the 28 checks is new, added by this work.
  `cycle_invoice_unique_per_subscription_period` looks for duplicate cycle
  invoices directly. Before it existed, the doctor ran all 27 checks against a
  database holding 129 duplicate invoices and **reported zero violations** — it
  was blind to the exact failure the index prevents. The index and the check
  fail independently: an index can be missing after a restore, dropped by a
  migration, left `INVALID` by a failed `CREATE INDEX CONCURRENTLY` (enforcing
  nothing while still appearing in `pg_indexes`), or quietly narrowed by a
  predicate change — migration 0101 narrowed this exact predicate once already.

## 5. How fast a dead leader is replaced

Singleton work is gated by a Postgres advisory lock. Two failure modes, measured
separately, because they behave nothing alike:

| Failure | Time until another replica can take over |
|---|---:|
| Process dies, host survives (panic, `SIGKILL`, OOM) | **~1 ms** |
| Host disappears without closing the socket (partition, power loss, VM terminate) | **90 s** |

The second number was **7,875 s (2 h 11 m)** before this work — the Postgres
default keepalive train — during which every replica skips its tick and billing
stops with no error and no log line at `Info`. See
`docs/ops/postgres-requirements.md`.

---

## What this does not show

Stated plainly, because a benchmark that hides its limits is the thing this
document is arguing against.

- **This is a correctness result, not a scale result.** 40 subscriptions on one
  laptop. It says nothing about throughput; that is a separate track and it is
  not finished.
- **Single-node Postgres.** No replication, no failover of the database itself.
  A leader dying is not the same event as Postgres dying, and only the first is
  measured here.
- **Simulated clock.** Both leaders run at one fixed instant so "which periods
  are due" is deterministic. Wall-clock behaviour across a real month boundary
  is covered by other tests, not by this one.
- **No payment provider in the loop.** The charger is a sentinel that fails
  loudly if reached, so this measures invoice generation, not settlement.
- **The 90 s figure is a configuration bound, not a demonstrated partition.** We
  assert the keepalive settings on the lock connection and compute the window;
  we do not sever a real network link. The 1 ms figure *is* directly measured.

## Reproducing

```bash
docker compose up -d postgres
go test -p 1 ./internal/billing/ -short=false -run TestExactlyOnce -v
go test -p 1 ./internal/platform/postgres/ -short=false -run TestLeaderFailover -v
```

To reproduce the negative control, drop `idx_invoices_billing_idempotency` from
the test database and re-run the first command. It should fail, loudly, with the
duplicate count. If it passes, the experiment did not do what it claims.
