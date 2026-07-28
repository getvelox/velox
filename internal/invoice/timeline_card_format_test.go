package invoice

import (
	"testing"
)

// TestFormatPaymentCardDetail covers the sub-line text shown
// beneath the "Invoice paid" timeline row (ADR-020). Brand is
// title-cased per Stripe convention; missing fields degrade
// cleanly.
func TestFormatPaymentCardDetail(t *testing.T) {
	cases := []struct {
		name  string
		brand string
		last4 string
		want  string
	}{
		{"both present, brand title-cased", "visa", "4242", "via Visa •••• 4242"},
		{"mastercard", "mastercard", "1234", "via Mastercard •••• 1234"},
		{"amex variant", "amex", "0005", "via American Express •••• 0005"},
		{"american express snake_case (Stripe DisplayBrand)", "american_express", "0005", "via American Express •••• 0005"},
		{"american express full", "american express", "0005", "via American Express •••• 0005"},
		{"cartes bancaires (dual-branded EU card)", "cartes_bancaires", "1111", "via Cartes Bancaires •••• 1111"},
		{"diners_club Stripe DisplayBrand form", "diners_club", "2222", "via Diners Club •••• 2222"},
		{"union_pay Stripe DisplayBrand form", "union_pay", "3333", "via UnionPay •••• 3333"},
		{"other -> Card", "other", "4444", "via Card •••• 4444"},
		{"unknown future brand title-cased per snake segment", "future_network", "9999", "via Future Network •••• 9999"},
		{"missing brand keeps last4 visible", "", "4242", "via card •••• 4242"},
		{"missing last4 keeps brand visible", "visa", "", "via Visa"},
		{"both empty: no sub-line", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatPaymentCardDetail(tc.brand, tc.last4)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
