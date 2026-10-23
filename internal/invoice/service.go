package invoice

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/tax"
)

// InvoiceNumberer allocates the next invoice number for a tenant.
// Atomicity and uniqueness are the numberer's responsibility.
type InvoiceNumberer interface {
	NextInvoiceNumber(ctx context.Context, tenantID string) (string, error)
	// NextInvoiceNumberTx is the in-transaction variant used by the
	// atomic AddItem-with-proration flow — allocate the number and
	// insert the invoice in one tx so a rollback frees the number.
	NextInvoiceNumberTx(ctx context.Context, tx *sql.Tx, tenantID string) (string, error)
}

// TaxCommitter finalizes an upstream tax calculation into a tax_transaction
// (Stripe Tax) at invoice finalize time. Optional: when nil, finalize proceeds
// without a tax commit — fine for manual/none providers or when the invoice
// has no CalculationID. Failures here do NOT block finalize; they only get
// logged, because invoice finalize must remain idempotent and a transient
// Stripe error shouldn't leave the invoice stuck in draft.
type TaxCommitter interface {
	CommitTax(ctx context.Context, tenantID, invoiceID, calculationID string) error
	// CheckTaxCalculationUsable answers, BEFORE the finalize transition:
	// can this invoice's build-time calculation still be committed? Non-nil
	// means it is expired at the provider and can never commit — the one
	// commit failure the post-finalize path must NOT swallow, because the
	// orphan reconciler is blocked by the same age rule and the invoice
	// would end up collected with tax printed but never reported. All
	// internal uncertainty (store unwired, row missing, lookup error)
	// returns nil: only a PROVEN expiry may block a finalize.
	CheckTaxCalculationUsable(ctx context.Context, tenantID, invoiceID, calculationID string) error
}

// TaxReverser issues a reversal of the invoice's committed tax transaction
// when the invoice is voided. Without this call, finalized-but-unpaid
// invoices that get voided would leave the tax transaction committed
// upstream — over-reporting tax collected to the authority. Optional —
// when unset, Void proceeds without the upstream reversal (suitable for
// none/manual providers and for narrow tests). Failures here log but
// do not block the void.
type TaxReverser interface {
	ReverseTax(ctx context.Context, tenantID string, req tax.ReversalRequest) (*tax.ReversalResult, error)
}

// CreditNoteTotaler reports the sum of ISSUED credit-note totals against an
// invoice — the credit notes that reversed (or will reverse, under their own
// reference) their slice of its tax. OPTIONAL — when wired, reverseInvoiceTax
// reverses exactly the gross no credit note covers (total − issued) on a void,
// which is correct even when customer credit was applied to the invoice
// (applied credit reduces amount_due without reversing tax). A DRAFT credit
// note is not counted: it has reversed nothing, and once its invoice is voided
// it is voided too without ever issuing. Unwired (narrow tests) → falls back to
// the amount_paid+amount_due proxy. Satisfied by *creditnote.Service.
type CreditNoteTotaler interface {
	IssuedCreditedCents(ctx context.Context, tenantID, invoiceID string) (int64, error)
	// UnreliefedClawbackCents sums clawback relief this invoice was owed but
	// never received (issue_pending drafts that were voided before issuing).
	// Non-zero means amount_due is stale-HIGH — bad-debt recovery refuses,
	// because charging would over-collect by exactly that much.
	UnreliefedClawbackCents(ctx context.Context, tenantID, invoiceID string) (int64, error)
}

// TaxRetrier is the narrow view into tax recompute + persistence the
// retry-tax endpoint depends on. Satisfied by billing.Engine in
// production — the contract lives next to the handler that calls it.
type TaxRetrier interface {
	RetryTaxForInvoice(ctx context.Context, tenantID, invoiceID string) (domain.Invoice, error)
	// ComputeTaxForInvoice computes tax for a draft invoice regardless of
	// tax_status. Finalize calls this for manual / operator-composed
	// invoices, which accrue line items incrementally and have no tax until
	// finalize (Stripe-parity: tax is calculated when the invoice finalizes).
	ComputeTaxForInvoice(ctx context.Context, tenantID, invoiceID string) (domain.Invoice, error)
}

// PaymentMethodReader is the narrow lookup the attention classifier
// uses to decide between no_payment_method (operator-actionable) and
// awaiting_payment (engine race window). Satisfied by the api package's
// composite payment-setup store — kept as a local interface so this
// package doesn't import internal/payment.
//
// Batch-shaped so a list page reads every row's customer in one go (issue
// #1142); a single invoice is a batch of one. A customer is ready when its
// payment setup is ready (an active default payment method) AND it has a
// Stripe customer to charge. Customers not ready are absent or false.
type PaymentMethodReader interface {
	PaymentReadyCustomers(ctx context.Context, tenantID string, customerIDs []string) (map[string]bool, error)
}

// StripeChecker reports whether a tenant has Stripe credentials
// connected for a given mode. Used by the attention classifier to
// distinguish two states that previously rendered identically on
// invoices stuck at tax_status='pending' with code
// 'provider_not_configured':
//   - Stripe NOT connected → operator action required.
//   - Stripe IS now connected (just connected, scheduler hasn't
//     ticked yet) → calculation will retry shortly; offer Retry now.
//
// Implemented by *payment.StripeClients via HasFor.
type StripeChecker interface {
	HasFor(ctx context.Context, tenantID string, livemode bool) bool
}

// CustomerReader reads the full customer record. Two consumers:
//   - Create stamps invoice.is_simulated authoritatively at write time from
//     the customer's test-clock pin — the same direct field check the engine
//     uses for cycle invoices (sub.TestClockID != ""), applied to the customer
//     for manual one-off invoices. This is the WRITE-time capture, NOT the
//     read-time snapshot heuristic ADR-030 bans: we record whether the
//     customer was pinned at the instant the invoice was born, then persist
//     it. (We can't use clock.IsSimulated(ctx) here — bindForCreate binds ctx
//     to the resolver's effective-now even for UNPINNED customers, so "ctx is
//     bound" doesn't mean "on a test clock".)
//   - attachAttention reads the customer's email presence to pick the honest
//     no_payment_method banner variant (emailed-a-link vs no-address).
//
// Satisfied by *customer.PostgresStore. GetMany is the attention read: one
// query for a whole list page (issue #1142); ids with no row are absent.
type CustomerReader interface {
	Get(ctx context.Context, tenantID, customerID string) (domain.Customer, error)
	GetMany(ctx context.Context, tenantID string, ids []string) ([]domain.Customer, error)
}

// TenantSettingsReader reads tenant settings so a manual invoice with no
// explicit net term uses the tenant's configured terms, 0 included — the same
// resolution as the billing engine's invoices. Optional; nil (narrow tests)
// uses 30.
type TenantSettingsReader interface {
	Get(ctx context.Context, tenantID string) (domain.TenantSettings, error)
}

// CreditReverser restores customer-balance credits that were applied to an
// invoice when that invoice is voided. Void calls the in-tx variants so both
// ledger legs commit in the SAME transaction as the status flip — a void
// either lands all its ledger effects or doesn't happen. Optional: when nil
// (narrow tests, none-credit tenants) Void proceeds without them. Satisfied
// by *credit.Service. Kept as a local interface so this package doesn't
// import internal/credit.
type CreditReverser interface {
	ReverseForInvoiceTx(ctx context.Context, tx *sql.Tx, tenantID, customerID, invoiceID, invoiceNumber string) (int64, error)
	// RetireCommitGrantForInvoiceTx retires the remaining balance of the
	// commit grant this invoice funded, if any (ADR-078 D3: voiding a
	// commit funding invoice kills the unfunded credits; consumed stays
	// consumed). Clean no-op for non-commit invoices and already-retired
	// grants.
	RetireCommitGrantForInvoiceTx(ctx context.Context, tx *sql.Tx, tenantID, invoiceID string) (int64, error)
}

type Service struct {
	store          Store
	clock          clock.Clock
	resolver       clock.Resolver
	numberer       InvoiceNumberer
	taxCommitter   TaxCommitter
	taxReverser    TaxReverser
	creditNotes    CreditNoteTotaler
	creditReverser CreditReverser
	taxRetrier     TaxRetrier
	paymentMethods PaymentMethodReader
	stripeChecker  StripeChecker
	customerReader CustomerReader
	dunningReader  DunningRunReader
	subPauseReader SubscriptionPauseReader
	commitGrants   CommitGrantReader
	paymentPages   PaymentPageReader
	cardRefusals   CardRefusalReader
	disputes       DisputeReader
	creditApplier  CreditApplier
	settings       TenantSettingsReader
	currencyFacts  CustomerCurrencyFacts
	audit          AuditLogger
	events         domain.EventDispatcher
	// collectionStopper and dunningResolver are AfterVoid's Stripe and
	// dunning legs; collectionStopper is also MarkUncollectible's Checkout
	// expiry. Only a test leaves them unwired.
	collectionStopper PaymentCanceler
	dunningResolver   DunningResolver
	// setupLinkEmailOff is true when this server cannot email a setup link
	// (SMTP_HOST or PAYMENT_UPDATE_URL unset). Read only by the
	// no-payment-method and recovery-ended notices, which must not say a
	// setup link is emailed when nothing can be sent. Zero value: sending
	// works, which is what every production boot wires explicitly.
	setupLinkEmailOff bool
}

// SetSetupLinkEmailConfigured records whether this server can email a setup
// link, so the no-payment-method notice says "a setup link is emailed" only
// when one can be. Wired at boot from both things the sender needs: SMTP and
// PAYMENT_UPDATE_URL.
func (s *Service) SetSetupLinkEmailConfigured(configured bool) { s.setupLinkEmailOff = !configured }

// SetCollectionStopper wires the Stripe leg every committed void runs
// (AfterVoid): expire the invoice's live Checkout sessions, then cancel its
// PaymentIntent. A committed write-off runs the session expiry alone.
// Satisfied by *payment.Stripe.
func (s *Service) SetCollectionStopper(p PaymentCanceler) { s.collectionStopper = p }

// SetDunningResolver wires the dunning leg every committed void runs
// (AfterVoid): close the invoice's active run as invoice_voided. Satisfied
// by *dunning.Service.
func (s *Service) SetDunningResolver(d DunningResolver) { s.dunningResolver = d }

// AuditLogger is the narrow audit-write interface the service uses to
// record state-changing operations that are reachable from multiple
// entry points (HTTP handler + dunning adapter MarkUncollectible
// final-action + the ResolveRun(invoice_not_collectible) operator
// flow). Same pattern as subscription.Service — single canonical
// audit row regardless of which path triggered the transition.
type AuditLogger interface {
	Log(ctx context.Context, tenantID, action, resourceType, resourceID, resourceLabel string, metadata map[string]any) error
	// LogInTx is the ADR-090 in-tx emission: Void uses it inside
	// UpdateStatusWithReversal so the status flip, the credit reversal,
	// the commit-grant retirement, and the audit row share one fate.
	LogInTx(ctx context.Context, tx *sql.Tx, e audit.Entry) error
}

// SetTenantSettingsReader wires the tenant-settings reader used to default a
// manual invoice's net payment terms when the caller omits them. Optional.
func (s *Service) SetTenantSettingsReader(r TenantSettingsReader) { s.settings = r }

// SetAuditLogger wires the audit logger.
func (s *Service) SetAuditLogger(l AuditLogger) { s.audit = l }

// SetEventDispatcher wires outbound webhook dispatch for state-
// transitioning service methods (MarkUncollectible, RecordPayment).
// Optional — unset paths skip dispatch.
func (s *Service) SetEventDispatcher(d domain.EventDispatcher) { s.events = d }

// dispatchEvent enqueues an outbound webhook event and LOGS on failure rather
// than silently dropping it. The dispatcher persists durably (ADR-040 outbox)
// once the enqueue commits, so a non-nil error here means the enqueue INSERT
// itself failed (a transient DB hiccup) and the event is lost — make that
// visible (no-silent-fallbacks) instead of the prior `_ =` swallow. Atomicity
// with the state-change (DispatchTx) is the deferred follow-up; this only closes
// the silent-error vector.
func (s *Service) dispatchEvent(ctx context.Context, tenantID, eventType string, payload map[string]any) {
	if s.events == nil {
		return
	}
	if err := s.events.Dispatch(ctx, tenantID, eventType, payload); err != nil {
		slog.ErrorContext(ctx, "failed to enqueue webhook event",
			"event_type", eventType, "tenant_id", tenantID, "error", err)
	}
}

// CustomerCurrencyFacts reads the stored facts a manual invoice's currency
// must agree with (ADR-100, ADR-129): the customer's billing-profile currency
// ("" when there is no profile or it sets none) and the plan currencies of the
// customer's non-terminal subscriptions. Plain strings, so this package
// imports neither the customer nor the subscription domain; the adapter lives
// in internal/api. A read error is returned, never read as "no fact".
type CustomerCurrencyFacts interface {
	CustomerCurrencyFacts(ctx context.Context, tenantID, customerID string) (profileCurrency string, planCurrencies []string, err error)
}

// SetCustomerCurrencyFacts wires the currency facts Create checks a manual
// invoice against. Production wires it at boot; an unwired reader (narrow
// unit tests) skips the check.
func (s *Service) SetCustomerCurrencyFacts(f CustomerCurrencyFacts) { s.currencyFacts = f }

// ErrCurrencyUnavailable is a Create whose currency could not be read. It is
// retryable; the handler answers 503 rather than guess a currency.
var ErrCurrencyUnavailable = errors.New("could not read the customer's billing currency")

// resolveCurrency is a manual invoice's currency (ADR-100, ADR-129). An
// explicit one must agree with the customer's stored facts: the billing
// profile's currency when set, and every non-terminal subscription's plan
// currency. An empty one takes the profile's, then the subscriptions' plan
// currency, then the workspace default, in the order the engine stamps a
// cycle invoice (Engine.invoiceCurrency); the result is then held to the same
// facts. A customer with neither fact may be invoiced in any allowed
// currency. A credit pays only invoices in its own currency, so an invoice in
// a currency the customer is not billed in would sell credit (a commit line)
// or take a payment that none of the customer's other money can meet.
func (s *Service) resolveCurrency(ctx context.Context, tenantID, customerID, requested, workspaceDefault string) (string, error) {
	var (
		profile string
		plans   []string
	)
	if s.currencyFacts != nil {
		p, ps, err := s.currencyFacts.CustomerCurrencyFacts(ctx, tenantID, customerID)
		if err != nil {
			return "", fmt.Errorf("%w: %v", ErrCurrencyUnavailable, err)
		}
		profile = strings.ToUpper(strings.TrimSpace(p))
		seen := map[string]bool{}
		for _, c := range ps {
			c = strings.ToUpper(strings.TrimSpace(c))
			if c != "" && !seen[c] {
				seen[c] = true
				plans = append(plans, c)
			}
		}
		sort.Strings(plans)
	}
	currency := requested
	switch {
	case currency != "":
	case profile != "":
		currency = profile
	case len(plans) > 0:
		currency = plans[0]
	default:
		currency = strings.ToUpper(strings.TrimSpace(workspaceDefault))
	}
	if err := domain.ValidateCurrency(currency); err != nil {
		return "", err
	}
	conflict := func(fact, source string) error {
		return errs.InvalidState(fmt.Sprintf(
			"this customer is billed in %s (%s); a manual invoice uses the customer's currency, so it cannot be in %s",
			fact, source, currency)).WithCode("customer_currency_conflict")
	}
	if profile != "" && profile != currency {
		return "", conflict(profile, "billing profile")
	}
	for _, p := range plans {
		if p != currency {
			return "", conflict(p, "subscription plan")
		}
	}
	return currency, nil
}

func NewService(store Store, clk clock.Clock, numberer InvoiceNumberer) *Service {
	if clk == nil {
		clk = clock.Real()
	}
	return &Service{store: store, clock: clk, numberer: numberer}
}

// SetTaxCommitter wires the upstream tax-transaction committer. Called from
// router.go with the billing engine so finalize can commit Stripe tax calcs.
func (s *Service) SetTaxCommitter(tc TaxCommitter) {
	s.taxCommitter = tc
}

// SetTaxReverser wires the upstream tax-transaction reverser. Called from
// router.go with the billing engine so Void on a finalized-but-unpaid
// invoice can reverse the tax_transaction committed at finalize time.
// Without this, voided invoices leave their tax recorded as collected
// with the authority — over-reporting tax revenue.
func (s *Service) SetTaxReverser(tr TaxReverser) {
	s.taxReverser = tr
}

func (s *Service) SetCreditNoteTotaler(c CreditNoteTotaler) {
	s.creditNotes = c
}

// SetCreditReverser wires the consumed-credit reverser used by Void to restore
// applied customer credits atomically with the status flip. Called from
// router.go with the credit service. Without it, Void still voids but reverses
// no credits (correct for tenants that never apply customer credit).
func (s *Service) SetCreditReverser(r CreditReverser) {
	s.creditReverser = r
}

// SetResolver wires the unified clock.Resolver used to bind
// effective-now at invoice service entry points. Once bound on ctx
// via clock.BindEffectiveNow, every downstream s.clock.Now(ctx)
// (including in the postgres store's INSERT-time stamps) reads
// frozen_time on clock-pinned entities. Optional: nil leaves binding
// off and every callsite reads wall-clock — the test-friendly default.
//
// Replaces the per-service ClockResolver pattern shipped during the
// post-ADR-029 patches; matches Stripe's "no semantic change"
// guarantee for billing-engine resources.
func (s *Service) SetResolver(r clock.Resolver) {
	s.resolver = r
}

