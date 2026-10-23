package usage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// HeldStore keeps usage rows that could not be ingested yet because their
// customer or meter did not exist (#1044, ADR-122). PostgresStore
// implements it; NewService picks it up from the Store it is given.
type HeldStore interface {
	// Hold inserts a held row. Returns errs.ErrDuplicateKey (nothing
	// written) when a held row with the same idempotency key exists.
	Hold(ctx context.Context, tenantID string, ev domain.HeldUsageEvent) (domain.HeldUsageEvent, error)
	// ListHeldSummary groups the rows still held, for the most recent
	// HeldListCustomers external customer ids, with totals over all of them.
	ListHeldSummary(ctx context.Context, tenantID string) (HeldSummary, error)
	// HeldFor returns the distinct event names and the row count still held
	// for one external customer id.
	HeldFor(ctx context.Context, tenantID, externalCustomerID string) (eventNames []string, rows int, err error)
	// ReleaseHeld moves held rows into usage_events in one transaction.
	ReleaseHeld(ctx context.Context, tenantID string, in ReleaseHeldInput, emit func(tx *sql.Tx, res ReleaseHeldResult) error) (ReleaseHeldResult, error)
	// DiscardHeld marks held rows discarded (kept, never ingested).
	DiscardHeld(ctx context.Context, tenantID, externalCustomerID, eventName string, emit func(tx *sql.Tx, n int) error) (int, error)
	// IngestResolvingHeld is the live LiteLLM ingest: it writes the event
	// unless a held row with the same key was discarded, and resolves a
	// held row with the same key as released.
	IngestResolvingHeld(ctx context.Context, tenantID string, event domain.UsageEvent) (domain.UsageEvent, error)
	// IngestCallResolvingHeld writes every event of one LiteLLM call in one
	// transaction, with IngestResolvingHeld's per-key rules and the
	// pre-split replay guard.
	IngestCallResolvingHeld(ctx context.Context, tenantID string, call StoreCall) (CallResult, error)
	// HoldCall holds every event of one LiteLLM call in one transaction,
	// with the same replay guard.
	HoldCall(ctx context.Context, tenantID string, call HeldCall) (CallResult, error)
}

// HeldListCustomers caps how many external customer ids
// GET /v1/usage-events/held lists. Every group of a listed id is returned,
// so a listed id's row count is complete; HeldSummary's totals count all ids.
const HeldListCustomers = 200

// HeldSummary is the held list plus totals over every row still held, so
// the dashboard can say when the list is cut.
type HeldSummary struct {
	Groups []HeldUsageGroup
	// TotalCustomers counts the distinct external customer ids with rows
	// still held, listed or not.
	TotalCustomers int
	// TotalRows counts every row still held, listed or not.
	TotalRows int
}

// HeldUsageGroup is one line of GET /v1/usage-events/held: the rows still
// held for one (external customer id, event name, reason).
type HeldUsageGroup struct {
	ExternalCustomerID string                 `json:"external_customer_id"`
	EventName          string                 `json:"event_name"`
	Reason             domain.HeldUsageReason `json:"reason"`
	Rows               int                    `json:"rows"`
	TotalQuantity      decimal.Decimal        `json:"total_quantity"`
	FirstEventAt       time.Time              `json:"first_event_at"`
	LastEventAt        time.Time              `json:"last_event_at"`
	LastReceivedAt     time.Time              `json:"last_received_at"`
	// CustomerID is the customer whose external_id now matches, or ""
	// when none does yet. Filled by the handler, not the store.
	CustomerID string `json:"customer_id"`
	// MeterExists says whether a meter with EventName exists now. Filled by
	// the handler, not the store.
	MeterExists bool `json:"meter_exists"`
}

// ReleaseHeldInput selects the held rows to release and where they go.
type ReleaseHeldInput struct {
	ExternalCustomerID string
	// CustomerID is the customer the rows are ingested for.
	CustomerID string
	// MeterIDs maps event name to meter id. Rows whose event name is absent
	// stay held.
	MeterIDs map[string]string
	// NotAfter is the customer's current time. Rows stamped after it stay
	// held: ingesting them would put usage in the customer's future.
	NotAfter time.Time
	// Limit caps the rows claimed by one call.
	Limit int
}

