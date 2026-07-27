package subscription

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

type PostgresStore struct {
	db     *postgres.DB
	outbox OutboxEnqueuer
}

func NewPostgresStore(db *postgres.DB) *PostgresStore {
	return &PostgresStore{db: db}
}

// OutboxEnqueuer enqueues an outbound webhook event inside the caller's tx,
// so the event is persisted atomically with the state change (ADR-040
// transactional outbox). Satisfied by *webhook.OutboxStore; declared
// consumer-side so this store needs no webhook import. Same seam as the
// invoice (invoice.paid / payment.succeeded) and credit (balance crossings)
// stores.
type OutboxEnqueuer interface {
	Enqueue(ctx context.Context, tx *sql.Tx, tenantID, eventType string, payload map[string]any) (string, error)
}

// SetOutboxEnqueuer wires transactional lifecycle events (2026-07-05,
// DispatchTx-seam subscription subset): subscription.created / .activated /
// .canceled / .trial_ended are enqueued IN the transition tx, so a crash in
// the old commit→dispatch window can no longer silently drop them — a
// dropped emission left no row anywhere (nothing to replay, nothing to
// reconcile). Optional — when unset (narrow tests), no events are enqueued.
func (s *PostgresStore) SetOutboxEnqueuer(o OutboxEnqueuer) { s.outbox = o }

// lifecyclePayload is the shared event payload base — mirrors the shape the
// handler's fireEvent historically emitted so consumers see no contract
// change: subscription_id, customer_id, status, item_count, period bounds.
func lifecyclePayload(sub domain.Subscription, extra map[string]any) map[string]any {
	payload := map[string]any{
		"subscription_id": sub.ID,
		"customer_id":     sub.CustomerID,
		"status":          string(sub.Status),
		"item_count":      len(sub.Items),
	}
	if sub.CurrentBillingPeriodStart != nil {
		payload["current_period_start"] = sub.CurrentBillingPeriodStart.UTC()
	}
	if sub.CurrentBillingPeriodEnd != nil {
		payload["current_period_end"] = sub.CurrentBillingPeriodEnd.UTC()
	}
	for k, v := range extra {
		payload[k] = v
	}
	return payload
}

// enqueueLifecycle enqueues one lifecycle event on the caller's tx. An
// enqueue failure fails the transition — atomicity with the state change is
// the entire point of the seam (the pre-fix post-commit dispatch could drop
// the event with no trace). No-op when the enqueuer isn't wired.
func (s *PostgresStore) enqueueLifecycle(ctx context.Context, tx *sql.Tx, eventType string, sub domain.Subscription, extra map[string]any) error {
	if s.outbox == nil {
		return nil
	}
	if _, err := s.outbox.Enqueue(ctx, tx, sub.TenantID, eventType, lifecyclePayload(sub, extra)); err != nil {
		return fmt.Errorf("enqueue %s: %w", eventType, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Column sets
// ---------------------------------------------------------------------------

const subCols = `id, tenant_id, code, display_name, customer_id, status, billing_time,
	trial_start_at, trial_end_at, started_at, activated_at, canceled_at,
	cancel_at, COALESCE(cancel_at_period_end, false),
	pause_collection_behavior, pause_collection_resumes_at,
	billing_threshold_amount_gte, COALESCE(billing_threshold_reset_cycle, true),
	current_billing_period_start, current_billing_period_end, next_billing_at,
	COALESCE(billing_anchor_day, 0),
	usage_cap_units, COALESCE(overage_action,'charge'),
	COALESCE(test_clock_id,''),
	created_at, updated_at`

const itemCols = `id, tenant_id, subscription_id, plan_id, quantity, metadata,
	COALESCE(pending_plan_id,''), pending_plan_effective_at, plan_changed_at,
	created_at, updated_at`

// ---------------------------------------------------------------------------
// Subscription CRUD
// ---------------------------------------------------------------------------

func (s *PostgresStore) Create(ctx context.Context, tenantID string, sub domain.Subscription) (domain.Subscription, error) {
	return s.CreateWithBill(ctx, tenantID, sub, nil)
}

// CreateWithBill inserts the subscription (+ items) AND runs billFn — the
// day-1 in_advance invoice insert — in the SAME transaction, so a billing
// failure rolls the whole create back instead of silently leaving an active
// subscription with no first-period invoice. That matters because the cycle
// scheduler bills the UPCOMING period and SKIPS the just-elapsed in_advance
// segment, so a dropped day-1 invoice is a permanent revenue leak, not a
// "deferred to next cycle close" one. ADR-056 coordinator pattern; the
// external steps (tax commit + auto-charge) run post-commit via the caller's
// FinalizeOnCreateInvoice. billFn may be nil (a plain create).
func (s *PostgresStore) CreateWithBill(ctx context.Context, tenantID string, sub domain.Subscription, billFn func(tx *sql.Tx, created domain.Subscription) error) (domain.Subscription, error) {
	if len(sub.Items) == 0 {
		return domain.Subscription{}, errs.Invalid("items", "a subscription must have at least one item")
	}

	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.Subscription{}, err
	}
	defer postgres.Rollback(tx)

	created, err := s.createInTx(ctx, tx, tenantID, sub)
	if err != nil {
		return domain.Subscription{}, err
	}

	if billFn != nil {
		if err := billFn(tx, created); err != nil {
			return domain.Subscription{}, err
		}
	}

	if err := tx.Commit(); err != nil {
		return domain.Subscription{}, err
	}
	return created, nil
}

// createInTx inserts the subscription row and its items on the given tx (no
// commit). Shared by Create / CreateWithBill so the day-1 invoice can join the
// same transaction.
func (s *PostgresStore) createInTx(ctx context.Context, tx *sql.Tx, tenantID string, sub domain.Subscription) (domain.Subscription, error) {
	id := postgres.NewID("vlx_sub")
	now := clock.Now(ctx)

	err := scanSubRow(tx.QueryRowContext(ctx, `
		INSERT INTO subscriptions (id, tenant_id, code, display_name, customer_id, status,
			billing_time, trial_start_at, trial_end_at, started_at,
			current_billing_period_start, current_billing_period_end, next_billing_at,
			billing_anchor_day,
			usage_cap_units, overage_action, test_clock_id,
			activated_at,
			created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,COALESCE(NULLIF($16,''),'charge'),NULLIF($17,''),$18,$19,$19)
		RETURNING `+subCols,
		id, tenantID, sub.Code, sub.DisplayName, sub.CustomerID,
		sub.Status, sub.BillingTime, postgres.NullableTime(sub.TrialStartAt),
		postgres.NullableTime(sub.TrialEndAt), postgres.NullableTime(sub.StartedAt),
		postgres.NullableTime(sub.CurrentBillingPeriodStart),
		postgres.NullableTime(sub.CurrentBillingPeriodEnd),
		postgres.NullableTime(sub.NextBillingAt),
		sub.BillingAnchorDay,
		sub.UsageCapUnits, sub.OverageAction, sub.TestClockID,
		postgres.NullableTime(sub.ActivatedAt), now,
	), &sub)

	if err != nil {
		if postgres.UniqueViolationConstraint(err) != "" {
			return domain.Subscription{}, errs.AlreadyExists("code",
				fmt.Sprintf("subscription code %q already exists", sub.Code))
		}
		return domain.Subscription{}, err
	}

	// Insert each requested item in the same tx. The UNIQUE (subscription_id,
	// plan_id) constraint rejects duplicate plans in the same request.
	inserted := make([]domain.SubscriptionItem, 0, len(sub.Items))
	for _, it := range sub.Items {
		qty := it.Quantity
		if qty <= 0 {
			qty = 1
		}
		var stored domain.SubscriptionItem
		err := tx.QueryRowContext(ctx, `
			INSERT INTO subscription_items (tenant_id, subscription_id, plan_id, quantity, metadata, created_at, updated_at)
			VALUES ($1,$2,$3,$4,COALESCE(NULLIF($5,'')::jsonb,'{}'::jsonb),$6,$6)
			RETURNING `+itemCols,
			tenantID, sub.ID, it.PlanID, qty, string(it.Metadata), now,
		).Scan(scanItemDest(&stored)...)
		if err != nil {
			if postgres.UniqueViolationConstraint(err) != "" {
				return domain.Subscription{}, errs.AlreadyExists("plan_id",
					fmt.Sprintf("duplicate plan %q in subscription items", it.PlanID))
			}
			return domain.Subscription{}, fmt.Errorf("insert item: %w", err)
		}
		inserted = append(inserted, stored)
	}
	sub.Items = inserted

	// ADR-101: a StartNow create IS the items' billable open — one interval
	// per item at the (already day-snapped) period start, same tx. Draft and
	// trialing creates open nothing; their activation writer owns it.
	if sub.Status == domain.SubscriptionActive {
		if err := intervalOpenForItems(ctx, tx, sub, "create"); err != nil {
			return domain.Subscription{}, err
		}
	}

	// subscription.created rides the create tx (DispatchTx subscription
	// subset, 2026-07-05) — durable iff the create commits.
	if err := s.enqueueLifecycle(ctx, tx, domain.EventSubscriptionCreated, sub, nil); err != nil {
		return domain.Subscription{}, err
	}
	return sub, nil
}

func (s *PostgresStore) Get(ctx context.Context, tenantID, id string) (domain.Subscription, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.Subscription{}, err
	}
	defer postgres.Rollback(tx)

	var sub domain.Subscription
	err = scanSubRow(tx.QueryRowContext(ctx, `SELECT `+subCols+` FROM subscriptions WHERE id = $1`, id), &sub)
	if err == sql.ErrNoRows {
		return domain.Subscription{}, errs.ErrNotFound
	}
	if err != nil {
		return domain.Subscription{}, err
	}

	if err := hydrateSubChildrenTx(ctx, tx, &sub); err != nil {
		return domain.Subscription{}, err
	}
	return sub, nil
}

func (s *PostgresStore) List(ctx context.Context, filter ListFilter) ([]domain.Subscription, int, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, filter.TenantID)
	if err != nil {
		return nil, 0, err
	}
	defer postgres.Rollback(tx)

	// Default 50, clamp to 100 — no-silent-fallbacks principle.
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	} else if limit > 100 {
		limit = 100
	}

	where, args := buildSubWhere(filter)

	var total int
	// Plan filter needs DISTINCT so a subscription with N items matching the
	// plan isn't counted N times. The JOIN is omitted (via buildSubWhere) when
	// PlanID isn't set, so the common list path still runs without the join.
	countQuery := `SELECT COUNT(DISTINCT s.id) FROM subscriptions s` + where
	if err := tx.QueryRowContext(ctx, countQuery, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	query := `SELECT DISTINCT ` + qualifiedSubCols("s") + ` FROM subscriptions s` + where +
		` ORDER BY ` + subscriptionOrderBy(filter.Sort, filter.SortDir) +
		` LIMIT $` + fmt.Sprintf("%d", len(args)+1) + ` OFFSET $` + fmt.Sprintf("%d", len(args)+2)
	args = append(args, limit, filter.Offset)

	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()

	var subs []domain.Subscription
	for rows.Next() {
		var sub domain.Subscription
		if err := scanSubRow(rows, &sub); err != nil {
			return nil, 0, err
		}
		subs = append(subs, sub)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	// Hydrate items for each subscription. A second query per subscription is
	// acceptable on the list path at the default 50-row page size; if list
	// growth becomes hot, batch this into one IN() query.
	for i := range subs {
		if err := hydrateSubChildrenTx(ctx, tx, &subs[i]); err != nil {
			return nil, 0, err
		}
	}
	return subs, total, nil
}

// CancelAtomic terminates a subscription. Cancellation is allowed from
// every non-terminal status — draft (operator scrapping a never-activated
// row), trialing (customer abandoning during trial — by far the dominant
// industry cancel path; Stripe / Lago / Recurly / Chargebee all allow it),
// and active. Only canceled/archived are rejected (the row already
// terminated). Note: the `paused` source state was removed in PR-8
// when the hard-pause API was deleted — no path now produces
// status='paused', so it's not in allowedFrom.
func cancelSpec() transitionSpec {
	return transitionSpec{
		targetStatus: string(domain.SubscriptionCanceled),
		allowedFrom: []string{
			string(domain.SubscriptionDraft),
			string(domain.SubscriptionTrialing),
			string(domain.SubscriptionActive),
		},
		setCanceledAt: true,
		wrongStateMsg: "cannot cancel %s subscription (already terminated)",
	}
}

func (s *PostgresStore) CancelAtomic(ctx context.Context, tenantID, id string) (domain.Subscription, error) {
	return s.transitionAtomic(ctx, tenantID, id, cancelSpec())
}

// CancelAtomicWithBill cancels the subscription AND runs billFn — the
// final-on-cancel partial-period invoice insert — in the SAME transaction, so a
// billing failure rolls the cancel back rather than leaving a canceled sub with
// an uninvoiced partial period (a revenue leak: there is no final-on-cancel
// reconciler). ADR-056 coordinator pattern. The external finalize (tax commit +
// auto-charge) and the in_advance proration credit run post-commit via the
// caller (FinalizeOnCreateInvoice / BillOnCancel). billFn may be nil.
func (s *PostgresStore) CancelAtomicWithBill(ctx context.Context, tenantID, id string, billFn func(tx *sql.Tx, canceled domain.Subscription) error) (domain.Subscription, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.Subscription{}, err
	}
	defer postgres.Rollback(tx)

	canceled, err := s.transitionInTx(ctx, tx, id, cancelSpec(), clock.Now(ctx))
	if err != nil {
		return domain.Subscription{}, err
	}
	if billFn != nil {
		if err := billFn(tx, canceled); err != nil {
			return domain.Subscription{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return domain.Subscription{}, err
	}
	return canceled, nil
}

// ScheduleCancellation persists the soft-cancel intent. The schedule fires
// through TWO consumers (ADR-069): the billing cycle scan at period
// boundaries for ACTIVE subs, and the trial-end guard/CancelAtTrialEnd for
// TRIALING subs (a free, no-invoice cancel at trial_end_at). Returns the
// updated subscription with hydrated items so the handler can echo the
// same shape it returns elsewhere.
//
// The UPDATE CAS-es on observedStatus — the status the caller validated the
// intent under — because the flag's MEANING is status-polymorphic: landing
// a "free at trial end" intent on a sub an activation writer just flipped
// would silently turn it into a paid-period cancel. Status drift returns
// InvalidState (409, re-read). Paused subs remain schedulable (Stripe
// parity); canceled/archived are rejected — nothing to schedule once
// terminated.
func (s *PostgresStore) ScheduleCancellation(ctx context.Context, tenantID, id string, cancelAt *time.Time, cancelAtPeriodEnd bool, observedStatus domain.SubscriptionStatus) (domain.Subscription, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.Subscription{}, err
	}
	defer postgres.Rollback(tx)

	now := clock.Now(ctx)
	var sub domain.Subscription
	// CAS on the status the caller VALIDATED the intent under (ADR-069): the
	// schedule flag is status-polymorphic — on a trialing sub AtPeriodEnd
	// means "cancel free at trial end", on an active sub it means "cancel at
	// paid-period end, charged". Landing the flag on a sub an activation
	// writer just flipped would silently invert the promise the operator was
	// shown. Zero rows with a live row = state changed → 409, re-read.
	err = scanSubRow(tx.QueryRowContext(ctx, `
		UPDATE subscriptions
		SET cancel_at = $1, cancel_at_period_end = $2, updated_at = $3
		WHERE id = $4 AND status = $5 AND status NOT IN ('canceled','archived')
		RETURNING `+subCols,
		postgres.NullableTime(cancelAt), cancelAtPeriodEnd, now, id, observedStatus,
	), &sub)
	if err == sql.ErrNoRows {
		var currentStatus string
		err2 := tx.QueryRowContext(ctx, `SELECT status FROM subscriptions WHERE id = $1`, id).Scan(&currentStatus)
		if err2 == sql.ErrNoRows {
			return domain.Subscription{}, errs.ErrNotFound
		}
		if err2 != nil {
			return domain.Subscription{}, err2
		}
		if currentStatus != string(observedStatus) {
			return domain.Subscription{}, errs.InvalidState(fmt.Sprintf(
				"subscription changed from %s to %s while scheduling — re-read and retry (the cancel's meaning depends on status)",
				observedStatus, currentStatus))
		}
		return domain.Subscription{}, errs.InvalidState(fmt.Sprintf("cannot schedule cancellation on %s subscription", currentStatus))
	}
	if err != nil {
		return domain.Subscription{}, err
	}

	if err := hydrateSubChildrenTx(ctx, tx, &sub); err != nil {
		return domain.Subscription{}, err
	}

	if err := tx.Commit(); err != nil {
		return domain.Subscription{}, err
	}
	return sub, nil
}

// ClearScheduledCancellation undoes a prior schedule. Idempotent — a row
// with both fields already cleared returns unchanged. Returns errs.ErrNotFound
// if the subscription doesn't exist; status is not checked because clearing
// a schedule on a canceled sub would be a no-op anyway and surfacing
// not-found there would mask the real "you already canceled" state.
func (s *PostgresStore) ClearScheduledCancellation(ctx context.Context, tenantID, id string) (domain.Subscription, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.Subscription{}, err
	}
	defer postgres.Rollback(tx)

	now := clock.Now(ctx)
	var sub domain.Subscription
	err = scanSubRow(tx.QueryRowContext(ctx, `
		UPDATE subscriptions
		SET cancel_at = NULL, cancel_at_period_end = false, updated_at = $1
		WHERE id = $2
		RETURNING `+subCols,
		now, id,
	), &sub)
	if err == sql.ErrNoRows {
		return domain.Subscription{}, errs.ErrNotFound
	}
	if err != nil {
		return domain.Subscription{}, err
	}

	if err := hydrateSubChildrenTx(ctx, tx, &sub); err != nil {
		return domain.Subscription{}, err
	}

	if err := tx.Commit(); err != nil {
		return domain.Subscription{}, err
	}
	return sub, nil
}

