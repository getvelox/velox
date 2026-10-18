# Contributing to Velox

Velox is a billing system you run yourself, written in Go on PostgreSQL. It
counts what your customers use, works out each bill, makes the invoice, and
charges the card through Stripe.

This page takes you from a fresh clone to a pull request that passes CI. It
shows how to run Velox from source, where the code goes, and which checks CI
runs on your change.

## Development setup

You need Go 1.26 or newer and Docker. Node 22 or newer is needed only for
dashboard work, and `jq` only for `scripts/demo.sh`.

```bash
git clone https://github.com/getvelox/velox.git
cd velox
cp .env.example .env
```

1. **Start Postgres.**

   ```bash
   docker compose up -d postgres
   ```

   **You see:** `docker compose ps` lists the `postgres` service as
   `healthy` after a few seconds.

2. **Create your first account and its keys.** This also runs the database
   migrations.

   ```bash
   make bootstrap
   ```

   **You see:** a "Velox Bootstrap Complete" box with the dashboard sign-in
   (email `admin@velox.local` unless you set `VELOX_BOOTSTRAP_EMAIL`), a
   generated password, and three API keys. Save the password: it is shown
   only once. Copy the key under "Secret Key — TEST mode"
   (`vlx_secret_test_…`).

3. **Start the API.** `make dev` reads `.env`, which points at the local
   Postgres and runs migrations on boot.

   ```bash
   make dev
   ```

   **You see:** a JSON log line with `"msg":"listening"` and
   `"addr":":8080"`. `curl http://localhost:8080/health` returns
   `{"status":"ok"}`.

4. **Start the dashboard**, in a second terminal.

   ```bash
   cd web-v2 && npm install && npm run dev
   ```

   **You see:** Vite prints `http://localhost:5173/`. Open
   `http://localhost:5173/login` and sign in with the email and password
   from step 2. The dashboard opens in test mode, where nothing charges real
   money.

5. **Bill a test customer end to end**, in a third terminal. This is the
   golden path: the one flow CI also runs on every PR, from loading prices
   to a finished invoice.

   ```bash
   ./scripts/demo.sh vlx_secret_test_...   # the key from step 2
   ```

   **You see:** nine numbered steps, each with a green `✓` line, then
   "Done: every step above ran and was checked" and two dashboard links,
   one to the invoice and one to the customer.

Run the tests with Postgres still up:

```bash
go test ./... -race -short -count=1         # unit tests (CI runs -race too)
go test -p 1 ./... -count=1 -short=false    # unit + integration tests
```

To try a release without Go or Node, use `deploy/compose/quickstart.sh`
instead (see the [README](README.md)).

### Without make

These are the same steps without make. `make dev` reads `.env`, which sets
`DATABASE_URL` and `RUN_MIGRATIONS_ON_BOOT=true`. Here you set them on the
command line:

```bash
docker compose up -d postgres
DATABASE_URL="postgres://velox:velox@localhost:5432/velox?sslmode=disable" go run ./cmd/velox-bootstrap
DATABASE_URL="postgres://velox:velox@localhost:5432/velox?sslmode=disable" RUN_MIGRATIONS_ON_BOOT=true go run ./cmd/velox
```

To run only the migrations:

```bash
DATABASE_URL="postgres://velox:velox@localhost:5432/velox?sslmode=disable" go run ./cmd/velox migrate
```

### Run your change in the Compose stack

This builds the API image from your checkout and runs it in the same stack
as a [one-VM install](deploy/compose/README.md):

```bash
docker build -t velox:local .        # from the repo root
cd deploy/compose
cp .env.example .env
$EDITOR .env   # set POSTGRES_PASSWORD, VELOX_APP_DB_PASSWORD, VELOX_ENCRYPTION_KEY, VELOX_BOOTSTRAP_TOKEN
VELOX_IMAGE=velox:local docker compose up -d
```

**You see:** `docker compose logs -f velox-api` prints a log line with
`"msg":"listening"` and `"addr":":8080"` when the API is ready.

To build the image and run it on its own, against a Postgres you already have:

```bash
docker build -t velox:latest .
docker run --rm -e DATABASE_URL="..." -p 8080:8080 velox:latest
```

## Project structure

Each billing domain (invoice, credit, customer, and so on) is a
self-contained package under `internal/`. [Architecture](docs/architecture.md)
shows how the packages work together.

```
internal/{domain}/
  store.go       — Store interface (what the domain needs from persistence)
  postgres.go    — PostgreSQL implementation of the store
  service.go     — Business logic (validates, orchestrates)
  handler.go     — HTTP handlers (decodes request, calls service, writes response)
  *_test.go      — Unit tests (in-memory store) + integration tests (real Postgres)
```

**Rules:**

- One domain package never calls another domain's `Service` or `Store`
  directly. A package may import shared value types, cross-cutting
  infrastructure (`auth`, `audit`, `session`), and the small interfaces of
  the `billing` coordinator. The allowlist in
  `internal/arch/boundaries_test.go` enforces this: a new cross-domain
  import fails the test until it is added there on purpose.
- Every handler uses `respond.JSON()` / `respond.FromError()` for responses.
- Every store method runs inside a transaction scoped to one tenant by
  Postgres Row-Level Security (RLS). A tenant is one account's data in a
  shared database; RLS hides every other tenant's rows.
- Integration tests use `testutil.SetupTestDB()`.

## Adding a new domain

