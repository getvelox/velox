package migrate

import (
	"database/sql"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
)

// The checks every migration file after 180 must pass (ADR-125, decision
// 6). They run with -short, so a bad file fails the unit job, before any
// database is involved. docs/dev/migrations.md explains the same rules to a
// contributor.
//
// lintMigrationDir runs them all on one directory. TestMigrationFiles runs
// it on sql/. TestLintMigrationDir runs it on small fixture directories,
// with at least one fixture that each rule must fail, so no rule goes
// untested while sql/ holds no file.

// TestMigrationFiles is the CI gate. It reads sql/ from disk, not the
// embedded copy: //go:embed leaves out a name that starts with "." or "_",
// and such a file must fail here instead of quietly never shipping.
func TestMigrationFiles(t *testing.T) {
	for _, f := range lintMigrationDir(os.DirFS("sql"), baselineSQL) {
		t.Error(f)
	}
}

// migrationFileName is the one name a migration file may have: five
// digits, an underscore, a lowercase name, .sql. goose itself accepts more
// (any number before the first underscore) and skips many other names
// without a word, so the stricter rule keeps every file one goose runs.
var migrationFileName = regexp.MustCompile(`^\d{5}_[a-z0-9_]+\.sql$`)

// lintMigrationDir checks a directory of migration files after the
// baseline and returns one line per problem, each naming its file.
func lintMigrationDir(fsys fs.FS, baseline string) []string {
	var out []string
	add := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }

	var named []string
	walkErr := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case path == ".":
			return nil
		case d.IsDir():
			add("sql/%s/: goose reads only the top level of sql/, so no file in this directory would run", path)
			return fs.SkipDir
		case path == "README.md":
			return nil
		case !migrationFileName.MatchString(path):
			add("sql/%s: a migration file is named NNNNN_name.sql: five digits, an underscore, then lowercase letters, "+
				"digits and underscores. goose skips many other names without a word, so this file might never run. "+
				"`make migration name=...` writes the right name", path)
			return nil
		}
		named = append(named, path)
		return nil
	})
	if walkErr != nil {
		add("read sql/: %v", walkErr)
		return out
	}

	// goose collects the files itself, so this is the list Up would run.
	// The handle never connects: listing sources reads only the files.
	db, err := sql.Open("pgx", "postgres://lint.invalid/unused")
	if err != nil {
		add("open an unconnected handle: %v", err)
		return out
	}
	defer func() { _ = db.Close() }()
	p, err := newProvider(db, fsys, false)
	if err != nil {
		add("goose cannot collect sql/: %v", err)
		return out
	}

	known := indexSet{}
	known.addFrom([]byte(baseline))
	collected := map[string]bool{}
	prev := baselineVersion
	for _, s := range p.ListSources() {
		if s.Type != goose.TypeSQL {
			continue // migration 180, the Go migration that runs the baseline
		}
		collected[s.Path] = true
		if s.Version != prev+1 {
			add("sql/%s: version %d follows %d. Versions after 180 go up by one with no gap, so this file should be %05d",
				s.Path, s.Version, prev, prev+1)
		}
		prev = s.Version
		body, err := fs.ReadFile(fsys, s.Path)
		if err != nil {
			add("sql/%s: %v", s.Path, err)
			continue
		}
		for _, f := range lintMigrationFile(body, isNoTxFile(fsys, s.Path), known) {
			add("sql/%s: %s", s.Path, f)
		}
	}
	for _, n := range named {
		if !collected[n] {
			add("sql/%s: goose does not collect this file, so it would never run. Versions start at %05d", n, baselineVersion+1)
		}
	}
	return out
}

