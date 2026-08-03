// Guards the Credit Notes page's "Refunded to Card" stat.
//
// The stat used to sum refund_amount_cents over every issued CN — counting
// allocations, not outcomes — so a refund whose Stripe leg FAILED (no refund
// object even created) displayed as money returned to a card. Observed live
// 2026-08-03 on Walkthrough Co: card showed $192.60 while $30.00 of it was two
// failed legs; the true figure was $162.60. "Refunded to Card" must count only
// refund_status=succeeded; pending/failed are surfaced separately, never
// folded into the money total.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { creditNoteStats } from '../src/lib/creditNoteStats.ts'

const cn = (over: Record<string, unknown>) => ({
  status: 'issued',
  refund_status: 'none',
  refund_amount_cents: 0,
  credit_amount_cents: 0,
  out_of_band_amount_cents: 0,
  total_cents: 0,
  ...over,
})

test('a failed refund leg is never counted as refunded to card', () => {
  const s = creditNoteStats([
    cn({ refund_amount_cents: 16260, refund_status: 'succeeded', total_cents: 16260 }),
    cn({ refund_amount_cents: 2000, refund_status: 'failed', total_cents: 2000 }),
    cn({ refund_amount_cents: 1000, refund_status: 'failed', total_cents: 1000 }),
  ])
  assert.equal(s.totalRefunded, 16260) // the live Walkthrough Co shape: $162.60, not $192.60
  assert.equal(s.refundFailedCents, 3000)
  assert.equal(s.refundPendingCents, 0)
})

test('a pending refund leg is in flight, not refunded', () => {
  const s = creditNoteStats([
    cn({ refund_amount_cents: 5000, refund_status: 'pending', total_cents: 5000 }),
  ])
  assert.equal(s.totalRefunded, 0)
  assert.equal(s.refundPendingCents, 5000)
})

test('draft and voided notes count toward no money stat', () => {
  const s = creditNoteStats([
    cn({ status: 'draft', refund_amount_cents: 700, refund_status: 'succeeded', total_cents: 700 }),
    cn({ status: 'voided', refund_amount_cents: 900, refund_status: 'succeeded', total_cents: 900 }),
  ])
  assert.equal(s.totalRefunded, 0)
  assert.equal(s.totalAmount, 0)
  assert.equal(s.draft, 1)
  assert.equal(s.voided, 1)
})

test('mixed-allocation CN contributes each channel to its own stat', () => {
  const s = creditNoteStats([
    cn({ refund_amount_cents: 1000, refund_status: 'failed', credit_amount_cents: 2000, total_cents: 3000 }),
  ])
  assert.equal(s.totalCredited, 2000)  // the credit leg settled at Issue
  assert.equal(s.totalRefunded, 0)     // the card leg did not
  assert.equal(s.refundFailedCents, 1000)
  assert.equal(s.totalAmount, 3000)
})
