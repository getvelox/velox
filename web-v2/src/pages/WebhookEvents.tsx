import { useEffect, useMemo, useRef, useState } from 'react'
import { usePageTitle } from '@/hooks/usePageTitle'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'
import { Activity, ChevronDown, ChevronRight, RefreshCw } from 'lucide-react'

import {
  api,
  formatDateTime,
  type WebhookEventStreamFrame,
  type WebhookDeliveryView,
} from '@/lib/api'
// Webhook deliveries are infrastructure egress — never clock-pinned — so their
// timestamps are genuinely wall-clock (wallClockNow), same family as the audit
// log and email outbox (ADR-030 forensic layer).
import { timeAgo, wallClockNow, timeUntil } from '@/lib/effectiveNow'
import { showApiError } from '@/lib/formErrors'
import { useNavigate } from 'react-router-dom'
import { Layout } from '@/components/Layout'
import { Tabs, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import { Card, CardContent } from '@/components/ui/card'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { EmptyState } from '@/components/EmptyState'
import { ScrollPane } from '@/components/ui/scroll-pane'

// Live-tail buffer cap. The dashboard's table re-renders on every frame
// flip; capping at 200 keeps the list snappy on a busy production tenant
// without forcing the operator to refresh to see history (the SSE
// snapshot path on connect already returns the last 50, so 200 in the
// buffer means roughly 4× recent history is visible at any time).
const MAX_BUFFER = 200

// Connection-status pill states. The SSE handler reconnects automatically
// on transient disconnects; we surface the status so the operator knows
// the difference between "no events lately" and "we lost the stream".
type StreamStatus = 'connecting' | 'live' | 'reconnecting' | 'error'

export default function WebhookEventsPage() {
  usePageTitle('Webhook events')
  const navigate = useNavigate()
  // Buffer of frames keyed by event_id. We dedupe on event_id because the
  // bus can emit two frames for the same event (pending → succeeded), and
  // the snapshot path can race with the first live frame; collapsing in
  // place is what keeps the row from flickering or duplicating.
  const [frames, setFrames] = useState<WebhookEventStreamFrame[]>([])
  // Resolve customer ids to display names for exactly the frames on
  // screen (ids=). The old limit=100 page meant any event from a
  // customer past the 100th rendered a raw id forever (P11).
  const frameCustomerIds = useMemo(
    () => Array.from(new Set(frames.map(f => f.customer_id).filter((id): id is string => !!id))),
    [frames],
  )
  const { data: customersData } = useQuery({
    queryKey: ['we-customers', frameCustomerIds],
    queryFn: () => api.listCustomers(`ids=${frameCustomerIds.join(',')}&limit=${frameCustomerIds.length}`),
    enabled: frameCustomerIds.length > 0,
    staleTime: 60_000,
  })
  const customerName = useMemo(() => {
    const m: Record<string, string> = {}
    for (const c of customersData?.data ?? []) m[c.id] = c.display_name
    return m
  }, [customersData])
  const [status, setStatus] = useState<StreamStatus>('connecting')
  const [expanded, setExpanded] = useState<string | null>(null)
  // Clone event id of the operator's most recent replay. The timeline
  // highlights and scrolls to its deliveries — in a family of 30+ rows
  // the just-created delivery lands at the BOTTOM of the chronological
  // list, off-screen, so without this the auto-expand shows everything
  // except the thing the operator just did.
  const [highlightEventId, setHighlightEventId] = useState<string | null>(null)
  const queryClient = useQueryClient()
  const eventSourceRef = useRef<EventSource | null>(null)

  useEffect(() => {
    // EventSource opens with credentials: 'include' on same-origin reqs
    // by default in the browsers we support. We're cookie-authed so this
    // is the only thing we need to do — no Authorization header.
    const es = new EventSource('/v1/webhook_events/stream', { withCredentials: true })
    eventSourceRef.current = es
    // No setStatus here: the state INITIALIZES to 'connecting', and the
    // open/error listeners below own every later transition. Mount paths
    // (incl. StrictMode double-invoke) can't observe anything else; the
    // one exception is dev Fast Refresh, which re-runs this effect with
    // state preserved — the pill may briefly keep its old label until
    // the new socket's 'open' fires. Dev-only and self-correcting.

    es.addEventListener('open', () => setStatus('live'))

    es.addEventListener('webhook_event', (ev: MessageEvent) => {
      try {
        const frame = JSON.parse(ev.data) as WebhookEventStreamFrame
        setFrames(prev => {
          // Replace by event_id; new event_ids prepend so newest renders
          // at the top. Cap to MAX_BUFFER to bound DOM / memory growth.
          const idx = prev.findIndex(f => f.event_id === frame.event_id)
          if (idx >= 0) {
            const next = [...prev]
            next[idx] = frame
            return next
          }
          return [frame, ...prev].slice(0, MAX_BUFFER)
        })
      } catch (err) {
        // Bad frame JSON shouldn't break the stream — log and move on.
        console.error('parse webhook_event frame', err, ev.data)
      }
    })

    es.addEventListener('error', () => {
      // EventSource auto-reconnects; surface the in-between state so the
      // operator knows the data freshness might be stale. readyState
      // CLOSED (2) means the browser gave up; CONNECTING (0) means it's
      // attempting reconnection.
      if (es.readyState === EventSource.CLOSED) {
        setStatus('error')
      } else {
        setStatus('reconnecting')
      }
    })

    return () => {
      es.close()
      eventSourceRef.current = null
    }
  }, [])

  // Sort by created_at desc as a stability guard — the buffer is mostly
  // ordered by arrival but a snapshot frame interleaved with a live one
  // can swap places on dedupe. Newest-first matches Stripe's events page
  // and is what operators expect when scanning incidents.
  const sortedFrames = useMemo(() => {
    return [...frames].sort((a, b) => {
      const ta = new Date(a.created_at).getTime()
      const tb = new Date(b.created_at).getTime()
      return tb - ta
    })
  }, [frames])

  const handleReplay = async (id: string) => {
    try {
      const res = await api.replayWebhookEventV2(id)
      toast.success(`Replayed event \u2014 clone ${res.event_id.slice(0, 12)}\u2026 queued`)
      // Expand the replayed row AND refetch its timeline. A timeline
      // that was already open holds a mounted query nothing else
      // invalidates \u2014 pre-fix it silently kept showing the pre-replay
      // family, so "watch the new attempt land" showed everything but.
      // Prefix-invalidate: every family member's timeline shows the
      // same stitched story, so all mounted copies must move together.
      setHighlightEventId(res.event_id)
      queryClient.invalidateQueries({ queryKey: ['webhook-deliveries'] })
      setExpanded(id)
    } catch (err) {
      showApiError(err, 'Failed to replay event')
    }
  }

  return (
    <Layout>
      {/* Same header + tab strip as /webhooks: Endpoints and Events are two
          tabs of ONE surface, not two products. This page owns the Events
          tab; selecting Endpoints navigates back. The duplicate plain list
          that used to live behind /webhooks?tab=events is gone — this view
          is the only events surface. */}
      <div>
        <h1 className="text-2xl font-semibold text-foreground">Webhooks</h1>
        <p className="text-sm text-muted-foreground mt-1">Manage outbound webhook endpoints and events</p>
      </div>

      <Tabs value="events" onValueChange={(t) => { if (t === 'endpoints') navigate('/webhooks') }} className="mt-6">
        <TabsList>
          <TabsTrigger value="endpoints">Endpoints</TabsTrigger>
          <TabsTrigger value="events">Events</TabsTrigger>
        </TabsList>
      </Tabs>

      <div className="flex items-start justify-between gap-4 mt-4">
        <p className="text-sm text-muted-foreground">
          Real-time tail of every event dispatched to your endpoints. Click a row to see per-attempt delivery history and replay.
        </p>
        <ConnectionPill status={status} count={sortedFrames.length} />
      </div>

      <Card className="mt-4">
        <CardContent className="p-0">
          {sortedFrames.length === 0 ? (
            <EmptyState
              icon={Activity}
              title={status === 'live' ? 'Waiting for events' : 'Connecting'}
              description={
                status === 'live'
                  ? 'No webhook events yet. Trigger an event from your account and it will appear here in real time.'
                  : 'Establishing live-tail connection\u2026'
              }
            />
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead className="w-8"></TableHead>
                  <TableHead>Event</TableHead>
                  <TableHead>Customer</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead>When</TableHead>
                  <TableHead className="text-right"></TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {sortedFrames.map(frame => {
                  const isExpanded = expanded === frame.event_id
                  return (
                    <FrameRows
                      customerName={customerName}
                      key={frame.event_id}
                      frame={frame}
                      expanded={isExpanded}
                      highlightEventId={highlightEventId}
                      onToggle={() => setExpanded(isExpanded ? null : frame.event_id)}
                      onReplay={() => handleReplay(frame.event_id)}
                    />
                  )
                })}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>
    </Layout>
  )
}

// FrameRows renders the summary row + (when expanded) the per-attempt
// deliveries timeline. Split into its own component so each row can lazy-
// fetch its deliveries only when expanded — a tenant with hundreds of
// events in the tail shouldn't fire a deliveries query for every row.
function FrameRows({
  frame,
  expanded,
  highlightEventId,
  onToggle,
  onReplay,
  customerName,
}: {
  frame: WebhookEventStreamFrame
  expanded: boolean
  highlightEventId: string | null
  onToggle: () => void
  onReplay: () => void
  customerName: Record<string, string>
}) {
  return (
    <>
      <TableRow
        className="cursor-pointer hover:bg-accent/40"
        onClick={onToggle}
      >
        <TableCell className="w-8 text-muted-foreground">
          {expanded ? <ChevronDown size={14} /> : <ChevronRight size={14} />}
        </TableCell>
        <TableCell>
          <div className="flex items-center gap-2">
            <Badge variant="outline">{frame.event_type}</Badge>
            {frame.replay_of_event_id && (
              <Badge variant="secondary" className="text-xs">replay</Badge>
            )}
          </div>
          <div className="text-xs text-muted-foreground font-mono mt-1">
            {frame.event_id}
          </div>
        </TableCell>
        <TableCell className="text-sm text-muted-foreground">
          {frame.customer_id
            ? (customerName[frame.customer_id]
              ?? <span className="font-mono" title={frame.customer_id}>{frame.customer_id.slice(0, 12)}…</span>)
            : '\u2014'}
        </TableCell>
        <TableCell>
          <StatusBadge status={frame.status} />
        </TableCell>
        <TableCell className="text-muted-foreground" title={formatDateTime(frame.created_at)}>
          {timeAgo(frame.created_at, wallClockNow())}
        </TableCell>
        <TableCell className="text-right">
          <Button
            variant="outline"
            size="sm"
            className="h-7 text-xs"
            onClick={e => {
              e.stopPropagation()
              onReplay()
            }}
          >
            <RefreshCw size={12} className="mr-1.5" />
            Replay
          </Button>
        </TableCell>
      </TableRow>
      {expanded && (
        <TableRow className="bg-muted/30">
          <TableCell colSpan={6} className="p-0">
            <DeliveryTimeline eventID={frame.event_id} highlightEventId={highlightEventId} />
          </TableCell>
        </TableRow>
      )}
    </>
  )
}

// DeliveryTimeline fetches the per-attempt history for a given event and
// renders a stripped-down timeline. The diff view ("payload identical"
// flag) compares request_payload_sha256 across attempts — Stripe-style
// replays don't mutate the payload, so this collapses to "identical" in
// the common case.
function DeliveryTimeline({ eventID, highlightEventId }: { eventID: string; highlightEventId: string | null }) {
  const { data, isLoading, error } = useQuery({
    queryKey: ['webhook-deliveries', eventID],
    queryFn: () => api.listWebhookDeliveries(eventID),
    // The family keeps moving while the row is open: replay attempts
    // land async, and a pending delivery resolves on the retry worker's
    // 30s cadence. A mounted (= expanded) timeline polls so those flips
    // arrive without collapsing and re-expanding the row.
    refetchInterval: 5_000,
  })

  const deliveries = data?.deliveries ?? []
  // First delivery born from the operator's latest replay — the ring
  // marks all of them, the scroll goes to the first.
  const firstHighlight = highlightEventId
    ? deliveries.findIndex(d => d.event_id === highlightEventId)
    : -1
  const highlightRef = useRef<HTMLLIElement | null>(null)
  useEffect(() => {
    if (firstHighlight >= 0) {
      const reduced = window.matchMedia('(prefers-reduced-motion: reduce)').matches
      highlightRef.current?.scrollIntoView({ block: 'nearest', behavior: reduced ? 'auto' : 'smooth' })
    }
  }, [firstHighlight])

  if (isLoading) {
    return (
      <div className="px-6 py-4 text-sm text-muted-foreground">
        Loading delivery history{'\u2026'}
      </div>
    )
  }
  if (error) {
    return (
      <div className="px-6 py-4 text-sm text-destructive">
        Failed to load delivery history.
      </div>
    )
  }
  if (deliveries.length === 0) {
    return (
      <div className="px-6 py-4 text-sm text-muted-foreground">
        No delivery attempts yet {'\u2014'} the event is queued.
      </div>
    )
  }
  // The timeline stitches the ORIGINAL event's deliveries together with
  // every replay clone's — one shared story, oldest first. Say so in the
  // header: a row whose own fan-out was 3 deliveries can legitimately
  // show a 30-row timeline once operators have replayed it, and an
  // unexplained count reads as a bug.
  const replayCount = new Set(deliveries.filter(d => d.is_replay).map(d => d.event_id)).size
  return (
    <div className="px-6 py-4">
      <h3 className="text-xs font-semibold uppercase text-muted-foreground mb-3">
        {/* "deliveries", not "attempts": each row is one delivery, whose own
            retry ladder can span up to 6 HTTP attempts (attempt_count). The
            old label under-counted reality by up to 6×. */}
        Delivery Timeline ({deliveries.length} {deliveries.length === 1 ? 'delivery' : 'deliveries'}
        {replayCount > 0 && ` · original + ${replayCount} ${replayCount === 1 ? 'replay' : 'replays'}`})
      </h3>
      <ol className="space-y-3">
        {deliveries.map((d, i) => (
          <li
            key={d.id}
            ref={i === firstHighlight ? highlightRef : undefined}
            className={
              'border rounded-md bg-card' +
              (highlightEventId && d.event_id === highlightEventId
                ? ' border-primary ring-2 ring-primary/40'
                : ' border-border')
            }
          >
            <DeliveryRow delivery={d} prevSha={i > 0 ? deliveries[i - 1].request_payload_sha256 : null} />
          </li>
        ))}
      </ol>
    </div>
  )
}

function DeliveryRow({
  delivery,
  prevSha,
}: {
  delivery: WebhookDeliveryView
  prevSha: string | null
}) {
  const [showBody, setShowBody] = useState(false)
  // Diff signal: identical vs different vs first attempt. We don't render
  // the actual JSON diff here (clones share an identical payload by
  // construction in v1); we render the verdict so the operator can spot
  // an unexpected payload mutation immediately.
  const diffLabel =
    prevSha == null ? 'first attempt'
      : prevSha === delivery.request_payload_sha256 ? 'payload identical to previous'
      : 'payload differs from previous'

  return (
    <div className="p-3">
      <div className="flex items-start justify-between gap-4">
        <div className="min-w-0">
          <div className="flex items-center gap-2 flex-wrap">
            {/* attempt_no is a global sequence across endpoints + replay
                clones — labeling it "Attempt #" implied per-endpoint
                numbering. "Delivery #" matches what the number actually
                counts. */}
            <span className="font-medium text-sm">Delivery #{delivery.attempt_no}</span>
            <StatusBadge status={delivery.status} />
            {delivery.is_replay && (
              <Badge variant="secondary" className="text-xs">replay clone</Badge>
            )}
            {delivery.status_code > 0 && (
              <Badge variant="outline" className="font-mono text-xs">HTTP {delivery.status_code}</Badge>
            )}
          </div>
          {/* Operator-first attribution: name the receiver by URL (and
              the operator's own description), not by an opaque id the
              endpoints page never displays. The id stays available on
              hover for support tickets / API cross-reference. */}
          <div className="text-xs text-muted-foreground mt-1 break-all" title={delivery.endpoint_id}>
            <span className="font-mono">{delivery.endpoint_url || delivery.endpoint_id}</span>
            {delivery.endpoint_description && (
              <span className="ml-1">· {delivery.endpoint_description}</span>
            )}
          </div>
          <div className="text-xs text-muted-foreground mt-1" title={formatDateTime(delivery.attempted_at)}>
            attempted {timeAgo(delivery.attempted_at, wallClockNow())}
            {delivery.completed_at && (
              <>
                {' \u00b7 completed '}{timeAgo(delivery.completed_at, wallClockNow())}
              </>
            )}
            {delivery.next_retry_at && (
              <>
                {' \u00b7 next retry '}{timeUntil(delivery.next_retry_at, wallClockNow())}
              </>
            )}
          </div>
          <div className="text-xs text-muted-foreground mt-1">
            <span className="font-mono" title={delivery.request_payload_sha256}>
              {'sha256 '}{delivery.request_payload_sha256.slice(0, 16)}{'\u2026'}
            </span>
            {' \u00b7 '}{diffLabel}
          </div>
          {delivery.error && (
            <div className="text-xs text-destructive mt-1 break-words">
              {delivery.error}
            </div>
          )}
        </div>
        {(delivery.response_body || delivery.error) && (
          <Button
            variant="outline"
            size="sm"
            className="h-7 text-xs shrink-0"
            onClick={() => setShowBody(prev => !prev)}
          >
            {showBody ? 'Hide body' : 'Show body'}
          </Button>
        )}
      </div>
      {showBody && (delivery.response_body || delivery.error) && (
        <ScrollPane as="pre" className="mt-3 p-2 bg-muted rounded text-xs font-mono whitespace-pre-wrap break-all max-h-48">
          {delivery.response_body || delivery.error}
        </ScrollPane>
      )}
    </div>
  )
}

function StatusBadge({ status }: { status: string }) {
  // Map service-side status strings to the dashboard variants. Anything
  // we don't recognize falls through to the neutral 'outline' so a future
  // server-side status (e.g. "throttled") doesn't crash the render.
  const variant: 'default' | 'secondary' | 'destructive' | 'success' | 'outline' =
    status === 'succeeded' || status === 'delivered'
      ? 'success'
      : status === 'failed'
      ? 'destructive'
      : status === 'pending'
      ? 'secondary'
      : 'outline'
  return <Badge variant={variant}>{status}</Badge>
}

function ConnectionPill({ status, count }: { status: StreamStatus; count: number }) {
  const label =
    status === 'live' ? `Live \u00b7 ${count} ${count === 1 ? 'event' : 'events'}`
      : status === 'connecting' ? 'Connecting\u2026'
      : status === 'reconnecting' ? 'Reconnecting\u2026'
      : 'Disconnected'
  const dotClass =
    status === 'live' ? 'bg-emerald-500'
      : status === 'connecting' || status === 'reconnecting' ? 'bg-amber-500 animate-pulse'
      : 'bg-destructive'
  return (
    <div className="flex items-center gap-2 text-xs text-muted-foreground bg-muted px-3 py-1.5 rounded-full shrink-0">
      <span className={`w-2 h-2 rounded-full ${dotClass}`} />
      <span>{label}</span>
    </div>
  )
}
