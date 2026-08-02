package payment

import (
	"context"
	"testing"
	"time"

	"github.com/stripe/stripe-go/v82"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// parkedFixture builds the exact ADR-107 parked shape, old enough for the
// ADR-108 sweep's age gate.
func parkedFixture(id string) domain.Invoice {
	return domain.Invoice{
		ID: id, TenantID: "t1", Status: domain.InvoiceFinalized,
		PaymentStatus: domain.PaymentUnknown, StripePaymentIntentID: "",
		AmountDueCents: 5000, InvoiceNumber: "INV-" + id,
		UpdatedAt: time.Now().UTC().Add(-2 * parkedSearchAge),
	}
}

// TestParkedSearch_SucceededPIWinsOverLive pins ADR-108's precedence: a
// succeeded PI settles the invoice PAID even when a live PI is also in the
// result set, and "newest first" within the succeeded class — the stale-history
// bias of an eventually-consistent index means older attempts' PIs are always
// the better-indexed ones, so an order-of-appearance loop would systematically
// pick the wrong PI (attack-verified).
func TestParkedSearch_SucceededPIWinsOverLive(t *testing.T) {
	store := newMockReconcileStore(parkedFixture("inv_1"))
	client := &mockStripeClient{searchResults: map[string][]PaymentIntentResult{
		"inv_1": {
			{ID: "pi_live", Status: "requires_action", CreatedAt: 300},
			{ID: "pi_succ_old", Status: "succeeded", CreatedAt: 100, AmountReceivedCents: 5000},
			{ID: "pi_succ_new", Status: "succeeded", CreatedAt: 200, AmountReceivedCents: 5000},
		},
	}}
	r := NewReconciler(client, store, time.Second)

	resolved, errs := r.Run(context.Background(), 10)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if resolved != 1 {
		t.Errorf("resolved: got %d, want 1", resolved)
	}
	got := store.byID["inv_1"]
	if got.Status != domain.InvoicePaid || got.PaymentStatus != domain.PaymentSucceeded {
		t.Errorf("invoice = %s/%s, want paid/succeeded — money moved and search found the proof", got.Status, got.PaymentStatus)
	}
	if got.StripePaymentIntentID != "pi_succ_new" {
		t.Errorf("settled against %q, want pi_succ_new — the NEWEST succeeded PI, not the best-indexed old one", got.StripePaymentIntentID)
	}
}

// TestParkedSearch_LivePIAdopted pins the adopt half: a live PI is CAS-stamped,
// the invoice joins the ordinary reconcilable population (processing WITH an
// id), and the seq moves exactly once — adoption records a NAMED attempt,
// which is ADR-105's condition for rotating the key seed.
func TestParkedSearch_LivePIAdopted(t *testing.T) {
	store := newMockReconcileStore(parkedFixture("inv_1"))
	store.byID["inv_1"].ChargeAttemptSeq = 7
	client := &mockStripeClient{searchResults: map[string][]PaymentIntentResult{
		"inv_1": {
			{ID: "pi_old_live", Status: "processing", CreatedAt: 100},
			{ID: "pi_new_live", Status: "requires_action", CreatedAt: 200},
		},
	}}
	r := NewReconciler(client, store, time.Second)

	resolved, errs := r.Run(context.Background(), 10)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if resolved != 1 {
		t.Errorf("resolved: got %d, want 1 (adoption counts — the invoice left the parked population)", resolved)
	}
	got := store.byID["inv_1"]
	if got.PaymentStatus != domain.PaymentProcessing || got.StripePaymentIntentID != "pi_new_live" {
		t.Errorf("invoice = %s/%q, want processing/pi_new_live — newest live PI adopted", got.PaymentStatus, got.StripePaymentIntentID)
	}
	if got.ChargeAttemptSeq != 8 {
		t.Errorf("seq = %d, want 8 — adopting a named PI must bump exactly once", got.ChargeAttemptSeq)
	}
}

// TestParkedSearch_AbsenceWritesNoMoneyOutcome is the design's core sentence as
// a test: terminal-only results and empty results both record an OBSERVATION
// and nothing else. No settle, no payment_status change, no seq movement — the
// deleted ADR-107 give-up write stays deleted (two attack-round BREAKS forced
// this; the reasoning lives in ADR-108).
func TestParkedSearch_AbsenceWritesNoMoneyOutcome(t *testing.T) {
	for _, tc := range []struct {
		name    string
		results []PaymentIntentResult
		wantObs string
	}{
		{"nothing found", nil, "search_not_found"},
		{"terminal-only found (stale history)", []PaymentIntentResult{
			{ID: "pi_declined_old", Status: "canceled", CreatedAt: 100},
		}, "search_terminal_only"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newMockReconcileStore(parkedFixture("inv_1"))
			store.byID["inv_1"].ChargeAttemptSeq = 7
			client := &mockStripeClient{searchResults: map[string][]PaymentIntentResult{"inv_1": tc.results}}
			r := NewReconciler(client, store, time.Second)

			resolved, errs := r.Run(context.Background(), 10)
			if len(errs) != 0 {
				t.Fatalf("unexpected errors: %v", errs)
			}
			if resolved != 0 {
				t.Errorf("resolved: got %d, want 0 — absence resolves nothing", resolved)
			}
			got := store.byID["inv_1"]
			if got.PaymentStatus != domain.PaymentUnknown || got.StripePaymentIntentID != "" || got.Status != domain.InvoiceFinalized {
				t.Errorf("invoice mutated to %s/%s/%q — absence from an eventually-consistent index must never write a money outcome", got.Status, got.PaymentStatus, got.StripePaymentIntentID)
			}
			if got.ChargeAttemptSeq != 7 {
				t.Errorf("seq moved to %d on absence — that rotates the idempotency key with no named attempt behind it", got.ChargeAttemptSeq)
			}
			if got.ProviderPaymentStatus != tc.wantObs {
				t.Errorf("observation = %q, want %q — the row must still be stamped or it heads the rotation queue forever", got.ProviderPaymentStatus, tc.wantObs)
			}
		})
	}
}

