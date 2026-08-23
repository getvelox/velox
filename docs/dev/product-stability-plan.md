# Product stability plan — fewest live defects, provably

**Aim:** product stability — the fewest issues/gaps/bugs in the product,
maintained as a *standing condition*, not a one-time push.

This is the third leg of the discipline set:
[money-path-robustness-playbook](money-path-robustness-playbook.md) (build
side), [manual-test-strategy](manual-test-strategy.md) (proof side), and this
document (maintenance side — where residual defects live and what finds each
class cheapest).

## What "stable" means here, measurably

1. **Zero open invariant violations** in any environment's data (see the
   doctor sweep below — this is a queryable number, not a feeling).
2. **Zero known-but-unfixed money traps.** A recorded trap (e.g. the
   Jan 31 → Feb 28 date-math clamp) is a scheduled fix, not lore.
3. **Dependabot: nothing open** except items on the deferred-with-trigger
   list (currently: react-router 8, orval-next).
4. **Runbook truth-rate ≈ 100%** on sampled re-walks. The runbook is
   `MANUAL_TEST.md`, the ledger of walked flows; a re-walk re-proves each of a
   flow's checkboxes against current behavior. A checked box means *proven at walk
   time*; the sample measures whether that still predicts current behavior.
5. **Every deferral carries a trigger**, in the ADR or the register in
   `velox-ops` (the private planning repo). An open gap without a trigger is
   a bug in this plan.

## Why this allocation — the evidence

How the last stretch of REAL defects were actually found (2026-07/08):

