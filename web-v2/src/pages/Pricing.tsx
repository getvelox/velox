import { Fragment, useState } from 'react'
import { usePageTitle } from '@/hooks/usePageTitle'
import { Link, useNavigate, useSearchParams } from 'react-router-dom'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'
import {
  api,
  formatCents,
  formatDate,
  dollarsToRateCents,
  getCurrencySymbol,
  getActiveCurrency,
  type Meter,
  type RatingRule,
} from '@/lib/api'
import { Layout } from '@/components/Layout'
import { priceRate, shiftDecimal } from '@/lib/priceDisplay'
import { latestPerKey } from '@/lib/rules'
import { meterAggregationLabel } from '@/lib/meterLabels'
import { showApiError } from '@/lib/formErrors'
import { statusBadgeVariant } from '@/lib/status'

import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
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
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { Combobox } from '@/components/Combobox'
import { Checkbox } from '@/components/ui/checkbox'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { TableSkeleton } from '@/components/ui/TableSkeleton'

import { Plus, Trash2, Loader2, Package, Gauge, Calculator } from 'lucide-react'
import { EmptyState } from '@/components/EmptyState'

// Static option sets for the pricing selects. Each is the single source of
// truth for BOTH the dropdown <SelectItem>s AND the Base UI `items` prop.
// Base UI's <SelectValue> renders the raw value in the trigger unless `items`
// maps value→label — the app-wide cause of "trigger shows the code, dropdown
// shows the name".
const PRICING_MODE_OPTIONS = [
  { value: 'flat', label: 'Flat Rate' },
  { value: 'graduated', label: 'Graduated Tiers' },
  { value: 'package', label: 'Package' },
]
const AGGREGATION_OPTIONS = [
  { value: 'sum', label: 'Sum' },
  { value: 'count', label: 'Count' },
  { value: 'max', label: 'Maximum' },
  { value: 'last', label: 'Latest Value' },
]
const INTERVAL_OPTIONS = [
  { value: 'monthly', label: 'Monthly' },
  { value: 'yearly', label: 'Yearly' },
]
const BILL_TIMING_OPTIONS = [
  { value: 'in_arrears', label: 'At end of period (usage-first)' },
  { value: 'in_advance', label: 'At start of period (platform fee + usage)' },
]

function badgeVariant(val: string): 'success' | 'info' | 'secondary' | 'outline' {
  switch (val) {
    case 'active': return 'success'
    case 'monthly': return 'info'
    case 'yearly': return 'info'
    case 'flat': return 'info'
    case 'graduated': return 'info'
    case 'package': return 'info'
    default: return 'secondary'
  }
}

