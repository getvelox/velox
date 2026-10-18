# Migrations after 180

Each file here is one goose migration, named `NNNNN_name.sql` (five digits,
lowercase name), starting at `00181`. Migration 180 is the v0.8.1 baseline
in [`../baseline/`](../baseline/), run by
[`../baseline.go`](../baseline.go). goose reads only `*.sql` and `*.go`
files, so this README is not a migration; it keeps the directory, and the
`//go:embed sql` line in [`../migrate.go`](../migrate.go), valid while
there are no files.

A normal file runs in one transaction. An index build is the only file that
runs without one, and it has a fixed shape. Both are described in
[ADR-125](../../../../docs/adr/125-goose-migrations-from-one-v081-baseline.md),
decision 6.
