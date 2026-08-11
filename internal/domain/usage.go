package domain

import (
	"time"

	"github.com/shopspring/decimal"
)

// UsageEventOrigin is the ingest path that produced a usage_events row.
// 'api' covers real-time POST /usage-events (and /batch); 'backfill' covers
// the explicit historical-ingest endpoint. Extend as new sources appear.
type UsageEventOrigin string

const (
	UsageOriginAPI      UsageEventOrigin = "api"
	UsageOriginBackfill UsageEventOrigin = "backfill"
)

// UsageEvent.Quantity is a NUMERIC(38, 12) decimal so AI usage primitives
// (GPU-hours, cached-token ratios, partial KV-cache reads) round-trip
// exactly. Maps to Stripe's quantity_decimal. JSON wire format is a string
// ("0.50") on the response side; the unmarshaller accepts both number (5)
// and string ("5.5") on the request side.
type UsageEvent struct {
	ID             string           `json:"id"`
	TenantID       string           `json:"tenant_id,omitempty"`
	CustomerID     string           `json:"customer_id"`
	MeterID        string           `json:"meter_id"`
	Quantity       decimal.Decimal  `json:"quantity"`
	Dimensions     map[string]any   `json:"dimensions,omitempty"`
	IdempotencyKey string           `json:"idempotency_key,omitempty"`
	Timestamp      time.Time        `json:"timestamp"`
	Origin         UsageEventOrigin `json:"origin,omitempty"`

	// ProviderCostMicros is the COGS stamp (ADR-079): what serving this
	// event cost the OPERATOR (their provider bill), in micro-dollars
	// (1e-6), resolved from provider_cost_rates at INGEST time — snapshot
	// semantics; later rate edits never rewrite it. Nil = unresolved
	// (costable dims but no matching rate — the actionable "add a rate"
	// signal) or pre-feature row. Operator-facing only.
	ProviderCostMicros *int64 `json:"provider_cost_micros,omitempty"`
	// ProviderCostSource: 'table' (inferred from provider_cost_rates),
	// 'not_applicable' (no costable dims — non-token meters), 'observed'
	// (the sender's own per-half cost, ADR-079 D4 — wins over inference;
	// stamped by usage/postgres.go when ObservedCostMicros is set).
	// Empty = unresolved/pre-feature.
	ProviderCostSource string `json:"provider_cost_source,omitempty"`
}

type BilledEntrySource string

const (
	BilledSourceAPI    BilledEntrySource = "api"
	BilledSourceReplay BilledEntrySource = "replay_adjustment"
)

type BilledEntry struct {
	ID             string            `json:"id"`
	TenantID       string            `json:"tenant_id,omitempty"`
	CustomerID     string            `json:"customer_id"`
	MeterID        string            `json:"meter_id"`
	AmountCents    int64             `json:"amount_cents"`
	IdempotencyKey string            `json:"idempotency_key,omitempty"`
	Source         BilledEntrySource `json:"source,omitempty"`
	Timestamp      time.Time         `json:"timestamp"`
}

// ProviderCostRate is one row of the operator's provider cost table
// (ADR-079): what THEY pay a provider per token for (provider, model,
// token_type), in decimal dollars — verified real-world rates reach
// 1.5e-06/token, so never floats, never cents. CURRENT-rate semantics:
// one row per key per mode, edited in place; the per-event stamp is the
// historical snapshot, so edits never rewrite history.
type ProviderCostRate struct {
	ID           string          `json:"id"`
	TenantID     string          `json:"tenant_id,omitempty"`
	Provider     string          `json:"provider"`
	Model        string          `json:"model"`
	TokenType    string          `json:"token_type"`
	CostPerToken decimal.Decimal `json:"cost_per_token"`
	Currency     string          `json:"currency"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
}
