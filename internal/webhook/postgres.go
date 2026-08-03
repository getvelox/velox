package webhook

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/crypto"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

type PostgresStore struct {
	db  *postgres.DB
	enc *crypto.Encryptor
}

func NewPostgresStore(db *postgres.DB) *PostgresStore {
	return &PostgresStore{db: db, enc: crypto.NewNoop()}
}

// SetEncryptor configures AES-256-GCM encryption for webhook signing secrets
// at rest. When set (non-noop), Create/UpdateEndpointSecret encrypt the raw
// whsec_ secret before INSERT; Get/ListEndpoints decrypt it after SELECT so
// the Dispatch path can sign with the plaintext key. Without this, the raw
// signing key is stored in plaintext — a DB dump yields webhook-forging
// capability against every tenant's receivers.
func (s *PostgresStore) SetEncryptor(enc *crypto.Encryptor) {
	if enc == nil {
		enc = crypto.NewNoop()
	}
	s.enc = enc
}

// lastFour returns the last 4 characters of the raw signing secret (e.g.
// "whsec_abc...xyz9" → "xyz9") for display in the UI. We compute it on
// write and persist alongside the ciphertext because the ciphertext is
// non-deterministic — we can't recompute last4 from storage without
// decrypting first.
func lastFour(secret string) string {
	if len(secret) <= 4 {
		return secret
	}
	return secret[len(secret)-4:]
}

func (s *PostgresStore) CreateEndpoint(ctx context.Context, tenantID string, ep domain.WebhookEndpoint) (domain.WebhookEndpoint, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.WebhookEndpoint{}, err
	}
	defer postgres.Rollback(tx)

	created, err := s.createEndpointTx(ctx, tx, tenantID, ep)
	if err != nil {
		return domain.WebhookEndpoint{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.WebhookEndpoint{}, err
	}
	return created, nil
}

// CreateEndpointTx inserts a webhook endpoint inside an existing tx. Used
// by recipe.Service.Instantiate so a recipe that wants a default outbound
// receiver lands its endpoint atomically with the rest of the recipe.
func (s *PostgresStore) CreateEndpointTx(ctx context.Context, tx *sql.Tx, tenantID string, ep domain.WebhookEndpoint) (domain.WebhookEndpoint, error) {
	return s.createEndpointTx(ctx, tx, tenantID, ep)
}

func (s *PostgresStore) createEndpointTx(ctx context.Context, tx *sql.Tx, tenantID string, ep domain.WebhookEndpoint) (domain.WebhookEndpoint, error) {
	id := postgres.NewID("vlx_whe")
	now := time.Now().UTC()
	eventsJSON, _ := json.Marshal(ep.Events)

	plaintextSecret := ep.Secret
	secretEncrypted, err := s.enc.Encrypt(plaintextSecret)
	if err != nil {
		return domain.WebhookEndpoint{}, fmt.Errorf("encrypt webhook secret: %w", err)
	}
	secretLast4 := lastFour(plaintextSecret)

	// New endpoints start in single-secret mode — secondary_* stay NULL
	// until the first RotateEndpointSecret call populates them.
	err = tx.QueryRowContext(ctx, `
		INSERT INTO webhook_endpoints (id, tenant_id, url, description, secret_encrypted, secret_last4, events, active, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$9)
		RETURNING id, tenant_id, livemode, url, COALESCE(description,''), events, active, created_at, updated_at
	`, id, tenantID, ep.URL, postgres.NullableString(ep.Description), secretEncrypted, secretLast4, eventsJSON, ep.Active, now,
	).Scan(&ep.ID, &ep.TenantID, &ep.Livemode, &ep.URL, &ep.Description, &eventsJSON, &ep.Active, &ep.CreatedAt, &ep.UpdatedAt)
	if err != nil {
		return domain.WebhookEndpoint{}, err
	}
	_ = json.Unmarshal(eventsJSON, &ep.Events)
	ep.Secret = plaintextSecret // Callers need it once to show to the user.
	ep.SecretLast4 = secretLast4
	return ep, nil
}

