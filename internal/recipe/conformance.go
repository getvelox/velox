package recipe

import (
	"fmt"
	"strings"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// This file implements the recipe ADOPTION GUARDS. When Instantiate finds an
// existing meter or rating rule matching a recipe key, it may adopt the
// object only if its billing-consulted config matches what the recipe
// declares; a divergent match is REFUSED (409), never silently adopted.
//
// Meters: AGGREGATION is the sole billing-consulted field — engine.go's
// deferredBucket/mapMeterAggregation drive usage roll-up off it — so adopting
// a same-key meter with a divergent aggregation would silently mis-bill.
//
// Rating rules: the MONEY fields (mode, currency, the rate scalars, tiers).
// Adoption never republishes — an existing key is bound at its current
// version precisely so a re-apply can't reprice live subs (ADR-070) — but
// ungated that makes divergence a silent substitution: the preview shows the
// recipe's declared rates while the installed plan bills the existing
// rule's. Same lie class ADR-083 killed for plans, same remedy: refuse
// loud, let the operator reconcile. (Gate added 2026-07-26 by the recipe
// e2e review; before that rule adoption was deliberately ungated, which
// protected live subs but let the preview lie about what installs.)
//
// There is deliberately NO live-subscription clause on either guard:
// adoption only READS the object (to wire a fresh plan and append disjoint
// pricing bindings), never mutates it, so config-match is the complete
// safety condition — and it lets a second AI recipe adopt the shared
// ADR-044 `tokens` meter after go-live. Plans are never adopted (each
// install generates a fresh born-unique plan, ADR-085). Names/units are
// display labels, not billing-consulted.

// fieldDiff is one billing-affecting field where an existing object diverges
// from what the recipe declares. Want is the recipe's value, Got the existing.
type fieldDiff struct {
	Field string
	Want  string
	Got   string
}

// meterConformanceDiff returns billing-affecting divergence between an existing
// meter (matched by key) and the recipe's declared meter. Only Aggregation is
// compared — the sole billing-consulted field.
func meterConformanceDiff(existing domain.Meter, rm domain.RecipeMeter) []fieldDiff {
	var diffs []fieldDiff
	if existing.Aggregation != rm.Aggregation {
		diffs = append(diffs, fieldDiff{"usage aggregation", rm.Aggregation, existing.Aggregation})
	}
	return diffs
}

// formatDiffs renders operator-facing "field: recipe wants X, existing is Y" copy.
func formatDiffs(diffs []fieldDiff) string {
	parts := make([]string, 0, len(diffs))
	for _, d := range diffs {
		parts = append(parts, fmt.Sprintf("%s: recipe wants %s, existing is %s", d.Field, d.Want, d.Got))
	}
	return strings.Join(parts, "; ")
}

// ruleConformanceDiff returns money-affecting divergence between an existing
// rating rule's CURRENT ACTIVE version (what adoption would bind, and what
// live usage on this key already bills) and the recipe's declared rule.
// Mode, currency, and every rate field are compared — each one changes what
// an invoice line computes. Name is a display label and is not compared.
func ruleConformanceDiff(existing domain.RatingRuleVersion, rr domain.RecipeRatingRule) []fieldDiff {
	var diffs []fieldDiff
	if existing.Mode != rr.Mode {
		diffs = append(diffs, fieldDiff{"pricing mode", string(rr.Mode), string(existing.Mode)})
	}
	if existing.Currency != rr.Currency {
		diffs = append(diffs, fieldDiff{"currency", rr.Currency, existing.Currency})
	}
	if !existing.FlatAmountCents.Equal(rr.FlatAmountCents) {
		diffs = append(diffs, fieldDiff{"per-unit rate (cents)", rr.FlatAmountCents.String(), existing.FlatAmountCents.String()})
	}
	if !tiersEqual(existing.GraduatedTiers, rr.GraduatedTiers) {
		diffs = append(diffs, fieldDiff{"graduated tiers", describeTiers(rr.GraduatedTiers), describeTiers(existing.GraduatedTiers)})
	}
	if existing.PackageSize != rr.PackageSize {
		diffs = append(diffs, fieldDiff{"package size", fmt.Sprintf("%d", rr.PackageSize), fmt.Sprintf("%d", existing.PackageSize)})
	}
	if existing.PackageAmountCents != rr.PackageAmountCents {
		diffs = append(diffs, fieldDiff{"package amount (cents)", fmt.Sprintf("%d", rr.PackageAmountCents), fmt.Sprintf("%d", existing.PackageAmountCents)})
	}
	if !existing.OverageUnitAmountCents.Equal(rr.OverageUnitAmountCents) {
		diffs = append(diffs, fieldDiff{"overage per-unit rate (cents)", rr.OverageUnitAmountCents.String(), existing.OverageUnitAmountCents.String()})
	}
	return diffs
}

// tiersEqual compares graduated tier ladders exactly: same length, same
// boundaries, same per-unit rates (decimal-equal, so "0.30" == "0.3").
func tiersEqual(a, b []domain.RatingTier) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].UpTo != b[i].UpTo || !a[i].UnitAmountCents.Equal(b[i].UnitAmountCents) {
			return false
		}
	}
	return true
}

// describeTiers renders a tier ladder compactly for the 409 message, e.g.
// "3 tiers (up_to 1000 @0.5, up_to 5000 @0.4, rest @0.3)".
func describeTiers(ts []domain.RatingTier) string {
	if len(ts) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(ts))
	for _, t := range ts {
		if t.UpTo == 0 {
			parts = append(parts, "rest @"+t.UnitAmountCents.String())
		} else {
			parts = append(parts, fmt.Sprintf("up_to %d @%s", t.UpTo, t.UnitAmountCents.String()))
		}
	}
	return fmt.Sprintf("%d tiers (%s)", len(ts), strings.Join(parts, ", "))
}