// bindForCreate binds effective-now at invoice-create time. Prefers
// the subscription pin when set (more specific), else falls back to
// the customer pin. Returns ctx unchanged on resolver error or
// missing ids — wall-clock fallback at every downstream callsite.
func (s *Service) bindForCreate(ctx context.Context, tenantID string, input CreateInput) context.Context {
	pin := clock.Pin{TenantID: tenantID}
	switch {
	case input.SubscriptionID != "":
		pin.SubscriptionID = input.SubscriptionID
	case input.CustomerID != "":
		pin.CustomerID = input.CustomerID
	default:
		return ctx
	}
	bound, _ := clock.BindEffectiveNow(ctx, s.resolver, pin)
	return bound
}

// bindForInvoice binds effective-now from an invoice id. Used by
// every per-invoice mutation entry point (Finalize, Void,
// RecordPayment, RetryTax, etc.) so downstream stamps
// inherit simulated time.
func (s *Service) bindForInvoice(ctx context.Context, tenantID, invoiceID string) context.Context {
	bound, _ := clock.BindEffectiveNow(ctx, s.resolver, clock.Pin{TenantID: tenantID, InvoiceID: invoiceID})
	return bound
}

// AuditCtx binds the invoice's simulated-time context (its test clock + that
// clock's frozen instant) for a HANDLER-level audit emission — collect, refund,
// send, rotate-token, retry-tax — so those rows land on the sim axis
// (audit_log.sim_effective_at / test_clock_id).
//
// Without it the money-movement rows would be the ONE class of clock-driven
// evidence missing from the clock filter: the service binds a ctx the handler
// never sees, so "operator collected payment on INV-1042 during the simulation"
// would be invisible to a clock-scoped forensic view — and after ADR-086
// teardown deletes the invoice, invisible everywhere.
//
// The gate is the invoice's OWN is_simulated flag: exact, and free on the
// wall-clock path (no resolver round-trip for the 99% case). domain.Invoice
// carries no test_clock_id — the pin lives on the customer/sub (ADR-027) — so
// the clock id itself still comes from one pin resolution, never guessed.
func (s *Service) AuditCtx(ctx context.Context, inv domain.Invoice) context.Context {
	if !inv.IsSimulated || s.resolver == nil {
		return ctx
	}
	if _, ok := clock.SimOf(ctx); ok {
		return ctx
	}
	return s.bindForInvoice(ctx, inv.TenantID, inv.ID)
}

// SetTaxRetrier wires the orchestrator behind the retry-tax endpoint.
// Production passes billing.Engine; tests can pass any implementation.
func (s *Service) SetTaxRetrier(r TaxRetrier) {
	s.taxRetrier = r
}

// SetPaymentMethodReader wires the customer payment-setup lookup used
// by the attention classifier to surface no_payment_method distinctly
// from generic awaiting_payment. Optional — when nil, the classifier
// falls back to the awaiting_payment branch (less specific but still
// correct for healthy/transient cases).
func (s *Service) SetPaymentMethodReader(r PaymentMethodReader) {
	s.paymentMethods = r
}

// DunningRunReader is the narrow dunning lookup the attention classifier
// uses to detect an ESCALATED run on a no-PM invoice (2026-07-22
// payment-surfacing audit, P1-4): once automatic recovery has exhausted
// and the policy's final action fired, the banner must stop implying
// recovery is still running. Satisfied by *dunning.PostgresStore.
// No run is the common case and simply means "not escalated". One read
// covers a list page (issue #1142); invoices without a run are absent.
type DunningRunReader interface {
	ListRunsByInvoices(ctx context.Context, tenantID string, invoiceIDs []string) ([]domain.InvoiceDunningRun, error)
}

// SetDunningRunReader wires the dunning-run lookup (see DunningRunReader).
func (s *Service) SetDunningRunReader(r DunningRunReader) {
	s.dunningReader = r
}

// SubscriptionPauseReader is the narrow subscription lookup the tax-retry
// auto-finalize gate uses to honour `pause_collection`, and that Create uses
// to confirm an operator-supplied subscription id is the caller's. The read
// is tenant- and mode-scoped. Satisfied by *subscription.PostgresStore.
// PauseCollections reads the pause of many subscriptions at once (nil = no
// pause; an id with no row is absent), so a list page's banners cost one read
// (issue #1142).
type SubscriptionPauseReader interface {
	Get(ctx context.Context, tenantID, id string) (domain.Subscription, error)
	PauseCollections(ctx context.Context, tenantID string, ids []string) (map[string]*domain.PauseCollection, error)
}

// SetSubscriptionPauseReader wires the pause lookup (see
// SubscriptionPauseReader). Optional: when nil, auto-finalize behaves as it
// did before the gate existed — the conservative direction is to finalize,
// since a stuck draft is the state operators complain about.
func (s *Service) SetSubscriptionPauseReader(r SubscriptionPauseReader) {
	s.subPauseReader = r
}

// CommitGrantReader is the narrow prepaid-commit lookup the attention
// classifier uses to report credit that is live against an invoice nobody
// paid (ADR-078 D3's named fast-follow). Satisfied by *credit.PostgresStore.
//
// Consumer-defined here rather than imported from `credit`: the arch boundary
// (internal/arch/boundaries_test.go) allows `invoice` only {audit, auth,
// payment, tax}, and this is a read for a banner — nowhere near enough reason
// to open a new edge between two money domains.
//
// Returns the two figures separately because only one of them is recoverable:
// drawable is what void still cancels, consumed is gone for good. `now` is the
// caller's resolver-bound instant, not wall-clock, so a clock-pinned invoice
// judges its grant's expiry on the same axis the drawdown does.
type CommitGrantReader interface {
	CommitGrantExposure(ctx context.Context, tenantID, invoiceID string, now time.Time) (drawableCents, consumedCents int64, err error)
}

// SetCommitGrantReader wires the prepaid-commit lookup (see
// CommitGrantReader). Optional: when nil the exposure fields stay zero and no
// exposure is claimed — the same safe default as an unwired dunning reader.
func (s *Service) SetCommitGrantReader(r CommitGrantReader) {
	s.commitGrants = r
}

// PaymentPageReader reports whether an invoice has a hosted payment page that
// may still take the customer's money (domain.HostedCheckoutLiveSQL, the rule
// the charge claims refuse on). Satisfied by *PostgresStore. Batch-shaped so
// a list page costs one read (issue #1142); invoices without a live page are
// absent or false.
type PaymentPageReader interface {
	PaymentPagesLive(ctx context.Context, tenantID string, invoiceIDs []string) (map[string]bool, error)
}

// SetPaymentPageReader wires the payment-page lookup for the attention banner.
// Optional: when nil no page is reported open, and the banner keeps offering
// Retry, which a live page would refuse with a clear 422.
func (s *Service) SetPaymentPageReader(r PaymentPageReader) {
	s.paymentPages = r
}

// CardRefusalReader reports, for a page of invoices, the ones whose next
// dunning retry would skip the customer's current default card: invoice id →
// Stripe's decline code, a hard decline (#966) or authentication_required.
// Satisfied by *PostgresStore (RetryBlockingDeclinesOnDefaultCards), the same
// rule the dunning retry skips the card on.
type CardRefusalReader interface {
	RetryBlockingDeclinesOnDefaultCards(ctx context.Context, tenantID string, invoiceIDs []string) (map[string]string, error)
}

// SetCardRefusalReader wires the refused-card lookup for the attention
// banner. Optional: when nil a refused card reads as an ordinary decline,
// whose banner still offers Update payment method first.
func (s *Service) SetCardRefusalReader(r CardRefusalReader) {
	s.cardRefusals = r
}

// DisputeReader reads the card disputes Stripe reported against invoices
// (#979). Satisfied by *PostgresStore, which also records them from the
// charge.dispute.* webhooks.
type DisputeReader interface {
	ListDisputesByInvoice(ctx context.Context, tenantID, invoiceID string) ([]domain.InvoiceDispute, error)
	LostDisputesByInvoices(ctx context.Context, tenantID string, invoiceIDs []string) (map[string]domain.InvoiceDispute, error)
}

// SetDisputeReader wires the dispute records behind the dispute_lost notice,
// the timeline's dispute rows and GET /v1/invoices/{id}/disputes. Optional:
// when nil no dispute is reported.
func (s *Service) SetDisputeReader(r DisputeReader) {
	s.disputes = r
}

// ListDisputes returns the invoice's card disputes, oldest first. Empty when
// no dispute reader is wired.
func (s *Service) ListDisputes(ctx context.Context, tenantID, invoiceID string) ([]domain.InvoiceDispute, error) {
	if s.disputes == nil {
		return []domain.InvoiceDispute{}, nil
	}
	return s.disputes.ListDisputesByInvoice(ctx, tenantID, invoiceID)
}

// ListInvoiceDisputes is ListDisputes for one invoice the caller names:
// errs.ErrNotFound when the invoice does not exist in this tenant and mode.
func (s *Service) ListInvoiceDisputes(ctx context.Context, tenantID, invoiceID string) ([]domain.InvoiceDispute, error) {
	if _, err := s.store.Get(ctx, tenantID, invoiceID); err != nil {
		return nil, err
	}
	return s.ListDisputes(ctx, tenantID, invoiceID)
}

// collectionPaused reports whether the invoice's subscription has
// pause_collection in force now. An unwired reader, no subscription id or a subscription
// that no longer exists answers false (nothing to pause). Any other read error
// is returned: the auto-finalize gate must not issue and charge an invoice on a
// guess that collection is not paused.
func (s *Service) collectionPaused(ctx context.Context, tenantID, subscriptionID string) (bool, error) {
	if subscriptionID == "" {
		return false, nil
	}
	paused, err := s.collectionPausedMany(ctx, tenantID, []string{subscriptionID})
	return paused[subscriptionID], err
}

// collectionPausedMany is collectionPaused for several subscriptions in one
// read, keyed by id. Unwired reader → empty map; an id with no row is absent
// (false). On a read error the map is empty and the error returned.
func (s *Service) collectionPausedMany(ctx context.Context, tenantID string, subscriptionIDs []string) (map[string]bool, error) {
	out := make(map[string]bool, len(subscriptionIDs))
	if s.subPauseReader == nil || len(subscriptionIDs) == 0 {
		return out, nil
	}
	pauses, err := s.subPauseReader.PauseCollections(ctx, tenantID, subscriptionIDs)
	if err != nil {
		return out, fmt.Errorf("read subscription pause state: %w", err)
	}
	// In force now, not merely set: a timed pause past resumes_at can stay on
	// the row until the sub's paused periods are closed (see
	// domain.PauseCollection.InForceAt).
	now := s.clock.Now(ctx)
	for id, pc := range pauses {
		out[id] = pc.InForceAt(now)
	}
	return out, nil
}

// SetCustomerReader wires the customer lookup used to (a) stamp is_simulated
// from the customer's test-clock pin at Create and (b) pick the no-PM banner
// variant from email presence at read. Optional — when nil (narrow unit
// tests), is_simulated defaults false (correct for any non-clock-pinned
// invoice) and the no-PM banner falls back to the no-email variant; production
// always wires it.
func (s *Service) SetCustomerReader(r CustomerReader) {
	s.customerReader = r
}

// customerOnTestClock reports whether the customer is pinned to a test clock —
// the authoritative write-time signal for invoice.is_simulated on manual
// invoices (mirrors the engine's sub.TestClockID check for cycle invoices).
// Unwired reader → false (narrow unit tests; prod always wires it). A lookup
// error is returned: is_simulated is durable, and a simulated invoice stored
// as real would count as revenue and go through the real reconcilers.
func (s *Service) customerOnTestClock(ctx context.Context, tenantID, customerID string) (bool, error) {
	if s.customerReader == nil || customerID == "" {
		return false, nil
	}
	cust, err := s.customerReader.Get(ctx, tenantID, customerID)
	if errors.Is(err, errs.ErrNotFound) {
		// This read runs in the caller's tenant and mode. The foreign key on
		// invoices.customer_id is on the id alone and foreign-key checks do
		// not apply row-level security, so the insert would accept another
		// tenant's or the other mode's customer.
		return false, errs.NotFound("customer")
	}
	if err != nil {
		return false, fmt.Errorf("read customer test-clock pin: %w", err)
	}
	return cust.TestClockID != "", nil
}

// requireSubscription confirms an operator-supplied subscription id exists in
// the caller's tenant and mode, and that it belongs to this customer. Same
// reason as the customer read above: the foreign key on
// invoices.subscription_id is on the id alone, so it says nothing about
// tenant, mode or owner. Dunning, the collection-paused gate, the test clock
// and the cycle slot all act on the invoice's subscription, so another
// customer's id would make this invoice act on that customer's account.
// Both columns are write-once, so checking here at create closes it.
// Unwired reader (narrow unit tests) skips the read.
func (s *Service) requireSubscription(ctx context.Context, tenantID, customerID, subscriptionID string) error {
	if s.subPauseReader == nil || subscriptionID == "" {
		return nil
	}
	sub, err := s.subPauseReader.Get(ctx, tenantID, subscriptionID)
	if err != nil {
		if errors.Is(err, errs.ErrNotFound) {
			return errs.NotFound("subscription")
		}
		return fmt.Errorf("get subscription: %w", err)
	}
	if sub.CustomerID != customerID {
		return errs.Invalid("subscription_id", "subscription does not belong to the requested customer")
	}
	return nil
}

// SetStripeChecker wires the per-tenant Stripe-connected probe used
// by the attention classifier to distinguish "Stripe not connected"
// from "Stripe just connected, calculation will retry shortly" on
// tax_status='pending' invoices. Optional — when nil, the classifier
// falls through the existing "Stripe isn't connected" copy (the
// pre-fix behaviour) which is safe-but-less-helpful for the
// gap-window case.
func (s *Service) SetStripeChecker(c StripeChecker) {
	s.stripeChecker = c
}

type CreateInput struct {
	CustomerID         string    `json:"customer_id"`
	SubscriptionID     string    `json:"subscription_id"`
	Currency           string    `json:"currency"`
	BillingPeriodStart time.Time `json:"billing_period_start"`
	BillingPeriodEnd   time.Time `json:"billing_period_end"`
	// NetPaymentTermDays is a pointer so the service can distinguish
	// "omitted" (nil → fall back to the tenant's configured net terms, then
	// 30) from an explicit 0 ("Due on receipt" — a valid choice the composer
	// offers). A plain int conflated the two, silently turning Due-on-receipt
	// into Net 30.
	NetPaymentTermDays *int   `json:"net_payment_term_days,omitempty"`
	Memo               string `json:"memo,omitempty"`
	// LineItems, when present, are created atomically with the invoice
	// header in a single transaction. The operator composer sends them
	// this way so a network failure mid-compose can't leave a draft with
	// a partial set of lines. Omitted (nil) keeps the bare-header create
	// for callers that add lines incrementally afterwards.
	LineItems []AddLineItemInput `json:"line_items,omitempty"`
}

