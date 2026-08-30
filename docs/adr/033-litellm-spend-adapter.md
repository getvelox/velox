# ADR-033: LiteLLM spend adapter — wedge integration

**Status:** Accepted
**Date:** 2026-05-14
**Related**: ADR-031 (per-plan bill_timing), `project_positioning_wedge.md`

## Context

LiteLLM is the de-facto open-source proxy / gateway for LLM API calls — Anthropic, OpenAI, Bedrock, Vertex, etc., behind one OpenAI-compatible interface. Most AI infra teams that self-host their LLM stack run LiteLLM in front of providers; it emits structured per-call spend logs (`StandardLoggingPayload`) via configurable callbacks.

Velox's wedge ("AI-native self-hosted billing engine") needs to feel native to that stack. Without a first-class LiteLLM integration, the demo answer to "how do I get my token spend into Velox?" is "write a custom ingest script." That's the friction LiteLLM itself exists to remove for LLM calls; Velox can't replicate it for billing.

## Decision

Ship a **push-mode adapter** that accepts LiteLLM's `StandardLoggingPayload` directly. Operator wires LiteLLM's `generic_api` callback at `POST /v1/integrations/litellm/spend` with a Velox API key as the Bearer token. Every LLM call lands in Velox as usage events automatically — no glue code in the operator's stack.

### Push vs pull

- **Push** (chosen): LiteLLM POSTs each call to a Velox URL via the generic_api callback. Standard LiteLLM pattern; works for every integration in their ecosystem.
- **Pull**: Velox polls LiteLLM's spend log API. Rejected — adds a worker, doubles the latency, and LiteLLM doesn't have a query API designed for incremental pull (the log is a sink, not a queryable store).

### Token mapping convention

> **Superseded by ADR-044 (2026-06-01).** The original two-meter shape
> (`tokens_input` / `tokens_output`) is replaced by the canonical single
> `tokens` meter with token role carried on the `token_type` dimension. The
> table below reflects the current (ADR-044) mapping.

Each LiteLLM call maps to up to two Velox usage events on the single `tokens` meter:

| LiteLLM field         | Velox event 1 (input)                          | Velox event 2 (output)                          |
|-----------------------|------------------------------------------------|-------------------------------------------------|
| `usage.prompt_tokens` | `meter_key=tokens`, `token_type=input`, qty=N  | —                                               |
| `usage.completion_tokens` | —                                          | `meter_key=tokens`, `token_type=output`, qty=N  |
| `model`               | `dimensions.model`                             | `dimensions.model`                              |
| `custom_llm_provider` | `dimensions.provider`                          | `dimensions.provider`                           |
| `metadata.team_id`    | `dimensions.team_id` (if present)              | `dimensions.team_id` (if present)               |
| `metadata.request_tags` | `dimensions.request_tags`                    | `dimensions.request_tags`                       |
| `user`                | (resolves to `external_customer_id`)           | (same)                                          |
| `id`                  | `idempotency_key = id + ":input"`              | `idempotency_key = id + ":output"`              |

Cache roles (`cache_read`, `cache_write_5m`, `cache_write_1h`) are a fast-follow once the payload parser reads `prompt_tokens_details.cached_tokens` / Anthropic `cache_creation` and normalizes them to additive-disjoint quantities (ADR-044).

### Customer resolution

LiteLLM's `user` field is the partner's caller identity. We require operators to set `user=<Velox external_customer_id>` on their LiteLLM calls; missing → 422 per row (skipped, not full-batch failure). This is the only operator-facing contract — every other field flows from LiteLLM's payload as-is.

### Meter creation

Operators must create the single `tokens` meter in Velox before LiteLLM events start arriving (ADR-044). Recommended path: instantiate the `anthropic_style` or `openai_style` recipe (both create the `tokens` meter + the per-`{model, token_type}` pricing rules). Missing meter → 422 per row.

Velox does NOT auto-create meters from LiteLLM payloads. Reasons:
- Aggregation choice (sum vs max vs last_during_period) is a pricing decision the operator owns
- Pricing rules / rating need to be configured before billing — silent auto-creation produces zero-priced meters that look correct but bill nothing
- Recipe path makes the "do this once" step explicit and reversible

### Cost figures

