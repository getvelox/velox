import { useState } from 'react'
import { usePageTitle } from '@/hooks/usePageTitle'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'
import { useNavigate } from 'react-router-dom'
import { api, formatDateTime } from '@/lib/api'
import type { RecipeListItem } from '@/lib/gen/schemas/recipeListItem'
import type { RecipeDetail } from '@/lib/gen/schemas/recipeDetail'
import type { RecipeOverride } from '@/lib/gen/schemas/recipeOverride'
import type { RecipePreviewResult } from '@/lib/gen/schemas/recipePreviewResult'
import { money, priceRate } from '@/lib/priceDisplay'
import { showApiError } from '@/lib/formErrors'
import { Layout } from '@/components/Layout'

import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import { Card, CardContent } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { EmptyState } from '@/components/EmptyState'
import { TableSkeleton } from '@/components/ui/TableSkeleton'

import { Box, Loader2, Eye, Sparkles, AlertTriangle, CheckCircle2 } from 'lucide-react'
import { ScrollPane } from '@/components/ui/scroll-pane'

export default function RecipesPage() {
  usePageTitle('Recipes')
  const [selected, setSelected] = useState<RecipeListItem | null>(null)

  const { data, isLoading, error, refetch } = useQuery({
    queryKey: ['recipes'],
    queryFn: async () => {
      try {
        return await api.listRecipes()
      } catch {
        return { data: [] }
      }
    },
  })

  const recipes = data?.data ?? []

  return (
    <Layout>
      {/* Header */}
      <div className="flex items-start justify-between">
        <div>
          <h1 className="text-2xl font-semibold text-foreground">Pricing recipes</h1>
          <p className="text-sm text-muted-foreground mt-1 max-w-2xl">
            Set up billing for a common AI usage model in one click. Each recipe creates the usage meter, prices, and a plan you need — pre-filled with a provider's public rates so you go from zero to a working setup in seconds, then tune the numbers to your own pricing.
          </p>
        </div>
      </div>

      {error ? (
        <Card className="mt-6">
          <CardContent className="p-8 text-center">
            <p className="text-sm text-destructive mb-3">{(error as Error).message}</p>
            <Button variant="outline" size="sm" onClick={() => refetch()}>Retry</Button>
          </CardContent>
        </Card>
      ) : isLoading ? (
        <Card className="mt-6">
          <CardContent className="p-0">
            <TableSkeleton columns={3} />
          </CardContent>
        </Card>
      ) : recipes.length === 0 ? (
        <Card className="mt-6">
          <CardContent className="p-0">
            <EmptyState
              icon={Box}
              title="No recipes available yet"
              description="Recipes are ready-made billing setups for common AI usage models — per-token pricing by model, GPU-time, API calls. None are available on this server yet."
            />
          </CardContent>
        </Card>
      ) : (
        <div className="grid grid-cols-1 md:grid-cols-2 gap-4 mt-6">
          {recipes.map(r => (
            <RecipeCard key={r.key} recipe={r} onConfigure={() => setSelected(r)} />
          ))}
        </div>
      )}

      {selected && (
        <RecipeDialog
          recipe={selected}
          onClose={() => setSelected(null)}
        />
      )}
    </Layout>
  )
}

