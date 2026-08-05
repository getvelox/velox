package dunning_test

import (
	"context"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/dunning"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestUpsertPolicy_AutoDefaultFirst is the write-side root fix for Finding 2
// (G1): the FIRST dunning policy created in a (tenant, livemode) scope is born
// is_default=true, so a recipe/first-create tenant resolves a working default
// with no explicit SetDefaultPolicy — while subsequent policies stay
// is_default=false. Real Postgres because the behaviour is a SQL
// NOT EXISTS(...) evaluated under RLS; a mem fake can't exercise it.
func TestUpsertPolicy_AutoDefaultFirst(t *testing.T) {
	db := testutil.SetupTestDB(t)
	store := dunning.NewPostgresStore(db)

	policy := func(name string) domain.DunningPolicy {
		return domain.DunningPolicy{
			Name: name, Enabled: true, MaxRetryAttempts: 3, GracePeriodDays: 3,
			RetrySchedule: []string{"72h", "120h"}, FinalSubscriptionAction: domain.SubActionNone, FinalInvoiceAction: domain.InvActionNone,
		}
	}

	t.Run("first policy in a scope is auto-default; second is not", func(t *testing.T) {
		ctx := postgres.WithLivemode(context.Background(), false)
		tenantID := testutil.CreateTestTenant(t, db, "AutoDefault Test")

		first, err := store.UpsertPolicy(ctx, tenantID, policy("first"))
		if err != nil {
			t.Fatalf("upsert first: %v", err)
		}
		if !first.IsDefault {
			t.Fatal("the FIRST policy must be born is_default=true (auto-default-first)")
		}

		second, err := store.UpsertPolicy(ctx, tenantID, policy("second"))
		if err != nil {
			t.Fatalf("upsert second: %v", err)
		}
		if second.IsDefault {
			t.Fatal("a SECOND policy must be is_default=false (operator re-points via SetDefaultPolicy)")
		}

		// GetDefaultPolicy resolves — the exact call that returned ErrNotFound
		// before the fix and poisoned the money-path catchup.
		def, err := store.GetDefaultPolicy(ctx, tenantID)
		if err != nil {
			t.Fatalf("GetDefaultPolicy should resolve after auto-default-first: %v", err)
		}
		if def.ID != first.ID {
			t.Errorf("default = %q, want the first policy %q", def.ID, first.ID)
		}
	})

	t.Run("livemode scoping: test-mode first insert does not shadow live default", func(t *testing.T) {
		tenantID := testutil.CreateTestTenant(t, db, "AutoDefault Livemode")
		testCtx := postgres.WithLivemode(context.Background(), false)
		liveCtx := postgres.WithLivemode(context.Background(), true)

		testFirst, err := store.UpsertPolicy(testCtx, tenantID, policy("test-first"))
		if err != nil {
			t.Fatalf("upsert test-mode: %v", err)
		}
		liveFirst, err := store.UpsertPolicy(liveCtx, tenantID, policy("live-first"))
		if err != nil {
			t.Fatalf("upsert live-mode: %v", err)
		}
		// Each livemode's FIRST policy is independently the default for its scope
		// (the NOT EXISTS subquery and the partial unique index are both
		// (tenant, livemode)-scoped via RLS).
		if !testFirst.IsDefault || !liveFirst.IsDefault {
			t.Fatalf("each livemode's first policy must be its own default: test=%v live=%v",
				testFirst.IsDefault, liveFirst.IsDefault)
		}
	})
}
