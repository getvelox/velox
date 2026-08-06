import { useState } from 'react'
import { usePageTitle } from '@/hooks/usePageTitle'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'
import { Pencil, Trash2, Plus, Star, AlertTriangle } from 'lucide-react'

import { api, type DunningPolicy, type DunningPolicyWithCount } from '@/lib/api'
import { Layout } from '@/components/Layout'
import { EmptyState } from '@/components/EmptyState'
import { FeedSkeleton } from '@/components/ui/TableSkeleton'
import { showApiError } from '@/lib/formErrors'

import { Button } from '@/components/ui/button'
import { AlertDialog, AlertDialogContent, AlertDialogHeader, AlertDialogTitle, AlertDialogDescription, AlertDialogFooter, AlertDialogCancel } from '@/components/ui/alert-dialog'
import { Badge } from '@/components/ui/badge'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Label } from '@/components/ui/label'
import { Input } from '@/components/ui/input'
import { Switch } from '@/components/ui/switch'
import { finalActionSummary } from '@/lib/dunningActions'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'

// DunningPoliciesPage — admin surface for the campaigns model
// (ADR-036). Operators create named policies ("Enterprise — Net 30",
// "SMB — fast-fail"), edit them, mark one as the tenant default,
// delete unused ones. Customer assignment happens on the customer
// detail page (one dropdown picks from this list).
//
// Industry shape verified against Stripe (account-level retry policy),
// Lago (campaigns + per-customer assignment), Orb (rules + schedules),
// Recurly (campaigns + per-account assignment). Velox follows the
// converging named-templates pattern.

// ADR-036 amendment — four Stripe-aligned terminal actions. Single
// source of truth for both the dropdown <SelectItem>s and the Base UI
// `items` prop that lets <SelectValue> render the selected label
// (Base UI shows the raw value otherwise). Labels reflect the actual
// semantics (pause = collection-only, not hard pause).
const SUBSCRIPTION_ACTION_OPTIONS = [
  { value: 'pause', label: 'Pause collection (keep drafting invoices)' },
  { value: 'cancel', label: 'Cancel the subscription' },
  { value: 'none', label: 'Leave it active' },
]

const INVOICE_ACTION_OPTIONS = [
  { value: 'none', label: 'Leave it open and due' },
  { value: 'mark_uncollectible', label: 'Write it off as bad debt' },
]

// Plain-language consequence for every combination. The old single-select
// left the debt's fate entirely unstated: an operator picking "Cancel
// subscription" had no way to learn the unpaid invoice then sits open with
// no closer anywhere in the system. Finance/ops read this, so it names
// money and next steps, not enum values.
function outcomeSentence(sub: string, inv: string): string {
  const subPart =
    sub === 'cancel' ? 'The subscription is canceled.'
    : sub === 'pause' ? 'Automatic charging stops; the cycle keeps drafting invoices until you resume.'
    : 'The subscription keeps billing as normal.'
  const invPart =
    inv === 'mark_uncollectible'
      ? 'The unpaid invoice is written off as bad debt — it leaves your receivables and stays on the books for audit. If the customer pays later, record the offline payment or issue a recovery invoice; nothing charges the written-off invoice itself.'
      : 'The unpaid invoice stays open and due. Nothing closes it automatically — you collect it or write it off yourself.'
  return `${subPart} ${invPart}`
}

