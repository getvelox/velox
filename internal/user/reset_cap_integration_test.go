package user_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/user"
)

// The per-account password-reset send cap lives in password_reset_tokens
// itself: the rows ARE the record of what was sent, so the budget is
// cluster-wide by construction and needs no Redis. Real Postgres because the
// guarantee is one transaction's lock + count + insert; a fake would only
// prove the fake.

func mkResetUser(t *testing.T, store *user.PostgresStore, ctx context.Context, email string) string {
	t.Helper()
	hash, err := user.HashPassword("original-pw-12ch")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	u, err := store.Create(ctx, email, hash)
	if err != nil {
		t.Fatalf("create user %s: %v", email, err)
	}
	return u.ID
}

// TestCreateResetToken_CapsSendsPerAccount: three links per account per
// window, the fourth writes nothing and reports ErrResetSendCapped, and the
// window rolls. An unrelated account is untouched — a cap that also stopped
// everyone else's resets would be a denial of service.
// Mutation check: delete the `sent >= ResetSendsPerWindow` branch in
// CreateResetToken → the fourth issue succeeds → this fails.
func TestCreateResetToken_CapsSendsPerAccount(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test — requires postgres")
	}
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	_ = testutil.CreateTestTenant(t, db, "Reset Cap")
	store := user.NewPostgresStore(db)
	victim := mkResetUser(t, store, ctx, "victim@resetcap.test")
	bystander := mkResetUser(t, store, ctx, "bystander@resetcap.test")

	for i := 0; i < user.ResetSendsPerWindow; i++ {
		if _, err := store.CreateResetToken(ctx, victim, "hash-a-"+time.Now().Format("150405.000000000"), time.Now().Add(time.Hour)); err != nil {
			t.Fatalf("send %d must be allowed: %v", i+1, err)
		}
	}
	if _, err := store.CreateResetToken(ctx, victim, "hash-over-cap", time.Now().Add(time.Hour)); !errors.Is(err, user.ErrResetSendCapped) {
		t.Fatalf("send %d: err = %v, want ErrResetSendCapped", user.ResetSendsPerWindow+1, err)
	}

	// Control: a different account is unaffected by the victim's budget.
	if _, err := store.CreateResetToken(ctx, bystander, "hash-bystander", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("an unrelated account must still be able to reset: %v", err)
	}

	// The window rolls: age the victim's sends past it and the next is allowed.
	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE password_reset_tokens SET created_at = now() - $2::interval - interval '1 minute' WHERE user_id = $1`,
		victim, user.ResetSendWindow.String()); err != nil {
		t.Fatalf("age tokens: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateResetToken(ctx, victim, "hash-after-window", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("after the window rolls the account must be able to reset again: %v", err)
	}
}

// TestCreateResetToken_ConcurrentRacersRespectCap is why the count and the
// insert share a transaction that locks the user row first: without the lock
// several racers read the same "2 sent" and all insert, and the cap becomes a
// suggestion. Mutation check: drop the `SELECT 1 FROM users … FOR UPDATE` →
// more than ResetSendsPerWindow rows land → this fails.
func TestCreateResetToken_ConcurrentRacersRespectCap(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test — requires postgres")
	}
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	_ = testutil.CreateTestTenant(t, db, "Reset Cap Race")
	store := user.NewPostgresStore(db)
	victim := mkResetUser(t, store, ctx, "victim@resetrace.test")

	const racers = 8
	var wg sync.WaitGroup
	issued := make(chan int, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := store.CreateResetToken(ctx, victim, "race-hash-"+time.Now().Format("150405.000000000")+"-"+string(rune('a'+i)), time.Now().Add(time.Hour))
			switch {
			case err == nil:
				issued <- i
			case errors.Is(err, user.ErrResetSendCapped):
			default:
				t.Errorf("racer %d: unexpected error %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	close(issued)
	n := 0
	for range issued {
		n++
	}
	if n != user.ResetSendsPerWindow {
		t.Fatalf("%d racers were issued a link; want exactly %d", n, user.ResetSendsPerWindow)
	}

	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatal(err)
	}
	defer postgres.Rollback(tx)
	var rows int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM password_reset_tokens WHERE user_id = $1`, victim).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != user.ResetSendsPerWindow {
		t.Fatalf("password_reset_tokens holds %d rows for the account; want %d", rows, user.ResetSendsPerWindow)
	}
}