export default function PricingPage() {
  usePageTitle('Pricing')
  const [searchParams, setSearchParams] = useSearchParams()
  const tab = (['plans', 'meters', 'rules'].includes(searchParams.get('tab') || '')
    ? searchParams.get('tab')
    : 'plans') as 'plans' | 'meters' | 'rules'
  const setTab = (t: string) => setSearchParams(t === 'plans' ? {} : { tab: t })
  const [createFor, setCreateFor] = useState<'plans' | 'meters' | 'rules' | null>(null)
  const navigate = useNavigate()
  const queryClient = useQueryClient()

  const { data: plansData, isLoading: plansLoading, error: plansError } = useQuery({
    queryKey: ['plans'],
    queryFn: () => api.listPlans(),
  })
  const { data: metersData, isLoading: metersLoading, error: metersError } = useQuery({
    queryKey: ['meters'],
    queryFn: () => api.listMeters(),
  })
  const { data: rulesData, isLoading: rulesLoading, error: rulesError } = useQuery({
    queryKey: ['rating-rules'],
    queryFn: () => api.listRatingRules(),
  })

  const plans = plansData?.data ?? []
  const meters = metersData?.data ?? []
  const rules = rulesData?.data ?? []

  // unitForRule resolves the unit a rule's rate is quoted in, through BOTH
  // binding edges (a meter's default binding and dimension-matched rows) so
  // recipe-installed token rules display per 1M. Unbound rules fall back to
  // "per unit".
  const { data: bindingsData } = useQuery({
    queryKey: ['meter-pricing-rules'],
    queryFn: () => api.listAllMeterPricingRules(),
  })
  const unitForRule = (ruleID: string): string => {
    const direct = meters.find(m => m.rating_rule_version_id === ruleID)
    if (direct) return direct.unit
    const binding = bindingsData?.data.find(b => b.rating_rule_version_id === ruleID)
    return meters.find(m => m.id === binding?.meter_id)?.unit ?? ''
  }

  // The Rules tab shows one row per PRICE (rule_key) — billing resolves the
  // key's latest active version per period (ADR-070), so historical
  // versions are history, not peers. They stay reachable via expansion.
  const currentRules = latestPerKey(rules)
  const olderVersions = (key: string) =>
    rules
      .filter(r => r.rule_key === key)
      .sort((a, b) => b.version - a.version)
      .filter(r => r.id !== currentRules.find(c => c.rule_key === key)?.id)
  const [expandedKeys, setExpandedKeys] = useState<Set<string>>(new Set())
  const [republishRule, setRepublishRule] = useState<RatingRule | null>(null)
  const [ruleSearch, setRuleSearch] = useState('')
  const visibleRules = currentRules.filter(r =>
    ruleSearch.trim() === '' ||
    r.name.toLowerCase().includes(ruleSearch.toLowerCase()) ||
    r.rule_key.toLowerCase().includes(ruleSearch.toLowerCase()))

  // Used-by: how many meters bill this key (both binding edges), so the
  // blast radius of a republish is visible before publishing.
  const metersUsingRule = (ruleKey: string): number => {
    const versionIDs = new Set(rules.filter(r => r.rule_key === ruleKey).map(r => r.id))
    const ids = new Set<string>()
    for (const m of meters) {
      if (m.rating_rule_version_id && versionIDs.has(m.rating_rule_version_id)) ids.add(m.id)
    }
    for (const b of bindingsData?.data ?? []) {
      if (versionIDs.has(b.rating_rule_version_id)) ids.add(b.meter_id)
    }
    return ids.size
  }
  const loading = plansLoading || metersLoading || rulesLoading
  const error = plansError || metersError || rulesError

  const showAdd =
    (tab === 'plans' && plans.length > 0) ||
    (tab === 'meters' && meters.length > 0) ||
    (tab === 'rules' && rules.length > 0)

  return (
    <Layout>
      <div className="flex justify-between items-center">
        <div>
          <h1 className="text-2xl font-semibold text-foreground">Pricing</h1>
          <p className="text-sm text-muted-foreground mt-1">Configure plans, meters, and rating rules</p>
        </div>
        {showAdd && (
          <Button size="sm" onClick={() => setCreateFor(tab)}>
            <Plus size={16} className="mr-2" />
            {tab === 'plans' ? 'Add Plan' : tab === 'meters' ? 'Add Meter' : 'Add Rule'}
          </Button>
        )}
      </div>

      <Tabs value={tab} onValueChange={setTab} className="mt-6">
        <TabsList>
          <TabsTrigger value="plans">Plans ({plans.length})</TabsTrigger>
          <TabsTrigger value="meters">Meters ({meters.length})</TabsTrigger>
          <TabsTrigger value="rules">Rules ({currentRules.length})</TabsTrigger>
        </TabsList>

        <Card className="mt-4">
          <CardContent className="p-0">
            {error ? (
              <div className="p-8 text-center">
                <p className="text-sm text-destructive mb-3">
                  {error instanceof Error ? error.message : 'Failed to load pricing data'}
                </p>
                <Button variant="outline" size="sm" onClick={() => {
                  // A failure may have come from any of the tab queries —
                  // retrying only one leaves the button dead for the rest.
                  for (const key of ['plans', 'meters', 'rating-rules', 'meter-pricing-rules']) {
                    queryClient.invalidateQueries({ queryKey: [key] })
                  }
                }}>
                  Retry
                </Button>
              </div>
            ) : loading ? (
              <TableSkeleton columns={6} />
            ) : (
              <>
                <TabsContent value="plans">
                  {plans.length === 0 ? (
                    <EmptyState
                      icon={Package}
                      title="No plans yet"
                      description="Create a plan to define pricing and intervals for your product."
                      action={{
                        label: 'Add Plan',
                        icon: Plus,
                        onClick: () => setCreateFor('plans'),
                      }}
                    />
                  ) : (
                    <Table>
                      <TableHeader>
                        <TableRow>
                          <TableHead>Name</TableHead>
                          <TableHead>Code</TableHead>
                          <TableHead>Interval</TableHead>
                          <TableHead>Status</TableHead>
                          <TableHead className="text-right">Base Price</TableHead>
                          <TableHead className="text-right">Meters</TableHead>
                        </TableRow>
                      </TableHeader>
                      <TableBody>
                        {plans.map(p => (
                          <TableRow
                            key={p.id}
                            className="cursor-pointer hover:bg-muted/50 transition-colors"
                            onClick={(e) => {
                              const target = e.target as HTMLElement
                              if (target.closest('button, a, input, select')) return
                              navigate(`/plans/${p.id}`)
                            }}
                          >
                            <TableCell>
                              <Link to={`/plans/${p.id}`} className="font-medium text-foreground hover:text-primary transition-colors">
                                {p.name}
                              </Link>
                            </TableCell>
                            <TableCell className="font-mono text-muted-foreground">{p.code}</TableCell>
                            <TableCell><Badge variant={badgeVariant(p.billing_interval)}>{p.billing_interval}</Badge></TableCell>
                            <TableCell><Badge variant={statusBadgeVariant(p.status)}>{p.status}</Badge></TableCell>
                            <TableCell className="text-right font-medium">{formatCents(p.base_amount_cents, p.currency)}</TableCell>
                            <TableCell className="text-right text-muted-foreground">{p.meter_ids?.length || 0}</TableCell>
                          </TableRow>
                        ))}
                      </TableBody>
                    </Table>
                  )}
                </TabsContent>

                <TabsContent value="meters">
                  {meters.length === 0 ? (
                    <EmptyState
                      icon={Gauge}
                      title="No meters yet"
                      description="Meters track consumption of billable units like API calls, messages, or compute time."
                      action={{
                        label: 'Add Meter',
                        icon: Plus,
                        onClick: () => setCreateFor('meters'),
                      }}
                    />
                  ) : (
                    <Table>
                      <TableHeader>
                        <TableRow>
                          <TableHead>Name</TableHead>
                          <TableHead>Key</TableHead>
                          <TableHead>Unit</TableHead>
                          <TableHead>Pricing</TableHead>
                          <TableHead>Aggregation</TableHead>
                          <TableHead>Created</TableHead>
                        </TableRow>
                      </TableHeader>
                      <TableBody>
                        {meters.map(m => (
                          <TableRow
                            key={m.id}
                            className="cursor-pointer hover:bg-muted/50 transition-colors"
                            onClick={(e) => {
                              const target = e.target as HTMLElement
                              if (target.closest('button, a, input, select')) return
                              navigate(`/meters/${m.id}`)
                            }}
                          >
                            <TableCell>
                              <Link to={`/meters/${m.id}`} className="font-medium text-foreground hover:text-primary transition-colors">
                                {m.name}
                              </Link>
                            </TableCell>
                            <TableCell className="font-mono text-muted-foreground">{m.key}</TableCell>
                            <TableCell className="text-muted-foreground">{m.unit}</TableCell>
                            {(() => {
                              const def = rules.find(r => r.id === m.rating_rule_version_id)
                              const dims = (bindingsData?.data ?? []).filter(b => b.meter_id === m.id).length
                              return (
                                <TableCell className="text-sm">
                                  {def ? (
                                    <span className="text-muted-foreground">{priceRate(def, m.unit)}</span>
                                  ) : dims > 0 ? (
                                    <span className="text-muted-foreground">{dims} dimension price{dims !== 1 ? 's' : ''}</span>
                                  ) : (
                                    <span className="text-amber-500">Not priced — usage won't be billed</span>
                                  )}
                                </TableCell>
                              )
                            })()}
                            <TableCell><Badge variant={badgeVariant(m.aggregation)}>{meterAggregationLabel(m.aggregation)}</Badge></TableCell>
                            <TableCell className="text-muted-foreground">{formatDate(m.created_at)}</TableCell>
                          </TableRow>
                        ))}
                      </TableBody>
                    </Table>
                  )}
                </TabsContent>

                <TabsContent value="rules">
                  {rules.length === 0 ? (
                    <EmptyState
                      icon={Calculator}
                      title="No rating rules yet"
                      description="Rating rules turn metered usage into billable amounts using tiers, volumes, or flat prices."
                      action={{
                        label: 'Add Rule',
                        icon: Plus,
                        onClick: () => setCreateFor('rules'),
                      }}
                    />
                  ) : (
                    <>
                    <div className="px-4 pt-4">
                      <Input
                        value={ruleSearch}
                        onChange={e => setRuleSearch(e.target.value)}
                        placeholder="Search prices by name or key…"
                        aria-label="Search prices"
                        className="max-w-sm"
                      />
                    </div>
                    <Table>
                      <TableHeader>
                        <TableRow>
                          <TableHead>Name</TableHead>
                          <TableHead>Rule Key</TableHead>
                          <TableHead>Mode</TableHead>
                          <TableHead>Version</TableHead>
                          <TableHead>Used by</TableHead>
                          <TableHead className="text-right">Price</TableHead>
                        </TableRow>
                      </TableHeader>
                      <TableBody>
                        {visibleRules.map(r => {
                          const older = olderVersions(r.rule_key)
                          const expanded = expandedKeys.has(r.rule_key)
                          return (
                            <Fragment key={r.rule_key}>
                              <TableRow>
                                <TableCell className="font-medium text-foreground">{r.name}</TableCell>
                                <TableCell className="font-mono text-muted-foreground">{r.rule_key}</TableCell>
                                <TableCell><Badge variant={badgeVariant(r.mode)}>{r.mode}</Badge></TableCell>
                                <TableCell className="text-muted-foreground">
                                  v{r.version}
                                  {r.lifecycle_state === 'archived' && <Badge variant="secondary" className="ml-1.5 text-xs">archived</Badge>}
                                  {older.length > 0 && (
                                    <button
                                      type="button"
                                      className="ml-1.5 text-xs text-muted-foreground underline-offset-2 hover:underline"
                                      aria-label={`${expanded ? 'Hide' : 'Show'} ${older.length} older version${older.length !== 1 ? 's' : ''} of ${r.rule_key}`}
                                      onClick={() => setExpandedKeys(prev => {
                                        const next = new Set(prev)
                                        if (next.has(r.rule_key)) next.delete(r.rule_key); else next.add(r.rule_key)
                                        return next
                                      })}
                                    >
                                      {expanded ? 'hide history' : `+${older.length} older`}
                                    </button>
                                  )}
                                </TableCell>
                                <TableCell className="text-muted-foreground text-sm">
                                  {metersUsingRule(r.rule_key) > 0
                                    ? `${metersUsingRule(r.rule_key)} meter${metersUsingRule(r.rule_key) !== 1 ? 's' : ''}`
                                    : <span className="text-amber-500">unused</span>}
                                </TableCell>
                                <TableCell className="text-right font-medium">
                                  <span>{priceRate(r, unitForRule(r.id))}</span>
                                  <Button
                                    variant="ghost"
                                    size="sm"
                                    className="ml-2 text-xs"
                                    onClick={() => setRepublishRule(r)}
                                  >
                                    New version
                                  </Button>
                                </TableCell>
                              </TableRow>
                              {expanded && older.map(o => (
                                <TableRow key={o.id} className="bg-muted/30">
                                  <TableCell className="text-muted-foreground pl-8">{o.name}</TableCell>
                                  <TableCell className="font-mono text-muted-foreground/70">{o.rule_key}</TableCell>
                                  <TableCell><Badge variant="secondary" className="text-xs">superseded</Badge></TableCell>
                                  <TableCell className="text-muted-foreground">v{o.version}</TableCell>
                                  <TableCell />
                                  <TableCell className="text-right text-muted-foreground">{priceRate(o, unitForRule(o.id))}</TableCell>
                                </TableRow>
                              ))}
                            </Fragment>
                          )
                        })}
                      </TableBody>
                    </Table>
                    </>
                  )}
                </TabsContent>
              </>
            )}
          </CardContent>
        </Card>
      </Tabs>

      {republishRule && (
        <CreateRuleDialog
          rules={rules}
          prefill={republishRule}
          onClose={() => setRepublishRule(null)}
          onCreated={() => {
            setRepublishRule(null)
            queryClient.invalidateQueries({ queryKey: ['rating-rules'] })
            toast.success(`Published v${republishRule.version + 1} of ${republishRule.rule_key}`)
          }}
        />
      )}
      {createFor === 'rules' && (
        <CreateRuleDialog
          rules={rules}
          onClose={() => setCreateFor(null)}
          onCreated={() => {
            setCreateFor(null)
            queryClient.invalidateQueries({ queryKey: ['rating-rules'] })
            toast.success('Rating rule created')
          }}
        />
      )}
      {createFor === 'meters' && (
        <CreateMeterDialog
          rules={rules}
          onClose={() => setCreateFor(null)}
          onCreated={(meterId) => {
            setCreateFor(null)
            queryClient.invalidateQueries({ queryKey: ['meters'] })
            toast.success('Meter created')
            // Land on the meter page — linking prices and adding
            // dimension rules happens there, not on this list.
            if (meterId) navigate(`/meters/${meterId}`)
          }}
        />
      )}
      {createFor === 'plans' && (
        <CreatePlanDialog
          meters={meters}
          rules={rules}
          onClose={() => setCreateFor(null)}
          onCreated={() => {
            setCreateFor(null)
            queryClient.invalidateQueries({ queryKey: ['plans'] })
            toast.success('Plan created')
          }}
        />
      )}
    </Layout>
  )
}

