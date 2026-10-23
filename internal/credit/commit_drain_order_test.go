package credit_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/credit"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/webhook"
)

// seedCustomer creates a customer for the credit tests.
func seedCustomer(t *testing.T, db *postgres.DB, ctx context.Context, tenantID, ext string) string {
	t.Helper()
	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: ext, DisplayName: ext,
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	return cust.ID
}

// seedPayableInvoice inserts a finalized non-commit invoice the drain can
// target (ApplyToInvoiceAtomic now locks + re-reads the invoice row, ADR-078).
func seedPayableInvoice(t *testing.T, db *postgres.DB, ctx context.Context, tenantID, customerID string, dueCents int64, number string) string {
	t.Helper()
	inv, err := invoice.NewPostgresStore(db).Create(ctx, tenantID, domain.Invoice{
		CustomerID:    customerID,
		InvoiceNumber: number,
		Status:        domain.InvoiceFinalized,
		PaymentStatus: domain.PaymentPending,
		Currency:      "USD",
		SubtotalCents: dueCents, TotalAmountCents: dueCents, AmountDueCents: dueCents,
		BillingPeriodStart: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		BillingPeriodEnd:   time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("seed invoice: %v", err)
	}
	return inv.ID
}

// grantKindOf reads a block's consumed_cents by (kind, source) for assertions.
func blockConsumed(t *testing.T, db *postgres.DB, ctx context.Context, tenantID, entryID string) int64 {
	t.Helper()
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	var consumed int64
	if err := tx.QueryRowContext(ctx,
		`SELECT consumed_cents FROM customer_credit_ledger WHERE id = $1`, entryID,
	).Scan(&consumed); err != nil {
		t.Fatalf("read consumed: %v", err)
	}
	return consumed
}

// TestDrainOrder_PromotionalFirst_NullSafe pins the ADR-078 D7 drain order
// against the panel-verified Postgres NULL trap: a bare
// `(grant_kind = 'promotional') DESC` sorts NULL-kind blocks FIRST (NULL >
// true under DESC NULLS FIRST), draining legacy money-derived credits before
// free promotional ones — the exact inversion zero-cost-basis-first exists to
// prevent. The shipped clause uses IS NOT DISTINCT FROM, so the order must be:
// promotional → paid class (legacy NULL + commit, by expires_at NULLS LAST,
// then created_at).
func TestDrainOrder_PromotionalFirst_NullSafe(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)

	store := credit.NewPostgresStore(db)
	svc := credit.NewService(store)
	tenantID := testutil.CreateTestTenant(t, db, "Drain Order NULL Safe")
	custID := seedCustomer(t, db, ctx, tenantID, "cus_drain_order")

	// Legacy NULL-kind grant FIRST (oldest) — the trap bait: under the buggy
	// clause it would drain before the promo.
	legacy, err := svc.Grant(ctx, tenantID, credit.GrantInput{
		Currency:   "USD",
		CustomerID: custID, AmountCents: 20000, Description: "legacy proration credit",
	})
	if err != nil {
		t.Fatalf("legacy grant: %v", err)
	}
	promo, err := svc.Grant(ctx, tenantID, credit.GrantInput{
		Currency:   "USD",
		CustomerID: custID, AmountCents: 10000, Description: "promo credits",
		GrantKind: domain.GrantKindPromotional,
	})
	if err != nil {
		t.Fatalf("promo grant: %v", err)
	}
	// Commit block via the real funding path shape (store-level, own tx).
	var commitEntry domain.CreditLedgerEntry
	fundingInv := seedPayableInvoice(t, db, ctx, tenantID, custID, 25000, "VLX-DRAIN-FUND")
	{
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		commitEntry, err = svc.GrantCommitForInvoiceTx(ctx, tx, tenantID, custID, fundingInv, "VLX-DRAIN-FUND", "USD", 30000, nil)
		if err != nil {
			t.Fatalf("commit grant: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}

	// Drain $250 into a normal invoice: promo ($100) must go first, then the
	// paid class in created_at order — legacy ($200) absorbs the remaining
	// $150; the commit block stays untouched.
	target := seedPayableInvoice(t, db, ctx, tenantID, custID, 25000, "VLX-DRAIN-TARGET")
	applied, err := svc.ApplyToInvoice(ctx, tenantID, custID, target, 25000, "VLX-DRAIN-TARGET")
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if applied != 25000 {
		t.Fatalf("applied = %d, want 25000", applied)
	}

	if got := blockConsumed(t, db, ctx, tenantID, promo.ID); got != 10000 {
		t.Errorf("promotional block consumed = %d, want 10000 (promo drains first)", got)
	}
	if got := blockConsumed(t, db, ctx, tenantID, legacy.ID); got != 15000 {
		t.Errorf("legacy NULL-kind block consumed = %d, want 15000 (paid class, after promo)", got)
	}
	if got := blockConsumed(t, db, ctx, tenantID, commitEntry.ID); got != 0 {
		t.Errorf("commit block consumed = %d, want 0 (younger paid-class block untouched)", got)
	}
}

// TestAdjustClawback_PromotionalFirst pins that the ORDER BY change applies to
// BOTH drainPositiveBlocks callers — clawback attribution (AdjustAtomic) too,
// per the panel's site-set finding.
func TestAdjustClawback_PromotionalFirst(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)

	store := credit.NewPostgresStore(db)
	svc := credit.NewService(store)
	tenantID := testutil.CreateTestTenant(t, db, "Clawback Promo First")
	custID := seedCustomer(t, db, ctx, tenantID, "cus_clawback_order")

	legacy, err := svc.Grant(ctx, tenantID, credit.GrantInput{
		Currency:   "USD",
		CustomerID: custID, AmountCents: 5000, Description: "legacy",
	})
	if err != nil {
		t.Fatalf("legacy grant: %v", err)
	}
	promo, err := svc.Grant(ctx, tenantID, credit.GrantInput{
		Currency:   "USD",
		CustomerID: custID, AmountCents: 5000, Description: "promo",
		GrantKind: domain.GrantKindPromotional,
	})
	if err != nil {
		t.Fatalf("promo grant: %v", err)
	}

	if _, err := svc.Adjust(ctx, tenantID, credit.AdjustInput{
		Currency:   "USD",
		CustomerID: custID, AmountCents: -3000, Description: "correction",
	}); err != nil {
		t.Fatalf("adjust: %v", err)
	}

	if got := blockConsumed(t, db, ctx, tenantID, promo.ID); got != 3000 {
		t.Errorf("promo consumed = %d, want 3000 (clawback drains promo first)", got)
	}
	if got := blockConsumed(t, db, ctx, tenantID, legacy.ID); got != 0 {
		t.Errorf("legacy consumed = %d, want 0", got)
	}
}

// TestApplyToInvoice_CommitInvoiceIsCashInstrument pins ADR-078 D4: the
// customer's balance — including the commit's own just-granted credits — can
// never pay the invoice that funds a grant ("credits buy credits": the
// panel-blocking auto-charge sweep scenario where a card-less customer's
// $10k commit invoice is settled by draining its own $10k grant, booking
// revenue on zero cash).
func TestApplyToInvoice_CommitInvoiceIsCashInstrument(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)

	store := credit.NewPostgresStore(db)
	svc := credit.NewService(store)
	tenantID := testutil.CreateTestTenant(t, db, "Commit Cash Instrument")
	custID := seedCustomer(t, db, ctx, tenantID, "cus_cash_instrument")

	// Pre-existing balance that must NOT drain into the commit invoice.
	if _, err := svc.Grant(ctx, tenantID, credit.GrantInput{
		Currency:   "USD",
		CustomerID: custID, AmountCents: 50000, Description: "existing balance",
	}); err != nil {
		t.Fatalf("grant: %v", err)
	}

	// A finalized invoice carrying a commit line.
	invID := seedPayableInvoice(t, db, ctx, tenantID, custID, 10000, "VLX-COMMIT-CASH")
	{
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO invoice_line_items (id, invoice_id, tenant_id, line_type, description,
				quantity, unit_amount_cents, amount_cents, total_amount_cents, currency, commit_granted_cents)
			VALUES ($1, $2, $3, 'add_on', 'Q3 commit', 1, 10000, 10000, 10000, 'USD', 12000)
		`, postgres.NewID("vlx_ili"), invID, tenantID); err != nil {
			t.Fatalf("seed commit line: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}

	applied, err := svc.ApplyToInvoice(ctx, tenantID, custID, invID, 10000, "VLX-COMMIT-CASH")
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if applied != 0 {
		t.Fatalf("applied = %d, want 0 — credits must never pay a commit funding invoice", applied)
	}
	bal, err := testutil.BalanceUSD(svc.GetBalance(ctx, tenantID, custID))
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if bal.BalanceCents != 50000 {
		t.Errorf("balance = %d, want 50000 (untouched)", bal.BalanceCents)
	}
}

// TestApplyToInvoice_SkipsSettledInvoice pins the stale-due race fix (ADR-078
// D6): a drain whose caller pre-read the invoice before a concurrent settle
// must no-op — pre-fix it silently burned credits into an already-paid
// invoice (usage entry + credits_applied bump against amount_due already 0).
func TestApplyToInvoice_SkipsSettledInvoice(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)

	store := credit.NewPostgresStore(db)
	svc := credit.NewService(store)
	tenantID := testutil.CreateTestTenant(t, db, "Apply Skips Settled")
	custID := seedCustomer(t, db, ctx, tenantID, "cus_apply_settled")

	if _, err := svc.Grant(ctx, tenantID, credit.GrantInput{
		Currency:   "USD",
		CustomerID: custID, AmountCents: 30000, Description: "balance",
	}); err != nil {
		t.Fatalf("grant: %v", err)
	}

	invID := seedPayableInvoice(t, db, ctx, tenantID, custID, 10000, "VLX-APPLY-SETTLED")
	if _, err := invoice.NewPostgresStore(db).MarkPaid(ctx, tenantID, invID, "pi_settled", time.Now()); err != nil {
		t.Fatalf("mark paid: %v", err)
	}

	// The caller's stale pre-read said $100 due; the in-tx re-read sees paid.
	applied, err := svc.ApplyToInvoice(ctx, tenantID, custID, invID, 10000, "VLX-APPLY-SETTLED")
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if applied != 0 {
		t.Fatalf("applied = %d, want 0 — invoice already settled", applied)
	}
	bal, err := testutil.BalanceUSD(svc.GetBalance(ctx, tenantID, custID))
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if bal.BalanceCents != 30000 {
		t.Errorf("balance = %d, want 30000 (no silent burn into a paid invoice)", bal.BalanceCents)
	}
}

// outboxEvents returns the event_types enqueued for the tenant, oldest first.
func outboxEvents(t *testing.T, db *postgres.DB, ctx context.Context, tenantID, eventType string) []map[string]any {
	t.Helper()
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	rows, err := tx.QueryContext(ctx,
		`SELECT payload FROM webhook_outbox WHERE tenant_id = $1 AND event_type = $2 ORDER BY next_attempt_at, id`,
		tenantID, eventType)
	if err != nil {
		t.Fatalf("query outbox: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []map[string]any
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scan payload: %v", err)
		}
		out = append(out, decodeJSONMap(t, raw))
	}
	return out
}

// TestBalanceCrossingEvents pins the ADR-078 D8 alert semantics end-to-end on
// the real transactional outbox: value crossings (low / depleted / recovered)
// enqueue in the same tx as the ledger write, the low threshold comes from
// tenant_settings, and payloads carry customer_id + balance_cents (+
// threshold_cents on low) so consumers can layer per-customer logic.
func TestBalanceCrossingEvents(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)

	store := credit.NewPostgresStore(db)
	store.SetOutboxEnqueuer(webhook.NewOutboxStore(db))
	svc := credit.NewService(store)
	tenantID := testutil.CreateTestTenant(t, db, "Balance Crossings")
	custID := seedCustomer(t, db, ctx, tenantID, "cus_crossings")

	// Arm the low threshold at $50.
	{
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO tenant_settings (tenant_id, credit_balance_low_threshold_cents)
			VALUES ($1, 5000)
			ON CONFLICT (tenant_id) DO UPDATE SET credit_balance_low_threshold_cents = 5000
		`, tenantID); err != nil {
			t.Fatalf("set threshold: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}

	// 0 → 10000: recovered fires (0 → >0 crossing).
	if _, err := svc.Grant(ctx, tenantID, credit.GrantInput{
		Currency:   "USD",
		CustomerID: custID, AmountCents: 10000, Description: "fund",
	}); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if got := len(outboxEvents(t, db, ctx, tenantID, domain.EventCreditBalanceRecovered)); got != 1 {
		t.Fatalf("recovered events after first grant = %d, want 1", got)
	}

	// 10000 → 3000 via drain: crosses below the 5000 threshold → low fires
	// with balance + threshold payload.
	target := seedPayableInvoice(t, db, ctx, tenantID, custID, 7000, "VLX-CROSS-1")
	if _, err := svc.ApplyToInvoice(ctx, tenantID, custID, target, 7000, "VLX-CROSS-1"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	lows := outboxEvents(t, db, ctx, tenantID, domain.EventCreditBalanceLow)
	if len(lows) != 1 {
		t.Fatalf("low events = %d, want 1", len(lows))
	}
	if err := domain.CheckWebhookData(domain.EventCreditBalanceLow, lows[0]); err != nil {
		t.Fatal(err)
	}
	lowBalance := lows[0][domain.WebhookResourceCreditBalance].(map[string]any)
	if lowBalance["balance_cents"] != float64(3000) || lows[0]["threshold_cents"] != float64(5000) {
		t.Errorf("low payload = %v, want balance_cents=3000 threshold_cents=5000", lows[0])
	}
	if lowBalance["customer_id"] != custID || lowBalance["currency"] != "USD" {
		t.Errorf("low payload balance = %v, want %s in USD", lowBalance, custID)
	}

	// 3000 → 0 via clawback: depleted fires; low does NOT re-fire (already
	// below threshold — crossing semantics, not level semantics).
	if _, err := svc.Adjust(ctx, tenantID, credit.AdjustInput{
		Currency:   "USD",
		CustomerID: custID, AmountCents: -3000, Description: "zero out",
	}); err != nil {
		t.Fatalf("adjust: %v", err)
	}
	if got := len(outboxEvents(t, db, ctx, tenantID, domain.EventCreditBalanceDepleted)); got != 1 {
		t.Fatalf("depleted events = %d, want 1", got)
	}
	if got := len(outboxEvents(t, db, ctx, tenantID, domain.EventCreditBalanceLow)); got != 1 {
		t.Errorf("low events after depletion = %d, want still 1 (no re-fire below threshold)", got)
	}

	// 0 → 2000: recovered fires again (complement of depleted — without it a
	// consumer's depleted state could never clear).
	if _, err := svc.Grant(ctx, tenantID, credit.GrantInput{
		Currency:   "USD",
		CustomerID: custID, AmountCents: 2000, Description: "top back up",
	}); err != nil {
		t.Fatalf("second grant: %v", err)
	}
	recovered := outboxEvents(t, db, ctx, tenantID, domain.EventCreditBalanceRecovered)
	if len(recovered) != 2 {
		t.Fatalf("recovered events = %d, want 2", len(recovered))
	}
	if b := recovered[1][domain.WebhookResourceCreditBalance].(map[string]any); b["balance_cents"] != float64(2000) {
		t.Errorf("recovered payload balance = %v, want 2000", b["balance_cents"])
	}
}

// decodeJSONMap unmarshals an outbox payload for assertions.
func decodeJSONMap(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	return m
}
