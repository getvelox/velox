# ADR-092: Split the org billing timezone from the org display timezone (DEFERRED design)

**Date:** 2026-07-14
**Status:** **Proposed — DEFERRED, not built.** This records the industry-standard root fix and a validated implementation design, held until a named trigger. In the meantime [ADR-091](091-org-timezone-change-seam-absorb.md) ships the money-correct one-zone seam handling, which closes the only actual defect; ADR-077's single org zone stands unchanged.

> **Trigger to build.** The first design partner that needs a **multi-timezone** capability the one-zone model can't express: (a) billing customers on *different* civil calendars (customer A at America/New_York midnight, customer B at Europe/London — Orb/Lago's per-customer grain), or (b) an operator with a distributed team who must change the *display* timezone without re-timing billing. None of these exists at 0 customers, single-org-zone US B2B, so building the split now is speculative enterprise scaffolding (per pre-launch scoping / defer-enterprise-until-named-pressure). A 15-agent extensibility audit (2026-07-14) confirmed the split is a clean **additive** build from the current code (the `domain.BillingLocation` precedence-walk seam + the invoice-stamp pattern), not a rewrite — so deferring costs nothing.
>
> **The design below (schema, single-resolver retarget, render rule, migration, rejected alternatives) was prototyped and adversarially reviewed; build it as specified when the trigger fires.** Also deferred, as strictly-additive follow-ons on top of this split (documented so they aren't silently descoped): a nullable per-**customer** timezone override (ADR-077's named next step), a per-**user** / browser-local display timezone for distributed teams, an immutable-billing-zone option, and a dedicated reporting timezone.

**Amends (when built)** [ADR-077](077-org-level-billing-timezone.md) — org-level single-zone billing stands; this would refine the one zone into two org-level roles. Keeps [ADR-058](058-billing-date-math-tenant-timezone.md) (civil-midnight DST-exact anchoring) and [ADR-076](076-enforcing-the-timezone-invariant.md) (FE required-zone types); [ADR-074](074-subscription-billing-timezone-snapshot.md) stays dead.

## Context

`tenant_settings.timezone` served two conflicting roles: the LIVE zone every
dashboard `formatDate` renders in (ADR-010), and the zone read at billing-compute
time (`Engine.tenantLocation`, `subscription.Service.tenantLocation`) to anchor
every running subscription's period-boundary and proration date-math (ADR-058).
An operator who changed the org timezone for a cosmetic reason therefore silently
re-timed the next period boundary and re-sized the proration denominator of every
active subscription — the ADR-091 seam overbill ($90 for two months instead of
$60, reproduced live). ADR-077 correctly removed the per-subscription snapshot
(ADR-074) that caused ~8 which-zone display bugs; the gap it left is that display
and billing were still the *same value*.

A 6-platform primary-doc survey (2026-07-14) confirmed the universal invariant:
**a display/reporting-zone change never re-times billing.** Stripe, Chargebee,
Recurly document it verbatim (UTC/instant anchor + separate mutable display
zone); Orb reaches it by making its single zone immutable; Lago is the cautionary
counter-case whose mutable single zone re-times billing with an undocumented
transition — which is exactly the bug Velox had.

## Decision

Split the one column into two org-level roles:

1. `tenant_settings.billing_timezone` (new, `TEXT NOT NULL DEFAULT 'UTC'`, migration
   0151, `UPDATE billing_timezone = timezone` for existing rows) — the sole civil-day
   anchor for ALL billing math. The two concrete resolvers repoint to it; the ~40
   anchor call sites and the invoice-stamp writers funnel through those two
   resolvers and are untouched.
2. `tenant_settings.timezone` (unchanged wire name and meaning) — the LIVE display
   zone every cosmetic surface reads.

Billing math reads ONLY `billing_timezone`; the cosmetic display control writes
ONLY `timezone`. A display-zone change is therefore *structurally* incapable of
re-timing a subscription — the separation is a column boundary, not a convention.
There is still exactly ONE billing zone per tenant, so ADR-074's per-entity
which-zone ambiguity cannot recur. Billing still resolves a fixed IANA zone and
snaps civil-midnight in it, preserving ADR-058 DST-exactness.

