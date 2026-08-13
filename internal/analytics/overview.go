package analytics

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"

	"github.com/sagarsuperuser/velox/internal/api/respond"
	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// OverviewResponse aggregates the headline billing metrics for a tenant over
// a requested period, plus matching values for the prior period so the UI can
// render period-over-period deltas without a second round-trip.
type OverviewResponse struct {
	Period string `json:"period"`

	// Currency is the tenant's default currency. All money figures below are
	// scoped to it — invoices in other currencies are excluded rather than
	// summed into a meaningless mixed-currency total. The dashboard renders
	// every amount labeled with this code.
	Currency string `json:"currency"`

	// Money (cents)
	MRR             int64 `json:"mrr"`
	MRRPrev         int64 `json:"mrr_prev"`
	ARR             int64 `json:"arr"`
	ARRPrev         int64 `json:"arr_prev"`
	Revenue         int64 `json:"revenue"`
	RevenuePrev     int64 `json:"revenue_prev"`
	OutstandingAR   int64 `json:"outstanding_ar"`
	AvgInvoiceValue int64 `json:"avg_invoice_value"`
	CreditBalance   int64 `json:"credit_balance_total"`

	// Counts
	ActiveCustomers  int   `json:"active_customers"`
	NewCustomers     int   `json:"new_customers"`
	ActiveSubs       int   `json:"active_subscriptions"`
	TrialingSubs     int   `json:"trialing_subscriptions"`
	PaidInvoices     int   `json:"paid_invoices"`
	FailedPayments   int   `json:"failed_payments"`
	OpenInvoices     int   `json:"open_invoices"`
	DunningActive    int   `json:"dunning_active"`
	RefundsAttention int   `json:"refunds_needing_attention"`
	UsageEvents      int64 `json:"usage_events"`

	// Rates (0..1)
	LogoChurnRate       float64 `json:"logo_churn_rate"`
	RevenueChurnRate    float64 `json:"revenue_churn_rate"`
	NRR                 float64 `json:"nrr"`
	DunningRecoveryRate float64 `json:"dunning_recovery_rate"`

	// MRR movement within the period (cents; churned/contraction are positive
	// magnitudes — the UI subtracts them visually).
	MRRMovement MRRMovementTotals `json:"mrr_movement"`
}

// MRRMovementTotals breaks MRR change into its standard SaaS components.
// Net = New + Expansion - Contraction - Churned.
type MRRMovementTotals struct {
	New         int64 `json:"new"`
	Expansion   int64 `json:"expansion"`
	Contraction int64 `json:"contraction"`
	Churned     int64 `json:"churned"`
	Net         int64 `json:"net"`
}

