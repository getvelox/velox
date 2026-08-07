package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// ---------------------------------------------------------------------------
// Mocks
// ---------------------------------------------------------------------------

type memStore struct {
	endpoints  map[string]domain.WebhookEndpoint
	events     []domain.WebhookEvent
	deliveries []domain.WebhookDelivery
	// deliveryLivemodes records postgres.Livemode(ctx) for each
	// CreateDelivery call so tests can assert the delivery write landed
	// on the right RLS mode partition (H5: the Replay goroutine path
	// used a bare ctx and wrote test-mode replays to the live partition).
	deliveryLivemodes []bool
	// deliverySignal, when non-nil, receives the captured livemode after
	// each CreateDelivery append — lets a test deterministically wait on
	// the async (goroutine) delivery path instead of sleeping.
	deliverySignal chan bool
	// dmu guards the delivery slices so the async (goroutine) delivery
	// path can be exercised under -race. Only the delivery-touching
	// methods take it; the rest of the store is used single-threaded.
	dmu sync.Mutex
}

func newMemStore() *memStore {
	return &memStore{endpoints: make(map[string]domain.WebhookEndpoint)}
}

func (m *memStore) CreateEndpoint(ctx context.Context, tenantID string, ep domain.WebhookEndpoint) (domain.WebhookEndpoint, error) {
	ep.ID = fmt.Sprintf("vlx_whe_%d", len(m.endpoints)+1)
	ep.TenantID = tenantID
	ep.Livemode = postgres.Livemode(ctx)
	ep.CreatedAt = time.Now().UTC()
	ep.UpdatedAt = ep.CreatedAt
	ep.SecretLast4 = lastFour(ep.Secret)
	m.endpoints[ep.ID] = ep
	return ep, nil
}

func (m *memStore) CreateEndpointTx(ctx context.Context, _ *sql.Tx, tenantID string, ep domain.WebhookEndpoint) (domain.WebhookEndpoint, error) {
	return m.CreateEndpoint(ctx, tenantID, ep)
}

func (m *memStore) GetEndpoint(_ context.Context, tenantID, id string) (domain.WebhookEndpoint, error) {
	ep, ok := m.endpoints[id]
	if !ok || ep.TenantID != tenantID {
		return domain.WebhookEndpoint{}, errs.ErrNotFound
	}
	return ep, nil
}

func (m *memStore) UpdateEndpoint(_ context.Context, tenantID string, ep domain.WebhookEndpoint) (domain.WebhookEndpoint, error) {
	existing, ok := m.endpoints[ep.ID]
	if !ok || existing.TenantID != tenantID {
		return domain.WebhookEndpoint{}, errs.ErrNotFound
	}
	existing.URL = ep.URL
	existing.Description = ep.Description
	existing.Events = ep.Events
	existing.Active = ep.Active
	m.endpoints[ep.ID] = existing
	return existing, nil
}

// ListEndpoints returns ALL endpoints including inactive — matching the real
// store since the FLOW W0 fix (dispatch filters ep.Active in its own match
// loop; the list must show inactive rows or they're unreachable from the UI).
func (m *memStore) ListEndpoints(ctx context.Context, tenantID string) ([]domain.WebhookEndpoint, error) {
	live := postgres.Livemode(ctx)
	var result []domain.WebhookEndpoint
	for _, ep := range m.endpoints {
		if ep.TenantID == tenantID && ep.Livemode == live {
			result = append(result, ep)
		}
	}
	return result, nil
}

func (m *memStore) DeleteEndpoint(_ context.Context, tenantID, id string) error {
	ep, ok := m.endpoints[id]
	if !ok || ep.TenantID != tenantID {
		return errs.ErrNotFound
	}
	delete(m.endpoints, id)
	return nil
}

func (m *memStore) RotateEndpointSecret(_ context.Context, tenantID, id, newSecret string, gracePeriod time.Duration) (domain.WebhookEndpoint, error) {
	ep, ok := m.endpoints[id]
	if !ok || ep.TenantID != tenantID {
		return domain.WebhookEndpoint{}, errs.ErrNotFound
	}
	if gracePeriod > 0 {
		ep.SecondarySecret = ep.Secret
		ep.SecondarySecretLast4 = ep.SecretLast4
		exp := time.Now().UTC().Add(gracePeriod)
		ep.SecondarySecretExpiresAt = &exp
	} else {
		ep.SecondarySecret = ""
		ep.SecondarySecretLast4 = ""
		ep.SecondarySecretExpiresAt = nil
	}
	ep.Secret = newSecret
	ep.SecretLast4 = lastFour(newSecret)
	ep.UpdatedAt = time.Now().UTC()
	m.endpoints[id] = ep
	return ep, nil
}

func (m *memStore) CreateEvent(ctx context.Context, tenantID string, event domain.WebhookEvent) (domain.WebhookEvent, error) {
	event.ID = fmt.Sprintf("vlx_whevt_%d", len(m.events)+1)
	event.TenantID = tenantID
	event.Livemode = postgres.Livemode(ctx)
	event.CreatedAt = time.Now().UTC()
	m.events = append(m.events, event)
	return event, nil
}

// CreateEventWithDeliveries mirrors the store's one-tx create: event +
// born-leased delivery rows (+ outbox mark, which the mem fake ignores —
// outbox rows live in a different store).
func (m *memStore) CreateEventWithDeliveries(ctx context.Context, tenantID string, event domain.WebhookEvent, endpointIDs []string, birthLease time.Duration, _ string) (domain.WebhookEvent, []domain.WebhookDelivery, error) {
	ev, err := m.CreateEvent(ctx, tenantID, event)
	if err != nil {
		return domain.WebhookEvent{}, nil, err
	}
	ds, err := m.CreateDeliveriesForEvent(ctx, tenantID, ev.ID, endpointIDs, birthLease)
	return ev, ds, err
}

