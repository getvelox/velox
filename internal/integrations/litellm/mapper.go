package litellm

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/usage"
)

// MeterKeyTokens is the single canonical meter every provider mapper writes
// to (ADR-044). Token ROLE rides on the `token_type` dimension, NOT on the
// meter key — so the anthropic_style / openai_style recipes (and the cost
// dashboard) see one `tokens` meter grouped by {model, token_type}, and a new
// provider never re-opens a meter-shape mismatch.
const MeterKeyTokens = "tokens"

// Canonical token-role values for the `token_type` dimension (ADR-044). The
// vocabulary is shared by the recipes and every provider mapper. LiteLLM
// currently exposes only input/output; the cache roles (cache_read,
// cache_write_5m, cache_write_1h) are a fast-follow once the payload parser
// reads prompt_tokens_details / cache_creation and normalizes them to
// additive-disjoint quantities.
const (
	TokenTypeInput     = "input"
	TokenTypeOutput    = "output"
	TokenTypeCacheRead = "cache_read"
)

// modelFamilies maps a normalized model id (lower-cased, provider-prefix
// stripped) to the canonical FAMILY token that the pricing recipes key their
// rules on (ADR-044: model normalization lives in the provider mapper, never
// the matcher). LiteLLM forwards the model string verbatim — a dated snapshot
// (claude-3-5-sonnet-20241022), a provider-prefixed string (azure/gpt-4o-eu),
// or a friendly alias — and the matcher does exact `properties @> dimension_match`
// (the industry-standard primitive: Orb/Lago/OpenMeter/Metronome all match
// exact strings, none family-match). So we canonicalize HERE: longest-prefix
// match the normalized id to a family, then emit the family on the `model`
// dimension (and the raw string on `model_raw` for audit).
//
// Entries are detection-prefix → recipe-token. The detection prefix is in real
// (dashed) API form; the recipe token is whatever the recipe rule uses (note
// anthropic_style keys "claude-3.5-sonnet" with a DOT). Keep in sync with the
// anthropic_style / openai_style recipe `dimension_match.model` values.
var modelFamilies = []struct{ prefix, recipeToken string }{
	// Current Anthropic lineup (5 generation + the Opus 4.x line; dated ids
	// like claude-opus-4-8-20260301 match via the prefix+"-" rule, and
	// longest-prefix keeps claude-opus-4-8 ahead of claude-opus-4).
	{"claude-fable-5", "claude-fable-5"},
	{"claude-mythos-5", "claude-mythos-5"},
	{"claude-opus-5", "claude-opus-5"},
	{"claude-opus-4-8", "claude-opus-4.8"},
	{"claude-opus-4-7", "claude-opus-4.7"},
	{"claude-opus-4-6", "claude-opus-4.6"},
	{"claude-opus-4-5", "claude-opus-4.5"},
	{"claude-sonnet-5", "claude-sonnet-5"},
	{"claude-sonnet-4-6", "claude-sonnet-4.6"},
	{"claude-sonnet-4-5", "claude-sonnet-4.5"},
	{"claude-haiku-4-5", "claude-haiku-4.5"},
	// Legacy Anthropic families — proxies mid-migration still route
	// them; removing a detection prefix silently un-families live
	// traffic (the model dim falls back to the raw id and no recipe
	// rule matches). claude-3-7-sonnet is deliberately ABSENT: retired
	// 2026-02-19 with no published price, so a recipe rule for it would
	// carry an invented rate — its traffic keeps the raw id and surfaces
	// as unmatched usage on the operator card instead (post-#551 loud).
	{"claude-opus-4-1", "claude-opus-4.1"},
	{"claude-opus-4", "claude-opus-4"},
	{"claude-sonnet-4", "claude-sonnet-4"},
	{"claude-3-5-sonnet", "claude-3.5-sonnet"},
	{"claude-3-5-haiku", "claude-3.5-haiku"},
	{"claude-3-opus", "claude-3-opus"},
	{"claude-3-sonnet", "claude-3-sonnet"},
	{"claude-3-haiku", "claude-3-haiku"},
	// Current OpenAI lineup. The match rule is equality-or-prefix+"-",
	// so the DOTTED ids (gpt-5.6, gpt-4.1) never collide with the bare
	// gpt-5 entry and need their own rows; -mini/-nano/-pro and the 5.6
	// tier names still win over their base family via longest-prefix.
	{"gpt-5.6-sol", "gpt-5.6-sol"},
	{"gpt-5.6-terra", "gpt-5.6-terra"},
	{"gpt-5.6-luna", "gpt-5.6-luna"},
	{"gpt-5.5-pro", "gpt-5.5-pro"},
	{"gpt-5.5", "gpt-5.5"},
	{"gpt-5.4-mini", "gpt-5.4-mini"},
	{"gpt-5.4-nano", "gpt-5.4-nano"},
	{"gpt-5.4-pro", "gpt-5.4-pro"},
	{"gpt-5.4", "gpt-5.4"},
	{"gpt-5.3-codex", "gpt-5.3-codex"},
	// o-series reasoning models (legacy; per-model page prices).
	{"o1-mini", "o1-mini"},
	{"o1-pro", "o1-pro"},
	{"o1", "o1"},
	{"o3-mini", "o3-mini"},
	{"o3-pro", "o3-pro"},
	{"o3", "o3"},
	{"o4-mini", "o4-mini"},
	{"gpt-5-mini", "gpt-5-mini"},
	{"gpt-5-nano", "gpt-5-nano"},
	{"gpt-5.1", "gpt-5.1"},
	{"gpt-5", "gpt-5"},
	{"gpt-4.1-mini", "gpt-4.1-mini"},
	{"gpt-4.1-nano", "gpt-4.1-nano"},
	{"gpt-4.1", "gpt-4.1"},
	// Legacy OpenAI families.
	{"gpt-4o-mini", "gpt-4o-mini"}, // before gpt-4o (longest-prefix wins)
	{"gpt-4o", "gpt-4o"},
	{"gpt-4-turbo", "gpt-4-turbo"},
	{"gpt-3.5-turbo", "gpt-3.5-turbo"},
	{"text-embedding-3-small", "text-embedding-3-small"},
	{"text-embedding-3-large", "text-embedding-3-large"},
	{"text-embedding-ada-002", "text-embedding-ada-002"},
}

