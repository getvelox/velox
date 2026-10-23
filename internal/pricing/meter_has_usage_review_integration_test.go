package pricing_test

// Review findings on PR #1376 (round 1), on real Postgres.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/pricing"
)

// The 409 meter_has_usage message says what holds the aggregation: the
// meter's usage events, counted under it. Held rows do not hold it (ADR-130
// §2, amended 2026-10-24), so there is one message.
func TestMeterAggregation_RefusalSaysWhatHoldsIt(t *testing.T) {
	f := newMeterEditFixture(t)
	m := f.meter(t, "agg_msg_events", "")
	f.event(t, m.ID)
	f.held(t, "agg_msg_events", "held")
	_, err := f.svc.UpdateMeter(f.ctx, f.tenantID, m.ID, pricing.UpdateMeterInput{Aggregation: ptr("max")})
	wantHasUsage(t, "usage events", err)
	msg := err.Error()
	for _, want := range []string{"usage events", "sum", "new meter"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message does not say %q: %s", want, msg)
		}
	}
	if strings.Contains(msg, "held") {
		t.Errorf("message names held rows, which do not fix the aggregation: %s", msg)
	}
}

// hasUsageFails is the real store, except that the has_usage read fails.
type hasUsageFails struct {
	*pricing.PostgresStore
}

func (hasUsageFails) MeterHasUsage(context.Context, string, domain.Meter) (bool, error) {
	return false, errors.New("has_usage read failed")
}

// POST and PATCH /v1/meters/{id} read has_usage after the write committed.
// When that read fails the write still happened, so the answer is the write's
// own status with the meter, and has_usage is left out rather than guessed.
// Before, the answer was a 500: a client that retried the create got 409
// "already exists" for the meter its first call made. GET writes nothing, so
// a failed read there is still a 500.
//
// Mutation-verify: answer 500 again when the read fails → the POST and PATCH
// checks fail.
func TestMeter_HasUsageReadFailsAfterTheWrite(t *testing.T) {
	f := newMeterEditFixture(t)
	svc := pricing.NewService(hasUsageFails{pricing.NewPostgresStore(f.db)})
	r := chi.NewRouter()
	r.Mount("/v1/meters", pricing.NewHandler(svc).MeterRoutes())
	do := func(method, path, body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithTenantID(f.ctx, f.tenantID))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	code, body := do(http.MethodPost, "/v1/meters", `{"event_name":"hu_read","name":"HU","aggregation":"sum"}`)
	id, _ := body["id"].(string)
	if code != http.StatusCreated || id == "" || body["event_name"] != "hu_read" {
		t.Fatalf("POST with a failing has_usage read: %d %v, want 201 with the meter", code, body)
	}
	if _, ok := body["has_usage"]; ok {
		t.Errorf("POST: has_usage = %v, want it left out when it could not be read", body["has_usage"])
	}
	if got := f.getMeter(t, id); got.EventName != "hu_read" {
		t.Fatalf("the meter was not created: %+v", got)
	}

	code, body = do(http.MethodPatch, "/v1/meters/"+id, `{"name":"Renamed"}`)
	if code != http.StatusOK || body["name"] != "Renamed" {
		t.Fatalf("PATCH with a failing has_usage read: %d %v, want 200 renamed", code, body)
	}
	if _, ok := body["has_usage"]; ok {
		t.Errorf("PATCH: has_usage = %v, want it left out when it could not be read", body["has_usage"])
	}

	if code, body := do(http.MethodGet, "/v1/meters/"+id, ""); code != http.StatusInternalServerError {
		t.Errorf("GET with a failing has_usage read: %d %v, want 500", code, body)
	}
}
