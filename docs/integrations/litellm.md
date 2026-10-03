# LiteLLM → Velox integration

This guide shows how to meter LLM usage into Velox through LiteLLM, the open-source proxy that fronts many LLM providers behind one API.

Add a success callback to your LiteLLM proxy config, as step 2 shows. It uses LiteLLM's generic-API logger, which POSTs each completed call to an HTTP endpoint. Every LLM call then lands in Velox as a [usage event](../README.md#glossary). You write no glue code.

## 1. Create the `tokens` meter in Velox

The adapter writes to a single meter called `tokens`. A meter is a named usage counter that Velox aggregates for billing. Each event carries its token role on a `token_type` dimension, a key/value attribute on the event. The meter must exist before LiteLLM starts POSTing.

The recommended way is to instantiate one of the AI-native recipes (pre-built pricing templates). Each recipe creates the `tokens` meter plus the per-`{model, token_type}` pricing rules.

```bash
curl -X POST "$VELOX/v1/recipes/anthropic_style/instantiate" \
  -H "Authorization: Bearer $VELOX_KEY" \
  -H "Content-Type: application/json" \
  -d '{}'
```

The API key you use picks test or live mode (`vlx_secret_test_…` or `vlx_secret_live_…`). There is no `livemode` request field.

You can also create the meter by hand. To bill it, you then still need pricing rules per `{model, token_type}`:

```bash
curl -X POST "$VELOX/v1/meters" \
  -H "Authorization: Bearer $VELOX_KEY" \
  -H "Content-Type: application/json" \
  -d '{"key":"tokens","name":"Tokens","unit":"tokens","aggregation":"sum"}'
```

## 2. Configure the LiteLLM proxy

Add the following to your `litellm_config.yaml`. It uses LiteLLM's `generic_api` callback, in the
current format as of LiteLLM's 2026-08 docs:

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
LiteLLM, but it is legacy. With either form, Velox accepts three payload shapes: single, batched
(`json_array`), and `{"events": [...]}`.

Point `<your-velox-host>` at your Velox API (local dev: `http://localhost:8080`). Use a **secret** key. Publishable keys (the client-side-safe key type) don't have `PermUsageWrite`, the permission to write usage events. See [API key types](../README.md#glossary) in the glossary.

## 3. Set `user=` on every call

The adapter matches LiteLLM's `user` field to the Velox customer whose `external_id` equals it. The code comment below calls the same field `external_customer_id`. It is your own customer identifier, stored on the Velox customer record. Set it on every LiteLLM call:

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

Without `user=`, the adapter rejects the event with `payload.user is required`. The rest of the batch is accepted normally. The failure lands as a per-row entry in the 200 response's `errors[]`.

## 4. What lands in Velox

For each completion call, the adapter emits **up to three** usage events. All of them go to the single `tokens` meter, and the `token_type` dimension tells them apart. The roles never count the same token twice (additive-disjoint). LiteLLM's `prompt_tokens` already includes cached reads, so the mapper splits them apart.

| `token_type` | Quantity                                         | Idempotency key             |
|--------------|--------------------------------------------------|-----------------------------|
| `input`      | `prompt_tokens − cached_tokens` (uncached input) | `<litellm_id>:input`        |
| `cache_read` | `prompt_tokens_details.cached_tokens` (if any)   | `<litellm_id>:cache_read`   |
| `output`     | `usage.completion_tokens`                        | `<litellm_id>:output`       |

The [idempotency key](../README.md#glossary) deduplicates retries: a resend with the same key can't count twice.

Every event also carries these dimensions:

- `{model, model_raw, provider, team_id?, request_tags?}`.
- `request_tags` is LiteLLM's list, joined to a sorted comma-separated string, because dimension values are scalars.
- `model` is the **canonical recipe family**, the normalized name that pricing rules key on. The mapper normalizes LiteLLM's raw string, for example `claude-sonnet-4-5-20250929` → `claude-sonnet-4.5`.
- `model_raw` preserves the verbatim string for audit.

Each event's metadata carries the LiteLLM call ID, the response cost (audit-only), and the call's original metadata under `litellm_metadata.*`.

Cache-**write** tokens (`cache_creation`) are seen but **not yet billed**. LiteLLM doesn't expose the 5m-vs-1h cache-write TTL split (BerriAI/litellm#15056). So the mapper logs a warning for them and defers billing them, as a follow-up to ADR-044.

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

`skipped` covers non-token-bearing calls (image generation, moderation) and zero-token failed completions.

`errors[]` lists per-row reasons. Each is a verdict Velox reached on that row: an unmapped `user`, a missing meter, or a payload that failed validation. These never make the batch fail, so monitor `errors[]`.

The status code answers a different question: could Velox record the batch at all?

| Status | Meaning | What to do |
|---|---|---|
| 400 | Malformed body. | — |
| `503 authentication_unavailable` | The API-key store is unreachable. The batch was **not** recorded. | Retry the whole batch. |
| `503 ingest_unavailable` | The usage store is unreachable or unable to commit. The batch was **not** recorded. | Retry the whole batch. |

On a retry, rows that did commit before the abort replay as `deduplicated`. Configure `max_retries` as in step 2, so LiteLLM does the retrying.

## Caveats

- **Cost figures**: LiteLLM's `response_cost` does not drive Velox billing.
  - The billable amount comes from your pricing rules.
  - Per-event COGS (cost of goods sold: what the provider charged you) comes from your provider [cost table](../README.md#glossary). See ADR-079: `PUT /v1/provider-costs`, stamped on each event at ingest as `provider_cost_micros`.
  - LiteLLM's own per-call figure is whole-call: one total that spans up to three per-role events. So Velox does not stamp it per event.
  - A named follow-up would stamp observed cost onto each per-role event (per-half) from `cost_breakdown`.
- **`tokens` meter must exist**: a missing meter lands as a per-row `errors[]` entry in the 200 response. This is the same path as a missing `user`.
- **Single tenant per API key**: each Velox API key pins to one [tenant](../README.md#glossary) (one isolated billing account). Multi-tenant LiteLLM proxies route through a separate API key per tenant, not through a metadata field on the call.

## Design history

These decisions are recorded as architecture decision records (ADRs). [ADR-033](../adr/033-litellm-spend-adapter.md) is the original adapter design. [ADR-044](../adr/044-canonical-ai-token-metering-model.md) superseded it for the metering shape: one `tokens` meter + `token_type` dimension.
