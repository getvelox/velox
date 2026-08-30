// Package leadertest lets store tests call leader-only methods with a real,
// live lease token without running a heartbeat: one admin statement takes the
// role for an hour and the token rides the returned ctx.
package leadertest

import (
	"context"
	"database/sql"
	"testing"

	"github.com/sagarsuperuser/velox/internal/platform/leader"
)

// Token acquires each role for one hour on admin (bypassing cadence and
// pause) and returns base with the tokens attached — one ctx can drive
// several roles' funnels (an end-to-end test that bills and then duns).
// t.Cleanup releases the rows. Tests using it must not run in parallel
// with another test holding the same role: the row is cluster state.
func Token(t *testing.T, admin *sql.DB, base context.Context, roles ...leader.Role) context.Context {
	t.Helper()
	ctx := base
	for _, role := range roles {
		ctx = one(t, admin, ctx, role)
	}
	return ctx
}

func one(t *testing.T, admin *sql.DB, base context.Context, role leader.Role) context.Context {
	t.Helper()
	ctx := base
	var token int64
	err := admin.QueryRowContext(ctx, `
INSERT INTO leader_leases (role, holder_token, holder_id, acquired_at, heartbeat_at, expires_at)
VALUES ($1, (extract(epoch FROM clock_timestamp()) * 1000)::bigint, 'test', now(), now(), now() + interval '1 hour')
ON CONFLICT (role) DO UPDATE
   SET holder_token = leader_leases.holder_token + 1, holder_id = 'test',
       acquired_at = now(), heartbeat_at = now(), expires_at = now() + interval '1 hour',
       paused_at = NULL, paused_by = NULL, pause_reason = NULL
RETURNING holder_token`, string(role)).Scan(&token)
	if err != nil {
		t.Fatalf("leadertest.Token(%s): %v", role, err)
	}
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(),
			`UPDATE leader_leases SET holder_id = NULL, acquired_at = NULL, heartbeat_at = NULL, expires_at = NULL WHERE role = $1 AND holder_token = $2`,
			string(role), token)
	})
	return leader.WithToken(base, role, token)
}
