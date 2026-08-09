// Per-customer usage view. Two stacked sections:
//
//   1. <UpcomingInvoicesSection> — Stripe / Lago / Orb / Metronome
//      pattern. Per-subscription "what will the next invoice charge?"
//      cards, each with cycle progress, projected total, and a link
//      to the last finalized invoice for rollover transparency.
//   2. <ActivitySection> — AWS / Datadog / OpenAI pattern. Customer-
//      level rolling-window view with daily bar chart and per-meter
//      drill-down for multi-dim attribution.
//
// The split is deliberate: cycle-bound and window-bound surfaces
// answer different operator questions ("what comes next?" vs
// "what happened?") and conflating them via tabs (the prior shape)
// produced the cycle-rollover trap where Current cycle showed $0
// even though the customer had a full month of activity. Research
// across 7 reference platforms confirmed this two-surface structure
// is the industry-standard answer.

import { money, unitScale } from '@/lib/priceDisplay'
import { useState, useMemo } from 'react'
import { Link } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import { ResponsiveContainer, BarChart, Bar, XAxis, YAxis, Tooltip, CartesianGrid } from 'recharts'
import { formatInTimeZone } from 'date-fns-tz'
import { api, formatCents, formatRate, formatDate, getTenantTimezone } from '@/lib/api'
import type { CustomerUsage, CustomerUsageMeter, CustomerUsageRule, Subscription, Invoice, InvoicePreview } from '@/lib/api'
import { cn } from '@/lib/utils'
import { cycleProgress, rollingWindow, sinceMs, type EffectiveNow } from '@/lib/effectiveNow'
import { useEffectiveNow, useEffectiveNowResolver } from '@/hooks/useClockFrozenMap'

