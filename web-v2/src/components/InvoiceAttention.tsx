import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Link } from 'react-router-dom'
import { AlertTriangle, AlertCircle, Info, ExternalLink, Calendar } from 'lucide-react'
import { api, formatCents, type Invoice } from '@/lib/api'
import { SendSetupLinkDialog } from '@/components/SendSetupLinkDialog'
import type {
  AttentionAction,
  AttentionSeverity,
} from '@/lib/api'
import { formatDate, formatDateTime } from '@/lib/api'
import { timeAgo, isOlderThan, type EffectiveNow } from '@/lib/effectiveNow'
import { Card, CardContent } from '@/components/ui/card'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { cn } from '@/lib/utils'

// InvoiceAttention is the unified "this invoice needs operator
// attention" banner. Reads server-derived invoice.attention and
// renders nothing when absent (healthy or terminal-state). Mirrors
// Stripe's invoice banner pattern — severity-tinted card, reason
// badge, headline message, prescribed actions, optional doc-link and
// raw-detail disclosure.
//
// Stays purely presentational: the typed reason / action / severity
// codes come from the server via ADR-009. Adding a new code on the
// server doesn't require touching this component (the default action
// renderer covers any AttentionAction).
export function InvoiceAttention({
  invoice,
  onRetryTax,
  onChargeNow,
  onSendReminder,
  onVoidInvoice,
  retrying,
  charging,
  sending,
  now,
}: {
  invoice: Invoice
  onRetryTax?: () => void
  onChargeNow?: () => void
  onSendReminder?: () => void
  // Opens the invoice's existing void confirmation. Surfaced only on
  // prepaid-commit exposure, where voiding is the sole way to stop credit
  // going out against an invoice nobody paid (ADR-078).
  onVoidInvoice?: () => void
  retrying?: boolean
  charging?: boolean
  sending?: boolean
  // ADR-030 / simulated-time discipline: when the owning subscription
  // is pinned to a test clock, relative-time renders ("since X ago")
  // must be computed against the clock's frozen_time, NOT wall-clock.
  // Wall-clock would surface absurd deltas like "833d ago" on a
  // newly-finalized invoice whose sub is frozen at 2024-02-01 while
  // wall-clock sits in 2026. Build the anchor with effectiveNow(...) —
  // the type makes passing it non-optional so this can't regress.
  now: EffectiveNow
}) {
  const att = invoice.attention
  if (!att) return null

  const styles = severityStyles(att.severity)
  const Icon = severityIcons[att.severity] ?? Info

  return (
    <Card className={cn('mb-6', styles.card)}>
      <CardContent className="p-5 space-y-4">
        <div className="flex items-start gap-3">
          <Icon size={18} className={cn('mt-0.5 shrink-0', styles.icon)} />
          <div className="flex-1 space-y-1.5 min-w-0">
            <div className="flex items-center gap-2 flex-wrap">
              <Badge variant="outline" className={cn('text-xs', styles.badge)}>
                {humanReason(att.reason, att.code)}
              </Badge>
              {/* `since` reads as duration but is sourced from
                  inv.UpdatedAt — for fresh invoices that's the
                  finalize moment. Hide for the first 24h to avoid the
                  misleading "since 5m ago" copy on a just-finalized
                  invoice; surface only when the state has actually
                  persisted long enough to matter. */}
              {att.since && isOlderThan(att.since, 24 * 60 * 60 * 1000, now) && (
                <span className="text-[10px] text-muted-foreground">
                  · since {timeAgo(att.since, now)}
                </span>
              )}
            </div>
            {/* Tech code — useful for support tickets but visually
                noisy at the headline. Demoted to a small disclosure
                under the actions row (line ~110) so the human-readable
                badge + message dominate the banner. */}
            <p className="text-sm text-foreground">{att.message}</p>
            {att.next_attempt_at && (
              <p className="text-xs text-muted-foreground flex items-center gap-1.5">
                <Calendar size={11} className="shrink-0" />
                Engine will retry: <span className="text-foreground">{formatDateTime(att.next_attempt_at)}</span>
              </p>
            )}
            {att.due_by && (
              // Date-only on purpose. due_at is computed as
              // finalize_at + net_payment_term_days; that arithmetic
              // carries finalize-time hour precision (e.g. "8:16 PM
              // GMT+5:30") which is meaningless to operators or
              // customers — the deadline is "this calendar date".
              // Stripe's banner uses date-only too.
              <p className="text-xs text-muted-foreground flex items-center gap-1.5">
                <Calendar size={11} className="shrink-0" />
                Due by: <span className="text-foreground">{formatDate(att.due_by)}</span>
              </p>
            )}
          </div>
        </div>

        {(att.actions?.length ?? 0) > 0 && (
          <div className="flex items-center gap-2 flex-wrap pl-7">
            {att.actions!.map((action, idx) => (
              <ActionButton
                key={action.code}
                action={action.code}
                label={action.label}
                primary={idx === 0}
                invoice={invoice}
                onRetryTax={onRetryTax}
                onChargeNow={onChargeNow}
                onSendReminder={onSendReminder}
                onVoidInvoice={onVoidInvoice}
                retrying={retrying}
                charging={charging}
                sending={sending}
              />
            ))}
            {att.doc_url && (
              <Button render={<a href={att.doc_url} target="_blank" rel="noopener noreferrer" />} variant="ghost" size="sm" className="text-xs">
                Learn more
                <ExternalLink size={12} className="ml-1" />
              </Button>
            )}
          </div>
        )}

        {/* The auto-collect ("attaching a card auto-charges…") reassurance
            is SERVER-OWNED — it lives inside att.message, where it is
            clock-aware (wall tick vs next test-clock advance). A second
            hardcoded copy here drifted from that truth and repeated the
            promise twice in one banner (2026-07-22 audit, P1-1/P2-1). */}

        {/* Tech code — useful for support tickets, demoted from the
            headline. Shown inline-mono next to the doc anchor so a
            screenshot sent to support carries the routing hint. */}
        {att.code && (
          <p className="text-[10px] text-muted-foreground/60 font-mono pl-7">
            {att.code}
          </p>
        )}

        {/* Two distinct disclosures (ADR-025):
            - "Detail" carries Velox's own framing (operator-safe).
            - "Provider response" carries the literal upstream payload
              from a third-party (Stripe etc.). Populated only when we
              actually called the provider — pre-flight classification
              errors render no disclosure at all. Honest labeling so an
              operator never wonders "is this Velox talking or Stripe?" */}
        {att.detail && (
          <details className="pl-7 text-xs text-muted-foreground">
            <summary className="cursor-pointer hover:text-foreground">
              Detail
            </summary>
            <pre className="mt-2 p-2 bg-muted/50 rounded text-[10px] font-mono overflow-x-auto whitespace-pre-wrap break-all max-w-xl">
              {att.detail}
            </pre>
          </details>
        )}
        {att.provider_response && (
          <details className="pl-7 text-xs text-muted-foreground">
            <summary className="cursor-pointer hover:text-foreground">
              Provider response
            </summary>
            <pre className="mt-2 p-2 bg-muted/50 rounded text-[10px] font-mono overflow-x-auto whitespace-pre-wrap break-all max-w-xl">
              {att.provider_response}
            </pre>
          </details>
        )}
      </CardContent>
    </Card>
  )
}

