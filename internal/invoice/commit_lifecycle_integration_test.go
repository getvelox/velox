package invoice_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/credit"
	"github.com/sagarsuperuser/velox/internal/creditnote"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/tenant"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/webhook"
)

// commitHarness wires the REAL production seams (ADR-078): the invoice store
// with credit.Service injected as commit funder, the invoice service with
// credit.Service as credit reverser/retirer, and the tenant settings store as
// the invoice numberer — the same wiring the router does.
type commitHarness struct {
	db        *postgres.DB
	tenantID  string
	custID    string
	invStore  *invoice.PostgresStore
	invSvc    *invoice.Service
	creditSvc *credit.Service
}

func newCommitHarness(t *testing.T, name string) (*commitHarness, context.Context) {
	t.Helper()
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)

	tenantID := testutil.CreateTestTenant(t, db, name)
	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_" + strings.ReplaceAll(strings.ToLower(name), " ", "_"), DisplayName: name,
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	creditStore := credit.NewPostgresStore(db)
	creditStore.SetOutboxEnqueuer(webhook.NewOutboxStore(db))
	creditSvc := credit.NewService(creditStore)
	invStore := invoice.NewPostgresStore(db)
	invStore.SetCommitFunder(creditSvc)
	invSvc := invoice.NewService(invStore, nil, tenant.NewSettingsStore(db))
	invSvc.SetCreditReverser(creditSvc)

	return &commitHarness{
		db: db, tenantID: tenantID, custID: cust.ID,
		invStore: invStore, invSvc: invSvc, creditSvc: creditSvc,
	}, ctx
}