LiteLLM's `response_cost` and `cost_breakdown` are stored on each event's `metadata` under the `velox.*` namespace (audit-only). The billable amount is computed by Velox's rating rules — same as every other usage event. This separation lets operators reconcile against LiteLLM's cost calc without that calc driving billing.

### Partial-failure semantics

The handler returns 200 with `{accepted, skipped, errors[]}` even when some rows fail. Reason: per-row failures (missing user, missing meter) must not fail the batch; monitor `errors[]`.

> **Correction (2026-07-06, HA-readiness audit):** this ADR originally
> claimed "LiteLLM retries the entire batch on 5xx." Verified against
> LiteLLM source: at stock config the generic logger has `max_retries=0`
> and clears its queue on ANY send error — **LiteLLM retries nothing by
> default**, and a 5xx (or an outage) drops the batch client-side.
> Operators can configure `max_retries`/`retry_delay` to shrink (never
> close) the window. Recovery is operator replay of LiteLLM's spend
> logs — deterministic idempotency keys make it a pure gap-fill, with a
> hard deadline at period finalize. Full analysis:
> `docs/dev/ha-readiness-2026-07-06.md` ("Downtime = money?").

> **Amendment (2026-08-30): a store failure is a 503, not a 200 with
> `errors[]`.** The status code answers one question — did Velox reach a
> verdict on the batch? Per-row verdicts (unmapped customer or meter,
> validation failure, duplicate) stay 200 + `errors[]`/`deduplicated`. A
> non-verdict — the usage store unreachable or unable to commit (managed-
> Postgres failover, a replica losing its pool mid-rolling-restart) — aborts
> the batch fail-fast with `503 api_error/ingest_unavailable` so a
> retry-configured LiteLLM re-sends it. Previously the persist edge wrapped
> *every* lookup error, including a dead database, as "customer not found"
> and returned 200; LiteLLM treats any 2xx as delivered and clears its queue,
> so every completion in the outage window was silently lost. Whole-batch
> retry is safe by construction: rows are idempotency-keyed and the store
> dedups on `(tenant_id, livemode, idempotency_key)`. Pair it with
> `max_retries: 4` in the callback config (`docs/integrations/litellm.md`).

### What's NOT in scope (deferred)

- **Cost-table fallback**: when LiteLLM doesn't emit cost (custom model, older proxy version), Velox could fall back to a built-in cost table by model name. Deferred — most operators run a recent LiteLLM with cost tracking enabled; building a cost table is multi-day work for a niche case.
- **Multi-tenant routing via metadata**: operators with multiple Velox tenants behind one LiteLLM proxy could route by `metadata.tenant_id`. Deferred — the API-key auth already pins the tenant; the multi-tenant LiteLLM-proxy case isn't a real DP pattern yet.
- **Webhook signature verification**: LiteLLM's generic_api doesn't sign payloads. The Bearer API key is the auth boundary. If a DP needs HMAC, follow Stripe-webhook pattern — that's a separate ADR.

## Industry reference

- **LiteLLM `generic_api` callback** + **StandardLoggingPayload**: the canonical pattern every LiteLLM integration uses (Datadog, Langfuse, Helicone, internal company webhooks). Velox matches the same shape.
- **Helicone / Langfuse**: both consume LiteLLM payloads similarly, but for observability not billing. The token mapping ↔ pricing-rule dispatch is Velox's specific concern.
- **Lago / Orb / Metronome**: none ship a first-class LiteLLM adapter as of 2026-05-14. Velox is shipping ahead of the peer set — wedge bet that AI infra DPs want the integration before they want anything else.

## Consequences

**Positive:**
- Demo headline becomes "drop this LiteLLM callback config, get token tracking in Velox." Zero glue code on the partner side.
- Forward-compat: LiteLLM adds fields → we ignore them. We add new dimension promotions → only positive (existing partners unaffected).
- Allowlist sanitization on metadata: future LiteLLM fields don't accidentally land in pricing-rule dispatch.

**Negative:**
- Operator must configure LiteLLM to set `user=<external_customer_id>`. Missing → events drop into the partial-error bucket. Documentable but a real footgun.
- Single API key per LiteLLM proxy maps to one Velox tenant. Multi-tenant proxies are a future ADR.
- Cost figures aren't authoritative for billing (stored as audit only). Operator can be surprised that LiteLLM's "$0.018 for this call" doesn't equal the Velox invoice line. Documented in MANUAL_TEST.
