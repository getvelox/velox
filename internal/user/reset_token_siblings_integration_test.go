package user_test

import (
	"context"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/user"
)

// TestConsumeResetToken_VoidsSiblingTokens: redeeming one password-reset
// token must kill every OTHER outstanding token for that user, in the same
// transaction.
//
// Single-use per token was never sufficient. Reset is requestable by anyone
// who knows the email, so one account can hold several live tokens at once —
// and before this fix, redeeming one left its siblings valid for the rest of
// their hour. Whoever held an earlier token (a since-recovered mailbox, a
// link forwarded in a support thread) could re-flip the password of the
// account that authorizes charges and refunds, immediately AFTER the rightful
// owner reset it.
//
// Real Postgres because the guarantee is one SQL statement's WHERE clause,
// and a fake store would only prove the fake. The second arm is the control:
// a DIFFERENT user's live token must survive, or the fix is a denial-of-
// service on every concurrent reset in the tenant.
func TestConsumeResetToken_VoidsSiblingTokens(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test — requires postgres")
	}
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	_ = testutil.CreateTestTenant(t, db, "Reset Siblings")
	store := user.NewPostgresStore(db)

	mk := func(email string) string {
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
	victim := mk("victim@reset.test")
	bystander := mk("bystander@reset.test")

	issue := func(userID, hash string) {
		t.Helper()
		if _, err := store.CreateResetToken(ctx, userID, hash, time.Now().Add(time.Hour)); err != nil {
			t.Fatalf("issue token %s: %v", hash, err)
		}
	}
	// Three live tokens on the victim (a plausible "clicked reset twice, then
	// once more" history) and one on an unrelated user.
	issue(victim, "hash_redeemed")
	issue(victim, "hash_sibling_a")
	issue(victim, "hash_sibling_b")
	issue(bystander, "hash_bystander")

	if _, err := store.ConsumeResetToken(ctx, "hash_redeemed"); err != nil {
		t.Fatalf("consume: %v", err)
	}

	// Treatment: both siblings must now be dead — proven by trying to REDEEM
	// them, not by reading a column, because redemption is the capability an
	// attacker actually needs.
	for _, h := range []string{"hash_sibling_a", "hash_sibling_b"} {
		if _, err := store.ConsumeResetToken(ctx, h); err == nil {
			t.Errorf("sibling token %s is still redeemable after another token reset the password — a second holder can re-flip the operator password", h)
		}
	}
	// Control: an unrelated user's token must be untouched, or this fix
	// would cancel every concurrent reset in the tenant.
	if _, err := store.ConsumeResetToken(ctx, "hash_bystander"); err != nil {
		t.Errorf("another user's live token was voided (%v) — the void must be scoped to the redeeming user", err)
	}
}
