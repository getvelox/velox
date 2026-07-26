# ADR-012: Day-Grade Calendar Billing & Period Boundary Snapping

**Date:** 2026-05-01
**Status:** Accepted

## Status
Accepted

## Date
2026-05-01

## Context

A subscription created at `May 1, 2026 14:00 IST` with `start_now=true`
and calendar-monthly billing was producing an invoice with "prorated
30/31 days" — even though the dashboard's "Period" column displayed
the full month `May 1 → Jun 1`. The math and the UI disagreed because
they were anchored on different precisions:

- **UI**: rendered `period_start` and `period_end` as date-only.
- **Math**: `internal/billing/engine.go` computed
  `periodDays := int(periodEnd.Sub(periodStart).Hours() / 24)`. A
  14:00-on-the-1st creation produced `periodEnd - periodStart =
  30d 10h`, truncated to 30 by `int()`.

The root cause was upstream in `internal/subscription/service.go`,
which set `period_start = now` (the exact creation timestamp) for
calendar billing instead of snapping to a day boundary.

This bites at two layers:

1. **First-day billing**: a sub created at any wall-clock time on day
   N gets billed for fewer days than a sub created at midnight.
2. **DST transitions**: on a DST spring-forward, the 31-day period
   from Mar 1 → Apr 1 spans only `31d - 1h = 743h`, which
   `int(743/24) = 30`. The opposite happens on fall-back.

## Decision

### Period boundaries snap to start-of-day in tenant TZ

`internal/subscription/service.go` snaps both `period_start` and
`period_end` to `00:00:00` in the tenant's configured timezone (read
via a new `SettingsReader` interface, ADR-010-aligned with the
dashboard's `@/lib/dates` helpers). Stored as UTC.

- **Calendar billing**: `period_start = beginningOfDayInTenantTZ(now)`,
  `period_end = beginningOfMonthInTenantTZ(next month)`. A sub created
  at any time on May 1 is billed for the full month of May.
- **Anniversary billing**: `period_start = beginningOfDayInTenantTZ(now)`,
  `period_end = period_start + 1 month`.
- **Trial**: trial-end snaps the same way before the first paid
  period anchors against it.
- **`Activate` path**: same snap when the sub had no period set yet.

When the tenant timezone is unset or unreadable, falls back to UTC.
When the `SettingsReader` is not wired (e.g. unit tests), also UTC.

### Proration math counts whole days, DST-robust

`internal/billing/engine.go` uses `int(math.Round(d.Hours() / 24))`
instead of `int(d.Hours() / 24)`:

- With both endpoints snapped to `00:00`, a non-DST period produces
  an exact day-multiple — `Round` is a no-op.
- A period crossing a DST boundary produces `±1h` drift —
  `Round` absorbs it (e.g. `Round(743/24) = 31`, `Round(745/24) =
  31`). Truncation would silently miscount.

### Days are inclusive of the start day

A sub starting on May 12 sees `period_start = May 12 00:00`,
`period_end = Jun 1 00:00` → `Jun 1 - May 12 = 20 days` →
prorated `20/31 days`. The customer is billed for the full day of
May 12 even if the sub was created at 23:59. This matches **Lago**'s
calendar-day model (`22 days × $50 / 31 = $35.48` from their docs)
and **Chargebee**'s day-based billing mode.

### UI explains the math one hover away

`web-v2/src/pages/InvoiceDetail.tsx` renders proration line items
with a tooltip on the description. Surfaces "Subscription started
mid-cycle. Charge covers N of M days. Period boundaries snap to
start-of-day in tenant timezone (signup day inclusive)" — keeps the
description string itself tight while making the *why* discoverable.

## Industry references

| Platform | Default behaviour |
|---|---|
| **Chargebee** (day-based mode) | Snaps to `00:00:00.000` start, `23:59:59.999` end. |
| **Lago** | Calendar-day count, signup day inclusive. `22 days/31 × $50 = $35.48`. |
| **Recurly** | Day-based, standard proration. |
| **Stripe** | Default: timestamp-precise to the second. Operators with edge cases configure a "custom proration script" picking a rounding interval (`hour`/`day`/`week`/`month`). |

Velox's choice is the **Chargebee/Lago/Recurly default**, the
operator-friendly model. Stripe's timestamp-precise default is more
"correct" mathematically but produces exactly the kind of UI/math
disagreement that surfaced in the screenshot review. We adopt the
Chargebee shape and defer Stripe-style configurable rounding to a
future ADR if a tenant needs hour- or second-grade.