func (s *Service) Create(ctx context.Context, tenantID string, input CreateInput) (domain.Invoice, error) {
	if input.CustomerID == "" {
		return domain.Invoice{}, errs.Required("customer_id")
	}
	// Both references are read in the caller's tenant and mode before
	// anything is written, including the invoice number: a refused request
	// must not use one up.
	onClock, err := s.customerOnTestClock(ctx, tenantID, input.CustomerID)
	if err != nil {
		return domain.Invoice{}, err
	}
	if err := s.requireSubscription(ctx, tenantID, input.CustomerID, input.SubscriptionID); err != nil {
		return domain.Invoice{}, err
	}
	// SubscriptionID is OPTIONAL: cycle invoices carry a subscription, one-off
	// invoices (operator composer, ad-hoc charges) do not. The DB column is
	// nullable as of migration 0060; the cycle-idempotency partial unique
	// index ignores NULLs so two one-off invoices can coexist for the same
	// (customer, period) without colliding.

	// Uppercase ISO-4217 — the canonical case across the system: the tenant
	// default currency is "USD" (tenant.DefaultSettings) and the analytics /
	// dunning revenue-by-currency queries filter with a case-SENSITIVE
	// `currency = $1` against it, so a stored "usd" would silently drop out
	// of revenue/at-risk sums. The store insert normalizes again as the
	// single chokepoint for all callers (incl. the cycle engine's "usd"
	// fallback); doing it here too keeps the in-memory struct consistent.
	// An explicit currency is allowlist-validated before anything is read:
	// it catches typos and the zero-decimal class (ADR-100) before an
	// unchargeable or 100x-mischarged label reaches Stripe.
	currency := strings.ToUpper(strings.TrimSpace(input.Currency))
	if currency != "" {
		if err := domain.ValidateCurrency(currency); err != nil {
			return domain.Invoice{}, err
		}
	}

	// Read tenant settings once, for the default Net terms, the billing
	// timezone and the workspace currency. A read failure fails the create
	// rather than guessing Net 30 (P24), a zone or a currency, and happens
	// before an invoice number is spent. No reader wired (tests) means Net 30,
	// UTC and USD, as in the engine.
	ts := domain.TenantSettings{DefaultCurrency: "USD"}
	if s.settings != nil {
		ts, err = s.settings.Get(ctx, tenantID)
		if err != nil {
			// With no currency in the request this read may hold the one
			// to use (the workspace default), so it is the documented,
			// retryable 503 currency_unavailable.
			if currency == "" {
				return domain.Invoice{}, fmt.Errorf("%w: read tenant settings: %v", ErrCurrencyUnavailable, err)
			}
			return domain.Invoice{}, fmt.Errorf("read tenant settings: %w", err)
		}
	}
	// The customer's currency (ADR-100, ADR-129): an explicit one must agree
	// with the stored facts, an empty one is taken from them.
	currency, err = s.resolveCurrency(ctx, tenantID, input.CustomerID, currency, ts.DefaultCurrency)
	if err != nil {
		return domain.Invoice{}, err
	}
	// The org billing timezone is stamped on the invoice at create, as every
	// engine writer does (ADR-077). Without it the PDF, hosted page and
	// dunning email render Issued/Due/Paid dates in UTC while the period and
	// the dashboard use the tenant zone, so the two disagree near midnight.
	billingLoc := domain.LoadLocationOrUTC(ts.Timezone)

	// Resolve net payment terms. An explicit value (including 0 = "Due on
	// receipt") is honored verbatim; a negative value is clamped to 0. When
	// omitted, the tenant's configured terms apply, 0 included, as on every
	// engine invoice.
	netDays := 30
	switch {
	case input.NetPaymentTermDays != nil:
		netDays = *input.NetPaymentTermDays
		if netDays < 0 {
			netDays = 0
		}
	case s.settings != nil:
		if ts.NetPaymentTerms < 0 {
			return domain.Invoice{}, fmt.Errorf("tenant net payment terms is negative (%d)", ts.NetPaymentTerms)
		}
		netDays = ts.NetPaymentTerms
	}

	ctx = s.bindForCreate(ctx, tenantID, input)
	now := s.clock.Now(ctx)

	// Validate and build every line before the invoice number is taken: a
	// refused line must not use one up. The commit-expiry check reads the
	// clock bound just above.
	items := make([]domain.InvoiceLineItem, 0, len(input.LineItems))
	var subtotal int64
	commitLines := 0
	for i, liInput := range input.LineItems {
		li, err := buildLineItem(liInput, currency)
		if err != nil {
			return domain.Invoice{}, fmt.Errorf("line item %d: %w", i+1, err)
		}
		// Compose-time expiry sanity (ADR-078); re-checked at finalize.
		if li.CommitExpiresAt != nil && !li.CommitExpiresAt.After(now) {
			return domain.Invoice{}, fmt.Errorf("line item %d: %w", i+1,
				errs.Invalid("commit_expires_at", "must be in the future"))
		}
		if li.IsCommitLine() {
			commitLines++
		}
		subtotal += li.AmountCents
		items = append(items, li)
	}
	// One commit per invoice (ADR-078). The store enforces it too, on both
	// create (createWithLineItemsInTx) and AddLineItem (under the row lock);
	// checking here as well keeps a refused pair from using up a number,
	// which the store's check runs too late to prevent.
	if commitLines > 1 {
		return domain.Invoice{}, errs.Invalid("commit_granted_cents", "invoice already has a commit line — one commit per invoice; use a separate invoice")
	}
	// One-off invoices that omit a billing window default to "now → now" so
	// the NOT NULL period columns get sane values. Cycle invoices always
	// pass an explicit window from the engine.
	periodStart := input.BillingPeriodStart
	periodEnd := input.BillingPeriodEnd
	if periodStart.IsZero() {
		periodStart = now
	}
	if periodEnd.IsZero() {
		periodEnd = now
	}

	invoiceNumber, err := s.numberer.NextInvoiceNumber(ctx, tenantID)
	if err != nil {
		return domain.Invoice{}, fmt.Errorf("allocate invoice number: %w", err)
	}
	issuedAt := now
	// Net N runs on the tenant's civil calendar, so the due date stays the
	// same wall-clock time across a DST change.
	dueAt := domain.AddDaysIn(now, netDays, s.invoiceLocation(ctx, tenantID))

	inv := domain.Invoice{
		CustomerID:         input.CustomerID,
		SubscriptionID:     input.SubscriptionID,
		BillingTimezone:    billingLoc.String(),
		InvoiceNumber:      invoiceNumber,
		Status:             domain.InvoiceDraft,
		PaymentStatus:      domain.PaymentPending,
		Currency:           currency,
		BillingPeriodStart: periodStart,
		BillingPeriodEnd:   periodEnd,
		IssuedAt:           &issuedAt,
		DueAt:              &dueAt,
		NetPaymentTermDays: netDays,
		Memo:               strings.TrimSpace(input.Memo),
		// Operator-composed invoices are billing_reason='manual', mirroring
		// Stripe. The marker drives two behaviours: (1) Finalize computes tax
		// at finalize-time for these (cycle invoices already carry it), and
		// (2) auto-finalize-after-tax-retry skips them so the operator keeps
		// the explicit finalize step while a draft is still being composed.
		BillingReason: domain.BillingReasonManual,
		// Persist whether this draft is born on a frozen test clock, from the
		// customer's pin (manual invoices have no subscription to look through).
		// This is the authoritative write-time capture — the same direct field
		// check the engine uses for cycle invoices (sub.TestClockID) — NOT
		// clock.IsSimulated(ctx): bindForCreate binds ctx to the resolver's
		// effective-now even for UNPINNED customers (it returns wall-clock), so
		// "ctx is bound" would mis-flag every manual invoice as simulated.
		IsSimulated: onClock,
	}

	// Bare-header create — caller adds line items incrementally afterwards.
	if len(input.LineItems) == 0 {
		return s.store.CreateAudited(ctx, tenantID, inv, s.createEmission(ctx, 0))
	}

	// Atomic create-with-lines: header + items persist in one transaction
	// (lines were validated above). This closes the partial-failure window
	// the old create-then-loop-AddLineItem flow had (a network drop mid-loop
	// left a draft with some-but-not-all lines). Tax stays 0 here by design —
	// it's computed at finalize for manual invoices (see Finalize →
	// ComputeTaxForInvoice). Totals therefore equal the subtotal; finalize
	// rewrites them once tax is known.
	inv.SubtotalCents = subtotal
	inv.TotalAmountCents = subtotal
	inv.AmountDueCents = subtotal
	return s.store.CreateWithLineItemsAudited(ctx, tenantID, inv, items, s.createEmission(ctx, len(items)))
}

// createEmission builds the in-tx audit emission for an operator/API invoice
// create (POST /v1/invoices). BOTH manual-create store paths carry it — the
// bare-header create and the atomic create-with-lines — so the audit row does
// not depend on which shape the composer sent; lineItemCount is the only
// difference in content. Engine/cycle invoices reach the store through the
// un-audited variants: their canonical evidence is the finalize row, and
// double-emitting a 'create' there would fabricate an operator action.
//
// Wire strings (action "create", resource "invoice") and metadata keys are
// FROZEN — the dashboard's filter vocabulary and badges key on them.
// nil when no logger is wired (unit tests): the store then skips emission.
func (s *Service) createEmission(ctx context.Context, lineItemCount int) func(tx *sql.Tx, out domain.Invoice) error {
	if s.audit == nil {
		return nil
	}
	return func(tx *sql.Tx, out domain.Invoice) error {
		return s.audit.LogInTx(ctx, tx, audit.Entry{
			Action:        domain.AuditActionCreate,
			ResourceType:  "invoice",
			ResourceID:    out.ID,
			ResourceLabel: out.InvoiceNumber,
			Metadata: map[string]any{
				"customer_id":        out.CustomerID,
				"total_amount_cents": out.TotalAmountCents,
				"currency":           out.Currency,
				"line_item_count":    lineItemCount,
			},
		})
	}
}

func (s *Service) Get(ctx context.Context, tenantID, id string) (domain.Invoice, error) {
	inv, err := s.store.Get(ctx, tenantID, id)
	if err != nil {
		return domain.Invoice{}, err
	}
	return s.attachAttention(ctx, inv), nil
}

// CreditApplier drains a customer's credit balance against an invoice —
// atomic ledger debit + amount_due reduction. Satisfied by credit.Service;
// kept as a local interface so this package doesn't import internal/credit
// (same pattern as PaymentMethodReader).
type CreditApplier interface {
	ApplyToInvoiceAt(ctx context.Context, tenantID, customerID, invoiceID string, amountCents int64, at time.Time, invoiceNumber ...string) (int64, error)
}

// SetCreditApplier wires the credit-balance drain used at manual finalize
// (ADR-088: the balance applies to one-off invoices too — Stripe parity).
func (s *Service) SetCreditApplier(c CreditApplier) {
	s.creditApplier = c
}

// ApplyCreditBalance drains the customer's credit balance against a
// FINALIZED invoice and returns the refreshed row (ADR-088). Clock-bound:
// the ledger entry's timestamp rides the invoice's test clock for pinned
// customers (bindForInvoice), mirroring the engine's SimForInvoice.
// No-op (invoice returned unchanged) when no applier is wired, nothing is
// due, or the invoice isn't finalized — a draft's credits wait for
// finalize. An apply error is returned for the caller to route to the
// retry sweep, which re-applies atomically before its own charge.
func (s *Service) ApplyCreditBalance(ctx context.Context, tenantID, id string) (domain.Invoice, error) {
	ctx = s.bindForInvoice(ctx, tenantID, id)
	inv, err := s.store.Get(ctx, tenantID, id)
	if err != nil {
		return domain.Invoice{}, err
	}
	// finalized only (ADR-113 — the uncollectible admission served the
	// deleted charge-in-place recovery; leaving it would carry a claim-to-
	// charge race through the credit step instead of stopping it here).
	if s.creditApplier == nil || inv.AmountDueCents <= 0 ||
		inv.Status != domain.InvoiceFinalized {
		return inv, nil
	}
	if _, err := s.creditApplier.ApplyToInvoiceAt(ctx, tenantID, inv.CustomerID, inv.ID, inv.AmountDueCents, s.clock.Now(ctx), inv.InvoiceNumber); err != nil {
		return inv, err
	}
	return s.store.Get(ctx, tenantID, id)
}

// SettleZeroDue marks a finalized invoice with nothing left to pay as PAID —
// the ADR-066 terminal for zero-due invoices (Stripe parity: zero-amount
// invoices auto-mark paid with no payment attempt). Called by the manual
// finalize's collect step; the engine's cycle/threshold/tax-retry writers
// carry their own equivalent arms. Defensive re-read: only a FINALIZED,
// still-pending, zero-due invoice settles — the store's MarkPaid guard
// additionally rejects drafts and non-ok tax (the DEMO-000906 class), so a
// racing tax-pending draft can never jump to paid through here. paid_at is
// clock-bound (bindForInvoice) so clock-pinned customers settle in simulated
// time.
func (s *Service) SettleZeroDue(ctx context.Context, tenantID, id string) (domain.Invoice, error) {
	ctx = s.bindForInvoice(ctx, tenantID, id)
	inv, err := s.store.Get(ctx, tenantID, id)
	if err != nil {
		return domain.Invoice{}, err
	}
	// finalized only (ADR-113 reverted the brief uncollectible admission —
	// nothing initiates collection against a written-off invoice, so nothing
	// credit-settles one either). payment_status admits 'failed' as well as
	// 'pending' so an operator retry of a declined charge that is now fully
	// credit-covered still settles.
	if inv.Status != domain.InvoiceFinalized ||
		inv.AmountDueCents > 0 ||
		(inv.PaymentStatus != domain.PaymentPending && inv.PaymentStatus != domain.PaymentFailed) {
		return inv, nil // nothing to settle — not an error, the caller keeps the invoice as-is
	}
	return s.store.MarkPaid(ctx, tenantID, id, "", s.clock.Now(ctx))
}

// ClaimChargeForManualCollect takes the per-invoice charge hold for the
// operator's Collect Payment action — the same hold the auto-charge sweep
// and dunning retry take, so no two initiators charge one invoice
// concurrently (2026-07-21 snapshot-race audit). The zero ChargeHold = the
// claim was refused: another charge holds the invoice, a lost charge waits
// for the stale-hold sweep (ADR-127), a hosted page is live, or the invoice
// left the chargeable state.
func (s *Service) ClaimChargeForManualCollect(ctx context.Context, tenantID, id string) (domain.ChargeHold, error) {
	return s.store.ClaimChargeForManualCollect(ctx, tenantID, id)
}

// ReleaseChargeClaim frees the hold Collect took, on paths that never sent a
// charge (claim taken, then a gate failed, or credits settled the invoice,
// BEFORE ChargeInvoice was called). Never call it after a charge attempt — an
// unclear outcome must keep the hold, because the call may have reached Stripe
// (see Store.ReleaseChargeHold).
func (s *Service) ReleaseChargeClaim(ctx context.Context, tenantID, id string, hold domain.ChargeHold) error {
	return s.store.ReleaseChargeHold(ctx, tenantID, id, hold)
}

// ChargeHoldState reads the invoice's charge hold outside any lock. It words
// a refused claim; it is never a gate.
func (s *Service) ChargeHoldState(ctx context.Context, tenantID, id string) (domain.ChargeHoldState, error) {
	return s.store.ChargeHoldStateOf(ctx, tenantID, id)
}

// SetNoPMNotifiedAt stamps the send-once no-PM email marker (see Store).
func (s *Service) SetNoPMNotifiedAt(ctx context.Context, tenantID, id string, at time.Time) error {
	return s.store.SetNoPMNotifiedAt(ctx, tenantID, id, at)
}

// attachAttention computes the unified Attention surface from durable
// invoice fields plus the customer's payment-method status. Internal
// callers that read straight from the store (engine, scheduler,
// finalize path) skip this; only user-facing reads see the Attention
// field. Keeps the derivation single-source so handlers, webhook
// serializers, and hosted-invoice rendering all see the same shape.
//
// One invoice is a page of one: the same reads, the same classifier, as a
// list page (attachAttentionAll). When a reader isn't wired (older test
// fixtures, isolated unit tests), its fact stays false — HasPaymentMethod
// false → no_payment_method shows up — but that's safe: tests that exercise
// specific reasons override the field anyway.
func (s *Service) attachAttention(ctx context.Context, inv domain.Invoice) domain.Invoice {
	return s.attachAttentionAll(ctx, []domain.Invoice{inv})[0]
}

// attentionFacts is what the attention classifier needs from outside the
// invoice row, read for a whole page at once. Before issue #1142 every row
// read its own facts — a payment-setup read (customer + payment methods), a
// Stripe-credential read and, for unpaid rows, customer, dunning, pause and
// payment-page reads — each its own transaction. A 25-row page cost ~75-175
// transactions, ~110 ms. Batched, a page costs a fixed handful whatever its
// size. The per-row guards below decide WHICH ids join each batch, so a row
// pays for a read exactly when it did before.
type attentionFacts struct {
	stripeConnected bool
	paymentReady    map[string]bool                  // customer id → ready payment method
	hasEmail        map[string]bool                  // customer id → email on file
	escalated       map[string]bool                  // invoice id → dunning run escalated
	paused          map[string]bool                  // subscription id → pause_collection in force
	pageLive        map[string]bool                  // invoice id → hosted payment page live
	refusedCard     map[string]string                // invoice id → decline code that stops retries charging the default card
	retrying        map[string]bool                  // invoice id → a charge retry is still to come (RetryScheduled)
	lostDispute     map[string]domain.InvoiceDispute // invoice id → newest dispute closed as lost
}

// attachAttentionAll decorates every invoice with its Attention, reading the
// outside facts once per tenant rather than once per row. List rows share one
// tenant; grouping keeps the reads tenant-scoped for any caller that doesn't.
func (s *Service) attachAttentionAll(ctx context.Context, invs []domain.Invoice) []domain.Invoice {
	byTenant := map[string][]int{}
	var order []string
	for i, inv := range invs {
		if _, ok := byTenant[inv.TenantID]; !ok {
			order = append(order, inv.TenantID)
		}
		byTenant[inv.TenantID] = append(byTenant[inv.TenantID], i)
	}
	for _, tenantID := range order {
		idx := byTenant[tenantID]
		page := make([]domain.Invoice, len(idx))
		for j, i := range idx {
			page[j] = invs[i]
		}
		facts := s.readAttentionFacts(ctx, tenantID, page)
		var tenantLoc *time.Location // read once per tenant, only if a row needs it
		for j, i := range idx {
			invs[i] = s.decorateInvoice(ctx, page[j], facts, &tenantLoc)
		}
	}
	return invs
}

// awaitsPaymentNoCard is the precondition of the classifier's no-card
// branches: finalized, payment pending, no ready payment method.
func awaitsPaymentNoCard(inv domain.Invoice, ready bool) bool {
	return !ready && inv.Status == domain.InvoiceFinalized && inv.PaymentStatus == domain.PaymentPending
}

// pauseCandidate reports whether a row's notice depends on the subscription's
// collection pause: an unpaid finalized invoice that is queued for the
// auto-charge, or that waits for a card. Its notice also depends on the
// invoice's dunning run, which the pause does not stop.
func pauseCandidate(inv domain.Invoice, ready bool) bool {
	return (inv.AutoChargePending || !ready) && inv.Status == domain.InvoiceFinalized &&
		inv.PaymentStatus == domain.PaymentPending
}

