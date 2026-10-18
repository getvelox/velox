# ADR-124: Run migrations with plain golang-migrate; each file sets its own lock limit

**Date:** 2026-10-17
**Status:** Superseded by [ADR-125](125-goose-migrations-from-one-v081-baseline.md) on 2026-10-18, before it was built. The ADR-073 runner it would have replaced ran up to v0.8.1; from v0.9.0 goose runs the migrations.
**Amends:** ADR-073 (decision 3, the hybrid migration runner, is replaced; its deadlock claim is corrected)

## Summary

Superseded by [ADR-125](125-goose-migrations-from-one-v081-baseline.md) on 2026-10-18, before it was built: Velox moves to goose instead. This ADR proposed the following.

Velox applies database migrations with golang-migrate as it is, with no
Velox runner around it. Every new migration file starts with
`SET LOCAL lock_timeout = '5s';`. A file that cannot get a table lock in
5 seconds stops and leaves nothing behind. An index built with
`CONCURRENTLY` sits alone in its own file with no time limit, so it waits
for older transactions and finishes valid. One process applies migrations
at a time: the API container under Compose, or one `velox migrate` step
before several copies start. This replaces the migration runner from
ADR-073.

## Context

A migration is a versioned SQL file that changes the database schema.
Velox has 178 of them (0001 to 0180; 0143 and 0166 do not exist). They are
embedded in the binary and applied by golang-migrate v4.19.1.

golang-migrate sends each file to Postgres as one query string. Postgres
runs a query string that holds several statements as one implicit
transaction, so each file is all-or-nothing. `CREATE INDEX CONCURRENTLY`
cannot run inside a transaction, so it needs different handling.

Three files build or drop such an index: 0062, 0112 and 0130. For them,
Velox wrapped the library in its own "hybrid" runner (ADR-073). Files with
a `-- velox:no-transaction` header go through Velox code. The other files
go through the library one step at a time, under a Velox lock that polls.
The runner runs on every install, because 0062 is always embedded.

