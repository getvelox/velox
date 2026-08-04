# Design — Prepaid commits + drawdown (LOCKED)

**Status:** LOCKED for build (2026-07-05) after (1) Orb 3-0 adversarial
verification, (2) a 4-platform peer sweep (Metronome/Lago/Stripe/AI-infra
— 43/45 claims quote-verified), and (3) a 6-lens adversarial design panel
(wf_7f79b774: accounting, atomicity, concurrency, idempotency, scope,
site-set — all verdicts folded in below; 4 census errors corrected).
Decision record: ADR-078.

## Why (wedge fit)

Commits + drawdown is a core wedge pillar ([[project_positioning_wedge]]
item 6). The first DP profile — AI infra, Series A–B — sells **commit +
usage**. Velox already has the metering and an Orb-style credit-block
ledger (`consumed_cents` per block, FIFO drain, structural idempotency,
ADR-071 atomic expiry); the commit primitive is the missing headline.
Peer-verified: gross purchase invoicing is unanimous; exhaust→overage,
expiry, and $0 fully-covered invoices are already correct in Velox.

## Phase 1 (build target) — locked decisions

### D1. Discriminator: `grant_kind` on the ledger

`entry_type='grant'` + new nullable `grant_kind`
(`'commit' | 'promotional' | NULL`) + new `source_invoice_id` column
(the existing `invoice_id` is non-unique/multi-row — NOT reusable) with
partial-unique index:

```sql
CREATE UNIQUE INDEX idx_credit_ledger_commit_fund_dedup
  ON customer_credit_ledger (tenant_id, source_invoice_id)
  WHERE source_invoice_id IS NOT NULL AND grant_kind = 'commit';
```

Legacy grants stay `NULL` and drain in the **paid class** (they are
money-derived liabilities). `promotional` is only ever explicit operator
input. Zero behavior change until a tenant creates a promotional grant
or commit.

### D2. Funding: grant-on-issue ONLY (payment gate → phase 2)

The commit grant lands **in the Finalize coordinator tx** — the
`UpdateStatusWithReversal` shape (status flip + caller-supplied ledger fn,
both-or-neither), via a narrow service-level granter interface mirroring
`s.creditReverser` in Void (`invoice/service.go:807`). Grant carries
`grant_kind='commit'`, `source_invoice_id`, optional `expires_at`.

- Industry-verified default (Metronome/Orb): negotiated Net-N B2B commits
  are drawable at issue; payment follows terms.
- A granter error fails Finalize **by design** (operator-synchronous,
  loud, retryable — CAS makes the retry clean). Never weaken to
  post-commit.
- The fund-once index is a pure structural **backstop**: the finalize CAS
  (D5) already guarantees the granter runs at most once, so an index
  violation = broken invariant = loud tx abort. The 0093/0106
  catch-ErrAlreadyExists pattern is own-tx-only and FORBIDDEN inside a
  shared coordinator tx (poisoned-tx; see `GrantForCreditNoteTx`'s
  documented contract).
- **Deferred to phase 2** (trigger: first DP asks pending-until-paid, or
  the self-serve/auto-top-up build, which hard-defaults to gated): the
  per-commit payment gate, the `markPaidReportingTransition` funder hook,
  and any pending-block state. Verified safe to defer: gating is opt-in
  default-off at Orb and Metronome too, and the paid-transition
  chokepoint (all 11 MarkPaid callers converge at
  `markPaidReportingTransition`) is additive later.
- The funder hooks ONLY the finalize transition. It is never wired into
  `MarkPaymentFailedReportingTransition` (verified: a stale
  payment.failed cannot un-pay or touch grants).

### D3. Unwind: retire on VOID only

`Void` of an invoice carrying a commit line retires the funded grant's
**remaining** balance in the same `UpdateStatusWithReversal` tx.

- Mechanism: `RetireGrantRemainingTx` — ADR-071 `ExpireGrantAtomic`'s
  locked shape executed on the caller's tx: customer advisory lock →
  grant row re-read under `FOR UPDATE` → `consumed_cents = amount_cents`
  flip gated on `consumed_cents < amount_cents` (the exactly-once CAS;
  0 rows = clean no-op) → append the negative entry sized from the
  IN-TX value. Diverges from ExpireGrantAtomic deliberately:
  runs on the caller's tx; works with NULL `expires_at`; entry stamped
  at **void-time** `clock.Now` (never the grant's expiry — no
  future-dated entries); `entry_type='adjustment'` with
  `metadata.reason='commit_void_retire'` (it is not a term lapse;
  documented that void-retires appear under adjustments, not
  TotalExpired).
