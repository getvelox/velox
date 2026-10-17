import { endOfDayInTZ, formatYMDInTZ, startOfDayInTZ } from './dates'
import { rollingWindow, type EffectiveNow } from './effectiveNow'

// Windows for the customer page's Activity and Margin cards (ADR-121).
//
// Both cards open on the customer's current billing period rather than a
// rolling "last 30 days". A rolling window never lines up with a billing
// period (periods run 28 to 31 days and start on the anchor day), so its
// totals disagreed with the invoice shown on the same page. Billing tools
// that show per-customer usage anchor it to the billing period too (Orb's
// customer costs, Lago's current usage). The rolling presets stay as options.
//
// With several subscriptions on different anchors there is no single
// customer period: "current period" is the cycle of the subscription whose
// cycle started last (the server's GET /v1/customers/{id}/usage default), and
// the cards print that window so the figure is never unlabelled.

// InvoicePeriodRow is the slice of an invoice these helpers read.
export interface InvoicePeriodRow {
  status: string
  subscription_id?: string
  billing_period_start?: string
  billing_period_end?: string
}

// lastInvoicedPeriod returns the half-open billing period [from, to) of the
// customer's most recent subscription invoice: the one whose period ends
// last. Drafts are not invoiced yet and voided invoices no longer bill
// anything, so both are skipped. One-off invoices (no subscription) have no
// billing period. null when no invoice qualifies.
export function lastInvoicedPeriod(invoices: InvoicePeriodRow[]): { from: string; to: string } | null {
  let best: { from: string; to: string; endMs: number; startMs: number } | null = null
  for (const inv of invoices) {
    if (inv.status === 'draft' || inv.status === 'voided') continue
    if (!inv.subscription_id || !inv.billing_period_start || !inv.billing_period_end) continue
    const startMs = new Date(inv.billing_period_start).getTime()
    const endMs = new Date(inv.billing_period_end).getTime()
    if (Number.isNaN(startMs) || Number.isNaN(endMs) || endMs <= startMs) continue
    if (!best || endMs > best.endMs || (endMs === best.endMs && startMs > best.startMs)) {
      best = { from: inv.billing_period_start, to: inv.billing_period_end, endMs, startMs }
    }
  }
  return best ? { from: best.from, to: best.to } : null
}

// isOutsideWindow reports whether an instant falls outside [from, to), the
// convention every usage and margin query uses (`timestamp < to`).
export function isOutsideWindow(iso: string, from: string, to: string): boolean {
  const t = new Date(iso).getTime()
  return t < new Date(from).getTime() || t >= new Date(to).getTime()
}

// rangeIncluding returns the civil-date range (yyyy-mm-dd, inclusive) from the
// day of `latestISO` to the day of `nowMs`, in the tenant timezone. It is the
// one-click "include the latest usage" range. Usage stamped after `now` (a
// test clock moved back, or a client sending future timestamps) still gets a
// range that covers it: the end is never before the start.
export function rangeIncluding(latestISO: string, nowMs: number, timezone?: string): { from: string; to: string } {
  const from = formatYMDInTZ(latestISO, timezone)
  const today = formatYMDInTZ(new Date(nowMs).toISOString(), timezone)
  return { from, to: today < from ? from : today }
}

// MarginTotals is the slice of a margin report that says whether the window
// had any usage at all.
export interface MarginTotals {
  revenue_cents: number
  cost_micros: number
  unresolved_events: number
  by_model: unknown[]
}

// MARGIN_EMPTY_NOTE is shown when the window holds no usage. It states only
// that, because whether provider rates exist is not known here (#1111).
export const MARGIN_EMPTY_NOTE = 'No usage in this window yet.'

// marginWindowIsEmpty is true when the window holds no usage: no revenue, no
// provider cost, no uncosted events and no model rows.
export function marginWindowIsEmpty(rep: MarginTotals): boolean {
  return rep.revenue_cents === 0 && rep.cost_micros === 0 && rep.unresolved_events === 0 && rep.by_model.length === 0
}

// usageWindowIsEmpty is true when no event at all fell in the Activity
// window. It reads the daily buckets, which are raw quantity sums including
// usage that matched no price, so unpriced usage does not count as "empty".
export function usageWindowIsEmpty(buckets: { per_meter: Record<string, string | number> }[]): boolean {
  return buckets.every(b => Object.values(b.per_meter).every(q => Number(q) === 0))
}

// MarginPreset is a Margin card window choice. 'cycle' (the default) and
// 'last_invoiced' are billing periods; the others are rolling or picked.
export type MarginPreset = 'cycle' | 'last_invoiced' | '7d' | '30d' | '90d' | 'custom'

export const MARGIN_PRESETS: { key: MarginPreset; label: string; days: number }[] = [
  { key: 'cycle', label: 'Current period', days: 0 },
  { key: 'last_invoiced', label: 'Last invoiced period', days: 0 },
  { key: '7d', label: 'Last 7 days', days: 7 },
  { key: '30d', label: 'Last 30 days', days: 30 },
  { key: '90d', label: 'Last 90 days', days: 90 },
  { key: 'custom', label: 'Custom', days: 0 },
]

// MarginWindow is what the card can query for a preset:
//   ready      [from, to) to send; `period` marks a billing period, which the
//              card prints as its inclusive dates
//   waiting    the period is still loading
//   pick-dates a custom range is missing an end
//   no-cycle   the customer has no current billing period
//   no-invoice the customer has no subscription invoice yet
export type MarginWindow =
  | { kind: 'ready'; from: string; to: string; period: boolean }
  | { kind: 'waiting' | 'pick-dates' | 'no-cycle' | 'no-invoice' }

// marginWindowFor resolves a preset to a window. `cycle` and `lastInvoiced`
// are the query results: undefined while loading, null when there is none.
export function marginWindowFor(
  preset: MarginPreset,
  inputs: {
    cycle: { from: string; to: string } | null | undefined
    lastInvoiced: { from: string; to: string } | null | undefined
    now: EffectiveNow
    customFrom: string
    customTo: string
    timezone?: string
  },
): MarginWindow {
  switch (preset) {
    case 'cycle':
      if (inputs.cycle === undefined) return { kind: 'waiting' }
      if (inputs.cycle === null) return { kind: 'no-cycle' }
      return { kind: 'ready', from: inputs.cycle.from, to: inputs.cycle.to, period: true }
    case 'last_invoiced':
      if (inputs.lastInvoiced === undefined) return { kind: 'waiting' }
      if (inputs.lastInvoiced === null) return { kind: 'no-invoice' }
      return { kind: 'ready', from: inputs.lastInvoiced.from, to: inputs.lastInvoiced.to, period: true }
    case 'custom':
      if (!inputs.customFrom || !inputs.customTo) return { kind: 'pick-dates' }
      return {
        kind: 'ready',
        from: startOfDayInTZ(inputs.customFrom, inputs.timezone),
        to: endOfDayInTZ(inputs.customTo, inputs.timezone),
        period: false,
      }
    default: {
      const days = MARGIN_PRESETS.find(p => p.key === preset)?.days ?? 30
      return { kind: 'ready', ...rollingWindow(days, inputs.now), period: false }
    }
  }
}

// The windows each card opens on. Both are the current billing period.
export const DEFAULT_MARGIN_PRESET: MarginPreset = 'cycle'
export const DEFAULT_ACTIVITY_PRESET = 'cycle' as const
