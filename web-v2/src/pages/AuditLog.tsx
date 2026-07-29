import { useEffect, useState } from 'react'
import { usePageTitle } from '@/hooks/usePageTitle'
import { useQuery } from '@tanstack/react-query'
import { Link } from 'react-router-dom'
import { formatInTimeZone } from 'date-fns-tz'
import { toast } from 'sonner'
import { api, formatDateTime, formatRate, getTenantTimezone, formatCents } from '@/lib/api'
import { startOfDayInTZ, endOfDayInTZ } from '@/lib/dates'
import type { AuditEntry } from '@/lib/api'
import { downloadServerCSV } from '@/lib/csv'
import {
  describeAction,
  resourceLink,
  HIGH_SEVERITY,
  MEDIUM_SEVERITY,
  DEFAULT_ACTIONS,
  DEFAULT_RESOURCE_TYPES,
} from '@/lib/auditVocabulary'
import { Layout } from '@/components/Layout'
import { EmptyState } from '@/components/EmptyState'
import { useUrlState } from '@/hooks/useUrlState'
import { cn } from '@/lib/utils'

import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { DatePicker } from '@/components/ui/date-picker'
import { Badge } from '@/components/ui/badge'
import { Card, CardContent } from '@/components/ui/card'
import {
  Pagination,
  PaginationContent,
  PaginationItem,
  PaginationNext,
  PaginationPrevious,
} from '@/components/ui/pagination'
import { CopyButton } from '@/components/CopyButton'
import { FeedSkeleton } from '@/components/ui/TableSkeleton'

import { Download, ChevronRight, History } from 'lucide-react'

const PAGE_SIZE = 50

