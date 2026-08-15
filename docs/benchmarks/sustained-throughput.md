# Sustained throughput

What one node holds, on named hardware, at a stated latency — and why the same
code measured on a laptop looked **9× faster** than it really is.

**Measured 2026-08-15 on AWS**, `ap-south-1a`, all components in one
availability zone:

| Role | Instance | Notes |
|---|---|---|
| App — **the node under measurement** | `c7g.large` (2 vCPU) | our shipped container image, bridge networking |
| Load generator | `c7g.xlarge` (4 vCPU) | deliberately larger, so it can never be the bottleneck |
| Database | RDS `db.m7g.large`, PostgreSQL 16.14, Single-AZ, 100 GB gp3 | |

Cost of the rig: **~$0.41/hr**. Everything below is reproducible with the
scripts in `scripts/bench-rig/`.

---

## The headline

| Path | Sustained rate | p50 | p99 |
|---|---:|---:|---:|
| Single event per request | **200/s** (small table) | 34.7 ms | 118.6 ms |
| Single event, once indexes grew | **fails at 200/s** — 190–193 achieved | 283–870 ms | 1.3–1.9 s |
| **Batched, 10 events per request** | **1,000/s** | 67 ms/call | 269 ms/call |

**A 10-minute soak at 1,000 events/sec delivered 600,030 events with 0 errors**,
holding 100.0% of the offered rate, p99 268.9 ms.

**The operational conclusion: batching is not an optimisation here, it is a
requirement.** The single-event path stopped holding 200/s once the table's
indexes had grown — confirmed across three consecutive runs after autovacuum had
settled — while the batched path was unaffected across three runs before and
after.

## Why: the database is a network hop away

The app node was **54% idle** at 400 events/sec and the load generator **98%
idle**. Nothing was CPU-bound. The limit is round trips:

- **1.04 ms** per round trip to RDS *in the same availability zone*
- **~12 round trips per ingested event** — customer resolve, meter resolve,
  insert, each its own transaction, plus three `set_config` calls per
  transaction for row-level security
- → a **12.2 ms floor per event**, measured at a rate low enough to have no
  queueing at all

Batching amortises those round trips and nothing else does. At batch=10 the
per-event cost fell to **4.2 ms**, a 2.9× improvement.

The write amplification compounds it: `usage_events` carries five indexes
including a GIN over the `properties` JSONB, and at 5.6M rows those indexes were
**1,472 MB against a 1,193 MB heap** — larger than the table they index. Every
single-event insert pays a commit plus maintenance on all five.

## Concurrency has to be sized, not maximised

Raising the load generator from 6 to 24 workers made everything **worse**: p50
went from 12 ms to 100 ms at a *lower* offered rate. The app holds a
20-connection pool on 2 vCPU, so 24 in-flight requests oversubscribe both.

Concurrency should be `rate × latency`, not "high, to be safe". At 200/s and
12 ms that is ~3 concurrent requests; six workers was already generous.

---

## What a laptop can and cannot tell you

This benchmark existed on a laptop first. Publishing both is deliberate,
because the gap is the most useful thing we learned.

| Laptop finding | Held up on real hardware? |
|---|---|
| COMMIT is 66% of in-database time, INSERT 28% | **Yes.** A property of the code, not the machine |
| Batch curve 2,714 → 8,245 → 10,821 → 12,095 ev/s, flattening after ~50 | **Shape yes.** Batching gave 5× on AWS too |
| HTTP costs ~2× in-process p50 | **Roughly yes** |
| **1,800 ev/s at p99 ≤ 50 ms** | **No — about 9× optimistic** |

The reason is structural, not sloppiness. **A laptop has no network between the
application and its database.** A round trip to localhost is ~0.05 ms, so the
same ~12 round trips per event cost about 0.6 ms — invisible, buried in noise.
On real infrastructure they cost 12.5 ms and become the floor under everything.

A laptop cannot produce the cost that dominates every real deployment, so it
will always overstate. That is not a caveat you can write your way out of; it is
a reason not to publish laptop absolutes at all.

**Use a laptop for ratios, a real deployment for absolutes.** The same laptop
run that produced a 9×-optimistic throughput number also produced the
66%-COMMIT profile that correctly *predicted* the AWS result — if commits
dominate, amortising commits is the lever, and batching is what did it.

---

## What this does not show

- **The batch sweep was not run.** We measured batch=10 because our own guidance
  recommends it for latency. The laptop curve suggests batch=50 is ~4× batch=10,
  which would put this rig near 4,000/s, but **that is extrapolation and is not
  measured.** Anyone comparing against another engine's batched figure should
  wait for this.
- **2 vCPU is a small app node**, chosen so the measurement is clearly about the
  software rather than the hardware. A larger node has not been measured.
- **The soak was 10 minutes, not 60.** Long enough for autovacuum and
  checkpoints to appear; not long enough for multi-hour effects.
- **The history seeding did not land where intended.** `usage_events.livemode`
  has a column default of `true` *and* a `set_livemode` trigger, which overrode
  the explicit `false` in the seeder — so the 5M seeded rows sit in the *other*
  livemode partition. The degradation finding still stands, because only the
  idempotency index includes `livemode` and the other btrees plus the GIN are
  shared and did grow. But the correct caption is "a 5.6M-row table with 1.47 GB
  of shared indexes", **not** "a tenant with 5M of its own events".
- **No payment provider, no nginx.** `deploy/compose` puts nginx in front of the
  app; this measured the app container directly, which is one hop fewer.
- **Single-node Postgres**, no replica, no pooler.

## Reproducing

```bash
cd scripts/bench-rig
./teardown.sh --check          # confirm the account is clean first
./provision.sh --yes           # ~$0.41/hr from this moment
( nohup ./watchdog.sh 240 & )  # force teardown after 4h if the driver dies
# ... run velox-bench from the loadgen against the app's private IP ...
./teardown.sh                  # and verify it prints CLEAN
```

Two traps that cost time and will cost yours:

**Amazon Linux ships Go with `GOTOOLCHAIN=local`**, so `go build` refuses when
`go.mod` requires a newer patch release, and `GOSUMDB=off` then blocks
downloading the right one. Build with
`GOTOOLCHAIN=auto GOSUMDB=sum.golang.org`. The container image is unaffected
because the Dockerfile pins its own toolchain.

**Create the app role *and* its default privileges before the first migration.**
`CREATE ROLE velox_app` alone is not enough — most tables get their grant from
`ALTER DEFAULT PRIVILEGES`, so skipping it produces a cluster that migrates
cleanly and then returns 500 on ingest.
