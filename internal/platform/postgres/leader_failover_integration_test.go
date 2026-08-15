package postgres_test

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// maxDetectionWindowSecs is the contract: after a leader's host vanishes
// without closing its socket, Postgres must reap the session — and so release
// the singleton lock — within this many seconds. The billing scheduler ticks
// on the order of a minute, so a few minutes costs at most a few skipped
// ticks. The Postgres default of 7875s (2h11m) does not qualify.
const maxDetectionWindowSecs = 300

// TestLeaderFailover_LockConnBoundsDetectionWindow is the regression gate for
// the silent-halt failure mode described on TryAdvisoryLock.
//
// Why this exists rather than a comment: if a leader replica's host disappears
// without a TCP FIN (partition, power loss, VM terminate), the advisory lock
// stays held by a session whose client is gone. Every surviving replica's tick
// then takes the !acquired branch and returns — logged at Debug, so a
// production log at Info level says nothing at all. Billing stops silently.
// The only thing bounding that outage is how fast Postgres decides the peer is
// dead, which is a pure function of the keepalive settings on the lock
// connection.
//
// A regression here is invisible to every other test: the lock still works,
// exclusion still works, release still works. Only recovery time changes.
func TestLeaderFailover_LockConnBoundsDetectionWindow(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	const key int64 = 99_888_101

	lock, ok, err := db.TryAdvisoryLock(ctx, key)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if !ok {
		t.Fatal("acquire should have succeeded on a fresh key")
	}
	defer lock.Release()

	idle, interval, count, window, err := lock.KeepaliveSettings(ctx)
	if err != nil {
		t.Fatalf("read keepalive settings: %v", err)
	}
	t.Logf("lock connection keepalives: idle=%ds interval=%ds count=%d -> detection window %ds",
		idle, interval, count, window)

	if window > maxDetectionWindowSecs {
		t.Fatalf("dead-leader detection window is %ds (idle=%d + interval=%d x count=%d), which "+
			"exceeds the %ds contract. A leader whose host vanishes without closing its socket "+
			"would hold the billing lock that long while every replica silently skips its tick.",
			window, idle, interval, count, maxDetectionWindowSecs)
	}
}

// TestLeaderFailover_DefaultConnHasUnboundedWindow is the control for the test
// above, and the reason to trust it.
//
// A passing assertion on the lock connection proves nothing alone — the server
// could simply be configured with short keepalives, in which case the code
// under test could be deleted and the gate would still pass. This asserts the
// opposite on an ordinary pooled connection: the inherited default is large.
// Together they show the narrow window is caused by TryAdvisoryLock and not by
// the environment.
func TestLeaderFailover_DefaultConnHasUnboundedWindow(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	conn, err := db.Pool.Conn(ctx)
	if err != nil {
		t.Fatalf("checkout plain conn: %v", err)
	}
	defer func() { _ = conn.Close() }()

	idle, interval, count := readKeepalives(t, ctx, conn)
	window := idle + interval*count
	t.Logf("inherited default keepalives: idle=%ds interval=%ds count=%d -> window %ds",
		idle, interval, count, window)

	if window <= maxDetectionWindowSecs {
		t.Skipf("server already ships a bounded keepalive window (%ds); the lock-connection "+
			"assertion is no longer distinguishable from the environment", window)
	}
}

