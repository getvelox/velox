package errs

import (
	"errors"
	"fmt"
)

// DomainError is a structured error carrying both machine-readable metadata
// (Kind for HTTP status routing, Code for business-rule codes, Field for the
// offending form input) and a human-readable Message. The API layer translates
// these into HTTP responses by matching on Kind first, then pulling Field onto
// the response envelope so the frontend can route the message to the right
// input. Cause is optional and used only for internal/log context.
type DomainError struct {
	Kind    error
	Code    string
	Field   string
	Message string
	Cause   error
}

func (e *DomainError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.Cause)
	}
	return e.Message
}

func (e *DomainError) Unwrap() error { return e.Cause }

// WithCode attaches a stable, machine-readable error code to a DomainError.
// Used to add domain-specific codes (e.g. "coupon_expired") on top of the
// generic Kind routing. Safe to chain onto any constructor:
//
//	return errs.InvalidState("coupon has expired").WithCode(coupon.CodeExpired)
//
// Promised codes are part of the public API — integrators switch on them.
// Change the code string only when the underlying failure semantics change.
func (e *DomainError) WithCode(code string) *DomainError {
	e.Code = code
	return e
}

// Is lets errors.Is match against this error's Kind sentinel. This is how
// respond.FromError routes DomainError{Kind: ErrValidation} to a 422 response
// without the caller needing to wrap manually.
func (e *DomainError) Is(target error) bool {
	if e.Kind == nil {
		return false
	}
	return e.Kind == target
}

func New(code, message string) *DomainError {
	return &DomainError{Code: code, Message: message}
}

func Wrap(cause error, code, message string) *DomainError {
	return &DomainError{Code: code, Message: message, Cause: cause}
}

func Code(err error) string {
	var de *DomainError
	if errors.As(err, &de) {
		return de.Code
	}
	return ""
}

// Field extracts the offending field name from a DomainError anywhere in the
// error chain, or returns "". Used by respond.FromError to populate
// ErrorDetail.Param on the response envelope.
func Field(err error) string {
	var de *DomainError
	if errors.As(err, &de) {
		return de.Field
	}
	return ""
}

// Required returns a validation error indicating a required field is missing.
// Maps to HTTP 422 with Param=field so the client can highlight the input.
//
//	return errs.Required("external_id")
func Required(field string) *DomainError {
	return &DomainError{
		Kind:    ErrValidation,
		Field:   field,
		Message: field + " is required",
	}
}

// Invalid returns a validation error for a field that was provided but failed
// a semantic check (bad format, out of range, etc.). The reason is shown to
// the user verbatim — phrase it as a complete sentence fragment that reads
// naturally under the field, e.g. "must be a valid email address".
//
//	return errs.Invalid("company_email", "must contain @ and a domain")
func Invalid(field, reason string) *DomainError {
	return &DomainError{
		Kind:    ErrValidation,
		Field:   field,
		Message: reason,
	}
}

// AlreadyExists marks a duplicate resource, optionally naming the offending
// field. Maps to HTTP 409. Use when a user-submitted value collides with an
// existing record (customer.external_id, plan.code, coupon.code, etc.).
//
//	return errs.AlreadyExists("code", fmt.Sprintf("plan code %q already exists", code))
func AlreadyExists(field, message string) *DomainError {
	return &DomainError{
		Kind:    ErrAlreadyExists,
		Field:   field,
		Message: message,
	}
}

// InvalidState rejects an operation because the target resource is in a state
// that disallows it (e.g. "can only activate draft subscriptions"). Maps to
// HTTP 409 (see respond/errors.go: the input is fine, the resource's state is
// the conflict — 422 would mean "fix your input"). These errors are not about
// a specific form field — the user usually needs a toast or inline banner,
// not a field highlight.
//
//	return errs.InvalidState("can only activate draft subscriptions")
func InvalidState(message string) *DomainError {
	return &DomainError{
		Kind:    ErrInvalidState,
		Message: message,
	}
}

// PreconditionFailed signals that an HTTP precondition — typically an
// If-Match header carrying the caller's expected resource version — did
// not match the current state. Maps to HTTP 412. The intended use is
// optimistic concurrency: the client sends If-Match with the ETag it last
// saw, and this error fires when a concurrent writer has bumped the
// version in between.
//
//	return errs.PreconditionFailed("coupon has been modified, refresh and retry")
func PreconditionFailed(message string) *DomainError {
	return &DomainError{
		Kind:    ErrPreconditionFailed,
		Message: message,
	}
}

// Forbidden marks an authorization denial — the caller is authenticated but
// not permitted to perform this action (distinct from Invalid, which is bad
// input, and from a missing-auth 401). Maps to HTTP 403. Use at the service
// layer when an authz rule must hold regardless of which handler/route the
// call arrived through, so the rule cannot be bypassed by a future caller.
//
//	return errs.Forbidden("platform keys can only be issued by an existing platform key")
func Forbidden(message string) *DomainError {
	return &DomainError{
		Kind:    ErrForbidden,
		Message: message,
	}
}

// Sentinel errors for store and service layers.
var (
	ErrNotFound           = errors.New("not found")
	ErrAlreadyExists      = errors.New("already exists")
	ErrDuplicateKey       = errors.New("duplicate key")
	ErrInvalidState       = errors.New("invalid state")
	ErrPreconditionFailed = errors.New("precondition failed")
	// ErrForbidden marks an authorization denial (authenticated but not
	// permitted). Maps to HTTP 403 via respond.FromError. Prefer errs.Forbidden.
	ErrForbidden = errors.New("forbidden")
	// ErrValidation marks an error caused by bad caller input (missing field,
	// malformed value). New code should prefer errs.Required / errs.Invalid /
	// errs.AlreadyExists which set Field metadata for the UI. The legacy
	// wrapper form still works:
	//
	//   return fmt.Errorf("%w: customer_id is required", errs.ErrValidation)
	//
	// but leaves the frontend with only a toast — no field highlight.
	ErrValidation = errors.New("validation error")
)

// Error code constants.
const (
	CodeBillingSetupIncomplete = "billing_setup_incomplete"
	CodeStripeRequired         = "stripe_credentials_required"
	CodeInvoiceFetchFailed     = "invoice_fetch_failed"
	CodePaymentRetryFailed     = "payment_retry_failed"
	CodeSubscriptionInvalid    = "subscription_invalid"
	CodePricingUnsupported     = "pricing_unsupported"
	CodeUsageSyncFailed        = "usage_sync_failed"
	CodeAdapterNotConfigured   = "adapter_not_configured"
)
