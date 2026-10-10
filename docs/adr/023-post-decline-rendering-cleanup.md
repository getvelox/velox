# ADR-023: Post-decline rendering — one banner, distinct emails, interactive suppression

**Status:** Accepted
**Date:** 2026-05-05

## Context

Operator-reported audit of an invoice that had hit a card-decline
surfaced four overlapping rendering issues, all caused by the
post-decline lifecycle moment being modeled as identical to the
finalize-no-PM moment:

1. **Two "Payment failed" banners on the invoice detail page.** The
   ADR-009 unified attention banner emitted `payment.declined` with
   only `[retry_payment]` action; the SPA had a hardcoded `<Card>`
   block at `InvoiceDetail.tsx:893-937` (pre-ADR-009 leftover) showing
   the same headline plus an Update Payment Method button. Two
   banners, same state, overlapping CTAs.

2. **Two "Customer notified — payment method required" rows in the
   activity timeline.** Both came from `email_type='payment_update_request'`,
   but were triggered by two different events:
   - `noPaymentMethodNotifierAdapter` at finalize (no PM on file).
   - `Stripe.handlePaymentFailed` after a charge declined.
   Same email type → same timeline label → operator can't tell them apart.

3. **Customer email after interactive Pay decline is noise.** Customer
   clicks Pay on the hosted invoice page, card declines, customer sees
   "Your card was declined" inline on the page. Velox then sends them
   an email saying the same thing. The customer was watching the
   transaction; the email tells them what they just saw.

4. **Same email body for two different lifecycle moments.** The
   `SendPaymentUpdateRequest` template was used both for "set up your
   first payment method" (finalize-no-PM) and "your card was declined"
   (post-decline). One copy, two semantically different moments.

The root cause is one design choice: the codebase had a single
`payment_update_request` email type and a single sender method
serving two different lifecycle moments. That collapses to identical
rendering everywhere downstream (timeline label, email subject/body,
attention-banner action set).

## Decision

Distinct primitives per lifecycle moment:

| Moment | Email type | Trigger | Timeline label | Attention banner CTA |
|---|---|---|---|---|
| Invoice finalized, **no PM on file** | `payment_setup_request` (renamed from `payment_update_request`) | `noPaymentMethodNotifierAdapter` | "Customer notified — set up payment method" | (already correct) Resend payment link / Open customer page |
| Invoice **charge attempted, declined** (auto-charge OR interactive Pay\*) | `payment_failed` (existing) | `Stripe.SettleFailed`, from whichever reports the decline first: the charge response, the webhook or the reconciler (ADR-049 amendment 2026-10-11) | "Payment-failed email sent to customer" | Update payment method / Retry payment |
| Dunning retries | `payment_failed` / `dunning_warning` / `dunning_escalation` | dunning notifier | (existing) | (existing) |

\* With one suppression: when the PI's `velox_purpose` metadata is
`hosted_invoice_pay` (interactive customer-on-page flow), no email is
sent. The customer is staring at the same failure inline.

### Implementation

**Email layer** (`internal/email/`):
- `TypePaymentUpdateRequest` constant renamed to `TypePaymentSetupRequest`
  ("payment_setup_request"). Kept stable as a tag once renamed.
- `Sender.SendPaymentUpdateRequest` → `SendPaymentSetupRequest`.
- `OutboxSender.SendPaymentUpdateRequest` → `SendPaymentSetupRequest`.
- Dispatcher's `EmailDeliverer` interface renamed accordingly + the
  switch case.
- The `SendPaymentFailed` method already existed (used by dunning) — no
  new template needed for the post-decline path; we just point the new
  consumer at the existing one.
- `outbox.go` SQL filter expanded to include `payment_setup_request`
  (replacing the old name).

**Payment layer** (`internal/payment/`):
- `EmailPaymentUpdate` interface deleted; new `EmailPaymentFailed`
  interface (`SendPaymentFailed(ctx, tenantID, to, customerName, invoiceNumber, reason, publicToken)` —
  same shape as `dunning.EmailNotifier`, so `OutboxSender` already
  satisfies it).
- `Stripe.SetEmailPaymentUpdate` → `SetEmailPaymentFailed` (no longer
  takes `paymentUpdateURL` — post-decline email points at the long-
  lived hosted invoice URL via `inv.PublicToken`, same as dunning).
