package payment

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	mw "github.com/sagarsuperuser/velox/internal/api/middleware"
	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/payment/breaker"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// ReconcileInvoiceStore is the narrow interface the reconciler needs from
// the invoice store. Kept separate from InvoiceUpdater so the reconciler
// can be wired independently of the hot charge / webhook paths.
type ReconcileInvoiceStore interface {
	ListUnknownPayments(ctx context.Context, olderThan time.Time, limit int) ([]domain.Invoice, error)
	// ListProcessingPayments returns invoices stuck in payment_status
	// 'processing' older than the (longer) processing cool-off — the dropped-
	// webhook backstop (ADR-049 Phase 2). A healthy card PI resolves in seconds
	// via webhook and async methods legitimately sit in processing for days, so
	// this window is much longer than the unknown one to keep the webhook
	// winning the common race.
	ListProcessingPayments(ctx context.Context, olderThan time.Time, limit int) ([]domain.Invoice, error)
	// Get re-reads an invoice fresh immediately before settling, so a webhook
	// that won the race during the GetPaymentIntent round-trip is observed
	// (the snapshot from the sweep list may be stale).
	Get(ctx context.Context, tenantID, id string) (domain.Invoice, error)
	// RecordProviderSync stamps the provider's verbatim PaymentIntent status
	// and the moment we observed it, for the non-terminal case this sweep
	// cannot settle. It is what makes the queue rotate rather than jam, and
	// what turns a long-running payment from silent into diagnosable
	// (migration 0167).
	RecordProviderSync(ctx context.Context, tenantID, id, providerStatus string) error
	// CountParkedInvoices reports invoices this sweep deliberately does NOT
	// process (ADR-107) — excluding them from the queue must not also make
	// them invisible.
	CountParkedInvoices(ctx context.Context) (open int, writtenOff int, err error)
	// ListParkedSearchable is the ADR-108 sweep's input: parked invoices
	// (unknown, empty PI id, finalized OR written off) old enough to search
	// and not searched
	// within the cool-off. Rotation, rate-pacing and the re-search interval
	// all live in its predicate.
	ListParkedSearchable(ctx context.Context, olderThan time.Time, limit int) ([]domain.Invoice, error)
	// AdoptPaymentIntentIfParked stamps a search-found LIVE PaymentIntent onto
	// a parked invoice via a CAS on the full parked shape. false = another
	// path (usually the webhook) won the race; the caller does nothing.
	AdoptPaymentIntentIfParked(ctx context.Context, tenantID, id, paymentIntentID string) (bool, error)
	UpdatePayment(ctx context.Context, tenantID, id string, ps domain.InvoicePaymentStatus, stripePaymentIntentID, lastPaymentError string, paidAt *time.Time) (domain.Invoice, error)
	MarkPaid(ctx context.Context, tenantID, id string, stripePaymentIntentID string, paidAt time.Time) (domain.Invoice, error)
}

// Settler is the shared payment-settlement primitive (ADR-049): it owns the
// complete terminal-transition side-effects (mark + sim-time, card stamp,
// payment.succeeded/failed event, dunning, customer email) plus the
// out-of-order guard. Implemented by *Stripe. The reconciler routes its
// recovered terminals through it so a dropped-webhook recovery fires the SAME
// consequences as the webhook — closing the silent-under-collection gap.
//
// Optional (nil-tolerant): when unwired — narrow unit tests only; production
// always wires it via SetSettler — the reconciler falls back to the legacy bare
// MarkPaid / UpdatePayment writes (no dunning/event/email). The fallback exists
// solely so status-discovery tests need not construct a full Stripe adapter.
type Settler interface {
	SettleSucceeded(ctx context.Context, tenantID string, inv domain.Invoice, paymentIntentID string, capturedCents int64, source SettlementSource) error
	SettleFailed(ctx context.Context, tenantID string, inv domain.Invoice, paymentIntentID, failureMsg string, suppressCustomerEmail bool, source SettlementSource) error
}

