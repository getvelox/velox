import { useState, useEffect, useMemo, type ReactNode } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Link, useLocation, useNavigate } from 'react-router-dom'
import {
  LayoutDashboard, Users, FileText, CreditCard, Tag, Wallet, LogOut, Settings,
  Receipt, AlertTriangle, ScrollText, Globe, Key, Menu, X, BarChart3,
  Sun, Moon, Search, ChevronsUpDown, MessageSquareWarning, Sparkles, Loader2,
  Clock as ClockIcon, FlaskConical,
  type LucideIcon,
} from 'lucide-react'
import { toast } from 'sonner'
import { useDarkMode } from '@/hooks/useDarkMode'
import { cn } from '@/lib/utils'
import { CONTENT_BAND } from '@/lib/contentBand'
import { api, setActiveCurrency } from '@/lib/api'
import { getLastRequestId } from '@/lib/lastRequestId'
import { useAuth } from '@/contexts/AuthContext'
import { Separator } from '@/components/ui/separator'
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { CommandPalette } from '@/components/CommandPalette'
import { VeloxLogo } from '@/components/VeloxLogo'
import { OnboardingLauncher } from '@/components/OnboardingLauncher'
import { useOnboardingSteps } from '@/hooks/useOnboardingSteps'
import { ScrollPane } from '@/components/ui/scroll-pane'

const billingNav = [
  { to: '/', icon: LayoutDashboard, label: 'Dashboard' },
  { to: '/customers', icon: Users, label: 'Customers' },
  { to: '/invoices', icon: FileText, label: 'Invoices' },
  { to: '/subscriptions', icon: CreditCard, label: 'Subscriptions' },
  { to: '/usage', icon: BarChart3, label: 'Usage' },
]

const configNav = [
  { to: '/pricing', icon: Tag, label: 'Pricing' },
  { to: '/recipes', icon: Sparkles, label: 'Recipes' },
  { to: '/credits', icon: Wallet, label: 'Credits' },
  { to: '/provider-costs', icon: Tag, label: 'Provider costs' },
  { to: '/credit-notes', icon: Receipt, label: 'Credit Notes' },
  { to: '/dunning', icon: AlertTriangle, label: 'Dunning' },
  { to: '/dunning-policies', icon: AlertTriangle, label: 'Dunning policies' },
]

type NavItem = { to: string; icon: LucideIcon; label: string }

const systemNav: NavItem[] = [
  { to: '/audit-log', icon: ScrollText, label: 'Audit Log' },
  { to: '/webhooks', icon: Globe, label: 'Webhooks' },
  { to: '/api-keys', icon: Key, label: 'API Keys' },
  { to: '/settings', icon: Settings, label: 'Settings' },
]

// Test-mode-only nav: shown when the active session is in test mode and
// hidden when in live. Test clocks are the only entry today; the array
// shape lets future test-only tooling slot in cleanly.
const testOnlyNav: NavItem[] = [
  { to: '/test-clocks', icon: ClockIcon, label: 'Test Clocks' },
]

function NavLink({
  to, icon: Icon, label, pathname, onClick, count, badgeTone = 'info',
}: {
  to: string; icon: LucideIcon; label: string; pathname: string; onClick?: () => void; count?: number; badgeTone?: 'info' | 'critical'
}) {
  // Prefix match keeps the section highlighted on detail pages
  // (/customers/:id highlights Customers). Pricing also owns the
  // /plans/:id and /meters/:id detail routes.
  const active = pathname === to
    || (to !== '/' && pathname.startsWith(to + '/'))
    || (to === '/pricing' && (pathname.startsWith('/plans/') || pathname.startsWith('/meters/')))
  return (
    <div>
    <Tooltip>
      <TooltipTrigger
        render={
          <Link
            to={to}
            onClick={onClick}
            aria-current={active ? 'page' : undefined}
            className={cn(
              'flex items-center gap-3 px-3 py-2 rounded-md text-sm transition-all duration-150 relative',
              active
                ? 'bg-sidebar-accent text-sidebar-accent-foreground font-medium'
                : 'text-muted-foreground hover:text-foreground hover:bg-sidebar-accent/50 hover:translate-x-0.5'
            )}
          />
        }
      >
          {active && (
            <span className="absolute left-0 top-1.5 bottom-1.5 w-[2px] rounded-r bg-sidebar-primary" />
          )}
          <div className="flex items-center justify-between w-full">
            <div className="flex items-center gap-3">
              <Icon size={18} />
              {label}
            </div>
            {count != null && count > 0 && (
              <span className={cn(
                'text-[10px] font-medium rounded-full px-1.5 py-0.5 min-w-[18px] text-center',
                badgeTone === 'critical'
                  ? 'bg-destructive text-destructive-foreground'
                  : 'bg-muted text-muted-foreground border border-border',
              )}>
                {count}
              </span>
            )}
          </div>
      </TooltipTrigger>
      <TooltipContent side="right" className="md:hidden">
        {label}
      </TooltipContent>
    </Tooltip>
    </div>
  )
}

