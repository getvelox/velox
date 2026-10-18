package migrate

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
)

// The generated baseline keeps the properties migration 180 relies on.
// scripts/gen-baseline.sh enforces them when it writes the file; this
// catches a hand edit. Equality with the v0.8.1 history is the migrations
// CI job's (scripts/ci/migration-convergence.sh).
func TestBaselineFile(t *testing.T) {
	if !strings.HasPrefix(baselineSQL, "-- Velox's schema at migration 180") {
		t.Fatal("the baseline lost its header: regenerate it with scripts/gen-baseline.sh")
	}
	var sets int
	for i, line := range strings.Split(baselineSQL, "\n") {
		switch {
		case strings.HasPrefix(line, `\`):
			t.Errorf("line %d is a psql meta-command: %s", i+1, line)
		case strings.HasPrefix(line, "SET ") && !strings.HasPrefix(line, "SET LOCAL "):
			t.Errorf("line %d sets a session value, which would leak into the next file: %s", i+1, line)
		case strings.HasPrefix(line, "SET LOCAL "):
			sets++
		case strings.HasPrefix(line, "COMMENT ON EXTENSION"):
			t.Errorf("line %d needs the extension's owner: %s", i+1, line)
		case strings.Contains(line, "ALTER DEFAULT PRIVILEGES"):
			t.Errorf("line %d: %s", i+1, line)
		}
	}
	if sets == 0 {
		t.Error("no SET LOCAL lines: the header SETs are missing")
	}
	for _, want := range []string{
		"SELECT pg_catalog.set_config('search_path', '', true);",
		"CREATE TABLE public.tenants (",
		"INSERT INTO public.leader_leases (role, holder_token)\nSELECT r, (extract(epoch FROM clock_timestamp()) * 1000)::bigint",
		"REVOKE UPDATE, DELETE, TRUNCATE ON TABLE public.audit_log FROM velox_app;",
	} {
		if !strings.Contains(baselineSQL, want) {
			t.Errorf("the baseline lacks %q", want)
		}
	}
	if strings.Contains(baselineSQL, "set_config('search_path', '', false)") {
		t.Error("search_path is set for the session")
	}
}

// The embedded set as goose sees it: migration 180, then the files in
// sql/, with no other source. It builds the provider over a handle that
// never connects: goose reads no database to list its sources.
func TestEmbeddedSources(t *testing.T) {
	db, err := sql.Open("pgx", "postgres://unused@127.0.0.1:1/unused")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	files, err := embeddedFiles(db, migrationFiles())
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 || files[0].Version != baselineVersion || files[0].Name != baselineName {
		t.Fatalf("want migration 180 first, got %+v", files)
	}
	for _, f := range files[1:] {
		if f.Version <= baselineVersion {
			t.Errorf("%s: files in sql/ start at 00181", f.Name)
		}
	}
	p, err := newProvider(db, migrationFiles(), true)
	if err != nil {
		t.Fatal(err)
	}
	if src := p.ListSources()[0]; src.Type != goose.TypeGo || src.Path != "" {
		t.Fatalf("migration 180 should be the registered Go migration, got %+v", src)
	}
}
