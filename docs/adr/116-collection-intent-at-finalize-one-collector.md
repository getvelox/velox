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
5. **No-payment dunning enrollment checks for a card, after collection settles.** The queue now holds every unpaid invoice, so `EnrollStalledForDunning` enrolls a row as `no_payment_method` only when:
   - it is owed;
   - `ResolveForCharge` finds no chargeable method (a lookup error skips the row for that tick);
   - it was last written more than 10 minutes ago.

   The last condition matters because the finalize write queues the invoice before the nudge applies the customer's credits, with the webhook dispatch and the Stripe tax commit in between. Without the settle window, a tick could dun an invoice that its credits are about to cover. The test-clock path has no window, because catchup collects before it enrolls.
6. **Any definite failure hands off to dunning.** A charge that fails definitely clears the flag, whether it carries a `decline_code` or not (for example an `invalid_request`). The charger has already marked the invoice failed and started dunning. Before, only a coded decline cleared the flag.

## Consequences

- **Decline ownership is uniform.** A decline clears the flag and dunning owns the retries. That was the sweep's rule; it is now every path's rule. ADR-087 §3's flag-on-decline analysis no longer applies.
- **Credits are applied when the invoice is finalized, not when the draft is built.** A tax-pending draft no longer has credits applied to it. They apply when tax resolves and the invoice finalizes. Allocation order across a customer's invoices follows finalize order.
- **The response shape is unchanged.** Day-1, cancel and operator finalize still return the post-collection invoice: paid, failed, or still queued.
- **Dashboard and webhook copy.** `auto_charge_pending` is now `true` on every unpaid finalized invoice, including in `invoice.finalized` payloads. The attention banner reads "payment scheduled" where it used to say "awaiting payment".
- **No backfill.** Rows written before this change that are finalized, unpaid and unflagged are left alone. The trigger to revisit is the first such row observed.
- **Not covered, with triggers.**
  - The sweep lists `ORDER BY created_at LIMIT batch`, and card-less invoices stay queued while they are in dunning. A backlog larger than the batch can delay crash recovery of newer rows. Trigger: a tick that lists a full batch with no charge.
  - Proration invoices are queued but get no nudge (unchanged). Trigger: an operator asking for an immediate charge on a plan change.
  - The dunning adapter's `$0 → recovered` branch does not settle the invoice; that is a separate change.
