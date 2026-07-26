package recipe

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"time"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// PricingWriter is the narrow tx-aware surface recipe.Service needs from
// pricing.Service. Defined here so the recipe package owns no cross-domain
// state and can be exercised with fakes in unit tests.
type PricingWriter interface {
	CreateRatingRuleTx(ctx context.Context, tx *sql.Tx, tenantID string, rule domain.RatingRuleVersion) (domain.RatingRuleVersion, error)
	CreateMeterTx(ctx context.Context, tx *sql.Tx, tenantID string, m domain.Meter) (domain.Meter, error)
	CreatePlanTx(ctx context.Context, tx *sql.Tx, tenantID string, p domain.Plan) (domain.Plan, error)
	UpsertMeterPricingRuleTx(ctx context.Context, tx *sql.Tx, tenantID string, rule domain.MeterPricingRule) (domain.MeterPricingRule, error)

	// Adoption reads (ADR-070/ADR-085): the catalog (meter + rating rules)
	// is shared reference data the operator owns, so apply RECONNECTS to an
	// existing graph by natural key instead of duplicating it. Reads run on
	// their own conn (the objects may pre-date this apply's tx). Adoption
	// never clobbers: an operator's edits to a rule/meter survive a re-apply
	// untouched — and the plan is always generated fresh, never adopted.
	GetRuleByKeyAsOf(ctx context.Context, tenantID, ruleKey string, asOf time.Time) (domain.RatingRuleVersion, error)
	GetMeterByKey(ctx context.Context, tenantID, key string) (domain.Meter, error)
	ListPlans(ctx context.Context, tenantID string) ([]domain.Plan, error)
}

// DunningWriter is the narrow tx-aware surface recipe.Service needs from
// dunning.Service.
type DunningWriter interface {
	UpsertPolicyTx(ctx context.Context, tx *sql.Tx, tenantID string, policy domain.DunningPolicy) (domain.DunningPolicy, error)
}

// WebhookWriter is the narrow tx-aware surface recipe.Service needs from
// webhook.Service.
type WebhookWriter interface {
	CreateEndpointTx(ctx context.Context, tx *sql.Tx, tenantID string, ep domain.WebhookEndpoint) (domain.WebhookEndpoint, error)
}

// AuditEmitter is the narrow in-tx audit seam (ADR-090). Instantiate owns the
// coordinator transaction that spans every cross-domain write, so it emits the
// audit row on that same tx: the installed object graph and the record of what
// was installed commit or roll back together.
type AuditEmitter interface {
	LogInTx(ctx context.Context, tx *sql.Tx, e audit.Entry) error
}

// Service is the orchestrator for the recipes feature: it answers
// "list recipes", "preview", and "instantiate" (idempotent apply). The
// canonical entities a recipe creates (meters, rating rules, plans,
// dunning policy, webhook endpoint) live in their own per-domain stores;
// this service threads a single transaction across the cross-domain
// writes so a recipe is either fully applied or not applied at all.
type Service struct {
	db          *postgres.DB
	store       Store
	registry    *Registry
	pricing     PricingWriter
	dunning     DunningWriter
	webhook     WebhookWriter
	auditLogger AuditEmitter
}

// NewService wires the recipe service. registry must already be loaded
// via Load(); the constructor does no I/O.
func NewService(
	db *postgres.DB,
	store Store,
	registry *Registry,
	pricing PricingWriter,
	dunning DunningWriter,
	webhook WebhookWriter,
) *Service {
	return &Service{
		db:       db,
		store:    store,
		registry: registry,
		pricing:  pricing,
		dunning:  dunning,
		webhook:  webhook,
	}
}

// SetAuditLogger wires in-tx audit emission for recipe apply (ADR-090). A nil
// emitter skips emission so unit tests can drive the service with fakes; the
// composition root's audit.MustWired check turns a forgotten wiring line into
// a boot-time panic rather than a silently un-audited install.
func (s *Service) SetAuditLogger(a AuditEmitter) { s.auditLogger = a }

