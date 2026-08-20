# Contributing to Velox

Thank you for your interest in contributing to Velox.

This page is for anyone preparing a pull request: how to set up a dev
environment, how the code is organized, which checks CI runs against your
change, and the house rules that gate a merge.

## Development Setup

```bash
# Prerequisites: Go 1.25+, Docker; Node 22+ only for dashboard work;
# jq only for scripts/demo.sh

# Clone
git clone https://github.com/getvelox/velox.git
cd velox

# Start Postgres
docker compose up -d postgres

# Run migrations
DATABASE_URL="postgres://velox:velox@localhost:5432/velox?sslmode=disable" go run ./cmd/velox migrate

# Run tests
go test ./... -race -short -count=1    # unit tests (CI runs -race too)
go test -p 1 ./... -count=1 -short=false  # unit + integration tests
```

## Project Structure

Each billing domain (invoice, credit, customer, and so on) is a
self-contained package under `internal/`:

```
internal/{domain}/
  store.go       — Store interface (what the domain needs from persistence)
  postgres.go    — PostgreSQL implementation of the store
  service.go     — Business logic (validates, orchestrates)
  handler.go     — HTTP handlers (decodes request, calls service, writes response)
  *_test.go      — Unit tests (in-memory store) + integration tests (real Postgres)
```

**Rules:**
- Peer domain packages never call each other's concrete `Service`/`Store` types. Cross-domain imports are limited to shared value types, cross-cutting infra, and the narrow interfaces of the `billing` coordinator. The allowlist in `internal/arch/boundaries_test.go` enforces this — a new cross-domain import fails the test until it is added there deliberately
- Every handler uses `respond.JSON()` / `respond.FromError()` for responses
- Every store method runs inside a transaction scoped to one tenant by Postgres Row-Level Security (RLS)
- Tests use `testutil.SetupTestDB()` for integration tests

## Adding a New Domain

1. Create `internal/{domain}/store.go` with the store interface
2. Create `internal/{domain}/postgres.go` implementing the store
3. Create `internal/{domain}/service.go` with business logic
4. Create `internal/{domain}/handler.go` with HTTP handlers using `respond` package
5. Add SQL migration in `internal/platform/migrate/sql/`
6. Wire into `internal/api/router.go`
7. Write tests (both unit with in-memory store AND integration with Postgres)

## Code Style

- `go fmt` and `go vet` must pass
- No exported types without doc comments
- Errors use `errs.DomainError` for domain-specific errors or `fmt.Errorf` for validation
- JSON field names are `snake_case`
- ID format: `vlx_{type}_{random_hex}` (e.g., `vlx_cus_abc123`)

## Frontend (web-v2)

The dashboard is a React + TypeScript + Vite app — see
[`web-v2/README.md`](web-v2/README.md) for setup, dev server, and the
directory's own CI checks. Two things bite first-timers: `npm install`
relies on `web-v2/.npmrc` (a peer-dependency mismatch is deliberately
tolerated — do not "fix" it), and typechecking is `npx tsc -b`, not
`tsc --noEmit` (which silently checks nothing here).

## What CI runs on your PR

Every gate below runs on every PR. Run the ones relevant to your change
locally before pushing, and nothing about a red build will surprise you:

| Gate | Local command | Notes |
|---|---|---|
| Unit tests + race detector | `go test ./... -race -short -count=1` | |
| Integration tests | `go test -p 1 ./... -count=1 -short=false` | needs `docker compose up -d postgres`; test DBs are auto-provisioned |
| Money-invariant sweep | `go run ./cmd/velox-doctor` | runs in CI after the integration suite |
| golangci-lint | `make lint` | config in `.golangci.yml` — much stricter than `go vet` |
| Clock discipline | `make lint-clock` | bans bare `time.Now()` in clock-pinned domains; bypass with `// wall-clock: <reason>` |
| Funding-set invariant | `make lint-funding-set` | period-wide money ops must use the funding-set lookup |
| MANUAL_TEST currency | `make lint-manual-test` | flows may not name things the code no longer has |
| Timezone display | `make lint-tz` | civil-day formats need an explicit zone; bypass with `//tz:ok` |
| Codegen drift | `make gen` then `git diff --exit-code` | required whenever `api/openapi.yaml` changes |
| Vulnerability scan | `govulncheck ./...` | can go red through no fault of yours when a new Go CVE lands — say so in the PR and we'll handle it |
| Frontend | `cd web-v2 && npm run build && npm test && npm run lint` | only if you touched `web-v2/` |
| Golden path | `scripts/demo.sh` | boots a real server and walks recipe → invoice end to end |

Fork PRs get the identical CI run — no secrets are involved in any gate.

## Pull Requests

1. Fork the repo and create a feature branch
2. Write tests for new functionality
3. Run the gates above that your change touches (at minimum the two test commands)
4. **Docs travel with the change, in the same PR:** any user-visible change
   gets a `CHANGELOG.md` entry, and any UI-visible behavior change updates
   the matching flow in `MANUAL_TEST.md` (the repo's hand-run walkthrough
   scripts). This is a hard house rule — the bar is "the doc doesn't lie."
5. Submit a PR with a clear description of what and why

**Touching money or a state machine** (invoices, payments, credits, dunning
(failed-payment retries), subscriptions, tax)? Follow the
[Money-Path Robustness Playbook](docs/dev/money-path-robustness-playbook.md).
Before opening the PR, work through its implementation checklist (§3). In the
PR description, include the state's site-set enumeration (§2) — the list of
every code site that can write or react to that state. Guards for concurrency
and money invariants need an automated test that forces the race and is
verified to fail once the guard is removed (a collision + mutation-verified
test, §5).
