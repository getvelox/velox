package recipe

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
)

// pinnedCatalog maps "<key>@<version>" to the SHA-256 of that recipe's YAML
// bytes. A recipe's version string is its cache-buster for operators ("is
// this the template I installed?") — but nothing structural forces a bump
// when the YAML moves, and the openai/anthropic catalogs each shipped
// content changes under an unmoved version before this guard existed
// (found by the 2026-07-26 recipe e2e review). Editing a recipe now fails
// this test until you EITHER bump the version and re-pin, or admit the
// content change under the same version is a bug.
//
// To update after a deliberate change: bump `version:` in the YAML, then
// replace the pin with the value from the test's failure message.
var pinnedCatalog = map[string]string{
	"anthropic_style@4.0.0": "b0a81eb74d40b831e68da5edca15d377abd707a548d17d06ff950b8fc6677b09",
	"openai_style@4.0.0":    "4abf6fd59385fefb39b1eb19b2111a8d6f790c92248b77bcddb173b5b1506fa4",
	"replicate_style@2.0.0": "b88b8c6481f9e4128a2ed244861208bde138857c8a859cfc575d530fc1694239",
}

// TestRecipeCatalog_VersionMovesWithContent asserts every embedded recipe's
// (version, content-hash) pair matches the pin above, in both directions:
// same version + different bytes = a content change hiding under a stale
// version; a version not in the pin map = a bump that forgot to re-pin.
func TestRecipeCatalog_VersionMovesWithContent(t *testing.T) {
	entries, err := recipeFS.ReadDir("recipes")
	if err != nil {
		t.Fatalf("read embedded recipes dir: %v", err)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		raw, err := recipeFS.ReadFile("recipes/" + e.Name())
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		rec, err := parseRecipe(raw)
		if err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		sum := sha256.Sum256(raw)
		got := hex.EncodeToString(sum[:])
		pinKey := fmt.Sprintf("%s@%s", rec.Key, rec.Version)
		seen[pinKey] = true

		want, ok := pinnedCatalog[pinKey]
		if !ok {
			t.Errorf("%s: version %s has no pin — add %q: %q to pinnedCatalog (and make sure the version was bumped on purpose)",
				e.Name(), rec.Version, pinKey, got)
			continue
		}
		if want != got {
			t.Errorf("%s: content changed but version is still %s — bump `version:` and re-pin (new hash %s). A moved template under an unmoved version lies to operators about what they installed.",
				e.Name(), rec.Version, got)
		}
	}
	for pinKey := range pinnedCatalog {
		if !seen[pinKey] {
			t.Errorf("pinnedCatalog entry %q matches no embedded recipe — prune the stale pin", pinKey)
		}
	}
}
