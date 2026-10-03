# Velox — Claude Code Context

## What is this?
Velox is an open-source usage-based billing engine built in Go. It handles pricing, subscriptions, usage metering, invoice generation, Stripe payments, dunning, and customer credits.

## Rules at a glance
Each rule is explained in the section named after it.

- **Architecture:** a domain never calls another domain's `Service`/`Store` directly. `internal/arch/boundaries_test.go` enforces this.
- **Money-path changes:** before writing, list every code place that touches the state. Follow the money-path playbook.
- **Walking MANUAL_TEST:** a UI box is not walked until you have looked at a screenshot of its interactive state.
- **Concurrent sessions:** work in your own git worktree, and check every branch before you claim a migration or ADR number.
- **Documentation discipline:** every user-visible ship updates its docs in the same PR. The doc must not lie.

## Architecture
- Per-domain packages in `internal/`. Each domain owns its store, service and handler.
- **Rule:** no cross-domain **concrete-service/store** coupling between peer business domains. A domain never calls another domain's `Service`/`Store` directly.
- **Allowed cross-domain imports:**
  - (a) cross-cutting infra (`auth`, `audit`, `session`);
  - (b) shared value types, DTOs and validation helpers (chiefly `tax.*`, `subscription.ListFilter`);
  - (c) the `billing` coordinator. The billing engine orchestrates peers through narrow **consumer-defined** interfaces.
- **Enforced by** `internal/arch/boundaries_test.go`. A new cross-domain import edge fails the test until it is justified in the allowlist.
- PostgreSQL with Row-Level Security for tenant isolation
- chi/v5 for HTTP routing

## Key patterns
- Store interfaces per domain (not a god Repository)
- RLS via `db.BeginTx(ctx, postgres.TxTenant, tenantID)`
- API key auth with 3 types: platform, secret, publishable
- HMAC-SHA256 webhook signing (both inbound Stripe and outbound)

## Money-path changes — read the playbook first
Any change that touches money or a state machine follows **[docs/dev/money-path-robustness-playbook.md](docs/dev/money-path-robustness-playbook.md)**.
State machines here are invoices, payments, credits, dunning, subscriptions and tax.

**The one rule the playbook enforces:** do not reason locally. Before writing, enumerate the state's
complete site-set (every writer, effect-firer, gated reader, caller/callee, crash
point).

- **Site-set:** every code place that touches one state, such as an invoice status.
- **Effect-firer:** code that sends an email, webhook, Stripe call or ledger write on a change.
- **Gated reader:** code that branches on the state.