// Reconciler resolves invoices stuck in PaymentUnknown by asking Stripe for
// the authoritative PaymentIntent state. An invoice lands in PaymentUnknown
// when ChargeInvoice returns an ambiguous error (5xx / timeout / connection
// reset) — Stripe might have processed the charge server-side, so we must
// not blindly retry.
//
// Cool-off window: unknown invoices younger than `olderThan` are skipped to
// let the webhook (payment_intent.succeeded / failed) arrive first, which
// resolves the state without an extra API call.
type Reconciler struct {
	client    StripeClient
	invoices  ReconcileInvoiceStore
	olderThan time.Duration // cool-off for the 'unknown' sweep (short)
	// olderThanProcessing is the cool-off for the stale-'processing' sweep —
	// much longer than olderThan so the webhook wins the common race and
	// async methods (which legitimately sit in processing for days) aren't
	// hammered every tick. Defaults to 30m (ADR-049 Phase 2).
	olderThanProcessing time.Duration
	now                 func() time.Time // injectable for tests
	breaker             *breaker.Breaker // optional; skip reconcile ticks when breaker is open
	// settler routes recovered terminals through the shared settlement
	// primitive so they fire the full side-effects (dunning/event/email).
	// Nil-tolerant: see Settler.
	settler Settler
	// resolver binds ctx to effective-now from the invoice's pin before
	// MarkPaid so clock-pinned invoices stamp `paid_at` in simulated
	// time. Wired via SetResolver from router.go (same pattern as
	// payment.Stripe + dunning.Handler). Nil-tolerant: when unwired
	// (tests, bare construction) paid_at falls back to r.now() wall-
	// clock — that's the legacy behavior, preserved so the constructor
	// stays zero-arg.
	resolver clock.Resolver
	// searchDisabled remembers account+mode pairs whose provider refused the
	// Search API itself (ErrSearchNotOffered — a request-shape error that
	// will not heal). Announce-once: first sight logs CRITICAL, later ticks
	// skip silently, and those tenants keep exactly the pre-ADR-108 behavior
	// (parked + gauge + write-off exit). Process-lifetime is deliberate: a
	// provider enabling search for an account is a deploy-scale event, not a
	// tick-scale one.
	searchDisabled map[string]bool
}

// NewReconciler constructs a Reconciler. If olderThan <= 0, defaults to 60s
// (the 'unknown' cool-off). The stale-'processing' cool-off defaults to 30m;
// override with SetProcessingReconcileAfter.
func NewReconciler(client StripeClient, invoices ReconcileInvoiceStore, olderThan time.Duration) *Reconciler {
	if olderThan <= 0 {
		olderThan = 60 * time.Second
	}
	return &Reconciler{
		client:              client,
		invoices:            invoices,
		olderThan:           olderThan,
		olderThanProcessing: 30 * time.Minute,
		now:                 func() time.Time { return time.Now().UTC() },
		searchDisabled:      map[string]bool{},
	}
}

// SetSettler wires the shared settlement primitive (production: *Stripe). When
// unset, recovered terminals fall back to legacy bare writes (test-only).
func (r *Reconciler) SetSettler(s Settler) { r.settler = s }

// SetProcessingReconcileAfter overrides the stale-'processing' cool-off. The
// caller picks it relative to the scheduler tick so the webhook wins the common
// race: long enough that a healthy card PI always resolves via webhook first,
// short enough that a dropped webhook resolves within an acceptable window.
func (r *Reconciler) SetProcessingReconcileAfter(d time.Duration) {
	if d > 0 {
		r.olderThanProcessing = d
	}
}

// SetBreaker wires the same global circuit breaker used by ChargeInvoice.
// When the breaker is open, the reconciler skips unresolved invoices this
// tick rather than piling Stripe reads onto an already-sick service; the
// next tick after cooldown picks them up.
func (r *Reconciler) SetBreaker(b *breaker.Breaker) {
	r.breaker = b
}

// SetResolver wires the clock resolver so `paid_at` writes land in
// simulated time on clock-pinned invoices. Without this the reconciler
// would call MarkPaid with wall-clock — leaking wall-clock onto an
// invoice whose every other timestamp (issued_at, due_at, billing
// period) lives on the simulated timeline. Same shape as
// payment.Stripe.SetResolver + dunning.Handler.SetResolver.
func (r *Reconciler) SetResolver(res clock.Resolver) { r.resolver = res }