// providerPrefixes are stripped before family matching — LiteLLM prepends the
// routing provider to the model string for non-OpenAI deployments.
var providerPrefixes = []string{"azure/", "bedrock/", "vertex_ai/", "openai/", "anthropic/", "anthropic.", "us.", "eu.", "apac."}

// canonicalModel resolves a raw LiteLLM model string to its canonical recipe
// family token, or "" if no known family matches (caller keeps the raw string
// and logs loudly — we never invent a family, which would mis-price). Match is
// longest-prefix so gpt-4o-mini-* beats gpt-4o-*.
func canonicalModel(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	for _, p := range providerPrefixes {
		s = strings.TrimPrefix(s, p)
	}
	best := ""
	bestToken := ""
	for _, f := range modelFamilies {
		if (s == f.prefix || strings.HasPrefix(s, f.prefix+"-")) && len(f.prefix) > len(best) {
			best = f.prefix
			bestToken = f.recipeToken
		}
	}
	return bestToken
}

// MapPayload converts a single LiteLLM StandardLoggingPayload into
// zero, one, or two Velox IngestInput events.
//
// Mapping (per ADR-044, superseding ADR-033's two-meter shape):
//
//	prompt_tokens     → {meter_key: tokens, token_type: input,  quantity: N}
//	completion_tokens → {meter_key: tokens, token_type: output, quantity: N}
//
// Skipped when:
//   - call_type isn't token-bearing (image_gen, moderation, etc.)
//   - Usage is nil
//   - both token counts are zero (empty response, error, etc.)
//
// Returns ErrMissingUser when payload.user is empty — the adapter
// refuses to bill anonymous usage to an unknown customer. Operator
// must set `user="<external_customer_id>"` on their LiteLLM calls.
//
// IngestInput.CustomerID carries the EXTERNAL id (payload.User); the
// handler resolves it to the internal customer_id via the existing
// usage Resolver path (same flow as POST /v1/usage-events).
func MapPayload(p StandardLoggingPayload) ([]ExternalIngest, error) {
	if !p.IsTokenBearing() {
		return nil, nil
	}
	if strings.TrimSpace(p.User) == "" {
		return nil, ErrMissingUser
	}
	if p.Usage == nil {
		return nil, nil
	}

	// Event timestamp: prefer EndTime (when the call actually completed), fall
	// back to StartTime. LiteLLM emits floats in unix-seconds; convert to
	// time.Time without loss for sub-second resolution.
	//
	// When the payload carries NEITHER (a synthetic or test-clock ingest that
	// omits both), leave the timestamp NIL rather than manufacturing
	// time.Now(): usage.Service then supplies the customer's effective-now —
	// the frozen_time of a test-clock-pinned customer, wall-clock otherwise —
	// exactly like the direct /v1/usage-events door (ADR-030, "no timestamp =>
	// frozen_time"). A manufactured time.Now() here is non-nil, so the service
	// skips the clock resolution and silently lands simulated usage at
	// wall-clock, defeating the test clock (the flagship advance-clock →
	// LiteLLM-ingest → billed demo depended on this).
	var ts *time.Time
	switch {
	case p.EndTime > 0:
		t := unixSecondsToTime(p.EndTime)
		ts = &t
	case p.StartTime > 0:
		t := unixSecondsToTime(p.StartTime)
		ts = &t
	}

	// Canonical model family on `model` (what recipe rules match), raw string
	// preserved on `model_raw` for audit/reconciliation (ADR-044 model
	// normalization in the mapper). An unrecognized model keeps its raw string
	// on `model` and is logged loudly — we never invent a family, which would
	// mis-price; it simply won't match a recipe rule (rates $0, loud by absence).
	modelDim := canonicalModel(p.Model)
	if modelDim == "" {
		modelDim = p.Model
		slog.Warn("litellm: unrecognized model — usage won't match recipe pricing rules until an alias is added (see modelFamilies)",
			"model", p.Model, "litellm_id", p.ID)
	}
	dims := map[string]any{
		"model":     modelDim,
		"model_raw": p.Model,
		"provider":  p.CustomLLMProvider,
	}
	// Forward useful operator-set metadata into dimensions for
	// pricing-rule dispatch. We promote `team_id` and `tags`
	// specifically because LiteLLM's spend page uses them as the
	// primary slice axes; everything else stays in the event's
	// metadata column for audit.
	if p.Metadata != nil {
		if v, ok := p.Metadata["team_id"]; ok {
			dims["team_id"] = v
		}
		// request_tags is list-typed on LiteLLM's wire ([]any after JSON
		// decode), but usage ingest accepts scalar dimension values only
		// (validateDimensions) — passing the array through rejected EVERY
		// token event on tagged calls, silently unbilling that traffic
		// (front-door audit 2026-07-05). Join to a stable comma-separated
		// string; scalars pass through unchanged.
		if v, ok := p.Metadata["request_tags"]; ok {
			if tags, isList := v.([]any); isList {
				parts := make([]string, 0, len(tags))
				for _, tag := range tags {
					parts = append(parts, fmt.Sprintf("%v", tag))
				}
				sort.Strings(parts)
				dims["request_tags"] = strings.Join(parts, ",")
			} else {
				dims["request_tags"] = v
			}
		}
	}

	out := make([]ExternalIngest, 0, 3)

	// One event per present role on the single `tokens` meter, role carried as
	// the `token_type` dimension (ADR-044). Roles are ADDITIVE-DISJOINT:
	// prompt_tokens INCLUDES cache-read tokens, so uncached input is the
	// remainder. The mapper does this split (the engine never does role math).
	// Dimensions are cloned per role so each event gets its own token_type.
	cacheRead := p.Usage.cachedReadTokens()
	uncachedInput := p.Usage.PromptTokens - cacheRead
	if uncachedInput < 0 {
		// Malformed payload (cached > prompt). Don't emit a negative quantity;
		// fall back to billing the whole prompt as uncached input.
		uncachedInput = p.Usage.PromptTokens
		cacheRead = 0
	}

	if uncachedInput > 0 {
		out = append(out, ExternalIngest{
			ExternalCustomerID: p.User,
			MeterKey:           MeterKeyTokens,
			Quantity:           decimal.NewFromInt(uncachedInput),
			Dimensions:         dimsWithTokenType(dims, TokenTypeInput),
			IdempotencyKey:     p.ID + ":" + TokenTypeInput,
			Timestamp:          ts,
			Metadata:           buildEventMetadata(p, TokenTypeInput),
		})
	}
	if cacheRead > 0 {
		out = append(out, ExternalIngest{
			ExternalCustomerID: p.User,
			MeterKey:           MeterKeyTokens,
			Quantity:           decimal.NewFromInt(cacheRead),
			Dimensions:         dimsWithTokenType(dims, TokenTypeCacheRead),
			IdempotencyKey:     p.ID + ":" + TokenTypeCacheRead,
			Timestamp:          ts,
			Metadata:           buildEventMetadata(p, TokenTypeCacheRead),
		})
	}
	if p.Usage.CompletionTokens > 0 {
		out = append(out, ExternalIngest{
			ExternalCustomerID: p.User,
			MeterKey:           MeterKeyTokens,
			Quantity:           decimal.NewFromInt(p.Usage.CompletionTokens),
			Dimensions:         dimsWithTokenType(dims, TokenTypeOutput),
			IdempotencyKey:     p.ID + ":" + TokenTypeOutput,
			Timestamp:          ts,
			Metadata:           buildEventMetadata(p, TokenTypeOutput),
		})
	}

	// Cache-WRITE tokens (cache_creation) are billable usage LiteLLM exposes,
	// but it does NOT expose the 5m-vs-1h TTL split the recipes price at
	// different rates (BerriAI/litellm#15056). Assigning a cache_write_* role
	// would risk mis-pricing a money path, so cache-write billing is deferred
	// (ADR-044 follow-up). Log loudly rather than silently drop or guess
	// (no-silent-fallbacks) so the unbilled usage is visible.
	if p.Usage.CacheCreationInputTokens > 0 {
		slog.Warn("litellm: cache-write tokens seen but not billed — LiteLLM does not expose the 5m/1h cache-write TTL split (BerriAI/litellm#15056); cache-write billing deferred (ADR-044)",
			"cache_creation_tokens", p.Usage.CacheCreationInputTokens,
			"model", modelDim, "litellm_id", p.ID)
	}

	return out, nil
}

