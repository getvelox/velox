# How we test Velox

Every change ships with a test at the right layer. This page says which test
to write for a change, how to run each kind, and what is checked by hand
before a release.

## The four layers

| Layer | Tools | What belongs here |
|---|---|---|
| 1. Logic and money | `go test`: unit tests (`-short`) and integration tests against a real Postgres (`internal/testutil`) | All money math, state changes, Row-Level Security (RLS), migrations, API contracts and architecture rules (the source tests in `internal/arch`) |
| 2. Static checks | golangci-lint, eslint, `tsc -b`, codegen drift (`make gen`) | Code rules. A rule we can check by reading the code becomes a linter rule or a Go test, not a script |
| 3. Dashboard end to end | Playwright (`@playwright/test`) | A smoke test over every page, plus a few key user journeys, against a real API with seeded data |
| 4. Live services | [Release checklist](dev/release-checklist.md), run by hand | Real Stripe test mode, real email, the LiteLLM callback and the invoice PDF. Run before each release tag |

Most tests belong in layer 1. Layers 3 and 4 stay small on purpose.

## Which test to write

| You changed | Write |
|---|---|
| Money, an invoice, payment, credit, dunning, subscription or tax state | A Go integration test against real Postgres. Follow the [money-path playbook](dev/money-path-robustness-playbook.md) §5: test the collision, not only the happy path |
| Pure logic with no database (a calculation, a parser, a formatter) | A Go unit test, or a Vitest test in `web-v2/tests/` for dashboard code |
| An API endpoint | A Go test that calls the handler and checks the status code and body |
| A rule about how code may be written | A golangci-lint rule in `.golangci.yml`, or a source test in `internal/arch` |
| A dashboard page or a user journey | A Playwright spec in `web-v2/e2e/`. New pages are added to the smoke list |
| Stripe, email or PDF behavior that a fake cannot prove | A line in the [release checklist](dev/release-checklist.md), plus a Go test for the logic around it |

A Playwright test checks that the screen shows what the API returned.
Arithmetic stays in Go tests.

A lint waiver names the linter and says why, on the same line. A bare
`//nolint` fails the lint. For example:

| Waiver | The rule it waives |
|---|---|
| `//nolint:forbidigo // wall-clock: <reason>` | No bare `time.Now()` in packages whose time can come from a test clock |
| `//nolint:forbidigo // single-invoice-ok: <reason>` | No lookup of one invoice for a billing period in `internal/subscription` and `internal/billing`, because several invoices can pay for one period |

One source test in `internal/arch` checks dates: a calendar date must be
formatted in an explicit time zone (`.In(loc)`), or carry a same-line
`//tz:ok` comment that marks a real UTC case.

## Run each layer locally

Start Postgres once: `docker compose up -d postgres`.

```bash
# Layer 1: Go
go test ./... -short                 # unit tests, no database
go test -p 1 ./... -short=false      # unit + integration; test databases are created for you

# Layer 2: static checks
make lint                            # golangci-lint
make gen && git diff --exit-code     # generated code matches api/openapi.yaml
cd web-v2 && npx tsc -b && npm run lint   # eslint, then knip (unused code)

# Dashboard unit tests (Vitest, web-v2/tests/*.test.ts)
cd web-v2 && npm test

# Layer 3: Playwright (needs a running API and dashboard; see web-v2/e2e/README.md)
cd web-v2 && npm run test:e2e
```

Use `npx tsc -b` for type checks. `tsc --noEmit` checks nothing in this repo.

[CONTRIBUTING](../CONTRIBUTING.md#what-ci-runs-on-your-pr) lists every CI
gate and its local command.

## Playwright: how the end-to-end tests work

The specs live in `web-v2/e2e/` and run in Chromium against a real API and
the built dashboard (`vite preview`, which forwards `/v1` to the API).

- **Smoke:** `smoke.spec.ts` opens every dashboard page at 1440px and 390px
  wide. A page fails if it logs a console error, an API call answers 400 or
  more, the page scrolls sideways, or its heading is missing. Each page is
  its own test, so a failure names the page.
- **Journeys:** `journeys.spec.ts` signs in, creates a customer and a
  subscription, advances a test clock to the renewal and opens the new
  invoice, opens the public invoice page, and grants credits.
  `subscriptions.spec.ts` changes a subscription's plan, items, trial and
  collection, and cancels it, checking each change against the API.
- **Data:** the tests make their data through the public API, with the
  test-mode secret key from `velox-bootstrap`. Each test uses its own unique
  names, so the suite can run many times against the same database. Use a
  database of your own anyway, so the tests never touch your dev data.
- **In CI:** the `e2e` job runs on every PR. It is not a required check yet.
  On a failure, download the `playwright-report` artifact and open its trace
  with `npx playwright show-trace`.

How to run them locally, step by step: [web-v2/e2e/README.md](../web-v2/e2e/README.md).

### Flaky tests

A flaky test passes and fails on the same code.

- CI retries a failed test once. A test that passes on the retry is
  reported as flaky, not as passed.
- Before you commit a spec, run it five times:
  `npx playwright test --repeat-each 5 <spec>`.
- Wait on the page or the network, never on a fixed sleep.
- Select by role and label (`getByRole`, `getByLabel`), not by CSS class.
- Fix or delete a flaky spec. Do not mark it as skipped and move on.

## Checking a change by hand

Automated tests come first. When you also check something by hand, three
habits have caught real bugs here:

- **Look at the screen.** Text and DOM checks cannot see layout bugs. A UI
  change is not done until you have looked at a screenshot of it, in the
  state a user would see.
- **Check money at the provider.** For a payment or refund, confirm the
  amount in the Stripe dashboard, not only in Velox.
- **For a fix, see the test fail without it.** Revert the fix, run the new
  test, watch it fail, then restore the fix. A test that passes either way
  proves nothing.

## Before a release

Run the [release checklist](dev/release-checklist.md). It covers what only a
real provider can show: a Stripe charge, refund and webhook, an email you
open and read, the LiteLLM callback, and the invoice PDF.

The golden-path demo (`scripts/demo.sh`) and the `velox-doctor` sweep also
run in CI on every PR. The doctor checks the money invariants over the rows
the golden path left behind.

## Old flow IDs

Until October 2026, hand-run test steps lived in `MANUAL_TEST.md`, with flow
IDs such as `FLOW TC4` or `FLOW B10`. Some code comments and ADRs still name
them. To read the last version of the file:

```bash
git show "$(git log -1 --format=%H --diff-filter=D -- MANUAL_TEST.md)^:MANUAL_TEST.md" | less
```
