package domain

import (
	"time"
)

// CustomerTaxStatus drives the tax decision for a customer. Mirrors the
// CHECK constraint on customer_billing_profiles.tax_status.
type CustomerTaxStatus string

const (
	// TaxStatusStandard: regular taxable customer. Tax applied per provider rules.
	TaxStatusStandard CustomerTaxStatus = "standard"
	// TaxStatusExempt: customer is not taxable (reseller, non-profit,
	// government entity with a certificate on file). Tax is zero;
	// exempt_reason is recorded on the invoice for audit.
	TaxStatusExempt CustomerTaxStatus = "exempt"
	// TaxStatusReverseCharge: B2B buyer self-accounts for tax in their
	// jurisdiction (EU VAT reverse charge, Indian GST RCM, etc.). Tax is
	// zero on the seller's invoice; the PDF renders the legally required
	// legend directing the buyer to self-assess.
	TaxStatusReverseCharge CustomerTaxStatus = "reverse_charge"
)

type CustomerStatus string

const (
	CustomerStatusActive   CustomerStatus = "active"
	CustomerStatusArchived CustomerStatus = "archived"
)

// CustomerEmailStatus mirrors the CHECK constraint on
// customers.email_status. 'unknown' is the default (we've never observed
// a delivery outcome); 'bounced' is populated by Sender on SMTP 5xx or
// later by provider webhooks (SES/SendGrid) plugging into the same
// customer.MarkEmailBounced code path.
type CustomerEmailStatus string

const (
	EmailStatusUnknown    CustomerEmailStatus = "unknown"
	EmailStatusOK         CustomerEmailStatus = "ok"
	EmailStatusBounced    CustomerEmailStatus = "bounced"
	EmailStatusComplained CustomerEmailStatus = "complained"
)

