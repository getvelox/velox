package webhook

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
)

// TestWireShape_WebhookEventsStream_SnakeCase pins the SSE frame schema
// the dashboard's EventSource consumer reads. Drift here (e.g. someone
// drops a json:"event_id" tag) silently breaks the live tail at runtime
// and the user sees a blank table — this test is the merge gate.
//
// The contract: each frame is a single JSON object with snake_case keys
// only. No PascalCase / camelCase leaks. last_attempt_at / replay_of_event_id
// are nullable and emit JSON null when unset (always-present idiom so the
// frontend reads `frame.replay_of_event_id` without optional chaining).
func TestWireShape_WebhookEventsStream_SnakeCase(t *testing.T) {
	store := newMemStore()
	svc := NewTestService(store, &mockHTTPClient{statusCode: 200})
	h := NewHandler(svc)

	// Seed an endpoint + an event so the snapshot path emits a frame.
	// Use livemode so the dashboard's default-live render path is
	// exercised (the contract is identical for test mode but the live
	// path is the one Track A's PR description anchors on).
	tenantID := "vlx_tenant_wireshape"
	ctx := auth.WithTenantID(context.Background(), tenantID)
	if _, err := svc.CreateEndpoint(ctx, tenantID, CreateEndpointInput{
		URL:    "http://localhost:9001/sink",
		Events: []string{"*"},
	}); err != nil {
		t.Fatalf("create endpoint: %v", err)
	}
	if err := svc.Dispatch(ctx, tenantID, "invoice.finalized", map[string]any{
		"customer_id": "vlx_cus_wireshape",
		"invoice_id":  "vlx_inv_42",
	}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	// Spin the SSE handler against a recorder. Use a context with a
	// 100ms deadline so the heartbeat-loop exits cleanly after the
	// snapshot frames flush — we only care about the bytes written
	// before the deadline fires.
	r := chi.NewRouter()
	r.Get("/v1/webhook_events/stream", h.streamEvents)

	streamCtx, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest("GET", "/v1/webhook_events/stream", nil).WithContext(streamCtx)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	// Verify SSE headers — these are the contract for any EventSource
	// client (browser or curl) and proxies route on Content-Type.
	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type: got %q, want text/event-stream", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control: got %q, want no-cache", got)
	}
	if got := rec.Header().Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering: got %q, want no", got)
	}

	// Parse the first data: line out of the SSE stream. The format is
	// "event: webhook_event\ndata: {<json>}\n\n" — split by "\ndata: "
	// and take the first frame's payload (everything up to the next
	// blank line).
	body := rec.Body.String()
	dataIdx := strings.Index(body, "data: ")
	if dataIdx == -1 {
		t.Fatalf("no data: line in SSE response, got: %q", body)
	}
	tail := body[dataIdx+len("data: "):]
	end := strings.Index(tail, "\n")
	if end == -1 {
		t.Fatalf("data: line not terminated: %q", tail)
	}
	frameJSON := tail[:end]

	var frame map[string]any
	if err := json.Unmarshal([]byte(frameJSON), &frame); err != nil {
		t.Fatalf("parse frame %q: %v", frameJSON, err)
	}

	// Required snake_case keys — the dashboard reads each one.
	required := []string{
		"event_id",
		"event_type",
		"customer_id",
		"status",
		"last_attempt_at",
		"created_at",
		"livemode",
		"replay_of_event_id",
	}
	for _, k := range required {
		if _, ok := frame[k]; !ok {
			t.Errorf("frame missing %q (keys=%v)", k, mapKeys(frame))
		}
	}

	// Forbidden PascalCase / camelCase leaks. If a struct tag is dropped
	// on StreamFrame, encoding/json falls back to the Go field name,
	// which is what we're guarding against.
	for _, k := range []string{
		"EventID", "EventType", "CustomerID", "Status",
		"LastAttemptAt", "CreatedAt", "Livemode", "ReplayOfEventID",
		"eventId", "eventType", "customerId", "lastAttemptAt", "replayOfEventId",
	} {
		if _, ok := frame[k]; ok {
			t.Errorf("frame leaked non-snake_case key %q", k)
		}
	}

	// Spot-check the values map back to the seeded event.
	if frame["event_type"] != "invoice.finalized" {
		t.Errorf("event_type: got %v, want invoice.finalized", frame["event_type"])
	}
	if frame["customer_id"] != "vlx_cus_wireshape" {
		t.Errorf("customer_id: got %v, want vlx_cus_wireshape", frame["customer_id"])
	}
	// Snapshot frames are minted with no last_attempt yet — the value
	// should be JSON null (present-but-nil), not the key omitted.
	if v, ok := frame["last_attempt_at"]; !ok {
		t.Errorf("last_attempt_at must be present (as null) on snapshot frames")
	} else if v != nil {
		// On synchronous-deliver test mode the dispatcher publishes a
		// follow-up frame within the same handler tick; if the parser
		// captured *that* frame instead, last_attempt_at is a string.
		// Either is acceptable as long as the key is present.
		if _, isString := v.(string); !isString {
			t.Errorf("last_attempt_at: got %T (%v), want null or RFC3339 string", v, v)
		}
	}
	// replay_of_event_id is nil for original events (not a clone) — must
	// be present-as-null so the frontend can read frame.replay_of_event_id
	// without an optional-chaining check.
	if v, ok := frame["replay_of_event_id"]; !ok {
		t.Errorf("replay_of_event_id must be present (as null) for original events")
	} else if v != nil {
		t.Errorf("replay_of_event_id should be null for original events, got %v", v)
	}

	// The heartbeat comment line ": heartbeat\n\n" may or may not have
	// fired before the deadline; we don't assert on it. The data: line
	// is what matters for the wire shape.
}

