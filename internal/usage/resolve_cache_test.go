package usage

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// countingCustomer returns a distinct internal ID per external ID and counts
// store round trips. Distinctness is the point: a cache that keys wrongly
// would resolve every event in a batch to the FIRST customer — billing every
// event to one account — and a call-count-only fake could not see that.
type countingCustomer struct{ calls int }

func (c *countingCustomer) GetByExternalID(_ context.Context, _, externalID string) (domain.Customer, error) {
	c.calls++
	return domain.Customer{ID: "cus_" + externalID}, nil
}

type countingMeter struct{ calls int }

func (m *countingMeter) GetMeterByKey(_ context.Context, _, key string) (domain.Meter, error) {
	m.calls++
	return domain.Meter{ID: "mtr_" + key}, nil
}

func evt(extCust, eventName string) apiEvent {
	return apiEvent{
		ExternalCustomerID: extCust,
		EventName:          eventName,
		Quantity:           decimal.NewFromInt(1),
	}
}

// TestResolveCache covers the batch-ingest lookup memo. Before it, a batch of
// N events cost 2N lookups — each its own RLS transaction — even when every
// event named the same customer and meter.
func TestResolveCache(t *testing.T) {
	ctx := context.Background()

	t.Run("repeated customer and meter cost one lookup each", func(t *testing.T) {
		cust, meter := &countingCustomer{}, &countingMeter{}
		h := NewHandler(NewService(newMemStore()), cust, meter)
		cache := newResolveCache()

		for i := 0; i < 50; i++ {
			in, err := h.resolve(ctx, "t1", evt("acme", "api_call"), cache)
			if err != nil {
				t.Fatalf("resolve[%d]: %v", i, err)
			}
			if in.CustomerID != "cus_acme" || in.MeterID != "mtr_api_call" {
				t.Fatalf("resolve[%d]: got (%s, %s), want (cus_acme, mtr_api_call)", i, in.CustomerID, in.MeterID)
			}
		}

		if cust.calls != 1 {
			t.Errorf("customer lookups = %d, want 1", cust.calls)
		}
		if meter.calls != 1 {
			t.Errorf("meter lookups = %d, want 1", meter.calls)
		}
	})

	// The correctness half: caching must not collapse distinct identifiers
	// onto one another. Interleaved so a naive "remember the last one" cache
	// fails here even though the call counts would look right.
	t.Run("distinct identifiers resolve distinctly", func(t *testing.T) {
		cust, meter := &countingCustomer{}, &countingMeter{}
		h := NewHandler(NewService(newMemStore()), cust, meter)
		cache := newResolveCache()

		events := []apiEvent{
			evt("acme", "api_call"),
			evt("globex", "tokens"),
			evt("acme", "tokens"),
			evt("globex", "api_call"),
			evt("acme", "api_call"),
		}
		want := [][2]string{
			{"cus_acme", "mtr_api_call"},
			{"cus_globex", "mtr_tokens"},
			{"cus_acme", "mtr_tokens"},
			{"cus_globex", "mtr_api_call"},
			{"cus_acme", "mtr_api_call"},
		}

		for i, e := range events {
			in, err := h.resolve(ctx, "t1", e, cache)
			if err != nil {
				t.Fatalf("resolve[%d]: %v", i, err)
			}
			if in.CustomerID != want[i][0] || in.MeterID != want[i][1] {
				t.Errorf("resolve[%d]: got (%s, %s), want (%s, %s)",
					i, in.CustomerID, in.MeterID, want[i][0], want[i][1])
			}
		}

		// Two distinct customers, two distinct meters — one lookup apiece.
		if cust.calls != 2 {
			t.Errorf("customer lookups = %d, want 2", cust.calls)
		}
		if meter.calls != 2 {
			t.Errorf("meter lookups = %d, want 2", meter.calls)
		}
	})

	// The single-event doors pass nil. Nothing may be carried between
	// requests, so every call must go to the store.
	t.Run("nil cache never memoizes", func(t *testing.T) {
		cust, meter := &countingCustomer{}, &countingMeter{}
		h := NewHandler(NewService(newMemStore()), cust, meter)

		for i := 0; i < 3; i++ {
			if _, err := h.resolve(ctx, "t1", evt("acme", "api_call"), nil); err != nil {
				t.Fatalf("resolve[%d]: %v", i, err)
			}
		}

		if cust.calls != 3 {
			t.Errorf("customer lookups = %d, want 3", cust.calls)
		}
		if meter.calls != 3 {
			t.Errorf("meter lookups = %d, want 3", meter.calls)
		}
	})
}

// failingCustomer fails only for one external ID, so a later event naming a
// GOOD customer must still resolve — the negative verdict is not cached.
type failingCustomer struct {
	calls int
	bad   string
}

func (c *failingCustomer) GetByExternalID(_ context.Context, _, externalID string) (domain.Customer, error) {
	c.calls++
	if externalID == c.bad {
		return domain.Customer{}, errNotFoundForTest
	}
	return domain.Customer{ID: "cus_" + externalID}, nil
}

var errNotFoundForTest = &notFoundErr{}

type notFoundErr struct{}

func (*notFoundErr) Error() string { return "not found" }

func TestResolveCache_FailuresAreNotCached(t *testing.T) {
	ctx := context.Background()
	cust := &failingCustomer{bad: "ghost"}
	h := NewHandler(NewService(newMemStore()), cust, &countingMeter{})
	cache := newResolveCache()

	if _, err := h.resolve(ctx, "t1", evt("ghost", "api_call"), cache); err == nil {
		t.Fatal("expected unknown customer to be rejected")
	}
	// Retrying the same bad ID must re-query rather than serve a cached
	// verdict: the cache holds resolved IDs only.
	if _, err := h.resolve(ctx, "t1", evt("ghost", "api_call"), cache); err == nil {
		t.Fatal("expected second attempt to be rejected too")
	}
	if cust.calls != 2 {
		t.Errorf("customer lookups = %d, want 2 (failures not cached)", cust.calls)
	}

	in, err := h.resolve(ctx, "t1", evt("acme", "api_call"), cache)
	if err != nil {
		t.Fatalf("good customer after a failure: %v", err)
	}
	if in.CustomerID != "cus_acme" {
		t.Errorf("CustomerID = %s, want cus_acme", in.CustomerID)
	}
}
