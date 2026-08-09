import { useState, useMemo } from 'react'
import { usePageTitle } from '@/hooks/usePageTitle'
import { Link, useNavigate } from 'react-router-dom'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'
import { api, formatDate } from '@/lib/api'
import type { Customer, Subscription } from '@/lib/api'
import { downloadServerCSV } from '@/lib/csv'
import { Layout } from '@/components/Layout'
import { useSortable, type SortDir } from '@/hooks/useSortable'
import { useUrlState } from '@/hooks/useUrlState'
import { useDebouncedValue } from '@/hooks/useDebouncedValue'
import { cn } from '@/lib/utils'
import { statusBadgeVariant, statusBorderColor } from '@/lib/status'
import { InitialsAvatar } from '@/components/InitialsAvatar'
import { TestClockBadge } from '@/components/TestClockBadge'
import { CreateSubscriptionDialog } from '@/components/CreateSubscriptionDialog'

import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Badge } from '@/components/ui/badge'
import { Card, CardContent } from '@/components/ui/card'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import {
  Pagination,
  PaginationContent,
  PaginationItem,
  PaginationLink,
  PaginationNext,
  PaginationPrevious,
} from '@/components/ui/pagination'
import { TableSkeleton } from '@/components/ui/TableSkeleton'

import { Plus, Search, Download, ArrowUpDown, ArrowUp, ArrowDown, Repeat } from 'lucide-react'
import { EmptyState } from '@/components/EmptyState'
import { ExpiryBadge } from '@/components/ExpiryBadge'
import { effectiveNow } from '@/lib/effectiveNow'
import { useTestClocks } from '@/hooks/useClockFrozenMap'

const PAGE_SIZE = 25

function SortableHead({
  label, sortKey: key, activeSortKey, sortDir, onSort, className,
}: {
  label: string; sortKey: string; activeSortKey: string; sortDir: 'asc' | 'desc'; onSort: (key: string) => void; className?: string
}) {
  const active = key === activeSortKey
  return (
    <TableHead className={className}>
      <button
        onClick={() => onSort(key)}
        className="flex items-center gap-1.5 text-xs font-medium hover:text-foreground transition-colors"
      >
        {label}
        {active ? (
          sortDir === 'asc' ? <ArrowUp size={14} /> : <ArrowDown size={14} />
        ) : (
          <ArrowUpDown size={14} className="opacity-40" />
        )}
      </button>
    </TableHead>
  )
}

