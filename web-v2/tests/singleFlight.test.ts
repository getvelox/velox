// Pins the stale-closure double-fire guard behind useSingleFlight. The bug it
// prevents: two clicks dispatched in one event-loop tick (a fast double-click,
// before React re-renders and `disabled` takes effect) both begin the same
// async mutation. On money paths that legally repeat — issue credit note,
// replay a webhook, create a one-off invoice — that is a real second refund /
// second fan-out / second invoice, because the backend does not dedup.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { singleFlight } from '../src/hooks/useSingleFlight.ts'

test('a same-tick second call is dropped while the first is in flight', async () => {
  let calls = 0
  let release!: () => void
  const gate = new Promise<void>((r) => { release = r })
  const guarded = singleFlight(async () => {
    calls++
    await gate
  })

  // Two synchronous calls before the first settles — the double-click.
  const p1 = guarded()
  const p2 = guarded()
  assert.equal(calls, 1, 'the second same-tick call must not enter fn')

  release()
  await Promise.all([p1, p2])
  assert.equal(calls, 1, 'still exactly one invocation after both settle')
})

test('the guard releases after the first call settles', async () => {
  let calls = 0
  const guarded = singleFlight(async () => { calls++ })

  await guarded()
  await guarded()
  assert.equal(calls, 2, 'sequential (non-overlapping) calls both run')
})

test('the guard releases even when fn rejects', async () => {
  let calls = 0
  const guarded = singleFlight(async () => {
    calls++
    throw new Error('boom')
  })

  await assert.rejects(guarded(), /boom/)
  // A rejected first call must not latch the guard shut forever.
  await assert.rejects(guarded(), /boom/)
  assert.equal(calls, 2)
})
