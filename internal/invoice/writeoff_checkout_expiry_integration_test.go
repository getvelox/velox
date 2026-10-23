package invoice_test

import (
	"context"
	"testing"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/webhook"
)

// stopperRecorder records both Stripe legs: StopCollection (a void's) and
// ExpireCheckoutSessions (a write-off's).
type stopperRecorder struct{ stopped, expired []string }

func (r *stopperRecorder) StopCollection(_ context.Context, _ string, inv domain.Invoice) {
	r.stopped = append(r.stopped, inv.ID)
}

func (r *stopperRecorder) ExpireCheckoutSessions(_ context.Context, _ string, inv domain.Invoice) {
	r.expired = append(r.expired, inv.ID)
}

// A write-off expires the invoice's open Stripe Checkout sessions, with the
// same call and the same best-effort handling a void uses. The write-off's
// transaction already closed Velox's checkout claims, but the session itself
// stayed payable at Stripe until it expired (up to an hour), so a customer
// with the page open could still pay a written-off invoice (ADR-110 closes
// self-service payment on one). A write-off does not cancel the
// PaymentIntent: only a void does. Every write-off goes through
// Service.MarkUncollectible (the operator endpoint, dunning's final action
// and dunning's invoice_not_collectible resolve), so it is tested here.
//
// Mutation: drop the expiry call from MarkUncollectible → expired is empty.
func TestMarkUncollectible_ExpiresCheckoutSessions(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Write-off Checkout Expiry")
	store := invoice.NewPostgresStore(db)
	inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-WO-EXPIRE")

	rec := &stopperRecorder{}
	svc := invoice.NewService(store, nil, nil)
	svc.SetCollectionStopper(rec)

	if _, err := svc.MarkUncollectible(ctx, tenantID, inv.ID); err != nil {
		t.Fatalf("write off: %v", err)
	}
	if len(rec.expired) != 1 || rec.expired[0] != inv.ID {
		t.Errorf("ExpireCheckoutSessions calls = %v, want exactly one for %s: a written-off invoice must not stay payable on an open Checkout page", rec.expired, inv.ID)
	}
	if len(rec.stopped) != 0 {
		t.Errorf("StopCollection calls = %v, want none: a write-off does not cancel the PaymentIntent", rec.stopped)
	}

	// A refused write-off (already written off) expires nothing.
	if _, err := svc.MarkUncollectible(ctx, tenantID, inv.ID); err == nil {
		t.Fatal("second write-off succeeded, want refused")
	}
	if len(rec.expired) != 1 {
		t.Errorf("ExpireCheckoutSessions calls after a refused write-off = %v, want still one", rec.expired)
	}
}

// endingStopper is the Stripe expiry. With endCaller set it is slow enough
// that the caller's context ends while it runs (the request's 30s timeout, a
// client disconnect, a dunning tick stopping), so it cancels that context. It
// records whether the context it was handed was still live.
type endingStopper struct {
	endCaller context.CancelFunc
	liveCtx   []bool
}

func (*endingStopper) StopCollection(context.Context, string, domain.Invoice) {}

func (s *endingStopper) ExpireCheckoutSessions(ctx context.Context, _ string, _ domain.Invoice) {
	s.liveCtx = append(s.liveCtx, ctx.Err() == nil)
	if s.endCaller != nil {
		s.endCaller()
	}
}

// endingAudit is the real audit logger, but the caller's context ends while
// it writes.
type endingAudit struct {
	invoice.AuditLogger
	endCaller context.CancelFunc
}

func (a endingAudit) Log(ctx context.Context, tenantID, action, resourceType, resourceID, resourceLabel string, metadata map[string]any) error {
	err := a.AuditLogger.Log(ctx, tenantID, action, resourceType, resourceID, resourceLabel, metadata)
	a.endCaller()
	return err
}

// A committed write-off runs its audit row, its invoice.marked_uncollectible
// event and its Stripe Checkout expiry to the end even when the caller's
// context ends part-way, as a committed void does (AfterVoid). Before, only
// the Stripe leg ran on a context that outlives the caller's, and the event
// was enqueued after it on the caller's: a slow Stripe lost the event for
// good while the write-off stood.
//
// Mutations: run the legs on the caller's context (drop the WithoutCancel)
// → the second case is red; also put the expiry back before the event → the
// first case is red too.
func TestMarkUncollectible_CallerContextEnds_EveryLegStillRuns(t *testing.T) {
	db := testutil.SetupTestDB(t)
	base := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Write-off Caller Ends")
	store := invoice.NewPostgresStore(db)

	for i, tc := range []struct {
		name   string
		during string // the leg during which the caller's context ends
	}{
		{"caller ends during the Stripe expiry", "expiry"},
		{"caller ends during the audit write", "audit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inv := seedClaimableInvoice(t, db, base, tenantID, "INV-WO-ENDS-"+string(rune('A'+i)))
			ctx, cancel := context.WithCancel(base)
			defer cancel()

			stopper := &endingStopper{}
			var logger invoice.AuditLogger = audit.NewLogger(db)
			if tc.during == "expiry" {
				stopper.endCaller = cancel
			} else {
				logger = endingAudit{AuditLogger: logger, endCaller: cancel}
			}
			svc := invoice.NewService(store, nil, nil)
			svc.SetCollectionStopper(stopper)
			svc.SetAuditLogger(logger)
			svc.SetEventDispatcher(webhook.NewOutboxDispatcher(webhook.NewOutboxStore(db)))

			got, err := svc.MarkUncollectible(ctx, tenantID, inv.ID)
			if err != nil {
				t.Fatalf("write off: %v", err)
			}
			if got.Status != domain.InvoiceUncollectible {
				t.Fatalf("status = %s, want uncollectible", got.Status)
			}
			if n := countRows(t, db, base, tenantID, `SELECT count(*) FROM webhook_outbox
				WHERE event_type = 'invoice.marked_uncollectible' AND payload->'invoice'->>'id' = $1`, inv.ID); n != 1 {
				t.Errorf("%d invoice.marked_uncollectible outbox rows, want 1: the write-off committed, so integrators must hear of it", n)
			}
			if n := countRows(t, db, base, tenantID, `SELECT count(*) FROM audit_log
				WHERE resource_id = $1 AND metadata->>'action' = 'marked_uncollectible'`, inv.ID); n != 1 {
				t.Errorf("%d marked_uncollectible audit rows, want 1", n)
			}
			if len(stopper.liveCtx) != 1 || !stopper.liveCtx[0] {
				t.Errorf("Stripe expiry calls with a live context = %v, want [true]: a call on an ended context leaves the Checkout page payable", stopper.liveCtx)
			}
		})
	}
}
