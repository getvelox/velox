import { useMemo, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Loader2, Trash2 } from 'lucide-react'
import { api, formatRate, formatDate, dollarsToRateCents, type PriceOverride, type RatingRule } from '@/lib/api'
import { showApiError } from '@/lib/formErrors'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { Label } from '@/components/ui/label'
import { Input } from '@/components/ui/input'
import { Combobox } from '@/components/Combobox'
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from '@/components/ui/dialog'
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from '@/components/ui/select'

// PriceOverridesCard is the operator home for negotiated per-customer
// prices (ADR-070). Before this card the surface was API-only: a deal was
// invisible in the dashboard except as unexplained math on the usage card
// and the invoice. The card shows each deal NEXT TO the current list price
// (the contrast is the information), and its copy carries the two ADR-070
// facts operators must not be surprised by: deals follow the rule across
// republishes, and every rate change lands at the NEXT period open.

const MODE_OPTIONS = [
  { value: 'flat', label: 'Flat per-unit' },
  { value: 'graduated', label: 'Graduated tiers' },
  { value: 'package', label: 'Package' },
]

// priceSummary renders a rule's or override's pricing block in operator
// language, e.g. "$0.01/unit", "3 tiers", "$10.00 per 100 units".
function priceSummary(p: { mode: string; flat_amount_cents: string; graduated_tiers?: { up_to: number; unit_amount_cents: string }[]; package_size: number; package_amount_cents: number }, currency?: string): string {
  switch (p.mode) {
    case 'flat':
      return `${formatRate(p.flat_amount_cents, currency)}/unit`
    case 'graduated':
      return `${p.graduated_tiers?.length ?? 0} tiers`
    case 'package':
      return `${formatRate(String(p.package_amount_cents), currency)} per ${p.package_size} units`
    default:
      return p.mode
  }
}

// latestByKey collapses the version history to the CURRENT list price per
// rule_key — the thing a deal is compared against.
function latestByKey(rules: RatingRule[]): Map<string, RatingRule> {
  const out = new Map<string, RatingRule>()
  for (const r of rules) {
    const cur = out.get(r.rule_key)
    if (!cur || r.version > cur.version) out.set(r.rule_key, r)
  }
  return out
}

// readOnly mirrors the archived-customer gate the rest of the customer page
// applies via !isArchived. The card renders below a banner that says "All
// data is read-only", so leaving its Add/End controls live let an operator
// open a full "Save override" form — and end an existing deal — on a customer
// the page had just declared frozen.
export function PriceOverridesCard({ customerId, customerTestClockId, readOnly = false }: { customerId: string; customerTestClockId?: string; readOnly?: boolean }) {
  const queryClient = useQueryClient()
  const [creating, setCreating] = useState(false)
  const [confirmDelete, setConfirmDelete] = useState<PriceOverride | null>(null)

  const { data: overridesData, isLoading } = useQuery({
    queryKey: ['price-overrides', customerId],
    queryFn: () => api.listPriceOverrides(customerId),
  })
  const { data: rulesData } = useQuery({
    queryKey: ['rating-rules'],
    queryFn: () => api.listRatingRules(),
  })

  const overrides = (overridesData?.data ?? []).filter(o => o.active)
  const listPrices = useMemo(() => latestByKey(rulesData?.data ?? []), [rulesData])

  const refresh = () => {
    queryClient.invalidateQueries({ queryKey: ['price-overrides', customerId] })
    queryClient.invalidateQueries({ queryKey: ['customer-usage', customerId] })
  }

  return (
    <Card>
      <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-3">
        <CardTitle className="text-sm">Price overrides</CardTitle>
        {!readOnly && (
          <Button size="sm" variant="outline" onClick={() => setCreating(true)}>
            Add override
          </Button>
        )}
      </CardHeader>
      <CardContent>
        {isLoading ? (
          <div className="flex justify-center py-4"><Loader2 size={16} className="animate-spin text-muted-foreground" /></div>
        ) : overrides.length === 0 ? (
          <div className="text-center py-4">
            <p className="text-sm text-muted-foreground">No negotiated prices — this customer bills at list price.</p>
            <p className="text-xs text-muted-foreground mt-1">
              An override replaces one pricing rule's rate for this customer only, and follows the rule across republishes.
            </p>
          </div>
        ) : (
          <div className="space-y-3">
            {overrides.map(o => {
              const list = listPrices.get(o.rule_key)
              return (
                <div key={o.id} className="flex items-start justify-between gap-3 rounded-md border border-border px-3 py-2">
                  <div className="min-w-0">
                    <p className="text-sm font-medium truncate">
                      {list?.name || o.rule_key}
                      <span className="ml-2 font-mono text-xs text-muted-foreground">{o.rule_key}</span>
                    </p>
                    <p className="text-sm mt-0.5">
                      <span className="font-semibold">{priceSummary(o, list?.currency)}</span>
                      {list && (
                        <span className="text-muted-foreground"> · list price {priceSummary(list, list.currency)}</span>
                      )}
                    </p>
                    <p className="text-xs text-muted-foreground mt-0.5">
                      {o.reason ? `${o.reason} · ` : ''}since {formatDate(o.created_at)}
                    </p>
                  </div>
                  {!readOnly && (
                    <button
                      type="button"
                      onClick={() => setConfirmDelete(o)}
                      aria-label={`End override on ${list?.name || o.rule_key}`}
                      className="text-muted-foreground hover:text-destructive transition-colors shrink-0 mt-1"
                    >
                      <Trash2 size={15} />
                    </button>
                  )}
                </div>
              )
            })}
          </div>
        )}
      </CardContent>

      {creating && (
        <CreateOverrideDialog
          customerId={customerId}
          clockPinned={!!customerTestClockId}
          rules={rulesData?.data ?? []}
          existing={overrides}
          onClose={() => setCreating(false)}
          onCreated={() => { setCreating(false); refresh() }}
        />
      )}
      {confirmDelete && (
        <DeleteOverrideDialog
          override={confirmDelete}
          clockPinned={!!customerTestClockId}
          ruleName={listPrices.get(confirmDelete.rule_key)?.name || confirmDelete.rule_key}
          onClose={() => setConfirmDelete(null)}
          onDeleted={() => { setConfirmDelete(null); refresh() }}
        />
      )}
    </Card>
  )
}

