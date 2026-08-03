import { useState } from 'react'
import { usePageTitle } from '@/hooks/usePageTitle'
import { CopyButton } from '@/components/CopyButton'
import { useSearchParams } from 'react-router-dom'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useForm } from 'react-hook-form'
import { zodResolver } from '@hookform/resolvers/zod'
import { z } from 'zod'
import { toast } from 'sonner'
import { api, formatDate, formatDateTime, type WebhookEndpoint } from '@/lib/api'
import { untilMs, wallClockNow } from '@/lib/effectiveNow'
import { applyApiError, showApiError } from '@/lib/formErrors'
import { Layout } from '@/components/Layout'

import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Badge } from '@/components/ui/badge'
import { Card, CardContent } from '@/components/ui/card'
import { Checkbox } from '@/components/ui/checkbox'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { TypedConfirmDialog } from '@/components/TypedConfirmDialog'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import {
  Form,
  FormControl,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
} from '@/components/ui/form'
import { TableSkeleton } from '@/components/ui/TableSkeleton'

import { Loader2, Plus, Webhook, Activity } from 'lucide-react'
import { EmptyState } from '@/components/EmptyState'
import { ScrollPane } from '@/components/ui/scroll-pane'

const createEndpointSchema = z.object({
  url: z.string().min(1, 'URL is required').refine(v => {
    try { const u = new URL(v); return u.protocol === 'https:' || u.protocol === 'http:' } catch { return false }
  }, 'Must be a valid URL'),
  description: z.string(),
})

type CreateEndpointData = z.infer<typeof createEndpointSchema>

export default function WebhooksPage() {
  usePageTitle('Webhooks')
  const [searchParams, setSearchParams] = useSearchParams()
  const tab = (searchParams.get('tab') === 'events' ? 'events' : 'endpoints') as 'endpoints' | 'events'
  const setTab = (t: string) => setSearchParams(t === 'endpoints' ? {} : { tab: t })

  return (
    <Layout>
      <div>
        <h1 className="text-2xl font-semibold text-foreground">Webhooks</h1>
        <p className="text-sm text-muted-foreground mt-1">Manage outbound webhook endpoints and events</p>
      </div>

      <Tabs value={tab} onValueChange={setTab} className="mt-6">
        <TabsList>
          <TabsTrigger value="endpoints">Endpoints</TabsTrigger>
          <TabsTrigger value="events">Events</TabsTrigger>
        </TabsList>

        <TabsContent value="endpoints">
          <EndpointsTab />
        </TabsContent>
        <TabsContent value="events">
          <EventsTab />
        </TabsContent>
      </Tabs>
    </Layout>
  )
}

/* ─── Endpoints Tab ─── */

