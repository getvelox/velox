import { useState } from 'react'
import { usePageTitle } from '@/hooks/usePageTitle'
import { useParams, Link, useNavigate } from 'react-router-dom'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'
import { api, formatCents, formatDateTime } from '@/lib/api'
import type { MeterPricingRule, MeterAggregationMode, RatingRule } from '@/lib/api'
import { showApiError } from '@/lib/formErrors'
import { money, priceRate, unitScale } from '@/lib/priceDisplay'
import { Layout } from '@/components/Layout'
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
import { TypedConfirmDialog } from '@/components/TypedConfirmDialog'

import { Loader2, Plus, Trash2 } from 'lucide-react'
import { CopyButton } from '@/components/CopyButton'
import { DetailBreadcrumb } from '@/components/DetailBreadcrumb'
import { DetailSkeleton } from '@/components/ui/DetailSkeleton'

const statusVariant = statusBadgeVariant

const AGGREGATION_MODES: { value: MeterAggregationMode; label: string; help: string }[] = [
  { value: 'sum', label: 'Sum', help: 'Add all event values in the period.' },
  { value: 'count', label: 'Count', help: 'Count matching events; ignore values.' },
  { value: 'last_during_period', label: 'Last value in period', help: 'Take the latest event in the period.' },
  { value: 'last_ever', label: 'Last value ever', help: 'Take the latest event across all time (state-style billing).' },
  { value: 'max', label: 'Maximum', help: 'Take the largest value in the period.' },
]

// Human label for an aggregation mode — operator copy never shows the raw
// snake_case enum.
const aggregationLabel = (mode: string): string =>
  AGGREGATION_MODES.find(m => m.value === mode)?.label ?? mode

