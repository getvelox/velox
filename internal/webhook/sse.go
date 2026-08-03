package webhook

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/sagarsuperuser/velox/internal/auth"
)

// streamEvents serves the live-tail SSE feed for a tenant's webhook
// events. The handler:
//
//  1. Sends a snapshot of recent events (ListEvents, last 50) so a
//     freshly-opened dashboard renders the current state immediately,
//     not a blank table.
//  2. Subscribes to the EventBus so any new Dispatch / Replay /
//     deliver-result publishes a frame to this connection.
//  3. Sends a heartbeat comment every 15s — proxies (nginx, ALB) idle
//     connections at 60s by default; a periodic ":\n\n" line keeps the
//     pipe warm without polluting the data channel.
//
// Tenant scoping is enforced two ways: ListEvents runs under the
// caller's tenant tx (RLS), and the EventBus subscriber is keyed by
// tenant_id so cross-tenant frames never reach this connection.
//
// We do NOT poll the DB on a tick — the EventBus is in-memory and
// source-of-truth-adjacent (the Service publishes synchronously from
// Dispatch). That means a single replica can serve the stream; the
// outbox dispatcher's success path doesn't currently publish frames
// (it ends up calling Service.Dispatch which does), so multi-replica
// SSE works as long as the tenant's HTTP traffic and dispatcher land
// on the same replica. v1 sizing is single-replica per CLAUDE.md, so
// this is correct for now; a v2 multi-replica deployment would route
// SSE through Postgres LISTEN/NOTIFY (one-line swap inside EventBus).
const (
	sseHeartbeatInterval = 15 * time.Second
	sseSnapshotLimit     = 50
)

// streamEvents is the chi handler. Mounted at GET /webhook_events/stream
// — registered BEFORE /webhook_events/{id} so chi's specificity-by-
// registration-order rule picks the literal route.
func (h *Handler) streamEvents(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	if tenantID == "" {
		http.Error(w, "tenant required", http.StatusUnauthorized)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	// Disable the server's WriteTimeout for this connection. The stream is
	// long-lived (clients tail webhook events for hours); the zero deadline
	// removes the hard cap that would otherwise drop it after WriteTimeout.
	// The 15s heartbeat below keeps idle proxies from closing it. Best-effort:
	// SetWriteDeadline only no-ops if the writer can't support it.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})

	// SSE headers. Cache-Control:no-cache prevents proxies from
	// buffering or replaying stale chunks; X-Accel-Buffering disables
	// nginx's response buffering specifically.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// Snapshot first: the dashboard renders these so a freshly-opened
	// page shows recent activity instead of a blank table while
	// waiting for the next live event. Newest-first is the SSE
	// convention; the frontend prepends new live frames on top so the
	// ordering is consistent.
	snapshot, err := h.svc.ListEvents(r.Context(), tenantID, sseSnapshotLimit)
	if err != nil {
		slog.ErrorContext(r.Context(), "sse snapshot failed", "error", err)
	}
	// One roll-up query for the whole snapshot: each event's status is
	// derived from its actual deliveries, not guessed from its age. An
	// event with no deliveries (no endpoint matched) gets "no_endpoints" —
	// the dashboard renders unknown statuses neutrally by design.
	ids := make([]string, 0, len(snapshot))
	for _, e := range snapshot {
		ids = append(ids, e.ID)
	}
	statuses, serr := h.svc.EventDeliveryStatuses(r.Context(), tenantID, ids)
	if serr != nil {
		slog.ErrorContext(r.Context(), "sse snapshot status roll-up failed", "error", serr)
	}
	for _, e := range snapshot {
		status, ok := statuses[e.ID]
		if !ok {
			status = "no_endpoints"
		}
		writeFrame(w, FrameFromEvent(e, status, nil))
	}
	flusher.Flush()

	// Subscribe to the live bus AFTER the snapshot completes so we
	// don't double-emit any event that arrived during snapshot
	// streaming. There's still a sub-millisecond window of "live event
	// queued + snapshot SELECT pulled it"; the dashboard de-dupes by
	// event_id so a stray double-frame just collapses into the same
	// row.
	frames, cancel := h.svc.EventBus().Subscribe(tenantID)
	defer cancel()

	heartbeat := time.NewTicker(sseHeartbeatInterval)
	defer heartbeat.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			// SSE comment lines (": ...\n\n") keep idle proxies from
			// dropping the connection. The data channel stays clean.
			if _, err := w.Write([]byte(": heartbeat\n\n")); err != nil {
				return
			}
			flusher.Flush()
		case frame, ok := <-frames:
			if !ok {
				return
			}
			writeFrame(w, frame)
			flusher.Flush()
		}
	}
}

// writeFrame serializes a single SSE event frame. We use the named
// `event: webhook_event` form so the EventSource consumer can attach
// addEventListener('webhook_event', ...) and ignore heartbeats /
// future event types. Errors writing to a closed connection are
// swallowed — the request context will fire next tick and the handler
// loop exits.
func writeFrame(w http.ResponseWriter, frame StreamFrame) {
	blob, err := json.Marshal(frame)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "event: webhook_event\ndata: %s\n\n", blob)
}

// hashPayload computes the canonical SHA-256 of an event payload's
// JSON serialization. The deliveries-list endpoint surfaces this so
// the dashboard's diff view can flag "payload identical between
// attempts" (the common case — Stripe-style replays don't mutate the
// payload). Returns empty string on marshal failure rather than
// crashing the handler.
func hashPayload(payload map[string]any) string {
	if payload == nil {
		return ""
	}
	blob, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(blob)
	return hex.EncodeToString(sum[:])
}

// truncateBody is the wire-shape contract guard: response bodies are
// capped at 4KB before hitting the dashboard so a misbehaving receiver
// returning a megabyte HTML page doesn't blow out the deliveries-list
// payload size.
func truncateBody(s string) string {
	const max = 4 * 1024
	if len(s) <= max {
		return s
	}
	return s[:max]
}

// _ ensures context import is used when the file is built standalone.
var _ = context.Background