See the [glossary](docs/README.md#glossary) for the full entry. Use the playbook at four stages:

| Stage | What the playbook gives you |
|---|---|
| Design | site-set enumeration + adversarial panel |
| Implementation | the 12 gates |
| Review | the per-class lens |
| Tests | collision + real-Postgres + concurrent-resolver fake + mutation-verify |

A concurrent-resolver fake is a test double that commits the racing transition at the external-call boundary, or between the precondition read and the write.

## Running locally
```bash
docker compose up -d postgres
DATABASE_URL="postgres://velox:velox@localhost:5432/velox?sslmode=disable" go run ./cmd/velox-bootstrap
DATABASE_URL="postgres://velox:velox@localhost:5432/velox?sslmode=disable" RUN_MIGRATIONS_ON_BOOT=true go run ./cmd/velox
```

## Testing
```bash
go test ./... -race -short -count=1          # unit tests only (what CI runs)
go test -p 1 ./... -count=1 -short=false  # includes integration tests (needs postgres)
```

## Concurrent sessions

Two or more Claude Code sessions may work this repo at the same time. Rules:

- **Every session works in its own git worktree** (`.claude/worktrees/<task-name>`). The main working tree stays on `main`. No session edits it, switches its branch, or stages files there.
- **Claim a migration _or ADR_ number** only after checking origin/main **and** every local branch (`git worktree list`, `git branch -a`). Another session's unmerged migration or ADR may already hold the next number.
  - Also run `grep docs/adr/` across branches before you pick an ADR number.
  - Why: a duplicate migration number fails loudly at integration-test time, but a duplicate ADR number fails silently. Two `050`s once shipped (#196/#197) and were only caught and renumbered much later (#292).
- **Stay on disjoint domains/packages** where possible. CHANGELOG/MANUAL_TEST conflicts are expected and cheap: whoever merges second rebases and keeps both sides' entries.
- **Merge small PRs promptly; rebase onto origin/main before push.** A conflicting PR gets no CI, because GitHub cannot build the merge ref. Rebase first, then watch checks.
- **Shared singletons:**
  - One local Postgres. Concurrent `-short=false` runs from two sessions can interfere. Run unit tests freely, and treat CI as the integration gate.
  - One `make dev` / vite (ports 8080/5173).

## Important decisions
- **Auth:** dashboard uses email + password; API uses Bearer keys.
  - Dashboard `POST /v1/auth/login` validates against `users.password_hash` (bcrypt cost 12). It mints an httpOnly `velox_session` cookie bound to `users.id`, not to any API key.
  - SDK / curl callers send `Authorization: Bearer <vlx_…>`. `internal/session.MiddlewareOrAPIKey` accepts either; the cookie takes precedence.
  - Password reset uses single-use 1h tokens delivered via SMTP (Mailpit in local dev: `docker compose up -d mailpit`).
  - Team invites shipped 2026-07-06 (ADR-081): tokenized email invites, and member removal with session revocation.
  - No RBAC: every member holds the full permission set. The role is recorded but not enforced.
  - No 2FA in v1.
  - See `docs/adr/011-email-password-auth-and-clean-api-keys.md`; ADR-007 and ADR-008 are superseded.
- **Email:** single delivery path.
  - `Sender` returns `ErrSMTPNotConfigured` when `SMTP_HOST` is unset. There is no stdout fallback.
  - Boot logs WARN once per missing email-link env (`HOSTED_INVOICE_BASE_URL`, `PAYMENT_UPDATE_URL`, `DASHBOARD_BASE_URL`). The producer always wires the real adapter.
- **Stripe:** PaymentIntent-only pattern (no Stripe Billing/Invoices to avoid 0.5% fee)
- **Background work:** no Temporal dependency in v1.
  - Background work runs as simple goroutine loops. Each loop is a leader-leased singleton role (`internal/platform/leader`, ADR-114).
  - A [leader lease](docs/README.md#glossary) is a per-tick lease row on the database clock. Its fence token is re-checked inside every claim statement. The lease is safe behind transaction-mode poolers.
  - Redis is used for distributed rate limiting only.
- **Credits:** event-sourced ledger (immutable append-only)

## Walking MANUAL_TEST — read the strategy first

Walking a flow (proving behavior) follows
**[docs/dev/manual-test-strategy.md](docs/dev/manual-test-strategy.md)**. It is the
proof-side sibling of the money-path playbook. It carries:

- the five lenses (behavior / money / honesty / design / **visual**);
- the twelve techniques that have each caught a real defect here (control-vs-treatment, negative controls,
  provider-side verification, mutation verification, first-use bias, …);
- the per-box protocol;
- the evidence standard for an annotation.

**The one rule worth repeating here:** text/DOM assertions cannot see layout
bugs. A UI box is not walked until you have looked at a screenshot of its interactive state.

## Documentation discipline

Every user-visible ship updates the docs that describe it, in the same PR:

- `CHANGELOG.md` (Keep-a-Changelog) — what shipped, dated.
- `MANUAL_TEST.md` — add or revise the matching FLOW so the assertions still match observable behavior. A flow whose assertions do not match observable behavior cannot be run; this kind of rot has already cost us once. Trimmed shape (post-2026-05-02): one observable per checkbox, no preamble prose, drop DB introspection unless it's the actual assertion. Delete or rewrite a stale flow; do not leave it with a note.
- `docs/adr/` if the change is a decision worth re-litigating later.
- `README.md` "Roadmap" section ("Recently shipped" / "Explicitly deferred") — keep aligned with reality; if a "Roadmap" item is silently descoped, edit the README first, then act.

How to write them: **[docs/dev/writing-style.md](docs/dev/writing-style.md)** (point first, one idea per sentence, define terms, clarity and truth in separate PRs).

The bar isn't "perfect docs." The bar is "the doc doesn't lie." A flow that says "logs link to stdout" when the code returns an error is worse than no flow.
