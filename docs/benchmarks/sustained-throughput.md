# Sustained throughput

Measured on AWS, on named hardware, with the configuration and the cost stated —
and with the number our own laptop produced shown alongside, because it was
**9× optimistic** and that gap is the most useful thing in this document.

**Date:** 2026-08-15/16 · **Region:** `ap-south-1`, everything in one
availability zone · **Reproduce:** `scripts/bench-rig/`

---

## The headline

| Configuration | Sustained | Notes |
|---|---:|---|
| Single event per request | **200/s** | p99 119 ms; the wrong way to use the API at volume |
| Batched (10/request) | **1,000/s** | p99 269 ms; 10-min soak, 600,030 events, **0 errors** |
| Batched (500/request), larger DB | **10,203/s** | 90 s, 925,000 events, **0 errors** |

**Cost at 10k events/sec: ~$1.16/hr** (`c7g.2xlarge` app + `db.m7g.2xlarge`),
which is **$0.032 per million events**. For comparison, Stripe Billing's 0.5%
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
- **~12 round trips per ingested event** — customer resolve, meter resolve,
  insert, each its own transaction, plus three `set_config` calls per
  transaction for row-level security
- → a **12.2 ms floor per event**, measured at a rate low enough to have no
  queueing at all

Batching amortises those round trips and nothing else does: at batch=10 the
per-event cost fell to **4.2 ms**.

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
| COMMIT is 66% of in-database time, INSERT 28% | **Yes** — a property of the code |
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
  it moved the measured median from 24.8 ms to 6.1 ms. **The rate-controlled
  latencies above are therefore pessimistic**, not optimistic — wrong in the
  safe direction, but wrong.
- **No nginx.** `deploy/compose` puts nginx in front of the app; this measured
  the app container directly, one hop fewer.
- **Single-node Postgres**, no replica, no pooler, Single-AZ.
- **10-minute soak, not 60.** Long enough for autovacuum and checkpoints to
  appear; not long enough for multi-hour effects.

## Reproducing

```bash
cd scripts/bench-rig
./teardown.sh --check          # confirm the account is clean first
./provision.sh --yes           # ~$0.41/hr from this moment; override
                               # APP_TYPE / GEN_TYPE / DB_CLASS for the larger rig
( nohup ./watchdog.sh 240 & )  # force teardown after 4h if the driver dies
./teardown.sh                  # and verify it prints CLEAN
```

Three traps that cost time and will cost yours:

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
