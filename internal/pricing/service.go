package pricing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
)

var slugPattern = regexp.MustCompile(`^[a-zA-Z0-9_\-]+$`)

// AuditEmitter is the narrow in-tx audit seam (ADR-090). The service builds
// the emission content at the mutation site; the store threads the closure
// onto its transaction so the pricing write and the audit row share fate.
type AuditEmitter interface {
	LogInTx(ctx context.Context, tx *sql.Tx, e audit.Entry) error
}

// SubscriptionPlanUsageReader counts subscriptions referencing a plan.
// Used by UpdatePlan to enforce immutability of billing-affecting
// fields when the plan has live subs — Stripe-parity (Prices are
// immutable for billing-relevant fields). Narrow shape; concrete
// query lives in *subscription.PostgresStore.
//
// "Live" = any sub that may still bill: status NOT IN ('canceled',
// 'archived'). Draft / active / trialing / paused all qualify
// because they could still produce future invoices and need
// deterministic terms.
type SubscriptionPlanUsageReader interface {
	CountLiveSubsByPlan(ctx context.Context, tenantID, planID string) (int, error)
}

type Service struct {
	store        Store
	subPlanUsage SubscriptionPlanUsageReader
	auditLogger  AuditEmitter
}

func NewService(store Store) *Service {
	return &Service{store: store}
}

// SetAuditLogger wires in-tx audit emission for the pricing mutations that
// own their transaction in the store: meter PATCH and pricing-rule DELETE
// (ADR-090). Nil-safe — an unwired service skips emission, which keeps the
// narrow unit tests fake-friendly. The handler's remaining post-hoc
// AuditWriter (plan/meter create, rule upsert, override) is a separate seam.
func (s *Service) SetAuditLogger(a AuditEmitter) { s.auditLogger = a }

// SetSubscriptionPlanUsageReader wires the live-sub counter used by
// UpdatePlan to gate billing-affecting field mutations. Optional —
// when unwired (narrow unit tests), the immutability guard is
// silent and all fields are mutable. Production wires
// *subscription.PostgresStore via router.go.
func (s *Service) SetSubscriptionPlanUsageReader(r SubscriptionPlanUsageReader) {
	s.subPlanUsage = r
}

// ---------------------------------------------------------------------------
// Rating Rules
// ---------------------------------------------------------------------------

type CreateRatingRuleInput struct {
	RuleKey                string              `json:"rule_key"`
	Name                   string              `json:"name"`
	Mode                   domain.PricingMode  `json:"mode"`
	Currency               string              `json:"currency"`
	FlatAmountCents        decimal.Decimal     `json:"flat_amount_cents"`
	GraduatedTiers         []domain.RatingTier `json:"graduated_tiers"`
	PackageSize            int64               `json:"package_size"`
	PackageAmountCents     int64               `json:"package_amount_cents"`
	OverageUnitAmountCents decimal.Decimal     `json:"overage_unit_amount_cents"`
}

func (s *Service) CreateRatingRule(ctx context.Context, tenantID string, input CreateRatingRuleInput) (domain.RatingRuleVersion, error) {
	if err := validateRatingRuleInput(input); err != nil {
		return domain.RatingRuleVersion{}, err
	}

	currency := strings.ToUpper(input.Currency)

	// Currency guard (ADR-070): overrides follow the rule_key across
	// version publishes and carry bare integer cents with no currency
	// of their own — a publish that changes the key's currency would
	// silently reinterpret every referencing override's amounts in the
	// new currency ($8.00 becomes €8.00). Reject while any active
	// override references the key; without overrides the change is
	// safe (each period resolves its pinned version, and invoice lines
	// carry their own currency).
	existing, err := s.store.ListRatingRules(ctx, RatingRuleFilter{
		TenantID:   tenantID,
		RuleKey:    input.RuleKey,
		LatestOnly: true,
	})
	if err != nil {
		return domain.RatingRuleVersion{}, err
	}
	if len(existing) > 0 && existing[0].Currency != currency {
		n, err := s.store.CountActiveOverridesByRuleKey(ctx, tenantID, input.RuleKey)
		if err != nil {
			return domain.RatingRuleVersion{}, fmt.Errorf("check overrides for currency guard: %w", err)
		}
		if n > 0 {
			return domain.RatingRuleVersion{}, errs.InvalidState(fmt.Sprintf(
				"cannot change currency from %s to %s: %d active customer price override(s) reference rule %q and would be silently repriced in the new currency — remove the overrides first",
				existing[0].Currency, currency, n, input.RuleKey))
		}
		// ADR-100: overrides are not the only downstream — plans bound to
		// this key (via meter pricing rules or a meter's default binding)
		// bill it at cycle close, and their invoices would silently carry
		// amounts computed in the new currency under the old label.
		if err := s.guardRuleCurrencyChange(ctx, tenantID, input.RuleKey, currency); err != nil {
			return domain.RatingRuleVersion{}, err
		}
	}

	rule := domain.RatingRuleVersion{
		RuleKey:                input.RuleKey,
		Name:                   input.Name,
		LifecycleState:         domain.RatingRuleActive,
		Mode:                   input.Mode,
		Currency:               currency,
		FlatAmountCents:        input.FlatAmountCents,
		GraduatedTiers:         input.GraduatedTiers,
		PackageSize:            input.PackageSize,
		PackageAmountCents:     input.PackageAmountCents,
		OverageUnitAmountCents: input.OverageUnitAmountCents,
	}

	// Validate the pricing config by computing a test amount (shape
	// errors beyond qty=1's reach are caught by validateRatingRuleInput).
	if _, err := domain.ComputeAmountCents(rule, decimal.NewFromInt(1)); err != nil {
		return domain.RatingRuleVersion{}, errs.Invalid("pricing", fmt.Sprintf("invalid pricing configuration: %v", err))
	}

	// The store allocates the version in SQL (MAX+1). Two publishes
	// racing the same key can still collide on the unique index — the
	// loser retries with a freshly-allocated number instead of
	// surfacing a spurious 409.
	var created domain.RatingRuleVersion
	var createErr error
	for attempt := 0; attempt < 3; attempt++ {
		created, createErr = s.store.CreateRatingRule(ctx, tenantID, rule)
		if createErr == nil || !errors.Is(createErr, errs.ErrAlreadyExists) {
			return created, createErr
		}
	}
	return domain.RatingRuleVersion{}, createErr
}

