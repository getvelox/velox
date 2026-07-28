package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/redis/go-redis/v9"

	"github.com/sagarsuperuser/velox/internal/analytics"
	mw "github.com/sagarsuperuser/velox/internal/api/middleware"
	"github.com/sagarsuperuser/velox/internal/api/respond"
	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/billing"
	"github.com/sagarsuperuser/velox/internal/credit"
	"github.com/sagarsuperuser/velox/internal/creditnote"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/dashmembers"
	"github.com/sagarsuperuser/velox/internal/dunning"
	"github.com/sagarsuperuser/velox/internal/email"
	"github.com/sagarsuperuser/velox/internal/hostedinvoice"
	"github.com/sagarsuperuser/velox/internal/integrations/litellm"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/payment"
	"github.com/sagarsuperuser/velox/internal/payment/breaker"
	"github.com/sagarsuperuser/velox/internal/paymentmethods"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/crypto"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/recipe"
	"github.com/sagarsuperuser/velox/internal/session"
	"github.com/sagarsuperuser/velox/internal/subscription"
	"github.com/sagarsuperuser/velox/internal/tax"
	"github.com/sagarsuperuser/velox/internal/tenant"
	"github.com/sagarsuperuser/velox/internal/tenantstripe"
	"github.com/sagarsuperuser/velox/internal/testclock"
	"github.com/sagarsuperuser/velox/internal/usage"
	"github.com/sagarsuperuser/velox/internal/user"
	"github.com/sagarsuperuser/velox/internal/webhook"

	"github.com/stripe/stripe-go/v82"
)

// --- Scheduler health tracking ---

var (
	schedulerMu       sync.RWMutex
	schedulerLastRun  time.Time
	schedulerInterval time.Duration

	registerQueueGaugesOnce sync.Once
)

// RecordSchedulerRun is called by the billing scheduler after each cycle
// so the health check can determine whether the scheduler is alive.
func RecordSchedulerRun() {
	schedulerMu.Lock()
	schedulerLastRun = time.Now()
	schedulerMu.Unlock()
	// Same fact, exported: the runbook's scheduler-liveness alert needs it
	// on /metrics, not only inside /health/ready.
	mw.RecordSchedulerTick()
}

// SetSchedulerInterval stores the configured scheduler interval so the
// health check knows when to flag it as degraded (>2x interval).
func SetSchedulerInterval(d time.Duration) {
	schedulerMu.Lock()
	schedulerInterval = d
	schedulerMu.Unlock()
}

type Server struct {
	router chi.Router

	// Exported for main.go to wire the billing scheduler + dunning
	BillingEngine    *billing.Engine
	DunningSvc       *dunning.Service
	SettingsStore    *tenant.SettingsStore
	WebhookOutSvc    *webhook.Service
	OutboxStore      *webhook.OutboxStore
	EmailOutboxStore *email.OutboxStore
	EmailSender      *email.Sender
	// EmailSettledChecker feeds the email dispatcher's staleness gate
	// (cmd/velox wires it via SetSettledChecker): action-required emails
	// whose invoice settled while queued are skipped, not delivered.
	EmailSettledChecker email.InvoiceSettledChecker
	CreditSvc           *credit.Service
	InvoiceSvc          *invoice.Service
	CreditNoteSvc       *creditnote.Service
	SubscriptionSvc     *subscription.Service
	TokenSvc            *payment.TokenService
	PaymentReconciler   *payment.Reconciler

	// TestClockSvc lets main.go wire the async catchup queue + worker
	// (per ADR-015 — Stripe-style async test-clock advance) and run
	// boot recovery for any clocks left in 'advancing' from a prior
	// process. Construction is here in router.go, lifecycle is in
	// main.go alongside the rest of the background workers.
	TestClockSvc *testclock.Service
}

