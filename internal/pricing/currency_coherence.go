package pricing

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/sagarsuperuser/velox/internal/errs"
)

// This file implements the CURRENCY COHERENCE guards (ADR-100). The billing
// engine attaches rule-computed cents to a single invoice currency without
// conversion, so any write that lets a rule's currency diverge from the
// plans billing it — or a plan diverge from the rules its meters resolve —
// silently re-denominates money ($100 becomes €100). The engine deliberately
// does not reconcile ("mismatches are a pricing config error"); these guards
// make the config error unwritable instead:
//
//   G1  rule republish may not change a key's currency while any in-scope
//       plan is bound to it (either binding edge)
//   G2  a plan may not be created — or gain meters via update — whose
//       currency differs from any bound rule key's resolved currency
//   G2b a meter may not bind a rule (pricing-rule row or default binding)
//       whose key resolves to a different currency than the meter's other
//       bound keys or the plans wiring it
//
// "In-scope plans" are non-archived plans plus archived plans that still
// carry non-terminal subscriptions (those keep billing, ADR-100); archived
// plans with no live subs are excluded so a wrong-currency install can be
// repaired by archiving the plan and republishing the key. A bound key's
// currency is its RESOLVED currency — latest active version, mirroring the
// engine's resolveRatedRule — never the binding row's pinned version. Keys
// whose active versions mix currencies are refused outright: as-of
// resolution could bill different periods in different currencies.

// keyCurrency is one bound rule key with its resolved currency, for guard
// messages that name the offender.
type keyCurrency struct {
	RuleKey  string
	Currency string
	Mixed    bool // active versions of this key span >1 currency
}

// planCurrency is one in-scope bound plan, for guard messages.
type planCurrency struct {
	Code     string
	Currency string
}

// guardRuleCurrencyChange refuses a currency-changing republish of ruleKey
// while any in-scope plan is bound to it (G1). Runs in addition to the
// ADR-070 overrides clause.
func (s *Service) guardRuleCurrencyChange(ctx context.Context, tenantID, ruleKey, newCurrency string) error {
	plans, err := s.store.PlanCurrenciesBoundToRuleKey(ctx, tenantID, ruleKey)
	if err != nil {
		return fmt.Errorf("currency guard: plans bound to %q: %w", ruleKey, err)
	}
	var conflicting []string
	for _, p := range plans {
		if !strings.EqualFold(p.Currency, newCurrency) {
			conflicting = append(conflicting, fmt.Sprintf("%s (%s)", p.Code, p.Currency))
		}
	}
	if len(conflicting) > 0 {
		sort.Strings(conflicting)
		return errs.InvalidState(fmt.Sprintf(
			"cannot change currency of price %q to %s: plan(s) %s bill this price and would silently re-denominate — archive those plans first, or keep the currency",
			ruleKey, newCurrency, strings.Join(conflicting, ", ")))
	}
	return nil
}

// guardPlanMeterCurrencies refuses a plan currency that differs from any
// rule key resolved by its wired meters (G2). Used at plan create and at
// update when meter_ids change.
func (s *Service) guardPlanMeterCurrencies(ctx context.Context, tenantID, planCurrency string, meterIDs []string) error {
	if len(meterIDs) == 0 {
		return nil
	}
	keys, err := s.store.MeterBoundKeyCurrencies(ctx, tenantID, meterIDs)
	if err != nil {
		return fmt.Errorf("currency guard: rules bound to meters: %w", err)
	}
	var conflicting []string
	for _, k := range keys {
		switch {
		case k.Mixed:
			conflicting = append(conflicting, fmt.Sprintf("%s (versions span multiple currencies)", k.RuleKey))
		case !strings.EqualFold(k.Currency, planCurrency):
			conflicting = append(conflicting, fmt.Sprintf("%s (%s)", k.RuleKey, k.Currency))
		}
	}
	if len(conflicting) > 0 {
		sort.Strings(conflicting)
		return errs.InvalidState(fmt.Sprintf(
			"plan currency %s conflicts with the price(s) its meters bill: %s — usage would be priced in one currency and invoiced in another without conversion; align the currencies first",
			planCurrency, strings.Join(conflicting, ", ")))
	}
	return nil
}

// guardMeterBinding refuses binding a rule version to a meter (pricing-rule
// row or default binding) when the version's key resolves to a currency that
// differs from the meter's other bound keys or from any in-scope plan wiring
// the meter (G2b).
func (s *Service) guardMeterBinding(ctx context.Context, tenantID, meterID, ruleVersionID string) error {
	newKey, err := s.store.RuleKeyResolvedCurrency(ctx, tenantID, ruleVersionID)
	if err != nil {
		return fmt.Errorf("currency guard: resolve rule version %s: %w", ruleVersionID, err)
	}
	if newKey.Mixed {
		return errs.InvalidState(fmt.Sprintf(
			"price %q has versions in more than one currency and cannot be bound to a meter — republish it in a single currency first", newKey.RuleKey))
	}
	existing, err := s.store.MeterBoundKeyCurrencies(ctx, tenantID, []string{meterID})
	if err != nil {
		return fmt.Errorf("currency guard: meter %s bound rules: %w", meterID, err)
	}
	for _, k := range existing {
		if k.RuleKey == newKey.RuleKey {
			continue
		}
		if k.Mixed || !strings.EqualFold(k.Currency, newKey.Currency) {
			return errs.InvalidState(fmt.Sprintf(
				"price %q is in %s but this meter already bills price %q in %s — a meter's prices must share one currency (ADR-100)",
				newKey.RuleKey, newKey.Currency, k.RuleKey, k.Currency))
		}
	}
	plans, err := s.store.PlanCurrenciesWiringMeters(ctx, tenantID, []string{meterID})
	if err != nil {
		return fmt.Errorf("currency guard: plans wiring meter %s: %w", meterID, err)
	}
	var conflicting []string
	for _, p := range plans {
		if !strings.EqualFold(p.Currency, newKey.Currency) {
			conflicting = append(conflicting, fmt.Sprintf("%s (%s)", p.Code, p.Currency))
		}
	}
	if len(conflicting) > 0 {
		sort.Strings(conflicting)
		return errs.InvalidState(fmt.Sprintf(
			"price %q is in %s but plan(s) %s wire this meter — their invoices would carry amounts priced in %s without conversion; align the currencies first",
			newKey.RuleKey, newKey.Currency, strings.Join(conflicting, ", "), newKey.Currency))
	}
	return nil
}

// AdoptedMeterCurrencyConflict reports (as operator-readable text) whether
// meterID already bills any rule key in a currency other than wantCurrency —
// the recipe-apply probe for adopting a shared meter (ADR-100): a second
// recipe installing in EUR must not silently stack its rules onto a meter
// whose existing rules bill USD plans. Empty string = coherent. Exported as
// a narrow seam for recipe.PricingWriter.
func (s *Service) AdoptedMeterCurrencyConflict(ctx context.Context, tenantID, meterID, wantCurrency string) (string, error) {
	keys, err := s.store.MeterBoundKeyCurrencies(ctx, tenantID, []string{meterID})
	if err != nil {
		return "", fmt.Errorf("meter currency probe: %w", err)
	}
	for _, k := range keys {
		if k.Mixed {
			return fmt.Sprintf("price %q on this meter has versions in more than one currency", k.RuleKey), nil
		}
		if !strings.EqualFold(k.Currency, wantCurrency) {
			return fmt.Sprintf("price %q on this meter bills in %s", k.RuleKey, k.Currency), nil
		}
	}
	return "", nil
}
