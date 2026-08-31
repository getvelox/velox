package webhook

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// blockingHTTPClient answers instantly for every endpoint except one, whose
// requests block until released. It records completion order.
type blockingHTTPClient struct {
	slowURL string
	release chan struct{}

	mu   sync.Mutex
	done []string
}

func (c *blockingHTTPClient) Do(req *http.Request) (*http.Response, error) {
	url := req.URL.String()
	if url == c.slowURL {
		<-c.release // hold the slot, as a dead endpoint holds it for its whole budget
	}
	c.mu.Lock()
	c.done = append(c.done, url)
	c.mu.Unlock()
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true}`))}, nil
}

func (c *blockingHTTPClient) completed() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.done...)
}

// TestRetryPendingDeliveries_SlowEndpointDoesNotBlockTheBatch is the
// head-of-line-blocking guard. One tenant's dead endpoint used to hold the
// whole tick: the batch was attempted sequentially, so its cost was the SUM of
// its slowest members (10 rows x perRetryRowBudget ~= 130s) and every other
// tenant's retries waited behind it.
//
// Deterministic, not timing-based: the slow endpoint blocks until this test
// releases it, and the assertion is that the fast deliveries COMPLETE while it
// is still blocked. Mutation check: set retryConcurrency to 1 → nothing
// completes until the slow endpoint is released → this fails.
func TestRetryPendingDeliveries_SlowEndpointDoesNotBlockTheBatch(t *testing.T) {
	store := newMemStore()
	client := &blockingHTTPClient{slowURL: "http://localhost:9999/slow", release: make(chan struct{})}
	svc := NewTestService(store, client)
	ctx := context.Background()

	slowEP, err := svc.CreateEndpoint(ctx, "t1", CreateEndpointInput{URL: client.slowURL, Events: []string{"*"}})
	if err != nil {
		t.Fatalf("create slow endpoint: %v", err)
	}
	fastEP, err := svc.CreateEndpoint(ctx, "t2", CreateEndpointInput{URL: "http://localhost:9999/fast", Events: []string{"*"}})
	if err != nil {
		t.Fatalf("create fast endpoint: %v", err)
	}

	// One event per tenant: a delivery resolves its event under its OWN
	// tenant, so a shared event id would resolve only for one of them.
	events := map[string]domain.WebhookEvent{}
	for _, tenant := range []string{"t1", "t2"} {
		ev, err := store.CreateEvent(ctx, tenant, domain.WebhookEvent{EventType: "invoice.finalized"})
		if err != nil {
			t.Fatalf("create event for %s: %v", tenant, err)
		}
		events[tenant] = ev
	}
	past := time.Now().UTC().Add(-time.Hour)
	mk := func(tenant, epID string) {
		t.Helper()
		if _, err := store.CreateDelivery(ctx, tenant, domain.WebhookDelivery{
			WebhookEventID: events[tenant].ID, WebhookEndpointID: epID,
			Status: domain.DeliveryPending, NextRetryAt: &past,
		}); err != nil {
			t.Fatalf("create delivery: %v", err)
		}
	}
	// The dead endpoint is claimed first, exactly as a backlogged tenant's rows
	// would be (oldest next_retry_at first).
	mk("t1", slowEP.Endpoint.ID)
	for i := 0; i < 3; i++ {
		mk("t2", fastEP.Endpoint.ID)
	}

	tickDone := make(chan error, 1)
	go func() { tickDone <- svc.RetryPendingDeliveries(ctx) }()

	// While the slow endpoint is still blocked, every fast delivery must land.
	deadline := time.Now().Add(5 * time.Second)
	for {
		n := 0
		for _, u := range client.completed() {
			if u != client.slowURL {
				n++
			}
		}
		if n == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d/3 fast deliveries completed while one dead endpoint held its slot — the batch is still serialized behind it", n)
		}
		time.Sleep(5 * time.Millisecond)
	}

	close(client.release)
	select {
	case err := <-tickDone:
		if err != nil {
			t.Fatalf("tick: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("tick did not finish after the slow endpoint was released")
	}
	if got := len(client.completed()); got != 4 {
		t.Fatalf("completed %d deliveries, want 4 (every claimed row is still attempted)", got)
	}
}
