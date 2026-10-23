package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/dunning"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/leader"
	"github.com/sagarsuperuser/velox/internal/platform/leader/leadertest"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// A3 (product sweep 2026-10-21, ADR-036 amendment 2026-10-22): when dunning
// paused a subscription's collection and the invoice whose dunning paused
// it is paid, collection resumes by itself and the subscription's activity
// says so. An operator's pause never lifts on a payment.
//
// Real router, real Postgres. The pause comes from a real dunning
// exhaustion under a policy whose final subscription action is `pause`, and
// each payment goes through the path an operator or a customer uses.

// pauseRig is the dates rig plus the stores the assertions read.
type pauseRig struct {
	*datesRig
	ctx   context.Context
	admin *sql.DB
	invs  *invoice.PostgresStore
	runs  *dunning.PostgresStore
	pol   domain.DunningPolicy
}

func newPauseRig(t *testing.T, name string, invoiceAction domain.DunningInvoiceAction) *pauseRig {
	t.Helper()
	r := &pauseRig{datesRig: newDatesRig(t, name), admin: testutil.AdminPool(t)}
	r.ctx = leadertest.Token(t, r.admin, postgres.WithLivemode(context.Background(), false), leader.RoleDunning)
	r.invs = invoice.NewPostgresStore(r.db)
	r.runs = dunning.NewPostgresStore(r.db)
	pol, err := r.srv.DunningSvc.UpsertPolicy(r.ctx, r.tenantID, domain.DunningPolicy{
		Name: "pause on exhaustion", Enabled: true, RetryDays: []int{3},
		FinalSubscriptionAction: domain.SubActionPause, FinalInvoiceAction: invoiceAction,
	})
	if err != nil {
		t.Fatalf("upsert policy: %v", err)
	}
	r.pol = pol
	return r
}

// sub is a wall-clock subscription on the in_advance plan and its day-1
// invoice (finalized, unpaid: the customer has no card).
func (r *pauseRig) sub(key string) (subID, dayOneID string) {
	r.t.Helper()
	subID, _ = r.subWithItem(key, "", r.planID)
	dayOne := r.dayOne(subID)
	if dayOne["status"] != "finalized" || flowInt(dayOne, "amount_due_cents") <= 0 {
		r.t.Fatalf("%s: day-1 invoice %s with %d due, want finalized and owed", key, dayOne["status"], flowInt(dayOne, "amount_due_cents"))
	}
	return subID, flowStr(dayOne, "id")
}

// openInvoice adds a finalized, unpaid invoice to the subscription.
func (r *pauseRig) openInvoice(subID, number string, cents int64) string {
	r.t.Helper()
	sub := r.call("GET", "/v1/subscriptions/"+subID, flowReq{}).want(r.t, 200, "read subscription").body
	now := time.Now().UTC()
	inv, err := r.invs.Create(r.ctx, r.tenantID, domain.Invoice{
		CustomerID: flowStr(sub, "customer_id"), SubscriptionID: subID, InvoiceNumber: number,
		Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentFailed, Currency: "USD",
		SubtotalCents: cents, TotalAmountCents: cents, AmountDueCents: cents,
		BillingPeriodStart: now.Add(-24 * time.Hour), BillingPeriodEnd: now, IssuedAt: &now,
	})
	if err != nil {
		r.t.Fatalf("create invoice %s: %v", number, err)
	}
	return inv.ID
}

