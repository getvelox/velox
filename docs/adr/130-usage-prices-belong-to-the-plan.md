# ADR-130: Usage prices belong to the plan, and a price edit applies from each subscriber's next period

**Date:** 2026-10-23
**Status:** Accepted (product owner, 2026-10-23). PR-0b (#1367) shipped the plan step of the lock order (section 8). PR-1 (#1373) shipped the one rater. PR-2 (#1376) shipped section 2, one aggregation per meter; #1379 amended it on 2026-10-24 (held rows do not fix the aggregation). The rest is not built yet: PR-3 to PR-8 build it before the v0.9.0 tag. Each rule below names the PR that brings it.

## Summary

Today a usage price sits on the meter, so every plan that uses a meter
bills the same rate. After this change each plan has its own usage
prices: Pro can pay $3 and Enterprise $2 per million input tokens, from the
same usage events. The meter only counts.

A price is a row on a plan. It names a meter, the events it takes (for
example `model = claude-opus-4.5, token_type = input`) and the rate. When
several prices could take an event, the one that names the most labels
wins. Two prices that could both take one event with the same number of
labels are refused when you save the second.

Editing a price never changes a period that has already started. The one
exception is measured in milliseconds: a period that starts while the edit
is being saved gets the edit (section 7). On a plan no subscription has
ever used, the edit applies at once. On a used plan,
each subscriber pays the new price from their next billing period. This
holds in each subscriber's own time, so a test-clock customer gets the edit
from its next simulated period. To keep old customers on an old price, put
new customers on a new plan. A customer's negotiated price replaces one
plan price for that customer. Prices take the plan's currency.

Usage on a yearly plan is billed once a year, at the year-end invoice. A
base-fee edit follows the same next-period rule if PR-7 ships; otherwise
the base fee stays locked while a live subscription uses the plan. Recipes
create meters and plans only; they no longer change dunning or webhook
settings.

## Words used here

| Word | Meaning |
|---|---|
| meter | What Velox counts from usage events, such as `tokens`, and how: sum, count, max or last value. |
| plan | What a customer subscribes to: a currency, a billing interval, a base fee and, after this ADR, its usage prices. |
| price | One row on a plan: a meter, a `dimension_match` and a rate. It has a stable id. |
| `dimension_match` | The labels an event must carry for a price to take it, such as `{"model": "claude-opus-4.5", "token_type": "input"}`. Labels are an event's dimensions. Values are strings. `{}` matches every event of the meter. |
| specificity | The number of keys in a `dimension_match`. The matching price with the most keys wins. |
| billing period | The dates one subscription invoice covers: a month or a year. A period normally starts at midnight in the account's billing timezone. |
| pin | The instant whose prices price a whole billing period: the period's start. |
| window | A stretch of a billing period during which a subscription was on one plan. A mid-period plan change splits the period into two windows. |
| used plan | A plan that a subscription has ever referenced, canceled ones included. |
| version | Velox's internal record of a price's content from one instant on. A removal is a version that says "removed from here on" (a tombstone). Versions never appear in the API. |
| test clock | A pretend clock for test customers. Its time is simulated time; real time is wall time. |
| override | A negotiated price for one customer. It replaces the rate of one plan price for that customer. |

## Context

**How pricing works today** (origin/main `c234a364`):

- A usage price is a rating rule with versions (`rating_rule_versions`).
  A meter points at one by default (`meters.rating_rule_version_id`).
  Meter pricing rules (`meter_pricing_rules`) add more rules, each with a
  `priority`, a `dimension_match` and its own aggregation mode.
- A plan lists meters (`plans.meter_ids`). Every plan that lists a meter
  bills that meter's rules. So Pro and Enterprise cannot charge different
  token rates without a second event name.
- A period is priced by the rule versions in force at its start
  ([ADR-070](070-price-change-semantics.md)). The meter's own pricing (its
  rules, default price and aggregation) is read at billing time, so editing
  it repriced the open period. Since D05 (#1362) those edits are refused
  with 409 `meter_in_use` while a draft, trialing or active subscription
  uses the meter.
- A plan's base fee, billing timing and meters are locked while a
  subscription that is not canceled uses the plan
  ([ADR-034](034-plan-billing-field-immutability.md);
  `internal/pricing/service.go:637-647`, a 422 `validation_error` on
  param `plan`). So a plan has two edit rules: rule versions apply next
  period, the base fee is locked.
- Rule versions are stamped with wall time, and so are overrides: nothing on
  the override write path binds the customer's test clock
  (`internal/pricing/handler.go:115` passes the request context;
  `internal/pricing/override.go:259-316`). A test clock compares its
  simulated period start with those wall stamps. A clock moved ahead of
  wall time gets an edit in the simulated period already open; a clock in
  the past waits for wall time. ADR-070 decision 4 and the dashboard
  (`web-v2/src/pages/Pricing.tsx:724`,
  `web-v2/src/components/PriceOverridesCard.tsx:384`) say so.

**What other billing engines do.** In all four open-source engines
checked, a plan owns its usage prices (code read 2026-10-22):

- Lago: a charge belongs to a plan, unique per `(plan_id, code)`
  (lago-api `2e52dfc`, `db/structure.sql:8911`).
- Flexprice: a price has an `entity_type`, and `PLAN` is one of them
  (flexprice `1c23b81`, `internal/types/price.go:39`,
  `internal/api/dto/price.go:20`).
- OpenMeter: rate cards sit in a plan phase (openmeter `f04b561`,
  `openmeter/productcatalog/phase.go:91-95`).
- Kill Bill: usage sections sit in a plan phase (killbill `3548b02`,
  `catalog/src/main/java/org/killbill/billing/catalog/DefaultPlanPhase.java:70-72`).

**Why now.** The strategy amendment of 2026-10-23 moved this change into
v0.9.0, before the first tag, because Velox has no known users or partners.
Changing the pricing contract now costs almost nothing; after the tag it
is a break. v0.9.0 is also a clean break: there is no upgrade from Velox
0.8.x (decided 2026-10-22).

## Decision

### 1. Prices are rows on the plan (PR-4)

- New tables, each with `tenant_id`, `livemode` and row-level security:
  - `plan_prices`: the identity of a price, unique on
    `(tenant_id, plan_id, meter_id, dimension_match)`. Rows are never
    deleted.
  - `plan_price_versions`: a price's content over time (`valid_from`, a
    tombstone flag, the shape fields).
  - `plan_price_version_clocks`: when a version takes effect on one test
    clock (section 7).