// lintMigrationFile checks one file and then adds the indexes it creates
// to known, for the files after it.
func lintMigrationFile(body []byte, noTx bool, known indexSet) []string {
	up, down, out := gooseSections(body)
	if len(out) > 0 {
		return out
	}
	for _, sec := range []struct {
		name string
		body []byte
	}{{"Up", up}, {"Down", down}} {
		chunks, problems := gooseChunks(sec.body)
		for _, p := range problems {
			out = append(out, fmt.Sprintf("the %s section: %s", sec.name, p))
		}
		stmts, err := splitSQLStatements(sec.body)
		switch {
		case err != nil:
			out = append(out, fmt.Sprintf("the %s section: %v", sec.name, err))
		case len(stmts) == 0 && sec.name == "Up":
			out = append(out, "the Up section has no statement")
		case len(stmts) == 0:
			out = append(out, "the Down section has no statement. Write one that undoes the Up, so `velox migrate rollback` "+
				"works while you develop the file")
		case noTx && len(problems) == 0 && len(chunks) != len(stmts):
			out = append(out, fmt.Sprintf("the %s section: put each statement on a line of its own. goose sends a file without "+
				"a transaction one line-ending \";\" at a time, and two statements sent together run in a transaction, "+
				"which CONCURRENTLY refuses", sec.name))
		}
	}
	if len(out) > 0 {
		return out
	}

	if noTx {
		out = append(out, indexBuildFindings(up, down, known)...)
	} else {
		tx, _ := txControlFindings(body) // the body split cleanly above
		if len(tx) > 0 {
			out = append(out, fmt.Sprintf("controls its own transaction (%s). goose already runs the file in one "+
				"transaction, together with its version row, so remove these statements", strings.Join(tx, ", ")))
		}
		set, _ := sessionSettingFindings(body)
		for _, f := range set {
			out = append(out, f+". goose runs every file of one command on one connection, so the setting would carry "+
				"into the files after this one. Use SET LOCAL, which ends with the file's transaction")
		}
	}

	blocking, _ := blockingDDLFindings(up)
	if len(blocking) > 0 && len(blockingDDLOptOutReason(body)) < minOptOutReason {
		out = append(out, fmt.Sprintf("blocks writes on a live table:\n  - %s\n"+
			"Use the write-safe form (docs/dev/migrations.md), or, if blocking is safe here, add a line `%s <why>` "+
			"(at least %d characters)", strings.Join(blocking, "\n  - "), blockingDDLOptOut, minOptOutReason))
	}

	known.addFrom(up)
	return out
}

// gooseSections splits a goose file into its Up and Down sections, reading
// annotations as goose does (gooseAnnotation). It also checks the
// annotations themselves: exactly one Up, then exactly one Down, and only
// the five Velox uses. ENVSUB, for one, would make a file's SQL depend on
// the environment of whoever runs the migration.
func gooseSections(body []byte) (up, down []byte, problems []string) {
	var upB, downB strings.Builder
	section := ""
	ups, downs := 0, 0
	for i, line := range strings.Split(string(body), "\n") {
		n := i + 1
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "--") && strings.Contains(line, "+goose") {
			cmd, ok := gooseAnnotation(line)
			if !ok {
				problems = append(problems, fmt.Sprintf("line %d: goose refuses an annotation with space before it", n))
				continue
			}
			switch strings.ToUpper(cmd) {
			case "UP":
				ups++
				section = "up"
			case "DOWN":
				downs++
				if section != "up" {
					problems = append(problems, fmt.Sprintf("line %d: the -- +goose Down line must come after the -- +goose Up line", n))
				}
				section = "down"
			case "STATEMENTBEGIN", "STATEMENTEND":
				// Kept in the section: gooseChunks reads them. To every other
				// check they are comments.
				switch section {
				case "up":
					upB.WriteString(line + "\n")
				case "down":
					downB.WriteString(line + "\n")
				}
			case "NO TRANSACTION":
			default:
				problems = append(problems, fmt.Sprintf("line %d: annotation %q is not allowed. Velox files use only Up, "+
					"Down, StatementBegin, StatementEnd and NO TRANSACTION; a comment must not contain \"+goose\"", n, cmd))
			}
			continue
		}
		switch section {
		case "":
			if trimmed != "" && !strings.HasPrefix(trimmed, "--") {
				problems = append(problems, fmt.Sprintf("line %d: SQL before the -- +goose Up line", n))
			}
		case "up":
			upB.WriteString(line + "\n")
		case "down":
			downB.WriteString(line + "\n")
		}
	}
	if ups != 1 {
		problems = append(problems, fmt.Sprintf("has %d -- +goose Up lines; a file needs exactly one", ups))
	}
	if downs != 1 {
		problems = append(problems, fmt.Sprintf("has %d -- +goose Down lines; a file needs exactly one, after the Up", downs))
	}
	return []byte(upB.String()), []byte(downB.String()), problems
}

