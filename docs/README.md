# Velox documentation

**New here? Read the [README](../README.md), then run the
[quick start](quick-start.md).**

After that, pick what you want to do and read the docs in the order
listed. Words you may not know are in the [glossary](glossary.md), with
plain meanings first.

## Try it

1. [README](../README.md): what Velox is, who it is for, and a demo.
2. [Quick start](quick-start.md): start Velox on your computer with only
   Docker, run the demo, and see one month of token usage become an
   invoice, its PDF and a margin report. Each step says what you see.
3. [Is Velox right for you?](../README.md#is-velox-right-for-you): who it fits, and the cases where
   another tool is the better choice.
4. [What's in the box](features.md): every feature, with what it does for you.
5. [How Velox compares](comparison.md) with four open-source billing
   engines, with sources, and [why Velox exists](why-velox.md).
6. [Roadmap](roadmap.md): what shipped recently and what is deferred.

## Understand it

1. [How Velox works](how-velox-works.md): one picture and six steps, from
   a usage event to a paid invoice.
2. [Glossary](glossary.md): the words you will meet, plain meaning first.
3. [How much traffic Velox handles](scale.md): what was measured, and
   where one Postgres stops being enough.
4. [How Velox is engineered](ENGINEERING.md): the principles, one page,
   and [the evidence behind them](engineering-evidence.md).
5. The measured runs in full:
   [correctness under failure](benchmarks/failure-correctness.md) and
   [sustained throughput](benchmarks/sustained-throughput.md).

## Connect your product

1. [Send usage and get your first invoice](first-invoice.md): create a
   customer, send token usage with curl or LiteLLM, and open the invoice,
   in about ten minutes. Start here.
2. [AI pricing reference](ai-pricing.md): the models each recipe prices,
   pricing your own model, prepaid credit, and margin.
3. [API surface](api-surface.md): the core routes. The full reference is
   [`api/openapi.yaml`](../api/openapi.yaml).
4. [API keys](api-keys.md): get a key, send it, test and live mode, and
   rotation.
5. [Webhooks](webhooks.md): events, signatures, and retries.
6. [Stop usage when credit runs out](stop-usage-when-credit-runs-out.md):
   poll a prepaid customer's live credit balance and gate in your own code.
7. [LiteLLM integration](integrations/litellm.md): send token usage from a
   LiteLLM proxy without writing code.

## Run it for real

1. [Self-host](self-host.md): what you need and how the pieces fit.
2. [Docker Compose on one VM](../deploy/compose/README.md): step-by-step
   install.
3. [Production on one VM](production-one-vm.md): HTTPS, email, Stripe,
   nightly backups, a go-live checklist and upgrades, for a small team.
4. [High availability](ops/high-availability.md): run two or more copies
   behind a load balancer, with health checks, shutdown and rolling upgrades.
5. [Postgres requirements](ops/postgres-requirements.md),
   [email setup](ops/email-setup.md),
   [backups](ops/backup-considerations.md) and
   [tax calculation](ops/tax-calculation.md).
6. When something breaks: the [operations runbook](ops/runbook/index.md)
   (find what you see, then follow its steps) and the
   [failure-mode catalog](failure-modes/README.md).

## Contribute

1. [CONTRIBUTING](../CONTRIBUTING.md): setup, tests, and what CI checks.
2. [Architecture](architecture.md): package layout and design rules.
3. [Money-path robustness playbook](dev/money-path-robustness-playbook.md):
   required for any change to invoices, payments, credits, dunning,
   subscriptions or tax.
4. [How we test](testing.md): which test to write, and how to run it.
   The [release checklist](dev/release-checklist.md) holds the live checks
   run by hand before each release, and the
   [Stripe end-to-end test](dev/stripe-end-to-end-test.md) has the
   step-by-step Stripe commands.
5. [OpenAPI workflow](dev/openapi-workflow.md): changing the API contract.
6. [Writing a database migration](dev/migrations.md): changing the
   database schema.
7. [Writing style](dev/writing-style.md): how to write docs here.
8. AI coding agents start at [CLAUDE.md](../CLAUDE.md).

## Archive

Background for a decision. You do not need these to use or run Velox.

- [Architecture decision records (ADRs)](adr/README.md): one decision per
  file, with the reasons and the alternatives.
- [Design docs](design/README.md): the plan for each feature, written
  before it was built. Where a design doc and the code disagree, the code
  and the ADRs are the current record.
- [Records](records/README.md): dated audits, reviews and plans, kept as
  written.
- [CHANGELOG](../CHANGELOG.md): every change, by date.
