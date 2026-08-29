# ADR-091: An org-timezone change never overbills a subscription — absorb/prorate the re-anchor "seam"

**Status:** Accepted (2026-07-14). Keeps [ADR-077](077-org-level-billing-timezone.md) (one org-level billing timezone) and [ADR-058](058-billing-date-math-tenant-timezone.md) (civil-midnight DST-exact anchoring). The full billing/display decouple is the deferred root fix — [ADR-092](092-split-billing-timezone-from-display.md), triggered by a design partner needing multi-timezone billing.
**Context:** money-path review of the ADR-058 + ADR-077 interaction. Reproduced live on the dev server; the anniversary/yearly case found by a 15-agent adversarial review.

## The defect

Billing period boundaries are re-resolved each roll against the one org timezone
(ADR-077, resolved live; ADR-058 anchors civil-midnight in it). When an operator
changes the org timezone mid-cycle — often for a purely cosmetic reason, e.g. to
read dashboard timestamps in a country they've travelled to — the next roll
re-interprets the stored period-end instant in the new zone. West-ward, "the
anchor in the new zone" lands a few hours *after* the stored period-end, so the
engine bills a degenerate short **"seam"** upcoming period. For an `in_advance`
base fee (length-insensitive) this overbills. It manifests two ways:

1. **Calendar (sub-day seam).** `NextBillingPeriodEnd` snaps to the 1st, ~9.5h
   past periodEnd. `roundDays(9.5h) == 0`, so the short-period proration guard
   no-ops and the full month is billed for ~hours. Live repro: a $30/mo sub
   switched `Asia/Kolkata → America/New_York` billed **$90 across two months
   instead of $60**.

2. **Anniversary / yearly seam — the subtle one.** `advanceAnchored` snaps to
   the next anchor *day*, so `baseEnd` and the proration denominator
   `fullCycleDays` (both `advanceBillingPeriod(baseStart)`) are **identical by
   construction** — `advanceDays == fullCycleDays`, the gate is dead code, and
   neither residual is sub-day, so the absorb misses it too. The magnitude
   differs by interval: anniversary-**monthly** bills a FULL month for a **~24h**
   seam (~30× overbill); **yearly** bills a FULL year for a **~335-day** residual
   (~$30 vs the correct ~$27.53, a ~1.09× overbill). This shipped uncovered
   because the original seam test only exercised calendar billing.

## Decision

Make every re-anchor seam money-correct, for every billing interval, while
keeping ADR-077's single org zone:

- **Calendar sub-day seam → absorb.** At the in_advance base-fee site, skip the
  base line when the upcoming period rounds to zero days (`advanceDays <= 0`) —
  the exact mirror of `emitBaseSegmentLine`'s pre-existing `segDays <= 0` in_arrears
  absorb (why in_arrears never had this bug). The sub re-aligns on the next full
  cycle; the sliver is free and self-healing.

- **Anniversary/yearly seam → prorate against a nominal cycle.** A seam leaves
  `baseStart` **off the anchor grid**. Detect it with `domain.IsPeriodStartOnAnchor`
  (day-of-month == the month-end-clamped anchor day; calendar's anchorDay 0 is
  always on-anchor and untouched). When off-anchor, measure `fullCycleDays`
  NOMINALLY — one calendar interval from `baseStart` (`advanceBillingPeriod(…,
  anchorDay 0)` = `addIntervalIn`, ~28–31 days / ~365) — instead of the
  re-anchored value that collapsed to the seam width. The gate then fires and the
  ~24h seam prorates to ~1 day's worth (e.g. $1 for a $30/mo sub; the ~335-day
  yearly residual prorates to ~$27.53) instead of a full fee.

A nominal denominator is used **only** for an off-anchor seam — a normal
month-end anniversary period (28-day February, day-31 anchor) is on-anchor, so it
keeps the exact current behavior and never spuriously prorates.

## Alternatives rejected

- **Merge the seam forward into the next full period.** Cannot distinguish a TZ
  seam from a legitimate short calendar-realign / plan-change stub (a Dec-31→Jan-1
  drift is a 1-day stub that overlaps the seam's length range); a length threshold
  breaks stub proration (`TestRunCycle_PlanIntervalChange_InAdvance_StubProrated`).
  Absorb/prorate by actual length sidesteps the discrimination entirely.

- **Split billing timezone from display timezone (the decouple, ADR-092).** The
  industry root fix — a display-zone change then *structurally* cannot re-time
  billing. **Deferred**, not rejected: it is a multi-timezone capability (bill
  customer A in New York, customer B in London; or an operator who must change
  *display* without touching billing), and Velox is pre-launch, 0 customers,
  single-org-zone US B2B. Building it now is enterprise-shaped work with no named
  pressure, and it adds real surface (two-knob UI, per-surface render
  classification). ADR-092 records the full design + 6-platform industry
  grounding + extensibility audit so it is a clean additive build the day the
  trigger arrives. Until then, this seam handling makes the one-zone model
  money-correct — which was the only actual defect.

## Consequences

- An org-timezone change can never bill a full cycle for a re-anchor seam, for
  calendar, anniversary, or yearly subs; the sub self-heals to clean boundaries
  next cycle.
- in_advance and in_arrears now handle a sub-day upcoming period identically.
- DST-exact civil-midnight boundaries (ADR-058) and the single org zone (ADR-077)
  are unchanged.
- Enforced by real-Postgres tests asserting no full fee for a sub-cycle period
  across all three billing intervals (mutation-verified against removing each
  guard) + a `IsPeriodStartOnAnchor` unit table (calendar / anniversary / month-end
  clamp).
