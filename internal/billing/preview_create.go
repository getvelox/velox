package billing

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/subscription"
)

// CustomerLookup is the narrow surface PreviewService needs from
// customer.Store. Returns errs.ErrNotFound for cross-tenant IDs (RLS hides
// the row); the handler propagates that as 404 customer_not_found.
type CustomerLookup interface {
	Get(ctx context.Context, tenantID, id string) (domain.Customer, error)
}

// SubscriptionLister returns the customer's subscriptions hydrated with
// their items so we can resolve the implicit subscription pick. Mirrors
// the customer-usage SubscriptionLister surface so a single subscription
// store implementation satisfies both.
type SubscriptionLister interface {
	List(ctx context.Context, filter subscription.ListFilter) ([]domain.Subscription, int, error)
	Get(ctx context.Context, tenantID, id string) (domain.Subscription, error)
}

// PreviewService composes the per-domain reads behind
// POST /v1/invoices/create_preview. See docs/design-create-preview.md.
//
// The hard work — dimension-match-aware aggregation in
// usage.AggregateByPricingRules and per-rule pricing via
// domain.ComputeAmountCents — already exists on Engine.previewMeter. This
// service is a thin composition: customer existence → subscription
// resolution → period resolution → defer to engine.previewWithWindow for
// the actual line generation.
//
// We deliberately compose against Engine (rather than reimplementing the
// per-(meter, rule) walk here) so the create_preview surface and the
// in-app debug route /v1/billing/preview/{subscription_id} share one code
// path — preview math == invoice math by construction.
type PreviewService struct {
	engine        *Engine
	customers     CustomerLookup
	subscriptions SubscriptionLister
}

// NewPreviewService wires the composition. All three collaborators are
// required.
func NewPreviewService(engine *Engine, customers CustomerLookup, subscriptions SubscriptionLister) *PreviewService {
	return &PreviewService{
		engine:        engine,
		customers:     customers,
		subscriptions: subscriptions,
	}
}

// CreatePreviewPeriod is the input window. Both From and To zero → default
// to the resolved subscription's current billing cycle. Partial bounds
// (one zero, one non-zero) are rejected with a 400 by resolveCreatePreviewPeriod.
type CreatePreviewPeriod struct {
	From time.Time
	To   time.Time
}

// CreatePreviewRequest is the input shape for POST /v1/invoices/create_preview.
//
//   - CustomerID: required. The customer to preview against. Cross-tenant
//     IDs surface as 404 via the RLS-safe customer lookup.
//   - SubscriptionID: optional. When omitted, the service picks the
//     customer's primary active or trialing subscription (most recent
//     cycle start wins). When passed, the service verifies the sub
//     belongs to the customer (defensive against the rare double-typo
//     case).
//   - Period: optional. Defaults to the resolved subscription's current
//     billing cycle. Both bounds must be supplied together if explicit;
//     partial windows are rejected.
type CreatePreviewRequest struct {
	CustomerID     string
	SubscriptionID string
	Period         CreatePreviewPeriod
}

// CreatePreview produces a dry-run invoice preview for the customer's
// next billing event. Composition order:
//  1. Customer existence (RLS makes cross-tenant IDs return ErrNotFound).
//  2. Subscription resolution (explicit ID, or pick primary active sub).
//  3. Period resolution (explicit window, or sub's current cycle).
//  4. Defer to engine.previewWithWindow for the line composition.
//
// Returns a PreviewResult with the same wire shape produced by
// Engine.Preview so the cost dashboard can consume both surfaces with one
// TS type set.
func (s *PreviewService) CreatePreview(ctx context.Context, tenantID string, req CreatePreviewRequest) (PreviewResult, error) {
	customerID := strings.TrimSpace(req.CustomerID)
	if customerID == "" {
		return PreviewResult{}, errs.Required("customer_id")
	}

	if _, err := s.customers.Get(ctx, tenantID, customerID); err != nil {
		return PreviewResult{}, err
	}

	sub, err := s.resolveSubscription(ctx, tenantID, customerID, req.SubscriptionID)
	if err != nil {
		return PreviewResult{}, err
	}

	from, to, err := resolveCreatePreviewPeriod(req.Period, sub)
	if err != nil {
		return PreviewResult{}, err
	}

	return s.engine.previewWithWindow(ctx, sub, from, to)
}

