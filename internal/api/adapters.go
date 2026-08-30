package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/stripe/stripe-go/v82"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/billing"
	"github.com/sagarsuperuser/velox/internal/credit"
	"github.com/sagarsuperuser/velox/internal/creditnote"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/dunning"
	"github.com/sagarsuperuser/velox/internal/email"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/payment"
	"github.com/sagarsuperuser/velox/internal/paymentmethods"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/crypto"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/subscription"
	"github.com/sagarsuperuser/velox/internal/tenant"
	"github.com/sagarsuperuser/velox/internal/user"
)

// compositePaymentSetupStore implements payment.PaymentSetupStore on
// top of the canonical sources — customers.stripe_customer_id +
// payment_methods. Lets payment/checkout.go and payment/stripe.go
// keep their existing PaymentSetupStore-shaped wiring while the
// deprecated customer_payment_setups table is read-only on its way
// out. Compose the CustomerPaymentSetup wire shape from canonical
// state on read; write only the parts the new architecture owns
// (stripe_customer_id) and drop the rest (card details flow through
// paymentmethods.Service via the webhook attach path).
type compositePaymentSetupStore struct {
	customers *customer.PostgresStore
	pms       *paymentmethods.Service
}

func (a *compositePaymentSetupStore) GetPaymentSetup(ctx context.Context, tenantID, customerID string) (domain.CustomerPaymentSetup, error) {
	cust, err := a.customers.Get(ctx, tenantID, customerID)
	if err != nil {
		return domain.CustomerPaymentSetup{}, err
	}
	pms, err := a.pms.List(ctx, tenantID, customerID)
	if err != nil {
		return domain.CustomerPaymentSetup{}, err
	}
	ps := domain.CustomerPaymentSetup{
		CustomerID:       customerID,
		TenantID:         tenantID,
		StripeCustomerID: cust.StripeCustomerID,
		UpdatedAt:        cust.UpdatedAt,
		CreatedAt:        cust.CreatedAt,
	}
	// Find the default active PM. List returns active rows only
	// (detached_at IS NULL) and orders is_default DESC.
	for _, pm := range pms {
		if pm.IsDefault {
			ps.SetupStatus = domain.PaymentSetupReady
			ps.DefaultPaymentMethodPresent = true
			ps.PaymentMethodType = pm.Type
			ps.StripePaymentMethodID = pm.StripePaymentMethodID
			ps.CardBrand = pm.CardBrand
			ps.CardLast4 = pm.CardLast4
			ps.CardExpMonth = pm.CardExpMonth
			ps.CardExpYear = pm.CardExpYear
			return ps, nil
		}
	}
	// Stripe Customer exists but no default PM yet → pending.
	// No Stripe Customer at all → missing.
	if cust.StripeCustomerID != "" {
		ps.SetupStatus = domain.PaymentSetupPending
	} else {
		ps.SetupStatus = domain.PaymentSetupMissing
	}
	return ps, nil
}

func (a *compositePaymentSetupStore) UpsertPaymentSetup(ctx context.Context, tenantID string, ps domain.CustomerPaymentSetup) (domain.CustomerPaymentSetup, error) {
	return a.UpsertPaymentSetupAudited(ctx, tenantID, ps, nil)
}

// UpsertPaymentSetupAudited threads the ADR-090 emission onto the mapping
// write's tx (the only durable mutation on this path post-0097).
func (a *compositePaymentSetupStore) UpsertPaymentSetupAudited(ctx context.Context, tenantID string, ps domain.CustomerPaymentSetup, emit func(tx *sql.Tx) error) (domain.CustomerPaymentSetup, error) {
	// Only the stripe_customer_id mapping survives on this code
	// path. Card details, default-PM flag, and setup_status now
	// live on payment_methods (written via paymentmethods.Service
	// from the webhook attach path). The other fields on ps are
	// ignored — they're either redundant with payment_methods or
	// state-machine vestiges from the dropped summary table.
	if ps.StripeCustomerID != "" && ps.CustomerID != "" {
		if err := a.customers.SetStripeCustomerIDAudited(ctx, tenantID, ps.CustomerID, ps.StripeCustomerID, emit); err != nil {
			return domain.CustomerPaymentSetup{}, err
		}
	}
	// When no mapping write happened (missing ids), the emission is
	// intentionally dropped — there is no durable mutation to attach
	// evidence to.
	// Return the post-write composed view so callers see their
	// write reflected.
	return a.GetPaymentSetup(ctx, tenantID, ps.CustomerID)
}

// stripeCustomerResolverAdapter implements payment.CustomerByStripeIDResolver
// over the customer store. Lets the setup_intent.succeeded webhook map a
// SetupIntent's `customer` (a Stripe Customer ID) back to the Velox customer
// without payment/ importing customer/ — same composition-root pattern as the
// other adapters here.
type stripeCustomerResolverAdapter struct {
	customers *customer.PostgresStore
}

func (a *stripeCustomerResolverAdapter) CustomerIDByStripeID(ctx context.Context, tenantID, stripeCustomerID string) (string, error) {
	c, err := a.customers.GetByStripeCustomerID(ctx, tenantID, stripeCustomerID)
	if err != nil {
		return "", err
	}
	return c.ID, nil
}

// paymentReadinessAdapter implements billing.PaymentReadiness by
// combining customer.PostgresStore (for the Stripe Customer ID
// mapping) and paymentmethods.Service (for the canonical "has
// default active PM?" query). Replaces the legacy
// customer_payment_setups.GetPaymentSetup read path which lumped
// both facts into a single denorm-cache table. Single source of
// truth per concern:
//   - customers.stripe_customer_id is the customer ↔ Stripe Customer
//     mapping (1:1, lazy creation).
//   - payment_methods is the canonical multi-PM store; the default
//     row (is_default=true, detached_at IS NULL) is the chargeable
//     PM. List filters out detached rows server-side.
type paymentReadinessAdapter struct {
	customers *customer.PostgresStore
	pms       *paymentmethods.Service
}

func (a *paymentReadinessAdapter) ResolveForCharge(ctx context.Context, tenantID, customerID string) (string, string, error) {
	cust, err := a.customers.Get(ctx, tenantID, customerID)
	if err != nil {
		return "", "", err
	}
	if cust.StripeCustomerID == "" {
		// No Stripe Customer object yet — can't auto-charge.
		return "", "", nil
	}
	pms, err := a.pms.List(ctx, tenantID, customerID)
	if err != nil {
		return "", "", err
	}
	for _, pm := range pms {
		if pm.IsDefault {
			// Return the default card's Stripe PaymentMethod id so the
			// engine charges this exact card (not whatever Stripe's own
			// default happens to be). List filters detached rows, so this
			// is always a chargeable, non-detached PM.
			return cust.StripeCustomerID, pm.StripePaymentMethodID, nil
		}
	}
	// Stripe Customer exists but no default PM attached (e.g. customer
	// created via Stripe but card removed). Empty PM id → engine treats
	// this as "no PM ready" → queues for retry-on-attach.
	return cust.StripeCustomerID, "", nil
}

type invoiceWriterAdapter struct {
	store *invoice.PostgresStore
}

func (a *invoiceWriterAdapter) GetInvoice(ctx context.Context, tenantID, id string) (domain.Invoice, error) {
	return a.store.Get(ctx, tenantID, id)
}

func (a *invoiceWriterAdapter) MarkPaid(ctx context.Context, tenantID, id string, stripePaymentIntentID string, paidAt time.Time) (domain.Invoice, error) {
	return a.store.MarkPaid(ctx, tenantID, id, stripePaymentIntentID, paidAt)
}

func (a *invoiceWriterAdapter) CreateInvoiceWithLineItems(ctx context.Context, tenantID string, inv domain.Invoice, items []domain.InvoiceLineItem) (domain.Invoice, error) {
	return a.store.CreateWithLineItems(ctx, tenantID, inv, items)
}

func (a *invoiceWriterAdapter) CreateInvoiceWithLineItemsAudited(ctx context.Context, tenantID string, inv domain.Invoice, items []domain.InvoiceLineItem, emit func(tx *sql.Tx, out domain.Invoice) error) (domain.Invoice, error) {
	return a.store.CreateWithLineItemsAudited(ctx, tenantID, inv, items, emit)
}

func (a *invoiceWriterAdapter) CreateInvoiceWithLineItemsTx(ctx context.Context, tx *sql.Tx, tenantID string, inv domain.Invoice, items []domain.InvoiceLineItem) (domain.Invoice, error) {
	return a.store.CreateWithLineItemsTx(ctx, tx, tenantID, inv, items)
}

