// Package paymentmethods is the customer-facing view of payment methods.
//
// payment_methods rows are the canonical many-to-one store (multiple cards
// per customer, one flagged default). The existing customer_payment_setups
// table stays as the 1:1 denorm summary that billing reads for the "one
// default card" path — this package writes both on every mutation.
//
// The operator routes run under API-key auth: tenantID comes from the auth
// ctx and customerID from the URL path; both are pushed into BeginTx.
package paymentmethods

import (
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// PaymentMethod mirrors one row in payment_methods.
type PaymentMethod struct {
	ID                    string
	TenantID              string
	Livemode              bool
	CustomerID            string
	StripePaymentMethodID string
	Type                  string // "card" for now; other Stripe PM types later
	CardBrand             string
	CardLast4             string
	CardExpMonth          int
	CardExpYear           int
	// CardFingerprint is Stripe's stable hash of the card number
	// (CVC + expiry don't affect it). Same physical card → same
	// fingerprint across re-tokenizations. Used by Upsert to dedupe:
	// if a customer re-runs Add and produces a PM with the same
	// fingerprint as an existing active row, the old row is detached
	// and the new one inherits its is_default flag. Empty for legacy
	// rows attached before the fingerprint plumbing existed (will
	// re-collapse the next time the customer re-attaches the card).
	CardFingerprint string
	IsDefault       bool
	DetachedAt      *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// View is the payment method as the API returns it and as the
// payment_method.* webhooks carry it.
func (p PaymentMethod) View() domain.PaymentMethodView {
	return domain.PaymentMethodView{
		ID: p.ID, CustomerID: p.CustomerID, Type: p.Type,
		CardBrand: p.CardBrand, CardLast4: p.CardLast4,
		CardExpMo: p.CardExpMonth, CardExpYr: p.CardExpYear,
		IsDefault: p.IsDefault, CreatedAt: p.CreatedAt,
	}
}

// IsActive — convenience for "attached and usable".
func (p PaymentMethod) IsActive() bool { return p.DetachedAt == nil }
