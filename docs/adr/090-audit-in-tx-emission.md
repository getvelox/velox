# ADR-090: In-transaction audit emission (LogInTx) and the declared-coverage model

Date: 2026-07-13
Status: Accepted
Relates: ADR-089 (fail-closed retirement — the interim step this supersedes structurally), ADR-030 (wall-clock audit timestamps — unchanged; the sim axis is a SECOND axis, not a replacement), ADR-029 (ctx effective-now binding — extended to carry the clock id), ADR-086 (sim-data lifecycle — amended: sim-axis columns + surfacing ship now)

## Summary

The target rule is that an audit row is written inside the caller's business transaction with `audit.Logger.LogInTx`, so a change and its row commit or roll back together. This replaces an HTTP catch-all that guessed rows from the URL path and missed background writers. Every mutating route is declared in `internal/api/audit_routes.go` as emitting its own row or exempt with a reason; a CI test fails on undeclared or stale entries, and a runtime observer counts any undeclared 2xx mutation that wrote no row. Migration is partial: engine invoice finalize, credit grants and routes whose registry note names `LogInTx` are in-tx, but most HTTP routes (including operator invoice finalize) and six `internal/billing` background sites, pinned in `internal/arch/audit_background_writers_test.go`, write after commit, so a failed write loses the row. Inbound Stripe webhook settlement writes none. Bulk CSV exports are audited before the first byte streams, fail closed; paginated list reads are not.

## Context

The 2026-07-13 audit-subsystem e2e audit (68 verified findings, 3-judge
panel, 5-lens adversarial design panel) identified three root causes in the
audit log's write model:

- **RC1 — coverage anchored to transport.** The HTTP middleware catch-all
  guaranteed "every /v1 mutation gets a row," so background/engine writers
  (dunning-driven cancels, engine voids, reconciler credit notes), routes
  mounted outside the /v1 block, and GET-shaped data egress silently got
  nothing. The perceived safety net bred the gaps.
- **RC2 — row content derived by heuristics.** parseAuditPath (a hardcoded
  verb list with 5 dead entries and ~25 live segments missing;
  resource_id = parts[1] unconditionally) and response-body sniffing
  fabricated FALSE permanent records in an append-only compliance log —
  e.g. deleting a meter's pricing rule recorded as "deleted meter X".
