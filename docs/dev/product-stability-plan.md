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
4. **Runbook truth-rate ≈ 100%** on sampled re-walks — a checked box means
   *proven at walk time*; the sample measures whether that still predicts
   current behavior.
5. **Every deferral carries a trigger**, in the ADR or the velox-ops
   register. An open gap without a trigger is a bug in this plan.

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
claim-to-read gaps, long-horizon time math, and — dominantly — **new code as
it lands**.

## Pillar A — Intake: keep new bugs out *(exists — do not weaken)*

New code is the largest defect source. The standing gates:

- **Money-path playbook** on any money/state-machine change: complete
  site-set enumeration before writing; the 12 gates; per-class review lens.
- **Adversarial verification on money diffs**: independent read-only finders
  over the finished diff (leftovers / regressions / doc-truth). This caught
  the #748 race after a clean local pass — treat it as mandatory for
  money-path PRs, cheap optional elsewhere. Findings are verified by the
  author, not a refuter panel (~5 finders max; refuter panels measured
  useless here).
- **Same-PR doc rule**: CHANGELOG + the matching MANUAL_TEST flow + ADR (+
  ADR index row) + README ship with the change. A doc that lies is a defect.
- **Pre-push gate**: gofmt · go vet · `go test ./... -short` ·
  golangci-lint (its `unused` finding = deleted-test alarm) · `make gen`
  (BOTH halves — `npm run gen` alone lets the Go types drift) · `tsc -b`
  (never `--noEmit`, it is a no-op here) · FE tests.
- **Belt gates narrow WITH claims** (the #748 lesson): any status-universe
  change must sweep claim SQL, service pre-reads, *and* the terminal
  provider-call gate in the same PR — the claims read before the handler
  re-reads, so the last gate is load-bearing, not redundant.

## Pillar B — Detection: find latent bugs *(the build list)*

### B1. `velox doctor` — the money-invariant sweep *(BUILD FIRST — does not exist)*

One command that interrogates any Velox database and reports invariant
violations. Converts the class that found the $46.69 stranding from
"noticed during an ADR" into "caught by CI, forever." First invariant set:

- per-invoice conservation: `amount_paid + amount_due + credited = total`
- status ↔ timestamp coherence: `paid ⇒ paid_at`, `uncollectible ⇒
  uncollectible_at`, `voided ⇒ voided_at`; no `tax_reversed_at` on a
  status that should not carry one
- credit ledger: grants − drawdowns = balances; no negative blocks;
  event-sourced sum matches materialized balance
- no expired-but-unreleased charge leases; no outbox rows pending past DLQ
  age; no dunning run `active` on a terminal invoice
- credit notes: `issue_pending=false ⇔ status transitioned`; no
  issued-but-unapplied against live invoices
- parked coherence: `payment_status='unknown' ⇒` no auto_charge_pending

Wire-up: run after the integration suite in CI (fails the build on any
violation) and runnable ad hoc against dev/walk DBs. When a prod deploy
exists, it becomes the ops health check. Every future bug class gets its
invariant added here in the fixing PR — that is the mechanized half of
"enforce invariant after a bug class."

### B2. Long-horizon clock soak *(build second)*

One scripted test clock driven ~14 months month-by-month across a year
boundary and a February, running the doctor after every advance. This is
the cheapest flush for the time-math class — including the blast radius of
the **known Jan 31 → Feb 28 clamp trap**, which gets FIXED as part of
building this (fix first, then the soak proves the class).

### B3. Runbook truth-rate sample *(one session)*

~25 boxes, random but money-weighted, re-run against current code. Output
is a number. ≈100% ⇒ the ledger is trustworthy and full re-walks stay
off the table; drift found ⇒ expand only where found.

### B4. Staleness flagger *(small script)*

Every annotation carries its walk date. Map flows → source areas, flag any
flow whose code changed after its annotation date. Run before anything
DP-facing; re-walk only flagged flows. Replaces calendar-based re-walking
with change-based re-walking.

### B5. Fresh-tenant smoke ritual *(no build — a rule)*

S1/S2 (~15 min) on a **fresh tenant** before any DP-facing milestone.
Fresh-tenant walks found 3 seam bugs that aged-tenant walks structurally
could not.

## Pillar C — Known-items burn-down *(scheduled, not aspirational)*

| item | state | action |
|---|---|---|
| fast-uri HIGH + audit-fixable batch | fixed in #750 | merge on green |
| Jan 31 → Feb 28 date-math clamp | **known trap, unfixed** | fix with B2 |
| ~41 LOWs backlog (2026-07-02 audit) | recorded | one triage pass: promote money-adjacent, close-with-reason the rest |
| cost-dashboard public-token hashing | **HARD GATE pre-prod** (lies audit) | schedule before any prod deploy |
| engine post-commit audit rows (6) | open, CI-gated | with the next engine arc |
| react-router 8, orval-next | deferred WITH trigger | leave; revisit on trigger |
| breached-pw + TLS (pre-deploy), MFA (pre-DP) | gates recorded (#507) | leave; they gate deploy, not today |
| HA N=2 hazards | trigger = prod cutover | leave |

## Pillar D — Rot prevention *(exists — keep enforced)*

- Runbook: stale flow = rewrite or delete, never leave-and-document; the
  same-PR revision rule is what kept D6 true through three redesigns in one
  week.
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
| staleness flagger | before each DP-facing milestone |
| clock soak | after B2 lands: with each release tag |
| truth-rate sample | once now; repeat only if drift found |
| burn-down table review | monthly, or when any trigger fires |

## Order of execution from here

1. Merge #750 (dep batch) — in flight.
2. **Fix the Feb-clamp trap, then build B1 (`velox doctor`)** — one arc,
   playbook rules apply (it touches money math).
3. B2 soak using the doctor as its oracle.
4. B3 sample + B4 flagger (one session together).
5. C-table triage pass (LOWs + register sweep for fired triggers).
