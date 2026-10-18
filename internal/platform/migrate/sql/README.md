# Migrations after 180

Each file here is one goose migration, named `NNNNN_name.sql` (five digits,
lowercase name), starting at `00181`. `make migration name=...` writes the
next one. [Writing a database migration](../../../../docs/dev/migrations.md)
has the steps, the two shapes a file can take, and the rules the `-short`
tests in [`../lint_test.go`](../lint_test.go) check.

Migration 180 is the v0.8.1 baseline in [`../baseline/`](../baseline/), run
by [`../baseline.go`](../baseline.go). goose reads only `*.sql` and `*.go`
files, so this README is not a migration. It also keeps the
`//go:embed sql` line in [`../migrate.go`](../migrate.go) valid while the
directory has no migration.
