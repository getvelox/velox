// One source for how a dunning policy's terminal outcome reads in the UI.
//
// ADR-112 split `final_action` into two independent columns — what happens
// to the subscription, and what happens to the unpaid invoice. Three
// surfaces render that pair (the policies list, the customer's effective-
// policy badge, and the policy-picker detail), and re-typing the mapping in
// each is how ADR-110's terminal-status rule got missed three separate
// times before it was centralised.

/** What exhaustion does to the subscription. */
export type DunningSubscriptionAction = 'none' | 'pause' | 'cancel'

/** What exhaustion does to the unpaid invoice. */
export type DunningInvoiceAction = 'none' | 'mark_uncollectible'

const SUB_LABEL: Record<string, string> = {
  pause: 'pause collection',
  cancel: 'cancel subscription',
  none: '',
}

const INV_LABEL: Record<string, string> = {
  mark_uncollectible: 'write off invoice',
  none: '',
}

/**
 * Compact summary for badges and list rows — e.g. "cancel subscription +
 * write off invoice".
 *
 * Returns "no action" when neither half acts, rather than an empty string:
 * a blank cell reads as missing data, and "the policy deliberately does
 * nothing and waits for you" is real, chosen configuration.
 *
 * Unknown values pass through verbatim instead of being dropped. A value
 * this build does not recognise is a deploy-skew signal the operator should
 * see, and silently rendering "no action" over it would claim the policy is
 * inert when it is not.
 */
export function finalActionSummary(policy: {
  final_subscription_action?: string
  final_invoice_action?: string
}): string {
  const parts: string[] = []
  const sub = policy.final_subscription_action ?? 'none'
  const inv = policy.final_invoice_action ?? 'none'
  const subLabel = sub in SUB_LABEL ? SUB_LABEL[sub] : sub
  const invLabel = inv in INV_LABEL ? INV_LABEL[inv] : inv
  if (subLabel) parts.push(subLabel)
  if (invLabel) parts.push(invLabel)
  return parts.length ? parts.join(' + ') : 'no action'
}
