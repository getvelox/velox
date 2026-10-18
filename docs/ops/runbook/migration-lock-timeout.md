# A database upgrade waited for a table lock and stopped

A migration is a versioned script that changes the database schema. This
page is for when one could not get a lock on a table in time, so the
upgrade stopped. Back to the [runbook index](index.md).

## Symptom

`velox migrate`, or Velox starting with `RUN_MIGRATIONS_ON_BOOT=true`,
fails with:

```
migration 00181_example.sql waited 5s for a table lock and stopped: another database session, usually a long-running transaction, holds a lock on a table it changes. Postgres undid the whole migration and the database is still at version 180, so nothing needs repair ...
```

## Why

To change a table, a migration needs a lock on it. While the migration
waits for that lock, every other query on the table waits behind it, so a
migration stuck behind one long transaction can stall that table for
everyone.

To keep that wait short, Velox gives each migration 5 seconds to get its
locks. If it cannot, the migration stops. It runs in one transaction, so
it leaves nothing behind, and the database is still at the version before.
Nothing needs repair.

Index builds (`CREATE INDEX CONCURRENTLY`) have no time limit, because
they do not block reads or writes while they wait.

## Check

Find what holds the lock. In `psql` as the database owner, list the open
transactions, oldest first:

```sql
SELECT pid, now() - xact_start AS open_for, state, usename, left(query, 80) AS query
FROM pg_stat_activity
WHERE xact_start IS NOT NULL AND pid <> pg_backend_pid()
ORDER BY xact_start
LIMIT 10;
```

**You see:** one or more sessions open for longer than a few seconds.
Common causes: a report or export still running, a `psql` session left in
a transaction (`state` is `idle in transaction`), or a backup tool.

## Fix

Pick one, then run the upgrade again (`velox migrate`, or start Velox).

- **Wait.** Run the upgrade again when the long transaction has finished
  or traffic is lower.
- **End the blocking session**, if it is safe to stop it:

  ```sql
  SELECT pg_terminate_backend(12345);  -- the pid from the Check step
  ```

- **Wait longer for the lock.** Set `MIGRATION_LOCK_TIMEOUT` for this one
  run, for example `MIGRATION_LOCK_TIMEOUT=30s velox migrate`. With
  Docker Compose: `docker compose run --rm -e MIGRATION_LOCK_TIMEOUT=30s velox-api migrate`.
  While the migration waits, queries on that table wait too. `0` means no
  limit.

If the upgrade instead says `another Velox process is migrating this
database`, it waited 30 minutes for another process's migration, not for
a table. `velox migrate status` shows that process under `running now`.
See [A database upgrade stopped](migration-failed-midway.md).

## Verify

- `velox migrate status` prints `state:        up to date, nothing to apply`.
- Velox starts, and `/health/ready` answers 200.
