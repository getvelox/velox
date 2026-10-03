# ADR-058: Billing calendar date-math is anchored in the tenant timezone

- Status: Accepted
- Date: 2026-06-09
- Relates: ADR-042 (integer day-ratio proration), ADR-030 (simulated time / clock-pinned), the period-creation + proration paths

## Summary

All month and year billing advances are computed in the tenant timezone. That covers period boundaries, renewal dates and the full-cycle day count used to prorate. Before this ADR, `AddDate` ran on UTC or host-local times, so tenants outside UTC got boundaries a day or more off and small over- or undercharges on mid-cycle changes. The ADR also fixed a separate bug in calendar billing: an anchor on the 29th to 31st skipped February. The fix snaps to the 1st of the month before advancing. The original fix sent every advance through one helper, `domain.addIntervalIn`. Day-based adds such as trial end and due dates do not need the timezone, so they skip it, and with no tenant timezone configured the math uses UTC. ADR-055 (2026-06-18) overturned this ADR's claim that `Jan 31 + 1 month = Mar 3` was acceptable. Anniversary monthly and yearly subscriptions now store a `billing_anchor_day` and clamp a 29th to 31st (or Feb 29) anchor to the month's last day, then return to the anchor day in longer months. They still compute in the tenant timezone, but through `advanceAnchored`, so `addIntervalIn` now handles only calendar billing and rows with no stored anchor day. Invoices and the subscription period-range displays show the inclusive last covered day, derived at read time from the stored `[start, end)` boundary. Event dates such as Renews and Cancels stay on the exclusive boundary.

## Context

Subscription period boundaries and the proration denominator are computed with `time.Time.AddDate` (advance one month/year). `AddDate` is **timezone-sensitive**: it operates on the *wall-clock calendar date* in the value's `Location`. Two facts made this silently wrong:

1. **pgx/v5 stdlib scans `timestamptz` as `time.Local`** (the host process timezone), not UTC. So a `period_start` stored as `2026-05-31T18:30:00Z` reads back as `2026-06-01 00:00 +05:30` on an `Asia/Kolkata` host — calendar date **June 1**, not May 31.
2. **The advance helpers ran `AddDate` on the value's ambient Location**, not the tenant's billing timezone:
   - `NextBillingPeriodEnd` (period boundaries) handled this correctly in its **calendar** branch (it added the month `In(loc)`), but the **anniversary** and **yearly** branches did a bare `AddDate` — ignoring the `loc` they were handed.
   - `AddBillingInterval` (the proration denominator, via `fullBillingCycleDays`/`advanceBillingPeriod`) took **no `loc` at all**.

**Net effect for any tenant in a non-UTC timezone:** every month/year advance lands one calendar day off whenever the UTC instant maps to a different calendar date in the tenant zone (month-end + offset). The audit found this is **systematic across the whole billing surface**, not a one-off.

### The motivating case (verified against the live DB + empirical replication)

An operator started an anniversary-monthly sub on **June 1** (IST). Stored: `period_start = 2026-05-31T18:30:00Z`. The anniversary branch did `AddDate(0,1,0)` on the **UTC** date *May 31* → overflowed the non-existent June 31 → **July 1 18:30 UTC** = a **31-day** first period (`Jun 1 → Jul 2` in IST), when the correct June anniversary is **30 days** (`Jun 1 → Jul 1`). A mid-cycle upgrade then divided `remaining/30` (denominator computed in IST via `time.Local`) against a numerator measured to the 31-day stored boundary, overcharging ~3.3%.

Worse cases verified: a Jan-31 anniversary on an east-of-UTC tenant lands 3 days off; calendar Jan-31 silently *skips February*; yearly leap anchors land a day early.

### Two distinct defects