// ActionButton dispatches typed action codes to the right UI element.
// edit_billing_profile / review_registration / wait_provider /
// rotate_api_key are link-out actions; retry_tax is server-bound and
// uses the supplied callback. Unknown codes fall through to a labeled
// no-op button so a server-added action doesn't render as broken.
function ActionButton({
  action,
  label,
  primary,
  invoice,
  onRetryTax,
  onChargeNow,
  onSendReminder,
  onVoidInvoice,
  retrying,
  charging,
  sending,
}: {
  action: AttentionAction
  label?: string
  primary: boolean
  invoice: Invoice
  onRetryTax?: () => void
  onChargeNow?: () => void
  onSendReminder?: () => void
  onVoidInvoice?: () => void
  retrying?: boolean
  charging?: boolean
  sending?: boolean
}) {
  const variant = primary ? 'default' : 'outline'
  const display = label ?? defaultLabel(action)

  switch (action) {
    case 'void_invoice':
      // Destructive styling regardless of `primary`: void annuls an invoice,
      // and it is never the primary CTA here (it is appended after whatever
      // the cause banner already recommended). Rendering it in the same
      // neutral outline as "Retry tax" would understate what it does.
      return (
        <Button
          variant="destructive"
          size="sm"
          onClick={onVoidInvoice}
          disabled={!onVoidInvoice}
        >
          {display}
        </Button>
      )
    case 'edit_billing_profile':
    case 'add_payment_method':
      return (
        <Button render={<Link to={`/customers/${invoice.customer_id}`} />} variant={variant} size="sm">
          {display}
        </Button>
      )
    case 'retry_tax':
      return (
        <Button
          variant={variant}
          size="sm"
          onClick={onRetryTax}
          disabled={!onRetryTax || retrying}
        >
          {retrying ? 'Retrying…' : display}
        </Button>
      )
    case 'wait_provider':
      return (
        <Button render={<a href="https://status.stripe.com/" target="_blank" rel="noopener noreferrer" />} variant={variant} size="sm">
          {display}
          <ExternalLink size={12} className="ml-1" />
        </Button>
      )
    case 'rotate_api_key':
      return (
        <Button render={<Link to="/settings" />} variant={variant} size="sm">
          {display}
        </Button>
      )
    case 'review_registration':
      return (
        <Button render={<Link to="/settings" />} variant={variant} size="sm">
          {display}
        </Button>
      )
    case 'connect_tax_provider':
      // Deep-link to Settings → Payments tab. The Payments tab is
      // mode-scoped (see the Settings audit) so landing the operator
      // there in the active mode lets them connect the right
      // credentials without a second mode toggle.
      return (
        <Button render={<Link to="/settings?tab=payments" />} variant={variant} size="sm">
          {display}
        </Button>
      )
    case 'charge_now':
    case 'retry_payment':
      // Same semantic — call the engine to attempt the charge now.
      // retry_payment falls in here because the lower-banner pre-cleanup
      // used the same Collect endpoint; both verbs map to the same
      // `collectMutation` callback the parent owns.
      return (
        <Button
          variant={variant}
          size="sm"
          onClick={onChargeNow}
          disabled={!onChargeNow || charging}
        >
          {charging ? 'Charging…' : display}
        </Button>
      )
    case 'send_reminder':
      return (
        <Button
          variant={variant}
          size="sm"
          onClick={onSendReminder}
          disabled={!onSendReminder || sending}
        >
          {sending ? 'Sending…' : display}
        </Button>
      )
    case 'update_payment_method':
      return (
        <UpdatePaymentMethodButton
          variant={variant}
          display={display}
          invoice={invoice}
        />
      )
    case 'reconcile_payment':
    default:
      return (
        <Button variant={variant} size="sm" disabled>
          {display}
        </Button>
      )
  }
}

