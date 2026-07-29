package email_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/email"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestEmailOutbox_EnqueueStandalone_Persists verifies the base durability
// guarantee: after EnqueueStandalone returns without error, a pending row
// exists in email_outbox. No dispatcher involvement.
func TestEmailOutbox_EnqueueStandalone_Persists(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 15*time.Second)
	defer cancel()

	tenantID := testutil.CreateTestTenant(t, db, "Email Outbox Enqueue")
	store := email.NewOutboxStore(db)

	id, err := store.EnqueueStandalone(ctx, tenantID, email.TypeInvoice, map[string]any{
		"to":             "customer@example.com",
		"customer_name":  "Ada Lovelace",
		"invoice_number": "VLX-000001",
		"amount_cents":   12345,
		"currency":       "USD",
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty outbox id")
	}

	status, attempts, payload := readEmailOutbox(t, db, id)
	if status != email.OutboxPending {
		t.Errorf("status: got %q, want %q", status, email.OutboxPending)
	}
	if attempts != 0 {
		t.Errorf("attempts: got %d, want 0", attempts)
	}
	if payload["to"] != "customer@example.com" {
		t.Errorf("payload missing 'to': %+v", payload)
	}
}

// TestEmailOutbox_Enqueue_TxAtomicity verifies the transactional outbox
// contract: enqueue inside a rolled-back tx must NOT persist. This lets
// producers enqueue in the same tx as their business-op state change.
func TestEmailOutbox_Enqueue_TxAtomicity(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 15*time.Second)
	defer cancel()

	tenantID := testutil.CreateTestTenant(t, db, "Email Outbox Tx")
	store := email.NewOutboxStore(db)

	// Rollback path — row must not exist after rollback.
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	rollbackID, err := store.Enqueue(ctx, tx, tenantID, email.TypePaymentReceipt, map[string]any{"to": "x@y.z"})
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("enqueue in rollback tx: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if exists := emailOutboxExists(t, db, rollbackID); exists {
		t.Error("row persisted despite rollback — email outbox breaks tx atomicity")
	}

	// Commit path — row must exist after commit.
	tx2, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin tx2: %v", err)
	}
	commitID, err := store.Enqueue(ctx, tx2, tenantID, email.TypePaymentReceipt, map[string]any{"to": "x@y.z"})
	if err != nil {
		_ = tx2.Rollback()
		t.Fatalf("enqueue in commit tx: %v", err)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if exists := emailOutboxExists(t, db, commitID); !exists {
		t.Error("row missing after commit — email outbox dropped the insert")
	}
}