- `Stripe.SetTokenService` removed: only the no-PM-at-finalize email
  needed single-use tokens; the post-decline email goes to the
  hosted invoice page.
- `handlePaymentFailed`:
  - Reads `data.object.metadata.velox_purpose` from the webhook payload
    via new `piPurposeFromPayload` helper.
  - If `velox_purpose == "hosted_invoice_pay"`: skip email, log INFO.
  - Otherwise: dispatch `SendPaymentFailed` with `inv.PublicToken`.
  - Still fires `event.payment.failed` on the dispatcher and starts
    dunning regardless of suppression — only the customer email is
    suppressed (suppression is about avoiding duplicate notification,
    not about disabling the dunning state machine).
  - *Amended 2026-10-11 (ADR-049 amendment):* a hosted-page decline no
    longer fires `payment.failed`, marks the invoice failed or starts
    dunning. Velox never waits on a hosted attempt, so its failure is
    recorded on its own attempt row only. The suppression above now
    applies to dunning-retry attempts.

**API wiring layer** (`internal/api/`):
- `noPaymentMethodNotifierAdapter` switched to a narrow consumer-side
  interface `paymentSetupEmailSender` (calls `SendPaymentSetupRequest`).
- Router wires `paymentSetupEmail` and `paymentFailedEmail` separately,
  both pointing at the same outbox/SMTP sender.

**Attention banner** (`internal/domain/invoice_attention.go`):
- New `AttentionActionUpdatePaymentMethod` constant.
- `classifyPaymentFailure` emits actions
  `[update_payment_method, retry_payment]` (primary first).

**SPA**:
- `web-v2/src/lib/api.ts`: `AttentionAction` union gains
  `update_payment_method`.
- `web-v2/src/components/InvoiceAttention.tsx`: new
  `UpdatePaymentMethodButton` calls `api.updatePaymentMethod(customer_id, return_url)`
  with `window.location.origin + window.location.pathname` as
  `return_url` (per ADR-022). `retry_payment` action wired to the
  existing `onChargeNow` callback (same Collect endpoint).
- `web-v2/src/pages/InvoiceDetail.tsx`: hardcoded "Payment failed"
  `<Card>` block at lines 893-937 deleted. The unified attention
  banner is now the single surface.
- `internal/invoice/handler.go`: timeline-label switch
  `case "payment_setup_request"` returns "Customer notified — set up
  payment method"; the old `payment_update_request` case removed.

## Consequences

**Operator's invoice detail page on a declined invoice**:
- One banner, not two. Critical-severity attention card with
  `[Update payment method, Retry payment]` actions.
- Activity timeline rows are distinct and meaningful: "Customer
  notified — set up payment method" at finalize (if applicable) →
  "Payment-failed email sent to customer" after the decline (if
  auto-charge — suppressed if interactive).

**Customer's email inbox**:
- Set-up-PM email: "Set up your payment method for invoice X" → links
  to the tokenized `/update-payment` SPA route (single-use token).
- Decline email: "Your payment for invoice X was declined" → links to
  the long-lived hosted invoice URL where they can update PM and
  retry. Same template dunning uses for retry warnings.

**Interactive Pay flow**:
- No email after decline. Customer sees the failure inline; we don't
  pile on a duplicate notification.

**Pre-launch with no production data**: no migration needed. The
`payment_update_request` constant is removed entirely (rename, not
add+delete) — there are no email_outbox rows to migrate. Same for
`EmailPaymentUpdate` Go interface (no external API surface).

## Alternatives considered

- **Keep one email type, vary subject/body via payload field.** Adds a
  conditional inside the template instead of two templates. Saves one
  outbox-type tag at the cost of muddying the lifecycle distinction
  in row data — operators investigating delivery issues would have to
  inspect the payload to know which moment the email was for. Two
  tags are clearer for ops without costing anything in code.
- **Add a "send despite interactive" flag for operators.** Skipped —
  no observed scenario where an operator wants to email a customer
  about a decline they just saw inline. If this becomes a real need,
  it's a tenant_settings flag, not a code change.
- **Surface "skipped (interactive)" as its own timeline row.** Decided
  against: the hosted invoice page already shows the decline state
  inline; a "skipped" row is internal plumbing, not operator-relevant
  signal. The slog INFO line is the right surface for that.