function RecipeCard({ recipe, onConfigure }: { recipe: RecipeListItem; onConfigure: () => void }) {
  const c = recipe.creates
  const installed = !!recipe.instantiated

  return (
    <Card className={installed ? 'border-primary/40' : ''}>
      <CardContent className="p-5">
        <div className="flex items-start justify-between gap-3">
          <div>
            <div className="flex items-center gap-2">
              <span className="font-mono text-sm text-foreground">{recipe.key}</span>
              <span className="text-xs text-muted-foreground">v{recipe.version}</span>
            </div>
            <h3 className="text-base font-semibold text-foreground mt-1">{recipe.name}</h3>
          </div>
          {installed && (
            <Badge variant="success" className="shrink-0">
              <CheckCircle2 size={12} className="mr-1" />
              Installed
            </Badge>
          )}
        </div>

        <p className="text-sm text-muted-foreground mt-3 leading-relaxed">{recipe.summary}</p>

        <div className="flex flex-wrap gap-1.5 mt-4">
          {c.meters > 0 && <Chip n={c.meters} label="usage meter" />}
          {c.rating_rules > 0 && <Chip n={c.rating_rules} label="price" />}
          {c.plans > 0 && <Chip n={c.plans} label="plan" />}
          {c.dunning_policies > 0 && <Chip n={c.dunning_policies} label="retry policy" />}
          {c.webhook_endpoints > 0 && <Chip n={c.webhook_endpoints} label="notification endpoint" />}
        </div>

        {recipe.instantiated && (
          <p className="text-xs text-muted-foreground mt-3">
            Installed {formatDateTime(recipe.instantiated.created_at)} · <span className="font-mono">{recipe.instantiated.id}</span>
          </p>
        )}

        <div className="mt-5 flex items-center justify-between">
          <span className="text-xs text-muted-foreground">
            {recipe.overridable.length} setting{recipe.overridable.length !== 1 ? 's' : ''} you can adjust
          </span>
          <Button size="sm" variant={installed ? 'outline' : 'default'} onClick={onConfigure}>
            {installed ? 'View' : 'Configure'}
          </Button>
        </div>
      </CardContent>
    </Card>
  )
}

function Chip({ n, label }: { n: number; label: string }) {
  return (
    <span className="inline-flex items-center text-xs font-medium bg-muted text-foreground px-2 py-0.5 rounded">
      {n} {label}{n !== 1 ? 's' : ''}
    </span>
  )
}