function CreateOverrideDialog({ customerId, clockPinned, rules, existing, onClose, onCreated }: {
  customerId: string
  clockPinned: boolean
  rules: RatingRule[]
  existing: PriceOverride[]
  onClose: () => void
  onCreated: () => void
}) {
  const latest = useMemo(() => latestByKey(rules), [rules])
  const [versionId, setVersionId] = useState('')
  const [mode, setMode] = useState('flat')
  const [flatAmount, setFlatAmount] = useState('')
  const [tiers, setTiers] = useState<{ upTo: string; price: string }[]>([
    { upTo: '1000', price: '0.10' },
    { upTo: '', price: '0.05' },
  ])
  const [packageSize, setPackageSize] = useState('100')
  const [packageAmount, setPackageAmount] = useState('10.00')
  const [reason, setReason] = useState('')
  const [tierError, setTierError] = useState('')
  const [saving, setSaving] = useState(false)

  const selectedRule = rules.find(r => r.id === versionId)
  const replacing = selectedRule && existing.some(o => o.rule_key === selectedRule.rule_key)

  const ruleOptions = useMemo(() =>
    [...latest.values()]
      .sort((a, b) => a.name.localeCompare(b.name) || a.rule_key.localeCompare(b.rule_key))
      .map(r => ({
        value: r.id,
        label: `${r.name} (${r.rule_key}) — list ${priceSummary(r, r.currency)}`,
        keywords: [r.rule_key, r.name],
      })), [latest])

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    if (!versionId) return
    setSaving(true)
    setTierError('')
    try {
      const payload: Parameters<typeof api.createPriceOverride>[0] = {
        customer_id: customerId,
        rating_rule_version_id: versionId,
        mode,
        reason: reason.trim() || undefined,
      }
      if (mode === 'flat') payload.flat_amount_cents = dollarsToRateCents(flatAmount)
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
      await api.createPriceOverride(payload)
      onCreated()
    } catch (err) {
      showApiError(err, 'Failed to create price override')
    } finally {
      setSaving(false)
    }
  }

  return (
    <Dialog open onOpenChange={(open) => { if (!open) onClose() }}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>Add price override</DialogTitle>
          <DialogDescription>
            A negotiated price for this customer on one pricing rule. It follows the rule across
            future republishes until you end it.
          </DialogDescription>
        </DialogHeader>
        <form onSubmit={submit} noValidate className="space-y-4">
          <div className="space-y-2">
            <Label>Pricing rule</Label>
            <Combobox
              value={versionId}
              onChange={setVersionId}
              options={ruleOptions}
              placeholder="Select a pricing rule..."
              emptyMessage="No pricing rules yet — create one on the Pricing page first."
              triggerClassName="w-full"
            />
            {replacing && (
              <p className="text-xs text-amber-600 dark:text-amber-500">
                This customer already has an override on this rule — saving replaces it from the next billing period.
              </p>
            )}
          </div>

          <div>
            <Label>Pricing model</Label>
            <Select items={MODE_OPTIONS} value={mode} onValueChange={(v) => setMode(v ?? 'flat')}>
              <SelectTrigger className="w-full mt-1">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {MODE_OPTIONS.map(o => (
                  <SelectItem key={o.value} value={o.value}>{o.label}</SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>

          {mode === 'flat' && (
            <div>
              <Label>Negotiated price per unit ($)</Label>
              <Input type="number" step="any" min={0} value={flatAmount}
                onChange={e => setFlatAmount(e.target.value)} placeholder="0.01" className="mt-1" />
              {selectedRule && (
                <p className="text-xs text-muted-foreground mt-1">
                  List price: {priceSummary(selectedRule, selectedRule.currency)}. Sub-cent rates allowed.
                </p>
              )}
            </div>
          )}

          {mode === 'graduated' && (
            <div className="space-y-3">
              <Label>Negotiated tiers</Label>
              <div className="rounded-lg border border-border overflow-hidden">
                <div className="grid grid-cols-[1fr_1fr_36px] gap-0 px-3 py-2 border-b border-border bg-muted">
                  <span className="text-xs font-medium text-muted-foreground uppercase tracking-wider">Up to (units)</span>
                  <span className="text-xs font-medium text-muted-foreground uppercase tracking-wider">Price per unit ($)</span>
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
                        <button type="button" aria-label={`Remove tier ${idx + 1}`}
                          onClick={() => setTiers(t => t.filter((_, i) => i !== idx))}
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
                <Label>Units per package</Label>
                <Input type="number" min={1} value={packageSize} onChange={e => setPackageSize(e.target.value)}
                  placeholder="100" className="mt-1" />
              </div>
              <div>
                <Label>Price per package ($)</Label>
                <Input type="number" step="0.01" min={0} value={packageAmount}
                  onChange={e => setPackageAmount(e.target.value)} placeholder="10.00" className="mt-1" />
              </div>
            </div>
          )}

          <div>
            <Label>Reason (optional)</Label>
            <Input value={reason} onChange={e => setReason(e.target.value)} maxLength={255}
              placeholder="e.g. Annual contract — negotiated Jan 2027" className="mt-1" />
            <p className="text-xs text-muted-foreground mt-1">Shown on this card and in the audit trail.</p>
          </div>

          {tierError && (
            <div className="px-3 py-2 rounded-md bg-destructive/10 border border-destructive/20">
              <p className="text-destructive text-sm">{tierError}</p>
            </div>
          )}

          <p className="text-xs text-muted-foreground border-t border-border pt-3">
            <strong className="text-foreground">Takes effect from this customer's next billing period.</strong>{' '}
            The period already in progress keeps the price it started with.
            {clockPinned && (
              <span className="block mt-1 text-amber-700 dark:text-amber-500">
                Test-clock customer: in simulations, price changes apply to the current simulated
                period too — the next-period rule holds only on real time.
              </span>
            )}
          </p>

          <DialogFooter>
            <Button type="button" variant="outline" onClick={onClose}>Cancel</Button>
            <Button type="submit" disabled={saving || !versionId}>
              {saving ? <><Loader2 size={14} className="animate-spin mr-2" /> Saving...</> : 'Save override'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

function DeleteOverrideDialog({ override, clockPinned, ruleName, onClose, onDeleted }: {
  override: PriceOverride
  clockPinned: boolean
  ruleName: string
  onClose: () => void
  onDeleted: () => void
}) {
  const [deleting, setDeleting] = useState(false)

  const confirm = async () => {
    setDeleting(true)
    try {
      await api.deletePriceOverride(override.id)
      onDeleted()
    } catch (err) {
      showApiError(err, 'Failed to end price override')
      setDeleting(false)
    }
  }

  return (
    <Dialog open onOpenChange={(open) => { if (!open) onClose() }}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>End price override</DialogTitle>
          <DialogDescription>
            {ruleName} returns to list price for this customer.
          </DialogDescription>
        </DialogHeader>
        <p className="text-sm text-muted-foreground">
          <strong className="text-foreground">List price applies from the next billing period.</strong>{' '}
          The period already in progress still bills the negotiated price it started with.
          {clockPinned && (
            <span className="block mt-1 text-xs text-amber-700 dark:text-amber-500">
              Test-clock customer: in simulations, ending a deal affects the current simulated
              period too — the next-period rule holds only on real time.
            </span>
          )}
        </p>
        <DialogFooter>
          <Button type="button" variant="outline" onClick={onClose}>Cancel</Button>
          <Button type="button" variant="destructive" onClick={confirm} disabled={deleting}>
            {deleting ? <><Loader2 size={14} className="animate-spin mr-2" /> Ending...</> : 'End override'}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