// RecipeListItem is one entry in the GET /v1/recipes response — the
// canonical recipe metadata plus per-tenant installation state and a
// creates summary so the picker UI can render "1 meter · 9 pricing
// rules · monthly billing" without an extra preview round-trip. The
// dashboard uses Instantiated to render an "Installed" badge instead of
// the "Install" CTA — re-apply is an idempotent no-op and there is no
// uninstall (ADR-085).
type RecipeListItem struct {
	domain.Recipe
	Creates      RecipeCreates          `json:"creates"`
	Instantiated *domain.RecipeInstance `json:"instantiated,omitempty"`
}

// RecipeCreates is the per-role count of objects a recipe will produce
// when instantiated. Surfaced on list + detail responses so the
// dashboard can render summary chips without round-tripping preview.
// Counts mirror the shape of domain.CreatedObjects (which is per-ID),
// scaled down to integers for display.
type RecipeCreates struct {
	Meters           int `json:"meters"`
	RatingRules      int `json:"rating_rules"`
	PricingRules     int `json:"pricing_rules"`
	Plans            int `json:"plans"`
	DunningPolicies  int `json:"dunning_policies"`
	WebhookEndpoints int `json:"webhook_endpoints"`
}

// countCreates derives a RecipeCreates summary from a parsed recipe.
// Optional sections (DunningPolicy, Webhook) count as 1 when present, 0
// when absent — they're singletons in the YAML schema, not slices.
func countCreates(r domain.Recipe) RecipeCreates {
	c := RecipeCreates{
		Meters:       len(r.Meters),
		RatingRules:  len(r.RatingRules),
		PricingRules: len(r.PricingRules),
		Plans:        len(r.Plans),
	}
	if r.DunningPolicy != nil {
		c.DunningPolicies = 1
	}
	if r.Webhook != nil {
		c.WebhookEndpoints = 1
	}
	return c
}

// ListRecipes returns every recipe in the registry tagged with this
// tenant's installation state. One indexed read per recipe — fine at the
// v1 catalog size (5 recipes); revisit if the catalog grows past ~50.
func (s *Service) ListRecipes(ctx context.Context, tenantID string) ([]RecipeListItem, error) {
	recipes := s.registry.List()
	out := make([]RecipeListItem, 0, len(recipes))
	for _, r := range recipes {
		item := RecipeListItem{Recipe: r, Creates: countCreates(r)}
		inst, err := s.store.GetByKey(ctx, tenantID, r.Key)
		switch {
		case err == nil:
			instCopy := inst
			item.Instantiated = &instCopy
		case errors.Is(err, errs.ErrNotFound):
			// not installed for this tenant — leave Instantiated nil
		default:
			return nil, err
		}
		out = append(out, item)
	}
	return out, nil
}

// RecipeDetail is the GET /v1/recipes/{key} response. Same fields as a
// list item plus the long-form description and full overridable_schema
// (already on the embedded domain.Recipe). Wrapping rather than
// returning bare domain.Recipe lets us co-emit the creates summary so
// the dashboard's picker drawer renders chips without a preview call.
type RecipeDetail struct {
	domain.Recipe
	Creates RecipeCreates `json:"creates"`
}

// GetRecipe returns the rendered-with-defaults form of a recipe so the
// dashboard can populate the override form. No DB I/O; pure registry.
func (s *Service) GetRecipe(key string) (RecipeDetail, error) {
	r, ok := s.registry.Get(key)
	if !ok {
		return RecipeDetail{}, errs.ErrNotFound
	}
	return RecipeDetail{Recipe: r, Creates: countCreates(r)}, nil
}

// ListInstances returns the recipe_instances rows for the tenant.
func (s *Service) ListInstances(ctx context.Context, tenantID string) ([]domain.RecipeInstance, error) {
	return s.store.List(ctx, tenantID)
}

// PreviewResult is the wire shape of POST /v1/recipes/{key}/preview.
// `objects` groups the would-be-created entities by role so the
// dashboard's preview panel renders one collapsible card per type.
// `warnings` surfaces non-fatal conditions (currency-vs-Stripe-account
// mismatches, placeholder webhook URLs, etc.); empty array in v1 — slot
// is in place so the contract stays stable when richer warnings land.
type PreviewResult struct {
	Key      string         `json:"key"`
	Version  string         `json:"version"`
	Objects  PreviewObjects `json:"objects"`
	Warnings []string       `json:"warnings"`
}