// ModeToggle — segmented control switching the dashboard between test
// and live without signing out. Backed by POST /v1/auth/mode, which
// updates dashboard_sessions.livemode for the current cookie session;
// every downstream API call inherits the new mode via session
// middleware. Lives at the TOP of the sidebar (the environment-switcher
// slot — Clerk/WorkOS put it top-left near the org; Stripe/Razorpay
// top-right): top-prominent, but sidebar-native so no empty top-bar
// band. Deliberately NOT inside the test-only nav section — a section
// headed "Test mode" cannot host the control whose other half is Live,
// and that section unmounts in live mode, which would strand the way
// back.
function ModeToggle({ livemode, busy, onToggle }: { livemode: boolean; busy: boolean; onToggle: () => void }) {
  // Optimistic visual position. The mode switch remounts the whole app
  // subtree (ModeAwareQueryProvider is keyed on livemode — same remount
  // that once ate the Toaster), so a thumb keyed to the SERVER state can
  // never be seen to move: by the time livemode flips, this instance is
  // gone and its replacement mounts already-arrived. Instead the thumb
  // slides to the TARGET the moment it is clicked — the 200ms glide plays
  // during the request — and the post-remount instance simply agrees with
  // where it landed. AuthContext.setMode holds the livemode flip until the
  // glide can finish (240ms floor — change these durations together), or
  // a fast local round trip remounts mid-glide and the thumb freezes then
  // snaps. On failure the render-phase reset below slides it back.
  const [pending, setPending] = useState<boolean | null>(null)
  // Failure reset, done during render (React's sanctioned adjust-state-on-
  // props-change pattern — an effect here trips the cascading-render lint):
  // if the request finished and the server mode did NOT move to the pending
  // target, drop the optimism and let the thumb slide back. Success needs no
  // reset — the mode switch remounts this component with fresh state.
  if (pending !== null && !busy && livemode !== pending) setPending(null)
  const visual = pending ?? livemode

  const segment = (target: boolean, label: 'Test' | 'Live', activeText: string) => {
    const active = visual === target
    return (
      <button
        type="button"
        role="radio"
        aria-checked={livemode === target}
        aria-label={`${label} mode`}
        disabled={busy || livemode === target}
        onClick={() => {
          if (livemode !== target && !busy) {
            setPending(target)
            onToggle()
          }
        }}
        className={cn(
          // Segments are transparent hit-targets layered OVER the sliding
          // thumb below; z-10 keeps labels above it.
          'relative z-10 flex flex-1 items-center justify-center gap-1.5 rounded-md px-3 py-1.5 text-xs font-medium transition-colors duration-200',
          active ? activeText : 'text-muted-foreground hover:text-foreground cursor-pointer',
          busy && !active && 'opacity-50 cursor-not-allowed',
        )}
      >
        {busy && active ? (
          // The spinner rides the segment the operator is switching TO —
          // pending-state affordance on the thing they clicked, not on the
          // mode they are leaving.
          <Loader2 size={11} className="animate-spin" aria-hidden="true" />
        ) : (
          <span
            className={cn(
              'h-1.5 w-1.5 rounded-full transition-colors duration-200',
              active ? (label === 'Live' ? 'bg-emerald-500' : 'bg-amber-500') : 'bg-muted-foreground/40',
              // Live is real money moving — the dot breathes like a record
              // light (stilled under prefers-reduced-motion). Test stays still.
              active && label === 'Live' && 'animate-pulse motion-reduce:animate-none',
            )}
          />
        )}
        {label}
      </button>
    )
  }
  return (
    <div role="radiogroup" aria-label="Test or live mode" className="relative flex w-full items-center rounded-lg bg-muted/60 p-1">
      {/* The sliding thumb: one raised surface that TRAVELS between the two
          positions (200ms ease-out; instant under prefers-reduced-motion).
          Width is half the track minus the 4px padding ring; translate-x
          moves it exactly one thumb-width. Driven by the OPTIMISTIC visual
          position (see above) so the glide is actually visible. */}
      <span
        aria-hidden="true"
        className={cn(
          'absolute left-1 top-1 bottom-1 w-[calc(50%-4px)] rounded-md bg-card shadow-sm ring-1 ring-black/5 dark:ring-white/10',
          'transition-transform duration-200 ease-out motion-reduce:transition-none',
          visual && 'translate-x-full',
        )}
      />
      {segment(false, 'Test', 'text-amber-700 dark:text-amber-300')}
      {segment(true, 'Live', 'text-emerald-700 dark:text-emerald-400')}
    </div>
  )
}