/* ─── Create Rule Dialog ─── */

function CreateRuleDialog({ rules, prefill, onClose, onCreated }: { rules: RatingRule[]; prefill?: RatingRule; onClose: () => void; onCreated: () => void }) {
  // Publishing under the same rule_key reprices from each customer's next
  // period on real time — but for test-clock customers the change lands in
  // the current simulated period (catalog stamps are wall-clock, ADR-070
  // caveat, observed live in the FLOW B9 walk). Surface that whenever the
  // tenant has clocks so simulation results don't silently surprise.
  const { data: clocksData } = useQuery({
    queryKey: ['test-clocks'],
    queryFn: () => api.listTestClocks(),
  })
  const hasClocks = (clocksData?.data ?? []).length > 0
  // Prefill = "publish new version": every field seeds from the current
  // version (rates cents→dollars via exact decimal shift) and the key is
  // locked — repricing must not require re-typing a tier ladder.
  const [mode, setMode] = useState(prefill?.mode ?? 'flat')
  const [flatAmount, setFlatAmount] = useState(prefill ? shiftDecimal(prefill.flat_amount_cents || '0', -2) : '')
  const [tiers, setTiers] = useState(
    prefill?.graduated_tiers?.length
      ? prefill.graduated_tiers.map(t => ({ upTo: t.up_to > 0 ? String(t.up_to) : '', price: shiftDecimal(t.unit_amount_cents || '0', -2) }))
      : [
          { upTo: '100', price: '0.10' },
          { upTo: '', price: '0.05' },
        ],
  )
  const [packageSize, setPackageSize] = useState(prefill ? String(prefill.package_size || 100) : '100')
  const [packageAmount, setPackageAmount] = useState(prefill ? (prefill.package_amount_cents / 100).toFixed(2) : '10.00')
  const [name, setName] = useState(prefill?.name ?? '')
  const [ruleKey, setRuleKey] = useState(prefill?.rule_key ?? '')

  // Live existing-key resolution: drives the version/currency disclosure
  // and the price-label symbols (the submit stamps THIS currency, so the
  // label must never show a different one).
  const existingLatest = (() => {
    const versions = rules.filter(r => r.rule_key === ruleKey.trim())
    return versions.length ? versions.reduce((a, b) => (b.version > a.version ? b : a)) : null
  })()
  const stampCurrency = existingLatest?.currency
  const [tierError, setTierError] = useState('')
  const [saving, setSaving] = useState(false)

  const modeDescriptions: Record<string, string> = {
    flat: 'Fixed price per unit',
    graduated: 'Price decreases as usage increases (tiered)',
    package: 'Charge per block of units',
  }

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    setSaving(true)
    setTierError('')
    try {
      // Publishing under an EXISTING key keeps that key's currency — this
      // form has no currency field, and stamping the tenant's current
      // default would turn a rate edit into a currency-changing republish
      // (refused by the ADR-100 guard) after a Settings currency switch.
      const currency = stampCurrency ?? getActiveCurrency()
      const payload: Record<string, unknown> = { rule_key: ruleKey, name, mode, currency }
      if (mode === 'flat') {
        payload.flat_amount_cents = dollarsToRateCents(flatAmount)
      }
      if (mode === 'graduated') {
        for (let i = 0; i < tiers.length - 1; i++) {
          const current = parseInt(tiers[i].upTo) || 0
          const next = tiers[i + 1].upTo === '' ? Infinity : (parseInt(tiers[i + 1].upTo) || 0)
          if (current <= 0) { setTierError(`Tier ${i + 1}: "Up to" must be greater than 0`); setSaving(false); return }
          if (next !== Infinity && next <= current) { setTierError(`Tier ${i + 2}: "Up to" must be greater than ${current}`); setSaving(false); return }
        }
        payload.graduated_tiers = tiers.map(t => ({
          up_to: t.upTo === '' ? 0 : parseInt(t.upTo) || 0,
          unit_amount_cents: dollarsToRateCents(t.price),
        }))
      }
      if (mode === 'package') {
        payload.package_size = parseInt(packageSize) || 1
        payload.package_amount_cents = Math.round(parseFloat(packageAmount || '0') * 100)
      }
      await api.createRatingRule(payload as Parameters<typeof api.createRatingRule>[0])
      onCreated()
    } catch (err) {
      showApiError(err, 'Failed to create rating rule')
    } finally {
      setSaving(false)
    }
  }

  return (
    <Dialog open onOpenChange={(open) => { if (!open) onClose() }}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>{prefill ? `Publish New Version — ${prefill.rule_key}` : 'Create Rating Rule'}</DialogTitle>
          <DialogDescription>{prefill ? `Currently v${prefill.version}. The new version takes effect from each customer's next billing period.` : 'Define a price. Link it to a meter to bill usage.'}</DialogDescription>
        </DialogHeader>
        <form onSubmit={submit} noValidate className="space-y-4">
          <div>
            <Label htmlFor="rule-name">Name</Label>
            <Input id="rule-name" value={name} onChange={e => setName(e.target.value)} placeholder="API Call Pricing" maxLength={255} className="mt-1" />
          </div>
          <div>
            <Label htmlFor="rule-key">Rule Key</Label>
            <Input id="rule-key" value={ruleKey} onChange={e => setRuleKey(e.target.value)} placeholder="api_calls" maxLength={100} className="mt-1 font-mono" disabled={!!prefill} />
            {existingLatest ? (
              <p className="text-xs text-amber-700 dark:text-amber-500 mt-1">
                Existing price — this publishes v{existingLatest.version + 1} in {existingLatest.currency}, taking effect from each customer's next billing period.
              </p>
            ) : (
              <p className="text-xs text-muted-foreground mt-1">Stable identity for this price. Publishing the same key again creates a new version. To bill usage, link the price to a meter.</p>
            )}
          </div>

          <div className="grid grid-cols-2 gap-3">
            <div>
              <Label>Pricing Model</Label>
              <Select items={PRICING_MODE_OPTIONS} value={mode} onValueChange={(v) => setMode(v ?? '')}>
                <SelectTrigger className="w-full mt-1">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {PRICING_MODE_OPTIONS.map(o => (
                    <SelectItem key={o.value} value={o.value}>{o.label}</SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
          </div>
          <p className="text-xs text-muted-foreground -mt-2">{modeDescriptions[mode]}</p>

          {mode === 'flat' && (
            <div>
              <Label htmlFor="rule-flat-price">Price per Unit ({getCurrencySymbol(stampCurrency)})</Label>
              <Input id="rule-flat-price" type="number" step="any" min={0} max={999999.99} value={flatAmount}
                onChange={e => setFlatAmount(e.target.value)} placeholder="0.01" className="mt-1" />
              <p className="text-xs text-muted-foreground mt-1">e.g. {getCurrencySymbol(stampCurrency)}0.01 per API call. Sub-cent rates allowed.</p>
            </div>
          )}

          {mode === 'graduated' && (
            <div className="space-y-3">
              <Label>Pricing Tiers</Label>
              <div className="rounded-lg border border-border overflow-hidden">
                <div className="grid grid-cols-[1fr_1fr_36px] gap-0 px-3 py-2 border-b border-border bg-muted">
                  <span className="text-xs font-medium text-muted-foreground uppercase tracking-wider">Up to (units)</span>
                  <span className="text-xs font-medium text-muted-foreground uppercase tracking-wider">Price per unit ({getCurrencySymbol(stampCurrency)})</span>
                  <span />
                </div>
                {tiers.map((tier, idx) => (
                  <div key={idx} className="grid grid-cols-[1fr_1fr_36px] gap-0 px-3 py-2 border-b border-border last:border-b-0 items-center">
                    <div className="pr-2">
                      <Input type="number" min={0} value={tier.upTo}
                        onChange={e => setTiers(t => t.map((r, i) => i === idx ? { ...r, upTo: e.target.value } : r))}
                        placeholder={idx === tiers.length - 1 ? 'Unlimited' : '1000'} />
                    </div>
                    <div className="pr-2">
                      <Input type="number" step="any" min={0} value={tier.price}
                        onChange={e => setTiers(t => t.map((r, i) => i === idx ? { ...r, price: e.target.value } : r))}
                        placeholder="0.01" />
                    </div>
                    <div className="flex justify-center">
                      {tiers.length > 1 && (
                        <button type="button" onClick={() => setTiers(t => t.filter((_, i) => i !== idx))}
                          aria-label={`Remove tier ${idx + 1}`}
                          className="text-muted-foreground hover:text-destructive transition-colors">
                          <Trash2 size={14} />
                        </button>
                      )}
                    </div>
                  </div>
                ))}
              </div>
              <button type="button" onClick={() => setTiers(t => [...t, { upTo: '', price: '' }])}
                className="text-sm font-medium text-primary hover:text-primary/80 transition-colors">
                + Add tier
              </button>
              <p className="text-xs text-muted-foreground">Leave "Up to" empty on the last tier for unlimited. Tiers are evaluated in order.</p>
            </div>
          )}

          {mode === 'package' && (
            <div className="grid grid-cols-2 gap-3">
              <div>
                <Label>Units per Package</Label>
                <Input type="number" min={1} value={packageSize} onChange={e => setPackageSize(e.target.value)}
                  placeholder="100" className="mt-1" />
                <p className="text-xs text-muted-foreground mt-1">e.g. 100 API calls per package</p>
              </div>
              <div>
                <Label htmlFor="rule-pkg-price">Price per Package ({getCurrencySymbol(stampCurrency)})</Label>
                <Input id="rule-pkg-price" type="number" step="0.01" min={0} max={999999.99} value={packageAmount}
                  onChange={e => setPackageAmount(e.target.value)} placeholder="10.00" className="mt-1" />
                <p className="text-xs text-muted-foreground mt-1">e.g. {getCurrencySymbol(stampCurrency)}10.00 per 100 calls</p>
              </div>
            </div>
          )}

          {tierError && (
            <div className="px-3 py-2 rounded-md bg-destructive/10 border border-destructive/20">
              <p className="text-destructive text-sm">{tierError}</p>
            </div>
          )}

          <p className="text-xs text-muted-foreground">
            Re-using an existing rule key republishes its price, taking effect from each customer's
            next billing period.{hasClocks && (
              <span className="text-amber-700 dark:text-amber-500"> Test-clock customers get it in their current simulated period.</span>
            )}
          </p>

          <DialogFooter>
            <Button type="button" variant="outline" onClick={onClose}>Cancel</Button>
            <Button type="submit" disabled={saving}>
              {saving ? <><Loader2 size={14} className="animate-spin mr-2" /> Saving...</> : (prefill ? `Publish v${prefill.version + 1}` : 'Create Rule')}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

/* ─── Create Meter Dialog ─── */

function CreateMeterDialog({ onClose, onCreated, rules }: { onClose: () => void; onCreated: (meterId?: string) => void; rules: RatingRule[] }) {
  const [form, setForm] = useState({ key: '', name: '', unit: 'unit', aggregation: 'sum', rating_rule_version_id: '' })
  const [saving, setSaving] = useState(false)

  const aggregationDescriptions: Record<string, string> = {
    sum: 'Add up all values in the billing period',
    count: 'Count the number of events',
    max: 'Use the highest value seen',
    last: 'Use the most recent value',
  }

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    setSaving(true)
    try {
      const created = await api.createMeter(form)
      onCreated(created?.id)
    } catch (err) {
      showApiError(err, 'Failed to create meter')
    } finally {
      setSaving(false)
    }
  }

  return (
    <Dialog open onOpenChange={(open) => { if (!open) onClose() }}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Create Meter</DialogTitle>
          <DialogDescription>Define a new usage meter for tracking events</DialogDescription>
        </DialogHeader>
        <form onSubmit={submit} noValidate className="space-y-4">
          <div>
            <Label>Name</Label>
            <Input value={form.name} onChange={e => setForm(f => ({ ...f, name: e.target.value }))}
              placeholder="API Calls" maxLength={255} className="mt-1" />
          </div>
          <div>
            <Label>Key</Label>
            <Input value={form.key} onChange={e => setForm(f => ({ ...f, key: e.target.value }))}
              placeholder="api_calls" maxLength={100} className="mt-1 font-mono" />
            <p className="text-xs text-muted-foreground mt-1">
              Used to match incoming usage events (e.g. api_calls, messages_sent)
            </p>
          </div>
          <div className="grid grid-cols-2 gap-3">
            <div>
              <Label>Unit Label</Label>
              <Input value={form.unit} onChange={e => setForm(f => ({ ...f, unit: e.target.value }))}
                placeholder="unit" className="mt-1" />
              <p className="text-xs text-muted-foreground mt-1">e.g. call, request, GB</p>
            </div>
            <div>
              <Label>Aggregation</Label>
              <Select items={AGGREGATION_OPTIONS} value={form.aggregation} onValueChange={v => setForm(f => ({ ...f, aggregation: v ?? '' }))}>
                <SelectTrigger className="w-full mt-1">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {AGGREGATION_OPTIONS.map(o => (
                    <SelectItem key={o.value} value={o.value}>{o.label}</SelectItem>
                  ))}
                </SelectContent>
              </Select>
              <p className="text-xs text-muted-foreground mt-1">{aggregationDescriptions[form.aggregation]}</p>
            </div>
          </div>
          {rules.length === 0 ? (
            <div>
              <Label>Rating Rule</Label>
              <p className="text-xs text-muted-foreground mt-1">
                No prices exist yet — without one, usage on this meter won't be billed.
                Create one on the <Link to="/pricing?tab=rules" className="text-primary hover:underline">Prices tab</Link>, then link it here or on the meter's page.
              </p>
            </div>
          ) : (
            <div>
              <Label>Rating Rule</Label>
              {/* Searchable: the option list is TENANT DATA (every price the
                  tenant defines), so it grows unbounded — a plain Select becomes
                  an unscannable wall. Fixed enums stay plain Selects. */}
              <div className="mt-1">
                <Combobox
                  value={form.rating_rule_version_id}
                  onChange={v => setForm(f => ({ ...f, rating_rule_version_id: v ?? '' }))}
                  options={latestPerKey(rules).map(r => ({ value: r.id, label: `${r.name} — ${priceRate(r, form.unit)}` }))}
                  placeholder="None (assign later)"
                  emptyMessage="No price matches that search."
                  clearable
                  triggerClassName="w-full"
                />
              </div>
            </div>
          )}

          <DialogFooter>
            <Button type="button" variant="outline" onClick={onClose}>Cancel</Button>
            <Button type="submit" disabled={saving}>
              {saving ? <><Loader2 size={14} className="animate-spin mr-2" /> Saving...</> : 'Create Meter'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

/* ─── Create Plan Dialog ─── */

function CreatePlanDialog({ onClose, onCreated, meters, rules }: { onClose: () => void; onCreated: () => void; meters: Meter[]; rules: RatingRule[] }) {
  const [name, setName] = useState('')
  const [code, setCode] = useState('')
  const [interval, setInterval] = useState('monthly')
  const [basePrice, setBasePrice] = useState('')
  const [billTiming, setBillTiming] = useState<'in_arrears' | 'in_advance'>('in_arrears')
  const [meterIds, setMeterIds] = useState<string[]>([])
  const [saving, setSaving] = useState(false)

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    setSaving(true)
    try {
      // Inherit currency from the wired meters' prices when resolvable
      // (ADR-100: plan and its meters' prices must agree — the backend
      // refuses a mismatch, so don't stamp the tenant default blindly).
      const inherited = meterIds
        .map(id => meters.find(m => m.id === id)?.rating_rule_version_id)
        .map(rid => rules.find(r => r.id === rid)?.currency)
        .find(c => !!c)
      await api.createPlan({
        name,
        code,
        currency: inherited ?? getActiveCurrency(),
        billing_interval: interval,
        base_amount_cents: Math.round(parseFloat(basePrice || '0') * 100),
        base_bill_timing: billTiming,
        meter_ids: meterIds,
      })
      onCreated()
    } catch (err) {
      showApiError(err, 'Failed to create plan')
    } finally {
      setSaving(false)
    }
  }

  return (
    <Dialog open onOpenChange={(open) => { if (!open) onClose() }}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Create Plan</DialogTitle>
          <DialogDescription>Define a new billing plan with pricing and meters</DialogDescription>
        </DialogHeader>
        <form onSubmit={submit} noValidate className="space-y-4">
          <div>
            <Label>Plan Name</Label>
            <Input value={name} onChange={e => setName(e.target.value)}
              placeholder="Pro Plan" maxLength={255} className="mt-1" />
          </div>
          <div>
            <Label htmlFor="plan-code">Code</Label>
            <Input id="plan-code" value={code} onChange={e => setCode(e.target.value)}
              placeholder="pro" maxLength={100} className="mt-1 font-mono" />
            <p className="text-xs text-muted-foreground mt-1">Unique identifier used in API calls</p>
          </div>
          <div className="grid grid-cols-2 gap-3">
            <div>
              <Label>Billing Interval</Label>
              <Select items={INTERVAL_OPTIONS} value={interval} onValueChange={(v) => setInterval(v ?? '')}>
                <SelectTrigger className="w-full mt-1">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {INTERVAL_OPTIONS.map(o => (
                    <SelectItem key={o.value} value={o.value}>{o.label}</SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
          </div>
          <div>
            <Label>Base Price ({getCurrencySymbol()})</Label>
            <Input type="number" step="0.01" min={0} max={999999.99} value={basePrice}
              onChange={e => setBasePrice(e.target.value)} placeholder="49.00" className="mt-1" />
            <p className="text-xs text-muted-foreground mt-1">Fixed {interval} charge before usage fees</p>
          </div>
          <div>
            <Label>Base fee billed</Label>
            <Select items={BILL_TIMING_OPTIONS} value={billTiming} onValueChange={(v) => setBillTiming((v ?? 'in_arrears') as 'in_arrears' | 'in_advance')}>
              <SelectTrigger className="w-full mt-1">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {BILL_TIMING_OPTIONS.map(o => (
                  <SelectItem key={o.value} value={o.value}>{o.label}</SelectItem>
                ))}
              </SelectContent>
            </Select>
            <p className="text-xs text-muted-foreground mt-1">
              {billTiming === 'in_advance'
                ? 'Customer is charged the base fee on day 1 of each period; usage settles at period end.'
                : 'Customer is charged the base fee plus any usage at the end of each period.'}
            </p>
          </div>

          {meters.length > 0 && (
            <div className="border-t border-border pt-4">
              <p className="text-xs font-semibold text-muted-foreground uppercase tracking-wider mb-3">Usage Meters</p>
              <div className="space-y-0 rounded-lg border border-border divide-y divide-border overflow-hidden">
                {meters.map(m => (
                  <label key={m.id} className="flex items-center gap-3 px-3 py-2.5 text-sm cursor-pointer hover:bg-accent transition-colors">
                    <Checkbox
                      checked={meterIds.includes(m.id)}
                      onCheckedChange={(checked) =>
                        setMeterIds(checked ? [...meterIds, m.id] : meterIds.filter(id => id !== m.id))
                      }
                    />
                    <div className="flex-1 min-w-0">
                      <span className="text-foreground">{m.name}</span>
                      <span className="text-muted-foreground font-mono text-xs ml-2">{m.key}</span>
                    </div>
                    <span className="text-xs text-muted-foreground">{m.aggregation} &middot; {m.unit}</span>
                  </label>
                ))}
              </div>
            </div>
          )}

          <DialogFooter>
            <Button type="button" variant="outline" onClick={onClose}>Cancel</Button>
            <Button type="submit" disabled={saving}>
              {saving ? <><Loader2 size={14} className="animate-spin mr-2" /> Saving...</> : 'Create Plan'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