// PreviewObjects mirrors the recipe's object-graph sections. Optional
// pieces (DunningPolicy, Webhook) are emitted as 0-or-1-length slices so
// the wire shape is uniform — picker UI iterates without null guards.
// Empty arrays are emitted (no `omitempty`) for the same reason.
type PreviewObjects struct {
	Meters           []domain.RecipeMeter         `json:"meters"`
	RatingRules      []domain.RecipeRatingRule    `json:"rating_rules"`
	PricingRules     []domain.RecipePricingRule   `json:"pricing_rules"`
	Plans            []domain.RecipePlan          `json:"plans"`
	DunningPolicies  []domain.RecipeDunningPolicy `json:"dunning_policies"`
	WebhookEndpoints []domain.RecipeWebhook       `json:"webhook_endpoints"`
}

// Preview renders a recipe with caller-supplied overrides and returns
// the would-be-created object graph. Pure in-memory; no DB writes, no
// transactions. Cheap enough to call on every override-form keystroke.
func (s *Service) Preview(_ context.Context, recipeKey string, overrides map[string]any) (PreviewResult, error) {
	r, ok := s.registry.Get(recipeKey)
	if !ok {
		return PreviewResult{}, errs.ErrNotFound
	}
	rendered, err := renderRecipe(r, overrides)
	if err != nil {
		return PreviewResult{}, errs.Invalid("overrides", err.Error())
	}
	return previewResultFrom(rendered), nil
}

// previewResultFrom converts a rendered domain.Recipe into the public
// preview wire shape. Slices default to non-nil so the JSON encoder
// emits `[]` rather than `null` — picker UI maps without guards.
func previewResultFrom(r domain.Recipe) PreviewResult {
	out := PreviewResult{
		Key:     r.Key,
		Version: r.Version,
		Objects: PreviewObjects{
			Meters:           r.Meters,
			RatingRules:      r.RatingRules,
			PricingRules:     r.PricingRules,
			Plans:            r.Plans,
			DunningPolicies:  []domain.RecipeDunningPolicy{},
			WebhookEndpoints: []domain.RecipeWebhook{},
		},
		Warnings: []string{},
	}
	if out.Objects.Meters == nil {
		out.Objects.Meters = []domain.RecipeMeter{}
	}
	if out.Objects.RatingRules == nil {
		out.Objects.RatingRules = []domain.RecipeRatingRule{}
	}
	if out.Objects.PricingRules == nil {
		out.Objects.PricingRules = []domain.RecipePricingRule{}
	}
	if out.Objects.Plans == nil {
		out.Objects.Plans = []domain.RecipePlan{}
	}
	if r.DunningPolicy != nil {
		out.Objects.DunningPolicies = []domain.RecipeDunningPolicy{*r.DunningPolicy}
	}
	if r.Webhook != nil {
		out.Objects.WebhookEndpoints = []domain.RecipeWebhook{*r.Webhook}
	}
	return out
}

// InstantiateOptions controls one-off knobs for Instantiate. CreatedBy is
// the actor (operator email or API key ID) recorded on
// recipe_instances.created_by for audit.
type InstantiateOptions struct {
	CreatedBy string
}