// TestParkedSearch_NotOfferedDisablesOnce pins the India-class degradation:
// the first ErrSearchNotOffered disables the sweep for that account+mode, and
// later ticks skip WITHOUT re-searching — announce-once, not a CRITICAL per
// tick (the log-spam shape #682 already fixed for the park announcement).
func TestParkedSearch_NotOfferedDisablesOnce(t *testing.T) {
	store := newMockReconcileStore(parkedFixture("inv_1"))
	client := &mockStripeClient{searchErr: ErrSearchNotOffered}
	r := NewReconciler(client, store, time.Second)

	if _, errs := r.Run(context.Background(), 10); len(errs) != 0 {
		t.Fatalf("a provider refusal must not surface as a sweep error (it is a documented account state): %v", errs)
	}
	firstCalls := len(client.searchCalls)
	if firstCalls != 1 {
		t.Fatalf("search calls after first tick: got %d, want 1", firstCalls)
	}

	if _, errs := r.Run(context.Background(), 10); len(errs) != 0 {
		t.Fatalf("unexpected errors on second tick: %v", errs)
	}
	if len(client.searchCalls) != firstCalls {
		t.Errorf("second tick searched again (%d calls) — a request-shape refusal will not heal; the account must be disabled after one sight", len(client.searchCalls))
	}
	// And the invoice keeps the pre-ADR-108 floor untouched.
	got := store.byID["inv_1"]
	if got.PaymentStatus != domain.PaymentUnknown || got.ProviderPaymentStatus != "" {
		t.Errorf("a not-offered tenant's parked invoice must keep exactly today's behavior; got %s / obs %q", got.PaymentStatus, got.ProviderPaymentStatus)
	}
}

// TestParkedSearch_TransientErrorStampsAndRetries: rate limits / 5xx stamp the
// row (an honest "we asked; the call failed" observation) so rotation keeps
// moving — an un-stamped erroring row would hold the head of the LIMITed queue
// forever, the exact starvation shape 0167 fixed for the sibling sweeps.
func TestParkedSearch_TransientErrorStampsAndRetries(t *testing.T) {
	store := newMockReconcileStore(parkedFixture("inv_1"))
	client := &mockStripeClient{searchErr: &stripe.Error{Type: stripe.ErrorTypeAPI, Msg: "upstream 500"}}
	r := NewReconciler(client, store, time.Second)

	if _, errs := r.Run(context.Background(), 10); len(errs) != 0 {
		t.Fatalf("a transient search failure must not surface as a sweep error: %v", errs)
	}
	got := store.byID["inv_1"]
	if got.ProviderPaymentStatus != "search_error" {
		t.Errorf("observation = %q, want search_error — the stamp is what rotates an erroring row to the back", got.ProviderPaymentStatus)
	}
	if got.PaymentStatus != domain.PaymentUnknown {
		t.Errorf("payment_status mutated to %s on a failed READ", got.PaymentStatus)
	}
	// Not disabled: a transient class must be retried after the cool-off.
	if r.searchDisabled["t1/test"] || r.searchDisabled["t1/live"] {
		t.Error("a transient error disabled the account — only the not-offered class may do that")
	}
}

// TestParkedSearch_AdoptRaceLostIsNotAnError: the webhook and the sweep both
// fire when the provider recovers, so losing the CAS is the designed outcome.
// The sweep must treat zero-rows-affected as "someone else resolved it" — no
// error, no retry-write, no overwrite of the winner's state.
func TestParkedSearch_AdoptRaceLostIsNotAnError(t *testing.T) {
	store := newMockReconcileStore(parkedFixture("inv_1"))
	// Simulate the webhook winning between list and adopt: the stored row is
	// already settled paid with a different PI.
	store.byID["inv_1"].Status = domain.InvoicePaid
	store.byID["inv_1"].PaymentStatus = domain.PaymentSucceeded
	store.byID["inv_1"].StripePaymentIntentID = "pi_webhook_won"
	// The sweep still saw the stale parked snapshot in its list; feed the
	// search result that would have adopted.
	stale := parkedFixture("inv_1")
	client := &mockStripeClient{searchResults: map[string][]PaymentIntentResult{
		"inv_1": {{ID: "pi_search_found", Status: "processing", CreatedAt: 100}},
	}}
	r := NewReconciler(client, store, time.Second)

	ok, err := r.searchAndAdoptOne(context.Background(), stale, "test")
	if err != nil {
		t.Fatalf("losing the adopt race must not be an error: %v", err)
	}
	if ok {
		t.Error("reported resolved after LOSING the CAS")
	}
	got := store.byID["inv_1"]
	if got.StripePaymentIntentID != "pi_webhook_won" || got.Status != domain.InvoicePaid {
		t.Errorf("the loser overwrote the winner: %s/%q", got.Status, got.StripePaymentIntentID)
	}
}
