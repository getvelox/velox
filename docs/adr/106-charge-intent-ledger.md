# ADR-106: Record the charge attempt before calling Stripe

**Status:** Accepted
**Date:** 2026-07-30
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
