// Guards the Usage Events window presets.
//
// The page used to send no time bound at all on first load, so its stat cards
// aggregated the tenant's entire event history every time. Bounding the default
// is the fix; these tests pin the two properties that make it safe:
//
//   1. an unknown ?range= falls back to the BOUNDED default, never to 'all'
//      (a typo'd URL must not silently reinstate the full-history scan), and
//   2. 'all' still resolves to a genuinely unbounded window, because bounding
//      the default must not remove the capability.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  DEFAULT_RANGE,
  RANGE_KEYS,
  parseRange,
  rangeCaption,
  resolveWindow,
  type RangeKey,
  type Window,
} from '../src/lib/usageRange.ts'

// Deterministic stand-in for the tenant-TZ date helper.
const fakeAgo = (amount: number, unit: 'day' | 'month') => `ago:${amount}${unit === 'day' ? 'd' : 'm'}`
const noCustom: Window = { from: '', to: '' }

test('the default range is bounded', () => {
  assert.notEqual(DEFAULT_RANGE, 'all')
  const w = resolveWindow(DEFAULT_RANGE, noCustom, fakeAgo)
  assert.notEqual(w.from, '', 'the default must carry a lower bound — that is the whole point')
})

test('unknown or missing ?range= falls back to the bounded default, not to all-time', () => {
  for (const raw of [null, undefined, '', 'all-time', 'ALL', '30', 'custom-range', '../etc/passwd']) {
    assert.equal(parseRange(raw), DEFAULT_RANGE, `input ${JSON.stringify(raw)}`)
  }
})

test('every declared range key round-trips through parseRange', () => {
  for (const k of RANGE_KEYS) assert.equal(parseRange(k), k)
})

test('presets bound the start and leave the end open', () => {
  const cases: Array<[RangeKey, string]> = [
    ['7d', 'ago:7d'],
    ['30d', 'ago:30d'],
    ['90d', 'ago:90d'],
    ['12m', 'ago:12m'],
  ]
  for (const [range, from] of cases) {
    const w = resolveWindow(range, noCustom, fakeAgo)
    assert.equal(w.from, from)
    assert.equal(w.to, '', 'a preset must not pin the end, or the window stops tracking forward')
  }
})

test('all-time resolves to a genuinely unbounded window', () => {
  assert.deepEqual(resolveWindow('all', noCustom, fakeAgo), { from: '', to: '' })
})

test('custom defers entirely to the operator dates and ignores the preset helper', () => {
  const custom: Window = { from: '2026-01-01', to: '2026-03-31' }
  assert.deepEqual(resolveWindow('custom', custom, fakeAgo), custom)
  // A preset must NOT leak the operator's stale custom dates through.
  assert.deepEqual(resolveWindow('7d', custom, fakeAgo), { from: 'ago:7d', to: '' })
})

test('the caption names the window, and never reads back the word "custom"', () => {
  assert.equal(rangeCaption('30d', { from: 'ago:30d', to: '' }), 'Totals for the last 30 days')
  assert.equal(rangeCaption('12m', { from: 'ago:12m', to: '' }), 'Totals for the last 12 months')
  assert.equal(rangeCaption('all', noCustom), 'Totals across all time')

  assert.equal(rangeCaption('custom', { from: '2026-01-01', to: '2026-03-31' }), 'Totals for 2026-01-01 to 2026-03-31')
  assert.equal(rangeCaption('custom', { from: '2026-01-01', to: '' }), 'Totals from 2026-01-01 onwards')
  assert.equal(rangeCaption('custom', { from: '', to: '2026-03-31' }), 'Totals up to 2026-03-31')
  // Custom with no dates IS unbounded — say so rather than implying a filter.
  assert.equal(rangeCaption('custom', noCustom), 'Totals across all time')

  for (const k of RANGE_KEYS) {
    assert.doesNotMatch(rangeCaption(k, noCustom), /custom/i, `caption for ${k} leaks the mode name`)
  }
})
