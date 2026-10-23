import { Fragment, useState } from 'react'
import { usePageTitle } from '@/hooks/usePageTitle'
import { useTestClocks } from '@/hooks/useClockFrozenMap'
import { Link, useNavigate, useSearchParams } from 'react-router-dom'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'
import {
  api,
  formatCents,
  formatDate,
  getCurrencySymbol,
  getActiveCurrency,
  type Meter,
  type RatingRule,
} from '@/lib/api'
import { dollarsToRateCents } from '@/lib/priceInput'
import { Layout } from '@/components/Layout'
import { LoadError } from '@/components/LoadError'
import { priceRate, shiftDecimal } from '@/lib/priceDisplay'
import { latestPerKey } from '@/lib/rules'
import { METER_AGGREGATIONS, meterAggregationHelp, meterAggregationLabel } from '@/lib/meterLabels'
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
  const showCounts = !loading && !error

  const showAdd =
    (tab === 'plans' && plans.length > 0) ||
    (tab === 'meters' && meters.length > 0) ||
    (tab === 'rules' && rules.length > 0)

  return (
    <Layout>
      <div className="flex flex-col gap-4 sm:flex-row sm:items-start sm:justify-between">
        <div className="max-w-3xl">
          <h1 className="text-2xl font-semibold text-foreground">Pricing</h1>
          <p className="text-sm text-muted-foreground mt-1">
            Set what you charge. A meter counts something your customers use, like API calls or tokens.
            A price says what that usage costs. A plan puts a base fee and priced meters together.
            To bill a customer, you put them on a plan with a subscription.
          </p>
          <p className="text-sm text-muted-foreground mt-2">
            New here? Start with a plan, or install a{' '}
            <Link to="/recipes" className="text-primary hover:underline">ready-made recipe</Link>.
          </p>
        </div>
        {showAdd && (
          <Button size="sm" className="shrink-0 self-start" onClick={() => setCreateFor(tab)}>
            <Plus size={16} className="mr-2" />
            {tab === 'plans' ? 'Add Plan' : tab === 'meters' ? 'Add Meter' : 'Add Price'}
          </Button>
        )}
      </div>

      <Tabs value={tab} onValueChange={setTab} className="mt-6">
        <TabsList>
          {/* Counts only once the lists have loaded: "(0)" over a load
              error or a skeleton claims an empty list nobody has seen. */}
          <TabsTrigger value="plans">Plans{showCounts ? ` (${plans.length})` : ''}</TabsTrigger>
          <TabsTrigger value="meters">Meters{showCounts ? ` (${meters.length})` : ''}</TabsTrigger>
          {/* The value stays 'rules' so saved ?tab=rules links still open it. */}
          <TabsTrigger value="rules">Prices{showCounts ? ` (${currentRules.length})` : ''}</TabsTrigger>
        </TabsList>

        <Card className="mt-4">
          <CardContent className="p-0">
            {error ? (
              <LoadError
                what="pricing"
                error={error}
                onRetry={() => {
                  // A failure may have come from any of the tab queries —
                  // retrying only one leaves the button dead for the rest.
                  for (const key of ['plans', 'meters', 'rating-rules', 'meter-pricing-rules']) {
                    queryClient.invalidateQueries({ queryKey: [key] })
                  }
                }}
              />
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
                          <TableHead>Event name</TableHead>
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
                            <TableCell className="font-mono text-muted-foreground">{m.event_name}</TableCell>
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
                      title="No prices yet"
                      description="A price sets what each unit of usage costs: flat, tiered, or per package. Link it to a meter to bill usage."
                      action={{
                        label: 'Add Price',
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
                          <TableHead>Key</TableHead>
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
                                    // Not a problem: a price no meter uses yet.
                                    // Amber stays for "Not priced", which is one.
                                    : <span className="text-muted-foreground">Not linked</span>}
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
            toast.success(`Published version ${republishRule.version + 1} of ${republishRule.name}`)
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
            toast.success('Price created')
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
  // useTestClocks is mode-gated: in live mode it never fetches (the server
  // refuses the resource) and hasClocks stays false — correct, since the
  // caveat below can only ever apply to test-clock simulation.
  const { data: clocksData } = useTestClocks()
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
  // Publishing under an EXISTING key keeps that key's currency — this form
  // has no currency field, and stamping the tenant's current default would
  // turn a rate edit into a currency-changing republish (refused by the
  // ADR-100 guard) after a Settings currency switch. A new key takes the
  // workspace default. The labels and the submit read this one value.
  const stampCurrency = existingLatest?.currency ?? getActiveCurrency()
  const [formError, setFormError] = useState('')
  const [saving, setSaving] = useState(false)

  const modeDescriptions: Record<string, string> = {
    flat: 'Fixed price per unit',
    graduated: 'Price decreases as usage increases (tiered)',
    package: 'Charge per block of units',
  }

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    setSaving(true)
    setFormError('')
    try {
      const payload: Record<string, unknown> = { rule_key: ruleKey, name, mode, currency: stampCurrency }
      // A blank or unreadable price is refused here: the server accepts 0 as
      // a deliberate free rate, so it cannot catch a price left empty.
      if (mode === 'flat') {
        const cents = dollarsToRateCents(flatAmount)
        if (cents === null) { setFormError('Price per unit: enter a price as a plain number, for example 0.01. Use 0 for free.'); setSaving(false); return }
        payload.flat_amount_cents = cents
      }
      if (mode === 'graduated') {
        for (let i = 0; i < tiers.length - 1; i++) {
          const current = parseInt(tiers[i].upTo) || 0
          const next = tiers[i + 1].upTo === '' ? Infinity : (parseInt(tiers[i + 1].upTo) || 0)
          if (current <= 0) { setFormError(`Tier ${i + 1}: "Up to" must be greater than 0`); setSaving(false); return }
          if (next !== Infinity && next <= current) { setFormError(`Tier ${i + 2}: "Up to" must be greater than ${current}`); setSaving(false); return }
        }
        const tierCents: string[] = []
        for (let i = 0; i < tiers.length; i++) {
          const cents = dollarsToRateCents(tiers[i].price)
          if (cents === null) { setFormError(`Tier ${i + 1}: enter a price as a plain number, for example 0.01. Use 0 for free.`); setSaving(false); return }
          tierCents.push(cents)
        }
        payload.graduated_tiers = tiers.map((t, i) => ({
          up_to: t.upTo === '' ? 0 : parseInt(t.upTo) || 0,
          unit_amount_cents: tierCents[i],
        }))
      }
      if (mode === 'package') {
        payload.package_size = parseInt(packageSize) || 1
        if (dollarsToRateCents(packageAmount) === null) { setFormError('Price per package: enter a price as a plain number, for example 10.00. Use 0 for free.'); setSaving(false); return }
        payload.package_amount_cents = Math.round(parseFloat(packageAmount) * 100)
      }
      await api.createRatingRule(payload as Parameters<typeof api.createRatingRule>[0])
      onCreated()
    } catch (err) {
      showApiError(err, 'Failed to create price')
    } finally {
      setSaving(false)
    }
  }

  return (
    <Dialog open onOpenChange={(open) => { if (!open) onClose() }}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>{prefill ? `New version of ${prefill.name}` : 'Create price'}</DialogTitle>
          <DialogDescription>{prefill ? `The latest is version ${prefill.version}. The new version applies from each customer's next billing period.` : 'Set what usage costs. To bill usage, link the price to a meter.'}</DialogDescription>
        </DialogHeader>
        <form onSubmit={submit} noValidate className="space-y-4">
          <div>
            <Label htmlFor="rule-name">Name</Label>
            <Input id="rule-name" value={name} onChange={e => setName(e.target.value)} placeholder="API Call Pricing" maxLength={255} className="mt-1" />
          </div>
          <div>
            <Label htmlFor="rule-key">Key</Label>
            <Input id="rule-key" value={ruleKey} onChange={e => setRuleKey(e.target.value)} placeholder="api_calls" maxLength={100} className="mt-1 font-mono" disabled={!!prefill} />
            {/* Publishing a new version already says when it takes effect, in
                the dialog's description; this line is for a key typed in the
                create form that turns out to exist. */}
            {prefill ? null : existingLatest ? (
              <p className="text-xs text-amber-700 dark:text-amber-500 mt-1">
                This key already exists, so saving publishes v{existingLatest.version + 1} in {existingLatest.currency}, from each customer's next billing period.
              </p>
            ) : (
              <p className="text-xs text-muted-foreground mt-1">A short name for this price, for example api_calls. Saving a price with a key that already exists makes a new version of that price.</p>
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

          {formError && (
            <div className="px-3 py-2 rounded-md bg-destructive/10 border border-destructive/20">
              <p className="text-destructive text-sm">{formError}</p>
            </div>
          )}

          {hasClocks && (prefill || existingLatest) && (
            <p className="text-xs text-muted-foreground">
              Test-clock customers get the new version in their current simulated period.
            </p>
          )}

          <DialogFooter>
            <Button type="button" variant="outline" onClick={onClose}>Cancel</Button>
            <Button type="submit" disabled={saving}>
              {saving ? <><Loader2 size={14} className="animate-spin mr-2" /> Saving...</> : (prefill ? `Publish version ${prefill.version + 1}` : 'Create price')}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

/* ─── Create Meter Dialog ─── */

function CreateMeterDialog({ onClose, onCreated, rules }: { onClose: () => void; onCreated: (meterId?: string) => void; rules: RatingRule[] }) {
  const [form, setForm] = useState({ event_name: '', name: '', unit: 'unit', aggregation: 'sum', rating_rule_version_id: '' })
  const [saving, setSaving] = useState(false)

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
          <DialogTitle>Create meter</DialogTitle>
          <DialogDescription>A meter counts one thing your customers use, like API calls or tokens.</DialogDescription>
        </DialogHeader>
        <form onSubmit={submit} noValidate className="space-y-4">
          <div>
            <Label>Name</Label>
            <Input value={form.name} onChange={e => setForm(f => ({ ...f, name: e.target.value }))}
              placeholder="API Calls" maxLength={255} className="mt-1" />
          </div>
          <div>
            <Label>Event name</Label>
            <Input value={form.event_name} onChange={e => setForm(f => ({ ...f, event_name: e.target.value }))}
              placeholder="api_calls" maxLength={100} className="mt-1 font-mono" />
            <p className="text-xs text-muted-foreground mt-1">
              The name your app sends with each usage event, e.g. api_calls.
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
              <Select items={METER_AGGREGATIONS.map(a => ({ value: a.value, label: a.label }))} value={form.aggregation} onValueChange={v => setForm(f => ({ ...f, aggregation: v ?? '' }))}>
                <SelectTrigger className="w-full mt-1">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {METER_AGGREGATIONS.map(o => (
                    <SelectItem key={o.value} value={o.value}>{o.label}</SelectItem>
                  ))}
                </SelectContent>
              </Select>
              <p className="text-xs text-muted-foreground mt-1">{meterAggregationHelp(form.aggregation)} Fixed once the meter has a usage event.</p>
            </div>
          </div>
          {rules.length === 0 ? (
            <div>
              <Label>Price</Label>
              <p className="text-xs text-muted-foreground mt-1">
                No prices yet. Usage on this meter isn't billed until you link one.{' '}
                <Link to="/pricing?tab=rules" className="text-primary hover:underline">Create a price</Link>
              </p>
            </div>
          ) : (
            <div>
              <Label>Price</Label>
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
              {saving ? <><Loader2 size={14} className="animate-spin mr-2" /> Saving...</> : 'Create meter'}
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

  // Inherit currency from the wired meters' prices when resolvable
  // (ADR-100: plan and its meters' prices must agree — the backend refuses a
  // mismatch, so don't stamp the tenant default blindly). The Base Price
  // label reads the same value, so it names the currency the plan is
  // created in.
  const planCurrency = meterIds
    .map(id => meters.find(m => m.id === id)?.rating_rule_version_id)
    .map(rid => rules.find(r => r.id === rid)?.currency)
    .find(c => !!c) ?? getActiveCurrency()

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    setSaving(true)
    try {
      await api.createPlan({
        name,
        code,
        currency: planCurrency,
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
          <DialogTitle>Create plan</DialogTitle>
          <DialogDescription>A plan is what a customer subscribes to: a base fee, priced meters, or both.</DialogDescription>
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
            <Label>Base Price ({getCurrencySymbol(planCurrency)})</Label>
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
                      <span className="text-muted-foreground font-mono text-xs ml-2">{m.event_name}</span>
                    </div>
                    <span className="text-xs text-muted-foreground">{meterAggregationLabel(m.aggregation)} &middot; {m.unit}</span>
                  </label>
                ))}
              </div>
            </div>
          )}

          <DialogFooter>
            <Button type="button" variant="outline" onClick={onClose}>Cancel</Button>
            <Button type="submit" disabled={saving}>
              {saving ? <><Loader2 size={14} className="animate-spin mr-2" /> Saving...</> : 'Create plan'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
