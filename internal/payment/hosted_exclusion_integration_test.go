package payment

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// hostedRow is one checkout_sessions row as a test seeds it. Times are
// offsets from the DB's now(), so the window arithmetic runs on the same
// clock the predicate reads.
type hostedRow struct {
	status   string
	recorded bool          // a Stripe session id was recorded
	created  time.Duration // created_at = now() + created
	expires  *time.Duration
}

func ago(d time.Duration) *time.Duration   { v := -d; return &v }
func ahead(d time.Duration) *time.Duration { return &d }

func seedHostedRow(t *testing.T, db *postgres.DB, ctx context.Context, tenantID, invoiceID string, r hostedRow) string {
	t.Helper()
	id := postgres.NewID("vlx_cks")
	var sessID sql.NullString
	if r.recorded {
		sessID = sql.NullString{String: "cs_" + id, Valid: true}
	}
	var expSecs sql.NullFloat64
	if r.expires != nil {
		expSecs = sql.NullFloat64{Float64: r.expires.Seconds(), Valid: true}
	}
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO checkout_sessions (id, tenant_id, invoice_id, livemode, stripe_session_id, amount_cents, currency, status,
			created_at, updated_at, expires_at)
		VALUES ($1, $2, $3, false, $4, 5000, 'USD', $5,
			now() + make_interval(secs => $6), now() + make_interval(secs => $6),
			CASE WHEN $7::float8 IS NULL THEN NULL ELSE now() + make_interval(secs => $7) END)
	`, id, tenantID, invoiceID, sessID, r.status, r.created.Seconds(), expSecs); err != nil {
		t.Fatalf("seed hosted row: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return id
}

func execTenant(t *testing.T, db *postgres.DB, ctx context.Context, tenantID, q string, args ...any) {
	t.Helper()
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(ctx, q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// TestChargeClaims_SkipInvoiceWithLiveHostedPage pins "one payer at a time"
// on every Velox charge initiator: while the customer may be paying on the
// hosted page, or has paid there and the payment is not recorded yet, no
// charge claim succeeds. The rows that must NOT block are the negative
// controls: a lapsed unfilled claim, a session the sweep already resolved, a
// row closed by an invoice exit, and a recorded session past the 24h cap.
func TestChargeClaims_SkipInvoiceWithLiveHostedPage(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Hosted Exclusion")
	store := invoice.NewPostgresStore(db)

	cases := []struct {
		name  string
		rows  []hostedRow
		block bool
	}{
		{"no hosted page", nil, false},
		{"unfilled claim just created", []hostedRow{{status: "open", created: 0}}, true},
		{"unfilled claim 64m old", []hostedRow{{status: "open", created: -64 * time.Minute}}, true},
		{"unfilled claim 66m old", []hostedRow{{status: "open", created: -66 * time.Minute}}, false},
		{"open session, live", []hostedRow{{status: "open", recorded: true, created: -10 * time.Minute, expires: ahead(50 * time.Minute)}}, true},
		{"open session expired, sweep has not looked", []hostedRow{{status: "open", recorded: true, created: -3 * time.Hour, expires: ago(2 * time.Hour)}}, true},
		{"open session past the 24h cap", []hostedRow{{status: "open", recorded: true, created: -26 * time.Hour, expires: ago(25 * time.Hour)}}, false},
		{"superseded session still live at Stripe", []hostedRow{{status: "superseded", recorded: true, created: -10 * time.Minute, expires: ahead(50 * time.Minute)}}, true},
		{"superseded unfilled claim", []hostedRow{{status: "superseded", created: -10 * time.Minute}}, false},
		{"completed, payment not recorded", []hostedRow{{status: "completed", recorded: true, created: -3 * time.Hour, expires: ago(2 * time.Hour)}}, true},
		{"expired (sweep resolved)", []hostedRow{{status: "expired", recorded: true, created: -10 * time.Minute, expires: ahead(50 * time.Minute)}}, false},
		{"invoice_settled", []hostedRow{{status: "invoice_settled", recorded: true, created: -10 * time.Minute, expires: ahead(50 * time.Minute)}}, false},
	}

	claims := []struct {
		name  string
		claim func(ctx context.Context, tenantID, id string) (bool, error)
	}{
		{"auto-charge", store.ClaimAutoCharge},
		{"dunning retry", store.ClaimChargeForDunningRetry},
		{"manual collect", store.ClaimChargeForManualCollect},
	}

	for _, c := range cases {
		for _, cl := range claims {
			t.Run(c.name+"/"+cl.name, func(t *testing.T) {
				invID := seedClaimInvoice(t, db, ctx, tenantID, "finalized", "pending", 5000)
				execTenant(t, db, ctx, tenantID, `UPDATE invoices SET auto_charge_pending = true WHERE id = $1`, invID)
				for _, r := range c.rows {
					seedHostedRow(t, db, ctx, tenantID, invID, r)
				}
				got, err := cl.claim(ctx, tenantID, invID)
				if err != nil {
					t.Fatalf("claim: %v", err)
				}
				if got == c.block {
					t.Fatalf("claimed = %v, want %v", got, !c.block)
				}
			})
		}
	}
}

// TestClaimOpen_RefusedWhileVeloxChargesOrAfterAnUnrecordedPayment is the
// other direction: no hosted page while a Velox charge holds the lease
// (payment_status is still pending until the call returns), and no second
// page once the customer has paid one whose payment is not recorded.
func TestClaimOpen_RefusedWhileVeloxChargesOrAfterAnUnrecordedPayment(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Hosted Claim Refusals")
	sessions := NewCheckoutSessionStore(db)
	invoices := invoice.NewPostgresStore(db)

	t.Run("charge lease held", func(t *testing.T) {
		invID := seedClaimInvoice(t, db, ctx, tenantID, "finalized", "failed", 5000)
		if ok, err := invoices.ClaimChargeForDunningRetry(ctx, tenantID, invID); err != nil || !ok {
			t.Fatalf("take lease: ok=%v err=%v", ok, err)
		}
		if _, _, err := sessions.ClaimOpen(ctx, tenantID, invID, 5000, "USD", false); !errors.Is(err, ErrChargeInFlight) {
			t.Fatalf("err = %v, want ErrChargeInFlight", err)
		}
		// Negative control: the charge finished and released the lease.
		if err := invoices.ReleaseAutoChargeClaim(ctx, tenantID, invID); err != nil {
			t.Fatalf("release: %v", err)
		}
		if _, winner, err := sessions.ClaimOpen(ctx, tenantID, invID, 5000, "USD", false); err != nil || !winner {
			t.Fatalf("after release: winner=%v err=%v", winner, err)
		}
	})

	t.Run("a paid session not recorded yet", func(t *testing.T) {
		invID := seedClaimInvoice(t, db, ctx, tenantID, "finalized", "pending", 5000)
		seedHostedRow(t, db, ctx, tenantID, invID, hostedRow{status: "completed", recorded: true, created: -20 * time.Minute, expires: ahead(40 * time.Minute)})
		if _, _, err := sessions.ClaimOpen(ctx, tenantID, invID, 5000, "USD", false); !errors.Is(err, ErrChargeInFlight) {
			t.Fatalf("err = %v, want ErrChargeInFlight", err)
		}
	})

	t.Run("an expired unpaid session does not refuse", func(t *testing.T) {
		invID := seedClaimInvoice(t, db, ctx, tenantID, "finalized", "pending", 5000)
		seedHostedRow(t, db, ctx, tenantID, invID, hostedRow{status: "expired", recorded: true, created: -2 * time.Hour, expires: ago(time.Hour)})
		if _, winner, err := sessions.ClaimOpen(ctx, tenantID, invID, 5000, "USD", false); err != nil || !winner {
			t.Fatalf("winner=%v err=%v", winner, err)
		}
	})

	t.Run("claim returns its creation time", func(t *testing.T) {
		invID := seedClaimInvoice(t, db, ctx, tenantID, "finalized", "pending", 5000)
		c, _, err := sessions.ClaimOpen(ctx, tenantID, invID, 5000, "USD", false)
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if c.CreatedAt.IsZero() || time.Since(c.CreatedAt) > time.Minute {
			t.Fatalf("CreatedAt = %v, want the row's created_at (the Stripe expiry anchors on it)", c.CreatedAt)
		}
	})
}

// TestChargeClaim_SeesHostedClaimCommittedWhileItWaited pins the lock-first
// order in the charge claims. A hosted claim holds FOR SHARE on the invoice
// while it inserts the session and commits without changing the invoice
// row. A charge claim that started meanwhile waits on that lock; it must then
// see the new session and refuse. As a single UPDATE it would proceed on its
// pre-commit snapshot and take the lease: both payers live.
func TestChargeClaim_SeesHostedClaimCommittedWhileItWaited(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Hosted Claim Interleave")
	invoices := invoice.NewPostgresStore(db)
	invID := seedClaimInvoice(t, db, ctx, tenantID, "finalized", "failed", 5000)

	// Stand-in for ClaimOpenAudited mid-transaction: the same FOR SHARE read
	// and the same INSERT, held open.
	hosted, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin hosted: %v", err)
	}
	defer postgres.Rollback(hosted)
	if _, err := hosted.ExecContext(ctx, `SELECT 1 FROM invoices WHERE id = $1 FOR SHARE`, invID); err != nil {
		t.Fatalf("for share: %v", err)
	}
	if _, err := hosted.ExecContext(ctx, `
		INSERT INTO checkout_sessions (id, tenant_id, invoice_id, livemode, amount_cents, currency, status)
		VALUES ($1, $2, $3, false, 5000, 'USD', 'open')
	`, postgres.NewID("vlx_cks"), tenantID, invID); err != nil {
		t.Fatalf("insert: %v", err)
	}

	type result struct {
		ok  bool
		err error
	}
	done := make(chan result, 1)
	go func() {
		ok, err := invoices.ClaimChargeForDunningRetry(ctx, tenantID, invID)
		done <- result{ok, err}
	}()
	// The claim queues on its lock-first read behind the hosted FOR SHARE.
	testutil.WaitForLockWaiter(t, ctx, testutil.AdminPool(t), "SELECT 1 FROM invoices WHERE id = $1 FOR UPDATE")

	if err := hosted.Commit(); err != nil {
		t.Fatalf("commit hosted: %v", err)
	}
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("claim: %v", r.err)
		}
		if r.ok {
			t.Fatal("charge claim took the lease although a hosted page committed while it waited: both payers live")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("charge claim did not finish after the hosted claim committed")
	}
}

// TestListHostedToReconcile_Selection: the sweep looks at sessions whose
// outcome Velox has not recorded, once the webhook has had 5 minutes to
// record it first; completed sessions only while their invoice is still
// unpaid.
func TestListHostedToReconcile_Selection(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Hosted Sweep Selection")
	sessions := NewCheckoutSessionStore(db)

	// A fresh invoice per row: at most one open row per invoice.
	unpaid := func() string { return seedClaimInvoice(t, db, ctx, tenantID, "finalized", "failed", 5000) }
	paid := func() string { return seedClaimInvoice(t, db, ctx, tenantID, "paid", "succeeded", 0) }
	old := hostedRow{recorded: true, created: -2 * time.Hour, expires: ago(time.Hour)}
	with := func(status string, r hostedRow) hostedRow { r.status = status; return r }

	want := map[string]bool{
		seedHostedRow(t, db, ctx, tenantID, unpaid(), with("open", old)):                                  true,
		seedHostedRow(t, db, ctx, tenantID, unpaid(), with("superseded", old)):                            true,
		seedHostedRow(t, db, ctx, tenantID, paid(), with("invoice_settled", old)):                         true,
		seedHostedRow(t, db, ctx, tenantID, unpaid(), with("completed", old)):                             true,
		seedHostedRow(t, db, ctx, tenantID, paid(), with("completed", old)):                               false, // recorded
		seedHostedRow(t, db, ctx, tenantID, unpaid(), with("expired", old)):                               false,
		seedHostedRow(t, db, ctx, tenantID, unpaid(), hostedRow{status: "open", created: -2 * time.Hour}): false, // never recorded
		// A live session is checked too (a paid session is final at Stripe
		// at once), but only after the webhook's head start.
		seedHostedRow(t, db, ctx, tenantID, unpaid(), hostedRow{status: "open", recorded: true, created: -10 * time.Minute, expires: ahead(50 * time.Minute)}): true,
		seedHostedRow(t, db, ctx, tenantID, unpaid(), hostedRow{status: "open", recorded: true, created: -2 * time.Minute, expires: ahead(58 * time.Minute)}):  false,
	}

	got, err := sessions.ListHostedToReconcile(ctx, time.Now().Add(-5*time.Minute), 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	listed := map[string]bool{}
	for _, c := range got {
		listed[c.ID] = true
	}
	for id, w := range want {
		if listed[id] != w {
			t.Errorf("row %s listed = %v, want %v", id, listed[id], w)
		}
	}

	// A test-mode sweep never sees live-mode rows.
	liveGot, err := sessions.ListHostedToReconcile(postgres.WithLivemode(context.Background(), true), time.Now().Add(-5*time.Minute), 100)
	if err != nil {
		t.Fatalf("list live: %v", err)
	}
	for _, c := range liveGot {
		if c.TenantID == tenantID {
			t.Fatalf("live-mode sweep listed test-mode row %s", c.ID)
		}
	}
}

// TestFillSession_ReportsAReplacedClaim: a create that finishes after its
// claim was superseded still records the session (so the sweeps can find
// and expire it), but reports the claim's status so the caller does not hand
// the URL out beside the newer claim's page.
func TestFillSession_ReportsAReplacedClaim(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Fill Replaced Claim")
	sessions := NewCheckoutSessionStore(db)
	exp := time.Now().Add(time.Hour)

	live := seedClaimInvoice(t, db, ctx, tenantID, "finalized", "pending", 5000)
	c, _, err := sessions.ClaimOpen(ctx, tenantID, live, 5000, "USD", false)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if st, err := sessions.FillSession(ctx, tenantID, c.ID, "cs_live", "https://pay/live", exp); err != nil || st != "open" {
		t.Fatalf("fill open claim: status=%q err=%v", st, err)
	}

	replaced := seedClaimInvoice(t, db, ctx, tenantID, "finalized", "pending", 5000)
	c2, _, err := sessions.ClaimOpen(ctx, tenantID, replaced, 5000, "USD", false)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if won, err := sessions.Supersede(ctx, tenantID, c2.ID); err != nil || !won {
		t.Fatalf("supersede: won=%v err=%v", won, err)
	}
	st, err := sessions.FillSession(ctx, tenantID, c2.ID, "cs_late", "https://pay/late", exp)
	if err != nil {
		t.Fatalf("fill: %v", err)
	}
	if st != "superseded" {
		t.Fatalf("status = %q, want superseded (the caller must not hand this URL out)", st)
	}
	unresolved, err := sessions.ListUnresolvedForInvoice(ctx, tenantID, replaced)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(unresolved) != 1 || unresolved[0].StripeSessionID != "cs_late" {
		t.Fatalf("late session not recorded for the sweeps: %+v", unresolved)
	}
}
