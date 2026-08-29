# ADR-077: The billing timezone is an org-level setting, not a per-subscription snapshot

**Date:** 2026-07-04
**Status:** Accepted — supersedes [ADR-074](074-subscription-billing-timezone-snapshot.md).
Keeps [ADR-075](075-canonical-utc-api-timestamps.md) (process-UTC pin + canonical
UTC wire) and [ADR-076](076-enforcing-the-timezone-invariant.md) (lint-tz + FE
required-zone types) unchanged. Refines [ADR-058](058-billing-date-math-tenant-timezone.md):
the zone billing math anchors in is the ORG timezone, not a per-sub one.

## Context

A billing calendar anchors its date-math in a timezone (ADR-058): period
boundaries snap to civil midnight in that zone; proration denominators count
days in it. ADR-074 made that zone a **per-subscription snapshot** — frozen from
the tenant setting at create, immutable after, read at ~23 sites via a
`subscriptionLocation` helper — reasoning by analogy to `billing_anchor_day`.

That analogy was wrong, and the deferred adversarial panel ADR-074 promised is
what showed it. Two independent problems:

1. **It models a case that does not exist.** A per-subscription timezone only
   earns its complexity if two subscriptions *in the same tenant* legitimately
   bill on different civil calendars. No product requirement, design partner, or
   peer platform asks for that. The unit a timezone attaches to in the real
   world is the **organization** (or, one level down, the customer) — never the
   individual subscription.

2. **Its display divergence was a bug factory.** Freezing the zone on the sub
   while the tenant setting stayed mutable created two zones that could disagree
   (the frozen sub zone vs. the live tenant zone). Every human-facing date then
   had to decide *which* zone to render in, and getting it wrong shifted a
   civil day. Across the ADR-074/075 arc, two exhaustive audits found and fixed
   **~8 render-site bugs of exactly this divergence shape**. ADR-076 added a CI
   lint + FE types to *catch* the ninth — but the divergence that made the class
   possible was self-inflicted by the snapshot itself.

### What the industry actually does (verified)

No mainstream billing platform anchors each subscription in its own frozen
timezone. The billing *anchor* is a UTC instant everywhere; the timezone is an
account/org (sometimes customer) concept, for display and civil-day boundaries.

- **Stripe** — `billing_cycle_anchor` is a Unix timestamp (a UTC instant). The
  Dashboard timezone is a **display preference**, freely changeable, and never
  re-times an existing subscription. No per-subscription timezone field exists.
- **Lago** — timezone is set on the **organization** (default UTC) with an
  optional **per-customer** override. Changing it re-anchors and specifically
  needs double-bill guards — evidence that a *mutable* billing zone is the hard
  case, which UTC-default + rare-change avoids.
- **Orb** — timezone is an **account/customer** attribute (default UTC),
  immutable after creation. Per-customer, not per-subscription.
