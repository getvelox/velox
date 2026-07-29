import { useState, useMemo } from 'react'
import { usePageTitle } from '@/hooks/usePageTitle'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from 'react-router-dom'
import { useMutation } from '@tanstack/react-query'
import { useForm } from 'react-hook-form'
import { zodResolver } from '@hookform/resolvers/zod'
import { z } from 'zod'
import { toast } from 'sonner'
import { invalidateMoneySurfaces } from '@/lib/invalidateMoney'
import { api, formatCents, formatDate, formatDateTime, getCurrencySymbol } from '@/lib/api'
import { CustomerCombobox } from '@/components/CustomerCombobox'
import { endOfDayInTZ } from '@/lib/dates'
import type { Customer, CreditBalance } from '@/lib/api'
import { applyApiError } from '@/lib/formErrors'
import { downloadCSV } from '@/lib/csv'
import { Layout } from '@/components/Layout'
import { useEffectiveNow } from '@/hooks/useClockFrozenMap'
import { TestClockBadge } from '@/components/TestClockBadge'
import { useSortable, type SortDir } from '@/hooks/useSortable'
import { useUrlState } from '@/hooks/useUrlState'
import { cn } from '@/lib/utils'
import { InitialsAvatar } from '@/components/InitialsAvatar'
import { DatePicker } from '@/components/ui/date-picker'
import { Checkbox } from '@/components/ui/checkbox'

import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Badge } from '@/components/ui/badge'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from '@/components/ui/alert-dialog'
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
import {
  Form,
  FormControl,
  FormDescription,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
} from '@/components/ui/form'
import { TableSkeleton } from '@/components/ui/TableSkeleton'

import { Plus, Minus, Download, ChevronRight, ArrowLeft, Loader2, ArrowUpDown, ArrowUp, ArrowDown, Coins } from 'lucide-react'
import { EmptyState } from '@/components/EmptyState'

const creditSchema = z.object({
  amount: z.string().min(1, 'Amount is required').refine(v => parseFloat(v) >= 0.01, 'Must be at least 0.01'),
  description: z.string().min(1, 'Description is required'),
  expiresAt: z.string(),
  // ADR-078: free/marketing credits — drained before paid credits.
  promotional: z.boolean(),
})

type CreditFormData = z.infer<typeof creditSchema>

const ENTRY_TYPES = ['All', 'grant', 'usage', 'adjustment'] as const

// Human labels for ledger entry types — 'usage' as a bare badge is
// ambiguous (a usage event? a deduction?); spell it out.
const ENTRY_TYPE_LABELS: Record<string, string> = {
  grant: 'Grant',
  usage: 'Deduction',
  adjustment: 'Adjustment',
  expiry: 'Expiry',
}
const entryTypeLabel = (t: string): string => ENTRY_TYPE_LABELS[t] ?? t

// The expiry worker writes "Expired grant <ledger-id>" descriptions. The
// stored text is display-only: since migration 0127 the backend's
// exactly-once expiry rides a consumed-vs-amount row-lock gate, not the
// old description-LIKE dedup. Display-side we render the human phrase and
// demote the machine id to subtext.
const EXPIRED_GRANT_RE = /^Expired grant (vlx_ccl_\w+)$/

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

function entryTypeVariant(type: string): 'default' | 'secondary' | 'destructive' | 'outline' {
  switch (type) {
    case 'grant': return 'default'
    case 'usage': return 'secondary'
    case 'adjustment': return 'outline'
    default: return 'outline'
  }
}

