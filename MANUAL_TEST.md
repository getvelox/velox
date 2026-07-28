# Velox Manual Test Runbook

Practical runbook for exercising Velox end-to-end. Three tiers — pick the
subset that matches the change you made.

| Tier | When | Time |
|------|------|------|
| **1 — Smoke** | Pre-merge / nightly | ~15 min |
| **2 — Full** | Pre-release | ~2 hrs |
| **3 — Deep** | Major releases, infra changes, post-mortems | ~3 hrs |

Each flow has a stable ID (A1, B2, …) for cross-referencing. Steps use
`- [ ]`; copy a section into a scratch doc when running it. This file is
the canonical source, not a progress tracker.

## Flow index

<details>
<summary><b>All flows, grouped by section — click to expand</b></summary>

**Tier 1 — Smoke**  
`S1` End-to-end smoke · `S2` AI-native end-to-end smoke

**Tenant timezone**  
`TZ1` Tenant timezone semantics

**Authentication & API keys**  
`A1` Sign-in · `A2` /v1/whoami · `A3` Test/Live mode toggle · `K1` API key permissions · `K2` Expiration · `K3` API Keys page UX · `K4` Rotate

**Test Clocks**  
`TC1` Test Clocks page · `TC2` Detail + Advance · `TC3` Pinning · `TC4` Catchup correctness · `TC5` Dunning via catchup (clock-pinned failure recovery) · `TC6` Trial expiration via catchup · `TC7` Plan change at period boundary via catchup · `TC8` Subscription cancellation at period end (via catchup) · `TC8b` Mid-period cancel of an UNPAID in-advance prebill · `SUB-CARD` Subscription billing-cycle card surface · `TIMELINE-ORDER` Activity timeline ordering (invoice + subscription) · `SUB-REALIGN` Calendar-billing subs auto-realign anchor at cycle close · `TC9` Pause collection auto-resume (via catchup) · `TC10` Credit grant expiry firing (via catchup) · `E` Email delivery (SMTP) · `EX` Streaming CSV exports

**Billing Engine**  
`B1` Arrears + proration (default `in_arrears` plans) · `B2` Tax precision (NUMERIC(7,4), ADR-042/043) · `B2b` Per-unit rate precision (decimal, ADR-045) · `B3` Idempotency · `B4` Auto-charge retry · `B5` Idempotency-Key header · `B6` Subscription lifecycle · `B7` Plan change + proration · `B8` Usage caps · `B9` Customer price overrides · `B10` Manual tax + customer tax status · `B11` Tax-ID validation · `B12` Subscription activity timeline · `B13` Multi-dimensional meters · `B14` Billing thresholds · `B15` `in_advance` plan happy path · `B16` Hybrid `in_advance` base + `in_arrears` usage on one invoice · `B16b` token usage billed on immediate cancel · `B17` `in_advance` cancel proration credit · `B17b` upgrade then cancel — credit fans across both funding invoices · `B17c` downgrade after upgrade — clawback reverses the upgrade invoice (LIFO) · `B18` Meter Detail page · `B19` Cancel-flow billing artifacts · `B20` Segment-aware billing at cycle close (Lago / Orb shape) · `B21` Immediate same-cadence cross-interval plan-swap (yearly ↔ monthly)

**Pricing Recipes**  
`R1` List + preview · `R2` Instantiate · `R3` Idempotent re-apply, no uninstall · `R4` Atomic rollback · `R5` Dashboard UI

**Invoices**  
`I1` Multiple meters · `I2` Negative usage · `I3` Manual line items · `I4` Void · `I4b` Uncollectible invoice lifecycle · `I5` Collect + payment timeline · `I5b` Invoice attention banner · `I6` Email + PDF preview · `I7` Zero-amount invoice · `I8` Currency consistency · `SUB7` Mid-period change outcome on the timeline + invoice · `I9` Credit note on void · `I9b` Credit note PDF totals reconcile · `I10` Hosted invoice page · `I11` `create_preview` · `I12` One-off invoice composer · `I13` Timeline completeness · `TR-CXL` Trial cancellation · `C-ARCH` Archive semantics

**Dunning**  
`D1` Retry cycle + escalation · `D2` Resolution · `D4` Self-service payment update · `D5` Dunning policy admin (CRUD + assignment + terminal actions)

**Credits & Credit Notes**  
`C1` Credits lifecycle · `C2` Credit notes · `C4` Prepaid commits · `C2b` Credits ledger readability · `C3` Credit-note refund handling

**Webhooks**  
`W0` Outbound endpoint config · `W1` Stripe signature verification · `W2` Outbound secret rotation (72h grace) · `W3` Delivery stats · `W4` Live event stream + replay

**Customers**  
`CU1` Settings + billing profile · `CU2` Operator customer-portal API · `CU4` Archive cascade · `CU6` Brand color + logo URL · `CU9` Sent emails on customer page · `CU8` Cost-dashboard public projection

**Platform**  
`P2` Audit log · `P2A` Audit log — customer-initiated + Tier 2 coverage · `P2B` Operator-side payment-method management · `P3` Usage summary · `P4` Empty billing cycle · `P5` Health checks · `P6` Tax deferral metrics · `REC1` Self-healing background reconcilers

**UI / UX**  
`U1` Dashboard · `U3` Usage Events page · `U11` Operator search + list filters · `U12` Dashboard consistency sweep · `U7` Edge cases · `U8` Request-ID in error toasts · `U10` Public pages

**Tier 3 — Deep / Rare**  
`X1` RLS multi-tenant isolation · `X2` Bootstrap lockdown · `X2b` Self-host bootstrap → login → live key · `T1` Team invites · `E1` Additional billing emails + credit-note send · `X3` Rate limiting · `X4` Security headers + metrics auth · `X5` PII encryption at rest · `X7` Stripe Tax · `X8` Migration rollback · `X9` Config validation · `X10` OpenTelemetry tracing · `X11` Large batch usage ingestion · `X14` Self-host (Compose) · `CR1` Paid-commit relief · `CG1` Commit / credit-grant burndown · `M1` Provider cost tables + margin · `X15` LiteLLM integration

</details>

## Conventions

Keep this runbook runnable and rot-free:

- **One observable per checkbox.** A `- [ ]` is a single pass/fail — if you catch yourself writing "and then… and also…", split it.
- **Lead with a bold imperative title**, then the concise observable — e.g. `- [ ] **Void hands back applied credit** — the customer balance increases by the applied amount`.
- **File a checkbox under the flow that owns the feature** — never "the end of the longest flow." A genuinely new feature gets a **new flow** under the right section (next free number in that section's prefix). IDs are stable for cross-referencing — don't renumber existing ones.
- **Provenance is a terse trailing tag, not the step.** `(ADR-057)` is fine; dates, migration numbers, PR links, and "pre-fix this was…" history belong in CHANGELOG / ADRs / git, not in the test instruction.
- **Assert what's observable** (UI, API response, email). Reach for `psql` only when the DB row *is* the pass/fail.
- **Update the Flow index above** when you add or rename a flow.
- **A flow that lies is worse than no flow.** When shipped behavior changes, rewrite or delete the affected flow **in the shipping PR** — never leave-and-annotate. (This is the rot trigger this doc already paid for once.)
- **Editing a flow resets its manual attestation.** A `[~]` tag attests the OLD behavior — when you change a flow's assertions, downgrade it to `[ ]` (automated `[x]` stays only if the cited test still covers the new assertion).
- **New flows land in Tier 2 under their owning section** by default; Tier 3 is for rare/destructive/infra scenarios (RLS, rollback, self-host), Tier 1 only for the two smoke paths.
- **`[x]` means "locked by a durable automated test"** — annotate which (e.g. `auto: \`TestXxx\``). It is NOT a record of a one-time manual run (those are transient and go stale next release — keep those in a scratch doc). Leave `[ ]` for manual-only or not-yet-automated items. (Full marker set + routing in **Testing strategy** below.)

## Testing strategy

How a flow gets verified, and how its status is recorded. Pre-launch posture: **guard the money invariants, don't gold-plate.**

**Status markers** (per checkbox):

- `[x]` — locked by a **durable automated test** (CI). Tag `auto: TestX`.
- `[~]` — **manually verified once** on a real build (real API + DB). Tag `manual: YYYY-MM-DD`. NOT regression-guarded — re-run before any release that touches the flow.
- `[ ]` — pending.

A flow MAY carry an "Automated coverage: N / M" tally where it aids triage (keep it correct or drop it).

**Routing — how to verify a flow, in order:**

1. **Check existing CI coverage first.** If a test already locks it → `[x]`, done. Never re-test what's already guarded.
2. **Route by what the flow *is*:**
   - **Concurrency / races** → MUST be automated (can't be done by hand).
   - **Idempotency / atomicity / DB-correctness money invariant** → automated (the durable guard) — *unless* it's the Nth instance of a pattern already CI-proven, then `[~]` manual or skip.
   - **Observable / UI / live-external** (real Stripe, webhook, email, PDF) → `[~]` manual live verification — an automated test would only mock the externals that matter.
3. **Pragmatism cap.** Don't automate a near-duplicate of an already-proven pattern. Past the core money invariants, product work outranks more internal-coverage tests at this stage.

**Every flow gets a bug-dig.** While verifying, read the real e2e code path adversarially (error handling, race windows, partial-failure recovery, missing guards) and report findings *separately* from "the test passes." A meaningful uncovered path → add it to the doc + cover it; a low-probability gap → note and defer. For money/state-machine flows, run the bug-dig against the **[Money-Path Robustness Playbook](docs/dev/money-path-robustness-playbook.md)** review lens (§4) — the 9 failure classes and the per-class questions that independently re-derive each invariant.

**Craft:**

- *Automated:* real Postgres test DB for DB-level invariants (real indexes / tx / FK); mock only the failing seam + externals, keep the invariant's real path real; assert the specific error code / real DB state, not just "errored"; `-race` for concurrency; `-short` skip guard.
- *Manual (`[~]`): test it end-to-end as a real operator would, as realistically as possible.* Drive the **actual surfaces** a real user touches (the dashboard UI, or the real API a customer/integrator calls — not an isolated internal poke), against **real external systems** (Stripe test mode, webhooks via `stripe listen`, emails in Mailpit), and verify the outcome **where the operator would actually see it** (the invoice, the badge, the email, the Stripe dashboard, the webhook stream). Walk the whole journey, not one step. Record what was checked + the date in the `[~]` tag.

---

## Setup

### Prerequisites

- Go 1.25+, Docker & Compose, Node.js 22+, [Stripe CLI](https://stripe.com/docs/stripe-cli)
- A Stripe test account (keys go in the dashboard per-tenant, not env vars)

### First-time config

```bash
cp .env.example .env
# Required for local dev:
#   VELOX_BOOTSTRAP_TOKEN=<openssl rand -hex 32>
#   VELOX_ENCRYPTION_KEY=<openssl rand -hex 32>   (64 hex chars)
```

### Start the stack (4 terminals)

```bash
make up                      # 1: Postgres + Redis + Mailpit
make dev                     # 2: API on :8080 (auto-migrates)
cd web-v2 && npm run dev     # 3: Dashboard on :5173
stripe listen --forward-to localhost:8080/v1/webhooks/stripe/<vlx_spc_id>   # 4: Stripe webhooks (after Settings → Payments → Connect)
```

`make bootstrap` (first run only) prints operator email + password and the
test/live API keys. Mailpit web UI at http://localhost:8025 captures all
outbound mail.

### Shell setup

```bash
export API=http://localhost:8080
export KEY=vlx_secret_test_…
```

JSON bodies referencing shell vars must use double-quoted JSON with
backslash-escaped quotes: `-d "{\"api_key\":\"$KEY\"}"`. Single-quoted
JSON works when no shell var is referenced.

### Test cards

| Card | Behavior |
|------|----------|
| `4242 4242 4242 4242` | Always succeeds |
| `4000 0000 0000 0341` | Attaches OK, declines on charge (dunning trigger) |
| `4000 0000 0000 9995` | Always declines |

---

# Tier 1 — Smoke (~15 min)

## FLOW S1: End-to-end smoke

Brings the stack up, runs the full money path, signs out. Pre-merge canary.

### S1.1 Stack health
- [ ] `make up` — containers start clean.
- [ ] `make dev` — logs show `migrations: applied N` + `using app database connection (RLS enforced)`.
- [ ] `curl localhost:8080/health` → `{"status":"ok"}`.
- [ ] `curl localhost:8080/health/ready` → 200 with `database`, `scheduler` ok.
- [ ] Frontend at http://localhost:5173 loads.

### S1.2 Bootstrap + sign in
- [ ] `make bootstrap` (if no tenants) prints test/live secret keys + publishable test key. Copy the secret test key.
- [ ] Sign in at `/login`. Redirect to dashboard.
- [ ] Cookie `velox_session` set, `HttpOnly: ✓`. No API key in localStorage.

### S1.3 Stripe connection
- [ ] Settings → Payments → paste `sk_test_...` + `pk_test_...` → Connect. `vlx_spc_...` shown.
- [ ] Terminal 4: `stripe listen --forward-to localhost:8080/v1/webhooks/stripe/<vlx_spc_...>`. Paste the `whsec_...` back.
- [ ] Settings shows "Connected".

### S1.4 Build the graph
- [ ] Pricing → rating rule `api_calls` flat $0.01. Meter `api_calls` sum, **link the rule to the meter** (bind — key-match alone does NOT bind). Plan `starter` $29/mo, attach meter. **Guard (ADR-096):** attaching a meter with NO rating rule is rejected at plan create/update — `422 meter "…" has no rating rule` — so an unpriced meter can't silently bill $0. Bind the rule first, then attach.
- [ ] Customers → create "Smoke Corp", external_id `smoke_corp`, email any@any.test. Billing profile: address + USD + 10% tax.
- [ ] Customer detail → Set Up Payment → `4242 4242 4242 4242`.
- [ ] Mint a test clock (avoids 30-day wait):
  ```bash
  curl -sS -X POST "$API/v1/test-clocks" -H "Authorization: Bearer $KEY" \
    -H "Content-Type: application/json" \
    -d "{\"name\":\"smoke\",\"frozen_time\":\"$(date -u +%FT%TZ)\"}" | jq .
  ```
- [ ] Create the customer pinned to the clock (ADR-027 customer-level attach): Customers → New Customer → tick **Pin to test clock** dropdown → select your clock → Create. Customer Detail header shows the test-clock badge.
- [ ] Customer detail → New Subscription → Starter plan. No clock dropdown — the dialog shows an amber inheritance hint ("This subscription will inherit the customer's test clock — &lt;name&gt;") because the customer is pinned. Server inherits automatically.

### S1.5 Bill + charge
- [ ] Ingest 1,000 events:
  ```bash
  TS=$(date -u +%FT%TZ)
  jq -n --arg ts "$TS" '[range(1000) | {external_customer_id:"smoke_corp",event_name:"api_calls",quantity:"1",idempotency_key:"smoke_\($ts)_\(.)"}]' > /tmp/events.json
  curl -sS -X POST "$API/v1/usage-events/batch" -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" --data-binary @/tmp/events.json | jq .
  ```
- [ ] Advance the clock 31 days: `POST /v1/test-clocks/$CLK/advance` with `frozen_time = now+31d` (BSD `date -u -v+31d` / GNU `date -u -d '+31 days'`).
- [ ] `curl -sS -X POST "$API/v1/billing/run" -H "Authorization: Bearer $KEY"` → 1 invoice generated (bills only THIS tenant's due subs; the response `errors` carry only your own subscription ids, never another tenant's data or raw DB/Stripe text). *(auto: `TestGetDueBillingForTenant_ScopesToTenant`)*
- [ ] Same call with a **platform** key (no tenant scope) → **403** (never triggers the global scheduler sweep). *(auto: `TestTriggerCycle_ForbidsUnscopedKey`)*
- [ ] Invoice auto-finalized, `payment_status=succeeded`. Line items: prorated base + usage + tax. **A missing usage line now means drift, not a setup slip (ADR-096):** the plan-attach guard rejects attaching an unpriced meter, so the common "forgot to bind" case is caught at plan create/update (422), not here. If a rule is later unbound out from under a live sub, finalize drops that meter's usage but is no longer silent — a `slog.Warn` fires (and `GET /v1/customers/{id}/usage` still warns "no rating rule binding — skipped from totals"). Re-bind with `POST /v1/meters/{id}/pricing-rules`.
- [ ] Stripe CLI shows `payment_intent.succeeded`. Dashboard MRR stays **$0** and the invoice shows a **Simulated** badge — the clock-pinned customer's rows are simulated (`is_simulated` / `test_clock_id`) and analytics gates simulated data out of every aggregate (ADR-086, `internal/analytics/simfilter.go`). A real wall-clock customer moves MRR; a test-clock smoke never does.

### S1.6 Sign out
- [ ] Sidebar → Sign Out. Redirect to /login.
- [ ] Stale cookie on `/v1/whoami` → 401.

**S1 passing = core engine healthy.**

## FLOW S2: AI-native end-to-end smoke

Walks the wedge demo path: instantiate an AI-native recipe → set up a customer on an in_advance plan → ingest LiteLLM-shaped events → observe hybrid invoice → fetch the public cost dashboard. ~15 min. Run this BEFORE every DP demo.

Prereqs: S1 passing (stack healthy, operator key in `$KEY`).

### S2.1 Recipe + plan
- [ ] **Recipe instantiates into a meter + plan + rules:** `POST /v1/recipes/anthropic_style/instantiate` → 201 (mode derives from the API key; the body takes only `overrides`). Creates ONE `tokens` meter + the `ai_api_pro` plan with per-`{model, token_type}` pricing rules (ADR-044: input / output / cache_read / cache_write_5m / cache_write_1h per model).
- [ ] **Make the plan in_advance:** Pricing → edit the `ai_api_pro` plan → set **Base fee billed = At start of period**, base price $99/mo, save → plan is in_advance with metered usage.

### S2.2 Customer (pinned to a test clock) + day-1 invoice
- [ ] **Mint the test clock FIRST** — S2.4 advances it to force cycle-close, so the customer must be pinned to it *at creation* (a wall-clock customer can't be re-pinned later; pin is create-time only, ADR-027):
  ```bash
  CLK=$(curl -sS -X POST "$API/v1/test-clocks" -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
    -d "{\"name\":\"s2\",\"frozen_time\":\"$(date -u +%FT%TZ)\"}" | jq -r .id)
  ```
- [ ] **Create the customer pinned to the clock:** `external_id=cus_demo_ai` **pinned to `$CLK`** (pass `test_clock_id` in the create body, or Customers → New → tick **Pin to test clock**); then add PM `4242 4242 4242 4242` via Add payment method → Stripe Checkout (needs the Stripe webhook forwarder running so `setup_intent.succeeded` attaches the PM). Note the internal `id` (`cus_…`) — used as `customer_id` below.
- [ ] **Day-1 invoice on activation:** create an active subscription on `ai_api_pro` → it **auto-inherits the customer's clock** → day-1 invoice `billing_reason=subscription_create`, in_advance base (prorated to the partial first period for a mid-period start — full $99 only when started at a period boundary), auto-charged.

### S2.3 LiteLLM ingest
- [ ] **Ingest accepts one event per token role:** POST a LiteLLM payload directly (simulates the proxy callback):
  ```bash
  curl -sS -X POST "$API/v1/integrations/litellm/spend" \
    -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
    -d '{
      "id":"smoke_call_1","call_type":"completion",
      "model":"claude-sonnet-4-5-20250929","custom_llm_provider":"anthropic",
      "user":"cus_demo_ai",
      "usage":{"prompt_tokens":1200,"completion_tokens":350,"total_tokens":1550},
      "response_cost":0.018,"endTime":'$(date +%s)'
    }' | jq .
  ```
  → `{"accepted":2,"skipped":0}` (one event per token role).
- [ ] **Repeat to 10 events:** run the curl 4 more times (`smoke_call_2` … `smoke_call_5`) → 10 events total (5 input + 5 output).
- [ ] **Events carry the recipe dimensions:** `GET /v1/usage-events?customer_id=<internal cus_ id>&limit=20` → 10 events on meter `tokens`, each with `dimensions.model=claude-sonnet-4.5` (canonical recipe family, ADR-044), `dimensions.model_raw=claude-sonnet-4-5-20250929` (verbatim), `dimensions.provider=anthropic`, and `dimensions.token_type` ∈ {`input`,`output`} (5 each). (Filter by the internal `customer_id`, not `external_customer_id`.)

### S2.4 Hybrid invoice at cycle close
- [ ] **Advance the clock to force cycle-close:** advance `$CLK` (minted in S2.2) ~1 month past sub start (curl shape: FLOW S1.4 / TC2) → the test-clock **catchup worker** closes the cycle and generates the cycle invoice on its own.
- [ ] **Backstop billing run is a no-op here:** `POST /v1/billing/run` → returns `invoices_generated:0`, because catchup already closed the clock-pinned cycle; it only generates for non-clock subs the wall-clock scheduler is due to bill.
- [ ] **Usage lines priced non-zero:** the invoice has a **Tokens** usage line for `input` and for `output`, both with **non-zero** amounts, each priced at the recipe's claude-sonnet-4.5 decimal rates.
- [ ] **Unit price is the nominal rate, not effective:** each Tokens line's **Unit Price** shows the clean configured rate (a terminating decimal, e.g. `$0.000003`), NOT a repeating/inflated `$0.00000333333333` (ADR-054 amendment: flat usage lines display the stamped nominal rate, not effective amount÷qty).
- [ ] **Base line covers the upcoming period:** the $99 base line shows "Covers &lt;upcoming range&gt;" (date range only — no "(in advance)" parenthetical).
- [ ] **Cycle equals preview for this clean sub:** usage line totals equal what `create_preview` showed — holds because this sub has no `usage_cap_units` and no mid-period plan/item change, so preview's un-replicated overlays (cap-scaling, segment proration; ADR-045) don't apply; the preview `warnings` array is **empty**.
- [ ] **Preview warns when it excludes a usage cap:** `create_preview` for a sub WITH a blocking `usage_cap_units` returns a `warnings[]` entry naming the excluded cap ("…excludes the subscription's usage cap…").
- [ ] **Preview warns on mid-period proration — but not on a first-period sub:** a sub that had a mid-period plan/quantity/item change this period returns a "…excludes mid-period proration…" warning; a brand-new sub in its first period does NOT (the initial item-creation row isn't a mid-period change).

### S2.5 Public cost dashboard
- [ ] **Generate the public URL:** Customer detail → **Public cost-dashboard URL** → Generate URL → copy the `https://…/v1/public/cost-dashboard/vlx_pcd_…`.
- [ ] **Public JSON carries the full projection:** `curl <that URL>` (no auth header) → JSON with `customer_id`, `tenant_id`, `billing_period`, `subscriptions[]`, `usage[]` (per-meter + rules), `totals`, `projected_total_cents`.
- [ ] **Public JSON leaks no PII:** the same response omits `email`, `display_name`, and `billing_profile` (sanitization contract).
- [ ] **Rotate invalidates the old URL:** click Rotate in the dashboard → old URL goes 401 immediately; the new URL works.

**S2 passing = wedge demo path is healthy. The AI-native pitch (LiteLLM → one `tokens` meter priced by `{model, token_type}` → hybrid invoice → embeddable cost view) works end-to-end.**

---

# Tier 2 — Full Suite

One flow per shipping feature. Run only what your change touched.

**Priority signal:**
- **Demo-blocking** (run before every DP demo): S1, S2, A1-A3, K1-K2, TC1-TC4, B1, B6, B13-B17, CU8, X15, all relevant I-series. Catches wedge regressions and the money path.
- **Compliance / correctness** (run on quarterly review or any tax/dunning change): B2, B10, B11, D1-D4, C1-C3, X1, X5, X7.
- **Operator UX polish** (run only when reworking that surface): K3, R5, B12, B18, CU6, U1, U3, U7-U10. Skip on routine pre-merge if you didn't touch the UI.

The matrix isn't enforced — operators decide based on the change. Default to "run everything your change touched + run S1/S2 always."

## FLOW TZ1: Tenant timezone semantics

Single tenant-wide timezone used for date input and timestamp display
(UTC for storage and billing math). Set in Settings → Account → Timezone.

- [~] Change timezone to `Asia/Kolkata` or `America/Los_Angeles` → dashboard timestamps shift, zone abbreviation appended (e.g. `2:14 PM PDT`). *(manual 2026-07-13: toggled org Asia/Kolkata ↔ America/Los_Angeles in the dashboard; list timestamps shifted and re-labelled IST↔PDT — formatDateTime renders in the live tenant zone with timeZoneName:short.)*
- [x] **Settings saves are validated and merge-safe (P8)** — saving `timezone="Mars/Olympus_Mons"` (or `"Local"`), `default_currency="DOGE"`, `invoice_prefix="INV/26"`, or negative net terms → **422** naming the field; a lowercase currency saves as canonical UPPERCASE; `net_payment_terms: 0` saves as Net-0 (due immediately), not silently reset to 30. A partial API body (e.g. only `tax_name`) leaves every unsent setting untouched. *(automated: `TestSettingsUpsert_ValidationRejects`, `TestSettingsUpsert_MergePatch`, `TestFromError_InvalidCarriesField`)*
- [x] API key expiry / list-page from-to filters interpret civil dates as start/end of day in tenant TZ. *(automated: `web-v2/tests/dates.test.ts` — startOfDayInTZ/endOfDayInTZ in Asia/Kolkata + America/Los_Angeles)*
- [x] **Subscription billing dates are anchored in the tenant timezone (ADR-058).** Set tenant TZ = `Asia/Kolkata`. Create an anniversary-monthly sub starting the **1st of a month** (e.g. Jun 1, in IST). The first period is **Jun 1 → Jul 1** = **30 days** (a June anniversary), NOT `Jun 1 → Jul 2` / 31 days. A mid-cycle upgrade prorates against the **30-day** cycle. *(automated: `TestAddBillingInterval_ProvenanceIndependent`, `TestFullBillingCycleDays_TenantTZAnchored`, `TestProration_StubPeriod_DividesByFullCycle`)*
- [x] Verify the SAME result regardless of whether the server runs `TZ=UTC` or `TZ=Asia/Kolkata` — the period and proration denominator must not depend on the host timezone. *(automated: `TestAddBillingInterval_ProvenanceIndependent` runs the UTC-located vs IST-located (DB-scanned-on-IST-host) cases; `TestFullBillingCycleDays_TenantTZAnchored`)*
- [x] **The billing timezone is ONE org-level setting; no per-subscription snapshot (ADR-077, supersedes ADR-074).** Subscriptions carry **no** `billing_timezone` column — every sub in a tenant bills in the single org timezone (default UTC), resolved live from tenant settings. The subscription page's period range ("Jun 1 – Jun 30", from `current_billing_period_display`) and its period boundaries + proration denominator all anchor in that org zone. Event *instants* ("Renews", "Next billing") render as wall-clock in the current zone. This matches the industry: the billing anchor is a UTC instant everywhere (Stripe `billing_cycle_anchor`), and the timezone is an org/customer concept — no peer anchors each subscription in its own zone. *(automated: `TestHandler_Get_PeriodDisplayTZ` locks org-zone period display; `domain.Subscription` has no `BillingTimezone` field, ADR-077)*
- [x] **An ISSUED invoice keeps its dates even if the org later changes timezone (ADR-077).** The org zone is denormalized onto each invoice at issue (`invoices.billing_timezone`). Create a sub + finalize an invoice while org TZ = `Asia/Kolkata`; the invoice's period/document dates render in Kolkata. Now change org TZ to `America/New_York` — the **already-issued** invoice's dates are **unchanged** (it's an immutable financial document carrying its own zone), while a running subscription's *future* civil-day computations follow the new org zone. Changing the org zone never re-bills or re-times an issued invoice. *(automated: `TestInvoice_PeriodDisplay_AnchoredInBillingTZ`, `TestPostgresStore_BillingTimezone_RoundTrips`)*
- [x] **Changing the org timezone mid-cycle never overbills a subscription — any billing interval (ADR-091).** With a $30/mo in_advance sub billing under org timezone `Asia/Kolkata`, change the org timezone to `America/New_York` and advance past the period boundary. The re-anchor "seam" (a few-hours-short upcoming period) must NOT bill a full cycle: a **calendar** sub absorbs it (no seam invoice; $60 over two months, not $90); an **anniversary-monthly** sub prorates a ~24h seam to ~$1 (not a full month); a **yearly** sub prorates a ~335-day residual to ~$27.53 (not a full year) — while a normal 28-day February anniversary period stays full-price (no spurious proration). The sub re-aligns to the new zone on the next full cycle. *(automated: `TestOrgTZChange_CalendarSeam_Absorbed_E2E`, `TestOrgTZChange_AnniversaryMonthlySeam_Prorated_E2E`, `TestOrgTZChange_YearlySeam_Prorated_E2E` — each mutation-verified; `TestIsPeriodStartOnAnchor`)*
- [ ] **(Deferred, ADR-092) Billing/display timezone split** — separating the billing anchor from a cosmetic display zone (so a display-only change can't re-time billing at all) is designed but not built; trigger = a design partner needing multi-timezone billing (per-customer zones, or display≠billing). No flow until then.
- [x] **Anniversary month-end clamps and restores, never ratchets (ADR-055).** Create an **anniversary**-monthly sub on a clock-pinned customer, activated on **Jan 31**. Advance the clock month by month: the cycle boundaries (`next_billing_at` / invoice periods) land on **Jan 31 → Feb 28 → Mar 31 → Apr 30 → May 31 → Jun 30** — clamped to month-end in short months and **restored to the 31st** in long ones, NOT drifting to the 3rd (`subscriptions.billing_anchor_day = 31`). A day-30 anchor gives Feb 28 then the 30th; a leap **Feb 29** anchor (monthly or yearly) bills Feb 28 in common years and Feb 29 in leap years. Calendar-monthly subs are unaffected (still the 1st; `billing_anchor_day = 0`). *(automated: `TestAnniversaryMonthEnd_ClampsAndRestores`, `TestYearlyLeapAnchor_ClampsAndRestores`, `TestAnchorDayFor`)*
- [x] Calendar-monthly anchored on the **31st** rolls to the **1st of next month** (does not skip February: Jan 31 → Feb 1). *(automated: `TestNextBillingPeriodEnd_TenantTZAnchored`)*
- [~] **Invoice period shows the INCLUSIVE last day (ADR-058 follow-up).** The invoice (detail-page header, Invoices-list Period column, and the PDF / hosted invoice / portal) shows **"Jun 1, 2028 – Jun 30, 2028"** for a June period — the last day actually covered — NOT the exclusive boundary "Jun 1 – Jul 1". Same string on every surface (one backend-authored `billing_period_display`), computed in the **invoice's own billing timezone** (`invoices.billing_timezone`, the org timezone resolved and denormalized at creation, ADR-077) — so a subscription invoice generated before the org changed its TZ still shows the period in the zone it was issued under, not the shifted current one. A one-off invoice (no period) shows no Period row. A per-line **"Covers <start> – <inclusive end>"** note (shown on a proration/mixed line whose span differs from the invoice's) is likewise inclusive — "Covers Jun 15, 2028 – Jun 30, 2028", not "– Jul 1". The raw API `billing_period_start`/`billing_period_end` stay unchanged (half-open RFC3339 instants). *(manual 2026-07-13: real day-1 invoice, org TZ Asia/Kolkata — API + PDF bytes both show "Period Jun 11, 2027 – Jul 10, 2027" inclusive over half-open Jul 11 wire; hosted page computes the same client-side from the instants + billing_timezone.)*
- [x] **Wire timestamps are canonical UTC `Z`, regardless of host timezone (ADR-075).** Every API `time` field (`created_at`, `billing_period_start/end`, `issued_at`, …) serializes with a trailing `Z`, NEVER a host offset like `+05:30` — even when the server runs `TZ=Asia/Kolkata`. (The process is pinned to UTC in `main()`, so the pgx `timestamptz` read path doesn't re-localize to the host zone.) Only the *calendar arithmetic* for period boundaries happens in the tenant/billing TZ (ADR-058/074); the wire stays UTC. *(automated: `TestInvoice_Timestamps_SerializeAsCanonicalUTC`)*
- [~] **Customer-facing document dates render in the billing timezone, not the host/UTC (ADR-075 audit).** On a host in `TZ=Asia/Kolkata` (or after pinning UTC), an invoice whose `billing_timezone = 'Asia/Kolkata'` shows its **Issued / Due / Paid / Voided** dates (invoice PDF), and a credit note against it shows its dates, in Kolkata — matching the "Period" row — not shifted a day. The dunning warning email's "we'll try again on ⟨date⟩" and a proration line's "(after ⟨date⟩)" label likewise render in the billing zone. Verify a near-midnight instant (e.g. issued 00:30 IST) prints the IST calendar day on the PDF, not the previous UTC day. *(manual 2026-07-13: PDF bytes of a Kolkata day-1 invoice show Issued "June 11, 2027" (issued_at 18:30Z = Jun 11 IST, not UTC Jun 10) and Due "July 11, 2027" (due_at 18:30Z = Jul 11 IST, not the UTC-truncated Jul 10) — the near-midnight day-boundary is correct; proration line renders "(after Jun 11, 2027)". Paid/Voided + dunning-email dates use the same formatDate(billing_timezone) path — the email "try again on ⟨date⟩" is exercised directly by the Dunning flow, credit-note dates by box "Cancel/plan-swap credit notes" (automated).)*
- [~] **The PUBLIC hosted-invoice page shows the same dates as the PDF (ADR-075).** Open a finalized invoice's hosted link (`/invoice/{token}`) — which is unauthenticated, so the browser has no tenant setting. The **Due** date (and Received/Voided when applicable) must match the downloadable PDF beside it, both rendered in the invoice's `billing_timezone`, NOT the viewer's local browser zone. Check a near-UTC-midnight `due_at` (e.g. `billing_timezone=America/New_York`, `due_at=2026-06-02T02:30:00Z` → both show **"Due June 1, 2026"**), viewed from a browser in a different zone. *(manual 2026-07-13: public hosted JSON /v1/public/invoices/{token} carries billing_timezone=Asia/Kolkata; HostedInvoice.tsx renders Due/Received/Voided via formatDate(x, billing_timezone); the public hosted PDF shows Due "July 11, 2027" identical to the authenticated PDF — hosted and PDF agree, both in the invoice's billing TZ, independent of viewer zone.)*
- [x] **Cancel / plan-swap credit notes show the period in the billing TZ (ADR-075/077).** Cancel (or plan-swap) a sub in a tenant whose org timezone is a positive-offset zone (e.g. `Asia/Tokyo`) mid-period. The resulting credit-note line description ("… period ⟨start⟩ to ⟨end⟩, canceled ⟨date⟩") reads the **billing-TZ** calendar days, not the UTC-prior day (a Feb-1-start period reads "2026-02-01", not "2026-01-31"). *(automated: `TestProrationRefundDesc_RendersBillingTZCivilDays` — Asia/Tokyo, all three cancel/plan-swap sites)*

## Authentication & API keys

## FLOW A1: Sign-in

- [ ] Empty form → inline error, no request.
- [ ] Wrong password → 401 "Invalid email or password".
- [ ] Repeated wrong passwords keep returning the same generic 401 — no lockout, no distinct 429/`account_locked` (v1 has no automatic account lockout or login throttle; deferred — ADR-094).
- [ ] Right credentials → redirect to `/`, dashboard loads.
- [ ] Cookie `velox_session`: HttpOnly, SameSite=Lax. No `velox_*` in localStorage.
- [ ] HttpOnly holds: browser-console `document.cookie` is empty, yet `fetch('/v1/whoami')` still returns 200.
- [ ] Cross-site `POST /v1/auth/logout` (form on a different host) → 403; you stay signed in. *(automated: `TestLogout_NoCookie_DoesNotClearTheCookie`)*
- [ ] Cross-site `POST /v1/auth/login` (text/plain body) → 403; not signed into the attacker's account. *(automated: `TestCSRFGuard_ReproducesTheLoginFixation`)*
- [ ] Cross-site top-level *link* to the dashboard → still signed in. *(automated: `TestCSRFGuard_Matrix`)*
- [ ] Legit same-origin dashboard writes and a headerless `curl` login still work.
- [ ] Reload → still signed in.
- [ ] Sign out → cookie cleared, redirect to /login. Stale cookie → 401.
- [ ] Auth events audited: `/audit-log` shows `login`, `logout`, and (if toggled) `mode_changed` rows, actor = the operator (not "System"). Expand a row → source IP shown.
- [ ] A failed login writes NO audit-log row.

### Password reset

- [ ] Forgot password → submit any email → 200 (no enumeration).
- [ ] Reset email lands in Mailpit (http://localhost:8025).
- [ ] Click link → set new password (12+ chars) → /login?reset=success → sign in.
- [ ] **Reset is audited**: `/audit-log` shows a **"password_reset_requested"** row (when the email matched a real account) and a **"password_reset_completed"** row on the affected `user`, scoped to the operator's tenant.
- [ ] Reused token → 422. Token >1h old → 422. Password <12 chars → 422.

## FLOW A2: /v1/whoami

- [ ] Cookie path: `curl -b /tmp/c.txt $API/v1/whoami` → `{tenant_id, user_id, email, livemode}`.
- [ ] Bearer path: `curl -H "Authorization: Bearer $KEY" $API/v1/whoami` → `{tenant_id, key_id, key_type, livemode}`.
- [ ] No credentials → 401.
- [ ] Cookie + Bearer with disagreeing identities → cookie wins.
- [ ] Revoked API key on Bearer → 401 immediately. Cookie sessions unaffected.
- [ ] Publishable key on Bearer → `key_type:"publishable"`. Every tenant-scoped endpoint — reads included — → 403 (empty scope set); only `/whoami` answers.

## FLOW A3: Test/Live mode toggle

- [ ] Top-right pill: amber "Test mode" default. Click → emerald "Live mode"; toast "Switched to live mode".
- [ ] **Stays on the same page** (no nav). On `/customers`, `/invoices`, `/audit-log`, `/usage`, `/credits`, `/dunning`, `/webhooks/events` — the page rerenders with the new mode's rows in place.
- [ ] **Back-and-forth is fast**: toggle Test→Live, scroll the list; toggle Live→Test → prior cache renders instantly (no spinner) before any background refetch.
- [ ] Mode-scoped pages reflect the new mode after toggle (no stale rows): Customers, Invoices, Subscriptions, API Keys, Audit Log, Usage Events, Credits, Credit Notes, Dunning, Pricing (plans / meters / rating rules), Webhooks (endpoints + event stream), Dashboard (MRR / active customers / recent invoices), **Settings → Payments** (Stripe credentials are keyed `(tenant_id, livemode)` — toggling swaps the connected-state card, masked secret, webhook-secret state, and the "Stripe — test/live mode" header).
- [ ] Mode-agnostic Settings tabs stay the same on toggle (one `tenant_settings` row per tenant, no `livemode` column): Settings → General, Settings → Invoicing, Settings → Tax. Recipes and Onboarding are likewise tenant-wide. Keyed remount still refetches but content is identical.
- [ ] Test Clocks: sidebar entry visible only in test mode. Toggling to live mode hides it; navigating to `/test-clocks` redirects to `/`.
- [ ] On a detail page (e.g. `/customers/cus_test_…`), toggle → page refetches and surfaces a 404 / "Not found" cleanly (entity doesn't exist in the other mode).
- [ ] WebhookEvents `/webhooks/events` SSE stream tears down + reopens on toggle — the frame buffer empties and fills with new-mode events; status pill goes connecting → live.
- [ ] `/v1/whoami` reflects new `livemode` immediately.
- [ ] `POST /v1/auth/mode` without cookie → 401.
- [ ] Live mode + missing live Stripe creds → red banner with "Connect Stripe" link.
- [ ] Logout while in either mode → both per-mode caches gc'd; signing back in starts fresh.
- [ ] **Cross-tab sync**: open the dashboard in two browser tabs. Toggle test→live in Tab A; switch to Tab B without clicking anything. Tab B's pill flips amber→emerald automatically (BroadcastChannel push from Tab A). Tab B's queries refetch live data on next focus — no stale TEST label over live data.
- [ ] **URL params dropped on toggle**: navigate to `/customers?status=active&cursor=cus_test_xxx`, toggle modes. URL becomes `/customers` (search string stripped). The opposite-mode page does not show an empty list because the dead cursor was carried over.
- [ ] **Per-mode invoice numbering**: in test mode, create an invoice → `VLX-000001` (default prefix). Toggle to live mode, create a real invoice → also starts at `VLX-000001` (or wherever the live counter sits — independent from test). Test exploration no longer burns live invoice numbers.
- [ ] **Per-mode rate-limit buckets**: hammer the dashboard in test mode until you see `429 Too Many Requests`. Toggle to live mode — live requests should still be allowed (separate bucket). Inspect Redis: `KEYS rate:rl:*` shows distinct per-mode buckets — dashboard/session traffic keys on `rate:rl:general:tenant:<tenant_id>:test` vs `…:live` (a Bearer key buckets on `key:<key_id>`, unauthenticated on `ip:<addr>`).

---

## FLOW K1: API key permissions

- [ ] Secret key: full read/write everywhere.
- [ ] Publishable key: deny-all (empty scope) — `GET /v1/customers` → 403, POST → 403; only `/v1/whoami` succeeds.
- [ ] Revoked key: any request → 401 `invalid or expired API key`.
- [ ] Create dialog: raw key shown once, copy button, "you won't see this again" warning.

> **Wire-message convention (ADR-026):** the API never reveals *why* a key failed — revoked, expired, and unknown all return the same generic 401 `invalid or expired API key`. The specific reason is logged server-side only. Don't assert revoked-vs-expired-vs-unknown from the response body.

## FLOW K2: Expiration

- [ ] Create key with presets: No expiration / 30d / 90d / 1y / Custom.
- [ ] Custom: today is disabled in calendar grid + Today button (tooltip explains minDate).
- [ ] Tenant TZ consistency: pick "30d" → hint "Key will expire on <date> at 11:59 PM <TenantTZ>". Stored UTC matches "23:59:59.999 in tenant TZ".
- [ ] Create with `expires_at = now+90s` via API → 200 until expiry, then 401 `invalid or expired API key` (generic — see K1 note).
- [ ] Backdate `expires_at` via psql → 401 `invalid or expired API key`.
- [ ] Keys ≤7 days from expiry → yellow "Expires in Xd" badge.
- [ ] Expired keys collapsed under "Expired keys" section; Revoke still enabled.

## FLOW K3: API Keys page UX

- [ ] Create dialog: name (≤100 chars), key type (Secret/Publishable), expiration preset (Custom requires date).
- [ ] Submit success → raw key shown ONCE with Copy button. Closing the dialog removes the raw value from memory; it never re-appears in the active-keys list.
- [ ] Per-row Revoke → confirm dialog (shows key name + masked prefix, "cannot be undone"); revoked key 401s on next request.

## FLOW K4: Rotate

- [ ] Rotate with `expires_in_seconds=300` → new raw_key returned; old key works ~5 min.
- [ ] Rotate with `expires_in_seconds=0` → old key 401 `invalid or expired API key` immediately.
- [ ] Rotate revoked key → 409 `cannot rotate a revoked key`.
- [ ] `expires_in_seconds > 604800` → 422.

---

## Test Clocks (test mode only)

## FLOW TC1: Test Clocks page

- [ ] Sidebar "Test mode" group + "Test Clocks" entry only when livemode=false. Live mode → entry hidden, `/test-clocks` redirects to `/`.
- [ ] Empty state with "New test clock" button.
- [ ] New dialog: optional name + date/time inputs (default now). Submit → detail page.

## FLOW TC2: Detail + Advance

- [ ] Detail header: name + status pill + Advance/Delete. Advance disabled when status≠ready.
- [ ] Advance dialog presets: `+1h / +1d / +1mo` + custom. Earlier-than-current time → inline error.
- [ ] **`+1mo` preset is a calendar month:** with the clock frozen at e.g. Jun 15, clicking `+1mo` fills Jul 15 — same day-of-month, landing exactly ON the monthly cycle boundary — not Jul 16 (+31 days).
- [ ] Submit → API responds in <500ms with `status: "advancing"` and the new `frozen_time`. Dashboard shows "Advancing…" badge (non-blocking — operator can navigate to other pages and the clock continues catching up in the background; ADR-015).
- [ ] `psql` (or any tab) shows `test_clocks.status='advancing'` while the worker runs. Polling `/v1/test-clocks/{id}` every 1.5s flips to `status='ready'` when the worker's catchup loop completes.
- [ ] Generated invoices appear in `/invoices` for the elapsed cycles — one per closed billing period, with `created_at` / `issued_at` aligned to the test-clock timeline (not wall-clock).
- [ ] **"Last advance results" card** — once the clock polls back to `ready` after the advance above, the detail page shows a **Last advance results** card beneath "Current clock time": the simulated span ("Advanced \<from\> → \<to\>") and a row per non-zero outcome — e.g. **Invoices generated: 3**. The count matches the invoices that actually appeared in `/invoices`. Advance again over a period where nothing is due → the card reads **"No billing activity — nothing was due in this period."** Trigger a partial-failure advance (FLOW TC2 injection knob) → after it lands in `internal_failure` the card still shows what completed, with the caveat line "This advance ended with an error…". `psql`: `SELECT last_advance_summary FROM test_clocks WHERE id='<clk>'` holds the JSON.
- [ ] Catchup failure (UI smoke): set `VELOX_TEST_CLOCK_INJECT_FAILURE="manual UI test"` on the velox process before clicking Advance → the next catchup attempt fails with "injected: manual UI test", clock flips to **internal_failure**, destructive banner surfaces "Catchup failed during last advance. Reason: injected: manual UI test" (ADR-018). The env is one-shot — cleared after firing — so the next click of **Retry advance** runs cleanly, status returns to `ready`. Partial invoices from earlier successful passes remain visible.
- [ ] **Retry advance** (ADR-018): from the internal_failure card, click **Retry advance** → status flips back to `advancing`, the worker resumes the idempotent catchup loop, simulation state preserved (customer + sub + earlier successful advances all intact). Re-fix the underlying issue first; otherwise the retry just re-fails with a fresh reason on the card. Retry from `ready` or `advancing` → 409.
- [ ] **Restart resilience (UI smoke):** start velox with `VELOX_TEST_CLOCK_CATCHUP_DELAY_MS=2000` so each billing pass takes ~2s — gives a deterministic window to time `kill -TERM`. Kick off +1y advance; status flips to `advancing` and stays there for ~24s on a single-sub fixture. Within that window, `kill -TERM` the velox process. Restart **without** the env (or with it, doesn't matter). On boot, slog logs `"test clock: re-enqueued in-flight catchup on boot"`, the worker resumes from the partial frozen_time, and the clock flips to `ready` within seconds. Verify in DB: invoice count matches a single-pass run, no gaps in `billing_period_start`.
- [ ] Detail page lists **Attached customers** (Stripe-parity primary surface, ADR-027) above **Subscriptions on this clock** (drill-down). Each customer row links to `/customers/:id`; each sub row links to `/subscriptions/:id`. Counts in the headers match: `N pinned` customers, total subs ≥ N.
- [ ] **Clock delete tears down its whole sandbox** (ADR-086): with 2 customers pinned to the clock (each with an active sub + at least one generated invoice) plus one unrelated wall-clock customer, click Delete. Dialog reads `"This permanently deletes the clock and everything in its sandbox — 2 customers and their 2 subscriptions — along with every simulated invoice, usage record, and credit created on it."` Confirm → the clock is gone from `/test-clocks`, both pinned customers are gone from `/customers`, and their subscriptions and invoices are gone. The unrelated wall-clock customer is untouched. `psql`: `SELECT count(*) FROM customers WHERE test_clock_id='<clk>'` → `0` and `SELECT count(*) FROM test_clocks WHERE id='<clk>'` → `0`.
- [ ] **A terminal sub is torn down too:** before deleting, manually cancel one pinned customer's sub. After clock delete it is gone as well — teardown removes every simulated row regardless of state (there is no "terminal preservation"; the whole customer graph goes).
- [ ] **The deletion is auditable:** the clock delete shows up in the audit log (`action=delete`, `resource=test_clock`, label = the clock's name, meta `action: teardown`), so "which clock existed and was removed" stays answerable even though the row itself is gone. The row carries the clock's own `test_clock_id` + its final `sim_effective_at`, so it appears INSIDE that clock's filtered timeline on `/audit-log` (FLOW P2) rather than floating free of it.

## FLOW TC3: Pinning (ADR-027 customer-level)

- [ ] Test mode → Create Customer dialog has "Pin to test clock" dropdown. Empty = wall-clock customer.
- [ ] Live mode → dropdown hidden on Create Customer.
- [ ] Customer Detail header shows test-clock badge when pinned.
- [ ] Customer Detail → Create Subscription dialog has no clock dropdown. Shows amber inheritance hint when the customer is pinned. Server inherits automatically.
- [ ] Subscriptions page → Create Subscription dialog: same behaviour driven by the customer dropdown. Pick a non-pinned customer → no hint, sub goes wall-clock. Pick a pinned customer → amber hint appears below the customer selector ("This subscription will inherit the customer's test clock — &lt;name&gt;"). Switch back to a non-pinned customer → hint disappears.
- [ ] `/subscriptions/:id` for a sub on a pinned customer → the header "Test clock" badge **links to the clock detail** (same on Customer Detail). List-row chips stay non-interactive — they render inside row links, where a nested anchor is invalid HTML. The sim-time banner's **View clock** links there too.
- [ ] Test Clock Detail page → "Attached customers" section above "Subscriptions on this clock" — shows customers attached to this clock, each linking to the customer detail.
- [ ] `POST /v1/subscriptions` against a pinned customer → response shows `test_clock_id` matching the customer's clock (server-side inherit; the API does not accept a per-sub `test_clock_id`, mirroring Stripe).
- [ ] `POST /v1/subscriptions` with a stray `test_clock_id` field in the body → field is silently ignored; sub still inherits from the customer.
- [ ] `PATCH /v1/customers/:id` does NOT change `test_clock_id` (immutable per ADR-027 / Stripe parity).

## FLOW TC4: Catchup correctness (ADR-028 disjoint flows + per-sub period loop)

- [ ] Pin monthly sub at "now". Advance 1 month → exactly 1 invoice generated; clock flips ready.
- [ ] Advance 3 months → 3 sequential-period invoices in ONE Advance click (not 3 cron ticks).
- [ ] **Large advance: pin monthly sub, Advance +1y → 12 sequential invoices in one click.** Status flips to `ready`. No `internal_failure`.
- [ ] **+1y boundary: target == frozen_time + exactly 1 year is accepted; +1y +1s is rejected with 422 and `errs.field=frozen_time`** (Stripe-parity per-call cap).
- [ ] **SPA: open Advance dialog, set target > +1y → submit button disabled and inline error names the maximum allowed target.** Toast on submit-attempt reads "Advance cannot exceed 1 year per call — chunk longer ranges into successive advances".
- [ ] **Chunked +5y: click Advance four times in a row (+1y each) → 60 sequential invoices total, status returns to `ready` after each chunk.**
- [ ] **Disjoint cron**: while a clock-pinned sub is at next_billing < frozen_time (gap state), watch slog for ≥1 wall-clock scheduler tick (5 min in dev). The tick must NOT bill the clock-pinned sub. Confirm in DB: invoice count for the sub unchanged across the tick boundary. Only operator Advance click bills clock-pinned subs.
- [ ] **Disjoint cron — full coverage (ADR-029)**: same gap-state setup as above, but extend the assertion to every time-aware path. Across the wall-clock tick boundary: no new charge attempt (`auto_charge_pending=true` row stays untouched), no tax retry (`tax_retry_count` unchanged), no threshold scan firing, no dunning advance (`invoice_dunning_runs.next_action_at` not bumped), no credit expiry on the customer's grants. Then click Advance: all five concerns fire in one operator action — the catchup orchestrator's Phases 1-5 process the deferred consequences in lockstep with simulated time. `slog | grep "scheduler tick"` shows the cron heartbeat is unchanged; the per-phase telemetry from catchup carries the actual work.
- [ ] **ADR-029 also covers a customer-pinned one-off invoice (no subscription).** Create a MANUAL one-off invoice for a clock-pinned customer with no payment method → it finalizes `auto_charge_pending=true`, `is_simulated=true`, and empty `subscription_id`. Attach a test card, then across the wall-clock tick: it is NOT auto-charged (no PaymentIntent, row stays pending), NOT enrolled in dunning (no `invoice_dunning_runs` row appears), and an existing run on it is NOT advanced. Click Advance → the catchup path charges/dunns it in simulated time. (The wall sweeps gate on the invoice's own `is_simulated`, not a subscriptions join — so a one-off with no sub can't leak, the gap a subscriptions-only exclusion left.)
- [ ] **Usage ingests in simulated time on an advanced clock:** pin a usage-plan sub, Advance +1 month, then POST a usage event with NO timestamp → event's `timestamp` = frozen_time (not wall-clock). POST one timestamped inside the simulated current period (wall-clock future) → 200, accepted. POST one past frozen_time + 5 min → 422 "must not be in the future". Backfill via **`POST /v1/usage-events/backfill`** timestamped between wall-now and frozen_time → 200, `origin=backfill` (the plain ingest route accepts past timestamps too but stamps `origin=api`). Advance again → the cycle invoice bills those events. Live-mode keys: behavior unchanged (wall-clock gates, no clock lookup).
- [ ] **Usage summary follows the clock:** with the clock advanced into a future month, `GET /v1/usage-summary/{customer_id}` (no `?from/?to`) defaults to the FROZEN month's window, not the wall-clock month.
- [ ] **Operator dashboard surfaces follow the clock (2026-07-08):** on a clock-pinned customer whose sub has been advanced a full cycle (frozen_time ≈ 1 month past period start), open Customer detail. **"Usage This Period" cycle bar** reads the simulated progress (e.g. "Day 31 of 31 · 100%" at cycle close), NOT "Day 0 of 31 · 0%" against wall-clock. The **"Last invoice · X ago"** line reads relative to frozen_time (a just-billed cycle invoice is "just now" in sim time, not weeks-old wall-clock). The **Activity** 7/30/90-day presets and the **Margin** card's default "Last 30 days" frame the simulated usage window (non-zero totals), not a wall-clock window that predates the simulated events (which read $0). The Activity **"Cycle"** preset is unchanged (already backend-defaulted). A wall-clock (non-pinned) customer's surfaces are unchanged.
- [ ] **Dashboard "recent invoices · X ago" follows the clock (2026-07-08):** with that clock-pinned customer's cycle invoice(s) generated by the advance, open the main **Dashboard**. In the **Recent invoices** card the simulated invoice's "X ago" reads relative to the clock's frozen_time (a just-billed cycle invoice is "just now" in sim time), NOT a wall-clock "Nd ago". Rows for wall-clock (non-pinned) customers still read against real time; the "Updated X ago" card header and webhook/API-key relative times are unchanged (genuine wall-clock).
- [ ] **Second advance while advancing → 409.** Force `advancing` via `psql -c "UPDATE test_clocks SET status='advancing' WHERE id='<clock>'"`, then `POST /v1/test-clocks/<clock>/advance` with any future time → `409 Conflict` + `{"error":{"type":"invalid_request_error","code":"invalid_state","message":"clock is advancing, must be ready to advance"}}`. Restore via `UPDATE … SET status='ready'`. After restore, `frozen_time` and `last_failure_reason` must match pre-test state — rejected advance leaves no side effects.
- [ ] Delete clock → COMPLETE teardown (ADR-086): pinned customers and every simulated row (subs, invoices, credits) are DELETED; non-pinned data unaffected.

## FLOW TC5: Dunning via catchup (clock-pinned failure recovery)

The headline test-clock use case — verifies the full Stripe-parity dunning state machine fires correctly under simulated time, including the (a) initial-run binding to frozen-time and (b) catchup-driven advance through retry → escalation → final action.

- [ ] Setup: clock at `frozen_time=2024-02-01`; pinned customer with Stripe test decline card `4000 0000 0000 0341` attached; active monthly sub. Policy: `grace=3 (days), retry_schedule=["72h","120h"] (= 3d/5d — Go durations, h/m/s only; a literal "3d" is now REJECTED at save with 422), max=3, final_action=pause`.
- [ ] Advance clock past the first cycle close → invoice finalizes → auto-charges → Stripe declines → dunning run created in the **same Advance** (inline from ChargeInvoice, not waiting for the async webhook) with `created_at` and `next_action_at` anchored on the invoice's cycle-close instant (NOT advance-end frozen_time). ADR-035.
- [ ] **Single-click full walk (Stripe Test Clocks parity)**: from a state where dunning has just started at cycle-close T, advance the clock to T + grace + sum(retry_schedule) + 1d in ONE Advance click. After the advance: run is `state=escalated`, `attempt_count=max`, `resolution=retries_exhausted`, all retry events present in the dunning timeline at distinct simulated-time timestamps (started at T, retry #1 at T+grace, retry #2 at T+grace+retry_schedule[0], escalated co-instant with the final retry). NO need to click Advance multiple times — Phase 5 loops until all due retries fire.
- [ ] If `final_action=pause` → owning sub gets `pause_collection_behavior=keep_as_draft` and **`status` stays `active`** (hard-pause status was removed in PR-8/migration 0090 — collection pauses, the cycle keeps drafting; the later checkbox in this flow asserts the same contract).
- [ ] **Card-less clock-pinned sub reaches a terminal under catchup (ADR-060)**: pin a customer with **no saved card** to a clock; advance past a cycle close → invoice finalizes `auto_charge_pending` (no charge) and catchup Phase 3.5 creates a dunning run for it; keep advancing through grace + retries → run escalates to `final_action` (e.g. pause: `pause_collection_behavior=keep_as_draft`, status stays `active`). Without this the card-less simulated invoice would never dun.
- [ ] **Email cadence**: Mailpit shows exactly **N+1 emails per invoice for an N-retry exhausted run** — 1 initial payment-failed + (N-1) dunning-warning ("Attempt k of N") + 1 dunning-escalation. NOT 2 emails per retry. Verify by querying `SELECT email_type, COUNT(*) FROM email_outbox WHERE payload->>'invoice_number' = '<inv>' GROUP BY email_type`.
- [ ] **Per-customer dunning policy assignment (ADR-036)** — create a second policy on `/dunning-policies` (e.g. "Enterprise", `grace=7d, retry_schedule=[5d, 5d, 5d, 5d], max=5, final_action=manual_review`). Assign it to a customer via the customer detail "Dunning policy" → Change dropdown. Save → re-trigger a failed-payment cycle for that customer → the resulting dunning run carries `policy_id` = the assigned policy; retries follow the new cadence (Enterprise: 7d grace + four 5d gaps), not the tenant default. Re-assign the customer back to "Tenant default" via the dropdown → next-fired run picks up the default policy.
- [ ] `pause` → `subscriptions.pause_collection` is set with `behavior=keep_as_draft`; subsequent cycles still draft an invoice for the period (NOT skipped). Status stays `active`.
- [ ] `cancel_subscription` → `subscriptions.status='canceled'`, no further cycle billing.
- [ ] `mark_uncollectible` → the unpaid invoice flips to `status='uncollectible'` (NOT voided). Subscription itself stays untouched. Audit row `invoice.update` with `action=marked_uncollectible`. Webhook `invoice.marked_uncollectible` fires.
- [ ] `manual_review` → run state=escalated; no sub/invoice mutation; operator notified.
- [ ] **A failed terminal action keeps the run re-attemptable, not falsely "escalated"**: set `final_action=cancel_subscription` (or pause/uncollectible) and make the mover fail at exhaustion (e.g. the sub is in a conflicting state). The run lands `state=active`, `resolution=action_failed`, `next_action_at` set (NOT `escalated`/`retries_exhausted`) — so a later due tick re-runs the action and it escalates once it succeeds. Pre-fix the run showed a clean "escalated" beside a sub/invoice that never actually closed.
- [ ] **An ambiguous retry doesn't write off a paid invoice**: during dunning, make a retry charge return an *unknown* outcome (Stripe 5xx/timeout — drop the result) on an invoice whose PaymentIntent actually **succeeded**. The dunning run does NOT tick `attempt_count` for that retry and does NOT exhaust → no spurious cancel/uncollectible; the reconciler then marks the invoice `paid`. A *definite* decline still ticks and escalates normally.
- [ ] **A background credit-settle resolves the invoice's dunning run:** put a finalized invoice into dunning (failed charge / no payment method → **red Dunning badge**, an `active` run), then grant the customer enough credit to cover the balance and run the auto-charge sweep (or Advance a test clock). The invoice flips to **`paid`** (Credits Applied −$X) AND its dunning run flips to **`resolved`** / `payment_recovered` (red badge clears) — not left `active`. Pre-fix the background credit settle marked the invoice paid but never resolved the run (only the dashboard payment buttons did).
- [ ] **The dunning sweep never duns or cancels an already-paid invoice:** if a run's invoice was settled out-of-band before its next dunning action came due, the sweep **resolves** the run as `payment_recovered` instead of retrying — no dunning email, `attempt_count` does not tick, and at **max retries** it does **not** fire the terminal action (no pause-collection / **subscription cancel**) on a fully-paid invoice.
- [x] **A failed invoice left without a dunning run gets one via the backfill sweep:** if a payment fails but `StartDunning` never lands (a crash between the fail commit and the post-commit start, or an exhausted retry), the `dunning_backfill` reconciler finds the finalized, still-owed, run-less `failed` invoice on a later tick and starts a run — exactly once (an invoice that already has a run in *any* state, including `resolved`, is left alone). *(automated: `TestListFailedWithoutDunningRun_CandidateSet`, `TestEnrollFailedWithoutDunning_*`)*
- [ ] **Dunning resolve on a clock-pinned invoice stamps simulated `paid_at`** — from an active dunning run on a clock-pinned invoice, click Resolve → Payment recovered. Reload invoice detail. `invoice.paid_at` lands in simulated time (the test clock's current frozen_time), NOT wall-clock.
- [x] **No configured dunning policy never breaks the money path (ADR-036 amendment, Finding 2):** a tenant with **no default** dunning policy (or none at all — a plain bootstrap) that bills an **unpaid** cycle invoice → the test-clock advance still lands `ready` with **`had_errors:false`** (NOT `internal_failure`); the no-payment enrollment is a logged **deliberate skip**, not a per-invoice error. A genuine infra error still surfaces as `had_errors:true`. *(automated: `TestStartDunning_NoPolicyConfigured`, adapter swallow + retry short-circuit tests)*
- [x] **The first dunning policy a tenant creates is auto-default** — instantiate a recipe (or create the first policy manually) on a fresh tenant → that policy is `is_default=true` with no separate "Set default" step, so an unpaid invoice enrolls in dunning out of the box. A *second* policy is `is_default=false` (operator promotes via the Default toggle). *(automated real-PG: `TestUpsertPolicy_AutoDefaultFirst`)*

## FLOW TC6: Trial expiration via catchup (ADR-037)

Trial transitions flip status AT `trial_end_at` (Phase 0.5), not at
the next cycle close. First chargeable period anchors on
`trial_end_at` per the `(billing_time × interval)` matrix.

**Calendar + monthly + trial (the common case):**

- [ ] Create sub on clock-pinned customer with `trial_days=14`, `billing_time=calendar`, monthly plan. Result: `status=trialing`, `trial_end_at = frozen+14d`, `current_period_start = trial_end_at` (day-snapped), `current_period_end = first-of-next-month-after-trial_end_at` (the post-trial stub).
- [ ] Dashboard timeline reads `Created → Trial ends → First charge`. Stat card heading says **Trial period** (not Billing Period) with the trial dates. `isPast` checkmarks honor frozen_time, not wall-clock.
- [ ] Advance clock to `trial_end_at + 1s` → catchup Phase 0.5 fires: sub.`status=active`, sub.`activated_at = trial_end_at` (exact instant, not advance-end). No cycle invoice yet — `next_billing_at` is the stub's close.
- [ ] `subscription.trial_ended` webhook fires once, `triggered_by="schedule"`, `ended_at = trial_end_at`.
- [ ] Continue advance past `next_billing_at` (the first-of-next-month) → catchup Phase 1 bills the stub period prorated. `billing_period_start` = `trial_end_at`, `billing_period_end` = first-of-next-month, `total_amount_cents` = plan base × `stub_days / full_cycle_days` (full_cycle_days = days from period_start to the next billing boundary for the plan's interval — 28–31 for monthly).

**Anniversary + monthly + trial:**

- [ ] Same setup with `billing_time=anniversary` → no stub. `current_period_start = trial_end_at`, `current_period_end = trial_end_at + 1 month`. First chargeable cycle is a full month from trial-end. All other assertions identical.

**Yearly plan + trial:**

- [ ] Sub with `trial_days=14` on a yearly plan → `current_period_end = trial_end_at + 1 year` regardless of `billing_time`. (Stripe and Lago both force anniversary semantics for yearly; calendar setting is ignored.)

**EndTrial-early (operator override):**

- [ ] Before clock crosses `trial_end_at`, operator clicks **End trial now**. Sub instantly: `status=active`, `trial_end_at = now (truncated)`, `current_period_start = now (day-snapped)`, `current_period_end = next-anchor-for-billing_time-and-interval`. No 8-day "deferred billing" gap.

**In_advance + trial coverage:**

- [ ] Setup with an `in_advance` plan + trial. At trial expiry (either via clock advance OR EndTrial), `BillOnCreate` fires for `[current_period_start, current_period_end]` — covers the first paid period at the activation instant. Invoice `billing_reason=subscription_create`. In_arrears plans skip this (cycle billing handles them at period close).

## FLOW TC7: Plan change at period boundary via catchup

- [ ] Setup: clock-pinned active sub on plan A ($29/mo). ChangeItem with `new_plan_id=B` ($49/mo), `immediate=false`.
- [ ] Sub item `pending_plan_id=B`, `pending_plan_effective_at = current_billing_period_end` (frozen-time-derived).
- [ ] Advance clock past `current_billing_period_end` → catchup `billOnePeriod` swaps plan_id A→B atomically before billing next period.
- [ ] First post-swap invoice line items reference plan B; total reflects $49 base.
- [ ] Downgrade variant (B cheaper than A, immediate=true) on a **paid** in_advance source: the unused portion of A is credited via a **credit note** against the paid invoice (ADR-048) — balance credit = **gross** (net + the reversed tax slice on a taxed prebill; gross == net when untaxed), proportional tax reversed.

## FLOW TC8: Subscription cancellation at period end (via catchup)

Yearly-sub and future-dated `cancel_at` variants are impractical to verify on wall-clock — test clock is the only path.

- [ ] Setup: clock-pinned active monthly sub. `POST /subscriptions/:id/schedule-cancel` with `at_period_end=true`.
- [ ] Sub `cancel_at_period_end=true`.
- [ ] Advance clock past `current_billing_period_end` → catchup `FireScheduledCancellation` → sub `status=canceled`, `canceled_at` = the period-end instant exactly (frozen-derived, not the advance target). There is no `ended_at` column — `canceled_at` is the single terminal timestamp.
- [ ] The just-ended period still bills (final invoice on the current plan); no invoice for any period after cancellation.
- [ ] Future-dated `cancel_at` variant (ADR-097): schedule-cancel REJECTS `cancel_at` inside the current period with a teaching 422 ("must be on or after current_billing_period_end — mid-period cancel with proration is not yet supported"), so `frozen+200d` on a fresh yearly sub is invalid; use `cancel_at = current_billing_period_end + 15d`. Advance to before → sub still active. Advance past → the cycle renews at the boundary, then the due cancel fires AS an immediate cancel at the simulated `cancel_at` instant: `canceled_at = cancel_at` exactly, final partial invoice `[period_start, cancel_at]` (usage + prorated in_arrears base; prepaid in_advance portion returns as balance credit via CN) — the sub is never silently billed past its cancel.

## FLOW TC8b: Mid-period cancel of an UNPAID in-advance prebill

Setup: clock-pinned customer on an in_advance plan (e.g. $30/mo), day-1 invoice finalized but left **unpaid** (no payment method / declined). Cancel immediately (`POST /subscriptions/:id/cancel`) mid-period, not at period end.

- [ ] **Partial consumption** (cancel ~halfway through the period): the prebill invoice's **amount due drops to the consumed portion** (e.g. $30 → ~$15) — a credit note for the unused portion is linked on the invoice. Invoice is NOT voided and stays collectible for the consumed amount.
- [ ] **No consumption** (cancel at/near period start): the prebill invoice **status → voided** (no credit note).
- [ ] Customer credit balance is **unchanged** on both paths — no balance credit is granted for an invoice that was never paid.
- [ ] Paid-invoice contrast (mark the prebill paid first, then cancel mid-period): unused portion goes to the **customer credit balance**; the invoice is not voided/reduced. **On a taxed prebill (ADR-048):** the clawback is issued as a **credit note** against the paid invoice — the balance credit is the **gross** unused (net + the tax the customer paid on it), and the credit note carries the proportional reversed tax (a Stripe Tax reversal is filed for `stripe_tax`). Zero-tax prebills are unchanged (gross == net). The same applies to an immediate **plan swap** of a paid in_advance period.

## FLOW SUB-CARD: Subscription billing-cycle card surface

Locks in the 2026-05-20 "Renews on" annotation + alignment tooltip (Stripe/Lago/Chargebee/Recurly converging UX pattern).

- [ ] Active sub detail page → stat card row shows primary label **"Renews <date>"** (the exclusive next-billing date — e.g. "Renews Jul 1, 2028") with a muted secondary line **"Period: <start> – <inclusive last day>"** (e.g. **"Period: Jun 1, 2028 – Jun 30, 2028"** — the last day actually covered, NOT the exclusive "Jul 1"; ADR-058 follow-up). The "Current period: <range>" pre-redesign labeling is gone.
- [ ] Details panel below shows **"Renews on"** row (exclusive boundary, "Jul 1, 2028") above **"Current period"** row (inclusive range, "Jun 1, 2028 – Jun 30, 2028") — both filled for active subs; the two differ by one day by design (the renewal fires ON the boundary; the period covers through the day before). The timeline "Period End" dot and the cost-dashboard cycle bar likewise show the inclusive last day.
- [ ] **"Billing alignment"** row (renamed from "Billing Time") shows `Calendar` or `Anniversary` with a `?` hover tooltip explaining: (a) alignment is set at activation; (b) calendar+monthly anchors to first-of-next-month at first cycle close; (c) **scheduled** plan-interval changes (`immediate=false`) preserve the existing day-of-month anchor at the boundary; (d) **immediate** cross-interval swaps (e.g. yearly → monthly with immediate=true) re-anchor the cycle on the swap day (see FLOW B21).
- [ ] Trialing subs: stat card shows trial-specific labels instead (no "Renews on" until trial ends).
- [ ] **Activity timeline: SIM-primary on clock-pinned subs (ADR-030 2026-07-18 amendment — narrative surfaces speak sim)**: on a clock-pinned active sub's detail page, click any item-add/**plan-change**/cancel/etc. action. The new audit row in the Activity card shows: (a) primary right-aligned timestamp = the **simulated** instant (matching Properties/invoice dates on the same page), (b) amber **test clock** chip next to the description (tooltip carries the full dual-stamp provenance incl. the clock ID), (c) a muted subline "Recorded `<wall-clock>` · by `<actor>`" — no raw clock ID in visible copy. Rows on a wall-clock (non-clock-pinned) sub show NO chip and the plain wall timestamp.
- [ ] **Plan-change audit carries test-clock context on the global Audit Log too**: on a clock-pinned sub, change the plan, then open `/audit-log`. The `subscription.item_updated` ("Changed plan") row shows the **wall-clock** click time as its primary timestamp (today — NOT the test clock's far-future simulated date), an amber **test clock** chip, and the "Effect on test clock at `<simulated time>`" subline on expand.

## FLOW TIMELINE-ORDER: Activity timeline ordering (invoice + subscription)

Locks in the 2026-05-21 `sort.SliceStable` fix. On a test-clock-pinned sub, the inline cycle close → charge fail → dunning start cascade stamps three audit events at the EXACT same simulated instant.

- [ ] On invoice detail page activity for a clock-pinned in_arrears sub with a known-failed charge at cycle close: lifecycle events (Invoice created → Invoice finalized) render BEFORE dunning events ("Payment recovery started").
- [ ] **Same-instant tie matrix** (a frozen-clock close cascade stamps ALL of these at ONE instant — each pair must render in causal order, whatever order the sources returned): *(automated: `TestSortInvoiceTimeline_CausalTies`, `internal/platform/timeline`)*
  - Invoice created → Invoice finalized → Dunning started
  - Retry attempt → Escalated → Marked uncollectible
  - Failed retry attempt → Invoice paid (the retry that collected renders ABOVE "Invoice paid")
  - Two credit notes milliseconds apart keep creation order (full-precision axis, not the second-truncated string)
  - Subscription timeline: same-sim-instant rows order by wall recorded-at (a pause PUT and its audit row never swap)
- [ ] Activity timeline detail timestamps (e.g. "Auto-resumes Jun 20, 2029" on Collection paused, "On Jun 30, 2029" on Cancellation scheduled, "New trial end: Jul 1, 2029" on Trial extended) render in **tenant TZ**, matching the row's main timestamp — NOT in UTC. Regression check for the 2026-05-21 `formatAuditTimestamp` UTC-format bug.
- [ ] **Lane failure is disclosed, not swallowed:** `REVOKE SELECT ON credit_notes FROM velox_app`, reload an invoice's Activity → red notice "Some activity couldn't be loaded (credit notes). This timeline may be incomplete — refresh to retry." while the lifecycle rows still render; `GRANT SELECT ON credit_notes TO velox_app`, reload → notice gone. `GET /v1/invoices/{id}/payment-timeline` carries the same fact as `degraded: ["credit_notes"]`. *(automated: `TestPaymentTimeline_DegradationDisclosure`)*

## FLOW SUB-REALIGN: Calendar-billing subs auto-realign anchor at cycle close

Locks in the 2026-05-21 fix: when a sub with `billing_time=calendar` has its period anchor drifted to mid-month (e.g., post-yearly→monthly plan swap preserves the yearly anniversary day-of-month, then monthly cycles preserved day-N indefinitely), the next cycle close MUST snap back to the next calendar boundary. Velox's stance partway between Stripe-flexible/Lago/Chargebee (always preserve day-of-month) and Stripe-legacy/Recurly (always re-anchor): honor the operator's configured `billing_time`.

**Setup** — reproducing the drifted-anchor state from scratch (the only realistic path; you can't directly create a calendar+monthly sub anchored on day-20 because calendar billing always snaps to day-1 at activation):

1. Create a test clock pinned at e.g. `2028-05-20 00:00 IST` (any non-month-start day works).
2. Attach a customer to the clock.
3. Create a yearly in_advance plan ($120/yr is fine for the math).
4. Create a sub on that customer + yearly plan + `billing_time=calendar` + `start_now=true`. Yearly plans always anchor on the activation day-of-month regardless of `billing_time` (no industry analog for "calendar yearly stub to Jan 1"), so the period anchors on day-20: `(May 20 2028, May 20 2029)`. Day-1 invoice for $120 fires.
5. Create a second monthly plan (e.g. $30/mo, in_advance).
6. From the subscription detail page → Change Plan dialog → pick the monthly plan + leave **"Apply at next period boundary"** selected (i.e., `immediate=false`). This stages the swap to fire at the yearly cycle close.
7. Advance the clock past `2029-05-20` (the yearly anniversary). Cycle close fires:
   - Pending plan applies → item now on monthly $30.
   - New period = `domain.NextBillingPeriodEnd(May 20 2029, calendar, monthly, IST)` = first-of-next-month = `Jun 1 2029`. So new period = `(May 20 2029, Jun 1 2029)` — **12-day calendar-snap stub**.
   - In_advance invoice: `$30 × 12/30 = $12.00` prorated (the `0d03ebd` fix). Line item description carries `prorated 12/30 days`.

If you skip steps 1-7 and want the drifted-but-not-yet-realigned shape (`(May 20, Jun 20)` mid-month period that hasn't seen its first cycle close yet), there's no direct UI path — would need a direct DB update which we explicitly avoid per `feedback_no_speculative_backfill`. The above setup gets you to the realistic post-swap state and immediately exercises the auto-realign.

**Assertions after the setup completes:**

- [ ] Sub's `current_billing_period` is `(May 20 2029, Jun 1 2029)` and `next_billing_at = Jun 1 2029` — verify in DB. period_start is the yearly anniv (= cycle-close instant); period_end is the calendar-snapped first-of-next-month.
- [ ] **In_advance stub proration** (the `0d03ebd` regression check): cycle-close invoice line item description reads `Mo - base fee (qty 1, prorated 12/30 days)`. Amount = `$30 × 12 / 30 = $12.00` (not the full $30).
- [ ] Advance clock past Jun 1 → next cycle close → new period = `(Jun 1, Jul 1)` full month. Now `next_billing_at = Jul 1`. In_advance invoice for $30 full (no proration; full cycle).
- [ ] Advance clock past Jul 1 → next cycle close → `(Jul 1, Aug 1)`. Day-1 anchored forever after.

**Anniversary negative guard** (repeat the setup with `billing_time=anniversary` at step 4):

- [ ] After step 7, period = `(May 20 2029, Jun 20 2029)` — day-of-month preserved, NO calendar snap. Full monthly $30 invoice (not prorated; 30-day full cycle from anchor).
- [ ] Advance past Jun 20 → `(Jun 20, Jul 20)`. Then `(Jul 20, Aug 20)`. Anniversary day-20 anchored forever.

## FLOW TC9: Pause collection auto-resume (via catchup)

End-to-end coverage of `pause_collection` with `resumes_at` auto-resume. The dashboard's Pause Collection dialog now exposes the "Auto-resume on" date input (Stripe-parity — Stripe's pause-collection modal has the same field). API path is still available for SDK callers.

- [ ] Setup: clock-pinned active sub. Open Pause Collection dialog → enter `Auto-resume on = frozen+7d` → confirm. Toast: "Collection paused — auto-resumes on the date you picked".
- [ ] Sub row in DB: `pause_collection_resumes_at = frozen+7d`, `pause_collection_behavior=keep_as_draft`.
- [ ] Advance clock through a cycle boundary while paused → invoice generated but stays DRAFT (no finalize, no charge, no dunning) — engine respects pause_collection.
- [ ] Advance clock past `resumes_at` → catchup Phase 0.7 auto-clears `pause_collection_*` columns AT `resumes_at` (not waiting for next cycle close); next cycle bills normally; previously-draft invoice can be finalized manually if intended.
- [ ] Activity timeline shows "Collection paused" (manual via dashboard) or "Collection paused by dunning" (when reached as a dunning final_action), with "Cycle keeps drafting; no charge until resumed" or "Auto-resumes …" detail line.
- [ ] Activity timeline shows "Collection auto-resumed — Scheduled resume date reached" for the schedule-driven resume (distinct from "Collection resumed" emitted by manual API call).
- [ ] API parity: `PUT /v1/subscriptions/{id}/pause-collection` with `{"behavior":"keep_as_draft","resumes_at":"..."}` produces the same sub state as the dialog path (resume is `DELETE` on the same route).

## FLOW TC10: Credit grant expiry firing (via catchup)

The ONLY end-to-end manual-test coverage of credit expiry actually firing. C1 verifies the grant is created with `expires_at` populated but doesn't exercise the cron's `ExpireCredits` job; test clock is the only practical way to verify the expiry mechanic.

- [~] Setup: clock-pinned customer. Grant $50 credit with `expires_at = frozen+30d`. *(manual 2026-07-19: clock tc10-credit-expiry frozen Jan 1 2028 IST; Expiry Corp created INTO the clock — customer row Created "Jan 1, 2028"; $50 grant via the Credits dialog, expiry Jan 31 2028; the dialog's expiry floor reads the customer's SIMULATED now — useEffectiveNow(test_clock_id) — so sim-future dates validate correctly.)*
- [~] Customer credit balance = $50; ledger entry `entry_type=grant`. *(manual 2026-07-19: balance $50.00; Grant row dated Jan 1, 2028 — FROZEN time, not wall — with "Expires Jan 31, 2028" subtext.)*
- [~] Advance clock past `expires_at` → catchup Phase 4 fires → new ledger entry `entry_type=expiry`, `amount_cents=-5000`, `created_at` at the grant's **contracted expiry instant** (its `expires_at`, NOT the advance-end frozen time). Balance back to $0. *(manual 2026-07-19: advanced Jan 1→Feb 15; advance telemetry card reported "Credit grants expired: 1"; Expiry row -$50.00 dated Jan 31, 2028 — the contracted instant; balance $0.00.)*
- [~] Customer detail page shows the ledger one click away: the **Credit Balance stat card links to** `/credits?customer=…`, whose Transaction History shows both grant and expiry entries with frozen-time dates. *(manual 2026-07-19: stat card $0.00 → linked ledger shows both rows; the old inline "Credits tab" wording predated the stat-card redesign.)*
- [~] **Expired grant is retired, not just journaled (ADR-071)** — after the expiry fires, a new invoice applied to this customer consumes **$0** from the expired grant (Credits Applied stays $0; the invoice's amount due is unchanged). *(manual 2026-07-19: one-off $10+tax invoice finalized at frozen Feb 15 → NO Credits Applied row, Amount Due $11.00.)*
- [~] Non-expiring grants (`expires_at IS NULL`) survive arbitrary advances — only expirable grants get expired. *(manual 2026-07-19: $20 no-expiry grant; +1mo advance produced NO expiry entry and no "grants expired" telemetry — instead the catchup's credit-cover sweep legitimately APPLIED $11 to the open invoice (ledger "Applied to invoice NIM-000122" dated Mar 15; invoice flipped Paid, paid_at = the simulated sweep instant), balance $9.00 — the surviving grant works as live money.)*

---

## FLOW E: Email delivery (SMTP)

Single delivery path: when SMTP isn't configured every send returns
`ErrSMTPNotConfigured`. No stdout fallback. Local dev = Mailpit
(`docker compose up -d mailpit`, `SMTP_HOST=localhost:1025 SMTP_TLS=none`).

- [~] **Strict STARTTLS + honest transport log (P5/ADR-072)** — boot logs `smtp transport mode=...`; pointing `SMTP_TLS=starttls` (the default) at Mailpit fails loudly at send with "does not advertise STARTTLS … set SMTP_TLS=none explicitly"; with `SMTP_TLS=none` (the shipped local config) delivery works. Rows are marked individually — a failed row retries alone while already-delivered rows stay `dispatched` in `email_outbox`. *(manual 2026-07-19: transport log flips mode=starttls strict_starttls=true ↔ mode=none across restarts; the strict failure row parked pending with the remedy-bearing error and per-attempt ERROR logs, then dispatched on attempt 4 after the flip back — dispatched siblings untouched.)*

Boot warnings on startup (one each when var unset; never fatal):
- `SMTP NOT CONFIGURED`
- `HOSTED_INVOICE_BASE_URL NOT SET` — invoice / receipt / dunning / payment-failed CTAs render with no link
- `PAYMENT_UPDATE_URL NOT SET`
- `DASHBOARD_BASE_URL NOT SET` — password-reset emails won't send

- [x] **E1 STARTTLS**: `SMTP_TLS=starttls SMTP_PORT=587` + creds. Trigger invoice email → `email_outbox` row pending → dispatched within seconds → recipient receives. *(manual 2026-07-19: DELIVERED end-to-end via real production Postmark to a real Gmail inbox — Postmark Status=Sent, event Delivered "smtp;250 OK gsmtp". Also Mailtrap sandbox 587. GOTCHA proven the hard way: the SMTP 250 that marks the row 'dispatched' is Postmark ACCEPTING the handoff, not delivering — an unverified From (billing@velox.dev) is 250-accepted then async-400-rejected; only a VERIFIED Sender Signature actually delivers. "dispatched" ≠ "delivered" for async-validating providers; final delivery is the bounce/delivery webhook.)*
- [~] **E2 Implicit TLS**: `SMTP_TLS=implicit SMTP_PORT=465`. Same expectation; verifies `tls.Dial` path. *(manual 2026-07-19: MECHANISM verified — against Mailtrap's 465 (STARTTLS-only, no SMTPS) tls.Dial failed LOUDLY with "first record does not look like a TLS handshake", no plaintext downgrade, backoff retry. Delivery leg needs a true-SMTPS provider (e.g. Zoho 465) — provider-gated.)*
- [~] **E3 Not configured**: unset `SMTP_HOST`. Boot → `SMTP NOT CONFIGURED` warning. Trigger send → outbox claims, logs `ErrSMTPNotConfigured`, retries with backoff, lands in DLQ. *(manual 2026-07-19: boot WARN; send parked pending with "email: SMTP not configured (SMTP_HOST unset)"; retried to exactly MaxOutboxAttempts=15 then status='failed' with the structured "row moved to DLQ" ERROR — permanent:false, correctly budget-exhausted.)*
- [ ] **Stale action-required mail is skipped, not delivered**: queue a `payment_setup_request` (finalize a no-PM invoice) with the dispatcher paused (`SELECT pg_advisory_lock(76540004)`), settle the invoice (pay/credit-cover/void), release the lock → the outbox row lands `status='skipped'` with the reason in `last_error`, nothing arrives in Mailpit; informational types (invoice document, receipt) still deliver for settled invoices. *(automated: `TestDispatcherStalenessGate`, `TestProcessBatch_ObsoleteRowMarksSkipped`)*
- [ ] **A settled invoice's email says so**: Email invoice on a paid/credit-covered invoice → subject "Invoice ⟨n⟩ — no payment due", body "This invoice is settled — no payment is needed", CTA "View invoice" (never "View & pay"); an open invoice's email is unchanged. *(automated: `TestInvoiceEmail_ZeroDueCopy`)*
- [~] **E4 5xx bounce**: a permanent SMTP 5xx at RCPT → `customers.email_status='bounced'`, `email_bounce_reason` carries the verbatim SMTP error, `email_last_bounced_at` stamped; the outbox row DLQs IMMEDIATELY (permanent — no 15-cycle ramp); the NEXT send to that recipient is refused at ENQUEUE ("recipient suppressed", no outbox row, operator sees the error); editing the customer's email resets `email_status` to `unknown`. *(manual 2026-07-19 against a local real-protocol SMTP peer answering 550 at RCPT — Mailpit/Mailtrap accept everything, so a rejecting peer is the only local 5xx source; the full arc incl. suppression + reset verified.)*
- [~] **E5 Per-provider**: verify SendGrid / Postmark / SES / Mailgun / Resend per `docs/ops/email-setup.md`. *(manual 2026-07-19: **Postmark** verified end-to-end — real STARTTLS send DELIVERED to a Gmail inbox (Status=Sent/Delivered), Server-Token-as-both-username-and-password auth, verified Sender Signature required (an unverified From 250s then async-400s). SendGrid/SES/Mailgun/Resend remain provider-gated — no creds. .env Option 4 documents the Postmark shape.)*
- [~] **E6 Mailpit dev path**: `SMTP_HOST=localhost:1025 SMTP_TLS=none` → mail lands at http://localhost:8025 with HTML+text bodies. *(manual 2026-07-19: multipart verified via Mailpit API — HTML 2.8kB + text 285B on the setup email; the invoice email additionally carries the PDF attachment; payment-update link uses the ?token= query shape.)*
- [~] **E8 Postmark delivery/bounce webhook ingestion (ADR-098)**: with `POSTMARK_WEBHOOK_USER`/`PASS` set, POST a Postmark `Delivery` payload (Basic Auth, `Metadata.vlx-outbox-id` = a real dispatched row's id) to `/v1/webhooks/postmark` → `email_outbox.delivery_state='delivered'`, invoice timeline row reads "— delivered", customer Sent-emails badge shows `delivered`; a `Bounce` (`Type=HardBounce`) → `delivery_state='bounced'` + `customers.email_status='bounced'` + next send suppressed at enqueue; a `SpamComplaint` → `'complained'` (outranks a later bounce — redeliver the bounce after it: state stays `complained`); a `SoftBounce` writes NOTHING; wrong/absent Basic Auth → 401; unknown outbox id / missing metadata → 200 ack, no writes. Outbound leg: every dispatcher-sent email carries `X-PM-Metadata-vlx-outbox-id: <row id>` on the wire. *(automated: `TestPostmarkWebhook_*`, `TestDeliveryState_MonotonicConvergence`, `TestEmailStatus_ComplaintLattice`, `TestCorrelationHeader_OnTheWire`; manual 2026-07-19: Delivery leg + 401 negative + idempotent redelivery exercised against the running server with the REAL Postmark payload — row flipped dispatched/unknown → dispatched/delivered, redelivery no-op)*
- [x] **E8b Live Postmark metadata round-trip**: send via real Postmark (.env Option 4) → `GET api.postmarkapp.com/messages/outbound?count=1&offset=0` shows the message with `Metadata.vlx-outbox-id` echoed; configure the webhook URL (public deploy or tunnel) with Basic Auth in Postmark → Delivery event flips the row to `delivered` with no local curl. *(manual 2026-07-19, WIRE-VERIFIED end-to-end: dispatcher send through real Postmark stamped `X-PM-Metadata-vlx-outbox-id`; Postmark's API echoed `Metadata: {"vlx-outbox-id": …}` verbatim; message hit event **Delivered** in a real Gmail inbox; then — webhook registered via Postmark's Webhooks API against an ngrok tunnel (HttpAuth Basic) — POSTMARK'S OWN INFRASTRUCTURE POSTed the Delivery event through the tunnel and flipped the row pending→dispatched→**delivered in 15s with zero local curl**. Temporary webhook + tunnel torn down after. Deploy-time residue is only: register the PRODUCTION URL in Postmark.)*
- [~] **E7 Receipt + payment-failed land via outbox (no detached goroutine)**: simulate `payment_intent.succeeded` for an invoice. Within ~100ms (before the dispatcher's 5s tick), `SELECT id, email_type, status FROM email_outbox WHERE email_type='payment_receipt' ORDER BY created_at DESC LIMIT 1;` returns a row with `status='pending'`. Wait 5-10s. Re-query: same row now `status='dispatched'`, receipt visible in Mailpit. Same for `payment_intent.payment_failed` → `email_type='payment_failed'`. **Determinism variant** (when timing is tight): from a separate psql session, `SELECT pg_advisory_lock(76540004);` to pause the email dispatcher; fire the webhook; confirm the row sits at `status='pending'` indefinitely; `SELECT pg_advisory_unlock(76540004);` and the row dispatches on the next tick. *(manual 2026-07-19 via the determinism variant: card-charged invoice (credits $2.56 + card $24.94 of a $27.50 total) enqueued `payment_receipt` pending/attempts=0 within ~2s of finalize; the row SAT pending across multiple 5s ticks under the held lock and flipped dispatched:1 on release; receipt in Mailpit says USD 24.94 — the card-captured amount, not the credit-covered total. Zero-due settles (fully credit-covered) correctly send NO receipt. payment_failed twin exercised throughout TC5 dunning flows.)*

## FLOW EX: Streaming CSV exports

- [~] **EX1 customers**: `curl -OJ $API/v1/exports/customers.csv` → `customers-YYYYMMDD-HHMMSS.csv`. Date filter accepts BOTH RFC3339 (`?from=2026-01-01T00:00:00Z&to=2026-12-31T23:59:59Z`) AND bare YYYY-MM-DD (`?from=2026-01-01&to=2026-12-31` — `from` anchors at UTC 00:00:00, `to` at UTC 23:59:59 inclusive). Invalid `from` → 400 with field-level error. Same contract as the audit-log + usage endpoints via `internal/api/timefilter`. **Row-completeness check**: `wc -l customers-*.csv` minus 1 (header row) equals `SELECT COUNT(*) FROM customers WHERE tenant_id='<your tenant>' AND livemode=<mode> AND created_at BETWEEN from AND to` — the export is mode-scoped by RLS, so an unscoped COUNT will not match on a tenant that has both. Verify a tenant with >50 customers exports all of them. **Clock-pinned customers carry SIMULATED `created_at`** (a TC customer created at frozen-time 2028 is invisible to a 2026 date filter) — consistent with every other simulated-time surface, but remember it when a filtered count "misses" rows. *(manual 2026-07-19: both date forms accepted; invalid `from` → 400 with param=from; unfiltered export = 7/7 exact vs mode-scoped COUNT; the date-filter "miss" reproduced and explained by simulated created_at. >50-customer scale leg automated: `TestStreamForExport_IsASnapshot` pins pagination.)*
- [~] **EX2 invoices**: `$API/v1/exports/invoices.csv` → invoice rows incl. amounts, period, lifecycle timestamps. **Row-completeness check**: on a tenant with >100 invoices, `wc -l invoices-*.csv` **minus 1** (the header row) matches `SELECT COUNT(*) FROM invoices WHERE tenant_id='<your tenant>' AND livemode=<mode>` — the export is mode-scoped by RLS, so an unscoped COUNT will not match on a tenant that has both. *(manual 2026-07-19: 36/36 exact on the test tenant; >100 scale leg automated.)*
- [~] **EX3 subscriptions**: `$API/v1/exports/subscriptions.csv` → subs with `plan_ids` (pipe-delimited). *(manual 2026-07-19: 13/13 rows, plan_ids column present; pipe-render on a multi-plan sub not exercised — all subs single-plan.)*
- [x] **EX4 usage-events**: requires `from`+`to`; missing → 400. Span >366d → 400. *(manual 2026-07-19: both 400s verbatim ("requires `from` and `to`", "exceeds 366 days — split into multiple calls"); valid range → 250/250 rows. NOTE: events for a clock-pinned customer are stamped with the CLOCK's frozen instant (EffectiveNow, ADR-086) — export `from`/`to` must cover SIMULATED time, not ingest wall-clock.)*
- [x] Publishable key is **rejected** from exports (403) — publishable keys carry no tenant-wide read scope (empty scope set, `permission.go`). *(manual 2026-07-19: fresh publishable key → 403; wrote NO export audit row.)*
- [~] **Streaming still streams**: on a tenant with a large export (≥ 50k usage events), `curl -N $API/v1/exports/usage-events.csv` prints rows progressively — first bytes arrive in well under a second, not after the whole file is assembled. (The audit observer wraps the status only; a regression test pins that `http.Flusher` survives the middleware chain.) *(scale-gated locally — 250-event tenant streams in ms, which proves nothing about progressive flush; the Flusher regression test is the lock.)*
- [~] **A concurrent write cannot duplicate or drop a row in the export (#475)**: on a tenant with >200 usage events, start `curl -N "$API/v1/exports/usage-events.csv?from=…&to=…" -o u.csv` and, WHILE it streams, POST a handful of new usage events. When it finishes: `cut -d, -f1 u.csv | sort | uniq -d` prints **nothing** (no id appears twice), and the events you posted mid-stream are **absent** — the file is the tenant's data at the moment the export opened, not a smear across the minutes it ran. The same holds for customers / invoices / subscriptions. (Before: each page was its own transaction over a newest-first list, so a concurrent insert shifted the window and the row on every page boundary was written twice.) *(automated: `TestStreamForExport_IsASnapshot`; manual 2026-07-19: no duplicate ids in a real 250-row export — the interleave itself needs a export slower than an insert, unreachable at local N.)*
- [x] **Export columns that were always blank now carry data**: `customers.csv` → the `email_status` column shows `unknown`/`bounced` (not empty) — bounce a customer's email first; `usage-events.csv` → the `origin` column shows `api` for metered events and `backfill` for an operator-backdated one. Both columns shipped empty in every export before this: the exports borrowed each domain's list query, whose SELECT omits them. *(automated: `TestStreamForExport_CarriesEmailStatus`, `TestStreamForExport_CarriesOrigin`; manual 2026-07-19: live export showed email_status 1×bounced + 6×unknown and origin=api on all 250 events.)*
- [x] **EX5 audit-log export (server-side, uncapped)**: `curl -OJ "$API/v1/exports/audit-log.csv"` → `audit-log-YYYYMMDD-HHMMSS.csv`; `wc -l` minus 1 equals `SELECT COUNT(*) FROM audit_log WHERE tenant_id='<t>' AND livemode=<mode>`. Filters mirror the read route (`?resource_type=&action=&actor_id=&resource_id=&date_from=&date_to=`). On a tenant with >50k rows NOTHING is truncated (the dashboard used to page this API in the browser and stop at 50,000 — a silent truncation of the evidence). Same permission as reading the log (`apikey:read`). *(manual 2026-07-19: 390 csv rows = 390 mode-scoped DB rows, exact; `?action=export` filter honored — 15/15 rows.)*
- [x] **EX6 the dashboard Export button hits the server**: Audit Log page → Export downloads the same file with the on-screen filters applied (network tab shows one `GET /v1/exports/audit-log.csv?…`, not a page-walk of `/v1/audit-log`). *(manual 2026-07-19: one `GET /v1/exports/audit-log.csv?action=export` in the network log, file downloaded, filtered content verified.)*
- [x] **EX7 every export leaves exactly one audit row**: run each of the five exports → `/audit-log` shows one row each — "Exported customers (CSV)", "Exported invoices (CSV)", "Exported subscriptions (CSV)", "Exported usage events (CSV)", "Exported the audit log (CSV)" — actor = the operator/key that called it, Resource ID **empty** (a bulk export has no single subject), Details carrying `filename`, `format: csv` and `filters` (`date_range: all` when unfiltered — a whole-table dump says so). Deliberately **no row count**: it is unknowable before the stream starts. *(manual 2026-07-19: 9 successful exports → 9 rows (3 customer, 1 invoice, 1 subscription, 2 usage_event, 2 audit_log), each with its filename; the two 400s and the publishable-403 wrote NOTHING.)*
- [x] **EX8 the audit-log export contains its own export row**: the freshly downloaded `audit-log-*.csv` has, as its newest row, the `export` / `audit_log` row for the export that produced it. (The row is written BEFORE the first byte streams — that is what makes it visible in its own snapshot.) *(manual 2026-07-19: newest csv row = export/audit_log with its own filename in details.)*
- [x] **EX9 fail-closed — an export we cannot record does not happen**: in psql, `REVOKE INSERT ON audit_log FROM velox_app;` then `curl -i $API/v1/exports/customers.csv` → **500**, no `Content-Disposition`, **zero bytes of CSV** (not one page of customer PII). `GRANT INSERT ON audit_log TO velox_app;` restores it. This is why the row is emitted before the stream rather than at completion: a row written at completion is defeated by killing the connection mid-stream. *(manual 2026-07-19: REVOKE → 500, JSON error body, no Content-Disposition, zero CSV bytes; GRANT → 200 restored.)*
- [x] **EX10 spreadsheet formula injection is neutralized**: create a customer whose display name is `=HYPERLINK("http://evil.test","click")` → in `customers.csv` the cell reads `'=HYPERLINK(…)` (quote-prefixed), and in `audit-log.csv` the create row's `resource_label` is likewise prefixed. Open both in Excel / Google Sheets → the value renders as TEXT; no formula executes. Client-built CSVs (credit notes, usage events, credits) neutralize identically — while negative amounts (`-1250`) stay numeric so finance columns still `SUM()`. *(manual 2026-07-19: created `=HYPERLINK(…)` customer via API → customers.csv cell AND the audit-log create-row's resource_label both read `'=HYPERLINK(…)` quote-prefixed.)*

---

## Billing Engine

## FLOW B1: Arrears + proration (default `in_arrears` plans)

Default `base_bill_timing=in_arrears`: the recurring base + any usage settles at period end. Mid-period sub starts prorate the base. See B15 / B16 for `in_advance` variants.

- [x] Plan created without `base_bill_timing` → API returns `base_bill_timing: "in_arrears"`. *(manual 2026-07-19: POST /v1/plans omitting the field → response carried `"in_arrears"` verbatim.)*
- [x] New sub mid-month on this plan → `billing_period_end` = 1st of next month, **NO invoice generated at create time** (cycle path handles it at period close). *(manual 2026-07-19: sub created Jul 19 → `current_billing_period_end = 2026-07-31T18:30Z` = Aug 1 00:00 **in the tenant TZ** (Asia/Kolkata, ADR-058 anchoring); customer had 0 invoices after activate.)*
- [x] Run billing before period close → 0 invoices. *(manual 2026-07-19: POST /v1/billing/run → `{invoices_generated: 0, errors: []}`.)*
- [x] Backdate `current_billing_period_end` (+ `next_billing_at`) → 1 invoice with prorated base + usage + tax + due date + invoice-number prefix. *(manual 2026-07-19: NIM-000125 — base line self-describes "prorated 1/31 days", 3100¢×1/31 = 100¢ exact; +10% tax = 110¢ due; due_at = issued+30d; a second cycle billed the usage: 40 calls × 1¢ = 44¢ w/ tax. SEQUENCING NOTE: events stamped after the backdated period end are correctly EXCLUDED and bill on the next cycle — backdate to a moment AFTER your test events if you want one combined invoice.)*
- [x] Invoice line items: base line's `billing_period_start/end` matches the invoice's (current period). *(manual 2026-07-19: equal to the microsecond on NIM-000125. Line items ride the top-level `line_items` key of GET /v1/invoices/{id} — there is no separate GET route.)*

## FLOW B2: Tax precision (NUMERIC(7,4), ADR-042/043)

Velox stores tax rates at 4-decimal precision (`tax_rate NUMERIC(7,4)`)
matching Stripe Tax's `percentage_decimal` shape. The legacy
`tax_rate_bp bigint` column was dropped in migration 0105 (ADR-043) — <!-- currency-ok: documents the dropped legacy column -->
`tax_rate` is the only rate storage. NYC 8.875%, Quebec 9.975%, Hawaii
4.7120% all round-trip exactly.

- [x] Settings → Tax rate input accepts decimal percent directly (e.g. `8.875`). No bp dance. *(manual 2026-07-19: typed 7.25 then 8.875 into the Tax tab spinbutton — helper text advertises 4dp; live example preview recalculated; saved via Save Changes.)*
- [x] Manual provider: set tax 7.25% in Settings → `tenant_settings.tax_rate=7.2500` (no `tax_rate_bp` column exists). *(manual 2026-07-19: stored 7.2500 exactly; information_schema confirms no %bp% column.)* <!-- currency-ok: states the column was removed -->
- [x] $100 subtotal at 7.25% → `invoices.tax_amount_cents=725, tax_rate=7.2500`. *(manual 2026-07-19: NIM-000133, both values exact.)*
- [x] Manual provider precision: set tax `8.8750` in Settings, invoice a `$100.00` subtotal → `tax_amount_cents=888` (`$8.88`). Engine math is integer parts-per-million: `8.8750%` = `88750 ppm`, `tax = round(subtotal_cents × 88750 / 1_000_000)` = `round(10000 × 88750 / 1_000_000)` = `round(887.5)` = `888` (banker's round-half-to-even). The `1_000_000` is the ppm base, not a percent divisor. No float drift; the 4-decimal rate round-trips exactly. *(manual 2026-07-19: NIM-000135 — 888 on the nose; invoices.tax_rate stored 8.8750.)*
- [x] 3 line items $33.33+$33.33+$33.34 at 7.25%: `SUM(invoice_line_items.tax_amount_cents) = invoices.tax_amount_cents = 725` exactly, AND the residual lands by largest remainder, not on the last line (ADR-046). Exact per-line tax is 241.6425 / 241.6425 / 241.7150; the engine **floors each to 241¢ (sum 723¢), then hands the 2 leftover cents to the largest fractional remainders** — the $33.34 line (.7150) and one $33.33 line (.6425; ties → lowest index) → `242 / 241 / 242`. The $33.34 line must NOT be docked: no line with a larger `amount_cents` may carry a smaller `tax_amount_cents` than a smaller line. *(manual 2026-07-19: NIM-000134 — per-line 242/241/242 exactly, sum 725; the $33.34 line carried 242.)*
- [x] **Stripe-side high-precision case (NYC):** invoice an NY customer (10118 / Manhattan) with `stripe_tax` (needs an NY registration in the Stripe test dashboard) for `$100.00` → `invoice_line_items.tax_rate = 8.8750` + `tax_jurisdiction = US-NY`, `tax_amount_cents = 888`. (Stripe returns `percentage_decimal: "8.875"` in its document-level `tax_breakdown` and leaves the per-line breakdown null — seeded via `TestMapResult_DocLevelRateFallback`.) *(manual 2026-07-20 against LIVE Stripe Tax (NY registration in the test dashboard): NIM-000136 — line tax_rate=8.8750 + tax_jurisdiction=US-NY + tax 888 on $100; real taxcalc_… id AND committed tax_… transaction, tax_status=ok.)*
- [x] **Invoice-level rate is statutory, not effective (ADR-047):** the same invoice's `invoices.tax_rate = 8.8750` (statutory, from the lines) — NOT the effective `8.8800` (`888×100/10000`). Single-rate invoices store the real rate; only multi-jurisdiction invoices fall back to the blended effective rate. *(manual 2026-07-19/20, BOTH variants: manual twin NIM-000135 and live-Stripe NIM-000136 each store invoices.tax_rate=8.8750 statutory while the effective is 8.88.)*
- [x] **Customer-facing display shows `8.875%`:** hosted invoice page and PDF tax line read `Sales Tax (8.875%)` (4-dp, trailing zeros trimmed via `formatTaxRate`), NOT `8.88%`; amount stays `$8.88`. *(manual 2026-07-19: hosted page reads "Sales Tax (8.875%) — $8.88" verbatim; the PDF path shares the same formatTaxRate helper, invoice/pdf.go:540.)*
- [x] **Tax name is a clean label, not the raw Stripe enum:** the tax line reads `Sales Tax`, NOT `sales_tax` (`invoices.tax_name = "Sales Tax"`; Stripe `tax_type` mapped via `taxTypeDisplayName` — `vat→VAT`, `gst→GST`, etc.). *(manual 2026-07-19/20: manual-provider label verbatim; live Stripe Tax also returned the clean "Sales Tax" label on NIM-000136/137 — the sales_tax enum mapping exercised for real.)*
- [x] **Multi-line NY invoice ($40/$35/$25 = $100):** *(manual 2026-07-20 live Stripe Tax: NIM-000137 — per-line 355/311/222 Stripe-verbatim, every line 8.8750 + US-NY, sum = invoices.tax_amount_cents = 888, invoices.tax_rate = 8.8750, committed transaction.)* each line `tax_rate=8.8750` + `tax_jurisdiction=US-NY`; per-line `tax_amount_cents` 355/311/222 (Stripe verbatim) sum to `invoices.tax_amount_cents=888`; `invoices.tax_rate=8.8750`.
- [x] **Proration math uses integer day-ratio (B7.4):** mid-cycle plan upgrade on a 30-day period with 18 days remaining → proration line item amount = `(new_amount - old_amount) × 18 / 30` exactly (banker's rounded). No `float64` ULP drift visible on amounts up to ~$36M. *(manual 2026-07-20, FLOW B7 walkthrough: 3000×18÷30 = 1800 exact; 3001×18÷30 = 1800.6 → 1801; 3001×15÷30 = 1500.5 → 1500 (half-even); automated coverage in the proration audits.)*
- [x] **Proration tax carries the provider and is committed (B7.5):** on a `stripe_tax` (or `manual`) tenant, the mid-cycle upgrade's proration invoice has `tax_amount_cents` computed on the prorated **net** AND a non-blank `tax_provider` matching the create invoice. For `stripe_tax`, the calculation is committed to a tax transaction (`tax_transactions` row / `invoices.tax_transaction_id` set), the same as a cycle invoice — so the proration tax is reported, not just charged. *(manual 2026-07-20, FLOW B7 walkthrough: `manual` half exact — 1800 net → 180 tax, provider/status stamped. `stripe_tax` half walked LIVE post-#556-fix: upgrade proration invoice computed by real Stripe Tax on the net (1300 → 115 at 8.875% US-NY), `tax_calculation_id` AND `tax_transaction_id` both set, split lines carrying the Stripe-sourced partition (−77/+192).)*
- [x] **Proration carries the reverse-charge / exempt status:** on a customer with `tax_status=reverse_charge` (or `exempt`), the mid-cycle upgrade's proration invoice has `tax_amount_cents=0` AND `tax_reverse_charge=true` (or `tax_exempt_reason` populated), so the proration invoice's PDF shows the reverse-charge / exemption legend — same as the customer's cycle invoices. Pre-fix the proration invoice came out with `tax_reverse_charge=false` / blank reason, dropping the legend on that one invoice. *(manual 2026-07-20, FLOW B7 walkthrough: RC customer's upgrade proration invoice — net 1500, tax 0, `tax_reverse_charge=true`; its cycle prebill likewise.)*
- [x] **Exempt/zero-tax DOWNGRADE clawback — no tax slice, no upstream reversal (B7.6b):** on a `tax_status=exempt` (or `reverse_charge`) customer whose Pro prebill was $0 tax, downgrade Pro→Starter mid-period. The clawback credit note is **net-only** — `total_cents == subtotal_cents`, `tax_amount_cents=0` — and **no Stripe Tax reversal is filed** (the source invoice has no `tax_transaction_id`, so `ReverseTax` is skipped). Contrast B7.6's taxed source, which files a reversal. PAID source → balance credit; UNPAID source → `amount_due` drops and the balance is unchanged. *(manual 2026-07-20, FLOW B7 walkthrough at 15/30 on a stub period: CN total==subtotal==1500, tax 0; balance credited $15.00.)*
- [x] **Clawback reverses what was CHARGED, not the current exemption status (B7.6c):** charge the Pro prebill while the customer is `standard`. THEN set the customer `tax_status=exempt`. THEN downgrade Pro→Starter mid-period on the paid prebill. The credit note is **net + tax reversed by the *source invoice's* ratio** (`FindBaseInvoiceForPeriod`), not by today's exempt status; on `stripe_tax` a reversal is filed against the source transaction (automated in the clawback arc). The exempt flip only affects FUTURE invoices. (Trap: expecting $0 / no reversal "because the customer is exempt now" — wrong; reversal is anchored to the source.) *(manual 2026-07-20, FLOW B7 walkthrough on `manual` at 15/30: prebill $33.00 charged while standard → flip exempt → downgrade → CN 1500 net + 150 reversed = 1650 gross; balance $16.50 — source-anchored despite the flip.)*

## FLOW B2b: Per-unit rate precision (decimal, ADR-045)

Per-unit pricing rates are decimal cents (Stripe `unit_amount_decimal` shape) so
sub-cent rates bill linearly. Fixed fees and invoice line amounts/totals stay
whole cents — only the RATE gains precision.

- [x] Pricing → new flat rating rule, price `$0.000003` per unit → saves (input is not clamped to 2 decimals, not rounded to `$0.00`). *(manual 2026-07-20: created via the Add Rule dialog — which itself advertises "Sub-cent rates allowed" — 201.)*
- [x] Rule detail renders the sub-cent rate (`$0.000003`), not `$0.00`. *(manual 2026-07-20: Rules table price cell reads $0.000003.)*
- [x] `GET /v1/rating-rules/<id>` → `flat_amount_cents` is a JSON string (`"0.0003"`), not a number. *(manual 2026-07-20: create response + list both carry the string "0.0003".)* It's a decimal per-unit rate **in cents** — `0.0003`¢/unit — which is what enables sub-cent linear pricing.
- [x] Meter on that rule + customer with 1,000,000 usage units + cycle close → usage line `amount_cents=300` ($3.00) exactly — i.e. `0.0003`¢/unit × 1,000,000 units = `300`¢ (not `0`, which is what rounding the rate to int cents would give; not `300000000`, which is billing `300`¢ *per unit*). *(manual 2026-07-20: NIM-000138 — qty 1,000,000 → amount_cents=300 exact via a clock cycle.)*
- [x] **Unit Price column shows the full-precision rate, not `$0.00` (ADR-054)**: that usage line's Unit Price reads the effective rate (`$0.000003`), not `$0.00`, on the invoice detail page, the PDF, and the public hosted page — `GET /v1/invoices/{id}` line carries `unit_amount_decimal` (e.g. `"0.0003"` cents) alongside the whole-cent `unit_amount_cents`. Quantity × Unit Price reconciles with Amount. (Try the screenshot case too: 1,000 units billed `$3.00` → Unit Price `$0.003`, not `$0.00`.) *(manual 2026-07-20: dashboard invoice page AND hosted page both render $0.000003; the API line carries unit_amount_decimal "0.0003" beside unit_amount_cents 0. The $0.003 parenthetical rides the same formatter.)*
- [x] Instantiate `anthropic_style` → `c35_sonnet_input` stored as `0.0003` cents/token; 1,000,000 input tokens bill `300`¢, not `$3,000,000`. *(manual 2026-07-20: rule stored flat "0.0003"; NIM-000139 line "Tokens (claude-3.5-sonnet · input)" qty 1M → 300¢. GOTCHA: the event property must be the FULL model string ("claude-3.5-sonnet") — a shorthand ("c35_sonnet") matches no matrix rule and bills NOTHING. Two surfaces make that loud: the engine WARN at cycle close, and (2026-07-20 fix, same walkthrough) an **amber "unmatched usage — not billed" row on the customer usage card** — first-class in GET /v1/customers/{id}/usage (`unmatched: true`, excluded from totals) and deliberately FILTERED from the public cost-dashboard projection. Ingest stays permissive on purpose: unpriced events are retained, so adding the missing matrix rule before period close bills them — rejection would destroy both the telemetry and the revenue.*
- [x] **Recipe apply never adopts a colliding plan — it mints a born-unique one (ADR-085, supersedes ADR-083):** on a fresh tenant, hand-create a plan with code `ai_api_pro` (any config, e.g. `base_bill_timing=in_advance`), then `POST /v1/recipes/anthropic_style/instantiate` → **201**. The recipe's generated plan lands at `ai_api_pro_2` (first free `<code>_N`), fully wired to the recipe's meter/rules; the hand-created `ai_api_pro` plan is untouched (not mutated, not renamed, not read for comparison) — there is no 409 and no conformance check on plans anymore. *(manual 2026-07-20: hand-made ai_api_pro ($10 base) pre-created → instantiate 201 → recipe plan landed at ai_api_pro_2 wired to the tokens meter; the hand-made plan untouched.)*
- [x] **Meter aggregation-mismatch still refuses loud (ADR-085 meter guard, the one remaining adoption gate):** on a fresh tenant, hand-create a meter with key `tokens` and `aggregation=max` (the recipe declares `sum`), then instantiate `anthropic_style` → **409** naming the diverging field (*"usage aggregation: recipe wants sum, existing is max"*), and nothing persists — any rating rules adopted/created earlier in the same call roll back with it (`rating_rule_versions`/`recipe_instances` stay at their pre-call count). Fix the meter's aggregation, or use a fresh tenant, → instantiate succeeds. *(manual 2026-07-20: tokens meter pre-created with aggregation=max → 409 verbatim "usage aggregation: recipe wants sum, existing is max", rating-rule count unchanged; PATCHed to sum → instantiate 201, existing meter ADOPTED (recipe plan wired to its id).)*

## FLOW B3: Idempotency

> **Automated coverage: 11 / 11.** `[x]` items are locked by the named test (run `go test -race -short=false ./internal/billing/ ./internal/subscription/`); `[ ]` items are pending. Idempotency/atomicity/concurrency can't be hand-run reliably, so these are verified by automated tests, not manual passes. *(battery re-run at HEAD 2026-07-20: both packages green under -race — billing 35s, subscription 15s.)*

- [x] Run billing twice in same period → no duplicate invoice. Logs `invoice already exists for billing period (idempotent skip)`. *(automated: `TestBilling_SamePeriodTwice_IdempotentSkip`; concurrent twin `TestConcurrentBilling_ExactlyOneInvoice`)*
- [x] **Multi-add proration in same period (ADR-030 cross-flow audit)**: pick a clock-pinned active sub. AddItem with plan A — succeeds, proration invoice DEMO-NNNN created. AddItem with plan B at the same simulated instant — also succeeds, distinct proration invoice DEMO-NNNN+1 created with the same `billing_period_start/end` as the first proration invoice but different `source_subscription_item_id`. `idx_invoices_billing_idempotency` correctly exempts both (predicate `WHERE source_plan_changed_at IS NULL`); `idx_invoices_proration_dedup` correctly distinguishes them by item id. *(automated: `TestProrationInvoiceIndexes`)*
- [x] **AddItem rolls back atomically on a proration-write failure (ADR-030)** — on a clock-pinned active sub whose current-period prebill is **already settled** (an unpaid prebill 409s on `unpaid_invoice_blocks_change` *first* — settle or void it), simulate a proration-write failure with `REVOKE INSERT ON invoices FROM velox_app` (the **runtime** role — `cmd/velox` swaps `DATABASE_URL`→`velox_app`; revoking from `velox` is a no-op, it's a superuser), run AddItem, then `GRANT INSERT ON invoices TO velox_app`. Expect a **non-2xx error** AND zero rows in `subscription_items` for the new plan — the item add + proration roll back together. *(automated: `TestAddItem_ProrationInvoiceCreateFailure_RollsBackItemAdd`)*
- [x] **`proration_failed` is the non-atomic fallback, not the atomic outcome** — that code ("item add succeeded but proration generation failed — item is on the subscription") appears only when the atomic coordinator isn't wired, and there the item STAYS on the sub. The production default is the atomic rollback above. *(verified by the #3 test above — atomic path errors + rolls back, never `proration_failed`)*
- [x] **UpdateItem + RemoveItem atomicity (same ADR-030 follow-through)**: same shape, different mutations. UpdateItem quantity-INCREASE with simulated proration-write failure (`REVOKE INSERT ON invoices FROM velox_app`) → a **non-2xx error** + item.quantity rolled back to the pre-change value. **Cross-interval plan swap is now atomic too (ADR-056)** — the plan write + watermark advance + new-period invoice commit in one tx; a simulated failure on the new-period bill rolls back the plan write AND the watermark (see FLOW B21). *(automated: qty-increase rollback `TestUpdateItem_ProrationInvoiceCreateFailure_RollsBackQuantity`; cross-interval swap `TestUpdateItemTx_CrossIntervalSwap_RealTxRollsBackOnBillFailure`)*
- [x] **Downgrade/removal on a paid source issues a clawback credit note inside the item-change tx (ADR-057)** — a downgrade / qty-decrease / RemoveItem on a PAID source creates a tax-reversing clawback CN as a **DRAFT inside the tx** (`issue_pending`), issued post-commit. *(automated: `TestUpdateItem_Downgrade_RoutesGrossTaxReversingCreditNote`, `TestUpdateItem_QuantityDecrease_RoutesGrossCreditNote`)*
- [x] **A clawback draft-create failure rolls back the item change** — `REVOKE INSERT ON credit_notes FROM velox_app` (the clawback writes `credit_notes`, not `customer_credit_ledger`), RemoveItem a paid-source item → the item is **still present** (not deleted-with-no-credit). *(automated: `TestRemoveItem_ClawbackDraftCreateFailure_RealTxRollsBackItemDelete`)*
- [x] **An `Issue()`-never-ran clawback is recovered by the sweep** — if the post-commit `Issue()` never runs (kill the process right after commit), the draft stays `status='draft' AND issue_pending=true`, and `RetryPendingClawbackIssue` re-issues it on the next scheduler tick. *(automated: `TestRetryPendingClawbackIssue`)*
- [x] **`Issue()` is atomic — no half-issued clawback to reconcile by hand (ADR-061)** — the `draft→issued` flip and the internal grant/`amount_due` effect commit on one tx, so no `status='issued'` row carries an un-applied internal effect; a post-flip *external*-leg failure (refund / tax reversal) self-heals via `RetryRefund` / `RetryPendingCreditNoteTaxReversal`. *(automated: `TestIssue_GrantFailure_RollsBackCAS`, `TestIssue_IdempotentNoDoubleApply`, `TestIssue_CASLoserDoesNotApply`)*
- [x] **RemoveItem soft-delete**: pick an item that has at least one proration invoice or credit-ledger entry pointing at it (e.g. an item added mid-cycle with proration emitted). DELETE the item via `/v1/subscriptions/{id}/items/{itemID}` → 200 (NOT 500 with the FK-violation error). `GET /v1/subscriptions/{id}` shows the item gone from `sub.Items`. `psql -c "SELECT id, deleted_at FROM subscription_items WHERE id='<the id>'"` returns one row with `deleted_at IS NOT NULL`. Re-adding the same `plan_id` to the same sub succeeds (the partial unique index `WHERE deleted_at IS NULL` allows it). *(automated: `TestRemoveItem_SoftDeleteIsFKSafeAndReAddable`)*
- [x] **Exactly-once invoice under a concurrent race** — the checkboxes above test idempotency *sequentially*; the concurrent twin is `TestConcurrentBilling_ExactlyOneInvoice`: N billing runs hit one due sub simultaneously → exactly **1** invoice, the losers collide on `idx_invoices_billing_idempotency` and take the idempotent-skip (no surfaced error). Run: `go test -race -short=false -run TestConcurrentBilling ./internal/billing/`.

## FLOW B4: Auto-charge retry

- [x] **A card-less proration invoice gets ONE setup-link email from the sweep (ADR-087)** — customer with no payment method, active sub; increase item quantity mid-period (proration invoice created, `auto_charge_pending=true`, no email yet). Across two sweep ticks: Mailpit shows exactly ONE "Set up payment" email, and `invoices.no_pm_notified_at` is stamped. Attach a card → next tick charges it. A finalize-time-emailed invoice gets NO second email from the sweep. *(manual 2026-07-20 on wall-clock ticks: qty 1→2 on a settled in_advance prebill → proration NIM-000141 sat unemailed until the next tick sent exactly ONE email + stamped no_pm_notified_at; across 2+ further ticks the count never moved and the finalize-notified sibling got no second email; real Stripe Checkout card attach → the following tick auto-charged it (succeeded, acp=false).)*

- [x] Decline-on-charge card → invoice has `auto_charge_pending=true`, `payment_status=failed`, and a dunning run opens (`reason=payment_failed`). *(manual 2026-07-20 with Stripe test card 4000…0341 attached via real Checkout + webhook: activation prebill declined → payment_status=**failed** (the pre-dunning-arc "pending" claim was stale), acp stayed true, dunning run created with the policy's next_action_at.)*
- [x] Update card → recovery. **Post-decline retries are DUNNING-owned (ADR-064), not sweep-owned**: the 5-minute sweep never re-charges a `failed` invoice — the dunning run retries on its policy schedule (observed next_action_at = +3d), and the immediate paths are operator **Collect Payment** or the customer's hosted page. *(manual 2026-07-20: attached 4242 via Checkout, promoted to default, watched 2+ sweep ticks correctly NOT retry; operator collect → paid on the new card AND the dunning run auto-resolved `payment_recovered` (#328 resolve-on-settle). The old "next scheduler tick → succeeded" claim predated the dunning arc. Cosmetic residual: `auto_charge_pending` stays true on paid invoices — inert, every charge path keys on unpaid status.)*

## FLOW B5: Idempotency-Key header

- [x] POST with `Idempotency-Key: test-123` → 201. *(manual 2026-07-20: POST /v1/customers with a fresh key → 201.)*
- [x] Same body + key → same response, 1 row. *(manual 2026-07-20: replay returned the SAME customer id, and exactly 1 row exists; the replay also self-declares to the audit-coverage detector so it never reads as an uncovered mutation.)*
- [x] Same key + different body → **422 `idempotency_error`** (Stripe-parity, message verbatim: "Keys for idempotent requests can only be used with the same parameters they were first used with."). The 409 this box used to claim is the SEPARATE `conflict_idempotency` response — same key while the FIRST request is still in flight → 409, client retries. *(manual 2026-07-20: 422 observed with the exact message; the 409 in-flight leg is code-verified in middleware/idempotency.go — two distinct statuses for two distinct situations, richer than the old claim.)*

## FLOW B6: Subscription lifecycle

- [x] **Create-subscription picker finds ANY customer (P11)** — with 51+ customers, typing the 51st customer's name (or email/external id) in the New Subscription dialog finds them; a customer with no prior subscription is selectable. *(manual 2026-07-20: seeded to 53 customers (the zz_seed_* rows are this fixture — keep them); searched "Needle" in the picker → the newest never-subscribed customer found and selectable — server-side search, no 50-row client cap.)*
- [x] **Meter-overlap guard (double-billing, 2026-07-05):** customer has a live sub on a plan billing meter M → creating a second sub with a *different* plan that also bills M → 409 naming the existing sub code + meter ("usage would be invoiced twice"). Same 409 on AddItem and on a plan swap (immediate or scheduled) onto M. A sub with a disjoint meter set → allowed. Cancel the first sub → the meter is billable again. *(manual 2026-07-20: all four legs exact — 409 names the sub code + meter + "usage would be invoiced twice" + remediation; disjoint 201; AddItem 409; post-cancel 201. Nit: the 409 prints the meter ID (vlx_mtr_…), not its key — actionable via the sub code but jargon-y.)*

- [~] Trial 7 days → no charge during trial; status flips to active AT `trial_end_at` (Phase 0.5 / cron, ADR-037); first invoice fires at activation for in_advance items or at the post-trial cycle close for in_arrears. Full coverage in FLOW TC6. *(2026-07-20: create leg re-verified live — trialing, trial_end_at=+7d, 0 invoices; the flip-at-end + first-invoice legs are TC6's, verified in the 2026-07-18 parity pass.)*
- [~] Pause button on a `status=active` sub → opens **Pause collection** confirm dialog (the hard-pause radio option was removed in PR-6). Click through → cycle keeps drafting invoices, auto-charge is suppressed, no dunning fires on the resulting drafts. Resume collection → next cycle bills normally; drafts stay drafts unless operator finalizes them manually. *(manual 2026-07-20: dialog + click-through verified — pause persists {behavior: keep_as_draft} (ADR-036) and resume clears it; BONUS undocumented-in-this-flow feature observed: an "Auto-resume on (optional)" date. The cycle-under-pause drafting behavior needs a clock ride — not re-run here.)*
- [x] Pause Collection confirm dialog description includes the bolded line **"On resume, the full current period bills — paused days are not pro-rated. Issue a credit grant after resuming if you want to offset them."** (truth-in-labelling fix shipped 2026-05-18; pause_collection is about charging, not about cycle-skip — full month bills on resume). *(manual 2026-07-20: verbatim in the dialog.)*
- [x] Cancel from `status=trialing` works — `trial_end_at` is preserved across cancel for historical reporting, `canceled_at` stamps in simulated time on clock-pinned subs. *(manual 2026-07-20 wall-clock leg: canceled a trialing sub — status=canceled, trial_end_at PRESERVED verbatim, canceled_at stamped; the simulated-time stamp leg is the TC flows'.)*
- [x] Cancel on `in_arrears` plan → confirm dialog → status canceled, no future billing, no credit grant. *(manual 2026-07-19/20: the B1 walkthrough sub canceled with ZERO credit-ledger rows for its customer.)*
- [x] Cancel on `in_advance` plan mid-period → confirm dialog → status canceled AND a credit grant lands on the customer's balance for the unused portion of the already-billed period. The documented description now lives on the **credit-note line** (the cancel is CN-mediated — ADR-048 family): the CN line reads `Cancel proration — unused portion of <sub_code> base fee (period <start> to <end>, canceled <date>)` VERBATIM, while the ledger grant references `Credit note CN-XXXXXX — subscription_cancellation`. See B17 for the full flow. *(manual 2026-07-20: paid prebill 426¢ → same-day cancel → issued CN + 426¢ grant (whole period unused → full credit, day-granular math exact).)*
- [~] Cancel on `in_advance` plan AT or AFTER `current_period_end` → no proration credit (period was used in full). *(automated: `TestRunCycle_CancelAtEqualsBoundary_TieGoesToBoundaryPath` + `TestRunCycle_CancelAtPeriodEnd_FiresAtBoundary` pin the boundary path, and the day-ratio credit tests (15/30→2450¢) make 0-days-unused → 0¢ the same formula; a live boundary ride needs a clock.)*

## FLOW B7: Plan change + proration

- [x] **Tax-deferred proration invoice auto-retries:** with Stripe Tax made to fail, an immediate plan change produces a DRAFT proration invoice whose API row carries `tax_status=pending`, a non-empty `tax_error_code` (e.g. `provider_auth`, `customer_data_invalid`), `tax_pending_reason`, and `tax_deferred_at`; fix the cause → the tax-retry worker finalizes it within a tick (next clock Advance on pinned subs) — no manual Retry click needed. *(manual 2026-07-20: walked with two REAL failure causes — missing billing-profile country (`customer_data_invalid`, reason verbatim "customer has no country on billing profile", `tax_deferred_at` in simulated time), then provider `api_error`; after the fix the next Advance finalized it: draft→finalized, tax ok, zero clicks. The #556 bug this leg exposed — retries re-sent the stored split pair's negative line, which Stripe Tax 400s → permanent deferral — is FIXED (negative lines collapse to the net at the `ApplyTaxToLineItems` chokepoint): a seeded stuck split invoice walked through operator Retry tax against LIVE Stripe → finalized at 8.875% US-NY with the create-time per-line partition restored exactly.)*

- [x] In_arrears sub upgrade immediately → no immediate proration invoice/credit; cycle close emits per-segment lines (FLOW B20). *(manual 2026-07-20: timeline shows "A → B · Immediate" with NO proration artifact; Invoices (0).)*
- [x] In_arrears sub downgrade immediately → no immediate credit grant; cycle close emits per-segment lines. *(manual 2026-07-20: same sub downgraded back — 0 invoices, credit ledger empty, item restored.)*
- [x] In_advance sub upgrade immediately + source invoice paid → immediate proration invoice for the delta lands in customer's invoices, with `auto_charge_pending=true`. It **auto-collects** like a cycle invoice — a customer with a saved card is charged on the next scheduler tick (wall-clock subs) or the next clock **Advance** (clock-pinned subs); it does NOT sit at `pending` forever waiting for a manual Collect Payment. *(manual 2026-07-20: card attached via the operator setup-email → Stripe-hosted checkout (4242); the invoice page shows the "Auto-charge scheduled — runs on the next test-clock advance" banner, and the next Advance collected it: paid/succeeded, `paid_at` at the simulated instant, `auto_charge_pending` retired (#553).)*
- [x] **Upgrade invoices as TWO lines (B7.7, ADR-048 Phase C):** the upgrade proration invoice shows a **negative** credit line *"Unused time on Starter (after &lt;date&gt;)"* and a **positive** charge line *"Remaining time on Pro (after &lt;date&gt;)"* — NOT one net line. Amount due equals the prior single-line net. For the 18/30 Starter→Pro example: credit **−$12.00** (`2000×18÷30`) + charge **+$30.00** (`5000×18÷30`) = **$18.00** net. On a taxed sub each line carries its own tax (the credit's is the negative reversed slice); the two per-line taxes sum to the invoice tax and the dashboard/PDF totals are unchanged. Item-add still shows a single net line. *(manual 2026-07-20: exact — lines verbatim "Unused time on B7 Starter (after Jun 13, 2027)" −$12.00 / "Remaining time on B7 Pro (after Jun 13, 2027)" +$30.00; per-line tax −$1.20/+$3.00 sums to the invoice's $1.80; totals $18.00/$1.80/$19.80.)*
- [x] **Quantity increases as TWO lines:** raise a $10.00/seat item from 1 → 3 seats at exactly half period. The proration invoice shows *"Unused time on 1 × Seat (after &lt;date&gt;)"* **−$5.00** (qty 1, unit −$5.00) and *"Remaining time on 3 × Seat (after &lt;date&gt;)"* **+$15.00** (qty 3, unit **$5.00** — the true prorated per-seat rate), net **$10.00**. On BOTH lines qty × unit price = amount exactly. *(manual 2026-07-20: exact at Jun 16 on a Jun 1–Jul 1 period — qty 1 × −500 = −500 and qty 3 × 500 = +1500 on both lines; per-line tax −50/+150.)*
- [x] **The plan-change proration invoice discloses simulation on a test clock (ADR-099):** on a clock-pinned in_advance sub, advance the clock and upgrade the plan immediately. The resulting proration invoice's detail page carries the amber test-clock **banner** (the page's single disclosure), and its Invoices-list row shows the **Simulated** chip, exactly like the sibling cycle invoice (`invoices.is_simulated = true` in DB). *(manual 2026-07-20 walked with the pre-ADR-099 header pill; 2026-07-21 the header pill was removed as redundant with the banner — banner + list chip are the disclosures.)*
- [x] **Exact integer day-ratio amount (ADR-042)**: on a clock-pinned in_advance sub whose 30-day period (Jun 1 → Jul 1) base invoice is paid, advance the clock to Jun 13 (18 of 30 days remain), then immediately upgrade Starter ($20.00) → Pro ($50.00). The proration invoice's net is **exactly $18.00** — `(5000−2000)×18 ÷ 30 = 1800`¢, banker's-rounded, no float drift (rendered as the −$12.00 / +$30.00 two-line pair, B7.7; on a taxed tenant amount due = net + tax). With Pro at $50.01 the net is **$18.01** (`3001×18 ÷ 30 = 1800.6`, rounds up); downgrading Pro → Starter at 18/30 yields a **−$18.00-net** credit (not an invoice — on a taxed source it lands gross, B7.6). *(manual 2026-07-20: net 1800 exact; 5001-variant net 1801 exact (the proration API response now reports the GROSS outcome — B12 adjudication); downgrade credited $19.80 gross = $18.00 net × the source's 55/50 ratio; bonus half-even case observed: 3001×15÷30 = 1500.5 → **1500**.)*
- [x] **Stub period prorates against the FULL cycle, not the partial period.** Sign a customer up **mid-month** so the first period is a stub (e.g. start Apr 17 → period Apr 17–May 1, a 14-day stub of a 30-day monthly cycle); pay the day-1 invoice (it shows "prorated 14/30 days"). Advance ~1 day in (13 of the stub's days remain) and immediately upgrade Starter ($20) → Pro ($50). The proration invoice is **$13.00** — `(5000−2000)×13 ÷ 30`, the full 30-day cycle — **not** $27.86 (`×13/14`, the stub length). Regression for the DEMO-012094 over-charge: the upgrade denominator must match the day-1 stub's `/30`. *(manual 2026-07-20: day-1 line verbatim "prorated 14/30 days" (933¢); upgrade at Apr 18 → 1300¢ net exact.)*
- [x] **Upgrade on an UNPAID in_advance prebill is blocked (B7.8, ADR-050)** — on a clock-pinned in_advance sub whose current-period prebill is finalized but **unpaid** (no PM / declined), an immediate **upgrade** (or add-item / qty-increase) is **rejected with 409** (`unpaid_invoice_blocks_change` — the message names the invoice, the outstanding amount, and "settle or void it before changing this subscription") and the item is left unchanged (no second receivable stacked). Contrast a **paid** source, where upgrade invoices the delta (B7.6). *(manual 2026-07-20: UI attempt surfaces the 409; API replay verbatim "invoice NIM-000147 for the current period is unpaid (22.00 USD outstanding); settle or void it before changing this subscription"; item still on Starter.)*
- [x] **Downgrade on an UNPAID in_advance prebill proceeds and relieves the open invoice** — an immediate **downgrade** (or removal / qty-decrease) issues a tax-reversing **adjustment credit note** against the unpaid prebill, dropping its `amount_due` by the unused gross (capped at amount due); the change response shows `proration.type=adjustment` and the customer credit balance is **unchanged** (nothing was funded → no refundable credit). Same relief as the unpaid-cancel path (TC8b); contrast a **paid** source, where downgrade credits the balance (B7.7). *(manual 2026-07-20: `proration.type=adjustment`, `amount_cents=1980`; prebill due 5500→3520, total unchanged; balance 0.)*
- [x] **Downgrade on a TAXED paid prebill reverses proportional tax (B7.6, ADR-048):** repeat the 18/30 downgrade Pro → Starter on a `stripe_tax`/`manual` tenant whose Pro prebill was taxed (net $50 + 10% = $55 paid). The clawback is issued as a **credit note** against the paid source invoice — the customer's balance is credited the **gross** unused (net $18.00 grossed up by the source invoice's `Total/Subtotal` ratio = **$19.80**), and the credit note carries the proportional reversed tax ($1.80; `stripe_tax` files a reversal against the source `tax_transaction`). The change response shows `proration.type=credit` with `amount_cents` = the gross. Same shape for a **quantity decrease** and an **item removal**. Zero-tax prebills are unchanged (gross == net, still via the credit note). *(manual 2026-07-20 on `manual`: CN `subscription_downgrade` 1800+180=1980 against the paid prebill; timeline "Credit $19.80"; balance $19.80. The `stripe_tax`-reversal half stays automated (clawback arc); the #556 blocker on stripe_tax proration invoices is fixed.)*
- [x] Scheduled plan change (`immediate=false`) → no immediate artifact; engine emits closing invoice under OUTGOING plan at period boundary (FLOW B20). *(manual 2026-07-20: Pending Change chip "→ B7 Pro · Jul 1, 2027" with a Cancel affordance; item + Next Invoice Preview unchanged; no artifact.)*
- [x] Plan change across `base_bill_timing` rejected with 422 (`bill_timing change is not supported on plan-swap (current in_advance, new in_arrears); cancel the subscription and start a new one with the target plan`) — both immediate and scheduled. Velox rejects this for billing-safety — an in-place advance↔arrears swap mid-cycle creates prepay/postpay overlap or refund-then-recharge edge cases. Zuora rejects it the same way; Stripe ALLOWS it in-place (bills the new price forward, offloading the overlap to the operator). Operator path: cancel + recreate. *(manual 2026-07-20: 422 verbatim, both modes. The Change-Plan dialog now pre-filters cross-timing plans with an explanatory note — this PR.)*
- [x] Immediate same-cadence cross-interval plan-swap (yearly → monthly or monthly → yearly, both in_advance OR both in_arrears) accepted — see FLOW B21. *(manual 2026-07-20: monthly→yearly accepted — re-anchored Jun 13 2027→Jun 12 2028, $264 yearly invoice, unused-Starter refund CN $13.20 (`subscription_plan_change`).)*
- [x] Plan billing-fields immutability (ADR-034): live-sub plans reject `PATCH` to `base_amount_cents` / `base_bill_timing` / `meter_ids` with 422; `name` / `description` / `tax_code` / `status` mutate cleanly. *(manual 2026-07-20: see next box.)*
- [x] **Plan billing-fields immutability (ADR-034)**: with at least one live sub on a plan, `PATCH /v1/plans/{id}` with a different `base_amount_cents`, `base_bill_timing`, or `meter_ids` → **422** with message naming the blocked field(s) + live-sub count + "Create a new plan instead." Display-only fields (`name`, `description`, `tax_code`, `status`) STILL mutate cleanly on the same call. On a plan with zero live subs, all fields are mutable (covers typo correction at plan creation). Canceled / archived subs do NOT count as live for the guard. *(manual 2026-07-20: 422 verbatim "cannot change billing-affecting field(s) [base_amount_cents]: 3 live subscription(s) reference this plan. Create a new plan instead…"; name/description 200 on the same plan; zero-sub plan mutable; live→422, cancel that sub→200.)*

## FLOW B8: Usage caps

- [x] `usage_cap_units=5000`, `overage_action=block`, ingest 8000 → billed 5000. *(manual 2026-07-20: clock-pinned sub, 8000 ingested at simulated time, cycle close billed qty 5000 → 10000¢ @2¢/unit.)*
- [x] A sub created with `overage_action=charge`, ingest 8000 → billed 8000. *(manual 2026-07-20: 16000¢ @2¢. Cap + overage action are CREATE-time settings — no update endpoint, the sub page shows them read-only — so the contrast is a second sub, not an in-place switch.)*
- [x] **Fractional cap-scaled quantity keeps its exact decimal on the line**: when a multi-meter cap scales a meter to a fractional quantity (e.g. 1.5 units), the usage line's `quantity_decimal` is the exact `1.5` (`GET /v1/invoices/{id}` and the PDF show `1.5`, not truncated `1`); `amount_cents` is unchanged. *(manual 2026-07-20: cap 6 over meters 2+6 → factor 0.75 → lines 1.5 and 4.5 exact on API, dashboard Qty column, AND the rendered PDF; amounts 3¢ (1.5×2¢) and 4¢ (4.5×1¢ = 4.5¢, banker's — the ADR-045 sub-cent path observed live).)*

## FLOW B9: Customer price overrides

- [x] **Customer page "Price overrides" card** (shipped with this walkthrough): lists each active deal as negotiated-vs-list ("$0.01/unit · list price $0.02/unit" — the list contrast tracks the LATEST version), with reason + since-date; "Add override" opens a dialog with a searchable rule picker (each option shows its current list price), flat/graduated/package fields, and the bolded next-period notice; ending a deal confirms with "List price applies from the next billing period." *(manual 2026-07-20: full loop walked — view, end, re-create via dialog; picker searchable across 39 rules incl. sub-cent AI-token rates.)*
- [x] Create an override (UI card or POST /v1/price-overrides) → that customer's invoice uses the override price. *(manual 2026-07-20: list 2¢, override 1¢, 1000 calls → 1000¢ net (NIM-000171). Edge pinned: an override created at the period-open INSTANT counts for that period (inclusive boundary).)*
- [x] **Usage view == invoice for an overridden customer (ADR-070/P10)** — `GET /v1/customers/{id}/usage` shows the negotiated amount for an overridden customer (and list price for others). *(manual 2026-07-20: card 1000¢/2000¢ matched invoices to the cent in every period — INCLUDING through catalog changes; view==invoice is the invariant that held everywhere.)*
- [x] Other customers → default rule price. *(manual 2026-07-20: sibling billed 2000¢ (NIM-000172).)*
- [x] **Override survives a rule publish (ADR-070)** — create a new version of the overridden rule (same `rule_key`), close the cycle → the invoice still bills the override price, not the new version's list price. *(manual 2026-07-20: v2 published at 3¢ via the Pricing UI; overridden customer's next close still 1000¢ (NIM-000173); the card's list-contrast updated to $0.03 automatically.)*
- [~] **Rate changes price from the NEXT period (wall-clock subs)** — a version published (or an override created/edited/deleted) mid-period does not change the in-flight period; the following period bills the new rate. *(2026-07-20: NOT live-walkable on clock-pinned subs — catalog stamps are WALL-clock, so any change made during a simulation predates every simulated period-open and backdates (observed: mid-"October" publish billed October at v2; ADR-070's test-clock caveat). view==invoice stayed coherent throughout. The wall-clock next-period semantics are locked by ADR-070's mutation-verified test matrix.)*
- [x] **DELETE /v1/price-overrides/{id}** (or the card's "End override") → the customer returns to list price from the next period. A second DELETE → 404. *(manual 2026-07-20: post-delete close billed list 3000¢; double-delete 404. The in-flight-period-still-bills-the-override half is wall-clock-only — same clock caveat as above, automated coverage.)*
- [x] **Malformed tiers are rejected at creation** — a graduated rule (or override) with non-increasing `up_to`, or without a final catch-all (`up_to=0`) tier → 422 naming the tier, instead of an invoice failure at cycle close. *(manual 2026-07-20: verbatim "tier 2: up_to (500) must be strictly greater than the previous tier's up_to (1000)" and "the final tier must be a catch-all (up_to=0)…"; same 422 on the override path; the dialogs also pre-validate ordering client-side.)*
- [x] **Currency change is blocked while overrides exist** — publishing a version with a different currency while any customer's active override references the rule → 409 naming the override count. *(manual 2026-07-20: verbatim "cannot change currency from USD to EUR: 1 active customer price override(s) reference rule …and would be silently repriced in the new currency".)*

## FLOW B10: Manual tax + customer tax status

Manual provider applies one flat tenant rate to every customer regardless of country (the old `tax_home_country` / cross-border zero-rating model was dropped — ADR-038). Exemption is driven solely by the customer's `tax_status` (`standard` / `exempt` / `reverse_charge`). Rate precision is covered by B2. <!-- currency-ok: documents the dropped tax_home_country model -->

- [x] Settings → set tax rate `18` + tax name `IGST` (`tenant_settings.tax_rate=18.0000`; no `tax_rate_bp` / `tax_home_country` columns exist). <!-- currency-ok: states the columns were removed --> *(manual 2026-07-20: via the Settings → Tax tab; DB column 18.0000 exact.)*
- [x] Any `standard` customer, any country: $100 → `tax_amount_cents=1800`, `tax_name=IGST`, PDF tax line `IGST (18%)` (decimal `%.4g`) with the customer-country suffix (`IGST (18%) [US]`). *(manual 2026-07-20: one-off composer invoice; header + PDF exact.)*
- [x] Customer `tax_status=exempt` → $0 tax, invoice `tax_exempt_reason` populated, PDF carries "Tax-exempt: ⟨reason⟩". (`tax_reason='customer_exempt'` on the LINE is the Stripe-Tax path — manual/none providers drive the legend from the invoice-level reason.) *(manual 2026-07-20: $100, tax 0, PDF legend "Tax-exempt: 501(c)(3) nonprofit" verbatim.)*
- [x] Customer `tax_status=reverse_charge` (India B2B): $0 tax; PDF carries the supplier tax ID under the company line + "Tax payable on reverse charge basis: YES — recipient is liable to pay GST under section 9(3)/9(4) of the CGST Act." and a "Tax (reverse charge) $0.00" totals row; buyer GSTIN under BILL TO. *(manual 2026-07-20: verbatim. The supplier id labels as generic "Tax ID" unless the seller country is set — the GSTIN label keys off it.)*
- [x] EU `reverse_charge` customer → $0 tax, PDF retains the EU reverse-charge wording ("Reverse charge — VAT to be accounted for by the recipient."). *(manual 2026-07-20: verbatim.)*
- [x] **Exemption inputs are enforced:** PUT a billing profile with `tax_status=exempt` and no `tax_exempt_reason` → **422** ("a reason is required when tax_status is 'exempt'"). `tax_status=reverse_charge` with no `tax_id` → **422** (buyer tax ID required). Both save once the field is supplied. (Direct-API guard; the dashboard already blocks these.) *(manual 2026-07-20: both verbatim — the repo-wide validation code is 422, not the 400 this flow used to claim.)*
- [x] **Country is validated to ISO alpha-2:** PUT a billing profile with `country="USA"` → **422** ("must be an ISO-3166 alpha-2 country code"). `country=" us "` saves and stores as `US`. *(manual 2026-07-20: both exact.)*
- [x] **`tax_provider=none` still renders the legend:** on a `none` tenant, a `reverse_charge` (or `exempt`) customer's invoice is $0 AND carries the reverse-charge / exemption legend (not a bare $0 with no notice). *(manual 2026-07-20: none-provider RC invoice — "Tax (reverse charge) $0.00" row + EU legend on the PDF.)*
- [x] **Domestic reverse charge is flagged, not silently zero-rated (ADR-052):** with tenant `company_country=DE`, set a customer in `DE` to `tax_status=reverse_charge` and generate an invoice → invoice is still $0 (override honored) BUT the API log carries a WARN `tax: domestic reverse charge — buyer is in the seller's registration country …`. A customer in `FR` (cross-border) logs nothing. Edit billing profile UI: selecting **Reverse charge** shows the help "prefer Standard + a Tax ID … never applies to a buyer in your own country"; selecting **Standard** shows "add their Tax ID below — reverse charge is then applied automatically where it applies (cross-border)". *(manual 2026-07-20: exactly one WARN (DE, cc=DE reg=DE), FR silent; both help texts verbatim.)*
- [x] Stripe-Tax path: `taxability_reason=not_collecting` round-trips → line item `tax_reason='not_collecting'`, "Not collecting in this jurisdiction" badge in dashboard. *(manual 2026-07-20 vs LIVE Stripe: found+fixed in this walk — Stripe returns not_collecting only in the DOCUMENT-level breakdown (per-line null, rate 0, tax 0) and the fallback's taxed-line gate dropped it; the reason now copies independent of rate/amount (mutation-verified test). Re-walked live: GB customer on the unregistered sandbox → line tax_reason=not_collecting + dashboard badge.)*

## FLOW B11: Tax-ID validation

- [x] `in_gst` + `27AAEPM1234C1Z5` → accepted. Legacy `gstin` → normalized to `in_gst` on write. *(manual 2026-07-20: response echoes `tax_id_type=in_gst` for both spellings.)*
- [x] `eu_vat` + `DE123456789`, `au_abn` + `51824753556` → accepted. *(manual 2026-07-20.)*
- [x] Unknown Stripe code (`za_vat`, `br_cnpj`) → accepted as-is. *(manual 2026-07-20: both stored verbatim, `br_cnpj` punctuation preserved.)*
- [x] Malformed (wrong SHAPE) `in_gst` / `eu_vat` / `au_abn` → 422 with format-specific message. Validation is deliberately format-only: a shape-valid ABN with a bad checksum (`11111111111`) is accepted — checksum/VIES upgrades are deferred in-code with named triggers (paying customer under a live registration). *(manual 2026-07-20: verbatim "invalid GSTIN format: expected 15-char code like 27AAEPM1234C1Z5", "invalid EU VAT format: expected 2-letter country prefix + alphanumerics", "invalid ABN format: expected 11 digits".)*
- [x] Empty `tax_id` → always accepted. *(manual 2026-07-20.)*

## FLOW B12: Subscription activity timeline

- [x] Create → activate → pause → resume → plan change → cancel. *(manual 2026-07-21: full lifecycle on a clock-pinned sub — all six events landed, sim-primary timestamps with wall "Recorded" provenance.)*
- [x] `GET /v1/subscriptions/{id}/timeline` → events ascending; each carries timestamp, source, event_type, status, description, and the structured actor triple (`actor_type`/`actor_name`/`actor_id` — an API key resolves to its display name) plus the sim payload (`is_simulated`/`sim_effective_at`/`test_clock_id`). Envelope is `{events, truncated}`. *(manual 2026-07-21: 6/6 ascending, all fields verbatim, actor_name="b7-walkthrough".)*
- [x] Operator cancel → "Subscription canceled by operator". Customer (portal) cancel → "Subscription canceled by customer" — the suffix comes from `canceled_by` metadata. *(manual 2026-07-21: operator variant verbatim; the old doc claimed a bare "Subscription canceled" for operators — stale.)*
- [x] Status colors: emerald/amber/red/violet/blue. *(manual 2026-07-21: blue create/plan-change, emerald activate/resume, amber pause, red cancel observed; violet is the remaining palette member for statuses this lifecycle doesn't produce.)*
- [x] Subscription detail UI shows Activity card; resolved actor renders "by {actor_name}". *(manual 2026-07-21: "Recorded … · by b7-walkthrough" on every row; pause row carries the helper "Cycle keeps drafting; no charge until resumed".)*
- [x] **Proration amounts on the timeline are GROSS in every variant (adjudicated this walk):** `proration.amount_cents` — and therefore "Proration invoice $X" / "Credit $X" / "Open invoice adjusted $X" — is the money amount of the outcome artifact (invoice total incl. tax / gross credited / amount_due relieved). Pre-fix the invoice variant carried the pre-tax net, so the feed read "$18.00" beside an invoice whose page said $19.80. *(manual 2026-07-21: upgrade showed "Proration invoice $33.00" = 3000 net + 300 tax; mutation-verified test.)*
- [x] Nonexistent sub ID → 404. *(manual 2026-07-21.)*
- [ ] **Audit-store failure is a 5xx, not an empty timeline**: with the audit query failing (e.g. stop Postgres mid-session), `GET /v1/subscriptions/{id}/timeline` → 500 — never a 200 with `events: []` masquerading as "no history".
- [ ] **>100-event history says so**: the response carries `truncated: true` and the Activity card shows "Showing the 100 most recent events — earlier history isn't displayed." (the earliest rows — create/activate — are the ones missing). Subs with ≤100 events: `truncated: false`, no notice.

## FLOW B13: Multi-dimensional meters

- [x] `POST /v1/usage-events` with dimensions `{model, operation, cached, tier}` → 201; value stored as NUMERIC. *(walked 2026-07-21: all four dimensions echoed back.)*
- [x] Decimal preserved end-to-end (`10000.5` round-trips). *(walked 2026-07-21: stored and read back exact.)*
- [x] Replay same idempotency_key → **200 + the ORIGINAL event** with `Idempotent-Replayed: true` header, no duplicate row (Stripe idempotency shape; pre-2026-07-05 this was a bare 409). *(walked 2026-07-21: original event id returned, count unchanged.)*
- [x] `POST /v1/usage-events/batch` with one invalid row → 422 `batch_rejected`, `errors[]` indexed per row, **zero events written** (all-or-nothing — a retry can never double-ingest a committed prefix). Replay a fully-committed keyed batch → 201 `{ingested: 0, deduplicated: N}`, no error rows. *(walked 2026-07-21: errors[] named `event[1]`/`event[2]`; replay gave deduplicated:2.)*
- [x] Batch body over the 1MB cap → 413 `batch_too_large` (not a misleading 400 "expected JSON array"). *(walked 2026-07-21: 1.4MB body.)*
- [x] Rule with `dimension_match={"token_type":"input"}` claims only input events; `{"token_type":"cache_read"}` claims only cache-read events. Token roles are DISJOINT (ADR-044), so each `{model, token_type}` matches exactly one rule — no priority tie-break needed (the old boolean `cached` + priority-wins model is gone). *(walked 2026-07-21: 1M input → 300¢, 200k output → 300¢, 5M cache_read → 150¢ — three disjoint lines, sub-cent rates exact. Note: the attach endpoint UPSERTS on (meter, rating-rule version) — re-posting the same rule id updates dimension_match/aggregation in place, it does not create a second rule; that update-in-place is how the aggregation switch below works.)*
- [x] Five aggregation modes — `aggregation_mode` on the meter **pricing rule** (`sum`/`count`/`max`/`last_during_period`/`last_ever`; the meter-level `aggregation` field is a 4-value default where `last` = last_during_period) — all bill correctly. A period with NO events for a `last_during_period` rule bills nothing, while `last_ever` carries the prior period's final reading (state-style billing, e.g. seats). Switching a rule's aggregation between cycles re-bills the next cycle only; finalized and drafted past invoices unchanged. *(walked 2026-07-21 on one meter, five dimension-matched 1¢ rules, series 10/30/20: sum=60¢ count=3¢ max=30¢ ldp=20¢ lev=20¢; empty period → ldp absent, lev=20¢; sum→max switch billed 9¢=max(7,2,9) next close, past invoices frozen.)*
- [x] **Meter default binding is settable post-create (2026-07-05):** `PATCH /v1/meters/{id} {"rating_rule_version_id": "<rule>"}` → unmatched-dimension usage prices at that default from the next close (the silently-unbilled remedy); a typo'd rule id → 422; `""` clears the binding. Meter detail page: **Link rule / Change / Unlink** on the Default pricing rule card round-trips the same PATCH. *(walked 2026-07-21: 10 unmatched units billed 20¢ at the 2¢ default; UI affordance shipped this walk — card was read-only before.)*
- [x] `cmd/velox-bench` runs clean (`errors: 0`; on failure it prints `first err:` — a 100%-error run must say why) and sustains the local single-row baseline, ~2.5–4k events/sec on Mac + Docker Postgres. The design doc's 50k events/sec is a cloud-hardware + batched-INSERT target (mitigations enumerated in docs/design-multi-dim-meters.md), **not** a local assertion — the pre-2026-07-21 wording claimed 50k locally, which was never measured anywhere. *(walked 2026-07-21: 32 workers/20s → 3,647 ev/s, p50 7.96ms; bench had been broken since the livemode-propagation gate — TxTenant refuses a bare ctx — fixed this walk with `postgres.WithLivemode(ctx, false)` + livemode=false fixtures.)*

## FLOW B14: Billing thresholds

- [x] `PUT /v1/subscriptions/{id}/billing-thresholds` `{item_thresholds:[{subscription_item_id, usage_gte:"10000"}]}` (per-item, keyed on `subscription_item_id` — not `meter_id`; `usage_gte` is a STRING decimal — a bare number is a 400). Ingest 9999 → no early finalize (verified against real ticks: a 9,999/10,000 control sub stayed invoice-free across ~40 min of scans). Ingest 1 more → invoice auto-finalized within 1 tick (the background scheduler, 5m local — `POST /v1/billing/run` runs the CYCLE scan only, not thresholds), `billing_reason="threshold"`, billing everything accrued (10,001 units → 11,001¢ gross). Default `reset_billing_cycle=false` (Stripe keep-anchor): the fire does NOT re-anchor the cycle. *(walked 2026-07-21 on the wall scheduler. FOUND+FIXED same walk: item-only thresholds fabricated `reset=true` in the hydrate — the engine re-anchored and the API echoed `true` regardless of the stored flag; amount thresholds were unaffected. Mutation-verified test `TestThresholdScan_ItemOnlyKeepsAnchor`.)*
- [x] PUT `{amount_gte:50000}` → cross $500 → same shape. *(walked 2026-07-21: 50,001 units → 55,001¢ gross, keep-anchor held — period start unchanged.)*
- [x] Cross threshold + immediately `POST /v1/billing/run` → idempotent skip. *(walked 2026-07-21: `invoices_generated: 0`, still exactly one invoice.)*
- [x] Subscription detail "Spend Thresholds" card: empty state with Set button. Edit dialog has subtotal cap, reset_billing_cycle checkbox (unchecked by default — keep-anchor), per-item rows. Save shows `$1,000.00` (from cents) and `≥ 10000.5 units`, subtotal cap annotated "(continues cycle on fire)". Clear thresholds → flips to empty, API reads null. *(walked 2026-07-21 in the UI; the dialog's helper text claimed checked was the default — copy fixed same walk.)*
- [x] **Threshold invoice on a test clock discloses simulation:** pin a sub with an amount threshold to a test clock, advance until the cap crosses → the threshold invoice's list row shows the **Simulated** chip and its detail page the test-clock banner (is_simulated=true), same as sibling cycle invoices on the clock. *(walked 2026-07-21: catchup Phase 1.5 fired it; row chip + banner + threshold window "Mar 1 – Mar 9, 2029" all present.)*
- [x] **Threshold fire with NO payment method queues + notifies (not silence):** on a sub whose customer has no card on file, cross the amount threshold → the threshold invoice finalizes with `auto_charge_pending=true` (visible via API) and Mailpit shows the **"Set up payment for invoice …"** email for that invoice (no decline language — the customer was never charged). Attach a card via the setup link → the next scheduler tick charges the invoice with no operator action (charge-on-attach). A tax-deferred (draft) threshold invoice is queued-but-quiet — `auto_charge_pending=true` (inert while draft) and NO email; once tax retry finalizes it, the next sweep collects it without operator action. *(walked 2026-07-21 live end to end: email → setup link → Stripe Checkout test card → webhook attach → tick charged NIM-000197 to paid. Draft-quiet variant is CI-locked: `TestThresholdScan_NoPM_DraftInvoiceQueuesButStaysQuiet`.)*
- [x] **`reset_billing_cycle=false` cycle close bills only the residual:** set `{amount_gte, reset_billing_cycle:false}`, cross the cap mid-cycle (threshold invoice fires with the FULL in-arrears base + usage-to-date), keep ingesting, then advance to period end → the cycle invoice has NO base-fee line and its usage line's period starts at the threshold invoice's `billing_period_end` (only post-fire usage). Sum of the two invoices == what one un-thresholded invoice would have been. **Card-less caveat:** a prior no-PM fire enrolls in dunning, which pauses collection — and the threshold scan skips paused subs by design; resume collection (or attach a card) before expecting the next fire. *(walked 2026-07-21: fire 47,000¢ net; residual close = one usage line, period [Jun 14 → Jun 30], 5,000¢; 47,000+5,000 = the un-thresholded month exactly.)*
- [x] **Fire-once, no burn:** after a `reset_billing_cycle=false` cap fires, `POST /v1/billing/run` a few more times → still exactly one threshold invoice, and the next invoice you create carries the immediately-next number (the re-ticks consumed no invoice number — no phantom gap in the sequence). *(walked 2026-07-21: 3 extra runs → 0; manual invoice took NIM-000199 right after NIM-000198.)*
- [x] **`reset_billing_cycle=true` fire prorates the base:** base-fee plan, cross the cap mid-month → the threshold invoice's base line reads `… base fee (qty N, prorated X/Y days)` with qty × unit == amount, NOT the full month's base; the subscription's current period now starts at the fire. Cross again next window → second prorated base; the bases across a cycle sum to ≈ one full base fee, never N×. *(walked 2026-07-21: "prorated 9/31 days" 871¢ then "prorated 6/31 days" 581¢ — 1,452¢ = 3,000 × 15/31 exactly for the elapsed 15 days.)*
- [x] **$0 invoice settles, never lingers:** free-rated plan ($0 base + $0/unit) + per-item `usage_gte` cap → crossing emits a $0 invoice that shows **Paid** immediately (no charge attempt, no dunning), and it does NOT appear in the failed/awaiting-payment attention views. *(walked 2026-07-21: born paid, no auto_charge_pending.)*
- [x] **Peak (max) meters bill once, at cycle close:** plan with a max-aggregated meter + `{amount_gte, reset_billing_cycle:false}`; drive a peak + enough sum usage to cross → the threshold invoice carries the sum usage but NO peak line; at cycle close the peak bills once at the FULL-period max (a pre-fire peak is not lost). Sum of the two invoices == one un-thresholded invoice. *(walked 2026-07-21: fire = sum 41,000¢ only; close = residual 2,000¢ + peak 400¢ full-period max.)*
- [x] **Pure-peak sub crossing is visible, not silent:** sub with ONLY a max meter + `amount_gte` below the peak's value → no invoice fires, no per-tick errors in logs, and the subscription timeline shows one "threshold deferred" entry for the cycle (deduped across advances) explaining the peak bills at close. *(walked 2026-07-21: one entry across two advances. The entry rendered its raw event name — humanized same walk: "Spending threshold reached — invoice deferred" + plain-language detail.)*
- [x] **Cancel right after a cap fires doesn't double-charge:** cross a `reset=false` cap (invoice fires with base + usage), then immediately cancel the subscription → the final cancel invoice contains NO base line and NO already-billed usage — only post-fire residual usage and any deferred peak meter (full-period max). *(walked 2026-07-21: cancel invoice = residual 1,500¢ + peak 500¢ (post-fire 500 > pre-fire 300), no base.)*
- [x] **Boundary defers to the cycle:** with a crossed `{amount_gte}` cap, advance the test clock straight past period end in one jump (simulating scheduler downtime over the boundary) → NO threshold invoice fires for the closed window; the cycle close bills the whole elapsed period once. No threshold+cycle double-charge for the same usage. *(walked 2026-07-21: one `subscription_cycle` invoice, 47,000¢, full period.)*
- [x] Canceled/archived subs → Set/Edit hidden; the config renders read-only. *(walked 2026-07-21. Also found+fixed: a canceled sub still advertised a "Next Invoice Preview" ($490 estimate for an invoice that will never exist) — the card now hides on terminal subs.)*

## FLOW B15: `in_advance` plan happy path (ADR-031)

- [x] **A tax-deferred day-1 DRAFT defers credit application too (ADR-088)** — with the tax provider erroring, a credit-holding customer's day-1 invoice parks as a tax-pending draft with credits UNAPPLIED; when tax retry finalizes it, the sweep applies credits before charging. *(CI-locked: `TestApplyCreditsAndCollect` subtest "draft invoice → credits not applied (waits for tax-retry + sweep)" + the sweep-side collect tests; ran green at HEAD 2026-07-21.)*
- [x] **Activating a DRAFT bills day-1 atomically (ADR-056 sibling)** — create a draft sub on an in_advance plan, then POST /{id}/activate → the sub flips active AND the `subscription_create` invoice exists with the first period (pre-fix the flip billed nothing: the first base fee was silently never invoiced). *(automated: `TestActivateAndCancel` subtests "activating a draft BILLS day-1 in the flip tx" + "bill failure rolls the activation back" — ran green at HEAD 2026-07-21.)*
- [x] **Credit balance pays the day-1 invoice (ADR-088)** — grant a customer credit ≥ the plan's base fee (`POST /v1/credits/grant`), subscribe to an in_advance plan. Day-1 invoice lands **Paid** with `credits_applied` covering the total; balance drained; no Stripe charge, no queue. With a partial balance: `credits_applied` for the balance, card charged exactly the remainder (card-less: remainder queues `auto_charge_pending=true` + setup email). *(walked 2026-07-21: full — 5,216¢ covered, paid, amount_due 0, balance 6,000→784 exact; partial — 2,000 applied, balance→0, amount_due 3,216 queued + "Action required" email. Charged-remainder variant CI-locked in `TestApplyCreditsAndCollect`.)*

Verifies the day-1 invoice + the cycle-close invoice that bills the upcoming period's base.

- [x] Pricing → New Plan: select **Base fee billed = At start of period** ("platform fee + usage"; helper reads "charged the base fee on day 1 of each period; usage settles at period end"). Create plan `pro-advance` $49/mo, no meters. *(walked 2026-07-21 in the dialog.)*
- [x] Plan Detail → Properties shows `Base fee billed: At start of period`. *(walked 2026-07-21.)*
- [x] Create customer with PM (`4242 4242 4242 4242`) — operator path: customer page → Payment methods → **Add payment method** → "Send email" → customer opens the Stripe-hosted setup link. *(walked 2026-07-21 live: email → Checkout → webhook attach, visa …4242 default.)*
- [x] Create active subscription on `pro-advance` → **invoice generated immediately**:
  - `billing_reason = "subscription_create"`
  - Period = today → period_end
  - Single base-fee line, qty 1, $49 (or prorated if mid-period — "prorated X/Y days")
  - Total = $49 + tax
  - `payment_status=succeeded` if PM ready (auto-charged), else `auto_charge_pending=true` + Mailpit shows the no-PM email.
  *(walked 2026-07-21: full-month sub → 5,390¢ auto-charged to paid; a second sub created one day into the period billed "prorated 30/31 days" = 4,742¢ base — both branches live.)*
- [x] Invoice Detail row's "Covers" sub-line not surfaced (line period == invoice period on this day-1 invoice). *(walked 2026-07-21.)*
- [x] Advance clock (or wait) to period close → cycle invoice generated:
  - `billing_reason = "subscription_cycle"`
  - Single base-fee line, total = full base + tax, no proration; auto-charged
  - On a **pure** in_advance plan (no meters) the invoice IS the next period's bill: invoice period == line period == the **upcoming** period, so NO "Covers" sub-line renders (nothing from the elapsed period to bill). The elapsed-period-invoice + "Covers next period" sub-line shape belongs to the HYBRID case — FLOW B16 — where in_arrears usage pins the invoice to the elapsed period and the in_advance base line's next-period range diverges. *(walked 2026-07-21: cycle invoice period = July, 5,390¢ paid; pre-walk wording claimed the elapsed period + sub-line here, which is B16's shape, not the pure plan's.)*

## FLOW B16: Hybrid `in_advance` base + `in_arrears` usage on one invoice

The standard B2B SaaS shape: platform fee charged at period start, usage settles at period end. Run on top of B15.

- [x] Plan `pro-advance-metered`: `Base fee billed = At start of period`, $99/mo, with one flat-$0.01 meter. *(walked 2026-07-21.)*
- [x] Day 1: create sub → day-1 invoice carries ONLY the base fee ($99 + per-line tax, auto-charged paid). Usage line absent (no events). *(walked 2026-07-21.)*
- [x] Ingest 1,000 events over the period. *(walked 2026-07-21: 400+350+250.)*
- [x] Period close → cycle invoice:
  - **Invoice header period = the NEXT period** (the in_advance shift, `billOnePeriod` — deliberate and load-bearing: it is what lets the day-1 and first cycle invoices coexist under the `(sub, period_start, period_end)` UNIQUE index, and it keeps the header in sync with the sub's new `current_period_*`). The pre-walk wording claimed header = elapsed period; that was never the design.
  - Base line: $99, `billing_period_start/end = next period` — **matches the header, so its sub-line is suppressed**.
  - Usage line: $10 (1,000 × $0.01), `billing_period_start/end = elapsed period` — diverges from the header, so **it** carries the sub-line: "Covers Sep 1, 2030 – Sep 30, 2030" (date range only — no "(in advance)" parenthetical; civil inclusive-end rendering).
  - Single invoice carries both — no separate invoice for the upcoming base.
  *(walked 2026-07-21: header = October, lines exactly as above.)*
- [x] Tax applies to both lines; per-line `tax_amount_cents` populated. *(walked 2026-07-21: 990¢ + 100¢.)*
- [x] Auto-charge fires once for the combined total. *(walked 2026-07-21: one settle event, 11,990¢ paid.)*

## FLOW B16b: token usage billed on immediate cancel (ADR-044 cancel path)

- [x] Setup: sub on a pure-usage plan with the multi-dim `tokens` meter (per-`{model, token_type}` pricing rules — meter has NO direct rating-rule binding). Ingest input + output token usage mid-period. *(walked 2026-07-21 on the B13 matrix fixtures; binding verified empty.)*
- [x] Cancel immediately → a final invoice IS emitted with `billing_reason=subscription_cancel`, one usage line per claimed rule (`… - canceled mid-period`), priced at the recipe's decimal rates. *(walked 2026-07-21: input 500,000 × $0.000003 = 150¢; output 100,000 × $0.000015 = 150¢.)*
- [x] Each usage line carries `quantity_decimal`; line amounts match what the same usage would bill at cycle close. *(walked 2026-07-21: exactly half of B13's verified 1M→300¢.)*

## FLOW B17: `in_advance` cancel proration credit

- [x] Setup: customer with PM, sub on `pro-advance` ($49/mo), day-1 invoice **paid** (B15). *(walked 2026-07-21 on the B15 fixtures; credits-funded variants below prove the same paths card-free.)*
- [x] Cancel mid-period → status `canceled` → credit-note-backed balance grant for the **unused GROSS** (base + its tax share), day-granular: canceled 16/31 days in → grant 2,608¢ = 5,390 gross × 15/31 exact. **Endpoint contract:** `POST /{id}/cancel` cancels IMMEDIATELY and **ignores any request body**; scheduled cancels are the separate `POST /{id}/schedule-cancel {at_period_end:true | cancel_at}`. *(walked 2026-07-21.)*
- [x] Ledger entry: `entry_type=grant`, description **references the funding credit note** ("Credit note CN-NNNNNN — subscription_cancellation") — the CN carries the full accounting (source invoice link, net/tax split, reason). The pre-walk prose-description claim predates the ADR-057 CN-funded redesign. *(walked 2026-07-21: CN 2,371 net + 237 tax = 2,608, issued against the most-recent funding invoice.)*
- [x] Cancel at/after `current_period_end` (via schedule-cancel + boundary advance) → sub flips canceled at the boundary, **no proration credit** (period used in full). Same-instant subscribe-and-cancel → FULL prepayment granted back. *(walked 2026-07-21: Fay — balance unchanged post-flip; Ella — full 2,608 back.)*
- [x] Cancel on a pure `in_arrears` sub → no proration credit (nothing was prebilled); a zero-elapsed zero-usage cancel emits no invoice at all. *(walked 2026-07-21.)*
- [x] ~~Mixed sub (one in_advance item + one in_arrears item)~~ **Mixed timing on one SUBSCRIPTION is validation-blocked** ("all items must share the same bill_timing") — the mixed shape is a single HYBRID plan (B16). Canceling one: credit = unused portion of the in_advance base only (5,269¢ = 10,890 × 15/31); elapsed in_arrears usage bills on the final cancel invoice. *(adjudicated + walked 2026-07-21 on the B16 fixture.)*
- [x] Future invoices on this customer auto-apply the credit (C1 behavior). *(walked 2026-07-21: next cycle's prebill consumed a 784¢ remainder automatically.)*
- [x] **Cash back to card instead of balance credit (B2B default is balance credit):** cancel grants a *balance* credit, not a card refund. To return cash, issue a credit note on the paid source invoice with **Refund to card** → Stripe refund processed, customer balance NOT credited. *(walked 2026-07-21 against live Stripe: `POST /invoices/{id}/refund` 1,000¢ → CN issued, `refund_status=succeeded`, balance untouched.)*
- [x] **Source invoice unpaid → invoice settled, not credited (ADR-031 amendment):** cancel mid-period → `canceled`, **NO credit ledger entry**, and the unpaid invoice's `amount_due` reduces to the consumed portion via an adjustment CN (log verbatim: `unpaid prebill relief: reduced amount_due to consumed portion`, `reduced_by_cents` = unused gross). Full variant coverage in FLOW TC8b. *(walked 2026-07-21: due 3,216 → 608, relief 2,608 = 15/30 days unused, day-granular.)*
- [x] **Plans > ~$36 base** (regression check): every grant in this walk is formula-exact at day granularity (2,608 = 5,390×15/31; 5,269 = 10,890×15/31; 15,614 across two CNs) — no zero-credit truncation. *(walked 2026-07-21.)*
- [x] **Atomic + crash-recoverable (ADR-057 ext):** the in-tx draft failure rollback and the crash-recovery reconciler are CI-locked as real-Postgres tests — `TestCancelCredit_DraftFailure_RealTxRollsBackCancel` (draft fails → cancel rolls back, sub stays active) and `TestCancelCredit_PaidInAdvance_DraftAtomicAndReconcilerRecovers` (post-commit crash → `RetryPendingClawbackIssue` lands the credit exactly once, and the issue retires `issue_pending`). These supersede the manual REVOKE/GRANT dance the box used to prescribe. *(ran green at HEAD 2026-07-21; issue_pending clear added same day.)*

## FLOW B17b: upgrade then cancel — credit fans across both funding invoices

- [x] Setup: in_advance $100/mo sub, day-1 invoice paid → upgrade to $200/mo mid-period (`PATCH /{id}/items/{itemID} {"new_plan_id":…, "immediate":true}` — without `immediate` the change is scheduled PENDING at period end) → proration invoice created and paid → cancel immediately. *(walked 2026-07-21, credits-funded: day-1 11,000¢; upgrade delta 8,161¢ = (200−100)×23/31 gross; the queued upgrade invoice was credit-settled by the next catchup sweep.)*
- [x] Credit Balance increases by the FULL unused prepayment at the $200 rate — NOT $0 and NOT clamped to the day-1 invoice. *(walked 2026-07-21: +15,614¢ = 20,000 × 22/31 × 1.1, balance exact to the cent.)*
- [x] Credit Notes: **two** for the cancel — one against the base invoice, one against the upgrade proration invoice — each within its own invoice's total; their sum equals the balance credit. *(walked 2026-07-21: 7,808 + 7,806 = 15,614.)*
- [x] On a taxed sub, each credit note reverses its own invoice's proportional tax. *(walked 2026-07-21: 709¢ + 709¢.)*
- [x] Server log: the cancel-tx draft path (ADR-057, the live path) is **silent on success** — the observable contract is the two CNs + exact balance + NO `customer not credited` error (that error is the loud-failure line). The `cancel proration credit issued … funding_invoices=N` line belongs to the engine's FALLBACK path only; the pre-walk box pinned the wrong path's log. *(adjudicated 2026-07-21; zero error lines.)*

## FLOW B17c: downgrade after upgrade — clawback reverses the upgrade invoice (LIFO)

- [x] Setup: in_advance sub, day-1 invoice paid → upgrade mid-period (second proration invoice, paid) → **downgrade** the plan.
- [x] The downgrade clawback issues its credit note against the **upgrade** (most-recent) invoice, not the day-1 base invoice — reversing that invoice's own tax; the base plan you're keeping is untouched. *(walked 2026-07-21 — FOUND+FIXED a tie-break bug: LIFO sorted by CreatedAt alone, and a clock catchup stamps every invoice it generates with the SAME simulated instant (same-second subscribe+upgrade does it on wall time too), so the stable sort fell back to the funding query's oldest-first order and the clawback hit the DAY-1 invoice. Equal timestamps now break toward the `subscription_update` invoice; mutation-verified regression `TestUpdateItem_Downgrade_LIFOTiebreak_SameInstantFunding`.)*
- [x] If the clawback exceeds the upgrade invoice's remaining room, it spills onto the base invoice (a second credit note); it never silently drops or loud-fails on a single invoice's cap. *(CI-locked: `TestSettleUnusedAcrossFunding_HeadroomSpill` + `_MixedPaidUnpaid_NoSilentDrop`, green at HEAD; spill also OBSERVED LIVE in the downgrade-then-cancel walk below.)*
- [x] **Downgrade then cancel** (no double-credit): after the downgrade, cancel. The cancel credit accounts for the headroom the downgrade already consumed and total credited never exceeds what was paid. *(walked 2026-07-21: downgrade 7,452 + cancel 7,451 (355 on the base's remaining room, 7,096 spilled to the upgrade invoice) = 14,903 ≤ 15,614 paid.)*
- [x] **Quantity decrease** (3→1 seats) on a sub funded by two invoices → the clawback splits **proportionally** across funding invoices (not LIFO), each reversing its own tax. *(mechanism: `AllocateByWeightCapped` on the qty/item-remove branch of `resolveClawbackPieces`; covered by the proration-clawback unit suite, green at HEAD 2026-07-21.)*

## FLOW B18: Meter Detail page

- [x] Default rule card renders the latest version of the linked rating rule (republish rule → version badge bumps). *(walked 2026-07-21: republished the linked rule_key → card followed v1 → v2, name and rate ($0.01 → $0.02/unit) updated with it.)*
- [x] Add dimension-matched rule: k=v rows, priority, rating-rule select → save → table refetches. *(walked 2026-07-21 in the dialog: three k=v rows + priority 100 + versioned rule picker → row rendered with dimension chips + toast.)*
- [x] Dimension value coercion: `true/false` → bool, numeric strings → number, else string. *(walked 2026-07-21, verified at the API: `{"cached": true (bool), "tier_level": 42 (number), "model": "gpt-4" (string)}`.)*
- [x] Per-row delete: typed `delete` confirm (button disabled until typed); already-finalized invoices unaffected. *(walked 2026-07-21: row removed, store empty; the meter's paid invoices kept their exact totals.)*

## FLOW B19: Cancel-flow billing artifacts

- [x] **Mid-period immediate cancel, `in_arrears` plan:** final invoice `billing_reason='subscription_cancel'`, period = [cycle open → cancel instant], lines = prorated elapsed base + usage. *(walked 2026-07-21: $20 plan canceled 15/30 days in → "base fee (qty 1, prorated 15/30 days)" 1,000¢ + 5,000¢ usage, period Nov 1 → Nov 15 exact.)*
- [x] **Same-day create-then-cancel bills one day-grade day, not $0 (ADR-012 amendment 2026-07-26):** an in_arrears sub created and immediately canceled the same day gets a final invoice with base "prorated 1/N days" — the consumed signup day counts whole (pre-fix: roundDays(hours)=0 dropped the base line entirely, and matching the in_advance twin's 1-elapsed-day credit math). *(automated: `TestBillFinalOnImmediateCancel_FirstPeriod_SameDayCancel`.)*
- [x] **Mid-period immediate cancel, `in_advance` plan:** TWO independent artifacts — (a) final invoice with usage line only (no base — already prepaid); (b) credit grant for the unused days (B17). The invoice does NOT pre-apply the credit. *(walked 2026-07-21 in the B17 hybrid walk: cancel invoice 200¢ usage-only + independent 5,269¢ CN-backed grant.)*
- [x] **Clean cancel at-or-after period_end:** no second final invoice, no credit. *(walked 2026-07-21: the boundary-canceled in_advance sub holds exactly ONE invoice — its day-1 prebill — after the flip.)*
- [x] **Scheduled cancel at period_end on `in_advance`:** at cycle close the invoice contains **NO upcoming-period base line** (the next period's base is never charged); the elapsed period's usage still bills normally; then the sub flips canceled. *(walked 2026-07-21: close invoice = one usage line 800¢ (at the meter's LATEST default-rule version), no $99 base; sub canceled. Note the invoice header carries the next-period range per the B16 adjudication — the usage line's own range is the elapsed period.)*
- [x] **Scheduled cancel at period_end on `in_arrears`:** cycle-close invoice = full base + usage for the just-elapsed period (correct — it was consumed), then the cancel fires. *(walked 2026-07-21: 2,000¢ full base + 3,000¢ usage, period Nov 1 → Nov 30; sub canceled after.)*

## FLOW B20: Segment-aware billing at cycle close (Lago / Orb shape)

- [x] **Mid-period plan swap (in_arrears, same interval):** cycle close bills TWO base segments. *(walked 2026-07-21: A "prorated 14/31 days" 1,355¢ + B "prorated 17/31 days" 3,290¢ — line periods tile exactly at the swap instant; an arrears swap emits NO immediate proration invoice, the segments settle at close.)*
- [x] **Mid-period plan swap, different meter sets:** TWO usage lines, each bounded to its plan's segment. *(walked 2026-07-21: Meter X 100×5¢=500¢ with line period [period open → swap instant]; Meter Y 50×10¢=500¢ with [swap instant → period end] — boundaries verbatim `2032-03-14T18:30` on all four lines.)*
- [x] **No mid-period changes (no-regress):** exactly one full-period base line at the current plan. *(walked 2026-07-21: control sub — one line, 3,000¢.)*
- [x] **Scheduled plan-swap at boundary (`immediate` omitted → pending):** close bills **one** base line at Plan A's price (3,000¢, not 6,000¢); Plan B takes effect next cycle. *(walked 2026-07-21.)*
- [x] **Mid-period item add:** first item full-period line + added item's segment line from add-time to period end (line `billing_period_start` = add-time, NOT period start — exception: an add on the period's FIRST tenant-TZ day bills the full period from period start; day-grade, ADR-012 amendment). *(walked 2026-07-21: 3,000¢ + "prorated 17/31 days" 3,290¢.)*
- [x] **First period bills the FULL base despite the creation `add` row (ADR-012 amendment 2026-07-26):** a start_now sub created mid-day (e.g. 21:05 tenant TZ) closes its first anniversary period with the full base — no "prorated 30/31 days" — and base + usage line periods equal the invoice period. *(found live: NIM-000247 billed $28.06 on a full month; fixed + mutation-verified: `TestRunCycle_FirstPeriod_CreationAddRow_BillsFullBase`, `TestFirstPeriod_TriggerAddRow_BillsFullBase_E2E`.)*
- [x] **Day-1 AddItem owes the whole period (both cadences):** an item added ON a period's first tenant-TZ calendar day bills the full period — in_arrears at cycle close (clamped segment) and in_advance immediately (`remainingDays` clamped) — never 30/31 from a post-noon add. *(automated: `TestItemBaseSegments_DayGradeAddClamp`, `TestAddItem_DayOneOfPeriod_BillsFullPeriod`.)*
- [x] **Mid-period item remove (in_arrears):** remaining item full-period + removed item's segment from period start to removal; NO credit grant (nothing was prepaid). *(walked 2026-07-21: 3,000¢ + "prorated 14/31 days" 2,710¢; ledger empty.)*
- [x] **Immediate cancel after mid-period plan swap:** the final `subscription_cancel` invoice carries BOTH segment base lines. *(walked 2026-07-21: swap day 15, cancel day 20 → A "14/31" 1,355¢ + B "5/31" 968¢ = 2,323¢ net, formula-exact.)*

## FLOW B21: Immediate same-cadence cross-interval plan-swap (yearly ↔ monthly)

Velox accepts `immediate=true` plan-swaps that change the billing interval as long as bill_timing matches on both sides (in_advance↔in_advance or in_arrears↔in_arrears). Same-cadence cross-interval swaps are industry-standard (Stripe / Lago / Orb ship them). Cross-cadence (in_advance↔in_arrears) is rejected by Velox for billing-safety (prepay/postpay overlap); Zuora rejects it the same way, while Stripe ALLOWS it in-place (bills new forward). Operator path is cancel + recreate.

- [x] **In_advance yearly → monthly downgrade (same cadence, cross interval):** three artifacts in one tick. *(walked 2026-07-21: grant 99,452¢ = 132,000 gross × 275/365 EXACT (ledger references the funding CN, reason `subscription_plan_change` — B17's ADR-057 shape); period jumped to [swap day → +1 month]; new monthly invoice created and settled same tick.)*
- [x] **In_advance monthly → yearly upgrade (same cadence, cross interval):** refund credit for the unused fraction; period jumps to (today, today + 1 year); new yearly invoice. *(walked 2026-07-21: grant 5,677¢ = 11,000 × 16/31 unused; period [Jan 15 → Jan 15 next year]; 132,000¢ invoice same tick.)*
- [x] **In_arrears yearly → monthly (same cadence, cross interval):** no immediate invoice or credit; period truncated; on the next tick/Advance the closing invoice fires under the OLD yearly plan, then the new monthly period opens. *(walked 2026-07-21: zero artifacts at swap; next advance → closing 29,589¢ net = 120,000 × 90/365 EXACT — the flow's own $295.89 figure — for [period open → swap]; new period [swap → +1 month].)*
- [x] **In_arrears monthly → yearly (same cadence, cross interval):** symmetric. *(walked 2026-07-21 — FOUND+FIXED a revenue-leak bug: the deferred close's proration denominator used the subscription's LIVE billing_anchor_day, which the swap had already rewritten — a calendar-monthly period closed at "prorated 15/46 days" (3,261¢) instead of 15/31 (4,839¢), silently under-billing every anchor-changing arrears swap. `emitBaseSegmentLine` (the choke point all four old-period billers share) now derives the anchor from the period being billed via `AnchorDayFor`; mutation-verified unit test + live re-verify ("prorated 15/30 days" = 5,000¢ exact on a fresh swap).)*
- [x] **Threshold `reset=true` fire follows the same anchor convention (2026-07-26 follow-up):** the fire invoice's base-proration denominator was a FIFTH copy of the old-period math the #581 fix never reached — it still read the swap-mutated `sub.billing_anchor_day` (same 15/46-class leak, scoped to reset=true fires after an anchor-changing swap). Now derives via `AnchorDayFor(periodStart)`. *(automated + mutation-verified: `TestEvaluateThresholds_ResetProration_SwapMutatedAnchorIgnored` — mutated anchor under-billed 1,002¢ vs the correct 1,470¢.)*
- [x] **Cross-cadence REJECTED (both directions, both immediate and scheduled):** 422 with the documented message. *(walked 2026-07-21: all four combinations, message verbatim.)*
- [x] **Unpaid in_advance source:** no credit grant — the unpaid prebill is relieved to its consumed portion, then the swap proceeds. *(walked 2026-07-21: due 11,000 → 5,323, relief log verbatim with `reason=subscription_plan_change`, zero ledger entries; the paid twin granted the SAME 5,677¢ — both sides of the coin exact.)*
- [x] **Same-interval same-cadence swap (no regression):** segment-aware close, no credit/jump/invoice. *(FLOW B20 walked same day, #580.)*
- [x] **Atomic — no silent revenue drop on a failed new-period bill (ADR-056):** CI-locked as a real-Postgres test — `TestUpdateItemTx_CrossIntervalSwap_RealTxRollsBackOnBillFailure` (bill fails → swap fully rolls back, pre-swap plan + period intact, no orphan invoice); supersedes the manual REVOKE/GRANT probe. *(ran green at HEAD 2026-07-21.)*
- [x] **Refund survives a crash after the swap commit (Bug B, closed 2026-07-05):** on an in_advance cross-interval swap whose OLD-period funding invoices are all **paid**, the refund credit notes are created as `issue_pending` **drafts inside the swap tx** (`SELECT status, issue_pending, reason FROM credit_notes` → `draft, true, subscription_plan_change` immediately after the swap response). The post-commit Issue relays the balance grant + tax reversal; kill the server between the swap commit and the Issue → the drafts remain and the **clawback reconciler issues them** on its next tick (no manual credit, no lost refund). With an **unpaid** funding source the in-tx half declines and the legacy post-commit relief path runs (unchanged). Dashboard item dialogs (add / plan change / quantity) now send an `Idempotency-Key` per dialog open, so a network-level retry replays the original response instead of 400ing on the same-plan guard. *(automated: `TestCrossIntervalSwap_DraftPath_IssuesDraftsAndSkipsImmediate` + declined-fallback + draft-error-rollback siblings)*

---

## Pricing Recipes

## FLOW R1: List + preview

- [x] `GET /v1/recipes` → 3 entries (anthropic_style, openai_style, replicate_style) — all AI-native after the Phase 2 wedge-alignment trim. *(walked 2026-07-26 UI: 3 cards live.)*
- [x] `POST /v1/recipes/{key}/preview` → projected meters/rating rules/pricing rules/plans/dunning/webhooks (no DB writes). No `audit_log` row is written (read-only preview, not a "Created recipe"). *(walked 2026-07-26 UI: openai Preview rendered GPT-5.1 prices + "OpenAI-style API — monthly, USD"; recipe audit rows stayed 0.)*
- [x] Preview's Prices section shows every price WITH its rate in the meter's own units — token rules per 1M tokens ("Claude Opus 4.5 input — $5.00 / 1M tokens"), per-second rules per second ("A100 — $0.0014 / second") — full list scrollable, nothing truncated. *(walked 2026-07-26 across all 3 recipes; was names-only truncated at 5 — user catch: a review screen must show the numbers it exists to review.)*
- [x] Unknown key → 404. *(walked 2026-07-26.)*

## FLOW R2: Instantiate

- [x] `POST /v1/recipes/{key}/instantiate {overrides:{}}` (test-mode via session/key) → 201 with all created IDs. DB now has a meter + prices + a plan + dunning policy + webhook endpoint — **no products** (recipes create no product entity). *(walked 2026-07-26 UI: openai Install → 201, 35 rules + plan, navigated to the created plan page.)*
- [x] Pricing rules carry `dimension_match` JSONB. *(walked 2026-07-26: e.g. `{"model":…,"token_type":"input"}`.)*
- [x] **Catalog currency (2026-07-26 refresh, page-verified):** anthropic_style v4 prices 19 models × 5 roles (95 rules) — the 5-generation lineup (Fable/Mythos/Opus/Sonnet 5), Opus 4.5–4.8, Sonnet 4.x, Haiku 4.5, and legacy families with a still-published price; claude-3-7-sonnet deliberately absent (retired, no published price). openai_style v4 adds the gpt-5.3–5.6 lineup and the o-series; gpt-4-turbo's fictitious cached-input rate DROPPED (no such price exists on OpenAI's model page). replicate_style rates are per-second retail with `sum` aggregation. Every (mapper family × emitted token_type) pair has a recipe rule — CI-locked by the axis-widened `TestModelFamilies_EveryTokenPricedByARecipe`; catalog edits without a version bump fail `TestRecipeCatalog_VersionMovesWithContent`.
- [x] Pre-create a rating rule with a recipe's key (`opus45_input`) at a DIFFERENT rate → instantiate → 409 naming the diverging field(s); zero rows created (tx rolled back). Same key at the EXACT recipe rate → install succeeds, rule adopted, still one version. *(CI-locked `TestService_Instantiate_RefusesDivergentRatingRule` / `AdoptsMatchingRatingRule`, real-Postgres, green 2026-07-26.)*
- [x] `GET /v1/recipes` and `GET /v1/recipes/{key}` render with defaults — no raw `{{ .currency }}` template syntax anywhere in either response. *(fixed 2026-07-26; was leaking on both.)*
- [x] Instantiate emits its OWN `create recipe` row inside the install transaction (ADR-090); `preview` and a no-op re-apply write nothing. *(walked 2026-07-26 on openai_style: install → exactly 1 `create|recipe` audit row; preview → 0; 2× no-op re-apply → 0 new rows. Per-recipe <500ms + `created_by=<key_id>` not separately timed this pass.)*

## FLOW R3: Idempotent re-apply, no uninstall (ADR-085)

- [x] Instantiate same recipe twice → second call is a **no-op**: `201` with the SAME `id` and `created_objects` as the first call (not a fresh instance, never a 409). Object counts (`meters`/`rating_rule_versions`/`plans`) are unchanged by the second call — no duplicate plan. *(walked 2026-07-26: re-POST openai_style → same id `vlx_rec_8a81…`, 35 rules, 201; UI disables re-install.)*
- [ ] Different tenant, same recipe → 201 (fresh instance, its own new objects).
- [x] Same tenant, other MODE → 201 with a **fresh** instance and fresh mode-scoped objects, not a no-op against the first mode's badge; each mode's card shows only its own "Installed" state. *(CI-locked `TestService_Instantiate_LivemodePartitioned`, real-Postgres, green 2026-07-26 — regression for the livemode-blind badge fixed in m0157.)*
- [x] No uninstall exists: there is no `DELETE` route under `/v1/recipes/instances` (removed along with `Force` and the `seed_sample_data` scaffolding) — the badge (`recipe_instances` row) is a permanent record and is never deleted by recipe machinery. To retire the generated plan, archive it via `PATCH /v1/plans/{id}` (existing plan-domain verb) — the badge still names it afterward, truthfully, as what this recipe created.

## FLOW R4: Atomic rollback

- [x] Inject mid-instantiate failure (e.g. invalid webhook URL) → 422; zero rows created. *(CI-locked `TestService_Instantiate_AtomicityRollback`, real-Postgres, green 2026-07-26.)*
- [x] No `recipe_instances` row. *(same lock.)*

## FLOW R5: Dashboard UI

- [x] `/recipes` → 3 cards (anthropic_style, openai_style, replicate_style). Preview opens side panel; Instantiate dialog names side-effects and opens the created plan (`/plans/{id}`; `/pricing` fallback) on confirm. *(walked 2026-07-26 UI end to end.)*
- [x] Once installed, the card shows "Installed \<date\> · \<instance id\>" and the dialog's CTA reads "Already installed" (disabled) — no Uninstall action anywhere in the UI.
- [x] An installed recipe's dialog is READ-ONLY: header reads "Installed with these settings", the fields show the values recorded on the badge (not defaults) and are disabled, with a caption pointing to the plan/pricing pages for edits — no live inputs whose values a no-op re-apply would ignore. *(walked 2026-07-26 both states; user catch — install-time parameters are not post-install settings.)* *(walked 2026-07-26: "Installed Jul 26, 2026, 12:44 PM · vlx_rec_8a81…", CTA disabled. NOTE: the date rendered "Invalid Date" until this PR — FE read `instantiated_at`, API returns `created_at`.)*

---

## Invoices

## FLOW I1: Multiple meters

- [x] Plan with **$29 base** + API ($0.01/call) + Storage ($0.10/GB). Ingest 2000 calls + 50 GB → invoice has 3 line items: base $29 + API $20 (2000 × $0.01) + storage $5 (50 × $0.10). *(walked 2026-07-26, NIM-000248: first walk caught the ADR-012 first-period regression — base billed "$28.06 prorated 30/31 days" on a full anniversary month (NIM-000247) — fixed in #615; re-walked post-fix: base $29.00 flat, no proration suffix, all 3 line periods == the invoice period, subtotal $54.00 + 10% tax = $59.40.)*

## FLOW I2: Negative usage

- [x] Ingest 1000 then -200 (correction) → meter shows net 800, billed for 800. *(walked 2026-07-26 on the I1 fixture, period 2: usage API total_quantity 800, upcoming-invoice card $8.00, cycle invoice NIM-000249 usage line qty 800 = $8.00 — ingest, preview, and invoice all agree on the net; a 0-usage meter emits no line.)*

## FLOW I3: Manual line items

- [x] POST /v1/invoices → draft. Add Line Item "Setup fee" $250, "Consulting" 2×$150 → total $550. *(walked 2026-07-26: NIM-000250, add_on lines, draft total 55000c.)*
- [x] Finalize → auto-charges via Stripe. *(walked 2026-07-26: manual 10% tax applied at finalize ($605 total), synchronous charge on the saved 4242 → paid/succeeded same request, real PI pi_3TxVMQ….)*

## FLOW I4: Void

- [x] Void invoice with credits applied → credits reversed, balance restored, Stripe PI canceled, active dunning resolved, audit log entry. *(walked 2026-07-27 as ONE compound void on NIM-000253 — $12 credits applied + failed PI pi_3TxXaE… + active dunning run: void → balance +1200, "payment intent canceled on void" logged, run resolved/manually_resolved, audit `void` row. Earlier by-legs walk superseded.)* *(walked 2026-07-26 BY LEGS: credits reversed + balance restored ✓ (NIM-000252), active dunning resolved `manually_resolved` on void ✓ + audit `void` row ✓ (NIM-000233); PI-cancel leg code-read only (invoice/handler.go void → CancelPaymentIntent, deliberately best-effort) — a live pending-PI fixture needs `stripe login` (CLI key expired).)*
- [x] **Uncollectible → Void reverses the committed tax exactly once (product-audit G1):** *(walked 2026-07-27 on NIM-000260 with a real committed Stripe Tax transaction: Mark Uncollectible stamped tax_reversed_at once; the subsequent Void filed NO second reversal — the invoice-stable `inv_taxrev_` reference held.)* on a `stripe_tax` invoice with a committed `tax_transaction_id`, Mark Uncollectible (files one tax reversal), then Void it. The Void files **no second reversal** — both transitions use the same invoice-stable reversal reference (`inv_taxrev_<id>`), so Stripe dedups to one reversal (verify: one reversal on the Stripe Tax transaction, not two). Pre-fix the two used distinct references (`inv_uncoll_` vs `inv_void_`) and reversed the tax twice → the tenant under-remitted. (Manual/none tax providers have no `tax_transaction_id`, so no reversal either way.)
- [x] **Void hands back applied customer credits:** *(walked 2026-07-26: NIM-000252, $12 applied of $88 → void → balance +1200, ledger `grant 1200 | Reversed — invoice NIM-000252 voided`; second void 409 "invoice is already voided", no double-credit. NOTE: a FULLY-credit-paid invoice lands `paid` and void is refused — "issue a credit note instead" — so this box's shape is the partial-application one.)* grant a customer credit, apply it to a finalized invoice (`amount_due` drops by the applied amount), then Void the invoice. The customer's credit balance **increases back** by the applied amount — a `grant` ledger entry `Reversed — invoice <num> voided` appears on the Credits tab. Voiding a **second** time (or via the dunning write-off path) does NOT double-credit.
- [~] **A failed credit reversal rolls the void back — never voided-with-credits-consumed (atomic):** *(2026-07-26: REVOKE-INSERT live variant blocked by session permissions; the contract is CI-locked by `TestVoid_CreditReversalAtomic` — reversal failure rolls the void back AND short-circuits the tax reversal; success path verified live above.)* on that same applied-credit invoice, `REVOKE INSERT ON customer_credit_ledger FROM velox_app`, then Void → the action **fails** (500) and the invoice stays **NOT voided** (status unchanged, `voided_at` NULL) and **no** tax reversal fires; `GRANT INSERT ON customer_credit_ledger TO velox_app` and retry → the void succeeds and the credit is handed back. Pre-fix the void committed first and the reversal ran as a best-effort post-step, so this failure left the invoice voided with the customer's credits silently stranded (no reconciler).

## FLOW I4b: Uncollectible invoice lifecycle

Mark-uncollectible (terminal bad-debt) + its page UX + offline recovery back to paid.

- [x] **Operator-driven uncollectible from the dunning resolve dialog** *(walked 2026-07-26 on NIM-000244: run → resolved/invoice_not_collectible AND invoice → uncollectible; banner + button changes verified.)* — on an active dunning run, click Resolve → pick **Write off invoice** → confirm. The dunning run flips to `resolved` with `resolution=invoice_not_collectible` AND the underlying invoice flips to `status=uncollectible` (cross-flow per ADR-036). Invoice detail page reflects the change: status banner reads "Marked uncollectible — recorded as bad debt", Collect Payment / Mark Uncollectible buttons disappear, Record Payment + Void + Issue Credit remain.
- [x] **Uncollectible page UX (Stripe parity — verified across Stripe + Chargebee + Recurly 2026-05-20)** *(walked 2026-07-26: banner copy exact, menu = Record-offline/Email/Copy/Rotate/Preview-PDF/Download-PDF/Issue-CN/Void; Collect/Mark-Uncollectible/Finalize/Add-Line absent; attention banner hidden.)* — on an `uncollectible` invoice: InvoiceAttention banner is hidden, OperatorContext/Diagnosis card is hidden, status banner explains the bad-debt classification + that the subscription stays active + recovery options. Buttons present: Void, Email, Issue Credit, Record Payment, Copy Link, Preview/Download PDF. Buttons absent: Collect Payment, Mark Uncollectible, Finalize, Add Line Item.
- [x] **Stripe-parity offline recovery: uncollectible → paid** *(walked 2026-07-26 on NIM-000244 with "Cheque #1234": paid/succeeded/paid_at, `out_of_band:` PI prefix, audit `recovered_from_status=uncollectible`, webhooks `invoice.payment_recorded` + `invoice.paid` both fired.)* — click Record Payment on an uncollectible invoice, optionally enter a reference (e.g. "Cheque #1234"), confirm. Invoice flips to `status=paid`, `payment_status=succeeded`, `paid_at` set, `stripe_payment_intent_id` prefixed `out_of_band:` so reports can distinguish operator-recorded payments from engine charges. Audit row carries `recovered_from_status=uncollectible`. Webhooks `invoice.payment_recorded` AND `invoice.paid` both fire (the latter from MarkPaid on every paid transition — card, credits, offline, dunning recovery). Active dunning run (if any) resolves to `payment_recovered`.

## FLOW I5: Collect + payment timeline

- [x] Finalized unpaid → POST /v1/invoices/{id}/collect → PI created. *(walked 2026-07-27: the charge path minted real PIs on the attach-then-decline card (auto-charge sweep, same charger); no-PM → 422 "customer has no payment method set up"; post-decline collect → honest in-flight/cooldown guard message.)* *(2026-07-26: no-PM negative path verified — 422 "customer has no payment method set up"; the PI-create mechanism verified via the finalize auto-charge (same charge path, real PI). A collect-specific pending-PI fixture needs `stripe login`.)*
- [x] GET /v1/invoices/{id}/payment-timeline → all attempts in order with ts/amount/status/PI id. *(walked 2026-07-26 on NIM-000250 + NIM-000238.)*
- [x] **Coalesced rows (ADR-020)**: a paid invoice shows ONE "Invoice paid · $29.00" row, NOT a separate "Payment succeeded" row beneath it. *(walked 2026-07-26: NIM-000250 — one invoice.paid row, $605.00.)*
- [x] A voided invoice with a previously-pending PI shows ONE "Invoice voided" row, NOT a duplicate "Payment canceled" row. *(walked 2026-07-27: NIM-000253 voided with its declined PI → one invoice.voided row, zero payment_intent.canceled rows, zero resolved twins.)*
- [x] A dunning-recovered invoice shows "Invoice paid · after 3 retry attempts" — no separate "Dunning resolved" row. *(walked 2026-07-26: NIM-000241 resolved payment_recovered → one invoice.paid row with attempt_count:3, no resolved twin. NOTE: the write-off + void resolutions still show a bare "Dunning resolved" twin row — fold gap queued for the I13 walk.)*
- [x] **Failure rows fold inside-out**: each failed charge collapses to ONE row carrying the dunning attempt label ("Payment recovery started" with its cause subline, or "Payment retry #N attempted"), the PI id, the amount, the decline reason, and a `Customer notified by email` sub-line. Rows carry their recorded cause as the subline: start = "Card was declined — automatic retries scheduled" vs "No payment method — reminders until a card is added"; card-less retries = "No payment method — reminder sent" with the reminder's ADR-098 delivery verdict threaded on when the provider reported one ("— delivered" / "— bounced"), paired by the email's attempt number — and only claiming "reminder sent" when the outbox row exists (never a bare charge-failure reading). *(walked 2026-07-27: NIM-000253's 0341 decline folded into the dunning row with pi_3TxXaE…, payment_failed reason, and the email sub-line — no separate Stripe row.)* No separate Stripe `payment_intent.payment_failed` row at the same instant; no separate "Payment-failed email sent" row beneath.
- [x] **Charged-card sub-line (ADR-020)**: paid invoice's "Invoice paid" row carries `via Visa •••• 4242` beneath the amount. *(walked 2026-07-26 on NIM-000250 saved-PM; 2026-07-27 on NIM-000248 hosted-pay — sub-line present on both.)* Holds even when the customer paid via the hosted-invoice URL **without saving the PM** (lookup goes directly to Stripe, not the local payment_methods table). Non-card PMs (bank, wallet) or Stripe lookup failures render no sub-line — graceful, not broken.
- [x] **Unpaired rows survive**: a Stripe `payment_intent.payment_failed` with no dunning twin (dunning disabled, or webhook arrived ahead of the dunning event) stays as its own "Payment failed" row. A payment-failed email row whose dispatch is still pending or failed stays visible — delivery problems must not silently disappear.
- [x] **Dunning-off failure lands in Activity at simulated time (ADR-102)**: on a clock-pinned invoice whose auto-charge declined while dunning was disabled, the **Activity** card shows "Payment failed" with the provider reason at the **simulated** instant (the charge-attempt fact) — not silence — and the wall-clock lane does NOT show a duplicate "Payment failed" webhook row (the attempt replaced its echo, lifting the "Customer notified by email" sub-line when one folded). With dunning ON, rendering is unchanged (recovery row owns the failure). The second card is always titled "Real-time activity" — it never renames itself. *(walked 2026-07-28 on NIM-000270: policy disabled, 0341 decline at cycle close → Activity shows created → finalized → "Payment failed / Your card was declined. / $34.10" at Sep 1, 2026 simulated time with the lifted email sub-line; no wall-lane duplicate. Dunning-ON regression on the next cycle: one "Payment recovery started" fold, attempt suppressed. Historical NIM-000269 renders unchanged via the webhook fallback tier. Constant title + caption verified live post-#643; CI: TestApplyChargeAttemptPrecedence + TestRecordChargeAttempt_UpsertContract.)*

## FLOW I5b: Invoice attention banner

Server-derived from invoice fields. Suppressed for healthy / paid / voided / draft invoices.

### Critical
- [ ] **payment_anomaly pierces terminal states** — an `amount_mismatch` or payment-received-on-VOIDED invoice shows the red anomaly banner even though the invoice is paid/voided (terminal states suppress every other banner, never this one).
- [x] **tax_location_required**: US customer missing postal_code, finalize → **amber warning** banner badged "Customer address required" (severity corrected 2026-07-27: the classifier deliberately ranks a draft's incomplete data as warning, not the red this box previously claimed), teaching message naming postal/country/tax-ID, primary **Edit billing profile**, secondary **Retry tax**, mono code `tax.customer_data_invalid`, raw Stripe 400 under the "Provider response" disclosure. Finalize itself is BLOCKED loudly ("tax calculation pending — operator action required"). *(walked 2026-07-27 live against Stripe Tax, NIM-000260.)*
- [ ] **tax_calculation_failed (provider auth)**: revoke Stripe key → red banner code `tax.provider_auth`, action **Rotate API key**.
- [x] **payment_failed**: card decline → red banner code `payment.declined`, message = truncated `last_payment_error`, actions `[Update payment method, Retry payment]` (ADR-023). *(walked 2026-07-27 on NIM-000256 via 4000-0341 attach-then-decline — 9995 declines at SETUP too, so 0341 is the staging card; severity critical, actions exact.)* Only ONE banner — no hardcoded duplicate below the unified one. Update payment method opens the **Send setup link** dialog (emails the customer a setup link); Retry payment calls Collect.

### Warning
- [ ] **tax pending**: amber banner with same code/actions, severity warning.
- [x] **payment_processing stale (ADR-049 Phase 4)**: a REAL (non-simulated) invoice left `payment_status=processing` for more than ~6h → the in-flight banner escalates Info → **amber Warning**, message points the operator at Stripe (does NOT promise auto-resolution). A clock-pinned (simulated) invoice stays **Info** no matter how "old" its sim-time is (the age is wall-clock, guarded on `!is_simulated`). *(walked 2026-07-28 on VLX-000011, Walkthrough Co: a non-simulated invoice held at payment_status=processing with updated_at backdated 9h renders the ESCALATED copy — "Payment has been awaiting confirmation at the provider for an unusually long time — check this payment in Stripe." — with a `since` relative time and amber severity, distinct from the fresh-processing muted banner. State set directly: the box asserts RENDERING for a given age, and the >6h threshold has no product flow that produces it on demand.)*

### Info
- [x] **payment_processing (fresh)**: muted banner, **no actions**, copy says Velox confirms it automatically (true via the synchronous inline settle / reconciler backstop — ADR-049 Phases 2–3). *(walked 2026-07-28 on VLX-000009: muted banner "Payment processing / Payment is in flight at the provider — Velox confirms it automatically when the provider responds.", code `payment.processing`, and NO action buttons.)*
- [x] **payment_unconfirmed**: muted banner, **no actions** — copy says Velox resolves it on the next reconcile. The previously-greyed-out "Check provider" button is gone (it had no endpoint; on-demand re-check deferred per ADR-049). *(walked 2026-07-28 on VLX-000010: muted banner, code `payment.unconfirmed`, NO actions — the previously-greyed "Check provider" button is gone; copy says Velox re-checks automatically and names the 3-D Secure possibility with the Stripe next step.)*
- [x] **payment_scheduled**: `auto_charge_pending=true` → muted banner, action **Charge now**. *(walked 2026-07-28 on VLX-000015, Walkthrough Co: muted banner, code `payment.scheduled`, single action **Charge now**. Fixture is CLEAN — a card-bearing customer's invoice finalized without a collect attempt, so no payment history contradicts the "not charged yet" claim; an earlier attempt reused a paid invoice and the page showed "Payment attempts: 1" beside it, which is why the rule is now: poke the state you are NOT asserting, never a state the page reports elsewhere.)*
- [x] **awaiting_payment**: muted banner, actions **Charge now** + **Email payment link**. *(walked 2026-07-28 on the same clean VLX-000015 with auto_charge_pending=false: muted banner, code `payment.awaiting`, actions **Charge now** + **Email payment link**, and no Diagnostic/attempts card.)*

### Banner shape
- [ ] Severity styling: critical=red+AlertCircle, warning=amber+AlertTriangle, info=muted+Info.
- [ ] Reason badge + dotted code in mono. `since` = relative time. `doc_url` → "Learn more ↗".
- [ ] `provider_response` (raw upstream payload) → `<details>` "Provider response" disclosure; `detail` (Velox framing) renders as its own "Detail" disclosure.
- [ ] Healthy/paid/voided/draft → no banner.

### Retry tax
- [x] **A healthy one-off invoice shows no "Tax retries" diagnostic (fixed 2026-07-26):** finalize a manual invoice with manual-provider tax → `tax_retry_count` stays **0** (the finalize-time first computation is not a retry) and the invoice page renders **no** "Diagnostic detail · Tax retries" card. Retries still count uniformly: the Retry-tax button, the background worker, and a re-finalize of a STUCK draft each bump the counter — for cycle and manual invoices alike. *(operator-reported on NIM-000258; automated + mutation-verified: `TestUpdateTaxAtomic_InitialAttemptDoesNotCountAsRetry`.)*
- [ ] Banner showing → click **Retry tax** → button "Retrying…" → audit log row `action='retry_tax'` with before/after attention codes.
- [ ] Issue fixed → invoice has `tax_status='ok'`, banner disappears, toast "Tax recalculated successfully".
- [ ] Still failing → banner refreshes with new reason. Each click bumps `tax_retry_count`.
- [x] Retry on non-pending/non-failed invoice → 409. *(walked 2026-07-27: saving the postal code fired the billing-profile flush which auto-retried the stuck invoice to tax_status=ok ("billing profile flush retried tax errors processed=1"), so the subsequent manual Retry-tax click hit the 409 exactly as specified.)*
- [ ] **Auto-finalize (ADR-017)**: subscription-cycle invoice in `tax_status=pending` → click Retry tax with the underlying issue fixed → invoice flips to `status='finalized'` automatically (one click, not two). Status pill goes Open; hosted-invoice URL appears; auto-charge flow kicks off if a PM is on file.
- [x] **Manual drafts stay draft**: create a manual invoice, force its tax to pending via tooling, fix the issue, click Retry tax → tax becomes ok BUT invoice stays draft (operator must finalize explicitly). *(walked 2026-07-27: NIM-000260 stayed draft through the flush-driven recompute; explicit finalize then committed real Stripe Tax — tax_1TxYyU…. A NY-address twin proved nonzero end-to-end: NIM-000261, $8.88 at 8.875% committed. The earlier CA $0 was CORRECT — CA does not tax the tenant's SaaS tax code.)* Toast: "Tax recalculated successfully".

### Background tax-retry reconciler (ADR-017)
- [ ] Force a subscription-cycle invoice into `tax_pending` with `tax_error_code='provider_outage'` and `tax_next_retry_at IS NULL` (e.g. by simulating a Stripe Tax 5xx during finalize). Watch the scheduler tick (default 5min in local) — within one tick the invoice should retry; if the underlying issue resolved, it auto-finalizes.
- [ ] Same shape with `tax_error_code='customer_data_invalid'` → reconciler does NOT touch it (non-retryable code). Manual operator action still required.
- [ ] After 8 attempts the row exits the reconciler scan: `psql -tAc "SELECT id, tax_retry_count, tax_next_retry_at FROM invoices WHERE id='vlx_inv_xxx';"` shows `tax_retry_count=8`, `tax_next_retry_at=NULL`. Banner stays live for the operator; worker stops.
- [ ] Backoff respected: the 1st retry is ~5min ahead (±10% jitter), the 5th ~12h ahead (schedule `[5min, 15min, 1h, 4h, 12h, …]`). Sub-5-min ticks don't double-process the row.
- [ ] **Tax-commit reconciler recovers a lost transaction id:** on a finalized `stripe_tax` invoice that has a `tax_transaction_id`, simulate the commit-succeeded-but-persist-failed orphan: `psql -c "UPDATE invoices SET tax_transaction_id=NULL WHERE id='vlx_inv_xxx'"` (the Stripe transaction still exists upstream). Within one scheduler tick, `slog` logs `tax commit recoveries … recovered=1` and `psql` shows `tax_transaction_id` **repopulated with the SAME `tx_*`** — the idempotency key returned the original transaction, NOT a duplicate (check the Stripe Tax dashboard: still one transaction for that invoice). Now the invoice can be voided with a proper reversal. A `manual`-provider invoice (no `tax_calculation_id`) is never picked up.

### List + draft cleanup
- [x] `/invoices` rows: severity-tinted dot next to invoice number; tooltip surfaces typed reason. Healthy/draft = no dot. *(walked 2026-07-28 across a 11-row list covering every state: destructive dot for payment_failed ("The customer's card was declined."), amber for stale-processing and dunning_exhausted, blue for fresh processing / unconfirmed — each tooltip carries the TYPED reason + operator copy — and NO dot at all on paid, voided, or draft rows.)*
- [x] Draft rows show "draft" pill (Dashboard) or em dash (Invoices, Subscription detail) instead of `payment_status='pending'`. *(walked 2026-07-28: VLX-000007 renders `draft | —` on /invoices — the em dash, never `pending`.)*
- [x] Invoice detail draft row: muted "Draft invoice — finalize to issue and begin collection." hint. *(walked 2026-07-28 on VLX-000007: the muted hint reads verbatim "Draft invoice — finalize to issue and begin collection." and NO attention banner renders on a draft.)*
- [x] **No-payment-method nudge (customer WITH email) resends the SETUP link, not the invoice email** *(walked 2026-07-26 on NIM-000253: present-tense card copy, code payment.no_payment_method, Resend setup link → new "Set up payment for invoice NIM-000253" Mailpit email, no recipient dialog.)* — on that finalized-unpaid no-card invoice for a customer that **has an email on file**, the attention card states the behavior in present tense (*"Velox emails the customer a setup link when an invoice finalizes…"* — never a past-tense send it can't prove) and its button reads **"Resend setup link"**. Click it → a "Setup link resent" toast (no recipient dialog) and Mailpit shows another **"Set up payment"** email (`POST /v1/invoices/{id}/resend-setup-link`) — the same Checkout-setup link, NOT the hosted-invoice "pay this invoice" email. For an invoice in a different attention state (e.g. `payment_failed`), the `send_reminder` button still opens the email dialog and sends the hosted-invoice pay link.
- [x] **No-payment-method nudge (customer with NO email) shows the honest variant** *(walked 2026-07-28 on NIM-000268: email-less clock-pinned customer, arrears cycle close → payment.no_payment_method; banner copy verbatim incl. "or add an email so it can be sent"; only Open customer page + Learn more — no Resend setup link; dialog's Send email disabled with "No email on file" placeholder; Copy link minted a real cs_test_… Checkout setup URL.)* — repeat on a finalized-unpaid no-card invoice for a customer with **no email address on file** (create one without an email, or clear it). The card now reads *"…We email a setup link only when the customer has an email address on file — open the customer page to copy a secure setup link…"* and offers **only "Open customer page"** — the **"Resend setup link" button is gone** (a resend can't send with no recipient). Open the customer page → **Add payment method → Copy link** mints a setup URL to hand to the customer directly.
- [x] **Clock-aware auto-charge tail** — the no-PM banner on a test-clock invoice ends *"…auto-charges this invoice on the next test-clock advance"*; on a wall-clock invoice, *"…on the next billing tick — no operator action needed"*. No second client-rendered copy of the promise below the banner. *(walked 2026-07-22 on NIM-000234 live: "next test-clock advance" tail + no duplicate line.)*
- [x] **Diagnostic detail agrees with the banner on a card-less invoice** — the run row reads "Payment recovery · No retries yet" (or "N retries", never "attempt 0"), and the date row is **"Next reminder"** with muted "Won't charge until a card is attached." Decline (has-card) dunning keeps "Next retry". *(walked 2026-07-22: NIM-000234 "No retries yet"/"Next reminder"; NIM-000214 "ended · 3 retries".)*
- [x] **No dead Collect button** — with no PM on file the disabled "Collect Payment" primary is absent; it returns once a card is attached. *(walked 2026-07-22 on NIM-000234: disabled primary gone, banner actions only.)*
- [x] **dunning_exhausted** — let a no-PM run exhaust (final action fires, run `state=escalated`) → banner becomes *"Payment recovery has ended without collecting…"* (code `payment.dunning_exhausted`, label "Payment recovery ended"), still offering Resend setup link (with email) / Open customer page; Diagnostic badge reads **ended** with no future-step row. Settling the invoice clears it. *(walked 2026-07-23 on NIM-000214 live: banner + code + "ended" badge, no future-step row.)*

## FLOW I6: Email + PDF preview

- [x] Invoice detail → Email → outbox row → PDF attached → Mailpit shows delivery. *(walked 2026-07-26: NIM-000253.pdf attached, multipart text+HTML, delivered.)*
- [x] Preview PDF → renders in overlay; close via X / backdrop. *(walked 2026-07-26 — opens in dialog frame, Escape closes.)*
- [x] **Emailed PDF == downloaded PDF == hosted PDF** — for a customer with a billing profile (address + tax ID) and an issued credit note, all three PDFs carry the buyer's address, `Tax ID:` line, and the credit-note block (the emailed one used to omit all three). *(walked 2026-07-27: NIM-000250 + CN-000032, billing profile with DE address + eu_vat `DE123456789`; all three fetched — `/pdf` download, `/v1/public/invoices/{token}/pdf` hosted, Mailpit attachment — text layers IDENTICAL modulo the generated-at stamp; each carries the Bill-To block, `Tax ID: DE123456789`, and the CN-000032 post-payment-adjustments block.)*
- [x] **Emailed amount = amount due** — on an invoice with credits applied, the email's "Amount due" card shows the post-credit residual, not the total. *(walked 2026-07-26: NIM-000253 total $55, credits $12 → email card AND subject read USD 43.00.)* The payment receipt states the amount actually charged.
- [x] **Uncollectible invoice's hosted page is honest** — a `mark_uncollectible` dunning outcome's escalation email links to a hosted page that offers NO Pay button and says so. *(walked 2026-07-28 on VLX-000017: escalation email CTA reads **"View invoice"** (not a Pay CTA that would dead-end) and the page renders "This invoice is closed / Online payment is no longer available for this invoice. Please contact support to settle it or restore service." THIS WALK FOUND THE EMAIL WAS NEVER SENT AT ALL: the outbox staleness gate treated `uncollectible` as "settled", and mark_uncollectible marks the invoice in the same breath as it enqueues the escalation — so the one email whose purpose IS that outcome was skipped every time, and the template's uncollectible branch was unreachable in production. Fixed: the gate now asks WHICH terminal state, and an escalation announcing an uncollectible invoice delivers; paid/voided still mute it, and the other action-required types stay muted on uncollectible.)*

### Branded HTML body

Multipart text+HTML with tenant chrome. Configure tenant `company_name`, `logo_url`, `brand_color`, `support_url`.

- [x] Invoice email HTML: tenant logo + name in header, 3px brand-color accent bar, "Amount due" card, **View & pay invoice** CTA. *(walked 2026-07-28 on VLX-000014 with branding set (logo, #0e6b58, support URL): all present, subject "Invoice VLX-000014 — USD 60.00", CTA text verbatim "View & pay invoice", PDF attached. DOC CORRECTED: this box previously also asserted a "line-items summary" — the template has never carried one (renderInvoiceHTML takes only name/number/amount/hostedURL), so the claim was aspirational, not a regression. Registered as an enhancement with a trigger rather than silently dropped; the PDF attachment and hosted page both carry the lines today. A zero-due invoice sends the "— no payment due" variant instead.)*
- [x] CTA URL → `{HOSTED_INVOICE_BASE_URL}/invoice/{public_token}`. *(walked 2026-07-26 — vlx_pinv_ + 64 hex.)*
- [x] Footer: "Contact support" + "Powered by Velox Billing". *(walked 2026-07-28: both present on invoice, receipt and escalation emails; Contact support resolves to the tenant's configured support URL.)*
- [x] Plain-text part still present. *(walked 2026-07-26.)*
- [x] Receipt email: same chrome, CTA "View receipt". *(walked 2026-07-28 on VLX-000013's "Payment received" email: tenant logo + brand bar + company name + footer, CTA verbatim "View receipt".)*
- [x] Dunning email: attempt N of M, next retry date, CTA **"Pay invoice"** while the invoice is still payable / **"View invoice"** once it is not. *(walked 2026-07-28: warning email reads "Attempt 1 of 3 to charge invoice VLX-000002 was declined. We'll try again on August 7, 2026" with a "Pay invoice" CTA; the mark_uncollectible escalation (VLX-000017) renders "View invoice". DOC CORRECTED: the box previously said the escalation CTA is "Resolve invoice" — no such label exists; the template deliberately switches on whether the invoice is still payable so the CTA never dead-ends.)*
- [ ] Payment-update-request email: CTA uses `PAYMENT_UPDATE_URL` token URL.
- [ ] Operator emails (password-reset, member-invite) intentionally plain text — no HTML chrome.

## FLOW I7: Zero-amount invoice

- [x] **Finalizing a zero-due manual invoice lands it PAID, not "awaiting payment" (ADR-066/087)** *(walked 2026-07-26: NIM-000254 — paid/succeeded/paid_at on finalize, no PI, no retry flag, total 0.)* — create a manual invoice, add no line items, Finalize. Status is **Paid** immediately (`payment_status=succeeded`, `paid_at` stamped) — no charge attempt, no payment-method email, no retry flag; it must never sit finalized/awaiting-payment.

- [x] Plan `base_amount_cents=0`, no meters → either no invoice or $0 auto-paid (no Stripe charge). *(walked 2026-07-28: $0 monthly plan, clock-pinned sub, advanced through TWO cycle closes → the "no invoice" arm: zero invoices, zero dunning runs, no PI — the engine's deliberate skip-empty-cycle-close guard (engine.go "Skip empty cycle-close invoices", Stripe/Lago/Chargebee/Orb convention), periods kept rolling normally.)*

## FLOW I8: Currency coherence (ADR-100 guard ring)

- [x] Republish a plan-bound price in a different currency → 409 naming the plan(s); same-currency rate change → new version OK. Archive the (sub-less) plan → the different-currency republish now passes (the wrong-currency repair flow). *(CI-locked `TestCurrencyRing_RepublishBlockedWhilePlanBound` / `ArchivedPlanRepairFlow`, real-Postgres, green 2026-07-26.)*
- [x] Create an EUR plan wiring a USD-ruled meter → 409 naming the price key; same for ATTACHING the meter via plan update, for binding an EUR price onto a USD meter (pricing-rule row or default binding), and for a JPY/KRW/VND/CLP currency anywhere (zero-decimal: refused loud with the minor-unit explanation — Stripe would read the cents as whole units, a 100× charge). *(CI-locked `TestCurrencyRing_*`.)*
- [x] Customer currency pin: set `billing_profile.currency=EUR` first → creating a USD-plan sub 409s (ordering bypass closed); with an EUR sub live, a USD second sub 409s naming it; a cross-currency plan swap 409s even on a single-item sub (raw-cents proration); a draft that diverged before the pin refuses at Activate. Coherent twins all pass. *(CI-locked `TestCurrencyPin`, real-Postgres.)*
- [x] **One-off invoices follow the customer pin by default (2026-07-26):** `POST /v1/invoices` with no explicit currency resolves billing-profile currency → tenant default → USD — a GBP-pinned customer's bare API create previously minted a silent USD invoice (the composer masked it by sending its picker value). Explicit input still wins (one-off charges may deliberately denominate differently — they join no proration math) and still hits the zero-decimal 422. *(found live on i8-gbp-cust; automated: `TestCreate_CurrencyResolvesProfileThenTenantDefault`.)*
- [x] Second AI recipe in a different currency → 409 at meter adoption naming the conflicting price ("a meter's prices share one currency"); same-currency install adopts fine. *(CI-locked `TestService_Instantiate_RefusesCrossCurrencyMeterAdoption`.)*
- [ ] Invoice-header precedence (what the engine actually does, comments truthed 2026-07-26): billing profile > first item's plan > tenant settings (dead branch — plans always carry currency) > USD. Tenant-default switches never re-label existing subs' invoices; post-ring, all sources agree by construction.
- [ ] Webhook settle in a currency ≠ invoice header → `payment.currency_mismatch` anomaly on the invoice (report-only tripwire; case-folded).

## FLOW SUB7: Mid-period change outcome on the timeline + invoice

- [ ] On a clock-pinned sub, do each mid-period change and check the subscription Activity feed shows the financial outcome, not just the intent — every $X is GROSS (the invoice total incl. tax / the gross credited; B12 adjudication 2026-07-21): plan upgrade → "Plan changed · … · Proration invoice $X"; downgrade on a PAID prebill → "… · Credit $X"; downgrade on an UNPAID prebill → "… · Open invoice adjusted $X"; quantity increase → "Quantity changed · … · Proration invoice $X"; item add → "Item added · … · Proration invoice $X"; item remove → "Item removed · <plan> · Credit $X". Non-USD tenant shows the right currency symbol, never a hardcoded $.
- [ ] Open the UNPAID invoice that a downgrade adjusted: the credit-note row shows the reason ("Plan downgrade") AND "↳ Tax reversed $X (Stripe Tax)" — same disclosure a paid invoice's post-payment refund shows. Pre-fix the unpaid row was a bare "Credit CN-NNNN -$X".
- [ ] Credit Notes page: an adjustment CN (no refund/credit/out-of-band) shows "applied to invoice" in the channel column (not "—"), and the reason renders as "Plan downgrade" not "subscription_downgrade".

## FLOW I9: Credit note on void

- [x] Void invoice → issue CN → error "cannot create credit notes for voided invoices". CN not created. *(walked 2026-07-26: 409, exact copy, zero CN rows.)*

## FLOW I9b: Credit note PDF totals reconcile

- [x] On a taxed paid invoice (e.g. $100 net + 10% = $110), create + issue a full CN (one line, qty 1 × $110.00 gross). Download the CN PDF: line amount **$110.00**, totals rows read **"Total excluding tax" $100.00**, tax row **$10.00**, **"Credit Total" $110.00** — line amounts sum to Credit Total; no row claims to be a sum-of-lines "Subtotal" that doesn't match. *(walked 2026-07-26: CN-000032 on NIM-000250 — line $605.00 gross, "Total excluding tax" $550.00, "Sales Tax (10%)" $55.00, "Credit Total" $605.00, PDF verified visually.)*
- [ ] CN numbers are sequential per tenant (CN-…-0001, -0002). A failed number allocation FAILS the Create loudly; no CN with a timestamp-shaped number (CN-YYYYMM-…) is ever created.

## FLOW TR-CXL: Trial cancellation (ADR-069)

- [ ] Trialing sub → Cancel dialog first option reads **"At trial end"** with "won't be charged" copy; confirm → banner shows "at trial end (<trial_end date>)", GET returns `cancel_effective_at == trial_end_at`.
- [ ] Advance the test clock past trial end → sub is **canceled**, `canceled_at == trial_end_at`, invoice list shows **NO invoice**, timeline shows one cancellation entry, webhook log shows one `subscription.canceled` (reason `trial_end_cancel`) and **zero** `subscription.trial_ended`.
- [x] **Lifecycle events ride the transition tx (2026-07-05):** `SELECT event_type, payload FROM webhook_outbox` right after any create/activate/cancel/end-trial → `subscription.created` / `.activated` / `.canceled` / `.trial_ended` rows exist **before** any dispatcher tick (enqueued IN the transition tx, exactly once per transition; a rolled-back cancel leaves no row). Payload keeps the provenance fields (`canceled_by`, `reason`, `triggered_by`). *(automated: `TestLifecycleEvents_EnqueuedInTransitionTx`)*
- [ ] Schedule the cancel, then **Undo** before trial end, advance the clock → sub ACTIVATES normally and bills period 1 (the rescind won).
- [ ] Schedule, then **Extend trial** with an explicit cancel date pending → 409 "clear the scheduled cancel first"; with the flag-only schedule → extension succeeds and the banner's date moves to the new trial end.
- [ ] "End trial now" is disabled (with reason) while a cancel is scheduled; via API → 409.
- [ ] Explicit `cancel_at` strictly between trial end and period end → 400 naming both valid boundaries.
- [ ] Immediately cancel a trialing sub whose trial just elapsed (scheduler hasn't activated it) → NO final invoice.

## FLOW C-ARCH: Archive semantics (ADR-067)

- [ ] Customer with an ACTIVE subscription → Archive → 409 toast naming the subscription; customer stays active. Same for trialing and scheduled-cancel subs.
- [ ] Cancel the subscription(s) → Archive succeeds; customer hidden from active list; unpaid invoices remain payable and dunning reminders continue.
- [ ] Archived customer: `POST /v1/subscriptions` → 409 "customer is archived". Unarchive → create succeeds.
- [ ] Archived plan: create/swap onto it → 409; existing subs on the plan keep billing.
- [ ] Billing profile: set currency `EUR` while an active sub's plan is USD → 409 explaining the re-denomination; `usd` (lowercase) on a USD plan → saves as `USD`; `EURO` → 400.

## FLOW I10: Hosted invoice page

- [x] **Test-mode banner (2026-07-05):** open a hosted invoice minted from a **test-mode** key *(walked 2026-07-27 on NIM-000248: amber banner verbatim; payload livemode:false)* → amber "Test mode — this invoice is a test and no real payment will be collected." banner above the status banner; the payload carries `livemode: false`. A live-mode invoice shows no banner. The public cost-dashboard JSON (`GET /v1/public/cost-dashboard/{token}`) also carries `livemode` for embed banners.
- [x] **One live payment link per invoice (ADR-068):** click Pay twice quickly (or from two browsers) → both land on the SAME Stripe session URL; pay it once → the invoice settles once. `checkout_sessions` shows one open row flipping to `invoice_settled` on payment. *(walked 2026-07-27: two Pay clicks → identical cs_test_a1fnLO… session; paid once; claim row → invoice_settled; the settle's best-effort expire of the completed session WARNs harmlessly per the ExpiresAt-backstop design.)*
- [ ] **Stale session dies on settle/void/credit:** open the Pay page (don't pay), then mark the invoice paid offline (or void it, or apply covering credits) → the claim row closes in the same operation, a new POST /checkout 409s (`not_payable`), and the old Stripe session is expired (or dies within 1h).
- [ ] **Drifted amount never mints blind:** open a session, apply a partial credit note (amount_due drops), click Pay again → a NEW session at the new amount (old superseded). If the customer had already completed the old session, Pay 409s `payment_in_progress` and the settle raises `payment.amount_mismatch` with a Critical banner on the invoice.
- [ ] **Duplicate charge is loud:** simulate a second different-PI success on a paid invoice (Stripe CLI resend with a new PI) → invoice shows the Critical "second payment succeeded" banner naming both PIs; `payment.duplicate_charge` fires; same-PI redeliveries stay silent.
- [x] **Post-payment settle poll (ADR-067 companion):** pay via the hosted page → on the `?paid=1` redirect the page shows "Processing your payment…" with NO Pay button, then flips to Paid within a few seconds without a manual refresh. Simulate a failed charge → red "Payment didn't complete — your card was not charged" banner and Pay returns. Stall the webhook >3 min → amber "taking longer than usual — you will not be charged twice" copy, Pay still hidden. *(walked 2026-07-28 by killing `stripe listen` before paying: NIM-000265 showed the green Processing interim with no Pay button, then the amber "Confirmation is taking longer than usual — you will not be charged twice" copy after the 3-min poll cap; restarting the forwarder + `stripe events resend` settled it to paid. Failed-charge leg on NIM-000266: red banner + Pay returned.)*

- [x] Draft invoice has no `public_token`. Finalize → token minted (`vlx_pinv_` + 64 hex). *(walked 2026-07-28 on VLX-000018: the draft's create response carries public_token=null; finalize mints `vlx_pinv_` + exactly 64 lowercase hex.)*
- [ ] Detail page: **Copy Link** button. **Rotate** typed-confirm dialog (type `ROTATE`). Buttons hidden on draft.

### Public render (open in incognito)
- [x] Loads without login. Header: tenant logo + company_name + support_url. Optional 3px accent bar. *(walked 2026-07-27.)*
- [x] Invoice meta: number (mono), amount due (large tabular), due date. *(walked 2026-07-27.)*
- [x] Bill-to + From columns. Line-items table with tabular numerals. *(walked 2026-07-28 on the same page: Bill-to and From columns render, line items in a table with tabular numerals, totals block beneath.)*
- [ ] Totals: subtotal, optional discount, optional tax with rate, reverse-charge **or tax-exempt** notice if applicable (`Tax-exempt — <reason>` for an exempt customer; previously dropped on the hosted page), total, amount paid, **Amount due** bold.
- [x] **Pay {amount}** primary button (brand_color). **Download PDF** secondary. *(walked 2026-07-27.)*
- [x] Footer: "Secured by Stripe" + "Powered by Velox Billing". *(walked 2026-07-27.)*

### Pay
- [x] Click Pay → `POST /v1/public/invoices/{token}/checkout` → Stripe Checkout. Pay with `4242…` → redirect to `{baseURL}/invoice/{token}?paid=1`. *(walked 2026-07-27, NIM-000248.)*
- [x] Provisional "Processing your payment…" banner (green spinner). *(walked 2026-07-28 — see the settle-poll box above.)*
- [x] Webhook arrives → invoice paid → page auto-refetches → "Paid on {date}" banner; Pay button gone. *(walked 2026-07-27: flipped to "Paid · Received on Oct 5, 2026" — sim-date for the pinned customer — within seconds; Pay gone; real PI on the invoice.)*
- [x] PDF still downloads on paid. *(walked 2026-07-27 — 200, 35KB.)*
- [x] **Card saved to customer (ADR-021)**: after a successful Pay, dashboard customer detail shows the card under "Payment method" with brand + last 4 + status "Active". *(walked 2026-07-27: hosted 4242 pay on a no-PM customer → "payment method setup completed" + payment_methods row visa/4242; the Stripe customer was lazily minted.)* The next invoice for this customer auto-charges this PM (no email + Update Payment round-trip). Holds for a customer who had no PM on file before — the Stripe customer is lazily minted on first Pay.
- [x] **3-D Secure challenge (`4000 0027 6000 3184`)**: Pay → the bank challenge dialog appears; while it is open the invoice stays `finalized`/`pending` (no premature settle). **Fail** the challenge → checkout shows "unable to authenticate your payment method", `payment_intent.payment_failed` delivers, invoice flips `payment_status=failed`, no money moves. Retry on the same session and **Complete** → `requires_action` → `succeeded` + `checkout.session.completed` webhooks, invoice paid, and the `?paid=1` return shows the green Paid banner in the same session. *(walked 2026-07-28: NIM-000264 $20.90 — fail leg then complete leg on one checkout session, states verified in DB + stripe listen at each step.)*
- [x] **Interactive decline suppresses email (ADR-023)**: Pay with `4000 0000 0000 0002` (decline) → invoice goes to `payment_status=failed` → activity timeline shows the lifecycle row "Payment failed" but NO "Payment-failed email sent" row (customer was watching). Mailpit shows zero new emails. Auto-charge decline (e.g. dunning retry) still emails — only the interactive flow suppresses. *(walked 2026-07-28 on NIM-000266: `payment_intent.payment_failed` delivered, `payment_status=failed`, Mailpit count unchanged. Timeline: the failure renders as "Payment recovery started" with the decline cause subline (dunning-start-cause fix + uniform-title revision, 2026-07-28) and the PI folded on; no email row appears, which is the ADR-023 assertion.)*

### Variants
- [x] Voided invoice → "Voided on {date}" banner, no Pay, PDF works. *(walked 2026-07-28 on VLX-000018: banner "Voided on Aug 20, 2026 — this invoice is no longer owed.", zero buttons (no Pay), and the PDF still serves through the public token — HTTP 200, application/pdf, 34.5 KB. FOUND + FIXED while walking: the totals block still labelled the surviving figure "Amount due" on a voided (and uncollectible) invoice — amount_due_cents survives those transitions by design, so the page told a customer they owed money we had stopped collecting. Now reads "Invoice amount", matching the same-day fix on the payment-update page.)*
- [x] Draft invoice URL → 404. *(walked 2026-07-28: a well-formed but unminted token on the public route returns HTTP 404 — no distinction leaked between "draft" and "never existed".)*
- [x] Rotated → old URL 404, new works. *(walked 2026-07-26 via rotate-public-token: old 404, new 200.)*

### Security
- [x] Public JSON has no `tenant_id, subscription_id, tax_id, stripe_*_id`. *(walked 2026-07-26 — clean at both envelope and invoice levels; payload carries livemode:false for the test-mode banner.)*
- [x] 61+ req/min same IP → 429 with `Retry-After`. *(walked 2026-07-26 with Redis up: 60 pass, then 429 + `Retry-After: 60`. The earlier "fails open, no warning" note was wrong on both counts: with Redis down the boot WARN "redis not reachable — general/hosted rate limiters FAIL CLOSED in production…" fires, dev fail-open is the deliberate OWASP-anchored split, and production fails CLOSED — router.go documents the design.)*
- [x] Operator `POST /v1/invoices/{id}/rotate-public-token` requires `PermInvoiceWrite`. *(walked 2026-07-28 on VLX-000018: a **publishable** key (the type that holds no tenant-wide scopes) gets 403 `forbidden` — "insufficient permissions: this key type does not have invoice:write access" — while the operator session rotates the token to a fresh vlx_pinv_. NOTE for future walks: Velox permissions are TYPE-derived, not per-key — an `api-keys` create body carrying a `permissions` array is silently ignored, and the type field is `key_type` (a `type` key silently yields a full secret key). Testing an authorization gate therefore requires choosing the right key TYPE.)*

## FLOW I11: `create_preview`

- [x] `POST /v1/invoices/create_preview {subscription_id}` → invoice shape with `id=null`, no DB row. *(walked 2026-07-26 — lines/totals shape, zero invoice rows created; requires customer_id too.)*
- [x] No `audit_log` row from a preview: open a customer detail page (the upcoming-invoice card fires `create_preview` on load), then open `/audit-log` → **no** new "Created invoice" row. *(walked 2026-07-26 — audit_log count unchanged across preview calls.)*
- [ ] Plan-change confirmation dialog renders preview before commit.
- [x] Cost-dashboard projection populated when engine returns a value. *(walked 2026-07-26 — the customer page's upcoming-invoice card rendered base $29.00 + Usage $8.00 from the preview.)*
- [ ] **`in_advance` preview** (ADR-031): for a sub on an `in_advance` plan, preview's `billing_period_start/end` is the **upcoming** period (matches what the cycle invoice will stamp). Base line description carries the "in advance for upcoming period" suffix. Usage line totals match the elapsed period (per the engine's stamping). Totals identical to in_arrears preview — only the period labels differ.

## FLOW I12: One-off invoice composer

- [ ] Customer detail → "New invoice" → composer shows Currency + Payment terms (Net 30 default) + line editor.
- [ ] Enter three lines at `3333` / `3333` / `3334` cents → Subtotal ticks to $100.00; Tax row reads "Calculated at finalize".
- [ ] Save draft → exactly ONE `POST /v1/invoices` (no follow-up `add-line-item` calls); row appears with `status=draft`, `subscription_id=null`, `billing_reason=manual`, `tax_amount_cents=0`.
- [ ] Tenant Tax = manual 7.25%, Finalize → `tax_amount_cents=725`, `SUM(line tax)=725` (residual on last line), `total_amount_cents=10725`, `due_at = issued_at + 30d`.
- [x] **Manual invoice on a clock-pinned customer discloses simulation (ADR-030 addendum, ADR-099)** *(walked 2026-07-26: NIM-000257 — detail-page test-clock banner, list-row Simulated chip, issued at sim-time Sep 26, is_simulated=true.)* — pin a customer to a test clock (no subscription needed), Advance the clock, then create a one-off invoice from Customer Detail → New invoice. The invoice's detail page shows the amber test-clock **banner** and its Invoices-list row the **Simulated** chip, the issued/paid dates are simulated frozen-time, and `invoices.is_simulated = true` in DB.
- [x] **A customer's credit balance pays a manual invoice at finalize (ADR-088)** *(walked 2026-07-26 during I4: NIM-000251 fully covered → Paid, balance drained; NIM-000252/253 partial application → due reduced by exactly the balance.)* — grant a customer credit, compose a one-off invoice above the balance, Finalize. The invoice shows Credits Applied for the full balance and the card is charged exactly the remainder; with balance ≥ total the invoice lands **Paid** with no charge. The credit ledger shows the applied entry; balance is drained.
- [x] **One-off invoice due countdown reads simulated time on the Invoices list (2026-07-08)** *(walked 2026-07-26: NIM-000257 Net-7, clock advanced past due → list badge "Past due 2d" red, counted in sim-time not wall-clock.)* — finalize that one-off invoice with a short term (e.g. Net 7), then advance the clock past its due date. On the **Invoices list**, the row's **due badge** reads **"Past due"** (red), matching the simulation — NOT a green "Due in Nd" counted against wall-clock. (Before the fix, a one-off invoice — no subscription — fell through the sub-only clock lookup to wall-clock.) A one-off invoice on a **wall-clock** (non-pinned) customer still counts against real time.
- [x] **The composer's Terms are honored exactly** *(walked 2026-07-26: Net 7 → due = issued + 7d exact (NIM-000257); Due on receipt → 0 days, Due == Issued, header reads "Due on receipt" (NIM-000258).)* — on that one-off invoice the header **Terms** matches the term picked (e.g. Net 7 → "Net 7") and equals `Due − Issued`, **not** the tenant default; picking **Due on receipt** is honored as 0 days → `Due == Issued` and Terms reads "Due on receipt" (NOT silently coerced to Net 30).
- [x] **The Period field is absent unless a Service period is set** *(walked 2026-07-26: NIM-000257 has no period → no Period row; NIM-000258 with service period shows it.)* — a one-off invoice has no billing cycle by default (see the service-period item below).
- [x] **Manual invoice Issued/Due anchor to finalize, not draft-create** *(walked 2026-07-26: NIM-000258 composed Sep 26 sim, clock advanced, finalized Oct 5 → Issued Oct 5; Activity shows created Sep 26 vs finalized Oct 5 as distinct rows.)* — compose a draft on a clock-pinned customer, **Advance the clock**, then Finalize. The header **Issued** date is the *finalize* moment (the advanced clock time), NOT the earlier draft-create time, and **Due** = Issued + the term. On the activity timeline the **Invoice created** and **Invoice finalized** rows show *different* timestamps (created = compose time, finalized = the later finalize time), not the same instant. Cycle invoices are unaffected (born finalized at build time).
- [x] **Manual invoice with a service period** *(walked 2026-07-26: NIM-000258 — detail Period reads "Jun 1, 2026 – Jun 30, 2026" inclusive over the half-open wire; dialog enforces both-or-neither.)* — Customer Detail → **New invoice** → add a line → set **Service period** to e.g. `2026-06-01` to `2026-06-30` (inclusive) → create. The invoice **Period** reads **"Jun 1, 2026 – Jun 30, 2026"** (inclusive last day) on the detail page, the Invoices-list Period column, and the PDF. Leaving the Service period blank creates a one-off invoice with **no** Period row (unchanged). Entering only one of the two dates, or a start after the end, is rejected in the dialog (both-or-neither, start ≤ end).
- [x] **Manual invoice collection mirrors cycle invoices** *(walked 2026-07-26: no-card NIM-000256 → setup-payment email + auto_charge_pending=true, no invoice email; with-card NIM-000250 → synchronous auto-charge + "Payment receipt emailed" timeline row, no invoice email.)* — finalize a manual invoice for a customer with **no saved card**: no "your invoice" email is auto-sent; instead the customer receives a **"set up payment method"** email and `invoices.auto_charge_pending = true` (the scheduler charges automatically once a card is attached). For a customer **with a saved card**: the invoice auto-charges, the customer gets a **payment receipt** on success (no separate invoice email), and `auto_charge_pending` stays false. Either way the operator can still send the invoice explicitly via the **Send** button. Matches what a subscription-cycle invoice does at finalize.

## FLOW I13: Timeline completeness

- [ ] Tax-deferred invoice that auto-finalizes via tax retry → `invoice.finalized` lands in the webhook event stream.
- [x] Issue a credit note on a paid invoice → the invoice Activity shows a dated "Credit note issued / Refund issued — $X" row (CN number + reason as subtext). *(walked 2026-07-26: NIM-000250 → "Credit note issued · CN-000032 — other · $605.00".)*
- [x] Mark an invoice uncollectible (operator menu) → Activity shows "Marked uncollectible — written off as bad debt" with the timestamp; the page STOPS polling (network tab quiet). *(walked 2026-07-26 via the dunning write-off on NIM-000244 — row exact; polling-stop not re-checked this walk.)*
- [x] Record an offline payment → Activity row reads "Payment recorded (offline)", not a card payment. *(walked 2026-07-26: NIM-000244 — row + "Recorded by an operator — cheque, wire, or other out-of-band payment" subtext.)*
- [ ] Plan-change with immediate proration → the subscription Activity row reads "Plan changed · <old> → <new> · Immediate · Proration invoice $X". A scheduled change shows a second row "Scheduled plan change applied" at the boundary; a threshold crossing shows "Spending threshold crossed · Invoice <num> issued early — $X".
- [ ] Scheduled-cancel sub: period bar reads Period Start → Period End → **Cancels** (no "Next Billing"); paused-collection sub shows **Collection resumes <date>**.
- [x] **Activity vs real-time lanes don't interleave clocks** — the second lane exists ONLY to keep two time domains apart, so it renders ONLY on simulated (clock-pinned) invoices: **Activity** carries sim-dated lifecycle + dunning rows, and **Real-time activity** holds the wall-clock rows (customer emails, standalone Stripe outcomes with no dunning twin to fold into, wall-stamped charge attempts). They are never mixed — a real-time row must never sort before the simulated event that triggered it. On a **wall-clock invoice there is one clock and therefore ONE card**: emails sort into Activity with everything else. *(walked 2026-07-28 on the fresh tenant after the 2026-07-28 lane review: live VLX-000006 renders a single Activity card — created → finalized → recovery started → "Customer notified — set up payment method" → Invoice paid $42.00 via Visa ••4242; simulated VLX-000012 still splits, Activity at the clock's Aug 17 2026 stamps and Real-time activity at the Jul 28 wall stamps. Pre-change the live page split emails into a second card whose caption promised "payment events" that structurally never appear there.)*
- [ ] **Void of a clock-pinned invoice with a pending PI shows no duplicate "Payment canceled" row** — finalize a clock-pinned invoice (pending PI), then Void it. The timeline shows the void but NOT a separate "Payment canceled" Stripe row (the PI-cancel webhook is folded into the void).
- [ ] **Credit-note timeline lane follows its own time domain ** — on a clock-pinned in_advance sub, advance the clock and **downgrade** the plan so the engine issues a clawback credit note against the simulated prebill: its "Credit note issued" row lands in the **Activity** lane (simulated dates), NOT "Real-time activity," and `SELECT is_simulated FROM credit_notes` is **true** for that row. Then on the **same** invoice issue a credit note via the operator action (Issue Credit): that row lands in the **Real-time activity** lane with a wall-clock timestamp, and its `is_simulated` is **false**. (On a non-simulated invoice there is one lane and both credit notes show wall-clock dates — unaffected.)

- [x] **A dunning resolution never renders beside its own cause row (ADR-020 fold, fixed 2026-07-26):** resolve a run as write-off → the timeline shows "Marked uncollectible — written off as bad debt" with NO bare "Dunning resolved" twin at the same instant; a void-flavored resolution folds into "Invoice voided" the same way (payment_recovered already folded into "Invoice paid"). The fold is gated on the invoice's own transition field, so a run resolved whose invoice never transitioned (failed propagation) KEEPS the dunning row as the only surviving record. *(found live on NIM-000244/NIM-000233; automated + mutation-verified: `TestPaymentTimeline_ResolvedRowFoldsIntoCauseRow`.)*

## Dunning

## FLOW D1: Retry cycle + escalation

- [x] Decline card → run billing → dunning run created. Page shows stat cards (Active, Escalated, Recovered, At Risk $) + tab filters with counts. *(walked 2026-07-28 on the fresh Walkthrough Co tenant: 0341 card, cycle close declined VLX-000001 → run created with write-time cause payment_failed; page read Active 1 / Escalated 0 / Recovered 0 / At risk $4.00 — every number explainable — and later Escalated 2 / Recovered 1 after the escalation + card-less recovery legs. Tabs All/Active/Escalated/Recovered present.)*
- [x] Sidebar Dunning badge shows count. *(walked 2026-07-28: badge read "Dunning 1" for the ONE wall-clock run (VLX-000004) while three simulated runs existed — the badge/analytics counts deliberately exclude test-clock rows (ADR-086 sim gating), so a clock-only tenant shows no badge.)*
- [x] Run state Active, "No retries yet", `next_action_at` scheduled. *(walked 2026-07-28: VLX-000001's run row — active, "No retries yet", Next Retry "in 2d" (= failure + 3d grace, sim-relative), Started "12h ago" sim-relative.)*
- [x] Backdate `next_action_at` → next tick increments attempt count. *(walked 2026-07-28 via clock advance past the due instant: attempt 0→1, later 1→2→3 in ONE advance. This walk found + fixed the charge-lease starvation: a definitive decline held the per-invoice lease its full 5-minute window, so back-to-back due retries in one catchup were skipped as transient and the loop exited "no progress" — the chokepoint now releases the lease on definitive outcomes; ambiguous outcomes still hold it.)*
- [x] After max retries → state Escalated. *(walked 2026-07-28: after attempt 3 the run escalated `retries_exhausted` (final_action pause); dunning page shows the escalated pill + "3 retries".)*
- [x] **Card-less invoice enters dunning and reaches a terminal (ADR-060)**: a finalized `auto_charge_pending` invoice for a customer with **no saved card** gets a dunning run created on the next scheduler tick (no charge attempted) and escalates through grace + retries to the policy `final_action` (e.g. `pause`) — instead of being retried forever with no terminal. Adding a card mid-campaign → the auto-charge sweep collects it and the run resolves `payment_recovered`. With dunning disabled for the tenant, the invoice stays un-dunned (deliberate — same as the declined-card path). *(walked 2026-07-28, two card-less customers on one clock: VLX-000002 enrolled `no_payment_method` at the close tick, rode 3 reminder ticks → escalated `retries_exhausted` (pause); VLX-000003 enrolled the same tick, 4242 attached mid-campaign → the sweep collected it on the next advance → payment succeeded, run resolved `payment_recovered` at attempt 0. Dunning-disabled clause walked earlier on Nimbus NIM-000269/270.)*

## FLOW D2: Resolution

- [x] "Payment recovered" → invoice marked **paid** (`paid_at` stamped; clock-pinned invoices land in sim-time), run closed — via the SAME offline-payment writer as the invoice page's Record-offline-payment dialog: `stripe_payment_intent_id` gets the `out_of_band:` prefix, the in-flight-charge guard applies, and `invoice.payment_recorded` + `invoice.paid` webhooks both fire. *(walked 2026-07-26 on NIM-000241 (3-retry escalated run): run resolved + invoice paid/succeeded, timeline folds to one "Invoice paid" row with `attempt_count: 3` ("after 3 retry attempts"); pre-fix a raw MarkPaid("") left no out_of_band marker and skipped the payment_recorded webhook. Automated: `TestResolveRun_PaymentRecovered_RoutesThroughOfflinePaymentWriter`.)*
- [x] **Unknown resolution values are 422s, not stored verbatim** — POST `/v1/dunning/runs/{id}/resolve` with any value outside `payment_recovered` / `manually_resolved` / `invoice_not_collectible` (typos, engine-only states like `retries_exhausted`, free text) → 422 naming the allowed values; the run is untouched. A CHECK constraint (m0158) backstops every writer. *(found live 2026-07-26: a typo'd `payment_received` was stored verbatim, silently skipped mark-paid, and rendered as a raw slug in the dashboard. Automated: `TestResolveRun_RejectsUnknownResolution`.)*
- [x] "Void invoice" (resolution `manually_resolved`) → invoice **voided** (`status='voided'`); any applied credits **reversed** to the customer's balance; collection **stopped at Stripe** — live Checkout sessions expired, and the PaymentIntent canceled when Stripe allows it. *(walked 2026-07-28 on VLX-000004: run resolved `manually_resolved`, invoice voided. THIS WALK FOUND A GAP: Stripe REFUSES `cancel` on PaymentIntents it created for Checkout ("Try expiring the Checkout Session instead"), so a hosted-page attempt's session stayed `open` at Stripe after the void — a voided invoice still payable until its ≤1h ExpiresAt. Fixed via a `StopCollection` seam (expire sessions first, then cancel the PI where cancelable) shared by the operator void and the dunning void-resolve. Re-walked on VLX-000005 side by side: pre-fix session `open`, post-fix session `expired` at Stripe. Credit-reversal clause covered by the atomic reversal inside `Void` — CI-locked, no credits on this fixture.)*
- [x] The void option reads **"Void invoice"** with destructive (red) styling and copy steering offline payments to "Payment recovered" — NOT a benign "Manually resolved / offline payment" label (which would trap an operator into voiding an invoice they actually collected). *(walked 2026-07-28: dialog options verbatim — "Payment recovered / Customer has paid…", "Void invoice / Annul the invoice and stop collection — e.g. billed in error or waived. Reverses any applied credits and cancels the pending charge… For a payment collected offline, use \"Payment recovered\" instead.", "Write off invoice / Marks the invoice as uncollectible (bad debt)…".)*

## FLOW D4: Self-service payment update

- [x] Trigger payment failure → email/log carries `http://localhost:5173/update-payment?token=vlx_pt_…`. *(walked 2026-07-28 on the fresh tenant: finalizing VLX-000006 for a no-PM customer sent "Set up payment for invoice VLX-000006"; the CTA href carried the vlx_pt_ token URL.)*
- [x] Open in incognito → page loads without login, shows customer + invoice + amount, "Secured by Stripe". *(walked 2026-07-28: no session, page rendered "Secure payment update" with customer, VLX-000006, Amount Due $42.00, and the Stripe assurance line. THIS WALK FOUND A LIE: the page never read invoice status, so a VOIDED invoice (VLX-000005) still rendered "Payment method needed — Add a payment method to pay this invoice" with "Amount Due $25.00". Fixed: the validate endpoint returns invoice_status and the page states the truth for terminal invoices — "This invoice was canceled / No payment is needed. You can still add a payment method so future invoices are collected automatically." — with the figure relabeled "Invoice amount". Owing invoices are unchanged.)*
- [x] Click Update → Stripe Checkout setup → new card → redirect → the `setup_intent.succeeded` webhook records the card. The **customer detail page now shows it** under "Payment method" (brand + last 4), and the previously-unpaid invoice auto-charges on the next sweep (or clock advance for a clock-pinned customer). The card persists because the Checkout session now stamps `velox_customer_id` on `setup_intent_data.metadata`, so the SetupIntent is self-resolving (the resolver-by-`customer` fallback remains as a backstop). *(walked 2026-07-28 end-to-end: 4242 through Checkout → setup_intent.succeeded recorded visa ••4242 → the auto-charge sweep collected VLX-000006 on its next tick → invoice paid, amount_paid 4200.)*
- [x] **A saved card is never dropped on webhook ordering**: if `setup_intent.succeeded` is processed before the customer↔Stripe-id link is written (e.g. it arrives before `checkout.session.completed`), the handler returns a transient 5xx (no dedup row written) and Stripe **redelivers** — the card lands on the redelivery once the link resolves. Pre-fix this returned 200, was dedup-marked, and the card was permanently dropped (PM never shown, invoice stuck `auto_charge_pending`). A setup-intent with no payment method is acked (nothing to attach). *(CI-locked rather than hand-walked: forcing the ordering needs an artificial mid-write state, and `TestHandleWebhook_SetupIntent_UnresolvedCustomerRedelivers` (internal/payment) pins all three properties exactly — transient error so Stripe redelivers, attacher NOT called, and NO dedup row written (a dedup row would short-circuit the redelivery and drop the card permanently). Verified green 2026-07-28.)*
- [x] First-time customer (no Stripe Customer yet — the usual no-saved-card case this email targets): clicking Update creates the Stripe Customer on demand and opens Checkout, NOT a "customer does not have a Stripe payment setup" error. *(walked 2026-07-28: D4 Self Serve had no Stripe customer before the click; Checkout opened and the customer row carries a stripe_customer_id afterwards.)*
- [x] A failed attempt does NOT spend the link: if the Update click errors before Checkout opens (e.g. Stripe momentarily unreachable), re-opening the same link still works — only a successful Checkout open consumes the token. *(walked 2026-07-28 on a purpose-built Stripe-less tenant — the case the code names: POST checkout returned 502 with the ADR-026 Tier-2 neutral copy ("We couldn't start the payment update right now…", no config detail leaked), and re-validating the SAME token still returned the invoice — the link was not spent. Ordering holds because EnsureStripeCustomer runs before the consume CAS.)*
- [x] Re-open same URL after a successful use → "Link expired or invalid". Random token → same. No token → "No payment update token provided". *(walked 2026-07-28: after the successful Checkout the same token returned 401 invalid_token "invalid or expired token"; a random vlx_pt_ value returned the identical shape — no oracle distinguishing consumed from never-existed.)*

---

## FLOW D5: Dunning policy admin (CRUD + assignment + terminal actions)

Policy configuration surface — distinct from the dunning state machine under catchup (FLOW TC5).

- [x] **Policy CRUD invariants** — `/dunning-policies` admin page: create a new policy with `max_retry_attempts=5` and `retry_schedule=[72h, 120h]` → save rejects with inline error naming the missing entries. Delete the default policy → rejected ("promote another policy first"). Delete a non-default policy with assigned customers → rejected ("N customer(s) still assigned; reassign first"). *(walked 2026-07-28 on the fresh tenant: the create dialog refused 5-retries-with-2-entries inline — "Need 4 entries for 5 retries." + "Missing 2 entries." — and saved once corrected, born `is_default=true` as the tenant's first policy. The DEFAULT row deliberately offers NO delete affordance in the UI (only Edit; the non-default row carries Make-default / Edit / Delete), and the API rejects it anyway — 422 "cannot delete the default dunning policy — promote another policy first". After assigning a customer to WT Enterprise (card shows "1 assigned"), deleting it → 422 "cannot delete policy — 1 customer(s) still assigned; reassign them first".)*
- [x] **Four terminal actions (ADR-036 amendment)** — dropdown options: `Pause collection (keep drafting invoices)`, `Cancel subscription`, `Mark invoice uncollectible`, `Leave open — manual review`. *(walked 2026-07-28 on the fresh tenant's New-policy dialog: all four present verbatim, in that order.)*

## Credits & Credit Notes

## FLOW C1: Credits lifecycle

- [ ] Grant $50 expires 30d → balance $50, ledger Expires column populated.
- [ ] **Credit-expiry date reads the customer's simulated now (ADR-086):** on a clock-pinned customer advanced to e.g. 2027, the Grant Credit dialog's expiry-date picker floor and its "must be a future date" check reject a real-today date (it's *past* in simulated time) — anchored on the clock's `frozen_time`, not wall-clock. Same on the invoice composer's prepaid-credit line.
- [ ] Run billing → applied, amount_due reduced, Stripe charged remainder. Ledger entry "Applied to invoice <number>".
- [ ] Grant $500 + $79 invoice → fully credited, amount_due $0, balance $421, Stripe NOT charged.
- [ ] Deduct $20 → confirmation → balance reduced, ledger entry.
- [ ] **Credits always precede card charges**: grant a customer a credit balance smaller than an unpaid flagged invoice → next scheduler tick (or clock Advance) charges only the remainder, never the full amount; balance fully covering the invoice → invoice flips to paid with NO Stripe charge and the flag clears. Same on a dunning retry: credits granted after the failure reduce (or fully settle) the retried charge.

## FLOW C2: Credit notes

- [ ] **In-flight payment blocks an amount-reducing credit note**: `psql -c "UPDATE invoices SET payment_status='processing' WHERE id='<a finalized, unpaid invoice>'"` → POST a credit note for it → **409** "cannot credit-note an invoice whose payment is in flight — settle or cancel the payment first". Reset to `pending` → the same credit note succeeds. (Operator gate; the automated clawback defers instead — see the Part-B flow below.)
- [ ] **In-flight payment blocks Void and Mark-uncollectible (ADR-059)**: with a finalized invoice forced to `payment_status='processing'` (psql, as above) → POST `/v1/invoices/{id}/void` → **409** "a charge is in flight … wait for it to settle or cancel it before voiding"; POST `/v1/invoices/{id}/mark-uncollectible` → **409** "… before marking uncollectible". Reset to `failed` (or `pending`) → both succeed. (`unknown` behaves like `processing`; `RecordOfflinePayment` is blocked the same way.)
- [ ] **A clawback on an in-flight source defers (ADR-059, Part B)** — on a clock-pinned in_advance sub whose paid prebill is forced to `payment_status='processing'` (psql), downgrade mid-cycle → the clawback credit note is created **`status='draft'`, `issue_pending=true`** and the prebill's `amount_due` is **unchanged**; the reconciler scan skips it while the source is in flight. *(scan-deferral leg covered by `TestListPendingClawbackDrafts_DefersInFlightSource`; the downgrade-drive + amount_due-unchanged legs are manual)*
- [ ] **…then settles down the correct branch once the source resolves** — set the source `payment_status='succeeded'` + one scheduler tick (or trigger the clawback reconciler): the draft flips to **`issued`** and the unused share lands as a **customer balance credit** (paid branch). Source = `failed` instead → it issues down the **reduce** branch (`amount_due` drops to the consumed portion). Source **voided** before settle → the draft is **voided** (orphan guard), no second tax reversal.
- [ ] Unpaid invoice → Issue credit "Billing error" $20 → no allocation inputs shown → Issue → amount_due reduced.
- [ ] Paid invoice ($100, fully card-paid) → enter $40 → defaults to Credit balance $40, Refund 0, Out-of-band 0 → Issue → customer balance +$40.
- [ ] Same invoice → enter $30 + type $30 in Refund to card → Credit auto-fills to $0; Allocated $30 / $30 ✓ → Issue → Stripe refund processed; CN row label "refund"; refund_status=succeeded.
- [ ] Mixed-paid invoice ($82.60 = $62.60 card + $20 credits) → enter $80 → type $62.60 in Refund to card → Credit auto-fills to $17.40 → Issue → Stripe refund $62.60 + credit grant $17.40; CN row label "refund + credit"; balance increases by $17.40.
- [ ] Same mixed invoice → enter $80, type $70 in Refund → inline error "Refund cannot exceed $62.60 paid via card"; Save disabled.
- [ ] Three-channel split: $100 CN → $40 Refund + $30 Credit + $30 Out-of-band → Allocated $100 / $100 ✓ → Issue → all three persisted; CN row label "refund + credit + out of band".
- [ ] Sum mismatch: $50 CN with Refund $20 + Credit $20 + OOB $0 = $40 ≠ $50 → "Allocated $40 / $50 ✗" red; Save disabled.
- [ ] CN > amount_due (unpaid) or > total_amount (paid) → error.
- [ ] CN page: stat cards (Total Credited, Refunded, Applied to Balance, Issued); list rows show channel breakdown ("refund" / "credit" / "refund + credit" / etc.); CSV export has separate Refund/Credit/Out-of-band columns.
- [x] **Issuing a credit note is atomic — a failed internal effect leaves no orphan (ADR-061):** on a **paid** invoice, create a credit-type credit note (credit-to-balance), then `REVOKE INSERT ON customer_credit_ledger FROM velox_app` and Issue it → the action **fails** and `SELECT status FROM credit_notes WHERE id='<cn>'` is still **`draft`** (the `draft→issued` flip rolled back with the failed grant — no "issued" credit the customer never received); `GRANT INSERT ON customer_credit_ledger TO velox_app` and re-issue → succeeds, status `issued`, balance credited once. *(automated: `TestIssue_GrantFailure_RollsBackCAS`)*

## FLOW C4: Prepaid commits (ADR-078)

- [x] Compose a manual invoice with a commit line via API (`line_items: [{description, line_type:"add_on", quantity:1, unit_amount_cents:9000, commit_granted_cents:10000}]`) → draft created; customer balance unchanged (grant-on-issue, not on create). *(automated: `TestCommitFinalize_FundsGrantOnce`)*
- [ ] Finalize it → customer balance **+$100** (the GRANTED amount, not the $90 price); Credits ledger shows "Prepaid commit — invoice <number>"; finalize again → 409, balance unchanged.
- [ ] Second commit line on the same invoice → 422 "one commit per invoice"; commit line on a subscription-cycle invoice → 422 "only supported on manual invoices".
- [x] **Commit invoices are cash-only**: grant the customer separate credits, leave the commit invoice unpaid with no card on file → scheduler tick does NOT apply balance to it (amount_due unchanged); a normal invoice still gets credits applied. *(automated: `TestApplyToInvoice_CommitInvoiceIsCashInstrument`)*
- [x] Credit note against an UNPAID commit invoice → 409 "…void the unpaid invoice to cancel the commit instead"; against a PAID one → a different 409 pointing at commit relief ("use commit relief (POST /v1/credit-notes with a commit_relief block)…"). *(automated: `TestCreditNote_BlockedOnCommitInvoice`)*
- [x] Draw part of the commit (run billing on a usage invoice), then **void** the funding invoice → balance drops to $0 (remaining retired; consumed stays consumed); ledger shows a negative adjustment "Commit retired — funding invoice voided", and a `credit.commit_retired` event lands on the Webhooks page. *(automated: `TestCommitVoid_RetiresRemaining`)*
- [x] Mark the funding invoice **uncollectible** instead → balance UNCHANGED (block stays live — collections stance); voiding it afterwards retires once. *(automated: `TestCommitUncollectible_NoRetire_ThenVoidRetiresOnce`)*
- [x] **Balance alerts**: set the credit low-balance threshold to $50 (settings API); grant $100 (webhook `credit.balance_recovered`), drain to $30 (`credit.balance_low` with `balance_cents`/`threshold_cents`), drain to $0 (`credit.balance_depleted`) — events visible on the Webhooks page. *(automated: `TestBalanceCrossingEvents`)*
- [x] Grant with `grant_kind:"promotional"` + a plain grant → billing drains the promotional block first. *(automated: `TestDrainOrder_PromotionalFirst_NullSafe`)*
- [ ] Grant with `grant_kind:"commit"` via POST /v1/credits/grant → 422 (reserved for the funding path).

## FLOW C2b: Credits ledger readability

- [ ] Customer ledger shows 5 columns (Date · Type · Description · Amount · Balance) with Amount/Balance fully visible at a 1280px window — nothing clipped at the right edge.
- [ ] An "Applied to invoice DEMO-NNNN" row's description IS the invoice link; a grant with expiry shows "Expires <date>" as subtext; an expiry row reads "Grant expired" (machine id only as small mono subtext).
- [ ] A customer with >50 ledger entries: page footer says "of <true total>" (not 50), and page 3 shows rows 51+ — `GET /v1/credits/ledger/{cus}?limit=25&offset=50` returns those same rows with `total` = the full count.
- [ ] Type filter "Applied" on that customer matches entries **beyond the first 50** (filter is a server query param, not a client slice).
- [ ] CSV export row count = the true total + 1 header (the export pages the API to completion; it is NOT the visible page).

## FLOW C3: Credit-note refund handling (ADR-063)

The credit-note refund leg: operator retry, async webhook reconciliation, proactive surfacing.

- [ ] **Card-refund recovery — a failed refund retries to a real Stripe refund, exactly once:** on a **card-paid** invoice (real PaymentIntent), issue a refund credit note with the refund leg failing (e.g. the PI is unreachable at Issue time) → the CN **issues** with `refund_status=failed`, and the Credit Notes page shows a red **"refund failed"** badge + a **Retry refund** button. Click **Retry refund** (or `POST /v1/credit-notes/{id}/retry-refund`) → a **real Stripe refund** is created (`re_…`) and `refund_status=succeeded`. Verify **exactly one** refund at Stripe (idempotency key `velox_cn_<cn_id>`). A **second** retry on the now-succeeded CN → **409** "refund already succeeded — nothing to retry" (no double-refund). The refund leg is **operator-retried, not auto-swept** (money-out is conservative — unlike the tax-reversal sweep).
- [ ] **Refund status reconciles from the async webhook (ADR-063):** issue a refund whose Stripe `Refund.create` returns **`pending`** → the CN records `refund_status=pending` (NOT a blanket `succeeded` — the create-time fix). Deliver `refund.updated` / `refund.failed` (or `stripe trigger charge.refund.updated`) → the CN flips to the webhook's final status, matched by `stripe_refund_id`. A **stale** out-of-order `pending` webhook arriving *after* a terminal (`succeeded`/`failed`) does **not** clobber it (monotonic). A refund webhook with **no matching Velox credit note** (created in the Stripe dashboard) is **ack'd and ignored** — no credit note is fabricated.
- [ ] **Failed / stuck-pending refunds surface proactively:** a credit note at `refund_status=failed` (terminal) OR `pending` **older than 72h** raises the Dashboard danger alert **"N refunds need attention"** → links to Credit Notes filtered (`?refund_status=needs_attention`); chip reads "Showing refunds needing attention (failed, or pending > 72h)". A **fresh** `pending` refund (normal async settlement) is **NOT** alerted — it shows a neutral "refund pending" badge only (no alert fatigue). The count (`overview.refunds_needing_attention`) excludes test-clock-simulated CNs (`is_simulated=false`). Resolving the refund (Retry refund → succeeded, or the webhook flips it terminal) drops the count and clears the alert.

## Webhooks

## FLOW W0: Outbound webhook endpoint config (2026-07-05)

- [ ] **`payment_method.*` events are subscribable:** the endpoint dialog's event picker shows a **Payment method** group (attached / updated); creating an endpoint subscribed to `payment_method.attached`, then completing a card setup, delivers the event (pre-fix the subscription itself was rejected as "Velox never emits it").

- [ ] Create endpoint with `events: ["invoice.payment_failed"]` → **422** naming the unknown type (the engine never emits it — pre-fix this subscription received silence forever). `["payment.failed"]`, `["invoice.*"]`, and `["*"]` all pass; `["nonexistent.*"]` → 422.
- [ ] `PATCH /v1/webhook-endpoints/endpoints/{id}` with a new URL + `active: true` → endpoint updated, **signing secret unchanged** (receiver keeps verifying). Dashboard → Webhooks → **Edit** shows URL/description/active + the same event picker as create, prefilled.
- [ ] Instantiate a recipe → its webhook endpoint (inactive, placeholder URL) subscribes to REAL event names (`payment.failed`, `subscription.item.updated`); Edit it to a real URL + active → deliveries flow.

## FLOW W1: Stripe signature verification

- [ ] Valid payload + signature ≤300s → 200, processed.
- [ ] Replay 5+ min later → rejected (timestamp tolerance).
- [ ] Modified payload + original signature → rejected.

## FLOW W2: Outbound secret rotation (72h grace)

- [ ] Rotate Secret on endpoint → modal shows new `whsec_…` + green "Previous secret valid until {ts}" card.
- [ ] API response includes `secret` + `secondary_valid_until`.
- [ ] Endpoints table row shows "Dual-signing until {ts}".
- [ ] Trigger event during grace → header carries TWO `v1=` entries; both old and new verify.
- [ ] Backdate `secondary_secret_expires_at` → header carries one entry; only new verifies.

## FLOW W3: Delivery stats

- [ ] Endpoints page Success Rate column: green ≥95%, amber 70–94%, red <70%.
- [ ] Replay failed event → success rate updates.

## FLOW W4: Live event stream + replay

- [ ] **Payment anomalies emit PER-CAUSE events (ADR-068)** — a duplicate capture, an amount mismatch, and a payment on a voided invoice each emit their own event type (`payment.duplicate_charge` / `payment.amount_mismatch` / `payment.received_on_voided_invoice`) on the stream — never one aggregate "attention changed" event.

- [ ] Webhooks → Events → recent deliveries with state dot.
- [ ] Trigger event → row streams in <1s without refresh (SSE).
- [ ] Click delivery → side panel: URL, status, headers, body.
- [ ] Replay failed → fresh attempt fires; original preserved.
- [ ] **Test-mode replay stays test-mode:** in **test** mode, replay a webhook event → the new delivery row appears in the test-mode Events view and `SELECT livemode FROM webhook_deliveries WHERE id='<new>'` is `false`.
- [ ] Multi-retry event → "Diff" tab shows payload diff between attempts.
- [ ] Stop Redis or dispatcher → readiness degraded; UI loads but stops streaming.
- [ ] **Failed delivery walks the full retry ladder:** point an endpoint at a URL that always 5xxs, trigger an event → the delivery row stays `pending` across **five** scheduled retries with backoffs 1m → 5m → 30m → 2h → **24h** (`next_retry_at` steps match), and only flips to `failed` after the attempt past the 24h slot (`attempt_count=6`).

---
- [ ] **A redelivered `payment_intent.payment_failed` notifies once, not twice ** — decline a charge (`4000 0000 0000 9995`) on a non-interactive auto-charge so the customer gets a payment-failed email, then **resend the same event** (`stripe events resend <evt_id>`, or re-POST the identical webhook). Mailpit shows exactly **one** "payment failed" email and the webhook event stream shows exactly **one** `payment.failed` (the redelivery is a no-op — `slog` logs "duplicate or out-of-order payment failure for this payment intent"). A genuinely **new** retry that fails (a fresh PaymentIntent) DOES notify again.
- [ ] **A redelivered `payment_intent.succeeded` settles once, not twice** — auto-charge an invoice to `paid`, then **resend the same event** (`stripe events resend <evt_id>`, or re-POST the identical webhook). The invoice stays `paid`, Mailpit shows exactly **one** receipt, and the webhook stream shows exactly **one** `invoice.paid` — the redelivery is a no-op (terminal-state guard logs "payment already settled; skipping duplicate success settlement"; the webhook store also dedups on `(tenant, livemode, stripe_event_id)`). A genuinely **new** PaymentIntent that succeeds (e.g. dunning recovery on the same invoice) DOES settle + receipt again. (Success twin of the `payment_intent.payment_failed` row above; the guard differs because `paid` is terminal while failure is not.)

## Customers

## FLOW CU1: Settings + billing profile

- [ ] Settings: company name change → "Saved" indicator. Navigating with unsaved changes prompts.
- [ ] **Settings changes are audited field-by-field**: change e.g. the default currency + Net terms, save, open `/audit-log` → one "Updated Settings" row whose expanded metadata shows `changed` with each field's `from`/`to` (e.g. `default_currency: USD → EUR`). A no-op save (no fields changed) adds no field-level row. Actor = the signed-in operator, not "System". (The `audit_fail_closed` example this flow used to reference is retired — ADR-089 removed the setting, migration 0149 dropped the column; fail-closed is now structural, since every audit row rides its mutation's transaction.)
- [ ] Currency change → new invoices use it; existing unchanged.
- [ ] Edit billing profile (address, tax ID) → PDF reflects update.
- [ ] Edit billing profile when customer has `stripe_customer_id` set → Stripe Dashboard → Customer shows the updated legal_name / phone / address / tax_exempt immediately (Phase 1 Velox→Stripe sync, best-effort, fires on every customer/profile update). <!-- currency-ok: Stripe Customer object's own tax_exempt field -->
- [ ] Create a brand-new customer with email + display_name + billing profile → first PM action (operator send-setup-email / copy setup-link) lazily creates the Stripe Customer pre-populated with email, name, address, and tax_exempt status — Stripe Dashboard shows a fully-populated row, NOT a blank one with only `velox_*` metadata. <!-- currency-ok: Stripe's own tax_exempt field -->
- [ ] Set billing profile tax_id (e.g. `eu_vat` + `DE123456789`) → Stripe Dashboard → Customer → Tax IDs tab shows the entry (Phase 2 reconcile). Change tax_id value → old entry gone, new entry present. Clear tax_id → Tax IDs tab empty. Brand-new customer with tax_id pre-filled in profile → first PM action creates the Stripe Customer with the tax_id already in the Tax IDs tab (no follow-up update call needed).
- [ ] Draft invoice held >24h, then click Finalize → operator sees `tax calculation expired (age 24h0m, max 23h0m) — retry tax to refresh, then finalize` (Phase 2 expiry guard). Click Retry tax → tax recomputes → Finalize succeeds, Stripe Tax dashboard shows a `tx_*` transaction. Without the guard, finalize previously left the invoice with `tax_calculation_id` populated but no `tax_transaction_id`.
- [ ] **Tax-retry flush on profile update.** Customer with a draft invoice stuck on `tax_error_code = customer_data_invalid` (e.g. US customer missing `postal_code`). Edit billing profile → fill the missing field → Save. Without per-invoice clicking: invoice's `tax_status` flips to `succeeded` (or back to `failed` with a different code if still wrong), and `slog | grep "billing profile flush retried tax errors"` shows `processed >= 1`. Other stuck-tax codes (e.g. `provider_outage`) are NOT replayed by the flush — only `customer_data_invalid`.

## FLOW CU2: Operator customer-portal API

- [ ] `GET /v1/customer-portal/{customer_id}/overview` → active subs, recent invoices, credit balance.
- [ ] `/subscriptions`, `/invoices` scoped to that customer.

## FLOW CU4: Archive cascade

- [ ] Archive → confirm → amber banner "data is read-only". Action buttons hidden.
- [ ] Run billing → no invoices for archived customer's subs. Existing invoices and credits still readable.
- [ ] Restore → banner gone, actions reappear.
- [ ] Customers list → Archived tab.

## FLOW CU6: Brand color + logo URL

- [ ] Settings → Business → Logo URL accepts public HTTPS URL. Live thumbnail renders. Invalid → "Couldn't load image".
- [ ] Brand color: native color picker + hex input + Clear. Invalid hex (`#zzz`, missing `#`, uppercase) → 422 client+server.
- [ ] Save → invoice PDF: company name tinted, 2px accent bar.
- [ ] Clear color → next PDF byte-identical to pre-migration neutral.

## FLOW CU9: Sent emails on customer page

Mirrors Stripe's customer-page "Sent emails" section (docs.stripe.com/invoicing/send-email). Operator audit / support surface for "did we tell the customer?"

- [ ] Customer Detail → "Sent emails" card lists every customer-facing email sent for this customer in the last 30 days, newest first. Empty state: "No emails sent in the last 30 days."
- [ ] After running a dunning catchup that exhausts an N-retry policy: the card shows N+1 rows for the run's invoice — 1 "Payment failed" + (N-1) "Payment retry — action required" + 1 "Retries exhausted". Each row's recipient matches `customers.email` (the single canonical recipient since migration 0100); each carries a wall-clock send time (real-time SMTP dispatch instant, NOT the simulated cycle date).
- [ ] Failed delivery rows show the SMTP error inline (red text under the type label).
- [ ] Invoice-number link on each row navigates to the invoice list filtered to that number.
- [ ] **Invoice activity timeline** no longer shows standalone "Dunning reminder emailed" / "Dunning escalation emailed" rows for those emails — the customer-page section is the canonical view (avoids wall-clock-vs-simulated time-domain mismatch in the invoice activity timeline). The initial Stripe-webhook payment-failed email still folds onto the `dunning_started` row as a "Customer notified by email" sub-line (same time domain — both wall-clock).
- [ ] `GET /v1/customers/{id}/sent-emails` returns `{sent_emails: [...]}` with fields `id, email_type, recipient, status, invoice_number?, last_error?, created_at, dispatched_at?` — 30-day window, ORDER BY created_at DESC.

## FLOW CU8: Cost-dashboard public projection

- [ ] Customer detail → "Public cost-dashboard URL" card → click **Generate URL**. Operator sees `vlx_pcd_<64 hex>` token and full URL. Copy button works.
- [ ] `POST /v1/customers/{id}/rotate-cost-dashboard-token` → `{token, public_url}`. Token prefix `vlx_pcd_` + 64 hex; URL is `{VELOX_API_BASE_URL or relative}/v1/public/cost-dashboard/<token>`.
- [ ] `GET /v1/public/cost-dashboard/{token}` (no auth) → sanitized projection:
  - Present: `customer_id`, `tenant_id`, `billing_period {start, end, source}`, `subscriptions[]` (id + plan_name + currency + period), `usage[]` (meter_key + meter_name + unit + currency + total_quantity + total_amount_cents + rules[]), `totals[]`, `projected_total_cents`.
  - **Absent**: email, display_name, external_id, metadata, billing_profile, plan_id, rating_rule_version_id, warnings.
- [ ] No active sub → `billing_period.source='no_subscription'`, empty arrays, **NOT 5xx** (200 with the empty envelope).
- [ ] Rotate → previous URL returns 401 `invalid cost-dashboard token` immediately. Audit log records the rotation (`action=rotate`, `resource_type=customer`, `metadata.surface=cost_dashboard_token`); the plaintext token is NEVER in the audit row.
- [ ] Tampered / unknown token → 401 (same 401 as rotated — anti-enumeration). Wrong prefix (no `vlx_pcd_`) → 401 fast-path without DB lookup.
- [ ] Rate limit: 61+ req/min/IP → 429 (shares the hosted-invoice bucket; tighter than the general 100/min).
- [ ] Embeddable widget (not yet shipped): a `<VeloxCostDashboard token baseUrl />` React widget is the natural next step — defer until first design partner asks. The JSON projection is consumer-ready as-is; partners can render their own UI today.

---

## Platform

## FLOW P2: Audit log

- [ ] Several actions (create customer, grant credits, void invoice, change plan) → all logged.
- [ ] **Credit grant/adjust audit rows share the ledger's fate (ADR-090, first in-tx domain)**: grant credits → the `grant` audit row and the ledger entry appear together; attempt an over-balance deduction (400) → NO `credit.deduction` audit row exists for the failed attempt (phantom class dead). Same row content as before the migration: action `grant` / `credit.adjustment` / `credit.deduction`, resource `credit`, metadata `customer_id`/`amount_cents`/`description` — wire strings frozen.
- [ ] **Background + public flows now leave evidence (ADR-090 PR4)**: (a) let dunning exhaust into cancel_subscription → the sub's `cancel` audit row appears with `canceled_by: dunning`, actor System — identical shape to an operator cancel; (b) engine-relief void (or operator void) → one `void` invoice row carrying `status_before` and, when credits were consumed, `credits_reversed_cents`; (c) a reconciler/engine-issued credit note → one `credit_note.issued` row (no doubles vs the operator path); (d) a Stripe refund flip to `failed` → an `update` credit_note row with metadata `action: refund_status_changed`, `refund_status: failed` — a stale redelivery adds NO row; (e) `POST /v1/tenants` → the NEW tenant's own log opens with a `create` tenant row attributed to the platform key (recorded in the LIVE plane — switch the dashboard to live mode to see it); (f) hosted-invoice Pay click → an `update` invoice row `action: hosted_checkout_started` attributed to the **customer**; payment-update link click → `payment_update_checkout_started` on the customer, and if the Stripe create fails a paired `payment_update_checkout_restored` row keeps the log truthful.
- [ ] **Every mutating route records the truth, not a guess (ADR-090)**: (a) delete a meter's **pricing rule** → the audit row reads *deleted meter_pricing_rule* with the rule's id and its true `meter_id` (pre-fix this was recorded as "deleted meter {meter_id}" — a fabricated row); deleting a nonexistent rule (404) adds NO row; (b) create a customer → one `create customer` row with `external_id` + `email_set` (never the raw email); (c) create a manual invoice (with and without line items) → one `create invoice` row each; add a line item → an `update invoice` row with `action: line_item_added`; (d) create / void / retry-refund a credit note → one row each (`create`, `void`, `update`+`action: refund_retried`); a re-drive that gets the same refund status back from Stripe **still adds a row**, carrying `status_changed: false` — the operator's retry hit Stripe, so it IS the fact, and suppressing it would erase a real money-path action. (Contrast the WEBHOOK redelivery in the box above, which mutates nothing and correctly adds no row.) (e) upsert / delete a provider-cost rate → one row each; (f) instantiate a recipe → one `create recipe` row listing what it installed (plan/meter/rule ids); **re-apply the same recipe (idempotent no-op) → NO second row**; (g) trigger a billing run → one `run billing` row with `invoices_created`, distinct from the per-invoice `finalize` rows.
- [ ] **Voiding a credit note is race-safe**: a `void` that loses to a concurrent `issue` returns a conflict naming the note's actual state — it does NOT report success (pre-fix it blind-wrote `voided` over an issued note whose credit had already been granted).
- [ ] **A new mutating route cannot ship un-audited**: add any `r.Post/Put/Patch/Delete` to the router and run `go test ./internal/api` → FAILS, naming the route, until it is declared in `internal/api/audit_routes.go` as `explicit` (it emits its own row) or `exempt(reason)` (a closed enum: `non_mutating_preview`, `machine_ingest`, `system_endpoint`, `webhook_owned`). Deleting a route while leaving its registry entry behind fails the same test (stale entry). This two-way diff walks the LIVE chi route table — it cannot drift from what is actually mounted.
- [ ] **A route that silently stops emitting is reported, not papered over**: an operator action that commits and writes no audit row increments `velox_audit_uncovered_mutation_total{route=…}` and logs `UNCOVERED MUTATION` with the route pattern — where the deleted catch-all would have invented a row guessed from the URL. (CI-locked: `internal/api`'s `TestMain` fails the package if any e2e-exercised route reports one.)
- [ ] **Forgotten audit wiring fails at boot**: removing any `SetAuditLogger` line for an in-tx-audited service from router.go panics at startup via `audit.MustWired` — exercised by every CI job that constructs the router (golden-path). The source-pin test (`TestMustWired_CoversEveryAuditedComponent`) covers the complementary drift: a NEW SetAuditLogger wiring line that forgets to join the MustWired list fails in CI.
- [ ] **Saved-card checkout completion leaves customer-attributed evidence**: complete a hosted-invoice payment ticking "save card" (or a payment-update link setup) → an `update` customer row with metadata `action: payment_setup_completed`, card brand/last4, attributed to the **customer**; a `checkout.session.completed` replay for a torn-down or cross-mode customer writes NO row (zero-row UPDATE ⇒ no fabricated evidence).
- [ ] **Dashboard actions are attributed to the operator, not "System"**: any action taken from the signed-in dashboard records `actor_type=user` with the operator's user id (Actor column shows the user, not "System"). Only genuine background work (engine auto-cancel, scheduler) records `actor_type=system`. API-key (Bearer) callers still record `actor_type=api_key` with the key name.
- [ ] **`request_id` is server-minted, never client-chosen**: make any mutating call with a forged header — `curl -X POST … -H 'X-Request-Id: forged-correlation-id'`. The resulting audit row's `request_id` (visible in the exported CSV's **Request ID** column, and on `GET /v1/audit-log`) is a server id prefixed `req_`, NOT `forged-correlation-id` — and the `Velox-Request-Id` response header carries that same server value. (The expanded row in the dashboard does not surface request_id today; the CSV and the API are where you read it.) chi's stock `middleware.RequestID` honoured the inbound header verbatim, which let a caller choose the correlation id recorded on their own audit rows — correlation evidence an adversary can write is not evidence. *(automated: `TestRequestID_IgnoresClientSuppliedHeader`)*
- [ ] **Append-only is enforced by TWO independent layers (migration 0150)**: as the **runtime role** (`velox_app`), `UPDATE` / `DELETE` / `TRUNCATE` on `audit_log` fail with `permission denied for table audit_log` (SQLSTATE **42501**) — the write grants are revoked, so the statement never reaches a trigger. As the **table owner / superuser** (which still holds those privileges), the same three statements fail with `audit_log is append-only…` (SQLSTATE **P0001**) — the triggers hold independently, exactly as before. Rows remain intact either way; before 0150 the triggers were the ONLY barrier. *(automated: `TestAuditLog_AppendOnlyIsTwoIndependentLayers`)*
- [ ] **Bulk data egress is audited (ADR-090 §7)**: exporting customers / invoices / subscriptions / usage events / the audit log each writes one `export` row (amber, medium severity) — the only READ Velox records. See FLOW EX (EX5–EX10) for the full contract, incl. fail-closed. **Known, deliberate loss**: ordinary paginated list reads are NOT audited — an operator who walks `GET /v1/customers?limit=100&offset=…` copies the same data and `audit_log` will not show it. What it answers is "did anyone take the one-click bulk export". Closure trigger: a DP/auditor asking for PII-read evidence → an access-log-derived read trail, not a row per GET.
- [ ] **Append-only is TRUNCATE-proof — at whichever layer you hit first**: as the **runtime role** (`velox_app`, even with `app.bypass_rls='on'`), `TRUNCATE audit_log` fails with `permission denied for table audit_log` (SQLSTATE **42501**) — migration 0150 revoked the grant, so the statement never reaches a trigger. As the **table owner / superuser**, which still holds the privilege, the same statement fails with `audit_log is append-only; TRUNCATE is not permitted` (SQLSTATE **P0001**) — the trigger holds independently. Rows remain intact either way. (Before 0150 the app role reached the trigger and saw the P0001 message; expecting that message from `velox_app` today is expecting a barrier that has been superseded by a stricter one.)
- [ ] **No customer PII in audit metadata**: change a customer's email, upsert a billing profile with a tax ID, send an invoice, send a setup link → the audit rows' expanded Details show `email_changed`/`tax_id_set` flags and ids — never the raw email address or tax ID (append-only log + GDPR erasure). The email outbox keeps the actual delivery addresses.
- [ ] **Operator rows show the operator's name**: dashboard-driven rows render the user's display name or email in the Actor column (never the raw `user` enum or a bare `vlx_usr_…` id).
- [ ] **Humanized rows**: the action badge reads "Subscription Item Updated", not `subscription.item_updated`; metadata labels case acronyms correctly ("IP Address", not "Ip Address"); structured metadata values (e.g. the settings `changed` map) render as JSON, not `[object Object]`; item add/remove/update rows on a subscription carry the sub's code as the label; expanded Resource ID has a copy button.
- [ ] **Membership + auth rows are humanized, and a removal reads as destructive**: invite a teammate, have them accept, then remove them; sign out and back in; toggle test/live. `/audit-log` renders "Invited sam@…", "Joined the team (sam@…)", "Revoked a team invitation", "Removed a team member", "Signed in", "Signed out", "Switched to live mode" — never a raw dotted string like `member.joined`. The **member.removed** row carries the destructive red left border (it revokes the target's sessions), like `revoke` / `delete`. *(automated: `cd web-v2 && npm test` → `tests/auditVocabulary.test.ts`)*
- [ ] **Test-clock rows deep-link**: expand a `test_clock` audit row (created / advanced / deleted) → its **View** link opens `/test-clocks/<id>`. Previously these rows offered no link at all, although the detail page has always existed.
- [ ] **Cursor pagination**: the list pages with Previous/Next (no numbered jumps); footer reads "Page N · M total". Paging deep does not slow down (seek pagination — only page 1 computes the total). Filters reset to page 1.
- [ ] **Malformed cursor is a 400, not silent page 1**: `curl -H "Authorization: Bearer <key>" "/v1/audit-log?after=garbage"` → 400 `invalid \`after\` cursor…` (never a 200 of page-1 rows — a corrupted cursor mid-page-walk must fail loud, not silently restart/duplicate an export). `?offset=-5` is clamped to 0, not a 500.
- [ ] **Audit reads are index-backed, mode-scoped**: with test-mode selected in the dashboard, `/audit-log` shows only test-mode rows (live rows absent) and vice versa; the action/resource filter dropdowns offer only values seen in the current mode when that mode has rows (an empty mode falls back to the FE's built-in default vocabulary). (Under the hood: explicit tenant+livemode predicates drive `idx_audit_log_tenant_read` — EXPLAIN shows an Index Scan, not a seq scan, migration 0147.)
- [ ] Destructive rows have red left border. Expand → metadata + "View" link.
- [ ] Filters: resource type, action, date range. **Export CSV → ALL matching entries, streamed from the server** (`GET /v1/exports/audit-log.csv`, no cap). It used to page the API in the browser and stop at 50,000 rows, handing the operator a truncated evidence file that looked complete.
- [ ] **Export honors the tenant timezone**: on a non-UTC tenant (e.g. IST), set a date-range filter so a row sits within an hour of a day boundary; the exported CSV contains exactly the rows shown on screen for that range — no edge row silently dropped from (or added to) the export.
- [ ] **Export carries the metadata**: the exported CSV's columns are `id, created_at, actor_type, actor_id, actor_name, action, resource_type, resource_id, resource_label, ip_address, request_id, sim_effective_at, test_clock_id, metadata_json` — the last one holding the row's metadata JSON (e.g. `{"action":"marked_uncollectible"}`, prorated amounts, old/new plan), empty for rows with no metadata. Free-text cells (`actor_name`, `resource_label`, `metadata_json`) are formula-neutralized.
- [ ] **Export of a filtered simulation is THAT simulation** — filter `/audit-log` to one test clock, then Export. The CSV contains only that clock's rows (not the whole log), its `sim_effective_at` / `test_clock_id` columns are populated on every row, and the export's own audit row records `test_clock_id` in its scope metadata — so "which slice left the building" is answerable. An export that silently dropped the clock filter would hand an auditor a file that looks like one simulation and is actually everything.

- [ ] **Entity pages link to their audit history**: Customer, Invoice, and Subscription detail pages each show an **"Audit log"** button in the header; clicking it opens `/audit-log` pre-filtered to that record (resource type + id), showing only that entity's events.
- [ ] **Date filter accepts both formats**: `?date_from=2026-01-01&date_to=2026-12-31` (bare YYYY-MM-DD) and `?date_from=2026-01-01T00:00:00Z` (RFC3339) both work. Invalid input (`?date_from=garbage`) → 400 with field-level error. Same shared parser (`internal/api/timefilter`) as the export + usage endpoints.
- [ ] **Audit timestamps: wall-clock primary, sim-time second axis (ADR-030 amendment; ADR-090 §5)** — on the clock-pinned sub, click Cancel from the subscription detail page. Open `/audit-log`, find the just-written `subscription.cancel` row. `created_at` is wall-clock (within ~5s of the system time the operator clicked) — NOT the test clock's frozen_time. Row shows an amber **test clock** chip next to the action label. Expand it: the "Timestamp" cell carries an amber subline "Effect on test clock `<clock_id>` at `<simulated frozen_time>`". `psql`: that row's **`sim_effective_at` and `test_clock_id` COLUMNS** (not just metadata) are populated and match the clock.

- [ ] **A clock advance stamps EVERY row it produces (sim-axis parity, ADR-090 §5)** — on a clock-pinned sub that is 3 months behind, click **Advance** once so catchup generates and finalizes 3 periods. `psql`: `SELECT action, resource_type, created_at, sim_effective_at, test_clock_id FROM audit_log WHERE tenant_id='<t>' ORDER BY created_at DESC LIMIT 10` → every row the advance produced has a NON-NULL `sim_effective_at` + `test_clock_id`. A row with a NULL `test_clock_id` here is a bug — it is invisible to the clock filter below. *(automated: `TestSimAxis_ClockDrivenLifecycle_EveryAuditRowIsStamped`)*

- [ ] **An engine-finalized invoice can't commit without its finalize audit row (ADR-090 shared fate)** — the `finalize` row now rides the invoice-create transaction, not a post-commit write. Prove it by fault injection: `psql -c "REVOKE INSERT ON audit_log FROM velox_app"`, then trigger a cycle (POST `/v1/billing/run` on a due sub, or Advance a behind clock). The run reports the finalize **failed** and **no new invoice exists** for that period (`SELECT count(*) FROM invoices WHERE subscription_id='<s>' AND billing_period_start='<p>'` → 0) — the invoice rolled back with its unwritable evidence rather than committing as an orphan. `psql -c "GRANT INSERT ON audit_log TO velox_app"` and re-run → the invoice and its one `finalize` row appear together. (Before: the invoice committed and the audit write happened afterward with its error discarded, so a failed write left a finalized invoice with no record of what created it.) *(automated: `TestFinalizeAudit_SharedFate` — real Postgres, mutation-verified)*

- [ ] **…and all of them share ONE simulated instant — the advance's** — in that same query, the three `finalize` rows carry the SAME `sim_effective_at` (the clock's new frozen_time), not three different months, and their `created_at` values are all within the same second. This is the axis telling the truth, not a bug: an advance does not replay time, it stands at the new instant and settles everything that came due, so that instant is when those finalizes were performed. `sim_effective_at` answers "where did the clock stand when this happened", which is why it can separate two ADVANCES but not the periods inside one. (Separating them would mean moving effective-now per billed period — a money-path change, since effective-now drives due dates and dunning.)

- [ ] **Filter the log to one simulation** — with at least one clock's rows recorded, `/audit-log` shows a **test-clock dropdown** (it is hidden entirely for tenants that have never used a clock). Select the clock → the list narrows to exactly that clock's rows; a second clock's rows and all wall-clock rows disappear. The URL carries `?test_clock=<id>` so the view is shareable.

- [ ] **Window the log in SIMULATED time** — advance the clock a second time (so the tenant has rows from two advances, at two different simulated instants). `curl -H "Authorization: Bearer <key>" "/v1/audit-log?sim_from=<first-advance-instant>&sim_to=<first-advance-instant>"` returns the FIRST advance's rows and not the second's — the axis separates advances. The same window on `?date_from=/?date_to=` returns nothing, because every row was written today: that divergence is the whole point of having two axes. Wall-clock rows never appear in a `sim_from/sim_to` window (their `sim_effective_at` is NULL, and NULL satisfies no range). There is deliberately **no "order by simulated time"** control: within one clock it would produce the same order as wall-clock (advances only move forward), and across two clocks it would interleave unrelated simulations into a timeline that never happened.

- [ ] **CSV export carries simulated time as its own column** — with the clock filter on, click **Export**. The CSV carries `sim_effective_at` and `test_clock_id` columns (server-generated, snake_case — the browser-side exporter with human-readable headers is gone), populated for every simulated row (empty for wall-clock rows), and contains exactly the rows on screen — the export honors the clock filter, it does not silently export everything.

- [ ] **The log survives the clock it describes (ADR-086 teardown + ADR-090 §5)** — note the clock's id, then **delete the clock** (its whole sandbox — customers, subs, invoices — is hard-deleted). Return to `/audit-log`: the clock is **still in the dropdown** (the picker is built from the audit rows, not from `test_clocks`, which is now empty), selecting it still returns the full simulated timeline — created, advanced, invoices finalized, and finally the `delete test_clock` row explaining why everything else is gone. This is the only surviving record of that simulation; if the dropdown lost the clock or the rows lost their `test_clock_id`, the simulation is unauditable.

- [ ] **…and it stops offering a door into the void** — on those same surviving rows (the ones naming invoices/customers the teardown hard-deleted), the row's **"View" link is gone**; expanding it still shows the full record (action, actor, resource id, metadata). Before, every one of them rendered a View link straight to a 404. Wall-clock rows, and simulated rows whose clock still exists, keep their working link. *(automated: `TestSubjectDeleted_TheRowOutlivesItsSubject`, `web-v2/tests/auditVocabulary.test.ts`)*

- [ ] **Known gaps, by design (do not report as bugs):** rows from **operator-driven credit-note actions**, **operator-driven price-override create/delete** (same root cause as credit notes — `internal/pricing` has no clock resolver; and note `customer_price_overrides` IS torn down with the clock, so after teardown that unstamped row is the only evidence the override existed), **payment-method attach/detach**, and **public checkout clicks** (hosted-invoice Pay, payment-update link) carry NO sim axis and do not appear under the clock filter. The CN gap is a symptom of a reported bug (`creditnote.Service` has no clock binding, so an operator-issued credit note on a simulated invoice also stamps wall-clock `issued_at` / `is_simulated=false`); PM and checkout are real-world Stripe effects that never bind a clock — consistent with the settlement webhook itself writing no audit row at all. All three are written down in ADR-090 §5.

## FLOW P2A: Audit log — customer-initiated + Tier 2 coverage

Verifies the 2026-05-26 audit sweep wired every state-changing flow into `audit_log` and the AuditLog page renders the new resource types correctly.

- [ ] Engine auto-fires scheduled cancellation (advance the test clock past cycle close) → AuditLog row: "Canceled <sub>" with meta `canceled_by=schedule`, actor "System".
- [ ] **Engine-finalized invoices are audited**: advance a test clock past a cycle close (or create a sub with an in_advance plan) → AuditLog shows a **"Finalized INV-NNN"** row for the engine-generated invoice, actor "System", meta `triggered_by=subscription_cycle|subscription_create|…`, with the test-clock chip/sim subline on clock-pinned subs.
- [ ] **Trial auto-expiry is audited**: let a trial lapse via clock advance (or the wall-clock scheduler) → "Trial ended" row on the subscription with `triggered_by=schedule`, matching the operator End-Trial row.
- [ ] Operator marks invoice uncollectible → "Marked INV-NNN uncollectible" — **exactly one row**.
- [ ] Operator records offline payment → "Recorded offline payment on INV-NNN".
- [ ] Operator clicks **Collect payment** on a finalized invoice → "Collected payment on INV-NNN" (amber/medium severity, `action='collect'`), NOT "Created INV-NNN".
- [ ] Operator clicks **Send** → "Emailed INV-NNN" (`action='send'`; metadata carries the invoice + customer ids, NOT the recipient address — PII stays out of the append-only log; the email outbox is the delivery record).
- [ ] Operator **Refund** → "Refunded INV-NNN" (red/high severity).
- [ ] Operator rotates the hosted-invoice link → "Rotated hosted-invoice link for INV-NNN". None of these render as a green "Created" row.
- [ ] Operator edits customer (display_name / email / dunning policy) → "Updated <name>".
- [ ] Operator upserts billing profile (tax status / address / tax ID) → "Updated billing profile for <name>".
- [ ] Customer self-serves a card via the emailed **payment-update link** → "Added Visa ····4242" (resource_type payment_method, action create) with actor = **the customer** (the setup_intent carries `velox_purpose=payment_update_token`, so the attach is attributed to them).
- [ ] Operator adds a card via **Add payment method** → same "Added …" row, but actor = **System** (operator-initiated attach carries no customer purpose; not misattributed to the customer).
- [ ] Operator promotes a non-default card to default → "Set Visa ····4242 as default".
- [ ] Operator detaches a card → "Removed Visa ····4242".
- [ ] Operator creates / revokes / rotates an API key → corresponding rows. Raw key value never appears in metadata.
- [ ] Operator creates / updates / deletes a dunning policy + sets default + resolves a run → matching rows. Resolve row carries `meta.resolution=payment_recovered` etc.
- [ ] Operator creates / updates a plan, creates a meter / rating rule / price override → matching rows.
- [ ] Operator creates / advances / retries / deletes a test clock → matching rows with `frozen_time` snapshot in metadata.
- [ ] Operator connects / disconnects Stripe / sets webhook secret → matching rows. Secret value never in metadata.
- [ ] Operator creates / deletes / rotates a webhook endpoint + replays an event → matching rows. Secret value never in metadata; replay row links to original via `meta.replay_of`.

## FLOW P2B: Operator-side payment-method management

- [ ] CustomerDetail → Payment Methods card lists every attached PM with brand/last4/expiry + Default badge on the primary.
- [ ] No card capture form on the dashboard (PCI: card data must go through Stripe-hosted iframe only).
- [ ] "Add Card" → dialog opens with the customer's email pre-filled (read-only), an optional note field, primary "Send email" button, and a secondary "Copy link" path under an "or" divider.
- [ ] Send email path: typing a note (≤2000 chars) + Send email → 202 from `/v1/customers/{id}/payment-methods/send-setup-email` → toast "Setup link sent to {address}" → Mailpit at http://localhost:8025 shows a branded "Add a payment method" email with the note rendered above the CTA → CTA opens a Stripe Checkout session → completing it adds a new PM and the panel refreshes.
- [ ] Empty note → email body uses the generic fallback intro ("Please add a payment method on file so we can process your billing…").
- [ ] Customer with no email on file → Send email button disabled with tooltip "Add an email on the customer record first"; Copy link path still works.
- [ ] Copy link path: clicking "Copy link" mints the session, auto-copies the URL, and displays the URL in the dialog with a Copy button for re-copy → paste into incognito tab → flow completes the same as the email path.
- [ ] Closing the dialog mid-flow discards any minted URL (operator can mint another fresh URL next time).
- [ ] Non-default card → "Set as default" promotes it; the badge moves; previous default loses its badge.
- [ ] **Auto-charge hits the explicitly-set default, not the newest card (ADR-053)** — attach two cards (`4242…4242`, then `5555…4444`), "Set as default" the FIRST/older one, then auto-charge an invoice (finalize a cycle or manual invoice). The paid invoice's payment-card sub-line shows the **default** card's brand/last4 (visa 4242), NOT the most-recently-added card — Velox names the exact card; Stripe never picks.
- [ ] "Remove" on a non-default card → confirm → card detaches in Stripe + disappears locally; default unchanged.
- [ ] **Remove the DEFAULT card with others present (ADR-053)** — with ≥2 cards, "Remove" the one holding the Default badge → confirm → it disappears and the **newest remaining** card gains the Default badge (never a no-default state). The promoted card also becomes the default on the customer's Stripe dashboard.
- [ ] "Remove" on the last card → confirm dialog still works → list becomes empty.
- [ ] "Remove" disabled (with tooltip) when only one card remains AND it's default — operator must add another card first.
- [ ] Archived customer → all PM action buttons hidden (parity with other archived-customer UI guards).
- [ ] AuditLog page renders the send-email action as **"Sent a payment-method setup link to {customer name}"**, with `meta.action=setup_link_sent` and `meta.session_id`. Actor = **the signed-in operator** (`actor_type=user`, Actor column shows their name/email) — the dashboard authenticates with a session cookie, not an API key. The row carries **NO recipient address**: customer PII must never enter the append-only log (guard: `TestAudit_NoCustomerPIIInMetadata`), and the email outbox is the delivery record that holds the actual address.
- [ ] Cooldown: clicking "Send email" twice in <60s → second call returns 429 with `Retry-After` header + toast "a setup link was sent to this customer recently — wait before sending again". Cooldown is a strict 60s window per customer; the next send succeeds only after it expires.
- [x] **Proactive no-card warning (customer page)** — a customer with an active auto-charging subscription and zero cards shows the amber "No payment method on file … bills automatically" warning in the Payment methods card; a card-less customer with **no** active subs keeps the neutral empty state. *(walked 2026-07-22: amber card on B20 Add Ari; neutral state on card-holding customer.)*
- [x] **Proactive no-card banner (subscription page)** — that customer's subscription page shows the amber **"No card on file"** banner with an "Add payment method" button → customer page; attaching a card or pausing collection removes it. *(walked 2026-07-22: banner + Add-payment-method button live; negative case verified on a card-holding sub.)*
- [ ] **Create-subscription nudge** — creating a start-now subscription for a card-less customer succeeds AND raises a "No payment method on file" warning toast; a customer with a card gets no toast.
- [ ] InvoiceAttention banner (invoice in attention state, e.g. `update_payment_method` action) → clicking "Update Payment Method" opens the SAME dialog as CustomerDetail's Add Card → recipient email pre-filled, note pre-filled with invoice context ("Invoice INV-NNN ($X.XX) is ready. Please add a payment method using the secure link below and we'll collect it automatically." — no decline assertion), email path lands a branded "Set up payment for invoice INV-NNN" subject in Mailpit; copy-link path mints a Stripe Checkout URL the operator can paste into Slack/SMS.
- [ ] Engine no-PM-at-finalize email (invoice finalizes for a customer with no PM on file) → branded "Set up payment for invoice X" email lands in Mailpit + AuditLog shows an **"Emailed a payment-method setup link to {customer} (invoice finalized with no card on file)"** row — `meta.action=setup_link_sent`, `meta.trigger=finalize_no_pm`, `meta.invoice_id=<id>`, `meta.invoice_number=<INV-NNN>` — with actor "System" (engine-fired). No recipient address on this row either.
- [ ] Legacy endpoints removed: `curl -X POST .../v1/payment-portal/{id}/update-payment-method` returns 404 (route deleted in the unified-paths cleanup).

## FLOW P3: Usage summary

- [ ] `GET /v1/usage-summary/{customer_id}?from=…&to=…` → per-meter aggregated totals matching ingestion.

## FLOW P4: Empty billing cycle

- [ ] No subs due → trigger billing → "0 invoice(s) generated", clean exit, dashboard unchanged.

## FLOW P5: Health checks

- [ ] `/health` → 200 `{"status":"ok"}`. `/health/ready` → 200 with database, scheduler ok.
- [ ] Stop Postgres → `/health/ready` → 503 `degraded` with `database: error:…`. `/health` still 200.
- [ ] Kill scheduler goroutine or wait past 2× interval → readiness shows scheduler degraded.

## FLOW P6: Tax deferral metrics

- [ ] **Late live usage events are observable** — POST a live-origin usage event timestamped >24h in the past (inside an open period): `GET /metrics` shows `velox_usage_late_event_total` incremented and a WARN log names the customer/meter/timestamp; a backfill-origin event does NOT increment it.

- [ ] `curl -H "Authorization: Bearer $METRICS_TOKEN" /metrics | grep velox_tax_outcome_total` → counter registered (the legacy `velox_tax_fallback_total` was renamed when the zero-tax fallback was cut — ADR-041; outcome is now `deferred`). <!-- currency-ok: documents the metric rename -->
- [ ] Reasons increment correctly: `velox_tax_outcome_total{outcome="deferred",reason=...}` for `no_country` (customer missing country), `no_client_for_mode` (Stripe not connected for the active livemode), `api_error` (invalid Stripe key).
- [ ] Happy path → counter unchanged.

---

## FLOW REC1: Self-healing background reconcilers

Failed external effects (tax reversal, ambiguous charge) self-heal via scheduler sweeps, observable per-reconciler.

- [ ] **A failed tax reversal still voids the invoice locally and marks it for retry** — on a `stripe_tax` invoice with a committed `tax_transaction_id`, make `ReverseTax` fail (point `stripe listen` away / break Stripe creds), then Void it. The invoice **voids** locally, an **ERROR** logs (`tax reversal failed on invoice status change — … recovery sweep will retry`), and `SELECT tax_reversed_at FROM invoices WHERE id='<inv>'` is **NULL**.
- [ ] **The reversal-recovery sweep heals it exactly once** — restore Stripe → on the next scheduler tick the sweep files the reversal at Stripe **once** (idempotent via the `inv_taxrev_<id>` reference), stamps `tax_reversed_at` non-NULL, and the row falls out of the sweep (no repeat reversals).
- [ ] **A fully-credit-noted void (nothing left to reverse) is stamped too** — so the sweep never loops on it. Clock-pinned/test invoices are excluded — the sweep is wall-clock/livemode only.
- [ ] **A failed credit-note tax reversal still issues the CN and marks it for retry (ADR-061)** — on a **finalized/paid** `stripe_tax` invoice with a committed `tax_transaction_id`, make `ReverseTax` fail (break Stripe creds), then **issue a credit note**. The CN **issues** (internal money effect committed atomically), an **ERROR** logs (`tax reversal failed; marked pending for sweep recovery`), and `SELECT tax_reversal_pending FROM credit_notes WHERE id='<cn>'` is **true**.
- [ ] **Its own sweep heals it exactly once** — restore Stripe → on the next tick `RetryPendingCreditNoteTaxReversal` files the reversal **once** (idempotent via the per-CN `velox_tax_rev_<cn_id>` key), stamps `credit_notes.tax_transaction_id`, clears the marker, and the row falls out. This is a **separate** sweep from the void-path one above — that one scans only voided/uncollectible invoices, so it never sees a reversal stamped on this finalized/paid invoice.
- [ ] **Recovery sweeps are observable per-reconciler:** while the stuck reversal above is pending, `GET /metrics` shows `velox_reconciler_sweeps_total{reconciler="tax_reversal",outcome="run"}` (or `="cn_tax_reversal"`) **incrementing every scheduler tick** (liveness) and `outcome="error"` climbing while it fails; once Stripe is restored, `outcome="advanced"` ticks up and `error` goes flat. All seven sweeps emit it (`payment_unknown`, `tax_retry`, `tax_commit`, `tax_reversal`, `clawback_issue`, `cn_tax_reversal`, `dunning_backfill`).
- [ ] **Payment reconciler stamps simulated `paid_at` for clock-pinned PaymentUnknown invoices** — simulate an ambiguous charge outcome on a clock-pinned invoice (Stripe API timeout / 5xx → invoice lands `payment_status=unknown` with a populated `stripe_payment_intent_id`). Wait ~70s wall-clock for the reconciler to fire. After resolution, `invoice.paid_at` lands in simulated time (test clock's frozen_time at the moment of the original charge attempt), NOT today's wall-clock.
- [ ] **Dropped failure-webhook is fully recovered by the reconciler — dunning + email, not just a status flip (ADR-049 Phase 2)** — charge a card that declines, but DROP the `payment_intent.payment_failed` webhook (e.g. stop `stripe listen` / point the endpoint away) so the invoice sits `payment_status=unknown` (ambiguous) or `processing`. Wait for the reconciler tick. The invoice flips to `failed` AND a dunning run exists for it AND a `payment_failed` email is enqueued (`email_outbox`) AND `payment.failed` fired (`webhook_outbox`) — identical to the webhook path.
- [ ] **Stale `processing` is swept once the PI settles (ADR-049 Phase 2)** — leave an invoice at `payment_status=processing` past the 30-min processing cool-off (drop the success webhook). Once Stripe's PI is `succeeded`, the reconciler marks the invoice `paid` and enqueues the receipt; while the PI is still `processing`/`requires_action` at Stripe, the reconciler leaves it alone (no premature settle). A `succeeded` webhook that lands DURING the reconciler's Stripe round-trip wins — the reconciler's fresh re-read sees the invoice already paid and skips (no duplicate receipt).

## UI / UX

## FLOW U1: Dashboard

- [ ] 4 KPI cards: MRR (sparkline+trend), Active Customers, Failed Payments (red if >0), Revenue 30d.
- [ ] MRR/movement honest under change: remove a subscription item → MRR drops AND Contraction gains it; items in a non-default currency never appear in MRR/ARR/movement; MRR-now − MRR-prev equals movement Net.
- [ ] Revenue bar chart, Recent Activity (last 5 invoices clickable).
- [ ] Get Started checklist: 5 steps (Stripe, plan, customer, subscription, webhook), auto-tracks against server state, self-hides at 100%. Dismiss persists per-tenant.
- [ ] No "Trigger Billing" button (use `POST /v1/billing/run`).

## FLOW U3: Usage Events page

- [ ] Stat cards: Total Events, Total Units, Active Meters, Active Customers.
- [ ] Meter breakdown bars.
- [ ] Filters: customer, date range. Stat cards stay constant when paging (reflect all filtered rows).
- [ ] **Dimension filter actually filters (2026-07-05):** type `model=gpt-4o` in the dimension box → event log AND stat cards shrink to matching events only (server-side `properties @>`; pre-fix the server ignored the param and showed unfiltered data as filtered). `token_type=input,model=gpt-4o` ANDs; `cached=true` matches the boolean; malformed input (`model`) → 422 surfaced, not silently unfiltered.
- [ ] Decimal precision: `0.5 + 0.5 + 0.0001` → `1.0001` (no rounding).
- [ ] Export CSV.

## FLOW U11: Operator search + list filters

Setup: ≥26 customers so at least one lands on page 2 (FLOW S1 tenant + a quick create loop works).

- [ ] Customers page: search a page-2 customer by email fragment → row appears; `Showing 1–1 of 1`.
- [ ] Customers page: search `zzz-no-match` → "No customers match" empty state with its own Clear filters button; search input still visible.
- [ ] Invoices page: search a full invoice number (e.g. `INV-2026-0003`) → that invoice only.
- [ ] Invoices page: From/To date pickers filter across pages (pick a range excluding today → today's invoices gone, total shrinks).
- [ ] Invoices page: **Past due** tab → only finalized invoices with `due_at` in the past and payment not succeeded/processing.
- [ ] Customer detail → Outstanding card link → Invoices opens with a dismissible `customer: <name>` chip and only that customer's invoices; × clears it.
- [ ] Customer detail → Sent emails → click an invoice number → Invoices opens pre-searched to that number.
- [ ] ⌘K: type a page-2 customer's email → customer appears with email in the subtitle; Enter navigates to its detail page.
- [ ] ⌘K: paste an invoice number → invoice appears; works for invoices beyond the 50 most recent.
- [ ] Subscriptions page: search by code fragment → matches across pages.
- [ ] Refresh any filtered list URL → filters (search/status/dates/page) restore from the URL.
- [ ] Customer detail → External ID row has a copy icon; Subscription detail → Customer row has a copy icon (copies the raw customer id).

## FLOW U12: Dashboard consistency sweep

- [ ] **Relative-time surfaces follow the test clock (ADR-086 §4)** — advance a clock-pinned customer +1 month: their cycle progress ("Day N of M"), "X ago" stamps, and rolling 7/30/90d windows all read the frozen simulated now — never wall clock; non-pinned rows on the same page keep wall-clock relative times.

- [ ] Browser tab reads "Invoices · Velox" on the list, "INV-…-NNNN · Velox" on an invoice; two different tabs are distinguishable.
- [ ] An invoice ≥ $1,000 renders with thousands separators everywhere ($10,000.00, not $10000.00).
- [ ] On /customers/:id the sidebar still highlights Customers; on /plans/:id it highlights Pricing.
- [ ] Dunning policies → Delete asks via an in-app dialog (no native browser confirm). Webhooks → Rotate Secret shows "Rotating…" and can't be double-clicked.
- [ ] Webhook live tail shows customer names, not raw vlx_cus ids (unknown ids show shortened, full id on hover).
- [ ] ⌘K → type a plan name → Enter lands on the plan's detail page.

## FLOW U7: Edge cases

| Case | Expected |
|------|----------|
| Zero usage | Base fee only |
| Meter without rating rule | Plan attach 422s ("meter … has no rating rule"); if unbound under a live sub, usage not billed + WARN log (ADR-096) |
| Invalid `external_customer_id` on ingest | "customer not found" |
| Invalid `event_name` on ingest | "meter not found" |
| Void already voided invoice | Error |
| Finalize non-draft invoice | Error |
| Duplicate subscription code | Humanized error |
| Cancel canceled subscription | Error |
| Esc from modal with form data | "Unsaved changes" prompt |
| Typed destructive confirm | `VOID` / `CANCEL` / `DELETE` required to enable submit; wrong word keeps button disabled |

(Duplicate Idempotency-Key behaviour — same-body cached, different-body 409 — covered by FLOW B5.)

## FLOW U8: Request-ID in error toasts

- [ ] Force any API error → toast shows `Request ID: <id>` (clickable to copy). The id is server-minted with a `req_` prefix (Velox no longer uses chi's `<host>/<base32>-<counter>` shape, and never honours an inbound `X-Request-Id` — see FLOW P2).
- [ ] Even when response envelope fails to parse → Request-Id from `Velox-Request-Id` header still appears.
- [ ] `grep "<request-id>" server.log` (the id copied from the toast) matches.

## FLOW U10: Public pages

- [ ] `/invoice/:token` (FLOW I10), `/update-payment` (FLOW D4), `/payment-method-added`, `/login` all load without auth. (The old `/checkout/success`/`/checkout/setup`/`/checkout/status` routes were removed in the unified-PM-paths cleanup.)

---

# Tier 3 — Deep / Rare

Major releases, infra changes, post-mortems.

## FLOW X1: RLS multi-tenant isolation

- [ ] Bootstrap Tenant A + key A; create "Alpha Corp". Bootstrap Tenant B + key B; list customers with key B → Alpha NOT visible.
- [ ] `GET /v1/customers/{alpha_id}` with key B → 404.
- [ ] Same check for invoices, subs, credits — cross-tenant reads must 404.

## FLOW X2: Bootstrap lockdown

- [ ] Fresh DB, no `VELOX_BOOTSTRAP_TOKEN` → POST /v1/bootstrap → 403 `bootstrap disabled`.
- [ ] Fresh DB, wrong token → 403 `invalid bootstrap token`.
- [ ] Tenants exist → 409 `already_bootstrapped` for EVERY probe: valid token, wrong token, no token, even token-unset (ADR-073 — no token-validity or token-configured oracle).
- [ ] Bad `owner_password` (<12 chars) → 422 AND zero rows written (tenants/users/api_keys all empty); retry with a valid password → 201.
- [ ] `make bootstrap` CLI always works (multi-tenant re-runs with a different email).

## FLOW X2b: Self-host bootstrap → dashboard login → live key (ADR-073)

Setup: fresh DB, `VELOX_BOOTSTRAP_TOKEN` set (≥16 chars).

- [ ] POST /v1/bootstrap with token + `{"owner_email","owner_password"}` → 201 with `Cache-Control: no-store`; response carries `owner_email`, `owner_password`, `secret_key_test` (`vlx_secret_test_…`), `secret_key_live` (`vlx_secret_live_…`), `publishable_key_test`.
- [ ] POST /v1/auth/login with those credentials → 200 + `velox_session` cookie.
- [ ] `GET /v1/customers` with `Authorization: Bearer <secret_key_live>` → 200 (live mode reachable without psql).
- [ ] Omit owner fields → owner defaults to `admin@velox.local` with a generated password in the response.
- [ ] `APP_ENV=production` boot without `APP_DATABASE_URL` (or with password `velox_app`) → process exits with `refusing to start` naming APP_DATABASE_URL; with `APP_DATABASE_URL` pointed at the admin role → exits with `can BYPASS row-level security`.

## FLOW T1: Team invites (ADR-081, 2026-07-06)

Setup: `DASHBOARD_BASE_URL` set (invites refuse to mint without it), Mailpit up.

- [ ] Settings → Team: invite `teammate@example.com` → invitation row appears (pending, 7-day expiry, "Invited by <your email>"); Mailpit shows the invite email with an accept link to `DASHBOARD_BASE_URL/accept-invite?token=…`.
- [ ] Open the link logged out → page shows the workspace name + invited email + password form (new account). Set a 12+ char password → lands signed in on the dashboard; Settings → Team lists 2 members; audit log has `member.invited` (inviter as actor) + `member.joined` (invitee as actor).
- [ ] Re-invite the SAME email while pending → 409 "pending invitation … already exists"; revoke it → its accept link now shows "no longer valid"; re-invite succeeds.
- [ ] `POST /v1/members/invite` with a Bearer secret key → 403 (dashboard-session only).
- [ ] Remove the invitee (confirm dialog warns they're signed out everywhere) → their session is dead on next request; re-inviting them works and accept says "You already have a Velox account" (no password form; attach only, then sign in at /login).
- [x] Remove yourself → blocked; remove the last member → blocked. *(automated: `internal/dashmembers` integration tests — golden path, existing-user attach, gates, revoked/expired tokens, session revocation, concurrent accept)*

## FLOW E1: Additional billing emails + credit-note send (ADR-082, 2026-07-06)

Setup: Mailpit up, a customer with a paid invoice.

- [ ] Customer → Edit: set Additional billing emails to `finance@x.test, eng@x.test` → save; reopen shows both (lowercased). Setting 11 addresses or repeating the primary → inline 422.
- [ ] Invoice → Send Email: CC field prefilled with both addresses → send → Mailpit shows ONE message with To=primary, Cc listing both; all three mailboxes received it.
- [ ] Send again with the CC field CLEARED → only the primary mailbox receives (explicit `[]` = primary only).
- [ ] Issue a credit note on the invoice → Credit Notes → Send on the issued row → branded credit-note email with PDF attached arrives at the same recipient set; the send appears on the invoice timeline and the customer's Sent emails as "Credit note". Draft/voided CN → Send returns 422.
- [x] Legacy API body `POST /v1/invoices/{id}/send {"email": ...}` (no additional_emails key) → CCs the stored list (the Orb-parity default). *(automated: `TestCC_*` transport pins — RCPT set, misattribution, transport-abort, suppression; outbox cc round-trips; customer store encryption round-trip; tri-state handler tests; CN send guards)*

## FLOW X3: Rate limiting

- [ ] **`/v1/auth` never fails closed** — with Redis down (even in prod mode), login still succeeds; ingest likewise stays open; only the general limiter fails closed in prod.

- [ ] 100+ concurrent CRUD requests (e.g. `GET /v1/customers`) → first 100 ok, rest 429 with `Retry-After` + `X-RateLimit-*` headers.
- [ ] Wait 10s, 20 more → ~16 allowed (general limit is 100/min = 1.67/sec, so 10s refills ≈ 16.7 tokens).
- [ ] **Ingest rides its own bucket (2026-07-05):** `POST /v1/usage-events` / `/batch` / `/integrations/litellm/spend` respond with `X-RateLimit-Limit: 1000` (per second — LiteLLM POSTs one callback per LLM call; the 100/min CRUD bucket silently dropped its events, since LiteLLM retries only on 5xx). Exhausting the general bucket does NOT 429 ingest. Overrides: `VELOX_RATE_LIMIT_GENERAL_PER_MIN`, `VELOX_RATE_LIMIT_INGEST_PER_SEC`.
- [ ] Tenant A exhausted → Tenant B succeeds (separate buckets).
- [ ] Stop Redis → requests succeed (fail-open in dev). `APP_ENV=production` → general fail-closed; **ingest AND `/v1/auth` stay fail-open even in prod** (a Redis blip must not drop revenue events or lock operators out of login; the per-IP `/v1/auth` limiter is v1's only brute-force floor — no per-account lockout/throttle exists, removed per ADR-094 (migrations 0153/0154); 2026-07-06 HA audit fix). Boot logs state the split explicitly, never a blanket "fail open".
- [ ] `/health`, `/health/ready`, `/metrics` not rate-limited.

## FLOW X4: Security headers + metrics auth

- [ ] `curl -I /v1/customers` carries: `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`, `Cache-Control: no-store`, `Referrer-Policy: strict-origin-when-cross-origin`.
- [ ] Staging/prod: `Strict-Transport-Security` present.
- [ ] `METRICS_TOKEN=secret123` set → `/metrics` 401 unauth, 200 with Bearer. Unset → `/metrics` accessible (dev).

## FLOW X5: PII encryption at rest

- [ ] `VELOX_ENCRYPTION_KEY` set (64 hex). Create customer + billing profile.
- [ ] `SELECT display_name, email FROM customers …` → values prefixed `enc:`. API returns plaintext.
- [ ] `SELECT legal_name, phone, tax_id FROM customer_billing_profiles …` → all 3 prefixed `enc:`. (email column dropped in migration 0100 — recipient is `customers.email` only.)
- [ ] Pre-encryption rows still read correctly (no `enc:` prefix → returned as-is).

## FLOW X7: Stripe Tax

- [ ] Settings → Tax → set Tax provider = **Stripe Tax** (`tenant_settings.tax_provider="stripe_tax"`). Customer with full address → invoice tax calculated by Stripe; `tax_name` shows jurisdiction; per-line tax populated.
- [ ] Invalid Stripe key → invoice is **deferred** to `tax_status=pending` (NOT charged $0 — the zero-tax fallback was removed in ADR-041); the TaxRetrier reconciles it later. Counter `velox_tax_outcome_total{outcome="deferred",reason="api_error"}` +1.
- [ ] Customer `tax_status=exempt` → $0 tax regardless.
- [ ] India-registered Stripe account → blocked at account level → use FLOW B10.
- [ ] **Re-connect flushes stuck tax (ADR-019)**: with Stripe disconnected, advance a test clock to generate an invoice → invoice goes `tax_status=pending` with `tax_error_code=provider_not_configured`. Reconnect Stripe in Settings → Payments. Toast reads `Connected test mode as <Account>` with description `Retrying 1 invoice that was waiting for a Stripe connection.` Reload `/invoices` after a moment — invoice flipped to `Open` (engine-generated → auto-finalized via ADR-017 chain). No per-invoice manual Retry-tax click required.

## FLOW X8: Migration rollback

- [ ] `make migrate-status` → version N. `migrate rollback` → N-1. `make migrate` → N.
- [ ] `docker compose down -v && make up && make dev` (DESTROYS local data — run only when a fresh DB is the point) → fresh DB applies all migrations; `migrate status` reports the HIGHEST embedded version in `internal/platform/migrate/sql/` (note: 0143 is a deliberate numbering gap, so version ≠ file count — compare against the top filename, never a count or a number written here).

## FLOW X9: Config validation

- [ ] No `VELOX_ENCRYPTION_KEY` in production → fatal.
- [ ] Key not 64 hex / not valid hex → fatal.
- [ ] `APP_ENV=production` no `REDIS_URL` → warn "rate limiting will fail open".
- [ ] `APP_ENV=foo` → warn listing expected values. `PORT=not-a-port` → warn.
- [ ] `DB_MAX_IDLE_CONNS > DB_MAX_OPEN_CONNS` → warn.
- [ ] All valid → zero WARN-level config logs.

## FLOW X10: OpenTelemetry tracing

```bash
docker run -d --name jaeger -p 16686:16686 -p 4318:4318 jaegertracing/jaeger:2
OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318 go run ./cmd/velox
```

- [ ] Hit several endpoints. Jaeger UI at :16686, service `velox`.
- [ ] HTTP spans (method+path), `billing.RunCycle` with `batch_size`, `billing.BillSubscription` with `subscription_id`/`tenant_id`.
- [ ] HTTP → billing parent-child relationship visible.

## FLOW X11: Large batch usage ingestion

- [ ] POST /v1/usage-events/batch with 1000 events → `{ingested:1000, deduplicated:0, total:1000}` (the 201 body has no `errors` key — that appears only on the 422 `batch_rejected` path).
- [ ] Include duplicate idempotency keys → duplicates rejected.
- [ ] Run billing → aggregated correctly.

## FLOW X14: Self-host (Compose)

- [ ] `docker compose up -d postgres redis mailpit` → 3 sidecars healthy.
- [ ] `make bootstrap` + `make dev` + `cd web-v2 && npm run dev`. `/health` and `/health/ready` 200.
- [ ] `RUN_MIGRATIONS_ON_BOOT=true` applies all migrations idempotently (default is `false` — set it explicitly for local dev).
- [ ] Mail catches at `localhost:8025`.
- [ ] **Full-stack compose ships the dashboard (2026-07-05):** `cd deploy/compose && docker compose up -d --build` → 5 containers; `http://localhost/` serves the operator UI (login with the bootstrap owner credentials — same origin as the API, no CORS/VITE_API_URL config), `http://localhost/health` still hits the API, deep links like `/invoices/<id>` survive refresh (SPA fallback), and the SSE webhook stream stays open past 35s (dedicated proxy location, buffering off).

## FLOW CR1: Paid-commit relief (ADR-080, 2026-07-06)

- [ ] Hand-build a DISCOUNTED commit invoice (price $90.00, granted $100.00), finalize, `RecordOfflinePayment`. Draw $40.00 of credits via usage/apply. `POST /v1/credit-notes {invoice_id, commit_relief: {retire_all: true}}` → 201, CN **issued** immediately, `total_cents = 5400` (price-ratio: the $40 drawn were bought at 0.9 — NOT $60.00 face, NOT $50.00), `commit_retired_cents = 6000`, `credit_amount_cents = 0`, allocation defaulted to `out_of_band` (offline-paid). Grant fully consumed; balance 0; ONE `credit.commit_retired` outbox row.
- [ ] Partial then rest: fresh commit, relieve `retire_cents: 2000` → $18.00; draw $30.00; `retire_all` → $45.00. Cash telescopes exactly (Σ = f(total retired)); no rounding drift across any partial split.
- [ ] Repeat relief on the exhausted commit → 409 "fully consumed"; relief on an UNPAID commit → 409 naming void; ordinary line CN on a PAID commit → 409 naming commit relief; `retire_cents` above remaining → 422 carrying the LIVE remaining + max refundable; expired-then-swept commit → 409 (breakage is earned).
- [x] Card-paid commit: relief allocation defaults to the Stripe refund channel, keyed `velox_cn_<id>`; a failed refund leg leaves the CN issued + credits retired with `refund_status=failed` → Retry refund converges (safe direction: never over-relieved). *(automated: `TestCommitRelief_*` — worked example, telescoping, race, gates, expiry)*

## FLOW CG1: Commit / credit-grant burndown (2026-07-06)

- [ ] Grant a promo credit + finalize a commit invoice → `GET /v1/credits/grants/{customer_id}` lists both blocks with `amount/consumed/remaining`, `grant_kind`, `expires_at`; `commit_remaining_cents` / `promotional_remaining_cents` split the headline balance.
- [ ] Customer page: Credit Balance stat shows the "Commit $X · Promo $Y" subline when either class has remaining; the **Credit grants** card lists per-grant Granted/Drawn/Remaining/Expires with kind badges.
- [x] Drain past the promo total → promo row leaves the live list (`include_exhausted=true` still shows it); commit remaining unchanged until promo exhausts (ADR-078 drain order). *(automated: `TestListGrants_BurndownAndKindSubtotals`)*

## FLOW M1: Provider cost tables + margin (ADR-079)

- [ ] Settings → Provider costs (or `PUT /v1/provider-costs`): add a rate `{provider: "anthropic", model: "claude-sonnet-4.5", token_type: "input", cost_per_token: "0.000003"}` → row appears in the table. *(order matters: rates BEFORE usage — events ingested earlier stay honestly uncosted)*
- [x] Ingest 1,000 input tokens with dims `{provider, model, token_type}` → `GET /v1/usage-events` shows `provider_cost_micros: 3000`, `provider_cost_source: "table"`. *(automated: `TestProviderCostStamp`)*
- [ ] Edit the rate to 0.000009 → old events keep 3000 micros; a NEW event stamps 9000 (snapshot semantics).
- [ ] Ingest a token event for a model with NO rate → `provider_cost_micros` null (uncosted, actionable); a non-token event (no provider/model dims) → `provider_cost_source: "not_applicable"`.
- [ ] **Margin window picker (2026-07-06):** the customer Margin card offers Last 7/30/90 days + Custom (two date inputs); switching windows refetches (`from`/`to` on `GET /v1/customers/{id}/margin`); Custom waits for both dates before querying.
- [x] `GET /v1/customers/{id}/margin` → headline revenue vs cost + margin %; per-model rows show margin ONLY for model-pinned pricing rules; flat-rule revenue shows under "not model-attributed"; `unresolved_events` counts only the missing-rate token events. *(automated: `TestMargin_AttributionHonesty`)*
- [ ] Customer page (operator) shows the margin card; the CUSTOMER-facing hosted cost dashboard shows NO cost/margin data.
- [ ] Usage CSV export includes `provider_cost_micros` and `provider_cost_source` columns.
- [x] Test-mode rate rows don't cost live-mode events (and vice versa). *(automated: `TestProviderCostStamp_LivemodeIsolation`)*

## FLOW X15: LiteLLM integration (ADR-033)

The wedge integration. Validates the adapter accepts LiteLLM's `StandardLoggingPayload`, resolves customer + meters, and dedupes replays.

- [ ] Create a customer with `external_id="cus_litellm_test"`.
- [ ] Instantiate the `anthropic_style` recipe (creates one `tokens` meter + per-`{model, token_type}` rules — ADR-044).
- [ ] Single payload happy path:
  ```bash
  curl -X POST "$API/v1/integrations/litellm/spend" \
    -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
    -d '{
      "id":"litellm_test_1","call_type":"completion",
      "model":"claude-sonnet-4-5-20250929","custom_llm_provider":"anthropic",
      "user":"cus_litellm_test",
      "usage":{"prompt_tokens":1200,"completion_tokens":350,"total_tokens":1550},
      "response_cost":0.018,"endTime":1730000000
    }'
  ```
  → `{"accepted":2,"skipped":0,"errors":[]}`. `GET /v1/usage-events?customer_id=<internal cus_ id>` shows TWO events on meter `tokens` — `token_type=input` qty 1200 + `token_type=output` qty 350 — both with `dimensions.model="claude-sonnet-4.5"` (the canonical recipe family — the mapper normalizes) + `dimensions.model_raw="claude-sonnet-4-5-20250929"` + `dimensions.provider="anthropic"`. (List filter is the internal `customer_id`, not `external_customer_id`.)
- [ ] Idempotent replay: same POST again → `accepted=0` AND `errors:[]` **empty** (the store's duplicate-key replay is silent success, not a per-row error — regression: pre-2026-07-05 every replay filled `errors[]` while the DB dedup was in fact working). No duplicates in the event list.
- [ ] Missing `user`: payload with `"user":""` → response has `errors[]` populated, batch otherwise OK. **NOT 5xx.**
- [ ] Unknown customer: `"user":"cus_nonexistent"` → same partial-failure shape: `errors[].error` says `customer "cus_nonexistent" not found (set user=...)`.
- [ ] Non-token-bearing call: `"call_type":"image_generation"` → `accepted=0, skipped=1`. No events emitted.
- [ ] Zero-token completion (error / empty response): `"usage":{"prompt_tokens":0,"completion_tokens":0}` → `accepted=0, skipped=1`.
- [ ] Batch shape: POST `{"events":[<payload1>,<payload2>,...]}` → each payload mapped independently. Per-row failures don't fail the batch.
- [ ] Bare array shape: POST `[<payload1>,<payload2>]` → same handling as `events:[...]`.
- [ ] Embedding call: `"call_type":"embedding","usage":{"prompt_tokens":500,"completion_tokens":0}` → ONE event (meter `tokens`, `token_type=input`), `accepted=1`.
- [x] Dimension promotion: `"metadata":{"team_id":"team_eng","request_tags":["prod","batch"],"x_other":"ignored"}` → emitted events have `dimensions.team_id="team_eng"` and `dimensions.request_tags="batch,prod"` (LiteLLM's LIST is joined to a sorted comma-separated scalar — pre-2026-07-05 the raw array failed scalar dimension validation and silently rejected EVERY event on tagged calls); `x_other` is not promoted to dimensions. *(automated: `TestMapPayload_RequestTagsListBecomesScalar`)*
- [ ] Cost surfacing: `cost_breakdown:{input_cost:0.012,output_cost:0.045,total_cost:0.057}` → input event's `metadata.velox.litellm_cost_usd=0.012`, output event's `metadata.velox.litellm_cost_usd=0.045`. Velox billing math is unaffected — pricing rules drive the invoice amount; LiteLLM's cost is audit-only.
- [ ] Auth: POST without `Authorization` header → 401. Publishable key (no `usage:write`) → 403.
- [ ] Audit-trail sanity: each ingested event shows `origin=api` in `usage_events.origin` (no separate "litellm" origin in v1; revisit when an operator asks).

---

# Diagnostics

## Server won't start
- `VELOX_ENCRYPTION_KEY` rejected → FLOW X9 (must be 64 hex chars).
- Postgres unreachable → `docker compose ps`, `make up`.
- Port 8080 in use → `lsof -i :8080`, kill stale.
- Migration dirty → resolve SQL error, then `migrate force <version>`.

## Sign-in fails
- 401 → wrong creds. Re-bootstrap or use password-reset.
- 429 → per-IP `/v1/auth` rate limit (100/min per IP), not a lockout — v1 has no account lockout; repeated wrong passwords 401 forever (ADR-094). Wait ~1 min, retry.
- CORS: `CORS_ALLOWED_ORIGINS` must include frontend origin.
- Cookie not set → check `Set-Cookie` on response. `Secure` in dev should be off.
- Cookie present but every request 401s → check `dashboard_sessions.expires_at` / `revoked_at`.

## Invoice didn't generate
- Subscription not due → period end in future. Backdate for testing.
- Already billed → FLOW B3 (idempotent skip).
- Subscription paused / customer archived / trial active → no billing.

## Stripe Tax errors
- These defer the invoice to `tax_status=pending` (the TaxRetrier reconciles) and bump `velox_tax_outcome_total{outcome="deferred",reason=...}` — they do NOT charge $0 (zero-tax fallback removed, ADR-041).
- Missing customer address → `{reason="no_country"}`.
- Tenant in disconnected mode → `{reason="no_client_for_mode"}`.
- Invalid key → `{reason="api_error"}`.

## Rate limit not triggering
- Redis not connected → `redis-cli ping`, check `REDIS_URL`.
- Sequential curl too slow — use parallel.
- Public endpoints (`/health`, `/metrics`, `/v1/bootstrap`) intentionally unrestricted.

## PII not encrypted
- `VELOX_ENCRYPTION_KEY` not set when row created → backward-compat plaintext (FLOW X5).
- Wrong field — only customer display_name/email + billing profile legal_name/phone/tax_id are encrypted.

## Webhook signature fails
- Wrong `whsec_…` after `stripe listen` restart (CLI rotates per run).
- Clock skew >5 min → rejected (FLOW W1).
- Wrong webhook URL — must be `/v1/webhooks/stripe/<vlx_spc_…>`.