func (s *Service) GetRatingRule(ctx context.Context, tenantID, id string) (domain.RatingRuleVersion, error) {
	return s.store.GetRatingRule(ctx, tenantID, id)
}

// GetRuleByKeyAsOf resolves the version in force at asOf — the single
// version-resolution rule every rating path shares (cycle close, cancel
// finalize, threshold fire, preview; ADR-070 pin-at-period-start).
func (s *Service) GetRuleByKeyAsOf(ctx context.Context, tenantID, ruleKey string, asOf time.Time) (domain.RatingRuleVersion, error) {
	return s.store.GetRuleByKeyAsOf(ctx, tenantID, ruleKey, asOf)
}

func (s *Service) ListRatingRules(ctx context.Context, filter RatingRuleFilter) ([]domain.RatingRuleVersion, error) {
	return s.store.ListRatingRules(ctx, filter)
}

func validateRatingRuleInput(input CreateRatingRuleInput) error {
	if strings.TrimSpace(input.RuleKey) == "" {
		return errs.Required("rule_key")
	}
	if err := domain.MaxLen("rule_key", input.RuleKey, 100); err != nil {
		return err
	}
	if !slugPattern.MatchString(input.RuleKey) {
		return errs.Invalid("rule_key", "must contain only alphanumeric characters, hyphens, and underscores")
	}
	if strings.TrimSpace(input.Name) == "" {
		return errs.Required("name")
	}
	if err := domain.MaxLen("name", input.Name, 255); err != nil {
		return err
	}
	if err := domain.ValidateCurrency(input.Currency); err != nil {
		return err
	}

	return validatePricingShape(input.Mode, input.FlatAmountCents, input.GraduatedTiers, input.PackageSize, input.PackageAmountCents)
}

