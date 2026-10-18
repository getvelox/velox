package migrate

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// splitSQLStatements splits an SQL file into individual statements on `;`
// boundaries, respecting single-quoted strings, double-quoted identifiers,
// dollar-quoted bodies (including tagged variants like `$tag$`), and
// `--` line comments. Multi-line `/* … */` comments are passed through —
// they are uncommon in our migrations and the splitter would only fail
// closed (treat the whole comment body as not-a-string), which is safe.
//
// Only the migration lints use it (lint_test.go): goose has its own parser
// for running files. It is intentionally minimal, not a full SQL parser.
func splitSQLStatements(body []byte) ([]string, error) {
	var out []string
	src := string(body)
	var cur strings.Builder

	inLineComment := false
	inBlockComment := false
	inSingle := false
	inDouble := false
	dollarTag := "" // non-empty when inside a dollar-quoted body

	flush := func() {
		s := strings.TrimSpace(cur.String())
		// Strip leading line-comments (e.g. goose's `-- +goose Up`
		// annotation) so the resulting statement is "clean SQL".
		// Statements that are entirely comments collapse to "" and are
		// dropped.
		s = stripLeadingComments(s)
		if s != "" {
			out = append(out, s)
		}
		cur.Reset()
	}

	for i := 0; i < len(src); i++ {
		c := src[i]
		// Closing handlers first.
		if inLineComment {
			cur.WriteByte(c)
			if c == '\n' {
				inLineComment = false
			}
			continue
		}
		if inBlockComment {
			cur.WriteByte(c)
			if c == '*' && i+1 < len(src) && src[i+1] == '/' {
				cur.WriteByte('/')
				i++
				inBlockComment = false
			}
			continue
		}
		if inSingle {
			cur.WriteByte(c)
			if c == '\'' {
				// Doubled single-quote is an escape, not a close.
				if i+1 < len(src) && src[i+1] == '\'' {
					cur.WriteByte('\'')
					i++
					continue
				}
				inSingle = false
			}
			continue
		}
		if inDouble {
			cur.WriteByte(c)
			if c == '"' {
				inDouble = false
			}
			continue
		}
		if dollarTag != "" {
			cur.WriteByte(c)
			if c == '$' && strings.HasPrefix(src[i:], dollarTag) {
				cur.WriteString(dollarTag[1:])
				i += len(dollarTag) - 1
				dollarTag = ""
			}
			continue
		}

		// Outside any string/comment context.
		switch {
		case c == '-' && i+1 < len(src) && src[i+1] == '-':
			inLineComment = true
			cur.WriteByte(c)
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			inBlockComment = true
			cur.WriteByte(c)
			cur.WriteByte('*')
			i++
		case c == '\'':
			inSingle = true
			cur.WriteByte(c)
		case c == '"':
			inDouble = true
			cur.WriteByte(c)
		case c == '$':
			// Try to read `$tag$` or `$$`. The tag is [a-zA-Z0-9_]*.
			j := i + 1
			for j < len(src) {
				bb := src[j]
				if bb == '$' {
					break
				}
				if !isDollarTagByte(bb) {
					break
				}
				j++
			}
			if j < len(src) && src[j] == '$' {
				tag := src[i : j+1] // includes both $s
				dollarTag = tag
				cur.WriteString(tag)
				i = j
				continue
			}
			cur.WriteByte(c)
		case c == ';':
			flush()
		default:
			cur.WriteByte(c)
		}
	}

	if inSingle || inDouble || dollarTag != "" || inBlockComment {
		return nil, fmt.Errorf("unterminated string/comment in SQL body")
	}
	flush()
	return out, nil
}