export default function SubscriptionsPage() {
  usePageTitle('Subscriptions')
  const [showCreate, setShowCreate] = useState(false)
  const [urlState, setUrlState] = useUrlState({
    search: '',
    status: '',
    page: '1',
    sort: 'created_at',
    dir: 'desc',
  })
  const { search, status: filterStatus, sort: sortKey } = urlState
  const sortDir = urlState.dir as SortDir
  const page = Math.max(1, parseInt(urlState.page) || 1)
  const navigate = useNavigate()
  const queryClient = useQueryClient()

  // Debounced server-side search (display_name / code) across the
  // full dataset — not just the visible page.
  const debouncedSearch = useDebouncedValue(search.trim(), 300)

  const queryParams = useMemo(() => {
    const params = new URLSearchParams()
    params.set('limit', String(PAGE_SIZE))
    params.set('offset', String((page - 1) * PAGE_SIZE))
    if (filterStatus) params.set('status', filterStatus)
    if (debouncedSearch) params.set('search', debouncedSearch)
    if (sortKey) params.set('sort', sortKey)
    if (sortDir) params.set('dir', sortDir)
    return params.toString()
  }, [page, filterStatus, debouncedSearch, sortKey, sortDir])

  const { data: subsData, isLoading: loading, error: loadErrorObj, refetch } = useQuery({
    queryKey: ['subscriptions', queryParams],
    queryFn: () => api.listSubscriptions(queryParams),
    placeholderData: (prev) => prev,
  })

  // Fetch only the customers referenced by the visible subs (avoids
  // the "Unknown" pagination bug — see Invoices.tsx for full rationale).
  const customerIdsForRef = useMemo(() => {
    const set = new Set<string>()
    ;(subsData?.data ?? []).forEach(s => { if (s.customer_id) set.add(s.customer_id) })
    return Array.from(set)
  }, [subsData])
  const { data: customersData } = useQuery({
    queryKey: ['customers-by-ids-for-subs', customerIdsForRef],
    queryFn: () => api.listCustomers(customerIdsForRef.length > 0 ? `ids=${customerIdsForRef.join(',')}&limit=${customerIdsForRef.length}` : ''),
    enabled: customerIdsForRef.length > 0,
  })

  const { data: plansData } = useQuery({
    queryKey: ['plans-ref'],
    queryFn: () => api.listPlans(),
  })

  // Test clocks → frozen_time map so per-row "Trial ends in N days"
  // reads from simulation time when the sub is pinned to a clock.
  // useTestClocks is mode-gated (no fetch in live, where the server
  // refuses the resource) and shares one cache entry app-wide.
  const { data: testClocksData } = useTestClocks()
  const clockFrozenMap = useMemo(() => {
    const m: Record<string, string> = {}
    ;(testClocksData?.data ?? []).forEach(c => { m[c.id] = c.frozen_time })
    return m
  }, [testClocksData])
  // Clock id → name lookup for the customer-inherits-clock hint
  // (ADR-027). Surfaced inside the Create dialog when the picked
  // customer is pinned to a clock so the operator knows the new
  // sub will bill on simulated time.
  const clockNameMap = useMemo(() => {
    const m: Record<string, string> = {}
    ;(testClocksData?.data ?? []).forEach(c => { m[c.id] = c.name || c.id })
    return m
  }, [testClocksData])

  const subs = subsData?.data ?? []
  const total = subsData?.total ?? 0
  const loadError = loadErrorObj instanceof Error ? loadErrorObj.message : loadErrorObj ? String(loadErrorObj) : null
  const customers = customersData?.data ?? []
  const customerMap = useMemo(() => {
    const cMap: Record<string, Customer> = {}
    customers.forEach(c => { cMap[c.id] = c })
    return cMap
  }, [customers])
  const plans = plansData?.data ?? []

  // Search is server-side (search= query param over display_name +
  // code) — rows arriving here are already filtered across the full
  // dataset, not just the page.
  //
  // Server-side sort end-to-end (closed allow-list + id tie-break).
  // Client-side re-sort would only sort the current page, breaking
  // pagination semantics.
  const { onSort } = useSortable(
    subs,
    sortKey,
    sortDir,
    (key, dir) => setUrlState({ sort: key, dir }),
  )
  const sorted = subs

  const totalPages = Math.ceil(total / PAGE_SIZE)

  // Server-side streaming export: full tenant dataset (every column
  // on Subscription including item plan_ids, trial/started/canceled
  // timestamps), not just the currently-visible page.
  const handleExport = async () => {
    try {
      await downloadServerCSV('/v1/exports/subscriptions.csv', 'subscriptions.csv')
    } catch (err) {
      const msg = err instanceof Error ? err.message : 'Export failed'
      toast.error(msg)
    }
  }

  return (
    <Layout>
      {/* Page header */}
      <div className="flex justify-between items-center">
        <div>
          <h1 className="text-2xl font-semibold text-foreground">Subscriptions</h1>
          <p className="text-sm text-muted-foreground mt-1">Manage subscriptions and billing cycles{total > 0 ? ` · ${total} total` : ''}</p>
        </div>
        <div className="flex items-center gap-2">
          {total > 0 && (
            <Button variant="outline" size="sm" onClick={handleExport}>
              <Download size={16} className="mr-2" />
              Export CSV
            </Button>
          )}
          {(total > 0 || filterStatus) && (
            <div className="flex gap-1 bg-muted rounded-lg p-1">
              {[
                { value: '', label: 'All' },
                { value: 'active', label: 'Active' },
                { value: 'paused', label: 'Paused' },
                { value: 'canceled', label: 'Canceled' },
                { value: 'draft', label: 'Draft' },
              ].map(f => (
                <button
                  key={f.value}
                  onClick={() => setUrlState({ status: f.value, page: '1' })}
                  className={cn(
                    'px-3 py-1.5 rounded-md text-xs font-medium transition-colors',
                    filterStatus === f.value
                      ? 'bg-background text-foreground shadow-sm'
                      : 'text-muted-foreground hover:text-foreground'
                  )}
                >
                  {f.label}
                </button>
              ))}
            </div>
          )}
          <Button size="sm" onClick={() => setShowCreate(true)}>
            <Plus size={16} className="mr-2" />
            Add Subscription
          </Button>
        </div>
      </div>

      {/* Search — server-side across the full dataset. Stays visible
          while a search is active so a zero-result query never hides
          its own input. */}
      {(total > 0 || search) && (
        <div className="flex items-center gap-3 mt-6">
          <div className="relative flex-1">
            <Search size={16} className="absolute left-3 top-2.5 text-muted-foreground" />
            <Input
              value={search}
              onChange={e => setUrlState({ search: e.target.value, page: '1' })}
              placeholder="Search by name or code..."
              className="pl-9"
            />
          </div>
        </div>
      )}

      {/* Table */}
      <Card className="mt-4">
        <CardContent className="p-0">
          {loadError ? (
            <div className="p-8 text-center">
              <p className="text-sm text-destructive mb-3">{loadError}</p>
              <Button variant="outline" size="sm" onClick={() => refetch()}>
                Retry
              </Button>
            </div>
          ) : loading ? (
            <TableSkeleton columns={6} />
          ) : total === 0 ? (
            filterStatus || debouncedSearch ? (
              <EmptyState
                title={
                  debouncedSearch
                    ? `No subscriptions match “${debouncedSearch}”`
                    : `No ${filterStatus} subscriptions`
                }
                description="Try a different filter to see more results."
                action={{
                  label: 'Clear filters',
                  variant: 'outline',
                  onClick: () => setUrlState({ status: '', search: '', page: '1' }),
                }}
              />
            ) : (
              <EmptyState
                icon={Repeat}
                title="No subscriptions yet"
                description="Create a subscription to start billing a customer."
                action={{
                  label: 'Add Subscription',
                  icon: Plus,
                  onClick: () => setShowCreate(true),
                }}
              />
            )
          ) : sorted.length === 0 ? (
            // Stale ?page= beyond the filtered range — point back to
            // page 1 rather than rendering a bare table.
            <div className="px-6 py-8 text-center">
              <p className="text-sm text-muted-foreground mb-3">This page is out of range for the current filters</p>
              <Button variant="outline" size="sm" onClick={() => setUrlState({ page: '1' })}>
                Back to first page
              </Button>
            </div>
          ) : (
            <>
              <Table>
                <TableHeader>
                  <TableRow>
                    <SortableHead label="Name" sortKey="display_name" activeSortKey={sortKey} sortDir={sortDir} onSort={onSort} />
                    <TableHead className="text-xs font-medium">Customer</TableHead>
                    <TableHead className="text-xs font-medium">Plan</TableHead>
                    <TableHead className="text-xs font-medium">Code</TableHead>
                    <SortableHead label="Status" sortKey="status" activeSortKey={sortKey} sortDir={sortDir} onSort={onSort} />
                    <SortableHead label="Next Billing" sortKey="next_billing_at" activeSortKey={sortKey} sortDir={sortDir} onSort={onSort} />
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {sorted.map((sub: Subscription) => (
                    <TableRow
                      key={sub.id}
                      className={cn('cursor-pointer hover:bg-muted/50 transition-colors border-l-[3px]', statusBorderColor(sub.status))}
                      onClick={(e) => {
                        const target = e.target as HTMLElement
                        if (target.closest('button, a, input, select')) return
                        navigate(`/subscriptions/${sub.id}`)
                      }}
                    >
                      <TableCell>
                        <div className="flex items-center gap-1.5">
                          <Link
                            to={`/subscriptions/${sub.id}`}
                            className="text-sm font-medium text-foreground hover:text-primary transition-colors truncate block max-w-[180px]"
                            title={sub.display_name}
                          >
                            {sub.display_name}
                          </Link>
                          {sub.test_clock_id && (
                            <TestClockBadge testClockId={sub.test_clock_id} />
                          )}
                        </div>
                      </TableCell>
                      <TableCell className="text-sm">
                        <div className="flex items-center gap-2.5">
                          <InitialsAvatar name={customerMap[sub.customer_id]?.display_name || 'Unknown'} size="xs" />
                          <Link
                            to={`/customers/${sub.customer_id}`}
                            onClick={e => e.stopPropagation()}
                            className="text-sm font-medium text-foreground hover:text-primary truncate block max-w-[160px]"
                            title={customerMap[sub.customer_id]?.display_name || 'Unknown'}
                          >
                            {customerMap[sub.customer_id]?.display_name || 'Unknown'}
                          </Link>
                        </div>
                      </TableCell>
                      <TableCell className="text-sm text-muted-foreground">
                        {(() => {
                          const items = sub.items ?? []
                          if (items.length === 0) return '\u2014'
                          if (items.length === 1) {
                            const name = plans.find(p => p.id === items[0].plan_id)?.name
                            const qty = items[0].quantity
                            return qty > 1 ? `${name || '\u2014'} × ${qty}` : (name || '\u2014')
                          }
                          const title = items
                            .map(it => `${plans.find(p => p.id === it.plan_id)?.name || it.plan_id}${it.quantity > 1 ? ` × ${it.quantity}` : ''}`)
                            .join(', ')
                          return <span title={title}>{items.length} items</span>
                        })()}
                      </TableCell>
                      <TableCell className="text-sm text-muted-foreground font-mono truncate max-w-[120px]" title={sub.code}>
                        {sub.code}
                      </TableCell>
                      <TableCell>
                        <div className="flex items-center gap-2">
                          <Badge variant={statusBadgeVariant(sub.status)}>{sub.status}</Badge>
                          <ExpiryBadge
                            expiresAt={sub.trial_end_at}
                            label="Trial ends"
                            warningDays={3}
                            now={effectiveNow(sub.test_clock_id ? clockFrozenMap[sub.test_clock_id] : undefined)}
                          />
                        </div>
                      </TableCell>
                      <TableCell className="text-sm text-muted-foreground">
                        {sub.next_billing_at ? formatDate(sub.next_billing_at) : '\u2014'}
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>

              {/* Pagination */}
              {totalPages > 1 && (
                <div className="border-t border-border px-4 py-3 flex items-center justify-between">
                  <p className="text-xs text-muted-foreground">
                    Showing {(page - 1) * PAGE_SIZE + 1}
                    {'\u2013'}
                    {Math.min(page * PAGE_SIZE, total)} of {total}
                  </p>
                  <Pagination>
                    <PaginationContent>
                      <PaginationItem>
                        <PaginationPrevious
                          onClick={() => setUrlState({ page: String(Math.max(1, page - 1)) })}
                          className={cn(page <= 1 && 'pointer-events-none opacity-50')}
                        />
                      </PaginationItem>
                      {Array.from({ length: Math.min(totalPages, 5) }, (_, i) => {
                        let pageNum: number
                        if (totalPages <= 5) {
                          pageNum = i + 1
                        } else if (page <= 3) {
                          pageNum = i + 1
                        } else if (page >= totalPages - 2) {
                          pageNum = totalPages - 4 + i
                        } else {
                          pageNum = page - 2 + i
                        }
                        return (
                          <PaginationItem key={pageNum}>
                            <PaginationLink
                              onClick={() => setUrlState({ page: String(pageNum) })}
                              isActive={page === pageNum}
                            >
                              {pageNum}
                            </PaginationLink>
                          </PaginationItem>
                        )
                      })}
                      <PaginationItem>
                        <PaginationNext
                          onClick={() => setUrlState({ page: String(Math.min(totalPages, page + 1)) })}
                          className={cn(page >= totalPages && 'pointer-events-none opacity-50')}
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

      <CreateSubscriptionDialog
        open={showCreate}
        onOpenChange={setShowCreate}
        plans={plans}
        clockNameMap={clockNameMap}
        onCreated={(created) => {
          queryClient.invalidateQueries({ queryKey: ['subscriptions'] })
          toast.success(`Subscription "${created.display_name}" created`)
          setShowCreate(false)
        }}
      />
    </Layout>
  )
}