With the repair and lock-timeout code added since (#967, #987), the
package holds about 1535 lines of code and 1694 lines of tests. About 450
lines of that code exist only for the three files. About 75 more copy the
library's own bookkeeping.

#987 put a 5-second `lock_timeout` on the migration connection. A
migration that cannot get a table lock now stops, so app queries on that
table do not queue behind it. The runner exempts `CONCURRENTLY`
statements from that limit.

An experiment on 2026-10-17 tested whether the plain library keeps the
same guarantees. It ran on Postgres 16, in CI only, and was never merged.

| Experiment | Result |
|---|---|
| E1 | The plain library applies all 178 files, steps back past 0062 and applies them again. Version 180, not dirty; the CONCURRENTLY indexes are valid. |
| E2 | A `lock_timeout` on the connection cuts off a CONCURRENTLY build and leaves an INVALID index (one Postgres keeps but never uses). A re-run with `IF NOT EXISTS` reports success, and the index stays INVALID. `lock_timeout` in the URL and `x-statement-timeout` fail the same way. |
| E2 (a) | No limit on the connection, and `SET LOCAL lock_timeout` at the top of each normal file. The CONCURRENTLY build waits and finishes valid. A normal file stops at the limit and Postgres rolls all of it back, but golang-migrate still marks the version dirty. The `SET LOCAL` ends with the file. |
| E3 | golang-migrate's lock blocks (`pg_advisory_lock`), and opening a library migrator already takes it. A second migrator that waits on it while a CONCURRENTLY build runs creates a deadlock. Postgres detects it (40P01) after `deadlock_timeout` (1 s by default) and cancels one side. When it cancels the build, the index is INVALID and the version is dirty. A lock that polls (`pg_try_advisory_lock`) has no such problem. ADR-073 said Postgres could not detect this deadlock; it can. |
| E5 | goose leaves no trace of a failed file. golang-migrate leaves a dirty version that needs `force`. |
| E7 | golang-migrate's `pgx/v5` driver uses the same `schema_migrations` table and the same lock id as the `postgres` driver. Both derive the id from the database, schema and table names (3828333823 for the test database). Its errors give the line and column. |

E4 and E6 tested the goose alternative below; both passed.

What other Postgres-focused Go projects do (read 2026-10-17):

- **OpenMeter** calls golang-migrate directly, with its `database/postgres`
  driver and no lock or transaction code of its own:
  `return m.filterErrNoChange(m.goMigrate.Up())`. The app waits for a
  separate migration job (`WaitForMigrationJob`) and reports
  `"database is dirty, please run migrations manually"`.
- **Coder** keeps golang-migrate and adds one driver of about 170 lines:
  "pgTxnDriver is a Postgres migration driver that runs all migrations in
  a single transaction", locked with `SELECT pg_advisory_xact_lock($1)`.
  A single transaction cannot run `CONCURRENTLY`, so this shape does not
  fit Velox.
- **HashiCorp Boundary** lists
  `github.com/golang-migrate/migrate/v4 v4.19.1` in its `go.mod`.

## Decision

1. **The library runs migrations.** `migrate.Up` opens the migration
   connection and calls golang-migrate's `Up()`. The hybrid runner is
   deleted with its tests: `upHybrid`, `applyNoTx`, `applyNoTxOnPool`,
   `stepOneViaLibrary`, `execNoTxStatement`, `acquireHybridLoopLock`,
   `LockKeyMigrateHybrid` and `rollbackHybrid`. `splitSQLStatements`
   moves into the lint's test file, its only user.

   Some helpers are shared with code that stays (5). `velox migrate force`
   uses `lookupDBAndSchema`, `generateAdvisoryLockID` and
   `setSchemaVersion`. `DirtyError` uses `postApplyVersion` and the header
   check. They stay only as far as those two still need them.
   `generateAdvisoryLockID` copies the library's exported
   `database.GenerateAdvisoryLockId`, so that function replaces it.
2. **The connection has no lock limit; each file sets its own.** Every
   migration file from 0181 on, up and down, starts with
   `SET LOCAL lock_timeout = '5s';`. The only exception is a CONCURRENTLY
   file. A `-short` test fails a file without the line.
   `MIGRATION_LOCK_TIMEOUT` is removed. When it is set, Velox logs one
   WARN at start-up.
3. **A CONCURRENTLY file holds exactly one statement.** It has no
   `SET LOCAL`, no `IF NOT EXISTS` on the `CREATE`, and nothing else. A
   `-short` test enforces this. Postgres runs a one-statement file outside
   a transaction, so the build has no lock limit and waits for older
   transactions. The header in 0062, 0112 and 0130 stays as an inert
   comment, because shipped files are never edited.
4. **One process applies migrations at a time.** Under Compose, that is
   the single API container with `RUN_MIGRATIONS_ON_BOOT=true`. With
   several copies, the steps are:
   1. Stop the old copies.
   2. Run `velox migrate` once.
   3. Start the new copies with `RUN_MIGRATIONS_ON_BOOT=false`.

   golang-migrate's own lock still serializes any overlap. The rule keeps
   a second migrator from waiting on that lock during an index build (E3).
5. **Failures.** `DirtyError` and `velox migrate force` stay. Postgres
   rolls back the whole file after a lock timeout (SQLSTATE 55P03) in a
   file without `CONCURRENTLY`. Velox then clears the dirty mark with the
   library's `Force`, back to the version before that file. A retry needs
   no repair, as in v0.8.1. Any other failure keeps the dirty mark.

   `velox migrate force` keeps its no-wait check, as in v0.8.1. It tries
   golang-migrate's lock with `pg_try_advisory_lock` and refuses while a
   migration holds it. It does not call the library's `Force` or open a
   library migrator, because both wait on the lock with
   `pg_advisory_lock`. A wait during an index build is the E3 deadlock.
   The version already reads dirty while the build runs, so that is when
   an operator is most likely to reach for `force`.
6. **Driver.** golang-migrate's `database/pgx/v5` driver replaces
   `database/postgres`, so errors give the line and column (E7).

## Why this design

- **It keeps every safety property with the least code Velox owns.**

  | Property | Kept by | Proof |
  |---|---|---|
  | Normal files are all-or-nothing | One query string per file, one implicit transaction | E1, E2 (a) |
  | Index builds never block traffic | One-statement CONCURRENTLY file, no lock limit | E1, E2 (a) |
  | Lock waits have a limit | `SET LOCAL lock_timeout` in each file, checked in CI | E2 (a) |
  | One migrator at a time | The library's advisory lock, plus rule 4 | E7, E3 |
  | Clear errors | `pgx/v5` driver, `DirtyError`, `LockTimeoutError` | E7 |

- **It changes nothing on an install's upgrade path.** The table, the
  rows and the lock id stay the same, and no upgrade stop is needed. If
  the dirty mark becomes a real problem, goose with a baseline is still
  possible later. The reverse would not be: rigor follows reversibility
  (`feedback_design_principles_simple_correct_standard`).
- **It is the common choice** (`feedback_prefer_common_choice_not_one_user`,
  `feedback_prefer_battle_tested_libs`). OpenMeter runs this shape: plain
  `Up()`, one migration job, and a dirty database as a manual step.
- **The one-migrator rule adds no new step.** With several copies, a
  release that has migrations already needs one migration step before
  the new copies start. Old copies must not run on the new schema, and no
  expand/contract rule exists yet.

## Alternatives considered

- **A. goose with a baseline squash.** goose has a per-file
  no-transaction mode and a polling lock built in. It writes the version
  row inside each file's transaction, so a failed file leaves nothing to
  repair (E4, E5). One baseline file would replace the 178 (E6).
  Rejected for now, for four reasons:
  - It changes every install's version table for good
    (`goose_db_version`, with an adopt step).
  - Installs older than the cut-off would need an upgrade stop.
  - The baseline needs three edits to pg_dump's output and one
    hand-kept seed statement (`leader_leases`), and it was proven only as
    a superuser.
  - All it removes beyond this decision is the dirty mark and `force`,
    and those already come with an error that names the commands.
- **B. golang-migrate with a Velox polling lock (A′).** A short
  `pg_try_advisory_lock` loop around `Up()` would keep boot-time migration
  from several copies safe (E3, polling lock). Rejected: rule 4 does the
  same with no code, and the HA guidance needs a separate migration step
  anyway.
- **C. A limit on the connection.** This means `lock_timeout` in the URL,
  or `x-statement-timeout`. Both cut off the CONCURRENTLY build and leave
  an INVALID index with a dirty version, and the re-run hides it (E2).
  `x-statement-timeout` also stops slow files that wait on no lock.
- **D. `x-multi-statement=true`.** It splits files on every `;`, which
  breaks the 16 files with `$$` function bodies. It also runs each
  statement on its own, so the 224 files with more than one statement
  lose all-or-nothing. (Both counts take up and down files together.)

## Consequences

### Positive

- About 500 lines of runner code and about 560 lines of tests are
  deleted.
- No header to remember. Both new file rules are checked in `-short`.
- The version table and the lock stay the same, so nothing changes for
  installs.
- Migration errors give the line and column.
- A lock timeout still leaves nothing to repair, as in v0.8.1.

### Risks / open items

- **Rule 4 is a rule, not code.** If it is broken while a release has a
  CONCURRENTLY file, the build can be cancelled (E3). The index is then
  INVALID, the version is dirty, and every copy refuses to start. This
  failure is loud. The `CREATE` has no `IF NOT EXISTS`, so a re-run after
  `force` fails with "already exists" until the runbook's step drops the
  INVALID index. Trigger to revisit B: an install that needs several
  copies to migrate as they start.
- **Any failure other than a lock timeout still leaves a dirty mark**
  (E5), and it needs `velox migrate force`. Trigger to revisit A: an
  upgrade where the right `force` target was hard to choose, or was
  chosen wrong.
- **`MIGRATION_LOCK_TIMEOUT` is removed one release after it shipped**
  (v0.8.1). An operator who set a longer wait gets 5 seconds per file
  again, and a WARN.
- **Files before 0181 have no lock line.** On a fresh install this is
  harmless, because there is no traffic yet. An install on v0.7.x or older
  that goes straight to this release applies 0178 to 0180 with no lock
  limit, as it did before v0.8.1.
- **E7 checked the `pgx/v5` driver for the table, the lock id and the
  errors only.** Its PR runs E1 and E2 (a) on that driver before merge.
- **A `SET LOCAL` on its own does nothing** (WARNING 25P01). It works here
  because the file has more statements and runs as one transaction. The
  check requires it as the first of at least two statements.
- **The lock-timeout clean-up runs after the library releases its lock.**
  This is safe: a dirty version stops every other `Up()`, so nothing else
  can move the version in between. A real-Postgres test covers it.
- **`DirtyError` reads the header today.** It uses the header to say
  whether the failed file ran outside a transaction. New CONCURRENTLY
  files have no header, so the runner-deletion PR must recognise them by
  rule 3's shape instead.
- **Integration tests stay one migrator at a time**, because CI runs them
  with `go test -p 1`.

## References

- ADR-073: its migration-runner part is replaced, and E3 corrects its
  claim that Postgres cannot detect the advisory-lock deadlock.
- #967 (dirty repair), #987 (lock timeout)
- Migrations 0062, 0112, 0130
- Experiment: [CI run 39047170196](https://github.com/getvelox/velox/actions/runs/39047170196)
- `feedback_prefer_common_choice_not_one_user`,
  `feedback_prefer_battle_tested_libs`, `feedback_no_overengineering`,
  `feedback_design_principles_simple_correct_standard`
- [OpenMeter `tools/migrate/migrate.go`](https://github.com/openmeterio/openmeter/blob/main/tools/migrate/migrate.go)
- [Coder `txnmigrator.go`](https://github.com/coder/coder/blob/main/coderd/database/migrations/txnmigrator.go)
- [Boundary `go.mod`](https://github.com/hashicorp/boundary/blob/main/go.mod)
