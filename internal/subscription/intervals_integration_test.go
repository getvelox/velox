package subscription

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// ADR-101 Phase 1 integration tests: every item-mutation writer must
// dual-write its billing interval in the same tx, and the table's
// constraints must turn writer bugs into loud tx failures. Real
// Postgres — the exclusion constraints and the set_livemode/RLS
// plumbing ARE the subject.

var (
	ivPS = time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	ivPE = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
)

type ivRead struct {
	planID   string
	quantity int64
	startsAt time.Time
	endsAt   *time.Time
	source   string
}

func readItemIntervals(t *testing.T, db *postgres.DB, ctx context.Context, tenantID, itemID string) []ivRead {
	t.Helper()
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	rows, err := tx.QueryContext(ctx, `
		SELECT plan_id, quantity, starts_at, ends_at, source
		FROM billing_intervals WHERE subscription_item_id = $1 ORDER BY starts_at, created_at`, itemID)
	if err != nil {
		t.Fatalf("read intervals: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ivRead
	for rows.Next() {
		var r ivRead
		if err := rows.Scan(&r.planID, &r.quantity, &r.startsAt, &r.endsAt, &r.source); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

func ivPlan(t *testing.T, db *postgres.DB, ctx context.Context, tenantID, code string, cents int64) domain.Plan {
	t.Helper()
	p, err := pricing.NewPostgresStore(db).CreatePlan(ctx, tenantID, domain.Plan{
		Code: code, Name: code, Currency: "USD",
		BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInAdvance,
		BaseAmountCents: cents, Status: domain.PlanActive,
	})
	if err != nil {
		t.Fatalf("create plan %s: %v", code, err)
	}
	return p
}

func ivCustomer(t *testing.T, db *postgres.DB, ctx context.Context, tenantID, ext string) string {
	t.Helper()
	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: ext, DisplayName: ext,
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	return cust.ID
}

// ivEndEq is the nil-safe ends_at comparison.
func ivEndEq(p *time.Time, want time.Time) bool { return p != nil && p.Equal(want) }

// bivPS/bivPE — ADR-101 fixture stamps for active-sub literals in this
// package's other integration tests (the interval writers refuse an
// active sub with no billing period).
func bivPS(t time.Time) *time.Time { return &t }

func bivPE(t time.Time) *time.Time {
	e := t.AddDate(0, 1, 0)
	return &e
}

func ivActiveSub(t *testing.T, store *PostgresStore, ctx context.Context, tenantID, custID, code string, items []domain.SubscriptionItem) domain.Subscription {
	t.Helper()
	// Pin the create to the period start so the items' created_at (and the
	// trigger's 'add' fact rows) land at ivPS, matching the fixture period
	// — a wall-clock create would strand the 'add' months after ivPE and
	// make the backfill replay assertions meaningless.
	sub, err := store.Create(clock.WithEffectiveNow(ctx, ivPS), tenantID, domain.Subscription{
		Code: code, DisplayName: code, CustomerID: custID,
		Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
		StartedAt:                 &ivPS,
		CurrentBillingPeriodStart: &ivPS,
		CurrentBillingPeriodEnd:   &ivPE,
		NextBillingAt:             &ivPE,
		Items:                     items,
	})
	if err != nil {
		t.Fatalf("create sub %s: %v", code, err)
	}
	return sub
}

func TestIntervals_CreateActiveOpensPerItem(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "IV Create")
	cust := ivCustomer(t, db, ctx, tenantID, "cus_iv_create")
	planA := ivPlan(t, db, ctx, tenantID, "iv-create-a", 1000)
	planB := ivPlan(t, db, ctx, tenantID, "iv-create-b", 2000)
	store := NewPostgresStore(db)

	sub := ivActiveSub(t, store, ctx, tenantID, cust, "sub-iv-create", []domain.SubscriptionItem{
		{PlanID: planA.ID, Quantity: 2}, {PlanID: planB.ID, Quantity: 1},
	})

	for _, it := range sub.Items {
		ivs := readItemIntervals(t, db, ctx, tenantID, it.ID)
		if len(ivs) != 1 {
			t.Fatalf("item %s: want 1 interval, got %d", it.ID, len(ivs))
		}
		iv := ivs[0]
		if !iv.startsAt.Equal(ivPS) || iv.endsAt != nil || iv.source != "create" || iv.planID != it.PlanID || iv.quantity != it.Quantity {
			t.Errorf("item %s: unexpected interval %+v", it.ID, iv)
		}
	}
}

func TestIntervals_DraftOpensNothing_ActivateOpens(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "IV Draft")
	cust := ivCustomer(t, db, ctx, tenantID, "cus_iv_draft")
	planA := ivPlan(t, db, ctx, tenantID, "iv-draft-a", 1000)
	store := NewPostgresStore(db)

	sub, err := store.Create(ctx, tenantID, domain.Subscription{
		Code: "sub-iv-draft", DisplayName: "d", CustomerID: cust,
		Status: domain.SubscriptionDraft, BillingTime: domain.BillingTimeCalendar,
		Items: []domain.SubscriptionItem{{PlanID: planA.ID, Quantity: 3}},
	})
	if err != nil {
		t.Fatalf("create draft: %v", err)
	}
	itemID := sub.Items[0].ID
	if got := readItemIntervals(t, db, ctx, tenantID, itemID); len(got) != 0 {
		t.Fatalf("draft create must open nothing, got %d intervals", len(got))
	}

	at := ivPS.Add(26 * time.Hour)
	if _, err := store.ActivateDraftWithBill(ctx, tenantID, sub.ID, at, ivPS, ivPE, 1, nil); err != nil {
		t.Fatalf("activate draft: %v", err)
	}
	ivs := readItemIntervals(t, db, ctx, tenantID, itemID)
	if len(ivs) != 1 || !ivs[0].startsAt.Equal(ivPS) || ivs[0].endsAt != nil || ivs[0].source != "activate" || ivs[0].quantity != 3 {
		t.Fatalf("activation must open at the period start: %+v", ivs)
	}
}

func TestIntervals_TrialOpensNothing_ConvertOpensAtStampedPeriod(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "IV Trial")
	cust := ivCustomer(t, db, ctx, tenantID, "cus_iv_trial")
	planA := ivPlan(t, db, ctx, tenantID, "iv-trial-a", 1000)
	store := NewPostgresStore(db)

	trialStart := ivPS
	trialEnd := ivPS.AddDate(0, 0, 14)
	postPE := trialEnd.AddDate(0, 1, 0)
	sub, err := store.Create(ctx, tenantID, domain.Subscription{
		Code: "sub-iv-trial", DisplayName: "t", CustomerID: cust,
		Status: domain.SubscriptionTrialing, BillingTime: domain.BillingTimeAnniversary,
		TrialStartAt: &trialStart, TrialEndAt: &trialEnd, StartedAt: &trialStart,
		CurrentBillingPeriodStart: &trialEnd, CurrentBillingPeriodEnd: &postPE, NextBillingAt: &postPE,
		Items: []domain.SubscriptionItem{{PlanID: planA.ID, Quantity: 1}},
	})
	if err != nil {
		t.Fatalf("create trialing: %v", err)
	}
	itemID := sub.Items[0].ID
	if got := readItemIntervals(t, db, ctx, tenantID, itemID); len(got) != 0 {
		t.Fatalf("trialing create must open nothing, got %d", len(got))
	}

	if _, err := store.ActivateAfterTrial(ctx, tenantID, sub.ID, trialEnd); err != nil {
		t.Fatalf("activate after trial: %v", err)
	}
	ivs := readItemIntervals(t, db, ctx, tenantID, itemID)
	if len(ivs) != 1 || !ivs[0].startsAt.Equal(trialEnd) || ivs[0].source != "trial_convert" {
		t.Fatalf("trial conversion must open at the stamped post-trial period start: %+v", ivs)
	}
}

func TestIntervals_AddItemDayGradeClamp(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "IV AddClamp")
	cust := ivCustomer(t, db, ctx, tenantID, "cus_iv_add")
	planA := ivPlan(t, db, ctx, tenantID, "iv-add-a", 1000)
	planB := ivPlan(t, db, ctx, tenantID, "iv-add-b", 2000)
	planC := ivPlan(t, db, ctx, tenantID, "iv-add-c", 3000)
	store := NewPostgresStore(db)
	sub := ivActiveSub(t, store, ctx, tenantID, cust, "sub-iv-add", []domain.SubscriptionItem{{PlanID: planA.ID, Quantity: 1}})

	// Same calendar day as the period start (tenant TZ = UTC default):
	// the open clamps to the period start itself (ADR-012 amendment).
	sameDay := clock.WithEffectiveNow(ctx, ivPS.Add(14*time.Hour))
	itB, err := store.AddItem(sameDay, tenantID, domain.SubscriptionItem{SubscriptionID: sub.ID, PlanID: planB.ID, Quantity: 2})
	if err != nil {
		t.Fatalf("add same-day item: %v", err)
	}
	ivs := readItemIntervals(t, db, ctx, tenantID, itB.ID)
	if len(ivs) != 1 || !ivs[0].startsAt.Equal(ivPS) || ivs[0].source != "add" {
		t.Fatalf("same-day add must clamp to period start %s: %+v", ivPS, ivs)
	}

	// Mid-period add keeps its raw instant.
	day5 := ivPS.Add(5*24*time.Hour + 9*time.Hour)
	itC, err := store.AddItem(clock.WithEffectiveNow(ctx, day5), tenantID, domain.SubscriptionItem{SubscriptionID: sub.ID, PlanID: planC.ID, Quantity: 1})
	if err != nil {
		t.Fatalf("add mid-period item: %v", err)
	}
	ivs = readItemIntervals(t, db, ctx, tenantID, itC.ID)
	if len(ivs) != 1 || !ivs[0].startsAt.Equal(day5) {
		t.Fatalf("mid-period add must open at its raw instant %s: %+v", day5, ivs)
	}
}