function EndpointsTab() {
  const [showCreate, setShowCreate] = useState(false)
  const [createdSecret, setCreatedSecret] = useState<{ secret: string; secondary_valid_until?: string } | null>(null)
  const [deleteTarget, setDeleteTarget] = useState<WebhookEndpoint | null>(null)
  const [editTarget, setEditTarget] = useState<WebhookEndpoint | null>(null)
  const [rotatingId, setRotatingId] = useState<string | null>(null)
  const queryClient = useQueryClient()

  const { data, isLoading: loading, error: loadError, refetch } = useQuery({
    queryKey: ['webhook-endpoints'],
    queryFn: () => Promise.all([
      api.listWebhookEndpoints(),
      api.getWebhookEndpointStats(),
    ]).then(([epRes, statsRes]) => {
      const map: Record<string, { total_deliveries: number; succeeded: number; failed: number; success_rate: number }> = {}
      for (const s of (statsRes.data || [])) {
        map[s.endpoint_id] = s
      }
      return { endpoints: epRes.data || [], stats: map }
    }),
  })

  const endpoints = data?.endpoints ?? []
  const stats = data?.stats ?? {}
  const errorMsg = loadError instanceof Error ? loadError.message : loadError ? String(loadError) : null

  const handleDelete = async () => {
    if (!deleteTarget) return
    try {
      await api.deleteWebhookEndpoint(deleteTarget.id)
      toast.success('Endpoint deleted')
      setDeleteTarget(null)
      queryClient.invalidateQueries({ queryKey: ['webhook-endpoints'] })
    } catch (err) {
      showApiError(err, 'Failed to delete endpoint')
    }
  }

  return (
    <>
      <div className="flex justify-end mt-4">
        <Button size="sm" onClick={() => setShowCreate(true)}>
          <Plus size={16} className="mr-2" />
          Add Endpoint
        </Button>
      </div>

      <Card className="mt-4">
        <CardContent className="p-0">
          {errorMsg ? (
            <div className="p-8 text-center">
              <p className="text-sm text-destructive mb-3">{errorMsg}</p>
              <Button variant="outline" size="sm" onClick={() => refetch()}>Retry</Button>
            </div>
          ) : loading ? (
            <TableSkeleton columns={7} />
          ) : endpoints.length === 0 ? (
            <EmptyState
              icon={Webhook}
              title="No webhook endpoints"
              description="Add an endpoint to receive real-time event notifications for your account."
              action={{
                label: 'Add Endpoint',
                icon: Plus,
                onClick: () => setShowCreate(true),
              }}
            />
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>URL</TableHead>
                  <TableHead>Description</TableHead>
                  <TableHead>Events</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead>Success Rate</TableHead>
                  <TableHead>Created</TableHead>
                  <TableHead className="text-right"></TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {endpoints.map(ep => {
                  // Grace period is active when secondary_secret_expires_at
                  // is populated AND still in the future. After it passes
                  // the dispatcher drops back to single-secret signing
                  // server-side \u2014 we just stop showing the banner.
                  const rotationActive =
                    !!ep.secondary_secret_expires_at &&
                    untilMs(ep.secondary_secret_expires_at, wallClockNow()) > 0
                  return (
                  <TableRow key={ep.id}>
                    <TableCell className="font-mono text-sm max-w-xs" title={ep.url}>
                      <div className="truncate">{ep.url}</div>
                      {rotationActive && ep.secondary_secret_expires_at && (
                        <div className="text-xs text-emerald-700 dark:text-emerald-400 font-normal mt-0.5">
                          Dual-signing until {formatDateTime(ep.secondary_secret_expires_at)}
                        </div>
                      )}
                    </TableCell>
                    <TableCell className="text-muted-foreground">{ep.description || '\u2014'}</TableCell>
                    <TableCell>
                      <div className="flex flex-wrap gap-1">
                        {(ep.events || []).map(ev => (
                          <Badge key={ev} variant="outline">{ev}</Badge>
                        ))}
                        {(!ep.events || ep.events.length === 0) && <span className="text-xs text-muted-foreground">all</span>}
                      </div>
                    </TableCell>
                    <TableCell>
                      <Badge variant={ep.active ? 'success' : 'secondary'}>{ep.active ? 'active' : 'paused'}</Badge>
                    </TableCell>
                    <TableCell className="font-medium">
                      {(() => {
                        const s = stats[ep.id]
                        if (!s || s.total_deliveries === 0) return <span className="text-muted-foreground">{'\u2014'}</span>
                        const rate = s.success_rate
                        const color = rate >= 95 ? 'text-green-600' : rate >= 70 ? 'text-amber-600' : 'text-red-600'
                        return <span className={color}>{rate.toFixed(1)}%</span>
                      })()}
                    </TableCell>
                    <TableCell className="text-muted-foreground">{formatDate(ep.created_at)}</TableCell>
                    <TableCell className="text-right">
                      <div className="flex items-center justify-end gap-1">
                        <Button variant="outline" size="sm" className="h-7 text-xs"
                          disabled={!!rotatingId}
                          onClick={async () => {
                            if (rotatingId) return
                            setRotatingId(ep.id)
                            try {
                              const res = await api.rotateWebhookSecret(ep.id)
                              setCreatedSecret({ secret: res.secret, secondary_valid_until: res.secondary_valid_until })
                              queryClient.invalidateQueries({ queryKey: ['webhook-endpoints'] })
                              toast.success('Secret rotated')
                            } catch (err) {
                              showApiError(err, 'Failed to rotate secret')
                            } finally {
                              setRotatingId(null)
                            }
                          }}>
                          {rotatingId === ep.id ? <><Loader2 size={12} className="animate-spin mr-1" />Rotating…</> : 'Rotate Secret'}
                        </Button>
                        <Button variant="outline" size="sm" className="h-7 text-xs"
                          onClick={() => setEditTarget(ep)}>
                          Edit
                        </Button>
                        <Button variant="outline" size="sm" className="h-7 text-xs text-destructive hover:text-destructive"
                          onClick={() => setDeleteTarget(ep)}>
                          Delete
                        </Button>
                      </div>
                    </TableCell>
                  </TableRow>
                  )
                })}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>

      {editTarget && (
        <EndpointEditDialog
          endpoint={editTarget}
          onClose={() => setEditTarget(null)}
          onSaved={() => {
            setEditTarget(null)
            queryClient.invalidateQueries({ queryKey: ['webhook-endpoints'] })
            toast.success('Endpoint updated')
          }}
        />
      )}

      {showCreate && (
        <CreateEndpointDialog
          onClose={() => setShowCreate(false)}
          onCreated={(secret) => {
            setShowCreate(false)
            setCreatedSecret({ secret })
            queryClient.invalidateQueries({ queryKey: ['webhook-endpoints'] })
            toast.success('Endpoint created')
          }}
        />
      )}

      {/* Secret display dialog — shown after create or rotate. Rotation
          response carries secondary_valid_until so we can tell the user
          their previous secret keeps working for the grace window,
          preventing a panic deploy. */}
      {createdSecret && (
        <Dialog open onOpenChange={() => setCreatedSecret(null)}>
          <DialogContent className="sm:max-w-md">
            <DialogHeader>
              <DialogTitle>Signing Secret</DialogTitle>
              {createdSecret.secondary_valid_until ? (
                <DialogDescription>
                  Save this secret now. It won't be shown again. The previous secret stays valid in parallel during the rollout window below.
                </DialogDescription>
              ) : (
                <DialogDescription>
                  Save this signing secret now. It will not be shown again.
                </DialogDescription>
              )}
            </DialogHeader>
            <div className="space-y-3">
              <div className="bg-amber-50 dark:bg-amber-500/10 border border-amber-200 dark:border-amber-500/20 rounded-lg p-4">
                <p className="text-xs text-amber-700 dark:text-amber-400 font-medium mb-1">New Signing Secret</p>
                <div className="flex items-start gap-2">
                  <p className="font-mono text-sm text-amber-900 dark:text-amber-300 break-all select-all flex-1">{createdSecret.secret}</p>
                  <Button variant="outline" size="sm" className="shrink-0"
                    onClick={() => {
                      navigator.clipboard.writeText(createdSecret.secret)
                      toast.success('Copied to clipboard')
                    }}>
                    Copy
                  </Button>
                </div>
              </div>

              {createdSecret.secondary_valid_until && (
                <div className="bg-emerald-50 dark:bg-emerald-500/10 border border-emerald-200 dark:border-emerald-500/20 rounded-lg p-4 space-y-1">
                  <p className="text-xs font-medium text-emerald-800 dark:text-emerald-300">
                    Previous secret valid until {formatDateTime(createdSecret.secondary_valid_until)}
                  </p>
                  <p className="text-xs text-emerald-700/80 dark:text-emerald-400/80">
                    Webhook deliveries are signed with both secrets during this window, so your receiver keeps accepting requests while you deploy the new key. After that the old secret is dropped.
                  </p>
                </div>
              )}
            </div>
            <DialogFooter>
              <Button onClick={() => setCreatedSecret(null)}>Done</Button>
            </DialogFooter>
          </DialogContent>
        </Dialog>
      )}

      {/* Delete confirmation */}
      <TypedConfirmDialog
        open={!!deleteTarget}
        onOpenChange={(open) => { if (!open) setDeleteTarget(null) }}
        title="Delete Endpoint"
        description={deleteTarget ? `Deleting "${deleteTarget.url}" will stop all future event deliveries to this URL. This action cannot be undone.` : ''}
        confirmWord="DELETE"
        confirmLabel="Delete Endpoint"
        onConfirm={handleDelete}
      />
    </>
  )
}

/* ─── Event Groups ─── */

// Must match the events the backend actually dispatches
// (internal/domain/webhook_outbound.go emit sites). An event listed here but
// never emitted is worse than omitting it — a subscriber gets silence forever.
const EVENT_GROUPS: { label: string; events: { type: string; description: string }[] }[] = [
  {
    label: 'Payment method',
    events: [
      { type: 'payment_method.attached', description: 'A payment method landed on a customer (Checkout setup completed)' },
      { type: 'payment_method.updated', description: "A customer's payment-method setup completed or refreshed (card brand + last4)" },
    ],
  },
  {
    label: 'Invoice',
    events: [
      { type: 'invoice.finalized', description: 'Invoice finalized and ready for payment' },
      { type: 'invoice.paid', description: 'Invoice fully paid (by card, credits, or an offline payment)' },
      { type: 'invoice.payment_recorded', description: 'An offline payment was recorded on an invoice' },
      { type: 'invoice.marked_uncollectible', description: 'Invoice written off as uncollectible (bad debt)' },
      { type: 'invoice.voided', description: 'Invoice voided' },
    ],
  },
  {
    label: 'Payment',
    events: [
      { type: 'payment.succeeded', description: 'Payment collected successfully' },
      { type: 'payment.failed', description: 'Payment attempt failed' },
      { type: 'payment.duplicate_charge', description: 'A second charge succeeded on an already-paid invoice — review and refund' },
      { type: 'payment.amount_mismatch', description: 'A charge settled for a different amount than the invoice was owed' },
      { type: 'payment.received_on_voided_invoice', description: 'A payment landed on a voided invoice — the customer is owed a refund' },
    ],
  },
  {
    label: 'Subscription',
    events: [
      { type: 'subscription.created', description: 'Subscription created' },
      { type: 'subscription.activated', description: 'Subscription activated' },
      { type: 'subscription.canceled', description: 'Subscription canceled' },
      { type: 'subscription.cancel_scheduled', description: 'Cancellation scheduled for the period end' },
      { type: 'subscription.cancel_cleared', description: 'A scheduled cancellation was cleared' },
      { type: 'subscription.collection_paused', description: 'Collection paused — invoices draft only' },
      { type: 'subscription.collection_resumed', description: 'Collection resumed' },
      { type: 'subscription.trial_ended', description: 'Trial ended' },
      { type: 'subscription.trial_extended', description: 'Trial extended' },
      { type: 'subscription.threshold_crossed', description: 'A usage billing threshold was crossed' },
      { type: 'subscription.item.added', description: 'An item was added to the subscription' },
      { type: 'subscription.item.updated', description: 'A subscription item changed (plan or quantity)' },
      { type: 'subscription.item.removed', description: 'An item was removed from the subscription' },
      { type: 'subscription.pending_change.scheduled', description: 'A change was scheduled for the next cycle' },
      { type: 'subscription.pending_change.applied', description: 'A scheduled change took effect' },
      { type: 'subscription.pending_change.canceled', description: 'A scheduled change was canceled' },
    ],
  },
  {
    label: 'Credits',
    events: [
      { type: 'credit.balance_low', description: 'Credit balance crossed below the configured low threshold — time to top up' },
      { type: 'credit.balance_depleted', description: 'Credit balance reached zero or below' },
      { type: 'credit.balance_recovered', description: 'Credit balance went positive again after being depleted' },
      { type: 'credit.commit_retired', description: 'A relief credit note retired commit credits (refund issued against the commit)' },
    ],
  },
  {
    label: 'Customer',
    events: [
      { type: 'customer.email_bounced', description: 'A customer email bounced and was suppressed' },
      { type: 'customer.email_complained', description: 'A customer marked an email as spam and was suppressed' },
    ],
  },
  {
    label: 'Dunning',
    events: [
      { type: 'dunning.started', description: 'Dunning process started' },
      { type: 'dunning.escalated', description: 'Dunning escalated after retries exhausted' },
      { type: 'dunning.resolved', description: 'Dunning resolved (payment recovered)' },
    ],
  },
]

const ALL_EVENT_TYPES = EVENT_GROUPS.flatMap(g => g.events.map(e => e.type))

/* ─── Shared event picker (create + edit dialogs) ─── */

function EventPicker({ selected, setSelected }: {
  selected: Set<string>
  setSelected: React.Dispatch<React.SetStateAction<Set<string>>>
}) {
  const toggleEvent = (eventType: string) => {
    setSelected(prev => {
      const next = new Set(prev)
      if (next.has(eventType)) next.delete(eventType)
      else next.add(eventType)
      return next
    })
  }
  const toggleGroup = (group: typeof EVENT_GROUPS[number]) => {
    const groupTypes = group.events.map(e => e.type)
    const allSelected = groupTypes.every(t => selected.has(t))
    setSelected(prev => {
      const next = new Set(prev)
      groupTypes.forEach(t => allSelected ? next.delete(t) : next.add(t))
      return next
    })
  }
  return (
    <ScrollPane className="border border-border rounded-lg max-h-72">
      <div className="flex items-center justify-between px-4 py-2.5 bg-muted border-b border-border sticky top-0 z-10">
        <label className="flex items-center gap-2 cursor-pointer">
          <Checkbox
            checked={selected.size === ALL_EVENT_TYPES.length}
            onCheckedChange={() => {
              if (selected.size === ALL_EVENT_TYPES.length) setSelected(new Set())
              else setSelected(new Set(ALL_EVENT_TYPES))
            }}
          />
          <span className="text-sm font-medium">Select all</span>
        </label>
        <span className="text-xs text-muted-foreground">{selected.size} of {ALL_EVENT_TYPES.length} selected</span>
      </div>
      {EVENT_GROUPS.map(group => {
        const groupTypes = group.events.map(e => e.type)
        const selectedCount = groupTypes.filter(t => selected.has(t)).length
        const allSelected = selectedCount === groupTypes.length
        return (
          <div key={group.label} className="border-b border-border last:border-b-0">
            <div className="flex items-center gap-2 px-4 py-2 bg-background">
              <Checkbox checked={allSelected} onCheckedChange={() => toggleGroup(group)} />
              <span className="text-sm font-semibold text-foreground">{group.label}</span>
              <span className="text-xs text-muted-foreground ml-auto">{selectedCount}/{groupTypes.length}</span>
            </div>
            {group.events.map(ev => (
              <label key={ev.type}
                className="flex items-start gap-2 px-4 py-1.5 pl-10 cursor-pointer hover:bg-accent transition-colors">
                <Checkbox
                  checked={selected.has(ev.type)}
                  onCheckedChange={() => toggleEvent(ev.type)}
                  className="mt-0.5 shrink-0"
                />
                <div className="min-w-0">
                  <p className="text-sm font-mono">{ev.type}</p>
                  <p className="text-xs text-muted-foreground">{ev.description}</p>
                </div>
              </label>
            ))}
          </div>
        )
      })}
    </ScrollPane>
  )
}

/* ─── Edit Endpoint Dialog ─── */

// PATCH without secret rotation — the receiver keeps verifying with the
// same signing secret while URL / events / active change. Also the surface
// that brings a recipe-created endpoint (inactive + placeholder URL) live.
function EndpointEditDialog({ endpoint, onClose, onSaved }: {
  endpoint: WebhookEndpoint
  onClose: () => void
  onSaved: () => void
}) {
  const isWildcard = !endpoint.events || endpoint.events.length === 0 || endpoint.events.includes('*')
  const [url, setUrl] = useState(endpoint.url)
  const [description, setDescription] = useState(endpoint.description || '')
  const [active, setActive] = useState(endpoint.active)
  const [listenMode, setListenMode] = useState<'all' | 'specific'>(isWildcard ? 'all' : 'specific')
  const [selectedEvents, setSelectedEvents] = useState<Set<string>>(
    new Set(isWildcard ? [] : endpoint.events),
  )
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState('')

  const save = async () => {
    setError('')
    if (!url.trim()) { setError('URL is required'); return }
    if (listenMode === 'specific' && selectedEvents.size === 0) {
      setError('Select at least one event')
      return
    }
    setSaving(true)
    try {
      await api.updateWebhookEndpoint(endpoint.id, {
        url: url.trim(),
        description: description.trim(),
        events: listenMode === 'all' ? ['*'] : Array.from(selectedEvents),
        active,
      })
      onSaved()
    } catch (err) {
      showApiError(err, 'Failed to update endpoint')
    } finally {
      setSaving(false)
    }
  }

  return (
    <Dialog open onOpenChange={(open) => { if (!open) onClose() }}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>Edit Webhook Endpoint</DialogTitle>
          <DialogDescription>
            URL, events, and status change without rotating the signing secret.
          </DialogDescription>
        </DialogHeader>
        <div className="space-y-4">
          <div className="space-y-1.5">
            <Label htmlFor="edit-ep-url">URL</Label>
            <Input id="edit-ep-url" type="url" maxLength={2048}
              value={url} onChange={e => setUrl(e.target.value)} />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="edit-ep-desc">Description</Label>
            <Input id="edit-ep-desc" maxLength={500}
              value={description} onChange={e => setDescription(e.target.value)} />
          </div>
          <label className="flex items-center gap-2 cursor-pointer">
            <Checkbox checked={active} onCheckedChange={v => setActive(v === true)} />
            <span className="text-sm">Active — deliver events to this endpoint</span>
          </label>
          <div>
            <Label className="mb-2 block">Events to send</Label>
            <div className="flex gap-4 mb-3">
              <label className="flex items-center gap-2 cursor-pointer">
                <input type="radio" name="editListenMode" checked={listenMode === 'all'}
                  onChange={() => setListenMode('all')} className="w-4 h-4 text-primary" />
                <span className="text-sm">Listen to all events</span>
              </label>
              <label className="flex items-center gap-2 cursor-pointer">
                <input type="radio" name="editListenMode" checked={listenMode === 'specific'}
                  onChange={() => setListenMode('specific')} className="w-4 h-4 text-primary" />
                <span className="text-sm">Select specific events</span>
              </label>
            </div>
            {listenMode === 'specific' && (
              <EventPicker selected={selectedEvents} setSelected={setSelectedEvents} />
            )}
          </div>
          {error && <p className="text-destructive text-sm">{error}</p>}
          <div className="flex justify-end gap-2">
            <Button variant="outline" onClick={onClose}>Cancel</Button>
            <Button onClick={save} disabled={saving}>
              {saving ? <><Loader2 size={14} className="animate-spin mr-2" />Saving…</> : 'Save changes'}
            </Button>
          </div>
        </div>
      </DialogContent>
    </Dialog>
  )
}

/* ─── Create Endpoint Dialog ─── */

function CreateEndpointDialog({ onClose, onCreated }: { onClose: () => void; onCreated: (secret: string) => void }) {
  const [listenMode, setListenMode] = useState<'all' | 'specific'>('all')
  const [selectedEvents, setSelectedEvents] = useState<Set<string>>(new Set())
  const [eventsError, setEventsError] = useState('')

  const form = useForm<CreateEndpointData>({
    resolver: zodResolver(createEndpointSchema),
    defaultValues: { url: '', description: '' },
  })

  const onSubmit = form.handleSubmit(async (data) => {
    setEventsError('')
    try {
      const eventList = listenMode === 'all' ? undefined : Array.from(selectedEvents)
      if (listenMode === 'specific' && (!eventList || eventList.length === 0)) {
        setEventsError('Select at least one event')
        return
      }
      const res = await api.createWebhookEndpoint({
        url: data.url,
        description: data.description || undefined,
        events: eventList,
      })
      onCreated(res.secret)
    } catch (err) {
      applyApiError(form, err, ['url', 'description'], {
        toastTitle: 'Failed to create endpoint',
      })
    }
  })

  return (
    <Dialog open onOpenChange={(open) => { if (!open) onClose() }}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>Add Webhook Endpoint</DialogTitle>
          <DialogDescription>Configure a URL to receive event notifications</DialogDescription>
        </DialogHeader>
        <Form {...form}>
        <form onSubmit={onSubmit} noValidate className="space-y-4">
          <FormField
            control={form.control}
            name="url"
            render={({ field }) => (
              <FormItem>
                <FormLabel>URL</FormLabel>
                <FormControl>
                  <Input type="url" placeholder="https://example.com/webhooks" maxLength={2048} {...field} />
                </FormControl>
                <FormMessage />
              </FormItem>
            )}
          />
          <FormField
            control={form.control}
            name="description"
            render={({ field }) => (
              <FormItem>
                <FormLabel>Description</FormLabel>
                <FormControl>
                  <Input placeholder="Production webhook" maxLength={500} {...field} />
                </FormControl>
                <FormMessage />
              </FormItem>
            )}
          />

          {/* Event selection */}
          <div>
            <Label className="mb-2 block">Events to send</Label>
            <div className="flex gap-4 mb-3">
              <label className="flex items-center gap-2 cursor-pointer">
                <input type="radio" name="listenMode" checked={listenMode === 'all'}
                  onChange={() => setListenMode('all')}
                  className="w-4 h-4 text-primary" />
                <span className="text-sm">Listen to all events</span>
              </label>
              <label className="flex items-center gap-2 cursor-pointer">
                <input type="radio" name="listenMode" checked={listenMode === 'specific'}
                  onChange={() => setListenMode('specific')}
                  className="w-4 h-4 text-primary" />
                <span className="text-sm">Select specific events</span>
              </label>
            </div>

            {listenMode === 'specific' && (
              <EventPicker selected={selectedEvents} setSelected={setSelectedEvents} />
            )}
            {eventsError && <p className="text-destructive text-sm mt-2">{eventsError}</p>}
          </div>

          <DialogFooter>
            <Button type="button" variant="outline" onClick={onClose}>Cancel</Button>
            <Button type="submit" disabled={form.formState.isSubmitting}>
              {form.formState.isSubmitting ? (
                <><Loader2 size={14} className="animate-spin mr-2" /> Creating...</>
              ) : (
                'Create Endpoint'
              )}
            </Button>
          </DialogFooter>
        </form>
        </Form>
      </DialogContent>
    </Dialog>
  )
}

/* ─── Events Tab ─── */

function EventsTab() {
  const { data: eventsData, isLoading: loading, error: loadError, refetch } = useQuery({
    queryKey: ['webhook-events'],
    queryFn: () => api.listWebhookEvents(),
  })

  const events = eventsData?.data ?? []
  const errorMsg = loadError instanceof Error ? loadError.message : loadError ? String(loadError) : null

  const handleReplay = async (id: string) => {
    try {
      await api.replayWebhookEvent(id)
      toast.success('Event replayed')
    } catch (err) {
      showApiError(err, 'Failed to replay event')
    }
  }

  return (
    <>
      <div className="flex justify-end mt-4">
        <Button size="sm" variant="outline" onClick={() => { window.location.href = '/webhooks/events' }}>
          <Activity size={14} className="mr-2" />
          Open Live Tail
        </Button>
      </div>
    <Card className="mt-4">
      <CardContent className="p-0">
        {errorMsg ? (
          <div className="p-8 text-center">
            <p className="text-sm text-destructive mb-3">{errorMsg}</p>
            <Button variant="outline" size="sm" onClick={() => refetch()}>Retry</Button>
          </div>
        ) : loading ? (
          <TableSkeleton columns={4} />
        ) : events.length === 0 ? (
          <EmptyState
            icon={Activity}
            title="No webhook events"
            description="Events will appear here as they are dispatched to your endpoints."
          />
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Event Type</TableHead>
                <TableHead>Event ID</TableHead>
                <TableHead>Created</TableHead>
                <TableHead className="text-right"></TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {events.map(ev => (
                <TableRow key={ev.id}>
                  <TableCell><Badge variant="outline">{ev.event_type}</Badge></TableCell>
                  <TableCell className="font-mono text-xs text-muted-foreground">
                    {/* The prefix is the same on every row (vlx_whevt_…) —
                        show the distinguishing tail and make the full id
                        copyable instead of printing 2 useful characters. */}
                    <span className="inline-flex items-center gap-1" title={ev.id}>
                      …{ev.id.slice(-8)}
                      <CopyButton text={ev.id} />
                    </span>
                  </TableCell>
                  <TableCell className="text-muted-foreground">{formatDate(ev.created_at)}</TableCell>
                  <TableCell className="text-right">
                    <Button variant="outline" size="sm" className="h-7 text-xs" onClick={() => handleReplay(ev.id)}>
                      Replay
                    </Button>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </CardContent>
    </Card>
    </>
  )
}