func (h *Handler) overview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenantID := auth.TenantID(ctx)
	period := parsePeriod(r.URL.Query().Get("period"))

	tx, err := h.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		slog.Error("analytics overview: begin tx", "error", err)
		respond.InternalError(w, r)
		return
	}
	defer func() { _ = tx.Rollback() }()

	resp := OverviewResponse{Period: period.Key}

	// Resolve the tenant's default currency once. Every invoice-money
	// aggregate (revenue, outstanding AR, avg invoice value) is scoped to it
	// so the dashboard never sums amounts across currencies into a corrupt
	// total. Defaults to USD when settings are unset (fresh tenant).
	defaultCurrency, err := defaultCurrencyFor(ctx, tx)
	if err != nil {
		slog.Error("analytics overview: default currency", "error", err)
		respond.InternalError(w, r)
		return
	}
	resp.Currency = defaultCurrency

	// MRR now and at the start of the period (approximated from current
	// subscription state — see mrrAtPointInTime).
	var err1, err2 error
	resp.MRR, err1 = currentMRR(ctx, tx, defaultCurrency)
	resp.MRRPrev, err2 = mrrAtPointInTime(ctx, tx, period.Start, defaultCurrency)
	if err := firstErr(err1, err2); err != nil {
		slog.Error("analytics overview: mrr", "error", err)
		respond.InternalError(w, r)
		return
	}
	resp.ARR = resp.MRR * 12
	resp.ARRPrev = resp.MRRPrev * 12

	// Revenue: paid invoices inside the period vs. the prior period.
	resp.Revenue, err1 = sumPaidRevenue(ctx, tx, period.Start, period.End, defaultCurrency)
	resp.RevenuePrev, err2 = sumPaidRevenue(ctx, tx, period.PrevStart, period.PrevEnd, defaultCurrency)
	if err := firstErr(err1, err2); err != nil {
		slog.Error("analytics overview: revenue", "error", err)
		respond.InternalError(w, r)
		return
	}

	// Snapshot counts
	queries := []struct {
		name string
		dst  any
		sql  string
		args []any
	}{
		{"active_customers", &resp.ActiveCustomers,
			`SELECT COUNT(*) FROM customers WHERE status = 'active'` + notSimCustomer, nil},
		{"new_customers", &resp.NewCustomers,
			`SELECT COUNT(*) FROM customers WHERE created_at >= $1 AND created_at < $2` + notSimCustomer,
			[]any{period.Start, period.End}},
		{"active_subs", &resp.ActiveSubs,
			`SELECT COUNT(*) FROM subscriptions WHERE status = 'active'` + notSimSubBare, nil},
		{"trialing_subs", &resp.TrialingSubs,
			`SELECT COUNT(*) FROM subscriptions WHERE status = 'active' AND trial_end_at IS NOT NULL AND trial_end_at > now()` + notSimSubBare, nil},
		{"outstanding_ar", &resp.OutstandingAR,
			`SELECT COALESCE(SUM(amount_due_cents), 0) FROM invoices WHERE status = 'finalized' AND payment_status != 'succeeded' AND currency = $1` + notSimInvoice,
			[]any{defaultCurrency}},
		{"avg_invoice", &resp.AvgInvoiceValue,
			`SELECT COALESCE(AVG(total_amount_cents), 0)::bigint FROM invoices WHERE status = 'paid' AND currency = $1 AND paid_at >= $2 AND paid_at < $3` + notSimInvoice,
			[]any{defaultCurrency, period.Start, period.End}},
		{"paid_invoices", &resp.PaidInvoices,
			`SELECT COUNT(*) FROM invoices WHERE status = 'paid' AND currency = $1 AND paid_at >= $2 AND paid_at < $3` + notSimInvoice,
			[]any{defaultCurrency, period.Start, period.End}},
		{"failed_payments", &resp.FailedPayments,
			`SELECT COUNT(*) FROM invoices WHERE payment_status = 'failed' AND created_at >= $1 AND created_at < $2` + notSimInvoice,
			[]any{period.Start, period.End}},
		{"open_invoices", &resp.OpenInvoices,
			`SELECT COUNT(*) FROM invoices WHERE status = 'finalized' AND payment_status != 'succeeded'` + notSimInvoice, nil},
		{"dunning_active", &resp.DunningActive,
			// Terminal states are 'resolved' and 'escalated' — see
			// domain.DunningRunState. Pre-fix this read 'exhausted',
			// which is not a real state, so 'escalated' runs slipped
			// through and the dashboard reported every terminal-escalated
			// invoice as still actively retrying.
			`SELECT COUNT(*) FROM invoice_dunning_runs WHERE state NOT IN ('resolved', 'escalated')` + notSimViaInvoice, nil},
		// tenant_id is explicit even though the tx is RLS-scoped: the RLS
		// policy is an OR (bypass_rls OR tenant match), which Postgres can
		// only apply as a post-scan Filter. usage_events is the one table
		// here big enough for that to matter — without the predicate this
		// COUNT seq-scans every tenant's events on every dashboard load.
		{"usage_events", &resp.UsageEvents,
			`SELECT COUNT(*) FROM usage_events WHERE tenant_id = $1 AND timestamp >= $2 AND timestamp < $3` + notSimViaCustomer,
			[]any{tenantID, period.Start, period.End}},
		// Issued credit notes whose Stripe refund leg is failed/pending — i.e.
		// a customer is owed money that hasn't been pushed back yet. The refund
		// is operator-retried (no auto-sweep), so surfacing the count is what
		// turns "an operator will notice" into "an operator is told". Real
		// refunds only (test-clock sims aren't an operator obligation).
		// failed (terminal — customer un-refunded) OR a 'pending' STUCK >72h
		// (~3 business days). Fresh pending is normal async settlement, NOT an
		// alert — else every healthy in-flight refund would light the banner.
		{"refunds_needing_attention", &resp.RefundsAttention,
			`SELECT COUNT(*) FROM credit_notes WHERE status = 'issued' AND (refund_status = 'failed' OR (refund_status = 'pending' AND updated_at < now() - interval '72 hours')) AND is_simulated = false`, nil},
	}
	for _, q := range queries {
		if err := tx.QueryRowContext(ctx, q.sql, q.args...).Scan(q.dst); err != nil {
			slog.Error("analytics overview", "query", q.name, "error", err)
			respond.InternalError(w, r)
			return
		}
	}

	// Credit balance: authoritative SUM(amount_cents) over the whole ledger,
	// matching credit.GetBalance. The previous DISTINCT ON (customer_id)
	// balance_after read the denormalized running balance of the latest row
	// BY created_at — but catchup / late-cron expiry entries can be inserted
	// out of created_at order, so "latest row" wasn't the true latest balance
	// and total credit liability was mis-reported. Summing amount_cents is
	// order-independent and exact.
	err = tx.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(amount_cents), 0) FROM customer_credit_ledger
		WHERE customer_id IN (SELECT id FROM customers WHERE test_clock_id IS NULL)
	`).Scan(&resp.CreditBalance)
	if err != nil {
		slog.Error("analytics overview: credit balance", "error", err)
		respond.InternalError(w, r)
		return
	}

	// MRR movement within the period
	mvt, err := computeMRRMovement(ctx, tx, period.Start, period.End, defaultCurrency)
	if err != nil {
		slog.Error("analytics overview: mrr movement", "error", err)
		respond.InternalError(w, r)
		return
	}
	resp.MRRMovement = mvt

	// Derived rates
	resp.LogoChurnRate = churnRate(ctx, tx, period.Start, period.End, resp.MRRPrev != 0)
	if resp.MRRPrev > 0 {
		resp.RevenueChurnRate = float64(mvt.Churned) / float64(resp.MRRPrev)
		resp.NRR = float64(resp.MRRPrev+mvt.Expansion-mvt.Contraction-mvt.Churned) / float64(resp.MRRPrev)
	}
	resp.DunningRecoveryRate = dunningRecoveryRate(ctx, tx, period.Start, period.End)

	_ = tx.Commit()
	respond.JSON(w, r, http.StatusOK, resp)
}

// currentMRR sums normalized-to-monthly base fees across all items of active
// subscriptions. Each item contributes plan.base × item.quantity, normalized
// from the plan's billing_interval. Scoped to the tenant's default currency —
// cents in different currencies must never land in one SUM.
func currentMRR(ctx context.Context, tx *sql.Tx, currency string) (int64, error) {
	var v int64
	err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(
			CASE WHEN p.billing_interval = 'yearly' THEN (p.base_amount_cents * si.quantity) / 12
			     ELSE p.base_amount_cents * si.quantity
			END
		), 0)
		FROM subscriptions s
		JOIN subscription_items si ON si.subscription_id = s.id AND si.deleted_at IS NULL
		JOIN plans p ON p.id = si.plan_id
		WHERE s.status = 'active'
		  AND p.currency = $1
		  AND s.test_clock_id IS NULL
	`, currency).Scan(&v)
	return v, err
}

