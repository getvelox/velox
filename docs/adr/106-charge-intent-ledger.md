# ADR-106: Record the charge attempt before calling Stripe

**Status:** PARKED — not implemented on `main`. Superseded as the answer to this
problem by [ADR-107](107-unknown-is-terminal-until-a-human.md), which makes the
double charge unreachable in ~328 lines instead of guarding it in ~1,900. This
document is kept, and its number claimed, because the design is sound and may be
resumed; see "Trigger to resume". The implementation lives on the unmerged
branch `fix/charge-intent-ledger` (PR #679).
**Date:** 2026-07-30 (parked 2026-07-31)
**Amends:** [ADR-105](105-charge-idempotency-key-seed.md) — its deferred residual
is closed here rather than by ADR-062's obligation queue, and its "mutable
params in the key" rejected-alternative is corrected to describe what ships.
**Builds on:** ADR-049 (payment reconciler), ADR-068 (checkout-session claim
ledger — the pattern this is the second instance of), ADR-102 (charge attempt
facts), ADR-105 (attempt-counter key seed).

## Context

ADR-105 fixed *which* idempotency key we send. One residual survived it, and no
key scheme can fix it, because the problem is not the key:

> Stripe can create a PaymentIntent that Velox never learns the name of.

Two ways in, neither exotic:

- the process dies between `CreatePaymentIntent` returning and the outcome
  write landing (a **timeout** does this without any crash — it is the commonest
  ambiguous outcome);
- the call returns an ambiguous error carrying no PaymentIntent id.

The invoice is then left with no `stripe_payment_intent_id`, and
`payment.reconcileOne` cannot query what it cannot name. Its empty-PI branch
settled the invoice **failed** — with the comment *"a safe retry generates a new
PI"* — which bumps `charge_attempt_seq`, which frees the next retry to open a
**second PaymentIntent beside one that may be live and charging right now**.

That is the same double charge ADR-105 closed, reached through a different door.

## Decision

**Record the attempt before the effect** (playbook class E: record-before-effect
+ requeryable state). `charge_intents` (migration 0166) is committed *before*
`CreatePaymentIntent` and stores the exact `Idempotency-Key` header plus every
parameter needed to re-issue the identical request.

### Recovery is idempotent replay, not search

The load-bearing fact, verified against Stripe's documented contract and against
our own client code before this was designed:

> "Stripe's idempotency works by saving the resulting status code and body of
> the first request made for any given idempotency key... Subsequent requests
> with the same key return the same result."

So the recovery sweep re-issues the stored request under the stored key and is
handed the **original** PaymentIntent. Verified in code that this arrives as an
ordinary success rather than an error: `LiveStripeClient.CreatePaymentIntent`
has no replay branch and no header inspection — it returns `pi.ID`
indistinguishably from a fresh create.

Two outcomes, both safe, and there is no third:

| Stripe's state | Replay returns | Why it is safe |
|---|---|---|
| holds a saved result for the key | the original PaymentIntent (or a cached decline, which still names it) | it cannot create a second — that is what the key prevents |
| holds nothing (the request never began execution) | a newly created PaymentIntent | the one charge we always intended, and step 4 re-proved it is still owed |

Consequences worth stating plainly: **zero new Stripe client methods** (replay
is `CreatePaymentIntent`, truth is `GetPaymentIntent` — both already exist), and
**zero new `invoice.Store` methods**, because the store lives in
`internal/payment` beside its structural twin `checkout_sessions_store.go`.

### The guarantee is structural, not procedural

Two partial unique indexes carry it:

- `(tenant_id, idempotency_key)` — two initiators at the same
  `charge_attempt_seq` compute the same key, converge on one row, and send one
  key; Stripe returns one PaymentIntent to both.
- `(tenant_id, invoice_id) WHERE state <> 'resolved'` — while any attempt is
  unresolved, a second one **cannot be recorded**; and since the pre-call write
  is fail-closed, what cannot be recorded cannot be made.

The predicate is `<> 'resolved'`, **not** `= 'open'`. A `needs_review` row is
precisely the case where nobody knows whether a PaymentIntent is live — the last
state in which starting another charge would be safe. Keying the index on
`'open'` would turn the quarantine into the double-charge trigger.

### The counterpart half: the give-up write must be gated

Recovery alone is pointless. `payment_unknown` runs every tick and would settle
the invoice failed seconds after recovery quarantined it, un-quarantining it and
bumping the seq. So `reconcileOne`'s empty-PI branch now defers whenever an
unresolved intent exists, and **fails closed** if the ledger is unreadable — an
unreadable ledger is not evidence that no attempt is outstanding, and the write
it guards is irreversible.

The two halves ship together or not at all: the guard without recovery strands
invoices; recovery without the guard is undone a tick later.

### A fourth reconciler eligibility shape

`reconciler_driver.go` documents the shapes a recovery sweep's predicate may
take. This is a new one, and stronger than the existing three:

> **0. PRE-EFFECT MARKER** — committed strictly before the effect can occur,
> with the effect unreachable unless that commit succeeded (fail-closed).
> Marker-absent implies effect-*impossible*, not merely effect-simultaneous.

`charge_intent` is the only instance.

### Corrections carried in the same change

- **The exported key function was lying.** `stripe_client.go` appended
  `_<PaymentMethodID>` to the key *after* `ChargeIdempotencyKey` returned it
  (since ADR-053/#281), so the "single source" returned a string Stripe never
  received — the operator ERROR added in #678 named an unsearchable key, and
  exact replay was impossible. The suffix moved into the exported function;
  wire-value-preserving.
- **ADR-105 listed "mutable params in the key" as a rejected alternative** while
  the payment-method half had shipped a month earlier. Amended: it is what
  ships, and the ledger — not the key shape — is what stops a swapped card from
  opening a second PaymentIntent.
- **The customer-facing checkout path guarded only `processing`.**
  `IsInFlight()` has always meant processing **or unknown**, so a customer could
  click Pay on an invoice with a live ambiguous attempt. Now guarded on both,
  plus on an unresolved intent — the same double charge by a customer-facing
  route.

## Consequences

**A deliberate liveness cost.** An invoice whose attempt cannot be resolved
stays at `payment_status='unknown'` instead of being settled failed. No money
moves (it is excluded from every charge-claim predicate), but **no dunning
starts either**, and a human must act. Stuck-and-loud beats
silently-double-charged. The quarantine ERROR names the idempotency key, and
Stripe's dashboard is searchable by it.

**An operator exit already exists**, verified rather than assumed: re-sending
the event from the Stripe dashboard settles the invoice, because
`handlePaymentSucceeded` falls back to `metadata["velox_invoice_id"]`. The
sweep's adoption step then closes the intent with no API call.

**Replay is time-bounded.** Stripe retains keys "at least 24 hours"; replaying
past that would mint a second PaymentIntent — the exact bug. Recovery refuses
beyond a 12h TTL (a deliberate 2× margin under a documented floor) and
quarantines instead.

## Corrections found by adversarial review before merge

The first implementation of this design shipped four defects that its own
prose already forbade. They are recorded here rather than silently fixed,
because each shows a way the design was easy to implement wrongly:

1. **The quarantine did not block the charge path.** The pre-call guard tested
   idempotency-key equality alone — and the key recomputes *identically* in
   exactly the dangerous case, since quarantine means no outcome was recorded
   and therefore `charge_attempt_seq` never moved. A `needs_review` attempt
   sailed straight through, re-sending a key Stripe had long expired: a second
   PaymentIntent, which is the bug this ADR exists to prevent. The guard now
   requires the row to be **ours, still open, and inside the replay window** —
   and the replay TTL, which had guarded only the sweep, now guards the far
   busier inline path too.

2. **Quarantine was permanent.** The recovery sweep listed `state = 'open'`, so
   a `needs_review` row was never revisited — and the documented operator exit
   (re-send the event; the webhook settles the invoice) closes the intent only
   through the sweep's *adoption* step. The invoice was blocked forever. The
   sweep now lists unresolved rows and gives quarantined ones the adoption check
   **only**: no replay, no Stripe call, no attempt burned.

3. **A resolved row blocked its own key forever.** The key-uniqueness index was
   total, so once a resolved row held key K, a later attempt recomputing K could
   not insert, found nothing unresolved to adopt, and was refused permanently. K
   recurs legitimately — a create that succeeded whose *settle* then failed
   leaves the seq unmoved. The index is now partial on `state <> 'resolved'`.

4. **A breaker skip could delete another caller's marker.** Two callers with the
   same key are handed the same row; the one whose breaker opened deleted it,
   erasing a pre-effect marker for an attempt still in flight. `Open` now reports
   whether *this* caller created the row, and only the creator may delete it.

Three smaller ones, same source: recovery stamped `processing` with
`paidAt=nil` without re-reading, so a webhook settling the invoice mid-replay
had its `paid_at` NULLed; a circuit-breaker skip burned a recovery attempt
though it provably made no call, letting an outage quarantine the whole queue;
and `sim_effective_at` was written but never read, while the migration, the
domain type and the tests all asserted it anchored the settle (ADR-030) — it now
does.

### A second round, after the first fixes

The eight fixes above were themselves reviewed (131 agents, dismissal requiring
UNANIMOUS refutation after the first round's majority vote was observed
DISMISSING defects other dimensions confirmed). They had introduced more:

- **A definite rejection with no PaymentIntent bricked the invoice.** Stripe
  refusing outright — a detached card, no credentials — proves nothing was
  created, so quarantining protects against nothing. But the failure write
  rotates the key, the stricter guard then refused every later attempt,
  recovery reproduced the same rejection until it quarantined, and quarantine's
  only exit is adopting a PaymentIntent that will never exist. Permanently
  uncollectible AND un-write-off-able, with dunning spinning silently forever
  because the deferral wrapped `ErrPaymentTransient`, which rewinds the attempt
  count. Now: a definite rejection CLOSES the intent, recovery closes on a
  reproduced rejection, and a quarantined deferral is no longer transient so
  dunning can escalate.
- **Quarantined rows starved the sweep.** They never advance `updated_at`, so in
  a `LIMIT`ed queue ordered by it they became permanent head-of-line blockers —
  one row of starvation per quarantine. Ordering now ranks open rows first.
- **The two fixes cancelled out.** The replay TTL measured the intent ROW's age
  while the partial key index deliberately permits a NEW row under an OLD key,
  so a re-attempt reset the clock and re-authorised a key the provider had
  already forgotten. `occurred_at` now inherits the key's first use.
- **The delete guard was half-closed.** The creator flag proves nobody else
  *created* the row, not that nobody else has since picked it up; a breaker skip
  could still erase a marker recovery was working from.
- **Fix 2 had no test.** The in-memory ledger kept the pre-fix predicate, so the
  quarantine-exit fix had no executing coverage anywhere and reverting it broke
  nothing. The fake now mirrors the SQL, and the predicate has a real-Postgres
  test of its own.
- **Recovery never recorded the ADR-102 attempt fact** — which this document
  asserted it did, so a recovered attempt was invisible on the invoice timeline.

## Trigger to resume

What this buys over ADR-107 is **automatic** recovery: no stuck invoice, no
human. ADR-107 already prevents the double charge, so this is a liveness
improvement, not a safety one.

Its marginal coverage is narrower than it first appears. A lost response alone is
already handled without it — the `payment_intent.*` webhook names the
PaymentIntent and the ordinary settle path runs (the handler can find the invoice
from `metadata.velox_invoice_id` even when no id was stored). So this ledger is
really **insurance against webhook failure**: lost response AND webhook lost,
misconfigured, or delayed past patience. Stripe retries webhooks with backoff for
about three days, which makes that band genuinely narrow.

**Resume when:** the first real parked invoice that a webhook never resolved, or
production cutover — whichever comes first. Resume **on top of** ADR-107, where
two of the worst defects found reviewing this design become unreachable code
(there is no give-up write left to un-gate), and where the operator lever, the
honest banner and the sweep-exclusion built in #682 already exist — which is
exactly the part this design kept getting wrong (two review rounds found its
`needs_review` quarantine had no exit at all).

### Its give-up surface is two modes, not three (found 2026-07-31)

A resume should NOT rebuild the param-drift guard. Auditing every writer of
`amount_due_cents` shows drift on a parked invoice is **unreachable**:

| Writer | Reachable while parked? |
|---|---|
| `ApplyCreditNoteTx` | no — credit-note creation refuses an in-flight payment |
| `AddLineItemAtomicAudited` | no — draft-only |
| `UpdateTaxAtomic` | no — draft-only, "frozen once finalized" |
| credit-balance apply | no — inside the auto-charge sweep, which requires `payment_status='pending'` |
| `markPaidReportingTransition` | that IS the settle — the legitimate exit |

Nor can the payment method cause a conflict: the PM is part of the idempotency
key, so a different card yields a different KEY (a deferral on the inline path),
and the recovery replay always uses the STORED params, so it is self-consistent
by construction.

That leaves two real give-up modes:

1. **Replay TTL exceeded** — past Stripe's key retention a replay stops being a
   replay and creates a second PaymentIntent.
2. **A cached non-2xx** — Stripe saves the first response once execution begins,
   *including* failures, so a cached 5xx replays forever and never names a
   PaymentIntent. Note a card decline does NOT land here: it carries
   `error.payment_intent`, so the id is learned immediately and no replay is
   needed. This mode requires a provider-side incident.

Both are consequences of the idempotency contract working AS DOCUMENTED, not of
the provider misbehaving — which is why they are irreducible rather than
something a better implementation would avoid.

## Residuals, each with a trigger

- **Stripe cached a non-2xx for the key** — replay is inert forever and can
  never name the PaymentIntent. Ends at `needs_review`. Closing it needs a
  `PaymentIntents.List` tier filtered on `metadata.velox_invoice_id`, exact
  rather than heuristic because at most one attempt per invoice is unresolved.
  *Trigger: the first `needs_review` row, or production cutover.*
- **Attempts older than the replay TTL** are never auto-recovered, by
  construction. *Same trigger.*
- **No in-product "attach this PaymentIntent" action** for a quarantined row —
  the exit is the Stripe dashboard. *Trigger: the second `needs_review` row, or
  the first design partner.*
- **Invoices already `unknown` with no PI at deploy** have no intent row, so the
  legacy settle-failed branch still applies to them. Acceptable at 0 customers;
  no backfill (no-speculative-backfill rule). The WARN naming the legacy path is
  the tripwire.
- **Refunds carry a related but DIFFERENT defect.** An earlier draft of this
  residual claimed refunds "pass an empty idempotency key, so stripe-go
  generates a random one per attempt", making them the same orphan class.
  Reading the call sites disproved it, and the correction is recorded here
  rather than quietly deleted: both callers pass `velox_cn_<credit_note_id>`,
  which is stable across every retry, so a lost response replays to the ORIGINAL
  refund. A double refund was never possible there — the credit-note row already
  is the durable pre-call record that the charge path lacked.

  The real defect is narrower. `CreateRefund` flattens its error with
  `fmt.Errorf("%s")`, destroying the `*PaymentError` and with it the bit that
  says whether the outcome was AMBIGUOUS. Every caller therefore treats a
  timeout like a decline and writes `refund_status='failed'` — telling an
  operator the customer was not refunded when the money may have left the
  account, and inviting a manual second refund. Nothing corrects it:
  `RetryRefund` is an operator HTTP action, not a sweep. *Fixed on its own
  branch, deliberately separate from this one so it is judged on its own
  evidence; it amends this residual when it lands.*
- **The seq-coupling CI scanner reads one file** (`internal/invoice/postgres.go`),
  so a PI-stamping UPDATE added elsewhere stays unenforced. *Trigger: the first
  such write outside that file.*

## Alternatives considered

**Extend `invoice_charge_attempts` instead of a new table.** Rejected on
identity: that table's identity *is* the PaymentIntent id (unique index on it,
and its upsert returns early when the id is empty), while a pre-call row by
definition has none. Extending means two competing identities on one table plus
hand-written fold logic when a webhook inserts the PI-keyed twin first. The
separate table gets that merge free — recovery calls the existing
`RecordChargeAttempt`, whose upsert on the PI id merges with any webhook twin,
preserving ADR-103's one-row-per-attempt invariant with no new code. It is also
a *fact* table (ADR-103 made it the sole display owner of timeline payment
rows); an intent is an operational record with a lifecycle, one state of which
is legitimately *deleted*.

**Route this through ADR-062's obligation queue.** Rejected, siding with the
code comment that already says payment state-syncs will never migrate there:
an intent records a *request whose truth lives at Stripe*, not a local
obligation to be drained.

**Bump the seq at claim time** so each attempt has its own key. Breaks the
crash case directly — a crash after the bump presents a different key and mints
a second PaymentIntent. Recovering that needs two counters (attempted vs
resolved): a state machine to avoid one table.

**Search Stripe by metadata on every ambiguous outcome.** Viable and closes the
cached-error residual, but adds a client method and a round-trip on a rare path.
Deferred to the trigger above rather than built speculatively.

## Amendment (2026-08-01): the key derivation is load-bearing — do not "simplify" it

This section originally argued the opposite: that deriving the idempotency key
from `charge_attempt_seq` was an incidental implementation choice, and that a
self-keyed intent (key = the row's own id) would be cleaner. **That was wrong,
and the reasoning is recorded here so nobody re-proposes it.**

There is no `Seq` column on `charge_intents`. The seq at attempt time survives in
exactly one place: **inside the stored key string**. So the key does two jobs, not
one:

1. the exact Stripe idempotency header, and
2. a snapshot of `charge_attempt_seq` at the moment the attempt began.

Which makes the first of the three guards a **staleness detector**, not an
identity check:

	stored ..._7   vs   recomputed ..._8
	mismatch  <=>  the seq moved
	          <=>  an outcome was recorded since this intent opened
	          <=>  the intent no longer describes the invoice — do not replay

A self-minted key (a UUID) carries no information about the invoice, so there
would be nothing to recompute and nothing to compare. The check would not become
simpler — it would **cease to exist**, and would have to be rebuilt by adding an
explicit seq column and comparing that instead. Strictly more machinery for the
same guarantee. The derivation is the mechanism, not a coupling to be removed.

The two hazards previously blamed on the derivation do not survive inspection
either. The key rotates only when an intent CLOSES, and closing happens on a
named outcome (the PaymentIntent is known) or a definite rejection (nothing is
live) — neither is dangerous. Rotation is only unsafe if the outcome was
MISCLASSIFIED, which is a `classifyStripeError` dependency shared with the
shipped design, not a property of the ledger. And a quarantined intent
recomputing an identical key is caught by the state check.

### What is actually left against this ADR

- **The replay TTL.** `chargeIntentReplayTTL` approximates an eviction policy
  Stripe does not contractually pin, measured from our clock. Only **read-based
  recovery** removes it — the metadata-search alternative already listed above,
  which composes with everything else here and needs no ledger changes.
- **`classifyStripeError`.** Shared with the shipped design; calling an ambiguous
  outcome definite closes an intent whose PaymentIntent may be live. Neither
  design escapes this, so it is the highest-value place to harden either.

Not cost-free, but neither is a correctness objection.

### And the strongest argument FOR resuming it

The shipped design **cannot replay at all**, and not by choice. The parked write
(`payment_status='unknown'`) goes through `UpdatePayment`, which bumps
`charge_attempt_seq` unconditionally — so the derived key rotates the moment the
ambiguous outcome is recorded. `main` persists the sent key nowhere. The only
handle that could ever reach that PaymentIntent is destroyed by the very write
that records the problem, which is exactly what this ADR's struct comment says:
*"A key we cannot reproduce is a PaymentIntent we cannot find."*

That is why ADR-107 must park the invoice for a human: not because refusal was
judged safer than replay, but because replay is **impossible** once the key is
gone. Resuming this ADR restores that capability.

**Resume it as written.** The branch works; the guards each earn their place.

### One cheaper experiment to run first — RUN AND REFUTED (2026-08-02)

The experiment proposed here (do not bump `charge_attempt_seq` when recording
`unknown` with no PaymentIntent id, so the key stays reproducible and `main`
gains replay without a ledger) was taken through the full money-path discipline:
a three-sweep site-set enumeration, a complete design, and an adversarial attack
round. It is REFUTED, twice over:

- **Economics.** The wire key is the derived seed plus `"_<PaymentMethodID>"`,
  appended in `stripe_client.go` — and the PM used at attempt time is persisted
  NOWHERE (0162's `invoice_charge_attempts` has no PM column; the PM is resolved
  from the customer's CURRENT default on every charge). Recomputing it at
  recovery time is a heuristic proxy for the attempt-time PM: a card change
  inside the window mints a NEW key and a fresh charge. So honest replay needs a
  migration, a recorder change, and a key-derivation refactor — the "tens of
  lines" framing was false.
- **A grounded BREAKS.** Even with the PM persisted, replay reconstructs the key
  from bookkeeping (the best-effort 0162 attempt row) that cannot prove it
  describes the CURRENT parked attempt. The attack round produced a concrete
  code-grounded sequence — a stale attempt row surviving a decline-then-park
  history — where the reconstructed key is one Stripe has never seen, which
  executes a fresh charge beside a possibly-live PI. The fix (persisting an
  attempt discriminator and refusing on mismatch) converges on storing the
  key — i.e. on this ledger — at which point the experiment is not cheaper.

**Where the recoverability question actually landed:** [ADR-108]
(108-parked-invoices-search-and-adopt.md). Stripe's PaymentIntent Search API can
find an unnamed PI by the `velox_invoice_id` metadata every engine PI has always
carried — a READ, so it cannot double-charge. Its found-PI arms survived the
same attack round; its give-up arms did not and were deleted from the design.
That delivers the automatic-recovery delta this ledger promised for the common
parked case, with no table, no TTL, and no replay.

**Panel verdict, recorded (2026-08-02).** A four-judge independent panel scored
this ledger against the shipped design on six dimensions, grounded in the code:
4–0 for the shipped design, five dimensions unanimous, the ledger winning only
recoverability. This ADR therefore stays PARKED; its trigger is unchanged, and
if it ever fires, resume per the 2026-08-01 amendment above — after ADR-108's
search-and-adopt has been given the chance to resolve the case first, since it
covers the same residual read-only.
