// Money sums for the Credit Notes stat cards.
//
// The load-bearing rule: refund_amount_cents on an issued credit note is an
// ALLOCATION — an instruction to move cash — not evidence that cash moved.
// The evidence is refund_status. "Refunded to Card" therefore counts only
// succeeded legs; pending and failed allocations are broken out so the page
// can say what they are instead of silently folding them into a money total
// (observed live 2026-08-03: $30.00 of FAILED refund legs — no Stripe refund
// even existed — displayed as money returned to cards).
//
// Contrast creditNoteCeilings.ts, which deliberately counts failed legs in
// priorRefunds: the over-refund CEILING mirrors the server cap and must stay
// conservative (a failed-locally refund may still exist at Stripe). A ceiling
// errs toward counting; a "money returned" stat must be true.

export interface CreditNoteStatsInput {
  status: string
  refund_status?: string | null
  refund_amount_cents: number
  credit_amount_cents: number
  out_of_band_amount_cents?: number | null
  total_cents: number
}

export interface CreditNoteStats {
  draft: number
  issued: number
  voided: number
  totalCredited: number
  /** Succeeded card legs only — cash that actually went back. */
  totalRefunded: number
  /** Card legs still in flight at the provider (allocated, outcome unknown). */
  refundPendingCents: number
  /** Card legs whose provider leg failed (allocated, no cash moved). */
  refundFailedCents: number
  totalOutOfBand: number
  totalAmount: number
}

export function creditNoteStats(notes: CreditNoteStatsInput[]): CreditNoteStats {
  const issued = notes.filter(n => n.status === 'issued')
  const refundLeg = (n: CreditNoteStatsInput, status: string) =>
    n.refund_amount_cents > 0 && n.refund_status === status ? n.refund_amount_cents : 0
  return {
    draft: notes.filter(n => n.status === 'draft').length,
    issued: issued.length,
    voided: notes.filter(n => n.status === 'voided').length,
    totalCredited: issued.reduce((sum, n) => sum + n.credit_amount_cents, 0),
    totalRefunded: issued.reduce((sum, n) => sum + refundLeg(n, 'succeeded'), 0),
    refundPendingCents: issued.reduce((sum, n) => sum + refundLeg(n, 'pending'), 0),
    refundFailedCents: issued.reduce((sum, n) => sum + refundLeg(n, 'failed'), 0),
    totalOutOfBand: issued.reduce((sum, n) => sum + (n.out_of_band_amount_cents ?? 0), 0),
    totalAmount: issued.reduce((sum, n) => sum + n.total_cents, 0),
  }
}
