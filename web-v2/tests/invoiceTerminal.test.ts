// Pins the terminal-invoice rule that has now been missed three times, always
// by forgetting `uncollectible` (see src/lib/invoiceTerminal.ts for the three).
//
// The failure mode these guard is not a crash — it is a customer-facing page
// asking for money on an invoice nobody is collecting, which renders perfectly.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  TERMINAL_INVOICE_STATUSES,
  isTerminalInvoiceStatus,
  invoiceAmountLabel,
} from '../src/lib/invoiceTerminal.ts'

test('uncollectible is terminal — the miss this module exists for', () => {
  assert.equal(isTerminalInvoiceStatus('uncollectible'), true)
  assert.equal(invoiceAmountLabel('uncollectible'), 'Invoice amount')
})

test('all three collection-ending states are terminal', () => {
  for (const s of ['paid', 'voided', 'uncollectible']) {
    assert.equal(isTerminalInvoiceStatus(s), true, `${s} must be terminal`)
    assert.equal(invoiceAmountLabel(s), 'Invoice amount', `${s} must relabel the figure`)
  }
})

test('collectible states are NOT terminal — the negative control', () => {
  // Without these, a predicate that returned true for everything would pass
  // every assertion above.
  for (const s of ['finalized', 'draft']) {
    assert.equal(isTerminalInvoiceStatus(s), false, `${s} is still collectible`)
    assert.equal(invoiceAmountLabel(s), 'Amount Due', `${s} must still say Amount Due`)
  }
})

test('absent / unknown status is treated as collectible, not terminal', () => {
  // invoice_status is optional on the token payload. Defaulting to "terminal"
  // would silently stop asking for payment on invoices that ARE owed — the
  // expensive direction of this mistake.
  for (const s of [undefined, null, '', 'some_future_status']) {
    assert.equal(isTerminalInvoiceStatus(s), false)
    assert.equal(invoiceAmountLabel(s), 'Amount Due')
  }
})

test('the exported list and the predicate cannot drift apart', () => {
  for (const s of TERMINAL_INVOICE_STATUSES) {
    assert.equal(isTerminalInvoiceStatus(s), true, `${s} is listed but not detected`)
  }
  assert.equal(TERMINAL_INVOICE_STATUSES.length, 3)
})
