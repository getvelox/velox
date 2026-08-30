package leader

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// Executor is the database surface the manager needs: a pool that runs
// single autocommit statements. *sql.DB satisfies it.
type Executor interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// LostFunc is told, once, why a led tick lost its lease: "takeover",
// "paused", "released", or "heartbeat_timeout". Wired to a Prometheus
// counter by the API server; nil is fine.
type LostFunc func(role Role, reason string)

// Manager leads roles for one process.
type Manager struct {
	db       Executor
	holderID string
	onLost   LostFunc

	driftOnce sync.Once
}

// New returns a manager whose statements run on db.
func New(db Executor, onLost LostFunc) *Manager {
	return &Manager{db: db, holderID: holderID(), onLost: onLost}
}

// HolderID is this process's lease identity (observability).
func (m *Manager) HolderID() string { return m.holderID }

// The four statements. All autocommit; no SET, no session state, no BEGIN;
// a pooler may route each to a different backend. Every timestamp is
// stamped and compared server-side.
const (
	// ACQUIRE — once per poll. 1 row = lead this tick; 0 rows = not due, held,
	// or paused. Racing replicas serialize on the row lock; ON CONFLICT DO
	// UPDATE re-evaluates its WHERE against the winner's row. A missing row is
	// recreated by the INSERT path with an epoch-ms token (above any issued).
	// A typo'd role violates the CHECK and errors loudly.
	sqlAcquire = `
INSERT INTO leader_leases (role, holder_token, holder_id, acquired_at, heartbeat_at, expires_at)
VALUES ($1, (extract(epoch FROM clock_timestamp()) * 1000)::bigint, $2, now(), now(), now() + make_interval(secs => $3))
ON CONFLICT (role) DO UPDATE
   SET holder_token = leader_leases.holder_token + 1,
       holder_id    = EXCLUDED.holder_id,
       acquired_at  = now(),
       heartbeat_at = now(),
       expires_at   = now() + make_interval(secs => $3)
 WHERE leader_leases.paused_at IS NULL
   AND (leader_leases.expires_at IS NULL OR leader_leases.expires_at <= now())
   AND (leader_leases.last_tick_ended_at IS NULL
        OR leader_leases.last_tick_ended_at <= now() - make_interval(secs => $4))
RETURNING holder_token, now()`

	// RENEW — the heartbeat. 0 rows is DEFINITIVE loss (takeover, release, or
	// pause — pause nulls the holder). No expires_at predicate: a holder may
	// renew its own expired-but-untaken lease; a renew racing a takeover on
	// an expired row is decided by the row lock. A statement ERROR is a missed
	// beat, retried next beat.
	sqlRenew = `
UPDATE leader_leases
   SET heartbeat_at = now(),
       expires_at   = now() + make_interval(secs => $3)
 WHERE role = $1 AND holder_token = $2 AND holder_id IS NOT NULL
RETURNING now()`

	// WHY-LOST — once, after a 0-row RENEW, for the log line only.
	sqlWhyLost = `SELECT holder_id, holder_token, paused_by IS NOT NULL FROM leader_leases WHERE role = $1`

	// RELEASE — after work returns. $3 is the outcome: 0 = completed (stamp
	// last_tick_ended_at = now(): due again after one interval); 1 =
	// interrupted by the parent ctx (clean SIGTERM: leave the cadence alone
	// so the role is due on another replica at its next poll, R3); 2 = lease
	// lost or heartbeat timed out (due again after a bounded cooldown, so a
	// slow database cannot turn an hourly tick into a partial tick every few
	// seconds). 0 rows is fine (already lost).
	sqlRelease = `
UPDATE leader_leases
   SET holder_id = NULL, acquired_at = NULL, heartbeat_at = NULL, expires_at = NULL,
       last_tick_holder   = holder_id,
       last_tick_ended_at = CASE $3::int
                              WHEN 0 THEN now()
                              WHEN 1 THEN last_tick_ended_at
                              ELSE now() - make_interval(secs => $4) + LEAST(make_interval(secs => $4), make_interval(secs => $5))
                            END
 WHERE role = $1 AND holder_token = $2 AND holder_id IS NOT NULL`
)

// Lead implements Gate.
func (m *Manager) Lead(ctx context.Context, role Role, interval time.Duration, work func(context.Context)) (bool, error) {
	token, dbNow, ok, err := m.acquire(ctx, role, interval)
	if err != nil || !ok {
		return false, err
	}

	workCtx, cancel := context.WithCancelCause(WithToken(ctx, role, token))
	hb := m.startHeartbeat(role, token, dbNow, cancel)

	// RELEASE always runs — on a panic too (the runner's recover wraps Lead,
	// so a panic here propagates after the deferred release).
	defer func() {
		hb.stop()
		outcome := releaseCompleted
		switch cause := context.Cause(workCtx); {
		case cause == nil:
		case errors.Is(cause, ErrLeaseLost):
			outcome = releaseLost
		default:
			outcome = releaseInterrupted // parent cancelled: clean shutdown
		}
		cancel(nil)
		m.release(role, token, interval, outcome)
	}()

	work(workCtx)
	return true, nil
}

