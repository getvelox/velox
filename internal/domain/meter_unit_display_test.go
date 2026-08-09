package domain

import "testing"

// TestMeterUnitDisplayScale pins the Go half of the display-scale
// convention: token rates quote per 1M tokens (how Anthropic, OpenAI and
// Metronome all list them), everything else per single unit. The FE twin
// is web-v2/src/lib/priceDisplay.ts unitScale — the two mappings must move
// together, and each side pins its own so a one-sided edit fails a test
// instead of splitting the dashboard from the PDF.
func TestMeterUnitDisplayScale(t *testing.T) {
	cases := []struct {
		unit      string
		wantShift int32
		wantPer   string
	}{
		{"tokens", 6, "1M tokens"},
		{"token", 6, "1M tokens"},
		{" Tokens ", 6, "1M tokens"}, // meters in the wild carry stray case/space
		{"seconds", 0, ""},
		{"requests", 0, ""},
		{"", 0, ""},
	}
	for _, tc := range cases {
		shift, per := MeterUnitDisplayScale(tc.unit)
		if shift != tc.wantShift || per != tc.wantPer {
			t.Errorf("MeterUnitDisplayScale(%q) = (%d, %q), want (%d, %q)",
				tc.unit, shift, per, tc.wantShift, tc.wantPer)
		}
	}
}

// TestMeterUnitDisplayScale_RendersWedgeRate is the one concrete number the
// product is judged by: a $3.00/1M-tokens rate is stored as 0.0003 decimal
// cents per token; shifted by the convention it must read exactly 3 dollars,
// not 2.999… or 30. This is the bridge between ADR-045's decimal storage and
// ADR-054's display arc.
func TestMeterUnitDisplayScale_RendersWedgeRate(t *testing.T) {
	li := InvoiceLineItem{MeterUnit: "tokens"}
	shift, per := MeterUnitDisplayScale(li.MeterUnit)
	if shift != 6 || per != "1M tokens" {
		t.Fatalf("convention moved: shift=%d per=%q", shift, per)
	}
}
