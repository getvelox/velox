# ADR-101: Billing intervals — write-time item lifetimes replace read-time log interpretation

Date: 2026-07-27
Status: Accepted (owner decision; phased — see Rollout)

## Context

Cycle billing derives item lifetimes by **interpreting** the
`subscription_item_changes` fact log at read time (`itemBaseSegments`),
applying day-grade policy while reading. Every bug in the 2026-07
class was an interpretation disagreement between the fact store (raw
instants, trigger-written) and period policy (day-snapped boundaries):
the #615 first-period "prorated 30/31" regression, the day-1-add
cadence asymmetry, and the org-TZ clamp-miss edge (ADR-012 amendment,
accepted edges). Per the mechanize-on-third-drift rule the class hit
its threshold, and the owner directed the structural fix.

## Decision

A new `billing_intervals` table stores **policy-applied billable
ranges** — answers, not questions. Each writer records, in the same
transaction as its item mutation, the day-grade-decided
`[starts_at, ends_at)` for (subscription_item, plan, quantity). Cycle
close, cancel billing, and usage windows become a dumb
interval × period intersection. The fact log stays untouched forever:
**facts are never backdated by policy** (the trigger keeps writing raw
instants; audit truth and billing state are separate artifacts).

Semantics are **migration-preserving**: the shipped ADR-012-amendment
policy moves verbatim into the writers (an add on the period-start
calendar day opens at periodStart ITSELF — never a day-floor, so
threshold-reset/ADR-091 mid-day period starts stay safe; every other
op is instant-precise). Uniform day-grade everywhere (killing the
mid-period noon cliff) is a **post-cutover policy option**, sequenced
after Phase 3 by necessity: Phase 2 parity requires preserving today's
semantics, and uniform day-grade would diverge by design.

### Schema (panel-hardened)

```sql
billing_intervals(
  id, tenant_id, livemode,
  subscription_id  REFERENCES subscriptions ON DELETE CASCADE, -- ADR-086 teardown carve-out from "no deletes"
  subscription_item_id, plan_id, quantity,
  starts_at timestamptz NOT NULL,
  ends_at   timestamptz NULL,          -- NULL = open
  source text, created_at timestamptz,
  CHECK (ends_at IS NULL OR ends_at >= starts_at),
  UNIQUE (subscription_item_id) WHERE ends_at IS NULL,          -- at most one open interval per item
  EXCLUDE USING gist (subscription_item_id WITH =,
    tstzrange(starts_at, COALESCE(ends_at,'infinity')) WITH &&) -- overlap unrepresentable
)
```

The constraints are the point: writer bugs in this table are silent
money, so the DB makes the two fatal shapes — overlap and
negative/duplicate-open — **loud transaction failures**, not data.

### Writer: ONE maintainer at the store layer