- Removed: `rating_rule_versions`, `meter_pricing_rules`,
  `meters.rating_rule_version_id` and `plans.meter_ids`. With them go the
  meter's default price, `priority`, the rule key, the rule lifecycle and
  every rule currency.
- A plan's meters are the meters it has a price row for. A meter only
  counts.
- `dimension_match` values are strings, at most 16 keys, and keys and
  values at most 100 characters.

### 2. A meter has one aggregation (PR-2)

- `aggregation` is one of `sum`, `count`, `max`, `last`. `last` is the
  latest event by timestamp, then by id, on every path. Today two readers
  break that tie differently (`internal/usage/postgres.go:481-486` uses the
  id, `:739` does not).
- `last_ever` is dropped, and so is the per-rule aggregation mode.
- `key` is renamed `event_name`, the name ingest already sends.
- The aggregation cannot change once the meter has events: 409
  `meter_has_usage`. Events are rows in `usage_events` for the meter. The
  meter gains `has_usage` so the dashboard can show this before an edit.
  `name` and `unit` change at once.

**Amendment (2026-10-24, the D08 PR-2 follow-up):** held rows
(`usage_held_events`) do not fix the aggregation; only rows in
`usage_events` do. This replaces the earlier rule that a held row counts.

- Why: a held row has never been billed. Under the earlier rule, a meter
  made with the wrong aggregation after its event name was held could never
  be fixed: there is no meter delete, and `event_name` cannot change. The
  only way out was to discard the rows, which are then never billed.
- Why no billed number changes: a release writes `usage_events`, and each
  insert's foreign key takes the meter row `FOR KEY SHARE`, the lock an
  ingest takes. The aggregation PATCH takes the meter `FOR UPDATE` before
  it reads whether the meter has events. So a PATCH and a release run one
  after the other: the PATCH sees the released events and refuses, or the
  release waits and its events are counted under the new aggregation.
- The release's counts (`in_open_period` and the rest) read the meter's
  aggregation on the release's own transaction, after its inserts, so they
  follow the aggregation its events are counted under.
- A hold takes no meter lock any more, which also removes the race #1376
  listed between a hold and a meter created during it.

### 3. Price shapes and units (PR-3)

- `per_unit`: `{unit_amount_cents}`. It replaces `flat`.
- `graduated`: `{tiers: [{up_to, unit_amount_cents}]}`. `up_to` strictly
  increases, and only the last tier has `up_to: null`.
- `package`: `{package_size, package_amount_cents}`, billed as
  `ceil(quantity / package_size) × package_amount_cents`. A part package
  bills as a whole one. There is no overage field.
- Every money field named `*_cents` is in the currency's minor unit.
  Rates are decimal strings with at most 12 decimal places. Invoice
  amounts stay whole cents, rounded once per line.
- Provider costs follow the same unit rule: `unit_cost_cents` replaces
  `cost_per_token` in dollars, and API cost fields become `*_cost_cents`.

### 4. How an event finds its price (PR-4)

1. The one live subscription that covers the customer and the meter owns
   the event. `rejectMeterOverlap` keeps that to one
   (`internal/subscription/service.go:608`).
2. The window holding the event's timestamp gives the plan
   (`billing_intervals`).
3. The pin is the start of the billing period being priced. It is the same
   for every reader of that period: the close, an immediate cancel, a spend
   threshold invoice, the preview, the live balance and customer usage.
   The pin is read in the subscription's own time: wall time, or its test
   clock's time.
