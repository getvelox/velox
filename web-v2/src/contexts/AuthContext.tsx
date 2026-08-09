import { createContext, useCallback, useContext, useEffect, useState, type ReactNode } from 'react'
import { authApi, type SessionInfo } from '@/lib/auth'
import { ApiError } from '@/lib/api'

// AuthContext exposes the resolved session context to the rest of the
// dashboard. Per ADR-011, the dashboard signs in via email + password;
// the resulting session is user-bound (UserContext.user_id is the
// canonical identifier). Email is rendered in the user dropdown.
//
// SDK callers using Authorization: Bearer never reach this context —
// they don't render React. The user_id field is therefore always
// present on a populated UserContext.
export interface UserContext {
  user_id: string
  tenant_id: string
  email: string
  livemode: boolean
}

interface AuthState {
  user: UserContext | null
  loading: boolean
  login: (email: string, password: string) => Promise<void>
  logout: () => Promise<void>
  refresh: () => Promise<void>
  setMode: (livemode: boolean) => Promise<void>
}

const AuthContext = createContext<AuthState | null>(null)

function toUserContext(s: SessionInfo): UserContext | null {
  // Cookie path returns user_id + email; Bearer path doesn't. The
  // dashboard only ever rides the cookie path, so a missing user_id
  // means whoami resolved to a Bearer key (e.g. someone hand-rolled
  // an Authorization header) — treat as not-signed-in.
  if (!s.user_id) return null
  return {
    user_id: s.user_id,
    tenant_id: s.tenant_id,
    email: s.email ?? '',
    livemode: s.livemode,
  }
}

// Customer-facing pages reached from emailed links — hosted invoice,
// payment update and its return page. Their visitors are end customers
// with no operator session, so the whoami bootstrap below is a
// guaranteed 401 in their console, and nothing on these routes renders
// operator state. The /login-family routes are NOT listed: their
// PublicOnlyRoute redirect depends on the probe.
function isCustomerTokenRoute(pathname: string): boolean {
  return (
    pathname.startsWith('/invoice/') ||
    pathname === '/update-payment' ||
    pathname === '/payment-method-added'
  )
}

