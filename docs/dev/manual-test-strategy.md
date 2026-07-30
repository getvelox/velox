# Manual-test strategy — how to walk a flow like a senior tester

**Status:** living doc. Consolidates the testing discipline that was previously
scattered across session memory and per-flow habits. Sibling to
[money-path-robustness-playbook.md](money-path-robustness-playbook.md): the
playbook governs **how to build** a money-path change, this governs **how to
prove** one behaves.

Every technique below is here because it caught a real defect in this codebase.
Where a technique names a PR, that PR is the bug it found. Nothing in this doc is
aspirational.

---

## 0. The stance

> **"I clicked it and nothing broke" is not a test result. Evidence that the
> system is correct — or a named reason I could not obtain it — is a test
> result.**

Three corollaries that decide almost every judgement call:

1. **A box is not walked until it could have failed.** If no outcome of the
   exercise would have made you write "this is broken", you performed a
   demonstration, not a test.
2. **Assert the consequence, not the call.** `200 OK` is not a passing money
   test. The row, the balance, the provider's own record, and the operator's
   screen are the assertions.
3. **When the doc and the code disagree, that is a finding — always.** One of
   them is wrong and you now know something nobody knew this morning. Never
   silently "fix" the doc to match observed behavior without deciding which was
   right (fix UI up, not doc down).

---

## 1. Five lenses on every flow

Run all five. Most escaped defects in this codebase were invisible to four of
them and obvious to the fifth.

| Lens | The question | Blind to |
|---|---|---|
| **Behavior** | does it do the documented thing? | silent omissions, wrong-but-plausible amounts |
| **Money** | is every cent accounted for, and did it move where claimed? | UI lies, ordering |
| **Honesty** | does every rendered claim trace to a recorded fact? | correct-but-unreadable screens |
| **Design** | is this architecture up to the mark for a mid-scale customer? | implementation defects |
| **Visual** | does it *look* right in its interactive states? | everything logical |

The **Visual** lens is the one most easily skipped, because DOM/text assertions
are *structurally incapable* of seeing layout: overflow, containment,
truncation, z-index, obscured controls. It cost two operator-reported defects
before it was written down (see PR #669: a dialog popover spilled ~110px past
both edges while every text assertion passed).

The **Design** lens earns its keep on passes too: a documented "this is correct
and here is why, and here are the 2–4 platforms that agree" is a first-class
outcome, not a consolation prize.

---

## 2. The technique catalogue

### 2.1 Control vs treatment — *the highest-yield technique*

Build **two fixtures identical in every respect but one variable**, run the same
action on both, and diff the outcome. This does two jobs at once: it isolates
the defect to one cause, and it proves the failure isn't your fixture being
wrong.

> **Found:** the clawback silent-drop. Two identical downgrades on identical
> subs; the only difference was the source invoice's `payment_status`. Control
> (`succeeded`) → credit note issued, customer balance **+$32.18**. Treatment
> (`processing`) → **no row anywhere**, balance **$0**, and the customer kept
> the cheaper plan. Without the control I could not have distinguished "bug"
> from "my proration was zero."

Use it whenever you suspect a branch. It converts "something's off" into a
one-line diagnosis.

### 2.2 Negative controls — prove the mechanism *discriminates*

An always-on check is indistinguishable from a working check until you feed it
the case it must **not** fire on.

> **Found/confirmed:** the currency-mismatch tripwire fires on a real EUR charge
> against a USD invoice — and stays silent for a lowercase `usd` charge against
> `USD`. That negative is what proves the case-folding works and explains why
> every ordinary settle is clean. Likewise: an unpinned entity must render **no**
> `Recorded` subline; a `customer_data_invalid` deferral must **not** be touched
> by the retry reconciler.

Rule: every guard box needs its "and it does nothing when it shouldn't" twin.

### 2.3 Provider-side verification — leave the building

For anything involving an external system, the local row is a *claim*. Go ask
the provider.

> **Did:** `stripe refunds retrieve re_3TynHk…` → `status: succeeded`,
> `amount: 3000 USD`, against the same PaymentIntent Velox charged. That is the
> difference between "we wrote `refund_status=succeeded`" and "money left the
> account."

