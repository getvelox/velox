# ADR-049: A single payment-settlement primitive (discover-then-settle)

- Status: Accepted
- Date: 2026-06-07
- Relates: ADR-001 (PaymentIntent-only Stripe), ADR-036 (dunning campaigns), ADR-030 (clock-pinned sim-time on financial writes)

## Summary

Every terminal Stripe PaymentIntent outcome settles through one idempotent primitive: `SettleSucceeded` (mark paid, fire `payment.succeeded`, send a receipt) or `SettleFailed` (mark failed, fire `payment.failed`, start dunning, send a failure email unless the flow suppresses it). Before this, about ten code paths wrote these states with different side effects, so a failure found by the reconciler started no dunning and sent nothing. Three callers find the outcome and pass it in: the webhook, the synchronous charge response when its status is `succeeded`, and the reconciler for stale `unknown` or `processing` invoices. Invoices that settle without a charge ($0 or fully covered by credits) and out-of-band manual payments stay on their own paths and send no receipt. Amended 2026-07-31: if a lost response leaves no PaymentIntent id, the invoice is parked in `unknown` and no charge path retries it. ADR-107 and ADR-108 cover how it is resolved: a provider search first, and an operator write-off otherwise. Amended 2026-10-11: a declined charge response now reports through `SettleFailed` too, and a failure moves the invoice only when it is about the attempt the invoice is waiting on (see the amendment below).

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

## Amendment 2026-10-11: failures report like successes, and only for the current attempt

Two defects shared one function, `MarkPaymentFailedReportingTransition`, the store write behind `SettleFailed`.

**The charge response's decline notified nobody.** Phase 3 made a `succeeded` response settle through `SettleSucceeded`. A declined response only stamped `failed` and started dunning; `payment.failed` and the customer email fired from `SettleFailed`, which only the webhook and the reconciler called. With no webhook (a lost delivery, a test clock with no forwarder) the decline was never announced. The charge call now reports its decline through `SettleFailed` with source `charge_response`. Whichever of the three reporters arrives first records it and fires `payment.failed` and the email; the others find the PaymentIntent already recorded and skip them (`failure_notified_pi`). A failure with no PaymentIntent (the create itself failed) is still stamped by `StampChargeOutcome` and announced to nobody: there is nothing to match, and it is usually a merchant configuration error. `StampChargeOutcome` now refuses a failure that names a PaymentIntent, so the report path cannot be bypassed.

**A late decline for an older attempt overwrote the newer one.** The webhook resolves the invoice by PaymentIntent and falls back to the `velox_invoice_id` metadata, so a decline for any attempt reached the invoice, and the write relinked that PaymentIntent, set `failed` and bumped `charge_attempt_seq`. A stale delivery hid an in-flight attempt from the reconciler (which only watches `processing` and `unknown`), refused the outcome of a charge in flight (its seq CAS, P13) and sent a late "payment failed" email. The rule is now `domain.PaymentFailureReport.MovesInvoice`, applied under the row lock:

- a paid invoice: never;
- the invoice already holds this PaymentIntent: record it, whoever created it (a parked invoice can adopt a hosted attempt through the ADR-108 search);
- any other hosted Checkout attempt (`velox_purpose=hosted_invoice_pay`): never;
- the charge call reporting its own attempt: record it only if the seq is still the one it charged with;
- a webhook or reconciler report about a different PaymentIntent: never while the invoice waits on an in-flight attempt; otherwise the invoice's attempt history decides. If the PaymentIntent is Velox's newest attempt, record it (the charge call's own report was lost). If Velox made a newer attempt since, ignore it (a late decline for an older attempt). With no Velox attempt row to judge by, record it only if the invoice holds no PaymentIntent yet (the first attempt, or a parked attempt whose id Velox never learned).

Why this identifies the attempt the invoice waits on: every Velox charge starts only from `pending` or `failed` under the charge lease. It records its `invoice_charge_attempts` row when Stripe answers, and only then reports its outcome through the seq CAS. So Velox's attempt rows are in attempt order, and the newest is the one the invoice waits on. A hosted Checkout attempt runs outside that lease, at the same time as Velox's own charges; its row is `external` and never counts. The charge call's write claims a row that a faster webhook inserted as `external` first. Every report lands on its own attempt row, so a failure that does not move the invoice stays on the timeline. Kill Bill has the same shape: outcomes are stored per payment transaction, and the invoice derives from the successful ones, so a late event updates only its own attempt.

Consequences:

- A decline on the hosted payment page no longer marks the invoice failed or sends `payment.failed`. The customer saw the decline on the page and can retry there; the attempt shows on the timeline.
- Dunning start is re-driven by every report that records the failure, not only the first. The first reporter's start can fail after its stamp committed, and a test-clock invoice has no other re-drive: the dunning backfill skips simulated invoices.
- The decline email is skipped for an invoice that is no longer `finalized` when the failure is recorded (voided or written off during the charge).
- A parked invoice (`unknown`, no PaymentIntent) is released only by a decline the attempt history cannot place as older: the parked attempt's own decline, whose id Velox never learned. A late decline for an older attempt is superseded by the parked attempt's row. Residual, registered (velox-ops DP-readiness register): the attempt-row write is best-effort, so an older attempt whose row was lost still looks like the parked one.

## Consequences

- One settlement implementation to test and reason about; the dropped-webhook backstop produces the same outcome as the webhook by construction.
- Fixes a live silent under-collection bug (Phase 2): a dropped `payment_intent.payment_failed` recovered by the reconciler will dun + notify + emit the event, instead of quietly marking `failed`.
- Reduces webhook dependence for the common synchronous-card path (Phase 3) and makes test-clock simulations resolve deterministically inside the Advance.
- The primitive is the single place future settlement behavior (disputes, async, partial payments) attaches to — no re-scattering.
