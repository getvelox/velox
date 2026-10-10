# ADR-116: Finalize records the collection intent; one collector does the work

**Status:** Accepted (2026-10-08)
**Supersedes:** [ADR-087](087-collect-after-finalize-pipeline.md)'s per-site collection (the shared pipeline, the per-site gates, and §3's flag-on-decline analysis).
**Closes:** [ADR-115](115-one-closer-for-the-billing-period.md)'s "named crash-point loss" (the born-$0 / fully-credited `MarkPaid` window).

## Summary

An invoice that becomes finalized and unpaid is queued for collection (`auto_charge_pending = TRUE`) in the same statement that finalizes it. Collection then runs in exactly one place, the auto-charge collector (`processAutoCharge`). The hourly sweep runs it, and so does a post-commit nudge (`Engine.CollectInvoice`) at every finalize site. The nudge keeps today's timing: the card is charged within seconds, in the same request or tick. A crash at any point after the commit leaves a queued invoice that the next sweep collects.

## Context

Before this, every finalize path committed the invoice with the flag off and then collected inline. There were six such paths: cycle close, threshold fire, day-1, final-on-cancel, operator finalize and tax-retry. Each inline copy set the flag only when its own charge failed. A crash between the commit and the inline step left an invoice that was finalized, owed and invisible: no sweep listed it, no email went out, and dunning never started. Nobody was waiting on those inline charges, so the inline-only path had no justification.

The inline code also existed in several copies: an engine pipeline, a cycle/threshold credit block, an operator handler copy, and the sweep. The copies had drifted. The "add a card" email was sent by both the inline step and the sweep. Three different credit-apply blocks existed, and three different zero-due settles. ADR-087 merged part of this and kept the rest per site.

## Decision

1. **The store records the intent.** These writes set `auto_charge_pending` from the new status in the same SQL statement:
   - the two INSERTs (`CreateAudited`, `createWithLineItemsInTx`);
   - `updateStatusInTx` (draft→finalized; void and uncollectible clear it);
   - `FinalizeWithDates`.

   The rule is `finalized AND payment_status = 'pending'`, so no per-site code can forget it. The amount is not part of the rule: a $0 invoice is queued and the collector settles it.
2. **One collector.** `processAutoCharge` is the only code that applies credits, settles a zero-due invoice, resolves the card, charges, hands a decline to dunning, and sends the "add a card" email (once, via `no_pm_notified_at`).
   - It claims the per-invoice lease first.
   - It then re-reads the row, so a rival collector's credit apply or email is never repeated.
3. **A nudge, not a second collector.** `CollectInvoice` runs the collector on one invoice right after the finalize commit, with `context.WithoutCancel`. Its errors are logged, not returned, because the flag is durable.
   - Cycle close and threshold pass their period instant, so credits and `paid_at` are anchored at the boundary. This matters for test-clock catch-up across several periods.
   - Request-time sites pass nil, meaning the invoice's own clock now.
   - The operator's manual finalize reaches it through a consumer-defined `invoice.Collector`.
4. **Zero-due settles.** The sweep lists and claims $0 rows (`amount_due_cents > 0` is dropped from both lists and `ClaimAutoCharge`), and the collector marks them paid. This covers an invoice brought to $0 by a credit note, which used to sit unpaid forever.
5. **The collector starts no-payment dunning.** *(Amended 2026-10-08, same day.)* In its no-card arm, `processAutoCharge` calls `StartDunning(no_payment_method)`. That arm is reached only after the credit step succeeded, with money still owed and no chargeable method, under the invoice's lease. A lookup error starts nothing.
   - This replaces the separate `EnrollStalledForDunning` sweeps. They read the queue, and once every unpaid invoice was queued from birth, a reader of the queue could dun an invoice whose credits were about to cover it.
   - The first fix was a 10-minute settle window on that sweep. It was a time guess, and it still dunned an invoice whose credit apply had just failed. Starting dunning in the collector makes the rule exact and deletes the second decider.
   - It mirrors the decline path, where the charger starts dunning.
7. **Starting dunning is one transaction.** `dunning.Store.StartRun` writes the run, its `dunning_started` timeline event, and the `dunning.started` webhook (an outbox row) together. It is the same seam the subscription and invoice stores use (`OutboxEnqueuer`). Before, these were three commits. A crash after the first left a run with no timeline row and no webhook, and nothing re-sent them, because `StartDunning` is idempotent on the existing run. The other dunning webhooks are still post-commit (see the README follow-ups table).
6. **Any definite failure hands off to dunning.** A charge that fails definitely clears the flag, whether it carries a `decline_code` or not (for example an `invalid_request`). The charger has already marked the invoice failed and started dunning. Before, only a coded decline cleared the flag.

## Consequences