func (a *invoiceWriterAdapter) SetAutoChargePending(ctx context.Context, tenantID, id string, pending bool) error {
	return a.store.SetAutoChargePending(ctx, tenantID, id, pending)
}

func (a *invoiceWriterAdapter) SetNoPMNotifiedAt(ctx context.Context, tenantID, invoiceID string, at time.Time) error {
	return a.store.SetNoPMNotifiedAt(ctx, tenantID, invoiceID, at)
}

func (a *invoiceWriterAdapter) ClaimAutoCharge(ctx context.Context, tenantID, id string) (bool, error) {
	return a.store.ClaimAutoCharge(ctx, tenantID, id)
}

func (a *invoiceWriterAdapter) ReleaseAutoChargeClaim(ctx context.Context, tenantID, id string) error {
	return a.store.ReleaseAutoChargeClaim(ctx, tenantID, id)
}

func (a *invoiceWriterAdapter) ListAutoChargePending(ctx context.Context, limit int) ([]domain.Invoice, error) {
	return a.store.ListAutoChargePending(ctx, limit)
}

func (a *invoiceWriterAdapter) ListAutoChargePendingForClock(ctx context.Context, tenantID, clockID string, limit int) ([]domain.Invoice, error) {
	return a.store.ListAutoChargePendingForClock(ctx, tenantID, clockID, limit)
}

func (a *invoiceWriterAdapter) RecordChargeAttempt(ctx context.Context, tenantID string, att domain.InvoiceChargeAttempt) error {
	return a.store.RecordChargeAttempt(ctx, tenantID, att)
}

func (a *invoiceWriterAdapter) ListFailedWithoutDunningRun(ctx context.Context, olderThan time.Time, limit int) ([]domain.Invoice, error) {
	return a.store.ListFailedWithoutDunningRun(ctx, olderThan, limit)
}

func (a *invoiceWriterAdapter) SetTaxTransaction(ctx context.Context, tenantID, id string, taxTransactionID string) error {
	return a.store.SetTaxTransaction(ctx, tenantID, id, taxTransactionID)
}

func (a *invoiceWriterAdapter) ListLineItems(ctx context.Context, tenantID, invoiceID string) ([]domain.InvoiceLineItem, error) {
	return a.store.ListLineItems(ctx, tenantID, invoiceID)
}

func (a *invoiceWriterAdapter) UpdateTaxAtomic(ctx context.Context, tenantID, invoiceID string, update domain.InvoiceTaxRetryUpdate, lineItems []domain.InvoiceLineItem) (domain.Invoice, error) {
	return a.store.UpdateTaxAtomic(ctx, tenantID, invoiceID, update, lineItems)
}

func (a *invoiceWriterAdapter) FindFundingInvoicesForPeriod(ctx context.Context, tenantID, subscriptionID string, periodStart, periodEnd time.Time) ([]domain.Invoice, error) {
	return a.store.FindFundingInvoicesForPeriod(ctx, tenantID, subscriptionID, periodStart, periodEnd)
}

func (a *invoiceWriterAdapter) GetLatestThresholdInvoiceForCycle(ctx context.Context, tenantID, subscriptionID string, periodStart, periodEnd time.Time) (domain.Invoice, error) {
	return a.store.GetLatestThresholdInvoiceForCycle(ctx, tenantID, subscriptionID, periodStart, periodEnd)
}

func (a *invoiceWriterAdapter) GetLatestThresholdInvoiceForCycleTx(ctx context.Context, tx *sql.Tx, tenantID, subscriptionID string, periodStart, periodEnd time.Time) (domain.Invoice, error) {
	return a.store.GetLatestThresholdInvoiceForCycleTx(ctx, tx, tenantID, subscriptionID, periodStart, periodEnd)
}

func (a *invoiceWriterAdapter) GetInvoiceForPeriod(ctx context.Context, tenantID, subscriptionID string, periodStart, periodEnd time.Time) (domain.Invoice, error) {
	return a.store.GetInvoiceForPeriod(ctx, tenantID, subscriptionID, periodStart, periodEnd)
}

// creditGrantAdapter bridges credit.Service → creditnote.CreditGranter.
type creditGrantAdapter struct {
	svc *credit.Service
}

func (a *creditGrantAdapter) Grant(ctx context.Context, tenantID string, input creditnote.CreditGrantInput) error {
	_, err := a.svc.Grant(ctx, tenantID, credit.GrantInput{
		CustomerID:  input.CustomerID,
		AmountCents: input.AmountCents,
		Description: input.Description,
		InvoiceID:   input.InvoiceID,
	})
	return err
}

// GrantForCreditNote bridges to credit.Service.GrantForCreditNote so
// CN Issue() retries dedup via migration 0093's partial unique index.
func (a *creditGrantAdapter) GrantForCreditNote(ctx context.Context, tenantID, creditNoteID string, input creditnote.CreditGrantInput) error {
	_, err := a.svc.GrantForCreditNote(ctx, tenantID, creditNoteID, credit.GrantInput{
		CustomerID:  input.CustomerID,
		AmountCents: input.AmountCents,
		Description: input.Description,
		InvoiceID:   input.InvoiceID,
	})
	return err
}

// GrantForCreditNoteTx bridges to credit.Service.GrantForCreditNoteTx so the
// grant rides CN Issue()'s coordinator tx (ADR-061): the credit ledger entry
// commits atomically with the draft→issued CAS.
func (a *creditGrantAdapter) GrantForCreditNoteTx(ctx context.Context, tx *sql.Tx, tenantID, creditNoteID string, input creditnote.CreditGrantInput) error {
	_, err := a.svc.GrantForCreditNoteTx(ctx, tx, tenantID, creditNoteID, credit.GrantInput{
		CustomerID:  input.CustomerID,
		AmountCents: input.AmountCents,
		Description: input.Description,
		InvoiceID:   input.InvoiceID,
	})
	return err
}

// LockCommitGrantForReliefTx / RetireCommitSliceForReliefTx bridge the
// ADR-080 relief pair to credit.Service on the CN coordinator tx.
func (a *creditGrantAdapter) LockCommitGrantForReliefTx(ctx context.Context, tx *sql.Tx, tenantID, invoiceID string) (int64, int64, int64, bool, error) {
	return a.svc.LockCommitGrantForReliefTx(ctx, tx, tenantID, invoiceID)
}

func (a *creditGrantAdapter) RetireCommitSliceForReliefTx(ctx context.Context, tx *sql.Tx, tenantID, invoiceID, creditNoteID string, slice, refundedGrossCents, grossPaidCents int64) (int64, error) {
	res, err := a.svc.RetireCommitSliceForReliefTx(ctx, tx, tenantID, invoiceID, creditNoteID, slice, refundedGrossCents, grossPaidCents)
	if err != nil {
		return 0, err
	}
	return res.RemainingAfterCents, nil
}

// creditNoteListerAdapter bridges creditnote.Service → invoice.CreditNoteLister.
type creditNoteListerAdapter struct {
	svc *creditnote.Service
}

func (a *creditNoteListerAdapter) List(ctx context.Context, tenantID, invoiceID string) ([]domain.CreditNote, error) {
	return a.svc.List(ctx, creditnote.ListFilter{
		TenantID:  tenantID,
		InvoiceID: invoiceID,
		// Unset Limit fell back to the store default (50) with no
		// disclosure. Request the store's max; the timeline handler
		// reports `truncated` when the response comes back at the cap.
		Limit: invoice.CreditNoteLaneCap,
	})
}

// refundIssuerAdapter bridges creditnote.Service → invoice.RefundIssuer.
// Translates the handler-facing invoice.RefundInput to the creditnote form;
// both types are near-identical by design so the handler doesn't have to
// import creditnote just to issue a refund.
type refundIssuerAdapter struct {
	svc *creditnote.Service
}

func (a *refundIssuerAdapter) IssueRefund(ctx context.Context, tenantID string, input invoice.RefundInput) (domain.CreditNote, error) {
	return a.svc.CreateRefund(ctx, tenantID, creditnote.RefundInput{
		InvoiceID:   input.InvoiceID,
		AmountCents: input.AmountCents,
		Reason:      input.Reason,
		Description: input.Description,
	})
}

// paymentRetrierAdapter bridges Stripe + invoice/customer stores → dunning.PaymentRetrier.
type paymentRetrierAdapter struct {
	charger       *payment.Stripe
	invoiceStore  *invoice.PostgresStore
	paymentSetups payment.PaymentSetupStore
	credits       *credit.Service
}

