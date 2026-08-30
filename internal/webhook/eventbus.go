package webhook

import (
	"sync"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// StreamFrame is the SSE-shaped projection of a webhook event. We use a
// flat snake_case struct (not the raw domain.WebhookEvent) because the
// dashboard contract is stable — adding a column to webhook_events
// shouldn't silently widen the wire shape.
//
// The frame carries the latest delivery rollup we know about at emit
// time:
//
//   - status        — the pub/sub-side aggregate ("dispatched" /
//     "pending" / "failed"). The dashboard renders this
//     directly; clicking the row fetches the per-attempt
//     timeline via /v1/webhook_events/{id}/deliveries.
//   - last_attempt_at — best-effort time of the most recent dispatch
//     attempt seen by the bus. NULL on the initial
//     "snapshot" frames sent at connect time when the
//     event hasn't been dispatched yet.
type StreamFrame struct {
	EventID         string     `json:"event_id"`
	EventType       string     `json:"event_type"`
	CustomerID      string     `json:"customer_id"`
	Status          string     `json:"status"`
	LastAttemptAt   *time.Time `json:"last_attempt_at"`
	CreatedAt       time.Time  `json:"created_at"`
	Livemode        bool       `json:"livemode"`
	ReplayOfEventID *string    `json:"replay_of_event_id"`
}

// FrameFromEvent builds a StreamFrame from a domain.WebhookEvent.
// status defaults to "pending" if not yet observed by the dispatcher.
// The frame inspects payload.customer_id (the convention all Velox
// internal Dispatchers honor) so the dashboard can group by customer
// without a JOIN.
func FrameFromEvent(e domain.WebhookEvent, status string, lastAttemptAt *time.Time) StreamFrame {
	customerID := ""
	if e.Payload != nil {
		// Most Velox event payloads carry the affected customer's ID
		// at top-level under "customer_id" — see internal/billing,
		// internal/subscription, internal/invoice. The few that don't
		// (some system/scheduled events with no customer
		// scope) drop through to "" and the dashboard renders "—".
		if v, ok := e.Payload["customer_id"].(string); ok {
			customerID = v
		}
	}
	if status == "" {
		status = "pending"
	}
	return StreamFrame{
		EventID:         e.ID,
		EventType:       e.EventType,
		CustomerID:      customerID,
		Status:          status,
		LastAttemptAt:   lastAttemptAt,
		CreatedAt:       e.CreatedAt,
		Livemode:        e.Livemode,
		ReplayOfEventID: e.ReplayOfEventID,
	}
}

// EventBus is the in-memory pub/sub the SSE handler uses to live-tail
// new events as they're dispatched. We deliberately do NOT poll the DB
// on a tick — that approach burns the partial-index cache on every
// poll and lags ~1s behind dispatch. Instead, the Service's Dispatch
// path (and the OutboxDispatcher's success callback) calls
// EventBus.Publish synchronously; subscribers receive frames within
// goroutine-scheduling latency.
//
// Slow subscribers do NOT block the publisher — Publish does a
// non-blocking send and drops frames for any subscriber whose buffer is
// full. Dropped frames are not retried; the dashboard's snapshot-at-
// connect compensates by re-fetching recent events on reconnect, so a
// disconnected client picks back up cleanly.
//
// Per-tenant fan-out keeps the bus a no-op for any tenant with zero
// dashboards open. We index subscribers by tenant_id at registration so
// Publish doesn't iterate every tenant's subscriber list.
type EventBus struct {
	mu          sync.RWMutex
	subscribers map[string]map[*subscription]struct{} // tenant_id → set
	// closed is set by CloseAll on shutdown: every subscriber channel is
	// closed (the SSE loop exits on a closed frames channel, so
	// http.Server.Shutdown's idle-connection wait completes in ms instead
	// of the full 30s drain) and a Subscribe that races shutdown gets an
	// already-closed channel instead of re-arming the wait.
	closed bool
}

// NewEventBus returns an empty in-memory bus. Single instance per
// process is sufficient — the API server is the only producer
// (replicas use the leader-elected outbox dispatcher; replicas with no
// HTTP traffic produce no live events to fan out).
func NewEventBus() *EventBus {
	return &EventBus{subscribers: make(map[string]map[*subscription]struct{})}
}

type subscription struct {
	ch chan StreamFrame
}

// Subscribe registers a new subscriber for the given tenant. Returns
// the receive channel and an unsubscribe func the caller MUST call
// (typically deferred at request-handler scope) so the goroutine
// scheduler can collect the closed connection.
//
// The buffer size of 32 is empirically picked: at ~5 frames/s steady-
// state per tenant (a busy production workload), the buffer absorbs
// ~6s of producer bursts before drops. A subscriber that can't drain
// at that rate is almost certainly a dead connection — better to drop
// than block the dispatcher.
func (b *EventBus) Subscribe(tenantID string) (<-chan StreamFrame, func()) {
	sub := &subscription{ch: make(chan StreamFrame, 32)}

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		close(sub.ch)
		return sub.ch, func() {}
	}
	if b.subscribers[tenantID] == nil {
		b.subscribers[tenantID] = make(map[*subscription]struct{})
	}
	b.subscribers[tenantID][sub] = struct{}{}
	b.mu.Unlock()

	cancel := func() {
		b.mu.Lock()
		if set, ok := b.subscribers[tenantID]; ok {
			if _, exists := set[sub]; exists {
				delete(set, sub)
				close(sub.ch)
			}
			if len(set) == 0 {
				delete(b.subscribers, tenantID)
			}
		}
		b.mu.Unlock()
	}
	return sub.ch, cancel
}

// Publish fans a frame out to every subscriber for the frame's tenant.
// Non-blocking by construction: a slow subscriber drops the frame
// silently rather than back-pressuring the dispatcher. The bus is hot-
// path adjacent (called from Service.Dispatch) so we keep the lock
// hold tight and read-only.
func (b *EventBus) Publish(tenantID string, frame StreamFrame) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	set, ok := b.subscribers[tenantID]
	if !ok {
		return
	}
	for sub := range set {
		select {
		case sub.ch <- frame:
		default:
			// Buffer full — frame dropped. Subscriber's snapshot-at-
			// reconnect will pick up the missing event when their UI
			// re-establishes the EventSource connection.
		}
	}
}

// CloseAll closes every subscriber channel and marks the bus closed.
// Wired via http.Server.RegisterOnShutdown in cmd/velox/main.go: Shutdown
// never cancels request contexts and waits for connections to go idle,
// and the SSE handler (sse.go) only returns when its frames channel
// closes — without this, every deploy with a dashboard tab on Webhook
// Events held the HTTP drain open for its full 30s bound. EventSource
// clients reconnect to a live replica on their own. Idempotent.
func (b *EventBus) CloseAll() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for tenantID, set := range b.subscribers {
		for sub := range set {
			close(sub.ch)
		}
		delete(b.subscribers, tenantID)
	}
}
