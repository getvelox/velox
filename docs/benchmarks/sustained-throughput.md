# Sustained throughput

Measured on AWS, on named hardware, with the configuration and the cost stated —
and with the number our own laptop produced shown alongside, because it was
**9× optimistic** and that gap is the most useful thing in this document.

**Date:** 2026-08-15/16 · **Region:** `ap-south-1`, everything in one
availability zone · **Reproduce:** `scripts/bench-rig/`

---

## The headline

Two kinds of number, and they are not the same kind:

**Rate-controlled (open loop — the only rows whose latency is an SLO)**

| Configuration | Held | Notes |
|---|---:|---|
| Single event per request | **200/s** | p99 119 ms; **did not hold** once indexes grew (below); the wrong way to use the API at volume |
| Batched (10/request) | **1,000/s** | p99 269 ms; 10-min soak, 600,030 events, **0 errors** |

**Ceiling (closed loop — 16 workers sending as fast as responses return; a maximum, never a service level)**

| Configuration | Reached | Notes |
|---|---:|---|
| Batched (500/request), `db.m7g.2xlarge` | **10,203/s** | 90 s, 925,000 events, **0 errors**; p50 ~800 ms at that point; a sister run reached 10,817 |

> **What every row above shares, stated once:** each is a **single run** (n=1);
> the event count is the load generator's own counter, not a database count; the
> generator has since been deleted for a pacing bug (direction: pessimistic); the
> fixtures were **one tenant, one customer, one meter**; and the run used the
> **test-mode** ingest path, which does one extra per-event lookup that the
> production (live-mode) path skips — so production does strictly less work per
> event than was measured here. The protocol that replaces these numbers
> (`measure.sh`, below) runs live mode by default, fans out over 200 customers,
> repeats each configuration, publishes latency medians with spread, and refuses
> to report a run whose rows do not match what the client claimed.

**Cost at 10k events/sec: ~$1.16/hr** (`c7g.2xlarge` app + `db.m7g.2xlarge`),
which is **$0.032 per million events** — app and database **compute only**,
on-demand `ap-south-1`, at the closed-loop ceiling (100% utilisation); storage,
the load generator, backups and Multi-AZ are excluded. At the rate-controlled
1,000/s on the same hardware the same arithmetic gives ~10x that per million. For comparison, Stripe Billing's 0.5%
would cost the same only if a million metered events represented $6.29 of
revenue.

## Batching is a requirement, not an optimisation

The single-event path **stopped holding 200/s once the table's indexes had
grown** — confirmed across three consecutive runs after autovacuum settled. The
batched path was unaffected across three runs before and after.

| batch | ev/s | p50 | p99 |
|---:|---:|---:|---:|
| 1 | 628 | 24 ms | 52 ms |
| 10 | 2,955 | 52 ms | 92 ms |
| 50 | 4,735 | 167 ms | 224 ms |
| 200 | 6,116 | 511 ms | — |
| 500 | 7,102 | 1.10 s | — |

*(`c7g.2xlarge` app, `db.m7g.xlarge`, 5M-row table, closed-loop maximum.)*

## Why: the database is a network hop away

Nothing was CPU-bound. At 400 ev/s the app node was **54% idle** and the load
generator **98% idle**. The limit is round trips:

- **1.04 ms** per round trip to RDS *in the same availability zone*
- **~26 database statements per single-event request on the production
  path, ~32 on the test-mode path** the published runs used — auth, customer
  resolve, meter resolve and the insert each open a transaction of their own
  (BEGIN, three `set_config` calls for row-level security, the query, COMMIT),
  plus a `last_used_at` touch. Measured with `log_statement=all` against an
  idle control, not counted from the code. An earlier version of this page said
  "~12 round trips"; that figure did not describe the code, and its
  arithmetic (`12 × 1.04 ms`) only coincidentally landed near the floor below.
- → a **12.2 ms floor per event**, **measured** at a rate low enough to have no
  queueing at all. Treat that as the measurement; the statement count above is
  the explanation of its order of magnitude, not a derivation of it.

Batching amortises those round trips and nothing else does: at batch=10 the
per-event cost fell to **4.2 ms**, and the statement count falls to **~3.4 per
event** (~34 per batch of 10, measured the same way).

Write amplification compounds it. `usage_events` carries five indexes including
a GIN over the `properties` JSONB; at 5.6M rows those indexes were **1,472 MB
against a 1,193 MB heap** — larger than the table they index.

## Reaching 10k is a database-sizing question