- **Uncollectible, dunning pause, and cancel_subscription do NOT retire**
  — the block stays live as a collections decision, consistent across
  all three non-void dunning terminals (dunning final actions are
  pause/cancel_subscription/mark_uncollectible; void is operator
  manual-resolve only). This also makes uncollectible→paid recovery
  correct for free: the grant was never retired, nothing to restore,
  and voided→paid is rejected so a retired grant can never be paid-for.
  Operator forcing function: void the invoice to kill the credits.
- Consumed stays consumed, always.

### D4. Commit invoices are cash instruments

- **Credit balance can never pay a commit funding invoice.**
  `ApplyToInvoiceAtomic` no-ops (returns 0) for invoices carrying a
  commit line — enforced INSIDE Apply via the in-tx invoice re-read (D6),
  covering all callers (auto-charge sweep `engine.go:5273`, dunning retry
  `adapters.go:340`, engine cycle paths). Otherwise a card-less
  customer's just-granted commit credits drain into their own funding
  invoice ("credits buy credits": $0 cash, revenue booked). Stored-PM
  auto-charge on a commit invoice stays allowed (real cash).
- **Credit notes are BLOCKED on invoices carrying a commit line** —
  typed error at CN create, both pre-pay (concession would decouple cash
  from grant size) and post-pay (refund would return cash while the
  block stays drawable — Void is blocked on paid invoices, so CN is the
  only paid-relief channel and it double-gives). Unwind paths: unpaid →
  void (retires); paid → phase 2 CN-retire leg (trigger: first DP commit
  refund ask; design sketch: retire min(remaining, CN gross) in the CN
  coordinator tx, refund capped by price×remaining/granted).

### D5. CAS prerequisites on hook-carrying flips (prod fix, in scope)

All three status writers the hooks ride are check-then-act today
(service-layer guards on stale pre-reads; UPDATEs have no status
predicate). Before money hangs off them:

- `FinalizeWithDates`: `... SET status='finalized' ... WHERE id=$X AND
  status='draft'`; 0 rows → re-read → `errs.InvalidState`. Granter runs
  only when the flip transitioned.
- `updateStatusInTx` (void/uncollectible): `SELECT ... FOR UPDATE`,
  in-tx re-check of allowed source statuses (void: draft/finalized/
  uncollectible — never paid; uncollectible: finalized), then flip.
- Kills: void→finalize resurrection minting a grant on a voided invoice;
  pay-vs-void retiring credits the customer just paid for.
- Concurrent mutation-verified tests required (playbook §5.3/§5.4).

### D6. One global lock order: invoice-row → advisory → ledger-rows

Corrected census: `ApplyToInvoiceAtomic` runs its OWN tx (not the
billing tx), takes NO advisory lock, and orders ledger-rows→invoice-row
— the reverse edge of the new hook paths (invoice→advisory). With
expiry's advisory→ledger-rows this is a reachable 3-way deadlock cycle
on the feature's hottest flow. Fix, in scope:

- `ApplyToInvoiceAtomic`: lock the **invoice row FOR UPDATE first**,
  re-read `status`/`amount_due_cents`/commit-line presence in-tx —
  no-op unless status ∈ (draft, finalized) AND due > 0 AND not a
  commit-funding invoice (D4) — then advisory lock, then ledger rows,
  drain against the re-read amount (kills the stale-amount_due race:
  settle-vs-reapply silently burning credits into a paid invoice).
  NOTE: draft stays allowed — billOnePeriod applies credits to
  tax-pending drafts.
- `AdjustAtomic`: advisory lock before its row locks.
- Grant path (advisory only) and expiry (advisory → ledger rows) are
  consistent suffixes; expiry takes no invoice locks.
- This unification is ALSO what makes alert crossings well-ordered (D8):
  today grant-vs-drain writers share no lock at all.

### D7. Drawdown order (NULL-safe)

```sql
ORDER BY (grant_kind IS NOT DISTINCT FROM 'promotional') DESC,
         expires_at NULLS LAST, created_at, id
```

(A bare `(grant_kind = 'promotional') DESC` sorts NULL first — verified
against live Postgres — draining legacy money-derived credits before
free promo: the exact inversion this feature exists to prevent.)
Applies to BOTH `drainPositiveBlocks` callers — drawdown
(ApplyToInvoiceAtomic) and clawback attribution (AdjustAtomic) —
promotional-first is intended for both. Mixed-kind regression test
required. No operator priority field (Metronome/Lago/Stripe numeric
priority = named phase-2 escape hatch).

### D8. Balance alerts (green field)