## Consequences

### Migration & backwards-compat

Velox is pre-launch (single tenant, no design partners). Existing
subs that pre-date this change keep their hour-precise period
boundaries until the next billing cycle, when snapped boundaries
take over. No backfill needed.

### Plan-change proration (mid-cycle)

The current implementation in `internal/subscription/handler.go` uses
`remainingPeriodFactor(sub, time.Now().UTC())` for proration credits.
With snapped boundaries, the factor's denominator is now a clean
whole-day count, but the numerator still uses `time.Now()` to the
second. This is acceptable: plan-change proration semantics are about
"how much of the remaining cycle is already paid for", which is
naturally precise to the moment the operator initiates the change.
Keeping it timestamp-precise matches Stripe behaviour.

### Tests

- `TestPeriod_DayGradeSnap` (new) pins the snap behaviour for
  calendar + anniversary + UTC fallback.
- `TestRunCycle_SkipsPendingChangeNotYetDue` (pre-existing) fails
  for an unrelated reason (date-hardcoded `2026-05-01` that today's
  calendar caught up to). Not regressed by this change.

### Future enhancements (deferred)

- Stripe-style configurable `proration_unit` per plan
  (`day` / `hour` / `second`).
- Per-plan `proration_behavior` (`prorate` / `none` / `credit_only`)
  to match Stripe's three-mode knob.
- Half-day inclusive at end (operator-confirmed credit on cancel
  mid-day).

## Amendment (2026-07-26): the segment-aware regression and the day-grade 'add' clamp

Segment-aware billing (2026-05-19, `itemBaseSegments`) silently
regressed this ADR's core guarantee. The `subscription_item_changes`
trigger (migrations 0029/0129) stamps every item INSERT with an `add`
row at the RAW creation instant, and because this ADR snaps the first
period start BACKWARD to the signup day's midnight, that row lands
strictly inside the cycle-close window (`changed_at > period_start`).
The segment walk read it as a mid-period add and re-prorated a full
first period — "prorated 30/31 days" on a full month, the exact
symptom in this ADR's Context. Found live on FLOW I1 (invoice
NIM-000247, $28.06 instead of $29). The in_advance day-1 path
(`buildOnCreateInvoice`) reads only the snapped period bounds and was
never affected — so the two cadences disagreed about the same period.

**Fix**: `itemBaseSegments` clamps a first-in-window `add` whose
`changed_at` falls on the period-start **calendar day (tenant TZ)** to
`period_start` itself. The predicate is day-grade policy, not
creation-row identification — no exact creation-vs-AddItem marker
exists in the schema, and none is needed: an add on the period's first
day owes the whole period regardless of which flow inserted it. The
clamp targets `period_start` itself (never `beginningOfDay`) so
threshold-reset and org-TZ-seam (ADR-091) periods that legitimately
start mid-day stay safe. The AddItem handler's in_advance immediate
proration clamps the same way (`remainingDays = totalDays` on a
period-start-day add) so both cadences charge the full period.

**Deliberate consequences**:

- A first-period immediate cancel bills from `period_start` (signup
  day counts whole) — a same-day create-then-cancel now bills 1
  day-grade day where it previously billed $0 for a consumed day.
  This also makes the in_arrears cancel amount agree with the
  in_advance cancel-credit twin (both net 1 elapsed day).
- First-invoice usage windows start at `period_start`, so signup-day
  events ingested before the creation instant (explicit past
  timestamps / backfill) now bill; base and usage line periods equal
  the invoice period on every surface.
- An operator AddItem landing on a RENEWAL period's first tenant-TZ
  day bills the full period. **Asymmetry, on purpose**: only `add`
  rows clamp — plan/quantity/remove changes keep timestamp-precise
  numerators per this ADR's "Plan-change proration" consequence.
  Mid-period adds (day 2+) keep instant boundaries (FLOW B20 pin).

**Accepted edges** (documented, not coded around):

- Same-day cancel→recreate on the same meter double-counts the
  morning's usage (and ≤1 day of base when the cancel was post-noon):
  the predecessor's final invoice billed through the cancel instant
  and the successor's day-grade window re-covers the day. Bounded to
  one day; revisit if the wedge makes same-day migration common.
- An org-TZ change between signup and first close can shift the
  calendar-day comparison so the clamp misses and that one sub still
  bills 30/31 (under-bill, ~1/31) — ADR-091 seam class, picked up by
  the deferred billing/display TZ decouple (ADR-092).