4. Each of the plan's prices for that meter takes its latest version in
   force at the pin. A price with no version then, or whose version is a
   tombstone, takes no part.
5. The candidates are the prices whose `dimension_match` the event's
   dimensions contain.
6. The candidate with the most keys wins. `{}` is the catch-all with 0
   keys.
7. No candidate: the event is stored and not billed (`no_price`, section
   12).
8. An override for the customer and the winning price, in force at the
   pin, replaces that price's rate.

**Ties are refused when a price is saved.** Under a lock on the plan row,
a new or changed price is compared with the prices on the same plan and
meter that take part from now on. Two prices with the same number of keys
and no shared key with different values could both take one event: 409
`price_conflict`, naming the other price. An identical `dimension_match` is
409 `price_exists` while that price takes part; a removed one is added
back (section 6). So the winner for any event is unique: two matching
candidates agree on every shared key, and equal key counts were refused.
A removed price and a later tying price are never in force at one pin,
because versions on a plan take effect in the order their writes commit
(section 7).
The check is D05's compatibility clause without the priority term
(`internal/pricing/postgres.go:1064-1088`).

### 5. The edit rule (PR-4; the base fee in PR-7)

A plan is **used** once any subscription item ever named it, as its plan
or its pending plan, deleted items included, or any `billing_intervals` row
names it.

| What | Never-used plan | Used plan |
|---|---|---|
| Usage price add, edit, remove | Applies at once. The price keeps one version, from the beginning of time; the write replaces every earlier version and its clock stamps. | Applies from each subscriber's next period. The write appends a version that takes effect when it commits (section 7); a removal appends a tombstone and ends the price's overrides (section 6). No period already open changes, except one that starts while the write is saving (section 7). |
| Base fee, if PR-7 ships | Applies at once. | Applies from each subscriber's next period. |
| Base fee, if PR-7 is cut | Editable. | Refused while a live subscription uses the plan, as today (ADR-034). |
| `base_bill_timing` | Editable, but not to `in_advance` at a base of 0 (section 14). | Locked. |
| `code`, `currency`, `billing_interval` | Fixed. | Fixed. |
| `name`, `description`, `tax_code`, `status` | At once. | At once. |
| Meter `aggregation`, `event_name` | Until the meter has events. | Until the meter has events. |
| Meter `name`, `unit` | At once. | At once. |
| Override create, replace, delete | From the customer's next period. | From the customer's next period. |

- Every price write answers with the price and `effective: "now"` or
  `"next_period"`.
- "Used" is conservative on purpose. A plan whose subscribers all canceled
  still waits a period per edit. A test-clock teardown deletes that
  clock's subscriptions, so their plan can become never-used again.
- A price added to a used plan takes part from the next period. Until
  then the plan's other prices bill its usage as before. A less specific
  price that matches, such as `{}`, bills it this period. With no matching
  price the usage is `no_price`. If the meter is new to the plan, the plan
  does not price that meter at the pin, so the usage is `not_on_plan`
  (section 12). There is no exception for a price that only prices
  unpriced usage (D05 allowed one): it would make an ingest flag false
  later.
- A PATCH can set the base fee to 0 (PR-4). Today it cannot: the update
  tests `> 0` (`internal/pricing/service.go:637`, `:673`).
- History stays internal: versions are kept once a plan is used, and every
  price write records an audit row with the old and new content in the
  same transaction as the write
  ([ADR-090](090-audit-in-tx-emission.md)).

The base fee next-period rule in PR-7 uses the same pin. An in_arrears
base and proration pin at the period start. An in_advance base built at a
close pins at the next period start. PR-7 ships in v0.9.0 unless it
cannot be made correct in time. If it is cut, the fallback row above
applies and the docs say so.

### 6. Overrides (PR-4)

- An override is keyed by (customer, price) and takes the price's shape
  plus a `reason`. Usage prices only; a negotiated base fee is a new plan.
- It follows its price across content edits.
- Its window is `[created_at, deactivated_at)`. A create, replace or
  delete stamps it after the write's locks (section 8), in the customer's
  own time: wall time read with `clock_timestamp()`, or the customer's
  clock time plus 1 microsecond. Not `now()`: that is the transaction's
  start, so a write that waited for a lock across a period start would
  change the period already open.
- Removing the price ends it. In the removal's transaction, each open
  override of the price is closed at the instant the tombstone takes
  effect for its customer. For a customer without a test clock that is
  the tombstone's `valid_from`; for a clock customer, the tombstone's
  stamp for its clock (section 7). So at every pin an override is open
  exactly while its price takes part, and the open period keeps the
  negotiated rate. On a never-used plan the
  removal applies at once, so it empties every window of the price's
  overrides.
- Adding back a removed price (the same plan, meter and
  `dimension_match`) appends a version to the same row, so the price keeps
  its id. Its old overrides do not come back.
- An override write takes the plan row `FOR KEY SHARE` and then the price
  row, before it reads the customer's clock (section 8). So it waits for a
  removal in progress. An override write on a removed price is refused
  with 409 `invalid_state`.
