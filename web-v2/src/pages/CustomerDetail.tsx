import { useEffect, useMemo, useRef, useState } from 'react'
import { usePageTitle } from '@/hooks/usePageTitle'
import { useEffectiveNow } from '@/hooks/useClockFrozenMap'
import { useNavigate, useParams, useSearchParams, Link } from 'react-router-dom'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useForm, Controller } from 'react-hook-form'
import { zodResolver } from '@hookform/resolvers/zod'
import { z } from 'zod'
import { toast } from 'sonner'
import { api, formatCents, formatDate, formatDateTime, getCurrencySymbol, type Customer, type BillingProfile, type Invoice, type DunningPolicyWithCount, type Subscription, type CustomerPaymentMethod } from '@/lib/api'
import { applyApiError, showApiError } from '@/lib/formErrors'
import { endOfDayInTZ, startOfDayInTZ } from '@/lib/dates'
import { Checkbox } from '@/components/ui/checkbox'
import { MarginCard } from '@/components/MarginCard'
import { Layout } from '@/components/Layout'
import { SendSetupLinkDialog } from '@/components/SendSetupLinkDialog'
import { PriceOverridesCard } from '@/components/PriceOverridesCard'
import { CostDashboard } from '@/components/CostDashboard'
import { TestClockBanner } from '@/components/TestClockBanner'
import { CreateSubscriptionDialog } from '@/components/CreateSubscriptionDialog'
import { cn } from '@/lib/utils'
import { customerNeedsPaymentMethod } from '@/lib/paymentReadiness'
import { statusBadgeVariant } from '@/lib/status'

import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from '@/components/ui/table'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Separator } from '@/components/ui/separator'
import {
  Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle,
} from '@/components/ui/dialog'
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from '@/components/ui/select'
import {
  AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent,
  AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle, AlertDialogTrigger,
} from '@/components/ui/alert-dialog'

import { Loader2, Pencil, CreditCard, Archive, Wand2, FilePlus2, Plus, Trash2, ChevronDown, ChevronRight, History, AlertTriangle } from 'lucide-react'
import { Textarea } from '@/components/ui/textarea'
import { CopyButton } from '@/components/CopyButton'
import { DetailBreadcrumb } from '@/components/DetailBreadcrumb'
import { DetailSkeleton } from '@/components/ui/DetailSkeleton'
import { Combobox } from '@/components/Combobox'
import {
  COUNTRIES,
  COUNTRY_NAME,
  CURRENCIES as GEO_CURRENCIES,
  statesForCountry,
  stateLabelForCountry,
  postalPlaceholderForCountry,
} from '@/lib/geo'
import { TAX_ID_HINTS, taxIdTypeOptions } from '@/lib/taxIdTypes'
import { ScrollPane } from '@/components/ui/scroll-pane'

const statusVariant = statusBadgeVariant

const editCustomerSchema = z.object({
  display_name: z.string().min(1, 'Display name is required'),
  email: z.string().email('Invalid email address').or(z.literal('')),
  // Comma-separated in the form; split/trimmed before submit. Server
  // validates each address, dedupes, and caps at 10 (ADR-082).
  additional_emails: z.string(),
})
type EditCustomerData = z.infer<typeof editCustomerSchema>

// splitEmails turns the comma-separated form value into the API list.
function splitEmails(value: string): string[] {
  return value.split(',').map(s => s.trim()).filter(Boolean)
}

const billingProfileSchema = z.object({
  legal_name: z.string(),
  phone: z.string().regex(/^[+\d\s()-]{7,20}$/, 'Invalid phone number').or(z.literal('')),
  address_line1: z.string(), address_line2: z.string(),
  city: z.string(), state: z.string(), postal_code: z.string(),
  country: z.string(), currency: z.string(),
  tax_status: z.enum(['standard', 'exempt', 'reverse_charge']),
  tax_exempt_reason: z.string().max(500, 'Must be at most 500 characters'),
  tax_id: z.string(), tax_id_type: z.string(),
}).refine(
  d => d.tax_status !== 'exempt' || d.tax_exempt_reason.trim().length > 0,
  { message: 'Exempt reason is required when tax status is Exempt', path: ['tax_exempt_reason'] }
)
type BillingProfileData = z.infer<typeof billingProfileSchema>

// sentEmailLabel maps the email_outbox.email_type to the operator-
// facing label rendered in the "Sent emails" section. Keep the strings
// short and unambiguous for ops/finance/support — no engineering
// jargon, no `email.X` namespacing.
function sentEmailLabel(emailType: string): string {
  switch (emailType) {
    case 'invoice': return 'Invoice'
    case 'payment_receipt': return 'Payment receipt'
    case 'payment_failed': return 'Payment failed'
    case 'payment_setup_request': return 'Payment method requested'
    case 'dunning_warning': return 'Payment retry — action required'
    case 'dunning_escalation': return 'Retries exhausted'
    case 'credit_note': return 'Credit note'
    default: return emailType
  }
}

// Subscription sort + bucketing on the customer detail page. Anchored
// to Stripe / Lago / Orb / Recurly customer-page conventions: active
// states surface first, terminal states collapse under a toggle, and
// each row is a single line with the next-event date as meta.
const SUB_STATUS_RANK: Record<string, number> = {
  trialing: 0, active: 1, past_due: 2, canceled: 3, ended: 4,
}

function subIsTerminal(status: string): boolean {
  return status === 'canceled' || status === 'ended'
}

function subMeta(sub: Subscription): string {
  if (sub.status === 'trialing' && sub.trial_end_at) {
    return `Trial ends ${formatDate(sub.trial_end_at)}`
  }
  // A scheduled cancel must beat the "Renews …" arm below, and the guard
  // has to hold for the COMMON shape: cancelling at period end sets
  // cancel_at_period_end + cancel_effective_at but leaves cancel_at unset
  // (that one is only for an explicit future-dated cancel). Requiring
  // cancel_at meant this branch never fired for a period-end cancel, so a
  // subscription that stops on Sep 1 advertised "Renews Sep 1".
  // cancel_effective_at is the backend's authoritative answer (ADR-069) and
  // is what SubscriptionDetail's timeline already uses.
  const cancelAt = sub.cancel_at_period_end || sub.cancel_at ? sub.cancel_effective_at || sub.cancel_at : null
  if (cancelAt) {
    return `Cancels ${formatDate(cancelAt)}`
  }
  if (sub.status === 'past_due' && sub.next_billing_at) {
    return `Past due · retry ${formatDate(sub.next_billing_at)}`
  }
  if (sub.status === 'active' && sub.next_billing_at) {
    return `Renews ${formatDate(sub.next_billing_at)}`
  }
  if (sub.status === 'canceled' && sub.canceled_at) {
    return `Canceled ${formatDate(sub.canceled_at)}`
  }
  if (sub.status === 'ended' && sub.current_billing_period_end) {
    return `Ended ${formatDate(sub.current_billing_period_end)}`
  }
  return ''
}

// The badge shows the provider-confirmed outcome when one exists
// (delivered / bounced / marked as spam), else the send-lifecycle
// status — 'dispatched' alone only means our relay accepted the
// handoff, which is a weaker claim than 'delivered'.
function sentEmailDisplayStatus(em: { status: string; delivery_state?: string }): string {
  if (em.delivery_state && em.delivery_state !== 'unknown') {
    return em.delivery_state === 'complained' ? 'marked as spam' : em.delivery_state
  }
  if (em.status === 'skipped') return 'not sent — settled first'
  return em.status
}

function sentEmailStatusVariant(display: string): 'default' | 'secondary' | 'destructive' | 'outline' {
  switch (display) {
    case 'delivered': return 'default'
    case 'bounced': return 'destructive'
    case 'marked as spam': return 'destructive'
    case 'dispatched': return 'default'
    case 'failed': return 'destructive'
    case 'pending': return 'secondary'
    default: return 'outline'
  }
}