- **Root A/B (the class, ~13 sites):** calendar `AddDate` run on a non-tenant-anchored `time.Time` — the same defect whether the value was freshly built in UTC or DB-scanned as `time.Local`. `NextBillingPeriodEnd` anniversary/yearly branches + `AddBillingInterval` and all its proration-denominator callers.
- **Root C (independent, 1 site):** `NextBillingPeriodEnd`'s calendar branch added the month *before* snapping to the 1st, so a day-29/30/31 anchor overflowed a short month and skipped a whole calendar month (`Jan 31 + 1mo = Mar 3` in Go's normalization → snapped to `Mar 1`, losing February). TZ-agnostic.

**~~Explicitly NOT a bug, do not "fix" it:~~ SUPERSEDED by ADR-055 (2026-06-18).** This ADR claimed Go's `AddDate` overflow (`Jan 31 + 1mo → Mar 3`) was acceptable "anniversary month-end behavior" and declined a clamp. That was wrong: an adversarial audit showed the advance is *self-referential* (it adds onto the previously-computed, already-drifted boundary), so a day-29/30/31 anchor **ratchets** off month-end permanently (`Jan 31 → Mar 3 → Apr 3 → …`) and diverges from Stripe/Chargebee/Lago, which clamp to the month's last day and restore (`Jan 31 → Feb 28 → Mar 31 → …`). **ADR-055 adds a persisted `billing_anchor_day` and a month-end clamp** for anniversary + yearly. (The timezone-offset and calendar snap-order fixes in *this* ADR remain correct and unchanged.)

## Decision

**All month/year billing advances are anchored in the tenant timezone**, via one shared helper, so the result depends only on the tenant `loc` — never on the input's ambient `Location` or the host `time.Local`.

1. New `domain.addIntervalIn(t, interval, loc)`: re-projects `t` into `loc`, `AddDate`s, returns a UTC instant. `NextBillingPeriodEnd` (all branches) and `AddBillingInterval` route through it. (Root A/B.)
2. `AddBillingInterval` gains a `loc` parameter; threaded through `advanceBillingPeriod` (engine) and `fullBillingCycleDays` (subscription proration). The subscription handler gets a `TenantLocator` dependency (`*billing.Engine.TenantLocation`) so its proration denominator anchors in the same zone the engine writes boundaries in.
3. `NextBillingPeriodEnd`'s calendar branch snaps to first-of-month **before** advancing: `BeginningOfMonthIn(periodEnd, loc).AddDate(0,1,0)`. (Root C.)

Day-grade adds (`AddDate(0,0,N)` for trial end, due dates, net-payment terms) are TZ-invariant — a 24h×N shift on an instant — and are deliberately **not** routed through the helper.

When the tenant timezone is unconfigured the helpers fall back to UTC (the prior behavior for UTC tenants; only offset-TZ tenants change).

## Why not the alternatives

- **Pin everything to UTC** — would make the proration agree with the *buggy* 31-day period and contradict the operator's intent (they picked June 1 in their calendar). The boundaries themselves must be tenant-anchored; UTC was the wrong direction.
- **Use the stored period length as the denominator** instead of recomputing the cycle — breaks the deliberate stub-period full-cycle denominator (ADR-042): a mid-cycle signup's current period is shorter than a cycle, and dividing by it over-charges. Rejected.
- **Normalize all DB-read `timestamptz` to UTC at the scan layer** — addresses provenance but not the core requirement that the *advance* happen in the tenant zone; the helper makes results provenance-independent anyway, so the scan-normalization would be redundant belt-and-suspenders. Not added.

## Consequences

- Period boundaries, renewal anchors, and the proration denominator are now **host-TZ-independent** and mutually consistent — the same sub prorates identically on a UTC CI runner and an IST production host.
- Fixes a systematic small-cents over/under-charge on every mid-cycle upgrade/downgrade/cancel/swap for offset-TZ tenants, plus day-drifted renewal anchors and the Feb-skip calendar bug.
- One existing test (`TestPeriod_DayGradeSnap`) asserted the *buggy* UTC-computed anniversary end; corrected to the tenant-anchored value.
- New regression tests assert **provenance-independence** (same instant, UTC- vs Local-located, yields the same tenant-anchored result) and the month-end / leap cases — the existing suite built every input in UTC and was blind to the class.

## Follow-up: inclusive-last-day display convention (shipped)