// FireScheduledCancellation transitions a subscription with a due cancel
// schedule to canceled in one statement. Differs from CancelAtomic in that
// (a) it accepts the engine's effectiveNow as the canceled_at timestamp so
// the audit trail stays consistent under test clocks, and (b) it clears
// the schedule fields so a subsequent cycle tick is a no-op rather than a
// confusing re-fire attempt. Returns errs.ErrNotFound if the row vanished
// or InvalidState if status was not active by the time the UPDATE ran (a
// concurrent immediate-cancel API call winning the race).
func (s *PostgresStore) FireScheduledCancellation(ctx context.Context, tenantID, id string, at time.Time) (domain.Subscription, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.Subscription{}, err
	}
	defer postgres.Rollback(tx)

	var sub domain.Subscription
	err = scanSubRow(tx.QueryRowContext(ctx, `
		UPDATE subscriptions
		SET status = 'canceled',
		    canceled_at = $1,
		    cancel_at = NULL,
		    cancel_at_period_end = false,
		    updated_at = $1
		WHERE id = $2 AND status = 'active'
		RETURNING `+subCols,
		at, id,
	), &sub)
	if err == sql.ErrNoRows {
		var currentStatus string
		err2 := tx.QueryRowContext(ctx, `SELECT status FROM subscriptions WHERE id = $1`, id).Scan(&currentStatus)
		if err2 == sql.ErrNoRows {
			return domain.Subscription{}, errs.ErrNotFound
		}
		if err2 != nil {
			return domain.Subscription{}, err2
		}
		return domain.Subscription{}, errs.InvalidState(fmt.Sprintf("scheduled cancel cannot fire on %s subscription", currentStatus))
	}
	if err != nil {
		return domain.Subscription{}, err
	}

	if err := hydrateSubChildrenTx(ctx, tx, &sub); err != nil {
		return domain.Subscription{}, err
	}

	// ADR-101: the scheduled fire seals every open interval at the
	// contracted instant, capped at the period end it fires against.
	if err := intervalCloseAllForCancel(ctx, tx, sub, at); err != nil {
		return domain.Subscription{}, err
	}

	// Schedule-driven cancel — enqueue in-tx with the schedule provenance
	// the engine's post-commit dispatch historically stamped.
	if err := s.enqueueLifecycle(ctx, tx, domain.EventSubscriptionCanceled, sub, map[string]any{
		"canceled_at": at.UTC(),
		"canceled_by": "schedule",
	}); err != nil {
		return domain.Subscription{}, err
	}

	if err := tx.Commit(); err != nil {
		return domain.Subscription{}, err
	}
	return sub, nil
}

// FireScheduledCancellationWithBill is the ADR-097 mid-period variant of
// FireScheduledCancellation: it flips the sub AND runs billFn (final
// partial-period invoice + cancel-proration drafts) in the SAME tx, the
// CancelAtomicWithBill shape — a billing failure rolls the flip back, and
// the relief drafts (which have no idempotency key of their own) are only
// ever written behind this CAS.
//
// The CAS matches cancel_at = $at as well as status: the fire must lose to
// a concurrent UnscheduleCancel (cancel_at NULLed) or a re-schedule to a
// different instant, not silently override the operator's newer intent.
// Zero rows disambiguate to ErrNotFound (row gone) or InvalidState (status
// changed, or the schedule changed under us) — callers treat InvalidState
// as a benign no-op: whichever writer won has already put the sub in a
// state the scans resolve (canceled drops out entirely; unscheduled drops
// out of the cancel arm).
//
// Timestamp split (ADR-097 §3): canceled_at = at (the contracted instant,
// possibly months in the past for the remediation cohort); updated_at =
// clock.Now(ctx) (operational time — never rewound).
func (s *PostgresStore) FireScheduledCancellationWithBill(ctx context.Context, tenantID, id string, at time.Time, billFn func(tx *sql.Tx, canceled domain.Subscription) error) (domain.Subscription, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.Subscription{}, err
	}
	defer postgres.Rollback(tx)

	var sub domain.Subscription
	err = scanSubRow(tx.QueryRowContext(ctx, `
		UPDATE subscriptions
		SET status = 'canceled',
		    canceled_at = $1,
		    cancel_at = NULL,
		    cancel_at_period_end = false,
		    updated_at = $2
		WHERE id = $3 AND status = 'active' AND cancel_at = $1
		RETURNING `+subCols,
		at, clock.Now(ctx), id,
	), &sub)
	if err == sql.ErrNoRows {
		var currentStatus string
		err2 := tx.QueryRowContext(ctx, `SELECT status FROM subscriptions WHERE id = $1`, id).Scan(&currentStatus)
		if err2 == sql.ErrNoRows {
			return domain.Subscription{}, errs.ErrNotFound
		}
		if err2 != nil {
			return domain.Subscription{}, err2
		}
		if currentStatus != string(domain.SubscriptionActive) {
			return domain.Subscription{}, errs.InvalidState(fmt.Sprintf("scheduled cancel cannot fire on %s subscription", currentStatus))
		}
		return domain.Subscription{}, errs.InvalidState("scheduled cancel no longer current (unscheduled or re-scheduled concurrently)")
	}
	if err != nil {
		return domain.Subscription{}, err
	}

	if err := hydrateSubChildrenTx(ctx, tx, &sub); err != nil {
		return domain.Subscription{}, err
	}

	// ADR-101: seal every open interval at the contracted instant (capped
	// at period end; a months-past ADR-097 remediation instant zero-widths
	// the current interval rather than rewriting billed history).
	if err := intervalCloseAllForCancel(ctx, tx, sub, at); err != nil {
		return domain.Subscription{}, err
	}

	if billFn != nil {
		if err := billFn(tx, sub); err != nil {
			return domain.Subscription{}, err
		}
	}

	if err := s.enqueueLifecycle(ctx, tx, domain.EventSubscriptionCanceled, sub, map[string]any{
		"canceled_at": at.UTC(),
		"canceled_by": "schedule",
	}); err != nil {
		return domain.Subscription{}, err
	}

	if err := tx.Commit(); err != nil {
		return domain.Subscription{}, err
	}
	return sub, nil
}

// SetPauseCollection writes (behavior, resumes_at) onto the row. Rejects
// rows in canceled/archived since collection-pause on a terminated sub has
// no meaning — the engine wouldn't observe the row anyway, but failing
// loudly here keeps the API honest. Active and paused (hard) are both
// allowed: a hard-paused sub can simultaneously have pause_collection
// configured for the moment status flips back to active.
func (s *PostgresStore) SetPauseCollection(ctx context.Context, tenantID, id string, pc domain.PauseCollection) (domain.Subscription, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.Subscription{}, err
	}
	defer postgres.Rollback(tx)

	now := clock.Now(ctx)
	var sub domain.Subscription
	err = scanSubRow(tx.QueryRowContext(ctx, `
		UPDATE subscriptions
		SET pause_collection_behavior = $1,
		    pause_collection_resumes_at = $2,
		    updated_at = $3
		WHERE id = $4 AND status NOT IN ('canceled','archived')
		RETURNING `+subCols,
		string(pc.Behavior), postgres.NullableTime(pc.ResumesAt), now, id,
	), &sub)
	if err == sql.ErrNoRows {
		var currentStatus string
		err2 := tx.QueryRowContext(ctx, `SELECT status FROM subscriptions WHERE id = $1`, id).Scan(&currentStatus)
		if err2 == sql.ErrNoRows {
			return domain.Subscription{}, errs.ErrNotFound
		}
		if err2 != nil {
			return domain.Subscription{}, err2
		}
		return domain.Subscription{}, errs.InvalidState(fmt.Sprintf("cannot pause collection on %s subscription", currentStatus))
	}
	if err != nil {
		return domain.Subscription{}, err
	}

	if err := hydrateSubChildrenTx(ctx, tx, &sub); err != nil {
		return domain.Subscription{}, err
	}

	if err := tx.Commit(); err != nil {
		return domain.Subscription{}, err
	}
	return sub, nil
}

