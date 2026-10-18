package pricing_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// P4 (ADR-070) store-level resolution semantics.

func backdateRow(t *testing.T, db *postgres.DB, ctx context.Context, table, id string, to time.Time) {
	t.Helper()
	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin bypass: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.Exec(`UPDATE `+table+` SET created_at = $1 WHERE id = $2`, to, id); err != nil {
		t.Fatalf("backdate %s: %v", table, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit backdate: %v", err)
	}
}

// TestGetRuleByKeyAsOf_ResolvesVersionInForce: the one shared resolver
// every rating path uses. Highest active version created at or before
// asOf; a key born after asOf resolves to its earliest active version;
// archived versions never resolve.
func TestGetRuleByKeyAsOf_ResolvesVersionInForce(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "AsOf Resolution")
	store := pricing.NewPostgresStore(db)
	svc := pricing.NewService(store)

	now := time.Now().UTC().Truncate(time.Microsecond)

	v1, err := svc.CreateRatingRule(ctx, tenantID, pricing.CreateRatingRuleInput{
		RuleKey: "tokens", Name: "Tokens v1",
		Mode: domain.PricingFlat, Currency: "USD", FlatAmountCents: decimal.NewFromInt(1),
	})
	if err != nil {
		t.Fatalf("create v1: %v", err)
	}
	backdateRow(t, db, ctx, "rating_rule_versions", v1.ID, now.Add(-48*time.Hour))

	v2, err := svc.CreateRatingRule(ctx, tenantID, pricing.CreateRatingRuleInput{
		RuleKey: "tokens", Name: "Tokens v2",
		Mode: domain.PricingFlat, Currency: "USD", FlatAmountCents: decimal.NewFromInt(2),
	})
	if err != nil {
		t.Fatalf("create v2: %v", err)
	}
	backdateRow(t, db, ctx, "rating_rule_versions", v2.ID, now.Add(-24*time.Hour))

	cases := []struct {
		name  string
		asOf  time.Time
		wantV int
	}{
		{"between v1 and v2 → v1", now.Add(-36 * time.Hour), 1},
		{"after v2 → v2", now, 2},
		{"before the key existed → earliest (key born mid-period)", now.Add(-72 * time.Hour), 1},
	}
	for _, tc := range cases {
		got, err := svc.GetRuleByKeyAsOf(ctx, tenantID, "tokens", tc.asOf)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got.Version != tc.wantV {
			t.Errorf("%s: got v%d, want v%d", tc.name, got.Version, tc.wantV)
		}
	}

	// Archived versions never resolve — archive v2, "after v2" now
	// resolves v1.
	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin bypass: %v", err)
	}
	if _, err := tx.Exec(`UPDATE rating_rule_versions SET lifecycle_state = 'archived' WHERE id = $1`, v2.ID); err != nil {
		t.Fatalf("archive v2: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit archive: %v", err)
	}
	got, err := svc.GetRuleByKeyAsOf(ctx, tenantID, "tokens", now)
	if err != nil {
		t.Fatalf("resolve after archive: %v", err)
	}
	if got.Version != 1 {
		t.Errorf("after archiving v2: got v%d, want v1 (archived versions never resolve)", got.Version)
	}

	if _, err := svc.GetRuleByKeyAsOf(ctx, tenantID, "no_such_key", now); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("unknown key: got %v, want ErrNotFound", err)
	}
}

// TestOverrideEffectivityWindows: overrides are append-only effectivity
// rows — [created_at, deactivated_at). A mid-period upsert or DELETE
// prices from the next period open; the period in flight resolves the
// row that was live when it opened.
func TestOverrideEffectivityWindows(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Override Windows")
	store := pricing.NewPostgresStore(db)
	svc := pricing.NewService(store)

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_win", DisplayName: "Windows",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	rule, err := svc.CreateRatingRule(ctx, tenantID, pricing.CreateRatingRuleInput{
		RuleKey: "gpu", Name: "GPU",
		Mode: domain.PricingFlat, Currency: "USD", FlatAmountCents: decimal.NewFromInt(10),
	})
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	periodOpen := now.Add(-24 * time.Hour) // the in-flight period's open

	// Deal A (5c), in place before the period opened.
	a, err := svc.CreateOverride(ctx, tenantID, pricing.CreateOverrideInput{
		CustomerID: cust.ID, RatingRuleVersionID: rule.ID,
		Mode: domain.PricingFlat, FlatAmountCents: decimal.NewFromInt(5),
	})
	if err != nil {
		t.Fatalf("create override A: %v", err)
	}
	if a.RuleKey != "gpu" {
		t.Fatalf("override rule_key: got %q, want gpu (derived from the referenced version)", a.RuleKey)
	}
	backdateRow(t, db, ctx, "customer_price_overrides", a.ID, now.Add(-48*time.Hour))

	// Mid-period re-negotiation (7c): closes A's window now, opens B.
	b, err := svc.CreateOverride(ctx, tenantID, pricing.CreateOverrideInput{
		CustomerID: cust.ID, RatingRuleVersionID: rule.ID,
		Mode: domain.PricingFlat, FlatAmountCents: decimal.NewFromInt(7),
	})
	if err != nil {
		t.Fatalf("create override B: %v", err)
	}

	// The in-flight period (opened yesterday) still resolves A.
	got, err := svc.GetOverrideByKeyAsOf(ctx, tenantID, cust.ID, "gpu", periodOpen)
	if err != nil {
		t.Fatalf("as-of period open: %v", err)
	}
	if !got.FlatAmountCents.Equal(decimal.NewFromInt(5)) {
		t.Errorf("as-of period open: got %s cents, want 5 (the edit prices from the NEXT period)", got.FlatAmountCents)
	}
	// The next period (opening after the edit) resolves B.
	got, err = svc.GetOverrideByKeyAsOf(ctx, tenantID, cust.ID, "gpu", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("as-of next period: %v", err)
	}
	if !got.FlatAmountCents.Equal(decimal.NewFromInt(7)) {
		t.Errorf("as-of next period: got %s cents, want 7", got.FlatAmountCents)
	}

	// DELETE ends the deal from the next period; the in-flight period
	// (still before B's creation) keeps resolving A.
	if err := svc.DeleteOverride(ctx, tenantID, b.ID); err != nil {
		t.Fatalf("delete override B: %v", err)
	}
	if _, err := svc.GetOverrideByKeyAsOf(ctx, tenantID, cust.ID, "gpu", now.Add(2*time.Minute)); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("as-of after delete: got %v, want ErrNotFound (list price from next period)", err)
	}
	got, err = svc.GetOverrideByKeyAsOf(ctx, tenantID, cust.ID, "gpu", periodOpen)
	if err != nil {
		t.Fatalf("as-of period open after delete: %v", err)
	}
	if !got.FlatAmountCents.Equal(decimal.NewFromInt(5)) {
		t.Errorf("period in flight after delete: got %s, want 5 (historical window intact)", got.FlatAmountCents)
	}

	// Second DELETE of the same row: clean 404.
	if err := svc.DeleteOverride(ctx, tenantID, b.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("second delete: got %v, want ErrNotFound", err)
	}
}