// gooseChunks cuts a section into statements the way goose v3.27.0 sends
// them (internal/sqlparser): a statement ends at every line whose last word
// before any "--" comment ends in ";", and everything between
// StatementBegin and StatementEnd is one statement. It returns the
// statements and the problems goose would hit. One is a statement that
// leaves a $$ body or a quoted string open: goose cut it at a ";" inside
// the body, and Postgres would reject both halves. Decision 6 of ADR-125
// puts such a body between StatementBegin and StatementEnd.
func gooseChunks(section []byte) (chunks, problems []string) {
	var buf strings.Builder
	inBlock := false
	flush := func() {
		chunk := strings.TrimSpace(buf.String())
		buf.Reset()
		if chunk == "" {
			return
		}
		if _, err := splitSQLStatements([]byte(chunk)); err != nil {
			problems = append(problems, fmt.Sprintf("goose ends a statement at a line ending in \";\" inside a $$ body or a "+
				"quoted string (%q), so Postgres would get it cut in two. Put the whole statement between "+
				"-- +goose StatementBegin and -- +goose StatementEnd", firstLine(chunk)))
		}
		chunks = append(chunks, chunk)
	}
	for _, line := range strings.Split(string(section), "\n") {
		if cmd, ok := gooseAnnotation(line); ok {
			switch strings.ToUpper(cmd) {
			case "STATEMENTBEGIN":
				if inBlock {
					problems = append(problems, "a StatementBegin inside another one")
				}
				flush()
				inBlock = true
				continue
			case "STATEMENTEND":
				if !inBlock {
					problems = append(problems, "a StatementEnd with no StatementBegin before it")
				}
				flush()
				inBlock = false
				continue
			}
		}
		trimmed := strings.TrimSpace(line)
		if buf.Len() == 0 && (trimmed == "" || strings.HasPrefix(trimmed, "--")) {
			continue
		}
		buf.WriteString(line + "\n")
		if !inBlock && gooseEndsStatement(line) {
			flush()
		}
	}
	switch {
	case inBlock:
		problems = append(problems, "a StatementBegin with no StatementEnd after it")
	case strings.TrimSpace(buf.String()) != "":
		problems = append(problems, fmt.Sprintf("the last statement (%q) does not end with \";\", and goose refuses the file", firstLine(buf.String())))
	}
	return chunks, problems
}

// gooseEndsStatement is goose's endsWithSemicolon: the last word of the
// line before any word that starts a "--" comment ends in ";".
func gooseEndsStatement(line string) bool {
	last := ""
	for _, w := range strings.Fields(line) {
		if strings.HasPrefix(w, "--") {
			break
		}
		last = w
	}
	return strings.HasSuffix(last, ";")
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + " ..."
	}
	return s
}

// ---- Index builds ---------------------------------------------------------

// indexBuildTemplate is the one shape a file without a transaction may
// have. goose writes such a file's version row only after the file, so a
// crash between the build and the row leaves an index with no row: the
// file drops the index first, and running it again is always safe. That is
// the promise FileError makes for a failed index build.
const indexBuildTemplate = `
    -- +goose NO TRANSACTION
    -- +goose Up
    SET lock_timeout = 0;
    DROP INDEX CONCURRENTLY IF EXISTS idx_x;
    CREATE [UNIQUE] INDEX CONCURRENTLY idx_x ON t (c);
    RESET lock_timeout;
    -- +goose Down
    SET lock_timeout = 0;
    DROP INDEX CONCURRENTLY IF EXISTS idx_x;
    RESET lock_timeout;
A file that only drops an index swaps the two sections. ` + "`make migration name=idx_x concurrent=1`" + ` writes one`

var (
	buildSetLockTimeout   = regexp.MustCompile(`^SET LOCK_TIMEOUT (?:=|TO) '?0'?$`)
	buildDropIndex        = regexp.MustCompile(`^DROP INDEX CONCURRENTLY IF EXISTS (\S+)$`)
	buildCreateIndex      = regexp.MustCompile(`^CREATE (?:UNIQUE )?INDEX CONCURRENTLY (\S+) ON \S`)
	buildResetLockTimeout = regexp.MustCompile(`^RESET LOCK_TIMEOUT$`)
)

