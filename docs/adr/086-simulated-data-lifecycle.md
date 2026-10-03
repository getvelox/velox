# ADR-086: Simulated-data lifecycle — durable `is_simulated` sweep-gates + clock-delete teardown

**Status:** Accepted 2026-07-09; **revised 2026-07-09** after the design-B pivot — this file replaces the original "immutable `sim_clock_id` discriminator on every sim-bearing table" plan (see *Alternatives considered — why not a durable discriminator on every table*).
**Supersedes:** ADR-016 (test-clock soft-delete + cancel-subs + detach-customers → **complete hard-delete teardown**).
**Extends:** ADR-029 (fully-disjoint test-clock flows).
**Reaffirms:** ADR-030 (simulated time on clock-pinned entities stays; `created_at` is domain time) **and its audit exception** (the audit log is always wall-clock).

## Summary

In test mode, jobs that run on real time were charging and dunning (retrying failed payments for) simulated test-clock invoices. The auto-charge, dunning and tax sweeps now skip invoices with `is_simulated = true`, a flag stamped once at write and never changed. Other real-time jobs (credit expiry, usage aggregation, some analytics counts) are not yet gated while a clock is alive. They are deferred because they cannot move live money. Deleting a test clock hard-deletes the clock, its customers and every row they own in one transaction, so no simulated billing row is left for any job to act on. The audit log is kept on purpose as the simulation's only surviving record. Its `test_clock_id` and `sim_effective_at` are filterable columns (2026-07-13 amendment). Schema-walking tests fail CI when a new customer-owned table is neither torn down nor allowlisted with a written reason, or when the teardown would leave a foreign key pointing at a deleted row. Frontend relative-time helpers require an explicit "now" anchor, so a clock-pinned entity is measured against the clock's frozen time.

## Context

