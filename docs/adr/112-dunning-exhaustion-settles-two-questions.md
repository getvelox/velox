# ADR-112: Dunning exhaustion settles two questions, not one

**Status:** Accepted (2026-08-05)
**Amends:** [ADR-036](036-dunning-campaigns-model.md) (dunning campaigns —
`final_action` and its 2026-05-16 four-value enum)
**Relates to:** [ADR-110](110-written-off-invoices-close-self-service-payment.md)
(a written-off invoice closes self-service payment),
[ADR-111](111-a-write-off-has-no-tax-leg.md) (a write-off has no tax leg)

## Context

When a dunning run exhausted its retries, one column decided everything:

```
final_action ∈ { manual_review, pause, mark_uncollectible, cancel_subscription }
```

Three of those four names describe the **subscription**. One describes the
**invoice**. They were never alternatives — they answer different questions —
and forcing a single choice produced two defects.

**The combination nobody could express.** Stripe's own default outcome is
cancel the subscription *and* write the invoice off. In Velox you picked one.

**The debt that never closed.** In `manual_review`, `pause`, and
`cancel_subscription` — three of the four — the unpaid invoice was left
`finalized` / `payment_status='failed'` / `amount_due > 0` **permanently**.
Verified, not assumed: `MarkUncollectible` has exactly three callers (the
operator endpoint, the dunning terminal action, the dunning resolve dialog),
and `subscription.Service.Cancel` never touches the outstanding invoice. No
sweep closes it. Cancel the subscription and the receivable sits open with no
closer anywhere in the tree.

The second defect is the worse one, and it was invisible in the UI: an
operator picking "Cancel subscription" had nothing telling them the debt
would outlive the subscription.

## Industry evidence (verified verbatim 2026-08-05)

**Every peer that has a write-off concept configures both axes. Velox was
the only one that made them exclusive.**

| | Subscription fate | Invoice fate |
|---|---|---|
| **Stripe** | "Cancel the subscription" / "Mark the subscription as unpaid" / "Leave the subscription overdue" | derived — cancel ⇒ invoice `uncollectible` |
| **Recurly** | subscriptions are "only automatically canceled when an invoice fails dunning — and only if your dunning settings are configured to expire subscriptions at the end of the dunning cycle" | "Recurly marks an invoice as failed at the end of the cycle if unpaid. A failed invoice is written off as bad debt" — **default on**; or "configure dunning to never auto-fail the invoice, leaving it overdue indefinitely" |
| **Chargebee** | "you can define the final action taken on the subscription. You can choose to let the subscription remain active or cancel the subscription at once" | "Mark the invoice as not paid, Void Invoice, Write-off the invoice, Reverse the invoice…" |
| **Velox (before)** | **one enum — exactly one of four** | |

- Stripe: https://docs.stripe.com/billing/revenue-recovery/smart-retries —
  the three subscription settings, quoted above. The pairing to write-off is
  documented separately: *"If your payment retry setting for Stripe
  subscriptions is to cancel the subscription, it marks the associated
  invoice as uncollectible."*
- Recurly: https://docs.recurly.com/docs/dunning-management
- Chargebee: https://www.chargebee.com/docs/payments/2.0/dunning/dunning-v2 —
  *"these are two separate choices."*

Lago and Orb have no write-off state at all (ADR-110's finding: zero hits
across Lago's 401 doc pages + OpenAPI and Orb's 309 doc pages + API
reference), so they are not evidence either way on this question.

**On the value itself, Velox was already at parity.** `mark_uncollectible`
leaving the subscription running matches Stripe verbatim — *"Stripe treats
the subscription as if the user paid and stops attempting to collect
payment. The user's subscription continues as normal."* The gap was never
the semantics of the value; it was the shape of the menu.

## Decision

**Split `final_action` into two independent columns.**

```
final_subscription_action ∈ { none, pause, cancel }
final_invoice_action      ∈ { none, mark_uncollectible }
```

`manual_review` is not missing — it **is** `(none, none)`, which is exactly
Chargebee's "remain active" + "not paid".

Migration 0171 backfills behaviour-preservingly. No existing policy changes
what it does:

| was | becomes |
|---|---|
| `manual_review` | `(none, none)` |
| `pause` | `(pause, none)` |
| `cancel_subscription` | `(cancel, none)` |
| `mark_uncollectible` | `(none, mark_uncollectible)` |

### The default stays `(pause, none)`

0071's default is kept exactly, and the invoice half deliberately does **not**
default to a write-off — even though Recurly defaults its auto-fail on.

A write-off is an accounting assertion that a debt has gone bad. A machine
should not make one on a fresh tenant's behalf, unasked, on a policy they
never opened. Same reasoning as ADR-111, which removed the write-off's tax
leg rather than have the platform decide a jurisdictional relief question
for the tenant.