`billing_timezone` **is** operator-changeable (not immutable — we are not Orb):
a distinct, confirm-gated "Billing timezone" control, separate from the cosmetic
"Display timezone" control. Changing it is go-forward-safe: stored period instants
(`current_billing_period_start/end`, `next_billing_at`) are untouched, only the
next roll re-resolves the new zone (no gap, no double-bill — `next_start` is the
prior period's end instant), and any resulting sub-day seam is absorbed by
ADR-091. It resolves ADR-077's deferred "operator confirm on org-TZ change": the
confirm now attaches to the *billing* control, and the *display* control needs no
confirm because it can no longer affect billing.

## Render rule (one choice per surface CLASS, never per entity)

Because `billing_timezone` is single-valued per tenant, "the billing zone" is
never a which-entity choice, so ADR-074's ambiguity cannot recur.

- **Class A — frozen issued documents** (invoice/credit-note PDF, hosted invoice,
  dunning next-retry line): each document's own stamped `invoices.billing_timezone`
  (ADR-077 — unchanged; its *source* is now the org billing zone). Fixes the
  `prorationRefundDesc` branch-name drift for free: refund text and the stamp now
  share the billing zone.
- **Class B — live billing surfaces** (subscription period display, trial ranges,
  proration line labels, preview): the org `billing_timezone`. Backend authors
  `current_billing_period_display` in it; the FE reads a module-scoped
  billing-zone singleton (no per-sub wire field).
- **Class C — instants** (created/issued/paid/voided, timeline, key/credit
  expiry) and relative-time: the org display `timezone`. Unchanged.

## Deliberate scope cuts (documented, not silent)

- **In-flight-period display is not grandfathered.** On a deliberate
  `billing_timezone` change, a *running* period's displayed dates re-render in the
  new zone (shift a civil day / off-midnight) until it rolls over (≤ 1 cycle);
  issued invoices stay frozen. Grandfathering it (a denormalized per-period
  display stamp) is deferred as cosmetic polish on a rare admin action.
- **Grain stays org-level, not per-customer/per-subscription.** Every surveyed
  leader anchors finer (subscription: Stripe/Chargebee/Metronome; per-customer TZ:
  Orb/Lago). That grain is a *multi-timezone-customer* capability; Velox is
  B2B-not-B2B2C, single-org-zone US market, 0 customers — no operator can even
  express "bill this customer in a different zone." **Trigger to revisit:** the
  first design partner needing per-customer billing zones (the additive path is
  ADR-077's deferred nullable `customers.timezone`, resolved in the same two
  resolvers).

## Alternatives rejected

- **Per-subscription frozen zone (revive ADR-074's dropped column).** Reverts
  ADR-077, models two-subs-on-different-calendars (a case ADR-077 established does
  not exist), 40-site sweep with a silent-UTC hydration trap, immutability forces
  cancel+recreate. Rejected on grain and blast radius.
- **Frozen UTC *instant* (Stripe's literal `billing_cycle_anchor`).** Advancing a
  fixed UTC instant drifts every DST-zone subscription's boundary to the prior
  civil day for ~4 winter months a year (verified), abandoning ADR-058's DST-exact
  civil-midnight periods. Rejected.
- **Immutable billing zone (Orb's model).** Simplest, but strands an operator who
  set the wrong zone or relocates (cancel+recreate to fix). Rejected in favor of a
  confirm-gated, go-forward-safe change control.

## Consequences

- A cosmetic timezone change can no longer move money. (+)
- Two org knobs the single-zone target market sets identically forever — the UI
  must make their relationship obvious and initialize billing from display. (−, mitigated)
- A deliberate `billing_timezone` change still re-anchors all active subs' next
  boundary org-wide (inherited ADR-077 tradeoff, moved behind a confirm, not
  eliminated); the sub-day seam is absorbed by ADR-091. (−, mitigated)
- A future billing-anchor read that reads `ts.Timezone` directly would silently
  re-couple billing to display; guarded by an arch test asserting `ts.Timezone` is
  read only in the two resolvers + the documented display fallbacks. (risk, gated)