// encodeCursor builds the seek-pagination token for ?after= — the same wire
// format the backend emits (base64url of {id, created_at}) — so the client can
// transition from the page-1 offset response onto the cursor path.
//
// There is ONE sort axis (created_at) and therefore one anchor. This used to
// carry a sim_effective_at anchor for an "order by simulated time" seek; that
// control does not exist (ADR-090 §5 — within a clock it is the same order, and
// across clocks it interleaves unrelated simulations), and the backend cursor no
// longer has the field at all.
function encodeCursor(e: AuditEntry): string {
  const cur: Record<string, string> = { id: e.id, created_at: e.created_at }
  return btoa(JSON.stringify(cur))
    .replace(/\+/g, '-')
    .replace(/\//g, '_')
}

function formatActorName(entry: AuditEntry): string {
  if (entry.actor_type === 'system') return 'System'
  if (entry.actor_type === 'api_key') {
    // Prefer the key's human-readable name (resolved server-side via api_keys
    // join). Falls back to a truncated key id for rows written before the
    // name was set, or for keys that have since been deleted.
    if (entry.actor_name) return entry.actor_name
    return entry.actor_id.startsWith('vlx_') ? `Key ${entry.actor_id.slice(0, 16)}...` : 'API Key'
  }
  if (entry.actor_type === 'user') {
    // Dashboard session operators (#225 actor identity). actor_name is the
    // join-resolved email (users has no display_name column); fall back to a
    // generic label so pre-join rows never render the raw enum "user".
    return entry.actor_name || 'Operator'
  }
  if (entry.actor_type === 'customer') {
    // Customer-portal-driven mutations (subscription cancel, profile edit,
    // payment-method changes) ship actor_id = customer.id. Prefer the
    // join-resolved actor_name; otherwise show "Customer" so operators
    // can see at-a-glance that the action came from the customer side.
    if (entry.actor_name) return entry.actor_name
    return 'Customer'
  }
  return entry.actor_type
}

// fixAcronyms repairs title-casing of common initialisms ("Ip Address" →
// "IP Address") after the generic word-capitalization pass.
function fixAcronyms(value: string): string {
  return value
    .replace(/\bIp\b/g, 'IP')
    .replace(/\bId\b/g, 'ID')
    .replace(/\bUrl\b/g, 'URL')
    .replace(/\bApi\b/g, 'API')
    .replace(/\bSmtp\b/g, 'SMTP')
}

function prettyLabel(value: string): string {
  return fixAcronyms(value
    .replace(/[_.]/g, ' ')
    .replace(/-/g, ' ')
    .replace(/\b\w/g, c => c.toUpperCase()))
}

// Test-clock sim-context keys. Since ADR-090 §5 these are real COLUMNS
// (sim_effective_at / test_clock_id, migration 0148) stamped by the audit
// writer from the ctx's clock binding. The writer also mirrors them into metadata
// (which is where rows written before 0148 carry them), but this page reads the
// COLUMNS ONLY and deliberately does not fall back — see simContext() below for
// why: the clock FILTER reads the columns, so lighting the chip off metadata
// would show a chip on a row the filter then cannot find.
//
// Excluded from generic metadata rendering — surfaced as a dedicated subline +
// chip so the operator reads "wall-clock click + simulated effect-time"
// coherently instead of seeing the keys mixed in with business metadata.
const SIM_CONTEXT_KEYS = new Set(['sim_effective_at', 'test_clock_id'])

// COLUMNS ONLY — deliberately no metadata fallback.
//
// The audit writer mirrors the pair into metadata as well, and reading that
// mirror would light the "test clock" chip on rows written before migration
// 0148. But those rows have NULL sim COLUMNS, and the clock filter reads the
// columns — so the operator would see a chip, pick that clock, and watch the row
// vanish. append-only forbids backfilling the columns, so the chip could never
// be made true. What is on screen must be exactly what the filter can find; a
// chip that lies about where a row lives is worse than no chip.
//
// The cost is confined to dev databases (Velox is pre-launch; no production log
// predates 0148). The subscription Activity timeline still reads the metadata
// mirror — it offers no clock filter, so it has no promise to break.
// The backend bounds its COUNT at COUNT_CAP: audit_log can never be pruned, so an
// exact total is an O(history) scan that grows forever, paid on every page-1 load
// to print a number nobody acts on. At or above the cap it reports the cap, and we
// say "10,000+" rather than stating a precise figure we know to be wrong.
const COUNT_CAP = 10000
const formatTotal = (n: number) =>
  n >= COUNT_CAP ? `${COUNT_CAP.toLocaleString()}+` : n.toLocaleString()

function simContext(entry: AuditEntry): { simEffectiveAt?: string; testClockID?: string } {
  return { simEffectiveAt: entry.sim_effective_at, testClockID: entry.test_clock_id }
}

function formatMetadata(meta: Record<string, unknown> | undefined): { label: string; value: string }[] {
  if (!meta) return []
  const items: { label: string; value: string }[] = []
  for (const [key, val] of Object.entries(meta)) {
    if (key === 'resource_label') continue
    if (SIM_CONTEXT_KEYS.has(key)) continue
    const label = fixAcronyms(key.replace(/_/g, ' ').replace(/\b\w/g, c => c.toUpperCase()))
    if (typeof val === 'number' && key.includes('cents')) {
      items.push({ label: label.replace(' Cents', ''), value: formatCents(val) })
    } else if (typeof val === 'string' && key.includes('cents') && val !== '' && Number.isFinite(Number(val))) {
      // Decimal per-unit rates serialize as strings (e.g. "0.0003" cents).
      // toFixed(2) would collapse sub-cent rates to $0.00 — render at full
      // precision instead.
      items.push({ label: label.replace(' Cents', ''), value: formatRate(val) })
    } else if (val !== null && typeof val === 'object') {
      // Structured values (e.g. the settings-change {field: {from, to}} map)
      // — String(val) renders "[object Object]"; show the JSON instead.
      items.push({ label, value: JSON.stringify(val) })
    } else if (val !== null && val !== undefined && val !== '') {
      items.push({ label, value: String(val) })
    }
  }
  return items
}

function groupByDate(entries: AuditEntry[]): { date: string; entries: AuditEntry[] }[] {
  // Group by tenant-TZ date so the section headers match the
  // dashboard's tenant-TZ display elsewhere. Around UTC-day
  // boundaries the prior browser-local grouping disagreed with the
  // entries' rendered timestamps. ADR-010.
  const tz = getTenantTimezone() || undefined
  const fmtDate = (iso: string) =>
    tz
      ? formatInTimeZone(new Date(iso), tz, 'MMMM d, yyyy')
      : new Date(iso).toLocaleDateString('en-US', { year: 'numeric', month: 'long', day: 'numeric' })

  const groups: { date: string; entries: AuditEntry[] }[] = []
  let currentDate = ''
  for (const entry of entries) {
    const date = fmtDate(entry.created_at)
    if (date !== currentDate) {
      groups.push({ date, entries: [] })
      currentDate = date
    }
    groups[groups.length - 1].entries.push(entry)
  }
  return groups
}

function actionVariant(action: string): 'default' | 'secondary' | 'destructive' | 'outline' | 'success' | 'info' | 'warning' | 'danger' {
  if (HIGH_SEVERITY.has(action)) return 'danger'
  if (MEDIUM_SEVERITY.has(action)) return 'warning'
  // Green for positive actions
  if (['create', 'activate', 'resume', 'grant', 'resolve'].includes(action)) return 'success'
  // Blue for updates/changes
  if (['update', 'finalize', 'run', 'credit_note.issued'].includes(action)) return 'info'
  return 'secondary'
}

export default function AuditLogPage() {
  usePageTitle('Audit log')
  const [expandedId, setExpandedId] = useState<string | null>(null)
  const [exporting, setExporting] = useState(false)
  const [urlState, setUrlState] = useUrlState({
    resource_type: '',
    action: '',
    actor: '',
    resource_id: '',
    date_from: '',
    date_to: '',
    // Sim axis (ADR-090 §5): scope to one simulation. It lives in the URL so a
    // forensic view of a torn-down clock is a shareable link. There is
    // deliberately no "order by simulated time" control — within one clock it is
    // the same order as wall-clock, and across clocks it interleaves unrelated
    // simulations into a timeline that never happened.
    test_clock: '',
  })
  const {
    resource_type: resourceType,
    action,
    actor: actorFilter,
    resource_id: resourceIdFilter,
    date_from: dateFrom,
    date_to: dateTo,
    test_clock: testClockFilter,
  } = urlState

  // Cursor (seek) pagination — Stripe-style Previous/Next. The stack holds
  // one cursor per page beyond the first; depth = current page - 1. Page 1
  // uses the offset path (which also returns the total once); deeper pages
  // ride ?after=, which skips the expensive COUNT entirely (the RLS OR-branch
  // makes that COUNT a full seq scan, so numbered offset pages paid it on
  // every click). Stack resets whenever a filter changes.
  const [cursors, setCursors] = useState<string[]>([])
  const [lastTotal, setLastTotal] = useState(0)
  const after = cursors.length > 0 ? cursors[cursors.length - 1] : ''
  const page = cursors.length + 1
  const filterKey = [resourceType, action, actorFilter, resourceIdFilter, dateFrom, dateTo, testClockFilter].join('|')
  useEffect(() => { setCursors([]) }, [filterKey])

  // Audit log entries query — keyed by every filter dimension so the
  // cache is sharded per-filter-set. Changing any filter triggers a
  // refetch automatically (RQ recognizes a new key). Tenant-TZ
  // grounded date instants (ADR-010) so the same calendar day
  // resolves identically across operator time zones.
  const qs = (() => {
    const params = new URLSearchParams()
    params.set('limit', String(PAGE_SIZE))
    if (after) params.set('after', after)
    else params.set('offset', '0')
    if (resourceType) params.set('resource_type', resourceType)
    if (action) params.set('action', action)
    if (resourceIdFilter) params.set('resource_id', resourceIdFilter)
    if (actorFilter) params.set('actor_id', actorFilter)
    if (dateFrom) params.set('date_from', startOfDayInTZ(dateFrom))
    if (dateTo) params.set('date_to', endOfDayInTZ(dateTo))
    if (testClockFilter) params.set('test_clock_id', testClockFilter)
    return params.toString()
  })()
  const entriesQuery = useQuery({
    queryKey: ['audit-log', qs],
    queryFn: () => api.listAuditLog(qs),
  })
  const entries = entriesQuery.data?.data ?? []
  // total only arrives on the page-1 (offset) response; keep the last-seen
  // value so the header count survives while paging through cursor pages.
  useEffect(() => {
    if (typeof entriesQuery.data?.total === 'number') setLastTotal(entriesQuery.data.total)
  }, [entriesQuery.data])
  const total = lastTotal
  const loading = entriesQuery.isLoading
  const error = entriesQuery.error instanceof Error ? entriesQuery.error.message : (entriesQuery.error ? 'Failed to load audit log' : null)
  const loadEntries = () => { void entriesQuery.refetch() }

  // Populate filter dropdowns from what's actually recorded for this tenant.
  // Fall back to defaults on error or empty — keeps the UI usable for new
  // tenants whose audit log hasn't been populated yet.
  const filterOptionsQuery = useQuery({
    queryKey: ['audit-log', 'filters'],
    queryFn: () => api.getAuditFilters(),
  })
  const filterOptions = {
    actions: filterOptionsQuery.data?.actions?.length ? filterOptionsQuery.data.actions : DEFAULT_ACTIONS,
    resourceTypes: filterOptionsQuery.data?.resource_types?.length ? filterOptionsQuery.data.resource_types : DEFAULT_RESOURCE_TYPES,
    // No fallback list: a clock the log has never seen cannot be filtered on,
    // and inventing entries would offer the operator a filter that returns
    // nothing. Empty => the control is hidden entirely.
    testClocks: filterOptionsQuery.data?.test_clocks ?? [],
  }

  // Page 1 (offset response) derives has-more from the total; cursor pages
  // carry has_more/next_cursor. For page 1 the next cursor is built client-
  // side from the last row — same wire format the backend emits
  // (base64url of {id, created_at}).
  const hasNext = after
    ? Boolean(entriesQuery.data?.has_more)
    : total > PAGE_SIZE
  const nextCursor = after
    ? (entriesQuery.data?.next_cursor ?? '')
    : (entries.length > 0 ? encodeCursor(entries[entries.length - 1]) : '')
  const groups = groupByDate(entries)

  // Export streams from the SERVER (GET /v1/exports/audit-log.csv), applying the
  // filters currently on screen.
  //
  // It used to page this API in the browser and stop at 50,000 rows — a SILENT
  // truncation of the compliance evidence itself: the operator got a file that
  // looked complete and nothing in it said otherwise. The server-side export has
  // no cap (audit.Logger.Stream), streams rather than materializing, and — being
  // bulk egress of the audit log — writes its own `export` audit row before the
  // first byte leaves (ADR-090 §7). Exporting the evidence is itself evidence.
  const handleExport = async () => {
    setExporting(true)
    try {
      const params = new URLSearchParams()
      if (resourceType) params.set('resource_type', resourceType)
      if (action) params.set('action', action)
      if (resourceIdFilter) params.set('resource_id', resourceIdFilter)
      if (actorFilter) params.set('actor_id', actorFilter)
      // The export must be the SAME slice that is on screen — a CSV that
      // silently ignored the clock filter would hand an auditor a file that
      // looks like one simulation and is actually the whole log.
      if (testClockFilter) params.set('test_clock_id', testClockFilter)
      // Wrap to tenant-TZ start/end-of-day instants, identical to the list query
      // above — otherwise the export anchors bare YYYY-MM-DD at UTC and a
      // non-UTC tenant's CSV silently drops (or adds) rows near day edges vs.
      // what's on screen.
      if (dateFrom) params.set('date_from', startOfDayInTZ(dateFrom))
      if (dateTo) params.set('date_to', endOfDayInTZ(dateTo))

      const qs = params.toString()
      await downloadServerCSV(`/v1/exports/audit-log.csv${qs ? `?${qs}` : ''}`, 'audit-log.csv')
    } catch (err) {
      // The export fails CLOSED when its own audit row can't be written: no file,
      // and the operator is told, rather than handed a partial one.
      toast.error(err instanceof Error ? err.message : 'Export failed')
    } finally {
      setExporting(false)
    }
  }

  const selectClass = "flex h-9 rounded-md border border-input bg-transparent px-3 py-1 text-sm shadow-xs transition-colors focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring"

  const hasFilters = resourceType || action || actorFilter || resourceIdFilter || dateFrom || dateTo || testClockFilter

  return (
    <Layout>
      {/* Header */}
      <div className="flex justify-between items-center">
        <div>
          <h1 className="text-2xl font-semibold text-foreground">Audit Log</h1>
          <p className="text-sm text-muted-foreground mt-1">Review all changes and actions{total > 0 ? ` · ${formatTotal(total)} total` : ''}</p>
        </div>
        {entries.length > 0 && (
          <Button variant="outline" size="sm" onClick={handleExport} disabled={exporting}>
            <Download size={16} className="mr-2" /> {exporting ? 'Exporting…' : 'Export'}
          </Button>
        )}
      </div>

      {/* Filters */}
      <div className="flex flex-wrap items-center gap-3 mt-6">
        <select
          value={resourceType}
          onChange={e => setUrlState({ resource_type: e.target.value })}
          className={cn(selectClass, 'w-44')}
        >
          <option value="">All resources</option>
          {filterOptions.resourceTypes.map(rt => (
            <option key={rt} value={rt}>{prettyLabel(rt)}</option>
          ))}
        </select>
        <select
          value={action}
          onChange={e => setUrlState({ action: e.target.value })}
          className={cn(selectClass, 'w-44')}
        >
          <option value="">All actions</option>
          {filterOptions.actions.map(a => (
            <option key={a} value={a}>{prettyLabel(a)}</option>
          ))}
        </select>
        <Input
          value={actorFilter}
          onChange={e => setUrlState({ actor: e.target.value })}
          placeholder="Filter by actor..."
          className="w-40"
        />
        <Input
          value={resourceIdFilter}
          onChange={e => setUrlState({ resource_id: e.target.value })}
          placeholder="Filter by resource ID..."
          className="w-44"
        />
        <DatePicker
          value={dateFrom}
          onChange={v => setUrlState({ date_from: v })}
          placeholder="From date"
          className="w-44"
        />
        <DatePicker
          value={dateTo}
          onChange={v => setUrlState({ date_to: v })}
          placeholder="To date"
          className="w-44"
          minDate={dateFrom ? new Date(dateFrom + 'T00:00:00') : undefined}
        />
        {/* Test-clock scope. Only rendered once the log has actually recorded a
            simulation — for the many tenants that never touch test clocks this
            control never appears. The list comes from the audit rows themselves,
            so a clock that has already been torn down (ADR-086 deletes its whole
            graph) is still selectable: that is the case where the audit log is
            the only surviving record of it. */}
        {filterOptions.testClocks.length > 0 && (
          <>
            <select
              value={testClockFilter}
              onChange={e => setUrlState({ test_clock: e.target.value })}
              className={cn(selectClass, 'w-52')}
            >
              <option value="">All test clocks</option>
              {filterOptions.testClocks.map(c => (
                <option key={c.id} value={c.id}>{c.name || c.id}</option>
              ))}
            </select>
          </>
        )}
        {hasFilters && (
          <Button
            variant="outline"
            size="sm"
            onClick={() => setUrlState({
              resource_type: '', action: '', actor: '', resource_id: '',
              date_from: '', date_to: '', test_clock: '',
            })}
          >
            Clear all
          </Button>
        )}
      </div>

      {/* Timeline */}
      <Card className="mt-4">
        <CardContent className="p-0">
          {error ? (
            <div className="p-8 text-center">
              <p className="text-sm text-destructive mb-3">{error}</p>
              <Button variant="outline" size="sm" onClick={loadEntries}>
                Retry
              </Button>
            </div>
          ) : loading ? (
            <FeedSkeleton rows={10} />
          ) : entries.length === 0 ? (
            <EmptyState
              icon={History}
              title="No audit entries"
              description="Actions will be recorded here automatically as you use the platform"
            />
          ) : (
            <>
              <div>
                {groups.map((group, gi) => (
                  <div key={group.date}>
                    <div className={cn(
                      'px-6 py-2 bg-muted/50 text-xs font-medium text-muted-foreground',
                      gi > 0 && 'border-t border-border'
                    )}>
                      {group.date}
                    </div>
                    <div className="divide-y divide-border">
                      {group.entries.map(entry => {
                        const tz = getTenantTimezone() || undefined
                        const time = tz
                          ? formatInTimeZone(new Date(entry.created_at), tz, 'h:mm a')
                          : new Date(entry.created_at).toLocaleTimeString('en-US', { hour: 'numeric', minute: '2-digit' })
                        const isHigh = HIGH_SEVERITY.has(entry.action)
                        const isMedium = MEDIUM_SEVERITY.has(entry.action)
                        const isExpanded = expandedId === entry.id
                        const link = resourceLink(entry)
                        const meta = formatMetadata(entry.metadata)
                        const sim = simContext(entry)

                        return (
                          <div key={entry.id}>
                            <div
                              className={cn(
                                'flex items-center px-6 py-2.5 transition-colors cursor-pointer',
                                isExpanded ? 'bg-muted/50' : 'hover:bg-muted/30',
                                isHigh ? 'border-l-2 border-l-destructive' : isMedium ? 'border-l-2 border-l-amber-400' : 'border-l-2 border-l-transparent'
                              )}
                              onClick={() => setExpandedId(isExpanded ? null : entry.id)}
                            >
                              <span className="text-sm text-muted-foreground w-20 shrink-0 tabular-nums">{time}</span>
                              <Badge variant={actionVariant(entry.action)}>{prettyLabel(entry.action)}</Badge>
                              <span className="text-sm text-foreground ml-2.5 flex-1 truncate" title={describeAction(entry)}>
                                {describeAction(entry)}
                              </span>
              {/* The chip carries the simulated instant itself, not just the fact
                  that there is one. The whole reason this axis exists is that the
                  row's visible timestamp (wall-clock, today) says nothing about
                  when the action happened IN the simulation — so making the
                  operator expand the row to find that out defeats the point. */}
                              {sim.simEffectiveAt && (
                                <span
                                  title={`Operator clicked at ${formatDateTime(entry.created_at)} (real time); the clock stood at simulated ${formatDateTime(sim.simEffectiveAt)}${sim.testClockID ? ` on test clock ${sim.testClockID}` : ''}`}
                                  className="inline-flex shrink-0 items-center gap-1 rounded border border-amber-500/30 bg-amber-500/10 px-1.5 py-0.5 text-[10px] font-medium leading-none text-amber-800 dark:text-amber-300 ml-2"
                                >
                                  Simulated
                                  <span className="font-mono opacity-80">{formatDateTime(sim.simEffectiveAt)}</span>
                                </span>
                              )}
                              {link && (
                                <Link
                                  to={link}
                                  onClick={e => e.stopPropagation()}
                                  className="text-xs text-primary hover:underline shrink-0 ml-3"
                                >
                                  View
                                </Link>
                              )}
                              <span className="text-xs text-muted-foreground shrink-0 ml-3 w-28 text-right truncate" title={entry.actor_id}>
                                {formatActorName(entry)}
                              </span>
                              <ChevronRight size={14} className={cn('text-muted-foreground ml-2 shrink-0 transition-transform', isExpanded && 'rotate-90')} />
                            </div>

                            {/* Expanded detail */}
                            {isExpanded && (
                              <div className="bg-muted/50 px-6 py-3 border-t border-border">
                                <div className="grid grid-cols-2 lg:grid-cols-4 gap-3 text-sm">
                                  <div>
                                    <p className="text-xs text-muted-foreground">Resource Type</p>
                                    <p className="text-foreground mt-0.5">{entry.resource_type.replace(/_/g, ' ')}</p>
                                  </div>
                                  <div>
                                    <p className="text-xs text-muted-foreground">Resource ID</p>
                                    <p className="text-foreground font-mono text-xs mt-0.5 truncate flex items-center gap-1" title={entry.resource_id}>
                                      <span className="truncate">{entry.resource_id}</span>
                                      {entry.resource_id && <CopyButton text={entry.resource_id} />}
                                    </p>
                                  </div>
                                  <div>
                                    <p className="text-xs text-muted-foreground">Actor</p>
                                    <p className="text-foreground mt-0.5">{formatActorName(entry)}</p>
                                  </div>
                                  <div>
                                    <p className="text-xs text-muted-foreground">Timestamp</p>
                                    <p className="text-foreground mt-0.5">{formatDateTime(entry.created_at)}</p>
                                    {sim.simEffectiveAt && (
                                      <p className="text-xs text-amber-700 dark:text-amber-400 mt-0.5">
                                        Effect on test clock {sim.testClockID ? <span className="font-mono">{sim.testClockID}</span> : ''} at {formatDateTime(sim.simEffectiveAt)}
                                      </p>
                                    )}
                                  </div>
                                  <div>
                                    <p className="text-xs text-muted-foreground">IP address</p>
                                    <p className="text-foreground font-mono text-xs mt-0.5">{entry.ip_address || '—'}</p>
                                  </div>
                                </div>
                                {meta.length > 0 && (
                                  <div className="mt-3 pt-3 border-t border-border">
                                    <p className="text-xs font-medium text-muted-foreground mb-2">Details</p>
                                    <div className="grid grid-cols-2 lg:grid-cols-3 gap-2">
                                      {meta.map(m => (
                                        <div key={m.label} className="bg-background rounded-lg px-3 py-2 border border-border">
                                          <p className="text-xs text-muted-foreground">{m.label}</p>
                                          <p className="text-sm text-foreground mt-0.5 font-mono truncate" title={m.value}>{m.value}</p>
                                        </div>
                                      ))}
                                    </div>
                                  </div>
                                )}
                              </div>
                            )}
                          </div>
                        )
                      })}
                    </div>
                  </div>
                ))}
              </div>

              {/* Pagination — cursor-based Previous/Next (no numbered jumps:
                  the seek query is O(log N) per page at any depth, where the
                  numbered-offset COUNT was a full scan per click). */}
              {(page > 1 || hasNext) && (
                <div className="border-t border-border px-4 py-3 flex items-center justify-between">
                  <p className="text-xs text-muted-foreground">
                    Page {page}{total > 0 ? ` · ${formatTotal(total)} total` : ''}
                  </p>
                  <Pagination>
                    <PaginationContent>
                      <PaginationItem>
                        <PaginationPrevious
                          onClick={() => setCursors(c => c.slice(0, -1))}
                          className={cn(page <= 1 && 'pointer-events-none opacity-50')}
                        />
                      </PaginationItem>
                      <PaginationItem>
                        <PaginationNext
                          onClick={() => nextCursor && setCursors(c => [...c, nextCursor])}
                          className={cn(!hasNext && 'pointer-events-none opacity-50')}
                        />
                      </PaginationItem>
                    </PaginationContent>
                  </Pagination>
                </div>
              )}
            </>
          )}
        </CardContent>
      </Card>
    </Layout>
  )
}
