# ADR-073: Self-host boot contract — one bootstrap writer, honored APP_DATABASE_URL, serialized hybrid migrations

**Date:** 2026-07-03
**Status:** Accepted (shipped in P7; design amended by a 2-lens adversarial panel — both SHIP-WITH-FIXES; amended 2026-08-30, 2026-10-17 and 2026-10-18). Decision 3, the migration runner, is replaced by [ADR-125](125-goose-migrations-from-one-v081-baseline.md) (goose, from v0.9.0), which superseded ADR-124.

## Summary

One function, `tenant.RunBootstrap`, creates a tenant, its keys and its
owner user in one transaction; the CLI and `POST /v1/bootstrap` both call
it. The app's database pool uses `APP_DATABASE_URL` as given. Outside
local development, Velox refuses to start when it is unset, when its
password is empty, `velox_app` or the user name, or when it connects as
a role that can bypass Row-Level Security.
Production also refuses to start without a reachable Redis (amendment
2026-08-30).

Decision 3 is corrected (amendment 2026-10-17). It says Postgres cannot
detect the wait between a migrator blocked on the migration lock and a
`CREATE INDEX CONCURRENTLY`. It can: it reports `deadlock detected`
(SQLSTATE 40P01) after about one second and cancels one side. When it
cancels the index build, the index is left INVALID and the version
dirty. The polling lock in decision 3 avoided that wait. ADR-125
replaced the runner with goose, whose own lock also polls (amendment
2026-10-18).

## Context

Three self-host roots from the 2026-07-02 audit:

1. **Two bootstrap writers had already drifted.** The CLI
   (`cmd/velox-bootstrap`) creates tenant + settings + test/live keys +
   owner user; the HTTP `POST /v1/bootstrap` created tenant + two
   unmarked test keys — no owner user (dashboard login impossible), no
   live key, no settings seed. An HTTP-bootstrapped install was a
   dead end. Docs referenced `VELOX_OWNER_*` env vars nothing reads.
2. **APP_DATABASE_URL was documented but never read.** The runtime RLS
   pool was derived by swapping credentials to the hardcoded
   `velox_app:velox_app`; the production compose init script created
   the role with exactly that public password. Anyone with TCP reach to
   Postgres had cross-tenant read/write.