func TestIntervals_QuantityAndPlanTransitions(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "IV Trans")
	cust := ivCustomer(t, db, ctx, tenantID, "cus_iv_trans")
	planA := ivPlan(t, db, ctx, tenantID, "iv-trans-a", 1000)
	planB := ivPlan(t, db, ctx, tenantID, "iv-trans-b", 2000)
	store := NewPostgresStore(db)
	sub := ivActiveSub(t, store, ctx, tenantID, cust, "sub-iv-trans", []domain.SubscriptionItem{{PlanID: planA.ID, Quantity: 1}})
	itemID := sub.Items[0].ID

	t1 := ivPS.Add(5 * 24 * time.Hour)
	if _, err := store.UpdateItemQuantity(clock.WithEffectiveNow(ctx, t1), tenantID, itemID, 4); err != nil {
		t.Fatalf("qty change: %v", err)
	}
	t2 := ivPS.Add(10 * 24 * time.Hour)
	if _, err := store.ApplyItemPlanImmediately(ctx, tenantID, itemID, planB.ID, t2); err != nil {
		t.Fatalf("plan change: %v", err)
	}

	ivs := readItemIntervals(t, db, ctx, tenantID, itemID)
	if len(ivs) != 3 {
		t.Fatalf("want 3 intervals after qty+plan, got %d: %+v", len(ivs), ivs)
	}
	assertSeg := func(i int, plan string, qty int64, start time.Time, end *time.Time, source string) {
		t.Helper()
		iv := ivs[i]
		if iv.planID != plan || iv.quantity != qty || !iv.startsAt.Equal(start) || iv.source != source ||
			(end == nil) != (iv.endsAt == nil) || (end != nil && !iv.endsAt.Equal(*end)) {
			t.Errorf("seg %d: got %+v, want plan=%s qty=%d [%s,%v) source=%s", i, iv, plan, qty, start, end, source)
		}
	}
	assertSeg(0, planA.ID, 1, ivPS, &t1, "create")
	assertSeg(1, planA.ID, 4, t1, &t2, "quantity")
	assertSeg(2, planB.ID, 4, t2, nil, "plan")

	// Same-value quantity write mints no interval churn.
	if _, err := store.UpdateItemQuantity(clock.WithEffectiveNow(ctx, t2.Add(time.Hour)), tenantID, itemID, 4); err != nil {
		t.Fatalf("no-op qty: %v", err)
	}
	if got := readItemIntervals(t, db, ctx, tenantID, itemID); len(got) != 3 {
		t.Fatalf("no-op quantity must not add intervals: got %d", len(got))
	}
}

