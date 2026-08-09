package domain

import (
	"errors"
	"math"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

type PricingMode string

const (
	PricingFlat      PricingMode = "flat"
	PricingGraduated PricingMode = "graduated"
	PricingPackage   PricingMode = "package"
)

type RatingTier struct {
	// UpTo is a tier boundary in whole meter units (never fractional).
	UpTo int64 `json:"up_to"`
	// UnitAmountCents is the per-unit price in cents, carried as an
	// arbitrary-precision decimal so sub-cent unit prices (e.g. $3.00 per
	// 1,000,000 tokens = 0.0003 cents/token) bill linearly and exactly. This
	// is the Stripe `unit_amount_decimal` model. Marshals to/from JSON as a
	// string (shopspring/decimal). Invoice line amounts and totals stay whole
	// int64 cents — only the per-unit RATE gains precision.
	UnitAmountCents decimal.Decimal `json:"unit_amount_cents"`
}

type RatingRuleLifecycle string

const (
	RatingRuleDraft    RatingRuleLifecycle = "draft"
	RatingRuleActive   RatingRuleLifecycle = "active"
	RatingRuleArchived RatingRuleLifecycle = "archived"
)

type RatingRuleVersion struct {
	ID             string              `json:"id"`
	TenantID       string              `json:"tenant_id,omitempty"`
	RuleKey        string              `json:"rule_key"`
	Name           string              `json:"name"`
	Version        int                 `json:"version"`
	LifecycleState RatingRuleLifecycle `json:"lifecycle_state,omitempty"`
	Mode           PricingMode         `json:"mode"`
	Currency       string              `json:"currency"`
	// FlatAmountCents and OverageUnitAmountCents are PER-UNIT rates (multiplied
	// by a decimal quantity), so they carry arbitrary-precision decimals for
	// sub-cent linear pricing (Stripe unit_amount_decimal model). PackageSize
	// and PackageAmountCents stay whole int64 — a package is a fixed fee for a
	// whole block, not a per-unit rate.
	FlatAmountCents        decimal.Decimal `json:"flat_amount_cents"`
	GraduatedTiers         []RatingTier    `json:"graduated_tiers"`
	PackageSize            int64           `json:"package_size"`
	PackageAmountCents     int64           `json:"package_amount_cents"`
	OverageUnitAmountCents decimal.Decimal `json:"overage_unit_amount_cents"`
	CreatedAt              time.Time       `json:"created_at"`
}

type Meter struct {
	ID                  string    `json:"id"`
	TenantID            string    `json:"tenant_id,omitempty"`
	Key                 string    `json:"key"`
	Name                string    `json:"name"`
	Unit                string    `json:"unit"`
	Aggregation         string    `json:"aggregation"`
	RatingRuleVersionID string    `json:"rating_rule_version_id"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
}

// MeterUnitDisplayScale is the display-scale convention for a meter unit:
// token rates are quoted per 1M tokens (how every AI provider and rate
// card lists them — Anthropic, OpenAI, Metronome); everything else quotes
// per single unit. Returns the decimal shift to apply to a per-unit rate
// and the "per" label. The FE twin is web-v2/src/lib/priceDisplay.ts
// unitScale — the two mappings are deliberately tiny and each side pins
// its own with a test; change them together.
func MeterUnitDisplayScale(unit string) (shift int32, per string) {
	switch u := strings.ToLower(strings.TrimSpace(unit)); u {
	case "tokens", "token":
		return 6, "1M tokens"
	default:
		return 0, ""
	}
}

type BillingInterval string

const (
	BillingMonthly BillingInterval = "monthly"
	BillingYearly  BillingInterval = "yearly"
)

type PlanStatus string

const (
	PlanDraft    PlanStatus = "draft"
	PlanActive   PlanStatus = "active"
	PlanArchived PlanStatus = "archived"
)

// BillTiming controls when the recurring base fee is invoiced relative
// to the period it covers. Usage lines are structurally arrears-only
// (future-period quantities are unknown) so this flag only governs
// the base. See ADR-031.
type BillTiming string

const (
	BillInArrears BillTiming = "in_arrears"
	BillInAdvance BillTiming = "in_advance"
)

// IsValid reports whether t is a recognised bill_timing value.
func (t BillTiming) IsValid() bool {
	return t == BillInArrears || t == BillInAdvance
}

type Plan struct {
	ID              string          `json:"id"`
	TenantID        string          `json:"tenant_id,omitempty"`
	Code            string          `json:"code"`
	Name            string          `json:"name"`
	Description     string          `json:"description,omitempty"`
	Currency        string          `json:"currency"`
	BillingInterval BillingInterval `json:"billing_interval"`
	Status          PlanStatus      `json:"status"`
	BaseAmountCents int64           `json:"base_amount_cents"`
	// BaseBillTiming controls whether the base fee is billed at the
	// start of the period (in_advance) or the end (in_arrears). Default
	// is in_arrears for backward compatibility with every existing
	// Velox tenant. See ADR-031.
	BaseBillTiming BillTiming `json:"base_bill_timing"`
	// TaxCode optionally overrides the tenant's default_product_tax_code for
	// this plan. Only consulted by the stripe_tax provider. Empty falls
	// back to tenant_settings.default_product_tax_code.
	TaxCode   string    `json:"tax_code,omitempty"`
	MeterIDs  []string  `json:"meter_ids"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// AggregationMode is how a meter pricing rule rolls up the events it
// claims into a single quantity for the period. The set is fixed at the
// schema layer (CHECK constraint on meter_pricing_rules.aggregation_mode).
//
// Stripe Tier 1 parity: count, last_during_period, last_ever, max are
// modes Stripe's Meter API supports that the prior single-mode meter
// could not express. Hoisted into v1 of multi-dim meters.
type AggregationMode string

const (
	AggSum              AggregationMode = "sum"
	AggCount            AggregationMode = "count"
	AggLastDuringPeriod AggregationMode = "last_during_period"
	AggLastEver         AggregationMode = "last_ever"
	AggMax              AggregationMode = "max"
)

// IsValid returns true if m is a recognised aggregation mode.
func (m AggregationMode) IsValid() bool {
	switch m {
	case AggSum, AggCount, AggLastDuringPeriod, AggLastEver, AggMax:
		return true
	default:
		return false
	}
}

// MeterPricingRule binds a rating rule version to a subset of a meter's
// events via a JSONB dimension match. At billing time, rules are walked
// in priority-DESC order and each event is claimed by the highest-
// priority matching rule (no double-count). The default rule for a
// meter has dimension_match='{}' which matches every event; priority=0
// keeps it last so specific rules win.
//
// See docs/design-multi-dim-meters.md for the resolution algorithm.
type MeterPricingRule struct {
	ID                  string          `json:"id"`
	TenantID            string          `json:"tenant_id,omitempty"`
	MeterID             string          `json:"meter_id"`
	RatingRuleVersionID string          `json:"rating_rule_version_id"`
	DimensionMatch      map[string]any  `json:"dimension_match"`
	AggregationMode     AggregationMode `json:"aggregation_mode"`
	Priority            int             `json:"priority"`
	CreatedAt           time.Time       `json:"created_at"`
	UpdatedAt           time.Time       `json:"updated_at"`
}

// RuleAggregation is one row of the priority+claim resolution result: events
// in a billing period that were claimed by a single pricing rule, rolled up
// per the rule's aggregation_mode into a final billable quantity.
//
// RuleID == "" indicates the unclaimed bucket — events that matched no rule.
// The caller falls back to meters.rating_rule_version_id with the meter's
// own aggregation, preserving back-compat for tenants without any rules.
type RuleAggregation struct {
	RuleID              string          `json:"rule_id,omitempty"`
	RatingRuleVersionID string          `json:"rating_rule_version_id,omitempty"`
	AggregationMode     AggregationMode `json:"aggregation_mode"`
	Quantity            decimal.Decimal `json:"quantity"`
}

var ErrInvalidPricingConfig = errors.New("invalid pricing config")

// ErrAmountOverflow is returned when a pricing computation would exceed
// int64. Callers must treat this as a hard failure — a silent wrap would
// emit a negative invoice line in production.
var ErrAmountOverflow = errors.New("amount overflow: exceeds int64 range")

// maxInt64Decimal caches a decimal copy of math.MaxInt64 for the overflow
// guard at the int64-cents conversion boundary.
var maxInt64Decimal = decimal.NewFromInt(math.MaxInt64)

// NominalUnitAmountDecimal is the configured per-unit rate to DISPLAY for a
// rule, in DECIMAL CENTS: the flat rate for flat rules, nil for graduated /
// package rules (which have no single unit rate — callers fall back to the
// effective amount÷quantity). This is the SINGLE source the invoice line stamp
// (billing.nominalRate) and the usage / cost-dashboard rule rows both use, so
// every surface renders the same unit price (ADR-054).
func NominalUnitAmountDecimal(rule RatingRuleVersion) *decimal.Decimal {
	if rule.Mode != PricingFlat {
		return nil
	}
	r := rule.FlatAmountCents
	return &r
}

// ComputeAmountCents prices a decimal quantity through the given rating rule
// and returns the result rounded to whole cents. Quantity is decimal so that
// fractional usage primitives (GPU-hours, cached-token ratios) round-trip
// without precision loss. Tier boundaries (`up_to`, `package_size`) stay
// integer because pricing config is authored that way; the math walks tiers
// in decimal space and converts to int64 cents only at the final round.
//
// Rounding: half-to-even (banker's rounding) at the cent boundary. This is
// the same convention used elsewhere in the engine (money.RoundHalfToEven)
// and the IEEE 754 default — minimizes systematic bias on bulk invoices.
func ComputeAmountCents(rule RatingRuleVersion, quantity decimal.Decimal) (int64, error) {
	if quantity.IsNegative() {
		return 0, ErrInvalidPricingConfig
	}

	switch rule.Mode {
	case PricingFlat:
		if rule.FlatAmountCents.IsNegative() {
			return 0, ErrInvalidPricingConfig
		}
		if quantity.IsZero() {
			return 0, nil
		}
		total := quantity.Mul(rule.FlatAmountCents)
		return decimalToCents(total)

	case PricingGraduated:
		if len(rule.GraduatedTiers) == 0 {
			return 0, ErrInvalidPricingConfig
		}
		// A catch-all tier (UpTo==0) consumes all remaining quantity and ends
		// rating, so it must be LAST and unique. A catch-all followed by more
		// tiers (incl. a second catch-all) makes those tiers dead config and
		// would silently price overflow quantity at the wrong rate — reject it.
		for i, t := range rule.GraduatedTiers {
			if t.UpTo == 0 && i != len(rule.GraduatedTiers)-1 {
				return 0, ErrInvalidPricingConfig
			}
		}
		remaining := quantity
		lastUpper := int64(0)
		amount := decimal.Zero
		for i, tier := range rule.GraduatedTiers {
			if tier.UnitAmountCents.IsNegative() || tier.UpTo < 0 {
				return 0, ErrInvalidPricingConfig
			}
			if remaining.IsZero() {
				break
			}
			if tier.UpTo == 0 {
				amount = amount.Add(remaining.Mul(tier.UnitAmountCents))
				remaining = decimal.Zero
				break
			}
			if tier.UpTo < lastUpper {
				return 0, ErrInvalidPricingConfig
			}
			tierCapacity := tier.UpTo - lastUpper
			if i == 0 {
				tierCapacity = tier.UpTo
			}
			if tierCapacity < 0 {
				return 0, ErrInvalidPricingConfig
			}
			capDec := decimal.NewFromInt(tierCapacity)
			consumed := remaining
			if consumed.GreaterThan(capDec) {
				consumed = capDec
			}
			amount = amount.Add(consumed.Mul(tier.UnitAmountCents))
			remaining = remaining.Sub(consumed)
			lastUpper = tier.UpTo
		}
		if remaining.IsPositive() {
			return 0, ErrInvalidPricingConfig
		}
		return decimalToCents(amount)

	case PricingPackage:
		if rule.PackageSize <= 0 || rule.PackageAmountCents < 0 || rule.OverageUnitAmountCents.IsNegative() {
			return 0, ErrInvalidPricingConfig
		}
		if quantity.IsZero() {
			return 0, nil
		}
		pkgSize := decimal.NewFromInt(rule.PackageSize)
		fullPackages := quantity.Div(pkgSize).Floor()
		remainder := quantity.Sub(fullPackages.Mul(pkgSize))
		total := fullPackages.Mul(decimal.NewFromInt(rule.PackageAmountCents)).
			Add(remainder.Mul(rule.OverageUnitAmountCents))
		return decimalToCents(total)

	default:
		return 0, ErrInvalidPricingConfig
	}
}

// decimalToCents rounds a decimal cents value (potentially fractional after
// quantity multiplication) to int64 using banker's rounding. Returns
// ErrAmountOverflow if the rounded value would exceed int64 — silent wrap
// would emit a negative invoice line in production, so this is fail-loud.
func decimalToCents(d decimal.Decimal) (int64, error) {
	rounded := d.RoundBank(0)
	// A negative value is < MaxInt64 so it slips past the overflow guard and
	// IntPart() would silently emit a negative invoice line — the exact
	// fail-open this function exists to prevent. Callers only ever reach here
	// with non-negative amounts (quantity and rates are validated >= 0), so a
	// negative here is a logic error: fail loud rather than bill a negative.
	if rounded.IsNegative() {
		return 0, ErrInvalidPricingConfig
	}
	if rounded.GreaterThan(maxInt64Decimal) {
		return 0, ErrAmountOverflow
	}
	return rounded.IntPart(), nil
}
