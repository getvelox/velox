> **Companion:** [sustained-throughput.md](sustained-throughput.md) — what
> the ingest API sustains on named AWS hardware under a gated, reconciled
> protocol (12,000 ev/s at p99 22 ms on `db.m7g.4xlarge`, batch 10; 15,000 at
> batch 100), what stops it, and the two product findings it produced.

# Correctness under failure

What happens to your invoices when the billing process dies mid-run.

This is the one benchmark we think is worth publishing first, because it is the
question a billing system can least afford to answer with a shrug — and the one
most likely to be answered by a slide rather than a test. Everything below is
reproducible from a clean checkout with `docker compose up -d postgres` and two
`go test` commands, printed at the bottom.

**Date:** 2026-08-15 · **Commit:** see `git log` for `test/track-a-exactly-once`
· **Hardware:** developer laptop, Postgres 16 in Docker, single node.

---

## The guarantee

Exactly-once billing in Velox is **not** application logic. It is a partial
unique index (migration 0101) — a uniqueness rule the database enforces only on
rows matching the index's `WHERE` clause:

```sql
CREATE UNIQUE INDEX idx_invoices_billing_idempotency
  ON invoices (tenant_id, subscription_id, billing_period_start, billing_period_end)
  WHERE status <> 'voided' AND source_plan_changed_at IS NULL;
```

A second live cycle invoice for a period that already has one cannot be
committed — the `WHERE` clause scopes the rule to non-voided, regular
billing-cycle invoices. Not "is unlikely to be" — cannot be. That distinction
is the whole result, and the negative control below — the same run with the
index removed, which must produce duplicates — is what makes it a measurement
rather than a claim.

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

A crash produces one interleaving. Running four leaders (four instances of the
billing engine, raced as goroutines in one test process) concurrently against the same due set produces many, including
both transactions inside the same `(subscription, period)` window
simultaneously — a state sequential takeover cannot construct on purpose.

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

`velox-doctor` (Velox's invariant-checking CLI) sweeps **28 money invariants**
as read-only SQL. After every scenario above:

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

Singleton work — jobs only one replica may run at a time — is gated by a
Postgres advisory lock (an application-defined lock the server holds for the
session). Two failure modes, measured separately, because they behave nothing
alike:

| Failure | Time until another replica can take over |
|---|---:|
| Process dies, host survives (panic, `SIGKILL`, OOM) | **under 1 ms** |
| Host disappears without closing the socket (partition, power loss, VM terminate) | **95 s, measured** |

The first figure is a bound, not a stopwatch reading. The test starts timing
after `wait()` reports the process reaped, and the *first* lock attempt already
succeeds — so 1 ms is the cost of one round-trip check, and the lock was free
before we could look. Identical across 6 consecutive runs, which is what a
measurement pinned to its own granularity looks like.

### The partition case, actually severed

The 90 s figure used to be arithmetic — we set the TCP keepalives (the kernel's
periodic probes that detect a silently dead peer) and computed `60 + 10×3`. It
was the only number in this document that had never been
observed, and the one most likely to be wrong, because keepalives depend on the
network path honouring them.

So we severed a real link. A holder process in its own container takes the lock
against Postgres over a private Docker network; `docker network disconnect`
then removes its interface, which stops packets **without sending a FIN** —
what a partition, a power cut, or a security-group change looks like from the
server's side. The holder process stays alive throughout, so nothing is being
measured except Postgres deciding the peer is gone.

| Keepalives (idle / interval / count) | Predicted window | Observed |
|---|---:|---:|
| 10 / 5 / 2 | 10–20 s | **13 s**, **21 s** — released |
| **60 / 10 / 3 (production)** | **60–90 s** | **95 s** — released |
| 7200 / 75 / 9 (Postgres default) | 7200–7875 s | **still held at 248 s**, when we stopped waiting |

Predicted is a *window*, not a point: the first probe fires up to `idle` seconds
after the last activity, so where the sever lands inside that interval moves the
result. Two runs at 10/5/2 gave 13 s and 21 s, which is the granularity being
honest about itself rather than an inconsistency.

The production setting recovers in 95 s against a 60–90 s predicted window —
just outside it, by about the 2 s polling interval plus scheduling slop. The default —
what this code did before the keepalive fix — was still holding the lock 2.6×
longer than that when the experiment ended, and by arithmetic would hold it for
over two hours.

**One trap, because it cost us a wrong answer first.** The holder session must
be **idle**, not running a query. Our first attempt held the lock with
`pg_sleep(3600)`, and the lock survived 185 s of partition with aggressive
10/5/2 keepalives — which looked like the mechanism failing. It was not: a
backend executing a query never touches its socket, so it cannot notice the peer
is gone no matter what the kernel concludes. `pg_stat_activity` said
`state=active` and that was the tell. The real scheduler holds this lock on an
idle connection while work happens elsewhere, so the test must too. Reproduce
with a session that is `state=idle` or you will measure the wrong thing — the
packaged drill asserts this and refuses to run otherwise:

```bash
./scripts/partition-drill.sh 60 10 3   # production setting
./scripts/partition-drill.sh           # no SET: the pre-fix default
```

The second number — takeover after a silent host loss — was **7,875 s (2 h 11 m)**
before this work: the Postgres default keepalive train. During that window every
replica skips its tick, and billing stops with no error and no log line at
`Info`. See
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
- **Both failover figures are now measured, not computed** — the partition case
  by severing a real network link (see above). What is still *not* covered:
  failover between physical machines, and Postgres itself dying. A leader dying
  is not the same event as its database dying, and only the first is measured
  here.

## Reproducing

```bash
docker compose up -d postgres
go test -p 1 ./internal/billing/ -short=false -run TestExactlyOnce -v
go test -p 1 ./internal/platform/postgres/ -short=false -run TestLeaderFailover -v
```

To reproduce the negative control, drop `idx_invoices_billing_idempotency` from
the test database and re-run the first command. It should fail, loudly, with the
duplicate count. If it passes, the experiment did not do what it claims.
