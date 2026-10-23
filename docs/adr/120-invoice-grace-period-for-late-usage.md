# ADR-120: A period's invoice is built after a grace period, and late usage after it is not billed automatically

**Date:** 2026-10-14
**Status:** Accepted (amended 2026-10-19: credit expiry no longer uses the grace; amended 2026-10-22: dunning's final pause and cancel wait for the grace; amended 2026-10-22: late usage after the invoice is flagged); amended by [ADR-130](130-usage-prices-belong-to-the-plan.md) (after_close reads the meter's aggregation, built in PR-2; a `no_price` status and one unbilled counter, not built yet)

**Amended by [ADR-130](130-usage-prices-belong-to-the-plan.md)**, decided 2026-10-23. Until each part is built, this ADR describes the code. In the after_close rule, "every bucket of the meter is sum or count" becomes "the meter's aggregation is sum or count", and the `last_ever` case goes (PR-2, built in #1376: no meter has a `last_ever` rule any more, so the bullet on it below no longer applies). "Prices its meter" is read from the plan's prices at the period's start (PR-4). A new `no_price` status brings one stated exception to "a flag is never false": a price write that spans a period start can make one `no_price` flag wrong (PR-6). Its counter `velox_usage_after_close_total{origin}` becomes `velox_usage_unbilled_total{reason="after_close", origin}`, one counter for every unbilled status (PR-6). The grace and the after_close flag stay.

## Summary

Velox builds a billing period's invoice once `INVOICE_GRACE_PERIOD` has
passed after the period ends. The default is 1 hour, the range is 0 to 72
hours, and 0 builds it as soon as the period ends. Usage stamped inside the
period that arrives within the grace is on the invoice, unless a
spend-threshold invoice was built after its timestamp (see Risks).

Once the invoice is built it does not change. Usage for that period that
arrives later is stored but billed on no invoice, now or later; the operator
can bill it with a one-off invoice. Velox does not refuse late usage, and
since 2026-10-22 it says so on the answer: `billing_status: after_close`
(see the amendment).

The grace delays every period-end invoice, flat-fee ones too, and the final
invoice of a cancel scheduled for a date. The invoice's dates and amounts do
not move; only when it is built, and what follows it, move later.

Actions that close the ended period at once, with no grace: an immediate
cancel, any other subscription write through the API or the dashboard,
`retry-billing`, a spend-threshold invoice and a test-clock advance.
Dunning's final pause or cancel waits for the grace (amended 2026-10-22).

## Context

Usage reaches a billing system some time after it happens: pipelines batch,
retry and replay. Issue #961 asked what Velox does with usage that arrives
after its period has ended.

Before this change the answer was luck. The hourly billing run closed a
period on its first tick after the boundary, which could be one second after
it. A replica that booted or took over the leader lease closed every due
period on its first poll. `POST /v1/billing/run` closed at any instant, and a
catch-up after downtime closed the newest period with no delay at all. The
usage read happens at close time, so whatever had arrived by then was billed
and the rest was not.

#1038 and #1039 first made the gap between a period end and its invoice safe
to lengthen. A subscription change in the gap now closes the ended period
first. `last_ever` values are read as of the period end, and a pause is read
as it stood at each period end. This ADR then makes the gap a fixed setting.

What other billing products do (all quoted from their docs):

| Product | Grace before the invoice is built | After it is built |
|---|---|---|
| [Stripe](https://docs.stripe.com/billing/subscriptions/usage-based/configure-grace-period) | "All invoices have a default finalisation grace period of 1 hour." "You can set a delay of up to 72 hours (3 days)." | "Stripe might accept an event after finalisation and include it in meter summaries without updating the finalised invoice or adding the usage to the next invoice." |
| [Orb](https://docs.withorb.com/invoicing/invoice-issuance) | "12 hours after the invoice period ends" | "Once the invoice is issued, its values cannot be changed." |
| [Metronome](https://docs.metronome.com/invoice-customers/how-invoicing-works/) | "The grace period is 24 hours by default." "This buffer period allows for late-arriving usage data and invoice corrections." | "no further changes can be made, even if additional usage is reported for the period." |
| [Chargebee](https://apidocs.chargebee.com/docs/api/usages.md) | the invoice is `pending` until it closes | "If `usage_date` corresponds to a time already invoiced, then it is stored but never invoiced unless the invoice is regenerated." |
| [Lago](https://knowledge.getlago.com/articles/1455954942-what-happens-to-usage-events-sent-after-the-grace-period-ends) | a [grace period](https://getlago.com/docs/guide/invoicing/invoicing-settings/grace-period.md) in days, invoices stay draft | "Late events are still accepted by the API, but they won't appear on any invoice, past or future." (knowledge-base article, which calls itself not officially supported documentation) |

**Correction (2026-10-23):** Lago's grace period needs a premium licence at
every level (organization, billing entity and customer). The free edition
uses 0 days, so it finalizes the invoice when the period closes. Checked in
lago-api `2e52dfc` (2026-10-22): `app/services/organizations/update_service.rb:59-61`,
`app/services/customers/update_invoice_issuing_date_settings_service.rb:56-62`,
`app/models/customer.rb:239-243`.

Kill Bill is the exception: it re-reads the past two periods and bills the
difference on a later invoice
([docs](https://docs.killbill.io/latest/userguide_subscription.html)).

## Decision

1. **Setting.** `INVOICE_GRACE_PERIOD`, a Go duration read once at start in
   `internal/config` (`loadInvoiceGracePeriod`). Unset is `1h`. Outside
   0 to 72h, or not a duration, start-up fails. It is passed to
   `api.NewServer` as `WithInvoiceGrace`, which threads the one value into
   the engine and the backlog gauges.
2. **What waits.** A due instant is a period end (`next_billing_at`) or a
   mid-period `cancel_at`. The two wall-clock billing runs, the scheduler's
   `RunCycle` and `POST /v1/billing/run` (`RunCycleForTenant`), admit a
   subscription only when its due instant is at or before now less the
   grace. Both arms of both due scans use that instant
   (`GetDueBilling`, `GetDueBillingForTenant`: `now` and `dueBy`), and
   `billSubscription` uses the same rule for its cancel branch and its
   caught-up check. So a catch-up after downtime bills the older periods at
   once and the newest after its grace.
3. **What does not wait.** The billing-failure wait (ADR-119) compares with
   the real now. `BillDueNow` passes no grace; it is the close behind an
   immediate cancel, `retry-billing`, and the pre-close every other
   subscription write runs (#1038). The test-clock run passes no grace, so
   advancing a clock to a period end still produces its invoice. Spend
   threshold invoices and the final invoice of an immediate cancel are built
   at once, as before.

   **Amended 2026-10-22: dunning's final pause and cancel wait.** They close
   the ended periods through `BillDue` with the grace
   (`subscription.Service.PauseCollectionForDunning`, `CancelForDunning`).
   Periods past their grace are closed. A period still inside it is left
   open, and nothing is paused or canceled. The dunning run's
   `next_action_at` moves to the end of the grace. A first exhaustion is
   not marked `action_failed` (a re-attempt of an earlier failed action
   keeps that mark, see ADR-112), and no write-off, escalation, email or
   webhook fires then. The dashboard labels the wait "Final step". Test
   clocks still pass no grace.
4. **Gauges.** `velox_billing_due_subscriptions` and
   `velox_billing_oldest_due_age_seconds` use the same shifted instant. The
   age is counted from the due instant plus the grace.
5. **Credit expiry does not use the grace** (amended 2026-10-19). Both
   expiry sweeps skip a grant while a close or a period invoice that can
   draw on it is owed (ADR-071 §2). Inside the grace the subscription's
   due instant is still before the grant's `expires_at`, so the grant is
   held until the close runs. The first version of this decision retired
   a grant once the grace had passed after its `expires_at`.
6. **Dates.** `billOnePeriod` still runs at the due instant: the invoice's
   period, `issued_at` and `due_at` stay at the boundary, and a scheduled
   cancel is stamped at its `cancel_at`.

## Why this design

**One grace on the due instant, nothing else.** The close already reads
usage by event timestamp at close time. Delaying the close is the whole
mechanism: no draft that is re-aggregated, no second read. A draft that is
refreshed was the alternative; Velox's draft lines are frozen once written,
so it would have needed a new re-aggregation path.

**Both scan arms and the engine use one instant.** If only the
`next_billing_at` arm waited, a row admitted by the `cancel_at` arm would be
declined by the engine, and the drain would save `errStillDue` as a billing
failure on a healthy subscription. The tests check that no failure is saved
inside the grace.

**The manual run waits.** `POST /v1/billing/run` is a due scan. If it did not
wait, one call would close every period inside its grace and cut off the
usage still arriving.

**Operator actions on one subscription do not wait.** An operator who cancels
or changes a subscription, or retries its billing, is acting now. Making them
wait would refuse the cancel with `unbilled_period` for up to the grace.

**Dunning does wait (amended 2026-10-22).** Dunning is not the operator
acting now. Its exhaustion can land minutes after a period end: a later
payment failure, a threshold or one-off invoice, a custom schedule. A close
then would bill none of the usage still arriving inside the grace. A
wait of at most the grace costs that much more service before the pause or
cancel. Reusing the `action_failed` re-attempt was rejected: it waits 24
hours, not the grace, and labels a deliberate wait a failure.

**Test clocks do not wait.** In a simulation the operator decides when usage
arrives relative to an advance, and every existing clock flow expects the
invoice at the boundary.

**The credit sweep waits for the close, not for the grace.** The close
draws on grants that were live at the period end. With a grace longer than
the tick, a tick inside the grace would retire such a grant before the
close, and the customer would lose credit that was live when the period
ended. Waiting out the grace after `expires_at` did not cover a close that
fails and waits on the ADR-119 retry ladder, or a draft whose tax is
computed hours later (#653). So the sweep skips a grant while its customer
still owes such a close or such an invoice (ADR-071 §2).

**1 hour by default.** It is Stripe's default and the short end of the
peers (Orb 12h, Metronome 24h). Today's hourly tick already delayed a close
by up to an hour, so 1h changes the least for an upgrade. An operator whose
pipeline lags longer raises it.

## Alternatives considered

- **A. Bill late usage on a later invoice (Kill Bill shape).** Rejected by
  the decided policy. It re-prices non-additive and tiered meters as
  differences, taxes old usage at the new invoice's date, and turns a resent
  event without an idempotency key into a direct double charge.
- **B. Refuse events older than a window (Stripe 35 days, Orb about 12 hours
  at ingestion).** Not now: refusing breaks retries and pipelines, and it
  does not bill anything that is refused. Deferred until abuse or volume
  asks for it.
- **C. Name it `USAGE_GRACE_PERIOD`.** Rejected: it delays every
  period-end invoice, including flat-fee ones, with their webhook, tax
  record, credits, charge and dunning start. Stripe calls it a finalization
  grace for all invoices too.
- **D. A per-tenant setting.** Deferred: Orb and Metronome change it only
  through support. Trigger: a tenant asks.

## Consequences

### Positive

- Usage that arrives within the grace is billed, whatever the tick phase,
  failover or restart. The one exception is in Risks: usage stamped before a
  spend-threshold invoice.
- The policy is written down and the same for every peer-shaped question:
  what counts, when the invoice is built, what happens after.

### Risks / open items

- **Everything after the invoice moves later by up to the grace:** the
  `invoice.finalized` webhook, the tax record, credits applied, the charge
  and the start of dunning. With net-0 terms an invoice is past due when it
  is created, as it already was by up to an hour.
- **Scheduled changes take effect at the close, too.** A cancel or plan
  change scheduled for a date, and a scheduled resume of paused collection
  that falls after a period end, wait for that close. Until then the
  subscription is `active` on its old plan, and `subscription.canceled` is
  sent at the close. `canceled_at` stays the scheduled date. An integration
  that gives access while `status` is `active` gives up to the grace (at
  most 72h) more.
- **Usage stamped before a spend-threshold invoice and arriving after it is
  billed on no invoice**, even inside the period. The period-end invoice
  bills usage only from the threshold invoice's end on
  (`meterUsageIntervals`). This was already so before the grace. Bill it
  with a one-off invoice. With `reset_billing_cycle`, the threshold invoice
  closes the period at once. *(2026-10-23: `reset_billing_cycle` is removed;
  a threshold invoice never closes the period.)*
- **The subscription shows the ended period during the grace.** Spend
  thresholds do not fire on the new period's usage, and the live credit
  balance leaves it out, until the invoice is built. A grant created during
  the grace is drawn by the ended period's invoice. Deferred until a
  spend-cap complaint.
- **An expired grant stays in the ledger balance while a close or a period
  invoice that can draw on it is owed.** That is the grace, and longer
  while a close is failing or a period invoice waits for tax or
  collection. Only a period or threshold invoice issued before the expiry
  draws from it, at its first collection or a dunning retry (ADR-071 §2).
  Every other draw tests expiry at its own instant.
- **Late usage after the invoice is flagged per event, but not listed.**
  Since 2026-10-22 (amendment below) the answer that writes such an event
  says so, and `velox_usage_after_close_total` counts them. There is still
  no list of them per customer and period, and no dashboard notice.
  **Deferred: the per-customer list** (and a pre-filled one-off invoice).
  Trigger: an operator needs to find which events missed an invoice after
  the fact, and the counter, the WARN log and comparing the customer's
  usage with the invoice lines are not enough.
- **Walks and scripts** that close a wall-clock period with
  `POST /v1/billing/run` right after it ends must run with
  `INVOICE_GRACE_PERIOD=0`.

## Amendment 2026-10-22: late usage after the invoice gets a signal

Usage that arrives after its period's invoice is still stored, answered
2xx, and billed on no invoice. What changed is that it is no longer silent:

- `POST /v1/usage-events` and `POST /v1/usage-events/backfill` answer 201
  with `billing_status: "after_close"`;
- `POST /v1/usage-events/batch` and the LiteLLM callback count such rows in
  `after_close`;
- `velox_usage_after_close_total{origin}` counts them, with a WARN log line
  per row.

**The rule** (`internal/usage/open_period.go`, `classify`). An event is
after_close when an active or trialing subscription that prices its meter
already invoiced its time, and no active or trialing subscription's open
period holds it. "Already invoiced" means either:

- the event is before the subscription's current period start, and not
  before its trial end (trial usage is free) or its start; or
- the event is before the end of a **paid** threshold invoice of the
  subscription's current period (a threshold fire with no reset), and
  every bucket of the meter is sum or count. These invoices are read for
  every active and trialing subscription, whether or not it still has
  thresholds: the close keys its watermark on the invoices, not on the
  setting.

Two kinds of meter are never flagged, because a later close can still
bill the event:

- a meter with a `last_ever` rule has no lower time bound;
- a max or last bucket that the threshold fire deferred is billed over the
  whole period at close (ADR-066 §4).

An event at or after the current period end (the grace) is never flagged:
the next close bills it.

An event before the end of a threshold invoice that is **not paid yet**
is not flagged either. The close's watermark counts every threshold
invoice that is not voided (#243; a written-off one counts since the
[ADR-066 amendment of 2026-10-23](066-threshold-money-semantics.md#amendment-2026-10-23-a-write-off-keeps-the-window-billed)):
if that invoice is voided before the close, the close bills the period's
sum and count usage from the period start, the event included. A paid
invoice cannot be voided (the status write refuses it), so its end is a
watermark the close never goes below. A written-off one still can be, so
its end does not flag. If the unpaid invoice is paid or written off
instead, the event is billed nowhere and was not flagged: a missed flag,
not a wrong one.

**Why the flag is never false.** Ingest reads the periods before it writes
the event. Every writer of a period start, and the threshold watermark
invoice, commits in the transaction that froze that window, after reading
its usage. So a read that shows the window frozen means it was frozen
before the event existed. A frozen window must also stay frozen: a built
period invoice is never re-opened, and a threshold window counts only
once its invoice is paid (above). A close that commits between the read and the
write is not seen, and that event goes unflagged; a missed flag is
accepted, a wrong one is not. Only the write flags. A replay answers with
the original row and no `billing_status`, because recomputing the status
later could call an event the close did bill "after_close". A crash after
the commit loses that row's flag; the client's retry dedups and is not
flagged.

**Scope.** Canceled subscriptions are not read: usage for a canceled
subscription's final period that arrives after its final invoice stays
silent. A failed read of the periods refuses the request with 503
`ingest_unavailable`, like a failed customer or meter lookup, so nothing is
written without its signal. The read costs one tenant transaction per
customer per request, plus one per active or trialing subscription for its
threshold invoices, and one per meter when an event could be after_close.
Measured locally on the close-lag rig with one subscription: single ingest
went from about 2.5 ms to about 4.2 ms per event (about 0.7 ms of it is the
threshold-invoice read); a 1,000-event batch for one customer was
unchanged (about 240 ms).

The held-usage release (ADR-122) uses the same rule. It reads the open
periods before it writes, and each meter's aggregation on its own
transaction after its inserts: a release can write a meter's first events, and the
aggregation can change until then (ADR-130 §2, amended 2026-10-24). A released row at or after the period end, before
that period's invoice, belongs to the next period: `in_open_period` when
the subscription continues past the end and its plan from then on prices
the meter. A row the rule calls after_close counts as
`outside_open_period`. A row before the end of an unpaid threshold invoice
counts as `pending_threshold_invoice`: the period's invoice bills it only
if that threshold invoice is voided first.

## References

- ADR-028 (test clocks and wall-clock runs are separate), ADR-097
  (mid-period scheduled cancel), ADR-115 (one closer per period), ADR-119
  (billing failure record).
- ADR-071 §2: the expiry sweep waits for owed draws.
- #961 (late usage), #1038 and #1039 (writes and pauses in the gap).
- `internal/config/config.go` `loadInvoiceGracePeriod`;
  `internal/billing/engine.go` `SetInvoiceGrace`, `RunCycle`,
  `RunCycleForTenant`, `billSubscription`;
  `internal/subscription/postgres.go` `GetDueBilling`,
  `GetDueBillingForTenant`; `internal/api/middleware/metrics.go`;
  `internal/domain/credit_expiry.go` `CreditDrawOwedSQL`.
