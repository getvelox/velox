# Design — Provider cost tables + margin (LOCKED)

**Status:** LOCKED for build (2026-07-05) after a 4-platform quote-verified
sweep (wf_2edadfc3: 37/40 claims) and a 5-lens adversarial panel
(wf_977f5dc5: ingest-perf, money-semantics, schema-lifecycle, scope,
siteset — all folds applied below). Decision record: ADR-079.

## Why

Wedge margin story: billed revenue vs provider COGS per customer.
**Verified differentiation:** no billing engine (Orb/Metronome/OpenMeter)
has a first-class provider-cost table or in-app margin report — Metronome
pushes COGS joins to the operator's warehouse. LLM tooling (LiteLLM/
Langfuse/Helicone) all model cost as a rate table. Velox builds the table
the billing engines lack, in the LLM-tooling shape, joined in-app.

## Corrected census (panel-verified; the pre-panel RFC was wrong)

- LiteLLM observed cost is **never persisted**: mapper stamps
  `velox.litellm_*_usd` onto ExternalIngest.Metadata (mapper.go:269) but
  litellm/handler.go persist() drops it — IngestInput, domain.UsageEvent,
  and the INSERT have no metadata carrier; usage_events has NO metadata
  column. payload.go:80-84's "metadata column" comment is doc-code drift —
  fix it in this build.
- usage_events.quantity is NUMERIC(38,12) (0054 ALTERed in place).
- There is NO batch write path: BatchIngest loops per event, each its own
  tx; every writer (live POST, batch, backfill, LiteLLM — plus
  the k6 bench profile, which drives the public endpoint like any other
  client) funnels through Service.ingest → store.Ingest. **Stamping
  lives in that single funnel; every path is covered by construction.**
- Event dims: `model` holds canonical FAMILY tokens for known models
  (e.g. `claude-3.5-sonnet`) but RAW ids for unknown ones; `model_raw`
  always holds the verbatim string. Cache-WRITE tokens never become
  events (ADR-044 deferral).
- Rated revenue per (meter, rule) exists via CustomerUsageService.rateMeter;
  per-model revenue exists ONLY when the rule's dimension_match pins model.
- Cost dashboard (cost_dashboard.go) is CUSTOMER-facing — COGS must never
  render there (allowlist projection verified leak-proof).

## Phase 1 (locked decisions D1–D10)

**D1. Table.** `provider_cost_rates`: id, tenant_id, livemode, provider,
model, token_type, cost_per_token NUMERIC (decimal string at API — rates
hit 1.5e-06/token; never float), currency (default USD), created_at,
updated_at. `UNIQUE(tenant_id, livemode, provider, model, token_type)`,
edit-in-place. **No effective_from** (no peer effective-dates cost rates;
the ingest stamp IS the snapshot; trigger to add: first pre-staged
price-change ask). Migration must EXPLICITLY add: livemode column,
set_livemode trigger (hard-coded per-table list, 0021 pattern),
ENABLE + FORCE ROW LEVEL SECURITY + tenant policy.

**D2. Stamp at ingest, single source.** Rate-table inference in phase 1;
observed cost added 2026-08-05 (see D4). New
nullable columns on usage_events: `provider_cost_micros BIGINT` (int64
micro-dollars, ROUND HALF UP at stamp; SUMs stay exact),
`provider_cost_source TEXT CHECK IN ('table','observed','not_applicable')`
— the shipped constraint (`0137_provider_cost_rates.up.sql:60-62`) already
carries `'observed'`, so the fast-follow needed no migration, exactly as
intended. (An earlier draft of this line omitted `'observed'` and was
stale against the migration from the day it shipped.)
Implementation: SQL scalar subquery inside the existing store.Ingest
INSERT — resolve `(provider, model_raw→model fallback, token_type)` from
the event's dims against provider_cost_rates; zero extra round trips, no
cache, always fresh, empty table ≈ free. Events with no resolvable dims
(non-token meters) stamp source='not_applicable'; token events with no
matching rate stay NULL ('unresolved'). A resolution error must not fail
ingestion beyond the tx it rides (it's one INSERT — atomic by shape).

**D3. Resolution key order:** `model_raw` exact → `model` (family) →
NULL. Non-retroactive forever: events ingested before a rate existed stay
NULL; NO recompute/backfill tooling (universal verified snapshot
semantics + house no-speculative-backfill).

**D4. Observed cost — SHIPPED 2026-08-05** (was: named fast-follow, not phase 1). Rule pinned now:
per-half CostBreakdown only (input_cost→input event, output_cost→output
event); cache_read halves and ResponseCost-only payloads fall to table
inference; whole-call ResponseCost is NEVER stamped on a per-half event
(3× COGS). Plumbing now exists: typed `ObservedCostMicros *int64` on
`litellm.ExternalIngest` → `usage.IngestInput` → `domain.UsageEvent` →
the `ingestOneTx` INSERT, where a non-NULL value wins over the rate
table and stamps `provider_cost_source='observed'`. The per-half rule is
enforced in `litellm.observedCostMicros`, which is the only place that
reads `CostBreakdown`. Float guard: reject
negative/NaN, document dollars-float→micros rounding.

**D5. No import in phase 1** (trigger: first operator with >10 models).
When it lands: file upload only (never live-URL fetch), unit
normalization explicit, keys translated through canonicalModel with
unmapped ids surfaced, preview-diff, operator rows always win.

**D6. One margin surface.** `GET /v1/customers/{id}/margin?from&to`
(operator auth): headline = customer-level margin (total rated usage
revenue vs total stamped cost, same window); per-model rows show cost
always, revenue + margin % ONLY for rules whose dimension_match pins
model; remaining revenue in an explicit "not model-attributed" row.
Rendered as a card on CustomerDetail (operator side). NEVER on the
customer-facing cost dashboard. Currency: no FX — if cost currency ≠
tenant billing currency, show cost separately, no margin %, loud note.

**D7. Uncosted honesty, scoped.** The card counts 'unresolved' token
events separately from 'not_applicable' (else api_calls meters drown the
signal). Copy: "events ingested before a matching rate existed; new
events are costed automatically." Disclose the cache-write exclusion.

**D8. Exports.** Operator usage-events CSV (exports.go ~:424) gains
provider_cost_micros + provider_cost_source columns — same PR (Metronome's
verified warehouse-join workflow is the fallback consumer).

**D9. CRUD surface.** REST CRUD for provider_cost_rates + OpenAPI +
dashboard page (Settings-adjacent "Provider costs"): list/add/edit/delete
rows; delete leaves stamped events untouched (documented).

**D10. Tests.** Rate resolution via mapper-emitted dims (not hand-written):
model_raw hit, family fallback, miss→NULL; not_applicable scoping;
micros rounding named-mode; RLS + livemode isolation on the new table
(trigger verified); margin endpoint attribution (pinned-model rule vs
flat rule → not-attributed row); export columns; stamp rides every funnel
path (live/batch/backfill/LiteLLM). MANUAL_TEST flow orders rates BEFORE
ingest.

## Phase 2+ (trigger-gated)

Observed-cost stamping (D4 rule) · effective_from versioning · import ·
regex model match + context-size tiers (Langfuse-verified requirements) ·
cost-plus pricing (2-2 verified split, not table stakes) · per-customer
negotiated cost rates · margin analytics page/trends.

## Research provenance

Peer sweep wf_2edadfc3 (2026-07-05): Orb 4/5, Metronome 11/11, OpenMeter
8/9, LiteLLM/Langfuse/Helicone 14/15 — adversarially quote-verified.
Panel wf_977f5dc5 (2026-07-05): 5 lenses, all amend, folds above.