// ReleasedRow is one held row the release wrote, or found already written.
type ReleasedRow struct {
	HeldID       string    `json:"-"`
	EventID      string    `json:"-"`
	EventName    string    `json:"-"`
	EventAt      time.Time `json:"-"`
	Deduplicated bool      `json:"-"`
}

// ReleaseHeldResult reports what one release did.
type ReleaseHeldResult struct {
	Rows []ReleasedRow
	// StillHeld counts rows for the external customer id that are still
	// held after this release and that this release could not take: a meter
	// that does not exist, or a time after the customer's current time.
	StillHeld int
	// MoreToRelease counts rows still held that this release could have
	// taken but did not, because it stopped at Limit. Another release takes
	// them.
	MoreToRelease int
}

// Hold inserts one held row. ON CONFLICT DO NOTHING on the idempotency key,
// so a LiteLLM resend of a row already held writes nothing and reports
// errs.ErrDuplicateKey, the same contract ingestOneTx has.
func (s *PostgresStore) Hold(ctx context.Context, tenantID string, ev domain.HeldUsageEvent) (domain.HeldUsageEvent, error) {
	props, err := propertiesJSON(ev.Dimensions)
	if err != nil {
		return domain.HeldUsageEvent{}, err
	}
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.HeldUsageEvent{}, err
	}
	defer postgres.Rollback(tx)

	err = tx.QueryRowContext(ctx, `
		INSERT INTO usage_held_events (id, tenant_id, source, external_customer_id,
			event_name, quantity, properties, idempotency_key, event_at,
			observed_cost_micros, reason)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (tenant_id, livemode, idempotency_key) DO NOTHING
		RETURNING id, received_at, status
	`, postgres.NewID("vlx_hue"), tenantID, ev.Source, ev.ExternalCustomerID,
		ev.EventName, ev.Quantity, props, ev.IdempotencyKey, ev.EventAt,
		ev.ObservedCostMicros, string(ev.Reason),
	).Scan(&ev.ID, &ev.ReceivedAt, &ev.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.HeldUsageEvent{}, fmt.Errorf("%w: held idempotency_key %q", errs.ErrDuplicateKey, ev.IdempotencyKey)
	}
	if err != nil {
		return domain.HeldUsageEvent{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.HeldUsageEvent{}, err
	}
	return ev, nil
}

