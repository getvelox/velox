package creditnote_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/creditnote"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// failingCNEmitter satisfies creditnote.AuditEmitter and always errors — the
// fault injection for the shared-fate direction (ADR-090): if the audit row
// cannot be written, the money-document mutation it evidences must not commit.
type failingCNEmitter struct{ calls int }

func (f *failingCNEmitter) LogInTx(_ context.Context, _ *sql.Tx, _ audit.Entry) error {
	f.calls++
	return errInjectedEmit
}

// seqNumbers is the credit-note number allocator for these tests (the real one
// lives in settings; Create refuses to run without a generator).
type seqNumbers struct {
	prefix string
	n      int
}

func (s *seqNumbers) NextCreditNoteNumber(context.Context, string) (string, error) {
	s.n++
	return fmt.Sprintf("CN-%s-%d", s.prefix, s.n), nil
}

// stubRefunder returns a scripted Stripe refund outcome for the retry path.
// getStatus scripts what the pre-create provider reconcile reports for an
// already-known refund id; it defaults to failed so retry tests fall through
// to the create leg the way the pre-reconcile flow did.
type stubRefunder struct {
	refundID  string
	status    domain.RefundStatus
	err       error
	calls     int
	getStatus domain.RefundStatus
}

func (r *stubRefunder) CreateRefund(_ context.Context, _ string, _ int64, _, _ string) (string, domain.RefundStatus, error) {
	r.calls++
	if r.err != nil {
		return "", "", r.err
	}
	return r.refundID, r.status, nil
}

func (r *stubRefunder) GetRefund(_ context.Context, _ string) (domain.RefundStatus, string, error) {
	if r.getStatus == "" {
		return domain.RefundFailed, "expired_or_canceled_card", nil
	}
	return r.getStatus, "", nil
}

func (r *stubRefunder) FindRefundForCreditNote(_ context.Context, _, _ string) (string, domain.RefundStatus, error) {
	return "", "", nil
}

// seedInvoiceForCN creates a customer + invoice in the given state. When
// paymentIntentID is non-empty it is stamped on the invoice (the store's Create
// does not persist it) so the refund legs have a PI to retry against.
func seedInvoiceForCN(t *testing.T, db *postgres.DB, tenantID, suffix string, status domain.InvoiceStatus, payStatus domain.InvoicePaymentStatus, paymentIntentID string) (domain.Customer, domain.Invoice) {
	t.Helper()
	ctx := postgres.WithLivemode(context.Background(), false)

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_routeaudit_" + suffix, DisplayName: "Route Audit " + suffix,
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	now := time.Now().UTC()
	issuedAt := now
	inv := domain.Invoice{
		CustomerID:         cust.ID,
		InvoiceNumber:      "INV-ROUTEAUDIT-" + suffix,
		Status:             status,
		PaymentStatus:      payStatus,
		Currency:           "USD",
		SubtotalCents:      10000,
		TotalAmountCents:   10000,
		BillingPeriodStart: now.Add(-30 * 24 * time.Hour),
		BillingPeriodEnd:   now,
		IssuedAt:           &issuedAt,
	}
	if status == domain.InvoicePaid {
		inv.AmountPaidCents = 10000
	} else {
		inv.AmountDueCents = 10000
	}
	created, err := invoice.NewPostgresStore(db).Create(ctx, tenantID, inv)
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}

	if paymentIntentID != "" {
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin PI stamp tx: %v", err)
		}
		defer postgres.Rollback(tx)
		if _, err := tx.ExecContext(ctx,
			`UPDATE invoices SET stripe_payment_intent_id=$1 WHERE id=$2`, paymentIntentID, created.ID); err != nil {
			t.Fatalf("stamp payment intent: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit PI stamp: %v", err)
		}
		created.StripePaymentIntentID = paymentIntentID
	}
	return cust, created
}

