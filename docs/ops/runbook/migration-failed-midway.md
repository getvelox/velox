# A database upgrade stopped

A migration is a versioned script that changes the database schema. This
page is for when `velox migrate`, or Velox starting with
`RUN_MIGRATIONS_ON_BOOT=true`, stopped with an error, or Velox refuses to
start because of the migration state. Back to the [runbook index](index.md).

From v0.9.0, a failed migration leaves nothing to repair: fix the cause and
run the upgrade again. The rest of this page says how to find the cause for
each kind of error.

## Before anything else

1. Make sure only one copy of Velox is trying to start. With Docker Compose,
   stop the API. It restarts every few seconds while it refuses, and with
   `RUN_MIGRATIONS_ON_BOOT=true` every restart runs the failed migration
   again from the start. A slow one, such as an index build or a data
   change on a large table, then loads the database on every restart until
   you stop it:

   ```bash
   docker compose stop velox-api
   ```

2. See the plan. With Docker Compose, use `docker compose run --rm velox-api`
   in place of `velox`:

   ```bash
   velox migrate status
   ```

   **You see:** the database's version, this release's, the state, the
   migrations still to apply, and `running now: no`. If it says
   `running now: yes`, another process is migrating: wait for it.

Then find your case below by the error text.

## A normal migration failed

**Symptom:**

```
migration 00182_add_invoice_memo.sql failed: ERROR: ... (SQLSTATE 23502). It runs in one transaction, so none of it was applied. Fix the cause and run the upgrade again.
```

**Why:** each migration runs in one transaction, and goose records the
version in that same transaction. A failure undoes all of it, so the
database is still at the version before.

**Fix:** read the Postgres error. It names the problem, often with a
`detail:` or `hint:` part: a full disk, a lost connection, a missing
permission, or rows the migration did not expect. Fix that, then run
`velox migrate` again (or start Velox).

If the error is about rows the migration did not expect, the migration
itself may need a fix: report it with the error text.

If it says `waited 5s for a table lock`, see
[An upgrade waited for a table lock](migration-lock-timeout.md).

## An index build failed

**Symptom:**

```
migration 00181_usage_rollup_index.sql failed: ERROR: could not create unique index "idx_x" (SQLSTATE 23505); detail: Key (id)=(2) is duplicated.; constraint: idx_x. It runs without a transaction, so Postgres may have left the index INVALID. Running the upgrade again is safe: the file drops the index and builds it again. Fix the cause first.
```

**Why:** an index build (`CREATE INDEX CONCURRENTLY`) does not block
writes, but it cannot run in a transaction. When it fails, Postgres can
leave the index behind, marked INVALID: Postgres keeps it up to date but
never uses it.

**Fix:** fix the cause the error names. For a unique index, `detail:`
names a duplicated key: remove or merge the duplicate rows. Then run
`velox migrate` again. Every index-build file drops its index first, so
the INVALID index goes away and is built again.

## Velox tables, but no migration record

**Symptom:**

```
migration 00180 v0.8.1 baseline failed: ERROR: function "leader_fence" already exists with same argument types (SQLSTATE 42723). This database already has Velox tables but no migration record, and none of the baseline was applied. ...
```

**Why:** the database has Velox's tables but neither `goose_db_version`
(this release's record) nor `schema_migrations` (the v0.8 record). Velox
cannot tell which version the tables are at. This happens when a backup
was restored without its migration table, or when the record was dropped
by hand.

**Fix:** restore the backup again, with its migration table, into an empty
database, or point Velox at an empty database. Then run `velox migrate`.

## A dirty database from v0.8.x

**Symptom:**

```
This database's v0.8 migration record (schema_migrations) says version 179, dirty=true. This release takes over only a clean v0.8.0 or v0.8.1 database at version 180. ...
```

**Why:** v0.9.0 takes over a database from v0.8.0 or v0.8.1 at version 180,
not dirty. Any other v0.8 state must be finished with v0.8.1 first.

**Fix:** with Velox v0.8.1, run `velox migrate`. If it reports the database
dirty, follow v0.8.1's
[A database upgrade failed midway](https://github.com/getvelox/velox/blob/v0.8.1/docs/ops/runbook/migration-failed-midway.md).
When v0.8.1's `velox migrate status` prints `version: 180, dirty: false`,
run v0.9.0's `velox migrate`.

A version above 180 came from a development build. Use an empty database.

## Two migration records

**Symptom:**

```
public.schema_migrations (version 180, dirty=false, without the comment this release adds when it takes a database over) does not match this release's record in goose_db_version (version 180). ...
```

**Why:** when v0.9.0 takes over a v0.8 database, it keeps
`schema_migrations` frozen at 180 and marks it with a table comment. Its
own record is `goose_db_version`. The two now disagree, so something
changed `schema_migrations` after the upgrade. Velox will not guess which
schema is present, and changes nothing.

**Fix:** pick the case.

- **`goose_db_version` says 180, and the record says version 180,
  dirty=false, without the comment.** The comment was lost: a copy tool
  left comments out (`pg_dump --no-comments`, some database migration
  services), or a v0.8.x backup was restored over a database still at 180.
  Either way the tables are v0.8.1's, at the 180 both records name. Put the
  comment back, then start this release:

  ```sql
  COMMENT ON TABLE public.schema_migrations IS 'velox: frozen at 180; the migration record moved to public.goose_db_version';
  ```

  This case is only for `goose_db_version` at 180. Above 180, newer
  migrations changed tables a v0.8.x backup does not have: see the next
  case.
- **A v0.8.x backup was restored over this database.** `scripts/restore.sh`
  replaces only the objects in the backup. It leaves `goose_db_version` and
  any table a newer migration created. Restore the backup into an **empty**
  database instead, then run `velox migrate`.
- **A v0.8.x binary ran on a database this release set up.** The record
  shows version 1, dirty. v0.8.x tried its first migration and failed on
  the existing tables, inside one transaction, so it changed nothing else.
  Drop its record, then start this release:

  ```sql
  DROP TABLE public.schema_migrations;
  ```

- **A v0.8.x `migrate rollback` or `migrate force` ran after the upgrade.**
  Run v0.8.1's `velox migrate` to return the database to 180. Then start
  this release, which checks the record again. If you cannot tell what the
  rollback changed, restore the backup taken before the upgrade, into an
  empty database.

## Verify

- `velox migrate status` prints `state:        up to date, nothing to apply`
  and `running now:  no`.
- Velox starts (with Docker Compose: `docker compose up -d velox-api`), and
  `/health/ready` answers 200.
