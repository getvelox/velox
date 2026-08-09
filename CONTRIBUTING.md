# Contributing to Velox

Thank you for your interest in contributing to Velox.

## Development Setup

```bash
# Prerequisites: Go 1.25+, Docker, jq

# Clone
git clone https://github.com/getvelox/velox.git
cd velox

# Start Postgres
docker compose up -d postgres

# Run migrations
DATABASE_URL="postgres://velox:velox@localhost:5432/velox?sslmode=disable" go run ./cmd/velox migrate

# Run tests
go test ./... -short -count=1          # unit tests
go test -p 1 ./... -count=1 -short=false  # unit + integration tests
```

## Project Structure

Each billing domain is a self-contained package under `internal/`:

```
internal/{domain}/
  store.go       — Store interface (what the domain needs from persistence)
  postgres.go    — PostgreSQL implementation of the store
  service.go     — Business logic (validates, orchestrates)
  handler.go     — HTTP handlers (decodes request, calls service, writes response)
  *_test.go      — Unit tests (in-memory store) + integration tests (real Postgres)
```

**Rules:**
- No concrete `Service`/`Store` coupling between peer domain packages — cross-domain imports are limited to shared value types, cross-cutting infra, and the billing coordinator's narrow interfaces, enforced by the allowlist in `internal/arch/boundaries_test.go`
- Every handler uses `respond.JSON()` / `respond.FromError()` for responses
- Every store method runs inside an RLS-scoped transaction
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

## Pull Requests

1. Fork the repo and create a feature branch
2. Write tests for new functionality
3. Run `go test -p 1 ./... -count=1 -short=false` (all tests must pass)
4. Submit a PR with a clear description of what and why

**Touching money or a state machine** (invoices, payments, credits, dunning,
subscriptions, tax)? Follow the [Money-Path Robustness Playbook](docs/dev/money-path-robustness-playbook.md):
run the implementation checklist (§3) before opening the PR, and include the
state's site-set enumeration (§2) in the PR description. Concurrency and
money-invariant guards need an automated collision + mutation-verified test (§5).