// UpdatePaymentMethodButton opens the shared SendSetupLinkDialog
// pre-loaded with invoice context — the customer hits an invoice
// they couldn't pay, operator nudges them with an email that
// references the specific invoice + amount due. Migrated from the
// legacy "open Stripe URL in operator's tab" flow (Path B in the
// old triple-flow audit) to the unified Path A backend so all PM
// link sends go through one service + one template + one audit row
// shape.
function UpdatePaymentMethodButton({ variant, display, invoice }: { variant: 'default' | 'outline'; display: string; invoice: Invoice }) {
  const [open, setOpen] = useState(false)
  // Lazy customer fetch: only triggers when the dialog opens, so we
  // don't pay the round-trip for every InvoiceAttention render.
  const { data: customer } = useQuery({
    queryKey: ['customer-for-setup-link', invoice.customer_id],
    queryFn: () => api.getCustomer(invoice.customer_id),
    enabled: open,
  })
  const amountDueLabel = formatCents(invoice.amount_due_cents, invoice.currency)
  return (
    <>
      <Button variant={variant} size="sm" onClick={() => setOpen(true)}>
        {display}
      </Button>
      <SendSetupLinkDialog
        open={open}
        onOpenChange={setOpen}
        customerId={invoice.customer_id}
        customerEmail={customer?.email}
        invoiceContext={{ invoiceNumber: invoice.invoice_number, amountDueLabel }}
      />
    </>
  )
}

