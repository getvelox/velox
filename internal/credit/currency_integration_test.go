package credit_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/credit"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/webhook"
)

// Credit carries its own currency (ADR-129). Every claim here is proven on
// real Postgres: the in-memory store in service_test.go is not trusted for
// currency (T14).

// ccyRig is one tenant with the real credit store and service.
type ccyRig struct {
	t        *testing.T
	db       *postgres.DB
	ctx      context.Context
	tenantID string
	store    *credit.PostgresStore
	svc      *credit.Service
}

func newCcyRig(t *testing.T, name string, withOutbox bool) *ccyRig {
	t.Helper()
	db := testutil.SetupTestDB(t)
	store := credit.NewPostgresStore(db)
	if withOutbox {
		store.SetOutboxEnqueuer(webhook.NewOutboxStore(db))
	}
	return &ccyRig{
		t: t, db: db, ctx: postgres.WithLivemode(context.Background(), false),
		tenantID: testutil.CreateTestTenant(t, db, name),
		store:    store, svc: credit.NewService(store),
	}
}

func (r *ccyRig) customer(ext string) string { return seedCustomer(r.t, r.db, r.ctx, r.tenantID, ext) }

// block appends a positive ledger block straight through the store.
func (r *ccyRig) block(customerID, currency string, cents int64) domain.CreditLedgerEntry {
	r.t.Helper()
	e, err := r.store.AppendEntry(r.ctx, r.tenantID, domain.CreditLedgerEntry{
		CustomerID: customerID, EntryType: domain.CreditGrant, Currency: currency,
		AmountCents: cents, Description: "block " + currency,
	})
	if err != nil {
		r.t.Fatalf("append %s block: %v", currency, err)
	}
	return e
}

