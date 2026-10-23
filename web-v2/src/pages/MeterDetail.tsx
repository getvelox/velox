import { useState } from 'react'
import { usePageTitle } from '@/hooks/usePageTitle'
import { useParams, Link, useNavigate } from 'react-router-dom'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'
import { api, formatCents, formatDateTime } from '@/lib/api'
import type { Meter, MeterPricingRule, RatingRule } from '@/lib/api'
import { showApiError } from '@/lib/formErrors'
import { money, priceRate, unitScale } from '@/lib/priceDisplay'
import { latestPerKey } from '@/lib/rules'
import { deleteMatchedPriceSentence, METER_IN_USE_TITLE, meterInUseMessage } from '@/lib/pricingCopy'
import {
  METER_AGGREGATIONS, METER_HAS_USAGE_SENTENCE, METER_NAME_UNIT_SENTENCE, METER_NO_USAGE_SENTENCE,
  meterAggregationHelp, meterAggregationLabel, meterHasUsageMessage,
} from '@/lib/meterLabels'
import { Layout } from '@/components/Layout'
import { LoadError } from '@/components/LoadError'
import { Callout } from '@/components/Callout'
import { statusBadgeVariant } from '@/lib/status'

import { Button } from '@/components/ui/button'
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip'
import { Badge } from '@/components/ui/badge'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from '@/components/ui/table'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { Combobox } from '@/components/Combobox'
import { TypedConfirmDialog } from '@/components/TypedConfirmDialog'

import { Loader2, Lock, Pencil, Plus, Trash2 } from 'lucide-react'
import { CopyButton } from '@/components/CopyButton'
import { DetailBreadcrumb } from '@/components/DetailBreadcrumb'
import { DetailSkeleton } from '@/components/ui/DetailSkeleton'

const statusVariant = statusBadgeVariant

