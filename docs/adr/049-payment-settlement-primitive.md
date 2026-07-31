# ADR-049: A single payment-settlement primitive (discover-then-settle)

- Status: Accepted
- Date: 2026-06-07
- Relates: ADR-001 (PaymentIntent-only Stripe), ADR-036 (dunning campaigns), ADR-030 (clock-pinned sim-time on financial writes)

## Context

A payment reaching a **terminal state** (`succeeded` / `failed`) is written in ~10 places across four packages, and each fires a *different subset* of the consequences:

**`→ succeeded` (`MarkPaid`)** — webhook `handlePaymentSucceeded` (`payment/stripe.go`) is the only complete one: sim-time `paid_at`, charged-card stamp, `payment.succeeded` event, receipt email. The others fire bare subsets — the reconciler (`payment/reconciler.go`), dunning-retry success (`dunning/handler.go`), out-of-band manual (`invoice/service.go`), and the engine/$0-credit paths (`billing/engine.go`, `billing/threshold_scan.go`).

**`→ failed` (`UpdatePayment(PaymentFailed)`)** — webhook `handlePaymentFailed` is again the only complete one: `payment.failed` event, dunning auto-start, customer email (suppressed for interactive/dunning-retry flows), out-of-order guard. The reconciler writes `failed` with **none** of that (a code comment there even *assumes* "the webhook runs in parallel" — but a **dropped** webhook is the exact case the reconciler-as-backstop exists for, so a backstop-recovered failure strands the invoice with no dunning, no event, no email = **silent under-collection**).

**Charge initiators** — cycle billing, threshold, portal "pay now", operator "charge now", finalize-time auto-charge — all create a PaymentIntent with `Confirm: true, OffSession: true` (so Stripe returns the real outcome **synchronously** in the create response) and then set `payment_status = processing` **unconditionally** (`payment/stripe.go`), discarding `result.Status` and waiting on a webhook that merely repeats what the response already said.

This is the classic overlapping-flows bug: several flows settle the same entity with **non-disjoint, drifted** side-effects. The visible symptom that surfaced it: a **test-clock**-pinned customer's auto-charge sat in `processing` forever — the create call returned `succeeded` synchronously *inside the Advance*, but Velox set `processing` and waited on a `payment_intent.succeeded` webhook that arrives in **wall-clock** time (and, in local dev, only when `stripe listen` is forwarding), fully decoupled from the simulated timeline. The reconciler can't help: it runs only on the wall-clock cron, over `payment_status = 'unknown'` (never `processing`, never during a test-clock Advance).

Industry (Stripe / Lago / Chargebee / Recurly, verified 2026-06-07) converges on: the provider's PaymentIntent is the authoritative state machine, the biller mirrors it, settlement happens only on a terminal signal, and **webhooks are primary truth but explicitly not infallible** — Stripe ships a reconciliation backstop (List Events `delivery_success=false`) precisely for dropped events. The problem isn't the *shape* (Velox already has the right states); it's that the **settlement action is not a single authoritative operation**.

## Decision

Introduce one idempotent **payment-settlement primitive** that owns the terminal transition **and the complete, correct side-effect set**. Every entry point becomes a thin *status discoverer* that hands the terminal outcome to the primitive:

- `SettleSucceeded(ctx, tenantID, inv, paymentIntentID, source)` — bind sim-time, `MarkPaid`, stamp the charged card, fire `payment.succeeded`, enqueue the receipt email. Idempotent (`MarkPaid` is a no-op on an already-paid invoice; the receipt/event are best-effort and log-only).
- `SettleFailed(ctx, tenantID, inv, paymentIntentID, failureMsg, suppressCustomerEmail, source)` — out-of-order guard (skip if already settled), `UpdatePayment(failed)`, fire `payment.failed`, auto-start dunning (sim-time anchored), enqueue the payment-failed email unless suppressed.

The four discoverers:

1. **Inbound webhook** — `payment_intent.succeeded` / `payment_intent.payment_failed` (primary truth).
2. **Charge synchronous response** — branch on `result.Status`: `succeeded` settles inline; `processing` / `requires_action` awaits the webhook + reconciler.
3. **Reconciler** — `GetPaymentIntent` for stale in-flight invoices (the dropped-webhook backstop), generalized from `unknown` to also cover stale `processing`.
4. **Operator "Check provider"** — on-demand reconcile from the attention banner.

Because all four route through the *same* primitive, a backstop-recovered settlement is **byte-identical** to a webhook one **by construction**, not by remembering to keep two code paths in sync.