// indexSection matches one section of an index-build file: "build" is SET,
// DROP, CREATE, RESET, and "drop" is SET, DROP, RESET. It returns "" for
// any other shape, with the names the DROP and the CREATE use.
func indexSection(body []byte) (shape, dropped, created string) {
	stmts, err := splitSQLStatements(body)
	if err != nil || len(stmts) < 3 || len(stmts) > 4 {
		return "", "", ""
	}
	s := make([]string, len(stmts))
	for i, raw := range stmts {
		s[i] = normalizeDDL(raw)
	}
	drop := buildDropIndex.FindStringSubmatch(s[1])
	if !buildSetLockTimeout.MatchString(s[0]) || drop == nil || !buildResetLockTimeout.MatchString(s[len(s)-1]) {
		return "", "", ""
	}
	if len(s) == 3 {
		return "drop", indexKey(drop[1]), ""
	}
	create := buildCreateIndex.FindStringSubmatch(s[2])
	if create == nil {
		return "", "", ""
	}
	return "build", indexKey(drop[1]), indexKey(create[1])
}

// indexBuildFindings checks a file that runs without a transaction against
// indexBuildTemplate, and checks the index's name: a build needs a name no
// earlier migration used, because its first step drops any index of that
// name; a drop needs an index that exists, or DROP ... IF EXISTS does
// nothing and the file is still recorded as done.
func indexBuildFindings(up, down []byte, known indexSet) []string {
	if strings.Contains(normalizeDDL(string(up)+"\n"+string(down)), "IF NOT EXISTS") {
		return []string{"IF NOT EXISTS is not allowed in an index build: after a failed run it finds the INVALID index " +
			"left behind, skips the build, and the file is recorded as done with an index Postgres never uses"}
	}
	us, ud, uc := indexSection(up)
	ds, dd, dc := indexSection(down)
	switch {
	case us == "" || ds == "":
		return []string{"a file with -- +goose NO TRANSACTION must have exactly this shape, one index per file:" + indexBuildTemplate}
	case us == "build" && ds == "drop":
		if uc != ud || dd != ud {
			return []string{fmt.Sprintf("the DROP and CREATE statements name different indexes (%s, %s, %s); they must all name one", ud, uc, dd)}
		}
		if kind, ok := known[ud]; ok {
			return []string{fmt.Sprintf("index %s already exists (%s, in the baseline or an earlier file). An index build needs "+
				"a new name, because its first step drops any index of that name", ud, kind)}
		}
	case us == "drop" && ds == "build":
		if dd != ud || dc != ud {
			return []string{fmt.Sprintf("the DROP and CREATE statements name different indexes (%s, %s, %s); they must all name one", ud, dd, dc)}
		}
		switch known[ud] {
		case "":
			return []string{fmt.Sprintf("drops %s, which neither the baseline nor an earlier file creates, so DROP ... IF EXISTS "+
				"would do nothing and the file would still be recorded as done", ud)}
		case kindConstraint:
			return []string{fmt.Sprintf("%s belongs to a constraint, and Postgres refuses DROP INDEX on it. Drop the constraint "+
				"in a normal file instead", ud)}
		}
	default:
		return []string{"the Down must undo the Up: a build's Down drops the index, and a drop's Down builds it again:" + indexBuildTemplate}
	}
	return nil
}

const (
	kindIndex      = "an index"
	kindConstraint = "a constraint's index"
)

var (
	ddlIndexName      = regexp.MustCompile(`^CREATE (?:UNIQUE )?INDEX (?:CONCURRENTLY )?(?:IF NOT EXISTS )?(\S+) ON `)
	ddlConstraintName = regexp.MustCompile(`\bCONSTRAINT (\S+) (?:PRIMARY KEY|UNIQUE|EXCLUDE)\b`)
)

// indexSet holds the index names migrations have created: plain indexes,
// and the indexes behind PRIMARY KEY, UNIQUE and EXCLUDE constraints, which
// share their constraint's name.
type indexSet map[string]string

func (s indexSet) addFrom(body []byte) {
	stmts, err := splitSQLStatements(body)
	if err != nil {
		return
	}
	for _, raw := range stmts {
		stmt := normalizeDDL(raw)
		if m := ddlIndexName.FindStringSubmatch(stmt); m != nil {
			s[indexKey(m[1])] = kindIndex
		}
		for _, m := range ddlConstraintName.FindAllStringSubmatch(stmt, -1) {
			s[indexKey(m[1])] = kindConstraint
		}
	}
}