// validatePricingShape enforces the FULL pricing contract at authoring
// time — shared by rating-rule creation and customer overrides. The
// tier rules mirror ComputeAmountCents' rating-time checks exactly
// (domain/pricing.go): the old validation probed ComputeAmountCents at
// qty=1, which breaks out of the tier loop before reaching tier 2, so
// a non-monotonic table ([{up_to:100},{up_to:50}]) or one without a
// catch-all passed authoring and then hard-failed billOnePeriod at the
// first cycle close whose quantity crossed tier 1 — blocking the sub's
// invoice every tick.
func validatePricingShape(mode domain.PricingMode, flat decimal.Decimal, tiers []domain.RatingTier, pkgSize, pkgAmount int64) error {
	switch mode {
	case domain.PricingFlat:
		// Zero is a legal rate: "this dimension is free" (a $0 embedding
		// model, an internal meter tracked but not billed). Almost every
		// AI-infra pricing page has an included-free shape, and rejecting
		// $0 at authoring forced per-customer credit-grant workarounds.
		// ComputeAmountCents has always accepted zero (it rejects only
		// negatives) — this gate was authoring-only. Negative stays
		// rejected: a negative RATE is a discount misspelled (credits are
		// the discount primitive, ADR-039).
		if flat.IsNegative() {
			return errs.Invalid("flat_amount_cents", "unit price must not be negative (use 0 for a free rate)")
		}
	case domain.PricingGraduated:
		if len(tiers) == 0 {
			return errs.Invalid("graduated_tiers", "at least one pricing tier is required")
		}
		lastUpper := int64(0)
		for i, tier := range tiers {
			// Zero-price tiers express included allowances — "first 1M
			// tokens free, then $2/M" (Stripe documents a $0 first tier
			// as the canonical graduated free-tier shape).
			if tier.UnitAmountCents.IsNegative() {
				return errs.Invalid("graduated_tiers", fmt.Sprintf("tier %d: unit price must not be negative (use 0 for a free tier)", i+1))
			}
			if tier.UpTo < 0 {
				return errs.Invalid("graduated_tiers", fmt.Sprintf("tier %d: up_to must be positive, or 0 for the final catch-all tier", i+1))
			}
			if tier.UpTo == 0 {
				if i != len(tiers)-1 {
					return errs.Invalid("graduated_tiers", fmt.Sprintf("tier %d: a catch-all tier (up_to=0) must be the last tier — tiers after it would never price anything", i+1))
				}
				continue
			}
			if tier.UpTo <= lastUpper {
				return errs.Invalid("graduated_tiers", fmt.Sprintf("tier %d: up_to (%d) must be strictly greater than the previous tier's up_to (%d)", i+1, tier.UpTo, lastUpper))
			}
			lastUpper = tier.UpTo
		}
		if tiers[len(tiers)-1].UpTo != 0 {
			return errs.Invalid("graduated_tiers", "the final tier must be a catch-all (up_to=0): a bounded last tier makes any usage beyond it unpriceable and blocks invoice generation at cycle close — use usage caps to limit consumption instead")
		}
	case domain.PricingPackage:
		if pkgSize <= 0 {
			return errs.Invalid("package_size", "package size must be greater than 0")
		}
		// Zero-priced packages are legal for the same included-allowance
		// reason as zero tiers (ComputeAmountCents already accepts them).
		if pkgAmount < 0 {
			return errs.Invalid("package_amount_cents", "package price must not be negative (use 0 for a free package)")
		}
	default:
		return errs.Invalid("mode", "must be one of: flat, graduated, package")
	}

	return nil
}

// ---------------------------------------------------------------------------
// Meters
// ---------------------------------------------------------------------------

type CreateMeterInput struct {
	Key                 string `json:"key"`
	Name                string `json:"name"`
	Unit                string `json:"unit"`
	Aggregation         string `json:"aggregation"`
	RatingRuleVersionID string `json:"rating_rule_version_id,omitempty"`
}

func (s *Service) CreateMeter(ctx context.Context, tenantID string, input CreateMeterInput) (domain.Meter, error) {
	key := strings.TrimSpace(input.Key)
	name := strings.TrimSpace(input.Name)
	if key == "" {
		return domain.Meter{}, errs.Required("key")
	}
	if err := domain.MaxLen("key", key, 100); err != nil {
		return domain.Meter{}, err
	}
	if !slugPattern.MatchString(key) {
		return domain.Meter{}, errs.Invalid("key", "must contain only alphanumeric characters, hyphens, and underscores")
	}
	if name == "" {
		return domain.Meter{}, errs.Required("name")
	}
	if err := domain.MaxLen("name", name, 255); err != nil {
		return domain.Meter{}, err
	}

	unit := input.Unit
	if unit == "" {
		unit = "unit"
	}
	agg := input.Aggregation
	if agg == "" {
		agg = "sum"
	}
	if agg != "sum" && agg != "count" && agg != "max" && agg != "last" {
		return domain.Meter{}, errs.Invalid("aggregation", "must be one of: sum, count, max, last")
	}

	return s.store.CreateMeter(ctx, tenantID, domain.Meter{
		Key:                 key,
		Name:                name,
		Unit:                unit,
		Aggregation:         agg,
		RatingRuleVersionID: input.RatingRuleVersionID,
	})
}

// UpdateMeterInput is the PATCH shape: nil = leave unchanged. A non-nil
// empty RatingRuleVersionID CLEARS the default binding.
type UpdateMeterInput struct {
	Name                *string `json:"name,omitempty"`
	Unit                *string `json:"unit,omitempty"`
	Aggregation         *string `json:"aggregation,omitempty"`
	RatingRuleVersionID *string `json:"rating_rule_version_id,omitempty"`
}