**Scope boundary — "payment succeeded" ≠ "invoice settled without a charge."** The primitive consolidates *Stripe-PaymentIntent terminal outcomes only*. The engine/threshold `MarkPaid` calls for **$0 / fully-credit-covered** invoices move no money and must **not** fire a receipt email or `payment.succeeded` — they stay a separate "settle without payment" path. Out-of-band manual payments are their own operator-recorded path. The primitive does not absorb these; conflating them would email customers a "receipt" for a charge that never happened.

## Phased rollout

- **Phase 0** — this ADR.
- **Phase 1** — extract `SettleSucceeded` / `SettleFailed` and refactor the two webhook handlers onto them. **Behavior-preserving**; the existing webhook tests are the pin. **Shipped** (#188).
- **Phase 2** — wire the reconciler onto the primitive (fixes the silent under-collection for `unknown` *today*) and generalize its sweep to stale `processing` with its own cool-off (30m default) + a pre-write fresh-read race guard. The reconciler replicates the webhook's email-suppression from the PI `velox_purpose` (plumbed onto `GetPaymentIntent`). **Shipped** (#189).
- **Phase 3** — settle synchronously from the charge `result.Status` (fixes the test-clock symptom and all charge initiators at once, since they share `ChargeInvoice`). **Shipped** (#190).
- **Phase 4** — honest surfacing. **Shipped** (#191): the `processing` banner is age-aware (Info under the expected-settle window; Warning past it, pointing at Stripe and *not* promising auto-resolution for the stuck case), aged off wall-clock `updated_at` (no new column — deferred per below) and guarded to non-simulated invoices (a clock-pinned invoice's age is sim-time, not a real-world duration). The banner copy on both the `processing` and `unconfirmed` banners now states the truth that Phases 2+3 made real — Velox confirms/reconciles automatically.

  **Deviation from the original plan:** the on-demand "Check provider" action is **deferred**, not wired. Phases 2 (reconciler backstop) + 3 (synchronous inline settle) mean a `processing` invoice now resolves on its own (inline, or within the reconciler window); the manual re-check button — which never had a backend endpoint and shipped greyed-out — lost its necessity. The dead disabled button was **removed** rather than shipped non-functional (a greyed-out button is itself a small UI lie). Re-add trigger: a real production stuck-PI that inline-settle + the 30m reconciler don't clear, i.e. genuine operator pressure to force a re-query.

### Phase 3 note: synchronous settle vs webhook-as-source-of-truth (verified 2026-06-07)

The inline settle is a deliberate, bounded **optimization layered on top of** the webhook — not a claim that "settle on the synchronous response" is the industry-recommended pattern. Verified across Stripe's own docs + billing peers:

- **Stripe's headline recommendation is webhook-as-source-of-truth**, not the confirm response: *"Don't attempt to handle order fulfilment on the client side... use webhooks to monitor the `payment_intent.succeeded` event and handle its completion asynchronously"* ([verifying-status](https://docs.stripe.com/payments/payment-intents/verifying-status)). Inspectable peers agree — **Lago** persists the payment `pending` and flips it on the inbound webhook; **Orb** keys off `invoice.payment_succeeded`. None settle primarily on the synchronous response.
- **BUT for a server-side off-session card confirm** (`confirm=true, off_session=true` — Velox's exact pattern) Stripe officially supports trusting the response: the Confirm API *"Returns the resulting PaymentIntent after all possible transitions are applied"*, and Stripe ships an entire **"Accept card payments without webhooks"** integration. So a `2xx` with `status=succeeded` from our own server call is authoritative **for a card**. The webhook guidance is aimed at client-side confirmation (browser can navigate away) and async methods (status arrives later) — neither applies here.

Velox is therefore in the **defensible shape**, not the discouraged synchronous-**only** anti-pattern, because all three safety conditions hold:

1. **Cards only** — inline settle fires solely on `result.Status == "succeeded"`; `processing`/`requires_action`/etc. fall through to await the webhook, so no async method is mis-settled.
2. **Idempotency key on every create** (`velox_inv_<id>_…`) — a lost/timed-out response replays the original PI rather than double-charging.
3. **Lost-response recovery is explicit, not blind-retry** — a 5xx/timeout maps to `payment_status = unknown` (never `failed`); the reconciler then `GetPaymentIntent`s and settles through the same primitive. So a charge that succeeded-but-whose-response-was-lost is recovered, not missed.

   **Amended 2026-07-31 ([ADR-107](107-unknown-is-terminal-until-a-human.md)).**
   "The reconciler then `GetPaymentIntent`s" assumes we hold a PaymentIntent id.
   In the sub-case where the response was lost *before* we learned the id — a
   timeout with no body, or an ambiguous error carrying no PI — there is nothing
   to call `GetPaymentIntent` with, so this condition does not hold and recovery
   is not automatic. That invoice is **parked**: it stays `unknown`, no charge
   path admits it (which is what makes a double charge unreachable rather than
   merely guarded), and a human resolves it by writing it off. The condition as
   written holds whenever an id exists, which is the overwhelming majority;
   ADR-107 covers the remainder, and is where the parked state's operator
   surfaces, gauge and liveness rules live.

The webhook + reconciler remain idempotent backstops routing through the one primitive, so a backstop-recovered settlement is byte-identical to the inline one by construction. The only residual gap — post-success **disputes / reversals** — is the deferred async/dispute tail below, and is not a regression (webhook-only billers miss disputes too unless they subscribe to `charge.dispute.*`).

## Deferred (named triggers — the triggering surface does not exist yet)

- **Async payment methods** (ACH / SEPA / BACS direct debit). Trigger: first design partner who needs bank debit.
- **Dispute / late-failure handling** (`charge.dispute.*`, post-success reversal → counter-booking, à la Recurly). Trigger: an async method or dispute webhooks enabled.
- **Inbound `charge.refunded` reconciliation** (an out-of-band refund done directly in the Stripe dashboard mirrored as an offsetting credit-note ledger entry). **Not a correctness gap** — a refund does not un-pay the invoice in Velox's model (`creditnote/service.go` keeps `status=paid`, same as Stripe/Recurly), so money-state stays correct; this is ledger/reporting *completeness*. While operators refund **through Velox** (the credit-note refund path, which records `stripe_refund_id` + `RefundStatus`), there is zero drift; drift only appears for dashboard-side refunds. Trigger: operators begin refunding in the Stripe dashboard, or refund reconciliation/reporting becomes a requirement. **Interim operational guidance: issue refunds via Velox (credit notes), not the Stripe dashboard.** Verified MINOR in the 2026-06-07 e2e parity audit. The handler must dedup against Velox-initiated refunds (which also fire `charge.refunded`) to avoid double-counting — design that in when built. **Build-note (ingress trap):** today `charge.refunded` is not even *captured* — the inbound handler (`internal/payment/handler.go`) skips any event whose `data.object.metadata.velox_tenant_id` is empty, and Velox stamps that metadata on the **PaymentIntent**, not the **Charge**, so a `charge.refunded` (object = Charge) hits the skip gate and is acked-and-discarded with no `stripe_webhook_events` row. So the future handler (a) must resolve the invoice via the charge's `payment_intent` field → invoice-by-PI lookup, NOT via charge metadata, and (b) needs a branch in the ingress `velox_tenant_id` gate for charge-typed events. Until then there is no captured audit trail of a dashboard refund either — which is why the "refund via Velox" guidance above is the actual safety net.
- **Method-specific expected-settle windows** (cards: hours; ACH: ~5–8 business days). Trigger: first async method. Until then a single short card-appropriate window is correct.
- **A standalone daily List-Events drift cron.** Velox stores the PI id per invoice, so a targeted `GetPaymentIntent` reconcile already covers the payment path; a full event-stream scan only matters once a second webhook-driven side-effect has no per-row reconcile.
- **On-demand "Check provider" action** (operator-triggered single-invoice reconcile endpoint + button). Trigger: real stuck-PI pressure the auto-resolution doesn't clear (see Phase 4 deviation).
- **Dedicated `payment_processing_since` column.** The age banner runs off `updated_at` today; add the column only when an async method or a mid-`processing` mutation path makes `updated_at` provably wrong (same trigger as method-specific windows).

These are documented decisions, not silent gaps; the code would be dead until the trigger fires.

## Consequences

- One settlement implementation to test and reason about; the dropped-webhook backstop produces the same outcome as the webhook by construction.
- Fixes a live silent under-collection bug (Phase 2): a dropped `payment_intent.payment_failed` recovered by the reconciler will dun + notify + emit the event, instead of quietly marking `failed`.
- Reduces webhook dependence for the common synchronous-card path (Phase 3) and makes test-clock simulations resolve deterministically inside the Advance.
- The primitive is the single place future settlement behavior (disputes, async, partial payments) attaches to — no re-scattering.
