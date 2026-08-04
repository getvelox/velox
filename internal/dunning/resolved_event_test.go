package dunning

import (
	"context"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// captureDispatcher records every dispatched event for assertion.
type captureDispatcher struct {
	events []capturedDunningEvent
}

type capturedDunningEvent struct {
	eventType string
	payload   map[string]any
}

func (c *captureDispatcher) Dispatch(_ context.Context, _, eventType string, payload map[string]any) error {
	c.events = append(c.events, capturedDunningEvent{eventType: eventType, payload: payload})
	return nil
}

func (c *captureDispatcher) firstOf(eventType string) (map[string]any, bool) {
	for _, e := range c.events {
		if e.eventType == eventType {
			return e.payload, true
		}
	}
	return nil, false
}

func (c *captureDispatcher) countOf(eventType string) int {
	n := 0
	for _, e := range c.events {
		if e.eventType == eventType {
			n++
		}
	}
	return n
}

// TestResolveRunNow_Idempotent_FiresEventOnce locks the resolve CAS: two resolvers
// hitting the SAME active run via resolveRunNow — the reported regression, where a
// card-settle resolve fires just before processRun's own resolve on a synchronous
// retry-success — must fire dunning.resolved EXACTLY ONCE (one outbound webhook, one
// resolved timeline row), not double-notify integrators. Calls resolveRunNow
// directly (not ResolveByInvoice, which pre-guards on GetActiveRunByInvoice) so the
// CAS itself is what's under test.
func TestResolveRunNow_Idempotent_FiresEventOnce(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, &noopRetrier{}, nil)
	disp := &captureDispatcher{}
	svc.SetEventDispatcher(disp)
	ctx := context.Background()

	run, err := svc.StartDunning(ctx, "t1", "inv_1", "cus_1", time.Now(), domain.DunningCausePaymentFailed)
	if err != nil {
		t.Fatalf("StartDunning: %v", err)
	}

	// Resolver #1 wins the CAS and fires the event.
	if _, err := svc.resolveRunNow(ctx, "t1", run, domain.ResolutionPaymentRecovered, "payment_recovered"); err != nil {
		t.Fatalf("resolve #1: %v", err)
	}
	// Resolver #2 passes the SAME stale in-memory run (as processRun does — its copy
	// is still State=active); the CAS must make it a no-op, not a second fire.
	if _, err := svc.resolveRunNow(ctx, "t1", run, domain.ResolutionPaymentRecovered, "payment_recovered"); err != nil {
		t.Fatalf("resolve #2 must no-op, not error: %v", err)
	}

	if got := disp.countOf(domain.EventDunningResolved); got != 1 {
		t.Fatalf("dunning.resolved dispatched %d times, want exactly 1 (a double-resolve must fire once)", got)
	}
}

// TestServiceResolveRun_Idempotent_AfterAutomatedResolve locks that the operator
// manual-resolve (Service.ResolveRun) also goes through the resolve CAS: if the run
// was already resolved by an automated path (a card settle / the scheduler), an
// operator resolve on the same run does NOT fire a second dunning.resolved.
func TestServiceResolveRun_Idempotent_AfterAutomatedResolve(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, &noopRetrier{}, nil)
	disp := &captureDispatcher{}
	svc.SetEventDispatcher(disp)
	ctx := context.Background()

	run, err := svc.StartDunning(ctx, "t1", "inv_1", "cus_1", time.Now(), domain.DunningCausePaymentFailed)
	if err != nil {
		t.Fatalf("StartDunning: %v", err)
	}

	// An automated path (e.g. the card-settle resolve) resolves the run first.
	if err := svc.ResolveByInvoice(ctx, "t1", "inv_1", domain.ResolutionPaymentRecovered); err != nil {
		t.Fatalf("automated resolve: %v", err)
	}
	// The operator then hits Resolve on the same (now-resolved) run — must no-op, not
	// fire a second webhook.
	if _, err := svc.ResolveRun(ctx, "t1", run.ID, domain.ResolutionInvoiceVoided); err != nil {
		t.Fatalf("operator ResolveRun must no-op, not error: %v", err)
	}

	if got := disp.countOf(domain.EventDunningResolved); got != 1 {
		t.Fatalf("dunning.resolved dispatched %d times, want exactly 1 (operator resolve after an automated resolve must not double-fire)", got)
	}
}

// TestDunningResolved_EventFires covers the medium-severity audit finding:
// dunning.resolved was advertised in the event catalog but never dispatched,
// silently dropping the payment-recovery signal that integrators subscribe to.
// Both operator-facing resolution paths (ResolveRun, ResolveByInvoice) must
// fire it with the run/invoice/customer/resolution payload.
func TestDunningResolved_EventFires(t *testing.T) {
	t.Run("ResolveRun", func(t *testing.T) {
		store := newMemStore()
		svc := NewService(store, &noopRetrier{}, nil)
		disp := &captureDispatcher{}
		svc.SetEventDispatcher(disp)
		ctx := context.Background()

		run, _ := svc.StartDunning(ctx, "t1", "inv_1", "cus_1", time.Now(), domain.DunningCausePaymentFailed)
		if _, err := svc.ResolveRun(ctx, "t1", run.ID, domain.ResolutionPaymentRecovered); err != nil {
			t.Fatalf("ResolveRun: %v", err)
		}

		payload, ok := disp.firstOf(domain.EventDunningResolved)
		if !ok {
			t.Fatalf("dunning.resolved not dispatched; got %+v", disp.events)
		}
		assertResolvedPayload(t, payload, "inv_1", "cus_1", string(domain.ResolutionPaymentRecovered))
	})

	t.Run("ResolveByInvoice", func(t *testing.T) {
		store := newMemStore()
		svc := NewService(store, &noopRetrier{}, nil)
		disp := &captureDispatcher{}
		svc.SetEventDispatcher(disp)
		ctx := context.Background()

		if _, err := svc.StartDunning(ctx, "t1", "inv_2", "cus_2", time.Now(), domain.DunningCausePaymentFailed); err != nil {
			t.Fatalf("StartDunning: %v", err)
		}
		if err := svc.ResolveByInvoice(ctx, "t1", "inv_2", domain.ResolutionPaymentRecovered); err != nil {
			t.Fatalf("ResolveByInvoice: %v", err)
		}

		payload, ok := disp.firstOf(domain.EventDunningResolved)
		if !ok {
			t.Fatalf("dunning.resolved not dispatched; got %+v", disp.events)
		}
		assertResolvedPayload(t, payload, "inv_2", "cus_2", string(domain.ResolutionPaymentRecovered))
	})
}

func assertResolvedPayload(t *testing.T, payload map[string]any, invoiceID, customerID, resolution string) {
	t.Helper()
	if payload["invoice_id"] != invoiceID {
		t.Errorf("invoice_id: got %v, want %s", payload["invoice_id"], invoiceID)
	}
	if payload["customer_id"] != customerID {
		t.Errorf("customer_id: got %v, want %s", payload["customer_id"], customerID)
	}
	if payload["resolution"] != resolution {
		t.Errorf("resolution: got %v, want %s", payload["resolution"], resolution)
	}
	if payload["run_id"] == nil || payload["run_id"] == "" {
		t.Errorf("run_id missing from payload: %+v", payload)
	}
}
