// latestPerKey collapses a multi-version rating-rule list to one entry per
// rule_key — the latest ACTIVE version (what billing resolves; ADR-070), or
// the latest version outright when none is active (fully-archived key).
// Version pickers must offer keys, not versions: billing re-resolves the
// key per period, so "choosing v1" is a choice that does not exist.
export function latestPerKey<T extends { rule_key: string; version: number; lifecycle_state?: string }>(rules: T[]): T[] {
  const byKey = new Map<string, T>()
  for (const r of rules) {
    const cur = byKey.get(r.rule_key)
    const rActive = (r.lifecycle_state ?? 'active') === 'active'
    const curActive = cur ? (cur.lifecycle_state ?? 'active') === 'active' : false
    if (!cur || (rActive && !curActive) || (rActive === curActive && r.version > cur.version)) {
      byKey.set(r.rule_key, r)
    }
  }
  return [...byKey.values()]
}