- A plan swap does not carry it: the new plan's price is another row.

### 7. Test clocks: every price resolves in the subscription's own time (PR-4)

- On a used plan, a version's `valid_from` is wall time read with
  `clock_timestamp()` after the plan lock, and at least 1 microsecond after
  the plan's latest `valid_from`. So versions on a plan take effect in the
  order their writes commit. Postgres `now()` is not used: it is the
  transaction's start, so of two racing edits the one saved last could get
  the earlier time and never apply.
- The wall time is read before the commit, so a period that starts in the
  milliseconds between the two gets the new version. A reader that priced
  that period in those milliseconds saw the old one. This is the same
  straddle as section 12's `no_price` exception. A clock's time cannot
  move in between, because the write holds the clock `FOR SHARE`.
- On a never-used plan, `valid_from` is the beginning of time.
- A write to a used test-mode plan also writes, for every test clock of
  the account, the clock's current time plus 1 microsecond
  (`plan_price_version_clocks`). It reads the clocks `FOR SHARE` after the
  plan lock, which orders the write against a clock advance
  (`MarkAdvancing`, `internal/testclock/postgres.go:228-231`). The extra
  microsecond keeps the old price for a period that opens exactly at the
  clock's time. Two writes at one clock time get equal stamps; the later
  `valid_from` wins.
- A write to a never-used plan writes no stamps, so its version is in
  force at every pin on every clock. A clock customer's first period
  starts at midnight, before the clock's time; a stamp would leave that
  period with no price. It deletes the stamps of the versions it replaces.
- Every price write and override write reads a clock `FOR SHARE` before
  it writes or deletes that clock's stamp rows or an override of its
  customer (section 8).
- A subscription on clock C compares its simulated pin with C's stamp. A
  version with no stamp for C counts as always in force: it was written
  before C existed, or on a never-used plan.
- One stamp per clock is needed because a clock keeps only its current
  time; its time at a past wall instant cannot be recovered.
- Override windows use the same times (section 6).
- The test-clock caveat of ADR-070 decision 4 is withdrawn, and the
  dashboard's three "applies to the current simulated period" warnings go.
- Teardown locks its clock row first (section 8) and deletes the clock's
  stamp rows. The plan, its prices and versions stay, as plans and meters
  do today
  ([ADR-086](086-simulated-data-lifecycle.md)).

### 8. One lock order (PR-0b shipped; PR-4)

Subscription writers, price writes and override writes take rows in this
order. Once one holds a row of one step, it takes no row of an earlier
step. A test-clock teardown is the one exception (below).

1. the subscription row ([ADR-115](115-one-closer-for-the-billing-period.md));
2. plan rows in ascending id, in one statement: `FOR KEY SHARE` for
   subscription writers and override writes, `FOR UPDATE` for writers of
   plan content;
3. price rows, `FOR KEY SHARE` only; no path locks a price row
   exclusively;
4. meter rows, ascending;
5. test clocks, ascending, `FOR SHARE`;
6. the rest a money path writes: override rows, the tenant's number row
   (`tenant_settings`, which numbers invoices and credit notes) and
   invoice rows.

So an override write takes its plan and its price before it reads the
customer's clock, and a writer takes its plans before it numbers an
invoice or a credit note. Until PR-4, D05's meter edit takes its meter and
then plans; no writer takes a plan and then a meter before PR-4, so this
makes no cycle.

**A test-clock teardown is outside this order.** It deletes bottom-up for
foreign keys: invoice rows before the subscription row, override rows
before the clock row (`internal/testclock/postgres.go:150-221`). From PR-4,
its first lock is its clock row, `FOR UPDATE`. A price write or override
write reads that clock `FOR SHARE` before it touches a row teardown
deletes: the clock's stamp rows or an override of its customer (section
7). So one of the two waits for the other at the clock row, and they make
no cycle. Teardown takes no plan or price row. Without the clock lock
first, a removal and a teardown deadlock: the removal holds the clock and
waits for an override row, and the teardown holds that override row and
waits to delete the clock. A real-Postgres model aborted one with `40P01`;
with the clock lock first, both committed. An override write on the
clock's customer deadlocks with a teardown the same way, and the clock lock
first fixes it too. A never-used write that deleted the clock's stamps
without reading the clock deadlocked even with a clock-first teardown;
reading the clock first fixed it.

