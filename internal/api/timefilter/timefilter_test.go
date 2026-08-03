package timefilter

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestParseRange(t *testing.T) {
	cases := []struct {
		name        string
		url         string
		wantFrom    string // RFC3339 expected; "" = zero
		wantTo      string
		wantErrCode string // errs.Invalid field name; "" = no error
	}{
		{
			name:     "both empty",
			url:      "/x",
			wantFrom: "",
			wantTo:   "",
		},
		{
			name:     "RFC3339 both ends",
			url:      "/x?from=2026-01-01T00:00:00Z&to=2026-12-31T23:59:59Z",
			wantFrom: "2026-01-01T00:00:00Z",
			wantTo:   "2026-12-31T23:59:59Z",
		},
		{
			name:     "date-only from anchors at midnight UTC",
			url:      "/x?from=2026-01-01",
			wantFrom: "2026-01-01T00:00:00Z",
		},
		{
			name:   "date-only to anchors at end-of-day UTC",
			url:    "/x?to=2026-01-31",
			wantTo: "2026-01-31T23:59:59Z",
		},
		{
			name:     "mixed RFC3339 and date-only",
			url:      "/x?from=2026-01-01T08:00:00Z&to=2026-01-31",
			wantFrom: "2026-01-01T08:00:00Z",
			wantTo:   "2026-01-31T23:59:59Z",
		},
		{
			name:        "invalid from",
			url:         "/x?from=not-a-date",
			wantErrCode: "from",
		},
		{
			name:        "invalid to",
			url:         "/x?to=garbage",
			wantErrCode: "to",
		},
		{
			name:        "to not after from",
			url:         "/x?from=2026-06-01&to=2026-05-01",
			wantErrCode: "to",
		},
		{
			// Equal endpoints are a VALID zero-width window: every consumer's
			// SQL is inclusive both ends, so from==to selects rows at exactly
			// that instant — the natural query on the quantized sim axis
			// (FLOW P2 walk, 2026-08-03: the flow's own single-instant example
			// 422'd against the previous stricter check).
			name:     "to equal to from is a valid single-instant window",
			url:      "/x?from=2026-06-01T00:00:00Z&to=2026-06-01T00:00:00Z",
			wantFrom: "2026-06-01T00:00:00Z",
			wantTo:   "2026-06-01T00:00:00Z",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", tc.url, nil)
			from, to, err := ParseRange(r, "from", "to")
			if tc.wantErrCode != "" {
				if err == nil {
					t.Fatalf("expected error on field %q; got nil", tc.wantErrCode)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			gotFrom := ""
			if !from.IsZero() {
				gotFrom = from.Format(time.RFC3339)
			}
			gotTo := ""
			if !to.IsZero() {
				gotTo = to.Format(time.RFC3339)
			}
			if gotFrom != tc.wantFrom {
				t.Errorf("from: got %q want %q", gotFrom, tc.wantFrom)
			}
			if gotTo != tc.wantTo {
				t.Errorf("to: got %q want %q", gotTo, tc.wantTo)
			}
		})
	}
}

func TestParseRange_CustomParamNames(t *testing.T) {
	r := httptest.NewRequest("GET", "/audit-log?date_from=2026-01-01&date_to=2026-01-31", nil)
	from, to, err := ParseRange(r, "date_from", "date_to")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := from.Format(time.RFC3339); got != "2026-01-01T00:00:00Z" {
		t.Errorf("date_from: got %q", got)
	}
	if got := to.Format(time.RFC3339); got != "2026-01-31T23:59:59Z" {
		t.Errorf("date_to: got %q", got)
	}
}
