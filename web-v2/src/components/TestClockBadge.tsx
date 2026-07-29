import { FlaskConical } from 'lucide-react'
import { Link } from 'react-router-dom'

// TestClockBadge is the inline marker for entities pinned to a test
// clock. One visible word — "Simulated" — per the ADR-099 vocabulary
// rule ("test clock" is builder jargon; raw clock IDs stay out of
// visible copy). Placement follows the once-per-scope rule: MIXED
// list rows and customer-scoped cards only; detail pages disclose via
// their banner instead.
//
// Two render modes, and the split is load-bearing:
// - Default <span>, NOT a <Link>: many parent rows are <Link>
//   (Dashboard recent invoices, CustomerDetail recent subs/invoices,
//   portal lists). Nesting <a> inside <a> is invalid HTML and causes
//   a React hydration error, so row badges stay informational —
//   navigation is one click away on the entity's detail page.
// - `link` mode for customer-scoped card headers (Credits ledger),
//   which are not inside an anchor: there the chip links straight to
//   the clock detail.
// Tooltip via native title=; amber chip matches the test-mode banner.
export function TestClockBadge({
  testClockId,
  link = false,
  className = '',
}: {
  testClockId: string
  link?: boolean
  className?: string
}) {
  const chipClass = `inline-flex items-center gap-1 rounded border border-amber-500/30 bg-amber-500/10 px-1.5 py-0.5 text-[10px] font-medium leading-none text-amber-800 dark:text-amber-300 ${className}`
  if (link) {
    return (
      <Link
        to={`/test-clocks/${testClockId}`}
        aria-label="View test clock"
        title="Dates are simulated (test clock) — not real time. Click to open the clock."
        className={`${chipClass} hover:border-amber-500/60 hover:bg-amber-500/20`}
      >
        <FlaskConical size={10} />
        Simulated
      </Link>
    )
  }
  return (
    <span
      title="Dates are simulated (test clock) — not real time."
      className={chipClass}
    >
      <FlaskConical size={10} />
      Simulated
    </span>
  )
}

// SimulatedBadge marks an invoice (or invoice row) whose domain timestamps
// were stamped on a test clock's simulated time. Unlike TestClockBadge, it's
// driven by the backend-authoritative `invoice.is_simulated` flag rather than
// a sub→clock map lookup — so it badges manual one-off invoices correctly
// (they have no subscription to look through) and never rots when a clock is
// later unpinned. Same amber aesthetic; non-interactive <span> (safe to nest
// inside row <Link>s). See ADR-030 / feedback_no_heuristic_proxies.
export function SimulatedBadge({
  className = '',
  title = "Dates on this invoice follow its test clock's calendar, not today's date",
}: {
  className?: string
  title?: string
}) {
  return (
    <span
      title={title}
      className={`inline-flex shrink-0 items-center gap-1 rounded border border-amber-500/30 bg-amber-500/10 px-1.5 py-0.5 text-[10px] font-medium leading-none text-amber-800 dark:text-amber-300 ${className}`}
    >
      <FlaskConical size={10} />
      Simulated
    </span>
  )
}
