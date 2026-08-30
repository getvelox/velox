package leader_test

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/sagarsuperuser/velox/internal/platform/leader"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestLeaseFailover_SIGKILLedHolderReplacedWithinTTL measures the number
// docs/benchmarks/failure-correctness.md §5 quotes for the lease: a holder
// killed with SIGKILL mid-tick (no release, no FIN handling needed) is
// replaced by a successor no earlier than its lease could still be live and
// no later than LeaseTTL after its last renew. Two bounds, both asserted:
//
//   - lower: the successor must NOT take the role while the dead holder's
//     lease can still be valid — an early takeover is the dual-leader bug;
//   - upper: LeaseTTL + one poll of slack — a slower replacement is the
//     "billing silently stalled" bug the advisory lock had (95 s measured).
//
// The holder is a separate OS process (helper-process pattern) so the kill
// is a real SIGKILL: no deferred release runs, no heartbeat goroutine
// survives. The parent polls acquire every 100 ms with interval 0 (always
// due) — a stand-in for a replica's scheduler poll.
func TestLeaseFailover_SIGKILLedHolderReplacedWithinTTL(t *testing.T) {
	if os.Getenv("LEASE_HOLDER_HELPER") == "1" {
		t.Skip("helper process")
	}
	db := testutil.SetupTestDB(t)
	admin := testutil.AdminPool(t)
	resetRoles(t, admin)
	role := leader.RoleWebhookDelivery

	cmd := exec.Command(os.Args[0], "-test.run=^TestLeaseHolderHelperProcess$", "-test.v")
	cmd.Env = append(os.Environ(), "LEASE_HOLDER_HELPER=1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()

	// Wait for the holder to report it holds the role.
	sc := bufio.NewScanner(stdout)
	held := false
	for sc.Scan() {
		if sc.Text() == "HELD" {
			held = true
			break
		}
	}
	if !held {
		t.Fatal("holder never reported HELD")
	}
	_, holder, _ := rowToken(t, admin, role)
	if !holder.Valid {
		t.Fatal("row shows no holder after HELD")
	}
	// Let at least one renew land so the measurement starts from a renewed
	// lease, the way a real mid-tick holder looks.
	time.Sleep(leader.HeartbeatEvery + 500*time.Millisecond)

	killed := time.Now()
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()

	m := leader.New(db.Pool, nil)
	ctx := context.Background()
	var took time.Duration
	for {
		led, err := m.Lead(ctx, role, 0, func(context.Context) {})
		if err != nil {
			t.Fatalf("successor acquire: %v", err)
		}
		if led {
			took = time.Since(killed)
			break
		}
		if time.Since(killed) > leader.LeaseTTL+leader.MaxPoll {
			t.Fatalf("no successor within LeaseTTL+MaxPoll (%s) of SIGKILL", leader.LeaseTTL+leader.MaxPoll)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("SIGKILLed holder replaced after %s (LeaseTTL %s, last renew ≤ %s before the kill)", took.Round(10*time.Millisecond), leader.LeaseTTL, leader.HeartbeatEvery)

	// Lower bound: the kill landed at most HeartbeatEvery after the last
	// renew, so the lease was valid until at least LeaseTTL-HeartbeatEvery
	// after the kill. A takeover before that is a live lease being stolen.
	if minWait := leader.LeaseTTL - leader.HeartbeatEvery - 500*time.Millisecond; took < minWait {
		t.Fatalf("successor took over after %s — before the dead holder's lease could have expired (>= %s)", took, minWait)
	}
	if took > leader.LeaseTTL+time.Second {
		t.Fatalf("successor took %s; want <= LeaseTTL+1s (%s)", took, leader.LeaseTTL+time.Second)
	}
}

// TestLeaseHolderHelperProcess is the child: it opens its own app pool
// (never SetupTestDB — that would truncate the parent's tables), takes the
// role with a work func that blocks forever, prints HELD, and waits to be
// killed. The heartbeat goroutine renews it every HeartbeatEvery until then.
func TestLeaseHolderHelperProcess(t *testing.T) {
	if os.Getenv("LEASE_HOLDER_HELPER") != "1" {
		t.Skip("helper process only")
	}
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		url = "postgres://velox_test_app:velox_test_app@localhost:5432/velox_test?sslmode=disable"
	}
	pool, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatal(err)
	}
	m := leader.New(pool, nil)
	led, err := m.Lead(context.Background(), leader.RoleWebhookDelivery, 0, func(ctx context.Context) {
		fmt.Println("HELD")
		<-ctx.Done() // only a lost lease ends this; SIGKILL is the plan
	})
	fmt.Println("EXIT", led, err)
}
