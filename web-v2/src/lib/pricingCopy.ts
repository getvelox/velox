// Words for the plan, meter and price pages that depend on state. Plain
// functions so the unit tests can check each version.

// "1 subscription uses" / "3 subscriptions use".
function subscriptionsUse(n: number): string {
  return n === 1 ? '1 subscription uses' : `${n.toLocaleString('en-US')} subscriptions use`
}

export const PLAN_LOCKED_TITLE = 'Plan in use'

// The plan lock (ADR-034): the server refuses a base price, billing timing
// or meter change while any subscription that is not canceled uses the
// plan (pricing.Service.UpdatePlan). `liveSubscriptions` is the
// server's own count of those (GET /v1/plans/{id} live_subscription_count),
// so the page and the refusal agree. Same sentence as the refusal.
export function planLockedSentence(liveSubscriptions: number): string {
  return `${subscriptionsUse(liveSubscriptions)} this plan, so its price, billing timing and meters are locked. Create a new plan with the new terms instead.`
}

// A plan's base price can be set to any amount above 0 but never back to 0:
// the server ignores 0 on update (UpdatePlan only applies a positive amount),
// so saving 0 would read "Plan updated" and change nothing.
export function basePriceZeroBlocked(input: string, currentCents: number): boolean {
  const n = parseFloat(input || '0')
  return currentCents > 0 && Number.isFinite(n) && Math.round(n * 100) === 0
}

export const BASE_PRICE_ZERO_HINT = "The base price can't go back to 0. Create a new plan without a base fee."

// The confirm for deleting a price for specific values on a meter. Events it
// matched go to another matching price, then to the meter's default price;
// with no default price and no other match they are not billed.
export function deleteMatchedPriceSentence(match: string, hasDefaultPrice: boolean): string {
  const what = match ? `Events it matched (${match})` : 'Events it matched'
  return hasDefaultPrice
    ? `${what} move to another matching price or the default price from the next invoice. Finalized invoices don't change.`
    : `${what} go unbilled from the next invoice unless another price below matches them. Finalized invoices don't change.`
}

export const METER_IN_USE_TITLE = 'Meter in use'

// The meter lock (D05): while subscriptions use a meter, the server refuses a
// change to its prices that would reprice the period already open, with code
// meter_in_use. (The aggregation has its own rule: fixed once the meter has
// usage events, meterLabels.ts.) Its message names the count and what to do
// instead, so the dialog shows it as it is. null for any other error.
export function meterInUseMessage(err: unknown): string | null {
  if (typeof err !== 'object' || err === null) return null
  const { code, message } = err as { code?: unknown; message?: unknown }
  if (code !== 'meter_in_use' || typeof message !== 'string' || !message.trim()) return null
  return message.trim()
}