// Run reconciles unresolved in-flight invoices against Stripe in two sweeps:
//   - 'unknown' (ambiguous charge outcomes) past the short cool-off, and
//   - stale 'processing' (the dropped-webhook backstop, ADR-049 Phase 2) past
//     the longer processing cool-off.
//
// Both route recovered terminals through the same path (reconcileOne →
// settler), so a backstop-recovered settlement fires the full side-effects.
// Returns the number resolved (succeeded or failed) and any per-invoice errors.
func (r *Reconciler) Run(ctx context.Context, limit int) (int, []error) {
	// Report the parked set before sweeping. These rows are excluded from the
	// queries below by design; publishing the count is what keeps "stuck and
	// loud" (ADR-107) actually loud, instead of log-only.
	if open, writtenOff, err := r.invoices.CountParkedInvoices(ctx); err == nil {
		mode := "live"
		if !postgres.Livemode(ctx) {
			mode = "test"
		}
		mw.RecordParkedInvoices(mode, "open", open)
		mw.RecordParkedInvoices(mode, "written_off", writtenOff)
	} else {
		slog.WarnContext(ctx, "could not count parked invoices for the gauge", "error", err)
	}

	n1, e1 := r.sweep(ctx, "unknown", r.invoices.ListUnknownPayments, r.olderThan, limit)
	n2, e2 := r.sweep(ctx, "processing", r.invoices.ListProcessingPayments, r.olderThanProcessing, limit)
	n3, e3 := r.sweepParked(ctx, limit)
	errs := append(e1, e2...)
	return n1 + n2 + n3, append(errs, e3...)
}

// parkedSearchAge is how long an invoice must have been parked before the
// ADR-108 sweep spends a search on it — 60x Stripe's documented normal search
// indexing lag ("under 1 minute"), so the ordinary payment_intent.* webhook
// wins the common race and early empty results (which we would ignore anyway)
// are mostly avoided.
const parkedSearchAge = time.Hour

// sweepParked is the ADR-108 search-and-adopt sweep: for each parked invoice,
// ask Stripe for the PaymentIntents carrying its velox_invoice_id metadata and
// act on POSITIVE evidence only.
//
//   - any SUCCEEDED PI  -> settle paid with it (newest first). Money moved;
//     recording it is the resolution ADR-107 always wanted.
//   - else any LIVE PI  -> CAS-adopt the newest (unknown -> processing WITH an
//     id); the invoice joins the ordinary sweeps and resolves there.
//   - terminal-only / nothing / error -> record an observation and WRITE NO
//     MONEY OUTCOME. Absence from an eventually-consistent index proves
//     nothing — the outage that mints a parked row is the same weather that
//     delays its PI's indexing, and a stale PI from a previous attempt can be
//     indexed while the one that matters is not (both attack-verified,
//     ADR-108). The deleted give-up write stays deleted.
func (r *Reconciler) sweepParked(ctx context.Context, limit int) (int, []error) {
	before := r.now().Add(-parkedSearchAge)
	invoices, err := r.invoices.ListParkedSearchable(ctx, before, limit)
	if err != nil {
		return 0, []error{fmt.Errorf("list parked searchable: %w", err)}
	}

	mode := "live"
	if !postgres.Livemode(ctx) {
		mode = "test"
	}

	var errs []error
	resolved := 0
	for _, inv := range invoices {
		if r.searchDisabled[inv.TenantID+"/"+mode] {
			continue
		}
		ok, serr := r.searchAndAdoptOne(ctx, inv, mode)
		if serr != nil {
			errs = append(errs, serr)
		}
		if ok {
			resolved++
		}
	}
	return resolved, errs
}

