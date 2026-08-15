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

## End-to-end over HTTP

Everything above calls `usage.Service.Ingest` in-process. `--http` drives the
public endpoint instead — `POST /v1/usage-events`, Bearer auth, JSON in and
out, customer and meter resolved by their public handles rather than handed to
the service pre-resolved. Same pacing code, same coordinated-omission
correction; the only difference is the transport.

**Per-request cost at 100 events/sec** — far below any saturation, so this is
service time rather than queueing. Both modes run back to back, twice:

| Pass | in-process p50 | HTTP p50 | in-process p99 | HTTP p99 |
|---:|---:|---:|---:|---:|
| 1 | 7.78 ms | 15.24 ms | 14.0 ms | 23.4 ms |
| 2 | 7.14 ms | 15.26 ms | 14.9 ms | 30.8 ms |

**The HTTP path costs about 2.1× the in-process path on p50**, and the two HTTP
measurements agree to within 0.02 ms. That is the number to carry: anyone
quoting the in-process figure as an API throughput number is overstating it by
roughly a factor of two on latency alone.

### Why there is no HTTP throughput number here

There should be one, and it is deliberately absent. The HTTP sweep on this
machine saturated at ~570 events/sec and produced results that were **not
monotonic** — 200 events/sec measured a worse p99 (210 ms) than 400 events/sec
(65 ms). Non-monotonic load response is the signature of interference, not of
system behaviour.

A control run confirmed it. Re-running the *in-process* case at 1,200
events/sec while the machine was busy gave p99 29.9 ms against 18.9 ms for the
identical run earlier — a 1.7× degradation from load alone, with nothing about
Velox changed. Meanwhile `com.apple.Virtualization` was consuming 287% CPU and
the Docker backend another 97%: Postgres-in-Docker was using roughly four of
this laptop's eight cores before the load generator and the server asked for
any.

All three tiers — load generator, API server, database — are competing for the
same eight cores. The per-request comparison above survives that because both
sides pay the same tax and it was measured back to back at a rate far below
saturation. A throughput ceiling does not survive it, so it is not published.
**That measurement needs a dedicated instance, and it is the specific reason to
run one.**

---

## What this does not show

- **The rate/latency curve is in-process, not HTTP.** It excludes the router,
  auth middleware, JSON decoding, and the customer/meter resolution the real
  request path performs. Measured overhead for those is ~2.1× on p50, so
  **treat the curve as an upper bound on what an API client would see.**
- **No HTTP throughput ceiling is published**, on purpose — see above. The
  machine could not produce a monotonic one.
- **Laptop, shared machine, and it mattered.** Load average ran 6–10 on eight
  cores during these runs, with Docker's VM alone taking ~4 of them. The
  repeatability table and the control run are the evidence of what that costs.
  A dedicated instance is the fix, not more repeats.
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
# The app role must exist AND hold default privileges BEFORE migrating.
# CREATE ROLE alone is not enough: most tables get their grant from
# ALTER DEFAULT PRIVILEGES, not from a per-table GRANT in the migration, so
# skipping this line leaves 11 tables unreadable by the runtime role and the
# server returns 500 on ingest with "permission denied for table
# provider_cost_rates". This mirrors deploy/compose/postgres-init.sh.
psql "postgres://velox:velox@localhost:55432/velox" <<'SQL'
CREATE ROLE velox_app LOGIN PASSWORD 'velox_app';
GRANT ALL PRIVILEGES ON DATABASE velox TO velox_app;
GRANT ALL ON SCHEMA public TO velox_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON TABLES TO velox_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON SEQUENCES TO velox_app;
SQL
export DATABASE_URL="postgres://velox:velox@localhost:55432/velox?sslmode=disable"
go run ./cmd/velox-bootstrap                                # migrates, then seeds

go run ./cmd/velox-bench --workers 8 --duration 20s --rate 1800 --slo-p99 50ms
go run ./cmd/velox-bench --workers 8 --duration 10s          # closed-loop, for contrast

# End-to-end over HTTP. The bench mints its own key for the bench tenant —
# the bootstrap tenant's key authenticates to a different tenant and would
# 404 on every request.
PORT=8099 LOG_LEVEL=warn go run ./cmd/velox &                # LOG_LEVEL matters:
                                                             # a line per request is
                                                             # real synchronous I/O
go run ./cmd/velox-bench --workers 4 --duration 15s --rate 100 --http http://localhost:8099
```

The role setup must happen before the first migration. Skipping the `CREATE
ROLE` leaves `schema_migrations` dirty at version 1 with no hint about why;
skipping the `ALTER DEFAULT PRIVILEGES` lines produces a cluster that migrates
cleanly and then fails at runtime, which is the more confusing of the two.