import { Card, CardContent } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import { Tabs, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { ChevronDown, ChevronRight, Activity, AlertTriangle, Loader2, Receipt } from 'lucide-react'

// ============================================================================
// Top-level wrapper
// ============================================================================

// customerTestClockId: the customer's test-clock pin (ADR-027, customer-level).
// When set, every relative-time / rolling-window surface below reads its "now"
// from the clock's frozen_time instead of wall-clock, so the cycle bar,
// last-invoice "X ago", and activity window all track simulation time. Omitted /
// undefined for wall-clock customers, which fall back to Date.now() unchanged.
export function CostDashboard({ customerId, customerTestClockId }: { customerId: string; customerTestClockId?: string }) {
  return (
    <div className="space-y-6">
      <UpcomingInvoicesSection customerId={customerId} />
      <ActivitySection customerId={customerId} customerTestClockId={customerTestClockId} />
    </div>
  )
}

// ============================================================================
// Upcoming invoices — per-sub cycle + projection + last-invoice ref
// ============================================================================

function UpcomingInvoicesSection({ customerId }: { customerId: string }) {
  const { data: subsRes, isLoading } = useQuery({
    queryKey: ['customer-subs-upcoming', customerId],
    queryFn: () => api.listSubscriptions(`customer_id=${encodeURIComponent(customerId)}`),
  })

  // Per-sub effective-now — the "now" baseline for this sub's cycle bar and
  // last-invoice "X ago". Resolved per subscription (not customer) because the
  // period bounds and invoice created_at were stamped on the sub's own clock.
  // Wall-clock subs (no test_clock_id) resolve to wall time.
  const resolveNow = useEffectiveNowResolver()

  const activeSubs = useMemo(
    () =>
      (subsRes?.data ?? [])
        .filter((s: Subscription) => s.status === 'active' || s.status === 'trialing')
        // Latest period start first — matches the server-side primary-
        // sub heuristic (used to be the only sub displayed; now we
        // show all but the latest is conventionally the "primary").
        .sort((a: Subscription, b: Subscription) => {
          const aStart = a.current_billing_period_start ?? ''
          const bStart = b.current_billing_period_start ?? ''
          return bStart.localeCompare(aStart)
        }),
    [subsRes],
  )

  const [expanded, setExpanded] = useState(false)
  const visibleSubs = expanded ? activeSubs : activeSubs.slice(0, 1)
  const hidden = activeSubs.length - visibleSubs.length

  return (
    <section>
      <h2 className="text-base font-semibold text-foreground mb-3">Upcoming invoices</h2>

      {isLoading ? (
        <Card>
          <CardContent className="p-8 flex justify-center">
            <Loader2 size={20} className="animate-spin text-muted-foreground" />
          </CardContent>
        </Card>
      ) : activeSubs.length === 0 ? (
        <Card>
          <CardContent className="p-8 text-center">
            <Receipt size={20} className="mx-auto text-muted-foreground mb-3" />
            <p className="text-sm text-muted-foreground">
              No active subscriptions. Create one for this customer to see upcoming invoices.
            </p>
          </CardContent>
        </Card>
      ) : (
        <div className="space-y-3">
          {visibleSubs.map((sub: Subscription) => (
            <UpcomingInvoiceCard
              key={sub.id}
              sub={sub}
              customerId={customerId}
              effectiveNow={resolveNow(sub.test_clock_id)}
            />
          ))}
          {hidden > 0 && (
            <button
              type="button"
              onClick={() => setExpanded(true)}
              className="text-xs text-muted-foreground hover:text-foreground inline-flex items-center gap-1"
            >
              + {hidden} more subscription{hidden === 1 ? '' : 's'}
            </button>
          )}
          {expanded && activeSubs.length > 1 && (
            <button
              type="button"
              onClick={() => setExpanded(false)}
              className="text-xs text-muted-foreground hover:text-foreground inline-flex items-center gap-1"
            >
              Hide other subscriptions
            </button>
          )}
        </div>
      )}
    </section>
  )
}

function UpcomingInvoiceCard({ sub, customerId, effectiveNow }: { sub: Subscription; customerId: string; effectiveNow: EffectiveNow }) {
  // Projected next invoice — Stripe Tier 1 create_preview parity.
  // Per-sub call so multi-sub customers see their projections
  // independently rather than rolled-up. Failures are silent — the
  // cycle progress still renders and the operator can click into the
  // sub itself for diagnostic detail.
  const { data: preview } = useQuery({
    queryKey: ['upcoming-preview', customerId, sub.id],
    queryFn: async () => {
      try {
        return await api.createInvoicePreview({
          customer_id: customerId,
          subscription_id: sub.id,
        })
      } catch {
        return null
      }
    },
  })

  // Last finalized / paid invoice for this sub. Renders inline as the
  // rollover-transparency line (Stripe Dashboard pattern). Fetched
  // separately so a missing-invoice tenant (sub never billed yet)
  // doesn't block the rest of the card.
  const { data: lastInvoice } = useQuery({
    queryKey: ['last-invoice', customerId, sub.id],
    queryFn: async () => {
      try {
        const r = await api.listInvoices(
          `customer_id=${encodeURIComponent(customerId)}&subscription_id=${encodeURIComponent(sub.id)}&limit=1`,
        )
        return r.data?.[0] ?? null
      } catch {
        return null
      }
    },
  })

  return (
    <Card>
      <CardContent className="p-5 space-y-4">
        {/* Header — plan name + sub id (small) + cycle progress */}
        <div className="space-y-2">
          <div className="flex items-baseline justify-between gap-3">
            <p className="text-sm font-medium text-foreground">
              {planNameFromPreview(preview) || 'Subscription'}
            </p>
            <span className="text-[10px] text-muted-foreground font-mono truncate max-w-[180px]" title={sub.id}>
              {sub.id}
            </span>
          </div>
          {sub.current_billing_period_start && sub.current_billing_period_end && (
            <CycleProgress
              start={sub.current_billing_period_start}
              end={sub.current_billing_period_end}
              label={sub.current_billing_period_display}
              now={effectiveNow}
            />
          )}
        </div>

        {/* Projection */}
        {preview && preview.totals.length > 0 ? (
          <ProjectionBreakdown preview={preview} />
        ) : (
          <p className="text-xs text-muted-foreground italic">Projection unavailable for this cycle.</p>
        )}

        {/* Last invoice — rollover transparency */}
        {lastInvoice && <LastInvoiceLine invoice={lastInvoice} effectiveNow={effectiveNow} />}
      </CardContent>
    </Card>
  )
}

function planNameFromPreview(preview: InvoicePreview | null | undefined): string | undefined {
  return preview?.plan_name
}

function CycleProgress({ start, end, label, now }: { start: string; end: string; label?: string; now: EffectiveNow }) {
  const { daysIn, totalDays, pct } = cycleProgress(start, end, now)

  return (
    <div className="space-y-1">
      <div className="flex items-center justify-between text-xs text-muted-foreground tabular-nums">
        <span>
          {label || formatDate(start)}
        </span>
        <span>
          Day {daysIn} of {totalDays} · {pct.toFixed(0)}%
        </span>
      </div>
      <div className="h-1.5 rounded-full bg-muted overflow-hidden">
        <div className="h-full bg-primary rounded-full" style={{ width: `${pct}%` }} />
      </div>
    </div>
  )
}

function ProjectionBreakdown({ preview }: { preview: InvoicePreview }) {
  // Group line items: base/plan vs usage. The wire shape doesn't have
  // a strict line_type taxonomy, so we lean on heuristics: lines with
  // a meter_id are usage, lines without are base/recurring. Falls
  // back to "Other" for anything that doesn't fit.
  const baseLines = preview.lines.filter(l => !l.meter_id)
  const usageLines = preview.lines.filter(l => l.meter_id)
  const total = preview.totals[0]?.amount_cents ?? 0
  const currency = preview.totals[0]?.currency ?? 'USD'

  return (
    <div className="space-y-2 pt-1">
      <div className="flex items-baseline justify-between">
        <p className="text-xs uppercase tracking-wide text-muted-foreground">Projected total</p>
        <p className="text-2xl font-semibold tabular-nums">{formatCents(total, currency)}</p>
      </div>
      <div className="text-xs text-muted-foreground space-y-0.5">
        {baseLines.map((l, i) => (
          <div key={`base-${i}`} className="flex justify-between">
            <span>├ {l.description || 'Base'}</span>
            <span className="tabular-nums">{formatCents(l.amount_cents, l.currency)}</span>
          </div>
        ))}
        {usageLines.length > 0 && (
          <div className="flex justify-between">
            <span>└ Usage ({usageLines.length} meter{usageLines.length === 1 ? '' : 's'})</span>
            <span className="tabular-nums">
              {formatCents(
                usageLines.reduce((s, l) => s + l.amount_cents, 0),
                currency,
              )}
            </span>
          </div>
        )}
      </div>
    </div>
  )
}

function LastInvoiceLine({ invoice, effectiveNow }: { invoice: Invoice; effectiveNow: EffectiveNow }) {
  const tone =
    invoice.payment_status === 'succeeded'
      ? 'success'
      : invoice.payment_status === 'failed'
      ? 'danger'
      : 'secondary'
  // Invoices produced during a clock-advance carry a simulated created_at
  // (billing/engine.go stamps effectiveNow). Measure "X ago" against the sub's
  // frozen_time, or a simulated-future invoice collapses to "just now".
  const finalizedAgo = invoice.created_at ? timeAgo(invoice.created_at, effectiveNow) : ''

  return (
    <Link
      to={`/invoices/${invoice.id}`}
      className="flex items-center justify-between gap-3 pt-3 border-t border-border text-xs hover:bg-muted/30 -mx-5 px-5 -mb-5 pb-3 transition-colors"
    >
      <div className="flex items-center gap-2 min-w-0">
        <span className="text-muted-foreground">Last invoice:</span>
        <Badge variant={tone === 'success' ? 'success' : tone === 'danger' ? 'danger' : 'secondary'}>
          {invoice.payment_status}
        </Badge>
        <span className="font-mono text-foreground truncate">{invoice.invoice_number}</span>
      </div>
      <div className="flex items-center gap-2 shrink-0">
        <span className="font-medium tabular-nums text-foreground">
          {formatCents(invoice.amount_due_cents, invoice.currency)}
        </span>
        {finalizedAgo && <span className="text-muted-foreground">· {finalizedAgo}</span>}
      </div>
    </Link>
  )
}

// timeAgo — CostDashboard's richer "X ago" ladder (adds weeks/months tails, for
// invoices that can be months old in a long simulation). Requires an anchor;
// build it with the resolver so a simulated-future invoice doesn't collapse to
// "just now".
function timeAgo(iso: string, now: EffectiveNow): string {
  const m = Math.floor(sinceMs(iso, now) / 60_000)
  if (m < 1) return 'just now'
  if (m < 60) return `${m}m ago`
  const h = Math.floor(m / 60)
  if (h < 24) return `${h}h ago`
  const d = Math.floor(h / 24)
  if (d < 7) return `${d}d ago`
  const w = Math.floor(d / 7)
  if (w < 5) return `${w}w ago`
  const mo = Math.floor(d / 30)
  return `${mo}mo ago`
}

// ============================================================================
// Activity — customer-level time-window view with chart + meter drill-down
// ============================================================================

type ActivityPreset = 'cycle' | 'last_7d' | 'last_30d' | 'last_90d'

const ACTIVITY_PRESETS: { value: ActivityPreset; label: string }[] = [
  { value: 'cycle', label: 'Cycle' },
  { value: 'last_7d', label: '7d' },
  { value: 'last_30d', label: '30d' },
  { value: 'last_90d', label: '90d' },
]

// activityPresetToParams anchors the rolling window's upper bound to `now`. For
// a clock-pinned customer that's the clock's frozen_time: usage events are
// stamped in simulation time (usage/service.go), so a wall-clock "last 30d"
// window sits entirely before the simulated events and the chart reads $0. The
// 'cycle' preset returns undefined on purpose — the backend defaults it to the
// frozen_time cycle.
function activityPresetToParams(p: ActivityPreset, now: EffectiveNow): { from?: string; to?: string } | undefined {
  if (p === 'cycle') return undefined
  const days = p === 'last_7d' ? 7 : p === 'last_30d' ? 30 : 90
  return rollingWindow(days, now)
}

function ActivitySection({ customerId, customerTestClockId }: { customerId: string; customerTestClockId?: string }) {
  const [preset, setPreset] = useState<ActivityPreset>('last_30d')

  // Anchor the rolling window to the customer's simulated "now" when pinned to a
  // test clock, so the 7/30/90d presets frame the simulated usage rather than a
  // wall-clock window that predates it. Keyed into the query so a clock advance
  // refetches; a wall-clock customer's anchor is stable across renders (captured
  // at mount) so it doesn't churn the query.
  const now = useEffectiveNow(customerTestClockId)

  const { data, isLoading, error, refetch } = useQuery({
    queryKey: ['customer-usage', customerId, preset, now],
    queryFn: async () => {
      try {
        return await api.customerUsage(customerId, activityPresetToParams(preset, now))
      } catch (err) {
        // 422 customer_has_no_subscription is the documented "no current
        // cycle" state — surface as an empty result so the section
        // renders the empty-state instead of a red error. 404 same.
        // Real network/auth errors fall through.
        if (err && typeof err === 'object' && 'status' in err && 'code' in err) {
          const s = (err as { status: number }).status
          const c = (err as { code?: string }).code
          if (s === 404) return null
          if (s === 422 && c === 'customer_has_no_subscription') return null
        }
        throw err
      }
    },
  })

  const totalCents = data?.totals.reduce((sum, t) => sum + t.amount_cents, 0) ?? 0
  const totalEvents = data?.meters.reduce((sum, m) => sum + Number(m.total_quantity), 0) ?? 0

  return (
    <section>
      <div className="flex items-center justify-between gap-4 mb-3">
        <div>
          <h2 className="text-base font-semibold text-foreground">Activity</h2>
          {data?.period && (
            <p className="text-xs text-muted-foreground mt-0.5 tabular-nums">
              {formatDate(data.period.from)} → {formatDate(data.period.to)}
            </p>
          )}
        </div>
        <Tabs value={preset} onValueChange={v => setPreset(v as ActivityPreset)}>
          <TabsList>
            {ACTIVITY_PRESETS.map(p => (
              <TabsTrigger key={p.value} value={p.value} className="text-xs">
                {p.label}
              </TabsTrigger>
            ))}
          </TabsList>
        </Tabs>
      </div>

      {error ? (
        <Card>
          <CardContent className="p-6 text-center">
            <p className="text-sm text-destructive mb-3">{(error as Error).message}</p>
            <Button variant="outline" size="sm" onClick={() => refetch()}>
              Retry
            </Button>
          </CardContent>
        </Card>
      ) : isLoading ? (
        <Card>
          <CardContent className="p-8 flex justify-center">
            <Loader2 size={20} className="animate-spin text-muted-foreground" />
          </CardContent>
        </Card>
      ) : !data ? (
        <Card>
          <CardContent className="p-8 text-center">
            <Activity size={20} className="mx-auto text-muted-foreground mb-3" />
            <p className="text-sm text-muted-foreground">
              No usage to show for this period.
              {preset === 'cycle'
                ? " Customer doesn't have an active subscription, or the cycle hasn't started yet."
                : ' Try a different window.'}
            </p>
          </CardContent>
        </Card>
      ) : (
        <div className="space-y-4">
          {/* Window total — primary KPI for this surface (cycle
              projection lives on the Upcoming card above). */}
          <Card>
            <CardContent className="px-5 py-4">
              <div className="flex items-baseline justify-between gap-4">
                <div>
                  <p className="text-xs uppercase tracking-wide text-muted-foreground">Total</p>
                  <p className="text-2xl font-semibold tabular-nums mt-1">
                    {formatCents(totalCents, data.totals[0]?.currency)}
                  </p>
                </div>
                <p className="text-xs text-muted-foreground tabular-nums">
                  {totalEvents.toLocaleString()} event{totalEvents === 1 ? '' : 's'}
                </p>
              </div>
            </CardContent>
          </Card>

          {/* Daily bar chart — primary visual primitive 5/7 reference
              platforms (Datadog, OpenAI, Anthropic, AWS, Orb) build
              their per-customer view around. Hidden if no events. */}
          {data.buckets.length > 0 && data.meters.length > 0 && <UsageChart usage={data} />}

          {/* Warnings */}
          {data.warnings.length > 0 && (
            <Card className="border-amber-200 dark:border-amber-900">
              <CardContent className="p-4">
                <div className="flex items-center gap-2 text-amber-700 dark:text-amber-300 text-xs font-medium mb-2">
                  <AlertTriangle size={12} />
                  {data.warnings.length} warning{data.warnings.length !== 1 ? 's' : ''}
                </div>
                <ul className="text-xs text-amber-700 dark:text-amber-300 space-y-1">
                  {data.warnings.map((w, i) => (
                    <li key={i}>{w}</li>
                  ))}
                </ul>
              </CardContent>
            </Card>
          )}

          {/* Per-meter cards with rule-resolution drill-down */}
          {data.meters.length === 0 ? (
            <Card>
              <CardContent className="p-8 text-center">
                <p className="text-sm text-muted-foreground">No metered usage in this period.</p>
              </CardContent>
            </Card>
          ) : (
            <div className="space-y-2">
              {data.meters.map(m => (
                <MeterRow key={m.meter_id} meter={m} />
              ))}
            </div>
          )}
        </div>
      )}
    </section>
  )
}

// ============================================================================
// UsageChart — daily-grain stacked bar chart by meter
// ============================================================================

function UsageChart({ usage }: { usage: CustomerUsage }) {
  const meters = usage.meters
  const data = useMemo(
    () =>
      usage.buckets.map(b => {
        // Render in tenant TZ so chart x-axis labels match what
        // operators see elsewhere on the dashboard. ADR-010.
        const tz = getTenantTimezone() || undefined
        const row: Record<string, number | string> = {
          date: b.bucket_start,
          label: tz
            ? formatInTimeZone(new Date(b.bucket_start), tz, 'MMM d')
            : new Date(b.bucket_start).toLocaleDateString('en-US', { month: 'short', day: 'numeric' }),
        }
        for (const m of meters) {
          row[m.meter_id] = Number(b.per_meter[m.meter_id] ?? 0)
        }
        return row
      }),
    [usage.buckets, meters],
  )

  // Stable index-based palette; meter colours don't shift across
  // renders so the chart reads consistently turn-over-turn.
  const palette = ['#6366f1', '#10b981', '#f59e0b', '#ef4444', '#8b5cf6', '#06b6d4', '#ec4899']

  return (
    <Card>
      <CardContent className="p-4">
        <div className="flex items-center justify-between mb-3">
          <p className="text-xs uppercase tracking-wide text-muted-foreground">Daily usage</p>
          <p className="text-xs text-muted-foreground tabular-nums">
            {data.length} day{data.length === 1 ? '' : 's'}
          </p>
        </div>
        <ResponsiveContainer width="100%" height={180}>
          <BarChart data={data} margin={{ top: 4, right: 4, left: 0, bottom: 0 }}>
            <CartesianGrid strokeDasharray="3 3" vertical={false} stroke="rgba(148,163,184,0.2)" />
            <XAxis
              dataKey="label"
              tick={{ fontSize: 10, fill: 'currentColor' }}
              tickLine={false}
              axisLine={false}
              interval="preserveStartEnd"
              minTickGap={20}
            />
            <YAxis
              tick={{ fontSize: 10, fill: 'currentColor' }}
              tickLine={false}
              axisLine={false}
              width={40}
            />
            <Tooltip
              contentStyle={{
                backgroundColor: 'var(--background)',
                border: '1px solid var(--border)',
                borderRadius: 6,
                fontSize: 12,
              }}
              labelFormatter={(label, payload) => {
                if (!payload || !payload[0]) return label
                const date = (payload[0].payload as { date: string }).date
                const tz = getTenantTimezone() || undefined
                return tz
                  ? formatInTimeZone(new Date(date), tz, 'EEE, MMM d')
                  : new Date(date).toLocaleDateString('en-US', { weekday: 'short', month: 'short', day: 'numeric' })
              }}
              formatter={(value, _name, item) => {
                const meterId = item.dataKey as string
                const meter = meters.find(m => m.meter_id === meterId)
                const label = meter ? meter.meter_name : meterId
                return [`${Number(value).toLocaleString()} ${meter?.unit ?? ''}`, label]
              }}
            />
            {meters.map((m, i) => (
              <Bar
                key={m.meter_id}
                dataKey={m.meter_id}
                stackId="a"
                fill={palette[i % palette.length]}
                radius={i === meters.length - 1 ? [3, 3, 0, 0] : 0}
              />
            ))}
          </BarChart>
        </ResponsiveContainer>
      </CardContent>
    </Card>
  )
}

// ============================================================================
// Per-meter card with rule-resolution drill-down
// ============================================================================

// Top-N cap for the rule breakdown. AWS Cost Explorer's "9 named bars
// + 1 Other" is the cleanest documented top-N treatment in the field;
// adopted exactly. Operators who want the long tail go to CSV.
const RULE_TOP_N = 9

function MeterRow({ meter }: { meter: CustomerUsageMeter }) {
  const [expanded, setExpanded] = useState(false)
  const hasUnmatched = meter.rules.some(r => r.unmatched)
  const hasMultipleRules = meter.rules.length > 1 || hasUnmatched
  const totalQty = useMemo(() => Number(meter.total_quantity), [meter.total_quantity])

  const visibleRules = useMemo<CustomerUsageRule[]>(() => {
    // Unmatched rows never fold into the "(N more)" bucket and always
    // render last — a not-billed warning must not hide in the long tail.
    const matched = meter.rules.filter(r => !r.unmatched)
    const unmatched = meter.rules.filter(r => r.unmatched)
    const sorted = [...matched].sort((a, b) => b.amount_cents - a.amount_cents)
    if (sorted.length <= RULE_TOP_N + 1) return [...sorted, ...unmatched]
    const top = sorted.slice(0, RULE_TOP_N)
    const tail = sorted.slice(RULE_TOP_N)
    const otherCents = tail.reduce((sum, r) => sum + r.amount_cents, 0)
    const otherQty = tail.reduce((sum, r) => sum + Number(r.quantity), 0)
    return [
      ...top,
      {
        rating_rule_version_id: '__other__',
        rule_key: `(${tail.length} more rules)`,
        dimension_match: undefined,
        quantity: String(otherQty),
        amount_cents: otherCents,
      },
      ...unmatched,
    ]
  }, [meter.rules])

  return (
    <Card>
      <CardContent className="p-0">
        <button
          type="button"
          className={cn(
            'w-full px-5 py-4 flex items-center gap-4 text-left transition-colors',
            hasMultipleRules ? 'cursor-pointer hover:bg-muted/30' : 'cursor-default',
          )}
          onClick={() => hasMultipleRules && setExpanded(e => !e)}
          disabled={!hasMultipleRules}
        >
          {hasMultipleRules ? (
            expanded ? (
              <ChevronDown size={14} className="text-muted-foreground shrink-0" />
            ) : (
              <ChevronRight size={14} className="text-muted-foreground shrink-0" />
            )
          ) : (
            <span className="w-3.5" />
          )}
          <div className="flex-1 min-w-0">
            <p className="text-sm font-medium text-foreground truncate">
              {meter.meter_name}
              {hasUnmatched && (
                <span className="ml-2 inline-flex items-center text-[10px] font-medium text-amber-500 border border-amber-500/40 bg-amber-500/10 px-1.5 py-0.5 rounded">
                  unmatched usage — not billed
                </span>
              )}
            </p>
            <p className="text-xs text-muted-foreground font-mono">
              {totalQty.toLocaleString(undefined, { maximumFractionDigits: 6 })} {meter.unit}
              {hasMultipleRules && <span className="ml-2">· {meter.rules.filter(r => !r.unmatched).length} rules</span>}
            </p>
          </div>
          <p className="text-sm font-semibold text-foreground tabular-nums shrink-0">
            {formatCents(meter.total_amount_cents, meter.currency)}
          </p>
        </button>

        {expanded && hasMultipleRules && (
          <div className="border-t border-border bg-muted/20">
            <p className="px-5 pt-3 pb-1 text-[10px] uppercase tracking-wide text-muted-foreground font-medium">
              By pricing rule
            </p>
            {visibleRules.map(r => (
              <RuleRow
                key={r.rating_rule_version_id}
                rule={r}
                totalCents={meter.total_amount_cents}
                unit={meter.unit}
                currency={meter.currency}
              />
            ))}
          </div>
        )}
      </CardContent>
    </Card>
  )
}

function RuleRow({
  rule,
  totalCents,
  unit,
  currency,
}: {
  rule: CustomerUsageRule
  totalCents: number
  unit: string
  currency: string
}) {
  const qty = Number(rule.quantity)
  const pct = totalCents > 0 ? (rule.amount_cents / totalCents) * 100 : 0
  // Per-unit price to show, in decimal cents: the backend's unit_amount_decimal
  // — the SAME nominal rate the invoice shows for flat rules (effective
  // amount÷qty for tiered), authoritative because it resolves the customer's
  // negotiated rate. NO client-side fallback: if the backend didn't send it we
  // show no rate rather than re-deriving amount÷qty — an absent rate is then a
  // visible signal something's wrong, not silently papered over with a proxy
  // (no-heuristic-proxies / no-silent-fallbacks). Rendered via formatRate so a
  // real sub-cent rate never collapses to $0.0000.
  const rateCents = rule.unit_amount_decimal ?? null
  const isOther = rule.rating_rule_version_id === '__other__'
  const hasDimensions =
    !isOther && rule.dimension_match && Object.keys(rule.dimension_match).length > 0

  if (rule.unmatched) {
    // Events that matched no pricing rule: billed nothing. This row is a
    // warning, not a spend line — no rate, no share bar, amber treatment.
    return (
      <div className="px-5 py-3 border-b border-border last:border-b-0 space-y-1">
        <div className="flex items-baseline justify-between gap-4">
          <span className="text-xs font-medium text-amber-500">
            No pricing rule matched — check event dimension values
          </span>
          <span className="text-xs font-semibold text-amber-500 shrink-0">not billed</span>
        </div>
        <div className="text-[10px] text-muted-foreground font-mono">
          {qty.toLocaleString(undefined, { maximumFractionDigits: 4 })} {unit}
        </div>
      </div>
    )
  }

  return (
    <div className="px-5 py-3 border-b border-border last:border-b-0 space-y-1.5">
      <div className="flex items-baseline justify-between gap-4">
        <div className="flex items-center gap-1.5 flex-wrap min-w-0">
          {hasDimensions ? (
            Object.entries(rule.dimension_match!).map(([k, v]) => (
              <span
                key={k}
                className="inline-flex items-center font-mono text-[10px] bg-background border border-border px-1.5 py-0.5 rounded"
              >
                <span className="text-muted-foreground">{k}</span>
                <span className="mx-1 text-muted-foreground/60">=</span>
                <span className="text-foreground">{String(v)}</span>
              </span>
            ))
          ) : isOther ? (
            <span className="text-xs text-muted-foreground italic">{rule.rule_key}</span>
          ) : (
            <Badge variant="secondary" className="font-mono text-[10px] py-0 px-1.5">
              {rule.rule_key || 'catch-all'}
            </Badge>
          )}
        </div>
        <span className="font-semibold text-sm tabular-nums shrink-0">
          {formatCents(rule.amount_cents, currency)}
        </span>
      </div>

      {/* Mixpanel-flavoured per-row proportion bar — "this rule's share
          of the meter's total spend" at a glance. Capped at 100%. */}
      <div className="h-1 bg-muted rounded-full overflow-hidden">
        <div
          className="h-full bg-primary/80 rounded-full"
          style={{ width: `${Math.min(100, pct)}%` }}
        />
      </div>

      <div className="flex items-center justify-between text-[10px] text-muted-foreground font-mono">
        <span>
          {qty.toLocaleString(undefined, { maximumFractionDigits: 4 })} {unit}
          {rateCents != null && Number(rateCents) > 0 && !isOther && (
            // Token rates quote per-1M — the same convention as the Pricing
            // pages and (since #766) every invoice surface; a per-token
            // "@ $0.000003/tokens" here contradicted the invoice one click away.
            <span className="ml-2">
              @ {unitScale(unit).scale > 0
                ? `${money(String(rateCents), unitScale(unit).scale, currency)} / ${unitScale(unit).per}`
                : `${formatRate(rateCents, currency)}/${unit}`}
            </span>
          )}
        </span>
        <span>{pct.toFixed(pct < 1 ? 1 : 0)}%</span>
      </div>
    </div>
  )
}

