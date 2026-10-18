.PHONY: build run test lint migrate migration dev clean cli gen gen-go gen-ts

# Build the velox binary
build:
	go build -o bin/velox ./cmd/velox

# Run the server locally
run: build
	./bin/velox

# Run server locally — loads .env if present
# Setup: cp .env.example .env && edit .env
-include .env
export

dev:
	go run ./cmd/velox

# Run all tests.
#
# `-p 1` runs packages sequentially. Integration tests share a single
# test database; running packages in parallel races each package's
# SetupTestDB truncate against another package's in-flight queries
# and produces flaky e2e failures (visible specifically on
# internal/api e2e tests). test-unit also uses -p 1 for env-var
# isolation; same reasoning applies broader.
#
# VELOX_LIVEMODE_STRICT=true escalates "TxTenant opened without ctx
# livemode" warnings to panics — every test that opens a tenant-
# scoped tx without setting postgres.WithLivemode(ctx, ...) on its
# entry point fails immediately. The escalation lives in tests
# because production logs the warning (loud-once-per-site, not
# fatal); tests catch the bug at PR time, not in customer logs.
test:
	VELOX_LIVEMODE_STRICT=true go test -p 1 ./... -count=1

# Run tests with short flag (unit only, sequential to avoid env var leaks)
test-unit:
	VELOX_LIVEMODE_STRICT=true go test -p 1 ./... -short -count=1

# Run tests verbose (for debugging)
test-verbose:
	VELOX_LIVEMODE_STRICT=true go test ./... -v -short -count=1

# Run linter. Velox code rules live in .golangci.yml (forbidigo: wall-clock
# time in clock-pinned packages, single-invoice period lookups) and in Go tests
# under internal/arch (civil-day dates need an explicit zone).
lint:
	golangci-lint run ./...

# Run benchmarks
bench:
	go test ./internal/domain/ -bench=. -benchmem -count=1

# Tidy dependencies
tidy:
	go mod tidy

# Migration management
migrate:
	go run ./cmd/velox migrate

migrate-status:
	go run ./cmd/velox migrate status

# Write the next migration file in internal/platform/migrate/sql/, numbered
# after the highest one in any worktree or on any branch. Rules and steps:
# docs/dev/migrations.md.
#   make migration name=add_invoice_purchase_order
#   make migration name=idx_invoices_purchase_order concurrent=1   (an index build)
migration:
	@scripts/new-migration.sh "$(name)" "$(concurrent)"

# Clean build artifacts
clean:
	rm -rf bin/

# Bootstrap: create demo tenant + API keys
bootstrap:
	DATABASE_URL="postgres://velox:velox@localhost:5432/velox?sslmode=disable" \
	go run ./cmd/velox-bootstrap

# Run demo script (requires: make up, make bootstrap, make dev)
demo:
	@echo "Run: ./scripts/demo.sh <YOUR_SECRET_KEY>"

# Integration tests (requires running postgres)
test-integration:
	go test -p 1 ./... -v -count=1 -short=false

# Code generation from the OpenAPI spec at api/openapi.yaml — see
# docs/dev/openapi-workflow.md. CI runs `make gen` followed by
# `git diff --exit-code` so any drift between the spec and the
# committed generated artifacts fails the build (same pattern Stripe
# uses internally with its openapi repo). Run locally after editing
# the spec; commit the regenerated files alongside the spec change.
gen: gen-go gen-ts

# Generate Go server interface + DTO types from api/openapi.yaml.
# oapi-codegen is pinned in tools/tools.go and resolved through
# go.mod, so the version is reproducible and bumps land like any
# other dep change.
gen-go:
	go run -tags tools github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen \
		--config api/cfg.yaml api/openapi.yaml

# Generate TypeScript types (openapi-typescript) into
# web-v2/src/lib/api.gen.ts and typed react-query hooks (orval) into
# web-v2/src/lib/gen/queries.gen.ts. Also refreshes
# web-v2/public/openapi.yaml — the SPA's predev/prebuild hook and the
# codegen pipeline (plus its CI drift check) read that copy of the
# canonical spec. (No embedded spec viewer exists — the /docs site was
# cut 2026-04-29.)
gen-ts:
	cd web-v2 && npm run gen

# Frontend
web-install:
	cd web-v2 && npm install

web-dev:
	cd web-v2 && npm run dev

web-build:
	cd web-v2 && npm run build

# Docker compose for local dev
up:
	docker compose up -d

down:
	docker compose down

# Project stats
stats:
	@echo "━━━ Velox Project Stats ━━━"
	@echo ""
	@printf "  Go files:     %s\n" "$$(find . -name '*.go' -not -path './vendor/*' | wc -l | tr -d ' ')"
	@printf "  Go lines:     %s\n" "$$(find . -name '*.go' -not -path './vendor/*' -exec cat {} + | wc -l | tr -d ' ')"
	@printf "  Test files:   %s\n" "$$(find . -name '*_test.go' | wc -l | tr -d ' ')"
	@printf "  SQL files:    %s\n" "$$(find . -name '*.sql' | wc -l | tr -d ' ')"
	@printf "  Packages:     %s\n" "$$(find ./internal -type d | wc -l | tr -d ' ')"
	@printf "  API routes:   %s\n" "$$(grep -rhoE '(^|[^A-Za-z0-9_])r\.(Get|Post|Put|Patch|Delete|Method|Handle)\(' internal --include='*.go' --exclude='*_test.go' | wc -l | tr -d ' ')"
	@printf "  TS/TSX files: %s\n" "$$(find ./web-v2/src -name '*.ts' -o -name '*.tsx' 2>/dev/null | wc -l | tr -d ' ')"
	@echo ""
	@printf "  Tests:        " && go test ./... -short -count=1 2>&1 | grep -c "^ok" && echo ""
	@echo "━━━━━━━━━━━━━━━━━━━━━━━━━━"