// ListHeldSummary returns one group per (external customer id, event name,
// reason) over the rows still held, newest arrival first.
func (s *PostgresStore) ListHeldSummary(ctx context.Context, tenantID string) (HeldSummary, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return HeldSummary{}, err
	}
	defer postgres.Rollback(tx)

	out := HeldSummary{Groups: []HeldUsageGroup{}}
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT external_customer_id), COUNT(*)
		FROM usage_held_events
		WHERE tenant_id = $1 AND status = 'held'
	`, tenantID).Scan(&out.TotalCustomers, &out.TotalRows); err != nil {
		return HeldSummary{}, err
	}

	// The cap is on external customer ids, not groups, so every listed id
	// carries all its held rows.
	rows, err := tx.QueryContext(ctx, `
		WITH listed AS (
			SELECT external_customer_id
			FROM usage_held_events
			WHERE tenant_id = $1 AND status = 'held'
			GROUP BY external_customer_id
			ORDER BY MAX(received_at) DESC, external_customer_id
			LIMIT $2
		)
		SELECT external_customer_id, event_name, reason, COUNT(*), SUM(quantity),
			MIN(event_at), MAX(event_at), MAX(received_at)
		FROM usage_held_events
		WHERE tenant_id = $1 AND status = 'held'
		  AND external_customer_id IN (SELECT external_customer_id FROM listed)
		GROUP BY external_customer_id, event_name, reason
		ORDER BY MAX(received_at) DESC, external_customer_id, event_name
	`, tenantID, HeldListCustomers)
	if err != nil {
		return HeldSummary{}, err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var g HeldUsageGroup
		if err := rows.Scan(&g.ExternalCustomerID, &g.EventName, &g.Reason, &g.Rows, &g.TotalQuantity,
			&g.FirstEventAt, &g.LastEventAt, &g.LastReceivedAt); err != nil {
			return HeldSummary{}, err
		}
		out.Groups = append(out.Groups, g)
	}
	return out, rows.Err()
}

// HeldFor returns the event names and row count still held for one external
// customer id.
func (s *PostgresStore) HeldFor(ctx context.Context, tenantID, externalCustomerID string) ([]string, int, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, 0, err
	}
	defer postgres.Rollback(tx)

	rows, err := tx.QueryContext(ctx, `
		SELECT event_name, COUNT(*) FROM usage_held_events
		WHERE tenant_id = $1 AND status = 'held' AND external_customer_id = $2
		GROUP BY event_name ORDER BY event_name
	`, tenantID, externalCustomerID)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	var keys []string
	total := 0
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			return nil, 0, err
		}
		keys = append(keys, k)
		total += n
	}
	return keys, total, rows.Err()
}

// ReleaseHeld ingests held rows for one external customer id in ONE
// transaction:
//
//  1. Claim: UPDATE ... SET status='released' WHERE status='held' ...
//     RETURNING. Row locks make a concurrent release wait, then re-check
//     status='held' and skip what this one claimed: disjoint sets.
//  2. Insert each claimed row through ingestOneTx, the same INSERT every
//     ingest path uses, under its ORIGINAL idempotency key and event time,
//     with origin 'backfill'. ON CONFLICT there means usage_events already
//     has the key (LiteLLM resent the call after the customer existed): the
//     held row is still marked released, with no event id, and nothing is
//     billed twice. A LiteLLM cache-write row is treated the same way when
//     usage_events already bills its call the pre-split way, writes inside
//     :input (writesBilledInInput).
//  3. Record the written event id on the held row, run emit (the service
//     reads the meters' buckets and writes the audit row on tx), commit. A
//     failure anywhere rolls all of it back: the rows stay held.
func (s *PostgresStore) ReleaseHeld(ctx context.Context, tenantID string, in ReleaseHeldInput, emit func(tx *sql.Tx, res ReleaseHeldResult) error) (ReleaseHeldResult, error) {
	keys := make([]string, 0, len(in.MeterIDs))
	for k := range in.MeterIDs {
		keys = append(keys, k)
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 5000
	}

	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return ReleaseHeldResult{}, err
	}
	defer postgres.Rollback(tx)

	rows, err := tx.QueryContext(ctx, `
		UPDATE usage_held_events SET status = 'released', resolved_at = now(),
			released_customer_id = $5
		WHERE id IN (
			SELECT id FROM usage_held_events
			WHERE tenant_id = $1 AND status = 'held'
			  AND external_customer_id = $2
			  AND event_name = ANY($3::text[])
			  AND event_at <= $4
			ORDER BY event_at, id
			LIMIT $6
		)
		AND status = 'held'
		RETURNING id, event_name, quantity, properties, idempotency_key, event_at, observed_cost_micros
	`, tenantID, in.ExternalCustomerID, postgres.StringArray(keys), in.NotAfter, in.CustomerID, limit)
	if err != nil {
		return ReleaseHeldResult{}, err
	}
	var claimed []domain.HeldUsageEvent
	for rows.Next() {
		var h domain.HeldUsageEvent
		if err := rows.Scan(&h.ID, &h.EventName, &h.Quantity, propertiesScanner{&h.Dimensions},
			&h.IdempotencyKey, &h.EventAt, &h.ObservedCostMicros); err != nil {
			_ = rows.Close()
			return ReleaseHeldResult{}, err
		}
		claimed = append(claimed, h)
	}
	if err := rows.Close(); err != nil {
		return ReleaseHeldResult{}, err
	}
	if err := rows.Err(); err != nil {
		return ReleaseHeldResult{}, err
	}

	res := ReleaseHeldResult{Rows: make([]ReleasedRow, 0, len(claimed))}
	for _, h := range claimed {
		row := ReleasedRow{HeldID: h.ID, EventName: h.EventName, EventAt: h.EventAt}
		if inputKey, isWrite := CallInputKey(h.IdempotencyKey); isWrite {
			// A LiteLLM cache-write row whose call usage_events already
			// bills the pre-split way (a copy of the previous release
			// wrote it after this one held the split): its tokens are
			// inside that :input row.
			billed, err := writesBilledInInput(ctx, tx, tenantID, inputKey)
			if err != nil {
				return ReleaseHeldResult{}, err
			}
			if billed {
				row.Deduplicated = true
				res.Rows = append(res.Rows, row)
				continue
			}
		}
		out, err := ingestOneTx(ctx, tx, tenantID, domain.UsageEvent{
			CustomerID:         in.CustomerID,
			MeterID:            in.MeterIDs[h.EventName],
			Quantity:           h.Quantity,
			Dimensions:         h.Dimensions,
			IdempotencyKey:     h.IdempotencyKey,
			Timestamp:          h.EventAt,
			Origin:             domain.UsageOriginBackfill,
			ProviderCostMicros: h.ObservedCostMicros,
		})
		switch {
		case errors.Is(err, errs.ErrDuplicateKey):
			row.Deduplicated = true
		case err != nil:
			return ReleaseHeldResult{}, fmt.Errorf("release held %s: %w", h.ID, err)
		default:
			row.EventID = out.ID
			if _, err := tx.ExecContext(ctx,
				`UPDATE usage_held_events SET released_event_id = $1 WHERE id = $2`, out.ID, h.ID); err != nil {
				return ReleaseHeldResult{}, err
			}
		}
		res.Rows = append(res.Rows, row)
	}

	// Split what is left: rows this release could have taken but for the
	// Limit (another release takes them) and rows it could not take.
	if err := tx.QueryRowContext(ctx, `
		SELECT
			COUNT(*) FILTER (WHERE event_name = ANY($3::text[]) AND event_at <= $4),
			COUNT(*) FILTER (WHERE NOT (event_name = ANY($3::text[]) AND event_at <= $4))
		FROM usage_held_events
		WHERE tenant_id = $1 AND status = 'held' AND external_customer_id = $2
	`, tenantID, in.ExternalCustomerID, postgres.StringArray(keys), in.NotAfter).Scan(&res.MoreToRelease, &res.StillHeld); err != nil {
		return ReleaseHeldResult{}, err
	}

	if emit != nil && len(res.Rows) > 0 {
		if err := emit(tx, res); err != nil {
			return ReleaseHeldResult{}, fmt.Errorf("release emission: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return ReleaseHeldResult{}, err
	}
	return res, nil
}

// DiscardHeld marks the held rows for one external customer id (and one
// event name, when given) discarded. The rows are kept as a record; they are
// never ingested.
func (s *PostgresStore) DiscardHeld(ctx context.Context, tenantID, externalCustomerID, eventName string, emit func(tx *sql.Tx, n int) error) (int, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return 0, err
	}
	defer postgres.Rollback(tx)

	res, err := tx.ExecContext(ctx, `
		UPDATE usage_held_events SET status = 'discarded', resolved_at = now()
		WHERE tenant_id = $1 AND status = 'held' AND external_customer_id = $2
		  AND ($3::text = '' OR event_name = $3::text)
	`, tenantID, externalCustomerID, eventName)
	if err != nil {
		return 0, err
	}
	n64, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	n := int(n64)
	if emit != nil && n > 0 {
		if err := emit(tx, n); err != nil {
			return 0, fmt.Errorf("audit emission: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

// IngestResolvingHeld writes one live LiteLLM event, in one transaction
// with the held row that has the same idempotency key, if any:
//
//   - discarded: nothing is written and errs.ErrDuplicateKey is returned.
//     The operator decided these rows are never billed, and a LiteLLM
//     resend after the customer exists must not undo that.
//   - held: the event is written (or found already written) and the held
//     row is marked released, so a later discard cannot mark a row
//     discarded whose usage is already in usage_events.
//   - released, or no held row: a plain ingest.
//
// SELECT ... FOR UPDATE takes the held row's lock, the same lock
// ReleaseHeld's and DiscardHeld's UPDATEs take, so whichever runs second
// sees the first one's status.
func (s *PostgresStore) IngestResolvingHeld(ctx context.Context, tenantID string, event domain.UsageEvent) (domain.UsageEvent, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.UsageEvent{}, err
	}
	defer postgres.Rollback(tx)

	var heldID, status string
	if event.IdempotencyKey != "" {
		err = tx.QueryRowContext(ctx, `
			SELECT id, status FROM usage_held_events
			WHERE tenant_id = $1 AND idempotency_key = $2
			FOR UPDATE
		`, tenantID, event.IdempotencyKey).Scan(&heldID, &status)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return domain.UsageEvent{}, err
		}
	}
	if status == string(domain.HeldStatusDiscarded) {
		return domain.UsageEvent{}, fmt.Errorf("%w: idempotency_key %q was held and discarded", errs.ErrDuplicateKey, event.IdempotencyKey)
	}

	out, ingestErr := ingestOneTx(ctx, tx, tenantID, event)
	if ingestErr != nil && !errors.Is(ingestErr, errs.ErrDuplicateKey) {
		return domain.UsageEvent{}, ingestErr
	}
	if status == string(domain.HeldStatusHeld) {
		// The event id is recorded only when this call wrote it; a
		// duplicate means usage_events already had the key.
		var eventID *string
		if ingestErr == nil {
			eventID = &out.ID
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE usage_held_events SET status = 'released', resolved_at = now(),
				released_customer_id = $2, released_event_id = $3
			WHERE id = $1 AND status = 'held'
		`, heldID, event.CustomerID, eventID); err != nil {
			return domain.UsageEvent{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return domain.UsageEvent{}, err
	}
	if ingestErr != nil {
		return domain.UsageEvent{}, ingestErr
	}
	return out, nil
}