// readAttentionFacts reads one tenant's attention facts for a page of
// invoices. Every read is display-only: a read error leaves its fact false
// for the page, the same safe direction a per-row error took before.
func (s *Service) readAttentionFacts(ctx context.Context, tenantID string, page []domain.Invoice) attentionFacts {
	f := attentionFacts{}

	if s.stripeChecker != nil && tenantID != "" {
		// Livemode comes off ctx (auth middleware set it) — invoice
		// rows don't carry the column on the domain struct since the
		// DB trigger derives it from app.livemode at insert time. The
		// request that's reading the invoice is already mode-scoped,
		// so the ctx value matches the row's stored mode under RLS.
		// Tenant- and mode-wide, so one read covers the page.
		f.stripeConnected = s.stripeChecker.HasFor(ctx, tenantID, postgres.Livemode(ctx))
	}

	if s.paymentMethods != nil {
		if ids := distinct(page, func(inv domain.Invoice) string { return inv.CustomerID }); len(ids) > 0 {
			if ready, err := s.paymentMethods.PaymentReadyCustomers(ctx, tenantID, ids); err == nil {
				f.paymentReady = ready
			}
		}
	}

	// Email presence drives the no_payment_method banner variant (the engine's
	// setup-link email skips silently with no address, so the banner must not
	// claim it was sent). Read only for the finalized-unpaid-no-PM invoices
	// that actually reach that classifier branch. The guard is a cheap superset
	// of the classifier's no_payment_method precondition; a rare over-fetch
	// when a higher-priority reason (tax/payment failure) preempts it is
	// harmless. On a read error CustomerHasEmail stays false: the classifier's
	// false-branch copy states engine behavior rather than asserting the
	// customer's email state, so the banner is honest even when undetermined.
	if s.customerReader != nil {
		ids := distinct(page, func(inv domain.Invoice) string {
			if awaitsPaymentNoCard(inv, f.paymentReady[inv.CustomerID]) {
				return inv.CustomerID
			}
			return ""
		})
		if len(ids) > 0 {
			if custs, err := s.customerReader.GetMany(ctx, tenantID, ids); err == nil {
				f.hasEmail = make(map[string]bool, len(custs))
				for _, c := range custs {
					f.hasEmail[c.ID] = c.Email != ""
				}
			}
		}
	}

	// A refused card (#966), or a bank's request that the customer confirm
	// the payment, changes only the payment-failure notice, so only failed,
	// finalized invoices join the read. A read error leaves it empty:
	// the notice then shows the ordinary decline, which still leads with
	// Update payment method. Read before the dunning runs, which it gates.
	if s.cardRefusals != nil {
		ids := distinct(page, func(inv domain.Invoice) string {
			if inv.Status == domain.InvoiceFinalized && inv.PaymentStatus == domain.PaymentFailed {
				return inv.ID
			}
			return ""
		})
		if len(ids) > 0 {
			refused, err := s.cardRefusals.RetryBlockingDeclinesOnDefaultCards(ctx, tenantID, ids)
			if err != nil {
				slog.Warn("invoice: attention could not read refused cards", "error", err,
					"tenant_id", tenantID, "invoices", len(ids))
			}
			f.refusedCard = refused
		}
	}

	// Same guard for the dunning-escalated signal (P1-4): only the
	// finalized-unpaid-no-PM invoices that reach classifyNoPaymentMethod
	// join the read. No run / read errors leave the zero-value (false) — the
	// banner shows the normal pre-escalation copy, which is the safe
	// direction of drift.
	//
	// The refused-card notice (#966), and the must-confirm one, join the same
	// read: each may say that
	// retries keep their schedule only while a charge retry is still to come
	// (InvoiceDunningRun.RetryScheduled: active, not paused, attempts left
	// under the bound policy). No run, or a read error, leaves retrying false, and the notice
	// then says no retry is scheduled — the direction that still sends the
	// operator to get a new card, rather than waiting on a retry that may
	// never run.
	//
	// The paused-collection notices join it too (sweep A2): a collection
	// pause does not stop a run already open on the invoice, so every row the
	// pause state is read for (pauseCandidate) needs the run as well. No run,
	// or a read error, leaves retrying false and the notice says nothing
	// charges the invoice until collection resumes.
	if s.dunningReader != nil {
		ids := distinct(page, func(inv domain.Invoice) string {
			if pauseCandidate(inv, f.paymentReady[inv.CustomerID]) || f.refusedCard[inv.ID] != "" {
				return inv.ID
			}
			return ""
		})
		if len(ids) > 0 {
			if runs, err := s.dunningReader.ListRunsByInvoices(ctx, tenantID, ids); err == nil {
				f.escalated = make(map[string]bool, len(runs))
				f.retrying = make(map[string]bool, len(runs))
				for _, run := range runs {
					f.escalated[run.InvoiceID] = run.State == domain.DunningEscalated
					// A run with every attempt used stays active while its final
					// action waits for the invoice grace or is re-attempted after
					// it failed; its next tick runs only the action, never a charge.
					f.retrying[run.InvoiceID] = run.RetryScheduled()
				}
			}
		}
	}

	// Pause state for the queued-but-not-scheduled banner, gated on the exact
	// precondition of the branch that reads it: QUEUED rows, and card-less
	// rows, whose notice says "charged once the customer adds a card" — false
	// while collection is paused (the sweep that charges on attach skips
	// paused subscriptions). Healthy, paid, draft and failed rows skip it.
	// Display-only: a read error shows the not-paused banner (logged); the
	// charge sweeps themselves skip a paused subscription. Dunning retries do
	// not, which is why these rows also read their dunning run above.
	subIDs := distinct(page, func(inv domain.Invoice) string {
		if pauseCandidate(inv, f.paymentReady[inv.CustomerID]) {
			return inv.SubscriptionID
		}
		return ""
	})
	if len(subIDs) > 0 {
		paused, err := s.collectionPausedMany(ctx, tenantID, subIDs)
		if err != nil {
			slog.Warn("invoice: attention could not read pause state", "error", err,
				"tenant_id", tenantID, "subscriptions", len(subIDs))
		}
		f.paused = paused
	}

	// A live hosted payment page holds every Velox charge (ADR-068). Only a
	// finalized, unpaid invoice can have one that matters. A read error leaves
	// it false: the banner then offers Retry, which a live page refuses with a
	// clear message.
	if s.paymentPages != nil {
		ids := distinct(page, func(inv domain.Invoice) string {
			if inv.Status == domain.InvoiceFinalized &&
				(inv.PaymentStatus == domain.PaymentPending || inv.PaymentStatus == domain.PaymentFailed) {
				return inv.ID
			}
			return ""
		})
		if len(ids) > 0 {
			if live, err := s.paymentPages.PaymentPagesLive(ctx, tenantID, ids); err == nil {
				f.pageLive = live
			}
		}
	}

	// A lost dispute (#979) can sit on any invoice that took a payment, and a
	// paid invoice is where it usually does, so every non-draft row joins
	// the read: one indexed query for the page. A read error leaves the map
	// empty and the notice off (logged); the dispute itself stays on the
	// invoice's timeline and in its disputes list.
	if s.disputes != nil {
		ids := distinct(page, func(inv domain.Invoice) string {
			if inv.Status != domain.InvoiceDraft {
				return inv.ID
			}
			return ""
		})
		if len(ids) > 0 {
			lost, err := s.disputes.LostDisputesByInvoices(ctx, tenantID, ids)
			if err != nil {
				slog.Error("invoice: attention could not read lost disputes", "error", err,
					"tenant_id", tenantID, "invoices", len(ids))
			}
			f.lostDispute = lost
		}
	}

	return f
}

// distinct collects the non-empty keys key returns over the page, each once,
// in first-seen order.
func distinct(page []domain.Invoice, key func(domain.Invoice) string) []string {
	seen := make(map[string]bool, len(page))
	var out []string
	for _, inv := range page {
		k := key(inv)
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
	}
	return out
}

// decorateInvoice sets one invoice's Attention, RecoveryWarning and
// BillingPeriodDisplay from its row and the page's facts. tenantLoc caches
// the tenant timezone across the page.
func (s *Service) decorateInvoice(ctx context.Context, inv domain.Invoice, f attentionFacts, tenantLoc **time.Location) domain.Invoice {
	// Wall-clock now ages the in-flight payment banner (Info → Warning past the
	// expected-settle window). Deliberately NOT s.clock.Now(ctx): processing
	// staleness is a real-world duration (the provider settles in wall-clock),
	// and the classifier guards on !IsSimulated so a clock-pinned invoice never
	// escalates on a wall-clock age it can't satisfy.
	atc := domain.AttentionContext{Now: time.Now().UTC()} //nolint:forbidigo // wall-clock: processing-staleness is a real-world duration (provider settles in wall-clock); classifier guards on !IsSimulated
	atc.HasPaymentMethod = inv.CustomerID != "" && f.paymentReady[inv.CustomerID]
	if awaitsPaymentNoCard(inv, atc.HasPaymentMethod) {
		atc.CustomerHasEmail = f.hasEmail[inv.CustomerID]
		atc.DunningEscalated = f.escalated[inv.ID]
	}
	if pauseCandidate(inv, atc.HasPaymentMethod) {
		atc.CollectionPaused = f.paused[inv.SubscriptionID]
	}
	atc.PaymentPageOpen = f.pageLive[inv.ID]
	atc.RetryBlockingDeclineCode = f.refusedCard[inv.ID]
	atc.DunningRetrying = f.retrying[inv.ID]
	if d, ok := f.lostDispute[inv.ID]; ok {
		atc.LostDispute = &d
	}
	// A clock-pinned invoice's failure banner dates "since" from its latest
	// failed charge's simulated instant. Simulated failed invoices only, so a
	// live list page never pays this read.
	if inv.IsSimulated && inv.PaymentStatus == domain.PaymentFailed && inv.ID != "" {
		if attempts, err := s.store.ListChargeAttemptsByInvoice(ctx, inv.TenantID, inv.ID); err == nil {
			for i := len(attempts) - 1; i >= 0; i-- {
				if attempts[i].Outcome == domain.ChargeAttemptFailed && attempts[i].SimEffectiveAt != nil {
					atc.LastFailedChargeAt = attempts[i].SimEffectiveAt
					break
				}
			}
		}
	}
	// Prepaid-commit exposure: credit this invoice already handed the customer
	// while the cash never arrived (ADR-078 D3 fast-follow). Read per row, but
	// behind the tightest guard of any attention read — commit lines
	// are manual-invoice-only and funded at finalize (ADR-078 D2/D3), so a
	// draft or a subscription-cycle invoice can never carry a live grant, and
	// a paid one has nothing to report (the cash arrived); none of them pays
	// for the lookup, except the paid case below. What remains is one hit on
	// the unique partial index idx_credit_ledger_commit_fund_dedup.
	//
	// The clock here is the RESOLVER's, deliberately unlike atc.Now a few lines
	// above: that one ages an in-flight payment, which settles in the real
	// world, while this one decides whether a grant has expired — and expiry is
	// a billing-domain instant that a test clock moves. drainPositiveBlocks
	// judges the same grant against the same axis, so a banner and a drawdown
	// can never disagree about whether credit is still live.
	//
	// A read error leaves both figures zero and claims no exposure. The
	// alternative — a banner asserting a balance we could not read — is the
	// failure worth avoiding, and the invoice's own cause banner still renders.
	//
	// One paid case joins the read: a lost dispute (#1287 review). The bank
	// took the cash back while the credit stays spendable, and the
	// dispute_lost notice must say so and point to commit relief instead of
	// the credit note a commit invoice refuses. Only paid invoices with a lost
	// dispute pay for it, which is almost none.
	if s.commitGrants != nil && inv.ID != "" &&
		inv.BillingReason == domain.BillingReasonManual &&
		(inv.Status == domain.InvoiceFinalized || inv.Status == domain.InvoiceUncollectible ||
			(inv.Status == domain.InvoicePaid && atc.LostDispute != nil)) {
		if drawable, consumed, err := s.commitGrants.CommitGrantExposure(ctx, inv.TenantID, inv.ID, s.clock.Now(ctx)); err == nil {
			atc.CommitGrantDrawableCents = drawable
			atc.CommitGrantConsumedCents = consumed
		}
	}
	atc.StripeConnected = f.stripeConnected
	atc.SetupLinkEmailOff = s.setupLinkEmailOff
	inv.Attention = domain.ClassifyInvoiceAttention(inv, atc)

	// What an operator must know BEFORE recording a wire against a
	// written-off invoice (tax already reported as uncollected / threshold
	// usage re-billed / unapplied clawback relief). Not a refusal — the money
	// arrived, and refusing to record it would only make the books wrong too —
	// but the consequence goes unreconciled and nothing downstream will flag
	// it. Guarded to uncollectible: no other status can carry the warning, and
	// the credit-note read is the only cost. (The charge-blocking twin this
	// read once also fed was removed by ADR-113 — nothing initiates money
	// against a written-off invoice anymore.)
	if inv.Status == domain.InvoiceUncollectible {
		var unrelieved int64
		if s.creditNotes != nil && inv.ID != "" {
			if n, err := s.creditNotes.UnreliefedClawbackCents(ctx, inv.TenantID, inv.ID); err == nil {
				unrelieved = n
			}
		}
		inv.RecoveryWarning = domain.RecoveryWarnsOnOfflinePayment(inv, unrelieved)
	}

	// Compute the inclusive display end ("Jun 1 – Jun 30") on the read path
	// (ADR-058 follow-up). Storage stays half-open; this is the single
	// backend-authored value every render surface (PDF, hosted, dashboard, list)
	// shows, so the inclusive end can't drift across runtimes. Anchored in the
	// invoice's OWN billing TZ (denormalized onto the invoice at
	// creation), not the live tenant TZ — so the inclusive last-day step lands on
	// the right civil date even after the tenant changes its timezone.
	inv.BillingPeriodDisplay = domain.FormatInclusivePeriod(
		inv.BillingPeriodStart, inv.BillingPeriodEnd, s.invoiceDisplayLocCached(ctx, inv, tenantLoc))
	return inv
}

// invoiceDisplayLoc resolves the timezone an invoice's period is anchored in:
// the denormalized snapshot (ADR-077) when present, else the live tenant TZ for
// ad-hoc/manual invoices with no owning subscription and legacy rows created
// before the snapshot column existed (preserving their prior display exactly).
func (s *Service) invoiceDisplayLoc(ctx context.Context, inv domain.Invoice) *time.Location {
	var tenantLoc *time.Location
	return s.invoiceDisplayLocCached(ctx, inv, &tenantLoc)
}

// invoiceDisplayLocCached is invoiceDisplayLoc reading the tenant TZ at most
// once into *tenantLoc, so a list page of one tenant's invoices pays one
// settings read, not one per row.
func (s *Service) invoiceDisplayLocCached(ctx context.Context, inv domain.Invoice, tenantLoc **time.Location) *time.Location {
	if inv.BillingTimezone != "" {
		return domain.LoadLocationOrUTC(inv.BillingTimezone)
	}
	if *tenantLoc == nil {
		*tenantLoc = s.invoiceLocation(ctx, inv.TenantID)
	}
	return *tenantLoc
}

// invoiceLocation resolves the tenant billing timezone for display math, UTC
// when no settings reader is wired or the tenant has no timezone configured —
// matching ADR-058 / engine.tenantLocation.
func (s *Service) invoiceLocation(ctx context.Context, tenantID string) *time.Location {
	if s.settings == nil {
		return time.UTC
	}
	ts, err := s.settings.Get(ctx, tenantID)
	if err != nil {
		return time.UTC
	}
	return domain.LoadLocationOrUTC(ts.Timezone)
}

func (s *Service) List(ctx context.Context, filter ListFilter) ([]domain.Invoice, int, error) {
	invs, total, err := s.store.List(ctx, filter)
	if err != nil {
		return nil, 0, err
	}
	return s.attachAttentionAll(ctx, invs), total, nil
}

func (s *Service) Finalize(ctx context.Context, tenantID, id string) (domain.Invoice, error) {
	return s.finalize(ctx, tenantID, id, true)
}

