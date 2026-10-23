package invoice_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/webhook"
)

// disputeHarness is one tenant with one invoice paid by card (PaymentIntent
// pi), and the real store with the real outbox, so the events a dispute
// queues are rows in webhook_outbox written by the dispute's transaction.
type disputeHarness struct {
	db       *postgres.DB
	ctx      context.Context
	tenantID string
	store    *invoice.PostgresStore
	invID    string
	pi       string
	t0       time.Time
}

func newDisputeHarness(t *testing.T, db *postgres.DB, name string) *disputeHarness {
	t.Helper()
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, name)
	store := invoice.NewPostgresStore(db)
	store.SetOutboxEnqueuer(webhook.NewOutboxStore(db))
	invID := seedFinalizedInvoice(t, db, store, ctx, tenantID)
	pi := "pi_dsp_" + invID
	if _, transitioned, err := store.MarkPaidCardSettlementTransition(ctx, tenantID, invID, pi,
		time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), nil); err != nil || !transitioned {
		t.Fatalf("settle the invoice by card: transitioned=%v err=%v", transitioned, err)
	}
	return &disputeHarness{
		db: db, ctx: ctx, tenantID: tenantID, store: store, invID: invID, pi: pi,
		t0: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC),
	}
}

// snap is one charge.dispute.* event: the dispute disputeID on h.pi, as it
// stood at at.
func (h *disputeHarness) snap(eventID, disputeID string, status domain.DisputeStatus, at time.Time) domain.DisputeSnapshot {
	due := h.t0.Add(14 * 24 * time.Hour)
	return domain.DisputeSnapshot{
		StripeEventID:         eventID,
		EventAt:               at,
		StripeDisputeID:       disputeID,
		StripeChargeID:        "ch_" + disputeID,
		StripePaymentIntentID: h.pi,
		Status:                status,
		Reason:                "fraudulent",
		AmountCents:           1000,
		Currency:              "usd",
		EvidenceDueBy:         &due,
		OpenedAt:              h.t0,
	}
}

func (h *disputeHarness) record(t *testing.T, s domain.DisputeSnapshot) domain.DisputeRecordResult {
	t.Helper()
	res, err := h.store.RecordStripeDispute(h.ctx, h.tenantID, s)
	if err != nil {
		t.Fatalf("record %s (%s, %s): %v", s.StripeEventID, s.StripeDisputeID, s.Status, err)
	}
	return res
}