- Events: `credit.balance_low` (crossed below per-tenant threshold),
  `credit.balance_depleted` (>0 → 0), `credit.balance_recovered`
  (0 → >0). Recovered is kept NOT for parity (single-peer, Orb) but as
  the state-machine complement of depleted — without it a consumer's
  depleted state can never clear.
- Payloads carry `customer_id`, `balance_cents`, `threshold_cents`.
  AMENDED 2026-07-06 (deferral reassessment falsified the original
  mitigation): `balance_low` fires ONLY on crossing the single
  tenant-level threshold, so "implement per-customer logic
  consumer-side" is impossible for that event — a consumer never sees a
  crossing for a customer whose commit dwarfs the tenant threshold. The
  workaround that actually exists: set the tenant threshold to the
  SMALLEST customer's alert level, rely on `balance_depleted` (always
  fires per customer), and poll `GET /v1/credits/grants/{customer_id}`
  for per-customer burndown. Per-customer threshold override
  (`COALESCE(customer, tenant)`) remains the named phase-2 escape hatch
  — trigger: first DP with heterogeneous commit sizes asking for
  per-customer low alerts (tracked in docs/adr/README.md follow-ups).
- Crossings computed on `SUM(amount_cents)` before/after inside each
  ledger-writing tx via ONE shared helper called from the **3** insert
  sites (`appendEntryInTx`, `AdjustAtomic`, `ApplyToInvoiceAtomic` —
  expiry converges into appendEntryInTx). Well-ordered per customer
  because of D6. Known, documented lag: an expired-but-unswept block
  keeps SUM > 0 until the sweep retires it — the sweep's expiry entry
  produces the crossing (minutes-scale lag, matches the existing expiry
  discipline). Do NOT refactor apply/adjust to funnel through
  appendEntryInTx — their drift-capping logic is deliberately different.
- Wiring: credit store gets an outbox enqueuer injection mirroring
  invoice's `SetOutboxEnqueuer` (`invoice/postgres.go:40`) — the credit
  store has ZERO outbox wiring today. Enqueued in the same tx = ADR-040
  transactional outbox. No sweep, no LockKey needed.
- Threshold: `tenant_settings.credit_balance_low_threshold_cents`
  (nullable; NULL = no low alerts; depleted/recovered always on).
- Cleanup ride-along: delete the phantom `VELOX_BILLING_ALERTS_INTERVAL`
  from `deploy/compose/docker-compose.yml:118` (nothing reads it).

### D9. Commit line schema + validation

Migration 0136 (verified free across main + all branches; next LockKey
76540008 NOT needed — no sweep):

- `invoice_line_items`: `commit_granted_cents BIGINT NULL`,
  `commit_expires_at TIMESTAMPTZ NULL`, CHECK
  (`commit_granted_cents IS NULL OR (line_type='add_on' AND
  commit_granted_cents > 0)`). Dedicated columns, NOT metadata JSONB
  (unvalidatable money config) and NOT a side-table (orphan lifecycle) —
  matches the table's existing nullable per-type column pattern; no
  tax-drift risk (written once at line create, read by exactly one
  consumer, never copied by engine writers).
- `customer_credit_ledger`: `grant_kind TEXT NULL` CHECK
  (`grant_kind IN ('commit','promotional')`), `source_invoice_id TEXT
  NULL`, the D1 partial-unique index.
- `tenant_settings`: threshold column (D8).
- Validation lives in **`buildLineItem`** — the shared funnel for BOTH
  line-creation entry points (`service.Create` create-with-lines — the
  composer's path — AND `service.AddLineItem`; "enforced at AddLineItem"
  alone would miss the composer). Rules: commit fields ⇒
  `line_type='add_on'`, `billing_reason='manual'`, `granted_cents > 0`,
  `expires_at` in the future if set, **at most one commit line per
  invoice** (keeps the per-invoice fund-once index sufficient; multiple
  commits = multiple invoices in phase 1).
- Expiry: optional explicit `commit_expires_at`, **default never** —
  "term-aligned default" is incoherent for manual invoices (no term
  object to align to; a silent invented term is the banned
  silent-fallback class). Term-aligned becomes the default in phase 2
  when recipe-carried commits attach to subscriptions. Never stays
  first-class (Lago/Stripe/Together default).
- The existing $1M per-grant cap stays for commit grants (operator
  splits larger commits; raise-on-first-DP-ask, documented).

### D10. Phase-1 ship surface (definition of done)

