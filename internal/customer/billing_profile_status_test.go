package customer

import (
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// profile_status is a verdict about the stored data, not a caller
// assertion. Before this was derived, PUT {profile_status:'ready'} on a
// profile with no legal name and no address was accepted and echoed back
// as "ready", and an unknown value fell through to the DB CHECK as a 500.
func TestDeriveProfileStatus(t *testing.T) {
	complete := domain.CustomerBillingProfile{
		LegalName:    "U1 Failed-Pay GmbH",
		AddressLine1: "Friedrichstrasse 1",
		City:         "Berlin",
		Country:      "DE",
	}

	t.Run("all bill-to fields present is ready", func(t *testing.T) {
		if got := deriveProfileStatus(complete); got != domain.BillingProfileReady {
			t.Fatalf("profile status = %q, want %q", got, domain.BillingProfileReady)
		}
	})

	// One subtest per field: a single loop asserting "any blank field is
	// incomplete" would still pass if the implementation only ever checked
	// LegalName and ignored the other three.
	blanking := map[string]func(*domain.CustomerBillingProfile){
		"legal_name":    func(p *domain.CustomerBillingProfile) { p.LegalName = "" },
		"address_line1": func(p *domain.CustomerBillingProfile) { p.AddressLine1 = "" },
		"city":          func(p *domain.CustomerBillingProfile) { p.City = "" },
		"country":       func(p *domain.CustomerBillingProfile) { p.Country = "" },
	}
	for field, blank := range blanking {
		t.Run("blank "+field+" is incomplete", func(t *testing.T) {
			p := complete
			blank(&p)
			if got := deriveProfileStatus(p); got != domain.BillingProfileIncomplete {
				t.Fatalf("with %s blank: profile status = %q, want %q", field, got, domain.BillingProfileIncomplete)
			}
		})
	}

	t.Run("whitespace-only does not satisfy a field", func(t *testing.T) {
		p := complete
		p.City = "   "
		if got := deriveProfileStatus(p); got != domain.BillingProfileIncomplete {
			t.Fatalf("whitespace city: profile status = %q, want %q", got, domain.BillingProfileIncomplete)
		}
	})

	// Neither is printed for every jurisdiction — Hong Kong and the UAE
	// issue no postal codes, and most countries have no state — so a
	// profile without them is still complete.
	t.Run("postal_code and state stay optional", func(t *testing.T) {
		p := complete
		p.PostalCode = ""
		p.State = ""
		if got := deriveProfileStatus(p); got != domain.BillingProfileReady {
			t.Fatalf("no postal/state: profile status = %q, want %q", got, domain.BillingProfileReady)
		}
	})

	// tax_id is only mandatory under reverse_charge, which
	// UpsertBillingProfile rejects outright, so it is not part of the
	// completeness set.
	t.Run("missing tax_id does not block ready", func(t *testing.T) {
		p := complete
		p.TaxID = ""
		p.TaxIDType = ""
		if got := deriveProfileStatus(p); got != domain.BillingProfileReady {
			t.Fatalf("no tax id: profile status = %q, want %q", got, domain.BillingProfileReady)
		}
	})

	// The caller-supplied value must not survive. This is the case that
	// used to let a client label an empty profile "ready".
	t.Run("caller-asserted status is ignored", func(t *testing.T) {
		empty := domain.CustomerBillingProfile{ProfileStatus: domain.BillingProfileReady}
		if got := deriveProfileStatus(empty); got != domain.BillingProfileIncomplete {
			t.Fatalf("empty profile asserting ready: profile status = %q, want %q", got, domain.BillingProfileIncomplete)
		}
		asserted := complete
		asserted.ProfileStatus = "banana"
		if got := deriveProfileStatus(asserted); got != domain.BillingProfileReady {
			t.Fatalf("complete profile asserting garbage: profile status = %q, want %q", got, domain.BillingProfileReady)
		}
	})
}