func (a *paymentRetrierAdapter) RetryPayment(ctx context.Context, tenantID, invoiceID, customerID string) error {
	inv, err := a.invoiceStore.Get(ctx, tenantID, invoiceID)
	if err != nil {
		return fmt.Errorf("get invoice: %w", err)
	}
	if inv.AmountDueCents <= 0 {
		return nil // Nothing to charge
	}

	// Per-invoice charge lease, shared with the auto-charge sweep (HA
	// hazard #11): the two paths' Stripe idempotency keys differ by
	// construction (purpose suffix), so this mutual exclusion is the
	// only double-charge guard between them. Claim-lose = the sweep (or
	// a rival dunning leader) owns the invoice, or payment_status moved
	// to a state that must not be re-charged ('unknown' waits for the
	// reconciler) — map to ErrTransientSkip so the run does NOT burn an
	// attempt on a charge that never reached Stripe.
	claimed, err := a.invoiceStore.ClaimChargeForDunningRetry(ctx, tenantID, invoiceID)
	if err != nil {
		return fmt.Errorf("claim dunning retry charge: %w", err)
	}
	if !claimed {
		return dunning.ErrTransientSkip
	}

	// Re-apply customer credits before retrying the card — same contract as
	// the auto-charge sweep (processAutoCharge): credits granted since the
	// original failure (or whose application failed at cycle close) must
	// reduce the charge, not sit unconsumed while the card is hit for the
	// full amount. ApplyToInvoiceAt is idempotent (drains min(due, balance)).
	// An apply FAILURE maps to ErrTransientSkip: the retry never reaches
	// Stripe, so it must not burn a dunning attempt.
	if a.credits != nil {
		at := clock.Now(ctx) // sim-aware: catchup binds effective-now on ctx
		if _, err := a.credits.ApplyToInvoiceAt(ctx, tenantID, customerID, invoiceID, inv.AmountDueCents, at, inv.InvoiceNumber); err != nil {
			// Provably pre-Stripe: release so the next due tick retries
			// without waiting out the lease.
			_ = a.invoiceStore.ReleaseAutoChargeClaim(ctx, tenantID, invoiceID)
			return dunning.ErrTransientSkip
		}
		inv, err = a.invoiceStore.Get(ctx, tenantID, invoiceID)
		if err != nil {
			return fmt.Errorf("refresh invoice after credit apply: %w", err)
		}
		if inv.AmountDueCents <= 0 {
			// Fully covered by credits — settle without a card charge. nil
			// return = recovered, so the dunning run resolves; MarkPaid fires
			// the transactional invoice.paid event.
			if _, err := a.invoiceStore.MarkPaid(ctx, tenantID, invoiceID, "", at); err != nil {
				return fmt.Errorf("mark credit-covered invoice paid: %w", err)
			}
			return nil
		}
	}

	ps, err := a.paymentSetups.GetPaymentSetup(ctx, tenantID, customerID)
	if err != nil || ps.StripeCustomerID == "" || ps.StripePaymentMethodID == "" {
		// Provably pre-Stripe: release the lease before reporting.
		// Typed sentinel so the dunning warning email renders clean
		// customer copy instead of this internal string (P2-3).
		_ = a.invoiceStore.ReleaseAutoChargeClaim(ctx, tenantID, invoiceID)
		return domain.ErrNoPaymentMethodOnRetry
	}

	// 15s bound on the Stripe leg. Scheduler ticks run tens of retries
	// back-to-back; without this, one tenant with a network-partitioned
	// Stripe call could hold the goroutine for the full request deadline
	// (minutes), starving every other tenant's retry this tick.
	chargeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	// Dedicated retry-charge method tags the PI as velox_purpose=
	// dunning_retry so the payment_intent.payment_failed webhook
	// handler suppresses its generic payment-failed email — dunning
	// fires its own warning / escalation email inline. Without the
	// tag the customer receives two emails per failed retry: the
	// webhook's "Payment failed for invoice X" plus dunning's
	// "Action required — payment retry for invoice X (Attempt N of M)".
	_, err = a.charger.ChargeInvoiceForDunningRetry(chargeCtx, tenantID, inv, ps.StripeCustomerID, ps.StripePaymentMethodID)
	if cerr := classifyDunningRetryError(err); cerr != nil {
		// Counted declines carry their PI id across the boundary (typed
		// dunning wrapper, no payment import in dunning) so the failed
		// retry's timeline row can be matched to its exact
		// payment_intent.payment_failed webhook twin. Transient skips
		// pass through untouched — no attempt row is written for them.
		var pe *payment.PaymentError
		if !errors.Is(cerr, dunning.ErrTransientSkip) && errors.As(err, &pe) && pe.PaymentIntentID != "" {
			return &dunning.RetryError{PaymentIntentID: pe.PaymentIntentID, Err: cerr}
		}
		return cerr
	}
	return nil
}

// classifyDunningRetryError maps a dunning-retry CHARGE result to the signal
// dunning's processRun consumes. A "call never happened" transient
// (payment.ErrPaymentTransient) AND an AMBIGUOUS outcome (Stripe
// 5xx/timeout/network — the PaymentIntent may have actually succeeded) both
// map to dunning.ErrTransientSkip, so dunning does NOT tick attempt_count or
// exhaust on an outcome that might be a real payment. The inline charge path
// already defers unknowns to the reconciler the same way; pre-fix the
// dunning-retry path counted an unknown as a failed attempt and could
// exhaust → cancel / write off a possibly-PAID invoice. A DEFINITE decline
// (*PaymentError{Unknown:false}) returns the raw error so dunning counts it
// and the campaign advances to its terminal. Applied ONLY to the charge
// result — the earlier "no payment method" return is a real counted failure
// (drives the no-card invoice to its dunning terminal, ADR-060), unchanged.
func classifyDunningRetryError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, payment.ErrPaymentTransient) || payment.IsUnknownPaymentFailure(err) {
		return dunning.ErrTransientSkip
	}
	return err
}

// dunningStarterAdapter bridges billing.DunningStarter → dunning.Service so
// the engine's no-payment enrollment sweep can route a card-less,
// auto_charge_pending invoice into a dunning campaign (closing the limbo
// where it would otherwise be retried forever with nothing to charge and
// never reach a terminal).
//
// StartDunning returns InvalidState for two deliberate-skip cases, both
// swallowed here as a no-op: (1) dunning DISABLED, and (2) dunning NOT
// CONFIGURED — the tenant has no effective/default policy (StartDunning maps
// that ErrNotFound to InvalidState so an unconfigured optional feature never
// errors the money-path sweep). A tenant in either state gets no automated
// retries on the no-card case, exactly as it gets none on the declined-card
// case — so the per-tick sweep doesn't emit a spurious error for every stalled
// invoice. A genuine error (DB down, etc.) is NOT InvalidState and still
// propagates as a sweep error (fail loud).
// dunningRunStarter is the minimal slice of dunning.Service the adapter
// needs — an interface so the disabled-skip branch is unit-testable.
type dunningRunStarter interface {
	StartDunning(ctx context.Context, tenantID, invoiceID, customerID string, failureAt time.Time, cause domain.DunningStartCause) (domain.InvoiceDunningRun, error)
}

type dunningStarterAdapter struct {
	dunning dunningRunStarter
}

func (a *dunningStarterAdapter) StartDunning(ctx context.Context, tenantID, invoiceID, customerID string, failureAt time.Time, cause domain.DunningStartCause) error {
	if _, err := a.dunning.StartDunning(ctx, tenantID, invoiceID, customerID, failureAt, cause); err != nil {
		if errors.Is(err, errs.ErrInvalidState) {
			return nil // dunning disabled — deliberate skip, not a sweep error
		}
		return err
	}
	return nil
}

// subscriptionPauserAdapter bridges subscription.Service →
// dunning.SubscriptionPauser. ADR-036 amendment: this now calls
// PauseCollection(keep_as_draft), not the pre-amendment hard
// PauseAtomic — matching Stripe's pause_collection.behavior=
// keep_as_draft so the cycle keeps drafting (no silent skip of
// invoice generation).
type subscriptionPauserAdapter struct {
	svc *subscription.Service
}

func (a *subscriptionPauserAdapter) PauseCollection(ctx context.Context, tenantID, id string) error {
	_, err := a.svc.PauseCollection(ctx, tenantID, id, subscription.PauseCollectionInput{
		Behavior:    domain.PauseCollectionKeepAsDraft,
		TriggeredBy: "dunning",
	})
	return err
}

