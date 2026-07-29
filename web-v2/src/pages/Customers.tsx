import { useState, useMemo } from 'react'
import { usePageTitle } from '@/hooks/usePageTitle'
import { Link, useNavigate } from 'react-router-dom'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { useForm } from 'react-hook-form'
import { zodResolver } from '@hookform/resolvers/zod'
import { z } from 'zod'
import { toast } from 'sonner'
import { api, formatDate } from '@/lib/api'
import type { Customer } from '@/lib/api'
import { applyApiError } from '@/lib/formErrors'
import { TestClockBadge } from '@/components/TestClockBadge'
import { useAuth } from '@/contexts/AuthContext'
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from '@/components/ui/select'
import { downloadServerCSV } from '@/lib/csv'
import { Layout } from '@/components/Layout'
import { useSortable, type SortDir } from '@/hooks/useSortable'
import { useUrlState } from '@/hooks/useUrlState'
import { useDebouncedValue } from '@/hooks/useDebouncedValue'
import { cn } from '@/lib/utils'
import { statusBadgeVariant, statusBorderColor } from '@/lib/status'
import { InitialsAvatar } from '@/components/InitialsAvatar'

import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Badge } from '@/components/ui/badge'
import { Card, CardContent } from '@/components/ui/card'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
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

import { Plus, Search, Download, Loader2, ArrowUpDown, ArrowUp, ArrowDown, Users } from 'lucide-react'
import { EmptyState } from '@/components/EmptyState'

const createCustomerSchema = z.object({
  external_id: z.string().min(1, 'External ID is required').regex(/^[a-zA-Z0-9_-]+$/, 'Only letters, numbers, hyphens, and underscores allowed'),
  display_name: z.string().min(1, 'Display name is required'),
  email: z.string().email('Invalid email address').optional().or(z.literal('')),
  // Test-clock pin: customer-level attach (ADR-027, Stripe parity).
  // Test-mode only; the field is hidden in live mode. Once set,
  // every Subscription / Invoice for this customer runs on the
  // clock's simulated time and the value cannot be changed.
  test_clock_id: z.string().optional(),
})

type CreateCustomerData = z.infer<typeof createCustomerSchema>

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