func (m *memStore) CreateDeliveriesForEvent(ctx context.Context, tenantID, eventID string, endpointIDs []string, birthLease time.Duration) ([]domain.WebhookDelivery, error) {
	lease := time.Now().UTC().Add(birthLease)
	out := make([]domain.WebhookDelivery, 0, len(endpointIDs))
	for _, epID := range endpointIDs {
		d, err := m.CreateDelivery(ctx, tenantID, domain.WebhookDelivery{
			WebhookEndpointID: epID,
			WebhookEventID:    eventID,
			Status:            domain.DeliveryPending,
			NextRetryAt:       &lease,
		})
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

func (m *memStore) ListEvents(_ context.Context, tenantID string, limit int) ([]domain.WebhookEvent, error) {
	// Mirror the postgres store: newest-first (created_at DESC), default 50,
	// HARD-clamped to 100 regardless of the requested limit. The real store
	// caps the result set, so an older event ages out of the window — the
	// retry path must NOT rely on this list to resolve its event.
	if limit <= 0 {
		limit = 50
	} else if limit > 100 {
		limit = 100
	}
	var all []domain.WebhookEvent
	for _, e := range m.events {
		if e.TenantID == tenantID {
			all = append(all, e)
		}
	}
	// Events are appended in creation order, so reverse for newest-first.
	var result []domain.WebhookEvent
	for i := len(all) - 1; i >= 0; i-- {
		result = append(result, all[i])
		if len(result) >= limit {
			break
		}
	}
	return result, nil
}

func (m *memStore) GetEvent(_ context.Context, tenantID, id string) (domain.WebhookEvent, error) {
	for _, e := range m.events {
		if e.ID == id && e.TenantID == tenantID {
			return e, nil
		}
	}
	return domain.WebhookEvent{}, errs.ErrNotFound
}

func (m *memStore) CreateReplayEvent(ctx context.Context, tenantID, originalEventID string) (domain.WebhookEvent, error) {
	for _, e := range m.events {
		if e.ID == originalEventID && e.TenantID == tenantID {
			rootID := originalEventID
			if e.ReplayOfEventID != nil && *e.ReplayOfEventID != "" {
				rootID = *e.ReplayOfEventID
			}
			clone := domain.WebhookEvent{
				EventType:       e.EventType,
				Payload:         e.Payload,
				ReplayOfEventID: &rootID,
			}
			created, err := m.CreateEvent(ctx, tenantID, clone)
			if err != nil {
				return domain.WebhookEvent{}, err
			}
			created.ReplayOfEventID = &rootID
			// Persist the replay pointer on the in-memory copy so
			// downstream filters (ListDeliveries replay-tree walk) see
			// it on subsequent reads.
			for i := range m.events {
				if m.events[i].ID == created.ID {
					m.events[i].ReplayOfEventID = &rootID
				}
			}
			return created, nil
		}
	}
	return domain.WebhookEvent{}, errs.ErrNotFound
}

func (m *memStore) CreateDelivery(ctx context.Context, tenantID string, d domain.WebhookDelivery) (domain.WebhookDelivery, error) {
	m.dmu.Lock()
	d.ID = fmt.Sprintf("vlx_whd_%d", len(m.deliveries)+1)
	d.TenantID = tenantID
	d.CreatedAt = time.Now().UTC()
	lm := postgres.Livemode(ctx)
	// Mirror the real store: the 0021 trigger stamps livemode from the
	// session GUC and the INSERT hydrates it via RETURNING — a fake row
	// frozen at the zero value would trip (or mask) livemode guards the
	// real store can't.
	d.Livemode = lm
	m.deliveries = append(m.deliveries, d)
	m.deliveryLivemodes = append(m.deliveryLivemodes, lm)
	signal := m.deliverySignal
	m.dmu.Unlock()
	if signal != nil {
		signal <- lm
	}
	return d, nil
}

func (m *memStore) UpdateDelivery(_ context.Context, _ string, d domain.WebhookDelivery) (domain.WebhookDelivery, error) {
	m.dmu.Lock()
	defer m.dmu.Unlock()
	for i, existing := range m.deliveries {
		if existing.ID == d.ID {
			m.deliveries[i] = d
			return d, nil
		}
	}
	return d, nil
}

func (m *memStore) ListDeliveries(_ context.Context, _, eventID string) ([]domain.WebhookDelivery, error) {
	m.dmu.Lock()
	defer m.dmu.Unlock()
	// Build the replay-tree set: the eventID itself plus every event
	// whose replay_of points back at it. Mirrors the postgres store's
	// JOIN-based walk so behavior parity holds in unit tests.
	tree := map[string]struct{}{eventID: {}}
	for _, e := range m.events {
		if e.ReplayOfEventID != nil && *e.ReplayOfEventID == eventID {
			tree[e.ID] = struct{}{}
		}
	}
	var result []domain.WebhookDelivery
	for _, d := range m.deliveries {
		if _, ok := tree[d.WebhookEventID]; ok {
			result = append(result, d)
		}
	}
	return result, nil
}

func (m *memStore) GetDelivery(_ context.Context, tenantID, id string) (domain.WebhookDelivery, error) {
	m.dmu.Lock()
	defer m.dmu.Unlock()
	for _, d := range m.deliveries {
		if d.ID == id && d.TenantID == tenantID {
			return d, nil
		}
	}
	return domain.WebhookDelivery{}, errs.ErrNotFound
}

// ListDeliveriesByEndpoint mirrors the real store's drill-down query:
// one endpoint's rows newest-first, each hydrated with its event's type
// and replay pivot (the postgres store does this via JOIN — a fake that
// skipped hydration would let the handler's is_replay mapping go
// untested).
func (m *memStore) ListDeliveriesByEndpoint(_ context.Context, tenantID, endpointID string, limit int) ([]domain.WebhookDelivery, error) {
	m.dmu.Lock()
	defer m.dmu.Unlock()
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	evByID := make(map[string]domain.WebhookEvent, len(m.events))
	for _, e := range m.events {
		evByID[e.ID] = e
	}
	var result []domain.WebhookDelivery
	// m.deliveries appends in creation order; walk backwards for DESC.
	for i := len(m.deliveries) - 1; i >= 0 && len(result) < limit; i-- {
		d := m.deliveries[i]
		if d.WebhookEndpointID != endpointID || d.TenantID != tenantID {
			continue
		}
		if e, ok := evByID[d.WebhookEventID]; ok {
			d.EventType = e.EventType
			if e.ReplayOfEventID != nil {
				d.EventReplayOfID = *e.ReplayOfEventID
			}
		}
		result = append(result, d)
	}
	return result, nil
}

// EventDeliveryStatuses mirrors the real store's roll-up (any pending →
// pending, else any failed → failed, else delivered; absent when no
// deliveries) — a drifted fake here would let the SSE snapshot assert
// against a double that agrees with itself.
func (m *memStore) EventDeliveryStatuses(_ context.Context, tenantID string, eventIDs []string) (map[string]string, error) {
	want := make(map[string]struct{}, len(eventIDs))
	for _, id := range eventIDs {
		want[id] = struct{}{}
	}
	pending := map[string]bool{}
	failed := map[string]bool{}
	seen := map[string]bool{}
	for _, d := range m.deliveries {
		if d.TenantID != tenantID {
			continue
		}
		if _, ok := want[d.WebhookEventID]; !ok {
			continue
		}
		seen[d.WebhookEventID] = true
		switch d.Status {
		case "pending":
			pending[d.WebhookEventID] = true
		case "failed":
			failed[d.WebhookEventID] = true
		}
	}
	out := map[string]string{}
	for id := range seen {
		switch {
		case pending[id]:
			out[id] = "pending"
		case failed[id]:
			out[id] = "failed"
		default:
			out[id] = "delivered"
		}
	}
	return out, nil
}

func (m *memStore) GetEndpointStats(_ context.Context, tenantID string) ([]EndpointStats, error) {
	counts := make(map[string]*EndpointStats)
	for _, d := range m.deliveries {
		if d.TenantID != tenantID {
			continue
		}
		s, ok := counts[d.WebhookEndpointID]
		if !ok {
			s = &EndpointStats{EndpointID: d.WebhookEndpointID}
			counts[d.WebhookEndpointID] = s
		}
		s.TotalDeliveries++
		if d.Status == domain.DeliverySucceeded { //nolint:staticcheck
			s.Succeeded++
		} else if d.Status == domain.DeliveryFailed {
			s.Failed++
		}
	}
	var result []EndpointStats
	for _, s := range counts {
		s.SuccessRate = endpointSuccessRate(s.Succeeded, s.Failed)
		result = append(result, *s)
	}
	return result, nil
}

func (m *memStore) ListPendingDeliveries(_ context.Context, limit int) ([]domain.WebhookDelivery, error) {
	m.dmu.Lock()
	defer m.dmu.Unlock()
	var result []domain.WebhookDelivery
	now := time.Now()
	for _, d := range m.deliveries {
		if d.Status == domain.DeliveryPending && d.NextRetryAt != nil && !d.NextRetryAt.After(now) {
			result = append(result, d)
			if len(result) >= limit {
				break
			}
		}
	}
	return result, nil
}

// mockHTTPClient captures requests and returns configurable responses.
//
// The webhook service delivers ASYNCHRONOUSLY (go s.deliver(...)), so a test
// that fires two overlapping deliveries — e.g. TestReplay_PinsLivemodeOnDelivery
// dispatches an event AND replays it — has two goroutines calling Do at once.
// mu guards the captured fields so those concurrent writes don't data-race
// under `-race` (the same reason the neighbouring memStore carries `dmu`).
// statusCode is set once at construction and never mutated, so Do reads it
// without the lock; lastRequest/lastBody are read back only by synchronous-
// delivery (NewTestService) tests, where program order already places the read
// after the write.
type mockHTTPClient struct {
	mu          sync.Mutex
	lastRequest *http.Request
	lastBody    []byte
	statusCode  int
}

func (m *mockHTTPClient) Do(req *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(req.Body)
	m.mu.Lock()
	m.lastRequest = req
	m.lastBody = body
	m.mu.Unlock()
	return &http.Response{
		StatusCode: m.statusCode,
		Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
	}, nil
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestEndpointSuccessRate guards that success rate is computed over COMPLETED
// deliveries only — pending deliveries (never passed in) don't depress it.
func TestEndpointSuccessRate(t *testing.T) {
	cases := []struct {
		name              string
		succeeded, failed int
		want              float64
	}{
		{"10 ok, 0 failed (5 pending excluded) → 100%", 10, 0, 100},
		{"5 ok, 5 failed → 50%", 5, 5, 50},
		{"0 ok, 4 failed → 0%", 0, 4, 0},
		{"nothing completed yet → 0%", 0, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := endpointSuccessRate(tc.succeeded, tc.failed); got != tc.want {
				t.Errorf("endpointSuccessRate(%d, %d) = %g, want %g", tc.succeeded, tc.failed, got, tc.want)
			}
		})
	}
}

func TestCreateEndpoint(t *testing.T) {
	svc := NewService(newMemStore(), nil)
	ctx := context.Background()

	t.Run("valid localhost", func(t *testing.T) {
		result, err := svc.CreateEndpoint(ctx, "t1", CreateEndpointInput{
			URL:    "http://localhost:8080/webhooks",
			Events: []string{"invoice.finalized", "payment.succeeded"},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.HasPrefix(result.Secret, "whsec_") {
			t.Errorf("secret should start with whsec_, got %q", result.Secret[:10])
		}
		if result.Endpoint.URL != "http://localhost:8080/webhooks" {
			t.Errorf("url: got %q", result.Endpoint.URL)
		}
		if len(result.Endpoint.Events) != 2 {
			t.Errorf("events: got %d, want 2", len(result.Endpoint.Events))
		}
		if result.Endpoint.SecretLast4 != result.Secret[len(result.Secret)-4:] {
			t.Errorf("secret_last4: got %q, want %q (last 4 of %q)",
				result.Endpoint.SecretLast4, result.Secret[len(result.Secret)-4:], result.Secret)
		}
	})

	t.Run("private IP blocked", func(t *testing.T) {
		_, err := svc.CreateEndpoint(ctx, "t1", CreateEndpointInput{URL: "https://10.0.0.1/hook"})
		if err == nil {
			t.Fatal("expected error for private IP URL")
		}
		if !strings.Contains(err.Error(), "private/internal IP") {
			t.Errorf("expected private IP error, got: %v", err)
		}
	})

	t.Run("missing url", func(t *testing.T) {
		_, err := svc.CreateEndpoint(ctx, "t1", CreateEndpointInput{})
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("non-https rejected", func(t *testing.T) {
		_, err := svc.CreateEndpoint(ctx, "t1", CreateEndpointInput{URL: "http://api.acme.com/webhooks"})
		if err == nil {
			t.Fatal("expected error for non-HTTPS URL")
		}
	})

	t.Run("localhost allowed", func(t *testing.T) {
		_, err := svc.CreateEndpoint(ctx, "t1", CreateEndpointInput{URL: "http://localhost:3000/webhooks"})
		if err != nil {
			t.Fatalf("localhost should be allowed: %v", err)
		}
	})

	t.Run("default wildcard events", func(t *testing.T) {
		result, _ := svc.CreateEndpoint(ctx, "t1", CreateEndpointInput{URL: "http://localhost:8080/hook"})
		if len(result.Endpoint.Events) != 1 || result.Endpoint.Events[0] != "*" {
			t.Errorf("default events should be [*], got %v", result.Endpoint.Events)
		}
	})
}

func TestDispatch(t *testing.T) {
	store := newMemStore()
	httpClient := &mockHTTPClient{statusCode: 200}
	svc := NewTestService(store, httpClient)
	ctx := context.Background()

	// Register an endpoint (localhost to bypass SSRF DNS check in tests)
	result, _ := svc.CreateEndpoint(ctx, "t1", CreateEndpointInput{
		URL:    "http://localhost:9999/webhooks",
		Events: []string{"invoice.finalized"},
	})

	// Dispatch an event
	err := svc.Dispatch(ctx, "t1", "invoice.finalized", map[string]any{
		"invoice_id": "inv_123",
		"total":      19900,
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	// Give async delivery goroutine time to complete
	// Delivery is synchronous in test mode

	// Verify event was created
	if len(store.events) != 1 {
		t.Fatalf("events: got %d, want 1", len(store.events))
	}
	if store.events[0].EventType != "invoice.finalized" {
		t.Errorf("event_type: got %q", store.events[0].EventType)
	}

	// Verify delivery was made
	if len(store.deliveries) != 1 {
		t.Fatalf("deliveries: got %d, want 1", len(store.deliveries))
	}
	if store.deliveries[0].Status != domain.DeliverySucceeded {
		t.Errorf("delivery status: got %q, want succeeded", store.deliveries[0].Status)
	}

	// Verify HMAC signature was sent
	if httpClient.lastRequest != nil {
		sig := httpClient.lastRequest.Header.Get("Velox-Signature")
		if !strings.Contains(sig, "t=") || !strings.Contains(sig, "v1=") {
			t.Errorf("missing signature header, got %q", sig)
		}

		// Verify signature is valid
		parts := strings.Split(sig, ",")
		var ts, v1 string
		for _, p := range parts {
			kv := strings.SplitN(p, "=", 2)
			if kv[0] == "t" {
				ts = kv[1]
			}
			if kv[0] == "v1" {
				v1 = kv[1]
			}
		}

		mac := hmac.New(sha256.New, []byte(result.Secret))
		mac.Write([]byte(ts + "." + string(httpClient.lastBody)))
		expected := hex.EncodeToString(mac.Sum(nil))
		if v1 != expected {
			t.Error("HMAC signature mismatch")
		}
	}

	// Verify Velox-Event-Type header
	if httpClient.lastRequest != nil {
		et := httpClient.lastRequest.Header.Get("Velox-Event-Type")
		if et != "invoice.finalized" {
			t.Errorf("Velox-Event-Type: got %q", et)
		}
	}

	// Verify payload structure
	if httpClient.lastBody != nil {
		var payload map[string]any
		_ = json.Unmarshal(httpClient.lastBody, &payload)
		if payload["event_type"] != "invoice.finalized" {
			t.Errorf("payload event_type: got %v", payload["event_type"])
		}
		data, _ := payload["data"].(map[string]any)
		if data["invoice_id"] != "inv_123" {
			t.Errorf("payload data.invoice_id: got %v", data["invoice_id"])
		}
	}
}

// TestInactiveEndpoint_ListedButNeverDispatched pins both halves of the FLOW
// W0 fix: an inactive endpoint MUST appear in the operator list (it was
// hidden by a store-level `WHERE active` — a deactivated or recipe-born
// endpoint became unreachable from the dashboard, with the FE's "paused"
// badge as dead code) and MUST NOT receive deliveries (dispatch's own
// ep.Active check is the filter that matters).
func TestInactiveEndpoint_ListedButNeverDispatched(t *testing.T) {
	store := newMemStore()
	svc := NewTestService(store, &mockHTTPClient{statusCode: 200})
	ctx := context.Background()

	if _, err := svc.CreateEndpoint(ctx, "t1", CreateEndpointInput{
		URL: "http://localhost:9901/hook", Events: []string{"*"},
	}); err != nil {
		t.Fatalf("create active endpoint: %v", err)
	}
	res, err := svc.CreateEndpoint(ctx, "t1", CreateEndpointInput{
		URL: "http://localhost:9902/hook", Events: []string{"*"},
	})
	if err != nil {
		t.Fatalf("create endpoint to deactivate: %v", err)
	}
	inactive := false
	if _, err := svc.UpdateEndpoint(ctx, "t1", res.Endpoint.ID, UpdateEndpointInput{Active: &inactive}); err != nil {
		t.Fatalf("deactivate: %v", err)
	}

	eps, err := svc.ListEndpoints(ctx, "t1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(eps) != 2 {
		t.Fatalf("list returned %d endpoints, want 2 — the inactive one must be visible or it is unreachable from the dashboard", len(eps))
	}

	if err := svc.Dispatch(ctx, "t1", "invoice.paid", map[string]any{"invoice_id": "inv_1"}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	for _, d := range store.deliveries {
		if d.WebhookEndpointID == res.Endpoint.ID {
			t.Fatalf("a delivery was created for the INACTIVE endpoint — listing it must not mean dispatching to it")
		}
	}
	if len(store.deliveries) == 0 {
		t.Fatal("no deliveries at all — the active endpoint should have received the event (control)")
	}

	// Delete is a DIFFERENT intent than pause (migration 0168): a deleted
	// endpoint leaves the list entirely instead of surfacing as "paused".
	if err := svc.DeleteEndpoint(ctx, "t1", res.Endpoint.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	eps, err = svc.ListEndpoints(ctx, "t1")
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	if len(eps) != 1 {
		t.Fatalf("list returned %d endpoints after delete, want 1 — deleted must not resurface as paused", len(eps))
	}
}

func TestDispatch_NonMatchingEvent(t *testing.T) {
	store := newMemStore()
	httpClient := &mockHTTPClient{statusCode: 200}
	svc := NewTestService(store, httpClient)
	ctx := context.Background()

	// Register endpoint for invoice events only
	_, _ = svc.CreateEndpoint(ctx, "t1", CreateEndpointInput{
		URL:    "http://localhost:9999/hook",
		Events: []string{"invoice.*"},
	})

	// Dispatch a payment event — should NOT trigger delivery
	_ = svc.Dispatch(ctx, "t1", "payment.succeeded", map[string]any{})

	// Delivery is synchronous in test mode

	if len(store.deliveries) != 0 {
		t.Errorf("no delivery expected for non-matching event, got %d", len(store.deliveries))
	}
}

func TestDispatch_WildcardEndpoint(t *testing.T) {
	store := newMemStore()
	httpClient := &mockHTTPClient{statusCode: 200}
	svc := NewTestService(store, httpClient)
	ctx := context.Background()

	// Register endpoint for all events
	_, _ = svc.CreateEndpoint(ctx, "t1", CreateEndpointInput{
		URL:    "http://localhost:9999/hook",
		Events: []string{"*"},
	})

	_ = svc.Dispatch(ctx, "t1", "dunning.started", map[string]any{})
	// Delivery is synchronous in test mode

	if len(store.deliveries) != 1 {
		t.Errorf("wildcard should match any event, got %d deliveries", len(store.deliveries))
	}
}

func TestMatchesEvent(t *testing.T) {
	tests := []struct {
		subscribed []string
		event      string
		want       bool
	}{
		{[]string{"*"}, "invoice.finalized", true},
		{[]string{"invoice.finalized"}, "invoice.finalized", true},
		{[]string{"invoice.finalized"}, "payment.succeeded", false},
		{[]string{"invoice.*"}, "invoice.finalized", true},
		{[]string{"invoice.*"}, "invoice.finalized", true},
		{[]string{"invoice.*"}, "payment.succeeded", false},
		{[]string{"invoice.finalized", "payment.succeeded"}, "payment.succeeded", true},
		{[]string{}, "anything", false},
	}

	for _, tt := range tests {
		got := matchesEvent(tt.subscribed, tt.event)
		if got != tt.want {
			t.Errorf("matchesEvent(%v, %q) = %v, want %v", tt.subscribed, tt.event, got, tt.want)
		}
	}
}

func TestDelivery_FailedHTTP(t *testing.T) {
	store := newMemStore()
	httpClient := &mockHTTPClient{statusCode: 500}
	svc := NewTestService(store, httpClient)
	ctx := context.Background()

	_, _ = svc.CreateEndpoint(ctx, "t1", CreateEndpointInput{URL: "http://localhost:9999/hook"})
	before := time.Now()
	_ = svc.Dispatch(ctx, "t1", "invoice.finalized", map[string]any{})
	// Delivery is synchronous in test mode

	if len(store.deliveries) != 1 {
		t.Fatalf("deliveries: got %d", len(store.deliveries))
	}
	// First failure schedules a retry (status stays pending)
	if store.deliveries[0].Status != domain.DeliveryPending {
		t.Errorf("status: got %q, want pending (scheduled for retry)", store.deliveries[0].Status)
	}
	// Exactly one attempt has happened. Pre-fix this was 2 — deliver()
	// incremented unconditionally AND scheduleRetryOrFail incremented again,
	// burning a retry and skipping the first backoff.
	if store.deliveries[0].AttemptCount != 1 {
		t.Errorf("attempt_count: got %d, want 1 (single increment per failed attempt)", store.deliveries[0].AttemptCount)
	}
	if store.deliveries[0].NextRetryAt == nil {
		t.Fatal("next_retry_at should be set for retry")
	}
	// The first retry must use the FIRST backoff (1m), not the second (5m).
	// next_retry_at = stamp + 1m + jitter(0–30s), where stamp ∈ [before, now].
	// Bound it against the captured dispatch window rather than time.Until at
	// read time: the latter subtracts the microseconds elapsed since stamping
	// and flaked at 59.9999s (just under 1m) whenever jitter landed near 0.
	gotRetry := *store.deliveries[0].NextRetryAt
	minRetry := before.Add(retryBackoffs[0])
	maxRetry := time.Now().Add(retryBackoffs[0] + 30*time.Second)
	if gotRetry.Before(minRetry) || gotRetry.After(maxRetry) {
		t.Errorf("next_retry_at = %v, want within [%v, %v] — first backoff must not be skipped", gotRetry, minRetry, maxRetry)
	}
}

// TestRetryLadder_AllBackoffsReachable locks the retry-ladder off-by-one
// fix: a delivery must walk EVERY retryBackoffs slot — including the final
// 24h one — before it permanently fails, and the fail must land on the
// attempt after the last slot is spent. Pre-fix the guard was
// `AttemptCount >= maxAttempts(5)`, so a delivery failed on attempt 5
// before retryBackoffs[4]=24h could ever schedule — the ladder ended at
// ~2.5h instead of the ~26.5h the 24h tail implies.
func TestRetryLadder_AllBackoffsReachable(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, &mockHTTPClient{statusCode: 500}) // unused; we drive scheduleRetryOrFail directly

	// Seed a fresh delivery (AttemptCount=0, as CreateDelivery makes it).
	d := domain.WebhookDelivery{
		ID: "vlx_whd_ladder", TenantID: "t1", WebhookEndpointID: "ep_1",
		WebhookEventID: "evt_gone", Status: domain.DeliveryPending,
	}
	store.deliveries = append(store.deliveries, d)
	store.deliveryLivemodes = append(store.deliveryLivemodes, false)

	read := func() domain.WebhookDelivery {
		for _, dd := range store.deliveries {
			if dd.ID == "vlx_whd_ladder" {
				return dd
			}
		}
		t.Fatal("delivery vanished")
		return domain.WebhookDelivery{}
	}

	// Each of the maxRetries failures must schedule the matching backoff
	// slot (1m, 5m, 30m, 2h, 24h) and keep the delivery pending.
	for i := 0; i < maxRetries; i++ {
		before := time.Now()
		svc.scheduleRetryOrFail(context.Background(), "t1", read(), "HTTP 500")
		got := read()
		if got.AttemptCount != i+1 {
			t.Fatalf("after failure %d: attempt_count=%d, want %d", i+1, got.AttemptCount, i+1)
		}
		if got.Status != domain.DeliveryPending {
			t.Fatalf("after failure %d (slot %v): status=%q, want pending — the ladder ended early", i+1, retryBackoffs[i], got.Status)
		}
		if got.NextRetryAt == nil {
			t.Fatalf("after failure %d: next_retry_at nil, want scheduled (backoff %v)", i+1, retryBackoffs[i])
		}
		// next_retry ≈ stamp + retryBackoffs[i] + jitter(0–30s).
		lo := before.Add(retryBackoffs[i])
		hi := time.Now().Add(retryBackoffs[i] + 30*time.Second)
		if got.NextRetryAt.Before(lo) || got.NextRetryAt.After(hi) {
			t.Errorf("failure %d backoff: next_retry=%v, want slot %v within [%v,%v]", i+1, *got.NextRetryAt, retryBackoffs[i], lo, hi)
		}
	}

	// The 24h slot (the previously-dead one) must have been the last
	// scheduled wait.
	last := read()
	if d24 := time.Until(*last.NextRetryAt); d24 < 23*time.Hour {
		t.Errorf("final scheduled wait = %v, want ~24h (retryBackoffs[%d]) — the 24h slot was never reached", d24, maxRetries-1)
	}

	// One more failure exhausts the ladder → permanent fail, no further retry.
	svc.scheduleRetryOrFail(context.Background(), "t1", read(), "HTTP 500")
	final := read()
	if final.Status != domain.DeliveryFailed {
		t.Errorf("after %d attempts: status=%q, want failed", maxRetries+1, final.Status)
	}
	if final.NextRetryAt != nil {
		t.Errorf("permanently-failed delivery must clear next_retry_at, got %v", *final.NextRetryAt)
	}
	if final.AttemptCount != maxRetries+1 {
		t.Errorf("final attempt_count=%d, want %d (1 initial + %d retries)", final.AttemptCount, maxRetries+1, maxRetries)
	}
}

func TestReplay(t *testing.T) {
	store := newMemStore()
	httpClient := &mockHTTPClient{statusCode: 200}
	svc := NewTestService(store, httpClient)
	ctx := context.Background()

	// Setup: create endpoint + dispatch event
	_, _ = svc.CreateEndpoint(ctx, "t1", CreateEndpointInput{
		URL:    "http://localhost:9999/hook",
		Events: []string{"invoice.finalized"},
	})
	_ = svc.Dispatch(ctx, "t1", "invoice.finalized", map[string]any{"id": "inv_1"})

	if len(store.deliveries) != 1 {
		t.Fatalf("initial deliveries: got %d, want 1", len(store.deliveries))
	}

	// Replay the event
	eventID := store.events[0].ID
	res, err := svc.Replay(ctx, "t1", eventID)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	// Replay clones the event into a fresh row tagged replay_of=eventID.
	if res.EventID == "" {
		t.Errorf("replay result missing event_id")
	}
	if res.ReplayOf != eventID {
		t.Errorf("replay result replay_of: got %q want %q", res.ReplayOf, eventID)
	}

	// Should have 2 deliveries now (original + replay clone's delivery)
	if len(store.deliveries) != 2 {
		t.Errorf("deliveries after replay: got %d, want 2", len(store.deliveries))
	}
}

// TestReplay_PinsLivemodeOnDelivery locks the H5 fix on the ASYNC
// (goroutine) delivery path — the production path, and the one the bug
// lived in. Pre-fix, Replay fired `go s.deliver(context.Background(), …)`
// with a bare ctx, so the delivery's CreateDelivery ran under
// postgres.Livemode's default of TRUE. A test-mode event's replay
// therefore wrote its delivery rows into the LIVE RLS partition —
// invisible in the test-mode dashboard, FK to the test-mode event broken.
// The fix pins the delivery ctx to the clone's livemode, mirroring
// Dispatch. We use NewService (async) and a signal channel to wait on the
// goroutine deterministically rather than sleeping.
func TestReplay_PinsLivemodeOnDelivery(t *testing.T) {
	store := newMemStore()
	store.deliverySignal = make(chan bool, 4)
	svc := NewService(store, &mockHTTPClient{statusCode: 200}) // async delivery

	// Endpoint + original event in TEST mode.
	testCtx := postgres.WithLivemode(context.Background(), false)
	_, _ = svc.CreateEndpoint(testCtx, "t1", CreateEndpointInput{
		URL:    "http://localhost:9999/hook",
		Events: []string{"invoice.finalized"},
	})
	_ = svc.Dispatch(testCtx, "t1", "invoice.finalized", map[string]any{"id": "inv_1"})

	// Drain the original dispatch's delivery signal (also async on this
	// service) and assert it was test-mode — proves the harness captures
	// the right thing.
	select {
	case lm := <-store.deliverySignal:
		if lm != false {
			t.Fatalf("dispatch delivery livemode: got %v, want false", lm)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("dispatch delivery goroutine did not fire")
	}

	eventID := store.events[0].ID
	if _, err := svc.Replay(testCtx, "t1", eventID); err != nil {
		t.Fatalf("replay: %v", err)
	}

	// The replay's delivery goroutine must write under the clone's TEST
	// mode. Pre-fix this arrived as TRUE (bare context.Background()).
	select {
	case lm := <-store.deliverySignal:
		if lm != false {
			t.Errorf("replay delivery livemode: got %v, want false — Replay goroutine ran the delivery write under the live partition", lm)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("replay delivery goroutine did not fire")
	}
}

// TestReplayDelivery_ScopedToOneEndpoint pins the property that makes the
// single-receiver replay worth having over event-wide Replay: with TWO
// endpoints subscribed to the event, replaying one endpoint's delivery
// creates exactly ONE new delivery — targeted at that endpoint — while
// the other receiver gets nothing. (Event-wide Replay on the same fixture
// would create two.)
func TestReplayDelivery_ScopedToOneEndpoint(t *testing.T) {
	store := newMemStore()
	svc := NewTestService(store, &mockHTTPClient{statusCode: 200})
	ctx := context.Background()

	epA, _ := svc.CreateEndpoint(ctx, "t1", CreateEndpointInput{
		URL: "http://localhost:9999/a", Events: []string{"invoice.finalized"},
	})
	epB, _ := svc.CreateEndpoint(ctx, "t1", CreateEndpointInput{
		URL: "http://localhost:9999/b", Events: []string{"invoice.finalized"},
	})
	_ = svc.Dispatch(ctx, "t1", "invoice.finalized", map[string]any{"id": "inv_1"})
	if len(store.deliveries) != 2 {
		t.Fatalf("initial deliveries: got %d, want 2", len(store.deliveries))
	}

	var bDelivery domain.WebhookDelivery
	for _, d := range store.deliveries {
		if d.WebhookEndpointID == epB.Endpoint.ID {
			bDelivery = d
		}
	}

	res, err := svc.ReplayDelivery(ctx, "t1", epB.Endpoint.ID, bDelivery.ID)
	if err != nil {
		t.Fatalf("replay delivery: %v", err)
	}
	if res.Status != "queued" || res.DeliveryID == "" {
		t.Errorf("result = %+v, want status=queued with a delivery_id", res)
	}
	if res.ReplayOf != bDelivery.WebhookEventID {
		t.Errorf("replay_of = %q, want the original event %q", res.ReplayOf, bDelivery.WebhookEventID)
	}

	if len(store.deliveries) != 3 {
		t.Fatalf("deliveries after scoped replay: got %d, want 3 (exactly one new)", len(store.deliveries))
	}
	newest := store.deliveries[len(store.deliveries)-1]
	if newest.WebhookEndpointID != epB.Endpoint.ID {
		t.Errorf("new delivery targets %q, want the replayed endpoint %q", newest.WebhookEndpointID, epB.Endpoint.ID)
	}
	for _, d := range store.deliveries {
		if d.WebhookEndpointID == epA.Endpoint.ID && d.ID != store.deliveries[0].ID && d.ID != store.deliveries[1].ID {
			t.Errorf("endpoint A received a delivery from B's scoped replay: %+v", d)
		}
	}
	// The clone must carry the replay pivot so the timeline folds it in.
	clone := store.events[len(store.events)-1]
	if clone.ReplayOfEventID == nil || *clone.ReplayOfEventID != bDelivery.WebhookEventID {
		t.Errorf("clone replay_of = %v, want %q", clone.ReplayOfEventID, bDelivery.WebhookEventID)
	}
}

// TestReplayDelivery_PausedEndpointRejected_NoOrphanClone pins guard
// ordering: a replay to a paused endpoint is rejected BEFORE the clone
// event is minted. Pre-ordering, the rejection path would leave behind a
// zero-delivery clone that renders as a phantom "no_endpoints" row in
// the live tail.
func TestReplayDelivery_PausedEndpointRejected_NoOrphanClone(t *testing.T) {
	store := newMemStore()
	svc := NewTestService(store, &mockHTTPClient{statusCode: 200})
	ctx := context.Background()

	ep, _ := svc.CreateEndpoint(ctx, "t1", CreateEndpointInput{
		URL: "http://localhost:9999/hook", Events: []string{"invoice.finalized"},
	})
	_ = svc.Dispatch(ctx, "t1", "invoice.finalized", map[string]any{"id": "inv_1"})
	deliveryID := store.deliveries[0].ID
	eventsBefore := len(store.events)

	inactive := false
	if _, err := svc.UpdateEndpoint(ctx, "t1", ep.Endpoint.ID, UpdateEndpointInput{Active: &inactive}); err != nil {
		t.Fatalf("pause endpoint: %v", err)
	}

	_, err := svc.ReplayDelivery(ctx, "t1", ep.Endpoint.ID, deliveryID)
	if err == nil {
		t.Fatal("expected paused-endpoint replay to be rejected")
	}
	var de *errs.DomainError
	if !errors.As(err, &de) || de.Kind != errs.ErrInvalidState {
		t.Errorf("error = %v, want errs.InvalidState (maps to 409 — a state conflict, not bad input)", err)
	}
	if len(store.events) != eventsBefore {
		t.Errorf("events after rejected replay: got %d, want %d — rejection minted an orphan clone", len(store.events), eventsBefore)
	}
	if len(store.deliveries) != 1 {
		t.Errorf("deliveries after rejected replay: got %d, want 1", len(store.deliveries))
	}
}

// TestReplayDelivery_WrongEndpointPath404s: a real delivery id reached
// under a DIFFERENT endpoint's route resolves as not-found — the URL
// pairing is authorization shape, not a hint.
func TestReplayDelivery_WrongEndpointPath404s(t *testing.T) {
	store := newMemStore()
	svc := NewTestService(store, &mockHTTPClient{statusCode: 200})
	ctx := context.Background()

	epA, _ := svc.CreateEndpoint(ctx, "t1", CreateEndpointInput{
		URL: "http://localhost:9999/a", Events: []string{"invoice.finalized"},
	})
	epB, _ := svc.CreateEndpoint(ctx, "t1", CreateEndpointInput{
		URL: "http://localhost:9999/b", Events: []string{"invoice.finalized"},
	})
	_ = svc.Dispatch(ctx, "t1", "invoice.finalized", map[string]any{"id": "inv_1"})

	var aDelivery domain.WebhookDelivery
	for _, d := range store.deliveries {
		if d.WebhookEndpointID == epA.Endpoint.ID {
			aDelivery = d
		}
	}

	_, err := svc.ReplayDelivery(ctx, "t1", epB.Endpoint.ID, aDelivery.ID)
	if !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound for a delivery under the wrong endpoint", err)
	}
}

func TestReplay_NotFound(t *testing.T) {
	svc := NewTestService(newMemStore(), &mockHTTPClient{statusCode: 200})

	_, err := svc.Replay(context.Background(), "t1", "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent event")
	}
}

// TestDispatch_ModeScoped verifies the core FEAT-8 P6 contract: a Dispatch
// in one mode (test or live) only creates deliveries for endpoints in the
// same mode. Cross-mode delivery would leak synthetic data into production
// monitoring (or vice versa), so both the RLS-driven ListEndpoints filter
// and the defensive ep.Livemode == event.Livemode check in service.go must
// hold. This test exercises both paths by seeding endpoints in both modes.
func TestDispatch_ModeScoped(t *testing.T) {
	store := newMemStore()
	client := &mockHTTPClient{statusCode: 200}
	svc := NewTestService(store, client)

	liveCtx := postgres.WithLivemode(context.Background(), true)
	testCtx := postgres.WithLivemode(context.Background(), false)

	liveEP, err := svc.CreateEndpoint(liveCtx, "t1", CreateEndpointInput{
		URL: "http://localhost:9001/live", Events: []string{"*"},
	})
	if err != nil {
		t.Fatalf("create live endpoint: %v", err)
	}
	testEP, err := svc.CreateEndpoint(testCtx, "t1", CreateEndpointInput{
		URL: "http://localhost:9002/test", Events: []string{"*"},
	})
	if err != nil {
		t.Fatalf("create test endpoint: %v", err)
	}

	if !liveEP.Endpoint.Livemode {
		t.Error("live endpoint should have livemode=true")
	}
	if testEP.Endpoint.Livemode {
		t.Error("test endpoint should have livemode=false")
	}

	// A test-mode Dispatch must land the event in the test partition and
	// only produce a delivery for the test endpoint.
	if err := svc.Dispatch(testCtx, "t1", "invoice.finalized", map[string]any{"n": 1}); err != nil {
		t.Fatalf("test dispatch: %v", err)
	}
	if len(store.events) != 1 || store.events[0].Livemode {
		t.Errorf("event after test dispatch: want livemode=false, got %+v", store.events)
	}
	if len(store.deliveries) != 1 {
		t.Fatalf("test dispatch: got %d deliveries, want 1", len(store.deliveries))
	}
	if store.deliveries[0].WebhookEndpointID != testEP.Endpoint.ID {
		t.Errorf("test dispatch went to wrong endpoint: %s", store.deliveries[0].WebhookEndpointID)
	}

	// A live-mode Dispatch must only hit the live endpoint.
	if err := svc.Dispatch(liveCtx, "t1", "invoice.finalized", map[string]any{"n": 2}); err != nil {
		t.Fatalf("live dispatch: %v", err)
	}
	if len(store.events) != 2 || !store.events[1].Livemode {
		t.Errorf("event after live dispatch: want livemode=true, got %+v", store.events[1])
	}
	if len(store.deliveries) != 2 {
		t.Fatalf("live dispatch: got %d deliveries total, want 2", len(store.deliveries))
	}
	if store.deliveries[1].WebhookEndpointID != liveEP.Endpoint.ID {
		t.Errorf("live dispatch went to wrong endpoint: %s", store.deliveries[1].WebhookEndpointID)
	}
}

// TestBuildSignatureHeader exercises the three states buildSignatureHeader
// has to get right: single-secret steady state, fresh rotation with a
// live secondary (Stripe-style dual v1=), and an expired secondary
// that must be skipped so rotation doesn't keep the old key alive
// beyond the grace window. The verifier on the partner side accepts any
// v1= match, so the test checks both signatures line up with their
// secrets.
func TestBuildSignatureHeader(t *testing.T) {
	body := []byte(`{"id":"evt_1","event_type":"invoice.finalized"}`)
	ts := "1714000000"
	now := time.Unix(1714000100, 0).UTC()

	verify := func(headerSecret, sigPart string) bool {
		mac := hmac.New(sha256.New, []byte(headerSecret))
		mac.Write([]byte(ts + "." + string(body)))
		return hex.EncodeToString(mac.Sum(nil)) == sigPart
	}
	extractV1 := func(header string) []string {
		var v1s []string
		for _, p := range strings.Split(header, ",") {
			kv := strings.SplitN(p, "=", 2)
			if len(kv) == 2 && kv[0] == "v1" {
				v1s = append(v1s, kv[1])
			}
		}
		return v1s
	}

	t.Run("single secret (steady state)", func(t *testing.T) {
		ep := domain.WebhookEndpoint{Secret: "whsec_primary"}
		got := buildSignatureHeader(ts, body, ep, now)
		sigs := extractV1(got)
		if len(sigs) != 1 {
			t.Fatalf("expected 1 v1 entry, got %d: %s", len(sigs), got)
		}
		if !verify("whsec_primary", sigs[0]) {
			t.Error("primary signature did not verify")
		}
	})

	t.Run("fresh rotation (dual v1)", func(t *testing.T) {
		future := now.Add(72 * time.Hour)
		ep := domain.WebhookEndpoint{
			Secret:                   "whsec_new",
			SecondarySecret:          "whsec_old",
			SecondarySecretExpiresAt: &future,
		}
		got := buildSignatureHeader(ts, body, ep, now)
		sigs := extractV1(got)
		if len(sigs) != 2 {
			t.Fatalf("expected 2 v1 entries during rotation, got %d: %s", len(sigs), got)
		}
		if !verify("whsec_new", sigs[0]) {
			t.Error("primary (new) signature did not verify")
		}
		if !verify("whsec_old", sigs[1]) {
			t.Error("secondary (old) signature did not verify")
		}
	})

	t.Run("expired secondary is skipped", func(t *testing.T) {
		past := now.Add(-time.Hour)
		ep := domain.WebhookEndpoint{
			Secret:                   "whsec_new",
			SecondarySecret:          "whsec_ancient",
			SecondarySecretExpiresAt: &past,
		}
		got := buildSignatureHeader(ts, body, ep, now)
		sigs := extractV1(got)
		if len(sigs) != 1 {
			t.Fatalf("expected 1 v1 entry after expiry, got %d: %s", len(sigs), got)
		}
		if !verify("whsec_new", sigs[0]) {
			t.Error("primary signature did not verify")
		}
	})

	t.Run("secondary secret present but expires_at nil", func(t *testing.T) {
		// Defensive: if some migration left a secondary secret without
		// a valid expiry, the header should NOT use it. Treat nil
		// expiry as already-expired.
		ep := domain.WebhookEndpoint{
			Secret:          "whsec_new",
			SecondarySecret: "whsec_orphan",
		}
		got := buildSignatureHeader(ts, body, ep, now)
		sigs := extractV1(got)
		if len(sigs) != 1 {
			t.Fatalf("expected 1 v1 entry when expires_at is nil, got %d: %s", len(sigs), got)
		}
	})
}

// TestRotateSecret_GracePeriod checks the end-to-end grace-period wiring:
// rotate returns a secondary_valid_until, a follow-up dispatch signs with
// both secrets, and after the window the header drops back to one.
func TestRotateSecret_GracePeriod(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()
	httpClient := &mockHTTPClient{statusCode: 200}
	svc := NewTestService(store, httpClient)

	created, err := svc.CreateEndpoint(ctx, "t1", CreateEndpointInput{
		URL:    "http://localhost:3000/webhook",
		Events: []string{"*"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	rot, err := svc.RotateSecret(ctx, "t1", created.Endpoint.ID)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if rot.SecondaryValidTill == nil {
		t.Fatal("rotate should return secondary_valid_until")
	}
	if !rot.SecondaryValidTill.After(time.Now().UTC()) {
		t.Error("secondary_valid_until should be in the future")
	}
	if rot.Secret == created.Secret {
		t.Fatal("rotate should produce a different secret")
	}

	// Dispatch — header should carry BOTH signatures while grace is live.
	if err := svc.Dispatch(ctx, "t1", "invoice.finalized", map[string]any{"invoice_id": "inv_1"}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if httpClient.lastRequest == nil {
		t.Fatal("no outbound request captured")
	}
	header := httpClient.lastRequest.Header.Get("Velox-Signature")
	if c := strings.Count(header, "v1="); c != 2 {
		t.Errorf("during grace, header should have 2 v1= entries, got %d: %s", c, header)
	}

	// Force the secondary to expire and re-dispatch — single sig again.
	ep := store.endpoints[created.Endpoint.ID]
	past := time.Now().UTC().Add(-time.Hour)
	ep.SecondarySecretExpiresAt = &past
	store.endpoints[created.Endpoint.ID] = ep

	httpClient.lastRequest = nil
	if err := svc.Dispatch(ctx, "t1", "invoice.finalized", map[string]any{"invoice_id": "inv_2"}); err != nil {
		t.Fatalf("dispatch 2: %v", err)
	}
	header2 := httpClient.lastRequest.Header.Get("Velox-Signature")
	if c := strings.Count(header2, "v1="); c != 1 {
		t.Errorf("after expiry, header should have 1 v1= entry, got %d: %s", c, header2)
	}
}

// TestRetryPendingDeliveries_OldEventResolvable is the regression guard for
// the stranded-delivery bug: the retry worker used to resolve the event via
// ListEvents(tenant, 1000) + linear scan, but the store clamps that list to a
// bounded newest-window (max 100). A delivery whose event had aged out of the
// window could never be matched, so it cycled in the pending pool forever and
// was never re-attempted. The fix point-looks the event up by id, which the
// store always resolves regardless of age. This test seeds enough newer events
// to push the target event out of the clamped window, then asserts the due
// delivery is actually re-attempted.
func TestRetryPendingDeliveries_OldEventResolvable(t *testing.T) {
	store := newMemStore()
	httpClient := &mockHTTPClient{statusCode: 200}
	svc := NewTestService(store, httpClient)
	ctx := context.Background()

	ep, err := svc.CreateEndpoint(ctx, "t1", CreateEndpointInput{
		URL:    "http://localhost:9999/hook",
		Events: []string{"*"},
	})
	if err != nil {
		t.Fatalf("create endpoint: %v", err)
	}

	// The event we'll attach the pending delivery to — created first, so it's
	// the OLDEST event in the store.
	oldEvent, err := store.CreateEvent(ctx, "t1", domain.WebhookEvent{EventType: "invoice.finalized"})
	if err != nil {
		t.Fatalf("create old event: %v", err)
	}

	// A delivery for that old event, due for retry (NextRetryAt in the past).
	past := time.Now().UTC().Add(-time.Hour)
	pending, err := store.CreateDelivery(ctx, "t1", domain.WebhookDelivery{
		WebhookEndpointID: ep.Endpoint.ID,
		WebhookEventID:    oldEvent.ID,
		Status:            domain.DeliveryPending,
		AttemptCount:      1,
		Livemode:          oldEvent.Livemode,
		NextRetryAt:       &past,
	})
	if err != nil {
		t.Fatalf("create pending delivery: %v", err)
	}

	// Bury the old event under enough newer events to push it out of the
	// store's hard 100-row ListEvents window — the exact production condition
	// that stranded the delivery (the retry worker requested 1000 but the
	// store clamps to 100).
	for i := 0; i < 120; i++ {
		if _, err := store.CreateEvent(ctx, "t1", domain.WebhookEvent{EventType: "noise.event"}); err != nil {
			t.Fatalf("seed noise event: %v", err)
		}
	}

	// Sanity: even requesting the max, the old event is NOT in the newest
	// window — this is exactly the condition that stranded the delivery.
	recent, err := store.ListEvents(ctx, "t1", 1000)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	for _, e := range recent {
		if e.ID == oldEvent.ID {
			t.Fatal("test precondition broken: old event should be outside the clamped window")
		}
	}

	if err := svc.RetryPendingDeliveries(ctx); err != nil {
		t.Fatalf("retry: %v", err)
	}

	// The delivery must have been re-attempted (mock returns 200 → succeeded).
	var got domain.WebhookDelivery
	for _, dd := range store.deliveries {
		if dd.ID == pending.ID {
			got = dd
		}
	}
	if got.Status != domain.DeliverySucceeded {
		t.Errorf("delivery status: got %q, want succeeded (event must resolve by id, not via the clamped list)", got.Status)
	}
	if httpClient.lastRequest == nil {
		t.Error("expected an outbound retry request, got none (delivery was stranded)")
	}
}

// TestRetryPendingDeliveries_DeletedEventFails verifies the not-found branch:
// when the event row is genuinely gone, the delivery is marked failed (not
// left cycling in the pending pool).
func TestRetryPendingDeliveries_DeletedEventFails(t *testing.T) {
	store := newMemStore()
	httpClient := &mockHTTPClient{statusCode: 200}
	svc := NewTestService(store, httpClient)
	ctx := context.Background()

	ep, err := svc.CreateEndpoint(ctx, "t1", CreateEndpointInput{
		URL:    "http://localhost:9999/hook",
		Events: []string{"*"},
	})
	if err != nil {
		t.Fatalf("create endpoint: %v", err)
	}

	past := time.Now().UTC().Add(-time.Hour)
	pending, err := store.CreateDelivery(ctx, "t1", domain.WebhookDelivery{
		WebhookEndpointID: ep.Endpoint.ID,
		WebhookEventID:    "vlx_whevt_gone", // never created
		Status:            domain.DeliveryPending,
		AttemptCount:      1,
		NextRetryAt:       &past,
	})
	if err != nil {
		t.Fatalf("create pending delivery: %v", err)
	}

	if err := svc.RetryPendingDeliveries(ctx); err != nil {
		t.Fatalf("retry: %v", err)
	}

	var got domain.WebhookDelivery
	for _, dd := range store.deliveries {
		if dd.ID == pending.ID {
			got = dd
		}
	}
	if got.Status != domain.DeliveryFailed {
		t.Errorf("delivery status: got %q, want failed (deleted event must not strand the delivery)", got.Status)
	}
	if httpClient.lastRequest != nil {
		t.Error("no outbound request should be made for a delivery whose event was deleted")
	}
}

// TestAssertDialAddrAllowed is the SSRF dial-control regression guard. The
// delivery client's DialContext rejects private/link-local/loopback/metadata
// targets at the actual dial address, closing the DNS-rebinding window that
// CreateEndpoint-time validation alone leaves open. In production mode
// (allowLoopback=false) loopback is blocked like everything else; the dev
// exception is covered by its own test below.
func TestAssertDialAddrAllowed(t *testing.T) {
	blocked := []string{
		"10.0.0.1",        // RFC 1918
		"172.16.5.4",      // RFC 1918
		"192.168.1.1",     // RFC 1918
		"127.0.0.1",       // loopback
		"169.254.169.254", // cloud metadata (link-local)
		"0.0.0.0",         // unspecified
		"::1",             // IPv6 loopback
		"fe80::1",         // IPv6 link-local
		"fd00::1",         // IPv6 ULA (private)
	}
	for _, ipStr := range blocked {
		if err := assertDialAddrAllowed(ipStr, false); err == nil {
			t.Errorf("assertDialAddrAllowed(%q, prod) = nil, want blocked", ipStr)
		}
	}

	allowed := []string{
		"8.8.8.8",              // public IPv4
		"1.1.1.1",              // public IPv4
		"2606:4700:4700::1111", // public IPv6
	}
	for _, ipStr := range allowed {
		if err := assertDialAddrAllowed(ipStr, false); err != nil {
			t.Errorf("assertDialAddrAllowed(%q, prod) = %v, want allowed", ipStr, err)
		}
	}
}

// TestLoopbackException_SameFlagAtBothLayers pins the fix for the walk find
// (FLOW W0, 2026-08-03): the create-time validator advertised "except
// localhost" while the egress-hardened dialer refused all loopback, so an
// endpoint the operator could create was one Velox could never deliver to —
// every delivery sat pending with the refusal buried in the delivery detail.
// The exception must be ONE flag honored by BOTH layers, per environment.
func TestLoopbackException_SameFlagAtBothLayers(t *testing.T) {
	loopbacks := []string{"http://localhost:9099/hook", "http://127.0.0.1:9099/hook", "http://[::1]:9099/hook"}

	// Development: both layers admit loopback.
	for _, u := range loopbacks {
		if err := validateWebhookURL(u, true); err != nil {
			t.Errorf("dev validate(%q) = %v, want allowed", u, err)
		}
	}
	for _, h := range []string{"localhost", "127.0.0.1", "::1"} {
		if err := assertDialAddrAllowed(h, true); err != nil {
			t.Errorf("dev dial(%q) = %v, want allowed", h, err)
		}
	}
	// Development: the exception is loopback ONLY — private/metadata stay blocked.
	for _, h := range []string{"10.0.0.1", "169.254.169.254", "fd00::1"} {
		if err := assertDialAddrAllowed(h, true); err == nil {
			t.Errorf("dev dial(%q) = nil, want blocked — the dev exception must not widen past loopback", h)
		}
	}

	// Production: BOTH layers refuse, and the validator refuses LOUDLY at
	// create instead of minting an endpoint the dialer will starve.
	for _, u := range loopbacks {
		if err := validateWebhookURL(u, false); err == nil {
			t.Errorf("prod validate(%q) = nil, want refused — create must not promise what delivery refuses", u)
		}
	}

	// The prefix bypass stays dead in both modes: "localhost.evil.com" is a
	// public host and must meet the HTTPS bar, not ride the loopback carve-out.
	for _, allow := range []bool{true, false} {
		if err := validateWebhookURL("http://localhost.evil.com/hook", allow); err == nil {
			t.Errorf("validate(localhost.evil.com, allowLoopback=%v) = nil — plaintext HTTP to a public host via the localhost prefix trick", allow)
		}
	}
}

// TestIsBlockedIP covers the predicate directly across IPv4 and IPv6 reserved
// ranges that the dial control depends on.
func TestIsBlockedIP(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		{"10.0.0.1", true},
		{"172.16.0.1", true},
		{"192.168.0.1", true},
		{"127.0.0.1", true},
		{"169.254.169.254", true},
		{"0.0.0.0", true},
		{"::1", true},
		{"fe80::1", true},
		{"fc00::1", true},
		{"8.8.8.8", false},
		{"203.0.113.10", false},
		{"2606:4700:4700::1111", false},
	}
	for _, c := range cases {
		ip := net.ParseIP(c.ip)
		if ip == nil {
			t.Fatalf("bad test IP %q", c.ip)
		}
		if got := isBlockedIP(ip); got != c.want {
			t.Errorf("isBlockedIP(%q) = %v, want %v", c.ip, got, c.want)
		}
	}
}

// Event-name validation (2026-07-05): endpoint create/update rejects
// subscriptions to event types the engine never emits — pre-fix any string
// was accepted and matchesEvent silently never fired (Velox's own recipes
// shipped two phantom names: invoice.payment_failed, subscription.updated).
func TestCreateEndpoint_RejectsUnknownEventNames(t *testing.T) {
	svc := NewService(newMemStore(), nil)
	ctx := context.Background()

	for _, bad := range []string{"invoice.payment_failed", "subscription.updated", "totally.made_up"} {
		if _, err := svc.CreateEndpoint(ctx, "t1", CreateEndpointInput{
			URL: "http://localhost:9099/hook", Events: []string{bad},
		}); err == nil {
			t.Errorf("phantom event %q must be rejected at create", bad)
		}
	}
	// The two payment_method.* events were EMITTED long before they were
	// admitted to the catalog — a subscriber to events Velox actually fires
	// was told "Velox never emits it" (2026-07-10 design census). Pin their
	// acceptance so the emit-catalog gap can't silently reopen.
	if _, err := svc.CreateEndpoint(ctx, "t1", CreateEndpointInput{
		URL: "http://localhost:9099/hook", Events: []string{"payment_method.attached", "payment_method.updated"},
	}); err != nil {
		t.Errorf("payment_method.* events are emitted and must be subscribable: %v", err)
	}
	// Real names, the bare wildcard, and prefix wildcards all pass.
	if _, err := svc.CreateEndpoint(ctx, "t1", CreateEndpointInput{
		URL: "http://localhost:9099/hook", Events: []string{"payment.failed", "subscription.item.updated", "invoice.*"},
	}); err != nil {
		t.Fatalf("valid names must pass: %v", err)
	}
	if _, err := svc.CreateEndpoint(ctx, "t1", CreateEndpointInput{
		URL: "http://localhost:9099/hook2", Events: []string{"nonexistent.*"}}); err == nil {
		t.Error("a prefix wildcard matching no emitted event must be rejected")
	}
}

// UpdateEndpoint mutates url/events/active WITHOUT touching the signing
// secret — the pre-fix delete+recreate path rotated it and forced a
// receiver redeploy, and recipe-created endpoints (inactive + placeholder
// URL) were permanently dead.
func TestUpdateEndpoint(t *testing.T) {
	svc := NewService(newMemStore(), nil)
	ctx := context.Background()

	created, err := svc.CreateEndpoint(ctx, "t1", CreateEndpointInput{
		URL: "http://localhost:9099/old", Events: []string{"invoice.finalized"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	strp := func(v string) *string { return &v }
	boolp := func(v bool) *bool { return &v }
	evp := func(v []string) *[]string { return &v }

	updated, err := svc.UpdateEndpoint(ctx, "t1", created.Endpoint.ID, UpdateEndpointInput{
		URL:    strp("http://localhost:9099/new"),
		Events: evp([]string{"payment.failed", "credit.balance_depleted"}),
		Active: boolp(false),
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.URL != "http://localhost:9099/new" || updated.Active {
		t.Fatalf("update not applied: %+v", updated)
	}
	if len(updated.Events) != 2 || updated.Events[0] != "payment.failed" {
		t.Fatalf("events not applied: %v", updated.Events)
	}

	// Secret unchanged — re-read and compare.
	after, err := svc.store.GetEndpoint(ctx, "t1", created.Endpoint.ID)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if after.Secret != created.Secret {
		t.Fatal("PATCH must never rotate the signing secret")
	}

	// Phantom names rejected on update too.
	if _, err := svc.UpdateEndpoint(ctx, "t1", created.Endpoint.ID, UpdateEndpointInput{
		Events: evp([]string{"invoice.payment_failed"}),
	}); err == nil {
		t.Fatal("phantom event must be rejected at update")
	}
	// Nil fields = unchanged.
	same, err := svc.UpdateEndpoint(ctx, "t1", created.Endpoint.ID, UpdateEndpointInput{})
	if err != nil {
		t.Fatalf("no-op update: %v", err)
	}
	if same.URL != "http://localhost:9099/new" || len(same.Events) != 2 {
		t.Fatalf("no-op update must leave fields unchanged: %+v", same)
	}
}