export default function DunningPoliciesPage() {
  usePageTitle('Dunning policies')
  const queryClient = useQueryClient()
  const [editingPolicy, setEditingPolicy] = useState<DunningPolicyWithCount | null>(null)
  const [showCreate, setShowCreate] = useState(false)
  const [deleteTarget, setDeleteTarget] = useState<DunningPolicyWithCount | null>(null)

  const { data: policiesData, isLoading } = useQuery({
    queryKey: ['dunning-policies'],
    queryFn: () => api.listDunningPolicies(),
  })
  const policies = policiesData?.data ?? []

  const invalidate = () => queryClient.invalidateQueries({ queryKey: ['dunning-policies'] })

  async function handleSetDefault(id: string) {
    try {
      await api.setDefaultDunningPolicy(id)
      invalidate()
      toast.success('Default policy updated')
    } catch (err) {
      showApiError(err, 'Failed to set default')
    }
  }

  async function handleDelete(p: DunningPolicyWithCount) {
    if (p.is_default) {
      toast.error('Promote another policy to default first')
      return
    }
    if (p.assigned_customers > 0) {
      toast.error(`${p.assigned_customers} customer(s) assigned — reassign them first`)
      return
    }
    setDeleteTarget(p)
  }

  async function confirmDelete() {
    if (!deleteTarget) return
    try {
      await api.deleteDunningPolicy(deleteTarget.id)
      invalidate()
      toast.success('Policy deleted')
    } catch (err) {
      showApiError(err, 'Failed to delete')
    } finally {
      setDeleteTarget(null)
    }
  }

  return (
    <Layout>
      <div className="space-y-6">
        <div className="flex items-center justify-between">
          <div>
            <h1 className="text-2xl font-semibold text-foreground">Dunning policies</h1>
            <p className="text-sm text-muted-foreground mt-1">
              Named retry policies (campaigns). One is the tenant default; customers can be assigned to other policies on the customer detail page.
            </p>
          </div>
          <Button onClick={() => setShowCreate(true)}>
            <Plus size={16} className="mr-2" /> New policy
          </Button>
        </div>

        <Card>
          <CardHeader>
            <CardTitle className="text-sm">Policies ({policies.length})</CardTitle>
          </CardHeader>
          <CardContent className="p-0">
            {isLoading && <FeedSkeleton rows={3} />}
            {!isLoading && policies.length === 0 && (
              <EmptyState
                icon={AlertTriangle}
                title="No dunning policies yet"
                description="A dunning policy decides how failed payments are retried and when customers are emailed. Create one to start recovering failed payments automatically."
                action={{ label: 'New policy', onClick: () => setShowCreate(true), icon: Plus }}
              />
            )}
            <div className="divide-y divide-border">
              {policies.map(p => (
                <div key={p.id} className="px-6 py-4 flex items-start justify-between gap-3">
                  <div className="min-w-0 flex-1">
                    <div className="flex items-center gap-2 flex-wrap">
                      <p className="text-sm font-medium text-foreground">{p.name || '(unnamed policy)'}</p>
                      {p.is_default && <Badge variant="default"><Star size={10} className="mr-1" /> default</Badge>}
                      {!p.enabled && <Badge variant="outline">disabled</Badge>}
                      <Badge variant="outline">{p.assigned_customers} assigned</Badge>
                    </div>
                    <div className="mt-1 grid grid-cols-2 sm:grid-cols-4 gap-x-4 gap-y-1 text-xs text-muted-foreground">
                      <span>Max retries: <span className="text-foreground">{p.max_retry_attempts}</span></span>
                      <span>Grace: <span className="text-foreground">{p.grace_period_days}d</span></span>
                      <span>Schedule: <span className="text-foreground">{p.retry_schedule.join(' · ') || '—'}</span></span>
                      <span>On exhaustion: <span className="text-foreground">{finalActionSummary(p)}</span></span>
                    </div>
                  </div>
                  <div className="flex items-center gap-2 shrink-0">
                    {!p.is_default && (
                      <Button variant="ghost" size="sm" onClick={() => handleSetDefault(p.id)}>
                        <Star size={14} className="mr-1" /> Make default
                      </Button>
                    )}
                    {/* Icon-only actions carry the policy name in their
                        accessible name. Without it both read as bare
                        "button" — and on a list of N policies that gives a
                        screen-reader user no way to tell which row's
                        DESTRUCTIVE action they are on. */}
                    <Button variant="ghost" size="sm" aria-label={`Edit policy ${p.name}`} onClick={() => setEditingPolicy(p)}>
                      <Pencil size={14} />
                    </Button>
                    {!p.is_default && (
                      <Button variant="ghost" size="sm" aria-label={`Delete policy ${p.name}`} onClick={() => handleDelete(p)}>
                        <Trash2 size={14} className="text-destructive" />
                      </Button>
                    )}
                  </div>
                </div>
              ))}
            </div>
          </CardContent>
        </Card>
      </div>

      {showCreate && (
        <PolicyDialog
          mode="create"
          onClose={() => setShowCreate(false)}
          onSaved={() => { setShowCreate(false); invalidate(); toast.success('Policy created') }}
        />
      )}
      {editingPolicy && (
        <PolicyDialog
          mode="edit"
          initial={editingPolicy}
          onClose={() => setEditingPolicy(null)}
          onSaved={() => { setEditingPolicy(null); invalidate(); toast.success('Policy saved') }}
        />
      )}
      <AlertDialog open={!!deleteTarget} onOpenChange={(open) => { if (!open) setDeleteTarget(null) }}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Delete policy &ldquo;{deleteTarget?.name}&rdquo;?</AlertDialogTitle>
            <AlertDialogDescription>
              The policy is removed permanently. Customers fall back to the tenant default policy for future failed payments.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Keep policy</AlertDialogCancel>
            <Button variant="destructive" onClick={confirmDelete}>Delete policy</Button>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </Layout>
  )
}

