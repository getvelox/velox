package pricing_test

// ADR-130 §2 on real Postgres: a meter's aggregation is fixed once the meter
// has usage events (rows in usage_events for the meter). Held rows
// (usage_held_events) do not fix it: they have never been billed, and a
// release writes usage_events under the meter lock like an ingest. Before
// the first usage event the aggregation can change, whether or not a
// subscription uses the meter. The PATCH locks the meter row, so it waits
// for an ingest that is still writing the meter's first event (C5).

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
)

const codeMeterHasUsage = "meter_has_usage"

// wantHasUsage asserts err is the refusal of an aggregation change on a
// meter with events.
func wantHasUsage(t *testing.T, what string, err error) {
	t.Helper()
	if !errors.Is(err, errs.ErrInvalidState) || errs.Code(err) != codeMeterHasUsage {
		t.Fatalf("%s: want ErrInvalidState with code %s, got %v (code %q)", what, codeMeterHasUsage, err, errs.Code(err))
	}
}

// eventSQL is the two statements that write a customer and one usage event
// of meterID.
func (f meterEditFixture) eventSQL(meterID string) (string, []any, string, []any) {
	custID := postgres.NewID("vlx_cus")
	return `INSERT INTO customers (id, tenant_id, external_id, display_name) VALUES ($1, $2, $3, $3)`,
		[]any{custID, f.tenantID, "ext_" + custID},
		`INSERT INTO usage_events (id, tenant_id, customer_id, meter_id, quantity, properties, timestamp)
		 VALUES ($1, $2, $3, $4, 5, '{}', now())`,
		[]any{postgres.NewID("vlx_evt"), f.tenantID, custID, meterID}
}

// event commits one usage event of meterID.
func (f meterEditFixture) event(t *testing.T, meterID string) {
	t.Helper()
	tx := f.begin(t)
	defer postgres.Rollback(tx)
	cq, cargs, eq, eargs := f.eventSQL(meterID)
	if _, err := tx.ExecContext(f.ctx, cq, cargs...); err != nil {
		t.Fatalf("insert customer: %v", err)
	}
	if _, err := tx.ExecContext(f.ctx, eq, eargs...); err != nil {
		t.Fatalf("insert event: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit event: %v", err)
	}
}

// held commits one held usage row for eventName with the given status.
func (f meterEditFixture) held(t *testing.T, eventName, status string) {
	t.Helper()
	tx := f.begin(t)
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(f.ctx, `
		INSERT INTO usage_held_events (id, tenant_id, source, external_customer_id, event_name, quantity,
			idempotency_key, event_at, reason, status)
		VALUES ($1, $2, 'litellm', 'ext_not_yet', $3, 5, $4, now(), 'customer_not_found', $5)
	`, postgres.NewID("vlx_hue"), f.tenantID, eventName, postgres.NewID("idem"), status); err != nil {
		t.Fatalf("insert held row: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit held row: %v", err)
	}
}