func (s *PostgresStore) GetEndpoint(ctx context.Context, tenantID, id string) (domain.WebhookEndpoint, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.WebhookEndpoint{}, err
	}
	defer postgres.Rollback(tx)

	var ep domain.WebhookEndpoint
	var eventsJSON []byte
	var secretEncrypted string
	var secondaryEncrypted sql.NullString
	var secondaryLast4 sql.NullString
	err = tx.QueryRowContext(ctx, `
		SELECT id, tenant_id, livemode, url, COALESCE(description,''), secret_encrypted, secret_last4,
			secondary_secret_encrypted, secondary_secret_last4, secondary_secret_expires_at,
			events, active, created_at, updated_at
		FROM webhook_endpoints WHERE id = $1 AND deleted_at IS NULL
	`, id).Scan(&ep.ID, &ep.TenantID, &ep.Livemode, &ep.URL, &ep.Description,
		&secretEncrypted, &ep.SecretLast4,
		&secondaryEncrypted, &secondaryLast4, &ep.SecondarySecretExpiresAt,
		&eventsJSON, &ep.Active, &ep.CreatedAt, &ep.UpdatedAt)
	if err == sql.ErrNoRows {
		return domain.WebhookEndpoint{}, errs.ErrNotFound
	}
	if err != nil {
		return domain.WebhookEndpoint{}, err
	}
	if ep.Secret, err = s.enc.Decrypt(secretEncrypted); err != nil {
		return domain.WebhookEndpoint{}, fmt.Errorf("decrypt webhook secret: %w", err)
	}
	if secondaryEncrypted.Valid && secondaryEncrypted.String != "" {
		if ep.SecondarySecret, err = s.enc.Decrypt(secondaryEncrypted.String); err != nil {
			return domain.WebhookEndpoint{}, fmt.Errorf("decrypt secondary webhook secret: %w", err)
		}
	}
	if secondaryLast4.Valid {
		ep.SecondarySecretLast4 = secondaryLast4.String
	}
	_ = json.Unmarshal(eventsJSON, &ep.Events)
	return ep, nil
}

// UpdateEndpoint mutates url / description / events / active WITHOUT
// touching the signing secret (Stripe's update shape: subscribers change
// the target or the event set without a receiver redeploy — pre-2026-07-05
// the only mutation path was delete+recreate, which minted a new secret,
// and recipe-created endpoints were permanently dead: Active:false with a
// placeholder URL and no way to fix either). The caller (service) has
// already validated URL + event names.
func (s *PostgresStore) UpdateEndpoint(ctx context.Context, tenantID string, ep domain.WebhookEndpoint) (domain.WebhookEndpoint, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.WebhookEndpoint{}, err
	}
	defer postgres.Rollback(tx)

	eventsJSON, err := json.Marshal(ep.Events)
	if err != nil {
		return domain.WebhookEndpoint{}, fmt.Errorf("marshal events: %w", err)
	}
	var out domain.WebhookEndpoint
	var outEvents []byte
	var secondaryLast4 sql.NullString
	err = tx.QueryRowContext(ctx, `
		UPDATE webhook_endpoints SET
			url = $1, description = $2, events = $3, active = $4, updated_at = NOW()
		WHERE id = $5
		RETURNING id, tenant_id, livemode, url, COALESCE(description,''), secret_last4,
			secondary_secret_last4, secondary_secret_expires_at,
			events, active, created_at, updated_at
	`, ep.URL, ep.Description, eventsJSON, ep.Active, ep.ID).
		Scan(&out.ID, &out.TenantID, &out.Livemode, &out.URL, &out.Description, &out.SecretLast4,
			&secondaryLast4, &out.SecondarySecretExpiresAt,
			&outEvents, &out.Active, &out.CreatedAt, &out.UpdatedAt)
	if err == sql.ErrNoRows {
		return domain.WebhookEndpoint{}, errs.ErrNotFound
	}
	if err != nil {
		return domain.WebhookEndpoint{}, err
	}
	_ = json.Unmarshal(outEvents, &out.Events)
	if secondaryLast4.Valid {
		out.SecondarySecretLast4 = secondaryLast4.String
	}
	if err := tx.Commit(); err != nil {
		return domain.WebhookEndpoint{}, err
	}
	return out, nil
}

