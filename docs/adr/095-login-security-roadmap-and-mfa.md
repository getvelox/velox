# ADR-095: Login-security roadmap & MFA design — minimal-complete for a self-hostable B2B money dashboard

Date: 2026-07-17
Status: Accepted as the DESIGN OF RECORD. The **decision** is settled — pull **MFA + offline breached-password forward** out of ADR-094's indefinite "deferred," build them in the *safe-complete* form below, and mark the consumer-scale controls **never** for this product shape. **Phasing amended 2026-07-18 (landing gates).** Split the deferred work. The two Phase-0 **floor** items — the embedded breached-password set + prod-TLS/`Secure` cookie — are **decoupled from the MFA bundle** and made a **hard pre-first-deployment gate**: cheap, non-weaponizable, **zero rework** when MFA lands, so deferring them alongside Phase 1 buys nothing. **Phase 1** (homegrown TOTP MFA **+** the ADR-094 per-account throttle, shipped as one unit) stays deferred but is itself a **hard gate — no first external/production deployment ("first DP") goes live without it** — the moment the NIST §5.2.2 per-account-failure gap stops being theoretical (rotating IPs otherwise give unlimited per-account guesses). No code ships from this ADR; these are the landing conditions.
Relates: ADR-011 (email+password auth), ADR-014 (homegrown/embedded SSO+MFA, no SaaS vendor, air-gap constraint), ADR-081 (team invites, **no RBAC** — every member holds full permissions), ADR-093 (CSRF gate), ADR-094 (login brute-force protection — this ADR **refines** its deferred plan).

## Summary

This ADR is the design of record. No code ships from it, and as of now neither control below is built. Velox keeps its existing login protections and adds two controls that were previously deferred: multi-factor authentication (MFA) with time-based one-time codes (TOTP), and a check that rejects known-breached passwords when a password is set. By default both run inside the `velox` binary with no outside service, because self-hosted deployments may have no internet access. MFA must ship complete, not as bare TOTP. A correct password only opens a short-lived partial session, which is capped on wrong codes and guarded against code reuse. Recovery codes and a break-glass reset command cover lost devices. MFA is forced on the bootstrap operator, and sensitive actions such as revealing Stripe keys ask the user to re-authenticate. Consumer-scale controls such as CAPTCHA, automatic account lockout and risk-based login are never built for this operator-only dashboard. Amended 2026-07-18: the embedded breached-password set and required TLS with a `Secure` cookie in production must ship before the first deployment, separately from MFA. TOTP MFA and the per-account login throttle ship together and must be live before the first external or production deployment. The throttle is keyed on IP and account together and slows repeated failures down without ever locking the account.

## Context

Velox's login surface is a **small, known, operator-only B2B dashboard** guarding Stripe keys and customer PII — not a consumer sign-in wall. A design review during the MANUAL_TEST walkthrough asked two questions ADR-094 left open: (1) should MFA + breached-password be pulled forward, given a serious B2B customer's security review will gate on them for a money product; and (2) which of ADR-094's deferred controls are genuinely needed for *this* shape versus consumer-scale over-engineering to cut.

To answer them without hand-waving, an **adversarial design panel** ran (four independent designs from different lenses — MVP, compliance/SOC2, self-host/air-gap, attacker-economics — each red-teamed, then synthesized). This ADR records the synthesized result.

### Process caveat (and corrections)

Two honesty notes about the panel:

