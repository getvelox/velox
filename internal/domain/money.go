package domain

import (
	"strconv"
	"strings"
)

// FormatMoneyMinor renders a minor-unit amount for operator-facing prose:
// FormatMoneyMinor(10000000, "usd") == "100,000.00 USD".
//
// Three deliberate choices, each of which had a wrong version in this repo:
//
//   - ISO code, never a bare symbol. A hardcoded "$" mislabels every non-USD
//     tenant's money, which is the exact defect `formatAmountForTimeline`
//     (internal/subscription/handler.go) was written to fix. ADR-100 makes one
//     currency per meter/plan/subscription/customer a structural invariant;
//     rendering it is the cheap half of keeping that honest.
//
//   - Integer math, not float64. Amounts here are commit grants, which run to
//     six and seven figures — past 2^53 minor units a float64 division stops
//     being exact, and a billing engine that rounds a displayed balance has
//     told the operator a number that reconciles against nothing.
//
//   - Thousands separators. Commit exposure is the first operator surface in
//     Velox that routinely shows amounts above five digits; "10000000.00 USD"
//     is a number a human misreads by an order of magnitude at a glance, which
//     defeats the point of surfacing it.
//
// Minor units are assumed to be hundredths. That is already baked into every
// int64 cents field in the schema — a zero-decimal currency (JPY, KRW) would
// need a currency-exponent table, and inventing an exponent here rather than
// looking one up is the banned silent-fallback class. Velox has no zero-decimal
// tenant today; when one arrives this is the single place that changes.
func FormatMoneyMinor(cents int64, currency string) string {
	code := strings.ToUpper(strings.TrimSpace(currency))

	neg := cents < 0
	// Negate in uint64 space: -math.MinInt64 overflows int64, and a balance
	// this function cannot render correctly must not render a wrong one.
	var abs uint64
	if neg {
		abs = uint64(-(cents + 1)) + 1
	} else {
		abs = uint64(cents)
	}

	whole := strconv.FormatUint(abs/100, 10)
	frac := abs % 100

	var b strings.Builder
	if neg {
		b.WriteByte('-')
	}
	// Group the integer part in threes from the right.
	for i, r := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	b.WriteByte('.')
	if frac < 10 {
		b.WriteByte('0')
	}
	b.WriteString(strconv.FormatUint(frac, 10))

	if code != "" {
		b.WriteByte(' ')
		b.WriteString(code)
	}
	return b.String()
}
