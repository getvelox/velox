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
// MAJOR bumps on 2026-08-05: ADR-112 replaced the dunning block's
// `final_action` key with `final_subscription_action` +
// `final_invoice_action`, and parseRecipe REJECTS the old key by name. A
// recipe pinned at the previous version no longer parses, which is a
// breaking DSL change, not an edit.
var pinnedCatalog = map[string]string{
	"anthropic_style@5.0.0": "dc1d147c8ace6fcbeb31d7a232b2d214ed0dee1c8492a329b7de586dff08f5b2",
	"openai_style@5.0.0":    "9e340351b37677c8e79215e9bd8fcd90d198756ecdf4b5a34bd5a706af28af58",
	"replicate_style@3.0.0": "8abd33097c29909d3bc53c51b0aacab9b9f8c27b984c94724030247b05b8796a",
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