// finalize is Finalize; checkTaxExpiry=false skips the tax-calculation expiry
// guard, for the one caller whose calculation was created moments earlier in
// the same call (the auto-finalize after a successful tax retry): it cannot
// be 24h old, and since the guard fails closed on a read error, running it
// there would only add ways to leave a freshly-taxed invoice a draft.
func (s *Service) finalize(ctx context.Context, tenantID, id string, checkTaxExpiry bool) (domain.Invoice, error) {
	ctx = s.bindForInvoice(ctx, tenantID, id)
	inv, err := s.store.Get(ctx, tenantID, id)
	if err != nil {
		return domain.Invoice{}, err
	}
	if inv.Status != domain.InvoiceDraft {
		return domain.Invoice{}, errs.InvalidState(fmt.Sprintf("can only finalize draft invoices, current status: %s", inv.Status))
	}
	// Compute tax for operator-composed (manual) invoices at finalize.
	// Cycle invoices already carry engine-computed tax from build time;
	// manual invoices accrue line items in the composer and have none
	// until now. This mirrors Stripe, which calculates tax when an invoice
	// is finalized rather than at draft-create. The engine resolves the
	// tenant's provider: 'none' → tax 0; 'manual' → flat-rate tax computed
	// synchronously; 'stripe' → a calculation that the tax_status block
	// below may park as pending until the retry worker commits it. Skipped
	// when no retrier is wired (isolated unit-test fixtures).
	if s.taxRetrier != nil && isOperatorComposed(inv) {
		taxed, terr := s.taxRetrier.ComputeTaxForInvoice(ctx, tenantID, id)
		if terr != nil {
			return domain.Invoice{}, fmt.Errorf("compute tax at finalize: %w", terr)
		}
		inv = taxed
	}
	// Block finalize while tax is unresolved. Sending an invoice with
	// wrong or missing tax creates compliance exposure; we defer until the
	// retry worker lifts the block (TaxStatus=ok) or an operator resolves
	// a failed calculation manually.
	if inv.TaxStatus == domain.InvoiceTaxPending {
		// The block message mirrors the reconciler's REAL policy
		// (domain.TaxRetryableErrorCodes / MaxTaxRetryAttempts) — the old
		// blanket "retry in progress" claimed retries for non-retryable
		// codes and for exhausted invoices, neither of which the
		// reconciler ever touches (2026-07-19 truth audit).
		code := inv.TaxErrorCode
		if code == "" {
			code = "unknown"
		}
		switch {
		case domain.TaxErrorCodeRetryable(code) && inv.TaxRetryCount < domain.MaxTaxRetryAttempts:
			return domain.Invoice{}, errs.InvalidState("tax calculation pending — automatic retry in progress, finalize blocked")
		case domain.TaxErrorCodeRetryable(code):
			return domain.Invoice{}, errs.InvalidState("tax calculation pending — automatic retries exhausted; retry tax manually, finalize blocked")
		default:
			return domain.Invoice{}, errs.InvalidState("tax calculation pending — operator action required (" + code + "), finalize blocked")
		}
	}
	// Expired-calculation check, BEFORE the transition — and ONLY for a
	// calculation whose transaction was never recorded. The tax commit
	// below deliberately does not unwind a finalize (transient provider
	// errors defer to the orphan reconciler, which re-commits within its
	// window) — but an EXPIRED calculation is permanent: the reconciler
	// is blocked by the same age rule, so deferring it produces an
	// invoice collected with tax printed and never reported to Stripe.
	// Refusing here, while the invoice is still a draft, is the only
	// point where the guard's own remedy — "retry tax to refresh, then
	// finalize" — is still available.
	//
	// The TaxTransactionID gate is load-bearing, found by walking the
	// first version of this check against a live engine-built draft:
	// cycle invoices commit their tax AT BUILD TIME (best-effort, right
	// after the row is durable), so the common paused-collection draft
	// already carries its transaction and its aged calc needs NO commit
	// at finalize. Blocking that shape would send the operator to "retry
	// tax", minting a second calculation beside a committed transaction —
	// double-reported tax. Only the shape where the build-time commit
	// FAILED (calc present, transaction absent) both needs the commit and
	// can never get it once expired.
	if checkTaxExpiry && s.taxCommitter != nil && inv.TaxCalculationID != "" && inv.TaxTransactionID == "" {
		if err := s.taxCommitter.CheckTaxCalculationUsable(ctx, tenantID, id, inv.TaxCalculationID); err != nil {
			return domain.Invoice{}, err
		}
	}
	// Anchor issue + due dates to the finalize moment for operator-composed
	// (manual) invoices, mirroring Stripe's finalized_at: a manual draft is
	// "issued" when the operator finalizes it — possibly on a later test-clock
	// instant than draft-create — and Net terms run from issuance. ctx is
	// already bound to the invoice's clock (bindForInvoice above), so
	// clock.Now is simulated time for clock-pinned customers. Net term is the
	// value the draft was created with. Cycle invoices are born finalized at
	// build time and keep those dates (the UpdateStatus branch).
	var finalized domain.Invoice
	if isOperatorComposed(inv) {
		issuedAt := s.clock.Now(ctx)
		dueAt := domain.AddDaysIn(issuedAt, inv.NetPaymentTermDays, s.invoiceDisplayLoc(ctx, inv))
		finalized, err = s.store.FinalizeWithDates(ctx, tenantID, id, issuedAt, dueAt)
	} else {
		finalized, err = s.store.UpdateStatus(ctx, tenantID, id, domain.InvoiceFinalized)
	}
	if err != nil {
		return domain.Invoice{}, err
	}
	// Generate the hosted-invoice-URL token. Non-fatal if it fails — mirrors
	// the tax-commit convention below: the invoice is already authoritative
	// after UpdateStatus, so a transient DB hiccup on a side-effect doesn't
	// unwind the state transition. Operators can repair via the rotate
	// endpoint. Happens BEFORE tax commit because the rotate endpoint will
	// anyway need to talk to Stripe-less code paths, and token generation
	// is pure Go with no external dependency.
	if token, tokenErr := GeneratePublicToken(); tokenErr != nil {
		slog.Warn("invoice: public token generation failed at finalize",
			"error", tokenErr, "tenant_id", tenantID, "invoice_id", finalized.ID)
	} else if err := s.store.SetPublicToken(ctx, tenantID, id, token); err != nil {
		slog.Warn("invoice: public token persist failed at finalize",
			"error", err, "tenant_id", tenantID, "invoice_id", finalized.ID)
	} else {
		finalized.PublicToken = token
	}
	// Commit Stripe Tax calculation to a tax_transaction at finalize. Missing
	// calculation id = manual/none provider — skip silently. Commit failure
	// does not unwind finalize: the invoice is already authoritative; we log
	// and continue so the customer-facing state stays consistent.
	// TaxTransactionID gate mirrors the pre-check above: an already-
	// committed calc (build-time commit succeeded) has nothing left to
	// commit, and re-committing an aged one only produces expiry noise.
	if s.taxCommitter != nil && finalized.TaxCalculationID != "" && finalized.TaxTransactionID == "" {
		if err := s.taxCommitter.CommitTax(ctx, tenantID, finalized.ID, finalized.TaxCalculationID); err != nil {
			slog.Warn("invoice: tax commit failed at finalize",
				"error", err, "tenant_id", tenantID, "invoice_id", finalized.ID,
				"tax_provider", finalized.TaxProvider, "calculation_id", finalized.TaxCalculationID)
		}
	}
	// Canonical finalize audit row — written HERE (not per entry point) so
	// every path through Finalize gets exactly one row: the operator HTTP
	// endpoint (actor = the operator, from ctx) AND the tax-retry
	// auto-finalize chain (actor = system). The handler's own write was
	// removed alongside this — two writers per action is the duplicate-row
	// bug mark-uncollectible / record-payment had.
	if s.audit != nil {
		_ = s.audit.Log(ctx, tenantID, domain.AuditActionFinalize, "invoice", finalized.ID, finalized.InvoiceNumber, map[string]any{
			"invoice_number":     finalized.InvoiceNumber,
			"customer_id":        finalized.CustomerID,
			"total_amount_cents": finalized.TotalAmountCents,
			"currency":           finalized.Currency,
		})
	}
	// invoice.finalized webhook — emitted HERE for the same single-writer
	// reason as the audit row above: pre-fix the handler fired it, so the
	// tax-retry auto-finalize chain (RetryTax → Finalize) silently never
	// notified integrators that a tax-deferred invoice finalized.
	// It carries the finalized invoice (ADR-131), as the engine's sender does
	// (billing.DispatchInvoiceFinalized). The collector applies credit after
	// this (ADR-116), so its amount_due_cents is before that credit.
	if s.events != nil {
		s.dispatchEvent(ctx, tenantID, domain.EventInvoiceFinalized, domain.InvoiceWebhookData(finalized, nil))
	}
	return finalized, nil
}

// reverseInvoiceTax reverses the upstream tax transaction for an invoice being
// VOIDED, or for a credit note — events that change what was supplied. NOT for
// a write-off, which changes only who paid (ADR-111).
//
// This comment previously cited two authorities for reversing on write-off and
// BOTH WERE FALSE. It attributed to Stripe: "When an invoice is voided or
// marked uncollectible, you must reverse the corresponding tax transaction" —
// a string that does not exist in Stripe's documentation, which describes its
// own reporting behaviour and imposes nothing on an integrator. And it cited
// EU VAT Directive Art. 90, which says the OPPOSITE: reduction on non-payment
// happens "under conditions which shall be determined by the Member States",
// with Art. 90(2) allowing derogation entirely. Two invented citations holding
// up a money decision.
//
// Best-effort: a transient failure
// logs but does NOT block the status transition — the invoice is the operator's
// primary record and must stay flippable even if upstream is unreachable.
//
// Reverses only the portion NOT already reversed by prior credit notes. A
// finalized-unpaid invoice can have adjustment credit notes (ADR-050 downgrade /
// cancel relief), each of which already fired its own ModePartial reversal for
// its gross slice; an unconditional ModeFull here would double-reverse that
// slice and under-remit the tenant's output tax. The remaining un-reversed gross
// = total − Σ(non-voided credit-note totals), read via the optional creditNotes
// reader. INVARIANT (guarded by this code, enforced by the reader): tax is
// reversed ONLY by credit notes — payments and applied customer credit reduce
// amount_due WITHOUT reversing tax — so the credited total, not amount_due, is
// the basis. Equals total when no credit note exists (→ ModeFull, unchanged);
// zero when fully credit-noted (skip).
//
// Preconditions: taxReverser wired (skipped for narrow tests + none/manual
// providers) and a committed upstream transaction (TaxTransactionID non-empty).
func (s *Service) reverseInvoiceTax(ctx context.Context, tenantID string, inv domain.Invoice, reference string) error {
	if s.taxReverser == nil || inv.TaxTransactionID == "" {
		return nil
	}
	req := tax.ReversalRequest{
		OriginalTransactionID: inv.TaxTransactionID,
		Reference:             reference,
		InvoiceID:             inv.ID,
		Mode:                  tax.ReversalModeFull,
	}
	// Remaining un-reversed gross = total minus what prior CREDIT NOTES already
	// reversed. ONLY credit notes reverse tax — both card payments AND applied
	// customer credit reduce amount_due WITHOUT reversing tax — so prefer the
	// credited total read directly. (amount_paid + amount_due is only a fallback
	// proxy; it equals the credited remainder when no customer credit was
	// applied, but UNDER-reverses when it was — the audit-confirmed bug.) With no
	// credit note this is the full total → ModeFull (unchanged); fully
	// credit-noted → zero, nothing to reverse.
	if inv.TotalAmountCents > 0 {
		remainingGross := inv.AmountPaidCents + inv.AmountDueCents
		if s.creditNotes != nil {
			creditNoted, cerr := s.creditNotes.IssuedCreditedCents(ctx, tenantID, inv.ID)
			if cerr != nil {
				// Fail, never guess: the amount-due proxy UNDER-reverses when
				// customer credit was applied, and the short reversal would be
				// filed for good under the one inv_taxrev_<id> reference. Failing
				// leaves tax_reversed_at NULL, so RetryPendingTaxReversal re-drives
				// it with the real credited total.
				return fmt.Errorf("read credit-noted total for tax reversal: %w", cerr)
			}
			remainingGross = inv.TotalAmountCents - creditNoted
		}
		if remainingGross < 0 {
			remainingGross = 0
		}
		if remainingGross <= 0 {
			// Fully credit-noted: nothing for THIS reference to reverse. Each
			// credit note already reversed its own slice under its OWN reference
			// (cn_<id>), with its own retry/recovery (the creditnote path) — so
			// reversing nothing here is correct, not a hidden failure. Terminal
			// success: stamp tax_reversed_at so the sweep stops re-scanning the
			// row (it carries a tax_transaction_id but the inv_taxrev slice is 0).
			s.markTaxReversed(ctx, tenantID, inv.ID)
			return nil
		}
		if remainingGross < inv.TotalAmountCents {
			req.Mode = tax.ReversalModePartial
			req.GrossAmountCents = remainingGross
		}
	}
	if _, err := s.taxReverser.ReverseTax(ctx, tenantID, req); err != nil {
		// Leave tax_reversed_at NULL: RetryPendingTaxReversal (the scheduler
		// sweep, symmetric sibling of #267's RetryPendingTaxCommit) re-drives
		// this on the next tick. The re-drive converges via the request's
		// Idempotency-Key (velox_tax_rev_<ref>): within Stripe's cache window a
		// retry returns the original reversal (a true no-op), and the stable
		// `inv_taxrev_<id>` Reference prevents a duplicate reversal from ever
		// being created. (A retry after the idempotency cache expires for a
		// reversal that already succeeded would get a reference-already-used
		// rejection — surfaced as this same loud ERROR, never a double-reverse.)
		// ERROR (not WARN) so a sustained failure is alertable and the
		// manual-reconcile path has a loud signal.
		slog.ErrorContext(ctx, "tax reversal failed on invoice status change — invoice updated locally; recovery sweep will retry",
			"invoice_id", inv.ID,
			"tax_transaction_id", inv.TaxTransactionID,
			"reference", reference,
			"reversal_mode", string(req.Mode),
			"error", err)
		return err
	}
	// Reversal confirmed upstream — stamp the marker so the sweep drops this row.
	s.markTaxReversed(ctx, tenantID, inv.ID)
	return nil
}

// markTaxReversed records that an invoice's upstream tax transaction has been
// reversed (or needed no reversal), clearing it from the RetryPendingTaxReversal
// sweep. Best-effort: a failed stamp leaves the row in the sweep, which re-drives
// the (idempotent) reversal until the stamp sticks — the same self-healing shape
// as the commit reconciler.
func (s *Service) markTaxReversed(ctx context.Context, tenantID, invoiceID string) {
	if err := s.store.MarkTaxReversed(ctx, tenantID, invoiceID); err != nil {
		slog.WarnContext(ctx, "failed to stamp tax_reversed_at; recovery sweep will re-confirm",
			"invoice_id", invoiceID, "error", err)
	}
}

func (s *Service) Void(ctx context.Context, tenantID, id string) (domain.Invoice, error) {
	ctx = s.bindForInvoice(ctx, tenantID, id)
	inv, err := s.store.Get(ctx, tenantID, id)
	if err != nil {
		return domain.Invoice{}, err
	}
	if err := voidRefusal(inv); err != nil {
		return domain.Invoice{}, err
	}

	// Atomic: flip status to voided AND reverse the consumed customer credits
	// (credits applied to this invoice reduce amount_due without touching the
	// ledger balance; voiding must hand them back) in ONE transaction. A
	// reversal failure rolls the void back, so the invoice never lands
	// voided-but-credits-still-consumed — the pre-fix shape, where the handler
	// called ReverseForInvoice as a separate WARN-swallowed step AFTER the void
	// committed, silently stripped the customer of paid-with credits on any
	// transient failure (no reconciler re-drove it). The 0106 dedup index keeps
	// the in-tx reversal exactly-once across redelivery.
	//
	// The refusal above ran on a copy read before the row lock; a charge can
	// claim the invoice between that read and this write (#975), so it runs
	// again on the locked row, with the charge hold.
	voided, err := s.store.UpdateStatusWithReversalPrior(ctx, tenantID, id, domain.InvoiceVoided, func(tx *sql.Tx, locked domain.Invoice) error {
		if err := voidRefusal(locked); err != nil {
			return err
		}
		if err := s.refuseWhileChargeHeld(ctx, tx, locked, domain.ActionVoid); err != nil {
			return err
		}
		return s.voidEffectsTx(ctx, tx, tenantID, locked, locked.Status)
	})
	if err != nil {
		return domain.Invoice{}, err
	}
	s.AfterVoid(ctx, tenantID, voided)
	return voided, nil
}

// voidRefusal is Void's state machine: paid and voided invoices cannot be
// voided, and neither can one whose charge is in flight.
func voidRefusal(inv domain.Invoice) error {
	if inv.Status == domain.InvoicePaid {
		return errs.InvalidState("cannot void a paid invoice — issue a credit note instead")
	}
	if inv.Status == domain.InvoiceVoided {
		return errs.InvalidState("invoice is already voided")
	}
	// A charge in flight (processing/unknown) may still succeed: voiding now
	// would strand captured money on a voided invoice and reverse tax on a
	// sale that completes. Block until the charge reaches a terminal state —
	// the reconciler resolves it, then the void can proceed. Stripe enforces
	// the same rule ("you can't void an invoice with open payments"). Checked
	// before any tax reversal can fire.
	if b := domain.PaymentBlocksAction(inv, domain.ActionVoid); b.Blocked {
		return errs.InvalidState(b.Message).WithCode(b.Code)
	}
	return nil
}

// refuseWhileChargeHeld is the payment-state refusal for void, write-off and
// offline payment, run on the row read under the row lock in tx. Beyond
// PaymentBlocksAction it refuses while a charge holds the invoice: a charge
// between its claim and its outcome leaves payment_status 'pending' or
// 'failed' and writes only the hold, so without this the three actions went
// through while Stripe could be taking the money — a charge then succeeding
// on a voided invoice, or a customer paying twice after an offline payment
// (#975, ADR-059 amendment).
//
// Since ADR-127 a stale hold refuses void and offline payment too: its
// charge's result was lost and Stripe may hold the payment. The stale-hold
// sweep asks Stripe within about an hour, so an operator (or a cancel's
// prebill void, R3) waits up to that long after a lost charge, and a hold left
// by a charge that was never sent (breaker open, credentials unreadable)
// costs the same wait. Write-off stays allowed on a stale hold (see
// domain.PaymentBlocksActionHeld).
//
// The hold's own owner passes: dunning's final action takes the hold for the
// subscription cancel, and the cancel's prebill void runs under that ctx
// (domain.WithChargeHoldOwner).
func (s *Service) refuseWhileChargeHeld(ctx context.Context, tx *sql.Tx, locked domain.Invoice, action domain.InvoiceAction) error {
	state, hold, err := s.store.ChargeHeldTx(ctx, tx, locked.ID)
	if err != nil {
		return err
	}
	if state == domain.ChargeHoldLive && domain.OwnsChargeHold(ctx, locked.ID, hold.Until) {
		state = domain.ChargeHoldNone
	}
	if b := domain.PaymentBlocksActionHeld(locked, action, state); b.Blocked {
		return errs.InvalidState(b.Message).WithCode(b.Code)
	}
	return nil
}