func (s *PostgresStore) ListEndpoints(ctx context.Context, tenantID string) ([]domain.WebhookEndpoint, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	// No active filter: both dispatch paths skip inactive endpoints in
	// their own match loops (ep.Active), and hiding inactive rows here made
	// them UNREACHABLE from the dashboard — a deactivated endpoint (or a
	// recipe-created one, born inactive with a placeholder URL) vanished
	// from the Endpoints page with no way to edit, activate, or delete it,
	// while the FE's "paused" badge sat as dead code. Found walking FLOW W0
	// (2026-08-03): the recipe flow tells the operator to bring the
	// endpoint live, and the list refused to show it.
	rows, err := tx.QueryContext(ctx, `
		SELECT id, tenant_id, livemode, url, COALESCE(description,''), secret_encrypted, secret_last4,
			secondary_secret_encrypted, secondary_secret_last4, secondary_secret_expires_at,
			events, active, created_at, updated_at
		FROM webhook_endpoints WHERE deleted_at IS NULL ORDER BY created_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var endpoints []domain.WebhookEndpoint
	for rows.Next() {
		var ep domain.WebhookEndpoint
		var eventsJSON []byte
		var secretEncrypted string
		var secondaryEncrypted sql.NullString
		var secondaryLast4 sql.NullString
		if err := rows.Scan(&ep.ID, &ep.TenantID, &ep.Livemode, &ep.URL, &ep.Description,
			&secretEncrypted, &ep.SecretLast4,
			&secondaryEncrypted, &secondaryLast4, &ep.SecondarySecretExpiresAt,
			&eventsJSON, &ep.Active, &ep.CreatedAt, &ep.UpdatedAt); err != nil {
			return nil, err
		}
		if ep.Secret, err = s.enc.Decrypt(secretEncrypted); err != nil {
			return nil, fmt.Errorf("decrypt webhook secret: %w", err)
		}
		if secondaryEncrypted.Valid && secondaryEncrypted.String != "" {
			if ep.SecondarySecret, err = s.enc.Decrypt(secondaryEncrypted.String); err != nil {
				return nil, fmt.Errorf("decrypt secondary webhook secret: %w", err)
			}
		}
		if secondaryLast4.Valid {
			ep.SecondarySecretLast4 = secondaryLast4.String
		}
		_ = json.Unmarshal(eventsJSON, &ep.Events)
		endpoints = append(endpoints, ep)
	}
	return endpoints, rows.Err()
}

func (s *PostgresStore) DeleteEndpoint(ctx context.Context, tenantID, id string) error {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)

	// Delete is an intent distinct from pause (migration 0168): it stamps
	// deleted_at, which removes the row from every list and Get. Pause is
	// active=false with deleted_at NULL — visible as "paused", reversible.
	result, err := tx.ExecContext(ctx, `UPDATE webhook_endpoints SET active = false, deleted_at = now(), updated_at = now() WHERE id = $1 AND deleted_at IS NULL`, id)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return errs.ErrNotFound
	}
	return tx.Commit()
}

func (s *PostgresStore) RotateEndpointSecret(ctx context.Context, tenantID, id, newSecret string, gracePeriod time.Duration) (domain.WebhookEndpoint, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.WebhookEndpoint{}, err
	}
	defer postgres.Rollback(tx)

	// FOR UPDATE so two concurrent rotates can't both read the same
	// current secret and both try to stash it as secondary — one would
	// see the other's new primary. Single-row lock, no contention at
	// real-world rotation frequency.
	var currentEncrypted, currentLast4 string
	err = tx.QueryRowContext(ctx, `
		SELECT secret_encrypted, secret_last4
		FROM webhook_endpoints WHERE id = $1 FOR UPDATE
	`, id).Scan(&currentEncrypted, &currentLast4)
	if err == sql.ErrNoRows {
		return domain.WebhookEndpoint{}, errs.ErrNotFound
	}
	if err != nil {
		return domain.WebhookEndpoint{}, err
	}

	newEncrypted, err := s.enc.Encrypt(newSecret)
	if err != nil {
		return domain.WebhookEndpoint{}, fmt.Errorf("encrypt webhook secret: %w", err)
	}
	newLast4 := lastFour(newSecret)

	var (
		secondaryEncArg  any
		secondaryLastArg any
		secondaryExpArg  any
		expiresAt        *time.Time
	)
	if gracePeriod > 0 {
		secondaryEncArg = currentEncrypted
		secondaryLastArg = currentLast4
		exp := time.Now().UTC().Add(gracePeriod)
		expiresAt = &exp
		secondaryExpArg = exp
	}

	var ep domain.WebhookEndpoint
	var eventsJSON []byte
	var secondaryLast4Scan sql.NullString
	err = tx.QueryRowContext(ctx, `
		UPDATE webhook_endpoints SET
			secret_encrypted = $1,
			secret_last4 = $2,
			secondary_secret_encrypted = $3,
			secondary_secret_last4 = $4,
			secondary_secret_expires_at = $5,
			updated_at = NOW()
		WHERE id = $6
		RETURNING id, tenant_id, livemode, url, COALESCE(description,''),
			secondary_secret_last4, secondary_secret_expires_at,
			events, active, created_at, updated_at
	`, newEncrypted, newLast4, secondaryEncArg, secondaryLastArg, secondaryExpArg, id).
		Scan(&ep.ID, &ep.TenantID, &ep.Livemode, &ep.URL, &ep.Description,
			&secondaryLast4Scan, &ep.SecondarySecretExpiresAt,
			&eventsJSON, &ep.Active, &ep.CreatedAt, &ep.UpdatedAt)
	if err != nil {
		return domain.WebhookEndpoint{}, err
	}
	_ = json.Unmarshal(eventsJSON, &ep.Events)
	ep.Secret = newSecret
	ep.SecretLast4 = newLast4
	if secondaryLast4Scan.Valid {
		ep.SecondarySecretLast4 = secondaryLast4Scan.String
	}
	// Preserve expiresAt on the returned struct even when Scan gave us
	// nil (immediate post-commit visibility quirk on some drivers).
	if ep.SecondarySecretExpiresAt == nil && expiresAt != nil {
		ep.SecondarySecretExpiresAt = expiresAt
	}
	if err := tx.Commit(); err != nil {
		return domain.WebhookEndpoint{}, err
	}
	return ep, nil
}

func (s *PostgresStore) CreateEvent(ctx context.Context, tenantID string, event domain.WebhookEvent) (domain.WebhookEvent, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.WebhookEvent{}, err
	}
	defer postgres.Rollback(tx)

	id := postgres.NewID("vlx_whevt")
	now := time.Now().UTC()
	payloadJSON, _ := json.Marshal(event.Payload)

	event.ID = id
	event.TenantID = tenantID
	event.CreatedAt = now

	// The 0021 trigger sets livemode from app.livemode — read it back so
	// callers (dispatcher) can pass the same mode through when delivering.
	// replay_of_event_id is NULL for fresh events; CreateReplayEvent below
	// is the only path that populates it.
	err = tx.QueryRowContext(ctx, `
		INSERT INTO webhook_events (id, tenant_id, event_type, payload, created_at, replay_of_event_id)
		VALUES ($1,$2,$3,$4,$5,$6)
		RETURNING livemode
	`, id, tenantID, event.EventType, payloadJSON, now, postgres.NullableStringPtr(event.ReplayOfEventID)).Scan(&event.Livemode)
	if err != nil {
		return domain.WebhookEvent{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.WebhookEvent{}, err
	}
	return event, nil
}

// CreateEventWithDeliveries creates the event AND all its delivery rows
// in ONE transaction (P5, CreateEndpointTx precedent) — the old shape
// committed the event, then each fan-out goroutine inserted its own
// delivery row: a crash in between minted an event no delivery row
// referenced, so no endpoint ever received it and nothing retried.
// Delivery rows are BORN LEASED (next_retry_at = now()+birthLease): the
// in-process goroutine owns the first attempt inside the lease, and the
// retry worker is the crash backstop that picks the row up at lease
// expiry — never a NULL next_retry_at double-POST race.
//
// outboxRowID, when non-empty, marks the webhook_outbox row dispatched
// INSIDE the same tx (handler-owns-mark): a crash after this commit but
// before the old separate outbox mark used to re-run the handler and
// mint a DUPLICATE event with a fresh id that receivers cannot dedupe.
func (s *PostgresStore) CreateEventWithDeliveries(ctx context.Context, tenantID string, event domain.WebhookEvent, endpointIDs []string, birthLease time.Duration, outboxRowID string) (domain.WebhookEvent, []domain.WebhookDelivery, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.WebhookEvent{}, nil, err
	}
	defer postgres.Rollback(tx)

	id := postgres.NewID("vlx_whevt")
	now := time.Now().UTC()
	payloadJSON, _ := json.Marshal(event.Payload)

	event.ID = id
	event.TenantID = tenantID
	event.CreatedAt = now

	err = tx.QueryRowContext(ctx, `
		INSERT INTO webhook_events (id, tenant_id, event_type, payload, created_at, replay_of_event_id)
		VALUES ($1,$2,$3,$4,$5,$6)
		RETURNING livemode
	`, id, tenantID, event.EventType, payloadJSON, now, postgres.NullableStringPtr(event.ReplayOfEventID)).Scan(&event.Livemode)
	if err != nil {
		return domain.WebhookEvent{}, nil, err
	}

	deliveries := make([]domain.WebhookDelivery, 0, len(endpointIDs))
	lease := now.Add(birthLease)
	for _, epID := range endpointIDs {
		d := domain.WebhookDelivery{
			ID:                postgres.NewID("vlx_whd"),
			TenantID:          tenantID,
			WebhookEndpointID: epID,
			WebhookEventID:    id,
			Status:            domain.DeliveryPending,
			NextRetryAt:       &lease,
			CreatedAt:         now,
		}
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO webhook_deliveries (id, tenant_id, webhook_endpoint_id, webhook_event_id,
				status, next_retry_at, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7)
			RETURNING livemode
		`, d.ID, tenantID, epID, id, d.Status, lease, now).Scan(&d.Livemode); err != nil {
			return domain.WebhookEvent{}, nil, fmt.Errorf("create delivery for endpoint %s: %w", epID, err)
		}
		deliveries = append(deliveries, d)
	}

	if outboxRowID != "" {
		// CAS: a replayed handler whose row was already resolved must
		// not resurrect it.
		if _, err := tx.ExecContext(ctx, `
			UPDATE webhook_outbox
			SET status = 'dispatched', dispatched_at = now(), last_error = NULL
			WHERE id = $1 AND status = 'pending'
		`, outboxRowID); err != nil {
			return domain.WebhookEvent{}, nil, fmt.Errorf("mark outbox row %s: %w", outboxRowID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return domain.WebhookEvent{}, nil, err
	}
	return event, deliveries, nil
}

// CreateDeliveriesForEvent batch-creates born-leased delivery rows for
// an event that already exists (Replay: the clone event commits in
// CreateReplayEvent first). One tx — a replay whose fan-out partially
// failed would otherwise deliver to a subset with no record of the rest.
func (s *PostgresStore) CreateDeliveriesForEvent(ctx context.Context, tenantID, eventID string, endpointIDs []string, birthLease time.Duration) ([]domain.WebhookDelivery, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	now := time.Now().UTC()
	lease := now.Add(birthLease)
	out := make([]domain.WebhookDelivery, 0, len(endpointIDs))
	for _, epID := range endpointIDs {
		d := domain.WebhookDelivery{
			ID:                postgres.NewID("vlx_whd"),
			TenantID:          tenantID,
			WebhookEndpointID: epID,
			WebhookEventID:    eventID,
			Status:            domain.DeliveryPending,
			NextRetryAt:       &lease,
			CreatedAt:         now,
		}
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO webhook_deliveries (id, tenant_id, webhook_endpoint_id, webhook_event_id,
				status, next_retry_at, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7)
			RETURNING livemode
		`, d.ID, tenantID, epID, eventID, d.Status, lease, now).Scan(&d.Livemode); err != nil {
			return nil, fmt.Errorf("create delivery for endpoint %s: %w", epID, err)
		}
		out = append(out, d)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

// CreateReplayEvent clones an existing event row into a new event whose
// replay_of_event_id points at the original. The clone reuses the
// original's event_type and payload byte-for-byte (so the diff viewer
// can verify "payload identical, only timestamps differ" — Stripe's
// "Resend" semantics).
func (s *PostgresStore) CreateReplayEvent(ctx context.Context, tenantID, originalEventID string) (domain.WebhookEvent, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.WebhookEvent{}, err
	}
	defer postgres.Rollback(tx)

	// Resolve the original. Tenant scoping comes via RLS; the row will be
	// invisible if it belongs to another tenant.
	var (
		eventType   string
		payloadJSON []byte
		origReplay  sql.NullString
	)
	err = tx.QueryRowContext(ctx, `
		SELECT event_type, payload, replay_of_event_id
		FROM webhook_events WHERE id = $1
	`, originalEventID).Scan(&eventType, &payloadJSON, &origReplay)
	if err == sql.ErrNoRows {
		return domain.WebhookEvent{}, errs.ErrNotFound
	}
	if err != nil {
		return domain.WebhookEvent{}, err
	}

	// Single-pivot rule: replays-of-replays still point at the *root*
	// original event, never at an intermediate clone. Keeps the
	// list-deliveries query a flat WHERE id = $1 OR replay_of = $1.
	rootID := originalEventID
	if origReplay.Valid && origReplay.String != "" {
		rootID = origReplay.String
	}

	id := postgres.NewID("vlx_whevt")
	now := time.Now().UTC()
	clone := domain.WebhookEvent{
		ID:              id,
		TenantID:        tenantID,
		EventType:       eventType,
		ReplayOfEventID: &rootID,
		CreatedAt:       now,
	}
	_ = json.Unmarshal(payloadJSON, &clone.Payload)

	err = tx.QueryRowContext(ctx, `
		INSERT INTO webhook_events (id, tenant_id, event_type, payload, created_at, replay_of_event_id)
		VALUES ($1,$2,$3,$4,$5,$6)
		RETURNING livemode
	`, id, tenantID, eventType, payloadJSON, now, rootID).Scan(&clone.Livemode)
	if err != nil {
		return domain.WebhookEvent{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.WebhookEvent{}, err
	}
	return clone, nil
}

// GetEvent returns a single event by id (tenant-scoped via RLS).
func (s *PostgresStore) GetEvent(ctx context.Context, tenantID, id string) (domain.WebhookEvent, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.WebhookEvent{}, err
	}
	defer postgres.Rollback(tx)

	var e domain.WebhookEvent
	var payloadJSON []byte
	var replayOf sql.NullString
	err = tx.QueryRowContext(ctx, `
		SELECT id, tenant_id, livemode, event_type, payload, created_at, replay_of_event_id
		FROM webhook_events WHERE id = $1
	`, id).Scan(&e.ID, &e.TenantID, &e.Livemode, &e.EventType, &payloadJSON, &e.CreatedAt, &replayOf)
	if err == sql.ErrNoRows {
		return domain.WebhookEvent{}, errs.ErrNotFound
	}
	if err != nil {
		return domain.WebhookEvent{}, err
	}
	_ = json.Unmarshal(payloadJSON, &e.Payload)
	if replayOf.Valid {
		s := replayOf.String
		e.ReplayOfEventID = &s
	}
	return e, nil
}

// EventDeliveryStatuses rolls each event's deliveries up to one status.
// Precedence: any pending → "pending" (work in flight), else any failed →
// "failed" (something needs attention — a partial success must not read as
// done), else "delivered". Replaces the age heuristic the SSE snapshot used
// to guess with ("<24h old ⇒ pending"), which showed a settled event as
// pending all day and would have shown a permanently-failed delivery as
// delivered once it aged past the ladder.
func (s *PostgresStore) EventDeliveryStatuses(ctx context.Context, tenantID string, eventIDs []string) (map[string]string, error) {
	out := make(map[string]string, len(eventIDs))
	if len(eventIDs) == 0 {
		return out, nil
	}
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	// string_to_array instead of a driver array type: ids are vlx_whevt_…
	// (no commas by construction), and the repo's database/sql driver has
	// no native []string binding.
	rows, err := tx.QueryContext(ctx, `
		SELECT webhook_event_id,
			CASE
				WHEN COUNT(*) FILTER (WHERE status = 'pending') > 0 THEN 'pending'
				WHEN COUNT(*) FILTER (WHERE status = 'failed')  > 0 THEN 'failed'
				ELSE 'delivered'
			END
		FROM webhook_deliveries
		WHERE webhook_event_id = ANY(string_to_array($1, ','))
		GROUP BY webhook_event_id
	`, strings.Join(eventIDs, ","))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, status string
		if err := rows.Scan(&id, &status); err != nil {
			return nil, err
		}
		out[id] = status
	}
	return out, rows.Err()
}

func (s *PostgresStore) ListEvents(ctx context.Context, tenantID string, limit int) ([]domain.WebhookEvent, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	// Default 50, clamp to 100 — no-silent-fallbacks principle.
	if limit <= 0 {
		limit = 50
	} else if limit > 100 {
		limit = 100
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT id, tenant_id, livemode, event_type, payload, created_at, replay_of_event_id
		FROM webhook_events ORDER BY created_at DESC LIMIT $1
	`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var events []domain.WebhookEvent
	for rows.Next() {
		var e domain.WebhookEvent
		var payloadJSON []byte
		var replayOf sql.NullString
		if err := rows.Scan(&e.ID, &e.TenantID, &e.Livemode, &e.EventType, &payloadJSON, &e.CreatedAt, &replayOf); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(payloadJSON, &e.Payload)
		if replayOf.Valid {
			s := replayOf.String
			e.ReplayOfEventID = &s
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

func (s *PostgresStore) CreateDelivery(ctx context.Context, tenantID string, d domain.WebhookDelivery) (domain.WebhookDelivery, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.WebhookDelivery{}, err
	}
	defer postgres.Rollback(tx)

	id := postgres.NewID("vlx_whd")
	now := time.Now().UTC()

	d.ID = id
	d.TenantID = tenantID
	d.CreatedAt = now

	err = tx.QueryRowContext(ctx, `
		INSERT INTO webhook_deliveries (id, tenant_id, webhook_endpoint_id, webhook_event_id,
			status, created_at)
		VALUES ($1,$2,$3,$4,$5,$6)
		RETURNING livemode
	`, id, tenantID, d.WebhookEndpointID, d.WebhookEventID, d.Status, now).Scan(&d.Livemode)
	if err != nil {
		return domain.WebhookDelivery{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.WebhookDelivery{}, err
	}
	return d, nil
}

func (s *PostgresStore) UpdateDelivery(ctx context.Context, tenantID string, d domain.WebhookDelivery) (domain.WebhookDelivery, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.WebhookDelivery{}, err
	}
	defer postgres.Rollback(tx)

	// CAS on status='pending' (P5): marks record the outcome of an
	// attempt that STARTED while the row was pending. Without the guard,
	// a slow worker whose lease expired could flip a row a sibling had
	// already resolved back to pending — a third POST of a delivered
	// webhook. A zero-row match means the mark is stale: dropped and
	// surfaced, never applied.
	res, err := tx.ExecContext(ctx, `
		UPDATE webhook_deliveries SET status=$1, http_status_code=$2,
			response_body=$3, error_message=$4, attempt_count=$5, completed_at=$6,
			next_retry_at=$7
		WHERE id=$8 AND status='pending'`,
		d.Status, d.HTTPStatusCode, postgres.NullableString(d.ResponseBody),
		postgres.NullableString(d.ErrorMessage), d.AttemptCount, postgres.NullableTime(d.CompletedAt),
		postgres.NullableTime(d.NextRetryAt), d.ID)
	if err != nil {
		return domain.WebhookDelivery{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return domain.WebhookDelivery{}, ErrStaleDeliveryMark
	}
	if err := tx.Commit(); err != nil {
		return domain.WebhookDelivery{}, err
	}
	return d, nil
}

// ErrStaleDeliveryMark is returned when a delivery mark matched no
// pending row — the row was resolved by another worker after this
// worker's lease expired. The stale writer drops its result.
var ErrStaleDeliveryMark = errors.New("webhook: stale delivery mark (row no longer pending)")

// ListPendingDeliveries atomically CLAIMS up to `limit` due deliveries and
// returns them. It is not a pure read: each returned row is leased by pushing
// next_retry_at one claim-lease into the future, and rows already locked by a
// concurrent worker are skipped (FOR UPDATE SKIP LOCKED). This makes the retry
// worker safe to run on multiple replicas — two workers never claim the same
// delivery, so a webhook isn't delivered twice per due-tick. The lease also
// provides crash recovery: if the claiming worker dies before overwriting the
// row's status, the lease (retryClaimLease — sized to the claim batch, P5) expires and another worker
// re-claims it. The window must exceed the per-attempt HTTP timeout (10s) by a
// wide margin so an in-flight delivery is never re-claimed underneath itself.
// TryRetryLock leader-gates the retry worker tick (LockKeyWebhookRetry).
func (s *PostgresStore) TryRetryLock(ctx context.Context) (DispatchLock, bool, error) {
	lock, ok, err := s.db.TryAdvisoryLock(ctx, postgres.LockKeyWebhookRetry)
	if err != nil || !ok {
		return nil, ok, err
	}
	return lock, true, nil
}

func (s *PostgresStore) ListPendingDeliveries(ctx context.Context, limit int) ([]domain.WebhookDelivery, error) {
	if limit <= 0 {
		limit = 10
	}

	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	rows, err := tx.QueryContext(ctx, `
		UPDATE webhook_deliveries
		SET next_retry_at = NOW() + make_interval(secs => $1)
		WHERE id IN (
			SELECT id FROM webhook_deliveries
			-- next_retry_at IS NULL covers a freshly-created 'pending' row whose
			-- first-attempt goroutine died before writing an outcome (NULL <= NOW()
			-- is never true, so it would otherwise strand forever). Sibling pattern:
			-- invoice tax-retry claim.
			WHERE status = 'pending' AND (next_retry_at IS NULL OR next_retry_at <= NOW())
			ORDER BY next_retry_at ASC NULLS FIRST
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id, tenant_id, livemode, webhook_endpoint_id, webhook_event_id, status,
			COALESCE(http_status_code, 0), COALESCE(response_body,''), COALESCE(error_message,''),
			attempt_count, next_retry_at, created_at, completed_at
	`, int(retryClaimLease(limit).Seconds()), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var deliveries []domain.WebhookDelivery
	for rows.Next() {
		var d domain.WebhookDelivery
		if err := rows.Scan(&d.ID, &d.TenantID, &d.Livemode, &d.WebhookEndpointID, &d.WebhookEventID,
			&d.Status, &d.HTTPStatusCode, &d.ResponseBody, &d.ErrorMessage,
			&d.AttemptCount, &d.NextRetryAt, &d.CreatedAt, &d.CompletedAt); err != nil {
			return nil, err
		}
		deliveries = append(deliveries, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return deliveries, tx.Commit()
}

// endpointSuccessRate is the delivered-success percentage over COMPLETED
// deliveries only (succeeded + failed). Pending deliveries — still in flight or
// awaiting retry — are excluded from the denominator so in-flight traffic
// doesn't artificially depress an endpoint's success rate. Returns 0 when
// nothing has completed yet.
func endpointSuccessRate(succeeded, failed int) float64 {
	completed := succeeded + failed
	if completed == 0 {
		return 0
	}
	return float64(succeeded) / float64(completed) * 100
}

func (s *PostgresStore) GetEndpointStats(ctx context.Context, tenantID string) ([]EndpointStats, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	rows, err := tx.QueryContext(ctx, `
		SELECT webhook_endpoint_id,
			COUNT(*) AS total,
			COUNT(*) FILTER (WHERE status = 'succeeded') AS succeeded,
			COUNT(*) FILTER (WHERE status = 'failed') AS failed
		FROM webhook_deliveries
		GROUP BY webhook_endpoint_id
	`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var stats []EndpointStats
	for rows.Next() {
		var s EndpointStats
		if err := rows.Scan(&s.EndpointID, &s.TotalDeliveries, &s.Succeeded, &s.Failed); err != nil {
			return nil, err
		}
		s.SuccessRate = endpointSuccessRate(s.Succeeded, s.Failed)
		stats = append(stats, s)
	}
	return stats, rows.Err()
}

func (s *PostgresStore) ListDeliveries(ctx context.Context, tenantID, eventID string) ([]domain.WebhookDelivery, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	// Replay-tree walk: an operator clicking the dashboard's Replay
	// button on event A produces clone B (replay_of=A), which gets its
	// own deliveries. The dashboard expects ALL deliveries — original +
	// every replay clone — stitched into one timeline. We pull both
	// arms in a single query so the unified ORDER BY created_at ASC is
	// applied across the whole tree without an in-memory merge.
	rows, err := tx.QueryContext(ctx, `
		SELECT d.id, d.tenant_id, d.livemode, d.webhook_endpoint_id, d.webhook_event_id, d.status,
			COALESCE(d.http_status_code, 0), COALESCE(d.response_body,''), COALESCE(d.error_message,''),
			d.attempt_count, d.next_retry_at, d.created_at, d.completed_at
		FROM webhook_deliveries d
		JOIN webhook_events e ON e.id = d.webhook_event_id
		WHERE e.id = $1 OR e.replay_of_event_id = $1
		ORDER BY d.created_at ASC
	`, eventID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var deliveries []domain.WebhookDelivery
	for rows.Next() {
		var d domain.WebhookDelivery
		if err := rows.Scan(&d.ID, &d.TenantID, &d.Livemode, &d.WebhookEndpointID, &d.WebhookEventID,
			&d.Status, &d.HTTPStatusCode, &d.ResponseBody, &d.ErrorMessage,
			&d.AttemptCount, &d.NextRetryAt, &d.CreatedAt, &d.CompletedAt); err != nil {
			return nil, err
		}
		deliveries = append(deliveries, d)
	}
	return deliveries, rows.Err()
}
