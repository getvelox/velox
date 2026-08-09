package billing_test

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/subscription"
)

// P4 (ADR-070): price-change lifecycle against real Postgres. The two
// decisions under test: (a) overrides are keyed by rule_key and survive
// version publishes; (b) the version (and override) in force at the
// period OPEN prices the whole period — cycle close, threshold fire,
// and preview agree across a mid-period publish.

// seededRule resolves the fixture's seeded "calls_flat" rule version.
func (f *thresholdFixture) seededRule(t *testing.T, ctx context.Context) domain.RatingRuleVersion {
	t.Helper()
	r, err := f.pricingSvc.GetRuleByKeyAsOf(ctx, f.tenantID, "calls_flat", time.Now().UTC())
	if err != nil {
		t.Fatalf("resolve seeded rule: %v", err)
	}
	return r
}

// backdateRuleVersion moves a rating-rule version's created_at before
// the fixture's period open, modelling a catalog that existed when the
// period started (the fixture creates everything "now", 72h into the
// period).
func backdateRuleVersion(t *testing.T, f *thresholdFixture, ctx context.Context, id string, to time.Time) {
	t.Helper()
	tx, err := f.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin bypass: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.Exec(`UPDATE rating_rule_versions SET created_at = $1 WHERE id = $2`, to, id); err != nil {
		t.Fatalf("backdate rule version: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit backdate: %v", err)
	}
}

// createBackdatedOverride sets a negotiated flat price for the fixture
// customer on the fixture rule, effective BEFORE the period opened (an
// override created mid-period prices from the next period by design —
// these tests model a deal already in place).
func createBackdatedOverride(t *testing.T, f *thresholdFixture, ctx context.Context, ruleVersionID string, flatCents int64) domain.CustomerPriceOverride {
	t.Helper()
	o, err := f.pricingSvc.CreateOverride(ctx, f.tenantID, pricing.CreateOverrideInput{
		CustomerID:          f.customerID,
		RatingRuleVersionID: ruleVersionID,
		Mode:                domain.PricingFlat,
		FlatAmountCents:     decimal.NewFromInt(flatCents),
		Reason:              "negotiated",
	})
	if err != nil {
		t.Fatalf("create override: %v", err)
	}
	tx, err := f.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin bypass: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.Exec(`UPDATE customer_price_overrides SET created_at = $1 WHERE id = $2`,
		f.cycleStart.Add(-time.Hour), o.ID); err != nil {
		t.Fatalf("backdate override: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit backdate: %v", err)
	}
	return o
}

