package customer

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
)

// fakeSubChecker satisfies SubscriptionChecker with a canned answer.
type fakeSubChecker struct {
	subs []NonTerminalSubscription
	err  error
}

func (f *fakeSubChecker) NonTerminalForCustomer(_ context.Context, _, _ string, _ bool) ([]NonTerminalSubscription, error) {
	return f.subs, f.err
}

func newArchiveTestSvc(t *testing.T, checker SubscriptionChecker) (*Service, domain.Customer) {
	t.Helper()
	svc := NewService(newMemoryStore())
	if checker != nil {
		svc.SetSubscriptionChecker(checker)
	}
	cust, err := svc.Create(context.Background(), "t1", CreateInput{
		ExternalID: "cus_guard", DisplayName: "Guard", Email: "g@example.test",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return svc, cust
}

// TestArchive_BlockedWhileSubsBill locks ADR-067: archive is a 409-BLOCK
// while ANY subscription still bills (active / trialing / active-with-
// scheduled-cancel), never an auto-cancel. Mutation seam: remove the checker
// call in Update and every blocked case silently archives.
func TestArchive_BlockedWhileSubsBill(t *testing.T) {
	for _, status := range []string{"active", "trialing", "active(scheduled-cancel)"} {
		t.Run(status, func(t *testing.T) {
			svc, cust := newArchiveTestSvc(t, &fakeSubChecker{subs: []NonTerminalSubscription{{ID: "sub_1", Status: status}}})
			_, err := svc.Update(context.Background(), "t1", cust.ID, UpdateInput{Status: "archived"})
			if !errors.Is(err, errs.ErrInvalidState) {
				t.Fatalf("archive with %s sub: err = %v, want InvalidState (409)", status, err)
			}
			if !strings.Contains(err.Error(), "sub_1") {
				t.Errorf("409 message must name the blocking subscription: %q", err.Error())
			}
			got, _ := svc.Get(context.Background(), "t1", cust.ID)
			if got.Status == domain.CustomerStatusArchived {
				t.Fatal("customer archived despite billing subscriptions")
			}
		})
	}
}

// TestArchive_AllowedWhenOnlyTerminalSubs: canceled-only (checker reports
// nothing non-terminal) → archive succeeds; unarchive always succeeds.
func TestArchive_AllowedWhenOnlyTerminalSubs(t *testing.T) {
	svc, cust := newArchiveTestSvc(t, &fakeSubChecker{})
	upd, err := svc.Update(context.Background(), "t1", cust.ID, UpdateInput{Status: "archived"})
	if err != nil {
		t.Fatalf("archive with no billing subs: %v", err)
	}
	if upd.Status != domain.CustomerStatusArchived {
		t.Fatalf("status = %s, want archived", upd.Status)
	}
	// Unarchive is never guarded.
	upd, err = svc.Update(context.Background(), "t1", cust.ID, UpdateInput{Status: "active"})
	if err != nil || upd.Status != domain.CustomerStatusActive {
		t.Fatalf("unarchive: err=%v status=%s", err, upd.Status)
	}
}

// TestArchive_FailsClosedWithoutChecker: an unwired checker must refuse the
// archive rather than silently permit one that keeps billing.
func TestArchive_FailsClosedWithoutChecker(t *testing.T) {
	svc, cust := newArchiveTestSvc(t, nil)
	_, err := svc.Update(context.Background(), "t1", cust.ID, UpdateInput{Status: "archived"})
	if err == nil {
		t.Fatal("archive without a wired checker must fail closed")
	}
}

// TestBillingProfileCurrency_Guard locks the ADR-067 currency rules: format
// validation + normalization always; mismatch vs a billing subscription's
// plan currency → 409.
func TestBillingProfileCurrency_Guard(t *testing.T) {
	// Mismatch: active sub priced in USD, profile wants EUR.
	svc, cust := newArchiveTestSvc(t, &fakeSubChecker{subs: []NonTerminalSubscription{
		{ID: "sub_1", Status: "active", PlanCurrencies: []string{"USD"}},
	}})
	_, err := svc.UpsertBillingProfile(context.Background(), "t1", domain.CustomerBillingProfile{
		CustomerID: cust.ID, Currency: "EUR",
	})
	if !errors.Is(err, errs.ErrInvalidState) {
		t.Fatalf("EUR profile over USD plan: err = %v, want InvalidState (silent re-denomination)", err)
	}

	// Match (case-normalized): "usd" → stored "USD", allowed.
	bp, err := svc.UpsertBillingProfile(context.Background(), "t1", domain.CustomerBillingProfile{
		CustomerID: cust.ID, Currency: "usd",
	})
	if err != nil {
		t.Fatalf("matching currency rejected: %v", err)
	}
	if bp.Currency != "USD" {
		t.Errorf("currency stored as %q, want canonical UPPERCASE USD", bp.Currency)
	}

	// Format: garbage code → 400 regardless of subs.
	_, err = svc.UpsertBillingProfile(context.Background(), "t1", domain.CustomerBillingProfile{
		CustomerID: cust.ID, Currency: "EURO",
	})
	if !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("invalid code EURO: err = %v, want validation error", err)
	}
}
