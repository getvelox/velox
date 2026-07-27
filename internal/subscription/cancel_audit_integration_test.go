package subscription

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/webhook"
)

// failingCancelEmitter satisfies subscription.AuditLogger and always errors on
// the in-tx emission — the fault injection the shared-fate direction demands:
// if the cancel's audit row cannot be written, the cancel must not commit.
// Log() (the residual own-tx seam used by other service methods) succeeds, so
// only the ADR-090 in-tx path is under test.
type failingCancelEmitter struct{}

func (failingCancelEmitter) Log(_ context.Context, _, _, _, _, _ string, _ map[string]any) error {
	return nil
}

func (failingCancelEmitter) LogInTx(_ context.Context, _ *sql.Tx, _ audit.Entry) error {
	return errors.New("injected audit failure")
}

// seedCancelAuditSub creates a customer + plan + ACTIVE subscription and
// returns the sub. Mirrors the scaffolding in
// cancel_atomic_rollback_integration_test.go.
func seedCancelAuditSub(t *testing.T, ctx context.Context, db *postgres.DB, tenantID, suffix string) domain.Subscription {
	t.Helper()

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_cancel_audit_" + suffix, DisplayName: "Cancel Audit " + suffix,
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	plan, err := pricing.NewPostgresStore(db).CreatePlan(ctx, tenantID, domain.Plan{
		Code: "cancel-audit-monthly-" + suffix, Name: "Cancel Audit " + suffix, Currency: "USD",
		BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInAdvance,
		BaseAmountCents: 5000, Status: domain.PlanActive,
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	sub, err := NewPostgresStore(db).Create(ctx, tenantID, domain.Subscription{
		Code: "sub-cancel-audit-" + suffix, DisplayName: "Cancel Audit " + suffix, CustomerID: cust.ID,
		Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
		CurrentBillingPeriodStart: bivPS(time.Now().UTC()), CurrentBillingPeriodEnd: bivPE(time.Now().UTC()),
		Items: []domain.SubscriptionItem{{PlanID: plan.ID, Quantity: 1}},
	})
	if err != nil {
		t.Fatalf("create sub: %v", err)
	}
	return sub
}

// Real-Postgres shared-fate coverage for the ADR-090 cancel emission
// (Service.Cancel → emitCancelAudit inside the CancelAtomicWithBill closure).
// The cancel flip and its audit row ride ONE transaction, so:
//   - a committed cancel ALWAYS carries exactly one 'cancel' row, and
//   - a failed emission rolls the cancel back (no canceled-but-unrecorded sub).
//
// Both services here are built WITHOUT a biller: the emission must not be
// conditioned on that unrelated dependency (the review finding this pins) —
// a biller-less Cancel still flips the sub, so it must still audit.
func TestCancelAudit_SharedFate(t *testing.T) {
	db := testutil.SetupTestDB(t)
	tenantID := testutil.CreateTestTenant(t, db, "Cancel InTx Audit")

	baseCtx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 60*time.Second)
	defer cancel()

	store := NewPostgresStore(db)
	logger := audit.NewLogger(db)

	t.Run("cancel commits the canceled status and its audit row together", func(t *testing.T) {
		// An API-key actor: cancelActorLabel maps api_key → "operator".
		ctx := auth.WithKeyID(baseCtx, "vlx_key_cancel_audit")
		sub := seedCancelAuditSub(t, ctx, db, tenantID, "ok")

		svc := NewService(store, nil)
		svc.SetAuditLogger(logger)

		canceled, credited, err := svc.Cancel(ctx, tenantID, sub.ID)
		if err != nil {
			t.Fatalf("cancel: %v", err)
		}
		if canceled.Status != domain.SubscriptionCanceled {
			t.Fatalf("status: got %q, want canceled", canceled.Status)
		}
		if credited != 0 {
			t.Errorf("credited: got %d, want 0 (no biller wired)", credited)
		}

		after, err := store.Get(ctx, tenantID, sub.ID)
		if err != nil {
			t.Fatalf("get after cancel: %v", err)
		}
		if after.Status != domain.SubscriptionCanceled {
			t.Fatalf("persisted status: got %q, want canceled", after.Status)
		}

		rows, _, err := logger.Query(ctx, tenantID, audit.QueryFilter{
			ResourceType: "subscription", ResourceID: sub.ID,
		})
		if err != nil {
			t.Fatalf("query audit: %v", err)
		}
		if len(rows) != 1 || rows[0].Action != domain.AuditActionCancel {
			t.Fatalf("want exactly one 'cancel' audit row for %s (emission must NOT be gated on the biller); got %+v", sub.ID, rows)
		}
		if rows[0].ResourceLabel != sub.Code {
			t.Errorf("resource_label: got %q, want %q", rows[0].ResourceLabel, sub.Code)
		}
		if rows[0].Metadata["customer_id"] != sub.CustomerID {
			t.Errorf("metadata customer_id: got %v, want %q", rows[0].Metadata["customer_id"], sub.CustomerID)
		}
		if rows[0].Metadata["canceled_by"] != "operator" {
			t.Errorf("metadata canceled_by: got %v, want \"operator\" (api_key actor)", rows[0].Metadata["canceled_by"])
		}
	})

	t.Run("a failed emission rolls the cancel back and leaves no audit row", func(t *testing.T) {
		ctx := auth.WithKeyID(baseCtx, "vlx_key_cancel_audit")
		sub := seedCancelAuditSub(t, ctx, db, tenantID, "fail")

		svc := NewService(store, nil)
		svc.SetAuditLogger(failingCancelEmitter{})

		if _, _, err := svc.Cancel(ctx, tenantID, sub.ID); err == nil {
			t.Fatal("cancel must fail when its audit emission fails (shared fate)")
		}

		after, err := store.Get(ctx, tenantID, sub.ID)
		if err != nil {
			t.Fatalf("get after rollback: %v", err)
		}
		if after.Status != domain.SubscriptionActive {
			t.Fatalf("subscription status: got %q, want active — a cancel committed without its audit row", after.Status)
		}
		if after.CanceledAt != nil {
			t.Errorf("canceled_at must be unset after rollback; got %v", after.CanceledAt)
		}

		rows, _, err := logger.Query(ctx, tenantID, audit.QueryFilter{
			ResourceType: "subscription", ResourceID: sub.ID,
		})
		if err != nil {
			t.Fatalf("query audit: %v", err)
		}
		if len(rows) != 0 {
			t.Errorf("phantom audit row for a rolled-back cancel: %+v", rows)
		}
	})

	// The audit row and the outbound subscription.canceled webhook are
	// written in the SAME transaction, so they must name the same canceler.
	// Pre-fix the webhook hardcoded "operator" while the audit row inferred
	// a background actor — one transaction, two contradictory claims.
	// Background machinery now STAMPS its identity (WithCancelOrigin) rather
	// than either writer guessing from the absence of a request actor.
	t.Run("background cancel: audit row and outbound webhook name the same canceler", func(t *testing.T) {
		cases := []struct {
			name string
			ctx  func(context.Context) context.Context
			want string
		}{
			{
				name: "dunning stamps its origin",
				ctx:  func(c context.Context) context.Context { return WithCancelOrigin(c, "dunning") },
				want: "dunning",
			},
			{
				// An unstamped background canceller is honestly "system" —
				// never a guess at whichever background path happens to
				// exist today.
				name: "unstamped background cancel is system, not a guess",
				ctx:  func(c context.Context) context.Context { return c },
				want: "system",
			},
		}
		for i, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				ctx := tc.ctx(baseCtx)
				sub := seedCancelAuditSub(t, ctx, db, tenantID, "origin"+string(rune('a'+i)))

				// The outbound subscription.canceled event is enqueued on the
				// transactional outbox inside the cancel tx — wire it so this
				// test can compare it against the audit row written in that
				// same tx.
				outboxStore := NewPostgresStore(db)
				outboxStore.SetOutboxEnqueuer(webhook.NewOutboxStore(db))

				svc := NewService(outboxStore, nil)
				svc.SetAuditLogger(logger)

				if _, _, err := svc.Cancel(ctx, tenantID, sub.ID); err != nil {
					t.Fatalf("cancel: %v", err)
				}

				rows, _, err := logger.Query(ctx, tenantID, audit.QueryFilter{
					ResourceType: "subscription", ResourceID: sub.ID,
				})
				if err != nil {
					t.Fatalf("query audit: %v", err)
				}
				if len(rows) != 1 {
					t.Fatalf("want one cancel audit row; got %+v", rows)
				}
				gotAudit, _ := rows[0].Metadata["canceled_by"].(string)
				if gotAudit != tc.want {
					t.Errorf("audit canceled_by: got %q, want %q", gotAudit, tc.want)
				}

				// The webhook event enqueued in the same tx must agree.
				gotEvent := canceledByOnOutboundEvent(t, ctx, db, tenantID, sub.ID)
				if gotEvent != tc.want {
					t.Errorf("webhook canceled_by: got %q, want %q", gotEvent, tc.want)
				}
				if gotEvent != gotAudit {
					t.Errorf("one transaction, two claims: audit says %q, webhook says %q", gotAudit, gotEvent)
				}
			})
		}
	})
}

// canceledByOnOutboundEvent reads canceled_by off the subscription.canceled
// event enqueued in the cancel transaction.
func canceledByOnOutboundEvent(t *testing.T, ctx context.Context, db *postgres.DB, tenantID, subID string) string {
	t.Helper()

	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin event read: %v", err)
	}
	defer postgres.Rollback(tx)

	// The lifecycle event is enqueued on the transactional outbox inside the
	// cancel tx (postgres.go enqueueLifecycle) — same table the
	// lifecycle-outbox suite reads.
	var canceledBy sql.NullString
	err = tx.QueryRowContext(ctx, `
		SELECT payload->>'canceled_by'
		FROM webhook_outbox
		WHERE event_type = $1
		  AND payload->>'subscription_id' = $2
		ORDER BY created_at DESC
		LIMIT 1
	`, string(domain.EventSubscriptionCanceled), subID).Scan(&canceledBy)
	if err != nil {
		t.Fatalf("read subscription.canceled outbox event (subscription %s): %v", subID, err)
	}
	return canceledBy.String
}
