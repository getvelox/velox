# Velox — Claude Code Context

## What is this?
Velox is an open-source usage-based billing engine built in Go. It handles pricing, subscriptions, usage metering, invoice generation, Stripe payments, dunning, and customer credits.

## Architecture
- Per-domain packages in `internal/` — each domain owns store, service, handler
- No cross-domain **concrete-service/store** coupling between peer business domains: a domain never calls another domain's `Service`/`Store` directly. Allowed cross-domain imports are (a) cross-cutting infra (`auth`, `audit`, `session`), (b) shared value types / DTOs / validation helpers (chiefly `tax.*`, `subscription.ListFilter`), and (c) the `billing` coordinator, which orchestrates peers via narrow **consumer-defined** interfaces. Enforced by `internal/arch/boundaries_test.go` — a new cross-domain import edge fails the test until it's justified in the allowlist.
- Billing engine coordinates via narrow interfaces
- PostgreSQL with Row-Level Security for tenant isolation
- chi/v5 for HTTP routing

## Key patterns
- Store interfaces per domain (not a god Repository)
- RLS via `db.BeginTx(ctx, postgres.TxTenant, tenantID)`
- API key auth with 3 types: platform, secret, publishable
- HMAC-SHA256 webhook signing (both inbound Stripe and outbound)

## Money-path changes — read the playbook first
Any change touching money or a state machine (invoices, payments, credits,
dunning, subscriptions, tax) follows **[docs/dev/money-path-robustness-playbook.md](docs/dev/money-path-robustness-playbook.md)**.
The one rule it exists to enforce: **don't reason locally — enumerate the state's
complete site-set (every writer, effect-firer, gated reader, caller/callee, crash
point) before writing.** Use it at four stages: design (site-set enumeration +
adversarial panel), implementation (the 12 gates), review (the per-class lens),
and tests (collision + real-Postgres + concurrent-resolver fake + mutation-verify).

## Running locally
```bash
docker compose up -d postgres
DATABASE_URL="postgres://velox:velox@localhost:5432/velox?sslmode=disable" go run ./cmd/velox-bootstrap
DATABASE_URL="postgres://velox:velox@localhost:5432/velox?sslmode=disable" RUN_MIGRATIONS_ON_BOOT=true go run ./cmd/velox
```

## Testing
```bash
go test ./... -short          # unit tests only
go test -p 1 ./... -short=false  # includes integration tests (needs postgres)
```

## Concurrent sessions

Two or more Claude Code sessions may work this repo at the same time. Rules:

- **Every session works in its own git worktree** (`.claude/worktrees/<task-name>`). The main working tree stays parked on `main` — no session edits it, switches its branch, or stages files there.
- **Claim a migration _or ADR_ number** by checking origin/main **and** every local branch (`git worktree list`, `git branch -a`) — another session's unmerged migration/ADR may already hold the next number. Migration duplicates fail loudly at integration-test time; **ADR duplicates fail silently** (two `050`s shipped via #196/#197, only caught + renumbered much later in #292), so `grep docs/adr/` across branches before picking an ADR number too.
- **Stay on disjoint domains/packages** where possible. CHANGELOG/MANUAL_TEST conflicts are expected and cheap: whoever merges second rebases and keeps both sides' entries.
- **Merge small PRs promptly; rebase onto origin/main before push.** A conflicting PR gets no CI (GitHub can't build the merge ref) — rebase first, then watch checks.
- **Shared singletons**: one local Postgres (concurrent `-short=false` runs from two sessions can interfere — run unit tests freely, treat CI as the integration gate) and one `make dev` / vite (ports 8080/5173).

## Important decisions
- Auth: dashboard uses email + password; API uses Bearer keys. Dashboard `POST /v1/auth/login` validates against `users.password_hash` (bcrypt cost 12) and mints an httpOnly `velox_session` cookie bound to `users.id` — not to any API key. SDK / curl callers send `Authorization: Bearer <vlx_…>`; `internal/session.MiddlewareOrAPIKey` accepts either, cookie taking precedence. Password reset uses single-use 1h tokens delivered via SMTP (Mailpit in local dev — `docker compose up -d mailpit`). Team invites shipped 2026-07-06 (ADR-081): tokenized email invites, member removal with session revocation, NO RBAC — every member holds the full permission set, role recorded but unenforced. No 2FA in v1. See `docs/adr/011-email-password-auth-and-clean-api-keys.md`; ADR-007 and ADR-008 are superseded.
- Email: single delivery path. `Sender` returns `ErrSMTPNotConfigured` when `SMTP_HOST` is unset — no stdout fallback. Boot logs WARN once per missing email-link env (`HOSTED_INVOICE_BASE_URL`, `PAYMENT_UPDATE_URL`, `DASHBOARD_BASE_URL`); the producer always wires the real adapter.
- Stripe: PaymentIntent-only pattern (no Stripe Billing/Invoices to avoid 0.5% fee)
- No Temporal dependency in v1 — simple background goroutine loops, each a leader-leased singleton role (`internal/platform/leader`, ADR-114: a per-tick lease row on the database clock, its fence token re-checked inside every claim statement; safe behind transaction-mode poolers). Redis used for distributed rate limiting only.
- Credits use event-sourced ledger (immutable append-only)

## Walking MANUAL_TEST — read the strategy first

Walking a flow (proving behavior) follows
**[docs/dev/manual-test-strategy.md](docs/dev/manual-test-strategy.md)** — the
proof-side sibling of the money-path playbook. It carries the five lenses
(behavior / money / honesty / design / **visual**), the twelve techniques that
have each caught a real defect here (control-vs-treatment, negative controls,
provider-side verification, mutation verification, first-use bias, …), the
per-box protocol, and the evidence standard for an annotation. The one rule
worth repeating inline: **text/DOM assertions are structurally blind to layout
bugs — a UI box is not walked until a screenshot of its interactive state has
been looked at.**

## Documentation discipline

Every user-visible ship updates the docs that describe it, in the same PR:

- `CHANGELOG.md` (Keep-a-Changelog) — what shipped, dated.
- `MANUAL_TEST.md` — add or revise the matching FLOW so the assertions still match observable behavior. If a flow lies, future-you can't run it; that's the rot trigger we already paid for once. Trimmed shape (post-2026-05-02): one observable per checkbox, no preamble prose, drop DB introspection unless it's the actual assertion. Stale flows = delete or rewrite, not leave-and-document.
- `docs/adr/` if the change is a decision worth re-litigating later.
- `README.md` "Recently shipped" / "In flight" sections — keep aligned with reality; if a "Roadmap" item is silently descoped, edit the README first, then act.

The bar isn't "perfect docs." The bar is "the doc doesn't lie." A flow that says "logs link to stdout" when the code returns an error is worse than no flow.
