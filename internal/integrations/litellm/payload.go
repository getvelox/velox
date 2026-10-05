// Package litellm adapts LiteLLM Proxy's StandardLoggingPayload to
// Velox usage events. LiteLLM is the de-facto AI-API gateway for
// self-hosted LLM stacks (Anthropic / OpenAI / Bedrock / etc.) — this
// adapter is the wedge integration: drop a `generic_api` callback in
// LiteLLM, get per-call token + cost tracking in Velox out of the
// box. See docs/integrations/litellm.md + ADR-033.
package litellm

// StandardLoggingPayload mirrors LiteLLM Proxy's per-call log shape:
// https://docs.litellm.ai/docs/proxy/logging_spec
//
// We deliberately accept the LiteLLM wire shape as-is rather than
// reshape it on the partner side. Operator just configures LiteLLM's
// generic_api callback at this endpoint with a Velox API key, and
// every completion lands in Velox without any glue code.
//
// Fields are a deliberate subset of LiteLLM's spec — only what the
// adapter actually consumes. LiteLLM's payload carries many more
// fields (prompts, responses, raw API params, etc.); accepting them
// in the wire shape but only using a subset keeps forward-compat
// when LiteLLM adds new fields.
type StandardLoggingPayload struct {
	// ID is LiteLLM's per-call identifier. Used as the idempotency
	// key prefix on every usage event derived from this payload.
	// Replays with the same id are silently deduped by the usage
	// store's UNIQUE (tenant_id, livemode, idempotency_key) — the
	// dedup scope is tenant-wide, so the mapper suffixes this id
	// with the token type to keep sibling events distinct.
	ID string `json:"id"`

	// CallType narrows what kind of LLM operation this was —
	// "completion", "embedding", "image_generation", etc. We only
	// emit usage events for token-bearing call types (completion,
	// chat, embedding). Other types are accepted but skipped.
	CallType string `json:"call_type"`

	// Model is the resolved model name LiteLLM dispatched to
	// (e.g. "claude-3-5-sonnet-20241022", "gpt-4o-2024-08-06").
	// Stamped as a dimension on every emitted usage event so the
	// operator can break cost down by model in the dashboard.
	Model string `json:"model"`

	// CustomLLMProvider is LiteLLM's inferred upstream provider
	// — "anthropic", "openai", "bedrock", "vertex_ai", etc.
	// Stamped as a dimension; lets operators slice spend by
	// provider for procurement / multi-cloud cost attribution.
	CustomLLMProvider string `json:"custom_llm_provider"`

	// User is LiteLLM's caller identity, conventionally the
	// end-user identifier the partner passes to litellm.completion
	// (`user="..."`). We treat this as the Velox external_customer_id
	// — operators set their LiteLLM call's user= to the customer's
	// Velox external_id. Missing → that row is rejected with a per-row
	// entry in the response's errors[] (the batch still answers 200);
	// the adapter refuses to bill "anonymous" usage to "unknown customer".
	User string `json:"user"`

	// Usage carries the token counts the adapter maps to two
	// usage events (input + output). Embedding calls carry only
	// prompt_tokens; the output event is skipped when 0.
	Usage *Usage `json:"usage,omitempty"`

	// ResponseCost is LiteLLM's pre-computed dollar cost for this
	// call (legacy single-field; cost_breakdown is the newer
	// per-component shape). NOTE (ADR-079 census correction): this is
	// accepted on the wire and stamped into the mapper's in-memory
	// metadata, but the ingest path DROPS it — usage_events has no
	// metadata column and IngestInput carries none, so it is never
	// persisted. It is also a WHOLE-CALL figure: a call maps to up to
	// three per-half events, so stamping it per event would multi-count
	// COGS. Velox's COGS comes from the operator's provider_cost_rates
	// table at ingest (ADR-079); per-half observed-cost stamping (from
	// CostBreakdown, never from this field) is the named fast-follow.
	ResponseCost float64 `json:"response_cost,omitempty"`

	// CostBreakdown is LiteLLM's per-component cost split. Same
	// audit-only role as ResponseCost — informs the operator,
	// doesn't drive billing.
	CostBreakdown *CostBreakdown `json:"cost_breakdown,omitempty"`

	// StartTime / EndTime are unix-seconds floats (LiteLLM's
	// convention). Used as the event timestamp; falls back to
	// time.Now if zero so a misconfigured proxy doesn't drop
	// the event entirely.
	StartTime float64 `json:"startTime,omitempty"`
	EndTime   float64 `json:"endTime,omitempty"`

	// Metadata is a free-form map LiteLLM passes through from the
	// caller. team_id and request_tags are promoted into event
	// DIMENSIONS (the mapper's dims block); the rest is currently
	// dropped at ingest — usage_events has no metadata column
	// (ADR-079 census; an earlier version of this comment claimed
	// otherwise).
	Metadata map[string]any `json:"metadata,omitempty"`
}