// UpdateMeter patches a meter — most importantly the DEFAULT rating-rule
// binding (meters.rating_rule_version_id), which pre-2026-07-05 could only
// be set at meter CREATE: a meter created without one (every recipe meter)
// had no way to gain a catch-all rate later, so usage matching no pricing
// rule was silently unbilled at cycle close with no operator remedy short
// of recreating the meter. The default binding is the escape hatch: it
// prices every event no dimension rule claims (Orb's required
// default_unit_amount is the peer shape). Aggregation switches are
// supported per FLOW B13 (re-bills next cycle; finalized invoices are
// immutable).
func (s *Service) UpdateMeter(ctx context.Context, tenantID, id string, input UpdateMeterInput) (domain.Meter, error) {
	m, err := s.store.GetMeter(ctx, tenantID, id)
	if err != nil {
		return domain.Meter{}, err
	}
	if input.Name != nil {
		name := strings.TrimSpace(*input.Name)
		if name == "" {
			return domain.Meter{}, errs.Required("name")
		}
		if err := domain.MaxLen("name", name, 255); err != nil {
			return domain.Meter{}, err
		}
		m.Name = name
	}
	if input.Unit != nil {
		unit := strings.TrimSpace(*input.Unit)
		if unit == "" {
			unit = "unit"
		}
		m.Unit = unit
	}
	if input.Aggregation != nil {
		agg := *input.Aggregation
		if agg != "sum" && agg != "count" && agg != "max" && agg != "last" {
			return domain.Meter{}, errs.Invalid("aggregation", "must be one of: sum, count, max, last")
		}
		m.Aggregation = agg
	}
	if input.RatingRuleVersionID != nil {
		rid := strings.TrimSpace(*input.RatingRuleVersionID)
		if rid != "" {
			// The binding must point at a real rule on this tenant — a
			// typo'd default silently prices nothing, the exact silence
			// this PATCH exists to close.
			if _, err := s.store.GetRatingRule(ctx, tenantID, rid); err != nil {
				return domain.Meter{}, errs.Invalid("rating_rule_version_id", "rating rule not found")
			}
			// Currency coherence (ADR-100): the default binding is a live
			// rating edge — the engine resolves it by rule_key at cycle
			// close — so it must share the meter's / wired plans' currency.
			if err := s.guardMeterBinding(ctx, tenantID, m.ID, rid); err != nil {
				return domain.Meter{}, err
			}
		}
		m.RatingRuleVersionID = rid
	}
	return s.store.UpdateMeterAudited(ctx, tenantID, m, s.meterUpdateEmission(ctx, input))
}

// meterUpdateEmission builds the in-tx audit emission for a meter PATCH.
// Wire strings (action "update", resource "meter") are FROZEN — they match
// the meter CREATE row, so a meter's audit timeline stays one resource.
//
// Metadata records the fields the REQUEST actually set, valued from the
// UPDATE's RETURNING row (read inside the tx). It is deliberately NOT a diff
// against the service's pre-tx GetMeter snapshot: that snapshot is read
// outside the write's transaction, so a concurrent PATCH could make the
// "before" side of such a diff a value that was never the row's state at
// write time. Request-intent + committed-value is the pair that cannot lie.
//
// rating_rule_version_id is emitted even when cleared to "" — unbinding a
// meter's default rate is precisely the change that silently unbills usage,
// so "the operator removed the default binding" must be visible, not absent.
func (s *Service) meterUpdateEmission(ctx context.Context, input UpdateMeterInput) func(tx *sql.Tx, out domain.Meter) error {
	if s.auditLogger == nil {
		return nil
	}
	return func(tx *sql.Tx, out domain.Meter) error {
		meta := map[string]any{"key": out.Key}
		if input.Name != nil {
			meta["name"] = out.Name
		}
		if input.Unit != nil {
			meta["unit"] = out.Unit
		}
		if input.Aggregation != nil {
			meta["aggregation"] = out.Aggregation
		}
		if input.RatingRuleVersionID != nil {
			meta["rating_rule_version_id"] = out.RatingRuleVersionID
		}
		return s.auditLogger.LogInTx(ctx, tx, audit.Entry{
			Action:        domain.AuditActionUpdate,
			ResourceType:  "meter",
			ResourceID:    out.ID,
			ResourceLabel: out.Name,
			Metadata:      meta,
		})
	}
}

func (s *Service) GetMeter(ctx context.Context, tenantID, id string) (domain.Meter, error) {
	return s.store.GetMeter(ctx, tenantID, id)
}

func (s *Service) GetMeterByKey(ctx context.Context, tenantID, key string) (domain.Meter, error) {
	return s.store.GetMeterByKey(ctx, tenantID, key)
}

func (s *Service) ListMeters(ctx context.Context, tenantID string) ([]domain.Meter, error) {
	return s.store.ListMeters(ctx, tenantID)
}

// ---------------------------------------------------------------------------
// Plans
// ---------------------------------------------------------------------------

type CreatePlanInput struct {
	Code            string                 `json:"code"`
	Name            string                 `json:"name"`
	Description     string                 `json:"description,omitempty"`
	Currency        string                 `json:"currency"`
	BillingInterval domain.BillingInterval `json:"billing_interval"`
	BaseAmountCents int64                  `json:"base_amount_cents"`
	// BaseBillTiming is optional on create. Empty defaults to in_arrears
	// (existing tenant behaviour). Setting in_advance opts the plan into
	// first-invoice-on-create + cancel-proration semantics per ADR-031.
	BaseBillTiming domain.BillTiming `json:"base_bill_timing,omitempty"`
	MeterIDs       []string          `json:"meter_ids"`
	Status         string            `json:"status,omitempty"`
	TaxCode        string            `json:"tax_code,omitempty"`
}