- **Metronome** — no timezone concept surfaced; usage and billing are UTC.
- **Chargebee** — site/customer timezone; changeable, display/forward-looking.
- **AWS / OpenAI / Anthropic** (the target market's own vendors) bill usage in
  **UTC**.

The consistent shape: **anchor = UTC instant; timezone = org/customer, for civil
boundaries + display; default UTC.** Per-subscription is nobody's model.

## Decision

**The billing timezone is a single org-level setting (`tenant_settings.timezone`,
default UTC). Subscriptions carry no timezone of their own.**

1. **Billing math anchors on the org timezone.** The three `subscriptionLocation`
   / `subLoc` helpers (engine, subscription service, subscription handler) are
   deleted; all ~30 call sites now resolve the zone via `tenantLocation(ctx,
   tenantID)` directly. Period boundaries, proration denominators, and the
   backend-authored `current_billing_period_display` all anchor in the one org
   zone. Because that zone defaults to UTC and changes rarely, there is no
   half-frozen split to drift.

2. **The zone is denormalized onto the INVOICE at issue** (`invoices.billing_timezone`,
   ADR-058/PR2 — kept). An invoice is a legal document; its printed dates must
   not move if the org later changes its timezone. So at invoice creation we
   resolve the org zone once and store it on the invoice. This is where
   historical stability actually belongs — on the immutable issued document, not
   speculatively on every running subscription. Invoice date rendering reads
   `invoice.billing_timezone`; subscription date rendering uses the live org zone
   (they coincide unless the org re-zoned after issue, which is exactly when the
   invoice *should* diverge and the sub should not).

3. **`subscriptions.billing_timezone` is dropped** (migration 0135) along with
   the `domain.Subscription.BillingTimezone` field and its FE type member. The
   column was added days earlier (0133) on the same arc, pre-launch, zero
   customers — nothing depends on it.

4. **Per-customer timezone override is deferred**, not rejected. If a design
   partner runs one org across genuinely different billing calendars (a
   plausible future — it's Lago's and Orb's model), the clean extension is a
   nullable `customers.timezone` that overrides the org default, resolved the
   same way at compute time. That is a strictly additive change to
   `tenantLocation`'s resolution, and it is per-CUSTOMER (an entity a human
   reasons about), never per-subscription. No speculative build now (zero
   customers, no named pressure).

## Alternatives considered

- **Keep the per-subscription snapshot (ADR-074).** Rejected: models a
  nonexistent case, and its self-inflicted display divergence is the direct
  cause of the ~8-bug class. Complexity with negative return.
- **UTC everywhere, no tenant timezone at all** (Metronome's model). Simplest,
  and defensible for a UTC-billing target market. Rejected as the *default* only
  because a tenant setting already exists, costs nothing at UTC, and lets an
  operator in a single non-UTC region see civil-day boundaries that match their
  books without a schema change. At UTC (the default) this collapses to exactly
  UTC-everywhere, so we keep the option without paying for it.
- **Mutable org timezone that re-anchors running subs** (part of Lago's model).
  Rejected: re-timing live subscriptions needs double-bill guards and surprises
  operators. The org zone is display-forward; it governs new civil-day
  computations, and issued invoices keep their denormalized zone. Changing it
  does not retroactively re-time or re-bill anything.

## Consequences

- One zone per tenant. The "which zone does this date render in?" decision
  collapses to org-zone-for-live-surfaces, invoice-zone-for-issued-documents —
  and the two agree except across a post-issue org re-zone, which is the one case
  where they *should* differ.
- ADR-075 (canonical UTC wire) and ADR-076 (lint-tz + FE required-zone types)
  are untouched and still earn their keep: the wire is UTC instants; every
  civil-day render still names its zone. Under Option B a subscription surface
  names it as "the org zone" by passing `undefined` (→ live tenant TZ), and an
  invoice surface names `invoice.billing_timezone` — the required-argument type
  still forces the choice to be conscious.
- Simpler blast radius for future work: date-math has one zone source
  (`tenantLocation`), and the per-customer override, if ever needed, slots into
  that single function.
- **Deferred (tracked) — operator confirm on an org-timezone change.** An org-TZ
  change is already forward-only *by construction*: stored period boundaries
  (`current_billing_period_start/end`, `next_billing_at`) are fixed instants and a
  settings write never touches `subscriptions` rows, so the open period keeps its
  window and only the next roll re-anchors — `next_start` is the prior period's end
  instant, so there is provably no gap and no double-bill (at most one seam period
  runs a few hours long/short). The missing finishing touch is the operator-facing
  half: a **confirm/preview** ("this re-times *future* boundaries for N active
  subscriptions; issued invoices are unaffected") before the change lands — a warn,
  not a hard block, so the common fix-a-wrong-default case still works. Deferred at
  0 customers / default-UTC, where it cannot bite (it needs an operator who sets a
  non-UTC zone, has active subs, and changes it). **Trigger to build:** before the
  first operator changes a non-UTC org timezone with active subscriptions. A
  from-scratch design panel (2026-07-04, unanimous incl. a freeze-biased lens)
  confirmed org-level over the per-sub freeze and named this confirm as the one
  finishing touch of the correct design — recorded here so the deferral is explicit,
  not a silent descope.
- **Lesson (persisted):** the per-subscription snapshot was built by analogy to
  `billing_anchor_day` *before* the adversarial panel and *before* industry
  grounding. The panel + a 6-platform check would have killed it at design time
  for the cost of an afternoon, instead of ~8 downstream render bugs, a lint, and
  this reversal. Anchor billing-model decisions to verified peer behavior and run
  the adversarial panel BEFORE building, not as a fast-follow.
