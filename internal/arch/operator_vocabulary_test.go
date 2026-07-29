package arch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ADR-104's operator contract bans builder vocabulary from operator-facing
// copy: an operator must never need the words "wall-clock" (or a
// definition-by-negation of the test clock's dates) to read a screen. The
// contract's rendering rule is positive and singular — the primary date is
// the entity's calendar; the real-world instant, when it differs, is a
// "Recorded <wall>" subline. This gate keeps the vocabulary from
// reappearing the way the two-lane captions did ("Real times — not the
// test clock's dates.").
//
// Scope: non-comment source lines in web-v2/src — JSX text, string
// literals, toasts, tooltips. Developer comments are exempt (engineers
// legitimately discuss wall-clock semantics); if a comment's phrasing is
// ever promoted into visible copy, the promoted literal is a non-comment
// line and the gate catches it at that moment. Test files and generated
// artifacts are skipped.
var forbiddenOperatorVocabulary = []string{
	"wall-clock",
	"wall clock",
	"not the test clock's dates",
	"Real-time activity",
}

func TestOperatorCopyAvoidsBuilderVocabulary(t *testing.T) {
	root := filepath.Join(filepath.Dir(internalDir(t)), "web-v2", "src")
	if _, err := os.Stat(root); err != nil {
		t.Skipf("web-v2/src not present: %v", err)
	}
	var hits []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Generated TS mirrors backend prose; not operator copy.
			if d.Name() == "gen" || d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".tsx") && !strings.HasSuffix(path, ".ts") {
			return nil
		}
		if strings.HasSuffix(path, ".gen.ts") || strings.HasSuffix(path, ".test.ts") || strings.HasSuffix(path, ".test.tsx") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		src := string(b)
		inBlockComment := false
		for i, line := range strings.Split(src, "\n") {
			trimmed := strings.TrimSpace(line)
			// Line-based comment stripping: enough for this repo's style
			// (no minified sources in src/). Block comments include the
			// {/* ... */} JSX form.
			if inBlockComment {
				if strings.Contains(trimmed, "*/") {
					inBlockComment = false
				}
				continue
			}
			if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "*") {
				continue
			}
			if idx := strings.Index(line, "/*"); idx >= 0 {
				if !strings.Contains(line[idx:], "*/") {
					inBlockComment = true
				}
				// Scan only the code before the comment opener.
				line = line[:idx]
			}
			// Inline trailing // comment: check only the code before it.
			if idx := strings.Index(line, "//"); idx >= 0 {
				line = line[:idx]
			}
			lower := strings.ToLower(line)
			for _, word := range forbiddenOperatorVocabulary {
				if strings.Contains(lower, strings.ToLower(word)) {
					rel, _ := filepath.Rel(root, path)
					hits = append(hits, rel+":"+itoa(i+1)+": "+strings.TrimSpace(line))
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk web-v2/src: %v", err)
	}
	for _, h := range hits {
		t.Errorf("builder vocabulary in operator-facing source (ADR-104 operator contract): %s", h)
	}
	if len(hits) > 0 {
		t.Log(`Reword using the contract's vocabulary: the primary date is the entity's calendar; the real instant renders as "Recorded <time>"; the page banner is the simulation disclosure.`)
	}
}
