package litellm

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// noCachePublished lists model families whose provider publishes NO
// cached-input price (verified against the official model pages
// 2026-07-26): OpenAI's pro-tier reasoning models follow that pattern, and
// gpt-4-turbo / gpt-3.5-turbo predate prompt caching entirely. For these,
// a recipe cache_read rule would carry an invented rate — so the coherence
// requirement drops to input+output. If a provider starts publishing a
// cached price for one of these, remove it here and the test will demand
// the recipe rule.
var noCachePublished = map[string]bool{
	"gpt-4-turbo":   true,
	"gpt-3.5-turbo": true,
	"o1-pro":        true,
	"o3-pro":        true,
	"gpt-5.4-pro":   true,
	"gpt-5.5-pro":   true,
}

// TestModelFamilies_EveryTokenPricedByARecipe pins mapper↔recipe coherence
// on BOTH axes: every recipe token the mapper can emit on the `model`
// dimension must have a pricing rule for every token_type the mapper can
// emit (input, output, cache_read — cache_write is deferred, see MapPayload)
// — or traffic on that (family, role) ingests fine and then silently
// doesn't bill at cycle close. The original version of this test checked
// only the model axis, which let a family with an input rule but no output
// rule pass while half its revenue leaked.
//
// A rule with no token_type in its dimension_match (the embedding rules)
// is a wildcard: it matches any role, so it covers the family completely.
//
// Direction matters: mapper tokens ⊆ recipe rules. The reverse (recipe
// prices a family the mapper doesn't detect, or a role it doesn't emit,
// like Anthropic's cache_write tiers) is harmless — direct API ingest can
// still use it.
func TestModelFamilies_EveryTokenPricedByARecipe(t *testing.T) {
	// recipeRoles parses a recipe YAML into model → set of priced
	// token_types, with the empty string standing for a wildcard rule.
	recipeRoles := func(path string) map[string]map[string]bool {
		t.Helper()
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		var doc struct {
			PricingRules []struct {
				DimensionMatch map[string]string `yaml:"dimension_match"`
			} `yaml:"pricing_rules"`
		}
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		roles := map[string]map[string]bool{}
		for _, r := range doc.PricingRules {
			m := r.DimensionMatch["model"]
			if m == "" {
				continue
			}
			if roles[m] == nil {
				roles[m] = map[string]bool{}
			}
			roles[m][r.DimensionMatch["token_type"]] = true
		}
		return roles
	}

	anthropic := recipeRoles("../../recipe/recipes/anthropic_style.yaml")
	openai := recipeRoles("../../recipe/recipes/openai_style.yaml")

	// The roles MapPayload can emit — keep in sync with the TokenType*
	// constants and the emission sites in mapper.go.
	mapperRoles := []string{TokenTypeInput, TokenTypeOutput, TokenTypeCacheRead}

	for _, f := range modelFamilies {
		var roles map[string]map[string]bool
		var where string
		switch {
		case strings.HasPrefix(f.recipeToken, "claude-"):
			roles, where = anthropic, "anthropic_style.yaml"
		case strings.HasPrefix(f.recipeToken, "gpt-"),
			strings.HasPrefix(f.recipeToken, "o1"),
			strings.HasPrefix(f.recipeToken, "o3"),
			strings.HasPrefix(f.recipeToken, "o4"),
			strings.HasPrefix(f.recipeToken, "text-embedding-"):
			roles, where = openai, "openai_style.yaml"
		default:
			t.Errorf("model family %q has no recipe mapping rule in this test — extend the switch when adding a provider", f.recipeToken)
			continue
		}
		priced := roles[f.recipeToken]
		if len(priced) == 0 {
			t.Errorf("mapper emits model=%q (prefix %q) but %s has NO pricing rule for it — that traffic ingests and then silently doesn't bill at cycle close", f.recipeToken, f.prefix, where)
			continue
		}
		if priced[""] {
			continue // wildcard rule (no token_type) covers every role
		}
		for _, role := range mapperRoles {
			if role == TokenTypeCacheRead && noCachePublished[f.recipeToken] {
				continue
			}
			if !priced[role] {
				t.Errorf("mapper can emit (model=%q, token_type=%q) but %s has no rule for that pair — that role's traffic silently doesn't bill at cycle close", f.recipeToken, role, where)
			}
		}
	}
}