// Instantiate applies a recipe for tenantID under one transaction. Order:
// rating rules → meters → pricing rules → plan → (optional) dunning policy →
// (optional) webhook endpoint → instance row. Any failure rolls back the whole
// graph; partial state never reaches the tenant. Apply is an idempotent EVENT
// (ADR-085): a second call on an already-installed recipe is a no-op that
// returns the existing instance — never a 409, never a duplicate plan.
func (s *Service) Instantiate(
	ctx context.Context,
	tenantID, recipeKey string,
	overrides map[string]any,
	opts InstantiateOptions,
) (domain.RecipeInstance, error) {
	r, ok := s.registry.Get(recipeKey)
	if !ok {
		return domain.RecipeInstance{}, errs.ErrNotFound
	}

	rendered, err := renderRecipe(r, overrides)
	if err != nil {
		return domain.RecipeInstance{}, errs.Invalid("overrides", err.Error())
	}

	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.RecipeInstance{}, err
	}
	defer postgres.Rollback(tx)

	// Idempotency — a recipe is an instantiation EVENT, applied once. If the
	// badge already exists, apply is a no-op: return the existing instance
	// unchanged — and, deliberately, emit NO audit row. Nothing was installed
	// on this call; a row here would be evidence of a mutation that never
	// happened (the fabricated-record class ADR-090 exists to kill). The
	// original apply's row is the truthful record. Everything the badge
	// recorded is still present (plans are
	// ON DELETE RESTRICT with no hard-delete; the catalog is shared reference
	// data), so there is nothing to re-create and never a second plan to mint
	// on a double-submit — the badge IS the idempotency gate. The badge is
	// mode-scoped like the objects it records (0157): RLS partitions this read
	// by app.livemode, so a test-mode install never gates a live-mode one.
	// Additive re-apply
	// against a NEWER template version is deferred (ADR-085); v1 ships a single
	// version per recipe, so a re-apply at the same version is a pure no-op.
	if existing, err := s.store.GetByKeyTx(ctx, tx, tenantID, recipeKey); err == nil {
		// Declare the no-op to the audit-coverage detector. This request is a
		// mutating POST that will answer 201 having changed NOTHING, so without
		// the declaration it is indistinguishable — to an observer at the
		// transport — from an install that forgot its audit row, and every
		// re-apply would be reported as an uncovered mutation.
		audit.MarkSkip(ctx)
		return existing, nil
	} else if !errors.Is(err, errs.ErrNotFound) {
		return domain.RecipeInstance{}, err
	}

	objs := domain.CreatedObjects{}

	// Rating rules first — meters and pricing rules reference them by ID. An
	// existing key is ADOPTED at its current version, NEVER republished: usage
	// bills the latest active version of a key as-of period start (engine
	// resolveRatedRule → GetRuleByKeyAsOf), so publishing a new version would
	// reprice every live sub on that key. Adopt-not-republish is the
	// load-bearing no-reprice invariant (ADR-085/ADR-070). The plan below
	// inherits its currency from these rules so it can't be minted in a
	// currency that mismatches the rates it bills against.
	ratingRuleIDByKey := make(map[string]string, len(rendered.RatingRules))
	ruleCurrency := ""
	for _, rr := range rendered.RatingRules {
		if existing, err := s.pricing.GetRuleByKeyAsOf(ctx, tenantID, rr.Key, clock.Now(ctx)); err == nil {
			ratingRuleIDByKey[rr.Key] = existing.ID
			objs.RatingRuleIDs = append(objs.RatingRuleIDs, existing.ID)
			if ruleCurrency == "" {
				ruleCurrency = existing.Currency
			}
			continue
		} else if !errors.Is(err, errs.ErrNotFound) {
			return domain.RecipeInstance{}, fmt.Errorf("rating_rule %q: adoption check: %w", rr.Key, err)
		}
		created, err := s.pricing.CreateRatingRuleTx(ctx, tx, tenantID, ratingRuleFromRecipe(rr))
		if err != nil {
			return domain.RecipeInstance{}, fmt.Errorf("rating_rule %q: %w", rr.Key, err)
		}
		ratingRuleIDByKey[rr.Key] = created.ID
		objs.RatingRuleIDs = append(objs.RatingRuleIDs, created.ID)
		if ruleCurrency == "" {
			ruleCurrency = created.Currency
		}
	}

	// Meters — adopt an existing key only if its AGGREGATION matches (the sole
	// billing-consulted field; sum vs max/count/last silently mis-rolls-up
	// usage), else refuse LOUD. No live-sub clause: adoption only READS the
	// meter to wire a fresh plan and append disjoint pricing bindings — it
	// never mutates the meter — so aggregation-match is the complete safety
	// condition, and it lets a second AI recipe adopt the shared ADR-044
	// `tokens` meter after go-live (ADR-085).
	meterIDByKey := make(map[string]string, len(rendered.Meters))
	for _, m := range rendered.Meters {
		if existing, err := s.pricing.GetMeterByKey(ctx, tenantID, m.Key); err == nil {
			if diffs := meterConformanceDiff(existing, m); len(diffs) > 0 {
				return domain.RecipeInstance{}, errs.AlreadyExists("meter",
					fmt.Sprintf("meter %q already exists with settings that don't match recipe %q (%s) — reconcile the existing meter before instantiating", m.Key, recipeKey, formatDiffs(diffs)))
			}
			meterIDByKey[m.Key] = existing.ID
			objs.MeterIDs = append(objs.MeterIDs, existing.ID)
			continue
		} else if !errors.Is(err, errs.ErrNotFound) {
			return domain.RecipeInstance{}, fmt.Errorf("meter %q: adoption check: %w", m.Key, err)
		}
		created, err := s.pricing.CreateMeterTx(ctx, tx, tenantID, domain.Meter{
			Key:         m.Key,
			Name:        m.Name,
			Unit:        m.Unit,
			Aggregation: m.Aggregation,
		})
		if err != nil {
			return domain.RecipeInstance{}, fmt.Errorf("meter %q: %w", m.Key, err)
		}
		meterIDByKey[m.Key] = created.ID
		objs.MeterIDs = append(objs.MeterIDs, created.ID)
	}

	// Pricing rules — bind each (meter, dimension_match) tuple to a rating
	// rule. The recipe parser already verified meter_key and rating_rule_key
	// resolve, so the lookups here are safe.
	for i, pr := range rendered.PricingRules {
		meterID := meterIDByKey[pr.MeterKey]
		ruleID := ratingRuleIDByKey[pr.RatingRuleKey]
		created, err := s.pricing.UpsertMeterPricingRuleTx(ctx, tx, tenantID, domain.MeterPricingRule{
			MeterID:             meterID,
			RatingRuleVersionID: ruleID,
			DimensionMatch:      pr.DimensionMatch,
			AggregationMode:     pr.AggregationMode,
			Priority:            pr.Priority,
		})
		if err != nil {
			return domain.RecipeInstance{}, fmt.Errorf("pricing_rules[%d]: %w", i, err)
		}
		objs.PricingRuleIDs = append(objs.PricingRuleIDs, created.ID)
	}

	// Plan — GENERATE a fresh plan; NEVER adopt an existing plan by code. A
	// freshly built plan is always wired to the recipe's meter, so silent-$0
	// from under-wiring is impossible, and there is no "does this existing plan
	// match?" question — dissolving the whole collision / conformance /
	// provenance family (ADR-085 supersedes ADR-083/084). The operator owns the
	// plan once created; the recipe never mutates or reuses one. On a code
	// collision (a prior recipe's plan, or a foreign plan) the NEW plan is
	// uniquified (`_2`, `_3`, …) — the incumbent is never renamed (k8s
	// generateName pattern); the immutable id is the identity, the code a
	// display slug with no functional readers (subscriptions bind by plan id).
	// Currency is inherited from the adopted rules so the plan can't be minted
	// in a currency that mismatches the rates it bills.
	existingPlans, err := s.pricing.ListPlans(ctx, tenantID)
	if err != nil {
		return domain.RecipeInstance{}, fmt.Errorf("list plans: %w", err)
	}
	takenCodes := make(map[string]bool, len(existingPlans))
	for _, ep := range existingPlans {
		takenCodes[ep.Code] = true
	}
	for _, p := range rendered.Plans {
		meterIDs := make([]string, 0, len(p.MeterKeys))
		for _, mk := range p.MeterKeys {
			meterIDs = append(meterIDs, meterIDByKey[mk])
		}
		currency := p.Currency
		if ruleCurrency != "" {
			currency = ruleCurrency
		}
		code := freePlanCode(p.Code, takenCodes)
		takenCodes[code] = true
		created, err := s.pricing.CreatePlanTx(ctx, tx, tenantID, domain.Plan{
			Code:            code,
			Name:            p.Name,
			Currency:        currency,
			BillingInterval: p.BillingInterval,
			Status:          domain.PlanActive,
			BaseAmountCents: p.BaseAmountCents,
			BaseBillTiming:  p.BaseBillTiming,
			MeterIDs:        meterIDs,
		})
		if err != nil {
			return domain.RecipeInstance{}, fmt.Errorf("plan %q: %w", code, err)
		}
		objs.PlanIDs = append(objs.PlanIDs, created.ID)
	}

	// Optional dunning policy.
	if rendered.DunningPolicy != nil {
		dp := dunningFromRecipe(*rendered.DunningPolicy)
		created, err := s.dunning.UpsertPolicyTx(ctx, tx, tenantID, dp)
		if err != nil {
			return domain.RecipeInstance{}, fmt.Errorf("dunning policy: %w", err)
		}
		objs.DunningPolicyID = created.ID
	}

	// Optional webhook endpoint. Created inactive with the YAML's
	// url_placeholder; the operator points it at a real URL and activates
	// it via PATCH /v1/webhook-endpoints/endpoints/{id} (or the dashboard's Edit) —
	// a surface that didn't exist until 2026-07-05, which left every
	// recipe endpoint permanently dead.
	if rendered.Webhook != nil {
		secret, err := newWebhookSecret()
		if err != nil {
			return domain.RecipeInstance{}, err
		}
		created, err := s.webhook.CreateEndpointTx(ctx, tx, tenantID, domain.WebhookEndpoint{
			URL:    rendered.Webhook.URLPlaceholder,
			Secret: secret,
			Events: rendered.Webhook.Events,
			Active: false,
		})
		if err != nil {
			return domain.RecipeInstance{}, fmt.Errorf("webhook endpoint: %w", err)
		}
		objs.WebhookEndpointID = created.ID
	}

	inst, err := s.store.CreateTx(ctx, tx, domain.RecipeInstance{
		TenantID:       tenantID,
		RecipeKey:      recipeKey,
		RecipeVersion:  rendered.Version,
		Overrides:      copyOverrides(overrides),
		CreatedObjects: objs,
		CreatedBy:      opts.CreatedBy,
	})
	if err != nil {
		return domain.RecipeInstance{}, err
	}

	// Audit rides the coordinator tx (ADR-090): the whole installed graph and
	// the record of what was installed share fate. Emitted only on the path
	// that actually installed something — the idempotent no-op above returns
	// before here.
	if err := s.emitInstantiated(ctx, tx, rendered, inst); err != nil {
		return domain.RecipeInstance{}, err
	}

	if err := tx.Commit(); err != nil {
		return domain.RecipeInstance{}, err
	}
	return inst, nil
}

