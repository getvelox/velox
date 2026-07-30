package payment

import (
	"context"
	"testing"

	"github.com/stripe/stripe-go/v82"

	auth "github.com/sagarsuperuser/velox/internal/auth"
)

// TestStripeRefunder_UnboundTenantIsNotAProviderRejection walks the REAL
// StripeRefunder rather than a fake, because the defect it guards was invisible
// to every fake-based test: a background sweep that lists cross-tenant and
// forgets to bind the row's tenant gets no client at all, and a caller reading
// that as "the provider refused" records a failure for a call never made.
//
// The unit tests for the refund sweep inject the refunder directly, so they
// exercise none of this resolution — which is exactly how the bug shipped past
// them (playbook: verify the REAL path, positive and negative).
func TestStripeRefunder_UnboundTenantIsNotAProviderRejection(t *testing.T) {
	r := &StripeRefunder{clients: &StripeClients{}}

	_, _, err := r.CreateRefund(context.Background(), "pi_1", 100, "velox_cn_x")
	if err == nil {
		t.Fatal("a refund with no resolvable client must error")
	}

	var pe *PaymentError
	if !asPaymentError(err, &pe) {
		t.Fatalf("want a *PaymentError so callers can classify it, got %T", err)
	}
	if pe.ProviderReached() {
		t.Error("an unresolvable client reported ProviderReached()=true — callers will record a REJECTION for a request that was never built")
	}
	if pe.AmbiguousOutcome() {
		t.Error("an unresolvable client reported an AMBIGUOUS outcome — nothing was attempted, so the money's state is not in doubt")
	}

	// Negative control — the discrimination must cut both ways, or a caller
	// could simply never record failures. A genuine provider rejection reports
	// ProviderReached()=true and IS evidence about the money.
	rejection := classifyStripeError(&stripe.Error{Type: stripe.ErrorTypeInvalidRequest, Msg: "no such charge"})
	if !rejection.ProviderReached() {
		t.Error("a real provider rejection reported ProviderReached()=false — callers would never record a definite failure")
	}
	if rejection.AmbiguousOutcome() {
		t.Error("a real provider rejection was classified ambiguous")
	}

	// And ForCtx is genuinely tenant-driven: without a tenant on ctx there is
	// no client to resolve, which is the whole mechanism behind the defect.
	if auth.TenantID(context.Background()) != "" {
		t.Fatal("precondition: a bare ctx must carry no tenant")
	}
}

func asPaymentError(err error, target **PaymentError) bool {
	pe, ok := err.(*PaymentError)
	if ok {
		*target = pe
	}
	return ok
}
