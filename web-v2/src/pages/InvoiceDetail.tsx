import { useState, useEffect } from 'react'
import { usePageTitle } from '@/hooks/usePageTitle'
import { useParams, useNavigate, Link } from 'react-router-dom'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { useForm } from 'react-hook-form'
import { zodResolver } from '@hookform/resolvers/zod'
import { z } from 'zod'
import { toast } from 'sonner'
import { invalidateMoneySurfaces } from '@/lib/invalidateMoney'
import { api, downloadPDF, formatCents, formatRate, formatDate, formatDateTime, formatTaxRate, getCurrencySymbol, pollIntervalForInvoice, type TenantSettings, type DunningRun, type TimelineEvent, type Invoice as ApiInvoice, type CreditNote } from '@/lib/api'
import { formatCivilPeriod } from '@/lib/dates'
import { InvoiceAttention } from '@/components/InvoiceAttention'
import { TestClockBanner } from '@/components/TestClockBanner'
import { useGetInvoice } from '@/lib/gen/queries.gen'
import type { Invoice } from '@/lib/gen/schemas/invoice'
import type { InvoiceLineItem as LineItem } from '@/lib/gen/schemas/invoiceLineItem'
import { applyApiError, showApiError } from '@/lib/formErrors'
import { taxReasonLabel } from '@/lib/taxReasons'
import { DueBadge } from '@/components/DueBadge'
import { effectiveNow } from '@/lib/effectiveNow'
import { Layout } from '@/components/Layout'
import { cn } from '@/lib/utils'
import { statusBadgeVariant, creditNoteReasonLabel, paymentIsUnresolved } from '@/lib/status'

import { Button } from '@/components/ui/button'
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip'
import { Badge } from '@/components/ui/badge'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Separator } from '@/components/ui/separator'
import {
  Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle,
} from '@/components/ui/dialog'
import { TypedConfirmDialog } from '@/components/TypedConfirmDialog'
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from '@/components/ui/table'
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from '@/components/ui/select'

