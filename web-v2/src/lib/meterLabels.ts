// Operator-facing labels for meter aggregations — raw enum slugs never
// reach the UI ('last' rendered literally on three surfaces before this).
export const METER_AGGREGATION_LABELS: Record<string, string> = {
  sum: 'Sum',
  count: 'Count',
  max: 'Maximum',
  last: 'Latest value',
}

export function meterAggregationLabel(agg: string): string {
  return METER_AGGREGATION_LABELS[agg] ?? agg
}