type Customer struct {
	ID          string `json:"id"`
	TenantID    string `json:"tenant_id,omitempty"`
	ExternalID  string `json:"external_id"`
	DisplayName string `json:"display_name"`
	Email       string `json:"email,omitempty"`
	// AdditionalEmails are CC'd on billing documents and billing-state
	// emails (invoice, receipt, dunning, payment failed, credit note) —
	// never on credential-bearing emails (setup links, resets, invites).
	// Stored encrypted alongside email (JSON array under the same
	// encryptor, migration 0140); normalized lowercase addr-specs, cap
	// 10, validated in the service. ADR-082.
	AdditionalEmails []string       `json:"additional_emails,omitempty"`
	Status           CustomerStatus `json:"status"`
	// Email deliverability signal populated by bounce-capture hooks.
	// Most rows stay 'unknown' until a send outcome is observed; partners
	// that never send stay at default forever.
	EmailStatus        CustomerEmailStatus `json:"email_status,omitempty"`
	EmailLastBouncedAt *time.Time          `json:"email_last_bounced_at,omitempty"`
	EmailBounceReason  string              `json:"email_bounce_reason,omitempty"`
	// There is deliberately NO CostDashboardToken field here (removed with
	// migration 0172). The public cost-dashboard credential is stored only
	// as a SHA-256 blind index, so there is no plaintext for a read to
	// hydrate — and the raw token now exists in-process for exactly the
	// length of one rotate response. Carrying it on this struct is what
	// made the 2026-07-19 truth audit's finding possible (every
	// authenticated customer GET/List re-disclosed the credential
	// indefinitely, because the field then had a json tag); the field is
	// gone rather than merely untagged so the mistake cannot recur by
	// someone adding a tag back. Resolve a presented token with
	// customer.Store.GetByCostDashboardToken, which hashes before it
	// compares.
	//
	// TestClockID pins this customer to a test clock (Stripe parity,
	// ADR-027). Once set at create time, every Subscription / Invoice
	// for this customer runs on that clock's simulated time. Empty
	// for live-mode customers and for test-mode customers explicitly
	// not pinned. Cannot be changed after creation — to switch
	// clocks, delete + recreate the customer (matches Stripe).
	TestClockID string `json:"test_clock_id,omitempty"`
	// DunningPolicyID assigns this customer to a specific dunning
	// policy / campaign (ADR-036). Empty/nil = use the tenant's
	// default policy. Updatable any time via PATCH; affects only the
	// NEXT dunning run started for this customer's invoices — runs
	// already in flight stay on their original policy.
	DunningPolicyID string `json:"dunning_policy_id,omitempty"`
	// StripeCustomerID is the Stripe Customer object mapped 1:1 to
	// this Velox customer. Created lazily on first PM-related action
	// (paymentmethods.StripeAdapter.EnsureStripeCustomer) — empty
	// for customers who never went through any PM flow. Single source
	// of truth for the mapping since migration 0096; previously lived
	// on customer_payment_setups (now deprecated).
	StripeCustomerID string `json:"stripe_customer_id,omitempty"`
	// Livemode is the mode the customer row lives in (live vs test).
	// Hydrated by lookups that resolve a customer outside of any tenant
	// context (e.g. GetByCostDashboardToken on the public cost-dashboard
	// route): the caller must pin this onto ctx via postgres.WithLivemode
	// before any TxTenant read, otherwise the RLS livemode predicate
	// defaults to live and a test-mode customer's reads return nothing.
	Livemode  bool      `json:"livemode,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type BillingProfileStatus string

const (
	BillingProfileMissing    BillingProfileStatus = "missing"
	BillingProfileIncomplete BillingProfileStatus = "incomplete"
	BillingProfileReady      BillingProfileStatus = "ready"
)

type CustomerBillingProfile struct {
	CustomerID string `json:"customer_id"`
	TenantID   string `json:"tenant_id,omitempty"`
	LegalName  string `json:"legal_name,omitempty"`
	// Email removed in migration 0100 — the billing-profile email
	// duplicated customers.email and broke the bounce-tracking key
	// once they diverged. Phase 1 of the dual-email collapse:
	// customers.email is the single canonical recipient. Phase 2
	// SHIPPED 2026-07-06 (ADR-082): customers.additional_emails CC list.
	// Phase 3 (a customer_email_contacts table — per-contact names,
	// per-type routing, per-contact deliverability) triggers when a DP
	// asks for per-recipient preferences.
	Phone        string `json:"phone,omitempty"`
	AddressLine1 string `json:"address_line1,omitempty"`
	AddressLine2 string `json:"address_line2,omitempty"`
	City         string `json:"city,omitempty"`
	State        string `json:"state,omitempty"`
	PostalCode   string `json:"postal_code,omitempty"`
	Country      string `json:"country,omitempty"`
	Currency     string `json:"currency,omitempty"`
	// TaxStatus drives tax-decision routing independent of the configured
	// provider. 'standard' is the default; 'exempt' and 'reverse_charge' both
	// zero the invoice tax but render different legends on the PDF (exempt
	// shows the certificate reason; reverse charge directs the buyer to
	// self-assess). Mirrors the CHECK constraint on
	// customer_billing_profiles.tax_status.
	TaxStatus       CustomerTaxStatus    `json:"tax_status"`
	TaxExemptReason string               `json:"tax_exempt_reason,omitempty"`
	TaxID           string               `json:"tax_id"`
	TaxIDType       string               `json:"tax_id_type"`
	ProfileStatus   BillingProfileStatus `json:"profile_status"`
	CreatedAt       time.Time            `json:"created_at"`
	UpdatedAt       time.Time            `json:"updated_at"`
}

type PaymentSetupStatus string

const (
	PaymentSetupMissing PaymentSetupStatus = "missing"
	PaymentSetupPending PaymentSetupStatus = "pending"
	PaymentSetupReady   PaymentSetupStatus = "ready"
	PaymentSetupError   PaymentSetupStatus = "error"
)

type CustomerPaymentSetup struct {
	CustomerID                  string             `json:"customer_id"`
	TenantID                    string             `json:"tenant_id,omitempty"`
	SetupStatus                 PaymentSetupStatus `json:"setup_status"`
	DefaultPaymentMethodPresent bool               `json:"default_payment_method_present"`
	PaymentMethodType           string             `json:"payment_method_type,omitempty"`
	StripeCustomerID            string             `json:"stripe_customer_id,omitempty"`
	StripePaymentMethodID       string             `json:"stripe_payment_method_id,omitempty"`
	CardBrand                   string             `json:"card_brand,omitempty"`
	CardLast4                   string             `json:"card_last4,omitempty"`
	CardExpMonth                int                `json:"card_exp_month,omitempty"`
	CardExpYear                 int                `json:"card_exp_year,omitempty"`
	LastVerifiedAt              *time.Time         `json:"last_verified_at,omitempty"`
	CreatedAt                   time.Time          `json:"created_at"`
	UpdatedAt                   time.Time          `json:"updated_at"`
}