import { Loader2, Mail, CreditCard, Link2, RotateCw, Info, MoreHorizontal, Download, Eye, XCircle, Receipt, AlertOctagon, History } from 'lucide-react'
import {
  DropdownMenu, DropdownMenuContent, DropdownMenuItem,
  DropdownMenuSeparator, DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { CopyButton } from '@/components/CopyButton'
import { DetailBreadcrumb } from '@/components/DetailBreadcrumb'
import { DetailSkeleton } from '@/components/ui/DetailSkeleton'

const statusVariant = statusBadgeVariant

// TaxReversedLine renders the "↳ Tax reversed $X (Stripe Tax)" sub-fact for a
// credit note, shared by the pre-payment and post-payment settlement blocks so
// the disclosure can't drift (pre-fix it lived only in the post-payment block,
// so unpaid-invoice adjustment CNs reversed tax with no on-screen disclosure).
function TaxReversedLine({ cn, currency }: { cn: CreditNote; currency: string }) {
  const taxReversed = cn.tax_amount_cents ?? 0
  if (taxReversed <= 0) return null
  const taxTxID = cn.tax_transaction_id
  return (
    <div
      className="text-[11px] mt-0.5 leading-snug pl-3 text-muted-foreground/80"
      title={taxTxID ? `Stripe Tax reversal: ${taxTxID}` : undefined}
    >
      {taxTxID
        ? `↳ Tax reversed ${formatCents(taxReversed, currency)} (Stripe Tax)`
        : `↳ Tax: ${formatCents(taxReversed, currency)} (no upstream provider)`}
    </div>
  )
}

const LINE_TYPE_LABELS: Record<string, string> = {
  base_fee: 'Base Fee',
  usage: 'Usage',
  add_on: 'Add-On',
  discount: 'Discount',
  tax: 'Tax',
}

function formatCompanyAddressLines(s?: TenantSettings | null): string[] {
  if (!s) return []
  const lines: string[] = []
  if (s.company_address_line1) lines.push(s.company_address_line1)
  if (s.company_address_line2) lines.push(s.company_address_line2)
  const cityStatePostal = [s.company_city, s.company_state].filter(Boolean).join(', ')
    + (s.company_postal_code ? ` ${s.company_postal_code}` : '')
  if (cityStatePostal.trim()) lines.push(cityStatePostal.trim())
  if (s.company_country) lines.push(s.company_country)
  return lines
}

function formatLineType(raw: string): string {
  return LINE_TYPE_LABELS[raw] || raw.replace(/_/g, ' ').replace(/\b\w/g, c => c.toUpperCase())
}

// humaniseInvoiceStatus maps the persisted enum to operator-facing
// copy. Server-side codes (draft/finalized/paid/voided/uncollectible)
// are stable; UI owns its own labels so wording changes don't require
// a server roll. Keep matches Stripe Dashboard's column copy.
function humaniseInvoiceStatus(raw: string): string {
  const map: Record<string, string> = {
    draft: 'Draft',
    finalized: 'Open',
    paid: 'Paid',
    voided: 'Voided',
    uncollectible: 'Uncollectible',
  }
  return map[raw] ?? raw
}

// aggregateTaxByJurisdiction rolls up per-line-item tax amounts into a single
// row per jurisdiction, preserving insertion order so the UI is deterministic
// when two jurisdictions have identical contributions. Mirrors the PDF
// breakdown so invoice totals match between web view and PDF.
function aggregateTaxByJurisdiction(items: LineItem[]): { label: string; amount: number }[] {
  const order: string[] = []
  const totals = new Map<string, number>()
  for (const item of items) {
    const amount = item.tax_amount_cents || 0
    // Skip only zero-tax lines; INCLUDE negative-tax lines (a two-line upgrade
    // proration's credit line reverses tax on the unused old slice — ADR-048
    // Phase C). Mirrors the PDF's `== 0` skip so the per-jurisdiction rows sum
    // to invoice.tax_amount_cents on both surfaces.
    if (amount === 0) continue
    const label = item.tax_jurisdiction || ''
    if (!label) continue
    if (!totals.has(label)) order.push(label)
    totals.set(label, (totals.get(label) || 0) + amount)
  }
  return order.map(label => ({ label, amount: totals.get(label) || 0 }))
}

const emailSchema = z.object({
  email: z.string().min(1, 'Email is required').email('Invalid email address'),
  // Comma-separated CC list (ADR-082). Cleared field = primary only;
  // server validates each address and caps at 10.
  additional_emails: z.string(),
})
type EmailFormData = z.infer<typeof emailSchema>

// Operator-readable names for the timeline's degraded-lane codes.
const LANE_LABELS: Record<string, string> = {
  credit_notes: 'credit notes',
  emails: 'customer notifications',
  stripe: 'payment events',
  dunning: 'payment recovery',
  audit: 'action history',
}
const laneLabel = (lane: string): string => LANE_LABELS[lane] ?? lane

export default function InvoiceDetailPage() {
  const { id } = useParams<{ id: string }>()
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const [showVoidConfirm, setShowVoidConfirm] = useState(false)
  const [showRotatePublicTokenConfirm, setShowRotatePublicTokenConfirm] = useState(false)
  const [showEmailModal, setShowEmailModal] = useState(false)
  const [showCreditModal, setShowCreditModal] = useState(false)
  const [showAddLineItem, setShowAddLineItem] = useState(false)
  const [pdfPreviewUrl, setPdfPreviewUrl] = useState<string | null>(null)

  useEffect(() => {
    return () => { if (pdfPreviewUrl) URL.revokeObjectURL(pdfPreviewUrl) }
  }, [pdfPreviewUrl])

  // Escape closes the PDF preview overlay (it is a hand-rolled modal, not
  // a base-ui Dialog, so it gets no focus/keyboard handling for free).
  useEffect(() => {
    if (!pdfPreviewUrl) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') { URL.revokeObjectURL(pdfPreviewUrl); setPdfPreviewUrl(null) }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [pdfPreviewUrl])

  // Generated react-query hook from api/openapi.yaml. The hook key is
  // `getGetInvoiceQueryKey(id)` (also exported), but we keep the
  // existing `['invoice', id]` shape for invalidations elsewhere in
  // this file. Both work — react-query keys are content-addressed.
  //
  // refetchInterval polls based on transient invoice state — see
  // pollIntervalForInvoice. Operator stays on this page after clicking
  // Pay/Retry; webhook arrives → next tick refetches → UI flips
  // "Processing" → "Paid" without manual refresh. Returns false on
  // terminal states so paid/voided invoices don't keep polling
  // forever.
  const { data: invoiceData, isLoading, error: loadError, refetch } = useGetInvoice(id!, {
    query: {
      queryKey: ['invoice', id],
      refetchInterval: (query) => pollIntervalForInvoice(query.state.data?.invoice as ApiInvoice | undefined),
    },
  })

  const invoice = invoiceData?.invoice
  const lineItems: LineItem[] = invoiceData?.line_items ?? []

  const { data: customer } = useQuery({
    queryKey: ['customer', invoice?.customer_id],
    queryFn: () => api.getCustomer(invoice!.customer_id),
    enabled: !!invoice?.customer_id,
  })

  const { data: subscription } = useQuery({
    queryKey: ['subscription', invoice?.subscription_id],
    queryFn: () => api.getSubscription(invoice!.subscription_id),
    enabled: !!invoice?.subscription_id,
  })

  // Resolve the test clock this invoice's dates live on. A subscription
  // invoice inherits the sub's pin; a MANUAL one-off invoice has no
  // subscription, so fall back to the customer's pin (the customer is
  // already loaded above, so this is no extra fetch). invoice.is_simulated
  // is the authoritative "these dates are simulated" flag; the resolved id
  // just tells the banner which clock's "currently at" time to show.
  const resolvedTestClockId = subscription?.test_clock_id || customer?.test_clock_id || ''

  // Test-clock frozen_time becomes the "now" for relative-time badges
  // (Due in N days) on this page. Without this the badge reads from
  // wall-clock and understates urgency on simulated invoices — "Due in
  // 45d" while the engine perceives 15d. Keyed off the resolved id so it
  // works for manual simulated invoices too (was sub-only, which made the
  // DueBadge count against wall-clock while the page claimed simulated).
  const { data: testClock } = useQuery({
    queryKey: ['test-clock', resolvedTestClockId],
    queryFn: () => api.getTestClock(resolvedTestClockId),
    enabled: !!resolvedTestClockId,
  })

  const { data: creditNotesData } = useQuery({
    queryKey: ['invoice-credit-notes', id],
    queryFn: async () => {
      const cn = await api.listCreditNotes(`invoice_id=${id}`)
      return (cn.data || []).filter(n => n.status === 'issued')
    },
    enabled: !!id,
  })
  const creditNotes = creditNotesData ?? []

  // Timeline shares the invoice's polling cadence — both surfaces
  // change in lock-step (webhook lands → invoice state flips +
  // timeline gains a row). Polling the invoice without polling the
  // timeline would surface "Paid" in the header while the activity
  // log still shows the older state.
  const { data: timelineData } = useQuery({
    queryKey: ['invoice-timeline', id],
    queryFn: () => api.getPaymentTimeline(id!),
    enabled: !!id && invoice?.status !== 'draft',
    refetchInterval: pollIntervalForInvoice(invoice as ApiInvoice | undefined),
  })
  const timeline = timelineData?.events ?? []
  // Lane-degradation disclosure: the server names any side-source
  // (credit notes, emails, payment events, payment recovery) whose
  // fetch failed — without the banner, a timeline missing its dunning
  // rows is indistinguishable from an invoice that never saw dunning.
  const timelineDegraded = timelineData?.degraded ?? []
  const timelineTruncated = timelineData?.truncated ?? false
  // Two time-domains must NOT be interleaved in one chronological list:
  // billing rows ride the customer's (possibly simulated) timeline, while
  // wall-clock external events (notification emails; Stripe payment-processor
  // outcomes) happen in real time. Sorting them together makes a real-time row
  // sort by its wall-clock instant and land out of place among simulated
  // billing rows — e.g. a "receipt sent" or "payment failed" at today's date
  // before a finalize at the simulated future cycle date.
  //
  // Routing:
  //   - email rows → always the external lane (a notification log reads
  //     cleanly on its own; matches Stripe's "email log" placement).
  //   - stripe rows → external lane ONLY when the invoice is simulated, where
  //     their wall-clock occurred_at would mis-sort. On a normal (one-clock)
  //     invoice they stay in Activity, where operators expect payment outcomes
  //     in the billing story. (Most stripe rows are already folded into the
  //     simulated paid/dunning rows upstream; only standalone failed/canceled
  //     events without a dunning twin reach here.)
  //   - credit_note rows → external lane ONLY when the CN itself is
  //     wall-clock (operator-issued via HTTP). Engine clawbacks
  //     (downgrade/cancel proration) issue under the clock-pinned sub's
  //     bound time → e.is_simulated → they belong in Activity, sorted with
  //     the other simulated rows. Routing them externally showed a
  //     simulated timestamp in the wall-clock lane (the bug this fixes).
  //   - charge-attempt rows (ADR-102) → routed by their OWN axis: a
  //     sim-stamped attempt is a billing fact and belongs in Activity;
  //     a wall-stamped attempt on a simulated invoice (interactive
  //     checkout, webhook-resolved) joins the wall-clock lane.
  //
  // The second lane exists for ONE reason: to stop two time domains from
  // interleaving. A wall-clock invoice has exactly one clock, so there is
  // nothing to separate — emails sort into Activity with everything else
  // and the operator reads one chronological story instead of checking two
  // cards. Splitting there would separate CONTENT TYPES, a distinction the
  // page never claimed to make, and it made the lane caption promise
  // payment events that structurally never appear on a live invoice
  // (2026-07-28 review). So: the whole external lane is gated on
  // is_simulated.
  // ONE lane (ADR-104). Every row's primary timestamp is on the
  // invoice's own calendar — simulated when clock-pinned, since every
  // fact is anchored at write time (outbox sim anchor for emails,
  // ADR-102 dual stamps for attempts, entity-derived stamps for the
  // rest). Rows whose two calendars differ carry recorded_at and render
  // a "Recorded <wall>" subline (operator contract, Invariant A) — the
  // second "Real-time activity" card this replaced was a residue of
  // emails lacking a billing anchor, not a design.
  const billingTimeline = timeline

  // Payment-method snapshot serves two purposes on this page:
  //   1. The success-state card on paid invoices (brand •••• last4).
  //   2. The Collect Payment button's disabled state — when no PM is
  //      ready, charging will fail, so the button gates on
  //      paymentSetup.setup_status === 'ready'. Mirrors the disable-
  //      with-tooltip pattern Velox uses for Finalize-on-tax-failure.
  //
  // Fetched whenever the invoice exists (and isn't a draft) so the
  // button has data before render. v1 caveat on the success card: uses
  // the customer's CURRENT default method, which may differ from what
  // was actually charged (deferred — needs a schema migration to
  // snapshot brand+last4 onto the invoice at payment time).
  // Canonical PM check post-cleanup: list payment_methods for the
  // customer and look for a default-flagged row. Replaces the legacy
  // setup_status='ready' check that polled the dropped
  // customer_payment_setups summary table.
  const { data: paymentMethodsList } = useQuery({
    queryKey: ['customer-payment-methods', invoice?.customer_id],
    queryFn: () => api.listCustomerPaymentMethods(invoice!.customer_id),
    enabled: !!invoice?.customer_id && invoice?.status !== 'draft',
  })
  const hasPaymentMethod = (paymentMethodsList?.data ?? []).some(pm => pm.is_default)

  // Dunning runs + operator-context (diagnosis/resolution) card. Shown
  // only when the invoice is in an actionable mid-flight state — the
  // operator can still influence outcome by retrying tax, attaching a
  // PM, or charging. Hidden on:
  //   - draft (no PaymentIntent exists; diagnosis is a category error)
  //   - paid (resolved)
  //   - voided (operator cancelled — no further collection context)
  //   - uncollectible (operator wrote off as bad debt — Stripe parity
  //     says further diagnosis/resolution framing contradicts the
  //     terminal decision and confuses the page)
  // Tax pending/failed always shows (independent of status — tax can
  // block finalize on draft, or appear on a finalized invoice).
  const showOperatorContext =
    invoice?.tax_status === 'pending' ||
    invoice?.tax_status === 'failed' ||
    (invoice?.status === 'finalized' &&
      invoice?.payment_status !== 'succeeded' &&
      invoice?.payment_status !== 'processing')
  const { data: dunningRunsData } = useQuery({
    queryKey: ['invoice-dunning-runs', id],
    queryFn: () => api.listDunningRuns(`invoice_id=${id}`).then(r => r.data || []),
    enabled: !!id && !!showOperatorContext,
  })
  // Active run when one exists; otherwise the ESCALATED run — hiding the
  // run the moment recovery gives up made the Diagnostic row vanish at
  // exactly the point the operator most needs to see it (2026-07-22
  // audit, P1-4). Resolved runs stay hidden: the invoice settled.
  const activeDunningRun =
    (dunningRunsData ?? []).find(r => r.state === 'active') ??
    (dunningRunsData ?? []).find(r => r.state === 'escalated')

  const { data: settings } = useQuery({
    queryKey: ['settings'],
    queryFn: () => api.getSettings(),
  })

  const invalidateAll = () => {
    queryClient.invalidateQueries({ queryKey: ['invoice', id] })
    queryClient.invalidateQueries({ queryKey: ['invoice-credit-notes', id] })
    queryClient.invalidateQueries({ queryKey: ['invoice-timeline', id] })
    queryClient.invalidateQueries({ queryKey: ['invoices'] })
    // Cross-page caches that surface this invoice. Dashboard recent
    // invoices renders status/payment_status/amount for the most
    // recent rows; CustomerDetail's overview returns recent_invoices
    // for the same set; SubscriptionDetail's invoices tab is keyed
    // per-sub. Without these, finalizing or voiding here left the
    // dashboard / customer / sub pages on stale state.
    queryClient.invalidateQueries({ queryKey: ['dashboard-recent-invoices'] })
    queryClient.invalidateQueries({ queryKey: ['dashboard-overview'] })
    if (invoice?.customer_id) {
      queryClient.invalidateQueries({ queryKey: ['customer-overview', invoice.customer_id] })
    }
    if (invoice?.subscription_id) {
      queryClient.invalidateQueries({ queryKey: ['subscription-invoices', invoice.subscription_id] })
    }
    // Credits issued from this page land on the customer's balance —
    // refresh every balance-bearing family too (2026-07-21 audit).
    invalidateMoneySurfaces(queryClient, { customerId: invoice?.customer_id, invoiceId: id })
  }

  const finalizeMutation = useMutation({
    mutationFn: () => api.finalizeInvoice(id!),
    onSuccess: () => { invalidateAll(); toast.success(`Invoice ${invoice?.invoice_number} finalized`) },
    onError: (err) => showApiError(err, 'Failed to finalize'),
  })

  const voidMutation = useMutation({
    mutationFn: () => api.voidInvoice(id!),
    onSuccess: () => { invalidateAll(); setShowVoidConfirm(false); toast.success(`Invoice ${invoice?.invoice_number} voided`) },
    onError: (err) => showApiError(err, 'Failed to void'),
  })

  const collectMutation = useMutation({
    mutationFn: () => api.collectPayment(id!),
    onSuccess: () => { invalidateAll(); toast.success('Payment initiated') },
    onError: (err) => showApiError(err, 'Payment failed'),
  })

  // No-payment-method nudge: re-send the SAME setup-link email the engine sent
  // at finalize (Stripe Checkout in setup mode → add a card → engine
  // auto-charges). Goes straight to the customer's email — no recipient modal,
  // unlike the hosted-invoice "pay" email behind setShowEmailModal.
  const resendSetupLinkMutation = useMutation({
    mutationFn: () => api.resendSetupLink(id!),
    onSuccess: () => toast.success('Setup link resent to the customer'),
    onError: (err) => showApiError(err, 'Failed to resend setup link'),
  })

  // Stripe-parity operator actions on finalized / uncollectible invoices.
  // markUncollectibleMutation halts dunning + flips status to bad debt.
  // recordPaymentMutation accepts an operator-recorded out-of-band payment
  // (cheque, wire, cash) — works on both finalized-unpaid AND uncollectible
  // (the Stripe "we wrote it off but they paid after all" recovery path).
  const [showMarkUncollectibleConfirm, setShowMarkUncollectibleConfirm] = useState(false)
  const [showRecordPaymentDialog, setShowRecordPaymentDialog] = useState(false)
  const [recordPaymentNote, setRecordPaymentNote] = useState('')
  const markUncollectibleMutation = useMutation({
    mutationFn: () => api.markInvoiceUncollectible(id!),
    onSuccess: () => {
      invalidateAll()
      setShowMarkUncollectibleConfirm(false)
      toast.success(`Invoice ${invoice?.invoice_number} marked uncollectible`)
    },
    onError: (err) => showApiError(err, 'Failed to mark uncollectible'),
  })
  const recordPaymentMutation = useMutation({
    mutationFn: () => api.recordOfflineInvoicePayment(id!, { note: recordPaymentNote.trim() || undefined }),
    onSuccess: () => {
      invalidateAll()
      setShowRecordPaymentDialog(false)
      setRecordPaymentNote('')
      toast.success(`Payment recorded — invoice ${invoice?.invoice_number} marked paid`)
    },
    onError: (err) => showApiError(err, 'Failed to record payment'),
  })

  const rotatePublicTokenMutation = useMutation({
    mutationFn: () => api.rotateInvoicePublicToken(id!),
    onSuccess: () => {
      invalidateAll()
      setShowRotatePublicTokenConfirm(false)
      toast.success('Public link rotated — previous URL no longer works')
    },
    onError: err => showApiError(err, 'Failed to rotate public link'),
  })

  const retryTaxMutation = useMutation({
    mutationFn: () => api.retryInvoiceTax(id!),
    onSuccess: (updated) => {
      invalidateAll()
      if (updated.tax_status === 'ok') {
        toast.success('Tax recalculated successfully')
      } else {
        toast.message('Tax retry attempted', {
          description: 'Still pending — see the attention card for the latest reason.',
        })
      }
    },
    onError: err => showApiError(err, 'Failed to retry tax'),
  })

  const acting = finalizeMutation.isPending || voidMutation.isPending || collectMutation.isPending || rotatePublicTokenMutation.isPending

  // Build the shareable hosted-invoice URL for clipboard copy. Uses the
  // dashboard origin — deployments that serve the dashboard and the
  // hosted page on separate domains should set VITE_HOSTED_INVOICE_BASE_URL.
  const hostedInvoiceBase = import.meta.env.VITE_HOSTED_INVOICE_BASE_URL || window.location.origin
  const publicInvoiceURL = invoice?.public_token ? `${hostedInvoiceBase}/invoice/${invoice.public_token}` : ''

  const copyPublicLink = async () => {
    if (!publicInvoiceURL) return
    try {
      await navigator.clipboard.writeText(publicInvoiceURL)
      toast.success('Public link copied to clipboard')
    } catch {
      toast.error('Failed to copy — you can select the URL manually')
    }
  }

  const loading = isLoading
  const error = loadError instanceof Error ? loadError.message : loadError ? String(loadError) : null

  usePageTitle(invoice?.invoice_number)

  if (loading) {
    return (
      <Layout>
        <DetailSkeleton to="/invoices" parentLabel="Invoices" />
      </Layout>
    )
  }

  if (error) {
    return (
      <Layout>
        <div className="py-16 text-center">
          <p className="text-sm text-destructive mb-3">{error}</p>
          <Button variant="outline" size="sm" onClick={() => refetch()}>Retry</Button>
        </div>
      </Layout>
    )
  }

  if (!invoice) {
    return (
      <Layout>
        <p className="text-sm text-muted-foreground py-16 text-center">Invoice not found</p>
      </Layout>
    )
  }

  return (
    <Layout>
      <DetailBreadcrumb to="/invoices" parentLabel="Invoices" currentLabel={invoice.invoice_number} />

      {/* Header */}
      <div className="flex items-center justify-between mb-6">
        <div>
          <div className="flex items-center gap-2">
            <h1 className="text-2xl font-semibold text-foreground">{invoice.invoice_number}</h1>
          </div>
          <div className="flex items-center gap-1.5 mt-1">
            <span className="text-xs font-mono text-muted-foreground">{invoice.id}</span>
            <CopyButton text={invoice.id} />
          </div>
          {/* Draft hint — orients operators on the next step. Drafts have
              no PaymentIntent yet, so payment_status is misleading; the
              page hides that pill (see status-badge block below) and
              points at Finalize as the next action. */}
          {invoice.status === 'draft' && (
            <p className="text-xs text-muted-foreground mt-1">
              Draft invoice — finalize to issue and begin collection.
            </p>
          )}
        </div>
        {/* Action surface — Stripe / Linear / Vercel dashboard pattern.
            One primary CTA per state + an overflow menu ⋯ for everything
            else. Replaces the prior 8-10 buttons flowing across the
            header which crowded the page and obscured the next action.

            Draft is the exception: Add Line Item is an inline EDIT
            action (mutating the line items table just below); keeping
            it as a flat header button preserves the "you're editing
            this draft" affordance. Everything else sits in the
            overflow. */}
        <div className="flex items-center gap-2">
          <Button variant="outline" size="sm"
            onClick={() => navigate(`/audit-log?resource_type=invoice&resource_id=${invoice.id}`)}>
            <History size={14} className="mr-1.5" />
            Audit log
          </Button>
          {invoice.status === 'draft' && (
            <Button variant="outline" size="sm" onClick={() => setShowAddLineItem(true)} disabled={acting}>
              Add Line Item
            </Button>
          )}

          {/* Primary CTA — the state-specific "next step." Operators
              should see one obvious green action. Other actions
              (including secondary destructive ones like Void) live
              behind the overflow menu. */}
          {invoice.status === 'draft' && (() => {
            // Server gates Finalize when tax_status != 'ok'. Mirror
            // the gate on the UI so operators see the constraint
            // before clicking. Stripe Dashboard does the same.
            const taxBlocked = invoice.tax_status === 'pending' || invoice.tax_status === 'failed'
            if (!taxBlocked) {
              return (
                <Button size="sm" onClick={() => finalizeMutation.mutate()} disabled={acting}>
                  {finalizeMutation.isPending ? <><Loader2 size={14} className="animate-spin mr-1.5" />Finalizing…</> : 'Finalize'}
                </Button>
              )
            }
            const reason = invoice.tax_status === 'pending'
              ? 'Tax calculation is still pending — retry in progress. Finalize unblocks once tax_status = ok.'
              : 'Tax calculation has failed after retries. Resolve the customer billing profile or retry tax before finalizing.'
            return (
              <Tooltip>
                <TooltipTrigger render={<span className="inline-block cursor-not-allowed" />}>
                  <Button size="sm" disabled>Finalize</Button>
                </TooltipTrigger>
                <TooltipContent>{reason}</TooltipContent>
              </Tooltip>
            )
          })()}

          {invoice.status === 'finalized' && invoice.payment_status !== 'succeeded' && !paymentIsUnresolved(invoice.payment_status) && invoice.amount_due_cents > 0 && (
            !hasPaymentMethod ? (
              // No card → no collect. A greyed-out primary next to the
              // banner's active primary was competing noise; the banner
              // owns the next step (2026-07-22 audit, P3-2 — Stripe hides
              // the collect action when it cannot run).
              null
            ) : (
              <Button size="sm" onClick={() => collectMutation.mutate()} disabled={acting}>
                {collectMutation.isPending ? <><Loader2 size={14} className="animate-spin mr-1.5" />Collecting…</> : 'Collect Payment'}
              </Button>
            )
          )}

          {/* Terminal states default to Download PDF as the visible
              primary — the most common operator action when the
              invoice is settled is "give me the receipt." Keeps the
              header from collapsing to a single ⋯ trigger which
              feels empty. */}
          {(invoice.status === 'paid' || invoice.status === 'voided' || invoice.status === 'uncollectible') && (
            <Button size="sm" onClick={() => downloadPDF(invoice.id, invoice.invoice_number)}>
              <Download size={14} className="mr-1.5" />
              Download PDF
            </Button>
          )}

          {/* Overflow menu — everything else. Ordered by frequency of
              use within each group, with the destructive Void at the
              bottom under a separator (Stripe / Linear / GitHub
              convention for destructive actions in menus).

              Items are grouped by intent:
                1. Recovery / state-change (Record Payment, Mark
                   Uncollectible) — operator drives the invoice
                   toward a terminal state.
                2. Customer-facing (Email, Copy Link, Rotate) —
                   surfaces or sends to the customer.
                3. Document (Preview PDF, Download PDF, Issue Credit) —
                   reference + bookkeeping actions.
                4. Destructive (Void) — last, separated.

              The menu always renders (even on draft where most rows
              are hidden) so the trigger position stays stable. */}
          <DropdownMenu>
            <DropdownMenuTrigger render={<Button variant="outline" size="sm" disabled={acting} aria-label="More invoice actions" />}>
              <MoreHorizontal size={16} />
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end" className="w-56">
              {((invoice.status === 'finalized' && invoice.payment_status !== 'succeeded' && !paymentIsUnresolved(invoice.payment_status) && invoice.amount_due_cents > 0) ||
                invoice.status === 'uncollectible') && (
                <DropdownMenuItem onClick={() => setShowRecordPaymentDialog(true)} disabled={acting}>
                  <Receipt size={14} className="mr-2" />
                  Record offline payment
                </DropdownMenuItem>
              )}
              {/* Mark uncollectible is the DELIBERATE exception (ADR-107). It is
                  the only action permitted on a parked invoice — the one whose
                  charge could not be identified — so hiding it here would leave
                  that invoice with no way to reach any terminal state, which is
                  the gap the write-off exists to close. A payment that is
                  genuinely still resolving is a different matter and is still
                  excluded. */}
              {invoice.status === 'finalized' && invoice.payment_status !== 'succeeded' &&
                invoice.payment_status !== 'processing' && (
                <DropdownMenuItem onClick={() => setShowMarkUncollectibleConfirm(true)} disabled={acting} className="text-amber-700 dark:text-amber-400">
                  <AlertOctagon size={14} className="mr-2" />
                  Mark uncollectible
                </DropdownMenuItem>
              )}
              {((invoice.status === 'finalized' && invoice.payment_status !== 'succeeded' && !paymentIsUnresolved(invoice.payment_status) && invoice.amount_due_cents > 0) ||
                invoice.status === 'uncollectible' ||
                (invoice.status === 'finalized' && invoice.payment_status !== 'succeeded' && !paymentIsUnresolved(invoice.payment_status))) && (
                <DropdownMenuSeparator />
              )}

              {/* Hidden while the payment is unresolved: the server refuses it
                  (the email carries a pay call-to-action, and for a parked
                  invoice the hosted page will not even show a Pay button), so
                  offering it here is discovery-by-failure. */}
              {invoice.status !== 'voided' && !paymentIsUnresolved(invoice.payment_status) && (
                <DropdownMenuItem onClick={() => setShowEmailModal(true)} disabled={acting}>
                  <Mail size={14} className="mr-2" />
                  Email invoice
                </DropdownMenuItem>
              )}
              {invoice.public_token && invoice.status !== 'draft' && (
                <DropdownMenuItem onClick={copyPublicLink} disabled={acting}>
                  <Link2 size={14} className="mr-2" />
                  Copy public link
                </DropdownMenuItem>
              )}
              {invoice.public_token && invoice.status !== 'draft' && invoice.status !== 'paid' && (
                <DropdownMenuItem onClick={() => setShowRotatePublicTokenConfirm(true)} disabled={acting}>
                  <RotateCw size={14} className="mr-2" />
                  Rotate public link
                </DropdownMenuItem>
              )}
              {(invoice.status !== 'voided' || invoice.public_token) && <DropdownMenuSeparator />}

              <DropdownMenuItem
                onClick={async () => {
                  try {
                    const res = await fetch(`/v1/invoices/${invoice.id}/pdf`, { credentials: 'same-origin' })
                    const blob = await res.blob()
                    const url = URL.createObjectURL(blob)
                    setPdfPreviewUrl(url)
                  } catch {
                    toast.error('Failed to load PDF preview')
                  }
                }}
                disabled={acting}
              >
                <Eye size={14} className="mr-2" />
                Preview PDF
              </DropdownMenuItem>
              <DropdownMenuItem onClick={() => downloadPDF(invoice.id, invoice.invoice_number)} disabled={acting}>
                <Download size={14} className="mr-2" />
                Download PDF
              </DropdownMenuItem>
              {/* Refused while the payment is unresolved — crediting could
                  refund money that was never collected. */}
              {(invoice.status === 'finalized' || invoice.status === 'paid' || invoice.status === 'uncollectible') &&
                !paymentIsUnresolved(invoice.payment_status) && (
                <DropdownMenuItem onClick={() => setShowCreditModal(true)} disabled={acting}>
                  <CreditCard size={14} className="mr-2" />
                  Issue credit note
                </DropdownMenuItem>
              )}

              {/* Refused while the payment is unresolved: voiding an invoice
                  whose charge may have succeeded would annul something that was
                  in fact paid. Mark uncollectible stays available above — it is
                  the one exit for a parked invoice. */}
              {invoice.status !== 'voided' && invoice.status !== 'paid' &&
                !paymentIsUnresolved(invoice.payment_status) && (
                <>
                  <DropdownMenuSeparator />
                  <DropdownMenuItem
                    onClick={() => setShowVoidConfirm(true)}
                    disabled={acting}
                    className="text-destructive focus:text-destructive focus:bg-destructive/10"
                  >
                    <XCircle size={14} className="mr-2" />
                    Void invoice
                  </DropdownMenuItem>
                </>
              )}
            </DropdownMenuContent>
          </DropdownMenu>
        </div>
      </div>

      {/* Test-clock banner — the page-level "Currently at <sim time>"
          anchor. Gated on the AUTHORITATIVE invoice.is_simulated (not the
          subscription's clock id), so manual one-off invoices get the
          banner too — they were previously left with the "Simulated" chip
          but no reference point. resolvedTestClockId falls back to the
          customer's pin; if it's empty (clock since deleted) the banner
          renders its deleted-clock variant. */}
      {invoice.is_simulated && (
        <TestClockBanner testClockId={resolvedTestClockId} />
      )}

      {/* Uncollectible status banner — Stripe-parity bad-debt
          framing. Stands in for the operator-context card (which is
          intentionally hidden on uncollectible) so the page still
          explains the state clearly. Surfaces the accounting
          consequence + the recovery paths so the operator isn't
          left wondering why most buttons disappeared. */}
      {invoice.status === 'uncollectible' && (
        <div className="mt-4 rounded-lg border border-amber-500/30 bg-amber-500/5 p-4">
          <div className="flex items-start gap-3">
            <div className="mt-0.5 h-2 w-2 rounded-full bg-amber-500 shrink-0" />
            <div className="flex-1 min-w-0">
              <p className="text-sm font-medium text-foreground">Marked uncollectible</p>
              <p className="text-xs text-muted-foreground mt-1">
                Recorded as bad debt. Dunning automation has halted and no further collection is attempted.
                The invoice stays on the books for audit. The subscription remains <strong>active</strong> —
                {' '}{subscription?.id ? (
                  <Link to={`/subscriptions/${subscription.id}`} className="text-primary hover:underline">cancel it separately</Link>
                ) : 'cancel it separately'} if you also want to stop future billing.
              </p>
              <p className="text-xs text-muted-foreground mt-2">
                If the customer pays after all, use <strong>Record Payment</strong> above. To reclassify as
                cancelled (rather than written-off), use <strong>Void</strong>.
              </p>
            </div>
          </div>
        </div>
      )}

      {/* Unified attention banner — typed reason + prescribed actions
          for tax/payment/overdue. Computed server-side; renders only
          when invoice.attention is set (healthy + terminal-state
          invoices return no attention). See ADR-009. */}
      <InvoiceAttention
        invoice={invoice as unknown as ApiInvoice}
        onRetryTax={() => retryTaxMutation.mutate()}
        onChargeNow={() => collectMutation.mutate()}
        // send_reminder is state-aware. At the no_payment_method stage the
        // matching action is to re-send the payment-METHOD setup link the
        // engine already emailed (Checkout setup mode → add a card → engine
        // auto-charges); that goes straight to the customer, no recipient
        // modal. Every other reason (awaiting_payment, payment_failed,
        // overdue) opens the email dialog to send the hosted-invoice "pay"
        // link, where the operator confirms the recipient.
        onSendReminder={() =>
          invoice?.attention?.reason === 'no_payment_method' ||
          invoice?.attention?.reason === 'dunning_exhausted'
            ? resendSetupLinkMutation.mutate()
            : setShowEmailModal(true)
        }
        retrying={retryTaxMutation.isPending}
        charging={collectMutation.isPending}
        // ADR-030 / simulated-time discipline: anchor "since X ago" on the
        // owning sub's test-clock frozen_time, not wall-clock. Without this, a
        // sub frozen at 2024-02-01 on a wall-clock 2026-05-15 dashboard shows
        // "since 833d ago" on a fresh attention state. effectiveNow(undefined)
        // for a wall-clock invoice → wall time.
        now={effectiveNow(testClock?.frozen_time)}
      />

      {/* Operator context — diagnostic detail (dunning, payment intent
          ids, attempt counts) below the attention banner. Hidden on
          terminal states. */}
      {showOperatorContext && (
        <OperatorContextCard
          invoice={invoice}
          dunningRun={activeDunningRun}
          timeline={timeline}
        />
      )}

      {/* Invoice Document */}
      <Card className={cn(
        'overflow-hidden',
        invoice.status === 'voided' && 'border-destructive/30'
      )}>
        <CardContent className="p-8 sm:p-10">
          {/* Document Header */}
          <div className="flex items-start justify-between">
            <div>
              <p className="text-2xl font-bold tracking-tight text-foreground">
                {settings?.company_name || 'VELOX'}
              </p>
              {formatCompanyAddressLines(settings).map((line, i) => (
                <p key={i} className="text-sm text-muted-foreground mt-1">{line}</p>
              ))}
            </div>
            <div className="text-right">
              <p className="text-sm font-medium uppercase tracking-widest text-muted-foreground">Invoice</p>
              <p className="text-xl font-semibold text-foreground mt-0.5">{invoice.invoice_number}</p>
            </div>
          </div>

          {/* Header status — single humanised pill. Payment-state detail
              and operator actions live in the InvoiceAttention banner
              below; duplicating "payment_awaiting" up here was leaky
              domain-code and visually noisy. */}
          <div className="flex items-center gap-2 mt-5">
            <Badge variant={statusVariant(invoice.status)}>{humaniseInvoiceStatus(invoice.status)}</Badge>
            {/* Simulated disclosure lives in the page banner alone
                (ADR-099: once per scope) — the header pill duplicated it. */}
            {/* Bare date — the badge already says "Paid"/"Voided", so
                prefixing the date with the same word is redundant.
                "Paid Paid May 2, 2026" → "Paid · May 2, 2026". */}
            {invoice.status === 'voided' && invoice.voided_at && (
              <span className="text-xs text-muted-foreground ml-1">{formatDate(invoice.voided_at)}</span>
            )}
            {invoice.status === 'paid' && invoice.paid_at && (
              <span className="text-xs text-muted-foreground ml-1">{formatDate(invoice.paid_at)}</span>
            )}
          </div>

          <Separator className="my-6" />

          {/* FROM / BILL TO */}
          <div className="grid grid-cols-2 gap-8">
            <div>
              <p className="text-xs font-semibold uppercase tracking-wider text-muted-foreground mb-2">From</p>
              <p className="text-sm font-medium text-foreground">{settings?.company_name || 'Your Company'}</p>
              {formatCompanyAddressLines(settings).map((line, i) => (
                <p key={i} className="text-sm text-muted-foreground mt-0.5">{line}</p>
              ))}
              {settings?.company_email && (
                <p className="text-sm text-muted-foreground mt-0.5">{settings.company_email}</p>
              )}
              {settings?.company_phone && (
                <p className="text-sm text-muted-foreground mt-0.5">{settings.company_phone}</p>
              )}
            </div>
            <div>
              <p className="text-xs font-semibold uppercase tracking-wider text-muted-foreground mb-2">Bill To</p>
              {customer ? (
                <>
                  <Link to={`/customers/${customer.id}`} className="text-sm font-medium text-primary hover:underline">
                    {customer.display_name}
                  </Link>
                  {customer.email && (
                    <p className="text-sm text-muted-foreground mt-0.5">{customer.email}</p>
                  )}
                </>
              ) : (
                <p className="text-sm font-mono text-muted-foreground">{invoice.customer_id}</p>
              )}
              {subscription && (
                <p className="text-sm text-muted-foreground mt-0.5">
                  Sub: <Link to={`/subscriptions/${subscription.id}`} className="text-primary hover:underline">{subscription.display_name}</Link>
                </p>
              )}
            </div>
          </div>

          <Separator className="my-6" />

          {/* Dates row */}
          <div className="grid grid-cols-2 sm:grid-cols-4 gap-4">
            <div>
              <p className="text-xs font-semibold uppercase tracking-wider text-muted-foreground mb-1">Issued</p>
              <p className="text-sm text-foreground">{invoice.issued_at ? formatDate(invoice.issued_at) : formatDate(invoice.created_at)}</p>
            </div>
            <div>
              <p className="text-xs font-semibold uppercase tracking-wider text-muted-foreground mb-1">Due</p>
              <div className="flex items-center gap-2">
                <p className="text-sm text-foreground">{invoice.due_at ? formatDate(invoice.due_at) : '\u2014'}</p>
                {/* Due-date countdown only applies while the invoice
                    is open (finalized). status='paid' or 'voided'
                    are terminal — countdown becomes meaningless. The
                    pre-fix gate checked payment_status !== 'paid'
                    but payment_status uses 'succeeded' for paid
                    invoices ('paid' is never the value), so the
                    badge leaked onto paid rows. */}
                {invoice.due_at && invoice.status === 'finalized' && (
                  <DueBadge dueAt={invoice.due_at} warningDays={3} now={effectiveNow(testClock?.frozen_time)} />
                )}
              </div>
            </div>
            {/* A billing period is a subscription-cycle concept. A manual
                one-off invoice has no cycle, so the backend defaults
                start==end (now→now) to satisfy the NOT NULL columns — which
                renders as a confusing same-day "Jan 14 – Jan 14". Industry
                (Stripe/Chargebee/Lago) omit an invoice-level period for
                one-off charges; show it only when there's a real span. */}
            {invoice.billing_period_display && (
              <div>
                <p className="text-xs font-semibold uppercase tracking-wider text-muted-foreground mb-1">Period</p>
                <p className="text-sm text-foreground">
                  {invoice.billing_period_display}
                </p>
              </div>
            )}
            <div>
              <p className="text-xs font-semibold uppercase tracking-wider text-muted-foreground mb-1">Terms</p>
              {/* Render the terms THIS invoice was issued with (its own
                  net_payment_term_days \u2014 the value that produced due_at),
                  not the current tenant default. The tenant setting only
                  seeds new invoices; an already-issued invoice keeps the
                  term it was created with, and Due = Issued + that term.
                  Reading the live setting here made Terms disagree with
                  the Due date whenever the default had since changed. */}
              <p className="text-sm text-foreground">
                {invoice.net_payment_term_days == null
                  ? '\u2014'
                  : invoice.net_payment_term_days === 0
                    ? 'Due on receipt'
                    : `Net ${invoice.net_payment_term_days}`}
              </p>
            </div>
          </div>

          <Separator className="my-6" />

          {/* Line Items Table */}
          <div className="-mx-2">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead className="w-[50%]">Item</TableHead>
                  <TableHead className="text-right">Qty</TableHead>
                  <TableHead className="text-right">Unit Price</TableHead>
                  <TableHead className="text-right">Amount</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {lineItems.map(item => {
                  // Surface the Stripe-canonical tax reason as a small badge
                  // when it's non-trivial (issue #4). standard_rated and
                  // empty reasons would just be noise — the Tax column
                  // already conveys the default-path case.
                  const reasonLabel = taxReasonLabel(item.tax_reason)
                  // Operators occasionally see "prorated 20/31 days" and
                  // wonder why the count is what it is. The tooltip
                  // surfaces the day-grade math: period boundaries snap
                  // to 00:00 in tenant TZ, days counted inclusive of
                  // signup day. Makes the *why* one hover away without
                  // bloating the line description.
                  const prorationMatch = /prorated (\d+)\/(\d+) days/.exec(item.description)
                  return (
                    <TableRow key={item.id} className={cn(invoice.status === 'voided' && 'opacity-50')}>
                      <TableCell>
                        <div className="flex items-center gap-2 flex-wrap">
                          {prorationMatch ? (
                            <Tooltip>
                              <TooltipTrigger render={<span className="text-sm text-foreground cursor-help underline decoration-dotted underline-offset-2" />}>
                                {item.description}
                              </TooltipTrigger>
                              <TooltipContent className="max-w-xs">
                                {`Subscription started mid-cycle. Charge covers ${prorationMatch[1]} of ${prorationMatch[2]} days in the period. Period boundaries snap to start-of-day in tenant timezone (signup day inclusive).`}
                              </TooltipContent>
                            </Tooltip>
                          ) : (
                            <span className="text-sm text-foreground">{item.description}</span>
                          )}
                          <span className="text-xs text-muted-foreground">({formatLineType(item.line_type)})</span>
                          {reasonLabel && (
                            <Badge variant="outline" className="text-[10px] font-normal px-1.5 py-0 h-4">
                              {reasonLabel}
                            </Badge>
                          )}
                        </div>
                        {/* ADR-031: surface the line's own period when it
                            differs from the invoice header. Period mismatch
                            has multiple causes (in_advance base on a mixed
                            invoice, partial cycle from mid-period activation,
                            segment-aware billing under plan changes) — the
                            explicit dates convey what the line covers without
                            us guessing the cause. */}
                        {item.billing_period_start && item.billing_period_end && (
                          item.billing_period_start !== invoice.billing_period_start ||
                          item.billing_period_end !== invoice.billing_period_end
                        ) && (
                          <div className="text-xs text-muted-foreground mt-0.5">
                            Covers {formatCivilPeriod(item.billing_period_start, item.billing_period_end, invoice.billing_timezone)}
                          </div>
                        )}
                      </TableCell>
                      <TableCell className="text-right font-mono tabular-nums text-sm">
                        {item.quantity_decimal && Number(item.quantity_decimal) !== 0
                          ? Number(item.quantity_decimal).toLocaleString(undefined, { maximumFractionDigits: 12 })
                          : item.quantity.toLocaleString()}
                      </TableCell>
                      <TableCell className="text-right font-mono tabular-nums text-sm">{formatRate(item.unit_amount_decimal ?? item.unit_amount_cents, invoice.currency)}</TableCell>
                      <TableCell className="text-right font-mono tabular-nums text-sm font-medium">{formatCents(item.amount_cents, invoice.currency)}</TableCell>
                    </TableRow>
                  )
                })}
              </TableBody>
            </Table>
          </div>

          {/* Summary section */}
          <div className="flex justify-end mt-4">
            <div className="w-72 space-y-2">
              {/* Subtotal */}
              <div className={cn('flex justify-between text-sm', invoice.status === 'voided' && 'text-muted-foreground')}>
                <span>Subtotal</span>
                <span className="font-mono tabular-nums">{formatCents(invoice.subtotal_cents, invoice.currency)}</span>
              </div>

              {/* Discount */}
              {invoice.discount_cents > 0 && (
                <div className={cn('flex justify-between text-sm', invoice.status === 'voided' && 'text-muted-foreground')}>
                  <span>Discount</span>
                  <span className="font-mono tabular-nums">-{formatCents(invoice.discount_cents, invoice.currency)}</span>
                </div>
              )}

              {/* Tax — three states: standard (amount), reverse charge (legend row), exempt (legend row) */}
              {invoice.tax_amount_cents > 0 && (
                <>
                  <div className={cn('flex justify-between text-sm', invoice.status === 'voided' && 'text-muted-foreground')}>
                    <span>{invoice.tax_name || 'Tax'}{invoice.tax_rate > 0 ? ` (${formatTaxRate(invoice.tax_rate)}%)` : ''}</span>
                    <span className="font-mono tabular-nums">{formatCents(invoice.tax_amount_cents, invoice.currency)}</span>
                  </div>
                  {(() => {
                    const jurisdictionRows = aggregateTaxByJurisdiction(lineItems)
                    if (jurisdictionRows.length < 2) return null
                    return (
                      <div className="pl-4 space-y-0.5">
                        {jurisdictionRows.map(row => (
                          <div key={row.label} className="flex justify-between text-xs text-muted-foreground">
                            <span>{row.label}</span>
                            <span className="font-mono tabular-nums">{formatCents(row.amount, invoice.currency)}</span>
                          </div>
                        ))}
                      </div>
                    )
                  })()}
                </>
              )}
              {invoice.tax_reverse_charge && invoice.tax_amount_cents === 0 && (
                <div className={cn('flex justify-between text-sm text-muted-foreground', invoice.status === 'voided' && 'opacity-60')}>
                  <span>Tax (reverse charge)</span>
                  <span className="font-mono tabular-nums">{formatCents(0, invoice.currency)}</span>
                </div>
              )}
              {invoice.tax_exempt_reason && invoice.tax_amount_cents === 0 && !invoice.tax_reverse_charge && (
                <div className={cn('flex justify-between text-sm text-muted-foreground', invoice.status === 'voided' && 'opacity-60')}>
                  <span>Tax (exempt)</span>
                  <span className="font-mono tabular-nums">{formatCents(0, invoice.currency)}</span>
                </div>
              )}

              {/* Total */}
              {(invoice.discount_cents > 0 || invoice.tax_amount_cents > 0) && (
                <div className={cn('flex justify-between text-sm font-medium', invoice.status === 'voided' && 'text-muted-foreground')}>
                  <span>Total</span>
                  <span className="font-mono tabular-nums">{formatCents(invoice.total_amount_cents, invoice.currency)}</span>
                </div>
              )}

              {/* Settlement waterfall */}
              {invoice.status === 'voided' ? (
                <>
                  <Separator />
                  <div className="flex justify-between text-sm font-semibold">
                    <span>Amount Due</span>
                    <span className="font-mono tabular-nums">$0.00</span>
                  </div>
                </>
              ) : (() => {
                // Post-payment CNs route the refund somewhere: PM, credit balance, or out-of-band.
                // Pre-payment CNs (unpaid invoice flow) just reduce amount_due and have all three zero.
                const isPostPaymentCN = (cn: CreditNote) =>
                  cn.refund_amount_cents > 0 || cn.credit_amount_cents > 0 || (cn.out_of_band_amount_cents ?? 0) > 0
                const prePaymentCNs = invoice.status === 'paid'
                  ? creditNotes.filter(cn => !isPostPaymentCN(cn))
                  : creditNotes
                const postPaymentCNs = invoice.status === 'paid'
                  ? creditNotes.filter(isPostPaymentCN)
                  : []

                return (
                  <>
                    {prePaymentCNs.map(cn => (
                      <div key={cn.id} className="text-sm text-emerald-600">
                        <div className="flex justify-between gap-2">
                          <span className="truncate mr-2">
                            Credit {cn.credit_note_number}
                            {cn.reason ? <span className="text-muted-foreground"> · {creditNoteReasonLabel(cn.reason)}</span> : null}
                          </span>
                          <span className="font-mono tabular-nums shrink-0">-{formatCents(cn.total_cents, invoice.currency)}</span>
                        </div>
                        <TaxReversedLine cn={cn} currency={invoice.currency} />
                      </div>
                    ))}

                    {invoice.credits_applied_cents > 0 && (
                      <div className="flex justify-between text-sm text-emerald-600">
                        <span>Credits Applied</span>
                        <span className="font-mono tabular-nums">-{formatCents(invoice.credits_applied_cents, invoice.currency)}</span>
                      </div>
                    )}

                    {invoice.amount_paid_cents > 0 && (
                      <div className="flex justify-between text-sm text-muted-foreground">
                        <span>Amount Paid</span>
                        <span className="font-mono tabular-nums">-{formatCents(invoice.amount_paid_cents, invoice.currency)}</span>
                      </div>
                    )}

                    <Separator />
                    <div className="flex justify-between font-semibold text-base pt-1">
                      <span>Amount Due</span>
                      <span className="font-mono tabular-nums">{formatCents(invoice.amount_due_cents, invoice.currency)}</span>
                    </div>

                    {/* Post-payment adjustments */}
                    {(() => {
                      // Show CNs that have settled at least one channel.
                      // Refund leg counts when refund_status=succeeded.
                      // Credit / out-of-band are immediate at Issue time.
                      const completedCNs = postPaymentCNs.filter(cn =>
                        cn.credit_amount_cents > 0 ||
                        (cn.out_of_band_amount_cents ?? 0) > 0 ||
                        (cn.refund_amount_cents > 0 && cn.refund_status === 'succeeded')
                      )
                      const channelDescription = (cn: CreditNote): string => {
                        const parts: string[] = []
                        if (cn.refund_amount_cents > 0) parts.push(`${formatCents(cn.refund_amount_cents, invoice.currency)} → card`)
                        if (cn.credit_amount_cents > 0) parts.push(`${formatCents(cn.credit_amount_cents, invoice.currency)} → credit`)
                        if ((cn.out_of_band_amount_cents ?? 0) > 0) parts.push(`${formatCents(cn.out_of_band_amount_cents ?? 0, invoice.currency)} → out of band`)
                        return parts.join(' · ')
                      }
                      return completedCNs.length > 0 ? (
                        <div className="mt-3 pt-3 border-t border-dashed border-border space-y-2">
                          <p className="text-[10px] font-medium text-muted-foreground uppercase tracking-wider">Post-payment adjustments</p>
                          {completedCNs.map(cn => {
                            return (
                              <div key={cn.id} className="text-xs text-muted-foreground">
                                <div className="flex justify-between gap-2">
                                  <span className="truncate">
                                    {cn.credit_note_number} -- {creditNoteReasonLabel(cn.reason)}
                                  </span>
                                  <span className="font-mono tabular-nums shrink-0">{formatCents(cn.total_cents, invoice.currency)}</span>
                                </div>
                                <div className="text-[11px] mt-0.5 leading-snug">
                                  {channelDescription(cn)}
                                </div>
                                <TaxReversedLine cn={cn} currency={invoice.currency} />
                              </div>
                            )
                          })}
                        </div>
                      ) : null
                    })()}
                  </>
                )
              })()}
            </div>
          </div>

          {/* Tax treatment legend — shown when invoice carries a non-standard tax disposition */}
          {(invoice.tax_reverse_charge || invoice.tax_exempt_reason) && (
            <div className="mt-6 pt-4 border-t border-dashed border-border space-y-1.5 text-xs text-muted-foreground">
              {invoice.tax_reverse_charge && (
                <p><span className="font-medium text-foreground">Reverse charge</span> — VAT to be accounted for by the recipient in their jurisdiction.</p>
              )}
              {invoice.tax_exempt_reason && !invoice.tax_reverse_charge && (
                <p><span className="font-medium text-foreground">Tax-exempt</span> — {invoice.tax_exempt_reason}</p>
              )}
            </div>
          )}

          {/* ID footer */}
          <div className="mt-8 pt-4 border-t border-dashed border-border flex items-center justify-between">
            <div className="flex items-center gap-1.5 text-xs text-muted-foreground">
              <span className="font-mono">{invoice.id}</span>
              <CopyButton text={invoice.id} />
            </div>
            <div className="flex items-center gap-3 text-xs text-muted-foreground">
              {invoice.tax_provider && invoice.tax_provider !== 'none' && (
                <span className="uppercase">
                  Tax: {invoice.tax_provider === 'stripe_tax' ? 'Stripe Tax' : invoice.tax_provider}
                </span>
              )}
              <span className="uppercase">{invoice.currency}</span>
            </div>
          </div>
        </CardContent>
      </Card>

      {/* Terminal-state banners (Paid, Voided) intentionally not
          rendered. The header status pill + the activity timeline
          (with ADR-020's "Invoice paid · via Visa •••• 4242"
          coalesced row) carry the same information. Stripe / Lago /
          Recurly all follow this pattern: banners are reserved for
          attention-required states (declined, dunning, tax pending);
          terminal states trust the header. ADR-020 / 2026-05-05
          cleanup. */}

      {/* Activity Timeline — chronology of lifecycle + payment events.
          Replaces the older "Payment Activity" framing now that
          lifecycle anchors (created, finalized, scheduled) sit
          alongside Stripe + dunning events. */}
      {(billingTimeline.length > 0 || timelineDegraded.length > 0) && invoice.status !== 'draft' && (
        <Card className={cn('mt-6', invoice.status === 'voided' && 'opacity-60')}>
          <CardHeader>
            <CardTitle className="text-sm">Activity</CardTitle>
          </CardHeader>
          <CardContent>
            {timelineDegraded.length > 0 && (
              <p className="text-xs text-destructive mb-3">
                Some activity couldn&apos;t be loaded ({timelineDegraded.map(laneLabel).join(', ')}).
                This timeline may be incomplete — refresh to retry.
              </p>
            )}
            {timelineTruncated && (
              <p className="text-xs text-muted-foreground mb-3">
                Showing the most recent credit notes only — earlier ones aren&apos;t displayed.
              </p>
            )}
            <div className="relative">
              {billingTimeline.map((event, i) => {
                return (
                <div key={event.id ?? `${event.source}:${event.event_type}:${event.timestamp}:${event.payment_intent_id ?? ''}`} className="flex gap-4 pb-2 last:pb-0">
                  <div className="flex flex-col items-center">
                    <div className={cn(
                      'w-2.5 h-2.5 rounded-full mt-1.5',
                      event.status === 'succeeded' || event.status === 'resolved' ? 'bg-emerald-500' :
                      event.status === 'failed' || event.status === 'canceled' ? 'bg-destructive' :
                      event.status === 'processing' || event.status === 'scheduled' ? 'bg-blue-500' :
                      event.status === 'escalated' ? 'bg-violet-500' :
                      'bg-amber-500'
                    )} />
                    {i < billingTimeline.length - 1 && <div className="w-px flex-1 bg-border mt-1" />}
                  </div>
                  <div className="flex-1 min-w-0">
                    <div className="flex items-center justify-between">
                      <p className="text-sm text-foreground min-w-0">{event.description}</p>
                      <span className="text-xs text-muted-foreground ml-4 whitespace-nowrap">{formatDateTime(event.timestamp)}</span>
                    </div>
                    {event.error && (event.status === 'failed' || event.status === 'canceled') && (
                      <p className="text-xs text-destructive mt-0.5">{event.error}</p>
                    )}
                    {event.amount_cents != null && event.amount_cents > 0 && (
                      <p className="text-xs text-muted-foreground mt-0.5">{formatCents(event.amount_cents, invoice.currency)}</p>
                    )}
                    {/* Sub-line: payment instrument, attempt count, etc. */}
                    {event.detail && (
                      <p className="text-xs text-muted-foreground mt-0.5">{event.detail}</p>
                    )}
                    {event.event_type === 'invoice.paid' && (event.attempt_count ?? 0) > 0 && (
                      <p className="text-xs text-muted-foreground mt-0.5">
                        after {event.attempt_count} retry attempt{event.attempt_count === 1 ? '' : 's'}
                      </p>
                    )}
                    {/* Operator contract, Invariant A: a row whose primary
                        timestamp is on the invoice's simulated calendar but
                        which also touched the real world (emails, charges)
                        shows the real instant too. One word product-wide —
                        "Recorded" — matching the subscription timeline. */}
                    {event.recorded_at && (
                      <p className="text-xs text-muted-foreground/70 mt-0.5">
                        Recorded {formatDateTime(event.recorded_at)}
                      </p>
                    )}
                  </div>
                </div>
                )
              })}
            </div>
          </CardContent>
        </Card>
      )}

      {/* Void Confirm */}
      <TypedConfirmDialog
        open={showVoidConfirm}
        onOpenChange={setShowVoidConfirm}
        title="Void Invoice"
        description="Voiding annuls the invoice — it'll be treated as if it was never owed. Any applied credits are returned and any open charge attempt is cancelled. Voiding is blocked while a payment is unresolved: if the charge is still being confirmed the server will refuse until it reports an outcome, and if its outcome can never be identified the invoice must be marked uncollectible instead. Use this when the invoice was created in error. For 'we tried to collect and failed', use Mark Uncollectible instead. This action cannot be undone."
        confirmWord="VOID"
        confirmLabel="Void Invoice"
        onConfirm={() => voidMutation.mutate()}
        loading={voidMutation.isPending}
      />

      {/* Mark Uncollectible Confirm — Stripe-parity bad-debt write-off.
          Distinct from Void: invoice stays on the books, halts
          dunning, no credits reversed, no PaymentIntent cancelled.
          Subscription stays active (operator decides separately). */}
      <TypedConfirmDialog
        open={showMarkUncollectibleConfirm}
        onOpenChange={setShowMarkUncollectibleConfirm}
        title="Mark Invoice Uncollectible"
        description="Records this invoice as bad debt. The invoice stays on the books for audit, but dunning automation halts and no further collection is attempted. Subscription stays active — cancel it separately if you also want to stop future billing. If the payment provider later confirms the charge did go through, the invoice still settles as paid on its own."
        confirmWord="WRITE OFF"
        confirmLabel="Mark Uncollectible"
        onConfirm={() => markUncollectibleMutation.mutate()}
        loading={markUncollectibleMutation.isPending}
      />

      {/* Rotate public-token Confirm — defensive affordance for when the
          public URL has been shared where it shouldn't be (wider email
          thread, ticketing-system paste, archive leak). Not destructive
          to the invoice itself, but irreversible for the old URL. */}
      <TypedConfirmDialog
        open={showRotatePublicTokenConfirm}
        onOpenChange={setShowRotatePublicTokenConfirm}
        title="Rotate public link"
        description="This invalidates the current public URL for this invoice and mints a new one. Any email, message, or page still linking to the old URL will stop working. Use this if the current link was shared somewhere it shouldn't have been."
        confirmWord="ROTATE"
        confirmLabel="Rotate Link"
        onConfirm={() => rotatePublicTokenMutation.mutate()}
        loading={rotatePublicTokenMutation.isPending}
      />

      {/* Record Payment Dialog — Stripe-parity operator path for
          out-of-band collection. Amount is implicit (full amount_due);
          the optional note captures the operator's recording method
          (cheque #, wire reference, etc) for the audit trail. */}
      {showRecordPaymentDialog && (
        <Dialog open onOpenChange={(open) => { if (!open) { setShowRecordPaymentDialog(false); setRecordPaymentNote('') } }}>
          <DialogContent className="sm:max-w-md">
            <DialogHeader>
              <DialogTitle>Record offline payment</DialogTitle>
            </DialogHeader>
            <div className="space-y-3 py-2">
              <p className="text-sm text-muted-foreground">
                Mark this invoice as paid based on an out-of-band collection
                (cheque, wire, cash). The full {formatCents(invoice.amount_due_cents)} will be recorded
                as paid. Use this for payments received outside Velox's
                Stripe-attached charge flow.
              </p>
              <div className="space-y-1.5">
                <Label htmlFor="record-payment-note">Reference (optional)</Label>
                <Input
                  id="record-payment-note"
                  value={recordPaymentNote}
                  onChange={(e) => setRecordPaymentNote(e.target.value)}
                  placeholder="Cheque #1234, Wire 2026-05-20, etc."
                  maxLength={200}
                  disabled={recordPaymentMutation.isPending}
                />
                <p className="text-xs text-muted-foreground">
                  Surfaces in the audit trail for finance reconciliation.
                </p>
              </div>
            </div>
            <DialogFooter>
              <Button variant="outline" onClick={() => { setShowRecordPaymentDialog(false); setRecordPaymentNote('') }} disabled={recordPaymentMutation.isPending}>Cancel</Button>
              <Button onClick={() => recordPaymentMutation.mutate()} disabled={recordPaymentMutation.isPending}>
                {recordPaymentMutation.isPending ? 'Recording…' : 'Record Payment'}
              </Button>
            </DialogFooter>
          </DialogContent>
        </Dialog>
      )}

      {/* Email Modal */}
      {showEmailModal && (
        <EmailInvoiceDialog
          invoiceId={invoice.id}
          defaultEmail={customer?.email || ''}
          defaultCc={customer?.additional_emails || []}
          onClose={() => setShowEmailModal(false)}
          onSent={() => { setShowEmailModal(false); toast.success('Invoice email sent') }}
        />
      )}

      {/* Credit Modal */}
      {showCreditModal && (
        <IssueCreditDialog
          invoice={invoice}
          existingCreditNotes={creditNotes}
          onClose={() => setShowCreditModal(false)}
          onCreated={() => { setShowCreditModal(false); toast.success('Credit note issued'); invalidateAll() }}
        />
      )}

      {/* Add Line Item Modal */}
      {showAddLineItem && (
        <AddLineItemDialog
          invoiceId={invoice.id}
          onClose={() => setShowAddLineItem(false)}
          onAdded={() => { setShowAddLineItem(false); toast.success('Line item added'); invalidateAll() }}
        />
      )}

      {/* PDF Preview */}
      {pdfPreviewUrl && (
        <div role="dialog" aria-modal="true" aria-label="Invoice PDF preview" className="fixed inset-0 z-50 flex items-center justify-center bg-black/50 backdrop-blur-sm" onClick={() => { URL.revokeObjectURL(pdfPreviewUrl); setPdfPreviewUrl(null) }}>
          <div className="relative w-full max-w-4xl h-[85vh] bg-background rounded-2xl shadow-2xl overflow-hidden" onClick={e => e.stopPropagation()}>
            <div className="flex items-center justify-between px-6 py-3 border-b border-border">
              <h2 className="text-sm font-semibold text-foreground">Invoice Preview -- {invoice.invoice_number}</h2>
              <Button variant="ghost" size="sm" onClick={() => { URL.revokeObjectURL(pdfPreviewUrl); setPdfPreviewUrl(null) }}>
                Close
              </Button>
            </div>
            <iframe src={pdfPreviewUrl} className="w-full h-full" title="Invoice PDF Preview" />
          </div>
        </div>
      )}
    </Layout>
  )
}

// OperatorContextCard renders diagnostic supporting detail below the
// unified InvoiceAttention banner — dunning state, payment intent ids,
// attempt counts, deferred timestamps. The "why is this stuck" headline
// and the per-reason actions live in InvoiceAttention; this card is
// the field-level dl for engineers debugging beyond the banner.
function OperatorContextCard({
  invoice,
  dunningRun,
  timeline,
}: {
  invoice: Invoice
  dunningRun?: DunningRun
  timeline: TimelineEvent[]
}) {
  // attempt_count for this invoice is sourced from the active dunning run
  // when one exists (Velox keeps retry counters there, not on the invoice).
  // The payment timeline gives a payment-attempt count even before dunning
  // takes over.
  const paymentAttempts = timeline.filter(
    t => t.source === 'stripe' && (t.event_type === 'payment_intent.succeeded' || t.event_type === 'payment_intent.payment_failed')
  ).length

  // tax_status badge styling. Pending = amber (transient, retrying);
  // failed = destructive (manual action needed); ok = muted (unlikely
  // here since the card hides on terminal states but kept for safety).
  const taxStatusBadge = invoice.tax_status && invoice.tax_status !== 'ok' ? (
    <Badge
      variant={invoice.tax_status === 'failed' ? 'destructive' : 'outline'}
      className={invoice.tax_status === 'pending' ? 'border-amber-300 text-amber-700 dark:text-amber-400' : ''}
    >
      {invoice.tax_status}
    </Badge>
  ) : null

  // Don't render the card at all when there's nothing diagnostic to
  // show. A freshly-finalized invoice that's awaiting its first charge
  // has no tax issues, no payment attempts, no dunning — so every dl
  // entry below is conditional-false and the card reads as empty
  // noise. The InvoiceAttention banner already explains the state;
  // operators don't need an empty "Diagnostic detail" card alongside.
  const hasAnyDiagnostic =
    !!taxStatusBadge ||
    (invoice.tax_retry_count ?? 0) > 0 ||
    !!invoice.tax_deferred_at ||
    paymentAttempts > 0 ||
    !!invoice.last_payment_error ||
    invoice.payment_status === 'unknown' ||
    !!invoice.stripe_payment_intent_id ||
    !!dunningRun
  if (!hasAnyDiagnostic) return null

  return (
    <Card className="mb-6 border-border bg-muted/30">
      <CardHeader className="flex-row items-center gap-2 space-y-0 pb-3">
        <Info size={16} className="text-muted-foreground" />
        <CardTitle className="text-sm font-medium">Diagnostic detail</CardTitle>
      </CardHeader>
      <CardContent className="space-y-3 text-sm">
        <dl className="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-2">
          {taxStatusBadge && (
            <>
              <dt className="text-xs text-muted-foreground self-center">Tax status</dt>
              <dd className="flex items-center gap-2 flex-wrap">
                {taxStatusBadge}
              </dd>
            </>
          )}

          {(invoice.tax_retry_count ?? 0) > 0 && (
            <>
              <dt className="text-xs text-muted-foreground">Tax retries</dt>
              <dd className="text-foreground">{invoice.tax_retry_count}</dd>
            </>
          )}

          {invoice.tax_deferred_at && (
            <>
              <dt className="text-xs text-muted-foreground">Tax deferred at</dt>
              <dd className="text-foreground">{formatDateTime(invoice.tax_deferred_at)}</dd>
            </>
          )}

          {paymentAttempts > 0 && (
            <>
              <dt className="text-xs text-muted-foreground">Payment attempts</dt>
              <dd className="text-foreground">{paymentAttempts}</dd>
            </>
          )}

          {invoice.last_payment_error && (
            <>
              <dt className="text-xs text-muted-foreground">Last payment failure</dt>
              <dd className="text-foreground">{invoice.last_payment_error}</dd>
            </>
          )}

          {invoice.payment_status === 'unknown' && (
            <>
              <dt className="text-xs text-muted-foreground">Payment outcome</dt>
              {/*
                Two cases, and stating the wrong one here is worse than saying
                nothing: this card sits directly under the attention banner, so
                until now a PARKED invoice showed "Velox will not resolve this"
                immediately above "Reconciler will query Stripe" — adjacent
                panels asserting opposites (found walking the page as an
                operator, 2026-07-31). Without a PaymentIntent id there is
                nothing to query and the reconciler skips it by design.
              */}
              <dd className="text-foreground">
                {invoice.stripe_payment_intent_id
                  ? 'Reconciler will query Stripe after the cool-off window.'
                  : 'No payment intent was recorded, so there is nothing to query — this will not resolve on its own.'}
              </dd>
            </>
          )}

          {invoice.stripe_payment_intent_id && (
            <>
              <dt className="text-xs text-muted-foreground">Payment intent</dt>
              <dd className="font-mono text-xs text-foreground">{invoice.stripe_payment_intent_id}</dd>
            </>
          )}

          {dunningRun && (() => {
            // For a card-less invoice the scheduled dunning tick can't
            // collect (it re-discovers no card, counts an attempt, sends a
            // reminder) — so labelling it "Next retry: <date>" told the
            // operator a charge would happen when it won't (2026-07-22
            // audit, P1-3). Relabel to "Next reminder" and, once the run
            // has escalated, drop the future-step line entirely. Real
            // decline dunning (has-card) keeps "Next retry". attempt 0 is
            // humanized to match the Dunning list's "No retries yet" (P2-2).
            const cardless =
              invoice.attention?.reason === 'no_payment_method' ||
              invoice.attention?.reason === 'dunning_exhausted'
            const escalated = dunningRun.state === 'escalated'
            const attemptLabel =
              dunningRun.attempt_count === 0
                ? 'No retries yet'
                : `${dunningRun.attempt_count} ${dunningRun.attempt_count === 1 ? 'retry' : 'retries'}`
            return (
              <>
                <dt className="text-xs text-muted-foreground">Payment recovery</dt>
                <dd className="flex items-center gap-2">
                  <Badge variant="outline" className="border-amber-300 text-amber-700 dark:text-amber-400">
                    {escalated ? 'ended' : dunningRun.state}
                  </Badge>
                  <span className="text-xs text-muted-foreground">{attemptLabel}</span>
                </dd>

                {dunningRun.next_action_at && !escalated && (
                  <>
                    <dt className="text-xs text-muted-foreground">{cardless ? 'Next reminder' : 'Next retry'}</dt>
                    <dd className="text-foreground">
                      {formatDateTime(dunningRun.next_action_at)}
                      {cardless && (
                        <span className="block text-xs text-muted-foreground">Won't charge until a card is attached.</span>
                      )}
                    </dd>
                  </>
                )}
              </>
            )
          })()}
        </dl>

      </CardContent>
    </Card>
  )
}

function AddLineItemDialog({ invoiceId, onClose, onAdded }: {
  invoiceId: string; onClose: () => void; onAdded: () => void
}) {
  const [description, setDescription] = useState('')
  const [lineType, setLineType] = useState('add_on')
  const [quantity, setQuantity] = useState('1')
  const [unitAmount, setUnitAmount] = useState('')
  const [saving, setSaving] = useState(false)
  const [descriptionError, setDescriptionError] = useState('')
  const [amountError, setAmountError] = useState('')

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    setDescriptionError('')
    setAmountError('')
    if (!description.trim()) { setDescriptionError('Description is required'); return }
    if (!unitAmount || parseFloat(unitAmount) <= 0) { setAmountError('Unit amount must be greater than 0'); return }
    setSaving(true)
    try {
      await api.addInvoiceLineItem(invoiceId, {
        description: description.trim(),
        line_type: lineType,
        quantity: parseInt(quantity) || 1,
        unit_amount_cents: Math.round(parseFloat(unitAmount) * 100),
      })
      onAdded()
    } catch (err) {
      showApiError(err, 'Failed to add line item')
    } finally {
      setSaving(false)
    }
  }

  return (
    <Dialog open onOpenChange={(open) => { if (!open) onClose() }}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Add Line Item</DialogTitle>
        </DialogHeader>
        <form onSubmit={handleSubmit} noValidate className="space-y-4">
          <div className="space-y-2">
            <Label htmlFor="description">Description</Label>
            <Input id="description" value={description} onChange={e => { setDescription(e.target.value); setDescriptionError('') }} placeholder="e.g. Setup fee, Consulting hours" maxLength={500} />
            {descriptionError && <p className="text-xs text-destructive">{descriptionError}</p>}
          </div>
          <div className="space-y-2">
            <Label>Type</Label>
            {/* items maps value→label so <SelectValue> renders the label,
                not the raw code (Base UI). Reuses the existing
                LINE_TYPE_LABELS record as the single source of truth. */}
            <Select items={LINE_TYPE_LABELS} value={lineType} onValueChange={(v) => setLineType(v ?? '')}>
              <SelectTrigger className="w-full">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="add_on">{LINE_TYPE_LABELS.add_on}</SelectItem>
                <SelectItem value="base_fee">{LINE_TYPE_LABELS.base_fee}</SelectItem>
                <SelectItem value="usage">{LINE_TYPE_LABELS.usage}</SelectItem>
                <SelectItem value="discount">{LINE_TYPE_LABELS.discount}</SelectItem>
              </SelectContent>
            </Select>
          </div>
          <div className="grid grid-cols-2 gap-3">
            <div className="space-y-2">
              <Label htmlFor="quantity">Quantity</Label>
              <Input id="quantity" type="number" min={1} value={quantity} onChange={e => setQuantity(e.target.value)} />
            </div>
            <div className="space-y-2">
              <Label htmlFor="unitAmount">Unit Price ({getCurrencySymbol()})</Label>
              <Input id="unitAmount" type="number" step="0.01" min={0.01} value={unitAmount} onChange={e => { setUnitAmount(e.target.value); setAmountError('') }} placeholder="10.00" />
              {amountError && <p className="text-xs text-destructive">{amountError}</p>}
            </div>
          </div>
          {unitAmount && quantity && (
            <p className="text-sm text-muted-foreground">
              Total: {getCurrencySymbol()}{((parseInt(quantity) || 1) * parseFloat(unitAmount || '0')).toFixed(2)}
            </p>
          )}
          <DialogFooter>
            <Button type="button" variant="outline" onClick={onClose}>Cancel</Button>
            <Button type="submit" disabled={saving}>
              {saving ? <><Loader2 size={14} className="animate-spin mr-2" />Adding...</> : 'Add Line Item'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

function EmailInvoiceDialog({ invoiceId, defaultEmail, defaultCc, onClose, onSent }: {
  invoiceId: string; defaultEmail: string; defaultCc: string[]; onClose: () => void; onSent: () => void
}) {
  const form = useForm<EmailFormData>({
    resolver: zodResolver(emailSchema),
    defaultValues: { email: defaultEmail, additional_emails: defaultCc.join(', ') },
  })
  const { register, handleSubmit, formState: { errors, isSubmitting } } = form

  const onSubmit = handleSubmit(async (data) => {
    try {
      // Always send the explicit list the operator sees in the field —
      // an emptied field means "primary only" (ADR-082 tri-state).
      const cc = data.additional_emails.split(',').map(s => s.trim()).filter(Boolean)
      await api.sendInvoiceEmail(invoiceId, data.email, cc)
      onSent()
    } catch (err) {
      applyApiError(form, err, ['email', 'additional_emails'], { toastTitle: 'Failed to send email' })
    }
  })

  return (
    <Dialog open onOpenChange={(open) => { if (!open) onClose() }}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Email Invoice</DialogTitle>
        </DialogHeader>
        <form onSubmit={onSubmit} noValidate className="space-y-4">
          <div className="space-y-2">
            <Label htmlFor="email">Recipient Email</Label>
            <Input id="email" type="email" placeholder="customer@example.com" maxLength={254} {...register('email')} />
            {errors.email && <p className="text-xs text-destructive">{errors.email.message}</p>}
          </div>
          <div className="space-y-2">
            <Label htmlFor="additional_emails">CC</Label>
            <Input id="additional_emails" placeholder="finance@customer.com, ap@customer.com" {...register('additional_emails')} />
            <p className="text-xs text-muted-foreground">Separate with commas. Clear the field to email only the recipient.</p>
            {errors.additional_emails && <p className="text-xs text-destructive">{errors.additional_emails.message}</p>}
          </div>
          <DialogFooter>
            <Button type="button" variant="outline" onClick={onClose}>Cancel</Button>
            <Button type="submit" disabled={isSubmitting}>
              {isSubmitting ? <><Loader2 size={14} className="animate-spin mr-2" />Sending...</> : 'Send Email'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

function IssueCreditDialog({ invoice, existingCreditNotes, onClose, onCreated }: {
  invoice: Invoice
  existingCreditNotes: CreditNote[]
  onClose: () => void
  onCreated: () => void
}) {
  const isPaid = invoice.status === 'paid'

  // Composition-aware caps for the paid path. Refund to PM is bounded
  // by what the customer actually paid via card (minus prior CN
  // refunds). Credit-balance and out-of-band have no per-channel cap;
  // the only invariant is sum == total.
  const priorRefunds = existingCreditNotes
    .filter(cn => cn.status !== 'voided')
    .reduce((sum, cn) => sum + cn.refund_amount_cents, 0)
  const pmRefundableCents = Math.max(0, invoice.amount_paid_cents - priorRefunds)

  const [amount, setAmount] = useState('')
  const [refund, setRefund] = useState('')
  const [credit, setCredit] = useState('')
  const [outOfBand, setOutOfBand] = useState('')
  const [reason, setReason] = useState('')
  const [submitError, setSubmitError] = useState('')
  const [submitting, setSubmitting] = useState(false)

  const parseDollarsToCents = (s: string): number => {
    const n = parseFloat(s || '0')
    if (!isFinite(n)) return 0
    return Math.round(n * 100)
  }
  const amountCents = parseDollarsToCents(amount)
  const refundCents = parseDollarsToCents(refund)
  const creditCents = parseDollarsToCents(credit)
  const outOfBandCents = parseDollarsToCents(outOfBand)
  const allocatedCents = refundCents + creditCents + outOfBandCents

  // Auto-balance: typing in Refund auto-fills Credit with the
  // remainder so the simple case stays one input + Save. The user
  // can override by editing Credit directly, which sets the manual
  // flag and stops auto-fill.
  const [manualCredit, setManualCredit] = useState(false)
  const [manualOOB, setManualOOB] = useState(false)

  const handleAmountChange = (v: string) => {
    setAmount(v)
    if (!isPaid) return
    // Default on a paid invoice: full amount → credit balance.
    if (!manualCredit && !manualOOB && refund === '') {
      setCredit(v)
    }
  }
  const handleRefundChange = (v: string) => {
    setRefund(v)
    if (!isPaid) return
    if (!manualCredit && !manualOOB) {
      const remainder = amountCents - parseDollarsToCents(v) - outOfBandCents
      setCredit(remainder >= 0 ? (remainder / 100).toFixed(2) : '0.00')
    }
  }
  const handleCreditChange = (v: string) => {
    setCredit(v)
    setManualCredit(true)
  }
  const handleOOBChange = (v: string) => {
    setOutOfBand(v)
    setManualOOB(true)
    if (!manualCredit) {
      const remainder = amountCents - refundCents - parseDollarsToCents(v)
      setCredit(remainder >= 0 ? (remainder / 100).toFixed(2) : '0.00')
    }
  }

  // Live invariants for the Save gate.
  const allocationMatches = isPaid ? allocatedCents === amountCents : true
  const refundOverCap = refundCents > pmRefundableCents
  const reasonOk = reason.trim().length > 0
  const amountOk = amountCents > 0
  const canSubmit = reasonOk && amountOk && allocationMatches && !refundOverCap && !submitting

  const onSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    if (!canSubmit) return
    setSubmitting(true)
    setSubmitError('')
    try {
      await api.createCreditNote({
        invoice_id: invoice.id,
        reason: reason.trim(),
        ...(isPaid
          ? {
              refund_amount_cents: refundCents,
              credit_amount_cents: creditCents,
              out_of_band_amount_cents: outOfBandCents,
            }
          : {}),
        auto_issue: true,
        lines: [{ description: reason.trim(), quantity: 1, unit_amount_cents: amountCents }],
      })
      onCreated()
    } catch (err) {
      setSubmitError(err instanceof Error ? err.message : 'Failed to create credit note')
    } finally {
      setSubmitting(false)
    }
  }

  const fmt = (cents: number) => `${getCurrencySymbol()}${(cents / 100).toFixed(2)}`

  return (
    <Dialog open onOpenChange={(open) => { if (!open) onClose() }}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>Issue credit note</DialogTitle>
        </DialogHeader>
        <form onSubmit={onSubmit} noValidate className="space-y-4">
          <div>
            <Label>Invoice</Label>
            <p className="text-sm text-muted-foreground font-mono mt-1">{invoice.invoice_number}</p>
          </div>
          <div className="space-y-2">
            <Label htmlFor="cn-reason">Reason</Label>
            <Input
              id="cn-reason"
              placeholder="e.g. Service disruption, billing error"
              maxLength={500}
              value={reason}
              onChange={(e) => setReason(e.target.value)}
            />
          </div>
          <div className="space-y-2">
            <Label htmlFor="cn-amount">Amount ({getCurrencySymbol()})</Label>
            <Input
              id="cn-amount"
              type="number"
              step="0.01"
              min={0.01}
              max={999999.99}
              value={amount}
              onChange={(e) => handleAmountChange(e.target.value)}
            />
          </div>

          {isPaid && (
            <div className="space-y-3 rounded-md border bg-muted/30 p-3">
              <div className="text-xs text-muted-foreground">
                Allocate the credit note total across these channels. The three amounts must sum to the credit note total.
              </div>
              <div className="space-y-2">
                <Label htmlFor="cn-refund" className="flex items-center justify-between">
                  <span>Refund to card</span>
                  <span className="text-xs text-muted-foreground font-normal">max {fmt(pmRefundableCents)}</span>
                </Label>
                <Input
                  id="cn-refund"
                  type="number"
                  step="0.01"
                  min={0}
                  value={refund}
                  onChange={(e) => handleRefundChange(e.target.value)}
                />
                {refundOverCap && (
                  <p className="text-xs text-destructive">
                    Refund cannot exceed {fmt(pmRefundableCents)} paid via card
                    {priorRefunds > 0 ? ` (after ${fmt(priorRefunds)} prior refunds)` : ''}.
                  </p>
                )}
              </div>
              <div className="space-y-2">
                <Label htmlFor="cn-credit">Credit balance</Label>
                <Input
                  id="cn-credit"
                  type="number"
                  step="0.01"
                  min={0}
                  value={credit}
                  onChange={(e) => handleCreditChange(e.target.value)}
                />
              </div>
              <div className="space-y-2">
                <Label htmlFor="cn-oob">Outside Stripe (cash, ACH, manual)</Label>
                <Input
                  id="cn-oob"
                  type="number"
                  step="0.01"
                  min={0}
                  value={outOfBand}
                  onChange={(e) => handleOOBChange(e.target.value)}
                />
              </div>
              <div className="flex items-center justify-between pt-2 border-t text-sm">
                <span className="text-muted-foreground">Allocated</span>
                <span className={allocationMatches ? 'font-medium text-foreground' : 'font-medium text-destructive'}>
                  {fmt(allocatedCents)} / {fmt(amountCents)}
                  {amountCents > 0 && (allocationMatches ? ' ✓' : ' ✗')}
                </span>
              </div>
            </div>
          )}

          {submitError && (
            <p className="text-xs text-destructive">{submitError}</p>
          )}

          <DialogFooter>
            <Button type="button" variant="outline" onClick={onClose}>Cancel</Button>
            <Button type="submit" disabled={!canSubmit}>
              {submitting ? <><Loader2 size={14} className="animate-spin mr-2" />Creating...</> : 'Issue credit note'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

