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

// TestUpdateRefundStatusAudited_AttemptScopedWrites pins the operator-writer
// half of the ADR-063 amendment (2026-08-04) against the REAL store — the
// webhook writer had SQL-level monotonicity from day one, while this writer
// was a flat UPDATE any stale answer could ride through. The finder review
// of the first cut then caught three more ways it lost, and each rule below
// exists because of a concrete failure:
//
//   - IDENTITY CAS: a slow reconcile about dead re_A must not stamp over a
//     completed re-drive's live re_B; a create-error persist that raced a
//     concurrent success must not mark the live refund failed (which wedged
//     permanently — the absorbing rule then blocked the truth forever).
//   - SAME-VALUE writes skip the UPDATE so updated_at holds: the 72h
//     needs-attention window measures time since the last real change, and
//     resetting it on the operator's own reconcile poke hid exactly the
//     stuck rows the alert exists for.
//   - Regressions are refused for the same identity — unless the value
//     came from a FRESH provider read, which (unlike a webhook delivery)
//     cannot be stale and may correct a locally-misstamped `failed`.
//   - The refusals still EMIT (changed=false): the action is the fact.
func TestUpdateRefundStatusAudited_AttemptScopedWrites(t *testing.T) {
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

	t.Run("identity CAS: a writer that saw a different refund id is stale and skips, but still emits", func(t *testing.T) {
		cn := seedRefundableCN(t, db, tenantID, "cas1", "re_cas_live", domain.RefundPending)

		calls, changed := 0, true
		// Writer's snapshot predates re_cas_live (it expected no id at all).
		if err := store.UpdateRefundStatusAudited(ctx, tenantID, cn.ID, creditnote.RefundStatusWrite{
			Status: domain.RefundFailed, StripeRefundID: "", ExpectedPriorRefundID: "",
		}, countingEmit(&calls, &changed)); err != nil {
			t.Fatalf("UpdateRefundStatusAudited: %v", err)
		}
		got, _ := store.Get(ctx, tenantID, cn.ID)
		if got.RefundStatus != domain.RefundPending || got.StripeRefundID != "re_cas_live" {
			t.Fatalf("stale writer overwrote the live attempt: (%q,%q)", got.RefundStatus, got.StripeRefundID)
		}
		if calls != 1 || changed {
			t.Fatalf("stale refusal must emit changed=false: calls=%d changed=%v", calls, changed)
		}
	})

	t.Run("same id: pending may not overwrite terminal failed (webhook-race window)", func(t *testing.T) {
		cn := seedRefundableCN(t, db, tenantID, "guard1", "re_guard_1", domain.RefundFailed)

		calls, changed := 0, true
		if err := store.UpdateRefundStatusAudited(ctx, tenantID, cn.ID, creditnote.RefundStatusWrite{
			Status: domain.RefundPending, StripeRefundID: "re_guard_1", ExpectedPriorRefundID: "re_guard_1",
		}, countingEmit(&calls, &changed)); err != nil {
			t.Fatalf("UpdateRefundStatusAudited: %v", err)
		}
		got, _ := store.Get(ctx, tenantID, cn.ID)
		if got.RefundStatus != domain.RefundFailed {
			t.Fatalf("terminal failed was overwritten by stale pending: %q", got.RefundStatus)
		}
		if calls != 1 || changed {
			t.Fatalf("refusal must still emit with changed=false: calls=%d changed=%v", calls, changed)
		}
	})

	t.Run("same id: a FRESH provider read may correct failed to succeeded (a read is never stale)", func(t *testing.T) {
		cn := seedRefundableCN(t, db, tenantID, "guard2", "re_guard_2", domain.RefundFailed)

		calls, changed := 0, false
		if err := store.UpdateRefundStatusAudited(ctx, tenantID, cn.ID, creditnote.RefundStatusWrite{
			Status: domain.RefundSucceeded, StripeRefundID: "re_guard_2",
			ExpectedPriorRefundID: "re_guard_2", ProviderRead: true,
		}, countingEmit(&calls, &changed)); err != nil {
			t.Fatalf("UpdateRefundStatusAudited: %v", err)
		}
		got, _ := store.Get(ctx, tenantID, cn.ID)
		if got.RefundStatus != domain.RefundSucceeded {
			t.Fatalf("provider-read correction refused: %q", got.RefundStatus)
		}
		if !changed {
			t.Fatal("state moved but emit saw changed=false")
		}
	})

	t.Run("same id: succeeded to failed is a legal forward transition (bank reject)", func(t *testing.T) {
		cn := seedRefundableCN(t, db, tenantID, "guard3", "re_guard_3", domain.RefundSucceeded)

		calls, changed := 0, false
		if err := store.UpdateRefundStatusAudited(ctx, tenantID, cn.ID, creditnote.RefundStatusWrite{
			Status: domain.RefundFailed, StripeRefundID: "re_guard_3", ExpectedPriorRefundID: "re_guard_3",
		}, countingEmit(&calls, &changed)); err != nil {
			t.Fatalf("UpdateRefundStatusAudited: %v", err)
		}
		got, _ := store.Get(ctx, tenantID, cn.ID)
		if got.RefundStatus != domain.RefundFailed {
			t.Fatalf("bank-reject transition refused: %q", got.RefundStatus)
		}
	})

	t.Run("NEW id over a dead one restarts the lifecycle when the writer passed the CAS", func(t *testing.T) {
		cn := seedRefundableCN(t, db, tenantID, "guard4", "re_guard_4_dead", domain.RefundFailed)

		calls, changed := 0, false
		if err := store.UpdateRefundStatusAudited(ctx, tenantID, cn.ID, creditnote.RefundStatusWrite{
			Status: domain.RefundPending, StripeRefundID: "re_guard_4_live", ExpectedPriorRefundID: "re_guard_4_dead",
		}, countingEmit(&calls, &changed)); err != nil {
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

	t.Run("same value does NOT touch updated_at — re-confirming 'still pending' is continued stuckness", func(t *testing.T) {
		cn := seedRefundableCN(t, db, tenantID, "guard5", "re_guard_5", domain.RefundPending)

		before, err := store.Get(ctx, tenantID, cn.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		calls, changed := 0, true
		if err := store.UpdateRefundStatusAudited(ctx, tenantID, cn.ID, creditnote.RefundStatusWrite{
			Status: domain.RefundPending, StripeRefundID: "re_guard_5",
			ExpectedPriorRefundID: "re_guard_5", ProviderRead: true,
		}, countingEmit(&calls, &changed)); err != nil {
			t.Fatalf("UpdateRefundStatusAudited: %v", err)
		}
		after, _ := store.Get(ctx, tenantID, cn.ID)
		if !after.UpdatedAt.Equal(before.UpdatedAt) {
			t.Fatalf("same-value write moved updated_at (before=%v after=%v) — this resets the 72h needs-attention clock and hides the stuck refund the alert exists for", before.UpdatedAt, after.UpdatedAt)
		}
		if calls != 1 || changed {
			t.Fatalf("no-op must still emit changed=false: calls=%d changed=%v", calls, changed)
		}
	})
}