func TestIntervals_ScheduledPlanApply_RetroactiveSplice(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "IV Splice")
	cust := ivCustomer(t, db, ctx, tenantID, "cus_iv_splice")
	planA := ivPlan(t, db, ctx, tenantID, "iv-splice-a", 1000)
	planB := ivPlan(t, db, ctx, tenantID, "iv-splice-b", 2000)
	store := NewPostgresStore(db)
	sub := ivActiveSub(t, store, ctx, tenantID, cust, "sub-iv-splice", []domain.SubscriptionItem{{PlanID: planA.ID, Quantity: 1}})
	itemID := sub.Items[0].ID

	// Schedule planB effective t2, then a LATER quantity change lands at
	// t3 before the apply fires (engine-down interleave): the apply at t2
	// must splice the sealed [ps, t3) interval and re-plan the successor.
	t2 := ivPS.Add(10 * 24 * time.Hour)
	t3 := ivPS.Add(20 * 24 * time.Hour)
	if _, err := store.SetItemPendingPlan(ctx, tenantID, itemID, planB.ID, t2); err != nil {
		t.Fatalf("schedule plan: %v", err)
	}
	if _, err := store.UpdateItemQuantity(clock.WithEffectiveNow(ctx, t3), tenantID, itemID, 7); err != nil {
		t.Fatalf("qty change: %v", err)
	}
	applied, err := store.ApplyDuePendingItemPlansAtomic(ctx, tenantID, sub.ID, t2)
	if err != nil {
		t.Fatalf("apply due pendings: %v", err)
	}
	if len(applied) != 1 || applied[0].PlanID != planB.ID {
		t.Fatalf("apply must swap to planB: %+v", applied)
	}

	ivs := readItemIntervals(t, db, ctx, tenantID, itemID)
	if len(ivs) != 3 {
		t.Fatalf("want 3 intervals after splice, got %d: %+v", len(ivs), ivs)
	}
	if ivs[0].planID != planA.ID || ivs[0].quantity != 1 || !ivs[0].startsAt.Equal(ivPS) || !ivEndEq(ivs[0].endsAt, t2) {
		t.Errorf("seg 0 must stay planA qty1 [ps,t2): %+v", ivs[0])
	}
	if ivs[1].planID != planB.ID || ivs[1].quantity != 1 || !ivs[1].startsAt.Equal(t2) || !ivEndEq(ivs[1].endsAt, t3) {
		t.Errorf("seg 1 must be planB qty1 [t2,t3): %+v", ivs[1])
	}
	if ivs[2].planID != planB.ID || ivs[2].quantity != 7 || !ivs[2].startsAt.Equal(t3) || ivs[2].endsAt != nil {
		t.Errorf("seg 2 must be re-planned planB qty7 [t3,∞): %+v", ivs[2])
	}
}