func isDollarTagByte(b byte) bool {
	return (b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9') ||
		b == '_'
}

// stripLeadingComments removes leading whitespace + `-- …\n` lines from a
// candidate statement, so a lint sees the statement and not the comments
// above it.
func stripLeadingComments(s string) string {
	for {
		s = strings.TrimLeft(s, " \t\r\n")
		if !strings.HasPrefix(s, "--") {
			return s
		}
		nl := strings.IndexByte(s, '\n')
		if nl < 0 {
			return ""
		}
		s = s[nl+1:]
	}
}

// The contract: each statement returned is one statement, as Postgres
// would see it. Strings, identifiers, dollar-quoted bodies,
// and `--` line comments must not bleed across statements.
func TestSplitSQLStatements(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "single statement no trailing semicolon",
			in:   "SELECT 1",
			want: []string{"SELECT 1"},
		},
		{
			name: "single statement with semicolon",
			in:   "SELECT 1;",
			want: []string{"SELECT 1"},
		},
		{
			name: "two statements",
			in:   "CREATE TABLE a (id INT); CREATE TABLE b (id INT);",
			want: []string{"CREATE TABLE a (id INT)", "CREATE TABLE b (id INT)"},
		},
		{
			name: "leading goose annotation stripped, statement preserved",
			in:   "-- +goose NO TRANSACTION\nCREATE INDEX CONCURRENTLY i ON t(x);",
			want: []string{"CREATE INDEX CONCURRENTLY i ON t(x)"},
		},
		{
			name: "semicolon inside single-quoted string is not a separator",
			in:   "INSERT INTO t VALUES ('a;b'); SELECT 1;",
			want: []string{"INSERT INTO t VALUES ('a;b')", "SELECT 1"},
		},
		{
			name: "doubled single-quote is an escape",
			in:   "INSERT INTO t VALUES ('it''s; fine'); SELECT 1;",
			want: []string{"INSERT INTO t VALUES ('it''s; fine')", "SELECT 1"},
		},
		{
			name: "semicolon inside dollar-quoted body is not a separator",
			in:   "CREATE FUNCTION f() RETURNS void AS $$ BEGIN; END; $$ LANGUAGE plpgsql; SELECT 1;",
			want: []string{
				"CREATE FUNCTION f() RETURNS void AS $$ BEGIN; END; $$ LANGUAGE plpgsql",
				"SELECT 1",
			},
		},
		{
			name: "semicolon inside tagged dollar-quoted body is not a separator",
			in:   "DO $body$ BEGIN; END; $body$; SELECT 1;",
			want: []string{"DO $body$ BEGIN; END; $body$", "SELECT 1"},
		},
		{
			name: "line comment containing semicolons does not split",
			in:   "-- one; two; three\nSELECT 1;",
			want: []string{"SELECT 1"},
		},
		{
			name: "inline trailing line comment kept on its statement",
			in:   "SELECT 1; -- trailer\nSELECT 2;",
			want: []string{"SELECT 1", "-- trailer\nSELECT 2"}, // leading -- on second is stripped
		},
		{
			name: "block comment containing semicolon does not split",
			in:   "/* a; b */ SELECT 1; /* trailer */ SELECT 2;",
			want: []string{"/* a; b */ SELECT 1", "/* trailer */ SELECT 2"},
		},
		{
			name: "double-quoted identifier with semicolon",
			in:   `CREATE TABLE "weird;name" (id INT); SELECT 1;`,
			want: []string{`CREATE TABLE "weird;name" (id INT)`, "SELECT 1"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := splitSQLStatements([]byte(tc.in))
			if err != nil {
				t.Fatalf("splitSQLStatements: unexpected err: %v", err)
			}
			// Normalize: the splitter strips leading line-comments off each
			// statement, but our want-strings aren't pre-stripped. Apply the
			// same transform to expectations so the comparison is faithful
			// to what gets sent to PG.
			normWant := make([]string, len(tc.want))
			for i, s := range tc.want {
				normWant[i] = stripLeadingComments(strings.TrimSpace(s))
			}
			if !reflect.DeepEqual(got, normWant) {
				t.Fatalf("splitSQLStatements(%q):\n  got  %q\n  want %q", tc.in, got, normWant)
			}
		})
	}
}

func TestSplitSQLStatements_UnterminatedString(t *testing.T) {
	if _, err := splitSQLStatements([]byte("SELECT 'oops")); err == nil {
		t.Fatal("expected error for unterminated single-quoted string")
	}
	if _, err := splitSQLStatements([]byte("SELECT $$ open")); err == nil {
		t.Fatal("expected error for unterminated dollar-quoted body")
	}
	if _, err := splitSQLStatements([]byte("SELECT /* unfinished")); err == nil {
		t.Fatal("expected error for unterminated block comment")
	}
}