At 7,102 ev/s on `db.m7g.xlarge` (4 vCPU) the picture was unambiguous: **RDS at
88% CPU while the app node still had 75% headroom.** Doubling *only* the
database to `db.m7g.2xlarge` (8 vCPU) moved batch=200 from 6,116 → **9,689** and
batch=500 from 7,102 → **10,817**.

At the 10,203/s sustained figure **neither tier was saturated** — app 74% idle,
RDS 50–71% CPU. So that is not a ceiling either; it is where 16 workers at
batch=500 happened to land. The real ceiling on that hardware is higher and was
not chased.

The prediction written down before measuring was 8,000–15,000 on the larger rig.
The small rig came in at 7,102 against a predicted 3,000–6,000 — **the
prediction was wrong on the low side**, recorded here rather than quietly
updated.

## Concurrency must be sized, not maximised

Raising the load generator from 6 to 24 workers made everything **worse**: p50
went from 12 ms to 100 ms at a *lower* offered rate. The app holds a
20-connection pool; 24 in-flight requests oversubscribe it. Size concurrency to
`rate × latency`, not "high, to be safe".

---

## What a laptop can and cannot tell you

This benchmark existed on a laptop first. Both are published because the gap is
the lesson.

| Laptop finding | Held on real hardware? |
|---|---|
| COMMIT is 66% of in-database time, INSERT 28% | **Not re-measured on RDS.** Expected to hold — it is a property of commits-per-event, which batching changes and the network does not — but the statement-level split was only ever taken on the laptop |
| Batch curve flattens after ~50 | **Shape yes** |
| HTTP costs ~2× in-process p50 | **Roughly** |
| **1,800 ev/s at p99 ≤ 50 ms** | **No — about 9× optimistic** |

The reason is structural, not sloppiness. **A laptop has no network between the
application and its database.** A round trip to localhost is ~0.05 ms, so the
same ~12 round trips cost ~0.6 ms — invisible. On real infrastructure they cost
12.5 ms and become the floor under everything.

**Use a laptop for ratios, a real deployment for absolutes.** The same laptop
run that overstated throughput 9× also produced the 66%-COMMIT profile that
correctly *predicted* the AWS result: if commits dominate, amortising commits is
the lever, and batching is what did it.

---

## What this does not show

- **Closed-loop for the batch curve.** Workers send as fast as responses come
  back, so the system is saturated by construction and p50 at batch=500 is
  795 ms. That is the honest number for "what is the ceiling", and it is **not**
  the same kind of number as the rate-controlled 1,000/s at p99 269 ms. Both are
  published; neither substitutes for the other.
- **Our load generator had a burst bug.** The uniform pacing had every worker
  computing identical due times from slot 0, firing as a synchronised burst
  rather than a staggered stream. The mean rate was correct, so it went
  unnoticed; the self-inflicted queueing was counted as Velox's latency. Fixing
  it moved the measured median from 24.8 ms to 6.1 ms — but **that fix was a
  scratch experiment and was never committed**, so every figure on this page
  was produced by the bursting build. **The rate-controlled latencies above are
  therefore pessimistic**, not optimistic — wrong in the safe direction, but
  wrong. The hand-rolled generator has since been deleted entirely in favour of
  k6's `constant-arrival-rate` executor, which cannot have that bug: there is
  one central schedule rather than a per-worker one. k6 has not yet been run on
  this rig, so re-measuring is the way to replace these numbers rather than
  merely caveat them.
- **No nginx.** `deploy/compose` puts nginx in front of the app; this measured
  the app container directly, one hop fewer.
- **Single-node Postgres**, no replica, no pooler, Single-AZ.
- **10-minute soak, not 60.** Long enough for autovacuum and checkpoints to
  appear; not long enough for multi-hour effects.

## Reproducing