func (r *Reconciler) searchAndAdoptOne(ctx context.Context, inv domain.Invoice, mode string) (bool, error) {
	// Per-tenant credentials, same as reconcileOne: background ctx carries no
	// auth, and the search must run under THIS invoice's tenant + mode key.
	ctx = auth.WithTenantID(ctx, inv.TenantID)

	results, err := r.client.SearchPaymentIntentsByInvoiceID(ctx, inv.ID)
	switch {
	case errors.Is(err, ErrStripeNotConfigured):
		// No credentials for this mode (common in dev) — not a provider
		// refusal; skip quietly and let the next tick retry.
		return false, nil
	case errors.Is(err, ErrSearchNotOffered):
		// Request-shape refusal (the documented India case and its kin).
		// Disable for this account+mode and say so ONCE — those tenants keep
		// exactly the pre-ADR-108 behavior, visibly (the parked gauge still
		// counts their rows and the write-off remains the exit).
		r.searchDisabled[inv.TenantID+"/"+mode] = true
		mw.RecordParkedSearchError(mode, "not_offered")
		slog.ErrorContext(ctx, "CRITICAL: payment provider refuses the Search API for this account — parked invoices here cannot self-resolve and keep the manual write-off exit (ADR-108)",
			"tenant_id", inv.TenantID, "mode", mode, "error", err)
		return false, nil
	case err != nil:
		// Transient (rate limit, 5xx, network). Stamp the row anyway — the
		// stamp is an honest observation ("we asked; the call failed") and it
		// is what keeps the rotation moving: an un-stamped erroring row would
		// hold the head of this LIMITed queue forever, the exact starvation
		// shape 0167 fixed for the sibling sweeps.
		mw.RecordParkedSearchError(mode, "transient")
		r.recordSync(ctx, inv, "search_error")
		slog.WarnContext(ctx, "parked search failed; will retry after the cool-off",
			"invoice_id", inv.ID, "tenant_id", inv.TenantID, "error", err)
		return false, nil
	}

	// Precedence is explicit and tested (attack-verified): a first-match loop
	// over an unordered result set can reach a wrong conclusion from a partial
	// view. Succeeded beats live; within a class, NEWEST by provider creation
	// time — the stale-history bias of an eventually-consistent index means
	// older attempts' PIs are always the better-indexed ones.
	var succeeded, live *PaymentIntentResult
	for i := range results {
		pi := results[i]
		switch pi.Status {
		case "succeeded":
			if succeeded == nil || pi.CreatedAt > succeeded.CreatedAt {
				succeeded = &results[i]
			}
		case "processing", "requires_action", "requires_confirmation", "requires_capture":
			if live == nil || pi.CreatedAt > live.CreatedAt {
				live = &results[i]
			}
		}
	}

	switch {
	case succeeded != nil:
		// Settle through the shared primitive — full side-effects, race-guarded
		// (settle re-reads fresh and skips an already-settled invoice). Any
		// SECOND succeeded PI is left to the webhook path, whose already-paid +
		// different-PI branch escalates the duplicate-charge anomaly.
		// suppressEmail=false: it only gates the FAILURE email, and this arm
		// settles success.
		done, serr := r.settle(ctx, inv, succeeded.ID, succeeded.AmountReceivedCents, false, "adopted: search found succeeded PI", terminalSucceeded, succeeded.Status)
		if serr != nil {
			return false, fmt.Errorf("settle search-found succeeded PI %s: %w", succeeded.ID, serr)
		}
		if done {
			slog.InfoContext(ctx, "parked invoice resolved by provider search — succeeded PaymentIntent adopted and settled",
				"invoice_id", inv.ID, "stripe_payment_intent_id", succeeded.ID)
		}
		return done, nil

	case live != nil:
		adopted, aerr := r.invoices.AdoptPaymentIntentIfParked(ctx, inv.TenantID, inv.ID, live.ID)
		if aerr != nil {
			return false, fmt.Errorf("adopt live PI %s: %w", live.ID, aerr)
		}
		if !adopted {
			// Another path won while we were searching — the COMMON race
			// (webhook and sweep both fire when the provider recovers). Losing
			// it is the designed outcome, not an error.
			slog.InfoContext(ctx, "parked adopt lost the race — another path already resolved the invoice",
				"invoice_id", inv.ID, "stripe_payment_intent_id", live.ID)
			return false, nil
		}
		slog.InfoContext(ctx, "parked invoice adopted a live PaymentIntent from provider search — ordinary reconciliation takes it from here",
			"invoice_id", inv.ID, "stripe_payment_intent_id", live.ID, "stripe_status", live.Status)
		return true, nil

	default:
		// Terminal-only or nothing at all. Either way: OBSERVE, never settle.
		// An empty result cannot prove no PI exists (indexing lags exactly in
		// the weather that creates parked rows), and a terminal-only result
		// cannot prove the CURRENT attempt is terminal (metadata carries the
		// invoice id, not the attempt id — the found set may be pure history).
		obs := "search_not_found"
		if len(results) > 0 {
			obs = "search_terminal_only"
		}
		r.recordSync(ctx, inv, obs)
		return false, nil
	}
}