- **One lens dropped** (attacker-economics) to a tooling failure; the synthesis ran on three fully red-teamed lenses. The remaining critiques covered the attacker-economics angle well, but the design **should get a fresh red-team pass at build time** (Phase 1), which happens naturally.
- **The panel agents inspected a stale worktree branch** (`fix/org-tz-seam-overbill`, predating this session's merges), so several of its "verified in-tree, current-state" claims are **wrong for `main`** and were re-verified. Corrections:
  - It flagged the weaponizable email-keyed lockout as "still live, no removal migration." **False on `main`** — `internal/user/lockout.go`, `users.locked_until`, and `ErrAccountLocked` were removed (#497→#499, migration 0154). That "remove the lockout" item is **done**.
  - It flagged `ClientIP` as trusting a spoofable leftmost `X-Forwarded-For`. **Already fixed on `main`** — `middleware.TrustedRealIP` only honors XFF from allow-listed trusted proxies.
  - Genuinely still open: the breached-password check is still the ~10-entry `commonPasswords` stub in `ValidatePassword`.

  Net: the panel's "Phase 0 NOW" list is *mostly already shipped*; only the breached-password set and a TLS/Secure-cookie prod check remain.

## Decision — the minimal-complete design

Keep the lean floor, pull exactly two things forward (MFA + breached-password), build MFA in a form that survives a security review, and refuse the consumer-scale machinery.

### Phase 0 — floor (mostly already shipped)

Keep bcrypt-12, the per-IP `/v1/auth` limiter, constant-time anti-enumeration (identical 401 + dummy bcrypt), the min-12 / 72-byte / common-password policy, the CSRF gate, session-revoke-on-reset, single-use hashed reset tokens, and `TrustedRealIP`.

**Remaining Phase-0 work — a HARD pre-first-deployment gate (amended 2026-07-18), NOT bundled with MFA:** (a) replace the `commonPasswords` stub with the embedded offline breached set (below); (b) mandate TLS in production — `Secure` cookie in prod, plain-HTTP is **dev-only**. Reject the framing of plain-HTTP LAN as a *feature*: over HTTP the session cookie and TOTP are sniffable and MFA buys nothing against a LAN adversary.

Why decoupled from Phase 1: today's live password floor is a **~10-entry `commonPasswords` stub**, so human-pattern passwords (`Nimbus@2026!`, `Password1234`) pass min-12 and land squarely in a distributed guesser's first attempts. The embedded breached set is **cheap, non-weaponizable, and needs zero rework when MFA arrives** — so deferring it *with* the MFA bundle costs real protection for no benefit. It is the one item that must not wait.

### The strategic pull-forward: homegrown TOTP MFA (Phase 1)

MFA is the one control that covers the **stolen/stuffed correct-password** threat that bcrypt, policy, and any throttle cannot — and the line-item a B2B security review gates on for a Stripe-key holder. **Landing gate (amended 2026-07-18): Phase 1 = MFA + the ADR-094 per-account throttle as one unit, and no first DP ships without it live.** A throttle alone is half a fix (it caps distributed *guessing*, not a *correct stolen* password); MFA alone leaves distributed guessing uncapped. With **no RBAC** (ADR-081) any one member's password is a full-org-compromise target, so this gate hardens before real teams exist. Build it in its **safe-complete** form; the pieces below are inseparable (shipping TOTP without them is the trap all three panel designs initially fell into):

- **Method:** TOTP per RFC 6238 (HMAC-SHA1, 6 digits, 30 s step, ±1 step skew, 160-bit `crypto/rand` secret), via a **vendored in-process pure-Go lib** (e.g. `pquerna/otp`, MIT). "Homegrown" (ADR-014) means *in-process, no SaaS, no runtime network* — not hand-rolled crypto. TOTP is the baseline over WebAuthn (origin-agnostic, no attestation/RP-ID config); WebAuthn/passkeys is a **later** additive phishing-resistant factor, not the baseline. **Not** SMS/email OTP.
- **Enrollment:** self-service in Settings→Security. Server mints the secret, stores it **AES-256-GCM-encrypted** (`VELOX_SECRET_ENCRYPTION_KEY`; **fail-loud at boot** if MFA is enabled and the key is unset), marked *pending*, and returns an `otpauth://` URI + a **locally-rendered** QR data-URI (no external QR service; issuer/label from the deployment's configured name, validated so two self-hosted instances don't collide in an authenticator app). The user must enter one current code to **confirm binding** before activation (catches mis-scans).
- **Verification state machine — the load-bearing part** (omitted by every source design, flagged by every critique): a correct password mints a **short-TTL (~5 min), single-use, server-side partial-session bound to the RESOLVED user id** (never a client-supplied id) with **no dashboard access**; only a valid TOTP or recovery code upgrades it to the real `httpOnly velox_session` cookie. **Enforce at session-mint, not per-handler** (avoids a missed-gate hole).
- **MFA needs its OWN throttle** (the key insight): a 6-digit TOTP with ±1 skew is ~3 live codes per million — an **online-brute-forceable** secret the attacker attacks *after* passing the password. So the partial-session carries a **per-session OTP-attempt cap** (e.g. 5 wrong → invalidate, force password re-auth) plus a **single-use / last-consumed-timestep replay guard**. This cap is **non-weaponizable** — it only slows the already-password-authenticated OTP step, nothing like the removed account lockout.
- **Recovery:** 10 single-use, ≥128-bit, hashed-at-rest recovery codes, shown once at confirm, burned on use, regenerate-invalidates-prior + fires a notification. **Primary offline recovery — deliberately not email/SMS** (SMTP is optional here, and email shares the compromise surface — the same circularity that rules out email-OTP). Lost-everything fallback: a **break-glass MFA-reset subcommand in the MAIN `velox` binary** (so single-operator containers aren't bricked — not a separate `velox-admin` a locked-down PaaS may lack), audited (`mfa_break_glass_reset`) on every use. **Password reset must NOT short-circuit MFA** (post-reset login still presents the second factor).
- **Enforcement + the no-RBAC implication (ADR-081):** a per-user enrolled flag + an org-level `require_mfa` policy. Because there is **no RBAC** (every member holds full permissions), disabling MFA, toggling `require_mfa` off, or resetting another member's MFA must require **step-up re-auth or CLI break-glass** — never a bare shared-permission action, or one compromised member strips the org's second factor. **MFA is forced on the platform/bootstrap operator that holds the Stripe keys** — not left to a default-off toggle. Recommend `require_mfa` **default-ON for new orgs** (with an unmissable enroll nag), **default-OFF only for the very first bootstrapping operator** so they can't self-lock on day one.
- **Step-up re-auth for crown-jewel actions:** reveal/rotate Stripe keys, change payout/bank details, disable MFA, toggle `require_mfa`, view/rotate API keys, invite/remove members. Login is otherwise the only gate — a stolen 7-day session or an unlocked laptop would otherwise mean full key exfiltration with no re-challenge.
- **Audit vocabulary (repo-specific must-do):** the audit vocabulary is **closed and totality/CHECK-constraint-gated**. Adding MFA requires extending it — `mfa_enrolled`, `mfa_disabled`, `mfa_challenge_failed`, `recovery_code_used`, `mfa_break_glass_reset` — **with its CHECK-expansion migration in the same commit**, or the events are literally unrecordable and fail the totality test. Reference `user_id`, never plaintext email.
- **Security-notification emails** (best-effort, degrade gracefully when SMTP is absent): MFA disabled, recovery codes regenerated, password changed, new-device login. The one real-time signal that catches a silent takeover — passive audit rows nobody reads don't.

### Offline breached-password (Phase 0/1)

Reject known-breached passwords **at password-SET time only** (bootstrap-create, reset-confirm, future change-password), reusing the existing `ValidatePassword` seam. Ship an **embedded** (compiled into the binary — not a mountable sidecar that can be absent and silently fail-open) **exact-match ordered top-N (~1M by prevalence)** set (binary search / map: zero false positives, can't be absent, no filter tuning). Refresh out-of-band by rebuilding in a release — **never fetched at runtime**. **No** login-time recheck (MFA is the better answer to "a password later got breached"; login-time adds latency + a timing oracle + forced resets). **No** runtime `api.pwnedpasswords.com` call by default (air-gap); the HIBP k-anonymity API is at most an explicit opt-in for internet-connected deployments.

### Minimal `(IP × account)` soft throttle — conditional, simplified

The non-weaponizable account-facing brake for **non-MFA** users and a bcrypt-DoS damper: a Postgres-backed increasing-delay / temporary-429 on `(IP × account)` — **no bare account lock, and NO IP-prefix bucketing**. Prefix bucketing (/24, /64) is botnet-defeating consumer-scale sophistication for a distributed-guessing pressure that doesn't exist at zero customers; plain `(IP × account)` is the pre-launch-complete shape. This **refines ADR-094**, which specified prefix keying. Build it with the MFA package or when observed pressure appears — whichever first.

## Phased roadmap (with triggers)

- **Phase 0 — floor (mostly done):** remaining = embedded breached-password set + TLS/Secure-cookie in prod. *Trigger: immediate; small self-contained PRs.*
- **Phase 1 — the MFA package (committed pre-launch):** homegrown TOTP in the safe-complete form above (confirm-on-enroll, encrypted secret, partial-session, OTP-attempt cap, replay guard, recovery codes, break-glass, force-on-bootstrap-operator + `require_mfa`, step-up for crown jewels, audit-vocab extension + CHECK migration, security-notification emails, minimal `(IP × account)` throttle). *Trigger: **before onboarding the first external operator or the first DP security review** — built ahead of the deadline, not scrambled under it.*
- **Phase 2 — DP-triggered, additive:** embedded in-process OIDC/SAML SSO (`zitadel/oidc` + `zitadel/saml`, ADR-014); WebAuthn/passkeys as an additive phishing-resistant factor for HTTPS deployments; optional opt-in HIBP k-anonymity (off by default); RBAC to properly scope the `require_mfa` / MFA-disable boundaries. *Trigger: a named DP demand (SSO/SAML, hardware-key MFA) or a compliance control the enforced-MFA + top-N set doesn't satisfy.*
- **Phase 3 — conditional (only under observed pressure):** a self-hosted proof-of-work step-up rung, IP-prefix bucketing on the throttle, login-time breached recheck — each only if the concrete pressure shows in logs, none with CAPTCHA or a SaaS vendor. *Trigger: credential-stuffing / distributed guessing actually observed.*

## Do NOT build (never — for this product shape)

Structurally mismatched to an operator-only, air-gapped, tiny-user-set money engine; revisit only on a fundamental pivot to public/consumer signup:

- **Bare per-account hard lockout** (auto-lock after N fails) — weaponizable DoS (OWASP anti-pattern); never touches live sessions so it can't stop a compromised account. (Already removed; do not reintroduce.)
- **Aggregate credential-stuffing detection** (HLL fan-in / EWMA) — needs millions of accounts for a base rate; nothing to fan in at N-operators.
- **Graduated challenge ladder** (tarpit → CAPTCHA/PoW → step-up) — machinery for a public-signup population to grade; Velox has none.
- **CAPTCHA** (reCAPTCHA/hCaptcha/Turnstile) — outbound third-party calls break the air-gap constraint.
- **Risk-based/adaptive auth, device fingerprinting, impossible-travel, trusted-device remember-me** — need telemetry volume + a fraud model a known-operator set never produces.
- **SMS or email OTP as an MFA factor** — SIM-swap-weak / paid gateway / SMTP-optional and re-secures the account via the reset channel (circular).
- **Full 850M-hash HIBP corpus + login-time recheck**, and **runtime HIBP API as the default** — ~1 GB / maintenance burden that stops paying off once MFA is enforced; outbound HTTPS breaks air-gap.
- **IP-prefix bucketing on the throttle** — consumer-scale botnet defense for pressure that doesn't exist (Phase-3 conditional at most).
- **Edge-first bcrypt-DoS appliance / WAF tier** — incoherent with a single-binary self-host model (no shared edge to place it at).
- **Redis as a hard dependency** for throttling/lockout — self-host stays single-binary + Postgres.
- **Dedicated MFA-secret KMS/HSM/pepper-rotation subsystem** — one env AES-GCM key is enough for secret-at-rest.
- **SaaS auth vendor** (Auth0/WorkOS/Clerk/Cognito/Duo/Stytch) — ADR-014 hard constraint.

## Open decisions (to settle at Phase 1)

- **`VELOX_SECRET_ENCRYPTION_KEY` lifecycle:** document a backup + rotate-with-reencrypt runbook, and make a missing/changed key **degrade per-login-gracefully** (block MFA users, keep non-MFA login and the billing engine up) rather than boot-crash. A real mass-brick path if unspecified.
- **`require_mfa` default:** recommended default-ON for new orgs (enroll nag), default-OFF only for the first bootstrapping operator. Confirm acceptable vs. opt-in.
- **Clock drift:** air-gapped boxes drift without NTP; ±1 step is thin. Document a clock-sync requirement + a "server clock may be off" diagnostic rather than widening skew (which enlarges the brute-force window).
- **Breach-corpus shape/size:** embedded exact-match top-N (recommended) vs bloom filter; confirm N.
- **The Bearer `vlx_` API-key surface is owned-but-separate** — it's the credential that actually moves money, and MFA protects only the *dashboard* door. Name it explicitly (rotation/scoping/leak-response) so a security review isn't surprised.

## Relationship to ADR-094

This ADR **refines** ADR-094's deferred plan: **MFA + breached-password move from "deferred" to Phase 1 "next";** the consumer-scale items (ladder, HLL/EWMA, CAPTCHA, risk-based, edge tier) are reclassified **never** for our shape; the throttle is **simplified to plain `(IP × account)`** (prefix bucketing → Phase-3 conditional). ADR-094 remains the record of the brute-force-throttle reasoning; ADR-095 is the record of the *whole* login-security posture and the MFA design.