// ClearPauseCollection nulls both pause_collection_* columns. Idempotent —
// runs even on a row that already has them null, returning the unchanged
// subscription. Returns errs.ErrNotFound if the row doesn't exist; status
// is not checked because clearing a no-op pause on a terminated sub is
// itself a no-op.
func (s *PostgresStore) ClearPauseCollection(ctx context.Context, tenantID, id string) (domain.Subscription, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.Subscription{}, err
	}
	defer postgres.Rollback(tx)

	now := clock.Now(ctx)
	var sub domain.Subscription
	err = scanSubRow(tx.QueryRowContext(ctx, `
		UPDATE subscriptions
		SET pause_collection_behavior = NULL,
		    pause_collection_resumes_at = NULL,
		    updated_at = $1
		WHERE id = $2
		RETURNING `+subCols,
		now, id,
	), &sub)
	if err == sql.ErrNoRows {
		return domain.Subscription{}, errs.ErrNotFound
	}
	if err != nil {
		return domain.Subscription{}, err
	}

	if err := hydrateSubChildrenTx(ctx, tx, &sub); err != nil {
		return domain.Subscription{}, err
	}

	if err := tx.Commit(); err != nil {
		return domain.Subscription{}, err
	}
	return sub, nil
}

// ActivateAfterTrial flips status 'trialing' → 'active' atomically. Sets
// activated_at = at if currently NULL (preserves the original activation
// timestamp on re-runs). Used by the billing engine when the trial window
// has elapsed during a cycle scan, and by the operator-facing EndTrial
// service action. Returns errs.InvalidState if the row's status was not
// 'trialing' at UPDATE time (e.g. it was already canceled or hard-paused);
// the caller distinguishes this from missing-row by querying current
// status when no row matches.
func (s *PostgresStore) ActivateAfterTrial(ctx context.Context, tenantID, id string, at time.Time) (domain.Subscription, error) {
	return s.ActivateAfterTrialWithBill(ctx, tenantID, id, at, nil)
}