// subscriptionCancelerAdapter bridges subscription.Service →
// dunning.SubscriptionCanceler. Stripe-default dunning terminal
// action (ADR-036 amendment).
type subscriptionCancelerAdapter struct {
	svc *subscription.Service
}

func (a *subscriptionCancelerAdapter) Cancel(ctx context.Context, tenantID, id string) error {
	// Stamp the origin so the cancel's audit row and its outbound
	// subscription.canceled webhook — both written in the cancel tx — name
	// dunning as the driver, rather than each guessing from the absence of
	// a request actor (ADR-090 PR4 review).
	ctx = subscription.WithCancelOrigin(ctx, "dunning")
	_, _, err := a.svc.Cancel(ctx, tenantID, id)
	return err
}

// subscriptionStateAdapter bridges subscription.Service →
// dunning.SubscriptionStateReader — the skip-if-done read that lets a
// re-attempted exhaustion converge instead of failing forever on the half
// that already landed (ADR-112).
type subscriptionStateAdapter struct {
	svc *subscription.Service
}

func (a *subscriptionStateAdapter) GetTerminalState(ctx context.Context, tenantID, id string) (dunning.SubscriptionTerminalState, error) {
	sub, err := a.svc.Get(ctx, tenantID, id)
	if err != nil {
		return dunning.SubscriptionTerminalState{}, err
	}
	// The two states in which BOTH movers refuse: cancelSpec() allows only
	// draft/trialing/active, and SetPauseCollection rejects canceled and
	// archived outright.
	return dunning.SubscriptionTerminalState{
		Terminated: sub.Status == domain.SubscriptionCanceled ||
			sub.Status == domain.SubscriptionArchived,
	}, nil
}

// invoiceUncollectibleAdapter bridges invoice.Service →
// dunning.InvoiceUncollectibleMarker. Stripe-standard dunning
// terminal action (ADR-036 amendment).
type invoiceUncollectibleAdapter struct {
	svc *invoice.Service
}

func (a *invoiceUncollectibleAdapter) MarkUncollectible(ctx context.Context, tenantID, id string) error {
	_, err := a.svc.MarkUncollectible(ctx, tenantID, id)
	return err
}

// invoiceAuditStampsAdapter bridges audit.Logger.Query → invoice.AuditStampFetcher
// (ADR-104: the timeline's read-time wall-stamp enrichment for lifecycle rows).
// Narrow on purpose: one resource's entries, capped — an invoice's audit trail
// is dozens of rows at most, and the join only consumes the transition actions.
type invoiceAuditStampsAdapter struct {
	logger *audit.Logger
}

func (a *invoiceAuditStampsAdapter) ListByInvoice(ctx context.Context, tenantID, invoiceID string) ([]domain.AuditEntry, error) {
	// 100 is Query's hard cap (it CLAMPS above that — asking for more
	// would be a silent fallback). Newest-first: an invoice with >100
	// audit rows would lose its OLDEST entries — exactly the transition
	// stamps the join wants — so such a row renders bare rather than
	// wrong. Accepted: reaching 100 audit rows on one invoice means ~90
	// operator sends/exports; revisit with an Action-filtered query if a
	// real invoice ever gets there.
	entries, _, err := a.logger.Query(ctx, tenantID, audit.QueryFilter{
		ResourceType: "invoice",
		ResourceID:   invoiceID,
		Limit:        100,
	})
	return entries, err
}

// dunningTimelineAdapter bridges dunning.Store → invoice.DunningTimelineFetcher.
type dunningTimelineAdapter struct {
	store *dunning.PostgresStore
}

func (a *dunningTimelineAdapter) ListRunsByInvoice(ctx context.Context, tenantID, invoiceID string) ([]domain.InvoiceDunningRun, error) {
	runs, _, err := a.store.ListRuns(ctx, dunning.RunListFilter{TenantID: tenantID, InvoiceID: invoiceID})
	return runs, err
}

func (a *dunningTimelineAdapter) ListEvents(ctx context.Context, tenantID, runID string) ([]domain.InvoiceDunningEvent, error) {
	return a.store.ListEvents(ctx, tenantID, runID)
}

// customerEmailFetcherAdapter bridges customer.PostgresStore → dunning.CustomerEmailFetcher + payment.CustomerEmailResolver.
type customerEmailFetcherAdapter struct {
	store *customer.PostgresStore
}

func (a *customerEmailFetcherAdapter) GetCustomerEmail(ctx context.Context, tenantID, customerID string) (string, string, []string, error) {
	cust, err := a.store.Get(ctx, tenantID, customerID)
	if err != nil {
		return "", "", nil, err
	}
	email := cust.Email
	name := cust.DisplayName
	// customers.email is the single canonical recipient (Phase 1 of the
	// dual-email collapse, migration 0100). Billing-profile legal_name
	// still wins for display because the operator types the document
	// display name there, but the send target is always cust.Email.
	if bp, err := a.store.GetBillingProfile(ctx, tenantID, customerID); err == nil {
		if bp.LegalName != "" {
			name = bp.LegalName
		}
	}
	// AdditionalEmails ride the same row read — zero extra queries. CC'd
	// on the billing-state emails (dunning, receipt, payment failed) per
	// the ADR-082 coverage matrix.
	return email, name, cust.AdditionalEmails, nil
}

// passwordResetEmailAdapter bridges email.OutboxSender (or the
// direct *email.Sender) to user.EmailSender's narrower 2-arg
// signature. The user package emits password-reset emails before
// the operator authenticates, so it has no tenant or display-name
// context — empty values flow through to email.Sender.brandingFor,
// which falls back to platform defaults. Production wires the
// outbox-backed sender so reset emails are durable.
type passwordResetEmailAdapter struct {
	sender interface {
		SendPasswordReset(ctx context.Context, tenantID, to, displayName, resetURL string) error
	}
}

func (a *passwordResetEmailAdapter) SendPasswordReset(ctx context.Context, tenantID, email, resetLink string) error {
	// Email-as-display-name: body shows "Hi alice@example.com"
	// rather than "Hi Alice". Acceptable trade for not threading
	// the user's display_name through the password-reset request
	// flow. tenantID comes from user.Service.IssueResetToken so the
	// outbox row stamps the right tenant for operator visibility.
	return a.sender.SendPasswordReset(ctx, tenantID, email, email, resetLink)
}

// invoiceEmailEventsAdapter bridges email.OutboxStore.ListByInvoice
// → invoice.EmailEventLister so the invoice timeline can surface
// customer-notification email rows alongside Stripe webhooks and
// dunning events. Pure shape conversion; the underlying query
// already filters to invoice-relevant email types.
type invoiceEmailEventsAdapter struct {
	store *email.OutboxStore
}

func (a *invoiceEmailEventsAdapter) ListByInvoice(ctx context.Context, tenantID, invoiceNumber string) ([]invoice.EmailEventRow, error) {
	rows, err := a.store.ListByInvoice(ctx, tenantID, invoiceNumber)
	if err != nil {
		return nil, err
	}
	out := make([]invoice.EmailEventRow, 0, len(rows))
	for _, r := range rows {
		to, _ := r.Payload["to"].(string)
		// JSON numbers decode to float64; dunning_warning payloads
		// stamp attempt_number for pairing with retry rows.
		attempt, _ := r.Payload["attempt_number"].(float64)
		trigger, _ := r.Payload["trigger"].(string)
		out = append(out, invoice.EmailEventRow{
			ID:             r.ID,
			EmailType:      r.EmailType,
			Status:         r.Status,
			DeliveryState:  r.DeliveryState,
			CreatedAt:      r.CreatedAt,
			DispatchedAt:   r.DispatchedAt,
			LastError:      r.LastError,
			To:             to,
			AttemptNumber:  int(attempt),
			Trigger:        trigger,
			SimEffectiveAt: r.SimEffectiveAt,
		})
	}
	return out, nil
}

// paymentSetupEmailSender is the narrow interface noPaymentMethodNotifierAdapter
// needs from email/. Defined here (consumer-side) so the email package
// doesn't bleed an interface name into the api/payment dep graph.
// Satisfied by both *email.Sender and *email.OutboxSender.
type paymentSetupEmailSender interface {
	SendPaymentSetupRequest(ctx context.Context, tenantID, to, customerName, invoiceNumber string, amountDueCents int64, currency, updateURL string, writtenOff bool, trigger string) error
}

