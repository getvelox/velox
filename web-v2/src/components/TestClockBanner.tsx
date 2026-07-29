import { Link } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import { FlaskConical } from 'lucide-react'
import { api, formatDateTime, ApiError } from '@/lib/api'

// TestClockBanner sits at the top of any detail page whose entity is
// pinned (directly or via its subscription) to a test clock. Test
// clocks let operators advance simulated time independently of
// wall-clock; rows finalized / billed / dunned during a clock-advance
// carry the simulated timestamp, which can render as a future date in
// the activity timeline (e.g. "Invoice finalized · Jun 1, 2026" while
// the operator is reading the page on May 2). The banner sets the
// expectation upfront so the operator stops wondering "why is this in
// the future."
//
// Crucially, the banner shows the test clock's CURRENT frozen_time so
// the operator has a reference point. Without this, an invoice with
// "Due Jul 17, 2026" rendered alongside a "Past due" pill looks
// contradictory — until you know the simulation has been advanced to
// Sep 2, 2026 and Jul 17 is genuinely past in simulation time.
// Industry pattern (Stripe Test Clocks UI): the simulation timestamp
// is always visible on every page that consults it.
//
// React Query dedupes the test-clock fetch across components — the
// parent page often already loads the same clock for other UI
// purposes, so this hook adds no network round-trip.
export function TestClockBanner({ testClockId }: { testClockId: string }) {
  const { data: clock, error } = useQuery({
    queryKey: ['test-clock', testClockId],
    queryFn: () => api.getTestClock(testClockId),
    enabled: !!testClockId,
    // Don't retry on 404 — the clock has been deleted (ADR-016 keeps
    // the historical pointer on the customer / subscription / invoice
    // even after the clock itself is soft-deleted, so the banner has
    // to render this state without 4 wasted retries first).
    retry: (failureCount, err) => {
      if (err instanceof ApiError && err.status === 404) return false
      return failureCount < 3
    },
  })

  // Deleted-clock state: customer / sub / invoice still references the
  // clock by id (intentionally preserved per ADR-016 for audit), but
  // the clock itself is gone. Render an explanatory variant without
  // the View-clock link — the link would 404 and the operator
  // shouldn't be invited into a dead route.
  const clockDeleted = error instanceof ApiError && error.status === 404

  return (
    <div className="mb-6 flex items-center gap-3 rounded-md border border-amber-300 bg-amber-50 px-4 py-3 text-sm text-amber-900 dark:border-amber-500/30 dark:bg-amber-500/10 dark:text-amber-300">
      <FlaskConical size={16} className="shrink-0" />
      <div className="flex-1">
        {clockDeleted ? (
          <>
            <span className="font-medium">Test clock deleted.</span>{' '}
            This record was attached to a test clock that has since been
            deleted. Some dates below carry simulated timestamps from when
            the clock was active — they follow the clock's calendar, not
            today's date.
          </>
        ) : (
          <>
            <span className="font-medium">Test clock simulation.</span>{' '}
            {clock ? (
              <>
                Currently at <span className="font-mono font-medium">{formatDateTime(clock.frozen_time)}</span>.
                Dates on this page follow the clock's calendar — rows produced
                during a clock advance (finalize, dunning runs, retries) carry
                the simulated timestamp, with any real-world moment shown as a
                "Recorded" line beneath.
              </>
            ) : (
              <>
                Dates on this page follow the clock's calendar — rows produced
                during a clock advance (finalize, dunning runs, retries) carry
                the simulated timestamp, with any real-world moment shown as a
                "Recorded" line beneath.
              </>
            )}
          </>
        )}
      </div>
      {!clockDeleted && (
        <Link
          to={`/test-clocks/${testClockId}`}
          className="shrink-0 text-xs underline-offset-2 hover:underline"
        >
          View clock
        </Link>
      )}
    </div>
  )
}
