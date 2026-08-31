package arch

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// WHY THESE GATES EXIST
//
// The audit redesign (ADR-089/090) was audited for completeness THREE times. Each
// round found ~30 places where a comment, an ADR paragraph, a registry note or a
// runbook box claimed something the code does not do — and each round's FIXES
// left siblings behind, or introduced fresh lies of their own. The third round
// caught a comment asserting a route is `explicit` when the registry says
// `exempt`, in a commit whose entire purpose was to remove false claims.
//
// The lesson is not "try harder". It is:
//
//	PROSE THAT MAKES A PRECISE, CHECKABLE CLAIM AND IS NEVER CHECKED WILL DRIFT.
//	Either mechanize the check, or do not make the claim.
//
// These are the mechanized checks for the two classes that drifted in every
// single round. They are deliberately cheap and deliberately literal.

// sourceFiles walks internal/ and hands back every non-test .go file.
func sourceFiles(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	root := internalDir(t)
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(filepath.Dir(root), path)
		out[rel] = string(b)
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal/: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("walked 0 source files — the gate is broken, not the code")
	}
	return out
}

// markSkipCallers is the CANONICAL list of paths that declare "this 2xx mutated
// nothing" (audit.MarkSkip). It is duplicated in prose in two places that keep
// going stale — internal/audit/context.go's "Live callers" list and ADR-090 §4 —
// so it is pinned HERE, against the code, and those two now point at this test
// rather than trying to enumerate from memory.
//
// Adding a MarkSkip call is a real decision: it tells the coverage detector that a
// successful mutating request legitimately wrote no audit row. If you add one,
// add it here and say why. If this list is longer than you expected, that is the
// point — a silent MarkSkip is a route quietly opting out of audit coverage.
// markSkipCaller is a declared opt-out of audit coverage: n calls in one file,
// and the reason they are legitimate.
//
// The COUNT is load-bearing. The first version of this gate keyed on the FILE
// alone — so adding a tenth MarkSkip to a file that already declared nine passed
// CI silently, while this very comment claimed "add a MarkSkip call without
// declaring it and CI fails". A gate that under-enforces its own stated contract
// is the failure class this whole arc is about, reproduced inside the fix for it.
// Pinning n means a NEW opt-out in an existing file breaks the build.
type markSkipCaller struct {
	n   int
	why string
}

var markSkipCallers = map[string]markSkipCaller{
	"internal/billing/create_preview_handler.go": {1, "invoice create_preview — read-only dry run"},
	"internal/recipe/handler.go":                 {1, "recipe preview — read-only render"},
	"internal/recipe/service.go":                 {1, "recipe re-apply that installs nothing"},
	"internal/tenant/settings.go":                {1, "settings save that changed no field"},
	"internal/usage/handler.go":                  {1, "duplicate usage ingest — idempotency key already exists, no event row written"},
	"internal/user/handler.go":                   {3, "logout with NO cookie; logout with a stale cookie (both revoke nothing); a password reset that issued no token — unknown address OR an account over its send cap, which share ONE fixed-200 path (that fixedness is the enumeration defence)"},
	"internal/api/adapters.go":                   {2, "hosted-invoice payment-session reuse — a second Pay click returns the same Stripe session, no new claim row"},
	"internal/api/middleware/idempotency.go":     {1, "idempotency replay — the cached response of a request whose original DID emit"},
	"internal/creditnote/handler.go":             {1, "credit-note issue that defers"},
}

