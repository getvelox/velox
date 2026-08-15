# Sustained throughput

The ingest rate one node holds while latency stays inside a budget — and why
that number is 41% lower than the throughput figure we could have published
instead.

**Status: local harness, provisional numbers.** These were measured on a
developer laptop with other work running on it. They are here because the
methodology is the point; the publishable single-node figure comes from a
dedicated cloud run, which has not happened yet. Every caveat is at the bottom
and none of them are buried.

---

## The number that would have been misleading

Running the benchmark the usual way — workers spinning as fast as they can —
gives this:

```
mode:       closed-loop
throughput: 3061 events/sec
p99:        4.9ms
```

**3,061 events/sec at a 4.9 ms p99** is a perfectly true sentence and a useless
one. In a closed-loop benchmark each worker sends its next request only after
the previous one returns, so the system is saturated by construction and the
offered load is *defined* by how fast the system happens to be. The latency it
reports is how long a request waited in a queue the benchmark itself sized. It
cannot tell you what happens to a client sending at a fixed rate, which is the
only thing a production system experiences.

## What we measure instead

`velox-bench --rate N` offers requests on a fixed schedule regardless of how
fast the system responds, and measures each request's latency **from when it
was due to be sent, not from when it was actually sent**.

That second half matters more than the first. If a run falls behind and you
start the stopwatch when the request finally goes out, every request looks fast
while the backlog grows without bound — the benchmark reports excellent latency
for a system that is failing. Timing from the due time is the standard
correction for *coordinated omission*, and it is the difference between a p99 a
buyer can reproduce and one that flatters us.

## The curve

8 workers, single-event path, 20 s per point, isolated Postgres 16 container.

| Offered | Achieved | p50 | p95 | p99 | p99.9 |
|---:|---:|---:|---:|---:|---:|
| 800 | 800 | 3.5 ms | 9.8 ms | 14.5 ms | 27.6 ms |
| 1,000 | 1,000 | 3.5 ms | 11.0 ms | 16.4 ms | 36.9 ms |
| 1,200 | 1,200 | 3.6 ms | 11.1 ms | 18.9 ms | 36.0 ms |
| 1,400 | 1,400 | 3.5 ms | 11.5 ms | 22.7 ms | 53.2 ms |
| 1,600 | 1,600 | 3.6 ms | 13.1 ms | 26.4 ms | 50.3 ms |
| 1,800 | 1,799 | 3.8 ms | 17.8 ms | 39.1 ms | 66.7 ms |
| 2,200 | 2,199 | 4.3 ms | 29.8 ms | 62.4 ms | 95.2 ms |
| 2,400 | 2,399 | 4.9 ms | 37.3 ms | 77.4 ms | 114.0 ms |

The offered rate is met exactly up to 2,400 — the system does not fall behind.
What degrades is the tail: p99 rises 4.3× between 800 and 2,400 while p50 barely
moves. Throughput alone would have shown none of that.

**Against a p99 ≤ 50 ms budget, the sustained rate is ~1,800 events/sec** —
about **41% below** the 3,061 the closed-loop run reports.

## Repeatability

Five consecutive runs at 1,800 events/sec:

| Run | Achieved | p99 | p99.9 |
|---:|---:|---:|---:|
| 1 | 1,800 | 34.2 ms | 56.8 ms |
| 2 | 1,800 | 37.0 ms | 66.9 ms |
| 3 | 1,800 | 21.4 ms | 37.6 ms |
| 4 | 1,796 | 38.4 ms | 77.6 ms |
| 5 | 1,799 | 38.3 ms | 67.2 ms |

The rate is held every time. p99 spans **21.4–38.4 ms, a 1.8× spread** — all
inside the budget, but that spread is why single runs are not quoted anywhere in
this document, and why the interesting rows above were measured at 20 s rather
than 10 s. A 10 s sweep produced a 696 ms p99 at 2,000 events/sec sitting
between a 39 ms result at 1,800 and a 62 ms result at 2,200; that point was
noise from a busy laptop, and it is exactly the kind of number a benchmark
should not publish without repeating.

## Gating

`--slo-p99` turns a run into a pass/fail check that exits non-zero:

```
PASS: p99 27.341ms within budget 50ms at 1800 events/sec
FAIL: p99 331.981ms exceeds budget 50ms
```

There is a third verdict, and it is the one that matters for honesty: if p99 is
inside budget **but the offered rate was not actually delivered**, the run fails
anyway. A system that quietly drops to half the requested load will otherwise
report beautiful latency, and reporting that as a pass would be the most
misleading thing this tool could do.

---

## What this does not show

- **Not HTTP.** The benchmark calls `usage.Service.Ingest` in-process. It
  excludes the router, auth middleware, JSON decoding, and the additional
  transactions the real request path carries. **Treat these as an upper bound on
  what an HTTP client would see, not an end-to-end figure.** Closing that gap is
  the next change to the harness, and we expect the honest end-to-end number to
  be materially lower.
- **Laptop, shared machine.** Other processes were running. The repeatability
  table is the evidence of how much that costs; a dedicated instance is the fix.
- **Single node, single Postgres.** No replication, no read replicas, no
  connection pooler.
- **One meter, one customer, 80 dimension combinations.** Realistic in shape,
  not in breadth.
- **The 50 ms budget is chosen, not derived.** It is a plausible ingest SLO, not
  a commitment. The curve is there so you can apply your own.

## Reproducing

```bash
docker run -d --name velox-bench-pg \
  -e POSTGRES_USER=velox -e POSTGRES_PASSWORD=velox -e POSTGRES_DB=velox \
  -p 55432:5432 postgres:16-alpine
psql "postgres://velox:velox@localhost:55432/velox" \
  -c "CREATE ROLE velox_app LOGIN PASSWORD 'velox_app';"   # before migrating
export DATABASE_URL="postgres://velox:velox@localhost:55432/velox?sslmode=disable"
go run ./cmd/velox-bootstrap                                # migrates, then seeds

go run ./cmd/velox-bench --workers 8 --duration 20s --rate 1800 --slo-p99 50ms
go run ./cmd/velox-bench --workers 8 --duration 10s          # closed-loop, for contrast
```

The `CREATE ROLE` must happen before the first migration. Skipping it leaves
`schema_migrations` dirty at version 1 with no hint about why.