// mrrAtPointInTime reconstructs MRR at instant t by replaying the audit log
// backwards from the current item state. For each item that existed at t, we
// sum its MRR using the plan/quantity it held at that moment — rewound
// through any subscription_item_changes that happened after t.
//
// An item's state at t is the most recent change event at or before t:
// 'add'/'plan'/'quantity' give its (plan, quantity) via to_*; a 'remove'
// means the item did NOT exist at t and is EXCLUDED (the original query
// ignored 'remove' entirely, so removed items were counted forever —
// MRRPrev/NRR/revenue-churn drifted permanently upward; audit HIGH #13).
// If no event exists at or before t (item's history starts after t), the
// earliest post-t event's from_* describes the pre-change state; an item
// whose first event is its post-t 'add' yields NULLs and drops out.
// Scoped to the tenant's default currency like every other MRR sum.
//
// The two LATERALs replace four correlated subqueries per item; both walk
// idx_sic_item (0131).
func mrrAtPointInTime(ctx context.Context, tx *sql.Tx, t any, currency string) (int64, error) {
	var v int64
	err := tx.QueryRowContext(ctx, `
		WITH items_at_t AS (
			SELECT
				pi.subscription_id,
				pi.subscription_item_id,
				last_le.change_type AS state_type,
				COALESCE(last_le.to_plan_id, first_gt.from_plan_id)     AS plan_id,
				COALESCE(last_le.to_quantity, first_gt.from_quantity)   AS quantity
			FROM (
				SELECT DISTINCT c.subscription_id, c.subscription_item_id
				FROM subscription_item_changes c
			) pi
			LEFT JOIN LATERAL (
				SELECT c2.change_type, c2.to_plan_id, c2.to_quantity
				FROM subscription_item_changes c2
				WHERE c2.subscription_item_id = pi.subscription_item_id
				  AND c2.changed_at <= $1
				  AND c2.change_type IN ('add', 'plan', 'quantity', 'remove')
				ORDER BY c2.changed_at DESC LIMIT 1
			) last_le ON true
			LEFT JOIN LATERAL (
				SELECT c3.from_plan_id, c3.from_quantity
				FROM subscription_item_changes c3
				WHERE c3.subscription_item_id = pi.subscription_item_id
				  AND c3.changed_at > $1
				  AND c3.change_type IN ('plan', 'quantity', 'remove')
				  AND c3.from_plan_id IS NOT NULL
				ORDER BY c3.changed_at ASC LIMIT 1
			) first_gt ON true
		)
		SELECT COALESCE(SUM(
			CASE WHEN p.billing_interval = 'yearly' THEN (p.base_amount_cents * i.quantity) / 12
			     ELSE p.base_amount_cents * i.quantity
			END
		), 0)
		FROM items_at_t i
		JOIN subscriptions s ON s.id = i.subscription_id
		JOIN plans p ON p.id = i.plan_id
		WHERE (i.state_type IS NULL OR i.state_type <> 'remove')
		  AND i.plan_id IS NOT NULL
		  AND i.quantity IS NOT NULL
		  AND p.currency = $2
		  AND s.activated_at IS NOT NULL
		  AND s.activated_at <= $1
		  AND (s.canceled_at IS NULL OR s.canceled_at > $1)
		  AND s.test_clock_id IS NULL
	`, t, currency).Scan(&v)
	return v, err
}