// dimsWithTokenType returns a copy of base with token_type set, so the two
// per-role events don't share (and overwrite) one dimensions map.
func dimsWithTokenType(base map[string]any, tokenType string) map[string]any {
	out := make(map[string]any, len(base)+1)
	for k, v := range base {
		out[k] = v
	}
	out["token_type"] = tokenType
	return out
}

// ErrMissingUser is returned by MapPayload when the LiteLLM payload
// has no `user` field set. Operators must configure their LiteLLM
// callers to pass user=<external_customer_id>; without it the
// adapter has no customer to attribute the spend to.
var ErrMissingUser = fmt.Errorf("litellm: payload.user is required (set user=<external_customer_id> on the litellm call)")

// ExternalIngest is the adapter's intermediate shape — uses the
// EXTERNAL customer id and meter key. The handler resolves these to
// internal IDs via the existing usage ingest path. Mirrors the
// existing usage.IngestRequest body shape so the resolver is shared.
type ExternalIngest struct {
	ExternalCustomerID string          `json:"external_customer_id"`
	MeterKey           string          `json:"meter_key"`
	Quantity           decimal.Decimal `json:"quantity"`
	Dimensions         map[string]any  `json:"dimensions,omitempty"`
	IdempotencyKey     string          `json:"idempotency_key,omitempty"`
	Timestamp          *time.Time      `json:"timestamp,omitempty"`
	Metadata           map[string]any  `json:"metadata,omitempty"`
}

