package creditnote_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/sagarsuperuser/velox/internal/creditnote"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestUpdateRefundStatusAudited_SameIdentityGuard pins the operator-writer
// half of the ADR-063 amendment (2026-08-04) against the REAL store — the
// webhook writer had SQL-level monotonicity from day one, while this writer
// was a flat UPDATE any stale answer could ride through. The window: the
// retry flow reads provider truth (GetRefund), a refund.failed webhook lands
// between that read and this persist, and the now-stale non-terminal answer
// overwrites the terminal one — which no webhook redelivery would ever
// correct (event-id dedup).
//
// The guard's three deliberate asymmetries are each pinned here:
// same-identity regressions are refused but still EMIT (the operator action
// is the fact); a NEW provider identity may write anything (a fresh attempt
// legitimately restarts the lifecycle); and a same-value persist still runs
// the UPDATE so updated_at moves (the "stuck pending >72h" attention window
// resets on a fresh provider confirmation).
func TestUpdateRefundStatusAudited_SameIdentityGuard(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "CN Retry Guard")

	store := creditnote.NewPostgresStore(db)

	countingEmit := func(calls *int, lastChanged *bool) func(*sql.Tx, domain.CreditNote, domain.RefundStatus, bool) error {
		return func(_ *sql.Tx, _ domain.CreditNote, _ domain.RefundStatus, changed bool) error {
			*calls++
			*lastChanged = changed
			return nil
		}
	}

	t.Run("same id: pending may not overwrite terminal failed, but the action still emits", func(t *testing.T) {
		cn := seedRefundableCN(t, db, tenantID, "guard1", "re_guard_1", domain.RefundFailed)

		calls, changed := 0, true
		if err := store.UpdateRefundStatusAudited(ctx, tenantID, cn.ID,
			domain.RefundPending, "re_guard_1", countingEmit(&calls, &changed)); err != nil {
			t.Fatalf("UpdateRefundStatusAudited: %v", err)
		}
		got, err := store.Get(ctx, tenantID, cn.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.RefundStatus != domain.RefundFailed {
			t.Fatalf("terminal failed was overwritten by stale pending: %q", got.RefundStatus)
		}
		if calls != 1 || changed {
			t.Fatalf("refusal must still emit with changed=false: calls=%d changed=%v", calls, changed)
		}
	})

	t.Run("same id: succeeded may not regress to pending", func(t *testing.T) {
		cn := seedRefundableCN(t, db, tenantID, "guard2", "re_guard_2", domain.RefundSucceeded)

		calls, changed := 0, true
		if err := store.UpdateRefundStatusAudited(ctx, tenantID, cn.ID,
			domain.RefundPending, "re_guard_2", countingEmit(&calls, &changed)); err != nil {
			t.Fatalf("UpdateRefundStatusAudited: %v", err)
		}
		got, _ := store.Get(ctx, tenantID, cn.ID)
		if got.RefundStatus != domain.RefundSucceeded {
			t.Fatalf("succeeded regressed to %q", got.RefundStatus)
		}
	})

	t.Run("same id: succeeded to failed is a legal forward transition (bank reject)", func(t *testing.T) {
		cn := seedRefundableCN(t, db, tenantID, "guard3", "re_guard_3", domain.RefundSucceeded)

		calls, changed := 0, false
		if err := store.UpdateRefundStatusAudited(ctx, tenantID, cn.ID,
			domain.RefundFailed, "re_guard_3", countingEmit(&calls, &changed)); err != nil {
			t.Fatalf("UpdateRefundStatusAudited: %v", err)
		}
		got, _ := store.Get(ctx, tenantID, cn.ID)
		if got.RefundStatus != domain.RefundFailed {
			t.Fatalf("bank-reject transition refused: %q", got.RefundStatus)
		}
		if !changed {
			t.Fatal("state moved but emit saw changed=false")
		}
	})

	t.Run("NEW id may write pending over failed — a fresh attempt restarts the lifecycle", func(t *testing.T) {
		cn := seedRefundableCN(t, db, tenantID, "guard4", "re_guard_4_dead", domain.RefundFailed)

		calls, changed := 0, false
		if err := store.UpdateRefundStatusAudited(ctx, tenantID, cn.ID,
			domain.RefundPending, "re_guard_4_live", countingEmit(&calls, &changed)); err != nil {
			t.Fatalf("UpdateRefundStatusAudited: %v", err)
		}
		got, _ := store.Get(ctx, tenantID, cn.ID)
		if got.RefundStatus != domain.RefundPending || got.StripeRefundID != "re_guard_4_live" {
			t.Fatalf("fresh attempt refused: (%q,%q)", got.RefundStatus, got.StripeRefundID)
		}
		if !changed {
			t.Fatal("identity swap must report changed=true")
		}
	})

	t.Run("same value still touches updated_at — the attention window resets on re-confirmation", func(t *testing.T) {
		cn := seedRefundableCN(t, db, tenantID, "guard5", "re_guard_5", domain.RefundPending)

		before, err := store.Get(ctx, tenantID, cn.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if err := store.UpdateRefundStatusAudited(ctx, tenantID, cn.ID,
			domain.RefundPending, "re_guard_5", nil); err != nil {
			t.Fatalf("UpdateRefundStatusAudited: %v", err)
		}
		after, _ := store.Get(ctx, tenantID, cn.ID)
		if !after.UpdatedAt.After(before.UpdatedAt) {
			t.Fatalf("same-value re-confirmation must move updated_at (before=%v after=%v)", before.UpdatedAt, after.UpdatedAt)
		}
	})
}
