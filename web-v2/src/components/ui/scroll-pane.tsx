import { useCallback, useEffect, useRef, useState, type ReactNode } from 'react'
import { cn } from '@/lib/utils'
import { scrollEdges, scrollFadeMask, type ScrollEdges } from '@/lib/scrollAffordance'

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
 * The affordance is a MASK on the pane's own content, not an overlaid gradient.
 * An overlay must match the surface's background (card / popover / muted, in
 * both themes), so every new caller is a fresh chance to mismatch it — and a
 * mismatched overlay reads as a rendering bug. A mask is background-agnostic
 * and cannot intercept clicks.
 */
export function ScrollPane({
  children,
  className,
  as: Tag = 'div',
  fadePx = 24,
  ...rest
}: {
  children: ReactNode
  className?: string
  /** Element to render. Panes carry real semantics — nav, ul, pre — keep them. */
  as?: 'div' | 'nav' | 'ul' | 'pre'
  /** Depth of the fade. Smaller for short panes so it doesn't swallow a row. */
  fadePx?: number
} & React.HTMLAttributes<HTMLElement>) {
  const ref = useRef<HTMLElement | null>(null)
  const [edges, setEdges] = useState<ScrollEdges>({ top: false, bottom: false })

  const sync = useCallback(() => {
    const el = ref.current
    if (el) setEdges(scrollEdges(el))
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

  const mask = scrollFadeMask(edges, fadePx)

  return (
    <Tag
      // eslint-disable-next-line no-restricted-syntax -- this IS the primitive the gate points callers at
      ref={ref as never}
      onScroll={sync}
      className={cn('overflow-y-auto', className)}
      style={mask ? { maskImage: mask, WebkitMaskImage: mask } : undefined}
      {...rest}
    >
      {children}
    </Tag>
  )
}