func (s *Service) CreatePlan(ctx context.Context, tenantID string, input CreatePlanInput) (domain.Plan, error) {
	code := strings.TrimSpace(input.Code)
	name := strings.TrimSpace(input.Name)
	currency := strings.ToUpper(strings.TrimSpace(input.Currency))

	if code == "" {
		return domain.Plan{}, errs.Required("code")
	}
	if err := domain.MaxLen("code", code, 100); err != nil {
		return domain.Plan{}, err
	}
	if !slugPattern.MatchString(code) {
		return domain.Plan{}, errs.Invalid("code", "must contain only alphanumeric characters, hyphens, and underscores")
	}
	if name == "" {
		return domain.Plan{}, errs.Required("name")
	}
	if err := domain.MaxLen("name", name, 255); err != nil {
		return domain.Plan{}, err
	}
	if err := domain.ValidateCurrency(currency); err != nil {
		return domain.Plan{}, err
	}
	if input.BillingInterval != domain.BillingMonthly && input.BillingInterval != domain.BillingYearly {
		return domain.Plan{}, errs.Invalid("billing_interval", "must be monthly or yearly")
	}
	if input.BaseAmountCents < 0 {
		return domain.Plan{}, errs.Invalid("base_amount_cents", "base fee must be 0 or more")
	}

	baseBillTiming := input.BaseBillTiming
	if baseBillTiming == "" {
		baseBillTiming = domain.BillInArrears
	}
	if !baseBillTiming.IsValid() {
		return domain.Plan{}, errs.Invalid("base_bill_timing", "must be in_advance or in_arrears")
	}

	taxCode := strings.TrimSpace(input.TaxCode)
	if err := domain.ValidateStripeTaxCode("tax_code", taxCode); err != nil {
		return domain.Plan{}, err
	}

	if input.MeterIDs == nil {
		input.MeterIDs = []string{}
	}
	if err := s.assertMetersPriced(ctx, tenantID, input.MeterIDs); err != nil {
		return domain.Plan{}, err
	}
	// Currency coherence (ADR-100): the plan's currency labels the invoice;
	// the wired meters' rules compute the amounts. They must agree or the
	// invoice re-denominates rule cents without conversion.
	if err := s.guardPlanMeterCurrencies(ctx, tenantID, currency, input.MeterIDs); err != nil {
		return domain.Plan{}, err
	}

	return s.store.CreatePlan(ctx, tenantID, domain.Plan{
		Code:            code,
		Name:            name,
		Description:     strings.TrimSpace(input.Description),
		Currency:        currency,
		BillingInterval: input.BillingInterval,
		Status:          domain.PlanActive,
		BaseAmountCents: input.BaseAmountCents,
		BaseBillTiming:  baseBillTiming,
		MeterIDs:        input.MeterIDs,
		TaxCode:         taxCode,
	})
}

func (s *Service) GetPlan(ctx context.Context, tenantID, id string) (domain.Plan, error) {
	return s.store.GetPlan(ctx, tenantID, id)
}

func (s *Service) ListPlans(ctx context.Context, tenantID string) ([]domain.Plan, error) {
	return s.store.ListPlans(ctx, tenantID)
}

func (s *Service) UpdatePlan(ctx context.Context, tenantID, id string, input CreatePlanInput) (domain.Plan, error) {
	existing, err := s.store.GetPlan(ctx, tenantID, id)
	if err != nil {
		return domain.Plan{}, err
	}

	// Plan-immutability guard. Billing-affecting fields can't change
	// once any live sub references this plan — Stripe-parity (Prices
	// are immutable for billing-relevant fields). Mutating in place
	// would silently change what existing subs bill at their next
	// cycle close, producing revenue gaps or surprise charges.
	//
	// Display-only fields (name, description, tax_code, status) stay
	// mutable: changing them doesn't move money for existing subs.
	//
	// Operator escape hatch: create a new plan with the desired
	// terms and (optionally) schedule existing subs to migrate at
	// their next cycle close via the pending-plan-change machinery.
	billingFieldChanged := false
	changedFields := []string{}
	if input.BaseAmountCents > 0 && input.BaseAmountCents != existing.BaseAmountCents {
		billingFieldChanged = true
		changedFields = append(changedFields, "base_amount_cents")
	}
	if input.BaseBillTiming != "" && input.BaseBillTiming != existing.BaseBillTiming {
		billingFieldChanged = true
		changedFields = append(changedFields, "base_bill_timing")
	}
	if input.MeterIDs != nil && !sameStringSet(input.MeterIDs, existing.MeterIDs) {
		billingFieldChanged = true
		changedFields = append(changedFields, "meter_ids")
	}
	if billingFieldChanged && s.subPlanUsage != nil {
		count, err := s.subPlanUsage.CountLiveSubsByPlan(ctx, tenantID, existing.ID)
		if err != nil {
			return domain.Plan{}, fmt.Errorf("check sub usage: %w", err)
		}
		if count > 0 {
			return domain.Plan{}, errs.Invalid("plan",
				fmt.Sprintf("cannot change billing-affecting field(s) %v: %d live subscription(s) reference this plan. Create a new plan instead and (optionally) migrate subs at their next cycle close.",
					changedFields, count))
		}
	}

	// Guard newly-attached meters are priced (ADR-096) — reject before the
	// write so an unpriced meter can't silently bill $0 usage at cycle close.
	// Runs after the immutability guard so a live-plan meter change still
	// returns the immutability error first.
	if input.MeterIDs != nil {
		if err := s.assertMetersPriced(ctx, tenantID, input.MeterIDs); err != nil {
			return domain.Plan{}, err
		}
		// Currency coherence (ADR-100): meter attach on UPDATE is the same
		// write edge as plan create — without this, a plan created meterless
		// (vacuously coherent) gains differently-denominated rules here.
		// Plan currency is immutable, so compare against the stored value.
		if err := s.guardPlanMeterCurrencies(ctx, tenantID, existing.Currency, input.MeterIDs); err != nil {
			return domain.Plan{}, err
		}
	}

	if name := strings.TrimSpace(input.Name); name != "" {
		existing.Name = name
	}
	existing.Description = strings.TrimSpace(input.Description)
	if input.BaseAmountCents > 0 {
		existing.BaseAmountCents = input.BaseAmountCents
	}
	if input.BaseBillTiming != "" {
		if !input.BaseBillTiming.IsValid() {
			return domain.Plan{}, errs.Invalid("base_bill_timing", "must be in_advance or in_arrears")
		}
		existing.BaseBillTiming = input.BaseBillTiming
	}
	if input.MeterIDs != nil {
		existing.MeterIDs = input.MeterIDs
	}
	if input.Status != "" {
		existing.Status = domain.PlanStatus(input.Status)
	}
	taxCode := strings.TrimSpace(input.TaxCode)
	if err := domain.ValidateStripeTaxCode("tax_code", taxCode); err != nil {
		return domain.Plan{}, err
	}
	existing.TaxCode = taxCode

	return s.store.UpdatePlan(ctx, tenantID, existing)
}

