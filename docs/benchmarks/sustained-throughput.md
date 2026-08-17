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
| batch 10, the recommended client shape | 1,000 ev/s at p99 **8.2 ms** (5/5) — and a wall at ~570 req/s (#818) | **12,000 ev/s** (1,200 req/s) at p99 **22.6 ms** (4/5) stock; **5/5, worst 10-s p99 52 ms with the WAL pool sized** (third run) |
| batch 100 | 5,000 ev/s until the table outgrew RAM (~60M rows) | **15,000 ev/s** at p99 **43.8 ms** (4/5) stock; **p99 40.2–42.8 ms, 5/5 with the WAL pool sized** (third run); 25,000 ev/s 2/5 stock with drops → **4/5, p50 36 ms, p99 49–120 ms, 0 drops** with a 16 GB pool that the sampler shows is still one size too small at that rate; 10,000 at p99 47.3 ms (4/5) on a table growing 61M → 85M rows |
| what stops it | RAM (index working set falls out of cache) | at 15–25k, the 100 GB gp3 volume (3,000 IOPS / 125 MiB/s); at any rate after a lull, RDS's default WAL segment pool (third run — one parameter fixes it); the app was never the limit (RDS CPU ≤ 67 %, app node ≥ 75 % idle) |
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

Two of the four failed repeats have a clear storage signature over the whole
minute — 25k and 15k: write IOPS at the volume's 3,000 baseline, disk queue
depth 59–65, p50 up to 1.9 s and hundreds of drops — checkpoint-era write
bursts on a volume with no headroom. The other two (10k repeat 4, 12k repeat 5)
are **not attributed**: re-reading CloudWatch for 10k repeat 4 without cutting
the window at the run's end shows its final minute at queue depth 28 and write
latency 17–25 ms (a datapoint the first write-up missed), so it is not
signature-free, but this rig had no 1-second instrumentation and pg_wal did
not grow in either window, so the third run's mechanism (below) is a candidate
for them, not a finding.

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

## Third run, 2026-08-17: what a tail event is, caught live

Same shape (`c7g.2xlarge` app, `c7g.xlarge` generator, `db.m7g.4xlarge`,
100 GB gp3), Velox `97ef608b`, this time instrumented for attribution: a
parameter group with the logging on (`log_autovacuum_min_duration=0`,
`log_lock_waits` with `deadlock_timeout=200ms`, `log_min_duration_statement=250`),
Performance Insights, **Enhanced Monitoring at 1 second**, and a 5-second
sampler on the app node reading `pg_stat_io`, `pg_stat_wal`, `pg_stat_bgwriter`,
vacuum/analyze progress, the GIN pending list, page/extension locks and every
backend's wait event as one JSON line (`scripts/bench-rig/db-sampler.sh`;
analysis: `analyze-tail.py`, `tail-census.py`; the Postgres log through
pgBadger). Control = stock RDS parameters, 12,000 ev/s at batch 10, 5 × 10 min
from a fresh 20M-row table. Postgres 16.14; RDS defaults that matter here:
`wal_segment_size` 64 MB, `max_wal_size` 6144 MB on this class, `min_wal_size`
192 MB, `wal_keep_size` 2048 MB, `checkpoint_timeout` 300 s,
`checkpoint_completion_target` 0.9, `archive_timeout` 300 s, `wal_init_zero` on.

### The mechanism from first principles (read this if the rest is Greek)

1. **Every commit is written to the write-ahead log (WAL) first**, and the
   commit is not acknowledged until that write is on disk. That is what makes
   a billing event durable.
2. **The WAL is a sequence of fixed-size files** ("segments"; 64 MB on RDS).
   Filling one and moving to the next happens ~every 4–5 s at 12,000 events/s.
3. **Postgres keeps a small stock of ready-made, already-formatted segments**
   ahead of the current one, so switching is instant. The stock is refilled by
   *recycling* at each checkpoint (~every 5 min): old segments that are no
   longer needed for crash recovery are renamed into future ones.