func TestIntervals_RemoveSealsAtDeleteInstant(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "IV Remove")
	cust := ivCustomer(t, db, ctx, tenantID, "cus_iv_rm")
	planA := ivPlan(t, db, ctx, tenantID, "iv-rm-a", 1000)
	planB := ivPlan(t, db, ctx, tenantID, "iv-rm-b", 2000)
	store := NewPostgresStore(db)
	sub := ivActiveSub(t, store, ctx, tenantID, cust, "sub-iv-rm", []domain.SubscriptionItem{
		{PlanID: planA.ID, Quantity: 1}, {PlanID: planB.ID, Quantity: 1},
	})
	itemID := sub.Items[1].ID

	at := ivPS.Add(12 * 24 * time.Hour)
	if err := store.RemoveItem(clock.WithEffectiveNow(ctx, at), tenantID, itemID); err != nil {
		t.Fatalf("remove: %v", err)
	}
	ivs := readItemIntervals(t, db, ctx, tenantID, itemID)
	if len(ivs) != 1 || !ivEndEq(ivs[0].endsAt, at) {
		t.Fatalf("remove must seal the interval at the delete instant: %+v", ivs)
	}
}

func TestIntervals_CancelClosesAll(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "IV Cancel")
	cust := ivCustomer(t, db, ctx, tenantID, "cus_iv_cxl")
	planA := ivPlan(t, db, ctx, tenantID, "iv-cxl-a", 1000)
	planB := ivPlan(t, db, ctx, tenantID, "iv-cxl-b", 2000)
	store := NewPostgresStore(db)

	// Immediate mid-period cancel: every open interval seals at the
	// cancel instant.
	sub := ivActiveSub(t, store, ctx, tenantID, cust, "sub-iv-cxl1", []domain.SubscriptionItem{
		{PlanID: planA.ID, Quantity: 1}, {PlanID: planB.ID, Quantity: 2},
	})
	at := ivPS.Add(15 * 24 * time.Hour)
	if _, err := store.CancelAtomic(clock.WithEffectiveNow(ctx, at), tenantID, sub.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	for _, it := range sub.Items {
		ivs := readItemIntervals(t, db, ctx, tenantID, it.ID)
		if len(ivs) != 1 || !ivEndEq(ivs[0].endsAt, at) {
			t.Fatalf("immediate cancel must seal item %s at %s: %+v", it.ID, at, ivs)
		}
	}

	// Cancel firing past the period end caps at the period end.
	sub2 := ivActiveSub(t, store, ctx, tenantID, cust, "sub-iv-cxl2", []domain.SubscriptionItem{{PlanID: planA.ID, Quantity: 1}})
	late := ivPE.Add(48 * time.Hour)
	if _, err := store.CancelAtomic(clock.WithEffectiveNow(ctx, late), tenantID, sub2.ID); err != nil {
		t.Fatalf("late cancel: %v", err)
	}
	ivs := readItemIntervals(t, db, ctx, tenantID, sub2.Items[0].ID)
	if len(ivs) != 1 || !ivEndEq(ivs[0].endsAt, ivPE) {
		t.Fatalf("late cancel must cap the seal at period end %s: %+v", ivPE, ivs)
	}

	// A backdated fire (ADR-097 contracted-instant shape) predating the
	// open zero-widths the interval instead of minting a negative range.
	sub3 := ivActiveSub(t, store, ctx, tenantID, cust, "sub-iv-cxl3", []domain.SubscriptionItem{{PlanID: planA.ID, Quantity: 1}})
	fresh3, err := store.Get(ctx, tenantID, sub3.ID)
	if err != nil {
		t.Fatalf("get sub3: %v", err)
	}
	if err := store.WithTenantTx(ctx, tenantID, func(tx *sql.Tx) error {
		_, err := store.FireScheduledCancellationTx(ctx, tx, tenantID, sub3.ID, SnapshotOf(fresh3), ivPS.Add(-time.Hour))
		return err
	}); err != nil {
		t.Fatalf("backdated fire: %v", err)
	}
	ivs = readItemIntervals(t, db, ctx, tenantID, sub3.Items[0].ID)
	if len(ivs) != 1 || !ivEndEq(ivs[0].endsAt, ivs[0].startsAt) {
		t.Fatalf("backdated cancel must zero-width the interval: %+v", ivs)
	}
}

