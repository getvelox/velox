import { useState, useMemo } from 'react'
import { usePageTitle } from '@/hooks/usePageTitle'
import { useParams, Link, useNavigate } from 'react-router-dom'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { useForm } from 'react-hook-form'
import { zodResolver } from '@hookform/resolvers/zod'
import { z } from 'zod'
import { toast } from 'sonner'
import { api, formatCents, formatDate, formatDateTime, type Plan, type Meter, type RatingRule, type Customer } from '@/lib/api'
import { applyApiError, showApiError } from '@/lib/formErrors'
import { Layout } from '@/components/Layout'
import { TestClockBadge } from '@/components/TestClockBadge'
import { statusBadgeVariant } from '@/lib/status'

import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import {
  Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle,
} from '@/components/ui/dialog'
import {
  AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent,
  AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle,
} from '@/components/ui/alert-dialog'
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from '@/components/ui/table'
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from '@/components/ui/select'

import { Loader2, Pencil, Plus } from 'lucide-react'
import { CopyButton } from '@/components/CopyButton'
import { DetailBreadcrumb } from '@/components/DetailBreadcrumb'
import { DetailSkeleton } from '@/components/ui/DetailSkeleton'

const statusVariant = statusBadgeVariant

// Static option sets — single source of truth for both the dropdown
// <SelectItem>s and the Base UI `items` prop that lets <SelectValue>
// render the selected option's label (Base UI shows the raw value
// otherwise).
const BILL_TIMING_OPTIONS = [
  { value: 'in_arrears', label: 'At end of period' },
  { value: 'in_advance', label: 'At start of period' },
]
const PLAN_STATUS_OPTIONS = [
  { value: 'active', label: 'Active' },
  { value: 'archived', label: 'Archived' },
]

const editPlanSchema = z.object({
  name: z.string().min(1, 'Name is required'),
})
type EditPlanData = z.infer<typeof editPlanSchema>

