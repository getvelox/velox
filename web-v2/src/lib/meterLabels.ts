// Operator-facing labels for meter aggregations — raw enum slugs never
// reach the UI ('last' rendered literally on three surfaces before this).
// A meter has one aggregation, and every price of the meter bills under it
// (ADR-130 §2). This is the one list of them.
export const METER_AGGREGATIONS: { value: string; label: string; help: string }[] = [
  { value: 'sum', label: 'Sum', help: 'Adds up the event values in the billing period.' },
  { value: 'count', label: 'Count', help: 'Counts the events; their values are ignored.' },
  { value: 'max', label: 'Maximum', help: 'Takes the largest event value in the billing period.' },
  { value: 'last', label: 'Latest value', help: 'Takes the latest event value in the billing period.' },
]

export const METER_AGGREGATION_LABELS: Record<string, string> = Object.fromEntries(
  METER_AGGREGATIONS.map(a => [a.value, a.label]),
)

export function meterAggregationLabel(agg: string): string {
  return METER_AGGREGATION_LABELS[agg] ?? agg
}

export function meterAggregationHelp(agg: string): string {
  return METER_AGGREGATIONS.find(a => a.value === agg)?.help ?? ''
}

// The aggregation lock (ADR-130 §2), said on the page before any edit. Only
// usage events fix the aggregation; held usage does not.
export const METER_HAS_USAGE_SENTENCE = 'This meter has usage events, so its aggregation is fixed. Make a new meter to count differently.'
export const METER_NO_USAGE_SENTENCE = 'The aggregation can change until this meter has a usage event. Held usage does not fix it.'
export const METER_NAME_UNIT_SENTENCE = 'A new name or unit applies at once to new invoice lines.'

// The server's refusal of an aggregation change on a meter with usage events
// (409 meter_has_usage). null for any other error.
export function meterHasUsageMessage(err: unknown): string | null {
  if (typeof err !== 'object' || err === null) return null
  const { code, message } = err as { code?: unknown; message?: unknown }
  if (code !== 'meter_has_usage' || typeof message !== 'string' || !message.trim()) return null
  return message.trim()
}