export function AuthProvider({ children }: { children: ReactNode }) {
  const [user, setUser] = useState<UserContext | null>(null)
  // On customer-token routes the probe never runs (see the effect), so
  // auth is "loaded" from the first render: user null, loading false.
  // window.location, not useLocation — this provider sits above the
  // router; the check is per page load, which is what the probe is too.
  const skipProbe = isCustomerTokenRoute(window.location.pathname)
  const [loading, setLoading] = useState(!skipProbe)

  const refresh = useCallback(async () => {
    try {
      const info = await authApi.whoami()
      setUser(toUserContext(info))
    } catch (err) {
      // 401 means no session — the user will be sent to /login by
      // ProtectedRoute. Anything else is logged but treated the same
      // to avoid pinning the app on a transient error.
      if (!(err instanceof ApiError) || err.status !== 401) {
        console.error('whoami failed:', err)
      }
      setUser(null)
    }
  }, [])

  // Session bootstrap: ask the server who owns the cookie, once, on
  // mount. This is the rule's own sanctioned case (synchronizing with an
  // external system — the session store); every setState here runs after
  // an await, so no cascading sync render exists. The linter's
  // interprocedural pass can't see through the async boundary.
  useEffect(() => {
    if (skipProbe) return
    // eslint-disable-next-line react-hooks/set-state-in-effect -- async session fetch; all state sets are post-await
    refresh().finally(() => setLoading(false))
  }, [refresh, skipProbe])

  const login = useCallback(async (email: string, password: string) => {
    const res = await authApi.login(email, password)
    setUser({
      user_id: res.user_id,
      tenant_id: res.tenant_id,
      email: res.email,
      livemode: res.livemode,
    })
    // Cache-clear is automatic: ModeAwareQueryProvider keys its
    // QueryClient instances on user identity, so any prior-session
    // cache is gc'd as soon as setUser fires.
  }, [])

  const logout = useCallback(async () => {
    try {
      await authApi.logout()
    } catch (err) {
      // Cookie may already be expired server-side; treat as a local
      // clear and keep going.
      console.warn('logout request failed:', err)
    }
    setUser(null)
  }, [])

  const setMode = useCallback(async (livemode: boolean) => {
    // Flipping user.livemode below remounts the entire app subtree
    // (ModeAwareQueryProvider is keyed on it), and that render saturates
    // the main thread the moment it starts. The sidebar's mode switch
    // begins its 200ms thumb glide on click (Layout.tsx ModeToggle,
    // duration-200 — change these together); a local sub-20ms round
    // trip lets the remount land mid-glide, freezing the thumb and then
    // snapping it. Hold the flip until the glide can complete. Real
    // network latency usually covers this anyway — the floor only pads
    // fast responses, never delays failure, and steps aside when the
    // user prefers reduced motion (the thumb doesn't animate then).
    const glideDone = window.matchMedia('(prefers-reduced-motion: reduce)').matches
      ? Promise.resolve()
      : new Promise(resolve => setTimeout(resolve, 240))
    await authApi.setMode(livemode)
    await glideDone
    setUser(prev => (prev ? { ...prev, livemode } : prev))
    // Notify other tabs sharing this session cookie. Without this,
    // Tab B keeps its amber "TEST" pill while the server-side
    // session is now live — next refetch in Tab B returns live
    // data under a TEST label. BroadcastChannel is supported in
    // every browser we care about; storage events are the fallback
    // path (localStorage write below also fires a `storage` event
    // in other tabs, picked up by the listener in useEffect).
    try {
      const ch = new BroadcastChannel('velox-mode')
      ch.postMessage({ livemode })
      ch.close()
    } catch {
      // BroadcastChannel unavailable — storage event handles it.
    }
    try {
      // eslint-disable-next-line no-restricted-syntax -- cross-tab sync timestamp; genuinely wall-clock, unrelated to any test clock
      localStorage.setItem('velox:mode-sync', JSON.stringify({ livemode, ts: Date.now() }))
    } catch {
      // Private-mode Safari throws on localStorage write — accept
      // single-tab behavior in that case.
    }
  }, [])

  // Listen for mode flips from other tabs. Either path lands here:
  // BroadcastChannel for evergreen browsers, storage event for the
  // localStorage-write fallback. We update the React state but
  // don't re-call POST /v1/auth/mode — the server-side flip
  // already happened in the originating tab.
  useEffect(() => {
    const apply = (livemode: boolean) => {
      setUser(prev => {
        if (!prev || prev.livemode === livemode) return prev
        return { ...prev, livemode }
      })
    }
    let ch: BroadcastChannel | null = null
    try {
      ch = new BroadcastChannel('velox-mode')
      ch.onmessage = ev => {
        if (ev.data && typeof ev.data.livemode === 'boolean') {
          apply(ev.data.livemode)
        }
      }
    } catch {
      // Fall through to storage-event path.
    }
    const onStorage = (ev: StorageEvent) => {
      if (ev.key !== 'velox:mode-sync' || !ev.newValue) return
      try {
        const { livemode } = JSON.parse(ev.newValue) as { livemode: boolean }
        if (typeof livemode === 'boolean') apply(livemode)
      } catch {
        // Ignore malformed payloads.
      }
    }
    window.addEventListener('storage', onStorage)
    return () => {
      ch?.close()
      window.removeEventListener('storage', onStorage)
    }
  }, [])

  return (
    <AuthContext.Provider value={{ user, loading, login, logout, refresh, setMode }}>
      {children}
    </AuthContext.Provider>
  )
}

export function useAuth(): AuthState {
  const ctx = useContext(AuthContext)
  if (!ctx) {
    throw new Error('useAuth must be used within AuthProvider')
  }
  return ctx
}