What replaces the default is **saying so**: the policy form now states the
consequence of the chosen pair in plain language, including *"The unpaid
invoice stays open and due. Nothing closes it automatically — you collect it
or write it off yourself."* The old form said nothing at all about the debt.

### Auto-void and Chargebee's "Reverse" are refused

Chargebee offers four invoice fates; Velox offers two. Void is not a harsher
write-off — it is a different assertion: *this sale never happened*. It
annuls the supply and reverses tax (ADR-111 keeps that behaviour precisely
because void means annulment). Having a **machine** assert that delivered
usage was never sold is the bad money event the governing rule refuses to
CREATE. Chargebee's "Reverse" (mark paid + adjustment credit note) is the
same category, further along — it makes the invoice read `Paid` to a
customer who never paid.

Deferred with a trigger: a design partner with a contractual auto-void
policy, at which point it is an operator-chosen action on a real contract
rather than a default the platform invented.

## Applying two actions where there was one

Splitting one terminal action into two created a convergence problem that
did not exist before, because a failed exhaustion is re-attempted 24h later
and **both** subscription movers refuse an already-terminal subscription
(`cancelSpec()` allows only draft/trialing/active; `SetPauseCollection`
rejects canceled/archived), while `MarkUncollectible` returns *"invoice is
already uncollectible"* rather than a no-op.

Without care, a re-attempt would fail forever on the half that already
succeeded and never reach the half that did not.

**Three rules make it converge.**

1. **Skip-if-done, read from the entity.** A narrow
   `SubscriptionStateReader` answers "is this subscription already
   terminal". Chosen over matching the movers' error strings (a heuristic
   proxy) and over recording applied-actions on `dunning_runs` (a second
   source of truth — and if an operator canceled the sub by hand, skipping
   is the *correct* behaviour, which only the entity knows).

2. **Subscription first, invoice second — and the invoice half is gated on
   the subscription half succeeding.** Not stylistic. `exhaustRun`'s
   late-paid re-check resolves any run whose invoice is already
   `uncollectible`; so writing the invoice off while the subscription action
   is still outstanding makes the re-attempt return at that check and
   **strand the cancel permanently**. Pinned by
   `TestExhaust_PartialFailure_ReattemptSkipsTheDoneHalf`, which fails when
   the gate is removed.

3. **"Applied" means the requested end state HOLDS, not that this call moved
   it.** The escalation email describes the subscription's and invoice's
   state, and "your subscription has been canceled" is true whoever canceled
   it. Keying on authorship would silently drop the sentence on exactly the
   re-attempt paths this split introduced.

## What the customer is told

The escalation email composes from the outcome, never the policy — a one-off
invoice under a `cancel` policy cancels nothing and must say nothing. It
still refuses to offer a "Pay invoice" link on a written-off invoice, since
the hosted page has no Pay button for one (ADR-110); the **invoice** half
alone decides that, because the subscription half never changes whether this
invoice can be paid online.

## Consequences

- Cancel-and-write-off is expressible — Stripe's default outcome, previously
  unreachable.
- The permanently-open debt is now a **stated, chosen** outcome rather than
  an unstated side effect of three of four menu items.
- **Breaking, pre-launch (0 customers).** `final_action` is gone from the
  API, the recipe DSL, and the webhook payload. Every entry point rejects
  the old key **by name** rather than ignoring it — `encoding/json` drops
  unknown fields silently, so a caller still sending
  `final_action: "cancel_subscription"` would otherwise have stored a policy
  that pauses and never writes off, with a 200 on the way out. Reference
  recipes take a MAJOR version bump (openai/anthropic 4.0.0 → 5.0.0,
  replicate 2.0.0 → 3.0.0) because a recipe pinned at the old version no
  longer parses.
- `dunning.escalated` now carries `final_subscription_action`,
  `final_invoice_action`, and `applied`. The third is the one subscribers
  reconciling their own records need: a one-off invoice under a cancel
  policy escalates with nothing canceled, and an aggregate `final_action`
  could not say so.
- The **down migration is lossy by construction** — the two columns express
  combinations the single enum has no value for. Precedence: **the
  subscription action wins.** A rollback rolls code back too, and that code
  fires one action; losing the subscription half means a policy whose
  operator asked to STOP billing keeps billing, while losing the invoice
  half only leaves a debt open on the books. Leave the debt, never keep
  charging. Asserted, and mutation-verified, in
  `TestMigration0171_DunningActionMapping`.

## Still deferred

- **Stripe's `unpaid` subscription state** (a third subscription fate: keep
  the subscription, keep generating invoices, stop collecting). Velox's
  `pause` covers the operator intent with `keep_as_draft`. Trigger: a design
  partner who needs the drafted invoices *finalized* rather than held.
- **A dunning-exhaustion outcome per customer segment.** Stripe reaches this
  through automations. Trigger: the first tenant who needs different
  terminal outcomes for enterprise vs self-serve on the same retry schedule.
