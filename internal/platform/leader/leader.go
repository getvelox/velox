// Package leader is singleton-role leadership as a ROW, not a Postgres
// session (ADR-114). One row per role in leader_leases; four single,
// self-contained statements — ACQUIRE, RENEW, RELEASE, and the operator's
// PAUSE/UNPAUSE — evaluated entirely on the database clock; a fencing token
// that every leader-only claim statement proves in its own snapshot
// (leader_fence(role, token)); a heartbeat that renews during long ticks and
// cancels the work when the lease is lost.
//
// Why a row: a session-scoped advisory lock's liveness is a TCP session (a
// vanished host held the billing lock for 2h11m — #797), it cannot be fenced
// (a superseded leader keeps issuing statements), and it strands under
// transaction-mode poolers. A row on the database clock has none of those
// properties. Nothing here compares an app timestamp to a DB timestamp:
// durations are measured against durations on the monotonic clock, and every
// DB predicate is now() against values written by now().
package leader

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"time"
)

// Role names one singleton loop. The list is pinned to migration 0174's
// CHECK constraint by TestLease_RolesSeededMatchGoConstants.
type Role string

const (
	RoleBilling         Role = "billing"
	RoleDunning         Role = "dunning"
	RoleWebhookOutbox   Role = "webhook_outbox"
	RoleEmailOutbox     Role = "email_outbox"
	RoleWebhookDelivery Role = "webhook_delivery"
)

// Roles is every role a binary may lead.
var Roles = []Role{RoleBilling, RoleDunning, RoleWebhookOutbox, RoleEmailOutbox, RoleWebhookDelivery}

// Timing. Compiled in, like the tick interval — a mis-set TTL is a silent
// double leader, not a tunable. TestLease_ConstantsLeaveNoElectionGap pins
// AbandonAfter + StatementTimeout < LeaseTTL: a holder that abandons itself
// stops issuing statements BEFORE any successor can be elected.
const (
	// LeaseTTL is how long a lease lives without a renew, on the DB clock.
	LeaseTTL = 10 * time.Second
	// HeartbeatEvery is the renew cadence while a tick runs.
	HeartbeatEvery = 3 * time.Second
	// StatementTimeout bounds every leadership statement.
	StatementTimeout = 2 * time.Second
	// AbandonAfter is how long a holder tolerates no acknowledged renew
	// (measured on the monotonic clock from the last renew's SEND instant)
	// before cancelling its own work: two missed beats.
	AbandonAfter = 6 * time.Second
	// MaxPoll caps how often a follower polls for a role: a dead leader's
	// role runs elsewhere within LeaseTTL + MaxPoll.
	MaxPoll = 5 * time.Second
	// interruptedCooldown is how soon an interrupted tick (lease lost,
	// SIGTERM, heartbeat timeout) is due again — bounded so a slow database
	// cannot turn an hourly tick into a partial tick every few seconds.
	interruptedCooldown = 60 * time.Second
)

var (
	// ErrLeaseLost is the cancellation cause of a tick whose lease was
	// taken over, paused, released, or could not be renewed in time.
	ErrLeaseLost = errors.New("leader: lease lost")
	// ErrNoToken is returned by Fence when a leader-only store method is
	// reached outside a led tick — no query runs, no silent unfenced fallback.
	ErrNoToken = errors.New("leader: ctx carries no lease token — leader-only store method reached outside a led tick")
)

// Gate is what scheduler.Run takes: one attempt to run ONE tick of role.
type Gate interface {
	// Lead returns (false, nil) when the role is not due, held elsewhere, or
	// paused — the caller simply polls again. When it leads, work runs with a
	// ctx that carries (role, token) and is cancelled with cause ErrLeaseLost
	// if the heartbeat cannot keep the lease; the lease is released after work
	// returns, on a background ctx, so a cancelled parent cannot skip it.
	Lead(ctx context.Context, role Role, interval time.Duration, work func(ctx context.Context)) (led bool, err error)
}

type tokenKey struct{}

type tokenVal struct {
	role  Role
	token int64
}

// WithToken stamps a led tick's (role, token) onto ctx. Only the manager (and
// the test doubles) mint tokens.
func WithToken(ctx context.Context, role Role, token int64) context.Context {
	return context.WithValue(ctx, tokenKey{}, tokenVal{role: role, token: token})
}

// Fence returns the (role, token) a leader-only store method must prove in
// its claim statement, or ErrNoToken.
func Fence(ctx context.Context) (Role, int64, error) {
	v, ok := ctx.Value(tokenKey{}).(tokenVal)
	if !ok || v.token <= 0 {
		return "", 0, ErrNoToken
	}
	return v.role, v.token, nil
}

// holderID is minted once per process: hostname:pid:8hex. Containers are
// all pid 1 and StatefulSet pods reuse hostnames, so the random suffix is
// what makes it unique. Observability only — never part of the fence.
func holderID() string {
	host, _ := os.Hostname()
	if host == "" {
		host = "unknown"
	}
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%s:%d:%s", host, os.Getpid(), hex.EncodeToString(b[:]))
}