func NewServer(db *postgres.DB, clk clock.Clock) *Server {
	if clk == nil {
		clk = clock.Real()
	}

	// Stores
	invoiceStore := invoice.NewPostgresStore(db)
	subStore := subscription.NewPostgresStore(db)
	usageStore := usage.NewPostgresStore(db)
	pricingStore := pricing.NewPostgresStore(db)
	webhookStore := payment.NewPostgresWebhookStore(db)

	// Auth
	authSvc := auth.NewService(auth.NewPostgresStore(db))
	authH := auth.NewHandler(authSvc)

	// Domain handlers
	tenantSvc := tenant.NewService(tenant.NewPostgresStore(db))
	tenantH := tenant.NewHandler(tenantSvc)
	customerStore := customer.NewPostgresStore(db)

	// PII + webhook-secret encryption at rest — AES-256-GCM via VELOX_ENCRYPTION_KEY.
	// Customer PII (email, names, phone, tax IDs), webhook signing secrets,
	// and per-tenant Stripe credentials all share the same encryptor so a
	// key rotation flows through uniformly. Without a key, they fall back
	// to plaintext — config.go already requires the key in production.
	var sharedEnc *crypto.Encryptor
	if encKey := strings.TrimSpace(os.Getenv("VELOX_ENCRYPTION_KEY")); encKey != "" {
		enc, err := crypto.NewEncryptor(encKey)
		if err != nil {
			slog.Error("invalid VELOX_ENCRYPTION_KEY, encryption at rest disabled", "error", err)
		} else {
			sharedEnc = enc
			customerStore.SetEncryptor(enc)
			invoiceStore.SetEncryptor(enc)
			slog.Info("encryption at rest enabled for customer PII, webhook secrets, Stripe credentials, and hosted-invoice tokens")
		}
	} else {
		slog.Warn("VELOX_ENCRYPTION_KEY not set — customer PII, webhook secrets, Stripe credentials, and hosted-invoice tokens stored in plaintext")
	}

	// Per-tenant Stripe credentials. Each tenant connects their own Stripe
	// account via POST /v1/settings/stripe; the service looks up the right
	// keys per request. There are no platform-level STRIPE_SECRET_KEY env
	// vars anymore — Velox is a billing engine, not a merchant of record.
	tenantStripeStore := tenantstripe.NewStore(db)
	if sharedEnc != nil {
		tenantStripeStore.SetEncryptor(sharedEnc)
	}
	tenantStripeSvc := tenantstripe.NewService(tenantStripeStore, func(secretKey string) *stripe.Client {
		return stripe.NewClient(secretKey)
	})
	tenantStripeH := tenantstripe.NewHandler(tenantStripeSvc)
	stripeClients := payment.NewStripeClients(tenantStripeSvc)

	// Payment methods service constructed early — the dunning
	// retrier, invoice handler, and billing engine all want a
	// payment-method-status read path that goes through canonical
	// sources (customers.stripe_customer_id + payment_methods)
	// rather than the deprecated customer_payment_setups summary.
	paymentMethodsStripe := paymentmethods.NewStripeAdapter(stripeClients, customerStore)
	paymentMethodsStore := paymentmethods.NewPostgresStore(db)
	paymentMethodsSvc := paymentmethods.NewService(paymentMethodsStore, paymentMethodsStripe, customerStore)
	// auditLogger is wired into paymentMethodsSvc later (after
	// auditLogger is constructed) — see the SetAuditLogger call below.
	// Setup-session return URL base: the SPA host where the customer
	// lands after Stripe Checkout. Production reads CUSTOMER_PORTAL_URL;
	// local dev falls through to http://localhost:5173 (the Vite dev
	// server). Without this set, a deployment behind a different host
	// would redirect customers back to localhost — broken UX in prod.
	paymentMethodsSvc.SetPortalBaseURL(strings.TrimSpace(os.Getenv("CUSTOMER_PORTAL_URL")))
	// compositePaymentSetupStore presents the customer_payment_setups
	// wire shape (domain.CustomerPaymentSetup) on top of canonical
	// sources. Lets payment/checkout.go + payment/stripe.go +
	// dunning retrier keep their existing PaymentSetupStore-shaped
	// wiring while structurally not reading the deprecated table.
	paymentSetupStore := &compositePaymentSetupStore{customers: customerStore, pms: paymentMethodsSvc}

	pricingSvc := pricing.NewService(pricingStore)
	// ADR-034: plan billing-fields freeze once any live sub references
	// the plan. UpdatePlan needs to count live subs; subStore implements
	// the narrow CountLiveSubsByPlan query.
	pricingSvc.SetSubscriptionPlanUsageReader(subStore)
	customerSvc := customer.NewService(customerStore)
	customerSvc.SetStripeSyncer(payment.NewStripeBillingSync(stripeClients), customerStore)
	customerH := customer.NewHandler(customerSvc)
	pricingH := pricing.NewHandler(pricingSvc)
	subSvc := subscription.NewService(subStore, clk)
	subH := subscription.NewHandler(subSvc)
	// Proration deps are wired below after creditSvc + invoiceStore are available
	usageSvc := usage.NewService(usageStore)
	usageH := usage.NewHandler(usageSvc, customerStore, pricingSvc)
	customerUsageSvc := usage.NewCustomerUsageService(usageSvc, customerStore, subStore, pricingSvc)
	customerUsageH := usage.NewCustomerUsageHandler(customerUsageSvc)
	// Provider cost rates + margin (ADR-079).
	providerCostH := usage.NewProviderCostHandler(usageStore, usage.NewMarginAssembler(usageStore, customerUsageSvc))
	settingsStore := tenant.NewSettingsStore(db)
	// Wire tenant settings into subscription so period boundaries snap
	// to start-of-day in the tenant's timezone (day-grade calendar
	// billing — Chargebee / Lago default).
	subSvc.SetSettingsReader(settingsStore)
	// Customer-level test-clock attach (ADR-027): subscription create
	// reads the owning customer's test_clock_id and stamps it on the
	// new sub. The API does not accept a per-sub test_clock_id —
	// Stripe doesn't either, and accepting one created a redundant
	// validation path against the canonical customer-level value.
	subSvc.SetCustomerReader(customerStore)
	subSvc.SetProfileReader(customerStore)
	// Yearly-aware period anchoring (Bug #10) — subscription Service
	// reads the first item's plan to determine BillingInterval at sub
	// lifecycle entry points (Create / Activate / EndTrial /
	// ExtendTrial). Without this, the period helpers hardcode
	// monthly math and a yearly + trial sub gets a 1-month first
	// cycle instead of a full year.
	subSvc.SetPlanReader(pricingStore)
	creditStore := credit.NewPostgresStore(db)
	creditSvc := credit.NewService(creditStore)
	creditH := credit.NewHandler(creditSvc)
	creditNoteStore := creditnote.NewPostgresStore(db)

	// Wire proration dependencies for plan change invoicing
	subH.SetProrationDeps(pricingSvc, &prorationInvoiceCreatorAdapter{store: invoiceStore, numberer: settingsStore}, &prorationCreditGranterAdapter{svc: creditSvc})

	// Payment / webhook / checkout / refund handlers
	stripeRefunder := payment.NewStripeRefunder(stripeClients)
	creditNoteSvc := creditnote.NewService(creditNoteStore, invoiceStore, stripeRefunder, &creditGrantAdapter{svc: creditSvc})
	creditNoteSvc.SetNumberGenerator(settingsStore)
	creditNoteH := creditnote.NewHandler(creditNoteSvc, creditnote.HandlerDeps{
		Customers: customerStore,
		Settings:  settingsStore,
		Invoices:  invoiceStore,
	})
	webhookOutStore := webhook.NewPostgresStore(db)
	if sharedEnc != nil {
		webhookOutStore.SetEncryptor(sharedEnc)
	}
	webhookOutSvc := webhook.NewService(webhookOutStore, nil)
	webhookOutH := webhook.NewHandler(webhookOutSvc)

	// Transactional outbox for outbound events (RES-1). Producers persist an
	// event intent to webhook_outbox inside the business-op transaction; the
	// dispatcher worker (cmd/velox/main.go) drains the queue and calls
	// Service.Dispatch for each row. Always-on per ADR-040 — the legacy
	// direct-dispatch fallback (VELOX_WEBHOOK_OUTBOX_ENABLED=false) was cut
	// 2026-05-30 since it voided every retry guarantee and was unreachable
	// by any operator pressure.
	outboxStore := webhook.NewOutboxStore(db)
	// Queue-depth gauges (Grafana/runbook alert on these): counts run on a
	// BYPASS tx — they are cross-tenant, and the app role's RLS would
	// report a healthy-looking 0 forever. registerQueueGaugesOnce guards
	// duplicate promauto registration when NewServer runs twice in-process
	// (integration tests).
	registerQueueGaugesOnce.Do(func() {
		mw.RegisterQueueDepthGauges(func(query string) (float64, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
			if err != nil {
				return 0, err
			}
			defer postgres.Rollback(tx)
			var n float64
			if err := tx.QueryRowContext(ctx, query).Scan(&n); err != nil {
				return 0, err
			}
			return n, nil
		})
	})
	eventDispatcher := webhook.NewOutboxDispatcher(outboxStore)
	// invoice.paid is emitted transactionally from MarkPaid (atomic with the
	// finalized→paid transition) so it fires exactly once across every
	// settlement path. Wire the outbox into the invoice store for that.
	invoiceStore.SetOutboxEnqueuer(outboxStore)
	// credit.balance_low / _depleted / _recovered are emitted transactionally
	// from every ledger-writing tx (ADR-078) — same outbox discipline.
	creditStore.SetOutboxEnqueuer(outboxStore)
	// subscription.created / .activated / .canceled / .trial_ended are
	// enqueued IN their transition txs (DispatchTx subscription subset,
	// 2026-07-05): the store's transition writers are the single emit site,
	// covering the operator API, the engine's schedule paths, and the trial
	// sweeps at once — a crash after commit can no longer silently drop a
	// lifecycle event a provisioning webhook consumer depends on.
	subStore.SetOutboxEnqueuer(outboxStore)
	// Commit funding at finalize (ADR-078 D2): FinalizeWithDates grants the
	// commit block in the finalize coordinator tx via credit.Service.
	invoiceStore.SetCommitFunder(creditSvc)
	subH.SetEventDispatcher(eventDispatcher)
	// Trial-expiry phases (catchup orchestrator + wall-clock cron)
	// fire subscription.trial_ended via the subscription Service —
	// matches the engine auto-flip path so webhook consumers see one
	// event per trial transition regardless of which path activated
	// the sub.
	subSvc.SetEventDispatcher(eventDispatcher)
	auditLogger := audit.NewLogger(db)
	// actor_name is resolved by LEFT JOIN at read time, and customers.display_name
	// is encrypted at rest — so without this, every customer-actor row (a
	// hosted-invoice Pay click, a payment-update link) rendered its Actor as
	// CIPHERTEXT in production: on the dashboard and in the CSV handed to an
	// auditor. Nil-safe; a no-op when VELOX_ENCRYPTION_KEY is unset.
	auditLogger.SetEncryptor(sharedEnc)
	auditH := audit.NewHandler(auditLogger)
	// Wire audit logging on the customer handler — currently used to
	// record cost-dashboard token rotations without leaking the
	// plaintext token into the audit trail.
	customerH.SetAuditLogger(auditLogger)
	// Public cost-dashboard projection assembler. Resolves token →
	// customer, composes the usage view, sanitizes the envelope.
	// Wired here so customerH can serve both the operator rotate
	// endpoint and the public GET-by-token route. See ADR-031 +
	// MANUAL_TEST FLOW CU8.
	costDashboardSvc := usage.NewCostDashboardAssembler(customerStore, customerUsageSvc, subStore)
	customerH.SetCostDashboardService(costDashboardSvc)
	if base := strings.TrimSpace(os.Getenv("VELOX_API_BASE_URL")); base != "" {
		customerH.SetAPIBaseURL(base)
	}
	settingsH := tenant.NewSettingsHandler(settingsStore)
	// Field-level settings-change audit (which fields, before/after). This is the
	// ONLY record of a settings change — the catch-all that used to log "a PUT
	// happened" is gone (ADR-090); the boot gate below is what guarantees this
	// wiring is never dropped.
	settingsH.SetAuditLogger(auditLogger)
	stripeClient := payment.NewLiveStripeClient(stripeClients)
	dunningStore := dunning.NewPostgresStore(db)
	dunningSvc := dunning.NewService(dunningStore, nil, clk) // retrier set below after stripeAdapter created
	dunningH := dunning.NewHandler(dunningSvc, dunning.HandlerDeps{
		PaymentCancel: payment.NewLiveStripeClient(stripeClients),
	})

	// Recipe registry — built-in pricing recipes loaded once at boot from
	// the embedded YAML. Failure here is fatal: a malformed recipe would
	// crash on first instantiate, and TestLoad already gates this in CI,
	// so reaching this panic implies a broken build that should not run.
	recipeRegistry, err := recipe.Load()
	if err != nil {
		panic(fmt.Sprintf("load recipe registry: %v", err))
	}
	recipeStore := recipe.NewPostgresStore(db)
	recipeSvc := recipe.NewService(db, recipeStore, recipeRegistry, pricingSvc, dunningSvc, webhookOutSvc)
	recipeH := recipe.NewHandler(recipeSvc)

	// Global circuit breaker around Stripe calls. When Stripe is unhealthy
	// (5xx/timeout/network), every caller backs off together so we don't
	// pile retries onto a struggling dependency. Opens after 5 consecutive
	// Unknown failures, probes after 30s, and emits state transitions to
	// the velox_stripe_breaker_state gauge so operators can alert on it.
	stripeBreaker := breaker.New(breaker.Config{
		FailureThreshold: 5,
		Cooldown:         30 * time.Second,
		Interval:         60 * time.Second,
		Countable:        payment.IsUnknownPaymentFailure,
		OnStateChange: func(_, to breaker.State) {
			mw.RecordStripeBreakerState(string(to))
		},
	})

	stripeAdapter := payment.NewStripe(stripeClient, invoiceStore, webhookStore, paymentSetupStore, dunningSvc)
	stripeAdapter.SetCardFetcher(stripeClient)
	stripeAdapter.SetEventDispatcher(eventDispatcher)
	// Durable payment-anomaly marker (ADR-068): the settle path stamps
	// duplicate-charge / amount-mismatch / paid-voided anomalies onto the
	// invoice row; the dashboard attention banner reads them (Critical,
	// pierces the terminal-state early-return).
	stripeAdapter.SetAnomalyRecorder(invoiceStore)
	// Claim-ledger truth-sync (checkout.session.completed) + settle-time
	// best-effort Stripe expire of still-live sessions (ADR-068).
	checkoutSessionStore := payment.NewCheckoutSessionStore(db)
	stripeAdapter.SetCheckoutSessionStore(checkoutSessionStore, stripeClients)
	stripeAdapter.SetBreaker(stripeBreaker)
	// Reconcile async refund outcomes (pending→succeeded/failed) from refund
	// webhooks onto the credit note — the create-call only sees the initial state.
	stripeAdapter.SetRefundStatusUpdater(creditNoteSvc)
	// Resolve an active dunning run promptly when a card payment settles (the
	// background-settle paths already resolve via the engine; this closes the
	// card-success symmetry — #317 follow-up). dunningSvc implements ResolveByInvoice.
	stripeAdapter.SetDunningResolver(dunningSvc)

	// Wire payment retrier now that stripeAdapter exists
	dunningSvc.SetRetrier(&paymentRetrierAdapter{
		charger:       stripeAdapter,
		invoiceStore:  invoiceStore,
		paymentSetups: paymentSetupStore,
		credits:       creditSvc,
	})
	// Pre-invoiceSvc dunning wires (pauser + canceler only depend on
	// subscription.Service + invoice store, both defined above).
	dunningSvc.SetSubscriptionPauser(&subscriptionPauserAdapter{svc: subSvc}, invoiceStore)
	dunningSvc.SetSubscriptionCanceler(&subscriptionCancelerAdapter{svc: subSvc})
	dunningSvc.SetEventDispatcher(eventDispatcher)
	// Customer→dunning_policy_id resolver so dunning service can pick
	// the effective policy at StartDunning time (ADR-036).
	dunningSvc.SetCustomerPolicyReader(customerSvc)
	// customer service emits customer.email_bounced when T0-20 bounce
	// reporting fires; needs the same webhook dispatcher as the other
	// domain services.
	customerSvc.SetEventDispatcher(eventDispatcher)
	webhookH := payment.NewHandler(stripeAdapter, tenantStripeSvc)

	invoiceSvc := invoice.NewService(invoiceStore, clk, settingsStore)
	// Mark-uncollectible adapter (ADR-036 amendment) — Stripe-standard
	// dunning terminal action. Wires here because it depends on the
	// invoice service which is defined just above; other dunning
	// adapters (pauser, canceler) wire earlier next to dunningSvc.
	dunningSvc.SetInvoiceUncollectibleMarker(&invoiceUncollectibleAdapter{svc: invoiceSvc})
	// Route the dunning manual-resolve void through the invoice service (single
	// void writer: guards + in-flight block + tax reversal + invoice.voided
	// event) instead of the raw store. Same lazy wire as the marker above.
	dunningH.SetInvoiceVoider(invoiceSvc)
	// Wire the post-connect tax-retry hook (ADR-019). When an
	// operator (re)connects Stripe in Settings → Payments, the
	// tenantstripe service fans out a goroutine that flushes any
	// invoices stuck on provider_not_configured / provider_auth.
	tenantStripeSvc.SetStuckRetrier(invoiceSvc)
	invoiceH := invoice.NewHandler(invoiceSvc, customerStore, settingsStore, invoice.HandlerDeps{
		CreditNotes:     &creditNoteListerAdapter{svc: creditNoteSvc},
		Charger:         stripeAdapter,
		PaymentSetups:   paymentSetupStore,
		PaymentCancel:   stripeClient,
		Dunning:         dunningSvc,
		WebhookEvents:   webhookStore,
		DunningTimeline: &dunningTimelineAdapter{store: dunningStore},
		Events:          eventDispatcher,
		RefundIssuer:    &refundIssuerAdapter{svc: creditNoteSvc},
	})
	// Return URLs are passed per-request now (return_url body field)
	// instead of read from env — every Checkout flow in the codebase
	// is contextual, not global. STRIPE_CHECKOUT_{SUCCESS,CANCEL}_URL
	// were a pre-portal anti-pattern.
	checkoutH := payment.NewCheckoutHandler(stripeClients, paymentSetupStore)

	// Token service for public payment update links. Used by the
	// no-PM-at-finalize email (noPaymentMethodNotifierAdapter) and
	// the public token-authenticated payment-update SPA flow. The
	// post-decline path (Stripe.handlePaymentFailed) does NOT mint a
	// single-use token — it points the customer at the long-lived
	// hosted invoice URL where they can update PM and retry, matching
	// the dunning email path. ADR-023.
	tokenSvc := payment.NewTokenService(db)

	// Reconciler for PaymentUnknown invoices (ambiguous Stripe outcomes).
	// 60s cool-off lets webhooks resolve the state naturally before we
	// spend an extra API call.
	paymentReconciler := payment.NewReconciler(stripeClient, invoiceStore, 60*time.Second)
	paymentReconciler.SetBreaker(stripeBreaker)
	// ADR-049 Phase 2: route recovered terminals through the shared settlement
	// primitive (so a dropped-webhook recovery dunns + notifies + emits the
	// event, not just flips payment_status), and sweep stale 'processing'
	// invoices in addition to 'unknown' as the dropped-webhook backstop. The
	// 30m processing cool-off keeps the webhook winning the common race (a
	// healthy card PI settles in seconds) while resolving a genuinely-dropped
	// webhook within ~one extra tick.
	paymentReconciler.SetSettler(stripeAdapter)
	paymentReconciler.SetProcessingReconcileAfter(30 * time.Minute)
	// Engine isn't constructed yet at this point — paymentReconciler's
	// resolver is wired below at line ~640 alongside dunningSvc and
	// stripeAdapter. Keep this declaration near the constructor; the
	// resolver-wiring block is the canonical place to find clock.Resolver
	// hookups across the router.
	publicPaymentH := payment.NewPublicPaymentHandler(tokenSvc, db, stripeClients, customerSvc, paymentMethodsStripe,
		strings.TrimSpace(os.Getenv("PAYMENT_UPDATE_RETURN_URL")))

	// paymentmethods.StripeAdapter constructed early so the hosted-invoice
	// Pay flow can use it to ensure a Stripe customer exists for the Velox
	// customer before creating the Checkout session — required for
	// `customer` + `setup_future_usage=off_session` to actually save the
	// PM to the customer's Stripe record (otherwise it's a guest checkout
	// and "save card" is a no-op). The store + service are constructed
	// later (line ~657) where the rest of the customer-portal stack lives.
	// paymentMethodsStripe + Store + Svc constructed earlier (right
	// after stripeClients) so the dunning retrier, invoice handler,
	// and billing engine can all use the canonical payment-status
	// view (compositePaymentSetupStore) without dual-table reads.

	// Hosted invoice page — Stripe-equivalent hosted_invoice_url surface.
	// Public tokenized routes that the partner's end customer visits from
	// an email (T0-17). The Stripe adapter picks live/test keys based on
	// the invoice row's livemode; HOSTED_INVOICE_BASE_URL is the public
	// SPA origin the Checkout success/cancel URLs redirect back to. In
	// dev, leave empty and the handler falls back to http://localhost:5173
	// so `make dev` works without extra config.
	// Constructed as a named var (not an inline literal) so the ADR-090
	// boot gate below can verify its audit emitter is wired.
	hostedInvoiceStripe := &hostedInvoiceStripeAdapter{clients: stripeClients, ensurer: paymentMethodsStripe, sessions: checkoutSessionStore, audit: auditLogger}

	hostedInvoiceH := hostedinvoice.New(hostedinvoice.Deps{
		Invoices:    invoiceSvc,
		Customers:   customerStore,
		Settings:    settingsStore,
		CreditNotes: &creditNoteListerAdapter{svc: creditNoteSvc},
		Stripe:      hostedInvoiceStripe,
		BaseURL:     strings.TrimSpace(os.Getenv("HOSTED_INVOICE_BASE_URL")),
	})

	// Email sender. Producers always enqueue into email_outbox via
	// OutboxSender; the dispatcher worker (cmd/velox/main.go) drains the
	// queue and invokes the direct *email.Sender below for the actual
	// SMTP round-trip. This makes email delivery durable across crashes
	// and transient SMTP failures, and gives operators a DLQ to inspect.
	// The legacy direct-SMTP-from-producer path (VELOX_EMAIL_OUTBOX_ENABLED
	// =false) was cut 2026-05-30 per ADR-040 — same shape as the webhook
	// outbox cut above.
	emailSender := email.NewSender()
	// Wire tenant settings so customer-facing emails pull per-tenant
	// branding (logo, brand color, company name, support URL). Cold-start
	// tenants without settings gracefully fall back to Velox defaults.
	emailSender.SetSettingsGetter(settingsStore)
	// Loud startup log so operators don't run prod without SMTP and
	// only discover it when a customer never receives their invoice.
	// Sender returns ErrSMTPNotConfigured per send when unconfigured —
	// every customer-facing email producer surfaces that error in its
	// own request handler / job log. This boot-time line sits above
	// the per-request volume so the misconfiguration is unmissable.
	if !emailSender.IsConfigured() {
		slog.Warn("SMTP NOT CONFIGURED — every customer-facing email (invoices, receipts, dunning, payment-setup links, password resets, team invites) will fail with ErrSMTPNotConfigured. Set SMTP_HOST + credentials. See docs/ops/email-setup.md.")
	} else {
		slog.Info("SMTP configured", "host", emailSender.SMTPHost())
	}
	// HOSTED_INVOICE_BASE_URL is the public origin the email "View & pay
	// invoice" / "View receipt" CTAs link at (resolves to /invoice/<token>
	// hosted page). When unset, the templates render NO link in the CTA —
	// the customer receives an email that looks broken. Same loud-boot
	// shape as the SMTP/PORTAL/PAYMENT_UPDATE warns so this can't quietly
	// ship to production.
	hostedInvoiceBaseURL := strings.TrimSpace(os.Getenv("HOSTED_INVOICE_BASE_URL"))
	if hostedInvoiceBaseURL == "" {
		slog.Warn("HOSTED_INVOICE_BASE_URL NOT SET — invoice / receipt / dunning / payment-failed emails will render with NO payment link in the CTA. Set this to your hosted-invoice page origin (e.g. https://billing.example.com).")
	}
	// bounceReporterAdapter wires Sender SMTP 5xx errors to
	// customer.MarkEmailBounced. Requires an email blinder (configured
	// later from VELOX_EMAIL_BIDX_KEY); if the blinder is missing, the
	// adapter short-circuits and the partner stays on pre-T0-20 behavior
	// (bounces logged, customer.email_status stays 'unknown').
	// Attached inside the blinder branch below once emailBlinder exists.
	emailOutboxStore := email.NewOutboxStore(db)
	// Surface customer-notification email rows on the invoice timeline.
	// Without this wiring, the timeline still renders, but operators
	// have no signal that the customer was actually notified about
	// no_payment_method / payment_failed / dunning events.
	invoiceH.SetEmailEvents(&invoiceEmailEventsAdapter{store: emailOutboxStore})
	// Sent-emails lister for the "Sent emails" section on the customer
	// detail page (Stripe shape — docs.stripe.com/invoicing/send-email
	// lists email log on the customer page, 30-day window).
	customerH.SetSentEmailsLister(&customerSentEmailsAdapter{store: emailOutboxStore})
	// Postmark Delivery/Bounce/SpamComplaint ingestion (ADR-098). One
	// platform-level route (a single Postmark account serves all
	// tenants — unlike Stripe BYOS there is no per-tenant secret);
	// tenant + livemode resolve from the stamped outbox row. Suppressor
	// attaches inside the blinder branch below.
	postmarkWebhookUser := strings.TrimSpace(os.Getenv("POSTMARK_WEBHOOK_USER"))
	postmarkWebhookPass := strings.TrimSpace(os.Getenv("POSTMARK_WEBHOOK_PASS"))
	postmarkH := email.NewPostmarkHandler(emailOutboxStore, postmarkWebhookUser, postmarkWebhookPass)
	if postmarkWebhookUser == "" || postmarkWebhookPass == "" {
		slog.Warn("POSTMARK_WEBHOOK_USER/POSTMARK_WEBHOOK_PASS not set — /v1/webhooks/postmark rejects all posts (401); delivery/bounce webhooks will not ingest")
	}
	// One OutboxSender wired everywhere. The seven typed interface vars
	// below are the per-domain views that satisfy each consumer's narrow
	// surface; concretely they're all the same OutboxSender instance.
	// emailOutboxSenderRef is the shared reference the blinder-wiring
	// block further below attaches the suppression-checker to.
	outboxSender := email.NewOutboxSender(emailOutboxStore)
	emailOutboxSenderRef := outboxSender
	var (
		invoiceEmail       invoice.EmailSender        = outboxSender
		dunningEmail       dunning.EmailNotifier      = outboxSender
		receiptEmail       payment.EmailReceipt       = outboxSender
		paymentSetupEmail  paymentSetupEmailSender    = outboxSender
		paymentFailedEmail payment.EmailPaymentFailed = outboxSender
		passwordResetEmail interface {
			SendPasswordReset(ctx context.Context, tenantID, to, displayName, resetURL string) error
		} = outboxSender
		setupLinkEmail paymentmethods.SetupLinkEmailer = outboxSender
	)
	invoiceH.SetEmailSender(invoiceEmail)
	customerEmailAdapter := &customerEmailFetcherAdapter{store: customerStore}
	dunningSvc.SetEmailNotifier(dunningEmail)
	dunningSvc.SetCustomerEmailFetcher(customerEmailAdapter)
	stripeAdapter.SetEmailReceipt(receiptEmail, customerEmailAdapter)
	paymentUpdateURL := strings.TrimSpace(os.Getenv("PAYMENT_UPDATE_URL"))
	if paymentUpdateURL == "" {
		slog.Warn("PAYMENT_UPDATE_URL NOT SET — payment-setup-request emails (no-PM-at-finalize) will fail at send time. Set this to your customer-portal payment-update page URL.")
	}
	stripeAdapter.SetEmailPaymentFailed(paymentFailedEmail, customerEmailAdapter)

	// Audit logging for financial operations
	// credit: in-tx emission on the SERVICE (ADR-090 first domain batch) —
	// grant/adjust audit rows ride the ledger transaction.
	creditSvc.SetAuditLogger(auditLogger)
	// tenant: platform provisioning previously left NO audit trail (the
	// /v1/tenants group mounts no catch-all); the create row now rides the
	// tenants INSERT tx, landing in the NEW tenant's own log (panel Q6).
	tenantSvc.SetAuditLogger(auditLogger)
	// creditnote: Issue()/relief emit on their coordinator tx — one
	// canonical credit_note.issued row for operator, engine, reconciler,
	// and catchup paths; refund-webhook status flips emit on the store tx.
	creditNoteSvc.SetAuditLogger(auditLogger)
	// payment: checkout.session.completed PM flip (webhook-driven,
	// previously invisible) + the public payment-update token consume/
	// restore pair. The hosted-invoice checkout claim gets its emitter at
	// construction (hostedInvoiceStripeAdapter literal above).
	stripeAdapter.SetAuditLogger(auditLogger)
	if publicPaymentH != nil {
		publicPaymentH.SetAuditLogger(auditLogger)
	}
	invoiceH.SetAuditLogger(auditLogger)
	subH.SetAuditLogger(auditLogger)
	creditNoteH.SetAuditLogger(auditLogger)
	// CN document email (ADR-082 rider) rides the transactional outbox
	// like every other customer email.
	creditNoteH.SetEmailSender(outboxSender)
	// Wire audit on paymentmethods.Service so attach/setDefault/detach
	// surface in the operator Activity feed + AuditLog page. Without
	// this, customer-driven card mutations are invisible to operators.
	paymentMethodsSvc.SetAuditLogger(auditLogger)
	// paymentMethodsH (Handler-side) is constructed further down — its
	// operator-only send-setup-email path is wired immediately after
	// the NewHandler call to keep declaration + dep-wiring co-located.
	// Tier-2 audit handlers — API key, dunning policy, pricing,
	// Stripe connection, webhook endpoint lifecycle. testClockH is
	// wired after its declaration further down. Each adds a forensics
	// trail for operator-driven state changes that previously had
	// no audit row.
	authH.SetAuditLogger(auditLogger)
	dunningH.SetAuditLogger(auditLogger)
	pricingH.SetAuditLogger(auditLogger)
	tenantStripeH.SetAuditLogger(auditLogger)
	// ADR-090 in-tx emitters for the routes whose ONLY audit coverage used to
	// be the URL-guessing catch-all (deleted in ADR-090; the registry + the
	// pure-observer detector replaced it). These are
	// SERVICE-side seams, distinct from the post-hoc handler seams above.
	customerSvc.SetAuditLogger(auditLogger)
	pricingSvc.SetAuditLogger(auditLogger)
	recipeSvc.SetAuditLogger(auditLogger)
	providerCostH.SetAuditLogger(auditLogger)
	// usage: BACKFILL only — live machine ingest stays unaudited by design
	// (usage_events is the record); a backdated operator insert is a money-path
	// action and rides the ingest tx.
	usageSvc.SetAuditLogger(auditLogger)
	webhookOutH.SetAuditLogger(auditLogger)
	// Service-level audit logger so state-changing service calls reachable
	// from multiple entry points (operator handler + dunning adapter + any
	// future caller) produce a single canonical audit row.
	subSvc.SetAuditLogger(auditLogger)
	// Invoice service audit + event dispatch: MarkUncollectible is
	// reachable both via the operator dashboard AND via dunning's
	// terminal action AND via ResolveRun(invoice_not_collectible).
	// All three paths share the service entry, so wiring at the
	// service layer guarantees a single audit row + a single
	// invoice.marked_uncollectible webhook event regardless of
	// trigger. RecordOfflinePayment uses the same wiring for its
	// invoice.payment_recorded event.
	invoiceSvc.SetAuditLogger(auditLogger)
	invoiceSvc.SetEventDispatcher(eventDispatcher)

	// Test clocks (FEAT-8 P5) — test-mode-only frozen-time simulator.
	// Constructed before the billing engine so the engine can read clock
	// state; the service then receives the engine as its billing runner via
	// SetBillingRunner below, breaking the circular dep.
	testClockStore := testclock.NewPostgresStore(db)
	testClockSvc := testclock.NewService(testClockStore)
	testClockH := testclock.NewHandler(testClockSvc)
	testClockH.SetAuditLogger(auditLogger)
	// Customer-level test-clock attach (ADR-027): customer service
	// validates test_clock_id at create time. Wired after the store
	// exists; before this point customer creation always rejects
	// test_clock_id (defensive default).
	customerSvc.SetTestClockChecker(testClockStore)
	// Archive/currency guard (ADR-067): customer.Update refuses to archive
	// while any subscription still bills, and UpsertBillingProfile refuses a
	// currency that would re-denominate an active sub's plan prices. The
	// adapter lives here (zero peer-imports between customer and
	// subscription packages).
	customerSvc.SetSubscriptionChecker(&subscriptionCheckerAdapter{subs: subStore, plans: pricingSvc})
	// Wire the per-customer tax-retry flush so a billing-profile save
	// (operator edits country / postal_code / state / tax_id) replays
	// every draft invoice for that customer stuck on
	// customer_data_invalid. Sibling of the ADR-019 Stripe-reconnect
	// flush, scoped per-customer. Without this, the operator must
	// click Retry per-invoice after fixing the address.
	customerSvc.SetTaxFlusher(invoiceSvc)
	// Attached-customers panel reads through the customer service so
	// display_name / email arrive decrypted (the customer package owns
	// the encrypt-at-rest wrapper). Without this, the testclock domain
	// would have to SELECT the customers table directly and skip
	// decryption — which is exactly the bug this wiring prevents.
	testClockSvc.SetCustomerReader(customerSvc)

	// Billing engine + manual trigger (with credit auto-application)
	// PaymentReadiness combines customers.stripe_customer_id +
	// payment_methods canonical → "can we charge this customer?"
	// in one call. Replaces the legacy customer_payment_setups
	// summary table.
	paymentReadiness := &paymentReadinessAdapter{customers: customerStore, pms: paymentMethodsSvc}
	engine := billing.NewEngine(subStore, usageStore, pricingSvc,
		&invoiceWriterAdapter{store: invoiceStore}, creditSvc, settingsStore, paymentReadiness, stripeAdapter, clk, customerStore)
	// ADR-101 Phase 4 (2026-07-28): billing_intervals is the ONLY
	// segment source — the legacy fact-log interpretation, the shadow
	// comparator, and the VELOX_BILLING_INTERVALS_READER kill switches
	// are gone (clean soak: 140/140 cutover sweep, two-mode corpus CI on
	// every PR since, zero unexplained divergences). A stale env value
	// can no longer change behavior; say so loudly instead of silently
	// ignoring an operator's kill-switch intent.
	if v := strings.TrimSpace(os.Getenv("VELOX_BILLING_INTERVALS_READER")); v != "" && v != "on" {
		slog.Warn("VELOX_BILLING_INTERVALS_READER is set but the flag was removed at ADR-101 Phase 4 — billing_intervals always bill; the legacy reader no longer exists", "value", v)
	}
	engine.SetIntervalReader(subStore)
	engine.SetTestClockReader(testClockStore)
	engine.SetEventDispatcher(eventDispatcher)
	// Coordinator-tx seam: fireThreshold's reset=true arm commits the
	// threshold invoice + cycle re-anchor atomically (ADR-066). The engine
	// fails loudly on reset fires if this is missing.
	engine.SetTxRunner(db)
	// Engine writes audit_log on background-fired lifecycle events
	// (currently: scheduled-cancellation auto-fire at period end).
	// Without this, the sub Activity timeline shows "Cancellation
	// scheduled" with no terminal "Subscription canceled" partner —
	// the gap that surfaced this session.
	engine.SetAuditLogger(auditLogger)
	// Customer reader powers SimForCustomer (subscription
	// create / one-off invoice composer / clock-pinned customer
	// path). Without this, the engine's customer-side clock resolution
	// falls back to wall-clock, leaking simulated-time invariants on
	// operator-driven creates against clock-pinned customers.
	engine.SetCustomerReader(customerStore)
	// Plumb effective-now resolution into dunning so every per-invoice
	// timestamp dunning writes (next_action_at / last_attempt_at /
	// resolved_at / created_at) lands in the same time domain the
	// catchup query reads against — frozen_time for clock-pinned runs,
	// wall-clock otherwise. Without this wire, clock-pinned dunning
	// runs whose stamps land in wall-clock 2026 are stranded against a
	// catchup window that compares to the frozen_time the operator is
	// advancing through. ADR-029 follow-up.
	dunningSvc.SetResolver(engine)
	// Dunning handler: resolveRun binds from invoice before MarkPaid so
	// `invoice.paid_at` lands in sim-time for clock-pinned invoices
	// instead of wall-clock. Last unbound seam in the operator invoice
	// path (2026-05-28).
	dunningH.SetResolver(engine)
	// Payment reconciler: the cron-driven fallback that resolves
	// invoices stuck in PaymentUnknown by polling Stripe for the real
	// outcome. Without the resolver, paid_at on a clock-pinned invoice
	// landed in wall-clock — split-brain against issued_at / due_at /
	// billing_period on the same row. ADR-030 cross-flow audit
	// 2026-05-28.
	paymentReconciler.SetResolver(engine)
	// Stripe webhook handler (handlePaymentSucceeded / handlePaymentFailed)
	// fires async with no inherited ctx binding. Wire the resolver so
	// every webhook-driven write (paid_at, payment-failure stamp,
	// dunning-run create) lands in simulated time on clock-pinned
	// invoices instead of leaking wall-clock.
	stripeAdapter.SetResolver(engine)
	// Customer-service mutations (Update, UpsertBillingProfile, etc.)
	// bind effective-now from the customer pin so postgres updated_at
	// stamps on a clock-pinned customer's row are simulated time.
	customerSvc.SetResolver(engine)
	// Credit grants for clock-pinned customers must stamp the ledger
	// entry's created_at in simulated time so the dashboard "credit
	// granted on …" line matches the rest of the simulated timeline.
	creditSvc.SetResolver(engine)
	// Usage ingest on a clock-pinned customer defaults timestamps to and
	// gates them against the clock's frozen_time, so simulated-time usage
	// can be ingested onto an advanced clock (every path funnels here:
	// live POST, batch, backfill, LiteLLM). Live mode never pays the
	// lookup — test clocks are test-mode-only by DB CHECK.
	usageSvc.SetResolver(engine)
	// subscription.Service uses the resolver at Create / Activate /
	// ChangeItem / ScheduleCancel / PauseCollection / EndTrial /
	// ExtendTrial so every per-sub timestamp the operator writes
	// lands in the right time domain. Without this, clock-pinned
	// subs created today against a 1-month-old idle clock get
	// next_billing_at stamped in 2026 wall-clock against a 2026-04
	// frozen_time — same stranding shape as dunning had at the
	// per-run level.
	subSvc.SetResolver(engine)
	// Handler-side resolver for proration math + changeAt stamps on
	// clock-pinned subs (PR-12). Without this, remainingPeriodFactor
	// + handleItemProration would use wall-clock now even when the
	// underlying sub is in simulated time.
	subH.SetResolver(engine)
	// ADR-058: anchor the proration denominator (fullBillingCycleDays) in the
	// tenant billing timezone, the same zone the engine advances period
	// boundaries in — so the cycle length can't disagree with the period and
	// is independent of the host time.Local.
	subH.SetTenantLocator(engine)
	// Proration invoices stamp the tenant's configured Net terms (and the
	// due date derived from them), same resolution as engine cycle/create
	// invoices — pre-fix the handler hardcoded Net 30, so a Net-15 tenant's
	// proration invoice carried a different due date than its siblings.
	subH.SetNetTermsReader(engine)
	// Wire the db handle so addItem (and updateItem / removeItem when
	// they migrate to the same pattern) can open an outer tx wrapping
	// the sub-item insert + proration writes — atomic guarantee that
	// pre-2026-05-29 left half-committed orphan items on proration
	// failures. ADR-030 atomic-proration follow-through.
	subH.SetDB(db)
	// ADR-031: wire the engine so subscription.Service.Create emits
	// the day-1 invoice for in_advance plans. No-op for in_arrears
	// (default) — best-effort path that logs but doesn't fail Create
	// on billing errors (the cycle scheduler picks up later periods).
	subSvc.SetBiller(engine)
	// ADR-031 slice 3: the engine's BillOnCancel uses creditSvc to
	// issue a credit grant for the unused portion of an in_advance
	// period when a sub is canceled mid-cycle. No-op for in_arrears.
	engine.SetCreditGranter(creditSvc)
	// #22: when the in_advance prebill for the canceled period is UNPAID,
	// BillOnCancel settles it down to the consumed portion instead of leaving
	// the full amount in dunning — voiding it (nothing consumed) or reducing
	// amount_due via an adjustment credit note (partially consumed).
	engine.SetInvoiceVoider(invoiceSvc)
	engine.SetCreditNoteAdjuster(creditNoteSvc)
	engine.SetCreditHeadroomReader(creditNoteSvc)
	// invoice.Service uses the resolver at Create so one-off
	// composer invoices and manual sub-attached addenda for
	// clock-pinned customers / subs stamp due_at in simulated time.
	// The reminder catchup compares due_at against frozen_time;
	// without the resolver wire, the row never lands in the catchup
	// window.
	invoiceSvc.SetResolver(engine)
	// Customer-pin reader: lets manual-invoice Create stamp is_simulated from
	// the customer's test_clock_id (the authoritative write-time signal, like
	// the engine's sub.TestClockID for cycle invoices). Without it, manual
	// invoices never carry the simulated badge.
	invoiceSvc.SetCustomerReader(customerStore)
	// Dunning-escalated signal for the attention classifier (2026-07-22
	// payment-surfacing audit P1-4) — narrow read, store-backed.
	invoiceSvc.SetDunningRunReader(dunningStore)
	invoiceSvc.SetCreditApplier(creditSvc)
	// Tenant net-terms fallback: a manual invoice created without an explicit
	// net_payment_term_days inherits the tenant's configured default (then 30),
	// mirroring the cycle engine. settingsStore satisfies TenantSettingsReader.
	invoiceSvc.SetTenantSettingsReader(settingsStore)
	testClockSvc.SetBillingRunner(engine)
	// Phase 0.5 (Bug #8): trial expiry — flip status='trialing' subs
	// to active at trial_end_at when sim time elapses past it, BEFORE
	// Phase 1 cycle billing. Without this, status stays 'trialing'
	// for up to ~30 days (calendar billing's stub close) past the
	// actual trial-end instant.
	testClockSvc.SetTrialExpirer(subSvc)
	// Catchup Phase 0.7 — pause_collection auto-resume. Subs whose
	// resumes_at has elapsed are unpaused BEFORE the cycle scan reads
	// the due list, so an Advance that crosses resumes_at produces a
	// finalized invoice for the next-due period instead of a draft.
	// Stripe-parity (resume AT resumes_at, not next cycle close).
	testClockSvc.SetPauseResumer(subSvc)
	// ADR-029 Phase 2: catchup orchestrator drives tax retry on
	// clock-pinned invoices. Without this, the cron-side filter
	// (NOT EXISTS clock-pinning) would correctly skip them but no
	// other path would retry — clock-pinned tax-pending invoices
	// would be stranded until an operator manually clicked Retry tax
	// per invoice.
	testClockSvc.SetTaxRetrier(invoiceSvc)
	// ADR-029 Phase 3.7: issue a clock's deferred simulated clawback drafts in
	// the catchup orchestrator against frozen_time. The wall-clock clawback
	// reconciler skips simulated rows, so without this a simulated downgrade
	// whose source was in-flight would strand its clawback draft.
	testClockSvc.SetClawbackRetrier(creditNoteSvc)
	// ADR-029 Phase 4: credit expiry for clock-pinned customers' grants
	// runs in the catchup orchestrator against the clock's frozen_time.
	testClockSvc.SetCreditExpirer(creditSvc)
	// ADR-029 Phase 5: dunning advances for clock-pinned invoices fire
	// in the catchup orchestrator against the clock's frozen_time, not
	// on the wall-clock 5-min tick.
	testClockSvc.SetDunning(dunningSvc)
	// No-payment-method finalize notifier: at finalize-time when a
	// customer has no PaymentSetup ready, the engine queues for retry
	// (so attaching a PM later auto-collects) AND emails the customer
	// with a payment-update link. Without this, customers learn about
	// the missing PM only when the invoice goes overdue weeks later —
	// too late for happy-path collection. Stripe sends the equivalent
	// email at charge-failure time; we extend the same pattern to the
	// no-PM-at-finalize case for symmetric customer experience.
	// ADR-013.
	//
	// Always wired. If paymentUpdateURL is empty the adapter surfaces
	// the misconfiguration at send time per the boot WARN above —
	// single send path, no silent skip.
	noPMNotifier := &noPaymentMethodNotifierAdapter{
		email:            paymentSetupEmail,
		customerEmail:    customerEmailAdapter,
		paymentUpdateURL: paymentUpdateURL,
		tokenSvc:         tokenSvc,
		auditLogger:      auditLogger,
	}
	// Struct-literal wiring again — invisible to the SetAuditLogger grep. This
	// adapter writes the ENGINE-driven setup_link_sent row at finalize (the
	// operator-driven one comes from paymentmethods.Handler), and its emitter is
	// nil-guarded — so a dropped wiring here loses that row in silence.
	audit.MustWired(noPMNotifier)
	engine.SetNoPaymentMethodNotifier(noPMNotifier)
	// Same adapter feeds the manual-invoice finalize path so an
	// operator-composed one-off invoice notifies the customer + queues for
	// auto-charge retry on no-PM identically to a cycle invoice.
	invoiceH.SetNoPaymentMethodNotifier(noPMNotifier)
	// No-payment dunning enrollment: the scheduler (and test-clock catchup)
	// route card-less auto_charge_pending invoices into dunning so they
	// reach a terminal instead of looping in RetryPendingCharges forever.
	engine.SetDunningStarter(&dunningStarterAdapter{dunning: dunningSvc})
	// ADR-097: a cancel_at strictly inside a billing period fires through
	// the subscription service's one-tx composition (flip + final invoice +
	// relief drafts). The engine detects due-ness via the scans' cancel
	// arm; unwired, a due mid-period cancel would spin the scan forever.
	engine.SetScheduledCancelExecutor(subSvc)
	// Close an active dunning run when a background settle (credit-cover sweep /
	// threshold close) pays the invoice without going through the invoice
	// handler. dunningSvc.ResolveByInvoice matches the interface directly (no
	// adapter; it no-ops when there's no active run).
	engine.SetDunningResolver(dunningSvc)

	// Tax: per-tenant provider resolution (none|manual|stripe_tax) + durable
	// audit trail in tax_calculations. Resolver reads tenant_settings and
	// hands back the concrete Provider; the store persists request/response
	// payloads so we can reconstruct tax decisions after Stripe's 24h
	// calculation expiry window.
	engine.SetTaxProviderResolver(tax.NewResolver(stripeClients))
	engine.SetTaxCalculationStore(tax.NewPostgresStore(db))
	// Last engine collaborator is wired above — fail closed NOW if any is
	// missing, naming every nil field, instead of silently diverging on a
	// money path at runtime (2026-07-10 design review, redesign #3 stage 1).
	engine.MustValidate()
	// Same fail-closed shape for ADR-090 audit wiring: services skip
	// emission on a nil emitter (fake-friendly in tests), so a forgotten
	// SetAuditLogger line silently un-audits a domain. Every in-tx-audited
	// component joins this list in the PR that audits it;
	// TestMustWired_CoversEveryAuditedComponent pins the list.
	// (publicPaymentH is exempt: its constructor legitimately returns nil
	// when Stripe isn't configured, so its wiring is guarded above.)
	audit.MustWired(creditSvc, tenantSvc, subSvc, invoiceSvc, creditNoteSvc,
		stripeAdapter, paymentMethodsSvc, engine, hostedInvoiceStripe,
		customerSvc, pricingSvc, recipeSvc, providerCostH, usageSvc)
	if publicPaymentH != nil {
		// Guarded: the constructor legitimately returns nil without Stripe
		// keys; when it exists, its emitter must be wired.
		audit.MustWired(publicPaymentH)
	}
	if checkoutH != nil {
		// Same guard: nil when Stripe isn't configured.
		checkoutH.SetAuditLogger(auditLogger)
		audit.MustWired(checkoutH)
	}

	// Invoice finalize commits the upstream Stripe Tax calculation into a
	// tax_transaction so the tenant's Stripe Tax reports reflect the final
	// invoice. Manual/none providers receive the call but no-op.
	invoiceSvc.SetTaxCommitter(engine)

	// Invoice Void reverses the upstream tax_transaction so the
	// authority stops reporting voided invoices as collected tax.
	// Same engine instance handles credit-note tax reversals — single
	// code path keeps the Reference uniqueness contract intact.
	invoiceSvc.SetTaxReverser(engine)
	// So void/uncollectible reverses exactly the un-credit-noted tax (total −
	// credited), correct even when customer credit was applied to the invoice.
	invoiceSvc.SetCreditNoteTotaler(creditNoteSvc)

	// Void reverses applied customer credits atomically with the status flip
	// (one coordinator tx) instead of the prior best-effort post-void handler
	// step that silently stripped paid-with credits on any transient failure.
	invoiceSvc.SetCreditReverser(creditSvc)

	// Operator-initiated retry-tax routes through the billing engine,
	// which owns provider resolve → recompute → atomic persist. Backs
	// the "Retry tax" action surfaced by the unified Attention shape on
	// invoices in tax_status pending or failed.
	invoiceSvc.SetTaxRetrier(engine)

	// Payment-method reader powers the no_payment_method attention
	// branch — distinguishes operator-actionable "add a PM" state from
	// the generic awaiting_payment race window. paymentSetupStore
	// (the canonical composite) satisfies PaymentMethodReader.
	invoiceSvc.SetPaymentMethodReader(paymentSetupStore)
	// Stripe-connected probe lets the attention classifier swap copy
	// from "Stripe isn't connected" → "calculation queued, retry now"
	// for tax_status='pending' invoices when the operator has just
	// connected Stripe but the scheduler hasn't ticked yet. ADR-019's
	// reconnect-flush already retries those invoices automatically;
	// this just makes the banner stop misleading the operator during
	// the gap window.
	invoiceSvc.SetStripeChecker(stripeClients)

	// Credit note issue reverses the invoice's tax_transaction so the
	// tenant's upstream tax liability is reduced alongside the refund.
	// Required for EU VAT, UK VAT, India GST compliance — without this
	// the credit note refunds the customer's money but leaves the
	// tenant over-remitting tax. Manual/none providers receive the call
	// but no-op.
	creditNoteSvc.SetTaxReverser(engine)

	// Proration invoices now share the billing engine's tax resolution path so
	// plan upgrades aren't silently tax-free. The adapter translates between
	// billing.TaxApplication and subscription.ProrationTaxResult.
	subH.SetProrationTaxApplier(&prorationTaxApplierAdapter{engine: engine})

	// ADR-048: route downgrade clawbacks (plan downgrade / quantity decrease /
	// item remove) on a PAID in_advance invoice through the tax-reversing
	// credit-note primitive instead of a bare net ledger grant — credits the
	// GROSS the customer paid for the unused slice AND reverses the proportional
	// output tax. *creditnote.Service satisfies CreditNoteIssuer directly.
	subH.SetCreditNoteIssuer(creditNoteSvc)

	billingH := billing.NewHandler(engine, subStore)
	// create_preview composes customer / subscription resolution on top of
	// the engine's preview path. Mounted at /v1/invoices/create_preview
	// (Stripe-equivalent), gated by PermInvoiceRead — read-only, no DB
	// writes. Sibling-mounted ahead of /invoices below so chi picks the
	// more-specific pattern (otherwise /{id} captures "create_preview").
	createPreviewH := billing.NewCreatePreviewHandler(
		billing.NewPreviewService(engine, customerStore, subStore),
	)

	analyticsH := analytics.NewHandler(db)

	// Email blind index (separate HMAC key from VELOX_ENCRYPTION_KEY so one
	// compromise doesn't reveal the other) powers bounce reporting + the
	// recipient-suppression gate. Without it those fail closed.
	if bidxKey := strings.TrimSpace(os.Getenv("VELOX_EMAIL_BIDX_KEY")); bidxKey != "" {
		b, err := crypto.NewBlinder(bidxKey)
		if err != nil {
			slog.Error("invalid VELOX_EMAIL_BIDX_KEY — bounce-report and suppression lookups will fail closed", "error", err)
		} else {
			customerStore.SetBlinder(b)
			// Wire bounce reporting now that we have the blinder. The
			// same adapter serves both the SMTP sync path (ReportBounce,
			// fire-and-forget) and the Postmark webhook's async path
			// (SuppressBounced/SuppressComplained, error-returning).
			bounceReporter := &bounceReporterAdapter{
				blinder: b,
				store:   customerStore,
				svc:     customerSvc,
			}
			emailSender.SetBounceReporter(bounceReporter)
			postmarkH.SetSuppressor(bounceReporter)
			// Wire the recipient-suppression gate on both code paths
			// (direct Sender + OutboxSender). Closes the bounce →
			// suppression loop: once email_status='bounced' is recorded,
			// no further sends to that recipient enter the outbox.
			// Same blinder powers both — symmetric to the bounce reporter.
			suppressionChecker := &suppressionCheckerAdapter{blinder: b, store: customerStore}
			emailSender.SetSuppressionChecker(suppressionChecker)
			if emailOutboxSenderRef != nil {
				emailOutboxSenderRef.SetSuppressionChecker(suppressionChecker)
			}
			slog.Info("email blind index enabled for bounce reporting + recipient suppression gate")
		}
	} else {
		slog.Warn("VELOX_EMAIL_BIDX_KEY not set — bounce reporting + suppression will fail closed (no customers findable by email)")
	}
	// Operator-side payment-method management — handler-only wiring
	// here; the store + service were constructed earlier (next to
	// the paymentMethodsStripe adapter) so the billing engine's
	// PaymentReadiness adapter can depend on the same service.
	paymentMethodsH := paymentmethods.NewHandler(paymentMethodsSvc)
	// Operator-side send-setup-email needs a customer lookup +
	// emailer + audit. Service-side audit lands separately on
	// paymentMethodsSvc above (Tier-1 attach/setDefault/detach).
	// Per-customer cooldown limiter is wired further down once
	// rdb is constructed (line ~960).
	paymentMethodsH.SetCustomerLookup(&pmCustomerLookupAdapter{store: customerStore})
	paymentMethodsH.SetEmailer(setupLinkEmail)
	paymentMethodsH.SetAuditLogger(auditLogger)

	// setup_intent.succeeded webhooks write payment_methods rows via the
	// service. Wired here because stripeAdapter (payment/) must not import
	// paymentmethods/ — the interface lives in payment/ and the method
	// name is AttachForWebhook so the two AttachFromSetupIntent callers
	// (tests, webhook) keep their own signatures.
	stripeAdapter.SetPaymentMethodAttacher(paymentMethodsSvc)
	// Authoritative fallback when a SetupIntent carries no velox_customer_id
	// metadata (hosted "update payment" / operator add-card Checkout flows
	// don't set setup_intent_data.metadata): resolve the velox customer from
	// the SetupIntent's `customer` field so the saved card still persists.
	stripeAdapter.SetCustomerResolver(&stripeCustomerResolverAdapter{customers: customerStore})

	// GDPR data export + erasure was removed 2026-05-29 pre-launch.
	// The prior implementation was a half-fix (didn't sync erasure
	// to Stripe Customer, no acknowledgement-window tracking).
	// Rebuild properly when the first EU-targeting DP signs.

	// Dashboard sessions are user-bound (ADR-011) — minted by the
	// email+password login flow, server-side revocable, httpOnly
	// cookie. The session service itself is a thin wrapper over the
	// dashboard_sessions table.
	sessionSvc := session.NewService(session.NewPostgresStore(db))

	// User accounts: email + password auth for the dashboard. See
	// ADR-011. The auth handler wires /v1/auth/login,
	// /v1/auth/logout, and the password-reset flow.
	userStore := user.NewPostgresStore(db)
	userSvc := user.NewService(userStore, nil)
	// Password-reset emails ride the same email infrastructure as
	// every other transactional email. The user.EmailSender interface
	// is narrower than email.OutboxSender's signature (no tenant or
	// display-name context at password-reset time, since the operator
	// hasn't authenticated yet), so we wrap with an adapter that
	// passes empty tenant + the email-as-name. brandingFor() falls
	// back to platform defaults on empty tenantID — fine for an
	// operator-grade email.
	// Dashboard SPA origin used for password-reset links. Optional in
	// single-origin prod (the request Host already serves both the API
	// and the SPA, so the bare host works) but required in split-origin
	// dev where the Vite server (:5173) and API (:8080) are different
	// origins — without it, the reset link points at the API server
	// which doesn't serve a /reset-password route, and the operator
	// hits a 404 when clicking the email.
	dashboardBaseURL := strings.TrimSpace(os.Getenv("DASHBOARD_BASE_URL"))
	if dashboardBaseURL == "" {
		slog.Warn("DASHBOARD_BASE_URL NOT SET — password-reset emails will NOT be sent (the reset link origin is never derived from request headers, to prevent host-header poisoning / token theft). Set this to your canonical dashboard URL (e.g. http://localhost:5173 in dev) to enable password-reset emails.")
	}
	dashboardAuthH := user.NewHandler(
		userSvc, sessionSvc, session.DefaultCookieConfig(),
		&passwordResetEmailAdapter{sender: passwordResetEmail},
		dashboardBaseURL,
		emailSender.IsConfigured(),
	)
	// Audit authenticated auth events (login, logout, mode change, password
	// reset). Failed logins go to the structured security log instead, not the
	// per-tenant audit_log. This is the DASHBOARD (session) auth handler —
	// distinct from authH (the API-key handler) wired above.
	dashboardAuthH.SetAuditLogger(auditLogger)

	// Team membership: invite / accept / remove (minimal — no RBAC; every
	// member gets the full owner permission set until roles land). Invite
	// emails ride the transactional outbox like every other email; accept
	// links are built from DASHBOARD_BASE_URL, same host-header-poisoning
	// posture as password-reset links.
	membersSvc := dashmembers.NewService(
		dashmembers.NewPostgresStore(db),
		&memberUserDirectoryAdapter{svc: userSvc, store: userStore},
		sessionSvc,
		outboxSender,
		&memberTenantNamerAdapter{svc: tenantSvc},
		nil, // real clock
		dashboardBaseURL,
	)
	membersH := dashmembers.NewHandler(membersSvc, sessionSvc, session.DefaultCookieConfig())
	membersH.SetAuditLogger(auditLogger)

	// HANDLER-level emitters are gated too. They were exempt on the grounds that
	// they "keep the *audit.Logger concrete type and fail loudly at compile time"
	// — and both halves of that were false. MOST of them take an INTERFACE
	// (auth.AuditWriter, subscription.auditRecorder, invoice.auditWriter, …), so a
	// nil emitter does not even fail to TYPE-check; and every one of them —
	// INCLUDING the two that DO hold the concrete *audit.Logger (customerH,
	// creditNoteH) — nil-GUARDS its emission and silently skips it. A concrete type
	// buys nothing when the field is nil-able and the call site tolerates nil. So a forgotten
	// SetAuditLogger line here did not fail to compile and did not fail any test:
	// it un-audited a whole domain in silence, with the route registry still
	// declaring those routes `explicit` and every gate green. The API-key
	// lifecycle (mint / revoke / rotate) sat behind exactly that hole.
	//
	// MustWired is generic — it reflects the audit field out of any struct — so
	// there was never a reason handlers could not be passed to it. This call is
	// second because paymentMethodsH / dashboardAuthH / membersH are constructed
	// after the first one.
	audit.MustWired(
		customerH, settingsH, invoiceH, subH, creditNoteH,
		authH, dunningH, pricingH, tenantStripeH, webhookOutH,
		testClockH, paymentMethodsH, dashboardAuthH, membersH,
	)

	s := &Server{
		BillingEngine:    engine,
		DunningSvc:       dunningSvc,
		SettingsStore:    settingsStore,
		WebhookOutSvc:    webhookOutSvc,
		OutboxStore:      outboxStore,
		EmailOutboxStore: emailOutboxStore,
		EmailSender:      emailSender,
		// The invoice store satisfies email.InvoiceSettledChecker
		// directly (ActionRequiredObsolete) — consumer-defined
		// interface, wired here at the composition root.
		EmailSettledChecker: invoiceStore,
		CreditSvc:           creditSvc,
		InvoiceSvc:          invoiceSvc,
		CreditNoteSvc:       creditNoteSvc,
		SubscriptionSvc:     subSvc,
		TokenSvc:            tokenSvc,
		PaymentReconciler:   paymentReconciler,
		TestClockSvc:        testClockSvc,
	}

	// Redis for distributed rate limiting. Failure direction is split:
	// general + hosted-invoice limiters fail CLOSED in production (DDoS
	// posture), ingest and /v1/auth always fail open (revenue events and
	// operator login must survive a Redis blip).
	var rdb *redis.Client
	if redisURL := strings.TrimSpace(os.Getenv("REDIS_URL")); redisURL != "" {
		opt, err := redis.ParseURL(redisURL)
		if err != nil {
			slog.Warn("invalid REDIS_URL — general/hosted rate limiters FAIL CLOSED in production (requests 429), fail open otherwise; ingest always fails open", "error", err)
		} else {
			rdb = redis.NewClient(opt)
			if err := rdb.Ping(context.Background()).Err(); err != nil {
				slog.Warn("redis not reachable — general/hosted rate limiters FAIL CLOSED in production (requests 429), fail open otherwise; ingest always fails open", "error", err)
			} else {
				slog.Info("redis connected for rate limiting")
			}
		}
	} else {
		slog.Info("REDIS_URL not set — rate limiting disabled in dev; in production the general/hosted limiters FAIL CLOSED (all covered requests 429) — set REDIS_URL")
	}
	// Login brute-force protection: v1 ships NO automatic per-account throttle.
	// The old users.locked_until auto-lockout (fragmenting across Redis↔in-memory
	// and weaponizable — any known operator email → a 15-min lockout on demand)
	// was removed, and the interim Postgres throttle that briefly replaced it was
	// removed too, to keep v1 lean until the full control lands. The per-IP
	// /v1/auth limiter below is the brute-force floor; the full design (a
	// non-weaponizable throttle + MFA + login-time breached-password check) is
	// documented in ADR-094 and deferred as one unit.
	rateLimiter := mw.NewRateLimiter(rdb, "general",
		envInt("VELOX_RATE_LIMIT_GENERAL_PER_MIN", 100), time.Minute)
	// In production, refuse requests when Redis is unreachable rather than
	// silently disabling rate limiting (DDoS vector). The general/IP limiter
	// deliberately stays fail-open in non-prod: per the industry split (OWASP
	// ASVS + practitioner consensus) generic API rate limiting should fail
	// open on a store blip. v1 has no dedicated per-account login throttle
	// (deferred — ADR-094); the per-IP /v1/auth limiter below is the floor.
	if strings.EqualFold(strings.TrimSpace(os.Getenv("APP_ENV")), "production") {
		rateLimiter.SetFailClosed(true)
	}
	// /v1/auth gets its OWN limiter instance — same Redis namespace and
	// limits as the general one (shared buckets), but NEVER fail-closed:
	// locking operators out of login during a Redis blip re-introduces
	// the DoS the #21 design avoids. This per-IP limiter is the only
	// brute-force floor on auth in v1 — a per-(IP×account) login throttle
	// is deferred (ADR-094). The HA-readiness audit (docs/dev/
	// ha-readiness-2026-07-06.md) found the prior wiring mounted the
	// fail-closed general limiter on /v1/auth, contradicting this
	// stated design — a prod Redis outage killed dashboard login.
	authLimiter := mw.NewRateLimiter(rdb, "general",
		envInt("VELOX_RATE_LIMIT_GENERAL_PER_MIN", 100), time.Minute)

	// Dedicated, much larger bucket for the ingest surface (/v1/usage-events*
	// + the LiteLLM adapter). The general 100/min bucket was sized for
	// operator CRUD; LiteLLM POSTs ONE callback per LLM call (ADR-033), so
	// any real AI-infra traffic above ~1.7 calls/s exhausted it inside the
	// first hour of wiring the proxy — and LiteLLM at stock config retries
	// NOTHING (max_retries=0; the queue is cleared on any send error —
	// verified against LiteLLM source 2026-07-06, see docs/dev/
	// ha-readiness-2026-07-06.md), so every 429 was a silently dropped,
	// permanently unbilled event. Peer
	// anchor: Stripe meter events allow 1,000 calls/s live; Orb's TEST mode
	// alone allows 2,000 events/min. Default 1000 req/s per key, override
	// via VELOX_RATE_LIMIT_INGEST_PER_SEC.
	//
	// Deliberately NEVER fail-closed, even in production: the surface is
	// authenticated (secret key required), and failing closed would turn a
	// Redis blip into dropped revenue data — for ingest, lost-events risk
	// outranks the DoS argument that closes the general bucket.
	ingestLimiter := mw.NewRateLimiter(rdb, "ingest",
		envInt("VELOX_RATE_LIMIT_INGEST_PER_SEC", 1000), time.Second)

	// Tighter bucket for the public hosted-invoice surface. Industry
	// practice (Stripe, Paddle) is to rate-limit payment pages more
	// aggressively than the general API because: (a) the URL is publicly
	// shareable so abuse potential is higher, (b) each Pay click creates a
	// Stripe Checkout Session which has its own upstream rate limit, and
	// (c) genuine customer traffic from a single NAT is small — ~5-10
	// requests per visit. 60/min per IP leaves headroom for ~10
	// simultaneous customers behind a single corporate NAT while keeping
	// enumerations and scraping bounded.
	hostedInvoiceRL := mw.NewRateLimiter(rdb, "hosted_invoice", 60, time.Minute)
	if strings.EqualFold(strings.TrimSpace(os.Getenv("APP_ENV")), "production") {
		hostedInvoiceRL.SetFailClosed(true)
	}

	// Per-customer cooldown on operator send-setup-email: 1 send /
	// (tenant, customer) / 60s. Defends against operator double-click
	// and the refresh-then-click race. Always fail-open even in prod —
	// a Redis hiccup shouldn't block a legitimate operator's "send link"
	// action; the worst case (Redis down + actual double-click) is two
	// near-identical emails, no money/state at risk.
	setupLinkCooldown := mw.NewRateLimiter(rdb, "setup_link", 1, time.Minute)
	paymentMethodsH.SetCooldown(setupLinkCooldown)

	r := chi.NewRouter()
	r.Use(mw.Tracing())
	// NOT chi's middleware.RequestID — it honours an inbound X-Request-Id header
	// verbatim, which would let a caller choose the request_id recorded on their
	// own audit_log rows. mw.RequestID always mints server-side. See its doc
	// comment and ADR-090 §7 (read egress).
	r.Use(mw.RequestID)
	// Only honor X-Forwarded-For / X-Real-IP from configured trusted proxies.
	// chi's middleware.RealIP trusted them unconditionally, letting any client
	// forge a forwarding header to rotate its per-IP rate-limit bucket
	// (enumeration bypass) or pin a victim's IP. TRUST_PROXY is a comma-list of
	// CIDRs / IPs (e.g. "10.0.0.0/8,127.0.0.1"); unset → trust nothing and key
	// rate limits on the raw TCP peer.
	trustedProxies := mw.ParseTrustedProxies(os.Getenv("TRUST_PROXY"))
	if len(trustedProxies) == 0 {
		slog.Info("TRUST_PROXY not set — X-Forwarded-For/X-Real-IP ignored; rate limiting keys on the direct TCP peer. Set TRUST_PROXY to your load balancer's CIDR if Velox runs behind a proxy.")
	}
	r.Use(mw.TrustedRealIP(trustedProxies))
	corsEnv := os.Getenv("CORS_ALLOWED_ORIGINS")
	if corsEnv == "" {
		corsEnv = "http://localhost:3000,http://localhost:5173,http://localhost:5174"
	}
	allowedOrigins := strings.Split(corsEnv, ",")
	r.Use(mw.CORS(allowedOrigins))
	r.Use(mw.Metrics())
	r.Use(requestLogger)
	r.Use(middleware.Recoverer)

	// Limit request body to 1 MB to prevent DoS via oversized payloads.
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MB
			next.ServeHTTP(w, r)
		})
	})

	r.Use(mw.SecurityHeaders())

	// Audit-coverage detector (ADR-090). A PURE OBSERVER: it reports any
	// mutating 2xx that produced no audit row and is not declared exempt in the
	// route-audit registry (audit_routes.go) — slog.Error + the
	// velox_audit_uncovered_mutation_total counter. It never touches the
	// response and never buffers the body.
	//
	// Mounted at the ROOT, not on /v1: the catch-all it replaces only ever saw
	// the /v1 block, so /v1/auth, /v1/tenants, /v1/public/*, /v1/webhooks and
	// /v1/bootstrap were invisible to it (ADR-090 RC1). It also sits AFTER
	// RequestID + TrustedRealIP so the ctx it seeds for every downstream audit
	// emission carries the request id and the real client IP.
	r.Use(mw.AuditCoverage(auditRouteExempt))

	// NOTE: middleware.Timeout(30s) used to live here as a global cap,
	// but the Week 6 SSE stream needs an unbounded connection lifetime
	// — clients tail webhook events for hours. Push the timeout DOWN
	// to each authenticated route block instead, and skip it on the
	// SSE block. The unauthenticated public surfaces (health, metrics,
	// auth/login) don't need a 30s cap; their handlers complete in ms
	// or fail fast on their own.

	// Public
	r.Get("/health", handleHealth)
	r.Get("/health/ready", handleDeepHealth(db))
	r.Handle("/metrics", mw.MetricsAuth(mw.MetricsHandler()))

	// Bootstrap — one-time setup (no auth, only works when no tenants
	// exist). The deps wire the peer user package into
	// tenant.RunBootstrap's single-tx owner-user creation (ADR-073).
	// Named (not an inline literal) so the boot gate below can see the Audit field
	// it carries. Wired by STRUCT LITERAL rather than SetAuditLogger, which is
	// exactly how it escaped the gate: the pin test greps for SetAuditLogger
	// receivers, and MustWired only sees what it is handed. Bootstrap mints a LIVE
	// secret key and the owner account, its route is `explicit` in the registry,
	// and deps.Audit is NIL-GUARDED (bootstrap_run.go) — so a dropped wiring here
	// would have silently un-audited the birth of a money-moving credential.
	bootstrapDeps := tenant.BootstrapDeps{
		HashPassword: user.HashPassword,
		CreateUserTx: user.NewPostgresStore(db).CreateInTx,
		Audit:        auditLogger,
	}
	audit.MustWired(&bootstrapDeps)
	bootstrapH := tenant.NewBootstrapHandler(db, bootstrapDeps)
	r.Group(func(r chi.Router) {
		r.Use(rateLimiter.Middleware())
		r.Use(middleware.Timeout(30 * time.Second))
		r.Mount("/v1/bootstrap", bootstrapH.Routes())
	})

	// Dashboard auth — email+password login, logout, password-reset
	// flow. Public surface (pre-session). Rate limited to slow
	// credential stuffing. ADR-011.
	r.Route("/v1/auth", func(r chi.Router) {
		// CSRF: these endpoints are browser-dashboard-only and MINT the session
		// cookie, so a cross-site POST here is the login-fixation vector. Reject
		// cross-site browser requests before login can set a cookie. No bearer
		// path exists here, so every unsafe request is origin-checked.
		r.Use(mw.CSRFGuard(allowedOrigins))
		r.Use(authLimiter.Middleware())
		r.Use(middleware.Timeout(30 * time.Second))
		// Invite accept flow — public like login/reset (the token IS the
		// credential) and sharing the same limiter, so invite-token
		// guessing is throttled like credential stuffing. Explicit routes
		// win over the Mount("/") wildcard below.
		membersH.RegisterPublicRoutes(r)
		r.Mount("/", dashboardAuthH.Routes())
	})

	// Stripe webhooks — no API key auth (verified by signature)
	r.Group(func(r chi.Router) {
		r.Use(middleware.Timeout(30 * time.Second))
		r.Mount("/v1/webhooks", webhookH.Routes())
		// Postmark email webhooks — no API key auth (Basic-Auth gated;
		// Postmark signs nothing). Static route wins over the Mount
		// wildcard above.
		r.Post("/v1/webhooks/postmark", postmarkH.HandleWebhook)
	})

	// Public payment update — no auth (validated by token). Same tight
	// per-IP bucket as the hosted-invoice surface: both are
	// unauthenticated payment endpoints, and this one previously had NO
	// rate limit at all — free token brute-force + Stripe API
	// amplification.
	if publicPaymentH != nil {
		r.Group(func(r chi.Router) {
			r.Use(hostedInvoiceRL.Middleware())
			r.Use(middleware.Timeout(30 * time.Second))
			r.Mount("/v1/public/payment-updates", publicPaymentH.Routes())
		})
	}

	// Public hosted invoice — Stripe-equivalent hosted_invoice_url.
	// Unauthenticated: the 256-bit public_token in the URL is the sole
	// credential. Wrapped in its own rate-limit bucket (60/min per IP)
	// because payment surfaces deserve tighter limits than the general
	// API — see hostedInvoiceRL above.
	r.Route("/v1/public/invoices", func(r chi.Router) {
		r.Use(hostedInvoiceRL.Middleware())
		r.Use(middleware.Timeout(30 * time.Second))
		r.Mount("/", hostedInvoiceH.Routes())
	})

	// Public cost-dashboard projection — ADR-031 / MANUAL_TEST CU8.
	// Unauthenticated: the 256-bit cost_dashboard_token in the path
	// IS the credential. Tighter rate limit (60/min/IP) than the
	// general API because the embed widget may poll, and we want
	// abuse from one site to not exhaust shared buckets — same
	// rationale + same limit shape as the hosted invoice surface.
	r.Route("/v1/public/cost-dashboard", func(r chi.Router) {
		r.Use(hostedInvoiceRL.Middleware())
		r.Use(middleware.Timeout(30 * time.Second))
		r.Mount("/", customerH.PublicCostDashboardRoutes())
	})

	// Platform routes
	r.Route("/v1/tenants", func(r chi.Router) {
		r.Use(auth.Middleware(authSvc))
		r.Use(auth.Require(auth.PermTenantWrite))
		r.Use(middleware.Timeout(30 * time.Second))
		r.Mount("/", tenantH.Routes())
	})

	// Real-time webhook event SSE stream — mounted ABOVE the /v1 route
	// block so it skips the global 30s middleware.Timeout (which would
	// kill any long-lived EventSource connection at 30s). We re-apply
	// the same auth chain that /v1/* gets (session cookie OR Bearer
	// API key, rate limit, perm check) but specifically NOT
	// idempotency (no value on a streaming GET) and NOT the Timeout.
	// The stream is GET-only and read-only; no body to idempotency-key,
	// nothing to audit beyond the existing access log.
	r.Route("/v1/webhook_events/stream", func(r chi.Router) {
		r.Use(session.MiddlewareOrAPIKey(sessionSvc, authSvc))
		r.Use(rateLimiter.Middleware())
		r.With(auth.Require(auth.PermAPIKeyRead)).Get("/", webhookOutH.StreamHandler())
	})

	// Streaming CSV exports — mounted as a sibling of the /v1 block
	// (same pattern as the SSE stream above) so they get a FIVE-minute
	// timeout instead of the general 30s CRUD cap: a full usage-events
	// export legitimately streams longer than 30s, and the old cap
	// killed it mid-file — which, pre-P12, produced a silently
	// truncated CSV (see exportAbort). Same auth chain as /v1;
	// idempotency skipped (GET-only). Each endpoint requires the
	// corresponding *Read permission. A publishable key holds the EMPTY
	// permission set (auth/permission.go: KeyTypePublishable: {}), so it can
	// export NOTHING — not "only what it can list". MANUAL_TEST asserts the 403.
	//
	// The audit catch-all used to be mounted here too — and it BUFFERED the
	// whole response to sniff a label out of it. Its buffer implemented no
	// http.Flusher, so exports.go's `if f, ok := w.(http.Flusher)` assertion
	// silently failed and the CSV accumulated in memory rather than streaming
	// — on the one route block given five minutes precisely BECAUSE it streams.
	// The catch-all is gone (ADR-090); nothing between the handler and the
	// socket buffers anything now.
	//
	// Every export EMITS an action=export audit row before it streams, and
	// refuses to stream if that row can't be written (ADR-090 §7, read egress):
	// a tamper-evidence system that cannot show who copied the evidence has a
	// hole in its chain of custody. audit-log.csv is here too — the compliance
	// export is itself an audited event, and it rides the same permission as the
	// audit-log read route below.
	r.Route("/v1/exports", func(r chi.Router) {
		r.Use(session.MiddlewareOrAPIKey(sessionSvc, authSvc))
		r.Use(rateLimiter.Middleware())
		r.Use(middleware.Timeout(5 * time.Minute))
		exportsH := newExportsHandler(customerStore, invoiceStore, subStore, usageStore, auditLogger, auditLogger)
		r.Mount("/", exportsH.Routes(
			auth.Require(auth.PermCustomerRead),
			auth.Require(auth.PermInvoiceRead),
			auth.Require(auth.PermSubscriptionRead),
			auth.Require(auth.PermUsageRead),
			auth.Require(auth.PermAPIKeyRead),
		))
	})

	// Tenant-scoped routes — accept either an httpOnly session cookie
	// (dashboard, minted via /v1/auth/login) OR
	// `Authorization: Bearer <api_key>` (SDKs, curl, server-to-server).
	// Cookie takes precedence when both are present so a stale Bearer
	// header doesn't bypass session revocation. Both code paths set
	// the same auth ctx keys so handlers don't branch.
	r.Route("/v1", func(r chi.Router) {
		// CSRF: this group serves BOTH the cookie dashboard and bearer SDK/API
		// callers. The guard origin-checks unsafe COOKIE-path requests and exempts
		// bearer requests (a non-ambient credential can't be CSRF'd). Runs before
		// auth so a cross-site browser mutation is refused before touching session
		// state. Safe methods and the SSE/export GET siblings are unaffected.
		r.Use(mw.CSRFGuard(allowedOrigins))
		r.Use(session.MiddlewareOrAPIKey(sessionSvc, authSvc))
		// After auth so tenant ID is available for the bucket key. Ingest
		// paths ride their own (much larger) bucket — see ingestLimiter.
		r.Use(mw.SplitRateLimit(isIngestPath, ingestLimiter, rateLimiter))
		r.Use(mw.Idempotency(db))
		// No audit middleware here. Audit rows are EMITTED by the code that
		// owns the mutation, inside its transaction (audit.Logger.LogInTx), and
		// every mutating route declares where its evidence comes from in the
		// route-audit registry (audit_routes.go). The root-mounted
		// mw.AuditCoverage detector reports any route that stops emitting.
		// Per-request timeout for ordinary CRUD endpoints. The SSE
		// stream lives above this block (it's mounted on a sibling
		// /v1/webhook_events/stream route) so it doesn't inherit the
		// 30s cap — long-lived EventSource connections need to run
		// indefinitely.
		r.Use(middleware.Timeout(30 * time.Second))

		// /v1/whoami — resolves the active credential to its tenant
		// context without requiring a specific permission. Cookie path
		// returns user_id + email (dashboard); Bearer path returns
		// key_id + key_type (SDK / curl). Both always return tenant_id
		// + livemode. The dashboard's AuthContext relies on user_id
		// being present on the cookie path — without it, refresh()
		// silently logs the operator out.
		r.Get("/whoami", func(w http.ResponseWriter, req *http.Request) {
			ctx := req.Context()
			out := map[string]any{
				"tenant_id": auth.TenantID(ctx),
				"livemode":  auth.Livemode(ctx),
			}
			if uid := auth.UserID(ctx); uid != "" {
				out["user_id"] = uid
				if u, err := userSvc.GetByID(ctx, uid); err == nil {
					out["email"] = u.Email
				}
			}
			if kid := auth.KeyID(ctx); kid != "" {
				out["key_id"] = kid
				out["key_type"] = string(auth.GetKeyType(ctx))
			}
			respond.JSON(w, req, http.StatusOK, out)
		})

		r.With(auth.Require(auth.PermAPIKeyWrite)).Mount("/api-keys", authH.Routes())
		// Team membership — dashboard sessions only. API keys are machine
		// credentials with no user identity to attribute invites/removals
		// to, so they 403 here (the service double-checks on invite).
		r.With(auth.RequireSession()).Mount("/members", membersH.Routes())
		// /customers subtree mixes reads (GET list/get/billing-profile) and
		// writes (POST create, PATCH update, PUT billing-profile, GDPR
		// delete-data). RequireMethod splits the gate by HTTP method so a
		// read-tier key can list/get but cannot create/update/delete. (NOT a
		// publishable key: those hold the EMPTY permission set and cannot read
		// customers at all — auth/permission.go. Pre-fix the entire subtree was
		// gated on PermCustomerRead, letting any read-tier key write.)
		r.With(auth.RequireMethod(auth.PermCustomerRead, auth.PermCustomerWrite)).Mount("/customers", customerH.Routes())
		// Customer-scoped usage view — composes usage aggregation with
		// customer / subscription / pricing reads. PermUsageRead so the
		// dashboard's read-only secret-tier key can call it without
		// inheriting customer write capability.
		r.Mount("/customers/{id}/usage", customerUsageH.CustomerUsageRoutes(
			auth.Require(auth.PermUsageRead),
		))
		// Per-customer margin report (ADR-079): rated usage revenue vs
		// stamped provider COGS. Operator-auth only — COGS never renders
		// on customer-facing surfaces (the hosted cost dashboard included).
		r.With(auth.Require(auth.PermUsageRead)).Get("/customers/{id}/margin", providerCostH.Margin)
		// Provider cost rates CRUD (ADR-079): the operator's COGS table.
		r.With(auth.RequireMethod(auth.PermUsageRead, auth.PermUsageWrite)).Mount("/provider-costs", providerCostH.Routes())
		// Operator-side payment-method management. Same Service as the
		// /v1/me/payment-methods customer-facing surface — only the
		// auth model + the customer_id source differ. List/setDefault/
		// detach + a "send setup link" affordance that mints a Stripe
		// Checkout setup URL the operator can hand to the customer.
		// Card capture stays in the Stripe-hosted iframe so the
		// operator's PCI scope stays SAQ-A. Industry parity:
		// Chargebee, Lago, Orb expose the same shape.
		r.With(auth.RequireMethod(auth.PermCustomerRead, auth.PermCustomerWrite)).Mount("/customers/{customer_id}/payment-methods", paymentMethodsH.OperatorRoutes())
		r.With(auth.RequireMethod(auth.PermPricingRead, auth.PermPricingWrite)).Mount("/meters", pricingH.MeterRoutes())
		// Meter-scoped pricing rule subtree. Mounted as a sibling of
		// /meters so reads (PermPricingRead) and writes (PermPricingWrite)
		// can carry independent guards without nesting permission middleware.
		r.Mount("/meters/{meter_id}/pricing-rules", pricingH.MeterPricingRuleRoutes(
			auth.Require(auth.PermPricingRead),
			auth.Require(auth.PermPricingWrite),
		))
		r.With(auth.Require(auth.PermPricingRead)).Mount("/meter-pricing-rules", pricingH.AllMeterPricingRuleRoutes())
		r.With(auth.RequireMethod(auth.PermPricingRead, auth.PermPricingWrite)).Mount("/plans", pricingH.PlanRoutes())
		r.With(auth.RequireMethod(auth.PermPricingRead, auth.PermPricingWrite)).Mount("/rating-rules", pricingH.RatingRuleRoutes())
		r.With(auth.Require(auth.PermPricingWrite)).Mount("/recipes", recipeH.Routes())
		r.With(auth.RequireMethod(auth.PermSubscriptionRead, auth.PermSubscriptionWrite)).Mount("/subscriptions", subH.Routes())
		// Backfill is mounted ahead of the /usage-events subtree so chi picks
		// the more-specific pattern; PermUsageWrite gates it to secret-tier
		// keys (publishable keys are read-only).
		r.With(auth.Require(auth.PermUsageWrite)).Post("/usage-events/backfill", usageH.Backfill)
		r.With(auth.RequireMethod(auth.PermUsageRead, auth.PermUsageWrite)).Mount("/usage-events", usageH.Routes())
		// LiteLLM adapter — POST /v1/integrations/litellm/spend.
		// Authenticated with a standard secret key (the operator's
		// LiteLLM proxy carries the Bearer token on every callback);
		// gated on PermUsageWrite because the call shape is "write
		// usage events on behalf of the partner." See ADR-033 +
		// docs/integrations/litellm.md.
		r.With(auth.Require(auth.PermUsageWrite)).Mount("/integrations/litellm",
			litellm.New(customerStore, pricingSvc, usageSvc).Routes())
		// create_preview must mount BEFORE /invoices because chi tries
		// patterns in registration order — once /invoices is mounted with
		// /{id}/... children, "create_preview" would be claimed as an
		// invoice ID. See docs/design-create-preview.md.
		r.With(auth.Require(auth.PermInvoiceRead)).Mount("/invoices/create_preview", createPreviewH.Routes())
		r.With(auth.RequireMethod(auth.PermInvoiceRead, auth.PermInvoiceWrite)).Mount("/invoices", invoiceH.Routes())
		r.With(auth.Require(auth.PermInvoiceWrite)).Mount("/credit-notes", creditNoteH.Routes())
		r.With(auth.Require(auth.PermPricingWrite)).Mount("/price-overrides", pricingH.OverrideRoutes())
		r.With(auth.Require(auth.PermCustomerWrite)).Mount("/credits", creditH.Routes())
		r.With(auth.RequireMethod(auth.PermDunningRead, auth.PermDunningWrite)).Mount("/dunning", dunningH.Routes())
		r.With(auth.Require(auth.PermInvoiceWrite)).Mount("/billing", billingH.Routes())
		r.With(auth.Require(auth.PermAPIKeyWrite)).Mount("/webhook-endpoints", webhookOutH.Routes())
		// Week 6 real-time event UI — replay + deliveries-timeline live
		// here (the SSE stream itself mounts ABOVE /v1 because chi's
		// 30s middleware.Timeout would kill long-lived streams). Uses
		// underscore (webhook_events) on purpose so the shape matches
		// Stripe's /v1/events convention and doesn't collide with the
		// legacy /webhook-endpoints/events path.
		//
		// PermAPIKeyRead is sufficient for both read (deliveries) and
		// replay — replay is state-mutating but is functionally a
		// re-issue of an already-emitted event, the same posture the
		// legacy /webhook-endpoints/events/{id}/replay surface takes.
		r.With(auth.Require(auth.PermAPIKeyRead)).Mount("/webhook_events", webhookOutH.EventRoutes())
		r.With(auth.Require(auth.PermAPIKeyRead)).Mount("/audit-log", auditH.Routes())
		r.With(auth.Require(auth.PermAPIKeyWrite)).Mount("/settings", settingsH.Routes())
		r.With(auth.Require(auth.PermAPIKeyWrite)).Mount("/settings/stripe", tenantStripeH.Routes())
		r.With(auth.Require(auth.PermInvoiceRead)).Mount("/analytics", analyticsH.Routes())
		r.With(auth.Require(auth.PermTestClockWrite)).Mount("/test-clocks", testClockH.Routes())
		r.With(auth.Require(auth.PermUsageRead)).Mount("/usage-summary", usageH.SummaryRoutes())
		if checkoutH != nil {
			r.With(auth.Require(auth.PermCustomerWrite)).Mount("/checkout", checkoutH.Routes())
		}

		// Customer portal — consolidated views across domains
		portal := newCustomerPortalHandler(subStore, invoiceStore, usageStore)
		r.With(auth.Require(auth.PermCustomerRead)).Mount("/customer-portal", portal.Routes())
		// Legacy /v1/payment-portal/{id}/update-payment-method removed:
		// all "add a payment method" flows now go through
		// paymentmethods.Service.CreateSetupSession (single path).
		// Operator dashboard uses POST /v1/customers/{id}/payment-methods/send-setup-email
		// (email) or POST /v1/customers/{id}/payment-methods/setup-session (copy-link).
	})

	s.router = r
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.router.ServeHTTP(w, r)
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	respond.JSON(w, r, http.StatusOK, map[string]string{"status": "ok"})
}

