# ADR-063: Refund status is reconciled from Stripe webhooks (async truth)

**Date:** 2026-06-28
**Status:** Accepted

## Context

Stripe refunds are **asynchronous**. `Refund.create` is idempotent and often
returns `status=pending`; the final outcome (`succeeded` / **`failed`** /
`canceled`) lands later via a webhook. Even `succeeded` means "submitted to the
card network", **not** "on the cardholder statement" (5–10 business days, no
confirming event). A `pending` refund can flip to **`failed`** (bank reject /
insufficient platform balance → money returns to the *platform* balance, the
customer gets **nothing**).

Velox recorded refund status **optimistically from the create-call**:
`StripeRefunder.CreateRefund` dropped `ref.Status` and `creditnote.Issue()` /
`RetryRefund` hard-coded `refund_status = succeeded` on any non-error create.
The webhook switch handled **no** refund events. So a refund recorded
`succeeded` that Stripe later **failed** was a silent, money-wrong state — the
customer is owed money, and nothing surfaced it (it wasn't even `failed`).

#319 had just shipped a "refunds need attention" dashboard alert + Credit Notes
filter keyed on `refund_status IN ('failed','pending')` — written when `pending`
was *rare* (only "no refunder/no PI at issue").

## Decision

1. **Record the create-time status faithfully.** `CreateRefund` returns Stripe's
   actual status (mapped to a Velox `refund_status`); `Issue()` and `RetryRefund`
   record `pending` when Stripe says pending, not a blanket `succeeded`. (The
   common healthy card refund still returns `succeeded` synchronously.)

2. **The webhook is the source of truth for the async outcome.** Handle
   `charge.refund.updated`, `refund.updated`, and `refund.failed` (all carry a
   Refund object; one status-driven handler) — admitted via
   `endpointAuthoritativeEvent` (refunds carry no `velox_*` metadata; the
   per-tenant endpoint + HMAC is authoritative). Match webhook→credit-note by
   `stripe_refund_id`; apply **monotonically** (terminal wins; a stale
   out-of-order `pending` never clobbers a terminal). Reuse the existing
   event-id dedup. A refund with no matching credit note (Stripe-dashboard /
   direct-API refund) is **ack'd permanently, never auto-creates a credit note**;
   a *very recent* unmatched refund gets a bounded (15-min) redelivery to cover
   the rare create→webhook race.

3. **Status mapping (lossy, no migration).** Stripe's 5 states collapse into
   Velox's 4-value `refund_status` CHECK: `succeeded→succeeded`,
   `failed→failed`, **`canceled→failed`** (money returned to platform — the
   operator-actionable bucket), `pending→pending`, `requires_action→pending`.
   The `canceled→failed` collapse is the re-litigable call here.

4. **Co-refine the #319 alert** (same change) so faithful `pending` doesn't flood
   it: "needs attention" = `failed` **OR** (`pending` older than **72h** ≈ 3
   business days). Fresh `pending` is normal async settlement and is shown as a
   neutral per-CN badge, not an alert. The aged-pending arm is also the cheap
   backstop for a **never-delivered** terminal webhook (there is no refund poll).

## Consequences

- A refund that fails asynchronously now becomes `failed` and surfaces; the
  false-success state is closed.
- The refund leg stays **operator-retried** (`RetryRefund`), not auto-swept —
  money-out is conservative; the webhook + alert make stuck refunds *visible*.
- Honest UI: `succeeded` should read as "refund issued / on its way", since even
  Stripe's `succeeded` is "submitted", not "on the statement".

## Deferred (named triggers)

- **Refund-poll / `GetRefund` reconciler** for never-delivered webhooks — the
  aged-pending alert is its stand-in. Trigger: an observed missed webhook.
- **Dedicated `RefundCanceled` enum + CHECK-widening migration (0124)** — trigger:
  a partner actually hits canceled refunds.
- **Ingesting external (dashboard) refunds** into the credit ledger — larger
  separate decision; trigger: a tenant relies on dashboard refunds.

## Related
- #319 (the refund "needs attention" alert this co-refines).
- ADR-061 (atomic `creditnote.Issue()`); ADR-040 (webhook outbox / dedup spine).

## Amendment (2026-08-04): retry convergence cannot rest on the idempotency key

An adversarial architecture review of the refund model found the original
retry contract — "same `velox_cn_<id>` key as Issue(), Stripe dedups, so a
retry converges on the original refund" — resting on a premise Stripe does
not provide: **v1 idempotency keys expire after ~24 hours**, while this ADR's
own needs-attention window tells the operator to act at **72 hours**. Two
shipped defects followed:

1. **Double-refund window.** Retrying a stuck *partial* `pending` refund past
   the key horizon minted a second live refund (Stripe's `amount_too_large`
   protects only full-amount refunds), and the persist's overwrite of
   `stripe_refund_id` orphaned the first refund's webhooks as "foreign".
2. **Truth regression.** Within the horizon, a key replay returns the SAVED
   first response — so a retry after a webhook-recorded terminal `failed`
   re-persisted the stale create-time `pending` through the operator writer,
   which (unlike the webhook writer) had no monotonic guard. Event-id dedup
   then swallowed any correcting redelivery forever.

**The amended contract — reconcile, adopt, only then create:**

- Every refund create stamps **metadata `velox_cn_id`**. Durable convergence
  is the metadata, not the key: a refund whose response was lost is always
  findable again.
- `RetryRefund` with a stored id calls **`GetRefund`** first — a read, which
  always returns current truth, never a replay. `succeeded` → recovered (this
  also collects the missed-webhook case, the stand-in this ADR deferred the
  poller for); `pending` → **refuse to mint a second refund** while one is in
  flight; `failed` → a new refund is legitimate.
- With no stored id, or past a provider-confirmed-dead one,
  **`FindRefundForCreditNote`** searches the PaymentIntent's refunds by
  metadata (excluding the dead id) and **adopts** a live match instead of
  creating — ADR-108's search-and-adopt, applied to money-out.
- The create key scopes to the CN's **state generation**
  (`velox_cn_<id>_r_<hash(refund_id|status|updated_at)>`): double-clicked
  retries share a snapshot and collapse into one refund; a retry after a
  persisted state change gets a fresh key instead of replaying a dead
  attempt's saved response.
- The operator writer now refuses **same-identity regressions** (failed is
  absorbing; succeeded yields only to failed) while still emitting the
  audit row — the action is the fact — and a NEW identity may write any
  status, because a fresh attempt legitimately restarts the lifecycle.
  Same-value persists still touch `updated_at` (the 72h window resets on a
  fresh provider confirmation).
- Stripe's **`failure_reason`** now rides every failed transition's audit
  metadata (webhook, reconcile, and create-error paths). Previously it lived
  only in process logs, so refund-failure forensics were unanswerable
  retroactively.

Also shipped with this amendment: `credit_notes` gained its first useful
lookup indexes (migration 0169 — `invoice_id`, partial `stripe_refund_id`);
the webhook match was previously a tenant-wide scan.

Unchanged: refunds stay **operator-retried**, never auto-swept; the refund
remains a leg of the credit note (the review confirmed the peer set —
Lago, Orb, Stripe Billing's own recommendation — models it the same way, and
rejected a freestanding refunds table); the deferred attempt-history table
(`invoice_refund_attempts`, mirroring ADR-102's charge attempts) keeps its
trigger: a DP asking for refund observability, a second PSP, or dispute
ingestion.