export default function CreditsPage() {
  usePageTitle('Credits')
  const queryClient = useQueryClient()
  const ledgerPageSize = 25

  // Modals
  const [showGrant, setShowGrant] = useState(false)
  const [showDeduct, setShowDeduct] = useState(false)
  const [modalCustomerId, setModalCustomerId] = useState('')

  const [urlState, setUrlState] = useUrlState({
    customer: '',
    entryType: 'All',
    page: '1',
    sort: 'created_at',
    dir: 'desc',
  })
  const selectedCustomerId = urlState.customer || null
  const entryTypeFilter = urlState.entryType
  const ledgerPage = Math.max(1, parseInt(urlState.page) || 1)

  // Page-level data: balances + customers. Two parallel queries
  // instead of one Promise.all so each section paints independently
  // when ready. RQ dedupes + caches across remounts and provides the
  // loading/error states the JSX consumes below.
  const balancesQuery = useQuery({
    queryKey: ['credits', 'balances'],
    queryFn: () => api.listBalances(),
  })
  // Fetch ONLY the customers referenced by balances (ids=): the old
  // bare 50-row page meant a customer with credits past the 50th row
  // vanished from this table entirely — outstanding balance invisible.
  const balanceCustomerIds = useMemo(
    () => Array.from(new Set((balancesQuery.data?.data ?? []).map(b => b.customer_id))),
    [balancesQuery.data],
  )
  const customersQuery = useQuery({
    queryKey: ['credits', 'customers', balanceCustomerIds],
    queryFn: () =>
      api.listCustomers(
        balanceCustomerIds.length > 0
          ? `ids=${balanceCustomerIds.join(',')}&limit=${balanceCustomerIds.length}`
          : '',
      ),
    enabled: !balancesQuery.isLoading,
  })
  const balances = balancesQuery.data?.data ?? []
  const customers = customersQuery.data?.data ?? []
  const loading = balancesQuery.isLoading || customersQuery.isLoading
  const firstErr = balancesQuery.error ?? customersQuery.error
  const error = firstErr instanceof Error ? firstErr.message : (firstErr ? 'Failed to load credits' : null)
  const loadData = () => {
    invalidateMoneySurfaces(queryClient)
  }

  // Per-customer ledger query — keyed by (customer, sort, dir) so
  // changing the sort triggers a refetch and the URL state survives
  // back-button navigation. Disabled when no customer is selected.
  const ledgerQuery = useQuery({
    queryKey: ['credits', 'ledger', urlState.customer, urlState.sort, urlState.dir, urlState.entryType, urlState.page],
    queryFn: () => api.listLedger(urlState.customer, {
      sort: urlState.sort,
      dir: urlState.dir,
      // Server-side page + filter: the store caps every response (50
      // default / 100 max), so client-side slicing presented a capped
      // fetch as the complete ledger — pagination, the type filter, and
      // the CSV all lied past 50 rows.
      limit: ledgerPageSize,
      offset: (Math.max(1, parseInt(urlState.page) || 1) - 1) * ledgerPageSize,
      ...(urlState.entryType !== 'All' ? { entry_type: urlState.entryType } : {}),
    }),
    enabled: !!urlState.customer,
  })
  const ledger = ledgerQuery.data?.data ?? []
  const ledgerTotal = ledgerQuery.data?.total ?? 0
  const ledgerLoading = ledgerQuery.isLoading && !!urlState.customer
  const loadLedger = (customerId: string) => {
    void queryClient.invalidateQueries({ queryKey: ['credits', 'ledger', customerId] })
  }

  const customerMap = useMemo(() => {
    const m: Record<string, Customer> = {}
    customers.forEach(c => { m[c.id] = c })
    return m
  }, [customers])

  const openCustomerDetail = (customerId: string) => {
    setUrlState({ customer: customerId, entryType: 'All', page: '1' })
  }

  const closeDetail = () => {
    setUrlState({ customer: '' })
  }

  const totalOutstanding = balances.reduce((sum, b) => sum + b.balance_cents, 0)
  const selectedBalance = balances.find(b => b.customer_id === selectedCustomerId) || null
  const selectedCustomer = selectedCustomerId ? customerMap[selectedCustomerId] : null

  // entry-type filtering happens SERVER-SIDE now (query param above);
  // the fetched page is already the filtered page.
  const filteredLedger = ledger

  const ledgerSortKey = urlState.sort
  const ledgerSortDir = urlState.dir as SortDir
  // Server-side sort: useSortable only owns the click-handler /
  // direction-flip / URL-state semantics. The data is rendered as
  // returned by the server (filtered and paginated server-side too).
  const { onSort: onLedgerSort } = useSortable(
    filteredLedger,
    ledgerSortKey,
    ledgerSortDir,
    (key, dir) => setUrlState({ sort: key, dir }),
  )
  const sortedLedger = filteredLedger

  const ledgerTotalPages = Math.ceil(ledgerTotal / ledgerPageSize)
  const ledgerCurrentPage = Math.min(ledgerPage, ledgerTotalPages || 1)
  // Server-paginated: the fetched slice IS the page.
  const ledgerPaginated = sortedLedger

  // ---- Detail View ----
  if (selectedCustomerId) {
    return (
      <Layout>
        {/* Header with back, customer info, balance, and actions */}
        <div className="flex items-start gap-3 mb-6">
          <Button variant="ghost" size="sm" onClick={closeDetail} className="mt-1">
            <ArrowLeft size={18} />
          </Button>
          <div className="flex-1">
            <div className="flex items-center gap-3">
              <h1 className="text-2xl font-semibold text-foreground">
                {selectedCustomer?.display_name || 'Customer'}
              </h1>
              <Link to={`/customers/${selectedCustomerId}`} className="text-xs text-primary hover:underline">
                View customer
              </Link>
            </div>
            <p className="text-sm text-muted-foreground mt-0.5">Credit Balance</p>
            <p className={cn('text-3xl font-semibold mt-1', (selectedBalance?.balance_cents || 0) > 0 ? 'text-emerald-600' : 'text-muted-foreground')}>
              {formatCents(selectedBalance?.balance_cents || 0)}
            </p>
          </div>
          <div className="flex items-center gap-2 mt-1">
            <Button variant="outline" size="sm" onClick={() => { setModalCustomerId(selectedCustomerId); setShowDeduct(true) }}>
              <Minus size={16} className="mr-2" /> Deduct
            </Button>
            <Button size="sm" onClick={() => { setModalCustomerId(selectedCustomerId); setShowGrant(true) }}>
              <Plus size={16} className="mr-2" /> Grant
            </Button>
          </div>
        </div>

        {/* Transaction History */}
        <Card>
          <CardHeader className="flex flex-row items-center justify-between pb-3">
            <CardTitle className="text-sm">
              <span className="inline-flex items-center gap-2">
                Transaction History
                {/* Ledger dates on a clock-pinned customer are simulated —
                    one badge covers the card, same pattern as the detail
                    pages (clock-design review 2026-07-21). */}
                {selectedCustomer?.test_clock_id && (
                  <TestClockBadge testClockId={selectedCustomer.test_clock_id} link />
                )}
              </span>
            </CardTitle>
            <div className="flex items-center gap-2">
              <select
                value={entryTypeFilter}
                onChange={e => setUrlState({ entryType: e.target.value, page: '1' })}
                className="flex h-8 rounded-md border border-input bg-transparent px-3 py-1 text-sm shadow-xs transition-colors focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring"
              >
                {ENTRY_TYPES.map(t => (
                  <option key={t} value={t}>{t === 'All' ? 'All types' : entryTypeLabel(t)}</option>
                ))}
              </select>
              {ledger.length > 0 && (
                <Button variant="outline" size="sm" onClick={() => {
                  // Complete export: page through the FULL filtered ledger
                  // (store caps each response at 100) — pre-fix this
                  // exported the visible capped slice of an immutable
                  // money ledger as if it were everything.
                  void (async () => {
                    const all: typeof ledger = []
                    const pageSize = 100
                    for (let offset = 0; ; offset += pageSize) {
                      const res = await api.listLedger(urlState.customer, {
                        sort: urlState.sort,
                        dir: urlState.dir,
                        limit: pageSize,
                        offset,
                        ...(urlState.entryType !== 'All' ? { entry_type: urlState.entryType } : {}),
                      })
                      all.push(...res.data)
                      if (res.data.length < pageSize || all.length >= res.total) break
                    }
                    const rows = all.map(e => [
                      formatDateTime(e.created_at),
                      e.entry_type,
                      e.description,
                      (e.amount_cents / 100).toFixed(2),
                      (e.balance_after / 100).toFixed(2),
                      e.invoice_id || '',
                    ])
                    downloadCSV(`credits-${selectedCustomer?.external_id || 'customer'}.csv`,
                      ['Date', 'Type', 'Description', 'Amount', 'Balance', 'Invoice ID'], rows)
                  })()
                }}>
                  <Download size={14} className="mr-1" /> CSV
                </Button>
              )}
            </div>
          </CardHeader>
          <CardContent className="p-0">
            {ledgerLoading ? (
              <TableSkeleton columns={5} />
            ) : filteredLedger.length === 0 ? (
              <div className="px-6 py-12 text-center">
                <p className="text-sm font-medium text-foreground">No transactions{entryTypeFilter !== 'All' ? ` of type "${entryTypeFilter}"` : ''}</p>
                <p className="text-sm text-muted-foreground mt-1">Grant credits to get started</p>
              </div>
            ) : (
              <>
                <Table>
                  <TableHeader>
                    <TableRow>
                      <SortableHead label="Date" sortKey="created_at" activeSortKey={ledgerSortKey} sortDir={ledgerSortDir} onSort={onLedgerSort} />
                      <SortableHead label="Type" sortKey="entry_type" activeSortKey={ledgerSortKey} sortDir={ledgerSortDir} onSort={onLedgerSort} />
                      <TableHead className="text-xs font-medium">Description</TableHead>
                      <SortableHead label="Amount" sortKey="amount_cents" activeSortKey={ledgerSortKey} sortDir={ledgerSortDir} onSort={onLedgerSort} className="text-right" />
                      <TableHead className="text-xs font-medium text-right">Balance</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {ledgerPaginated.map(entry => (
                      <TableRow key={entry.id}>
                        <TableCell className="text-sm text-muted-foreground whitespace-nowrap" title={formatDateTime(entry.created_at)}>{formatDate(entry.created_at)}</TableCell>
                        <TableCell>
                          <span className="inline-flex items-center gap-1.5">
                            <Badge variant={entryTypeVariant(entry.entry_type)}>{entryTypeLabel(entry.entry_type)}</Badge>
                            {entry.grant_kind === 'commit' && <Badge variant="outline">Commit</Badge>}
                            {entry.grant_kind === 'promotional' && <Badge variant="secondary">Promo</Badge>}
                          </span>
                        </TableCell>
                        <TableCell className="text-sm text-foreground">
                          {(() => {
                            const expired = entry.description?.match(EXPIRED_GRANT_RE)
                            const text = expired ? 'Grant expired' : (entry.description || '—')
                            return (
                              <>
                                {entry.invoice_id ? (
                                  <Link to={`/invoices/${entry.invoice_id}`} className="text-primary hover:underline">
                                    {text}
                                  </Link>
                                ) : (
                                  text
                                )}
                                {expired && (
                                  <span className="block text-xs text-muted-foreground font-mono mt-0.5" title={expired[1]}>
                                    {expired[1].slice(0, 18)}…
                                  </span>
                                )}
                                {entry.expires_at && !expired && (
                                  <span className="block text-xs text-muted-foreground mt-0.5">
                                    Expires {formatDate(entry.expires_at)}
                                  </span>
                                )}
                              </>
                            )
                          })()}
                        </TableCell>
                        <TableCell className={cn('text-right tabular-nums font-mono text-sm', entry.amount_cents >= 0 ? 'text-emerald-600' : 'text-destructive')}>
                          {entry.amount_cents >= 0 ? '+' : ''}{formatCents(entry.amount_cents)}
                        </TableCell>
                        <TableCell className="text-right tabular-nums font-mono text-sm">{formatCents(entry.balance_after)}</TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>

                {/* Pagination */}
                {ledgerTotalPages > 1 && (
                  <div className="border-t border-border px-4 py-3 flex items-center justify-between">
                    <p className="text-xs text-muted-foreground">
                      Showing {(ledgerCurrentPage - 1) * ledgerPageSize + 1}
                      {'\u2013'}
                      {(ledgerCurrentPage - 1) * ledgerPageSize + ledgerPaginated.length} of {ledgerTotal}
                    </p>
                    <Pagination>
                      <PaginationContent>
                        <PaginationItem>
                          <PaginationPrevious
                            onClick={() => setUrlState({ page: String(Math.max(1, ledgerCurrentPage - 1)) })}
                            className={cn(ledgerCurrentPage <= 1 && 'pointer-events-none opacity-50')}
                          />
                        </PaginationItem>
                        {Array.from({ length: Math.min(ledgerTotalPages, 5) }, (_, i) => {
                          let pageNum: number
                          if (ledgerTotalPages <= 5) {
                            pageNum = i + 1
                          } else if (ledgerCurrentPage <= 3) {
                            pageNum = i + 1
                          } else if (ledgerCurrentPage >= ledgerTotalPages - 2) {
                            pageNum = ledgerTotalPages - 4 + i
                          } else {
                            pageNum = ledgerCurrentPage - 2 + i
                          }
                          return (
                            <PaginationItem key={pageNum}>
                              <PaginationLink
                                onClick={() => setUrlState({ page: String(pageNum) })}
                                isActive={ledgerCurrentPage === pageNum}
                              >
                                {pageNum}
                              </PaginationLink>
                            </PaginationItem>
                          )
                        })}
                        <PaginationItem>
                          <PaginationNext
                            onClick={() => setUrlState({ page: String(Math.min(ledgerTotalPages, ledgerCurrentPage + 1)) })}
                            className={cn(ledgerCurrentPage >= ledgerTotalPages && 'pointer-events-none opacity-50')}
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

        {showGrant && (
          <CreditDialog mode="grant" customerId={modalCustomerId}
            customerName={customerMap[modalCustomerId]?.display_name || ''}
            open={showGrant}
            onOpenChange={(open) => { if (!open) setShowGrant(false) }}
            onDone={() => { setShowGrant(false); loadLedger(selectedCustomerId); loadData(); toast.success('Credits granted') }} />
        )}
        {showDeduct && (
          <CreditDialog mode="deduct" customerId={modalCustomerId}
            customerName={customerMap[modalCustomerId]?.display_name || ''}
            open={showDeduct}
            onOpenChange={(open) => { if (!open) setShowDeduct(false) }}
            onDone={() => { setShowDeduct(false); loadLedger(selectedCustomerId); loadData(); toast.success('Credits deducted') }} />
        )}
      </Layout>
    )
  }

  // ---- List View ----
  const balMap: Record<string, CreditBalance> = {}
  balances.forEach(b => { balMap[b.customer_id] = b })
  const customersWithCredits = customers
    .filter(c => balMap[c.id])
    .map(c => ({ customer: c, balance: balMap[c.id] }))
    .sort((a, b) => b.balance.balance_cents - a.balance.balance_cents)

  return (
    <Layout>
      <div className="flex justify-between items-center">
        <div>
          <h1 className="text-2xl font-semibold text-foreground">Credits</h1>
          <p className="text-sm text-muted-foreground mt-1">
            Manage prepaid credit balances{customersWithCredits.length > 0 ? ` · ${formatCents(totalOutstanding)} across ${customersWithCredits.length} customer${customersWithCredits.length !== 1 ? 's' : ''}` : ''}
          </p>
        </div>
        {customersWithCredits.length > 0 && (
          <Button size="sm" onClick={() => { setModalCustomerId(''); setShowGrant(true) }}>
            <Plus size={16} className="mr-2" /> Grant Credits
          </Button>
        )}
      </div>

      {/* Customer balances table */}
      <Card className="mt-6">
        <CardContent className="p-0">
          {error ? (
            <div className="p-8 text-center">
              <p className="text-sm text-destructive mb-3">{error}</p>
              <Button variant="outline" size="sm" onClick={loadData}>
                Retry
              </Button>
            </div>
          ) : loading ? (
            <TableSkeleton columns={3} widths={['60%', '30%', '20%']} />
          ) : customersWithCredits.length === 0 ? (
            <EmptyState
              icon={Coins}
              title="No credit activity"
              description="Grant credits to a customer to get started."
              action={{
                label: 'Grant Credits',
                icon: Plus,
                onClick: () => { setModalCustomerId(''); setShowGrant(true) },
              }}
            />
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead className="text-xs font-medium">Customer</TableHead>
                  <TableHead className="text-xs font-medium text-right">Balance</TableHead>
                  <TableHead className="text-xs font-medium text-right"></TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {customersWithCredits.map(({ customer, balance }) => (
                  <TableRow
                    key={customer.id}
                    className="cursor-pointer hover:bg-muted/50 transition-colors"
                    onClick={() => openCustomerDetail(customer.id)}
                  >
                    <TableCell>
                      <div className="flex items-center gap-2.5">
                        <InitialsAvatar name={customer.display_name} />
                        <div>
                          <Link
                            to={`/customers/${customer.id}`}
                            onClick={e => e.stopPropagation()}
                            className="text-sm font-medium text-foreground hover:text-primary"
                          >
                            {customer.display_name}
                          </Link>
                          <p className="text-xs text-muted-foreground">{customer.external_id}</p>
                        </div>
                      </div>
                    </TableCell>
                    <TableCell className={cn('text-right tabular-nums font-mono text-sm',
                      balance.balance_cents > 0 ? 'text-emerald-600' : balance.balance_cents === 0 ? 'text-muted-foreground' : 'text-destructive'
                    )}>
                      {formatCents(balance.balance_cents)}
                    </TableCell>
                    <TableCell className="text-right">
                      <ChevronRight size={16} className="text-muted-foreground inline" />
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>

      {showGrant && (
        <CreditDialog mode="grant" customerId={modalCustomerId} customers={customers}
          customerName={customerMap[modalCustomerId]?.display_name || ''}
          open={showGrant}
          onOpenChange={(open) => { if (!open) setShowGrant(false) }}
          onDone={() => { setShowGrant(false); loadData(); toast.success('Credits granted') }} />
      )}
    </Layout>
  )
}

// ---- Grant / Deduct Dialog ----

function CreditDialog({ mode, customerId, customerName, customers, open, onOpenChange, onDone }: {
  mode: 'grant' | 'deduct'
  customerId: string
  customerName: string
  customers?: Customer[]
  open: boolean
  onOpenChange: (open: boolean) => void
  onDone: () => void
}) {
  const [selectedCustomer, setSelectedCustomer] = useState(customerId)
  const [customerError, setCustomerError] = useState('')
  const [showConfirm, setShowConfirm] = useState(false)

  const isDeduct = mode === 'deduct'

  const form = useForm<CreditFormData>({
    resolver: zodResolver(creditSchema),
    defaultValues: { amount: '', description: '', expiresAt: '', promotional: false },
  })

  // Credit-expiry floor reads the selected customer's simulated now when
  // clock-pinned (same class as CustomerDetail's expiry validation): a wall-clock
  // floor would block valid sim-future dates or admit sim-past ones. Re-resolves
  // as the dialog's customer selection changes.
  // `customers` above is scoped to customers who ALREADY have a balance
  // (the page's table), but the dialog can grant to ANY customer — so on a
  // FIRST-EVER grant the lookup missed and the floor silently fell back to
  // wall-clock, exactly the failure this guard exists to prevent (found
  // walking FLOW C1 2026-07-29: a customer pinned to a Sep-2027 clock
  // offered a Jul-2026 floor, letting the operator pick a sim-PAST date the
  // API then rejected with "must be in the future" — for a date that IS in
  // the future by the wall calendar the picker was drawn from).
  //
  // Fall back to the combobox's own by-id fetch, sharing its EXACT query key
  // so React Query serves it from cache — no extra request, and the picker
  // is guaranteed to have resolved the same row it is displaying.
  const selectedInBalances = customers?.find(c => c.id === selectedCustomer)
  const { data: pickedCustomerData } = useQuery({
    queryKey: ['customers-picker-selected', selectedCustomer],
    queryFn: () => api.listCustomers(`ids=${selectedCustomer}&limit=1`),
    enabled: !!selectedCustomer && !selectedInBalances,
    staleTime: 60_000,
  })
  const activeCustomer = selectedInBalances ?? pickedCustomerData?.data?.[0]
  const now = useEffectiveNow(activeCustomer?.test_clock_id)

  // One idempotency key per dialog OPEN. Retries on transient failure
  // (network, 5xx) replay through the API's Idempotency middleware and
  // converge on the same ledger entry — no double-grant / double-deduct.
  // The key resets on dialog close (component unmounts) so a fresh open
  // can submit a new entry against the same customer.
  const [idemKey] = useState(() => crypto.randomUUID())

  const effectiveCustomerId = selectedCustomer || customerId
  // The confirm step names the customer; with the server-searched picker
  // there's no local list to look it up in, so fetch the picked one.
  const { data: pickedData } = useQuery({
    queryKey: ['credit-dialog-picked', effectiveCustomerId],
    queryFn: () => api.listCustomers(`ids=${effectiveCustomerId}&limit=1`),
    enabled: !!effectiveCustomerId && !customerName,
    staleTime: 30_000,
  })
  const effectiveCustomerName =
    customers?.find(c => c.id === effectiveCustomerId)?.display_name ||
    pickedData?.data?.[0]?.display_name ||
    customerName

  const saveMutation = useMutation({
    mutationFn: async () => {
      const amountCents = Math.round(parseFloat(form.getValues('amount')) * 100)
      const description = form.getValues('description')
      const expiresAt = form.getValues('expiresAt')
      if (isDeduct) {
        return api.adjustCredits({
          customer_id: effectiveCustomerId,
          amount_cents: -amountCents,
          description,
        }, idemKey)
      } else {
        return api.grantCredits({
          customer_id: effectiveCustomerId,
          amount_cents: amountCents,
          description,
          ...(expiresAt ? { expires_at: endOfDayInTZ(expiresAt) } : {}),
          ...(form.getValues('promotional') ? { grant_kind: 'promotional' as const } : {}),
        }, idemKey)
      }
    },
    onSuccess: () => {
      form.reset()
      onDone()
    },
    onError: (err) => {
      applyApiError(form, err, {
        amount_cents: 'amount',
        description: 'description',
        expires_at: 'expiresAt',
      })
    },
  })

  const onFormSubmit = form.handleSubmit(() => {
    if (!effectiveCustomerId) { setCustomerError('Select a customer'); return }
    setCustomerError('')
    setShowConfirm(true)
  })

  const handleConfirm = () => {
    setShowConfirm(false)
    saveMutation.mutate()
  }

  const confirmAmount = parseFloat(form.watch('amount') || '0')

  return (
    <>
      <Dialog open={open} onOpenChange={(o) => {
        onOpenChange(o)
        if (!o) { form.reset(); setCustomerError('') }
      }}>
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>{isDeduct ? 'Deduct Credits' : 'Grant Credits'}</DialogTitle>
          </DialogHeader>
          <Form {...form}>
            <form onSubmit={onFormSubmit} noValidate className="space-y-4">
              {!customerId && (
                <div>
                  <Label className="text-sm font-medium">Customer</Label>
                  {/* Server-searched (P11): the old select listed only the
                      first 50 customers — credits ungrantable to the rest. */}
                  <div className="mt-2">
                    <CustomerCombobox
                      value={selectedCustomer}
                      onChange={(v) => { setSelectedCustomer(v); setCustomerError('') }}
                      placeholder="Search customers..."
                    />
                  </div>
                  {customerError && <p className="text-destructive text-sm mt-1">{customerError}</p>}
                </div>
              )}
              {customerId && (
                <div className="bg-muted rounded-lg px-4 py-3">
                  <p className="text-sm text-muted-foreground">Customer</p>
                  <p className="text-sm font-medium text-foreground mt-0.5">{effectiveCustomerName}</p>
                </div>
              )}

              <FormField
                control={form.control}
                name="amount"
                render={({ field }) => (
                  <FormItem>
                    <FormLabel>Amount ({getCurrencySymbol()})</FormLabel>
                    <FormControl>
                      <Input type="number" step="0.01" min="0.01" max={999999.99} placeholder="50.00" {...field} />
                    </FormControl>
                    <FormDescription>
                      {isDeduct ? "Removed from the customer's prepaid balance" : "Added to the customer's prepaid balance"}
                    </FormDescription>
                    <FormMessage />
                  </FormItem>
                )}
              />

              <FormField
                control={form.control}
                name="description"
                render={({ field }) => (
                  <FormItem>
                    <FormLabel>Description</FormLabel>
                    <FormControl>
                      <Input
                        placeholder={isDeduct ? 'e.g. Billing correction, clawback' : 'e.g. Welcome credit, compensation'}
                        maxLength={500}
                        {...field}
                      />
                    </FormControl>
                    <FormMessage />
                  </FormItem>
                )}
              />

              {!isDeduct && (
                <FormField
                  control={form.control}
                  name="expiresAt"
                  render={({ field }) => (
                    <FormItem>
                      <FormLabel>Expires At</FormLabel>
                      <DatePicker
                        value={field.value}
                        onChange={field.onChange}
                        placeholder="Never expires"
                        minDate={new Date(now)}
                      />
                      <FormDescription>Leave empty for credits that never expire. Past dates are blocked — a credit that expires before today would be unusable.</FormDescription>
                    </FormItem>
                  )}
                />
              )}

              {!isDeduct && (
                <FormField
                  control={form.control}
                  name="promotional"
                  render={({ field }) => (
                    <FormItem className="flex flex-row items-start gap-3 space-y-0 rounded-md border border-border p-3">
                      <FormControl>
                        <Checkbox checked={field.value} onCheckedChange={field.onChange} />
                      </FormControl>
                      <div className="space-y-1 leading-none">
                        <FormLabel className="font-normal">Promotional credits</FormLabel>
                        <FormDescription>
                          Free / marketing credits. They are used up before any purchased or
                          refunded credits. Leave off for paid-for or goodwill balances.
                        </FormDescription>
                      </div>
                    </FormItem>
                  )}
                />
              )}

              <DialogFooter>
                <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>
                  Cancel
                </Button>
                <Button type="submit" disabled={saveMutation.isPending} variant={isDeduct ? 'destructive' : 'default'}>
                  {saveMutation.isPending ? (
                    <>
                      <Loader2 size={14} className="animate-spin mr-2" />
                      Processing...
                    </>
                  ) : isDeduct ? 'Deduct Credits' : 'Grant Credits'}
                </Button>
              </DialogFooter>
            </form>
          </Form>
        </DialogContent>
      </Dialog>

      <AlertDialog open={showConfirm} onOpenChange={(open) => { if (!open) setShowConfirm(false) }}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{isDeduct ? 'Confirm Deduction' : 'Confirm Credit Grant'}</AlertDialogTitle>
            <AlertDialogDescription>
              {isDeduct ? 'Deduct' : 'Grant'} ${confirmAmount.toFixed(2)} {isDeduct ? 'from' : 'to'} {effectiveCustomerName}?
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              onClick={handleConfirm}
              className={cn(isDeduct && 'bg-destructive text-destructive-foreground hover:bg-destructive/90')}
            >
              {isDeduct ? 'Deduct' : 'Grant Credits'}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  )
}
