package domain

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/sagarsuperuser/velox/internal/errs"
)

// MaxLen validates a field doesn't exceed max length. Returns an errs.Invalid
// so callers can surface the message on the named field.
func MaxLen(field, value string, max int) error {
	if len(value) > max {
		return errs.Invalid(field, "must be at most "+strconv.Itoa(max)+" characters")
	}
	return nil
}

// ValidateCurrency checks for common ISO 4217 currency codes.
//
// Zero-decimal currencies (JPY, KRW, VND, CLP — currencies with no minor
// unit) are DELIBERATELY absent (ADR-100): every Velox amount is minor-unit
// cents divided by 100 at render, and Stripe reads zero-decimal amounts as
// WHOLE units — accepting them would 100x-overcharge at the PaymentIntent.
// They return a loud not-yet-supported error instead of a silent wrong
// charge; restore each only alongside real zero-decimal amount handling.
var validCurrencies = map[string]bool{
	"USD": true, "EUR": true, "GBP": true, "CAD": true, "AUD": true,
	"CHF": true, "CNY": true, "INR": true, "BRL": true,
	"MXN": true, "SGD": true, "HKD": true, "NZD": true, "SEK": true,
	"NOK": true, "DKK": true, "ZAR": true, "TWD": true,
	"PLN": true, "CZK": true, "HUF": true, "ILS": true, "AED": true,
	"SAR": true, "THB": true, "MYR": true, "IDR": true, "PHP": true,
	"COP": true, "PEN": true, "ARS": true,
}

// zeroDecimalCurrencies get a distinct, honest refusal (not "invalid code")
// so the operator learns the real constraint.
var zeroDecimalCurrencies = map[string]bool{
	"JPY": true, "KRW": true, "VND": true, "CLP": true,
}

// ValidateCurrency returns an errs.Invalid/Required tied to the "currency"
// field. When the caller's form uses a different field name (e.g.
// "default_currency"), wrap the returned message with the right field by
// calling errs.Invalid yourself instead.
func ValidateCurrency(currency string) error {
	upper := strings.ToUpper(strings.TrimSpace(currency))
	if upper == "" {
		return errs.Required("currency")
	}
	if zeroDecimalCurrencies[upper] {
		return errs.Invalid("currency", upper+" is not supported yet: it has no minor unit, and Velox amounts are minor-unit cents — charging would be off by 100x. Zero-decimal currency support is tracked; use a decimal currency for now")
	}
	if !validCurrencies[upper] {
		return errs.Invalid("currency", "invalid currency code: "+upper)
	}
	return nil
}

// stripeTaxCodeFormat matches a Stripe product tax code: "txcd_" followed by
// 8 digits (e.g. txcd_10103001 for SaaS business use). The full catalog is
// maintained by Stripe; we only enforce the format to catch typos.
var stripeTaxCodeFormat = regexp.MustCompile(`^txcd_[0-9]{8}$`)

// ValidateStripeTaxCode checks a Stripe product tax code format. Empty is
// allowed — callers supply a default when the field is optional.
func ValidateStripeTaxCode(field, code string) error {
	if code == "" {
		return nil
	}
	if !stripeTaxCodeFormat.MatchString(code) {
		return errs.Invalid(field, "must be a Stripe product tax code like 'txcd_10103001'")
	}
	return nil
}
