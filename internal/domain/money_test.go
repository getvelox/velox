package domain

import (
	"math"
	"testing"
)

func TestFormatMoneyMinor(t *testing.T) {
	cases := []struct {
		name     string
		cents    int64
		currency string
		want     string
	}{
		{"zero", 0, "USD", "0.00 USD"},
		{"sub-dollar keeps both decimals", 5, "USD", "0.05 USD"},
		{"ten cents is not one", 10, "USD", "0.10 USD"},
		{"no separator below a thousand", 99999, "USD", "999.99 USD"},
		{"first separator", 100000, "USD", "1,000.00 USD"},
		{"the commit-sized amount", 10000000, "USD", "100,000.00 USD"},
		{"two separators", 123456789, "EUR", "1,234,567.89 EUR"},
		{"currency is upcased", 10000, "usd", "100.00 USD"},
		{"currency is trimmed", 10000, " gbp ", "100.00 GBP"},
		{"negative", -12345, "USD", "-123.45 USD"},
		{"negative with separator", -100000, "USD", "-1,000.00 USD"},

		// A missing currency omits the code rather than inventing one. ADR-100
		// makes currency coherent across the money path, so this is a
		// zero-value/test-fixture case, not a live one — but guessing "USD"
		// here is precisely the silent fallback the house rules ban.
		{"empty currency omits the code", 10000, "", "100.00"},

		// The reason this uses integer math. float64(math.MaxInt64)/100 is not
		// representable, so a float implementation returns 92233720368547758.08
		// — off by four cents, silently.
		{"max int64 stays exact", math.MaxInt64, "USD", "92,233,720,368,547,758.07 USD"},

		// Negating math.MinInt64 overflows int64; the uint64 path must survive
		// it rather than wrapping to a positive number.
		{"min int64 does not overflow", math.MinInt64, "USD", "-92,233,720,368,547,758.08 USD"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := FormatMoneyMinor(tc.cents, tc.currency); got != tc.want {
				t.Fatalf("FormatMoneyMinor(%d, %q) = %q, want %q", tc.cents, tc.currency, got, tc.want)
			}
		})
	}
}