// VoidUnchangedTx voids snapshot's invoice on the CALLER's transaction — the
// billing engine relieving a fully-unused unpaid prebill inside the cancel /
// plan-swap transaction, so the relief commits or rolls back with the change.
// The engine decided to void from snapshot; under the row lock the invoice
// must still be exactly that (status, payment state, amounts), else it returns
// an error and the caller's transaction rolls back — the retry re-reads and
// decides again (a payment that landed meanwhile turns the void into a credit
// note). After the caller commits it must call AfterVoid.
func (s *Service) VoidUnchangedTx(ctx context.Context, tx *sql.Tx, tenantID string, snapshot domain.Invoice) (domain.Invoice, error) {
	return s.store.UpdateStatusWithReversalPriorTx(ctx, tx, tenantID, snapshot.ID, domain.InvoiceVoided, func(tx *sql.Tx, locked domain.Invoice) error {
		if err := voidRefusal(locked); err != nil {
			return err
		}
		// A fully-unused prebill is voided on its period's first day, which is
		// when the first automatic charge fires: refuse while a charge holds
		// it (#975), so the cancel or plan swap rolls back and is retried.
		// A stale hold refuses too (ADR-127): the retry succeeds once the
		// stale-hold sweep has confirmed the lost charge with Stripe, up to
		// about an hour later (residual R3).
		if err := s.refuseWhileChargeHeld(ctx, tx, locked, domain.ActionVoid); err != nil {
			return err
		}
		if locked.Status != snapshot.Status || locked.PaymentStatus != snapshot.PaymentStatus ||
			locked.AmountDueCents != snapshot.AmountDueCents || locked.AmountPaidCents != snapshot.AmountPaidCents ||
			locked.TotalAmountCents != snapshot.TotalAmountCents {
			return errs.InvalidState(fmt.Sprintf("invoice %s changed while it was being voided; retry", snapshot.ID)).WithCode("invoice_changed")
		}
		return s.voidEffectsTx(ctx, tx, tenantID, locked, locked.Status)
	})
}

// voidEffectsTx is the in-tx half of a void, shared by Void and
// VoidUnchangedTx: hand back the consumed customer credits, retire a funded
// commit grant, and write the audit row — all on the void's transaction.
func (s *Service) voidEffectsTx(ctx context.Context, tx *sql.Tx, tenantID string, inv domain.Invoice, prior domain.InvoiceStatus) error {
	var creditsReversed, commitRetired int64
	if s.creditReverser != nil && inv.CustomerID != "" {
		reversed, rerr := s.creditReverser.ReverseForInvoiceTx(ctx, tx, tenantID, inv.CustomerID, inv.ID, inv.InvoiceNumber)
		if rerr != nil {
			return rerr
		}
		creditsReversed = reversed
		// ADR-078 D3: voiding a commit FUNDING invoice retires the funded
		// grant's remaining balance in this same tx (no-op for non-commit
		// invoices). Void is the ONLY retire trigger — uncollectible and
		// the dunning pause/cancel terminals leave the block live as a
		// collections stance, and voided→paid is rejected so a retired
		// grant can never be paid for afterwards. The consumed_cents CAS
		// inside makes the legal uncollectible→void sequence and retries
		// converge on one retirement.
		retired, rerr := s.creditReverser.RetireCommitGrantForInvoiceTx(ctx, tx, tenantID, inv.ID)
		if rerr != nil {
			return rerr
		}
		commitRetired = retired
	}
	// ADR-090 in-tx emission — one canonical void row for BOTH the
	// operator endpoint and engine-triggered voids (unpaid-prebill
	// relief). The reversal/retirement amounts ride the row so the ledger
	// effects of the void are readable without cross-referencing the ledger.
	if s.audit != nil {
		meta := map[string]any{
			"invoice_number":     inv.InvoiceNumber,
			"customer_id":        inv.CustomerID,
			"total_amount_cents": inv.TotalAmountCents,
			"currency":           inv.Currency,
			"status_before":      string(prior),
		}
		if creditsReversed > 0 {
			meta["credits_reversed_cents"] = creditsReversed
		}
		if commitRetired > 0 {
			meta["commit_retired_cents"] = commitRetired
		}
		if auditErr := s.audit.LogInTx(ctx, tx, audit.Entry{
			Action:        domain.AuditActionVoid,
			ResourceType:  "invoice",
			ResourceID:    inv.ID,
			ResourceLabel: inv.InvoiceNumber,
			Metadata:      meta,
		}); auditErr != nil {
			return fmt.Errorf("audit emission: %w", auditErr)
		}
	}
	return nil
}

// AfterVoid runs a committed void's post-commit legs, the same for every
// void: Void calls it itself, and a caller of VoidUnchangedTx (the cancel and
// plan-swap relief, Engine.FinishRelief) calls it once its transaction
// commits. Before 2026-10-22 the Stripe and dunning legs ran only in the two
// HTTP handlers, so a relief void left the invoice payable on a live
// Checkout session and its dunning run active until the run's next due tick.
//
// In order:
//  1. Stop collection at Stripe: expire the invoice's live Checkout
//     sessions, then cancel its PaymentIntent. Money first.
//  2. Send invoice.voided, with the row the void committed. It goes before
//     the tax leg, which stamps tax_reversed_at, so the event shows the
//     row as it was when Velox created the event (ADR-131).
//  3. Reverse the upstream tax.
//  4. Resolve the invoice's active dunning run as invoice_voided. A caller
//     that resolves the run itself afterwards (the dunning handler) loses
//     the resolve CAS and gets the resolved row back.
//
// Each leg is best-effort, and a crash before one loses nothing durable:
// the void's own transaction closed Velox's checkout claims, a Checkout
// session expires within an hour at Stripe, and a payment that still lands
// raises the paid-on-voided anomaly (ADR-068); RetryPendingTaxReversal
// re-drives the reversal (tax_reversed_at stays NULL); dunning's next tick
// on the run resolves it (processRun reads the voided invoice).
func (s *Service) AfterVoid(ctx context.Context, tenantID string, voided domain.Invoice) {
	// The void has committed, so every leg runs to the end even if the
	// caller's context ends now (an API timeout, a client disconnect, a
	// dunning tick stopping): the same rule as finishReliefThenCollect.
	ctx = context.WithoutCancel(ctx)
	if s.collectionStopper != nil {
		s.collectionStopper.StopCollection(ctx, tenantID, voided)
	}
	// invoice.voided webhook — single-writer at the service layer (same
	// pattern as finalized/marked_uncollectible/payment_recorded), so
	// engine-triggered voids notify integrators too.
	if s.events != nil {
		s.dispatchEvent(ctx, tenantID, domain.EventInvoiceVoided, domain.InvoiceWebhookData(voided, nil))
	}
	// Invoice-stable reversal reference (inv_taxrev_<id>). Void permits an
	// `uncollectible` source (annulling a bad debt is a legitimate operator
	// action), and MarkUncollectible already reversed the tax under THIS same
	// reference — so reusing it dedups at Stripe (same reference + idempotency
	// key) and the tax is reversed exactly once. Pre-fix this used inv_void_<id>,
	// a distinct reference that did NOT dedup → uncollectible-then-void reversed
	// the tax twice → tenant under-remitted (ADR-056 follow-up / product audit G1).
	// Best-effort post-commit: a failure here is logged ERROR and recovered by
	// RetryPendingTaxReversal, so the return is deliberately ignored.
	_ = s.reverseInvoiceTax(ctx, tenantID, voided, "inv_taxrev_"+voided.ID)
	if s.dunningResolver != nil {
		if err := s.dunningResolver.ResolveByInvoice(ctx, tenantID, voided.ID, domain.ResolutionInvoiceVoided); err != nil {
			slog.WarnContext(ctx, "resolve the voided invoice's dunning run failed; its next dunning tick resolves it",
				"invoice_id", voided.ID, "error", err)
		}
	}
}

// MarkUncollectible flips a finalized-but-unpaid invoice to
// status='uncollectible'. Stripe-standard semantics (ADR-036
// amendment): operator (or dunning's mark_uncollectible final action)
// acknowledges the receivable won't be collected; the invoice stays
// in financial reporting but no further collection is attempted.
// Distinct from Void, which annuls the invoice.
//
// Refuses paid (collected — credit note instead) and already
// uncollectible (idempotent) states. Voided is allowed to transition
// only one direction (voided is terminal), so this errors on it too.
func (s *Service) MarkUncollectible(ctx context.Context, tenantID, id string) (domain.Invoice, error) {
	ctx = s.bindForInvoice(ctx, tenantID, id)
	inv, err := s.store.Get(ctx, tenantID, id)
	if err != nil {
		return domain.Invoice{}, err
	}
	switch inv.Status {
	case domain.InvoicePaid:
		return domain.Invoice{}, errs.InvalidState("cannot mark paid invoice uncollectible — issue a credit note instead")
	case domain.InvoiceVoided:
		return domain.Invoice{}, errs.InvalidState("cannot mark voided invoice uncollectible — void is terminal")
	case domain.InvoiceUncollectible:
		return domain.Invoice{}, errs.InvalidState("invoice is already uncollectible")
	}
	// A charge in flight may still succeed. MarkPaid IS allowed from
	// uncollectible (the "wrote it off but paid after all" recovery), so if we
	// reverse tax + flip to uncollectible now and the charge then settles, the
	// tax for a real collected sale has been reversed → the tenant under-remits.
	// Block until terminal. Stripe enforces the same ("can't mark uncollectible
	// with open payments"). Guard sits BEFORE reverseInvoiceTax.
	// A PARKED invoice is the exception (ADR-107): payment_status='unknown' with
	// no PaymentIntent id means Stripe's answer was unclear and named no
	// payment. The stale-hold sweep finishes it within a day (REC-5), and
	// writing it off is the action for a customer who paid another way, or
	// for a park Stripe cannot be asked about (found by the honesty sweep,
	// 2026-07-31, when it had no exit at all).
	//
	// Writing it off is safe in a way voiding is not: it moves no money, and if
	// the charge did succeed the payment webhook or the stale-hold sweep still
	// marks the invoice paid, which audits recovered_from. Void
	// stays refused because a voided-then-succeeded invoice is a contradiction,
	// and RecordOfflinePayment stays refused because it would label a card
	// charge as out-of-band.
	if b := domain.PaymentBlocksAction(inv, domain.ActionMarkUncollectible); b.Blocked {
		return domain.Invoice{}, errs.InvalidState(b.Message).WithCode(b.Code)
	}

	// Re-run under the row lock, with the charge hold: the check above read
	// the invoice before the lock, and a charge can claim it in between
	// (#975). A parked invoice still passes (the hold is not consulted on
	// 'unknown'), so ADR-107's only exit stays open, and so does an invoice
	// whose hold is stale (ADR-127): writing off moves no money, and a payment
	// the stale-hold sweep finds later still settles it.
	updated, err := s.store.UpdateStatusWithReversalPrior(ctx, tenantID, id, domain.InvoiceUncollectible, func(tx *sql.Tx, locked domain.Invoice) error {
		return s.refuseWhileChargeHeld(ctx, tx, locked, domain.ActionMarkUncollectible)
	})
	if err != nil {
		return domain.Invoice{}, err
	}

	// Reverse the upstream tax transaction AFTER the status flip commits —
	// mirrors Void (ADR-056). Flipping first means a status-write failure leaves
	// the invoice 'finalized' with NO upstream reversal fired (no orphaned
	// Stripe side effect on a still-collectible invoice); a reversal failure
	// post-flip leaves the row 'uncollectible' with tax_reversed_at NULL, which
	// RetryPendingTaxReversal then recovers. Remaining un-reversed portion only
	// (see reverseInvoiceTax). Jurisdictional caveat — bad-debt VAT relief rules
	// vary (EU permits reclaim under specific conditions; US sales tax varies by
	// state). We follow Stripe's default behaviour and let tenants whose
	// jurisdiction requires the tax to stay re-commit manually. The reversal
	// NO TAX LEG (ADR-111). A write-off changes who PAID, not what was SOLD:
	// the service was delivered, the invoice was correct, and the tax was
	// correctly reported on accrual. The entry is Dr bad debt / Cr AR.
	//
	// Reversing here shipped a machine-driven under-remit with no human in it:
	// an ambiguous charge parks an invoice (ADR-107), write-off is its only
	// legal exit and the attention banner advises it, the reversal fires, then
	// the stale-hold sweep finds the charge succeeded at Stripe and
	// markPaidReportingTransition settles uncollectible -> paid — leaving a
	// PAID invoice whose tax is un-reported and nothing to re-report it. The
	// in-flight guard above catches only the payment already moving at
	// write-off time, not the one that arrives later.
	//
	// Recovering tax remitted on an uncollected debt is BAD-DEBT RELIEF: a
	// separate claim conditioned on facts Velox does not hold (UK: 6 months
	// overdue AND written off to a separate bad-debt account; CA/NY/TX: charged
	// off for federal income tax purposes). Claiming it at write-off time is
	// premature by construction. It is the tenant's claim to make.
	//
	// Void and credit note still reverse — see reverseInvoiceTax. That
	// asymmetry is the decision: they change what was supplied; this does not.

	// The write-off has committed, so every leg below runs to the end even if
	// the caller's context ends now (an API timeout, a client disconnect, a
	// dunning tick stopping), as in AfterVoid. The audit row and the event go
	// first: they are local writes, and the Stripe expiry after them can take
	// as long as Stripe does.
	ctx = context.WithoutCancel(ctx)

	if s.audit != nil {
		_ = s.audit.Log(ctx, tenantID, domain.AuditActionUpdate, "invoice", updated.ID, updated.InvoiceNumber, map[string]any{
			"action":           "marked_uncollectible",
			"customer_id":      updated.CustomerID,
			"amount_due_cents": updated.AmountDueCents,
			"currency":         updated.Currency,
		})
	}
	if s.events != nil {
		s.dispatchEvent(ctx, tenantID, domain.EventInvoiceMarkedUncollectible, domain.InvoiceWebhookData(updated, nil))
	}

	// Stop the invoice being payable on a Checkout page the customer still has
	// open: the flip above closed Velox's claims in its transaction, and this
	// expires the sessions at Stripe, the same call and handling a void uses
	// (AfterVoid). Best-effort: a session whose expiry fails ends at its
	// ExpiresAt (at most an hour after it opened); a payment through it
	// settles the invoice once.
	if s.collectionStopper != nil {
		s.collectionStopper.ExpireCheckoutSessions(ctx, tenantID, updated)
	}
	return updated, nil
}