// TestCreateAudit_SharedFate pins the ADR-090 emission on the DRAFT CREATE
// (POST /v1/credit-notes) — the route that until now relied on the middleware
// catch-all guessing "create credit_note {id}" from the URL. The create row is
// built by the service and written on the credit-note INSERT's own transaction,
// so both directions hold: the note and its evidence commit together, and an
// emission failure leaves neither.
func TestCreateAudit_SharedFate(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "CN Create Audit")

	store := creditnote.NewPostgresStore(db)
	invStore := invoice.NewPostgresStore(db)
	logger := audit.NewLogger(db)

	t.Run("create commits the note and exactly one create row", func(t *testing.T) {
		cust, inv := seedInvoiceForCN(t, db, tenantID, "create-ok", domain.InvoiceFinalized, domain.PaymentPending, "")

		svc := creditnote.NewService(store, invStore, nil)
		svc.SetNumberGenerator(&seqNumbers{prefix: "CREATEOK"})
		svc.SetAuditLogger(logger)

		cn, err := svc.Create(ctx, tenantID, creditnote.CreateInput{
			InvoiceID: inv.ID,
			Reason:    "order_change",
			Lines: []creditnote.CreditLineInput{
				{Description: "over-billed seats", Quantity: 2, UnitAmountCents: 1500},
			},
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if cn.Status != domain.CreditNoteDraft {
			t.Fatalf("status: got %q, want draft", cn.Status)
		}

		rows := cnAuditRows(ctx, t, logger, tenantID, cn.ID)
		if len(rows) != 1 {
			t.Fatalf("want exactly one audit row for the create; got %+v", rows)
		}
		row := rows[0]
		if row.Action != domain.AuditActionCreate {
			t.Errorf("action: got %q, want %q", row.Action, domain.AuditActionCreate)
		}
		if row.ResourceType != "credit_note" || row.ResourceID != cn.ID {
			t.Errorf("resource: got %s/%s, want credit_note/%s", row.ResourceType, row.ResourceID, cn.ID)
		}
		if row.ResourceLabel != cn.CreditNoteNumber {
			t.Errorf("resource_label: got %q, want %q", row.ResourceLabel, cn.CreditNoteNumber)
		}
		if row.Metadata["invoice_id"] != inv.ID {
			t.Errorf("metadata invoice_id: got %v, want %s", row.Metadata["invoice_id"], inv.ID)
		}
		if row.Metadata["customer_id"] != cust.ID {
			t.Errorf("metadata customer_id: got %v, want %s", row.Metadata["customer_id"], cust.ID)
		}
		if got := row.Metadata["total_cents"]; fmt.Sprint(got) != "3000" {
			t.Errorf("metadata total_cents: got %v, want 3000", got)
		}
		if row.Metadata["reason"] != "order_change" {
			t.Errorf("metadata reason: got %v, want order_change", row.Metadata["reason"])
		}
		if row.Metadata["currency"] != "USD" {
			t.Errorf("metadata currency: got %v, want USD", row.Metadata["currency"])
		}
	})

	t.Run("emit failure rolls the credit note back entirely", func(t *testing.T) {
		_, inv := seedInvoiceForCN(t, db, tenantID, "create-fail", domain.InvoiceFinalized, domain.PaymentPending, "")

		emitter := &failingCNEmitter{}
		svc := creditnote.NewService(store, invStore, nil)
		svc.SetNumberGenerator(&seqNumbers{prefix: "CREATEFAIL"})
		svc.SetAuditLogger(emitter)

		_, err := svc.Create(ctx, tenantID, creditnote.CreateInput{
			InvoiceID: inv.ID,
			Reason:    "order_change",
			Lines: []creditnote.CreditLineInput{
				{Description: "must roll back", Quantity: 1, UnitAmountCents: 2500},
			},
		})
		if !errors.Is(err, errInjectedEmit) {
			t.Fatalf("want the injected emission error surfaced (shared fate); got %v", err)
		}
		if emitter.calls != 1 {
			t.Fatalf("emit fired %d times, want exactly 1 (the test is vacuous otherwise)", emitter.calls)
		}

		// The header AND its lines must be gone — the credit note never existed.
		cns, err := store.List(ctx, creditnote.ListFilter{TenantID: tenantID, InvoiceID: inv.ID})
		if err != nil {
			t.Fatalf("list credit notes: %v", err)
		}
		if len(cns) != 0 {
			t.Fatalf("credit note leaked from a failed audit emission: %+v", cns)
		}
	})

	t.Run("auto-issue emits one create row and one issued row", func(t *testing.T) {
		// The AutoIssue handler path = Create then Issue. On an UNPAID source the
		// issue reduces amount_due (no granter needed), so the two facts are the
		// create and the issue — never a second create.
		_, inv := seedInvoiceForCN(t, db, tenantID, "create-autoissue", domain.InvoiceFinalized, domain.PaymentPending, "")

		svc := creditnote.NewService(store, invStore, nil)
		svc.SetNumberGenerator(&seqNumbers{prefix: "AUTOISSUE"})
		svc.SetAuditLogger(logger)

		cn, err := svc.Create(ctx, tenantID, creditnote.CreateInput{
			InvoiceID: inv.ID,
			Reason:    "duplicate",
			Lines: []creditnote.CreditLineInput{
				{Description: "auto-issued", Quantity: 1, UnitAmountCents: 4000},
			},
			AutoIssue: true,
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		issued, err := svc.Issue(ctx, tenantID, cn.ID)
		if err != nil {
			t.Fatalf("Issue: %v", err)
		}
		if issued.Status != domain.CreditNoteIssued {
			t.Fatalf("status: got %q, want issued", issued.Status)
		}

		rows := cnAuditRows(ctx, t, logger, tenantID, cn.ID)
		if len(rows) != 2 {
			t.Fatalf("want exactly two audit rows (create + issued); got %+v", rows)
		}
		counts := map[string]int{}
		for _, r := range rows {
			counts[r.Action]++
		}
		if counts[domain.AuditActionCreate] != 1 || counts["credit_note.issued"] != 1 {
			t.Fatalf("want one create + one credit_note.issued row; got %v", counts)
		}
	})
}

// TestVoidAudit_SharedFate pins the ADR-090 emission on POST
// /v1/credit-notes/{id}/void. The flip is CAS-gated (draft→voided), so the row
// can only evidence a transition that actually happened: a lost CAS (someone
// else already voided) writes nothing, and an emission failure rolls the flip
// back. Drafts are seeded through the store directly, so the ONLY row a test
// can see is the void row.
func TestVoidAudit_SharedFate(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "CN Void Audit")

	store := creditnote.NewPostgresStore(db)
	invStore := invoice.NewPostgresStore(db)
	logger := audit.NewLogger(db)

	seedDraft := func(t *testing.T, suffix string) domain.CreditNote {
		t.Helper()
		cust, inv := seedInvoiceForCN(t, db, tenantID, suffix, domain.InvoiceFinalized, domain.PaymentPending, "")
		cn, err := store.Create(ctx, tenantID, domain.CreditNote{
			InvoiceID:         inv.ID,
			CustomerID:        cust.ID,
			CreditNoteNumber:  "CN-VOIDAUDIT-" + suffix,
			Status:            domain.CreditNoteDraft,
			Reason:            "order_change",
			SubtotalCents:     5000,
			TotalCents:        5000,
			CreditAmountCents: 5000,
			Currency:          "USD",
			RefundStatus:      domain.RefundNone,
		})
		if err != nil {
			t.Fatalf("seed draft credit note: %v", err)
		}
		return cn
	}

	t.Run("void commits the flip and exactly one void row", func(t *testing.T) {
		cn := seedDraft(t, "void-ok")

		svc := creditnote.NewService(store, invStore, nil)
		svc.SetNumberGenerator(&seqNumbers{prefix: "VOIDOK"})
		svc.SetAuditLogger(logger)

		voided, err := svc.Void(ctx, tenantID, cn.ID)
		if err != nil {
			t.Fatalf("Void: %v", err)
		}
		if voided.Status != domain.CreditNoteVoided {
			t.Fatalf("status: got %q, want voided", voided.Status)
		}

		rows := cnAuditRows(ctx, t, logger, tenantID, cn.ID)
		if len(rows) != 1 {
			t.Fatalf("want exactly one audit row for the void; got %+v", rows)
		}
		row := rows[0]
		if row.Action != domain.AuditActionVoid {
			t.Errorf("action: got %q, want %q", row.Action, domain.AuditActionVoid)
		}
		if row.ResourceLabel != cn.CreditNoteNumber {
			t.Errorf("resource_label: got %q, want %q", row.ResourceLabel, cn.CreditNoteNumber)
		}
		if row.Metadata["credit_note_number"] != cn.CreditNoteNumber {
			t.Errorf("metadata credit_note_number: got %v, want %q", row.Metadata["credit_note_number"], cn.CreditNoteNumber)
		}
		if row.Metadata["invoice_id"] != cn.InvoiceID {
			t.Errorf("metadata invoice_id: got %v, want %s", row.Metadata["invoice_id"], cn.InvoiceID)
		}
		if row.Metadata["customer_id"] != cn.CustomerID {
			t.Errorf("metadata customer_id: got %v, want %s", row.Metadata["customer_id"], cn.CustomerID)
		}
	})

	t.Run("emit failure rolls the void back", func(t *testing.T) {
		cn := seedDraft(t, "void-fail")

		emitter := &failingCNEmitter{}
		svc := creditnote.NewService(store, invStore, nil)
		svc.SetNumberGenerator(&seqNumbers{prefix: "VOIDFAIL"})
		svc.SetAuditLogger(emitter)

		if _, err := svc.Void(ctx, tenantID, cn.ID); !errors.Is(err, errInjectedEmit) {
			t.Fatalf("want the injected emission error surfaced (shared fate); got %v", err)
		}
		if emitter.calls != 1 {
			t.Fatalf("emit fired %d times, want exactly 1 (the test is vacuous otherwise)", emitter.calls)
		}

		got, err := store.Get(ctx, tenantID, cn.ID)
		if err != nil {
			t.Fatalf("get credit note: %v", err)
		}
		if got.Status != domain.CreditNoteDraft {
			t.Fatalf("status: got %q, want draft — the void must roll back with its failed audit emission", got.Status)
		}
		if got.VoidedAt != nil {
			t.Errorf("voided_at must be unset after rollback; got %v", got.VoidedAt)
		}
		if rows := cnAuditRows(ctx, t, logger, tenantID, cn.ID); len(rows) != 0 {
			t.Errorf("audit row leaked from a rolled-back void: %+v", rows)
		}
	})

	t.Run("already-voided note is rejected and emits nothing", func(t *testing.T) {
		cn := seedDraft(t, "void-twice")

		svc := creditnote.NewService(store, invStore, nil)
		svc.SetNumberGenerator(&seqNumbers{prefix: "VOIDTWICE"})
		svc.SetAuditLogger(logger)

		if _, err := svc.Void(ctx, tenantID, cn.ID); err != nil {
			t.Fatalf("first Void: %v", err)
		}
		if _, err := svc.Void(ctx, tenantID, cn.ID); err == nil {
			t.Fatal("a second Void must be rejected — the note is already voided")
		}

		rows := cnAuditRows(ctx, t, logger, tenantID, cn.ID)
		if len(rows) != 1 {
			t.Fatalf("want exactly one void row across two attempts (no fabricated second void); got %+v", rows)
		}
	})

	// The guard rejects an ALREADY-issued note before the CAS is ever reached.
	// That is the easy half; the subtest after this one exercises the race the
	// CAS actually exists for.
	t.Run("void of an already-issued note is rejected by the guard and emits nothing", func(t *testing.T) {
		cn := seedDraft(t, "void-lostcas")

		svc := creditnote.NewService(store, invStore, nil)
		svc.SetNumberGenerator(&seqNumbers{prefix: "VOIDLOST"})
		svc.SetAuditLogger(logger)

		won, err := store.TransitionStatus(ctx, tenantID, cn.ID, domain.CreditNoteDraft, domain.CreditNoteIssued)
		if err != nil || !won {
			t.Fatalf("seed the concurrent issue: won=%v err=%v", won, err)
		}
		if _, err := svc.Void(ctx, tenantID, cn.ID); err == nil {
			t.Fatal("void of an issued credit note must be rejected")
		}

		got, err := store.Get(ctx, tenantID, cn.ID)
		if err != nil {
			t.Fatalf("get credit note: %v", err)
		}
		if got.Status != domain.CreditNoteIssued {
			t.Fatalf("status: got %q, want issued — the blind write must not clobber the issued state", got.Status)
		}
		if rows := cnAuditRows(ctx, t, logger, tenantID, cn.ID); len(rows) != 0 {
			t.Errorf("audit row emitted for a void that never happened: %+v", rows)
		}
	})

	// THE RACE THE CAS EXISTS FOR. Void's guards read the note on a DIFFERENT
	// transaction than its flip, so an Issue() can land in between: the guard
	// sees 'draft' and admits the void, while the row is already 'issued' with
	// the customer's credit granted. Pre-fix, Void then blind-wrote 'voided'
	// over it — credit granted against a note recorded as void, and (because
	// the over-credit ceiling sums only non-voided notes) the same money
	// creditable a second time.
	//
	// A stale-read store reproduces that interleaving deterministically: the
	// guard is handed a 'draft' snapshot while the DB row is already 'issued'.
	// The CAS must LOSE — no flip, no audit row, and a truthful error.
	t.Run("void that loses the CAS mid-flight leaves the issued state and emits nothing", func(t *testing.T) {
		cn := seedDraft(t, "void-caslost-race")

		draftSnapshot, err := store.Get(ctx, tenantID, cn.ID)
		if err != nil {
			t.Fatalf("snapshot the draft: %v", err)
		}

		// The concurrent Issue() commits — the note is now issued in the DB.
		won, err := store.TransitionStatus(ctx, tenantID, cn.ID, domain.CreditNoteDraft, domain.CreditNoteIssued)
		if err != nil || !won {
			t.Fatalf("seed the concurrent issue: won=%v err=%v", won, err)
		}

		// ...but Void's guard still sees the pre-issue snapshot.
		svc := creditnote.NewService(staleReadStore{Store: store, stale: draftSnapshot}, invStore, nil)
		svc.SetNumberGenerator(&seqNumbers{prefix: "VOIDRACE"})
		svc.SetAuditLogger(logger)

		if _, err := svc.Void(ctx, tenantID, cn.ID); err == nil {
			t.Fatal("a void that loses the CAS must return an error, not silently report success")
		}

		got, err := store.Get(ctx, tenantID, cn.ID)
		if err != nil {
			t.Fatalf("get credit note: %v", err)
		}
		if got.Status != domain.CreditNoteIssued {
			t.Fatalf("status: got %q, want issued — the losing void must not clobber the issued state (this is the money bug)", got.Status)
		}
		if rows := cnAuditRows(ctx, t, logger, tenantID, cn.ID); len(rows) != 0 {
			t.Errorf("audit row emitted for a void the CAS rejected: %+v", rows)
		}
	})
}

// staleReadStore hands Void's guard a stale snapshot of the credit note while
// the real row has already moved on — the deterministic stand-in for a
// concurrent Issue() landing between the guard read and the flip. Every other
// method (including the CAS itself) goes to the real store.
type staleReadStore struct {
	creditnote.Store
	stale domain.CreditNote
}

func (s staleReadStore) Get(_ context.Context, _, _ string) (domain.CreditNote, error) {
	return s.stale, nil
}

// TestRetryRefundAudit_SharedFate pins the ADR-090 emission on POST
// /v1/credit-notes/{id}/retry-refund — a MONEY path that re-fires a Stripe
// refund. The row rides the refund-state persist and is gated on the state
// ACTUALLY moving: an idempotent re-drive that returns the same 'pending'
// records nothing.
func TestRetryRefundAudit_SharedFate(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "CN Retry Refund Audit")

	store := creditnote.NewPostgresStore(db)
	invStore := invoice.NewPostgresStore(db)
	logger := audit.NewLogger(db)

	// seedIssuedRefundCN: an issued, refund-bearing CN on a paid invoice with a
	// PaymentIntent — the retry-refund preconditions.
	seedIssuedRefundCN := func(t *testing.T, suffix string, status domain.RefundStatus, refundID string) domain.CreditNote {
		t.Helper()
		cust, inv := seedInvoiceForCN(t, db, tenantID, suffix, domain.InvoicePaid, domain.PaymentSucceeded, "pi_"+suffix)
		cn, err := store.Create(ctx, tenantID, domain.CreditNote{
			InvoiceID:         inv.ID,
			CustomerID:        cust.ID,
			CreditNoteNumber:  "CN-RETRYAUDIT-" + suffix,
			Status:            domain.CreditNoteIssued,
			Reason:            "fraudulent",
			SubtotalCents:     5000,
			TotalCents:        5000,
			RefundAmountCents: 5000,
			Currency:          "USD",
			RefundStatus:      domain.RefundNone,
		})
		if err != nil {
			t.Fatalf("seed credit note: %v", err)
		}
		// Stamp the starting refund leg state (Create doesn't persist the id).
		if err := store.UpdateRefundStatus(ctx, tenantID, cn.ID, status, refundID); err != nil {
			t.Fatalf("stamp refund state: %v", err)
		}
		got, err := store.Get(ctx, tenantID, cn.ID)
		if err != nil {
			t.Fatalf("re-read seeded credit note: %v", err)
		}
		return got
	}

	t.Run("failed to succeeded emits one refund_retried row", func(t *testing.T) {
		cn := seedIssuedRefundCN(t, "retry-ok", domain.RefundFailed, "")

		refunder := &stubRefunder{refundID: "re_retry_ok", status: domain.RefundSucceeded}
		svc := creditnote.NewService(store, invStore, refunder)
		svc.SetNumberGenerator(&seqNumbers{prefix: "RETRYOK"})
		svc.SetAuditLogger(logger)

		got, err := svc.RetryRefund(ctx, tenantID, cn.ID)
		if err != nil {
			t.Fatalf("RetryRefund: %v", err)
		}
		if refunder.calls != 1 {
			t.Fatalf("stripe refund fired %d times, want 1", refunder.calls)
		}
		if got.RefundStatus != domain.RefundSucceeded || got.StripeRefundID != "re_retry_ok" {
			t.Fatalf("persisted refund leg: got (%s, %s), want (succeeded, re_retry_ok)", got.RefundStatus, got.StripeRefundID)
		}

		rows := cnAuditRows(ctx, t, logger, tenantID, cn.ID)
		if len(rows) != 1 {
			t.Fatalf("want exactly one audit row for the retry; got %+v", rows)
		}
		row := rows[0]
		if row.Action != domain.AuditActionUpdate {
			t.Errorf("action: got %q, want %q (frozen vocabulary + metadata discriminator)", row.Action, domain.AuditActionUpdate)
		}
		if row.ResourceType != "credit_note" || row.ResourceID != cn.ID {
			t.Errorf("resource: got %s/%s, want credit_note/%s", row.ResourceType, row.ResourceID, cn.ID)
		}
		if row.Metadata["action"] != "refund_retried" {
			t.Errorf("metadata action: got %v, want refund_retried", row.Metadata["action"])
		}
		if row.Metadata["refund_status"] != string(domain.RefundSucceeded) {
			t.Errorf("metadata refund_status: got %v, want succeeded", row.Metadata["refund_status"])
		}
		if row.Metadata["prior_refund_status"] != string(domain.RefundFailed) {
			t.Errorf("metadata prior_refund_status: got %v, want failed", row.Metadata["prior_refund_status"])
		}
		if row.Metadata["stripe_refund_id"] != "re_retry_ok" {
			t.Errorf("metadata stripe_refund_id: got %v, want re_retry_ok", row.Metadata["stripe_refund_id"])
		}
		if row.Metadata["invoice_id"] != cn.InvoiceID {
			t.Errorf("metadata invoice_id: got %v, want %s", row.Metadata["invoice_id"], cn.InvoiceID)
		}
	})

	// An operator's retry is a MONEY ACTION: it issues a real refund request
	// against the customer's payment. Stripe may idempotently converge (same
	// refund id, same status) so nothing in the DB moves — but the attempt
	// happened, and a compliance log that omits repeated cash-back attempts
	// because they converged is lying by silence. So the retry emits even when
	// the persisted state does not move; `status_changed` distinguishes the two.
	//
	// This is the DELIBERATE OPPOSITE of ApplyRefundWebhook, where a no-op
	// redelivery is a non-event Stripe happened to send twice and recording it
	// would fabricate a transition that never occurred.
	t.Run("idempotent re-drive still records the operator's retry, flagged as no-change", func(t *testing.T) {
		cn := seedIssuedRefundCN(t, "retry-noop", domain.RefundPending, "re_retry_noop")

		// ADR-063 amendment: the retry reconciles first. The provider
		// confirms the refund is STILL pending → no second refund is minted
		// (the old flow re-fired Create and leaned on key replay), the row
		// is unchanged, and the operator gets a clear refusal. The reconcile
		// that hit the provider is still recorded, flagged as no-change.
		refunder := &stubRefunder{getStatus: domain.RefundPending}
		svc := creditnote.NewService(store, invStore, refunder)
		svc.SetNumberGenerator(&seqNumbers{prefix: "RETRYNOOP"})
		svc.SetAuditLogger(logger)

		_, err := svc.RetryRefund(ctx, tenantID, cn.ID)
		if err == nil {
			t.Fatal("want the still-processing refusal; a pending refund must not be re-fired")
		}
		if refunder.calls != 0 {
			t.Fatalf("stripe create fired %d times for an in-flight refund, want 0", refunder.calls)
		}
		got, gerr := store.Get(ctx, tenantID, cn.ID)
		if gerr != nil {
			t.Fatalf("get: %v", gerr)
		}
		if got.RefundStatus != domain.RefundPending || got.StripeRefundID != "re_retry_noop" {
			t.Fatalf("persisted refund leg moved: got (%s, %s), want (pending, re_retry_noop)", got.RefundStatus, got.StripeRefundID)
		}
		rows := cnAuditRows(ctx, t, logger, tenantID, cn.ID)
		if len(rows) != 1 {
			t.Fatalf("the operator's reconcile must be recorded even when nothing moved; got %d rows: %+v", len(rows), rows)
		}
		if rows[0].Metadata["action"] != "refund_retried" {
			t.Errorf("metadata action: got %v, want refund_retried", rows[0].Metadata["action"])
		}
		if changed, _ := rows[0].Metadata["status_changed"].(bool); changed {
			t.Errorf("status_changed: got true, want false — the row must say the state did not move")
		}
		if rec, _ := rows[0].Metadata["reconciled_from_provider"].(bool); !rec {
			t.Errorf("reconciled_from_provider: want true — the row must say where the answer came from")
		}
	})

	t.Run("emit failure rolls the refund-state persist back", func(t *testing.T) {
		cn := seedIssuedRefundCN(t, "retry-fail", domain.RefundFailed, "")

		emitter := &failingCNEmitter{}
		refunder := &stubRefunder{refundID: "re_retry_fail", status: domain.RefundSucceeded}
		svc := creditnote.NewService(store, invStore, refunder)
		svc.SetNumberGenerator(&seqNumbers{prefix: "RETRYFAIL"})
		svc.SetAuditLogger(emitter)

		if _, err := svc.RetryRefund(ctx, tenantID, cn.ID); !errors.Is(err, errInjectedEmit) {
			t.Fatalf("want the injected emission error surfaced (shared fate); got %v", err)
		}
		if emitter.calls != 1 {
			t.Fatalf("emit fired %d times, want exactly 1 (the test is vacuous otherwise)", emitter.calls)
		}

		got, err := store.Get(ctx, tenantID, cn.ID)
		if err != nil {
			t.Fatalf("get credit note: %v", err)
		}
		// The persist rolls back with its failed emission: refund_status stays
		// failed and the Stripe refund id is NOT stamped. The next retry
		// re-converges on the same idempotency key (Stripe returns the same
		// refund), so no cash is at risk — but the log never claims a state the
		// database doesn't hold.
		if got.RefundStatus != domain.RefundFailed {
			t.Fatalf("refund_status: got %q, want failed — the persist must roll back with its failed audit emission", got.RefundStatus)
		}
		if got.StripeRefundID != "" {
			t.Errorf("stripe_refund_id must not be stamped by a rolled-back persist; got %q", got.StripeRefundID)
		}
		if rows := cnAuditRows(ctx, t, logger, tenantID, cn.ID); len(rows) != 0 {
			t.Errorf("audit row leaked from a rolled-back persist: %+v", rows)
		}
	})

	t.Run("stripe error records both facts: the reconcile move and the failed create", func(t *testing.T) {
		cn := seedIssuedRefundCN(t, "retry-stripefail", domain.RefundPending, "re_retry_stripefail")

		// Provider says the stored refund is dead (stub GetRefund defaults to
		// failed) → the reconcile persists pending→failed with the provider's
		// reason; the re-drive create then errors. TWO audit rows, each a real
		// fact: the state move the provider reported, and the operator's
		// create attempt that failed. The old single-row expectation encoded
		// the pre-reconcile flow.
		refunder := &stubRefunder{err: errors.New("stripe: card_declined")}
		svc := creditnote.NewService(store, invStore, refunder)
		svc.SetNumberGenerator(&seqNumbers{prefix: "RETRYSTRIPEFAIL"})
		svc.SetAuditLogger(logger)

		if _, err := svc.RetryRefund(ctx, tenantID, cn.ID); err == nil {
			t.Fatal("RetryRefund must surface the Stripe error")
		}

		got, err := store.Get(ctx, tenantID, cn.ID)
		if err != nil {
			t.Fatalf("get credit note: %v", err)
		}
		if got.RefundStatus != domain.RefundFailed {
			t.Fatalf("refund_status: got %q, want failed", got.RefundStatus)
		}

		rows := cnAuditRows(ctx, t, logger, tenantID, cn.ID)
		if len(rows) != 2 {
			t.Fatalf("want two audit rows (reconcile move + failed create attempt); got %+v", rows)
		}
		// cnAuditRows returns newest-first: rows[1] is the reconcile, rows[0]
		// the create failure.
		if rec, _ := rows[1].Metadata["reconciled_from_provider"].(bool); !rec {
			t.Errorf("first row must be the provider reconcile; got %+v", rows[1].Metadata)
		}
		if rows[1].Metadata["prior_refund_status"] != string(domain.RefundPending) {
			t.Errorf("reconcile prior: got %v, want pending", rows[1].Metadata["prior_refund_status"])
		}
		if rows[1].Metadata["provider_failure_reason"] != "expired_or_canceled_card" {
			t.Errorf("reconcile must carry the provider failure reason; got %v", rows[1].Metadata["provider_failure_reason"])
		}
		if rows[0].Metadata["provider_error"] != "stripe: card_declined" {
			t.Errorf("create-failure row must carry provider_error; got %v", rows[0].Metadata["provider_error"])
		}
		if changed, _ := rows[0].Metadata["status_changed"].(bool); changed {
			t.Errorf("failed→failed create record: status_changed must be false")
		}
	})
}