```bash
cd scripts/bench-rig

./calibrate/calibrate.sh       # prove the MEASURING TOOL is honest first;
                               # needs only go + k6, costs nothing, and must
                               # print CALIBRATED before any number means anything

./teardown.sh --check          # confirm the account is clean first.
                               # exit 0 = clean, 1 = not clean, 2 = COULD NOT LOOK
                               # (a failed query is never "clean")

./provision.sh --yes           # ~$0.41/hr from this moment; override
                               # APP_TYPE / GEN_TYPE / DB_CLASS for the larger rig
( nohup ./watchdog.sh 240 & )  # force teardown after 4h if the driver dies

./bringup.sh                   # hardware -> a RUNNING, SEEDED, VERIFIED velox
                               # (LIVE-mode fixtures, 200 customers by default;
                               # BENCH_LIVEMODE=false / BENCH_CUSTOMERS=n to change)

DATABASE_URL=... ./seed-history.sh 20000000
                               # OPTIONAL but recommended: a table with history,
                               # spread across the seeded customers. Without it
                               # the runs measure an EMPTY table — the optimistic
                               # case. measure.sh records the table size before
                               # every run either way, so a reader can tell.

DATABASE_URL=... VELOX_EVS=<measured> ./db-ceiling.sh
                               # the DENOMINATOR: pgbench on the same database
                               # and row shape. Leg A = raw commit floor, leg B =
                               # Velox's per-tx RLS protocol; prints Velox's
                               # measured ev/s as a fraction of each. Run AFTER
                               # measure.sh, on the same table state.

./measure.sh                   # the PROTOCOL: warmup (discarded), N repeats,
                               # latency medians with spread over PASSING runs,
                               # every run gated on: k6 exit 0, no drops, no
                               # failures, claimed > 0, claimed == rows written.

./teardown.sh                  # and verify it prints CLEAN
```

`db-ceiling.sh` is the control every Velox throughput figure had been missing:
without the database's own ceiling for this row shape, "10,203 events/sec" has
no denominator. On a laptop the raw commit floor was ~5.9k rows/s at batch 1
and ~23k at batch 10; the three per-transaction `set_config` calls that
row-level security costs took **44–49%** of that floor — a share that grows on
RDS, where each round trip is ~1 ms rather than ~50 µs; and Velox's single-event
HTTP path landed at ~12% of the DB-with-RLS figure, which is what the ~26
statements of auth, resolve and insert cost. Both legs verify their row count
in the intended livemode partition, and the script refuses to run against
fixtures seeded in the other mode.

`measure.sh` exits non-zero if **any repeat of any configuration** fails its
gate, and a configuration is only reported as held at a rate when every repeat
passed. `bringup.sh` exists because provisioning hardware is not the same as
having something to measure. It refuses to continue on any of the failures that used to
be silent: a cross-AZ RDS, a dirty schema, an app role that cannot read the
tables the migration just created, a server that fell back to the **admin** pool
(which would measure a configuration with no RLS on the request path, which
nobody self-hosts), or a `201` that wrote no row.

## Measuring responsiveness, not just throughput

A throughput number with no concurrent latency budget on the read path is the
half of the benchmark that flatters the vendor. Nobody experiences "10,203
events/sec"; a finance team experiences whether the invoice page loads while
ingest is at peak.

```bash
k6 run -e BASE=http://<app-private-ip>:8080 -e API_KEY=... -e CUSTOMER_ID=... \
       -e RATE=1000 -e BATCH=10 -e DURATION=10m \
       -e PROBE_RATE=5 -e PROBE_P99_MS=500 ingest.js
```

`PROBE_RATE` adds a second, concurrent scenario that reads what a human waits
on — `usage-summary` for a random customer (which aggregates that customer's
rows in the very table being written), the invoice list, the customer list —
and `PROBE_P99_MS` (default 500) is a **threshold**, so a run whose read path
degrades under write load **exits non-zero** instead of publishing a throughput
figure beside an unusable product. Ingest latency is reported from the ingest
scenario only; the probe's samples never pool into it. Under `measure.sh` a
`DEGRADED` probe fails the run. Known limits: the invoice list is empty on a
bench tenant, and the three endpoints share one verdict.

## Traps that cost time and will cost yours



**Amazon Linux ships Go with `GOTOOLCHAIN=local`**, so `go build` refuses when
`go.mod` requires a newer patch release, `GOSUMDB=off` then blocks downloading
the right one, and cloud-init runs without `HOME` so the module cache cannot be
found. Build with `HOME=/root GOPATH=/root/go GOTOOLCHAIN=auto
GOSUMDB=sum.golang.org`. The container image is unaffected — the Dockerfile pins
its own toolchain.

**Create the app role *and* its default privileges before the first migration.**
`CREATE ROLE velox_app` alone is not enough: most tables get their grant from
`ALTER DEFAULT PRIVILEGES`, so skipping it produces a cluster that migrates
cleanly and then returns 500 on ingest across 11 tables.

**`usage_events.livemode` is set by a trigger, not by your INSERT.** The
`set_livemode` trigger overwrites the column from the `app.livemode` session
GUC. Seeding history without `SET app.livemode = 'off'` puts every row in the
*other* partition — the seeder does this and then verifies the split, because
an earlier run silently benchmarked a table it did not think it was measuring.
