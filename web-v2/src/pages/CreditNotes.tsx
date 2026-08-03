import { useState, useMemo } from 'react'
import { usePageTitle } from '@/hooks/usePageTitle'
import { Link } from 'react-router-dom'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { useForm } from 'react-hook-form'
import { zodResolver } from '@hookform/resolvers/zod'
import { z } from 'zod'
import { toast } from 'sonner'
import { invalidateMoneySurfaces } from '@/lib/invalidateMoney'
import { api, downloadCreditNotePDF, formatCents, formatDate, formatDateTime, getCurrencySymbol } from '@/lib/api'
import type { CreditNote, Invoice, Customer } from '@/lib/api'
import { applyApiError, showApiError } from '@/lib/formErrors'
import { downloadCSV } from '@/lib/csv'
import { Layout } from '@/components/Layout'
import { SimulatedBadge } from '@/components/TestClockBadge'
import { useSortable, type SortDir } from '@/hooks/useSortable'
import { useUrlState } from '@/hooks/useUrlState'
import { cn } from '@/lib/utils'
import { statusBadgeVariant, statusBorderColor, creditNoteReasonLabel } from '@/lib/status'

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
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from '@/components/ui/alert-dialog'
import { TypedConfirmDialog } from '@/components/TypedConfirmDialog'
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

import { Plus, Search, Download, Loader2, ArrowUpDown, ArrowUp, ArrowDown, FileMinus, Mail } from 'lucide-react'
import { EmptyState } from '@/components/EmptyState'

const creditNoteSchema = z.object({
  invoice_id: z.string().min(1, 'Invoice is required'),
  reason: z.string().min(1, 'Reason is required'),
  amount: z.string().min(1, 'Amount is required').refine(v => parseFloat(v) >= 0.01, 'Must be at least 0.01'),
  refundType: z.string(),
})

type CreditNoteFormData = z.infer<typeof creditNoteSchema>

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

