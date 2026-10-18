# Browser tests (Playwright)

These tests open the dashboard in Chromium against a real Velox API and real
data. They check what only a browser can see: that pages load cleanly and fit
the screen, and that the main things an operator does work end to end.

- `smoke.spec.ts` opens every page at 1440px and 390px wide. A page fails if
  the browser logs a console error, an API call answers 400 or more, the page
  scrolls sideways, or its heading is missing. Each page is its own test, so a
  failure names the page.
- `journeys.spec.ts` signs in, creates a customer, creates a subscription,
  advances a test clock to the renewal and opens the new invoice, opens the
  public invoice page, and grants credits. Each journey checks that the screen
  shows what the API returned. The arithmetic behind the numbers is tested in
  Go, not here.
- `subscriptions.spec.ts` changes subscriptions on screen: a plan change now
  (with its proration invoice) and at renewal, cancel at period end, undo and
  cancel now, quantity and item changes against the upcoming invoice, and the
  trial and collection controls. Each journey checks the page, the toast and
  any invoice the change made against the API.
- `global.setup.ts` runs first. It seeds data through the API (code in
  `fixtures/`) and signs in once through the login page. The other tests reuse
  that session.

## Run them locally

You need a Velox API with a bootstrapped tenant, and the dashboard built and
served by `vite preview`, which forwards `/v1` to the API. Use your own
database so the tests never touch your dev data.

```bash
export PGPASSWORD=velox
# 1. A database of your own (the docker compose Postgres on :5432). The
#    velox_app role the API runs as is created by docker/init.sql.
psql -h localhost -U velox -d postgres -c 'CREATE DATABASE velox_e2e OWNER velox'
psql -h localhost -U velox -d velox_e2e -c 'GRANT ALL ON SCHEMA public TO velox_app'
psql -h localhost -U velox -d velox_e2e -c 'ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON TABLES TO velox_app'
psql -h localhost -U velox -d velox_e2e -c 'ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON SEQUENCES TO velox_app'
export DATABASE_URL='postgres://velox:velox@localhost:5432/velox_e2e?sslmode=disable'

# 2. Bootstrap a tenant (prints the vlx_secret_test_ key) and start the API
VELOX_BOOTSTRAP_EMAIL=e2e@example.com VELOX_BOOTSTRAP_PASSWORD=e2e-password \
  go run ./cmd/velox-bootstrap
PORT=8190 go run ./cmd/velox

# 3. In web-v2/: build, serve, and run the tests
npm run build
VELOX_API_TARGET=http://127.0.0.1:8190 npx vite preview --host 127.0.0.1 --port 4190
npx playwright install chromium   # once
E2E_BASE_URL=http://127.0.0.1:4190 E2E_API_URL=http://127.0.0.1:8190 \
E2E_API_KEY=vlx_secret_test_... E2E_EMAIL=e2e@example.com E2E_PASSWORD=e2e-password \
  npm run test:e2e
```

Rebuild (`npm run build`) after you change the dashboard: `vite preview`
serves the built files.

Useful flags: `npx playwright test smoke -g invoices` runs one page,
`--ui` opens the test runner, and `npx playwright show-trace <trace.zip>`
opens a trace from a CI failure (CI uploads them as the `playwright-report`
artifact).

## Rules

- Find elements the way a person does: `getByRole`, `getByLabel`. If a control
  has no accessible name, give it one in the app.
- Never wait a fixed time. Wait for something on screen or for the API.
- Each test makes its own data with unique names, so tests can run in any
  order, in parallel, and many times against the same database.
- Before you commit a new or changed test, run it five times:
  `npx playwright test --repeat-each 5`. Fix or delete a test that fails even
  once. CI retries a failed test once and reports a pass on retry as flaky.
- A new page in `src/main.tsx` gets a line in the `routes` list in
  `smoke.spec.ts`.
