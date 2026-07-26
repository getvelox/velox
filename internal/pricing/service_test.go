package pricing

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/shopspring/decimal"
)

type memStore struct {
	rules      map[string]domain.RatingRuleVersion
	meters     map[string]domain.Meter
	plans      map[string]domain.Plan
	meterRules map[string]domain.MeterPricingRule
	overrides  map[string]domain.CustomerPriceOverride
}

func newMemStore() *memStore {
	return &memStore{
		rules:      make(map[string]domain.RatingRuleVersion),
		meters:     make(map[string]domain.Meter),
		plans:      make(map[string]domain.Plan),
		meterRules: make(map[string]domain.MeterPricingRule),
	}
}

// seedPricedMeter injects a meter carrying a default rating-rule binding, so
// the ADR-096 plan-attach guard treats it as priced. For tests whose subject
// is NOT pricing completeness (e.g. the ADR-034 immutability guard) but which
// still attach meters to plans. Direct map write bypasses CreateMeter's ID
// reassignment so the meter is reachable by the id the test uses.
func seedPricedMeter(m *memStore, tenantID, id string) {
	m.meters[id] = domain.Meter{
		ID: id, TenantID: tenantID, Key: id, Aggregation: "sum",
		RatingRuleVersionID: "vlx_rrv_seed_" + id,
	}
}

func (m *memStore) CreateRatingRule(_ context.Context, tenantID string, r domain.RatingRuleVersion) (domain.RatingRuleVersion, error) {
	// Mirror the store's SQL allocation: version = MAX+1 per (tenant,
	// rule_key); the caller's Version field is ignored.
	next := 1
	for _, existing := range m.rules {
		if existing.TenantID == tenantID && existing.RuleKey == r.RuleKey && existing.Version >= next {
			next = existing.Version + 1
		}
	}
	r.Version = next
	r.ID = fmt.Sprintf("vlx_rrv_%d", len(m.rules)+1)
	r.TenantID = tenantID
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now().UTC()
	}
	m.rules[r.ID] = r
	return r, nil
}

// GetRuleByKeyAsOf mirrors PostgresStore's ADR-070 resolution: highest
// active version created at or before asOf, else the key's earliest
// active version (key born mid-period).
func (m *memStore) GetRuleByKeyAsOf(_ context.Context, tenantID, ruleKey string, asOf time.Time) (domain.RatingRuleVersion, error) {
	var best, earliest domain.RatingRuleVersion
	foundBest, foundAny := false, false
	for _, r := range m.rules {
		if r.TenantID != tenantID || r.RuleKey != ruleKey || r.LifecycleState != domain.RatingRuleActive {
			continue
		}
		if !foundAny || r.Version < earliest.Version {
			earliest = r
			foundAny = true
		}
		if !r.CreatedAt.After(asOf) && (!foundBest || r.Version > best.Version) {
			best = r
			foundBest = true
		}
	}
	if foundBest {
		return best, nil
	}
	if foundAny {
		return earliest, nil
	}
	return domain.RatingRuleVersion{}, errs.ErrNotFound
}

func (m *memStore) GetRatingRule(_ context.Context, tenantID, id string) (domain.RatingRuleVersion, error) {
	r, ok := m.rules[id]
	if !ok || r.TenantID != tenantID {
		return domain.RatingRuleVersion{}, errs.ErrNotFound
	}
	return r, nil
}

func (m *memStore) ListRatingRules(_ context.Context, filter RatingRuleFilter) ([]domain.RatingRuleVersion, error) {
	var result []domain.RatingRuleVersion
	for _, r := range m.rules {
		if r.TenantID != filter.TenantID {
			continue
		}
		if filter.RuleKey != "" && r.RuleKey != filter.RuleKey {
			continue
		}
		if filter.LifecycleState != "" && string(r.LifecycleState) != filter.LifecycleState {
			continue
		}
		result = append(result, r)
	}
	return result, nil
}

func (m *memStore) CreateMeter(_ context.Context, tenantID string, meter domain.Meter) (domain.Meter, error) {
	for _, existing := range m.meters {
		if existing.TenantID == tenantID && existing.Key == meter.Key {
			return domain.Meter{}, fmt.Errorf("%w: meter key %q", errs.ErrAlreadyExists, meter.Key)
		}
	}
	meter.ID = fmt.Sprintf("vlx_mtr_%d", len(m.meters)+1)
	meter.TenantID = tenantID
	m.meters[meter.ID] = meter
	return meter, nil
}

func (m *memStore) GetMeter(_ context.Context, tenantID, id string) (domain.Meter, error) {
	meter, ok := m.meters[id]
	if !ok || meter.TenantID != tenantID {
		return domain.Meter{}, errs.ErrNotFound
	}
	return meter, nil
}

func (m *memStore) GetMeterByKey(_ context.Context, tenantID, key string) (domain.Meter, error) {
	for _, meter := range m.meters {
		if meter.TenantID == tenantID && meter.Key == key {
			return meter, nil
		}
	}
	return domain.Meter{}, errs.ErrNotFound
}

func (m *memStore) ListMeters(_ context.Context, tenantID string) ([]domain.Meter, error) {
	var result []domain.Meter
	for _, meter := range m.meters {
		if meter.TenantID == tenantID {
			result = append(result, meter)
		}
	}
	return result, nil
}

func (m *memStore) UpdateMeter(_ context.Context, tenantID string, meter domain.Meter) (domain.Meter, error) {
	existing, ok := m.meters[meter.ID]
	if !ok || existing.TenantID != tenantID {
		return domain.Meter{}, errs.ErrNotFound
	}
	m.meters[meter.ID] = meter
	return meter, nil
}

func (m *memStore) CreatePlan(_ context.Context, tenantID string, p domain.Plan) (domain.Plan, error) {
	for _, existing := range m.plans {
		if existing.TenantID == tenantID && existing.Code == p.Code {
			return domain.Plan{}, fmt.Errorf("%w: plan code %q", errs.ErrAlreadyExists, p.Code)
		}
	}
	p.ID = fmt.Sprintf("vlx_pln_%d", len(m.plans)+1)
	p.TenantID = tenantID
	m.plans[p.ID] = p
	return p, nil
}

func (m *memStore) GetPlan(_ context.Context, tenantID, id string) (domain.Plan, error) {
	p, ok := m.plans[id]
	if !ok || p.TenantID != tenantID {
		return domain.Plan{}, errs.ErrNotFound
	}
	return p, nil
}

func (m *memStore) ListPlans(_ context.Context, tenantID string) ([]domain.Plan, error) {
	var result []domain.Plan
	for _, p := range m.plans {
		if p.TenantID == tenantID {
			result = append(result, p)
		}
	}
	return result, nil
}

func (m *memStore) UpdatePlan(_ context.Context, tenantID string, p domain.Plan) (domain.Plan, error) {
	existing, ok := m.plans[p.ID]
	if !ok || existing.TenantID != tenantID {
		return domain.Plan{}, errs.ErrNotFound
	}
	m.plans[p.ID] = p
	return p, nil
}