// events counts the outbox rows of one event type for one dispute.
func (h *disputeHarness) events(t *testing.T, eventType, disputeID string) int {
	t.Helper()
	tx, err := h.db.BeginTx(h.ctx, postgres.TxTenant, h.tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	var n int
	if err := tx.QueryRowContext(h.ctx, `
		SELECT COUNT(*) FROM webhook_outbox
		WHERE event_type = $1 AND payload->'dispute'->>'stripe_dispute_id' = $2`, eventType, disputeID).Scan(&n); err != nil {
		t.Fatalf("count %s events: %v", eventType, err)
	}
	return n
}

func (h *disputeHarness) wantEvents(t *testing.T, disputeID string, opened, closed int) {
	t.Helper()
	if got := h.events(t, domain.EventPaymentDisputeOpened, disputeID); got != opened {
		t.Errorf("%s: %d payment.dispute_opened events, want %d", disputeID, got, opened)
	}
	if got := h.events(t, domain.EventPaymentDisputeClosed, disputeID); got != closed {
		t.Errorf("%s: %d payment.dispute_closed events, want %d", disputeID, got, closed)
	}
}

func (h *disputeHarness) dispute(t *testing.T, disputeID string) domain.InvoiceDispute {
	t.Helper()
	all, err := h.store.ListDisputesByInvoice(h.ctx, h.tenantID, h.invID)
	if err != nil {
		t.Fatalf("list disputes: %v", err)
	}
	var out []domain.InvoiceDispute
	for _, d := range all {
		if d.StripeDisputeID == disputeID {
			out = append(out, d)
		}
	}
	if len(out) != 1 {
		t.Fatalf("%s: %d rows, want 1 (one Stripe dispute = one row): %+v", disputeID, len(out), all)
	}
	return out[0]
}

// wantInvoiceUntouched: a dispute never changes the invoice (#979). It stays
// paid, with what was paid.
func (h *disputeHarness) wantInvoiceUntouched(t *testing.T) {
	t.Helper()
	inv, err := h.store.Get(h.ctx, h.tenantID, h.invID)
	if err != nil {
		t.Fatalf("get invoice: %v", err)
	}
	if inv.Status != domain.InvoicePaid || inv.PaymentStatus != domain.PaymentSucceeded ||
		inv.AmountPaidCents != 1000 || inv.AmountDueCents != 0 || inv.StripePaymentIntentID != h.pi {
		t.Errorf("a dispute changed the invoice: status=%s payment_status=%s paid=%d due=%d pi=%s; want paid, succeeded, 1000, 0, %s",
			inv.Status, inv.PaymentStatus, inv.AmountPaidCents, inv.AmountDueCents, inv.StripePaymentIntentID, h.pi)
	}
}

// TestRecordStripeDispute_InOrder walks one dispute through Stripe's events in
// the order they happen: opened (one payment.dispute_opened), evidence sent
// (no event), lost (one payment.dispute_closed, closed_at = that event's
// time). The invoice stays paid throughout, and the lost dispute is what the
// attention read finds.
func TestRecordStripeDispute_InOrder(t *testing.T) {
	db := testutil.SetupTestDB(t)
	h := newDisputeHarness(t, db, "Dispute In Order")
	const du = "du_in_order"

	r := h.record(t, h.snap("evt_open", du, domain.DisputeNeedsResponse, h.t0))
	if !r.Applied || !r.Opened || r.Closed {
		t.Fatalf("created: %+v, want applied+opened, not closed", r)
	}
	if r.Dispute.InvoiceID != h.invID || r.Dispute.Currency != "USD" || r.Dispute.ClosedAt != nil {
		t.Errorf("created row: %+v, want invoice %s, USD, open", r.Dispute, h.invID)
	}
	h.wantEvents(t, du, 1, 0)

	r = h.record(t, h.snap("evt_review", du, domain.DisputeUnderReview, h.t0.Add(time.Minute)))
	if !r.Applied || r.Opened || r.Closed || r.Dispute.Status != domain.DisputeUnderReview {
		t.Fatalf("updated: %+v, want applied under_review, no event", r)
	}
	h.wantEvents(t, du, 1, 0)

	lostAt := h.t0.Add(2 * time.Minute)
	r = h.record(t, h.snap("evt_lost", du, domain.DisputeLost, lostAt))
	if !r.Applied || r.Opened || !r.Closed {
		t.Fatalf("closed: %+v, want applied+closed", r)
	}
	d := h.dispute(t, du)
	if d.Status != domain.DisputeLost || d.ClosedAt == nil || !d.ClosedAt.Equal(lostAt) ||
		!d.OpenedAt.Equal(h.t0) || d.AmountCents != 1000 || d.Reason != "fraudulent" ||
		d.StripeChargeID != "ch_"+du || d.StripePaymentIntentID != h.pi {
		t.Errorf("row after loss: %+v, want lost at %s, opened %s, 1000, fraudulent", d, lostAt, h.t0)
	}
	h.wantEvents(t, du, 1, 1)
	h.wantInvoiceUntouched(t)

	lost, err := h.store.LostDisputesByInvoices(h.ctx, h.tenantID, []string{h.invID, "vlx_inv_other"})
	if err != nil {
		t.Fatalf("lost disputes: %v", err)
	}
	if got, ok := lost[h.invID]; !ok || got.StripeDisputeID != du || len(lost) != 1 {
		t.Errorf("lost disputes = %+v, want only %s on %s", lost, du, h.invID)
	}
}

// TestRecordStripeDispute_AnyOrderAndRedelivery: Stripe does not deliver in
// order and redelivers. The row ends at the newest snapshot whatever the
// arrival order; an older event or a redelivery writes nothing and queues
// nothing; each event type is queued once per transition. Within one second,
// the later stage of the dispute's life wins. A late win (lost, then won)
// is a new close.
func TestRecordStripeDispute_AnyOrderAndRedelivery(t *testing.T) {
	db := testutil.SetupTestDB(t)
	h := newDisputeHarness(t, db, "Dispute Any Order")

	t.Run("close first, then older events", func(t *testing.T) {
		const du = "du_close_first"
		lostAt := h.t0.Add(2 * time.Minute)
		r := h.record(t, h.snap("evt_cf_lost", du, domain.DisputeLost, lostAt))
		if !r.Applied || !r.Opened || !r.Closed {
			t.Fatalf("first event is the close: %+v, want opened AND closed", r)
		}
		h.wantEvents(t, du, 1, 1)

		for _, s := range []domain.DisputeSnapshot{
			h.snap("evt_cf_open", du, domain.DisputeNeedsResponse, h.t0),
			h.snap("evt_cf_review", du, domain.DisputeUnderReview, h.t0.Add(time.Minute)),
			h.snap("evt_cf_lost", du, domain.DisputeLost, lostAt), // redelivery
		} {
			if r := h.record(t, s); r.Applied || r.Opened || r.Closed {
				t.Errorf("%s arrived after a newer event: %+v, want nothing applied", s.StripeEventID, r)
			}
		}
		d := h.dispute(t, du)
		if d.Status != domain.DisputeLost || d.ClosedAt == nil || !d.ClosedAt.Equal(lostAt) || d.LastStripeEventID != "evt_cf_lost" {
			t.Errorf("row: %+v, want still lost at %s from evt_cf_lost", d, lostAt)
		}
		h.wantEvents(t, du, 1, 1)

		// A late win (Stripe's "late win": lost, then won) is a new close.
		wonAt := h.t0.Add(10 * 24 * time.Hour)
		if r := h.record(t, h.snap("evt_cf_won", du, domain.DisputeWon, wonAt)); !r.Applied || r.Opened || !r.Closed {
			t.Fatalf("late win: %+v, want applied+closed", r)
		}
		if d := h.dispute(t, du); d.Status != domain.DisputeWon || d.ClosedAt == nil || !d.ClosedAt.Equal(wonAt) {
			t.Errorf("after the late win: %+v, want won at %s", d, wonAt)
		}
		h.wantEvents(t, du, 1, 2)
		if r := h.record(t, h.snap("evt_cf_lost", du, domain.DisputeLost, lostAt)); r.Applied {
			t.Errorf("the old loss redelivered after the win was applied: %+v", r)
		}
	})

	t.Run("same second: the later stage wins in either order", func(t *testing.T) {
		at := h.t0.Add(time.Hour)
		for _, tc := range []struct {
			du    string
			order []domain.DisputeStatus
		}{
			{"du_same_second_a", []domain.DisputeStatus{domain.DisputeNeedsResponse, domain.DisputeLost}},
			{"du_same_second_b", []domain.DisputeStatus{domain.DisputeLost, domain.DisputeNeedsResponse}},
		} {
			for i, st := range tc.order {
				h.record(t, h.snap(fmt.Sprintf("evt_%s_%d", tc.du, i), tc.du, st, at))
			}
			if d := h.dispute(t, tc.du); d.Status != domain.DisputeLost || d.ClosedAt == nil {
				t.Errorf("%s (%v at one instant): %+v, want lost", tc.du, tc.order, d)
			}
			h.wantEvents(t, tc.du, 1, 1)
		}
	})

	t.Run("an inquiry that closes without a chargeback", func(t *testing.T) {
		const du = "du_inquiry"
		h.record(t, h.snap("evt_inq_open", du, domain.DisputeWarningNeedsResponse, h.t0))
		r := h.record(t, h.snap("evt_inq_closed", du, domain.DisputeWarningClosed, h.t0.Add(120*24*time.Hour)))
		if !r.Closed || r.Dispute.Status != domain.DisputeWarningClosed {
			t.Errorf("inquiry close: %+v, want closed warning_closed", r)
		}
		h.wantEvents(t, du, 1, 1)
	})

	h.wantInvoiceUntouched(t)
	// du_close_first was won in the end, and the same-second pair is lost.
	lost, err := h.store.LostDisputesByInvoices(h.ctx, h.tenantID, []string{h.invID})
	if err != nil {
		t.Fatalf("lost disputes: %v", err)
	}
	if got := lost[h.invID]; got.Status != domain.DisputeLost || got.StripeDisputeID == "du_close_first" {
		t.Errorf("lost dispute for the invoice = %+v, want one of the same-second pair", got)
	}
}

// TestRecordStripeDispute_ConcurrentEventsRecordOnce is the collision test
// (playbook §5.1): every event of a new dispute, each delivered twice, all at
// once. They serialize on the invoice row; exactly one row results, ending at
// the newest snapshot, with one opened and one closed event.
func TestRecordStripeDispute_ConcurrentEventsRecordOnce(t *testing.T) {
	db := testutil.SetupTestDB(t)
	h := newDisputeHarness(t, db, "Dispute Concurrent")
	const du = "du_concurrent"
	snaps := []domain.DisputeSnapshot{
		h.snap("evt_cc_open", du, domain.DisputeNeedsResponse, h.t0),
		h.snap("evt_cc_review", du, domain.DisputeUnderReview, h.t0.Add(time.Minute)),
		h.snap("evt_cc_lost", du, domain.DisputeLost, h.t0.Add(2*time.Minute)),
	}
	snaps = append(snaps, snaps...)

	start := make(chan struct{})
	errsCh := make(chan error, len(snaps))
	var wg sync.WaitGroup
	for _, s := range snaps {
		wg.Add(1)
		go func(s domain.DisputeSnapshot) {
			defer wg.Done()
			<-start
			_, err := h.store.RecordStripeDispute(h.ctx, h.tenantID, s)
			errsCh <- err
		}(s)
	}
	close(start)
	wg.Wait()
	close(errsCh)
	for err := range errsCh {
		if err != nil {
			t.Errorf("concurrent record: %v", err)
		}
	}
	if d := h.dispute(t, du); d.Status != domain.DisputeLost || d.LastStripeEventID != "evt_cc_lost" {
		t.Errorf("row: %+v, want lost from evt_cc_lost", d)
	}
	h.wantEvents(t, du, 1, 1)
	h.wantInvoiceUntouched(t)
}

// TestRecordStripeDispute_FindsTheInvoiceByThePayment: a dispute names a
// payment, not an invoice. It is found from any payment Velox recorded on the
// invoice: the one that paid it, another attempt on it, or an unexpected
// payment flagged as an anomaly. A payment no invoice holds is not Velox's:
// ErrNotFound, and nothing is written.
func TestRecordStripeDispute_FindsTheInvoiceByThePayment(t *testing.T) {
	db := testutil.SetupTestDB(t)
	h := newDisputeHarness(t, db, "Dispute Matching")

	const otherAttempt, anomalyPI = "pi_dsp_other_attempt", "pi_dsp_anomaly"
	if err := h.store.RecordChargeAttempt(h.ctx, h.tenantID, domain.InvoiceChargeAttempt{
		InvoiceID: h.invID, StripePaymentIntentID: otherAttempt, Trigger: domain.ChargeTriggerExternal,
		Outcome: domain.ChargeAttemptSucceeded, AmountCents: 1000, OccurredAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("record the other attempt: %v", err)
	}
	if err := h.store.RecordPaymentAnomaly(h.ctx, h.tenantID, h.invID, domain.EventPaymentDuplicateCharge, anomalyPI, 1000); err != nil {
		t.Fatalf("record the anomaly: %v", err)
	}

	for _, pi := range []string{h.pi, otherAttempt, anomalyPI} {
		s := h.snap("evt_match_"+pi, "du_match_"+pi, domain.DisputeNeedsResponse, h.t0)
		s.StripePaymentIntentID = pi
		if r := h.record(t, s); r.Dispute.InvoiceID != h.invID {
			t.Errorf("dispute on %s recorded against %q, want %s", pi, r.Dispute.InvoiceID, h.invID)
		}
	}

	foreign := h.snap("evt_foreign", "du_foreign", domain.DisputeNeedsResponse, h.t0)
	foreign.StripePaymentIntentID = "pi_not_velox"
	if _, err := h.store.RecordStripeDispute(h.ctx, h.tenantID, foreign); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("dispute on a payment no invoice holds: err=%v, want ErrNotFound", err)
	}
	all, err := h.store.ListDisputesByInvoice(h.ctx, h.tenantID, h.invID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("%d disputes on the invoice, want 3 (the foreign one is not recorded)", len(all))
	}
	h.wantEvents(t, "du_foreign", 0, 0)

	bad := h.snap("evt_bad_status", "du_bad_status", domain.DisputeStatus("under_arbitration"), h.t0)
	if _, err := h.store.RecordStripeDispute(h.ctx, h.tenantID, bad); err == nil || errors.Is(err, errs.ErrNotFound) {
		t.Errorf("unknown status: err=%v, want a non-NotFound error (Stripe must redeliver it)", err)
	}
}

// TestDisputes_TenantAndModeIsolation: RLS, not the SQL, keeps one tenant's
// disputes from another, and test mode from live mode (playbook §5.2): the
// count query has no WHERE. Another tenant cannot record against this
// tenant's payment either: the lookup does not see it.
func TestDisputes_TenantAndModeIsolation(t *testing.T) {
	db := testutil.SetupTestDB(t)
	h := newDisputeHarness(t, db, "Dispute RLS Owner")
	h.record(t, h.snap("evt_rls", "du_rls", domain.DisputeLost, h.t0))

	other := testutil.CreateTestTenant(t, db, "Dispute RLS Other")
	count := func(ctx context.Context, tenantID string) int {
		t.Helper()
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer postgres.Rollback(tx)
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM invoice_disputes`).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}
	if n := count(h.ctx, h.tenantID); n != 1 {
		t.Fatalf("owner sees %d disputes, want 1", n)
	}
	if n := count(h.ctx, other); n != 0 {
		t.Errorf("another tenant sees %d disputes, want 0", n)
	}
	if n := count(postgres.WithLivemode(context.Background(), true), h.tenantID); n != 0 {
		t.Errorf("the owner's live mode sees %d test-mode disputes, want 0", n)
	}
	if got, err := h.store.ListDisputesByInvoice(h.ctx, other, h.invID); err != nil || len(got) != 0 {
		t.Errorf("another tenant lists %d disputes (err %v), want 0", len(got), err)
	}
	cross := h.snap("evt_cross", "du_cross", domain.DisputeNeedsResponse, h.t0)
	if _, err := h.store.RecordStripeDispute(h.ctx, other, cross); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("another tenant recording against this payment: err=%v, want ErrNotFound", err)
	}
}

// TestInvoiceService_LostDisputeRaisesAttention: the dispute_lost notice
// appears on the paid invoice once a dispute on it is lost, and not while
// it is open or after it is won.
func TestInvoiceService_LostDisputeRaisesAttention(t *testing.T) {
	db := testutil.SetupTestDB(t)
	h := newDisputeHarness(t, db, "Dispute Attention")
	svc := invoice.NewService(h.store, nil, nil)
	svc.SetDisputeReader(h.store)
	attention := func() *domain.Attention {
		t.Helper()
		inv, err := svc.Get(h.ctx, h.tenantID, h.invID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		return inv.Attention
	}

	h.record(t, h.snap("evt_att_open", "du_att", domain.DisputeNeedsResponse, h.t0))
	if att := attention(); att != nil {
		t.Errorf("open dispute: attention %+v, want none", att)
	}
	lostAt := h.t0.Add(time.Hour)
	h.record(t, h.snap("evt_att_lost", "du_att", domain.DisputeLost, lostAt))
	att := attention()
	if att == nil || att.Reason != domain.AttentionReasonDisputeLost || att.Severity != domain.AttentionSeverityCritical {
		t.Fatalf("lost dispute: attention %+v, want critical dispute_lost", att)
	}
	if att.Since == nil || !att.Since.Equal(lostAt) {
		t.Errorf("since = %v, want the loss at %s", att.Since, lostAt)
	}
	h.record(t, h.snap("evt_att_won", "du_att", domain.DisputeWon, lostAt.Add(time.Hour)))
	if att := attention(); att != nil {
		t.Errorf("won after all: attention %+v, want none", att)
	}

	disputes, err := svc.ListInvoiceDisputes(h.ctx, h.tenantID, h.invID)
	if err != nil || len(disputes) != 1 {
		t.Fatalf("list via the service: %d disputes, err %v, want 1", len(disputes), err)
	}
	if _, err := svc.ListInvoiceDisputes(h.ctx, h.tenantID, "vlx_inv_missing"); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("unknown invoice: err=%v, want ErrNotFound", err)
	}
}