// ActivateAfterTrialWithBill flips 'trialing' → 'active' AND runs billFn (the
// day-1 in_advance invoice insert) in the SAME transaction, so a billing
// failure rolls the activation back rather than leaving an active sub with no
// first-period invoice (the cycle scheduler skips the just-elapsed in_advance
// segment, so a lost day-1 invoice is a permanent revenue leak). ADR-056.
// billFn may be nil.
// ActivateDraftWithBill — see the Store interface doc. Draft→active flip +
// period/anchor stamps + day-1 bill in one tx (the ADR-056 pattern applied to
// the plain-draft path).
func (s *PostgresStore) ActivateDraftWithBill(ctx context.Context, tenantID, id string, at, periodStart, periodEnd time.Time, anchorDay int, billFn func(tx *sql.Tx, activated domain.Subscription) error) (domain.Subscription, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.Subscription{}, err
	}
	defer postgres.Rollback(tx)

	var sub domain.Subscription
	err = scanSubRow(tx.QueryRowContext(ctx, `
		UPDATE subscriptions
		SET status = 'active',
		    activated_at = $1,
		    started_at = $1,
		    current_billing_period_start = $2,
		    current_billing_period_end = $3,
		    next_billing_at = $3,
		    billing_anchor_day = $4,
		    updated_at = $1
		WHERE id = $5 AND status = 'draft'
		RETURNING `+subCols,
		at, periodStart, periodEnd, anchorDay, id,
	), &sub)
	if err == sql.ErrNoRows {
		// Row exists in another status (or not at all) — surface the same
		// typed error the pre-fix service check produced.
		return domain.Subscription{}, errs.InvalidState("can only activate draft subscriptions")
	}
	if err != nil {
		return domain.Subscription{}, err
	}
	if err := hydrateSubChildrenTx(ctx, tx, &sub); err != nil {
		return domain.Subscription{}, err
	}
	// ADR-101: activation IS the items' billable open.
	if err := intervalOpenForItems(ctx, tx, sub, "activate"); err != nil {
		return domain.Subscription{}, err
	}
	// The CAS above admits only draft rows, so reaching here IS the
	// draft→active activation transition — enqueue subscription.activated
	// in-tx (shared fate with the transition AND the day-1 bill below: a
	// billFn failure rolls the event back with everything else). This
	// enqueue previously lived in the store's Update method, which lost its
	// last caller when Activate migrated here — the event silently stopped
	// firing while Update's outbox test stayed green against the dead path.
	if err := s.enqueueLifecycle(ctx, tx, domain.EventSubscriptionActivated, sub, nil); err != nil {
		return domain.Subscription{}, err
	}
	if billFn != nil {
		if err := billFn(tx, sub); err != nil {
			return domain.Subscription{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return domain.Subscription{}, err
	}
	return sub, nil
}

func (s *PostgresStore) ActivateAfterTrialWithBill(ctx context.Context, tenantID, id string, at time.Time, billFn func(tx *sql.Tx, activated domain.Subscription) error) (domain.Subscription, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.Subscription{}, err
	}
	defer postgres.Rollback(tx)

	sub, err := s.activateAfterTrialInTx(ctx, tx, id, at)
	if err != nil {
		return domain.Subscription{}, err
	}
	if billFn != nil {
		if err := billFn(tx, sub); err != nil {
			return domain.Subscription{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return domain.Subscription{}, err
	}
	return sub, nil
}

func (s *PostgresStore) activateAfterTrialInTx(ctx context.Context, tx *sql.Tx, id string, at time.Time) (domain.Subscription, error) {
	var sub domain.Subscription
	// The schedule-empty predicate is the ADR-069 TOCTOU killer: a
	// cancel-at-trial-end committing between a scan's snapshot and this
	// UPDATE must WIN — activating (and billing) a customer who canceled is
	// the audit bug this arc exists to fix. Callers route the typed
	// ErrTrialCancelDue to the dedicated cancel transition.
	err := scanSubRow(tx.QueryRowContext(ctx, `
		UPDATE subscriptions
		SET status = 'active',
		    activated_at = COALESCE(activated_at, $1),
		    updated_at = $1
		WHERE id = $2 AND status = 'trialing'
		  AND cancel_at IS NULL AND cancel_at_period_end = false
		RETURNING `+subCols,
		at, id,
	), &sub)
	if err == sql.ErrNoRows {
		return domain.Subscription{}, s.classifyActivationConflict(ctx, tx, id)
	}
	if err != nil {
		return domain.Subscription{}, err
	}
	if err := hydrateSubChildrenTx(ctx, tx, &sub); err != nil {
		return domain.Subscription{}, err
	}
	// ADR-101: trial conversion IS the items' billable open — at the
	// post-trial period start stamped at creation (kept current by
	// ExtendTrial), not at the flip instant.
	if err := intervalOpenForItems(ctx, tx, sub, "trial_convert"); err != nil {
		return domain.Subscription{}, err
	}
	// Schedule-driven trial end (engine catchup / expiry sweep) — enqueue
	// in-tx with the provenance the prior post-commit dispatches stamped.
	if err := s.enqueueLifecycle(ctx, tx, domain.EventSubscriptionTrialEnded, sub, map[string]any{
		"ended_at":     at.UTC(),
		"triggered_by": "schedule",
	}); err != nil {
		return domain.Subscription{}, err
	}
	return sub, nil
}

// classifyActivationConflict names WHY a trialing→active UPDATE matched no
// row: gone, already non-trialing, or blocked by a pending cancel schedule
// (ErrTrialCancelDue — the caller routes to CancelAtTrialEnd).
func (s *PostgresStore) classifyActivationConflict(ctx context.Context, tx *sql.Tx, id string) error {
	var currentStatus string
	var cancelAt sql.NullTime
	var cancelAtPeriodEnd bool
	err := tx.QueryRowContext(ctx,
		`SELECT status, cancel_at, cancel_at_period_end FROM subscriptions WHERE id = $1`, id,
	).Scan(&currentStatus, &cancelAt, &cancelAtPeriodEnd)
	if err == sql.ErrNoRows {
		return errs.ErrNotFound
	}
	if err != nil {
		return err
	}
	if currentStatus == "trialing" && (cancelAt.Valid || cancelAtPeriodEnd) {
		return ErrTrialCancelDue
	}
	return errs.InvalidState(fmt.Sprintf("cannot end trial on %s subscription", currentStatus))
}

// EndTrialEarly atomically flips status 'trialing' → 'active', stamps
// activated_at, truncates trial_end_at to `at` (historical evidence
// that the operator ended the trial before its scheduled end), and
// resets the period anchor to (periodStart, periodEnd) so the first
// chargeable cycle starts immediately. The caller (Service.EndTrial)
// computes periodStart/periodEnd via firstPeriodForActivate so the
// reset honors billing_time. Returns errs.InvalidState if status is
// not 'trialing' at UPDATE time.
func (s *PostgresStore) EndTrialEarly(ctx context.Context, tenantID, id string, at, periodStart, periodEnd, nextBilling time.Time, anchorDay int) (domain.Subscription, error) {
	return s.EndTrialEarlyWithBill(ctx, tenantID, id, at, periodStart, periodEnd, nextBilling, anchorDay, nil)
}

// EndTrialEarlyWithBill flips 'trialing' → 'active' (resetting the period anchor)
// AND runs billFn (the day-1 in_advance invoice insert) in the SAME transaction,
// so a billing failure rolls the early-end back rather than leaving an active
// sub with no first-period invoice (revenue leak). ADR-056. billFn may be nil.
func (s *PostgresStore) EndTrialEarlyWithBill(ctx context.Context, tenantID, id string, at, periodStart, periodEnd, nextBilling time.Time, anchorDay int, billFn func(tx *sql.Tx, activated domain.Subscription) error) (domain.Subscription, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.Subscription{}, err
	}
	defer postgres.Rollback(tx)

	sub, err := s.endTrialEarlyInTx(ctx, tx, id, at, periodStart, periodEnd, nextBilling, anchorDay)
	if err != nil {
		return domain.Subscription{}, err
	}
	if billFn != nil {
		if err := billFn(tx, sub); err != nil {
			return domain.Subscription{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return domain.Subscription{}, err
	}
	return sub, nil
}

func (s *PostgresStore) endTrialEarlyInTx(ctx context.Context, tx *sql.Tx, id string, at, periodStart, periodEnd, nextBilling time.Time, anchorDay int) (domain.Subscription, error) {
	var sub domain.Subscription
	err := scanSubRow(tx.QueryRowContext(ctx, `
		UPDATE subscriptions
		SET status = 'active',
		    activated_at = COALESCE(activated_at, $1),
		    trial_end_at = $1,
		    current_billing_period_start = $2,
		    current_billing_period_end = $3,
		    next_billing_at = $4,
		    billing_anchor_day = $5,
		    updated_at = $1
		WHERE id = $6 AND status = 'trialing'
		  AND cancel_at IS NULL AND cancel_at_period_end = false
		RETURNING `+subCols,
		at, periodStart, periodEnd, nextBilling, anchorDay, id,
	), &sub)
	if err == sql.ErrNoRows {
		// A pending schedule blocks the early end IN SQL (ADR-069): the
		// service-level 409 alone is a snapshot check that loses the exact
		// race it exists to prevent (ScheduleCancel committing in the gap →
		// active + day-1 invoice + a schedule that now means paid-period
		// cancel). classifyActivationConflict maps it to ErrTrialCancelDue;
		// EndTrial translates that to the operator 409.
		return domain.Subscription{}, s.classifyActivationConflict(ctx, tx, id)
	}
	if err != nil {
		return domain.Subscription{}, err
	}
	if err := hydrateSubChildrenTx(ctx, tx, &sub); err != nil {
		return domain.Subscription{}, err
	}
	// ADR-101: the early end resets the period anchor above, so the open
	// lands at the just-stamped period start.
	if err := intervalOpenForItems(ctx, tx, sub, "trial_convert"); err != nil {
		return domain.Subscription{}, err
	}
	// Operator-driven early trial end — enqueue in-tx.
	if err := s.enqueueLifecycle(ctx, tx, domain.EventSubscriptionTrialEnded, sub, map[string]any{
		"ended_at":     at.UTC(),
		"triggered_by": "operator",
	}); err != nil {
		return domain.Subscription{}, err
	}
	return sub, nil
}

// ExtendTrial atomically updates trial_end_at AND the period anchor on
// a 'trialing' row. The caller (Service.ExtendTrial) validates that
// newTrialEnd makes sense (in the future, after the existing
// trial_end_at) and computes the new period via firstPeriodAfterTrial.
// Returns errs.InvalidState if the row's status is not 'trialing' at
// UPDATE time — distinguishes operator-already-ended / hard-paused /
// canceled from missing-row by re-querying status when no row matches.
func (s *PostgresStore) ExtendTrial(ctx context.Context, tenantID, id string, newTrialEnd, periodStart, periodEnd, nextBilling time.Time, anchorDay int) (domain.Subscription, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.Subscription{}, err
	}
	defer postgres.Rollback(tx)

	now := clock.Now(ctx)
	var sub domain.Subscription
	err = scanSubRow(tx.QueryRowContext(ctx, `
		UPDATE subscriptions
		SET trial_end_at = $1,
		    current_billing_period_start = $2,
		    current_billing_period_end = $3,
		    next_billing_at = $4,
		    billing_anchor_day = $5,
		    updated_at = $6
		WHERE id = $7 AND status = 'trialing'
		  AND cancel_at IS NULL
		RETURNING `+subCols,
		newTrialEnd, periodStart, periodEnd, nextBilling, anchorDay, now, id,
	), &sub)
	if err == sql.ErrNoRows {
		// A pending EXPLICIT cancel_at blocks the extension in SQL (ADR-069):
		// extending past a pinned timestamp strands it — nothing fires inside
		// a running trial, so the sub would silently outlive the operator's
		// echoed-in-GET cancel date. Flag-only schedules
		// (cancel_at_period_end) pass through: they MOVE with the trial by
		// design. Same two-layer shape as the EndTrial guard.
		var currentStatus string
		var cancelAt sql.NullTime
		err2 := tx.QueryRowContext(ctx, `SELECT status, cancel_at FROM subscriptions WHERE id = $1`, id).Scan(&currentStatus, &cancelAt)
		if err2 == sql.ErrNoRows {
			return domain.Subscription{}, errs.ErrNotFound
		}
		if err2 != nil {
			return domain.Subscription{}, err2
		}
		if currentStatus == "trialing" && cancelAt.Valid {
			return domain.Subscription{}, errs.InvalidState("subscription has a scheduled cancellation with an explicit date — clear the scheduled cancel first, then extend the trial")
		}
		return domain.Subscription{}, errs.InvalidState(fmt.Sprintf("cannot extend trial on %s subscription", currentStatus))
	}
	if err != nil {
		return domain.Subscription{}, err
	}

	if err := hydrateSubChildrenTx(ctx, tx, &sub); err != nil {
		return domain.Subscription{}, err
	}

	if err := tx.Commit(); err != nil {
		return domain.Subscription{}, err
	}
	return sub, nil
}

type transitionSpec struct {
	targetStatus  string
	allowedFrom   []string
	setCanceledAt bool
	wrongStateMsg string
}

func (s *PostgresStore) transitionAtomic(ctx context.Context, tenantID, id string, spec transitionSpec) (domain.Subscription, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.Subscription{}, err
	}
	defer postgres.Rollback(tx)

	sub, err := s.transitionInTx(ctx, tx, id, spec, clock.Now(ctx))
	if err != nil {
		return domain.Subscription{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.Subscription{}, err
	}
	return sub, nil
}

// transitionInTx runs the status-transition UPDATE (+ child hydration) on the
// given tx WITHOUT committing. Shared by transitionAtomic and the
// CancelAtomicWithBill coordinator so the final-on-cancel invoice can join the
// same transaction (ADR-056).
func (s *PostgresStore) transitionInTx(ctx context.Context, tx *sql.Tx, id string, spec transitionSpec, now time.Time) (domain.Subscription, error) {
	// Build the WHERE status IN (...) clause with positional args starting at $3
	// ($1 = updated_at, $2 = id). canceled_at slots in at $3 when needed.
	canceledAtArg := "canceled_at"
	args := []any{now, id}
	argIdx := 3
	if spec.setCanceledAt {
		canceledAtArg = fmt.Sprintf("$%d", argIdx)
		args = append(args, now)
		argIdx++
	}
	statusPlaceholders := make([]string, len(spec.allowedFrom))
	for i, st := range spec.allowedFrom {
		statusPlaceholders[i] = fmt.Sprintf("$%d", argIdx)
		args = append(args, st)
		argIdx++
	}

	query := fmt.Sprintf(`
		UPDATE subscriptions
		SET status = '%s', canceled_at = %s, updated_at = $1
		WHERE id = $2 AND status IN (%s)
		RETURNING %s`,
		spec.targetStatus,
		canceledAtArg,
		strings.Join(statusPlaceholders, ","),
		subCols,
	)

	var sub domain.Subscription
	err := scanSubRow(tx.QueryRowContext(ctx, query, args...), &sub)
	if err == sql.ErrNoRows {
		// Row either doesn't exist or is in a disallowed status. Re-query to
		// distinguish and build a precise error.
		var currentStatus string
		err2 := tx.QueryRowContext(ctx, `SELECT status FROM subscriptions WHERE id = $1`, id).Scan(&currentStatus)
		if err2 == sql.ErrNoRows {
			return domain.Subscription{}, errs.ErrNotFound
		}
		if err2 != nil {
			return domain.Subscription{}, err2
		}
		return domain.Subscription{}, errs.InvalidState(fmt.Sprintf(spec.wrongStateMsg, currentStatus))
	}
	if err != nil {
		return domain.Subscription{}, err
	}

	if err := hydrateSubChildrenTx(ctx, tx, &sub); err != nil {
		return domain.Subscription{}, err
	}

	// Cancel via CancelAtomic / CancelAtomicWithBill (cancelSpec is the only
	// canceled-target spec). Enqueue in-tx. canceled_by comes from
	// cancelActorLabel — the SAME rule the ADR-090 audit row in this very
	// transaction uses, so the outbound webhook and the audit evidence can
	// never disagree about who canceled. Pre-fix this hardcoded "operator"
	// while the audit row could say "dunning" for the same event.
	if spec.targetStatus == "canceled" {
		// ADR-101: an immediate cancel seals every open interval — at the
		// cancel instant for the common mid-period case, capped at the
		// period end the final invoice covers.
		if err := intervalCloseAllForCancel(ctx, tx, sub, now); err != nil {
			return domain.Subscription{}, err
		}
		extra := map[string]any{"canceled_by": cancelActorLabel(ctx)}
		if sub.CanceledAt != nil {
			extra["canceled_at"] = sub.CanceledAt.UTC()
		}
		if err := s.enqueueLifecycle(ctx, tx, domain.EventSubscriptionCanceled, sub, extra); err != nil {
			return domain.Subscription{}, err
		}
	}
	return sub, nil
}

func (s *PostgresStore) GetDueBilling(ctx context.Context, before time.Time, limit int) ([]domain.Subscription, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	if limit <= 0 {
		limit = 50
	}

	// ADR-028 disjoint flows: GetDueBilling is the wall-clock cron's
	// query — it processes ONLY subs without a test clock. Clock-
	// pinned subs are operator-controlled (Advance click triggers
	// GetDueBillingForClock). Without this filter, the cron would
	// silently drip-bill stuck clock-pinned subs in the background,
	// breaking the operator's mental model of simulation time AND
	// racing with the test-clock catchup worker via SKIP LOCKED.
	//
	// Stripe Test Clocks operate the same way: no cron touches
	// clock-pinned customers; advance is the sole path.
	//
	// TxBypass is used to cross tenants (scheduler-wide sweep), but the
	// caller's ctx livemode must still scope us to a single partition —
	// otherwise the per-sub TxTenant calls downstream (plan / test-clock /
	// settings lookups) default to live and silently fail for test-mode
	// subs. The scheduler fans out ctx per livemode; we honour that here
	// with an explicit WHERE clause since TxBypass doesn't set app.livemode.
	rows, err := tx.QueryContext(ctx, `
		SELECT `+qualifiedSubCols("s")+` FROM subscriptions s
		WHERE s.status IN ('active', 'trialing')
		  AND s.livemode = $1
		  AND s.test_clock_id IS NULL
		  AND (s.next_billing_at <= $2
		       OR (s.status = 'active' AND s.cancel_at IS NOT NULL AND s.cancel_at <= $2))
		ORDER BY s.next_billing_at ASC LIMIT $3
		FOR UPDATE OF s SKIP LOCKED
	`, postgres.Livemode(ctx), before, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var subs []domain.Subscription
	for rows.Next() {
		var sub domain.Subscription
		if err := scanSubRow(rows, &sub); err != nil {
			return nil, err
		}
		subs = append(subs, sub)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i := range subs {
		if err := hydrateSubChildrenTx(ctx, tx, &subs[i]); err != nil {
			return nil, err
		}
	}
	return subs, nil
}

// GetDueBillingForTenant is the operator-triggered (POST /v1/billing/run)
// counterpart to GetDueBilling, scoped to ONE tenant under RLS. Where
// GetDueBilling runs TxBypass to sweep every tenant for the wall-clock cron,
// this runs TxTenant so the subscriptions RLS policy (0020, mode-aware)
// restricts the result to the caller's own subscriptions AND livemode — the
// manual trigger can never observe or bill another tenant's subs (the pre-fix
// cross-tenant leak). Same disjoint-flow rules as GetDueBilling: clock-pinned
// subs are excluded (operator Advance owns them). FOR UPDATE SKIP LOCKED keeps a
// manual run and the concurrent wall-clock scheduler from contending on the same
// row DURING the fetch; it does NOT by itself prevent double-billing — these
// locks release when this fetch tx rolls back, before billSubscription runs.
// Exactly-once is guaranteed downstream by idx_invoices_billing_idempotency (a
// loser's 23505 → graceful ErrAlreadyExists skip), a mechanism this path reuses
// unchanged.
func (s *PostgresStore) GetDueBillingForTenant(ctx context.Context, tenantID string, before time.Time, limit int) ([]domain.Subscription, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	if limit <= 0 {
		limit = 50
	}

	// No explicit tenant_id/livemode predicate: TxTenant sets app.tenant_id +
	// app.livemode and the mode-aware RLS policy scopes both. Adding them to the
	// WHERE would be redundant with — and weaker than — the policy fence.
	rows, err := tx.QueryContext(ctx, `
		SELECT `+qualifiedSubCols("s")+` FROM subscriptions s
		WHERE s.status IN ('active', 'trialing')
		  AND s.test_clock_id IS NULL
		  AND (s.next_billing_at <= $1
		       OR (s.status = 'active' AND s.cancel_at IS NOT NULL AND s.cancel_at <= $1))
		ORDER BY s.next_billing_at ASC LIMIT $2
		FOR UPDATE OF s SKIP LOCKED
	`, before, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var subs []domain.Subscription
	for rows.Next() {
		var sub domain.Subscription
		if err := scanSubRow(rows, &sub); err != nil {
			return nil, err
		}
		subs = append(subs, sub)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i := range subs {
		if err := hydrateSubChildrenTx(ctx, tx, &subs[i]); err != nil {
			return nil, err
		}
	}
	return subs, nil
}

// GetDueBillingForClock returns subs attached to a specific test
// clock whose next_billing_at is on-or-before the clock's frozen
// time. Operator-driven catchup path (ADR-028 disjoint flows) —
// fired by the test-clock catchup worker after MarkAdvancing,
// never by the wall-clock cron. The complement of GetDueBilling.
//
// The query joins test_clocks (not LEFT JOIN — clock-pinned subs
// always have a row) and uses the clock's frozen_time as the
// "now" cutoff. Tenant scope comes from RLS via the BeginTx
// caller, but because the catchup worker is a per-clock unit of
// work, we also filter by test_clock_id explicitly.
func (s *PostgresStore) GetDueBillingForClock(ctx context.Context, tenantID, clockID string, limit int) ([]domain.Subscription, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	if limit <= 0 {
		limit = 50
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT `+qualifiedSubCols("s")+` FROM subscriptions s
		JOIN test_clocks tc ON tc.id = s.test_clock_id
		WHERE s.status IN ('active', 'trialing')
		  AND s.test_clock_id = $1
		  AND (s.next_billing_at <= tc.frozen_time
		       OR (s.status = 'active' AND s.cancel_at IS NOT NULL AND s.cancel_at <= tc.frozen_time))
		ORDER BY s.next_billing_at ASC LIMIT $2
		FOR UPDATE OF s SKIP LOCKED
	`, clockID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var subs []domain.Subscription
	for rows.Next() {
		var sub domain.Subscription
		if err := scanSubRow(rows, &sub); err != nil {
			return nil, err
		}
		subs = append(subs, sub)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range subs {
		if err := hydrateSubChildrenTx(ctx, tx, &subs[i]); err != nil {
			return nil, err
		}
	}
	return subs, nil
}

// ListExpiredPauseCollections returns wall-clock subs whose
// pause_collection_resumes_at has passed. Used by the scheduler's
// pause-resume tick (the dedicated scan that pairs with the trial-
// expiry scan — independent of cycle billing). Excludes clock-
// pinned subs to honor ADR-028 disjoint flows; those are processed
// by ListExpiredPauseCollectionsForClock instead.
//
// Stripe-parity rationale: Stripe resumes collection AT resumes_at
// rather than at the next cycle close. Before this method existed,
// the engine's auto-resume gate lived inside billOnePeriod and only
// fired when a cycle was due — so a sub whose resumes_at had passed
// but whose next_billing_at was still in the future would stay
// paused indefinitely. The new scan closes that gap.
func (s *PostgresStore) ListExpiredPauseCollections(ctx context.Context, before time.Time, limit int) ([]domain.Subscription, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	if limit <= 0 {
		limit = 50
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT `+qualifiedSubCols("s")+` FROM subscriptions s
		WHERE s.livemode = $1
		  AND s.test_clock_id IS NULL
		  AND s.pause_collection_resumes_at IS NOT NULL
		  AND s.pause_collection_resumes_at <= $2
		ORDER BY s.pause_collection_resumes_at ASC LIMIT $3
		FOR UPDATE OF s SKIP LOCKED
	`, postgres.Livemode(ctx), before, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var subs []domain.Subscription
	for rows.Next() {
		var sub domain.Subscription
		if err := scanSubRow(rows, &sub); err != nil {
			return nil, err
		}
		subs = append(subs, sub)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range subs {
		if err := hydrateSubChildrenTx(ctx, tx, &subs[i]); err != nil {
			return nil, err
		}
	}
	return subs, nil
}

// ListExpiredPauseCollectionsForClock is the clock-scoped counterpart.
// Per ADR-028 disjoint flows: invoked from the catchup orchestrator
// inside an Advance window, never by the wall-clock cron. Uses the
// clock's frozen_time as the cutoff so an Advance that crosses
// resumes_at picks the row up in the same advance window.
func (s *PostgresStore) ListExpiredPauseCollectionsForClock(ctx context.Context, tenantID, clockID string, frozen time.Time, limit int) ([]domain.Subscription, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	if limit <= 0 {
		limit = 50
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT `+qualifiedSubCols("s")+` FROM subscriptions s
		WHERE s.test_clock_id = $1
		  AND s.pause_collection_resumes_at IS NOT NULL
		  AND s.pause_collection_resumes_at <= $2
		ORDER BY s.pause_collection_resumes_at ASC LIMIT $3
		FOR UPDATE OF s SKIP LOCKED
	`, clockID, frozen, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var subs []domain.Subscription
	for rows.Next() {
		var sub domain.Subscription
		if err := scanSubRow(rows, &sub); err != nil {
			return nil, err
		}
		subs = append(subs, sub)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range subs {
		if err := hydrateSubChildrenTx(ctx, tx, &subs[i]); err != nil {
			return nil, err
		}
	}
	return subs, nil
}

// UpdateBillingCycle re-stamps the period boundaries, next_billing_at, and the
// billing anchor day. Normal cycle close passes the sub's existing
// BillingAnchorDay unchanged; the re-anchor paths (cross-interval plan swap,
// threshold reset) pass the recomputed anchor day for the new "now" cadence
// (ADR-055).
func (s *PostgresStore) UpdateBillingCycle(ctx context.Context, tenantID, id string, periodStart, periodEnd, nextBillingAt time.Time, anchorDay int) error {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)

	if err := s.UpdateBillingCycleTx(ctx, tx, tenantID, id, periodStart, periodEnd, nextBillingAt, anchorDay); err != nil {
		return err
	}
	return tx.Commit()
}

// UpdateBillingCycleTx is the in-transaction variant of UpdateBillingCycle: it
// runs the watermark UPDATE on the caller's tx so a coordinator can advance the
// billing period atomically alongside other writes. The cross-interval plan
// swap relies on this — the watermark must never move unless the new-period
// invoice is committed in the same tx, otherwise a failed day-1 bill silently
// drops the new period (the scheduler advances past it and never re-bills).
// tenantID is accepted for symmetry with the store's other *Tx methods; RLS
// scoping rides the tx's tenant binding.
func (s *PostgresStore) UpdateBillingCycleTx(ctx context.Context, tx *sql.Tx, tenantID, id string, periodStart, periodEnd, nextBillingAt time.Time, anchorDay int) error {
	result, err := tx.ExecContext(ctx, `
		UPDATE subscriptions SET current_billing_period_start = $1, current_billing_period_end = $2,
			next_billing_at = $3, billing_anchor_day = $4, updated_at = $5
		WHERE id = $6
	`, periodStart, periodEnd, nextBillingAt, anchorDay, clock.Now(ctx), id)
	if err != nil {
		return err
	}

	n, _ := result.RowsAffected()
	if n == 0 {
		return errs.ErrNotFound
	}
	return nil
}

// ---------------------------------------------------------------------------
// Item CRUD
// ---------------------------------------------------------------------------

func (s *PostgresStore) ListItems(ctx context.Context, tenantID, subscriptionID string) ([]domain.SubscriptionItem, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)
	return listItemsTx(ctx, tx, subscriptionID)
}

func (s *PostgresStore) GetItem(ctx context.Context, tenantID, itemID string) (domain.SubscriptionItem, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.SubscriptionItem{}, err
	}
	defer postgres.Rollback(tx)

	var item domain.SubscriptionItem
	// deleted_at IS NULL — soft-deleted items are hidden from live
	// reads (migration 0102). Operators wanting historical state read
	// from subscription_item_changes / the audit log.
	err = tx.QueryRowContext(ctx, `SELECT `+itemCols+` FROM subscription_items WHERE id = $1 AND deleted_at IS NULL`, itemID).
		Scan(scanItemDest(&item)...)
	if err == sql.ErrNoRows {
		return domain.SubscriptionItem{}, errs.ErrNotFound
	}
	return item, err
}

func (s *PostgresStore) AddItem(ctx context.Context, tenantID string, item domain.SubscriptionItem) (domain.SubscriptionItem, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.SubscriptionItem{}, err
	}
	defer postgres.Rollback(tx)
	stored, err := s.addItemInTx(ctx, tx, tenantID, item)
	if err != nil {
		return domain.SubscriptionItem{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.SubscriptionItem{}, err
	}
	return stored, nil
}

// AddItemTx is the in-transaction variant of AddItem — the caller owns
// the tx (already opened with the correct RLS/tenant context) and is
// responsible for commit/rollback. Used by the subscription handler's
// AddItem flow to compose the item insert atomically with the proration
// invoice or credit insert. Without this, item add committed in its own
// tx and a subsequent proration failure left an orphan item — the bug
// surfaced during EX3 manual test 2026-05-28.
func (s *PostgresStore) AddItemTx(ctx context.Context, tx *sql.Tx, tenantID string, item domain.SubscriptionItem) (domain.SubscriptionItem, error) {
	return s.addItemInTx(ctx, tx, tenantID, item)
}

// addItemInTx is the shared implementation. AddItem opens+commits its
// own tx; AddItemTx delegates commit to the caller.
func (s *PostgresStore) addItemInTx(ctx context.Context, tx *sql.Tx, tenantID string, item domain.SubscriptionItem) (domain.SubscriptionItem, error) {
	qty := item.Quantity
	if qty <= 0 {
		qty = 1
	}
	now := clock.Now(ctx)
	var stored domain.SubscriptionItem
	err := tx.QueryRowContext(ctx, `
		INSERT INTO subscription_items (tenant_id, subscription_id, plan_id, quantity, metadata, created_at, updated_at)
		VALUES ($1,$2,$3,$4,COALESCE(NULLIF($5,'')::jsonb,'{}'::jsonb),$6,$6)
		RETURNING `+itemCols,
		tenantID, item.SubscriptionID, item.PlanID, qty, string(item.Metadata), now,
	).Scan(scanItemDest(&stored)...)
	if err != nil {
		if postgres.UniqueViolationConstraint(err) != "" {
			return domain.SubscriptionItem{}, errs.AlreadyExists("plan_id",
				fmt.Sprintf("subscription already has an item for plan %q", item.PlanID))
		}
		return domain.SubscriptionItem{}, err
	}
	// ADR-101: same-tx interval open (with the day-grade 'add' clamp).
	if err := intervalOpenForAdd(ctx, tx, stored.ID, stored.SubscriptionID, stored.PlanID, stored.Quantity, now); err != nil {
		return domain.SubscriptionItem{}, err
	}
	return stored, nil
}

func (s *PostgresStore) UpdateItemQuantity(ctx context.Context, tenantID, itemID string, quantity int64) (domain.SubscriptionItem, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.SubscriptionItem{}, err
	}
	defer postgres.Rollback(tx)
	stored, err := s.updateItemQuantityInTx(ctx, tx, itemID, quantity)
	if err != nil {
		return domain.SubscriptionItem{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.SubscriptionItem{}, err
	}
	return stored, nil
}

// UpdateItemQuantityTx is the in-transaction variant. Used by the
// atomic UpdateItem-with-proration flow so the quantity update + the
// proration write share one tx. ADR-030 atomic-proration follow-
// through (2026-05-29).
func (s *PostgresStore) UpdateItemQuantityTx(ctx context.Context, tx *sql.Tx, _ string, itemID string, quantity int64) (domain.SubscriptionItem, error) {
	return s.updateItemQuantityInTx(ctx, tx, itemID, quantity)
}

func (s *PostgresStore) updateItemQuantityInTx(ctx context.Context, tx *sql.Tx, itemID string, quantity int64) (domain.SubscriptionItem, error) {
	now := clock.Now(ctx)
	var stored domain.SubscriptionItem
	err := tx.QueryRowContext(ctx, `
		UPDATE subscription_items
		SET quantity = $1, updated_at = $2
		WHERE id = $3
		RETURNING `+itemCols,
		quantity, now, itemID,
	).Scan(scanItemDest(&stored)...)
	if err == sql.ErrNoRows {
		return domain.SubscriptionItem{}, errs.ErrNotFound
	}
	if err != nil {
		return domain.SubscriptionItem{}, err
	}
	// ADR-101: quantity moves at the raw instant — end the current
	// interval, open the successor, same tx.
	if err := intervalTransition(ctx, tx, stored.ID, stored.SubscriptionID, now, "", &quantity, "quantity"); err != nil {
		return domain.SubscriptionItem{}, err
	}
	return stored, nil
}

func (s *PostgresStore) ApplyItemPlanImmediately(ctx context.Context, tenantID, itemID, newPlanID string, changedAt time.Time) (domain.SubscriptionItem, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.SubscriptionItem{}, err
	}
	defer postgres.Rollback(tx)
	stored, err := s.applyItemPlanImmediatelyInTx(ctx, tx, itemID, newPlanID, changedAt)
	if err != nil {
		return domain.SubscriptionItem{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.SubscriptionItem{}, err
	}
	return stored, nil
}

// ApplyItemPlanImmediatelyTx is the in-transaction variant — same
// atomicity rationale as the other Tx variants. ADR-030 atomic-
// proration follow-through (2026-05-29).
func (s *PostgresStore) ApplyItemPlanImmediatelyTx(ctx context.Context, tx *sql.Tx, _ string, itemID, newPlanID string, changedAt time.Time) (domain.SubscriptionItem, error) {
	return s.applyItemPlanImmediatelyInTx(ctx, tx, itemID, newPlanID, changedAt)
}

func (s *PostgresStore) applyItemPlanImmediatelyInTx(ctx context.Context, tx *sql.Tx, itemID, newPlanID string, changedAt time.Time) (domain.SubscriptionItem, error) {
	// Clears any scheduled change — an immediate swap supersedes a pending one.
	var stored domain.SubscriptionItem
	err := tx.QueryRowContext(ctx, `
		UPDATE subscription_items
		SET plan_id = $1,
		    plan_changed_at = $2,
		    pending_plan_id = NULL,
		    pending_plan_effective_at = NULL,
		    updated_at = $2
		WHERE id = $3
		RETURNING `+itemCols,
		newPlanID, changedAt, itemID,
	).Scan(scanItemDest(&stored)...)
	if err == sql.ErrNoRows {
		return domain.SubscriptionItem{}, errs.ErrNotFound
	}
	if err != nil {
		if postgres.UniqueViolationConstraint(err) != "" {
			return domain.SubscriptionItem{}, errs.AlreadyExists("plan_id",
				fmt.Sprintf("subscription already has an item for plan %q", newPlanID))
		}
		return domain.SubscriptionItem{}, err
	}
	// ADR-101: the plan swap moves at its effective instant, same tx.
	if err := intervalTransition(ctx, tx, stored.ID, stored.SubscriptionID, changedAt, newPlanID, nil, "plan"); err != nil {
		return domain.SubscriptionItem{}, err
	}
	return stored, nil
}

func (s *PostgresStore) SetItemPendingPlan(ctx context.Context, tenantID, itemID, pendingPlanID string, effectiveAt time.Time) (domain.SubscriptionItem, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.SubscriptionItem{}, err
	}
	defer postgres.Rollback(tx)

	now := clock.Now(ctx)
	var stored domain.SubscriptionItem
	err = tx.QueryRowContext(ctx, `
		UPDATE subscription_items
		SET pending_plan_id = $1,
		    pending_plan_effective_at = $2,
		    updated_at = $3
		WHERE id = $4
		RETURNING `+itemCols,
		pendingPlanID, effectiveAt, now, itemID,
	).Scan(scanItemDest(&stored)...)
	if err == sql.ErrNoRows {
		return domain.SubscriptionItem{}, errs.ErrNotFound
	}
	if err != nil {
		return domain.SubscriptionItem{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.SubscriptionItem{}, err
	}
	return stored, nil
}

func (s *PostgresStore) ClearItemPendingPlan(ctx context.Context, tenantID, itemID string) (domain.SubscriptionItem, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.SubscriptionItem{}, err
	}
	defer postgres.Rollback(tx)

	now := clock.Now(ctx)
	var stored domain.SubscriptionItem
	err = tx.QueryRowContext(ctx, `
		UPDATE subscription_items
		SET pending_plan_id = NULL,
		    pending_plan_effective_at = NULL,
		    updated_at = $1
		WHERE id = $2
		RETURNING `+itemCols,
		now, itemID,
	).Scan(scanItemDest(&stored)...)
	if err == sql.ErrNoRows {
		return domain.SubscriptionItem{}, errs.ErrNotFound
	}
	if err != nil {
		return domain.SubscriptionItem{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.SubscriptionItem{}, err
	}
	return stored, nil
}

func (s *PostgresStore) ApplyDuePendingItemPlansAtomic(ctx context.Context, tenantID, subscriptionID string, now time.Time) ([]domain.SubscriptionItem, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	// Swap every item under the subscription whose pending change is due.
	// All items move in one statement so a caller reading the result sees a
	// consistent snapshot. Other items (no pending change or pending-but-future)
	// are untouched. Soft-deleted items are excluded (ADR-101 wiring pass):
	// applying a stale pending to a removed row mutated dead state no reader
	// bills, wrote no fact row (the 0129 trigger skips deleted rows), and
	// would now demand an interval for an item whose lifetime already ended.
	rows, err := tx.QueryContext(ctx, `
		UPDATE subscription_items
		SET plan_id = pending_plan_id,
		    plan_changed_at = $1,
		    pending_plan_id = NULL,
		    pending_plan_effective_at = NULL,
		    updated_at = $1
		WHERE subscription_id = $2
		  AND deleted_at IS NULL
		  AND pending_plan_id IS NOT NULL
		  AND pending_plan_effective_at IS NOT NULL
		  AND pending_plan_effective_at <= $1
		RETURNING `+itemCols,
		now, subscriptionID,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var updated []domain.SubscriptionItem
	for rows.Next() {
		var it domain.SubscriptionItem
		if err := rows.Scan(scanItemDest(&it)...); err != nil {
			return nil, err
		}
		updated = append(updated, it)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// ADR-101: each swap moves at the gate it was applied at ($now — the
	// caller's period boundary during catch-up, NOT re-snapped). When a
	// later-instant change already sealed past the gate (engine-down
	// interleave), intervalTransition splices retroactively.
	for _, it := range updated {
		if err := intervalTransition(ctx, tx, it.ID, it.SubscriptionID, now, it.PlanID, nil, "plan_scheduled"); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return updated, nil
}

func (s *PostgresStore) RemoveItem(ctx context.Context, tenantID, itemID string) error {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)
	if err := s.removeItemInTx(ctx, tx, itemID); err != nil {
		return err
	}
	return tx.Commit()
}

// RemoveItemTx is the in-transaction variant — same atomicity
// rationale as the other Tx variants. ADR-030 atomic-proration follow-
// through (2026-05-29).
func (s *PostgresStore) RemoveItemTx(ctx context.Context, tx *sql.Tx, _ string, itemID string) error {
	return s.removeItemInTx(ctx, tx, itemID)
}

func (s *PostgresStore) removeItemInTx(ctx context.Context, tx *sql.Tx, itemID string) error {
	// Soft-delete (migration 0102). Physical DELETE fought the
	// `invoices_source_subscription_item_id_fkey` constraint whenever
	// the item had been involved in a proration event — common after
	// the first cycle. Marking the row deleted preserves the FK
	// back-pointer for auditors while making the item invisible to
	// active-state queries via their `deleted_at IS NULL` filter.
	now := clock.Now(ctx)
	var subID string
	err := tx.QueryRowContext(ctx,
		`UPDATE subscription_items SET deleted_at = $1, updated_at = $1 WHERE id = $2 AND deleted_at IS NULL RETURNING subscription_id`,
		now, itemID,
	).Scan(&subID)
	if err == sql.ErrNoRows {
		return errs.ErrNotFound
	}
	if err != nil {
		return err
	}
	// ADR-101: the soft-delete instant seals the item's billable lifetime.
	return intervalCloseForRemove(ctx, tx, itemID, subID, now)
}

// FindMeterConflicts implements the double-billing guard's conflict scan:
// one query walks the customer's live (draft/trialing/active) subs → live
// items → plans → unnested meter_ids, intersected with the candidate set.
// pending_plan_id joins too — a scheduled swap starts billing its meters at
// the next cycle boundary, so admitting an overlap against it now just
// defers the double-bill. Tenant scoping rides the RLS tx like every other
// reader; candidate meters travel as a jsonb array (matching how plans
// store meter_ids) rather than pulling in a driver-specific array type.
func (s *PostgresStore) FindMeterConflicts(ctx context.Context, tenantID, customerID, excludeItemID string, meterIDs []string) ([]MeterConflict, error) {
	if len(meterIDs) == 0 {
		return nil, nil
	}
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	candidates, err := json.Marshal(meterIDs)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT DISTINCT m.meter_id, sub.id, sub.code
		FROM subscriptions sub
		JOIN subscription_items si
		  ON si.subscription_id = sub.id AND si.deleted_at IS NULL
		JOIN plans p
		  ON p.id = si.plan_id OR p.id = si.pending_plan_id
		-- meter_ids is 'null' (not '[]') for meterless plans — the pricing
		-- store marshals a nil slice — and unnesting a scalar is an error.
		CROSS JOIN LATERAL jsonb_array_elements_text(
			CASE WHEN jsonb_typeof(p.meter_ids) = 'array' THEN p.meter_ids ELSE '[]'::jsonb END
		) AS m(meter_id)
		WHERE sub.customer_id = $1
		  AND sub.status IN ('draft', 'trialing', 'active')
		  AND si.id <> $2
		  AND m.meter_id IN (SELECT jsonb_array_elements_text($3::jsonb))
		ORDER BY m.meter_id, sub.code`,
		customerID, excludeItemID, string(candidates))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var conflicts []MeterConflict
	for rows.Next() {
		var c MeterConflict
		if err := rows.Scan(&c.MeterID, &c.SubscriptionID, &c.SubscriptionCode); err != nil {
			return nil, err
		}
		conflicts = append(conflicts, c)
	}
	return conflicts, rows.Err()
}

// ---------------------------------------------------------------------------
// Billing thresholds
// ---------------------------------------------------------------------------

// SetBillingThresholds writes the (amount_gte, reset_cycle, item_thresholds)
// triple onto the row in one transaction. Replaces the per-item threshold
// set: any item_thresholds row not in the new slice is deleted, present
// rows are upserted by primary key (subscription_item_id).
//
// Rejects rows in canceled/archived since a threshold on a terminal
// subscription has no meaning — the engine wouldn't observe it anyway,
// but the API surface should be consistent. Service layer is responsible
// for validating that every t.ItemThresholds[i].SubscriptionItemID
// belongs to this subscription.
func (s *PostgresStore) SetBillingThresholds(ctx context.Context, tenantID, id string, t domain.BillingThresholds) (domain.Subscription, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.Subscription{}, err
	}
	defer postgres.Rollback(tx)

	now := clock.Now(ctx)

	// Update the parent subscription columns in the same tx as the per-item
	// upserts so a partial commit can't leave amount_gte set without the
	// item rows the caller intended.
	var amountArg sql.NullInt64
	if t.AmountGTE > 0 {
		amountArg = sql.NullInt64{Int64: t.AmountGTE, Valid: true}
	}
	var sub domain.Subscription
	err = scanSubRow(tx.QueryRowContext(ctx, `
		UPDATE subscriptions
		SET billing_threshold_amount_gte = $1,
		    billing_threshold_reset_cycle = $2,
		    updated_at = $3
		WHERE id = $4 AND status NOT IN ('canceled','archived')
		RETURNING `+subCols,
		amountArg, t.ResetBillingCycle, now, id,
	), &sub)
	if err == sql.ErrNoRows {
		var currentStatus string
		err2 := tx.QueryRowContext(ctx, `SELECT status FROM subscriptions WHERE id = $1`, id).Scan(&currentStatus)
		if err2 == sql.ErrNoRows {
			return domain.Subscription{}, errs.ErrNotFound
		}
		if err2 != nil {
			return domain.Subscription{}, err2
		}
		return domain.Subscription{}, errs.InvalidState(fmt.Sprintf("cannot set billing threshold on %s subscription", currentStatus))
	}
	if err != nil {
		return domain.Subscription{}, err
	}

	// Replace the per-item set in one statement: DELETE everything not in the
	// new slice, then UPSERT each new row. This pattern preserves the
	// (subscription_item_id) PK across calls and lets idempotent re-set
	// converge to the same state without churn.
	keepIDs := make([]string, 0, len(t.ItemThresholds))
	for _, it := range t.ItemThresholds {
		keepIDs = append(keepIDs, it.SubscriptionItemID)
	}
	if len(keepIDs) > 0 {
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM subscription_item_thresholds
			WHERE subscription_id = $1 AND NOT (subscription_item_id = ANY($2::text[]))
		`, id, keepIDs); err != nil {
			return domain.Subscription{}, fmt.Errorf("delete stale item thresholds: %w", err)
		}
	} else {
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM subscription_item_thresholds WHERE subscription_id = $1
		`, id); err != nil {
			return domain.Subscription{}, fmt.Errorf("clear item thresholds: %w", err)
		}
	}

	for _, it := range t.ItemThresholds {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO subscription_item_thresholds (subscription_id, subscription_item_id, tenant_id, usage_gte, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $5)
			ON CONFLICT (subscription_item_id) DO UPDATE
			SET usage_gte = EXCLUDED.usage_gte,
			    updated_at = EXCLUDED.updated_at
		`, id, it.SubscriptionItemID, tenantID, it.UsageGTE.String(), now); err != nil {
			return domain.Subscription{}, fmt.Errorf("upsert item threshold for %s: %w", it.SubscriptionItemID, err)
		}
	}

	if err := hydrateSubChildrenTx(ctx, tx, &sub); err != nil {
		return domain.Subscription{}, err
	}

	if err := tx.Commit(); err != nil {
		return domain.Subscription{}, err
	}
	return sub, nil
}

// ClearBillingThresholds nulls the amount_gte column, resets reset_cycle
// to its default (true), and deletes every per-item threshold row for
// this subscription. Idempotent — clearing on a sub with no threshold
// returns the unchanged subscription.
func (s *PostgresStore) ClearBillingThresholds(ctx context.Context, tenantID, id string) (domain.Subscription, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.Subscription{}, err
	}
	defer postgres.Rollback(tx)

	now := clock.Now(ctx)
	var sub domain.Subscription
	err = scanSubRow(tx.QueryRowContext(ctx, `
		UPDATE subscriptions
		SET billing_threshold_amount_gte = NULL,
		    billing_threshold_reset_cycle = TRUE,
		    updated_at = $1
		WHERE id = $2
		RETURNING `+subCols,
		now, id,
	), &sub)
	if err == sql.ErrNoRows {
		return domain.Subscription{}, errs.ErrNotFound
	}
	if err != nil {
		return domain.Subscription{}, err
	}

	if _, err := tx.ExecContext(ctx, `
		DELETE FROM subscription_item_thresholds WHERE subscription_id = $1
	`, id); err != nil {
		return domain.Subscription{}, fmt.Errorf("clear item thresholds: %w", err)
	}

	// Reset BillingThresholds to nil since both amount and per-item are
	// now cleared. scanSubRow already left it nil (column became NULL),
	// but be explicit to keep hydrate from re-allocating an empty struct.
	sub.BillingThresholds = nil

	items, err := listItemsTx(ctx, tx, sub.ID)
	if err != nil {
		return domain.Subscription{}, err
	}
	sub.Items = items

	if err := tx.Commit(); err != nil {
		return domain.Subscription{}, err
	}
	return sub, nil
}

// ListWithThresholds returns active or trialing subscriptions in the
// given livemode partition that have at least one threshold configured
// (amount_gte set, OR at least one row in subscription_item_thresholds).
// Used by the threshold scan tick. Result is hydrated with items +
// thresholds so the caller can run the cycle math without a second
// round-trip per row.
//
// Uses TxBypass + explicit livemode filter for the same reason as
// GetDueBilling: the scheduler is cross-tenant and the per-tenant
// downstream calls (usage aggregation, plan reads) carry their own
// tenant context.
// ListWithThresholds — CRON path. ADR-029 Phase 3: clock-pinned subs
// are excluded; ListWithThresholdsForClock is the catchup-side
// counterpart driven by Engine.ScanThresholdsForClock during Advance.
// Without this filter the wall-clock cron would fire threshold-based
// invoices on clock-pinned subs even though their running cycle
// subtotals are accumulated against simulated time — same drip-bill
// shape ADR-028 closed for period generation.
func (s *PostgresStore) ListWithThresholds(ctx context.Context, livemode bool, afterID string, limit int) ([]domain.Subscription, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	if limit <= 0 {
		limit = 100
	}

	// afterID is the scan's drain cursor: strictly-increasing over ORDER BY
	// s.id, so the caller pages through the WHOLE candidate set (fired subs
	// stay in the set — thresholds remain configured — hence the cursor, not
	// an offset or a shrinking predicate). "" starts from the beginning.
	rows, err := tx.QueryContext(ctx, `
		SELECT `+qualifiedSubCols("s")+` FROM subscriptions s
		WHERE s.status IN ('active', 'trialing')
		  AND s.livemode = $1
		  AND s.test_clock_id IS NULL
		  AND s.id > $2
		  AND (s.billing_threshold_amount_gte IS NOT NULL
		       OR EXISTS (SELECT 1 FROM subscription_item_thresholds sit WHERE sit.subscription_id = s.id))
		ORDER BY s.id ASC
		LIMIT $3
	`, livemode, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var subs []domain.Subscription
	for rows.Next() {
		var sub domain.Subscription
		if err := scanSubRow(rows, &sub); err != nil {
			return nil, err
		}
		subs = append(subs, sub)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i := range subs {
		if err := hydrateSubChildrenTx(ctx, tx, &subs[i]); err != nil {
			return nil, err
		}
	}
	return subs, nil
}

// ListWithThresholdsForClock is the catchup-path counterpart to
// ListWithThresholds. ADR-029 Phase 3 — returns active+trialing
// subscriptions with billing thresholds configured that are pinned to
// the given clock. Caller (Engine.ScanThresholdsForClock) evaluates
// running-cycle subtotals against the clock's frozen_time.
func (s *PostgresStore) ListWithThresholdsForClock(ctx context.Context, tenantID, clockID, afterID string, limit int) ([]domain.Subscription, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	if limit <= 0 {
		limit = 100
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT `+qualifiedSubCols("s")+` FROM subscriptions s
		WHERE s.status IN ('active', 'trialing')
		  AND s.tenant_id = $1
		  AND s.test_clock_id = $2
		  AND s.id > $3
		  AND (s.billing_threshold_amount_gte IS NOT NULL
		       OR EXISTS (SELECT 1 FROM subscription_item_thresholds sit WHERE sit.subscription_id = s.id))
		ORDER BY s.id ASC
		LIMIT $4
	`, tenantID, clockID, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var subs []domain.Subscription
	for rows.Next() {
		var sub domain.Subscription
		if err := scanSubRow(rows, &sub); err != nil {
			return nil, err
		}
		subs = append(subs, sub)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range subs {
		if err := hydrateSubChildrenTx(ctx, tx, &subs[i]); err != nil {
			return nil, err
		}
	}
	return subs, nil
}

// ListExpiredTrialsForClock returns subs pinned to this clock whose
// trial has elapsed in simulated time (trial_end_at <= frozen) but
// whose status is still 'trialing'. Catchup Phase 0.5 picks these
// up and flips status to active at trial_end_at so the dashboard
// doesn't lie about lifecycle state for the (potentially 30-day)
// gap between trial_end_at and the next cycle close.
//
// FOR UPDATE SKIP LOCKED guards against a concurrent operator
// EndTrial racing the catchup pass — whichever path takes the row
// first wins; the loser sees a no-row UPDATE and continues to the
// next sub. Items are hydrated for the in_advance BillOnCreate
// downstream.
func (s *PostgresStore) ListExpiredTrialsForClock(ctx context.Context, tenantID, clockID string, frozen time.Time, limit int) ([]domain.Subscription, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	if limit <= 0 {
		limit = 100
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT `+qualifiedSubCols("s")+` FROM subscriptions s
		WHERE s.status = 'trialing'
		  AND s.test_clock_id = $1
		  AND s.trial_end_at IS NOT NULL
		  AND s.trial_end_at <= $2
		ORDER BY s.trial_end_at ASC
		LIMIT $3
		FOR UPDATE OF s SKIP LOCKED
	`, clockID, frozen, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var subs []domain.Subscription
	for rows.Next() {
		var sub domain.Subscription
		if err := scanSubRow(rows, &sub); err != nil {
			return nil, err
		}
		subs = append(subs, sub)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range subs {
		if err := hydrateSubChildrenTx(ctx, tx, &subs[i]); err != nil {
			return nil, err
		}
	}
	return subs, nil
}

// ListExpiredTrials is the wall-clock counterpart for the cron tick —
// returns non-clock-pinned trialing subs whose trial_end_at has
// elapsed. Mirrors ListExpiredTrialsForClock but scoped per livemode
// rather than per clock; uses TxBypass to cross tenants (scheduler-
// wide sweep) with an explicit livemode filter (TxBypass doesn't set
// app.livemode for RLS).
//
// ADR-028 disjoint flows: clock-pinned subs are EXPLICITLY EXCLUDED
// (`test_clock_id IS NULL`). Those flow through the catchup
// orchestrator's Phase 0.5 — running them here too would race the
// orchestrator and could drift status / generate duplicate
// trial-end invoices.
//
// FOR UPDATE SKIP LOCKED so concurrent scheduler ticks (multi-
// replica) don't double-process the same trialing sub.
func (s *PostgresStore) ListExpiredTrials(ctx context.Context, before time.Time, livemode bool, limit int) ([]domain.Subscription, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	if limit <= 0 {
		limit = 100
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT `+qualifiedSubCols("s")+` FROM subscriptions s
		WHERE s.status = 'trialing'
		  AND s.livemode = $1
		  AND s.test_clock_id IS NULL
		  AND s.trial_end_at IS NOT NULL
		  AND s.trial_end_at <= $2
		ORDER BY s.trial_end_at ASC
		LIMIT $3
		FOR UPDATE OF s SKIP LOCKED
	`, livemode, before, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var subs []domain.Subscription
	for rows.Next() {
		var sub domain.Subscription
		if err := scanSubRow(rows, &sub); err != nil {
			return nil, err
		}
		subs = append(subs, sub)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range subs {
		if err := hydrateSubChildrenTx(ctx, tx, &subs[i]); err != nil {
			return nil, err
		}
	}
	return subs, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// hydrateSubChildrenTx hydrates a subscription's items + billing thresholds
// inside a single transaction. Replaces the historic listItemsTx + manual
// item assignment pattern at most call sites — feature additions like
// thresholds should land here, not be duplicated at every read path.
//
// Threshold hydration only runs when the row has amount_gte set OR there
// is at least one row in subscription_item_thresholds. Pure no-threshold
// subs pay one extra LEFT JOIN'd EXISTS check, which is index-supported
// (idx_subscription_item_thresholds_subscription) and effectively free.
func hydrateSubChildrenTx(ctx context.Context, tx *sql.Tx, sub *domain.Subscription) error {
	items, err := listItemsTx(ctx, tx, sub.ID)
	if err != nil {
		return err
	}
	sub.Items = items
	return hydrateThresholds(ctx, tx, sub)
}

// listItemsTx reads a subscription's items inside an existing transaction so
// callers on the hot load path don't pay a second BEGIN/COMMIT. Returns items
// ordered by created_at so item display order stays stable across requests.
func listItemsTx(ctx context.Context, tx *sql.Tx, subscriptionID string) ([]domain.SubscriptionItem, error) {
	// deleted_at IS NULL — sub.Items hydration sees the LIVE item set
	// only (migration 0102 soft-delete model). The
	// idx_subscription_items_live partial index keeps this hot.
	rows, err := tx.QueryContext(ctx, `
		SELECT `+itemCols+` FROM subscription_items
		WHERE subscription_id = $1 AND deleted_at IS NULL
		ORDER BY created_at ASC, id ASC
	`, subscriptionID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var items []domain.SubscriptionItem
	for rows.Next() {
		var it domain.SubscriptionItem
		if err := rows.Scan(scanItemDest(&it)...); err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

// qualifiedSubCols prefixes every column in subCols with the given table alias.
// Needed when subscriptions is JOINed against another table (e.g. test_clocks
// or subscription_items for filtering) with overlapping column names.
func qualifiedSubCols(alias string) string {
	var b strings.Builder
	for i, col := range splitTopLevelCommas(subCols) {
		if i > 0 {
			b.WriteString(", ")
		}
		col = strings.TrimSpace(col)
		if strings.HasPrefix(col, "COALESCE(") {
			closing := strings.IndexByte(col, ')')
			inner := col[len("COALESCE("):closing]
			parts := strings.SplitN(inner, ",", 2)
			b.WriteString("COALESCE(")
			b.WriteString(alias)
			b.WriteByte('.')
			b.WriteString(strings.TrimSpace(parts[0]))
			if len(parts) == 2 {
				b.WriteString(",")
				b.WriteString(parts[1])
			}
			b.WriteString(col[closing:])
			continue
		}
		b.WriteString(alias)
		b.WriteByte('.')
		b.WriteString(col)
	}
	return b.String()
}

func splitTopLevelCommas(s string) []string {
	var out []string
	depth := 0
	start := 0
	for i, r := range s {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}

// rowScanner abstracts *sql.Row and *sql.Rows so scanSubRow works for both
// QueryRowContext and the per-row loop in QueryContext.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanSubRow scans subCols into sub. Handles fields that need post-processing
// — currently the nullable (behavior, resumes_at) pair that composes into
// the *PauseCollection field on the domain struct, plus the (amount_gte,
// reset_cycle) pair that composes into BillingThresholds. Per-item
// thresholds are hydrated separately by hydrateThresholds since they
// live in an aux table.
func scanSubRow(row rowScanner, sub *domain.Subscription) error {
	var pauseBehavior sql.NullString
	var pauseResumesAt sql.NullTime
	var thresholdAmountGTE sql.NullInt64
	var thresholdResetCycle bool
	dest := []any{
		&sub.ID, &sub.TenantID, &sub.Code, &sub.DisplayName, &sub.CustomerID,
		&sub.Status, &sub.BillingTime, &sub.TrialStartAt, &sub.TrialEndAt, &sub.StartedAt,
		&sub.ActivatedAt, &sub.CanceledAt,
		&sub.CancelAt, &sub.CancelAtPeriodEnd,
		&pauseBehavior, &pauseResumesAt,
		&thresholdAmountGTE, &thresholdResetCycle,
		&sub.CurrentBillingPeriodStart,
		&sub.CurrentBillingPeriodEnd, &sub.NextBillingAt,
		&sub.BillingAnchorDay,
		&sub.UsageCapUnits, &sub.OverageAction,
		&sub.TestClockID,
		&sub.CreatedAt, &sub.UpdatedAt,
	}
	if err := row.Scan(dest...); err != nil {
		return err
	}
	if pauseBehavior.Valid {
		pc := &domain.PauseCollection{
			Behavior: domain.PauseCollectionBehavior(pauseBehavior.String),
		}
		if pauseResumesAt.Valid {
			t := pauseResumesAt.Time
			pc.ResumesAt = &t
		}
		sub.PauseCollection = pc
	}
	// BillingThresholds is partially populated here from the columns on the
	// row. ItemThresholds is filled in by hydrateThresholds because it lives
	// in an aux table. The struct is allocated here only when amount_gte is
	// set; for item-only thresholds hydrateThresholds allocates it and
	// re-reads the row's reset_cycle column itself (it is NOT derivable
	// from struct-nilness — assuming the column default here is the bug
	// fixed 2026-07-21).
	if thresholdAmountGTE.Valid {
		sub.BillingThresholds = &domain.BillingThresholds{
			AmountGTE:         thresholdAmountGTE.Int64,
			ResetBillingCycle: thresholdResetCycle,
			ItemThresholds:    []domain.SubscriptionItemThreshold{},
		}
	}
	// ADR-069: derive the read-only cancel_effective_at at THE scan choke
	// point so every read path (Get/List/scans/API) agrees on when the sub
	// actually cancels.
	sub.DeriveCancelEffectiveAt()
	return nil
}

// hydrateThresholds fills sub.BillingThresholds.ItemThresholds from the
// aux table. Called after scanSubRow on a hot read path (Get, List, the
// scan tick) when the caller wants the full threshold view. Two cases:
//
//   - sub.BillingThresholds is non-nil from scanSubRow because amount_gte
//     was set: append item rows (if any) to the existing slice.
//
//   - sub.BillingThresholds is nil because amount_gte was NULL: if any
//     item rows exist, allocate a new struct with the aux rows; otherwise
//     leave nil.
//
// Empty aux rows + NULL amount_gte means no thresholds — sub stays nil.
func hydrateThresholds(ctx context.Context, tx *sql.Tx, sub *domain.Subscription) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT subscription_item_id, usage_gte
		FROM subscription_item_thresholds
		WHERE subscription_id = $1
		ORDER BY subscription_item_id ASC
	`, sub.ID)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	var items []domain.SubscriptionItemThreshold
	for rows.Next() {
		var t domain.SubscriptionItemThreshold
		var usageGTE string
		if err := rows.Scan(&t.SubscriptionItemID, &usageGTE); err != nil {
			return err
		}
		dec, err := decimal.NewFromString(usageGTE)
		if err != nil {
			return fmt.Errorf("parse usage_gte for item %s: %w", t.SubscriptionItemID, err)
		}
		t.UsageGTE = dec
		items = append(items, t)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(items) == 0 {
		// No per-item thresholds. Leave existing struct as-is (which may
		// have an amount-only threshold from scanSubRow), or nil.
		return nil
	}
	if sub.BillingThresholds == nil {
		// Item-only thresholds: scanSubRow allocates the struct only when
		// amount_gte is set, so the row's real reset_cycle value was
		// discarded — read it back rather than guess. Fabricating `true`
		// here made every item-only threshold sub re-anchor its cycle on
		// fire regardless of the stored flag (found live, FLOW B14
		// 2026-07-21: engine + API both consumed the fabricated value).
		var resetCycle bool
		if err := tx.QueryRowContext(ctx, `
			SELECT billing_threshold_reset_cycle FROM subscriptions WHERE id = $1
		`, sub.ID).Scan(&resetCycle); err != nil {
			return fmt.Errorf("read reset_cycle for item thresholds on %s: %w", sub.ID, err)
		}
		sub.BillingThresholds = &domain.BillingThresholds{
			ResetBillingCycle: resetCycle,
			ItemThresholds:    items,
		}
		return nil
	}
	sub.BillingThresholds.ItemThresholds = items
	return nil
}

func scanItemDest(it *domain.SubscriptionItem) []any {
	return []any{
		&it.ID, &it.TenantID, &it.SubscriptionID, &it.PlanID, &it.Quantity, &it.Metadata,
		&it.PendingPlanID, &it.PendingPlanEffectiveAt, &it.PlanChangedAt,
		&it.CreatedAt, &it.UpdatedAt,
	}
}

// subscriptionOrderBy validates sort + dir against a closed allow-list
// (no SQL injection) and adds a deterministic id tie-break matching
// the primary direction. Same pattern as invoiceOrderBy. Unknown sort
// keys default to created_at.
func subscriptionOrderBy(sort, dir string) string {
	col := subscriptionSortColumn(sort)
	d := "DESC"
	if dir == "asc" {
		d = "ASC"
	}
	return col + " " + d + ", s.id " + d
}

func subscriptionSortColumn(key string) string {
	switch key {
	case "next_billing_at":
		return "s.next_billing_at"
	case "status":
		return "s.status"
	case "display_name", "name":
		// display_name is a column on subscriptions itself (subCols),
		// not derived from customers — the previous created_at proxy
		// (and its JOIN-required comment) was wrong about the schema.
		return "s.display_name"
	case "trial_end_at":
		return "s.trial_end_at"
	default:
		return "s.created_at"
	}
}

func buildSubWhere(f ListFilter) (string, []any) {
	var clauses []string
	var args []any
	idx := 1

	// Joining items only when PlanID is set keeps the common list path
	// (no plan filter) off the join entirely.
	hasPlanFilter := f.PlanID != ""

	if f.CustomerID != "" {
		clauses = append(clauses, fmt.Sprintf("s.customer_id = $%d", idx))
		args = append(args, f.CustomerID)
		idx++
	}
	if hasPlanFilter {
		clauses = append(clauses, fmt.Sprintf("si.plan_id = $%d", idx))
		args = append(args, f.PlanID)
		idx++
	}
	if f.Status != "" {
		clauses = append(clauses, fmt.Sprintf("s.status = $%d", idx))
		args = append(args, f.Status)
		idx++
	}
	if f.Search != "" {
		// One placeholder reused across both columns — Postgres
		// allows repeating $N, and it keeps the pattern arg single-
		// source. Metacharacters escaped so "100%" matches literally.
		clauses = append(clauses, fmt.Sprintf("(s.display_name ILIKE $%d OR s.code ILIKE $%d)", idx, idx))
		args = append(args, "%"+postgres.EscapeLike(f.Search)+"%")
	}

	prefix := ""
	if hasPlanFilter {
		// AND si.deleted_at IS NULL — only live items count for the
		// plan filter (migration 0102 soft-delete model).
		prefix = " JOIN subscription_items si ON si.subscription_id = s.id AND si.deleted_at IS NULL"
	}

	if len(clauses) == 0 {
		return prefix, args
	}
	where := prefix + " WHERE "
	for i, c := range clauses {
		if i > 0 {
			where += " AND "
		}
		where += c
	}
	return where, args
}

// CountLiveSubsByPlan returns the number of subscriptions referencing
// the given plan whose status is not canceled or archived. Used by
// pricing.Service.UpdatePlan (ADR-034) to gate billing-affecting
// field mutations — once any live sub attaches, the plan's base
// price / timing / meter set is frozen.
//
// "Live" excludes canceled + archived. Draft / active / trialing /
// paused all qualify because they can still produce future invoices
// and need deterministic terms.
func (s *PostgresStore) CountLiveSubsByPlan(ctx context.Context, tenantID, planID string) (int, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return 0, err
	}
	defer postgres.Rollback(tx)

	var count int
	err = tx.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT s.id)
		FROM subscriptions s
		JOIN subscription_items si ON si.subscription_id = s.id AND si.deleted_at IS NULL
		WHERE si.plan_id = $1
		  AND s.status NOT IN ('canceled', 'archived')
	`, planID).Scan(&count)
	if err != nil {
		return 0, err
	}
	return count, nil
}

// ListItemChangesInPeriod returns every subscription_item_changes row
// for the given subscription whose changed_at falls within
// (periodStart, periodEnd]. Drives segment-aware cycle-close billing —
// each row demarcates a [pre-change, post-change] boundary the engine
// uses to bill each segment at its own plan + quantity rate.
//
// Range is exclusive-left, inclusive-right: a change exactly at
// periodEnd (a scheduled plan swap firing at the boundary) belongs to
// the closing period, NOT the next period. Same-instant rows (frozen
// sim time: an add and a plan swap can share one changed_at) tie-break
// by wall-clock created_at — ids are random hex and carry no sequence,
// and id-ordering was observed on real data putting a 'plan' row
// before its own item's 'add', which walks the segments at the WRONG
// plan (ADR-101 Phase 2 root-cause fix; previously "stable but
// arbitrary").
func (s *PostgresStore) ListItemChangesInPeriod(ctx context.Context, tenantID, subscriptionID string, periodStart, periodEnd time.Time) ([]domain.SubscriptionItemChange, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	rows, err := tx.QueryContext(ctx, `
		SELECT id, tenant_id, subscription_id,
		       COALESCE(subscription_item_id, ''), change_type,
		       COALESCE(from_plan_id, ''), COALESCE(to_plan_id, ''),
		       COALESCE(from_quantity, 0), COALESCE(to_quantity, 0),
		       changed_at, created_at
		FROM subscription_item_changes
		WHERE subscription_id = $1
		  AND changed_at > $2
		  AND changed_at <= $3
		ORDER BY changed_at ASC, created_at ASC, id ASC
	`, subscriptionID, periodStart, periodEnd)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []domain.SubscriptionItemChange
	for rows.Next() {
		var c domain.SubscriptionItemChange
		if err := rows.Scan(&c.ID, &c.TenantID, &c.SubscriptionID,
			&c.SubscriptionItemID, &c.ChangeType,
			&c.FromPlanID, &c.ToPlanID,
			&c.FromQuantity, &c.ToQuantity,
			&c.ChangedAt, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Subscription scan now includes PlanChangedAt on items, but the bytes
// exchange via JSON. pgx returns bytea for JSONB by default; store/consume
// raw bytes on the Metadata field so the caller owns the encoding policy.
var _ = sql.ErrNoRows // retain import

// ErrTrialCancelDue / ErrTrialCancelConflict live in domain (the billing
// engine routes on them without a peer import); aliased here for the
// package's own callers.
var (
	ErrTrialCancelDue      = domain.ErrTrialCancelDue
	ErrTrialCancelConflict = domain.ErrTrialCancelConflict
)

// CancelAtTrialEnd is the dedicated trialing→canceled transition (ADR-069):
// trials are free, so it emits NO invoice, stamps canceled_at = trial_end_at
// (never the observing site's `now` — the engine path fires up to a full
// interval late), and clears the schedule fields. The CAS predicates on the
// exact state that justified the cancel — observed trial_end_at (an
// ExtendTrial in the gap must win) and a schedule actually due at trial end
// (a ClearScheduledCancel in the gap must win) — so a customer who rescinded
// or was extended is never terminated by a stale snapshot.
func (s *PostgresStore) CancelAtTrialEnd(ctx context.Context, tenantID, id string, observedTrialEnd time.Time) (domain.Subscription, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.Subscription{}, err
	}
	defer postgres.Rollback(tx)

	now := clock.Now(ctx)
	var sub domain.Subscription
	err = scanSubRow(tx.QueryRowContext(ctx, `
		UPDATE subscriptions
		SET status = 'canceled',
		    canceled_at = trial_end_at,
		    cancel_at = NULL,
		    cancel_at_period_end = false,
		    updated_at = $1
		WHERE id = $2
		  AND status = 'trialing'
		  AND trial_end_at = $3
		  AND (cancel_at_period_end = true OR (cancel_at IS NOT NULL AND cancel_at <= trial_end_at))
		RETURNING `+subCols,
		now, id, observedTrialEnd,
	), &sub)
	if err == sql.ErrNoRows {
		return domain.Subscription{}, ErrTrialCancelConflict
	}
	if err != nil {
		return domain.Subscription{}, err
	}
	if err := hydrateSubChildrenTx(ctx, tx, &sub); err != nil {
		return domain.Subscription{}, err
	}
	// ADR-101: no interval work — the CAS admits only trialing rows, and a
	// trialing sub has never opened an interval (the activation writer
	// would have been the first). Nothing to seal on a free cancel.
	// Free trialing→canceled (ADR-069) — enqueue in-tx with the provenance
	// both prior dispatch sites (service + engine) stamped.
	extra := map[string]any{"canceled_by": "schedule", "reason": "trial_end_cancel"}
	if sub.CanceledAt != nil {
		extra["canceled_at"] = sub.CanceledAt.UTC()
	}
	if err := s.enqueueLifecycle(ctx, tx, domain.EventSubscriptionCanceled, sub, extra); err != nil {
		return domain.Subscription{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.Subscription{}, err
	}
	return sub, nil
}

// subRowWithExtra lets StreamForExport reuse scanSubRow unchanged while
// scanning EXTRA trailing columns of its own. scanSubRow owns the subscription
// column list and its dest order; duplicating that here to bolt one column on
// the end is how the export's columns drift from the API's.
type subRowWithExtra struct {
	rows  *sql.Rows
	extra []any
}

func (r subRowWithExtra) Scan(dest ...any) error {
	return r.rows.Scan(append(dest, r.extra...)...)
}

// StreamForExport walks EVERY subscription in the date range in ONE snapshot
// transaction. See customer.PostgresStore.StreamForExport for why an export
// takes a single snapshot instead of paging a list method (issue #475).
//
// plan_ids is an AGGREGATE in the query, not a second lookup per row. List
// hydrates each subscription's items with its own follow-up query, which the
// export cannot do: a second query on the same transaction while the row cursor
// is open blocks on the connection. Doing it as a correlated aggregate also
// deletes the N+1 the export was paying for a single pipe-joined string.
func (s *PostgresStore) StreamForExport(ctx context.Context, tenantID string, from, to time.Time, fn func(sub domain.Subscription, planIDs string) error) error {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)

	// Tenant + livemode isolation is RLS's (TxTenant), as in List.
	where := ""
	var args []any
	if !from.IsZero() {
		args = append(args, from)
		where += fmt.Sprintf(" AND s.created_at >= $%d", len(args))
	}
	if !to.IsZero() {
		args = append(args, to)
		where += fmt.Sprintf(" AND s.created_at <= $%d", len(args))
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT `+qualifiedSubCols("s")+`,
			COALESCE((
				SELECT string_agg(si.plan_id, '|' ORDER BY si.created_at, si.id)
				FROM subscription_items si
				WHERE si.subscription_id = s.id
			), '') AS plan_ids
		FROM subscriptions s
		WHERE TRUE`+where+`
		ORDER BY s.created_at DESC, s.id DESC`, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var sub domain.Subscription
		var planIDs string
		if err := scanSubRow(subRowWithExtra{rows: rows, extra: []any{&planIDs}}, &sub); err != nil {
			return err
		}
		if err := fn(sub, planIDs); err != nil {
			return err
		}
	}
	return rows.Err()
}

// CustomerSubPlanCurrencies returns the distinct (sub code, plan currency)
// pairs across the customer's non-terminal subscriptions — including pending
// plan swaps, which will bill at the next cycle boundary — excluding one sub
// id ("" excludes none). Backs the ADR-100 customer currency pin: one
// customer's live billing stays in one currency.
func (s *PostgresStore) CustomerSubPlanCurrencies(ctx context.Context, tenantID, customerID, excludeSubID string) ([]SubPlanCurrency, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)
	rows, err := tx.QueryContext(ctx, `
		SELECT DISTINCT sub.code, p.currency
		FROM subscriptions sub
		JOIN subscription_items si
		  ON si.subscription_id = sub.id AND si.deleted_at IS NULL
		JOIN plans p
		  ON p.id = si.plan_id OR p.id = si.pending_plan_id
		WHERE sub.customer_id = $1
		  AND sub.status IN ('draft', 'trialing', 'active')
		  AND sub.id <> $2
		ORDER BY sub.code`,
		customerID, excludeSubID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []SubPlanCurrency
	for rows.Next() {
		var sc SubPlanCurrency
		if err := rows.Scan(&sc.SubCode, &sc.Currency); err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, tx.Commit()
}