func (m *memStore) CreateOverride(_ context.Context, tenantID string, o domain.CustomerPriceOverride) (domain.CustomerPriceOverride, error) {
	if m.overrides == nil {
		m.overrides = make(map[string]domain.CustomerPriceOverride)
	}
	// Mirror the append-only store: close the prior active row's window,
	// insert a fresh active row.
	for id, existing := range m.overrides {
		if existing.TenantID == tenantID && existing.CustomerID == o.CustomerID && existing.RuleKey == o.RuleKey && existing.Active {
			existing.Active = false
			m.overrides[id] = existing
		}
	}
	o.ID = fmt.Sprintf("vlx_cpo_%d", len(m.overrides)+1)
	o.TenantID = tenantID
	o.Active = true
	m.overrides[o.ID] = o
	return o, nil
}

func (m *memStore) GetOverrideByKeyAsOf(_ context.Context, tenantID, customerID, ruleKey string, _ time.Time) (domain.CustomerPriceOverride, error) {
	for _, o := range m.overrides {
		if o.TenantID == tenantID && o.CustomerID == customerID && o.RuleKey == ruleKey && o.Active {
			return o, nil
		}
	}
	return domain.CustomerPriceOverride{}, errs.ErrNotFound
}

func (m *memStore) DeactivateOverride(_ context.Context, tenantID, id string) error {
	o, ok := m.overrides[id]
	if !ok || o.TenantID != tenantID || !o.Active {
		return errs.ErrNotFound
	}
	o.Active = false
	m.overrides[id] = o
	return nil
}

func (m *memStore) CountActiveOverridesByRuleKey(_ context.Context, tenantID, ruleKey string) (int, error) {
	n := 0
	for _, o := range m.overrides {
		if o.TenantID == tenantID && o.RuleKey == ruleKey && o.Active {
			n++
		}
	}
	return n, nil
}

func (m *memStore) ListOverrides(_ context.Context, _, _ string) ([]domain.CustomerPriceOverride, error) {
	return nil, nil
}

func (m *memStore) UpsertMeterPricingRule(_ context.Context, tenantID string, r domain.MeterPricingRule) (domain.MeterPricingRule, error) {
	if m.meterRules == nil {
		m.meterRules = make(map[string]domain.MeterPricingRule)
	}
	// Dedup on (tenant_id, meter_id, rating_rule_version_id) — same
	// UNIQUE key the Postgres schema enforces.
	for id, existing := range m.meterRules {
		if existing.TenantID == tenantID && existing.MeterID == r.MeterID && existing.RatingRuleVersionID == r.RatingRuleVersionID {
			r.ID = id
			r.TenantID = tenantID
			r.CreatedAt = existing.CreatedAt
			m.meterRules[id] = r
			return r, nil
		}
	}
	if r.ID == "" {
		r.ID = fmt.Sprintf("vlx_mpr_%d", len(m.meterRules)+1)
	}
	r.TenantID = tenantID
	m.meterRules[r.ID] = r
	return r, nil
}

func (m *memStore) GetMeterPricingRule(_ context.Context, tenantID, id string) (domain.MeterPricingRule, error) {
	r, ok := m.meterRules[id]
	if !ok || r.TenantID != tenantID {
		return domain.MeterPricingRule{}, errs.ErrNotFound
	}
	return r, nil
}

func (m *memStore) ListMeterPricingRulesByMeter(_ context.Context, tenantID, meterID string) ([]domain.MeterPricingRule, error) {
	var out []domain.MeterPricingRule
	for _, r := range m.meterRules {
		if r.TenantID == tenantID && r.MeterID == meterID {
			out = append(out, r)
		}
	}
	return out, nil
}

func (m *memStore) DeleteMeterPricingRule(_ context.Context, tenantID, id string) error {
	r, ok := m.meterRules[id]
	if !ok || r.TenantID != tenantID {
		return errs.ErrNotFound
	}
	delete(m.meterRules, id)
	return nil
}

// Audited variants (ADR-090): the fake has no real tx, so the emission runs
// with a nil *sql.Tx — and to keep SHARED-FATE faithful to the Postgres
// store, an emission error rolls the in-memory write back exactly as the
// aborted tx would. The real coupling is pinned against Postgres in
// intx_audit_integration_test.go.
func (m *memStore) UpdateMeterAudited(ctx context.Context, tenantID string, meter domain.Meter, emit func(tx *sql.Tx, out domain.Meter) error) (domain.Meter, error) {
	before, existed := m.meters[meter.ID]
	out, err := m.UpdateMeter(ctx, tenantID, meter)
	if err != nil {
		return domain.Meter{}, err
	}
	if emit != nil {
		if err := emit(nil, out); err != nil {
			if existed {
				m.meters[meter.ID] = before // shared fate: roll the write back
			}
			return domain.Meter{}, fmt.Errorf("audit emission: %w", err)
		}
	}
	return out, nil
}

func (m *memStore) DeleteMeterPricingRuleAudited(ctx context.Context, tenantID, id string, emit func(tx *sql.Tx, deleted domain.MeterPricingRule) error) error {
	deleted, ok := m.meterRules[id]
	if !ok || deleted.TenantID != tenantID {
		return errs.ErrNotFound // zero-row delete: never emits
	}
	if err := m.DeleteMeterPricingRule(ctx, tenantID, id); err != nil {
		return err
	}
	if emit != nil {
		if err := emit(nil, deleted); err != nil {
			m.meterRules[id] = deleted // shared fate: roll the delete back
			return fmt.Errorf("audit emission: %w", err)
		}
	}
	return nil
}

// *Tx variants forward to the non-Tx methods — the in-memory fake doesn't
// model transaction semantics, and recipe-package tests that need real
// rollback go through the Postgres integration tests instead.
func (m *memStore) CreateRatingRuleTx(ctx context.Context, _ *sql.Tx, tenantID string, r domain.RatingRuleVersion) (domain.RatingRuleVersion, error) {
	return m.CreateRatingRule(ctx, tenantID, r)
}

func (m *memStore) CreateMeterTx(ctx context.Context, _ *sql.Tx, tenantID string, meter domain.Meter) (domain.Meter, error) {
	return m.CreateMeter(ctx, tenantID, meter)
}

func (m *memStore) CreatePlanTx(ctx context.Context, _ *sql.Tx, tenantID string, p domain.Plan) (domain.Plan, error) {
	return m.CreatePlan(ctx, tenantID, p)
}

