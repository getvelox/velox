import { useCallback, useEffect, useRef, useState, type ReactNode } from 'react'
import { cn } from '@/lib/utils'
import { scrollEdges, scrollEdgeShadow, type ScrollEdges } from '@/lib/scrollAffordance'

/**
 * ScrollPane — the one scrollable pane in the app.
 *
 * Every vertically-scrolling region of CONTENT goes through this rather than a
 * hand-rolled `overflow-y-auto`, and `eslint.config.js` enforces that. The
 * reason is a defect that is invisible to whoever writes the pane:
 *
 * macOS uses overlay scrollbars — invisible until the user is already
 * scrolling. So a capped pane whose content happens to exceed the cap looks
 * EXACTLY like a pane whose content ended there. Nothing is broken; every item
 * is present, scrollable and clickable. It simply gives no reason to try.
 *
 * Walked 2026-08-03: the sidebar hid five entries including Settings at a
 * laptop window height, and an operator reading a screenshot concluded those
 * pages did not exist. That is the failure mode — not a rendering fault, a
 * silent one, and it repeats anywhere a `max-h-*` meets a list of unknown
 * length. Auditing found ten such panes; this is the fix applied once.
 *
 * The affordance is an inset SHADOW at whichever edge has more beyond it — the
 * conventional cue, and the one that needs no learning. Two alternatives were
 * built and rejected on the way here: an overlaid gradient (must match each
 * surface's background in both themes, so every caller is a chance to mismatch
 * it), and a content mask (background-agnostic, but it DIMS the edge — on a
 * dense list that ghosts a real row, and it fights a sticky header whose whole
 * job is to own that band). A shadow leaves content legible and composes with
 * a sticky header exactly as the platform convention does.
 */
export function ScrollPane({
  children,
  className,
  as: Tag = 'div',
  ...rest
}: {
  children: ReactNode
  className?: string
  /** Element to render. Panes carry real semantics — nav, ul, pre, main — keep them. */
  as?: 'div' | 'nav' | 'ul' | 'pre' | 'main'
} & React.HTMLAttributes<HTMLElement>) {
  const ref = useRef<HTMLElement | null>(null)
  const [edges, setEdges] = useState<ScrollEdges>({ top: false, bottom: false })
  const sync = useCallback(() => {
    const el = ref.current
    if (!el) return
    setEdges(scrollEdges(el))
  }, [])

  useEffect(() => {
    sync()
    const el = ref.current
    if (!el) return
    // Observe the pane AND its children. Resizing the window changes
    // clientHeight; content arriving (a list loading, a badge appearing)
    // changes scrollHeight without the pane resizing at all. Watching only one
    // strands the fade in whichever state it was first measured in.
    const ro = new ResizeObserver(sync)
    ro.observe(el)
    for (const child of Array.from(el.children)) ro.observe(child)
    return () => ro.disconnect()
  }, [sync, children])

  const shadow = scrollEdgeShadow(edges)

  return (
    <Tag
      // eslint-disable-next-line no-restricted-syntax -- this IS the primitive the gate points callers at
      ref={ref as never}
      onScroll={sync}
      className={cn('overflow-y-auto', className)}
      style={shadow ? { boxShadow: shadow } : undefined}
      {...rest}
    >
      {children}
    </Tag>
  )
}
