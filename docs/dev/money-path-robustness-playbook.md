# Money-Path Robustness Playbook

A runbook for building and reviewing any change that touches money or a state
machine (invoices, payments, credits, dunning (automated payment-failure
follow-up), subscriptions, tax). Written for anyone building or reviewing
Velox's billing code, prior context or not. It exists because these bugs don't
show up when you read only the path where everything succeeds (the happy-path
read) — they live in *sibling* call sites, *concurrent* interleavings, and
*crash windows* (the gaps between two writes where a process can die with half
the work done).

**The motivating lesson (PR #325).** A dunning resolve change was reviewed four
times; each round caught a *different* instance of the *same* root problem, and
each fix exposed the next. The failure wasn't "review harder" — it was reasoning
*locally* (about the function in the diff) when the real surface was the whole
state machine: every writer of the state, every effect-firer (code that
triggers an email, a Stripe call, or a webhook off the transition), every gated
reader (code whose behavior branches on the state), every crash point. This
playbook makes that enumeration a checklist instead of a four-round discovery.

When the design or review stage runs as a multi-agent panel/workflow (several
AI agents prompted in parallel), prompt the agents per
[agent-prompting-standards.md](agent-prompting-standards.md) — it covers
grounded claims, sending the reasoning along with each request
(reason-with-request), effort routing, and the optional mid-build
spec-conformance verifier.

Pre-launch posture: **guard the money invariants, don't gold-plate.** Every rule
below is anchored to real Velox code — copy the pattern, don't reinvent it.

---

## 1. The failure-class map

The complete set of ways a billing engine silently does the wrong thing with
money. Each class has a *different fix kind* and a *different detection method* —
that's why they're separate.

Terms the tables lean on, glossed once: an **invariant** is a property that
must hold at all times; **idempotent** means safe to run twice with the effect
landing once; a **dedup key** is a stored deduplication key that makes a repeat
attempt collide instead of double-firing; a **tx** is a database transaction;
**CAS** is compare-and-swap — an `UPDATE` that applies only while the row is
still in the expected state; the **outbox** pattern enqueues an external call
as a DB row inside the same transaction as the state change; **RLS** is
Postgres Row-Level Security, per-tenant row filtering enforced by the database
itself; a **PI** is a Stripe PaymentIntent (Stripe's object for a single
payment); **livemode** is the flag separating real money data from test-mode
data; **ADR-NNN** is an Architecture Decision Record in `docs/adr/`.

| # | Class | Invariant | Fix kind | Velox example |
|---|-------|-----------|----------|---------------|
| **A** | **Exactly-once / idempotency** | Every money mutation is idempotent-by-construction or dedup-key-guarded | dedup index / CAS / stable Stripe key | proration `idx_invoices_proration_dedup` → `invoice_proration_source_taken`; MarkPaid no-op re-read branch (avoids `amount_paid=amount_due` re-zero) |
| **B** | **Dual-write atomicity** | Internal state + its coupled effect never diverge | coordinator-tx (internal, ADR-056) / outbox-in-tx (external, ADR-040); reconciler is the *fallback* | `invoice.paid`+`payment.succeeded` enqueue **in** the MarkPaid tx; cancel final-bill folded into the cancel tx (#307) |
| **C** | **External-truth / webhooks** | Stripe (signed webhook or server response) is the *sole* money-outcome authority; ingestion is idempotent + order-insensitive; never mark money-state from a browser redirect | write the dedup row **last** (after the effect commits); 5xx to force redelivery | dedup row after effect (`row present ⇒ effect committed`); hosted-pay `?status=success` is cosmetic only |
| **D** | **Concurrency races (C1–C4)** | A transition is one atomic CAS; every write/effect/action around it is proven safe | see the C1–C4 sub-table | `SELECT … FOR UPDATE` gates + `ResolveRun WHERE state<>'resolved'` |
| **E** | **Partial-failure & loud-fail** | A mid-flow crash is recoverable (record-before-effect + requeryable state, or one atomic tx); every money error is surfaced; **no comment claims a backstop that doesn't exist** | in-tx / requeryable state / ERROR+operator path | test-clock panic → `internal_failure` on a `WithoutCancel` ctx; PaymentUnknown loud-fail CRITICAL-with-PI |
| **F** | **Incomplete-set read** | An effect that spans a *set* must read the *whole* set — no `LIMIT 1` over what is plural | fan-out query over the true set | single-source `FindBaseInvoiceForPeriod` missed the mid-period upgrade sibling → **4 of 5** money bugs (#276/#277/#278) |
| **G** | **Lifecycle termination (liveness sink)** | Every run/subscription/invoice reaches a terminal state — no stuck-active, no infinite retry, never escalate/cancel a paid customer | ensure a writer advances *every* reachable state to terminal | card-less `auto_charge_pending` retried forever (#297); exhaustRun set `escalated` even when the mover failed (#299) |
| **H** | **Tenant/livemode isolation** | Every `tenant_id` table has RLS `ENABLE`+**`FORCE`**+policy; app runs as non-superuser `velox_app` (fail-closed); scope is server-derived, never client-supplied; every `TxBypass` carries explicit `WHERE tenant_id` | migration adds RLS in the same PR; `openAppPool` role swap | 45 tables covered; `os.Exit(1)` refuses to boot RLS-bypassed in non-local |
| **I** | **Time / precision / validation** | Month/year math anchored in tenant TZ (never host `time.Local`); simulated vs wall-clock never cross-compare; int64 cents with `RoundHalfToEven` (no float); currency UPPERCASE at store; malformed input fails loud; a call that never reached the provider isn't a burned attempt | `addIntervalIn` / `time.Now().UTC()` both sides / `ErrTransientSkip` | tenant-TZ `domain.addIntervalIn` (ADR-058); `ErrTransientSkip` rewinds the attempt |
| **J** | **Contracted-instant stamping** | A transition executed LATE (catchup sweep, background settle, webhook, operator retry) stamps the instant it was *contracted* to fire — never the executor's effective-now; purely operational stamps (`issued_at`, `updated_at`, audit recorded-at) stay at effective-now. One advance crossing N boundaries produces N artifacts each at its own boundary instant | plumb the contracted instant INTO the writer (an `at` param like `resolveRunAt`; a `WithSim` ctx rebind per boundary; a PI-metadata anchor across the provider round-trip) — never re-resolve `clock.Now()` at the write site | **6 sightings before mechanizing**: trial flips, boundary cancels (ADR-097), pause resumes (#516), dunning resolves + success retry rows (#520), retry-charge ctx (#523), `paid_at` via `velox_anchor_at` (#523) |

### The C1–C4 concurrency sub-table (class D)

| | Class | Tell | Fix |
|--|-------|------|-----|
| **C1** | Non-idempotent effect under a **new caller** *or* a **crash-retry** | an effect (`fireEvent`/`Dispatch`/email/Stripe call) fires unconditionally after a state write | fire **only on the winning CAS branch** (`RowsAffected==1`) *and/or* carry an internal dedup/source key |
| **C2** | Incomplete invariant rollout | a new guard/chokepoint is added, but not every writer routes through it | enumerate ALL writers, route each through the one chokepoint |
| **C3** | Unguarded write racing a transition | `UPDATE … WHERE id=$X` with no state predicate | state-predicate `WHERE … AND status=<expected>` + `RowsAffected` gate |
| **C4** | Irreversible action on a **stale precondition** | a tick-start / handler-start check that goes stale across a Stripe (or DB) round-trip | re-read the precondition **immediately before** the irreversible action |

> **C3 and C4 are one root** — *check-then-act across a gap* (a concurrent-tx gap
> vs. a slow-external-call gap). The single scanning heuristic — "is the
> precondition re-asserted atomically at the moment of the write/action?" —
> catches both.

---

## 2. The meta-practice: complete-site-set enumeration

**Never reason locally about a money/state change. Enumerate its complete
site-set — every code site that writes, reacts to, or gates on the state — and
prove each element covered before writing a line.** This one discipline
collapses PR #325's four rounds into a single pass, because the racing firer,
the clobbering write, and the stale-gated action all live in sibling branches
and callees a diff-scoped read never opens.

Make the **state value** (`'resolved'`, `SubscriptionActive`, …) — not your diff
— the unit of proof. For the state machine you touch, list and discharge:

1. **Every WRITER** of this state (handler, service, engine, scheduler, webhook,
   operator action, reconciler — a background sweep that finds and repairs
   half-done work). Do they ALL route through the one guarded transition
   chokepoint (the single function every transition must pass through)?
   *(`ResolveRun` has six resolve paths, all funneled through `resolveRunNow`;
   a seventh site that writes the state directly is the
   bug — cf. `subscription.Activate`, the one status-flip that bypasses
   `transitionInTx`.)*
2. **Every EFFECT-FIRER** hanging off the transition (Stripe call, ledger write,
   email, outbound webhook). Is each idempotent under replay, and either in-tx or
   outbox-enqueued?
3. **Every CALLER and CALLEE.** Does a `*Tx` variant (the flavor of a store
   method that runs inside a caller-supplied transaction) skip a validation or
   effect the non-Tx path runs? *(the pre-#333 `UpsertPolicyTx`, which skipped the
   retry-schedule-length check `UpsertPolicy` enforced — both writers now share
   the `normalizeAndValidatePolicy` chokepoint.)* Does the guard extend into functions the changed
   one *calls*? *(the exhaustRun miss.)*
4. **Every precondition-check-vs-external-call GAP.** Between the check and the
   irreversible action, can a concurrent settle/redelivery invalidate it?
5. **Every CRASH POINT.** For the line *after* each commit, name the exact
   reconciler row / outbox obligation / marker column that re-drives the missing
   effect — and **open it** to confirm it sweeps THIS state.

Write the site-set as a checklist in the PR description and check each box
**against grep output**, not against memory:

```
grep -n "TxTenant\|MarkPaid\|resolveRunNow\|transitionInTx\|\.Dispatch(\|fireEvent" internal/<domain>/
```

If a new writer can't reuse the existing chokepoint (`transitionAtomic`,
`resolveRunNow`, `MarkPaid*Transition`), **create or extend the chokepoint first,
then add the caller** — never hand-roll a fresh `UPDATE`+effect.

---

## 3. Implementation checklist — gates before opening a money-path PR

Ordered by leverage. A "no" blocks the PR.

1. **Dedup primitive chosen at design time?** Client write → `/v1`
   Idempotency-Key middleware (dedup keyed on a client-sent HTTP header).
   Server/engine/scheduler/webhook write → a `source_*` partial-unique dedup
   index OR a same-tx CAS. **Never rely on the
   API header for engine paths** — they carry none.
2. **Stripe idempotency key is provenance-stable, not a fresh UUID?** Derived
   from the durable id you dedup on (`velox_inv_<id>_<charge_attempt_seq>`,
   `velox_cn_<id>`, `inv_taxrev_<id>`); can't collide across purposes (finalize
   vs dunning suffix) nor dedup two genuinely-different charges.
   **The varying part must be the EVENT you mean, never a proxy for it.** The
   key answers "same attempt or new attempt?", so it may move only when an
   attempt outcome is recorded. This gate used to bless `velox_inv_<id>_<UpdatedAt>`,
   but `updated_at` answers the much broader question "did anything touch this
   row?". That made every unrelated writer a participant in the payment
   protocol, and a tax-commit stamp landing in a crash window minted a second
   PaymentIntent (ADR-105, #678). When a seed is a proxy, ask what writes it
   that you didn't intend — and if the honest answer is "a whole table's worth
   of writers", introduce the explicit counter instead.
3. **Coupled effect classified and placed?** Internal DB write → threaded
   `*sql.Tx` in-tx (ADR-056). External call → `OutboxStore.Enqueue(ctx, tx, …)`
   in the commit tx (ADR-040), or a self-clearing marker column + scheduler sweep.
4. **The UPDATE itself is idempotent?** No-op re-read branch when the transition
   already ran (guard the `amount_paid=amount_due` re-zeroing class); CAS is
   `WHERE state<>target` and `RowsAffected`-gated.
5. **Webhook: the dedup-row write (`IngestEvent`) is the LAST write, after the
   effect commits?** Only never-processable errors (`ErrNotFound`) ack (get
   acknowledged, which ends redelivery); everything else returns 5xx to force
   redelivery. No money-state from a browser redirect.
6. **Irreversible action re-reads its precondition immediately before firing?**
   Cancel/void/pause/uncollectible re-fetch terminal STATUS (not a stale
   pre-read), falling through on a fetch error so a DB blip never burns it.
7. **Every money-path error is loud?** No `_ =` / `_, _ =` (Go's
   discard-the-error assignment) on a store write or `Dispatch`. WARN→ERROR
   when sustained failure means under-collected money.
8. **Every background goroutine/worker/advance wrapped in `recover()`** (Go's
   panic catch) that flips the entity to a *requeryable terminal* state — one an
   operator can find and act on later (`MarkFailed` on a `WithoutCancel`
   ctx) — never leaving it in-progress with no operator exit.
9. **New `tenant_id` table? Same migration adds `ENABLE` + `FORCE` +
   `tenant_isolation`.** `ENABLE`-without-`FORCE` exempts the table-owner role
   from RLS (the bug migration 0111 shipped).
   Every new `TxBypass`/`db.Pool` query carries explicit `WHERE tenant_id` (+
   `livemode`).
10. **Time/money hygiene:** every +1mo/+1yr goes through
    `domain.addIntervalIn`/`AddBillingInterval` with tenant loc; any TTL/staleness
    compare uses `time.Now().UTC()` on **both** sides (never `clock.Now(ctx)` vs a
    DB-`now()` column — the tax-commit bug shape); no `float64` reaches cents;
    currency written only at the `ToUpper` store chokepoint.
11. **Validation enforced at EVERY save entrypoint** including the Tx/recipe/bulk
    variant — if a Tx-variant skips it "because upstream validated," point at the
    exact upstream check for each invariant.
12. **Grep your own diff** for `catchup will retry` / `next cycle` / `reconciler
    will catch` / `EnqueueStandalone` / `_ =` on side-effects — and prove each
    named backstop EXISTS and sweeps this state.

---

## 4. Review lens — questions that INDEPENDENTLY re-derive each invariant

Don't accept "none of X can Y." Run the greps yourself; make the author's proof
land on the table.

- **A (exactly-once):** "What is the dedup key for THIS write, and is it stable
  across (a) at-least-once redelivery, (b) a crash between the Stripe call and the
  DB commit, and (c) two concurrent callers? If the answer is 'the CAS already
  guarantees exactly-once' with no atomic internal effect, an atomic option was
  skipped."
- **B (dual-write):** "Does the internal side-effect share the state-change tx
  (grep the `*sql.Tx` param), or run post-commit? If post-commit, is it a durable
  enqueued obligation/marker, or a fire-and-forget a crash loses? Show me the
  `Enqueue`-in-tx before the `.Dispatch(`."
- **C (webhooks):** "If Stripe redelivers this exact event after `processEvent`
  committed but `IngestEvent` failed, does re-running double-count money or
  re-fire an outbound event? Does any money-state come from a browser redirect?"
- **D (concurrency):** "Show me the writer set (`grep` every `UPDATE` of this
  column — does each carry a state predicate + `RowsAffected` gate?). Trace each
  effect up to its CAS. Where was the precondition last read relative to the
  irreversible action?"
- **E (partial-failure):** "If the process crashes on the line AFTER this commit,
  name the exact reconciler/outbox row/marker that re-drives the effect — and open
  it to confirm it re-visits THIS state. Is this error `_ =` swallowed? Is it WARN
  where sustained failure = lost money?"
- **F (incomplete-set):** "What query anchors this effect, and is its result set
  complete? Any `LIMIT 1` or single-status predicate over what is actually plural
  (multiple funding invoices per period; orphan paid/void/uncollectible rows)?"
- **G (liveness):** "Does every reachable state have a writer that advances it to
  terminal? What happens to this entity if the terminal action *fails*?"
- **H (isolation):** "Does every `tenant_id` table this diff touches have BOTH
  `ENABLE` and `FORCE` + a policy? Any new `TxBypass`/`db.Pool` query — explicit
  `WHERE tenant_id AND livemode`? Does any `BeginTx(TxTenant, X)` take X from
  request input rather than the auth-derived ctx?"
- **I (time/precision/validation):** "Does this time compare put `clock.Now(ctx)`
  on one side and a DB-`now()`/Stripe timestamp on the other? Does any advance skip
  `addIntervalIn`? Does a `float64` reach cents? Which entrypoints reach this store
  write, and does EACH run the same validation? For an external 5xx/timeout where
  the effect *may* have happened — is it counted as a real attempt?"

---

## 5. Test-lock doctrine

**MUST be an automated test** (per the MANUAL_TEST `[x]` durable rule: a
behavior ticked `[x]` in MANUAL_TEST.md is locked by an automated test, while
`[~]` marks manual-only verification):
(i) **concurrency** — always; (ii) **money-invariant** — automate unless it's an
Nth duplicate of a proven pattern; (iii) **partial-failure / crash-between-writes.**
**Manual `[~]`** only for observable/UI/live-external surfaces (live-Stripe
exactly-once stays manual).

Five non-negotiable patterns:

1. **Collision, not happy-path.** Fire the SAME mutation twice — concurrently AND
   serially — assert exactly-one effect + the SPECIFIC dedup error code
   (`invoice_proration_source_taken`, `credit_reversal_source_taken`). Assert the
   re-run invariant explicitly: second MarkPaid leaves `amount_paid` unchanged;
   second credit-apply drains 0. *Pattern:*
   `internal/invoice/postgres_proration_dedup_integration_test.go`,
   `internal/billing/engine_idempotency_integration_test.go`.
2. **Real Postgres, the DB did the filtering.** Atomicity: force the second leg to
   fail inside the tx, assert the FIRST leg rolled back (row absent) — what
   #307/#309/#312 shipped. RLS: open `TxTenant(A)`, write, then assert
   `TxTenant(B)` sees ZERO rows **with no `WHERE` clause in the query** (proving
   the DB, not the SQL, filtered). A schema test enumerating `information_schema`
   for every `tenant_id` table + asserting `relrowsecurity AND
   relforcerowsecurity` in `pg_class` catches every future 0111-class RLS slip
   automatically.
3. **Concurrent-resolver / fault-injecting fake.** A test double that, at the
   external-call boundary or between the precondition read and the write,
   *concurrently commits the racing transition* (pays/cancels/resolves the
   target). Assert BOTH outcomes: winner did the effect exactly once (assert the
   *count*, not just final state); loser took the `RowsAffected==0` branch — no
   second fire, no clobber. *Pattern:* `resolvingCanceler`/`resolvingRetrier` in
   the dunning tests, `TestResolveRun_CAS_ExactlyOnce`. A test asserting only "no
   error returned" is vacuous.
4. **Mutation-verify the guard is non-vacuous.** Temporarily revert the `WHERE`
   predicate / the CAS gate / the re-read and confirm the test goes **red**. The
   red run is the *only* proof the guard exists rather than being decorative — and
   it's what a later refactor that re-introduces an unconditional fire or a second
   tx trips over in CI. (PR #325 shipped a 5-mutation check.)
5. **One advance, many boundaries (class J).** Drive a test clock (a simulated
   clock the engine's time-based behavior can be advanced against) across ≥2
   contracted instants in a SINGLE advance (two retry dates; a resume date plus a
   cycle close) and assert each artifact — `resolved_at`, `paid_at`, timeline
   rows, the charge ctx's `Sim.At` — stamps ITS OWN boundary instant, none the
   advance-end frozen time. Asserting only the final state is vacuous: the sweep
   reaches it either way; the *instants* are what regress. *Pattern:*
   `TestProcessDueRunsForClock_RecoveryStampsContractedInstant`,
   `TestProcessDueRunsForClock_ChargeCtxCarriesAnchoredInstant`.

---

## 6. Current posture (as of 2026-07-11)

**Update (2026-07-11).** Since the 2026-07-02 census below: the simulated-data
lifecycle became its own design-of-record (**ADR-086** — is_simulated gates on
all five wall-clock money sweeps, clock teardown, branded `EffectiveNow` on the
FE; supersedes ADR-016); the HA/N=2 posture is documented in
`ha-readiness-2026-07-06.md` (10 hazards, 17 verified-safe; build trigger =
production cutover); and the 2026-07-10 design-quality review
(`design-review-2026-07-10.md`) shipped a fix-now batch (#427–#440) including
the `domain.TaxFacts` embed (class-A/B adjacent: the tax-fact field-drop class
is now a compile error), typed effect outcomes (`domain.NotifyOutcome` — a
skipped effect is never a silent nil), and fail-loud parity on the cancel
builder. A proposed **rendered/asserted-truth class** and the
containment trip-wire live in that review, still pending adoption here (the
review called it "class J", but that letter has since been taken by the adopted
class J, contracted-instant stamping — see §1).

## 6a. Census posture (as of 2026-07-02)

Classes **A** (exactly-once), **B** (money-event dual-writes), **D** (concurrency
C1–C4), and **H** (tenant isolation) are locked and test-covered — don't
re-litigate them. Every MEDIUM finding from the original census (the full sweep
of the codebase against the failure-class map above) has shipped:

- ~~`subscription.Activate` lost-update~~ — **fixed #327**: `Update` now carries
  `AND status='draft'` + an `ErrNoRows` re-query → `InvalidState` conflict,
  matching the `transitionInTx` chokepoint its siblings use. Locked by the
  real-Postgres `TestActivate_StoreUpdate_GuardsAgainstConcurrentCancel`
  (mutation-verified).
- ~~`SettleFailed` dunning crash-window~~ — **fixed #328**: the `dunning_backfill`
  reconciler (`Engine.EnrollFailedWithoutDunning`) re-drives the idempotent
  `StartDunning` for failed invoices with no run (state-agnostic `NOT EXISTS`,
  0085-exactly-once). **ADR-064** ratifies the triggered-primary +
  derived-backstop architecture; **#330** additionally moved the `payment.failed`
  event into the fail-tx (outbox, gated on `firstForThisPI`).
- ~~`dashboard_sessions` no RLS~~ — **fixed #331 (m0124)**, which also fenced
  `user_tenants` — a second unfenced table **found by the new
  `TestRLSIsolation_EveryTenantTableIsFenced`** (the §5.2 enumeration test, now
  built: discovers every `tenant_id` table from `information_schema`, asserts
  `ENABLE`+`FORCE`+policy, empty reason-required allowlist). The manual audit
  sweep had missed `user_tenants` — enumeration beats lists; the whole
  missing-RLS class is now CI-caught.

The LOW residue is closed too — **the census is fully discharged**:

- ~~`UpsertPolicyTx` validation parity~~ — **fixed #333**: both policy writers
  route through one shared `normalizeAndValidatePolicy` chokepoint, so a
  mismatched recipe fails at instantiate-time, not mid-campaign. Locked by
  `TestUpsertPolicyTx_ValidationParity` (mutation-verified).
- ~~64KB webhook-body truncation~~ — **fixed #334**: an over-cap body is
  detected (read cap+1) and rejected as 413 `payload_too_large` with a size
  diagnostic, instead of truncating → HMAC-failing → a misleading
  "invalid signature" 400. Locked by
  `TestWebhookHandler_OversizedBodyIs413NotSignatureFailure` (mutation-verified).

Deferred-with-trigger and honestly documented (do not re-flag): the failed
customer **email** post-commit best-effort (by design — symmetric to the receipt
email; the *event* is in-tx since #330); `EventDispatcher.Dispatch` no-`*sql.Tx`
for ~16 *notification* webhooks (zero consumers, no money event affected);
`relieveUnpaidPrebill` unpaid-branch post-commit; exhaustRun's 24h self-heal
re-attempting a permanently-failing mover unbounded (the deliberate
"keep requeryable" tradeoff); `RetryPendingTaxCommitForClock` absent (test-mode
only — clock-pinned ⇒ `livemode=false` by CHECK constraint, no real-VAT exposure);
a durable `collection_failed_at` anchor + the ADR-062 queue (see ADR-064's
cheap-strengthening triggers).
