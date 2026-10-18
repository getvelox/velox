package testutil

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// lockWaitTimeout is generous because CI runners have been measured at 2 to
// 4.7 times slower than a laptop. A passing test waits only until the
// goroutine reaches the lock.
const lockWaitTimeout = 15 * time.Second

// WaitForLockWaiter returns once a backend in the test database is waiting on
// a lock while it runs a statement whose text contains queryLike (ILIKE, so
// case does not matter). A race test calls it after starting the goroutine
// that must queue behind a lock the test holds, and before the test lets that
// lock go.
//
// It replaces a fixed sleep. On a slow runner a sleep can end before the
// goroutine reaches the lock: the two sides then run one after the other and
// the test passes without the interleaving it says it proves.
//
// admin must be able to see other sessions' queries: pass AdminPool(t).
// Postgres keeps only the first track_activity_query_size bytes (1 KiB by
// default) of a statement in pg_stat_activity, so for a long statement match
// text near its start.
//
// If no such backend appears within 15 seconds the test fails with "no
// backend blocked". That usually means the code under test no longer takes
// the lock the test is about, or takes a different one first.
func WaitForLockWaiter(t *testing.T, ctx context.Context, admin *sql.DB, queryLike string) {
	t.Helper()
	deadline := time.Now().Add(lockWaitTimeout)
	for {
		var n int
		if err := admin.QueryRowContext(ctx, `
			SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock' AND query ILIKE $1
		`, "%"+queryLike+"%").Scan(&n); err != nil {
			t.Fatalf("WaitForLockWaiter: read pg_stat_activity: %v", err)
		}
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no backend blocked on a lock in a statement matching %q within %s", queryLike, lockWaitTimeout)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
