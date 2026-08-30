# LiteLLM → Velox integration

Setup guide for metering LLM usage into Velox through LiteLLM, the open-source proxy that fronts many LLM providers behind one API.

Drop a `"generic"` success callback (LiteLLM's generic-API logger, which POSTs each completed call to an HTTP endpoint) into your LiteLLM proxy config and every LLM call lands in Velox as a usage event. No glue code.

Design history, recorded as architecture decision records (ADRs): [ADR-033](../adr/033-litellm-spend-adapter.md) is the original adapter design, superseded for the metering shape by [ADR-044](../adr/044-canonical-ai-token-metering-model.md) (one `tokens` meter + `token_type` dimension).

## 1. Create the `tokens` meter in Velox

The adapter writes to a single meter (a named usage counter Velox aggregates for billing) called `tokens`, carrying the token role on a `token_type` dimension (a key/value attribute on each event; ADR-044). The meter must exist before LiteLLM starts POSTing.

Recommended: instantiate one of the AI-native recipes (pre-built pricing templates) — each creates the `tokens` meter plus the per-`{model, token_type}` pricing rules.

```bash
curl -X POST "$VELOX/v1/recipes/anthropic_style/instantiate" \
  -H "Authorization: Bearer $VELOX_KEY" \
  -H "Content-Type: application/json" \
  -d '{}'
```

Test vs live mode follows the API key you use (`vlx_secret_test_…` vs `vlx_secret_live_…`) — there is no `livemode` request field.

Or create it by hand (you still need pricing rules per `{model, token_type}` to bill it):

```bash
curl -X POST "$VELOX/v1/meters" \
  -H "Authorization: Bearer $VELOX_KEY" \
  -H "Content-Type: application/json" \
  -d '{"key":"tokens","name":"Tokens","unit":"tokens","aggregation":"sum"}'
```

## 2. Configure the LiteLLM proxy

Add to your `litellm_config.yaml` (LiteLLM's `generic_api` callback — current
format as of LiteLLM's 2026-08 docs):

```yaml
litellm_settings:
  callbacks: ["velox"]

callback_settings:
  velox:
    callback_type: generic_api
    endpoint: "https://<your-velox-host>/v1/integrations/litellm/spend"
    headers:
      Authorization: Bearer vlx_secret_test_…
    # Retry on 5xx / transport errors. LiteLLM defaults to max_retries 0
    # and, via YAML, a 0s delay — it drops the batch on the first failed
    # send. Velox answers 503 when its usage store is unreachable (a
    # managed-Postgres failover, a replica mid-rolling-restart); 5+10+20+40s
    # of retries covers both. Replays are safe: every row is
    # idempotency-keyed, so an already-recorded row comes back as
    # `deduplicated`, never double-counted.
    max_retries: 4
    retry_delay: 5
    timeout: 10
```

The older env-var form (`success_callback: ["generic"]` +
`GENERIC_LOGGER_ENDPOINT`/`GENERIC_LOGGER_HEADERS`) still works on current
LiteLLM but is legacy. Either way, Velox accepts single, batched
(`json_array`), and `{"events": [...]}` payload shapes.

Point `<your-velox-host>` at your Velox API (local dev: `http://localhost:8080`). Use a **secret** key — publishable keys (the client-side-safe key type) don't have `PermUsageWrite`, the permission to write usage events.

## 3. Set `user=` on every call

The adapter resolves LiteLLM's `user` field to a Velox customer via `external_id` — your own customer identifier, stored on the Velox customer record. Set it on every LiteLLM call:

```python
import litellm

response = litellm.completion(
    model="claude-sonnet-4-5-20250929",
    messages=[{"role": "user", "content": "Hello"}],
    user="cus_acme_corp",   # ← Velox external_customer_id
    metadata={
        "team_id": "team_engineering",  # surfaces as a Velox dimension
    },
)
```

Without `user=`, the adapter rejects the event with `payload.user is required`. The rest of the batch is accepted normally; the failure lands as a per-row entry in the 200 response's `errors[]`.

## 4. What lands in Velox

For each completion call, the adapter emits **up to three** usage events — all on the single `tokens` meter, distinguished by the `token_type` dimension (ADR-044). The roles never count the same token twice (additive-disjoint): LiteLLM's `prompt_tokens` already includes cached reads, so the mapper splits them apart.

| `token_type` | Quantity                                         | Idempotency key             |
|--------------|--------------------------------------------------|-----------------------------|
| `input`      | `prompt_tokens − cached_tokens` (uncached input) | `<litellm_id>:input`        |
| `cache_read` | `prompt_tokens_details.cached_tokens` (if any)   | `<litellm_id>:cache_read`   |
| `output`     | `usage.completion_tokens`                        | `<litellm_id>:output`       |

The idempotency key deduplicates retries: a resend with the same key can't count twice.

Every event also carries dimensions `{model, model_raw, provider, team_id?, request_tags?}` (`request_tags` is LiteLLM's list, joined to a sorted comma-separated string — dimension values are scalars). `model` is the **canonical recipe family** — the normalized name pricing rules key on; the mapper normalizes LiteLLM's raw string, e.g. `claude-sonnet-4-5-20250929` → `claude-sonnet-4.5`. `model_raw` preserves the verbatim string for audit. Each event's metadata carries the LiteLLM call ID, the response cost (audit-only), and the call's original metadata under `litellm_metadata.*`.

Cache-**write** tokens (`cache_creation`) are seen but **not yet billed**. LiteLLM doesn't expose the 5m-vs-1h cache-write TTL split (BerriAI/litellm#15056), so the mapper logs them loudly and defers billing them (ADR-044 follow-up).

## 5. Verify

```bash
# Resolve the internal customer id from the external one, then tail events.
# (The usage-events list filters on the INTERNAL customer_id.)
CUST_ID=$(curl -s "$VELOX/v1/customers?external_id=cus_acme_corp" \
  -H "Authorization: Bearer $VELOX_KEY" | jq -r '.data[0].id')
curl "$VELOX/v1/usage-events?customer_id=$CUST_ID&limit=5" \
  -H "Authorization: Bearer $VELOX_KEY"
```

You should see one or more `tokens` events per LiteLLM call (up to three when prompt caching is used: `input`, `cache_read`, `output`), each with a `token_type` dimension plus `model` / `model_raw` / `provider`.

## Reference: response shape

`POST /v1/integrations/litellm/spend` returns 200 with:

```json
{
  "accepted": 12,
  "skipped": 1,
  "errors": [
    {
      "id": "litellm_call_xyz",
      "error": "customer \"cus_unknown\" not found (set user=<external_customer_id> on the LiteLLM call)"
    }
  ]
}
```

`skipped` covers non-token-bearing calls (image generation, moderation) and zero-token failed completions. `errors[]` lists per-row reasons — verdicts Velox reached on that row (an unmapped `user`, a missing meter, a payload that failed validation); they never make the batch fail, so monitor `errors[]`. The status code answers a different question — *could Velox record the batch at all?* A malformed body is 400. `503 authentication_unavailable` (API-key store unreachable) or `503 ingest_unavailable` (usage store unreachable or unable to commit) means the batch was **not** recorded and must be retried whole; rows that did commit before the abort replay as `deduplicated`. Configure `max_retries` as above so LiteLLM does the retrying.

## Caveats

- **Cost figures**: LiteLLM's `response_cost` does NOT drive Velox billing. The billable amount comes from your pricing rules; per-event COGS (cost of goods sold — what the provider charged you) comes from your provider cost table (ADR-079: `PUT /v1/provider-costs`, stamped on each event at ingest as `provider_cost_micros`). LiteLLM's own per-call figure is whole-call — one total spanning up to three per-role events — so it is not stamped per event; stamping observed cost onto each per-role event (per-half) from `cost_breakdown` is a named follow-up.
- **`tokens` meter must exist**: a missing meter lands as a per-row `errors[]` entry in the 200 response — the same path as a missing `user`.
- **Single tenant per API key**: each Velox API key pins to one tenant (one isolated billing account). Multi-tenant LiteLLM proxies route via separate API keys per tenant (not a metadata field on the call).