// armLowThreshold sets (or clears with 0) the tenant's balance_low threshold.
func (h *commitHarness) armLowThreshold(t *testing.T, ctx context.Context, cents int64) {
	t.Helper()
	tx, err := h.db.BeginTx(ctx, postgres.TxTenant, h.tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	var v any
	if cents > 0 {
		v = cents
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO tenant_settings (tenant_id, credit_balance_low_threshold_cents)
		VALUES ($1, $2)
		ON CONFLICT (tenant_id) DO UPDATE SET credit_balance_low_threshold_cents = $2
	`, h.tenantID, v); err != nil {
		t.Fatalf("arm threshold: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// newCustomer creates a fresh customer for per-round isolation.
func (h *commitHarness) newCustomer(t *testing.T, ctx context.Context, round int) string {
	t.Helper()
	cust, err := customer.NewPostgresStore(h.db).Create(ctx, h.tenantID, domain.Customer{
		ExternalID: fmt.Sprintf("cus_round_%d", round), DisplayName: fmt.Sprintf("Round %d", round),
	})
	if err != nil {
		t.Fatalf("round customer: %v", err)
	}
	return cust.ID
}

// seedPayable inserts a finalized non-commit invoice as a drain target.
func (h *commitHarness) seedPayable(t *testing.T, ctx context.Context, custID string, dueCents int64, round int) string {
	t.Helper()
	inv, err := invoice.NewPostgresStore(h.db).Create(ctx, h.tenantID, domain.Invoice{
		CustomerID: custID, InvoiceNumber: fmt.Sprintf("VLX-CONC-%d", round),
		Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending,
		Currency: "USD", SubtotalCents: dueCents, TotalAmountCents: dueCents, AmountDueCents: dueCents,
		BillingPeriodStart: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		BillingPeriodEnd:   time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("seed payable: %v", err)
	}
	return inv.ID
}

// countEvents counts outbox rows of one event type whose payload names the
// customer.
func (h *commitHarness) countEvents(t *testing.T, ctx context.Context, custID, eventType string) int {
	t.Helper()
	tx, err := h.db.BeginTx(ctx, postgres.TxTenant, h.tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	var n int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM webhook_outbox
		WHERE tenant_id = $1 AND event_type = $2 AND payload->'credit_balance'->>'customer_id' = $3
	`, h.tenantID, eventType, custID).Scan(&n); err != nil {
		t.Fatalf("count events: %v", err)
	}
	return n
}

// createCommitInvoice composes a manual invoice with one commit line through
// the real service path (Create → buildLineItem → CreateWithLineItems).
func (h *commitHarness) createCommitInvoice(t *testing.T, ctx context.Context, priceCents, grantCents int64, expiresAt *time.Time) domain.Invoice {
	t.Helper()
	inv, err := h.invSvc.Create(ctx, h.tenantID, invoice.CreateInput{
		CustomerID: h.custID,
		Currency:   "USD",
		LineItems: []invoice.AddLineItemInput{{
			Description:        "Prepaid commit — Q3",
			LineType:           "add_on",
			Quantity:           1,
			UnitAmountCents:    priceCents,
			CommitGrantedCents: &grantCents,
			CommitExpiresAt:    expiresAt,
		}},
	})
	if err != nil {
		t.Fatalf("create commit invoice: %v", err)
	}
	return inv
}

func (h *commitHarness) balance(t *testing.T, ctx context.Context) int64 {
	t.Helper()
	bal, err := testutil.BalanceUSD(h.creditSvc.GetBalance(ctx, h.tenantID, h.custID))
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	return bal.BalanceCents
}

// commitGrant returns the commit grant row funded by the invoice, or false.
func (h *commitHarness) commitGrant(t *testing.T, ctx context.Context, invoiceID string) (domain.CreditLedgerEntry, bool) {
	t.Helper()
	entries, err := h.creditSvc.ListEntries(ctx, credit.ListFilter{
		TenantID: h.tenantID, CustomerID: h.custID,
	})
	if err != nil {
		t.Fatalf("list entries: %v", err)
	}
	for _, e := range entries {
		if e.SourceInvoiceID == invoiceID && e.GrantKind == domain.GrantKindCommit {
			return e, true
		}
	}
	return domain.CreditLedgerEntry{}, false
}

// TestCommitFinalize_FundsGrantOnce pins ADR-078 D2: finalizing a manual
// invoice with a commit line grants the block IN the finalize tx —
// grant_kind='commit', source_invoice_id set, granted amount independent of
// the line price (discounted commit) — and the finalize CAS makes a second
// finalize a clean 409 with no second grant.
func TestCommitFinalize_FundsGrantOnce(t *testing.T) {
	h, ctx := newCommitHarness(t, "Commit Fund Once")

	// Discounted commit: pay $90, receive $100 of credits.
	inv := h.createCommitInvoice(t, ctx, 9000, 10000, nil)
	if h.balance(t, ctx) != 0 {
		t.Fatalf("balance before finalize = %d, want 0 (grant-on-ISSUE, not on create)", h.balance(t, ctx))
	}

	finalized, err := h.invSvc.Finalize(ctx, h.tenantID, inv.ID)
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if finalized.Status != domain.InvoiceFinalized {
		t.Fatalf("status = %s, want finalized", finalized.Status)
	}
	if got := h.balance(t, ctx); got != 10000 {
		t.Fatalf("balance after finalize = %d, want 10000 (the GRANTED amount, not the price)", got)
	}
	grant, ok := h.commitGrant(t, ctx, inv.ID)
	if !ok {
		t.Fatal("no commit grant found for the funding invoice")
	}
	if grant.ExpiresAt != nil {
		t.Errorf("grant expiry = %v, want nil (phase-1 default: never)", grant.ExpiresAt)
	}
	if grant.AmountCents != 10000 {
		t.Errorf("grant amount = %d, want 10000", grant.AmountCents)
	}

	// Second finalize: CAS rejects, no double grant.
	if _, err := h.invSvc.Finalize(ctx, h.tenantID, inv.ID); err == nil {
		t.Fatal("second finalize should fail (already finalized)")
	}
	if got := h.balance(t, ctx); got != 10000 {
		t.Errorf("balance after re-finalize attempt = %d, want 10000 (no double fund)", got)
	}
}

// TestCommitFinalize_FunderErrorKeepsDraft pins the both-or-neither contract:
// a funder error (commit expiry passed while the invoice sat in draft) fails
// Finalize loudly, the status flip rolls back, and no ledger row exists —
// never a finalized purchase that silently granted nothing.
func TestCommitFinalize_FunderErrorKeepsDraft(t *testing.T) {
	h, ctx := newCommitHarness(t, "Commit Funder Error")

	future := time.Now().Add(24 * time.Hour)
	inv := h.createCommitInvoice(t, ctx, 9000, 10000, &future)

	// The line was composed valid, then sat in draft past its expiry.
	{
		tx, err := h.db.BeginTx(ctx, postgres.TxTenant, h.tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE invoice_line_items SET commit_expires_at = now() - interval '1 hour'
			WHERE invoice_id = $1 AND commit_granted_cents IS NOT NULL
		`, inv.ID); err != nil {
			t.Fatalf("backdate expiry: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}

	_, err := h.invSvc.Finalize(ctx, h.tenantID, inv.ID)
	if err == nil {
		t.Fatal("finalize should fail: commit expiry already passed (dead-on-arrival grant)")
	}
	if !strings.Contains(err.Error(), "expire on arrival") {
		t.Errorf("error = %v, want the dead-on-arrival expiry validation", err)
	}

	got, err := h.invSvc.Get(ctx, h.tenantID, inv.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != domain.InvoiceDraft {
		t.Fatalf("status after failed finalize = %s, want draft (flip rolled back)", got.Status)
	}
	if bal := h.balance(t, ctx); bal != 0 {
		t.Errorf("balance = %d, want 0 (no grant landed)", bal)
	}
	if _, ok := h.commitGrant(t, ctx, inv.ID); ok {
		t.Error("commit grant exists despite failed finalize")
	}
}

// TestCommitVoid_RetiresRemaining pins ADR-078 D3: voiding an unpaid commit
// funding invoice retires the grant's REMAINING balance in the void tx;
// consumed stays consumed. And D3's exactly-once: the legal
// uncollectible→void sequence retires once, uncollectible alone retires
// nothing.
func TestCommitVoid_RetiresRemaining(t *testing.T) {
	h, ctx := newCommitHarness(t, "Commit Void Retire")

	inv := h.createCommitInvoice(t, ctx, 10000, 10000, nil)
	if _, err := h.invSvc.Finalize(ctx, h.tenantID, inv.ID); err != nil {
		t.Fatalf("finalize: %v", err)
	}

	// Customer draws $40 against a normal invoice before the commit is paid.
	target, err := invoice.NewPostgresStore(h.db).Create(ctx, h.tenantID, domain.Invoice{
		CustomerID: h.custID, InvoiceNumber: "VLX-VOIDRETIRE-DRAW",
		Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending,
		Currency: "USD", SubtotalCents: 4000, TotalAmountCents: 4000, AmountDueCents: 4000,
		BillingPeriodStart: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		BillingPeriodEnd:   time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("seed drawdown invoice: %v", err)
	}
	if _, err := h.creditSvc.ApplyToInvoice(ctx, h.tenantID, h.custID, target.ID, 4000, "VLX-VOIDRETIRE-DRAW"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := h.balance(t, ctx); got != 6000 {
		t.Fatalf("balance after draw = %d, want 6000", got)
	}

	// Void the funding invoice: the remaining $60 retires in the same tx.
	if _, err := h.invSvc.Void(ctx, h.tenantID, inv.ID); err != nil {
		t.Fatalf("void: %v", err)
	}
	if got := h.balance(t, ctx); got != 0 {
		t.Fatalf("balance after void = %d, want 0 (remaining retired; consumed stays consumed)", got)
	}
	grant, ok := h.commitGrant(t, ctx, inv.ID)
	if !ok {
		t.Fatal("grant row should still exist (append-only ledger)")
	}
	if grant.ConsumedCents != grant.AmountCents {
		t.Errorf("grant consumed = %d, want %d (fully retired)", grant.ConsumedCents, grant.AmountCents)
	}
}

// TestCommitUncollectible_NoRetire_ThenVoidRetiresOnce pins the D3 decision
// boundary: mark-uncollectible leaves the block LIVE (collections stance —
// uncollectible→paid recovery then needs no restore leg), and the follow-up
// void (a legal transition) retires exactly once.
func TestCommitUncollectible_NoRetire_ThenVoidRetiresOnce(t *testing.T) {
	h, ctx := newCommitHarness(t, "Commit Uncollectible Then Void")

	inv := h.createCommitInvoice(t, ctx, 10000, 10000, nil)
	if _, err := h.invSvc.Finalize(ctx, h.tenantID, inv.ID); err != nil {
		t.Fatalf("finalize: %v", err)
	}

	if _, err := h.invSvc.MarkUncollectible(ctx, h.tenantID, inv.ID); err != nil {
		t.Fatalf("mark uncollectible: %v", err)
	}
	if got := h.balance(t, ctx); got != 10000 {
		t.Fatalf("balance after uncollectible = %d, want 10000 (no retire — block stays live)", got)
	}

	// uncollectible → void: retire fires now, once.
	if _, err := h.invSvc.Void(ctx, h.tenantID, inv.ID); err != nil {
		t.Fatalf("void after uncollectible: %v", err)
	}
	if got := h.balance(t, ctx); got != 0 {
		t.Fatalf("balance after void = %d, want 0", got)
	}

	// Exactly one retirement entry.
	entries, err := h.creditSvc.ListEntries(ctx, credit.ListFilter{TenantID: h.tenantID, CustomerID: h.custID})
	if err != nil {
		t.Fatalf("list entries: %v", err)
	}
	retires := 0
	for _, e := range entries {
		if e.EntryType == domain.CreditAdjustment && e.AmountCents < 0 {
			retires++
		}
	}
	if retires != 1 {
		t.Errorf("retirement entries = %d, want exactly 1", retires)
	}
}

// TestUpdateStatus_CAS_RejectsPaidToVoid pins ADR-078 D5 at the STORE level:
// the void flip carries an in-SQL allowed-source predicate, so a pay-vs-void
// race (service guard read a stale pre-paid snapshot) cannot flip a PAID
// invoice to voided — which would retire credits the customer just paid for.
func TestUpdateStatus_CAS_RejectsPaidToVoid(t *testing.T) {
	h, ctx := newCommitHarness(t, "CAS Paid To Void")

	inv := h.createCommitInvoice(t, ctx, 10000, 10000, nil)
	if _, err := h.invSvc.Finalize(ctx, h.tenantID, inv.ID); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if _, err := h.invStore.MarkPaid(ctx, h.tenantID, inv.ID, "pi_cas_test", time.Now()); err != nil {
		t.Fatalf("mark paid: %v", err)
	}

	// Direct store-level flip, simulating a Void whose service guard passed
	// BEFORE the payment landed.
	_, err := h.invStore.UpdateStatusWithReversal(ctx, h.tenantID, inv.ID, domain.InvoiceVoided, nil)
	if err == nil {
		t.Fatal("paid→voided must be rejected by the in-tx CAS")
	}
	if !strings.Contains(err.Error(), "cannot transition") {
		t.Errorf("error = %v, want the CAS transition rejection", err)
	}
	if got := h.balance(t, ctx); got != 10000 {
		t.Errorf("balance = %d, want 10000 (paid commit's credits untouched)", got)
	}
}

// TestCreditNote_BlockedOnCommitInvoice pins ADR-078 D4: credit notes are
// rejected on invoices carrying a commit line — pre-pay AND post-pay — with a
// typed operator error (the CN paths have no grant-unwind leg in phase 1;
// silence would ship a doubling machine).
func TestCreditNote_BlockedOnCommitInvoice(t *testing.T) {
	h, ctx := newCommitHarness(t, "CN Blocked On Commit")

	inv := h.createCommitInvoice(t, ctx, 10000, 10000, nil)
	if _, err := h.invSvc.Finalize(ctx, h.tenantID, inv.ID); err != nil {
		t.Fatalf("finalize: %v", err)
	}

	cnSvc := creditnote.NewService(creditnote.NewPostgresStore(h.db), h.invStore, nil, nil)

	// Pre-payment (finalized).
	_, err := cnSvc.Create(ctx, h.tenantID, creditnote.CreateInput{
		InvoiceID: inv.ID, Reason: "duplicate",
		Lines: []creditnote.CreditLineInput{{Description: "concession", Quantity: 1, UnitAmountCents: 1000}},
	})
	if err == nil || !strings.Contains(err.Error(), "prepaid commit") {
		t.Fatalf("CN on finalized commit invoice: err = %v, want the commit block error", err)
	}

	// Post-payment (paid).
	if _, err := h.invStore.MarkPaid(ctx, h.tenantID, inv.ID, "pi_cn_block", time.Now()); err != nil {
		t.Fatalf("mark paid: %v", err)
	}
	_, err = cnSvc.Create(ctx, h.tenantID, creditnote.CreateInput{
		InvoiceID: inv.ID, Reason: "refund",
		Lines: []creditnote.CreditLineInput{{Description: "refund", Quantity: 1, UnitAmountCents: 10000}},
	})
	if err == nil || !strings.Contains(err.Error(), "prepaid commit") {
		t.Fatalf("CN on paid commit invoice: err = %v, want the commit block error", err)
	}
}