// Usage is LiteLLM's token-count subset (OpenAI-compatible shape).
//
// Prompt-cache fields (ADR-044 cache roles): LiteLLM normalizes every
// provider's cache accounting into this OpenAI-compatible shape. Two facts the
// mapper relies on, confirmed against LiteLLM's usage object:
//   - prompt_tokens INCLUDES cached (read) tokens, so the uncached input is
//     prompt_tokens − prompt_tokens_details.cached_tokens (additive-disjoint).
//   - cache_creation (write) tokens are NOT in prompt_tokens — they're a
//     separate quantity. LiteLLM does not yet expose the 5m-vs-1h cache-write
//     TTL split (BerriAI/litellm#15056), so the mapper cannot tell
//     cache_write_5m from cache_write_1h and defers cache-write billing (it
//     logs loudly when cache_creation tokens appear). cache_read is billable
//     now; cache_write is the documented follow-up.
type Usage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`

	// PromptTokensDetails carries the OpenAI-shape cache-read count. Present
	// for OpenAI and for Anthropic-via-LiteLLM (LiteLLM mirrors cache reads
	// here for cross-provider consistency).
	PromptTokensDetails *PromptTokensDetails `json:"prompt_tokens_details,omitempty"`

	// CacheReadInputTokens / CacheCreationInputTokens are Anthropic's top-level
	// cache fields, surfaced verbatim by LiteLLM. CacheReadInputTokens mirrors
	// PromptTokensDetails.CachedTokens (we take the max defensively).
	// CacheCreationInputTokens is the cache-WRITE count (deferred — see above).
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens,omitempty"`
}

// PromptTokensDetails is the OpenAI-compatible breakdown of prompt tokens.
type PromptTokensDetails struct {
	// CachedTokens is the subset of prompt_tokens served from cache (a cache
	// hit / read). prompt_tokens already includes these.
	CachedTokens int64 `json:"cached_tokens,omitempty"`
}

// cachedReadTokens returns the cache-read token count, taking the max of the
// OpenAI-shape (prompt_tokens_details.cached_tokens) and Anthropic-shape
// (cache_read_input_tokens) fields — LiteLLM may populate either depending on
// provider/version, and they represent the same quantity.
func (u *Usage) cachedReadTokens() int64 {
	read := u.CacheReadInputTokens
	if u.PromptTokensDetails != nil && u.PromptTokensDetails.CachedTokens > read {
		read = u.PromptTokensDetails.CachedTokens
	}
	if read < 0 {
		return 0
	}
	return read
}

// CostBreakdown is LiteLLM's per-component cost split (cents NOT
// supported upstream — dollars-as-float, per their wire). Velox
// stores the value verbatim on the usage event metadata; billing
// math runs through the operator's configured rating rules, not
// these floats.
type CostBreakdown struct {
	InputCost     float64 `json:"input_cost"`
	OutputCost    float64 `json:"output_cost"`
	ToolUsageCost float64 `json:"tool_usage_cost,omitempty"`
	TotalCost     float64 `json:"total_cost"`
}

// SpendRequest is the wire shape POSTed to /v1/integrations/litellm/spend.
// Accepts either a single payload (LiteLLM's default callback shape)
// or a batch (some self-hosted setups buffer + flush). The handler
// normalizes both into a slice before mapping.
type SpendRequest struct {
	// Single is set when LiteLLM POSTs one call per request.
	// Single and Batch are mutually exclusive — the handler picks
	// whichever is non-empty.
	Single *StandardLoggingPayload `json:",inline,omitempty"`
	// Batch is set when LiteLLM (or a downstream buffer) sends
	// multiple calls per request.
	Batch []StandardLoggingPayload `json:"events,omitempty"`
}

// IsTokenBearing reports whether this call type produces tokens
// Velox should map to usage events. Other types (image_generation,
// moderation, etc.) are accepted by the adapter but skipped without
// emitting events — they don't have a clean per-token billing model.
func (p *StandardLoggingPayload) IsTokenBearing() bool {
	switch p.CallType {
	case "completion", "acompletion", "chat", "chat.completion",
		"embedding", "aembedding",
		"text_completion", "atext_completion":
		return true
	default:
		return false
	}
}