1. Create `internal/{domain}/store.go` with the store interface.
2. Create `internal/{domain}/postgres.go` implementing the store.
3. Create `internal/{domain}/service.go` with business logic.
4. Create `internal/{domain}/handler.go` with HTTP handlers using the
   `respond` package.
5. Add a migration with `make migration name=...`, then follow
   [Writing a database migration](docs/dev/migrations.md).
6. Wire it into `internal/api/router.go`.
7. Write tests at the layers [How we test](docs/testing.md#which-test-to-write)
   names for your change.

## Code style

- `go fmt` and `go vet` must pass.
- No exported types without doc comments.
- Errors use `errs.DomainError` for domain-specific errors or `fmt.Errorf`
  for validation.
- JSON field names are `snake_case`.
- ID format: `vlx_{type}_{random_hex}` (for example `vlx_cus_abc123`).

## Frontend (web-v2)

The dashboard is a React + TypeScript + Vite app. See
[`web-v2/README.md`](web-v2/README.md) for setup, the dev server, and the
directory's own CI checks. Two things often surprise first-time contributors:

- `npm install` relies on `web-v2/.npmrc`. That file tolerates a
  peer-dependency mismatch on purpose, so do not "fix" the mismatch.
- Typechecking is `npx tsc -b`. Do not use `tsc --noEmit`: here it checks
  nothing and reports no error.

## What CI runs on your PR

CI runs every check below on every PR. Run the ones your change touches
locally before you push. Fork PRs get the same run, because no check uses
secrets.

**Required to merge.** A PR cannot merge until each of these passes.

| CI job | What it checks | Local command |
|---|---|---|
| `unit` | Unit tests with the race detector, then `go vet`. Includes the source-rule tests in `internal/arch` | `go test ./... -race -short -count=1` |
| `test` | Build, module checksums, and the integration tests against a real Postgres | `go test -p 1 ./... -count=1 -short=false` (needs `docker compose up -d postgres`; test databases are created for you) |
| `test-pgbouncer` | The same suite through PgBouncer, a connection pooler, in transaction mode | `scripts/pgbouncer-test.sh` (needs Docker) |
| `golden-path` | Boots a real server, runs `scripts/demo.sh` from prices to a finished invoice, then runs velox-doctor over the rows it wrote | Steps 2 to 5 of [Development setup](#development-setup), then `go run ./cmd/velox-doctor` |
| `lint` | golangci-lint (`.golangci.yml`), the Go version in `go.mod` against the `Dockerfile`, the quickstart release against the Compose template, and `govulncheck` | `make lint`, then `govulncheck ./...` |
| `codegen-drift` | Generated Go and TypeScript match `api/openapi.yaml` | `make gen && git diff --exit-code` |
| `frontend` | Dashboard build (`tsc -b` and Vite), unit tests, eslint, and knip (unused files, exports and dependencies) | `cd web-v2 && npm run build && npm test && npm run lint` |

**Also run, not required to merge.**

| CI job | What it checks | Local command |
|---|---|---|
| `e2e` | Playwright tests against a real API and dashboard. Setup: [`web-v2/e2e/README.md`](web-v2/e2e/README.md) | `cd web-v2 && npm run test:e2e` |
| `doc-links` | Every internal Markdown link and `#anchor` resolves. It turns red on a broken link; fix it before merging | `lychee --offline --include-fragments '**/*.md'` |
| `doc-style` | Vale prose lint on the lines your PR adds. It comments only and never fails | `vale <changed .md files>` |

Notes on three of them:

- **velox-doctor** is a read-only command that scans a database for states
  no correct code could produce, such as invoice totals that do not add up.
  Point `DATABASE_URL` at the owner connection: under the app's RLS role it
  sees no rows and reports clean.
- **`govulncheck`** can fail when a new Go vulnerability is published, even
  if your change is fine. Say so in the PR and we will handle it.
- **Lint waivers and source rules** (the `//nolint:forbidigo` reasons and
  the `//tz:ok` date rule) are described in
  [How we test](docs/testing.md#which-test-to-write).

## Pull requests

1. Fork the repo and create a feature branch.
2. Write tests for new behavior, at the layer
   [How we test](docs/testing.md) names for your change.
3. Run the checks above that your change touches (at least the two test
   commands).
4. Update the docs in the same PR. Every user-visible change gets a
   `CHANGELOG.md` entry, and every UI change is covered by a Playwright
   spec in `web-v2/e2e/`. A doc that no longer matches the code is a bug.
   For how to write docs, see the
   [writing style guide](docs/dev/writing-style.md).
5. Open the PR with a clear description of what changed and why.

**Touching money or a state machine?** This covers invoices, payments,
credits, dunning (retrying a failed card payment and emailing the
customer), subscriptions and tax. Follow the
[Money-path robustness playbook](docs/dev/money-path-robustness-playbook.md):

- Before opening the PR, work through its implementation checklist
  ([§3](docs/dev/money-path-robustness-playbook.md#3-implementation-checklist--gates-before-opening-a-money-path-pr)).
- In the PR description, list the state's site-set
  ([§2](docs/dev/money-path-robustness-playbook.md#2-the-meta-practice-complete-site-set-enumeration)):
  every place in the code that writes that state or reacts to it.
- A guard against a race or a broken money rule needs an automated test.
  The test forces the race, and you show it fails once the guard is
  removed ([§5](docs/dev/money-path-robustness-playbook.md#5-test-lock-doctrine)).

Words used here that you have not met are in the
[glossary](docs/glossary.md#for-contributors).