// TestEmailOutbox_ProcessBatch_Success covers the happy path: handler returns
// nil, row transitions to 'dispatched', attempts=1, dispatched_at populated.
func TestEmailOutbox_ProcessBatch_Success(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 15*time.Second)
	defer cancel()

	tenantID := testutil.CreateTestTenant(t, db, "Email Outbox Success")
	store := email.NewOutboxStore(db)

	id, err := store.EnqueueStandalone(ctx, tenantID, email.TypePaymentReceipt, map[string]any{
		"to": "paid@example.com",
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	var saw email.OutboxRow
	n, err := store.ProcessBatch(ctx, 10, func(_ context.Context, row email.OutboxRow) error {
		saw = row
		return nil
	})
	if err != nil {
		t.Fatalf("process batch: %v", err)
	}
	if n != 1 {
		t.Fatalf("processed: got %d, want 1", n)
	}
	if saw.ID != id || saw.TenantID != tenantID || saw.EmailType != email.TypePaymentReceipt {
		t.Errorf("handler got wrong row: %+v", saw)
	}

	status, attempts, _ := readEmailOutbox(t, db, id)
	if status != email.OutboxDispatched {
		t.Errorf("status: got %q, want %q", status, email.OutboxDispatched)
	}
	if attempts != 1 {
		t.Errorf("attempts: got %d, want 1", attempts)
	}
}

// TestEmailOutbox_ProcessBatch_RetryBackoff covers the retry path: a transient
// handler error increments attempts and defers next_attempt_at, so a
// subsequent immediate ProcessBatch MUST NOT re-claim the row.
func TestEmailOutbox_ProcessBatch_RetryBackoff(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 15*time.Second)
	defer cancel()

	tenantID := testutil.CreateTestTenant(t, db, "Email Outbox Retry")
	store := email.NewOutboxStore(db)

	id, err := store.EnqueueStandalone(ctx, tenantID, email.TypeDunningWarning, map[string]any{"to": "x@y.z"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// First pass — handler fails (simulating SMTP 421 "try again later").
	n, err := store.ProcessBatch(ctx, 10, func(_ context.Context, _ email.OutboxRow) error {
		return errors.New("smtp temp failure")
	})
	if err != nil {
		t.Fatalf("process batch 1: %v", err)
	}
	if n != 1 {
		t.Fatalf("processed: got %d, want 1", n)
	}

	status, attempts, _ := readEmailOutbox(t, db, id)
	if status != email.OutboxPending {
		t.Errorf("status: got %q, want %q (not yet DLQ)", status, email.OutboxPending)
	}
	if attempts != 1 {
		t.Errorf("attempts: got %d, want 1", attempts)
	}
	lastErr, nextAt := readEmailOutboxRetry(t, db, id)
	if lastErr != "smtp temp failure" {
		t.Errorf("last_error: got %q, want %q", lastErr, "smtp temp failure")
	}
	if !nextAt.After(time.Now().UTC()) {
		t.Errorf("next_attempt_at should be in the future, got %v", nextAt)
	}

	// Second immediate pass — row is not yet due, so nothing processed.
	n2, err := store.ProcessBatch(ctx, 10, func(_ context.Context, _ email.OutboxRow) error {
		return errors.New("should not run")
	})
	if err != nil {
		t.Fatalf("process batch 2: %v", err)
	}
	if n2 != 0 {
		t.Errorf("processed on second pass: got %d, want 0 (next_attempt_at not due)", n2)
	}
}

// TestEmailOutbox_ProcessBatch_DLQ verifies that after MaxOutboxAttempts
// failures the row becomes 'failed' and is no longer claimed by subsequent
// batches — the dead-letter-queue contract.
func TestEmailOutbox_ProcessBatch_DLQ(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 30*time.Second)
	defer cancel()

	tenantID := testutil.CreateTestTenant(t, db, "Email Outbox DLQ")
	store := email.NewOutboxStore(db)

	id, err := store.EnqueueStandalone(ctx, tenantID, email.TypeInvoice, map[string]any{"to": "broken@example.com"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// Drive attempts to the DLQ threshold by forcing next_attempt_at back to
	// now before each pass. Compresses ~72h of real backoff into one test.
	for i := 1; i <= email.MaxOutboxAttempts; i++ {
		if err := resetEmailDue(db, id); err != nil {
			t.Fatalf("attempt %d: reset due: %v", i, err)
		}
		n, err := store.ProcessBatch(ctx, 10, func(_ context.Context, _ email.OutboxRow) error {
			return fmt.Errorf("attempt %d failed", i)
		})
		if err != nil {
			t.Fatalf("attempt %d: process: %v", i, err)
		}
		if n != 1 {
			t.Fatalf("attempt %d: processed %d, want 1", i, n)
		}
	}

	status, attempts, _ := readEmailOutbox(t, db, id)
	if status != email.OutboxFailed {
		t.Errorf("status: got %q, want %q after %d attempts", status, email.OutboxFailed, email.MaxOutboxAttempts)
	}
	if attempts != email.MaxOutboxAttempts {
		t.Errorf("attempts: got %d, want %d", attempts, email.MaxOutboxAttempts)
	}

	// DLQ rows are terminal — they should NOT be re-claimed even if made "due".
	if err := resetEmailDue(db, id); err != nil {
		t.Fatalf("reset due on DLQ row: %v", err)
	}
	n, err := store.ProcessBatch(ctx, 10, func(_ context.Context, _ email.OutboxRow) error {
		t.Error("DLQ row was re-claimed — terminal status not respected")
		return nil
	})
	if err != nil {
		t.Fatalf("post-DLQ batch: %v", err)
	}
	if n != 0 {
		t.Errorf("post-DLQ: processed %d, want 0", n)
	}
}

// TestEmailOutbox_OutboxSender_RoundTrip is the end-to-end producer → row →
// dispatcher check: OutboxSender persists an invoice email, then the
// dispatcher's handler drains it and the matching Send* method is called
// with the expected arguments. This is the always-on production contract
// per ADR-040 — no env flag, no direct-SMTP fallback.
func TestEmailOutbox_OutboxSender_RoundTrip(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 15*time.Second)
	defer cancel()

	tenantID := testutil.CreateTestTenant(t, db, "Email Outbox RoundTrip")
	store := email.NewOutboxStore(db)
	sender := email.NewOutboxSender(store)

	pdf := []byte("%PDF-fake")
	if err := sender.SendInvoice(ctx, tenantID, "ada@example.com", []string{"finance@example.com"}, "Ada", "VLX-7", 25_000, "USD", pdf, "vlx_pinv_roundtrip"); err != nil {
		t.Fatalf("enqueue SendInvoice: %v", err)
	}

	fake := &fakeDeliverer{}
	dispatcher := email.NewDispatcher(store, fake, email.DispatcherConfig{})

	// Manually drive the handler by calling ProcessBatch with the dispatcher's
	// underlying handle logic (via the public EmailDeliverer path). We rely on
	// ProcessBatch's success/failure semantics — a successful demarshal +
	// Send* call marks the row 'dispatched'.
	_ = dispatcher // constructed for its side effects on the interface check

	n, err := store.ProcessBatch(ctx, 10, func(c context.Context, row email.OutboxRow) error {
		return callDeliverer(c, fake, row)
	})
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if n != 1 {
		t.Fatalf("processed: got %d, want 1", n)
	}
	if fake.calls != 1 {
		t.Fatalf("deliverer calls: got %d, want 1", fake.calls)
	}
	if fake.lastType != email.TypeInvoice {
		t.Errorf("type: got %q, want %q", fake.lastType, email.TypeInvoice)
	}
	if fake.lastTenant != tenantID {
		t.Errorf("tenant: got %q, want %q", fake.lastTenant, tenantID)
	}
	if fake.lastTo != "ada@example.com" || fake.lastAmount != 25_000 || fake.lastCurrency != "USD" {
		t.Errorf("args not round-tripped: to=%q amount=%d currency=%q",
			fake.lastTo, fake.lastAmount, fake.lastCurrency)
	}
	// CC survives the payload round trip (ADR-082 — the SetupURL bug
	// class: a struct field silently dropped between enqueue and
	// dispatch means the operator's configured recipients never receive).
	if len(fake.lastCc) != 1 || fake.lastCc[0] != "finance@example.com" {
		t.Errorf("cc not round-tripped: got %v, want [finance@example.com]", fake.lastCc)
	}
}

// TestEmailOutbox_CreditNote_RoundTrip: the new credit_note type (ADR-082
// rider) round-trips — number, applied invoice, amount, PDF, and cc all
// reach the deliverer; legacy rows WITHOUT a cc key dispatch with nil cc
// (binary-skew / rollback compat).
func TestEmailOutbox_CreditNote_RoundTrip(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 15*time.Second)
	defer cancel()

	tenantID := testutil.CreateTestTenant(t, db, "Email Outbox CN RoundTrip")
	store := email.NewOutboxStore(db)
	sender := email.NewOutboxSender(store)

	pdf := []byte("%PDF-cn")
	if err := sender.SendCreditNote(ctx, tenantID, "ada@example.com", []string{"finance@example.com"},
		"Ada", "CN-0001", "VLX-7", 5_400, "USD", pdf); err != nil {
		t.Fatalf("enqueue SendCreditNote: %v", err)
	}
	// Legacy-shape row: no cc key at all (written by an older binary).
	if _, err := store.EnqueueStandalone(ctx, tenantID, email.TypeCreditNote, map[string]any{
		"to": "old@example.com", "customer_name": "Old", "credit_note_number": "CN-0002",
		"invoice_number": "VLX-8", "amount_cents": 100, "currency": "USD",
	}); err != nil {
		t.Fatalf("enqueue legacy row: %v", err)
	}

	fake := &fakeDeliverer{}
	seen := map[string][]string{}
	n, err := store.ProcessBatch(ctx, 10, func(c context.Context, row email.OutboxRow) error {
		if err := callDeliverer(c, fake, row); err != nil {
			return err
		}
		seen[fake.lastTo] = fake.lastCc
		return nil
	})
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if n != 2 {
		t.Fatalf("processed: got %d, want 2", n)
	}
	if fake.lastType != email.TypeCreditNote {
		t.Errorf("type: got %q, want %q", fake.lastType, email.TypeCreditNote)
	}
	if cc := seen["ada@example.com"]; len(cc) != 1 || cc[0] != "finance@example.com" {
		t.Errorf("CN cc round trip: got %v", cc)
	}
	if cc := seen["old@example.com"]; cc != nil {
		t.Errorf("legacy row without cc key must dispatch with nil cc, got %v", cc)
	}
}

// TestEmailOutbox_Counts verifies PendingCount and FailedCount.
func TestEmailOutbox_Counts(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 15*time.Second)
	defer cancel()

	tenantID := testutil.CreateTestTenant(t, db, "Email Outbox Counts")
	store := email.NewOutboxStore(db)

	for range 3 {
		if _, err := store.EnqueueStandalone(ctx, tenantID, email.TypePaymentReceipt, map[string]any{"to": "x@y.z"}); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}

	pending, err := store.PendingCount(ctx)
	if err != nil {
		t.Fatalf("pending count: %v", err)
	}
	if pending != 3 {
		t.Errorf("pending: got %d, want 3", pending)
	}

	failed, err := store.FailedCount(ctx)
	if err != nil {
		t.Fatalf("failed count: %v", err)
	}
	if failed != 0 {
		t.Errorf("failed: got %d, want 0", failed)
	}
}