// RecordOfflinePayment is the operator-driven Stripe-parity offline-
// recovery path. Lets the operator mark an unpaid invoice as paid
// without going through a PaymentIntent — for cheque, wire, cash,
// or any out-of-band collection. Mirrors Stripe's "Pay outside of
// Stripe" / paid_out_of_band=true affordance.
//
// Allowed source states: finalized (any payment_status that isn't
// already succeeded or in flight — i.e. not processing/unknown) AND
// uncollectible (the Stripe-parity recovery transition — "we wrote it
// off but the customer paid after all"). Rejects paid (idempotent —
// nothing to do), voided (terminal), and in-flight charges (an
// ambiguous "unknown" charge may have actually succeeded at Stripe, so
// recording an offline payment on top risks double-collection once the
// reconciler resolves it).
//
// note is a short operator memo persisted in the audit metadata
// (e.g. "Cheque #1234", "Wire 2026-05-20"). Not surfaced in
// customer-facing payloads. Empty string is permitted.
func (s *Service) RecordOfflinePayment(ctx context.Context, tenantID, id, note string) (domain.Invoice, error) {
	ctx = s.bindForInvoice(ctx, tenantID, id)
	inv, err := s.store.Get(ctx, tenantID, id)
	if err != nil {
		return domain.Invoice{}, err
	}
	switch inv.Status {
	case domain.InvoicePaid:
		return domain.Invoice{}, errs.InvalidState("invoice is already paid")
	case domain.InvoiceVoided:
		return domain.Invoice{}, errs.InvalidState("cannot record payment on a voided invoice")
	case domain.InvoiceDraft:
		return domain.Invoice{}, errs.InvalidState("finalize the invoice before recording a payment")
	}
	if b := domain.PaymentBlocksAction(inv, domain.ActionRecordOfflinePayment); b.Blocked {
		return domain.Invoice{}, errs.InvalidState(b.Message).WithCode(b.Code)
	}
	// BAD-DEBT RECOVERY WARNINGS. Deliberately WARN, never refuse — the money
	// already arrived. The governing rule for this whole area: refuse to CREATE
	// a bad money event (the card path gates these), never refuse to RECORD one
	// that already happened, because refusing only makes the books wrong too.
	//
	// But silence is not the alternative to refusal. Both conditions below mean
	// the recorded payment does not reconcile with what the tenant has told the
	// tax authority or already billed elsewhere, and nothing downstream will
	// notice: MarkPaid flips status to 'paid', which drops the invoice OUT of
	// the tax-reversal sweep's predicate (status IN ('voided','uncollectible')),
	// so the discrepancy becomes permanently invisible the instant it is
	// created.
	var unrelievedRelief int64
	if s.creditNotes != nil {
		if n, cerr := s.creditNotes.UnreliefedClawbackCents(ctx, tenantID, inv.ID); cerr == nil {
			unrelievedRelief = n
		}
	}
	if w := domain.RecoveryWarnsOnOfflinePayment(inv, unrelievedRelief); w != nil {
		slog.WarnContext(ctx, "offline payment recorded on a written-off invoice with an unreconciled consequence — "+w.Message,
			"invoice_id", inv.ID, "invoice_number", inv.InvoiceNumber, "code", w.Code,
			"amount_cents", inv.AmountDueCents, "currency", inv.Currency)
	}
	now := s.clock.Now(ctx)
	// MarkPaidOffline atomically flips status='paid',
	// payment_status='succeeded', amount_paid=amount_due, amount_due=0 — the
	// same end state as a successful engine charge, so reports, the customer
	// balance and dunning scans treat the recovery identically — and records
	// how the money came: paid_out_of_band, with no PaymentIntent (Stripe's
	// paid_out_of_band). Nothing was charged at Stripe, so there is no
	// PaymentIntent to record, and no card to refund.
	//
	// The payment refusal runs again under MarkPaidOffline's row lock, with
	// the charge hold: a charge claimed after the read above would otherwise
	// collect the same invoice a second time (#975).
	updated, err := s.store.MarkPaidOffline(ctx, tenantID, id, now,
		func(tx *sql.Tx, locked domain.Invoice) error {
			// A charge that settled after the read above leaves the row paid;
			// the paid transition would then be a quiet no-op, and the audit
			// row and invoice.payment_recorded below would record a payment
			// that never happened.
			if locked.Status == domain.InvoicePaid {
				return errs.InvalidState("invoice is already paid")
			}
			return s.refuseWhileChargeHeld(ctx, tx, locked, domain.ActionRecordOfflinePayment)
		})
	if err != nil {
		return domain.Invoice{}, err
	}
	// amount_cents is what the settle BOOKED — AmountPaidCents on the
	// post-transition row. MarkPaid flips amount_paid=amount_due and
	// amount_due=0 in the same statement, so reading AmountDueCents here
	// (the pre-fix bug) reported amount_cents: 0 on every offline payment
	// — the audit trail and the invoice.payment_recorded webhook both
	// told integrators a zero-value payment was recorded.
	if s.audit != nil {
		meta := map[string]any{
			"action":                "payment_recorded",
			"customer_id":           updated.CustomerID,
			"amount_cents":          updated.AmountPaidCents,
			"currency":              updated.Currency,
			"recovered_from_status": string(inv.Status),
		}
		// The operator's note is FREE TEXT from the request body, and audit_log is
		// append-only — 0150 revoked DELETE from the runtime role, so whatever lands
		// here is permanent and cannot be redacted. An unbounded field is therefore
		// an unbounded permanent liability: paste a customer's email, a card number
		// or a home address into "note" and it is in the compliance log forever.
		//
		// Bound it. 500 chars is generous for "cheque #1234, received 12 May" and
		// far too small to be a dumping ground, and the row records that the note
		// was truncated rather than silently keeping half a sentence.
		if note != "" {
			const maxNote = 500
			if len(note) > maxNote {
				meta["note"] = note[:maxNote]
				meta["note_truncated"] = true
			} else {
				meta["note"] = note
			}
		}
		_ = s.audit.Log(ctx, tenantID, domain.AuditActionUpdate, "invoice", updated.ID, updated.InvoiceNumber, meta)
	}
	if s.events != nil {
		// The paid invoice holds the amount (amount_paid_cents) and the time
		// (paid_at, which MarkPaidOffline set to now). The status it was paid
		// from is gone from it, so it rides beside it.
		s.dispatchEvent(ctx, tenantID, domain.EventInvoicePaymentRecorded, domain.InvoiceWebhookData(updated, map[string]any{
			"recovered_from": string(inv.Status),
		}))
	}
	return updated, nil
}

// RecordPayment and RecordPaymentFailure were DELETED here (ADR-107). They wrote
// payment_status with no state guard whatsoever and had zero production callers
// — they existed only to be wired up by someone later. Under ADR-107 the
// no-double-charge invariant rests on an invoice at payment_status='unknown'
// being unreachable by every writer that could make it claimable again, and
// these two were the easiest possible way to reopen that hole by accident:
// convenient names, obvious signatures, no guard. Anything that needs to record
// a payment outcome goes through the settle primitives, which carry the guards.

// ListChargeAttempts returns the invoice's ADR-102 charge-attempt
// facts, oldest first — the timeline's billing-axis payment rows.
func (s *Service) ListChargeAttempts(ctx context.Context, tenantID, invoiceID string) ([]domain.InvoiceChargeAttempt, error) {
	return s.store.ListChargeAttemptsByInvoice(ctx, tenantID, invoiceID)
}

func (s *Service) GetWithLineItems(ctx context.Context, tenantID, id string) (domain.Invoice, []domain.InvoiceLineItem, error) {
	inv, err := s.store.Get(ctx, tenantID, id)
	if err != nil {
		return domain.Invoice{}, nil, err
	}
	items, err := s.store.ListLineItems(ctx, tenantID, id)
	if err != nil {
		return domain.Invoice{}, nil, err
	}
	return s.attachAttention(ctx, inv), items, nil
}

// GetByPublicToken resolves a hosted-invoice-URL token to the invoice and
// its livemode. Exposed on the service so the public hosted-invoice
// handler can look up the invoice (and hence the owning tenant + mode)
// before any tenant context is available. Thin forward to the store —
// the store method is the one that uses TxBypass. The livemode return
// is what the handler uses to pin postgres.WithLivemode on the request
// context for every downstream RLS-scoped read.
func (s *Service) GetByPublicToken(ctx context.Context, token string) (domain.Invoice, bool, error) {
	inv, livemode, err := s.store.GetByPublicToken(ctx, token)
	if err != nil {
		return domain.Invoice{}, false, err
	}
	// Pin livemode on the ctx before attachAttention. attachAttention
	// reads payment_setups under TxTenant; without the pin it would
	// default to live and either return a stale-mode row or trip the
	// "livemode propagation missing" WARN. The hosted-invoice handler
	// pins again on the request ctx for its own downstream reads, but
	// service-level pinning here keeps callers (and the attention
	// classification specifically) honest regardless of who's calling.
	ctx = postgres.WithLivemode(ctx, livemode)
	return s.attachAttention(ctx, inv), livemode, nil
}

// SetPublicToken persists a rotated public_token on a non-draft invoice.
// Exposed on the service so the operator rotate-public-token endpoint
// (T0-17.4) can delegate without reaching past the service boundary.
func (s *Service) SetPublicToken(ctx context.Context, tenantID, invoiceID, token string) error {
	return s.store.SetPublicToken(ctx, tenantID, invoiceID, token)
}

type AddLineItemInput struct {
	Description     string `json:"description"`
	LineType        string `json:"line_type"`
	Quantity        int64  `json:"quantity"`
	UnitAmountCents int64  `json:"unit_amount_cents"`

	// CommitGrantedCents marks the line as a prepaid-commit purchase
	// (ADR-078): when the invoice finalizes, a credit block of this many
	// cents is granted (may differ from the line price — discounted
	// commits). add_on lines on manual invoices only, at most one per
	// invoice (invoice-level rules enforced in the store, in-tx).
	CommitGrantedCents *int64 `json:"commit_granted_cents,omitempty"`
	// CommitExpiresAt is the granted block's expiry; nil = never expires
	// (phase-1 default).
	CommitExpiresAt *time.Time `json:"commit_expires_at,omitempty"`
}

func (s *Service) AddLineItem(ctx context.Context, tenantID, invoiceID string, input AddLineItemInput) (domain.InvoiceLineItem, error) {
	ctx = s.bindForInvoice(ctx, tenantID, invoiceID)
	// Currency is left empty here — AddLineItemAtomic stamps it from the
	// invoice row it locks. The atomic-create path passes the resolved
	// currency instead, since the header isn't persisted yet.
	li, err := buildLineItem(input, "")
	if err != nil {
		return domain.InvoiceLineItem{}, err
	}
	// Compose-time expiry sanity (ADR-078): a commit whose expiry already
	// passed would grant dead-on-arrival credits. Checked against the
	// invoice's clock (bindForInvoice above) so clock-pinned customers
	// evaluate simulated time. Re-checked at finalize — the line may sit
	// in draft past its expiry.
	if li.CommitExpiresAt != nil && !li.CommitExpiresAt.After(s.clock.Now(ctx)) {
		return domain.InvoiceLineItem{}, errs.Invalid("commit_expires_at", "must be in the future")
	}
	item, _, err := s.store.AddLineItemAtomicAudited(ctx, tenantID, invoiceID, li, s.lineItemAddedEmission(ctx))
	return item, err
}

// lineItemAddedEmission builds the in-tx audit emission for POST
// /v1/invoices/{id}/line-items. The event is a MUTATION of the invoice (its
// totals are rewritten), so it rides the frozen action vocabulary as
// action=update on resource_type="invoice" with a metadata "action"
// discriminator — inventing a top-level "line_item_added" action string would
// break the dashboard's filter/badge maps, which key on the existing set.
// nil when no logger is wired (unit tests): the store then skips emission.
func (s *Service) lineItemAddedEmission(ctx context.Context) func(tx *sql.Tx, item domain.InvoiceLineItem, inv domain.Invoice) error {
	if s.audit == nil {
		return nil
	}
	return func(tx *sql.Tx, item domain.InvoiceLineItem, inv domain.Invoice) error {
		return s.audit.LogInTx(ctx, tx, audit.Entry{
			Action:        domain.AuditActionUpdate,
			ResourceType:  "invoice",
			ResourceID:    inv.ID,
			ResourceLabel: inv.InvoiceNumber,
			Metadata: map[string]any{
				"action":       "line_item_added",
				"description":  item.Description,
				"amount_cents": item.AmountCents,
				"quantity":     item.Quantity,
			},
		})
	}
}

// buildLineItem validates an AddLineItemInput and returns the domain line
// item with its amount computed. Shared by AddLineItem (incremental add) and
// Create (atomic create-with-lines) so both paths apply identical rules:
// description required, quantity > 0, unit_amount > 0, line_type one of
// add_on (the default), base_fee, usage. currency stamps the per-line currency column the store
// persists; pass "" when a downstream store call derives it from the
// invoice.
func buildLineItem(input AddLineItemInput, currency string) (domain.InvoiceLineItem, error) {
	desc := strings.TrimSpace(input.Description)
	if desc == "" {
		return domain.InvoiceLineItem{}, errs.Required("description")
	}
	if input.Quantity <= 0 {
		return domain.InvoiceLineItem{}, errs.Invalid("quantity", "must be greater than 0")
	}
	if input.UnitAmountCents <= 0 {
		return domain.InvoiceLineItem{}, errs.Invalid("unit_amount_cents", "must be greater than 0")
	}

	lineType := strings.TrimSpace(input.LineType)
	if lineType == "" {
		// add_on is the default for operator-added line items (one-off invoice
		// composer, ad-hoc charges).
		lineType = string(domain.LineTypeAddOn)
	}
	switch domain.InvoiceLineItemType(lineType) {
	case domain.LineTypeBaseFee, domain.LineTypeUsage, domain.LineTypeAddOn:
		// Quantity and unit amount are positive, so every line adds money.
		// A "discount" line would raise the total, and a "tax" line would
		// be taxed again at finalize, so neither is accepted. The DB CHECK
		// still allows both: rows written before this rule may exist.
	default:
		return domain.InvoiceLineItem{}, errs.Invalid("line_type", "must be one of: add_on, base_fee, usage")
	}

	amountCents := input.Quantity * input.UnitAmountCents
	// int64 overflow guard: quantity and unit price are individually
	// valid (> 0), but their product can wrap negative and PERSIST as a
	// negative line/total — a silently corrupted money document.
	// Quantity > 0 was enforced above, so the division is safe.
	if amountCents/input.Quantity != input.UnitAmountCents {
		return domain.InvoiceLineItem{}, errs.Invalid("amount", "quantity × unit_amount_cents overflows — split the charge across multiple line items")
	}

	// Commit purchase lines (ADR-078): line-local rules here; the
	// invoice-level rules (manual invoices only, at most one commit line
	// per invoice) are enforced in the store under the invoice row lock,
	// covering both this path's consumers (AddLineItemAtomic and
	// CreateWithLineItems).
	if input.CommitGrantedCents != nil {
		if *input.CommitGrantedCents <= 0 {
			return domain.InvoiceLineItem{}, errs.Invalid("commit_granted_cents", "must be greater than 0")
		}
		if *input.CommitGrantedCents > 100_000_000 { // matches the credit grant cap; raise on first DP ask (ADR-078)
			return domain.InvoiceLineItem{}, errs.Invalid("commit_granted_cents", "cannot exceed 1,000,000")
		}
		if domain.InvoiceLineItemType(lineType) != domain.LineTypeAddOn {
			return domain.InvoiceLineItem{}, errs.Invalid("commit_granted_cents", "commit purchase lines must be add_on lines")
		}
	} else if input.CommitExpiresAt != nil {
		return domain.InvoiceLineItem{}, errs.Invalid("commit_expires_at", "requires commit_granted_cents")
	}

	return domain.InvoiceLineItem{
		LineType:           domain.InvoiceLineItemType(lineType),
		Description:        desc,
		Quantity:           input.Quantity,
		UnitAmountCents:    input.UnitAmountCents,
		AmountCents:        amountCents,
		TotalAmountCents:   amountCents,
		Currency:           currency,
		CommitGrantedCents: input.CommitGrantedCents,
		CommitExpiresAt:    input.CommitExpiresAt,
	}, nil
}

// RetryTax routes a "Retry tax" action through the billing engine.
// Called from the operator-triggered HTTP handler, the background
// reconciler (RetryPendingTax), the test-clock catch-up and the two
// fan-outs (Stripe connect, billing-profile save). The engine owns the
// recompute and atomic persist; this service method layers
// auto-finalize on top: when the retry succeeds AND the invoice
// is engine-generated (billing_reason != "manual"), finalize in
// the same call. Manual invoices stay draft so the operator
// retains the explicit finalize step.
//
// Once the tax write commits, an engine draft with tax ok and no pause is
// owed a finalize, and no sweep lists it again: every tax sweep lists only
// pending tax. So everything after the tax write runs on a
// detached ctx. A SIGTERM or a lost lease cancels the tax_retry tick's ctx
// (main.go); a finalize on that ctx failed and left the period unissued for
// good. After a lease loss the new leader's sweep does not list this row
// (its tax is ok). A rival that finalizes first (an operator's Finalize, or
// a second RetryTax on the same row) wins the draft→finalized CAS; nothing
// is owed then, so that returns nil. For the same reason, a failed pause
// read or finalize is returned as an error naming the remedy, so the sweep
// counts it. A paused draft is legal and returns nil.
//
// Still uncovered: a hard process death in the few milliseconds between the
// tax commit and the finalize commit. That includes the connect fan-out
// goroutine, which shutdown does not wait for, so it is covered only for its
// own 5-minute timeout. It also includes a SIGTERM during the tax write's
// COMMIT round trip: pgx commits on the BeginTx ctx, so the commit can apply
// and still return context canceled, and RetryTax returns before the detach.
// The full fix finalizes inside UpdateTaxAtomic's tx.
//
// Returns the updated invoice with its Attention re-derived so the caller
// renders the new state immediately. Once the tax write has committed, an
// error comes back with the taxed draft, so the handler still audits the
// click's tax change.
func (s *Service) RetryTax(ctx context.Context, tenantID, invoiceID string) (domain.Invoice, error) {
	ctx = s.bindForInvoice(ctx, tenantID, invoiceID)
	if s.taxRetrier == nil {
		return domain.Invoice{}, errs.InvalidState("tax retry is not configured")
	}
	inv, err := s.taxRetrier.RetryTaxForInvoice(ctx, tenantID, invoiceID)
	if err != nil {
		return domain.Invoice{}, err
	}
	// The tax write has committed: the pause read, finalize and its tail, and
	// the $0 settle below must not stop on a cancelled ctx (see above).
	ctx = context.WithoutCancel(ctx)
	// A paused subscription keeps its invoices as drafts — that is the whole
	// meaning of pause_collection's keep_as_draft, and the engine holds the
	// same line at cycle close. Auto-finalize is the one path that ignored it,
	// because a tax-deferred draft is draft for two independent reasons and
	// resolving the tax only clears one of them. Issuing it here would hand
	// the operator an invoice they asked us not to send, and its banner would
	// promise an automatic charge that collection (correctly) refuses to make.
	// The draft stays a draft until an operator finalizes it: resuming
	// collection does not finalize it (Stripe: after a resume, "set
	// auto_advance=true on each draft invoice").
	if shouldAutoFinalizeAfterRetry(inv) {
		paused, perr := s.collectionPaused(ctx, tenantID, inv.SubscriptionID)
		if perr != nil {
			// Unknown is not "not paused": leave the draft (tax is already
			// persisted), and return the error like a failed auto-finalize below.
			return s.attachAttention(ctx, inv), fmt.Errorf("tax is calculated but the invoice was not finalized: %w; finalize it, or Retry tax after 24h", perr)
		}
		if paused {
			slog.Info("invoice: tax resolved but collection is paused on the subscription; leaving the invoice as a draft",
				"tenant_id", tenantID, "invoice_id", invoiceID, "subscription_id", inv.SubscriptionID)
			return s.attachAttention(ctx, inv), nil
		}
	}
	if shouldAutoFinalizeAfterRetry(inv) {
		final, ferr := s.finalize(ctx, tenantID, invoiceID, false) // the calc is seconds old
		if ferr != nil {
			// A rival finalized it first: the connect fan-out and the
			// tax_retry sweep can retry the same provider_not_configured
			// row at once, or an operator clicked Finalize. Nothing is
			// owed, so that is not a failure.
			if cur, gerr := s.store.Get(ctx, tenantID, invoiceID); gerr == nil && cur.Status != domain.InvoiceDraft {
				return s.attachAttention(ctx, cur), nil
			}
			// Tax already updated and persisted; finalize-side failure
			// does not unwind the recompute. No sweep lists a draft whose
			// tax is ok, so return the error: the sweep counts it, and
			// the operator's click fails.
			return s.attachAttention(ctx, inv), fmt.Errorf("tax is calculated but the invoice was not finalized: %w; finalize it, or Retry tax after 24h", ferr)
		}
		// Nothing owed after finalize (a $0 invoice): settle it now so the
		// operator who clicked Retry tax sees it paid. The auto-charge sweep
		// would settle it too (it is queued), just a tick later. Credits are
		// not applied to drafts, so a credit-covered invoice is not $0 here;
		// the collector applies the credits and settles it.
		if final.AmountDueCents <= 0 {
			now := s.clock.Now(ctx)
			paid, perr := s.store.MarkPaid(ctx, tenantID, invoiceID, "", now)
			if perr != nil {
				slog.Warn("invoice: auto-pay after tax-retry finalize failed; invoice stays finalized with $0 due",
					"error", perr, "tenant_id", tenantID, "invoice_id", invoiceID,
					"amount_due_cents", final.AmountDueCents)
				return s.attachAttention(ctx, final), nil
			}
			return s.attachAttention(ctx, paid), nil
		}
		// Still owed after finalize: nothing more to do here. The finalize
		// write queued the invoice for collection (auto_charge_pending), and
		// the auto-charge sweep is its collector: credits, the card, a
		// decline into dunning, or the setup-link email. This runs inside a
		// cron batch with no operator waiting, so there is no nudge; the
		// catchup's Phase 2 → Phase 3 order collects it in the same Advance.
		return s.attachAttention(ctx, final), nil
	}
	return s.attachAttention(ctx, inv), nil
}