function RecipeDialog({ recipe, onClose }: { recipe: RecipeListItem; onClose: () => void }) {
  const queryClient = useQueryClient()
  const navigate = useNavigate()
  const installed = !!recipe.instantiated
  // Installed recipes get a READ-ONLY dialog seeded with the settings the
  // install actually used (recorded on the badge): re-apply is a no-op that
  // never reads overrides, so live inputs here would accept edits that can
  // do nothing. Preview stays enabled — with the recorded values it shows
  // what this install rendered.
  const [overrides, setOverrides] = useState<Record<string, string | number>>(() => {
    if (!installed) return {}
    const recorded = recipe.instantiated?.overrides ?? {}
    const out: Record<string, string | number> = {}
    for (const schema of recipe.overridable) {
      const v = recorded[schema.key]
      if (typeof v === 'string' || typeof v === 'number') out[schema.key] = v
    }
    return out
  })
  const [preview, setPreview] = useState<RecipePreviewResult | null>(null)

  const detailQuery = useQuery({
    queryKey: ['recipe', recipe.key],
    queryFn: async () => {
      try {
        return await api.getRecipe(recipe.key)
      } catch {
        return null as RecipeDetail | null
      }
    },
  })
  const detail = detailQuery.data

  const previewMutation = useMutation({
    mutationFn: () => api.previewRecipe(recipe.key, overrides),
    onSuccess: (res) => setPreview(res),
    onError: (err) => showApiError(err, 'Preview failed'),
  })

  const installMutation = useMutation({
    mutationFn: () =>
      api.instantiateRecipe({
        key: recipe.key,
        overrides,
      }),
    onSuccess: (res) => {
      toast.success(`Installed ${recipe.name}`)
      queryClient.invalidateQueries({ queryKey: ['recipes'] })
      onClose()
      // Send the operator straight to the catalog — the new plan is the
      // most likely next thing they want to look at. plan_ids is omitted
      // when empty (server-side omitempty), so guard the read.
      const planId = res.created_objects.plan_ids?.[0]
      if (planId) {
        navigate(`/plans/${planId}`)
      } else {
        navigate('/pricing')
      }
    },
    onError: (err) => showApiError(err, 'Install failed'),
  })

  const setOverride = (k: string, v: string | number) => {
    setOverrides(prev => ({ ...prev, [k]: v }))
  }

  return (
    <Dialog open onOpenChange={(open) => { if (!open) onClose() }}>
      {/* eslint-disable-next-line no-restricted-syntax -- DialogContent scrolls as ONE box including its own close button, so a content mask would fade that button. Giving dialogs a scrolling BODY inside a fixed shell is a redesign of every dialog; tracked, not bundled here. */}
      <DialogContent className="sm:max-w-2xl max-h-[85vh] overflow-y-auto">
        <DialogHeader>
          <DialogTitle className="flex items-center gap-2">
            <Sparkles size={16} className="text-primary" />
            {recipe.name}
          </DialogTitle>
          <DialogDescription>{recipe.summary}</DialogDescription>
        </DialogHeader>

        {detail?.description && (
          <div className="text-sm text-muted-foreground leading-relaxed whitespace-pre-line border-l-2 border-muted pl-3">
            {detail.description}
          </div>
        )}

        {/* Overrides */}
        <div>
          <Label className="text-xs uppercase tracking-wide text-muted-foreground">
            {installed ? 'Installed with these settings' : 'Customize before installing'}
          </Label>
          {recipe.overridable.length > 0 ? (
            <div className="space-y-3 mt-2">
              {recipe.overridable.map(schema => (
                <OverrideField
                  key={schema.key}
                  schema={schema}
                  value={overrides[schema.key] ?? String(schema.default ?? '')}
                  onChange={(v) => setOverride(schema.key, v)}
                  disabled={installed}
                />
              ))}
            </div>
          ) : (
            <p className="text-sm text-muted-foreground mt-2">Nothing to customize — this recipe installs as-is.</p>
          )}
          {installed && (
            <p className="text-xs text-muted-foreground mt-2">
              These were set when the recipe was installed and can't be changed here — edit the plan's name on its page and prices on the Pricing page. Re-running a recipe never changes what it already created.
            </p>
          )}
        </div>

        {/* Preview */}
        {preview && (
          <div className="rounded-lg border border-input p-4 space-y-3">
            <div className="flex items-center justify-between">
              <p className="text-sm font-medium text-foreground">Preview</p>
              <span className="text-xs text-muted-foreground">v{preview.version}</span>
            </div>

            {preview.warnings.length > 0 && (
              <div className="rounded bg-amber-50 dark:bg-amber-950/30 border border-amber-200 dark:border-amber-800 p-3">
                <div className="flex items-center gap-2 text-amber-700 dark:text-amber-300 text-xs font-medium mb-1.5">
                  <AlertTriangle size={12} />
                  {preview.warnings.length} warning{preview.warnings.length !== 1 ? 's' : ''}
                </div>
                <ul className="text-xs text-amber-700 dark:text-amber-300 space-y-1">
                  {preview.warnings.map((w, i) => <li key={i}>· {w}</li>)}
                </ul>
              </div>
            )}

            <ObjectsList preview={preview} />
          </div>
        )}

        <DialogFooter className="gap-2 sm:justify-between">
          <span />
          <div className="flex items-center gap-2">
            <Button variant="outline" onClick={onClose}>Cancel</Button>
            <Button
              variant="outline"
              onClick={() => previewMutation.mutate()}
              disabled={previewMutation.isPending}
            >
              {previewMutation.isPending ? <Loader2 size={14} className="mr-1.5 animate-spin" /> : <Eye size={14} className="mr-1.5" />}
              Preview
            </Button>
            <Button
              onClick={() => installMutation.mutate()}
              disabled={installMutation.isPending || !!recipe.instantiated}
            >
              {installMutation.isPending ? <Loader2 size={14} className="mr-1.5 animate-spin" /> : <Sparkles size={14} className="mr-1.5" />}
              {recipe.instantiated ? 'Already installed' : 'Install recipe'}
            </Button>
          </div>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

// Friendly labels + hints for the known recipe settings (stable across all
// recipes). Falls back to a humanized key for anything unmapped.
const OVERRIDE_LABELS: Record<string, { label: string; hint?: string }> = {
  currency: { label: 'Currency' },
  plan_name: { label: 'Plan name', hint: 'Shown to your customers' },
  plan_code: { label: 'Plan ID', hint: 'Used in links & the API — lowercase letters, numbers, underscores' },
}

function overrideLabel(key: string): string {
  return OVERRIDE_LABELS[key]?.label ?? (key.charAt(0).toUpperCase() + key.slice(1).replace(/_/g, ' '))
}

function OverrideField({
  schema,
  value,
  onChange,
  disabled,
}: {
  schema: RecipeOverride
  value: string | number
  onChange: (v: string | number) => void
  disabled?: boolean
}) {
  const id = `override-${schema.key}`
  const label = overrideLabel(schema.key)
  const hint = OVERRIDE_LABELS[schema.key]?.hint
  return (
    <div>
      <div className="flex items-center justify-between gap-3">
        <Label htmlFor={id} className="text-sm">{label}</Label>
        {hint && <span className="text-xs text-muted-foreground text-right">{hint}</span>}
      </div>
      <Input
        id={id}
        type={schema.type === 'int' ? 'number' : 'text'}
        value={String(value)}
        onChange={(e) => {
          const raw = e.target.value
          onChange(schema.type === 'int' ? Number(raw) : raw)
        }}
        className="mt-1.5"
        maxLength={schema.max_length}
        pattern={schema.pattern}
        disabled={disabled}
      />
    </div>
  )
}

function ObjectsList({ preview }: { preview: RecipePreviewResult }) {
  // Rules don't carry their meter; the bindings do. rule key → meter unit so
  // each price renders in that meter's units (tokens vs seconds vs calls).
  const unitByMeterKey = new Map((preview.objects.meters ?? []).map(m => [m.key, m.unit]))
  const unitByRuleKey = new Map(
    (preview.objects.pricing_rules ?? []).map(pr => [pr.rating_rule_key, unitByMeterKey.get(pr.meter_key) ?? '']),
  )

  const sections: { label: string; items: string[] }[] = []
  if (preview.objects.meters?.length) sections.push({ label: 'What gets counted', items: preview.objects.meters.map(m => `${m.name} (${m.aggregation} of ${m.unit})`) })
  if (preview.objects.rating_rules?.length) sections.push({
    label: 'Prices',
    items: preview.objects.rating_rules.map(r => `${r.name || r.key} — ${priceRate(r, unitByRuleKey.get(r.key) ?? '')}`),
  })
  if (preview.objects.plans?.length) sections.push({
    label: 'Plan',
    items: preview.objects.plans.map(p =>
      `${p.name} — ${p.billing_interval}, ${p.currency}` + (p.base_amount_cents > 0 ? `, ${money(String(p.base_amount_cents), 0, p.currency)} base` : ''),
    ),
  })
  if (preview.objects.dunning_policies?.length) sections.push({ label: 'Payment retries', items: preview.objects.dunning_policies.map(d => `${d.name} — up to ${d.max_retries} retries`) })
  if (preview.objects.webhook_endpoints?.length) sections.push({ label: 'Notifications', items: preview.objects.webhook_endpoints.map(w => `${w.url_placeholder} — set your real URL after install`) })

  if (sections.length === 0) {
    return <p className="text-xs text-muted-foreground">No objects to preview.</p>
  }

  // Every item renders — review is this panel's whole job, so nothing is
  // truncated; long price lists scroll inside their own section instead.
  return (
    <div className="space-y-2.5">
      {sections.map(s => (
        <div key={s.label}>
          <p className="text-xs font-medium text-muted-foreground uppercase tracking-wide mb-1">
            {s.label}
            {s.items.length > 8 && <span className="ml-1.5 normal-case tracking-normal">({s.items.length})</span>}
          </p>
          <ScrollPane as="ul" fadePx={12} className="text-xs text-foreground space-y-0.5 font-mono max-h-48 pr-1">
            {s.items.map((it, i) => <li key={i}>· {it}</li>)}
          </ScrollPane>
        </div>
      ))}
    </div>
  )
}