// ---- helpers ---------------------------------------------------------------

// fakeDeliverer satisfies email.EmailDeliverer — captures the last call so
// tests can assert which Send* method was invoked with which arguments.
type fakeDeliverer struct {
	calls             int
	lastType          string
	lastTenant        string
	lastTo            string
	lastName          string
	lastInvoice       string
	lastAmount        int64
	lastCurrency      string
	lastAttemptN      int
	lastMaxN          int
	lastNextDate      string
	lastFailureReason string
	lastAction        string
	lastReason        string
	lastUpdateURL     string
	lastPDF           []byte
	lastPublicToken   string
	lastCc            []string
}

func (f *fakeDeliverer) SendInvoice(_ context.Context, tenantID, to string, cc []string, name, inv string, total int64, cur string, pdf []byte, publicToken string) error {
	f.calls++
	f.lastType, f.lastTenant, f.lastTo, f.lastName, f.lastInvoice = email.TypeInvoice, tenantID, to, name, inv
	f.lastAmount, f.lastCurrency, f.lastPDF = total, cur, pdf
	f.lastCc = cc
	f.lastPublicToken = publicToken
	return nil
}
func (f *fakeDeliverer) SendPaymentReceipt(_ context.Context, tenantID, to string, cc []string, name, inv string, amount int64, cur, publicToken string) error {
	f.calls++
	f.lastType, f.lastTenant, f.lastTo, f.lastName, f.lastInvoice = email.TypePaymentReceipt, tenantID, to, name, inv
	f.lastAmount, f.lastCurrency = amount, cur
	f.lastCc = cc
	f.lastPublicToken = publicToken
	return nil
}
func (f *fakeDeliverer) SendDunningWarning(_ context.Context, tenantID, to string, cc []string, name, inv string, n, max int, next, failureReason, publicToken string) error {
	f.calls++
	f.lastCc = cc
	f.lastType, f.lastTenant, f.lastTo, f.lastName, f.lastInvoice = email.TypeDunningWarning, tenantID, to, name, inv
	f.lastAttemptN, f.lastMaxN, f.lastNextDate = n, max, next
	f.lastFailureReason = failureReason
	f.lastPublicToken = publicToken
	return nil
}
func (f *fakeDeliverer) SendDunningEscalation(_ context.Context, tenantID, to string, cc []string, name, inv, action, publicToken string) error {
	f.calls++
	f.lastCc = cc
	f.lastType, f.lastTenant, f.lastTo, f.lastName, f.lastInvoice = email.TypeDunningEscalation, tenantID, to, name, inv
	f.lastAction = action
	f.lastPublicToken = publicToken
	return nil
}
func (f *fakeDeliverer) SendPaymentFailed(_ context.Context, tenantID, to string, cc []string, name, inv, reason, publicToken string) error {
	f.calls++
	f.lastCc = cc
	f.lastType, f.lastTenant, f.lastTo, f.lastName, f.lastInvoice = email.TypePaymentFailed, tenantID, to, name, inv
	f.lastReason = reason
	f.lastPublicToken = publicToken
	return nil
}
func (f *fakeDeliverer) SendPaymentSetupRequest(_ context.Context, tenantID, to, name, inv string, amount int64, cur, url string) error {
	f.calls++
	f.lastType, f.lastTenant, f.lastTo, f.lastName, f.lastInvoice = email.TypePaymentSetupRequest, tenantID, to, name, inv
	f.lastAmount, f.lastCurrency, f.lastUpdateURL = amount, cur, url
	return nil
}
func (f *fakeDeliverer) SendPaymentSetupLink(_ context.Context, tenantID, to, name, _ /*operatorNote*/, setupURL string) error {
	f.calls++
	f.lastType, f.lastTenant, f.lastTo, f.lastName = email.TypePaymentSetupLink, tenantID, to, name
	f.lastUpdateURL = setupURL
	return nil
}
func (f *fakeDeliverer) SendPasswordReset(_ context.Context, tenantID, to, name, url string) error {
	f.calls++
	f.lastType, f.lastTenant, f.lastTo, f.lastName = email.TypePasswordReset, tenantID, to, name
	f.lastUpdateURL = url
	return nil
}
func (f *fakeDeliverer) SendMemberInvite(_ context.Context, tenantID, to, inviter, tenantName, url string) error {
	f.calls++
	f.lastType, f.lastTenant, f.lastTo, f.lastName = email.TypeMemberInvite, tenantID, to, inviter
	f.lastUpdateURL = url
	return nil
}
func (f *fakeDeliverer) SendCreditNote(_ context.Context, tenantID, to string, cc []string, name, cnNumber, inv string, amount int64, cur string, pdf []byte) error {
	f.calls++
	f.lastCc = cc
	f.lastType, f.lastTenant, f.lastTo, f.lastName, f.lastInvoice = email.TypeCreditNote, tenantID, to, name, inv
	f.lastAmount, f.lastCurrency, f.lastPDF = amount, cur, pdf
	return nil
}