// shouldAutoFinalizeAfterRetry encodes the gate for chaining a
// successful retry into Finalize. Both must be true:
//
//   - Tax is now resolved (status=ok). Pending/failed retries leave
//     the invoice draft regardless.
//   - Invoice came from the engine, not a manual draft. Manual
//     drafts can still be works-in-progress (operator may add line
//     items, edit memo, etc.); auto-finalize would surprise them.
//     billing_reason='manual' marks operator-created drafts;
//     subscription_cycle / threshold / proration / etc. all qualify
//     for auto-finalize.
//
// Subscription PaymentMethod readiness is intentionally not gated
// here — Finalize doesn't require a PM (PM matters for collection,
// which fires post-finalize via the auto-charge path).
func shouldAutoFinalizeAfterRetry(inv domain.Invoice) bool {
	if inv.Status != domain.InvoiceDraft {
		return false
	}
	if inv.TaxStatus != domain.InvoiceTaxOK {
		return false
	}
	if isOperatorComposed(inv) {
		return false
	}
	return true
}

// isOperatorComposed reports whether an invoice was created by an operator
// (the one-off composer / ad-hoc charge path) rather than the billing
// engine. billing_reason='manual' is the explicit marker Create stamps; the
// empty string covers legacy drafts written before that stamp existed.
// Engine invoices (subscription_cycle / create / cancel / threshold) return
// false. Drives finalize-time tax computation (manual invoices have no tax
// until finalize) and the auto-finalize-after-retry gate (manual drafts may
// still be works-in-progress, so we don't finalize them out from under the
// operator).
func isOperatorComposed(inv domain.Invoice) bool {
	return inv.BillingReason == "" || inv.BillingReason == domain.BillingReasonManual
}

// RetryProviderConfigErrors flushes every invoice in the (tenant,
// livemode) partition that's stuck on Stripe-configuration tax
// errors (provider_not_configured / provider_auth). Called by
// tenantstripe.Service.Connect on verify-success — when the
// operator just supplied fresh credentials, the system should
// catch up the work that was waiting on that signal. ADR-019.
//
// Reuses RetryTax per row, which already includes the auto-finalize
// chain (ADR-017): engine-generated invoices that recompute clean
// will move from draft → finalized in the same call. Manual drafts
// stay draft for explicit operator finalize.
//
// Errors are collected, not aborted-on. One bad row (e.g. concurrent
// operator click racing the same retry) shouldn't stall the rest of
// the flush.
func (s *Service) RetryProviderConfigErrors(ctx context.Context, tenantID string, livemode bool) (int, []error) {
	if s.taxRetrier == nil {
		return 0, nil
	}
	stuck, err := s.store.ListProviderConfigErrors(ctx, tenantID, livemode)
	if err != nil {
		return 0, []error{fmt.Errorf("list stuck provider-config invoices: %w", err)}
	}
	return s.retryStuck(ctx, tenantID, stuck)
}

// RetryCustomerDataErrors fans out a tax retry across every draft
// invoice for ONE customer stuck on `customer_data_invalid`. Fired
// from customer.Service.UpsertBillingProfile after a successful
// billing-profile save — when the operator fixes the customer's
// country / postal_code / state / tax_id, every invoice that was
// failing because of those fields auto-retries without per-invoice
// clicking. Same architectural shape as RetryProviderConfigErrors
// (which fires on Stripe-reconnect for provider-credential errors);
// scoped per-customer instead of per-tenant. ADR-019 sibling.
//
// Surgical-filter principle: only `customer_data_invalid` rows. Other
// codes (provider_outage, jurisdiction_unsupported, etc.) aren't
// resolved by a billing-profile change, so retrying them here would
// just burn `tax_retry_count` budget. The wall-clock scheduler's
// RetryPendingTax + the existing per-invoice operator Retry button
// cover those cases.
func (s *Service) RetryCustomerDataErrors(ctx context.Context, tenantID, customerID string) (int, []error) {
	if s.taxRetrier == nil {
		return 0, nil
	}
	stuck, err := s.store.ListCustomerDataInvalidErrors(ctx, tenantID, customerID)
	if err != nil {
		return 0, []error{fmt.Errorf("list customer-data-invalid invoices: %w", err)}
	}
	return s.retryStuck(ctx, tenantID, stuck)
}

// retryStuck is the shared per-row body of RetryProviderConfigErrors
// and RetryCustomerDataErrors. The candidate list shape differs by
// trigger; the per-invoice retry call is identical.
func (s *Service) retryStuck(ctx context.Context, tenantID string, stuck []domain.Invoice) (int, []error) {
	var (
		processed int
		errs      []error
	)
	for i, inv := range stuck {
		// Stop before the next row, as processTaxRetryBatch does. Unlike a
		// sweep, a fan-out runs once, and the tax_retry sweep skips a
		// customer_data_invalid row or one past its 8 tries. So the stop
		// (the connect fan-out's 5-minute timeout, or the request ending)
		// is reported.
		if err := ctx.Err(); err != nil {
			return processed, append(errs, fmt.Errorf("stopped before %d of %d invoices: %w", len(stuck)-i, len(stuck), err))
		}
		if _, retryErr := s.RetryTax(ctx, tenantID, inv.ID); retryErr != nil {
			errs = append(errs, fmt.Errorf("invoice %s: %w", inv.ID, retryErr))
			continue
		}
		processed++
	}
	return processed, errs
}

// CountProviderConfigErrors returns how many invoices would be
// retried by RetryProviderConfigErrors right now. Used by the
// connect handler to populate the response body so the dashboard
// can render "Retrying N stuck invoices" without waiting for the
// async fan-out to finish.
func (s *Service) CountProviderConfigErrors(ctx context.Context, tenantID string, livemode bool) (int, error) {
	stuck, err := s.store.ListProviderConfigErrors(ctx, tenantID, livemode)
	if err != nil {
		return 0, err
	}
	return len(stuck), nil
}

// RetryPendingTax is the background-reconciler entry. Scans
// invoices whose retryable-coded tax failure is due for another
// attempt, calls RetryTax for each (which may auto-finalize on
// success), returns counts for telemetry.
//
// Cross-tenant: each invoice carries its tenant_id, and RetryTax
// re-pins it on ctx through the engine's path. Bounded by `batch`
// per tick so a Stripe Tax outage that stuck thousands of
// invoices doesn't burn the entire scheduler tick on tax retries.
//
// Errors per invoice are collected, not aborted-on — one bad row
// (e.g. concurrent operator-driven Finalize racing the reconciler)
// shouldn't stall the rest of the batch.
func (s *Service) RetryPendingTax(ctx context.Context, batch int) (int, []error) {
	if s.taxRetrier == nil {
		return 0, nil
	}
	// The scan filter comes from the DECLARED single source
	// (domain.TaxRetryableErrorCodes — its doc comment says exactly
	// that), never a hand-rolled copy. The previous hardcoded list
	// here silently dropped provider_not_configured: the banner
	// promised "calculation will retry on the next scheduler tick"
	// (classified via the same domain list) while the queue never
	// contained the row — the ADR-019 post-connect flush was the only
	// thing papering over it, and a missed flush stranded the invoice
	// forever. Found live 2026-07-29: two scratch invoices sat through
	// twelve 5-minute ticks at retry_count=0. Retrying
	// provider_not_configured is pre-flight (no Stripe call, no
	// quota), and the connect-flush has no attempt cap, so exhaustion
	// never strands a late connect.
	codes := domain.TaxRetryableErrorCodes()
	maxAttempts := domain.MaxTaxRetryAttempts
	// Reconciler runs once per mode in the scheduler tick — pull
	// the mode off ctx and filter the SQL scan so this tick only
	// processes its own partition's invoices. Without the filter,
	// the cross-mode RLS-bypassed scan returns rows for both modes
	// and per-row RetryTax fails with "not found" on the half whose
	// livemode doesn't match ctx.
	livemode := postgres.Livemode(ctx)
	stuck, err := s.store.ListPendingTaxRetry(ctx, batch, codes, maxAttempts, livemode)
	if err != nil {
		return 0, []error{fmt.Errorf("list pending tax retries: %w", err)}
	}
	return s.processTaxRetryBatch(ctx, stuck)
}

// RetryPendingTaxCommit recovers the orphan state where CommitTax created the
// Stripe tax_transaction at finalize but the local tax_transaction_id write
// failed (or the process crashed between the two). Those invoices are
// finalized + tax_status=ok with a calculation id but no transaction id — tax
// was charged and reported upstream, but Velox can't reverse it on a later void
// (the reversal guard keys on tax_transaction_id). Re-committing is safe:
// CommitTax's idempotency key makes Stripe return the ORIGINAL transaction
// (never a duplicate), and CommitTax persists it. Self-healing — a re-commit
// whose persist fails again is simply picked up on the next tick until it
// sticks; once persisted the invoice falls out of the scan.
//
// Runs on the wall-clock scheduler tick (clock-pinned invoices are excluded by
// the store query). Per-row errors collected, not aborted-on.
func (s *Service) RetryPendingTaxCommit(ctx context.Context, batch int) (int, []error) {
	if s.taxCommitter == nil {
		return 0, nil
	}
	livemode := postgres.Livemode(ctx)
	orphans, err := s.store.ListPendingTaxCommit(ctx, batch, livemode)
	if err != nil {
		return 0, []error{fmt.Errorf("list pending tax commits: %w", err)}
	}
	var errsOut []error
	recovered := 0
	for _, inv := range orphans {
		if ctx.Err() != nil {
			return recovered, errsOut // see processTaxRetryBatch
		}
		if err := s.taxCommitter.CommitTax(ctx, inv.TenantID, inv.ID, inv.TaxCalculationID); err != nil {
			errsOut = append(errsOut, fmt.Errorf("re-commit tax for invoice %s: %w", inv.ID, err))
			continue
		}
		recovered++
	}
	return recovered, errsOut
}

// RetryPendingTaxReversal recovers voided/uncollectible stripe_tax invoices
// whose upstream tax reversal failed at status-change time (a transient Stripe
// error left the transaction reported as collected → the tenant over-remits).
// The symmetric sibling of RetryPendingTaxCommit. Each is re-driven through
// reverseInvoiceTax — idempotent at Stripe (the invoice-stable inv_taxrev_<id>
// reference dedups), and it stamps tax_reversed_at on success so the row falls
// out of the next scan. Self-healing: a re-reversal whose stamp fails again is
// picked up next tick.
//
// Runs on the wall-clock scheduler tick (clock-pinned invoices excluded by the
// store query). Per-row errors collected, not aborted-on.
func (s *Service) RetryPendingTaxReversal(ctx context.Context, batch int) (int, []error) {
	if s.taxReverser == nil {
		return 0, nil
	}
	livemode := postgres.Livemode(ctx)
	pending, err := s.store.ListPendingTaxReversal(ctx, batch, livemode)
	if err != nil {
		return 0, []error{fmt.Errorf("list pending tax reversals: %w", err)}
	}
	var errsOut []error
	recovered := 0
	for _, inv := range pending {
		if ctx.Err() != nil {
			return recovered, errsOut // see processTaxRetryBatch
		}
		// reverseInvoiceTax already logs an ERROR + leaves tax_reversed_at NULL
		// on failure (re-scanned next tick); aggregate the error for the return.
		if rerr := s.reverseInvoiceTax(ctx, inv.TenantID, inv, "inv_taxrev_"+inv.ID); rerr != nil {
			errsOut = append(errsOut, fmt.Errorf("re-reverse tax for invoice %s: %w", inv.ID, rerr))
			continue
		}
		recovered++
	}
	return recovered, errsOut
}

// RetryPendingTaxForClock is the catchup-path counterpart to
// RetryPendingTax. ADR-029 Phase 2: tax retries on clock-pinned
// invoices fire only when an operator advances the clock — the
// wall-clock cron's ListPendingTaxRetry filters them out via NOT
// EXISTS so the two paths process disjoint row sets.
//
// One retry per row per Advance (no backoff gate). Operator-friendly:
// each click does something visible. Faithful per-window retry-
// sequence simulation (Stripe-parity event-walking) is deferred —
// it's a niche use case operators don't typically run, and the
// trade-off "I clicked Advance and nothing happened" is a worse
// failure mode for the workflows operators actually exercise.
//
// Catchup-side ordering: this runs BEFORE the auto-charge phase so
// any invoices that get unstuck (tax_status: pending → ok) become
// charge-eligible in the same Advance click. Without this ordering,
// an operator who fixes the tax provider then clicks Advance would
// see the period-gen + auto-charge phases run, but the still-pending
// tax invoices wouldn't retry until the NEXT advance — confusing.
func (s *Service) RetryPendingTaxForClock(ctx context.Context, tenantID, clockID string, batch int) (int, []error) {
	if s.taxRetrier == nil {
		return 0, nil
	}
	// Same single source as the wall cron above — the catchup scan had
	// the identical hand-rolled list with the identical drift.
	codes := domain.TaxRetryableErrorCodes()
	maxAttempts := domain.MaxTaxRetryAttempts
	stuck, err := s.store.ListPendingTaxRetryForClock(ctx, tenantID, clockID, codes, maxAttempts, batch)
	if err != nil {
		return 0, []error{fmt.Errorf("list pending tax retries for clock %s: %w", clockID, err)}
	}
	return s.processTaxRetryBatch(ctx, stuck)
}

// processTaxRetryBatch is the per-row body of the cron-path
// RetryPendingTax. The catchup path (RetryPendingTaxForClock) has
// its own event-walking loop with per-row ctx-binding; the cron
// path doesn't need that since it's already wall-clock-driven.
func (s *Service) processTaxRetryBatch(ctx context.Context, stuck []domain.Invoice) (int, []error) {
	var (
		processed int
		errs      []error
	)
	for _, inv := range stuck {
		// Shutdown or a lost lease: stop before the next row. No error here,
		// as in processAutoCharge, so one stop is not counted as a failed
		// row per remaining row: the next tick lists them again. The same
		// check stops RetryPendingTaxCommit and RetryPendingTaxReversal;
		// retryStuck, which no tick repeats, reports its stop.
		if ctx.Err() != nil {
			return processed, errs
		}
		// RetryTax pins tenant via the engine entry point that
		// already does WithTenantID. We pass the per-row tenant
		// explicitly here rather than relying on ctx because this
		// loop is cross-tenant.
		if _, retryErr := s.RetryTax(ctx, inv.TenantID, inv.ID); retryErr != nil {
			errs = append(errs, fmt.Errorf("invoice %s: %w", inv.ID, retryErr))
			continue
		}
		processed++
	}
	return processed, errs
}