// armRun leaves invoiceID with an active dunning run under the pause
// policy, its one retry used and due at next: the run the invoice's failed
// collection started, or a new one.
func (r *pauseRig) armRun(invoiceID, customerID string, next time.Time) string {
	r.t.Helper()
	if run, err := r.runs.GetActiveRunByInvoice(r.ctx, r.tenantID, invoiceID); err == nil {
		if _, err := r.admin.Exec(`UPDATE invoice_dunning_runs SET policy_id = $1, attempt_count = $2, next_action_at = $3
			WHERE id = $4`, r.pol.ID, r.pol.Retries(), next, run.ID); err != nil {
			r.t.Fatalf("arm run: %v", err)
		}
		return run.ID
	}
	run, err := r.runs.CreateRun(r.ctx, r.tenantID, domain.InvoiceDunningRun{
		InvoiceID: invoiceID, CustomerID: customerID, PolicyID: r.pol.ID,
		State: domain.DunningActive, Reason: "payment_failed", AttemptCount: r.pol.Retries(), NextActionAt: &next,
	})
	if err != nil {
		r.t.Fatalf("create run: %v", err)
	}
	if ok, err := r.runs.UpdateRunIfActive(r.ctx, r.tenantID, run, run.AttemptCount, nil); err != nil || !ok {
		r.t.Fatalf("record the attempts: applied=%v err=%v", ok, err)
	}
	return run.ID
}

// exhaust runs invoiceID's dunning to its end on the wall clock: its run
// with its one retry used, due now, then a dunning tick. Returns the run.
func (r *pauseRig) exhaust(subID, invoiceID string) domain.InvoiceDunningRun {
	r.t.Helper()
	sub := r.call("GET", "/v1/subscriptions/"+subID, flowReq{}).want(r.t, 200, "read subscription").body
	runID := r.armRun(invoiceID, flowStr(sub, "customer_id"), time.Now().UTC().Add(-time.Minute))
	if _, errs := r.srv.DunningSvc.ProcessDueRuns(r.ctx, r.tenantID, 50); len(errs) > 0 {
		r.t.Fatalf("dunning tick: %v", errs)
	}
	got, err := r.runs.GetRun(r.ctx, r.tenantID, runID)
	if err != nil {
		r.t.Fatalf("get run: %v", err)
	}
	if got.State != domain.DunningEscalated {
		r.t.Fatalf("run %s/%q after its retries ran out, want escalated", got.State, got.Resolution)
	}
	return got
}

// dunningPaused is a subscription whose day-1 invoice exhausted the pause
// policy: collection is paused by dunning.
func (r *pauseRig) dunningPaused(key string) (subID, dayOneID string) {
	r.t.Helper()
	subID, dayOneID = r.sub(key)
	r.exhaust(subID, dayOneID)
	pc := r.pause(subID)
	if pc == nil || pc["resumes_at"] != nil {
		r.t.Fatalf("%s: pause_collection after exhaustion = %v, want a pause with no resumes_at", key, pc)
	}
	if pc["set_by"] != "dunning" {
		r.t.Errorf("%s: pause_collection after exhaustion = %v, want set_by=dunning", key, pc)
	}
	return subID, dayOneID
}

// pause is the subscription's pause_collection, nil when not paused.
func (r *pauseRig) pause(subID string) map[string]any {
	r.t.Helper()
	sub := r.call("GET", "/v1/subscriptions/"+subID, flowReq{}).want(r.t, 200, "read subscription").body
	pc, _ := sub["pause_collection"].(map[string]any)
	return pc
}

// invoice is the invoice as GET /v1/invoices/{id} returns it.
func (r *pauseRig) invoice(id string) map[string]any {
	r.t.Helper()
	return flowObj(r.call("GET", "/v1/invoices/"+id, flowReq{}).want(r.t, 200, "read invoice").body, "invoice")
}

func (r *pauseRig) payOffline(invoiceID string) map[string]any {
	r.t.Helper()
	return r.call("POST", "/v1/invoices/"+invoiceID+"/record-payment", flowReq{body: map[string]any{"note": "wire received"}}).
		want(r.t, 200, "record payment").body
}

// creditNote issues a credit note for cents against the invoice.
func (r *pauseRig) creditNote(invoiceID string, cents int64) {
	r.t.Helper()
	r.call("POST", "/v1/credit-notes", flowReq{body: map[string]any{
		"invoice_id": invoiceID, "reason": "Waived", "auto_issue": true,
		"lines": []map[string]any{{"description": "Waived", "quantity": 1, "unit_amount_cents": cents}},
	}}).want(r.t, 201, "issue credit note")
}