3. **The hybrid migration runner raced.** `upHybrid` decided what to
   apply from an UNLOCKED read of `schema_migrations`; two replicas
   booting together could double-apply a no-tx migration, rewind the
   version, or (the panel's blocker) mis-dispatch a `CONCURRENTLY`
   file through the library's in-tx path — a deployment-wide dirty
   crash-loop.

## Decisions

1. **One bootstrap writer.** `tenant.RunBootstrap(ctx, db, deps, opts)`
   is the only code path that provisions a tenant; the CLI and the HTTP
   handler are thin callers. It runs **everything in ONE bypass tx** —
   `pg_advisory_xact_lock(LockKeyBootstrap)`, the exists/uniqueness
   guards re-checked under the lock, tenant insert, `tenant_settings`
   seed, three keys (`vlx_secret_test_`, `vlx_secret_live_`,
   `vlx_pub_test_`; livemode pinned per insert against the 0021
   trigger), owner user + tenant attach via an injected tx-scoped
   `deps.CreateUserTx` (tenant must not import its peer `user`).
   All inputs validate BEFORE the first write (email shape; password
   hashed up front — `user.HashPassword` enforces
   `MinPasswordLength = 12`): a malformed request can no longer commit
   a half-bootstrap that 409s forever after (the old orphan window the
   CLI comment admitted is deleted, not documented).
   Guard order on the HTTP handler: **already-bootstrapped 409 →
   token-unset 403 → constant-time compare 403 → run**. A bootstrapped
   install answers a uniform 409 regardless of token validity or
   presence — the perpetual token-validity oracle and the
   token-configured oracle are both gone; the disclosure of
   virgin-install state is accepted (rate-limited since P12, and the
   409 pre-check is one cheap SELECT). Owner email/password are
   optional request fields (defaults: `admin@velox.local`, generated
   96-bit password) returned once with `Cache-Control: no-store`.
   The bootstrap token must never appear in a query string (body or
   Authorization header only — keeps it out of proxy access logs).
2. **APP_DATABASE_URL honored verbatim; production fails closed.**
   - Set → used as-is for the app pool. Unopenable = fatal in EVERY
     env (explicit config never silently degrades).
   - Set, non-local, and its password is empty (trust-auth shape),
     `velox_app`, or equal to its username → fatal.
   - Unset, non-local → fatal ("set APP_DATABASE_URL…"). Unset, local →
     today's `velox_app:velox_app` derivation, warn-and-fallback
     unchanged.
   - **Runtime-role capability check** (the actual closure of the
     finding — both panel lenses ruled it in scope): after opening the
     app pool, `SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE
     rolname = current_user`; true = fatal non-local, warn local. This
     catches the post-fix path of least resistance — copying
     DATABASE_URL into APP_DATABASE_URL — which the string checks
     cannot see. Table-ownership is NOT checked: every RLS table uses
     `FORCE ROW LEVEL SECURITY`, so owners are subject; only
     superuser/BYPASSRLS capability defeats it.
   - `VELOX_BOOTSTRAP_TOKEN`, when set in non-local, must be ≥ 16
     chars (config.validateFatal) — it mints the owner login and the
     live key, so a guessable token is deployment takeover.
3. **The whole hybrid migration loop is serialized**, not just
   `applyNoTx`. `upHybrid`/`rollbackHybrid` take a Velox-owned
   session-scoped advisory lock (`LockKeyMigrateHybrid` — deliberately
   NOT golang-migrate's derived id, which would deadlock the library's
   own `Lock()`) on a dedicated conn for the loop's whole duration.
   Under that lock the loop's per-iteration version+dirty reads are
   authoritative: the loser replica skips already-applied versions
   instead of double-applying, rewinding, or mis-dispatching a no-tx
   file through the library's in-tx path. `applyNoTx` keeps its
   internal golang-migrate-id lock (cross-path guard against a
   concurrent pure-library runner mid-step).
   **Acquisition POLLS `pg_try_advisory_lock`** (500ms interval, 30-min
   loud-fail cap) rather than blocking on `pg_advisory_lock`: the
   racing-appliers test caught that a blocked lock statement holds a
   snapshot for as long as it waits, and the winner's `CREATE INDEX
   CONCURRENTLY` must wait out every older snapshot — a mutual wait
   Postgres cannot detect (advisory waits aren't in its deadlock
   graph). Two replicas booting together would have deadlocked the
   deploy. Try-lock statements hold nothing between polls.
4. **The compose stack ships what its own topology requires**
   (panel: these are wiring, not doc riders):
   - a `redis` service + `REDIS_URL` (APP_ENV defaults to production
     there, which fail-closes the rate limiter — without Redis the
     bootstrap/login walkthrough 429s and invites the `APP_ENV=local`
     workaround that silently reopens the RLS fallback);
   - `VELOX_APP_DB_PASSWORD` passed to BOTH services (init script runs
     in the postgres container), consumed via `psql -v pw=… PASSWORD
     :'pw'` (correct literal quoting), with `openssl rand -hex 24`
     guidance so the DSN stays URL-safe by construction; existing
     pgdata volumes upgrade via the same `ALTER ROLE … PASSWORD :'pw'`
     recipe (doc note only — pre-launch, no migration tooling);
   - `TRUST_PROXY` defaulting to the compose network's private range
     (nginx fronts the API; without it every client shares one
     rate-limit bucket keyed on the nginx container IP and audit logs
     record the proxy);
   - `SMTP_TLS`, `LOG_LEVEL`, `DASHBOARD_BASE_URL`,
     `VELOX_EMAIL_BIDX_KEY` passthroughs; fabricated
     `VELOX_DASHBOARD_*` and phantom `STRIPE_WEBHOOK_SECRET` (nothing
     reads it — Stripe webhook secrets are per-tenant in the DB)
     removed.
5. **Docs route operators to the real path.** `docs/self-host.md`
   labels the `make dev` flow as local development and points
   production self-hosters at `deploy/compose/` as the canonical
   walkthrough; bootstrap credentials are shown being retrieved ON the
   VM (`http://localhost/v1/bootstrap`) with an explicit
   don't-send-over-plain-HTTP note (compose's nginx is :80; TLS stays
   out of v1 scope).

## Accepted residuals

- HTTP bootstrap key prefixes change from unmarked `vlx_secret_` to
  mode-marked (`vlx_secret_test_`/`vlx_secret_live_`) with no compat
  shim — pre-launch; the unmarked form parsed as LIVE, which is itself
  a reason it dies.
- `VELOX_APP_DB_PASSWORD` is visible in `docker inspect` alongside its
  sibling secrets — moving one var to docker secrets while
  `POSTGRES_PASSWORD`/`VELOX_ENCRYPTION_KEY` stay in env would be
  theater on a single VM.
