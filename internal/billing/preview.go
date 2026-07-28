package billing

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
)

// PreviewLine is one line of an invoice preview. Mirrors the line shape the
// cycle scan persists onto invoice_line_items, with two differences:
//
//   - Quantity is a decimal string on the wire (NUMERIC(38,12) precision
//     preserved end-to-end — fractional AI-usage primitives like GPU-hours
//     and cached-token ratios round-trip without precision loss).
//   - Multi-rule meters emit one line per (meter, rule) pair with
//     DimensionMatch echoed from the meter pricing rule (the canonical
//     pricing identity). Single-rule meters keep the one-line-per-meter
//     shape with no DimensionMatch.
//
// See docs/design-create-preview.md.
type PreviewLine struct {
	LineType            string          `json:"line_type"`
	Description         string          `json:"description"`
	MeterID             string          `json:"meter_id,omitempty"`
	RatingRuleVersionID string          `json:"rating_rule_version_id,omitempty"`
	RuleKey             string          `json:"rule_key,omitempty"`
	DimensionMatch      map[string]any  `json:"dimension_match,omitempty"`
	Currency            string          `json:"currency"`
	Quantity            decimal.Decimal `json:"quantity"`
	UnitAmountCents     int64           `json:"unit_amount_cents"`
	AmountCents         int64           `json:"amount_cents"`
	PricingMode         string          `json:"pricing_mode,omitempty"`
	// AggregationMode is the rule BUCKET's aggregation (sum/count/max/
	// last_during_period/last_ever) — per pricing rule, not per meter, since
	// one meter can carry mixed modes. Internal-only (json:"-"): the
	// threshold scan classifies non-additive buckets with it (ADR-066 §4);
	// the public create_preview wire is unchanged.
	AggregationMode domain.AggregationMode `json:"-"`
	// PlanID + BaseBillTiming identify the base_fee line's plan, internal-
	// only (json:"-"): the threshold fire consumes them to skip prepaid
	// in_advance bases and prorate reset-mode bases PER LINE — replacing a
	// positional parallel-array contract that silently mis-attributed
	// plans if the emission order ever changed (2026-07-10 design review).
	PlanID         string            `json:"-"`
	BaseBillTiming domain.BillTiming `json:"-"`
	// NominalUnitAmountDecimal carries the flat-mode configured per-unit rate
	// (decimal cents) from the OVERRIDE-APPLIED rule, internal-only (json:"-")
	// so an overage invoice line built from this preview shows the clean
	// nominal rate like a cycle usage line (ADR-054 re-examination). Nil for
	// graduated/package/base_fee → the invoice line's effective-rate fallback.
	NominalUnitAmountDecimal *decimal.Decimal `json:"-"`
}

// PreviewTotal is one currency's roll-up across the preview's lines. We
// always emit a list (one entry per distinct currency) even when there's
// only one currency — consistent shape lets clients read totals[0]
// without branching on cardinality, and lines up with the customer-usage
// endpoint's totals shape so a single TS type covers both surfaces.
type PreviewTotal struct {
	Currency    string `json:"currency"`
	AmountCents int64  `json:"amount_cents"`
}

// PreviewResult is the response shape for both the in-app
// GET /v1/billing/preview/{subscription_id} debug route and the public
// POST /v1/invoices/create_preview surface. PlanName is a joined list of
// the subscription's item plans (comma-separated) — the previous
// single-plan shape can't represent a multi-item sub.
type PreviewResult struct {
	CustomerID         string         `json:"customer_id"`
	SubscriptionID     string         `json:"subscription_id"`
	PlanName           string         `json:"plan_name"`
	BillingPeriodStart time.Time      `json:"billing_period_start"`
	BillingPeriodEnd   time.Time      `json:"billing_period_end"`
	Lines              []PreviewLine  `json:"lines"`
	Totals             []PreviewTotal `json:"totals"`
	Warnings           []string       `json:"warnings"`
	GeneratedAt        time.Time      `json:"generated_at"`
}

// Preview generates a dry-run invoice for a subscription without persisting
// anything. Walks every item, resolves its plan, and emits a base_fee line
// per item plus per-(meter, rule) usage lines via the priority+claim
// LATERAL JOIN — same code path the cycle scan uses, so preview math ==
// invoice math.
func (e *Engine) Preview(ctx context.Context, sub domain.Subscription) (PreviewResult, error) {
	if sub.CurrentBillingPeriodStart == nil || sub.CurrentBillingPeriodEnd == nil {
		return PreviewResult{}, fmt.Errorf("subscription has no billing period set")
	}
	if len(sub.Items) == 0 {
		return PreviewResult{}, fmt.Errorf("subscription has no items")
	}
	periodStart := *sub.CurrentBillingPeriodStart
	periodEnd := *sub.CurrentBillingPeriodEnd
	return e.previewWithWindow(ctx, sub, periodStart, periodEnd)
}

