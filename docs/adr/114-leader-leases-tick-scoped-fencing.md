# ADR-114: Leader election is a row, not a session — tick-scoped leases with fencing tokens

**Status:** Accepted (2026-08-30) — cutover shipped in PR-D of this arc; PR-E added the PgBouncer transaction-mode CI job and the measured failover numbers (SIGKILL 9.5 s, severed link 9 s — `docs/benchmarks/failure-correctness.md` §5)
**Date:** 2026-08-30
**Amended:** 2026-08-30 — migration 0175 adds `not_before` (cooldown after a lost tick) so `last_tick_*` means completed ticks only (sweep S5)
**Amends:** ADR-006 (background scheduler; the leader gate it assumed), ADR-072 (transport lease model — unchanged, now composed with a leader fence), ADR-073 (boot contract — `VerifyAdvisoryLockTopology` retired)
**Plan of record:** [docs/dev/ha-readiness-2026-07-06.md](../dev/ha-readiness-2026-07-06.md) (2026-08-30 header) · velox-ops `ha-program-2026-08-30.md`

## Context

Five singleton roles run once per tick cluster-wide — billing, dunning, the
webhook outbox dispatcher, the email outbox dispatcher, and the webhook
delivery/retry worker. Since ADR-006 each tick is gated by a **session-scoped
Postgres advisory lock** (`pg_try_advisory_lock` on a pinned connection,
`internal/platform/postgres/advisory_lock.go`). That primitive carried three
costs that the N ≥ 2 production posture (decided 2026-08-30) turns from
theoretical into routine:

1. **Liveness is a TCP session.** A leader whose host vanished held the lock
   until TCP keepalives expired — 2h11m on the shipped defaults — and every
   replica skipped every tick with no error (#797, found by analysis; bounded
   to ~90 s by #800 with keepalive tuning plus a 100-line boot prober that
   exists only to detect the primitive's own fragility).
2. **No fencing.** A leader that was superseded (Postgres failover freed its
   lock while its tick kept running on other connections; a frozen process
   that resumed) could keep issuing statements. Money converged through
   row-level CAS and idempotency indexes; non-CAS effect-firers could
   duplicate (HA-readiness hazard 5).
3. **Not pooler-safe.** Session locks strand under PgBouncer transaction mode
   and RDS Proxy — PgBouncer's own feature matrix rates them "Never" — so the
   server refused to boot behind them (`docs/ops/postgres-requirements.md`), a
   real cost for a product sold as "self-host in your VPC".

## Decision

Leadership becomes a **row per role** in a new table, driven by
**single, self-contained SQL statements evaluated on the database clock**,
with a **fencing token** that every leader-only claim statement proves in its
own snapshot.

### Schema (migration 0174 — re-claim the number at PR time)

`leader_leases(role PK CHECK (role IN (…five roles…)), holder_token BIGINT
NOT NULL, holder_id TEXT, acquired_at, expires_at, last_tick_ended_at,
last_tick_holder, paused_at, paused_by, pause_reason)` with CHECKs that a
held row has a holder and an expiry, and a paused row is unheld. Five rows
are seeded with an epoch-millisecond token so a rollback + re-apply reseeds
above any token ever issued. `leader_fence(role, token)` is a `STABLE` SQL
function (`EXISTS … holder_token = token AND paused_at IS NULL AND expires_at
> now()`); `leader_status` is a view for operators; `leader_pause` /
`leader_unpause` are SQL functions callable from any psql session.
`velox_app` gets `SELECT, UPDATE` on the table and `EXECUTE` on the
functions — never ownership, never `INSERT/DELETE`. No `tenant_id`: the table
sits outside the RLS fence like `schema_migrations`.

### The four statements (all autocommit; no `SET`, no session state, no `BEGIN`)

- **ACQUIRE** — once per poll: `INSERT … ON CONFLICT (role) DO UPDATE SET
  holder_token = holder_token + 1, holder_id = $2, acquired_at = now(),
  expires_at = now() + TTL WHERE paused_at IS NULL AND (expires_at IS NULL OR
  expires_at <= now()) AND (last_tick_ended_at IS NULL OR last_tick_ended_at
  <= now() - interval) RETURNING holder_token, now()`. Racing replicas
  serialize on the row lock; the loser's `WHERE` re-evaluates against the
  winner's row. 1 row = lead this tick; 0 rows = not due, held, or paused.
- **RENEW** — every 3 s while the tick runs, 2 s statement timeout: `UPDATE
  … SET expires_at = now() + TTL WHERE role = $1 AND holder_token = $2 AND
  holder_id IS NOT NULL RETURNING now()`. 0 rows is **definitive loss**
  (takeover, release, or pause) → the work ctx is cancelled with cause
  `ErrLeaseLost`. A statement error is a missed beat, not loss.
- **RELEASE** — after work returns (normal, panic-recovered, or cancelled),
  on a 2 s background ctx: clears the holder; stamps `last_tick_ended_at`
  only if the tick completed, so an interrupted tick leaves the role due —
  with a fixed cooldown (`now() + min(interval, 60 s)`) so a slow database
  cannot turn the hourly tick into a partial tick every few seconds.
- **PAUSE / UNPAUSE** — `SELECT leader_pause('email_outbox', 'who', 'why')`
  nulls the holder and sets `paused_at` in one statement: the fence is false
  from the commit instant, the holder's next RENEW returns 0 rows (≤ 3 s), and
  ACQUIRE is refused cluster-wide. Unpause leaves the role due at the next
  poll. This replaces the psql `pg_advisory_lock(7654000x)` pause trick in
  MANUAL_TEST, which required a session held open.

### Fencing boundary: the five claim funnels

The token travels in ctx (`leader.WithToken`); a leader-only store method
calls `leader.Fence(ctx)` and returns `ErrNoToken` if absent — **no silent
unfenced fallback**. Each of the five claim statements carries `AND
leader_fence($role, $token)` in its innermost `WHERE`, so a stale leader's
claim returns zero rows within the claim's own snapshot:
`subscription.GetDueBilling` (billing), `dunning.ListDueRuns` (dunning),
`webhook.ProcessBatch` claim (webhook_outbox), `email.claimBatch`
(email_outbox), `webhook.ListPendingDeliveries` (webhook_delivery).

Deliberately **not** fenced: completions already in flight (they land through
the existing row CAS — `ErrStaleDeliveryMark`, email `markCAS`, the dunning
CAS, `ClaimAutoCharge`'s lease column, `idx_invoices_billing_idempotency`);
the eight secondary sweeps and reconcilers (CAS/idempotency-guarded or shared
with non-leader callers; fencing them would be a second mechanism for a
closed hazard); operator-path methods (`RunCycleForTenant`, replay). The one
named non-CAS effect-firer reachable from a leader tick —
`ClearPauseCollection` → `subscription.collection_resumed` — becomes a CAS in
PR-B (the clear reports whether it changed the row; the event fires only if
it did), and the subscription period writer is guarded against a resumed
stale leader's in-memory work. *(Amended 2026-08-30, ADR-115: the monotonic
guard this sentence first named — `UpdateBillingCycleTx … AND next_billing_at
< $new` — never shipped and would have been wrong: a plan swap and a
threshold reset truncate a period, moving the watermark backward on purpose.
Every period writer now proves the (status, period start, watermark)
snapshot it read, in the first statement of the transaction that also
inserts the invoice; a truncation from a fresh snapshot is accepted, any
write from a stale one is refused.)*

### Runtime (`internal/platform/leader`, hand-built, ~200 LOC)

Compiled-in constants, like the tick interval: `LeaseTTL = 10 s`,
`HeartbeatEvery = 3 s`, `StatementTimeout = 2 s`, `AbandonAfter = 6 s`,
`MaxPoll = 5 s`; a unit test pins `AbandonAfter + StatementTimeout <
LeaseTTL`. `Manager.Lead(ctx, role, interval, work)` is the parameter
`scheduler.Run` takes (the `scheduler.Gated` refactor's slot): ACQUIRE → run
`work` with a ctx carrying the token, a heartbeat goroutine on its own root
renewing every 3 s → RELEASE on a background ctx so a cancelled parent cannot
skip it. The heartbeat anchors `lastAck` at the RENEW **send** instant on the
monotonic clock and evaluates `AbandonAfter` **before** issuing the next
RENEW, so "no statement of ours starts after the abandon" holds with a real
margin; RENEW/ACQUIRE `RETURNING now()` lets the manager observe DB-vs-app
clock drift without ever comparing a DB timestamp to an app timestamp.
`hostname:pid:8hex` identifies the holder (observability only; never part of
the fence).

### Role model

Per-role leases, five rows, five independent leaders: operator pause is per
role (pausing the email dispatcher during an SMTP key rotation must not stop
billing), the codebase already runs billing and dunning under separate keys
so one replica can run dunning while another finishes a long billing half,
and tick-scoped leases spread roles across replicas by construction (no
rebalancing to build).

### Observability

`SELECT * FROM leader_status` answers "who leads what, since when, expires
when, paused by whom" from any replica. Prometheus: `velox_leader_last_tick_age_seconds{role}`
(cluster fact, scraped from the table by any replica; an error/never-ran
state is exported separately, never as a negative the alert would ignore),
`velox_leader_paused{role}`, `velox_leader_lease_lost_total{role,reason}`
(the named trigger instrument for revisiting the TTL). Transition-only INFO
logs move into the leader package. No doctor check: every reachable row
state is legal or self-heals (the ACQUIRE upsert recreates a missing row;
pause has an alert).

### Rollout

No transition code. Migration 0174 must precede any new replica
(`CheckSchemaReady` enforces it). Old replicas gate on session keys, new on
the table, neither sees the other — so for THIS one upgrade the documented
procedure is Kubernetes `strategy: Recreate` (or `maxSurge: 0`) / compose
`up -d`; the reference stack is N=1. A transaction-scoped advisory-lock bridge
(~40 LOC) was designed and parked with a trigger (a design partner found on
N ≥ 2 of a pre-ADR-114 release): its cost — an idle-in-transaction connection
per led role for the tick, an `idle_in_transaction_session_timeout` floor on
managed Postgres, and a deletion PR next release — fails the earns-its-place
bar for a defect class with no instance.

## Consequences

- **Failover bound:** a dead leader's role is acquirable ≤ 10 s after its
  last heartbeat and running elsewhere ≤ 15 s; a clean SIGTERM releases
  immediately. Process-death takeover regresses from "<1 ms" (socket EOF) to
  ≤ 10 s — inherent to clock-free, session-free leases; the failure benchmark
  will publish the worse number beside the much better partition number.
- **Pooler support:** the app pool is transaction-pooler safe by
  construction (`set_config(…, true)` RLS GUCs are already transaction-local).
  Only migrations (`LockKeyMigrateHybrid`, session-scoped) and `velox migrate`
  keep a direct/session-mode requirement. `VerifyAdvisoryLockTopology`, the
  keepalive bound, the three `Locker` seams and `lock_adapter.go` are
  deleted in PR-D; `docs/ops/postgres-requirements.md` is narrowed, not
  deleted. Proof, not prose: PR-E runs the leadership and claim suites through
  a PgBouncer transaction-mode service container in CI.
- **Split-brain, stated per path:** fenced funnels → the fence; in-memory
  completions → row CAS / idempotency index; the detached collect legs
  (`WithoutCancel` Stripe charge under its own 30 s timeout) → ownership +
  `ClaimAutoCharge` + the attempt-sequence idempotency key. Residual: a
  frozen process that resumes may complete one already-started non-claim
  statement — outbound events remain at-least-once across a takeover
  (documented since ADR-040).
- **Effect-firer audit (owed before hazard 5 reads CLOSED):** every
  `dispatchEvent`/`Enqueue`/audit write reachable from `runBillingCycleForMode`
  and `runCrossModeCleanup`, classified CAS-gated or not, recorded here as
  an amendment by PR-B/PR-D; any non-CAS firer gets a changed-rows gate in
  PR-B's shape.
- **Operators:** pause/unpause is SQL from any session; `leader_status` is the
  first query in the runbook; the partition drill is rewritten around the
  table; MANUAL_TEST's three pause boxes move to `leader_pause`.

## Alternatives considered

- **`cirello.io/pglock`** (121★, 28 importers): read the source — staleness is
  measured on the contender's app clock, not `now()`; `FailIfLocked` (the
  per-tick shape) can never take over from a dead leader; a config typo
  closes the shared pool; lib/pq coupling. It hands back the hard part.
- **River's elector**: internal package, Go ≥ 1.26 from v0.45.0, brings a
  job-queue schema Velox does not use.
- **One global leader**: loses per-role pause and the billing/dunning
  independence already relied on; concentrates every tick on one replica.
- **Full token threading through every money writer**: the site-set is every
  store method the five roles touch (~40 functions) and it would make
  completions refuse to land; the existing CAS layer already covers them.
- **Fencing at `BeginTx` / `FOR SHARE` on the lease row**: universal but
  holds a lock across every leader-tick transaction and couples the fence to
  transaction shape; rejected for the five-funnel form.
- **Tighten advisory-lock keepalives** (drill-measured 13–21 s): inside the
  old design, but leaves the pooler refusal and the session-liveness class.

## Test locks (planned; filled in with measured results at PR-C/D/E)

Real Postgres: two managers contend (exactly one leads per role); leader
killed without release → takeover within TTL, measured; stale token claims
nothing on each of the five real stores (one table-driven test, each row
mutation-verifiable by deleting the fence predicate); pause mid-tick cancels
within one heartbeat and refuses acquire; unpause resumes at next poll; clean
shutdown releases immediately; `TestLease_RolesSeededMatchGoConstants` pins
the Go role list to the CHECK constraint. CI: `test-pgbouncer` job
(transaction mode) running the same suites; the advisory-lock tests failing
through it first is the negative control. Drill: `scripts/partition-drill.sh`
rewritten; `docs/benchmarks/failure-correctness.md` §leader numbers marked
superseded until re-measured.

## Amendment 2026-08-30 (PR-B) — effect-firer audit and the two CAS fixes

The gate condition for hazard 5 reading CLOSED was an enumeration of every
external effect reachable from a leader tick, classified by what stops a
superseded leader from firing it twice. Enumerated from
`billing/scheduler.go` (`runBillingCycleForMode`: reconcilers,
`RetryPendingCharges`, `EnrollStalledForDunning`, `ScanThresholds`,
`ProcessExpiredTrials`, `ProcessExpiredPauseCollections`, `RunCycle`;
`runCrossModeCleanup`; `ProcessDueRuns`):

| Effect | Site | Guard |
|---|---|---|
| `invoice.finalized` webhook (cycle, threshold, cancel-final) | `billing/engine.go` `dispatchInvoiceFinalized` | fires only after the invoice INSERT succeeded; a second creator hits `idx_invoices_billing_idempotency` → `ErrAlreadyExists` → returns before the dispatch (`threshold_scan.go:812-816`) |
| `subscription.pending_change.applied` webhook + audit | `billing/engine.go` after `ApplyDuePendingItemPlansAtomic` | the store returns ONLY the rows its conditional `UPDATE … RETURNING` transitioned (`subscription/postgres.go`); a stale leader gets an empty slice and fires nothing |
| `subscription.threshold_crossed` webhook + audit | `billing/threshold_scan.go:915` | after the fire-once probe + invoice idempotency index; `ErrAlreadyExists` returns `(false, nil)` before the dispatch |
| `subscription.trial_ended` / lifecycle webhooks | `subscription/postgres.go:854,961` `enqueueLifecycle` | enqueued **in the transition's transaction** (`ActivateAfterTrial` CAS on status; `ErrInvalidState` for the loser) |
| `credit.balance_*`, `credit.commit_retired` | `credit/postgres.go:60-83,1087` | in-tx with the ledger append (`ExpireGrantAtomic` CAS) |
| `invoice.paid`, `payment.succeeded`, `payment.failed` | `invoice/postgres.go:917,1131,1151` | in-tx with the settle CAS (`transitioned` / `firstForThisPI`) |
| `dunning.*` webhooks, warning/escalation emails, timeline rows | `dunning/service.go` (`fireEvent` callers at :398/:1028/:1269; emails :1476/:1505) | every transition is `UpdateRunIfActive(run) (applied bool)` and the effect fires only when `applied` (:621,:764,:968,:992); the one `_`-ignored call (:652) is the transient-skip **rewind**, which fires nothing — and, since ha-8 (2026-08-31), the same write proves the attempt count it was derived from (`AND attempt_count = $expected`), so a superseded tick's stale write can neither record nor rewind an attempt |
| `payment.failed` email + `payment_setup_request` email from the auto-charge sweep | `billing/engine.go` collect legs | per-invoice `ClaimAutoCharge` lease (m0141) + the `NoPMNotifiedAt` marker |
| `subscription.collection_resumed` webhook + audit | `subscription/service.go` `ProcessExpiredPauseCollections[ForClock]` | **was unguarded — fixed in PR-B:** `ClearPauseCollection` is now a CAS (`AND pause_collection_behavior IS NOT NULL`); `ErrNotPaused` → the schedule paths announce nothing, the operator path returns the row unchanged |
| billing-cycle period write | `billing/engine.go` `commitPeriodClose`, `handleTrialState`; `billing/threshold_scan.go` `fireThreshold`; `subscription/service.go` `applyCrossIntervalPlanSwapTx` | **was unguarded — fixed in PR-B, superseded by ADR-115 (2026-08-30):** every period writer runs `ClosePeriodTx`, one UPDATE that row-locks the subscription and proves the (status, period start, watermark) snapshot the writer read, as the FIRST statement of the transaction that also inserts its invoice. Plan swaps and threshold resets go through the same CAS — they truncate from a fresh snapshot, which the CAS accepts; a stale write is refused. `ErrWatermarkMoved` → the loser wrote nothing (the engine re-reads once; the swap returns 409 `subscription_period_moved`) |
| Stripe calls from reconcilers (tax retry/reversal, payment_unknown, refunds) | `billing/tax_retry.go`, `creditnote/service.go`, `payment/reconciler.go` | idempotency keys per object (`velox_tax_rev_<cn>`, `inv_taxrev_<inv>`, attempt-sequence PI keys) + CAS marks; Stripe dedups the second call |
| audit rows | everywhere | evidence, not money; a duplicate row is noise the reader tolerates — deliberately not gated |

Verdict: after PR-B no effect reachable from a leader tick fires without a
conditional write in front of it. Hazard 5 reads CLOSED in the HA-readiness
doc from this PR. The two fixes are pinned by real-Postgres tests
(`TestClearPauseCollection_CASOneWinner` — 20 racing clearers, exactly one
winner; `TestClosePeriodTx_CASOneWinner`, which replaced
`TestAdvanceBillingCycle_StaleWatermarkIsNoOp` at ADR-115) and by
service/engine unit tests, each mutation-verified.