export default function PlanDetailPage() {
  const { id } = useParams<{ id: string }>()
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const [showEdit, setShowEdit] = useState(false)
  const [showAttachMeter, setShowAttachMeter] = useState(false)
  const [detachTarget, setDetachTarget] = useState<Meter | null>(null)

  const { data: plan, isLoading, error: loadError, refetch } = useQuery({
    queryKey: ['plan', id],
    queryFn: () => api.getPlan(id!),
    enabled: !!id,
  })

  const { data: meters } = useQuery({
    queryKey: ['meters'],
    queryFn: () => api.listMeters(),
    select: (res) => res.data,
  })

  const { data: ratingRules } = useQuery({
    queryKey: ['rating-rules'],
    queryFn: () => api.listRatingRules(),
    select: (res) => res.data,
  })

  const { data: subsData } = useQuery({
    queryKey: ['plan-subscriptions', id],
    queryFn: () => api.listSubscriptions().then(r => r.data.filter(s => s.items?.some(it => it.plan_id === id))),
    enabled: !!id,
  })

  // ids= of the subscribers actually shown — the old bare 50-row page
  // rendered raw customer ids for anyone past the 50th (P11).
  const planSubCustomerIds = useMemo(
    () => Array.from(new Set((Array.isArray(subsData) ? subsData : []).map(s => s.customer_id).filter(Boolean))),
    [subsData],
  )
  const { data: customersData } = useQuery({
    queryKey: ['customers-map', planSubCustomerIds],
    queryFn: () =>
      api.listCustomers(
        planSubCustomerIds.length > 0
          ? `ids=${planSubCustomerIds.join(',')}&limit=${planSubCustomerIds.length}`
          : '',
      ),
    enabled: planSubCustomerIds.length > 0,
  })
  const customerMap = useMemo(() => {
    const map: Record<string, Customer> = {}
    ;(customersData?.data ?? []).forEach(c => { map[c.id] = c })
    return map
  }, [customersData])

  const subscriptions = Array.isArray(subsData) ? subsData : []
  const allMeters = Array.isArray(meters) ? meters : []
  const allRules = Array.isArray(ratingRules) ? ratingRules : []

  const attachedMeters = useMemo(() => {
    if (!plan?.meter_ids) return []
    return plan.meter_ids
      .map(mid => allMeters.find(m => m.id === mid))
      .filter((m): m is Meter => !!m)
  }, [plan, allMeters])

  const ruleMap = useMemo(() => {
    const map: Record<string, RatingRule> = {}
    allRules.forEach(r => { map[r.id] = r })
    return map
  }, [allRules])

  const activeSubscriptions = useMemo(() =>
    subscriptions.filter(s => s.status === 'active'),
  [subscriptions])

  const unattachedMeters = useMemo(() =>
    allMeters.filter(m => !plan?.meter_ids?.includes(m.id)),
  [allMeters, plan])

  const updatePlanMutation = useMutation({
    mutationFn: (data: { meter_ids: string[] }) => api.updatePlan(id!, data),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['plan', id] })
      queryClient.invalidateQueries({ queryKey: ['plans'] })
    },
  })

  const handleAttachMeter = async (meterId: string) => {
    try {
      await updatePlanMutation.mutateAsync({ meter_ids: [...(plan?.meter_ids || []), meterId] })
      setShowAttachMeter(false)
      toast.success('Meter attached')
    } catch (err) {
      showApiError(err, 'Failed to attach meter')
    }
  }

  const handleDetachMeter = async (meterId: string) => {
    try {
      await updatePlanMutation.mutateAsync({ meter_ids: (plan?.meter_ids || []).filter(mid => mid !== meterId) })
      toast.success('Meter detached')
    } catch (err) {
      showApiError(err, 'Failed to detach meter')
    }
  }

  const loading = isLoading
  const error = loadError instanceof Error ? loadError.message : loadError ? String(loadError) : null

  usePageTitle(plan?.name)

  if (loading) {
    return (
      <Layout>
        <DetailSkeleton to="/pricing" parentLabel="Plans" />
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

  if (!plan) {
    return (
      <Layout>
        <p className="text-sm text-muted-foreground py-16 text-center">Plan not found</p>
      </Layout>
    )
  }

  return (
    <Layout>
      <DetailBreadcrumb to="/pricing" parentLabel="Plans" currentLabel={plan.name} />

      {/* Header */}
      <div className="flex items-center justify-between mb-6">
        <div>
          <h1 className="text-2xl font-semibold text-foreground">{plan.name}</h1>
          <div className="flex items-center gap-2 mt-1">
            <span className="text-xs text-muted-foreground font-mono bg-muted px-2 py-0.5 rounded">{plan.id}</span>
            <CopyButton text={plan.id} />
            <span className="text-xs text-muted-foreground font-mono bg-muted px-2 py-0.5 rounded">{plan.code}</span>
          </div>
        </div>
        <div className="flex items-center gap-3">
          <Badge variant={statusVariant(plan.status)}>{plan.status}</Badge>
          <Button variant="outline" size="sm" onClick={() => setShowEdit(true)}>
            <Pencil size={14} className="mr-1.5" />
            Edit
          </Button>
        </div>
      </div>

      {/* Key metrics */}
      <Card>
        <CardContent className="p-0">
          <div className="flex divide-x divide-border">
            <div className="flex-1 px-6 py-4">
              <p className="text-sm text-muted-foreground">Base Price</p>
              <p className="text-lg font-semibold text-foreground mt-1">{formatCents(plan.base_amount_cents)}</p>
            </div>
            <div className="flex-1 px-6 py-4">
              <p className="text-sm text-muted-foreground">Interval</p>
              <p className="text-lg font-semibold text-foreground mt-1">{plan.billing_interval === 'yearly' ? 'Yearly' : 'Monthly'}</p>
            </div>
            <div className="flex-1 px-6 py-4">
              <p className="text-sm text-muted-foreground">Currency</p>
              <p className="text-lg font-semibold text-foreground mt-1">{plan.currency.toUpperCase()}</p>
            </div>
            <div className="flex-1 px-6 py-4">
              <p className="text-sm text-muted-foreground">Active Subscriptions</p>
              <p className="text-lg font-semibold text-foreground mt-1">{activeSubscriptions.length}</p>
            </div>
          </div>
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
              <span className="text-sm text-muted-foreground">Code</span>
              <span className="text-sm text-foreground font-mono">{plan.code}</span>
            </div>
            <div className="flex items-center justify-between px-6 py-3">
              <span className="text-sm text-muted-foreground">Billing Interval</span>
              <Badge variant="info">{plan.billing_interval === 'yearly' ? 'yearly' : 'monthly'}</Badge>
            </div>
            <div className="flex items-center justify-between px-6 py-3">
              <span className="text-sm text-muted-foreground">Base fee billed</span>
              <span className="text-sm text-foreground font-medium">
                {plan.base_bill_timing === 'in_advance' ? 'At start of period' : 'At end of period'}
              </span>
            </div>
            <div className="flex items-center justify-between px-6 py-3">
              <span className="text-sm text-muted-foreground">Currency</span>
              <span className="text-sm text-foreground font-medium">{plan.currency.toUpperCase()}</span>
            </div>
            <div className="flex items-center justify-between px-6 py-3">
              <span className="text-sm text-muted-foreground">Status</span>
              <Badge variant={statusVariant(plan.status)}>{plan.status}</Badge>
            </div>
            <div className="flex items-center justify-between px-6 py-3">
              <span className="text-sm text-muted-foreground">Created</span>
              <span className="text-sm text-foreground">{formatDateTime(plan.created_at)}</span>
            </div>
            <div className="flex items-center justify-between px-6 py-3">
              <span className="text-sm text-muted-foreground">ID</span>
              <div className="flex items-center gap-2">
                <span className="text-sm text-foreground font-mono">{plan.id}</span>
                <CopyButton text={plan.id} />
              </div>
            </div>
          </div>
        </CardContent>
      </Card>

      {/* Attached Meters */}
      <Card className="mt-6">
        <CardHeader>
          <div className="flex items-center justify-between">
            <CardTitle className="text-sm">Meters ({attachedMeters.length})</CardTitle>
            {unattachedMeters.length > 0 && (
              <Button size="sm" onClick={() => setShowAttachMeter(true)} disabled={updatePlanMutation.isPending}>
                <Plus size={14} className="mr-1.5" />
                Attach Meter
              </Button>
            )}
          </div>
        </CardHeader>
        <CardContent className="p-0">
          {attachedMeters.length > 0 ? (
            <div className="divide-y divide-border">
              {attachedMeters.map(meter => {
                const rule = meter.rating_rule_version_id ? ruleMap[meter.rating_rule_version_id] : null
                return (
                  <div key={meter.id} className="flex items-center justify-between px-6 py-3.5 hover:bg-accent/50 transition-colors group">
                    <Link to={`/meters/${meter.id}`} className="flex-1 min-w-0">
                      <p className="text-sm font-medium text-foreground group-hover:text-primary transition-colors">{meter.name}</p>
                      <p className="text-xs text-muted-foreground font-mono mt-0.5">{meter.key}</p>
                    </Link>
                    <div className="flex items-center gap-2">
                      <Badge variant="secondary">{meter.aggregation}</Badge>
                      <span className="text-xs text-muted-foreground">{meter.unit}</span>
                      {rule && (
                        <span className="text-xs text-muted-foreground bg-muted px-2 py-0.5 rounded-full">{rule.name}</span>
                      )}
                      <Button
                        variant="destructive"
                        size="sm"
                        className="ml-2"
                        onClick={() => setDetachTarget(meter)}
                        disabled={updatePlanMutation.isPending}
                      >
                        Detach
                      </Button>
                    </div>
                  </div>
                )
              })}
            </div>
          ) : (
            <p className="text-sm text-muted-foreground text-center py-8">No meters attached</p>
          )}
        </CardContent>
      </Card>

      {/* Subscriptions */}
      <Card className="mt-6">
        <CardHeader>
          <div className="flex items-center justify-between">
            <CardTitle className="text-sm">Subscriptions ({subscriptions.length})</CardTitle>
            {subscriptions.length > 0 && (
              <Link to="/subscriptions" className="text-sm text-muted-foreground hover:text-foreground transition-colors">
                View all
              </Link>
            )}
          </div>
        </CardHeader>
        <CardContent className="p-0">
          {subscriptions.length > 0 ? (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Name</TableHead>
                  <TableHead>Customer</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead>Current period</TableHead>
                  <TableHead>Created</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {subscriptions.map(sub => (
                  <TableRow
                    key={sub.id}
                    className="cursor-pointer hover:bg-muted/50 transition-colors"
                    onClick={(e) => {
                      const target = e.target as HTMLElement
                      if (target.closest('button, a, input, select')) return
                      navigate(`/subscriptions/${sub.id}`)
                    }}
                  >
                    <TableCell>
                      <span className="inline-flex items-center gap-2">
                        <Link to={`/subscriptions/${sub.id}`} className="text-sm font-medium text-foreground hover:text-primary transition-colors">
                          {sub.display_name}
                        </Link>
                        {sub.test_clock_id && <TestClockBadge testClockId={sub.test_clock_id} />}
                      </span>
                      <p className="text-xs text-muted-foreground font-mono">{sub.code}</p>
                    </TableCell>
                    <TableCell>
                      <Link to={`/customers/${sub.customer_id}`} className="text-sm text-primary hover:underline">
                        {customerMap?.[sub.customer_id]?.display_name || sub.customer_id.slice(0, 8) + '...'}
                      </Link>
                    </TableCell>
                    <TableCell><Badge variant={statusVariant(sub.status)}>{sub.status}</Badge></TableCell>
                    <TableCell className="text-sm text-muted-foreground">
                      {sub.current_billing_period_display || '\u2014'}
                    </TableCell>
                    <TableCell className="text-sm text-muted-foreground">{formatDate(sub.created_at)}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          ) : (
            <p className="text-sm text-muted-foreground text-center py-8">No subscriptions are using this plan yet</p>
          )}
        </CardContent>
      </Card>

      {/* Attach Meter Dialog */}
      <Dialog open={showAttachMeter} onOpenChange={setShowAttachMeter}>
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>Attach Meter</DialogTitle>
          </DialogHeader>
          <p className="text-sm text-muted-foreground mb-4">Select a meter to attach to this plan.</p>
          {unattachedMeters.length > 0 ? (
            <div className="divide-y divide-border border border-border rounded-lg overflow-hidden max-h-64 overflow-y-auto">
              {unattachedMeters.map(meter => (
                <button
                  key={meter.id}
                  onClick={() => handleAttachMeter(meter.id)}
                  disabled={updatePlanMutation.isPending}
                  className="w-full flex items-center justify-between px-4 py-3 hover:bg-accent/50 transition-colors text-left disabled:opacity-50"
                >
                  <div>
                    <p className="text-sm font-medium text-foreground">{meter.name}</p>
                    <p className="text-xs text-muted-foreground font-mono mt-0.5">{meter.key}</p>
                  </div>
                  <div className="flex items-center gap-2">
                    <Badge variant="secondary">{meter.aggregation}</Badge>
                    <span className="text-xs text-muted-foreground">{meter.unit}</span>
                  </div>
                </button>
              ))}
            </div>
          ) : (
            <p className="text-sm text-muted-foreground text-center py-4">All meters are already attached</p>
          )}
        </DialogContent>
      </Dialog>

      {/* Detach Meter Confirm */}
      <AlertDialog open={!!detachTarget} onOpenChange={(open) => { if (!open) setDetachTarget(null) }}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Detach Meter</AlertDialogTitle>
            <AlertDialogDescription>
              Are you sure you want to detach "{detachTarget?.name}" from this plan? Usage events for this meter will still be ingested and visible — but subscriptions on this plan will no longer be billed for it.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel onClick={() => setDetachTarget(null)}>Cancel</AlertDialogCancel>
            <AlertDialogAction
              variant="destructive"
              onClick={() => {
                if (detachTarget) {
                  handleDetachMeter(detachTarget.id)
                  setDetachTarget(null)
                }
              }}
            >
              Detach
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      {/* Edit Plan Dialog */}
      {showEdit && (
        <EditPlanDialog
          plan={plan}
          onClose={() => setShowEdit(false)}
          onSaved={() => {
            setShowEdit(false)
            queryClient.invalidateQueries({ queryKey: ['plan', id] })
            queryClient.invalidateQueries({ queryKey: ['plans'] })
            toast.success('Plan updated')
          }}
        />
      )}
    </Layout>
  )
}

function EditPlanDialog({ plan, onClose, onSaved }: {
  plan: Plan; onClose: () => void; onSaved: () => void
}) {
  const [basePrice, setBasePrice] = useState((plan.base_amount_cents / 100).toFixed(2))
  const [status, setStatus] = useState(plan.status)
  const [billTiming, setBillTiming] = useState<'in_arrears' | 'in_advance'>(plan.base_bill_timing ?? 'in_arrears')

  const form = useForm<EditPlanData>({
    resolver: zodResolver(editPlanSchema),
    defaultValues: { name: plan.name },
  })
  const { register, handleSubmit, watch, formState: { errors, isSubmitting } } = form

  const name = watch('name')
  const hasChanges = name !== plan.name ||
    basePrice !== (plan.base_amount_cents / 100).toFixed(2) ||
    status !== plan.status ||
    billTiming !== (plan.base_bill_timing ?? 'in_arrears')

  const onSubmit = handleSubmit(async (data) => {
    try {
      await api.updatePlan(plan.id, {
        name: data.name,
        base_amount_cents: Math.round(parseFloat(basePrice || '0') * 100),
        base_bill_timing: billTiming,
        status,
      })
      onSaved()
    } catch (err) {
      applyApiError(form, err, ['name'], { toastTitle: 'Failed to update plan' })
    }
  })

  return (
    <Dialog open onOpenChange={(open) => { if (!open) onClose() }}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Edit Plan</DialogTitle>
        </DialogHeader>
        <form onSubmit={onSubmit} noValidate className="space-y-4">
          <div className="bg-muted rounded-lg px-4 py-3 flex items-center justify-between">
            <div>
              <p className="text-xs text-muted-foreground">Code</p>
              <p className="text-sm text-foreground font-mono mt-0.5">{plan.code}</p>
            </div>
            <div className="text-right">
              <p className="text-xs text-muted-foreground">Interval</p>
              <p className="text-sm text-foreground mt-0.5">{plan.billing_interval === 'yearly' ? 'Yearly' : 'Monthly'}</p>
            </div>
          </div>

          <div className="space-y-2">
            <Label htmlFor="name">Plan Name</Label>
            <Input id="name" maxLength={255} {...register('name')} />
            {errors.name && <p className="text-xs text-destructive">{errors.name.message}</p>}
          </div>

          <div className="space-y-2">
            <Label htmlFor="basePrice">Base Price ({plan.currency.toUpperCase()})</Label>
            <Input
              id="basePrice"
              type="number"
              step="0.01"
              min={0}
              max={999999.99}
              value={basePrice}
              onChange={e => setBasePrice(e.target.value)}
              placeholder="49.00"
            />
            <p className="text-xs text-muted-foreground">{plan.billing_interval === 'yearly' ? 'Yearly' : 'Monthly'} recurring charge</p>
          </div>

          <div className="space-y-2">
            <Label>Base fee billed</Label>
            <Select items={BILL_TIMING_OPTIONS} value={billTiming} onValueChange={(v) => setBillTiming((v ?? 'in_arrears') as 'in_arrears' | 'in_advance')}>
              <SelectTrigger className="w-full">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {BILL_TIMING_OPTIONS.map(o => (
                  <SelectItem key={o.value} value={o.value}>{o.label}</SelectItem>
                ))}
              </SelectContent>
            </Select>
            <p className="text-xs text-muted-foreground">
              {billTiming === 'in_advance'
                ? 'Customer charged the base fee on day 1 of each period; usage settles at period end.'
                : 'Customer charged the base fee plus any usage at the end of each period.'}
            </p>
          </div>

          <div className="space-y-2">
            <Label>Status</Label>
            <Select items={PLAN_STATUS_OPTIONS} value={status} onValueChange={(v) => setStatus(v ?? '')}>
              <SelectTrigger className="w-full">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {PLAN_STATUS_OPTIONS.map(o => (
                  <SelectItem key={o.value} value={o.value}>{o.label}</SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>

          <DialogFooter>
            <Button type="button" variant="outline" onClick={onClose}>Cancel</Button>
            <Button type="submit" disabled={isSubmitting || !hasChanges}>
              {isSubmitting ? (
                <><Loader2 size={14} className="animate-spin mr-2" />Saving...</>
              ) : hasChanges ? 'Save' : 'No changes'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