// previewWithWindow is the explicit-period variant. Used by the
// create_preview service when the caller passes a custom window; the
// wall-clock Preview entry above defers to the subscription's current
// cycle. Both paths converge here so the per-line composition stays in
// one place.
func (e *Engine) previewWithWindow(ctx context.Context, sub domain.Subscription, periodStart, periodEnd time.Time) (PreviewResult, error) {
	plans := make(map[string]domain.Plan, len(sub.Items))
	planNames := make([]string, 0, len(sub.Items))
	var allMeterIDs []string
	seenMeter := make(map[string]struct{})
	for _, it := range sub.Items {
		if _, ok := plans[it.PlanID]; ok {
			continue
		}
		pl, err := e.pricing.GetPlan(ctx, sub.TenantID, it.PlanID)
		if err != nil {
			return PreviewResult{}, fmt.Errorf("get plan %s: %w", it.PlanID, err)
		}
		plans[it.PlanID] = pl
		planNames = append(planNames, pl.Name)
		for _, mid := range pl.MeterIDs {
			if _, ok := seenMeter[mid]; ok {
				continue
			}
			seenMeter[mid] = struct{}{}
			allMeterIDs = append(allMeterIDs, mid)
		}
	}

	// ADR-031: when ANY item is in_advance, the cycle invoice's
	// HEADER period shifts to the upcoming period (engine.billOnePeriod
	// does the same). Preview must mirror this so the operator's
	// "what does the next invoice look like" answer matches what
	// actually lands. Totals are unchanged either way — only the
	// period label differs.
	invoiceStart, invoiceEnd := periodStart, periodEnd
	for _, it := range sub.Items {
		if plans[it.PlanID].BaseBillTiming == domain.BillInAdvance {
			invoiceStart = periodEnd
			invoiceEnd = advanceBillingPeriod(periodEnd, plans[it.PlanID].BillingInterval, e.tenantLocation(ctx, sub.TenantID), sub.BillingAnchorDay)
			break
		}
	}

	result := PreviewResult{
		CustomerID:         sub.CustomerID,
		SubscriptionID:     sub.ID,
		PlanName:           strings.Join(planNames, ", "),
		BillingPeriodStart: invoiceStart,
		BillingPeriodEnd:   invoiceEnd,
		Lines:              []PreviewLine{},
		Warnings:           []string{},
		GeneratedAt:        e.clock.Now(ctx),
	}

	// Base-fee lines from each subscription item's plan. Quantity here is
	// the item's seat / count (always integer); we still expose it as a
	// decimal for shape uniformity.
	//
	// in_advance items bill the FULL upcoming base (no proration —
	// preview at cycle close, partial-period creation already settled
	// by BillOnCreate). in_arrears items bill the period being previewed.
	for _, it := range sub.Items {
		plan := plans[it.PlanID]
		if plan.BaseAmountCents <= 0 {
			continue
		}
		baseFee := plan.BaseAmountCents * it.Quantity
		desc := fmt.Sprintf("%s - base fee (qty %d)", plan.Name, it.Quantity)
		if plan.BaseBillTiming == domain.BillInAdvance {
			desc = fmt.Sprintf("%s - base fee (qty %d, in advance for upcoming period)", plan.Name, it.Quantity)
		}
		result.Lines = append(result.Lines, PreviewLine{
			LineType:        "base_fee",
			Description:     desc,
			Currency:        plan.Currency,
			Quantity:        decimal.NewFromInt(it.Quantity),
			UnitAmountCents: plan.BaseAmountCents,
			AmountCents:     baseFee,
			PlanID:          plan.ID,
			BaseBillTiming:  plan.BaseBillTiming,
		})
	}

	// Usage lines per (meter, rule). The priority+claim LATERAL JOIN inside
	// AggregateByPricingRules is the canonical pricing identity — the
	// cycle scan calls the same path, so a multi-dim tenant's preview
	// matches what its actual invoice will be.
	//
	// A previewMeter ERROR aborts the whole preview (ADR-070,
	// no-silent-fallbacks). Config-shaped soft cases (no rule binding,
	// rule not found, rating failure) already surface as warnings from
	// INSIDE previewMeter; what reaches this err is infrastructure
	// failure (store errors, transient resolution failures), and a
	// partial preview built from those is a lie — worse, the threshold
	// scan PERSISTS these lines as a fire invoice, and a silently
	// dropped meter's pre-fire usage would then be billed by nobody:
	// the cycle close clamps every meter's window to the fire
	// watermark, deferral only exists for non-additive buckets. The
	// pre-ADR-070 warning-degradation here defeated previewMeter's own
	// fail-loud contract for exactly the money path it matters on.
	usageLineCount := 0
	for _, meterID := range allMeterIDs {
		meterLines, warnings, err := e.previewMeter(ctx, sub.TenantID, sub.CustomerID, meterID, periodStart, periodEnd)
		if err != nil {
			return PreviewResult{}, fmt.Errorf("preview meter %q: %w", meterID, err)
		}
		result.Lines = append(result.Lines, meterLines...)
		result.Warnings = append(result.Warnings, warnings...)
		usageLineCount += len(meterLines)
	}

	// Estimate-scope warnings (ADR-045): create_preview is a full-period
	// estimate that does NOT replicate the two overlays the cycle scan
	// applies — usage-cap scaling and mid-period segment proration. Rather
	// than silently overstating a capped sub or misstating one that changed
	// mid-period, surface the divergence on the existing warnings channel so
	// the estimate is honest about its own scope. (Replicating the overlays
	// is the deferred ADR-045 follow-up; this is honesty now, precision when
	// a design partner turns the features on.)
	//
	// Cap: only when there's billable usage the cap could scale — a
	// configured cap on a zero-usage period can't bind, so no warning.
	if usageLineCount > 0 && sub.UsageCapUnits != nil && *sub.UsageCapUnits > 0 && sub.OverageAction == "block" {
		result.Warnings = append(result.Warnings, fmt.Sprintf(
			"Estimate excludes the subscription's usage cap (%d units per period); if billable usage exceeds the cap, the actual invoice will be lower.",
			*sub.UsageCapUnits))
	}

	// Segment proration: a mid-LIFE change within [periodStart, periodEnd]
	// makes the cycle bill base fees and usage per segment (each at its own
	// rate × duration), which the full-period estimate flattens. Only plan
	// swaps, quantity changes, and item removals qualify — the 'add' rows
	// include every item's INITIAL creation (emitted by the subscription_items
	// insert trigger, migration 0029), which is not a mid-period change and
	// would false-positive the warning on every sub in its first period. Since
	// the ADR-012 amendment (2026-07-26) the interval writers agree:
	// a period-start-day 'add' opens its interval at periodStart (write-time
	// day-grade, ADR-101), so a first period bills the full base and this filter matches the money. Same
	// [periodStart, periodEnd] window the cycle's segment-aware billing keys
	// off. e.subs is always wired in production and the create_preview path;
	// the nil-guard only spares narrow subs-less unit tests (the warning is
	// advisory, never a number).
	if e.subs != nil {
		itemChanges, err := e.subs.ListItemChangesInPeriod(ctx, sub.TenantID, sub.ID, periodStart, periodEnd)
		if err != nil {
			return PreviewResult{}, fmt.Errorf("preview: list item changes: %w", err)
		}
		midLifeChange := false
		for _, c := range itemChanges {
			if c.ChangeType != "add" { // plan | quantity | remove
				midLifeChange = true
				break
			}
		}
		if midLifeChange {
			result.Warnings = append(result.Warnings,
				"Estimate excludes mid-period proration; the plan, quantity, or item set changed during this period, so the actual invoice may rate base fees and usage per segment.")
		}
	}

	result.Totals = computePreviewTotals(result.Lines)
	return result, nil
}