func (m *memStore) UpsertMeterPricingRuleTx(ctx context.Context, _ *sql.Tx, tenantID string, r domain.MeterPricingRule) (domain.MeterPricingRule, error) {
	return m.UpsertMeterPricingRule(ctx, tenantID, r)
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestCreateRatingRule_Flat(t *testing.T) {
	svc := NewService(newMemStore())
	ctx := context.Background()

	rule, err := svc.CreateRatingRule(ctx, "tenant1", CreateRatingRuleInput{
		RuleKey:         "api_calls",
		Name:            "API Calls",
		Mode:            domain.PricingFlat,
		Currency:        "usd",
		FlatAmountCents: decimal.NewFromInt(500),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rule.RuleKey != "api_calls" {
		t.Errorf("got rule_key %q, want %q", rule.RuleKey, "api_calls")
	}
	if rule.Version != 1 {
		t.Errorf("got version %d, want 1", rule.Version)
	}
	if rule.Currency != "USD" {
		t.Errorf("got currency %q, want USD", rule.Currency)
	}
	if rule.LifecycleState != domain.RatingRuleActive {
		t.Errorf("got lifecycle %q, want active", rule.LifecycleState)
	}
}

func TestCreateRatingRule_Graduated(t *testing.T) {
	svc := NewService(newMemStore())
	ctx := context.Background()

	rule, err := svc.CreateRatingRule(ctx, "tenant1", CreateRatingRuleInput{
		RuleKey:  "storage",
		Name:     "Storage GB",
		Mode:     domain.PricingGraduated,
		Currency: "USD",
		GraduatedTiers: []domain.RatingTier{
			{UpTo: 100, UnitAmountCents: decimal.NewFromInt(10)},
			{UpTo: 0, UnitAmountCents: decimal.NewFromInt(5)},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rule.GraduatedTiers) != 2 {
		t.Errorf("got %d tiers, want 2", len(rule.GraduatedTiers))
	}
}

func TestCreateRatingRule_Package(t *testing.T) {
	svc := NewService(newMemStore())
	ctx := context.Background()

	_, err := svc.CreateRatingRule(ctx, "tenant1", CreateRatingRuleInput{
		RuleKey:            "emails",
		Name:               "Emails",
		Mode:               domain.PricingPackage,
		Currency:           "USD",
		PackageSize:        1000,
		PackageAmountCents: 2000,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCreateRatingRule_AutoVersioning(t *testing.T) {
	svc := NewService(newMemStore())
	ctx := context.Background()

	v1, _ := svc.CreateRatingRule(ctx, "tenant1", CreateRatingRuleInput{
		RuleKey: "api_calls", Name: "V1", Mode: domain.PricingFlat, Currency: "USD", FlatAmountCents: decimal.NewFromInt(100),
	})
	v2, _ := svc.CreateRatingRule(ctx, "tenant1", CreateRatingRuleInput{
		RuleKey: "api_calls", Name: "V2", Mode: domain.PricingFlat, Currency: "USD", FlatAmountCents: decimal.NewFromInt(200),
	})

	if v1.Version != 1 {
		t.Errorf("v1 got version %d, want 1", v1.Version)
	}
	if v2.Version != 2 {
		t.Errorf("v2 got version %d, want 2", v2.Version)
	}
}

func TestCreateRatingRule_Validation(t *testing.T) {
	svc := NewService(newMemStore())
	ctx := context.Background()

	tests := []struct {
		name  string
		input CreateRatingRuleInput
	}{
		{"missing rule_key", CreateRatingRuleInput{Name: "test", Mode: domain.PricingFlat, Currency: "USD"}},
		{"missing name", CreateRatingRuleInput{RuleKey: "x", Mode: domain.PricingFlat, Currency: "USD"}},
		{"missing currency", CreateRatingRuleInput{RuleKey: "x", Name: "test", Mode: domain.PricingFlat}},
		{"invalid mode", CreateRatingRuleInput{RuleKey: "x", Name: "test", Mode: "bad", Currency: "USD"}},
		{"graduated no tiers", CreateRatingRuleInput{RuleKey: "x", Name: "test", Mode: domain.PricingGraduated, Currency: "USD"}},
		{"package zero size", CreateRatingRuleInput{RuleKey: "x", Name: "test", Mode: domain.PricingPackage, Currency: "USD", PackageSize: 0}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := svc.CreateRatingRule(ctx, "tenant1", tt.input)
			if err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

// Zero rates are LEGAL (included/free allowances — "first 1M tokens free,
// then $2/M" is the canonical AI-infra free-tier shape; Stripe documents a
// $0 first graduated tier). Pre-2026-07-05 authoring rejected them with
// "unit price must be greater than 0", forcing per-customer credit-grant
// workarounds. Negative rates stay rejected — a negative RATE is a
// discount misspelled (credits are the discount primitive, ADR-039).
func TestCreateRatingRule_ZeroRatesAllowed_NegativeRejected(t *testing.T) {
	svc := NewService(newMemStore())
	ctx := context.Background()

	if _, err := svc.CreateRatingRule(ctx, "t1", CreateRatingRuleInput{
		RuleKey: "free-flat", Name: "free", Mode: domain.PricingFlat, Currency: "USD",
		FlatAmountCents: decimal.Zero,
	}); err != nil {
		t.Errorf("zero flat rate must be allowed (free dimension): %v", err)
	}

	if _, err := svc.CreateRatingRule(ctx, "t1", CreateRatingRuleInput{
		RuleKey: "free-tier", Name: "free tier", Mode: domain.PricingGraduated, Currency: "USD",
		GraduatedTiers: []domain.RatingTier{
			{UpTo: 1_000_000, UnitAmountCents: decimal.Zero},         // included allowance
			{UpTo: 0, UnitAmountCents: decimal.NewFromFloat(0.0002)}, // then $2/M
		},
	}); err != nil {
		t.Errorf("zero-price first tier must be allowed (included allowance): %v", err)
	}

	if _, err := svc.CreateRatingRule(ctx, "t1", CreateRatingRuleInput{
		RuleKey: "free-pkg", Name: "free pkg", Mode: domain.PricingPackage, Currency: "USD",
		PackageSize: 1000, PackageAmountCents: 0,
	}); err != nil {
		t.Errorf("zero-priced package must be allowed: %v", err)
	}

	if _, err := svc.CreateRatingRule(ctx, "t1", CreateRatingRuleInput{
		RuleKey: "neg-flat", Name: "neg", Mode: domain.PricingFlat, Currency: "USD",
		FlatAmountCents: decimal.NewFromInt(-1),
	}); err == nil {
		t.Error("negative flat rate must be rejected")
	}
	if _, err := svc.CreateRatingRule(ctx, "t1", CreateRatingRuleInput{
		RuleKey: "neg-tier", Name: "neg tier", Mode: domain.PricingGraduated, Currency: "USD",
		GraduatedTiers: []domain.RatingTier{{UpTo: 0, UnitAmountCents: decimal.NewFromInt(-1)}},
	}); err == nil {
		t.Error("negative tier rate must be rejected")
	}
}

func TestCreateMeter(t *testing.T) {
	svc := NewService(newMemStore())
	ctx := context.Background()

	t.Run("valid", func(t *testing.T) {
		m, err := svc.CreateMeter(ctx, "tenant1", CreateMeterInput{
			Key:  "api_calls",
			Name: "API Calls",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if m.Key != "api_calls" {
			t.Errorf("got key %q, want %q", m.Key, "api_calls")
		}
		if m.Aggregation != "sum" {
			t.Errorf("got aggregation %q, want sum", m.Aggregation)
		}
	})

	t.Run("missing key", func(t *testing.T) {
		_, err := svc.CreateMeter(ctx, "tenant1", CreateMeterInput{Name: "test"})
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("invalid aggregation", func(t *testing.T) {
		_, err := svc.CreateMeter(ctx, "tenant1", CreateMeterInput{Key: "x", Name: "x", Aggregation: "invalid"})
		if err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestCreatePlan(t *testing.T) {
	svc := NewService(newMemStore())
	ctx := context.Background()

	t.Run("valid", func(t *testing.T) {
		p, err := svc.CreatePlan(ctx, "tenant1", CreatePlanInput{
			Code:            "pro",
			Name:            "Pro Plan",
			Currency:        "usd",
			BillingInterval: domain.BillingMonthly,
			BaseAmountCents: 9900,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if p.Code != "pro" {
			t.Errorf("got code %q, want %q", p.Code, "pro")
		}
		if p.Status != domain.PlanActive {
			t.Errorf("got status %q, want active", p.Status)
		}
		if p.Currency != "USD" {
			t.Errorf("got currency %q, want USD", p.Currency)
		}
	})

	t.Run("missing code", func(t *testing.T) {
		_, err := svc.CreatePlan(ctx, "tenant1", CreatePlanInput{Name: "x", Currency: "USD", BillingInterval: domain.BillingMonthly})
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("invalid billing interval", func(t *testing.T) {
		_, err := svc.CreatePlan(ctx, "tenant1", CreatePlanInput{Code: "x", Name: "x", Currency: "USD", BillingInterval: "weekly"})
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("duplicate code", func(t *testing.T) {
		_, _ = svc.CreatePlan(ctx, "tenant1", CreatePlanInput{Code: "dup", Name: "x", Currency: "USD", BillingInterval: domain.BillingMonthly})
		_, err := svc.CreatePlan(ctx, "tenant1", CreatePlanInput{Code: "dup", Name: "y", Currency: "USD", BillingInterval: domain.BillingMonthly})
		if err == nil {
			t.Fatal("expected error for duplicate code")
		}
	})

	t.Run("base_bill_timing defaults to in_arrears", func(t *testing.T) {
		p, err := svc.CreatePlan(ctx, "tenant1", CreatePlanInput{
			Code: "tdef", Name: "Default Timing", Currency: "USD", BillingInterval: domain.BillingMonthly,
		})
		if err != nil {
			t.Fatalf("unexpected: %v", err)
		}
		if p.BaseBillTiming != domain.BillInArrears {
			t.Errorf("got %q, want in_arrears", p.BaseBillTiming)
		}
	})

	t.Run("base_bill_timing in_advance accepted", func(t *testing.T) {
		p, err := svc.CreatePlan(ctx, "tenant1", CreatePlanInput{
			Code: "tadv", Name: "Advance", Currency: "USD",
			BillingInterval: domain.BillingMonthly,
			BaseBillTiming:  domain.BillInAdvance,
		})
		if err != nil {
			t.Fatalf("unexpected: %v", err)
		}
		if p.BaseBillTiming != domain.BillInAdvance {
			t.Errorf("got %q, want in_advance", p.BaseBillTiming)
		}
	})

	t.Run("base_bill_timing invalid rejected", func(t *testing.T) {
		_, err := svc.CreatePlan(ctx, "tenant1", CreatePlanInput{
			Code: "tinv", Name: "Invalid", Currency: "USD",
			BillingInterval: domain.BillingMonthly,
			BaseBillTiming:  "nightly",
		})
		if err == nil {
			t.Fatal("expected error for invalid bill_timing")
		}
	})
}

func TestUpdatePlan(t *testing.T) {
	svc := NewService(newMemStore())
	ctx := context.Background()

	created, _ := svc.CreatePlan(ctx, "tenant1", CreatePlanInput{
		Code: "basic", Name: "Basic", Currency: "USD", BillingInterval: domain.BillingMonthly, BaseAmountCents: 4900,
	})

	updated, err := svc.UpdatePlan(ctx, "tenant1", created.ID, CreatePlanInput{
		Name:            "Basic Plus",
		BaseAmountCents: 5900,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if updated.Name != "Basic Plus" {
		t.Errorf("got name %q, want %q", updated.Name, "Basic Plus")
	}
	if updated.BaseAmountCents != 5900 {
		t.Errorf("got base_amount %d, want 5900", updated.BaseAmountCents)
	}
}

// TestPlanAttachGuard_UnpricedMeter_ADR096 covers the plan-attach guard:
// attaching a meter with no rating rule is rejected at authoring, so its
// usage can never silently bill $0 at cycle close. Both binding mechanisms
// (a default meter.rating_rule_version_id and a meter_pricing_rules row)
// count as priced.
func TestPlanAttachGuard_UnpricedMeter_ADR096(t *testing.T) {
	ctx := context.Background()
	plan := func(code string, meters []string) CreatePlanInput {
		return CreatePlanInput{
			Code: code, Name: code, Currency: "USD",
			BillingInterval: domain.BillingMonthly, BaseAmountCents: 2900,
			MeterIDs: meters,
		}
	}

	t.Run("CreatePlan rejects an unpriced meter", func(t *testing.T) {
		store := newMemStore()
		store.meters["m1"] = domain.Meter{ID: "m1", TenantID: "t", Key: "api_calls", Aggregation: "sum"}
		if _, err := NewService(store).CreatePlan(ctx, "t", plan("p", []string{"m1"})); err == nil {
			t.Fatal("expected rejection: unpriced meter attached to plan")
		}
	})

	t.Run("CreatePlan accepts a meter with a default rule binding", func(t *testing.T) {
		store := newMemStore()
		store.meters["m1"] = domain.Meter{ID: "m1", TenantID: "t", Key: "api_calls", Aggregation: "sum", RatingRuleVersionID: "vlx_rrv_1"}
		if _, err := NewService(store).CreatePlan(ctx, "t", plan("p", []string{"m1"})); err != nil {
			t.Fatalf("default-bound meter should be accepted: %v", err)
		}
	})

	t.Run("CreatePlan accepts a meter priced via a meter_pricing_rules row", func(t *testing.T) {
		store := newMemStore()
		store.meters["m1"] = domain.Meter{ID: "m1", TenantID: "t", Key: "api_calls", Aggregation: "sum"}
		store.meterRules["mpr1"] = domain.MeterPricingRule{ID: "mpr1", TenantID: "t", MeterID: "m1", RatingRuleVersionID: "vlx_rrv_1"}
		if _, err := NewService(store).CreatePlan(ctx, "t", plan("p", []string{"m1"})); err != nil {
			t.Fatalf("meter with a pricing rule should be accepted: %v", err)
		}
	})

	t.Run("UpdatePlan rejects adding an unpriced meter (no live subs)", func(t *testing.T) {
		store := newMemStore()
		seedPricedMeter(store, "t", "m1")
		store.meters["m2"] = domain.Meter{ID: "m2", TenantID: "t", Key: "unpriced", Aggregation: "sum"}
		svc := NewService(store)
		p, err := svc.CreatePlan(ctx, "t", plan("p", []string{"m1"}))
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		if _, err := svc.UpdatePlan(ctx, "t", p.ID, plan("p", []string{"m1", "m2"})); err == nil {
			t.Fatal("expected rejection: unpriced meter added on update")
		}
	})
}

// fakePlanUsage is a stub SubscriptionPlanUsageReader that returns a
// fixed live-sub count for any plan. Used by the immutability guard
// tests (ADR-034).
type fakePlanUsage struct {
	count int
	err   error
}

func (f *fakePlanUsage) CountLiveSubsByPlan(_ context.Context, _, _ string) (int, error) {
	return f.count, f.err
}

func TestUpdatePlan_ImmutabilityGuard_ADR034(t *testing.T) {
	ctx := context.Background()

	setup := func(liveCount int) (*Service, domain.Plan) {
		store := newMemStore()
		// meter_a / meter_b priced (default binding): this test's subject is
		// the ADR-034 immutability guard, not ADR-096 pricing completeness.
		seedPricedMeter(store, "tenant1", "meter_a")
		seedPricedMeter(store, "tenant1", "meter_b")
		svc := NewService(store)
		svc.SetSubscriptionPlanUsageReader(&fakePlanUsage{count: liveCount})
		p, err := svc.CreatePlan(ctx, "tenant1", CreatePlanInput{
			Code: "guard", Name: "Guard", Currency: "USD",
			BillingInterval: domain.BillingMonthly, BaseAmountCents: 4900,
			BaseBillTiming: domain.BillInArrears,
			MeterIDs:       []string{"meter_a"},
		})
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		return svc, p
	}

	t.Run("no live subs: billing-affecting fields ARE mutable", func(t *testing.T) {
		svc, p := setup(0)
		updated, err := svc.UpdatePlan(ctx, "tenant1", p.ID, CreatePlanInput{
			BaseAmountCents: 9900,
			BaseBillTiming:  domain.BillInAdvance,
			MeterIDs:        []string{"meter_a", "meter_b"},
		})
		if err != nil {
			t.Fatalf("expected allowed (no subs): %v", err)
		}
		if updated.BaseAmountCents != 9900 {
			t.Errorf("base_amount not applied: %d", updated.BaseAmountCents)
		}
		if updated.BaseBillTiming != domain.BillInAdvance {
			t.Errorf("base_bill_timing not applied: %q", updated.BaseBillTiming)
		}
	})

	t.Run("live subs: base_amount_cents change rejected", func(t *testing.T) {
		svc, p := setup(1)
		_, err := svc.UpdatePlan(ctx, "tenant1", p.ID, CreatePlanInput{
			BaseAmountCents: 9900,
		})
		if err == nil {
			t.Fatal("expected error blocking base_amount_cents mutation")
		}
	})

	t.Run("live subs: base_bill_timing flip rejected", func(t *testing.T) {
		svc, p := setup(2)
		_, err := svc.UpdatePlan(ctx, "tenant1", p.ID, CreatePlanInput{
			BaseBillTiming: domain.BillInAdvance,
		})
		if err == nil {
			t.Fatal("expected error blocking base_bill_timing flip")
		}
	})

	t.Run("live subs: meter_ids change rejected", func(t *testing.T) {
		svc, p := setup(1)
		_, err := svc.UpdatePlan(ctx, "tenant1", p.ID, CreatePlanInput{
			MeterIDs: []string{"meter_a", "meter_b"},
		})
		if err == nil {
			t.Fatal("expected error blocking meter_ids mutation")
		}
	})

	t.Run("live subs: meter_ids same set (different order) NOT a change", func(t *testing.T) {
		svc, p := setup(5)
		// Plan was created with [meter_a]; pass [meter_a] again.
		_, err := svc.UpdatePlan(ctx, "tenant1", p.ID, CreatePlanInput{
			MeterIDs: []string{"meter_a"},
		})
		if err != nil {
			t.Errorf("identical meter set should not trigger guard: %v", err)
		}
	})

	t.Run("live subs: display-only fields STILL mutable", func(t *testing.T) {
		svc, p := setup(3)
		updated, err := svc.UpdatePlan(ctx, "tenant1", p.ID, CreatePlanInput{
			Name:        "Renamed",
			Description: "New description",
			TaxCode:     "txcd_10000000",
		})
		if err != nil {
			t.Fatalf("display-only mutation should be allowed: %v", err)
		}
		if updated.Name != "Renamed" {
			t.Errorf("name not applied: %q", updated.Name)
		}
	})

	t.Run("live subs: no-op same value passes", func(t *testing.T) {
		svc, p := setup(1)
		_, err := svc.UpdatePlan(ctx, "tenant1", p.ID, CreatePlanInput{
			BaseAmountCents: 4900, // same as seed
		})
		if err != nil {
			t.Errorf("no-op base_amount should pass: %v", err)
		}
	})

	t.Run("unwired reader: guard is silent (test-only fallback)", func(t *testing.T) {
		svc := NewService(newMemStore())
		// NOT wiring SetSubscriptionPlanUsageReader.
		p, _ := svc.CreatePlan(ctx, "tenant1", CreatePlanInput{
			Code: "unwired", Name: "Unwired", Currency: "USD",
			BillingInterval: domain.BillingMonthly, BaseAmountCents: 4900,
		})
		_, err := svc.UpdatePlan(ctx, "tenant1", p.ID, CreatePlanInput{
			BaseAmountCents: 9900,
		})
		if err != nil {
			t.Errorf("unwired reader should fail-open in narrow tests: %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// Meter Pricing Rules
// ---------------------------------------------------------------------------

// seedMeterAndRule seeds a meter and a rating rule into the in-memory
// store so the meter_pricing_rules tests can reference them by ID
// without going through the public Create paths (those have their own
// validation tested elsewhere).
func seedMeterAndRule(t *testing.T, svc *Service, tenantID string) (meterID, rrvID string) {
	t.Helper()
	rule, err := svc.CreateRatingRule(context.Background(), tenantID, CreateRatingRuleInput{
		RuleKey: "tokens_in", Name: "Input tokens", Mode: domain.PricingFlat,
		Currency: "USD", FlatAmountCents: decimal.NewFromInt(5),
	})
	if err != nil {
		t.Fatalf("seed rating rule: %v", err)
	}
	meter, err := svc.CreateMeter(context.Background(), tenantID, CreateMeterInput{
		Key: "tokens", Name: "Tokens", Unit: "tokens", Aggregation: "sum",
		RatingRuleVersionID: rule.ID,
	})
	if err != nil {
		t.Fatalf("seed meter: %v", err)
	}
	return meter.ID, rule.ID
}

func TestUpsertMeterPricingRule_Valid(t *testing.T) {
	svc := NewService(newMemStore())
	ctx := context.Background()
	meterID, rrvID := seedMeterAndRule(t, svc, "t1")

	rule, err := svc.UpsertMeterPricingRule(ctx, "t1", UpsertMeterPricingRuleInput{
		MeterID:             meterID,
		RatingRuleVersionID: rrvID,
		DimensionMatch:      map[string]any{"model": "gpt-4", "cached": false},
		AggregationMode:     domain.AggSum,
		Priority:            100,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rule.AggregationMode != domain.AggSum {
		t.Errorf("agg mode: got %q, want sum", rule.AggregationMode)
	}
	if rule.Priority != 100 {
		t.Errorf("priority: got %d, want 100", rule.Priority)
	}
	if rule.DimensionMatch["model"] != "gpt-4" {
		t.Errorf("dimension_match did not round-trip: got %+v", rule.DimensionMatch)
	}
}

func TestUpsertMeterPricingRule_DefaultModeIsSum(t *testing.T) {
	svc := NewService(newMemStore())
	ctx := context.Background()
	meterID, rrvID := seedMeterAndRule(t, svc, "t1")

	rule, err := svc.UpsertMeterPricingRule(ctx, "t1", UpsertMeterPricingRuleInput{
		MeterID: meterID, RatingRuleVersionID: rrvID,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rule.AggregationMode != domain.AggSum {
		t.Errorf("default agg mode: got %q, want sum", rule.AggregationMode)
	}
}

func TestUpsertMeterPricingRule_RejectsBadMode(t *testing.T) {
	svc := NewService(newMemStore())
	ctx := context.Background()
	meterID, rrvID := seedMeterAndRule(t, svc, "t1")

	_, err := svc.UpsertMeterPricingRule(ctx, "t1", UpsertMeterPricingRuleInput{
		MeterID: meterID, RatingRuleVersionID: rrvID, AggregationMode: "average",
	})
	if err == nil {
		t.Fatal("expected error for unknown aggregation mode")
	}
}

func TestUpsertMeterPricingRule_RejectsTooManyDimensions(t *testing.T) {
	svc := NewService(newMemStore())
	ctx := context.Background()
	meterID, rrvID := seedMeterAndRule(t, svc, "t1")

	dims := map[string]any{}
	for i := 0; i < maxDimensionKeys+1; i++ {
		dims[fmt.Sprintf("k%d", i)] = "v"
	}
	_, err := svc.UpsertMeterPricingRule(ctx, "t1", UpsertMeterPricingRuleInput{
		MeterID: meterID, RatingRuleVersionID: rrvID, DimensionMatch: dims,
	})
	if err == nil {
		t.Fatal("expected error for too many dimension keys")
	}
}

func TestUpsertMeterPricingRule_RejectsNonScalarDimensionValue(t *testing.T) {
	svc := NewService(newMemStore())
	ctx := context.Background()
	meterID, rrvID := seedMeterAndRule(t, svc, "t1")

	_, err := svc.UpsertMeterPricingRule(ctx, "t1", UpsertMeterPricingRuleInput{
		MeterID: meterID, RatingRuleVersionID: rrvID,
		DimensionMatch: map[string]any{"models": []string{"gpt-4", "claude"}},
	})
	if err == nil {
		t.Fatal("expected error for slice value in dimension_match")
	}
}

func TestUpsertMeterPricingRule_RequiresMeterAndRule(t *testing.T) {
	svc := NewService(newMemStore())
	ctx := context.Background()

	_, err := svc.UpsertMeterPricingRule(ctx, "t1", UpsertMeterPricingRuleInput{})
	if err == nil {
		t.Fatal("expected required-field error")
	}
}

func TestUpsertMeterPricingRule_UnknownMeterRejected(t *testing.T) {
	svc := NewService(newMemStore())
	ctx := context.Background()
	_, rrvID := seedMeterAndRule(t, svc, "t1")

	_, err := svc.UpsertMeterPricingRule(ctx, "t1", UpsertMeterPricingRuleInput{
		MeterID: "vlx_mtr_does_not_exist", RatingRuleVersionID: rrvID,
	})
	if err == nil {
		t.Fatal("expected error for unknown meter")
	}
}

func TestUpsertMeterPricingRule_TenantIsolation(t *testing.T) {
	svc := NewService(newMemStore())
	ctx := context.Background()
	// Tenant A seeds its own meter and rule; tenant B should not be able
	// to bind a pricing rule to A's IDs.
	meterID, rrvID := seedMeterAndRule(t, svc, "tenantA")

	_, err := svc.UpsertMeterPricingRule(ctx, "tenantB", UpsertMeterPricingRuleInput{
		MeterID: meterID, RatingRuleVersionID: rrvID,
	})
	if err == nil {
		t.Fatal("expected cross-tenant attempt to be rejected")
	}
}

func TestListMeterPricingRulesByMeter(t *testing.T) {
	svc := NewService(newMemStore())
	ctx := context.Background()
	meterID, rrvID := seedMeterAndRule(t, svc, "t1")

	// Seed a second rating rule so we can attach two pricing rules.
	rule2, err := svc.CreateRatingRule(ctx, "t1", CreateRatingRuleInput{
		RuleKey: "tokens_cached", Name: "Cached", Mode: domain.PricingFlat,
		Currency: "USD", FlatAmountCents: decimal.NewFromInt(1),
	})
	if err != nil {
		t.Fatalf("second rating rule: %v", err)
	}

	if _, err := svc.UpsertMeterPricingRule(ctx, "t1", UpsertMeterPricingRuleInput{
		MeterID: meterID, RatingRuleVersionID: rrvID,
		DimensionMatch: map[string]any{}, Priority: 0,
	}); err != nil {
		t.Fatalf("default rule: %v", err)
	}
	if _, err := svc.UpsertMeterPricingRule(ctx, "t1", UpsertMeterPricingRuleInput{
		MeterID: meterID, RatingRuleVersionID: rule2.ID,
		DimensionMatch: map[string]any{"cached": true}, Priority: 100,
	}); err != nil {
		t.Fatalf("specific rule: %v", err)
	}

	rules, err := svc.ListMeterPricingRulesByMeter(ctx, "t1", meterID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rules) != 2 {
		t.Fatalf("got %d rules, want 2", len(rules))
	}
}

func TestUpsertMeterPricingRule_IsIdempotent(t *testing.T) {
	svc := NewService(newMemStore())
	ctx := context.Background()
	meterID, rrvID := seedMeterAndRule(t, svc, "t1")

	first, err := svc.UpsertMeterPricingRule(ctx, "t1", UpsertMeterPricingRuleInput{
		MeterID: meterID, RatingRuleVersionID: rrvID, Priority: 10,
	})
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	// Re-issuing with the same (meter, rule) pair must update, not create.
	second, err := svc.UpsertMeterPricingRule(ctx, "t1", UpsertMeterPricingRuleInput{
		MeterID: meterID, RatingRuleVersionID: rrvID, Priority: 50,
	})
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if first.ID != second.ID {
		t.Errorf("upsert created a new row instead of updating: first=%s second=%s", first.ID, second.ID)
	}
	if second.Priority != 50 {
		t.Errorf("priority not updated: got %d want 50", second.Priority)
	}
}

// ---------------------------------------------------------------------------
// P4 (ADR-070) authoring guards
// ---------------------------------------------------------------------------

func TestCreateRatingRule_TierShapeValidation(t *testing.T) {
	svc := NewService(newMemStore())
	ctx := context.Background()

	mk := func(tiers []domain.RatingTier) CreateRatingRuleInput {
		return CreateRatingRuleInput{
			RuleKey: "tiered", Name: "Tiered", Mode: domain.PricingGraduated,
			Currency: "USD", GraduatedTiers: tiers,
		}
	}
	unit := decimal.NewFromInt(10)

	// Non-monotonic up_to: accepted pre-ADR-070 (the qty=1 probe never
	// reached tier 2), then hard-failed billOnePeriod at cycle close.
	if _, err := svc.CreateRatingRule(ctx, "t1", mk([]domain.RatingTier{
		{UpTo: 100, UnitAmountCents: unit}, {UpTo: 50, UnitAmountCents: unit}, {UpTo: 0, UnitAmountCents: unit},
	})); err == nil {
		t.Error("non-monotonic up_to accepted; want 422")
	}
	// Catch-all not last: dead tiers after it.
	if _, err := svc.CreateRatingRule(ctx, "t1", mk([]domain.RatingTier{
		{UpTo: 0, UnitAmountCents: unit}, {UpTo: 100, UnitAmountCents: unit},
	})); err == nil {
		t.Error("mid-table catch-all accepted; want 422")
	}
	// Bounded final tier: quantity past the bound is unpriceable and
	// blocks invoice generation.
	if _, err := svc.CreateRatingRule(ctx, "t1", mk([]domain.RatingTier{
		{UpTo: 100, UnitAmountCents: unit},
	})); err == nil {
		t.Error("bounded final tier accepted; want 422 (requires a catch-all)")
	}
	// Valid shape.
	if _, err := svc.CreateRatingRule(ctx, "t1", mk([]domain.RatingTier{
		{UpTo: 100, UnitAmountCents: unit}, {UpTo: 0, UnitAmountCents: decimal.NewFromInt(5)},
	})); err != nil {
		t.Errorf("valid tier table rejected: %v", err)
	}
}

func TestCreateRatingRule_CurrencyGuardWithActiveOverrides(t *testing.T) {
	store := newMemStore()
	svc := NewService(store)
	ctx := context.Background()

	v1, err := svc.CreateRatingRule(ctx, "t1", CreateRatingRuleInput{
		RuleKey: "gpu", Name: "GPU", Mode: domain.PricingFlat,
		Currency: "usd", FlatAmountCents: decimal.NewFromInt(100),
	})
	if err != nil {
		t.Fatalf("create v1: %v", err)
	}
	if _, err := svc.CreateOverride(ctx, "t1", CreateOverrideInput{
		CustomerID: "cus_1", RatingRuleVersionID: v1.ID,
		Mode: domain.PricingFlat, FlatAmountCents: decimal.NewFromInt(80),
	}); err != nil {
		t.Fatalf("create override: %v", err)
	}

	// Currency change while an override references the key: the
	// override's bare cents would be silently reinterpreted in EUR.
	if _, err := svc.CreateRatingRule(ctx, "t1", CreateRatingRuleInput{
		RuleKey: "gpu", Name: "GPU EUR", Mode: domain.PricingFlat,
		Currency: "EUR", FlatAmountCents: decimal.NewFromInt(100),
	}); err == nil {
		t.Error("currency change with active override accepted; want 409")
	}
	// Same currency: publishes fine (version 2).
	v2, err := svc.CreateRatingRule(ctx, "t1", CreateRatingRuleInput{
		RuleKey: "gpu", Name: "GPU v2", Mode: domain.PricingFlat,
		Currency: "USD", FlatAmountCents: decimal.NewFromInt(120),
	})
	if err != nil {
		t.Fatalf("same-currency publish: %v", err)
	}
	if v2.Version != 2 {
		t.Errorf("v2 version: got %d, want 2", v2.Version)
	}
	// Without overrides a currency change is allowed: new key, no override.
	if _, err := svc.CreateRatingRule(ctx, "t1", CreateRatingRuleInput{
		RuleKey: "cpu", Name: "CPU", Mode: domain.PricingFlat,
		Currency: "USD", FlatAmountCents: decimal.NewFromInt(10),
	}); err != nil {
		t.Fatalf("create cpu v1: %v", err)
	}
	if _, err := svc.CreateRatingRule(ctx, "t1", CreateRatingRuleInput{
		RuleKey: "cpu", Name: "CPU EUR", Mode: domain.PricingFlat,
		Currency: "EUR", FlatAmountCents: decimal.NewFromInt(10),
	}); err != nil {
		t.Errorf("currency change WITHOUT overrides rejected: %v (should be allowed — periods pin their version)", err)
	}
}

func TestCreateOverride_ResolvesRuleInTenant(t *testing.T) {
	store := newMemStore()
	svc := NewService(store)
	ctx := context.Background()

	// Unknown / cross-tenant version id: clean 422, not a raw FK error.
	if _, err := svc.CreateOverride(ctx, "t1", CreateOverrideInput{
		CustomerID: "cus_1", RatingRuleVersionID: "vlx_rrv_nope",
		Mode: domain.PricingFlat, FlatAmountCents: decimal.NewFromInt(5),
	}); err == nil {
		t.Error("override against unknown rule accepted; want 422")
	}

	v1, err := svc.CreateRatingRule(ctx, "t1", CreateRatingRuleInput{
		RuleKey: "tokens", Name: "Tokens", Mode: domain.PricingFlat,
		Currency: "USD", FlatAmountCents: decimal.NewFromInt(1),
	})
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}
	// Another tenant's id must not resolve (RLS scoping modelled by the
	// fake's tenant check).
	if _, err := svc.CreateOverride(ctx, "t2", CreateOverrideInput{
		CustomerID: "cus_1", RatingRuleVersionID: v1.ID,
		Mode: domain.PricingFlat, FlatAmountCents: decimal.NewFromInt(5),
	}); err == nil {
		t.Error("cross-tenant rule id accepted; want 422")
	}

	// Malformed override tiers rejected with the same authoring rules.
	if _, err := svc.CreateOverride(ctx, "t1", CreateOverrideInput{
		CustomerID: "cus_1", RatingRuleVersionID: v1.ID,
		Mode: domain.PricingGraduated,
		GraduatedTiers: []domain.RatingTier{
			{UpTo: 100, UnitAmountCents: decimal.NewFromInt(10)},
			{UpTo: 50, UnitAmountCents: decimal.NewFromInt(5)},
		},
	}); err == nil {
		t.Error("non-monotonic override tiers accepted; want 422")
	}

	// Valid: derives rule_key from the referenced version.
	o, err := svc.CreateOverride(ctx, "t1", CreateOverrideInput{
		CustomerID: "cus_1", RatingRuleVersionID: v1.ID,
		Mode: domain.PricingFlat, FlatAmountCents: decimal.NewFromInt(5),
	})
	if err != nil {
		t.Fatalf("create override: %v", err)
	}
	if o.RuleKey != "tokens" {
		t.Errorf("override rule_key: got %q, want tokens", o.RuleKey)
	}
}

// UpdateMeter is the operator's remedy for silently-unbilled unmatched
// usage: the DEFAULT rating-rule binding becomes settable post-create
// (pre-2026-07-05 it existed only as a create-time field, so recipe meters
// could never gain a catch-all rate). A typo'd rule id must 422, not bind
// a default that prices nothing.
func TestUpdateMeter_DefaultBinding(t *testing.T) {
	svc := NewService(newMemStore())
	ctx := context.Background()

	rule, err := svc.CreateRatingRule(ctx, "t1", CreateRatingRuleInput{
		RuleKey: "catchall", Name: "catch-all", Mode: domain.PricingFlat, Currency: "USD",
		FlatAmountCents: decimal.NewFromFloat(0.0001),
	})
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}
	meter, err := svc.CreateMeter(ctx, "t1", CreateMeterInput{Key: "tokens_um", Name: "Tokens"})
	if err != nil {
		t.Fatalf("create meter: %v", err)
	}
	if meter.RatingRuleVersionID != "" {
		t.Fatalf("meter starts unbound, got %q", meter.RatingRuleVersionID)
	}

	strp := func(v string) *string { return &v }

	// Bind the default post-create — the escape hatch.
	updated, err := svc.UpdateMeter(ctx, "t1", meter.ID, UpdateMeterInput{RatingRuleVersionID: strp(rule.ID)})
	if err != nil {
		t.Fatalf("bind default: %v", err)
	}
	if updated.RatingRuleVersionID != rule.ID {
		t.Fatalf("default binding: got %q, want %q", updated.RatingRuleVersionID, rule.ID)
	}
	if updated.Name != "Tokens" || updated.Aggregation != "sum" {
		t.Fatalf("untouched fields must survive the patch: %+v", updated)
	}

	// A typo'd rule id must 422.
	if _, err := svc.UpdateMeter(ctx, "t1", meter.ID, UpdateMeterInput{RatingRuleVersionID: strp("vlx_rrv_nope")}); err == nil {
		t.Fatal("nonexistent rule must be rejected — a typo'd default silently prices nothing")
	}

	// Explicit empty string clears the binding.
	cleared, err := svc.UpdateMeter(ctx, "t1", meter.ID, UpdateMeterInput{RatingRuleVersionID: strp("")})
	if err != nil {
		t.Fatalf("clear default: %v", err)
	}
	if cleared.RatingRuleVersionID != "" {
		t.Fatalf("binding must clear, got %q", cleared.RatingRuleVersionID)
	}

	// Aggregation patch validates the enum.
	if _, err := svc.UpdateMeter(ctx, "t1", meter.ID, UpdateMeterInput{Aggregation: strp("median")}); err == nil {
		t.Fatal("bad aggregation must be rejected")
	}
	if _, err := svc.UpdateMeter(ctx, "t1", meter.ID, UpdateMeterInput{Aggregation: strp("max")}); err != nil {
		t.Fatalf("valid aggregation switch: %v", err)
	}
}

// ---- ADR-100 currency-coherence fake implementations -----------------------
// Real semantics over the in-memory maps, mirroring the SQL: in-scope plans
// (non-archived; the fake has no subs, so archived == out of scope), key
// currency = highest active version, both binding edges.

func (m *memStore) keyOf(versionID string) string {
	if r, ok := m.rules[versionID]; ok {
		return r.RuleKey
	}
	return ""
}

func (m *memStore) resolvedKey(ruleKey string) keyCurrency {
	kc := keyCurrency{RuleKey: ruleKey}
	bestVersion := -1
	seen := map[string]bool{}
	for _, r := range m.rules {
		if r.RuleKey != ruleKey || r.LifecycleState != domain.RatingRuleActive {
			continue
		}
		seen[r.Currency] = true
		if r.Version > bestVersion {
			bestVersion = r.Version
			kc.Currency = r.Currency
		}
	}
	kc.Mixed = len(seen) > 1
	return kc
}

func (m *memStore) metersBoundToKey(ruleKey string) map[string]bool {
	out := map[string]bool{}
	for _, mt := range m.meters {
		if mt.RatingRuleVersionID != "" && m.keyOf(mt.RatingRuleVersionID) == ruleKey {
			out[mt.ID] = true
		}
	}
	for _, mr := range m.meterRules {
		if m.keyOf(mr.RatingRuleVersionID) == ruleKey {
			out[mr.MeterID] = true
		}
	}
	return out
}

func (m *memStore) PlanCurrenciesBoundToRuleKey(_ context.Context, _ string, ruleKey string) ([]planCurrency, error) {
	bound := m.metersBoundToKey(ruleKey)
	var out []planCurrency
	for _, p := range m.plans {
		if p.Status == domain.PlanArchived {
			continue
		}
		for _, mid := range p.MeterIDs {
			if bound[mid] {
				out = append(out, planCurrency{Code: p.Code, Currency: p.Currency})
				break
			}
		}
	}
	return out, nil
}

func (m *memStore) PlanCurrenciesWiringMeters(_ context.Context, _ string, meterIDs []string) ([]planCurrency, error) {
	want := map[string]bool{}
	for _, id := range meterIDs {
		want[id] = true
	}
	var out []planCurrency
	for _, p := range m.plans {
		if p.Status == domain.PlanArchived {
			continue
		}
		for _, mid := range p.MeterIDs {
			if want[mid] {
				out = append(out, planCurrency{Code: p.Code, Currency: p.Currency})
				break
			}
		}
	}
	return out, nil
}

func (m *memStore) MeterBoundKeyCurrencies(_ context.Context, _ string, meterIDs []string) ([]keyCurrency, error) {
	want := map[string]bool{}
	for _, id := range meterIDs {
		want[id] = true
	}
	keys := map[string]bool{}
	for _, mt := range m.meters {
		if want[mt.ID] && mt.RatingRuleVersionID != "" {
			if k := m.keyOf(mt.RatingRuleVersionID); k != "" {
				keys[k] = true
			}
		}
	}
	for _, mr := range m.meterRules {
		if want[mr.MeterID] {
			if k := m.keyOf(mr.RatingRuleVersionID); k != "" {
				keys[k] = true
			}
		}
	}
	var out []keyCurrency
	for k := range keys {
		out = append(out, m.resolvedKey(k))
	}
	return out, nil
}

func (m *memStore) RuleKeyResolvedCurrency(_ context.Context, _ string, ruleVersionID string) (keyCurrency, error) {
	k := m.keyOf(ruleVersionID)
	if k == "" {
		return keyCurrency{}, errs.ErrNotFound
	}
	return m.resolvedKey(k), nil
}
