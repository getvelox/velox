# ADR-094: Login brute-force protection — Postgres-authoritative, non-weaponizable, extensible

Date: 2026-07-17
Status: Accepted as the DESIGN OF RECORD; implementation DEFERRED. An interim
first-cut throttle shipped (PR #497) and was then removed to keep v1 lean — see
"What actually ships" below. The old weaponizable lockout stays removed, and the
dormant `users.locked_until` manual backstop was removed too — v1 has no login
lockout or throttle of any kind. Login brute-force protection (the throttle + MFA
+ breached-password check) is deferred as one unit.
**REFINED BY ADR-095** (2026-07-17): an adversarial design panel moved MFA +
breached-password from "deferred" to the next build phase, reclassified the
consumer-scale controls below (graduated ladder, HLL/EWMA aggregate detection,
CAPTCHA) as **never** for this operator-only shape, and simplified the throttle to
plain `(IP × account)` (prefix bucketing → conditional). See ADR-095 for the whole
login-security posture and the concrete MFA design; the brute-force-throttle
reasoning below still stands.
Relates: ADR-011 (homegrown email+password auth — the login this protects), ADR-014 (SSO stays homegrown/embedded, no SaaS auth vendor — same self-host constraint), ADR-093 (CSRF gate — sibling pre-auth hardening)

## Summary

This ADR records the target design for protecting the dashboard login against password guessing. v1 ships none of it, so the per-IP `/v1/auth` rate limiter is the only brute-force control today. The old lockout was removed because it split its count across Redis and memory, let anyone who knew an operator's email lock them out, and stored plaintext emails in Redis; an interim Postgres throttle (PR #497) and the `users.locked_until` column were removed too. In the target design the failed-login count lives in Postgres, blocks apply only to an IP and account together, and a bare account is never locked. A counter only catches guessing, so MFA and a breached-password check are what stop credential stuffing; ADR-095 now governs when each ships, simplifies the key to `(IP × account)`, and drops the graduated ladder, aggregate detection and CAPTCHA. Bcrypt overload from login floods is a separate problem, best fixed at the edge or reverse proxy.

## Context

The dashboard operator login (email+password → httpOnly cookie, ADR-011) shipped
with a brute-force lockout: a per-email failed-login counter in Redis, an
in-process fallback behind a circuit breaker, and a hard `users.locked_until`
lock after 5 misses (velox-ops #21). Walking it end-to-end surfaced three
problems worth fixing at the root rather than patching:

1. **It fragments.** The count lives in *two* stores (Redis + the in-process
   fallback) that never merge. During a Redis flap, some attempts land in Redis
   and some in memory, loosening the effective threshold to ~2× — the exact
   distributed-counter footgun the two-store design invites.
2. **It is weaponizable.** A hard lock keyed on a *bare account* means anyone who
   knows an operator's email can lock them out on demand (5 wrong guesses → 15
   minutes of denial). This is the on-demand DoS lever OWASP explicitly cautions
   against; account lockout is a known anti-pattern.
3. **It leaks PII.** The Redis key was `velox:login_fail:<PLAINTEXT-EMAIL>` —
   operator emails in a store visible via `KEYS`/`SCAN` and RDB/AOF snapshots,
   the same class the audit #485 arc spent effort purging.

A design panel (5 independent architectures, each adversarially red-teamed, then
scored and synthesized) produced the target design below. This ADR records that
target in full so it is predetermined. An interim first cut was built (PR #497)
and then removed (see "What actually ships"): at zero customers, with no named
attacker pressure, a partial throttle whose real value only arrives alongside the
deferred MFA + breached-password controls was premature complexity to carry.
Login brute-force protection is deferred as **one unit**, to be built to this
design rather than grown from a stub.

## The target design (documented now, built incrementally)

The load-bearing insight from the panel, which every proposal reached
independently: **a failed-attempt counter only catches attackers who *guess*.**
Credential stuffing and password spray often submit the *correct* (breached)
password and succeed on the first try — no counter ever ticks. So the throttle is
the cheap outer supplement; the controls that actually stop the dominant threats
are **mandatory MFA/step-up** and a **login-time breached-password check**
(HIBP k-anonymity, or a shipped local Pwned-Passwords filter for air-gapped
operators — consistent with ADR-014's self-host constraint).

Architecture (defense in depth, from cheap/outer to real/inner):

- **Postgres is the single durable AUTHORITY** for both the counter-floor and the
  escalation decision. Login already hard-depends on Postgres (it reads
  `password_hash`), so making the throttle authoritative here adds **no new
  SPOF** and there is no second store to diverge from — the fragmentation class
  is gone by construction.
- **Optional accelerators strictly ABOVE the authority**, governed by two
  invariants so every degradation fails *safe*: (I1) accelerators write through
  to the Postgres floor and, on degradation, *seed from* it rather than starting
  a fresh parallel count; (I2) **ratchet-monotonicity** — an accelerator may only
  make a decision *more* restrictive; only a full authenticated success relaxes
  it. Under these, an asymmetric partition can at most *over*-restrict slightly,
  never the unbounded *loosening* of today's dual-count.
- **The counter is a sensor, not a gate.** It feeds a **graduated ladder**
  (tarpit → CAPTCHA/PoW → post-password step-up/MFA), never a hard account lock.
  The only hard block attaches to the **attacker dimension** — `(IP-prefix ×
  account)` — so a party who knows only an email can, at worst, force the owner
  through one self-clearing challenge they always pass. There is deliberately no
  bare-account lock: a compromised operator account is handled out-of-band by
  password reset (which revokes sessions) / disable, not by refusing its login.
- **Multi-dimension keying:** per-`(IP-prefix × account)` (the hard-block dim),
  per-account and per-IP-prefix sensors, a sharded global rollup, and an HLL
  fan-in (distinct IP-prefixes per account) — the last two **alert-only**, never
  fed back into a per-request decision (that would be a global-friction DoS
  lever). IPs are keyed on prefixes (IPv4 /24, IPv6 /64); a bare IPv6 address is
  2⁶⁴ fresh sources. Every subject is `HMAC(pepper, …)`, never plaintext.
- **Fail direction, per component:** Postgres shares fate with auth (down = login
  down anyway, correct). Redis accelerator: fail-*open* for latency, fail-*closed*
  for security (fall through to the Postgres floor; never drop the check, never
  wholesale-block on a blip; emit a degraded metric — loud, not silent).
- **Self-host tiers, one code path:** full (Redis + N instances), single
  instance, and multi-instance-no-Redis all work because the authority is always
  Postgres; aggregate-only dimensions degrade to a boot-WARN + a metric, never a
  silent no-op.

The panel's rejected alternatives (kept so the reasoning survives): a blocking
`pg_advisory_xact_lock` on the write path (connection-pool self-DoS); an exact
sliding-window-*log* COUNT (quadratic under the flood it measures); Redis as the
authority or a hard multi-instance dependency (wrong shape for a self-hostable
product); a single multi-key `EVALSHA` (Redis Cluster CROSSSLOT); RedisBloom
CMS/TopK (unloadable on managed/self-host Redis); hard account lockout as the
primary response; distinct `429`/`account_locked` responses (enumeration oracle);
ASN/shared-IP as a hard-block dimension (collateral DoS); and treating hashcash
PoW as a strong control (a botnet out-computes a phone).

## What actually ships

The design above is the record of where login protection is going. v1 ships none
of it — login is bare email+password behind the per-IP `/v1/auth` limiter — and
removes the old machinery so nothing dormant lingers:

1. **The weaponizable, fragmenting lockout is removed entirely.** Deleted
   `internal/user/lockout.go` (the Redis + in-memory + circuit-breaker
   `FallbackFailureCounter`), the `FailureCounter` interface, `SetFailureCounter`,
   and `Service.RecordFailedAttempt`.
2. **The `users.locked_until` backstop is removed too** (column dropped in
   migration 0154, along with `Store.Lock` and `ErrAccountLocked`). After #497
   nothing wrote to it — it was a dormant manual lock with no wired caller, and a
   weak one: a lock only gates the login endpoint, never live sessions, so it
   can't actually stop a compromised account. Incident response for a compromised
   operator account is **password reset (revokes all sessions) / disable**, not a
   login lock. Removing the lock also removes the login timing-oracle concern at
   the source — there is no second check to order after bcrypt.
3. **No automatic per-account throttle ships.** An interim `LoginGuard` +
   `PostgresLoginGuard` — a fixed-window failed-login counter keyed on
   `HMAC(pepper, ip_prefix|account)` (migration 0152, `login_throttle`) — was
   built in PR #497 and then removed (migration 0153 drops the table): a partial
   throttle whose real value only lands with the deferred MFA + breached-password
   controls was premature complexity to carry at zero customers. Login brute-force
   protection is deferred as **one unit** (below), built to the target design
   rather than grown from this stub. Until then, the per-IP `/v1/auth` rate
   limiter is the brute-force floor.

## Deferred (documented, triggered)

The whole protection is deferred; these are the phases, cheapest-outer to
real-inner:

- **The `LoginGuard` seam + the Postgres `(IP-prefix × account)` throttle** — the
  first buildable layer (built in #497, since removed). Rebuild it to the
  target design: the `Check`/`Record` seam, `HMAC(pepper, …)` subjects (a dedicated
  pepper key, or `crypto` derivation from `VELOX_ENCRYPTION_KEY`), and a fixed
  window. Trigger: any real user traffic, or the MFA phase below (whichever first).
- **MFA / step-up (TOTP homegrown, per ADR-014) and login-time breached-password**
  — the controls that actually stop distributed stuffing. *These are the highest
  security value and should lead the next phase.* Trigger: any real user traffic /
  a DP security review (ADR-011/014 named "a DP demands MFA/SSO").
- **The graduated ladder** (tarpit → CAPTCHA/PoW → step-up) behind the existing
  `Decision` seam — replaces the current allow/deny with softer rungs.
- **Aggregate credential-stuffing detection** (per-account fan-in HLL, per-IP
  spread, global-rate EWMA) as **alert-only** observability + runbooks.
- **The optional Redis accelerator** under the I1/I2 invariants, if the
  failed-login path ever becomes hot (it essentially never does for operator
  login).
- **A dedicated throttle pepper key** (separate from `VELOX_ENCRYPTION_KEY`), and
  a **retention sweep** (`DELETE WHERE window_start < now() - ttl`) wired on the
  scheduler — the throttle table's only growth vector; negligible at operator-
  login volume, so deferred with the throttle itself.

### Resource protection (bcrypt-DoS) — a SEPARATE axis, mostly not app-level

Everything above answers "is this the real person?" (protect the *account*). It
does nothing for "are we being flooded?" (protect the *service*). These are
different problems and must not be conflated: bcrypt is deliberately ~100–250ms
per verify, so it is a CPU **amplifier** — a distributed login flood that never
guesses a password and never beats MFA can still pin cores and starve
billing/ingest. **Do not ship MFA and declare brute force "solved": MFA protects
accounts, not CPU; the resource door stays open until the items below are.**

- **Edge-first.** Volumetric floods belong at the deployment edge — WAF/CDN, edge
  rate-limiting, per-IP connection caps, autoscaling — **not** in the binary.
  Building app-level DDoS machinery is over-engineering; the operator's reverse
  proxy is the front line (also why `TRUST_PROXY` matters). Industry norm: leaders
  offload auth and/or absorb floods at the edge, not with a bespoke in-app governor.
  **Self-hosted deployments almost always still run a reverse proxy / LB** (nginx,
  Caddy, Traefik, HAProxy, cloud LB) — if only for TLS termination — so an edge is
  nearly always present; the bare binary exposed directly is the rare exception. A
  plain proxy stops volumetric/connection floods but not *distributed* bcrypt
  amplification (it doesn't know `/v1/auth` is expensive). The cheapest fix for that
  is an **endpoint-scoped rate limit on `/v1/auth` at the proxy they already run**
  (nginx `limit_req`, Caddy, HAProxy stick-tables) — edge-layer, no app code.
- **Already shipped, and sufficient for now:** the per-IP `/v1/auth` limiter +
  the ingest bulkhead (separate bucket) — bounds per-source amplification and keeps
  a login flood off the ingest/billing budget. This is the correct app-level posture
  pre-launch; nothing to add.
- **The one in-app lever to add IF a self-hoster hits real abuse:** a **bounded
  login-CPU budget** — a semaphore / small worker-pool around the password verify,
  sized to a fraction of cores, that sheds excess to `429` *before* bcrypt. It is
  fail-safe (login degrades and recovers; billing/ingest keep running) and
  non-weaponizable (it bounds *total* login work, never targets an account). The
  ladder's CAPTCHA/PoW rung above does the same job from the other side — an
  unsolved challenge never reaches bcrypt.
- **Trigger — measured, not speculative.** Build it when `/v1/auth` request-rate +
  latency (already emitted by the metrics middleware) spike **and** coincide with
  rising latency/errors on non-login endpoints (billing/ingest) — i.e. login bcrypt
  is measurably stealing CPU from real work. A login spike alone is not enough; it
  must be hurting something else. And only once the cheaper layers can't absorb it,
  in order: (1) a cloud/horizontally-scaled deploy behind a real edge (WAF/CDN +
  autoscaling) likely **never** triggers — the edge sheds the flood and scale dilutes
  bcrypt; (2) a self-host behind its (almost-always-present) reverse proxy adds an
  **endpoint rate limit on `/v1/auth`** at that proxy first — still no app code. The
  in-app budget is the **sole** lever only in the genuinely edge-less corner (bare
  binary exposed, no proxy to endpoint-limit) under real distributed abuse — rare.
  NOT triggered by launch, customer count, or
  "feels risky." Cheap pre-work (do this instead of building): keep the signal
  watchable — `/v1/auth` rate+latency already is; add a bcrypt-concurrency gauge if
  the threshold ever needs to be unambiguous. Named here so nobody (a) bolts a CPU
  governor into the binary prematurely, or (b) calls brute force solved with the
  resource axis still open.

## Consequences

- **Removes a real DoS lever and a PII leak**, and eliminates the fragmentation
  class — a net security improvement even with no throttle in its place.
- **Honest reduction to be aware of:** v1 has **no automatic per-account
  brute-force control** on login. Repeated wrong passwords keep returning the same
  generic 401; the per-IP `/v1/auth` limiter is the only floor. A single-account
  brute force — distributed *or* from one source under the per-IP cap — is not
  throttled until the deferred layers land. This is stated, not hidden; the
  trigger to build is any real user traffic or a DP security review.
- MANUAL_TEST FLOW A1's account-lockout steps are removed in the same PR (there is
  no lockout or throttle behavior left to assert).

### Residuals to design against (they apply to the DEFERRED throttle, not to any shipped code)

Inherent to a lean IP-prefix throttle — recorded here so the rebuild designs
around them, and the reason the graduated ladder + MFA are the *real* fix:

- **TRUST_PROXY precondition.** The non-weaponizability rests on the client IP
  being the real client's. Behind a proxy with `TRUST_PROXY` unset, every peer is
  the proxy → all clients collapse to one prefix → the throttle degrades toward a
  bare-account lock. The rebuild should add a production boot-WARN; the real fix is
  correct deployment. **Do not run behind a proxy without `TRUST_PROXY`.**
- **Colocated-attacker collateral.** An attacker SHARING the owner's IP-prefix
  (same office NAT, CGNAT/mobile pool, VPN exit, cloud region) can hold the owner
  out ~15 min per 10 requests on the shared `(prefix × account)` subject. Narrower
  than the old any-email lock (needs colocation) but real; the deferred
  tarpit/CAPTCHA rung (a challenge the owner passes, not a block) is the fix.
- **IPv6 /64 granularity.** RIRs route /56–/48 to one customer, so a routed /56
  yields 256 /64s = 256× the guess budget against one email. `/64` is a tunable
  default; a coarser mask trades collateral for budget.
- **Concurrency burst (Check→Record TOCTOU).** Check and Record are separate
  statements, so a simultaneous burst can overshoot the threshold before an
  increment is observed — the throttle bounds steady rate, not a single burst.
  bcrypt cost-12 is the per-attempt floor; a pre-bcrypt concurrency cap is deferred.
- **Retention.** The throttle table's window rows are its only growth vector, so
  the rebuild needs a periodic sweep (`DELETE WHERE window_start < now() - ttl`)
  on the scheduler. At operator-login volume growth is negligible and stale rows
  are inert (ignored by the window predicate), so it can land with the throttle.