// sweep lists invoices in one non-terminal state older than the given cool-off
// and reconciles each against Stripe.
func (r *Reconciler) sweep(ctx context.Context, label string, list func(context.Context, time.Time, int) ([]domain.Invoice, error), olderThan time.Duration, limit int) (int, []error) {
	before := r.now().Add(-olderThan)
	invoices, err := list(ctx, before, limit)
	if err != nil {
		return 0, []error{fmt.Errorf("list %s payments: %w", label, err)}
	}

	var (
		resolved int
		errs     []error
	)
	for _, inv := range invoices {
		changed, err := r.reconcileOne(ctx, inv)
		if err != nil {
			errs = append(errs, fmt.Errorf("invoice %s: %w", inv.ID, err))
			continue
		}
		if changed {
			resolved++
		}
	}
	return resolved, errs
}

type terminalKind int

const (
	terminalSucceeded terminalKind = iota
	terminalFailed
)

// reconcileOne queries Stripe for the PI and, on a TERMINAL status, settles the
// invoice through the shared primitive (succeeded / failed). A still-in-flight
// status is left for the webhook. Covers both the 'unknown' and stale-
// 'processing' sweeps.
func (r *Reconciler) reconcileOne(ctx context.Context, inv domain.Invoice) (bool, error) {
	// Stamp the invoice's tenant onto ctx so the per-tenant Stripe client
	// resolver can look up credentials. Background ctx has no auth.
	ctx = auth.WithTenantID(ctx, inv.TenantID)

	if inv.StripePaymentIntentID == "" {
		// DO NOTHING. This branch used to settle the invoice failed, with the
		// comment "a safe retry generates a new PI" — which was the whole bug:
		// an ambiguous charge may have left a PaymentIntent live at Stripe that
		// we never learned the name of, and settling failed both bumps
		// charge_attempt_seq (rotating the idempotency key) and moves
		// payment_status into a claimable state, so the next retry opens a
		// SECOND PaymentIntent beside the live one.
		//
		// Leaving it at 'unknown' closes that by construction, because every
		// charge-claim predicate already excludes it:
		//
		//   ClaimAutoCharge            payment_status = 'pending'
		//   ClaimChargeForManualCollect payment_status IN ('pending','failed')
		//   ClaimChargeForDunningRetry  payment_status IN ('pending','failed')
		//
		// So no sweep, no operator click and no dunning retry can re-charge it.
		// The invoice is stuck and LOUD rather than silently double-charged,
		// which is the trade this makes deliberately (ADR-107).
		//
		// It is not a dead end: the ordinary payment_intent.* webhook names the
		// PaymentIntent and settles the invoice through the normal path, which
		// is what happens in almost every real occurrence. Only a LOST webhook
		// plus a lost response needs a human.
		//
		// UNREACHABLE FROM THE SWEEP since #682: ListUnknownPayments excludes
		// empty-PI rows at SQL (they are not reconcilable, and they starved the
		// queue). This branch is kept as defense-in-depth so a future widening
		// of the sweep's predicate cannot silently resurrect the give-up write
		// this ADR deleted — if it ever logs, the sweep's SQL has drifted.
		//
		// The advice below matches the payment gate: Void is REFUSED on a
		// parked invoice (a voided-then-succeeded invoice is a contradiction);
		// mark-uncollectible is the one allowed exit. A prior version of this
		// message said "settle or void it by hand" — advice the gate refuses,
		// found by the 2026-08-02 design-review panel.
		slog.ErrorContext(ctx, "CRITICAL: an ambiguous charge left no PaymentIntent id — this invoice cannot be reconciled automatically and is deliberately parked rather than risk a second charge. Find the attempt in the Stripe dashboard (search by customer and amount): if money was taken, the payment webhook settles it once Stripe reports it; if nothing was taken, mark the invoice uncollectible",
			"invoice_id", inv.ID, "tenant_id", inv.TenantID,
			"customer_id", inv.CustomerID, "amount_due_cents", inv.AmountDueCents,
			"invoice_number", inv.InvoiceNumber)
		return false, nil
	}

	var res PaymentIntentResult
	var err error
	if r.breaker != nil {
		var out any
		out, err = r.breaker.Execute(ctx, func(ctx context.Context) (any, error) {
			return r.client.GetPaymentIntent(ctx, inv.StripePaymentIntentID)
		})
		if errors.Is(err, breaker.ErrOpen) {
			// Breaker open — don't pile reads onto Stripe while it's
			// struggling; return silently so Run() doesn't log a per-invoice
			// error for every unresolved invoice.
			return false, nil
		}
		if out != nil {
			res = out.(PaymentIntentResult)
		}
	} else {
		res, err = r.client.GetPaymentIntent(ctx, inv.StripePaymentIntentID)
	}
	if err != nil {
		// Stripe itself returned 5xx on the reconcile query — leave the
		// invoice in its current state, try again next tick.
		return false, fmt.Errorf("get payment intent %s: %w", inv.StripePaymentIntentID, err)
	}

	switch res.Status {
	case "succeeded":
		return r.settle(ctx, inv, res.ID, res.AmountReceivedCents, false, "", terminalSucceeded, res.Status)

	case "canceled", "requires_payment_method":
		// Replicate the webhook's customer-email suppression from the PI
		// purpose: a dunning-retry PI already sent its own per-attempt email,
		// and a hosted-pay PI's decline was shown inline. Other failures send.
		suppressEmail := res.Purpose == "hosted_invoice_pay" || res.Purpose == "dunning_retry"
		return r.settle(ctx, inv, res.ID, 0, suppressEmail, "reconciled: "+res.Status, terminalFailed, res.Status)

	case "processing", "requires_action", "requires_confirmation", "requires_capture":
		// Not terminal, so nothing settles here — but RECORD WHAT WE OBSERVED
		// (migration 0167). This branch used to return with no write at all.
		//
		// That mattered because the sweep is LIMITed and ordered by how
		// recently we looked: a row nothing ever writes never ages, so it
		// headed the queue permanently and, once batch-size of them
		// accumulated, newly-ambiguous charges were never reached at all. It is
		// not a hypothetical shape — this package's own contract notes that
		// "async methods legitimately sit in processing for days", and those
		// are exactly the rows that used to freeze.
		//
		// Recording the observation makes the queue rotate instead, and makes a
		// long-running payment diagnosable ("stuck at requires_action") rather
		// than merely silent.
		r.recordSync(ctx, inv, res.Status)
		slog.Debug("reconcile: PI still in flight, recorded and skipping",
			"invoice_id", inv.ID, "stripe_payment_intent_id", res.ID, "stripe_status", res.Status)
		return false, nil

	default:
		return false, fmt.Errorf("unexpected stripe PI status %q", res.Status)
	}
}