export default function CreditNotesPage() {
  usePageTitle('Credit notes')
  const [showCreate, setShowCreate] = useState(false)
  const [confirmIssue, setConfirmIssue] = useState<string | null>(null)
  const [confirmVoid, setConfirmVoid] = useState<string | null>(null)
  const [sendTarget, setSendTarget] = useState<CreditNote | null>(null)
  const [urlState, setUrlState] = useUrlState({
    search: '',
    status: '',
    refund_status: '',
    page: '1',
    sort: 'created_at',
    dir: 'desc',
  })
  const { search, status: filterStatus, refund_status: refundStatusFilter, sort: sortKey } = urlState
  const sortDir = urlState.dir as SortDir
  const page = Math.max(1, parseInt(urlState.page) || 1)
  const queryClient = useQueryClient()

  // Server-side sort: pass sort/dir; backend tie-breaks on id.
  const notesQueryParams = useMemo(() => {
    const params = new URLSearchParams()
    // Explicit page-size ceiling (server clamps at 100; the implicit
    // default was 25, silently truncating the list under the client-side
    // pagination). Search here spans joined customer/invoice fields, so
    // rows stay client-paginated; the notice below surfaces truncation.
    params.set('limit', '100')
    if (filterStatus) params.set('status', filterStatus)
    if (refundStatusFilter) params.set('refund_status', refundStatusFilter)
    if (sortKey) params.set('sort', sortKey)
    if (sortDir) params.set('dir', sortDir)
    return params.toString()
  }, [filterStatus, refundStatusFilter, sortKey, sortDir])
  const { data: notesData, isLoading: notesLoading, error: notesErrorObj, refetch: refetchNotes } = useQuery({
    queryKey: ['credit-notes', filterStatus, refundStatusFilter, sortKey, sortDir],
    queryFn: () => api.listCreditNotes(notesQueryParams),
  })

  // Fetch only the customers + invoices referenced by visible credit
  // notes (avoids the "Unknown" / truncated-ID pagination bug — see
  // Invoices.tsx for full rationale). Both lookups re-fetch when the
  // visible credit-note page changes.
  const refIds = useMemo(() => {
    const cSet = new Set<string>()
    const iSet = new Set<string>()
    ;(notesData?.data ?? []).forEach(n => {
      if (n.customer_id) cSet.add(n.customer_id)
      if (n.invoice_id) iSet.add(n.invoice_id)
    })
    return { customers: Array.from(cSet), invoices: Array.from(iSet) }
  }, [notesData])
  const { data: invoicesData } = useQuery({
    queryKey: ['invoices-by-ids-for-cn', refIds.invoices],
    queryFn: () => api.listInvoices(refIds.invoices.length > 0 ? `ids=${refIds.invoices.join(',')}&limit=${refIds.invoices.length}` : ''),
    enabled: refIds.invoices.length > 0,
  })

  const { data: customersData } = useQuery({
    queryKey: ['customers-by-ids-for-cn', refIds.customers],
    queryFn: () => api.listCustomers(refIds.customers.length > 0 ? `ids=${refIds.customers.join(',')}&limit=${refIds.customers.length}` : ''),
    enabled: refIds.customers.length > 0,
  })

  const notes = notesData?.data ?? []
  const loading = notesLoading
  const loadError = notesErrorObj instanceof Error ? notesErrorObj.message : notesErrorObj ? String(notesErrorObj) : null

  const invoiceMap = useMemo(() => {
    const iMap: Record<string, Invoice> = {}
    ;(invoicesData?.data ?? []).forEach(inv => { iMap[inv.id] = inv })
    return iMap
  }, [invoicesData])

  const customerMap = useMemo(() => {
    const cMap: Record<string, Customer> = {}
    ;(customersData?.data ?? []).forEach(c => { cMap[c.id] = c })
    return cMap
  }, [customersData])

  const issueMutation = useMutation({
    mutationFn: (id: string) => api.issueCreditNote(id),
    onSuccess: () => {
      invalidateMoneySurfaces(queryClient)
      toast.success('Credit note issued')
    },
    onError: (err) => showApiError(err, 'Failed to issue'),
  })

  const voidMutation = useMutation({
    mutationFn: (id: string) => api.voidCreditNote(id),
    onSuccess: () => {
      invalidateMoneySurfaces(queryClient)
      toast.success('Credit note voided')
    },
    onError: (err) => showApiError(err, 'Failed to void'),
  })

  const retryRefundMutation = useMutation({
    mutationFn: (id: string) => api.retryCreditNoteRefund(id),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['credit-notes'] })
      toast.success('Refund retried successfully')
    },
    onError: (err) => showApiError(err, 'Failed to retry refund'),
  })

  // Stats
  const stats = useMemo(() => {
    const draft = notes.filter(n => n.status === 'draft').length
    const issued = notes.filter(n => n.status === 'issued')
    const voided = notes.filter(n => n.status === 'voided').length
    const totalCredited = issued.reduce((sum, n) => sum + n.credit_amount_cents, 0)
    const totalRefunded = issued.reduce((sum, n) => sum + n.refund_amount_cents, 0)
    const totalOutOfBand = issued.reduce((sum, n) => sum + (n.out_of_band_amount_cents ?? 0), 0)
    const totalAmount = issued.reduce((sum, n) => sum + n.total_cents, 0)
    return { draft, issued: issued.length, voided, totalCredited, totalRefunded, totalOutOfBand, totalAmount }
  }, [notes])

  // Filter + search
  const filtered = useMemo(() => notes.filter(n => {
    if (filterStatus && n.status !== filterStatus) return false
    if (search) {
      const q = search.toLowerCase()
      const custName = customerMap[n.customer_id]?.display_name?.toLowerCase() || ''
      const invNum = invoiceMap[n.invoice_id]?.invoice_number?.toLowerCase() || ''
      if (!n.credit_note_number.toLowerCase().includes(q) && !custName.includes(q) && !invNum.includes(q) && !n.reason.toLowerCase().includes(q)) return false
    }
    return true
  }), [notes, filterStatus, search, customerMap, invoiceMap])

  // Server-side sort end-to-end. Client-side re-sort would only sort
  // the current page, breaking pagination semantics.
  const { onSort } = useSortable(
    filtered,
    sortKey,
    sortDir,
    (key, dir) => setUrlState({ sort: key, dir }),
  )
  const sorted = filtered

  const truncated = notes.length >= 100
  const totalPages = Math.ceil(sorted.length / PAGE_SIZE)
  const currentPage = Math.min(page, totalPages || 1)
  const paginated = sorted.slice((currentPage - 1) * PAGE_SIZE, currentPage * PAGE_SIZE)

  const handleExport = () => {
    const rows = filtered.map(n => [
      n.credit_note_number,
      customerMap[n.customer_id]?.display_name || '',
      invoiceMap[n.invoice_id]?.invoice_number || '',
      n.status,
      (n.refund_amount_cents / 100).toFixed(2),
      (n.credit_amount_cents / 100).toFixed(2),
      ((n.out_of_band_amount_cents ?? 0) / 100).toFixed(2),
      n.reason,
      (n.total_cents / 100).toFixed(2),
      n.issued_at || n.created_at,
    ])
    downloadCSV('credit-notes.csv', ['Number', 'Customer', 'Invoice', 'Status', 'Refund', 'Credit', 'Out of band', 'Reason', 'Amount', 'Date'], rows)
  }

  return (
    <Layout>
      {/* Header */}
      <div className="flex justify-between items-center">
        <div>
          <h1 className="text-2xl font-semibold text-foreground">Credit Notes</h1>
          <p className="text-sm text-muted-foreground mt-1">Issue refunds and adjustments on invoices{notes.length > 0 ? ` · ${notes.length} total` : ''}</p>
        </div>
        <div className="flex items-center gap-2">
          {notes.length > 0 && (
            <Button variant="outline" size="sm" onClick={handleExport}>
              <Download size={16} className="mr-2" /> Export
            </Button>
          )}
          <Button size="sm" onClick={() => setShowCreate(true)}>
            <Plus size={16} className="mr-2" /> Issue Credit Note
          </Button>
        </div>
      </div>

      {/* Summary cards */}
      {!loading && notes.length > 0 && (
        <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-4 gap-4 mt-6">
          <Card>
            <CardContent className="px-5 py-4">
              <p className="text-xs font-medium text-muted-foreground">Total Credited</p>
              <p className="text-xl font-semibold text-foreground mt-1 tabular-nums">{formatCents(stats.totalAmount)}</p>
            </CardContent>
          </Card>
          <Card>
            <CardContent className="px-5 py-4">
              <p className="text-xs font-medium text-muted-foreground">Refunded to Card</p>
              <p className="text-xl font-semibold text-primary mt-1 tabular-nums">{formatCents(stats.totalRefunded)}</p>
            </CardContent>
          </Card>
          <Card>
            <CardContent className="px-5 py-4">
              <p className="text-xs font-medium text-muted-foreground">Applied to Balance</p>
              <p className="text-xl font-semibold text-emerald-600 mt-1 tabular-nums">{formatCents(stats.totalCredited)}</p>
            </CardContent>
          </Card>
          <Card>
            <CardContent className="px-5 py-4">
              <p className="text-xs font-medium text-muted-foreground">Issued</p>
              <p className="text-xl font-semibold text-foreground mt-1">{stats.issued}</p>
              {stats.draft > 0 && <p className="text-xs text-amber-600 mt-0.5">{stats.draft} draft</p>}
            </CardContent>
          </Card>
        </div>
      )}

      {/* Filter tabs + search */}
      {(notes.length > 0 || filterStatus) && (
        <div className="flex items-center gap-3 mt-6">
          <div className="flex gap-1 bg-muted rounded-lg p-1">
            {[
              { value: '', label: 'All', count: notes.length },
              { value: 'draft', label: 'Draft', count: stats.draft },
              { value: 'issued', label: 'Issued', count: stats.issued },
              { value: 'voided', label: 'Voided', count: stats.voided },
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
                {f.count > 0 && <span className="ml-1 text-muted-foreground/60">{f.count}</span>}
              </button>
            ))}
          </div>
          <div className="relative flex-1">
            <Search size={16} className="absolute left-3 top-2.5 text-muted-foreground" />
            <Input
              value={search}
              onChange={e => setUrlState({ search: e.target.value, page: '1' })}
              placeholder="Search by number, customer, invoice, or reason..."
              className="pl-9"
            />
          </div>
        </div>
      )}

      {refundStatusFilter === 'needs_attention' && (
        <div className="mt-3 flex items-center gap-2 text-xs">
          <span className="inline-flex items-center gap-1.5 rounded-full bg-destructive/10 px-2.5 py-1 font-medium text-destructive">
            Showing refunds needing attention (failed, or pending &gt; 72h)
            <button
              onClick={() => setUrlState({ refund_status: '', page: '1' })}
              className="hover:text-foreground"
              aria-label="Clear refund filter"
            >
              ✕
            </button>
          </span>
        </div>
      )}

      {/* Table */}
      <Card className="mt-4">
        <CardContent className="p-0">
          {loadError ? (
            <div className="p-8 text-center">
              <p className="text-sm text-destructive mb-3">{loadError}</p>
              <Button variant="outline" size="sm" onClick={() => refetchNotes()}>
                Retry
              </Button>
            </div>
          ) : loading ? (
            <TableSkeleton columns={7} />
          ) : notes.length === 0 ? (
            filterStatus ? (
              <EmptyState
                title={`No ${filterStatus} credit notes`}
                description="Try a different filter to see more results."
                action={{
                  label: 'Clear filter',
                  variant: 'outline',
                  onClick: () => setUrlState({ status: '', page: '1' }),
                }}
              />
            ) : (
              <EmptyState
                icon={FileMinus}
                title="No credit notes yet"
                description="Issue a credit note to apply credits or refunds against invoices."
                action={{
                  label: 'Issue Credit Note',
                  icon: Plus,
                  onClick: () => setShowCreate(true),
                }}
              />
            )
          ) : filtered.length === 0 ? (
            <p className="px-6 py-8 text-sm text-muted-foreground text-center">
              No credit notes match your search
            </p>
          ) : (
            <>
              <Table>
                <TableHeader>
                  <TableRow>
                    <SortableHead label="Number" sortKey="credit_note_number" activeSortKey={sortKey} sortDir={sortDir} onSort={onSort} />
                    <TableHead className="text-xs font-medium">Customer</TableHead>
                    <TableHead className="text-xs font-medium">Invoice</TableHead>
                    <TableHead className="text-xs font-medium">Reason</TableHead>
                    <SortableHead label="Status" sortKey="status" activeSortKey={sortKey} sortDir={sortDir} onSort={onSort} />
                    <SortableHead label="Amount" sortKey="total_cents" activeSortKey={sortKey} sortDir={sortDir} onSort={onSort} className="text-right" />
                    <SortableHead label="Date" sortKey="created_at" activeSortKey={sortKey} sortDir={sortDir} onSort={onSort} />
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {paginated.map((note: CreditNote) => {
                    const isRefund = note.refund_amount_cents > 0
                    const channels: string[] = []
                    if (note.refund_amount_cents > 0) channels.push('refund')
                    if (note.credit_amount_cents > 0) channels.push('credit')
                    if ((note.out_of_band_amount_cents ?? 0) > 0) channels.push('out of band')
                    // All three channels zero = an adjustment that reduced the
                    // invoice's amount_due (the unpaid-downgrade path), not a refund/credit.
                    const channelLabel = channels.length === 0 ? 'applied to invoice' : channels.join(' + ')
                    return (
                      <TableRow key={note.id} className={cn('border-l-[3px]', statusBorderColor(note.status))}>
                        <TableCell>
                          <span className="inline-flex items-center gap-2">
                            <p className="text-sm font-medium text-foreground">{note.credit_note_number}</p>
                            {note.is_simulated && <SimulatedBadge title="Dates on this credit note follow its test clock's calendar, not today's date" />}
                          </span>
                          <p className="text-xs text-muted-foreground mt-0.5">{channelLabel}</p>
                        </TableCell>
                        <TableCell className="text-sm">
                          <Link to={`/customers/${note.customer_id}`} className="text-primary hover:underline">
                            {customerMap[note.customer_id]?.display_name || 'Unknown'}
                          </Link>
                        </TableCell>
                        <TableCell className="text-sm">
                          <Link to={`/invoices/${note.invoice_id}`} className="text-primary hover:underline font-mono">
                            {invoiceMap[note.invoice_id]?.invoice_number || note.invoice_id.slice(0, 12) + '...'}
                          </Link>
                        </TableCell>
                        {/* Tooltip must show what the CELL shows. `note.reason` is the raw
                            enum code for system-issued notes — the cell renders
                            "Quantity decrease" while the hover said
                            "subscription_quantity_decrease". For operator free text
                            the two are identical, so nothing is lost either way. */}
                        <TableCell className="text-sm text-muted-foreground max-w-[220px] truncate" title={creditNoteReasonLabel(note.reason)}>
                          {creditNoteReasonLabel(note.reason)}
                        </TableCell>
                        <TableCell>
                          <div className="flex items-center gap-1.5">
                            <Badge variant={statusBadgeVariant(note.status)}>{note.status}</Badge>
                            {isRefund && note.status === 'issued' && note.refund_status && note.refund_status !== 'none' && (
                              <Badge variant={note.refund_status === 'succeeded' ? 'success' : note.refund_status === 'failed' ? 'danger' : 'warning'}>
                                {note.refund_status === 'succeeded' ? 'refunded' : note.refund_status === 'failed' ? 'refund failed' : 'refund pending'}
                              </Badge>
                            )}
                          </div>
                        </TableCell>
                        <TableCell className="text-right tabular-nums font-mono text-sm">
                          {formatCents(note.total_cents)}
                        </TableCell>
                        <TableCell>
                          <div className="flex items-center justify-between">
                            <span className="text-sm text-muted-foreground whitespace-nowrap">
                              {note.issued_at ? formatDate(note.issued_at) : formatDateTime(note.created_at)}
                            </span>
                            {note.status === 'draft' && (
                              <div className="flex items-center gap-1 ml-3">
                                <Button variant="outline" size="sm" onClick={() => setConfirmIssue(note.id)}>
                                  Issue
                                </Button>
                                <Button variant="outline" size="sm" className="text-destructive hover:text-destructive" onClick={() => setConfirmVoid(note.id)}>
                                  Void
                                </Button>
                              </div>
                            )}
                            {note.status === 'issued' && (
                              <div className="flex items-center gap-1 ml-3">
                                {(note.refund_status === 'failed' || note.refund_status === 'pending') && (
                                  <Button
                                    variant="outline"
                                    size="sm"
                                    onClick={() => retryRefundMutation.mutate(note.id)}
                                    disabled={retryRefundMutation.isPending}
                                    title={note.refund_status === 'failed' ? 'Retry Stripe refund' : 'Process Stripe refund'}
                                  >
                                    Retry refund
                                  </Button>
                                )}
                                <Button
                                  variant="outline"
                                  size="sm"
                                  onClick={async () => {
                                    try {
                                      await downloadCreditNotePDF(note.id, note.credit_note_number)
                                    } catch (err) {
                                      showApiError(err, 'Failed to download PDF')
                                    }
                                  }}
                                >
                                  <Download size={14} className="mr-1.5" /> PDF
                                </Button>
                                <Button variant="outline" size="sm" onClick={() => setSendTarget(note)}>
                                  <Mail size={14} className="mr-1.5" /> Send
                                </Button>
                              </div>
                            )}
                          </div>
                        </TableCell>
                      </TableRow>
                    )
                  })}
                </TableBody>
              </Table>

              {truncated && (
                <div className="border-t border-border px-4 py-2 text-xs text-muted-foreground">
                  Showing the 100 most recent credit notes. Use the status filter to narrow older ones.
                </div>
              )}

              {/* Pagination */}
              {totalPages > 1 && (
                <div className="border-t border-border px-4 py-3 flex items-center justify-between">
                  <p className="text-xs text-muted-foreground">
                    Showing {(currentPage - 1) * PAGE_SIZE + 1}
                    {'\u2013'}
                    {Math.min(currentPage * PAGE_SIZE, sorted.length)} of {sorted.length}
                  </p>
                  <Pagination>
                    <PaginationContent>
                      <PaginationItem>
                        <PaginationPrevious
                          onClick={() => setUrlState({ page: String(Math.max(1, currentPage - 1)) })}
                          className={cn(currentPage <= 1 && 'pointer-events-none opacity-50')}
                        />
                      </PaginationItem>
                      {Array.from({ length: Math.min(totalPages, 5) }, (_, i) => {
                        let pageNum: number
                        if (totalPages <= 5) {
                          pageNum = i + 1
                        } else if (currentPage <= 3) {
                          pageNum = i + 1
                        } else if (currentPage >= totalPages - 2) {
                          pageNum = totalPages - 4 + i
                        } else {
                          pageNum = currentPage - 2 + i
                        }
                        return (
                          <PaginationItem key={pageNum}>
                            <PaginationLink
                              onClick={() => setUrlState({ page: String(pageNum) })}
                              isActive={currentPage === pageNum}
                            >
                              {pageNum}
                            </PaginationLink>
                          </PaginationItem>
                        )
                      })}
                      <PaginationItem>
                        <PaginationNext
                          onClick={() => setUrlState({ page: String(Math.min(totalPages, currentPage + 1)) })}
                          className={cn(currentPage >= totalPages && 'pointer-events-none opacity-50')}
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

      {/* Create Credit Note Dialog */}
      <CreateCreditNoteDialog
        open={showCreate}
        onOpenChange={setShowCreate}
        customerMap={customerMap}
        onCreated={() => {
          setShowCreate(false)
          invalidateMoneySurfaces(queryClient)
          toast.success('Credit note issued')
        }}
      />

      {/* Issue Confirm */}
      <AlertDialog open={confirmIssue !== null} onOpenChange={(open) => { if (!open) setConfirmIssue(null) }}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Issue Credit Note</AlertDialogTitle>
            <AlertDialogDescription>
              Issue this credit note? This will apply the credit or initiate the refund. Once issued, it cannot be reversed.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction onClick={() => { if (confirmIssue) issueMutation.mutate(confirmIssue); setConfirmIssue(null) }}>
              Issue Credit Note
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      {/* Void Confirm */}
      <TypedConfirmDialog
        open={confirmVoid !== null}
        onOpenChange={(open) => { if (!open) setConfirmVoid(null) }}
        title="Void Credit Note"
        description="Voiding this credit note reverses its effect permanently. This action cannot be undone."
        confirmWord="VOID"
        confirmLabel="Void Credit Note"
        onConfirm={() => { if (confirmVoid) voidMutation.mutate(confirmVoid); setConfirmVoid(null) }}
        loading={voidMutation.isPending}
      />

      {/* Send credit-note email */}
      {sendTarget && (
        <SendCreditNoteDialog
          note={sendTarget}
          onClose={() => setSendTarget(null)}
          onSent={() => { setSendTarget(null); toast.success('Credit note email sent') }}
        />
      )}
    </Layout>
  )
}

// --- Send Credit Note Email Dialog (ADR-082) ---

function SendCreditNoteDialog({ note, onClose, onSent }: {
  note: CreditNote; onClose: () => void; onSent: () => void
}) {
  // Fetch the full customer for the recipient + CC prefill — the list
  // rows the page already holds don't carry additional_emails.
  const { data: cust } = useQuery({
    queryKey: ['customer', note.customer_id],
    queryFn: () => api.getCustomer(note.customer_id),
  })
  const [email, setEmail] = useState('')
  const [cc, setCc] = useState('')
  const [touched, setTouched] = useState(false)
  const [sending, setSending] = useState(false)
  const [error, setError] = useState('')

  // Prefill once the customer loads (unless the operator already typed).
  if (cust && !touched && email === '' && (cust.email || (cust.additional_emails || []).length > 0)) {
    setEmail(cust.email || '')
    setCc((cust.additional_emails || []).join(', '))
  }

  const handleSend = async () => {
    if (!email.trim() || sending) return
    setSending(true)
    setError('')
    try {
      const ccList = cc.split(',').map(s => s.trim()).filter(Boolean)
      await api.sendCreditNoteEmail(note.id, email.trim(), ccList)
      onSent()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to send')
    } finally {
      setSending(false)
    }
  }

  return (
    <Dialog open onOpenChange={(open) => { if (!open) onClose() }}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Email Credit Note {note.credit_note_number}</DialogTitle>
          <DialogDescription>Sends the credit note PDF to your customer.</DialogDescription>
        </DialogHeader>
        <div className="space-y-4">
          <div className="space-y-2">
            <label htmlFor="cn-email" className="text-sm font-medium">Recipient Email</label>
            <Input id="cn-email" type="email" value={email}
              onChange={e => { setEmail(e.target.value); setTouched(true) }}
              placeholder="customer@example.com" />
          </div>
          <div className="space-y-2">
            <label htmlFor="cn-cc" className="text-sm font-medium">CC</label>
            <Input id="cn-cc" value={cc}
              onChange={e => { setCc(e.target.value); setTouched(true) }}
              placeholder="finance@customer.com, ap@customer.com" />
            <p className="text-xs text-muted-foreground">Separate with commas. Clear the field to email only the recipient.</p>
          </div>
          {error && <p className="text-sm text-destructive">{error}</p>}
          <DialogFooter>
            <Button type="button" variant="outline" onClick={onClose}>Cancel</Button>
            <Button onClick={handleSend} disabled={sending || !email.trim()}>
              {sending ? <><Loader2 size={14} className="animate-spin mr-2" />Sending...</> : 'Send Email'}
            </Button>
          </DialogFooter>
        </div>
      </DialogContent>
    </Dialog>
  )
}

// --- Create Credit Note Dialog ---

function CreateCreditNoteDialog({ open, onOpenChange, customerMap, onCreated }: {
  open: boolean
  onOpenChange: (open: boolean) => void
  customerMap: Record<string, Customer>
  onCreated: () => void
}) {
  // Invoices for the dropdown — combined finalized + paid lists.
  // Two parallel queries gated on dialog `open` so we lazy-fetch and
  // share the cache with the parent CreditNotes page (which uses the
  // same backend). React Query handles the StrictMode dedupe.
  const finalizedQuery = useQuery({
    queryKey: ['cn-dialog', 'invoices', 'finalized'],
    queryFn: () => api.listInvoices('status=finalized'),
    enabled: open,
  })
  const paidQuery = useQuery({
    queryKey: ['cn-dialog', 'invoices', 'paid'],
    queryFn: () => api.listInvoices('status=paid'),
    enabled: open,
  })
  const invoices: Invoice[] = [...(finalizedQuery.data?.data ?? []), ...(paidQuery.data?.data ?? [])]
  const loadingInvoices = (finalizedQuery.isLoading || paidQuery.isLoading) && open

  const form = useForm<CreditNoteFormData>({
    resolver: zodResolver(creditNoteSchema),
    defaultValues: { invoice_id: '', reason: '', amount: '', refundType: 'credit' },
  })

  const invoiceId = form.watch('invoice_id')
  const reason = form.watch('reason')
  const amount = form.watch('amount')
  const refundType = form.watch('refundType')


  const selectedInvoice = invoices.find(inv => inv.id === invoiceId)
  const isPaid = selectedInvoice?.status === 'paid'
  const maxAmount = selectedInvoice
    ? isPaid ? selectedInvoice.amount_paid_cents : selectedInvoice.amount_due_cents
    : 0

  const createMutation = useMutation({
    mutationFn: async (data: CreditNoteFormData) => {
      const amountCents = Math.round(parseFloat(data.amount) * 100)
      return api.createCreditNote({
        invoice_id: data.invoice_id,
        reason: data.reason,
        refund_type: data.refundType,
        auto_issue: true,
        lines: [{ description: data.reason, quantity: 1, unit_amount_cents: amountCents }],
      })
    },
    onSuccess: () => {
      form.reset()
      onCreated()
    },
    onError: (err) => {
      applyApiError(form, err, {
        invoice_id: 'invoice_id',
        reason: 'reason',
        lines: 'amount',
        unit_amount_cents: 'amount',
        quantity: 'amount',
      })
    },
  })

  const onSubmit = form.handleSubmit((data) => {
    createMutation.mutate(data)
  })

  return (
    <Dialog open={open} onOpenChange={(o) => {
      onOpenChange(o)
      if (!o) form.reset()
    }}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>Issue Credit Note</DialogTitle>
          <DialogDescription>
            Create a credit note against an invoice.
          </DialogDescription>
        </DialogHeader>

        {loadingInvoices ? (
          <div className="py-8 flex justify-center">
            <Loader2 className="h-6 w-6 animate-spin text-muted-foreground" />
          </div>
        ) : (
          <Form {...form}>
            <form onSubmit={onSubmit} noValidate className="space-y-4">
              <FormField
                control={form.control}
                name="invoice_id"
                render={({ field }) => (
                  <FormItem>
                    <FormLabel>Invoice</FormLabel>
                    <FormControl>
                      <select
                        value={field.value}
                        onChange={(e) => {
                          field.onChange(e.target.value)
                          const inv = invoices.find(i => i.id === e.target.value)
                          if (inv?.status !== 'paid') form.setValue('refundType', 'credit')
                        }}
                        className="flex h-9 w-full rounded-md border border-input bg-transparent px-3 py-1 text-sm shadow-xs transition-colors focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring"
                      >
                        <option value="">Select invoice...</option>
                        {invoices.map(inv => {
                          const custName = customerMap[inv.customer_id]?.display_name || ''
                          return (
                            <option key={inv.id} value={inv.id}>
                              {inv.invoice_number} - {custName} ({formatCents(inv.total_amount_cents)})
                            </option>
                          )
                        })}
                      </select>
                    </FormControl>
                    <FormMessage />
                  </FormItem>
                )}
              />

              {selectedInvoice && (
                <div className="bg-muted rounded-lg px-4 py-3">
                  <div className="grid grid-cols-3 gap-4 text-sm">
                    <div>
                      <p className="text-muted-foreground">Customer</p>
                      <p className="font-medium text-foreground mt-0.5">{customerMap[selectedInvoice.customer_id]?.display_name || 'Unknown'}</p>
                    </div>
                    <div>
                      <p className="text-muted-foreground">{isPaid ? 'Amount Paid' : 'Amount Due'}</p>
                      <p className="font-medium text-foreground mt-0.5">{formatCents(maxAmount)}</p>
                    </div>
                    <div>
                      <p className="text-muted-foreground">Invoice Status</p>
                      <div className="mt-0.5">
                        <Badge variant={statusBadgeVariant(selectedInvoice.status)}>{selectedInvoice.status}</Badge>
                      </div>
                    </div>
                  </div>
                </div>
              )}

              <div>
                <FormLabel>Reason</FormLabel>
                <div className="flex flex-wrap gap-1.5 my-2">
                  {['Service disruption', 'Billing error', 'Goodwill credit', 'Feature downgrade', 'Contract adjustment'].map(r => (
                    <button
                      key={r}
                      type="button"
                      onClick={() => form.setValue('reason', r, { shouldValidate: true })}
                      className={cn(
                        'px-2.5 py-1 rounded-md text-xs font-medium transition-colors',
                        reason === r
                          ? 'bg-primary/10 text-primary ring-1 ring-primary/30'
                          : 'bg-muted text-muted-foreground hover:bg-muted/80'
                      )}
                    >
                      {r}
                    </button>
                  ))}
                </div>
                <FormField
                  control={form.control}
                  name="reason"
                  render={({ field }) => (
                    <FormItem>
                      <FormControl>
                        <Input placeholder="Or type a custom reason..." maxLength={500} {...field} />
                      </FormControl>
                      <FormMessage />
                    </FormItem>
                  )}
                />
              </div>

              <div className="grid grid-cols-2 gap-4">
                <FormField
                  control={form.control}
                  name="amount"
                  render={({ field }) => (
                    <FormItem>
                      <FormLabel>Amount ({getCurrencySymbol()})</FormLabel>
                      <FormControl>
                        <Input
                          type="number"
                          step="0.01"
                          min="0.01"
                          max={maxAmount ? (maxAmount / 100).toFixed(2) : '999999.99'}
                          placeholder="25.00"
                          {...field}
                        />
                      </FormControl>
                      {maxAmount > 0 && <FormDescription>Max: {formatCents(maxAmount)}</FormDescription>}
                      <FormMessage />
                    </FormItem>
                  )}
                />
                {isPaid ? (
                  <FormField
                    control={form.control}
                    name="refundType"
                    render={({ field }) => (
                      <FormItem>
                        <FormLabel>Credit Type</FormLabel>
                        <FormControl>
                          <select
                            value={field.value}
                            onChange={field.onChange}
                            className="flex h-9 w-full rounded-md border border-input bg-transparent px-3 py-1 text-sm shadow-xs transition-colors focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring"
                          >
                            <option value="credit">Credit to balance</option>
                            <option value="refund">Refund to payment method</option>
                          </select>
                        </FormControl>
                      </FormItem>
                    )}
                  />
                ) : (
                  <div className="pt-8">
                    <p className="text-sm text-muted-foreground">Reduces amount due on invoice</p>
                  </div>
                )}
              </div>

              {amount && selectedInvoice && (
                <div className="bg-blue-50 dark:bg-blue-950/30 border border-blue-100 dark:border-blue-900 rounded-lg px-4 py-3 text-sm">
                  <p className="text-blue-800 dark:text-blue-200">
                    {isPaid && refundType === 'refund'
                      ? `${getCurrencySymbol()}${parseFloat(amount || '0').toFixed(2)} will be refunded to the customer's payment method`
                      : isPaid
                        ? `${getCurrencySymbol()}${parseFloat(amount || '0').toFixed(2)} will be added to the customer's credit balance`
                        : `Invoice amount due will be reduced by ${getCurrencySymbol()}${parseFloat(amount || '0').toFixed(2)}`
                    }
                  </p>
                </div>
              )}

              <DialogFooter>
                <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>
                  Cancel
                </Button>
                <Button type="submit" disabled={createMutation.isPending}>
                  {createMutation.isPending ? (
                    <>
                      <Loader2 size={14} className="animate-spin mr-2" />
                      Issuing...
                    </>
                  ) : (
                    'Issue Credit Note'
                  )}
                </Button>
              </DialogFooter>
            </form>
          </Form>
        )}
      </DialogContent>
    </Dialog>
  )
}