// previewMeter rates one meter's events for the customer over [from, to)
// and emits one PreviewLine per rule bucket. Mirrors usage.rateMeter from
// internal/usage/customer_usage.go — the cycle scan calls the same
// AggregateByPricingRules + ComputeAmountCents path, so each line
// produced here matches the invoice_line_item the cycle scan would
// persist.
func (e *Engine) previewMeter(ctx context.Context, tenantID, customerID, meterID string, from, to time.Time) ([]PreviewLine, []string, error) {
	meter, err := e.pricing.GetMeter(ctx, tenantID, meterID)
	if err != nil {
		return nil, nil, fmt.Errorf("get meter: %w", err)
	}

	defaultMode := mapMeterAggregation(meter.Aggregation)
	aggs, err := e.usage.AggregateByPricingRules(ctx, tenantID, customerID, meterID, defaultMode, from, to)
	if err != nil {
		return nil, nil, fmt.Errorf("aggregate by pricing rules: %w", err)
	}

	rules, err := e.pricing.ListMeterPricingRulesByMeter(ctx, tenantID, meterID)
	if err != nil {
		return nil, nil, fmt.Errorf("list pricing rules: %w", err)
	}
	rulesByID := make(map[string]domain.MeterPricingRule, len(rules))
	for _, rule := range rules {
		rulesByID[rule.ID] = rule
	}

	var lines []PreviewLine
	var warnings []string

	for _, agg := range aggs {
		if agg.Quantity.IsZero() {
			continue
		}

		ratingRuleID := agg.RatingRuleVersionID
		if ratingRuleID == "" {
			ratingRuleID = meter.RatingRuleVersionID
		}
		if ratingRuleID == "" {
			warnings = append(warnings, fmt.Sprintf("meter %q has events with no rating rule binding — skipped", meter.Key))
			continue
		}

		// Same resolution rule as the cycle scan and cancel finalize
		// (resolveRatedRule; ADR-070 pin-at-period-start): version and
		// override in force at the window open — `from` IS the period
		// start for both callers (operator preview and the threshold
		// scan, which persists these lines as a real invoice). This is
		// what makes preview == invoice hold across a mid-period
		// publish, and a threshold fire agree with the later close.
		ratingRule, err := e.resolveRatedRule(ctx, tenantID, customerID, ratingRuleID, from)
		if err != nil {
			if errors.Is(err, errs.ErrNotFound) {
				warnings = append(warnings, fmt.Sprintf("meter %q rule %q: rating rule not found", meter.Key, ratingRuleID))
				continue
			}
			// Transient failure: abort rather than degrade — a
			// threshold fire built from this preview would silently
			// misprice (e.g. list price for a negotiated customer).
			return nil, nil, fmt.Errorf("resolve pricing for meter %q rule %q: %w", meter.Key, ratingRuleID, err)
		}

		amount, err := domain.ComputeAmountCents(ratingRule, agg.Quantity)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("meter %q rule %q: rating failed: %s", meter.Key, ratingRule.RuleKey, err.Error()))
			continue
		}

		// Unit amount is amount / quantity, rounded to nearest cent. The
		// IsZero short-circuit above guarantees the divisor is non-zero.
		unitAmount := decimal.NewFromInt(amount).Div(agg.Quantity).RoundBank(0).IntPart()

		var dimMatch map[string]any
		if agg.RuleID != "" {
			if rule, ok := rulesByID[agg.RuleID]; ok && len(rule.DimensionMatch) > 0 {
				dimMatch = rule.DimensionMatch
			}
		}

		// Use the exact description the cycle scan persists (meter + dimension
		// match, e.g. "Tokens (claude-3.5-sonnet · input)") so a preview line
		// reads identically to the invoice_line_item it predicts. The quantity
		// lives in the Quantity field, not the description.
		desc := usageLineDescription(meter, rulesByID[agg.RuleID])

		lines = append(lines, PreviewLine{
			LineType:                 "usage",
			Description:              desc,
			MeterID:                  meterID,
			RatingRuleVersionID:      ratingRule.ID,
			RuleKey:                  ratingRule.RuleKey,
			DimensionMatch:           dimMatch,
			Currency:                 ratingRule.Currency,
			Quantity:                 agg.Quantity,
			UnitAmountCents:          unitAmount,
			AmountCents:              amount,
			PricingMode:              string(ratingRule.Mode),
			AggregationMode:          agg.AggregationMode,
			NominalUnitAmountDecimal: nominalRate(ratingRule),
		})
	}

	return lines, warnings, nil
}

