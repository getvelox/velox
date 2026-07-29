# ADR-104: One activity lane — the entity's calendar

**Status:** Accepted
**Date:** 2026-07-29
**Supersedes:** the invoice page's two-lane timeline (the "Real-time
activity" card and its classification rules; #649's constant-identity
patch to it); partially delivers the velox-ops `unified-activity-event-log`
register entry.
**Builds on:** ADR-030 (simulated time everywhere; the 2026-07-18
narrative/forensic display split), ADR-102 (dual-stamped charge-attempt
facts), ADR-103 (one payment owner).

## Context

The invoice page answered "which calendar is this row on?" with a
different model than every other narrative surface. The subscription and
customer timelines follow ADR-030's display rule — one lane, simulated
instant primary, wall-clock demoted to a `Recorded …` subline. The
invoice page instead *classified* rows (`isExternalRow`: emails, stripe,
non-simulated payments/credit-notes) and physically split them into an
"Activity" card on the billing axis and a "Real-time activity" card on
the wall axis, captioned "Real times — not the test clock's dates."

The split existed for one mechanical reason: **email rows had no
simulated timestamp.** The outbox stamped SQL `now()` at enqueue and
nothing else, so a receipt email could only sort by its wall instant —
which lands weeks before the simulated "Invoice paid" that caused it.
The second lane was containment for that data gap, not a design.

The containment kept leaking. In eight days: #649 (delete the lane on
wall-clock invoices), ADR-102 (payment rows leave the lane — attempts
gained dual stamps), ADR-103 (webhook rows deleted from rendering
entirely), and the 2026-07-29 credit-note fix (operator CNs were
stamped wall-clock against ADR-030 — the owner, walking the page, asked
"why are these two credits in different lanes?" and the page could not
answer). After those fixes the lane's remaining content was: emails.

## Decision

**One lane. Every row's primary position is on the entity's calendar,
and rows that also touched the real world carry the real instant as a
`Recorded <wall>` subline.**

Mechanically:

0. **Handlers bind before enqueuing** — the load-bearing fix the build
   itself exposed. ADR-030 says "bind at every operator entry point",
   and *services* do; but emails are enqueued from **handlers**, which
   sit outside the service's bound scope. So an invoice's own state
   changes carried simulated stamps while the emails announcing them did
   not. Four paths were unbound (finalize's setup link, the operator
   resend, "Email invoice", "Email credit note") — caught live when the
   first email sent through the UI after the anchor shipped still wrote
   NULL. Mechanized by `internal/arch/email_enqueue_binding_test.go`,
   which fails on any handler passing `r.Context()` into a Send*/Notify*
   call; its first run found a fifth site the hand audit had missed.
1. **The outbox learns its cause's billing instant** (migration 0163):
   nullable `sim_effective_at` + `test_clock_id`, stamped at **enqueue**
   from the bound ctx (`clock.SimOf`) in the one chokepoint every writer
   funnels through (`OutboxStore.Enqueue`). NULL is a fact — "no clock
   was bound" — never backfilled and never inferred at read time.
   A CHECK enforces the pair invariant (both set or neither).
2. **Email timeline rows anchor to the billing axis** when the anchor
   exists, with the wall send instant as `recorded_at`; the ladder rank
   they always had reserved (`rankEmail = 90`, previously dormant)
   places them after the money events they announce — a notification is
   only ever an effect. Legacy rows without an anchor keep their wall
   stamp: misplaced but honest, confined to pre-launch fixtures (the
   ADR-103 no-backfill precedent).
3. **The second card is deleted**, along with `isExternalRow` and the
   negation caption. Charge-attempt rows also surface their existing
   dual stamp as `recorded_at` (they had both since ADR-102).
4. **Every timeline row gets a stable `id`** (outbox / attempt / dunning
   / credit-note row ids; deterministic synthetics for lifecycle rows).
   Composite timestamp keys collided the moment same-instant rows
   existed — the frozen-clock case is *entirely* same-instant.
5. **Teardown completeness extends to the outbox** (ADR-086): clock
   delete sweeps `email_outbox WHERE test_clock_id = $1`, and the
   completeness arch-test now discovers `test_clock_id`-carrying tables
   as well as `customer_id` ones, so the next clock-anchored table
   cannot slip through unclassified.

Ordering is guaranteed at three levels, unchanged in mechanism:
full-precision `sortAt` → declared causal ladder (`tieRank`) → stable
insertion order. The golden test pins the degenerate case: an invoice
whose entire life happens at one frozen instant renders
created → finalized → attempt → paid → credit note → email.

## The operator contract

The model, in one sentence:

> **Every entity lives on one calendar. If it's pinned to a test clock,
> that clock IS its calendar — everything about it happens there. When
> something also touched the real world, the real moment is noted
> underneath.**

The rule of thumb (tooltip-grade, reused verbatim):

> **Big dates are the entity's calendar. Small "Recorded" lines are
> ours. The banner tells you when you're in a simulation.**

Per-surface: a surface answers exactly one question, and the question
picks the calendar.