func handleDeepHealth(db *postgres.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()

		overallStatus := "ok"
		checks := map[string]string{"api": "ok"}

		// Database check
		if err := db.Pool.PingContext(ctx); err != nil {
			checks["database"] = "error: " + err.Error()
			overallStatus = "degraded"
		} else {
			checks["database"] = "ok"
		}

		// Scheduler check — degraded if no run recorded within 2x the interval.
		// Before the first run completes the scheduler is considered ok (just started).
		schedulerMu.RLock()
		lastRun := schedulerLastRun
		interval := schedulerInterval
		schedulerMu.RUnlock()

		if interval > 0 && !lastRun.IsZero() {
			sinceLastRun := time.Since(lastRun)
			if sinceLastRun > 2*interval {
				checks["scheduler"] = fmt.Sprintf("degraded: last run %s ago (interval %s)", sinceLastRun.Truncate(time.Second), interval)
				overallStatus = "degraded"
			} else {
				checks["scheduler"] = fmt.Sprintf("ok: last run %s ago", sinceLastRun.Truncate(time.Second))
			}
		} else if interval > 0 && lastRun.IsZero() {
			checks["scheduler"] = "ok: awaiting first run"
		} else {
			checks["scheduler"] = "ok: not configured"
		}

		status := http.StatusOK
		if overallStatus != "ok" {
			status = http.StatusServiceUnavailable
		}

		respond.JSON(w, r, status, map[string]any{
			"status": overallStatus,
			"checks": checks,
		})
	}
}

