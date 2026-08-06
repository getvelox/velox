package domain

import (
	"reflect"
	"strings"
	"testing"
)

// TestCustomer_CarriesNoCostDashboardCredential locks the show-once contract
// the API docs promise: the ONLY place the plaintext cost-dashboard token
// leaves the system is the rotate endpoint's own response payload.
//
// History, because it explains why this test is shaped the way it is. The
// credential originally lived on Customer WITH a json tag, so every
// authenticated customer GET/List re-disclosed it indefinitely while the spec
// said "shown ONCE — Velox never returns it again" (found by the 2026-07-19
// truth audit). The first fix was to mark it json:"-", and the test that
// replaced this one marshalled a Customer and asserted the token was absent
// from the bytes.
//
// That test could only fail AFTER someone re-added a json tag — it defended
// the serialization, not the invariant. Migration 0172 removed the field
// entirely (the DB now holds only a SHA-256 blind index, so nothing can
// hydrate a plaintext token onto this struct). This version asserts the
// stronger, earlier property: the struct carries no field capable of holding
// the credential at all — tagged, untagged, or otherwise. Re-introducing one
// fails here rather than at the next audit.
func TestCustomer_CarriesNoCostDashboardCredential(t *testing.T) {
	rt := reflect.TypeOf(Customer{})
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		name := strings.ToLower(f.Name)
		// The hash is a blind index, not a credential — a field holding it
		// would be fine. A field holding the token itself is not.
		if strings.Contains(name, "costdashboardtoken") && !strings.Contains(name, "hash") {
			t.Fatalf("domain.Customer.%s re-introduces the cost-dashboard credential on the customer struct — "+
				"every read that hydrates it becomes a disclosure path (the 2026-07-19 audit's finding). "+
				"Resolve tokens via customer.Store.GetByCostDashboardToken, which hashes before it compares.", f.Name)
		}
	}
}