func TestMeterAggregation_FixedOnceTheMeterHasEvents(t *testing.T) {
	f := newMeterEditFixture(t)

	t.Run("an event of the meter: refused, and the aggregation is unchanged", func(t *testing.T) {
		m := f.meter(t, "agg_events", "")
		f.event(t, m.ID)
		_, err := f.svc.UpdateMeter(f.ctx, f.tenantID, m.ID, pricing.UpdateMeterInput{Aggregation: ptr("max")})
		wantHasUsage(t, "aggregation change with an event", err)
		if got := f.getMeter(t, m.ID); got.Aggregation != "sum" {
			t.Errorf("aggregation = %q, want sum", got.Aggregation)
		}
	})

	// Mutation-verify: count held rows in meterHasUsageSQL again → refused,
	// and this fails.
	t.Run("held rows for the meter's event name do not count", func(t *testing.T) {
		m := f.meter(t, "agg_held", "")
		f.held(t, "agg_held", "held")
		got, err := f.svc.UpdateMeter(f.ctx, f.tenantID, m.ID, pricing.UpdateMeterInput{Aggregation: ptr("last")})
		if err != nil {
			t.Fatalf("aggregation change with only held rows: %v, want it changed: held rows have never been billed", err)
		}
		if got.Aggregation != "last" {
			t.Errorf("aggregation = %q, want last", got.Aggregation)
		}
	})

	t.Run("released or discarded held rows do not count", func(t *testing.T) {
		m := f.meter(t, "agg_held_done", "")
		f.held(t, "agg_held_done", "released")
		f.held(t, "agg_held_done", "discarded")
		if _, err := f.svc.UpdateMeter(f.ctx, f.tenantID, m.ID, pricing.UpdateMeterInput{Aggregation: ptr("count")}); err != nil {
			t.Fatalf("aggregation change with only released and discarded held rows: %v", err)
		}
	})

	t.Run("another meter's events do not count", func(t *testing.T) {
		other := f.meter(t, "agg_other", "")
		f.event(t, other.ID)
		m := f.meter(t, "agg_quiet", "")
		if _, err := f.svc.UpdateMeter(f.ctx, f.tenantID, m.ID, pricing.UpdateMeterInput{Aggregation: ptr("max")}); err != nil {
			t.Fatalf("aggregation change on a meter with no events: %v", err)
		}
	})

	t.Run("with events, a rename, a unit change and the same aggregation pass", func(t *testing.T) {
		m := f.meter(t, "agg_rename", "")
		f.event(t, m.ID)
		got, err := f.svc.UpdateMeter(f.ctx, f.tenantID, m.ID, pricing.UpdateMeterInput{
			Name: ptr("Renamed"), Unit: ptr("requests"), Aggregation: ptr("sum"),
		})
		if err != nil {
			t.Fatalf("rename with events: %v", err)
		}
		if got.Name != "Renamed" || got.Unit != "requests" {
			t.Errorf("got name %q unit %q", got.Name, got.Unit)
		}
	})
}

// Before any event the aggregation is the operator's to change, even while
// subscriptions use the meter: no usage has been counted under the old one.
func TestMeterAggregation_ChangesBeforeAnyEvent(t *testing.T) {
	f := newMeterEditFixture(t)
	for _, status := range []string{"draft", "trialing", "active"} {
		t.Run("a "+status+" subscription uses the meter", func(t *testing.T) {
			price := f.rule(t, "agg_before_price_"+status, 1)
			m := f.meter(t, "agg_before_"+status, price.ID)
			p := f.plan(t, "agg_before_plan_"+status, m.ID)
			f.sub(t, p.ID, status)
			got, err := f.svc.UpdateMeter(f.ctx, f.tenantID, m.ID, pricing.UpdateMeterInput{Aggregation: ptr("max")})
			if err != nil {
				t.Fatalf("aggregation change before any event: %v", err)
			}
			if got.Aggregation != "max" {
				t.Errorf("aggregation = %q, want max", got.Aggregation)
			}
		})
	}
}