func (m *Manager) acquire(ctx context.Context, role Role, interval time.Duration) (token int64, dbNow time.Time, ok bool, err error) {
	sctx, cancel := context.WithTimeout(ctx, StatementTimeout)
	defer cancel()
	err = m.db.QueryRowContext(sctx, sqlAcquire, string(role), m.holderID, LeaseTTL.Seconds(), interval.Seconds()).Scan(&token, &dbNow)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, time.Time{}, false, nil
	}
	if err != nil {
		return 0, time.Time{}, false, err
	}
	return token, dbNow, true, nil
}

// Release outcomes (the $3 of sqlRelease).
const (
	releaseCompleted   = 0
	releaseInterrupted = 1
	releaseLost        = 2
)

func (m *Manager) release(role Role, token int64, interval time.Duration, outcome int) {
	rctx, cancel := context.WithTimeout(context.Background(), StatementTimeout)
	defer cancel()
	if _, err := m.db.ExecContext(rctx, sqlRelease, string(role), token, outcome, interval.Seconds(), interruptedCooldown.Seconds()); err != nil {
		// The row stays held <= LeaseTTL; the next ACQUIRE accepts an expired
		// row and the role is due (last_tick_ended_at was not stamped).
		slog.Error("leader: release failed — lease expires on its own", "role", role, "token", token, "error", err)
	}
}

type heartbeat struct {
	done chan struct{}
	wg   sync.WaitGroup
}

func (h *heartbeat) stop() {
	close(h.done)
	h.wg.Wait()
}

// startHeartbeat renews every HeartbeatEvery on its OWN root context (the
// parent's cancellation must not kill the renewer before the release).
//
// lastAck anchors at the SEND instant of the last acknowledged renew, on the
// monotonic clock, and the abandon decision is taken BEFORE issuing the next
// renew: the DB stamped expires_at >= T_send + LeaseTTL on its own clock, so
// cancelling at T_send + AbandonAfter (< LeaseTTL - StatementTimeout) means
// no statement of ours starts after a successor could exist. Never an app
// timestamp against a DB timestamp; the RETURNING now() is used only to
// observe DB-vs-app drift as a duration-against-duration.
func (m *Manager) startHeartbeat(role Role, token int64, dbNowAtAcquire time.Time, cancel context.CancelCauseFunc) *heartbeat {
	h := &heartbeat{done: make(chan struct{})}
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		lastAck := time.Now()       // monotonic
		lastDBNow := dbNowAtAcquire // DB clock at the last ack
		ticker := time.NewTicker(HeartbeatEvery)
		defer ticker.Stop()
		for {
			select {
			case <-h.done:
				return
			case <-ticker.C:
			}
			if time.Since(lastAck) >= AbandonAfter {
				m.lost(role, token, "heartbeat_timeout", cancel)
				return
			}
			sendAt := time.Now()
			sctx, scancel := context.WithTimeout(context.Background(), StatementTimeout)
			var dbNow time.Time
			err := m.db.QueryRowContext(sctx, sqlRenew, string(role), token, LeaseTTL.Seconds()).Scan(&dbNow)
			scancel()
			switch {
			case err == nil:
				m.observeDrift(role, dbNow.Sub(lastDBNow), sendAt.Sub(lastAck))
				lastAck, lastDBNow = sendAt, dbNow
			case errors.Is(err, sql.ErrNoRows):
				m.lost(role, token, m.whyLost(role, token), cancel)
				return
			default:
				// A missed beat, not a loss: retried next beat; the abandon
				// check above bounds how long we tolerate it.
				slog.Warn("leader: renew failed — will retry", "role", role, "token", token, "error", err)
			}
		}
	}()
	return h
}

func (m *Manager) whyLost(role Role, token int64) string {
	sctx, cancel := context.WithTimeout(context.Background(), StatementTimeout)
	defer cancel()
	var holder sql.NullString
	var current int64
	var paused bool
	if err := m.db.QueryRowContext(sctx, sqlWhyLost, string(role)).Scan(&holder, &current, &paused); err != nil {
		return "takeover"
	}
	switch {
	case paused:
		return "paused"
	case current != token:
		return "takeover"
	default:
		return "released"
	}
}

func (m *Manager) lost(role Role, token int64, reason string, cancel context.CancelCauseFunc) {
	slog.Error("leader: lease lost mid-tick — cancelling the tick's work", "role", role, "token", token, "reason", reason, "holder", m.holderID)
	cancel(ErrLeaseLost)
	if m.onLost != nil {
		m.onLost(role, reason)
	}
}

// observeDrift compares the DB clock's advance to our monotonic advance over
// the same ack-to-ack span — a duration against a duration — and warns once
// if they disagree by more than a statement timeout: the app host's clock is
// irrelevant to correctness, but a skewed DB clock across a failover is
// worth a line in the log.
func (m *Manager) observeDrift(role Role, dbDelta, monoDelta time.Duration) {
	diff := dbDelta - monoDelta
	if diff < 0 {
		diff = -diff
	}
	if diff > StatementTimeout {
		m.driftOnce.Do(func() {
			slog.Warn("leader: database clock advanced differently from the process clock between heartbeats", "role", role, "db_delta", dbDelta, "process_delta", monoDelta)
		})
	}
}