// defaultCurrencyFor returns the tenant's configured default currency, or
// "USD" when unset. Analytics money totals are scoped to this currency —
// invoices in other currencies are excluded rather than summed into a corrupt
// mixed-currency total. Returns a non-nil error only on a real DB failure (a
// missing settings row falls back to USD).
func defaultCurrencyFor(ctx context.Context, tx *sql.Tx) (string, error) {
	cur := "USD"
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(NULLIF(default_currency, ''), 'USD') FROM tenant_settings LIMIT 1`,
	).Scan(&cur); err != nil && err != sql.ErrNoRows {
		return "USD", err
	}
	return cur, nil
}

func sumPaidRevenue(ctx context.Context, tx *sql.Tx, start, end any, currency string) (int64, error) {
	var v int64
	err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(total_amount_cents), 0)
		FROM invoices
		WHERE status = 'paid' AND paid_at >= $1 AND paid_at < $2 AND currency = $3 AND is_simulated = false
	`, start, end, currency).Scan(&v)
	return v, err
}

// computeMRRMovement returns gross new/expansion/contraction/churned MRR
// within [start, end).
//
// New / Churned use current subscription_items (items are preserved on
// cancel — only the subscription's status/canceled_at change). Expansion /
// Contraction come from the subscription_item_changes audit log, restricted
// to subscriptions that were active throughout the period so their events
// don't double-count against New / Churned.
func computeMRRMovement(ctx context.Context, tx *sql.Tx, start, end any, currency string) (MRRMovementTotals, error) {
	var m MRRMovementTotals

	// New MRR: items of subscriptions activated in [start, end).
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(
			CASE WHEN p.billing_interval = 'yearly' THEN (p.base_amount_cents * si.quantity) / 12
			     ELSE p.base_amount_cents * si.quantity
			END
		), 0)
		FROM subscriptions s
		JOIN subscription_items si ON si.subscription_id = s.id AND si.deleted_at IS NULL
		JOIN plans p ON p.id = si.plan_id
		WHERE s.activated_at >= $1 AND s.activated_at < $2
		  AND p.currency = $3
		  AND s.test_clock_id IS NULL
	`, start, end, currency).Scan(&m.New); err != nil {
		return m, err
	}

	// Churned MRR: items of subscriptions canceled in [start, end).
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(
			CASE WHEN p.billing_interval = 'yearly' THEN (p.base_amount_cents * si.quantity) / 12
			     ELSE p.base_amount_cents * si.quantity
			END
		), 0)
		FROM subscriptions s
		JOIN subscription_items si ON si.subscription_id = s.id AND si.deleted_at IS NULL
		JOIN plans p ON p.id = si.plan_id
		WHERE s.canceled_at >= $1 AND s.canceled_at < $2
		  AND p.currency = $3
		  AND s.test_clock_id IS NULL
	`, start, end, currency).Scan(&m.Churned); err != nil {
		return m, err
	}

	// Expansion / Contraction: plan, quantity, AND item add/remove changes
	// on subs that were active throughout the period. Item adds/removes on
	// continuing subs ARE expansion/contraction — the old inner joins on
	// from_plan_id/to_plan_id silently dropped them ('add' has no from_*,
	// 'remove' has no to_*), so Net never reconciled with the headline MRR
	// delta. Subs activated or canceled inside the period stay excluded —
	// their MRR impact (including their items' add/remove rows) is already
	// in New / Churned.
	err := tx.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(CASE WHEN delta > 0 THEN delta ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN delta < 0 THEN -delta ELSE 0 END), 0)
		FROM (
			SELECT (
				COALESCE(CASE WHEN pto.billing_interval = 'yearly' THEN pto.base_amount_cents * c.to_quantity / 12
				      ELSE pto.base_amount_cents * c.to_quantity END, 0)
			  - COALESCE(CASE WHEN pfrom.billing_interval = 'yearly' THEN pfrom.base_amount_cents * c.from_quantity / 12
				      ELSE pfrom.base_amount_cents * c.from_quantity END, 0)
			) AS delta
			FROM subscription_item_changes c
			JOIN subscriptions s ON s.id = c.subscription_id
			LEFT JOIN plans pfrom ON pfrom.id = c.from_plan_id
			LEFT JOIN plans pto ON pto.id = c.to_plan_id
			WHERE c.change_type IN ('plan', 'quantity', 'add', 'remove')
			  AND c.changed_at >= $1 AND c.changed_at < $2
			  AND (pfrom.id IS NULL OR pfrom.currency = $3)
			  AND (pto.id IS NULL OR pto.currency = $3)
			  AND s.activated_at IS NOT NULL
			  AND s.activated_at < $1
			  AND (s.canceled_at IS NULL OR s.canceled_at >= $2)
			  AND s.test_clock_id IS NULL
		) d
	`, start, end, currency).Scan(&m.Expansion, &m.Contraction)
	if err != nil {
		return m, err
	}

	m.Net = m.New + m.Expansion - m.Contraction - m.Churned
	return m, nil
}

// churnRate = canceled-in-period / active-at-period-start.
// Silent-fails to 0 on query errors to avoid nuking the whole overview for a
// derived metric — the caller has already reported success on core fields.
func churnRate(ctx context.Context, tx *sql.Tx, start, end any, _ bool) float64 {
	var canceled, activeAtStart int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM subscriptions
		WHERE canceled_at >= $1 AND canceled_at < $2 AND test_clock_id IS NULL
	`, start, end).Scan(&canceled); err != nil {
		return 0
	}
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM subscriptions
		WHERE activated_at IS NOT NULL
		  AND activated_at <= $1
		  AND (canceled_at IS NULL OR canceled_at > $1)
		  AND test_clock_id IS NULL
	`, start).Scan(&activeAtStart); err != nil {
		return 0
	}
	if activeAtStart == 0 {
		return 0
	}
	return float64(canceled) / float64(activeAtStart)
}

// dunningRecoveryRate = resolved-in-period / opened-in-period. Only counts
// resolutions that landed on 'resolved' (successful recovery), ignoring
// 'exhausted' / 'written_off' outcomes.
func dunningRecoveryRate(ctx context.Context, tx *sql.Tx, start, end any) float64 {
	var opened, recovered int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM invoice_dunning_runs
		WHERE created_at >= $1 AND created_at < $2 AND invoice_id IN (SELECT id FROM invoices WHERE is_simulated = false)
	`, start, end).Scan(&opened); err != nil {
		return 0
	}
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM invoice_dunning_runs
		WHERE created_at >= $1 AND created_at < $2 AND state = 'resolved' AND invoice_id IN (SELECT id FROM invoices WHERE is_simulated = false)
	`, start, end).Scan(&recovered); err != nil {
		return 0
	}
	if opened == 0 {
		return 0
	}
	return float64(recovered) / float64(opened)
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}