// customerSentEmailsAdapter bridges email.OutboxStore.ListByCustomer →
// customer.SentEmailsLister. Pure shape conversion plus payload field
// extraction (recipient, invoice_number) so the customer package
// doesn't import the email package.
type customerSentEmailsAdapter struct {
	store *email.OutboxStore
}

func (a *customerSentEmailsAdapter) ListByCustomer(ctx context.Context, tenantID, customerID string) ([]customer.SentEmailOutboxRow, error) {
	rows, err := a.store.ListByCustomer(ctx, tenantID, customerID)
	if err != nil {
		return nil, err
	}
	out := make([]customer.SentEmailOutboxRow, 0, len(rows))
	for _, r := range rows {
		to, _ := r.Payload["to"].(string)
		invNum, _ := r.Payload["invoice_number"].(string)
		out = append(out, customer.SentEmailOutboxRow{
			ID:            r.ID,
			EmailType:     r.EmailType,
			Recipient:     to,
			Status:        r.Status,
			DeliveryState: r.DeliveryState,
			LastError:     r.LastError,
			CreatedAt:     r.CreatedAt,
			DispatchedAt:  r.DispatchedAt,
			InvoiceNumber: invNum,
		})
	}
	return out, nil
}

// noPaymentMethodNotifierAdapter bridges email.SendPaymentSetupRequest
// into billing.NoPaymentMethodNotifier so the engine can dispatch a
// "set up your payment method" email at finalize when the customer
// has no PM ready. Distinct from the post-decline path
// (Stripe.handlePaymentFailed) which points the customer at the
// hosted invoice URL — this email mints a single-use token for the
// /update-payment SPA route. ADR-013, ADR-023.
//
// The token-then-SPA indirection is retained (vs. embedding the Stripe
// Checkout URL directly) because the SPA validates the customer-scoped
// token first, surfaces invoice context to the customer, and only THEN
// mints a Checkout session. That two-step is the right shape for an
// email link the customer clicks days after issuance: the operator can
// invalidate tokens at any time, and the SPA shows "for invoice X, $Y"
// before sending the customer to Stripe.
type noPaymentMethodNotifierAdapter struct {
	email            paymentSetupEmailSender
	customerEmail    *customerEmailFetcherAdapter
	paymentUpdateURL string
	tokenSvc         *payment.TokenService
	auditLogger      *audit.Logger // optional — engine fires this background task; audit row records the send for operator forensics.
}

// trigger is supplied by the CALLER because only the caller knows why the link is
// being sent. This adapter used to hardcode "finalize_no_pm" — so an OPERATOR
// clicking "Resend setup link" produced a permanent row asserting the mail was sent
// by the finalize path, which never ran. A row that names the wrong cause is the
// same class of false evidence the URL-guessing catch-all was deleted for.
func (a *noPaymentMethodNotifierAdapter) NotifyNoPaymentMethod(ctx context.Context, tenantID string, inv domain.Invoice, trigger string) (domain.NotifyOutcome, error) {
	if a.paymentUpdateURL == "" {
		return "", errors.New("PAYMENT_UPDATE_URL not configured")
	}
	if a.tokenSvc == nil {
		// The SPA enforces ?token= and rejects URLs without it
		// ("Link expired or invalid · No payment update token
		// provided"). Refusing to send is strictly better than
		// emailing the customer a permanently-broken link.
		return "", errors.New("payment update TokenService not wired")
	}
	// CC list deliberately discarded: payment_setup_request carries a
	// single-use tokenized payment-credential URL — never-CC by
	// construction (ADR-082 coverage matrix).
	to, name, _, err := a.customerEmail.GetCustomerEmail(ctx, tenantID, inv.CustomerID)
	if err != nil || to == "" {
		// Missing email is a delivery gap, not a billing failure — but it
		// is REPORTED, not swallowed: the typed skip lets the engine log
		// the real disposition and lets the resend endpoint answer with a
		// typed 409 instead of a false "sent" (2026-07-10 design review).
		return domain.NotifySkippedNoEmail, nil
	}
	rawToken, err := a.tokenSvc.Create(ctx, tenantID, inv.CustomerID, inv.ID)
	if err != nil {
		return "", fmt.Errorf("create payment update token: %w", err)
	}
	updateURL := fmt.Sprintf("%s?token=%s", a.paymentUpdateURL, rawToken)
	if err := a.email.SendPaymentSetupRequest(ctx, tenantID, to, name, inv.InvoiceNumber, inv.AmountDueCents, inv.Currency, updateURL, inv.Status == domain.InvoiceUncollectible, trigger); err != nil {
		return "", err
	}
	// Audit the engine-fired send so the operator can answer
	// "did we email the customer at finalize?" from the AuditLog page
	// without grepping outbox tables. Mirrors the operator-driven
	// setup_link_sent audit row written by paymentmethods.Handler.
	if a.auditLogger != nil {
		// No recipient address in the append-only row (GDPR erasure) — the
		// email outbox holds the delivery record; the row links the customer.
		_ = a.auditLogger.Log(ctx, tenantID, "update", "customer", inv.CustomerID, name, map[string]any{
			"action":         "setup_link_sent",
			"invoice_id":     inv.ID,
			"invoice_number": inv.InvoiceNumber,
			"trigger":        trigger,
		})
	}
	return domain.NotifySent, nil
}

// prorationCreditGranterAdapter bridges credit.Service → subscription.ProrationCreditGranter.
type prorationCreditGranterAdapter struct {
	svc *credit.Service
}

func (a *prorationCreditGranterAdapter) GrantProration(ctx context.Context, tenantID string, input subscription.ProrationGrantInput) error {
	planChangedAt := input.SourcePlanChangedAt
	_, err := a.svc.Grant(ctx, tenantID, credit.GrantInput{
		CustomerID:               input.CustomerID,
		AmountCents:              input.AmountCents,
		Description:              input.Description,
		SourceSubscriptionID:     input.SourceSubscriptionID,
		SourceSubscriptionItemID: input.SourceSubscriptionItemID,
		SourcePlanChangedAt:      &planChangedAt,
		SourceChangeType:         input.SourceChangeType,
	})
	return err
}

func (a *prorationCreditGranterAdapter) GetByProrationSource(ctx context.Context, tenantID, subscriptionID, subscriptionItemID string, changeType domain.ItemChangeType, changeAt time.Time) (domain.CreditLedgerEntry, error) {
	return a.svc.GetByProrationSource(ctx, tenantID, subscriptionID, subscriptionItemID, changeType, changeAt)
}

// GrantProrationTx is the in-transaction variant. The caller owns the
// tx; this adapter forwards to the credit service's tx-aware grant
// path. ADR-030 atomic-proration follow-through (2026-05-29).
func (a *prorationCreditGranterAdapter) GrantProrationTx(ctx context.Context, tx *sql.Tx, tenantID string, input subscription.ProrationGrantInput) error {
	planChangedAt := input.SourcePlanChangedAt
	_, err := a.svc.GrantTx(ctx, tx, tenantID, credit.GrantInput{
		CustomerID:               input.CustomerID,
		AmountCents:              input.AmountCents,
		Description:              input.Description,
		SourceSubscriptionID:     input.SourceSubscriptionID,
		SourceSubscriptionItemID: input.SourceSubscriptionItemID,
		SourcePlanChangedAt:      &planChangedAt,
		SourceChangeType:         input.SourceChangeType,
	})
	return err
}

// prorationInvoiceCreatorAdapter bridges invoice.PostgresStore + tenant.SettingsStore → subscription.ProrationInvoiceCreator.
type prorationInvoiceCreatorAdapter struct {
	store    *invoice.PostgresStore
	numberer invoice.InvoiceNumberer
}

func (a *prorationInvoiceCreatorAdapter) CreateInvoiceWithLineItems(ctx context.Context, tenantID string, inv domain.Invoice, items []domain.InvoiceLineItem) (domain.Invoice, error) {
	return a.store.CreateWithLineItems(ctx, tenantID, inv, items)
}

func (a *prorationInvoiceCreatorAdapter) GetByProrationSource(ctx context.Context, tenantID, subscriptionID, subscriptionItemID string, changeType domain.ItemChangeType, changeAt time.Time) (domain.Invoice, error) {
	return a.store.GetByProrationSource(ctx, tenantID, subscriptionID, subscriptionItemID, changeType, changeAt)
}

func (a *prorationInvoiceCreatorAdapter) NextInvoiceNumber(ctx context.Context, tenantID string) (string, error) {
	return a.numberer.NextInvoiceNumber(ctx, tenantID)
}

