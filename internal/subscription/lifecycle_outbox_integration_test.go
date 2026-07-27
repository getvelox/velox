package subscription_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/subscription"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/webhook"
)

// TestLifecycleEvents_EnqueuedInTransitionTx is the real-Postgres proof of
// the DispatchTx subscription subset (2026-07-05): subscription.created /
// .activated / .canceled / .trial_ended are enqueued into webhook_outbox
// INSIDE the transition tx by the store's writers — so a crash after commit
// can no longer silently drop a lifecycle event (a dropped emission left no
// row anywhere: nothing to replay, nothing to reconcile). Also proves the
// atomicity direction (a failing billFn rolls the event back with the
// cancel) and the CAS exactly-once property (a losing duplicate transition
// enqueues nothing).
func TestLifecycleEvents_EnqueuedInTransitionTx(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Lifecycle Outbox")

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_lifecycle_obx", DisplayName: "Lifecycle Obx",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	plan, err := pricing.NewPostgresStore(db).CreatePlan(ctx, tenantID, domain.Plan{
		Code: "lifecycle-monthly", Name: "Lifecycle", Currency: "USD",
		BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInArrears,
		BaseAmountCents: 900, Status: domain.PlanActive,
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}

	store := subscription.NewPostgresStore(db)
	store.SetOutboxEnqueuer(webhook.NewOutboxStore(db))

	// events returns (count, lastPayload) for an event type on this tenant.
	events := func(eventType string) (int, map[string]any) {
		t.Helper()
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin read tx: %v", err)
		}
		defer postgres.Rollback(tx)
		rows, err := tx.QueryContext(ctx,
			`SELECT payload FROM webhook_outbox WHERE event_type = $1 ORDER BY created_at`, eventType)
		if err != nil {
			t.Fatalf("query outbox: %v", err)
		}
		defer func() { _ = rows.Close() }()
		var n int
		var last map[string]any
		for rows.Next() {
			var raw []byte
			if err := rows.Scan(&raw); err != nil {
				t.Fatalf("scan payload: %v", err)
			}
			last = nil
			if err := json.Unmarshal(raw, &last); err != nil {
				t.Fatalf("decode payload: %v", err)
			}
			n++
		}
		return n, last
	}

	newSub := func(code string, status domain.SubscriptionStatus, mutate func(*domain.Subscription)) domain.Subscription {
		t.Helper()
		s := domain.Subscription{
			Code: code, DisplayName: code, CustomerID: cust.ID,
			Status: status, BillingTime: domain.BillingTimeAnniversary,
			Items: []domain.SubscriptionItem{{PlanID: plan.ID, Quantity: 1}},
		}
		// Real creates stamp the billing period for both shapes: StartNow
		// actives carry the current period, trialing subs carry the
		// post-trial one (firstPeriodAfterTrial) — and the ADR-101
		// activation writers require it.
		if status == domain.SubscriptionActive || status == domain.SubscriptionTrialing {
			s.CurrentBillingPeriodStart = bivPS(time.Now().UTC())
			s.CurrentBillingPeriodEnd = bivPE(time.Now().UTC())
		}
		if mutate != nil {
			mutate(&s)
		}
		created, err := store.Create(ctx, tenantID, s)
		if err != nil {
			t.Fatalf("create %s: %v", code, err)
		}
		return created
	}

	// (1) created — rides the create tx.
	sub := newSub("lc-created", domain.SubscriptionDraft, nil)
	if n, p := events(domain.EventSubscriptionCreated); n != 1 || p["subscription_id"] != sub.ID {
		t.Fatalf("created: got n=%d payload=%v", n, p)
	}

	// (2) activated — ActivateDraftWithBill is Activate's draft→active
	// writer; the event rides its tx (the enqueue once lived in a store
	// Update method that lost its last caller, so this event silently
	// stopped firing while this test stayed green against the dead path).
	actAt := time.Date(2027, 4, 1, 0, 0, 0, 0, time.UTC)
	if _, err := store.ActivateDraftWithBill(ctx, tenantID, sub.ID, actAt, actAt, actAt.AddDate(0, 1, 0), 1, nil); err != nil {
		t.Fatalf("activate: %v", err)
	}
	if n, p := events(domain.EventSubscriptionActivated); n != 1 || p["status"] != "active" {
		t.Fatalf("activated: got n=%d payload=%v", n, p)
	}

	// (3) canceled (operator) — CancelAtomic; a second cancel (CAS loser)
	// must not enqueue a second event. canceled_by is no longer hardcoded:
	// it derives from the request identity (cancelActorLabel), the same rule
	// the ADR-090 audit row uses, so the webhook and the audit evidence in
	// one tx can't disagree. An OPERATOR cancel is therefore one whose ctx
	// carries operator identity — a bare ctx is a background actor.
	opCtx := auth.WithKeyID(ctx, "vlx_key_lifecycle_obx")
	if _, err := store.CancelAtomic(opCtx, tenantID, sub.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := store.CancelAtomic(opCtx, tenantID, sub.ID); err == nil {
		t.Fatal("second cancel should fail (already canceled)")
	}
	if n, p := events(domain.EventSubscriptionCanceled); n != 1 || p["canceled_by"] != "operator" {
		t.Fatalf("canceled(operator): got n=%d payload=%v", n, p)
	}

	// (4) canceled (schedule) — FireScheduledCancellation on an active sub.
	subB := newSub("lc-sched", domain.SubscriptionActive, nil)
	if _, err := store.FireScheduledCancellation(ctx, tenantID, subB.ID, time.Now().UTC()); err != nil {
		t.Fatalf("fire scheduled cancel: %v", err)
	}
	if n, p := events(domain.EventSubscriptionCanceled); n != 2 || p["canceled_by"] != "schedule" {
		t.Fatalf("canceled(schedule): got n=%d payload=%v", n, p)
	}

	// (5) canceled (trial_end_cancel) — CancelAtTrialEnd on a trialing sub
	// carrying a due schedule.
	trialEnd := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	subC := newSub("lc-trialcancel", domain.SubscriptionTrialing, func(s *domain.Subscription) {
		s.TrialEndAt = &trialEnd
	})
	// The schedule flag is persisted by ScheduleCancellation, not create.
	if _, err := store.ScheduleCancellation(ctx, tenantID, subC.ID, nil, true, domain.SubscriptionTrialing); err != nil {
		t.Fatalf("schedule cancel: %v", err)
	}
	if _, err := store.CancelAtTrialEnd(ctx, tenantID, subC.ID, trialEnd); err != nil {
		t.Fatalf("cancel at trial end: %v", err)
	}
	if n, p := events(domain.EventSubscriptionCanceled); n != 3 || p["reason"] != "trial_end_cancel" {
		t.Fatalf("canceled(trial_end): got n=%d payload=%v", n, p)
	}

	// (6) trial_ended (schedule) — ActivateAfterTrial.
	subD := newSub("lc-trialflip", domain.SubscriptionTrialing, func(s *domain.Subscription) {
		s.TrialEndAt = &trialEnd
	})
	if _, err := store.ActivateAfterTrial(ctx, tenantID, subD.ID, time.Now().UTC()); err != nil {
		t.Fatalf("activate after trial: %v", err)
	}
	if n, p := events(domain.EventSubscriptionTrialEnded); n != 1 || p["triggered_by"] != "schedule" {
		t.Fatalf("trial_ended(schedule): got n=%d payload=%v", n, p)
	}

	// (7) trial_ended (operator) — EndTrialEarly.
	subE := newSub("lc-trialearly", domain.SubscriptionTrialing, func(s *domain.Subscription) {
		future := time.Now().UTC().Add(72 * time.Hour)
		s.TrialEndAt = &future
	})
	now := time.Now().UTC()
	if _, err := store.EndTrialEarly(ctx, tenantID, subE.ID, now, now, now.AddDate(0, 1, 0), now.AddDate(0, 1, 0), 0); err != nil {
		t.Fatalf("end trial early: %v", err)
	}
	if n, p := events(domain.EventSubscriptionTrialEnded); n != 2 || p["triggered_by"] != "operator" {
		t.Fatalf("trial_ended(operator): got n=%d payload=%v", n, p)
	}

	// (8) Atomicity: a failing billFn inside CancelAtomicWithBill rolls the
	// cancel AND its event back together.
	subF := newSub("lc-rollback", domain.SubscriptionActive, nil)
	sentinel := errors.New("bill failed")
	if _, err := store.CancelAtomicWithBill(ctx, tenantID, subF.ID, func(*sql.Tx, domain.Subscription) error {
		return sentinel
	}); !errors.Is(err, sentinel) {
		t.Fatalf("cancel-with-bill: got %v, want sentinel", err)
	}
	if n, _ := events(domain.EventSubscriptionCanceled); n != 3 {
		t.Fatalf("rolled-back cancel leaked an event: canceled count = %d, want 3", n)
	}
	got, err := store.Get(ctx, tenantID, subF.ID)
	if err != nil || got.Status != domain.SubscriptionActive {
		t.Fatalf("subF must remain active after rollback: status=%v err=%v", got.Status, err)
	}
}