function PolicyDialog({ mode, initial, onClose, onSaved }: {
  mode: 'create' | 'edit'
  initial?: DunningPolicy
  onClose: () => void
  onSaved: () => void
}) {
  const [form, setForm] = useState({
    name: initial?.name ?? '',
    enabled: initial?.enabled ?? true,
    max_retry_attempts: String(initial?.max_retry_attempts ?? 3),
    grace_period_days: String(initial?.grace_period_days ?? 3),
    retry_schedule: (initial?.retry_schedule ?? ['72h', '120h']).join(', '),
    final_subscription_action: initial?.final_subscription_action || 'pause',
    final_invoice_action: initial?.final_invoice_action || 'none',
  })
  const [saving, setSaving] = useState(false)

  // Parse "72h, 120h" → ["72h", "120h"]. Empty entries dropped so
  // trailing commas don't confuse the backend validator.
  const parseSchedule = (raw: string): string[] =>
    raw.split(',').map(s => s.trim()).filter(Boolean)

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    setSaving(true)
    try {
      const payload: Partial<DunningPolicy> = {
        name: form.name.trim(),
        enabled: form.enabled,
        max_retry_attempts: parseInt(form.max_retry_attempts, 10) || 0,
        grace_period_days: parseInt(form.grace_period_days, 10) || 0,
        retry_schedule: parseSchedule(form.retry_schedule),
        final_subscription_action: form.final_subscription_action,
        final_invoice_action: form.final_invoice_action,
      }
      if (mode === 'create') {
        await api.createDunningPolicy(payload)
      } else if (initial) {
        await api.updateDunningPolicy(initial.id, payload)
      }
      onSaved()
    } catch (err) {
      showApiError(err, mode === 'create' ? 'Failed to create' : 'Failed to save')
    } finally {
      setSaving(false)
    }
  }

  const scheduleEntries = parseSchedule(form.retry_schedule)
  const max = parseInt(form.max_retry_attempts, 10) || 0
  const scheduleShortage = max - 1 - scheduleEntries.length

  return (
    <Dialog open onOpenChange={(open) => { if (!open) onClose() }}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>{mode === 'create' ? 'New dunning policy' : 'Edit policy'}</DialogTitle>
        </DialogHeader>
        <form onSubmit={handleSubmit} noValidate className="space-y-4">
          <div className="space-y-2">
            <Label>Name</Label>
            <Input value={form.name} placeholder="Enterprise — Net 30 grace"
              onChange={e => setForm(f => ({ ...f, name: e.target.value }))} />
          </div>
          <div className="flex items-center justify-between">
            <Label>Enabled</Label>
            <Switch checked={form.enabled} onCheckedChange={(val) => setForm(f => ({ ...f, enabled: val }))} />
          </div>
          <div className="grid grid-cols-2 gap-3">
            <div className="space-y-2">
              <Label>Max retry attempts</Label>
              <Input type="number" min={1} max={15} value={form.max_retry_attempts}
                onChange={e => setForm(f => ({ ...f, max_retry_attempts: e.target.value }))} />
            </div>
            <div className="space-y-2">
              <Label>Grace (days)</Label>
              <Input type="number" min={1} max={30} value={form.grace_period_days}
                onChange={e => setForm(f => ({ ...f, grace_period_days: e.target.value }))} />
            </div>
          </div>
          <div className="space-y-2">
            <Label>Retry schedule</Label>
            <Input value={form.retry_schedule} placeholder="72h, 120h, 168h"
              onChange={e => setForm(f => ({ ...f, retry_schedule: e.target.value }))} />
            <p className="text-xs text-muted-foreground">
              Comma-separated Go durations (h / m / s). Need {Math.max(0, max - 1)} entries for {max} retries.
              {scheduleShortage > 0 && (
                <span className="text-destructive"> Missing {scheduleShortage} entries.</span>
              )}
            </p>
          </div>
          <fieldset className="space-y-3 rounded-md border p-3">
            <legend className="px-1 text-sm font-medium">When all retries fail</legend>
            <div className="space-y-2">
              <Label>Do this to the subscription</Label>
              <Select items={SUBSCRIPTION_ACTION_OPTIONS} value={form.final_subscription_action} onValueChange={(val) => setForm(f => ({ ...f, final_subscription_action: val ?? 'pause' }))}>
                <SelectTrigger className="w-full">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {SUBSCRIPTION_ACTION_OPTIONS.map(o => (
                    <SelectItem key={o.value} value={o.value}>{o.label}</SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <div className="space-y-2">
              <Label>Do this to the unpaid invoice</Label>
              <Select items={INVOICE_ACTION_OPTIONS} value={form.final_invoice_action} onValueChange={(val) => setForm(f => ({ ...f, final_invoice_action: val ?? 'none' }))}>
                <SelectTrigger className="w-full">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {INVOICE_ACTION_OPTIONS.map(o => (
                    <SelectItem key={o.value} value={o.value}>{o.label}</SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <p className="text-xs text-muted-foreground">
              {outcomeSentence(form.final_subscription_action, form.final_invoice_action)}
            </p>
          </fieldset>
          <DialogFooter>
            <Button type="button" variant="outline" onClick={onClose}>Cancel</Button>
            <Button type="submit" disabled={saving}>
              {saving ? 'Saving…' : 'Save'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
