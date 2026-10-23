package webhook_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/webhook"
)

func enqueueFailureCount(t *testing.T, eventType string) float64 {
	t.Helper()

	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}

	for _, family := range families {
		if family.GetName() != "velox_webhook_outbox_enqueue_failures_total" {
			continue
		}

		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetName() == "event_type" &&
					label.GetValue() == eventType {
					return metric.GetCounter().GetValue()
				}
			}
		}
	}

	// CounterVec does not expose a label combination until it is used.
	return 0
}

func TestOutboxDispatcher_EnqueueFailureMetric(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Dispatcher metrics")
	dispatcher := webhook.NewOutboxDispatcher(webhook.NewOutboxStore(db))

	const eventType = "invoice.finalized"
	const otherEventType = "subscription.created"

	t.Run("successful enqueue does not increment counter", func(t *testing.T) {
		before := enqueueFailureCount(t, eventType)

		err := dispatcher.Dispatch(ctx, tenantID, eventType, map[string]any{
			"invoice_id": "inv_metric_test",
		})
		if err != nil {
			t.Fatalf("dispatch: %v", err)
		}

		after := enqueueFailureCount(t, eventType)
		if after != before {
			t.Errorf("counter changed after success: before=%v, after=%v",
				before, after)
		}
	})

	t.Run("failed enqueue increments only its event type", func(t *testing.T) {
		before := enqueueFailureCount(t, eventType)
		otherBefore := enqueueFailureCount(t, otherEventType)

		// Channels cannot be serialized as JSON, so enqueue must fail.
		err := dispatcher.Dispatch(ctx, tenantID, eventType, map[string]any{
			"invalid": make(chan int),
		})

		var jsonErr *json.UnsupportedTypeError
		if !errors.As(err, &jsonErr) {
			t.Fatalf("expected JSON unsupported-type error, got %v", err)
		}

		after := enqueueFailureCount(t, eventType)
		if after != before+1 {
			t.Errorf("counter: got %v, want %v", after, before+1)
		}

		otherAfter := enqueueFailureCount(t, otherEventType)
		if otherAfter != otherBefore {
			t.Errorf("unrelated counter changed: before=%v, after=%v",
				otherBefore, otherAfter)
		}
	})

	t.Run("another event type has its own counter", func(t *testing.T) {
		before := enqueueFailureCount(t, otherEventType)
		originalBefore := enqueueFailureCount(t, eventType)

		err := dispatcher.Dispatch(ctx, tenantID, otherEventType, map[string]any{
			"invalid": make(chan int),
		})
		if err == nil {
			t.Fatal("expected enqueue error")
		}

		after := enqueueFailureCount(t, otherEventType)
		if after != before+1 {
			t.Errorf("counter: got %v, want %v", after, before+1)
		}

		originalAfter := enqueueFailureCount(t, eventType)
		if originalAfter != originalBefore {
			t.Errorf("unrelated counter changed: before=%v, after=%v",
				originalBefore, originalAfter)
		}
	})
}
