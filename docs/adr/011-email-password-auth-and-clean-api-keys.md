# ADR-011: Email/Password Dashboard Auth, Pure-CRUD API Keys

**Date:** 2026-05-01
**Status:** Accepted — amended by ADR-081 (2026-07-06): multi-user invites
shipped (tokenized accept, no RBAC); the "invite flow comes when it's
needed" deferrals below are partially resolved.

## Summary

The dashboard signs in with email and password, and API keys are only for SDK and curl callers, as on every surveyed platform. A dashboard session (`dashboard_sessions`) belongs to a user, not to an API key. This let us delete every safeguard that existed only because the API key was also the dashboard login: the guard against revoking the tenant's last active secret or platform key, the refusal to revoke or rotate the key making the request, and the revocation of dashboard sessions when their key was revoked. API keys now have a plain create, list, revoke and rotate lifecycle. Password reset uses single-use tokens that expire after 1 hour. Amended 2026-07-06 (ADR-081): team invites shipped with tokenized acceptance and no RBAC, so a tenant can have several users. Amended 2026-07-17 (ADR-094): the failed-login counter and lockout were removed, and the per-IP `/v1/auth` rate limiter is the only brute-force limit in v1.

## Status
Accepted

## Date
2026-05-01

## Context

Velox's auth model has zigzagged. ADR-007 reverted from email+password to "API key paste" because the email+password surface had been built ahead of evidence — the silent deviation from `project_auth_decision.md`. ADR-008 then refined the dashboard half by minting an httpOnly cookie derived from the pasted API key (so the credential isn't JS-readable).

The combined effect is that **the API key is the dashboard sign-in credential**, and a series of safeguards exist to prevent the foot-guns that creates:

- Last-active-secret-or-platform safeguard in `auth.Store.Revoke` (prevents tenant lockout)
- `self_revoke` 422 in `handler.go:80` (caller can't revoke the key authenticating the request)
- `self_rotate` 422 in `handler.go:106` (same reason)
- Cookie fan-out in `auth.Service.RevokeKey` (revoking a key invalidates dashboard sessions tied to it)
- `isCurrent` disabled state on the dashboard's Revoke button
- `wouldOrphanTenant` UI logic (currently dead in v1, kept as defense-in-depth for future multi-user accounts)

Each safeguard is correct given the model. None of them would exist if the dashboard had its own auth credential.

By 2026-05-01 we hit a line where adding more API key features (Rotate UI, restricted keys, IP allowlist) requires either expanding the safeguard surface or finally separating dashboard auth from API key auth. The latter is the simpler path forward. It also removes a real DP-credibility friction: "paste your secret API key into the login screen" is not a UX a non-Stripe-trained operator expects.

A 2026-05-01 industry survey across Stripe, GitHub, AWS IAM, Auth0, Datadog, Vercel, Twilio, and Paddle found that **every mature platform separates user auth from API key auth**. Stripe has account login (email+2FA) entirely separate from `sk_live_…` keys. GitHub has user accounts entirely separate from PATs. None use API-key-as-dashboard-login. Velox's current model is the outlier.

## Decision

Add a minimal homegrown email+password auth for the dashboard. Make `dashboard_sessions` user-bound, not key-bound. Drop the entire safeguard subsystem on the API key path. API keys become pure SDK/curl credentials with a clean CRUD lifecycle.

### Why homegrown over WorkOS / Zitadel / Clerk

1. **OSS purity**. Self-hosters picked Velox because it has no SaaS dependencies — `Postgres + binary` is the moat. Forcing them to register for WorkOS would either add friction or maintain two complete auth modes. Homegrown email+password is zero-dependency.
2. **The recipe is well-trodden**. `bcrypt cost 12` + `crypto/rand` + `subtle.ConstantTimeCompare` + Redis-backed login rate-limit is ~600 LOC of standard Go. Not experimental cryptography. Auditing the recipe is faster than auditing a vendor integration.
3. **Email+password is the dashboard standard**. Every operator expects it. API-key paste was a workaround for not-yet-built auth; it isn't a feature.
4. **WorkOS-later is non-breaking**. Add a `users.workos_user_id NULL` column when SAML/SSO becomes a DP requirement. Same `users` table, dual sign-in paths.

### Why this is not a re-revert of ADR-007

ADR-007 reverted email+password because it was built **ahead of evidence**, in violation of `project_auth_decision.md`. The evidence has now arrived: the API-key-as-credential model creates a class of safeguards (last-key, self-revoke, cookie fan-out, etc.) that exist purely to mitigate the model itself. The original reversion was correct given what was known. Re-adding now is correct given what we know.

`project_auth_decision.md` is updated to reflect the new state (memory rule per `feedback_amend_decisions_when_course_changes`).

## What changes

### New tables

```sql
CREATE TABLE users (
    id            TEXT PRIMARY KEY DEFAULT 'vlx_usr_' || encode(gen_random_bytes(12), 'hex'),
    email         CITEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_login_at TIMESTAMPTZ,
    locked_until  TIMESTAMPTZ
);

CREATE TABLE user_tenants (
    user_id   TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    role      TEXT NOT NULL DEFAULT 'owner' CHECK (role IN ('owner')),
    PRIMARY KEY (user_id, tenant_id)
);

CREATE TABLE password_reset_tokens (
    id         TEXT PRIMARY KEY DEFAULT 'vlx_prt_' || encode(gen_random_bytes(12), 'hex'),
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash TEXT NOT NULL UNIQUE,
    expires_at TIMESTAMPTZ NOT NULL,
    used_at    TIMESTAMPTZ
);
```

Notes:
- `email` uses `CITEXT` so login is case-insensitive without an awkward LOWER() index.
- `user_tenants.role` is currently single-value (`owner`) but the column shape supports growth (member, viewer, etc.).
- `password_reset_tokens.token_hash` stores SHA-256 of the random token; the plaintext only exists in the email link.
- `locked_until` is the failed-login lockout timestamp.

### Modified table

```sql
ALTER TABLE dashboard_sessions
  DROP COLUMN key_id,
  ADD COLUMN user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE;
```

Pre-launch with one operator and one tenant: drop existing rows, no backfill. Operator re-bootstraps and signs in via email+password.

### New endpoints

| Endpoint | Verb | Purpose |
|---|---|---|
| `/v1/auth/login` | POST | email + password → mint session cookie |
| `/v1/auth/logout` | POST | clear cookie + revoke session |
| `/v1/auth/password-reset/request` | POST | email → send reset link if user exists |
| `/v1/auth/password-reset/confirm` | POST | token + new password → set, invalidate token |

### Removed endpoint

`POST /v1/auth/exchange` — no consumer. Dashboard no longer mints sessions from API keys.

### Auth resolution after the change

**Cookie path** (dashboard):
```
velox_session cookie
  → SHA256(value) → dashboard_sessions row (revoked_at IS NULL, expires_at > NOW())
  → users row → user_tenants row
  → ctx: { tenant_id, user_id, role:'owner', permissions: <full> }
```

**Bearer path** (SDK/curl):
```
Authorization: Bearer vlx_secret_…
  → prefix lookup → api_keys row (revoked_at IS NULL, expires_at IS NULL OR > NOW())
  → ctx: { tenant_id, key_id, key_type, permissions: <by tier> }
```

The two paths are now structurally independent. Permission middleware doesn't care which produced `ctx.permissions`.

### Bootstrap shift

`make bootstrap` today produces a tenant + 3 API keys (platform, secret_test, secret_live). After this ADR:

- Still produces a tenant + the 3 API keys (for SDK callers — unchanged contract)
- Additionally produces a user (email + password) bound to the tenant via `user_tenants`
- Email source: `--email` flag or `VELOX_BOOTSTRAP_EMAIL` env. Password source: `--password` flag or random-generated and printed once
- The console output explains: "Email + password for dashboard login. API keys for SDK/curl callers."

### Permission map

`internal/auth/permission.go` already has a `KeyTypeSession` entry that's currently dormant. After this ADR, cookie-resolved sessions populate `ctx` with `KeyType: KeyTypeSession`, and the permission middleware looks up the existing entry — no structural rewrite. Roles (`owner`, future `member`/`viewer`) get layered on top later when a DP asks; for v1 every cookie session gets full permissions, mirroring the bootstrap-equivalent operator role.

### Login rate-limit + lockout

> **SUPERSEDED by ADR-094 (2026-07-17).** The account-lockout described in this
> section was removed: it was weaponizable (any known email → an on-demand
> lockout) and fragmenting. The interim throttle that briefly replaced it was also
> removed, and the `users.locked_until` column was dropped (migration 0154). **v1
> now has no login lockout or throttle** — the per-IP `/v1/auth` rate limiter is
> the brute-force floor; a non-weaponizable throttle + MFA + breached-password
> check are deferred as one unit (ADR-094). The paragraph below is historical.

Already-existing Redis rate limiter gets a new key shape: `auth:login:<email_lowercased>`. Rule: 5 attempts per sliding 1-minute window. After 5 fails: `locked_until = now() + 15min` is set on the user row, login endpoint returns 429 with retry-after until cleared.

### Password recipe (boring + correct)

- `golang.org/x/crypto/bcrypt` cost 12 for hashing
- `subtle.ConstantTimeCompare` for token comparison (timing-attack mitigation)
- Reset tokens: 32 random bytes from `crypto/rand`, base64url-encoded for the email link, SHA-256 hashed in DB
- Token expiry: 1 hour from issue
- Token single-use: `used_at` flipped on first redeem, second use is rejected

### Password complexity

- Minimum 12 characters
- No maximum (avoid the bcrypt 72-byte cliff by pre-hashing if needed; bcrypt accepts up to 72 bytes which covers 12-char passwords by orders of magnitude)
- No required-character classes (NIST SP 800-63B says these reduce security)
- Reject the most-common-1000 password list (built into the validator)

### Email infrastructure for password reset

Velox's existing `internal/email` package handles tenant-configured SMTP for invoice emails. Reuse for password reset emails — new template, same dispatcher.

Edge case: if SMTP isn't configured, password reset emails can't be sent. Mitigation: a `make reset-password --email=foo@bar` CLI subcommand that bypasses email and prints a one-time reset link to stdout. Operators in the no-SMTP-yet state always have a recovery path.

## What gets deleted from the API key subsystem

The whole point of this ADR is the simplification on the API key side. Concretely:

| File | Surface | Lines (approx) |
|---|---|---|
| `internal/auth/postgres.go` | `Revoke`'s lock+count safeguard | ~50 |
| `internal/auth/service.go` | `SessionRevoker` interface, `SetSessionRevoker`, `fanOutSessionRevoke` | ~30 |
| `internal/auth/handler.go` | `self_revoke` and `self_rotate` 422s | ~10 |
| `internal/api/router.go` | `authSvc.SetSessionRevoker(sessionSvc)` wire | ~5 |
| `internal/session/store.go` | `RevokeAllForKey` method | ~15 |
| `internal/session/postgres.go` | `key_id` reads + writes | ~20 |
| `web-v2/src/pages/ApiKeys.tsx` | `isCurrent` detection, `wouldOrphanTenant`, `disabledReason` ternary, conditional Tooltip wrapping, `isRevokingSelf` state | ~100 |
| `web-v2/src/lib/auth.ts` | `authApi.exchange(apiKey)` | ~20 |
| `web-v2/src/contexts/AuthContext.tsx` | `login(apiKey)` flow | ~30 |
| `internal/auth/service_test.go` | `TestRevokeKey_Safeguard`, `TestRevokeKey_FanOut`, supporting helpers | ~150 |
| `internal/api/router.go` | `POST /v1/auth/exchange` route | ~3 |

Net: **~430 LOC removed** from the API key subsystem.

The remaining API key surface is a clean CRUD: create, list, revoke, rotate. No cross-cutting safeguards. The only API key endpoint that needs operator confirmation in the dashboard is the create-result-shown-once flow (already handled correctly).

## What stays

- API key tiers (platform / secret / publishable) and modes (test / live) — unchanged
- `expires_at`, `revoked_at`, `last_used_at` audit fields — unchanged
- Permission map by tier — unchanged
- Rotate-with-grace (max 7 days) — unchanged, but now without the self-rotate caveat
- Audit log on create/revoke/rotate — unchanged
- HMAC-SHA256 webhook signing — unrelated subsystem, untouched

## Phasing

Hard cutover, not dual-mode. Pre-launch with 1 user, dual-mode is unjustified carrying cost.

| Phase | Effort | Outcome |
|---|---|---|
| 1 — Schema + endpoints + bootstrap | ~1 day | Migrations applied, /v1/auth/login working via curl, bootstrap produces a user |
| 2 — Frontend cutover | ~1 day | /login is email+password; /forgot-password and /reset-password ship; /v1/auth/exchange dropped; ApiKeys page simplifies |
| 3 — API key safeguard cleanup | ~half day | Delete all safeguards listed above; Rotate UI ships (now trivial) |
| 4 — Docs + memory updates | ~half day | MANUAL_TEST FLOW A rewritten, FLOW K3/K4 simplified, CLAUDE.md updated, project_auth_decision memory updated |

Each phase ships as one commit. The branch stays mergeable at each phase boundary.

## Migration story for self-hosters

Pre-launch reality: 1 operator, no production data. The `make bootstrap` re-run is the migration. Existing API keys remain valid for SDK use; operator just signs into the dashboard with email+password instead of pasting a key.

Post-launch (after first DP onboards but before this ADR ships): we'd ship a migration that auto-creates a user from the existing tenant's bootstrap email and prints a CLI password-reset link. Out of scope for v1 since no production tenants exist.

## Out of scope (deferred)

Per `feedback_pre_launch_scoping`:

- **2FA / TOTP** — defer until a DP requires. Adding later is non-breaking (`users.totp_secret NULL` column).
- **WorkOS / SSO** — defer. Non-breaking add (`users.workos_user_id NULL`).
- **Email verification on signup** — no public signup in v1; bootstrap creates the first user; multi-user invite flow comes when it's needed.
- **Password complexity policies per tenant** — single hard-coded recipe in v1.
- **Concurrent session limits** — multiple devices stay allowed (current behavior).
- **Account recovery beyond password reset** — if email + reset CLI both fail, manual psql intervention is acceptable for self-hosters.
- **Multi-tenant per user UI** — `user_tenants` join table supports it, but UI assumes 1:1 in v1.

## Consequences

### Positive
- API key subsystem becomes a clean CRUD with no cross-cutting safeguards. Adding new key features (Rotate UI, restricted keys, IP allowlist) doesn't accumulate guard surface area.
- Dashboard auth UX matches industry standard. DP onboarding doesn't waste a beat on "what's an API key?"
- ~430 LOC of safeguard logic deleted; ~1,200 LOC of standard auth added; **net new ~770 LOC** in exchange for a much cleaner mental model.
- WorkOS path stays open (non-breaking column add).

### Negative
- Re-introduces the user/sessions/password_hash/password_reset_tokens surface that ADR-007 removed. The duplication of effort feels bad.
- Email+password is one more thing to maintain (login rate limit, password reset email deliverability, lockout policy). The recipe is boring but it's still code.
- Bootstrap UX is one extra step (email+password) compared to "paste this key."
- Operators with existing dashboards bookmark `/login` expecting the API key paste; they'll see a different form. Pre-launch: nobody's bookmarked it. Post-launch: needs a release note.

### Open follow-ups
- ADR-012 (when SAML/SSO becomes real): WorkOS integration, opt-in via env var.
- Multi-user account model: invite flow, role differentiation beyond `owner`, audit log entries for "user X added user Y to tenant T."
- Per-key restricted scopes (Stripe RAK pattern): the v2 item from the API key research; not blocked by this ADR but easier to reason about once the safeguard surface shrinks.

## Amendment 2026-06-01: failed-login counter degrades, it never silently disables (velox-ops #21)

> **SUPERSEDED by ADR-094 (2026-07-17; note added 2026-07-19).** The always-on
> failed-login counter this amendment describes was removed together with the
> lockout it fed: `user.FallbackFailureCounter`, `SetFailureCounter`, and
> `RecordFailedAttempt` no longer exist in the codebase, and `users.locked_until`
> was dropped (migration 0154). **v1 has no failed-login counter, lockout, or
> per-account throttle** — the per-IP `/v1/auth` rate limiter is the brute-force
> floor; the replacement design (non-weaponizable throttle + MFA +
> breached-password check) is deferred as one unit in ADR-094. The amendment
> below is historical.

**Context.** The "Login rate-limit + lockout" section above described the failed-login counter as Redis-backed. The implementation wired that counter **only when `REDIS_URL` was set** and the service no-op'd otherwise (`RecordFailedAttempt` returned early when no counter was configured). Consequence: in any deployment without Redis — and during a Redis outage in deployments that had it — the 5-strikes lockout never fired and dashboard login was unthrottled against online password guessing. The original design treated the login counter like the general HTTP rate limiter, which is fail-open in non-prod by design. That conflation is wrong: a request-volume limiter is an availability guard; a failed-login throttle is an **authentication control**, and OWASP ASVS requires auth controls to degrade rather than disappear.

**Decision.** The failed-login counter is now **always-on**, via `user.FallbackFailureCounter`:

- Serves from the shared Redis counter (`RedisFailureCounter`, `INCR` + `EXPIRE NX`) when healthy.
- Transparently degrades to a **process-local in-memory counter** (`memFailureCounter`) when Redis is unconfigured (`REDIS_URL` unset → `rdb == nil`) or erroring. The in-memory window seeds its TTL only on the 0→1 transition (mirrors `EXPIRE NX`, so a fast attacker can't push the window out), lazily prunes expired entries each increment (bounds the map under distinct-email credential stuffing), and uses the same lower-cased/trimmed email key.
- A **circuit breaker** trips to local-only after 3 consecutive Redis errors and half-opens after 30s, so a Redis blip doesn't hammer a dead backend on every login, and recovery is automatic. A single WARN logs per trip.
- Wired **unconditionally** at startup (`SetFailureCounter(NewFallbackFailureCounter(rdb, clk))` in `internal/api/router.go`), replacing the `if rdb != nil` guard.

**Explicitly rejected: fail-closed.** "Redis down → block every login" trades a brute-force gap for a self-inflicted total-auth outage. Degrade-to-local keeps the throttle enforced without that blast radius.

**Source of truth unchanged.** The lock decision (`users.locked_until` in Postgres) was always authoritative and is untouched by Redis/breaker state — an already-locked account never silently unlocks during an outage; the counter only decides *when* to write the lock.

**Known, accepted limit.** Behind N app instances during a Redis outage, the effective global threshold is ~`5 × N` (each instance counts independently). Bounded and acceptable for the threat (online guessing against bcrypt-cost-12 hashes), and vastly better than the prior unbounded gap. If a deployment needs a hard global limit during Redis outages, that's a future toggle, not a v1 requirement.

**Out of scope (unchanged from this ADR's posture):** general/IP HTTP rate limiter keeps fail-open-in-nonprod; no operator-configurable lockout policy; no exponential backoff / CAPTCHA; no per-IP `/v1/auth` throttle.