// indexKey is an index name from a normalized statement, without quotes
// or the public schema, in lower case.
func indexKey(raw string) string {
	return strings.ToLower(ddlTableName(raw))
}

// ---- Session settings -----------------------------------------------------

var (
	sqlSet              = regexp.MustCompile(`^SET\b`)
	sqlSetForTx         = regexp.MustCompile(`^SET (?:LOCAL|CONSTRAINTS|TRANSACTION)\b`)
	sqlReset            = regexp.MustCompile(`^RESET\b`)
	sqlSetConfigSession = regexp.MustCompile(`^SELECT (?:PG_CATALOG\.)?SET_CONFIG ?\(.*, ?FALSE ?\)$`)
)

// sessionSettingFindings lists the statements of a normal file that change
// a setting for the rest of the session instead of the transaction: SET
// without LOCAL, RESET, and set_config(..., false).
func sessionSettingFindings(body []byte) ([]string, error) {
	stmts, err := splitSQLStatements(body)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, raw := range stmts {
		s := normalizeDDL(raw)
		if (sqlSet.MatchString(s) && !sqlSetForTx.MatchString(s)) || sqlReset.MatchString(s) || sqlSetConfigSession.MatchString(s) {
			out = append(out, fmt.Sprintf("%q changes a setting for the whole session", s))
		}
	}
	return out, nil
}

// ---- Write-blocking DDL (#987) --------------------------------------------

// A migration that builds an index or adds a constraint the plain way
// holds a lock that blocks every write to the table until it finishes: on
// a large table, that is an outage of the table for the whole build. The
// write-safe forms are
//
//   - CREATE INDEX CONCURRENTLY, in an index-build file;
//   - ADD CONSTRAINT ... NOT VALID, then VALIDATE CONSTRAINT in the next
//     file. A file is one transaction, so a VALIDATE in the same file would
//     check every row while still holding the lock the ADD took;
//   - ADD CONSTRAINT ... UNIQUE / PRIMARY KEY USING INDEX, on an index
//     built CONCURRENTLY first;
//   - SET NOT NULL after a validated CHECK (col IS NOT NULL) constraint,
//     which lets Postgres skip the table scan.
//
// Statements on a table the same file creates are exempt: the table is
// empty and nobody else can see it yet.
//
// A migration that must use a blocking form anyway (a table known to be
// small, say) says so with a line
//
//	-- velox:blocking-ddl-ok: <why this is safe>
//
// The check covers each file's Up section. Migration 180 is the v0.8.1
// baseline, which every install already has.

const blockingDDLOptOut = "-- velox:blocking-ddl-ok:"

// minOptOutReason keeps the reason a sentence, not a placeholder.
const minOptOutReason = 15

var (
	ddlLineComment   = regexp.MustCompile(`--[^\n]*`)
	ddlSpace         = regexp.MustCompile(`\s+`)
	ddlCreateTable   = regexp.MustCompile(`^CREATE (?:UNLOGGED )?TABLE (?:IF NOT EXISTS )?([^\s(]+)`)
	ddlCreateIndex   = regexp.MustCompile(`^CREATE (?:UNIQUE )?INDEX (CONCURRENTLY )?.*? ON (?:ONLY )?([^\s(]+)`)
	ddlAlterTable    = regexp.MustCompile(`^ALTER TABLE (?:IF EXISTS )?(?:ONLY )?([^\s(]+) (.*)$`)
	ddlAddCheckOrFK  = regexp.MustCompile(`\bADD (?:CONSTRAINT \S+ )?(?:FOREIGN KEY|CHECK)\b`)
	ddlAddUniqueOrPK = regexp.MustCompile(`\bADD (?:CONSTRAINT \S+ )?(?:UNIQUE|PRIMARY KEY)\b`)
	ddlSetNotNull    = regexp.MustCompile(`\bSET NOT NULL\b`)
	ddlValidate      = regexp.MustCompile(`\bVALIDATE CONSTRAINT\b`)
	ddlValidateOnly  = regexp.MustCompile(`^VALIDATE CONSTRAINT \S+$`)
)