// TestMarkSkipCallers_MatchTheCode is the gate on the class that drifted in all
// three audit rounds: a hand-maintained list of "who calls MarkSkip" that nobody
// re-derives when a caller is added.
func TestMarkSkipCallers_MatchTheCode(t *testing.T) {
	call := regexp.MustCompile(`(?:audit\.)?MarkSkip\(`)

	found := map[string]int{}
	for path, src := range sourceFiles(t) {
		// The definition itself, and the package's own doc, are not callers.
		if path == "internal/audit/context.go" {
			continue
		}
		if n := len(call.FindAllString(src, -1)); n > 0 {
			found[path] = n
		}
	}

	var undeclared, stale, miscounted []string
	for path, n := range found {
		d, ok := markSkipCallers[path]
		if !ok {
			undeclared = append(undeclared, fmt.Sprintf("%s (%d call(s))", path, n))
			continue
		}
		if d.n != n {
			miscounted = append(miscounted, fmt.Sprintf("%s: %d call(s) in the code, %d declared — %s", path, n, d.n, d.why))
		}
	}
	for path := range markSkipCallers {
		if found[path] == 0 {
			stale = append(stale, path)
		}
	}
	sort.Strings(undeclared)
	sort.Strings(stale)
	sort.Strings(miscounted)

	if len(undeclared) > 0 {
		t.Errorf(`%d file(s) call audit.MarkSkip but are NOT declared in markSkipCallers:

  %s

MarkSkip tells the coverage detector that a successful MUTATING request legitimately
wrote no audit row. That is a route opting out of audit coverage, and it must be a
decision someone made on purpose — not a line that appeared. Add the file to
markSkipCallers with its call COUNT and a one-line reason.`,
			len(undeclared), strings.Join(undeclared, "\n  "))
	}
	if len(miscounted) > 0 {
		t.Errorf(`%d file(s) have a DIFFERENT number of MarkSkip calls than declared:

  %s

A NEW MarkSkip in a file that already had one is a NEW route opting out of audit
coverage — the count is what makes that visible. If the new call is intended, bump
the count and extend the reason to cover it.`, len(miscounted), strings.Join(miscounted, "\n  "))
	}
	if len(stale) > 0 {
		t.Errorf(`%d entr(y/ies) in markSkipCallers call MarkSkip nowhere:

  %s

The caller was removed but the list was not. A list that names paths which no longer
exist is exactly the kind of confident-and-wrong documentation this gate exists to
prevent.`, len(stale), strings.Join(stale, "\n  "))
	}
}

// deletedThings are things this arc DELETED, paired with the phrasings that assert
// they still exist. Every audit round found comments still describing them in the
// present tense — the catch-all middleware was found in round 1, round 2 AND round
// 3, in five different files, because each pass fixed only the instances it was
// handed.
//
// The check is deliberately literal: it looks for a present-tense assertion, not a
// mere mention. Saying "the catch-all was deleted" or "this used to opt out of the
// catch-all" is correct and stays legal — describing history is how you stop the
// next reader re-deriving a wrong conclusion.
var forbiddenPresentTense = []struct {
	pattern *regexp.Regexp
	why     string
}{
	{
		// The first version required the exact word order "middleware['s] catch-all
		// <verb>", so it sailed past the phrasing the codebase actually uses ("the
		// catch-all middleware writes a row for any mutating request"). A gate that
		// only matches the phrasing you happened to have written is not a gate.
		regexp.MustCompile(`(?i)catch-all[^.\n]{0,40}\b(records|writes|logs|invents|fires|still|would|falls back|alone)\b|\b(records|writes|logs|invents)\b[^.\n]{0,30}\bcatch-all\b`),
		"the catch-all audit middleware was DELETED (ADR-090, #471). It does not record, write, invent or fall back to anything. Describing history is fine — say \"used to\", \"was deleted\".",
	},
	{
		regexp.MustCompile(`which the next PR deletes`),
		"the catch-all was deleted in #471; there is no 'next PR'. A comment describing a completed deletion as pending tells the reader the opposite of the truth.",
	},
	{
		regexp.MustCompile(`(?i)AuditLog middleware\b|middleware\.AuditLog\b|\bMarkHandled\b`),
		"the AuditLog middleware and MarkHandled were DELETED (ADR-090, #471). There is ONE post-hoc write path (Logger.Log) plus the in-tx LogInTx, and MarkSkip (not MarkHandled) is the only declaration a handler may make.",
	},
	{
		regexp.MustCompile(`order=sim_effective_at|order by simulated time|OrderBySim`),
		"there is deliberately NO 'order by simulated time' control (ADR-090 §5): within one clock it is the same order as created_at, and across clocks it interleaves unrelated simulations. Removed in #474 with its cursor axis.",
	},
}

// negated marks a sentence that DENIES the deleted thing exists ("there is
// deliberately NO order by simulated time", "the catch-all WAS DELETED"). Those
// are the sentences we WANT — describing history, or the absence of a mechanism,
// is how you stop the next reader re-deriving a wrong conclusion. A gate that
// fires on the correct sentence is a gate somebody deletes, so it must not.
//
// But it must not be a blanket escape either. The first version exempted any line
// containing a bare "no" or "not" ANYWHERE — words so common in this codebase's
// comments that the exemption swallowed most of the class it was guarding. It is
// now restricted to markers that actually signal past tense or absence.
var negated = regexp.MustCompile(`(?i)\b(deleted|delete|removed|retired|superseded|gone|used to|no longer|gone away|gets rid of|gets deleted|gets removed|gets retired|deliberately (no|not)|there is no|gates? that)\b|\bwas\b|\bwere\b|\bhistoric`)