A common render convention — Stripe, Zuora, and Lago (Stripe's API even labels invoice-line `period.end` "inclusive") — is to *show* the **inclusive last covered day** ("Jun 1 – Jun 30") while storing the half-open `[start, end)` boundary. It is **not universal**: Chargebee and Recurly render the exclusive boundary ("Jun 1 – Jul 1"). Velox previously showed the exclusive end too — which is what made this very off-by-one hard to spot on the invoice. (Resolved 2026-06-10: the earlier "all five render inclusive" claim was overstated — the data-model timestamp is universally exclusive; the *rendered* display splits ~3-of-5 inclusive.)

Shipped for the **invoice** period: `domain.FormatInclusivePeriod(start, end, loc)` renders the inclusive last day, date-only, in the tenant TZ (snap end to civil midnight in `loc`, then step back one CALENDAR day — never a 24h instant subtraction, the same trap as the period math above). The invoice read path sets the computed `billing_period_display` string; the PDF / hosted / portal paths (which fetch via `GetByPublicToken`, bypassing the read decorator) author it from the *same* helper — so PDF, hosted, dashboard, and list all show one identical string, no cross-runtime drift. Raw half-open `billing_period_start/end` stay unchanged (SDK contract). One-off / no-period invoices omit the period.

Shipped for the **subscription** surfaces (2026-06-09): the period *coverage* displays — SubscriptionDetail's current-period range + its timeline "Period End" dot, PlanDetail's subscriptions table, the cost-dashboard cycle bar, and the invoice line-item "Covers <range>" — now render the inclusive last day via two TS helpers in `web-v2/src/lib/dates.ts`, `formatCivilDate` / `formatCivilPeriod`, that mirror `domain.InclusiveDisplayEnd` / `FormatInclusivePeriod` (the canonical spec — kept byte-for-byte). *Event* dates stay on the exclusive instant: "Renews"/"Cancels"/"Ended"/"Next billing"/"keeps access until"/Portal's relative "Period ends in N days" each fire **on** the boundary, so the day-before would be wrong. Trial dates are out of scope.

### Why the subscription surfaces compute client-side, but the invoice doesn't

This is the same question two ways — "store the inclusive day in the DB?" and "why a TS helper instead of a backend field like the invoice?" — recorded here so it isn't re-litigated.

- **Don't persist the inclusive day.** The half-open `[start, end)` instant pair is the canonical fact: absolute, gap-free (`nextStart == thisEnd`), instant-precise, and the shape every range/usage query already assumes. The "inclusive last covered day" is a *projection* of it — tenant-TZ-dependent and only date-granular. Persisting it would (a) duplicate the instant as a denormalized cache that can silently disagree if any write path updates one and not the other (the failure class we refuse for money), (b) bake in a timezone at write time that goes stale the moment the tenant changes TZ (the instant stays correct; the stored day doesn't), and (c) push the subtle ADR-058 calendar math into *every* period-writing path. A Postgres generated column can't do it either (not `IMMUTABLE` — the tenant TZ lives in another table); a view would just reimplement the calendar math in a third language. So: store half-open, derive the day at read.
- **Derive it where the renderer already has the instants.** The invoice carries a backend `billing_period_display` because its primary surface is a **server-rendered PDF** with no client to compute anything — and authoring it once server-side also covers the hosted/portal paths that bypass the read decorator. The subscription surfaces are all **client-rendered React** that already receive the raw half-open `current_billing_period_start/end` over the wire, so the inclusive day is a pure render-time function of data already present. A backend field there would mean a new computed API field threaded through every subscription read path (several of which bypass the decorated service — usage reads the raw store), i.e. a lot of plumbing to ship a string the client can compute in four lines. The cost of the client path is one duplicated implementation of the calendar step — bounded to one `dates.ts` module, mirroring the Go spec, and pinned by the Go `TestFormatInclusivePeriod`. (web-v2 has no test runner yet; the TS mirror was verified against the Go edge-case table via a throwaway parity script — a committed FE guard via vitest is a flagged follow-up.)