In **test mode** (`livemode=false`) two time domains share one dataset: **wall-clock** (real-test data + every cron sweep on `time.Now()`) and **simulated** (test-clock-pinned entities whose billing timestamps are stamped at the clock's `frozen_time`). An adversarial audit found **24 defects** where a wall-clock plane (auto-charge, dunning, tax, credit-expiry, usage-billing, analytics, ~8 FE surfaces) processes a *simulated* record against wall-clock time. All are test-mode-only (no live-money impact), but they violate ADR-029, corrupt demos/QA, and lie in the UI.

The 24 split into **two independent problems**:

1. **Live-clock leak.** While a clock is alive, a simulated invoice stamped at `frozen_time` can look "due"/"overdue" to a wall-clock sweep *right now* and get auto-charged or dunned — even though nothing was deleted. ADR-029 requires wall-clock sweeps to skip simulated rows.
2. **Deleted-clock leak.** The old model (ADR-016) *soft*-deleted the clock and detached its customers (`test_clock_id → NULL`), leaving the simulated rows behind with their pin nulled. The "skip me, I'm simulated" pin vanished and every wall-clock plane grabbed the orphans. The pin-join also missed customer-pinned **one-off** invoices (no subscription to join through).

## Decision

Two targeted mechanisms, one per problem — **no new per-row discriminator, no write-chokepoint** (see *Alternatives considered*).

### 1. Live-clock half — money sweeps gate on the durable `is_simulated` (shipped #417/#420)

Every wall-clock money sweep is **invoice-anchored** (auto-charge `ListAutoChargePending`; dunning `ListDueRuns` / `ListFailedWithoutDunningRun`; tax `ListPendingTaxRetry` / `ListPendingTaxCommit`). `invoices` already carries a durable `is_simulated` flag (migration 0109, stamped once at write from `customerOnTestClock`); `credit_notes` too (0117). The sweeps now filter `AND i.is_simulated = false` instead of the old mutable pin-join. Because `is_simulated` is stamped at creation and never mutates, detaching or deleting the clock can't flip an existing invoice back into a wall-clock sweep. This closes the money-critical live-clock leaks with **zero schema change** — the flag was already there.

### 2. Deleted-clock half — clock delete = **complete teardown** (this ADR's change)

Deleting a test clock **hard-deletes the clock and tears down its entire simulated customer graph** in one `TxTenant` transaction (`internal/testclock.Delete`): every customer pinned to the clock (`customers.test_clock_id = $clock`) and every row those customers own — invoices with their line-item / dunning / tax children, credit notes, subscriptions (+ items / changes / thresholds via CASCADE), usage events, the credit ledger, per-customer config, and portal rows — deleted **bottom-up** so migration-0015's `RESTRICT` guards never trip, and the customers **last** (before the clock) so `customers.test_clock_id`'s `ON DELETE SET NULL` fires on nothing. After it runs **no simulated row survives**, so *no* wall-clock plane — money or not, invoice-anchored or not — can ever act on a stranded simulated row. The deleted-clock class is dissolved wholesale, not sweep-by-sweep.

This is the Stripe test-clock model: the customer is born into the sandbox and is discarded with it. History is not lost — the clock deletion is recorded in the audit log (wall-clock, ADR-030 exception). The old soft-delete + cancel-subs + detach dance (ADR-016) is gone, and with it the `test_clocks.deleted_at` column, its partial index, and the 8 now-vestigial `deleted_at IS NULL` read filters (migration 0144). Phantom-MRR from `canceled_at = NULL` subs dissolves because the subs are *deleted*, not canceled.

### 3. Anti-regression — a mechanized **completeness** guard (two layers)

Teardown is only safe if it is *complete*: a simulated table left out of the delete set silently re-opens the deleted-clock leak. Two arch-tests make completeness un-forgettable, discovering the live schema from `information_schema` / `pg_constraint` (no hand-maintained list):

- **Layer A — customer-scope completeness** (`TestTeardownCoversEverySimulatedTable`). Every table with a `customer_id` column must be either in the teardown delete set or in an explicit `teardownKeepAllowlist` with a written reason. A new customer-owned table added later fails CI until it is classified. This is the guard that caught the two real gaps found while building: `customer_discounts` missing from the delete set, and `customer_payment_setups` (a table that had been dropped from the schema).
- **Layer B — FK-closure integrity** (`TestTeardownLeavesNoDanglingReference`). For every foreign key whose *parent* rows the teardown deletes, the *child* rows must be deleted too — otherwise the teardown aborts (RESTRICT / NO ACTION child) or leaks a nulled reference (SET NULL — the exact original bug shape, `customers.test_clock_id`). CASCADE children are folded into the deleted set first; any non-cascade FK into that closure whose child sits outside it fails, with no seed data required.

A real-Postgres integration test (`TestDelete_TearsDownSimulatedGraph_KeepsEverythingElse`) exercises the actual ordered DELETEs and the pinned-vs-unpinned survivor discrimination; a mutation-verify confirms Layer B bites when a table is dropped from the set.

### 4. FE relative-time surfaces — a required-anchor helper (this change)

The **~8 frontend surfaces** in the audit (Context) render values *relative to "now"* — "X ago", "in N days", cycle %, rolling usage windows, due/expiry badges. On a clock-pinned entity "now" is the clock's `frozen_time`, not wall-clock; the earlier #410/#411/#413 sweep fixed the *instances* by threading an optional `nowISO`, but every helper still ended in `?? Date.now()` — a **forgettable** fallback (a new surface that omits the anchor silently reverts to wall-clock, wrong only once a clock is advanced). That residual is the same shape as the sweep leaks: correctness by discipline, not by construction.

This change converts the discipline into a **mechanized gate** (`web-v2/src/lib/effectiveNow.ts`):

- A **branded `EffectiveNow`** type. The only ways to make one are `effectiveNow(frozenISO)` (entity surfaces — `frozen_time` when pinned, wall time when not) and `wallClockNow()` (an *explicit, greppable* wall-clock opt-in for the forensic/egress layer — audit log, email/webhook outbox, reaffirming ADR-030 — and never-pinned entities like API keys / webhook endpoints). Every relative-time helper **requires** an `EffectiveNow`, with **no `?? Date.now()` anywhere** — so "did you handle the test clock?" becomes a **compile error**, not a runtime bug. This is the stronger, enumerable guarantee, the same principle the teardown chose over the discriminator.
- A **second, independent** gate: an ESLint `no-restricted-syntax` rule bans raw `Date.now()` / argless `new Date()` in app code, so a *hand-rolled* relative-time computation can't bypass the helpers. Genuine calendar/infra uses opt out with a one-line reason; the date-infra modules (`lib/dates.ts`, the date-picker) are exempted.
- The gate immediately caught **two real latent bugs the instance-sweep missed** — both *input validation*, not display: a clock-pinned customer's **credit-expiry** date validation (the CustomerDetail invoice composer and the Credits grant dialog) checked "must be a future date" against wall-clock, so a customer frozen in 2027 would wrongly accept a 2026 (sim-past) expiry. Both now anchor on the customer's effective-now.

The helper sits over the existing client-side frozen-time resolution (`useClockFrozenMap`); if the backend later ships `effective_now` per row (the `InvoiceDunningRun.effective_now` pattern is the template), `effectiveNow()` takes that value with **no call-site change**. A pure-logic drift-guard (`web-v2/tests/effectiveNow.test.ts`, `npm test`, no new runner) proves each helper measures against the anchor — a frozen-2027 assertion a leaked `Date.now()` would fail.

## Alternatives considered — why not a durable discriminator on every table

The first design (a full day of panels) proposed the *comprehensive* fix: add an immutable `sim_clock_id` birth-stamp to **every** sim-bearing table, derive `is_simulated` as a `GENERATED` column from it, funnel all sim-capable writes through a `WithSimulation(clockID)` chokepoint, `ON DELETE CASCADE` the stamp, and add a sweep-filter arch-test asserting every sweep carries an `is_simulated` predicate. It would have worked. It was rejected because it is **containment machinery for a problem teardown deletes outright**:

- It adds a column + generated column + `CHECK` + partial index to ~7 tables, a write-chokepoint that every creation door must route through (including the LiteLLM ingest door — the #406 lint-scope lesson), and a sweep arch-test whose core predicate — *"is this `SELECT` a wall-clock sweep?"* — **isn't decidable**, so it ships as an allowlist-backed gate, not an airtight one. That is the complexity-accretion smell: each guard spawns the next.
- Its entire payoff over teardown is *keeping* simulated rows readable after a clock is deleted. At **zero customers, pre-launch**, discarding a deleted sandbox's data is a non-cost — it is exactly what Stripe test clocks do.
- Teardown's completeness is **enumerable** (Layer A/B walk the schema and fail closed); the discriminator's sweep-safety is **not** (the undecidable predicate). The smaller design carries the *stronger* guarantee.

The live-clock half still needs a durable flag — but only on the **money sweeps**, which are all invoice-anchored, and `invoices.is_simulated` already exists. So the discriminator's one genuinely-needed piece was already in the schema; the rest was apparatus to avoid deleting test data.

## Known limitations & deferred (each with a trigger)

- **Non-money live-clock gates** — credit-expiry (`ListExpiredGrants`), usage-aggregation, and analytics counts over non-invoice tables are **deferred**, not built via a discriminator: they are test-mode-only, carry no live-money impact, and are narrow (the wall-clock billing scheduler already excludes clock-pinned subs, ADR-028). Trigger: an operator hits one in a real simulation.
- **No `created_at` time-split** — `created_at` / `updated_at` stay simulated (`clock.Now`) on clock-pinned entities, reaffirming ADR-030; the wall-clock forensic layer is the audit log (`sim_effective_at` metadata carries the simulated instant). Trigger: a compliance/debugging need beyond the audit log.
- **Pricing `GetRuleByKeyAsOf`** compares a rule's wall-clock `created_at` against a sim `asOf` (`pricing/postgres.go`). Correct for forward-advanced clocks and single-version rules (≈all usage) and it never errors (fallback to the earliest active version). Trigger: an operator hits it with a behind-anchored clock on a multi-version rule.
- **Post-teardown forensic grouping** — once the rows are gone, "which clock produced this" is unanswerable; acceptable by design. Trigger: a named audit need → capture a summary audit row at teardown before the delete.

## References

- ADR-016 (superseded), ADR-029 (extended), ADR-030 (reaffirmed, incl. audit exception), ADR-027 (customer-level pin at creation), ADR-028 (disjoint wall-clock / catchup billing planes).
- The 24-finding clock-delete-detach audit and the four-candidate design panel (2026-07-08/09); the Design-A → Design-B pivot (2026-07-09).
- Implemented by migration 0144 + `internal/testclock` (teardown + Layer A/B completeness arch-tests + real-Postgres integration test) and the invoice/dunning/tax `is_simulated` sweep gates (#417/#420).
- FE relative-time required-anchor helper: `web-v2/src/lib/effectiveNow.ts` (branded `EffectiveNow` + `effectiveNow`/`wallClockNow`), the `useEffectiveNow` / `useEffectiveNowResolver` hooks, the `no-restricted-syntax` lint gate (`web-v2/eslint.config.js`), and the drift-guard (`web-v2/tests/effectiveNow.test.ts`); builds on the #410/#411/#413 relative-time audit.

## Amendment 2026-07-13 (ADR-090 arc): sim-axis columns promoted

The "post-teardown forensic grouping" deferral above named its trigger — "a
named audit need" — and the 2026-07-13 audit-subsystem e2e audit met it:
after teardown the audit log is a simulation's ONLY surviving record, yet
`sim_effective_at`/`test_clock_id` lived solely inside metadata JSON —
unfilterable, unsortable, unindexed — and a clock advance collapsed months
of simulated events into one wall-clock instant for every date filter and
group header.

Migration 0148 promotes both to nullable `audit_log` columns with a partial
index on the clock slice.

**SHIPPED (ADR-090 §5) — this amendment's plan changed in two ways as it was
built, and the doc is corrected rather than left describing a design that does
not exist:**

1. There is no `SimContext` emission field. A per-emission field means ~90
   emitters each remembering to populate it, and an emitter that forgets is
   invisible — its rows silently drop out of `?test_clock_id=`. The axis is
   instead DERIVED from the ctx clock binding (`clock.Sim`, one value from one
   read), so no emitter can forget and none may hand-stamp it.
2. There is no ordering by simulated time. `sim_effective_at` is the instant the
   clock STOOD AT when a mutation was performed, and an advance performs
   everything it settles at one instant — so it separates advances, not the
   periods inside one. Ordering by it would change nothing within a clock and
   would interleave unrelated simulations across clocks.

Surfaced as `?test_clock_id=` / `?sim_from=` / `?sim_to=`, a clock filter on
`/audit-log` sourced from the AUDIT ROWS (not `test_clocks` — that table is
empty exactly when the forensic view is wanted), and sim columns in the CSV
export. Wall-clock `created_at` primacy (ADR-030) is unchanged.
