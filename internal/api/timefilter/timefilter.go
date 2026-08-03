// Package timefilter is the shared parser for ?from / ?to (and named
// variants like ?date_from / ?date_to) query parameters across
// operator-facing list and export endpoints. One contract everywhere
// so an operator who learns one endpoint doesn't trip on the next.
//
// Accepts two formats:
//   - RFC3339 instants: "2026-01-01T00:00:00Z" — what the dashboard
//     sends, derived from tenant-TZ start/end-of-day per ADR-010.
//   - Bare YYYY-MM-DD dates: "2026-01-01" — convenient for curl/CLI
//     users running ad-hoc queries.
//
// Date-only inputs are anchored at UTC: `from` becomes 00:00:00Z (start
// of day), `to` becomes 23:59:59Z (end of day) so a YYYY-MM-DD range is
// inclusive on both ends — matches what an operator typing
// "from=2026-01-01&to=2026-01-31" intuitively expects.
package timefilter

import (
	"net/http"
	"time"

	"github.com/sagarsuperuser/velox/internal/errs"
)

// ParseRange reads two named query params from the request and returns
// their parsed time.Time values. Missing params return zero-value times
// (callers branch on IsZero to skip the filter).
//
// Returns errs.Invalid on malformed input — the handler should pipe
// through respond.FromError to surface a 400. Also enforces that
// `to` (when both set) lands strictly after `from`.
//
// fromParam and toParam are usually "from"/"to" but the audit log uses
// the legacy "date_from"/"date_to" naming. Pass whichever the endpoint
// has historically exposed; this helper doesn't normalize the param
// names because that would be an operator-visible URL contract change.
func ParseRange(r *http.Request, fromParam, toParam string) (from, to time.Time, err error) {
	if v := r.URL.Query().Get(fromParam); v != "" {
		from, err = parseOne(v, false)
		if err != nil {
			return time.Time{}, time.Time{}, errs.Invalid(fromParam, "must be RFC3339 (e.g. 2026-01-01T00:00:00Z) or YYYY-MM-DD")
		}
	}
	if v := r.URL.Query().Get(toParam); v != "" {
		to, err = parseOne(v, true)
		if err != nil {
			return time.Time{}, time.Time{}, errs.Invalid(toParam, "must be RFC3339 (e.g. 2026-12-31T23:59:59Z) or YYYY-MM-DD")
		}
	}
	// Reject only INVERTED ranges. Equal endpoints are a valid zero-width
	// window: every consumer's SQL is inclusive on both ends (>= / <=), so
	// from==to selects rows at exactly that instant — which is THE natural
	// query on the sim axis, where sim_effective_at is quantized to advance
	// targets and cycle boundaries (found on the FLOW P2 walk: the flow's
	// own single-instant example 422'd against the stricter check, while
	// the package doc and the SQL beneath both promised inclusivity).
	if !from.IsZero() && !to.IsZero() && to.Before(from) {
		return time.Time{}, time.Time{}, errs.Invalid(toParam, "must not be before `"+fromParam+"`")
	}
	return from, to, nil
}

// parseOne tries RFC3339 first (the canonical wire format), then bare
// YYYY-MM-DD as the operator-friendly fallback. endOfDay=true anchors
// a date-only value at 23:59:59Z instead of 00:00:00Z — used for the
// `to` side of the range so the day named in the filter is included.
func parseOne(s string, endOfDay bool) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		if endOfDay {
			return time.Date(t.Year(), t.Month(), t.Day(), 23, 59, 59, 0, time.UTC), nil
		}
		return t.UTC(), nil
	}
	return time.Time{}, errInvalidFormat
}

// errInvalidFormat is returned by parseOne for malformed input. The
// caller (ParseRange) wraps it in an errs.Invalid with the param name
// + format hint so the eventual 400 response identifies which field
// to fix.
var errInvalidFormat = invalidFormatErr{}

type invalidFormatErr struct{}

func (invalidFormatErr) Error() string { return "invalid date format" }
