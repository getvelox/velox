# ADR-062: Async obligation backbone — extend the outbox, not Temporal/River/pgx (build deferred)

**Date:** 2026-06-25
**Status:** Decided (design + build-vs-buy). **Build deferred** to a named trigger.

This ADR records a decision we are deliberately **not building yet**, so the
next session inherits the verdict instead of re-litigating Temporal vs River vs
a `database/sql`→pgx migration. ADR-061 forward-references it; this is that doc.

## Context

Velox runs **four bespoke re-drive sweeps** that each recover a post-commit
async side-effect that failed: tax-commit (#267 `RetryPendingTaxCommit`),
tax-reversal (#310 `RetryPendingTaxReversal`), clawback-issue (ADR-057
`RetryPendingClawbackIssue`), and CN-tax-reversal (PR2/ADR-061
`RetryPendingCreditNoteTaxReversal`). Each is the same machinery re-implemented:
a scheduler tick + leader gate + a `ListPendingX` scan + a `RetryX` loop + a
status/marker column + its own retry/aging semantics. That duplication grows
linearly with each new async effect.

The recognised industry consolidation is a **single durable, in-DB-transaction-
enqueued obligation queue** (verified 2026-06-25, deep-research wa8o61214:
River/Oban/Solid Queue are mainstream; the queue COMPLEMENTS — never replaces —
idempotency + reconciliation). The question: do we adopt a product (River /
Temporal), and do we migrate `database/sql`→pgx-native to unlock one?

Velox facts that decide it: `database/sql` over the `pgx/v5/stdlib` driver
(`*sql.DB`/`*sql.Tx`; the RLS tenant-isolation core is built on the `BeginTx`
wrapper — 60 non-test files / 385 query sites). We **already operate** a
transactional outbox (`webhook_outbox`, ADR-040): in-tx `Enqueue(ctx, tx, …)`,
`FOR UPDATE SKIP LOCKED` draining, DLQ (15 attempts / ~72h backoff),
advisory-lock leader. It is wired as a **customer-notification fan-out**
(`dispatcher → svc.Dispatch → matchesEvent` against `'*'`/prefix subscriptions).

## Decision

**When we consolidate, build a SEPARATE `obligations` table (B2) that reuses the
`webhook_outbox` DRAIN machinery** — in-tx `Enqueue`, `FOR UPDATE SKIP LOCKED`,
DLQ, advisory-lock leader, and the `billing.Reconciler` seam (#315) — with a
kind→handler registry that only ever calls **in-process** handlers reusing the
existing re-drive bodies. Effects enqueue an obligation IN their coordinator
`*sql.Tx`; the drainer dispatches by kind; the bespoke `ListPendingX` scan +
marker column are then deleted. Migrate the four re-drive sweeps one at a time —
the cheap, already-in-an-in-scope-tx effects (`clawback_issue`,
`cn_tax_reversal`) first; the post-commit `tax_commit`/`tax_reversal` LAST
(money-path surgery on the finalize/void paths where #267/#310/G1 already bit —
dual-run the bespoke sweep as a backstop during cutover, and don't inherit the
webhook DLQ's terminal-at-15 tuning for money kinds: a tax reversal must retry
until recovered, not dead-letter into a silent over-remit).

**Amended 2026-06-25: build shape flipped B1 → B2** (current-vs-queue decision
panel). The original lean was B1 — *generalise `webhook_outbox`* with an
internal/external discriminator. Rejected: `webhook_outbox` is a
customer-notification fan-out — `webhook.Service.Dispatch` writes a
customer/dashboard-visible event + an operator SSE frame, and `matchesEvent`
returns true on `'*'` for ANY event type — so an internal obligation sharing it
is one `Dispatch` call from leaking tax/refund payloads (`customer_id`, amounts,
intent ids) to customer endpoints, the worst bug class for a billing engine. A
discriminator would make that leak *impossible-by-correct-branch* on the hottest
customer-delivery path; **B2 makes it impossible-by-construction** (no `Dispatch`
reachability at all) for the cost of one table + one lock key. PR2/ADR-061
already refused `webhook_outbox` for this exact fan-out reason — B2 is consistent
with that precedent.

Decided **against**:

- **Temporal / DBOS (durable execution).** Its enqueue is an **RPC to a separate
  datastore**, so it is NOT in our Postgres transaction → reintroduces the exact
  dual-write the queue eliminates. It is also a stateful cluster (or SaaS that
  breaks the self-host wedge) solving a **bigger problem** — long-running,
  multi-step, stateful *workflows* — than we have (single-step idempotent jobs).
- **River (Go/Postgres) — for now.** *(Premise corrected 2026-08-30 — see the
  Amendment at the end; the decision stands on other grounds.)* Right shape, but its transactional
  `InsertTx` takes a **pgx-native `pgx.Tx`**; we are on `database/sql`, so its
  headline in-tx-enqueue property would not compose with our coordinator
  `*sql.Tx` without migrating the whole data layer. Buying the library and
  losing its one advantage is the worst of both.
- **`database/sql` → pgx-native migration, now.** Evaluated explicitly because it
  is the gate to River. We already use pgx as the *driver*; pgx-**native** would
  add binary-protocol perf, rich types, `LISTEN/NOTIFY`, and `COPY` — but our
  workload exercises **none** of them today (transactional billing CRUD at
  moderate QPS; money is `int64` cents; JSONB is already hand-marshalled). The
  one real future fit is **`COPY` for high-volume usage-event ingestion** (the
  AI-native metering wedge). "Do it now while the codebase is small" does not win
  because (a) the **risk is size-independent** — it touches the RLS security
  boundary and every money query, where subtle type/NULL/query-mode differences
  hide money bugs and RLS leaks; (b) **zero payoff today** at pre-launch; and
  (c) the migration is **incremental and path-local** — pgx-native and
  `database/sql` coexist in one binary (two pools, same Postgres), so the future
  cost is "add a pgx pool on the ingestion path," not a big-bang refactor. So the
  "it only gets bigger later" premise does not hold.

## Status: build deferred

The **design** is decided; the **build** is not scheduled. Reasons: at low/mid
scale (0 customers, 1 dev) the four bespoke sweeps are **correct** and the
consolidation is **mostly** a *maintainability* win — adopting any queue does not
make money more correct on 5 of 6 sweeps (exactly-once effects = at-least-once
delivery + idempotent consumers + reconciliation, which the sweeps already are).
Per `feedback_pre_launch_scoping` / `feedback_no_overengineering`, a generic
async backbone is a solution to a problem (many heterogeneous effects + an ops
team) we do not have yet.

**ONE correctness exception, named honestly** (current-vs-queue panel, verified
in `creditnote/postgres.go`): `RetryPendingCreditNoteTaxReversal` carries a
second, structural eligibility branch — an issued CN with no reversal stamped
against a tax-bearing source, bounded to 24h — that exists ONLY to catch the
compound failure where `ReverseTax` *and* the `tax_reversal_pending` marker write
both fail in the same `Issue()`, an otherwise-invisible **permanent silent tax
over-remit** whose 24h window can age the orphan out for good. Enqueue-in-tx
(B2) deletes that failure mode and its window outright, because the obligation
commits atomically with the state change — there is no "did the marker write
land" question. This is real money-correctness, not maintainability — but it is
true for exactly **one of the four** migratable sweeps (`tax_commit`/`tax_reversal`
derive from pure durable state with TTL-encoding windows no queue can delete;
`clawback_issue` is safe-by-construction), and it is a low-probability, isolatable
failure. So it does **not** justify the backbone today — and if that orphan ever
bites, the cheap hedge is to make `cn_tax_reversal` alone enqueue-in-tx, not to
build the whole queue. The deferral stands; the "zero correctness on the table"
framing does not.

**Triggers to build the obligation queue:** a durable async obligation that is
NOT webhook-notification-shaped AND for which a targeted reconciler is
insufficient (e.g. it needs multi-step ordering / cross-effect dependency a flat
sweep can't model); OR the `cn_tax_reversal` orphan above actually bites in
production, OR a *second* sweep grows its own structural-orphan + bounded-window
backstop (the duplication has become a correctness pattern); OR the count of
**obligation-shaped** effects outgrows "one drainer + a few bespoke sweeps"
(rule of thumb ~6–7 — count MIGRATABLE effects only: Stripe state-syncs like
`payment_unknown` and settlement/payout reconciliation never enqueue, so the
migratable set is **5 today** — re-counted 2026-07-05 during the deferral
reassessment: the plan-swap refund drafts (#381, Bug B transplant) joined
the clawback-issue family as a fifth issue_pending consumer of the same
reconciler; still under the ~6–7 line, and notably ADR-078 (commits)
added ZERO bespoke sweeps); OR a `cmd/velox-worker` process split; OR
a second engineer joins (copy-paste a 1-dev shop tolerates, a 2-dev shop
shouldn't).

**Triggers to revisit pgx-native (and then River):** usage-event ingestion
becomes a *measured* bottleneck → stand up a pgx-native pool on **just the
ingestion path**, prove the `COPY` gain against a target, expand only if
justified. *(Amended 2026-08-30: this trigger is about `COPY` on the ingestion
path only. River is NOT gated on pgx-native — see the Amendment below for its own
trigger.)*

## Consequences

- We keep four small, proven, individually-correct sweeps for now; the
  duplication is real but bounded and not yet painful.
- The CN-tax-reversal recovery (PR2) deliberately did **not** route onto
  `webhook_outbox` (the fan-out leak) and used a per-CN marker + structural
  sweep instead — that decision stands until this queue exists.
- ADR-061's "committed PR3" wording is clarified: the *design* is committed
  (this ADR), the *build* is trigger-gated.

## Amendment (2026-06-25): the sweep driver shipped as the thin log/metric envelope

A pre-build design panel (asked to design a uniform reconciler *framework* as the
foundation for this queue) reached a sharper conclusion: a generic
`Source[T]`/`Drive[T]` framework would be a foundation for the *rejected* (c) —
its "swap a scan Source for a queue Source" extension assumes a new obligation
table + per-effect Source seam, which is the new-table/River shape this ADR ruled
out. The decided (c) instead **enqueues** obligations onto the generalised
`webhook_outbox` and drains them with **one** kind-dispatching drainer, collapsing
the per-effect scans — so a per-effect Source seam would be discarded when (c)
lands. We therefore shipped only the **thin sweep driver**: a single-method
`billing.Reconciler{ Name(); Reconcile(ctx, batch) (int, []error) }` + a ~25-line
ordered driver (`runReconcilers` / `s.reconcilers()`) that owns nil-skip, uniform
logging, and ONE new per-reconciler metric (`velox_reconciler_sweeps_total{reconciler,mode,outcome}`
— previously only auto-charge was metered). The six recovery sweeps
(payment_unknown, tax_retry, tax_commit, tax_reversal, clawback_issue,
cn_tax_reversal) ride it; their re-drive bodies and eligibility SQL are unchanged.

This **is** the foundation-seam for (c), aimed correctly: when the four re-drive
sweeps migrate onto the obligation queue (one at a time — enqueue in the
coordinator tx, register a kind-dispatch handler reusing the same re-drive body,
delete the bespoke `ListPendingX`), the outbox drainer slots in as **one more
`Reconciler`** in the same ordered slice — driver, leader gate, mode fan-out,
logging, and metric reused verbatim, **zero rewrite**. `payment_unknown` (a
Stripe state-sync) rides the metric envelope but never migrates to (c). No
generics, no `Source/Drive` split, and deliberately **no `Mark` hook** — marking
stays the last line of each re-drive body, so the PR2 marker-gating anti-pattern
has nowhere to recur. Shipped as a behaviour-preserving, net-negative-LOC PR; no
migration, no new table.

## Related

- ADR-040 — the transactional outbox this would generalise (`webhook_outbox`).
- ADR-056 — coordinator-tx (the in-tx-enqueue primitive the outbox already uses).
- ADR-061 (#313) — atomic+recoverable `creditnote.Issue()`; names this ADR as the
  home for the build-vs-buy + pgx rationale.
- `reference_billing_reliability_industry_grounding` (memory) — the verified
  River/Oban/Solid-Queue/Temporal + sync-vs-async grounding.

## Amendment (2026-07-06): the one exception re-examined — deferral re-affirmed with sharper remedies

A design review of the CN tax-reversal orphan (the "one correctness
exception" above) sharpened three things; the deferral STANDS (user
decision, 2026-07-06), now with named remedies cheaper than the
enqueue-in-tx hedge originally recorded:

1. **The residual is recovery-bounded, not detection-bounded.** The
   structural sweep branch (b) finds the marker-less orphan, but a
   FAILED retry attempt writes nothing — so the 24h freshness window
   effectively requires the Stripe reversal to *succeed* (not merely be
   noticed) within 24h of issuance, or the orphan ages out into a
   permanent silent over-remit. Survival of a money obligation
   currently depends on an external system recovering inside an
   arbitrary window.
2. **Named first remedy — promote-on-detect** (supersedes enqueue-in-tx
   as the cheap hedge): make the retry loop's FIRST action on a
   branch-(b) row `SetTaxReversalPending(true)` — one local Postgres
   write, no Stripe dependency — promoting the orphan into the
   unbounded, indexed marker branch. The 24h bound then bounds only
   detection latency (one healthy sweep tick), the obligation itself
   never expires, and the scan stays cheap. Rider in the same change:
   a `pending_tax_reversals` gauge + runbook row (tier-2 escalation —
   today a permanently-stuck reversal is visible only as an hourly log
   line). Apply symmetrically to the invoice-side #310 sweep. ~1-2h.
3. **Two premises updated.** (a) The 24h bound's anti-burst rationale
   (pre-feature CNs re-reversed on first deploy) is stale for v0.1.0+
   installs — no pre-feature production database exists anywhere; the
   bound's remaining value is scan cost. (b) The marker branch's
   UNBOUNDED retry is re-affirmed as correct money-kind terminal
   semantics (obligations exit only by success or explicit human
   write-off), deliberately opposite to the webhook outbox's
   15-attempt DLQ — the per-kind distinction this ADR already records.

**Trigger:** production cutover (row added to the HA-readiness
work list) — or immediately if the orphan is ever observed. At pilot
scale the triple coincidence (two same-second write failures + >24h
recovery unavailability) stays negligible.

## Amendment 2026-08-30 — the River premise was false; the decision stands

The "River — for now" bullet above rejected River because its transactional
`InsertTx` "takes a pgx-native `pgx.Tx`". That has been false since River
v0.10.0 (2024-07-19): `riverdriver/riverdatabasesql` (`New(*sql.DB)`,
`UnwrapExecutor(*sql.Tx)`) lets `river.Client[*sql.Tx].InsertTx` compose with
the coordinator's `*sql.Tx` from `db.BeginTx(ctx, postgres.TxTenant, tenantID)`
directly — no data-layer migration. The premise was wrong for 23 months and is
repeated in ADR-061; both are corrected today.

What is actually true of River (verified against its source and changelog,
2026-08-30): the `database/sql` driver is poll-only — no LISTEN/NOTIFY — unless
`NewWithPgxListener` (v0.46.0) adds a dedicated pgx pool; River's tables carry
no `tenant_id` and sit outside the RLS fence exactly like `schema_migrations`;
v0.45.0+ requires Go ≥ 1.26 (satisfied by the 2026-08-30 toolchain bump); its
nightly `REINDEX INDEX CONCURRENTLY` runs under its leader and logs failures at
ERROR rather than silently; and `river_job` bloat under churn is generic to any
high-churn table — River ships a batched cleaner and a reindexer that this
repo's own outboxes do not.

Why the decision stands anyway:

1. **The billing path is correctly not job-shaped.** The entity is the queue:
   `subscriptions.next_billing_at` scanned with `FOR UPDATE SKIP LOCKED`. A job
   row per subscription per cycle would dual-write the due time against
   `next_billing_at` — the exact class the outbox exists to kill.
2. **The outboxes work and are tested against real Postgres.** Replacing them
   with a schema Velox does not own costs a data migration for in-flight rows
   and a rewrite of money-path code for no current gain.

**Trigger to adopt River for the outbox drainers specifically:** a design
partner's outbox volume makes the drainers a measured bottleneck, or a third
outbox-shaped table is needed (the obligation backbone above). Then River is the
first option evaluated, not the last — and it needs no pgx-native migration.

## Amendment 2026-08-30 — promote-on-detect SHIPPED (the 2026-07-06 hazard)

The 2026-07-06 amendment recorded that the credit-note tax-reversal sweep's
structural branch *detects* marker-less orphans but does not *promote*
them, so its 24h window bounded the obligation: a >24h provider outage
coinciding with the compound failure (reversal failed AND marker write
failed) silently over-remitted. Under the N ≥ 2 production posture that
compound failure is routine (a replica SIGKILLed between the two
post-commit writes), so the trigger fired. Shipped: on a failed re-drive,
`RetryPendingCreditNoteTaxReversal` sets `tax_reversal_pending=true` on a
marker-less row — one local Postgres write — moving it onto the unbounded
marker branch; the 24h window now bounds detection latency only. Also
closed: a provider returning *no* reversal transaction for a tax-bearing
invoice used to clear the marker and count as reversed (which would have
undone the promotion); it is now a failure. Refuted alternative: removing
the window (no terminal stamp exists on the CN side, so the structural
scan would churn every pre-feature CN forever). The invoice-side half was
already unbounded per ADR-111.