function severityStyles(s: AttentionSeverity) {
  switch (s) {
    case 'critical':
      return {
        card: 'border-destructive/40 bg-destructive/5',
        icon: 'text-destructive',
        badge: 'border-destructive/40 text-destructive',
      }
    case 'warning':
      return {
        card: 'border-amber-300/60 bg-amber-50/40 dark:bg-amber-950/20',
        icon: 'text-amber-600 dark:text-amber-400',
        badge: 'border-amber-300 text-amber-700 dark:text-amber-400',
      }
    case 'info':
    default:
      return {
        card: 'border-border bg-muted/30',
        icon: 'text-muted-foreground',
        badge: 'border-border text-muted-foreground',
      }
  }
}

// Module-scope lookup (not a function called during render): these are
// references to existing icon components, and a plain index makes that
// visible to both readers and the react-hooks/static-components rule —
// a render-time call assigned to a Capitalized JSX variable reads as
// creating a new component type per render.
const severityIcons: Record<string, typeof Info> = {
  critical: AlertCircle,
  warning: AlertTriangle,
  info: Info,
}

// humanReason maps a typed reason code to dashboard-display copy.
// Server sends the typed code; the UI owns its own label so wording
// changes don't require a server roll.
function humanReason(reason: string, code?: string): string {
  // A PARKED invoice shares the payment_unconfirmed REASON with a charge that is
  // genuinely still being confirmed, but they are opposite situations — one
  // resolves itself, the other never will. The server distinguishes them by
  // CODE, so the chip does too: labelling a permanently stuck invoice
  // "Payment unconfirmed" reads as "hang on a moment" (found walking the page
  // as an operator, 2026-07-31).
  if (code === 'payment.unidentifiable') return 'Payment unidentifiable'
  const map: Record<string, string> = {
    tax_calculation_failed: 'Tax calculation failed',
    tax_location_required: 'Customer address required',
    payment_failed: 'Payment failed',
    payment_unconfirmed: 'Payment unconfirmed',
    payment_processing: 'Payment processing',
    payment_scheduled: 'Auto-charge scheduled',
    awaiting_payment: 'Awaiting first charge',
    no_payment_method: 'No payment method',
    dunning_exhausted: 'Payment recovery ended',
    collection_paused: 'Collection paused',
    // Operator language, not ledger language. "Commit exposure" is the term the
    // ADR and the schema use; a finance operator reading a chip needs the plain
    // fact — an invoice went unpaid and the credit it bought is already live.
    commit_exposure: 'Unpaid prepaid credit',
  }
  return map[reason] ?? reason
}

function defaultLabel(action: AttentionAction): string {
  const map: Record<AttentionAction, string> = {
    edit_billing_profile: 'Edit billing profile',
    retry_tax: 'Retry tax',
    retry_payment: 'Retry payment',
    wait_provider: 'Check provider status',
    rotate_api_key: 'Rotate API key',
    reconcile_payment: 'Reconcile',
    review_registration: 'Review tax registration',
    charge_now: 'Charge now',
    // send_reminder = server-side email to the customer with a
    // payment link. Verb is "Email" not "Share" — operators
    // mistakenly expected a clipboard action with the older "Share
    // invoice link" copy. The per-reason routing is CLIENT-side:
    // InvoiceDetail's onSendReminder branches on attention.reason
    // (no_payment_method → resend-setup-link endpoint, else → the
    // invoice email dialog); each backend endpoint sends one fixed
    // template.
    send_reminder: 'Email payment link',
    add_payment_method: 'Add payment method',
    update_payment_method: 'Update payment method',
    connect_tax_provider: 'Connect Stripe',
    void_invoice: 'Void invoice',
  }
  return map[action] ?? action
}