// StoreCall is one LiteLLM call for IngestCallResolvingHeld: Events is how
// the current mapper records it, Legacy (with LegacyKey) how the mapper
// before the cache-write split did. See CallInput.
type StoreCall struct {
	Events    []domain.UsageEvent
	Legacy    []domain.UsageEvent
	LegacyKey string
}

// HeldCall is StoreCall for HoldCall.
type HeldCall struct {
	Events    []domain.HeldUsageEvent
	Legacy    []domain.HeldUsageEvent
	LegacyKey string
}

// heldKeyRow is a held row's state, read under its lock.
type heldKeyRow struct {
	id       string
	status   string
	quantity decimal.Decimal
}

// lockHeldKeys reads and locks the held rows with these keys, in key order
// so two calls sharing keys lock them in the same order. The lock is the one
// ReleaseHeld's and DiscardHeld's UPDATEs take, so whichever runs second
// sees the first one's status.
func lockHeldKeys(ctx context.Context, tx *sql.Tx, tenantID string, keys []string) (map[string]heldKeyRow, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT idempotency_key, id, status, quantity FROM usage_held_events
		WHERE tenant_id = $1 AND idempotency_key = ANY($2::text[])
		ORDER BY idempotency_key
		FOR UPDATE
	`, tenantID, postgres.StringArray(keys))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]heldKeyRow{}
	for rows.Next() {
		var k string
		var r heldKeyRow
		if err := rows.Scan(&k, &r.id, &r.status, &r.quantity); err != nil {
			return nil, err
		}
		out[k] = r
	}
	return out, rows.Err()
}

// storedEventQuantity is the quantity of the usage_events row with this key.
func storedEventQuantity(ctx context.Context, tx *sql.Tx, key string) (decimal.Decimal, bool, error) {
	var q decimal.Decimal
	err := tx.QueryRowContext(ctx, `SELECT quantity FROM usage_events WHERE idempotency_key = $1`, key).Scan(&q)
	if errors.Is(err, sql.ErrNoRows) {
		return decimal.Decimal{}, false, nil
	}
	return q, err == nil, err
}

// callKeys is every key of both mappings, deduplicated.
func callKeys(groups ...[]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, g := range groups {
		for _, k := range g {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	return out
}

// IngestCallResolvingHeld writes one LiteLLM call — every event the mapper
// made from it — in ONE transaction, so a crash or a store failure leaves
// all of the call or none of it, and a LiteLLM resend finds the call whole.
//
// Each key follows IngestResolvingHeld's rules: a key held and then
// discarded is not written (counted as deduplicated), a key still held is
// written and its held row marked released, a key usage_events already has
// is deduplicated. The held rows are read under FOR UPDATE first.
//
// The replay guard (when call.Legacy is set): the call's input key may
// already be stored by the mapper before the cache-write split, whose input
// quantity includes the writes. Writing the split mapping on top would add
// the write rows on new keys while :input dedups: the writes billed twice.
// So the input key decides, from the row already stored under it:
//
//   - a usage_events row decides whenever there is one: it is what bills.
//     With a held row for the key, that row's lock (taken above) is the one
//     every LiteLLM writer of the key takes first — this release, the
//     previous one's IngestResolvingHeld, ReleaseHeld's claim — so a plain
//     read now sees any such write that committed.
//   - with a held row and no usage_events row, the held row's quantity
//     decides (any status: a discarded row is never written).
//   - with neither, the split :input is INSERTed first. That insert is the
//     claim: ON CONFLICT on the (tenant_id, livemode, idempotency_key)
//     UNIQUE waits for an uncommitted insert of the same key — such as a
//     copy of the previous release writing the old :input during a rolling
//     deploy — and then sees it. A SELECT-then-INSERT would not see the
//     uncommitted row and would double-bill. On conflict the stored row's
//     quantity decides.
//
// A deciding quantity equal to the pre-split input quantity means the call
// was first recorded the old way: it is written as call.Legacy (no write
// rows, the old input quantity and cost), so every key of it dedups or
// releases its held row, and any held write row of the call is marked
// released with no event: its tokens are already billed inside :input. Any
// other quantity, or none, means the split mapping.
//
// A deadlock (another transaction locking the call's held rows in another
// order, such as ReleaseHeld's claim) aborts the transaction; it is run
// once more before the error reaches the caller.
func (s *PostgresStore) IngestCallResolvingHeld(ctx context.Context, tenantID string, call StoreCall) (CallResult, error) {
	res, err := s.ingestCallTx(ctx, tenantID, call)
	if postgres.IsDeadlock(err) {
		res, err = s.ingestCallTx(ctx, tenantID, call)
	}
	return res, err
}

func (s *PostgresStore) ingestCallTx(ctx context.Context, tenantID string, call StoreCall) (CallResult, error) {
	eventKey := func(e domain.UsageEvent) string { return e.IdempotencyKey }
	keysOf := func(evs []domain.UsageEvent) []string {
		out := make([]string, 0, len(evs))
		for _, e := range evs {
			out = append(out, e.IdempotencyKey)
		}
		return out
	}

	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return CallResult{}, err
	}
	defer postgres.Rollback(tx)

	held, err := lockHeldKeys(ctx, tx, tenantID, callKeys(keysOf(call.Events), keysOf(call.Legacy)))
	if err != nil {
		return CallResult{}, err
	}

	var res CallResult
	events := call.Events
	written := map[string]bool{}
	if len(call.Legacy) > 0 {
		legacy, ok := firstWithKey(call.Legacy, call.LegacyKey, eventKey)
		claim, okSplit := firstWithKey(call.Events, call.LegacyKey, eventKey)
		if !ok || !okSplit {
			return CallResult{}, fmt.Errorf("usage: call legacy key %q is not in both mappings", call.LegacyKey)
		}
		var prior *decimal.Decimal
		if h, isHeld := held[call.LegacyKey]; isHeld {
			q, found, err := storedEventQuantity(ctx, tx, call.LegacyKey)
			if err != nil {
				return CallResult{}, err
			}
			if found {
				prior = &q
			} else {
				prior = &h.quantity
			}
		} else {
			_, err := ingestOneTx(ctx, tx, tenantID, claim)
			switch {
			case err == nil:
				res.Inserted++
				written[call.LegacyKey] = true
			case errors.Is(err, errs.ErrDuplicateKey):
				q, found, err := storedEventQuantity(ctx, tx, call.LegacyKey)
				if err != nil {
					return CallResult{}, err
				}
				if found {
					prior = &q
				}
			default:
				return CallResult{}, err
			}
		}
		if prior != nil && prior.Equal(legacy.Quantity) {
			events = call.Legacy
			res.Legacy = true
			// The split-only keys (the cache writes) are inside :input now.
			// A held row of one — the call held split, then written the old
			// way — is resolved, so no release bills it on top.
			inLegacy := map[string]bool{}
			for _, k := range keysOf(call.Legacy) {
				inLegacy[k] = true
			}
			for _, ev := range call.Events {
				h, isHeld := held[ev.IdempotencyKey]
				if inLegacy[ev.IdempotencyKey] || !isHeld || h.status != string(domain.HeldStatusHeld) {
					continue
				}
				if err := markHeldReleased(ctx, tx, h.id, ev.CustomerID, nil); err != nil {
					return CallResult{}, err
				}
			}
		}
	}

	for i, ev := range events {
		if written[ev.IdempotencyKey] {
			// The claim: the split :input, written above.
			res.Written = append(res.Written, i)
			continue
		}
		h, isHeld := held[ev.IdempotencyKey]
		if isHeld && h.status == string(domain.HeldStatusDiscarded) {
			// The operator decided these rows are never billed, and a
			// LiteLLM resend after the customer exists must not undo that.
			res.Deduplicated++
			continue
		}
		out, err := ingestOneTx(ctx, tx, tenantID, ev)
		switch {
		case err == nil:
			res.Inserted++
			res.Written = append(res.Written, i)
		case errors.Is(err, errs.ErrDuplicateKey):
			res.Deduplicated++
		default:
			return CallResult{}, err
		}
		if isHeld && h.status == string(domain.HeldStatusHeld) {
			// The event id is recorded only when this call wrote it; a
			// duplicate means usage_events already had the key.
			var eventID *string
			if err == nil {
				eventID = &out.ID
			}
			if err := markHeldReleased(ctx, tx, h.id, ev.CustomerID, eventID); err != nil {
				return CallResult{}, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return CallResult{}, err
	}
	return res, nil
}

// markHeldReleased marks one held row released for this customer. eventID
// is the usage_events row written for it, or nil when usage_events already
// bills it.
func markHeldReleased(ctx context.Context, tx *sql.Tx, heldID, customerID string, eventID *string) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE usage_held_events SET status = 'released', resolved_at = now(),
			released_customer_id = $2, released_event_id = $3
		WHERE id = $1 AND status = 'held'
	`, heldID, customerID, eventID)
	return err
}