- The `app.bypass_rls` GUC remains reachable by any role holding valid
  credentials — RLS defends against app bugs, not hostile SQL clients;
  the password perimeter above is the load-bearing fix.
- Two pure-library replicas racing (no pending no-tx files) rely on
  golang-migrate's own lock, as before.

## Test locks (real Postgres, mutation-verified)

Bootstrap: two concurrent POSTs → exactly one 201/one 409/one tenant;
invalid password → 422 with ZERO rows written, retry succeeds, login
works; HTTP result has owner user + live key + settings row (parity by
construction). Migrate: loser-skips (schema_migrations pre-advanced →
hybrid runner applies nothing, no dirty, no rewind); two racing
`migrate.Up` from before the last no-tx version both return nil with
correct final version. Boot: fail-closed matrix unit tests
(unset/default/empty-password/username-equal × env) + capability-check
fatal on a BYPASSRLS role.

## Amendment 2026-08-30 — Redis joins the production boot contract

Under the N ≥ 2 production posture a replica that boots without Redis is
not degraded, it is wrong: the general and hosted-invoice rate limiters
fail CLOSED without a client (nil or unreachable ⇒ 429 for every covered
request, including hosted-invoice pay pages), `/health/ready` checks only
api/database/scheduler, and the limiter exempts `/health*` — so the load
balancer keeps routing 1/N of traffic to a process that answers 429 to
all of it. The boot contract now has two Redis guards, one per failure
class: `config.validateFatal` rejects an UNSET `REDIS_URL` in production
(beside the encryption-key fatal); `cmd/velox/main.go` opens and pings
Redis (`mw.OpenRedis`, 5 s bound) after the topology check and refuses
to start in production on an invalid or unreachable one. Other
environments warn and run fail-open. The config warning that said the
opposite ("rate limiting will fail open") is corrected. `APP_ENV` is
case-normalised so `Production` cannot slip past either fatal.

## Amendment 2026-10-17 — Postgres detects the migration deadlock; ADR-124 replaces the runner

Decision 3 says a migrator blocked in `pg_advisory_lock` and another
migrator's `CREATE INDEX CONCURRENTLY` wait on each other in a way
"Postgres cannot detect (advisory waits aren't in its deadlock graph)".
That is wrong. An experiment on 2026-10-17 (E3, Postgres 16,
[CI run 39047170196](https://github.com/getvelox/velox/actions/runs/39047170196))
showed what happens:

- The index build waits on the blocked call's transaction (a `virtualxid`
  lock). The blocked call waits on the advisory lock. Both are ordinary
  lock waits, so Postgres's deadlock check sees the cycle.
- After `deadlock_timeout` (1 s by default), Postgres reports
  `deadlock detected` (SQLSTATE 40P01) and cancels one of the two
  statements.
- Either side can be cancelled. When the blocked lock call was cancelled,
  the build finished and the index was valid. When the build was
  cancelled, the index was left INVALID and `schema_migrations` dirty.
- A lock that polls (`pg_try_advisory_lock`) holds nothing between
  tries, and E3 showed no deadlock with it.

So the hazard decision 3 guards against is real, but the result is a
cancelled statement, not a deploy that hangs forever. The polling lock
in decision 3 was the right fix for it.

[ADR-124](124-plain-golang-migrate-each-file-sets-its-lock-limit.md)
replaces the runner from decision 3. Velox will run golang-migrate as it
is. Each new migration file sets its own lock limit, and one process
applies migrations at a time, so no second migrator waits on the lock
during an index build. Until the PR that deletes the runner merges,
decision 3 describes the code.

## Amendment 2026-10-18 — ADR-125 replaces the runner with goose

ADR-124 was superseded before it was built.
[ADR-125](125-goose-migrations-from-one-v081-baseline.md) replaces
decision 3 instead. From v0.9.0, goose applies migrations, starting from
one baseline at version 180.

goose's own session lock polls `pg_try_advisory_lock`, as decision 3's
lock does. So copies that start together still take turns, and a waiting
copy cannot deadlock against a `CREATE INDEX CONCURRENTLY`. The library
now keeps that guarantee, and Velox no longer needs the
one-migrator rule that ADR-124 planned. The 2026-10-17 correction of the
deadlock claim still stands. Decision 3 describes the code up to v0.8.1;
from v0.9.0 the runner and its lock key (76540007) are gone.

