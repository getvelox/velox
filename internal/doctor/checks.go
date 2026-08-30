// Code-adjacent data: the doctor's invariant catalog.
//
// Every check here was mined from the domain's writers and then SURVIVED an
// adversarial pass hunting legal counterexamples (grandfathered cohorts,
// deliberate states, simulated rows, lagging best-effort stamps) — the full
// per-check reasoning and writer citations live in the mining run recorded in
// the PR that added each check. Checks that a DB constraint already makes
// unrepresentable were deliberately KILLED, not shipped (no belt-and-
// suspenders). Exclusion fences you will see below and must not "simplify"
// away:
//
//   - stripe_invoice_id IS NULL       — importer rows dropped components
//   - id NOT LIKE 'vlx_inv_safe%%'     — velox-migrate-safety seeder rows
//   - created_at >= '2026-07-05'      — pre-#365/#347 grandfathered subs
//   - grace intervals on sweeps       — post-commit stamps lag their tx
package doctor

// Checks is the shipping catalog. Order groups by domain for readable output.
var Checks = []Check{
	{
		Name:   "invoice_total_identity",
		Domain: "invoices",
		Why:    "total must equal subtotal + tax - discount (importer rows and the negative-clamp excluded); drift means a rated amount nobody computed",
		SQL:    `SELECT id, tenant_id, status, subtotal_cents, discount_cents, tax_amount_cents, total_amount_cents FROM invoices WHERE stripe_invoice_id IS NULL AND total_amount_cents <> subtotal_cents + tax_amount_cents - discount_cents AND NOT (total_amount_cents = 0 AND subtotal_cents + tax_amount_cents - discount_cents < 0)`,
	},
	{
		Name:   "invoice_due_conservation",
		Domain: "invoices",
		Why:    "amount_due must equal GREATEST(total - paid - credits_applied, 0) absent credit notes; drift means money owed and money booked disagree",
		SQL:    `SELECT i.id, i.tenant_id, i.status, i.payment_status, i.total_amount_cents, i.amount_paid_cents, i.credits_applied_cents, i.amount_due_cents FROM invoices i WHERE i.stripe_invoice_id IS NULL AND i.id NOT LIKE 'vlx_inv_safe%' AND NOT EXISTS (SELECT 1 FROM credit_notes cn WHERE cn.invoice_id = i.id) AND i.amount_due_cents <> GREATEST(i.total_amount_cents - i.amount_paid_cents - i.credits_applied_cents, 0)`,
	},
	{
		Name:   "invoice_paid_settled_coherence",
		Domain: "invoices",
		Why:    "a paid/succeeded invoice must carry paid_at and zero due — the single settle writer stamps all three atomically",
		SQL:    `SELECT id, tenant_id, status, payment_status, paid_at, amount_due_cents, amount_paid_cents FROM invoices WHERE stripe_invoice_id IS NULL AND status = 'paid' AND payment_status = 'succeeded' AND (paid_at IS NULL OR amount_due_cents <> 0)`,
	},
	{
		Name:   "invoice_terminal_timestamp_coherence",
		Domain: "invoices",
		Why:    "voided=>voided_at, finalized=>issued_at, and uncollectible_at never on draft/finalized — the status machine stamps these in the same UPDATE",
		SQL:    `SELECT id, tenant_id, status, issued_at, voided_at, uncollectible_at FROM invoices WHERE stripe_invoice_id IS NULL AND id NOT LIKE 'vlx_inv_safe%' AND ((status = 'voided' AND voided_at IS NULL) OR (status = 'finalized' AND issued_at IS NULL) OR (uncollectible_at IS NOT NULL AND status IN ('draft','finalized')))`,
	},
	{
		Name:   "ledger_row_structural_bounds",
		Domain: "credit-ledger",
		Why:    "a credit block can never be over-consumed, and non-block rows carry no consumption",
		SQL: `SELECT id, tenant_id, customer_id, entry_type, amount_cents, consumed_cents, cn_retired_cents, grant_kind, invoice_id, created_at,
  CASE
    WHEN amount_cents > 0 AND consumed_cents > amount_cents THEN 'consumed exceeds block amount'
    WHEN amount_cents <= 0 AND consumed_cents <> 0 THEN 'consumed_cents set on non-block row'
    WHEN cn_retired_cents < 0 OR cn_retired_cents > consumed_cents THEN 'cn_retired outside [0, consumed]'
    WHEN cn_retired_cents <> 0 AND grant_kind IS DISTINCT FROM 'commit' THEN 'cn_retired on non-commit row'
    WHEN entry_type = 'grant' AND amount_cents <= 0 THEN 'non-positive grant'
    WHEN entry_type = 'usage' AND amount_cents >= 0 THEN 'non-negative usage'
    WHEN entry_type = 'expiry' AND amount_cents >= 0 THEN 'non-negative expiry'
    WHEN entry_type = 'usage' AND COALESCE(invoice_id, '') = '' THEN 'usage without invoice link'
    WHEN amount_cents = 0 THEN 'zero-amount entry'
  END AS violation
FROM customer_credit_ledger
WHERE (amount_cents > 0 AND consumed_cents > amount_cents)
   OR (amount_cents <= 0 AND consumed_cents <> 0)
   OR cn_retired_cents < 0 OR cn_retired_cents > consumed_cents
   OR (cn_retired_cents <> 0 AND grant_kind IS DISTINCT FROM 'commit')
   OR (entry_type = 'grant' AND amount_cents <= 0)
   OR (entry_type = 'usage' AND (amount_cents >= 0 OR COALESCE(invoice_id, '') = ''))
   OR (entry_type = 'expiry' AND amount_cents >= 0)
   OR amount_cents = 0;`,
	},
	{
		Name:   "balance_exceeds_block_capacity",
		Domain: "credit-ledger",
		Why:    "a customer's materialized balance can never exceed the open capacity of their live blocks",
		SQL: `SELECT tenant_id, customer_id,
       SUM(amount_cents) AS balance_cents,
       SUM(CASE WHEN amount_cents > 0 THEN amount_cents - consumed_cents ELSE 0 END) AS block_remaining_cents,
       SUM(amount_cents) - SUM(CASE WHEN amount_cents > 0 THEN amount_cents - consumed_cents ELSE 0 END) AS phantom_cents
FROM customer_credit_ledger
GROUP BY tenant_id, customer_id
HAVING SUM(amount_cents) > SUM(CASE WHEN amount_cents > 0 THEN amount_cents - consumed_cents ELSE 0 END);`,
	},
	{
		Name:   "expired_grant_headroom_reopened",
		Domain: "credit-ledger",
		Why:    "an expired grant must stay fully consumed-or-expired; reopened headroom is money reappearing after expiry",
		SQL: `SELECT g.id, g.tenant_id, g.customer_id, g.amount_cents, g.consumed_cents,
       g.amount_cents - g.consumed_cents AS reopened_headroom_cents, g.expires_at,
       e.id AS expiry_entry_id, e.amount_cents AS expiry_amount_cents, e.created_at AS expired_at
FROM customer_credit_ledger g
JOIN customer_credit_ledger e
  ON e.tenant_id = g.tenant_id AND e.customer_id = g.customer_id
 AND e.entry_type = 'expiry' AND e.description = 'Expired grant ' || g.id
WHERE g.entry_type = 'grant' AND g.consumed_cents < g.amount_cents;`,
	},
	{
		Name:   "cn_total_identity",
		Domain: "credit-notes",
		Why:    "credit-note total must equal subtotal + tax",
		SQL:    `SELECT id, tenant_id, credit_note_number, status, subtotal_cents, tax_amount_cents, total_cents FROM credit_notes WHERE subtotal_cents + tax_amount_cents <> total_cents;`,
	},
	{
		Name:   "cn_lines_sum_to_gross_total",
		Domain: "credit-notes",
		Why:    "credit-note lines are GROSS and must sum to the note total",
		SQL:    `SELECT cn.id, cn.tenant_id, cn.credit_note_number, cn.status, cn.total_cents, COALESCE(SUM(li.amount_cents), 0) AS line_sum_cents, COUNT(li.id) AS line_count FROM credit_notes cn LEFT JOIN credit_note_line_items li ON li.credit_note_id = cn.id GROUP BY cn.id HAVING COALESCE(SUM(li.amount_cents), 0) <> cn.total_cents;`,
	},
	{
		Name:   "cn_allocation_split_all_or_nothing",
		Domain: "credit-notes",
		Why:    "an issued note's refund/out-of-band/credit/commit-retired split must exactly cover its total",
		SQL:    `SELECT id, tenant_id, credit_note_number, status, total_cents, refund_amount_cents, credit_amount_cents, out_of_band_amount_cents, commit_retired_cents FROM credit_notes WHERE refund_amount_cents < 0 OR credit_amount_cents < 0 OR out_of_band_amount_cents < 0 OR (refund_amount_cents + credit_amount_cents + out_of_band_amount_cents) NOT IN (0, total_cents);`,
	},
	{
		Name:   "cn_issue_pending_cleared_on_transition",
		Domain: "credit-notes",
		Why:    "issue_pending must clear in the same statement as the status transition (the #740 class)",
		SQL:    `SELECT id, tenant_id, credit_note_number, status, issue_pending, issued_at, voided_at FROM credit_notes WHERE issue_pending AND status <> 'draft' AND recorded_at IS NOT NULL;`,
	},
	{
		Name:   "cn_status_timestamp_coherence",
		Domain: "credit-notes",
		Why:    "issued=>issued_at, voided=>voided_at on credit notes",
		SQL:    `SELECT id, tenant_id, credit_note_number, status, issued_at, voided_at, created_at, recorded_at FROM credit_notes WHERE (status = 'draft' AND (issued_at IS NOT NULL OR voided_at IS NOT NULL)) OR (status = 'issued' AND issued_at IS NULL) OR (status = 'voided' AND voided_at IS NULL) OR (issued_at IS NOT NULL AND voided_at IS NOT NULL AND recorded_at IS NOT NULL);`,
	},
	{
		Name:   "dunning_active_run_on_terminal_invoice_stale",
		Domain: "dunning",
		Why:    "no ACTIVE dunning run may persist on a terminal invoice once the resolve seam has had time to run",
		SQL:    `SELECT r.id, r.tenant_id, r.invoice_id, r.state, r.resolution, r.next_action_at, r.updated_at AS run_updated_at, i.status AS invoice_status, i.payment_status, i.amount_due_cents, i.currency FROM invoice_dunning_runs r JOIN invoices i ON i.id = r.invoice_id JOIN customers c ON c.id = i.customer_id WHERE r.state = 'active' AND r.paused = false AND i.status IN ('paid','voided','uncollectible') AND i.is_simulated = false AND c.test_clock_id IS NULL AND r.next_action_at IS NOT NULL AND r.next_action_at < now() - interval '48 hours' AND r.updated_at < now() - interval '48 hours' AND i.updated_at < now() - interval '48 hours'`,
	},
	{
		Name:   "dunning_resolved_run_missing_resolved_at_or_pending_action",
		Domain: "dunning",
		Why:    "a resolved run must carry resolved_at + resolution and no pending action",
		SQL:    `SELECT r.id, r.tenant_id, r.invoice_id, r.state, r.resolution, r.resolved_at, r.next_action_at, r.updated_at FROM invoice_dunning_runs r WHERE r.state = 'resolved' AND (r.resolved_at IS NULL OR r.next_action_at IS NOT NULL)`,
	},
	{
		Name:   "dunning_escalated_run_shape",
		Domain: "dunning",
		Why:    "an escalated run must be retries_exhausted with resolved_at set and no next action",
		SQL:    `SELECT r.id, r.tenant_id, r.invoice_id, r.state, r.resolution, r.resolved_at, r.next_action_at, r.attempt_count FROM invoice_dunning_runs r WHERE r.state = 'escalated' AND (r.resolution IS DISTINCT FROM 'retries_exhausted' OR r.resolved_at IS NULL OR r.next_action_at IS NOT NULL)`,
	},
	{
		Name:   "dunning_action_failed_marker_on_closed_run",
		Domain: "dunning",
		Why:    "action_failed is a re-attempt marker — it may not survive on a closed run",
		SQL:    `SELECT r.id, r.tenant_id, r.invoice_id, r.state, r.resolution, r.resolved_at, r.next_action_at, r.attempt_count FROM invoice_dunning_runs r WHERE r.resolution = 'action_failed' AND r.state <> 'resolved' AND (r.state <> 'active' OR r.resolved_at IS NOT NULL)`,
	},
	{
		Name:   "canceled_status_iff_canceled_at",
		Domain: "subscriptions",
		Why:    "canceled status and canceled_at imply each other",
		SQL:    `SELECT id, tenant_id, code, status, canceled_at, cancel_at, cancel_at_period_end, updated_at FROM subscriptions WHERE (status = 'canceled') <> (canceled_at IS NOT NULL);`,
	},
	{
		Name:   "cycle_invoice_unique_per_subscription_period",
		Domain: "invoices",
		Why:    "each closed billing period bills exactly once per subscription; a second live cycle invoice for the same period is a double charge",
		// This duplicates what idx_invoices_billing_idempotency already
		// enforces, and that is the point: the two fail independently. An
		// index can be absent after a restore, dropped by a migration, left
		// INVALID by a failed CREATE INDEX CONCURRENTLY (which enforces
		// nothing while still appearing in pg_indexes), or quietly narrowed
		// by a predicate change — migration 0101 narrowed this exact
		// predicate once already. In every one of those cases the write path
		// keeps succeeding and nothing raises an error, so the only way to
		// learn about it is to look for the duplicate rows themselves.
		// Measured: with the index removed, four concurrent billing leaders
		// produced 129 invoices across 40 periods and reported no failures.
		SQL: `SELECT i.id, i.tenant_id, i.subscription_id, i.billing_period_start, i.billing_period_end, i.status, i.total_amount_cents FROM invoices i WHERE i.subscription_id IS NOT NULL AND i.status <> 'voided' AND i.source_plan_changed_at IS NULL AND EXISTS (SELECT 1 FROM invoices d WHERE d.tenant_id = i.tenant_id AND d.subscription_id = i.subscription_id AND d.billing_period_start = i.billing_period_start AND d.billing_period_end = i.billing_period_end AND d.status <> 'voided' AND d.source_plan_changed_at IS NULL AND d.id <> i.id);`,
	},
	{
		Name:   "usage_billed_once_per_subscription_meter_window",
		Domain: "invoices",
		Why:    "the same usage instant of one (subscription, meter, rating-rule bucket) must sit on at most one live invoice; a second is a double charge the header-exact check cannot see (threshold + cycle, or a rewound period re-billed)",
		// Read-side twin of ADR-115's period CAS: the CAS makes two closers of
		// one period unable to both commit; this looks for the money shape the
		// CAS exists to prevent — two live invoices whose usage windows for
		// the same meter overlap. Half-open compare, so a threshold window
		// [P0,t1) beside its residual [t1,P1) is clean. Paired per rating-rule
		// bucket, not per meter: under the ADR-066 §4 deferred-bucket
		// protocol one meter can carry a sum rule (billed on the threshold
		// invoice) and a max rule (deferred to the cycle invoice, full
		// window), and the max bucket's window legitimately overlaps the sum
		// bucket's. Lines on one invoice are excluded (i.id < j.id).
		SQL: `SELECT a.invoice_id, a.tenant_id, i.subscription_id, a.meter_id, a.rating_rule_version_id, a.billing_period_start, a.billing_period_end, b.invoice_id AS other_invoice_id
		        FROM invoice_line_items a
		        JOIN invoices i ON i.id = a.invoice_id
		        JOIN invoices j ON j.subscription_id = i.subscription_id AND j.tenant_id = i.tenant_id AND j.id <> i.id
		        JOIN invoice_line_items b ON b.invoice_id = j.id AND b.meter_id = a.meter_id AND b.line_type = 'usage'
		         AND b.rating_rule_version_id IS NOT DISTINCT FROM a.rating_rule_version_id
		       WHERE a.line_type = 'usage' AND i.subscription_id IS NOT NULL
		         AND i.status NOT IN ('voided','uncollectible') AND j.status NOT IN ('voided','uncollectible')
		         AND a.billing_period_start IS NOT NULL AND b.billing_period_start IS NOT NULL
		         AND a.billing_period_start < b.billing_period_end AND b.billing_period_start < a.billing_period_end
		         AND i.id < j.id;`,
	},
	{
		Name:   "billing_period_never_inverted",
		Domain: "subscriptions",
		Why:    "a billing period can never strictly invert (start > end)",
		SQL:    `SELECT id, tenant_id, code, status, current_billing_period_start, current_billing_period_end, next_billing_at, billing_anchor_day FROM subscriptions WHERE current_billing_period_start IS NOT NULL AND current_billing_period_end IS NOT NULL AND current_billing_period_start > current_billing_period_end;`,
	},
	{
		Name:   "live_sub_cycle_stamps_complete_and_watermark_aligned",
		Domain: "subscriptions",
		Why:    "live subscriptions (post-2026-07-05) must carry complete cycle stamps with next_billing_at == period end",
		SQL:    `SELECT id, tenant_id, code, status, created_at, started_at, activated_at, current_billing_period_start, current_billing_period_end, next_billing_at FROM subscriptions WHERE status IN ('active','trialing') AND created_at >= TIMESTAMPTZ '2026-07-05' AND (current_billing_period_start IS NULL OR current_billing_period_end IS NULL OR next_billing_at IS NULL OR started_at IS NULL OR (status = 'active' AND activated_at IS NULL) OR next_billing_at IS DISTINCT FROM current_billing_period_end);`,
	},
	{
		Name:   "trialing_trial_window_coherent",
		Domain: "subscriptions",
		Why:    "a trialing sub must carry a strictly positive trial window",
		SQL:    `SELECT id, tenant_id, code, status, trial_start_at, trial_end_at, started_at, created_at FROM subscriptions WHERE status = 'trialing' AND (trial_start_at IS NULL OR trial_end_at IS NULL OR trial_end_at <= trial_start_at);`,
	},
	{
		Name:   "scheduled_cancel_not_both_modes",
		Domain: "subscriptions",
		Why:    "a scheduled cancel is date-mode or period-end-mode, never both",
		SQL:    `SELECT id, tenant_id, code, status, cancel_at, cancel_at_period_end, current_billing_period_end, updated_at FROM subscriptions WHERE cancel_at IS NOT NULL AND cancel_at_period_end = true;`,
	},
	{
		Name:   "transport_outbox_dispatched_iff_dispatched_at",
		Domain: "transport",
		Why:    "outbox dispatched status and dispatched_at imply each other, both directions",
		SQL:    `SELECT 'webhook_outbox' AS src, id, tenant_id, status, attempts, dispatched_at, created_at, COALESCE(last_error,'') AS last_error FROM webhook_outbox WHERE (status = 'dispatched') <> (dispatched_at IS NOT NULL) UNION ALL SELECT 'email_outbox' AS src, id, tenant_id, status, attempts, dispatched_at, created_at, COALESCE(last_error,'') AS last_error FROM email_outbox WHERE (status = 'dispatched') <> (dispatched_at IS NOT NULL)`,
	},
	{
		Name:   "transport_outbox_pending_past_dlq_policy",
		Domain: "transport",
		Why:    "a pending outbox row at max attempts must not sit past-due for a day — the DLQ writer should have claimed it",
		SQL:    `SELECT 'webhook_outbox' AS src, id, tenant_id, event_type AS kind, attempts, next_attempt_at, created_at, COALESCE(last_error,'') AS last_error FROM webhook_outbox WHERE status = 'pending' AND attempts >= 15 AND next_attempt_at < now() - interval '24 hours' UNION ALL SELECT 'email_outbox' AS src, id, tenant_id, email_type AS kind, attempts, next_attempt_at, created_at, COALESCE(last_error,'') AS last_error FROM email_outbox WHERE status = 'pending' AND attempts >= 15 AND next_attempt_at < now() - interval '24 hours'`,
	},
	{
		Name:   "payment_methods_detached_but_still_default",
		Domain: "transport",
		Why:    "a detached card may never remain the default — that is the charge-a-removed-card hazard",
		SQL:    `SELECT id, tenant_id, livemode, customer_id, stripe_payment_method_id, card_last4, is_default, detached_at, updated_at FROM payment_methods WHERE detached_at IS NOT NULL AND is_default = true`,
	},
	{
		Name:   "cost_dashboard_token_plaintext_at_rest",
		Domain: "customers",
		Why:    "customers.cost_dashboard_token_hash must hold a SHA-256 blind index, never the credential itself; a vlx_pcd_-prefixed value means some writer stored the raw public-dashboard token and a DB read yields a replayable URL (migration 0172)",
		SQL:    `SELECT id, tenant_id, livemode, external_id, cost_dashboard_token_hash, updated_at FROM customers WHERE cost_dashboard_token_hash IS NOT NULL AND (cost_dashboard_token_hash LIKE 'vlx_%' OR length(cost_dashboard_token_hash) <> 64)`,
	},
	{
		Name:   "cn_deferred_draft_stale",
		Domain: "credit-notes",
		Why:    "an issue_pending draft waiting on an in-flight charge should resolve in minutes (ADR-059); one waiting days means the charge wedged and the relief is silently unissued",
		SQL: `SELECT id, tenant_id, credit_note_number, invoice_id, status, created_at
		FROM credit_notes
		WHERE issue_pending = true
		  AND status = 'draft'
		  AND created_at < now() - interval '7 days'`,
	},
	{
		Name:   "invoice_tax_calculated_never_committed",
		Domain: "invoices",
		Why:    "a finalized stripe_tax invoice carrying tax with a calculation but no committed transaction never reported that tax — and the ~24h calculation TTL means it can no longer be committed automatically",
		SQL: `SELECT id, tenant_id, invoice_number, status, tax_amount_cents, tax_calculation_id, issued_at
		FROM invoices
		WHERE stripe_invoice_id IS NULL
		  AND id NOT LIKE 'vlx_inv_safe%'
		  AND is_simulated = false
		  AND tax_provider = 'stripe_tax'
		  AND tax_status = 'ok'
		  AND tax_amount_cents > 0
		  AND COALESCE(tax_calculation_id, '') <> ''
		  AND COALESCE(tax_transaction_id, '') = ''
		  AND status IN ('finalized','paid','voided','uncollectible')
		  AND updated_at < now() - interval '48 hours'`,
	},
}
