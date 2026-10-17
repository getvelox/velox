import { useMemo, useState } from 'react'
import { Link } from 'react-router-dom'
import { Loader2 } from 'lucide-react'

import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { DatePicker } from '@/components/ui/date-picker'
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from '@/components/ui/table'
import { formatCents, formatDate, getCurrencySymbol } from '@/lib/api'
import { formatCivilPeriod } from '@/lib/dates'
import { marginCardView } from '@/lib/actionOutcomes'
import { marginCostNote, marginCostState } from '@/lib/marginCost'
import {
  DEFAULT_MARGIN_PRESET, MARGIN_EMPTY_NOTE, MARGIN_PRESETS, isOutsideWindow, marginWindowFor, marginWindowIsEmpty, rangeIncluding, type MarginPreset,
} from '@/lib/usageWindow'
import { useGetV1CustomersIdMargin } from '@/lib/gen/queries.gen'
import { useEffectiveNow } from '@/hooks/useClockFrozenMap'
import { useCurrentCycleUsage, useLastInvoicedPeriod, useLatestUsageAt } from '@/hooks/useCustomerUsageWindows'

// Margin (ADR-079): rated usage revenue vs stamped provider COGS over an
// operator-picked window. Operator-only surface — COGS never renders on
// customer-facing pages. Attribution is honest by design: per-model margin
// shows only where pricing rules pin the model; everything else appears as
// "not model-attributed" rather than being split by heuristic.
//
// The card opens on the customer's current billing period, with the last
// invoiced period one click away (ADR-121). A rolling 30 days never matched a
// billing period, so its total disagreed with the invoice on the same page.
// currency: the customer's currency (useCustomerCurrency). The report names
// no revenue currency when the window has no revenue, and that $0 must still
// be in the customer's currency. Undefined while it loads: "—" until then.
export function MarginCard({ customerId, customerTestClockId, currency }: { customerId: string; customerTestClockId?: string; currency?: string }) {
  const [preset, setPreset] = useState<MarginPreset>(DEFAULT_MARGIN_PRESET)
  const [customFrom, setCustomFrom] = useState('')
  const [customTo, setCustomTo] = useState('')

  // For a clock-pinned customer, anchor the rolling presets' upper bound to
  // the clock's frozen_time: provider-costed usage events are stamped in
  // simulation time (usage/service.go), so a wall-clock "last 30 days" sits
  // before them. The billing periods come from the server, already on the
  // customer's clock. Custom absolute dates are left untouched.
  const now = useEffectiveNow(customerTestClockId)

  const cycleQuery = useCurrentCycleUsage(customerId, customerTestClockId, preset === 'cycle')
  const invoicedQuery = useLastInvoicedPeriod(customerId, preset === 'last_invoiced')

  const marginWindow = useMemo(() => marginWindowFor(preset, {
    cycle: cycleQuery.data === undefined ? undefined : cycleQuery.data ? cycleQuery.data.period : null,
    lastInvoiced: invoicedQuery.data,
    now,
    customFrom,
    customTo,
  }), [preset, cycleQuery.data, invoicedQuery.data, now, customFrom, customTo])
  const params = marginWindow.kind === 'ready' ? { from: marginWindow.from, to: marginWindow.to } : undefined
  const windowError = (preset === 'cycle' && cycleQuery.isError) || (preset === 'last_invoiced' && invoicedQuery.isError)

  const { data: rep, isError: marginError, refetch } = useGetV1CustomersIdMargin(customerId, params, {
    query: { enabled: params !== undefined },
  })
  const isError = windowError || marginError
  const view = marginCardView({ hasWindow: marginWindow.kind !== 'pick-dates', isError, hasData: !!rep })

  // An empty window points at the customer's newest usage, so "no usage
  // here" never reads as "no usage at all".
  const empty = !!rep && marginWindowIsEmpty(rep)
  const latestQuery = useLatestUsageAt(customerId, empty)
  const latestAt = empty && params && latestQuery.data && isOutsideWindow(latestQuery.data, params.from, params.to)
    ? latestQuery.data
    : null
  const showLatest = () => {
    if (!latestAt) return
    const r = rangeIncluding(latestAt, now)
    setCustomFrom(r.from)
    setCustomTo(r.to)
    setPreset('custom')
  }
  const retry = () => {
    if (windowError) {
      void cycleQuery.refetch()
      void invoicedQuery.refetch()
    } else {
      void refetch()
    }
  }

  // Revenue is in the currency of the customer's prices; provider cost is in
  // the report's cost currency (USD). The API gives no margin when they
  // differ, and no revenue currency when the window mixes currencies.
  const revenueCurrency = rep?.revenue_currency
  const costCurrency = rep?.cost_currency || 'USD'
  const mixedRevenue = !!rep && !revenueCurrency && rep.revenue_cents > 0
  const currencyMismatch = !!rep && !!revenueCurrency && revenueCurrency !== costCurrency && rep.revenue_cents > 0
  const shownCurrency = revenueCurrency || currency
  const billed = (cents: number) => (mixedRevenue || !shownCurrency ? '—' : formatCents(cents, shownCurrency))
  // micros (1e-6 of the currency unit) → display amount.
  const cost = (micros: number) =>
    `${getCurrencySymbol(costCurrency)}${(micros / 1_000_000).toFixed(micros >= 100_000 ? 2 : 4)}`
  // No event carries a provider cost: the cost is unknown, not $0, and the
  // margin is not 100% (the server leaves margin_bps out).
  const costState = rep ? marginCostState(rep) : 'measured'
  const costNote = marginCostNote(costState)
  const costKnown = costState === 'measured'

  return (
    <Card>
      <CardHeader>
        <div className="flex items-start justify-between gap-3 flex-wrap">
          <div>
            <CardTitle>Margin</CardTitle>
            <CardDescription>
              Usage billing vs your provider costs. Excludes base fees, credits,
              and taxes — this is usage unit economics, not accounting margin.
            </CardDescription>
            {marginWindow.kind === 'ready' && marginWindow.period && (
              <p className="text-xs text-muted-foreground mt-1 tabular-nums">
                {formatCivilPeriod(marginWindow.from, marginWindow.to, undefined)}
              </p>
            )}
          </div>
          <div className="flex flex-wrap items-center gap-2">
            <select
              aria-label="Margin window"
              className="h-8 rounded-md border border-input bg-transparent px-2 text-sm"
              value={preset}
              onChange={e => setPreset(e.target.value as MarginPreset)}
            >
              {MARGIN_PRESETS.map(p => (
                <option key={p.key} value={p.key}>{p.label}</option>
              ))}
            </select>
            {preset === 'custom' && (
              <>
                {/* w-48: the field also holds a calendar icon, a clear button and
                    a chevron, so w-40 cut the year off "Sep 30, 2026". */}
                <DatePicker value={customFrom} onChange={setCustomFrom} placeholder="From" className="w-48" />
                <DatePicker value={customTo} onChange={setCustomTo} placeholder="To" className="w-48"
                  minDate={customFrom ? new Date(customFrom + 'T00:00:00') : undefined} />
              </>
            )}
          </div>
        </div>
      </CardHeader>
      <CardContent>
        {view === 'pick-dates' ? (
          <p className="text-sm text-muted-foreground p-6 text-center">
            Pick a start and end date to see margin for that range.
          </p>
        ) : view === 'error' ? (
          <div className="flex flex-col items-center gap-3 p-6 text-center">
            <p className="text-sm text-foreground">Couldn&apos;t load margin. Try again.</p>
            <Button variant="outline" size="sm" onClick={retry}>Retry</Button>
          </div>
        ) : marginWindow.kind === 'no-cycle' || marginWindow.kind === 'no-invoice' ? (
          <div className="flex flex-col items-center gap-3 p-6 text-center">
            <p className="text-sm text-muted-foreground">
              {marginWindow.kind === 'no-cycle'
                ? 'This customer has no current billing period: no active subscription.'
                : 'This customer has no invoiced billing period yet.'}
            </p>
            <Button variant="outline" size="sm" onClick={() => setPreset('30d')}>Show last 30 days</Button>
          </div>
        ) : view === 'loading' || !rep ? (
          <div className="flex justify-center p-6">
            <Loader2 className="h-5 w-5 animate-spin text-muted-foreground" />
          </div>
        ) : (
          <>
            <div className="grid grid-cols-3 gap-4">
              <div>
                <p className="text-xs text-muted-foreground uppercase tracking-wide">Usage billed</p>
                <p className="text-lg font-semibold text-foreground mt-0.5">{billed(rep.revenue_cents)}</p>
              </div>
              <div>
                <p className="text-xs text-muted-foreground uppercase tracking-wide">Provider cost</p>
                <p className="text-lg font-semibold text-foreground mt-0.5">{costKnown ? cost(rep.cost_micros) : '—'}</p>
              </div>
              <div>
                <p className="text-xs text-muted-foreground uppercase tracking-wide">Margin</p>
                <p className="text-lg font-semibold text-foreground mt-0.5">
                  {rep.margin_bps != null ? `${(rep.margin_bps / 100).toFixed(1)}%` : '—'}
                </p>
              </div>
            </div>

            {costNote && (
              <p className="text-xs text-muted-foreground mt-3">
                {costNote}{' '}
                <Link to="/provider-costs" className="text-primary underline-offset-2 hover:underline">
                  {costState === 'no-rates' ? 'Set up provider costs' : 'Provider costs'}
                </Link>
              </p>
            )}
            {currencyMismatch && costKnown && (
              <p className="text-xs text-muted-foreground mt-3">
                Margin needs billing and provider cost in one currency. This customer is
                billed in {revenueCurrency} and provider cost is in {costCurrency}, so no
                margin is shown.
              </p>
            )}
            {mixedRevenue && (
              <p className="text-xs text-muted-foreground mt-3">
                Usage in this window was billed in more than one currency, so the billed
                total and the margin are not shown.
              </p>
            )}

            {rep.by_model.length > 0 && (
              <Table className="mt-4">
                <TableHeader>
                  <TableRow>
                    <TableHead>Model</TableHead>
                    <TableHead className="text-right">Billed</TableHead>
                    <TableHead className="text-right">Cost</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {rep.by_model.map(m => (
                    <TableRow key={m.model}>
                      <TableCell className="text-sm font-mono">{m.model}</TableCell>
                      <TableCell className="text-sm text-right">
                        {m.attributed
                          ? billed(m.revenue_cents ?? 0)
                          : <Badge variant="outline">not model-priced</Badge>}
                      </TableCell>
                      <TableCell className="text-sm text-right">{cost(m.cost_micros)}</TableCell>
                    </TableRow>
                  ))}
                  {rep.unattributed_revenue_cents > 0 && (
                    <TableRow>
                      <TableCell className="text-sm text-muted-foreground">Not model-attributed</TableCell>
                      <TableCell className="text-sm text-right">{billed(rep.unattributed_revenue_cents)}</TableCell>
                      <TableCell className="text-sm text-right text-muted-foreground">—</TableCell>
                    </TableRow>
                  )}
                </TableBody>
              </Table>
            )}

            {rep.unresolved_events > 0 && (
              <p className="text-xs text-muted-foreground mt-3">
                {rep.unresolved_events === 1
                  ? '1 usage event has no provider cost'
                  : `${rep.unresolved_events.toLocaleString()} usage events have no provider cost`}
                : no rate in Provider costs matched it when it arrived. Events that arrive
                after you add a matching rate are costed.
              </p>
            )}
            {empty && latestAt ? (
              <div className="flex flex-wrap items-center gap-3 mt-3">
                <p className="text-sm text-muted-foreground">
                  No usage in this window. Latest usage: {formatDate(latestAt)}.
                </p>
                <Button variant="outline" size="sm" onClick={showLatest}>
                  Show from {formatDate(latestAt)}
                </Button>
              </div>
            ) : empty && (
              <p className="text-sm text-muted-foreground mt-2">{MARGIN_EMPTY_NOTE}</p>
            )}
          </>
        )}
      </CardContent>
    </Card>
  )
}
