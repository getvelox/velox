// Guards the Issue-credit-note dialog's ceilings.
//
// A `max` rendered on a money input is a promise about what the field accepts.
// Two of these promises have been wrong in production code:
//
//   1. The Amount field advertised no ceiling at all (max=999999.99) while the
//      server enforced a real remaining-creditable limit — the operator learned
//      it only by being rejected on submit. (fixed #702)
//   2. The Refund field advertised the CARD headroom, ignoring that a refund is
//      one of three channels that must sum to the note's own total. On an
//      invoice with $58.59 of card headroom but only $8.59 still creditable, it
//      read `max $58.59`; typing $20 was accepted by the field and caught only
//      by the allocation line going ✗. (this change)
//
// The fixture below is that real invoice — VLX-000067, walked 2026-08-03.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  creditNoteCeilings,
  hasCardPayment,
  type CeilingNote,
} from '../src/lib/creditNoteCeilings.ts'

// VLX-000067: $88.59 total = $20.00 credits + $68.59 card, with CN-000030
// already issued for $80.00 of which $10.00 went back to the card.
const VLX67 = {
  invoiceTotalCents: 8859,
  amountPaidCents: 6859,
  stripePaymentIntentId: 'pi_3U015R',
  creditNotes: [
    { status: 'issued', total_cents: 8000, refund_amount_cents: 1000 },
  ] as CeilingNote[],
}

test('hasCardPayment: only a real PaymentIntent counts', () => {
  assert.equal(hasCardPayment('pi_3TynLT'), true)
  assert.equal(hasCardPayment(''), false)
  assert.equal(hasCardPayment(undefined), false)
  assert.equal(hasCardPayment('out_of_band:2026-08-02T15:22:52Z'), false)
  // Guards the prefix test against a contains/suffix rewrite.
  assert.equal(hasCardPayment('pi_out_of_band_lookalike'), true)
})

test('the refund ceiling is the BINDING rule, not the card rule alone', () => {
  // Nothing typed yet: the note could still grow to the full $8.59 remaining,
  // so that — not the $58.59 of card headroom — is what the field can accept.
  const c = creditNoteCeilings({ ...VLX67, amountCents: 0 })

  assert.equal(c.alreadyCreditedCents, 8000)
  assert.equal(c.creditableRemainingCents, 859, 'rule 1: 88.59 − 80.00')
  assert.equal(c.cardRefundableCents, 5859, 'rule 2: 68.59 − 10.00 already refunded')
  assert.equal(c.refundCeilingCents, 859, 'the note binds before the card does')
  // The invoice's remaining creditable amount is the FLOOR here: no note,
  // however large, can exceed $8.59, so the $58.59 of otherwise-unrefunded card
  // money is permanently unreachable on this invoice. Calling it "still
  // refundable" would be the same false promise this module exists to prevent.
  assert.equal(c.refundBoundBy, 'creditable')
})

test('the refund ceiling tracks the typed note total, live', () => {
  const typed = creditNoteCeilings({ ...VLX67, amountCents: 500 })
  assert.equal(typed.refundCeilingCents, 500, 'a $5.00 note can refund at most $5.00')
  // Here raising the note DOES unlock more (up to $8.59), so the copy may say so.
  assert.equal(typed.refundBoundBy, 'note-total')
  // The card fact survives so the UI can still show it — losing it would trade
  // one missing truth for another.
  assert.equal(typed.cardRefundableCents, 5859)
})

test('when the card is the binding rule, it stays the ceiling', () => {
  // Fresh invoice, no prior notes: $68.59 card of an $88.59 total. A note for
  // $80.00 has plenty of room, so the CARD is what binds.
  const c = creditNoteCeilings({
    invoiceTotalCents: 8859,
    amountPaidCents: 6859,
    stripePaymentIntentId: 'pi_x',
    creditNotes: [],
    amountCents: 8000,
  })
  assert.equal(c.refundCeilingCents, 6859)
  assert.equal(c.refundBoundBy, 'card', 'the card rule binds; do not blame the note')
})

test('drafts consume headroom exactly as the server counts them', () => {
  const withDraft = creditNoteCeilings({
    ...VLX67,
    creditNotes: [
      ...VLX67.creditNotes,
      { status: 'draft', total_cents: 200, refund_amount_cents: 200 },
    ],
    amountCents: 0,
  })
  assert.equal(withDraft.creditableRemainingCents, 659, '88.59 − 80.00 − 2.00')
  assert.equal(withDraft.cardRefundableCents, 5659, '68.59 − 10.00 − 2.00')
})

test('voided notes release their claim', () => {
  const withVoid = creditNoteCeilings({
    ...VLX67,
    creditNotes: [
      ...VLX67.creditNotes,
      { status: 'voided', total_cents: 5000, refund_amount_cents: 5000 },
    ],
    amountCents: 0,
  })
  assert.equal(withVoid.creditableRemainingCents, 859, 'the voided $50 is not consumed')
  assert.equal(withVoid.cardRefundableCents, 5859)
})

test('no card payment means no refund ceiling at all', () => {
  const offline = creditNoteCeilings({
    invoiceTotalCents: 5362,
    amountPaidCents: 5362,
    stripePaymentIntentId: 'out_of_band:2026-08-02T15:22:52Z',
    creditNotes: [],
    amountCents: 1000,
  })
  assert.equal(offline.cardRefundableCents, 0)
  assert.equal(offline.refundCeilingCents, 0)
  // The note is not what's stopping this one — there is simply no card. The UI
  // says "not paid by card" here rather than blaming the note's size.
  assert.equal(offline.refundBoundBy, 'no-card')
})

test('a fully credited invoice offers nothing on either axis', () => {
  const spent = creditNoteCeilings({
    ...VLX67,
    creditNotes: [{ status: 'issued', total_cents: 8859, refund_amount_cents: 0 }],
    amountCents: 0,
  })
  assert.equal(spent.creditableRemainingCents, 0)
  assert.equal(spent.refundCeilingCents, 0)
})

test('over-refunded history clamps at zero rather than going negative', () => {
  const over = creditNoteCeilings({
    ...VLX67,
    creditNotes: [{ status: 'issued', total_cents: 8000, refund_amount_cents: 9999 }],
    amountCents: 0,
  })
  assert.equal(over.cardRefundableCents, 0)
  assert.equal(over.refundCeilingCents, 0)
})
