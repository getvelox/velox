package domain

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// eventTypeValue is the shape of an outbound webhook event type, such as
// "payment.currency_mismatch" or "subscription.item.added".
var eventTypeValue = regexp.MustCompile(`^[a-z_]+(\.[a-z_]+)+$`)

// declaredEventTypes reads every string constant named Event* in this
// package whose value looks like an event type. Emit sites dispatch these
// constants, so a constant that is declared is a type Velox can send.
func declaredEventTypes(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package domain: %v", err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, e.Name(), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		files = append(files, f)
	}
	out := map[string]string{} // value -> constant name
	for _, f := range files {
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, name := range vs.Names {
					if !strings.HasPrefix(name.Name, "Event") || i >= len(vs.Values) {
						continue
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					v, err := strconv.Unquote(lit.Value)
					if err != nil || !eventTypeValue.MatchString(v) {
						continue
					}
					out[v] = name.Name
				}
			}
		}
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// TestWebhookCatalog_EveryDeclaredEventIsSubscribable: every event type
// Velox can send is in KnownWebhookEventTypes, the set endpoint create and
// update validate against. payment.currency_mismatch was emitted for months
// while missing from it, so no endpoint could subscribe to it by name.
func TestWebhookCatalog_EveryDeclaredEventIsSubscribable(t *testing.T) {
	declared := declaredEventTypes(t)
	if len(declared) < 30 {
		t.Fatalf("found only %d Event* constants; the parser is not reading the package", len(declared))
	}
	for _, v := range sortedKeys(declared) {
		if !KnownWebhookEventTypes[v] {
			t.Errorf("%s (%q) is declared but missing from KnownWebhookEventTypes", declared[v], v)
		}
	}
	for _, v := range sortedKeys(KnownWebhookEventTypes) {
		if _, ok := declared[v]; !ok {
			t.Errorf("KnownWebhookEventTypes has %q, but no Event* constant declares it", v)
		}
	}
}

// TestWebhookCatalog_DashboardPickerMatches: the dashboard's event picker
// (EVENT_GROUPS in web-v2/src/pages/Webhooks.tsx) offers exactly the
// catalog. A missing type cannot be picked; an extra one would be refused.
func TestWebhookCatalog_DashboardPickerMatches(t *testing.T) {
	raw, err := os.ReadFile("../../web-v2/src/pages/Webhooks.tsx")
	if err != nil {
		t.Fatalf("read Webhooks.tsx: %v", err)
	}
	src := string(raw)
	start := strings.Index(src, "const EVENT_GROUPS")
	end := strings.Index(src, "const ALL_EVENT_TYPES")
	if start < 0 || end < start {
		t.Fatal("Webhooks.tsx: EVENT_GROUPS ... ALL_EVENT_TYPES block not found")
	}
	picker := map[string]bool{}
	for _, m := range regexp.MustCompile(`type: '([^']+)'`).FindAllStringSubmatch(src[start:end], -1) {
		picker[m[1]] = true
	}
	for _, v := range sortedKeys(KnownWebhookEventTypes) {
		if !picker[v] {
			t.Errorf("the dashboard event picker does not offer %q", v)
		}
	}
	for _, v := range sortedKeys(picker) {
		if !KnownWebhookEventTypes[v] {
			t.Errorf("the dashboard event picker offers %q, which is not in KnownWebhookEventTypes", v)
		}
	}
}

// TestWebhookCatalog_DocsListEveryEvent: docs/webhooks.md has a row for
// every event in the catalog, and no row for one outside it. A row may
// name siblings as "`a.b` / `.c`", which means a.b and a.c.
func TestWebhookCatalog_DocsListEveryEvent(t *testing.T) {
	raw, err := os.ReadFile("../../docs/webhooks.md")
	if err != nil {
		t.Fatalf("read docs/webhooks.md: %v", err)
	}
	documented := map[string]bool{}
	name := regexp.MustCompile("`([a-z_.]+)`")
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "| `") {
			continue
		}
		cell := strings.SplitN(strings.TrimPrefix(line, "|"), "|", 2)[0]
		var first string
		for _, m := range name.FindAllStringSubmatch(cell, -1) {
			n := m[1]
			switch {
			case first == "" && eventTypeValue.MatchString(n):
				first = n
				documented[n] = true
			case first != "" && strings.HasPrefix(n, "."):
				documented[first[:strings.LastIndex(first, ".")]+n] = true
			}
		}
	}
	for _, v := range sortedKeys(KnownWebhookEventTypes) {
		if !documented[v] {
			t.Errorf("docs/webhooks.md has no row for %q", v)
		}
	}
	for _, v := range sortedKeys(documented) {
		if !KnownWebhookEventTypes[v] {
			t.Errorf("docs/webhooks.md lists %q, which is not in KnownWebhookEventTypes", v)
		}
	}
}

