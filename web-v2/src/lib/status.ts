export function statusBadgeVariant(status: string): 'default' | 'secondary' | 'destructive' | 'outline' | 'success' | 'info' | 'warning' | 'danger' {
  switch (status) {
    // Green
    case 'active': case 'paid': case 'succeeded': case 'resolved': return 'success'
    // Blue
    case 'finalized': case 'processing': case 'issued': case 'trialing': return 'info'
    // Red
    case 'voided': case 'canceled': case 'failed': case 'revoked': case 'uncollectible': case 'expired': return 'danger'
    // Amber
    case 'paused': case 'pending': case 'escalated': return 'warning'
    // Gray
    case 'draft': case 'archived': case 'ready': return 'secondary'
    default: return 'outline'
  }
}

export function statusBorderColor(status: string): string {
  switch (status) {
    case 'active': case 'paid': case 'succeeded': case 'resolved': return 'border-l-emerald-500'
    case 'finalized': case 'processing': case 'issued': case 'trialing': return 'border-l-blue-500'
    case 'voided': case 'canceled': case 'failed': case 'revoked': case 'uncollectible': return 'border-l-red-500'
    case 'paused': case 'pending': case 'escalated': return 'border-l-amber-500'
    case 'draft': case 'archived': return 'border-l-gray-300 dark:border-l-gray-600'
    default: return 'border-l-transparent'
  }
}

// creditNoteReasonLabel humanizes the credit-note `reason` for display.
// System-generated credit notes carry machine-categorical codes
// (subscription_downgrade, subscription_item_removed, …); operator-issued
// ones carry free text. Map the known codes to finance-readable phrases;
// pass anything else through unchanged (it's operator free text).
export function creditNoteReasonLabel(reason: string): string {
  switch (reason) {
    case 'subscription_downgrade': return 'Plan downgrade'
    case 'subscription_quantity_decrease': return 'Quantity decrease'
    case 'subscription_item_removed': return 'Item removed'
    case 'subscription_cancellation': return 'Cancellation'
    default: return reason
  }
}

// paymentIsUnresolved mirrors domain.InvoicePaymentStatus.IsInFlight() — the
// server's single source for whether a payment blocks operator actions
// (domain.PaymentBlocksAction). The dashboard used to hand-write
// `!== 'succeeded' && !== 'processing'` at four call sites, all of which omitted
// 'unknown' — so a parked invoice (ADR-107) rendered a green "Collect Payment"
// button directly beneath a Critical banner saying no further charge would be
// attempted.
//
// This is deliberately ONE copy rather than four, and the Go-side scanner now
// walks .ts/.tsx so a fifth cannot quietly appear. The durable version is the
// server returning per-action availability so the dashboard derives instead of
// mirroring; until then, this is the single place to change.
export function paymentIsUnresolved(paymentStatus: string): boolean {
  return paymentStatus === 'processing' || paymentStatus === 'unknown'
}