// emitInstantiated writes the recipe-apply audit row on the coordinator tx.
//
// Wire strings are FROZEN vocabulary: action "create" (an apply is the
// creation of an install), resource_type "recipe". resource_id is the recipe
// KEY, not the instance id — the key is what the operator surface addresses
// (GET /v1/recipes/{key}, the dashboard's install badge), so it is the id that
// resolves to something a human can open; the instance id rides in metadata.
//
// Metadata is the answer to "what did this recipe install?", which nothing
// else can answer once the operator starts editing the objects: it mirrors the
// instance row's created-object ids. Per ADR-085 those ids include ADOPTED
// meters/rating rules (apply reconnects to an existing catalog rather than
// duplicating it) — i.e. the graph this apply wired, which is exactly the
// question an auditor asks.
func (s *Service) emitInstantiated(ctx context.Context, tx *sql.Tx, rendered domain.Recipe, inst domain.RecipeInstance) error {
	if s.auditLogger == nil {
		return nil
	}
	objs := inst.CreatedObjects
	meta := map[string]any{
		"instance_id":    inst.ID,
		"recipe_key":     inst.RecipeKey,
		"recipe_version": inst.RecipeVersion,
	}
	if len(objs.PlanIDs) > 0 {
		meta["plan_ids"] = objs.PlanIDs
	}
	if len(objs.MeterIDs) > 0 {
		meta["meter_ids"] = objs.MeterIDs
	}
	if len(objs.RatingRuleIDs) > 0 {
		meta["rating_rule_ids"] = objs.RatingRuleIDs
	}
	if len(objs.PricingRuleIDs) > 0 {
		meta["pricing_rule_ids"] = objs.PricingRuleIDs
	}
	if objs.DunningPolicyID != "" {
		meta["dunning_policy_id"] = objs.DunningPolicyID
	}
	if objs.WebhookEndpointID != "" {
		meta["webhook_endpoint_id"] = objs.WebhookEndpointID
	}
	if err := s.auditLogger.LogInTx(ctx, tx, audit.Entry{
		Action:        domain.AuditActionCreate,
		ResourceType:  "recipe",
		ResourceID:    inst.RecipeKey,
		ResourceLabel: rendered.Name,
		Metadata:      meta,
	}); err != nil {
		return fmt.Errorf("audit emission: %w", err)
	}
	return nil
}