export default function MeterDetailPage() {
  const { id } = useParams<{ id: string }>()
  const navigate = useNavigate()
  const queryClient = useQueryClient()

  const [createOpen, setCreateOpen] = useState(false)
  const [editOpen, setEditOpen] = useState(false)
  const [defaultRuleOpen, setDefaultRuleOpen] = useState(false)
  const [deleteTarget, setDeleteTarget] = useState<MeterPricingRule | null>(null)
  const [deleteInUse, setDeleteInUse] = useState<string | null>(null)

  const { data: meter, isLoading: meterLoading, error: meterError, refetch } = useQuery({
    queryKey: ['meter', id],
    queryFn: () => api.getMeter(id!),
    enabled: !!id,
  })

  const { data: ratingRule } = useQuery({
    queryKey: ['meter-rating-rule', meter?.rating_rule_version_id],
    queryFn: async () => {
      if (!meter?.rating_rule_version_id) return null
      const linkedRule = await api.getRatingRule(meter.rating_rule_version_id)
      try {
        const allRules = await api.listRatingRules()
        const sameKey = allRules.data.filter(r => r.rule_key === linkedRule.rule_key)
        if (sameKey.length > 0) {
          return sameKey.reduce((a, b) => b.version > a.version ? b : a)
        }
        return linkedRule
      } catch {
        return linkedRule
      }
    },
    enabled: !!meter?.rating_rule_version_id,
  })

  const { data: plans } = useQuery({
    queryKey: ['meter-plans', id],
    queryFn: async () => {
      const res = await api.listPlans()
      return res.data.filter(p => p.meter_ids?.includes(id!))
    },
    enabled: !!id,
  })

  // Multi-dimensional pricing rules. A failed load shows as a load error,
  // never as an empty list: "No dimension-matched rules" on a failure told
  // the operator every event used the default price when that was unknown.
  const { data: rules, error: rulesError, refetch: refetchRules } = useQuery({
    queryKey: ['meter-pricing-rules', id],
    queryFn: async (): Promise<MeterPricingRule[]> => {
      const res = await api.listMeterPricingRules(id!)
      return res.data ?? []
    },
    enabled: !!id,
  })

  const { data: ratingRules } = useQuery({
    queryKey: ['rating-rules-all'],
    queryFn: () => api.listRatingRules(),
  })

  const loading = meterLoading

  const graduatedTierLabel = (tier: { up_to: number }, index: number, tiers: { up_to: number }[]) => {
    const prev = index > 0 ? tiers[index - 1].up_to : 0
    if (tier.up_to === 0 || tier.up_to === -1) {
      return `Beyond ${prev.toLocaleString()} units`
    }
    if (index === 0) {
      return `First ${tier.up_to.toLocaleString()} units`
    }
    return `Next ${(tier.up_to - prev).toLocaleString()} units`
  }

  const deleteMutation = useMutation({
    mutationFn: (ruleId: string) => api.deleteMeterPricingRule(id!, ruleId),
    onMutate: () => setDeleteInUse(null),
    onSuccess: () => {
      toast.success('Price removed from meter')
      queryClient.invalidateQueries({ queryKey: ['meter-pricing-rules', id] })
      setDeleteTarget(null)
    },
    onError: (err) => {
      const inUse = meterInUseMessage(err)
      if (inUse) setDeleteInUse(inUse)
      else showApiError(err, 'Failed to remove price')
    },
  })

  usePageTitle(meter?.name)

  if (loading) {
    return (
      <Layout>
        <DetailSkeleton to="/pricing?tab=meters" parentLabel="Meters" />
      </Layout>
    )
  }

  if (meterError) {
    return (
      <Layout>
        <LoadError what="this meter" error={meterError} onRetry={() => refetch()} className="py-16" />
      </Layout>
    )
  }

  if (!meter) {
    return (
      <Layout>
        <p className="text-sm text-muted-foreground py-16 text-center">Meter not found</p>
      </Layout>
    )
  }

  return (
    <Layout>
      <DetailBreadcrumb to="/pricing?tab=meters" parentLabel="Meters" currentLabel={meter.name} />

      {/* Header */}
      <div className="flex flex-wrap items-start justify-between gap-x-4 gap-y-3 mb-6">
        <div className="min-w-0">
          <h1 className="text-2xl font-semibold text-foreground">{meter.name}</h1>
          <div className="flex items-center gap-2 mt-1">
            <span className="text-xs text-muted-foreground font-mono bg-muted px-2 py-0.5 rounded">{meter.id}</span>
            <CopyButton text={meter.id} />
          </div>
        </div>
        <Badge variant="secondary">{meterAggregationLabel(meter.aggregation)}</Badge>
      </div>

      {/* Properties */}
      <Card>
        <CardHeader>
          <div className="flex items-center justify-between gap-3">
            <CardTitle className="text-sm">Properties</CardTitle>
            <Button variant="outline" size="sm" onClick={() => setEditOpen(true)}>
              <Pencil size={14} className="mr-1.5" /> Edit
            </Button>
          </div>
        </CardHeader>
        <CardContent className="p-0">
          <div className="divide-y divide-border">
            <div className="flex items-center justify-between gap-3 px-6 py-3">
              <span className="text-sm text-muted-foreground">Event name</span>
              <span className="text-sm text-foreground font-mono break-all text-right">{meter.event_name}</span>
            </div>
            <div className="flex items-center justify-between px-6 py-3">
              <span className="text-sm text-muted-foreground">Unit</span>
              <span className="text-sm text-foreground">{meter.unit}</span>
            </div>
            <div className="px-6 py-3">
              <div className="flex items-center justify-between">
                <span className="text-sm text-muted-foreground">Aggregation</span>
                <Badge variant="secondary">{meterAggregationLabel(meter.aggregation)}</Badge>
              </div>
              <p className="text-xs text-muted-foreground mt-1.5 flex items-start gap-1.5" data-testid="meter-aggregation-lock">
                {meter.has_usage && <Lock size={12} className="mt-0.5 shrink-0" aria-hidden />}
                <span>
                  {meterAggregationHelp(meter.aggregation)}{' '}
                  {meter.has_usage ? METER_HAS_USAGE_SENTENCE : METER_NO_USAGE_SENTENCE}
                </span>
              </p>
            </div>
            <div className="flex items-center justify-between px-6 py-3">
              <span className="text-sm text-muted-foreground">Created</span>
              <span className="text-sm text-foreground">{formatDateTime(meter.created_at)}</span>
            </div>
            <div className="flex items-center justify-between px-6 py-3">
              <span className="text-sm text-muted-foreground">ID</span>
              <div className="flex items-center gap-2">
                <span className="text-sm text-foreground font-mono truncate max-w-xs" title={meter.id}>{meter.id}</span>
                <CopyButton text={meter.id} />
              </div>
            </div>
          </div>
        </CardContent>
      </Card>

      {/* Default pricing rule */}
      <Card className="mt-6">
        <CardHeader>
          <div className="flex items-center justify-between">
            <div>
              <CardTitle className="text-sm">Default price</CardTitle>
              <p className="text-xs text-muted-foreground mt-0.5">
                Applies to events that no price below matches.
              </p>
              {ratingRule && (
                <p className="text-sm text-muted-foreground mt-2">{ratingRule.name}</p>
              )}
            </div>
            <div className="flex items-center gap-2">
              {ratingRule && (
                <Badge variant="secondary">v{ratingRule.version}</Badge>
              )}
              <Button variant="outline" size="sm" onClick={() => setDefaultRuleOpen(true)}>
                {meter.rating_rule_version_id ? 'Change' : 'Link a price'}
              </Button>
            </div>
          </div>
        </CardHeader>
        <CardContent>
          {ratingRule ? (
            <div>
              <div className="flex items-center gap-2 mb-4">
                <Badge variant="info">{ratingRule.mode}</Badge>
                {ratingRule.currency && (
                  <span className="text-xs text-muted-foreground font-medium uppercase">{ratingRule.currency}</span>
                )}
              </div>

              {ratingRule.mode === 'flat' && (
                <div>
                  <span className="text-3xl font-semibold text-foreground">{money(ratingRule.flat_amount_cents, unitScale(meter.unit).scale, ratingRule.currency)}</span>
                  <span className="text-sm text-muted-foreground ml-2">per {unitScale(meter.unit).per}</span>
                </div>
              )}

              {ratingRule.mode === 'graduated' && ratingRule.graduated_tiers && (
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead>Tier</TableHead>
                      <TableHead className="text-right">Price / {unitScale(meter.unit).per}</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {ratingRule.graduated_tiers.map((tier, i) => (
                      <TableRow key={i}>
                        <TableCell>{graduatedTierLabel(tier, i, ratingRule.graduated_tiers!)}</TableCell>
                        <TableCell className="text-right">{money(tier.unit_amount_cents, unitScale(meter.unit).scale, ratingRule.currency)}</TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              )}

              {ratingRule.mode === 'package' && (
                <div>
                  <span className="text-3xl font-semibold text-foreground">{ratingRule.package_size.toLocaleString()}</span>
                  <span className="text-sm text-muted-foreground ml-2">{meter.unit || 'units'} per package at</span>
                  <span className="text-lg font-semibold text-foreground ml-1">{formatCents(ratingRule.package_amount_cents, ratingRule.currency)}</span>
                  {ratingRule.overage_unit_amount_cents && ratingRule.overage_unit_amount_cents !== '0' && (
                    <p className="text-sm text-muted-foreground mt-1">
                      Beyond full packages: {money(ratingRule.overage_unit_amount_cents, unitScale(meter.unit).scale, ratingRule.currency)} per {unitScale(meter.unit).per}
                    </p>
                  )}
                </div>
              )}
            </div>
          ) : (
            <div className="text-center py-4">
              <p className="text-sm text-muted-foreground">
                No default price. Usage that no price below matches <span className="text-amber-500">won't be billed</span>.
              </p>
            </div>
          )}
        </CardContent>
      </Card>

      {/* Dimension-matched pricing rules (multi-dim) */}
      <Card className="mt-6">
        <CardHeader>
          <div className="flex items-center justify-between">
            <div>
              <CardTitle className="text-sm">Prices for specific values</CardTitle>
              <p className="text-xs text-muted-foreground mt-0.5">
                Each prices the events whose values match, such as model = gpt-4, under the meter's aggregation. When an event matches more than one, the higher priority prices it.
              </p>
            </div>
            <Button size="sm" onClick={() => setCreateOpen(true)}>
              <Plus size={14} className="mr-1.5" /> Add price
            </Button>
          </div>
        </CardHeader>
        <CardContent className="p-0">
          {rulesError ? (
            <LoadError what="this meter's prices" error={rulesError} onRetry={() => refetchRules()} />
          ) : !rules ? (
            <div className="flex justify-center px-6 py-8">
              <Loader2 size={16} className="animate-spin text-muted-foreground" aria-label="Loading" />
            </div>
          ) : rules.length > 0 ? (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead className="text-xs font-medium w-20">Priority</TableHead>
                  <TableHead className="text-xs font-medium">Only for events with these values</TableHead>
                  <TableHead className="text-xs font-medium">Price</TableHead>
                  <TableHead className="text-xs font-medium w-12 text-right" />
                </TableRow>
              </TableHeader>
              <TableBody>
                {rules.map(rule => {
                  const ratingRuleObj = ratingRules?.data.find(r => r.id === rule.rating_rule_version_id)
                  const ratingRuleName = ratingRuleObj?.name
                  return (
                    <TableRow key={rule.id}>
                      <TableCell className="text-sm font-mono tabular-nums">{rule.priority}</TableCell>
                      <TableCell>
                        <div className="flex flex-wrap gap-1">
                          {Object.entries(rule.dimension_match).length === 0 ? (
                            <span className="text-xs text-muted-foreground italic">all events</span>
                          ) : (
                            Object.entries(rule.dimension_match).map(([k, v]) => (
                              <span key={k} className="inline-flex items-center text-xs font-mono bg-muted px-1.5 py-0.5 rounded">
                                {k}=<span className="text-foreground">{String(v)}</span>
                              </span>
                            ))
                          )}
                        </div>
                      </TableCell>
                      <TableCell className="text-sm text-muted-foreground">
                        {ratingRuleName ? (
                          <span title={rule.rating_rule_version_id}>
                            {ratingRuleName}
                            {ratingRuleObj && <span className="text-foreground"> — {priceRate(ratingRuleObj, meter.unit)}</span>}
                          </span>
                        ) : (
                          <span className="font-mono text-xs">{rule.rating_rule_version_id.slice(0, 16)}…</span>
                        )}
                      </TableCell>
                      <TableCell className="text-right">
                        <Button
                          variant="ghost"
                          size="icon"
                          className="h-7 w-7 text-muted-foreground hover:text-destructive"
                          onClick={() => setDeleteTarget(rule)}
                          aria-label="Remove price from meter"
                        >
                          <Trash2 size={14} />
                        </Button>
                      </TableCell>
                    </TableRow>
                  )
                })}
              </TableBody>
            </Table>
          ) : (
            <div className="px-6 py-8 text-center">
              <p className="text-sm text-muted-foreground">
                {meter.rating_rule_version_id
                  ? 'No prices for specific values. Every event uses the default price above.'
                  : 'No prices for specific values, and no default price, so usage on this meter isn\'t billed.'}
              </p>
              <Button variant="outline" size="sm" className="mt-3" onClick={() => setCreateOpen(true)}>
                <Plus size={14} className="mr-1.5" /> Add a price
              </Button>
            </div>
          )}
        </CardContent>
      </Card>

      {/* Plans */}
      <Card className="mt-6">
        <CardHeader>
          <CardTitle className="text-sm">
            Used by {plans?.length ?? 0} plan{(plans?.length ?? 0) !== 1 ? 's' : ''}
          </CardTitle>
        </CardHeader>
        <CardContent className="p-0">
          {plans && plans.length > 0 ? (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Name</TableHead>
                  <TableHead>Code</TableHead>
                  <TableHead>Interval</TableHead>
                  <TableHead className="text-right">Base Price</TableHead>
                  <TableHead>Status</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {plans.map(plan => (
                  <TableRow
                    key={plan.id}
                    className="cursor-pointer hover:bg-muted/50 transition-colors"
                    onClick={(e) => {
                      const target = e.target as HTMLElement
                      if (target.closest('button, a, input, select')) return
                      navigate(`/plans/${plan.id}`)
                    }}
                  >
                    <TableCell>
                      <Link to={`/plans/${plan.id}`} className="text-sm font-medium text-foreground hover:text-primary transition-colors">
                        {plan.name}
                      </Link>
                    </TableCell>
                    <TableCell className="text-sm text-muted-foreground font-mono">{plan.code}</TableCell>
                    <TableCell className="text-sm text-muted-foreground">{plan.billing_interval}</TableCell>
                    <TableCell className="text-sm font-medium text-foreground text-right">{formatCents(plan.base_amount_cents, plan.currency)}</TableCell>
                    <TableCell><Badge variant={statusVariant(plan.status)}>{plan.status}</Badge></TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          ) : (
            <p className="text-sm text-muted-foreground text-center py-8">No plans are currently using this meter</p>
          )}
        </CardContent>
      </Card>

      {editOpen && (
        <EditMeterDialog
          meter={meter}
          onClose={() => setEditOpen(false)}
          onSaved={() => {
            setEditOpen(false)
            queryClient.invalidateQueries({ queryKey: ['meter', id] })
            queryClient.invalidateQueries({ queryKey: ['meters'] })
          }}
          onStale={() => queryClient.invalidateQueries({ queryKey: ['meter', id] })}
        />
      )}

      {createOpen && (
        <CreatePricingRuleDialog
          meterUnit={meter.unit}
          meterId={meter.id}
          ratingRules={ratingRules?.data ?? []}
          onClose={() => setCreateOpen(false)}
          onCreated={() => {
            setCreateOpen(false)
            queryClient.invalidateQueries({ queryKey: ['meter-pricing-rules', meter.id] })
          }}
        />
      )}

      {defaultRuleOpen && (
        <SetDefaultRuleDialog
          meterUnit={meter.unit}
          meterId={meter.id}
          currentRuleId={meter.rating_rule_version_id ?? ''}
          ratingRules={ratingRules?.data ?? []}
          onClose={() => setDefaultRuleOpen(false)}
          onSaved={() => {
            setDefaultRuleOpen(false)
            queryClient.invalidateQueries({ queryKey: ['meter', id] })
            queryClient.invalidateQueries({ queryKey: ['meter-rating-rule'] })
          }}
        />
      )}

      {deleteTarget && (
        <TypedConfirmDialog
          open
          onOpenChange={(open) => { if (!open) { setDeleteTarget(null); setDeleteInUse(null) } }}
          title={`Remove ${ratingRules?.data.find(r => r.id === deleteTarget?.rating_rule_version_id)?.name ?? 'this price'} from this meter?`}
          description={
            <>
              {deleteMatchedPriceSentence(
                Object.entries(deleteTarget?.dimension_match ?? {}).map(([k, v]) => `${k}=${v}`).join(', '),
                !!meter.rating_rule_version_id,
              )}{' '}
              Type <span className="font-mono">remove</span> to confirm.
            </>
          }
          notice={deleteInUse && <MeterInUseNotice message={deleteInUse} />}
          confirmWord="remove"
          confirmLabel="Remove"
          onConfirm={() => deleteMutation.mutate(deleteTarget.id)}
          loading={deleteMutation.isPending}
        />
      )}
    </Layout>
  )
}

function SetDefaultRuleDialog({
  meterId,
  meterUnit,
  currentRuleId,
  ratingRules,
  onClose,
  onSaved,
}: {
  meterId: string
  meterUnit: string
  currentRuleId: string
  ratingRules: RatingRule[]
  onClose: () => void
  onSaved: () => void
}) {
  const [ratingRuleId, setRatingRuleId] = useState<string>(currentRuleId)
  const [inUse, setInUse] = useState<string | null>(null)

  const saveMutation = useMutation({
    mutationFn: (ruleId: string) => api.updateMeter(meterId, { rating_rule_version_id: ruleId }),
    onMutate: () => setInUse(null),
    onSuccess: (_, ruleId) => {
      toast.success(ruleId ? 'Default price linked' : 'Default price unlinked')
      onSaved()
    },
    onError: (err) => {
      const msg = meterInUseMessage(err)
      if (msg) setInUse(msg)
      else showApiError(err, 'Failed to update default price')
    },
  })

  return (
    <Dialog open onOpenChange={(open) => { if (!open) onClose() }}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Default price</DialogTitle>
          <DialogDescription>
            Prices events that no other price on this meter matches, from the next
            invoice. Without it, that usage isn't billed.
          </DialogDescription>
        </DialogHeader>

        <div>
          <Label className="text-xs uppercase tracking-wide text-muted-foreground">Price</Label>
          {/* Searchable: rating rules are TENANT DATA and grow unbounded. */}
          <div className="mt-2">
            <Combobox
              value={ratingRuleId}
              onChange={(v) => setRatingRuleId(v ?? '')}
              options={latestPerKey(ratingRules).map(rr => ({ value: rr.id, label: `${rr.name} — ${priceRate(rr, meterUnit)} · v${rr.version}` }))}
              placeholder="Select a price…"
              emptyMessage={ratingRules.length === 0 ? 'No prices yet. Create one on the Pricing page first.' : 'No price matches that search.'}
              triggerClassName="w-full"
            />
          </div>
        </div>

        {inUse && <MeterInUseNotice message={inUse} />}

        <DialogFooter className={currentRuleId ? 'sm:justify-between' : undefined}>
          {currentRuleId && (
            <Button
              variant="ghost"
              className="text-destructive hover:text-destructive"
              onClick={() => saveMutation.mutate('')}
              disabled={saveMutation.isPending}
            >
              Unlink price
            </Button>
          )}
          <div className="flex gap-2">
            <Button variant="outline" onClick={onClose}>Cancel</Button>
            <Button
              onClick={() => saveMutation.mutate(ratingRuleId)}
              disabled={!ratingRuleId || ratingRuleId === currentRuleId || saveMutation.isPending}
            >
              {saveMutation.isPending ? <Loader2 size={14} className="mr-1.5 animate-spin" /> : null}
              Save
            </Button>
          </div>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

interface DimensionRow {
  key: string
  value: string
}

function CreatePricingRuleDialog({
  meterId,
  meterUnit,
  ratingRules,
  onClose,
  onCreated,
}: {
  meterId: string
  meterUnit: string
  ratingRules: RatingRule[]
  onClose: () => void
  onCreated: () => void
}) {
  const [dimensions, setDimensions] = useState<DimensionRow[]>([{ key: '', value: '' }])
  const [ratingRuleId, setRatingRuleId] = useState<string>('')
  const [priority, setPriority] = useState<number>(100)
  const [inUse, setInUse] = useState<string | null>(null)

  const createMutation = useMutation({
    mutationFn: () => {
      const dimMap: Record<string, string | number | boolean> = {}
      for (const row of dimensions) {
        const k = row.key.trim()
        if (!k) continue
        dimMap[k] = parseDimensionValue(row.value)
      }
      return api.createMeterPricingRule(meterId, {
        rating_rule_version_id: ratingRuleId,
        dimension_match: dimMap,
        priority,
      })
    },
    onMutate: () => setInUse(null),
    onSuccess: () => {
      toast.success('Price added')
      onCreated()
    },
    onError: (err) => {
      const msg = meterInUseMessage(err)
      if (msg) setInUse(msg)
      else showApiError(err, 'Failed to add price')
    },
  })

  const updateRow = (i: number, patch: Partial<DimensionRow>) => {
    setDimensions(rows => rows.map((r, idx) => (idx === i ? { ...r, ...patch } : r)))
  }
  const addRow = () => setDimensions(rows => [...rows, { key: '', value: '' }])
  const removeRow = (i: number) => setDimensions(rows => rows.filter((_, idx) => idx !== i))

  const canSubmit = !!ratingRuleId && !createMutation.isPending

  return (
    <Dialog open onOpenChange={(open) => { if (!open) onClose() }}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>Add a price for specific values</DialogTitle>
          <DialogDescription>
            Applies to events that include all of the values below. Leave empty to match every event.
          </DialogDescription>
        </DialogHeader>

        <div className="space-y-5">
          <div>
            <Label className="text-xs uppercase tracking-wide text-muted-foreground">Only for events with these values</Label>
            <div className="space-y-2 mt-2">
              {dimensions.map((row, i) => (
                <div key={i} className="flex items-center gap-2">
                  <Input
                    placeholder="key (e.g. model)"
                    value={row.key}
                    onChange={(e) => updateRow(i, { key: e.target.value })}
                    className="font-mono text-sm"
                  />
                  <span className="text-muted-foreground">=</span>
                  <Input
                    placeholder="value (e.g. gpt-4)"
                    value={row.value}
                    onChange={(e) => updateRow(i, { value: e.target.value })}
                    className="font-mono text-sm"
                  />
                  {/* shadcn Tooltip — native `title` is unreliable on
                      disabled-button-in-span. See ApiKeys / DatePicker
                      for the same swap. */}
                  {dimensions.length === 1 ? (
                    <Tooltip>
                      <TooltipTrigger render={<span className="inline-block shrink-0 cursor-not-allowed" />}>
                        <Button
                          variant="ghost"
                          size="icon"
                          className="h-9 w-9 text-muted-foreground"
                          disabled
                          aria-label="Remove dimension"
                        >
                          <Trash2 size={14} />
                        </Button>
                      </TooltipTrigger>
                      <TooltipContent>The last row can't be removed — leave it blank to match every event.</TooltipContent>
                    </Tooltip>
                  ) : (
                    <Button
                      variant="ghost"
                      size="icon"
                      className="h-9 w-9 shrink-0 text-muted-foreground"
                      onClick={() => removeRow(i)}
                      aria-label="Remove dimension"
                    >
                      <Trash2 size={14} />
                    </Button>
                  )}
                </div>
              ))}
            </div>
            <Button variant="outline" size="sm" className="mt-2" onClick={addRow}>
              <Plus size={14} className="mr-1.5" /> Add dimension
            </Button>
            <p className="text-xs text-muted-foreground mt-2">
              Values that look like numbers or <span className="font-mono">true</span>/<span className="font-mono">false</span> are matched as such; anything else matches as text.
            </p>
          </div>

          <div>
            <Label className="text-xs uppercase tracking-wide text-muted-foreground">Priority</Label>
            <Input
              type="number"
              value={priority}
              onChange={(e) => setPriority(parseInt(e.target.value || '0', 10) || 0)}
              className="mt-2 font-mono sm:max-w-40"
            />
            <p className="text-xs text-muted-foreground mt-1">When an event matches more than one price, the higher number prices it.</p>
          </div>

          <div>
            <Label className="text-xs uppercase tracking-wide text-muted-foreground">Price</Label>
            {/* Searchable: rating rules are TENANT DATA and grow unbounded. */}
            <div className="mt-2">
              <Combobox
                value={ratingRuleId}
                onChange={(v) => setRatingRuleId(v ?? '')}
                options={latestPerKey(ratingRules).map(rr => ({ value: rr.id, label: `${rr.name} — ${priceRate(rr, meterUnit)} · v${rr.version}` }))}
                placeholder="Select a price…"
                emptyMessage={ratingRules.length === 0 ? 'No prices yet. Create one on the Pricing page first.' : 'No price matches that search.'}
                triggerClassName="w-full"
              />
            </div>
          </div>
        </div>

        {inUse && <MeterInUseNotice message={inUse} />}

        <DialogFooter>
          <Button variant="outline" onClick={onClose}>Cancel</Button>
          <Button onClick={() => createMutation.mutate()} disabled={!canSubmit}>
            {createMutation.isPending ? <Loader2 size={14} className="mr-1.5 animate-spin" /> : null}
            Add price
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

// EditMeterDialog changes the meter's name, unit and aggregation. The
// aggregation is fixed once the meter has usage events (ADR-130 §2): the
// select is then read-only and the dialog says why before any edit. A refusal the
// page did not foresee (the first event arrived while the dialog was open)
// shows the server's sentence and reloads the meter.
function EditMeterDialog({
  meter,
  onClose,
  onSaved,
  onStale,
}: {
  meter: Meter
  onClose: () => void
  onSaved: () => void
  onStale: () => void
}) {
  const [name, setName] = useState(meter.name)
  const [unit, setUnit] = useState(meter.unit)
  const [aggregation, setAggregation] = useState(meter.aggregation)
  const [refusal, setRefusal] = useState<string | null>(null)
  const locked = !!meter.has_usage

  const saveMutation = useMutation({
    mutationFn: () => api.updateMeter(meter.id, {
      name: name.trim(),
      unit: unit.trim(),
      ...(aggregation !== meter.aggregation ? { aggregation } : {}),
    }),
    onMutate: () => setRefusal(null),
    onSuccess: () => {
      toast.success('Meter saved')
      onSaved()
    },
    onError: (err) => {
      const msg = meterHasUsageMessage(err)
      if (msg) {
        setRefusal(msg)
        setAggregation(meter.aggregation)
        onStale()
      } else showApiError(err, 'Failed to save meter')
    },
  })

  const changed = name.trim() !== meter.name || unit.trim() !== meter.unit || aggregation !== meter.aggregation
  const canSave = !!name.trim() && changed && !saveMutation.isPending

  return (
    <Dialog open onOpenChange={(open) => { if (!open) onClose() }}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Edit meter</DialogTitle>
          <DialogDescription>{METER_NAME_UNIT_SENTENCE}</DialogDescription>
        </DialogHeader>

        <div className="space-y-4">
          <div>
            <Label htmlFor="meter-name">Name</Label>
            <Input id="meter-name" value={name} onChange={e => setName(e.target.value)} maxLength={255} className="mt-1" />
          </div>
          <div>
            <Label htmlFor="meter-unit">Unit</Label>
            <Input id="meter-unit" value={unit} onChange={e => setUnit(e.target.value)} className="mt-1" />
          </div>
          <div>
            <Label>Aggregation</Label>
            <Select
              items={METER_AGGREGATIONS.map(a => ({ value: a.value, label: a.label }))}
              value={aggregation}
              onValueChange={(v) => setAggregation(v ?? meter.aggregation)}
              disabled={locked}
            >
              <SelectTrigger className="mt-1 w-full" aria-label="Aggregation">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {METER_AGGREGATIONS.map(a => (
                  <SelectItem key={a.value} value={a.value}>{a.label}</SelectItem>
                ))}
              </SelectContent>
            </Select>
            <p className="text-xs text-muted-foreground mt-1">{meterAggregationHelp(aggregation)}</p>
            <p className="text-xs text-muted-foreground mt-1 flex items-start gap-1.5">
              {locked && <Lock size={12} className="mt-0.5 shrink-0" aria-hidden />}
              <span>{locked ? METER_HAS_USAGE_SENTENCE : METER_NO_USAGE_SENTENCE}</span>
            </p>
          </div>
        </div>

        {refusal && (
          <Callout tone="amber" role="alert" title="Aggregation fixed">{refusal}</Callout>
        )}

        <DialogFooter>
          <Button variant="outline" onClick={onClose}>Cancel</Button>
          <Button onClick={() => saveMutation.mutate()} disabled={!canSave}>
            {saveMutation.isPending ? <Loader2 size={14} className="mr-1.5 animate-spin" /> : null}
            Save
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

// The meter lock's refusal, kept in the dialog that sent the change. The
// server's sentence names the count and the way that waits for the next
// period; the link opens the prices list, where New version is.
function MeterInUseNotice({ message }: { message: string }) {
  return (
    <Callout
      tone="amber"
      role="alert"
      title={METER_IN_USE_TITLE}
      actions={
        <Button variant="outline" size="sm" render={<Link to="/pricing?tab=rules" />}>
          Open prices
        </Button>
      }
    >
      {message}
    </Callout>
  )
}

function parseDimensionValue(raw: string): string | number | boolean {
  const trimmed = raw.trim()
  if (trimmed === 'true') return true
  if (trimmed === 'false') return false
  if (trimmed !== '' && !isNaN(Number(trimmed))) return Number(trimmed)
  return raw
}
