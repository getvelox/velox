package webhook

import (
	"context"
	"database/sql"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// EndpointStats holds delivery statistics for a single webhook endpoint.
type EndpointStats struct {
	EndpointID      string  `json:"endpoint_id"`
	TotalDeliveries int     `json:"total_deliveries"`
	Succeeded       int     `json:"succeeded"`
	Failed          int     `json:"failed"`
	SuccessRate     float64 `json:"success_rate"`
}

type Store interface {
	// Endpoints
	CreateEndpoint(ctx context.Context, tenantID string, ep domain.WebhookEndpoint) (domain.WebhookEndpoint, error)
	CreateEndpointTx(ctx context.Context, tx *sql.Tx, tenantID string, ep domain.WebhookEndpoint) (domain.WebhookEndpoint, error)
	GetEndpoint(ctx context.Context, tenantID, id string) (domain.WebhookEndpoint, error)
	// UpdateEndpoint mutates url/description/events/active without touching
	// the signing secret (the PATCH shape; delete+recreate was the only
	// mutation path before and it rotated the secret).
	UpdateEndpoint(ctx context.Context, tenantID string, ep domain.WebhookEndpoint) (domain.WebhookEndpoint, error)
	ListEndpoints(ctx context.Context, tenantID string) ([]domain.WebhookEndpoint, error)
	DeleteEndpoint(ctx context.Context, tenantID, id string) error
	// RotateEndpointSecret atomically moves the current signing secret into
	// the secondary slot with `gracePeriod` left on its clock, and replaces
	// the primary with `newSecret`. Runs under a single transaction with
	// FOR UPDATE so a concurrent rotate can't lose the old secret. If
	// gracePeriod is zero, no secondary is written (hard-replace).
	RotateEndpointSecret(ctx context.Context, tenantID, id, newSecret string, gracePeriod time.Duration) (domain.WebhookEndpoint, error)

	// Events
	CreateEvent(ctx context.Context, tenantID string, event domain.WebhookEvent) (domain.WebhookEvent, error)
	// CreateEventWithDeliveries creates the event and every fan-out
	// delivery row (born leased) in ONE tx; outboxRowID, when set, marks
	// the producing webhook_outbox row dispatched in the same tx (P5
	// handler-owns-mark — see PostgresStore for the crash analysis).
	CreateEventWithDeliveries(ctx context.Context, tenantID string, event domain.WebhookEvent, endpointIDs []string, birthLease time.Duration, outboxRowID string) (domain.WebhookEvent, []domain.WebhookDelivery, error)
	// CreateDeliveriesForEvent batch-creates born-leased delivery rows
	// for an EXISTING event (the replay path — the clone commits first).
	CreateDeliveriesForEvent(ctx context.Context, tenantID, eventID string, endpointIDs []string, birthLease time.Duration) ([]domain.WebhookDelivery, error)
	ListEvents(ctx context.Context, tenantID string, limit int) ([]domain.WebhookEvent, error)
	// EventDeliveryStatuses rolls each event's deliveries up to one status:
	// any pending → "pending", else any failed → "failed", else "delivered".
	// Events with no deliveries are absent from the map (no endpoint matched).
	EventDeliveryStatuses(ctx context.Context, tenantID string, eventIDs []string) (map[string]string, error)
	GetEvent(ctx context.Context, tenantID, id string) (domain.WebhookEvent, error)
	// CreateReplayEvent clones an existing event into a fresh event row
	// with replay_of_event_id pointing back at the original. The clone
	// goes through the normal Dispatch fan-out so each subscribed
	// endpoint produces its own delivery row — visible alongside the
	// original's deliveries on the dashboard timeline.
	CreateReplayEvent(ctx context.Context, tenantID, originalEventID string) (domain.WebhookEvent, error)

	// Deliveries
	CreateDelivery(ctx context.Context, tenantID string, d domain.WebhookDelivery) (domain.WebhookDelivery, error)
	// UpdateDelivery writes the delivery's outcome. expectedAttempts is the
	// attempt_count the caller derived d from: the row must still carry it, or
	// the write is refused with ErrStaleDeliveryMark and NOTHING is written.
	// Without it the count was a read-modify-write — two attempters that both
	// read N both wrote N+1, so one real POST vanished from the record and a
	// delivery could out-run maxRetries.
	UpdateDelivery(ctx context.Context, tenantID string, d domain.WebhookDelivery, expectedAttempts int) (domain.WebhookDelivery, error)
	ListDeliveries(ctx context.Context, tenantID, eventID string) ([]domain.WebhookDelivery, error)
	GetDelivery(ctx context.Context, tenantID, id string) (domain.WebhookDelivery, error)
	// ListDeliveriesByEndpoint is the endpoint drill-down query: every
	// delivery sent to ONE receiver, newest first, each row hydrated with
	// its event's type + replay pivot (the surface lists deliveries across
	// events, so rows must be self-describing).
	ListDeliveriesByEndpoint(ctx context.Context, tenantID, endpointID string, limit int) ([]domain.WebhookDelivery, error)

	// Stats
	GetEndpointStats(ctx context.Context, tenantID string) ([]EndpointStats, error)

	// Retry support (cross-tenant, system-level)
	ListPendingDeliveries(ctx context.Context, limit int) ([]domain.WebhookDelivery, error)
}