// invoiceIn is a finalized invoice in the currency, due cents.
func (r *ccyRig) invoiceIn(customerID, currency string, cents int64, number string) string {
	r.t.Helper()
	inv, err := invoice.NewPostgresStore(r.db).Create(r.ctx, r.tenantID, domain.Invoice{
		CustomerID: customerID, InvoiceNumber: number,
		Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending, Currency: currency,
		SubtotalCents: cents, TotalAmountCents: cents, AmountDueCents: cents,
		BillingPeriodStart: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		BillingPeriodEnd:   time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		r.t.Fatalf("seed %s invoice: %v", currency, err)
	}
	return inv.ID
}

func (r *ccyRig) exec(q string, args ...any) {
	r.t.Helper()
	tx, err := r.db.BeginTx(r.ctx, postgres.TxBypass, "")
	if err != nil {
		r.t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(r.ctx, q, args...); err != nil {
		r.t.Fatalf("exec %q: %v", q, err)
	}
	if err := tx.Commit(); err != nil {
		r.t.Fatalf("commit: %v", err)
	}
}

func (r *ccyRig) scanInt(q string, args ...any) int64 {
	r.t.Helper()
	tx, err := r.db.BeginTx(r.ctx, postgres.TxBypass, "")
	if err != nil {
		r.t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	var n int64
	if err := tx.QueryRowContext(r.ctx, q, args...).Scan(&n); err != nil {
		r.t.Fatalf("query %q: %v", q, err)
	}
	return n
}

// sumIn is the customer's ledger balance in one currency, read with SQL.
func (r *ccyRig) sumIn(customerID, currency string) int64 {
	r.t.Helper()
	return r.scanInt(`SELECT COALESCE(SUM(amount_cents), 0) FROM customer_credit_ledger WHERE customer_id = $1 AND currency = $2`, customerID, currency)
}

func (r *ccyRig) consumed(entryID string) int64 {
	r.t.Helper()
	return r.scanInt(`SELECT consumed_cents FROM customer_credit_ledger WHERE id = $1`, entryID)
}

// usageRows is the invoice's usage entries: currency and balance_after each.
func (r *ccyRig) usageRows(invoiceID string) []struct {
	currency    string
	amount, bal int64
} {
	r.t.Helper()
	tx, err := r.db.BeginTx(r.ctx, postgres.TxBypass, "")
	if err != nil {
		r.t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	rows, err := tx.QueryContext(r.ctx, `SELECT COALESCE(currency, ''), amount_cents, balance_after FROM customer_credit_ledger
		WHERE invoice_id = $1 AND entry_type = 'usage' ORDER BY created_at, id`, invoiceID)
	if err != nil {
		r.t.Fatalf("usage rows: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []struct {
		currency    string
		amount, bal int64
	}
	for rows.Next() {
		var u struct {
			currency    string
			amount, bal int64
		}
		if err := rows.Scan(&u.currency, &u.amount, &u.bal); err != nil {
			r.t.Fatalf("scan usage: %v", err)
		}
		out = append(out, u)
	}
	return out
}

func (r *ccyRig) rowCurrency(q string, args ...any) string {
	r.t.Helper()
	tx, err := r.db.BeginTx(r.ctx, postgres.TxBypass, "")
	if err != nil {
		r.t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	var c string
	if err := tx.QueryRowContext(r.ctx, q, args...).Scan(&c); err != nil {
		r.t.Fatalf("query %q: %v", q, err)
	}
	return c
}

func (r *ccyRig) invoiceMoney(invoiceID string) (due, applied int64) {
	r.t.Helper()
	inv, err := invoice.NewPostgresStore(r.db).Get(r.ctx, r.tenantID, invoiceID)
	if err != nil {
		r.t.Fatalf("get invoice: %v", err)
	}
	return inv.AmountDueCents, inv.CreditsAppliedCents
}

// T1: with a USD block and a EUR block, a EUR invoice draws only the EUR
// block, the usage row is EUR, and its balance_after is the EUR balance.
//
// Mutation-verify (ADR-129 PR): (m1) drop `AND currency` from
// drainPositiveBlocks → the USD block is consumed, red; (m2) drop it from the
// apply's balance SUM → balance_after reads 5000, red; (m3) read
// i.currency without UPPER → the lowercase subtest drains 0, red.
func TestApply_DrainsOnlyTheInvoiceCurrency(t *testing.T) {
	for _, c := range []struct{ name, stored string }{{"uppercase invoice", "EUR"}, {"lowercase invoice", "eur"}} {
		t.Run(c.name, func(t *testing.T) {
			r := newCcyRig(t, "Drain Currency "+c.stored, false)
			cust := r.customer("cus_drain_ccy")
			usd := r.block(cust, "USD", 5000)
			eur := r.block(cust, "EUR", 1000)
			inv := r.invoiceIn(cust, "EUR", 3000, "VLX-DRAIN-EUR")
			// invoices.currency has no CHECK; only the store's insert
			// uppercases. The apply must not rely on that.
			r.exec(`UPDATE invoices SET currency = $1 WHERE id = $2`, c.stored, inv)

			applied, err := r.svc.ApplyToInvoice(r.ctx, r.tenantID, cust, inv, 3000, "VLX-DRAIN-EUR")
			if err != nil {
				t.Fatalf("apply: %v", err)
			}
			if applied != 1000 {
				t.Errorf("applied = %d, want 1000 (the EUR block only)", applied)
			}
			if due, credited := r.invoiceMoney(inv); due != 2000 || credited != 1000 {
				t.Errorf("invoice due %d, credits_applied %d; want 2000 and 1000", due, credited)
			}
			if got := r.consumed(usd.ID); got != 0 {
				t.Errorf("USD block consumed = %d, want 0", got)
			}
			if got := r.consumed(eur.ID); got != 1000 {
				t.Errorf("EUR block consumed = %d, want 1000", got)
			}
			use := r.usageRows(inv)
			if len(use) != 1 || use[0].currency != "EUR" || use[0].amount != -1000 || use[0].bal != 0 {
				t.Errorf("usage rows = %+v, want one EUR row of -1000 with balance_after 0", use)
			}
			if got := r.sumIn(cust, "USD"); got != 5000 {
				t.Errorf("USD balance = %d, want 5000", got)
			}
		})
	}
}

// T2: a customer with credit only in USD and a EUR invoice: nothing is
// written anywhere. Control: a USD invoice drains.
func TestApply_NoCreditInTheInvoiceCurrency(t *testing.T) {
	r := newCcyRig(t, "Drain No Currency", true)
	cust := r.customer("cus_no_ccy")
	r.block(cust, "USD", 5000)
	inv := r.invoiceIn(cust, "EUR", 3000, "VLX-NO-EUR")
	r.exec(`INSERT INTO checkout_sessions (id, tenant_id, invoice_id, livemode, amount_cents, currency, status)
		VALUES ('vlx_cs_no_ccy', $1, $2, false, 3000, 'EUR', 'open')`, r.tenantID, inv)
	outboxBefore := r.scanInt(`SELECT count(*) FROM webhook_outbox WHERE tenant_id = $1`, r.tenantID)

	applied, err := r.svc.ApplyToInvoice(r.ctx, r.tenantID, cust, inv, 3000, "VLX-NO-EUR")
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if applied != 0 {
		t.Errorf("applied = %d, want 0", applied)
	}
	if rows := r.usageRows(inv); len(rows) != 0 {
		t.Errorf("usage rows = %+v, want none", rows)
	}
	if due, credited := r.invoiceMoney(inv); due != 3000 || credited != 0 {
		t.Errorf("invoice due %d, credits_applied %d; want 3000 and 0", due, credited)
	}
	if st := r.rowCurrency(`SELECT status FROM checkout_sessions WHERE id = 'vlx_cs_no_ccy'`); st != "open" {
		t.Errorf("checkout session status %q, want open (nothing changed the amount due)", st)
	}
	if n := r.scanInt(`SELECT count(*) FROM webhook_outbox WHERE tenant_id = $1`, r.tenantID); n != outboxBefore {
		t.Errorf("outbox rows %d, want %d (no balance moved, no event)", n, outboxBefore)
	}
	if got := r.sumIn(cust, "USD"); got != 5000 {
		t.Errorf("USD balance = %d, want 5000", got)
	}

	control := r.invoiceIn(cust, "USD", 3000, "VLX-NO-USD")
	if applied, err := r.svc.ApplyToInvoice(r.ctx, r.tenantID, cust, control, 3000, "VLX-NO-USD"); err != nil || applied != 3000 {
		t.Errorf("control USD apply = %d, %v; want 3000", applied, err)
	}
}

// T3: collisions. Two invoices of one customer in different currencies drain
// concurrently, each from its own currency only; a grant racing an apply in
// one currency leaves a consistent balance_after chain; a second serial
// apply drains nothing.
func TestApply_CurrencyCollisions(t *testing.T) {
	t.Run("concurrent USD and EUR invoices", func(t *testing.T) {
		r := newCcyRig(t, "Collide Two Currencies", false)
		cust := r.customer("cus_collide_two")
		usd := r.block(cust, "USD", 5000)
		eur := r.block(cust, "EUR", 4000)
		invUSD := r.invoiceIn(cust, "USD", 3000, "VLX-COL-USD")
		invEUR := r.invoiceIn(cust, "EUR", 3000, "VLX-COL-EUR")

		var wg sync.WaitGroup
		start := make(chan struct{})
		got := make([]int64, 2)
		errsOut := make([]error, 2)
		for i, inv := range []string{invUSD, invEUR} {
			wg.Add(1)
			go func(i int, inv string) {
				defer wg.Done()
				<-start
				got[i], errsOut[i] = r.svc.ApplyToInvoice(r.ctx, r.tenantID, cust, inv, 3000)
			}(i, inv)
		}
		close(start)
		wg.Wait()
		for i, err := range errsOut {
			if err != nil || got[i] != 3000 {
				t.Errorf("apply %d = %d, %v; want 3000", i, got[i], err)
			}
		}
		if r.sumIn(cust, "USD") != 2000 || r.sumIn(cust, "EUR") != 1000 {
			t.Errorf("balances USD %d EUR %d, want 2000 and 1000", r.sumIn(cust, "USD"), r.sumIn(cust, "EUR"))
		}
		if r.consumed(usd.ID) != 3000 || r.consumed(eur.ID) != 3000 {
			t.Errorf("consumed USD %d EUR %d, want 3000 each", r.consumed(usd.ID), r.consumed(eur.ID))
		}
		for inv, cur := range map[string]string{invUSD: "USD", invEUR: "EUR"} {
			if rows := r.usageRows(inv); len(rows) != 1 || rows[0].currency != cur {
				t.Errorf("invoice %s usage rows %+v, want one %s row", inv, rows, cur)
			}
		}
	})

	t.Run("concurrent EUR grant and EUR apply", func(t *testing.T) {
		r := newCcyRig(t, "Collide Grant Apply", false)
		cust := r.customer("cus_collide_grant")
		r.block(cust, "USD", 9000)
		r.block(cust, "EUR", 2000)
		inv := r.invoiceIn(cust, "EUR", 1500, "VLX-COL-GA")

		var wg sync.WaitGroup
		start := make(chan struct{})
		var grantErr, applyErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, grantErr = r.store.AppendEntry(r.ctx, r.tenantID, domain.CreditLedgerEntry{
				CustomerID: cust, EntryType: domain.CreditGrant, Currency: "EUR", AmountCents: 1000, Description: "racing grant",
			})
		}()
		go func() {
			defer wg.Done()
			<-start
			_, applyErr = r.svc.ApplyToInvoice(r.ctx, r.tenantID, cust, inv, 1500)
		}()
		close(start)
		wg.Wait()
		if grantErr != nil || applyErr != nil {
			t.Fatalf("grant err %v, apply err %v", grantErr, applyErr)
		}
		grantBal := r.scanInt(`SELECT balance_after FROM customer_credit_ledger WHERE customer_id = $1 AND description = 'racing grant'`, cust)
		use := r.usageRows(inv)
		if len(use) != 1 {
			t.Fatalf("usage rows %+v, want 1", use)
		}
		// Either order, each balance_after is the EUR balance after it.
		grantFirst := grantBal == 3000 && use[0].bal == 1500
		applyFirst := use[0].bal == 500 && grantBal == 1500
		if !grantFirst && !applyFirst {
			t.Errorf("grant balance_after %d, usage balance_after %d: not a serial EUR chain (want 3000/1500 or 1500/500)", grantBal, use[0].bal)
		}
		if got := r.sumIn(cust, "EUR"); got != 1500 {
			t.Errorf("EUR balance = %d, want 1500", got)
		}
	})

	t.Run("serial double apply", func(t *testing.T) {
		r := newCcyRig(t, "Collide Double Apply", false)
		cust := r.customer("cus_collide_double")
		r.block(cust, "EUR", 5000)
		inv := r.invoiceIn(cust, "EUR", 1500, "VLX-COL-DBL")
		first, err := r.svc.ApplyToInvoice(r.ctx, r.tenantID, cust, inv, 1500)
		if err != nil || first != 1500 {
			t.Fatalf("first apply = %d, %v; want 1500", first, err)
		}
		second, err := r.svc.ApplyToInvoice(r.ctx, r.tenantID, cust, inv, 1500)
		if err != nil || second != 0 {
			t.Errorf("second apply = %d, %v; want 0", second, err)
		}
		if rows := r.usageRows(inv); len(rows) != 1 {
			t.Errorf("usage rows %+v, want exactly 1", rows)
		}
		if n := r.scanInt(`SELECT count(*) FROM customer_credit_ledger WHERE customer_id = $1`, cust); n != 2 {
			t.Errorf("ledger rows = %d, want 2 (the block and one usage row)", n)
		}
		if got := r.sumIn(cust, "EUR"); got != 3500 {
			t.Errorf("EUR balance = %d, want 3500", got)
		}
	})
}

func eventsWithCurrency(r *ccyRig, eventType, currency string) int64 {
	r.t.Helper()
	return r.scanInt(`SELECT count(*) FROM webhook_outbox WHERE tenant_id = $1 AND event_type = $2 AND payload->'credit_balance'->>'currency' = $3`,
		r.tenantID, eventType, currency)
}

// T4: balance crossings are per currency, and each payload names it.
//
// Mutation-verify: compute the crossings on the customer's total across
// currencies → the EUR depleted event disappears, red.
func TestBalanceCrossings_PerCurrency(t *testing.T) {
	t.Run("depleted in one currency while another holds credit", func(t *testing.T) {
		r := newCcyRig(t, "Crossing Depleted", true)
		cust := r.customer("cus_cross_depleted")
		r.block(cust, "USD", 1000)
		r.block(cust, "EUR", 500)
		inv := r.invoiceIn(cust, "EUR", 500, "VLX-CROSS-EUR")
		if _, err := r.svc.ApplyToInvoice(r.ctx, r.tenantID, cust, inv, 500); err != nil {
			t.Fatalf("apply: %v", err)
		}
		if n := eventsWithCurrency(r, domain.EventCreditBalanceDepleted, "EUR"); n != 1 {
			t.Errorf("EUR depleted events = %d, want 1", n)
		}
		if n := eventsWithCurrency(r, domain.EventCreditBalanceDepleted, "USD"); n != 0 {
			t.Errorf("USD depleted events = %d, want 0", n)
		}
		// Recovered is per currency too: a EUR grant on an empty EUR balance
		// recovers EUR although the USD balance never ran out.
		r.block(cust, "EUR", 200)
		if n := eventsWithCurrency(r, domain.EventCreditBalanceRecovered, "EUR"); n != 2 {
			t.Errorf("EUR recovered events = %d, want 2 (first block, then the top-up)", n)
		}
	})

	t.Run("the low threshold applies in each currency", func(t *testing.T) {
		r := newCcyRig(t, "Crossing Low", true)
		r.exec(`INSERT INTO tenant_settings (tenant_id, credit_balance_low_threshold_cents) VALUES ($1, 800)
			ON CONFLICT (tenant_id) DO UPDATE SET credit_balance_low_threshold_cents = 800`, r.tenantID)
		cust := r.customer("cus_cross_low")
		r.block(cust, "USD", 1000)
		r.block(cust, "EUR", 900)
		inv := r.invoiceIn(cust, "EUR", 200, "VLX-LOW-EUR")
		if _, err := r.svc.ApplyToInvoice(r.ctx, r.tenantID, cust, inv, 200); err != nil {
			t.Fatalf("apply: %v", err)
		}
		if n := eventsWithCurrency(r, domain.EventCreditBalanceLow, "EUR"); n != 1 {
			t.Errorf("EUR low events = %d, want 1 (900 → 700 crosses 800)", n)
		}
		if n := r.scanInt(`SELECT count(*) FROM webhook_outbox WHERE tenant_id = $1 AND event_type = $2 AND (payload->'credit_balance'->>'balance_cents')::bigint = 700`,
			r.tenantID, domain.EventCreditBalanceLow); n != 1 {
			t.Errorf("low events with balance_cents 700 = %d, want 1 (the EUR balance, not the total)", n)
		}
	})
}

// T5 (ledger part): rows a writer derives from another row take that row's
// currency.
func TestCurrency_DerivedRowsInherit(t *testing.T) {
	t.Run("expiry entry takes the grant's currency", func(t *testing.T) {
		r := newCcyRig(t, "Inherit Expiry", false)
		cust := r.customer("cus_inherit_expiry")
		past := time.Now().UTC().Add(-time.Hour)
		g, err := r.store.AppendEntry(r.ctx, r.tenantID, domain.CreditLedgerEntry{
			CustomerID: cust, EntryType: domain.CreditGrant, Currency: "EUR", AmountCents: 700,
			Description: "expiring", ExpiresAt: &past,
		})
		if err != nil {
			t.Fatalf("grant: %v", err)
		}
		if n, err := r.store.ExpireGrantAtomic(r.ctx, r.tenantID, cust, g.ID); err != nil || n != 700 {
			t.Fatalf("expire = %d, %v; want 700", n, err)
		}
		if c := r.rowCurrency(`SELECT COALESCE(currency, '') FROM customer_credit_ledger WHERE entry_type = 'expiry' AND customer_id = $1`, cust); c != "EUR" {
			t.Errorf("expiry entry currency %q, want EUR", c)
		}
		if got := r.sumIn(cust, "EUR"); got != 0 {
			t.Errorf("EUR balance after expiry = %d, want 0", got)
		}
	})

	t.Run("void reversal returns the drawn currency", func(t *testing.T) {
		r := newCcyRig(t, "Inherit Reversal", false)
		cust := r.customer("cus_inherit_reversal")
		r.block(cust, "USD", 4000)
		r.block(cust, "EUR", 2000)
		inv := r.invoiceIn(cust, "EUR", 1500, "VLX-REV-EUR")
		if _, err := r.svc.ApplyToInvoice(r.ctx, r.tenantID, cust, inv, 1500); err != nil {
			t.Fatalf("apply: %v", err)
		}
		r.reverse(cust, inv, false)
		if c := r.rowCurrency(`SELECT COALESCE(currency, '') FROM customer_credit_ledger WHERE source_invoice_reversal_id = $1`, inv); c != "EUR" {
			t.Errorf("reversal currency %q, want EUR", c)
		}
		if r.sumIn(cust, "EUR") != 2000 || r.sumIn(cust, "USD") != 4000 {
			t.Errorf("balances USD %d EUR %d, want 4000 and 2000", r.sumIn(cust, "USD"), r.sumIn(cust, "EUR"))
		}
	})

	t.Run("usage in two currencies on one invoice is refused loudly", func(t *testing.T) {
		r := newCcyRig(t, "Inherit Reversal Mixed", false)
		cust := r.customer("cus_inherit_mixed")
		r.block(cust, "EUR", 2000)
		inv := r.invoiceIn(cust, "EUR", 1500, "VLX-REV-MIX")
		if _, err := r.svc.ApplyToInvoice(r.ctx, r.tenantID, cust, inv, 1500); err != nil {
			t.Fatalf("apply: %v", err)
		}
		// A tenant tx, so the row lands in this test's mode (the livemode
		// trigger reads the session).
		tx, err := r.db.BeginTx(r.ctx, postgres.TxTenant, r.tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := tx.ExecContext(r.ctx, `INSERT INTO customer_credit_ledger (id, tenant_id, customer_id, entry_type, currency, amount_cents, balance_after, description, invoice_id)
			VALUES ('vlx_ccl_mixed_usd', $1, $2, 'usage', 'USD', -100, -100, 'drift', $3)`, r.tenantID, cust, inv); err != nil {
			t.Fatalf("seed USD usage row: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
		r.reverse(cust, inv, true)
		if n := r.scanInt(`SELECT count(*) FROM customer_credit_ledger WHERE source_invoice_reversal_id = $1`, inv); n != 0 {
			t.Errorf("reversal rows = %d, want 0", n)
		}
	})
}

// reverse runs the void reversal on its own transaction.
func (r *ccyRig) reverse(customerID, invoiceID string, wantErr bool) {
	r.t.Helper()
	tx, err := r.db.BeginTx(r.ctx, postgres.TxTenant, r.tenantID)
	if err != nil {
		r.t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	_, err = r.svc.ReverseForInvoiceTx(r.ctx, tx, r.tenantID, customerID, invoiceID, "")
	if wantErr {
		if err == nil {
			r.t.Error("reversal over usage rows in two currencies: no error, want a loud one")
		}
		return
	}
	if err != nil {
		r.t.Fatalf("reverse: %v", err)
	}
	if err := tx.Commit(); err != nil {
		r.t.Fatalf("commit: %v", err)
	}
}

// T6: every writer refuses a row with no currency, and a second entry for the
// same source in another currency is a mismatch, not a replay.
func TestCurrency_EveryWriterRequiresOne(t *testing.T) {
	r := newCcyRig(t, "Writers Require Currency", false)
	cust := r.customer("cus_writers_ccy")

	if _, err := r.store.AppendEntry(r.ctx, r.tenantID, domain.CreditLedgerEntry{
		CustomerID: cust, EntryType: domain.CreditGrant, AmountCents: 100, Description: "no currency",
	}); err == nil {
		t.Error("store append with no currency: accepted, want an error")
	} else if errs.Code(err) != "" {
		t.Errorf("store append with no currency: a %q API error, want a plain error (a code bug, not bad input)", errs.Code(err))
	}

	if _, err := r.svc.Grant(r.ctx, r.tenantID, credit.GrantInput{CustomerID: cust, AmountCents: 100, Description: "x"}); errs.Field(err) != "currency" {
		t.Errorf("grant with no currency: err %v, want a currency field error", err)
	}
	if _, err := r.svc.Adjust(r.ctx, r.tenantID, credit.AdjustInput{CustomerID: cust, AmountCents: 100, Description: "x"}); errs.Field(err) != "currency" {
		t.Errorf("adjust with no currency: err %v, want a currency field error", err)
	}

	tx, err := r.db.BeginTx(r.ctx, postgres.TxTenant, r.tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	_, err = r.svc.GrantForCreditNoteTx(r.ctx, tx, r.tenantID, "vlx_cn_writers", credit.GrantInput{
		CustomerID: cust, AmountCents: 100, Description: "credit note",
	})
	postgres.Rollback(tx)
	if err == nil {
		t.Error("credit-note grant with no currency: accepted, want an error")
	}

	inv := r.invoiceIn(cust, "EUR", 1000, "VLX-WRITERS-COMMIT")
	tx, err = r.db.BeginTx(r.ctx, postgres.TxTenant, r.tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	_, err = r.svc.GrantCommitForInvoiceTx(r.ctx, tx, r.tenantID, cust, inv, "VLX-WRITERS-COMMIT", "", 1000, nil)
	postgres.Rollback(tx)
	if err == nil {
		t.Error("commit grant with no currency: accepted, want an error")
	}
	if _, err := r.store.AdjustAtomic(r.ctx, r.tenantID, cust, "", "no currency", 100); err == nil {
		t.Error("store adjustment with no currency: accepted, want an error")
	}

	if n := r.scanInt(`SELECT count(*) FROM customer_credit_ledger WHERE customer_id = $1`, cust); n != 0 {
		t.Errorf("ledger rows = %d, want 0", n)
	}

	cn := func(currency string) error {
		_, err := r.store.AppendEntry(r.ctx, r.tenantID, domain.CreditLedgerEntry{
			CustomerID: cust, EntryType: domain.CreditGrant, Currency: currency, AmountCents: 5000,
			Description: "credit note CN-CCY", SourceCreditNoteID: "vlx_cn_same_ccy",
		})
		return err
	}
	if err := cn("EUR"); err != nil {
		t.Fatalf("first credit-note entry: %v", err)
	}
	err = cn("USD")
	if errs.Code(err) != "credit_source_amount_mismatch" {
		t.Errorf("same source, same amount, other currency: err %v (code %q), want credit_source_amount_mismatch", err, errs.Code(err))
	}
	if errors.Is(err, errs.ErrAlreadyExists) {
		t.Error("a currency mismatch must not read as an already-exists replay")
	}
}

// The ledger read (GET /v1/credits/ledger/{id}) computes each row's
// balance_after as the running balance in that row's own currency, in time
// order. Mixed ledger USD 5000, EUR 1000, USD 200: the EUR row reads 1000 and
// the second USD row 5200, not 6000 and 6200. The stored balance_after column
// is overwritten with -1 first, so only the read's own window can pass.
//
// Mutation-verify (review round 1): drop `currency` from the window's
// PARTITION BY in ListEntries → the EUR row reads 6000, red.
func TestListEntries_BalanceAfterIsPerCurrency(t *testing.T) {
	r := newCcyRig(t, "Ledger Read Currency", false)
	cust := r.customer("cus_ledger_ccy")
	usd1 := r.block(cust, "USD", 5000)
	eur := r.block(cust, "EUR", 1000)
	usd2 := r.block(cust, "USD", 200)
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	for i, id := range []string{usd1.ID, eur.ID, usd2.ID} {
		r.exec(`UPDATE customer_credit_ledger SET created_at = $1, balance_after = -1 WHERE id = $2`,
			base.Add(time.Duration(i)*time.Minute), id)
	}

	entries, err := r.store.ListEntries(r.ctx, credit.ListFilter{TenantID: r.tenantID, CustomerID: cust, Limit: 100})
	if err != nil {
		t.Fatalf("list entries: %v", err)
	}
	got := map[string]domain.CreditLedgerEntry{}
	for _, e := range entries {
		got[e.ID] = e
	}
	for _, w := range []struct {
		id, currency string
		balance      int64
	}{{usd1.ID, "USD", 5000}, {eur.ID, "EUR", 1000}, {usd2.ID, "USD", 5200}} {
		e, ok := got[w.id]
		if !ok {
			t.Errorf("entry %s missing from the read", w.id)
			continue
		}
		if e.Currency != w.currency || e.BalanceAfter != w.balance {
			t.Errorf("entry %s: %s balance_after %d, want %s %d", w.id, e.Currency, e.BalanceAfter, w.currency, w.balance)
		}
	}
}