| Surface | Question | Calendar |
|---|---|---|
| Timelines (invoice / subscription / customer) | what happened, in order? | the entity's |
| Audit log | who did what, really when? | real, always (ADR-030) |
| Sent-emails card, webhook deliveries | did it actually go out / arrive? | real |
| Banner | am I in a simulation, and where is it parked? | stated once per page (ADR-099) |

What an operator must NEVER need to know to read a screen: who wrote a
row (engine / operator / webhook — provenance never changes
interpretation); which table a row came from; the words "wall-clock" or
any definition-by-negation of the clock's dates; any rule of the form
"X is always real-time".

Invariants, each mechanized or asserted in MANUAL_TEST:

- **A — no orphan dates:** any row whose two calendars differ shows
  both (`Recorded` subline). Cross-surface comparisons always reconcile
  on-screen. *Amendment 2026-07-29, hours after shipping:* the original
  text carved out credit notes as "story-only financial documents"
  (wall record delegated to the audit log). The first real operator
  disproved it the same afternoon: a CN issued on a frozen-clock
  invoice landed mid-timeline dated Jun 1 2027 — the only row among
  Recorded-bearing neighbors with no subline, which reads as missing
  information, not as a document convention. The corrected boundary is
  a CLASS rule: **every INSERT-backed narrative row shows both
  calendars when they differ** (emails 0163, attempts 0162, credit
  notes + dunning events 0164 — `recorded_at` stamped `now()` at
  INSERT); rows *derived from entity state columns* (the lifecycle
  rows) get their wall stamp by **read-time audit enrichment** — the
  named path, delivered the same day after the operator nudged on
  manual invoice actions: the timeline joins the invoice's audit
  entries by EXACT key (top-level action for create/finalize/void; the
  ADR-090 frozen-vocabulary `metadata.action` discriminator for
  mark-uncollectible and record-payment, which ride `action=update`),
  earliest row wins, and the card-settle paid row — which has no
  operator audit row — lifts `occurred_at` from the succeeded charge
  attempt it already folds in. Per-transition shadow columns remain the
  rejected audit-duplication class; enrichment failure degrades the
  lane loudly ("action history"), and a transition with no audit row
  renders bare — honest for pre-audit history. Known bounded edge: the
  audit query caps at 100 newest rows, so a pathological invoice with
  >100 audit entries loses its oldest stamps (renders bare, never
  wrong). Known
  residuals accepted: credit-grant rows on the customer page (not a
  timeline surface) and pre-0164 rows (NULL `recorded_at`, no subline,
  no backfill).
- **B — confusion is quarantined to the sandbox:** dual dates,
  sublines, badges, banners exist only in test mode. Live mode is one
  calendar and zero simulation vocabulary.
- **C — one word:** the real-world stamp is always **"Recorded"**,
  product-wide — the subscription timeline's existing vocabulary, which
  this aligns to; "Sent" never labels a wall stamp. *Known collision
  (found in the 2026-07-29 walk, judged acceptable):* "record" is also
  the product's verb for manually entering an out-of-band payment
  ("Record Payment", "Payment recorded (offline)", "Recorded by an
  operator — cheque, wire"). The two are distinguishable by grammar and
  position — the stamp is always a bare `Recorded <timestamp>` subline,
  the payment sense always takes an agent or parenthetical — and
  changing the timeline word would break alignment with the
  subscription surface, which is the stronger consistency. Revisit if
  an operator ever reads one as the other.
- **Vocabulary gate:** `internal/arch/operator_vocabulary_test.go`
  fails CI on "wall-clock" / "Real-time activity" / the negation
  caption in operator-facing web-v2 source (comments exempt). Its first
  run caught live copy on five surfaces, including the test-clock
  banner itself.

## Consequences

- The invoice page joins the ADR-030 narrative rule instead of being
  its outlier; the lane-classification bug family (four fixes in eight
  days) has nothing left to classify.
- Emails on simulated invoices read causally ("Invoice paid" →
  "Receipt emailed" at the same story instant) with dispatch reality
  preserved per-row.
- A deleted clock now takes its emails with it — previously outbox rows
  survived teardown invisibly (no `customer_id`, keyed only by invoice
  number inside jsonb).
- **Accepted cost:** pre-0163 emails on simulated fixtures render at
  their wall stamp, out of story order. Our own test data; production
  tenants start clean. No backfill — an inferred anchor would be a
  different, weaker fact wearing the same name.
- The "Payment reminders on the customer page →" pointer died with the
  card; dunning retry rows already carry each reminder's delivery
  verdict inline (#641), which is the information the link pointed at.

## Alternatives rejected

- **Keep two lanes, improve the copy** (the pre-decision direction —
  a better caption and a "Real time" pill were prototyped): polishes
  the explanation of a split whose only justification was the missing
  anchor. The best fix for confusing copy is not needing it.
- **Sort emails by wall time within the one lane:** re-creates the
  effect-before-cause inversion the lane was invented to hide.
- **Backfill anchors for legacy emails:** the enqueue-time binding is
  the fact; a read-time reconstruction is a heuristic (banned class),
  and ADR-103 already set the precedent for accepting the gap instead.