// computePreviewTotals groups each line's amount by currency and emits
// one PreviewTotal per distinct currency. Order is insertion-stable
// (first currency seen wins position 0) so the response is deterministic
// across calls. Mirrors the customer-usage computeTotals helper.
func computePreviewTotals(lines []PreviewLine) []PreviewTotal {
	bucket := map[string]int64{}
	var order []string
	for _, line := range lines {
		if line.Currency == "" {
			continue
		}
		if _, ok := bucket[line.Currency]; !ok {
			order = append(order, line.Currency)
		}
		bucket[line.Currency] += line.AmountCents
	}
	out := make([]PreviewTotal, 0, len(order))
	for _, cur := range order {
		out = append(out, PreviewTotal{Currency: cur, AmountCents: bucket[cur]})
	}
	return out
}

// mapMeterAggregation translates the meter's stored aggregation string
// ("sum"/"count"/"max"/"last") to the AggregationMode the priority+claim
// resolver accepts. Duplicated from internal/usage/customer_usage.go
// rather than imported to keep the billing package's dependency graph
// flat (no cross-domain imports between peer packages — see CLAUDE.md
// "Architecture").
func mapMeterAggregation(agg string) domain.AggregationMode {
	switch agg {
	case "sum":
		return domain.AggSum
	case "count":
		return domain.AggCount
	case "max":
		return domain.AggMax
	case "last":
		return domain.AggLastDuringPeriod
	default:
		return domain.AggSum
	}
}
