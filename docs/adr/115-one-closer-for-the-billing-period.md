# ADR-115: One closer for the billing period

**Status:** Accepted (2026-08-30)
**Supersedes:** the watermark compare-and-swap of ADR-114 PR-B (`AdvanceBillingCycle`) and the never-shipped monotonic guard ADR-114 §Scope named; extends ADR-066's fire+re-anchor atomicity to every period writer.
**Related:** ADR-065 (threshold scan boundary / fire-once), ADR-066 (threshold watermark protocol), ADR-114 (leader leases; `POST /v1/billing/run` stays unfenced).

## Context

A subscription's billing period is written by four things: the cycle close
(the leader's tick, or an operator's `POST /v1/billing/run`), a threshold
fire (`reset_billing_cycle=true` re-anchors the period; `=false` does not
touch the row at all), an immediate cross-interval plan swap (truncates or
re-anchors), and a cancel (terminates it). Before this decision they had
disjoint idempotency seams — the cycle invoice's `(subscription, period
start, period end)` index for the close, the partial threshold index for the
fire, nothing at all for the swap — and no shared row lock. Two of them could
plan from the same period and both commit.

The concrete defect (sweep 2026-08-30, S1): the leader's threshold-scan fire
and a cycle close are two unserialized period writers. An operator drain
(`RunCycleForTenant`, deliberately unfenced per ADR-114 §97) or the leader
itself can close the period while the fire is in flight. Usage in
`[P0, t1)` then lands on both the threshold invoice and the cycle invoice,
and in one order the reset arm's `UPDATE … WHERE id` rewinds the watermark
the close had just advanced. Reachable at N=1: an HTTP goroutine against the
scheduler goroutine.

The close also committed its invoice and advanced the period in two separate
transactions, so a crash between them left a committed invoice with the
subscription still due (healed on re-entry), and the fire's `reset=false`
arm never wrote the row, so nothing could serialize it against anyone.

## Decision

**No invoice that bills a subscription's period window, and no write that
moves a subscription's period, commits except inside one transaction whose
FIRST statement row-locks the subscription and proves that
`(status, current_billing_period_start, next_billing_at)` still equal the
snapshot the work was derived from — and, when an invoice is inserted, that
the threshold watermark it was built against is still the watermark.** Two
writers planning from the same period can never both commit.

Concretely:

1. **`subscription.ClosePeriodTx` is THE period writer.** One `UPDATE`
   whose `WHERE` carries the caller's `PeriodSnapshot` (status, period
   start, watermark). The UPDATE takes the row lock (`FOR NO KEY UPDATE` —
   no period column is in a unique index) and proves the snapshot in the same
   statement; under READ COMMITTED a writer that blocked on another writer's
   lock re-evaluates the `WHERE` against the row that writer committed, so
   the loser sees 0 rows and returns `ErrWatermarkMoved` having written
   nothing. `AdvanceBillingCycle`, `UpdateBillingCycle` and
   `UpdateBillingCycleTx` are deleted; the terminal close is
   `FireScheduledCancellationTx`, the same CAS plus `status = 'active'`.
2. **Every closer runs the CAS as the first statement of its transaction,
   and the invoice rides the same transaction.** The cycle close
   (`commitPeriodClose`): CAS → in-tx watermark re-read → invoice number →
   invoice + lines → finalize audit row. The threshold fire: CAS → number →
   invoice → audit, for BOTH reset arms. The plan swap: CAS → refund drafts
   → plan write → day-1 invoice. A lost race burns no invoice number and
   leaves no half-written state; the crash window between "invoice
   committed" and "period advanced" no longer exists.
3. **The `reset=false` fire writes the row verify-only.** Same values, so the
   period does not move, but the UPDATE takes the lock and proves the
   snapshot through the one statement every closer shares. It must be an
   UPDATE, not `SELECT … FOR UPDATE`: one statement, one lock mode, one test
   — and `FOR UPDATE` would take the stronger lock that blocks the foreign-key
   `FOR KEY SHARE` of unrelated invoice inserts.
4. **The close re-reads the threshold watermark inside its transaction.** A
   `reset=false` fire that commits between the close's line build and its
   commit passes the CAS (the period did not move) but has already billed
   part of the window. The close compares the newest threshold invoice for
   the period, read on the closer's tx after the row lock, with the watermark
   its lines were built against; a mismatch rolls back and the engine
   re-plans once (`billSubscription`'s one bounded retry — a second miss
   leaves the sub to the next fetch).
5. **The swap refuses a period it cannot truncate honestly.** After its CAS,
   the swap reads the same watermark under the row lock; if a threshold fire
   already billed past the swap's effective instant, the swap returns 409
   `subscription_period_moved` rather than re-anchoring the period below the
   fire's window (which would re-bill `[now, fire end)` at the next close).
   The operator's instant is never moved silently.
6. **A read-side alarm.** `velox-doctor` gains
   `usage_billed_once_per_subscription_meter_window`: the same usage instant
   of one (subscription, meter, rating-rule bucket) on two live invoices —
   the money shape the CAS exists to prevent, on the read side, no schema.
   Paired per rating-rule bucket because under ADR-066 §4 one meter can carry
   a sum bucket (billed on the fire) and a max bucket (deferred to the close,
   full window) whose windows legitimately overlap.

Why the snapshot, not monotonicity: a plan swap and a threshold reset
truncate a period — they move `next_billing_at` backward on purpose. A
monotonic guard (`next_billing_at < $new`) would reject them. The snapshot
CAS accepts a truncation from a fresh read and rejects any write from a
stale one; "stale" is the property, not "backward".

Why the store owns the transaction: `SubscriptionReader.WithTenantTx`
replaces the engine's `TxRunner` seam. The store that owns the period state
machine opens the coordinator transaction (the `CancelAtomicWithBill`
ownership rule), the engine hands the same `*sql.Tx` to the invoice and
settings stores.

## Rejected angles

- **Route `POST /v1/billing/run` through the leader.** `Manager.acquire`
  makes billing due one interval after `last_tick_ended_at`, and the operator
  drain is a synchronous `invoices_generated` contract; fencing it would
  either queue the operator behind the tick or turn the response into a
  promise. ADR-114 §97 stands: operator paths are unfenced, and the row CAS
  — not the lease — is the split-brain guard. The tests hold a live billing
  lease on BOTH writers to prove it.
- **Fold the threshold fire into the cycle close** (a "pure reset").
  Loses `billing_reason='threshold'`, the prorated base of a reset fire, and
  the ADR-066 §4 deferred-bucket protocol.
- **A version column, a trigger, or an `EXCLUDE` constraint on usage
  windows.** Belt-and-suspenders on the composite CAS, which is one
  statement. The doctor check is the read-side alarm instead.
- **A reconciler that voids the second invoice.** The bad state must be
  unreachable inside one transaction; a sweep that repairs it would be the
  design, not the fallback.

## Consequences

- `PATCH /v1/subscriptions/{id}/items/{item}` may return 409
  `subscription_period_moved` (the period changed while the request ran, or
  a threshold fire billed past the requested instant): re-read and retry.
- `POST /v1/billing/run` may report 0 for a subscription the leader is
  committing at that instant — the due fetch is `SKIP LOCKED`; the leader's
  commit bills it.
- A lost race logs one Info line and writes nothing; it does not increment
  `velox_billing_cycle_errors_total` (the engine absorbs it inside
  `billSubscription`), and it burns no invoice number.
- The `reset=false` fire now bumps `updated_at` on the subscription row.
- **Operational prerequisite (documented, not enforced at boot):**
  `ALTER ROLE velox_app SET idle_in_transaction_session_timeout = '30s';`.
  A frozen holder of a subscription row lock now stalls every other closer of
  that subscription and hides it from the `SKIP LOCKED` fetches; the timeout
  is the liveness bound. No closer transaction spans network I/O (tax runs
  before, the charge after), so a healthy holder releases in milliseconds.
- **Rolling deploy window.** No schema changes, so old and new binaries
  coexist; an old replica still runs the unguarded writers for the length of
  the rollout — the same window as before, not wider. Reference deploy is
  `Recreate` / N=1 (ADR-114); on N≥2 do not drain with `POST /v1/billing/run`
  until every replica runs the new binary.
- **Named crash-point loss.** Merging the advance into the invoice's
  transaction removes the `ErrAlreadyExists` re-entry that healed a crash
  between the close commit and the born-$0 / fully-credited `MarkPaid` (that
  heal only ran while the sub was still due). No money moves; the row sits in
  the attention queue. Trigger: the first observed
  `finalized AND payment_pending AND amount_due <= 0` row → a state-derived
  sweep calling the existing `healStrandedZeroDue`.
- Not covered, with triggers: a pause landing between snapshot and commit
  (one more predicate in `ClosePeriodTx` on the first paused-sub charge
  complaint); a schedule landing after the snapshot fires one period later
  (pre-existing); an epoch-column switch is the trigger for a third
  closer-side state that lives off the row.

## Tests

Real Postgres unless noted; each mutation-verified (the named guard removed
turns the test red):

`TestClosePeriodTx_CASOneWinner` (20 racers, truncation and close shapes,
cancel), `TestThresholdFire_vs_OperatorClose_CloseFirst_ResetTrue`,
`TestCloseVsThresholdFire_BothOrders_BillsUsageOnce` (barrier, 20 iterations,
both lock orders asserted), `TestThresholdFire_vs_Close_FireFirst_ResetTrue`,
`TestThresholdFire_vs_Close_ResetFalse_FireFirst` (the in-tx re-read),
`TestThresholdFire_vs_Close_ResetFalse_CloseFirst` (the verify-only CAS),
`TestImmediateCancel_vs_ThresholdFire_Concurrent` (the fire's CAS blocked
behind the cancel's lock, observed in `pg_stat_activity`),
`TestPlanSwap_StaleSnapshot_RejectedAfterClose` (with the fire-vs-swap and
residual-refusal subtests), `TestImmediateCancel_vs_BoundaryClose_NoDoubleBill`,
`TestCycleClose_InvoiceAndAdvance_ShareFate`,
`TestManualRunVsSchedulerRace_ExactlyOneInvoice` (repaired: lease token,
loser observed `ErrWatermarkMoved`, never the index),
`TestBillSubscription_PeriodMoved_RetriesOnce` (mocks), the doctor's seeded
double / residual / deferred-bucket rows. All run under the PgBouncer
transaction-mode job.