// ratingRuleFromRecipe maps the recipe's rating-rule shape to the
// canonical domain.RatingRuleVersion. The version number is allocated by
// the store in SQL (MAX+1 per key) — a recipe only CREATES a rule when the
// key doesn't already exist (an existing key is adopted, never republished;
// ADR-085/ADR-070), so the recipe's rules land as version 1 on a fresh
// tenant. Existing customer overrides keep applying — they follow the
// rule_key.
func ratingRuleFromRecipe(r domain.RecipeRatingRule) domain.RatingRuleVersion {
	name := r.Name
	if name == "" {
		name = r.Key
	}
	return domain.RatingRuleVersion{
		RuleKey:                r.Key,
		Name:                   name,
		LifecycleState:         domain.RatingRuleActive,
		Mode:                   r.Mode,
		Currency:               r.Currency,
		FlatAmountCents:        r.FlatAmountCents,
		GraduatedTiers:         r.GraduatedTiers,
		PackageSize:            r.PackageSize,
		PackageAmountCents:     r.PackageAmountCents,
		OverageUnitAmountCents: r.OverageUnitAmountCents,
	}
}

// dunningFromRecipe converts the recipe's dunning shape (max_retries +
// intervals_hours) to the engine's DunningPolicy (MaxRetryAttempts +
// retry_schedule of duration strings). intervals_hours is an int slice
// in the recipe so YAML stays human-friendly; the policy stores
// "Xh"-style strings to match how operators author policies in the API.
func dunningFromRecipe(p domain.RecipeDunningPolicy) domain.DunningPolicy {
	schedule := make([]string, 0, len(p.IntervalsHours))
	for _, h := range p.IntervalsHours {
		schedule = append(schedule, strconv.Itoa(h)+"h")
	}
	final := domain.DunningFinalAction(p.FinalAction)
	if final == "" {
		final = domain.DunningActionManualReview
	}
	return domain.DunningPolicy{
		Name:             p.Name,
		Enabled:          true,
		RetrySchedule:    schedule,
		MaxRetryAttempts: p.MaxRetries,
		FinalAction:      final,
		GracePeriodDays:  3,
	}
}

