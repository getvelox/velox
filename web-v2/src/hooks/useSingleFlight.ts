import { useCallback, useRef } from 'react'

// singleFlight wraps an async function so a second call cannot start while the
// first is still in flight — the fix for the stale-closure double-fire class.
// A `disabled` attribute or a `useState` guard only takes effect on the NEXT
// render, so two calls dispatched in one event-loop tick both read the
// pre-click state and both fire. This closure flag is written synchronously and
// is visible to the second call in the same tick. While a call is active the
// wrapper is a no-op that resolves undefined, so callers need no own guard.
//
// Extracted from the hook so it can be unit-tested without a React renderer
// (see tests/singleFlight.test.ts).
export function singleFlight<A extends unknown[]>(
  fn: (...args: A) => Promise<void>,
): (...args: A) => Promise<void> {
  let inFlight = false
  return async (...args: A) => {
    if (inFlight) return
    inFlight = true
    try {
      await fn(...args)
    } finally {
      inFlight = false
    }
  }
}

// useSingleFlight is the React binding. The in-flight flag lives in a ref that
// is touched only at call time (never during render), so two clicks in one
// tick share the same flag and the second is dropped. Use it wherever a
// double-fire produces a REAL duplicate rather than a server-rejected no-op:
// replays, credit-note issue, one-off invoice create, key/secret rotation,
// resend-email. State-machine transitions that 409 on repeat (finalize, void,
// revoke) and idempotent upserts don't need it — the server is already the
// guard there. Keep any existing `useState`/`disabled` loading UI; that's for
// the spinner and the disabled look, this is for correctness.
export function useSingleFlight<A extends unknown[]>(
  fn: (...args: A) => Promise<void>,
): (...args: A) => Promise<void> {
  const inFlight = useRef(false)
  return useCallback(
    async (...args: A) => {
      if (inFlight.current) return
      inFlight.current = true
      try {
        await fn(...args)
      } finally {
        inFlight.current = false
      }
    },
    [fn],
  )
}
