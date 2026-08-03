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
 * The CSS mask that fades the pane's own content at whichever edge has more
 * beyond it. Returns undefined when nothing should fade, so the caller can drop
 * the style entirely rather than paint an identity mask.
 *
 * A mask rather than an overlaid gradient div, deliberately: an overlay has to
 * match the pane's background colour, which differs per surface (card, popover,
 * muted) and per theme, so every new caller is a chance to get it subtly wrong
 * — and a mismatched overlay looks like a rendering bug. Masking the content
 * itself is background-agnostic and cannot be mismatched. It also cannot
 * intercept clicks, which an overlay can if anyone forgets pointer-events-none.
 */
export function scrollFadeMask(edges: ScrollEdges, fadePx: number): string | undefined {
  if (!edges.top && !edges.bottom) return undefined
  const from = edges.top ? `transparent 0, #000 ${fadePx}px` : '#000 0'
  const to = edges.bottom ? `#000 calc(100% - ${fadePx}px), transparent 100%` : '#000 100%'
  return `linear-gradient(to bottom, ${from}, ${to})`
}