func TestIntervals_ConstraintsRejectWriterBugs(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "IV Constraints")
	cust := ivCustomer(t, db, ctx, tenantID, "cus_iv_con")
	planA := ivPlan(t, db, ctx, tenantID, "iv-con-a", 1000)
	store := NewPostgresStore(db)
	sub := ivActiveSub(t, store, ctx, tenantID, cust, "sub-iv-con", []domain.SubscriptionItem{{PlanID: planA.ID, Quantity: 1}})
	itemID := sub.Items[0].ID

	exec := func(q string, args ...any) error {
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer postgres.Rollback(tx)
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			return err
		}
		return tx.Commit()
	}

	// Second open interval for the same item — one_open must reject.
	if err := exec(`INSERT INTO billing_intervals (tenant_id, subscription_id, subscription_item_id, plan_id, quantity, starts_at, source)
		VALUES ($1,$2,$3,$4,1,$5,'test')`, tenantID, sub.ID, itemID, planA.ID, ivPS.Add(time.Hour)); err == nil {
		t.Error("second open interval must violate billing_intervals_one_open")
	}
	// Range overlapping the open interval — no_overlap must reject.
	if err := exec(`INSERT INTO billing_intervals (tenant_id, subscription_id, subscription_item_id, plan_id, quantity, starts_at, ends_at, source)
		VALUES ($1,$2,$3,$4,1,$5,$6,'test')`, tenantID, sub.ID, itemID, planA.ID, ivPS.Add(time.Hour), ivPS.Add(2*time.Hour)); err == nil {
		t.Error("overlapping range must violate billing_intervals_no_overlap")
	}
	// ends before starts — range_check must reject.
	if err := exec(`INSERT INTO billing_intervals (tenant_id, subscription_id, subscription_item_id, plan_id, quantity, starts_at, ends_at, source)
		VALUES ($1,$2,'it_other',$3,1,$4,$5,'test')`, tenantID, sub.ID, planA.ID, ivPS, ivPS.Add(-time.Minute)); err == nil {
		t.Error("negative range must violate billing_intervals_range_check")
	}
}