// assertMetersPriced rejects attaching a meter to a plan unless that meter
// is priced — it carries a default rating-rule binding
// (meters.rating_rule_version_id) OR at least one meter_pricing_rules row.
//
// Why here, at authoring time (ADR-096): the billing engine's usage-rating
// loop has no rule to apply for an unpriced meter, so it drops that meter's
// usage and bills $0 for it — a silent under-bill on the money-moving path.
// The industry norm (Stripe rate, Orb/Lago price/charge) makes pricing an
// explicit part of attaching a metric; this guard does the same and removes
// the "a same-keyed rating rule LOOKS bound but isn't" trap. Recipes bind the
// rule before creating the plan, so they pass; a genuinely metering-only
// meter is deferred (it would carry an explicit flag — ADR-096).
func (s *Service) assertMetersPriced(ctx context.Context, tenantID string, meterIDs []string) error {
	for _, id := range meterIDs {
		m, err := s.store.GetMeter(ctx, tenantID, id)
		if err != nil {
			if errors.Is(err, errs.ErrNotFound) {
				return errs.Invalid("meter_ids", fmt.Sprintf("meter %q not found", id))
			}
			return err
		}
		if m.RatingRuleVersionID != "" {
			continue
		}
		rules, err := s.store.ListMeterPricingRulesByMeter(ctx, tenantID, id)
		if err != nil {
			return fmt.Errorf("list pricing rules for meter %s: %w", id, err)
		}
		if len(rules) == 0 {
			return errs.Invalid("meter_ids", fmt.Sprintf(
				"meter %q has no rating rule — bind one (POST /v1/meters/{id}/pricing-rules) before attaching it to a plan, or its usage will bill $0", m.Key))
		}
	}
	return nil
}