| defect | found by |
|---|---|
| 4 money bugs (B-series) | walking fresh flows |
| $46.69 stranded tax reversals | **a DB query** for invariant violations |
| claim-to-charge race (#748) | **adversarial review of a fresh diff** |
| unsatisfiable relief gate (#740) | reading a false comment against code |
| dead gate behind an open PR | **an is-it-actually-merged audit** |
| 3 closed doors around recovery (#747) | walking fresh flows |
| gross-charge bug (B1) | walking + service-level (not store-level) test |

Pattern: **walks find defects in fresh code; queries and diff-review find
them in stable code.** The runbook is at 991/997 with every survivor
deferred-with-reason, so walking stable code is past diminishing returns.
The residual population lives in: latent data-invariant violations, races in
claim-to-read gaps (between a worker claiming a row and re-reading it before
acting), long-horizon time math, and — dominantly — **new code as
it lands**.

## Pillar A — Intake: keep new bugs out *(exists — do not weaken)*

New code is the largest defect source. The standing gates:

- **Money-path playbook** on any money/state-machine change: complete
  site-set enumeration (every writer, effect-firer, gated reader, caller/callee,
  and crash point of the state) before writing; the 12 gates; per-class
  review lens.
- **Adversarial verification on money diffs**: independent read-only finders
  (defect-hunting reviewer agents) over the finished diff (leftovers /
  regressions / doc-truth). This caught the #748 race after a clean local
  pass — treat it as mandatory for money-path PRs, cheap optional elsewhere.
  Findings are verified by the author, not a refuter panel (agents assigned
  to knock findings down — measured useless here). Cap at ~5 finders.
- **Same-PR doc rule**: CHANGELOG + the matching MANUAL_TEST flow + ADR (+
  ADR index row) + README ship with the change. A doc that lies is a defect.
- **Pre-push gate**: gofmt · go vet · `go test ./... -short` ·
  golangci-lint (its `unused` finding = deleted-test alarm) · `make gen`
  (BOTH halves — `npm run gen` alone lets the Go types drift) · `tsc -b`
  (never `--noEmit`, it is a no-op here) · FE tests.
- **Belt gates narrow WITH claims** (the #748 lesson). Belt gates are the
  backstop checks downstream of a claim. Any status-universe change (a
  change to which statuses a path treats as eligible) must sweep claim SQL,
  service pre-reads, *and* the terminal provider-call gate in the same PR —
  the claims read before the handler re-reads, so the last gate is
  load-bearing, not redundant.

## Pillar B — Detection: find latent bugs *(the build list)*

### B1. `velox doctor` — the money-invariant sweep *(SHIPPED — CI-wired, #753/#756)*

One command that interrogates any Velox database and reports invariant
violations. Converts the class that found the $46.69 stranding from
"noticed during an ADR" into "caught by CI, forever." First invariant set:

- per-invoice conservation: `amount_paid + amount_due + credited = total`
- status ↔ timestamp coherence: `paid ⇒ paid_at`, `uncollectible ⇒
  uncollectible_at`, `voided ⇒ voided_at`; no `tax_reversed_at` on a
  status that should not carry one
- credit ledger: grants − drawdowns = balances; no negative blocks;
  event-sourced sum matches materialized balance
- no expired-but-unreleased charge leases; no outbox rows (queued outbound
  deliveries) pending past DLQ (dead-letter) age; no dunning run `active`
  on a terminal invoice
- credit notes: `issue_pending=false ⇔ status transitioned`; no
  issued-but-unapplied against live invoices
- parked coherence: `payment_status='unknown' ⇒` no auto_charge_pending

Wire-up: run after the integration suite in CI (fails the build on any
violation) and runnable ad hoc against dev/walk DBs. When a prod deploy
exists, it becomes the ops health check. Every future bug class gets its
invariant added here in the fixing PR — that is the mechanized half of
"enforce invariant after a bug class."

### B2. Long-horizon clock soak *(SHIPPED 2026-08-06 — CI-locked)*

`TestE2E_ClockSoak_MonthEndAnchorThroughYearBoundary` (internal/api): a
day-31-anchored subscription driven through 13 REAL cycle closes across a
year boundary and two Februaries, via the actual server + catchup
orchestrator (the worker that closes billing cycles a clock advance jumped
past), with a doctor sweep after every advance. Runs in every integration
CI pass — stronger than the per-release cadence originally planned.

The "known Jan 31 → Feb 28 clamp trap" this section was going to fix first
turned out to be a STALE memory. ADR-055 (`advanceAnchored` + migration
0120) had already shipped the clamp, with unit pins (unit tests pinning the
behavior) in anniversary_month_end_test.go. Verified 2026-08-06 by
enumerating every remaining `addIntervalIn` caller; the record was corrected
rather than the fixed code re-fixed.

### B3. Runbook truth-rate sample *(DONE 2026-08-06 — result: 25/25 TRUE)*

A deterministic hash-ordered sample (no cherry-picking): 17 money-path + 8
general boxes. Each was verified adversarially against current code by an
independent reader required to LOCATE the producing code, with verdicts
anchored to current file:line. Moved citations and renamed identifiers
explicitly did not count as drift — only behavioral mismatch did.
**Truth rate: 100%.**
Consequence, per this section's own rule: the ledger is trustworthy and
full re-walks are permanently off the table. Re-measure only if the same-PR
revision rule is ever suspended.

### B4. Staleness flagger *(REFUTED 2026-08-06 — do not build)*

Evidence-gated and it failed the gate. The zero-maintenance design (derive
watched paths from file citations inside each flow's own annotations) covers
**3 of 129 flows** — 97 flows cite no machine-parseable paths. Its one
flag was an already-registered known item (X14). Useful coverage would
require a hand-maintained flow→source map, a rot-prone surface bought for
~2% signal. The actual staleness defense is the same-PR revision rule, which
B3 just measured working at 100%. Do not rebuild without new evidence that
the same-PR rule is failing.

### B5. Fresh-tenant smoke ritual *(no build — a rule)*

S1/S2 (the MANUAL_TEST.md smoke flows; ~15 min) on a **fresh tenant**
before any DP-facing (design-partner-facing) milestone.
Fresh-tenant walks found 3 seam bugs that aged-tenant walks structurally
could not.

## Pillar C — Known-items burn-down *(scheduled, not aspirational)*

| item | state | action |
|---|---|---|
| fast-uri HIGH + audit-fixable batch | **CLOSED** — #750 merged 2026-08-06 | none |
| Jan 31 → Feb 28 date-math clamp | **CLOSED — the trap was already fixed when this row was written (2026-08-06 correction)** | none. ADR-055's `advanceAnchored` (internal/domain/subscription.go) ships the clamp-and-restore; `internal/domain/anniversary_month_end_test.go` pins day-29/30/31 anchors incl. the leap-year case; B2's soak (#754) then drove a day-31 anchor through **two non-leap Februaries and a year boundary** against the real server, doctor-clean after all 13 closes. The row was carried over from a stale note, not re-derived from code — the same born-false class this plan exists to catch, found in the plan itself. |
| ~41 LOWs backlog (2026-07-02 audit) | **TRIAGED 2026-08-06** | 104 items adjudicated (LOWs + every register/ADR deferral). 28 FIXED_ALREADY, 3 GONE_SURFACE, 69 ACCEPT-with-reason, **4 PROMOTED and shipped**. Backlog closed — see below. |
| cost-dashboard public-token hashing | **CLOSED 2026-08-06** | done, not scheduled. Migration 0172 stores only a SHA-256 blind index; the backfill was walked as a real 171→172 upgrade, so links minted before it still resolve. Doing it pre-DP was the cheap moment — after a design partner it would have meant a dual-read window or invalidating live links. Guarded by `TestCostDashboardToken_HashLookupAndNoPlaintextAtRest` (mutation-verified) and doctor check `cost_dashboard_token_plaintext_at_rest`. |
| engine post-commit audit rows (6) | open, CI-gated | with the next engine arc |
| react-router 8, orval-next | deferred WITH trigger | leave; revisit on trigger |
| breached-pw + TLS (pre-deploy), MFA (pre-DP) | gates recorded (#507) | leave; they gate deploy, not today |
| HA N=2 hazards | trigger = prod cutover | leave |

### The 2026-08-06 triage result

Every one of the ~41 remaining LOWs plus every deferral in the velox-ops
register and in ADR-110…113 was adjudicated against current code — 104
items. The distribution is the useful finding: **28 had already been fixed**
by unrelated work and **3 lived in code that no longer exists**. In other
words, a third of a five-week-old backlog decays on its own. 69 are ACCEPT-with-
reason (cosmetic, or a deferral whose named trigger has not fired — no DP,
no production, no traffic).

Four promoted, all shipped in the triage PR:

1. **Password-reset siblings survived a reset** (security, real): redeeming
   one token left the user's other live tokens redeemable for the rest of
   their hour. A holder of an earlier token could therefore re-flip the
   password of the account that authorizes charges. Now voided in the same tx;
   mutation-verified both ways (remove the void → siblings redeemable;
   over-broaden it → the bystander control fails).
2. **Invoice PDF download/preview lacked `res.ok`** — an error body saved as
   `<invoice_number>.pdf`. The credit-note twin always had the guard.
3. **Stale deferred-CN-draft alarm** (ADR-059) and
4. **tax-calculated-never-committed sensor** — two register items whose
   named vehicles (the arcs designated to carry them) had already passed
   them by twice. Both were one doctor check each; the doctor was the
   vehicle they were waiting for.

Two register items were struck as **stale premises**, not deferrals:
ADR-110's "restart dunning on recovery" and "tax re-commit on recovery" —
both describe a charge-in-place path ADR-113 deleted.

## Pillar D — Rot prevention *(exists — keep enforced)*

- Runbook: stale flow = rewrite or delete, never leave-and-document; the
  same-PR revision rule is what kept D6 (a MANUAL_TEST flow) true through
  three redesigns in one week.
- ADR index row lands in the same PR as the ADR (it silently rotted for a
  month at 28 missing entries).
- Deferrals: every defer-with-trigger written to the velox-ops register in
  the same session.
- Superseded prose gets a banner IN the superseded doc — a new ADR pointing
  backwards is not enough (ADR-036/110/111 all needed forward notes).

## What we deliberately do NOT do

- **No full runbook re-walk.** Weeks of effort re-proving what CI pins;
  the sample (B3) + flagger (B4) buy the same confidence for ~2 days.
- **No speculative hardening** (pre-launch scoping rule): enterprise-shaped
  work waits for named pressure; refuted alternatives stay refuted unless
  their recorded trigger fires.
- **No heuristic detectors.** Doctor invariants are exact predicates over
  data, or they don't ship.

## Cadence

| mechanism | when |
|---|---|
| doctor sweep | every CI integration run + before/after any walk session |
| pre-push gate + adversarial verify | every PR (verify: mandatory on money paths) |
| fresh-tenant S1/S2 | before each DP-facing milestone |
| clock soak | CI-locked (every integration run) |
| truth-rate sample | DONE (100%); repeat only if the same-PR rule is suspended |
| burn-down table review | monthly, or when any trigger fires |

## Order of execution from here

1. ~~Merge #750 (dep batch)~~ — MERGED 2026-08-06.
2. ~~Fix the Feb-clamp trap, then build B1 (`velox doctor`)~~ — B1 SHIPPED
   and CI-wired (#753/#756); the clamp trap was fixed by ADR-055.
3. ~~B2 soak~~ — SHIPPED, CI-locked (and the clamp trap was already fixed by ADR-055; memory corrected).
4. ~~B3 sample~~ — DONE, 25/25 TRUE. ~~B4 flagger~~ — REFUTED, not built.
5. C-table triage pass (LOWs + register sweep for fired triggers) — the one
   remaining item.