export default function CustomersPage() {
  usePageTitle('Customers')
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

  const form = useForm<CreateCustomerData>({
    resolver: zodResolver(createCustomerSchema),
    defaultValues: { external_id: '', display_name: '', email: '', test_clock_id: '' },
  })

  // Test-clock pin (ADR-027): the field is gated on test mode +
  // clocks existing — same shape as the old sub-create gating.
  // useAuth gives us the dashboard's current mode; useQuery for
  // clocks reuses the same key as other surfaces so the cache is
  // shared.
  const { user } = useAuth()
  const isTestMode = user?.livemode === false
  const { data: clocksData } = useQuery({
    queryKey: ['test-clocks'],
    queryFn: () => api.listTestClocks(),
    enabled: isTestMode,
  })
  const clocks = clocksData?.data ?? []

  // Debounced server-side search: matches name / email / external ID
  // (the backend decrypts and matches the encrypted PII columns) — not
  // just the visible page, though the backend caps the scan at its
  // first 5000 rows in sort order.
  const debouncedSearch = useDebouncedValue(search.trim(), 300)

  // Server-side paginated fetch
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

  const { data: customersData, isLoading: loading, error: loadErrorObj, refetch } = useQuery({
    queryKey: ['customers', queryParams],
    queryFn: () => api.listCustomers(queryParams),
    placeholderData: (prev) => prev,
  })

  const customers = customersData?.data ?? []
  const total = customersData?.total ?? 0
  const loadError = loadErrorObj instanceof Error ? loadErrorObj.message : loadErrorObj ? String(loadErrorObj) : null

  const createMutation = useMutation({
    mutationFn: (data: CreateCustomerData) => api.createCustomer(data),
    onSuccess: (created) => {
      queryClient.invalidateQueries({ queryKey: ['customers'] })
      toast.success(`Customer "${created.display_name}" created`)
      setShowCreate(false)
      form.reset()
    },
    onError: (err) => {
      applyApiError(form, err, ['external_id', 'display_name', 'email', 'test_clock_id'])
    },
  })

  // Search is server-side (search= query param) — rows arriving here
  // are already filtered across the full dataset, not just the page.
  //
  // Server-side sort end-to-end. Client-side re-sort would only reorder
  // the current page, breaking pagination semantics.
  const { onSort } = useSortable(
    customers,
    sortKey,
    sortDir,
    (key, dir) => setUrlState({ sort: key, dir }),
  )
  const sorted = customers

  const totalPages = Math.ceil(total / PAGE_SIZE)

  const onSubmit = form.handleSubmit((data: CreateCustomerData) => {
    createMutation.mutate(data)
  })

  // Export uses the server-side streaming endpoint so the full
  // tenant dataset is dumped (not just the page currently in view).
  // Client-side row-build approach previously here only exported
  // the visible page — wrong for "I take my data" use case.
  const handleExport = async () => {
    try {
      await downloadServerCSV('/v1/exports/customers.csv', 'customers.csv')
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
          <h1 className="text-2xl font-semibold text-foreground">Customers</h1>
          <p className="text-sm text-muted-foreground mt-1">Manage customers and billing profiles{total > 0 ? ` · ${total} total` : ''}</p>
        </div>
        <div className="flex items-center gap-2">
          {total > 0 && (
            <Button variant="outline" size="sm" onClick={handleExport}>
              <Download size={16} className="mr-2" />
              Export CSV
            </Button>
          )}
          <Button size="sm" onClick={() => setShowCreate(true)}>
            <Plus size={16} className="mr-2" />
            Add Customer
          </Button>
        </div>
      </div>

      {/* Filters — search is server-side across the full dataset. The
          row stays visible while a search is active so a zero-result
          query never hides its own input. */}
      {(total > 0 || filterStatus || search) && (
        <div className="flex items-center gap-3 mt-6">
          <div className="flex gap-1 bg-muted rounded-lg p-1">
            {[
              { value: '', label: 'All' },
              { value: 'active', label: 'Active' },
              { value: 'archived', label: 'Archived' },
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
          <div className="relative flex-1">
            <Search size={16} className="absolute left-3 top-2.5 text-muted-foreground" />
            <Input
              value={search}
              onChange={e => setUrlState({ search: e.target.value, page: '1' })}
              placeholder="Search by name, email, or ID..."
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
                    ? `No customers match “${debouncedSearch}”`
                    : `No ${filterStatus} customers`
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
                icon={Users}
                title="No customers yet"
                description="Add your first customer to start billing."
                action={{
                  label: 'Add Customer',
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
                    <SortableHead label="Email" sortKey="email" activeSortKey={sortKey} sortDir={sortDir} onSort={onSort} />
                    <SortableHead label="Status" sortKey="status" activeSortKey={sortKey} sortDir={sortDir} onSort={onSort} />
                    <SortableHead label="Created" sortKey="created_at" activeSortKey={sortKey} sortDir={sortDir} onSort={onSort} />
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {sorted.map((c: Customer) => (
                    <TableRow
                      key={c.id}
                      className={cn('cursor-pointer hover:bg-muted/50 transition-colors border-l-[3px]', statusBorderColor(c.status))}
                      onClick={(e) => {
                        const target = e.target as HTMLElement
                        if (target.closest('button, a, input, select')) return
                        navigate(`/customers/${c.id}`)
                      }}
                    >
                      <TableCell>
                        <div className="flex items-center gap-2.5">
                          <InitialsAvatar name={c.display_name} />
                          <div className="min-w-0">
                            <div className="flex items-center gap-1.5">
                              <Link
                                to={`/customers/${c.id}`}
                                className="text-sm font-medium text-foreground hover:text-primary transition-colors truncate block max-w-[200px]"
                                title={c.display_name}
                              >
                                {c.display_name}
                              </Link>
                              {/* Customer-level test-clock badge (ADR-027).
                                  Lets operators scan which customers are
                                  in simulation without drilling into each
                                  detail page. Mirrors Stripe's customer
                                  list. */}
                              {c.test_clock_id && <TestClockBadge testClockId={c.test_clock_id} />}
                            </div>
                            <p className="text-xs text-muted-foreground font-mono truncate max-w-[220px]" title={c.external_id}>{c.external_id}</p>
                          </div>
                        </div>
                      </TableCell>
                      <TableCell className="text-sm text-muted-foreground">
                        {c.email || '\u2014'}
                      </TableCell>
                      <TableCell>
                        <Badge variant={statusBadgeVariant(c.status)}>{c.status}</Badge>
                      </TableCell>
                      <TableCell className="text-sm text-muted-foreground">
                        {formatDate(c.created_at)}
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

      {/* Create Customer Dialog */}
      <Dialog open={showCreate} onOpenChange={(open) => {
        setShowCreate(open)
        if (!open) { form.reset() }
      }}>
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>Create Customer</DialogTitle>
            <DialogDescription>
              Add a new customer to start billing. The external ID is your unique identifier.
            </DialogDescription>
          </DialogHeader>
          <Form {...form}>
            <form onSubmit={onSubmit} noValidate className="space-y-4">
              <FormField
                control={form.control}
                name="display_name"
                render={({ field }) => (
                  <FormItem>
                    <FormLabel>Display Name</FormLabel>
                    <FormControl>
                      <Input placeholder="Acme Corporation" maxLength={255} {...field} />
                    </FormControl>
                    <FormMessage />
                  </FormItem>
                )}
              />
              <FormField
                control={form.control}
                name="external_id"
                render={({ field }) => (
                  <FormItem>
                    <FormLabel>External ID</FormLabel>
                    <FormControl>
                      <Input placeholder="acme_corp" maxLength={100} className="font-mono" {...field} />
                    </FormControl>
                    <FormDescription>
                      Only letters, numbers, hyphens, and underscores
                    </FormDescription>
                    <FormMessage />
                  </FormItem>
                )}
              />
              <FormField
                control={form.control}
                name="email"
                render={({ field }) => (
                  <FormItem>
                    <FormLabel>Email</FormLabel>
                    <FormControl>
                      <Input type="email" placeholder="billing@acme.com" maxLength={254} {...field} />
                    </FormControl>
                    <FormMessage />
                  </FormItem>
                )}
              />
              {/* Customer-level test-clock attach (ADR-027, Stripe
                  parity). Test mode + clocks-exist gate matches the
                  pre-ADR-027 sub-create form. Once set, every
                  Subscription and Invoice for this customer simulates
                  on the clock's frozen time. Cannot be changed
                  post-creation — to switch clocks, recreate the
                  customer. */}
              {isTestMode && clocks.length > 0 && (
                <FormField
                  control={form.control}
                  name="test_clock_id"
                  render={({ field }) => (
                    <FormItem>
                      <FormLabel>Pin to test clock <span className="text-muted-foreground">(optional)</span></FormLabel>
                      <Select value={field.value ?? ''} onValueChange={(v) => field.onChange(v ?? '')}>
                        <SelectTrigger className="w-full">
                          <SelectValue placeholder="No test clock — bills in real time">
                            {(value: string) => {
                              if (!value) return 'No test clock — bills in real time'
                              const c = clocks.find(c => c.id === value)
                              return c ? (c.name || c.id) : value
                            }}
                          </SelectValue>
                        </SelectTrigger>
                        <SelectContent>
                          <SelectItem value="">No test clock — bills in real time</SelectItem>
                          {clocks.map(c => (
                            <SelectItem key={c.id} value={c.id}>
                              {c.name || c.id}
                            </SelectItem>
                          ))}
                        </SelectContent>
                      </Select>
                      <FormDescription>
                        Pinned customers run all subscriptions and invoices on the clock's simulated time. Cannot be changed later.
                      </FormDescription>
                      <FormMessage />
                    </FormItem>
                  )}
                />
              )}

              <DialogFooter>
                <Button type="button" variant="outline" onClick={() => setShowCreate(false)}>
                  Cancel
                </Button>
                <Button type="submit" disabled={createMutation.isPending}>
                  {createMutation.isPending ? (
                    <>
                      <Loader2 size={14} className="animate-spin mr-2" />
                      Creating...
                    </>
                  ) : (
                    'Create Customer'
                  )}
                </Button>
              </DialogFooter>
            </form>
          </Form>
        </DialogContent>
      </Dialog>
    </Layout>
  )
}
