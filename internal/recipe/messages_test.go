package recipe

import "testing"

// TestConflictMessages locks the install refusals. The exact differences
// stay in the text (money is shown, not summarised), the price or meter key
// stays (operators find it on the Pricing page by it), and the next step
// follows what differs: a currency-only difference points at the install
// form's currency, any rate difference at publishing a matching version
// (with what that does to customers already on the price), and both at both.
func TestConflictMessages(t *testing.T) {
	rate := []fieldDiff{{"per-unit rate (cents)", "0.0015", "0.002"}}
	currency := []fieldDiff{{"currency", "EUR", "USD"}}
	both := []fieldDiff{{"currency", "EUR", "USD"}, {"per-unit rate (cents)", "0.0015", "0.002"}}

	cases := []struct {
		name, got, want string
	}{
		{"price, rates differ", priceConflictMessage("claude_opus_input", rate),
			`Price "claude_opus_input" already exists with different settings (per-unit rate (cents): recipe wants 0.0015, existing is 0.002). To use it, publish a new version with the recipe's rates on the Pricing page, then install again. Customers billed at this price pay the new rates from their next billing period.`},
		{"price, only currency differs", priceConflictMessage("claude_opus_input", currency),
			`Price "claude_opus_input" already exists with different settings (currency: recipe wants EUR, existing is USD). Install the recipe in USD to use the existing price.`},
		{"price, currency and rates differ", priceConflictMessage("claude_opus_input", both),
			`Price "claude_opus_input" already exists with different settings (currency: recipe wants EUR, existing is USD; per-unit rate (cents): recipe wants 0.0015, existing is 0.002). Install the recipe in USD, and publish a new version of the price with the recipe's rates on the Pricing page. Customers billed at this price pay the new rates from their next billing period.`},
		{"meter aggregation", meterConflictMessage("tokens", []fieldDiff{{"usage aggregation", "sum", "max"}}),
			`Meter "tokens" already exists and adds up usage another way (usage aggregation: recipe wants sum, existing is max), so the recipe can't use it. Its aggregation can change only until the meter has a usage event; after that it is fixed.`},
		{"meter currency", meterCurrencyConflictMessage("tokens", `price "custom_tokens_usd" on this meter bills in USD`, "EUR"),
			`Meter "tokens" already has prices in another currency (price "custom_tokens_usd" on this meter bills in USD), and this install is in EUR. Install the recipe in the meter's currency.`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("got  %q\nwant %q", tc.got, tc.want)
			}
		})
	}
}