func (a *prorationInvoiceCreatorAdapter) FindBaseInvoiceForPeriod(ctx context.Context, tenantID, subscriptionID string, periodStart time.Time) (domain.Invoice, error) {
	return a.store.FindBaseInvoiceForPeriod(ctx, tenantID, subscriptionID, periodStart)
}

func (a *prorationInvoiceCreatorAdapter) FindFundingInvoicesForPeriod(ctx context.Context, tenantID, subscriptionID string, periodStart, periodEnd time.Time) ([]domain.Invoice, error) {
	return a.store.FindFundingInvoicesForPeriod(ctx, tenantID, subscriptionID, periodStart, periodEnd)
}

// CreateInvoiceWithLineItemsTx + NextInvoiceNumberTx are the
// in-transaction variants used by the atomic AddItem-with-proration
// flow. The caller (subscription.Handler) owns the tx; this adapter
// just forwards to the underlying store + numberer's Tx methods.
// ADR-030 atomic-proration follow-through (2026-05-29).
func (a *prorationInvoiceCreatorAdapter) CreateInvoiceWithLineItemsTx(ctx context.Context, tx *sql.Tx, tenantID string, inv domain.Invoice, items []domain.InvoiceLineItem) (domain.Invoice, error) {
	return a.store.CreateWithLineItemsTx(ctx, tx, tenantID, inv, items)
}

func (a *prorationInvoiceCreatorAdapter) NextInvoiceNumberTx(ctx context.Context, tx *sql.Tx, tenantID string) (string, error) {
	return a.numberer.NextInvoiceNumberTx(ctx, tx, tenantID)
}

// SetAutoChargePending enrolls a finalized proration charge invoice into the
// auto-charge sweep (forwards to the store's existing method — the same one the
// engine's finalize path uses).
func (a *prorationInvoiceCreatorAdapter) SetAutoChargePending(ctx context.Context, tenantID, id string, pending bool) error {
	return a.store.SetAutoChargePending(ctx, tenantID, id, pending)
}

// SetAutoChargePendingTx forwards to the store's tx-aware variant so the atomic
// proration path enrolls inside its own tx.
func (a *prorationInvoiceCreatorAdapter) SetAutoChargePendingTx(ctx context.Context, tx *sql.Tx, tenantID, id string, pending bool) error {
	return a.store.SetAutoChargePendingTx(ctx, tx, tenantID, id, pending)
}

// prorationTaxApplierAdapter bridges billing.Engine → subscription.ProrationTaxApplier.
// Narrow translation: same signature, different named return type so the
// subscription package doesn't import billing.
type prorationTaxApplierAdapter struct {
	engine *billing.Engine
}

func (a *prorationTaxApplierAdapter) ApplyTaxToLineItems(ctx context.Context, tenantID, customerID, currency string, subtotal, discount int64, lineItems []domain.InvoiceLineItem) (subscription.ProrationTaxResult, error) {
	r, err := a.engine.ApplyTaxToLineItems(ctx, tenantID, customerID, currency, subtotal, discount, lineItems)
	if err != nil {
		return subscription.ProrationTaxResult{}, err
	}
	return subscription.ProrationTaxResult{
		// ONE assignment carries every tax fact (see domain/tax_facts.go) —
		// this hand-copy dropped fields three separate times; now impossible.
		TaxFacts:      r.TaxFacts,
		SubtotalCents: r.SubtotalCents,
		DiscountCents: r.DiscountCents,
	}, nil
}

// CommitTax commits a provider tax calculation into a reportable tax
// transaction, delegating to the same engine method the cycle/create paths
// use. No-op for manual/none providers (no calculation id).
func (a *prorationTaxApplierAdapter) CommitTax(ctx context.Context, tenantID, invoiceID, calculationID string) error {
	return a.engine.CommitTax(ctx, tenantID, invoiceID, calculationID)
}

// pmCustomerLookupAdapter bridges customer.PostgresStore →
// paymentmethods.CustomerLookup. paymentmethods doesn't import
// customer (keeps the dep graph one-way); router.go composes the
// adapter so the operator send-setup-email endpoint can resolve the
// recipient address + display name from the canonical customer row.
type pmCustomerLookupAdapter struct {
	store *customer.PostgresStore
}

func (a *pmCustomerLookupAdapter) GetForSetupLink(ctx context.Context, tenantID, customerID string) (email, displayName string, err error) {
	c, err := a.store.Get(ctx, tenantID, customerID)
	if err != nil {
		return "", "", err
	}
	return c.Email, c.DisplayName, nil
}

// bounceReporterAdapter bridges email.Sender → customer.Service. When
// SMTP rejects a send with a permanent-failure (5xx), the Sender calls
// ReportBounce with (tenantID, email, reason). This adapter resolves
// the email via the blind-index lookup and flips email_status on every
// matching customer in that tenant. Multiple matches can happen when
// two customers share an email (rare; we mark all).
//
// Lives in internal/api so email doesn't import customer — keeps the
// one-way-coupled layering intact.
type bounceReporterAdapter struct {
	blinder *crypto.Blinder
	store   *customer.PostgresStore
	svc     *customer.Service
}

func (a *bounceReporterAdapter) ReportBounce(ctx context.Context, tenantID, email, reason string) {
	if a == nil || a.store == nil || a.svc == nil || a.blinder == nil || email == "" {
		return
	}
	blind := a.blinder.Blind(email)
	if blind == "" {
		return
	}
	matches, err := a.store.FindByEmailBlindIndex(ctx, blind, 10)
	if err != nil {
		return
	}
	for _, m := range matches {
		if m.TenantID != tenantID {
			continue
		}
		if err := a.svc.MarkEmailBounced(ctx, tenantID, m.CustomerID, reason); err != nil {
			// Swallow — the sender has already logged the upstream bounce.
			// Failure here just means the badge won't update, not a
			// correctness issue.
			_ = err
		}
	}
}

// SuppressBounced / SuppressComplained satisfy email.RecipientSuppressor
// for the Postmark webhook (ADR-098). Same blind-index resolution as
// ReportBounce, but errors RETURN so a transient DB failure can 5xx and
// ride Postmark's redelivery into the idempotent writes. Zero matches is
// benign (a never-a-customer CC alias — log-only per ADR-082), as is a
// store-level ErrNotFound (customer deleted between lookup and write).
func (a *bounceReporterAdapter) SuppressBounced(ctx context.Context, tenantID, email, reason string) error {
	return a.suppress(ctx, tenantID, email, reason, false)
}

func (a *bounceReporterAdapter) SuppressComplained(ctx context.Context, tenantID, email, reason string) error {
	return a.suppress(ctx, tenantID, email, reason, true)
}

func (a *bounceReporterAdapter) suppress(ctx context.Context, tenantID, email, reason string, complaint bool) error {
	if a == nil || a.store == nil || a.svc == nil || a.blinder == nil || email == "" {
		return nil
	}
	blind := a.blinder.Blind(email)
	if blind == "" {
		return nil
	}
	matches, err := a.store.FindByEmailBlindIndex(ctx, blind, 10)
	if err != nil {
		return err
	}
	for _, m := range matches {
		if m.TenantID != tenantID {
			continue
		}
		var markErr error
		if complaint {
			markErr = a.svc.MarkEmailComplained(ctx, tenantID, m.CustomerID, reason)
		} else {
			markErr = a.svc.MarkEmailBounced(ctx, tenantID, m.CustomerID, reason)
		}
		if markErr != nil && !errors.Is(markErr, errs.ErrNotFound) {
			return markErr
		}
	}
	return nil
}

// suppressionCheckerAdapter is the inverse of bounceReporterAdapter:
// given (tenant, email), it answers "is this recipient suppressable
// because we've already seen them bounce or complain?". Both
// email.Sender and email.OutboxSender consult this before every send
// so bounced addresses don't get hammered with retry-DLQ noise.
//
// Returns the first matching customer's email_status; if multiple
// customers share the same email (rare) and any one is suppressed,
// the address is treated as suppressed for that tenant. Better to
// suppress one legitimate send than to keep mailing a dead address.
type suppressionCheckerAdapter struct {
	blinder *crypto.Blinder
	store   *customer.PostgresStore
}

