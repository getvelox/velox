# Sustained throughput

Measured on AWS, on named hardware, with the configuration and the cost stated,
under a protocol that refuses to publish a run it cannot reconcile — and with
the two product bottlenecks it found stated as plainly as the numbers.

**Date:** 2026-08-16 (supersedes the 2026-08-15 figures, which are kept at the
end for the record) · **Region:** `ap-south-1`, everything in one availability
zone · **Reproduce:** `scripts/bench-rig/` — `./run.sh`, or step by step below.

---

## The headline (2026-08-16, measured under the protocol below)

**Rig:** `c7g.2xlarge` app (8 vCPU Graviton, Velox in its own container) +
`c7g.xlarge` load generator + `db.m7g.2xlarge` RDS PostgreSQL 16, all in
`ap-south-1a`. **~$1.26/hr.** Live-mode ingest path, 200 customers, one meter,
each request a random customer. Table seeded to **22M rows** before the first
measurement and **47M** by the last; **10 GB+ of indexes**. Every figure below
is **open-loop** (requests offered on a fixed schedule; drops reported), every
repeat is gated (k6 thresholds held, 0 dropped, 0 failed, rows written ==
events claimed, Σ quantity == Σ gained, ≥1000 samples, no drift), and every
event was reconciled against the database.

### Rate-controlled — the numbers that are a service level

| offered rate | batch | repeats | duration each | p50 | p99 | reads under load |
|---:|---:|---|---|---:|---:|---|
| **200 ev/s** | 1 | 4/5 passed¹ | 10 min | **4.1 ms** (4.1–4.1) | **4.9 ms** (4.9–5.0) | responsive |
| **1,000 ev/s** | 10 | **5/5 passed** | 10 min | **7.2 ms** (7.1–7.2) | **8.2 ms** (8.1–8.3) | responsive |
| 5,000 ev/s | 100 | **1/3, then a cliff** | 10 min | 35–39 ms | 51 ms → 116 ms → 9.9 s as the table passed ~60M rows | see § reads |
| 10,000 ev/s | 100 | **0/2 — a knee** | 10 min | 38–40 ms | 152 ms then 3.4 s | see § reads |

¹ Repeat 1 failed its gate on a single 5-second stall 20 s in, coincident with
autovacuum working through the 22M-row bulk seed finished minutes earlier
(RDS write IOPS 2× baseline for the first five minutes); the other four were
clean and the protocol now settles after seeding. Reported, not hidden.

