# LiteLLM → Velox integration

Drop a `"generic"` success callback in your LiteLLM proxy config (LiteLLM's generic-API logger) and every LLM call lands in Velox as a usage event. No glue code.

Design: [ADR-033](../adr/033-litellm-spend-adapter.md), superseded for the metering shape by [ADR-044](../adr/044-canonical-ai-token-metering-model.md) (one `tokens` meter + `token_type` dimension).

## 1. Create the `tokens` meter in Velox

The adapter writes to a single meter, `tokens`, carrying the token role on a `token_type` dimension (ADR-044). It must exist before LiteLLM starts POSTing.

Recommended: instantiate one of the AI-native recipes — each creates the `tokens` meter plus the per-`{model, token_type}` pricing rules.

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
```

The older env-var form (`success_callback: ["generic"]` +
`GENERIC_LOGGER_ENDPOINT`/`GENERIC_LOGGER_HEADERS`) still works on current
LiteLLM but is legacy. Either way, Velox accepts single, batched
(`json_array`), and `{"events": [...]}` payload shapes.

Point `<your-velox-host>` at your Velox API (local dev: `http://localhost:8080`). Use a **secret** key — publishable keys don't have `PermUsageWrite`.

## 3. Set `user=` on every call

The adapter resolves LiteLLM's `user` field to a Velox customer via `external_id`. Set it on every LiteLLM call:

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

Without `user=`, the adapter rejects the event with `payload.user is required`. The rest of the batch is accepted normally — the failure lands as a per-row entry in the 200 response's `errors[]`; monitor `errors[]`, not the HTTP status code.

## 4. What lands in Velox

For each completion call, the adapter emits **up to three** usage events — all on the single `tokens` meter, distinguished by the `token_type` dimension (ADR-044). Roles are additive-disjoint: `prompt_tokens` already includes cached reads, so the mapper splits them.

| `token_type` | Quantity                                         | Idempotency key             |
|--------------|--------------------------------------------------|-----------------------------|
| `input`      | `prompt_tokens − cached_tokens` (uncached input) | `<litellm_id>:input`        |
| `cache_read` | `prompt_tokens_details.cached_tokens` (if any)   | `<litellm_id>:cache_read`   |
| `output`     | `usage.completion_tokens`                        | `<litellm_id>:output`       |

Every event also carries dimensions `{model, model_raw, provider, team_id?, request_tags?}` (`request_tags` is LiteLLM's list, joined to a sorted comma-separated string — dimension values are scalars). `model` is the **canonical recipe family** (the mapper normalizes LiteLLM's raw string — e.g. `claude-sonnet-4-5-20250929` → `claude-sonnet-4.5`); `model_raw` preserves the verbatim string for audit. Each event's metadata carries the LiteLLM call ID, response cost (audit-only), and the call's original metadata under `litellm_metadata.*`.

Cache-**write** tokens (`cache_creation`) are seen but **not yet billed** — LiteLLM doesn't expose the 5m-vs-1h cache-write TTL split (BerriAI/litellm#15056), so the mapper logs them loudly and defers (ADR-044 follow-up).

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

`skipped` covers non-token-bearing calls (image generation, moderation) and zero-token failed completions. `errors[]` lists per-row reasons. The handler never returns 5xx once past decode: a malformed body is a 400, and everything else — including per-row persist failures during a DB outage — surfaces as `errors[]` entries in a 200 envelope (a full DB outage also tends to die earlier, at API-key auth, as a 401). Monitor `errors[]`, not the status code.

## Caveats

- **Cost figures**: LiteLLM's `response_cost` does NOT drive Velox billing — the billable amount comes from your pricing rules, and per-event COGS comes from your provider cost table (ADR-079: `PUT /v1/provider-costs`, stamped on each event at ingest as `provider_cost_micros`). LiteLLM's own per-call figure is whole-call (it spans up to three per-role events), so it is not stamped per event; per-half observed-cost stamping from `cost_breakdown` is a named follow-up.
- **`tokens` meter must exist**: missing meter → a per-row `errors[]` entry in the 200 response. Same path as missing `user` — monitor `errors[]`, not the status code.
- **Single tenant per API key**: each Velox API key pins to one tenant. Multi-tenant LiteLLM proxies route via separate API keys per tenant (not a metadata field on the call).
