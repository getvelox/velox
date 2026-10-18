package migrate

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

var planFiles = []PlanFile{
	{Version: 180, Name: baselineName},
	{Version: 181, Name: "00181_usage_rollup_index.sql", NoTx: true},
	{Version: 182, Name: "00182_add_invoice_memo.sql"},
}

func TestPlanFor(t *testing.T) {
	marked := v08Record{exists: true, rows: 1, version: 180, marked: true}
	cases := []struct {
		name     string
		st       state
		state    PlanState
		database int64
		pending  []int64
		ahead    []int64
	}{
		{"empty", state{}, PlanEmpty, 0, []int64{180, 181, 182}, nil},
		{"v0.8 at 180", state{v08: v08Record{exists: true, rows: 1, version: 180}}, PlanAdopt, 180, []int64{181, 182}, nil},
		{"behind", state{gooseTable: true, applied: []int64{180}}, PlanBehind, 180, []int64{181, 182}, nil},
		{"behind by one, adopted", state{gooseTable: true, applied: []int64{180, 181}, v08: marked}, PlanBehind, 181, []int64{182}, nil},
		{"up to date", state{gooseTable: true, applied: []int64{180, 181, 182}}, PlanUpToDate, 182, nil, nil},
		{"ahead", state{gooseTable: true, applied: []int64{180, 181, 182, 183, 184}}, PlanAhead, 184, nil, []int64{183, 184}},
		{"refused", state{v08: v08Record{exists: true, rows: 1, version: 179}}, PlanRefused, 0, nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := planFor(tc.st, planFiles)
			if p.State != tc.state || p.Database != tc.database || p.Latest != 182 {
				t.Fatalf("got state %q database %d latest %d, want %q %d 182", p.State, p.Database, p.Latest, tc.state, tc.database)
			}
			var pending, ahead []int64
			for _, f := range p.Pending {
				pending = append(pending, f.Version)
			}
			for _, a := range p.Ahead {
				ahead = append(ahead, a.Version)
			}
			if !reflect.DeepEqual(pending, tc.pending) || !reflect.DeepEqual(ahead, tc.ahead) {
				t.Fatalf("pending %v ahead %v, want %v %v", pending, ahead, tc.pending, tc.ahead)
			}
			if (p.Refusal != nil) != (tc.state == PlanRefused) {
				t.Fatalf("refusal %v", p.Refusal)
			}
		})
	}
}

func TestFormatPlan(t *testing.T) {
	refusal := decide(state{gooseTable: true, applied: []int64{180}, v08: v08Record{exists: true, rows: 1, version: 179, marked: true}}).refusal()
	cases := []struct {
		name string
		plan Plan
		want string
	}{
		{
			name: "behind, with an index build",
			plan: Plan{State: PlanBehind, Database: 180, Latest: 182, Pending: planFiles[1:]},
			want: `database:     180 (goose_db_version)
this release: 182 (velox v0.9.1)
state:        behind, 2 migrations to apply
  00181_usage_rollup_index.sql             no transaction (index build)
  00182_add_invoice_memo.sql
running now:  no
next: take a backup, then run "velox migrate". A server started with RUN_MIGRATIONS_ON_BOOT=true runs it as it starts.
`,
		},
		{
			name: "empty",
			plan: Plan{State: PlanEmpty, Latest: 180, Pending: planFiles[:1]},
			want: `database:     empty
this release: 180 (velox v0.9.1)
state:        empty, 1 migration to apply
  00180 v0.8.1 baseline
running now:  no
next: take a backup, then run "velox migrate". A server started with RUN_MIGRATIONS_ON_BOOT=true runs it as it starts.
`,
		},
		{
			name: "v0.8 database, nothing after 180",
			plan: Plan{State: PlanAdopt, Database: 180, Latest: 180},
			want: `database:     180 (recorded by Velox v0.8.x)
this release: 180 (velox v0.9.1)
state:        ready to move: the next "velox migrate" records 180 in goose_db_version and changes no table
running now:  no
next: take a backup, then run "velox migrate". A server started with RUN_MIGRATIONS_ON_BOOT=true runs it as it starts.
`,
		},
		{
			name: "v0.8 database with files after 180",
			plan: Plan{State: PlanAdopt, Database: 180, Latest: 182, Pending: planFiles[2:]},
			want: `database:     180 (recorded by Velox v0.8.x)
this release: 182 (velox v0.9.1)
state:        ready to move: the next "velox migrate" records 180 in goose_db_version and changes no table
  then 1 migration to apply
  00182_add_invoice_memo.sql
running now:  no
next: take a backup, then run "velox migrate". A server started with RUN_MIGRATIONS_ON_BOOT=true runs it as it starts.
`,
		},
		{
			name: "up to date, a migration running",
			plan: Plan{State: PlanUpToDate, Database: 182, Latest: 182, Running: "yes, pid 4242 (velox-migrate)"},
			want: `database:     182 (goose_db_version)
this release: 182 (velox v0.9.1)
state:        up to date, nothing to apply
running now:  yes, pid 4242 (velox-migrate)
`,
		},
		{
			name: "ahead",
			plan: Plan{State: PlanAhead, Database: 183, Latest: 182, Ahead: []PlanVersion{{183, time.Date(2026, 11, 2, 9, 30, 0, 0, time.UTC)}}},
			want: `database:     183 (goose_db_version)
this release: 182 (velox v0.9.1)
state:        ahead, the database has migrations this release does not ship
  00183   applied 2026-11-02 09:30:00 UTC
running now:  no
next: this binary is older than the database; to go back, restore the backup taken before the upgrade
`,
		},
		{
			name: "refused",
			plan: Plan{State: PlanRefused, Database: 180, Latest: 180, Refusal: refusal,
				Records: "goose_db_version: 180; schema_migrations: version 179, dirty=false"},
			want: `database:     goose_db_version: 180; schema_migrations: version 179, dirty=false
this release: 180 (velox v0.9.1)
state:        refused, this release will not migrate this database
running now:  no

` + refusal.Error() + "\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := FormatPlan(tc.plan, "v0.9.1"); got != tc.want {
				t.Errorf("got:\n%s\nwant:\n%s", got, tc.want)
			}
		})
	}
}

func TestDescribeRecords(t *testing.T) {
	st := state{gooseTable: true, applied: []int64{180}, v08: v08Record{exists: true, rows: 1, version: 1, dirty: true}}
	if got, want := describeRecords(st), "goose_db_version: 180; schema_migrations: version 1, dirty=true"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got, want := describeRecords(state{v08: v08Record{exists: true, rows: 1, version: 179}}), "goose_db_version: none; schema_migrations: version 179, dirty=false"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	var refused *V08Error
	if p := planFor(st, planFiles); !errors.As(p.Refusal, &refused) || refused.kind != v08Mismatch {
		t.Fatalf("planFor: want M2, got %v", p.Refusal)
	}
}