// buildEventMetadata composes the per-event metadata blob: LiteLLM's
// own metadata + a velox.* namespace for adapter-injected context
// (cost figures, source identifier). Operator can query usage events
// by metadata in the audit dashboard.
func buildEventMetadata(p StandardLoggingPayload, half string) map[string]any {
	out := map[string]any{
		"velox.source":     "litellm",
		"velox.litellm_id": p.ID,
		"velox.call_type":  p.CallType,
		"velox.token_half": half, // "input" | "output"
	}
	if p.ResponseCost > 0 {
		out["velox.litellm_response_cost_usd"] = p.ResponseCost
	}
	if p.CostBreakdown != nil {
		// Surface the relevant half's cost. LiteLLM reports dollars-
		// as-float; we store verbatim so cost reconciliation against
		// the provider invoice is trivial.
		switch half {
		case "input":
			if p.CostBreakdown.InputCost > 0 {
				out["velox.litellm_cost_usd"] = p.CostBreakdown.InputCost
			}
		case "output":
			if p.CostBreakdown.OutputCost > 0 {
				out["velox.litellm_cost_usd"] = p.CostBreakdown.OutputCost
			}
		}
	}
	// Forward LiteLLM's own metadata under a namespaced key so it
	// doesn't collide with our velox.* keys. Operators can still
	// query by `metadata->litellm_metadata->>team_id` etc.
	if len(p.Metadata) > 0 {
		out["litellm_metadata"] = p.Metadata
	}
	return out
}

func unixSecondsToTime(secs float64) time.Time {
	whole := int64(secs)
	nanos := int64((secs - float64(whole)) * 1e9)
	return time.Unix(whole, nanos).UTC()
}

// _ ensures the usage package import stays — the handler uses
// usage.IngestInput (resolved form) downstream.
var _ = usage.IngestInput{}
