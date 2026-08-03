// Guards the sidebar's scroll affordance.
//
// The defect this exists for: on macOS the scrollbar is invisible until you
// scroll, so a nav with content past the fold looked exactly like a nav that
// had ended. At an 800px window that hid five items including Settings —
// reachable, clickable, and entirely undiscoverable.
//
// The boundary cases are where this kind of predicate goes wrong: a fade stuck
// on at the true bottom reads as a rendering bug, and a fade that never appears
// leaves the original problem in place.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { scrollEdges, scrollFadeMask } from '../src/lib/scrollAffordance.ts'

// The real sidebar measurement at a 1440x800 window, walked 2026-08-03.
const SIDEBAR_800 = { scrollTop: 0, clientHeight: 628, scrollHeight: 857 }

test('content below the fold shows a bottom fade only', () => {
  assert.deepEqual(scrollEdges(SIDEBAR_800), { top: false, bottom: true })
})

test('scrolled to the very bottom clears the bottom fade', () => {
  const atBottom = { ...SIDEBAR_800, scrollTop: 857 - 628 }
  assert.deepEqual(scrollEdges(atBottom), { top: true, bottom: false })
})

test('scrolled to the middle shows both edges', () => {
  const middle = { ...SIDEBAR_800, scrollTop: 100 }
  assert.deepEqual(scrollEdges(middle), { top: true, bottom: true })
})

test('a pane that fits shows no fade at all', () => {
  // The tall-window case: every nav item visible, nothing to hint at.
  assert.deepEqual(
    scrollEdges({ scrollTop: 0, clientHeight: 900, scrollHeight: 857 }),
    { top: false, bottom: false },
  )
})

test('exactly-fitting content shows no fade', () => {
  assert.deepEqual(
    scrollEdges({ scrollTop: 0, clientHeight: 857, scrollHeight: 857 }),
    { top: false, bottom: false },
  )
})

test('sub-pixel slack does not strand a fade on', () => {
  // Fractional layout heights leave a fraction of a pixel at the true bottom.
  // Without the epsilon this reports bottom:true forever and the fade never
  // clears — indistinguishable from a stuck overlay.
  const nearlyBottom = { scrollTop: 228.6, clientHeight: 628, scrollHeight: 857 }
  assert.equal(scrollEdges(nearlyBottom).bottom, false)

  const nearlyTop = { scrollTop: 0.4, clientHeight: 628, scrollHeight: 857 }
  assert.equal(scrollEdges(nearlyTop).top, false)
})

test('a one-pixel overflow is not worth a fade', () => {
  assert.deepEqual(
    scrollEdges({ scrollTop: 0, clientHeight: 856, scrollHeight: 857 }),
    { top: false, bottom: false },
  )
})

test('zero-height pane (hidden / not yet laid out) claims no edges', () => {
  // The mobile drawer's nav measures 0 before it opens; it must not flash a
  // fade on mount.
  assert.deepEqual(
    scrollEdges({ scrollTop: 0, clientHeight: 0, scrollHeight: 0 }),
    { top: false, bottom: false },
  )
})

// The mask is what actually renders the affordance. An overlay gradient would
// have to match each surface's background (card / popover / muted, light and
// dark); masking the content is background-agnostic, so a new caller cannot get
// it subtly wrong.
test('no mask when nothing overflows — the style is dropped, not an identity mask', () => {
  assert.equal(scrollFadeMask({ top: false, bottom: false }, 24), undefined)
})

test('bottom-only fades the bottom edge and leaves the top solid', () => {
  const m = scrollFadeMask({ top: false, bottom: true }, 24)
  assert.equal(m, 'linear-gradient(to bottom, #000 0, #000 calc(100% - 24px), transparent 100%)')
})

test('top-only fades the top edge and leaves the bottom solid', () => {
  const m = scrollFadeMask({ top: true, bottom: false }, 24)
  assert.equal(m, 'linear-gradient(to bottom, transparent 0, #000 24px, #000 100%)')
})

test('mid-scroll fades both edges', () => {
  const m = scrollFadeMask({ top: true, bottom: true }, 24)
  assert.equal(m, 'linear-gradient(to bottom, transparent 0, #000 24px, #000 calc(100% - 24px), transparent 100%)')
})

test('the fade depth is honoured', () => {
  assert.match(scrollFadeMask({ top: false, bottom: true }, 8)!, /calc\(100% - 8px\)/)
})