// C5: an aggregation PATCH against the meter's first ingest. The PATCH locks
// the meter row FOR UPDATE, which waits for the ingest's foreign-key lock on
// the meter, so it sees the event once the ingest commits and refuses. In the
// other order the PATCH commits first and the event is counted under the new
// aggregation. Either way no event is counted under an aggregation that
// changed after it was written.
//
// Mutation: drop the meter lock and the PATCH does not wait; it sees no event
// and changes the aggregation of a meter whose event then commits.
func TestMeterAggregation_PatchAgainstTheFirstIngest(t *testing.T) {
	f := newMeterEditFixture(t)

	t.Run("the ingest writes first: the PATCH waits, then refuses", func(t *testing.T) {
		m := f.meter(t, "agg_race_ingest", "")
		ingestTx := f.begin(t)
		defer postgres.Rollback(ingestTx)
		cq, cargs, eq, eargs := f.eventSQL(m.ID)
		if _, err := ingestTx.ExecContext(f.ctx, cq, cargs...); err != nil {
			t.Fatalf("insert customer: %v", err)
		}
		if _, err := ingestTx.ExecContext(f.ctx, eq, eargs...); err != nil {
			t.Fatalf("insert event: %v", err)
		}

		done := make(chan error, 1)
		go func() {
			_, err := f.svc.UpdateMeter(f.ctx, f.tenantID, m.ID, pricing.UpdateMeterInput{Aggregation: ptr("max")})
			done <- err
		}()
		select {
		case err := <-done:
			t.Fatalf("the PATCH must wait for the ingest still writing; it returned %v", err)
		case <-time.After(500 * time.Millisecond):
		}
		if err := ingestTx.Commit(); err != nil {
			t.Fatalf("commit ingest: %v", err)
		}
		select {
		case err := <-done:
			wantHasUsage(t, "PATCH after the first event commits", err)
		case <-time.After(10 * time.Second):
			t.Fatal("the PATCH never finished")
		}
		if got := f.getMeter(t, m.ID); got.Aggregation != "sum" {
			t.Errorf("aggregation = %q, want sum", got.Aggregation)
		}
	})

	t.Run("the PATCH commits first: the event counts under the new aggregation", func(t *testing.T) {
		m := f.meter(t, "agg_race_patch", "")
		if _, err := f.svc.UpdateMeter(f.ctx, f.tenantID, m.ID, pricing.UpdateMeterInput{Aggregation: ptr("max")}); err != nil {
			t.Fatalf("PATCH before any event: %v", err)
		}
		f.event(t, m.ID)
		if got := f.getMeter(t, m.ID); got.Aggregation != "max" {
			t.Errorf("aggregation = %q, want max", got.Aggregation)
		}
		_, err := f.svc.UpdateMeter(f.ctx, f.tenantID, m.ID, pricing.UpdateMeterInput{Aggregation: ptr("sum")})
		wantHasUsage(t, "PATCH after the event", err)
	})
}

// What an API caller sees: 409 with code meter_has_usage, and GET says
// has_usage before the edit, so the dashboard can show the lock up front.
func TestMeterAggregation_HTTP(t *testing.T) {
	f := newMeterEditFixture(t)
	h := pricing.NewHandler(f.svc)
	r := chi.NewRouter()
	r.Mount("/v1/meters", h.MeterRoutes())
	do := func(method, path, body string) (int, map[string]any) {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithTenantID(f.ctx, f.tenantID))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	m := f.meter(t, "agg_http", "")
	if code, body := do(http.MethodGet, "/v1/meters/"+m.ID, ""); code != http.StatusOK || body["has_usage"] != false {
		t.Fatalf("GET before any event: %d %v, want 200 with has_usage false", code, body)
	}
	f.held(t, "agg_http", "held")
	if code, body := do(http.MethodGet, "/v1/meters/"+m.ID, ""); code != http.StatusOK || body["has_usage"] != false {
		t.Fatalf("GET with only a held row: %d %v, want 200 with has_usage false", code, body)
	}
	f.event(t, m.ID)
	if code, body := do(http.MethodGet, "/v1/meters/"+m.ID, ""); code != http.StatusOK || body["has_usage"] != true || body["event_name"] != "agg_http" {
		t.Fatalf("GET after an event: %d %v, want 200 with has_usage true and event_name agg_http", code, body)
	}
	code, body := do(http.MethodPatch, "/v1/meters/"+m.ID, `{"aggregation":"max"}`)
	errBody, _ := body["error"].(map[string]any)
	if code != http.StatusConflict || errBody["code"] != codeMeterHasUsage {
		t.Fatalf("PATCH aggregation with usage: %d %v, want 409 %s", code, body, codeMeterHasUsage)
	}
	if code, body := do(http.MethodPatch, "/v1/meters/"+m.ID, `{"name":"Renamed"}`); code != http.StatusOK || body["name"] != "Renamed" || body["has_usage"] != true {
		t.Fatalf("PATCH name with usage: %d %v, want 200 renamed with has_usage true", code, body)
	}
}
