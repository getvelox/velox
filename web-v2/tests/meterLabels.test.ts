import { test } from 'vitest'
import assert from 'node:assert/strict'

import {
  METER_AGGREGATIONS, METER_HAS_USAGE_SENTENCE, METER_NO_USAGE_SENTENCE, meterAggregationHelp, meterAggregationLabel,
  meterHasUsageMessage,
} from '../src/lib/meterLabels'

// A meter has one aggregation (ADR-130 §2): sum, count, max or last. The
// per-rule modes last_during_period and last_ever are gone, so the one
// list offers exactly the four the server accepts.
test('the one aggregation list is the four meter aggregations', () => {
  assert.deepEqual(METER_AGGREGATIONS.map(a => a.value), ['sum', 'count', 'max', 'last'])
  assert.equal(meterAggregationLabel('last'), 'Latest value')
  assert.equal(meterAggregationLabel('max'), 'Maximum')
  assert.equal(meterAggregationHelp('count'), 'Counts the events; their values are ignored.')
  for (const a of METER_AGGREGATIONS) assert.ok(a.help.endsWith('.'), a.value)
})

// Only usage events fix the aggregation; held usage does not (ADR-130 §2,
// amended 2026-10-24).
test('the lock sentences say what fixes the aggregation and what to do instead', () => {
  assert.equal(METER_HAS_USAGE_SENTENCE, 'This meter has usage events, so its aggregation is fixed. Make a new meter to count differently.')
  assert.equal(METER_NO_USAGE_SENTENCE, 'The aggregation can change until this meter has a usage event. Held usage does not fix it.')
})

test('a meter_has_usage refusal is shown as the server wrote it; other errors are not', () => {
  const msg = 'Meter "gpu" has usage, so its aggregation is fixed: its usage so far was counted as max. Make a new meter to count differently.'
  assert.equal(meterHasUsageMessage({ code: 'meter_has_usage', message: ` ${msg} ` }), msg)
  assert.equal(meterHasUsageMessage({ code: 'meter_in_use', message: msg }), null)
  assert.equal(meterHasUsageMessage({ code: 'meter_has_usage', message: '' }), null)
  assert.equal(meterHasUsageMessage(null), null)
  assert.equal(meterHasUsageMessage('meter_has_usage'), null)
})