// TestWireShape_WebhookEventReplay pins the response shape for
// POST /v1/webhook_events/{id}/replay. The dashboard's Replay button
// reads {event_id, replay_of, status} — drift here breaks the toast
// notification ("Replayed event evt_X — clone evt_Y queued") and the
// optimistic-highlight path that the live tail uses to flash the new row.
func TestWireShape_WebhookEventReplay(t *testing.T) {
	store := newMemStore()
	svc := NewTestService(store, &mockHTTPClient{statusCode: 200})
	h := NewHandler(svc)

	tenantID := "vlx_tenant_replay"
	ctx := auth.WithTenantID(context.Background(), tenantID)
	if _, err := svc.CreateEndpoint(ctx, tenantID, CreateEndpointInput{
		URL:    "http://localhost:9002/sink",
		Events: []string{"*"},
	}); err != nil {
		t.Fatalf("create endpoint: %v", err)
	}
	if err := svc.Dispatch(ctx, tenantID, "invoice.finalized", map[string]any{
		"invoice_id": "vlx_inv_99",
	}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(store.events) != 1 {
		t.Fatalf("setup: want 1 event seeded, got %d", len(store.events))
	}
	originalID := store.events[0].ID

	// Hit the replay endpoint via the EventRoutes mount (mirrors the
	// production wire-up at /v1/webhook_events/{id}/replay).
	r := chi.NewRouter()
	r.Mount("/v1/webhook_events", h.EventRoutes())

	req := httptest.NewRequest("POST", "/v1/webhook_events/"+originalID+"/replay", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("parse body: %v; body=%s", err, rec.Body.String())
	}

	// Required snake_case keys for the replay envelope.
	for _, k := range []string{"event_id", "replay_of", "status"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("replay response missing %q (keys=%v)", k, mapKeys(raw))
		}
	}

	// Forbidden PascalCase / camelCase leaks.
	for _, k := range []string{
		"EventID", "ReplayOf", "Status",
		"eventId", "replayOf", "outboxRowId",
	} {
		if _, ok := raw[k]; ok {
			t.Errorf("replay response leaked non-snake_case key %q", k)
		}
	}

	// Value sanity:
	// - status must be the string "queued"
	// - replay_of must equal the original event id
	// - event_id must be a fresh, distinct id (the clone)
	if raw["status"] != "queued" {
		t.Errorf("status: got %v, want queued", raw["status"])
	}
	if raw["replay_of"] != originalID {
		t.Errorf("replay_of: got %v, want %s", raw["replay_of"], originalID)
	}
	cloneID, ok := raw["event_id"].(string)
	if !ok {
		t.Fatalf("event_id should marshal as string, got %T (%v)", raw["event_id"], raw["event_id"])
	}
	if cloneID == "" || cloneID == originalID {
		t.Errorf("event_id should be a fresh clone id (got %q vs original %q)", cloneID, originalID)
	}
}

