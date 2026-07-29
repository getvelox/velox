import { useState } from 'react'
import { usePageTitle } from '@/hooks/usePageTitle'
import { useParams, Link, useNavigate } from 'react-router-dom'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'
import { formatInTimeZone, fromZonedTime } from 'date-fns-tz'
import { invalidateMoneySurfaces } from '@/lib/invalidateMoney'
import { api, formatCents, formatDate, formatDateTime, getTenantTimezone, type Subscription, type SubscriptionItem, type Plan, type ItemChangeResult } from '@/lib/api'
import { formatCivilDate, formatCivilPeriod, startOfDayInTZ } from '@/lib/dates'
import { showApiError } from '@/lib/formErrors'
import { Layout } from '@/components/Layout'
import { Combobox, type ComboboxOption } from '@/components/Combobox'
import { TestClockBanner } from '@/components/TestClockBanner'
import { ExpiryBadge } from '@/components/ExpiryBadge'
import { effectiveNow } from '@/lib/effectiveNow'
import { cn } from '@/lib/utils'
import { statusBadgeVariant } from '@/lib/status'
import { subscriptionWillAutoCharge, hasUsableCard } from '@/lib/paymentReadiness'

import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Label } from '@/components/ui/label'
import { Checkbox } from '@/components/ui/checkbox'
import { Input } from '@/components/ui/input'
import { DatePicker } from '@/components/ui/date-picker'
import { DateTimePicker } from '@/components/ui/datetime-picker'
import {
  Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle, DialogDescription,
} from '@/components/ui/dialog'
import {
  AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent,
  AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle,
} from '@/components/ui/alert-dialog'
import { TypedConfirmDialog } from '@/components/TypedConfirmDialog'
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from '@/components/ui/table'
import { Loader2, Plus, HelpCircle, History, AlertTriangle } from 'lucide-react'
import { CopyButton } from '@/components/CopyButton'
import { DetailBreadcrumb } from '@/components/DetailBreadcrumb'
import { DetailSkeleton } from '@/components/ui/DetailSkeleton'
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip'

const statusVariant = statusBadgeVariant

// trimDecimal strips trailing zeros from a fractional decimal string while
// leaving integers untouched ("1000.000000000000" → "1000", "3.140" → "3.14",
// "1000" → "1000"). Backend usage_gte arrives as a NUMERIC(38,12) string per
// ADR-005; surface it without the bookkeeping zeros.
function trimDecimal(s: string): string {
  if (!s.includes('.')) return s
  return s.replace(/0+$/, '').replace(/\.$/, '')
}

type ItemDialogState =
  | { kind: 'add' }
  | { kind: 'change-plan'; item: SubscriptionItem }
  | { kind: 'change-quantity'; item: SubscriptionItem }
  | { kind: 'remove'; item: SubscriptionItem }
  | null