All item mutations funnel through exactly **6 SQL chokepoints** in
`internal/subscription/postgres.go` (sub-create item loop, addItemInTx,
updateItemQuantityInTx, applyItemPlanImmediatelyInTx,
ApplyDuePendingItemPlansAtomic, removeItemInTx). One
`applyIntervalChange(ctx, tx, item, op, at)` maintainer — store-level,
because the non-Tx variants own their transactions internally and a
service-level writer cannot join them — owns the close-then-open
pairing and the add-clamp policy. Interval writes derive strictly from
the mutation's RETURNING rows (actuals, never caller intent),
inheriting existing idempotency. Close checks RowsAffected==1 and
fails LOUD on 0. Draft/trialing items open no interval at insert; the
activation/conversion writers (draft Activate, the four trial-conversion
paths — which open at the sub's current period start as of the flip)
own those opens. Retroactive applies (engine catch-up firing scheduled
changes at past instants) must SPLICE: close the interval containing
the effective instant, or fail loud if the open interval starts after
it — never produce overlap (the EXCLUDE constraint enforces this).

Non-Go writers closed off: the 0129 trigger's un-delete 'add' branch
becomes RAISE EXCEPTION until a real un-delete flow (with an interval
writer) exists; test-clock teardown cascades via the FK and joins
clockTeardownStatements (the teardown arch test forces it).

### Reader invariant (the enforcement keystone)

A live, non-deleted item with **no interval overlapping the period
fails the subscription's cycle loudly** — never bills zero silently
(inverting today's defensive full-period fallback, per
no-silent-fallbacks). Symmetric: an open interval for a soft-deleted
item is an integrity error. This invariant is what makes the
trigger→Go-writer migration survivable after the interpretation layer
is deleted.

### Backfill

Boot-time idempotent Go one-shot (not SQL — it reuses
`itemBaseSegments` verbatim, tenant-TZ clamp included), with
`subscription_items` as ground truth (created_at→open,
deleted_at→close) and the log only for interior boundaries — because
the log is a known-incomplete lifetime source (pre-0029 items have no
'add'; the 0102→0129 window wrote no 'remove'). The window-relative
clamp is applied only to adds inside the current open period, against
CurrentBillingPeriodStart; historical precision is immaterial (only
intervals intersecting the current period forward are ever billed).

## Rollout

1. **Dual-write** (unconditional — writers are NEVER gated by the
   flag; that is what keeps every later state reversible).
   **SHIPPED 2026-07-27** (migration 0159 + store writers + boot-time
   state-grounded backfill). The 0129 trigger's un-delete branch now
   RAISEs — resurrection has no interval writer, so the write is
   refused rather than left to silently un-bill after cutover.
2. **Shadow parity**: cycle close computes both ways **in one
   repeatable-read snapshot** (two-tx shadow reads produce false
   divergence), compares line-item multisets, WARN+metric; CI
   hard-fails on the walked fixture corpus (B20/B21, I-series
   NIM-000248/249/253/257/258, the day-clamp matrix). The comparator
   carries a **known-divergence allowlist** where the interval side is
   MORE correct by design (the org-TZ clamp-miss class — write-time
   freezing fixes ADR-012's second accepted edge; parity asserting
   equality there would train WARN-blindness).
   **SHIPPED 2026-07-27.** Implementation notes vs this spec: the
   snapshot is ONE SQL statement (UNION of window-scoped fact rows +
   the item's full interval history) — a single statement is a single
   MVCC snapshot even under read committed, so no isolation-level
   surgery on the money path; the comparator diffs NORMALIZED SEGMENT
   multisets rather than line items (segments determine lines
   deterministically given plans/period/TZ, and merging equal-adjacent
   slices makes the comparison equivalence-exact). The allowlist grew
   two classes found by sweeping the real dev database — pre-0102
   hard-delete residue (legacy bills a phantom for a row that no
   longer exists) and 0102→0129 remove-gap residue (legacy misses a
   sealed stub) — both shapes where the interval side is authoritative,
   plus the registered catch-up-lifetime class. Same-instant fact rows
   now tie-break by wall created_at in the legacy reader (root-cause
   fix, not allowlisted). Direct hard-DELETE of subscription_items is
   refused by trigger (0160), closing the writer bypass the residue
   class revealed. Sweep evidence: 140/140 active subs, 2 allowlisted,
   0 unexplained.
3. **Cutover**: `VELOX_BILLING_INTERVALS_READER=off|shadow|on`,
   global env, read ONCE at boot (no half-legacy invoices); clamp +
   interpretation stay dormant one release.
   **Machinery SHIPPED 2026-07-27** (mode plumbed through the one
   segment-source seam; `on` bills from intervals with a loud
   missing-interval invariant for live items; corpus CI runs every
   shape in BOTH modes and asserts line-for-line identical invoices).
   **CUTOVER EXECUTED 2026-07-27, same day**: default flipped to `on`
   after a live test-clock walk under the interval reader (TZ-seam
   period, mid-period quantity change, exact 9/31 + 22/31 day
   segments, correct tax, zero divergence logged). `shadow`/`off`
   remain the kill switches; the dormant interpretation keeps running
   inside the comparator on every close.
4. **Remove** interpretation; the flag dies in the same PR (a lying
   'off' value violates the doc-doesn't-lie bar).
   **EXECUTED 2026-07-28** (trigger amended by the owner: pre-launch,
   local-only, the soak evidence was already in — zero unexplained
   divergences across the 140/140 cutover sweep, the two-mode corpus on
   every PR since #635, and every dev cycle close in between; and the
   dual-reader tax was real, since every billing change had to keep two
   readers in agreement). The legacy fact-log interpretation
   (`itemBaseSegments`), the shadow comparator + allowlist classifier,
   the one-statement UNION snapshot, and the
   `VELOX_BILLING_INTERVALS_READER` modes are deleted; the reader is a
   dumb interval×window intersection over `ListItemIntervals`, keeping
   the loud missing-interval invariant. A set env var now logs a boot
   WARN naming the removal (never silently ignored). The corpus test
   pins GOLDEN line fingerprints captured under the two-mode gate; the
   TZ-clamp and catch-up allowlist tests became truth-side behavioral
   assertions (a later TZ change cannot re-interpret a write-time open;
   an item added after the window never bills it). Deliberate
   consequence accepted while pre-launch: there is no revert-to-legacy
   kill switch — a writer bug is fixed forward, guarded by the DB
   exclusion constraints, the golden corpus, and the invariant.

## Consequences

- The #615 class and the org-TZ clamp-miss become structurally
  impossible; the same-day cancel→recreate boundary-day edge carries
  over unchanged (pigeonhole — one day, two owners; a follow-up may
  enforce non-overlap per (customer, meter) with a second EXCLUDE
  constraint, turning the edge into an explicit written rule).
- Write-time day-grade IS a partial billing/display TZ decouple —
  ADR-012/ADR-092 amended accordingly.
- Known latent bug in the CURRENT reader (found by this design's
  adversarial panel, register-tracked): engine-down catch-up across a
  boundary with an outage-window qty change + a scheduled swap due at
  the boundary bills the whole period at the old plan (boundary fact
  row excluded by the exclusive-left window; stale from_plan_id).
  Phase 2 parity cannot adjudicate it (both sides wrong differently);
  the interval writer's splice semantics fix it at cutover.
