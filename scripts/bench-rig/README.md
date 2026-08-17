# Velox benchmark rig — reproduce the numbers yourself

Everything in `docs/benchmarks/sustained-throughput.md` is produced by the
scripts in this directory, and the point of this README is that **you can run
them and check us**. One command does the whole thing; every step is also
runnable on its own.

```bash
cd scripts/bench-rig
./run.sh            # ~2.5 h, ~$3.50 on the large rig; stops at the first failing step
```

## What you need before `run.sh`

| need | why | check |
|---|---|---|
| **AWS account + CLI** configured for `ap-south-1` | the rig is 2 EC2 (Graviton) + 1 RDS in one AZ | `aws sts get-caller-identity` answers |
| **permissions**: EC2 (run/terminate instances, key pairs, security groups, describe), RDS (create/delete instance + subnet/parameter group, describe, log download), SSM `GetParameter` (AMI lookup), CloudWatch `GetMetricStatistics` + Logs `FilterLogEvents` (evidence capture, optional), Service Quotas read (optional); **optional, for the 1-second view**: `pi:GetResourceMetrics`/`pi:Describe*` (Performance Insights via API) and an IAM role named `velox-bench-rds-monitoring` with `AmazonRDSEnhancedMonitoringRole` attached (Enhanced Monitoring; the name matters — the benchmark policy may only pass `velox-bench-*` roles) | that is everything the scripts call. **No IAM writes are needed or used**; the role and the PI grant are one-time account-owner actions, and everything works without them, just without 1 s OS/wait-event data | `./teardown.sh --check` exits `0` (clean) — exit `2` means the CLI cannot see the account |
| **quota**: ≥ 12 on-demand Standard vCPUs in the region (`L-1216C47A`) for the large rig, ≥ 6 for the small | `c7g.2xlarge` (8) + `c7g.xlarge` (4) | `aws service-quotas get-service-quota --service-code ec2 --quota-code L-1216C47A` |
| **a default VPC** in the region with subnets in `ap-south-1a` and `ap-south-1b` | RDS subnet groups need two AZs even for Single-AZ | `aws ec2 describe-vpcs --filters Name=isDefault,Values=true` |
| **locally**: `go`, `k6`, `psql`, `ssh`, `curl`, `python3` (any), `aws` | `calibrate.sh` needs go + k6; the rest is glue | `k6 version` — pin **v2.2.0**, the version the calibration was proven against |
| **macOS or Linux** shell | every script parses under macOS's bash 3.2 as well as bash 5 | — |

Cost is on-demand `ap-south-1` compute: ~$0.44/hr small rig, ~$1.26/hr large.
Nothing here needs a NAT gateway or an IAM instance profile.

## What `run.sh` does, in order — and what stops it

| step | script | stops the run when |
|---|---|---|
| 0 | `calibrate/calibrate.sh` | the load generator fails any of six checks against a stub with a known truth (see below) |
| 0 | `teardown.sh --check` | the account is not clean, or the CLI could not look (`UNKNOWN`) |
| 1 | `provision.sh --yes` + `watchdog.sh 240` | AWS refuses; a watchdog force-tears-down at 4 h if nothing else does |
| 2 | `bringup.sh` | cross-AZ RDS, dirty schema, an app role that cannot read tables, the server falling back to the admin pool, or a 201 that wrote no row |
| 3 | `seed-history.sh 20000000` | rows land in the wrong livemode partition, or the count does not match |
| 4a | `measure.sh` (`SUSTAINED=1`) | continues, but any repeat that fails its gate is reported `[FAIL: reason]` and excluded |
| 4b | `measure.sh` (`K6_MODE=max`) | the closed-loop ceiling, one 90 s run |
| 5 | `db-ceiling.sh` | pgbench on the same DB and row shape — the denominator |
| 6 | `teardown.sh` | does not reach `CLEAN` |

Set `KEEP=1` to leave the rig up afterwards (it keeps billing). `TARGET=local
ADMIN_DSN=postgres://…` runs the identical sequence against a local Postgres,
which is how the sequence is tested without spending anything.

## Reading the result

`~/.velox-bench-rig/results-<timestamp>/summary.txt`, one line per configuration:

```
batched   offered 1000 ev/s batch 10 | 5/5 passed | p50 median 9.1ms (8.7-9.4) | p99 median 41ms (33-58)
ceiling   CLOSED-LOOP 16 VUs batch 500 | 1/1 passed | ceiling median 9870 ev/s | p50 median 780ms | ...
```

- **`n/N passed`** — a configuration is only "held" at a rate when *every*
  repeat passed. The gate per repeat: k6 exit 0 (thresholds held), 0 dropped
  iterations (the rate was actually offered), 0 failed requests, events
  claimed > 0, events claimed == rows written, Σ quantity sent == Σ gained,
  ≥ 1000 latency samples, last-third p99 ≤ 2× first-third (drift).
- **The number is latency, not ev/s.** In rate mode ev/s equals what you
  offered whenever nothing dropped; the medians and ranges of p50/p99 across
  repeats are the measurement.
- **`[FAIL: dropped=…]`** — not sustained at that rate; lower it.
  **`[FAIL: samples=…<1000]`** — the run was too short for a p99; lengthen it
  (the summary says INSUFFICIENT SAMPLES, not NOT SUSTAINED).
  **`[FAIL: probe=DEGRADED]`** — the read path missed its p99 budget while
  ingest ran. That is a finding about the product, not the rig.
- **`ceiling`** is closed-loop: a maximum, never a service level. Its p50 is
  queue depth. It is there so a buyer can compare with vendors who only publish
  that kind of number.
- **`db-ceiling`** prints the DB's own commit floor for this row shape (leg A),
  the same with Velox's per-transaction RLS protocol (leg B), and the closed-loop
  ceiling as a fraction of each.

Every run also leaves its evidence: `<tag>.txt` (k6 summary), `<tag>.k6.json`,
`<tag>.samples.jsonl.gz` (raw k6 samples), `<tag>.app.vmstat.log` (5 s CPU on the
app node), `<tag>.rds.*.json` (RDS CloudWatch, 60 s). Load them into Grafana if
you want a picture; the verdict never depends on it.

