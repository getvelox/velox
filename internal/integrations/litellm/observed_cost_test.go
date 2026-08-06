package litellm

import (
	"math"
	"testing"
)

// payloadWithBreakdown builds a completion whose input side splits into an
// uncached half and a cache-read half, so the 3x-COGS trap is reachable.
func payloadWithBreakdown(in, out float64, cacheRead int64) StandardLoggingPayload {
	p := StandardLoggingPayload{
		ID: "call_1", User: "cus_x", Model: "claude-3.5-sonnet",
		CallType: "completion",
		Usage: &Usage{
			PromptTokens: 1000, CompletionTokens: 200,
			PromptTokensDetails: &PromptTokensDetails{CachedTokens: cacheRead},
		},
		CostBreakdown: &CostBreakdown{
			InputCost: in, OutputCost: out, TotalCost: in + out,
		},
	}
	return p
}

func costOf(t *testing.T, evs []ExternalIngest, half string) *int64 {
	t.Helper()
	for _, e := range evs {
		if e.Dimensions["token_type"] == half {
			return e.ObservedCostMicros
		}
	}
	return nil
}

// TestObservedCost_PerHalfOnly is the money assertion behind ADR-079 D4.
// One LiteLLM call fans out to up to three usage events. Stamping a
// whole-call figure on each would multi-count COGS by up to 3x, which is
// precisely why phase 1 stamped nothing. Each half must carry only its own
// side's cost, and cache_read must carry none.
func TestObservedCost_PerHalfOnly(t *testing.T) {
	t.Parallel()
	evs, err := MapPayload(payloadWithBreakdown(0.012, 0.045, 300))
	if err != nil {
		t.Fatalf("map: %v", err)
	}

	in := costOf(t, evs, TokenTypeInput)
	if in == nil || *in != 12000 {
		t.Errorf("input half: got %v, want 12000 micros ($0.012)", in)
	}
	out := costOf(t, evs, TokenTypeOutput)
	if out == nil || *out != 45000 {
		t.Errorf("output half: got %v, want 45000 micros ($0.045)", out)
	}
	// The load-bearing one: InputCost already covers the whole input side,
	// so attaching it here too would bill the input cost twice.
	if cr := costOf(t, evs, TokenTypeCacheRead); cr != nil {
		t.Errorf("cache_read half: got %v, want nil — InputCost already covers "+
			"the input side, so stamping it here double-counts COGS", *cr)
	}

	// And the total across halves must equal the call's total, not a multiple.
	var sum int64
	for _, e := range evs {
		if e.ObservedCostMicros != nil {
			sum += *e.ObservedCostMicros
		}
	}
	if sum != 57000 {
		t.Errorf("summed observed cost = %d micros, want 57000 ($0.057) — "+
			"a larger number means a whole-call figure leaked onto a half", sum)
	}
}

// TestObservedCost_ResponseCostNeverStamped: ResponseCost is a WHOLE-CALL
// figure. It is the obvious fallback when CostBreakdown is absent, and D4
// forbids it for exactly that reason.
func TestObservedCost_ResponseCostNeverStamped(t *testing.T) {
	t.Parallel()
	p := payloadWithBreakdown(0, 0, 0)
	p.CostBreakdown = nil
	p.ResponseCost = 0.057

	evs, err := MapPayload(p)
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	for _, e := range evs {
		if e.ObservedCostMicros != nil {
			t.Errorf("%s: got %d, want nil — ResponseCost is whole-call and must "+
				"fall to rate-table inference, never be stamped per half",
				e.Dimensions["token_type"], *e.ObservedCostMicros)
		}
	}
}

// TestObservedCost_RejectsNonFinite: a NaN cast to int64 is garbage that
// would silently corrupt margin. Falling back to the table is the honest
// answer, so these must produce nil rather than a clamped number.
func TestObservedCost_RejectsNonFinite(t *testing.T) {
	t.Parallel()
	for name, v := range map[string]float64{
		"NaN":      math.NaN(),
		"+Inf":     math.Inf(1),
		"-Inf":     math.Inf(-1),
		"negative": -0.01,
	} {
		t.Run(name, func(t *testing.T) {
			evs, err := MapPayload(payloadWithBreakdown(v, 0.045, 0))
			if err != nil {
				t.Fatalf("map: %v", err)
			}
			if in := costOf(t, evs, TokenTypeInput); in != nil {
				t.Errorf("input half with %s cost: got %d, want nil", name, *in)
			}
			// The healthy half is unaffected — one bad number must not
			// discard the cost we do trust.
			if out := costOf(t, evs, TokenTypeOutput); out == nil || *out != 45000 {
				t.Errorf("output half: got %v, want 45000", out)
			}
		})
	}
}

// TestObservedCost_ZeroIsARealAnswer: a free or fully-credited call costs
// zero. That must be recorded as observed 0, not dropped — otherwise the
// margin card cannot tell "cost was zero" from "we don't know the cost".
func TestObservedCost_ZeroIsARealAnswer(t *testing.T) {
	t.Parallel()
	evs, err := MapPayload(payloadWithBreakdown(0, 0, 0))
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	in := costOf(t, evs, TokenTypeInput)
	if in == nil {
		t.Fatal("input half: got nil, want an explicit 0 — zero cost is a fact, not an absence")
	}
	if *in != 0 {
		t.Errorf("input half: got %d, want 0", *in)
	}
}

// TestObservedCost_RoundsHalfUp matches the rate-table path's ROUND() in
// internal/usage/postgres.go, so the two cost sources agree at the boundary
// instead of differing by a micro.
func TestObservedCost_RoundsHalfUp(t *testing.T) {
	t.Parallel()
	// 0.0000155 USD = 15.5 micros -> 16
	evs, err := MapPayload(payloadWithBreakdown(0.0000155, 0, 0))
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	if in := costOf(t, evs, TokenTypeInput); in == nil || *in != 16 {
		t.Errorf("got %v, want 16 micros (15.5 rounds half UP)", in)
	}
}

// TestObservedCost_AbsentBreakdownFallsToTable pins the no-regression case:
// every pre-existing ingest path sends no CostBreakdown and must keep
// getting rate-table inference.
func TestObservedCost_AbsentBreakdownFallsToTable(t *testing.T) {
	t.Parallel()
	p := payloadWithBreakdown(0, 0, 0)
	p.CostBreakdown = nil

	evs, err := MapPayload(p)
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	if len(evs) == 0 {
		t.Fatal("expected events")
	}
	for _, e := range evs {
		if e.ObservedCostMicros != nil {
			t.Errorf("%s: got %d, want nil so the rate table stays in charge",
				e.Dimensions["token_type"], *e.ObservedCostMicros)
		}
	}
}