// Query params that select a view rather than reference a row, and so
// remain valid across a test/live switch. Everything else is dropped on
// toggle — see handleToggleMode.
const MODE_INDEPENDENT_PARAMS = ['tab'] as const

export function Layout({ children }: { children: ReactNode }) {
  const location = useLocation()
  const navigate = useNavigate()
  const [sidebarOpen, setSidebarOpen] = useState(false)
  const [commandOpen, setCommandOpen] = useState(false)
  const { dark, toggle: toggleDark } = useDarkMode()
  const { user, logout, setMode } = useAuth()
  const [modeBusy, setModeBusy] = useState(false)
  const handleToggleMode = async () => {
    if (!user || modeBusy) return
    const next = !user.livemode
    setModeBusy(true)
    try {
      await setMode(next)
      toast.success(next ? 'Switched to live mode' : 'Switched to test mode')
      // Strip mode-scoped query params before staying on the page. Params
      // that name a ROW or an offset into the other mode's dataset go
      // stale the moment the mode flips: Usage Events' ?customer=<id>
      // filters on an id that doesn't exist in the new mode, and a ?page=
      // deep into 74 test invoices is out of range against 1 live one.
      // Pathname stays so detail-page IDs surface the existing "Not found"
      // branch for entities that don't exist in the new mode.
      //
      // MODE_INDEPENDENT_PARAMS survives the strip: ?tab= selects a pane,
      // not a row. Dropping it bounced the operator off Settings→Payments,
      // Pricing→meters/rules and Webhooks→events on every toggle — the
      // three surfaces whose whole point is comparing test against live.
      if (location.search) {
        const kept = new URLSearchParams()
        const current = new URLSearchParams(location.search)
        for (const name of MODE_INDEPENDENT_PARAMS) {
          const value = current.get(name)
          if (value !== null) kept.set(name, value)
        }
        const search = kept.toString()
        if (search !== current.toString()) {
          navigate({ pathname: location.pathname, search: search ? `?${search}` : '' }, { replace: true })
        }
      }
    } catch (err) {
      const msg = err instanceof Error ? err.message : 'Failed to switch mode'
      toast.error(msg)
    } finally {
      setModeBusy(false)
    }
  }
  // Drives the live-mode Stripe-missing hard-blocker. The launcher itself
  // calls the same hook — React Query dedupes by key, so no duplicate fetches.
  const { hasLiveStripe } = useOnboardingSteps()

  const handleLogout = async () => {
    await logout()
    navigate('/login', { replace: true })
  }

  // Global keyboard shortcut: Cmd/Ctrl+K opens the command palette.
  useEffect(() => {
    const onKeyDown = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key === 'k') {
        e.preventDefault()
        setCommandOpen(prev => !prev)
      }
    }
    document.addEventListener('keydown', onKeyDown)
    return () => document.removeEventListener('keydown', onKeyDown)
  }, [])

  // Tenant currency drives the formatCents/getCurrencySymbol helpers
  // app-wide. Side-effect on success: push to the module-level
  // setActiveCurrency so non-React call sites pick it up. React Query
  // handles the StrictMode dedupe + cache across Layout remounts on
  // route navigation.
  const settingsQuery = useQuery({
    queryKey: ['layout', 'settings'],
    queryFn: () => api.getSettings(),
  })
  useEffect(() => {
    const cur = settingsQuery.data?.default_currency
    if (cur) setActiveCurrency(cur)
  }, [settingsQuery.data?.default_currency])

  // Sidebar nav badges (open invoices count, active dunning count).
  // Stale data is fine — these are background hints; RQ's
  // staleWhileRevalidate behavior gives the operator a near-real-time
  // count without per-mount thrash.
  const analyticsQuery = useQuery({
    queryKey: ['layout', 'analytics-overview'],
    queryFn: () => api.getAnalyticsOverview(),
  })
  const navCounts = useMemo<Record<string, number>>(() => {
    const ov = analyticsQuery.data
    if (!ov) return {}
    const counts: Record<string, number> = {}
    if (ov.open_invoices > 0) counts['/invoices'] = ov.open_invoices
    if (ov.dunning_active > 0) counts['/dunning'] = ov.dunning_active
    return counts
  }, [analyticsQuery.data])

  const closeSidebar = () => setSidebarOpen(false)

  const sidebarContent = (
    <>
      {/* Header */}
      <div className="p-4 border-b border-border flex items-center justify-between">
        <VeloxLogo size="sm" />
        <button
          onClick={closeSidebar}
          aria-label="Close menu"
          className="md:hidden text-muted-foreground hover:text-foreground"
        >
          <X size={20} />
        </button>
      </div>

      {/* Mode switch — top of the sidebar, the environment-switcher slot
          (Clerk/WorkOS pattern). Every reference product puts the test/live
          switch at the TOP (Stripe/Razorpay top-right; Clerk/WorkOS top-left
          near the org). We keep it sidebar-native rather than a top bar so
          there's no empty full-width band, but at the TOP where the eye and
          muscle memory expect it. */}
      {user && (
        <div className="px-3 pt-3">
          <ModeToggle
            livemode={user.livemode}
            busy={modeBusy}
            onToggle={handleToggleMode}
          />
        </div>
      )}

      {/* Search trigger */}
      <div className="px-3 pt-3">
        <button
          onClick={() => setCommandOpen(true)}
          className="w-full flex items-center gap-2 px-3 py-2 bg-muted rounded-md text-sm text-muted-foreground hover:bg-accent transition-colors"
        >
          <Search size={14} />
          <span className="flex-1 text-left">Search...</span>
          <kbd className="text-[11px] bg-background px-1.5 py-0.5 rounded border border-border font-medium text-muted-foreground">
            {navigator.platform?.includes('Mac') ? '\u2318' : 'Ctrl+'}K
          </kbd>
        </button>
      </div>

      <ScrollPane as="nav" aria-label="Main navigation" className="flex-1 min-h-0 p-3 space-y-1">
        <p className="text-xs uppercase text-muted-foreground tracking-wider px-3 pt-2 pb-1">
          Billing
        </p>
        {billingNav.map(item => (
          <NavLink key={item.to} {...item} pathname={location.pathname} onClick={closeSidebar} count={navCounts[item.to]} badgeTone={item.to === '/dunning' ? 'critical' : 'info'} />
        ))}

        <p className="text-xs uppercase text-muted-foreground tracking-wider px-3 pt-4 pb-1">
          Configuration
        </p>
        {configNav.map(item => (
          <NavLink key={item.to} {...item} pathname={location.pathname} onClick={closeSidebar} count={navCounts[item.to]} badgeTone={item.to === '/dunning' ? 'critical' : 'info'} />
        ))}

        <Separator className="my-2" />

        <p className="text-xs uppercase text-muted-foreground tracking-wider px-3 pt-2 pb-1">
          System
        </p>
        {systemNav.map(item => (
          <NavLink key={item.to} {...item} pathname={location.pathname} onClick={closeSidebar} count={navCounts[item.to]} badgeTone={item.to === '/dunning' ? 'critical' : 'info'} />
        ))}

        {user && !user.livemode && (
          <>
            <p className="text-xs uppercase text-muted-foreground tracking-wider px-3 pt-4 pb-1 flex items-center gap-1.5">
              <FlaskConical size={11} className="text-amber-500" />
              Test mode
            </p>
            {testOnlyNav.map(item => (
              <NavLink key={item.to} {...item} pathname={location.pathname} onClick={closeSidebar} />
            ))}
          </>
        )}
      </ScrollPane>

      {/* Footer — account menu (who am I). The mode switch lives at the TOP
          of the sidebar (environment-switcher slot), not here. */}
      <div className="p-2 border-t border-border">
        {user && (
          <DropdownMenu>
            <DropdownMenuTrigger
              aria-label="Account menu"
              className="w-full flex items-center gap-2 rounded-md px-2 py-1.5 text-left hover:bg-accent data-[popup-open]:bg-accent outline-none focus-visible:ring-2 focus-visible:ring-ring transition-colors"
            >
              <div
                aria-hidden="true"
                className="h-7 w-7 shrink-0 rounded-full bg-gradient-to-br from-primary/25 to-primary/5 ring-1 ring-primary/20 text-primary flex items-center justify-center text-xs font-semibold"
              >
                {(user.email || 'U').charAt(0).toUpperCase()}
              </div>
              <p className="text-xs text-foreground truncate flex-1 min-w-0" title={user.email}>
                {user.email}
              </p>
              <ChevronsUpDown size={14} className="shrink-0 text-muted-foreground" aria-hidden="true" />
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end" side="top" sideOffset={8} className="w-60">
              <div className="flex items-center gap-2.5 px-2 py-2">
                <div
                  aria-hidden="true"
                  className="h-9 w-9 shrink-0 rounded-full bg-gradient-to-br from-primary/25 to-primary/5 ring-1 ring-primary/20 text-primary flex items-center justify-center text-sm font-semibold"
                >
                  {(user.email || 'U').charAt(0).toUpperCase()}
                </div>
                <div className="flex-1 min-w-0">
                  <p className="text-sm font-medium text-foreground truncate" title={user.email}>
                    {user.email}
                  </p>
                  <p className="text-[11px] text-muted-foreground">
                    {user.livemode ? 'Live mode' : 'Test mode'}
                  </p>
                </div>
              </div>
              <DropdownMenuSeparator />
              <DropdownMenuItem
                onClick={() => {
                  // Build the mailto body at click-time so the request_id we
                  // send is the freshest one — not whatever was current when
                  // the menu rendered. tenant_id helps us scope the search on
                  // our end without asking the user to reproduce.
                  const requestId = getLastRequestId()
                  const body =
                    `What happened:\n\n\n` +
                    `--- context ---\n` +
                    `tenant_id: ${user.tenant_id}\n` +
                    `url: ${window.location.href}\n` +
                    `user_agent: ${navigator.userAgent}\n` +
                    (requestId ? `request_id: ${requestId}\n` : '')
                  window.location.href = `mailto:support@velox.dev?subject=${encodeURIComponent(
                    'Velox issue report',
                  )}&body=${encodeURIComponent(body)}`
                }}
              >
                <MessageSquareWarning />
                <span>Report an issue</span>
              </DropdownMenuItem>
              <DropdownMenuSeparator />
              <DropdownMenuItem onClick={toggleDark}>
                {dark ? <Sun /> : <Moon />}
                <span>{dark ? 'Light mode' : 'Dark mode'}</span>
              </DropdownMenuItem>
              <DropdownMenuSeparator />
              <DropdownMenuItem variant="destructive" onClick={handleLogout}>
                <LogOut />
                <span>Sign out</span>
              </DropdownMenuItem>
              <DropdownMenuSeparator />
              <div className="px-2 py-1.5 text-[10px] text-muted-foreground/60 tracking-wide">
                Velox v2.0
              </div>
            </DropdownMenuContent>
          </DropdownMenu>
        )}
      </div>
    </>
  )

  return (
    <div className="flex h-screen">
      {/* Mobile overlay */}
      {sidebarOpen && (
        <div
          className="fixed inset-0 bg-black/20 backdrop-blur-sm z-30 md:hidden"
          onClick={closeSidebar}
        />
      )}

      {/* Sidebar - desktop */}
      <aside className="hidden md:flex w-60 bg-card border-r border-border flex-col shrink-0">
        {sidebarContent}
      </aside>

      {/* Sidebar - mobile */}
      {/* inert when closed: the drawer is only translated off-canvas, so
          without it every control inside — including the Test/Live switch —
          stays tab-reachable while invisible; the first Tab press on mobile
          could focus (and Enter could flip) the mode switch sight unseen. */}
      <aside
        inert={!sidebarOpen}
        className={cn(
          'fixed inset-y-0 left-0 z-40 w-60 bg-card border-r border-border flex flex-col transition-transform duration-200 md:hidden',
          sidebarOpen ? 'translate-x-0' : '-translate-x-full'
        )}
      >
        {sidebarContent}
      </aside>

      {/* Main content.
          Goes through ScrollPane for the same reason the sidebar does, and it
          is the pane where the omission cost most: this is where every page's
          content lives, so on any window shorter than the page the last card
          was sliced with nothing on screen saying so. There is no document
          scrollbar to fall back on either — the shell is exactly viewport
          height, so the browser draws none, and macOS hides overlay scrollbars
          until you are already scrolling. */}
      <ScrollPane as="main" className="flex-1 bg-background">
        {/* Header stack — mode/safety banner + top-bar, pinned together so
            whichever banner is live (Stripe-missing hard blocker in live
            mode, test-mode strip otherwise) can never scroll out of sight.
            Mirrors Stripe's persistent chrome — the single strongest guard
            against "did I just do that on live?" operator confusion. */}
        <div className="sticky top-0 z-20">
          {/* Hard blocker — live mode without a Stripe live credential means
              every real charge will 4xx. Non-dismissible; the only fix is to
              connect Stripe. Mutually exclusive with the test-mode banner
              (this only fires in live mode). */}
          {user && user.livemode && hasLiveStripe === false && (
            <div
              role="alert"
              className="flex items-center justify-center gap-2 bg-destructive px-4 py-1.5 text-xs font-medium text-destructive-foreground"
            >
              <AlertTriangle size={14} aria-hidden="true" />
              <span>
                <strong className="font-semibold">LIVE</strong> mode but no Stripe live credentials — real charges will fail.
              </span>
              <Link
                to="/settings?tab=payments"
                className="ml-1 underline decoration-destructive-foreground/50 underline-offset-2 hover:decoration-destructive-foreground"
              >
                Connect Stripe
              </Link>
            </div>
          )}
          {user && !user.livemode && (
            <div
              role="status"
              aria-live="polite"
              className="flex items-center justify-center gap-2 bg-amber-500 px-4 py-1.5 text-xs font-medium text-amber-950"
            >
              <AlertTriangle size={14} aria-hidden="true" />
              <span>
                You're viewing <strong className="font-semibold">TEST</strong> data. No real money is moving.
              </span>
            </div>
          )}
          {/* Top bar — MOBILE ONLY (hamburger + logo). On desktop this row
              no longer exists: its only occupant was the mode toggle, which
              made it a full-width band that was ~85px of empty chrome on
              every page (a reader reviewing the README hero caught it). The
              toggle now lives in the fixed sidebar above the account block;
              the mode BANNER above stays sticky and is the loud signal. */}
          <div className="md:hidden border-b border-border bg-card">
            <div className={cn(CONTENT_BAND, 'flex items-center gap-3 px-4 py-3')}>
              <button
                onClick={() => setSidebarOpen(true)}
                aria-label="Open menu"
                className="text-muted-foreground hover:text-foreground"
              >
                <Menu size={22} />
              </button>
              <VeloxLogo size="sm" />
            </div>
          </div>
        </div>
        <div className={cn(CONTENT_BAND, 'p-4 md:p-8')}>
          {children}
        </div>
      </ScrollPane>

      {/* Command Palette */}
      <CommandPalette open={commandOpen} onClose={() => setCommandOpen(false)} />

      {/* Global onboarding launcher — floats bottom-right, self-hides when
          the checklist is complete or dismissed. Rendered at the Layout level
          so it persists across all authed routes, not just Dashboard. */}
      <OnboardingLauncher />
    </div>
  )
}
