// Window presets for the Usage Events page.
//
// The page defaults to a BOUNDED window because its stat cards are a
// server-side aggregate over everything in scope: unfiltered, that scanned the
// tenant's entire event history on every page load. Two constraints shape the
// design:
//
//   - 'all' must stay reachable. Bounding the default is a performance change;
//     removing the ability to ask "ever" would be a product regression.
//   - There must be exactly ONE answer to "what range am I looking at?".
//     'custom' is a distinct mode rather than a pair of pickers sitting beside
//     an active preset, because two live controls for one question leave the
//     operator unable to tell which is in force.
//
// The resolution is a pure function of (range, custom dates, today) so it can
// be tested without a DOM or a clock.

export type RangeKey = '7d' | '30d' | '90d' | '12m' | 'all' | 'custom'

export const RANGES: Record<RangeKey, { label: string; amount: number; unit: 'day' | 'month' }> = {
  '7d': { label: 'Last 7 days', amount: 7, unit: 'day' },
  '30d': { label: 'Last 30 days', amount: 30, unit: 'day' },
  '90d': { label: 'Last 90 days', amount: 90, unit: 'day' },
  '12m': { label: 'Last 12 months', amount: 12, unit: 'month' },
  all: { label: 'All time', amount: 0, unit: 'day' },
  custom: { label: 'Custom range', amount: 0, unit: 'day' },
}

export const RANGE_KEYS = Object.keys(RANGES) as RangeKey[]
export const DEFAULT_RANGE: RangeKey = '30d'

// parseRange coerces an untrusted URL value. An unknown ?range= must land on
// the bounded default, never on 'all' — a typo'd URL should not silently
// reinstate the full-history scan this module exists to prevent.
export function parseRange(raw: string | null | undefined): RangeKey {
  return raw && (RANGE_KEYS as string[]).includes(raw) ? (raw as RangeKey) : DEFAULT_RANGE
}

export interface Window {
  from: string // yyyy-mm-dd, '' means unbounded
  to: string // yyyy-mm-dd, '' means "until now"
}

// resolveWindow maps a range to the civil dates actually queried. `civilAgo` is
// injected so this stays pure: the page passes the tenant-TZ helper, tests pass
// a fixed one.
//
// A preset sets only `from` and leaves `to` open, so the window tracks forward
// as time passes rather than freezing at the moment the page loaded.
export function resolveWindow(
  range: RangeKey,
  custom: Window,
  civilAgo: (amount: number, unit: 'day' | 'month') => string,
): Window {
  if (range === 'custom') return { from: custom.from, to: custom.to }
  if (range === 'all') return { from: '', to: '' }
  const { amount, unit } = RANGES[range]
  return { from: civilAgo(amount, unit), to: '' }
}

// rangeCaption names the window the stat cards cover. Without it, an operator
// landing on the 30-day default reads a smaller "Total Events" than they saw
// last week and has no way to tell a filter from missing data.
//
// 'custom' reads back the actual dates rather than the word "custom", which
// would say nothing.
export function rangeCaption(range: RangeKey, window: Window): string {
  if (range === 'all') return 'Totals across all time'
  if (range !== 'custom') return `Totals for the ${RANGES[range].label.replace(/^Last /, 'last ')}`
  if (window.from && window.to) return `Totals for ${window.from} to ${window.to}`
  if (window.from) return `Totals from ${window.from} onwards`
  if (window.to) return `Totals up to ${window.to}`
  return 'Totals across all time'
}