4. **If the stock is empty when the next segment is needed**, the transaction
   that needs it must create the file — write 64 MB of zeros and flush it —
   while holding the lock every other commit needs. Everyone freezes for the
   ~0.2–0.3 s that takes. Until the next checkpoint refills the stock, this
   repeats at every segment boundary: a freeze every ~5 s. Typical writes
   (p50) are unaffected; the slowest 1 % (p99) jump 5–10×.
5. **The stock is sized to about one checkpoint's worth with almost no
   slack**, by Postgres's recycling rule at these settings. Two ordinary events
   remove the slack: a quiet period makes checkpoints small, which makes the
   next one recycle fewer files (RDS's `min_wal_size` = 192 MB is the floor
   that would prevent it), and after an idle RDS's 2 GB of retained log sits
   inside the window, so the stock on resume is shallower than usual and the
   first timer-driven checkpoint refills it too late (`max_wal_size` bounds
   the depth). Both were reproduced on demand.
6. **The fix is a deeper stock:** `min_wal_size = max_wal_size` big enough for
   the worst refill gap at your write rate (`wal_keep_size + 1.9 × WAL rate ×
   checkpoint_timeout`; 16 GB here). The stock is a revolving buffer — used and
   refilled every cycle, never "consumed" — so it costs disk space and nothing
   else. It grows only as segments get created, so set it at provisioning (a
   bulk load grows it for free); raised on a live instance it fills over
   ~15 minutes of peak traffic, stalling in the meantime, then stops.
7. **What it does not fix:** if the volume cannot absorb the write rate
   (15–25k events/s on a 100 GB gp3), that is capacity, and only a bigger
   volume or less WAL per event helps.

### The control caught one

Repeat 1 held (p99 24.5 ms, 0 drops) but carried a 45-second event at
04:52:06–04:52:49Z: 10-second p99 120–177 ms against a 22 ms baseline (1-second
p99 up to 230, worst request 279 ms), p50 untouched. Inside it, from the raw k6
samples: **exactly ten stall-seconds, 4–5 s apart, each 0.17–0.28 s long**
(14–24 % of the requests in that second slowed, 3.5 % over the whole window).
What every source said about those seconds:

| source | what it showed |
|---|---|
| Enhanced Monitoring, 1 s | the `rdsdev` device at **10,395 write IOs/s of ~13 KB = 133 MB/s**, util 95 %, device-mapper queue depth 40–96 — the volume at its throughput ceiling — while the NVMe beneath took them merged (~1,600 IOs/s of ~80 KB, queue 5, await 3 ms; read live from the EM stream, now retained by the fetch step). Roughly 100 MB of writes above baseline in each stall second, ~1 GB over the window |
| the same minute in CloudWatch | 1,039 IOPS, 42 MB/s, queue depth 1.3 — a 60-second average |
| Postgres's own writers (`pg_stat_io`, 5 s) | **unchanged**: checkpointer a perfectly even ~1,290 buffers/s (10 MB/s) through the whole checkpoint, client backends 0 writes / 0 evictions (only relation extends, ~930/s), WAL 14 MB/s, `wal_buffers_full` = 0 |
| sampler tick 04:52:11 | one client backend in **`IO:WALInitSync`**, **19 in `LWLock:WALWrite`** |
| Performance Insights, 1 s | `LWLock:WALWrite` = 18 sessions at 04:52:15 (PI's 1-second samples otherwise mostly miss ~0.2 s stalls) |
| Postgres log | the checkpoint before the event (04:48:53, the first after warm-up) *recycled 29 and unlinked 10* old segments; the one completing at 04:52:48 recycled 50 |
| CloudWatch `TransactionLogsDiskUsage` | pg_wal 6.44 → 5.77 GB at 04:49 (10 fewer files), back to 6.44 GB during the event |
| not implicated | autovacuum (its passes fell outside the window; autoanalyze 0.5 s each), the GIN pending list (5–486 pages, its normal sawtooth), buffer eviction (0), lock waits (0 lines) |

**A committing backend creating a WAL segment.** When the next 64 MB segment
is needed and no pre-made one is waiting, the backend writes 64 MB of zeros
(256 KB `pwritev` calls) and fsyncs the file, holding `WALWriteLock` throughout
— every other commit waits (`IO:WALInitWrite`/`WALInitSync`, `xlog.c`
`XLogFileInitInternal`, reached from `XLogFlush`). At 14 MB/s of WAL that is
one segment every ~4.7 s, each ~0.2–0.28 s here; segment size sets the stall's
grain, the flush rate its length. **Ten stalls = the ten segments the 04:48:53
checkpoint had not recycled.**

### Why the pool runs dry — from the source

Pre-made segments come from recycling at checkpoint completion:
`XLOGfileslop` keeps segments up to `1.1 × (1 + checkpoint_completion_target) ×
distance_estimate` beyond the redo point, clamped to `[min_wal_size,
max_wal_size]`; a WAL-driven checkpoint fires at `max_wal_size / (1 + target)`
(`CalculateCheckpointSegments`). At this write rate the estimate converges to
that distance, `1.1 × 1.9 × D` exceeds `max_wal_size` and the clamp binds — so
the future pool at each completion is `max_wal_size − 0.9·D ≈ 50 segments`,
and the next completion lands 49.4–50 segments later. **The margin is under
one segment**, not the formula's 10 % (that slop only exists once
`1.1 × 1.9 × D < max_wal_size`, i.e. when checkpoints are time-driven). Two
things then take the margin away, both after a quiet spell:

1. **Fewer segments recycled.** Small checkpoints decay the distance estimate
   (10 % each: 50.0 → 48.5 → 43.9 → 40.2 segments across the three that
   bracketed the control's warm-up); once `1.1 × 1.9 × estimate` falls under
   the clamp the recycle target drops and old segments are unlinked instead of
   recycled ("10 removed"). `min_wal_size` is the floor that would stop that —
   RDS's 192 MB never binds. The next cycle is those segments short.
2. **Retained segments inside the window.** RDS keeps 32 segments (2 GB,
   `wal_keep_size`) behind the end of WAL. In steady state those sit before the
   redo point and cost nothing; after an idle (redo ≈ end of WAL) they sit
   *inside* the `max_wal_size` window, so the pool on resume is ~63 segments,
   not ~95 — and the first checkpoint after resume fires on the 5-minute timer
   and only refills when it completes ~270 s later. Neither `min_wal_size` nor
   `max_wal_size` alone changes that arithmetic.

### Provoked on demand: idle 15 min, then 12k ev/s for 5 min

| arm | `min_wal_size` | `max_wal_size` | 10-s buckets > 5× | worst 10-s p99 | seconds at cap | what pg_wal / the pool did |
|---|---:|---:|---:|---:|---:|---|
| A — stock | 192 MB | 6 GB | 3 | 254 ms | 6 | flat 6.44 GB through the idle (nothing removed); stall began 07:34:18, ~300 s after load started, after ~63 segments of WAL — the pool at resume — with the timer checkpoint (started 07:32:23) still writing; +0.47 GB (7 segments created) by the time the 5-min run ended |
| B — floor only | 6 GB | 6 GB | 5 | 196 ms | 7 | same shape: dry after ~57 segments, +0.80 GB created before the run ended |
| C — depth only | 192 MB | 16 GB | 3 | 162 ms | 3 | −1.21 GB *during the idle* (18 unlinked, path 1) then a stall at 09:01:30–09:01:56 |
| D — floor and depth, applied to a shallow pool | 16 GB | 16 GB | 13 | 234 ms | 20 | pg_wal 5.10 → 6.98 GB *during the run*: the setting does not create segments — every new one was built under load, so the whole run stalled every ~5 s (the transition cost of raising the setting; `pg_switch_wal()` to pre-grow is not available to `rds_superuser`) |
| D2 — the same, after the pool had grown to depth (a 12k series ran in between) | 16 GB | 16 GB | **0** | **24.7 ms** | **0** | flat at 12.15 GB through idle and resume; no `WALInit*`, no pile-up — the same recipe that stalled A–D |

The 50-minute series with `min_wal_size = max_wal_size = 6 GB` (T3, same load
as the control) had no sustained event (>5× buckets 5 → 0; in-run seconds at
the cap with small IOs 12 → 0; `WALInit*` sightings 1 → 0; pg_wal flat) — but
it is a **null test** of the floor: its distance estimate never decayed below
the point where the floor would bind, so the treatment was never exercised;
the clean result is checkpoint phase, one series. Arm B is the honest verdict
on that setting.

### The setting under the full series (T5): 12,000 ev/s × batch 10, 5 × 10 min

Same load and protocol as the control, `min_wal_size = max_wal_size = 16 GB`,
pool at 147 pre-made segments when the series began (the reseed had grown it):

| | control (stock) | `min_wal_size` = `max_wal_size` = 6 GB (T3, null test) | **`min_wal_size` = `max_wal_size` = 16 GB (T5)** |
|---|---:|---:|---:|
| repeats passed, p99 range | 5/5, 21.8–24.5 ms | 5/5, 22.1–23.7 ms | **5/5, 22.4–23.3 ms** |
| 10-s buckets > 3× / > 5× the series median | 5 / 5 | 2 / 0 | **0 / 0** |
| worst 10-s p99 | 177 ms | 75 ms | **52 ms** |
| seconds at the throughput ceiling with small IOs, inside repeats | 12 | 0 | 1 |
| sampler ticks with a backend in `WALInit*` | 1 | 0 | 0 |
| checkpoints timed / WAL-driven | 3 / 10 | 2 / 9 | **10 / 0** |
| pre-made segments, min / median (`pg_ls_waldir`) | not sampled | not sampled | 45 / 99 |
| segments unlinked at checkpoints | 10 | 0 | 0 |

With the pool deep, checkpoints are time-driven (so the recycling formula's
10 % slop is back in play), the pool never fell below 45 segments, and no
tail window appeared. One series each — but three of the four provocation
arms stall on demand and this setting is the only one under which the pool
never got near empty. And the provocation that stalled every other arm (D2 in the table above) did not stall it.

Then the same setting at **15,000 ev/s × batch 100** (T6, 5 × 10 min, fresh
20M): **5/5, p50 33.6–34.0 ms, p99 40.2–42.8 ms**, 0 drops, no tail bucket,
worst 10-s p99 88 ms, all 9 checkpoints time-driven, WAL 17.6 MB/s, pool
minimum 30 pre-made segments (median 82) — thinner than at 12k, as the rule
predicts when the rate rises. Last night's stock run at this rate was 4/5
(43.3–44.6 ms) with a 50-second volume-saturation stall. Caveat on repeat 5:
the operator's laptop slept mid-series; k6 ran to completion on the load
generator (90,001 iterations, 9,000,100 events, matching the row count) and its
summary and samples were read afterwards, so its p50/p99/drift/drops come from
the same files as every other repeat, read by hand. 

And at **25,000 ev/s × batch 100** (T7, same protocol): **4/5, p50 35.6–36.1 ms,
p99 49–120 ms, 0 drops, every event reconciled** — the one failure a drift
gate (repeat 2's last third ×2.9 its first, p99 67 ms), not a stall. Compare
stock last night: 2/5, p50 up to 1.9 s, hundreds of drops, disk queue depth
37–65. Here the pool is *not* deep enough and the evidence says so: WAL runs at
29.6 MB/s, so the rule asks for ~19 GB and 16 GB is short — the sampler shows
the pre-made pool at **0 segments at 17:10 and 17:25**, two checkpoints
turned WAL-driven (`starting: wal`), 471 seconds at the throughput ceiling
with small IOs, two `WALInit*` sightings, 18 buckets between 3× and 5× (none
above; worst 10-s p99 208 ms), and total WAL files growing 181 → 244 through
the series — the created segments deepening the pool, which is why repeats
3–5 (p99 49–64 ms) are cleaner than 1–2. **So the boundary is measured: 16 GB
covers 12–15k on this rig; at 25k size to ~24–32 GB (or shorten
`checkpoint_timeout`).**

The same reading offers a candidate for last night's stock 25k/15k stalls that
this write-up still classifies as storage-bound: at 29.6 MB/s a stock pool of
~50 segments is consumed in ~108 s — dry at *every* cycle end — and the
segments created are unlinked at the next checkpoint, so `TransactionLogsDiskUsage`
stays flat while the churn's 8 KB writes consume the volume's 3,000 IOPS. It
fits the 2,400–3,500 IOPS / queue 59–65 signature and the fact that the same
load with the pool sized showed no saturation and no drops; it is not provable
from that rig's data, so it stays a candidate.



### What this leaves

- **Not attributed:** last night's 12k repeat 5 and 10k repeat 4 (this
  mechanism is a candidate; pg_wal did not grow in their windows, and that
  rig had no 1-second view). The 15k/25k failures are storage-bound bursts as
  originally written.
- **Product side, quantified from the same evidence:** Velox writes ~1.2 KB of
  WAL and 7.7 WAL records per 200-byte event; full-page images are *not* the
  driver (0.05 per event with `wal_compression=zstd`). The event insert runs
  as **one statement per event** (36.1M calls for 36.1M events at batch 10 —
  a `WITH rate AS (SELECT … provider_cost_rates …) INSERT …`), plus three
  foreign-key `FOR KEY SHARE` lookups per event (customers, meters, and the
  *same* tenants row 12,000×/s — visible as `LWLock:MultiXactGen`); index
  growth per event: customer_meter 121 B, idempotency 117 B, pkey 88 B,
  tenant_time 37 B, GIN 9 B, heap 247 B. Multi-row inserts per batch and a
  cheaper strategy for hot parent rows are the levers; they cut the WAL rate
  every number in this section scales with. Filed as
  [#823](https://github.com/getvelox/velox/issues/823), not built here.

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
  footnote ¹, and the third run); not long enough for multi-hour effects.
- **The read probe is one budget over three endpoints**, reported per endpoint;
  its `usage-summary` result scales with events-per-customer, so the budget it
  meets depends on how big your customers are.
- **No nginx** in front (deploy/compose puts one there); **Single-AZ**, no pooler,
  no replica; on-demand pricing; storage and the load generator excluded from
  the $/hr.
- **On the `db.m7g.2xlarge`, 15,000 ev/s was only a 2 × 90 s ladder rung**
  (drift ×1.4 — a knee approaching), not a sustained result. The sustained
  15,000 ev/s figure on this page is the 4xlarge: 4 of 5 ten-minute repeats,
  drift ×0.88–1.01; repeat 5 hit one 50-second write-IOPS stall at 56M rows
  (455 drops). Reported as 4/5, not rounded up.
- **The tail attribution (third run) rests on one control event and four
  provocation arms** on one instance shape; the two run-2 events it does not
  explain are named as unexplained. Two settings tested (`min_wal_size` alone,
  `max_wal_size` alone) did not hold up under provocation; the combined
  setting is verified only in the arms and series shown.
- **No dashboard in the loop, but the evidence is captured**: every run leaves
  the raw k6 sample stream, a 5-second `vmstat` from the app node, and the RDS
  CloudWatch series over its window (`~/.velox-bench-rig/results-*/`); the
  third run adds the 5-second DB sampler, 1-second Enhanced Monitoring and
  Performance Insights pulls and the Postgres log per series. Every claim in
  the sections above was read from those files, not from a console.

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
