# Leader leases — operator runbook

How Velox decides which replica runs each background job, what to look at
when one seems stuck, and how to pause one on purpose. Design and the
reasoning behind it: [ADR-114](../adr/114-leader-leases-tick-scoped-fencing.md).

## What a lease is

Five background jobs must run on exactly one replica at a time (a
"singleton role"): `billing` and `dunning` (every hour in production, every
5 minutes locally), `webhook_outbox` (2 s), `email_outbox` (5 s) and
`webhook_delivery` (30 s — the retry worker). Each role is one row in the
`leader_leases` table. To run a tick, a replica takes that row for **10
seconds** (the lease), renews it every **3 seconds** while its work runs,
and releases it when the tick ends. The clock is the database's, so the
replicas never need agreed time.

Nothing is elected for longer than one tick. Whichever replica polls first
after the role is due (one interval after the last tick **ended**) takes
the next one. There is no standing leader, and so nothing to fail over.

Three things keep a tick from running twice:

- **A dead holder cannot keep the row.** No renew for 10 s and the lease is
  expired on the database clock; any replica takes it on its next poll
  (every 5 s at most). Worst case a role is silent for **15 s** after a
  crash, partition, VM pause or `SIGSTOP`.
- **A holder that cannot renew stops itself** after 6 s without an
  acknowledged renew — inside the 10 s, so it is gone before a successor
  can start.
- **Every claim statement re-checks the tick's token.** The row carries a
  number that increases on every acquire (the fence token). A tick's
  claims — due subscriptions, due dunning runs, outbox rows, pending
  deliveries — are `... AND leader_fence(role, token)`, so a tick that
  somehow outlived its lease claims nothing. Work already claimed is
  covered by row-level compare-and-swap at its completion writes.

## Look first

```sql
SELECT role, held, holder_id, expires_in_s, last_tick_holder,
       last_tick_age_s, paused_at, paused_by, pause_reason
  FROM leader_status ORDER BY role;
```

| Column | Meaning |
|---|---|
| `held` | a tick is running right now (lease not expired) |
| `holder_id` | `host:pid:hex` of the replica running it — observability only |
| `expires_in_s` | seconds until the lease lapses if the holder stops renewing |
| `last_tick_holder`, `last_tick_age_s` | which replica last finished a tick, and how long ago — **the cluster fact** |
| `paused_*` | an operator paused the role (below) |

The same facts as metrics, on every scrape:

| Metric | Alert when |
|---|---|
| `velox_leader_last_tick_age_seconds{role}` | > 3× the role's interval — no replica has finished a tick; this is the stall signal a single replica's liveness gauge (`velox_scheduler_last_run_timestamp_seconds`, which followers also stamp) cannot give |
| `velox_leader_held{role}` | stays 1 while `last_tick_age` grows — a tick is wedged but still renewing (its heartbeat is alive even when its work is stuck); find the holder in `holder_id` and restart that replica |
| `velox_leader_paused{role}` | 1 outside a planned window |
| `velox_leader_lease_lost_total{role,reason}` | any increase. `heartbeat` = a holder could not renew for 6 s and cancelled its own tick; `release` = the release found the row taken over. Correctness held; the cause (frozen process, database stall, pooler hiccup) is what to chase. The replica that lost a lease waits 60 s before leading that role again |
| `velox_leader_status_scrape_errors_total` | 1 — the view could not be read; the database is the problem, not the roles |

Log lines: `scheduler tick led` (INFO for billing/dunning, DEBUG for the
fast roles) on the replica that ran it; `leader: lease lost` at ERROR.

## Pause a role on purpose

```sql
SELECT leader_pause('email_outbox', 'your-name', 'why');   -- returns true
SELECT leader_unpause('email_outbox');
```

While paused no replica takes the role; work queues and resumes on
unpause (an outbox drains, a billing cycle catches up on its next tick).
A pause survives restarts — it is a row, not a process — so **unpause is
part of the change**, and `velox_leader_paused` is the reminder.

Do not edit `leader_leases` by hand beyond these two functions, and never
delete its rows: the five roles are seeded by migration 0174 and a missing
row is a role that never runs.

## When a role looks stuck

1. `SELECT * FROM leader_status` — is it paused? Is `held` true with a
   growing `last_tick_age_s` (wedged holder — restart that replica) or false
   with a growing age (no replica is polling: check every replica's logs
   and `/health/ready`)?
2. A long tick is not a stuck one: billing over a large tenant set can run
   past its interval; the next tick starts one interval after it ends.
   `velox_billing_cycle_duration_seconds` says how long ticks take.
3. After a database failover the roles carry on — the row moved with the
   data. Expect at most one `lease_lost` per role from the ticks that were
   in flight.

## Poolers and migrations

Every lease statement is self-contained, so the server is safe behind
PgBouncer in transaction mode and RDS Proxy. Migrations are not — they hold
a transaction-scoped advisory lock on a dedicated connection and some run
outside a transaction; run them on a direct or session-mode connection
([postgres-requirements.md](postgres-requirements.md)).

## Drill it

`scripts/partition-drill.sh` severs a holder's network link and reports
when the row expires and a successor acquires — expect ~10-13 s. The
retired advisory-lock drill measured 95 s and depended on TCP keepalives;
this one depends on nothing but the database clock.