// newWebhookSecret mints a fresh whsec_-prefixed signing key. Same
// length and prefix as webhook.Service.CreateEndpoint so a recipe-created
// endpoint is indistinguishable from a hand-created one once the
// operator activates it.
func newWebhookSecret() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate webhook secret: %w", err)
	}
	return "whsec_" + hex.EncodeToString(buf), nil
}

// copyOverrides returns a defensive shallow copy of the caller-provided
// overrides map so the recipe_instances row can't be mutated through the
// caller's reference after persistence.
func copyOverrides(in map[string]any) map[string]any {
	if in == nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(in))
	maps.Copy(out, in)
	return out
}

// freePlanCode returns `desired` if no plan holds it, else the first free
// `<desired>_N` (N≥2). A recipe never adopts an existing plan by code, so a
// collision (a prior recipe's plan, or a foreign plan) is side-stepped by
// uniquifying the NEW plan — never by renaming the incumbent (k8s generateName
// pattern). The plan's immutable id is its identity; the code is a display slug
// with no functional readers (subscriptions bind by plan id). A concurrent
// writer that steals the chosen code between the ListPlans read and the INSERT
// trips the UNIQUE(tenant_id, code) constraint and rolls the whole tx back — a
// loud retry, never a silent duplicate.
func freePlanCode(desired string, taken map[string]bool) string {
	if !taken[desired] {
		return desired
	}
	for n := 2; ; n++ {
		c := fmt.Sprintf("%s_%d", desired, n)
		if !taken[c] {
			return c
		}
	}
}