export default function CustomerDetailPage() {
  const { id } = useParams<{ id: string }>()
  const queryClient = useQueryClient()
  const navigate = useNavigate()

  const [showEditCustomer, setShowEditCustomer] = useState(false)
  const [showEditBilling, setShowEditBilling] = useState(false)
  const [showCreateSub, setShowCreateSub] = useState(false)
  const [showDunningOverride, setShowDunningOverride] = useState(false)
  const [showNewInvoice, setShowNewInvoice] = useState(false)
  // Collapse-by-default for the "Sent emails" card — show 5 latest
  // rows inline; "Show all" expands into a scroll-constrained list.
  // Operator-dashboard convention (Stripe / Linear / Vercel all
  // chunk long activity logs this way) so the customer page stays
  // skimmable when a customer has 20+ emails in the 30-day window.
  const [sentEmailsExpanded, setSentEmailsExpanded] = useState(false)
  const { data: customer, isLoading, error: loadError, refetch } = useQuery({
    queryKey: ['customer', id],
    queryFn: () => api.getCustomer(id!),
    enabled: !!id,
  })

  // Fetch test clocks only when the customer is pinned — used by
  // CreateSubscriptionDialog to surface the ADR-027 inherit hint.
  const { data: clocksData } = useQuery({
    queryKey: ['test-clocks-for-customer-create-sub'],
    queryFn: () => api.listTestClocks(),
    enabled: !!customer?.test_clock_id,
  })
  const clockNameMap = useMemo(() => {
    const m: Record<string, string> = {}
    ;(clocksData?.data ?? []).forEach(c => { m[c.id] = c.name || c.id })
    return m
  }, [clocksData])

  // Commit/promo burndown (2026-07-06): per-grant remaining + kind
  // subtotals — the DP's primary finance question on hybrid commit+usage.
  const { data: creditGrants } = useQuery({
    queryKey: ['credit-grants', id],
    queryFn: () => api.listCreditGrants(id!),
    enabled: !!id,
  })

  const { data: overview } = useQuery({
    queryKey: ['customer-overview', id],
    queryFn: () => api.customerOverview(id!),
    enabled: !!id,
  })

  const { data: balanceData } = useQuery({
    queryKey: ['customer-balance', id],
    queryFn: () => api.getBalance(id!).catch(() => ({ balance_cents: 0 })),
    enabled: !!id,
  })
  const balance = balanceData?.balance_cents ?? 0

  const { data: billingProfile } = useQuery({
    queryKey: ['customer-billing-profile', id],
    queryFn: () => api.getBillingProfile(id!).catch(() => null),
    enabled: !!id,
  })

  const { data: metersData } = useQuery({
    queryKey: ['meters'],
    queryFn: () => api.listMeters(),
  })

  const meterMap: Record<string, { name: string; unit: string }> = {}
  metersData?.data?.forEach(m => { meterMap[m.id] = { name: m.name, unit: m.unit }; meterMap[m.key] = { name: m.name, unit: m.unit } })

  const { data: plans } = useQuery({
    queryKey: ['plans-active'],
    queryFn: () => api.listPlans().then(r => r.data.filter(p => p.status === 'active')),
  })

  const { data: allSubs } = useQuery({
    queryKey: ['customer-subscriptions', id],
    queryFn: () => api.listSubscriptions().then(r => r.data.filter(s => s.customer_id === id)),
    enabled: !!id,
  })

  // Split subs into active-ish (trialing/active/past_due) and terminal
  // (canceled/ended), sorted within each bucket by status priority then
  // most-recent first. Lets the card render active states front-and-
  // center and tuck terminal history under a toggle.
  const subBuckets = useMemo(() => {
    const sorted = [...(allSubs ?? [])].sort((a, b) => {
      const ra = SUB_STATUS_RANK[a.status] ?? 99
      const rb = SUB_STATUS_RANK[b.status] ?? 99
      if (ra !== rb) return ra - rb
      return (b.created_at ?? '').localeCompare(a.created_at ?? '')
    })
    return {
      active: sorted.filter(s => !subIsTerminal(s.status)),
      terminal: sorted.filter(s => subIsTerminal(s.status)),
    }
  }, [allSubs])
  const [showAllActiveSubs, setShowAllActiveSubs] = useState(false)
  const [showTerminalSubs, setShowTerminalSubs] = useState(false)
  const SUB_VISIBLE_CAP = 5
  const activeSubsToShow = showAllActiveSubs
    ? subBuckets.active
    : subBuckets.active.slice(0, SUB_VISIBLE_CAP)

  // Operator-side PM list (post-FEAT-3 surface). Lists every attached
  // PM with brand/last4/exp + default flag, drives the multi-card UI
  // below. Card capture stays in Stripe Checkout via setup-session URL
  // — operator never touches PAN.
  const { data: paymentMethodsList, refetch: refetchPMs } = useQuery({
    queryKey: ['customer-payment-methods', id],
    queryFn: () => api.listCustomerPaymentMethods(id!),
    enabled: !!id,
  })
  const paymentMethods: CustomerPaymentMethod[] = paymentMethodsList?.data ?? []
  // Shift-left warning: this customer bills automatically but has no card
  // (2026-07-22 audit, P1-5). Derived from data already loaded above.
  const needsCard = customerNeedsPaymentMethod(allSubs ?? [], paymentMethods)
  const [pmActionLoading, setPmActionLoading] = useState<string | null>(null)
  const [setupLinkDialogOpen, setSetupLinkDialogOpen] = useState(false)

  const handleSetPMDefault = async (pmId: string) => {
    if (!id) return
    setPmActionLoading(pmId)
    try {
      await api.setDefaultCustomerPaymentMethod(id, pmId)
      await refetchPMs()
      toast.success('Default payment method updated')
    } catch (err) {
      showApiError(err, 'Failed to update default payment method')
    } finally {
      setPmActionLoading(null)
    }
  }
  const handleDetachPM = async (pmId: string) => {
    if (!id) return
    setPmActionLoading(pmId)
    try {
      await api.detachCustomerPaymentMethod(id, pmId)
      await refetchPMs()
      toast.success('Payment method removed')
    } catch (err) {
      showApiError(err, 'Failed to remove payment method')
    } finally {
      setPmActionLoading(null)
    }
  }

  // Sent emails — last 30 days, Stripe shape (customer page email log).
  // Empty list is the common case for a fresh customer; failing the
  // query (e.g. backend not deployed yet) falls back to empty so the
  // page still renders.
  const { data: sentEmailsData } = useQuery({
    queryKey: ['customer-sent-emails', id],
    queryFn: () => api.listCustomerSentEmails(id!).catch(() => ({ sent_emails: [] })),
    enabled: !!id,
  })
  const sentEmails = sentEmailsData?.sent_emails ?? []

  // Dunning policies for this tenant (ADR-036 campaigns model). The
  // list drives the assignment dropdown on the customer detail page;
  // the row marked is_default=true is what the customer falls back
  // to when dunning_policy_id is empty.
  const { data: dunningPoliciesData } = useQuery({
    queryKey: ['dunning-policies'],
    queryFn: () => api.listDunningPolicies().catch(() => ({ data: [] })),
  })
  const dunningPolicies = dunningPoliciesData?.data ?? []
  const defaultDunningPolicy = dunningPolicies.find(p => p.is_default)
  const assignedDunningPolicy = customer?.dunning_policy_id
    ? dunningPolicies.find(p => p.id === customer.dunning_policy_id)
    : undefined
  const effectiveDunningPolicy = assignedDunningPolicy ?? defaultDunningPolicy

  const isArchived = customer?.status === 'archived'

  // After a Stripe Checkout return, the URL carries `?payment=success`
  // or `?payment=cancel`. Toast appropriately, refetch the payment
  // setup so the card details flip from pending → ready, and strip
  // the query string so a refresh doesn't re-toast. The webhook is
  // the authoritative path that flips the row; the toast is a UX
  // hint that the customer's redirect landed.
  //
  // The ref-guard is required: React StrictMode (dev) double-invokes
  // useEffect, and `setSearchParams` batches across both invocations
  // — both pass `payment` is still 'success' and would fire the toast
  // twice. Marking the param as handled on first run dedupes deterministic-
  // ally without relying on render order.
  const [searchParams, setSearchParams] = useSearchParams()
  const handledPaymentParam = useRef<string | null>(null)
  useEffect(() => {
    const payment = searchParams.get('payment')
    if (!payment) return
    if (handledPaymentParam.current === payment) return
    handledPaymentParam.current = payment
    if (payment === 'success') {
      toast.success('Payment method saved')
      queryClient.invalidateQueries({ queryKey: ['customer-payment-methods', id] })
    } else if (payment === 'cancel') {
      toast.info('Setup canceled — no changes were made')
    }
    const next = new URLSearchParams(searchParams)
    next.delete('payment')
    setSearchParams(next, { replace: true })
  }, [searchParams, setSearchParams, queryClient, id])

  const invalidateAll = () => {
    queryClient.invalidateQueries({ queryKey: ['customer', id] })
    queryClient.invalidateQueries({ queryKey: ['customer-overview', id] })
    queryClient.invalidateQueries({ queryKey: ['customer-balance', id] })
    queryClient.invalidateQueries({ queryKey: ['customer-billing-profile', id] })
    queryClient.invalidateQueries({ queryKey: ['customer-subscriptions', id] })
    queryClient.invalidateQueries({ queryKey: ['customer-usage', id] })
    queryClient.invalidateQueries({ queryKey: ['customer-payment-methods', id] })
    queryClient.invalidateQueries({ queryKey: ['customer-dunning-override', id] })
    queryClient.invalidateQueries({ queryKey: ['customers'] })
  }

  const loading = isLoading
  const error = loadError instanceof Error ? loadError.message : loadError ? String(loadError) : null

  usePageTitle(customer?.display_name)

  if (loading) {
    return (
      <Layout>
        <DetailSkeleton to="/customers" parentLabel="Customers" />
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

  if (!customer) {
    return (
      <Layout>
        <p className="text-sm text-muted-foreground py-16 text-center">Customer not found</p>
      </Layout>
    )
  }

  return (
    <Layout>
      <DetailBreadcrumb to="/customers" parentLabel="Customers" currentLabel={customer.display_name} />

      {/* Archived Banner */}
      {isArchived && (
        <Card className="mb-4 border-amber-200 bg-amber-50 dark:border-amber-900/50 dark:bg-amber-950/20">
          <CardContent className="px-5 py-3 flex items-center justify-between">
            <p className="text-sm text-amber-800 dark:text-amber-300">This customer has been archived. All data is read-only.</p>
            <Button variant="outline" size="sm"
              onClick={() => {
                api.updateCustomer(customer.id, { status: 'active' }).then(() => {
                  toast.success('Customer restored')
                  invalidateAll()
                }).catch((err: Error) => showApiError(err, 'Failed to update'))
              }}>
              Restore Customer
            </Button>
          </CardContent>
        </Card>
      )}

      {/* Header */}
      <div className="flex items-start justify-between mb-6">
        <div>
          <div className="flex items-center gap-2">
            <h1 className="text-2xl font-semibold text-foreground">{customer.display_name}</h1>
            {/* Customer-level test-clock attach (ADR-027). Badge on
                the header so the operator immediately sees that
                everything for this customer is on simulated time. */}
          </div>
          <div className="flex items-center gap-1.5 mt-1">
            <span className="text-xs text-muted-foreground font-mono">{customer.id}</span>
            <CopyButton text={customer.id} />
          </div>
        </div>
        <div className="flex items-center gap-3">
          {!isArchived && (
            <Button size="sm" onClick={() => setShowNewInvoice(true)}>
              <FilePlus2 size={14} className="mr-1.5" />
              New invoice
            </Button>
          )}
          {!isArchived && (
            <Button variant="outline" size="sm" onClick={() => setShowEditCustomer(true)}>
              <Pencil size={14} className="mr-1.5" />
              Edit
            </Button>
          )}
          <Button variant="outline" size="sm"
            onClick={() => navigate(`/audit-log?resource_type=customer&resource_id=${customer.id}`)}>
            <History size={14} className="mr-1.5" />
            Audit log
          </Button>
          {customer.status === 'active' && (
            <AlertDialog>
              <AlertDialogTrigger render={<Button variant="outline" size="sm" className="text-destructive hover:text-destructive" />}>
                <Archive size={14} className="mr-1.5" />
                Archive
              </AlertDialogTrigger>
              <AlertDialogContent>
                <AlertDialogHeader>
                  <AlertDialogTitle>Archive {customer.display_name}?</AlertDialogTitle>
                  <AlertDialogDescription>
                    Archiving hides this customer from active lists and blocks new subscriptions. It does <strong>not</strong> cancel existing subscriptions or stop billing — cancel any active subscriptions first. Unpaid invoices remain collectible and payment reminders continue. This can be undone.
                  </AlertDialogDescription>
                </AlertDialogHeader>
                <AlertDialogFooter>
                  <AlertDialogCancel>Cancel</AlertDialogCancel>
                  <AlertDialogAction
                    className="bg-destructive text-destructive-foreground hover:bg-destructive/90"
                    onClick={() => {
                      api.updateCustomer(customer.id, { status: 'archived' }).then(() => {
                        toast.success('Customer archived')
                        // invalidateAll() covers ['customers'] (list)
                        // + per-customer queries. refetch() alone
                        // updated only the local detail view, so the
                        // archived customer kept appearing on the
                        // /customers list until a hard refresh.
                        invalidateAll()
                      }).catch((err: Error) => {
                        // The backend 409s while any subscription still
                        // bills (ADR-067) and its message names the
                        // blocking subscription ids — surface it verbatim
                        // so the operator knows exactly what to cancel
                        // (the subscriptions are listed on this page).
                        showApiError(err, 'Cannot archive')
                      })
                    }}
                  >
                    Archive Customer
                  </AlertDialogAction>
                </AlertDialogFooter>
              </AlertDialogContent>
            </AlertDialog>
          )}
          <Badge variant={statusVariant(customer.status)}>{customer.status}</Badge>
        </div>
      </div>

      {/* Test-clock banner — top of the detail page when the customer
          is pinned (ADR-027). Carries the clock's current frozen_time
          + a "View clock" link, same shape used on SubscriptionDetail
          and InvoiceDetail. The header badge above is the at-a-glance
          marker; the banner is the explainer + navigation. */}
      {customer.test_clock_id && (
        <TestClockBanner testClockId={customer.test_clock_id} />
      )}

      {/* Key Metrics */}
      <Card>
        <CardContent className="p-0">
          <div className="flex divide-x divide-border">
            <div className="flex-1 px-6 py-4">
              <p className="text-sm text-muted-foreground">Email</p>
              <div className="flex items-center gap-2 mt-1">
                <p className="text-sm font-medium text-foreground">{customer.email || '—'}</p>
                {customer.email_status === 'bounced' && (
                  <Badge variant="destructive" className="text-xs" title={customer.email_bounce_reason || 'Permanent delivery failure'}>
                    Bounced
                  </Badge>
                )}
              </div>
            </div>
            <Link to={`/credits?customer=${id}`} className="flex-1 px-6 py-4 hover:bg-accent/50 transition-colors">
              <p className="text-sm text-muted-foreground">Credit Balance</p>
              <p className={cn('text-sm font-medium mt-1', balance > 0 ? 'text-emerald-600' : 'text-foreground')}>{formatCents(balance)}</p>
              {/* Kind split — money-backed commit liability must not read
                  as one number with free promo credits. */}
              {creditGrants && (creditGrants.commit_remaining_cents > 0 || creditGrants.promotional_remaining_cents > 0) && (
                <p className="text-xs text-muted-foreground mt-0.5">
                  {creditGrants.commit_remaining_cents > 0 && <>Commit {formatCents(creditGrants.commit_remaining_cents)}</>}
                  {creditGrants.commit_remaining_cents > 0 && creditGrants.promotional_remaining_cents > 0 && ' · '}
                  {creditGrants.promotional_remaining_cents > 0 && <>Promo {formatCents(creditGrants.promotional_remaining_cents)}</>}
                </p>
              )}
            </Link>
            {/* Outstanding balance — accounts-receivable exposure for
                this customer (Stripe / Lago / Chargebee / Recurly all
                surface this on the customer page). Distinct from
                Credit Balance (positive credit owed TO the customer);
                this is unpaid invoices owed BY the customer. Links to
                invoices filtered to their unpaid list so the operator
                can drill into resolution. Hidden when there's no
                exposure — keeps the row uncluttered on healthy
                customers. */}
            {(overview?.outstanding_balance?.total_cents ?? 0) > 0 && (
              <Link to={`/invoices?customer=${id}`} className="flex-1 px-6 py-4 hover:bg-accent/50 transition-colors">
                <p className="text-sm text-muted-foreground">Outstanding</p>
                <p className="text-sm font-medium mt-1 text-red-600">
                  {formatCents(overview?.outstanding_balance?.total_cents ?? 0)}
                  <span className="text-xs text-muted-foreground ml-1.5 font-normal">
                    · {overview?.outstanding_balance?.unpaid_count} invoice{overview?.outstanding_balance?.unpaid_count === 1 ? '' : 's'}
                  </span>
                </p>
              </Link>
            )}
            <div className="flex-1 px-6 py-4">
              <p className="text-sm text-muted-foreground">Subscriptions</p>
              <p className="text-sm font-medium text-foreground mt-1">{allSubs?.length ?? 0}</p>
            </div>
            <div className="flex-1 px-6 py-4">
              <p className="text-sm text-muted-foreground">Created</p>
              <p className="text-sm font-medium text-foreground mt-1">{formatDateTime(customer.created_at)}</p>
            </div>
          </div>
        </CardContent>
      </Card>

      {/* Details */}
      <Card className="mt-6">
        <CardHeader>
          <CardTitle className="text-sm">Details</CardTitle>
        </CardHeader>
        <CardContent className="p-0">
          <div className="divide-y divide-border">
            <div className="flex items-center justify-between px-6 py-3">
              <span className="text-sm text-muted-foreground">External ID</span>
              <div className="flex items-center gap-1.5">
                <span className="text-sm text-foreground font-mono">{customer.external_id}</span>
                {/* external_id is the identifier operators paste into
                    API calls and support threads — same copy affordance
                    as the internal ID below. */}
                <CopyButton text={customer.external_id} />
              </div>
            </div>
            <div className="flex items-center justify-between px-6 py-3">
              <span className="text-sm text-muted-foreground">Email</span>
              <span className="text-sm text-foreground flex items-center gap-2">
                {customer.email || '—'}
                {customer.email_status === 'bounced' && customer.email_last_bounced_at && (
                  <Badge variant="destructive" className="text-xs" title={customer.email_bounce_reason || 'Permanent delivery failure'}>
                    Bounced · {formatDate(customer.email_last_bounced_at)}
                  </Badge>
                )}
              </span>
            </div>
            <div className="flex items-center justify-between px-6 py-3">
              <span className="text-sm text-muted-foreground">Status</span>
              <Badge variant={statusVariant(customer.status)}>{customer.status}</Badge>
            </div>
            <div className="flex items-center justify-between px-6 py-3">
              <span className="text-sm text-muted-foreground">Created</span>
              <span className="text-sm text-foreground">{formatDateTime(customer.created_at)}</span>
            </div>
            <div className="flex items-center justify-between px-6 py-3">
              <span className="text-sm text-muted-foreground">ID</span>
              <div className="flex items-center gap-1.5">
                <span className="text-sm text-foreground font-mono">{customer.id}</span>
                <CopyButton text={customer.id} />
              </div>
            </div>
          </div>
        </CardContent>
      </Card>

      {/* Billing Profile */}
      <Card className="mt-6">
        {billingProfile ? (
          <>
            <CardHeader>
              <div className="flex items-center justify-between">
                <CardTitle className="text-sm">Billing Profile</CardTitle>
                {!isArchived && (
                  <Button variant="outline" size="sm" onClick={() => setShowEditBilling(true)}>
                    <Pencil size={12} className="mr-1.5" />
                    Edit
                  </Button>
                )}
              </div>
            </CardHeader>
            <CardContent className="space-y-6">
              {/* Contact */}
              <section>
                <p className="text-xs font-medium uppercase tracking-wide text-muted-foreground mb-3">Contact</p>
                <div className="grid grid-cols-1 md:grid-cols-2 gap-x-8 gap-y-4">
                  <div>
                    <p className="text-xs text-muted-foreground">Legal Name</p>
                    <p className="text-sm text-foreground mt-1">{billingProfile.legal_name || '—'}</p>
                  </div>
                  <div>
                    <p className="text-xs text-muted-foreground">Phone</p>
                    <p className="text-sm text-foreground mt-1">{billingProfile.phone || '—'}</p>
                  </div>
                </div>
              </section>

              <Separator />

              {/* Address */}
              <section>
                <p className="text-xs font-medium uppercase tracking-wide text-muted-foreground mb-3">Address</p>
                {billingProfile.address_line1 || billingProfile.city || billingProfile.country ? (
                  <div className="text-sm text-foreground leading-relaxed">
                    {billingProfile.address_line1 && <p>{billingProfile.address_line1}</p>}
                    {billingProfile.address_line2 && <p>{billingProfile.address_line2}</p>}
                    {(billingProfile.city || billingProfile.state || billingProfile.postal_code) && (
                      <p>
                        {[billingProfile.city, billingProfile.state].filter(Boolean).join(', ')}
                        {billingProfile.postal_code && ` ${billingProfile.postal_code}`}
                      </p>
                    )}
                    {billingProfile.country && <p>{COUNTRY_NAME[billingProfile.country] || billingProfile.country}</p>}
                  </div>
                ) : (
                  <p className="text-sm text-muted-foreground">No address on file</p>
                )}
              </section>

              <Separator />

              {/* Tax */}
              <section>
                <div className="flex items-center justify-between mb-3">
                  <p className="text-xs font-medium uppercase tracking-wide text-muted-foreground">Tax</p>
                  {billingProfile.tax_status === 'exempt' && <Badge variant="warning">Exempt</Badge>}
                  {billingProfile.tax_status === 'reverse_charge' && <Badge variant="outline">Reverse charge</Badge>}
                </div>
                {billingProfile.tax_status === 'exempt' ? (
                  <div className="space-y-2">
                    <p className="text-sm text-muted-foreground">Tax-exempt — invoices issue with zero tax and an audit note.</p>
                    {billingProfile.tax_exempt_reason && (
                      <p className="text-sm text-foreground">
                        <span className="text-xs text-muted-foreground">Reason: </span>
                        {billingProfile.tax_exempt_reason}
                      </p>
                    )}
                  </div>
                ) : billingProfile.tax_status === 'reverse_charge' ? (
                  <p className="text-sm text-muted-foreground">
                    B2B reverse charge — tax is zero on the invoice; the buyer self-accounts for VAT/GST in their jurisdiction.
                  </p>
                ) : (
                  <div className="grid grid-cols-1 md:grid-cols-2 gap-x-8 gap-y-4">
                    <div>
                      <p className="text-xs text-muted-foreground">Tax ID Type</p>
                      <p className="text-sm text-foreground mt-1 uppercase">{billingProfile.tax_id_type || '—'}</p>
                    </div>
                    <div>
                      <p className="text-xs text-muted-foreground">Tax ID</p>
                      <p className="text-sm text-foreground mt-1 font-mono">{billingProfile.tax_id || '—'}</p>
                    </div>
                  </div>
                )}
              </section>

              <Separator />

              {/* Currency */}
              <section>
                <p className="text-xs font-medium uppercase tracking-wide text-muted-foreground mb-3">Currency</p>
                <p className="text-sm text-foreground">
                  {billingProfile.currency
                    ? billingProfile.currency.toUpperCase()
                    : <span className="text-muted-foreground">Use tenant default</span>}
                </p>
              </section>
            </CardContent>
          </>
        ) : (
          <>
            <CardHeader>
              <CardTitle className="text-sm">Billing Profile</CardTitle>
            </CardHeader>
            <CardContent className="text-center py-8">
              <div className="w-10 h-10 rounded-full bg-muted flex items-center justify-center mx-auto mb-3">
                <CreditCard size={18} className="text-muted-foreground" />
              </div>
              <p className="text-sm text-foreground">No billing profile</p>
              <p className="text-sm text-muted-foreground mt-1 max-w-xs mx-auto">Set up billing details to enable invoicing and payments for this customer</p>
              {!isArchived && (
                <Button size="sm" className="mt-4" onClick={() => setShowEditBilling(true)}>
                  Set Up Billing Profile
                </Button>
              )}
            </CardContent>
          </>
        )}
      </Card>

      {/* Dunning policy — Stripe / Lago / Recurly campaigns shape
          (ADR-036). Operator either assigns a named policy or leaves
          the customer on the tenant default. */}
      <Card className="mt-6">
        <CardHeader>
          <div className="flex items-center justify-between">
            <CardTitle className="text-sm">Dunning policy</CardTitle>
            {!isArchived && (
              <Button variant="outline" size="sm" onClick={() => setShowDunningOverride(true)}>
                Change
              </Button>
            )}
          </div>
        </CardHeader>
        <CardContent className="p-0">
          {effectiveDunningPolicy ? (
            <div className="divide-y divide-border">
              <div className="flex items-center justify-between px-6 py-3">
                <span className="text-sm text-muted-foreground">Policy</span>
                <div className="flex items-center gap-2">
                  <span className="text-sm text-foreground">{effectiveDunningPolicy.name || 'Default'}</span>
                  {!assignedDunningPolicy && (
                    <Badge variant="outline">tenant default</Badge>
                  )}
                </div>
              </div>
              <div className="flex items-center justify-between px-6 py-3">
                <span className="text-sm text-muted-foreground">Max retry attempts</span>
                <span className="text-sm text-foreground">{effectiveDunningPolicy.max_retry_attempts}</span>
              </div>
              <div className="flex items-center justify-between px-6 py-3">
                <span className="text-sm text-muted-foreground">Grace period</span>
                <span className="text-sm text-foreground">{effectiveDunningPolicy.grace_period_days} days</span>
              </div>
              <div className="flex items-center justify-between px-6 py-3">
                <span className="text-sm text-muted-foreground">Retry schedule</span>
                <span className="text-sm text-foreground">{effectiveDunningPolicy.retry_schedule.join(' · ') || '—'}</span>
              </div>
              <div className="flex items-center justify-between px-6 py-3">
                <span className="text-sm text-muted-foreground">Final action</span>
                <Badge variant="outline">{effectiveDunningPolicy.final_action}</Badge>
              </div>
            </div>
          ) : (
            <p className="px-6 py-6 text-sm text-muted-foreground text-center">No dunning policies configured</p>
          )}
        </CardContent>
      </Card>

      {/* Usage This Period — multi-dim cost dashboard backed by
          GET /v1/customers/{id}/usage. Operator-facing surface.
          customerTestClockId pins its relative-time/window surfaces to the
          customer's simulated clock (ADR-027) instead of wall-clock. */}
      <div className="mt-6">
        <CostDashboard customerId={id!} customerTestClockId={customer.test_clock_id} />
      </div>

      {/* Margin — rated usage revenue vs provider COGS (ADR-079).
          Operator-only; never rendered on customer-facing surfaces. */}
      <div className="mt-6">
        <MarginCard customerId={id!} customerTestClockId={customer.test_clock_id} />
      </div>

      {/* Commit / credit-grant burndown — per-grant drawn vs remaining
          (Metronome/Orb surface commitment burndown on the customer view;
          this is the hybrid commit+usage DP's primary finance question). */}
      {creditGrants && creditGrants.grants.length > 0 && (
        <Card className="mt-6">
          <CardHeader>
            <CardTitle>Credit grants</CardTitle>
            <CardDescription>
              Drawdown per grant. Commit grants are money-backed (funded by a
              paid commit invoice); promotional grants burn first.
            </CardDescription>
          </CardHeader>
          <CardContent>
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Grant</TableHead>
                  <TableHead>Kind</TableHead>
                  <TableHead className="text-right">Granted</TableHead>
                  <TableHead className="text-right">Drawn</TableHead>
                  <TableHead className="text-right">Remaining</TableHead>
                  <TableHead>Expires</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {creditGrants.grants.map(g => (
                  <TableRow key={g.id}>
                    <TableCell className="text-sm max-w-xs truncate" title={g.description}>{g.description}</TableCell>
                    <TableCell>
                      {g.grant_kind === 'commit' ? <Badge>Commit</Badge>
                        : g.grant_kind === 'promotional' ? <Badge variant="secondary">Promo</Badge>
                        : <Badge variant="outline">Credit</Badge>}
                    </TableCell>
                    <TableCell className="text-sm text-right">{formatCents(g.amount_cents)}</TableCell>
                    <TableCell className="text-sm text-right">{formatCents(g.consumed_cents)}</TableCell>
                    <TableCell className="text-sm text-right font-medium">{formatCents(g.remaining_cents)}</TableCell>
                    <TableCell className="text-sm text-muted-foreground">{g.expires_at ? formatDate(g.expires_at) : 'never'}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </CardContent>
        </Card>
      )}

      {/* Public cost-dashboard token — ADR-031 / MANUAL_TEST CU8. Operator
          rotates to mint a shareable URL the customer can embed (iframe
          or fetch) without an API key. Rotation invalidates the previous
          token immediately. */}
      <div className="mt-6">
        <PublicCostDashboardCard customerId={id!} />
      </div>

      {/* Payment methods — multi-card operator panel.
          Industry-standard surface (Stripe, Chargebee, Recurly, Lago,
          Orb): operator can list, set-default, detach. Card capture
          happens browser → Stripe.js → Stripe via the setup-session
          URL — operator never touches PAN, tenant stays in PCI SAQ-A.
          The "Send setup link" button mints a Stripe Checkout URL
          the operator copies and hands to the customer. */}
      <Card className="mt-6">
        <CardHeader>
          <div className="flex items-center justify-between">
            <CardTitle className="text-sm">Payment methods</CardTitle>
            {!isArchived && (
              <Button size="sm" onClick={() => setSetupLinkDialogOpen(true)}>
                Add payment method
              </Button>
            )}
          </div>
        </CardHeader>
        <CardContent className="p-0">
          {paymentMethods.length === 0 ? (
            needsCard ? (
              <div className="mx-6 my-4 rounded-lg border border-amber-300 dark:border-amber-500/30 bg-amber-50 dark:bg-amber-500/10 px-4 py-3">
                <div className="flex items-start gap-2.5">
                  <AlertTriangle size={16} className="text-amber-500 mt-0.5 shrink-0" />
                  <div>
                    <p className="text-sm font-medium text-amber-800 dark:text-amber-400">No payment method on file</p>
                    <p className="text-xs text-amber-700/90 dark:text-amber-500/90 mt-0.5">
                      This customer has an active subscription that bills automatically. Add a card so renewal charges can be collected — without one, the next invoice will need manual follow-up.
                    </p>
                  </div>
                </div>
              </div>
            ) : (
              <div className="px-6 py-8 text-center">
                <CreditCard size={28} className="text-muted-foreground mx-auto mb-2" />
                <p className="text-sm text-foreground">No payment methods on file</p>
                <p className="text-xs text-muted-foreground mt-1">
                  Click "Add payment method" to email the customer a secure setup link.
                </p>
              </div>
            )
          ) : (
            <div className="divide-y divide-border">
              {paymentMethods.map(pm => (
                <div key={pm.id} className="flex items-center justify-between px-6 py-3">
                  <div className="flex items-center gap-3 min-w-0">
                    <div className="w-9 h-9 rounded-lg bg-muted flex items-center justify-center shrink-0">
                      <CreditCard size={16} className="text-foreground" />
                    </div>
                    <div className="min-w-0">
                      <div className="flex items-center gap-2">
                        <p className="text-sm font-medium text-foreground truncate">
                          {(pm.card_brand || 'Card').charAt(0).toUpperCase() + (pm.card_brand || 'card').slice(1)} ····{pm.card_last4 || '????'}
                        </p>
                        {pm.is_default && <Badge variant="outline" className="shrink-0 text-[10px]">Default</Badge>}
                      </div>
                      <p className="text-xs text-muted-foreground">
                        {pm.card_exp_month && pm.card_exp_year
                          ? `Expires ${String(pm.card_exp_month).padStart(2, '0')}/${pm.card_exp_year}`
                          : 'Saved card'}
                      </p>
                    </div>
                  </div>
                  {!isArchived && (
                    <div className="flex items-center gap-2 shrink-0">
                      {!pm.is_default && (
                        <Button
                          variant="ghost"
                          size="sm"
                          onClick={() => handleSetPMDefault(pm.id)}
                          disabled={pmActionLoading === pm.id}
                        >
                          {pmActionLoading === pm.id ? <><Loader2 size={14} className="animate-spin mr-1.5" />Updating…</> : 'Set as default'}
                        </Button>
                      )}
                      <AlertDialog>
                        <AlertDialogTrigger
                          render={
                            <Button
                              variant="ghost"
                              size="sm"
                              disabled={pmActionLoading === pm.id}
                            />
                          }
                        >
                          {pmActionLoading === pm.id ? <Loader2 size={14} className="animate-spin" /> : <Trash2 size={14} />}
                        </AlertDialogTrigger>
                        <AlertDialogContent>
                          <AlertDialogHeader>
                            <AlertDialogTitle>Remove this payment method?</AlertDialogTitle>
                            <AlertDialogDescription>
                              {paymentMethods.length === 1
                                ? `${(pm.card_brand || 'Card').charAt(0).toUpperCase() + (pm.card_brand || 'card').slice(1)} ····${pm.card_last4 || '????'} will be detached in Stripe and removed from this customer. After this the customer will have no payment method on file — new invoices won't be auto-charged: they'll be flagged for attention and the customer will be emailed a link to add a card. Billing resumes automatically once a new card is added.`
                                : `${(pm.card_brand || 'Card').charAt(0).toUpperCase() + (pm.card_brand || 'card').slice(1)} ····${pm.card_last4 || '????'} will be detached in Stripe and removed from this customer. Any subscriptions billing against it will fall back to the new default (if any) on next charge.`}
                            </AlertDialogDescription>
                          </AlertDialogHeader>
                          <AlertDialogFooter>
                            <AlertDialogCancel>Cancel</AlertDialogCancel>
                            <AlertDialogAction onClick={() => void handleDetachPM(pm.id)}>Remove</AlertDialogAction>
                          </AlertDialogFooter>
                        </AlertDialogContent>
                      </AlertDialog>
                    </div>
                  )}
                </div>
              ))}
            </div>
          )}
        </CardContent>
      </Card>

      {/* Price overrides — negotiated per-customer prices (ADR-070). The
          card is the deal's only operator-visible home: without it an
          override exists purely as unexplained math on the usage card
          and the invoice. */}
      <div className="mt-6">
        <PriceOverridesCard customerId={id!} customerTestClockId={customer?.test_clock_id} />
      </div>

      <SendSetupLinkDialog
        open={setupLinkDialogOpen}
        onOpenChange={setSetupLinkDialogOpen}
        customerId={id || ''}
        customerEmail={customer?.email}
      />

      {/* Subscriptions & Invoices grid */}
      <div className="grid grid-cols-1 lg:grid-cols-2 gap-6 mt-6">
        {/* Subscriptions — compact one-line rows, sorted by status
            priority (trialing → active → past_due). Terminal subs
            (canceled / ended) collapse under a toggle so they don't
            inflate the card on heavy-use customers. Anchored to
            Stripe / Lago / Orb / Recurly customer-page conventions. */}
        <Card>
          <CardHeader>
            <div className="flex items-center justify-between">
              <CardTitle className="text-sm">Subscriptions ({allSubs?.length ?? 0})</CardTitle>
              {!isArchived && <Button size="sm" onClick={() => setShowCreateSub(true)}>Create Subscription</Button>}
            </div>
          </CardHeader>
          <CardContent className="p-0">
            <div className="divide-y divide-border">
              {activeSubsToShow.map(sub => (
                <Link key={sub.id} to={`/subscriptions/${sub.id}`} className="flex items-center justify-between gap-3 px-6 py-2.5 hover:bg-accent/50 transition-colors">
                  <div className="flex items-center gap-2 min-w-0 flex-1">
                    <Badge variant={statusVariant(sub.status)} className="shrink-0">{sub.status}</Badge>
                    <span className="text-sm font-medium text-foreground truncate">{sub.display_name}</span>
                  </div>
                  <span className="text-xs text-muted-foreground whitespace-nowrap">{subMeta(sub)}</span>
                </Link>
              ))}
              {subBuckets.active.length > SUB_VISIBLE_CAP && !showAllActiveSubs && (
                <button
                  type="button"
                  onClick={() => setShowAllActiveSubs(true)}
                  className="flex items-center gap-1 w-full px-6 py-2 text-xs text-muted-foreground hover:bg-accent/50 hover:text-foreground transition-colors"
                >
                  <ChevronDown className="h-3 w-3" />
                  Show {subBuckets.active.length - SUB_VISIBLE_CAP} more
                </button>
              )}
              {subBuckets.terminal.length > 0 && (
                <>
                  <button
                    type="button"
                    onClick={() => setShowTerminalSubs(v => !v)}
                    aria-expanded={showTerminalSubs}
                    className="flex items-center gap-1 w-full px-6 py-2 text-xs text-muted-foreground hover:bg-accent/50 hover:text-foreground transition-colors"
                  >
                    {showTerminalSubs ? <ChevronDown className="h-3 w-3" /> : <ChevronRight className="h-3 w-3" />}
                    {subBuckets.terminal.length} past {subBuckets.terminal.length === 1 ? 'subscription' : 'subscriptions'}
                  </button>
                  {showTerminalSubs && subBuckets.terminal.map(sub => (
                    <Link key={sub.id} to={`/subscriptions/${sub.id}`} className="flex items-center justify-between gap-3 px-6 py-2.5 hover:bg-accent/50 transition-colors">
                      <div className="flex items-center gap-2 min-w-0 flex-1">
                        <Badge variant={statusVariant(sub.status)} className="shrink-0">{sub.status}</Badge>
                        <span className="text-sm font-medium text-muted-foreground truncate">{sub.display_name}</span>
                      </div>
                      <span className="text-xs text-muted-foreground whitespace-nowrap">{subMeta(sub)}</span>
                    </Link>
                  ))}
                </>
              )}
              {(!allSubs || allSubs.length === 0) && (
                <p className="px-6 py-6 text-sm text-muted-foreground text-center">No subscriptions</p>
              )}
            </div>
          </CardContent>
        </Card>

        {/* Recent Invoices */}
        <Card>
          <CardHeader>
            <CardTitle className="text-sm">Recent Invoices</CardTitle>
          </CardHeader>
          <CardContent className="p-0">
            <div className="divide-y divide-border">
              {overview?.recent_invoices.map(inv => {
                return (
                  <Link key={inv.id} to={`/invoices/${inv.id}`} className="flex items-center justify-between px-6 py-3 hover:bg-accent/50 transition-colors">
                    <div>
                      <p className="text-sm font-medium text-foreground">{inv.invoice_number}</p>
                      <p className="text-xs text-muted-foreground">{formatDate(inv.created_at)}</p>
                    </div>
                    <div className="flex items-center gap-3">
                      <Badge variant={statusVariant(inv.status)}>{inv.status}</Badge>
                      <span className="text-sm font-medium">{formatCents(inv.total_amount_cents)}</span>
                    </div>
                  </Link>
                )
              })}
              {(!overview?.recent_invoices.length) && (
                <p className="px-6 py-4 text-sm text-muted-foreground">No invoices</p>
              )}
            </div>
          </CardContent>
        </Card>

        {/* Sent emails — anchored on Stripe's customer-page email log
            (docs.stripe.com/invoicing/send-email; verified
            customer-page placement specifically, 30-day window). Other
            platforms vary — Recurly explicitly lacks this surface, so
            this isn't an industry-converging pattern. Kept for the
            operator-audit gap it fills ("did the customer get the
            dunning warning?"). */}
        <Card className="lg:col-span-2">
          <CardHeader>
            <div className="flex items-center justify-between">
              <CardTitle className="text-sm">
                Sent emails {sentEmails.length > 0 && <span className="text-muted-foreground font-normal">({sentEmails.length})</span>}
              </CardTitle>
              <span className="text-xs text-muted-foreground">Last 30 days</span>
            </div>
          </CardHeader>
          <CardContent className="p-0">
            <ScrollPane className={sentEmailsExpanded && sentEmails.length > 5 ? 'max-h-96' : ''}>
              <div className="divide-y divide-border">
                {(sentEmailsExpanded ? sentEmails : sentEmails.slice(0, 5)).map(em => (
                  <div key={em.id} className="px-6 py-3 flex items-start justify-between gap-3">
                    <div className="min-w-0 flex-1">
                      <div className="flex items-center gap-2 flex-wrap">
                        <p className="text-sm font-medium text-foreground">{sentEmailLabel(em.email_type)}</p>
                        {/* ?search= is the Invoices page's URL key (was
                            ?q=, which the page never read — dead link). */}
                        {em.invoice_number && (
                          <Link to={`/invoices?search=${encodeURIComponent(em.invoice_number)}`} className="text-xs text-muted-foreground hover:text-foreground underline-offset-2 hover:underline">
                            {em.invoice_number}
                          </Link>
                        )}
                      </div>
                      <p className="text-xs text-muted-foreground truncate">to {em.recipient}</p>
                      {em.status === 'failed' && em.last_error && (
                        <p className="mt-1 text-xs text-destructive truncate">{em.last_error}</p>
                      )}
                    </div>
                    <div className="flex items-center gap-2 shrink-0">
                      <Badge variant={sentEmailStatusVariant(sentEmailDisplayStatus(em))}>{sentEmailDisplayStatus(em)}</Badge>
                      <span className="text-xs text-muted-foreground whitespace-nowrap">
                        {formatDateTime(em.dispatched_at ?? em.created_at)}
                      </span>
                    </div>
                  </div>
                ))}
                {sentEmails.length === 0 && (
                  <p className="px-6 py-4 text-sm text-muted-foreground">No emails sent in the last 30 days</p>
                )}
              </div>
            </ScrollPane>
            {sentEmails.length > 5 && (
              <div className="border-t border-border px-6 py-2 flex justify-center">
                <button
                  type="button"
                  className="text-xs text-muted-foreground hover:text-foreground transition-colors"
                  onClick={() => setSentEmailsExpanded(v => !v)}
                >
                  {sentEmailsExpanded
                    ? 'Show recent only'
                    : `Show all (${sentEmails.length})`}
                </button>
              </div>
            )}
          </CardContent>
        </Card>
      </div>

      {/* Edit Customer Dialog */}
      {showEditCustomer && (
        <EditCustomerDialog
          customer={customer}
          onClose={() => setShowEditCustomer(false)}
          onSaved={() => {
            setShowEditCustomer(false)
            queryClient.invalidateQueries({ queryKey: ['customer', id] })
            // Display name / email change must propagate to the
            // /customers list too. Pre-fix only the detail page
            // refreshed; list rows stayed on the old name.
            queryClient.invalidateQueries({ queryKey: ['customers'] })
            toast.success('Customer updated')
          }}
        />
      )}

      {/* Create Subscription Dialog — shared with the Subscriptions
          page; locked to this customer so the picker is hidden and
          the customer_id is pre-filled. */}
      {plans && customer && (
        <CreateSubscriptionDialog
          open={showCreateSub}
          onOpenChange={setShowCreateSub}
          lockedCustomer={customer}
          plans={plans}
          clockNameMap={clockNameMap}
          onCreated={(sub) => {
            setShowCreateSub(false)
            toast.success(`Subscription "${sub.display_name}" created`)
            invalidateAll()
          }}
        />
      )}

      {/* Edit Billing Profile Dialog */}
      {showEditBilling && id && customer && (
        <EditBillingProfileDialog
          customerId={id}
          customer={customer}
          profile={billingProfile ?? null}
          onClose={() => setShowEditBilling(false)}
          onSaved={() => {
            setShowEditBilling(false)
            queryClient.invalidateQueries({ queryKey: ['customer-billing-profile', id] })
            toast.success('Billing profile saved')
          }}
        />
      )}

      {/* Assign dunning policy (ADR-036 campaigns model) */}
      {showDunningOverride && id && customer && (
        <AssignDunningPolicyDialog
          customerId={id}
          currentPolicyID={customer.dunning_policy_id ?? ''}
          policies={dunningPolicies}
          onClose={() => setShowDunningOverride(false)}
          onSaved={() => {
            setShowDunningOverride(false)
            queryClient.invalidateQueries({ queryKey: ['customer', id] })
            toast.success('Dunning policy assignment saved')
          }}
        />
      )}

      {/* One-off Invoice Composer (Week 7). Drawer-styled dialog —
          create a draft invoice, attach line items, optionally finalize+send.
          Backed by POST /v1/invoices, POST /v1/invoices/{id}/line-items,
          POST /v1/invoices/{id}/finalize, POST /v1/invoices/{id}/send. */}
      {showNewInvoice && id && customer && (
        <NewInvoiceDialog
          customerId={id}
          customer={customer}
          billingProfile={billingProfile ?? null}
          onClose={() => setShowNewInvoice(false)}
          onCreated={({ invoice, sent }) => {
            setShowNewInvoice(false)
            queryClient.invalidateQueries({ queryKey: ['customer-overview', id] })
            queryClient.invalidateQueries({ queryKey: ['invoices'] })
            if (sent) {
              toast.success(`Invoice ${invoice.invoice_number} sent`, {
                action: {
                  label: 'View invoice',
                  onClick: () => navigate(`/invoices/${invoice.id}`),
                },
              })
            } else {
              toast.success(`Draft ${invoice.invoice_number} saved`, {
                action: {
                  label: 'View invoice',
                  onClick: () => navigate(`/invoices/${invoice.id}`),
                },
              })
            }
          }}
        />
      )}

    </Layout>
  )
}

/* ─── Edit Customer ──────────────────────────────────────────── */

function EditCustomerDialog({ customer, onClose, onSaved }: {
  customer: Customer; onClose: () => void; onSaved: () => void
}) {
  const form = useForm<EditCustomerData>({
    resolver: zodResolver(editCustomerSchema),
    defaultValues: {
      display_name: customer.display_name,
      email: customer.email || '',
      additional_emails: (customer.additional_emails || []).join(', '),
    },
  })
  const { register, handleSubmit, formState: { errors, isSubmitting, isDirty } } = form

  const onSubmit = handleSubmit(async (data) => {
    if (!isDirty) return
    try {
      await api.updateCustomer(customer.id, {
        display_name: data.display_name,
        email: data.email,
        additional_emails: splitEmails(data.additional_emails),
      })
      onSaved()
    } catch (err) {
      applyApiError(form, err, ['display_name', 'email', 'additional_emails'], { toastTitle: 'Failed to update customer' })
    }
  })

  return (
    <Dialog open onOpenChange={(open) => { if (!open) onClose() }}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Edit Customer</DialogTitle>
        </DialogHeader>
        <form onSubmit={onSubmit} noValidate className="space-y-4">
          <div className="space-y-2">
            <Label htmlFor="display_name">Display Name</Label>
            <Input id="display_name" maxLength={255} {...register('display_name')} />
            {errors.display_name && <p className="text-xs text-destructive">{errors.display_name.message}</p>}
          </div>
          <div className="space-y-2">
            <Label htmlFor="email">Email</Label>
            <Input id="email" type="email" maxLength={254} {...register('email')} />
            {errors.email && <p className="text-xs text-destructive">{errors.email.message}</p>}
          </div>
          <div className="space-y-2">
            <Label htmlFor="additional_emails">Additional billing emails</Label>
            <Input id="additional_emails" placeholder="ap@customer.com, finance@customer.com" {...register('additional_emails')} />
            <p className="text-xs text-muted-foreground">
              CC&apos;d on invoices, receipts, credit notes and payment-issue emails. Separate with commas (up to 10).
            </p>
            {errors.additional_emails && <p className="text-xs text-destructive">{errors.additional_emails.message}</p>}
          </div>
          <DialogFooter>
            <Button type="button" variant="outline" onClick={onClose}>Cancel</Button>
            <Button type="submit" disabled={isSubmitting || !isDirty}>
              {isSubmitting ? <><Loader2 size={14} className="animate-spin mr-2" />Saving...</> : isDirty ? 'Save' : 'No changes'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}


/* ─── Edit Billing Profile ───────────────────────────────────── */

function EditBillingProfileDialog({ customerId, customer, profile, onClose, onSaved }: {
  customerId: string
  customer: Customer
  profile: BillingProfile | null
  onClose: () => void
  onSaved: () => void
}) {
  const defaultValues: BillingProfileData = {
    legal_name: profile?.legal_name || '',
    phone: profile?.phone || '',
    address_line1: profile?.address_line1 || '',
    address_line2: profile?.address_line2 || '',
    city: profile?.city || '',
    state: profile?.state || '',
    postal_code: profile?.postal_code || '',
    country: profile?.country || '',
    currency: profile?.currency || '',
    tax_status: profile?.tax_status || 'standard',
    tax_exempt_reason: profile?.tax_exempt_reason || '',
    tax_id: profile?.tax_id || '',
    tax_id_type: profile?.tax_id_type || '',
  }

  const formMethods = useForm<BillingProfileData>({
    resolver: zodResolver(billingProfileSchema),
    defaultValues,
  })
  const { register, handleSubmit, watch, setValue, control, formState: { errors: formErrors, isSubmitting, isDirty } } = formMethods
  const form = watch()

  const onSubmit = handleSubmit(async (data) => {
    if (!isDirty) return
    try {
      await api.upsertBillingProfile(customerId, data)
      onSaved()
    } catch (err) {
      applyApiError(formMethods, err, [
        'legal_name', 'phone',
        'address_line1', 'address_line2', 'city', 'state', 'postal_code', 'country',
        'currency', 'tax_status', 'tax_exempt_reason', 'tax_id', 'tax_id_type',
      ], { toastTitle: 'Failed to save billing profile' })
    }
  })

  const fillFromCustomer = () => {
    if (!form.legal_name && customer.display_name) {
      setValue('legal_name', customer.display_name, { shouldDirty: true })
    }
  }

  const countryOptions = COUNTRIES.map(([code, name]) => ({
    value: code, label: name, keywords: [code],
  }))
  const statesList = statesForCountry(form.country)
  const stateLabel = stateLabelForCountry(form.country)
  const postalPlaceholder = postalPlaceholderForCountry(form.country)
  const stateOptions = statesList
    ? statesList.map(([code, name]) => ({ value: code, label: name, keywords: [code] }))
    : null

  const currencyOptions = [
    { value: '', label: 'Use tenant default', keywords: ['default', 'tenant', 'inherit'] },
    ...GEO_CURRENCIES.map(c => ({
      value: c.code.toLowerCase(),
      label: `${c.code} — ${c.label}`,
      keywords: [c.code, c.code.toLowerCase(), c.symbol, c.label],
      prefix: <span className="text-muted-foreground font-mono text-[11px] w-7 inline-block text-center">{c.symbol}</span>,
    })),
  ]

  const canFillFromCustomer = !profile && !form.legal_name && !!customer.display_name

  return (
    <Dialog open onOpenChange={(open) => { if (!open) onClose() }}>
      {/* eslint-disable-next-line no-restricted-syntax -- DialogContent scrolls as ONE box including its own close button, so a content mask would fade that button. Giving dialogs a scrolling BODY inside a fixed shell is a redesign of every dialog; tracked, not bundled here. */}
      <DialogContent className="sm:max-w-2xl max-h-[85vh] overflow-y-auto">
        <DialogHeader>
          <DialogTitle>{profile ? 'Edit Billing Profile' : 'Set Up Billing Profile'}</DialogTitle>
        </DialogHeader>
        <form onSubmit={onSubmit} noValidate className="space-y-6">

          {canFillFromCustomer && (
            <button
              type="button"
              onClick={fillFromCustomer}
              className="w-full flex items-center justify-between gap-3 px-3 py-2.5 rounded-md border border-dashed border-border bg-muted/30 hover:bg-muted text-sm text-foreground transition-colors"
            >
              <span className="flex items-center gap-2 min-w-0">
                <Wand2 size={14} className="shrink-0 text-muted-foreground" />
                <span>Use customer name as legal name</span>
              </span>
              <span className="text-xs text-muted-foreground truncate">
                {customer.display_name}
              </span>
            </button>
          )}

          {/* Contact */}
          <div className="space-y-3">
            <p className="text-xs font-semibold text-muted-foreground uppercase tracking-wider">Contact</p>
            <div className="space-y-2">
              <Label>Legal Name</Label>
              <Input maxLength={255} placeholder="Acme Corporation Inc." {...register('legal_name')} />
            </div>
            <div className="space-y-2">
              <Label>Phone</Label>
              <Input type="tel" placeholder="+1 (555) 123-4567" maxLength={20} {...register('phone')} />
              {formErrors.phone && <p className="text-xs text-destructive">{formErrors.phone.message}</p>}
            </div>
          </div>

          <Separator />

          {/* Address */}
          <div className="space-y-3">
            <p className="text-xs font-semibold text-muted-foreground uppercase tracking-wider">Address</p>
            <div className="space-y-2">
              <Label>Country</Label>
              <Combobox
                value={form.country}
                onChange={(val) => {
                  setValue('country', val, { shouldDirty: true })
                  setValue('state', '', { shouldDirty: true })
                }}
                options={countryOptions}
                placeholder="Select country..."
                clearable
              />
            </div>
            <div className="space-y-2">
              <Label>Street Address</Label>
              <Input maxLength={200} placeholder="123 Main Street" {...register('address_line1')} />
            </div>
            <div className="space-y-2">
              <Label>Apt / Suite / Floor <span className="text-muted-foreground font-normal">(optional)</span></Label>
              <Input maxLength={200} placeholder="Suite 100" {...register('address_line2')} />
            </div>
            <div className="grid grid-cols-3 gap-4">
              <div className="space-y-2">
                <Label>City</Label>
                <Input maxLength={100} placeholder="San Francisco" {...register('city')} />
              </div>
              <div className="space-y-2">
                <Label>{stateLabel}</Label>
                {stateOptions ? (
                  <Combobox
                    value={form.state}
                    onChange={(val) => setValue('state', val, { shouldDirty: true })}
                    options={stateOptions}
                    placeholder={`Select ${stateLabel.toLowerCase()}...`}
                    clearable
                  />
                ) : (
                  <Input placeholder={stateLabel} maxLength={100} {...register('state')} />
                )}
              </div>
              <div className="space-y-2">
                <Label>Postal Code</Label>
                <Input placeholder={postalPlaceholder} maxLength={20} {...register('postal_code')} />
              </div>
            </div>
          </div>

          <Separator />

          {/* Tax — status picks a path: standard (tax ID fields), exempt (reason), reverse_charge (buyer's tax ID) */}
          <div className="space-y-3">
            <p className="text-xs font-semibold text-muted-foreground uppercase tracking-wider">Tax</p>

            <div className="space-y-2">
              <Label>Tax status</Label>
              <Controller
                name="tax_status"
                control={control}
                render={({ field }) => (
                  <Select value={field.value} onValueChange={field.onChange}>
                    <SelectTrigger className="w-full"><SelectValue /></SelectTrigger>
                    <SelectContent>
                      <SelectItem value="standard">Standard — tax applied per provider rules</SelectItem>
                      <SelectItem value="exempt">Exempt — no tax (certificate, non-profit, etc.)</SelectItem>
                      <SelectItem value="reverse_charge">Reverse charge — B2B buyer self-accounts</SelectItem>
                    </SelectContent>
                  </Select>
                )}
              />
              <p className="text-xs text-muted-foreground">
                {form.tax_status === 'exempt'
                  ? 'Forces zero tax with the exempt reason printed on the PDF audit trail. Use for a certificate or non-profit the tax provider can\'t see.'
                  : form.tax_status === 'reverse_charge'
                    ? 'Forces zero tax with the reverse-charge legend (buyer self-assesses). For most B2B customers, prefer Standard + a Tax ID — reverse charge is then applied automatically only where it\'s valid. It never applies to a buyer in your own country.'
                    : 'Tax is calculated by your provider. For a B2B customer, add their Tax ID below — reverse charge is then applied automatically where it applies (cross-border).'}
              </p>
            </div>

            {form.tax_status === 'exempt' && (
              <div className="space-y-2">
                <Label>Exempt reason</Label>
                <Input
                  maxLength={500}
                  placeholder="e.g. 501(c)(3) non-profit — certificate on file"
                  {...register('tax_exempt_reason')}
                />
                {formErrors.tax_exempt_reason
                  ? <p className="text-xs text-destructive">{formErrors.tax_exempt_reason.message}</p>
                  : <p className="text-xs text-muted-foreground">Printed on every invoice for this customer as audit trail.</p>}
              </div>
            )}

            {form.tax_status !== 'exempt' && (
              <div className="grid grid-cols-2 gap-4">
                <div className="space-y-2">
                  <Label>Tax ID Type</Label>
                  <Combobox
                    value={form.tax_id_type}
                    onChange={(val) => setValue('tax_id_type', val, { shouldDirty: true })}
                    options={taxIdTypeOptions(form.tax_id_type).map(t => ({
                      value: t.value,
                      label: t.label,
                      keywords: [t.value, t.country].filter(Boolean),
                      prefix: (
                        <span className="text-muted-foreground font-mono text-[11px] w-7 inline-block text-center">
                          {t.country || '—'}
                        </span>
                      ),
                    }))}
                    placeholder="Select type..."
                    clearable
                  />
                </div>
                <div className="space-y-2">
                  <Label>Tax ID</Label>
                  <Input maxLength={50} className="font-mono" placeholder="Identifier" {...register('tax_id')} />
                  {form.tax_id_type && TAX_ID_HINTS[form.tax_id_type] && (
                    <p className="text-xs text-muted-foreground">{TAX_ID_HINTS[form.tax_id_type]}</p>
                  )}
                  {form.tax_status === 'reverse_charge' && (
                    <p className="text-xs text-muted-foreground">Required — reverse charge is only valid when the buyer has a registered tax ID.</p>
                  )}
                </div>
              </div>
            )}
          </div>

          <Separator />

          {/* Billing */}
          <div className="space-y-3">
            <p className="text-xs font-semibold text-muted-foreground uppercase tracking-wider">Billing</p>
            <div className="space-y-2">
              <Label>Currency</Label>
              <Combobox
                value={form.currency}
                onChange={(val) => setValue('currency', val, { shouldDirty: true })}
                options={currencyOptions}
                placeholder="Select currency..."
              />
              <p className="text-xs text-muted-foreground">Overrides the tenant default for invoices on this customer.</p>
            </div>
          </div>

          <DialogFooter>
            <Button type="button" variant="outline" onClick={onClose}>Cancel</Button>
            <Button type="submit" disabled={isSubmitting || !isDirty}>
              {isSubmitting ? <><Loader2 size={14} className="animate-spin mr-2" />Saving...</> : isDirty ? 'Save' : 'No changes'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

/* ─── Dunning Policy Assignment (ADR-036) ─────────────────────────
   Replaces the partial-field-override dialog. Customers either
   inherit the tenant default policy or are explicitly assigned to a
   named policy (Stripe / Lago / Recurly campaigns shape). To create
   or edit policies, operators use the /dunning-policies admin page.
─────────────────────────────────────────────────────────────────── */

function AssignDunningPolicyDialog({ customerId, currentPolicyID, policies, onClose, onSaved }: {
  customerId: string
  currentPolicyID: string
  policies: DunningPolicyWithCount[]
  onClose: () => void
  onSaved: () => void
}) {
  const [selected, setSelected] = useState(currentPolicyID || '__default__')
  const [saving, setSaving] = useState(false)

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    setSaving(true)
    try {
      // '__default__' sentinel maps to empty string on the wire,
      // which the customer service interprets as "clear assignment,
      // fall back to tenant default" (ADR-036).
      const policyId = selected === '__default__' ? '' : selected
      await api.updateCustomer(customerId, { dunning_policy_id: policyId })
      onSaved()
    } catch (err) {
      showApiError(err, 'Failed to save')
    } finally {
      setSaving(false)
    }
  }

  const defaultPolicy = policies.find(p => p.is_default)
  const selectedPolicy = selected === '__default__'
    ? defaultPolicy
    : policies.find(p => p.id === selected)

  return (
    <Dialog open onOpenChange={(open) => { if (!open) onClose() }}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Dunning policy</DialogTitle>
        </DialogHeader>
        <form onSubmit={handleSubmit} noValidate className="space-y-4">
          <div className="space-y-2">
            <Label>Policy</Label>
            {/* Searchable: policies are TENANT DATA and grow unbounded. The
                sentinel "__default__" row stays FIRST so "inherit the tenant
                default" is never something you have to search for. */}
            <Combobox
              value={selected}
              onChange={(val) => setSelected(val ?? '__default__')}
              options={[
                { value: '__default__', label: defaultPolicy ? `Tenant default (${defaultPolicy.name || 'Default'})` : 'Tenant default' },
                ...policies.filter(p => !p.is_default).map(p => ({ value: p.id, label: p.name || '(unnamed policy)' })),
              ]}
              emptyMessage="No policy matches that search."
              triggerClassName="w-full"
            />
            <p className="text-xs text-muted-foreground">
              Manage policies on the <Link to="/dunning-policies" className="underline">Dunning policies</Link> page.
            </p>
          </div>
          {selectedPolicy && (
            <div className="rounded-md border border-border bg-muted/30 p-3 text-xs space-y-1">
              <div className="flex justify-between"><span className="text-muted-foreground">Max retries</span><span>{selectedPolicy.max_retry_attempts}</span></div>
              <div className="flex justify-between"><span className="text-muted-foreground">Grace period</span><span>{selectedPolicy.grace_period_days} days</span></div>
              <div className="flex justify-between"><span className="text-muted-foreground">Retry schedule</span><span>{selectedPolicy.retry_schedule.join(' · ') || '—'}</span></div>
              <div className="flex justify-between"><span className="text-muted-foreground">Final action</span><span>{selectedPolicy.final_action}</span></div>
            </div>
          )}
          <div className="flex justify-end gap-2 pt-2">
            <Button type="button" variant="outline" onClick={onClose}>Cancel</Button>
            <Button type="submit" disabled={saving}>
              {saving ? <><Loader2 size={14} className="animate-spin mr-2" />Saving...</> : 'Save'}
            </Button>
          </div>
        </form>
      </DialogContent>
    </Dialog>
  )
}

/* ─── One-off Invoice Composer ────────────────────────────────── */

// Fixed catalog of line types the operator can pick from. Mirrors the DB
// CHECK on invoice_line_items.line_type. tax/discount are intentionally
// disabled in the composer — those flow through tax provider + coupon
// surfaces, not the manual line editor.
const COMPOSER_LINE_TYPES: { value: string; label: string }[] = [
  { value: 'add_on', label: 'Add-on / one-off charge' },
  { value: 'base_fee', label: 'Base fee' },
  { value: 'usage', label: 'Usage' },
]

// Payment-term presets → net_payment_term_days. Net 30 is the default; the
// backend computes due_at = issued_at + N at create time.
const PAYMENT_TERM_OPTIONS: { value: string; label: string }[] = [
  { value: '0', label: 'Due on receipt' },
  { value: '7', label: 'Net 7' },
  { value: '14', label: 'Net 14' },
  { value: '30', label: 'Net 30' },
  { value: '60', label: 'Net 60' },
]

type ComposerLine = {
  description: string
  quantity: string         // string-bound input; coerced at submit time
  unit_amount_cents: string  // string-bound; cent values, no decimals
  line_type: string
  // Prepaid-credit sale (ADR-078): when set, finalizing the invoice grants
  // this many cents of credits to the customer. add_on lines only; the
  // backend enforces one commit line per invoice.
  sellsCredits: boolean
  commitGrantedCents: string // string-bound; cents
  commitExpiresAt: string    // yyyy-mm-dd; '' = never expires
}

const blankLine = (): ComposerLine => ({
  description: '',
  quantity: '1',
  unit_amount_cents: '',
  line_type: 'add_on',
  sellsCredits: false,
  commitGrantedCents: '',
  commitExpiresAt: '',
})

function NewInvoiceDialog({ customerId, customer, billingProfile, onClose, onCreated }: {
  customerId: string
  customer: Customer
  billingProfile: BillingProfile | null
  onClose: () => void
  onCreated: (result: { invoice: Invoice; sent: boolean }) => void
}) {
  // Default currency: prefer the customer's billing profile currency, else
  // tenant default (USD here is a placeholder until the tenant settings query
  // is plumbed; the operator can switch in one click).
  const initialCurrency = (billingProfile?.currency || 'USD').toUpperCase()
  const [currency, setCurrency] = useState(initialCurrency)
  const [memo, setMemo] = useState('')
  // Service period (optional) — the inclusive dates this invoice covers.
  // Industry-standard (Stripe invoiceitem.period); shows as the invoice
  // "Period" and feeds revenue recognition. Stored half-open (ADR-058).
  const [periodStart, setPeriodStart] = useState('')
  const [periodEnd, setPeriodEnd] = useState('')
  // Payment terms in days from issue → due date. Net 30 is the default;
  // Net 0 means due on receipt. Passed straight through as
  // net_payment_term_days (the backend computes due_at = issued_at + N).
  const [paymentTermDays, setPaymentTermDays] = useState(30)
  const [lines, setLines] = useState<ComposerLine[]>([blankLine()])
  // Anchor date validation on the customer's simulated "now" when clock-pinned:
  // a customer frozen at 2027 must reject a 2026 credit-expiry (past in sim
  // time), which a wall-clock "today" would wrongly accept as "future". Surfaced
  // by the no-restricted-syntax relative-time gate (ADR-086 Phase 4).
  const now = useEffectiveNow(customer?.test_clock_id)
  const [errors, setErrors] = useState<{
    lines?: Record<number, { description?: string; quantity?: string; unit_amount_cents?: string; commit?: string }>
    form?: string
    period?: string
  }>({})
  const [submitting, setSubmitting] = useState<null | 'draft' | 'send'>(null)

  const currencySymbol = getCurrencySymbol(currency)

  const subtotalCents = useMemo(() => {
    return lines.reduce((acc, l) => {
      const q = Number(l.quantity)
      const u = Number(l.unit_amount_cents)
      if (!Number.isFinite(q) || !Number.isFinite(u) || q <= 0 || u < 0) return acc
      return acc + Math.round(q) * Math.round(u)
    }, 0)
  }, [lines])

  const updateLine = (idx: number, patch: Partial<ComposerLine>) => {
    setLines(prev => prev.map((l, i) => (i === idx ? { ...l, ...patch } : l)))
    // Clear inline error for the field being edited so the user gets
    // immediate feedback that the issue is resolved.
    setErrors(prev => {
      if (!prev.lines?.[idx]) return prev
      const lineErrs = { ...prev.lines[idx] }
      for (const k of Object.keys(patch)) delete lineErrs[k as keyof typeof lineErrs]
      return { ...prev, lines: { ...prev.lines, [idx]: lineErrs } }
    })
  }

  const addLine = () => setLines(prev => [...prev, blankLine()])
  const removeLine = (idx: number) => setLines(prev => prev.filter((_, i) => i !== idx))

  // validate returns sanitized lines suitable for the API. Returns null on
  // any validation failure (errors are surfaced via setErrors).
  const validate = (): { description: string; line_type: string; quantity: number; unit_amount_cents: number; commit_granted_cents?: number; commit_expires_at?: string }[] | null => {
    const lineErrors: Record<number, { description?: string; quantity?: string; unit_amount_cents?: string; commit?: string }> = {}
    const cleaned: { description: string; line_type: string; quantity: number; unit_amount_cents: number; commit_granted_cents?: number; commit_expires_at?: string }[] = []
    if (lines.length === 0) {
      setErrors({ form: 'Add at least one line item.' })
      return null
    }
    let commitLines = 0
    lines.forEach((l, i) => {
      const errs: { description?: string; quantity?: string; unit_amount_cents?: string; commit?: string } = {}
      const desc = l.description.trim()
      if (!desc) errs.description = 'Description is required'
      const qty = Number(l.quantity)
      if (!Number.isFinite(qty) || !Number.isInteger(qty) || qty <= 0) {
        errs.quantity = 'Whole number greater than 0'
      }
      const unit = Number(l.unit_amount_cents)
      if (!Number.isFinite(unit) || !Number.isInteger(unit) || unit < 0) {
        errs.unit_amount_cents = 'Whole cents, 0 or more'
      }
      // Backend currently rejects unit_amount_cents <= 0. Surface that
      // upfront so the user fixes it before the round-trip.
      if (Number.isInteger(unit) && unit === 0) {
        errs.unit_amount_cents = 'Must be greater than 0'
      }
      // Prepaid-credit sale: granted cents required and positive; at most
      // one commit line per invoice (the backend enforces both too).
      let commitGranted: number | undefined
      let commitExpires: string | undefined
      if (l.sellsCredits && (l.line_type || 'add_on') === 'add_on') {
        commitLines++
        const granted = Number(l.commitGrantedCents)
        if (!Number.isFinite(granted) || !Number.isInteger(granted) || granted <= 0) {
          errs.commit = 'Credits granted: whole cents, greater than 0'
        } else if (commitLines > 1) {
          errs.commit = 'Only one line per invoice can sell prepaid credits — use a separate invoice'
        } else {
          commitGranted = granted
          if (l.commitExpiresAt) {
            const today = new Date(now).toISOString().slice(0, 10)
            if (l.commitExpiresAt <= today) {
              errs.commit = 'Credit expiry must be a future date'
            } else {
              commitExpires = endOfDayInTZ(l.commitExpiresAt)
            }
          }
        }
      }
      if (Object.keys(errs).length > 0) {
        lineErrors[i] = errs
      } else {
        cleaned.push({
          description: desc,
          line_type: l.line_type || 'add_on',
          quantity: qty,
          unit_amount_cents: unit,
          ...(commitGranted !== undefined ? { commit_granted_cents: commitGranted } : {}),
          ...(commitExpires !== undefined ? { commit_expires_at: commitExpires } : {}),
        })
      }
    })
    if (Object.keys(lineErrors).length > 0) {
      setErrors({ lines: lineErrors })
      return null
    }
    setErrors({})
    return cleaned
  }

  // submit creates the draft invoice with all its line items in a single
  // atomic request, then (when action=send) finalizes + emails it. The
  // create step can no longer half-succeed — header and lines commit
  // together or not at all. Finalize/send remain separate transitions
  // (Stripe-parity); if one of those fails after the draft exists, the
  // partial-success branch below tells the operator what survived.
  const submit = async (action: 'draft' | 'send') => {
    const cleaned = validate()
    if (!cleaned) return
    // Service period (optional): both-or-neither, start on/before end. The
    // operator enters INCLUSIVE dates; store half-open [start, endExclusive)
    // (ADR-058) so the invoice reads "Jun 1 – Jun 30" — endExclusive = end+1d,
    // grounded at start-of-day in tenant TZ (same boundary math as the engine).
    let period: { billing_period_start?: string; billing_period_end?: string } = {}
    if (periodStart || periodEnd) {
      if (!periodStart || !periodEnd) {
        setErrors({ period: 'Enter both a start and end date, or leave both blank.' })
        return
      }
      if (periodStart > periodEnd) {
        setErrors({ period: 'The start date must be on or before the end date.' })
        return
      }
      const [ey, em, ed] = periodEnd.split('-').map(Number)
      const next = new Date(ey, em - 1, ed + 1)
      const endExclusive = `${next.getFullYear()}-${String(next.getMonth() + 1).padStart(2, '0')}-${String(next.getDate()).padStart(2, '0')}`
      period = {
        billing_period_start: startOfDayInTZ(periodStart),
        billing_period_end: startOfDayInTZ(endExclusive),
      }
    }
    setSubmitting(action)
    let invoice: Invoice | null = null
    try {
      invoice = await api.createInvoice({
        customer_id: customerId,
        currency: currency,
        ...period,
        net_payment_term_days: paymentTermDays,
        memo: memo.trim() || undefined,
        line_items: cleaned,
      })

      let sent = false
      if (action === 'send') {
        invoice = await api.finalizeInvoice(invoice.id)
        const recipient = customer.email
        if (recipient) {
          // Best-effort send: a transient SMTP failure shouldn't unwind a
          // successfully finalized invoice. Operators can resend from the
          // invoice detail page if the email never lands.
          try {
            await api.sendInvoiceEmail(invoice.id, recipient)
            sent = true
          } catch {
            toast.warning(
              `Invoice finalized, but email to ${recipient} failed. Resend from the invoice detail page.`,
            )
          }
        } else {
          toast.warning('Invoice finalized, but no email is on file. Add one to send.')
        }
      }

      onCreated({ invoice, sent })
    } catch (err) {
      const message = err instanceof Error ? err.message : 'Failed to create invoice'
      // Partial-success messaging: if we got an invoice ID but failed mid-flight
      // (line item add, finalize, etc.) tell the operator what survived so they
      // can pick it up from the invoice detail page.
      if (invoice) {
        setErrors({
          form: `Draft ${invoice.invoice_number} created, but a later step failed: ${message}. Open the invoice detail page to continue.`,
        })
      } else {
        setErrors({ form: message })
      }
    } finally {
      setSubmitting(null)
    }
  }

  const isBusy = submitting !== null

  return (
    <Dialog open onOpenChange={(open) => { if (!open && !isBusy) onClose() }}>
      {/* eslint-disable-next-line no-restricted-syntax -- DialogContent scrolls as ONE box including its own close button, so a content mask would fade that button. Giving dialogs a scrolling BODY inside a fixed shell is a redesign of every dialog; tracked, not bundled here. */}
      <DialogContent className="sm:max-w-3xl max-h-[90vh] overflow-y-auto">
        <DialogHeader>
          <DialogTitle>New invoice</DialogTitle>
        </DialogHeader>

        <div className="space-y-6">
          {/* Customer summary — confirm the operator is invoicing the right
              account before they commit lines. */}
          <div className="rounded-md border border-border bg-muted/40 px-4 py-3 flex items-center justify-between">
            <div className="min-w-0">
              <p className="text-sm font-medium text-foreground">{customer.display_name}</p>
              <p className="text-xs text-muted-foreground truncate">{customer.email || 'No email on file'}</p>
            </div>
            <Badge variant="outline">{customer.external_id}</Badge>
          </div>

          {/* Currency + payment terms */}
          <div className="grid grid-cols-1 sm:grid-cols-2 gap-4">
            <div className="space-y-2">
              <Label htmlFor="composer-currency">Currency</Label>
              <Select
                items={GEO_CURRENCIES.map(c => ({ value: c.code, label: `${c.symbol} ${c.code} — ${c.label}` }))}
                value={currency}
                onValueChange={(v) => setCurrency((v ?? 'USD').toUpperCase())}
              >
                <SelectTrigger id="composer-currency" className="w-full">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {GEO_CURRENCIES.map(c => (
                    <SelectItem key={c.code} value={c.code}>
                      {c.symbol} {c.code} — {c.label}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
              {billingProfile?.currency && billingProfile.currency.toUpperCase() === currency && (
                <p className="text-xs text-muted-foreground">From customer billing profile.</p>
              )}
            </div>

            <div className="space-y-2">
              <Label htmlFor="composer-terms">Payment terms</Label>
              <Select
                items={PAYMENT_TERM_OPTIONS}
                value={String(paymentTermDays)}
                onValueChange={(v) => setPaymentTermDays(Number(v ?? '30'))}
              >
                <SelectTrigger id="composer-terms" className="w-full">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {PAYMENT_TERM_OPTIONS.map(o => (
                    <SelectItem key={o.value} value={o.value}>{o.label}</SelectItem>
                  ))}
                </SelectContent>
              </Select>
              <p className="text-xs text-muted-foreground">
                {paymentTermDays === 0
                  ? 'Payable on receipt.'
                  : `Due ${paymentTermDays} days after the invoice is issued.`}
              </p>
            </div>
          </div>

          {/* Line items */}
          <div className="space-y-3">
            <div className="flex items-center justify-between">
              <Label>Line items</Label>
              <Button type="button" variant="outline" size="sm" onClick={addLine} disabled={isBusy}>
                <Plus size={14} className="mr-1.5" />
                Add line
              </Button>
            </div>

            <div className="rounded-md border border-border overflow-hidden">
              <div className="grid grid-cols-[1fr_150px_140px_160px_44px] gap-0 bg-muted/40 text-[11px] font-medium uppercase tracking-wide text-muted-foreground">
                <div className="px-3 py-2">Description</div>
                <div className="px-3 py-2">Type</div>
                <div className="px-3 py-2">Qty</div>
                <div className="px-3 py-2">Unit ({currencySymbol.trim() || currency})</div>
                <div className="px-3 py-2"></div>
              </div>
              <div className="divide-y divide-border">
                {lines.map((line, idx) => {
                  const lineErrs = errors.lines?.[idx] || {}
                  const qty = Number(line.quantity)
                  const unit = Number(line.unit_amount_cents)
                  const totalCents = Number.isFinite(qty) && Number.isFinite(unit) && qty > 0 && unit >= 0
                    ? Math.round(qty) * Math.round(unit)
                    : 0
                  return (
                    <div key={idx}>
                    <div className="grid grid-cols-[1fr_150px_140px_160px_44px] items-start gap-0">
                      <div className="px-3 py-2 space-y-1">
                        <Input
                          placeholder="Implementation services"
                          value={line.description}
                          onChange={e => updateLine(idx, { description: e.target.value })}
                          aria-invalid={!!lineErrs.description}
                          maxLength={500}
                          disabled={isBusy}
                        />
                        {lineErrs.description && <p className="text-xs text-destructive">{lineErrs.description}</p>}
                      </div>
                      <div className="px-3 py-2">
                        <Select items={COMPOSER_LINE_TYPES} value={line.line_type} onValueChange={v => updateLine(idx, { line_type: v ?? 'add_on' })}>
                          <SelectTrigger className="w-full">
                            <SelectValue />
                          </SelectTrigger>
                          <SelectContent>
                            {COMPOSER_LINE_TYPES.map(t => (
                              <SelectItem key={t.value} value={t.value}>{t.label}</SelectItem>
                            ))}
                          </SelectContent>
                        </Select>
                      </div>
                      <div className="px-3 py-2 space-y-1">
                        <Input
                          type="number"
                          inputMode="numeric"
                          min={1}
                          step={1}
                          placeholder="1"
                          value={line.quantity}
                          onChange={e => updateLine(idx, { quantity: e.target.value })}
                          aria-invalid={!!lineErrs.quantity}
                          disabled={isBusy}
                        />
                        {lineErrs.quantity && <p className="text-xs text-destructive">{lineErrs.quantity}</p>}
                      </div>
                      <div className="px-3 py-2 space-y-1">
                        <Input
                          type="number"
                          inputMode="numeric"
                          min={0}
                          step={1}
                          placeholder="2500"
                          value={line.unit_amount_cents}
                          onChange={e => updateLine(idx, { unit_amount_cents: e.target.value })}
                          aria-invalid={!!lineErrs.unit_amount_cents}
                          disabled={isBusy}
                        />
                        {lineErrs.unit_amount_cents
                          ? <p className="text-xs text-destructive">{lineErrs.unit_amount_cents}</p>
                          : <p className="text-xs text-muted-foreground">= {formatCents(totalCents, currency)}</p>}
                      </div>
                      <div className="px-3 py-2 flex justify-center">
                        <Button
                          type="button"
                          variant="ghost"
                          size="icon"
                          aria-label={`Remove line ${idx + 1}`}
                          onClick={() => removeLine(idx)}
                          disabled={isBusy || lines.length === 1}
                        >
                          <Trash2 size={14} />
                        </Button>
                      </div>
                    </div>
                    {/* Prepaid-credit sale (ADR-078): finalizing the invoice
                        grants the entered credits to the customer. add_on
                        lines only; one per invoice. */}
                    {(line.line_type || 'add_on') === 'add_on' && (
                      <div className="px-3 pb-2 space-y-1.5">
                        <label className="flex items-center gap-2 text-xs text-muted-foreground cursor-pointer w-fit">
                          <Checkbox
                            checked={line.sellsCredits}
                            onCheckedChange={(v) => updateLine(idx, { sellsCredits: v === true })}
                            disabled={isBusy}
                          />
                          This line sells prepaid credits
                        </label>
                        {line.sellsCredits && (
                          <div className="flex flex-wrap items-center gap-2 pl-6">
                            <Input
                              type="number"
                              inputMode="numeric"
                              min={1}
                              step={1}
                              placeholder="Credits granted (cents)"
                              aria-label={`Line ${idx + 1} credits granted in cents`}
                              className="w-[210px]"
                              value={line.commitGrantedCents}
                              onChange={e => updateLine(idx, { commitGrantedCents: e.target.value })}
                              disabled={isBusy}
                            />
                            <Input
                              type="date"
                              aria-label={`Line ${idx + 1} credits expire`}
                              className="w-auto"
                              value={line.commitExpiresAt}
                              onChange={e => updateLine(idx, { commitExpiresAt: e.target.value })}
                              disabled={isBusy}
                            />
                            <span className="text-xs text-muted-foreground">
                              {Number(line.commitGrantedCents) > 0
                                ? `Grants ${formatCents(Number(line.commitGrantedCents), currency)} of credits when the invoice is finalized.`
                                : 'Expiry optional — blank means the credits never expire.'}
                            </span>
                          </div>
                        )}
                        {lineErrs.commit && <p className="text-xs text-destructive pl-6">{lineErrs.commit}</p>}
                      </div>
                    )}
                    </div>
                  )
                })}
              </div>
            </div>
            <p className="text-xs text-muted-foreground">
              Amounts are in cents (e.g. 2500 = {formatCents(2500, currency)}). Tax is applied per the tenant's tax provider when the invoice finalizes.
            </p>
          </div>

          {/* Service period (optional) — the dates this invoice covers. Shows
              as the invoice "Period" and feeds revenue recognition (Stripe
              invoiceitem.period parity). Inclusive dates; stored half-open so
              it reads "Jun 1 – Jun 30". Blank = a one-off charge with no period. */}
          <div className="space-y-2">
            <Label>Service period (optional)</Label>
            <div className="flex items-center gap-2">
              <Input
                type="date"
                aria-label="Service period start"
                className="w-auto"
                value={periodStart}
                onChange={e => setPeriodStart(e.target.value)}
                disabled={isBusy}
              />
              <span className="text-sm text-muted-foreground">to</span>
              <Input
                type="date"
                aria-label="Service period end"
                className="w-auto"
                value={periodEnd}
                onChange={e => setPeriodEnd(e.target.value)}
                disabled={isBusy}
              />
            </div>
            <p className="text-xs text-muted-foreground">Both dates inclusive. Leave blank for a one-off charge with no period.</p>
            {errors.period && <p className="text-xs text-destructive">{errors.period}</p>}
          </div>

          {/* Memo */}
          <div className="space-y-2">
            <Label htmlFor="composer-memo">Memo (optional)</Label>
            <Textarea
              id="composer-memo"
              placeholder="Notes for the customer — appears on the invoice PDF."
              value={memo}
              onChange={e => setMemo(e.target.value)}
              maxLength={2000}
              disabled={isBusy}
            />
          </div>

          <Separator />

          {/* Subtotal — read-only client-computed sum of lines. Tax is server-
              computed at finalize and reflected on the invoice detail page;
              we don't preview tax here because the tax provider is
              tenant-configurable and a wrong preview is worse than no
              preview. */}
          <div className="space-y-2">
            <div className="flex items-center justify-between">
              <span className="text-sm text-muted-foreground">Subtotal</span>
              <span className="text-sm font-medium text-foreground">{formatCents(subtotalCents, currency)}</span>
            </div>
            <div className="flex items-center justify-between">
              <span className="text-sm text-muted-foreground">Tax</span>
              <span className="text-sm text-muted-foreground">Calculated at finalize</span>
            </div>
            <div className="flex items-center justify-between border-t border-border pt-2">
              <span className="text-sm font-semibold text-foreground">Total (before tax)</span>
              <span className="text-sm font-semibold text-foreground">{formatCents(subtotalCents, currency)}</span>
            </div>
          </div>

          {errors.form && (
            <div className="rounded-md border border-destructive/30 bg-destructive/5 px-3 py-2 text-sm text-destructive">
              {errors.form}
            </div>
          )}
        </div>

        <DialogFooter>
          <Button type="button" variant="outline" onClick={onClose} disabled={isBusy}>
            Cancel
          </Button>
          <Button
            type="button"
            variant="outline"
            onClick={() => submit('draft')}
            disabled={isBusy}
          >
            {submitting === 'draft'
              ? <><Loader2 size={14} className="animate-spin mr-2" />Saving draft...</>
              : 'Save draft'}
          </Button>
          <Button type="button" onClick={() => submit('send')} disabled={isBusy}>
            {submitting === 'send'
              ? <><Loader2 size={14} className="animate-spin mr-2" />Finalizing...</>
              : 'Save & send'}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

// PublicCostDashboardCard surfaces the rotate-and-share flow for the
// public cost-dashboard token (ADR-031). Rotation mints a fresh
// `vlx_pcd_…` token that the operator can paste into an iframe src /
// fetch call from the customer's app — no API key needed. Rotating
// invalidates the previous token immediately (read-only surface; the
// rotate intent is "stop the previous URL right now").
function PublicCostDashboardCard({ customerId }: { customerId: string }) {
  const [latest, setLatest] = useState<{ token: string; public_url: string } | null>(null)
  const [rotating, setRotating] = useState(false)

  const onRotate = async () => {
    setRotating(true)
    try {
      const res = await api.rotateCostDashboardToken(customerId)
      setLatest(res)
      toast.success('Cost-dashboard URL rotated')
    } catch (err) {
      showApiError(err, 'Failed to rotate cost-dashboard token')
    } finally {
      setRotating(false)
    }
  }

  const copy = (s: string) => {
    void navigator.clipboard.writeText(s)
    toast.success('Copied')
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-sm">Public cost-dashboard URL</CardTitle>
      </CardHeader>
      <CardContent className="space-y-3">
        <p className="text-xs text-muted-foreground">
          Generate a shareable URL the customer can embed in their own app to view their cycle cost
          breakdown without an API key. Rotation invalidates any previous URL immediately.
        </p>
        {latest ? (
          <div className="rounded-md border border-border bg-muted/50 p-3 space-y-2">
            <div className="flex items-center justify-between gap-2">
              <span className="text-xs text-muted-foreground">URL</span>
              <Button size="sm" variant="outline" onClick={() => copy(latest.public_url)}>Copy</Button>
            </div>
            <code className="block text-[11px] font-mono break-all text-foreground">{latest.public_url}</code>
            <p className="text-[11px] text-amber-700 dark:text-amber-400">
              Save this URL now — Velox doesn't show it again after navigation. Re-rotate to mint a new one.
            </p>
            {/* The server composes an absolute URL only when VELOX_API_BASE_URL
                is set; unset, public_url is a bare path and the thing the
                operator copies cannot be opened by their customer. The server
                warns about this at boot, but boot logs are not where the person
                clicking Copy is looking — so say it here, next to the value
                that's wrong. Deliberately NOT auto-prefixed with the dashboard's
                own origin: the API origin is a different host in any real
                deployment, so guessing would hand over a confidently broken URL
                instead of an obviously incomplete one. */}
            {!/^https?:\/\//i.test(latest.public_url) && (
              <p className="text-[11px] text-amber-700 dark:text-amber-400">
                This is a path, not a full URL — your customer can't open it as-is. Set{' '}
                <code className="font-mono">VELOX_API_BASE_URL</code> to your public API origin and
                re-generate, or prefix the path with that origin yourself.
              </p>
            )}
          </div>
        ) : null}
        <Button onClick={onRotate} disabled={rotating} size="sm">
          {rotating ? <><Loader2 size={14} className="animate-spin mr-2" />Rotating…</> : (latest ? 'Rotate again' : 'Generate URL')}
        </Button>
      </CardContent>
    </Card>
  )
}