// recordSync stamps the provider's verbatim status and the moment we observed
// it. Best-effort by design: a failed stamp must not fail the sweep or mask the
// real payment state. The only cost of losing one is that this row is polled
// again sooner than strictly necessary — the safe direction, since the write
// exists to stop a never-resolving row from monopolising the queue.
func (r *Reconciler) recordSync(ctx context.Context, inv domain.Invoice, providerStatus string) {
	if err := r.invoices.RecordProviderSync(ctx, inv.TenantID, inv.ID, providerStatus); err != nil {
		slog.WarnContext(ctx, "reconcile: could not record provider sync",
			"invoice_id", inv.ID, "stripe_status", providerStatus, "error", err)
	}
}

// settle re-reads the invoice fresh (race guard: a webhook may have settled it
// during the GetPaymentIntent round-trip) and routes the terminal transition
// through the settlement primitive so it fires the full side-effects (dunning,
// event, email, card stamp) — identical to the webhook (ADR-049 Phase 2).
// Falls back to legacy bare writes when no settler is wired (test-only).
func (r *Reconciler) settle(ctx context.Context, inv domain.Invoice, piID string, capturedCents int64, suppressEmail bool, failMsg string, kind terminalKind, providerStatus string) (bool, error) {
	// Fresh re-read so a webhook that won the race during the round-trip is
	// observed (the sweep-list snapshot may be stale). On read error, proceed
	// with the snapshot — the primitive's own guards still apply.
	fresh := inv
	if got, gerr := r.invoices.Get(ctx, inv.TenantID, inv.ID); gerr == nil {
		fresh = got
	}
	// Already settled by another path during the round-trip — the webhook won;
	// skip to avoid a duplicate receipt (succeeded) or a duplicate
	// dunning-advance + email (failed).
	if fresh.Status == domain.InvoicePaid ||
		fresh.PaymentStatus == domain.PaymentSucceeded ||
		fresh.PaymentStatus == domain.PaymentFailed {
		// Record the observation even though nothing settles: this was the one
		// return on the sweep path that wrote nothing, and a row that keeps
		// re-entering the LIMITed queue with provider_synced_at NULL heads it
		// forever (the 0167 shape). Ordinarily the winner's settle removes the
		// row from the sweep's predicate and this stamp is a harmless one-off —
		// but a row whose invoice status is terminal while payment_status stays
		// in-flight (no real writer produces one; seed/ops artifacts can) hits
		// this branch every tick, and two such rows were found monopolising the
		// walk DB's queue head, never stamped, since May 2026.
		r.recordSync(ctx, inv, providerStatus)
		slog.Info("reconcile: invoice already settled by another path, skipping",
			"invoice_id", inv.ID, "payment_status", fresh.PaymentStatus, "invoice_status", fresh.Status)
		return false, nil
	}

	if r.settler != nil {
		switch kind {
		case terminalSucceeded:
			// capturedCents = the PI's amount_received (not the pre-fix
			// literal 0) so the ADR-068 amount-mismatch alarm works on the
			// backstop path exactly as it does on the webhook path.
			if err := r.settler.SettleSucceeded(ctx, inv.TenantID, fresh, piID, capturedCents, SourceReconciler); err != nil {
				return false, fmt.Errorf("settle succeeded: %w", err)
			}
		default:
			if err := r.settler.SettleFailed(ctx, inv.TenantID, fresh, piID, failMsg, suppressEmail, SourceReconciler); err != nil {
				return false, fmt.Errorf("settle failed: %w", err)
			}
		}
		return true, nil
	}

	// Legacy fallback (no settler wired). Bare writes — no dunning/event/email.
	// Production ALWAYS wires the settler (router.go calls SetSettler right
	// after construction), so this branch is reachable only in narrow unit
	// tests. Log loudly rather than silently under-settling: if this ever fires
	// outside a test it means a misconfigured reconciler is dropping the
	// dunning/notification side-effects (the silent-under-collection this phase
	// exists to fix). Not an error return — the bare write still records the
	// terminal state, which is strictly better than leaving the invoice stuck.
	slog.Warn("reconcile: settler not wired — settling with INCOMPLETE side-effects (no dunning/event/email); production must SetSettler",
		"invoice_id", inv.ID, "terminal_succeeded", kind == terminalSucceeded)
	switch kind {
	case terminalSucceeded:
		markCtx := ctx
		if r.resolver != nil {
			markCtx, _ = clock.BindEffectiveNow(markCtx, r.resolver, clock.Pin{TenantID: inv.TenantID, InvoiceID: inv.ID})
		}
		now := clock.Now(markCtx)
		if _, err := r.invoices.MarkPaid(markCtx, inv.TenantID, inv.ID, piID, now); err != nil {
			return false, fmt.Errorf("mark paid (legacy): %w", err)
		}
		return true, nil
	default:
		if _, err := r.invoices.UpdatePayment(ctx, inv.TenantID, inv.ID,
			domain.PaymentFailed, piID, failMsg, nil); err != nil {
			return false, fmt.Errorf("mark failed (legacy): %w", err)
		}
		return true, nil
	}
}
