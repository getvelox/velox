// The Margin card's empty-window note. It must say only what is true: a
// window with no usage. Provider rates are set up elsewhere, so the card must
// not tell an operator to add them (#1111). This renders the real MarginCard
// with its data hooks stubbed to an empty window, so it checks the copy the
// operator actually sees.
import { createElement } from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
import { test, vi } from 'vitest'
import assert from 'node:assert/strict'

const emptyReport = {
  revenue_cents: 0,
  revenue_currency: 'USD',
  cost_micros: 0,
  cost_currency: 'USD',
  margin_bps: null,
  unresolved_events: 0,
  by_model: [],
}

vi.mock('@/lib/gen/queries.gen', () => ({
  useGetV1CustomersIdMargin: () => ({ data: emptyReport, isError: false, refetch: vi.fn() }),
}))

vi.mock('@/hooks/useClockFrozenMap', () => ({
  useEffectiveNow: () => Date.parse('2026-10-16T12:00:00Z'),
}))

vi.mock('@/hooks/useCustomerUsageWindows', () => ({
  useCurrentCycleUsage: () => ({
    data: { period: { from: '2026-10-01', to: '2026-10-31' } },
    isError: false,
    refetch: vi.fn(),
  }),
  useLastInvoicedPeriod: () => ({ data: undefined, isError: false, refetch: vi.fn() }),
  useLatestUsageAt: () => ({ data: undefined }),
}))

test('empty margin window says there is no usage, without setup advice', async () => {
  const { MarginCard } = await import('../src/components/MarginCard.tsx')
  const html = renderToStaticMarkup(createElement(MarginCard, { customerId: 'vlx_cus_test' }))

  assert.match(html, /No usage in this window yet\./)
  assert.doesNotMatch(html, /provider rates/i)
})