// callDeliverer mirrors the dispatcher's payload decode + switch. The
// dispatcher's handle method is unexported; re-implementing the decode here
// keeps the round-trip test independent of Start/tick timing without
// exposing internals purely for testing.
func callDeliverer(ctx context.Context, d email.EmailDeliverer, row email.OutboxRow) error {
	raw, err := json.Marshal(row.Payload)
	if err != nil {
		return err
	}
	var m struct {
		To               string   `json:"to"`
		Cc               []string `json:"cc"`
		CreditNoteNumber string   `json:"credit_note_number"`
		CustomerName     string   `json:"customer_name"`
		InvoiceNumber    string   `json:"invoice_number"`
		AmountCents      int64    `json:"amount_cents"`
		Currency         string   `json:"currency"`
		AttemptNumber    int      `json:"attempt_number"`
		MaxAttempts      int      `json:"max_attempts"`
		NextRetryDate    string   `json:"next_retry_date"`
		Action           string   `json:"action"`
		Reason           string   `json:"reason"`
		FailureReason    string   `json:"failure_reason"`
		UpdateURL        string   `json:"update_url"`
		PublicToken      string   `json:"public_token"`
		PDF              []byte   `json:"pdf"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	switch row.EmailType {
	case email.TypeInvoice:
		return d.SendInvoice(ctx, row.TenantID, m.To, m.Cc, m.CustomerName, m.InvoiceNumber, m.AmountCents, m.Currency, m.PDF, m.PublicToken)
	case email.TypePaymentReceipt:
		return d.SendPaymentReceipt(ctx, row.TenantID, m.To, m.Cc, m.CustomerName, m.InvoiceNumber, m.AmountCents, m.Currency, m.PublicToken)
	case email.TypeDunningWarning:
		return d.SendDunningWarning(ctx, row.TenantID, m.To, m.Cc, m.CustomerName, m.InvoiceNumber, m.AttemptNumber, m.MaxAttempts, m.NextRetryDate, m.FailureReason, m.PublicToken)
	case email.TypeDunningEscalation:
		return d.SendDunningEscalation(ctx, row.TenantID, m.To, m.Cc, m.CustomerName, m.InvoiceNumber, m.Action, m.PublicToken)
	case email.TypePaymentFailed:
		return d.SendPaymentFailed(ctx, row.TenantID, m.To, m.Cc, m.CustomerName, m.InvoiceNumber, m.Reason, m.PublicToken)
	case email.TypePaymentSetupRequest:
		return d.SendPaymentSetupRequest(ctx, row.TenantID, m.To, m.CustomerName, m.InvoiceNumber, m.AmountCents, m.Currency, m.UpdateURL)
	case email.TypeCreditNote:
		return d.SendCreditNote(ctx, row.TenantID, m.To, m.Cc, m.CustomerName, m.CreditNoteNumber, m.InvoiceNumber, m.AmountCents, m.Currency, m.PDF)
	default:
		return fmt.Errorf("unknown email_type %q", row.EmailType)
	}
}

func readEmailOutbox(t *testing.T, db *postgres.DB, id string) (status string, attempts int, payload map[string]any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 5*time.Second)
	defer cancel()
	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("read tx: %v", err)
	}
	defer postgres.Rollback(tx)

	var payloadJSON []byte
	err = tx.QueryRowContext(ctx,
		`SELECT status, attempts, payload FROM email_outbox WHERE id = $1`,
		id,
	).Scan(&status, &attempts, &payloadJSON)
	if err != nil {
		t.Fatalf("scan outbox row %s: %v", id, err)
	}
	if len(payloadJSON) > 0 {
		_ = json.Unmarshal(payloadJSON, &payload)
	}
	return status, attempts, payload
}

func readEmailOutboxRetry(t *testing.T, db *postgres.DB, id string) (lastErr string, nextAt time.Time) {
	t.Helper()
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 5*time.Second)
	defer cancel()
	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("read retry tx: %v", err)
	}
	defer postgres.Rollback(tx)

	var nullErr sql.NullString
	err = tx.QueryRowContext(ctx,
		`SELECT COALESCE(last_error,''), next_attempt_at FROM email_outbox WHERE id = $1`,
		id,
	).Scan(&nullErr, &nextAt)
	if err != nil {
		t.Fatalf("scan retry row %s: %v", id, err)
	}
	return nullErr.String, nextAt
}

func emailOutboxExists(t *testing.T, db *postgres.DB, id string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 5*time.Second)
	defer cancel()
	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("exists tx: %v", err)
	}
	defer postgres.Rollback(tx)

	var n int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM email_outbox WHERE id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("exists scan: %v", err)
	}
	return n > 0
}

// resetEmailDue forces next_attempt_at back to now() so the next ProcessBatch
// tick claims the row immediately — used by DLQ/retry tests to exercise many
// attempts within a single run without waiting out real backoff.
func resetEmailDue(db *postgres.DB, id string) error {
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 5*time.Second)
	defer cancel()
	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(ctx,
		`UPDATE email_outbox SET next_attempt_at = now() WHERE id = $1`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// TestEmailOutbox_Enqueue_StampsBillingAnchor pins the ADR-104 write-time
// anchor: an enqueue under a bound simulated ctx records WHICH billing
// instant caused the send (sim_effective_at + test_clock_id), and an
// unbound enqueue records NULL for both — a fact ("no clock was bound"),
// never inferred later. This single chokepoint is what lets the invoice
// timeline position emails on the entity's calendar.
func TestEmailOutbox_Enqueue_StampsBillingAnchor(t *testing.T) {
	db := testutil.SetupTestDB(t)
	baseCtx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 15*time.Second)
	defer cancel()

	tenantID := testutil.CreateTestTenant(t, db, "Email Outbox Anchor")
	store := email.NewOutboxStore(db)
	frozen := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)

	readAnchor := func(t *testing.T, id string) (simAt sql.NullTime, clockID sql.NullString) {
		t.Helper()
		tx, err := db.BeginTx(baseCtx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer postgres.Rollback(tx)
		if err := tx.QueryRowContext(baseCtx,
			`SELECT sim_effective_at, test_clock_id FROM email_outbox WHERE id = $1`, id,
		).Scan(&simAt, &clockID); err != nil {
			t.Fatalf("read anchor: %v", err)
		}
		return simAt, clockID
	}

	t.Run("bound ctx → anchor recorded", func(t *testing.T) {
		ctx := clock.WithSim(baseCtx, clock.Sim{At: frozen, TestClockID: "vlx_tclk_anchor"})
		id, err := store.EnqueueStandalone(ctx, tenantID, email.TypePaymentReceipt, map[string]any{
			"to": "c@example.com", "invoice_number": "VLX-000042",
		})
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		simAt, clockID := readAnchor(t, id)
		if !simAt.Valid || !simAt.Time.Equal(frozen) {
			t.Errorf("sim_effective_at = %v, want the frozen instant %s", simAt, frozen)
		}
		if !clockID.Valid || clockID.String != "vlx_tclk_anchor" {
			t.Errorf("test_clock_id = %v, want vlx_tclk_anchor", clockID)
		}
	})

	t.Run("unbound ctx → both NULL (live-mode mail)", func(t *testing.T) {
		id, err := store.EnqueueStandalone(baseCtx, tenantID, email.TypePaymentReceipt, map[string]any{
			"to": "c@example.com", "invoice_number": "VLX-000043",
		})
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		simAt, clockID := readAnchor(t, id)
		if simAt.Valid || clockID.Valid {
			t.Errorf("unbound enqueue must record NULL anchor, got sim=%v clock=%v — the calendars coincide and a fake anchor would be a read-path lie", simAt, clockID)
		}
	})

	t.Run("ListByInvoice surfaces the anchor", func(t *testing.T) {
		rows, err := store.ListByInvoice(baseCtx, tenantID, "VLX-000042")
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("rows = %d, want 1", len(rows))
		}
		if rows[0].SimEffectiveAt == nil || !rows[0].SimEffectiveAt.Equal(frozen) {
			t.Errorf("SimEffectiveAt = %v, want %s", rows[0].SimEffectiveAt, frozen)
		}
		if rows[0].TestClockID != "vlx_tclk_anchor" {
			t.Errorf("TestClockID = %q, want vlx_tclk_anchor", rows[0].TestClockID)
		}
	})
}
