# Sustained throughput

Measured on AWS, on named hardware, with the configuration and the cost stated,
under a protocol that refuses to publish a run it cannot reconcile — and with
the two product bottlenecks it found stated as plainly as the numbers.

**Dates:** 2026-08-16 (`db.m7g.2xlarge`) and 2026-08-16/17 (`db.m7g.4xlarge`,
with the #818 fix); both supersede the 2026-08-15 figures, which are kept at the
end for the record · **Region:** `ap-south-1`, everything in one availability
zone · **Reproduce:** `scripts/bench-rig/` — `./run.sh`, or step by step below.

**At a glance** (open-loop, 5 × 10 min per row, every event reconciled; full tables and the evidence for each below):

| what a self-hoster can plan on | on `db.m7g.2xlarge` (32 GB) | on `db.m7g.4xlarge` (64 GB), #818 fixed |
|---|---|---|
| single events, 200 ev/s | p99 **4.9 ms** | — |
| batch 10, the recommended client shape | 1,000 ev/s at p99 **8.2 ms** (5/5) — and a wall at ~570 req/s (#818) | **12,000 ev/s** (1,200 req/s) at p99 **22.6 ms** (4/5) |
| batch 100 | 5,000 ev/s until the table outgrew RAM (~60M rows) | **15,000 ev/s** at p99 **43.8 ms** (4/5); 10,000 at p99 47.3 ms (4/5) on a table growing 61M → 85M rows |
| what stops it | RAM (index working set falls out of cache) | write IOPS during checkpoints on a 100 GB gp3 volume (3,000 IOPS); the app was never the limit (RDS CPU ≤ 67 %, app node ≥ 75 % idle) |
| closed-loop maximum, batch 500 (not a service level) | 25,424 ev/s | **41,172 ev/s** (p50 187 / p99 226 ms) |
| the database's own floor for this row shape (`pgbench`, 16 clients) | 6,840 one-row commits/s | 7,184 one-row commits/s; **52,713 rows/s at batch 500 — and Velox's closed loop is 78 % of that, 101 % of the same with the RLS protocol** |
| reads under load | list endpoints 2–9 ms at every rate; per-customer usage summary is a linear scan, ~2.7 µs/event (#819) | same |

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

## Second run, 2026-08-16/17: `db.m7g.4xlarge`, with the hot-row fix

Same app node and load generator; the database doubled to **`db.m7g.4xlarge`
(16 vCPU, 64 GB RAM, 100 GB gp3 — same 3,000 IOPS baseline)**; **~$2.21/hr**
on-demand for the three nodes (`ap-south-1` list prices: 1.916 + 0.196 +
0.098), which is **~$0.041 per million events at 15,000 ev/s**; Velox at
`db7f86b0`, which carries the fix for #818. Fresh 20M-row seed, 10-minute
settle, then both ladders (2 × 90 s per rung), then sustained attempts. The
first sustained attempt (10k) ran straight after the ladders on the table they
left (61M rows); **every later attempt started from a fresh 20M-row table**
(TRUNCATE, reseed, 10-minute settle), so a run is judged on its rate, not on
the previous run's leftovers. Table size at the start of each repeat is in the
row.

### The ladders — every rung, zero drops, every event reconciled

| batch 10 | p99 | held? | | batch 100 | p99 | held? |
|---:|---:|---|---|---:|---:|---|
| 4,000 | 8.8 ms | yes | | 5,000 | 39.9 ms | yes |
| **6,000** | **18.5 ms** | **yes — was the #818 wall** | | 10,000 | 42.7 ms | yes |
| 8,000 | 21.4 ms | yes | | 15,000 | 44.2 ms | yes |
| 12,000 | 24.4 ms | yes | | 20,000 | 62 ms | yes (variance) |
| 16,000 | 36–155 ms | yes (variance) | | 25,000 | 54.7 ms | yes |
| 20,000 | — | **no** (delivered ~18.3k, p50 0.9 s) | | 30,000 | 67.7 ms | yes |
| | | | | 40,000 | — | **no** (delivered 30–36k, p50 1.7–2.3 s) |

**#818, verified on the rig**: 6,000 ev/s at batch 10 went from "not held,
~5.5k delivered, p50 1.5 s, thousands of drops" to **p50 7.2 / p99 18.5 ms,
zero drops**, and the batch-10 knee moved from ~570 requests/s to between
1,600 and 2,000. The 25,000 ev/s that was yesterday's *closed-loop ceiling* is
held **open-loop** at p99 55 ms.

### The sustained attempts (5 × 10 min each)

| offered | batch | passed | p50 | p99 (passing repeats) | what stopped it |
|---:|---:|---|---:|---:|---|
| 10,000 | 100 | **4/5** | 35.8 ms | 41.6–49.3 ms | ran on the ladders' table, 61M → 85M rows; repeats 1, 3, 5 flat (p99 41–49 ms), repeat 2 had a checkpoint bump early (queue depth 24, p99 100 ms, but no drift), repeat 4 failed on drift — its tail rose 42 → 67 ms through the run and 137–190 ms in the last 90 s with **no** write-IOPS, read-IOPS, CPU or memory signature during it and a 1,200-IOPS read burst the minute after; consistent with an insert-triggered autovacuum pass, unconfirmed. Repeat 5, at 85M rows, was clean; memory (FreeableMemory) never moved from 46.7 GB in the whole series |
| 25,000 | 100 | **2/5** | 39.0 ms | 52.6–65 ms | fresh 20M table; repeats 1–2 clean, then from repeat 3 on (51M rows +) **checkpoint write bursts of 2,400–3,500 IOPS** against the volume's 3,000 baseline — disk queue depth 37–65 in the stall minutes, ReadIOPS ~0, RDS CPU 58–67 % — storage, not CPU or memory |
| **12,000** | **10** | **4/5** | **8.1 ms** | **22.4–22.8 ms** | fresh 20M table; four repeats within 0.4 ms of each other. Repeat 5 (49M rows) was clean for eight minutes, then its last two minutes ran p99 713 / 515 ms with 463 drops and **p50 unchanged at 8.1 ms**; the only concurrent signal is the run's highest write IOPS (1,300 → 1,609, queue depth 1.5 → 3.2) — a checkpoint, but nowhere near saturation. **1,200 requests/s sustained on the recommended client shape** — the hot row capped this at ~570 |
| **15,000** | **100** | **4/5** | **36.5 ms** | **43.3–44.6 ms** | fresh 20M table; four repeats within 1.3 ms of each other, drift ×0.88–1.01, ReadIOPS ~0 throughout (the cache never missed — FreeableMemory flat at 46.7 GB); repeat 5, at 56M rows, hit **one 50-second stall** (23:52:00–23:52:50Z: disk queue depth 59, write IOPS 2,802 against the 3,000 baseline; 455 drops, p50 up to 1.9 s for that window). 49 of 50 minutes clean |

All four failed repeats turn out to be the same mechanism at different
sizes — see the third run below, which caught it live. Two (25k and 15k) were
big enough to show in CloudWatch's 60-second averages (queue depth 59–65,
write IOPS at the 3,000 baseline); two (10k repeat 4, 12k repeat 5) were not —
though re-reading CloudWatch for 10k repeat 4 *without* cutting the window at
the run's end shows its final minute at queue depth 28 and write latency
17–25 ms, a datapoint the first write-up missed. What the 60-second averages
cannot show is that the stalls are 1-second bursts at the volume's
**throughput** cap; that took the third rig's 1-second instrumentation.

Reading it as capacity: on this rig, steady batch-100 ingest above ~10k rows/s
becomes bound by **write IOPS during checkpoints** — five indexes on
`usage_events` and a 3,000-IOPS volume — before it is bound by CPU (≤67 %) or
memory. At 15k one burst in fifty minutes crossed the line; at 25k they crossed
every few minutes. The levers are provisioned IOPS / `io2`, `max_wal_size` and
checkpoint spreading, or fewer indexes; none of them is Velox code, and none was
pulled for this run — the numbers above are the stock RDS defaults on a 100 GB
volume.

### Ceiling and denominator on the 4xlarge (fresh 20M-row table, 10-minute settle)

| | reached | p50 | p99 |
|---|---:|---:|---:|
| closed loop, 16 workers, batch 500, 90 s | **41,172 ev/s** (3,713,000 events, all reconciled) | 187 ms | 226 ms |

`pgbench` on the same database and row shape, **batch 500, 16 clients, two
interleaved A/B rounds** (`db-ceiling.sh`), right after the ceiling run:

| leg | round 1 | round 2 | median |
|---|---:|---:|---:|
| A — plain batched INSERT, one transaction per 500 rows (the commit floor) | 58,444 rows/s | 46,982 | **52,713 rows/s** |
| B — the same with Velox's per-transaction RLS protocol (`BEGIN`, three `set_config`, INSERT, `COMMIT`) | 45,195 | 36,154 | **40,674 rows/s** |

Velox's closed loop, **41,172 ev/s, is 78 % of the raw commit floor and 101 %
of leg B** — at batch 500 the HTTP layer, auth, customer/meter resolve and the
Go service add nothing measurable; the ceiling is the database's own batched
insert path with the RLS protocol on it (which costs 23 % of the floor here,
13–19 % on the 2xlarge at batch 1). Two things to keep in view: the rounds
were 60 s each and each leg added ~3M rows, and **both legs lost ~20 % from
round 1 to round 2** as the table grew — the same growth sensitivity the
sustained runs show, in a 4-minute window. So "101 %" is "within the
round-to-round variance", not a claim that Velox is free.

And the like-for-like cell for the run-1 comparison — **batch 1**, 16 clients,
two interleaved rounds: A **7,184** rows/s (6,890 / 7,478), B **7,106** (6,943 /
7,269). At 16 clients a one-row commit is round-trip-bound (~2.2 ms per
transaction, 16 in flight), so the RLS protocol costs ~1 % here against 13 % on
the 2xlarge — the cost shows when the DB is CPU-bound, not when it is waiting
on the client. (`db-ceiling.sh` also prints Velox-as-a-fraction lines; they are
only meaningful when `VELOX_EVS` came from a closed-loop run at the same
`BATCH` — the batch-1 legs above are not compared to the batch-500 ceiling.)

## Third run, 2026-08-17: what the tail events are

Same shape (`c7g.2xlarge` app, `c7g.xlarge` generator, `db.m7g.4xlarge`,
100 GB gp3), Velox `97ef608b`, this time instrumented for attribution: a custom
parameter group (`log_autovacuum_min_duration=0`, `log_lock_waits` with
`deadlock_timeout=200ms`, `log_min_duration_statement=250`), Performance
Insights, **Enhanced Monitoring at 1 second**, and a 5-second sampler on the
app node reading `pg_stat_io`, `pg_stat_wal`, `pg_stat_bgwriter`, vacuum/analyze
progress, the GIN pending list, page/extension locks and every backend's wait
event as one JSON line (`scripts/bench-rig/db-sampler.sh`; the analysis is
`analyze-tail.py` + `tail-census.py`; the Postgres log went through pgBadger).
Control = stock RDS parameters, 12,000 ev/s at batch 10, 5 × 10 min from a
fresh 20M-row table — last night's E1 configuration.

### The control caught one, and every source agrees on what it was

Repeat 1 held (p99 24.5 ms, 0 drops) but carried a 45-second event at
04:52:06–04:52:49Z: 10-second p99 120–230 ms against a 22 ms baseline, p50
untouched. In that window:

| source | what it showed |
|---|---|
| Enhanced Monitoring, 1 s | the `rdsdev` device at **10,395 write IOs/s of ~25 KB = 133 MB/s — the gp3 (<400 GB) throughput cap of 125 MiB/s** — with device-mapper queue depth 40–96, while the NVMe beneath merged them into 1,634 IOs of 163 KB at queue 5 / 3 ms: the EBS *throughput* limit, not IOPS, not the disk. Alternating seconds: ~130 MB/s, then ~30 MB/s, every ~5 s |
| the same minute in CloudWatch | 41 MB/s, queue depth 3.2 — a 60-second average of the above |
| Postgres's own writers (`pg_stat_io`, 5 s) | **unchanged**: checkpointer a perfectly even ~1,290 buffers/s (10 MB/s) through the whole checkpoint, client backends 0 writes / 0 evictions (only relation extends, ~930/s), WAL 14 MB/s, `wal_buffers_full` = 0 |
| the excess | ≈ 650 MB over 45 s ≈ **14 MB/s, in 8 KB writes — the WAL rate itself** |
| sampler tick 04:52:11 | one client backend in **`IO:WALInitSync`**, **19 in `LWLock:WALWrite`** |
| Performance Insights, 1 s | `LWLock:WALWrite` = 18 sessions at 04:52:15 |
| Postgres log | the checkpoint completing at 04:52:48 recycled 50 segments; the one before it (04:48:53) *removed 10 and recycled 29* |
| not implicated | autovacuum (its passes fell outside the window; autoanalyze 0.5 s each), the GIN pending list (5–486 pages, its normal sawtooth), buffer eviction (0), lock waits (0 lines) |

Put together: **a WAL segment being created from scratch.** RDS uses 64 MB WAL
segments; when a committing backend needs the next segment and no recycled one
is waiting, it zero-fills 64 MB in 8 KB writes and fsyncs it *while holding
`WALWriteLock`* — every other commit waits. At 14 MB/s of WAL that is one
segment every ~4.6 s, each a ~0.5 s stall at the volume's throughput cap: 10 %
of requests in every 10-second bucket, which is exactly a p99 that jumps while
p50 does not move.

### Why the pool runs dry — from the source, and then provoked on demand

Postgres recycles old segments into pre-made future ones at each checkpoint
completion, keeping segments up to `1.1 × (1 + checkpoint_completion_target) ×
distance_estimate` beyond the redo point, clamped between `min_wal_size` and
`max_wal_size` (`XLOGfileslop`, `xlog.c`); a WAL-driven checkpoint fires at
`max_wal_size / (1 + target)` (`CalculateCheckpointSegments`). At this write
rate on RDS's class-scaled `max_wal_size` (6 GB), checkpoints are WAL-driven,
so the pool left at each completion is roughly what the next cycle consumes —
about 10 % of margin, and two things eat it:

1. **A small checkpoint after a lull decays the distance estimate, and the
   pool is trimmed to it.** RDS's `min_wal_size` (192 MB) puts no floor under
   that. The control's event: the checkpoint at 04:48:53 (the first after the
   warm-up) *removed 10 segments* — pg_wal fell 6.44 → 5.77 GB — and the next
   cycle came up exactly that short: pg_wal grew back to 6.44 GB during the
   stall (+0.67 GB = the ~650 MB of excess 8 KB writes). Across the other 12
   checkpoint completions of the control, pg_wal never moved and there was no
   event.
2. **After an idle, the first checkpoint fires on the timer and refills late.**
   Provocation arm A (stock, idle 15 min, then 12k ev/s): pg_wal flat through
   the idle (nothing removed), the checkpoint after resume started on the
   5-minute timer at 07:32:23 and, spread over 270 s, would complete at
   07:36:53 — 501 s after resume — while the 6.44 GB pool lasted 467 s at
   13.8 MB/s. Result: **+0.47 GB of new segments (= 34 s × 13.8 MB/s, to the
   megabyte), a backend seen in `IO:WALInitSync`, 6 seconds at the throughput
   cap, worst 10-s p99 254 ms**, in a 5-minute run.

| provocation arm (idle 15 min → resume 12k ev/s, 5 min) | `min_wal_size` | `max_wal_size` | 10-s buckets > 5× | worst 10-s p99 | seconds at cap | new segments (pg_wal growth) |
|---|---:|---:|---:|---:|---:|---:|
| A — stock | 192 MB | 6 GB | 3 | 254 ms | 6 | +0.47 GB |
| B — floor only | 6 GB | 6 GB | 5 | 196 ms | 7 | +0.80 GB |
| C — depth only | 192 MB | 16 GB | __C_B5__ | __C_P99__ | __C_CAP__ | __C_WAL__ |
| D — floor and depth | 16 GB | 16 GB | __D_B5__ | __D_P99__ | __D_CAP__ | __D_WAL__ |

The 50-minute series with `min_wal_size = max_wal_size = 6 GB` (T3, same load
as the control) had no sustained event — >5× buckets 5 → 0, in-run seconds at
the cap 14 → 0, `WALInit*` sightings 1 → 0, pg_wal flat — which is consistent
with the floor blocking path 1; but it is one series, and path 2 does not need
a removal, so it is not the whole fix. __ARMS_VERDICT__

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