// sameStringSet returns true when two string slices contain the same
// elements regardless of order. Used by the plan-immutability guard
// to detect meter_ids mutations — list order isn't semantically
// meaningful, set membership is.
func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]struct{}, len(a))
	for _, v := range a {
		seen[v] = struct{}{}
	}
	for _, v := range b {
		if _, ok := seen[v]; !ok {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Meter Pricing Rules — N-rules-per-meter dispatch.
// ---------------------------------------------------------------------------

// UpsertMeterPricingRuleInput is the public input shape. The combination
// (meter_id, rating_rule_version_id) identifies the rule — re-issuing
// the same point pair with new dimension_match / mode / priority
// updates the existing rule (idempotent reconfigure).
type UpsertMeterPricingRuleInput struct {
	MeterID             string                 `json:"meter_id"`
	RatingRuleVersionID string                 `json:"rating_rule_version_id"`
	DimensionMatch      map[string]any         `json:"dimension_match"`
	AggregationMode     domain.AggregationMode `json:"aggregation_mode"`
	Priority            int                    `json:"priority"`
}

// maxDimensionKeys caps the size of the JSONB filter to keep aggregation
// queries cheap and to bound pathological tenants. 16 dimensions is
// generous for the AI use case (model × operation × cached × tier ≈ 4),
// matches the open-question in the design doc, and is enforced here at
// the service boundary so the store never has to deal with bloated
// filters.
const maxDimensionKeys = 16

// UpsertMeterPricingRule validates the input and upserts the rule.
// Concretely the validations are:
//   - meter_id and rating_rule_version_id required
//   - rating rule must exist for this tenant (404 surfaces as 400 to
//     avoid leaking other tenants' IDs through the API surface)
//   - meter must exist for this tenant (same reasoning)
//   - aggregation_mode must be one of the five accepted values
//   - dimension_match has ≤ maxDimensionKeys keys
//   - dimension_match values are scalars (string / number / bool); object
//     and array values are rejected — Postgres `@>` would still match
//     them but the semantics aren't well-defined for v1
func (s *Service) UpsertMeterPricingRule(ctx context.Context, tenantID string, input UpsertMeterPricingRuleInput) (domain.MeterPricingRule, error) {
	meterID := strings.TrimSpace(input.MeterID)
	rrvID := strings.TrimSpace(input.RatingRuleVersionID)
	if meterID == "" {
		return domain.MeterPricingRule{}, errs.Required("meter_id")
	}
	if rrvID == "" {
		return domain.MeterPricingRule{}, errs.Required("rating_rule_version_id")
	}

	if _, err := s.store.GetMeter(ctx, tenantID, meterID); err != nil {
		return domain.MeterPricingRule{}, errs.Invalid("meter_id", fmt.Sprintf("meter %q not found", meterID))
	}
	if _, err := s.store.GetRatingRule(ctx, tenantID, rrvID); err != nil {
		return domain.MeterPricingRule{}, errs.Invalid("rating_rule_version_id", fmt.Sprintf("rating rule %q not found", rrvID))
	}
	// Currency coherence (ADR-100): binding is the third write edge that can
	// put a differently-denominated rule under a plan (no republish, no plan
	// create involved) — guard it like the other two.
	if err := s.guardMeterBinding(ctx, tenantID, meterID, rrvID); err != nil {
		return domain.MeterPricingRule{}, err
	}

	mode := input.AggregationMode
	if mode == "" {
		mode = domain.AggSum
	}
	if !mode.IsValid() {
		return domain.MeterPricingRule{}, errs.Invalid("aggregation_mode", fmt.Sprintf("must be one of sum, count, last_during_period, last_ever, max; got %q", mode))
	}

	match := input.DimensionMatch
	if match == nil {
		match = map[string]any{}
	}
	if len(match) > maxDimensionKeys {
		return domain.MeterPricingRule{}, errs.Invalid("dimension_match", fmt.Sprintf("at most %d keys (got %d)", maxDimensionKeys, len(match)))
	}
	for k, v := range match {
		switch v.(type) {
		case string, bool, float64, float32, int, int32, int64, nil:
			// scalar — fine.
		default:
			return domain.MeterPricingRule{}, errs.Invalid("dimension_match", fmt.Sprintf("key %q value must be a scalar (string/number/bool), got %T", k, v))
		}
	}

	return s.store.UpsertMeterPricingRule(ctx, tenantID, domain.MeterPricingRule{
		MeterID:             meterID,
		RatingRuleVersionID: rrvID,
		DimensionMatch:      match,
		AggregationMode:     mode,
		Priority:            input.Priority,
	})
}

// GetMeterPricingRule fetches one rule by id.
func (s *Service) GetMeterPricingRule(ctx context.Context, tenantID, id string) (domain.MeterPricingRule, error) {
	return s.store.GetMeterPricingRule(ctx, tenantID, id)
}

// ListMeterPricingRulesByMeter returns rules in priority-DESC order; the
// store already enforces the ordering so callers can iterate top-down.
// ListMeterPricingRules returns every binding for the tenant.
func (s *Service) ListMeterPricingRules(ctx context.Context, tenantID string) ([]domain.MeterPricingRule, error) {
	return s.store.ListMeterPricingRules(ctx, tenantID)
}

func (s *Service) ListMeterPricingRulesByMeter(ctx context.Context, tenantID, meterID string) ([]domain.MeterPricingRule, error) {
	if strings.TrimSpace(meterID) == "" {
		return nil, errs.Required("meter_id")
	}
	return s.store.ListMeterPricingRulesByMeter(ctx, tenantID, meterID)
}

// DeleteMeterPricingRule removes a rule. Pre-existing usage events are
// not retroactively re-scored; deletion only affects future billing
// finalize cycles.
//
// The audit row rides the DELETE's own tx (ADR-090). Before this, the route
// was covered only by the HTTP catch-all, which recorded it as
// "delete meter {meter_id}" — a permanent, un-editable claim that the
// operator destroyed the whole meter. resource_type stays the EXISTING
// "meter_pricing_rule" string (the upsert emission and the dashboard's
// filter vocabulary already use it), so a rule's create/update and its
// delete land on one resource timeline.
func (s *Service) DeleteMeterPricingRule(ctx context.Context, tenantID, id string) error {
	return s.store.DeleteMeterPricingRuleAudited(ctx, tenantID, id, s.pricingRuleDeleteEmission(ctx))
}

// pricingRuleDeleteEmission builds the in-tx audit emission for a
// pricing-rule DELETE. Content comes from the DELETED row (RETURNING), not
// from the request: meter_id is the rule's own, so the row is true even
// though the route's {meter_id} segment is never checked against the rule.
func (s *Service) pricingRuleDeleteEmission(ctx context.Context) func(tx *sql.Tx, deleted domain.MeterPricingRule) error {
	if s.auditLogger == nil {
		return nil
	}
	return func(tx *sql.Tx, deleted domain.MeterPricingRule) error {
		return s.auditLogger.LogInTx(ctx, tx, audit.Entry{
			Action:       domain.AuditActionDelete,
			ResourceType: "meter_pricing_rule",
			ResourceID:   deleted.ID,
			Metadata: map[string]any{
				"meter_id":               deleted.MeterID,
				"rating_rule_version_id": deleted.RatingRuleVersionID,
			},
		})
	}
}

// ---------------------------------------------------------------------------
// Tx variants — used by recipe.Service to compose pricing inserts inside a
// single cross-domain transaction. Validation is intentionally skipped here
// because the recipe template layer already validated the inputs against
// the recipe schema; re-validating in the recipe path would only duplicate
// what the template parser already enforced.
// ---------------------------------------------------------------------------

// CreateRatingRuleTx forwards to the store's tx-aware insert. Caller owns
// the *sql.Tx and is responsible for Commit/Rollback.
func (s *Service) CreateRatingRuleTx(ctx context.Context, tx *sql.Tx, tenantID string, rule domain.RatingRuleVersion) (domain.RatingRuleVersion, error) {
	// Currency hardening (ADR-100): this Tx path used to skip every currency
	// check on the promise that the recipe template layer validated — but
	// that layer checks enum membership only, and the store persists
	// verbatim (the lowercase class that broke analytics once). Normalize +
	// allowlist here, and run the ADR-070/100 republish clauses when the key
	// already has versions: a recipe CREATES only when no ACTIVE version
	// exists (adoption otherwise), so a create over an all-archived key is a
	// republish in disguise — the exact silent-reprice those guards block.
	rule.Currency = strings.ToUpper(strings.TrimSpace(rule.Currency))
	if err := domain.ValidateCurrency(rule.Currency); err != nil {
		return domain.RatingRuleVersion{}, err
	}
	existing, err := s.store.ListRatingRules(ctx, RatingRuleFilter{
		TenantID: tenantID, RuleKey: rule.RuleKey, LatestOnly: true,
	})
	if err != nil {
		return domain.RatingRuleVersion{}, err
	}
	if len(existing) > 0 && existing[0].Currency != rule.Currency {
		n, err := s.store.CountActiveOverridesByRuleKey(ctx, tenantID, rule.RuleKey)
		if err != nil {
			return domain.RatingRuleVersion{}, fmt.Errorf("check overrides for currency guard: %w", err)
		}
		if n > 0 {
			return domain.RatingRuleVersion{}, errs.InvalidState(fmt.Sprintf(
				"cannot change currency from %s to %s: %d active customer price override(s) reference rule %q and would be silently repriced in the new currency — remove the overrides first",
				existing[0].Currency, rule.Currency, n, rule.RuleKey))
		}
		if err := s.guardRuleCurrencyChange(ctx, tenantID, rule.RuleKey, rule.Currency); err != nil {
			return domain.RatingRuleVersion{}, err
		}
	}
	return s.store.CreateRatingRuleTx(ctx, tx, tenantID, rule)
}

// CreateMeterTx forwards to the store's tx-aware insert.
func (s *Service) CreateMeterTx(ctx context.Context, tx *sql.Tx, tenantID string, m domain.Meter) (domain.Meter, error) {
	return s.store.CreateMeterTx(ctx, tx, tenantID, m)
}

// CreatePlanTx forwards to the store's tx-aware insert.
func (s *Service) CreatePlanTx(ctx context.Context, tx *sql.Tx, tenantID string, p domain.Plan) (domain.Plan, error) {
	return s.store.CreatePlanTx(ctx, tx, tenantID, p)
}

// UpsertMeterPricingRuleTx forwards to the store's tx-aware upsert. No
// currency guard here BY DESIGN: within a recipe apply the bound rule was
// created (or adopted with a conformance check) in the same call, and the
// binding guard's committed-state reads cannot see this tx's own inserts —
// it would falsely refuse every fresh install. Recipe-level coherence is
// enforced in recipe.Instantiate (single {{ .currency }} + adopted-meter
// currency check); the guarded public path is UpsertMeterPricingRule.
func (s *Service) UpsertMeterPricingRuleTx(ctx context.Context, tx *sql.Tx, tenantID string, rule domain.MeterPricingRule) (domain.MeterPricingRule, error) {
	return s.store.UpsertMeterPricingRuleTx(ctx, tx, tenantID, rule)
}