// normalizeDDL strips line comments, collapses whitespace and upper-cases
// a statement, so the patterns above can stay simple.
func normalizeDDL(stmt string) string {
	s := ddlLineComment.ReplaceAllString(stmt, " ")
	return strings.ToUpper(strings.TrimSpace(ddlSpace.ReplaceAllString(s, " ")))
}

func ddlTableName(raw string) string {
	name := strings.ReplaceAll(raw, `"`, "")
	return strings.TrimPrefix(name, "PUBLIC.")
}

// blockingDDLFindings lists the write-blocking statements in a migration
// body, each as a short explanation.
func blockingDDLFindings(body []byte) ([]string, error) {
	stmts, err := splitSQLStatements(body)
	if err != nil {
		return nil, err
	}
	created := map[string]bool{}
	locked := map[string]bool{} // tables an earlier ALTER TABLE in this file holds a write-blocking lock on
	var out []string
	for _, raw := range stmts {
		s := normalizeDDL(raw)
		if m := ddlCreateTable.FindStringSubmatch(s); m != nil {
			created[ddlTableName(m[1])] = true
			continue
		}
		if m := ddlCreateIndex.FindStringSubmatch(s); m != nil {
			table := ddlTableName(m[2])
			if m[1] == "" && !created[table] {
				out = append(out, fmt.Sprintf("CREATE INDEX on %s without CONCURRENTLY blocks writes to %s until the index is built", strings.ToLower(table), strings.ToLower(table)))
			}
			continue
		}
		m := ddlAlterTable.FindStringSubmatch(s)
		if m == nil {
			continue
		}
		table, rest := ddlTableName(m[1]), m[2]
		if created[table] {
			continue
		}
		lt := strings.ToLower(table)
		if ddlAddCheckOrFK.MatchString(rest) && !strings.Contains(rest, "NOT VALID") {
			out = append(out, fmt.Sprintf("ADD CONSTRAINT (CHECK / FOREIGN KEY) on %s without NOT VALID blocks writes to %s while every row is checked", lt, lt))
		}
		if ddlAddUniqueOrPK.MatchString(rest) && !strings.Contains(rest, "USING INDEX") {
			out = append(out, fmt.Sprintf("ADD UNIQUE / PRIMARY KEY on %s without USING INDEX blocks writes to %s while its index is built", lt, lt))
		}
		if ddlSetNotNull.MatchString(rest) {
			out = append(out, fmt.Sprintf("SET NOT NULL on %s blocks writes to %s while every row is checked, unless a validated CHECK (col IS NOT NULL) exists", lt, lt))
		}
		if ddlValidate.MatchString(rest) && (locked[table] || !ddlValidateOnly.MatchString(rest)) {
			out = append(out, fmt.Sprintf("VALIDATE CONSTRAINT on %s in the same transaction as another change to %s checks every row "+
				"while that change's lock blocks writes to %s; validate in the next file", lt, lt, lt))
		}
		if !ddlValidateOnly.MatchString(rest) {
			locked[table] = true
		}
	}
	return out, nil
}

// blockingDDLOptOutReason returns the reason written after the opt-out
// marker, or "" when the file has none.
func blockingDDLOptOutReason(body []byte) string {
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, blockingDDLOptOut) {
			return strings.TrimSpace(strings.TrimPrefix(line, blockingDDLOptOut))
		}
	}
	return ""
}

// ---- Transaction control --------------------------------------------------

var txControl = regexp.MustCompile(`^(BEGIN|START TRANSACTION|COMMIT|END|ROLLBACK|SAVEPOINT|RELEASE)\b`)

// txControlFindings lists the statements of a migration file that control
// the transaction. goose wraps a normal file in one transaction and writes
// its version row in that same transaction; a COMMIT inside the file would
// make part of it permanent with no version row (ADR-125, decision 6).
func txControlFindings(body []byte) ([]string, error) {
	stmts, err := splitSQLStatements(body)
	if err != nil {
		return nil, err
	}
	var out []string
	for i, raw := range stmts {
		if s := normalizeDDL(raw); txControl.MatchString(s) {
			out = append(out, fmt.Sprintf("statement %d (%q)", i+1, s))
		}
	}
	return out, nil
}
