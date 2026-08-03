// Scroll-edge affordance for scrollable panes.
//
// macOS uses overlay scrollbars: the scrollbar is INVISIBLE until the user is
// already scrolling. On a pane whose content happens to end at the fold, that
// makes "there is more below" and "that is everything" render identically.
//
// The sidebar hit exactly this (walked 2026-08-03): at an 800px window, 229px
// of navigation sat below the fold — Audit Log, Webhooks, API Keys, Settings
// and Test Clocks — with the list appearing to simply end at "Dunning
// policies". Nothing was broken: the pane scrolls fine and every item is
// reachable and clickable. It just gave no reason to try.
//
// The fix is a fade at whichever edge has content beyond it, which is what
// Linear, Vercel and Notion all do. This module is the "is there more?"
// predicate, kept pure so the boundary conditions are testable without a DOM.

/** The subset of a scrollable element this needs. */
export interface ScrollMetrics {
  scrollTop: number
  clientHeight: number
  scrollHeight: number
}

export interface ScrollEdges {
  /** Content exists above the visible region. */
  top: boolean
  /** Content exists below the visible region. */
  bottom: boolean
}

// Sub-pixel slack. Fractional layout heights and browser zoom routinely leave
// scrollTop + clientHeight a hair under scrollHeight at the true bottom; without
// this the fade never quite disappears, which looks like a rendering artifact.
const EPSILON = 1

export function scrollEdges(m: ScrollMetrics): ScrollEdges {
  // Not scrollable at all — no edge can have content beyond it.
  if (m.scrollHeight <= m.clientHeight + EPSILON) {
    return { top: false, bottom: false }
  }
  return {
    top: m.scrollTop > EPSILON,
    bottom: m.scrollTop + m.clientHeight < m.scrollHeight - EPSILON,
  }
}

/**
 * The inset shadow that marks whichever edge has content beyond it. Returns
 * undefined when nothing should be marked, so the caller drops the style
 * rather than painting a no-op.
 *
 * A SHADOW rather than a fade, per the established affordance pattern: a fade
 * dims the content at the edge, which on a dense list ghosts a real row and
 * reads as a rendering artifact — and it fights a sticky header, whose whole
 * job is to own that band. A shadow leaves content legible, and composes with
 * a sticky header the way the platform convention already does (content
 * scrolling under a header that casts a shadow).
 *
 * Inset on the container, so it pins to the pane's edges rather than scrolling
 * with the content, and it paints beneath the text rather than over it.
 */
export function scrollEdgeShadow(edges: ScrollEdges): string | undefined {
  const parts: string[] = []
  if (edges.top) parts.push('inset 0 9px 7px -8px var(--scroll-shadow)')
  if (edges.bottom) parts.push('inset 0 -9px 7px -8px var(--scroll-shadow)')
  return parts.length ? parts.join(', ') : undefined
}
