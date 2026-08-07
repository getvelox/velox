import { useState } from 'react'
import { usePageTitle } from '@/hooks/usePageTitle'
import { useNavigate, useParams } from 'react-router-dom'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'
import { ArrowLeft, ChevronDown, ChevronRight, Inbox, RefreshCw } from 'lucide-react'

import { api, type EndpointDelivery } from '@/lib/api'
// Webhook deliveries are infrastructure egress — never clock-pinned — so
// their timestamps are genuinely wall-clock, same family as the audit log
// and the events live tail (ADR-030 forensic layer).
import { timeAgo, timeUntil, wallClockNow } from '@/lib/effectiveNow'
import { showApiError } from '@/lib/formErrors'
import { Layout } from '@/components/Layout'
import { Tabs, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { EmptyState } from '@/components/EmptyState'
import { ScrollPane } from '@/components/ui/scroll-pane'
import { TableSkeleton } from '@/components/ui/TableSkeleton'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'

// One receiver's delivery history — the drill-down behind the endpoints
// table's success-rate band. The events live tail answers "did business
// fact X propagate?"; this page answers the other operator question,
// "is MY receiver healthy, what's failing, and can I resend it?" —
// which is why replay here is scoped to this endpoint only.
export default function WebhookEndpointDetailPage() {
  usePageTitle('Webhook endpoint')
  const navigate = useNavigate()
  const { id } = useParams<{ id: string }>()
  const queryClient = useQueryClient()
  const [expanded, setExpanded] = useState<string | null>(null)
  const [replayingId, setReplayingId] = useState<string | null>(null)

  const { data, isLoading, error } = useQuery({
    queryKey: ['webhook-endpoint-deliveries', id],
    queryFn: () => api.listWebhookEndpointDeliveries(id!),
    enabled: !!id,
    // A pending row's ladder resolves on the retry worker's cadence;
    // refetch keeps the page honest without an SSE dependency.
    refetchInterval: 10_000,
  })

  const endpoint = data?.endpoint
  const deliveries = data?.data ?? []
  const stats = summarize(deliveries)

  const handleReplay = async (d: EndpointDelivery) => {
    if (!id || replayingId) return
    setReplayingId(d.id)
    try {
      await api.replayWebhookDelivery(id, d.id)
      toast.success('Redelivery queued to this endpoint')
      queryClient.invalidateQueries({ queryKey: ['webhook-endpoint-deliveries', id] })
    } catch (err) {
      showApiError(err, 'Failed to replay delivery')
    } finally {
      setReplayingId(null)
    }
  }

  return (
    <Layout>
      {/* Same header + tab strip as /webhooks — this page is a drill-down
          of the Endpoints tab, not a third surface. */}
      <div>
        <h1 className="text-2xl font-semibold text-foreground">Webhooks</h1>
        <p className="text-sm text-muted-foreground mt-1">Manage outbound webhook endpoints and events</p>
      </div>

      <Tabs value="endpoints" onValueChange={(t) => { if (t === 'events') navigate('/webhooks/events') }} className="mt-6">
        <TabsList>
          <TabsTrigger value="endpoints">Endpoints</TabsTrigger>
          <TabsTrigger value="events">Events</TabsTrigger>
        </TabsList>
      </Tabs>

      <div className="mt-4">
        <Button variant="ghost" size="sm" className="text-muted-foreground -ml-2" onClick={() => navigate('/webhooks')}>
          <ArrowLeft size={14} className="mr-1" />
          All endpoints
        </Button>
      </div>

      {error ? (
        <Card className="mt-3">
          <CardContent className="p-8 text-center">
            <p className="text-sm text-destructive">
              {error instanceof Error ? error.message : 'Failed to load endpoint'}
            </p>
          </CardContent>
        </Card>
      ) : (
        <>
          <div className="mt-3 flex flex-wrap items-center gap-x-3 gap-y-2">
            <span className="font-mono text-base text-foreground break-all">{endpoint?.url ?? '…'}</span>
            {endpoint && (
              <Badge variant={endpoint.active ? 'success' : 'secondary'}>
                {endpoint.active ? 'active' : 'paused'}
              </Badge>
            )}
          </div>
          {endpoint?.description && (
            <p className="text-sm text-muted-foreground mt-1">{endpoint.description}</p>
          )}
          {endpoint && (
            <div className="flex flex-wrap items-center gap-x-4 gap-y-1 mt-2 text-sm text-muted-foreground">
              <span>
                {stats.completed === 0
                  ? 'Nothing completed yet'
                  : <>Success rate <span className={rateColor(stats.rate)}>{stats.rate.toFixed(1)}%</span> over {stats.completed} completed</>}
                {stats.pending > 0 && <> {'·'} {stats.pending} pending</>}
              </span>
              <span className="flex flex-wrap gap-1">
                {(endpoint.events || []).map(ev => (
                  <Badge key={ev} variant="outline">{ev}</Badge>
                ))}
                {(!endpoint.events || endpoint.events.length === 0) && <Badge variant="outline">all events</Badge>}
              </span>
            </div>
          )}

          <Card className="mt-4">
            <CardContent className="p-0">
              {isLoading ? (
                <TableSkeleton columns={6} />
              ) : deliveries.length === 0 ? (
                <EmptyState
                  icon={Inbox}
                  title="No deliveries yet"
                  description="Events matching this endpoint's subscriptions will appear here as they are delivered."
                />
              ) : (
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead className="w-8"></TableHead>
                      <TableHead>Event</TableHead>
                      <TableHead>Status</TableHead>
                      <TableHead>HTTP</TableHead>
                      <TableHead>Attempts</TableHead>
                      <TableHead>When</TableHead>
                      <TableHead className="text-right"></TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {deliveries.map(d => (
                      <DeliveryRows
                        key={d.id}
                        delivery={d}
                        expanded={expanded === d.id}
                        onToggle={() => setExpanded(expanded === d.id ? null : d.id)}
                        onReplay={() => handleReplay(d)}
                        replaying={replayingId === d.id}
                        replayDisabled={!!replayingId || !endpoint?.active}
                      />
                    ))}
                  </TableBody>
                </Table>
              )}
            </CardContent>
          </Card>
        </>
      )}
    </Layout>
  )
}

// summarize mirrors the backend's endpointSuccessRate: completed-only
// denominator (succeeded + failed), pending excluded — the page-level
// numbers must agree with the band the operator clicked to get here.
// Computed over the visible window (latest 50), so it can differ from
// the all-time band after long histories; the copy says "over N
// completed" to keep the scope honest.
function summarize(deliveries: EndpointDelivery[]) {
  let succeeded = 0
  let failed = 0
  let pending = 0
  for (const d of deliveries) {
    if (d.status === 'succeeded') succeeded++
    else if (d.status === 'failed') failed++
    else pending++
  }
  const completed = succeeded + failed
  return {
    completed,
    pending,
    rate: completed === 0 ? 0 : (succeeded / completed) * 100,
  }
}

function rateColor(rate: number) {
  return rate >= 95 ? 'text-green-600' : rate >= 70 ? 'text-amber-600' : 'text-red-600'
}

function statusBadge(status: EndpointDelivery['status']) {
  if (status === 'succeeded') return <Badge variant="success">succeeded</Badge>
  if (status === 'failed') return <Badge variant="destructive">failed</Badge>
  return <Badge variant="secondary">pending</Badge>
}

function DeliveryRows({
  delivery: d,
  expanded,
  onToggle,
  onReplay,
  replaying,
  replayDisabled,
}: {
  delivery: EndpointDelivery
  expanded: boolean
  onToggle: () => void
  onReplay: () => void
  replaying: boolean
  replayDisabled: boolean
}) {
  return (
    <>
      <TableRow className="cursor-pointer" onClick={onToggle}>
        <TableCell className="text-muted-foreground">
          {expanded ? <ChevronDown size={14} /> : <ChevronRight size={14} />}
        </TableCell>
        <TableCell>
          <div className="flex items-center gap-2">
            <Badge variant="outline">{d.event_type}</Badge>
            {d.is_replay && <Badge variant="secondary">replay</Badge>}
          </div>
          <div className="font-mono text-xs text-muted-foreground mt-0.5">{d.event_id}</div>
        </TableCell>
        <TableCell>{statusBadge(d.status)}</TableCell>
        <TableCell className="font-mono text-sm">
          {d.status_code > 0 ? d.status_code : '—'}
        </TableCell>
        <TableCell className="text-sm">
          {d.attempt_count}
          {d.status === 'pending' && d.next_retry_at && (
            <span className="text-xs text-muted-foreground"> {'·'} retry {timeUntil(d.next_retry_at, wallClockNow())}</span>
          )}
        </TableCell>
        <TableCell className="text-sm text-muted-foreground" title={d.created_at}>
          {timeAgo(d.created_at, wallClockNow())}
        </TableCell>
        <TableCell className="text-right">
          <Button
            variant="outline"
            size="sm"
            className="h-7 text-xs"
            disabled={replayDisabled}
            title={replayDisabled && !replaying ? 'Resume the endpoint to replay deliveries' : undefined}
            onClick={(e) => {
              e.stopPropagation()
              onReplay()
            }}
          >
            <RefreshCw size={12} className={replaying ? 'mr-1 animate-spin' : 'mr-1'} />
            {replaying ? 'Replaying…' : 'Replay'}
          </Button>
        </TableCell>
      </TableRow>
      {expanded && (
        <TableRow>
          <TableCell colSpan={7} className="bg-muted/30">
            <div className="px-2 py-3 space-y-2 text-sm">
              {d.error && (
                <div>
                  <span className="text-xs font-semibold uppercase text-muted-foreground mr-2">Error</span>
                  <span className="text-destructive">{d.error}</span>
                </div>
              )}
              <div>
                <span className="text-xs font-semibold uppercase text-muted-foreground mr-2">Response body</span>
                {d.response_body ? (
                  <ScrollPane as="pre" className="mt-1 p-2 rounded bg-card border border-border font-mono text-xs whitespace-pre-wrap break-all max-h-48">
                    {d.response_body}
                  </ScrollPane>
                ) : (
                  <span className="text-muted-foreground">empty</span>
                )}
              </div>
              <div className="text-xs text-muted-foreground">
                Created {timeAgo(d.created_at, wallClockNow())}
                {d.completed_at && <> {'·'} completed {timeAgo(d.completed_at, wallClockNow())}</>}
                {d.is_replay && d.replay_of_event_id && (
                  <> {'·'} replay of <span className="font-mono">{d.replay_of_event_id}</span></>
                )}
              </div>
            </div>
          </TableCell>
        </TableRow>
      )}
    </>
  )
}
