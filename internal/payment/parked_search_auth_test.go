package payment

import (
	"errors"
	"net/http"
	"testing"

	"github.com/stripe/stripe-go/v82"
)

// Stripe answers BOTH "this account cannot use Search" and "your API key is
// bad" with type `invalid_request_error`, so the type alone cannot separate a
// permanent capability refusal from a credential fault that heals the moment
// the operator fixes the key. Verified against the live API 2026-08-05:
//
//	bad/revoked key                          -> HTTP 401
//	genuine request-shape refusal (bad query) -> HTTP 400
//
// The distinction matters because the two get opposite treatment: a refusal
// latches the parked-invoice sweep OFF for the account, while a credential
// fault must stay transient and retry. Mapping 401 to the refusal branch meant
// one botched key rotation silently disabled parked-invoice self-resolution
// for the whole process — and fixing the key did not bring it back, because
// the latch is in memory and never re-probes. Walked live before the fix: five
// reconciler ticks after the key was restored, the parked row had still never
// been asked about again.
func TestSearchErrorClassification_AuthIsTransientNotRefusal(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      int
		wantRefusal bool
	}{
		{"revoked or bad API key (401)", http.StatusUnauthorized, false},
		{"account cannot use Search (400)", http.StatusBadRequest, true},
		{"malformed query (400)", http.StatusBadRequest, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := classifySearchError(&stripe.Error{
				Type:           stripe.ErrorTypeInvalidRequest,
				Msg:            "some provider message",
				HTTPStatusCode: tc.status,
			})
			gotRefusal := errors.Is(err, ErrSearchNotOffered)
			if gotRefusal != tc.wantRefusal {
				t.Errorf("ErrSearchNotOffered = %v, want %v — a %d must %s",
					gotRefusal, tc.wantRefusal, tc.status,
					map[bool]string{true: "latch the sweep off", false: "stay transient and retry"}[tc.wantRefusal])
			}
		})
	}
}