Applies to: refunds, charges, Checkout sessions (`open` vs `expired` — how the
StopCollection bug was proven), tax transactions, webhook delivery.

### 2.4 Exact-number fixtures — never approximate the interesting case

Build the fixture to the numbers that make the distinction *visible*. Rounding
"close enough" hides exactly the bug the box was written for.

> **Mattered:** the mixed-paid invoice was built to **$82.60 = $20.00 credits +
> $62.60 card**, so the refund field could be shown reading `max $62.60` — the
> **card portion**, not the $82.60 total. With a tax-free approximation the cap
> and the total would have coincided and the assertion would have proven
> nothing.

### 2.5 Mutation verification — test the test

After a test passes, **break the code it guards and watch it fail**. A test that
never failed is a test whose subject you have not identified.

> **Did:** reverting the outbox anchor stamp → the anchor test failed on exactly
> the missing field; restoring the old hardcoded tax-code list → the scan test
> failed naming exactly `provider_not_configured`.

Non-negotiable for any test shipped alongside a money-path fix.

### 2.6 Declared-source vs call-site audit

When something declares itself the single source of truth, **grep every
consumer** and check they actually read it. Prose cannot enforce this.

> **Found (PR #661):** `TaxRetryableErrorCodes()` documents itself as "the single
> source for which codes the reconciler retries", and the operator banner
> classifies from it — while **both** scan call sites hardcoded a shorter list.
> The banner promised a retry from a queue that never contained the row. Live
> proof: twelve consecutive ticks at `retry_count=0`.

Generalise the fix: make the test **derive its fixtures from the source** (one
stuck row per declared code), so the next divergence fails whichever entry it
drops.

### 2.7 First-use / cold-path bias

Test the **first** time, the **empty** state, and the **new** entity — guards are
most often absent exactly where the happy path is most common.

> **Found (PR #666):** the credit-expiry date floor read the customer's test
> clock correctly… except on a **first-ever grant**, because the dialog resolved
> the customer from a list scoped to customers who *already had a balance*. The
> guard was missing in its most common case, under a comment describing a
> protection that wasn't running.

### 2.8 Adversarial state — the states no happy path produces

Some states are only reachable by force, and the box should say so. Forcing them
with `psql` is legitimate **when no product flow can produce them** — an
in-flight payment on demand, a lost provider transaction id, a revoked table
grant. It is *not* legitimate as a shortcut around a flow that exists.

Always restore and re-assert the positive half afterwards: `processing` → 409,
then reset → the same action succeeds. A guard proven only in the blocking
direction might just be broken.

### 2.9 Degenerate & same-instant states

Ask "what does this do when everything happens at once, or when the collection
is empty, or when there is exactly one?" On a frozen clock, *every* row shares
an instant — so same-instant behavior isn't an edge case, it's the common case.

> **Drove:** the ADR-104 ordering work. The golden test is an invoice whose
> entire life happens at one frozen instant.

### 2.10 Time-based observation — some proofs only accrue

A backoff ladder, a lease expiry, a delivery verdict arriving days later: these
cannot be asserted at t=0. Arm the fixture, record what you expect and when, and
come back. Say so in the box rather than claiming the untested rungs.

> **Did:** the tax retry ladder — `+5m`, `+15m`, `+1h`, `+4h`, `+12h` observed
> across a real afternoon, with the 8-attempt exhaustion box left explicitly
> armed for ~7 days later.

### 2.11 Re-derive the blocker

A "blocked" note is a claim with an expiry date. Re-derive it before trusting it.

> **Found:** the I5b blocked-note was wrong **twice** — a retryable failure *was*
> reachable without touching the tenant's Stripe (a scratch tenant with the
> provider selected but never connected), and the real schedule was days not
> hours. Walking past that stale note is what found the #661 money bug.

### 2.12 Cross-surface reconciliation

The same fact rendered on two screens must agree, and the operator must be able
to reconcile them **without leaving the page**.

> **Drove:** Invariant A — any row whose two calendars differ shows both, so the
> invoice timeline and the Sent-emails card can be checked against each other.

---

## 3. The per-box protocol

For each box, in order. Steps 4 and 7 are the ones most often skipped.

1. **Read the box as a spec.** What exactly is claimed? Any claim you cannot
   observe is a doc bug — fix the doc or delete the claim.
2. **Predict the outcome before acting.** A surprise is only informative if you
   had an expectation.
3. **Exercise the real path** — dashboard for UI claims, API for contract
   claims. `psql` only per §2.8.
4. **Screenshot the interactive state and LOOK at it.** Open dropdowns, dialogs
   mid-interaction, long-content and empty states. Text assertions cannot see
   layout.
5. **Assert the consequence** in the DB *and* at the provider *and* on screen.
6. **Run the negative control.**
7. **Apply the design lens** — and record the verdict either way.
8. **Record evidence in the box** (§5), including anything you could *not* prove.

---

## 4. Fixture strategy

- **One purpose-built tenant per campaign.** Aggregate surfaces (stat cards,
  badges, lists) can only be *asserted* when the tenant's contents are fully
  known; in an aged tenant "the number renders" is the only checkable claim.
- **Fresh fixtures per box family, named for the box** (`C2 Guards Co`,
  `TR-CXL negatives`). Reused fixtures accumulate state that silently changes the
  premise — a "fully card-paid" box quietly becomes credits-paid.
- **Never contaminate a documented fixture.** Existing annotations cite specific
  invoice numbers; those are evidence and must stay reproducible.
- **Disposability is a feature.** Clock-pinned fixtures are removed completely by
  a clock delete — which is itself worth walking (a surviving row is a lie told
  later).
- **Keep the fixture that proves the finding**, and name it in the box, so the
  next session can re-observe rather than rebuild.

---

## 5. Evidence standard for an annotation

A walked box carries **what was observed**, not "works". Minimum:

- the fixture identity (invoice/CN/sub number, clock position)
- the **verbatim** operator-visible string where copy is the claim
- the numbers, with the arithmetic shown when it reconciles
  (`5000 + 3473 = 8473`)
- provider-side evidence where money moved
- the CI test name where a leg is automated rather than hand-walked
- **explicitly what was NOT proven**, and why

That last item is what keeps the doc honest. Half-proven is fine; half-proven
described as proven is the rot this discipline exists to prevent.

---

## 6. Stopping rules

- **Automate** concurrency, money invariants, and any bug class that has now
  bitten twice. Three drifts of one class ⇒ mechanise a gate.
- **Hand-walk** anything whose failure mode is *operator perception* — copy,
  layout, ordering, banner honesty. No test sees these.
- **Defer with a named trigger**, never with "later": what unblocks it, and
  roughly when. An armed fixture beats a vague intention.
- **Cap the pragmatism** at pre-launch scope: do not build fault-injection
  harnesses for a corridor no customer can reach — say so in the box instead.

---

## 7. Anti-patterns (all committed in this campaign, by me)

| Anti-pattern | What it cost |
|---|---|
| Text-only assertions on a UI box | two operator-reported layout defects (#669) |
| Trusting a "blocked" note | delayed the #661 money bug by a day |
| Approximating fixture numbers | would have hidden the refund-cap distinction |
| Asserting the API's `200` and moving on | a silently-404'd setup call turned a "fully card-paid" fixture into credits-paid; the box's premise was gone and I nearly recorded it |
| Guessing an endpoint/param shape | phantom "bugs" (`/send-email`, `/credits/deduct`, `at_trial_end`) — verify the route before reporting behavior |
| Claiming a whole box when one leg was walked | the fix: say which leg, name the CI test for the rest |

---

## 8. Consolidated from

This doc supersedes the scattered guidance in these memories, which now point
here: manual-test currency · prefer-real-flow-over-DB · walkthrough
design-review trigger (incl. the visual-QA clause) · verify-real-path ·
testing-strategy-rules · verify-completion-don't-assert · industry-parity
per-flow. The money-path playbook and the ADRs remain authoritative for
*design*; this doc is authoritative for *proof*.
