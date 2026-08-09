import { useMemo, useCallback } from 'react'
import { useQuery } from '@tanstack/react-query'
import { api } from '@/lib/api'
import { useAuth } from '@/contexts/AuthContext'
import { effectiveNow, type EffectiveNow } from '@/lib/effectiveNow'

// useClockFrozenMap returns a `test_clock_id -> frozen_time` lookup so any
// relative-time / cycle-progress surface can compute its "now" baseline from
// the SIMULATED time of a clock-pinned entity instead of wall-clock Date.now().
//
// Test clocks let operators advance simulated time independently of the wall
// clock; an entity's domain timestamps (billing-period bounds, finalized_at,
// due/expiry dates) are then stamped in simulation time and can sit far in the
// browser's future/past. Any UI that renders a value RELATIVE to "now" — "X
// ago", "in N days", cycle progress %, "Day N of M" — has to read that "now"
// from the clock, or it lies. See ExpiryBadge / DueBadge / InvoiceAttention /
// pages/Dunning for the established effective-now pattern.
//
// Previously this map was hand-rolled inline (Subscriptions.tsx); the omission
// of that copy in CostDashboard is exactly what left its cycle progress reading
// wall-clock time on clock-pinned subs. Centralizing it here removes the
// per-call-site drift that caused the miss.
// useTestClocks is THE fetch of the test-clock list — every consumer
// (frozen-time maps here, the clock-name lookup in Subscriptions, the
// has-clocks check in Pricing's rule dialog) derives from this one query.
// Test clocks are a test-mode-only resource: the server 403s the list in
// live mode, and a live entity can never carry a test_clock_id, so the
// data could never be consulted there anyway. Don't ask — gating lived
// per call site before this hook, and three inline copies each earned a
// 403 console error in live mode (the exact per-call-site drift the file
// header describes). All consumers are operator-authed surfaces; the
// public cost dashboard is a separate component with its own payload.
export function useTestClocks() {
  const { user } = useAuth()
  return useQuery({
    queryKey: ['test-clocks'],
    queryFn: () => api.listTestClocks(),
    enabled: !(user?.livemode ?? false),
  })
}

export function useClockFrozenMap(): Record<string, string> {
  const { data } = useTestClocks()
  return useMemo(() => {
    const m: Record<string, string> = {}
    ;(data?.data ?? []).forEach(c => {
      m[c.id] = c.frozen_time
    })
    return m
  }, [data])
}

// clockNow resolves the effective "now" ISO for an entity: the owning test
// clock's frozen_time when the entity is pinned, else undefined so the caller
// falls back to real Date.now(). Pass the entity's own test_clock_id (a
// subscription's, a customer's) — NOT a clock id you already know is set.
export function clockNow(
  map: Record<string, string>,
  testClockId?: string | null,
): string | undefined {
  return testClockId ? map[testClockId] : undefined
}

// useEffectiveNow is the single-entity path: one clock pin, one stable anchor.
// The returned EffectiveNow is memoized on the frozen-time map + the pin, so it
// is safe to feed a useMemo dependency array or a React-Query queryKey without
// churning every render (a wall-clock entity's Date.now() is captured once at
// mount, exactly as the prior in-memo fallback did). Use in detail pages and
// per-customer cards; use useEffectiveNowResolver for lists of mixed pins.
export function useEffectiveNow(testClockId?: string | null): EffectiveNow {
  const map = useClockFrozenMap()
  // Memoize on the RESOLVED frozen_time string, not [map, testClockId]: an
  // unpinned entity resolves to undefined and keeps one wall-clock anchor for
  // the mount (so a background test-clocks refetch can't churn a queryKey built
  // from it), while a pinned entity recomputes only when its clock advances.
  const frozen = clockNow(map, testClockId)
  return useMemo(() => effectiveNow(frozen), [frozen])
}

// useEffectiveNowResolver is the ergonomic path for list/feed components that
// render many entities of differing clock pins. It bundles the frozen-time map
// fetch and returns a resolver: pass an entity's own test_clock_id and get its
// EffectiveNow anchor (frozen_time when pinned, wall time when not). Feed the
// result straight into the required-anchor helpers in lib/effectiveNow.
//
//   const resolveNow = useEffectiveNowResolver()
//   ...timeAgo(inv.created_at, resolveNow(customer.test_clock_id))
export function useEffectiveNowResolver(): (testClockId?: string | null) => EffectiveNow {
  const map = useClockFrozenMap()
  return useCallback(
    (testClockId?: string | null) => effectiveNow(clockNow(map, testClockId)),
    [map],
  )
}