Backend: migration 0136; D2 granter + D3 retire + D5 CAS + D6 lock
order + D7 order-by + D8 alerts; API `AddLineItemInput` +
create-with-lines commit fields; CN block; Apply exclusion.
Frontend (web-v2): composer commit fields on InvoiceDetail ("pay $X,
grant $Y credits, expires …"); Credits page `grant_kind` badge +
funding-invoice link; Settings threshold input.
Docs: ADR-078; CHANGELOG; MANUAL_TEST FLOW (sell commit → finalize →
draw → deplete alert → void-retire); webhook event docs.
Tests: mixed-kind drain order; finalize-CAS concurrent double-finalize;
funder-error-fails-finalize (invoice stays draft, no ledger row);
void-retire exactly-once incl. legal uncollectible→void sequence;
concurrent MarkPaid×2 (no grant — gate is phase 2, so assert NO paid
hook); credits-cannot-pay-commit-invoice; CN-blocked; crossing events
under concurrent grant+drain (post-D6).

## Known accepted limitations (documented, phase-2 triggers named)

- **ReverseForInvoice laundering (pre-existing class):** voiding a
  drawdown invoice re-grants drawn credits as a fresh NULL-kind,
  never-expiring block (no per-block attribution exists). Phase 1
  inherits: reversal grants stay NULL-kind (paid class — correct-ish);
  a commit's drawn slice escapes its expiry on drawdown-invoice void,
  and the void-retired escape co-occurrence is accepted at zero
  customers. Phase-2 trigger: first DP with expiring commits → design
  per-block drain attribution on usage entries.
- **Uncollectible/pause/cancel leave commit blocks live** (D3) —
  collections stance. The fast-follow named here (surfacing unpaid-funded
  grants in the attention dashboard) **shipped 2026-08-04**: a
  `commit_exposure` reason + `void_invoice` action, folded onto whatever
  banner the invoice already raises rather than competing with it. The
  block still stays live — that part is unchanged and deliberate; it is no
  longer invisible. See the ADR-078 amendment of the same date.
- **Reporting stays face-value-aggregate** (dashboard credit balance =
  raw SUM across kinds; a discounted commit's cash≠face gap is not
  surfaced). Phase-2: kind-split in GetBalance/overview when a DP asks.
- **Alert lag on expired-unswept blocks** (D8).

## Phase 2 (deferred, triggers named)

Payment gate + markPaid funder hook (first pending-until-paid ask /
self-serve build) · auto-recharge (hard-default gated) · CN-retire leg
for paid commit refunds · recipe-carried commits + term-aligned expiry
default · numeric drain-priority override · per-customer threshold
override · pending-block visibility · true-up (postpaid construct) ·
kind-split reporting · per-block drain attribution.

## Corrected census (per panel; verified file:line)

- `ApplyToInvoiceAtomic`: OWN tx (credit/postgres.go:469), called after
  the invoice-create tx committed (engine.go:3114); NO advisory lock
  (scope caveat credit/postgres.go:61-65); order ledger-rows FOR UPDATE
  (:480-485) → invoice-row UPDATE (:559-565).
- Ledger insert sites: **3** (appendEntryInTx :103, AdjustAtomic :359,
  ApplyToInvoiceAtomic :546); expiry's entry converges into
  appendEntryInTx (:263).
- Manual-invoice finalize entry: operator handler ONLY
  (`invoice/handler.go:442` → `service.Finalize:550`) — tax-retry
  auto-finalize excludes operator-composed invoices
  (`shouldAutoFinalizeAfterRetry`, service.go:1206-1217).
- Line-creation entries: create-with-lines (service.go:329→436-447) AND
  AddLineItem (:1063), shared funnel `buildLineItem` (:1083).
- Dunning final actions: pause / cancel_subscription /
  mark_uncollectible (dunning/service.go:31-50); void = operator
  manual-resolve only (dunning/handler.go:34-42).
- Paid-transition chokepoint: 11 callers converge at
  `markPaidReportingTransition` (FOR UPDATE + already-paid early return
  verified safe for redelivery).
- Credit store outbox wiring: none today — injection required (D8).
- Next migration 0136; ADR-078 free across branches.

## Research provenance

- Orb 3-0 (2026-07-03): gross purchase, deterministic drawdown,
  overage-not-blocked, in-arrears-only, configurable expiry,
  auto-top-up, require_successful_payment.
- Peer sweep (2026-07-05, wf_7b081b9a): Metronome 10/12, Lago 10/10,
  Stripe 7/7, AI-infra 16/16 — quote-checked by adversarial verifiers.
- Design panel (2026-07-05, wf_7f79b774): 6 lenses, 6× amend, all
  folded; census errors corrected above.