// TestLeaderFailover_SIGKILLedLeaderReleasesLock covers the death mode that IS
// self-healing, so the keepalive fix is not mistaken for covering both.
//
// This is the literal Track A scenario: a real leader process takes the real
// lock through the real code path and is SIGKILLed mid-hold — no defers run,
// no unlock is sent. The kernel closes its sockets, Postgres reads EOF, and a
// surviving replica must pick up.
//
// It runs a subprocess rather than simulating one in-process, because every
// in-process shortcut models the wrong thing: sql.Conn.Close() returns the
// session to the pool with the lock still held, and sql.DB.Close() closes only
// IDLE connections — the lock connection is checked out, so its socket
// survives and the lock does not release. (Both were tried; the second is why
// this test looked like a product bug before it was a test bug.) Only an
// actual process death closes an actual checked-out socket.
func TestLeaderFailover_SIGKILLedLeaderReleasesLock(t *testing.T) {
	follower := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const key int64 = 99_888_102

	leader := exec.Command(os.Args[0], "-test.run=TestLeaderHelperProcess")
	leader.Env = append(os.Environ(),
		leaderHelperEnv+"=1",
		"VELOX_TEST_LOCK_KEY="+strconv.FormatInt(key, 10),
	)
	stdout, err := leader.StdoutPipe()
	if err != nil {
		t.Fatalf("leader stdout pipe: %v", err)
	}
	leader.Stderr = os.Stderr
	if err := leader.Start(); err != nil {
		t.Fatalf("start leader subprocess: %v", err)
	}
	defer func() { _ = leader.Process.Kill(); _, _ = leader.Process.Wait() }()

	// Wait for the subprocess to report that it actually holds the lock.
	held := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			if line := strings.TrimSpace(sc.Text()); line == leaderHeldMarker {
				held <- line
				return
			}
		}
		close(held)
	}()
	select {
	case _, ok := <-held:
		if !ok {
			t.Fatal("leader subprocess exited without acquiring the lock")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("leader subprocess never reported holding the lock")
	}

	// Control: while the leader holds it, a follower must be locked out.
	if l2, acquired, err := follower.TryAdvisoryLock(ctx, key); err != nil {
		t.Fatalf("follower probe while held: %v", err)
	} else if acquired {
		l2.Release()
		t.Fatal("follower acquired a key the leader subprocess still holds — exclusion is broken")
	}

	// Kill the leader outright. No defers, no unlock, no graceful anything.
	if err := leader.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL leader: %v", err)
	}
	state, err := leader.Process.Wait()
	if err != nil {
		t.Fatalf("wait for killed leader: %v", err)
	}
	// Prove the signal is what ended it. Without this the test passes just as
	// happily when the helper dies on its own (a panic also closes the socket),
	// which would make this a test of Go's runtime rather than of failover.
	ws, ok := state.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("leader did not die from SIGKILL (state=%v); the test would be measuring "+
			"some other exit path", state)
	}

	// Treatment: a surviving replica must take over promptly.
	start := time.Now()
	deadline := start.Add(20 * time.Second)
	for {
		l3, acquired, err := follower.TryAdvisoryLock(ctx, key)
		if err != nil {
			t.Fatalf("follower acquire after leader SIGKILL: %v", err)
		}
		if acquired {
			l3.Release()
			t.Logf("follower took over %v after the leader was SIGKILLed", time.Since(start).Round(time.Millisecond))
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("lock still held 20s after the leader was SIGKILLed; the EOF path that " +
				"makes process death self-healing is not working, and every replica would " +
				"skip its billing tick until TCP keepalives expire")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

const (
	leaderHelperEnv  = "VELOX_TEST_LEADER_HOLD"
	leaderHeldMarker = "VELOX_LEADER_HOLDS_LOCK"
)

// TestLeaderHelperProcess is not a test. It is the leader replica that
// TestLeaderFailover_SIGKILLedLeaderReleasesLock kills: when the guard env var
// is set it takes the advisory lock through the production code path,
// announces it, and blocks forever waiting to be killed. Without the env var
// it returns immediately, so a normal `go test ./...` run skips it.
func TestLeaderHelperProcess(t *testing.T) {
	if os.Getenv(leaderHelperEnv) != "1" {
		return
	}
	key, err := strconv.ParseInt(os.Getenv("VELOX_TEST_LOCK_KEY"), 10, 64)
	if err != nil {
		return
	}

	pool, err := sql.Open("pgx", appDSN())
	if err != nil {
		fmt.Fprintf(os.Stderr, "helper: open pool: %v\n", err)
		return
	}
	pool.SetMaxOpenConns(2)
	db := postgres.NewDB(pool, 10*time.Second)

	lock, ok, err := db.TryAdvisoryLock(context.Background(), key)
	if err != nil {
		fmt.Fprintf(os.Stderr, "helper: acquire: %v\n", err)
		return
	}
	if !ok {
		fmt.Fprintln(os.Stderr, "helper: lock already held")
		return
	}
	_ = lock // never released — this process is about to be killed

	fmt.Println(leaderHeldMarker)
	_ = os.Stdout.Sync()
	// Sleep rather than select{}: an empty select parks every goroutine, and
	// Go's deadlock detector then panics the process on its own — which closes
	// the socket and makes the test pass without any SIGKILL ever landing.
	time.Sleep(10 * time.Minute)
}

func readKeepalives(t *testing.T, ctx context.Context, conn *sql.Conn) (idle, interval, count int) {
	t.Helper()
	if err := conn.QueryRowContext(ctx,
		`SELECT current_setting('tcp_keepalives_idle')::int,
		        current_setting('tcp_keepalives_interval')::int,
		        current_setting('tcp_keepalives_count')::int`,
	).Scan(&idle, &interval, &count); err != nil {
		t.Fatalf("read keepalive settings: %v", err)
	}
	return idle, interval, count
}

// appDSN mirrors testutil's app-role connection string so the leader can hold
// its own pool. Kept local rather than exported from testutil: only this test
// needs a second pool, and widening testutil's surface for one caller is worse
// than one duplicated default.
func appDSN() string {
	if v := os.Getenv("TEST_DATABASE_URL"); v != "" {
		return v
	}
	return "postgres://velox_test_app:velox_test_app@localhost:5432/velox_test?sslmode=disable"
}