// TestWebhookPayload_EveryCatalogEventHasARule: every event in the catalog
// belongs to a family with a resource, and WebhookEventExtras lists its
// extra keys (an empty list for none). An event without a rule would send
// whatever its emitter happened to put together.
func TestWebhookPayload_EveryCatalogEventHasARule(t *testing.T) {
	for _, v := range sortedKeys(KnownWebhookEventTypes) {
		if WebhookEventResource(v) == "" {
			t.Errorf("%q: its family %q has no resource in webhookFamilyResource", v, WebhookEventFamily(v))
		}
		if _, ok := WebhookEventExtras[v]; !ok {
			t.Errorf("%q: missing from WebhookEventExtras", v)
		}
	}
	for _, v := range sortedKeys(WebhookEventExtras) {
		if !KnownWebhookEventTypes[v] {
			t.Errorf("WebhookEventExtras has %q, which is not in KnownWebhookEventTypes", v)
		}
	}
	resources := map[string]bool{}
	for _, r := range webhookFamilyResource {
		resources[r] = true
	}
	for _, extras := range WebhookEventExtras {
		for _, e := range extras {
			if resources[e] {
				t.Errorf("extra key %q is also a resource key", e)
			}
		}
	}
}

// TestWebhookPayload_DocsShowEachEventsData: in docs/webhooks.md each event
// row sits under the heading of its family's resource ("### …
// `data.invoice`"), and its last column lists exactly the keys
// WebhookEventExtras gives it, or "—" for none.
func TestWebhookPayload_DocsShowEachEventsData(t *testing.T) {
	raw, err := os.ReadFile("../../docs/webhooks.md")
	if err != nil {
		t.Fatalf("read docs/webhooks.md: %v", err)
	}
	heading := regexp.MustCompile("^### .*`data\\.([a-z_]+)`")
	name := regexp.MustCompile("`([a-z_.]+)`")
	resource := ""
	seen := map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		if m := heading.FindStringSubmatch(line); m != nil {
			resource = m[1]
			continue
		}
		if !strings.HasPrefix(line, "| `") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		var events []string
		var first string
		for _, m := range name.FindAllStringSubmatch(cells[0], -1) {
			n := m[1]
			switch {
			case first == "" && eventTypeValue.MatchString(n):
				first = n
				events = append(events, n)
			case first != "" && strings.HasPrefix(n, "."):
				events = append(events, first[:strings.LastIndex(first, ".")]+n)
			}
		}
		if len(events) == 0 {
			continue
		}
		var documented []string
		for _, m := range name.FindAllStringSubmatch(cells[len(cells)-1], -1) {
			documented = append(documented, m[1])
		}
		sort.Strings(documented)
		for _, ev := range events {
			seen[ev] = true
			if want := WebhookEventResource(ev); resource != want {
				t.Errorf("docs/webhooks.md: %s is under `data.%s`, but its data carries `%s`", ev, resource, want)
			}
			want := append([]string(nil), WebhookEventExtras[ev]...)
			sort.Strings(want)
			if strings.Join(documented, ",") != strings.Join(want, ",") {
				t.Errorf("docs/webhooks.md: %s lists %v beside the resource, the code sends %v", ev, documented, want)
			}
		}
	}
	for _, v := range sortedKeys(KnownWebhookEventTypes) {
		if !seen[v] {
			t.Errorf("docs/webhooks.md: no row shows what %s carries", v)
		}
	}
}