// closeCycle rewinds the sub's period end to the past and runs the
// cycle scan (the standard close-drive pattern in this suite).
func closeCycle(t *testing.T, f *thresholdFixture, ctx context.Context) {
	t.Helper()
	pastEnd := time.Now().UTC().Add(-1 * time.Hour).Truncate(time.Microsecond)
	tx, err := f.db.BeginTx(ctx, postgres.TxTenant, f.tenantID)
	if err != nil {
		t.Fatalf("begin rewind tx: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(ctx, `
		UPDATE subscriptions SET current_billing_period_end = $1, next_billing_at = $1 WHERE id = $2
	`, pastEnd, f.subID); err != nil {
		t.Fatalf("rewind period: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit rewind: %v", err)
	}
	generated, failures := f.engine.RunCycleForTenant(ctx, f.tenantID, 50)
	if len(failures) > 0 {
		t.Fatalf("cycle failures: %v", failures)
	}
	if generated != 1 {
		t.Fatalf("cycle generated = %d, want 1", generated)
	}
}

func usageLineTotal(t *testing.T, f *thresholdFixture, ctx context.Context, invoiceID string) (int64, []domain.InvoiceLineItem) {
	t.Helper()
	lines, err := f.invStore.ListLineItems(ctx, f.tenantID, invoiceID)
	if err != nil {
		t.Fatalf("list line items: %v", err)
	}
	var total int64
	var usageLines []domain.InvoiceLineItem
	for _, l := range lines {
		if l.LineType == domain.LineTypeUsage {
			total += l.AmountCents
			usageLines = append(usageLines, l)
			// Every usage line must carry its meter's unit (migration
			// 0173): it's what lets the invoice surfaces render token
			// rates per-1M. Asserted here — in the helper every cycle
			// test funnels through — so dropping the stamp at ANY of the
			// engine's line-construction sites fails real-Postgres tests,
			// not just the one that happened to look.
			if l.MeterUnit == "" {
				t.Fatalf("usage line %q has no MeterUnit — the 0173 stamp was dropped at its construction site", l.Description)
			}
		}
	}
	return total, usageLines
}

// TestPriceChange_OverrideSurvivesPublish_SingleRule is the audit-High
// detach regression: a negotiated override must keep applying after the
// rule gets a new version. Pre-ADR-070 the close resolved the LATEST
// version and looked the override up by that version's id — publishing
// v2 made the v1-keyed override unfindable and the customer silently
// reverted to list price for the whole period.
//
// Mutation-verify: key the override lookup by version id (pass rule.ID
// instead of rule.RuleKey in resolveRatedRule) — this test fails.
func TestPriceChange_OverrideSurvivesPublish_SingleRule(t *testing.T) {
	f := newThresholdFixture(t, "P4 Detach SingleRule")
	ctx := postgres.WithLivemode(context.Background(), false)

	// Catalog + deal in place before the period opened.
	v1 := f.seededRule(t, ctx)
	backdateRuleVersion(t, f, ctx, v1.ID, f.cycleStart.Add(-2*time.Hour))
	createBackdatedOverride(t, f, ctx, v1.ID, 5) // negotiated 5c/call (list 1c)

	// Mid-period publish: v2 at 2c/call.
	v2, err := f.pricingSvc.CreateRatingRule(ctx, f.tenantID, pricing.CreateRatingRuleInput{
		RuleKey: "calls_flat", Name: "Calls Flat v2",
		Mode: domain.PricingFlat, Currency: "USD", FlatAmountCents: decimal.NewFromInt(2),
	})
	if err != nil {
		t.Fatalf("publish v2: %v", err)
	}
	if v2.Version != 2 {
		t.Fatalf("v2 version: got %d, want 2", v2.Version)
	}

	f.ingestUsage(t, ctx, 100, 10) // 1000 calls
	closeCycle(t, f, ctx)

	invoices := f.listInvoices(t, ctx)
	if len(invoices) != 1 {
		t.Fatalf("invoices: got %d, want 1", len(invoices))
	}
	total, usageLines := usageLineTotal(t, f, ctx, invoices[0].ID)
	// 1000 calls × 5c override = 5000c. List v1 would be 1000c; v2 would
	// be 2000c — either means the override detached or repriced.
	if total != 5000 {
		t.Errorf("usage total: got %d cents, want 5000 (override survives the publish; 1000=v1 list, 2000=v2 list)", total)
	}
	// Provenance: the line records the version in force at period open.
	if len(usageLines) == 1 && usageLines[0].RatingRuleVersionID != v1.ID {
		t.Errorf("line rule version: got %s, want %s (v1 — the period opened on v1)", usageLines[0].RatingRuleVersionID, v1.ID)
	}
	// ADR-054 re-examination — the override trap: the stamped nominal rate is
	// the OVERRIDE-applied rate (5c/call), NOT the list price at the persisted
	// version id (v1 = 1c). The override preserves the version id, so a naive
	// derive-on-read from rating_rule_versions.flat_amount_cents would
	// misdisplay list price on this negotiated line — which is exactly why the
	// rate is stamped at build from the resolved (override-applied) rule.
	// Mutation-verify: stamp rule.ID's list rate instead of rule.FlatAmountCents
	// and this fails with got 1.
	if len(usageLines) == 1 {
		nom := usageLines[0].NominalUnitAmountDecimal
		if nom == nil {
			t.Errorf("nominal rate: got nil, want 5 (a flat line must stamp its configured rate)")
		} else if !nom.Equal(decimal.NewFromInt(5)) {
			t.Errorf("nominal rate: got %s, want 5 (the negotiated override, not v1 list 1c)", nom.String())
		}
	}
}

// TestPriceChange_OverrideSurvivesPublish_MultiDim: same regression on
// the multi-dim path (meter_pricing_rules bindings), which pre-ADR-070
// never resolved by key at all — it priced the pinned binding version
// and looked the override up by that id.
func TestPriceChange_OverrideSurvivesPublish_MultiDim(t *testing.T) {
	f := newThresholdFixture(t, "P4 Detach MultiDim")
	ctx := postgres.WithLivemode(context.Background(), false)

	v1 := f.seededRule(t, ctx)
	backdateRuleVersion(t, f, ctx, v1.ID, f.cycleStart.Add(-2*time.Hour))
	// Catch-all pricing rule flips the meter onto the multi-dim path.
	if _, err := f.pricingSvc.UpsertMeterPricingRule(ctx, f.tenantID, pricing.UpsertMeterPricingRuleInput{
		MeterID:             f.meterID,
		RatingRuleVersionID: v1.ID,
		AggregationMode:     domain.AggSum,
	}); err != nil {
		t.Fatalf("upsert meter pricing rule: %v", err)
	}
	createBackdatedOverride(t, f, ctx, v1.ID, 5)

	if _, err := f.pricingSvc.CreateRatingRule(ctx, f.tenantID, pricing.CreateRatingRuleInput{
		RuleKey: "calls_flat", Name: "Calls Flat v2",
		Mode: domain.PricingFlat, Currency: "USD", FlatAmountCents: decimal.NewFromInt(2),
	}); err != nil {
		t.Fatalf("publish v2: %v", err)
	}

	f.ingestUsage(t, ctx, 100, 10)
	closeCycle(t, f, ctx)

	invoices := f.listInvoices(t, ctx)
	if len(invoices) != 1 {
		t.Fatalf("invoices: got %d, want 1", len(invoices))
	}
	total, _ := usageLineTotal(t, f, ctx, invoices[0].ID)
	if total != 5000 {
		t.Errorf("usage total (multi-dim): got %d cents, want 5000 (override survives the publish)", total)
	}
}

// TestPriceChange_FireThenPublishThenClose: one period, one price. The
// threshold fire bills the pre-fire window at the period-open version;
// a publish lands mid-period; the cycle close bills the residual at the
// SAME version — never v2 (pre-ADR-070 the close resolved latest-by-key
// while the fire priced the pinned version: two prices in one period,
// and the graduated-tier restart math silently assumed one).
//
// Mutation-verify: resolve with asOf=now instead of periodStart — the
// close prices at v2 and this test fails.
func TestPriceChange_FireThenPublishThenClose(t *testing.T) {
	f := newThresholdFixture(t, "P4 FirePublishClose")
	ctx := postgres.WithLivemode(context.Background(), false)

	v1 := f.seededRule(t, ctx)
	backdateRuleVersion(t, f, ctx, v1.ID, f.cycleStart.Add(-2*time.Hour))

	// Threshold: fire once the running subtotal crosses $5, keep the
	// cycle (reset=false) so the close bills the residual window of the
	// SAME period.
	noReset := false
	if _, err := f.subSvc.SetBillingThresholds(ctx, f.tenantID, f.subID, subscription.BillingThresholdsInput{
		AmountGTE: 500, ResetBillingCycle: &noReset,
	}); err != nil {
		t.Fatalf("set thresholds: %v", err)
	}

	f.ingestUsage(t, ctx, 100, 10) // 1000 calls × 1c = 1000c ≥ 500c
	fired, errs := f.engine.ScanThresholds(ctx, 50)
	if len(errs) > 0 {
		t.Fatalf("scan errors: %v", errs)
	}
	if fired != 1 {
		t.Fatalf("threshold fired = %d, want 1", fired)
	}

	// Mid-period publish at 5c/call.
	if _, err := f.pricingSvc.CreateRatingRule(ctx, f.tenantID, pricing.CreateRatingRuleInput{
		RuleKey: "calls_flat", Name: "Calls Flat v2",
		Mode: domain.PricingFlat, Currency: "USD", FlatAmountCents: decimal.NewFromInt(5),
	}); err != nil {
		t.Fatalf("publish v2: %v", err)
	}

	// Residual usage AFTER the fire watermark (the fire invoice's line
	// BillingPeriodEnd), inside a period end just past it — the close's
	// watermark protocol bills exactly [fireUpper, periodEnd).
	fireInvoices := f.listInvoices(t, ctx)
	if len(fireInvoices) != 1 {
		t.Fatalf("fire invoices: got %d, want 1", len(fireInvoices))
	}
	fireLines, err := f.invStore.ListLineItems(ctx, f.tenantID, fireInvoices[0].ID)
	if err != nil || len(fireLines) == 0 || fireLines[0].BillingPeriodEnd == nil {
		t.Fatalf("read fire watermark: err=%v lines=%d", err, len(fireLines))
	}
	fireUpper := fireLines[0].BillingPeriodEnd.UTC()
	residualAt := fireUpper.Add(time.Second).Truncate(time.Microsecond)
	newEnd := fireUpper.Add(2 * time.Second).Truncate(time.Microsecond)

	tx, err := f.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin bypass: %v", err)
	}
	// The 0021 trigger overwrites livemode from the session GUC — a
	// bypass tx must SET LOCAL or the row lands in live mode, invisible
	// to the test-mode aggregation.
	if _, err := tx.Exec(`SET LOCAL app.livemode = 'off'`); err != nil {
		t.Fatalf("set local livemode: %v", err)
	}
	if _, err := tx.Exec(`
		INSERT INTO usage_events (id, tenant_id, customer_id, meter_id, quantity, timestamp, livemode)
		VALUES ($1, $2, $3, $4, 500, $5, false)
	`, postgres.NewID("vlx_ue"), f.tenantID, f.customerID, f.meterID, residualAt); err != nil {
		t.Fatalf("seed residual usage: %v", err)
	}
	if _, err := tx.Exec(`
		UPDATE subscriptions SET current_billing_period_end = $1, next_billing_at = $1 WHERE id = $2
	`, newEnd, f.subID); err != nil {
		t.Fatalf("shrink period to just past the fire: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit residual seed: %v", err)
	}

	// The shrunken period end sits ~2s past the fire watermark — wait
	// for the boundary to elapse so the close scan sees the sub as due.
	if wait := time.Until(newEnd.Add(500 * time.Millisecond)); wait > 0 {
		time.Sleep(wait)
	}
	generated, failures := f.engine.RunCycleForTenant(ctx, f.tenantID, 50)
	if len(failures) > 0 {
		t.Fatalf("cycle failures: %v", failures)
	}
	if generated != 1 {
		t.Fatalf("cycle generated = %d, want 1", generated)
	}

	invoices := f.listInvoices(t, ctx)
	if len(invoices) != 2 {
		t.Fatalf("invoices: got %d, want 2 (fire + close)", len(invoices))
	}
	var totals []int64
	for _, inv := range invoices {
		tot, _ := usageLineTotal(t, f, ctx, inv.ID)
		totals = append(totals, tot)
	}
	// Fire billed 1000 calls at v1 (1000c); close bills the residual 500
	// calls at the SAME v1 price (500c) — 2500c would mean v2 leaked in.
	seen := map[int64]bool{totals[0]: true, totals[1]: true}
	if !seen[1000] || !seen[500] {
		t.Errorf("fire+close usage totals: got %v, want {1000, 500} — the whole period prices at the period-open version", totals)
	}
}

// TestPriceChange_PreviewMatchesCloseAcrossPublish CI-locks the parity
// claim in preview.go's doc comment ("the cycle scan calls the same
// path, so a multi-dim tenant's preview matches what its actual invoice
// will be") that was false pre-ADR-070: previewMeter
// priced the meter-pinned version while the close priced latest-by-key,
// so preview != invoice after any publish.
func TestPriceChange_PreviewMatchesCloseAcrossPublish(t *testing.T) {
	f := newThresholdFixture(t, "P4 PreviewParity")
	ctx := postgres.WithLivemode(context.Background(), false)

	v1 := f.seededRule(t, ctx)
	backdateRuleVersion(t, f, ctx, v1.ID, f.cycleStart.Add(-2*time.Hour))
	createBackdatedOverride(t, f, ctx, v1.ID, 3)

	if _, err := f.pricingSvc.CreateRatingRule(ctx, f.tenantID, pricing.CreateRatingRuleInput{
		RuleKey: "calls_flat", Name: "Calls Flat v2",
		Mode: domain.PricingFlat, Currency: "USD", FlatAmountCents: decimal.NewFromInt(7),
	}); err != nil {
		t.Fatalf("publish v2: %v", err)
	}

	f.ingestUsage(t, ctx, 100, 10) // 1000 calls

	sub, err := f.subStore.Get(ctx, f.tenantID, f.subID)
	if err != nil {
		t.Fatalf("get sub: %v", err)
	}
	preview, err := f.engine.Preview(ctx, sub)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if len(preview.Totals) != 1 {
		t.Fatalf("preview totals: got %d entries (%v), want 1 — overridden lines must carry the rule's currency", len(preview.Totals), preview.Totals)
	}
	previewTotal := preview.Totals[0].AmountCents

	closeCycle(t, f, ctx)
	invoices := f.listInvoices(t, ctx)
	if len(invoices) != 1 {
		t.Fatalf("invoices: got %d, want 1", len(invoices))
	}
	closeTotal, _ := usageLineTotal(t, f, ctx, invoices[0].ID)

	if previewTotal != closeTotal {
		t.Errorf("preview total %d != close usage total %d (must agree across a publish)", previewTotal, closeTotal)
	}
	if closeTotal != 3000 {
		t.Errorf("close total: got %d, want 3000 (1000 calls × 3c override)", closeTotal)
	}
}

// TestThresholdFire_ZeroBaseAllOverride_ResolvesCurrency: the fixture
// sub has NO base fee, and its only usage rule is overridden. The old
// replace-wholesale ToRatingRule fabricated a rule with Currency == ""
// — every preview line carried an empty currency, invoice-currency
// resolution found no non-empty line, and the fire hard-failed "no
// invoice currency resolved" for exactly the negotiated customers the
// spend-cap wedge targets.
//
// Mutation-verify: revert ApplyTo to fabricate-from-scratch — this
// test fails.
func TestThresholdFire_ZeroBaseAllOverride_ResolvesCurrency(t *testing.T) {
	f := newThresholdFixture(t, "P4 ZeroBaseOverrideFire")
	ctx := postgres.WithLivemode(context.Background(), false)

	v1 := f.seededRule(t, ctx)
	backdateRuleVersion(t, f, ctx, v1.ID, f.cycleStart.Add(-2*time.Hour))
	createBackdatedOverride(t, f, ctx, v1.ID, 5)

	if _, err := f.subSvc.SetBillingThresholds(ctx, f.tenantID, f.subID, subscription.BillingThresholdsInput{
		AmountGTE: 500,
	}); err != nil {
		t.Fatalf("set thresholds: %v", err)
	}

	f.ingestUsage(t, ctx, 100, 10) // 1000 calls × 5c override = 5000c ≥ 500c
	fired, errs := f.engine.ScanThresholds(ctx, 50)
	if len(errs) > 0 {
		t.Fatalf("scan errors: %v (pre-fix: 'no invoice currency resolved')", errs)
	}
	if fired != 1 {
		t.Fatalf("threshold fired = %d, want 1", fired)
	}

	invoices := f.listInvoices(t, ctx)
	if len(invoices) != 1 {
		t.Fatalf("invoices: got %d, want 1", len(invoices))
	}
	if invoices[0].Currency != "USD" {
		t.Errorf("fire invoice currency: got %q, want USD (patched override keeps the rule's currency)", invoices[0].Currency)
	}
	total, _ := usageLineTotal(t, f, ctx, invoices[0].ID)
	if total != 5000 {
		t.Errorf("fire usage total: got %d, want 5000 (override price)", total)
	}
}