// TestNoPresentTenseReferencesToDeletedThings gates the other class that survived
// every round: a comment that still describes machinery the arc removed. An
// out-of-date comment about a DELETED mechanism is worse than no comment — it
// sends the next reader looking for a safety net that is not there.
//
// Matching is per-LINE and negation-aware: the forbidden phrasing only fails when
// the line asserts the thing is live.
func TestNoPresentTenseReferencesToDeletedThings(t *testing.T) {
	for path, src := range sourceFiles(t) {
		// The gate's own table names the phrases it forbids.
		if strings.HasSuffix(path, "audit_prose_gates_test.go") {
			continue
		}
		lines := strings.Split(src, "\n")
		for i, line := range lines {
			for _, f := range forbiddenPresentTense {
				m := f.pattern.FindString(line)
				if m == "" {
					continue
				}
				// Comments WRAP. A sentence that says "(It used to opt out of a /
				// catch-all middleware that would have invented …)" puts the
				// past-tense marker on a DIFFERENT line from the match, so a
				// line-scoped negation check would fail the very sentence it is
				// meant to bless. Look at the surrounding window instead.
				lo, hi := max(0, i-3), min(len(lines), i+4)
				if negated.MatchString(strings.Join(lines[lo:hi], " ")) {
					continue
				}
				t.Errorf("%s:%d: %q\n\n  %s\n", path, i+1, m, f.why)
			}
		}
	}
}

// TestAuditProseGates_AreThemselvesWired is the belt on the gates: a regex that
// matches nothing (because someone renamed the thing it looked for) is a gate that
// has silently stopped gating — the exact failure mode of the audit coverage this
// whole arc was about.
func TestAuditProseGates_AreThemselvesWired(t *testing.T) {
	files := sourceFiles(t)
	call := regexp.MustCompile(`(?:audit\.)?MarkSkip\(`)
	total := 0
	for path, src := range files {
		if path == "internal/audit/context.go" {
			continue
		}
		total += len(call.FindAllString(src, -1))
	}
	if total == 0 {
		t.Fatal("found ZERO MarkSkip calls — either they are all gone (delete this gate) or the pattern stopped matching (fix it). A gate that matches nothing is not a gate.")
	}
	if len(markSkipCallers) == 0 {
		t.Fatal("markSkipCallers is empty")
	}
	fmt.Printf("audit prose gates: %d MarkSkip call sites across %d declared files\n", total, len(markSkipCallers))
}

// TestAuditProseGates_CatchTheRealPhrasings is the gate ON the gate.
//
// The first version of forbiddenPresentTense matched only the exact word order it
// happened to have been written against, and sailed past the phrasing the
// codebase's own docs use ("the catch-all middleware writes a row for any mutating
// request"). A gate that only catches the sentence you already fixed is theatre.
//
// These are real re-introductions of each deleted mechanism. If one stops failing,
// the gate has regressed and the class it guards is open again. The mirror half
// matters just as much: a gate that fires on the CORRECT sentence ("the catch-all
// was deleted") is a gate the next person deletes.
func TestAuditProseGates_CatchTheRealPhrasings(t *testing.T) {
	fires := func(line string) bool {
		for _, f := range forbiddenPresentTense {
			if f.pattern.MatchString(line) && !negated.MatchString(line) {
				return true
			}
		}
		return false
	}

	mustFail := []string{
		"// the catch-all middleware writes a row for any mutating /v1 request",
		"// The audit catch-all still records a row here",
		"// AuditLog middleware writes the fallback row",
		"// the handler calls MarkHandled so the catch-all stays quiet",
		"// pass ?order=sim_effective_at to sort by simulated time",
	}
	mustPass := []string{
		"// the catch-all was deleted (ADR-090); it no longer records anything",
		"// It used to opt out of a catch-all middleware that would have invented a row; that middleware is gone",
		"// there is deliberately no order by simulated time",
	}

	for _, l := range mustFail {
		if !fires(l) {
			t.Errorf("the gate MISSES a real re-introduction — the class is open again:\n  %s", l)
		}
	}
	for _, l := range mustPass {
		if fires(l) {
			t.Errorf("the gate FIRES on a CORRECT sentence — it will be deleted by the next person it annoys:\n  %s", l)
		}
	}
}
