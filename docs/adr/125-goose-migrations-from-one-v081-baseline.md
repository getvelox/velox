# ADR-125: Run migrations with goose from one v0.8.1 baseline

**Date:** 2026-10-18
**Status:** Accepted (2026-10-18). The cut-over is built; it ships in v0.9.0.
**Supersedes:** [ADR-124](124-plain-golang-migrate-each-file-sets-its-lock-limit.md), which was never built
**Amends:** [ADR-073](073-selfhost-boot-and-bootstrap-contract.md) (decision 3, the migration runner, is replaced) and [ADR-107](107-unknown-is-terminal-until-a-human.md) (migration 0009's rollback stops shipping)

## Summary

From v0.9.0, Velox applies database migrations with goose
(`github.com/pressly/goose/v3` v3.27.0) instead of golang-migrate and its
own runner. The 178 old migration files become one generated baseline at
version 180, where the old files end. An empty database runs that
baseline. A database from v0.8.0 or v0.8.1 is taken over with no table
change. A failed migration leaves nothing to repair, so the dirty state
and `velox migrate force` go away, and several copies may migrate as they
start. We switch now because Velox has no customer installs yet: the
first partners start on goose and never need an adopt step.

## Context

A migration is a versioned SQL file that changes the database schema.
Velox has 178 of them (0001 to 0180; 0143 and 0166 do not exist), applied
by golang-migrate v4.19.1. golang-migrate keeps one row in
`schema_migrations`: a version and a `dirty` flag. When a file fails, the
flag stays set, and Velox refuses to start until someone runs
`velox migrate force`.

`CREATE INDEX CONCURRENTLY` builds an index without blocking writes, but
it cannot run inside a transaction. Three files use it (0062, 0112, 0130).
For them, Velox wraps the library in its own "hybrid" runner (ADR-073).
With the repair and lock-timeout code added since (#967, #987), the
package holds about 1,535 lines of code and 1,694 lines of tests.

[ADR-124](124-plain-golang-migrate-each-file-sets-its-lock-limit.md)
(2026-10-17) chose to delete the runner and run plain golang-migrate. It
kept the dirty flag and `force`. It also made "one process migrates at a
time" a rule for operators, because golang-migrate's lock can deadlock
against an index build (E3 below). It rejected goose mainly because every
install would have to change its version table, with an adopt step.

Three facts change that weighing:

- **No customer runs Velox yet.** The adopt step costs nothing today, and
  it costs more with every install. If goose ships before the first
  partner installs, partners start on goose and never adopt.
- **golang-migrate has not fixed its lock.**
  [Issue #960](https://github.com/golang-migrate/migrate/issues/960),
  "Postgres deadlocking when multiple processes run
  `CREATE INDEX CONCURRENTLY`", has been open since 2023-07-25. Both fix
  PRs, [#962](https://github.com/golang-migrate/migrate/pull/962)
  (2023-07-26) and [#1451](https://github.com/golang-migrate/migrate/pull/1451)
  (2026-10-03), are open and unmerged (checked 2026-10-18). So ADR-124's
  one-migrator rule would stay a rule with no code behind it.
- **The baseline's open questions have answers.** ADR-124 noted that a
  baseline needs edits to pg_dump's output and was proven only as a
  superuser. Here a script generates it from the published v0.8.1 image,
  and a CI job proves on every run that a new install and an upgraded
  install are identical, under three role setups.

The experiments ran on 2026-10-17 on Postgres 16, in CI only, on a branch
that was never merged
([CI run 39047170196](https://github.com/getvelox/velox/actions/runs/39047170196)).
An INVALID index is one Postgres keeps but never uses.

| Experiment | Result |
|---|---|
| E3 | golang-migrate's lock blocks (`pg_advisory_lock`). A second migrator that waits on it during a `CONCURRENTLY` build creates a deadlock. Postgres cancels one side (40P01). When it cancels the build, the index is INVALID and the version dirty. A lock that polls (`pg_try_advisory_lock`) showed no deadlock. |
| E4, phase 1 | Two goose processes, with an older transaction open. One ran a no-transaction index file, every statement on one connection, with `lock_timeout` going 2s, 0, 0, 2s. The other polled the lock, then found nothing to do. One version row; the index is valid. |
| E4, phase 2 | A normal file stopped at its `lock_timeout` (55P03) after 2 seconds. goose wrote no version row, and a rerun needed no repair. |
| E4, phase 3 | A no-transaction build failed on a duplicate key (23505) and left an INVALID index. It also left `lock_timeout = 0` on its connection, because `RESET` never ran. After the data was fixed, a rerun that drops the index first built it valid. |
| E5 | A failed goose file leaves no table and no version row. The same file under golang-migrate leaves a dirty version that needs `force`. |
| E6 | A baseline made with `pg_dump` from the 178 files, applied by goose, gives the same schema and data as the 178 files, compared both ways. It needed edits to pg_dump's output and the `leader_leases` seed, and ran only as a superuser. |

## Decision

1. **goose runs migrations.** Velox uses goose's `Provider` API at
   v3.27.0, with the global registry off. `migrate.Up(dsn)` keeps its
   signature, so its four callers do not change. These are deleted:
   - golang-migrate, the hybrid runner and the lock-timeout clean-up;
   - `DirtyError` and `velox migrate force` (a stub stays for one release
     and exits with code 2 and a pointer to v0.8.1's runbook);
   - `cmd/velox-migrate-safety`, which steps through the old files;
   - the 178 old files. They stay in git at tag `v0.8.1` and in the
     v0.8.1 image.
2. **One baseline at version 180.**
   `scripts/gen-baseline.sh` writes
   `internal/platform/migrate/baseline/00180_baseline.sql`. It runs the
   published v0.8.1 image, pinned by digest, on an empty Postgres 16. It
   checks the result (version 180, not dirty; only the five
   `leader_leases` rows), then runs `pg_dump --schema-only`. Nobody edits
   the output by hand. The script makes a fixed set of edits, and a guard
   stops it on anything unexpected:
   - It drops psql meta-commands (`\restrict`) and the `Dumped by` lines.
   - It turns each header `SET` into `SET LOCAL`, and makes `search_path`
     transaction-local. goose runs every file of one `Up` on the same
     connection, so a plain `SET` would leak into the next file.
   - It drops `COMMENT ON EXTENSION`. Only an extension's owner may run
     it, and managed Postgres creates the extensions itself.
   - It appends migration 0174's `leader_leases` seed as written, so each
     token comes from `clock_timestamp()` at install time.
   - It appends migration 0150's
     `REVOKE UPDATE, DELETE, TRUNCATE ON public.audit_log FROM velox_app`.
     Where `velox_app` has default privileges (Compose, the managed
     setup), the dump alone would lose that revoke.

   New files start at `00181`, named `NNNNN_name.sql`. Version numbers do
   not change across the switch.
3. **Migration 180 builds or adopts.** It is a goose Go migration, so it
   runs inside goose's lock and transaction, and goose writes row 180 in
   the same transaction:
   - With no `schema_migrations`, it runs the baseline.
   - With a v0.8 record at (180, not dirty), it changes no table. It
     marks `schema_migrations` as frozen with a table comment.
   - With any other record, it refuses.

   goose is the only writer of `public.goose_db_version`. The name is
   schema-qualified, so the baseline's empty `search_path` cannot hide
   it. `schema_migrations` stays, frozen: if it were dropped, a v0.8.x
   binary would replay 0001 on a full database. Migration 180's Down
   refuses and says to restore the backup taken before the upgrade.
4. **A read-only state check runs first.** It runs before every `Up`, in
   the boot check (`CheckSchemaReady`) and in `velox migrate status`. It
   reads both version records, takes no lock and creates no table. It
   refuses three states. Each message states the values it found, ends
   "Nothing was changed.", and names the next step.

   | State found | What the message says |
   |---|---|
   | A v0.8 record that is not a clean 180: older, dirty, or from a development build | Upgrade with v0.8.1 first, or use an empty database |
   | goose has 180, and `schema_migrations` is unmarked or not at (180, not dirty) | The records disagree. A v0.8.x backup was restored over this database, or a v0.8.x binary ran `migrate`, `rollback` or `force`. Follow the runbook |
   | Velox's tables resolve outside the `public` schema | Restore the database so its tables are in `public` |

   The table comment is what tells a frozen record from one that a
   v0.8.x backup or binary brought back.
5. **Locks and connections.**
   - goose's session lock polls `pg_try_advisory_lock` (lock id
     4097083626) every 5 seconds, for up to 30 minutes, the same total
     wait as v0.8.1.
     When the wait runs out, Velox returns a typed error that points at
     `velox migrate status`.
   - Each command opens its own pool of one connection and closes it on
     return. `MIGRATION_LOCK_TIMEOUT` (default 5 seconds) stays and sets
     `lock_timeout` on that connection. Closing the pool matters, because
     a failed index file leaves `lock_timeout = 0` on its session (E4,
     phase 3).
   - The session goose migrates on has `search_path = public`. The
     baseline names every object `public.x`, so a later file's
     `CREATE TABLE x` must land in `public` too, not in a schema the admin
     user's own `search_path` lists first. The state check reads before
     that, with the operator's `search_path`, so it still finds a v0.8
     install outside `public`. The three extensions must be in `public`
     too.
6. **Rules for new files**, checked by `-short` lints:
   - A normal file runs in one goose transaction. It has no transaction
     control and uses only `SET LOCAL`. A multi-line `$$` body goes
     between `-- +goose StatementBegin` and `-- +goose StatementEnd`.
   - An index build is the only file allowed to run without a
     transaction, and it has one fixed shape:

     ```sql
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
     ```

     One index per file, with a new name and no `IF NOT EXISTS`. goose
     writes the version row after a no-transaction file. A crash between
     the build and that row leaves a valid index with no row, so the file
     drops the index first, and every rerun is safe. `IF NOT EXISTS`
     stays banned because it hides an INVALID index (ADR-124, E2).
7. **Several copies.** Copies may migrate as they start
   (`RUN_MIGRATIONS_ON_BOOT=true`). They take turns on the polling lock
   and cannot deadlock against an index build (E3, E4). Old code on a new
   schema is still not covered, so for a release with migrations, the old
   copies stop before the migration runs. From v0.8.x, run `velox migrate`
   with v0.9.0 once. It records 180 and changes no table, so the v0.8.x
   copies keep serving while it runs. Then upgrade the copies one at a
   time.
8. **CLI.**
   - `velox migrate status` shows the plan: the database's version, this
     release's version, the state, the files still to apply, and whether
     a migration is running now. It reuses the output from #1171, and it
     is read-only.
   - `velox migrate rollback` runs only when `APP_ENV` is `local`, one
     step at a time, and never below 180. Anywhere else it refuses: going
     back means restoring the backup taken before the upgrade.
9. **Proof in CI.** A new `migrations` job runs on every PR:
   - A new install of HEAD must equal an upgraded install (the v0.8.1
     image, then HEAD twice). It compares the schema with owners and
     access rights, the data, and the applied versions.
   - It runs under three setups: Compose (with `postgres-init.sh`),
     managed (a migrating role that is not a superuser, extensions made in
     advance) and the CI test database.
   - A restore drill backs up a v0.8.1 database, adopts it, and restores
     the backup over it. The next `velox migrate` must refuse and change
     nothing.
   - HEAD applies on Postgres 14. If it fails, the docs stop calling 14
     the oldest version that should work.
10. **Rollout.** Four PRs, in order:
    1. This ADR.
    2. The cut-over: code, baseline, tests, the `migrations` job and the
       operator docs. No golang-migrate file may merge before it.
    3. The lints, a `make migration` scaffold and `docs/dev/migrations.md`.
       It merges before the first `00181` file: until then a `-short`
       test fails on any file in `sql/`, and PR 3 deletes that test.
    4. Release v0.9.0, after an upgrade drill from v0.8.1 and a cold
       install.

## Why this design

- **It removes what ADR-124 left to people.** A failed file leaves
  nothing to repair (E4 phase 2, E5), so there is no dirty flag and no
  `force` target to choose. goose's polling lock makes boot-time
  migration from several copies safe in code (E3, E4 phase 1), not by an
  operator rule.
- **This is the cheapest time.** ADR-124 kept golang-migrate partly
  because the switch cannot be undone for installs. With no customer
  installs, that one-way cost falls on almost no one. A v0.8.x install
  adopts with no table change. Rigor follows reversibility
  (`feedback_design_principles_simple_correct_standard`), and this step
  is cheapest to take before anyone depends on the old shape.
- **A maintained library does the hard parts**
  (`feedback_prefer_battle_tested_libs`). goose has a per-file
  no-transaction mode and a polling lock, and it writes a normal file's
  version row in that file's transaction. Velox keeps about 350 lines:
  the state check, migration 180, the status output and the error
  messages.
- **One writer and one lock.** Adoption is goose's own migration 180, so
  goose's lock, transaction and version write cover both adoption and the
  baseline. There is no second writer of the version table and no rule
  about what may run first.
- **Version numbers keep their meaning.** A database recorded at 180
  stays at 180. Comments and tests that cite a migration number, such as
  "migration 0150", still mean one thing.
- **The proof runs on every PR, not once.** The baseline's edits cannot
  drift, because the `migrations` job compares a new install with an
  upgraded one each time.
- **Almost no other module moves.** goose v3.27.0 raises one module in
  Velox's Linux build: `prometheus/procfs`, 0.16.1 to 0.19.2, which the
  Prometheus client uses to read process metrics from `/proc` (the first
  count was taken on macOS, where that module is not built; corrected
  2026-10-18 in the cut-over PR). Its provider, lock and Postgres code is
  the same as in v3.29.0.

## Alternatives considered

- **A. Plain golang-migrate (ADR-124).** It deletes the runner, and each
  new file sets its own lock limit. Rejected: it keeps the dirty flag and
  `force`, and the one-migrator rule has no code behind it while
  golang-migrate#960 stays open. The cost of switching later grows with
  every install.
- **B. A baseline at version 1**, as goose projects usually start.
  Rejected: a v0.8 database at 180 and a new one at 1 would describe the
  same schema with different numbers. Every reference to a migration
  number would need care.
- **C. A separate adopt step (a "bridge") before goose runs.** Rejected:
  it is a second writer of `goose_db_version` with its own lock, and it
  needs a rule that no goose call may run before it. Migration 180 does
  the same work inside goose.
- **D. goose v3.29.0, the latest release.** Rejected for now: it raises
  18 modules in Velox's build, including pgx (5.9.2 to 5.11.0) and
  shopspring/decimal (1.4.0 to 1.5.0), the money type. Releases 3.27.1
  to 3.29.0 add dependency upgrades, a `goose create` fix and features
  for other databases; their Postgres code is unchanged. A later bump is
  an ordinary dependency PR.
- **E. Drop `schema_migrations` at adoption.** Rejected: a v0.8.x binary
  would then see no record and replay 0001 on a full database.
- **F. Drop `goose_db_version` in `scripts/restore.sh`.** Rejected: it
  covers only that script, and the drop cannot join `pg_restore`'s single
  transaction. A v0.8.x restore with `--clean` also leaves tables that
  files after 180 created. Only a restore into an empty database is
  always right, and the state check refuses the rest.
- **G. Port `cmd/velox-migrate-safety`.** Rejected: it steps through the
  old files, which no longer ship. A replacement waits for its trigger
  (below).

## Consequences

### Positive

- No dirty state and no `force`. After a failure, fix the cause and run
  the upgrade again.
- Several copies may migrate as they start.
- About 1,535 lines of runner code and 1,694 lines of tests give way to
  about 350 lines of code and new tests.
- A new install runs one file instead of 178.
- CI proves on every PR that a new install equals an upgraded one,
  access rights included, under three role setups.
- An index build can be rerun safely after any crash.

### Risks / open items

- **Every v0.8.x install changes its version table once.** Adoption adds
  `goose_db_version` and a table comment, and changes no other table.
  Installs older than v0.8.0 upgrade to v0.8.1 first.
- **With `RUN_MIGRATIONS_ON_BOOT=false`** (the default outside Compose),
  run `velox migrate` once before starting v0.9.0. Until then v0.9.0
  refuses to start and says one migration is waiting. The release notes
  say so.
- **A v0.8.x backup restores only into an empty database.** Restored over
  an adopted database, the state check refuses and changes nothing.
- **v0.8.x binaries can still write `schema_migrations`** after adoption,
  through `migrate rollback` or `migrate force`. The state check catches
  it on the next v0.9 start. To go back, restore the backup. When no file
  after 180 has run, the v0.8.1 image can also run as it is.
- **Adoption needs to own `schema_migrations`** to add the comment. If
  v0.9 migrates as a different role than v0.8 did, adoption fails with
  "must be owner" and changes nothing.
- **The `velox migrate status` output changes.** Scripts that read the old
  `version: N, dirty: …` line break. None in this repo does.
- **`velox migrate rollback` is local only**, and the old down files no
  longer ship. The ADR-107 amendment covers migration 0009's rollback.
- **The `migrations` job pulls the v0.8.1 image** from GHCR by digest. If
  GHCR is down, the job fails loudly.
- **A role-level `statement_timeout`** on the migrating user cuts off a
  long index build, as it does today.
- **`velox_app` keeps all rights on `goose_db_version`** where default
  privileges grant them, as it has on `schema_migrations` today.
- **Old code on a new schema is still not covered.** Deferred work, each
  with its trigger:
  - An expand/contract rule (old and new code share one schema during an
    upgrade). Trigger: the first partner who needs upgrades without
    downtime across several copies.
  - A tool that times migrations on populated tables. Trigger: the first
    file after 180 that touches `usage_events`, `invoices`,
    `subscriptions`, `customer_credit_ledger` or `audit_log`.
  - A replacement for `velox-migrate-safety`. Trigger: the first file
    after 180 that moves data.
  - At the next baseline: delete migration 180's adopt branch, the table
    comment check and the three refusals, and drop the frozen
    `schema_migrations`.
  - Move to the current goose release with the next by-hand Go
    dependency pass.

## References

- [ADR-124](124-plain-golang-migrate-each-file-sets-its-lock-limit.md),
  superseded by this ADR; its experiments E1, E2 and E7 still stand.
- [ADR-073](073-selfhost-boot-and-bootstrap-contract.md): decision 3, the
  hybrid runner, is replaced (amendment 2026-10-18).
- [ADR-107](107-unknown-is-terminal-until-a-human.md): migration 0009's
  rollback (amendment 2026-10-18).
- #967 (dirty repair), #987 (lock timeout), #1170 (ADR-124), #1171 (the
  status output this design reuses)
- Migrations 0062, 0112 and 0130 (`CONCURRENTLY`), 0150 (the only
  `REVOKE`), 0174 (the `leader_leases` seed), 0180 (the last v0.8.1 file)
- Experiments:
  [CI run 39047170196](https://github.com/getvelox/velox/actions/runs/39047170196)
- golang-migrate
  [#960](https://github.com/golang-migrate/migrate/issues/960),
  [#962](https://github.com/golang-migrate/migrate/pull/962),
  [#1451](https://github.com/golang-migrate/migrate/pull/1451)
- goose v3.27.0 source:
  [`provider_run.go`](https://github.com/pressly/goose/blob/v3.27.0/provider_run.go)
  (the version row in the file's transaction, or after a no-transaction
  file),
  [`lock/postgres.go`](https://github.com/pressly/goose/blob/v3.27.0/lock/postgres.go)
  (the polling lock, id 4097083626)
- `feedback_prefer_battle_tested_libs`,
  `feedback_design_principles_simple_correct_standard`,
  `feedback_no_overengineering`