// resolveSubscription picks the subscription the preview will run
// against. Two paths:
//
//   - Explicit subscriptionID: look it up. ErrNotFound propagates as 404.
//     Cross-customer mismatch surfaces as 422 validation_error — defensive
//     against the operator typoing both IDs to plausible-but-wrong values.
//
//   - Implicit (subscriptionID == ""): list the customer's subs, filter
//     to active or trialing, pick the one with the most recent
//     current_period_start (same heuristic as customer-usage's primary
//     active subscription pick). Zero matches → coded
//     customer_has_no_subscription so the dashboard's empty-state branch
//     covers both surfaces.
func (s *PreviewService) resolveSubscription(ctx context.Context, tenantID, customerID, subscriptionID string) (domain.Subscription, error) {
	subscriptionID = strings.TrimSpace(subscriptionID)

	if subscriptionID != "" {
		sub, err := s.subscriptions.Get(ctx, tenantID, subscriptionID)
		if err != nil {
			return domain.Subscription{}, err
		}
		if sub.CustomerID != customerID {
			return domain.Subscription{}, errs.Invalid(
				"subscription_id",
				"subscription does not belong to the requested customer",
			)
		}
		return sub, nil
	}

	subs, _, err := s.subscriptions.List(ctx, subscription.ListFilter{
		TenantID:   tenantID,
		CustomerID: customerID,
	})
	if err != nil {
		return domain.Subscription{}, fmt.Errorf("list subscriptions: %w", err)
	}

	primary, ok := pickPrimarySubscription(subs)
	if !ok {
		return domain.Subscription{}, errs.Invalid(
			"customer_id",
			"customer has no active or trialing subscription; pass subscription_id explicitly to preview a non-active sub",
		).WithCode("customer_has_no_subscription")
	}
	return primary, nil
}

// pickPrimarySubscription mirrors customer-usage's "primary active
// subscription" heuristic: filter to active or trialing, pick the most
// recent current_period_start. Subs without a current cycle (paused,
// canceled, draft) are excluded — they have no projected bill to preview.
func pickPrimarySubscription(subs []domain.Subscription) (domain.Subscription, bool) {
	var primary *domain.Subscription
	for i := range subs {
		sub := &subs[i]
		if sub.Status != domain.SubscriptionActive && sub.Status != domain.SubscriptionTrialing {
			continue
		}
		if sub.CurrentBillingPeriodStart == nil || sub.CurrentBillingPeriodEnd == nil {
			continue
		}
		if primary == nil || sub.CurrentBillingPeriodStart.After(*primary.CurrentBillingPeriodStart) {
			primary = sub
		}
	}
	if primary == nil {
		return domain.Subscription{}, false
	}
	return *primary, true
}

// resolveCreatePreviewPeriod decides the [from, to) window to preview.
// Default is the resolved subscription's current cycle. Explicit
// from/to must both be set and form a strictly increasing window.
//
// Partial windows (one zero, one non-zero) are rejected — a half-set
// period is almost always a caller bug, not an intent. Same shape as
// customer-usage.resolvePeriod minus the 1-year cap (a preview is always
// within a single cycle, so no cap makes sense).
func resolveCreatePreviewPeriod(period CreatePreviewPeriod, sub domain.Subscription) (time.Time, time.Time, error) {
	hasFrom := !period.From.IsZero()
	hasTo := !period.To.IsZero()

	if hasFrom != hasTo {
		return time.Time{}, time.Time{}, errs.Invalid("period", "both from and to must be supplied together")
	}

	if hasFrom && hasTo {
		if !period.From.Before(period.To) {
			return time.Time{}, time.Time{}, errs.Invalid("period", "from must be strictly before to")
		}
		return period.From, period.To, nil
	}

	if sub.CurrentBillingPeriodStart == nil || sub.CurrentBillingPeriodEnd == nil {
		return time.Time{}, time.Time{}, errs.Invalid(
			"period",
			"subscription has no current billing cycle; pass period.from and period.to explicitly",
		).WithCode("subscription_has_no_cycle")
	}
	return *sub.CurrentBillingPeriodStart, *sub.CurrentBillingPeriodEnd, nil
}
