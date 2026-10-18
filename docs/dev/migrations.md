# Writing a database migration

A migration is a numbered SQL file that changes Velox's database schema.
This page is for contributors. It shows how to add one in three steps, the
two shapes a file can take, and the rules CI checks before any database is
involved.

Velox applies migrations with [goose](https://github.com/pressly/goose).
Migration 180 is the whole schema of Velox v0.8.1 in one generated file. New
files go in `internal/platform/migrate/sql/` and start at `00181`.

## Add a migration in three steps

Step 3 needs the local Postgres from
[CONTRIBUTING](../../CONTRIBUTING.md#development-setup)
(`docker compose up -d postgres`).

1. **Create the file.** Fetch first, so the number counts every merged
   file:

   ```bash
   git fetch origin
   make migration name=add_invoice_purchase_order
   ```

   **You see:**

   ```text
   Wrote internal/platform/migrate/sql/00181_add_invoice_purchase_order.sql
   The highest number before it was 00180, in the v0.8.1 baseline.
   ```

   The number is one above the highest file in any worktree of your clone
   or on any branch you have fetched. For an index, add `concurrent=1` and
   use the index's name: `make migration name=idx_invoices_purchase_order
   concurrent=1` (see [An index build](#an-index-build)).

2. **Write the Up and the Down, then run the checks.** The Up makes the
   change, and the Down undoes it. Replace the `TODO` lines:

   ```sql
   -- +goose Up
   -- Invoices keep the purchase-order number the buyer gives.
   ALTER TABLE invoices ADD COLUMN purchase_order TEXT;

   -- +goose Down
   ALTER TABLE invoices DROP COLUMN purchase_order;
   ```

   ```bash
   go test -short ./internal/platform/migrate/
   ```

   **You see:**

   ```text
   ok  	github.com/sagarsuperuser/velox/internal/platform/migrate	0.031s
   ```

   A file that breaks a rule fails with its name and the reason. Before you
   replace the `TODO` lines, for example:

   ```text
   lint_test.go:30: sql/00181_add_invoice_purchase_order.sql: the Up section has no statement
   ```

3. **Apply it to your local database, roll it back, and apply it again.**
   This proves the Down undoes the Up.

   ```bash
   export DATABASE_URL="postgres://velox:velox@localhost:5432/velox?sslmode=disable"
   go run ./cmd/velox migrate status
   go run ./cmd/velox migrate
   go run ./cmd/velox migrate rollback
   go run ./cmd/velox migrate
   ```

   **You see** (on an empty database, so the baseline runs first):

   ```text
   $ go run ./cmd/velox migrate status
   database:     empty
   this release: 181 (velox dev)
   state:        empty, 2 migrations to apply
     00180 v0.8.1 baseline
     00181_add_invoice_purchase_order.sql
   running now:  no
   next: take a backup, then run "velox migrate". A server started with RUN_MIGRATIONS_ON_BOOT=true runs it as it starts.
   $ go run ./cmd/velox migrate
   2026/10/18 01:00:33 INFO migration applied migration="00180 v0.8.1 baseline" version=180 duration_ms=493
   2026/10/18 01:00:33 INFO migration applied migration=00181_add_invoice_purchase_order.sql version=181 duration_ms=4
   2026/10/18 01:00:33 INFO migrations applied version=181 applied=2 duration_ms=507
   migrations applied successfully
   $ go run ./cmd/velox migrate rollback
   rolled back to version 180
   $ go run ./cmd/velox migrate
   2026/10/18 01:00:33 INFO migration applied migration=00181_add_invoice_purchase_order.sql version=181 duration_ms=2
   2026/10/18 01:00:33 INFO migrations applied version=181 applied=1 duration_ms=5
   migrations applied successfully
   ```

   `velox migrate rollback` undoes one migration at a time, and only when
   `APP_ENV` is `local` (the default when it is unset). It never goes below
   180.

Then commit the file with the code that uses it. CI applies every migration
to a new database and to one upgraded from v0.8.1, and checks that the two
end up identical.

## The two shapes of a file

### A normal file

goose runs the whole file in one transaction, together with the row that
records it as applied. If any statement fails, Postgres undoes the whole
file and nothing is recorded: fix the cause and run it again.

- **No transaction control.** No `BEGIN`, `COMMIT`, `ROLLBACK`,
  `SAVEPOINT` or `END`. A `COMMIT` would make part of the file permanent
  with no record of it.
- **`SET LOCAL`, never `SET`.** goose runs every file of one
  `velox migrate` on the same database connection. A plain `SET`, a `RESET`
  or `set_config(..., false)` would carry into the files after this one.
  `SET LOCAL` ends with the file's transaction.
- **Put a multi-line `$$` body between two annotation lines**, as below.
  goose ends a statement at every line that ends in `;`, so without them it
  cuts a function body apart, and Postgres rejects the pieces.

  ```sql
  -- +goose StatementBegin
  CREATE FUNCTION touch_updated_at() RETURNS trigger LANGUAGE plpgsql AS $$
  BEGIN
    NEW.updated_at := now();
    RETURN NEW;
  END
  $$;
  -- +goose StatementEnd
  ```

- **Keep `+goose` out of comments.** goose reads any comment line that
  contains it as an annotation, and fails on one it does not know.

### An index build

`CREATE INDEX CONCURRENTLY` builds an index without blocking writes to the
table, but it cannot run inside a transaction. It is the only statement
that goes in a file without one, and that file has exactly this shape:

```sql
-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = 0;
DROP INDEX CONCURRENTLY IF EXISTS idx_invoices_purchase_order;
CREATE INDEX CONCURRENTLY idx_invoices_purchase_order ON invoices (purchase_order);
RESET lock_timeout;
-- +goose Down
SET lock_timeout = 0;
DROP INDEX CONCURRENTLY IF EXISTS idx_invoices_purchase_order;
RESET lock_timeout;
```

`make migration name=idx_invoices_purchase_order concurrent=1` writes it,
and you replace `your_table (your_column)`. `UNIQUE` may follow `CREATE`.
One index goes in each file, with a name no earlier migration used, and
never with `IF NOT EXISTS`. Each statement stays on a line of its own:
goose sends this file one statement at a time, and two statements sent
together would run in a transaction.

Why each line is there:

- **The DROP before the build.** goose records a file without a
  transaction only after the whole file has run. If Velox stops between the
  build and that record, the index exists but the file is not recorded. The
  next run drops the index and builds it again, so running the file again
  is always safe. This is also why the name must be new: the DROP would
  remove any older index of that name.
- **No `IF NOT EXISTS`.** A failed build can leave an INVALID index, one
  Postgres keeps but never uses. `IF NOT EXISTS` would find it, skip the
  build, and record the file as done.
- **`SET lock_timeout = 0`, then `RESET`.** A concurrent build waits for
  every transaction that started before it. The usual limit on lock waits
  (`MIGRATION_LOCK_TIMEOUT`, 5 seconds by default) would cancel it, so the
  file lifts the limit for the build and puts it back for the files after
  it.

A file that only drops an index swaps the two sections: its Up is
`SET`, `DROP`, `RESET`, and its Down builds the index again.

## Common changes

Every migration must work on a table that already holds millions of rows,
while Velox keeps serving: installs upgrade with their data in place.

- **Add a column.** `ADD COLUMN purchase_order TEXT`, or
  `NOT NULL DEFAULT` with a constant. Both are instant: Postgres stores the
  default without rewriting the table.
- **Add an index.** Use an [index build](#an-index-build).
- **Add a CHECK or a foreign key.** Write `ADD CONSTRAINT ... NOT VALID` in
  one file, then `VALIDATE CONSTRAINT` in the next file. `NOT VALID`
  checks only new rows, so it is instant. `VALIDATE` checks the old rows
  without blocking writes, but only in a file of its own. In the same file,
  the `ADD`'s lock is still held while every row is checked.
- **Add a UNIQUE constraint.** Build the index with
  `CREATE UNIQUE INDEX CONCURRENTLY`, then write
  `ADD CONSTRAINT ... UNIQUE USING INDEX ...` in the next file.
- **Make a column `NOT NULL`.**
  1. One file adds `CHECK (col IS NOT NULL) NOT VALID`.
  2. The next file runs `VALIDATE CONSTRAINT`, then `SET NOT NULL`, then
     drops the CHECK. Postgres trusts the valid CHECK and skips the scan.
     This file needs the [opt-out line](#what-the-checks-enforce), because
     the check cannot see the CHECK.
- **Add a value to a Go enum stored in a column.** Change the column's
  CHECK constraint in the same PR. A value the CHECK does not list fails
  every insert that uses it. Drop the old constraint and add the new one
  `NOT VALID`, then `VALIDATE` it in the next file.

**Never edit a file once it is merged.** A database that already ran it
has recorded its number and never runs it again, so an edit reaches only
new installs. Write a new file instead.

## What the checks enforce

`go test -short ./internal/platform/migrate/` runs these on every file in
`sql/`. CI runs them in the `unit` job.

| Rule | Why |
|---|---|
| The name is `NNNNN_name.sql`: five digits, an underscore, lowercase letters, digits and underscores, directly in `sql/` | goose skips many other names without a word, so the file would never run |
| Numbers go up by one from `00181`, with no gap or duplicate | A file merged since your branch started shows up here as a duplicate when you rebase, so you renumber before merging |
| Exactly one `-- +goose Up` line, then one `-- +goose Down` line, each followed by at least one statement | The Down is what `velox migrate rollback` runs while you develop the file |
| Annotations are only `Up`, `Down`, `StatementBegin`, `StatementEnd` and `NO TRANSACTION` | `ENVSUB` would make the SQL depend on the environment of whoever runs it |
| Every statement ends with `;`, and a multi-line `$$` body sits between `StatementBegin` and `StatementEnd` | goose cuts statements at lines that end in `;` |
| A normal file has no transaction control and uses only `SET LOCAL` | See [A normal file](#a-normal-file) |
| A file with `NO TRANSACTION` has exactly the [index-build shape](#an-index-build), one statement per line, with a new index name; a drop names an index that exists | See [An index build](#an-index-build) |
| No statement that blocks writes on an existing table: a plain `CREATE INDEX`, an `ADD CONSTRAINT` without `NOT VALID` or `USING INDEX`, `SET NOT NULL`, or a `VALIDATE` after another change to the same table in the file | On a large table, the lock stops every write until the statement ends. A table the same file creates is exempt |

When blocking is safe, for example on a table known to stay small, say so
in the file. The reason must be at least 15 characters:

```sql
-- velox:blocking-ddl-ok: fx_rates has under 200 rows on every install
```

## Related

[ADR-125](../adr/125-goose-migrations-from-one-v081-baseline.md) records
why Velox uses goose with one baseline. Operators whose upgrade stopped
start at the [runbook](../ops/runbook/migration-failed-midway.md).
