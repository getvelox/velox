package usage

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
)

type stubCustomer struct{}

func (stubCustomer) GetByExternalID(_ context.Context, _, _ string) (domain.Customer, error) {
	return domain.Customer{ID: "cus_1"}, nil
}

type stubMeter struct{}

func (stubMeter) GetMeterByKey(_ context.Context, _, _ string) (domain.Meter, error) {
	return domain.Meter{ID: "mtr_1"}, nil
}

func rawMsg(s string) *json.RawMessage {
	m := json.RawMessage(s)
	return &m
}

// TestResolve_MalformedTimestampRejected covers the medium-severity audit
// finding: a non-nil but malformed/non-string timestamp was silently
// discarded and the event stamped at wall-clock now — back-dating or
// future-dating usage into the wrong billing period with no signal to the
// caller. A sent timestamp must parse or the ingest is rejected, matching
// the Backfill and getSummary paths.
func TestResolve_MalformedTimestampRejected(t *testing.T) {
	h := NewHandler(NewService(newMemStore()), stubCustomer{}, stubMeter{})
	ctx := context.Background()

	base := apiEvent{ExternalCustomerID: "cus_ext", EventName: "api_call", Quantity: decimal.NewFromInt(1)}

	t.Run("valid RFC3339 honored", func(t *testing.T) {
		evt := base
		evt.Timestamp = rawMsg(`"2026-03-15T10:00:00Z"`)
		in, err := h.resolve(ctx, "t1", evt, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if in.Timestamp == nil || in.Timestamp.Year() != 2026 || in.Timestamp.Month() != 3 {
			t.Errorf("timestamp not parsed: %v", in.Timestamp)
		}
	})

	t.Run("rejected", func(t *testing.T) {
		cases := map[string]*json.RawMessage{
			"non-string number":  rawMsg(`1716000000`),
			"non-string object":  rawMsg(`{"t":1}`),
			"unparseable string": rawMsg(`"not-a-date"`),
			"empty string":       rawMsg(`""`),
		}
		for name, ts := range cases {
			t.Run(name, func(t *testing.T) {
				evt := base
				evt.Timestamp = ts
				_, err := h.resolve(ctx, "t1", evt, nil)
				if err == nil {
					t.Fatalf("expected rejection, got nil error")
				}
				if !errors.Is(err, errs.ErrValidation) {
					t.Errorf("expected validation error, got %v", err)
				}
				if errs.Field(err) != "timestamp" {
					t.Errorf("expected field=timestamp, got %q", errs.Field(err))
				}
			})
		}
	})

	t.Run("absent timestamp ok", func(t *testing.T) {
		evt := base // Timestamp nil
		in, err := h.resolve(ctx, "t1", evt, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if in.Timestamp != nil {
			t.Errorf("expected nil timestamp, got %v", in.Timestamp)
		}
	})
}