func (r *pauseRig) resumer() {
	r.t.Helper()
	if _, errs := r.srv.SubscriptionSvc.ProcessExpiredPauseCollections(r.ctx, 100); len(errs) > 0 {
		r.t.Fatalf("pause resumer: %v", errs)
	}
}

// auditMetas returns the metadata of the subscription's audit rows whose
// metadata action is action.
func (r *pauseRig) auditMetas(subID, action string) []map[string]any {
	r.t.Helper()
	rows, err := r.admin.Query(`SELECT metadata FROM audit_log
		WHERE resource_type = 'subscription' AND resource_id = $1 AND metadata->>'action' = $2 ORDER BY created_at`, subID, action)
	if err != nil {
		r.t.Fatalf("read audit rows: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []map[string]any
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			r.t.Fatalf("scan audit row: %v", err)
		}
		m := map[string]any{}
		if err := json.Unmarshal(raw, &m); err != nil {
			r.t.Fatalf("audit metadata %s: %v", raw, err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		r.t.Fatalf("audit rows: %v", err)
	}
	return out
}

func (r *pauseRig) outboxCount(subID, eventType string) int {
	r.t.Helper()
	var n int
	if err := r.admin.QueryRow(`SELECT count(*) FROM webhook_outbox
		WHERE tenant_id = $1 AND event_type = $2 AND payload->'subscription'->>'id' = $3`, r.tenantID, eventType, subID).Scan(&n); err != nil {
		r.t.Fatalf("count %s: %v", eventType, err)
	}
	return n
}

// subLineTitles returns every subscription timeline line titled title.
func (r *pauseRig) subLines(subID, title string) []map[string]any {
	r.t.Helper()
	events := flowList(r.call("GET", "/v1/subscriptions/"+subID+"/timeline", flowReq{}).want(r.t, 200, "subscription timeline").body, "events")
	var found []map[string]any
	for _, l := range linesOf(r.t, events) {
		if l["title"] == title {
			found = append(found, l)
		}
	}
	return found
}

// lineText joins a line's detail segments' text.
func lineText(l map[string]any) string {
	var b strings.Builder
	detail, _ := l["detail"].([]any)
	for _, s := range detail {
		parts, _ := s.([]any)
		for _, p := range parts {
			pm, _ := p.(map[string]any)
			if txt, ok := pm["text"].(string); ok {
				b.WriteString(txt)
			}
		}
	}
	return b.String()
}

// assertReleasedBy checks the pause was released by the payment of
// paidInvoiceID: resumes_at is the payment's instant, in the past, so the
// pause no longer holds.
func (r *pauseRig) assertReleasedBy(subID, paidInvoiceID string) {
	r.t.Helper()
	pc := r.pause(subID)
	if pc == nil {
		r.t.Fatal("pause_collection gone before the resumer ran; want it kept, timed at the payment")
	}
	resumes, _ := pc["resumes_at"].(string)
	if resumes == "" {
		r.t.Fatalf("pause_collection = %v after the invoice it was set for was paid, want resumes_at set to the payment's instant", pc)
	}
	ra, err := time.Parse(time.RFC3339Nano, resumes)
	if err != nil {
		r.t.Fatalf("resumes_at %q: %v", resumes, err)
	}
	inv := r.invoice(paidInvoiceID)
	paidAt := at(r.t, inv, "paid_at")
	if d := ra.Sub(paidAt); d < -5*time.Second || d > 5*time.Second {
		r.t.Errorf("resumes_at %v, want the payment's instant (paid_at %v)", ra, paidAt)
	}
	if ra.After(time.Now()) {
		r.t.Errorf("resumes_at %v is in the future: the pause still holds", ra)
	}
}

// assertResumedByPayment runs the resumer and checks it lifted the pause
// once, said why, and announced it once.
func (r *pauseRig) assertResumedByPayment(subID string) {
	r.t.Helper()
	r.resumer()
	if pc := r.pause(subID); pc != nil {
		r.t.Fatalf("pause_collection = %v after the resumer, want none", pc)
	}
	var pauseInvoice sql.NullString
	if err := r.admin.QueryRow(`SELECT pause_collection_invoice_id FROM subscriptions WHERE id = $1`, subID).Scan(&pauseInvoice); err != nil {
		r.t.Fatalf("read pause_collection_invoice_id: %v", err)
	}
	if pauseInvoice.Valid {
		r.t.Errorf("pause_collection_invoice_id = %q after the resume, want NULL", pauseInvoice.String)
	}
	resumed := r.auditMetas(subID, domain.SubAuditCollectionResumed)
	if len(resumed) != 1 || resumed[0]["triggered_by"] != "payment" {
		r.t.Fatalf("collection_resumed rows %v, want exactly one with triggered_by=payment", resumed)
	}
	lines := r.subLines(subID, "Collection resumed")
	if len(lines) != 1 {
		r.t.Fatalf("%d \"Collection resumed\" lines, want 1", len(lines))
	}
	if got := lineText(lines[0]); got != "The overdue invoice was paid" {
		r.t.Errorf("\"Collection resumed\" detail %q, want %q", got, "The overdue invoice was paid")
	}
	if n := r.outboxCount(subID, domain.EventSubscriptionCollectionResumed); n != 1 {
		r.t.Errorf("%d subscription.collection_resumed events queued, want 1", n)
	}
}

// assertStillPaused checks the payment left the pause as it was and the
// resumer does not lift it.
func (r *pauseRig) assertStillPaused(subID, wantSetBy string) {
	r.t.Helper()
	pc := r.pause(subID)
	if pc == nil || pc["resumes_at"] != nil || pc["set_by"] != wantSetBy {
		r.t.Fatalf("pause_collection = %v, want a %s pause with no resumes_at", pc, wantSetBy)
	}
	r.resumer()
	if r.pause(subID) == nil {
		r.t.Fatal("the resumer lifted a pause no payment released")
	}
	if rows := r.auditMetas(subID, domain.SubAuditCollectionResumed); len(rows) != 0 {
		r.t.Errorf("collection_resumed rows %v, want none", rows)
	}
}

// T1. Every way an operator or a customer pays the overdue invoice lifts a
// dunning pause. The release rides the paid transaction (the invoice
// store's markPaidLockedTx, ADR-068's choke point), so each path below
// reaches it through its own writer. The card, reconciler and
// credits-cover paths are the same choke point, covered at the store by
// TestChokePointCloses in internal/invoice.
// Fails on main: resumes_at stays NULL and the pause never lifts.
func TestDunningPause_PaymentResumesCollection(t *testing.T) {
	cases := []struct {
		name string
		pay  func(r *pauseRig, run domain.InvoiceDunningRun, invoiceID string, due int64)
	}{
		{"offline payment", func(r *pauseRig, _ domain.InvoiceDunningRun, invoiceID string, _ int64) {
			r.payOffline(invoiceID)
		}},
		{"dunning run resolved as recovered", func(r *pauseRig, run domain.InvoiceDunningRun, _ string, _ int64) {
			r.call("POST", "/v1/dunning/runs/"+run.ID+"/resolve", flowReq{body: map[string]any{"resolution": "payment_recovered"}}).
				want(r.t, 200, "resolve run")
		}},
		{"credit note for everything due", func(r *pauseRig, _ domain.InvoiceDunningRun, invoiceID string, due int64) {
			r.creditNote(invoiceID, due)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newPauseRig(t, "Dunning Pause Paid "+tc.name, domain.InvActionNone)
			subID, invID := r.sub("paid")
			run := r.exhaust(subID, invID)
			if pc := r.pause(subID); pc == nil {
				t.Fatal("no pause_collection after exhaustion under a pause policy")
			} else if pc["set_by"] != "dunning" {
				t.Errorf("pause_collection after exhaustion = %v, want set_by=dunning", pc)
			}
			inv := r.invoice(invID)
			tc.pay(r, run, invID, flowInt(inv, "amount_due_cents"))
			if got := flowStr(r.invoice(invID), "status"); got != "paid" {
				t.Fatalf("invoice %s after the payment, want paid", got)
			}
			r.assertReleasedBy(subID, invID)
			r.assertResumedByPayment(subID)
		})
	}
}

// T2. Negative controls: only the full payment of the invoice a dunning
// pause was set for lifts it.
func TestDunningPause_PaymentLeavesOtherPausesAlone(t *testing.T) {
	t.Run("operator pause", func(t *testing.T) {
		r := newPauseRig(t, "Operator Pause Paid", domain.InvActionNone)
		subID, invID := r.sub("operator")
		r.call("PUT", "/v1/subscriptions/"+subID+"/pause-collection", flowReq{body: map[string]any{"behavior": "keep_as_draft"}}).
			want(t, 200, "pause collection")
		r.payOffline(invID)
		r.assertStillPaused(subID, "operator")
	})

	t.Run("operator took over a dunning pause", func(t *testing.T) {
		r := newPauseRig(t, "Operator Took Over Pause", domain.InvActionNone)
		subID, invID := r.dunningPaused("takeover")
		r.call("PUT", "/v1/subscriptions/"+subID+"/pause-collection", flowReq{body: map[string]any{"behavior": "keep_as_draft"}}).
			want(t, 200, "pause collection")
		r.payOffline(invID)
		r.assertStillPaused(subID, "operator")
	})

	t.Run("credit note leaves some due", func(t *testing.T) {
		r := newPauseRig(t, "Partial Credit Note Pause", domain.InvActionNone)
		subID, invID := r.dunningPaused("partial")
		inv := r.invoice(invID)
		r.creditNote(invID, flowInt(inv, "amount_due_cents")/2)
		r.assertStillPaused(subID, "dunning")
	})
}

// T3. Dunning never overrides a pause in force, and a second exhaustion on
// a subscription it already paused writes nothing. Fails on main: dunning's
// pause replaced the operator's resumes_at with none, and every exhaustion
// wrote its own "Collection paused" row.
func TestDunningPause_KeepsThePauseInForce(t *testing.T) {
	t.Run("operator timed pause", func(t *testing.T) {
		r := newPauseRig(t, "Dunning Keeps Operator Pause", domain.InvActionNone)
		subID, invID := r.sub("timed")
		resumes := time.Now().UTC().Add(30 * 24 * time.Hour).Truncate(time.Second)
		r.call("PUT", "/v1/subscriptions/"+subID+"/pause-collection", flowReq{body: map[string]any{
			"behavior": "keep_as_draft", "resumes_at": resumes.Format(time.RFC3339),
		}}).want(t, 200, "pause collection")
		run := r.exhaust(subID, invID)

		pc := r.pause(subID)
		got, _ := pc["resumes_at"].(string)
		if ra, err := time.Parse(time.RFC3339Nano, got); err != nil || !ra.Equal(resumes) || pc["set_by"] != "operator" {
			t.Errorf("pause_collection after exhaustion = %v, want the operator's pause resuming %v", pc, resumes)
		}
		var reason string
		if err := r.admin.QueryRow(`SELECT reason FROM invoice_dunning_events WHERE run_id = $1 AND event_type = $2`,
			run.ID, domain.DunningEventEscalated).Scan(&reason); err != nil {
			t.Fatalf("read escalated event: %v", err)
		}
		if reason != "subscription_paused" {
			t.Errorf("escalated event reason %q, want subscription_paused: collection is paused", reason)
		}
		if rows := r.auditMetas(subID, domain.SubAuditCollectionPaused); len(rows) != 1 || rows[0]["triggered_by"] != "operator" {
			t.Errorf("collection_paused rows %v, want only the operator's", rows)
		}
	})

	t.Run("second exhaustion", func(t *testing.T) {
		r := newPauseRig(t, "Dunning Pause Twice", domain.InvActionNone)
		subID, _ := r.dunningPaused("twice")
		second := r.openInvoice(subID, "INV-A3-SECOND", 2500)
		r.exhaust(subID, second)
		if pc := r.pause(subID); pc == nil || pc["set_by"] != "dunning" || pc["resumes_at"] != nil {
			t.Errorf("pause_collection after the second exhaustion = %v, want the dunning pause unchanged", pc)
		}
		if rows := r.auditMetas(subID, domain.SubAuditCollectionPaused); len(rows) != 1 {
			t.Errorf("%d collection_paused rows, want 1: the second exhaustion found collection already paused", len(rows))
		}
	})
}

// T4. The pause belongs to the invoice whose dunning set it (Sagar's A3
// decision, 2026-10-21): paying another invoice leaves it, and paying that
// invoice lifts it even while a third invoice is still owed. Fails before
// the review fix, which held the pause until nothing at all was owed.
func TestDunningPause_OnlyThePausingInvoiceLiftsIt(t *testing.T) {
	r := newPauseRig(t, "Dunning Pause Two Invoices", domain.InvActionNone)
	subID, pausing := r.dunningPaused("two")
	other := r.openInvoice(subID, "INV-A3-X2", 2500)
	r.openInvoice(subID, "INV-A3-X3", 2500) // still owed at the end

	r.payOffline(other)
	r.assertStillPaused(subID, "dunning")

	r.payOffline(pausing)
	r.assertReleasedBy(subID, pausing)
	r.assertResumedByPayment(subID)
}

// T7 (C7). On a test clock: a period that ends under the dunning pause
// bills a draft, the payment lifts the pause at its own instant, and the
// next period bills normally. Two shapes: the period closed before the
// payment (an advance closed it), and the period had ended but was not
// closed when the payment landed (the clock moved, the catchup's cycle
// phase had not run yet). Fails on main: the pause never lifts, so the
// next period's invoice is a draft too.
func TestDunningPause_TestClockPaymentAtPeriodBoundary(t *testing.T) {
	for _, closedFirst := range []bool{true, false} {
		name := "period closed before the payment"
		if !closedFirst {
			name = "period ended, not closed, when the payment lands"
		}
		t.Run(name, func(t *testing.T) {
			r := newPauseRig(t, "Dunning Pause Clock "+name, domain.InvActionNone)
			start := time.Date(2027, 2, 1, 4, 30, 0, 0, time.UTC)
			clockID := r.newClock("a3 "+name, start)
			subID, _ := r.subWithItem("clock", clockID, r.planID)
			dayOneID := flowStr(r.dayOne(subID), "id")
			sub := r.call("GET", "/v1/subscriptions/"+subID, flowReq{}).want(t, 200, "read subscription").body
			t1 := at(t, sub, "current_billing_period_end")

			// The day-1 invoice's dunning exhausts at start.
			r.armRun(dayOneID, flowStr(sub, "customer_id"), start.Add(-time.Minute))
			simCtx := clock.WithSim(r.ctx, clock.Sim{At: start, TestClockID: clockID})
			if _, errs := r.srv.DunningSvc.ProcessDueRunsForClock(simCtx, r.tenantID, clockID, start, 50); len(errs) != 0 {
				t.Fatalf("dunning phase: %v", errs)
			}
			if pc := r.pause(subID); pc == nil || pc["resumes_at"] != nil {
				t.Fatalf("pause_collection after exhaustion = %v, want a pause with no resumes_at", pc)
			}

			t2 := t1.Add(48 * time.Hour)
			if closedFirst {
				r.advance(clockID, t2)
			} else if _, err := r.admin.Exec(`UPDATE test_clocks SET frozen_time = $1 WHERE id = $2`, t2, clockID); err != nil {
				t.Fatalf("move the clock: %v", err)
			}
			r.payOffline(dayOneID)
			pc := r.pause(subID)
			got, _ := pc["resumes_at"].(string)
			if ra, err := time.Parse(time.RFC3339Nano, got); err != nil || !ra.Equal(t2) {
				t.Fatalf("pause_collection after the payment = %v, want resumes_at at the payment's instant %v", pc, t2)
			}

			sub = r.call("GET", "/v1/subscriptions/"+subID, flowReq{}).want(t, 200, "read subscription").body
			t3 := at(t, sub, "current_billing_period_end")
			if !closedFirst {
				t3 = t3.AddDate(0, 1, 0) // P is still open: its end is t1, and the next one ends a month later
			}
			r.advance(clockID, t3.Add(48*time.Hour))

			var cycle []map[string]any
			for _, inv := range r.invoices(subID) {
				if inv["billing_reason"] == "subscription_cycle" {
					cycle = append(cycle, inv)
				}
			}
			if len(cycle) != 2 {
				t.Fatalf("%d cycle invoices, want 2: %s", len(cycle), mustJSON(cycle))
			}
			if cycle[0]["status"] != "draft" {
				t.Errorf("invoice closed at %v (under the pause) is %s, want draft", t1, cycle[0]["status"])
			}
			if cycle[1]["status"] != "finalized" && cycle[1]["status"] != "paid" {
				t.Errorf("invoice closed at %v (after the payment) is %s, want finalized", t3, cycle[1]["status"])
			}
			if r.pause(subID) != nil {
				t.Error("the advance's resume phase left the pause on the subscription")
			}
			resumed := r.auditMetas(subID, domain.SubAuditCollectionResumed)
			if len(resumed) != 1 || resumed[0]["triggered_by"] != "payment" {
				t.Fatalf("collection_resumed rows %v, want one with triggered_by=payment", resumed)
			}
			if ra, err := time.Parse(time.RFC3339Nano, fmt.Sprint(resumed[0]["resumed_at"])); err != nil || !ra.Equal(t2) {
				t.Errorf("resumed_at %v, want the payment's instant %v", resumed[0]["resumed_at"], t2)
			}
		})
	}
}

// T8. A canceled subscription keeps its pause: no payment lifts it, and no
// resume line appears on it.
func TestDunningPause_CanceledSubscriptionNotReleased(t *testing.T) {
	r := newPauseRig(t, "Dunning Pause Canceled", domain.InvActionNone)
	subID, dayOne := r.sub("canceled")
	r.payOffline(dayOne)
	x := r.openInvoice(subID, "INV-A3-CANCELED", 2500)
	r.exhaust(subID, x)
	if r.pause(subID) == nil {
		t.Fatal("no pause_collection after exhaustion under a pause policy")
	}
	r.call("POST", "/v1/subscriptions/"+subID+"/cancel", flowReq{}).want(t, 200, "cancel subscription")
	r.payOffline(x)
	if pc := r.pause(subID); pc != nil && pc["resumes_at"] != nil {
		t.Errorf("pause_collection = %v on the canceled subscription, want no release", pc)
	}
	r.resumer()
	if rows := r.auditMetas(subID, domain.SubAuditCollectionResumed); len(rows) != 0 {
		t.Errorf("collection_resumed rows %v on a canceled subscription, want none", rows)
	}
}

// T9. pause + mark_uncollectible: the written-off invoice paid later still
// lifts the pause. Fails on main.
func TestDunningPause_WrittenOffInvoicePaid(t *testing.T) {
	r := newPauseRig(t, "Dunning Pause Written Off", domain.InvActionMarkUncollectible)
	subID, invID := r.dunningPaused("written-off")
	if got := flowStr(r.invoice(invID), "status"); got != "uncollectible" {
		t.Fatalf("invoice %s after exhaustion, want uncollectible", got)
	}
	r.payOffline(invID)
	r.assertReleasedBy(subID, invID)
	r.assertResumedByPayment(subID)
}