export default function SubscriptionDetailPage() {
  const { id } = useParams<{ id: string }>()
  const navigate = useNavigate()
  const queryClient = useQueryClient()

  const [showCancelConfirm, setShowCancelConfirm] = useState(false)
  const [showCancelChoice, setShowCancelChoice] = useState(false)
  // Default to the less-destructive option ("at period end") — Stripe / Linear
  // / GitHub all default to the safer choice on destructive flows so a misclick
  // doesn't escalate. Operator picks "immediately" deliberately.
  const [cancelMode, setCancelMode] = useState<'period_end' | 'immediately'>('period_end')
  const [showPauseConfirm, setShowPauseConfirm] = useState(false)
  const [showExtendTrial, setShowExtendTrial] = useState(false)
  const [extendTrialDate, setExtendTrialDate] = useState('')
  const [extendTrialTime, setExtendTrialTime] = useState('')
  const [pauseResumesAt, setPauseResumesAt] = useState('')
  const [itemDialog, setItemDialog] = useState<ItemDialogState>(null)
  const [showThresholdsDialog, setShowThresholdsDialog] = useState(false)

  const { data: sub, isLoading, error: loadError, refetch } = useQuery({
    queryKey: ['subscription', id],
    queryFn: () => api.getSubscription(id!),
    enabled: !!id,
  })

  const { data: customer } = useQuery({
    queryKey: ['customer', sub?.customer_id],
    queryFn: () => api.getCustomer(sub!.customer_id),
    enabled: !!sub?.customer_id,
  })

  // Shift-left no-card warning (2026-07-22 audit, P1-5): fetch the
  // customer's payment methods only when this subscription actually
  // auto-charges — a paused/terminal sub won't, so no query and no
  // warning. One existing call, no new endpoint.
  const subAutoCharges = !!sub && subscriptionWillAutoCharge(sub)
  const { data: subCustomerPMs } = useQuery({
    queryKey: ['customer-payment-methods', sub?.customer_id],
    queryFn: () => api.listCustomerPaymentMethods(sub!.customer_id),
    enabled: !!sub?.customer_id && subAutoCharges,
  })
  const subNeedsCard = subAutoCharges && !!subCustomerPMs && !hasUsableCard(subCustomerPMs.data ?? [])

  const { data: plansData } = useQuery({
    queryKey: ['plans'],
    queryFn: () => api.listPlans(),
    select: (res) => res.data,
  })

  const planById = (planID: string): Plan | undefined => plansData?.find(p => p.id === planID)
  const items = sub?.items ?? []

  const { data: invoices } = useQuery({
    queryKey: ['subscription-invoices', id],
    queryFn: () => api.listInvoices('subscription_id=' + id).then(r => r.data),
    enabled: !!id,
  })

  // Test-clock frozen_time for relative-time badges (Trial ends in N
  // days). Without it the trial-end countdown reads from wall-clock,
  // not the simulation time the engine bills under.
  const { data: testClock } = useQuery({
    queryKey: ['test-clock', sub?.test_clock_id],
    queryFn: () => api.getTestClock(sub!.test_clock_id!),
    enabled: !!sub?.test_clock_id,
  })

  const { data: preview, error: previewError } = useQuery({
    queryKey: ['subscription-preview', id],
    queryFn: () => api.invoicePreview(id!),
    enabled: !!id,
    retry: false,
  })

  // Simulated "now" as a Date (the clock's frozen_time when this sub is pinned,
  // else wall time) — the single anchor for the lifecycle timeline's past/future
  // dots and the date-picker floors below. A sub frozen in the past must not
  // offer wall-clock dates its engine would treat as far-future.
  const nowDate = new Date(effectiveNow(sub?.test_clock_id ? testClock?.frozen_time : undefined))

  // Activity timeline (T0-18) — chronological feed of lifecycle events
  // pulled from the audit log. Separate query key from the period-
  // progress visualization further down; the two are unrelated despite
  // both being called "timeline" in local parlance.
  const { data: activityTimelineData, isError: activityTimelineError } = useQuery({
    queryKey: ['subscription-activity-timeline', id],
    queryFn: () => api.getSubscriptionTimeline(id!).then(r => ({ events: r.events || [], truncated: !!r.truncated })),
    enabled: !!id,
  })
  const activityTimeline = activityTimelineData?.events ?? []
  // Backend caps the feed at the 100 newest audit rows; when true, the
  // earliest events (create/activate) are missing from the top of the list.
  const activityTimelineTruncated = activityTimelineData?.truncated ?? false
  // The backend now 500s when the audit store is down (it used to return an
  // empty 200) — the whole point is that "couldn't load history" must not
  // render identically to "no history", so the error keeps the card visible
  // with an explicit failure state instead of hiding it.

  const invalidateAll = () => {
    queryClient.invalidateQueries({ queryKey: ['subscription', id] })
    queryClient.invalidateQueries({ queryKey: ['subscription-invoices', id] })
    queryClient.invalidateQueries({ queryKey: ['subscription-preview', id] })
    queryClient.invalidateQueries({ queryKey: ['subscription-activity-timeline', id] })
    queryClient.invalidateQueries({ queryKey: ['subscriptions'] })
    // Cross-page caches that surface this subscription's state.
    // CustomerDetail's overview + sub list show a sub badge / status
    // pill; the Subscriptions list-with-test-clock-chip uses the same
    // base query. Without these, pausing/canceling here left the
    // CustomerDetail page stale until a hard refresh.
    if (sub?.customer_id) {
      queryClient.invalidateQueries({ queryKey: ['customer-overview', sub.customer_id] })
      queryClient.invalidateQueries({ queryKey: ['customer-subscriptions', sub.customer_id] })
    }
    queryClient.invalidateQueries({ queryKey: ['subscriptions-for-test-clock-chip'] })
    queryClient.invalidateQueries({ queryKey: ['subscriptions-for-trial-badge'] })
    // Plan/quantity changes can credit the customer's balance via
    // proration — refresh the balance-bearing families (2026-07-21 audit).
    invalidateMoneySurfaces(queryClient, { customerId: sub?.customer_id })
  }

  const activateMutation = useMutation({
    mutationFn: () => api.activateSubscription(id!),
    onSuccess: () => { invalidateAll(); toast.success('Subscription activated') },
    onError: (err) => showApiError(err, 'Failed to activate'),
  })

  const cancelMutation = useMutation({
    mutationFn: () => api.cancelSubscription(id!),
    onSuccess: () => { invalidateAll(); toast.success('Subscription canceled'); setShowCancelConfirm(false) },
    onError: (err) => showApiError(err, 'Failed to cancel'),
  })

  const scheduleCancelMutation = useMutation({
    mutationFn: () => api.scheduleSubscriptionCancel(id!, { at_period_end: true }),
    onSuccess: (updated) => {
      invalidateAll()
      // ADR-069: on a trialing sub the flag cancels FREE at trial end — the
      // old hardcoded 'at period end' toast promised the wrong date.
      toast.success(
        updated?.status === 'trialing' && updated?.cancel_effective_at
          ? `Cancellation scheduled at trial end (${formatDate(updated.cancel_effective_at)}) — the customer won't be charged`
          : 'Cancellation scheduled at period end',
      )
      setShowCancelChoice(false)
    },
    onError: (err) => showApiError(err, 'Failed to schedule cancellation'),
  })

  const clearScheduledCancelMutation = useMutation({
    mutationFn: () => api.clearScheduledSubscriptionCancel(id!),
    onSuccess: () => { invalidateAll(); toast.success('Scheduled cancellation cleared') },
    onError: (err) => showApiError(err, 'Failed to clear scheduled cancellation'),
  })

  const pauseCollectionMutation = useMutation({
    mutationFn: () => {
      const body: { behavior: 'keep_as_draft'; resumes_at?: string } = { behavior: 'keep_as_draft' }
      // DatePicker emits yyyy-MM-dd (date-only). Interpret it as start-of-day
      // in the TENANT timezone (not the browser's): the backend's auto-resume
      // scan compares the instant literally, and every other operator-picked
      // civil date (extend-trial, credit expiry) anchors in tenant TZ. A
      // browser-local parse shifted the resume by the operator/tenant offset.
      if (pauseResumesAt.trim() !== '') {
        body.resumes_at = startOfDayInTZ(pauseResumesAt)
      }
      return api.pauseSubscriptionCollection(id!, body)
    },
    onSuccess: () => {
      invalidateAll()
      toast.success(pauseResumesAt ? 'Collection paused — auto-resumes on the date you picked' : 'Collection paused — invoices will draft only')
      setShowPauseConfirm(false)
      setPauseResumesAt('')
    },
    onError: (err) => showApiError(err, 'Failed to pause collection'),
  })

  const resumeCollectionMutation = useMutation({
    mutationFn: () => api.resumeSubscriptionCollection(id!),
    onSuccess: () => { invalidateAll(); toast.success('Collection resumed') },
    onError: (err) => showApiError(err, 'Failed to resume collection'),
  })

  const endTrialMutation = useMutation({
    mutationFn: () => api.endSubscriptionTrial(id!),
    onSuccess: () => { invalidateAll(); toast.success('Trial ended — subscription is now active') },
    onError: (err) => showApiError(err, 'Failed to end trial'),
  })

  const extendTrialMutation = useMutation({
    mutationFn: (trialEnd: string) => api.extendSubscriptionTrial(id!, { trial_end: trialEnd }),
    onSuccess: () => {
      invalidateAll()
      toast.success('Trial extended')
      setShowExtendTrial(false)
      setExtendTrialDate('')
      setExtendTrialTime('')
    },
    onError: (err) => showApiError(err, 'Failed to extend trial'),
  })

  const cancelPendingMutation = useMutation({
    mutationFn: (itemID: string) => api.cancelPendingItemChange(id!, itemID),
    onSuccess: () => { invalidateAll(); toast.success('Pending plan change canceled') },
    onError: (err) => showApiError(err, 'Failed to cancel pending change'),
  })

  const acting =
    activateMutation.isPending ||
    cancelMutation.isPending ||
    scheduleCancelMutation.isPending ||
    clearScheduledCancelMutation.isPending ||
    pauseCollectionMutation.isPending ||
    resumeCollectionMutation.isPending ||
    endTrialMutation.isPending ||
    extendTrialMutation.isPending

  const loading = isLoading
  const error = loadError instanceof Error ? loadError.message : loadError ? String(loadError) : null

  usePageTitle(sub?.display_name)

  if (loading) {
    return (
      <Layout>
        <DetailSkeleton to="/subscriptions" parentLabel="Subscriptions" />
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

  if (!sub) {
    return (
      <Layout>
        <p className="text-sm text-muted-foreground py-16 text-center">Subscription not found</p>
      </Layout>
    )
  }

  const firstItemPlan = items[0] ? planById(items[0].plan_id) : undefined

  return (
    <Layout>
      <DetailBreadcrumb to="/subscriptions" parentLabel="Subscriptions" currentLabel={sub.display_name} />

      {/* Header */}
      <div className="flex items-center justify-between mb-6">
        <div>
          <div className="flex items-center gap-2">
            <h1 className="text-2xl font-semibold text-foreground">{sub.display_name}</h1>
            {/* Test-clock chip lives next to the title alongside the
                CustomerDetail / InvoiceDetail treatment. Classification
                chips (which scope this resource belongs to) sit on the
                left; lifecycle state pills (active/paused/canceled) sit
                on the right with the action buttons. Stripe / Linear /
                Vercel all separate the two zones this way. */}
          </div>
          <div className="flex items-center gap-2 mt-1">
            <span className="text-xs text-muted-foreground font-mono bg-muted px-2 py-0.5 rounded">{sub.id}</span>
            <CopyButton text={sub.id} />
            <span className="text-xs text-muted-foreground font-mono bg-muted px-2 py-0.5 rounded">{sub.code}</span>
          </div>
        </div>
        <div className="flex items-center gap-2">
          <Button variant="outline"
            onClick={() => navigate(`/audit-log?resource_type=subscription&resource_id=${sub.id}`)}>
            <History size={16} className="mr-1.5" />
            Audit log
          </Button>
          {sub.status === 'draft' && (
            <Button onClick={() => activateMutation.mutate()} disabled={acting}>
              {activateMutation.isPending ? <><Loader2 size={14} className="animate-spin mr-1.5" />Activating…</> : 'Activate'}
            </Button>
          )}
          {sub.status === 'active' && (
            <>
              {/* Pause hidden when collection is already paused — the
                  Resume Collection banner below owns the toggle once a
                  pause is in effect. Stripe-style soft pause keeps
                  status=active, so without this guard both the Pause
                  button and the Resume Collection banner would render
                  simultaneously. */}
              {!sub.pause_collection && (
                <Button variant="outline" className="border-amber-300 text-amber-600 hover:bg-amber-50" onClick={() => setShowPauseConfirm(true)} disabled={acting}>
                  Pause
                </Button>
              )}
              <Button variant="outline" className="border-destructive text-destructive hover:bg-destructive/10" onClick={() => setShowCancelChoice(true)} disabled={acting}>
                Cancel
              </Button>
            </>
          )}
          {sub.status === 'trialing' && (
            <>
              <Button variant="outline" onClick={() => {
                const tz = getTenantTimezone() || 'UTC'
                const seed = sub.trial_end_at ? new Date(sub.trial_end_at) : nowDate
                seed.setDate(seed.getDate() + 7)
                setExtendTrialDate(formatInTimeZone(seed, tz, 'yyyy-MM-dd'))
                setExtendTrialTime(formatInTimeZone(seed, tz, 'HH:mm'))
                setShowExtendTrial(true)
              }} disabled={acting}>
                Extend trial
              </Button>
              {/* Ending a trial with a pending cancel schedule 409s (the
                  schedule would silently become a paid-period cancel) —
                  surface the conflict BEFORE the click. */}
              <Button
                variant="outline"
                className="border-primary text-primary hover:bg-primary/10"
                onClick={() => endTrialMutation.mutate()}
                disabled={acting || !!(sub.cancel_at_period_end || sub.cancel_at)}
                title={sub.cancel_at_period_end || sub.cancel_at ? 'Clear the scheduled cancellation first' : undefined}
              >
                {endTrialMutation.isPending ? <><Loader2 size={14} className="animate-spin mr-1.5" />Ending trial…</> : 'End trial now'}
              </Button>
              <Button variant="outline" className="border-destructive text-destructive hover:bg-destructive/10" onClick={() => setShowCancelChoice(true)} disabled={acting}>
                Cancel
              </Button>
            </>
          )}
          <Badge variant={statusVariant(sub.status)}>{sub.status}</Badge>
          {sub.status === 'trialing' && sub.trial_end_at && (
            <ExpiryBadge expiresAt={sub.trial_end_at} label="Trial ends" warningDays={3} now={effectiveNow(sub.test_clock_id ? testClock?.frozen_time : undefined)} />
          )}
        </div>
      </div>

      {/* Test-clock banner — sets expectation that period_end / next_billing /
          dunning timestamps below may reflect simulated time, not wall-clock.
          The header already carries a small badge linking to the clock; the
          banner is the explainer. */}
      {sub.test_clock_id && (
        <TestClockBanner testClockId={sub.test_clock_id} />
      )}

      {/* No card on file — this subscription bills automatically. Fires
          before the first invoice (the reactive no-payment-method invoice
          banner takes over after finalize). */}
      {subNeedsCard && (
        <div className="mb-6 rounded-md border border-amber-300 dark:border-amber-500/30 bg-amber-50 dark:bg-amber-500/10 px-4 py-3">
          <div className="flex items-start justify-between gap-3">
            <div className="flex items-start gap-2.5">
              <AlertTriangle size={16} className="text-amber-500 mt-0.5 shrink-0" />
              <div className="text-sm text-amber-900 dark:text-amber-300">
                <span className="font-medium">No card on file</span>
                <p className="text-xs text-amber-800/90 dark:text-amber-400/90 mt-0.5">
                  This subscription bills automatically, but the customer has no payment method saved. Add one before the next renewal so the charge goes through.
                </p>
              </div>
            </div>
            {sub.customer_id && (
              <Button size="sm" variant="outline" className="shrink-0" onClick={() => navigate(`/customers/${sub.customer_id}`)}>
                Add payment method
              </Button>
            )}
          </div>
        </div>
      )}

      {/* Scheduled cancellation banner. Surfaces both modes (at_period_end
          and explicit cancel_at) with an obvious Undo so an operator who
          set this in error can clear it without guessing the API surface. */}
      {(sub.cancel_at_period_end || sub.cancel_at) && (
        <div className="mb-6 rounded-md border border-amber-300 bg-amber-50 px-4 py-3 flex items-center justify-between">
          <div className="text-sm text-amber-900">
            <span className="font-medium">Cancellation scheduled</span>
            {/* cancel_effective_at is the backend's authoritative answer —
                on a TRIALING sub the flag means trial end (free), NOT the
                first paid period's end, so deriving the date client-side
                showed the wrong day (ADR-069). */}
            {sub.cancel_effective_at ? (
              sub.status === 'trialing' && sub.cancel_at_period_end
                ? <> — at trial end ({formatDateTime(sub.cancel_effective_at)}); the customer won't be charged</>
                : <> — {sub.cancel_at_period_end ? 'at end of current period' : 'on'} {formatDateTime(sub.cancel_effective_at)}</>
            ) : null}
          </div>
          <Button
            variant="outline"
            size="sm"
            className="border-amber-400 text-amber-900 hover:bg-amber-100"
            onClick={() => clearScheduledCancelMutation.mutate()}
            disabled={acting}
          >
            {clearScheduledCancelMutation.isPending ? <><Loader2 size={14} className="animate-spin mr-1.5" />Undoing…</> : 'Undo'}
          </Button>
        </div>
      )}

      {/* Collection-paused banner. Distinct from the hard pause (status=paused)
          — sub stays active but invoices generate as drafts and skip
          finalize/charge/dunning until resumed. */}
      {sub.pause_collection && (
        <div className="mb-6 rounded-md border border-blue-300 bg-blue-50 px-4 py-3 flex items-center justify-between">
          <div className="text-sm text-blue-900">
            <span className="font-medium">Collection paused</span>
            {' — invoices will generate as drafts and skip charge until resumed'}
            {sub.pause_collection.resumes_at && (
              <> (auto-resumes {formatDateTime(sub.pause_collection.resumes_at)})</>
            )}
          </div>
          <Button
            variant="outline"
            size="sm"
            className="border-blue-400 text-blue-900 hover:bg-blue-100"
            onClick={() => resumeCollectionMutation.mutate()}
            disabled={acting}
          >
            {resumeCollectionMutation.isPending ? <><Loader2 size={14} className="animate-spin mr-1.5" />Resuming…</> : 'Resume collection'}
          </Button>
        </div>
      )}

      {/* Subscription Timeline */}
      {(() => {
        const timelinePoints: { label: string; date: string; isPast: boolean }[] = []

        // "now" baseline must read from the test clock for clock-pinned
        // subs — otherwise every dot reads as past for any sub whose
        // sim time is older than wall-clock (the common case during a
        // catchup demo). nowDate is the sub's simulated now (see above).
        const now = nowDate

        timelinePoints.push({
          label: 'Created',
          date: formatDate(sub.created_at),
          isPast: true,
        })

        if (sub.status === 'trialing') {
          // Trialing subs get distinct dots: Trial ends + First charge.
          // Pre-fix the period_start/period_end labels read "Last Period"
          // and "Period End" for trialing subs — misleading, because
          // there's no "last" period yet (the sub is still trialing) and
          // the period_end IS the first chargeable cycle close. Stripe /
          // Lago surface trial dates explicitly in trialing state.
          if (sub.trial_end_at) {
            const trialEnd = new Date(sub.trial_end_at)
            timelinePoints.push({
              label: 'Trial ends',
              date: formatDate(sub.trial_end_at),
              isPast: trialEnd <= now,
            })
          }
          // First charge fires at trial_end_at for in_advance plans (Phase 0.5's
          // BillOnCreate covers the just-opened paid period at activation) and at
          // the post-trial cycle close (next_billing_at) for in_arrears. Reading
          // base_bill_timing from the first item's plan because the sub itself
          // doesn't expose it directly; mixed-interval-or-timing items on one sub
          // are rejected at Create / AddItem / UpdateItem so first-item-wins is
          // the durable answer.
          const firstItemPlan = items.length > 0 ? planById(items[0].plan_id) : undefined
          const inAdvance = firstItemPlan?.base_bill_timing === 'in_advance'
          const firstChargeAt = inAdvance ? sub.trial_end_at : sub.next_billing_at
          if (firstChargeAt) {
            const firstCharge = new Date(firstChargeAt)
            timelinePoints.push({
              label: 'First charge',
              date: formatDate(firstChargeAt),
              isPast: firstCharge <= now,
            })
          }
        } else if (
          // A trial canceled at trial end never billed its "first paid
          // period" — the bar shows the trial instead of phantom period
          // dates (ADR-069).
          sub.status === 'canceled' && sub.trial_end_at && sub.canceled_at &&
          new Date(sub.canceled_at) <= new Date(sub.trial_end_at)
        ) {
          if (sub.trial_start_at) {
            timelinePoints.push({
              label: 'Trial start',
              date: formatDate(sub.trial_start_at),
              isPast: true,
            })
          }
          timelinePoints.push({
            label: 'Canceled at trial end',
            date: formatDate(sub.canceled_at),
            isPast: true,
          })
        } else {
          if (sub.current_billing_period_start) {
            const periodStart = new Date(sub.current_billing_period_start)
            timelinePoints.push({
              label: sub.status === 'active' ? 'Period Start' : 'Last Period',
              // Inclusive period-start boundary — render its civil date in the
              // org billing TZ (formatDate, NOT formatCivilDate: no step-back),
              // so it matches the Period range. A sub bills in the one org TZ (=
              // the live tenant setting, ADR-077), so undefined → tenant TZ is
              // the anchor.
              date: formatDate(sub.current_billing_period_start, undefined),
              isPast: periodStart <= now,
            })
          }

          if (sub.current_billing_period_end) {
            const periodEnd = new Date(sub.current_billing_period_end)
            timelinePoints.push({
              label: 'Period End',
              // Last day the period covers (inclusive), not the exclusive
              // boundary instant — distinct from the "Next Billing" dot and
              // consistent with the "Current period" range below (ADR-058).
              date: formatCivilDate(sub.current_billing_period_end, undefined),
              isPast: periodEnd <= now,
            })
          }

          if (sub.cancel_at_period_end || sub.cancel_at) {
            // A scheduled cancel replaces "Next Billing" — billing the
            // next period is exactly what won't happen. The bar reads
            // Period Start → Period End → Cancels.
            // cancel_effective_at is the backend's authoritative answer
            // (ADR-069); re-deriving from the flag shows the wrong date on
            // trialing subs.
            const cancelAt = sub.cancel_effective_at || sub.cancel_at || sub.current_billing_period_end
            if (cancelAt) {
              timelinePoints.push({
                label: 'Cancels',
                date: formatDate(cancelAt),
                isPast: new Date(cancelAt) <= now,
              })
            }
          } else if (sub.pause_collection) {
            // Collection paused: invoices draft but don't charge — the
            // honest next point is the resume, not "Next Billing". With no
            // resume date (keep_as_draft, e.g. dunning-exhausted) the pause
            // is open-ended, so say "Indefinite" — a bare "—" in the date
            // column reads as missing data next to the "paused" banner.
            timelinePoints.push({
              label: sub.pause_collection.resumes_at ? 'Collection resumes' : 'Collection paused',
              date: sub.pause_collection.resumes_at ? formatDate(sub.pause_collection.resumes_at) : 'Indefinite',
              isPast: false,
            })
          } else if (sub.next_billing_at) {
            const nextBilling = new Date(sub.next_billing_at)
            timelinePoints.push({
              label: 'Next Billing',
              date: formatDate(sub.next_billing_at),
              isPast: nextBilling <= now,
            })
          }
        }

        if (timelinePoints.length < 2) return null

        const lastPastIndex = timelinePoints.reduce((acc, p, i) => (p.isPast ? i : acc), -1)
        const progressPercent = lastPastIndex >= 0
          ? (lastPastIndex / (timelinePoints.length - 1)) * 100
          : 0

        return (
          <Card className="mt-6 mb-6">
            <CardContent className="py-5 px-6">
              <div className="relative">
                <div className="absolute left-[calc(0%+6px)] right-[calc(0%+6px)] top-[11px] h-[2px] bg-border" />
                <div
                  className="absolute left-[calc(0%+6px)] top-[11px] h-[2px] bg-primary transition-all duration-300"
                  style={{ width: `calc(${progressPercent}% - 12px)` }}
                />

                <div className="relative flex justify-between">
                  {timelinePoints.map((point, i) => (
                    <div key={i} className="flex flex-col items-center" style={{ width: 90 }}>
                      <div className={cn(
                        'w-6 h-6 rounded-full flex items-center justify-center',
                        point.isPast
                          ? 'bg-primary'
                          : 'bg-background border-2 border-border'
                      )}>
                        {point.isPast && (
                          <svg width="10" height="10" viewBox="0 0 10 10" fill="none">
                            <path d="M2 5L4.5 7.5L8 3" stroke="white" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" />
                          </svg>
                        )}
                      </div>
                      <p className={cn(
                        'text-xs mt-2 text-center',
                        point.isPast ? 'font-medium text-foreground' : 'text-muted-foreground'
                      )}>{point.label}</p>
                      <p className="text-[11px] text-muted-foreground mt-0.5">{point.date}</p>
                    </div>
                  ))}
                </div>
              </div>
            </CardContent>
          </Card>
        )
      })()}

      {/* Key metrics */}
      <Card>
        <CardContent className="p-0">
          <div className="flex divide-x divide-border">
            <div className="flex-1 px-6 py-4">
              <p className="text-sm text-muted-foreground">Customer</p>
              {customer ? (
                <Link to={`/customers/${customer.id}`} className="text-lg font-semibold text-primary hover:underline mt-1 block">
                  {customer.display_name}
                </Link>
              ) : (
                <p className="text-lg font-semibold text-foreground mt-1">{sub.customer_id.slice(0, 8)}...</p>
              )}
            </div>
            <div className="flex-1 px-6 py-4">
              <p className="text-sm text-muted-foreground">Items</p>
              {items.length === 0 ? (
                <p className="text-lg font-semibold text-foreground mt-1">—</p>
              ) : items.length === 1 && firstItemPlan ? (
                <>
                  <Link to={`/plans/${firstItemPlan.id}`} className="text-lg font-semibold text-primary hover:underline mt-1 block">
                    {firstItemPlan.name}
                  </Link>
                  <p className="text-xs text-muted-foreground mt-0.5">
                    {items[0].quantity > 1 ? `${items[0].quantity} × ` : ''}
                    {formatCents(firstItemPlan.base_amount_cents)}/{firstItemPlan.billing_interval === 'yearly' ? 'yr' : 'mo'}
                  </p>
                </>
              ) : (
                <p className="text-lg font-semibold text-foreground mt-1">{items.length} items</p>
              )}
            </div>
            <div className="flex-1 px-6 py-4">
              {sub.status === 'trialing' && sub.trial_start_at && sub.trial_end_at ? (
                <>
                  <p className="text-sm text-muted-foreground">Trial period</p>
                  <p className="text-lg font-semibold text-foreground mt-1">
                    {formatDate(sub.trial_start_at)} {'\u2014'} {formatDate(sub.trial_end_at)}
                  </p>
                  {sub.current_billing_period_start && sub.current_billing_period_end && (
                    <p className="text-xs text-muted-foreground mt-1">
                      First billing: {sub.current_billing_period_display}
                    </p>
                  )}
                </>
              ) : (
                <>
                  {/* Industry-standard pattern (Recurly "Renews On",
                      Chargebee "Next Billing Date", Stripe's
                      current_period_end). Surfacing the next-renewal
                      date as a concrete primary label eliminates the
                      "is this a bug?" confusion when the period
                      anchor doesn't match `billing_time` (e.g., a
                      sub that started yearly and swapped to monthly
                      preserves its day-of-month anchor \u2014 industry-
                      standard behavior, but the dates alone don't
                      explain it). The full period range stays as a
                      muted secondary line for completeness. */}
                  <p className="text-sm text-muted-foreground">Billing cycle</p>
                  <p className="text-lg font-semibold text-foreground mt-1">
                    {/* A scheduled cancel has to win here for the same
                        reason it replaces "Next Billing" in the timeline
                        bar above: renewal is exactly what won't happen.
                        Pre-fix this row said "Renews <date>" on the very
                        date the bar said "Cancels <date>" \u2014 one page
                        stating both outcomes for one date. Uses the
                        backend's authoritative cancel_effective_at
                        (ADR-069), matching the bar. */}
                    {(() => {
                      const cancelAt =
                        sub.cancel_at_period_end || sub.cancel_at
                          ? sub.cancel_effective_at || sub.cancel_at || sub.current_billing_period_end
                          : null
                      if (cancelAt) return `Cancels ${formatDate(cancelAt)}`
                      return sub.current_billing_period_end
                        ? `Renews ${formatDate(sub.current_billing_period_end)}`
                        : '\u2014'
                    })()}
                  </p>
                  {sub.current_billing_period_start && sub.current_billing_period_end && (
                    <p className="text-xs text-muted-foreground mt-0.5">
                      Period: {sub.current_billing_period_display}
                    </p>
                  )}
                </>
              )}
            </div>
            <div className="flex-1 px-6 py-4">
              <p className="text-sm text-muted-foreground">Status</p>
              <div className="mt-1.5">
                <Badge variant={statusVariant(sub.status)}>{sub.status}</Badge>
              </div>
            </div>
          </div>
        </CardContent>
      </Card>

      {/* Items */}
      <Card className="mt-6">
        <CardHeader>
          <div className="flex items-center justify-between">
            <CardTitle className="text-sm">Items ({items.length})</CardTitle>
            {sub.status === 'active' && plansData && (
              <Button size="sm" variant="outline" onClick={() => setItemDialog({ kind: 'add' })}>
                <Plus size={14} className="mr-1" /> Add Item
              </Button>
            )}
          </div>
        </CardHeader>
        <CardContent className="p-0">
          {items.length === 0 ? (
            <p className="text-sm text-muted-foreground text-center py-8">
              No items on this subscription yet
            </p>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Plan</TableHead>
                  <TableHead className="text-right">Quantity</TableHead>
                  <TableHead>Pending Change</TableHead>
                  <TableHead className="text-right">Actions</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {items.map(item => {
                  const itemPlan = planById(item.plan_id)
                  const pendingPlan = item.pending_plan_id ? planById(item.pending_plan_id) : undefined
                  return (
                    <TableRow key={item.id}>
                      <TableCell>
                        {itemPlan ? (
                          <div>
                            <Link to={`/plans/${itemPlan.id}`} className="text-sm font-medium text-primary hover:underline">
                              {itemPlan.name}
                            </Link>
                            <p className="text-xs text-muted-foreground mt-0.5">
                              {formatCents(itemPlan.base_amount_cents)}/{itemPlan.billing_interval === 'yearly' ? 'yr' : 'mo'}
                            </p>
                          </div>
                        ) : (
                          <span className="text-sm text-foreground font-mono">{item.plan_id.slice(0, 8)}...</span>
                        )}
                      </TableCell>
                      <TableCell className="text-right text-sm font-medium">{item.quantity}</TableCell>
                      <TableCell>
                        {item.pending_plan_id ? (
                          <div className="flex items-center gap-2">
                            <Badge variant="outline">
                              → {pendingPlan?.name || item.pending_plan_id.slice(0, 8) + '...'}
                            </Badge>
                            {item.pending_plan_effective_at && (
                              <span className="text-xs text-muted-foreground">
                                {formatDate(item.pending_plan_effective_at)}
                              </span>
                            )}
                            <Button
                              variant="ghost"
                              size="sm"
                              className="h-7 text-xs"
                              disabled={cancelPendingMutation.isPending}
                              onClick={() => cancelPendingMutation.mutate(item.id)}
                            >
                              Cancel
                            </Button>
                          </div>
                        ) : (
                          <span className="text-xs text-muted-foreground">—</span>
                        )}
                      </TableCell>
                      <TableCell className="text-right">
                        {sub.status === 'active' && (
                          <div className="flex items-center gap-2 justify-end">
                            <Button size="sm" variant="outline" className="h-7 text-xs" onClick={() => setItemDialog({ kind: 'change-quantity', item })}>
                              Change Qty
                            </Button>
                            <Button size="sm" variant="outline" className="h-7 text-xs" onClick={() => setItemDialog({ kind: 'change-plan', item })}>
                              Change Plan
                            </Button>
                            <Button size="sm" variant="outline" className="h-7 text-xs border-destructive text-destructive hover:bg-destructive/10" onClick={() => setItemDialog({ kind: 'remove', item })}>
                              Remove
                            </Button>
                          </div>
                        )}
                      </TableCell>
                    </TableRow>
                  )
                })}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>

      {/* Spend Thresholds */}
      <Card className="mt-6">
        <CardHeader className="flex flex-row items-start justify-between space-y-0">
          <div className="space-y-1">
            <CardTitle className="text-sm">Spend thresholds</CardTitle>
            <p className="text-xs text-muted-foreground">
              Stripe-parity hard cap. Fires an early invoice the moment the running cycle subtotal or any item's running quantity crosses a configured cap.
            </p>
          </div>
          {sub.status !== 'canceled' && sub.status !== 'archived' && (
            <Button size="sm" variant="outline" onClick={() => setShowThresholdsDialog(true)}>
              {sub.billing_thresholds ? 'Edit' : 'Set thresholds'}
            </Button>
          )}
        </CardHeader>
        <CardContent className={sub.billing_thresholds ? 'p-0' : undefined}>
          {sub.billing_thresholds ? (
            <div className="divide-y divide-border">
              {sub.billing_thresholds.amount_gte != null && (
                <div className="flex items-center justify-between px-6 py-3">
                  <span className="text-sm text-muted-foreground w-40 shrink-0">Subtotal cap</span>
                  <span className="text-sm text-foreground">
                    {formatCents(sub.billing_thresholds.amount_gte, items[0] ? planById(items[0].plan_id)?.currency : undefined)}
                    <span className="text-xs text-muted-foreground ml-2">
                      ({sub.billing_thresholds.reset_billing_cycle ? 'resets cycle on fire' : 'continues cycle on fire'})
                    </span>
                  </span>
                </div>
              )}
              {sub.billing_thresholds.item_thresholds.map(t => {
                const ti = items.find(i => i.id === t.subscription_item_id)
                const tp = ti ? planById(ti.plan_id) : undefined
                return (
                  <div key={t.subscription_item_id} className="flex items-center justify-between px-6 py-3">
                    <span className="text-sm text-muted-foreground w-40 shrink-0">{tp?.name || t.subscription_item_id}</span>
                    <span className="text-sm text-foreground">&ge; {trimDecimal(t.usage_gte)} units</span>
                  </div>
                )
              })}
            </div>
          ) : (
            <p className="text-sm text-muted-foreground">No spend cap configured. The cycle scan is the only invoice-emitting path.</p>
          )}
        </CardContent>
      </Card>

      {/* Properties */}
      <Card className="mt-6">
        <CardHeader>
          <CardTitle className="text-sm">Properties</CardTitle>
        </CardHeader>
        <CardContent className="p-0">
          <div className="divide-y divide-border">
            <div className="flex items-center justify-between px-6 py-3">
              <span className="text-sm text-muted-foreground w-40 shrink-0">Code</span>
              <span className="text-sm text-foreground font-mono">{sub.code}</span>
            </div>
            <div className="flex items-center justify-between px-6 py-3">
              <span className="text-sm text-muted-foreground w-40 shrink-0">Customer</span>
              {/* Customer ID stays copyable whether or not the customer
                  record has resolved — operators paste it into API
                  calls and support threads. */}
              <div className="flex items-center gap-1.5">
                {customer ? (
                  <Link to={`/customers/${customer.id}`} className="text-sm font-medium text-primary hover:underline">
                    {customer.display_name}
                  </Link>
                ) : (
                  <span className="text-sm text-foreground font-mono">{sub.customer_id}</span>
                )}
                <CopyButton text={sub.customer_id} title="Copy customer ID" />
              </div>
            </div>
            <div className="flex items-center justify-between px-6 py-3">
              <span className="text-sm text-muted-foreground w-40 shrink-0">Status</span>
              <Badge variant={statusVariant(sub.status)}>{sub.status}</Badge>
            </div>
            {sub.billing_time && (
              <div className="flex items-center justify-between px-6 py-3">
                <span className="text-sm text-muted-foreground w-40 shrink-0 inline-flex items-center gap-1.5">
                  Billing alignment
                  <Tooltip>
                    <TooltipTrigger render={<span className="inline-flex cursor-help" />}>
                      <HelpCircle size={13} className="text-muted-foreground/70" />
                    </TooltipTrigger>
                    <TooltipContent className="max-w-xs">
                      <p className="text-xs">
                        <strong>{sub.billing_time === 'anniversary' ? 'Anniversary' : 'Calendar'}</strong> is the alignment chosen at activation.{' '}
                        {sub.billing_time === 'calendar'
                          ? 'For monthly plans, the first period runs from the activation day to the next 1st of the month, then bills on the 1st thereafter.'
                          : 'The period anchors on the activation day-of-month and rolls forward by the plan interval.'}
                        {' '}Scheduled plan-interval changes (immediate=false) preserve the existing day-of-month anchor at the boundary. Immediate cross-interval swaps (e.g. yearly → monthly with immediate=true) re-anchor the cycle on the swap day.
                      </p>
                    </TooltipContent>
                  </Tooltip>
                </span>
                <span className="text-sm text-foreground">{sub.billing_time === 'anniversary' ? 'Anniversary' : 'Calendar'}</span>
              </div>
            )}
            {sub.status === 'trialing' && sub.trial_start_at && sub.trial_end_at && (
              <div className="flex items-center justify-between px-6 py-3">
                <span className="text-sm text-muted-foreground w-40 shrink-0">Trial period</span>
                <span className="text-sm text-foreground">
                  {formatDate(sub.trial_start_at)} {'\u2014'} {formatDate(sub.trial_end_at)}
                </span>
              </div>
            )}
            {/* Third surface that has to respect a scheduled cancel — the
                timeline bar and the Billing-cycle summary are the other two.
                All three keyed off the period end alone, so one page could
                say "Cancels Sep 1" and "Renews on Sep 1" at the same time. */}
            {sub.status !== 'trialing' && sub.status !== 'canceled' && sub.current_billing_period_end && (
              <div className="flex items-center justify-between px-6 py-3">
                <span className="text-sm text-muted-foreground w-40 shrink-0">
                  {sub.cancel_at_period_end || sub.cancel_at ? 'Cancels on' : 'Renews on'}
                </span>
                <span className="text-sm text-foreground">
                  {formatDate(
                    sub.cancel_at_period_end || sub.cancel_at
                      ? sub.cancel_effective_at || sub.cancel_at || sub.current_billing_period_end
                      : sub.current_billing_period_end,
                  )}
                </span>
              </div>
            )}
            {/* A trial canceled at trial end never billed its "first paid
                period" — showing those dates reads as a period that existed.
                Render the trial range instead (ADR-069). */}
            {sub.status === 'canceled' && sub.trial_end_at && sub.canceled_at && new Date(sub.canceled_at) <= new Date(sub.trial_end_at) ? (
              <div className="flex items-center justify-between px-6 py-3">
                <span className="text-sm text-muted-foreground w-40 shrink-0">Trial</span>
                <span className="text-sm text-foreground">
                  {formatCivilPeriod(sub.trial_start_at, sub.trial_end_at, undefined) || '\u2014'} — canceled at trial end
                </span>
              </div>
            ) : (
              <div className="flex items-center justify-between px-6 py-3">
                <span className="text-sm text-muted-foreground w-40 shrink-0">
                  {sub.status === 'trialing' ? 'First billing period' : 'Current period'}
                </span>
                <span className="text-sm text-foreground">
                  {sub.current_billing_period_display || '\u2014'}
                </span>
              </div>
            )}
            {sub.usage_cap_units != null && (
              <div className="flex items-center justify-between px-6 py-3">
                <span className="text-sm text-muted-foreground w-40 shrink-0">Usage Cap</span>
                <span className="text-sm text-foreground">
                  {sub.usage_cap_units.toLocaleString()} units / period
                  <span className="text-xs text-muted-foreground ml-2">({sub.overage_action === 'block' ? 'hard cap' : 'charge overage'})</span>
                </span>
              </div>
            )}
            <div className="flex items-center justify-between px-6 py-3">
              <span className="text-sm text-muted-foreground w-40 shrink-0">Created</span>
              <span className="text-sm text-foreground">{formatDateTime(sub.created_at)}</span>
            </div>
            <div className="flex items-center justify-between px-6 py-3">
              <span className="text-sm text-muted-foreground w-40 shrink-0">ID</span>
              <div className="flex items-center gap-2">
                <span className="text-sm text-foreground font-mono">{sub.id}</span>
                <CopyButton text={sub.id} />
              </div>
            </div>
          </div>
        </CardContent>
      </Card>

      {/* Invoice Preview. Hidden on terminal subs — a canceled sub has no
          next invoice, and the full-period estimate read as a real upcoming
          charge (found in the FLOW B14 walk: canceled sub advertising $490). */}
      {sub.status !== 'canceled' && sub.status !== 'archived' && (
      <Card className="mt-6">
        <CardHeader>
          <CardTitle className="text-sm">Next Invoice Preview</CardTitle>
        </CardHeader>
        <CardContent className="p-0">
          {preview ? (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Description</TableHead>
                  <TableHead className="text-right">Quantity</TableHead>
                  <TableHead className="text-right">Amount</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {preview.lines.map((line, i) => (
                  <TableRow key={i}>
                    <TableCell>{line.description}</TableCell>
                    <TableCell className="text-right tabular-nums">{Number(line.quantity).toLocaleString(undefined, { maximumFractionDigits: 6 })}</TableCell>
                    <TableCell className="text-right font-medium">{formatCents(line.amount_cents, line.currency)}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
              <tfoot>
                {preview.totals.map(t => (
                  <TableRow key={t.currency} className="border-t-2">
                    <TableCell colSpan={2} className="text-right font-semibold">Subtotal{preview.totals.length > 1 ? ` (${t.currency})` : ''}</TableCell>
                    <TableCell className="text-right font-semibold">{formatCents(t.amount_cents, t.currency)}</TableCell>
                  </TableRow>
                ))}
              </tfoot>
            </Table>
          ) : previewError ? (
            <div className="px-6 py-8 text-center">
              <p className="text-sm text-muted-foreground">Preview not available</p>
              <p className="text-sm text-muted-foreground mt-1">Activate the subscription and set a billing period to see a preview</p>
            </div>
          ) : (
            <p className="text-sm text-muted-foreground text-center py-8">Invoice preview will appear once a billing period is set</p>
          )}
        </CardContent>
      </Card>
      )}

      {/* Related Invoices */}
      <Card className="mt-6">
        <CardHeader>
          <div className="flex items-center justify-between">
            <CardTitle className="text-sm">Invoices ({invoices?.length ?? 0})</CardTitle>
            {(invoices?.length ?? 0) > 5 && (
              <Link to="/invoices" className="text-xs text-primary hover:underline">View all</Link>
            )}
          </div>
        </CardHeader>
        <CardContent className="p-0">
          {invoices && invoices.length > 0 ? (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Invoice</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead>Payment</TableHead>
                  <TableHead className="text-right">Amount</TableHead>
                  <TableHead>Date</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {invoices.slice(0, 5).map(inv => (
                  <TableRow
                    key={inv.id}
                    className="cursor-pointer hover:bg-muted/50 transition-colors"
                    onClick={(e) => {
                      const target = e.target as HTMLElement
                      if (target.closest('button, a, input, select')) return
                      navigate(`/invoices/${inv.id}`)
                    }}
                  >
                    <TableCell>
                      <Link to={`/invoices/${inv.id}`} className="text-sm font-medium text-foreground hover:text-primary transition-colors">
                        {inv.invoice_number}
                      </Link>
                    </TableCell>
                    <TableCell><Badge variant={statusVariant(inv.status)}>{inv.status}</Badge></TableCell>
                    <TableCell>
                      {/* payment_status is meaningless on drafts — no
                          PaymentIntent exists yet. Hide the pill (Stripe
                          parity); show an em dash so the column stays
                          aligned. */}
                      {inv.status === 'draft' ? (
                        <span className="text-sm text-muted-foreground">—</span>
                      ) : (
                        <Badge variant={statusVariant(inv.payment_status)}>{inv.payment_status}</Badge>
                      )}
                    </TableCell>
                    <TableCell className="text-right font-medium">{formatCents(inv.total_amount_cents)}</TableCell>
                    <TableCell className="text-sm text-muted-foreground">{formatDate(inv.created_at)}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          ) : (
            <p className="text-sm text-muted-foreground text-center py-8">Invoices will appear here after billing runs</p>
          )}
        </CardContent>
      </Card>

      {/* Activity Timeline (T0-18) — lifecycle audit feed. Mirrors the
          invoice payment-activity panel so CS reps see the same shape
          on both resources. Hidden when there's nothing to show. */}
      {(activityTimeline.length > 0 || activityTimelineError) && (
        <Card className="mt-6">
          <CardHeader>
            <CardTitle className="text-sm">Activity</CardTitle>
          </CardHeader>
          <CardContent>
            {activityTimelineError && (
              <p className="text-xs text-destructive">
                Couldn&apos;t load activity — the audit history is temporarily unavailable. Refresh to retry.
              </p>
            )}
            {activityTimelineTruncated && (
              <p className="text-xs text-muted-foreground mb-3">
                Showing the 100 most recent events — earlier history isn&apos;t displayed.
              </p>
            )}
            <div className="relative">
              {activityTimeline.map((event, i) => (
                <div key={i} className="flex gap-4 pb-2 last:pb-0">
                  <div className="flex flex-col items-center">
                    <div className={cn(
                      'w-2.5 h-2.5 rounded-full mt-1.5',
                      event.status === 'succeeded' || event.status === 'resolved' ? 'bg-emerald-500' :
                      event.status === 'canceled' ? 'bg-destructive' :
                      event.status === 'warning' ? 'bg-amber-500' :
                      event.status === 'escalated' ? 'bg-violet-500' :
                      'bg-blue-500'
                    )} />
                    {i < activityTimeline.length - 1 && <div className="w-px flex-1 bg-border mt-1" />}
                  </div>
                  <div className="flex-1 min-w-0">
                    <div className="flex items-center justify-between">
                      <div className="flex items-center gap-1.5 min-w-0">
                        <p className="text-sm text-foreground">{event.description}</p>
                        {/* Narrative surface (ADR-030 2026-07-18 amendment):
                            primary timestamp is the simulated instant,
                            wall-clock demotes to the "Recorded …" subline.
                            No per-row simulated chip here — the page banner
                            is the single disclosure for this scope (ADR-099);
                            the row's own timestamps carry the information.
                            The AuditLog page keeps per-row chips: it is a
                            MIXED wall-primary forensic list. */}
                      </div>
                      <span className="text-xs text-muted-foreground ml-4 whitespace-nowrap">{formatDateTime(event.sim_effective_at || event.timestamp)}</span>
                    </div>
                    {(event.detail || event.detail_timestamp) && (
                      <p className="text-xs text-muted-foreground mt-0.5">
                        {event.detail}
                        {event.detail_timestamp && (
                          <>{event.detail ? ' ' : ''}{formatDateTime(event.detail_timestamp)}</>
                        )}
                      </p>
                    )}
                    {event.sim_effective_at ? (
                      <p className="text-xs text-muted-foreground mt-0.5">
                        Recorded {formatDateTime(event.timestamp)}
                        {(event.actor_name || event.actor_type) && <> · by {event.actor_name || event.actor_type}</>}
                      </p>
                    ) : (
                      (event.actor_name || event.actor_type) && (
                        <p className="text-xs text-muted-foreground mt-0.5">
                          by {event.actor_name || event.actor_type}
                        </p>
                      )
                    )}
                  </div>
                </div>
              ))}
            </div>
          </CardContent>
        </Card>
      )}

      {/* Pause collection (Stripe-style soft pause). Cycle keeps
          drafting invoices on schedule; new invoices land at
          status=draft and skip auto-charge until collection is
          resumed. The "Resume on" input is optional — blank means
          indefinite pause (operator manually resumes via Resume
          Collection); filled means the catchup orchestrator
          auto-clears pause_collection at that timestamp. Matches
          Stripe's dashboard UX where the same modal exposes both
          options. */}
      <Dialog open={showPauseConfirm} onOpenChange={(open) => { if (!open) { setShowPauseConfirm(false); setPauseResumesAt('') } }}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Pause collection</DialogTitle>
            <DialogDescription>
              Future invoices on this subscription will be created as drafts and skip auto-charge until collection is resumed. The billing cycle keeps running so you can still see what was owed across the pause. <strong>On resume, the full current period bills — paused days are not pro-rated. Issue a credit grant after resuming if you want to offset them.</strong>
            </DialogDescription>
          </DialogHeader>
          <div className="space-y-2 py-2">
            <Label htmlFor="pause-resumes-at">Auto-resume on (optional)</Label>
            <DatePicker
              value={pauseResumesAt}
              onChange={setPauseResumesAt}
              placeholder="Pick a date"
              minDate={nowDate}
            />
            <p className="text-xs text-muted-foreground">
              Leave blank to pause indefinitely; you can resume manually any time. Set a date to auto-resume — the next cycle on or after this date bills normally.
            </p>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => { setShowPauseConfirm(false); setPauseResumesAt('') }} disabled={pauseCollectionMutation.isPending}>
              Keep active
            </Button>
            <Button onClick={() => pauseCollectionMutation.mutate()} disabled={pauseCollectionMutation.isPending}>
              Pause collection
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* Cancel Confirm */}
      <TypedConfirmDialog
        open={showCancelConfirm}
        onOpenChange={setShowCancelConfirm}
        title="Cancel Subscription"
        description="Cancelling ends billing and stops future invoices for this subscription. This action cannot be undone."
        confirmWord="CANCEL"
        confirmLabel="Cancel Subscription"
        onConfirm={() => cancelMutation.mutate()}
        loading={cancelMutation.isPending}
      />

      {/* Cancel Choice — radio-card pattern (Stripe / Linear / GitHub).
          Two clear options with visible selection state, single primary
          action in the footer that commits the chosen mode. "Immediately"
          still routes through the typed-confirm dialog because the action
          is irreversible — friction is intentional there. */}
      <AlertDialog
        open={showCancelChoice}
        onOpenChange={(o) => { setShowCancelChoice(o); if (!o) setCancelMode('period_end') }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Cancel subscription</AlertDialogTitle>
            <AlertDialogDescription>
              Choose when this subscription should stop billing.
              {sub.status === 'trialing' && sub.trial_end_at
                ? <> The trial ends on {formatDate(sub.trial_end_at)}.</>
                : sub.current_billing_period_end && (
                  <> The current period ends on {formatDate(sub.current_billing_period_end)}.</>
                )}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <div className="space-y-2 py-2" role="radiogroup" aria-label="Cancellation timing">
            <RadioCard
              selected={cancelMode === 'period_end'}
              onClick={() => setCancelMode('period_end')}
              disabled={acting}
              title={sub.status === 'trialing' ? 'At trial end' : 'At period end'}
              description={
                // ADR-069: on a trialing sub the flag cancels FREE at trial
                // end — say so; the old copy showed the first PAID period's
                // end date, promising a month the customer would have paid for.
                sub.status === 'trialing' && sub.trial_end_at
                  ? `Trial runs until ${formatDate(sub.trial_end_at)}, then the subscription cancels — the customer won't be charged. Reversible until then.`
                  : sub.current_billing_period_end
                    ? `Customer keeps access until ${formatDate(sub.current_billing_period_end)}. Reversible until then.`
                    : 'Customer keeps access until the current period ends. Reversible until then.'
              }
            />
            <RadioCard
              selected={cancelMode === 'immediately'}
              onClick={() => setCancelMode('immediately')}
              disabled={acting}
              destructive
              title="Immediately"
              description="Stops billing right now. Cannot be undone."
            />
          </div>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={acting}>Keep subscription</AlertDialogCancel>
            <Button
              variant={cancelMode === 'immediately' ? 'destructive' : 'default'}
              disabled={acting}
              onClick={() => {
                if (cancelMode === 'immediately') {
                  setShowCancelChoice(false)
                  setShowCancelConfirm(true)
                } else {
                  scheduleCancelMutation.mutate()
                }
              }}
            >
              {scheduleCancelMutation.isPending
                ? <><Loader2 size={14} className="animate-spin mr-1.5" />Scheduling…</>
                : cancelMode === 'immediately' ? 'Cancel immediately…' : 'Schedule cancellation'}
            </Button>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      {/* Extend Trial — pushes trial_end_at later. The backend rejects values
          before the current trial_end_at, so the dialog seeds with current+7d
          and surfaces any server validation error inline via toast. */}
      <Dialog open={showExtendTrial} onOpenChange={setShowExtendTrial}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Extend trial</DialogTitle>
            <DialogDescription>
              Push the trial end date later. Must be after the current trial end
              {sub.trial_end_at && <> ({formatDate(sub.trial_end_at)})</>}.
            </DialogDescription>
          </DialogHeader>
          <div className="space-y-2 py-2">
            <Label>New trial end</Label>
            <DateTimePicker
              date={extendTrialDate}
              time={extendTrialTime}
              onDateChange={setExtendTrialDate}
              onTimeChange={setExtendTrialTime}
              minDate={sub.trial_end_at ? new Date(sub.trial_end_at) : nowDate}
            />
            <p className="text-xs text-muted-foreground">
              Times are in your tenant timezone ({getTenantTimezone() || 'UTC'}).
            </p>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setShowExtendTrial(false)} disabled={acting}>
              Cancel
            </Button>
            <Button
              onClick={() => {
                if (!extendTrialDate || !/^\d{2}:\d{2}$/.test(extendTrialTime)) return
                const tz = getTenantTimezone() || 'UTC'
                const iso = fromZonedTime(`${extendTrialDate}T${extendTrialTime}:00`, tz).toISOString()
                extendTrialMutation.mutate(iso)
              }}
              disabled={acting || !extendTrialDate || !/^\d{2}:\d{2}$/.test(extendTrialTime)}
            >
              Extend
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* Add Item */}
      {itemDialog?.kind === 'add' && plansData && (
        <AddItemDialog
          subscription={sub}
          plans={plansData}
          existingPlanIDs={items.map(i => i.plan_id)}
          onClose={() => setItemDialog(null)}
          onAdded={() => { setItemDialog(null); invalidateAll(); toast.success('Item added') }}
        />
      )}

      {/* Change Item Plan */}
      {itemDialog?.kind === 'change-plan' && plansData && (
        <ChangeItemPlanDialog
          subscriptionID={sub.id}
          item={itemDialog.item}
          plans={plansData}
          existingPlanIDs={items.map(i => i.plan_id)}
          onClose={() => setItemDialog(null)}
          onChanged={(res) => {
            setItemDialog(null)
            invalidateAll()
            if (res.proration) {
              if (res.proration.type === 'invoice') {
                toast.success(`Proration invoice created for ${formatCents(res.proration.amount_cents)}`)
              } else if (res.proration.type === 'adjustment') {
                toast.success(`Open invoice reduced by ${formatCents(Math.abs(res.proration.amount_cents))}`)
              } else {
                toast.success(`${formatCents(Math.abs(res.proration.amount_cents))} credited to customer balance`)
              }
            } else {
              toast.success('Plan change saved')
            }
          }}
        />
      )}

      {/* Change Item Quantity */}
      {itemDialog?.kind === 'change-quantity' && (
        <ChangeItemQuantityDialog
          subscriptionID={sub.id}
          item={itemDialog.item}
          plan={planById(itemDialog.item.plan_id)}
          onClose={() => setItemDialog(null)}
          onChanged={(res) => {
            setItemDialog(null)
            invalidateAll()
            if (res.proration) {
              if (res.proration.type === 'invoice') {
                toast.success(`Proration invoice created for ${formatCents(res.proration.amount_cents)}`)
              } else if (res.proration.type === 'adjustment') {
                toast.success(`Open invoice reduced by ${formatCents(Math.abs(res.proration.amount_cents))}`)
              } else {
                toast.success(`${formatCents(Math.abs(res.proration.amount_cents))} credited to customer balance`)
              }
            } else {
              toast.success('Quantity updated')
            }
          }}
        />
      )}

      {/* Remove Item */}
      {itemDialog?.kind === 'remove' && (
        <RemoveItemConfirm
          subscriptionID={sub.id}
          item={itemDialog.item}
          plan={planById(itemDialog.item.plan_id)}
          onClose={() => setItemDialog(null)}
          onRemoved={() => { setItemDialog(null); invalidateAll(); toast.success('Item removed') }}
        />
      )}

      {/* Edit Billing Thresholds */}
      {showThresholdsDialog && (
        <EditBillingThresholdsDialog
          subscription={sub}
          items={items}
          planById={planById}
          onClose={() => setShowThresholdsDialog(false)}
          onSaved={(verb) => { setShowThresholdsDialog(false); invalidateAll(); toast.success(verb) }}
        />
      )}
    </Layout>
  )
}

// planOptionLabel is the single source of truth for how a plan reads in a
// picker. The code disambiguates same-named plans (two "Pro Monthly"s are
// indistinguishable without it) and the billing timing is the one fact that
// predicts whether a swap is even allowed, so both earn a place in the row.
function planOptionLabel(p: Plan): string {
  const timing = p.base_bill_timing === 'in_advance' ? 'billed in advance' : 'billed in arrears'
  return `${p.name} (${p.code}) — ${formatCents(p.base_amount_cents)}/${p.billing_interval} · ${timing}`
}

// planComboboxOptions sorts by display name (code as tiebreak, so duplicate
// names order stably) and carries the code as a search keyword.
function planComboboxOptions(plans: Plan[]): ComboboxOption[] {
  return [...plans]
    .sort((a, b) => a.name.localeCompare(b.name) || a.code.localeCompare(b.code))
    .map(p => ({ value: p.id, label: planOptionLabel(p), keywords: [p.code, p.name] }))
}

// AddItemDialog picks a plan + quantity and POSTs to /subscriptions/:id/items.
// The backend rejects duplicates on (subscription, plan); we pre-filter to
// keep the dropdown clean.
function AddItemDialog({ subscription, plans, existingPlanIDs, onClose, onAdded }: {
  subscription: Subscription
  plans: Plan[]
  existingPlanIDs: string[]
  onClose: () => void
  onAdded: () => void
}) {
  const [selectedPlan, setSelectedPlan] = useState('')
  const [quantity, setQuantity] = useState('1')
  const [submitting, setSubmitting] = useState(false)
  // One idempotency key per dialog open (same contract as the credit
  // dialogs): item adds fire prorations, so a transient-failure retry must
  // replay the original response, not add a second item's worth of charges.
  const [idemKey] = useState(() => crypto.randomUUID())

  // Same-interval only: the backend's mixed-interval guard rejects adding a
  // yearly item to a monthly sub (and vice versa), so those options are
  // noise that can only end in an error toast. Timing may mix (hybrid
  // advance-base + arrears-usage subs are supported).
  const currentInterval = plans.find(p => (subscription.items ?? []).some(i => i.plan_id === p.id))?.billing_interval
  const candidatePlans = plans.filter(p => p.status === 'active' && !existingPlanIDs.includes(p.id))
  const availablePlans = currentInterval
    ? candidatePlans.filter(p => p.billing_interval === currentInterval)
    : candidatePlans
  const hiddenCount = candidatePlans.length - availablePlans.length

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    if (!selectedPlan) return
    const qty = parseInt(quantity, 10)
    if (!Number.isFinite(qty) || qty < 1) {
      toast.error('Quantity must be a positive integer')
      return
    }
    setSubmitting(true)
    try {
      await api.addSubscriptionItem(subscription.id, { plan_id: selectedPlan, quantity: qty }, idemKey)
      onAdded()
    } catch (err) {
      showApiError(err, 'Failed to add item')
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <Dialog open onOpenChange={(open) => { if (!open) onClose() }}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Add Item</DialogTitle>
          <DialogDescription>
            Add a plan to this subscription. Mid-cycle adds are prorated against the remaining period.
          </DialogDescription>
        </DialogHeader>
        <form onSubmit={handleSubmit} noValidate className="space-y-4">
          <div className="space-y-2">
            <Label>Plan</Label>
            <Combobox
              value={selectedPlan}
              onChange={setSelectedPlan}
              options={planComboboxOptions(availablePlans)}
              placeholder="Select a plan..."
              emptyMessage="No plans available — all matching active plans are already on this subscription."
              triggerClassName="w-full"
            />
            {hiddenCount > 0 && (
              <p className="text-xs text-muted-foreground">
                {hiddenCount} plan{hiddenCount === 1 ? '' : 's'} on a different billing interval
                {hiddenCount === 1 ? ' is' : ' are'} not shown — a subscription bills on one interval.
              </p>
            )}
          </div>

          <div className="space-y-2">
            <Label htmlFor="add-qty">Quantity</Label>
            <Input
              id="add-qty"
              type="number"
              min="1"
              value={quantity}
              onChange={(e) => setQuantity(e.target.value)}
            />
          </div>

          <DialogFooter>
            <Button type="button" variant="outline" onClick={onClose}>Cancel</Button>
            <Button type="submit" disabled={submitting || !selectedPlan}>
              {submitting ? (
                <><Loader2 size={14} className="animate-spin mr-2" />Adding...</>
              ) : 'Add Item'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

// ChangeItemPlanDialog swaps an item's plan. Immediate swaps prorate; deferred
// swaps stamp pending_plan_id and apply at the next cycle boundary.
function ChangeItemPlanDialog({ subscriptionID, item, plans, existingPlanIDs, onClose, onChanged }: {
  subscriptionID: string
  item: SubscriptionItem
  plans: Plan[]
  existingPlanIDs: string[]
  onClose: () => void
  onChanged: (res: ItemChangeResult) => void
}) {
  const [selectedPlan, setSelectedPlan] = useState('')
  const [immediate, setImmediate] = useState(false)
  const [submitting, setSubmitting] = useState(false)
  // One idempotency key per dialog open: a plan change fires prorations and
  // (cross-interval) refund credit notes — a retry after a dropped response
  // must replay, not re-credit. The retry would otherwise 400 on the
  // same-plan guard while leaving the operator unsure what happened.
  const [idemKey] = useState(() => crypto.randomUUID())

  // Candidates: active plans that aren't the current item's plan and that
  // aren't already attached to another item on this subscription (the backend
  // rejects the duplicate with 409). Bill-timing must match the current plan
  // — the backend 422s advance↔arrears swaps (Zuora-shape rejection; the
  // operator path is cancel + recreate) — so cross-timing options are noise.
  // Cross-INTERVAL swaps (monthly ↔ yearly) stay: those are supported.
  const currentPlan = plans.find(p => p.id === item.plan_id)
  const candidatePlans = plans.filter(
    p => p.status === 'active' && p.id !== item.plan_id && !existingPlanIDs.includes(p.id),
  )
  const availablePlans = currentPlan
    ? candidatePlans.filter(p => p.base_bill_timing === currentPlan.base_bill_timing)
    : candidatePlans
  const hiddenCount = candidatePlans.length - availablePlans.length

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    if (!selectedPlan) return
    setSubmitting(true)
    try {
      const res = await api.updateSubscriptionItem(subscriptionID, item.id, {
        new_plan_id: selectedPlan,
        immediate,
      }, idemKey)
      onChanged(res)
    } catch (err) {
      showApiError(err, 'Failed to change plan')
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <Dialog open onOpenChange={(open) => { if (!open) onClose() }}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Change Plan</DialogTitle>
        </DialogHeader>
        <form onSubmit={handleSubmit} noValidate className="space-y-4">
          <div>
            <p className="text-sm text-muted-foreground">Current plan</p>
            <p className="text-sm font-semibold text-foreground mt-0.5">
              {currentPlan?.name || item.plan_id}
            </p>
          </div>

          <div className="space-y-2">
            <Label>New Plan</Label>
            <Combobox
              value={selectedPlan}
              onChange={setSelectedPlan}
              options={planComboboxOptions(availablePlans)}
              placeholder="Select a plan..."
              emptyMessage="No other plans available"
              triggerClassName="w-full"
            />
            {hiddenCount > 0 && (
              <p className="text-xs text-muted-foreground">
                {hiddenCount} plan{hiddenCount === 1 ? '' : 's'} with a different billing timing
                {hiddenCount === 1 ? ' is' : ' are'} not shown — a plan swap keeps its billing timing.
                To move to {currentPlan?.base_bill_timing === 'in_advance' ? 'arrears' : 'advance'} billing,
                cancel and create a new subscription.
              </p>
            )}
          </div>

          <label className="flex items-start gap-2 text-sm cursor-pointer">
            <Checkbox
              checked={immediate}
              onCheckedChange={(checked) => setImmediate(checked === true)}
              className="mt-0.5"
            />
            <div>
              <span className="font-medium text-foreground">Apply immediately (with proration)</span>
              {immediate ? (
                <p className="text-xs text-muted-foreground mt-1">
                  The remaining time on the current billing period will be prorated. A credit or charge is applied based on the price difference between plans.
                </p>
              ) : (
                <p className="text-xs text-muted-foreground mt-1">
                  Scheduled — the plan swap will apply at the next billing cycle boundary. No proration.
                </p>
              )}
            </div>
          </label>

          <DialogFooter>
            <Button type="button" variant="outline" onClick={onClose}>Cancel</Button>
            <Button type="submit" disabled={submitting || !selectedPlan}>
              {submitting ? (
                <><Loader2 size={14} className="animate-spin mr-2" />Changing...</>
              ) : 'Change Plan'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

// ChangeItemQuantityDialog updates a single item's quantity. The backend
// rejects no-ops (new qty == current qty) with a 422 — we pre-check to keep
// the UX tight.
function ChangeItemQuantityDialog({ subscriptionID, item, plan, onClose, onChanged }: {
  subscriptionID: string
  item: SubscriptionItem
  plan: Plan | undefined
  onClose: () => void
  onChanged: (res: ItemChangeResult) => void
}) {
  const [quantity, setQuantity] = useState(String(item.quantity))
  const [submitting, setSubmitting] = useState(false)
  // One idempotency key per dialog open — quantity changes fire prorations;
  // a retry must replay, not re-prorate.
  const [idemKey] = useState(() => crypto.randomUUID())

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    const qty = parseInt(quantity, 10)
    if (!Number.isFinite(qty) || qty < 1) {
      toast.error('Quantity must be a positive integer')
      return
    }
    if (qty === item.quantity) {
      toast.error('New quantity is the same as current quantity')
      return
    }
    setSubmitting(true)
    try {
      const res = await api.updateSubscriptionItem(subscriptionID, item.id, { quantity: qty }, idemKey)
      onChanged(res)
    } catch (err) {
      showApiError(err, 'Failed to change quantity')
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <Dialog open onOpenChange={(open) => { if (!open) onClose() }}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Change Quantity</DialogTitle>
          <DialogDescription>
            Applied immediately. Quantity increases charge prorated incrementally; decreases credit the customer.
          </DialogDescription>
        </DialogHeader>
        <form onSubmit={handleSubmit} noValidate className="space-y-4">
          <div>
            <p className="text-sm text-muted-foreground">Plan</p>
            <p className="text-sm font-semibold text-foreground mt-0.5">{plan?.name || item.plan_id}</p>
          </div>

          <div className="space-y-2">
            <Label htmlFor="qty-input">Quantity</Label>
            <Input
              id="qty-input"
              type="number"
              min="1"
              value={quantity}
              onChange={(e) => setQuantity(e.target.value)}
            />
            <p className="text-xs text-muted-foreground">Current: {item.quantity}</p>
          </div>

          <DialogFooter>
            <Button type="button" variant="outline" onClick={onClose}>Cancel</Button>
            <Button type="submit" disabled={submitting}>
              {submitting ? (
                <><Loader2 size={14} className="animate-spin mr-2" />Saving...</>
              ) : 'Save'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

// RemoveItemConfirm deletes an item. Removing mid-period generates a credit
// for the unused portion of the just-paid period on the backend — surface
// that explicitly so the operator isn't surprised.
function RemoveItemConfirm({ subscriptionID, item, plan, onClose, onRemoved }: {
  subscriptionID: string
  item: SubscriptionItem
  plan: Plan | undefined
  onClose: () => void
  onRemoved: () => void
}) {
  const [submitting, setSubmitting] = useState(false)

  const handleConfirm = async () => {
    setSubmitting(true)
    try {
      await api.removeSubscriptionItem(subscriptionID, item.id)
      onRemoved()
    } catch (err) {
      showApiError(err, 'Failed to remove item')
      setSubmitting(false)
    }
  }

  return (
    <AlertDialog open onOpenChange={(open) => { if (!open) onClose() }}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>Remove item from subscription?</AlertDialogTitle>
          <AlertDialogDescription>
            Remove <strong>{plan?.name || item.plan_id}</strong> (qty {item.quantity}) from this subscription. If removed mid-period, a credit will be issued for the unused portion.
          </AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel disabled={submitting}>Cancel</AlertDialogCancel>
          <AlertDialogAction variant="destructive" onClick={handleConfirm} disabled={submitting}>
            {submitting ? 'Removing...' : 'Remove Item'}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}

// EditBillingThresholdsDialog sets or clears the Stripe-parity hard cap. The
// PUT body is replace-all: any per-item row not in the submitted list is
// deleted by the store, so we always send the full set the operator sees.
// amount_gte is captured in major units in the input and converted to integer
// cents on submit; usage_gte is a decimal-string per ADR-005.
function EditBillingThresholdsDialog({ subscription, items, planById, onClose, onSaved }: {
  subscription: Subscription
  items: SubscriptionItem[]
  planById: (planID: string) => Plan | undefined
  onClose: () => void
  onSaved: (verb: string) => void
}) {
  const existing = subscription.billing_thresholds
  const subCurrency = items[0] ? planById(items[0].plan_id)?.currency : undefined

  const [amountStr, setAmountStr] = useState(
    existing?.amount_gte != null ? (existing.amount_gte / 100).toFixed(2) : ''
  )
  const [resetCycle, setResetCycle] = useState(existing?.reset_billing_cycle ?? false)
  const [perItem, setPerItem] = useState<Record<string, string>>(() => {
    const out: Record<string, string> = {}
    for (const t of existing?.item_thresholds ?? []) {
      out[t.subscription_item_id] = trimDecimal(t.usage_gte)
    }
    return out
  })
  const [submitting, setSubmitting] = useState(false)

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    const body: { amount_gte?: number; reset_billing_cycle?: boolean; item_thresholds?: { subscription_item_id: string; usage_gte: string }[] } = {
      reset_billing_cycle: resetCycle,
    }

    const trimmedAmount = amountStr.trim()
    if (trimmedAmount !== '') {
      const amt = Number(trimmedAmount)
      if (!Number.isFinite(amt) || amt <= 0) {
        toast.error('Subtotal cap must be a positive number')
        return
      }
      body.amount_gte = Math.round(amt * 100)
    }

    const itemThresholds: { subscription_item_id: string; usage_gte: string }[] = []
    for (const item of items) {
      const raw = (perItem[item.id] ?? '').trim()
      if (raw === '') continue
      if (!/^\d+(\.\d+)?$/.test(raw)) {
        toast.error(`Per-item cap for ${planById(item.plan_id)?.name || item.id} must be a non-negative number`)
        return
      }
      itemThresholds.push({ subscription_item_id: item.id, usage_gte: raw })
    }
    if (itemThresholds.length > 0) {
      body.item_thresholds = itemThresholds
    }

    if (body.amount_gte == null && (body.item_thresholds == null || body.item_thresholds.length === 0)) {
      toast.error('Set at least one cap (subtotal or per-item)')
      return
    }

    setSubmitting(true)
    try {
      await api.setSubscriptionBillingThresholds(subscription.id, body)
      onSaved(existing ? 'Thresholds updated' : 'Thresholds set')
    } catch (err) {
      showApiError(err, 'Failed to save thresholds')
      setSubmitting(false)
    }
  }

  const handleClear = async () => {
    setSubmitting(true)
    try {
      await api.clearSubscriptionBillingThresholds(subscription.id)
      onSaved('Thresholds cleared')
    } catch (err) {
      showApiError(err, 'Failed to clear thresholds')
      setSubmitting(false)
    }
  }

  return (
    <Dialog open onOpenChange={(open) => { if (!open && !submitting) onClose() }}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>{existing ? 'Edit spend thresholds' : 'Set spend thresholds'}</DialogTitle>
          <DialogDescription>
            Configure a hard cap. The billing engine fires an early invoice the moment the running cycle subtotal or any item's running quantity crosses a configured cap. At least one cap is required.
          </DialogDescription>
        </DialogHeader>
        <form onSubmit={handleSubmit} noValidate className="space-y-5">
          <div className="space-y-2">
            <Label htmlFor="amount-gte">Subtotal cap{subCurrency ? ` (${subCurrency.toUpperCase()})` : ''}</Label>
            <Input
              id="amount-gte"
              type="number"
              inputMode="decimal"
              step="0.01"
              min="0"
              placeholder="e.g. 1000.00"
              value={amountStr}
              onChange={(e) => setAmountStr(e.target.value)}
            />
            <p className="text-xs text-muted-foreground">
              Fires when the running cycle subtotal crosses this amount. Leave blank for no subtotal cap.
            </p>
          </div>

          <div className="flex items-start gap-2">
            <Checkbox
              id="reset-cycle"
              checked={resetCycle}
              onCheckedChange={(v) => setResetCycle(v === true)}
            />
            <div className="space-y-1">
              <Label htmlFor="reset-cycle" className="font-normal">Reset billing cycle on fire</Label>
              <p className="text-xs text-muted-foreground">
                When checked, a new billing cycle starts at fire time. When unchecked (default), the original cycle continues and the remaining usage bills at the natural cycle end.
              </p>
            </div>
          </div>

          {items.length > 0 && (
            <div className="space-y-2">
              <Label>Per-item caps</Label>
              <div className="rounded-md border divide-y divide-border">
                {items.map(item => {
                  const plan = planById(item.plan_id)
                  return (
                    <div key={item.id} className="flex items-center gap-3 px-3 py-2">
                      <span className="text-sm text-foreground flex-1 truncate">{plan?.name || item.id}</span>
                      <Input
                        type="text"
                        inputMode="decimal"
                        placeholder="leave blank for none"
                        className="w-40 h-8 text-sm"
                        value={perItem[item.id] ?? ''}
                        onChange={(e) => setPerItem(prev => ({ ...prev, [item.id]: e.target.value }))}
                      />
                      <span className="text-xs text-muted-foreground w-10 shrink-0">units</span>
                    </div>
                  )
                })}
              </div>
              <p className="text-xs text-muted-foreground">
                Fires when any item's running cycle quantity crosses its cap. Decimal allowed (cached-token ratios, GPU-hours).
              </p>
            </div>
          )}

          <DialogFooter className="gap-2 sm:justify-between">
            {existing ? (
              <Button type="button" variant="destructive" onClick={handleClear} disabled={submitting}>
                Clear thresholds
              </Button>
            ) : <span />}
            <div className="flex gap-2">
              <Button type="button" variant="outline" onClick={onClose} disabled={submitting}>Cancel</Button>
              <Button type="submit" disabled={submitting}>
                {submitting ? (
                  <><Loader2 size={14} className="animate-spin mr-2" />Saving...</>
                ) : 'Save'}
              </Button>
            </div>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

// RadioCard renders a selectable option for choice-dialogs (Cancel,
// Pause). Industry-standard shape: visible radio dot, title + sub-line,
// clear selected/destructive states. Local to this file until a third
// use case appears — premature to extract to /ui.
function RadioCard({
  selected, onClick, disabled, destructive, title, description,
}: {
  selected: boolean
  onClick: () => void
  disabled?: boolean
  destructive?: boolean
  title: string
  description: string
}) {
  return (
    <button
      type="button"
      role="radio"
      aria-checked={selected}
      onClick={onClick}
      disabled={disabled}
      className={cn(
        'w-full text-left rounded-lg border p-3 transition-colors flex items-start gap-3',
        selected
          ? destructive
            ? 'border-destructive bg-destructive/5'
            : 'border-foreground bg-muted/40'
          : 'border-border hover:border-foreground/40',
        disabled && 'opacity-50 cursor-not-allowed',
      )}
    >
      <span
        className={cn(
          'mt-0.5 h-4 w-4 rounded-full border-2 flex items-center justify-center shrink-0',
          selected
            ? destructive ? 'border-destructive' : 'border-foreground'
            : 'border-muted-foreground',
        )}
        aria-hidden="true"
      >
        {selected && (
          <span className={cn('h-2 w-2 rounded-full', destructive ? 'bg-destructive' : 'bg-foreground')} />
        )}
      </span>
      <span className="min-w-0 flex-1">
        <span className={cn('block text-sm font-medium', destructive && selected && 'text-destructive')}>{title}</span>
        <span className="block text-xs text-muted-foreground mt-0.5 whitespace-normal">{description}</span>
      </span>
    </button>
  )
}