// requestLogger writes the access log.
//
// It logs WHO, not just what. The access log used to carry only method, path,
// status and duration — which meant that after an incident it could answer "was
// there a 500 on this route" and nothing else. It could not answer the two
// questions that actually get asked: "what did this actor do", and "which tenant
// was affected". And with no request_id on the line, a log entry could not be
// joined to the audit row written by the same request, even though the audit row
// has carried request_id all along.
//
// The identity fields come from the SAME resolver the audit rows use
// (audit.ResolveActor) — deliberately, so a request cannot be one actor in the log
// and a different actor in the evidence.
//
// The actor NAME is not logged, only the id: names are emails for user actors, and
// logs ship to sinks with no erasure story (the same reason 0150's append-only
// audit_log stores no addresses either).
func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)

		// Read identity AFTER the handler: auth runs downstream of this middleware,
		// so before ServeHTTP the context is still anonymous.
		ctx := r.Context()
		actorType, actorID := audit.ResolveActor(ctx)

		attrs := []any{
			"method", r.Method,
			"path", loggablePath(r),
			"status", ww.Status(),
			"duration_ms", time.Since(start).Milliseconds(),
			"actor_type", actorType,
			// Never empty: ResolveActor answers "system"/"system" for an
			// unauthenticated request, which is a real answer rather than a missing
			// one — and is what the audit row would record for the same request.
			"actor_id", actorID,
			// Joins this line to the audit row the same request wrote.
			"request_id", middleware.GetReqID(ctx),
			"ip", audit.ClientIP(ctx),
		}
		// Omit rather than log empty: an unauthenticated request has no tenant, and a
		// blank field reads as "we lost it" instead of "there wasn't one".
		if tenantID := auth.TenantID(ctx); tenantID != "" {
			attrs = append(attrs, "tenant_id", tenantID)
		}
		slog.Info("request", attrs...)
	})
}