// writesBilledInInput reports whether a held cache-write row's call is
// already billed the pre-split way: usage_events has the call's :input at a
// quantity other than the call's held :input, so that row includes the
// writes. The held :input is locked first: every LiteLLM writer of the
// input key takes that lock before writing it (see
// IngestCallResolvingHeld), so the read sees any such write that committed.
// No held :input (never the case for a call held whole) means no answer:
// false.
func writesBilledInInput(ctx context.Context, tx *sql.Tx, tenantID, inputKey string) (bool, error) {
	var heldQty decimal.Decimal
	err := tx.QueryRowContext(ctx, `
		SELECT quantity FROM usage_held_events
		WHERE tenant_id = $1 AND idempotency_key = $2
		FOR UPDATE
	`, tenantID, inputKey).Scan(&heldQty)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	q, found, err := storedEventQuantity(ctx, tx, inputKey)
	if err != nil || !found {
		return false, err
	}
	return !q.Equal(heldQty), nil
}

// HoldCall holds one LiteLLM call — every event the mapper made from it — in
// ONE transaction, ON CONFLICT DO NOTHING per key, so a resend of a call
// already held is counted as deduplicated. It applies
// IngestCallResolvingHeld's replay guard, with the claim on the held
// table: the call's input key is looked up in usage_events (a customer
// deleted and recreated under the same external id), and otherwise the
// split :input is held first. That insert waits for an uncommitted hold of
// the same key, and on conflict the held row's quantity (any status)
// decides. A stored pre-split input quantity holds the call as call.Legacy,
// so the held list shows no write rows for it.
//
// Held write rows can still meet a pre-split :input: this release holds
// the call split, then a copy of the previous release writes the old
// :input once the customer exists (a rolling deploy, a rollback, a replay
// it serves). Billing is guarded where the write rows would be written:
// ReleaseHeld and IngestCallResolvingHeld read the stored :input and bill
// no write row on top of it. Only a release run by the previous release's
// code would; the CHANGELOG says not to release held LiteLLM usage while
// copies of the previous release serve.
//
// HoldCall takes no row locks (its inserts wait only on an uncommitted
// insert of the same key, and every hold of a call inserts :input first),
// so unlike IngestCallResolvingHeld it cannot deadlock and is not retried.
func (s *PostgresStore) HoldCall(ctx context.Context, tenantID string, call HeldCall) (CallResult, error) {
	heldKey := func(e domain.HeldUsageEvent) string { return e.IdempotencyKey }

	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return CallResult{}, err
	}
	defer postgres.Rollback(tx)

	var res CallResult
	events := call.Events
	written := map[string]bool{}
	if len(call.Legacy) > 0 {
		legacy, ok := firstWithKey(call.Legacy, call.LegacyKey, heldKey)
		claim, okSplit := firstWithKey(call.Events, call.LegacyKey, heldKey)
		if !ok || !okSplit {
			return CallResult{}, fmt.Errorf("usage: call legacy key %q is not in both mappings", call.LegacyKey)
		}
		var prior *decimal.Decimal
		if q, found, err := storedEventQuantity(ctx, tx, call.LegacyKey); err != nil {
			return CallResult{}, err
		} else if found {
			prior = &q
		} else {
			inserted, err := insertHeldTx(ctx, tx, tenantID, claim)
			if err != nil {
				return CallResult{}, err
			}
			if inserted {
				res.Inserted++
				written[call.LegacyKey] = true
			} else {
				var q decimal.Decimal
				if err := tx.QueryRowContext(ctx,
					`SELECT quantity FROM usage_held_events WHERE tenant_id = $1 AND idempotency_key = $2`, tenantID, call.LegacyKey).Scan(&q); err != nil {
					return CallResult{}, err
				}
				prior = &q
			}
		}
		if prior != nil && prior.Equal(legacy.Quantity) {
			events = call.Legacy
			res.Legacy = true
		}
	}

	for _, ev := range events {
		if written[ev.IdempotencyKey] {
			continue
		}
		inserted, err := insertHeldTx(ctx, tx, tenantID, ev)
		if err != nil {
			return CallResult{}, err
		}
		if inserted {
			res.Inserted++
		} else {
			res.Deduplicated++
		}
	}
	if err := tx.Commit(); err != nil {
		return CallResult{}, err
	}
	return res, nil
}

// firstWithKey is the event with this key.
func firstWithKey[E any](events []E, key string, keyOf func(E) string) (E, bool) {
	for _, e := range events {
		if keyOf(e) == key {
			return e, true
		}
	}
	var zero E
	return zero, false
}

// insertHeldTx inserts one held row, ON CONFLICT DO NOTHING on the
// idempotency key. It reports whether the row was new.
func insertHeldTx(ctx context.Context, tx *sql.Tx, tenantID string, ev domain.HeldUsageEvent) (bool, error) {
	props, err := propertiesJSON(ev.Dimensions)
	if err != nil {
		return false, err
	}
	var id string
	err = tx.QueryRowContext(ctx, `
		INSERT INTO usage_held_events (id, tenant_id, source, external_customer_id,
			event_name, quantity, properties, idempotency_key, event_at,
			observed_cost_micros, reason)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (tenant_id, livemode, idempotency_key) DO NOTHING
		RETURNING id
	`, postgres.NewID("vlx_hue"), tenantID, ev.Source, ev.ExternalCustomerID,
		ev.EventName, ev.Quantity, props, ev.IdempotencyKey, ev.EventAt,
		ev.ObservedCostMicros, string(ev.Reason),
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}
