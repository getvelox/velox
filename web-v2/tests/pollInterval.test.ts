// Pins the poll gate that decides whether the invoice page keeps refreshing.
//
// `uncollectible` was treated as terminal-no-trailing-events, which was true
// until bad-debt recovery shipped. After that, an operator could click "Charge
// customer", watch one refetch draw the in-flight banner, and then see NOTHING
// EVER AGAIN — no settle, no paid row, no status flip — until a manual reload.
// The banner whose entire purpose is "a second operator must see money moving"
// could not clear on its own.
//
// This is the only automatable proof that fix holds; the alternative is
// noticing a page that quietly stopped updating.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { pollIntervalForInvoice } from '../src/lib/invoicePolling.ts'
type Invoice = { status: string; payment_status: string }

function inv(status: string, payment_status: string): Invoice {
  return { status, payment_status } as unknown as Invoice
}

test('a written-off invoice with a recovery IN FLIGHT keeps polling', () => {
  for (const ps of ['processing', 'unknown']) {
    const got = pollIntervalForInvoice(inv('uncollectible', ps))
    assert.notEqual(got, false, `uncollectible + ${ps} must keep polling — the in-flight banner can never clear otherwise`)
  }
})

test('a settled written-off invoice stops polling — the negative control', () => {
  // Without this, "poll whenever uncollectible" would satisfy the test above
  // and reintroduce the forever-polling bug the terminal check was added for.
  for (const ps of ['pending', 'failed']) {
    assert.equal(
      pollIntervalForInvoice(inv('uncollectible', ps)), false,
      `uncollectible + ${ps} has nothing in flight and must not poll`,
    )
  }
})

test('draft and voided stay terminal', () => {
  assert.equal(pollIntervalForInvoice(inv('draft', 'pending')), false)
  assert.equal(pollIntervalForInvoice(inv('voided', 'pending')), false)
  // …even mid-flight: a voided invoice's charge is cancelled, not awaited.
  assert.equal(pollIntervalForInvoice(inv('voided', 'processing')), false)
})

test('an ordinary in-flight invoice still polls (regression guard)', () => {
  assert.notEqual(pollIntervalForInvoice(inv('finalized', 'processing')), false)
})