// loggablePath returns the request path with capability tokens removed.
// The public surfaces carry bearer-equivalent secrets IN the path
// (/v1/public/invoices/{256-bit token}, /v1/public/payment-updates/
// {single-use token}) — logging the raw path writes live payment
// credentials into every log sink. Prefer chi's matched route pattern
// ("/v1/public/invoices/{token}"); when no pattern matched (404s),
// redact the segment after any known public token prefix instead —
// unmatched probes against those prefixes still contain candidate
// secrets.
func loggablePath(r *http.Request) string {
	if rctx := chi.RouteContext(r.Context()); rctx != nil {
		if pat := rctx.RoutePattern(); pat != "" {
			return pat
		}
	}
	path := r.URL.Path
	for _, prefix := range []string{"/v1/public/invoices/", "/v1/public/payment-updates/"} {
		if rest, ok := strings.CutPrefix(path, prefix); ok && rest != "" {
			// Keep any sub-path (e.g. /pdf, /checkout) after the token.
			if i := strings.IndexByte(rest, '/'); i >= 0 {
				return prefix + "[redacted]" + rest[i:]
			}
			return prefix + "[redacted]"
		}
	}
	return path
}

// envInt reads an integer env override, falling back on empty/malformed
// values. Used for the rate-limit knobs — a bad value silently reverting
// to the default is acceptable here (the default is safe; a boot failure
// over a limiter tweak is not).
func envInt(key string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

// isIngestPath scopes the dedicated ingest rate-limit bucket: the
// usage-events subtree (single, batch, backfill — reads ride along
// harmlessly) and the LiteLLM adapter, which POSTs one callback per LLM
// call and must never be squeezed through the operator-CRUD bucket.
func isIngestPath(r *http.Request) bool {
	return strings.HasPrefix(r.URL.Path, "/v1/usage-events") ||
		strings.HasPrefix(r.URL.Path, "/v1/integrations/litellm")
}