func TestIntervals_UndeleteRefusedLoudly(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "IV Undelete")
	cust := ivCustomer(t, db, ctx, tenantID, "cus_iv_und")
	planA := ivPlan(t, db, ctx, tenantID, "iv-und-a", 1000)
	planB := ivPlan(t, db, ctx, tenantID, "iv-und-b", 2000)
	store := NewPostgresStore(db)
	sub := ivActiveSub(t, store, ctx, tenantID, cust, "sub-iv-und", []domain.SubscriptionItem{
		{PlanID: planA.ID, Quantity: 1}, {PlanID: planB.ID, Quantity: 1},
	})
	itemID := sub.Items[1].ID
	if err := store.RemoveItem(ctx, tenantID, itemID); err != nil {
		t.Fatalf("remove: %v", err)
	}

	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	_, err = tx.ExecContext(ctx, `UPDATE subscription_items SET deleted_at = NULL WHERE id = $1`, itemID)
	if err == nil || !strings.Contains(err.Error(), "ADR-101") {
		t.Fatalf("un-delete must be refused by the trigger, got: %v", err)
	}
}

func TestIntervals_BackfillReconstructsHistory(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "IV Backfill")
	cust := ivCustomer(t, db, ctx, tenantID, "cus_iv_bf")
	planA := ivPlan(t, db, ctx, tenantID, "iv-bf-a", 1000)
	planB := ivPlan(t, db, ctx, tenantID, "iv-bf-b", 2000)
	store := NewPostgresStore(db)

	// Build real history through the live writers (add → qty → plan →
	// remove on a second item), then erase the dual-written intervals to
	// simulate a pre-ADR-101 database.
	sub := ivActiveSub(t, store, ctx, tenantID, cust, "sub-iv-bf", []domain.SubscriptionItem{{PlanID: planA.ID, Quantity: 1}})
	itemID := sub.Items[0].ID
	t1 := ivPS.Add(5 * 24 * time.Hour)
	t2 := ivPS.Add(10 * 24 * time.Hour)
	if _, err := store.UpdateItemQuantity(clock.WithEffectiveNow(ctx, t1), tenantID, itemID, 4); err != nil {
		t.Fatalf("qty: %v", err)
	}
	if _, err := store.ApplyItemPlanImmediately(ctx, tenantID, itemID, planB.ID, t2); err != nil {
		t.Fatalf("plan: %v", err)
	}

	wipe := func() {
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer postgres.Rollback(tx)
		if _, err := tx.ExecContext(ctx, `DELETE FROM billing_intervals WHERE subscription_id = $1`, sub.ID); err != nil {
			t.Fatalf("wipe intervals: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}
	wipe()

	n, err := store.BackfillBillingIntervals(ctx)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if n != 3 {
		t.Fatalf("backfill must insert 3 segments, got %d", n)
	}
	ivs := readItemIntervals(t, db, ctx, tenantID, itemID)
	if len(ivs) != 3 {
		t.Fatalf("backfill must yield 3 segments: %+v", ivs)
	}
	if ivs[0].planID != planA.ID || ivs[0].quantity != 1 || !ivs[0].startsAt.Equal(ivPS) || !ivEndEq(ivs[0].endsAt, t1) {
		t.Fatalf("backfill seg 0 mismatch: %+v", ivs[0])
	}
	if ivs[1].planID != planA.ID || ivs[1].quantity != 4 || !ivs[1].startsAt.Equal(t1) || !ivEndEq(ivs[1].endsAt, t2) {
		t.Fatalf("backfill seg 1 mismatch: %+v", ivs[1])
	}
	if ivs[2].planID != planB.ID || ivs[2].quantity != 4 || !ivs[2].startsAt.Equal(t2) || ivs[2].endsAt != nil {
		t.Fatalf("backfill seg 2 mismatch: %+v", ivs[2])
	}
	for _, iv := range ivs {
		if iv.source != "backfill" {
			t.Errorf("backfilled row must be source=backfill: %+v", iv)
		}
	}
	// Note ivs[0] starts at ivPS: the item's 'add' fact row is stamped at
	// the sub-creation instant, which the walk emits verbatim — here the
	// fixture creates at ivPS exactly, so no clamp is needed.

	// Idempotent: second run inserts nothing.
	if n2, err := store.BackfillBillingIntervals(ctx); err != nil || n2 != 0 {
		t.Fatalf("second backfill must be a no-op, got n=%d err=%v", n2, err)
	}
}

func TestIntervals_BackfillLogGaps(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "IV BF Gaps")
	cust := ivCustomer(t, db, ctx, tenantID, "cus_iv_bfg")
	planA := ivPlan(t, db, ctx, tenantID, "iv-bfg-a", 1000)
	planB := ivPlan(t, db, ctx, tenantID, "iv-bfg-b", 2000)
	store := NewPostgresStore(db)

	sub := ivActiveSub(t, store, ctx, tenantID, cust, "sub-iv-bfg", []domain.SubscriptionItem{
		{PlanID: planA.ID, Quantity: 2}, {PlanID: planB.ID, Quantity: 1},
	})
	preItem := sub.Items[0].ID  // will become the pre-0029 shape (no 'add' row)
	goneItem := sub.Items[1].ID // will become the 0102→0129 shape (removed, no 'remove' row)

	t1 := ivPS.Add(8 * 24 * time.Hour)
	if _, err := store.UpdateItemQuantity(clock.WithEffectiveNow(ctx, t1), tenantID, preItem, 5); err != nil {
		t.Fatalf("qty: %v", err)
	}
	tDel := ivPS.Add(12 * 24 * time.Hour)
	if err := store.RemoveItem(clock.WithEffectiveNow(ctx, tDel), tenantID, goneItem); err != nil {
		t.Fatalf("remove: %v", err)
	}

	// Simulate the historical gaps: drop the 'add' row for preItem and the
	// 'remove' row for goneItem, and erase the dual-written intervals.
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	for q, args := range map[string][]any{
		`DELETE FROM subscription_item_changes WHERE subscription_item_id = $1 AND change_type = 'add'`:    {preItem},
		`DELETE FROM subscription_item_changes WHERE subscription_item_id = $1 AND change_type = 'remove'`: {goneItem},
		`DELETE FROM billing_intervals WHERE subscription_id = $1`:                                         {sub.ID},
	} {
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("fixture %s: %v", q, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if _, err := store.BackfillBillingIntervals(ctx); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	// preItem: synthetic open at created_at with the qty row's from-state.
	pre := readItemIntervals(t, db, ctx, tenantID, preItem)
	if len(pre) != 2 {
		t.Fatalf("pre-0029 reconstruction must yield 2 segments: %+v", pre)
	}
	if pre[0].planID != planA.ID || pre[0].quantity != 2 || !pre[0].startsAt.Equal(ivPS) || !ivEndEq(pre[0].endsAt, t1) {
		t.Fatalf("pre-0029 seg 0 mismatch: %+v", pre[0])
	}
	if pre[1].quantity != 5 || pre[1].endsAt != nil {
		t.Fatalf("pre-0029 seg 1 mismatch: %+v", pre[1])
	}
	// goneItem: sealed at deleted_at despite the missing 'remove' row.
	gone := readItemIntervals(t, db, ctx, tenantID, goneItem)
	if len(gone) != 1 || !ivEndEq(gone[0].endsAt, tDel) {
		t.Fatalf("remove-gap reconstruction mismatch: %+v", gone)
	}
}

func TestIntervals_BackfillCanceledSubSeals(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "IV BF Cancel")
	cust := ivCustomer(t, db, ctx, tenantID, "cus_iv_bfc")
	planA := ivPlan(t, db, ctx, tenantID, "iv-bfc-a", 1000)
	store := NewPostgresStore(db)

	sub := ivActiveSub(t, store, ctx, tenantID, cust, "sub-iv-bfc", []domain.SubscriptionItem{{PlanID: planA.ID, Quantity: 1}})
	at := ivPS.Add(20 * 24 * time.Hour)
	if _, err := store.CancelAtomic(clock.WithEffectiveNow(ctx, at), tenantID, sub.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(ctx, `DELETE FROM billing_intervals WHERE subscription_id = $1`, sub.ID); err != nil {
		t.Fatalf("wipe: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if _, err := store.BackfillBillingIntervals(ctx); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	ivs := readItemIntervals(t, db, ctx, tenantID, sub.Items[0].ID)
	if len(ivs) != 1 || !ivEndEq(ivs[0].endsAt, at) {
		t.Fatalf("canceled-sub backfill must seal at canceled_at %s: %+v", at, ivs)
	}
}

func TestIntervals_HardDeleteRefusedLoudly(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "IV HardDelete")
	cust := ivCustomer(t, db, ctx, tenantID, "cus_iv_hd")
	planA := ivPlan(t, db, ctx, tenantID, "iv-hd-a", 1000)
	store := NewPostgresStore(db)
	sub := ivActiveSub(t, store, ctx, tenantID, cust, "sub-iv-hd", []domain.SubscriptionItem{{PlanID: planA.ID, Quantity: 1}})

	// Direct hard delete is refused (0160): it would strand the item's
	// interval rows with no owning row.
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM subscription_items WHERE id = $1`, sub.Items[0].ID)
	if err == nil || !strings.Contains(err.Error(), "ADR-101") {
		t.Fatalf("direct hard delete must be refused by the trigger, got: %v", err)
	}
	postgres.Rollback(tx)

	// The cascade path (parent subscription deleted — the ADR-086
	// teardown shape) must stay silent and sweep the intervals away.
	tx2, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx2)
	if _, err := tx2.ExecContext(ctx, `DELETE FROM subscriptions WHERE id = $1`, sub.ID); err != nil {
		t.Fatalf("cascade delete must stay allowed: %v", err)
	}
	var n int
	if err := tx2.QueryRowContext(ctx, `SELECT count(*) FROM billing_intervals WHERE subscription_id = $1`, sub.ID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("cascade must sweep billing_intervals, %d rows remain", n)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// TestIntervals_BackfillTwoReplicasConverge is sweep-2026-08-30 S6: two
// replicas boot together and both run BackfillBillingIntervals on the same
// partition. Both must return nil — the second committer's EXCLUDE violation
// means "a sibling reconstructed this first", not a boot failure — and the
// table must hold exactly the one reconstruction. The barrier hook makes the
// race deterministic: both goroutines read "no rows" before either inserts.
// Mutation check: drop the IsExclusionViolation branch in
// BackfillBillingIntervals → one goroutine errors → this fails.
func TestIntervals_BackfillTwoReplicasConverge(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "IV Backfill Race")
	cust := ivCustomer(t, db, ctx, tenantID, "cus_iv_race")
	planA := ivPlan(t, db, ctx, tenantID, "iv-race-a", 1000)
	store := NewPostgresStore(db)
	sub := ivActiveSub(t, store, ctx, tenantID, cust, "sub-iv-race", []domain.SubscriptionItem{{PlanID: planA.ID, Quantity: 1}})
	itemID := sub.Items[0].ID

	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM billing_intervals WHERE subscription_id = $1`, sub.ID); err != nil {
		t.Fatalf("wipe intervals: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// Barrier: both "replicas" must have read the partition before either
	// inserts. Each calls the hook once per partition (one partition here).
	var arrive sync.WaitGroup
	arrive.Add(2)
	backfillBeforeInsert = func() { arrive.Done(); arrive.Wait() }
	t.Cleanup(func() { backfillBeforeInsert = nil })

	type result struct {
		n   int
		err error
	}
	results := make(chan result, 2)
	for i := 0; i < 2; i++ {
		go func() {
			n, err := store.BackfillBillingIntervals(ctx)
			results <- result{n, err}
		}()
	}
	var total int
	for i := 0; i < 2; i++ {
		r := <-results
		if r.err != nil {
			t.Fatalf("a booting replica failed its backfill because a sibling won the race: %v", r.err)
		}
		total += r.n
	}
	if total != 1 {
		t.Fatalf("exactly one replica must reconstruct the segment; inserted=%d", total)
	}
	if ivs := readItemIntervals(t, db, ctx, tenantID, itemID); len(ivs) != 1 {
		t.Fatalf("table must hold one reconstructed segment, got %+v", ivs)
	}
}
