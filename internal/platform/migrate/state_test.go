package migrate

import (
	"testing"
)

// T2. Every row of decide's table, with the exact message for each
// refusal. Mutations: check schema_migrations before goose (swap rows 3
// and 5), or accept an unmarked record in row 2.
func TestDecide(t *testing.T) {
	clean := v08Record{exists: true, rows: 1, version: 180}
	marked := clean
	marked.marked = true
	rec := func(version int64, dirty, isMarked bool) v08Record {
		return v08Record{exists: true, rows: 1, version: version, dirty: dirty, marked: isMarked}
	}

	cases := []struct {
		name    string
		st      state
		row     int
		act     action
		message string
	}{
		{"new database", state{}, 4, actBaseline, ""},
		{"goose table with only row 0 (a rolled-back 180)", state{gooseTable: true}, 4, actBaseline, ""},
		{"v0.8 at 180", state{v08: clean}, 5, actAdopt, ""},
		{"v0.8 at 180, already marked (adoption rolled back after the comment)", state{v08: marked}, 5, actAdopt, ""},
		{"goose, no v0.8 record", state{gooseTable: true, applied: []int64{180}}, 1, actNormal, ""},
		{"goose and the frozen record", state{gooseTable: true, applied: []int64{180, 181}, v08: marked}, 2, actNormal, ""},
		{
			"v0.8 at 179", state{v08: rec(179, false, false)}, 6, actRefuse,
			"This database's v0.8 migration record (schema_migrations) says version 179, dirty=false. " +
				"This release takes over only a clean v0.8.0 or v0.8.1 database at version 180. " +
				"Run `velox migrate` with Velox v0.8.1 first; if it reports the database dirty, follow the runbook it links. " +
				"A version above 180 came from a development build: use an empty database. Nothing was changed.",
		},
		{"v0.8 dirty at 180", state{v08: rec(180, true, false)}, 6, actRefuse, ""},
		{"v0.8 at 181", state{v08: rec(181, false, false)}, 6, actRefuse, ""},
		{"v0.8 record with no version", state{v08: v08Record{exists: true}}, 6, actRefuse, ""},
		{
			"restored over: goose 180 and an unmarked (180, false)", state{gooseTable: true, applied: []int64{180}, v08: clean}, 3, actRefuse,
			"public.schema_migrations (version 180, dirty=false, without the comment this release adds when it takes a database over) " +
				"does not match this release's record in goose_db_version (version 180). " +
				"Either a v0.8.x backup was restored over this database, or a v0.8.x binary ran `migrate`, `migrate rollback` " +
				"or `migrate force` against it after the upgrade. Velox will not guess which schema is present. " +
				"Steps: docs/ops/runbook/migration-failed-midway.md#two-migration-records. Nothing was changed.",
		},
		{"rolled back by v0.8 after adoption", state{gooseTable: true, applied: []int64{180}, v08: rec(179, false, true)}, 3, actRefuse, ""},
		{"v0.8 binary ran on a new database", state{gooseTable: true, applied: []int64{180, 181}, v08: rec(1, true, false)}, 3, actRefuse, ""},
		{
			"tables outside public", state{outside: "billing", v08: clean}, 0, actRefuse,
			`Velox v0.8 kept its tables in schema "billing" (found through search_path). ` +
				"This release keeps them in public and will not move them. " +
				"Restore the database so that its tables are in public. Nothing was changed.",
		},
		{"outside public wins over a goose record", state{outside: "billing", gooseTable: true, applied: []int64{180}}, 0, actRefuse, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := decide(tc.st)
			if d.row != tc.row || d.act != tc.act {
				t.Fatalf("decide: row %d act %d, want row %d act %d", d.row, d.act, tc.row, tc.act)
			}
			if (d.refusal() != nil) != (tc.act == actRefuse) {
				t.Fatalf("refusal %v for act %d", d.refusal(), d.act)
			}
			if tc.message != "" && d.refusal().Error() != tc.message {
				t.Fatalf("message:\n got  %s\n want %s", d.refusal().Error(), tc.message)
			}
		})
	}
}

func TestV08RecordDescribe(t *testing.T) {
	for _, tc := range []struct {
		r    v08Record
		want string
	}{
		{v08Record{exists: true}, "no version"},
		{v08Record{exists: true, rows: 1, version: 179}, "version 179, dirty=false"},
		{v08Record{exists: true, rows: 1, version: 180, dirty: true}, "version 180, dirty=true"},
		{v08Record{exists: true, rows: 1, version: 180, marked: true}, "version 180, dirty=false"},
		{v08Record{exists: true, rows: 2, version: 180}, "2 rows, highest version 180"},
	} {
		if got := tc.r.describe(); got != tc.want {
			t.Errorf("%+v: got %q, want %q", tc.r, got, tc.want)
		}
	}
}