- **RC3 — audit as an out-of-band observer.** Each row rode its own tx, so
  phantom rows (logout logged before revoke), lost rows (client
  disconnect), and the fail-open/fail-closed response swap (whose
  idempotency interaction was the audit's worst finding; ADR-089) were all
  structural.

## Decision

### 1. Emission joins the business transaction (shared fate, no savepoints)

`audit.Logger.LogInTx(ctx, tx, Entry)` writes the audit row on the CALLER's
transaction: the mutation and its evidence commit or roll back together.
"Committed but unrecorded" and "recorded but rolled back" become
unrepresentable on covered paths — this is real fail-closed, for every
tenant, with no post-commit window to police and no response swapping.

The panel explicitly REJECTED a savepoint variant (absorb audit-INSERT
failure for non-strict tenants): its only trigger class is deterministic
code bugs, which totality (below) plus CI kill at build time — and absorbing
the failure means silently committing money state that diverges from the
compliance record, which is RC3 again. Savepoint-per-mutation also carries a
known subtransaction-overflow pathology at scale.

Shared fate cuts both ways on external-effect flows (Stripe charge executed,
persisting tx then aborts on an audit bug): the state is recovered by the
same webhook/reconciler machinery that already owns effect-then-crash, and
the recovered persist emits the audit row with recovery provenance. A
deterministic emission bug on such a path is an OUTAGE class (Stripe retries
loop until a deploy) — accepted as strictly better than silently-unrecorded
money mutations, and gated by the totality tests.

### 2. LogInTx is total

An input a caller can construct must never abort a money tx on telemetry
grounds:

- `tenant_id` comes from the transaction's own `app.tenant_id` GUC via
  `NULLIF(current_setting(…), '')` — it cannot disagree with RLS, and a
  GUC-less transaction violates NOT NULL loudly. The NULLIF matters: on a
  pooled connection a reverted `is_local` GUC reads back as EMPTY STRING,
  not NULL (the dominant case in a warm pool), and without the fold the
  loud failure would silently depend on the incidental tenants FK — which
  retention-driven partitioning could remove later.
- `livemode` is trigger-stamped from the same tx session (0021) — the row
  lands in the plane of the transaction it rides in, by construction.
- Unmarshalable metadata degrades to `{"marshal_error": …}` instead of a
  nil that would violate the JSONB NOT NULL.
- Action/resource wire strings are the existing constants, FROZEN — FE
  filter vocabulary, badge maps, and historical rows key on them. A
  vocabulary round-trip integration test INSERTs every declared value so a
  schema-rejected constant cannot ship (the enum+CHECK drift class).

### 3. Ownership rule for the migration (store-interface surgery)

The SERVICE owns audit-row content — it builds the typed `audit.Entry` where
intent is known. The layer that OWNS the transaction exposes it: tx-owning
store methods gain `…Audited(…, emit func(tx, out) error)` variants that run
the service-built emission before commit. Handlers stop writing audit rows.
Background/engine/webhook writers pass their BUSINESS tx — never an
audit-only tx.

> **Status of that last sentence: PARTLY the present tense, partly still the target.**
> As of 2026-07-15 the engine has migrated its **first and highest-value** domain:
> **`finalize`.** Every engine finalize path — the cycle close, subscription
> create (day-1 in_advance), both cancel-final paths, the cross-interval swap, and
> the threshold reset — now emits its audit row **inside the invoice-create
> transaction** (`CreateInvoiceWithLineItemsAudited` on the own-tx paths;
> `LogInTx` on the coordinator tx for the swap, the atomic cancel and the threshold
> reset). A finalized invoice can no longer commit without its audit row, or vice
> versa. Proven by `TestFinalizeAudit_SharedFate` (real Postgres, mutation-verified)
> and the existing sim-axis parity test.
>
> **Still residual (six sites, `internal/billing`):** `cancel` ×2, `update`,
> `subscription.pending_change_applied`, `subscription.threshold_crossed`,
> `subscription.threshold_deferred` — these still emit post-commit on their own
> transaction. `internal/dunning`, `internal/webhook` and `internal/stripe`
> background paths emit none at all. Handlers are NOT fully migrated: the
> money-path routes named `LogInTx` in the route registry ride the business tx,
> but the majority of mutating HTTP routes (subscription updates, pricing,
> webhook config, dunning ops, test clocks, auth, …) still emit post-commit
> `Log` with the same residual exposure. `internal/api/audit_routes.go` is the
> authoritative per-route record of which is which — trust it, not any prose
> summary, including this one.
>
> This paragraph previously stated the whole target as accomplished fact, which is
> the failure mode this arc exists to remove — evidence, or a claim about evidence,
> that reads as true and is not. It is kept honest here: an ADR that overstates its
> own adoption is how a reviewer comes to believe a guarantee holds on a path where
> it does not.
>
> **The residual exposure, stated plainly.** For those six, the business change
> commits first and the audit row is written afterwards in a separate transaction.
> If that second write fails, the mutation stands and its evidence does not. Every
> one of them discards the error, correctly — post-commit there is nothing the
> caller can do. It surfaces as
> `velox_audit_write_errors_total{outcome="row_lost"}`, and nothing retries it.
>
> **What holds the line meanwhile.** `internal/arch/audit_background_writers_test.go`
> pins that exact set. A new post-commit writer in a background package fails CI;
> a migrated one must be deleted from the list or CI fails too. The set is the
> migration backlog and is only permitted to shrink — `finalize` was the first
> entry removed, proven gone.
>
> **Trigger to finish it.** Each remaining domain migrates in its own PR under the
> money-path playbook gates (the emission must ride the tx that performs the
> change, which means threading it through the tx-owning store method). `finalize`
> is done; `cancel`/`update` come next when a trigger arrives (the subscription
> lifecycle rows share the item-change tx the way the clawback CN already does,
> ADR-057).

First batch (this PR): credit grant/adjust. The handler's post-hoc `Log`
calls are gone; emissions ride the ledger tx (`AppendEntryAudited`,
`AdjustAtomicAudited`). Each subsequent domain migrates in its own PR under
the money-path playbook gates.

Declared scope note: because the emission attaches inside `Service.Grant`
(own-tx path), every caller of that path is audited — the operator/API
routes AND the proration-fallback + credit-note-bridge flows that reach
`Grant` from other requests. That widening is deliberate: a grant is a
grant, and the actor attribution (the enclosing request's identity) is
the rule this ADR applies to operator-triggered synchronous effects.
*(Edited 2026-10-05: this sentence named that rule "D16", a label from the
design panel's working list, which is not in the repository.)* Amended in PR4: `GrantTx` (and therefore `GrantForCreditNoteTx`, which
routes through it) now emits on the caller's transaction, closing the
own-tx/caller-tx divergence — a credit-note Issue tx deliberately carries
BOTH its `credit_note.issued` row and the grant's `grant` row.
`GrantCommitForInvoiceTx` stays unaudited by design: it exists only inside
invoice finalize, whose own `finalize` row is the canonical evidence.

### 4. The migration bridge — DONE (uninstalled 2026-07-14)

During the migration, `LogInTx` did NOT mark the request handled: an in-tx
emission can be rolled back after the call returns, and a secondary mutation
deep inside another route's flow must not suppress that route's own catch-all
row. Suppression was a REQUEST-scoped decision made by the route's owning
handler (`audit.MarkHandled`) after its operation committed. The catch-all
stayed installed until the END of the arc, and the uninstall shipped as one
complete change, never split.

**It happened.** The catch-all writer (`mw.AuditLog`), its heuristic
classifiers (`parseAuditPath`, `extractLabel`, `extractID`,
`canonicalResourceType`), its response buffer (`bufferedResponse`), its writer
(`writeAudit`), and `audit.MarkHandled` are DELETED. What replaced them:

- **A route-audit registry** (`internal/api/audit_routes.go`) — one table
  mapping every mutating (method, chi route pattern) to `explicit` (the route
  emits its own typed row; the note names the emitter) or `exempt(reason)`,
  where reason is a CLOSED enum and the note records the justification and any
  accepted loss. 102 routes: 92 explicit, 10 exempt. (Backfill and bootstrap were reclassified from exempt to explicit during review: an operator backdating usage, and a first-run install minting a live secret key, are both money-relevant actions that must leave evidence.)
- **A two-way-diff arch test** (`audit_routes_test.go`) — walks the LIVE chi
  route table: an undeclared mutating route fails CI, and a stale registry
  entry fails CI. This is the drift gate; a new route now forces an audit
  decision at review time. It is the *only* thing standing between a new route
  and silent un-auditing, which is why it diffs both directions.
- **A pure-observer detector** (`mw.AuditCoverage`, mounted at the ROOT, not
  on /v1 — RC1's fix). It NEVER mutates the response and NEVER buffers the
  body: a mutating 2xx that produced no audit row and is not declared exempt
  increments `velox_audit_uncovered_mutation_total{route}` and logs
  `UNCOVERED MUTATION`. It reports; it does not invent.

Two amendments this forced, both deliberate:

- **`Log`/`LogInTx` now self-mark** (the §4 rule above, inverted). That rule
  was written for the catch-all WRITER, where a stray mark cost a route its
  only row. It does not carry to an OBSERVER: a rolled-back emission means the
  handler answers non-2xx, and the detector only inspects 2xx. Self-marking was
  also *required* — `POST /v1/tenants` and both public checkout routes emit only
  in-tx and sit outside the old catch-all, so nothing else would account for
  them. Named blind spot: a route that emits nothing of its own but nests
  someone else's emission reads as covered at runtime — bounded by the
  registry's static gate.
- **`MarkSkip` survives, `MarkHandled` does not.** `MarkSkip` is now the
  detector's "this request mutated nothing" declaration, and it is load-bearing:
  A set of live paths return 2xx having mutated nothing — read-only previews, an
  idempotency replay, a stale-cookie logout, the fixed-200 password-reset defences,
  a settings save that changed no field, a recipe re-apply that installs nothing, a
  credit-note issue that defers, a duplicate usage ingest, a hosted-invoice
  payment-session reuse.

  That list is deliberately NOT enumerated exhaustively here. Every attempt to do
  so went stale within one PR (three consecutive audits caught it), because a
  paragraph cannot be re-derived when a caller is added. The authoritative set is
  DERIVED FROM THE CODE and gated in CI —
  `internal/arch/audit_prose_gates_test.go` (`markSkipCallers`) — which fails,
  naming the file, when a MarkSkip call appears without a declared reason.
  Without the declaration each would report as an
  uncovered mutation forever, and a detector that cries wolf on a normal client
  retry is a detector nobody keeps. `MarkHandled` is gone because it was the
  exported escape hatch that let a handler ASSERT coverage it did not have; the
  only thing that may claim coverage now is a row that actually landed.

The observer wraps the response's STATUS only (chi's `middleware.WrapResponseWriter`),
never its body, so `http.Flusher` / `Hijacker` / `ReaderFrom` pass through untouched —
the streaming CSV exports and the SSE stream are unaffected by it. A regression test
pins that property.

### 5. Sim-time axis (ADR-086 amendment)

`sim_effective_at` + `test_clock_id` are nullable audit_log columns
(migration 0148, partial index on the clock slice), because after ADR-086
teardown the audit log is a simulation's ONLY surviving record — yet nothing
could scope it to a clock, and `created_at` (wall-clock by ADR-030, and
correctly so) puts a whole simulated year inside one afternoon, so it cannot
say *when, in the simulation,* anything happened.

**What the axis means, exactly.** `sim_effective_at` is *the simulated instant
the clock stood at when the mutation was performed* — NOT the period the
mutation was about. An advance does not replay time: it stands at the new
frozen_time and settles everything that came due. So a single Jan→Mar advance
that finalizes three monthly invoices stamps all three rows **March**, and a
January window correctly returns nothing — the clock never stood in January.
The axis separates **advances**, not the periods inside one advance.

That is a real limit, so it is written down rather than sold around:

- There is **no `?order=sim_effective_at`**. Within one clock it would produce
  the same order as `created_at` (advances only move forward; rows inside one
  advance tie and fall back to the id tiebreak either way), and across clocks it
  would interleave two unrelated simulations into a timeline that never
  happened. A sort control that changes nothing, under a label promising it
  separates the events inside an advance, is a lie with a checkbox.
- Per-event simulated instants would require the engine to move effective-now
  per billed period. That is a **money-path change** — effective-now drives due
  dates, proration and dunning — and is deliberately not made here. Closure
  trigger: an operator who needs intra-advance ordering.

**The axis is derived from the ctx clock binding, not supplied per
emission.** The original `SimContext` field on the emission input is
DELETED. A per-emission field means ~90 emitters each remembering to
populate it; an emitter that forgets is invisible, and its rows are then
invisible to `?test_clock_id=` — the filter lies BY OMISSION, which is
worse than no filter, because an auditor reading a complete-looking
timeline of a simulation is reading an incomplete one. Instead:

- `clock.Sim{At, TestClockID}` is one value carrying both halves. A clock
  id resolved separately from its instant is how you get a row claiming
  "clock C at wall-clock now"; `Sim` makes that unrepresentable, and every
  consumer treats a half-set binding as absent.
- `clock.Resolver` returns `Sim` (renamed `SimForCustomer/Subscription/
  Invoice`), so **one pin resolution yields both halves**. Every existing
  `BindEffectiveNow` call site therefore carries the clock for free — the
  services already had to bind, or their business timestamps would be
  wall-clock (ADR-029).
- The ForClock/catchup drivers bind `WithSim` (clock id + frozen_time) ONCE
  for the whole advance. This is the linchpin: every row an advance produces —
  finalize, cancel, clawback issue, dunning, threshold — inherits it without any
  emitter knowing test clocks exist. (Credit expiry runs under that binding but
  emits no audit row: the event-sourced credit ledger IS its record. An earlier
  draft of this list said otherwise, which credited a row that does not exist.)
- **BOTH writers stamp**: `LogInTx` and the residual own-tx `Log`. The
  own-tx callers are not a lesser class of evidence — `invoice.Service.
  Finalize` and `billing.Engine.auditInvoiceFinalized` — the TWO writers of the
  finalize row, the heaviest evidence in the log — both write through `Log`.
  (Engine invoices are born finalized and never pass through
  `invoice.Service.Finalize`; earlier prose here named it as the writer for
  "every invoice a clock advance generates", the opposite of what
  `billing/engine.go` says.)
- Handler-level emitters (which emit on `r.Context()`, a ctx the service's
  internal bind never reaches) bind explicitly before emitting:
  `invoice` (gated on `inv.IsSimulated` — exact and free on the wall-clock
  path), `subscription`, `customer`, `dunning`, and the `test_clock` routes
  themselves. The clock's own create/advance/delete rows are what make a
  post-teardown view self-explaining: without them the log has the effects
  but not the cause, and no row saying why everything else is gone.
- `auditMetaForSub` is DELETED. It merged the same pair into metadata from
  TWO sources — the clock id off the entity, the instant off
  `sub.UpdatedAt` — which is only true when the current request is what
  last wrote that row; on `subscription.proration_failed` (writes nothing)
  it stamped a stale sim instant. The writer still mirrors both keys into
  metadata, so the dashboard's existing subline and pre-0148 rows keep
  rendering.

Surfacing ships in the same PR as the parity it depends on: `?test_clock_id=`,
`?sim_from=/?sim_to=`, a clock picker sourced from the AUDIT ROWS (not
`test_clocks` — that table is empty exactly when the forensic view is wanted),
`sim_effective_at` + `test_clock_id` columns in the CSV export, and the clock
filter carried INTO that export (an export that silently dropped it would hand
an auditor a file that looks like one simulation and is actually the whole log).
There is one sort axis — `created_at` — and the cursor seeks on the same tuple
it sorts on.

**The writer owns the axis; no emitter may hand-stamp it.** `simColumns`
overwrites `metadata["sim_effective_at"]` and `metadata["test_clock_id"]`
unconditionally and silently, so an emitter that also writes them cannot tell
its value was dropped. Seven did. Four were writing the identical instant (pure
duplication); three — the trial-end cancel/activate rows — were writing the
TRIAL-END instant, a genuinely different fact, and had it destroyed under
catchup, where the cancel is performed at the advance's instant, possibly weeks
of simulated time after the trial ended. All seven hand-stamps are deleted; the
trial-end instant now travels as `metadata["trial_end_at"]`, which says what it
is. A new simulated instant on a row gets its OWN key.

**Nested resolution may refine the binding, never erase it.** Every resolver
falls back to `Sim{At: clock.Now(ctx)}` — empty clock id, nil error — when it
cannot resolve its pin. Under a catchup ctx that value is poison, because
`Now(ctx)` reads the binding: the fallback is `{simulated instant, no clock}`,
the mirror image of the "clock C at wall-clock now" half-truth `Sim` was built
to prevent. Binding it would keep the simulated instant and drop the clock, and
every audit row below that call would silently leave the sim axis — invisible in
exactly the way that looks identical to "nothing happened". Services re-bind
under catchup routinely (dunning, the payment reconciler, invoice, subscription),
so `BindEffectiveNow` refuses to bind a clock-less `Sim` over an inherited one.

**Accepted losses, written down rather than glossed** (an unstamped row is
not a rounding error; it is a row the clock filter cannot see):

- **Operator-driven credit-note routes** (create draft / issue / void /
  retry-refund / send) emit NO sim axis. Root cause: `creditnote.Service`
  has no `clock.Resolver` at all — which is the same defect that makes an
  operator-issued CN against a SIMULATED invoice stamp wall-clock
  `issued_at` and `is_simulated=false` (reported separately; it leaks
  simulated credit notes into wall-clock analytics via
  `analytics.notSimInvoice`'s CN counterpart). Stamping the audit row while
  the entity itself stays wall-clock would paper over that bug in the audit
  layer. **Closure trigger: fix the CN clock binding; the sim axis then
  follows from the ctx with NO further audit change.** Clock-DRIVEN CN
  paths (the catchup clawback issuer) ARE stamped today.
- **Operator-driven price-override routes** (create / delete a customer price
  override) emit NO sim axis. Same root cause as the CN routes: `internal/
  pricing` has no `clock.Resolver` at all, so the handler emits on an unbound
  `r.Context()` and the row lands with NULL sim columns. Do NOT mistake this
  for the "not in the clock domain" case below — `customer_price_overrides`
  IS torn down with the clock (ADR-086), so an operator who negotiates a price
  for a simulated customer mid-simulation leaves a row that is invisible to
  `?test_clock_id=` AND, after teardown, is the only surviving evidence the
  override ever existed. **Closure trigger: give `pricing` a `clock.Resolver`
  and bind at the handler; the axis then follows from ctx with NO further audit
  change.**
- **Payment-method routes** and **public checkout rows** (hosted-invoice Pay
  click, payment-update link, checkout setup) emit no sim axis. Neither
  path binds a pin: both stamp wall-clock and drive REAL Stripe, so their rows are
  wall-clock facts about a real-world effect. (Note what is NOT the reason:
  "payment methods are outside the clock domain" is false — payment_methods is
  torn down with the clock, exactly like price overrides. The reason is that the
  CARD is real: attaching one mid-simulation attaches a real card at Stripe, and
  stamping that into simulated time would claim it happened at an instant that
  never existed.) This is consistent with the boundary the clock view already has — the
  SETTLEMENT those clicks lead to arrives as a Stripe webhook, which this
  same ADR already records as writing no audit row at all. Closure trigger
  for both: auditing the settle path (the registry's `reasonWebhookOwned`
  note), at which point the checkout breadcrumbs should join it.

### 6. Row integrity: the parts of a row a client must not be able to write

Two residual holes in the row's *evidentiary* value, both closed here.

**`request_id` was client-writable.** chi's `middleware.RequestID` uses an
inbound `X-Request-Id` header verbatim when present, generating one only when it
is absent. `audit_log.request_id` is presented in the UI as forensic correlation
evidence and is what support uses to join a customer's report back to server
logs — so a caller could pick the correlation id on their own audit rows: pin it
to a constant to make their actions unjoinable, collide it with another tenant's
traffic, or forge a value implying an action came from elsewhere. Correlation
evidence an adversary can write is not evidence; CloudTrail's `eventID`, Stripe's
request id and GCP's `insertId` are all server-minted.

`mw.RequestID` (internal/api/middleware/request_id.go) replaces chi's and ALWAYS
mints server-side (`req_` + xid), storing under chi's own `RequestIDKey` so every
existing `chimw.GetReqID` reader — the audit writer, `telemetry.ContextHandler`,
`respond.go`, `payment/stripe` — is unchanged.

The inbound value is DROPPED, not preserved under a second key. Nothing consumed
it as INPUT (the name still appears — in the test that forges the header to prove it is ignored, and in this ADR; an earlier version of this sentence claimed no reference existed anywhere, which was false 15 lines below its own use of the name. State the property, not a grep result); the published
contract is the `Velox-Request-Id` RESPONSE header); cross-service trace
continuity is already carried properly by W3C Trace Context via `mw.Tracing()`;
and recording an unverified client string anywhere on the row — even under an
honestly-named key — is precisely the "unverified input in a permanent record"
class this redesign exists to kill. **Accepted loss, written down:** rows written
BEFORE this change may carry a client-supplied `request_id`, and append-only
forbids rewriting them. A `req_` prefix marks the server-minted era; a
`request_id` without it is not trustworthy as provenance.

**The append-only triggers were the only barrier.** `audit_log` is immutable via
BEFORE UPDATE/DELETE (0011) and BEFORE TRUNCATE (0115) triggers that hold even
for an RLS-bypassing role — but `velox_app` still HELD `UPDATE`/`DELETE`/
`TRUNCATE` from 0001's blanket `GRANT ALL` (0115's own header admits it was never
revoked). One dropped or disabled trigger and the tamper-evidence log was
writable by the role that serves customer traffic. Migration 0150 revokes those
three privileges from `velox_app` (INSERT/SELECT retained — the writer only ever
appends). The privilege system and the triggers now fail independently:
`velox_app` is refused at the permission check (42501) before a trigger runs; the
owner/superuser is still refused by the trigger (P0001). Retention purges are
unaffected — they remain an owner-role operation, as 0011 documented.
### 7. Read egress: bulk exports are audited, fail-closed, emit-before-stream

RC1 named it and this closes it: a full-tenant EXPORT produced zero audit rows.
The catch-all only ever considered mutating methods, and the export handlers
emitted nothing — so copying every customer's PII, every invoice, every
subscription, or a year of usage events left the log silent. A tamper-evidence
system that cannot show who copied the evidence has a hole in its chain of
custody; Stripe and AWS CloudTrail both log data-export/read events.

- **`action=export`** (`domain.AuditActionExport`) is a NEW top-level wire string
  — the first in the vocabulary that records a READ. `resource_type` is the
  exported resource; `resource_id` is EMPTY (a bulk export has no single
  subject); metadata carries the filters and the exact filename delivered.
- **Emit BEFORE the first byte, fail closed.** The row is written before the
  stream opens, and a failed write means 500 with nothing streamed. The ordering
  is the decision: a row written at stream COMPLETION is defeated by killing the
  connection mid-stream, so pages of PII would egress with nothing recorded.
  Emit-then-stream can only OVER-record (a row for a file that later aborted —
  reconcilable against the EXPORT_INCOMPLETE marker); stream-then-emit
  UNDER-records, and in an append-only log that is unrecoverable.
- **Own-tx, detached.** An export is a read: there is no business transaction for
  LogInTx to ride. It uses `Logger.Log` (own tx, `context.WithoutCancel`), so a
  client that hangs up the moment the export starts still leaves the row.
- **No row count on the row.** It is unknowable before the stream starts, and a
  count we cannot honour is a lie in a permanent record.
- **The audit log exports itself** (`GET /v1/exports/audit-log.csv`, same
  permission as the read route). It replaces a dashboard Export button that paged
  the API in the browser and stopped at 50,000 rows — a silent truncation of the
  compliance evidence itself. `Logger.Stream` applies no cap. Because the row is
  written first, it is always in the LOG — and an UNFILTERED export therefore
  also carries the record of its own export in the FILE. A filtered export need
  not: any filter that excludes the row excludes it, and a clock-scoped export
  (`?test_clock_id=`) never contains it, because the export row is a wall-clock
  row with NULL sim columns. The row is in the log either way; the FILE is not
  self-certifying in general.
- **Declared, not inferred** — a small read-egress registry
  (`auditEgressRegistry`) with its own two-way-diff arch test over the live GET
  routes under `/v1/exports`. Deliberately NOT a general GET axis in the mutating
  registry: five routes do not justify declaring an audit story for every read
  endpoint, and a registry nobody can keep current is a registry that lies. The
  runtime detector is unchanged (it observes mutations only).
- **ACCEPTED LOSS, written down:** ordinary paginated list reads are NOT audited.
  An operator can still copy the tenant by walking `GET /v1/customers?limit=…`,
  and `audit_log` will not show it. What it CAN answer is "did anyone take the
  one-click bulk export, and when" — the question an auditor asks and the action a
  departing employee takes. A row per list call would put audit_log on the
  dashboard's hottest read path, and that noise is what makes an audit log
  unreadable (same volume argument as the ingest exemption). CLOSURE TRIGGER: a DP
  or auditor asking for PII-read evidence (SOC 2 CC6.x) → an access-log-derived
  read trail, aggregated per session/actor, not one row per GET.

Adjacent hardening shipped with it: **CSV formula injection**. A cell beginning
with `= + - @` (or TAB/CR) executes as a formula in Excel/Sheets/LibreOffice, and
customer display names flow into these files — including the audit log's own
`resource_label`. Both builders neutralize (Go free-text columns; the browser-side
builder), because the CSV is the artifact an operator hands an AUDITOR, and a file
that runs code when opened is not evidence. The client-side neutralizer exempts
cells that ARE numbers, or every negative amount in a finance export becomes text
and `SUM()` breaks.

## Consequences

- Explicit emissions gain a hard error contract: callers PROPAGATE LogInTx
  errors (aborting their tx). The `_ =` discard pattern dies with the
  own-tx writer as domains migrate.
- Email sends remain a documented best-effort lossy audit class (SMTP has
  no query-back; post-effect emission with WithoutCancel).
- WAL/latency cost of one extra INSERT inside money txs: single-digit
  percent on real flows (priced by the panel; the duplicate-index drop in
  0147 pre-paid most of it).
- Old heuristic rows are kept as-is (append-only forbids rewriting);
  mixed-era vocabulary in a long-lived dev DB is cosmetic.
- `velox_audit_write_errors_total` narrows to residual own-tx callers as
  migration proceeds; covered paths ride the mutation's own error SLO.