export default function MeterDetailPage() {
  const { id } = useParams<{ id: string }>()
  const navigate = useNavigate()
  const queryClient = useQueryClient()

  const [createOpen, setCreateOpen] = useState(false)
  const [defaultRuleOpen, setDefaultRuleOpen] = useState(false)
  const [deleteTarget, setDeleteTarget] = useState<MeterPricingRule | null>(null)

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

  // Multi-dimensional pricing rules. Falls back to an empty list when the
  // backend endpoint isn't deployed yet — keeps the page usable on tenants
  // running pre-multi-dim builds.
  const { data: rules } = useQuery({
    queryKey: ['meter-pricing-rules', id],
    queryFn: async () => {
      try {
        const res = await api.listMeterPricingRules(id!)
        return res.data ?? []
      } catch {
        return [] as MeterPricingRule[]
      }
    },
    enabled: !!id,
  })

  const { data: ratingRules } = useQuery({
    queryKey: ['rating-rules-all'],
    queryFn: () => api.listRatingRules(),
  })

  const loading = meterLoading
  const error = meterError instanceof Error ? meterError.message : meterError ? String(meterError) : null

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
    onSuccess: () => {
      toast.success('Pricing rule deleted')
      queryClient.invalidateQueries({ queryKey: ['meter-pricing-rules', id] })
      setDeleteTarget(null)
    },
    onError: (err) => showApiError(err, 'Failed to delete pricing rule'),
  })

  usePageTitle(meter?.name)

  if (loading) {
    return (
      <Layout>
        <DetailSkeleton to="/pricing?tab=meters" parentLabel="Meters" />
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
      <div className="flex items-center justify-between mb-6">
        <div>
          <h1 className="text-2xl font-semibold text-foreground">{meter.name}</h1>
          <div className="flex items-center gap-2 mt-1">
            <span className="text-xs text-muted-foreground font-mono bg-muted px-2 py-0.5 rounded">{meter.id}</span>
            <CopyButton text={meter.id} />
          </div>
        </div>
        <Badge variant="secondary">{aggregationLabel(meter.aggregation)}</Badge>
      </div>

      {/* Properties */}
      <Card>
        <CardHeader>
          <CardTitle className="text-sm">Properties</CardTitle>
        </CardHeader>
        <CardContent className="p-0">
          <div className="divide-y divide-border">
            <div className="flex items-center justify-between px-6 py-3">
              <span className="text-sm text-muted-foreground">Key</span>
              <span className="text-sm text-foreground font-mono">{meter.key}</span>
            </div>
            <div className="flex items-center justify-between px-6 py-3">
              <span className="text-sm text-muted-foreground">Unit</span>
              <span className="text-sm text-foreground">{meter.unit}</span>
            </div>
            <div className="flex items-center justify-between px-6 py-3">
              <span className="text-sm text-muted-foreground">Default aggregation</span>
              <Badge variant="secondary">{aggregationLabel(meter.aggregation)}</Badge>
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
              <CardTitle className="text-sm">Default pricing rule</CardTitle>
              <p className="text-xs text-muted-foreground mt-0.5">
                Applies to events not claimed by a dimension-matched rule below.
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
                {meter.rating_rule_version_id ? 'Change' : 'Link rule'}
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
            <p className="text-sm text-muted-foreground text-center py-4">No pricing rule linked</p>
          )}
        </CardContent>
      </Card>

      {/* Dimension-matched pricing rules (multi-dim) */}
      <Card className="mt-6">
        <CardHeader>
          <div className="flex items-center justify-between">
            <div>
              <CardTitle className="text-sm">Dimension-matched rules</CardTitle>
              <p className="text-xs text-muted-foreground mt-0.5">
                Pricing rules that match a subset of event dimensions. Higher priority claims events first.
              </p>
            </div>
            <Button size="sm" onClick={() => setCreateOpen(true)}>
              <Plus size={14} className="mr-1.5" /> Add rule
            </Button>
          </div>
        </CardHeader>
        <CardContent className="p-0">
          {rules && rules.length > 0 ? (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead className="text-xs font-medium w-20">Priority</TableHead>
                  <TableHead className="text-xs font-medium">Dimension match</TableHead>
                  <TableHead className="text-xs font-medium w-40">Aggregation</TableHead>
                  <TableHead className="text-xs font-medium">Rating rule</TableHead>
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
                      <TableCell>
                        <Badge variant="secondary" className="text-xs">{aggregationLabel(rule.aggregation_mode)}</Badge>
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
                          aria-label="Delete pricing rule"
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
                No dimension-matched rules. Every event uses the default pricing rule above.
              </p>
              <Button variant="outline" size="sm" className="mt-3" onClick={() => setCreateOpen(true)}>
                <Plus size={14} className="mr-1.5" /> Add a rule
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
                    <TableCell className="text-sm font-medium text-foreground text-right">{formatCents(plan.base_amount_cents)}</TableCell>
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
          onOpenChange={(open) => { if (!open) setDeleteTarget(null) }}
          title="Delete pricing rule?"
          description={
            <>
              This rule stops applying to new events at finalize time. Invoices
              already finalized are unaffected. Type <span className="font-mono">delete</span> to confirm.
            </>
          }
          confirmWord="delete"
          confirmLabel="Delete rule"
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

  const saveMutation = useMutation({
    mutationFn: (ruleId: string) => api.updateMeter(meterId, { rating_rule_version_id: ruleId }),
    onSuccess: (_, ruleId) => {
      toast.success(ruleId ? 'Default pricing rule linked' : 'Default pricing rule unlinked')
      onSaved()
    },
    onError: (err) => showApiError(err, 'Failed to update default pricing rule'),
  })

  return (
    <Dialog open onOpenChange={(open) => { if (!open) onClose() }}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Default pricing rule</DialogTitle>
          <DialogDescription>
            Prices events that no dimension-matched rule claims. Takes effect at the
            next invoice close; without it, unmatched usage is not billed.
          </DialogDescription>
        </DialogHeader>

        <div>
          <Label className="text-xs uppercase tracking-wide text-muted-foreground">Rating rule</Label>
          <Select
            items={ratingRules.map(rr => ({ value: rr.id, label: `${rr.name} — ${priceRate(rr, meterUnit)} · v${rr.version}` }))}
            value={ratingRuleId}
            onValueChange={(v) => setRatingRuleId(v ?? '')}
          >
            <SelectTrigger className="mt-2 w-full">
              <SelectValue placeholder="Select rating rule…" />
            </SelectTrigger>
            <SelectContent>
              {ratingRules.length === 0 ? (
                <div className="px-3 py-2 text-sm text-muted-foreground">No rating rules — create one first.</div>
              ) : (
                ratingRules.map(rr => (
                  <SelectItem key={rr.id} value={rr.id}>
                    <span className="text-sm">{rr.name}</span>
                    <span className="text-xs text-muted-foreground ml-2">{rr.mode} · v{rr.version}</span>
                  </SelectItem>
                ))
              )}
            </SelectContent>
          </Select>
        </div>

        <DialogFooter className={currentRuleId ? 'sm:justify-between' : undefined}>
          {currentRuleId && (
            <Button
              variant="ghost"
              className="text-destructive hover:text-destructive"
              onClick={() => saveMutation.mutate('')}
              disabled={saveMutation.isPending}
            >
              Unlink rule
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
  const [aggregation, setAggregation] = useState<MeterAggregationMode>('sum')
  const [ratingRuleId, setRatingRuleId] = useState<string>('')
  const [priority, setPriority] = useState<number>(100)

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
        aggregation_mode: aggregation,
        priority,
      })
    },
    onSuccess: () => {
      toast.success('Pricing rule created')
      onCreated()
    },
    onError: (err) => showApiError(err, 'Failed to create pricing rule'),
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
          <DialogTitle>Add dimension-matched pricing rule</DialogTitle>
          <DialogDescription>
            Match events whose dimensions are a superset of the keys below. Leave empty to match every event.
          </DialogDescription>
        </DialogHeader>

        <div className="space-y-5">
          <div>
            <Label className="text-xs uppercase tracking-wide text-muted-foreground">Dimension match</Label>
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
                      <TooltipContent>A meter requires at least one dimension.</TooltipContent>
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
              Values <span className="font-mono">true</span>, <span className="font-mono">false</span>, and numeric strings are coerced; everything else is treated as a string.
            </p>
          </div>

          <div className="grid grid-cols-2 gap-4">
            <div>
              <Label className="text-xs uppercase tracking-wide text-muted-foreground">Aggregation</Label>
              <Select
                items={AGGREGATION_MODES.map(m => ({ value: m.value, label: m.label }))}
                value={aggregation}
                onValueChange={(v) => setAggregation(v as MeterAggregationMode)}
              >
                <SelectTrigger className="mt-2 w-full">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {AGGREGATION_MODES.map(m => (
                    <SelectItem key={m.value} value={m.value}>
                      <span className="font-mono">{m.label}</span>
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
              <p className="text-xs text-muted-foreground mt-1">
                {AGGREGATION_MODES.find(m => m.value === aggregation)?.help}
              </p>
            </div>
            <div>
              <Label className="text-xs uppercase tracking-wide text-muted-foreground">Priority</Label>
              <Input
                type="number"
                value={priority}
                onChange={(e) => setPriority(parseInt(e.target.value || '0', 10) || 0)}
                className="mt-2 font-mono"
              />
              <p className="text-xs text-muted-foreground mt-1">Higher priority claims events first.</p>
            </div>
          </div>

          <div>
            <Label className="text-xs uppercase tracking-wide text-muted-foreground">Rating rule</Label>
            <Select
              items={ratingRules.map(rr => ({ value: rr.id, label: `${rr.name} — ${priceRate(rr, meterUnit)} · v${rr.version}` }))}
              value={ratingRuleId}
              onValueChange={(v) => setRatingRuleId(v ?? '')}
            >
              <SelectTrigger className="mt-2 w-full">
                <SelectValue placeholder="Select rating rule…" />
              </SelectTrigger>
              <SelectContent>
                {ratingRules.length === 0 ? (
                  <div className="px-3 py-2 text-sm text-muted-foreground">No rating rules — create one first.</div>
                ) : (
                  ratingRules.map(rr => (
                    <SelectItem key={rr.id} value={rr.id}>
                      <span className="text-sm">{rr.name}</span>
                      <span className="text-xs text-muted-foreground ml-2">{rr.mode} · v{rr.version}</span>
                    </SelectItem>
                  ))
                )}
              </SelectContent>
            </Select>
          </div>
        </div>

        <DialogFooter>
          <Button variant="outline" onClick={onClose}>Cancel</Button>
          <Button onClick={() => createMutation.mutate()} disabled={!canSubmit}>
            {createMutation.isPending ? <Loader2 size={14} className="mr-1.5 animate-spin" /> : null}
            Create rule
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

function parseDimensionValue(raw: string): string | number | boolean {
  const trimmed = raw.trim()
  if (trimmed === 'true') return true
  if (trimmed === 'false') return false
  if (trimmed !== '' && !isNaN(Number(trimmed))) return Number(trimmed)
  return raw
}
