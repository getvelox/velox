# A 0.3-second freeze every 5 seconds — and the one Postgres setting that ends it

**RFM-001 — WAL recycled-pool exhaustion.** Your database looks healthy on
every dashboard. It is stalling every few seconds anyway.

> Entry 001 of the [failure-mode catalog](README.md). Diagnosed on AWS during
> Velox's [sustained-throughput benchmark](../benchmarks/sustained-throughput.md);
> reproducible with the rig scripts in [`scripts/bench-rig/`](../../scripts/bench-rig/).

---

## The symptom

Your write latency is fine. Then, for about a quarter of a second, every
commit stops. Five seconds later it happens again. And again — a metronome —
until it quietly stops on its own.

What makes it hard to catch:

- **The median request is unaffected** (p50 — the middle of your latency
  distribution — looks exactly as it always did).
- **The slowest 1 in 100 requests get 5–10× worse** (p99). A small, unlucky
  slice takes seconds.
- **It follows a lull.** Traffic comes back after a quiet spell, and the
  freezes start.
- **CPU is bored, memory is fine, and the query plans have not changed.**

If you bill per request or serve interactive traffic, your customers are
feeling seconds-long stalls in a system whose dashboards say "healthy".

## Why the dashboards can't answer it

They show it. They cannot explain it.

A latency graph proves the stall happened. A disk graph shows I/O wait
climbing at the same moment. Both are true, and neither tells you *which
mechanism* did it — checkpoint flushing, autovacuum, lock contention and WAL
segment creation all present as "slow writes with I/O wait." So the graphs
give you a list of suspects and no answer, and you end up comparing panels by
eye at 3am.

The evidence that resolves it is not on the dashboard, because nobody
records it: what the WAL segment pool was doing *in the same second* as the
stall.

## The mechanism

Before Postgres changes any data, it first writes what it is about to do
into a log — the write-ahead log, or WAL — so that a crash can be recovered
from. That log is stored as fixed-size files called segments: 16 MB each by
default, 64 MB on Amazon RDS.

Every few minutes Postgres runs a **checkpoint**: it flushes changed data to
disk, which means the older log files are no longer needed. Rather than
delete them all, Postgres **renames some of them for reuse** — keeping a pool
of ready-made files, so a future commit never has to wait for a new file to
be created.

One setting, `min_wal_size`, decides how many files that pool keeps. RDS
ships with **192 MB** — three 64 MB files.

Now run a write-heavy workload after a quiet period. The pool is shallow. A
commit needs a segment that isn't ready, so it **creates one** — allocating a
64 MB file — while holding the lock every other committing transaction needs.
Everyone waits. When the next checkpoint refills the pool, the freezes stop
until the pool runs shallow again.

That is the whole problem: **every commit stops while one of them creates a
file.**

## The signature — how to prove it's this one

Three observations, together, distinguish it from every other suspect:

1. **Coincidence.** Sample the pool depth once per second. Each freeze-second
   coincides with a pool-empty reading — in the instrumented run,
   **11 of 11**.
2. **Negative coincidence.** Zero of those freeze-seconds coincide with a
   logged autovacuum completion. (Vacuum storms produce a different
   fingerprint: average I/O size collapsing ~107 KB → ~6 KB with queue depth
   in the hundreds — that's [RFM-002](README.md), a different entry.)
3. **Arithmetic.** Measured WAL rate × checkpoint interval, compared against
   configured `min_wal_size`. If one checkpoint cycle writes more WAL than
   the pool holds, the pool must run dry inside every cycle.

The median-versus-slowest split above supports the diagnosis but does not
prove it: the median stays flat because only the requests that land inside a
freeze window are hurt.

## The fix

Size the pool to hold at least one checkpoint cycle's worth of WAL, with
slack. Verified under both provocation and full measured series:

```
min_wal_size = max_wal_size ≥ wal_keep_size + 1.9 × WAL rate × checkpoint_timeout
```

Worked example from the rig — a `db.m7g.4xlarge` ingesting through an HTTP
API, every event reconciled against the database:

| Ingest rate | Sized pool | Result |
|---|---|---|
| 12,000 events/s (batch 10) | 16 GB | **5 of 5** ten-minute repeats, worst 10-second p99 **52 ms** (stock: 4/5) |
| 15,000 events/s (batch 100) | 16 GB | **5 of 5**, p99 **40.2–42.8 ms** (stock: 4/5) |
| 25,000 events/s (batch 100) | 16 GB | 4 of 5, **0 drops** (stock: 2 of 5 with drops) — the sampler shows 16 GB is still one size too small here; size to ~24–32 GB |

Applying it takes no restart:

```sql
ALTER SYSTEM SET min_wal_size = '16GB';
ALTER SYSTEM SET max_wal_size = '16GB';
SELECT pg_reload_conf();
```

On RDS, set the same parameters in the parameter group.

**Verify:** the freeze rhythm disappears. Watch p99 across repeats rather
than the median — the median never showed the problem in the first place.

**The cost:** disk space for the pool, and a longer crash recovery, since
more WAL may sit between checkpoints. Shortening `checkpoint_timeout` is the
alternative lever if the space matters more than the extra checkpoint
frequency.

## What this entry does not claim

- **The pool is not the only wall on this path.** In the same campaign,
  checkpoint-era write bursts saturated a 100 GB gp3 volume (3,000 IOPS) at
  15–25k events/s. Where the volume genuinely cannot absorb the rate, a
  deeper pool does not help — that is a capacity problem, not this disease.
  A collapse observed at 25k on one stock series did not replicate on
  another, and is published as *storage-bound, not replicated*.
- **At very high rates on stock settings, the freezes can stop on their own**
  as Postgres grows the pool. But the stalls still happened first — and the
  pool goes shallow again after every restart or quiet spell, so they come
  back next time. Sizing it up front means they never happen at all.
- Everything above was measured on one rig shape, one AZ, in ten-minute
  windows. The method, gates, evidence files and the full *what this does not
  show* section are in the
  [benchmark document](../benchmarks/sustained-throughput.md).

## Reproduce it

The rig that produced these numbers is in
[`scripts/bench-rig/`](../../scripts/bench-rig/) — provisioning, load
generation, the per-second sampler whose pool-empty ticks make the
coincidence visible, and the census script that classifies freeze-seconds by
mechanism marker. An entry in this catalog is only as good as its
reproduction; this one's is public.

---

*Found while benchmarking [Velox](https://github.com/getvelox/velox), an
open-source usage-based billing engine. We publish the failure modes we
diagnose, with their evidence and their fixes.*