func (a *suppressionCheckerAdapter) IsSuppressed(ctx context.Context, tenantID, emailAddr string) (bool, string, error) {
	if a == nil || a.store == nil || a.blinder == nil || emailAddr == "" || tenantID == "" {
		return false, "", nil
	}
	blind := a.blinder.Blind(emailAddr)
	if blind == "" {
		return false, "", nil
	}
	matches, err := a.store.FindByEmailBlindIndex(ctx, blind, 10)
	if err != nil {
		return false, "", err
	}
	for _, m := range matches {
		if m.TenantID != tenantID {
			continue
		}
		c, err := a.store.Get(ctx, tenantID, m.CustomerID)
		if err != nil {
			continue
		}
		switch c.EmailStatus {
		case domain.EmailStatusBounced:
			return true, "bounced: " + c.EmailBounceReason, nil
		case domain.EmailStatusComplained:
			return true, "complained", nil
		}
	}
	return false, "", nil
}

// stripeCustomerEnsurer resolves-or-creates the Stripe customer for a
// Velox customer + persists the mapping. Satisfied by
// *paymentmethods.StripeAdapter — kept as a one-method interface here
// so api/adapters.go doesn't need to import paymentmethods directly.
type stripeCustomerEnsurer interface {
	EnsureStripeCustomer(ctx context.Context, tenantID, customerID string) (string, error)
}

// hostedInvoiceStripeAdapter bridges *payment.StripeClients →
// hostedinvoice.CheckoutSessionCreator. The caller is a public
// unauthenticated request: livemode is read from the request ctx
// (pinned by hostedinvoice.resolveInvoice from the GetByPublicToken
// row), then we pick the matching Stripe key and build a Checkout
// Session in payment mode with a single pre-totaled line item.
// Velox owns the tax computation, so Stripe's automatic_tax is
// intentionally off — the UnitAmount already includes tax from the
// invoice row.
//
// Metadata stamps velox_purpose=hosted_invoice_pay so the existing
// payment-intent webhook path can route successful charges to
// Invoice.RecordPayment via invoice_id lookup, instead of mis-identifying
// these as subscription-billing charges.
//
// `ensurer` resolves or mints the Stripe customer so the Checkout
// session can attach the saved card to it; without `customer` +
// `setup_future_usage=off_session` Stripe takes the card but never
// links it to the Velox customer's Stripe customer record, so the
// "save card" tick on the hosted page would be a no-op.
type hostedInvoiceStripeAdapter struct {
	clients *payment.StripeClients
	ensurer stripeCustomerEnsurer
	// sessions is the checkout-claim ledger (ADR-068): claim-first dedup
	// with a claim-derived Stripe idempotency key, so one invoice can never
	// hold two live payable sessions.
	sessions *payment.CheckoutSessionStore
	// audit is the ADR-090 in-tx emitter: the hosted-checkout claim (the
	// flow's one durable mutation) carries its evidence atomically. Set at
	// construction — a public customer-token money path must not silently
	// run unaudited.
	audit *audit.Logger
}

func (a *hostedInvoiceStripeAdapter) CreateInvoicePaymentSession(
	ctx context.Context, tenantID string, inv domain.Invoice, successURL, cancelURL string,
) (string, error) {
	if a == nil || a.clients == nil {
		return "", fmt.Errorf("stripe not configured")
	}
	if a.sessions == nil {
		// Fail loud: minting without the claim ledger recreates the
		// double-charge bug this adapter exists to prevent (ADR-068).
		return "", fmt.Errorf("hosted invoice: checkout session store not wired")
	}
	// Livemode comes from ctx, pinned by resolveInvoice off the
	// public_token lookup. Previous version did a raw db.Pool query
	// on `invoices` which has RLS — without app.tenant_id /
	// app.bypass_rls set on the session, the policy returned zero
	// rows ("resolve livemode: sql: no rows in result set").
	livemode := postgres.Livemode(ctx)
	sc := a.clients.For(ctx, tenantID, livemode)
	if sc == nil {
		return "", fmt.Errorf("stripe not configured for mode livemode=%v", livemode)
	}
	if a.ensurer == nil {
		return "", fmt.Errorf("hosted invoice: Stripe customer ensurer not wired")
	}
	stripeCustomerID, err := a.ensurer.EnsureStripeCustomer(ctx, tenantID, inv.CustomerID)
	if err != nil {
		return "", fmt.Errorf("ensure stripe customer: %w", err)
	}

	// ---- Claim protocol (ADR-068) ----
	// Reuse an existing open claim when its session is still live and its
	// amount still matches; supersede on time-expiry or amount drift; the
	// concurrent-double-POST loser converges on the winner's session via the
	// claim-derived idempotency key.
	claim, err := a.sessions.GetOpenForInvoice(ctx, tenantID, inv.ID)
	switch {
	case err == nil:
		expired := claim.ExpiresAt != nil && time.Now().After(claim.ExpiresAt.Add(-time.Minute))
		drifted := claim.AmountCents != inv.AmountDueCents
		if !expired && !drifted && claim.URL != "" {
			// Straight reuse: both devices get the SAME session. NOTHING is
			// mutated — the claim already exists and only its winner emitted.
			// Declare that to the audit observer, or a customer clicking "Pay"
			// twice reports as an uncovered mutation forever (ADR-090: a
			// detector that cries wolf on a normal retry is one nobody keeps).
			audit.MarkSkip(ctx)
			return claim.URL, nil
		}
		if !expired && !drifted && claim.URL == "" {
			// Pending claim (a crash between insert and create, or a racing
			// winner mid-create): re-drive the create with the claim's own
			// idempotency key — Stripe returns the SAME session either way.
			// No new claim row, so no emission: same declaration as above.
			audit.MarkSkip(ctx)
			return a.mintForClaim(ctx, sc, claim, stripeCustomerID, inv, successURL, cancelURL)
		}
		if drifted && !expired && claim.StripeSessionID != "" {
			// Amount drift with a live session: the customer may be PAYING it
			// right now. Branch on the Stripe-side truth before minting a
			// second charge vehicle (panel: expiring a COMPLETED session
			// fails — minting anyway double-charges).
			if perr := a.expireStripeSession(ctx, sc, claim.StripeSessionID); perr != nil {
				if errors.Is(perr, payment.ErrSessionCompleted) {
					// Payment already happened at the old amount; the webhook
					// will settle + escalate the amount mismatch. Never mint.
					return "", payment.ErrChargeInFlight
				}
				// Network failure: accepted ≤1h residual (Stripe-side
				// ExpiresAt bounds the orphan); proceed to supersede + remint.
				slog.WarnContext(ctx, "checkout: best-effort expire of drifted session failed",
					"invoice_id", inv.ID, "claim_id", claim.ID, "error", perr)
			}
		}
		// Supersede via CAS — exactly one concurrent caller wins the remint.
		if won, sErr := a.sessions.Supersede(ctx, tenantID, claim.ID); sErr != nil {
			return "", fmt.Errorf("supersede stale claim: %w", sErr)
		} else if !won {
			// Another racer superseded and is reminting; fall through to
			// ClaimOpen which returns THEIR fresh claim (loser protocol).
			_ = won
		}
	case errors.Is(err, errs.ErrNotFound):
		// No open claim — claim fresh below.
	default:
		return "", fmt.Errorf("read open checkout claim: %w", err)
	}

	var emit func(tx *sql.Tx, claim payment.CheckoutClaim) error
	if a.audit != nil {
		emit = func(tx *sql.Tx, claim payment.CheckoutClaim) error {
			return a.audit.LogInTx(ctx, tx, audit.Entry{
				Action:        domain.AuditActionUpdate,
				ResourceType:  "invoice",
				ResourceID:    inv.ID,
				ResourceLabel: inv.InvoiceNumber,
				Metadata: map[string]any{
					"action":       "hosted_checkout_started",
					"amount_cents": claim.AmountCents,
					"currency":     claim.Currency,
					"claim_id":     claim.ID,
				},
			})
		}
	}
	freshClaim, _, err := a.sessions.ClaimOpenAudited(ctx, tenantID, inv.ID, inv.AmountDueCents, inv.Currency, livemode, emit)
	if err != nil {
		// ErrInvoiceNotPayable / ErrChargeInFlight propagate typed — the
		// handler maps them to honest 409s.
		return "", err
	}
	if freshClaim.URL != "" {
		// Loser protocol fast path: the winner already filled the session.
		return freshClaim.URL, nil
	}
	return a.mintForClaim(ctx, sc, freshClaim, stripeCustomerID, inv, successURL, cancelURL)
}