PR-0b (#1367, merged as `c234a364`) brought step 2 to every subscription
writer. It fixed a deadlock it reproduced on main. D05's meter edit takes
plans in ascending id. An immediate swap from plan A to plan B took B and
then A through foreign keys. With id(A) < id(B), Postgres aborted one of
them with `40P01`. It also found a cycle through the number row: an
activation holds plan B and waits for the number row. So a swap takes its
plans before it numbers its refund credit note
(`internal/subscription/postgres.go:2084-2097`). PR-4 deletes D05's plan
lock with the rule tables.

### 9. Windows and the spend threshold invoice (PR-4)

- Usage windows are per (plan, meter), not per meter. After a mid-period
  swap between two plans that price one meter, each window is priced by
  its own plan, as two lines. Tiers restart in each window and each line
  rounds on its own, as ADR-066 already does for separate windows.
- Stripe's flexible billing mode prices a mid-cycle metered price swap the
  same way. Stripe's docs: "When you swap a metered price mid-cycle, we
  preserve the rate that was active when the usage occurred: Pre-swap usage
  is rated at the old price … Post-swap usage is rated at the new price"
  ([manage usage-based billing setup](https://docs.stripe.com/billing/subscriptions/usage-based/manage-billing-setup),
  read 2026-10-23). That is: each side of the swap is priced by the price
  it was on.
- A spend threshold invoice prices the close's windows. Today it prices
  one flat window with the current items' plans
  (`internal/billing/threshold_scan.go:509-510`), which would price
  pre-swap usage at the new plan's rates.
- A graduated price's ladder continues across a threshold invoice keyed by
  price id ([ADR-066](066-threshold-money-semantics.md) amendment
  2026-10-11). Peak and last-value deferral keys on (meter, price): with
  one aggregation per meter, "not additive" is a property of the meter.

### 10. Customer usage, the cost dashboard and margin (PR-4)

- **Current period: exact.** Each subscription is priced by the close's
  own rater over its windows, floors, threshold watermark and ladder. The
  customer page then matches the invoice and the live balance.
- **An explicit `?from=&to=` window: an estimate,** labelled so. Each
  subscription's windows are clipped to it and priced at its current
  period's pin. Past periods are not priced at their own pins. Nothing
  bills from this read.
- Margin reads the explicit-window estimate and attributes revenue by the
  winning price's `dimension_match.model`.
- The public cost dashboard reads the current period: exact.
- Rows are `meters[].prices[]` with `plan_id`, because two plans can
  price one meter differently.

### 11. Currency (PR-4)

A price and an override have no currency; they take the plan's. The
invoice currency order is unchanged (billing profile, then plan). The
currency guards that compared rule currencies go: G1, G2, G2b, the override
currency clause and the recipe meter probe of
[ADR-100](100-currency-coherence-guard-ring.md). G3 (the customer currency
pin), G5 to G8 and ADR-129 stay.

### 12. Ingest says when an event matched no price (PR-6)

- The ingest answer's `billing_status` is omitted when an open period will
  bill the event. Otherwise it is one of:
  - `after_close`: shipped ([ADR-120](120-invoice-grace-period-for-late-usage.md));
  - `not_on_plan` and `outside_subscription`: the D07 change, sequenced
    with PR-6;
  - `no_price`: the subscription's plan prices the meter at the pin, but
    no price matches the event's dimensions. New here.
- `/batch` and the LiteLLM callback count each status. Held-usage release
  gains `no_price` beside `not_on_plan`.
- One counter, `velox_usage_unbilled_total{reason, origin}`, counts every
  status. It replaces ADR-120's `velox_usage_after_close_total{origin}`,
  which becomes `reason="after_close"`.
- Ingest pins at the open period's start, and at its end for an event in
  the grace after it.
- Matching runs in Go. With string values, per-key equality is the same as
  Postgres `@>`; a real-Postgres test checks the two agree.
- **One stated exception to "a flag is never false"** (ADR-120): a price
  write whose transaction spans a period start, and commits after an
  ingest read, can make one `no_price` flag wrong. Closing it needs a lock
  on the plan per ingested event, at the 1,000 to 12,000 events per second
  measured in [scale](../scale.md). Rejected.

### 13. Yearly plans

Usage on a yearly plan is billed once a year, at the year-end invoice, as
today: a subscription has one billing period, and the close reads usage
over it (`internal/billing/engine.go:2789`, `:2899`).

- A spend threshold (`amount_gte`) bills early when usage runs high.
- For monthly usage beside a yearly fee, use two subscriptions: a yearly
  plan with no prices and a monthly plan with the usage prices. Their
  meters are disjoint, so the overlap guard allows it. The pricing guide
  says so (PR-8).
- Not now: a usage interval of its own per plan. One period per
  subscription is assumed by the closer, the threshold watermark, the
  after_close rule, credit expiry and test clocks. Trigger: a partner, or
  two evaluators, selling a yearly base fee with monthly usage on one
  subscription.

### 14. `base_bill_timing` (PR-4)

`base_bill_timing` is required when `base_amount_cents` is above 0, and the
422 names both values. With a base of 0 it may be left out and is stored
`in_arrears`. A write that would leave a plan `in_advance` with a base of
0 is refused: a create, a base edit to 0 on an `in_advance` plan, and a
`base_bill_timing` edit to `in_advance` on a plan whose base is 0. Today
the field defaults to `in_arrears` (`internal/pricing/service.go:544-547`). An in_advance plan moves the cycle
invoice's header to the next period even at base 0
(`internal/billing/engine.go:3350-3371`), so a usage-only plan must be
in_arrears.

### 15. Recipes (PR-4, PR-5)

- PR-4: the recipe parser turns the current YAML into plan prices, so
  install keeps working.
- PR-5: the YAML holds only `meters` and `plans` with their `prices`.
  `rating_rules`, `pricing_rules`, `priority` and per-price `currency` go.
- An apply always creates a new plan with a free code, as today
  ([ADR-085](085-recipe-idempotent-apply.md)). From PR-4 it reuses a meter
  by `event_name` only when the aggregation matches, read inside the
  apply's transaction (today it is read on its own connection,
  `internal/recipe/service.go:422`). The plan's currency is the recipe's.
- PR-5: recipes no longer create a dunning policy or a webhook endpoint.
  Their `dunning` and `webhook` sections are removed, and the parser
  refuses them.

### 16. API (PR-4)

- `POST /v1/plans` takes `prices[]` and creates the plan and its prices in
  one transaction. `GET /v1/plans/{id}` returns `prices[]` and `used`.
- `/v1/plans/{plan_id}/prices`: GET, POST, PATCH `/{id}` (content only;
  meter and `dimension_match` are the identity) and DELETE `/{id}`. A POST
  that names a removed price's meter and `dimension_match` adds it back
  under the same id.
- `/v1/price-overrides` keys on `price_id`.
- Invoice and preview lines carry `price_id` and `dimension_match`.
  `pricing_mode` is `per_unit`, `graduated` or `package`.
- Removed: `/v1/rating-rules`, `/v1/meters/{meter_id}/pricing-rules`,
  `/v1/meter-pricing-rules`, `Meter.rating_rule_version_id`,
  `Plan.meter_ids`, `rule_key` and the `meter_in_use` code.

### 17. No data migration

There is no upgrade from Velox 0.8.x, so this ADR moves no data, replays no
old invoices and keeps no list of old rows to refuse. Each migration is
plain DDL with a working Down. One that drops an old pricing table first
checks it is empty, and stops with "install fresh" if not. PR-1's new rater
is proved inside one build: the old rating loops and the new rater run
over the existing billing fixtures and must give identical lines.

### 18. No index for the meter-has-events check

The aggregation lock asks whether `usage_events` holds any row for the
meter (section 2; held rows stopped counting on 2026-10-24).
No index on `usage_events` starts with `(tenant_id, meter_id)`.
The nearest are `(tenant_id, customer_id, meter_id, timestamp)` and
`(tenant_id, timestamp)` (baseline `00180`, `:3048`, `:3062`). No index is
added now: the edit is rare. Revisit if a large account reports a slow
meter PATCH.

## Why this design

- **Money.** One pin per period means no edit reprices usage already
  recorded. The close, the threshold invoice, the preview and the live
  balance share one rater (PR-1), so a preview equals the invoice.
- **One edit rule.** Usage prices, the base fee (with PR-7) and overrides
  all apply from the next period on a used plan.
- **Fewer concepts.** One noun, price. No priority, no meter default, no
  rule currency, no rule versions in the API, no per-rule aggregation.
- **Stable ids.** A price keeps its id across edits, so an integration
  stores one id. Versions stay internal.
- **A deterministic winner.** Refusing ties at write time means the winner
  for an event never depends on which price is older.

## Alternatives considered

- **A. Immutable prices: a new price id per rate change, as Stripe does.**
  Stripe says "you can't change a price's amount in the API. Instead, we
  recommend creating a new price for the new amount, switch to the new
  price's ID, then update the old price to be inactive"
  ([manage prices](https://docs.stripe.com/products-prices/manage-prices),
  read 2026-10-23). That is: Stripe never edits a price, and moving
  subscriptions to the new price is the integrator's job. Velox moves
  every subscriber at their next period by itself, so copying Stripe's
  identity rule would not give Stripe's behaviour. Rejected: two
  identities and a replace verb buy no money safety beyond internal
  versions. The design that proposed it also had the money breaks in the
  [panel record](#panel-record).
- **B. Keep the meter's default price as a fallback under per-plan
  prices.** The smallest change. Rejected: two ways to price one meter, and
  `priority` stays.
- **C. Edit in place and refresh open drafts, as Lago does.** Lago updates
  an existing charge by id (lago-api `2e52dfc`,
  `app/services/plans/update_service.rb:250-256`) and flags draft invoices
  of the plan's subscriptions for refresh (`:84`, `:101-125`). Rejected: an
  edit then reaches usage already recorded in the open period.
- **D. Break ties by age, as Lago does.** Lago picks the filter with the
  most keys, then the fewest allowed values, then the oldest
  (`app/models/charge_filter.rb:81-90`, picked with `min_by` in
  `app/services/events/billing_period_filters/event_matching_service.rb:19`).
  Rejected: the winner then changes without notice when a price is
  removed and added again.

## Panel record

Two designs were written and attacked by one adversarial pass: "plan-rows"
(prices as rows on the plan) and "immutable-prices" (a new price per rate
change). Plan-rows was chosen with six changes. How each rejected design or
part breaks:

**Immutable-prices** (rejected):

- **Pin at the close's commit time.** It pinned a period at
  `prices_as_of`, the wall time the close committed, which is after the
  grace. An edit at 00:30 on the 1st, before a 01:00 close, would reprice
  the 30 minutes of usage already recorded.
- **An overlap lock that did not hold.** Its check locked other plans in
  `FindMeterConflicts`, which opens its own transaction
  (`internal/subscription/postgres.go:2601-2609`). The lock ended before
  the subscription insert, so the race stayed open.
- **A flat threshold window.** It kept the threshold invoice's one flat
  window priced with the current plans, so after a swap pre-swap usage
  would bill at the new plan's rates.
- **More contract.** Two identities (price id and slot), a `replace` verb
  with its own 409, derived `replaced_by` fields and a new subscription
  column.
- **Two edit rules.** The base fee stayed locked while usage prices moved
  next period.

**Parts of plan-rows that were dropped:**

- **A second pin rule.** The first period pinned at
  `max(period start, activated_at)`. Two pin rules for one engine, and a
  different version than the period start for a first period.
- **"Used" as live users only.** A plan whose only subscriber canceled
  would have its history rewritten in place, so `invoice_line_items.price_id`
  would point at terms other than those billed.
- **Closing overrides at wall now on removal.** For a test-clock customer
  ahead of wall time, that ends the override before its open period's pin,
  so the open period loses the negotiated price. A removal closes them at
  the instant its tombstone takes effect for each customer instead
  (section 6).
- **Numeric dimension values.** Normalized as float64 in Go but compared
  exactly in Postgres, so the ingest flag and the close could disagree on
  large numbers. Values are strings only.
- **Recipes adopting a plan by code**, with a 409 on any difference. It
  contradicts ADR-085's fresh plan per apply and adds an error path.
- **A `plan_meters` view with a coverage end.** Approximate: after a
  removal it could under-bill. A plan's meters are every meter it has
  ever priced.
- **One large stack.** The store, engine and API changes had to merge
  together. The PR order now ships each step alone.

Kept as a stated residual: adding a price for a new meter to a used plan
is check-then-act against a subscription created on another plan at the
same moment (Risks).

The breaks that concerned moving 0.8.x data went with the clean break.

## Consequences

### Positive

- Two plans can charge different rates for the same meter.
  Grandfathering an old price is a new plan.
- One edit rule, shown on the plan page before any edit.
- Removed: `priority`, the meter's default price, rule currencies, rule
  versions in the API, the per-rule aggregation and `last_ever`, the
  package overage field, and the currency guards that compared rules.
- The close, cancel, preview and live balance share one rater instead of
  separate rating loops.
- Test-clock customers see price edits as live customers do.
- The ingest answer says when usage matched no price.

### Risks / open items

- **Test clocks cost one stamp row per clock per test-mode price write.**
  An account with 1,000 clocks writes 1,000 rows per edit. Test mode only.
- **The commit straddle, measured in milliseconds.** A price write's wall
  time is read before it commits. A period that starts in between gets the
  write (section 7), and one ingest `no_price` flag can be wrong (section
  12).
- **A test-clock teardown is outside the lock order.** It takes its clock
  row first, which orders it against price and override writes (section
  8).
- **Price add against a concurrent subscription create is check-then-act.**
  A price for a meter new to a used plan runs the overlap check for every
  customer on the plan (409 `meter_overlap`). A subscription created on
  another plan at the same moment is not ordered with it. The same class
  as two concurrent subscription creates today; it joins ADR-100's
  deferred locks. Trigger: the first account with several operators.
- **A new subscriber later on the day of an edit** pays the old price for
  the first period, because the pin is the period start, a midnight.
  (A spend threshold with `reset_billing_cycle=true` used to start a period
  at the invoice's own instant. *Removed 2026-10-23 in its own PR (#1371): a
  threshold invoice no longer starts a period, so PR-4 carries no reset
  re-anchor and no reset base.*)
- **After a mid-period swap** between two plans that price one meter, the
  period bills two lines, and tiers restart in the second window.
- **The threshold invoice becomes window-aware:** one line per plan window
  instead of one flat line.
- **Overrides do not follow a swap.** The operator creates the override
  again on the new plan's price.
- **A raw model-name price needs one more key** than the price it refines,
  because priority is gone. For example
  `{model, model_raw, token_type}` beats the recipe's `{model, token_type}`;
  `{model_raw, token_type}` beside it is 409 `price_conflict` (PR-5).
- **Customer usage over an explicit window is an estimate** at each
  subscription's current pin. Display only.
- **The meter-has-events check can scan** an account's time index (section
  18).
- **PR-7 may be cut.** Then the base fee stays locked while a live
  subscription uses the plan, as in ADR-034.
- **Monthly usage on a yearly plan needs two subscriptions** until a usage
  interval per plan ships (section 13).
- **No upgrade path from 0.8.x.** No 0.8.x install is known.

## ADRs this changes

None of these changes is built yet, except where a row says so. Each ADR's status line links here,
and a banner under it names the part that changes and the PR that changes
it.

| ADR | What ADR-130 changes |
|---|---|
| [031](031-per-plan-base-bill-timing.md) | `base_bill_timing` no longer defaults to `in_arrears` when the base fee is above 0; in_advance with a base of 0 is refused (PR-4). |
| [034](034-plan-billing-field-immutability.md) | The meter-set lock goes with `meter_ids` (PR-4). The base-fee lock becomes next-period edits (PR-7), or stays if PR-7 is cut. The `base_bill_timing` lock holds once a plan is used (PR-4). |
| [044](044-canonical-ai-token-metering-model.md) | Decision 4: token prices are plan prices on the `tokens` meter, most specific wins, no priority (PR-4). |
| [045](045-decimal-per-unit-pricing-rates.md) | `package_amount_cents` becomes a decimal too, `flat` becomes `per_unit` and overage goes (PR-3). Rates take at most 12 decimal places: built in #1378, ahead of PR-3. |
| [066](066-threshold-money-semantics.md) | Decision 4's rule-bucket granularity becomes the meter (PR-2). The ladder seed keys on price id, so the ambiguous-config failure goes (PR-4). The package clamp goes (PR-3). The fire prices per plan window (PR-4). |
| [070](070-price-change-semantics.md) | Decision 1: overrides key on the price, not `rule_key`. Decision 4's earliest-version fallback and test-clock caveat go. Decision 5's currency guard goes. The 2026-10-23 meter-in-use amendment is replaced. Customer usage over an explicit window is priced at each subscription's current pin (section 10). All in PR-4. The period-start pin stays. |
| [085](085-recipe-idempotent-apply.md) | Rule adoption, the rule conformance check, the currency taken from adopted rules and the 2026-10-23 amendment go (PR-4); recipes no longer create a dunning policy or webhook endpoint (PR-5). |
| [096](096-unpriced-meter-plan-attach-guard.md) | The plan-attach guard and the meter-in-use guard that replaced the last-price guard go: a plan's meters are its prices' meters (PR-4). The WARN and counter stay; ingest adds `no_price` (PR-6). |
| [100](100-currency-coherence-guard-ring.md) | G1, G2, G2b, the override currency clause and G4's recipe probe go; prices take the plan's currency (PR-4). |
| [120](120-invoice-grace-period-for-late-usage.md) | The after_close rule reads the meter's aggregation instead of rule buckets and drops the `last_ever` case (PR-2, PR-4). `no_price` adds one stated exception to "a flag is never false" (PR-6). The after_close counter becomes `velox_usage_unbilled_total{reason="after_close"}` (PR-6). |
| [123](123-new-accounts-start-with-a-default-dunning-policy.md) | A recipe no longer creates a dunning policy, so only an operator's first enabled policy takes over the seeded default (PR-5). |

## References

- ADR-031, ADR-034, ADR-044, ADR-045, ADR-066, ADR-070, ADR-085,
  ADR-086, ADR-096, ADR-100, ADR-101, ADR-115, ADR-120, ADR-123, ADR-129.
- PR-0b: getvelox/velox#1367, merged as `c234a364` (the lock order).
- Migrations: PR-2 is `00208_meter_one_aggregation.sql` with its CHECK
  validated in `00209`; one each for PR-3, PR-4 and PR-7, numbered when each
  PR opens.
- Design of record: velox-ops `design-plan-pricing-2026-10-23.md`
  (sections 4.18 for the PR split, 2 for the panel).
- Stripe, read 2026-10-23:
  [Price object](https://docs.stripe.com/api/prices/object)
  (`unit_amount_decimal`: "a decimal string with at most 12 decimal
  places"),
  [manage prices](https://docs.stripe.com/products-prices/manage-prices)
  (package pricing: "Purchases are rounded up by default"),
  [manage usage-based billing setup](https://docs.stripe.com/billing/subscriptions/usage-based/manage-billing-setup).
- Peer code read 2026-10-22: lago-api `2e52dfc`, flexprice `1c23b81`,
  openmeter `f04b561`, killbill `3548b02`. Package rounds up in Lago
  (`app/services/charge_models/package_service.rb:12`) and OpenMeter
  (`openmeter/billing/rating/service/rate/package.go:55-63`). An open last
  tier is `null` in OpenMeter (`openmeter/productcatalog/price.go:723-725`).
  OpenMeter's meter update takes no aggregation
  (`openmeter/meter/service.go:213-221`). `base_bill_timing` defaults
  differ: Lago `pay_in_advance DEFAULT false` (`db/structure.sql:4056`),
  Flexprice `invoice_cadence` required (`internal/api/dto/price.go:30`),
  OpenMeter `DefaultPaymentTerm = InAdvancePaymentTerm`
  (`openmeter/productcatalog/price.go:17`).