// TestWireShape_WebhookEventDeliveries pins the deliveries-timeline
// envelope: {root_event_id, deliveries: [{...}]}. Each delivery row
// carries every field the dashboard's expandable-row component reads —
// dropping any of them breaks the timeline render at runtime.
func TestWireShape_WebhookEventDeliveries(t *testing.T) {
	store := newMemStore()
	svc := NewTestService(store, &mockHTTPClient{statusCode: 200})
	h := NewHandler(svc)

	tenantID := "vlx_tenant_deliveries"
	ctx := auth.WithTenantID(context.Background(), tenantID)
	if _, err := svc.CreateEndpoint(ctx, tenantID, CreateEndpointInput{
		URL:    "http://localhost:9003/sink",
		Events: []string{"*"},
	}); err != nil {
		t.Fatalf("create endpoint: %v", err)
	}
	if err := svc.Dispatch(ctx, tenantID, "invoice.finalized", map[string]any{
		"invoice_id":  "vlx_inv_77",
		"customer_id": "vlx_cus_77",
	}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(store.events) != 1 || len(store.deliveries) != 1 {
		t.Fatalf("setup: want 1 event + 1 delivery seeded, got events=%d deliveries=%d",
			len(store.events), len(store.deliveries))
	}
	eventID := store.events[0].ID

	// Drive the deliveries endpoint via the EventRoutes mount so the
	// chi URL param resolves correctly (the handler calls chi.URLParam).
	r := chi.NewRouter()
	r.Mount("/v1/webhook_events", h.EventRoutes())

	req := httptest.NewRequest("GET", "/v1/webhook_events/"+eventID+"/deliveries", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("parse body: %v; body=%s", err, rec.Body.String())
	}

	// Top-level envelope: {root_event_id, deliveries}.
	for _, k := range []string{"root_event_id", "deliveries"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("response missing %q (keys=%v)", k, mapKeys(raw))
		}
	}
	for _, k := range []string{"RootEventID", "Deliveries", "rootEventId"} {
		if _, ok := raw[k]; ok {
			t.Errorf("response leaked non-snake_case key %q", k)
		}
	}
	if raw["root_event_id"] != eventID {
		t.Errorf("root_event_id: got %v, want %s", raw["root_event_id"], eventID)
	}

	deliveries, ok := raw["deliveries"].([]any)
	if !ok {
		t.Fatalf("deliveries must be a JSON array, got %T", raw["deliveries"])
	}
	if len(deliveries) != 1 {
		t.Fatalf("deliveries count: got %d, want 1", len(deliveries))
	}
	row, ok := deliveries[0].(map[string]any)
	if !ok {
		t.Fatalf("delivery[0] must be a JSON object, got %T", deliveries[0])
	}

	// Per-row required keys — every column the dashboard renders.
	required := []string{
		"id",
		"event_id",
		"endpoint_id",
		"endpoint_url",
		"endpoint_description",
		"attempt_no",
		"status",
		"status_code",
		"response_body",
		"error",
		"request_payload_sha256",
		"attempted_at",
		"completed_at",
		"next_retry_at",
		"is_replay",
		"replay_event_id",
	}
	for _, k := range required {
		if _, ok := row[k]; !ok {
			t.Errorf("delivery row missing %q (keys=%v)", k, mapKeys(row))
		}
	}

	// Forbidden PascalCase / camelCase leaks.
	for _, k := range []string{
		"ID", "EventID", "EndpointID", "AttemptNo", "StatusCode",
		"ResponseBody", "RequestPayloadSHA256", "AttemptedAt",
		"CompletedAt", "NextRetryAt", "IsReplay", "ReplayEventID",
		"eventId", "endpointId", "attemptNo", "statusCode",
		"responseBody", "requestPayloadSha256", "attemptedAt",
		"completedAt", "nextRetryAt", "isReplay", "replayEventId",
	} {
		if _, ok := row[k]; ok {
			t.Errorf("delivery row leaked non-snake_case key %q", k)
		}
	}

	// Value spot-checks:
	// - attempt_no is a JSON number (1-indexed).
	// - request_payload_sha256 is a hex string (SHA-256 = 64 chars).
	// - is_replay is false for the original delivery.
	if attempt, ok := row["attempt_no"].(float64); !ok || attempt != 1 {
		t.Errorf("attempt_no: got %v (%T), want 1 (float64)", row["attempt_no"], row["attempt_no"])
	}
	if sha, ok := row["request_payload_sha256"].(string); !ok || len(sha) != 64 {
		t.Errorf("request_payload_sha256: got %v (%T), want 64-char hex string", row["request_payload_sha256"], row["request_payload_sha256"])
	}
	if v, ok := row["is_replay"].(bool); !ok || v {
		t.Errorf("is_replay: got %v (%T), want false for original delivery", row["is_replay"], row["is_replay"])
	}

	// Nullable timestamps are present-as-(null|string) so the frontend
	// doesn't optional-chain. completed_at is set on a synchronous-
	// success delivery so it'll be a string here; next_retry_at is null.
	if _, ok := row["completed_at"]; !ok {
		t.Errorf("completed_at must be present (as null or string)")
	}
	if v, ok := row["next_retry_at"]; !ok {
		t.Errorf("next_retry_at must be present (as null or string)")
	} else if v != nil {
		t.Errorf("next_retry_at should be null on a succeeded delivery, got %v", v)
	}
}

// mapKeys returns the keys of a JSON-decoded object as a slice for
// stable error-message rendering. Local to the test file since helpers
// in other test packages aren't importable.
func mapKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// _ ensures the auth + domain imports are exercised even if the wire-
// shape test surface evolves to use only one or the other.
var _ = auth.PermAPIKeyRead
var _ = domain.WebhookEvent{}