What that evidence can and cannot attribute: a storage stall shows up plainly
(write IOPS at the volume's ceiling, disk queue depth in the tens); a tail event
with storage, CPU and memory all flat at 60-second resolution does not — two
such repeats in the second AWS run stayed unexplained until the third run's
1-second tooling (next section) caught the mechanism.

## Attributing a tail event (the third run's tooling)

A gate can only say a repeat failed; these say *why*, at 1–5 second resolution:

- `provision.sh` attaches a parameter group that turns on the logging that
  attribution needs (`log_autovacuum_min_duration=0`, `log_checkpoints`,
  `log_lock_waits` with `deadlock_timeout=200`, `log_min_duration_statement=250`
  without parameters, `pg_stat_statements.track=all`), Performance Insights, and
  Enhanced Monitoring at 1 s when the role exists. Numbers a run publishes are
  still the engine's stock behaviour — none of this is tuning.
- `db-sampler.sh start|stop|fetch|settings <tag>` — one SQL statement every 5 s
  on the app node, one JSON line per tick: wait events of every backend,
  `pg_stat_io`, `pg_stat_wal`, `pg_stat_bgwriter`, vacuum/analyze progress, the
  GIN pending list, page/extension locks, active statements > 100 ms. `fetch`
  also pulls the RDS postgres log for the window, PI 1 s wait-event load, and
  EM 1 s disk/memory samples.
- `tail-series.sh <label> <rate> <batch> [repeats]` — one series, control or
  treatment, run identically: reseed, settle (sampler already on), measure,
  fetch, `analyze-tail.py`, pgBadger. `SKIP_SEED=1` resumes at measure.
- `analyze-tail.py <dir>` — finds tail windows in the k6 samples (10 s buckets
  with p99 > 3× the run's median) and lays every source side by side for each:
  DB counter deltas vs the five minutes before, wait-event census, EM seconds,
  PI seconds, log lines. `tail-census.py <dir> --series A --series B` — the
  series-wide numbers a treatment is judged on.
- `provoke-walinit.sh <label> <min_wal_size>` — reproduces the mechanism the
  third run found on demand: apply the setting, idle past a `checkpoint_timeout`
  so a small checkpoint trims the WAL segment pool, resume the rate, read
  `TransactionLogsDiskUsage` growth and the 1 s device writes.

What the third run found with these, in one line: the tail events were WAL
segment creation under `WALWriteLock` when the recycled-segment pool ran dry
(RDS default `min_wal_size` = 192 MB) — `docs/benchmarks/sustained-throughput.md`
§ third run, and the runbook entry "On RDS, set `min_wal_size`".

## Why the calibration comes first

`ingest.js` reports numbers about Velox, and nothing about Velox tells you
whether those numbers are true. `calibrate/` points the same script at a stub
with a known service time and exact server-side counters and checks six things:
delivered rate == offered and p50 == truth; server errors are reported and NOT
counted as throughput; a too-slow server is reported at the *delivered* rate
with drops flagged; no idempotency key is ever replayed across runs (a replayed
key measures the dedupe path); a known 5%-at-200 ms tail comes back as p99 ≈ 200;
a known +20 µs/request slope shows up in the drift line. Every one of those
checks corresponds to a bug that actually shipped in this rig once.

## Things that will bite you (each one did)

- **The bench is live-mode by default** (`BENCH_LIVEMODE=false` for test mode).
  Test-mode ingest does a per-event test-clock lookup production skips, so
  numbers measured in test mode understate production. Fixtures, key, seeded
  history and pgbench rows must all be in the same partition; the scripts refuse
  otherwise.
- **`k6` puts your process environment in `__ENV`.** Exporting `PROBE_RATE`,
  `VUS`, `MODE`, … reconfigures the script silently. `calibrate.sh` and
  `measure.sh` scrub these; if you run `k6` by hand, pass everything with `-e`.
- **RDS is not publicly reachable.** `seed-history.sh` and `db-ceiling.sh` take
  `TARGET=aws` and run themselves on the app node over ssh; `measure.sh` runs k6
  on the loadgen node. Do not point them at the RDS endpoint from a laptop.
- **On the instances**: `HOME` is unset under cloud-init (Go builds fail
  without `HOME`/`GOPATH`); `ec2-user` is not in the docker group (`sudo
  docker`); k6 has no arm64 rpm (tarball only); `--monitoring` wants
  `Enabled=true`. `provision.sh` and `bringup.sh` handle all four; they are
  listed so you recognise them if you change it.
- **A bulk seed leaves autovacuum busy for minutes.** The first measured repeat
  after a 22M-row load hit a 5-second stall 20 s in and (correctly) failed;
  `run.sh` settles after seeding. If you seed by hand, wait before measuring.
- **Two gates, reported separately.** "Ingest sustained?" and "reads within
  budget?" are different questions; `PROBE_GATE=0` reports the read probe
  without failing ingest on it, and the probe is broken down per endpoint.
  On the real rig `usage-summary` cost ~2.7 µs per event the customer holds
  (#819) — the budget it meets depends on customer size, not write rate.
- **Watch the read side of the database.** A tail that rises within a run
  with CPU and write IOPS flat is the index working set falling out of cache:
  ReadIOPS, ReadLatency and DiskQueueDepth are captured for that reason.
- **Never start a long step inside a shell that might be killed** (a tool call
  with a timeout, a terminal that closes): `nohup … &` it in its own command
  and watch the log. A whole rung was lost to that once.
- **macOS bash 3.2** cannot parse a `case` inside a heredoc inside `$( )`.
  Every script here is checked with `/bin/bash -n` for that reason.
- **A stale calibration stub can hold port 8123** if a previous run was killed
  mid-pipeline. `start_stub` now kills by port and verifies `/whoami`; if you
  see `NOT CALIBRATED` on case 2 with ~525 failures, that was it.

## Small rig vs large

```bash
./run.sh                                                             # small: c7g.large + db.m7g.large
APP_TYPE=c7g.2xlarge DB_CLASS=db.m7g.2xlarge ./run.sh                # large: what produced 10k ev/s
```

The published page states which rig each row came from, the table size at the
time, the mode, and the customer count. If your run disagrees with ours, that
is interesting — open an issue with your `summary.txt` and `runs.txt`.
