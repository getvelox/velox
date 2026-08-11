# Security Policy

## Reporting a vulnerability

Email **security@velox.dev** with a description of the issue. Encrypted reports are accepted via age (`age --recipient age1velox...`); the public key lives at `https://velox.dev/.well-known/age.txt` once the project is mature enough to require it. For now, email is sufficient.

Please do **not** open public GitHub issues for suspected vulnerabilities.

We commit to:

| Stage | Target |
|---|---|
| Acknowledge receipt | within 2 business days |
| Initial triage + severity assessment | within 5 business days |
| Patch landed in main | within 30 days for high/critical, 90 days for medium, best-effort for low |
| Public disclosure (with credit) | after a fixed release is available, coordinated with the reporter |

## In scope

- The Velox Go binary (`cmd/velox`, `cmd/velox-bootstrap`, `cmd/velox-bench`, `cmd/velox-doctor`, `cmd/velox-migrate-safety`)
- Code under `internal/` and `pkg/`
- The migration runner and schema in `internal/platform/migrate/`
- The web-v2 dashboard (`web-v2/`)
- Docker Compose deployment config under `deploy/compose/`
- Outbound webhook signing, inbound Stripe webhook verification, API key handling, session/cookie auth, RLS policy enforcement, AES-GCM encryption-at-rest, HMAC blind index for email

## Out of scope

- Vulnerabilities in the operator's deployment environment (Kubernetes cluster, managed Postgres, load balancer, secrets store, IAM)
- Vulnerabilities in third-party services that Velox integrates with (Stripe, cloud providers, email providers, S3, KMS) — report those to the vendor
- Configuration mistakes by the operator (e.g., running with `VELOX_ENCRYPTION_KEY` unset in a non-production env, or exposing the dashboard without TLS termination)
- DoS via traffic flooding (a property of the operator's load balancer + WAF, not Velox)
- Self-XSS, social engineering, physical attacks
- Reports against forks or vendored copies of Velox — please reproduce on the canonical `main` branch first

## Hardening status

Velox is **pre-launch** and pre-audit. Encryption-at-rest, RLS, audit log immutability, HMAC webhook signing, bcrypt (cost 12) passwords, SHA-256 session/API-key/token hashing, security headers, GCRA rate limiting, and TLS-only intent are all implemented. (A SOC 2 control mapping is deferred until a design partner requires it — see the deferred list in the README.)

Known gaps, documented openly:

- No built-in mechanism to rotate `VELOX_ENCRYPTION_KEY` or `VELOX_EMAIL_BIDX_KEY` (envelope encryption rebuild planned)
- No MFA on dashboard login (no MFA in v1; SSO direction is embedded OIDC/SAML per ADR-014 — Velox will not depend on a SaaS auth vendor)
- No SAST in CI (Semgrep / CodeQL planned)
- Dashboard image not signed — CI keyless-signs the server image with cosign (Sigstore), but the `-dashboard` image is published unsigned
- No threat model document (STRIDE / LINDDUN planned)
- No external penetration test on record yet

If you can help close any of these, contributions are welcome via the normal PR process.

## Safe-harbor

We will not pursue legal action against good-faith security research that:

- Does not access, modify, or destroy data belonging to anyone other than the researcher
- Does not degrade availability for other users
- Stays within the in-scope list above
- Coordinates disclosure with us before going public

If a researcher inadvertently violates this policy in good faith, please tell us promptly and we'll work it out.
