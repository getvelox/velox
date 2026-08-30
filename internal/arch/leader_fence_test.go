package arch

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The five leader-only claim funnels (ADR-114). Each must (1) take its
// fence token from the ctx pinned to ITS OWN role and (2) carry
// leader_fence(...) inside the claim statement. A funnel that loses either
// half in a refactor is a tick that can run twice after a takeover — the
// integration tests prove one funnel honours the fence; this pins that the
// other four still have one to honour.
var leaderFunnels = map[string]struct{ fn, role string }{
	"internal/subscription/postgres.go": {"GetDueBilling", "RoleBilling"},
	"internal/dunning/postgres.go":      {"ListDueRuns", "RoleDunning"},
	"internal/webhook/postgres.go":      {"ListPendingDeliveries", "RoleWebhookDelivery"},
	"internal/webhook/outbox.go":        {"ProcessBatch", "RoleWebhookOutbox"},
	"internal/email/outbox.go":          {"claimBatch", "RoleEmailOutbox"},
}

func TestLeaderFunnels_KeepTheirFence(t *testing.T) {
	root := repoRoot(t)
	for rel, want := range leaderFunnels {
		src, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		body := funcBody(string(src), want.fn)
		if body == "" {
			t.Fatalf("%s: func %s not found — if it moved, move this entry", rel, want.fn)
		}
		if !strings.Contains(body, "leader.Fence(ctx, leader."+want.role+")") {
			t.Errorf("%s: %s must take its token with leader.Fence(ctx, leader.%s)", rel, want.fn, want.role)
		}
		if !strings.Contains(body, "leader_fence(") {
			t.Errorf("%s: %s's claim statement must carry AND leader_fence(role, token)", rel, want.fn)
		}
	}
}

// TestSingletonLoops_AreExactlyTheFiveRoles pins the runner's doc claim:
// `grep scheduler.Run(` is the authoritative list of leader-gated loops,
// and every one passes a leader.Role constant (never an ad-hoc string).
func TestSingletonLoops_AreExactlyTheFiveRoles(t *testing.T) {
	root := repoRoot(t)
	call := regexp.MustCompile(`scheduler\.Run\(ctx, leader\.(Role\w+),`)
	seen := map[string]int{}
	err := filepath.WalkDir(filepath.Join(root, "internal"), func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return err
		}
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for _, m := range call.FindAllStringSubmatch(string(src), -1) {
			seen[m[1]]++
		}
		if strings.Contains(string(src), "scheduler.Run(") && !call.Match(src) && !strings.HasSuffix(p, "runner.go") {
			t.Errorf("%s: scheduler.Run called without a leader.Role constant", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"RoleBilling", "RoleDunning", "RoleWebhookOutbox", "RoleEmailOutbox", "RoleWebhookDelivery"}
	for _, r := range want {
		if seen[r] != 1 {
			t.Errorf("leader.%s: %d scheduler.Run sites, want exactly 1", r, seen[r])
		}
	}
	if len(seen) != len(want) {
		t.Errorf("roles with a loop: %v — update leader.Role and this list together", seen)
	}
}

// funcBody returns the source of `func (recv) name(` … up to the next
// top-level `\nfunc ` (good enough for a containment check).
func funcBody(src, name string) string {
	re := regexp.MustCompile(`(?m)^func (\([^)]*\) )?` + regexp.QuoteMeta(name) + `\(`)
	loc := re.FindStringIndex(src)
	if loc == nil {
		return ""
	}
	rest := src[loc[1]:]
	if end := strings.Index(rest, "\nfunc "); end >= 0 {
		rest = rest[:end]
	}
	return rest
}

// repoRoot walks up from the test's cwd to the directory holding go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above " + dir)
		}
		dir = parent
	}
}
