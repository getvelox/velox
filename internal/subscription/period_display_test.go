package subscription

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
)

// fakeTenantLocator stands in for the injected org billing-timezone resolver:
// it reports the ONE timezone the tenant bills in (ADR-077). The
// backend-authored period display must render in exactly this zone.
type fakeTenantLocator struct{ loc *time.Location }

func (f fakeTenantLocator) TenantLocation(_ context.Context, _ string) (*time.Location, error) {
	return f.loc, nil
}

// TestHandler_Get_PeriodDisplayTZ is the display half of ADR-077: the
// backend-authored current_billing_period_display is anchored in the ORG's
// billing timezone (whatever the tenant locator reports), not the process/host
// zone. Subscriptions carry no per-sub timezone — the org setting is the single
// source, so the SAME half-open instants render differently under two org zones.
//
// The half-open period is [May 1 18:30 UTC, May 31 18:30 UTC). Rendered in
// Asia/Kolkata it reads "May 2 – May 31" (those instants are May 2 00:00 /
// Jun 1 00:00 IST, inclusive end Jun 1 − 1 day = May 31); rendered in
// America/New_York the same instants land a full calendar day earlier —
// "May 1 – May 30". The two strings differ, so the assertion cannot pass
// vacuously.
//
// Mutation-verify: change stampPeriodDisplay to pass time.UTC instead of
// h.tenantLoc(...) — the Kolkata case then renders "May 1 – May 31" and fails.
func TestHandler_Get_PeriodDisplayTZ(t *testing.T) {
	ctx := context.Background()
	tenantID := "t1"

	// May 1 18:30 UTC = May 2 00:00 IST; May 31 18:30 UTC = Jun 1 00:00 IST.
	start := time.Date(2026, 5, 1, 18, 30, 0, 0, time.UTC)
	end := time.Date(2026, 5, 31, 18, 30, 0, 0, time.UTC)

	kolkata, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatalf("load Kolkata: %v", err)
	}
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("load NY: %v", err)
	}

	cases := []struct {
		name  string
		orgTZ *time.Location // the tenant's (org) billing timezone
		want  string
	}{
		{"org TZ east of UTC", kolkata, "May 2, 2026 – May 31, 2026"},
		{"org TZ west of UTC", ny, "May 1, 2026 – May 30, 2026"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newMemStore()
			subID, _ := seedSubWithItemAt(t, store, tenantID, "cus_1", "plan_basic", start, end)

			h := NewHandler(NewService(store, nil))
			h.SetTenantLocator(fakeTenantLocator{loc: tc.orgTZ})

			req := httptest.NewRequest(http.MethodGet, "/subscriptions/"+subID, nil)
			rctx := chi.NewRouteContext()
			rctx.URLParams.Add("id", subID)
			req = req.WithContext(context.WithValue(
				context.WithValue(ctx, chi.RouteCtxKey, rctx),
				auth.TestTenantIDKey(), tenantID,
			))

			rr := httptest.NewRecorder()
			h.get(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("status: got %d, want 200. body=%s", rr.Code, rr.Body.String())
			}

			var got domain.Subscription
			if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got.CurrentBillingPeriodDisplay != tc.want {
				t.Errorf("current_billing_period_display: got %q, want %q", got.CurrentBillingPeriodDisplay, tc.want)
			}
		})
	}
}
