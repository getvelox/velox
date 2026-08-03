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
import { scrollEdges, scrollEdgeShadow } from '../src/lib/scrollAffordance.ts'

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

// The shadow is what renders the affordance. A fade was the first attempt and
// was replaced: it dims the edge, which ghosts a real row on a dense list and
// fights a sticky header (the webhook event picker's "Select all · 0 of 37"
// bar). A shadow leaves content legible and is the conventional cue.
test('no shadow when nothing overflows — the style is dropped, not a no-op', () => {
  assert.equal(scrollEdgeShadow({ top: false, bottom: false }), undefined)
})

test('bottom-only marks just the bottom edge', () => {
  assert.equal(scrollEdgeShadow({ top: false, bottom: true }),
    'inset 0 -9px 7px -8px var(--scroll-shadow)')
})

test('top-only marks just the top edge', () => {
  assert.equal(scrollEdgeShadow({ top: true, bottom: false }),
    'inset 0 9px 7px -8px var(--scroll-shadow)')
})

test('mid-scroll marks both edges', () => {
  assert.equal(scrollEdgeShadow({ top: true, bottom: true }),
    'inset 0 9px 7px -8px var(--scroll-shadow), inset 0 -9px 7px -8px var(--scroll-shadow)')
})

test('the shadow colour is a theme token, not a hardcoded black', () => {
  // A black shadow is invisible on a dark surface; the token carries a
  // different alpha per theme.
  assert.match(scrollEdgeShadow({ top: true, bottom: true })!, /var\(--scroll-shadow\)/)
  assert.doesNotMatch(scrollEdgeShadow({ top: true, bottom: true })!, /#000|rgba?\(/)
})