// mintForClaim creates (or idempotently re-drives) the Stripe Checkout
// session for one claim and fills the row. The idempotency key IS the claim
// id: a crash before the fill, a concurrent loser, or a retry all receive
// the same session from Stripe — one invoice can never grow two live
// payable sessions.
func (a *hostedInvoiceStripeAdapter) mintForClaim(
	ctx context.Context, sc *stripe.Client, claim payment.CheckoutClaim,
	stripeCustomerID string, inv domain.Invoice, successURL, cancelURL string,
) (string, error) {
	currency := strings.ToLower(claim.Currency)
	productName := "Invoice " + inv.InvoiceNumber
	// Duplicate the metadata on BOTH the session and the PaymentIntent.
	// Session-level metadata only lives on the checkout_session object;
	// Stripe does NOT automatically copy it to the underlying PaymentIntent.
	// Our webhook path routes payment_intent.succeeded → Invoice.MarkPaid
	// by reading velox_invoice_id off the PI's metadata (payment/handler.go
	// line 195), so PaymentIntentData.Metadata is the one that actually
	// matters. The session copy is kept for operator visibility when
	// inspecting a checkout session directly in Stripe dashboard.
	meta := map[string]string{
		"velox_invoice_id":  inv.ID,
		"velox_tenant_id":   claim.TenantID,
		"velox_customer_id": inv.CustomerID,
		"velox_purpose":     "hosted_invoice_pay",
	}
	// `customer` + `setup_future_usage=off_session` is the canonical
	// Stripe pattern for "charge now, save the card for auto-charge of
	// future invoices" (https://docs.stripe.com/payments/save-during-payment).
	// Without `customer`, Stripe creates a guest checkout and the
	// PaymentMethod is never attached to anything. Without
	// `setup_future_usage`, the card is consumed by the one-shot PI and
	// detached. Both are required for dunning auto-retry to work.
	sess, err := sc.V1CheckoutSessions.Create(ctx, &stripe.CheckoutSessionCreateParams{
		Customer: stripe.String(stripeCustomerID),
		Mode:     stripe.String(string(stripe.CheckoutSessionModePayment)),
		LineItems: []*stripe.CheckoutSessionCreateLineItemParams{
			{
				Quantity: stripe.Int64(1),
				PriceData: &stripe.CheckoutSessionCreateLineItemPriceDataParams{
					Currency:   stripe.String(currency),
					UnitAmount: stripe.Int64(claim.AmountCents),
					ProductData: &stripe.CheckoutSessionCreateLineItemPriceDataProductDataParams{
						Name: stripe.String(productName),
					},
				},
			},
		},
		PaymentMethodTypes: stripe.StringSlice([]string{"card"}),
		SuccessURL:         stripe.String(successURL),
		CancelURL:          stripe.String(cancelURL),
		PaymentIntentData: &stripe.CheckoutSessionCreatePaymentIntentDataParams{
			Metadata:         meta,
			Description:      stripe.String(productName),
			SetupFutureUsage: stripe.String("off_session"),
		},
		// 1h Stripe-side expiry (min 30m, default 24h): the DB TTL alone
		// would leave a ~23h invisible payable session (ADR-068).
		ExpiresAt: stripe.Int64(time.Now().Add(time.Hour).Unix()),
		Params: stripe.Params{
			Metadata: meta,
			// The claim id: every re-drive for this claim converges on the
			// SAME Stripe session.
			IdempotencyKey: stripe.String(claim.ID),
		},
	})
	if err != nil {
		return "", fmt.Errorf("checkout session: %w", err)
	}
	// Persist Stripe's ACTUAL expiry (it clamps requests) via CAS; losing the
	// CAS to a concurrent re-drive is fine — same session, same values.
	expiresAt := time.Unix(sess.ExpiresAt, 0).UTC()
	if fErr := a.sessions.FillSession(ctx, claim.TenantID, claim.ID, sess.ID, sess.URL, expiresAt); fErr != nil {
		// The session exists at Stripe and the claim row will be re-driven to
		// the same session on the next POST (idempotency key) — log, don't
		// fail the customer.
		slog.WarnContext(ctx, "checkout: fill claim row failed (recoverable via idempotent re-drive)",
			"claim_id", claim.ID, "error", fErr)
	}
	return sess.URL, nil
}

// expireStripeSession best-effort expires a live session, classifying the
// failure: a COMPLETED session returns payment.ErrSessionCompleted (caller
// must NOT mint), an already-expired session is success.
func (a *hostedInvoiceStripeAdapter) expireStripeSession(ctx context.Context, sc *stripe.Client, sessionID string) error {
	_, err := sc.V1CheckoutSessions.Expire(ctx, sessionID, &stripe.CheckoutSessionExpireParams{})
	if err == nil {
		return nil
	}
	// Classify against the session's live status — the expire error text is
	// not a contract.
	sess, rErr := sc.V1CheckoutSessions.Retrieve(ctx, sessionID, &stripe.CheckoutSessionRetrieveParams{})
	if rErr != nil {
		return err // original failure; caller treats as network residual
	}
	switch sess.Status {
	case stripe.CheckoutSessionStatusComplete:
		return payment.ErrSessionCompleted
	case stripe.CheckoutSessionStatusExpired:
		return nil // already expired = the outcome we wanted
	}
	return err
}

// subscriptionCheckerAdapter implements customer.SubscriptionChecker over the
// subscription store + pricing service (ADR-067 archive/currency guards).
// Non-terminal = active or trialing; an active sub with a scheduled cancel
// still bills until the boundary and is deliberately included.
type subscriptionCheckerAdapter struct {
	subs  *subscription.PostgresStore
	plans *pricing.Service
}

func (a *subscriptionCheckerAdapter) NonTerminalForCustomer(ctx context.Context, tenantID, customerID string, includeDraft bool) ([]customer.NonTerminalSubscription, error) {
	var out []customer.NonTerminalSubscription
	statuses := []string{string(domain.SubscriptionActive), string(domain.SubscriptionTrialing)}
	if includeDraft {
		statuses = append(statuses, string(domain.SubscriptionDraft))
	}
	for _, status := range statuses {
		subs, _, err := a.subs.List(ctx, subscription.ListFilter{
			TenantID:   tenantID,
			CustomerID: customerID,
			Status:     status,
			Limit:      100,
		})
		if err != nil {
			return nil, err
		}
		for _, sub := range subs {
			ref := customer.NonTerminalSubscription{ID: sub.ID, Status: string(sub.Status)}
			seen := map[string]struct{}{}
			for _, it := range sub.Items {
				plan, perr := a.plans.GetPlan(ctx, tenantID, it.PlanID)
				if perr != nil {
					// Fail loud: the currency guard cannot decide safely on a
					// half-resolved plan set.
					return nil, fmt.Errorf("resolve plan %s for guard: %w", it.PlanID, perr)
				}
				if plan.Currency == "" {
					continue
				}
				if _, dup := seen[plan.Currency]; dup {
					continue
				}
				seen[plan.Currency] = struct{}{}
				ref.PlanCurrencies = append(ref.PlanCurrencies, plan.Currency)
			}
			out = append(out, ref)
		}
	}
	return out, nil
}

// memberUserDirectoryAdapter bridges dashmembers.UserDirectory onto the
// user package — service for account creation (which validates the
// password and hashes it), store for lookups/attach. Keeps dashmembers
// decoupled from internal/user.
type memberUserDirectoryAdapter struct {
	svc   *user.Service
	store *user.PostgresStore
}

func (a *memberUserDirectoryAdapter) GetByEmail(ctx context.Context, email string) (domain.User, error) {
	return a.store.GetByEmail(ctx, email)
}

func (a *memberUserDirectoryAdapter) TenantsForUser(ctx context.Context, userID string) ([]domain.UserTenant, error) {
	return a.store.TenantsForUser(ctx, userID)
}

func (a *memberUserDirectoryAdapter) CreateUser(ctx context.Context, email, plaintext, tenantID, role string) (domain.User, error) {
	return a.svc.CreateUser(ctx, email, plaintext, tenantID, role)
}

func (a *memberUserDirectoryAdapter) AttachTenant(ctx context.Context, userID, tenantID, role string) error {
	return a.store.AttachTenant(ctx, userID, tenantID, role)
}

// memberTenantNamerAdapter resolves the workspace display name for
// invite emails and the accept page.
type memberTenantNamerAdapter struct {
	svc *tenant.Service
}

func (a *memberTenantNamerAdapter) GetTenantName(ctx context.Context, tenantID string) (string, error) {
	t, err := a.svc.Get(ctx, tenantID)
	if err != nil {
		return "", err
	}
	return t.Name, nil
}
