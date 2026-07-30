package payment

import (
	"errors"
	"testing"

	"github.com/stripe/stripe-go/v82"
)

// TestClassifyStripeError_IdempotencyConflictIsAmbiguous pins the correction
// made in #678. ErrorTypeIdempotency used to sit alongside ErrorTypeCard and
// ErrorTypeInvalidRequest under the comment "Stripe explicitly rejected the
// request; no charge occurred" — backwards, and expensively so.
//
// Stripe raises this type when the idempotency key was ALREADY USED with
// different parameters. The one thing it proves is that an attempt under this
// key reached Stripe; a PaymentIntent may be live and charging right now.
// Calling that a definite failure stamped payment_status='failed', started
// dunning inline, told the customer their card had failed, and let the next
// retry open a SECOND PaymentIntent beside the live one.
//
// Unknown=true routes it to the payment reconciler instead — the component
// whose whole job is resolving "an attempt may exist at Stripe".
func TestClassifyStripeError_IdempotencyConflictIsAmbiguous(t *testing.T) {
	err := &stripe.Error{
		Type: stripe.ErrorTypeIdempotency,
		Msg:  "Keys for idempotent requests can only be used with the same parameters they were first used with.",
	}

	pe := classifyStripeError(err)
	if pe == nil {
		t.Fatal("classifyStripeError returned nil for a non-nil error")
	}
	if !pe.Unknown {
		t.Error("an idempotency conflict was classified as a DEFINITE failure — it proves the " +
			"opposite (an attempt under this key reached Stripe), so this starts dunning on a " +
			"customer whose charge may be succeeding and frees a retry to open a second PaymentIntent")
	}
}

// TestClassifyStripeError_RealRejectionsStayDefinite is the negative control:
// widening ambiguity is not free. A card decline classified as Unknown would
// stop dunning from starting inline and leave the invoice waiting on a
// reconciler round-trip that has nothing to resolve.
func TestClassifyStripeError_RealRejectionsStayDefinite(t *testing.T) {
	for _, tc := range []struct {
		name string
		typ  stripe.ErrorType
	}{
		{"card decline", stripe.ErrorTypeCard},
		{"invalid request", stripe.ErrorTypeInvalidRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pe := classifyStripeError(&stripe.Error{Type: tc.typ, Msg: "rejected"})
			if pe.Unknown {
				t.Errorf("%s classified as ambiguous — Stripe rejected the request outright, "+
					"no charge occurred, and dunning should start inline", tc.name)
			}
		})
	}
}

// TestIsIdempotencyConflict_DiscriminatesByType guards the predicate that
// decides whether the operator-facing ERROR (naming the key for a Stripe
// dashboard lookup) fires. A predicate that matched everything would bury the
// one case where the key is the only handle on a live PaymentIntent.
func TestIsIdempotencyConflict_DiscriminatesByType(t *testing.T) {
	if !isIdempotencyConflict(&stripe.Error{Type: stripe.ErrorTypeIdempotency}) {
		t.Error("an idempotency error was not recognised as one")
	}
	for _, other := range []stripe.ErrorType{
		stripe.ErrorTypeCard,
		stripe.ErrorTypeAPI,
		stripe.ErrorTypeInvalidRequest,
	} {
		if isIdempotencyConflict(&stripe.Error{Type: other}) {
			t.Errorf("%s was misread as an idempotency conflict", other)
		}
	}
	if isIdempotencyConflict(errors.New("plain error")) {
		t.Error("a non-Stripe error was misread as an idempotency conflict")
	}
}