**5,000 ev/s is where this instance's memory runs out.** Repeat 1 held flat
(p50 35 ms, p99 51 ms, first third 52 → last third 50). Repeat 2, thirty
minutes and 6M rows later at **62M rows / 14 GB heap + 18 GB indexes on a 32 GB
instance**, rose from 50 ms to 308 ms in its last third — and the evidence says
exactly why: RDS CPU flat at 61–63 %, write IOPS flat, but **read IOPS climbing
108 → 2,486/s** through the run (the gp3 volume's baseline is 3,000), read
latency 0.6 → 2.6 ms and disk queue depth 0.5 → 10.7 in the final minute. The
index working set had fallen out of cache and every insert was fetching index
pages from EBS. Repeat 3, at 65M rows, delivered 4,035 of the 5,000 offered with a p99 of 9.9 s and 5,792 drops; the series was stopped there.

That is a **capacity cliff, not a Velox defect**, and it is the single most
useful number here for a self-hoster: on `db.m7g.2xlarge` + 100 GB gp3, steady
inserts turn read-IOPS-bound at roughly **60M rows / 30 GB of table + index**.
The levers are the usual ones — RAM (`db.m7g.4xlarge` = 64 GB), provisioned
IOPS, fewer or smaller indexes (five on `usage_events`, one GIN), or
time-partitioning so the hot working set stays small.

**10,000 ev/s held for 90 seconds at p99 66 ms** (rate ladder, below) — and
did not hold for ten minutes. Repeat 1 delivered the rate with the tail climbing
through the run (first third p99 70 ms, last third 195 ms; 4 drops), repeat 2
delivered 9,741 with a p99 of 3.4 s and 1,556 drops. RDS CPU went 68 → 73 %
during the run and the table grew by 6M rows per repeat into 15 GB of indexes.
That is a knee on `db.m7g.2xlarge`, not a plateau, and it is reported as one;
this is what a 10-minute repeat is for.

The **1,000 ev/s** row is the direct successor of the figure published before
this run — same configuration, previously **p99 269 ms** from a single run on
a bursting generator. Under the protocol, five 10-minute repeats agree to
within 0.2 ms: **p99 8.2 ms**. The generator was inflating the tail ~30×.

### Ceiling — closed loop, a maximum, never a service level

| | reached | p50 | p99 |
|---|---:|---:|---:|
| 16 workers, batch 500, 90 s | **25,424 ev/s** (2,293,500 events, all reconciled) | 296 ms | 520 ms |

The previously published closed-loop figure for this configuration was
10,203/s: test-mode path, one customer, bursting generator. Live mode, 200
customers and an honest generator: **2.5×**.

### The rate ladder (2 × 90 s per rung; the "held" column is the ingest gate)

| batch | 2,000 | 4,000 | 5,000 | 6,000 | 8,000 | 10,000 | 15,000 |
|---|---|---|---|---|---|---|---|
| **10** | held (p99 10.6 ms)² | held (p99 31–36 ms) | — | **not held** (delivered ~5.5k, p50 1.5 s) | not held (~5.8k) | — | — |
| **100** | — | — | held (p99 42–44 ms) | — | — | **held (p99 64–66 ms)** | held (p99 262–315 ms, drift ×1.4) |

² one of two repeats hit the same post-bulk-write stall as ¹; the other passed.

## What the ladder found: the wall is a hot row, not the hardware

At batch 10 the open-loop knee sits between 4,000 and 6,000 ev/s — about
**570 requests per second**, at any batch size. That is not the machine:

| candidate | test | result |
|---|---|---|
| RDS CPU / app CPU | CloudWatch + on-box `vmstat` during the 6k runs | RDS 39–44 %, app node 88 % idle |
| the 20-connection pool | control-vs-treatment: `DB_MAX_OPEN_CONNS=60` | **worse** (p50 2–3 s, one repeat drifting ×3.9) |
| commit / fsync rate | `pgbench` one-row transactions on the same RDS | **6,840 commits/s** at 16 clients, 14,284 at 64 — 10× the plateau |
| what the backends were actually doing | `pg_stat_activity` sampled during a 6k run | **50–55 of 61 backends waiting on `UPDATE api_keys SET last_used_at = $1 WHERE id = $2`** — `Lock:transactionid`, `Lock:tuple`, `LWLock:LockManager` |

Every request touches `last_used_at` on the API key it authenticated with.
One high-volume client means one row, and a row lock hands off at roughly one
RTT (~1.75 ms) — ~570 requests/s, on any hardware. More connections only add
waiters. Batch 100 and 500 stay under 150 requests/s, which is why 10,000 and
15,000 ev/s held while 6,000 at batch 10 did not. This is a product finding,
not a rig finding, and the fix is small (debounce the touch); filed as
[#818](https://github.com/getvelox/velox/issues/818) with this evidence. **Until then: batch, and use more than one API key per
high-volume producer.**

## What the read probe found: the per-customer usage summary is a linear scan

While ingest ran, a second k6 scenario read what a finance user reads —
`usage-summary` for a random customer over 30 days, the invoice list, the
customer list — 5 requests/s, p99 budget 500 ms. The two list endpoints never
left **2–9 ms** at any write rate. `usage-summary` did not care about the
write rate either; it cared about **how many events the customer had**:

| rows in table | ≈ events per customer | usage-summary p50 | p99 |
|---:|---:|---:|---:|
| 22M | 110k | 332 ms | 342 ms |
| 25M | 125k | 354 ms | 362 ms |
| 39M | 195k | 523 ms | 554 ms |

About 2.7 µs per event, linear: a `COUNT + SUM GROUP BY meter` over the
customer's rows with no rollup. Above ~180k events per customer per month it
misses a 500 ms budget regardless of load. Also a product finding, with a
number on it, and the classic fix (a per-customer daily rollup) is well
understood — filed as [#819](https://github.com/getvelox/velox/issues/819). The read gate is reported separately from the ingest gate for
exactly this reason: at 10,000 ev/s ingest held and the summary missed its
budget, and both are true.

## Why: round trips, and where the time goes

The RDS round trip in the same availability zone is ~1 ms. On the production
path a single-event request costs **~26 database statements** (auth, customer
resolve, meter resolve and the insert each open a transaction of their own —
BEGIN, three `set_config` calls for row-level security, the query, COMMIT —
plus the `last_used_at` touch), measured with `log_statement=all` against an
idle control. Batching amortises everything except the per-request part:
**~3.4 statements per event at batch 10**, and the request rate — not the
event rate — is what the hot row above caps.

`pgbench` on the same database and row shape (`db-ceiling.sh`) puts the
database's own one-row commit floor at 6,840/s (16 clients) and 14,284/s (64),
and the same with Velox's per-transaction RLS protocol at 5,961 and 11,546.
So the three `set_config` round trips cost 13–19 % at the DB, and Velox's
25k ev/s ceiling is a fraction of what the DB can absorb — the remainder is
HTTP + auth + resolve + the Go service, and, at high request rates, the row.

## What this measured is a steady, well-behaved client

The rate is constant and evenly spaced, keys are never retried, timestamps are
server-side, batches are uniform, one tenant, one meter. That is the right
*first* benchmark and what every peer publishes; it is not a burst test and it
does not claim to be. A spike scenario and a retry mix are the two cheapest
next steps and are not part of this run.

---

## What a laptop can and cannot tell you — revised

This benchmark existed on a laptop first, and an earlier version of this page
called the laptop **9× optimistic** because it reported 1,800 ev/s at p99 ≤ 50 ms
where the first AWS run held 200. That verdict was wrong, and it is worth
saying why: the first AWS run's latencies came from a load generator with a
lockstep-burst bug (since deleted), on the test-mode path, against a
one-customer fixture. Under the current protocol the same class of hardware
holds **10,000 ev/s at p99 66 ms** through the full HTTP path — the laptop's
in-process figure was, if anything, **pessimistic** by ~5×.

What did transfer from the laptop, and still does:

| laptop finding | held on the rig? |
|---|---|
| COMMIT dominates in-database time; batching is the lever | **Yes** — the batch ladder is the whole story above |
| the batch curve flattens after ~50 | **Yes** in shape |
| ~26 statements per single-event request | **Yes** — measured on both |
| the per-request wall (hot row) | **Not visible on a laptop** — needs a real RTT to bite at a rate a laptop can offer |

**Use a laptop for ratios and mechanisms; use the rig for absolutes and for
anything that depends on a real round trip.** And never let the generator's
bugs be attributed to either.

## What this does not show

- **Steady state only.** Constant, evenly spaced arrivals; no spikes, no
  diurnal shape, no retries or duplicate keys, server-side timestamps, uniform
  batches, one tenant, one meter. A steady, well-behaved client — the right
  first benchmark, not a "real traffic" claim.
- **"Sustained" means 5 × 10 minutes** with a cool-down between, not an hour
  unbroken. Long enough for autovacuum and checkpoints to appear (they did — see
  footnote ¹); not long enough for multi-hour effects.
- **The read probe is one budget over three endpoints**, reported per endpoint;
  its `usage-summary` result scales with events-per-customer, so the budget it
  meets depends on how big your customers are.
- **No nginx** in front (deploy/compose puts one there); **Single-AZ**, no pooler,
  no replica; on-demand pricing; storage and the load generator excluded from
  the $/hr.
- **The 15,000 ev/s rung is 2 × 90 s**, not sustained; it held with drift ×1.4,
  which is a knee approaching, and it is reported as such.
- **No dashboard in the loop, but the evidence is captured**: every run leaves
  the raw k6 sample stream, a 5-second `vmstat` from the app node, and the RDS
  CloudWatch series over its window (`~/.velox-bench-rig/results-*/`). Every
  claim in the sections above was read from those files, not from a console.

---

## Reproducing

```bash
cd scripts/bench-rig
./run.sh                 # the whole thing: calibrate -> clean-check -> provision
                         # -> watchdog -> bringup -> seed 20M -> SUSTAINED protocol
                         # -> closed-loop ceiling -> pgbench denominator -> teardown.
                         # Stops at the first step that fails; one log in
                         # ~/.velox-bench-rig/run-<timestamp>.log, readable while
                         # it runs. KEEP=1 leaves the rig up (billing).
```

`run.sh` only sequences the scripts below; each is runnable on its own, and
that is how a failed step is re-run:

```bash
./calibrate/calibrate.sh          # 0. instrument: must print CALIBRATED (six cases)
./teardown.sh --check             #    account: exit 0 = clean; 2 = COULD NOT LOOK
./provision.sh --yes              # 1. hardware — billing starts here
( nohup ./watchdog.sh 240 >/tmp/velox-rig-watchdog.log 2>&1 & )
./bringup.sh                      # 2. running, seeded (live mode, 200 customers), verified
TARGET=aws ./seed-history.sh 20000000            # 3. history, into the fixtures' partition
TARGET=aws SUSTAINED=1 CONFIGS="single:200:1 batched:1000:10" PROBE_RATE=5 ./measure.sh
TARGET=aws K6_MODE=max VUS=16 CONFIGS="ceiling:0:500" DURATION=90s ./measure.sh
TARGET=aws BATCH=500 VELOX_EVS=<ceiling ev/s> ./db-ceiling.sh   # 5. denominator
./teardown.sh                     # 6. must end CLEAN
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
GUC — and so do `customers`, `meters` and `api_keys`. Fixtures, key, seeded
history and pgbench rows must all be in the same partition; every script here
detects the fixtures' mode and refuses a mismatch, because an earlier run
silently benchmarked a table it did not think it was measuring.

**On the instances, three things the container-based rehearsal could not
show:** `ec2-user` is not in the docker group (`sudo docker`); `--monitoring`
wants `Enabled=true`; and a bulk seed leaves autovacuum busy for minutes — the
first measured repeat after a 22M-row load hit a 5-second stall and failed its
gate, so `run.sh` settles after seeding.

**Never `git stash` in a shared worktree repo** while working on this: the
stash is repo-global and a `pop` can land another session's work in your tree.

## Superseded: the 2026-08-15 measurements

For the record — the figures this page carried before the protocol existed,
each a single run from a since-deleted generator with a lockstep-burst bug, on
the test-mode path, against a one-customer fixture:

| configuration | published then | under the protocol now |
|---|---:|---:|
| 200 ev/s single-event | p99 119 ms | **p99 4.9 ms** (4/5 × 10 min) |
| 1,000 ev/s batch 10 | p99 269 ms | **p99 8.2 ms** (5/5 × 10 min) |
| closed-loop, batch 500, `db.m7g.2xlarge` | 10,203 ev/s | **25,424 ev/s** |
| "$0.032 per million events" | compute-only, at that ceiling | ~$0.014/M at the new ceiling, same basis; ~$0.035/M at 10,000 ev/s open-loop (app + DB compute, on-demand, ~$1.26/hr) |

The mechanisms the earlier page identified — commits per event as the lever,
batching as the requirement, same-AZ as the placement decision — were right.
The absolutes were not, for reasons that were entirely the measurement's.
