/**
 * Which invoice states have stopped being collectible, and what to call the
 * money once they have.
 *
 * This exists because the same rule has now been missed three times, always
 * the same way — a branch tests `voided` (with or without `paid`) and forgets
 * `uncollectible`, so a written-off invoice keeps demanding payment:
 *
 *   1. #651 relabelled the figure on the hosted invoice's totals block and
 *      left its twin in the header saying "Amount Due" — that page's own
 *      comment records the fix as "Same rule, both places."
 *   2. The public payment-update page never read invoice status at all: a
 *      VOIDED invoice rendered "Payment method needed · Amount Due $25.00"
 *      (found on the FLOW D4 walk, 2026-07-28).
 *   3. That fix covered `paid || voided` and stopped there, so a WRITTEN-OFF
 *      invoice still showed the amber demand — while the hosted invoice page,
 *      facing the same customer from the same dunning email, already said
 *      "This invoice is closed". Two public pages, opposite answers, same
 *      invoice (found live on TC Walk Co VLX-000152, 2026-08-05).
 *
 * Three misses of one rule is the point at which a shared predicate is cheaper
 * than the next miss. Both public pages now read this, and it is pinned by
 * tests/invoiceTerminal.test.ts.
 *
 * NOTE the copy is deliberately NOT shared. The hosted invoice page drops its
 * Pay button and says "contact support"; the payment-update page keeps its
 * "add a payment method" button, because saving a card still serves the NEXT
 * invoice even when this one is closed. Same predicate, different voice.
 */

/**
 * Terminal = collection has ended, by any route. `uncollectible` belongs here
 * even though a write-off does NOT annul the debt (the invoice stays on the
 * books and MarkPaid remains legal from it) — what ended is SELF-SERVICE
 * collection, which is the only thing these customer-facing pages offer.
 */
export const TERMINAL_INVOICE_STATUSES = ['paid', 'voided', 'uncollectible'] as const

export function isTerminalInvoiceStatus(status: string | undefined | null): boolean {
  return !!status && (TERMINAL_INVOICE_STATUSES as readonly string[]).includes(status)
}

/**
 * `amount_due_cents` survives a void/settle/write-off by design — the
 * transition ends collection, it does not rewrite the figure. So the number
 * stays true and only its LABEL changes: calling it "Amount Due" on an invoice
 * nobody is collecting is the actual lie.
 */
export function invoiceAmountLabel(status: string | undefined | null): string {
  return isTerminalInvoiceStatus(status) ? 'Invoice amount' : 'Amount Due'
}