- **Decline ownership is uniform.** A decline clears the flag and dunning owns the retries. That was the sweep's rule; it is now every path's rule. ADR-087 §3's flag-on-decline analysis no longer applies.
- **Credits are applied when the invoice is finalized, not when the draft is built.** A tax-pending draft no longer has credits applied to it. They apply when tax resolves and the invoice finalizes. Allocation order across a customer's invoices follows finalize order.
- **The response shape is unchanged.** Day-1, cancel and operator finalize still return the post-collection invoice: paid, failed, or still queued.
- **Dashboard and webhook copy.** `auto_charge_pending` is now `true` on every unpaid finalized invoice, including in `invoice.finalized` payloads. The attention banner reads "payment scheduled" where it used to say "awaiting payment".
- **No backfill.** Rows written before this change that are finalized, unpaid and unflagged are left alone. The trigger to revisit is the first such row observed.
- **Not covered, with triggers.**
  - ~~The sweep lists `ORDER BY created_at LIMIT batch`… Trigger: a tick that lists a full batch with no charge.~~ Fixed by the amendment below. The trigger could not fire: nothing emitted that signal.
  - Proration invoices are queued but get no nudge (unchanged). Trigger: an operator asking for an immediate charge on a plan change.
  - The dunning adapter's `$0 → recovered` branch does not settle the invoice; that is a separate change.

## Amendment 2026-10-10: the sweep visits the whole queue every tick

**What was wrong.** The sweep read the 50 oldest queued invoices per mode, across all tenants, once per tick. Card-less invoices stay queued by design, and some never leave on their own:
- one-off invoices, and invoices of canceled subscriptions (dunning's pause has nothing to act on, and the default final invoice action is none);
- every card-less invoice of a tenant with no dunning policy (bootstrap seeds none).

Fifty of them filled the page for good, and nothing newer was retried. That includes the invoices that rely only on the sweep:
- proration charges;
- invoices finalized by the tax retry;
- charges retried after a temporary Stripe error;
- card-less invoices whose customer later adds a card.

It was reproduced against real Postgres (`TestRetryPendingCharges_CardlessHeadDoesNotBlockNewerInvoices`).

**Change.**
- The sweep and the test-clock sweep page by `(created_at, id)` until a short page (`drainInvoiceQueue`). Every queued row is visited once per tick, however many rows ahead of it never leave. Both columns are fixed at insert, so the cursor moves strictly forward; a row that is queued behind it during the drain is visited on the next tick.
- A sweep stops before the next invoice when its ctx ends (shutdown, lost lease), and the scheduler passes its liveness stamp to the sweep, as it does to the billing drain (P27).
- New gauge `velox_auto_charge_queued_invoices{mode}`, using the list's predicate.

**Kept on purpose.** Card-less invoices stay queued and are visited every tick. That visit is what charges an invoice once a card is attached, settles it when credits arrive, retries a setup email that was skipped, and starts dunning once the tenant creates a policy. The cost grows with the queue: about ten statements per resident row per tick, and for a tenant with no dunning policy, one "dunning not configured" WARN per card-less invoice per tick.

**Considered and deferred: park card-less invoices until they can be collected** (Stripe and Kill Bill do not retry with no payment method). Reviewers found it loses the email self-heal and the late-policy dunning start unless both are rebuilt. It also needs SQL copies of the payment-method, credit and policy resolution, which can drift towards stranding an invoice that has a card on file. Trigger to revisit: `velox_auto_charge_queued_invoices` above 5,000 for a sustained period, the `auto-charge sweep` log line's `duration_ms` above 600,000, or the no-policy WARN volume becoming a log-cost problem.

**Considered and rejected: keep the queue flag until the charge outcome is saved.** If a decline's `failed` write fails, the invoice stays `pending`. Keeping it queued looked safer, but the next visit charges again under the same idempotency key with rebuilt parameters (amount after credits, a swapped card, the test-clock anchor). Stripe rejects that as an idempotency conflict, which parks the invoice as unknown for good. Clearing the flag leaves it to the inline dunning run, which retries under its own key, and to the `payment_failed` webhook.

**Not changed: the dunning backfill** (`ListFailedWithoutDunningRun`, `ORDER BY updated_at LIMIT 50`). It has the same shape: a no-policy skip writes nothing, so those rows stay at the head. Paging it every tick would scan every such invoice forever, and a starved row needs a lost dunning start on both the inline and the webhook path. Trigger to revisit: a failed invoice with no dunning run older than a day in a tenant that has a policy. The fix then is to make the skip durable, not to page.

**First deploy.** Rows that sat past position 50 are visited for the first time. Those with a card on file are charged then, which can be well after the invoice was issued. Card-less ones get their setup email and no-payment dunning start, anchored on the issue date, so an old invoice's grace period may already have passed.
