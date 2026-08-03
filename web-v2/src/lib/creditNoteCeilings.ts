// Ceilings for the Issue-credit-note dialog.
//
// Extracted from InvoiceDetail so the arithmetic is testable on its own: these
// numbers are rendered as `max` on money inputs, which is a PROMISE that the
// value is the largest the field accepts. A `max` that isn't binding is the
// same defect class as a status label that reports intent instead of outcome —
// the surface asserts something the system won't honour.
//
// Three server rules bound a credit note, and the dialog must mirror all three:
//
//   1. total(all non-voided notes) ≤ invoice total   → creditableRemaining
//   2. refund ≤ card paid − refunds already committed → cardRefundable
//   3. refund + credit + out_of_band == this note's total
//
// Rule 3 is the one the refund field used to ignore when advertising its
// ceiling: a refund can never exceed the note it belongs to, so the honest
// ceiling for that input is min(rule 2, rule 3), not rule 2 alone.

/** Mirrors domain.Invoice.HasCardPayment. */
export const OUT_OF_BAND_PI_PREFIX = 'out_of_band:'

export function hasCardPayment(stripePaymentIntentId?: string | null): boolean {
  return !!stripePaymentIntentId && !stripePaymentIntentId.startsWith(OUT_OF_BAND_PI_PREFIX)
}

export interface CeilingNote {
  status: string
  total_cents: number
  refund_amount_cents: number
}

export interface CeilingInput {
  invoiceTotalCents: number
  amountPaidCents: number
  stripePaymentIntentId?: string | null
  /** Every credit note on the invoice — filtering happens here, not at the caller. */
  creditNotes: CeilingNote[]
  /** What the operator has typed as this note's total; 0 when the field is empty. */
  amountCents: number
}

/**
 * Which rule actually produces the refund ceiling. The UI phrases its
 * explanation from this rather than from a raw number, because the honest
 * sentence differs per case — and one of them ("the card has headroom left")
 * is only TRUE when the card is what binds.
 *
 *   'no-card'    — nothing was paid by card; there is no refund rail at all
 *   'card'       — rule 2 binds; the ceiling IS the card figure, self-evident
 *   'note-total' — the typed note total binds, and raising it would allow more
 *   'creditable' — the invoice's remaining creditable amount binds; raising the
 *                  note total CANNOT help, so the leftover card headroom is
 *                  permanently unreachable and must not be described as
 *                  "still refundable"
 */
export type RefundBoundBy = 'no-card' | 'card' | 'note-total' | 'creditable'

export interface Ceilings {
  /** Sum of every non-voided note's total — what rule 1 has already consumed. */
  alreadyCreditedCents: number
  /** Rule 1: how much of the invoice may still be credited at all. */
  creditableRemainingCents: number
  /** Rule 2 alone: card money the payment history would permit refunding. NOT
   *  necessarily reachable — rule 1 can put it permanently out of range. */
  cardRefundableCents: number
  /** min(rule 2, rule 3) — the largest value the refund input will actually accept. */
  refundCeilingCents: number
  /** Which rule produced that ceiling. */
  refundBoundBy: RefundBoundBy
}

export function creditNoteCeilings(input: CeilingInput): Ceilings {
  // Voided notes released their claim; everything else — including DRAFTS —
  // still counts, because that is what the server counts.
  const live = input.creditNotes.filter(cn => cn.status !== 'voided')

  const alreadyCreditedCents = live.reduce((sum, cn) => sum + cn.total_cents, 0)
  const creditableRemainingCents = Math.max(0, input.invoiceTotalCents - alreadyCreditedCents)

  const priorRefunds = live.reduce((sum, cn) => sum + cn.refund_amount_cents, 0)
  const cardRefundableCents = hasCardPayment(input.stripePaymentIntentId)
    ? Math.max(0, input.amountPaidCents - priorRefunds)
    : 0

  // Before a total is typed, the note can still grow to the whole remaining
  // creditable amount, so that is the operative rule-3 bound. Once typed, the
  // note's own total is the bound and the ceiling tracks it live.
  const noteCeilingCents =
    input.amountCents > 0 ? input.amountCents : creditableRemainingCents

  const refundCeilingCents = Math.min(cardRefundableCents, noteCeilingCents)

  let refundBoundBy: RefundBoundBy
  if (!hasCardPayment(input.stripePaymentIntentId)) {
    refundBoundBy = 'no-card'
  } else if (cardRefundableCents <= noteCeilingCents) {
    refundBoundBy = 'card'
  } else if (input.amountCents > 0 && input.amountCents < creditableRemainingCents) {
    // The operator's own total is the binding rule and there IS room above it,
    // so raising the note genuinely unlocks more refund.
    refundBoundBy = 'note-total'
  } else {
    // Rule 1 is the floor. No note, however large, can go above it — so any
    // card headroom beyond this point can never be refunded on this invoice.
    refundBoundBy = 'creditable'
  }

  return {
    alreadyCreditedCents,
    creditableRemainingCents,
    cardRefundableCents,
    refundCeilingCents,
    refundBoundBy,
  }
}
